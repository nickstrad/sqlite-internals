package node

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickstrad/sqlite-internals/ch10-multi-database-platform-capstone/service/catalog"
	"github.com/nickstrad/sqlite-internals/ch10-multi-database-platform-capstone/service/fence"
	"github.com/nickstrad/sqlite-internals/internal/connmgr"
	"github.com/nickstrad/sqlite-internals/internal/recipe"
	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
	"github.com/nickstrad/sqlite-internals/internal/walship"
)

type cluster struct {
	work    string
	catalog *catalog.Catalog
	store   walship.Store
}

func newCluster(t *testing.T, ids ...string) *cluster {
	t.Helper()
	ctx := context.Background()
	work := t.TempDir()
	cat, err := catalog.Open(ctx, filepath.Join(work, "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cat.Close() })
	store, err := walship.NewDirStore(filepath.Join(work, "store"))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if err := cat.Register(ctx, id, PlatformSettings()); err != nil {
			t.Fatal(err)
		}
	}
	return &cluster{work: work, catalog: cat, store: store}
}

func (c *cluster) start(t *testing.T, name string, capacity int) *Node {
	t.Helper()
	workload := recipe.Default()
	n, err := Start(Config{
		Name: name, Dir: filepath.Join(c.work, name), Catalog: c.catalog, Store: c.store, Capacity: capacity,
		File:   workload.SQLiteOptions().File,
		Create: func(ctx context.Context, path string) error { return workload.Build(ctx, path, 0) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func nextStep(ctx context.Context, db *sqlitex.DB) error {
	_, err := recipe.Default().ApplyNext(ctx, db)
	return err
}

func mustWrite(t *testing.T, n *Node, id string, mode AckMode) Ack {
	t.Helper()
	ack, err := n.Write(context.Background(), id, mode, nextStep)
	if err != nil {
		t.Fatalf("write %s %s: %v", id, mode, err)
	}
	return ack
}

// TestTakeoverFencesTheOldOwner verifies that a second node's Claim restores the last shipped position, fences the old owner's writes, removes its belief file, and leaves the new owner's closed file equal to RestoreAt.
func TestTakeoverFencesTheOldOwner(t *testing.T) {
	ctx := context.Background()
	c := newCluster(t, "db-1")
	a, b := c.start(t, "a", 2), c.start(t, "b", 2)
	if result, err := a.Claim(ctx, "db-1"); err != nil || result != (ClaimResult{Epoch: 1, RestoredFrom: 0, Fence: 1, Attempts: 1}) {
		t.Fatalf("%+v %v", result, err)
	}
	if ack := mustWrite(t, a, "db-1", StoreDurable); ack.Position != 2 || ack.UserVersion != 1 || ack.Epoch != 1 {
		t.Fatalf("%+v", ack)
	}
	if ack := mustWrite(t, a, "db-1", LocalDurable); ack.Position != 0 || ack.UserVersion != 2 {
		t.Fatalf("%+v", ack)
	}
	if result, err := b.Claim(ctx, "db-1"); err != nil || result.Epoch != 2 || result.RestoredFrom != 2 || result.Fence != 3 {
		t.Fatalf("%+v %v", result, err)
	}
	_, err := a.Write(ctx, "db-1", StoreDurable, nextStep)
	var fenced *fence.FencedError
	if !errors.As(err, &fenced) || fenced.Holder != 2 || fenced.Position != 3 {
		t.Fatalf("stale owner write: %v", err)
	}
	if _, err := a.Write(ctx, "db-1", LocalDurable, nextStep); !errors.Is(err, fence.ErrFenced) {
		t.Fatalf("second stale write: %v", err)
	}
	if _, ok := a.Owned("db-1"); ok {
		t.Fatal("fenced node still owns db-1")
	}
	if _, err := os.Stat(filepath.Join(c.work, "a", "db-1.owner")); !os.IsNotExist(err) {
		t.Fatalf("belief file kept: %v", err)
	}
	if ack := mustWrite(t, b, "db-1", StoreDurable); ack.Position != 4 || ack.UserVersion != 2 {
		t.Fatalf("%+v", ack)
	}
	if err := errors.Join(a.Close(), b.Close()); err != nil {
		t.Fatal(err)
	}
	chain, restorable, err := fence.Audit(ctx, c.store, "db-1")
	if err != nil || len(chain) != 4 || chain[2].Epoch != 2 || chain[3].Last != 4 {
		t.Fatalf("%+v %v", chain, err)
	}
	image, err := fence.RestoreAt(restorable, 4)
	if err != nil {
		t.Fatal(err)
	}
	status, err := connmgr.CheckAtRest(filepath.Join(c.work, "b", "db-1.db"))
	if err != nil || !status.AtRest || status.SHA256 != walship.SHA256Hex(image) {
		t.Fatalf("owner file %v %v", status, err)
	}
	record, err := c.catalog.Get(ctx, "db-1")
	if err != nil || record.Owner != "b" || record.Epoch != 2 || record.LastShipped != 4 || record.SchemaVersion != 2 {
		t.Fatalf("%+v %v", record, err)
	}
}

// TestRestartedNodeBelievesItsClaimUntilTheStoreSaysOtherwise checks that a restarted stale owner loads its .owner file and is fenced on its first write when walship lists the store.
func TestRestartedNodeBelievesItsClaimUntilTheStoreSaysOtherwise(t *testing.T) {
	ctx := context.Background()
	c := newCluster(t, "db-1")
	a := c.start(t, "a", 2)
	if _, err := a.Claim(ctx, "db-1"); err != nil {
		t.Fatal(err)
	}
	b := c.start(t, "b", 2)
	if _, err := b.Claim(ctx, "db-1"); err != nil {
		t.Fatal(err)
	}
	restarted := c.start(t, "a", 2)
	if epoch, ok := restarted.Owned("db-1"); !ok || epoch != 1 {
		t.Fatalf("restarted node belief %d %t", epoch, ok)
	}
	if _, err := restarted.Write(ctx, "db-1", StoreDurable, nextStep); !errors.Is(err, fence.ErrFenced) {
		t.Fatalf("restarted stale owner: %v", err)
	}
	if _, ok := restarted.Owned("db-1"); ok {
		t.Fatal("belief kept after the store fenced it")
	}
	if err := errors.Join(restarted.Close(), b.Close(), a.Close()); err != nil {
		t.Fatal(err)
	}
}

// TestEvictionShipsAndVerifiesTheFileAtRest verifies LRU eviction ships local-durable writes, AdoptAtRest skips a reopen snapshot, and offline tampering or deletion is rejected until re-claimed.
func TestEvictionShipsAndVerifiesTheFileAtRest(t *testing.T) {
	ctx := context.Background()
	c := newCluster(t, "db-1", "db-2")
	a := c.start(t, "a", 1)
	for _, id := range []string{"db-1", "db-2"} {
		if _, err := a.Claim(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite(t, a, "db-1", LocalDurable)
	mustWrite(t, a, "db-2", LocalDurable)
	if problems := a.TakeProblems(); len(problems) != 0 {
		t.Fatal(problems)
	}
	chain, _, err := fence.Audit(ctx, c.store, "db-1")
	if err != nil || len(chain) != 2 || chain[1].Kind != fence.KindSegment || chain[1].Last != 2 {
		t.Fatalf("eviction did not ship the local-durable write: %+v %v", chain, err)
	}
	if ack := mustWrite(t, a, "db-1", StoreDurable); ack.Position != 3 || ack.UserVersion != 2 {
		t.Fatalf("%+v", ack)
	}
	if stats := a.Stats(); stats.MaxOpen != 1 || stats.Evictions != 2 {
		t.Fatalf("%+v", stats)
	}
	outside, err := sqlitex.Open(ctx, filepath.Join(c.work, "a", "db-2.db"), recipe.Default().SQLiteOptions())
	if err != nil {
		t.Fatal(err)
	}
	if err := nextStep(ctx, outside); err != nil {
		t.Fatal(err)
	}
	if err := outside.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Write(ctx, "db-2", StoreDurable, nextStep); !errors.Is(err, ErrChangedWhileShut) {
		t.Fatalf("outside change accepted: %v", err)
	}
	if err := os.Remove(filepath.Join(c.work, "a", "db-2.db")); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Write(ctx, "db-2", StoreDurable, nextStep); !errors.Is(err, ErrMissingLocalFile) {
		t.Fatalf("missing file: %v", err)
	}
	if result, err := a.Claim(ctx, "db-2"); err != nil || result.RestoredFrom != 2 || result.Fence != 3 {
		t.Fatalf("reclaim %+v %v", result, err)
	}
	if ack := mustWrite(t, a, "db-2", StoreDurable); ack.UserVersion != 2 || ack.Position != 4 {
		t.Fatalf("%+v", ack)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestLocalDurableNeedsSynchronousFull verifies LocalDurable refuses synchronous=NORMAL while StoreDurable succeeds, and unknown modes or unowned ids are rejected.
func TestLocalDurableNeedsSynchronousFull(t *testing.T) {
	ctx := context.Background()
	c := newCluster(t)
	settings := PlatformSettings()
	settings.Synchronous = sqlitex.SyncNormal
	if err := c.catalog.Register(ctx, "db-1", settings); err != nil {
		t.Fatal(err)
	}
	a := c.start(t, "a", 1)
	if _, err := a.Claim(ctx, "db-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Write(ctx, "db-1", LocalDurable, nextStep); !errors.Is(err, ErrNotLocallyDurable) {
		t.Fatalf("NORMAL accepted for local-durable: %v", err)
	}
	if ack := mustWrite(t, a, "db-1", StoreDurable); ack.UserVersion != 1 {
		t.Fatalf("%+v", ack)
	}
	if _, err := a.Write(ctx, "db-1", "eventually", nextStep); err == nil {
		t.Fatal("unknown mode accepted")
	}
	if _, err := a.Write(ctx, "db-9", StoreDurable, nextStep); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("not owner: %v", err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
}
