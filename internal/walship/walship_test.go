package walship

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickstrad/sqlite-internals/internal/procs"
	"github.com/nickstrad/sqlite-internals/internal/recipe"
	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
	"github.com/nickstrad/sqlite-internals/internal/walfile"
)

func TestMain(m *testing.M) {
	if procs.Serve(procs.Roles) {
		return
	}
	os.Exit(m.Run())
}

type harness struct {
	t      *testing.T
	ctx    context.Context
	r      recipe.Recipe
	dir    string
	db     string
	store  *DirStore
	hashes []recipe.FileHashes
}

func newHarness(t *testing.T, base int) *harness {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	t.Setenv("COURSE_WORK_DIR", filepath.Join(dir, "work"))
	r := recipe.Default().WithSteps(16)
	h := &harness{t: t, ctx: ctx, r: r, dir: dir, db: filepath.Join(dir, "live", "db.sqlite")}
	if err := os.MkdirAll(filepath.Dir(h.db), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := r.Build(ctx, h.db, base); err != nil {
		t.Fatal(err)
	}
	store, err := NewDirStore(filepath.Join(dir, "store"))
	if err != nil {
		t.Fatal(err)
	}
	h.store = store
	hashes, err := r.StepHashes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	h.hashes = hashes
	return h
}

func (h *harness) config(every int, origin int64) Config {
	return Config{Database: h.db, Store: h.store, Options: h.r.SQLiteOptions(), SnapshotEvery: every, Origin: origin}
}

func (h *harness) open(every int, origin int64) *Shipper {
	h.t.Helper()
	s, err := Open(h.ctx, h.config(every, origin))
	if err != nil {
		h.t.Fatal(err)
	}
	return s
}

func (h *harness) step(k int) {
	h.t.Helper()
	options := h.r.SQLiteOptions()
	options.Conn.WALAutocheckpoint = 0
	db, err := sqlitex.Open(h.ctx, h.db, options)
	if err != nil {
		h.t.Fatal(err)
	}
	if err := h.r.ApplyStep(h.ctx, db, k); err != nil {
		db.Close()
		h.t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) cycle(s *Shipper) CycleReport {
	h.t.Helper()
	report, err := s.Cycle(h.ctx)
	if err != nil {
		h.t.Fatal(err)
	}
	return report
}

func (h *harness) catalog() *Catalog {
	h.t.Helper()
	catalog, err := LoadCatalog(h.ctx, h.store)
	if err != nil {
		h.t.Fatal(err)
	}
	return catalog
}

func (h *harness) checkEveryPosition(catalog *Catalog, first, last int64) {
	h.t.Helper()
	for position := first; position <= last; position++ {
		plan, err := catalog.Plan(position)
		if err != nil {
			h.t.Fatalf("plan %d: %v", position, err)
		}
		image, _, err := plan.Execute()
		if err != nil {
			h.t.Fatalf("restore %d: %v", position, err)
		}
		if got, want := SHA256Hex(image), h.hashes[position].Exact; got != want {
			h.t.Fatalf("restore %d: sha256 %s, recipe step %d is %s", position, got[:16], position, want[:16])
		}
	}
}

func TestShipAndRestoreEveryCommit(t *testing.T) {
	h := newHarness(t, 2)
	s := h.open(2, 2)
	if s.OpenSnapshot() == nil || s.OpenSnapshot().Reason != ReasonInitial || s.OpenSnapshot().UserVersion != 2 {
		t.Fatalf("initial snapshot %+v", s.OpenSnapshot())
	}
	generations := map[string]bool{}
	for k := 3; k <= 12; k++ {
		h.step(k)
		if k%3 == 0 {
			h.step(k + 1)
			k++
		}
		report := h.cycle(s)
		if report.Segment == nil {
			t.Fatalf("step %d: no segment", k)
		}
		if report.Segment.LastPosition != int64(k) || report.Segment.Commits[len(report.Segment.Commits)-1].UserVersion != uint32(k) {
			t.Fatalf("segment %+v does not end at %d", report.Segment, k)
		}
		if !report.FullBackfill {
			t.Fatalf("step %d: checkpoint %s did not copy every frame", k, report.Checkpoint)
		}
		generations[report.Segment.Generation.Header] = true
	}
	if len(generations) < 2 {
		t.Fatalf("every segment came from one WAL generation; restarts never happened")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	catalog := h.catalog()
	if len(catalog.Damaged) != 0 {
		t.Fatalf("damaged: %+v", catalog.Damaged)
	}
	if len(catalog.Snapshots) < 3 {
		t.Fatalf("%d snapshots, want periodic ones", len(catalog.Snapshots))
	}
	h.checkEveryPosition(catalog, 2, 12)
	for _, snap := range catalog.Snapshots {
		for target := snap.Meta.Position; target <= 12; target++ {
			plan, err := catalog.PlanFrom(snap.Meta.Position, target)
			if err != nil {
				t.Fatal(err)
			}
			image, _, err := plan.Execute()
			if err != nil {
				t.Fatal(err)
			}
			if SHA256Hex(image) != h.hashes[target].Exact {
				t.Fatalf("from %d to %d differs", snap.Meta.Position, target)
			}
		}
	}
}

func TestNothingNewShipsNothing(t *testing.T) {
	h := newHarness(t, 2)
	s := h.open(0, 2)
	defer s.Close()
	report := h.cycle(s)
	if report.Segment != nil || report.Snapshot != nil || report.Change != WALUnchanged {
		t.Fatalf("unexpected report %+v", report)
	}
	h.step(3)
	if report := h.cycle(s); report.Segment == nil {
		t.Fatal("no segment after a commit")
	}
	if report := h.cycle(s); report.Segment != nil {
		t.Fatal("a second segment without a commit")
	}
}

func (h *harness) keeper() *sql.Conn {
	h.t.Helper()
	db, err := sqlitex.Open(h.ctx, h.db, h.r.SQLiteOptions())
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { db.Close() })
	conn, err := db.Conn(h.ctx)
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { conn.Close() })
	return conn
}

func TestResumeInTheSameGenerationContinues(t *testing.T) {
	h := newHarness(t, 2)
	reader := h.keeper()
	s := h.open(0, 2)
	h.step(3)
	if _, err := reader.ExecContext(h.ctx, "BEGIN"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := reader.QueryRowContext(h.ctx, "SELECT count(*) FROM accounts").Scan(&n); err != nil {
		t.Fatal(err)
	}
	h.step(4)
	report := h.cycle(s)
	if report.FullBackfill || report.Segment == nil || report.Segment.LastPosition != 4 {
		t.Fatalf("with a reader at step 3: %v", report.Lines((&GenerationNames{}).Name))
	}
	h.step(5)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	h.step(6)
	s = h.open(0, 2)
	if !s.Resumed() || s.Position() != 4 {
		t.Fatalf("resumed=%t position=%d", s.Resumed(), s.Position())
	}
	report = h.cycle(s)
	if report.Snapshot != nil || report.Segment == nil || report.Segment.FirstPosition != 5 || report.Segment.LastPosition != 6 {
		t.Fatalf("after resume: %v", report.Lines((&GenerationNames{}).Name))
	}
	if _, err := reader.ExecContext(h.ctx, "COMMIT"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	h.checkEveryPosition(h.catalog(), 2, 6)
}

func TestResumeAfterARestartTakesASnapshot(t *testing.T) {
	h := newHarness(t, 2)
	h.keeper()
	s := h.open(0, 2)
	h.step(3)
	if report := h.cycle(s); !report.FullBackfill {
		t.Fatalf("no full backfill: %v", report.Lines((&GenerationNames{}).Name))
	}
	h.step(4)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	h.step(5)
	s = h.open(0, 2)
	defer s.Close()
	report := h.cycle(s)
	if report.Snapshot == nil || report.Snapshot.Reason != ReasonDiscontinuity || report.Snapshot.Position != 4 || report.Snapshot.UserVersion != 5 {
		t.Fatalf("want a discontinuity snapshot at position 4 holding step 5: %v", report.Lines((&GenerationNames{}).Name))
	}
	if report.Change != WALDiscontinuity || !strings.Contains(report.Reason, "held no read mark") {
		t.Fatalf("change %s %q", report.Change, report.Reason)
	}
}

func TestOutsideCheckpointForcesSnapshot(t *testing.T) {
	h := newHarness(t, 2)
	keeper := h.keeper()
	s := h.open(0, 2)
	h.step(3)
	h.cycle(s)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	h.step(4)
	h.step(5)
	if _, err := sqlitex.Checkpoint(h.ctx, keeper, "", sqlitex.CheckpointTruncate); err != nil {
		t.Fatal(err)
	}
	h.step(6)
	s = h.open(0, 2)
	defer s.Close()
	report := h.cycle(s)
	if report.Snapshot == nil || report.Snapshot.Reason != ReasonDiscontinuity || report.Snapshot.Position != 4 || report.Snapshot.UserVersion != 6 {
		t.Fatalf("want a discontinuity snapshot at position 4 holding step 6: %v", report.Lines((&GenerationNames{}).Name))
	}
	if report.Change != WALDiscontinuity || !strings.Contains(report.Reason, "held no read mark") {
		t.Fatalf("change %s %q", report.Change, report.Reason)
	}
	h.step(7)
	report = h.cycle(s)
	if report.Segment == nil || report.Segment.FirstPosition != 5 || report.Segment.Commits[0].UserVersion != 7 {
		t.Fatalf("after the snapshot: %v", report.Lines((&GenerationNames{}).Name))
	}
	catalog := h.catalog()
	plan, err := catalog.Plan(5)
	if err != nil {
		t.Fatal(err)
	}
	image, _, err := plan.Execute()
	if err != nil {
		t.Fatal(err)
	}
	if SHA256Hex(image) != h.hashes[7].Exact {
		t.Fatal("position 5 does not restore recipe step 7")
	}
	if _, err := catalog.PlanFrom(2, 5); !errors.Is(err, ErrBrokenChain) {
		t.Fatalf("crossing the gap from the first snapshot: %v", err)
	}
}

func (h *harness) shipSteps(s *Shipper, from, to int) {
	h.t.Helper()
	for k := from; k <= to; k++ {
		h.step(k)
		h.cycle(s)
	}
}

func copyStore(t *testing.T, from *DirStore) *DirStore {
	t.Helper()
	to, err := NewDirStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	keys, err := from.List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range keys {
		data, err := from.Get(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		if err := to.PutIfAbsent(ctx, key, data); err != nil {
			t.Fatal(err)
		}
	}
	return to
}

func planError(t *testing.T, store Store, target int64) error {
	t.Helper()
	catalog, err := LoadCatalog(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	_, err = catalog.Plan(target)
	return err
}

func TestDamagedStoresAreRefusedBeforeReplay(t *testing.T) {
	h := newHarness(t, 2)
	s := h.open(0, 2)
	h.shipSteps(s, 3, 7)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := planError(t, h.store, 7); err != nil {
		t.Fatalf("intact store: %v", err)
	}
	ctx := context.Background()
	get := func(store Store, key string) []byte {
		data, err := store.Get(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	replace := func(store Store, key string, data []byte) {
		if err := store.Delete(ctx, key); err != nil {
			t.Fatal(err)
		}
		if err := store.PutIfAbsent(ctx, key, data); err != nil {
			t.Fatal(err)
		}
	}

	missing := copyStore(t, h.store)
	if err := missing.Delete(ctx, SegmentKey(5)); err != nil {
		t.Fatal(err)
	}
	if err := planError(t, missing, 7); !errors.Is(err, ErrBrokenChain) || !strings.Contains(err.Error(), "no segment starts at position 5") {
		t.Fatalf("missing segment: %v", err)
	}
	if err := planError(t, missing, 4); err != nil {
		t.Fatalf("a target before the gap still restores: %v", err)
	}

	swapped := copyStore(t, h.store)
	four, five := get(swapped, SegmentKey(4)), get(swapped, SegmentKey(5))
	replace(swapped, SegmentKey(4), five)
	replace(swapped, SegmentKey(5), four)
	if err := planError(t, swapped, 7); !errors.Is(err, ErrBrokenChain) || !strings.Contains(err.Error(), "damaged") {
		t.Fatalf("swapped segments: %v", err)
	}

	rekeyed := copyStore(t, h.store)
	meta4, frames4, err := DecodeSegment(four)
	if err != nil {
		t.Fatal(err)
	}
	meta5, frames5, err := DecodeSegment(five)
	if err != nil {
		t.Fatal(err)
	}
	renumber := func(m SegmentMeta, first int64) SegmentMeta {
		m.Commits = append([]CommitPoint(nil), m.Commits...)
		for i := range m.Commits {
			m.Commits[i].Position = first + int64(i)
		}
		m.FirstPosition, m.LastPosition = first, first+int64(len(m.Commits))-1
		return m
	}
	encoded4, err := EncodeSegment(renumber(meta5, 4), frames5)
	if err != nil {
		t.Fatal(err)
	}
	encoded5, err := EncodeSegment(renumber(meta4, 5), frames4)
	if err != nil {
		t.Fatal(err)
	}
	replace(rekeyed, SegmentKey(4), encoded4)
	replace(rekeyed, SegmentKey(5), encoded5)
	if err := planError(t, rekeyed, 7); !errors.Is(err, ErrBrokenChain) || !strings.Contains(err.Error(), "before hash") {
		t.Fatalf("reordered with consistent keys: %v", err)
	}

	flipped := copyStore(t, h.store)
	object := get(flipped, SegmentKey(6))
	object[len(object)-100] ^= 0x40
	replace(flipped, SegmentKey(6), object)
	if err := planError(t, flipped, 7); !errors.Is(err, ErrBrokenChain) || !strings.Contains(err.Error(), "damaged") {
		t.Fatalf("flipped body byte: %v", err)
	}

	resealed := copyStore(t, h.store)
	meta6, frames6, err := DecodeSegment(get(resealed, SegmentKey(6)))
	if err != nil {
		t.Fatal(err)
	}
	frames6[len(frames6)-100] ^= 0x40
	encoded6, err := EncodeSegment(meta6, frames6)
	if err != nil {
		t.Fatal(err)
	}
	replace(resealed, SegmentKey(6), encoded6)
	if err := planError(t, resealed, 7); !errors.Is(err, ErrBrokenChain) || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("flipped byte with a recomputed body hash: %v", err)
	}
}

func TestStandaloneWALIsAValidWAL(t *testing.T) {
	h := newHarness(t, 2)
	s := h.open(0, 2)
	h.step(3)
	h.step(4)
	report := h.cycle(s)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	meta := *report.Segment
	catalog := h.catalog()
	frames := catalog.Segments[0].Frames
	for _, c := range meta.Commits {
		wal, err := StandaloneWAL(meta, frames, c.Frame)
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := walfile.Parse(wal)
		if err != nil {
			t.Fatal(err)
		}
		if parsed.ValidFrames != c.Frame-meta.FirstFrame+1 || parsed.LastCommitFrame != parsed.ValidFrames || parsed.DatabasePages() != c.Pages {
			t.Fatalf("commit at frame %d: valid %d last commit %d pages %d", c.Frame, parsed.ValidFrames, parsed.LastCommitFrame, parsed.DatabasePages())
		}
	}
	if _, err := StandaloneWAL(meta, frames, meta.LastFrame+1); err == nil {
		t.Fatal("a frame past the segment was accepted")
	}
	wrongSeed := meta
	wrongSeed.SeedChecksum[0]++
	if err := VerifySegmentFrames(wrongSeed, frames); !errors.Is(err, ErrChecksumChain) {
		t.Fatalf("wrong seed: %v", err)
	}
	if _, _, err := ApplySegment(catalog.Snapshots[0].Image, wrongSeed, frames, meta.LastFrame); !errors.Is(err, ErrChecksumChain) {
		t.Fatalf("apply with a wrong seed: %v", err)
	}
}

func TestPruneKeepsWhatRestoresNeed(t *testing.T) {
	h := newHarness(t, 2)
	s := h.open(2, 2)
	h.shipSteps(s, 3, 9)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	catalog := h.catalog()
	var positions []int64
	for _, snap := range catalog.Snapshots {
		positions = append(positions, snap.Meta.Position)
	}
	if !reflect.DeepEqual(positions, []int64{2, 4, 6, 8}) {
		t.Fatalf("snapshot positions %v", positions)
	}
	deleted, err := Prune(h.ctx, h.store, 7)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{SnapshotKey(2), SnapshotKey(4), SegmentKey(3), SegmentKey(4), SegmentKey(5), SegmentKey(6)}
	if !reflect.DeepEqual(deleted, want) {
		t.Fatalf("deleted %v, want %v", deleted, want)
	}
	h.checkEveryPosition(h.catalog(), 6, 9)
	if err := planError(t, h.store, 5); !errors.Is(err, ErrNoSnapshot) {
		t.Fatalf("restoring before the kept snapshot: %v", err)
	}
	if _, err := Prune(h.ctx, h.store, 1); !errors.Is(err, ErrNoSnapshot) {
		t.Fatalf("prune before every snapshot: %v", err)
	}
}

func TestReadingTheWALInAProcessWithAConnectionKeepsItsLocks(t *testing.T) {
	h := newHarness(t, 2)
	options := procs.SQLOptions{Path: h.db, JournalMode: sqlitex.JournalWAL}
	var out bytes.Buffer
	script := procs.NewScript(&out)
	defer script.Kill()
	holder, err := script.Spawn("holder", procs.RoleSQL, options.Args()...)
	if err != nil {
		t.Fatal(err)
	}
	other, err := script.Spawn("other", procs.RoleSQL, options.Args()...)
	if err != nil {
		t.Fatal(err)
	}
	expect := func(child *procs.Child, code int, action string, args ...string) {
		t.Helper()
		r, err := script.Do(child, action, args...)
		if err != nil {
			t.Fatal(err)
		}
		if r.Code != code {
			t.Fatalf("%s %s %v: %s", child.Name, action, args, r.Outcome())
		}
	}
	expect(holder, 0, "begin", "immediate")
	expect(holder, 0, "exec", "PRAGMA user_version = 99")
	expect(holder, 0, "stray-close", "-wal")
	expect(other, sqlitex.CodeBusy, "begin", "immediate")
	expect(holder, 0, "stray-close", "-shm")
	expect(other, 0, "begin", "immediate")
	expect(other, 0, "rollback")
	expect(holder, 0, "rollback")
	if err := script.Close(); err != nil {
		t.Fatal(err)
	}
}
