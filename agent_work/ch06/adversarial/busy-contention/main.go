package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/nickstrad/sqlite-internals/internal/procs"
	"github.com/nickstrad/sqlite-internals/internal/recipe"
	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
)

const (
	longTimeout  = 120 * time.Second
	stepDeadline = 20 * time.Second
	rerunCount   = 100
)

func clean(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "|", "/")
	return strings.Join(strings.Fields(s), " ")
}

func report(id, tried, expect, got string, held bool) {
	status := "HELD"
	if !held {
		status = "BROKEN"
	}
	fmt.Printf("ATTACK %s | %s | EXPECT %s | GOT %s | %s\n", id, clean(tried), clean(expect), clean(got), status)
}

func fatal(what string, err error) {
	fmt.Fprintf(os.Stderr, "FATAL %s: %v\n", what, err)
	os.Exit(1)
}

func mustCreateLabDB(ctx context.Context, path, journal string) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		fatal("mkdir", err)
	}
	if err := procs.CreateDatabase(ctx, path, journal,
		"CREATE TABLE lab(id INTEGER PRIMARY KEY, who TEXT NOT NULL)",
		"INSERT INTO lab(who) VALUES ('seed')",
	); err != nil {
		fatal("create "+path, err)
	}
}

func mustDo(script *procs.Script, c *procs.Child, action string, args ...string) procs.Result {
	r, err := script.Do(c, action, args...)
	if err != nil {
		fatal(c.Name+" "+action, err)
	}
	if !r.OK() {
		fatal(c.Name+" "+action, fmt.Errorf("unexpected %s (%s)", r.Outcome(), r.Error))
	}
	return r
}

func doRes(script *procs.Script, c *procs.Child, action string, args ...string) procs.Result {
	r, err := script.Do(c, action, args...)
	if err != nil {
		fatal(c.Name+" "+action, err)
	}
	return r
}

// attackBusy01a tests three writers + one concurrent reader in WAL mode (4 child processes).
// Two writers queue simultaneously behind writer-1 while reader reads freely; all 3 commit.
func attackBusy01a(ctx context.Context, root string) {
	db := filepath.Join(root, "busy01a", "lab.db")
	mustCreateLabDB(ctx, db, sqlitex.JournalWAL)

	script := procs.NewScript(io.Discard)
	script.Timeout = stepDeadline
	defer script.Close()

	wOpts := procs.SQLOptions{Path: db, JournalMode: sqlitex.JournalWAL, BusyTimeout: longTimeout}
	rOpts := procs.SQLOptions{Path: db, JournalMode: sqlitex.JournalWAL, BusyTimeout: 0}
	w1, err := script.Spawn("writer-1", procs.RoleSQL, wOpts.Args()...)
	if err != nil {
		fatal("busy01a spawn w1", err)
	}
	w2, err := script.Spawn("writer-2", procs.RoleSQL, wOpts.Args()...)
	if err != nil {
		fatal("busy01a spawn w2", err)
	}
	w3, err := script.Spawn("writer-3", procs.RoleSQL, wOpts.Args()...)
	if err != nil {
		fatal("busy01a spawn w3", err)
	}
	rd, err := script.Spawn("reader", procs.RoleSQL, rOpts.Args()...)
	if err != nil {
		fatal("busy01a spawn reader", err)
	}

	mustDo(script, w1, "begin", "immediate")
	mustDo(script, w1, "exec", "INSERT INTO lab(who) VALUES ('w1')")

	c2, err := script.Begin(w2, "begin", "immediate")
	if err != nil {
		fatal("busy01a begin w2", err)
	}
	c3, err := script.Begin(w3, "begin", "immediate")
	if err != nil {
		fatal("busy01a begin w3", err)
	}
	// Give both children time to reach SQLite's busy handler before polling.
	time.Sleep(50 * time.Millisecond)
	ans2, err := c2.Answered()
	if err != nil {
		fatal("busy01a poll w2", err)
	}
	ans3, err := c3.Answered()
	if err != nil {
		fatal("busy01a poll w3", err)
	}

	// Reader must read immediately with timeout=0 while 1 writer holds and 2 writers wait.
	readDuring := doRes(script, rd, "query", "SELECT count(*) FROM lab")

	// Commit w1; wait until either w2 or w3 acquires the write lock, then commit it, then the other.
	mustDo(script, w1, "commit")

	// Poll until one of c2 or c3 answers, then drive it to commit so the other can proceed.
	var firstWinner, secondWinner *procs.Child
	var firstCall, secondCall *procs.Call
	deadline := time.Now().Add(stepDeadline)
	for time.Now().Before(deadline) {
		a2, err := c2.Answered()
		if err != nil {
			fatal("busy01a poll2", err)
		}
		if a2 {
			firstWinner, secondWinner = w2, w3
			firstCall, secondCall = c2, c3
			break
		}
		a3, err := c3.Answered()
		if err != nil {
			fatal("busy01a poll3", err)
		}
		if a3 {
			firstWinner, secondWinner = w3, w2
			firstCall, secondCall = c3, c2
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if firstWinner == nil {
		fatal("busy01a", fmt.Errorf("neither w2 nor w3 acquired lock after w1 commit"))
	}
	rFirst, err := script.Finish(firstCall)
	if err != nil {
		fatal("busy01a finish first", err)
	}
	mustDo(script, firstWinner, "exec", fmt.Sprintf("INSERT INTO lab(who) VALUES ('%s')", firstWinner.Name))
	mustDo(script, firstWinner, "commit")

	rSecond, err := script.Finish(secondCall)
	if err != nil {
		fatal("busy01a finish second", err)
	}
	mustDo(script, secondWinner, "exec", fmt.Sprintf("INSERT INTO lab(who) VALUES ('%s')", secondWinner.Name))
	mustDo(script, secondWinner, "commit")

	if err := script.Close(); err != nil {
		fatal("busy01a close", err)
	}
	insp, err := procs.InspectDatabase(ctx, db, sqlitex.JournalWAL, "SELECT count(*) FROM lab")
	if err != nil {
		fatal("busy01a inspect", err)
	}

	held := !ans2 && !ans3 && readDuring.OK() && readDuring.Value == "1" && rFirst.OK() && rSecond.OK() && insp.Integrity == "ok" && insp.Values[0] == "4"
	report(
		"BUSY-01a",
		"3 WAL writers (2 queued concurrently) + 1 zero-timeout reader across 4 processes",
		"both queued writers wait, reader reads 1 row immediately, all 3 writers commit cleanly (4 total rows, integrity ok)",
		fmt.Sprintf("w2_waited=%v w3_waited=%v reader=%s first=%s(%s) second=%s(%s) integrity=%s rows=%s", !ans2, !ans3, readDuring.Outcome(), firstWinner.Name, rFirst.Outcome(), secondWinner.Name, rSecond.Outcome(), insp.Integrity, insp.Values[0]),
		held,
	)
}

// attackBusy01b runs 3 writers applying recipe steps 6..17 under scripted contention.
func attackBusy01b(ctx context.Context, root string) {
	r := recipe.Default()
	db := filepath.Join(root, "busy01b", "r.db")
	if err := os.MkdirAll(filepath.Dir(db), 0o755); err != nil {
		fatal("busy01b mkdir", err)
	}
	if err := recipe.RemoveDatabase(db); err != nil {
		fatal("busy01b remove", err)
	}
	if err := r.Build(ctx, db, 5); err != nil {
		fatal("busy01b build", err)
	}

	script := procs.NewScript(io.Discard)
	script.Timeout = stepDeadline
	defer script.Close()
	opts := procs.SQLOptions{Path: db, JournalMode: sqlitex.JournalWAL, BusyTimeout: longTimeout}
	writers := make([]*procs.Child, 3)
	for i := range writers {
		c, err := script.Spawn(fmt.Sprintf("writer-%d", i+1), procs.RoleSQL, opts.Args()...)
		if err != nil {
			fatal("busy01b spawn", err)
		}
		writers[i] = c
	}

	allWaited, allOK := true, true
	// Apply steps 6..17 (12 steps) in pairs where next writer queues while current holds.
	for k := 6; k <= 17; k += 2 {
		holder := writers[(k/2)%3]
		waiter := writers[(k/2+1)%3]
		mustDo(script, holder, "begin", "immediate")
		mustDo(script, holder, "recipe-exec", strconv.Itoa(k))
		call, err := script.Begin(waiter, "recipe-step", strconv.Itoa(k+1))
		if err != nil {
			fatal("busy01b begin waiter", err)
		}
		ans, err := call.Answered()
		if err != nil {
			fatal("busy01b poll", err)
		}
		if ans {
			allWaited = false
		}
		if !doRes(script, holder, "commit").OK() {
			allOK = false
		}
		res, err := script.Finish(call)
		if err != nil {
			fatal("busy01b finish", err)
		}
		if !res.OK() {
			allOK = false
		}
	}
	if err := script.Close(); err != nil {
		fatal("busy01b close", err)
	}
	match, err := r.MatchStep(ctx, db)
	if err != nil {
		fatal("busy01b match", err)
	}
	held := allWaited && allOK && match.Found() && match.Step == 17
	report(
		"BUSY-01b",
		"3 WAL writers applying recipe steps 6-17 under scripted contention (BEGIN IMMEDIATE + 120s timeout)",
		"all waiting steps block until holder commits, all 12 steps succeed, closed DB matches recipe step 17",
		fmt.Sprintf("all_waited=%v all_ok=%v match=%s", allWaited, allOK, match.String()),
		held,
	)
}

// attackBusy02 tests 3 processes in rollback mode: reader-1 holds SHARED, writer hits
// BUSY at COMMIT and retains PENDING, reader-2 is blocked by PENDING while reader-1
// can still query inside its open transaction.
func attackBusy02(ctx context.Context, root string) {
	db := filepath.Join(root, "busy02", "lab.db")
	mustCreateLabDB(ctx, db, sqlitex.JournalDelete)

	script := procs.NewScript(io.Discard)
	script.Timeout = stepDeadline
	defer script.Close()
	opts := procs.SQLOptions{Path: db, JournalMode: sqlitex.JournalDelete, BusyTimeout: 0}
	r1, err := script.Spawn("reader-1", procs.RoleSQL, opts.Args()...)
	if err != nil {
		fatal("busy02 spawn r1", err)
	}
	r2, err := script.Spawn("reader-2", procs.RoleSQL, opts.Args()...)
	if err != nil {
		fatal("busy02 spawn r2", err)
	}
	w, err := script.Spawn("writer", procs.RoleSQL, opts.Args()...)
	if err != nil {
		fatal("busy02 spawn w", err)
	}

	mustDo(script, r1, "begin", "deferred")
	mustDo(script, r1, "query", "SELECT count(*) FROM lab")

	mustDo(script, w, "begin", "immediate")
	mustDo(script, w, "exec", "INSERT INTO lab(who) VALUES ('w')")
	wCommit1 := doRes(script, w, "commit") // fails BUSY (5), retains PENDING

	// reader-2 (a brand-new 3rd process) tries to read -> must fail BUSY (5) due to PENDING.
	r2During := doRes(script, r2, "query", "SELECT count(*) FROM lab")
	// reader-1 ALREADY holds SHARED -> must still be able to read inside its txn!
	r1Still := doRes(script, r1, "query", "SELECT count(*) FROM lab")

	mustDo(script, r1, "commit")
	wCommit2 := doRes(script, w, "commit")
	r2After := doRes(script, r2, "query", "SELECT count(*) FROM lab")

	if err := script.Close(); err != nil {
		fatal("busy02 close", err)
	}
	held := wCommit1.Code == sqlitex.CodeBusy &&
		r2During.Code == sqlitex.CodeBusy &&
		r1Still.OK() && r1Still.Value == "1" &&
		wCommit2.OK() &&
		r2After.OK() && r2After.Value == "2"
	report(
		"BUSY-02",
		"3 processes in rollback mode: reader-1 holds SHARED, writer fails COMMIT (PENDING), reader-2 attempts read",
		"writer COMMIT->BUSY(5), reader-2 blocked BUSY(5), existing reader-1 still reads 1, after reader-1 commits writer succeeds and reader-2 sees 2",
		fmt.Sprintf("w_commit1=%s r2_during=%s r1_still=%s w_commit2=%s r2_after=%s", wCommit1.Outcome(), r2During.Outcome(), r1Still.Outcome(), wCommit2.Outcome(), r2After.Outcome()),
		held,
	)
}

// attackBusy03a tests the C sqlite3 CLI as a contending writer in WAL mode (timeout 0 and 30s).
func attackBusy03a(ctx context.Context, cli sqlitex.CLI, root string) {
	db := filepath.Join(root, "busy03a", "lab.db")
	mustCreateLabDB(ctx, db, sqlitex.JournalWAL)

	script := procs.NewScript(io.Discard)
	script.Timeout = stepDeadline
	defer script.Close()
	goW, err := script.Spawn("go-writer", procs.RoleSQL, procs.SQLOptions{Path: db, JournalMode: sqlitex.JournalWAL}.Args()...)
	if err != nil {
		fatal("busy03a spawn", err)
	}
	mustDo(script, goW, "begin", "immediate")
	mustDo(script, goW, "exec", "INSERT INTO lab(who) VALUES ('go')")

	// 1. CLI with timeout 0 must fail immediately with database is locked.
	res0, err0 := cli.Run(ctx, sqlitex.CLIRun{
		Stdin: strings.NewReader(sqlitex.OpenScript(db, "PRAGMA busy_timeout=0;\nBEGIN IMMEDIATE;\nINSERT INTO lab(who) VALUES ('cli0');\nCOMMIT;\n")),
	})
	cli0Locked := err0 != nil && strings.Contains(res0.Stderr, "database is locked")

	// 2. CLI with .timeout 30000 starts async, waits while Go holds lock, succeeds after Go commits.
	cliCmd := cli.Command(ctx, sqlitex.CLIRun{
		Stdin: strings.NewReader(sqlitex.OpenScript(db, ".timeout 30000\nBEGIN IMMEDIATE;\nINSERT INTO lab(who) VALUES ('cli-waited');\nCOMMIT;\n")),
	})
	var outBuf, errBuf bytes.Buffer
	cliCmd.Stdout = &outBuf
	cliCmd.Stderr = &errBuf
	if err := cliCmd.Start(); err != nil {
		fatal("busy03a start cli", err)
	}
	cliDone := make(chan error, 1)
	go func() { cliDone <- cliCmd.Wait() }()

	// Verify CLI is still waiting after 120ms while Go holds the write lock.
	stillWaiting := false
	select {
	case err := <-cliDone:
		fatal("busy03a cli exited early", fmt.Errorf("%v stderr=%q", err, errBuf.String()))
	case <-time.After(120 * time.Millisecond):
		stillWaiting = true
	}

	mustDo(script, goW, "commit")
	var cliWaitErr error
	select {
	case cliWaitErr = <-cliDone:
	case <-time.After(stepDeadline):
		cliCmd.Process.Kill()
		fatal("busy03a cli timeout", fmt.Errorf("stderr=%q", errBuf.String()))
	}

	if err := script.Close(); err != nil {
		fatal("busy03a close", err)
	}
	insp, err := procs.InspectDatabase(ctx, db, sqlitex.JournalWAL, "SELECT count(*) FROM lab")
	if err != nil {
		fatal("busy03a inspect", err)
	}

	held := cli0Locked && stillWaiting && cliWaitErr == nil && insp.Integrity == "ok" && insp.Values[0] == "3"
	report(
		"BUSY-03a",
		"C sqlite3 CLI as writer vs Go writer in WAL mode (busy_timeout=0 then .timeout 30000)",
		"CLI timeout=0 fails locked; CLI .timeout 30000 waits while Go holds lock and succeeds after Go commits (3 rows)",
		fmt.Sprintf("cli0_locked=%v cli_waited=%v cli_wait_ok=%v integrity=%s rows=%s", cli0Locked, stillWaiting, cliWaitErr == nil, insp.Integrity, insp.Values[0]),
		held,
	)
}

// attackBusy03b tests C sqlite3 CLI hitting SQLITE_BUSY_SNAPSHOT despite .timeout 60000.
func attackBusy03b(ctx context.Context, cli sqlitex.CLI, root string) {
	db := filepath.Join(root, "busy03b", "lab.db")
	mustCreateLabDB(ctx, db, sqlitex.JournalWAL)

	script := procs.NewScript(io.Discard)
	script.Timeout = stepDeadline
	defer script.Close()
	goW, err := script.Spawn("go-writer", procs.RoleSQL, procs.SQLOptions{Path: db, JournalMode: sqlitex.JournalWAL}.Args()...)
	if err != nil {
		fatal("busy03b spawn", err)
	}

	// Start interactive CLI (without -bail so it survives the BUSY_SNAPSHOT error).
	cmd := exec.CommandContext(ctx, cli.Path, "-batch", "-init", os.DevNull)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		fatal("busy03b stdin", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		fatal("busy03b stdout", err)
	}
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf
	if err := cmd.Start(); err != nil {
		fatal("busy03b start cli", err)
	}
	rd := bufio.NewReader(stdout)
	waitMarker := func(marker string) error {
		done := make(chan error, 1)
		go func() {
			for {
				line, err := rd.ReadString('\n')
				if err != nil {
					done <- err
					return
				}
				if strings.TrimSpace(line) == marker {
					done <- nil
					return
				}
			}
		}()
		select {
		case err := <-done:
			return err
		case <-time.After(10 * time.Second):
			return fmt.Errorf("timeout waiting for %s", marker)
		}
	}

	if _, err := io.WriteString(stdin, sqlitex.OpenScript(db, ".timeout 60000\nBEGIN DEFERRED;\nSELECT count(*) FROM lab;\nSELECT 'CLI_READ_OK';\n")); err != nil {
		cmd.Process.Kill()
		fatal("busy03b cli init", err)
	}
	if err := waitMarker("CLI_READ_OK"); err != nil {
		cmd.Process.Kill()
		fatal("busy03b cli read", err)
	}

	// Go writer commits a new row while CLI holds its deferred read snapshot.
	mustDo(script, goW, "begin", "immediate")
	mustDo(script, goW, "exec", "INSERT INTO lab(who) VALUES ('go')")
	mustDo(script, goW, "commit")

	// CLI tries to upgrade to write; must fail IMMEDIATELY despite .timeout 60000.
	t0 := time.Now()
	if _, err := io.WriteString(stdin, "INSERT INTO lab(who) VALUES ('cli-stale');\nSELECT 'CLI_AFTER_STALE';\n"); err != nil {
		cmd.Process.Kill()
		fatal("busy03b cli stale", err)
	}
	if err := waitMarker("CLI_AFTER_STALE"); err != nil {
		cmd.Process.Kill()
		fatal("busy03b cli wait stale", err)
	}
	elapsed := time.Since(t0)

	// Restart transaction in CLI and retry the insert -> must succeed.
	if _, err := io.WriteString(stdin, "ROLLBACK;\nBEGIN IMMEDIATE;\nINSERT INTO lab(who) VALUES ('cli-retry');\nCOMMIT;\nSELECT 'CLI_RETRY_OK';\n.quit\n"); err != nil {
		cmd.Process.Kill()
		fatal("busy03b cli retry", err)
	}
	if err := waitMarker("CLI_RETRY_OK"); err != nil {
		cmd.Process.Kill()
		fatal("busy03b cli wait retry", err)
	}
	stdin.Close()
	_ = cmd.Wait()
	if err := script.Close(); err != nil {
		fatal("busy03b close", err)
	}

	insp, err := procs.InspectDatabase(ctx, db, sqlitex.JournalWAL, "SELECT count(*) FROM lab")
	if err != nil {
		fatal("busy03b inspect", err)
	}
	gotLockedErr := strings.Contains(stderrBuf.String(), "database is locked")
	fastFail := elapsed < 5*time.Second
	held := gotLockedErr && fastFail && insp.Integrity == "ok" && insp.Values[0] == "3"
	report(
		"BUSY-03b",
		"C sqlite3 CLI deferred read-then-write upgrade after Go commit with .timeout 60000",
		"CLI fails immediately (<5s, not 60s) with database is locked (BUSY_SNAPSHOT), succeeds after ROLLBACK+BEGIN IMMEDIATE (3 rows)",
		fmt.Sprintf("locked_err=%v elapsed=%dms integrity=%s rows=%s", gotLockedErr, elapsed.Milliseconds(), insp.Integrity, insp.Values[0]),
		held,
	)
}

// attackBusy04 re-runs EVERY cell of Experiment 6.2 100 times to confirm zero timing dependence.
func attackBusy04(ctx context.Context, root string) {
	// Cell A: WAL reader + writer (timeout 0)
	cellAPass := 0
	{
		db := filepath.Join(root, "busy04", "cell-a.db")
		mustCreateLabDB(ctx, db, sqlitex.JournalWAL)
		script := procs.NewScript(io.Discard)
		script.Timeout = stepDeadline
		opts := procs.SQLOptions{Path: db, JournalMode: sqlitex.JournalWAL, BusyTimeout: 0}
		rd, _ := script.Spawn("reader", procs.RoleSQL, opts.Args()...)
		wr, _ := script.Spawn("writer", procs.RoleSQL, opts.Args()...)
		for i := 0; i < rerunCount; i++ {
			b1 := doRes(script, rd, "begin", "deferred")
			s1 := doRes(script, rd, "query", "SELECT count(*) FROM lab")
			w1 := doRes(script, wr, "begin", "immediate")
			w2 := doRes(script, wr, "exec", "INSERT INTO lab(who) VALUES ('a')")
			w3 := doRes(script, wr, "commit")
			s2 := doRes(script, rd, "query", "SELECT count(*) FROM lab")
			c1 := doRes(script, rd, "commit")
			if b1.OK() && s1.OK() && w1.OK() && w2.OK() && w3.OK() && s2.OK() && s2.Value == s1.Value && c1.OK() {
				cellAPass++
			}
		}
		_ = script.Close()
	}

	// Cell B: WAL two writers, timeout 0 (DEFERRED read+write -> BUSY 5; IMMEDIATE -> BUSY 5)
	cellBPass := 0
	{
		db := filepath.Join(root, "busy04", "cell-b.db")
		mustCreateLabDB(ctx, db, sqlitex.JournalWAL)
		script := procs.NewScript(io.Discard)
		script.Timeout = stepDeadline
		opts := procs.SQLOptions{Path: db, JournalMode: sqlitex.JournalWAL, BusyTimeout: 0}
		w1, _ := script.Spawn("w1", procs.RoleSQL, opts.Args()...)
		w2, _ := script.Spawn("w2", procs.RoleSQL, opts.Args()...)
		for i := 0; i < rerunCount; i++ {
			mustDo(script, w1, "begin", "immediate")
			mustDo(script, w1, "exec", "INSERT INTO lab(who) VALUES ('b')")
			dbeg := doRes(script, w2, "begin", "deferred")
			dread := doRes(script, w2, "query", "SELECT count(*) FROM lab")
			dwrite := doRes(script, w2, "exec", "INSERT INTO lab(who) VALUES ('b2')")
			mustDo(script, w2, "rollback")
			imm := doRes(script, w2, "begin", "immediate")
			mustDo(script, w1, "commit")
			if dbeg.OK() && dread.OK() && dwrite.Code == sqlitex.CodeBusy && imm.Code == sqlitex.CodeBusy {
				cellBPass++
			}
		}
		_ = script.Close()
	}

	// Cells C1, C2, C3: WAL two writers, timeout 120s
	cellC1Pass, cellC2Pass, cellC3Pass := 0, 0, 0
	{
		db := filepath.Join(root, "busy04", "cell-c.db")
		mustCreateLabDB(ctx, db, sqlitex.JournalWAL)
		script := procs.NewScript(io.Discard)
		script.Timeout = stepDeadline
		opts := procs.SQLOptions{Path: db, JournalMode: sqlitex.JournalWAL, BusyTimeout: longTimeout}
		w1, _ := script.Spawn("w1", procs.RoleSQL, opts.Args()...)
		w2, _ := script.Spawn("w2", procs.RoleSQL, opts.Args()...)
		for i := 0; i < rerunCount; i++ {
			// C1: BEGIN IMMEDIATE waits and succeeds
			mustDo(script, w1, "begin", "immediate")
			mustDo(script, w1, "exec", "INSERT INTO lab(who) VALUES ('c1')")
			call1, err := script.Begin(w2, "begin", "immediate")
			if err != nil {
				fatal("busy04 c1", err)
			}
			ans1, _ := call1.Answered()
			mustDo(script, w1, "commit")
			r1, err := script.Finish(call1)
			if err != nil {
				fatal("busy04 c1 finish", err)
			}
			mustDo(script, w2, "exec", "INSERT INTO lab(who) VALUES ('c1b')")
			mustDo(script, w2, "commit")
			if !ans1 && r1.OK() {
				cellC1Pass++
			}

			// C2: BEGIN DEFERRED with no read waits at first write
			mustDo(script, w1, "begin", "immediate")
			mustDo(script, w1, "exec", "INSERT INTO lab(who) VALUES ('c2')")
			mustDo(script, w2, "begin", "deferred")
			call2, err := script.Begin(w2, "exec", "INSERT INTO lab(who) VALUES ('c2b')")
			if err != nil {
				fatal("busy04 c2", err)
			}
			ans2, _ := call2.Answered()
			mustDo(script, w1, "commit")
			r2, err := script.Finish(call2)
			if err != nil {
				fatal("busy04 c2 finish", err)
			}
			mustDo(script, w2, "commit")
			if !ans2 && r2.OK() {
				cellC2Pass++
			}

			// C3: BEGIN DEFERRED + read -> BUSY (5) while held, BUSY_SNAPSHOT (517) after commit
			mustDo(script, w2, "begin", "deferred")
			mustDo(script, w2, "query", "SELECT count(*) FROM lab")
			mustDo(script, w1, "begin", "immediate")
			mustDo(script, w1, "exec", "INSERT INTO lab(who) VALUES ('c3')")
			upg := doRes(script, w2, "exec", "INSERT INTO lab(who) VALUES ('c3b')")
			mustDo(script, w1, "commit")
			snap := doRes(script, w2, "exec", "INSERT INTO lab(who) VALUES ('c3b')")
			mustDo(script, w2, "rollback")
			if upg.Code == sqlitex.CodeBusy && snap.Code == sqlitex.CodeBusySnapshot {
				cellC3Pass++
			}
		}
		_ = script.Close()
	}

	// Cell D: rollback mode reader blocks writer COMMIT, writer keeps PENDING blocking new read
	cellDPass := 0
	{
		db := filepath.Join(root, "busy04", "cell-d.db")
		mustCreateLabDB(ctx, db, sqlitex.JournalDelete)
		script := procs.NewScript(io.Discard)
		script.Timeout = stepDeadline
		opts := procs.SQLOptions{Path: db, JournalMode: sqlitex.JournalDelete, BusyTimeout: 0}
		rd, _ := script.Spawn("reader", procs.RoleSQL, opts.Args()...)
		wr, _ := script.Spawn("writer", procs.RoleSQL, opts.Args()...)
		for i := 0; i < rerunCount; i++ {
			mustDo(script, rd, "begin", "deferred")
			mustDo(script, rd, "query", "SELECT count(*) FROM lab")
			mustDo(script, wr, "begin", "immediate")
			mustDo(script, wr, "exec", "INSERT INTO lab(who) VALUES ('d')")
			cFail := doRes(script, wr, "commit")
			mustDo(script, rd, "commit")
			rBlocked := doRes(script, rd, "query", "SELECT count(*) FROM lab")
			cOK := doRes(script, wr, "commit")
			if cFail.Code == sqlitex.CodeBusy && rBlocked.Code == sqlitex.CodeBusy && cOK.OK() {
				cellDPass++
			}
		}
		_ = script.Close()
	}

	held := cellAPass == rerunCount && cellBPass == rerunCount &&
		cellC1Pass == rerunCount && cellC2Pass == rerunCount &&
		cellC3Pass == rerunCount && cellDPass == rerunCount
	report(
		"BUSY-04",
		fmt.Sprintf("%d re-runs of every 6.2 matrix cell (A, B, C1, C2, C3, D)", rerunCount),
		fmt.Sprintf("%d/%d exact extended-code matches in every cell (zero timing dependence)", rerunCount, rerunCount),
		fmt.Sprintf("A=%d/%d B=%d/%d C1=%d/%d C2=%d/%d C3=%d/%d D=%d/%d",
			cellAPass, rerunCount, cellBPass, rerunCount, cellC1Pass, rerunCount, cellC2Pass, rerunCount, cellC3Pass, rerunCount, cellDPass, rerunCount),
		held,
	)
}

// attackBusy05a probes kernel locks under BEGIN EXCLUSIVE in WAL mode (locking_mode=NORMAL).
func attackBusy05a(ctx context.Context, root string) {
	db := filepath.Join(root, "busy05a", "lab.db")
	mustCreateLabDB(ctx, db, sqlitex.JournalWAL)

	script := procs.NewScript(io.Discard)
	script.Timeout = stepDeadline
	defer script.Close()
	opts := procs.SQLOptions{Path: db, JournalMode: sqlitex.JournalWAL, BusyTimeout: 0}
	w, err := script.Spawn("writer", procs.RoleSQL, opts.Args()...)
	if err != nil {
		fatal("busy05a spawn w", err)
	}
	r, err := script.Spawn("reader", procs.RoleSQL, opts.Args()...)
	if err != nil {
		fatal("busy05a spawn r", err)
	}

	mustDo(script, w, "begin", "exclusive")
	mustDo(script, w, "exec", "INSERT INTO lab(who) VALUES ('excl')")

	shmStates, err := procs.ProbeRanges(db+"-shm", procs.ShmLockRanges())
	if err != nil {
		fatal("busy05a probe shm", err)
	}
	dbStates, err := procs.ProbeRanges(db, procs.DatabaseLockRanges())
	if err != nil {
		fatal("busy05a probe db", err)
	}
	readRes := doRes(script, r, "query", "SELECT count(*) FROM lab")
	mustDo(script, w, "commit")
	_ = script.Close()

	shmWrite := procs.WriteLockedBy(shmStates, "WAL_WRITE_LOCK", w.Pid)
	dbSharedOnly := procs.ReadLocked(dbStates, "SHARED") && procs.Unlocked(dbStates, "PENDING", "RESERVED")
	held := shmWrite && dbSharedOnly && readRes.OK() && readRes.Value == "1"
	report(
		"BUSY-05a",
		"probe kernel locks during BEGIN EXCLUSIVE in WAL mode (locking_mode=NORMAL)",
		"behaves like BEGIN IMMEDIATE: WAL_WRITE_LOCK held on -shm, DB holds only SHARED (no PENDING/RESERVED), reader reads 1 row OK",
		fmt.Sprintf("WAL_WRITE_LOCK=%v db_shared_only=%v reader=%s", shmWrite, dbSharedOnly, readRes.Outcome()),
		held,
	)
}

// attackBusy05b tests PRAGMA locking_mode=EXCLUSIVE in WAL mode (pager.c:5982-5988),
// checking whether a WAL writer with locking_mode=EXCLUSIVE DOES block readers in another process.
func attackBusy05b(ctx context.Context, root string) {
	db := filepath.Join(root, "busy05b", "lab.db")
	mustCreateLabDB(ctx, db, sqlitex.JournalWAL)

	script := procs.NewScript(io.Discard)
	script.Timeout = stepDeadline
	defer script.Close()
	opts := procs.SQLOptions{Path: db, JournalMode: sqlitex.JournalWAL, BusyTimeout: 0}
	w, err := script.Spawn("writer", procs.RoleSQL, opts.Args()...)
	if err != nil {
		fatal("busy05b spawn w", err)
	}
	// Set locking_mode=EXCLUSIVE before starting the second process so writer can take exclusive.
	mode := mustDo(script, w, "query", "PRAGMA locking_mode=EXCLUSIVE")
	mustDo(script, w, "begin", "immediate")
	mustDo(script, w, "exec", "INSERT INTO lab(who) VALUES ('excl-mode')")
	mustDo(script, w, "commit")

	// Narrowed claim (build-notes #6, pager.c:5982-5988): "writer never blocks readers"
	// applies to default locking_mode=NORMAL. Under PRAGMA locking_mode=EXCLUSIVE, a WAL
	// writer takes and keeps the database EXCLUSIVE lock and blocks a second process's reader
	// with SQLITE_BUSY (either during open or on its query).
	r, spawnErr := script.Spawn("reader", procs.RoleSQL, opts.Args()...)
	var gotStr string
	var readerBlocked bool
	if spawnErr != nil {
		errText := spawnErr.Error()
		readerBlocked = strings.Contains(errText, "database is locked") || strings.Contains(errText, "SQLITE_BUSY")
		gotStr = fmt.Sprintf("locking_mode=%s reader_blocked_at_open=%v err=%q", mode.Value, readerBlocked, errText)
	} else {
		readRes := doRes(script, r, "query", "SELECT count(*) FROM lab")
		readerBlocked = readRes.Code == sqlitex.CodeBusy
		gotStr = fmt.Sprintf("locking_mode=%s reader=%s", mode.Value, readRes.Outcome())
	}
	_ = script.Close()

	held := mode.Value == "exclusive" && readerBlocked
	report(
		"BUSY-05b",
		"WAL writer sets PRAGMA locking_mode=EXCLUSIVE and commits; 2nd process attempts read",
		"build-notes #6 boundary (pager.c:5982-5988): WAL locking_mode=EXCLUSIVE holds DB EXCLUSIVE lock and blocks 2nd-process reader with SQLITE_BUSY (5)",
		gotStr,
		held,
	)
}

// attackBusy06 tests deferred read-then-write upgrade in ROLLBACK mode with busy_timeout=120s.
func attackBusy06(ctx context.Context, root string) {
	db := filepath.Join(root, "busy06", "lab.db")
	mustCreateLabDB(ctx, db, sqlitex.JournalDelete)

	script := procs.NewScript(io.Discard)
	script.Timeout = stepDeadline
	defer script.Close()
	opts := procs.SQLOptions{Path: db, JournalMode: sqlitex.JournalDelete, BusyTimeout: longTimeout}
	r, err := script.Spawn("reader", procs.RoleSQL, opts.Args()...)
	if err != nil {
		fatal("busy06 spawn r", err)
	}
	w, err := script.Spawn("writer", procs.RoleSQL, opts.Args()...)
	if err != nil {
		fatal("busy06 spawn w", err)
	}

	mustDo(script, r, "begin", "deferred")
	mustDo(script, r, "query", "SELECT count(*) FROM lab") // holds SHARED
	mustDo(script, w, "begin", "immediate")
	mustDo(script, w, "exec", "INSERT INTO lab(who) VALUES ('w')") // holds RESERVED

	t0 := time.Now()
	upgrade := doRes(script, r, "exec", "INSERT INTO lab(who) VALUES ('r-upgrade')")
	elapsed := time.Since(t0)

	mustDo(script, r, "rollback")
	mustDo(script, w, "commit")
	_ = script.Close()

	held := upgrade.Code == sqlitex.CodeBusy && elapsed < 5*time.Second
	report(
		"BUSY-06",
		"rollback mode deferred read-then-write upgrade while another writer holds RESERVED (busy_timeout=120000ms)",
		"returns SQLITE_BUSY (5) immediately (<5s, skipping busy handler per btree.c:3740-3741)",
		fmt.Sprintf("upgrade=%s elapsed=%dms", upgrade.Outcome(), elapsed.Milliseconds()),
		held,
	)
}

// attackBusy07 addresses build-notes' observation that immediate StillWaiting cannot
// prove the child actually entered SQLite's busy-sleep loop:
// 07a holds for 150ms before polling Answered(); 07b uses busy_timeout=100ms and proves expiry.
func attackBusy07(ctx context.Context, root string) {
	db := filepath.Join(root, "busy07", "lab.db")
	mustCreateLabDB(ctx, db, sqlitex.JournalWAL)

	script := procs.NewScript(io.Discard)
	script.Timeout = stepDeadline
	defer script.Close()
	w1, err := script.Spawn("w1", procs.RoleSQL, procs.SQLOptions{Path: db, JournalMode: sqlitex.JournalWAL, BusyTimeout: longTimeout}.Args()...)
	if err != nil {
		fatal("busy07 spawn w1", err)
	}
	w2, err := script.Spawn("w2", procs.RoleSQL, procs.SQLOptions{Path: db, JournalMode: sqlitex.JournalWAL, BusyTimeout: longTimeout}.Args()...)
	if err != nil {
		fatal("busy07 spawn w2", err)
	}

	// 07a: wait 150ms before polling Answered(), then commit w1 and verify w2 finishes OK.
	mustDo(script, w1, "begin", "immediate")
	mustDo(script, w1, "exec", "INSERT INTO lab(who) VALUES ('hold')")
	callA, err := script.Begin(w2, "begin", "immediate")
	if err != nil {
		fatal("busy07a begin", err)
	}
	time.Sleep(150 * time.Millisecond)
	ansAfter150, err := callA.Answered()
	if err != nil {
		fatal("busy07a poll", err)
	}
	mustDo(script, w1, "commit")
	resA, err := script.Finish(callA)
	if err != nil {
		fatal("busy07a finish", err)
	}
	mustDo(script, w2, "commit")

	// 07b: set w2 busy_timeout=100ms while w1 holds the lock; verify w2 sleeps ~100ms then returns BUSY (5).
	mustDo(script, w2, "timeout", "100")
	mustDo(script, w1, "begin", "immediate")
	mustDo(script, w1, "exec", "INSERT INTO lab(who) VALUES ('hold2')")
	t0 := time.Now()
	resB := doRes(script, w2, "begin", "immediate")
	elapsedB := time.Since(t0)
	mustDo(script, w1, "commit")
	_ = script.Close()

	heldA := !ansAfter150 && resA.OK()
	heldB := resB.Code == sqlitex.CodeBusy && elapsedB >= 80*time.Millisecond && elapsedB < 2*time.Second
	report(
		"BUSY-07",
		"prove SQLite busy-wait loop runs (150ms hold poll) and expires on schedule (busy_timeout=100ms)",
		"still unanswered after 150ms then SQLITE_OK on release; with 100ms timeout returns SQLITE_BUSY (5) after >=80ms and <2s",
		fmt.Sprintf("unanswered_at_150ms=%v after_release=%s timeout100ms=%s elapsed=%dms", !ansAfter150, resA.Outcome(), resB.Outcome(), elapsedB.Milliseconds()),
		heldA && heldB,
	)
}

// attackBusy08 is the negative control for SQLITE_BUSY_SNAPSHOT (517):
// if writer-1 acquires WAL_WRITE_LOCK and ROLLBACKs without committing,
// writer-2's deferred read transaction CAN upgrade to write and commit with SQLITE_OK.
func attackBusy08(ctx context.Context, root string) {
	db := filepath.Join(root, "busy08", "lab.db")
	mustCreateLabDB(ctx, db, sqlitex.JournalWAL)

	script := procs.NewScript(io.Discard)
	script.Timeout = stepDeadline
	defer script.Close()
	opts := procs.SQLOptions{Path: db, JournalMode: sqlitex.JournalWAL, BusyTimeout: longTimeout}
	w1, err := script.Spawn("w1", procs.RoleSQL, opts.Args()...)
	if err != nil {
		fatal("busy08 spawn w1", err)
	}
	w2, err := script.Spawn("w2", procs.RoleSQL, opts.Args()...)
	if err != nil {
		fatal("busy08 spawn w2", err)
	}

	mustDo(script, w2, "begin", "deferred")
	mustDo(script, w2, "query", "SELECT count(*) FROM lab")

	mustDo(script, w1, "begin", "immediate")
	mustDo(script, w1, "exec", "INSERT INTO lab(who) VALUES ('aborted')")
	mustDo(script, w1, "rollback")

	// Because w1 rolled back, the shared wal-index header did not advance -> w2 upgrade must succeed!
	upg := doRes(script, w2, "exec", "INSERT INTO lab(who) VALUES ('w2-ok')")
	com := doRes(script, w2, "commit")
	_ = script.Close()

	insp, err := procs.InspectDatabase(ctx, db, sqlitex.JournalWAL, "SELECT group_concat(who, ',') FROM (SELECT who FROM lab ORDER BY id)")
	if err != nil {
		fatal("busy08 inspect", err)
	}
	held := upg.OK() && com.OK() && insp.Integrity == "ok" && insp.Values[0] == "seed,w2-ok"
	report(
		"BUSY-08",
		"negative control for BUSY_SNAPSHOT: writer-1 takes write lock and ROLLBACKs; deferred reader upgrades to write",
		"upgrade and COMMIT return SQLITE_OK (517 triggers only on a committed header change), rows=seed,w2-ok",
		fmt.Sprintf("upgrade=%s commit=%s integrity=%s rows=%s", upg.Outcome(), com.Outcome(), insp.Integrity, insp.Values[0]),
		held,
	)
}

// attackBusy09 tests two claims behind 6.2's StillWaiting / Part E contention check:
//  1. Does Lab.StillWaiting distinguish a blocked waiter from an immediate busy_timeout=0
//     failure (i.e. return false when the child immediately returns SQLITE_BUSY)?
//  2. Under strace (Linux), does 6.2-busy-rules Part E actually incur EAGAIN on -shm byte 120
//     for all 5 waiting rounds (steps 7, 9, 11, 13, 15)?
func attackBusy09(ctx context.Context, repoRoot, root string) {
	// Part 1: falsify Lab.StillWaiting using a busy_timeout=0 child that NEVER waits.
	db := filepath.Join(root, "busy09", "lab.db")
	mustCreateLabDB(ctx, db, sqlitex.JournalWAL)
	lab := procs.NewLab(io.Discard)
	lab.Script.Timeout = stepDeadline
	opts0 := procs.SQLOptions{Path: db, JournalMode: sqlitex.JournalWAL, BusyTimeout: 0}
	w1 := lab.SpawnSQL("w1", opts0)
	w2 := lab.SpawnSQL("w2", opts0)
	lab.Must(w1, "begin", "immediate")
	lab.Must(w1, "exec", "INSERT INTO lab(who) VALUES ('hold')")
	call0 := lab.Begin(w2, "begin", "immediate")
	// w2 has busy_timeout=0 so it returns SQLITE_BUSY immediately without waiting.
	// A non-vacuous StillWaiting must return false; a 0ns poll falsely returns true!
	falseWait := lab.StillWaiting(call0)
	res0 := lab.Finish(call0)
	lab.Must(w1, "commit")
	lab.StopChildren()

	// Part 2: run 6.2-busy-rules under strace (Linux) and count EAGAIN on l_start=120.
	// Expected minimum EAGAINs across 6.2: B(2) + C1(>=1) + C2(>=1) + C3(1) + E(5 rounds >=5) => at least 10
	// (specifically Part E alone claims 5 waiting rounds). We count total EAGAIN on l_start=120.
	eagainDetail := "strace=skipped(non-linux)"
	straceOK := true
	if runtime.GOOS == "linux" {
		bin := filepath.Join(root, "busy09", "exp62")
		buildCmd := exec.CommandContext(ctx, "go", "build", "-o", bin, "./ch06-locking-and-concurrency/experiments/6.2-busy-rules")
		buildCmd.Dir = repoRoot
		if out, err := buildCmd.CombinedOutput(); err != nil {
			fatal("busy09 build 6.2", fmt.Errorf("%v: %s", err, string(out)))
		}
		traceFile := filepath.Join(root, "busy09", "exp62.strace")
		expWork := filepath.Join(root, "busy09", "work")
		if err := os.MkdirAll(expWork, 0o755); err != nil {
			fatal("busy09 mkdir work", err)
		}
		cmd := exec.CommandContext(ctx, "strace", "-f", "-e", "trace=fcntl", "-o", traceFile, bin)
		cmd.Dir = repoRoot
		cmd.Env = append(os.Environ(), "COURSE_WORK_DIR="+expWork)
		if out, err := cmd.CombinedOutput(); err != nil {
			fatal("busy09 strace 6.2", fmt.Errorf("%v: %s", err, string(out)))
		}
		raw, err := os.ReadFile(traceFile)
		if err != nil {
			fatal("busy09 read strace", err)
		}
		// Count how many distinct F_WRLCK lock acquisitions on l_start=120 were preceded by EAGAIN
		// vs total successful F_WRLCK acquisitions across 6.2 (and total EAGAIN count on byte 120).
		eagain120 := 0
		for _, line := range strings.Split(string(raw), "\n") {
			if strings.Contains(line, "l_start=120, l_len=1") && strings.Contains(line, "F_WRLCK") && strings.Contains(line, "EAGAIN") {
				eagain120++
			}
		}
		// Parts B(2) + C1(>=1) + C2(>=1) + C3(1) + E(5 rounds, >=1 each) = at least 10 EAGAINs if every waiting step actually contended.
		straceOK = eagain120 >= 10
		eagainDetail = fmt.Sprintf("eagain_on_byte120=%d(want>=10)", eagain120)
	}

	held := !falseWait && res0.Code == sqlitex.CodeBusy && straceOK
	report(
		"BUSY-09",
		"falsify Lab.StillWaiting with busy_timeout=0 child + strace EAGAIN count on -shm byte 120 in 6.2-busy-rules",
		"StillWaiting returns false for immediate busy_timeout=0 failure and 6.2 generates >=10 EAGAINs on byte 120",
		fmt.Sprintf("still_waiting_on_timeout0=%v res0=%s %s", falseWait, res0.Outcome(), eagainDetail),
		held,
	)
}

func main() {
	if procs.Serve(procs.Roles) {
		return
	}
	ctx := context.Background()
	repoRoot, err := sqlitex.RepoRoot()
	if err != nil {
		fatal("repo root", err)
	}
	workRoot := filepath.Join(repoRoot, "_work", "adv-ch06", "busy-contention")
	if err := os.RemoveAll(workRoot); err != nil {
		fatal("clean work dir", err)
	}
	if err := os.MkdirAll(workRoot, 0o755); err != nil {
		fatal("mkdir work dir", err)
	}
	cli, err := sqlitex.LocateCLI(ctx)
	if err != nil {
		fatal("locate sqlite3 CLI", err)
	}

	attackBusy01a(ctx, workRoot)
	attackBusy01b(ctx, workRoot)
	attackBusy02(ctx, workRoot)
	attackBusy03a(ctx, cli, workRoot)
	attackBusy03b(ctx, cli, workRoot)
	attackBusy04(ctx, workRoot)
	attackBusy05a(ctx, workRoot)
	attackBusy05b(ctx, workRoot)
	attackBusy06(ctx, workRoot)
	attackBusy07(ctx, workRoot)
	attackBusy08(ctx, workRoot)
	attackBusy09(ctx, repoRoot, workRoot)
}
