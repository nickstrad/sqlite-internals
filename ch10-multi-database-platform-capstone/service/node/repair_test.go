package node

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickstrad/sqlite-internals/ch10-multi-database-platform-capstone/service/fence"
	"github.com/nickstrad/sqlite-internals/internal/recipe"
	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
	"github.com/nickstrad/sqlite-internals/internal/walship"
)

type hookStore struct {
	walship.Store
	beforePut func() error
	afterPut  func()
}

func (s *hookStore) PutIfAbsent(ctx context.Context, key string, data []byte) error {
	if s.beforePut != nil {
		if err := s.beforePut(); err != nil {
			return err
		}
	}
	err := s.Store.PutIfAbsent(ctx, key, data)
	if err == nil && s.afterPut != nil {
		s.afterPut()
	}
	return err
}

func catalogRow(t *testing.T, c *cluster, id string) (int64, int) {
	t.Helper()
	record, err := c.catalog.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return record.LastShipped, record.SchemaVersion
}

func storeEnd(t *testing.T, c *cluster, id string) int64 {
	t.Helper()
	chain, _, err := fence.Audit(context.Background(), c.store, id)
	if err != nil {
		t.Fatal(err)
	}
	return chain[len(chain)-1].Last
}

func outsideWrite(t *testing.T, path string) {
	t.Helper()
	ctx := context.Background()
	outside, err := sqlitex.Open(ctx, path, recipe.Default().SQLiteOptions())
	if err != nil {
		t.Fatal(err)
	}
	if err := nextStep(ctx, outside); err != nil {
		t.Fatal(err)
	}
	if err := outside.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestReclaimAfterAnotherOwnerAdvancedTheDatabase(t *testing.T) {
	ctx := context.Background()
	c := newCluster(t, "db-1", "db-2")
	a, b := c.start(t, "a", 1), c.start(t, "b", 1)
	for _, id := range []string{"db-1", "db-2"} {
		if _, err := a.Claim(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite(t, a, "db-1", StoreDurable)
	mustWrite(t, a, "db-2", StoreDurable)
	if _, err := b.Claim(ctx, "db-1"); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, b, "db-1", StoreDurable)
	if result, err := a.Claim(ctx, "db-1"); err != nil || result.RestoredFrom != 4 || result.Fence != 5 {
		t.Fatalf("reclaim %+v %v", result, err)
	}
	if ack := mustWrite(t, a, "db-1", StoreDurable); ack.Position != 6 || ack.UserVersion != 3 {
		t.Fatalf("%+v", ack)
	}
	if err := errors.Join(a.Close(), b.Close()); err != nil {
		t.Fatal(err)
	}
}

func TestShipsOnEvictionCloseAndReopenUpdateTheCatalog(t *testing.T) {
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
	if shipped, version := catalogRow(t, c, "db-1"); shipped != 2 || version != 1 || storeEnd(t, c, "db-1") != 2 {
		t.Fatalf("after eviction: catalog %d %d", shipped, version)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if shipped, version := catalogRow(t, c, "db-2"); shipped != 2 || version != 1 || storeEnd(t, c, "db-2") != 2 {
		t.Fatalf("after close: catalog %d %d", shipped, version)
	}
	if err := os.Remove(filepath.Join(c.work, "a", "db-2.atrest")); err != nil {
		t.Fatal(err)
	}
	outsideWrite(t, filepath.Join(c.work, "a", "db-2.db"))
	restarted := c.start(t, "a", 1)
	if err := restarted.Read(ctx, "db-2", func(context.Context, *sqlitex.DB) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if shipped, version := catalogRow(t, c, "db-2"); shipped != 3 || version != 2 || storeEnd(t, c, "db-2") != 3 {
		t.Fatalf("after reopen: catalog %d %d store %d", shipped, version, storeEnd(t, c, "db-2"))
	}
	if err := restarted.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestClaimSucceedsOnceTheStoreAcceptedItsFence(t *testing.T) {
	ctx := context.Background()
	c := newCluster(t, "db-1")
	hooked := &hookStore{Store: c.store}
	c.store = hooked
	a := c.start(t, "a", 1)
	hooked.afterPut = func() {
		hooked.afterPut = nil
		if _, err := c.catalog.Claim(ctx, "db-1", "b"); err != nil {
			t.Error(err)
		}
	}
	result, err := a.Claim(ctx, "db-1")
	if err != nil || result.Epoch != 1 || result.Fence != 1 {
		t.Fatalf("%+v %v", result, err)
	}
	if epoch, ok := a.Owned("db-1"); !ok || epoch != 1 {
		t.Fatalf("owned %d %t", epoch, ok)
	}
	if ack := mustWrite(t, a, "db-1", StoreDurable); ack.Position != 2 {
		t.Fatalf("%+v", ack)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestFailedReclaimForgetsTheBeliefOnDisk(t *testing.T) {
	ctx := context.Background()
	c := newCluster(t, "db-1")
	hooked := &hookStore{Store: c.store}
	c.store = hooked
	a := c.start(t, "a", 1)
	if _, err := a.Claim(ctx, "db-1"); err != nil {
		t.Fatal(err)
	}
	refused := errors.New("store unreachable")
	hooked.beforePut = func() error { return refused }
	if _, err := a.Claim(ctx, "db-1"); !errors.Is(err, refused) {
		t.Fatalf("reclaim: %v", err)
	}
	if _, ok := a.Owned("db-1"); ok {
		t.Fatal("owned after a failed claim")
	}
	if _, err := os.Stat(filepath.Join(c.work, "a", "db-1.owner")); !os.IsNotExist(err) {
		t.Fatalf("belief file kept: %v", err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.start(t, "a", 1).Owned("db-1"); ok {
		t.Fatal("restart resurrected the old belief")
	}
}

func TestStartRejectsBadRecords(t *testing.T) {
	for name, file := range map[string][2]string{
		"good":             {"db-1.owner", "epoch 1\n"},
		"epoch zero":       {"db-1.owner", "epoch 0\n"},
		"short digest":     {"db-1.atrest", "abc\n"},
		"uppercase digest": {"db-1.atrest", "AB" + strings.Repeat("0", 62) + "\n"},
	} {
		c := newCluster(t)
		dir := filepath.Join(c.work, "a")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, file[0]), []byte(file[1]), 0o644); err != nil {
			t.Fatal(err)
		}
		n, err := Start(Config{Name: "a", Dir: dir, Catalog: c.catalog, Store: c.store, Capacity: 1, Create: func(context.Context, string) error { return nil }})
		if name == "good" {
			if err != nil {
				t.Fatal(err)
			}
			if err := n.Close(); err != nil {
				t.Fatal(err)
			}
		} else if err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestRestartRefusesAFileChangedWhileShut(t *testing.T) {
	ctx := context.Background()
	c := newCluster(t, "db-1")
	a := c.start(t, "a", 1)
	if _, err := a.Claim(ctx, "db-1"); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, a, "db-1", StoreDurable)
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	outsideWrite(t, filepath.Join(c.work, "a", "db-1.db"))
	restarted := c.start(t, "a", 1)
	if _, err := restarted.Write(ctx, "db-1", StoreDurable, nextStep); !errors.Is(err, ErrChangedWhileShut) {
		t.Fatalf("offline change accepted: %v", err)
	}
	if storeEnd(t, c, "db-1") != 2 {
		t.Fatal("the changed file reached the store")
	}
	if err := restarted.Close(); err != nil {
		t.Fatal(err)
	}
	untouched := c.start(t, "a", 1)
	if result, err := untouched.Claim(ctx, "db-1"); err != nil || result.RestoredFrom != 2 {
		t.Fatalf("%+v %v", result, err)
	}
	if ack := mustWrite(t, untouched, "db-1", StoreDurable); ack.Position != 4 || ack.UserVersion != 2 {
		t.Fatalf("%+v", ack)
	}
	if err := untouched.Close(); err != nil {
		t.Fatal(err)
	}
	clean := c.start(t, "a", 1)
	if ack := mustWrite(t, clean, "db-1", StoreDurable); ack.Position != 5 || ack.UserVersion != 3 {
		t.Fatalf("clean restart: %+v", ack)
	}
	if err := clean.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPageSizeOfRejectsBadImages(t *testing.T) {
	image := func(size int, field uint16) []byte {
		b := make([]byte, size)
		if size >= 18 {
			b[16], b[17] = byte(field>>8), byte(field)
		}
		return b
	}
	if size, err := pageSizeOf(image(8192, 4096)); size != 4096 || err != nil {
		t.Fatalf("%d %v", size, err)
	}
	if size, err := pageSizeOf(image(65536, 1)); size != 65536 || err != nil {
		t.Fatalf("%d %v", size, err)
	}
	for name, b := range map[string][]byte{
		"short":          image(50, 4096),
		"zeroed":         image(4096, 0),
		"not power of 2": image(3000, 1000),
		"too small":      image(4096, 256),
		"not a multiple": image(6000, 4096),
	} {
		if _, err := pageSizeOf(b); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}
