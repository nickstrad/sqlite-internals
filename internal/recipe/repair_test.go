package recipe

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickstrad/walfleet/internal/sqlitex"
)

// TestInvalidRecipeIsRejectedByStepAndBuild checks that Step, Build, and
// BuildEach reject recipes failing Validate without creating a database file.
func TestInvalidRecipeIsRejectedByStepAndBuild(t *testing.T) {
	invalid := Recipe{Seed: 1, Steps: MaxSteps + 1, JournalMode: sqlitex.JournalWAL}
	if _, err := invalid.Step(0); err == nil {
		t.Fatal("Step(0) on an invalid recipe: want an error")
	}
	path := filepath.Join(t.TempDir(), "invalid.db")
	if err := invalid.Build(context.Background(), path, 0); err == nil {
		t.Fatal("Build(0) on an invalid recipe: want an error")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("Build on an invalid recipe created %s", path)
	}
	badMode := Recipe{Seed: 1, Steps: 3, JournalMode: "truncate"}
	if err := badMode.BuildEach(context.Background(), path, 0, nil); err == nil {
		t.Fatal("BuildEach with an unsupported journal mode: want an error")
	}
}

// TestMatchStepRejectsDisagreeingCounters checks that MatchStep rejects files
// whose bytes 24..27 and 92..95 disagree while still accepting a masked match
// when both counters moved together.
func TestMatchStepRejectsDisagreeingCounters(t *testing.T) {
	ctx := context.Background()
	t.Setenv(sqlitex.WorkDirEnv, t.TempDir())
	r := small(sqlitex.JournalWAL, 3)
	path := filepath.Join(t.TempDir(), "desync.db")
	if err := r.Build(ctx, path, 3); err != nil {
		t.Fatal(err)
	}
	image, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !ChangeCountersAgree(image) {
		t.Fatal("a freshly built database has disagreeing counters")
	}
	binary.BigEndian.PutUint32(image[24:28], 0xDEADBEEF)
	binary.BigEndian.PutUint32(image[92:96], 1)
	if err := os.WriteFile(path, image, 0o644); err != nil {
		t.Fatal(err)
	}
	match, err := r.MatchStep(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if match.Found() {
		t.Fatalf("disagreeing counters matched %v", match)
	}
	binary.BigEndian.PutUint32(image[92:96], 0xDEADBEEF)
	if err := os.WriteFile(path, image, 0o644); err != nil {
		t.Fatal(err)
	}
	match, err = r.MatchStep(ctx, path)
	if err != nil || match != (Match{Step: 3}) {
		t.Fatalf("agreeing but moved counters: %v, %v", match, err)
	}
	if ChangeCountersAgree(image[:50]) {
		t.Fatal("a short image cannot have agreeing counters")
	}
}

func writeRawCache(t *testing.T, path string, hashes []FileHashes, checksum string) {
	t.Helper()
	var b strings.Builder
	for k, h := range hashes {
		fmt.Fprintf(&b, "%d %s %s\n", k, h.Exact, h.Masked)
	}
	if checksum != "" {
		b.WriteString(cacheChecksumPrefix + checksum + "\n")
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestStepHashesRejectsTamperedAndStaleCaches checks that StepHashes discards
// and rewrites caches with missing, mismatched, or step-0-stale checksums, and
// that cacheChecksum depends on the recipe.
func TestStepHashesRejectsTamperedAndStaleCaches(t *testing.T) {
	ctx := context.Background()
	t.Setenv(sqlitex.WorkDirEnv, t.TempDir())
	r := small(sqlitex.JournalWAL, 3)
	good, err := r.StepHashes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cachePath, err := r.CachePath()
	if err != nil {
		t.Fatal(err)
	}
	zero := strings.Repeat("0", 64)
	zeros := make([]FileHashes, len(good))
	for i := range zeros {
		zeros[i] = FileHashes{Exact: zero, Masked: zero}
	}
	validChecksumForZeros, err := r.cacheChecksum(zeros)
	if err != nil {
		t.Fatal(err)
	}
	laterTampered := append([]FileHashes(nil), good...)
	laterTampered[2] = FileHashes{Exact: zero, Masked: zero}
	goodChecksum, err := r.cacheChecksum(good)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name     string
		hashes   []FileHashes
		checksum string
	}{
		{"all-zero hashes, no checksum", zeros, ""},
		{"all-zero hashes, old checksum", zeros, goodChecksum},
		{"all-zero hashes, matching checksum", zeros, validChecksumForZeros},
		{"one later step tampered, old checksum", laterTampered, goodChecksum},
		{"good hashes, no checksum", good, ""},
	}
	for _, c := range cases {
		writeRawCache(t, cachePath, c.hashes, c.checksum)
		got, err := r.StepHashes(ctx)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if !reflect.DeepEqual(got, good) {
			t.Fatalf("%s: StepHashes trusted the cache", c.name)
		}
		rewritten, checksum, err := readHashCache(cachePath)
		if err != nil || !reflect.DeepEqual(rewritten, good) || checksum != goodChecksum {
			t.Fatalf("%s: the cache was not rewritten: %v", c.name, err)
		}
	}
	other := Recipe{Seed: 2, Steps: 3, JournalMode: sqlitex.JournalWAL}
	otherChecksum, err := other.cacheChecksum(good)
	if err != nil {
		t.Fatal(err)
	}
	if otherChecksum == goodChecksum {
		t.Fatal("the checksum does not depend on the recipe")
	}
}

// TestWriteHashCacheRemovesTempFileWhenRenameFails checks that writeHashCache
// cleans up its sibling temporary file if os.Rename fails.
func TestWriteHashCacheRemovesTempFileWhenRenameFails(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "cache.txt")
	if err := os.MkdirAll(filepath.Join(target, "occupied"), 0o755); err != nil {
		t.Fatal(err)
	}
	r := small(sqlitex.JournalWAL, 0)
	hashes := []FileHashes{{Exact: strings.Repeat("a", 64), Masked: strings.Repeat("b", 64)}}
	if err := r.writeHashCache(target, hashes); err == nil {
		t.Fatal("renaming onto a non-empty directory: want an error")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("temp file left behind: %v", names)
	}
}
