package relocate

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickstrad/walfleet/ch10-multi-database-platform-capstone/service/catalog"
	"github.com/nickstrad/walfleet/ch10-multi-database-platform-capstone/service/faultstore"
	"github.com/nickstrad/walfleet/ch10-multi-database-platform-capstone/service/fence"
	"github.com/nickstrad/walfleet/ch10-multi-database-platform-capstone/service/node"
	"github.com/nickstrad/walfleet/internal/connmgr"
	"github.com/nickstrad/walfleet/internal/recipe"
	"github.com/nickstrad/walfleet/internal/sqlitex"
	"github.com/nickstrad/walfleet/internal/walship"
)

type lab struct {
	t       *testing.T
	work    string
	catalog *catalog.Catalog
	store   walship.Store
	nodes   map[string]*node.Node
}

func newLab(t *testing.T) *lab {
	t.Helper()
	return newFaultyLab(t, nil)
}

func newFaultyLab(t *testing.T, faults map[string]faultstore.Rule) *lab {
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
	if err := cat.Register(ctx, "db-1", node.PlatformSettings()); err != nil {
		t.Fatal(err)
	}
	l := &lab{t: t, work: work, catalog: cat, store: store, nodes: map[string]*node.Node{}}
	for _, name := range []string{"a", "b"} {
		var nodeStore walship.Store
		if rule, ok := faults[name]; ok {
			nodeStore = faultstore.New(store, rule)
		}
		l.start(name, nodeStore)
	}
	return l
}

func (l *lab) start(name string, store walship.Store) *node.Node {
	l.t.Helper()
	if store == nil {
		store = l.store
	}
	workload := recipe.Default()
	n, err := node.Start(node.Config{
		Name: name, Dir: filepath.Join(l.work, name), Catalog: l.catalog, Store: store, Capacity: 2,
		File:   workload.SQLiteOptions().File,
		Create: func(ctx context.Context, path string) error { return workload.Build(ctx, path, 0) },
	})
	if err != nil {
		l.t.Fatal(err)
	}
	l.nodes[name] = n
	l.t.Cleanup(func() { n.Close() })
	return n
}

func (l *lab) coordinator() *Coordinator {
	return &Coordinator{Catalog: l.catalog, Store: l.store, Node: func(name string) Participant { return l.nodes[name] }}
}

func nextStep(ctx context.Context, db *sqlitex.DB) error {
	_, err := recipe.Default().ApplyNext(ctx, db)
	return err
}

func (l *lab) write(name string, mode node.AckMode) (node.Ack, error) {
	return l.nodes[name].Write(context.Background(), "db-1", mode, nextStep)
}

func (l *lab) prepare() {
	l.t.Helper()
	if _, err := l.nodes["a"].Claim(context.Background(), "db-1"); err != nil {
		l.t.Fatal(err)
	}
	for _, mode := range []node.AckMode{node.StoreDurable, node.StoreDurable, node.LocalDurable} {
		if _, err := l.write("a", mode); err != nil {
			l.t.Fatal(err)
		}
	}
}

func (l *lab) closeAll() {
	l.t.Helper()
	for name, n := range l.nodes {
		if err := n.Close(); err != nil {
			l.t.Fatalf("close %s: %v", name, err)
		}
	}
}

func (l *lab) requireOwner(owner string, epoch uint64, userVersion int) {
	l.t.Helper()
	ctx := context.Background()
	record, err := l.catalog.Get(ctx, "db-1")
	if err != nil || record.Owner != owner || record.Epoch != epoch {
		l.t.Fatalf("catalog %+v %v, want owner %s epoch %d", record, err, owner, epoch)
	}
	writable := 0
	for name, n := range l.nodes {
		if n.Status("db-1") == "owner at epoch "+itoa(epoch)+", writable" {
			if name != owner {
				l.t.Fatalf("node %s is writable, the owner is %s", name, owner)
			}
			writable++
		}
	}
	if writable != 1 {
		l.t.Fatalf("%d writable nodes", writable)
	}
	chain, restorable, err := fence.Audit(ctx, l.store, "db-1")
	if err != nil {
		l.t.Fatal(err)
	}
	if chain[len(chain)-1].Epoch != epoch {
		l.t.Fatalf("the newest slot has epoch %d, want %d", chain[len(chain)-1].Epoch, epoch)
	}
	image, err := fence.RestoreAt(restorable, chain[len(chain)-1].Last)
	if err != nil {
		l.t.Fatal(err)
	}
	if got := int(binary.BigEndian.Uint32(image[60:64])); got != userVersion {
		l.t.Fatalf("the store's last state has user_version %d, want %d", got, userVersion)
	}
	l.closeAll()
	status, err := connmgr.CheckAtRest(filepath.Join(l.work, owner, "db-1.db"))
	if err != nil || !status.AtRest || status.SHA256 != walship.SHA256Hex(image) {
		l.t.Fatalf("the owner's file %s %v is not the store's last state", status, err)
	}
}

func itoa(v uint64) string {
	digits := []byte{}
	for {
		digits = append([]byte{byte('0' + v%10)}, digits...)
		v /= 10
		if v == 0 {
			return string(digits)
		}
	}
}

// TestRelocationMovesOwnershipAndKeepsEveryWrite runs a full relocation end to end, confirming sealed-source refusal, retired-source cleanup, and destination writes.
func TestRelocationMovesOwnershipAndKeepsEveryWrite(t *testing.T) {
	l := newLab(t)
	l.prepare()
	var duringSeal error
	c := l.coordinator()
	c.AfterStep = func(step Step) {
		if step == SealSource {
			_, duringSeal = l.write("a", node.StoreDurable)
		}
	}
	result, err := c.Run(context.Background(), "db-1", "a", "b")
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(duringSeal, node.ErrSealed) {
		t.Fatalf("a write on the sealed source: %v", duringSeal)
	}
	if result.Outcome != Done || result.Position != 4 || result.Fence != 5 || result.Epoch != 2 {
		t.Fatalf("%+v", result)
	}
	if _, err := l.write("a", node.StoreDurable); !errors.Is(err, node.ErrNotOwner) {
		t.Fatalf("the retired source wrote: %v", err)
	}
	if _, err := os.Stat(filepath.Join(l.work, "a", "db-1.db")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the retired source kept its file: %v", err)
	}
	ack, err := l.write("b", node.StoreDurable)
	if err != nil || ack.Position != 6 || ack.UserVersion != 4 {
		t.Fatalf("%+v %v", ack, err)
	}
	r, err := l.catalog.GetRelocation(context.Background(), "db-1")
	if err != nil || r.State != catalog.RelocationDone || r.Step != int(RetireSource) || r.Position != 4 || r.SHA256 != result.SHA256 {
		t.Fatalf("%+v %v", r, err)
	}
	l.requireOwner("b", 2, 4)
}

// TestCrashAfterEveryStepResumesToOneOwner crashes the coordinator after each of the seven steps and verifies a fresh Coordinator rolls forward cleanly.
func TestCrashAfterEveryStepResumesToOneOwner(t *testing.T) {
	for _, step := range Steps() {
		t.Run(step.String(), func(t *testing.T) {
			l := newLab(t)
			l.prepare()
			c := l.coordinator()
			c.CrashAfter = step
			if _, err := c.Run(context.Background(), "db-1", "a", "b"); !errors.Is(err, ErrCrashed) {
				t.Fatalf("crash after %s: %v", step, err)
			}
			result, err := l.coordinator().Resume(context.Background(), "db-1")
			if err != nil {
				t.Fatal(err)
			}
			if result.Outcome != RolledForward || result.Position != 4 {
				t.Fatalf("%+v", result)
			}
			l.requireOwner("b", 2, 3)
		})
	}
}

// TestRollbackBeforeTheFenceGivesTheSourceBackEveryWrite crashes after each pre-fence step and verifies Rollback returns ownership and all writes to the source.
func TestRollbackBeforeTheFenceGivesTheSourceBackEveryWrite(t *testing.T) {
	for _, step := range Steps()[:Activate-1] {
		t.Run(step.String(), func(t *testing.T) {
			l := newLab(t)
			l.prepare()
			c := l.coordinator()
			c.CrashAfter = step
			if _, err := c.Run(context.Background(), "db-1", "a", "b"); !errors.Is(err, ErrCrashed) {
				t.Fatalf("crash after %s: %v", step, err)
			}
			result, err := l.coordinator().Rollback(context.Background(), "db-1")
			if err != nil {
				t.Fatal(err)
			}
			if result.Outcome != RolledBack || result.Epoch != 3 {
				t.Fatalf("%+v", result)
			}
			if _, err := os.Stat(filepath.Join(l.work, "b", "db-1.staged")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("the destination kept its staged file: %v", err)
			}
			if _, err := l.write("b", node.StoreDurable); !errors.Is(err, node.ErrNotOwner) {
				t.Fatalf("the destination wrote after the rollback: %v", err)
			}
			l.requireOwner("a", 3, 3)
		})
	}
}

// TestRollbackAfterTheFenceRollsForward verifies Rollback refuses to undo a landed fence, rolls forward instead, and returns ErrFenceInStore.
func TestRollbackAfterTheFenceRollsForward(t *testing.T) {
	l := newLab(t)
	l.prepare()
	c := l.coordinator()
	c.CrashAfter = Activate
	if _, err := c.Run(context.Background(), "db-1", "a", "b"); !errors.Is(err, ErrCrashed) {
		t.Fatal(err)
	}
	result, err := l.coordinator().Rollback(context.Background(), "db-1")
	if !errors.Is(err, ErrFenceInStore) || result.Outcome != RolledForward {
		t.Fatalf("%+v %v", result, err)
	}
	l.requireOwner("b", 2, 3)
}

// TestStoreFaultsOnTheFinalShipAndTheFence injects FailBeforePut and FailAfterPut on the final segment and fence puts and checks Resume recovers every write.
func TestStoreFaultsOnTheFinalShipAndTheFence(t *testing.T) {
	for _, tc := range []struct {
		fault   faultstore.Fault
		match   string
		failsAt Step
		drained int64
	}{
		{faultstore.FailBeforePut, "log/000000000004", ShipFinal, 4},
		{faultstore.FailAfterPut, "log/000000000004", ShipFinal, 5},
		{faultstore.FailBeforePut, "log/000000000006", Activate, 5},
		{faultstore.FailAfterPut, "log/000000000006", Activate, 5},
	} {
		t.Run(string(tc.fault)+" "+tc.failsAt.String(), func(t *testing.T) {
			owner := "a"
			if tc.failsAt == Activate {
				owner = "b"
			}
			l := newFaultyLab(t, map[string]faultstore.Rule{owner: {Fault: tc.fault, Match: tc.match, Nth: 1}})
			l.prepare()
			if _, err := l.write("a", node.LocalDurable); err != nil {
				t.Fatal(err)
			}
			_, err := l.coordinator().Run(context.Background(), "db-1", "a", "b")
			var stepErr *StepError
			if !errors.As(err, &stepErr) || stepErr.Step != tc.failsAt || !strings.Contains(err.Error(), faultstore.ErrInjected.Error()) {
				t.Fatalf("%v", err)
			}
			result, err := l.coordinator().Resume(context.Background(), "db-1")
			if err != nil {
				t.Fatal(err)
			}
			if result.Outcome != RolledForward || result.Position != tc.drained || result.Fence != tc.drained+1 {
				t.Fatalf("%+v", result)
			}
			l.requireOwner("b", 2, 4)
		})
	}
}

// TestSkippingTheFenceLeavesTwoWriters confirms the SkipFence mutant leaves both source and destination reporting writable.
func TestSkippingTheFenceLeavesTwoWriters(t *testing.T) {
	l := newLab(t)
	l.prepare()
	c := l.coordinator()
	c.SkipFence = true
	if _, err := c.Run(context.Background(), "db-1", "a", "b"); err != nil {
		t.Fatal(err)
	}
	if a, b := l.nodes["a"].Status("db-1"), l.nodes["b"].Status("db-1"); a != "owner at epoch 1, writable" || b != "owner at epoch 2, writable" {
		t.Fatalf("a %q, b %q", a, b)
	}
}

// TestActivateRefusesAStoreThatMovedAfterTheDrain checks Activate rejects a stale drained position with ErrMovedAfterDrain and a mismatched digest with ErrStagedMismatch.
func TestActivateRefusesAStoreThatMovedAfterTheDrain(t *testing.T) {
	l := newLab(t)
	l.prepare()
	c := l.coordinator()
	c.CrashAfter = Rebuild
	if _, err := c.Run(context.Background(), "db-1", "a", "b"); !errors.Is(err, ErrCrashed) {
		t.Fatal(err)
	}
	r, err := l.catalog.GetRelocation(context.Background(), "db-1")
	if err != nil {
		t.Fatal(err)
	}
	_, err = l.nodes["b"].Activate(context.Background(), "db-1", r.ToEpoch, r.Position-1, r.SHA256, true)
	if !errors.Is(err, node.ErrMovedAfterDrain) {
		t.Fatalf("%v", err)
	}
	_, err = l.nodes["b"].Activate(context.Background(), "db-1", r.ToEpoch, r.Position, walship.SHA256Hex([]byte("other")), true)
	if !errors.Is(err, node.ErrStagedMismatch) {
		t.Fatalf("%v", err)
	}
}

type unreachable struct {
	Participant
}

var errUnreachable = errors.New("the node is not running")

func (unreachable) Discard(ctx context.Context, id string) error { return errUnreachable }

// TestRollbackWhileTheDestinationIsDown verifies Rollback marks the relocation rolled-back, returns DiscardErr, and permits a new relocation even when the destination is unreachable.
func TestRollbackWhileTheDestinationIsDown(t *testing.T) {
	ctx := context.Background()
	l := newLab(t)
	l.prepare()
	c := l.coordinator()
	c.CrashAfter = Rebuild
	if _, err := c.Run(ctx, "db-1", "a", "b"); !errors.Is(err, ErrCrashed) {
		t.Fatal(err)
	}
	down := l.coordinator()
	down.Node = func(name string) Participant {
		if name == "b" {
			return unreachable{l.nodes["b"]}
		}
		return l.nodes[name]
	}
	result, err := down.Rollback(ctx, "db-1")
	if err != nil || result.Outcome != RolledBack || result.Epoch != 3 || !errors.Is(result.DiscardErr, errUnreachable) {
		t.Fatalf("%+v %v", result, err)
	}
	relocation, err := l.catalog.GetRelocation(ctx, "db-1")
	if err != nil || relocation.State != catalog.RelocationRolledBack {
		t.Fatalf("%s %v", relocation, err)
	}
	if again, err := l.coordinator().Rollback(ctx, "db-1"); err != nil || again.Outcome != NothingToDo {
		t.Fatalf("%+v %v", again, err)
	}
	if _, err := l.catalog.BeginRelocation(ctx, "db-1", "a", "b"); err != nil {
		t.Fatal(err)
	}
}

// TestRollbackFinishesARollbackThatStoppedAfterTheClaim checks that retrying Rollback after the source already re-claimed finishes as rolled-back rather than superseded.
func TestRollbackFinishesARollbackThatStoppedAfterTheClaim(t *testing.T) {
	ctx := context.Background()
	l := newLab(t)
	l.prepare()
	c := l.coordinator()
	c.CrashAfter = Rebuild
	if _, err := c.Run(ctx, "db-1", "a", "b"); !errors.Is(err, ErrCrashed) {
		t.Fatal(err)
	}
	a := l.nodes["a"]
	if err := a.Seal(ctx, "db-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Drain(ctx, "db-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Claim(ctx, "db-1"); err != nil {
		t.Fatal(err)
	}
	result, err := l.coordinator().Rollback(ctx, "db-1")
	if err != nil || result.Outcome != RolledBack || result.Epoch != 3 || result.DiscardErr != nil {
		t.Fatalf("%+v %v", result, err)
	}
	if _, err := os.Stat(filepath.Join(l.work, "b", "db-1.staged")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the destination kept its staged file: %v", err)
	}
	relocation, err := l.catalog.GetRelocation(ctx, "db-1")
	if err != nil || relocation.State != catalog.RelocationRolledBack {
		t.Fatalf("%s %v", relocation, err)
	}
	l.requireOwner("a", 3, 3)
}

// TestRollbackStillReportsAThirdNodesTakeover ensures a higher epoch claimed by a node other than the source still finishes as Superseded with ErrSupersededBy.
func TestRollbackStillReportsAThirdNodesTakeover(t *testing.T) {
	ctx := context.Background()
	l := newLab(t)
	l.prepare()
	c := l.coordinator()
	c.CrashAfter = Rebuild
	if _, err := c.Run(ctx, "db-1", "a", "b"); !errors.Is(err, ErrCrashed) {
		t.Fatal(err)
	}
	if _, err := l.nodes["b"].Claim(ctx, "db-1"); err != nil {
		t.Fatal(err)
	}
	result, err := l.coordinator().Rollback(ctx, "db-1")
	if !errors.Is(err, ErrSupersededBy) || result.Outcome != Superseded {
		t.Fatalf("%+v %v", result, err)
	}
}
