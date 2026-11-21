package fence

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/nickstrad/walfleet/internal/walship"
)

func snapshotObject(t *testing.T, position int64) []byte {
	t.Helper()
	image := make([]byte, 4096)
	image[0] = byte(position)
	object, err := walship.EncodeSnapshot(walship.SnapshotMeta{Position: position, Reason: "fence", PageSize: 4096, Pages: 1, StateSHA256: walship.SHA256Hex(image)}, image)
	if err != nil {
		t.Fatal(err)
	}
	return object
}

func segmentObject(t *testing.T, first, last int64) []byte {
	t.Helper()
	frames := int(last - first + 1)
	meta := walship.SegmentMeta{FirstPosition: first, LastPosition: last, Generation: walship.Generation{Header: hex.EncodeToString(make([]byte, 32))}, FirstFrame: 1, LastFrame: frames, PageSize: 4096, AfterSHA256: "after"}
	for i := 0; i < frames; i++ {
		meta.Commits = append(meta.Commits, walship.CommitPoint{Position: first + int64(i), Frame: i + 1, Pages: 1})
	}
	meta.Commits[frames-1].StateSHA256 = "after"
	object, err := walship.EncodeSegment(meta, make([]byte, frames*(4096+24)))
	if err != nil {
		t.Fatal(err)
	}
	return object
}

func newStore(t *testing.T) *walship.DirStore {
	t.Helper()
	store, err := walship.NewDirStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func adapter(t *testing.T, store walship.Store, epoch uint64) *Adapter {
	t.Helper()
	a, err := New(store, "db-1", epoch)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// TestEnvelopeRoundTripAndStrictDecode verifies valid slot encoding and checks that 12 malformed envelopes are rejected as ErrDamaged.
func TestEnvelopeRoundTripAndStrictDecode(t *testing.T) {
	ctx := context.Background()
	key := SlotKey("db-1", 1)
	data := Encode(3, KindSnapshot, 1, 1, snapshotObject(t, 1))
	slot, err := Decode(key, data)
	if err != nil || slot.Epoch != 3 || slot.Kind != KindSnapshot || slot.First != 1 || slot.Last != 1 {
		t.Fatalf("%+v %v", slot, err)
	}
	bad := map[string][]byte{
		"no newline":        []byte("fenced-slot v1 epoch=1"),
		"other tag":         append([]byte("fenced-slot v2 epoch=3 kind=snapshot first=1 last=1\n"), snapshotObject(t, 1)...),
		"padded number":     append([]byte("fenced-slot v1 epoch=03 kind=snapshot first=1 last=1\n"), snapshotObject(t, 1)...),
		"epoch zero":        Encode(0, KindSnapshot, 1, 1, snapshotObject(t, 1)),
		"key position":      Encode(3, KindSnapshot, 2, 2, snapshotObject(t, 2)),
		"unknown kind":      Encode(3, "manifest", 1, 1, snapshotObject(t, 1)),
		"kind mismatch":     Encode(3, KindSegment, 1, 1, snapshotObject(t, 1)),
		"object position":   Encode(3, KindSnapshot, 1, 1, snapshotObject(t, 7)),
		"segment range":     Encode(3, KindSegment, 1, 3, segmentObject(t, 1, 2)),
		"truncated object":  Encode(3, KindSnapshot, 1, 1, snapshotObject(t, 1)[:100]),
		"snapshot range":    Encode(3, KindSnapshot, 1, 2, snapshotObject(t, 1)),
		"trailing envelope": append([]byte("fenced-slot v1 epoch=3 kind=snapshot first=1 last=1 x\n"), snapshotObject(t, 1)...),
	}
	for name, data := range bad {
		if _, err := Decode(key, data); !errors.Is(err, ErrDamaged) {
			t.Errorf("%s: %v", name, err)
		}
	}
	store := newStore(t)
	if err := store.PutIfAbsent(ctx, key, bad["truncated object"]); err != nil {
		t.Fatal(err)
	}
	if _, err := adapter(t, store, 1).Refresh(ctx); !errors.Is(err, ErrDamaged) {
		t.Fatalf("damaged slot listed: %v", err)
	}
}

// TestAdapterAppendsOnlyAtTheEnd checks contiguous appending, rejection of out-of-order or overlapping puts, key translation on List/Get, and refusal of Delete.
func TestAdapterAppendsOnlyAtTheEnd(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	a := adapter(t, store, 1)
	if err := a.PutIfAbsent(ctx, walship.SnapshotKey(2), snapshotObject(t, 2)); !errors.Is(err, ErrOutOfOrder) {
		t.Fatalf("put at 2 on an empty chain: %v", err)
	}
	if err := a.PutIfAbsent(ctx, walship.SnapshotKey(1), snapshotObject(t, 1)); err != nil {
		t.Fatal(err)
	}
	if err := a.PutIfAbsent(ctx, walship.SegmentKey(2), segmentObject(t, 2, 4)); err != nil {
		t.Fatal(err)
	}
	if err := a.PutIfAbsent(ctx, walship.SegmentKey(4), segmentObject(t, 4, 4)); !errors.Is(err, ErrOutOfOrder) {
		t.Fatalf("overlapping put: %v", err)
	}
	keys, err := a.List(ctx, "")
	if err != nil || strings.Join(keys, ",") != "segments/000000000002,snapshots/000000000001" {
		t.Fatalf("%v %v", keys, err)
	}
	object, err := a.Get(ctx, walship.SegmentKey(2))
	if err != nil || !bytes.Equal(object, segmentObject(t, 2, 4)) {
		t.Fatalf("get: %v", err)
	}
	if _, err := a.Get(ctx, walship.SnapshotKey(2)); !errors.Is(err, walship.ErrNotFound) {
		t.Fatalf("snapshot key for a segment slot: %v", err)
	}
	if end, err := adapter(t, store, 1).Refresh(ctx); end != 4 || err != nil {
		t.Fatalf("end %d %v", end, err)
	}
	if err := a.Delete(ctx, walship.SnapshotKey(1)); err == nil {
		t.Fatal("delete allowed")
	}
}

// TestNewerEpochFencesTheOlderWriter verifies that a higher epoch's fence blocks both active and restarted older-epoch adapters with FencedError.
func TestNewerEpochFencesTheOlderWriter(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	old := adapter(t, store, 1)
	if err := old.PutIfAbsent(ctx, walship.SnapshotKey(1), snapshotObject(t, 1)); err != nil {
		t.Fatal(err)
	}
	newer := adapter(t, store, 2)
	if end, err := newer.Refresh(ctx); end != 1 || err != nil {
		t.Fatalf("%d %v", end, err)
	}
	if err := newer.PutIfAbsent(ctx, walship.SnapshotKey(2), snapshotObject(t, 2)); err != nil {
		t.Fatal(err)
	}
	err := old.PutIfAbsent(ctx, walship.SegmentKey(2), segmentObject(t, 2, 2))
	var fenced *FencedError
	if !errors.As(err, &fenced) || !errors.Is(err, ErrFenced) || fenced.Holder != 2 || fenced.Epoch != 1 || fenced.Position != 2 {
		t.Fatalf("old writer not fenced: %v", err)
	}
	if _, err := adapter(t, store, 1).Refresh(ctx); !errors.Is(err, ErrFenced) {
		t.Fatalf("restarted old writer not fenced at refresh: %v", err)
	}
	stale := adapter(t, store, 3)
	if err := stale.PutIfAbsent(ctx, walship.SnapshotKey(3), snapshotObject(t, 3)); err != nil {
		t.Fatal(err)
	}
	if err := newer.PutIfAbsent(ctx, walship.SegmentKey(3), segmentObject(t, 3, 3)); !errors.Is(err, ErrFenced) {
		t.Fatalf("epoch 2 not fenced by 3: %v", err)
	}
}

// TestRetriedIdenticalPutSucceeds checks that an ErrExists collision with byte-identical content is treated as success and advances the adapter's chain.
func TestRetriedIdenticalPutSucceeds(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	object := snapshotObject(t, 1)
	if err := store.PutIfAbsent(ctx, SlotKey("db-1", 1), Encode(2, KindSnapshot, 1, 1, object)); err != nil {
		t.Fatal(err)
	}
	same := &Adapter{Base: store, Database: "db-1", Epoch: 2, slots: map[int64]Slot{}, listed: true}
	if err := same.PutIfAbsent(ctx, walship.SnapshotKey(1), object); err != nil {
		t.Fatalf("identical retry: %v", err)
	}
	if same.end != 1 || same.slots[1].Epoch != 2 {
		t.Fatalf("retry did not advance the chain: end %d", same.end)
	}
}

// TestLowerEpochHolderIsReportedAsExists checks that colliding with an older epoch returns walship.ErrExists rather than ErrFenced so a claim can re-list and retry.
func TestLowerEpochHolderIsReportedAsExists(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	if err := store.PutIfAbsent(ctx, SlotKey("db-1", 1), Encode(1, KindSnapshot, 1, 1, snapshotObject(t, 1))); err != nil {
		t.Fatal(err)
	}
	stale := &Adapter{Base: store, Database: "db-1", Epoch: 2, slots: map[int64]Slot{}, listed: true}
	err := stale.PutIfAbsent(ctx, walship.SnapshotKey(1), snapshotObject(t, 1))
	if !errors.Is(err, walship.ErrExists) || errors.Is(err, ErrFenced) {
		t.Fatalf("%v", err)
	}
}

// TestCheckLineage verifies valid lineages pass and five malformed chain shapes fail with ErrBadChain.
func TestCheckLineage(t *testing.T) {
	snap := func(epoch uint64, p int64) Slot { return Slot{Epoch: epoch, Kind: KindSnapshot, First: p, Last: p} }
	seg := func(epoch uint64, first, last int64) Slot {
		return Slot{Epoch: epoch, Kind: KindSegment, First: first, Last: last}
	}
	good := []Slot{snap(1, 1), seg(1, 2, 3), snap(2, 4), seg(2, 5, 5)}
	if err := CheckLineage(good); err != nil {
		t.Fatal(err)
	}
	for name, chain := range map[string][]Slot{
		"starts late":           {snap(1, 2)},
		"starts with segment":   {seg(1, 1, 1)},
		"gap":                   {snap(1, 1), seg(1, 3, 3)},
		"epoch goes down":       {snap(2, 1), snap(1, 2)},
		"new epoch via segment": {snap(1, 1), seg(2, 2, 2)},
	} {
		if err := CheckLineage(chain); !errors.Is(err, ErrBadChain) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// TestNewRejectsBadIdentity checks that New refuses invalid database ids and epoch 0.
func TestNewRejectsBadIdentity(t *testing.T) {
	store := newStore(t)
	for _, id := range []string{"", "a/b", "DB", "../x"} {
		if _, err := New(store, id, 1); err == nil {
			t.Errorf("id %q accepted", id)
		}
	}
	if _, err := New(store, "db", 0); err == nil {
		t.Error("epoch 0 accepted")
	}
}

// TestListAfterFencedRefreshStaysFenced verifies that a fenced Refresh does not cache a partial pre-fence view that subsequent List or Put calls would accept.
func TestListAfterFencedRefreshStaysFenced(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	if err := adapter(t, store, 1).PutIfAbsent(ctx, walship.SnapshotKey(1), snapshotObject(t, 1)); err != nil {
		t.Fatal(err)
	}
	newer := adapter(t, store, 2)
	if _, err := newer.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if err := newer.PutIfAbsent(ctx, walship.SnapshotKey(2), snapshotObject(t, 2)); err != nil {
		t.Fatal(err)
	}
	old := adapter(t, store, 1)
	if _, err := old.Refresh(ctx); !errors.Is(err, ErrFenced) {
		t.Fatalf("refresh: %v", err)
	}
	if keys, err := old.List(ctx, ""); !errors.Is(err, ErrFenced) {
		t.Fatalf("list after fenced refresh: %v %v", keys, err)
	}
	if err := old.PutIfAbsent(ctx, walship.SegmentKey(2), segmentObject(t, 2, 2)); !errors.Is(err, ErrFenced) {
		t.Fatalf("put after fenced refresh: %v", err)
	}
}

// TestMalformedPutNeverReachesTheStore confirms PutIfAbsent validates envelopes locally so corrupt objects are never written into immutable slots.
func TestMalformedPutNeverReachesTheStore(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	a := adapter(t, store, 1)
	if err := a.PutIfAbsent(ctx, walship.SnapshotKey(1), snapshotObject(t, 1)[:100]); !errors.Is(err, ErrDamaged) {
		t.Fatalf("truncated snapshot: %v", err)
	}
	if err := a.PutIfAbsent(ctx, walship.SnapshotKey(1), snapshotObject(t, 7)); !errors.Is(err, ErrDamaged) {
		t.Fatalf("snapshot for another position: %v", err)
	}
	if err := a.PutIfAbsent(ctx, walship.SnapshotKey(1), snapshotObject(t, 1)); err != nil {
		t.Fatal(err)
	}
	if err := a.PutIfAbsent(ctx, walship.SegmentKey(2), segmentObject(t, 3, 3)); !errors.Is(err, ErrDamaged) {
		t.Fatalf("segment for another position: %v", err)
	}
	keys, err := store.List(ctx, SlotPrefix("db-1"))
	if err != nil || len(keys) != 1 {
		t.Fatalf("store holds %v %v", keys, err)
	}
	if end, err := adapter(t, store, 1).Refresh(ctx); end != 1 || err != nil {
		t.Fatalf("chain end %d %v", end, err)
	}
}

// TestDecodeRejectsMalformedKeys checks that Decode rejects empty, uppercase, or wrongly prefixed slot keys.
func TestDecodeRejectsMalformedKeys(t *testing.T) {
	data := Encode(1, KindSnapshot, 1, 1, snapshotObject(t, 1))
	for _, key := range []string{"dbs//log/000000000001", "x/db-1/log/000000000001", "dbs/db-1/seg/000000000001", "dbs/DB/log/000000000001"} {
		if _, err := Decode(key, data); !errors.Is(err, ErrDamaged) {
			t.Errorf("%s: %v", key, err)
		}
	}
}

// TestAdapterKeepsNoObjectBytes verifies that Adapter caches slot metadata only and reads object bytes from the underlying store on demand.
func TestAdapterKeepsNoObjectBytes(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	a := adapter(t, store, 1)
	if err := a.PutIfAbsent(ctx, walship.SnapshotKey(1), snapshotObject(t, 1)); err != nil {
		t.Fatal(err)
	}
	for p := int64(2); p <= 20; p++ {
		if err := a.PutIfAbsent(ctx, walship.SegmentKey(p), segmentObject(t, p, p)); err != nil {
			t.Fatal(err)
		}
	}
	reopened := adapter(t, store, 1)
	if _, err := reopened.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	for _, cached := range []*Adapter{a, reopened} {
		if len(cached.slots) != 20 {
			t.Fatalf("%d slots", len(cached.slots))
		}
		for position, slot := range cached.slots {
			if slot.Object != nil || slot.raw != nil {
				t.Fatalf("slot %d keeps %d object bytes", position, len(slot.Object))
			}
		}
		object, err := cached.Get(ctx, walship.SegmentKey(7))
		if err != nil || !bytes.Equal(object, segmentObject(t, 7, 7)) {
			t.Fatalf("get: %v", err)
		}
	}
}
