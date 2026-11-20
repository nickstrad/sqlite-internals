package recipe

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
)

func small(mode string, steps int) Recipe {
	return Default().WithJournalMode(mode).WithSteps(steps)
}

// TestValidate checks default recipe settings and rejection of invalid step counts or journal modes.
func TestValidate(t *testing.T) {
	if err := Default().Validate(); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []Recipe{
		{Seed: 1, Steps: -1, JournalMode: sqlitex.JournalWAL},
		{Seed: 1, Steps: MaxSteps + 1, JournalMode: sqlitex.JournalWAL},
		{Seed: 1, Steps: 3, JournalMode: "truncate"},
		{Seed: 1, Steps: 3, JournalMode: ""},
	} {
		if err := bad.Validate(); err == nil {
			t.Errorf("%v: want a validation error", bad)
		}
	}
	if Default().JournalMode != sqlitex.JournalWAL {
		t.Errorf("default journal mode %q, want wal", Default().JournalMode)
	}
}

// TestPlanIsDeterministic checks that generating the default plan twice returns identical steps.
func TestPlanIsDeterministic(t *testing.T) {
	a, err := Default().Plan()
	if err != nil {
		t.Fatal(err)
	}
	b, err := Default().Plan()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatal("two plans of the same recipe differ")
	}
}

// TestStepDoesNotDependOnStepCount checks that step k is identical regardless of total recipe length.
func TestStepDoesNotDependOnStepCount(t *testing.T) {
	short, err := Default().WithSteps(7).Plan()
	if err != nil {
		t.Fatal(err)
	}
	long, err := Default().WithSteps(25).Plan()
	if err != nil {
		t.Fatal(err)
	}
	for k := range short {
		if !reflect.DeepEqual(short[k], long[k]) {
			t.Fatalf("step %d differs between a 7-step and a 25-step recipe", k)
		}
	}
	single, err := Default().Step(7)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(single, short[7]) {
		t.Fatal("Step(7) differs from Plan()[7]")
	}
	if _, err := Default().WithSteps(3).Step(4); err == nil {
		t.Fatal("Step past the last step: want an error")
	}
}

// TestSeedsDiffer checks that different seeds change workload steps while keeping schema step 0 identical.
func TestSeedsDiffer(t *testing.T) {
	a, err := Recipe{Seed: 1, Steps: 2, JournalMode: sqlitex.JournalWAL}.Plan()
	if err != nil {
		t.Fatal(err)
	}
	b, err := Recipe{Seed: 2, Steps: 2, JournalMode: sqlitex.JournalWAL}.Plan()
	if err != nil {
		t.Fatal(err)
	}
	if reflect.DeepEqual(a[1], b[1]) {
		t.Fatal("seeds 1 and 2 gave the same step 1")
	}
	if !reflect.DeepEqual(a[0], b[0]) {
		t.Fatal("step 0 (schema) should not depend on the seed")
	}
}

// TestPlanShape checks schema, user_version markers, explicit ids, banned SQL functions, index timing, and overflow body sizes.
func TestPlanShape(t *testing.T) {
	plan, err := Default().Plan()
	if err != nil {
		t.Fatal(err)
	}
	if len(plan) != DefaultSteps+1 {
		t.Fatalf("plan has %d steps, want %d", len(plan), DefaultSteps+1)
	}
	if plan[0].Counts().Schema != len(SchemaSQL) {
		t.Fatalf("step 0: %s", plan[0].Summary())
	}
	var total StepCounts
	overflowing, inline := 0, 0
	for k, s := range plan {
		if s.Number != k {
			t.Fatalf("step %d numbered %d", k, s.Number)
		}
		if len(s.Statements) != len(s.Kinds) {
			t.Fatalf("step %d: %d statements, %d kinds", k, len(s.Statements), len(s.Kinds))
		}
		last := s.Statements[len(s.Statements)-1]
		if last.SQL != userVersionSQL(k) || s.Kinds[len(s.Kinds)-1] != KindUserVersion {
			t.Fatalf("step %d does not end by setting user_version: %q", k, last.SQL)
		}
		counts := s.Counts()
		total.Inserts += counts.Inserts
		total.Updates += counts.Updates
		total.Deletes += counts.Deletes
		total.Indexes += counts.Indexes
		for i, statement := range s.Statements {
			lower := strings.ToLower(statement.SQL)
			for _, banned := range []string{"random", "now", "current_", "select"} {
				if strings.Contains(lower, banned) {
					t.Errorf("step %d statement %d uses %q: %s", k, i, banned, statement.SQL)
				}
			}
			if s.Kinds[i] == KindInsert {
				if !strings.Contains(statement.SQL, "(id,") {
					t.Errorf("step %d insert without an explicit id: %s", k, statement.SQL)
				}
				if id, ok := statement.Args[0].(int64); !ok || id <= 0 {
					t.Errorf("step %d insert id %v", k, statement.Args[0])
				}
			}
			for _, arg := range statement.Args {
				if body, ok := arg.([]byte); ok {
					if len(body) > 4061 {
						overflowing++
					} else {
						inline++
					}
				}
			}
		}
	}
	if total.Indexes != 1 || plan[IndexStep].Counts().Indexes != 1 {
		t.Errorf("want exactly one index creation, at step %d; got %d", IndexStep, total.Indexes)
	}
	if total.Inserts == 0 || total.Updates == 0 || total.Deletes == 0 {
		t.Errorf("want inserts, updates and deletes: %+v", total)
	}
	if overflowing == 0 || inline == 0 {
		t.Errorf("want bodies both above and below the 4061-byte local limit: %d above, %d below", overflowing, inline)
	}
}

// TestBuildIsReproducibleAndAtRest checks byte-identical rebuilds, at-rest state, integrity, overflow pages, and journal mode.
func TestBuildIsReproducibleAndAtRest(t *testing.T) {
	ctx := context.Background()
	for _, mode := range []string{sqlitex.JournalWAL, sqlitex.JournalDelete} {
		r := small(mode, 8)
		dir := t.TempDir()
		first := filepath.Join(dir, "first.db")
		second := filepath.Join(dir, "second.db")
		for _, path := range []string{first, second} {
			if err := r.Build(ctx, path, r.Steps); err != nil {
				t.Fatal(mode, err)
			}
			if err := CheckAtRest(path); err != nil {
				t.Fatal(mode, err)
			}
		}
		a, err := HashFile(first)
		if err != nil {
			t.Fatal(err)
		}
		b, err := HashFile(second)
		if err != nil {
			t.Fatal(err)
		}
		if a != b {
			t.Fatalf("%s: two builds differ: %s vs %s", mode, a.Exact, b.Exact)
		}
		db, err := r.Open(ctx, first)
		if err != nil {
			t.Fatal(err)
		}
		step, err := CurrentStep(ctx, db)
		if err != nil || step != r.Steps {
			t.Fatalf("%s: CurrentStep = %d, %v; want %d", mode, step, err, r.Steps)
		}
		var integrity string
		if err := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil || integrity != "ok" {
			t.Fatalf("%s: integrity %q %v", mode, integrity, err)
		}
		var overflowPages int
		if err := db.QueryRowContext(ctx, "SELECT count(*) FROM dbstat WHERE pagetype = 'overflow'").Scan(&overflowPages); err != nil || overflowPages == 0 {
			t.Fatalf("%s: overflow pages %d %v", mode, overflowPages, err)
		}
		var journalMode string
		if err := db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journalMode); err != nil || journalMode != mode {
			t.Fatalf("journal mode %q %v, want %q", journalMode, err, mode)
		}
		db.Close()
	}
}

// TestStepHashesEqualSeparateBuilds checks that ComputeStepHashes matches fresh builds to each k and all hashes are distinct.
func TestStepHashesEqualSeparateBuilds(t *testing.T) {
	ctx := context.Background()
	r := small(sqlitex.JournalWAL, 5)
	dir := t.TempDir()
	hashes, err := r.ComputeStepHashes(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(hashes) != r.Steps+1 {
		t.Fatalf("%d hashes, want %d", len(hashes), r.Steps+1)
	}
	seen := map[string]int{}
	for k := 0; k <= r.Steps; k++ {
		path := filepath.Join(dir, "k.db")
		if err := r.Build(ctx, path, k); err != nil {
			t.Fatal(err)
		}
		h, err := HashFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if h != hashes[k] {
			t.Fatalf("step %d: separate build %s, step list %s", k, h.Exact, hashes[k].Exact)
		}
		if previous, ok := seen[h.Exact]; ok {
			t.Fatalf("steps %d and %d have the same hash", previous, k)
		}
		seen[h.Exact] = k
	}
}

// TestLongLivedConnectionDiffersOnlyInChangeCounterUnderWAL checks that one WAL connection differs from canonical only in the change counter.
func TestLongLivedConnectionDiffersOnlyInChangeCounterUnderWAL(t *testing.T) {
	ctx := context.Background()
	for _, mode := range []string{sqlitex.JournalWAL, sqlitex.JournalDelete} {
		r := small(mode, 6)
		dir := t.TempDir()
		canonical := filepath.Join(dir, "canonical.db")
		if err := r.Build(ctx, canonical, r.Steps); err != nil {
			t.Fatal(err)
		}
		longLived := filepath.Join(dir, "long.db")
		db, err := r.Open(ctx, longLived)
		if err != nil {
			t.Fatal(err)
		}
		for k := 0; k <= r.Steps; k++ {
			if err := r.ApplyStep(ctx, db, k); err != nil {
				t.Fatal(err)
			}
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		a, err := HashFile(canonical)
		if err != nil {
			t.Fatal(err)
		}
		b, err := HashFile(longLived)
		if err != nil {
			t.Fatal(err)
		}
		if a.Masked != b.Masked {
			t.Fatalf("%s: long-lived build differs beyond the change counter", mode)
		}
		wantExact := mode == sqlitex.JournalDelete
		if (a.Exact == b.Exact) != wantExact {
			t.Fatalf("%s: exact equal = %v, want %v", mode, a.Exact == b.Exact, wantExact)
		}
		match := MatchHashes([]FileHashes{a}, b)
		if match.Step != 0 || match.Exact != wantExact {
			t.Fatalf("%s: match %+v", mode, match)
		}
	}
}

// TestMatchStepAndCache checks step-hash caching, prefix reuse, exact/masked/missing MatchStep lookups, and corrupt-cache recovery.
func TestMatchStepAndCache(t *testing.T) {
	ctx := context.Background()
	t.Setenv(sqlitex.WorkDirEnv, t.TempDir())
	r := small(sqlitex.JournalWAL, 4)
	hashes, err := r.StepHashes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cachePath, err := r.CachePath()
	if err != nil {
		t.Fatal(err)
	}
	cached, _, err := readHashCache(cachePath)
	if err != nil || !reflect.DeepEqual(cached, hashes) {
		t.Fatalf("cache round trip: %v", err)
	}
	shorter, err := r.WithSteps(2).StepHashes(ctx)
	if err != nil || !reflect.DeepEqual(shorter, hashes[:3]) {
		t.Fatalf("shorter recipe should reuse the cached prefix: %v", err)
	}
	expected, err := r.ExpectedHash(ctx, 3)
	if err != nil || expected != hashes[3].Exact {
		t.Fatalf("ExpectedHash(3) = %s, %v", expected, err)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "probe.db")
	if err := r.Build(ctx, path, 3); err != nil {
		t.Fatal(err)
	}
	match, err := r.MatchStep(ctx, path)
	if err != nil || match != (Match{Step: 3, Exact: true}) {
		t.Fatalf("MatchStep = %v, %v", match, err)
	}
	image, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	image[27]++
	image[95]++
	if err := os.WriteFile(path, image, 0o644); err != nil {
		t.Fatal(err)
	}
	match, err = r.MatchStep(ctx, path)
	if err != nil || match != (Match{Step: 3}) {
		t.Fatalf("after changing the change counter: %v, %v", match, err)
	}
	image[4096*2]++
	if err := os.WriteFile(path, image, 0o644); err != nil {
		t.Fatal(err)
	}
	match, err = r.MatchStep(ctx, path)
	if err != nil || match.Found() {
		t.Fatalf("after changing page 3: %v, %v", match, err)
	}
	if err := os.WriteFile(path+"-wal", nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := r.MatchStep(ctx, path); err == nil {
		t.Fatal("a database with a -wal file is not at rest: want an error")
	}

	if err := os.WriteFile(cachePath, []byte("0 nothex nothex\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	again, err := r.StepHashes(ctx)
	if err != nil || !reflect.DeepEqual(again, hashes) {
		t.Fatalf("a corrupt cache should be rebuilt: %v", err)
	}
}

// TestApplyNextFromEmpty checks stepping an empty database through every step and failing past the final step.
func TestApplyNextFromEmpty(t *testing.T) {
	ctx := context.Background()
	r := small(sqlitex.JournalWAL, 3)
	path := filepath.Join(t.TempDir(), "next.db")
	db, err := r.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	step, err := CurrentStep(ctx, db)
	if err != nil || step != -1 {
		t.Fatalf("empty database: CurrentStep = %d, %v", step, err)
	}
	for want := 0; want <= r.Steps; want++ {
		got, err := r.ApplyNext(ctx, db)
		if err != nil || got != want {
			t.Fatalf("ApplyNext = %d, %v; want %d", got, err, want)
		}
	}
	if _, err := r.ApplyNext(ctx, db); err == nil {
		t.Fatal("ApplyNext past the last step: want an error")
	}
}

// TestFailedStepRollsBack checks that a failed step leaves CurrentStep unchanged.
func TestFailedStepRollsBack(t *testing.T) {
	ctx := context.Background()
	r := small(sqlitex.JournalWAL, 2)
	path := filepath.Join(t.TempDir(), "fail.db")
	if err := r.Build(ctx, path, 1); err != nil {
		t.Fatal(err)
	}
	db, err := r.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := r.ApplyStep(ctx, db, 1); err == nil {
		t.Fatal("re-applying step 1 must fail on its explicit primary keys")
	}
	step, err := CurrentStep(ctx, db)
	if err != nil || step != 1 {
		t.Fatalf("after the failed step: CurrentStep = %d, %v", step, err)
	}
}

// TestMaskChangeCounter checks that MaskChangeCounter zeroes only bytes 24..27 and 92..95 without mutating the input slice.
func TestMaskChangeCounter(t *testing.T) {
	image := make([]byte, 100)
	for i := range image {
		image[i] = 0xAA
	}
	masked := MaskChangeCounter(image)
	for i, b := range masked {
		inCounter := (i >= 24 && i < 28) || (i >= 92 && i < 96)
		if inCounter != (b == 0) {
			t.Fatalf("byte %d = %#x", i, b)
		}
	}
	if image[24] != 0xAA {
		t.Fatal("MaskChangeCounter changed its input")
	}
}
