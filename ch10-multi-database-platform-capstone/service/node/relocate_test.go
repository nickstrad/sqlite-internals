package node

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickstrad/sqlite-internals/ch10-multi-database-platform-capstone/service/bounds"
	"github.com/nickstrad/sqlite-internals/ch10-multi-database-platform-capstone/service/fence"
	"github.com/nickstrad/sqlite-internals/internal/walship"
)

// TestSealDrainStageActivateRetire walks all participant steps of a clean relocation and checks guards, idempotency, sidecar cleanup, and catalog state.
func TestSealDrainStageActivateRetire(t *testing.T) {
	ctx := context.Background()
	c := newCluster(t, "db-1")
	a, b := c.start(t, "a", 2), c.start(t, "b", 2)
	if err := a.Seal(ctx, "db-1"); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("seal before the claim: %v", err)
	}
	if _, err := a.Claim(ctx, "db-1"); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, a, "db-1", StoreDurable)
	mustWrite(t, a, "db-1", LocalDurable)
	if _, err := a.Drain(ctx, "db-1"); !errors.Is(err, ErrNotSealed) {
		t.Fatalf("drain before the seal: %v", err)
	}
	if err := a.Retire(ctx, "db-1"); !errors.Is(err, ErrNotSealed) {
		t.Fatalf("retire of a writable owner: %v", err)
	}
	if got := a.Status("db-1"); got != "owner at epoch 1, writable" {
		t.Fatal(got)
	}
	if err := a.Seal(ctx, "db-1"); err != nil {
		t.Fatal(err)
	}
	if got := a.Status("db-1"); got != "owner at epoch 1, sealed" {
		t.Fatal(got)
	}
	if _, err := a.Write(ctx, "db-1", StoreDurable, nextStep); !errors.Is(err, ErrSealed) {
		t.Fatalf("write on a sealed database: %v", err)
	}
	drained, err := a.Drain(ctx, "db-1")
	if err != nil || drained.Position != 3 {
		t.Fatalf("%+v %v", drained, err)
	}
	again, err := a.Drain(ctx, "db-1")
	if err != nil || again != drained {
		t.Fatalf("a second drain %+v %v, the first %+v", again, err, drained)
	}
	epoch, err := c.catalog.Claim(ctx, "db-1", "b")
	if err != nil {
		t.Fatal(err)
	}
	staged, err := b.Stage(ctx, "db-1", epoch)
	if err != nil || staged.Position != drained.Position || staged.SHA256 != drained.SHA256 {
		t.Fatalf("%+v %v, drained %+v", staged, err, drained)
	}
	if _, err := b.Activate(ctx, "db-1", epoch, drained.Position, "not-a-sha256", true); err == nil {
		t.Fatal("a malformed sha256 was accepted")
	}
	activated, err := b.Activate(ctx, "db-1", epoch, drained.Position, drained.SHA256, true)
	if err != nil || activated != (Activated{Fence: 4}) {
		t.Fatalf("%+v %v", activated, err)
	}
	if again, err := b.Activate(ctx, "db-1", epoch, drained.Position, drained.SHA256, true); err != nil || again != (Activated{Fence: 4, Adopted: true}) {
		t.Fatalf("a repeated activation %+v %v", again, err)
	}
	if _, err := os.Stat(filepath.Join(c.work, "b", "db-1.staged")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the staged file is still there: %v", err)
	}
	if ack := mustWrite(t, b, "db-1", StoreDurable); ack.Position != 5 || ack.UserVersion != 3 {
		t.Fatalf("%+v", ack)
	}
	if _, err := a.Write(ctx, "db-1", StoreDurable, nextStep); !errors.Is(err, ErrSealed) {
		t.Fatalf("the sealed source wrote: %v", err)
	}
	if err := a.Retire(ctx, "db-1"); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{".db", ".owner", ".sealed", ".atrest"} {
		if _, err := os.Stat(filepath.Join(c.work, "a", "db-1"+suffix)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("retired source kept %s: %v", suffix, err)
		}
	}
	if got := a.Status("db-1"); got != "not owner" {
		t.Fatal(got)
	}
	record, err := c.catalog.Get(ctx, "db-1")
	if err != nil || record.Owner != "b" || record.Epoch != 2 || record.LastShipped != 5 || record.SchemaVersion != 3 {
		t.Fatalf("%+v %v", record, err)
	}
}

// TestActivateAdoptsAFenceItDidNotRecord verifies Activate adopts an already-landed fence snapshot from the store and rejects an older epoch with ErrFenced.
func TestActivateAdoptsAFenceItDidNotRecord(t *testing.T) {
	ctx := context.Background()
	c := newCluster(t, "db-1")
	a, b := c.start(t, "a", 2), c.start(t, "b", 2)
	if _, err := a.Claim(ctx, "db-1"); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, a, "db-1", StoreDurable)
	if err := a.Seal(ctx, "db-1"); err != nil {
		t.Fatal(err)
	}
	drained, err := a.Drain(ctx, "db-1")
	if err != nil {
		t.Fatal(err)
	}
	epoch, err := c.catalog.Claim(ctx, "db-1", "b")
	if err != nil {
		t.Fatal(err)
	}
	image, err := os.ReadFile(filepath.Join(c.work, "a", "db-1.db"))
	if err != nil {
		t.Fatal(err)
	}
	object, _, err := fenceSnapshot(drained.Position+1, image)
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := fence.New(c.store, "db-1", epoch)
	if err != nil {
		t.Fatal(err)
	}
	if err := adapter.PutIfAbsent(ctx, walship.SnapshotKey(drained.Position+1), object); err != nil {
		t.Fatal(err)
	}
	activated, err := b.Activate(ctx, "db-1", epoch, drained.Position, drained.SHA256, true)
	if err != nil || activated != (Activated{Fence: drained.Position + 1, Adopted: true}) {
		t.Fatalf("%+v %v", activated, err)
	}
	if epochOwned, ok := b.Owned("db-1"); !ok || epochOwned != epoch {
		t.Fatalf("b owns %d %t", epochOwned, ok)
	}
	if _, err := a.Activate(ctx, "db-1", epoch-1, drained.Position, drained.SHA256, true); !errors.Is(err, fence.ErrFenced) {
		t.Fatalf("an older epoch activated: %v", err)
	}
}

// TestRelocationCommandsAndReplies exercises the line-protocol relocation, status, and check commands through Serve.
func TestRelocationCommandsAndReplies(t *testing.T) {
	ctx := context.Background()
	c := newCluster(t, "db-1")
	a := c.start(t, "a", 2)
	assertion, err := bounds.Start(bounds.Bounds{OpenDatabases: 2, ConnectionsPerDatabase: ConnectionsPerDatabase, CacheBytes: 1 << 20, ReserveDescriptors: ReserveDescriptors})
	if err != nil {
		t.Fatal(err)
	}
	in := strings.NewReader("claim db-1\nstatus db-1\nseal db-1\nwrite db-1 store-durable\ndrain db-1\ncheck db-1\nstage db-1 x\nactivate db-1 1 2\ndiscard db-1\nstatus db-9\nretire db-1\nstatus db-1\n")
	var out bytes.Buffer
	if err := Serve(ctx, a, assertion, nextStep, in, &out); err != nil {
		t.Fatal(err)
	}
	var replies []string
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(line, "ok ") || strings.HasPrefix(line, "refused ") {
			replies = append(replies, line)
		}
	}
	want := []string{
		"ok claim db-1: epoch 1, restored position 0, fence at position 1, attempts 1",
		"ok status db-1: owner at epoch 1, writable",
		"ok seal db-1",
		"refused write db-1 store-durable: error: the database is sealed for relocation: db-1",
		"ok drain db-1: position 1, sha256 ",
		"ok check db-1: user_version 0, integrity ok",
		"refused stage db-1 x: error: strconv.ParseUint: parsing \"x\": invalid syntax",
		"refused activate db-1 1 2: unknown command",
		"ok discard db-1",
		"ok status db-9: not owner",
		"ok retire db-1",
		"ok status db-1: not owner",
	}
	if len(replies) != len(want) {
		t.Fatalf("%d replies: %q", len(replies), replies)
	}
	for i := range want {
		if !strings.HasPrefix(replies[i], want[i]) {
			t.Fatalf("reply %d: %q, want %q", i, replies[i], want[i])
		}
	}
}

// TestParseAckIsStrict verifies ParseAck accepts canonical LocalDurable and StoreDurable reply lines and rejects padding, extra tokens, or wrong formats.
func TestParseAckIsStrict(t *testing.T) {
	id, ack, err := ParseAck("ok write db-1 store-durable: acknowledged at epoch 2, user_version 3, stored through position 7")
	if err != nil || id != "db-1" || ack != (Ack{Mode: StoreDurable, Epoch: 2, UserVersion: 3, Position: 7}) {
		t.Fatalf("%s %+v %v", id, ack, err)
	}
	id, ack, err = ParseAck("ok write db-2 local-durable: acknowledged at epoch 1, user_version 4, committed locally (not yet in the store)")
	if err != nil || id != "db-2" || ack != (Ack{Mode: LocalDurable, Epoch: 1, UserVersion: 4}) {
		t.Fatalf("%s %+v %v", id, ack, err)
	}
	for _, bad := range []string{
		"",
		"refused write db-1 store-durable: not the owner",
		"ok write db-1 store-durable: acknowledged at epoch 2, user_version 3, stored through position 7 extra",
		"ok write db-1 store-durable: acknowledged at epoch 02, user_version 3, stored through position 7",
		"ok write db-1 fast: acknowledged at epoch 2, user_version 3, stored through position 7",
		"ok write db-1  store-durable: acknowledged at epoch 2, user_version 3, stored through position 7",
		"ok write db-1 local-durable: acknowledged at epoch 1, user_version 4",
	} {
		if _, _, err := ParseAck(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}

func relocateToB(t *testing.T, c *cluster, a, b *Node) (Drained, uint64) {
	t.Helper()
	ctx := context.Background()
	if _, err := a.Claim(ctx, "db-1"); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, a, "db-1", StoreDurable)
	mustWrite(t, a, "db-1", LocalDurable)
	if err := a.Seal(ctx, "db-1"); err != nil {
		t.Fatal(err)
	}
	drained, err := a.Drain(ctx, "db-1")
	if err != nil {
		t.Fatal(err)
	}
	epoch, err := c.catalog.Claim(ctx, "db-1", "b")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Stage(ctx, "db-1", epoch); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Activate(ctx, "db-1", epoch, drained.Position, drained.SHA256, true); err != nil {
		t.Fatal(err)
	}
	return drained, epoch
}

// TestRepeatedActivateRepairsAnUnrecordedFence verifies a repeated Activate re-records location, last_shipped, and schema_version when the store still ends at the fence.
func TestRepeatedActivateRepairsAnUnrecordedFence(t *testing.T) {
	ctx := context.Background()
	c := newCluster(t, "db-1")
	a, b := c.start(t, "a", 2), c.start(t, "b", 2)
	drained, epoch := relocateToB(t, c, a, b)
	if _, err := c.catalog.DB.ExecContext(ctx, "UPDATE databases SET location = '', last_shipped = ?, schema_version = 0 WHERE id = 'db-1'", drained.Position); err != nil {
		t.Fatal(err)
	}
	again, err := b.Activate(ctx, "db-1", epoch, drained.Position, drained.SHA256, true)
	if err != nil || again != (Activated{Fence: drained.Position + 1, Adopted: true}) {
		t.Fatalf("%+v %v", again, err)
	}
	record, err := c.catalog.Get(ctx, "db-1")
	if err != nil || record.Owner != "b" || record.Epoch != epoch || record.Location != "b:db-1.db" || record.LastShipped != drained.Position+1 || record.SchemaVersion != 2 {
		t.Fatalf("%+v %v", record, err)
	}
}

// TestRepeatedActivateAfterLaterWritesKeepsTheCatalog ensures a repeated Activate does not rewind catalog metadata once the new owner has shipped past the fence.
func TestRepeatedActivateAfterLaterWritesKeepsTheCatalog(t *testing.T) {
	ctx := context.Background()
	c := newCluster(t, "db-1")
	a, b := c.start(t, "a", 2), c.start(t, "b", 2)
	drained, epoch := relocateToB(t, c, a, b)
	ack := mustWrite(t, b, "db-1", StoreDurable)
	before, err := c.catalog.Get(ctx, "db-1")
	if err != nil || before.LastShipped != ack.Position {
		t.Fatalf("%+v %v", before, err)
	}
	if _, err := b.Activate(ctx, "db-1", epoch, drained.Position, drained.SHA256, true); err != nil {
		t.Fatal(err)
	}
	after, err := c.catalog.Get(ctx, "db-1")
	if err != nil || after != before {
		t.Fatalf("%+v %v, before %+v", after, err, before)
	}
}

// TestDrainReportsAFailedAtRestRecord confirms Drain returns an error when the final eviction reaches at-rest state but fails to write .atrest.
func TestDrainReportsAFailedAtRestRecord(t *testing.T) {
	ctx := context.Background()
	c := newCluster(t, "db-1")
	a := c.start(t, "a", 2)
	if _, err := a.Claim(ctx, "db-1"); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, a, "db-1", StoreDurable)
	if err := a.Seal(ctx, "db-1"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(c.work, "a", "db-1.atrest.tmp"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Drain(ctx, "db-1"); err == nil || !strings.Contains(err.Error(), "reported") {
		t.Fatalf("drain: %v", err)
	}
	if problems := a.TakeProblems(); len(problems) != 0 {
		t.Fatalf("%q", problems)
	}
}
