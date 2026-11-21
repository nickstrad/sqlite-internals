package node

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickstrad/walfleet/ch10-multi-database-platform-capstone/service/fence"
	"github.com/nickstrad/walfleet/internal/recipe"
	"github.com/nickstrad/walfleet/internal/sqlitex"
	"github.com/nickstrad/walfleet/internal/walship"
)

// Sentinel errors returned by relocation participant operations on a Node.
var (
	// ErrSealed reports that Write targeted a database sealed for relocation.
	ErrSealed = errors.New("the database is sealed for relocation")
	// ErrNotSealed reports that Drain or Retire was called on an owned database
	// that has not been sealed yet.
	ErrNotSealed = errors.New("the database is not sealed for relocation")
	// ErrMovedAfterDrain reports that Activate found the store chain no longer
	// ends at the drained position recorded before activation.
	ErrMovedAfterDrain = errors.New("the store moved past the drained position")
	// ErrStagedMismatch reports that the staged (or adopted) database image
	// does not hash to the drained sha256 recorded at step 3.
	ErrStagedMismatch = errors.New("the staged image is not the drained state")
)

// Drained reports the stream Position and at-rest file SHA256 left by Drain
// after shipping the source's final commits and closing the database cleanly.
type Drained struct {
	Position int64
	SHA256   string
}

// Staged reports the stream Position and SHA256 of the image rebuilt into
// <id>.staged on the destination node during relocation step 4.
type Staged struct {
	Position int64
	SHA256   string
}

// Activated reports the store Fence position written (or adopted) by Activate,
// with Adopted true when the destination's fence snapshot was already in the store.
type Activated struct {
	Fence   int64
	Adopted bool
}

// Inspection holds the PRAGMA user_version and PRAGMA integrity_check results
// returned by Node.Inspect.
type Inspection struct {
	UserVersion int
	Integrity   string
}

func (n *Node) sidecar(id, suffix string) string {
	return filepath.Join(n.config.Dir, id+suffix)
}

func (n *Node) isSealed(id string) bool {
	_, err := os.Stat(n.sidecar(id, ".sealed"))
	return err == nil
}

func (n *Node) refuseSealed(id string) error {
	if n.isSealed(id) {
		return fmt.Errorf("%w: %s", ErrSealed, id)
	}
	return nil
}

// Seal performs relocation step 2 on the source node: it writes <id>.sealed
// atomically so subsequent Write calls return ErrSealed before draining.
// Sealing is a cooperative courtesy; the store fence in Activate is what binds
// an uncooperative source.
func (n *Node) Seal(ctx context.Context, id string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if _, err := n.path(id); err != nil {
		return err
	}
	if err := n.fenced[id]; err != nil {
		return err
	}
	epoch, ok := n.owned[id]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotOwner, id)
	}
	return n.writeFile(id, ".sealed", fmt.Sprintf("epoch %d\n", epoch))
}

// Drain performs relocation step 3 on a sealed source node: it acquires id
// (reopening and shipping any unshipped local commits if needed) and evicts it
// so evicted runs a final walship cycle, closes the last SQLite connection
// (triggering sqlite3WalClose at src/wal.c:2507-2571 to checkpoint and delete
// -wal/-shm), and proves the closed file's sha256 equals the shipped state.
// Any eviction problem is returned as an error rather than left in n.problems.
func (n *Node) Drain(ctx context.Context, id string) (Drained, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if _, err := n.path(id); err != nil {
		return Drained{}, err
	}
	if !n.isSealed(id) {
		return Drained{}, fmt.Errorf("%w: %s", ErrNotSealed, id)
	}
	h, _, err := n.acquire(ctx, id)
	if err != nil {
		return Drained{}, err
	}
	c := n.open[id]
	h.Release()
	if c == nil {
		return Drained{}, fmt.Errorf("node: %s has no open shipper", id)
	}
	// Capture problems emitted by this eviction so Drain fails explicitly if
	// either the at-rest verification, .atrest write, or catalog update failed.
	before := len(n.problems)
	n.evictIfOpen(id)
	problems := append([]string(nil), n.problems[before:]...)
	n.problems = n.problems[:before]
	if err := n.fenced[id]; err != nil {
		return Drained{}, err
	}
	digest, ok := n.atRest[id]
	if !ok {
		return Drained{}, fmt.Errorf("node: drain %s: the final ship did not leave the shipped state at rest: %s", id, strings.Join(problems, "; "))
	}
	if len(problems) > 0 {
		return Drained{}, fmt.Errorf("node: drain %s: the final ship left the state at rest but reported: %s", id, strings.Join(problems, "; "))
	}
	return Drained{Position: c.shipper.Position(), SHA256: digest}, nil
}

// Stage performs relocation step 4 on the destination node: it lists the slot
// chain through a fence.Adapter at epoch (failing with FencedError if a newer
// epoch is already in the store), rebuilds the database image at the chain's
// end, and writes it to <id>.staged without installing it as live.
func (n *Node) Stage(ctx context.Context, id string, epoch uint64) (Staged, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	path, err := n.path(id)
	if err != nil {
		return Staged{}, err
	}
	adapter, err := fence.New(n.config.Store, id, epoch)
	if err != nil {
		return Staged{}, err
	}
	end, err := adapter.Refresh(ctx)
	if err != nil {
		return Staged{}, err
	}
	if end == 0 {
		return Staged{}, fmt.Errorf("node: stage %s: the store holds nothing to rebuild", id)
	}
	image, err := n.imageAt(ctx, adapter, path, end)
	if err != nil {
		return Staged{}, err
	}
	staged := n.sidecar(id, ".staged")
	if err := recipe.RemoveDatabase(staged); err != nil {
		return Staged{}, err
	}
	if err := walship.WriteDatabase(staged, image); err != nil {
		return Staged{}, err
	}
	return Staged{Position: end, SHA256: walship.SHA256Hex(image)}, nil
}

func fenceSnapshot(position int64, image []byte) ([]byte, uint32, error) {
	pageSize, err := pageSizeOf(image)
	if err != nil {
		return nil, 0, err
	}
	meta := walship.SnapshotMeta{
		Position: position, Reason: ReasonFence, PageSize: pageSize, Pages: uint32(len(image) / pageSize),
		UserVersion: binary.BigEndian.Uint32(image[60:64]), StateSHA256: walship.SHA256Hex(image),
	}
	object, err := walship.EncodeSnapshot(meta, image)
	return object, meta.UserVersion, err
}

// Activate performs relocation step 6 on the destination node. It verifies no
// newer epoch exists in the store, puts the fence snapshot at position+1 with
// put-if-absent (unless fenceStore is false for the skip-fence mutant, or the
// fence already landed on an earlier attempt and is adopted), installs the
// verified image as <id>.db, writes <id>.owner, clears temporary sidecars, and
// records the new location and position in the catalog.
//
// When called again on a node that already holds epoch and whose fence is
// already in the store, Activate repairs the catalog row if the store still
// ends at position+1 (recovering from a prior catalog error or crash after
// writeBelief) without overwriting later writes if the chain has moved on.
func (n *Node) Activate(ctx context.Context, id string, epoch uint64, position int64, digest string, fenceStore bool) (Activated, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	path, err := n.path(id)
	if err != nil {
		return Activated{}, err
	}
	if position < 1 || !sha256Pattern.MatchString(digest) {
		return Activated{}, fmt.Errorf("node: activate %s at position %d with sha256 %q", id, position, digest)
	}
	chain, err := fence.Chain(ctx, n.config.Store, id)
	if err != nil {
		return Activated{}, err
	}
	var image []byte
	var fenceSlot *fence.Slot
	result := Activated{Fence: position + 1}
	for _, slot := range chain {
		if slot.Epoch > epoch {
			return Activated{}, &fence.FencedError{Database: id, Epoch: epoch, Holder: slot.Epoch, Position: slot.First}
		}
	}
	for i, slot := range chain {
		if slot.Epoch == epoch && fenceSlot == nil {
			if slot.First != position+1 || slot.Kind != fence.KindSnapshot {
				return Activated{}, fmt.Errorf("node: activate %s: epoch %d starts at position %d with a %s, not with the fence at %d", id, epoch, slot.First, slot.Kind, position+1)
			}
			fenceSlot = &chain[i]
		}
	}
	if fenceSlot != nil {
		if owned, ok := n.owned[id]; ok && owned == epoch {
			result.Adopted = true
			// If the node already shipped writes past the fence, leave the
			// catalog alone so a retried Activate never rewinds last_shipped.
			if chainEnd(chain) != position+1 {
				return result, nil
			}
			meta, _, err := walship.DecodeSnapshot(fenceSlot.Object)
			if err != nil {
				return Activated{}, err
			}
			return result, n.recordShipped(ctx, id, epoch, position+1, int(meta.UserVersion))
		}
		// Adopt an already-landed fence (for example after fail-after-put or
		// crash-after-put) by decoding the image straight from the store slot.
		if end := chainEnd(chain); end != position+1 {
			return Activated{}, fmt.Errorf("node: activate %s: epoch %d wrote through position %d after its fence, and this node does not hold it", id, epoch, end)
		}
		if _, image, err = walship.DecodeSnapshot(fenceSlot.Object); err != nil {
			return Activated{}, err
		}
		result.Adopted = true
	}
	if image == nil {
		if end := chainEnd(chain); end != position {
			return Activated{}, fmt.Errorf("%w: %s: the store ends at position %d, the drain ended at %d", ErrMovedAfterDrain, id, end, position)
		}
		if image, err = os.ReadFile(n.sidecar(id, ".staged")); err != nil {
			return Activated{}, err
		}
	}
	if walship.SHA256Hex(image) != digest {
		return Activated{}, fmt.Errorf("%w: %s: %s, drained %s", ErrStagedMismatch, id, walship.SHA256Hex(image), digest)
	}
	object, userVersion, err := fenceSnapshot(position+1, image)
	if err != nil {
		return Activated{}, err
	}
	shipped := position + 1
	switch {
	case result.Adopted:
	case fenceStore:
		adapter, err := fence.New(n.config.Store, id, epoch)
		if err != nil {
			return Activated{}, err
		}
		if err := adapter.PutIfAbsent(ctx, walship.SnapshotKey(position+1), object); err != nil {
			return Activated{}, err
		}
	default:
		result.Fence, shipped = 0, position
	}
	n.evictIfOpen(id)
	if err := recipe.RemoveDatabase(path); err != nil {
		return result, err
	}
	if err := walship.WriteDatabase(path, image); err != nil {
		return result, err
	}
	if err := errors.Join(recipe.RemoveDatabase(n.sidecar(id, ".staged")), n.removeFile(id, ".atrest"), n.removeFile(id, ".sealed")); err != nil {
		return result, err
	}
	if err := n.writeBelief(id, epoch); err != nil {
		return result, err
	}
	delete(n.fenced, id)
	delete(n.atRest, id)
	n.owned[id] = epoch
	return result, n.recordShipped(ctx, id, epoch, shipped, int(userVersion))
}

func chainEnd(chain []fence.Slot) int64 {
	if len(chain) == 0 {
		return 0
	}
	return chain[len(chain)-1].Last
}

// Retire performs relocation step 7 on the source node: after verifying an
// owned database is sealed, it evicts any open handle, drops local ownership
// state, and deletes the database file and all four per-database sidecars.
func (n *Node) Retire(ctx context.Context, id string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	path, err := n.path(id)
	if err != nil {
		return err
	}
	if _, owned := n.owned[id]; owned && !n.isSealed(id) {
		return fmt.Errorf("%w: %s is owned by this node and still writable", ErrNotSealed, id)
	}
	n.evictIfOpen(id)
	delete(n.owned, id)
	delete(n.fenced, id)
	delete(n.atRest, id)
	return errors.Join(recipe.RemoveDatabase(path), recipe.RemoveDatabase(n.sidecar(id, ".staged")),
		n.removeFile(id, ".owner"), n.removeFile(id, ".atrest"), n.removeFile(id, ".sealed"))
}

// Discard removes any leftover <id>.staged image on a destination node when a
// relocation is rolled back before activation.
func (n *Node) Discard(ctx context.Context, id string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if _, err := n.path(id); err != nil {
		return err
	}
	return recipe.RemoveDatabase(n.sidecar(id, ".staged"))
}

// Status reports this node's local view of id ("fenced", "not owner",
// "owner at epoch E, sealed", or "owner at epoch E, writable") for relocation
// boundary assertions.
func (n *Node) Status(id string) string {
	n.mu.Lock()
	defer n.mu.Unlock()
	if _, err := n.path(id); err != nil {
		return "invalid id"
	}
	if n.fenced[id] != nil {
		return "fenced"
	}
	epoch, ok := n.owned[id]
	switch {
	case !ok:
		return "not owner"
	case n.isSealed(id):
		return fmt.Sprintf("owner at epoch %d, sealed", epoch)
	}
	return fmt.Sprintf("owner at epoch %d, writable", epoch)
}

// Inspect opens database id through Read and returns its PRAGMA integrity_check
// and PRAGMA user_version results (used after crash recovery in Experiment 10.2).
func (n *Node) Inspect(ctx context.Context, id string) (Inspection, error) {
	var inspection Inspection
	err := n.Read(ctx, id, func(ctx context.Context, db *sqlitex.DB) error {
		if err := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&inspection.Integrity); err != nil {
			return err
		}
		return db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&inspection.UserVersion)
	})
	return inspection, err
}
