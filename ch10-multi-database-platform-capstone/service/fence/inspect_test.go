package fence_test

import (
	"context"
	"encoding/binary"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/nickstrad/sqlite-internals/ch10-multi-database-platform-capstone/service/catalog"
	"github.com/nickstrad/sqlite-internals/ch10-multi-database-platform-capstone/service/fence"
	"github.com/nickstrad/sqlite-internals/ch10-multi-database-platform-capstone/service/node"
	"github.com/nickstrad/sqlite-internals/internal/recipe"
	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
	"github.com/nickstrad/sqlite-internals/internal/walship"
)

func shippedStore(t *testing.T, writes int) *walship.DirStore {
	t.Helper()
	ctx := context.Background()
	work := t.TempDir()
	cat, err := catalog.Open(ctx, filepath.Join(work, "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	store, err := walship.NewDirStore(filepath.Join(work, "store"))
	if err != nil {
		t.Fatal(err)
	}
	if err := cat.Register(ctx, "db-1", node.PlatformSettings()); err != nil {
		t.Fatal(err)
	}
	workload := recipe.Default()
	n, err := node.Start(node.Config{
		Name: "a", Dir: filepath.Join(work, "a"), Catalog: cat, Store: store, Capacity: 1,
		File:   workload.SQLiteOptions().File,
		Create: func(ctx context.Context, path string) error { return workload.Build(ctx, path, 0) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := n.Claim(ctx, "db-1"); err != nil {
		t.Fatal(err)
	}
	for range writes {
		if _, err := n.Write(ctx, "db-1", node.StoreDurable, func(ctx context.Context, db *sqlitex.DB) error {
			_, err := workload.ApplyNext(ctx, db)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := n.Close(); err != nil {
		t.Fatal(err)
	}
	return store
}

func TestInspectNamesTheLastGoodRestorePoint(t *testing.T) {
	ctx := context.Background()
	store := shippedStore(t, 3)
	var versions []uint32
	inspection, err := fence.Inspect(ctx, store, "db-1", func(position int64, image []byte) error {
		versions = append(versions, binary.BigEndian.Uint32(image[60:64]))
		return nil
	})
	if err != nil || inspection.Problem != "" || inspection.Slots != 4 || inspection.RestorePoints != 4 || inspection.LastGood != 4 || inspection.BytesRead == 0 {
		t.Fatalf("%+v %v", inspection, err)
	}
	if !slices.Equal(versions, []uint32{0, 1, 2, 3}) {
		t.Fatalf("user_versions %v", versions)
	}
	key := fence.SlotKey("db-1", 3)
	data, err := store.Get(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	inspection, err = fence.Inspect(ctx, store, "db-1", nil)
	if err != nil || inspection.LastGood != 2 || inspection.RestorePoints != 2 || !strings.Contains(inspection.Problem, "position 4 follows 2") {
		t.Fatalf("gap: %+v %v", inspection, err)
	}
	corrupted := append([]byte(nil), data...)
	corrupted[len(corrupted)-1] ^= 0xff
	if err := store.PutIfAbsent(ctx, key, corrupted); err != nil {
		t.Fatal(err)
	}
	inspection, err = fence.Inspect(ctx, store, "db-1", nil)
	if err != nil || inspection.LastGood != 2 || inspection.Problem == "" {
		t.Fatalf("corrupt: %+v %v", inspection, err)
	}
	if !strings.Contains(inspection.String(), "unrecoverable past position 2 (last good restore point 2)") {
		t.Fatal(inspection.String())
	}
	empty, err := fence.Inspect(ctx, store, "db-2", nil)
	if err != nil || empty.Problem == "" || empty.RestorePoints != 0 {
		t.Fatalf("empty: %+v %v", empty, err)
	}
}
