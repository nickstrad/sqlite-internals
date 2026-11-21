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

	"github.com/nickstrad/sqlite-internals/internal/walship"
)

const (
	KindSnapshot = "snapshot"
	KindSegment  = "segment"
	envelopeTag  = "fenced-slot v1"
)

var (
	ErrFenced     = errors.New("fenced")
	ErrDamaged    = errors.New("damaged slot")
	ErrBadChain   = errors.New("slot chain is not one lineage")
	ErrOutOfOrder = errors.New("put is not at the next position of this writer's chain")
)

type FencedError struct {
	Database string
	Epoch    uint64
	Holder   uint64
	Position int64
}

func (e *FencedError) Error() string {
	return fmt.Sprintf("fenced: %s position %d is held by epoch %d, this writer has epoch %d", e.Database, e.Position, e.Holder, e.Epoch)
}

func (e *FencedError) Is(target error) bool { return target == ErrFenced }

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

func SlotPrefix(database string) string { return "dbs/" + database + "/log/" }

func SlotKey(database string, position int64) string {
	return fmt.Sprintf("%s%012d", SlotPrefix(database), position)
}

func Encode(epoch uint64, kind string, first, last int64, object []byte) []byte {
	header := fmt.Sprintf("%s epoch=%d kind=%s first=%d last=%d\n", envelopeTag, epoch, kind, first, last)
	return append([]byte(header), object...)
}

func Decode(key string, data []byte) (Slot, error) {
	line, object, found := bytes.Cut(data, []byte("\n"))
	slot := Slot{Key: key, Object: object, raw: data}
	if !found {
		return slot, fmt.Errorf("%w: %s has no envelope line", ErrDamaged, key)
	}
	_, err := fmt.Sscanf(string(line), envelopeTag+" epoch=%d kind=%s first=%d last=%d", &slot.Epoch, &slot.Kind, &slot.First, &slot.Last)
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

func Chain(ctx context.Context, base walship.Store, database string) ([]Slot, error) {
	keys, err := base.List(ctx, SlotPrefix(database))
	if err != nil {
		return nil, err
	}
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

type Adapter struct {
	Base     walship.Store
	Database string
	Epoch    uint64
	mu       sync.Mutex
	slots    map[int64]Slot
	end      int64
	listed   bool
}

func New(base walship.Store, database string, epoch uint64) (*Adapter, error) {
	if err := walship.ValidateKey("dbs/" + database); err != nil || strings.Contains(database, "/") {
		return nil, fmt.Errorf("fence: database id %q is not a valid key segment", database)
	}
	if epoch == 0 {
		return nil, errors.New("fence: epoch 0 is not an ownership epoch")
	}
	return &Adapter{Base: base, Database: database, Epoch: epoch, slots: map[int64]Slot{}}, nil
}

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
	slots, end := map[int64]Slot{}, int64(0)
	for _, slot := range chain {
		if slot.Epoch > a.Epoch {
			return 0, &FencedError{Database: a.Database, Epoch: a.Epoch, Holder: slot.Epoch, Position: slot.First}
		}
		slots[slot.First] = slot.withoutBytes()
		end = slot.Last
	}
	a.slots, a.end, a.listed = slots, end, true
	return a.end, nil
}

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
	if first != a.end+1 {
		return fmt.Errorf("%w: %s at position %d, the chain this writer knows ends at %d", ErrOutOfOrder, a.Database, first, a.end)
	}
	slotKey := SlotKey(a.Database, first)
	envelope := Encode(a.Epoch, kind, first, last, data)
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
		case holder.Epoch > a.Epoch:
			return &FencedError{Database: a.Database, Epoch: a.Epoch, Holder: holder.Epoch, Position: first}
		default:
			return fmt.Errorf("%w: %s position %d is held by epoch %d", walship.ErrExists, a.Database, first, holder.Epoch)
		}
	}
	a.slots[first], a.end = slot.withoutBytes(), last
	return nil
}

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

func RestoreAt(restorable *walship.Catalog, position int64) ([]byte, error) {
	plan, err := restorable.Plan(position)
	if err != nil {
		return nil, err
	}
	image, _, err := plan.Execute()
	return image, err
}

func (a *Adapter) Delete(ctx context.Context, key string) error {
	return fmt.Errorf("fence: %s: slots are never deleted by a writer", key)
}
