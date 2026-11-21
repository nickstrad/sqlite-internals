// Package fence owns the store-backed fencing scheme for single-writer
// ownership across nodes.
//
// Each database's stream is stored in fixed-width position slots under
// dbs/<id>/log/<12-digit position>. Every slot object begins with a one-line
// envelope naming the writer's epoch, object kind (snapshot or segment), and
// covered position range, followed by the unchanged walship object bytes.
//
// Fencing has two halves because an S3 store cannot read an epoch inside an
// object and compare it against another object:
//   - Store half (slot exclusivity): PutIfAbsent (If-None-Match: "*") ensures
//     each position key can be written at most once and never overwritten.
//   - Writer half (Adapter): a writer lists the chain, stops with FencedError
//     if any slot holds an epoch higher than its own, and appends only at
//     end+1 of the chain it validated.
//
// Both halves are necessary. Without slot exclusivity, a stale listing lets a
// writer overwrite an accepted slot. Without the writer rule, a restarted
// stale owner (such as raw walship.Open resuming at the store's global end)
// could append into a free slot after the new owner's fence and have
// If-None-Match accept it.
package fence

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"

	"github.com/nickstrad/walfleet/internal/walship"
)

// Envelope constants identifying slot object kinds and the wire header tag.
const (
	// KindSnapshot marks a slot carrying a full database snapshot (and every
	// new epoch's opening fence object).
	KindSnapshot = "snapshot"
	// KindSegment marks a slot carrying a walship WAL segment.
	KindSegment = "segment"
	envelopeTag = "fenced-slot v1"
)

// Sentinel errors returned by slot decoding, lineage checks, and Adapter calls.
var (
	// ErrFenced is matched by FencedError via errors.Is when a newer epoch
	// occupies the next slot or appears anywhere in the chain.
	ErrFenced = errors.New("fenced")
	// ErrDamaged reports a malformed slot key, envelope line, or embedded
	// walship object, or a slot whose metadata changed after it was listed.
	ErrDamaged = errors.New("damaged slot")
	// ErrBadChain reports a slot sequence that breaks the single-lineage rules
	// checked by CheckLineage.
	ErrBadChain = errors.New("slot chain is not one lineage")
	// ErrOutOfOrder reports a PutIfAbsent whose first position is not end+1 of
	// the chain the adapter knows.
	ErrOutOfOrder = errors.New("put is not at the next position of this writer's chain")
)

// FencedError records that a writer with Epoch was stopped because a newer
// epoch (Holder) already owns Position in Database's slot chain.
type FencedError struct {
	Database string
	Epoch    uint64
	Holder   uint64
	Position int64
}

// Error formats e as a deterministic message suitable for logs and node replies.
func (e *FencedError) Error() string {
	return fmt.Sprintf("fenced: %s position %d is held by epoch %d, this writer has epoch %d", e.Database, e.Position, e.Holder, e.Epoch)
}

// Is allows callers to test for any FencedError with errors.Is(err, ErrFenced).
func (e *FencedError) Is(target error) bool { return target == ErrFenced }

// Slot is one decoded entry in a database's fenced log. Object holds the
// embedded walship snapshot or segment payload when loaded; cached Adapter
// entries clear Object and raw so open databases do not hold shipped bytes in RAM.
type Slot struct {
	Key    string
	Epoch  uint64
	Kind   string
	First  int64
	Last   int64
	Object []byte
	raw    []byte
}

func (s Slot) withoutBytes() Slot {
	s.Object, s.raw = nil, nil
	return s
}

// SlotPrefix returns the object-store key prefix under which database's slots live.
func SlotPrefix(database string) string { return "dbs/" + database + "/log/" }

// SlotKey returns the fixed-width key for position in database's slot chain.
// Twelve zero-padded digits keep lexicographic order identical to numeric order.
func SlotKey(database string, position int64) string {
	return fmt.Sprintf("%s%012d", SlotPrefix(database), position)
}

// Encode prepends the single-line fenced-slot v1 envelope to object.
func Encode(epoch uint64, kind string, first, last int64, object []byte) []byte {
	header := fmt.Sprintf("%s epoch=%d kind=%s first=%d last=%d\n", envelopeTag, epoch, kind, first, last)
	return append([]byte(header), object...)
}

// Decode parses and strictly validates a slot object against key. It requires
// the envelope line to re-encode identically (rejecting padding or extra
// fields), epoch >= 1, first matching key's position, last >= first, a valid
// database segment in key, and a decodable embedded walship snapshot or segment
// whose positions match the envelope.
func Decode(key string, data []byte) (Slot, error) {
	line, object, found := bytes.Cut(data, []byte("\n"))
	slot := Slot{Key: key, Object: object, raw: data}
	if !found {
		return slot, fmt.Errorf("%w: %s has no envelope line", ErrDamaged, key)
	}
	_, err := fmt.Sscanf(string(line), envelopeTag+" epoch=%d kind=%s first=%d last=%d", &slot.Epoch, &slot.Kind, &slot.First, &slot.Last)
	// Round-trip comparison rejects zero-padded integers, extra whitespace, or
	// trailing fields that fmt.Sscanf alone would silently ignore.
	if err != nil || string(Encode(slot.Epoch, slot.Kind, slot.First, slot.Last, nil)) != string(line)+"\n" {
		return slot, fmt.Errorf("%w: %s envelope %q", ErrDamaged, key, line)
	}
	if slot.Epoch == 0 || slot.First < 1 || slot.Last < slot.First || keyDatabase(key) == "" || key != SlotKey(keyDatabase(key), slot.First) {
		return slot, fmt.Errorf("%w: %s envelope %q does not fit its key", ErrDamaged, key, line)
	}
	switch slot.Kind {
	case KindSnapshot:
		meta, _, err := walship.DecodeSnapshot(object)
		if err != nil || meta.Position != slot.First || slot.Last != slot.First {
			return slot, fmt.Errorf("%w: %s snapshot object does not match its envelope: %v", ErrDamaged, key, err)
		}
	case KindSegment:
		meta, _, err := walship.DecodeSegment(object)
		if err != nil || meta.FirstPosition != slot.First || meta.LastPosition != slot.Last {
			return slot, fmt.Errorf("%w: %s segment object does not match its envelope: %v", ErrDamaged, key, err)
		}
	default:
		return slot, fmt.Errorf("%w: %s kind %q", ErrDamaged, key, slot.Kind)
	}
	return slot, nil
}

func keyDatabase(key string) string {
	parts := strings.Split(key, "/")
	if len(parts) != 4 || parts[0] != "dbs" || parts[2] != "log" || parts[1] == "" || walship.ValidateKey("dbs/"+parts[1]) != nil {
		return ""
	}
	return parts[1]
}

// Chain lists, fetches, decodes, and lineage-checks every slot for database in
// base, sorted by key (and therefore by position).
func Chain(ctx context.Context, base walship.Store, database string) ([]Slot, error) {
	keys, err := base.List(ctx, SlotPrefix(database))
	if err != nil {
		return nil, err
	}
	// Sort explicitly because S3 listings are not guaranteed to arrive in byte order.
	sort.Strings(keys)
	var chain []Slot
	for _, key := range keys {
		data, err := base.Get(ctx, key)
		if err != nil {
			return nil, err
		}
		slot, err := Decode(key, data)
		if err != nil {
			return nil, err
		}
		chain = append(chain, slot)
	}
	return chain, CheckLineage(chain)
}

// CheckLineage verifies that chain forms a single unbroken lineage: if
// non-empty, it begins with a snapshot at position 1, every subsequent slot
// begins at previous.Last+1, epochs never decrease, and every epoch increase
// begins with a fence snapshot.
func CheckLineage(chain []Slot) error {
	for i, slot := range chain {
		if i == 0 {
			if slot.First != 1 || slot.Kind != KindSnapshot {
				return fmt.Errorf("%w: the chain starts with a %s at position %d, want a snapshot at 1", ErrBadChain, slot.Kind, slot.First)
			}
			continue
		}
		previous := chain[i-1]
		switch {
		case slot.First != previous.Last+1:
			return fmt.Errorf("%w: position %d follows %d", ErrBadChain, slot.First, previous.Last)
		case slot.Epoch < previous.Epoch:
			return fmt.Errorf("%w: epoch %d at position %d after epoch %d", ErrBadChain, slot.Epoch, slot.First, previous.Epoch)
		case slot.Epoch > previous.Epoch && slot.Kind != KindSnapshot:
			return fmt.Errorf("%w: epoch %d starts at position %d with a %s, not a fence snapshot", ErrBadChain, slot.Epoch, slot.First, slot.Kind)
		}
	}
	return nil
}

// Adapter presents one database's fenced slot chain as a walship.Store for a
// writer operating at Epoch. It translates between walship's snapshots/<p> and
// segments/<p> keys and the store's dbs/<id>/log/<p> slots, enforcing the
// writer half of the fencing rule.
type Adapter struct {
	Base     walship.Store
	Database string
	Epoch    uint64
	mu       sync.Mutex
	slots    map[int64]Slot
	end      int64
	listed   bool
}

// New constructs an Adapter for database at epoch, rejecting invalid key
// segments and epoch 0 (which is not an ownership epoch).
func New(base walship.Store, database string, epoch uint64) (*Adapter, error) {
	if err := walship.ValidateKey("dbs/" + database); err != nil || strings.Contains(database, "/") {
		return nil, fmt.Errorf("fence: database id %q is not a valid key segment", database)
	}
	if epoch == 0 {
		return nil, errors.New("fence: epoch 0 is not an ownership epoch")
	}
	return &Adapter{Base: base, Database: database, Epoch: epoch, slots: map[int64]Slot{}}, nil
}

// Refresh reloads the database's slot chain from Base, fails with FencedError
// if any slot holds an epoch higher than a.Epoch, and returns the chain's last
// position.
func (a *Adapter) Refresh(ctx context.Context) (int64, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.refreshLocked(ctx)
}

func (a *Adapter) refreshLocked(ctx context.Context) (int64, error) {
	chain, err := Chain(ctx, a.Base, a.Database)
	if err != nil {
		return 0, err
	}
	// Build the new metadata map in locals and replace a.slots only after the
	// entire chain passes the epoch check; otherwise a fenced Refresh would
	// leave a truncated pre-fence view cached as listed.
	slots, end := map[int64]Slot{}, int64(0)
	for _, slot := range chain {
		if slot.Epoch > a.Epoch {
			return 0, &FencedError{Database: a.Database, Epoch: a.Epoch, Holder: slot.Epoch, Position: slot.First}
		}
		// Keep metadata only so open databases do not retain shipped bytes in RAM.
		slots[slot.First] = slot.withoutBytes()
		end = slot.Last
	}
	a.slots, a.end, a.listed = slots, end, true
	return a.end, nil
}

// List returns walship-style snapshot and segment keys matching prefix from the
// adapter's cached listing, refreshing once from Base if not yet listed.
// Listing only once per Refresh lets a claim compute both its restore image and
// its fence position from a single consistent listing.
func (a *Adapter) List(ctx context.Context, prefix string) ([]string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.listed {
		if _, err := a.refreshLocked(ctx); err != nil {
			return nil, err
		}
	}
	var keys []string
	for _, slot := range a.slots {
		key := walship.SnapshotKey(slot.First)
		if slot.Kind == KindSegment {
			key = walship.SegmentKey(slot.First)
		}
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys, nil
}

// Get fetches and decodes the slot for walship key from Base on demand,
// returning its embedded walship object bytes. If the slot was previously
// listed and its epoch or range no longer match, Get returns ErrDamaged.
func (a *Adapter) Get(ctx context.Context, key string) ([]byte, error) {
	kind, position, err := walship.ParseKey(key)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	known, ok := a.slots[position]
	a.mu.Unlock()
	if ok && known.Kind != kind {
		return nil, fmt.Errorf("%w: %s", walship.ErrNotFound, key)
	}
	data, err := a.Base.Get(ctx, SlotKey(a.Database, position))
	if err != nil {
		return nil, err
	}
	slot, err := Decode(SlotKey(a.Database, position), data)
	if err != nil {
		return nil, err
	}
	if ok && (slot.Epoch != known.Epoch || slot.Last != known.Last) {
		return nil, fmt.Errorf("%w: %s changed after it was listed", ErrDamaged, slot.Key)
	}
	if slot.Kind != kind {
		return nil, fmt.Errorf("%w: %s", walship.ErrNotFound, key)
	}
	return slot.Object, nil
}

// PutIfAbsent wraps data in a fenced-slot envelope for a.Epoch and writes it
// conditionally at end+1 of this adapter's known chain.
//
// It validates the envelope before sending anything so a malformed object never
// becomes an immutable slot. When Base reports ErrExists, PutIfAbsent reads the
// occupying slot to distinguish three cases: identical bytes (an SDK retry of
// this writer's own put, treated as success), a higher epoch (FencedError), or
// a lower/equal epoch from a stale listing (walship.ErrExists so the caller
// re-lists and retries).
func (a *Adapter) PutIfAbsent(ctx context.Context, key string, data []byte) error {
	kind, first, err := walship.ParseKey(key)
	if err != nil {
		return err
	}
	last := first
	if kind == KindSegment {
		meta, _, err := walship.DecodeSegment(data)
		if err != nil {
			return err
		}
		last = meta.LastPosition
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.listed {
		if _, err := a.refreshLocked(ctx); err != nil {
			return err
		}
	}
	// Writer half of the fencing rule: append only directly after the last
	// position this adapter validated.
	if first != a.end+1 {
		return fmt.Errorf("%w: %s at position %d, the chain this writer knows ends at %d", ErrOutOfOrder, a.Database, first, a.end)
	}
	slotKey := SlotKey(a.Database, first)
	envelope := Encode(a.Epoch, kind, first, last, data)
	// Decode before uploading so malformed objects are rejected locally rather
	// than written into an immutable slot.
	slot, err := Decode(slotKey, envelope)
	if err != nil {
		return err
	}
	if err := a.Base.PutIfAbsent(ctx, slotKey, envelope); err != nil {
		if !errors.Is(err, walship.ErrExists) {
			return err
		}
		stored, getErr := a.Base.Get(ctx, slotKey)
		if getErr != nil {
			return errors.Join(err, getErr)
		}
		holder, decodeErr := Decode(slotKey, stored)
		switch {
		case decodeErr != nil:
			return errors.Join(err, decodeErr)
		case bytes.Equal(stored, envelope):
			// Identical bytes mean an earlier attempt of this same put already
			// landed (for example, before an SDK retry).
		case holder.Epoch > a.Epoch:
			return &FencedError{Database: a.Database, Epoch: a.Epoch, Holder: holder.Epoch, Position: first}
		default:
			return fmt.Errorf("%w: %s position %d is held by epoch %d", walship.ErrExists, a.Database, first, holder.Epoch)
		}
	}
	a.slots[first], a.end = slot.withoutBytes(), last
	return nil
}

// Audit loads and lineage-checks database's full slot chain and returns both
// the slots and the walship.Catalog built over them using a read-only adapter
// at epoch MaxUint64 (which is never fenced and never writes).
func Audit(ctx context.Context, base walship.Store, database string) ([]Slot, *walship.Catalog, error) {
	chain, err := Chain(ctx, base, database)
	if err != nil {
		return nil, nil, err
	}
	auditor, err := New(base, database, math.MaxUint64)
	if err != nil {
		return nil, nil, err
	}
	restorable, err := walship.LoadCatalog(ctx, auditor)
	return chain, restorable, err
}

// RestoreAt plans and executes a walship restore of restorable to position,
// returning the rebuilt database file bytes.
func RestoreAt(restorable *walship.Catalog, position int64) ([]byte, error) {
	plan, err := restorable.Plan(position)
	if err != nil {
		return nil, err
	}
	image, _, err := plan.Execute()
	return image, err
}

// Delete always refuses because fenced log slots are write-once and never
// deleted by a writer.
func (a *Adapter) Delete(ctx context.Context, key string) error {
	return fmt.Errorf("fence: %s: slots are never deleted by a writer", key)
}
