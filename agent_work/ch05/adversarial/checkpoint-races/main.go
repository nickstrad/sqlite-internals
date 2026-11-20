// checkpoint-races attacks Experiment 5.4's claims about checkpoint modes:
// writer-vs-checkpoint lock contention and concurrent racing, rogue second
// connection checkpointing, the OUTLINE 5.4 "salt-1 increments on restart"
// claim under cross-connection TRUNCATE, WAL_CKPT_LOCK immediate-busy behavior
// (ignoring busy_timeout), lock-free NOOP, and last-connection-close cleanup.
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/nickstrad/sqlite-internals/internal/equal"
	"github.com/nickstrad/sqlite-internals/internal/recipe"
	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
	"github.com/nickstrad/sqlite-internals/internal/walfile"

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

func openConn(ctx context.Context, path string, immediate bool, busy time.Duration, autoCkpt int) *sqlitex.DB {
	opts := recipe.Default().SQLiteOptions()
	opts.ImmediateTx = immediate
	opts.Conn.BusyTimeout = busy
	opts.Conn.WALAutocheckpoint = autoCkpt
	opts.SingleConnection = true
	db, err := sqlitex.Open(ctx, path, opts)
	if err != nil {
		fatal("openConn", err)
	}
	return db
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
	scratch := filepath.Join(repoRoot, "_work", "adv-ch05", "checkpoint-races")
	if err := os.RemoveAll(scratch); err != nil {
		fatal("clean scratch", err)
	}
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		fatal("mkdir scratch", err)
	}

	// Build a closed step-5 template reused across sub-cases.
	templatePath := filepath.Join(scratch, "template.db")
	if err := recipe.Default().Build(ctx, templatePath, 5); err != nil {
		fatal("build template", err)
	}
	templateBytes, err := os.ReadFile(templatePath)
	if err != nil {
		fatal("read template", err)
	}

	cloneTemplate := func(name string) (string, string) {
		d := filepath.Join(scratch, name)
		if err := os.MkdirAll(d, 0o755); err != nil {
			fatal("mkdir "+name, err)
		}
		p := filepath.Join(d, "r.db")
		if err := os.WriteFile(p, templateBytes, 0o644); err != nil {
			fatal("write template "+name, err)
		}
		return d, p
	}

	// CHK-01a (OUTLINE 5.4 bullet 1, part a): Held write transaction (WAL_WRITE_LOCK)
	// vs each of the 4 checkpoint modes.
	{
		const busyMs = 150
		modes := []string{
			sqlitex.CheckpointPassive,
			sqlitex.CheckpointFull,
			sqlitex.CheckpointRestart,
			sqlitex.CheckpointTruncate,
		}
		var problems []string
		var summary []string
		r := recipe.Default().WithSteps(10)

		for _, mode := range modes {
			d, p := cloneTemplate("chk01a-" + mode)
			w := openConn(ctx, p, true, 5*time.Second, 0)
			if err := r.ApplyStep(ctx, w, 6); err != nil {
				fatal("step 6", err)
			}
			if err := r.ApplyStep(ctx, w, 7); err != nil {
				fatal("step 7", err)
			}
			before := liveState(ctx, p, d)
			committedFrames := int(before.Index.Header().MaxFrame)

			// Writer acquires WAL_WRITE_LOCK via BEGIN IMMEDIATE + UPDATE and holds it.
			tx, err := w.BeginTx(ctx, nil)
			if err != nil {
				fatal("begin write", err)
			}
			if _, err := tx.ExecContext(ctx, "UPDATE events SET amount = amount + 1 WHERE id = 1"); err != nil {
				tx.Rollback()
				w.Close()
				fatal("update write", err)
			}

			cp := openConn(ctx, p, true, busyMs*time.Millisecond, 0)
			t0 := time.Now()
			res, err := sqlitex.Checkpoint(ctx, cp, "", mode)
			elapsed := time.Since(t0)
			if err != nil {
				problems = append(problems, fmt.Sprintf("%s err=%v", mode, err))
			}
			after := liveState(ctx, p, d)

			cp.Close()
			tx.Rollback()
			w.Close()

			wantBusy := mode != sqlitex.CheckpointPassive
			waited := elapsed >= busyMs*time.Millisecond
			summary = append(summary, fmt.Sprintf("%s=%v/%d/%d(%dms,wal=%dB)", mode, res.Busy, res.LogFrames, res.Checkpointed, elapsed.Milliseconds(), after.WALSize))

			// Per wal.c:4357-4366 & 4431: FULL/RESTART/TRUNCATE wait for WAL_WRITE_LOCK,
			// fall back to PASSIVE (so they STILL copy all committedFrames), do NOT truncate,
			// and return busy=1. PASSIVE does not wait and returns busy=0.
			if res.Busy != wantBusy || res.LogFrames != committedFrames || res.Checkpointed != committedFrames {
				problems = append(problems, fmt.Sprintf("%s res=%s want busy=%v log=%d ckpt=%d", mode, res, wantBusy, committedFrames, committedFrames))
			}
			if waited != wantBusy {
				problems = append(problems, fmt.Sprintf("%s waited=%v(elapsed=%dms) want=%v", mode, waited, elapsed.Milliseconds(), wantBusy))
			}
			if after.WALSize != before.WALSize {
				problems = append(problems, fmt.Sprintf("%s changed WAL size %d -> %d under held writer", mode, before.WALSize, after.WALSize))
			}
		}

		report("CHK-01a",
			"writer holds open BEGIN IMMEDIATE write tx (WAL_WRITE_LOCK) while 2nd conn runs PASSIVE/FULL/RESTART/TRUNCATE (busy=150ms)",
			"PASSIVE returns busy=0 immediately with full backfill; FULL/RESTART/TRUNCATE wait >=150ms, fall back to PASSIVE (full backfill, WAL unshrunk), return busy=1",
			fmt.Sprintf("results=%v problems=%v", summary, problems),
			len(problems) == 0)
	}

	// CHK-01b (OUTLINE 5.4 bullet 1, part b): Active concurrent writer goroutine
	// committing 40 transactions while a checkpointer races all 4 modes.
	{
		_, p := cloneTemplate("chk01b-race")
		w := openConn(ctx, p, true, 5*time.Second, 0)
		cp := openConn(ctx, p, true, 50*time.Millisecond, 0)
		if _, err := w.ExecContext(ctx, "CREATE TABLE race_log (id INTEGER PRIMARY KEY, val TEXT)"); err != nil {
			fatal("create race_log", err)
		}

		const totalCommits = 40
		var wg sync.WaitGroup
		var writeErr error
		var cpErr error
		cpCounts := map[string]int{}
		cpBusy := map[string]int{}
		doneWriting := make(chan struct{})

		wg.Add(1)
		go func() {
			defer wg.Done()
			defer close(doneWriting)
			for i := 1; i <= totalCommits; i++ {
				tx, err := w.BeginTx(ctx, nil)
				if err != nil {
					writeErr = fmt.Errorf("begin %d: %w", i, err)
					return
				}
				if _, err := tx.ExecContext(ctx, "INSERT INTO race_log(id, val) VALUES (?, ?)", i, fmt.Sprintf("commit-%03d", i)); err != nil {
					tx.Rollback()
					writeErr = fmt.Errorf("insert %d: %w", i, err)
					return
				}
				if err := tx.Commit(); err != nil {
					writeErr = fmt.Errorf("commit %d: %w", i, err)
					return
				}
				time.Sleep(2 * time.Millisecond)
			}
		}()

		wg.Add(1)
		go func() {
			defer wg.Done()
			modes := []string{
				sqlitex.CheckpointPassive,
				sqlitex.CheckpointFull,
				sqlitex.CheckpointRestart,
				sqlitex.CheckpointTruncate,
			}
			idx := 0
			for {
				select {
				case <-doneWriting:
					return
				default:
				}
				m := modes[idx%len(modes)]
				idx++
				res, err := sqlitex.Checkpoint(ctx, cp, "", m)
				if err != nil {
					cpErr = fmt.Errorf("ckpt %s: %w", m, err)
					return
				}
				cpCounts[m]++
				if res.Busy {
					cpBusy[m]++
				}
			}
		}()
		wg.Wait()

		var rowCount int
		var ic string
		if err := w.QueryRowContext(ctx, "SELECT count(*) FROM race_log").Scan(&rowCount); err != nil {
			fatal("count race_log", err)
		}
		if err := w.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&ic); err != nil {
			fatal("ic race_log", err)
		}
		cp.Close()
		w.Close()

		held := writeErr == nil && cpErr == nil && rowCount == totalCommits && ic == "ok"
		report("CHK-01b",
			fmt.Sprintf("concurrent writer committing %d txs racing PASSIVE/FULL/RESTART/TRUNCATE checkpointer", totalCommits),
			"zero SQL errors, all 40 commits durable, PRAGMA integrity_check=ok",
			fmt.Sprintf("rows=%d/%d ic=%q write_err=%v cp_err=%v runs=%v busy=%v", rowCount, totalCommits, ic, writeErr, cpErr, cpCounts, cpBusy),
			held)
	}

	// CHK-02 (OUTLINE 5.4 bullet 2): A second ("rogue") connection checkpoints
	// while the primary writer (wal_autocheckpoint=0) believes it owns checkpointing.
	{
		r := recipe.Default().WithSteps(10)
		// (a) Rogue connection runs PASSIVE (simulating an app connection with default
		// wal_autocheckpoint hitting threshold); primary's next write silently restarts WAL!
		dA, pA := cloneTemplate("chk02-rogue-passive")
		primaryA := openConn(ctx, pA, true, 5*time.Second, 0)
		for _, step := range []int{6, 7} {
			if err := r.ApplyStep(ctx, primaryA, step); err != nil {
				fatal("step chk02a", err)
			}
		}
		beforeA := liveState(ctx, pA, dA)
		// Rogue connection opens with wal_autocheckpoint=10 (below current 38 frames) and commits 1 tiny write.
		rogueA := openConn(ctx, pA, true, 5*time.Second, 10)
		if _, err := rogueA.ExecContext(ctx, "PRAGMA user_version = 70"); err != nil {
			fatal("rogue write chk02a", err)
		}
		afterRogueA := liveState(ctx, pA, dA)
		rogueA.Close()
		// Primary writer commits step 8, unaware rogue auto-checkpointed!
		if err := r.ApplyStep(ctx, primaryA, 8); err != nil {
			fatal("primary step 8 chk02a", err)
		}
		afterPrimaryA := liveState(ctx, pA, dA)
		primaryA.Close()

		rogueAutoTriggered := afterRogueA.Index.Checkpoint.Backfill == afterRogueA.Index.Header().MaxFrame &&
			afterRogueA.Index.Header().MaxFrame > beforeA.Index.Header().MaxFrame
		primaryRestartedA := afterPrimaryA.Index.Header().MaxFrame < beforeA.Index.Header().MaxFrame
		saltChangedA := afterPrimaryA.WAL.Header.Salt1 != beforeA.WAL.Header.Salt1 ||
			afterPrimaryA.WAL.Header.Salt2 != beforeA.WAL.Header.Salt2

		// (b) Rogue connection runs TRUNCATE behind primary's back.
		dB, pB := cloneTemplate("chk02-rogue-truncate")
		primaryB := openConn(ctx, pB, true, 5*time.Second, 0)
		for _, step := range []int{6, 7} {
			if err := r.ApplyStep(ctx, primaryB, step); err != nil {
				fatal("step chk02b", err)
			}
		}
		beforeB := liveState(ctx, pB, dB)
		rogueB := openConn(ctx, pB, true, 5*time.Second, 0)
		if _, err := sqlitex.Checkpoint(ctx, rogueB, "", sqlitex.CheckpointTruncate); err != nil {
			fatal("rogue truncate", err)
		}
		afterRogueB := liveState(ctx, pB, dB)
		rogueB.Close()
		if err := r.ApplyStep(ctx, primaryB, 8); err != nil {
			fatal("primary step 8 chk02b", err)
		}
		afterPrimaryB := liveState(ctx, pB, dB)
		primaryB.Close()

		primaryRestartedB := afterRogueB.WALSize == 0 && afterPrimaryB.Index.Header().MaxFrame < beforeB.Index.Header().MaxFrame
		saltChangedB := afterPrimaryB.WAL.Header.Salt1 != beforeB.WAL.Header.Salt1 ||
			afterPrimaryB.WAL.Header.Salt2 != beforeB.WAL.Header.Salt2

		held := rogueAutoTriggered && primaryRestartedA && saltChangedA && primaryRestartedB && saltChangedB
		report("CHK-02",
			"rogue 2nd connection (a) triggers autocheckpoint and (b) runs TRUNCATE while primary has wal_autocheckpoint=0",
			"both hijack WAL lifecycle and force primary's next write to restart at frame 1; salt-change check detects both generations",
			fmt.Sprintf("auto_ckpt=%v restartA=%v(mx %d->%d) saltChgA=%v | trunc_wal=%dB restartB=%v(mx %d->%d) saltChgB=%v(seq=%d)",
				rogueAutoTriggered, primaryRestartedA, beforeA.Index.Header().MaxFrame, afterPrimaryA.Index.Header().MaxFrame, saltChangedA,
				afterRogueB.WALSize, primaryRestartedB, beforeB.Index.Header().MaxFrame, afterPrimaryB.Index.Header().MaxFrame, saltChangedB, afterPrimaryB.WAL.Header.CheckpointSequence),
			held)
	}

	// CHK-03 (OUTLINE 5.4 literal claim test): OUTLINE 5.4 Evidence literally states
	// "salt-1 increments on restart". Test this claim when connection C runs TRUNCATE
	// and connection W (whose per-connection pWal->nCkpt is 0) writes the next WAL header.
	{
		r := recipe.Default().WithSteps(10)
		d, p := cloneTemplate("chk03-salt-rule")
		w := openConn(ctx, p, true, 5*time.Second, 0)
		for _, step := range []int{6, 7} {
			if err := r.ApplyStep(ctx, w, step); err != nil {
				fatal("step chk03", err)
			}
		}
		before := liveState(ctx, p, d)

		// Separate checkpointer connection runs TRUNCATE.
		c := openConn(ctx, p, true, 5*time.Second, 0)
		if _, err := sqlitex.Checkpoint(ctx, c, "", sqlitex.CheckpointTruncate); err != nil {
			fatal("truncate chk03", err)
		}
		afterTrunc := liveState(ctx, p, d)
		c.Close()

		// Primary writer W writes the next generation (restarted WAL header).
		if err := r.ApplyStep(ctx, w, 8); err != nil {
			fatal("step 8 chk03", err)
		}
		afterWrite := liveState(ctx, p, d)
		w.Close()

		// Does WAL header salt-1 equal before.Salt1 + 1 as OUTLINE 5.4 states?
		walSalt1Incremented := afterWrite.WAL.Header.Salt1 == before.WAL.Header.Salt1+1
		shmSalt1Incremented := afterTrunc.Index.Header().Salt1 == before.WAL.Header.Salt1+1
		report("CHK-03",
			"connection C runs TRUNCATE then connection W writes restarted WAL header; check OUTLINE 5.4 claim 'salt-1 increments on restart'",
			"restarted WAL header salt-1 == prev_salt1 + 1 (OUTLINE 5.4 literal evidence claim)",
			fmt.Sprintf("before_salt1=0x%08x after_trunc_shm_salt1=0x%08x(+1:%v) restarted_wal_salt1=0x%08x(+1:%v) ckpt_seq=%d->%d",
				before.WAL.Header.Salt1, afterTrunc.Index.Header().Salt1, shmSalt1Incremented,
				afterWrite.WAL.Header.Salt1, walSalt1Incremented, before.WAL.Header.CheckpointSequence, afterWrite.WAL.Header.CheckpointSequence),
			walSalt1Incremented)
	}

	// CHK-04 (wal.c:4335-4345): Two connections overlapping checkpoints contend on
	// WAL_CKPT_LOCK; SQLite must NOT invoke busy_timeout on the second checkpointer
	// and must return busy=1, log=-1, checkpointed=-1 immediately.
	{
		r := recipe.Default().WithSteps(10)
		d, p := cloneTemplate("chk04-ckpt-lock")
		w := openConn(ctx, p, true, 5*time.Second, 0)
		if err := r.ApplyStep(ctx, w, 6); err != nil {
			fatal("step 6 chk04", err)
		}
		// Open reader pinning frame f1 so checkpointer 1's RESTART holds WAL_CKPT_LOCK while waiting.
		rOpts := recipe.Default().SQLiteOptions()
		rOpts.ImmediateTx = false
		rOpts.Conn.WALAutocheckpoint = 0
		rOpts.SingleConnection = true
		rdb, err := sqlitex.Open(ctx, p, rOpts)
		if err != nil {
			fatal("open reader chk04", err)
		}
		rtx, err := rdb.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		if err != nil {
			fatal("begin reader chk04", err)
		}
		var n int
		if err := rtx.QueryRowContext(ctx, "SELECT count(*) FROM events").Scan(&n); err != nil {
			fatal("read chk04", err)
		}
		if err := r.ApplyStep(ctx, w, 7); err != nil {
			fatal("step 7 chk04", err)
		}
		_ = d

		cp1 := openConn(ctx, p, true, 450*time.Millisecond, 0)
		cp2 := openConn(ctx, p, true, 500*time.Millisecond, 0)

		var cp1Res sqlitex.CheckpointResult
		var cp1Err error
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			cp1Res, cp1Err = sqlitex.Checkpoint(ctx, cp1, "", sqlitex.CheckpointRestart)
		}()

		// Give cp1 a brief head start to acquire WAL_CKPT_LOCK and begin waiting on reader.
		time.Sleep(80 * time.Millisecond)

		var problems []string
		var summary []string
		for _, mode := range []string{sqlitex.CheckpointPassive, sqlitex.CheckpointFull, sqlitex.CheckpointTruncate} {
			t0 := time.Now()
			res, err := sqlitex.Checkpoint(ctx, cp2, "", mode)
			elapsed := time.Since(t0)
			summary = append(summary, fmt.Sprintf("%s=%v/%d/%d(%dms)", mode, res.Busy, res.LogFrames, res.Checkpointed, elapsed.Milliseconds()))
			if err != nil {
				problems = append(problems, fmt.Sprintf("%s err=%v", mode, err))
			}
			// Must fail immediately (< 150ms despite 500ms busy_timeout) with busy=1, -1, -1.
			if !res.Busy || res.LogFrames != -1 || res.Checkpointed != -1 || elapsed >= 150*time.Millisecond {
				problems = append(problems, fmt.Sprintf("%s got %s in %dms (want busy=1,-1,-1 in <150ms)", mode, res, elapsed.Milliseconds()))
			}
		}

		wg.Wait()
		rtx.Rollback()
		rdb.Close()
		cp1.Close()
		cp2.Close()
		w.Close()

		held := cp1Err == nil && cp1Res.Busy && len(problems) == 0
		report("CHK-04",
			"2nd checkpointer (busy_timeout=500ms) runs PASSIVE/FULL/TRUNCATE while 1st checkpointer holds WAL_CKPT_LOCK",
			"2nd checkpointer ignores busy_timeout (wal.c:4338) and returns busy=1, log=-1, checkpointed=-1 immediately (<150ms)",
			fmt.Sprintf("cp1=%s(err=%v) cp2=%v problems=%v", cp1Res, cp1Err, summary, problems),
			held)
	}

	// CHK-05: (a) Lock-free NOOP under simultaneous held writer + held checkpointer +
	// reader; (b) last-connection-close auto-checkpoint & -wal/-shm deletion E1;
	// (c) read-only connection PRAGMA wal_checkpoint behavior.
	{
		r := recipe.Default().WithSteps(10)
		d, p := cloneTemplate("chk05-lifecycle")
		w := openConn(ctx, p, true, 5*time.Second, 0)
		if err := r.ApplyStep(ctx, w, 6); err != nil {
			fatal("step 6 chk05", err)
		}
		if err := r.ApplyStep(ctx, w, 7); err != nil {
			fatal("step 7 chk05", err)
		}
		// Copy base + WAL while open so we can later compare last-close E1 against walfile.Replay.
		copyDir := filepath.Join(d, "live-copy")
		if err := os.MkdirAll(copyDir, 0o755); err != nil {
			fatal("mkdir copy chk05", err)
		}
		if _, err := walfile.CopyLiveFiles(ctx, []walfile.CopyPair{
			{From: p, To: filepath.Join(copyDir, "r.db")},
			{From: p + "-wal", To: filepath.Join(copyDir, "r.db-wal")},
		}); err != nil {
			fatal("copy chk05", err)
		}
		replayedPath := filepath.Join(copyDir, "replayed.db")
		if _, err := walfile.ReplayFiles(filepath.Join(copyDir, "r.db"), filepath.Join(copyDir, "r.db-wal"), replayedPath); err != nil {
			fatal("replay chk05", err)
		}
		expectedFrames := int(liveState(ctx, p, d).Index.Header().MaxFrame)

		// Hold reader + active write tx + blocked RESTART checkpointer simultaneously.
		rOpts := recipe.Default().SQLiteOptions()
		rOpts.ImmediateTx = false
		rOpts.Conn.WALAutocheckpoint = 0
		rOpts.SingleConnection = true
		rdb, err := sqlitex.Open(ctx, p, rOpts)
		if err != nil {
			fatal("open reader chk05", err)
		}
		rtx, err := rdb.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		if err != nil {
			fatal("begin reader chk05", err)
		}
		var n int
		if err := rtx.QueryRowContext(ctx, "SELECT count(*) FROM events").Scan(&n); err != nil {
			fatal("read chk05", err)
		}
		wtx, err := w.BeginTx(ctx, nil)
		if err != nil {
			fatal("begin write chk05", err)
		}
		if _, err := wtx.ExecContext(ctx, "UPDATE events SET amount = amount + 1 WHERE id = 1"); err != nil {
			fatal("update chk05", err)
		}
		cpBlock := openConn(ctx, p, true, 350*time.Millisecond, 0)
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = sqlitex.Checkpoint(ctx, cpBlock, "", sqlitex.CheckpointRestart)
		}()
		time.Sleep(60 * time.Millisecond)

		// (a) NOOP on 4th connection must take NO locks and return immediately with accurate log frames.
		noopConn := openConn(ctx, p, false, 500*time.Millisecond, 0)
		t0 := time.Now()
		noopRes, noopErr := sqlitex.Checkpoint(ctx, noopConn, "", sqlitex.CheckpointNoop)
		noopElapsed := time.Since(t0)

		wg.Wait()
		wtx.Rollback()
		rtx.Rollback()
		cpBlock.Close()
		noopConn.Close()

		// (c) Read-only connection calling wal_checkpoint(PASSIVE) while WAL is live.
		roOpts := recipe.Default().SQLiteOptions()
		roOpts.ReadOnly = true
		roOpts.ImmediateTx = false
		roOpts.SingleConnection = true
		roDB, err := sqlitex.Open(ctx, p, roOpts)
		if err != nil {
			fatal("open ro chk05", err)
		}
		roRes, roErr := sqlitex.Checkpoint(ctx, roDB, "", sqlitex.CheckpointPassive)
		roDB.Close()

		// (b) Close reader (1 of 2 remaining connections): -wal must STILL exist.
		rdb.Close()
		_, walAfter1Close := os.Stat(p + "-wal")
		walKeptWithOpenConn := walAfter1Close == nil

		// Close writer (LAST connection): SQLite auto-checkpoints and removes -wal and -shm.
		w.Close()
		_, walAfterLast := os.Stat(p + "-wal")
		_, shmAfterLast := os.Stat(p + "-shm")
		cleanedOnLastClose := errors.Is(walAfterLast, os.ErrNotExist) && errors.Is(shmAfterLast, os.ErrNotExist)
		cmp, err := equal.CompareFiles(replayedPath, p, 4096)
		if err != nil {
			fatal("compare chk05", err)
		}

		// Read-only checkpoint in SQLite returns busy=0, log=-1, checkpointed=-1 (or error), never checkpoints.
		roSafe := roErr != nil || (roRes.LogFrames == -1 && roRes.Checkpointed == -1)
		noopOK := noopErr == nil && !noopRes.Busy && noopRes.LogFrames == expectedFrames && noopElapsed < 100*time.Millisecond

		held := noopOK && walKeptWithOpenConn && cleanedOnLastClose && cmp.Equal && roSafe
		report("CHK-05",
			"lock-free NOOP under simultaneous writer+checkpointer+reader; multi-conn close cleanup vs Replay E1; read-only checkpoint",
			"NOOP returns immediately (<100ms, busy=0, accurate log); -wal persists until last close then auto-checkpoints E1-equal and deletes -wal/-shm; mode=ro cannot checkpoint",
			fmt.Sprintf("noop=%s(%dms,err=%v) wal_kept_1close=%v cleaned_last=%v E1=%v ro_ckpt=%s(err=%v)",
				noopRes, noopElapsed.Milliseconds(), noopErr, walKeptWithOpenConn, cleanedOnLastClose, cmp.Equal, roRes, roErr),
			held)
	}
}
