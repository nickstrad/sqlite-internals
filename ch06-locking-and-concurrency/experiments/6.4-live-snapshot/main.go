// Command 6.4-live-snapshot implements Rebuild Ladder rung R4 (Live snapshot):
// capturing a consistent database from a live WAL writer without stopping writes.
//
// Method 1 reads every page through sqlite_dbpage inside one held read
// transaction while a writer commits and runs PASSIVE checkpoints between chunks,
// proving E1 byte equality against the recipe run to the snapshot step.
// Method 2 copies the database file (torn mid-copy by a PASSIVE checkpoint) and
// then copies -wal while holding a read transaction, proving Chapter 5's WAL
// replayer repairs the torn base to the last commit before the -wal copy.
// The -hold flag selects read (default, BEGIN DEFERRED + read) or immediate
// (BEGIN IMMEDIATE, blocking concurrent commits while checkpoints still run).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/nickstrad/walfleet/internal/capture"
	"github.com/nickstrad/walfleet/internal/equal"
	"github.com/nickstrad/walfleet/internal/procs"
	"github.com/nickstrad/walfleet/internal/recipe"
	"github.com/nickstrad/walfleet/internal/sqlitex"
	"github.com/nickstrad/walfleet/internal/walfile"
)

const (
	baseStep      = 5
	snapshotStep  = 8
	chunks        = 4
	holdRead      = "read"
	holdImmediate = "immediate"
)

type run struct {
	ctx      context.Context
	lab      *procs.Lab
	r        recipe.Recipe
	hold     string
	dir      string
	db       string
	writer   *procs.Child
	capturer *procs.Child
	copier   *procs.Child
	next     int
}

// betweenReads advances the live database between capture chunks: under -hold
// read it commits the next recipe step and runs a PASSIVE checkpoint; under
// -hold immediate it can only checkpoint because the capture holds WAL_WRITE_LOCK.
func (x *run) betweenReads() int {
	if x.hold == holdRead {
		x.lab.Must(x.writer, "recipe-step", strconv.Itoa(x.next))
		x.next++
	}
	before := x.backfill()
	x.lab.Must(x.writer, "checkpoint", sqlitex.CheckpointPassive)
	return x.backfill() - before
}

func (x *run) writerWork() string {
	if x.hold == holdRead {
		return "commits a recipe step and checkpoints"
	}
	return "checkpoints (it cannot commit while the capture holds BEGIN IMMEDIATE)"
}

func (x *run) backfill() int {
	result := x.lab.Must(x.writer, "checkpoint", sqlitex.CheckpointNoop)
	var busy, logFrames, checkpointed int
	if _, err := fmt.Sscanf(result.Value, "busy=%d log=%d checkpointed=%d", &busy, &logFrames, &checkpointed); err != nil {
		x.lab.Fatal("backfill", fmt.Errorf("%q: %w", result.Value, err))
	}
	return checkpointed
}

// beginCapture starts the capture transaction and reads PRAGMA user_version so
// the read snapshot (and its WAL read mark) is actually acquired before copying.
func (x *run) beginCapture() int {
	if x.hold == holdRead {
		x.lab.Must(x.capturer, "begin", "deferred")
	} else {
		x.lab.Must(x.capturer, "begin", "immediate")
	}
	seen := x.lab.Must(x.capturer, "user-version")
	step, err := strconv.Atoi(seen.Value)
	if err != nil {
		x.lab.Fatal("user_version", err)
	}
	return step
}

func (x *run) endCapture(seen int) {
	still := x.lab.Must(x.capturer, "user-version")
	x.lab.Must(x.capturer, "commit")
	x.lab.Check(still.Value == strconv.Itoa(seen), fmt.Sprintf("the capture's transaction saw step %d from its first read to its last", seen))
}

func (x *run) lastCommitted() int { return x.next - 1 }

func (x *run) pageCapture(pages int) int {
	size := (pages + chunks - 1) / chunks
	firstBackfill := -1
	for first := 1; first <= pages; first += size {
		last := min(first+size-1, pages)
		x.lab.Must(x.capturer, "dbpage-write", strconv.Itoa(first), strconv.Itoa(last))
		if last < pages {
			copied := x.betweenReads()
			if firstBackfill < 0 {
				firstBackfill = copied
			}
		}
	}
	return firstBackfill
}

func (x *run) fileAndWALCopy() bool {
	half := x.lab.Must(x.writer, "query", "SELECT (page_count / 2) * page_size FROM pragma_page_count, pragma_page_size")
	x.lab.Must(x.copier, "copy-range", "db", "copy.db", "0", half.Value)
	copied := x.betweenReads()
	x.lab.Must(x.copier, "copy-range", "db", "copy.db", half.Value, "end")
	if x.hold == holdRead {
		x.lab.Must(x.writer, "recipe-step", strconv.Itoa(x.next))
		x.next++
	}
	x.lab.Must(x.copier, "copy", "-wal", "copy.wal")
	return copied > 0
}

// reference builds the exact ground-truth database for step by applying steps
// 6..step on a single long-lived connection (matching the writer child's change
// counter history) and running a TRUNCATE checkpoint so Method 1 can be checked at E1.
func (x *run) reference(step int) string {
	path := filepath.Join(x.dir, "reference", "r.db")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		x.lab.Fatal("mkdir", err)
	}
	if err := x.r.Build(x.ctx, path, baseStep); err != nil {
		x.lab.Fatal("build reference", err)
	}
	options := x.r.SQLiteOptions()
	options.Conn.WALAutocheckpoint = 0
	db, err := sqlitex.Open(x.ctx, path, options)
	if err != nil {
		x.lab.Fatal("open reference", err)
	}
	for k := baseStep + 1; k <= step; k++ {
		if err := x.r.ApplyStep(x.ctx, db, k); err != nil {
			db.Close()
			x.lab.Fatal("reference step", err)
		}
	}
	if _, err := sqlitex.Checkpoint(x.ctx, db, "", sqlitex.CheckpointTruncate); err != nil {
		db.Close()
		x.lab.Fatal("reference checkpoint", err)
	}
	if err := db.Close(); err != nil {
		x.lab.Fatal("close reference", err)
	}
	return path
}

func (x *run) verify(label, path string) (recipe.Match, procs.Inspection) {
	match, err := x.r.MatchStep(x.ctx, path)
	if err != nil {
		x.lab.Fatal("match", err)
	}
	sum, err := equal.FileSHA256(path)
	if err != nil {
		x.lab.Fatal("hash", err)
	}
	inspection, err := procs.InspectDatabase(x.ctx, path, sqlitex.JournalWAL, "PRAGMA user_version")
	if err != nil {
		x.lab.Fatal("inspect", err)
	}
	after, err := equal.FileSHA256(path)
	if err != nil || after != sum {
		x.lab.Fatal("inspect", fmt.Errorf("opening %s changed it (%v)", label, err))
	}
	fmt.Printf("%s: sha256 %s, integrity_check %s, user_version %s, equals recipe %s\n", label, sum[:16], procs.IntegritySummary(inspection.Integrity), inspection.Values[0], match)
	return match, inspection
}

func main() {
	if procs.Serve(procs.Roles) {
		return
	}
	hold := flag.String("hold", holdRead, "transaction held by the capture connection: read (BEGIN, then a read) or immediate (BEGIN IMMEDIATE)")
	flag.Parse()
	if *hold != holdRead && *hold != holdImmediate {
		fmt.Printf("FATAL -hold must be %s or %s, got %q\n", holdRead, holdImmediate, *hold)
		os.Exit(2)
	}
	ctx := context.Background()
	sqlitex.MustPinCheckMemory(ctx)
	dir := sqlitex.MustWorkDir("6.4")
	fmt.Printf("INFO: work dir %s\n", sqlitex.DisplayPath(dir))
	lab := procs.NewLab(os.Stdout)
	x := &run{ctx: ctx, lab: lab, r: recipe.Default(), hold: *hold, dir: dir}
	live := filepath.Join(dir, "live")
	if err := os.MkdirAll(live, 0o755); err != nil {
		lab.Fatal("mkdir", err)
	}
	x.db = filepath.Join(live, "r.db")
	if err := x.r.Build(ctx, x.db, baseStep); err != nil {
		lab.Fatal("build", err)
	}
	snapshotFile := filepath.Join(dir, "snapshot.db")
	fmt.Printf("== live database: recipe built to step %d; the writer commits steps %d-%d (wal_autocheckpoint=0); every capture holds hold=%s\n", baseStep, baseStep+1, snapshotStep, x.hold)
	x.writer = lab.SpawnSQL("writer", procs.SQLOptions{Path: x.db, JournalMode: sqlitex.JournalWAL})
	x.capturer = lab.SpawnSQL("capture", procs.SQLOptions{Path: x.db, JournalMode: sqlitex.JournalWAL, PagesOut: snapshotFile})
	x.copier = lab.Spawn("copier", procs.RoleCopier, "-db", x.db, "-out", dir)
	for k := baseStep + 1; k <= snapshotStep; k++ {
		lab.Must(x.writer, "recipe-step", strconv.Itoa(k))
	}
	x.next = snapshotStep + 1

	fmt.Println()
	fmt.Printf("-- method 1: every page through sqlite_dbpage inside one transaction, in %d chunks; between chunks the writer %s\n", chunks, x.writerWork())
	seen := x.beginCapture()
	lab.Check(seen == snapshotStep, fmt.Sprintf("the capture's transaction sees recipe step %d", snapshotStep))
	pageCount := lab.Must(x.capturer, "query", "PRAGMA page_count")
	pages, err := strconv.Atoi(pageCount.Value)
	if err != nil || pages < chunks {
		lab.Fatal("page count", fmt.Errorf("%q", pageCount.Value))
	}
	firstBackfill := x.pageCapture(pages)
	x.endCapture(seen)
	lab.Check(firstBackfill > 0, "the first checkpoint during the page capture copied WAL frames into the database file under the open transaction")

	fmt.Println()
	fmt.Printf("-- method 2: in a new transaction, plain copies of the database file (in two halves) and then its -wal; between the halves the writer %s\n", x.writerWork())
	secondSnapshot := x.beginCapture()
	lab.Check(secondSnapshot == x.lastCommitted(), fmt.Sprintf("the second transaction sees step %d, the last commit", x.lastCommitted()))
	baseTorn := x.fileAndWALCopy()
	x.endCapture(secondSnapshot)
	if x.hold == holdRead {
		lab.Check(baseTorn, "the checkpoint between the two halves wrote WAL frames into the database file while it was being copied")
	}
	writerSees := lab.Must(x.writer, "user-version")
	if x.hold == holdRead {
		lab.Check(writerSees.Value == strconv.Itoa(x.lastCommitted()) && x.lastCommitted() > snapshotStep,
			fmt.Sprintf("meanwhile the writer committed steps %d-%d", snapshotStep+1, x.lastCommitted()))
	} else {
		lab.Check(writerSees.Value == strconv.Itoa(snapshotStep), "under BEGIN IMMEDIATE the writer committed nothing; it only checkpointed")
	}
	lab.StopChildren()

	fmt.Println()
	fmt.Println("== checks after every process closed")
	captureDir := filepath.Join(dir, "capture")
	manifest, err := capture.Pages(snapshotFile, captureDir)
	if err != nil {
		lab.Fatal("capture", err)
	}
	rebuilt := filepath.Join(dir, "rebuilt.db")
	if _, err := capture.Rebuild(captureDir, rebuilt); err != nil {
		lab.Fatal("rebuild", err)
	}
	fmt.Printf("page capture: %d pages, manifest sha256 %s\n", manifest.PageCount(), manifest.SHA256()[:16])
	pageMatch, pageInspection := x.verify("rebuilt from the page capture", rebuilt)
	reference := x.reference(snapshotStep)
	x.verify(fmt.Sprintf("reference (recipe to step %d on one connection, then checkpoint)", snapshotStep), reference)
	comparison, err := equal.CompareFiles(rebuilt, reference, 4096)
	if err != nil {
		lab.Fatal("compare", err)
	}
	fmt.Printf("E1 rebuilt vs reference: %s\n", comparison)
	lab.Check(pageInspection.Integrity == "ok" && pageMatch.Found() && pageMatch.Step == snapshotStep && comparison.Equal,
		fmt.Sprintf("the page capture passes integrity_check and its bytes equal the recipe run to exactly step %d and checkpointed (E1)", snapshotStep))

	replayed := filepath.Join(dir, "replayed.db")
	report, err := walfile.ReplayFiles(filepath.Join(dir, "copy.db"), filepath.Join(dir, "copy.wal"), replayed)
	if err != nil {
		lab.Fatal("replay", err)
	}
	for _, line := range report.Lines() {
		fmt.Printf("replay: %s\n", line)
	}
	copyMatch, copyInspection := x.verify("rebuilt from the file + -wal copy", replayed)
	lab.Check(copyInspection.Integrity == "ok" && copyMatch.Found() && copyMatch.Step >= secondSnapshot && copyMatch.Step == x.lastCommitted(),
		fmt.Sprintf("the file + -wal copy rebuilds to a recipe state at or after its transaction's snapshot (step %d): step %d, the last commit before the -wal was copied", secondSnapshot, x.lastCommitted()))
	baseMatch, baseInspection := x.verify("the database-file copy on its own", filepath.Join(dir, "copy.db"))
	if x.hold == holdRead {
		lab.Check(baseInspection.Integrity != "ok" || !baseMatch.Found(), "on its own the database-file copy is torn (it fails integrity_check or equals no recipe state); the -wal frames repair it")
	}
	lab.Report("LIVE SNAPSHOT")
}
