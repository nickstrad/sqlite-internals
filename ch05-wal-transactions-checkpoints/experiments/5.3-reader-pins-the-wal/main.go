// Experiment 5.3 (reader-pins-the-wal) demonstrates both ways an open read
// transaction stops checkpoints and forces unbounded WAL growth: part A holds
// a read mark in slot 1..4 so nBackfill stops at the reader's mark and the WAL
// cannot restart; part B begins on a fully checkpointed WAL so the reader holds
// read lock 0 (WAL_READ_LOCK(0)), letting the WAL restart once while blocking
// all subsequent backfill (nBackfill stays 0).
package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickstrad/sqlite-internals/internal/recipe"
	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
	"github.com/nickstrad/sqlite-internals/internal/walfile"

	_ "modernc.org/sqlite"
)

const (
	baseStep           = 5
	readerStartsAfter  = 6
	lastLoopStep       = 30
	nextWriteStep      = 31
	autocheckpointSize = 200
	countQuery         = "SELECT count(*) FROM events"
)

var failures []string

func check(ok bool, claim string) {
	status := "ok"
	if !ok {
		status = "FAILED"
		failures = append(failures, claim)
	}
	fmt.Printf("check %s: %s\n", status, claim)
}

func fatal(what string, err error) {
	fmt.Printf("FATAL %s: %v\n", what, err)
	os.Exit(1)
}

func must[T any](value T, err error) func(string) T {
	return func(what string) T {
		if err != nil {
			fatal(what, err)
		}
		return value
	}
}

type sample struct {
	walSize    int64
	maxFrame   uint32
	backfill   uint32
	marks      [walfile.ReadMarkCount]uint32
	salt1      uint32
	sequence   uint32
	validCount int
	frames     int
}

func take(ctx context.Context, path, scratch string) sample {
	state := must(walfile.ReadLiveState(ctx, path, scratch))("live state")
	if !state.WALPresent || !state.IndexPresent || state.WAL == nil {
		fatal("live state", fmt.Errorf("WAL present %v, wal-index present %v, problem %q", state.WALPresent, state.IndexPresent, state.WALProblem))
	}
	h := state.Index.Header()
	return sample{
		walSize:    state.WALSize,
		maxFrame:   h.MaxFrame,
		backfill:   state.Index.Checkpoint.Backfill,
		marks:      state.Index.Checkpoint.ReadMarks,
		salt1:      state.WAL.Header.Salt1,
		sequence:   state.WAL.Header.CheckpointSequence,
		validCount: state.WAL.ValidFrames,
		frames:     len(state.WAL.Frames),
	}
}

func marksText(marks [walfile.ReadMarkCount]uint32) string {
	parts := make([]string, len(marks))
	for i, m := range marks {
		parts[i] = walfile.ReadMarkText(m)
	}
	return strings.Join(parts, ",")
}

func count(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) int {
	var n int
	if err := q.QueryRowContext(ctx, countQuery).Scan(&n); err != nil {
		fatal("count", err)
	}
	return n
}

func hasMark(marks [walfile.ReadMarkCount]uint32, mark uint32) bool {
	for i := 1; i < len(marks); i++ {
		if marks[i] == mark {
			return true
		}
	}
	return false
}

func openWriter(ctx context.Context, path string) *sqlitex.DB {
	options := recipe.Default().SQLiteOptions()
	options.Conn.WALAutocheckpoint = autocheckpointSize
	return must(sqlitex.Open(ctx, path, options))("open writer")
}

// openReader starts a deferred read-only transaction and executes countQuery so
// SQLite actually calls walTryBeginRead (src/wal.c:3020-3272) and acquires a
// read lock; BEGIN DEFERRED alone takes no lock until the first statement.
func openReader(ctx context.Context, path string) (*sqlitex.DB, *sql.Tx, int) {
	options := recipe.Default().SQLiteOptions()
	options.ImmediateTx = false
	options.Conn.WALAutocheckpoint = 0
	reader := must(sqlitex.Open(ctx, path, options))("open reader")
	tx := must(reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true}))("begin read")
	return reader, tx, count(ctx, tx)
}

func header(label string) {
	fmt.Printf("%-24s %9s %8s %9s  %-22s %11s %11s\n", label, "WAL bytes", "mxFrame", "nBackfill", "read marks [0..4]", "reader rows", "writer rows")
}

func row(label string, s sample, readerRows, writerRows int) {
	reader := "-"
	if readerRows >= 0 {
		reader = fmt.Sprintf("%d", readerRows)
	}
	fmt.Printf("%-24s %9d %8d %9d  %-22s %11s %11d\n", label, s.walSize, s.maxFrame, s.backfill, marksText(s.marks), reader, writerRows)
}

func prepare(ctx context.Context, dir, name string) (string, string) {
	root := filepath.Join(dir, name)
	if err := os.MkdirAll(root, 0o755); err != nil {
		fatal("mkdir", err)
	}
	path := filepath.Join(root, "r.db")
	if err := recipe.Default().Build(ctx, path, baseStep); err != nil {
		fatal("build", err)
	}
	return path, root
}

func partA(ctx context.Context, dir string) {
	fmt.Println()
	fmt.Printf("== A. a reader holds one read transaction open while a writer commits recipe steps %d-%d (wal_autocheckpoint=%d)\n", readerStartsAfter+1, lastLoopStep, autocheckpointSize)
	r := recipe.Default().WithSteps(nextWriteStep)
	path, scratch := prepare(ctx, dir, "a-pinned")
	writer := openWriter(ctx, path)
	if err := r.ApplyStep(ctx, writer, readerStartsAfter); err != nil {
		fatal("step", err)
	}
	header("moment")
	start := take(ctx, path, scratch)
	row(fmt.Sprintf("after step %d", readerStartsAfter), start, -1, count(ctx, writer))
	reader, tx, readerRows := openReader(ctx, path)
	pinned := take(ctx, path, scratch)
	row("reader began", pinned, readerRows, count(ctx, writer))
	snapshot := pinned.maxFrame
	check(snapshot == start.maxFrame && hasMark(pinned.marks, snapshot) && pinned.backfill < snapshot,
		"the reader's snapshot is a read mark equal to mxFrame when it began, and that WAL content was not yet checkpointed")
	samples := []sample{pinned}
	readerStable, marksHeld, backfillBounded := true, true, true
	firstAutocheckpointed := -1
	for k := readerStartsAfter + 1; k <= lastLoopStep; k++ {
		if err := r.ApplyStep(ctx, writer, k); err != nil {
			fatal("step", err)
		}
		s := take(ctx, path, scratch)
		rows := count(ctx, tx)
		row(fmt.Sprintf("step %d committed", k), s, rows, count(ctx, writer))
		readerStable = readerStable && rows == readerRows
		marksHeld = marksHeld && hasMark(s.marks, snapshot)
		backfillBounded = backfillBounded && s.backfill <= snapshot
		if firstAutocheckpointed < 0 && s.backfill == snapshot {
			firstAutocheckpointed = k
		}
		samples = append(samples, s)
	}
	growing, neverRestarted := true, true
	for i := 1; i < len(samples); i++ {
		growing = growing && samples[i].walSize > samples[i-1].walSize
		neverRestarted = neverRestarted && samples[i].maxFrame > samples[i-1].maxFrame && samples[i].salt1 == pinned.salt1
	}
	last := samples[len(samples)-1]
	fmt.Printf("autocheckpoint first reached the reader's mark %d at step %d; mxFrame at the end %d, so %d frames could not be checkpointed\n", snapshot, firstAutocheckpointed, last.maxFrame, last.maxFrame-last.backfill)
	check(readerStable, "the reader's row count never changed while the writer committed")
	check(growing && neverRestarted, "the WAL grew on every commit and never restarted while the reader stayed open")
	check(marksHeld && backfillBounded && firstAutocheckpointed > 0 && last.backfill == snapshot,
		"checkpoints ran (autocheckpoint) but nBackfill stopped at the reader's mark and stayed there")

	// Ending the read transaction releases its shared lock on WAL_READ_LOCK(1)
	// but leaves its mark number written in aReadMark[1] until a checkpoint or
	// later reader updates that slot (src/wal.c:2241-2242, 3196).
	if err := tx.Rollback(); err != nil {
		fatal("end read", err)
	}
	if err := reader.Close(); err != nil {
		fatal("close reader", err)
	}
	ended := take(ctx, path, scratch)
	row("reader ended", ended, -1, count(ctx, writer))
	result := must(sqlitex.Checkpoint(ctx, writer, "", sqlitex.CheckpointPassive))("checkpoint")
	afterCheckpoint := take(ctx, path, scratch)
	row("PASSIVE checkpoint", afterCheckpoint, -1, count(ctx, writer))
	fmt.Printf("wal_checkpoint(PASSIVE) -> %s\n", result)
	check(!result.Busy && afterCheckpoint.backfill == afterCheckpoint.maxFrame && afterCheckpoint.walSize == last.walSize,
		"with the reader gone one checkpoint brings nBackfill to mxFrame; the WAL file keeps its size")
	if err := r.ApplyStep(ctx, writer, nextWriteStep); err != nil {
		fatal("step", err)
	}
	restarted := take(ctx, path, scratch)
	row(fmt.Sprintf("step %d committed", nextWriteStep), restarted, -1, count(ctx, writer))
	fmt.Printf("after the next write: WAL header checkpoint-seq %d -> %d, salt-1 advanced by %d; valid frames %d of %d in the file (the rest are the old generation)\n",
		pinned.sequence, restarted.sequence, restarted.salt1-pinned.salt1, restarted.validCount, restarted.frames)
	check(restarted.maxFrame < last.maxFrame && restarted.validCount == int(restarted.maxFrame) && restarted.salt1 == pinned.salt1+1 && restarted.sequence == pinned.sequence+1 && restarted.walSize == last.walSize,
		"the next write restarted the WAL at frame 1 (salt-1 + 1, checkpoint sequence + 1); the file is reused, not shrunk")
	if err := writer.Close(); err != nil {
		fatal("close", err)
	}
}

func partB(ctx context.Context, dir string) {
	fmt.Println()
	fmt.Println("== B. a reader that began right after a complete checkpoint reads the database file only (read mark 0)")
	r := recipe.Default()
	path, scratch := prepare(ctx, dir, "b-mark-zero")
	writer := openWriter(ctx, path)
	if err := r.ApplyStep(ctx, writer, readerStartsAfter); err != nil {
		fatal("step", err)
	}
	result := must(sqlitex.Checkpoint(ctx, writer, "", sqlitex.CheckpointPassive))("checkpoint")
	fmt.Printf("wal_checkpoint(PASSIVE) after step %d -> %s\n", readerStartsAfter, result)
	header("moment")
	before := take(ctx, path, scratch)
	row("checkpointed", before, -1, count(ctx, writer))
	// With nBackfill == mxFrame, walTryBeginRead takes WAL_READ_LOCK(0) shared
	// instead of setting a mark in slots 1..4 (src/wal.c:3134-3163). The writer
	// can restart the WAL because walRestartLog only locks slots 1..4
	// (src/wal.c:3885), but walCheckpoint cannot backfill a single page because
	// it must take WAL_READ_LOCK(0) exclusively first (src/wal.c:2260).
	reader, tx, readerRows := openReader(ctx, path)
	began := take(ctx, path, scratch)
	row("reader began", began, readerRows, count(ctx, writer))
	check(began.backfill == began.maxFrame && began.marks == before.marks, "the WAL was fully checkpointed, so the reader took no read mark in 1..4 (it holds read lock 0 and ignores the WAL)")
	readerStable, backfillZero := true, true
	var samples []sample
	for k := readerStartsAfter + 1; k <= lastLoopStep; k++ {
		if err := r.ApplyStep(ctx, writer, k); err != nil {
			fatal("step", err)
		}
		s := take(ctx, path, scratch)
		rows := count(ctx, tx)
		row(fmt.Sprintf("step %d committed", k), s, rows, count(ctx, writer))
		readerStable = readerStable && rows == readerRows
		backfillZero = backfillZero && s.backfill == 0
		samples = append(samples, s)
	}
	first, last := samples[0], samples[len(samples)-1]
	fmt.Printf("first write after the reader began: salt-1 advanced by %d, mxFrame %d (the WAL restarted); at the end mxFrame %d with nBackfill %d\n",
		first.salt1-before.salt1, first.maxFrame, last.maxFrame, last.backfill)
	check(first.salt1 == before.salt1+1 && first.maxFrame < before.maxFrame, "a reader on read lock 0 does not stop the writer from restarting the WAL")
	check(readerStable, "the reader's row count never changed")
	check(backfillZero && last.maxFrame >= autocheckpointSize, "but no checkpoint can copy a single frame while it is open: nBackfill stays 0 although autocheckpoint ran on every commit past the threshold")
	if err := tx.Rollback(); err != nil {
		fatal("end read", err)
	}
	if err := reader.Close(); err != nil {
		fatal("close reader", err)
	}
	result = must(sqlitex.Checkpoint(ctx, writer, "", sqlitex.CheckpointPassive))("checkpoint")
	after := take(ctx, path, scratch)
	row("reader ended + PASSIVE", after, -1, count(ctx, writer))
	fmt.Printf("wal_checkpoint(PASSIVE) -> %s\n", result)
	check(after.backfill == after.maxFrame, "once the reader ends, a checkpoint copies everything")
	if err := writer.Close(); err != nil {
		fatal("close", err)
	}
}

func main() {
	// Re-exec entry point for child-process live copies.
	if walfile.ServeLiveCopyChild(os.Args) {
		return
	}
	ctx := context.Background()
	sqlitex.MustPinCheckMemory(ctx)
	dir := sqlitex.MustWorkDir("5.3")
	fmt.Printf("INFO: work dir %s\n", displayPath(dir))
	partA(ctx, dir)
	partB(ctx, dir)
	fmt.Println()
	if len(failures) > 0 {
		fmt.Printf("READER PINS THE WAL FAILED: %d claim(s) did not hold\n", len(failures))
		for _, f := range failures {
			fmt.Printf("  - %s\n", f)
		}
		os.Exit(1)
	}
	fmt.Println("READER PINS THE WAL OK")
}

func displayPath(path string) string {
	root, err := sqlitex.RepoRoot()
	if err != nil {
		return path
	}
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	relative, err := filepath.Rel(root, path)
	if err != nil || strings.HasPrefix(relative, "..") {
		return path
	}
	return filepath.ToSlash(relative)
}
