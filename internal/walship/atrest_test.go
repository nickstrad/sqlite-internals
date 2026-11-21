package walship

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickstrad/sqlite-internals/internal/recipe"
)

func openShipper(t *testing.T, path string, store Store) *Shipper {
	t.Helper()
	shipper, err := Open(context.Background(), Config{Database: path, Store: store, Options: recipe.Default().SQLiteOptions(), Origin: 1})
	if err != nil {
		t.Fatal(err)
	}
	return shipper
}

func commitStep(t *testing.T, path string, step int) {
	t.Helper()
	ctx := context.Background()
	r := recipe.Default()
	db, err := r.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := r.ApplyStep(ctx, db, step); err != nil {
		t.Fatal(err)
	}
}

// TestAdoptAtRestSkipsTheReopenSnapshot verifies that AdoptAtRest avoids a discontinuity snapshot after a clean close while keeping every position restorable byte for byte.
func TestAdoptAtRestSkipsTheReopenSnapshot(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "a.db")
	if err := recipe.Default().Build(ctx, path, 0); err != nil {
		t.Fatal(err)
	}
	store, err := NewDirStore(filepath.Join(dir, "store"))
	if err != nil {
		t.Fatal(err)
	}
	shipper := openShipper(t, path, store)
	commitStep(t, path, 1)
	if report, err := shipper.Cycle(ctx); err != nil || report.Segment == nil {
		t.Fatalf("%+v %v", report, err)
	}
	if err := shipper.Close(); err != nil {
		t.Fatal(err)
	}
	if err := recipe.CheckAtRest(path); err != nil {
		t.Fatal(err)
	}

	without := openShipper(t, path, store)
	report, err := without.Cycle(ctx)
	if err != nil || report.Snapshot == nil || report.Snapshot.Reason != ReasonDiscontinuity {
		t.Fatalf("plain resume after a clean close: %+v %v", report, err)
	}
	if err := without.Close(); err != nil {
		t.Fatal(err)
	}

	again := openShipper(t, path, store)
	adopted, err := again.AdoptAtRest(ctx)
	if err != nil || !adopted {
		t.Fatalf("adopted %t %v", adopted, err)
	}
	commitStep(t, path, 2)
	report, err = again.Cycle(ctx)
	if err != nil || report.Snapshot != nil || report.Segment == nil || report.Change != WALNewGeneration || report.Segment.FirstFrame != 1 {
		t.Fatalf("after adopting: %+v %v", report, err)
	}
	position := again.Position()
	if err := again.Close(); err != nil {
		t.Fatal(err)
	}
	catalog, err := LoadCatalog(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	for target := int64(1); target <= position; target++ {
		plan, err := catalog.Plan(target)
		if err != nil {
			t.Fatal(target, err)
		}
		image, _, err := plan.Execute()
		if err != nil {
			t.Fatal(target, err)
		}
		if got, want := userVersion(image), map[int64]uint32{1: 0, 2: 1, 3: 1, 4: 2}[target]; got != want {
			t.Fatalf("position %d holds user_version %d, want %d", target, got, want)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if SHA256Hex(data) != SHA256Hex(mustRestore(t, catalog, position)) {
		t.Fatal("closed file differs from the restored last position")
	}
}

func mustRestore(t *testing.T, catalog *Catalog, position int64) []byte {
	t.Helper()
	plan, err := catalog.Plan(position)
	if err != nil {
		t.Fatal(err)
	}
	image, _, err := plan.Execute()
	if err != nil {
		t.Fatal(err)
	}
	return image
}

// TestAdoptAtRestRefusesAChangedFileOrUnshippedFrames checks that AdoptAtRest declines an offline-modified file and a WAL holding committed frames, leaving Cycle to snapshot.
func TestAdoptAtRestRefusesAChangedFileOrUnshippedFrames(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "a.db")
	if err := recipe.Default().Build(ctx, path, 0); err != nil {
		t.Fatal(err)
	}
	store, err := NewDirStore(filepath.Join(dir, "store"))
	if err != nil {
		t.Fatal(err)
	}
	if err := openShipper(t, path, store).Close(); err != nil {
		t.Fatal(err)
	}
	if err := recipe.Default().ApplyInSession(ctx, path, mustStep(t, 1)); err != nil {
		t.Fatal(err)
	}
	changed := openShipper(t, path, store)
	if adopted, err := changed.AdoptAtRest(ctx); err != nil || adopted {
		t.Fatalf("changed file adopted: %t %v", adopted, err)
	}
	commitStep(t, path, 2)
	if adopted, err := changed.AdoptAtRest(ctx); err != nil || adopted {
		t.Fatalf("WAL with frames adopted: %t %v", adopted, err)
	}
	report, err := changed.Cycle(ctx)
	if err != nil || report.Snapshot == nil || report.Snapshot.Reason != ReasonDiscontinuity {
		t.Fatalf("%+v %v", report, err)
	}
	if err := changed.Close(); err != nil {
		t.Fatal(err)
	}
}

func mustStep(t *testing.T, k int) recipe.Step {
	t.Helper()
	step, err := recipe.Default().Step(k)
	if err != nil {
		t.Fatal(err)
	}
	return step
}
