// reader-pinning attacks Experiment 5.3's claims about WAL read marks, multiple
// readers (including exceeding the 4 WAL read-mark slots), half-read cursors,
// journal_size_limit vs TRUNCATE shrinking, read-lock-0 backfill blocking, and
// stale read marks in -shm.
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/nickstrad/walfleet/internal/recipe"
	"github.com/nickstrad/walfleet/internal/sqlitex"
	"github.com/nickstrad/walfleet/internal/walfile"

	_ "modernc.org/sqlite"
)

func report(id, tried, expect, got string, held bool) {
	verdict := "BROKEN"
	if held {
		verdict = "HELD"
	}
	fmt.Printf("ATTACK %s | %s | EXPECT %s | GOT %s | %s\n", id, tried, expect, got, verdict)
}

func fatal(what string, err error) {
	fmt.Fprintf(os.Stderr, "FATAL %s: %v\n", what, err)
	os.Exit(1)
}

func openWriter(ctx context.Context, path string, autoCkpt int) *sqlitex.DB {
	opts := recipe.Default().SQLiteOptions()
	opts.Conn.WALAutocheckpoint = autoCkpt
	db, err := sqlitex.Open(ctx, path, opts)
	if err != nil {
		fatal("open writer", err)
	}
	return db
}

func openReader(ctx context.Context, path string) (*sqlitex.DB, *sql.Tx, int) {
	opts := recipe.Default().SQLiteOptions()
	opts.ImmediateTx = false
	opts.Conn.WALAutocheckpoint = 0
	opts.SingleConnection = true
	db, err := sqlitex.Open(ctx, path, opts)
	if err != nil {
		fatal("open reader", err)
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		db.Close()
		fatal("begin reader", err)
	}
	var n int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM events").Scan(&n); err != nil {
		tx.Rollback()
		db.Close()
		fatal("reader count", err)
	}
	return db, tx, n
}

func queryCount(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) int {
	var n int
	if err := q.QueryRowContext(ctx, "SELECT count(*) FROM events").Scan(&n); err != nil {
		fatal("count", err)
	}
	return n
}

func liveState(ctx context.Context, path, scratch string) walfile.LiveState {
	st, err := walfile.ReadLiveState(ctx, path, scratch)
	if err != nil {
		fatal("live state", err)
	}
	return st
}

func main() {
	if walfile.ServeLiveCopyChild(os.Args) {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	repoRoot, err := sqlitex.RepoRoot()
	if err != nil {
		fatal("repo root", err)
	}
	scratch := filepath.Join(repoRoot, "_work", "adv-ch05", "reader-pinning")
	if err := os.RemoveAll(scratch); err != nil {
		fatal("clean scratch", err)
	}
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		fatal("mkdir scratch", err)
	}

	// PIN-01a (OUTLINE 5.3 bullet 1): Three concurrent readers at distinct commit
	// frames f1 < f2 < f3; verify PASSIVE nBackfill advances step-by-step
	// f1 -> f2 -> f3 -> f4 as each oldest reader closes, and WAL restarts only
	// after the final reader closes.
	{
		dir := filepath.Join(scratch, "pin01a")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fatal("mkdir pin01a", err)
		}
		dbPath := filepath.Join(dir, "r.db")
		r := recipe.Default().WithSteps(12)
		if err := r.Build(ctx, dbPath, 5); err != nil {
			fatal("build pin01a", err)
		}
		w := openWriter(ctx, dbPath, 0)

		type readerSlot struct {
			db    *sqlitex.DB
			tx    *sql.Tx
			rows  int
			frame uint32
		}
		var readers []readerSlot
		for step := 6; step <= 8; step++ {
			if err := r.ApplyStep(ctx, w, step); err != nil {
				fatal("step pin01a", err)
			}
			st := liveState(ctx, dbPath, dir)
			rdb, rtx, cnt := openReader(ctx, dbPath)
			readers = append(readers, readerSlot{db: rdb, tx: rtx, rows: cnt, frame: st.Index.Header().MaxFrame})
		}
		// Commit step 9 beyond all three readers.
		if err := r.ApplyStep(ctx, w, 9); err != nil {
			fatal("step 9 pin01a", err)
		}
		f4 := liveState(ctx, dbPath, dir).Index.Header().MaxFrame
		saltBefore := liveState(ctx, dbPath, dir).WAL.Header.Salt1

		// With all 3 open: PASSIVE stops at f1.
		cp0, err := sqlitex.Checkpoint(ctx, w, "", sqlitex.CheckpointPassive)
		if err != nil {
			fatal("cp0 pin01a", err)
		}
		// Verify all 3 readers still see their exact historical row counts.
		countsStable := true
		for _, rd := range readers {
			if queryCount(ctx, rd.tx) != rd.rows {
				countsStable = false
			}
		}
		// Close R1 -> PASSIVE advances to f2.
		readers[0].tx.Rollback()
		readers[0].db.Close()
		cp1, err := sqlitex.Checkpoint(ctx, w, "", sqlitex.CheckpointPassive)
		if err != nil {
			fatal("cp1 pin01a", err)
		}
		// Close R2 -> PASSIVE advances to f3.
		readers[1].tx.Rollback()
		readers[1].db.Close()
		cp2, err := sqlitex.Checkpoint(ctx, w, "", sqlitex.CheckpointPassive)
		if err != nil {
			fatal("cp2 pin01a", err)
		}
		// Close R3 -> PASSIVE advances to f4.
		readers[2].tx.Rollback()
		readers[2].db.Close()
		cp3, err := sqlitex.Checkpoint(ctx, w, "", sqlitex.CheckpointPassive)
		if err != nil {
			fatal("cp3 pin01a", err)
		}
		// Next commit restarts WAL at frame 1.
		if err := r.ApplyStep(ctx, w, 10); err != nil {
			fatal("step 10 pin01a", err)
		}
		afterRestart := liveState(ctx, dbPath, dir)
		w.Close()

		f1, f2, f3 := readers[0].frame, readers[1].frame, readers[2].frame
		held := countsStable &&
			cp0.Checkpointed == int(f1) &&
			cp1.Checkpointed == int(f2) &&
			cp2.Checkpointed == int(f3) &&
			cp3.Checkpointed == int(f4) &&
			afterRestart.Index.Header().MaxFrame < f4 &&
			afterRestart.WAL.Header.Salt1 == saltBefore+1
		report("PIN-01a",
			"3 concurrent readers pinned at distinct frames f1<f2<f3, closing oldest-first with PASSIVE after each",
			"nBackfill advances strictly f1 -> f2 -> f3 -> f4; snapshots stay stable; next write restarts WAL",
			fmt.Sprintf("marks=[%d,%d,%d] f4=%d backfill=[%d->%d->%d->%d] counts_stable=%v restarted_mx=%d",
				f1, f2, f3, f4, cp0.Checkpointed, cp1.Checkpointed, cp2.Checkpointed, cp3.Checkpointed, countsStable, afterRestart.Index.Header().MaxFrame),
			held)
	}

	// PIN-01b (OUTLINE 5.3 bullet 1 extended): 5 concurrent readers at 5 distinct
	// commit frames f1 < f2 < f3 < f4 < f5, exceeding the 4 WAL read-mark slots
	// aReadMark[1..4] (wal.c:3182-3212).
	{
		dir := filepath.Join(scratch, "pin01b")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fatal("mkdir pin01b", err)
		}
		dbPath := filepath.Join(dir, "r.db")
		r := recipe.Default().WithSteps(14)
		if err := r.Build(ctx, dbPath, 5); err != nil {
			fatal("build pin01b", err)
		}
		w := openWriter(ctx, dbPath, 0)

		type slot struct {
			db    *sqlitex.DB
			tx    *sql.Tx
			rows  int
			frame uint32
		}
		var readers []slot
		for step := 6; step <= 10; step++ {
			if err := r.ApplyStep(ctx, w, step); err != nil {
				fatal("step pin01b", err)
			}
			mx := liveState(ctx, dbPath, dir).Index.Header().MaxFrame
			rdb, rtx, cnt := openReader(ctx, dbPath)
			readers = append(readers, slot{db: rdb, tx: rtx, rows: cnt, frame: mx})
		}
		// Commit step 11 beyond all 5 readers.
		if err := r.ApplyStep(ctx, w, 11); err != nil {
			fatal("step 11 pin01b", err)
		}
		stAll := liveState(ctx, dbPath, dir)
		f6 := stAll.Index.Header().MaxFrame

		// Verify R5 sees step 10's row count (strictly greater than R4's step 9 count),
		// even though all 4 read-mark slots aReadMark[1..4] were occupied by R1..R4.
		r5Distinct := readers[4].rows > readers[3].rows

		// Close R1, R2, R3, AND R4, leaving ONLY R5 open!
		for i := 0; i < 4; i++ {
			readers[i].tx.Rollback()
			readers[i].db.Close()
		}
		cpWithOnlyR5, err := sqlitex.Checkpoint(ctx, w, "", sqlitex.CheckpointPassive)
		if err != nil {
			fatal("cp R5 pin01b", err)
		}
		r5StillStable := queryCount(ctx, readers[4].tx) == readers[4].rows

		// Close R5 -> now PASSIVE can backfill all the way to f6.
		readers[4].tx.Rollback()
		readers[4].db.Close()
		cpAfterR5, err := sqlitex.Checkpoint(ctx, w, "", sqlitex.CheckpointPassive)
		if err != nil {
			fatal("cp after R5 pin01b", err)
		}
		w.Close()

		// Because aReadMark only has 4 WAL slots (1..4), R5 had to share slot 4 (marked at f4),
		// so with only R5 open, checkpointing stops conservatively at f4 (< f5)!
		f4, f5 := readers[3].frame, readers[4].frame
		held := r5Distinct && r5StillStable &&
			cpWithOnlyR5.Checkpointed == int(f4) &&
			cpWithOnlyR5.Checkpointed < int(f5) &&
			cpAfterR5.Checkpointed == int(f6)
		report("PIN-01b",
			"5 concurrent readers at 5 distinct commits (exceeding 4 WAL read-mark slots aReadMark[1..4]); close R1..R4 leaving only R5",
			"R5 opens without error, sees its own snapshot f5, shares slot 4 (f4), so PASSIVE stops conservatively at f4 until R5 closes",
			fmt.Sprintf("f4=%d f5=%d f6=%d R4_rows=%d R5_rows=%d marks=%v backfill_only_R5=%d backfill_after_R5=%d",
				f4, f5, f6, readers[3].rows, readers[4].rows, stAll.Index.Checkpoint.ReadMarks, cpWithOnlyR5.Checkpointed, cpAfterR5.Checkpointed),
			held)
	}

	// PIN-02 (OUTLINE 5.3 bullet 2): A reader holding only a half-read result set
	// (*sql.Rows with 1 row scanned, no explicit BEGIN, no rows.Close()).
	{
		dir := filepath.Join(scratch, "pin02")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fatal("mkdir pin02", err)
		}
		dbPath := filepath.Join(dir, "r.db")
		r := recipe.Default().WithSteps(31)
		if err := r.Build(ctx, dbPath, 5); err != nil {
			fatal("build pin02", err)
		}
		w := openWriter(ctx, dbPath, 200)
		if err := r.ApplyStep(ctx, w, 6); err != nil {
			fatal("step 6 pin02", err)
		}
		pinFrame := liveState(ctx, dbPath, dir).Index.Header().MaxFrame

		// Open reader with NO BeginTx — just a plain Query() left half-consumed.
		rOpts := recipe.Default().SQLiteOptions()
		rOpts.ImmediateTx = false
		rOpts.Conn.WALAutocheckpoint = 0
		rOpts.SingleConnection = true
		rdb, err := sqlitex.Open(ctx, dbPath, rOpts)
		if err != nil {
			fatal("open cursor reader", err)
		}
		rows, err := rdb.QueryContext(ctx, "SELECT id, note FROM events ORDER BY id")
		if err != nil {
			fatal("query cursor", err)
		}
		if !rows.Next() {
			fatal("cursor next", fmt.Errorf("expected at least 1 row"))
		}
		var firstID int
		var firstNote string
		if err := rows.Scan(&firstID, &firstNote); err != nil {
			fatal("cursor scan", err)
		}

		// Commit steps 7..30 while the single unclosed *sql.Rows sits half-read.
		prevSize := liveState(ctx, dbPath, dir).WALSize
		monotone := true
		backfillCapped := true
		for k := 7; k <= 30; k++ {
			if err := r.ApplyStep(ctx, w, k); err != nil {
				fatal("step pin02", err)
			}
			st := liveState(ctx, dbPath, dir)
			if st.WALSize <= prevSize {
				monotone = false
			}
			prevSize = st.WALSize
			if st.Index.Checkpoint.Backfill > pinFrame {
				backfillCapped = false
			}
		}
		peak := liveState(ctx, dbPath, dir)

		// Finish reading remaining rows from the half-read cursor to prove it still
		// sees its original snapshot, then close rows.
		remaining := 1
		for rows.Next() {
			remaining++
		}
		if err := rows.Err(); err != nil {
			fatal("cursor err", err)
		}
		if err := rows.Close(); err != nil {
			fatal("cursor close", err)
		}
		rdb.Close()

		// Now PASSIVE completes and step 31 restarts WAL.
		cpRes, err := sqlitex.Checkpoint(ctx, w, "", sqlitex.CheckpointPassive)
		if err != nil {
			fatal("cp pin02", err)
		}
		if err := r.ApplyStep(ctx, w, 31); err != nil {
			fatal("step 31 pin02", err)
		}
		restarted := liveState(ctx, dbPath, dir)
		w.Close()

		held := monotone && backfillCapped &&
			peak.Index.Checkpoint.Backfill == pinFrame &&
			peak.Index.Header().MaxFrame > 500 &&
			!cpRes.Busy && cpRes.Checkpointed == int(peak.Index.Header().MaxFrame) &&
			restarted.Index.Header().MaxFrame < peak.Index.Header().MaxFrame
		report("PIN-02",
			"reader leaves a single SELECT *sql.Rows half-read (no explicit BEGIN) while writer commits 24 steps (autocheckpoint=200)",
			"half-read cursor pins read mark, freezes nBackfill at pinFrame, grows WAL monotonically; rows.Close() unblocks checkpoint + restart",
			fmt.Sprintf("pin=%d peak_wal=%dB peak_mx=%d peak_backfill=%d cursor_rows=%d after_close_ckpt=%d restarted_mx=%d",
				pinFrame, peak.WALSize, peak.Index.Header().MaxFrame, peak.Index.Checkpoint.Backfill, remaining, cpRes.Checkpointed, restarted.Index.Header().MaxFrame),
			held)
	}

	// PIN-03 (OUTLINE 5.3 bullet 3): When does the WAL file actually shrink?
	// Compare PASSIVE, PRAGMA journal_size_limit=65536 (during checkpoint vs on
	// first commit after restart), PRAGMA journal_size_limit=0, TRUNCATE, and
	// last-connection close.
	{
		growWAL := func(sub string) (string, string, *sqlitex.DB, int64) {
			d := filepath.Join(scratch, "pin03-"+sub)
			if err := os.MkdirAll(d, 0o755); err != nil {
				fatal("mkdir pin03", err)
			}
			p := filepath.Join(d, "r.db")
			r := recipe.Default().WithSteps(20)
			if err := r.Build(ctx, p, 5); err != nil {
				fatal("build pin03", err)
			}
			w := openWriter(ctx, p, 0)
			for k := 6; k <= 16; k++ {
				if err := r.ApplyStep(ctx, w, k); err != nil {
					fatal("step pin03", err)
				}
			}
			peak := liveState(ctx, p, d).WALSize
			return d, p, w, peak
		}

		// (a) journal_size_limit = 65536: PASSIVE does NOT shrink; first commit after restart DOES shrink!
		d1, p1, w1, peak1 := growWAL("limit64k")
		var gotLimit int64
		if err := w1.QueryRowContext(ctx, "PRAGMA journal_size_limit = 65536").Scan(&gotLimit); err != nil {
			fatal("set journal_size_limit", err)
		}
		if _, err := sqlitex.Checkpoint(ctx, w1, "", sqlitex.CheckpointPassive); err != nil {
			fatal("passive pin03a", err)
		}
		sizeAfterPassive := liveState(ctx, p1, d1).WALSize
		// Commit 1 tiny transaction (restarts WAL at frame 1 -> walLimitSize runs at wal.c:4225).
		if _, err := w1.ExecContext(ctx, "PRAGMA user_version = 99"); err != nil {
			fatal("tiny commit pin03a", err)
		}
		sizeAfterRestartCommit := liveState(ctx, p1, d1).WALSize
		w1.Close()

		// (b) journal_size_limit = 0: PASSIVE keeps peak; next tiny commit shrinks to exact new frames (no old tail).
		d2, p2, w2, peak2 := growWAL("limit0")
		if _, err := w2.ExecContext(ctx, "PRAGMA journal_size_limit = 0"); err != nil {
			fatal("set limit 0", err)
		}
		if _, err := sqlitex.Checkpoint(ctx, w2, "", sqlitex.CheckpointPassive); err != nil {
			fatal("passive pin03b", err)
		}
		size0AfterPassive := liveState(ctx, p2, d2).WALSize
		if _, err := w2.ExecContext(ctx, "PRAGMA user_version = 99"); err != nil {
			fatal("tiny commit pin03b", err)
		}
		st0AfterCommit := liveState(ctx, p2, d2)
		w2.Close()

		// (c) TRUNCATE without journal_size_limit shrinks to 0 immediately; last close deletes -wal/-shm.
		d3, p3, w3, peak3 := growWAL("truncate-close")
		if _, err := sqlitex.Checkpoint(ctx, w3, "", sqlitex.CheckpointTruncate); err != nil {
			fatal("truncate pin03c", err)
		}
		sizeAfterTruncate := liveState(ctx, p3, d3).WALSize
		if err := w3.Close(); err != nil {
			fatal("close pin03c", err)
		}
		_, walStatErr := os.Stat(p3 + "-wal")
		_, shmStatErr := os.Stat(p3 + "-shm")
		deletedOnClose := errors.Is(walStatErr, os.ErrNotExist) && errors.Is(shmStatErr, os.ErrNotExist)

		exactNewSize := int64(walfile.HeaderSize) + int64(st0AfterCommit.WAL.ValidFrames)*int64(walfile.FrameHeaderSize+4096)
		held := peak1 > 500000 &&
			gotLimit == 65536 &&
			sizeAfterPassive == peak1 &&
			sizeAfterRestartCommit == 65536 &&
			size0AfterPassive == peak2 &&
			st0AfterCommit.WALSize == exactNewSize &&
			len(st0AfterCommit.WAL.Frames) == st0AfterCommit.WAL.ValidFrames &&
			peak3 > 500000 &&
			sizeAfterTruncate == 0 &&
			deletedOnClose
		report("PIN-03",
			"shrink a ~1 MB WAL via PRAGMA journal_size_limit=65536, journal_size_limit=0, wal_checkpoint(TRUNCATE), and last close",
			"PASSIVE never shrinks; journal_size_limit shrinks on the FIRST commit after restart (65536 or exact new frames); TRUNCATE shrinks to 0 immediately; last close removes -wal/-shm",
			fmt.Sprintf("peak=%dB limit64k(after_passive=%dB,after_commit=%dB) limit0(after_passive=%dB,after_commit=%dB=exact_%dB) truncate=%dB deleted_on_close=%v",
				peak1, sizeAfterPassive, sizeAfterRestartCommit, size0AfterPassive, st0AfterCommit.WALSize, exactNewSize, sizeAfterTruncate, deletedOnClose),
			held)
	}

	// PIN-04 (build-notes correction 3): Read-lock-0 reader (started when
	// nBackfill == mxFrame) blocks ALL four checkpoint modes from copying a
	// single frame, even though aReadMark[1..4] are unused/0.
	{
		dir := filepath.Join(scratch, "pin04")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fatal("mkdir pin04", err)
		}
		dbPath := filepath.Join(dir, "r.db")
		r := recipe.Default().WithSteps(10)
		if err := r.Build(ctx, dbPath, 5); err != nil {
			fatal("build pin04", err)
		}
		w := openWriter(ctx, dbPath, 0)
		if err := r.ApplyStep(ctx, w, 6); err != nil {
			fatal("step 6 pin04", err)
		}
		if _, err := sqlitex.Checkpoint(ctx, w, "", sqlitex.CheckpointPassive); err != nil {
			fatal("initial passive pin04", err)
		}
		// Start reader while WAL is fully backfilled -> acquires WAL_READ_LOCK(0).
		rdb, rtx, rCnt := openReader(ctx, dbPath)
		// Writer restarts WAL and commits steps 7 and 8 (succeeds despite read-lock-0!).
		if err := r.ApplyStep(ctx, w, 7); err != nil {
			fatal("step 7 pin04", err)
		}
		if err := r.ApplyStep(ctx, w, 8); err != nil {
			fatal("step 8 pin04", err)
		}
		st := liveState(ctx, dbPath, dir)
		mx := int(st.Index.Header().MaxFrame)

		const busyMs = 150
		cpOpts := recipe.Default().SQLiteOptions()
		cpOpts.Conn.WALAutocheckpoint = 0
		cpOpts.Conn.BusyTimeout = busyMs * time.Millisecond
		cpDB, err := sqlitex.Open(ctx, dbPath, cpOpts)
		if err != nil {
			fatal("open cp pin04", err)
		}
		modes := []string{sqlitex.CheckpointPassive, sqlitex.CheckpointFull, sqlitex.CheckpointRestart, sqlitex.CheckpointTruncate}
		var problems []string
		var summary []string
		for _, mode := range modes {
			t0 := time.Now()
			res, err := sqlitex.Checkpoint(ctx, cpDB, "", mode)
			elapsed := time.Since(t0)
			if err != nil {
				problems = append(problems, fmt.Sprintf("%s err=%v", mode, err))
				continue
			}
			wantBusy := mode != sqlitex.CheckpointPassive
			waited := elapsed >= busyMs*time.Millisecond
			summary = append(summary, fmt.Sprintf("%s=%v/%d/%d(%dms)", mode, res.Busy, res.LogFrames, res.Checkpointed, elapsed.Milliseconds()))
			if res.Busy != wantBusy || res.Checkpointed != 0 || res.LogFrames != mx || waited != wantBusy {
				problems = append(problems, fmt.Sprintf("%s got busy=%v(want %v) ckpt=%d(want 0) waited=%v(want %v)", mode, res.Busy, wantBusy, res.Checkpointed, waited, wantBusy))
			}
		}
		rStill := queryCount(ctx, rtx) == rCnt
		rtx.Rollback()
		rdb.Close()
		cpDB.Close()
		w.Close()

		report("PIN-04",
			"read-lock-0 reader (begun after full backfill) vs PASSIVE/FULL/RESTART/TRUNCATE after WAL restarts",
			"writer restarts WAL; all 4 modes backfill 0 frames; PASSIVE returns busy=0 immediately; FULL/RESTART/TRUNCATE wait and return busy=1",
			fmt.Sprintf("mx=%d r_stable=%v results=%v problems=%v", mx, rStill, summary, problems),
			rStill && len(problems) == 0)
	}

	// PIN-05 (build-notes correction 5): Read marks in -shm outlive their readers.
	{
		dir := filepath.Join(scratch, "pin05")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fatal("mkdir pin05", err)
		}
		dbPath := filepath.Join(dir, "r.db")
		r := recipe.Default().WithSteps(10)
		if err := r.Build(ctx, dbPath, 5); err != nil {
			fatal("build pin05", err)
		}
		w := openWriter(ctx, dbPath, 0)
		if err := r.ApplyStep(ctx, w, 6); err != nil {
			fatal("step 6 pin05", err)
		}
		f1 := liveState(ctx, dbPath, dir).Index.Header().MaxFrame
		rdb, rtx, _ := openReader(ctx, dbPath)
		if err := r.ApplyStep(ctx, w, 7); err != nil {
			fatal("step 7 pin05", err)
		}
		f2 := liveState(ctx, dbPath, dir).Index.Header().MaxFrame
		// Close reader completely.
		rtx.Rollback()
		rdb.Close()

		// Inspect -shm BEFORE any checkpoint: mark f1 must still sit in aReadMark[1..4].
		afterClose := liveState(ctx, dbPath, dir)
		staleMarkPresent := false
		for i := 1; i < walfile.ReadMarkCount; i++ {
			if afterClose.Index.Checkpoint.ReadMarks[i] == f1 {
				staleMarkPresent = true
			}
		}
		// Run PASSIVE checkpoint: because no lock is held on that slot, SQLite advances
		// the mark and copies all f2 frames.
		cpRes, err := sqlitex.Checkpoint(ctx, w, "", sqlitex.CheckpointPassive)
		if err != nil {
			fatal("cp pin05", err)
		}
		afterCkpt := liveState(ctx, dbPath, dir)
		w.Close()

		held := staleMarkPresent && !cpRes.Busy && cpRes.Checkpointed == int(f2) && afterCkpt.Index.Checkpoint.Backfill == f2
		report("PIN-05",
			"decode -shm read marks immediately after closing the only reader, before any checkpoint",
			"old mark f1 remains written in -shm aReadMark[] even with 0 live readers, yet PASSIVE immediately checkpoints to f2",
			fmt.Sprintf("f1=%d f2=%d marks_after_close=%v stale_mark_present=%v passive_ckpt=%d",
				f1, f2, afterClose.Index.Checkpoint.ReadMarks, staleMarkPresent, cpRes.Checkpointed),
			held)
	}
}
