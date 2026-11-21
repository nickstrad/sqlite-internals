package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nickstrad/sqlite-internals/internal/equal"
	"github.com/nickstrad/sqlite-internals/internal/procs"
	"github.com/nickstrad/sqlite-internals/internal/recipe"
	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
	"github.com/nickstrad/sqlite-internals/internal/walfile"
	"github.com/nickstrad/sqlite-internals/internal/walship"
)

const pageSize = 4096

func report(id, tried, expect, got string, held bool) {
	status := "BROKEN"
	if held {
		status = "HELD"
	}
	fmt.Printf("ATTACK %s | %s | EXPECT %s | GOT %s | %s\n", id, tried, expect, got, status)
}

func fatal(msg string, err error) {
	fmt.Fprintf(os.Stderr, "FATAL %s: %v\n", msg, err)
	os.Exit(1)
}

func copyFile(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	return errors.Join(copyErr, out.Sync(), out.Close())
}

func onlyJournalMode(diffs []string) bool {
	if len(diffs) == 0 {
		return false
	}
	for _, d := range diffs {
		if !strings.Contains(d, "journal_mode") {
			return false
		}
	}
	return true
}

type refSet struct {
	ctx  context.Context
	r    recipe.Recipe
	dir  string
	maxK int
}

func buildRefSet(ctx context.Context, r recipe.Recipe, dir string, maxK int) (*refSet, error) {
	rs := &refSet{ctx: ctx, r: r, dir: dir, maxK: maxK}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	build := filepath.Join(dir, "build.db")
	err := r.BuildEach(ctx, build, maxK, func(k int) error {
		return copyFile(build, rs.path(k))
	})
	return rs, err
}

func (rs *refSet) path(k int) string {
	return filepath.Join(rs.dir, fmt.Sprintf("step-%02d.db", k))
}

func (rs *refSet) canonical(k int) (string, error) {
	p := filepath.Join(rs.dir, fmt.Sprintf("canonical-%02d.db", k))
	if _, err := os.Stat(p); err == nil {
		return p, nil
	}
	db, err := rs.r.Open(rs.ctx, rs.path(k))
	if err != nil {
		return "", err
	}
	_, execErr := db.ExecContext(rs.ctx, "VACUUM INTO ?", p)
	if err := errors.Join(execErr, db.Close()); err != nil {
		return "", err
	}
	return p, nil
}

// matchRecipe checks capture against recipe steps [minK..maxK] at the requested
// equality level ("E1", "E2", or "VACUUM") and verifies PRAGMA integrity_check=ok.
func (rs *refSet) matchRecipe(capturePath, level string, minK, maxK int) (step int, integrity string, err error) {
	jmode := sqlitex.JournalWAL
	if level == "VACUUM" {
		jmode = sqlitex.JournalDelete
	}
	insp, err := procs.InspectDatabase(rs.ctx, capturePath, jmode)
	if err != nil {
		return -1, "", err
	}
	integrity = insp.Integrity
	if integrity != "ok" {
		return -1, integrity, nil
	}
	for k := minK; k <= maxK; k++ {
		switch level {
		case "E1":
			res, err := equal.CompareFiles(capturePath, rs.path(k), pageSize)
			if err != nil {
				return -1, integrity, err
			}
			if res.Equal {
				return k, integrity, nil
			}
		case "E2":
			res, err := equal.ComparePageFiles(capturePath, rs.path(k), pageSize)
			if err != nil {
				return -1, integrity, err
			}
			if res.Equal {
				return k, integrity, nil
			}
		case "VACUUM":
			e3, _, _, err := equal.CompareLogicalFiles(rs.ctx, capturePath, rs.path(k))
			if err != nil {
				return -1, integrity, err
			}
			if e3.Equal || onlyJournalMode(e3.Differences) {
				canon, err := rs.canonical(k)
				if err != nil {
					return -1, integrity, err
				}
				cRes, err := equal.CompareFiles(capturePath, canon, pageSize)
				if err != nil {
					return -1, integrity, err
				}
				if cRes.Equal {
					return k, integrity, nil
				}
			}
		}
	}
	return -1, integrity, nil
}

// attackCAP01 stresses all four live capture methods concurrently against an
// active writer child committing recipe steps plus periodic PASSIVE checkpoints.
func attackCAP01(ctx context.Context, r recipe.Recipe, refs *refSet, work string) {
	dir := filepath.Join(work, "cap01")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("cap01 mkdir", err)
	}
	live := filepath.Join(dir, "live.db")
	if err := copyFile(refs.path(4), live); err != nil {
		fatal("cap01 copy", err)
	}

	var scriptOut strings.Builder
	script := procs.NewScript(&scriptOut)
	defer script.Close()

	opts := procs.SQLOptions{Path: live, JournalMode: sqlitex.JournalWAL, BusyTimeout: 5 * time.Second}
	writer, err := script.Spawn("writer", procs.RoleSQL, opts.Args()...)
	if err != nil {
		fatal("cap01 spawn writer", err)
	}
	ckpter, err := script.Spawn("ckpter", procs.RoleSQL, opts.Args()...)
	if err != nil {
		fatal("cap01 spawn ckpter", err)
	}

	// Open capture connection with busy_timeout so backup/read transactions wait cleanly.
	sqlOpts := r.SQLiteOptions()
	sqlOpts.Conn.WALAutocheckpoint = 0
	sqlOpts.Conn.BusyTimeout = 5 * time.Second
	db, err := sqlitex.Open(ctx, live, sqlOpts)
	if err != nil {
		fatal("cap01 open", err)
	}
	defer db.Close()
	conn, err := db.Conn(ctx)
	if err != nil {
		fatal("cap01 conn", err)
	}
	defer conn.Close()

	// Drive writer steps 5..20 concurrently in a goroutine while running 4 captures per method.
	var writerErr error
	var writerDone atomic.Bool
	var committed atomic.Int32
	committed.Store(4)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer writerDone.Store(true)
		for k := 5; k <= 20; k++ {
			res, err := script.Do(writer, "recipe-session", strconv.Itoa(k))
			if err != nil || res.Code != 0 {
				writerErr = fmt.Errorf("step %d: %v (%s)", k, err, res.Outcome())
				return
			}
			committed.Store(int32(k))
			if k%3 == 0 {
				// Trigger a PASSIVE checkpoint from a separate process mid-stream.
				_, _ = script.Do(ckpter, "checkpoint", sqlitex.CheckpointPassive)
			}
			time.Sleep(15 * time.Millisecond)
		}
	}()

	type captureRecord struct {
		method string
		path   string
		level  string
	}
	var captures []captureRecord
	var capErrs []string

	round := 0
	for !writerDone.Load() || round < 4 {
		round++
		if round > 16 {
			break
		}
		// 1. sqlite_dbpage inside one read transaction.
		if _, err := conn.ExecContext(ctx, "BEGIN"); err != nil {
			capErrs = append(capErrs, fmt.Sprintf("r%d dbpage begin: %v", round, err))
		} else {
			img, rerr := walship.ReadPages(ctx, conn, pageSize)
			_, cerr := conn.ExecContext(ctx, "COMMIT")
			if rerr != nil || cerr != nil {
				capErrs = append(capErrs, fmt.Sprintf("r%d dbpage: %v %v", round, rerr, cerr))
			} else {
				p := filepath.Join(dir, fmt.Sprintf("dbpage-%d.db", round))
				if err := walship.WriteDatabase(p, img); err != nil {
					capErrs = append(capErrs, fmt.Sprintf("r%d dbpage write: %v", round, err))
				} else {
					captures = append(captures, captureRecord{"dbpage", p, "E1"})
				}
			}
		}

		// 2. file + WAL under a held read transaction (copied by child process to avoid POSIX lock drop).
		if _, err := conn.ExecContext(ctx, "BEGIN"); err != nil {
			capErrs = append(capErrs, fmt.Sprintf("r%d filewal begin: %v", round, err))
		} else {
			var dummy int
			_ = conn.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_schema").Scan(&dummy)
			baseCopy := filepath.Join(dir, fmt.Sprintf("fw-base-%d.db", round))
			walCopy := filepath.Join(dir, fmt.Sprintf("fw-wal-%d.wal", round))
			_, cErr := walfile.CopyLiveFiles(ctx, []walfile.CopyPair{
				{From: live, To: baseCopy},
				{From: live + "-wal", To: walCopy},
			})
			_, endErr := conn.ExecContext(ctx, "COMMIT")
			if cErr != nil || endErr != nil {
				capErrs = append(capErrs, fmt.Sprintf("r%d filewal copy: %v %v", round, cErr, endErr))
			} else {
				out := filepath.Join(dir, fmt.Sprintf("fw-replayed-%d.db", round))
				if _, err := walfile.ReplayFiles(baseCopy, walCopy, out); err != nil {
					capErrs = append(capErrs, fmt.Sprintf("r%d filewal replay: %v", round, err))
				} else {
					captures = append(captures, captureRecord{"file+WAL", out, "E1"})
				}
			}
		}

		// 3. VACUUM INTO.
		vacPath := filepath.Join(dir, fmt.Sprintf("vac-%d.db", round))
		if _, err := conn.ExecContext(ctx, "VACUUM INTO ?", vacPath); err != nil {
			capErrs = append(capErrs, fmt.Sprintf("r%d vacuum: %v", round, err))
		} else {
			captures = append(captures, captureRecord{"VACUUM INTO", vacPath, "VACUUM"})
		}

		// 4. Online backup API (-1 pages per step, with busy_timeout=5s).
		bakPath := filepath.Join(dir, fmt.Sprintf("bak-%d.db", round))
		if _, err := sqlitex.Backup(ctx, conn, bakPath, -1, nil); err != nil {
			capErrs = append(capErrs, fmt.Sprintf("r%d backup: %v", round, err))
		} else {
			captures = append(captures, captureRecord{"backup API", bakPath, "E2"})
		}

		if writerDone.Load() && round >= 4 {
			break
		}
	}

	wg.Wait()
	if writerErr != nil {
		fatal("cap01 writer", writerErr)
	}

	matched := 0
	var bad []string
	seenSteps := map[string]map[int]bool{}
	for _, c := range captures {
		k, integ, err := refs.matchRecipe(c.path, c.level, 4, 20)
		if err != nil || integ != "ok" || k < 0 {
			bad = append(bad, fmt.Sprintf("%s(%s): step=%d integ=%q err=%v", c.method, filepath.Base(c.path), k, integ, err))
			continue
		}
		matched++
		if seenSteps[c.method] == nil {
			seenSteps[c.method] = map[int]bool{}
		}
		seenSteps[c.method][k] = true
	}

	held := len(capErrs) == 0 && len(bad) == 0 && matched == len(captures) && matched > 0
	got := fmt.Sprintf("%d/%d captures matched exact recipe states across steps [4..20] (capErrs=%d, mismatches=%d)", matched, len(captures), len(capErrs), len(bad))
	if len(capErrs) > 0 {
		got += "; first capErr: " + capErrs[0]
	}
	if len(bad) > 0 {
		got += "; first mismatch: " + bad[0]
	}
	report("CAP-01", "concurrent stress of 4 live capture methods under active writer + PASSIVE checkpoints",
		"every capture passes integrity_check and matches an exact recipe step at its claimed equality level", got, held)
}

// runCLIBackupRace spawns a rapid loop of fresh sqlite3 CLI writers (each opening,
// inserting, and closing — triggering sqlite3WalClose when no keeper is open) while
// running N sqlite3 ".backup" calls.
func runCLIBackupRace(ctx context.Context, cli sqlitex.CLI, dbPath, dir string, backups int, timeoutMS int, holdKeeper bool) (succeeded, locked, other int, sampleErr string) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("cap02 out mkdir", err)
	}
	if holdKeeper {
		r := recipe.Default()
		opts := r.SQLiteOptions()
		opts.Conn.WALAutocheckpoint = 0
		opts.Conn.BusyTimeout = 5 * time.Second
		keeper, err := sqlitex.Open(ctx, dbPath, opts)
		if err != nil {
			fatal("cap02 keeper open", err)
		}
		// Ensure keeper actually acquires WAL handles (not just lazy open).
		var n int
		if err := keeper.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_schema").Scan(&n); err != nil {
			keeper.Close()
			fatal("cap02 keeper query", err)
		}
		defer keeper.Close()
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		i := 0
		for {
			select {
			case <-stop:
				return
			default:
			}
			i++
			sqlText := fmt.Sprintf("PRAGMA busy_timeout=2000; INSERT INTO churn(v) VALUES (%d);", i)
			_, _ = cli.Run(ctx, sqlitex.CLIRun{Args: []string{dbPath, sqlText}})
		}
	}()

	// Give the writer loop a moment to start churning.
	time.Sleep(30 * time.Millisecond)

	for i := 0; i < backups; i++ {
		dst := filepath.Join(dir, fmt.Sprintf("bak-%03d.db", i))
		var cmdStr string
		if timeoutMS > 0 {
			cmdStr = fmt.Sprintf(".timeout %d\n.backup %q\n", timeoutMS, dst)
		} else {
			cmdStr = fmt.Sprintf(".backup %q\n", dst)
		}
		res, err := cli.Run(ctx, sqlitex.CLIRun{Args: []string{dbPath}, Stdin: strings.NewReader(cmdStr)})
		if err == nil {
			succeeded++
		} else {
			msg := strings.TrimSpace(res.Stderr)
			if msg == "" {
				msg = err.Error()
			}
			if strings.Contains(strings.ToLower(msg), "locked") || strings.Contains(strings.ToLower(msg), "busy") {
				locked++
				if sampleErr == "" {
					sampleErr = msg
				}
			} else {
				other++
				if sampleErr == "" {
					sampleErr = msg
				}
			}
		}
	}

	close(stop)
	wg.Wait()
	return succeeded, locked, other, sampleErr
}

// attackCAP02 reproduces the research observation that CLI ".backup" fails with
// "database is locked" under fresh-process writers, isolates the cause (sqlite3WalClose
// taking EXCLUSIVE lock when the last connection closes), and verifies the deterministic
// lock behavior and .timeout fix.
func attackCAP02(ctx context.Context, cli sqlitex.CLI, work string) {
	dir := filepath.Join(work, "cap02")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("cap02 mkdir", err)
	}

	makeChurnDB := func(name string) string {
		p := filepath.Join(dir, name)
		initSQL := fmt.Sprintf("PRAGMA journal_mode=WAL; PRAGMA page_size=4096; PRAGMA application_id=%d; CREATE TABLE churn(id INTEGER PRIMARY KEY, v INTEGER); "+
			"WITH RECURSIVE c(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM c WHERE x<1500) INSERT INTO churn(v) SELECT x FROM c;",
			recipe.Default().SQLiteOptions().File.ApplicationID)
		if _, err := cli.Run(ctx, sqlitex.CLIRun{Args: []string{p, initSQL}}); err != nil {
			fatal("cap02 init "+name, err)
		}
		return p
	}

	// CAP-02a: No keeper connection, no .timeout -> fresh CLI writers close as the last connection.
	dbA := makeChurnDB("a.db")
	okA, lockedA, otherA, errA := runCLIBackupRace(ctx, cli, dbA, filepath.Join(dir, "out-a"), 40, 0, false)
	// Confirms build-notes correction 10: CLI .backup without .timeout hits "database is locked"
	// when racing fresh writer processes closing as the last connection (sqlite3WalClose, wal.c:2507-2571).
	gotA := fmt.Sprintf("ok=%d/40 locked=%d other=%d (sample=%q)", okA, lockedA, otherA, errA)
	report("CAP-02a", "CLI .backup (no .timeout) racing fresh sqlite3 writer processes (each closes as last connection)",
		"reproduces database-is-locked errors (locked>0, other=0) due to sqlite3WalClose EXCLUSIVE lock without .timeout", gotA, lockedA > 0 && otherA == 0)

	// CAP-02b: Exact same race, but holding 1 persistent idle connection so writers are never the last connection.
	dbB := makeChurnDB("b.db")
	okB, lockedB, otherB, errB := runCLIBackupRace(ctx, cli, dbB, filepath.Join(dir, "out-b"), 40, 0, true)
	gotB := fmt.Sprintf("ok=%d/40 locked=%d other=%d (sample=%q)", okB, lockedB, otherB, errB)
	report("CAP-02b", "same CLI .backup race (no .timeout) with 1 persistent idle connection preventing sqlite3WalClose",
		"0 database-is-locked errors once last-connection close EXCLUSIVE lock is prevented", gotB, lockedB == 0 && otherB == 0)

	// CAP-02c: Deterministic EXCLUSIVE lock (e.g. closing last connection / wal_checkpoint or BEGIN EXCLUSIVE)
	// against CLI .backup (timeout=0 vs timeout=5000) and sqlitex.Backup (timeout=0 vs timeout=5000).
	dbC := makeChurnDB("c.db")
	var scriptOut strings.Builder
	script := procs.NewScript(&scriptOut)
	defer script.Close()
	holder, err := script.Spawn("holder", procs.RoleSQL, procs.SQLOptions{Path: dbC, JournalMode: sqlitex.JournalWAL}.Args()...)
	if err != nil {
		fatal("cap02c spawn", err)
	}
	// Hold a writer lock + run PRAGMA locking_mode=EXCLUSIVE so the DB file holds an EXCLUSIVE lock,
	// identical to the EXCLUSIVE lock sqlite3WalClose acquires in wal.c:2520-2529.
	if _, err := script.Do(holder, "exec", "PRAGMA locking_mode=EXCLUSIVE"); err != nil {
		fatal("cap02c locking_mode", err)
	}
	if _, err := script.Do(holder, "exec", "INSERT INTO churn(v) VALUES (9999)"); err != nil {
		fatal("cap02c lock write", err)
	}

	// 1. CLI .backup with timeout=0 must fail immediately with database is locked.
	res0, err0 := cli.Run(ctx, sqlitex.CLIRun{
		Args:  []string{dbC},
		Stdin: strings.NewReader(fmt.Sprintf(".backup %q\n", filepath.Join(dir, "c-cli0.db"))),
	})
	cli0Locked := err0 != nil && strings.Contains(strings.ToLower(res0.Stderr), "locked")

	// 2. sqlitex.Backup with busy_timeout=0 (opened via raw sql.Open so open doesn't pre-query)
	// must fail immediately with SQLITE_BUSY when backup.Step tries to begin a read transaction.
	db0, err := sql.Open("sqlite", dbC)
	if err != nil {
		fatal("cap02c open0", err)
	}
	conn0, err := db0.Conn(ctx)
	if err != nil {
		fatal("cap02c conn0", err)
	}
	_, go0Err := sqlitex.Backup(ctx, conn0, filepath.Join(dir, "c-go0.db"), 10, nil)
	conn0.Close()
	db0.Close()
	go0Busy := sqlitex.IsBusy(go0Err)

	// 3. Release EXCLUSIVE lock after 250ms while CLI .timeout 5000 + .backup runs.
	releaseDone := make(chan error, 1)
	go func() {
		time.Sleep(250 * time.Millisecond)
		if _, err := script.Do(holder, "exec", "PRAGMA locking_mode=NORMAL"); err != nil {
			releaseDone <- err
			return
		}
		_, err := script.Do(holder, "query", "SELECT count(*) FROM churn")
		releaseDone <- err
	}()
	_, cli5kErr := cli.Run(ctx, sqlitex.CLIRun{
		Args:  []string{dbC},
		Stdin: strings.NewReader(fmt.Sprintf(".timeout 5000\n.backup %q\n", filepath.Join(dir, "c-cli5k.db"))),
	})
	relErr := <-releaseDone

	heldC := cli0Locked && go0Busy && cli5kErr == nil && relErr == nil
	gotC := fmt.Sprintf("cli(timeout=0) locked=%t; sqlitex.Backup(timeout=0) busy=%t (%v); cli(.timeout 5000) err=%v",
		cli0Locked, go0Busy, go0Err, cli5kErr)
	report("CAP-02c", "deterministic EXCLUSIVE lock vs CLI .backup (timeout=0 vs 5000) and sqlitex.Backup (timeout=0)",
		"timeout=0 fails with SQLITE_BUSY/locked; .timeout 5000 waits and succeeds after lock release", gotC, heldC)
}

// attackCAP03 tests same-connection ROLLBACK (pager.c:3185) and zero-page COMMIT
// (pager.c:6543) during a stepped backup to see whether they trigger sqlite3BackupRestart.
func attackCAP03(ctx context.Context, r recipe.Recipe, refs *refSet, work string) {
	dir := filepath.Join(work, "cap03")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("cap03 mkdir", err)
	}

	runCase := func(name string, action func(ctx context.Context, exec sqlitex.SourceExecer) error) (copied []int, err error) {
		src := filepath.Join(dir, name+"-src.db")
		dst := filepath.Join(dir, name+"-dst.db")
		if err := copyFile(refs.path(10), src); err != nil {
			return nil, err
		}
		opts := r.SQLiteOptions()
		opts.Conn.WALAutocheckpoint = 0
		db, err := sqlitex.Open(ctx, src, opts)
		if err != nil {
			return nil, err
		}
		defer db.Close()
		conn, err := db.Conn(ctx)
		if err != nil {
			return nil, err
		}
		defer conn.Close()

		_, err = sqlitex.BackupWithSourceExec(ctx, conn, dst, 5, func(step sqlitex.BackupStep, exec sqlitex.SourceExecer) error {
			copied = append(copied, step.PageCount-step.Remaining)
			if step.Step == 2 {
				return action(ctx, exec)
			}
			if step.Step == 4 {
				return errors.New("stop")
			}
			return nil
		})
		if err != nil && err.Error() == "stop" {
			err = nil
		}
		return copied, err
	}

	// Case A: Same-connection ROLLBACK of a write transaction after step 2 (copied=10).
	copiedRB, errRB := runCase("rollback", func(ctx context.Context, exec sqlitex.SourceExecer) error {
		if _, err := exec.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
			return err
		}
		if _, err := exec.ExecContext(ctx, "UPDATE accounts SET balance = balance + 1"); err != nil {
			return err
		}
		_, err := exec.ExecContext(ctx, "ROLLBACK")
		return err
	})
	if errRB != nil {
		fatal("cap03 rollback", errRB)
	}
	// Build-notes correction 1 (narrowed): same-connection COMMITS do not restart a backup,
	// whereas a same-connection write+ROLLBACK calls sqlite3BackupRestart (pager.c:3185) -> [5, 10, 5, 10].
	restarted := len(copiedRB) == 4 && copiedRB[0] == 5 && copiedRB[1] == 10 && copiedRB[2] == 5 && copiedRB[3] == 10
	report("CAP-03a", "same-connection write+ROLLBACK after backup step 2 (copied=10)",
		"copied resets to [5 10 5 10] via sqlite3BackupRestart (pager.c:3185), qualifying 9.1's same-connection commits rule",
		fmt.Sprintf("copied sequence = %v (restarted=%t via pager.c:3185)", copiedRB, restarted), restarted)

	// Case B: Same-connection empty write transaction (BEGIN IMMEDIATE; COMMIT; with 0 dirty pages).
	copiedEmpty, errEmpty := runCase("emptycommit", func(ctx context.Context, exec sqlitex.SourceExecer) error {
		if _, err := exec.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
			return err
		}
		_, err := exec.ExecContext(ctx, "COMMIT")
		return err
	})
	if errEmpty != nil {
		fatal("cap03 emptycommit", errEmpty)
	}
	emptyNoRestart := len(copiedEmpty) >= 3 && copiedEmpty[2] == 15
	report("CAP-03b", "same-connection empty BEGIN IMMEDIATE; COMMIT after backup step 2 (copied=10)",
		"copied advances steadily [5,10,15,20] without restarting",
		fmt.Sprintf("copied sequence = %v (restarted=%t)", copiedEmpty, !emptyNoRestart), emptyNoRestart)
}

// attackCAP04 tests VACUUM INTO running truly concurrently with an active writer
// committing recipe steps in another process (9.1 ran VACUUM INTO before the writer's commit).
func attackCAP04(ctx context.Context, r recipe.Recipe, refs *refSet, work string) {
	dir := filepath.Join(work, "cap04")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("cap04 mkdir", err)
	}
	live := filepath.Join(dir, "live.db")
	if err := copyFile(refs.path(15), live); err != nil {
		fatal("cap04 copy", err)
	}

	var scriptOut strings.Builder
	script := procs.NewScript(&scriptOut)
	defer script.Close()
	writer, err := script.Spawn("writer", procs.RoleSQL, procs.SQLOptions{Path: live, JournalMode: sqlitex.JournalWAL, BusyTimeout: 5 * time.Second}.Args()...)
	if err != nil {
		fatal("cap04 spawn", err)
	}

	opts := r.SQLiteOptions()
	opts.Conn.WALAutocheckpoint = 0
	opts.Conn.BusyTimeout = 5 * time.Second
	db, err := sqlitex.Open(ctx, live, opts)
	if err != nil {
		fatal("cap04 open", err)
	}
	defer db.Close()

	// Run 5 consecutive VACUUM INTO calls while the writer commits steps 16..24 continuously.
	var writerErr error
	var commitsDuring atomic.Int32
	var vacuumActive atomic.Bool
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for k := 16; k <= 24; k++ {
			res, err := script.Do(writer, "recipe-session", strconv.Itoa(k))
			if err != nil || res.Code != 0 {
				writerErr = fmt.Errorf("step %d: %v %s", k, err, res.Outcome())
				return
			}
			if vacuumActive.Load() {
				commitsDuring.Add(1)
			}
		}
	}()

	var vacPaths []string
	var vacErr error
	for i := 0; i < 5; i++ {
		p := filepath.Join(dir, fmt.Sprintf("vac-%d.db", i))
		vacuumActive.Store(true)
		_, err := db.ExecContext(ctx, "VACUUM INTO ?", p)
		vacuumActive.Store(false)
		if err != nil {
			vacErr = err
			break
		}
		vacPaths = append(vacPaths, p)
	}
	wg.Wait()

	if writerErr != nil {
		report("CAP-04", "concurrent VACUUM INTO while writer commits recipe steps 16..24",
			"writer is not blocked and every VACUUM INTO matches an exact recipe state at E1-canonical",
			fmt.Sprintf("writer failed: %v", writerErr), false)
		return
	}
	if vacErr != nil {
		report("CAP-04", "concurrent VACUUM INTO while writer commits recipe steps 16..24",
			"writer is not blocked and every VACUUM INTO matches an exact recipe state at E1-canonical",
			fmt.Sprintf("VACUUM INTO failed: %v", vacErr), false)
		return
	}

	var steps []int
	allOK := true
	for _, p := range vacPaths {
		k, integ, err := refs.matchRecipe(p, "VACUUM", 15, 24)
		if err != nil || integ != "ok" || k < 0 {
			allOK = false
		}
		steps = append(steps, k)
	}
	held := allOK && len(vacPaths) == 5
	got := fmt.Sprintf("5/5 VACUUM INTO matched exact recipe steps %v (commits overlapped with VACUUM INTO=%d, writer errors=0)", steps, commitsDuring.Load())
	report("CAP-04", "concurrent VACUUM INTO while writer child commits steps 16..24",
		"every VACUUM INTO succeeds without blocking writer and matches an exact recipe step (E3 ex. journal_mode, E1 canonical)", got, held)
}

// attackCAP05 tests file + WAL copy under a held read transaction when an outside
// PASSIVE checkpoint actively modifies live.db between copying the first half and
// second half of live.db.
func attackCAP05(ctx context.Context, r recipe.Recipe, refs *refSet, work string) {
	dir := filepath.Join(work, "cap05")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("cap05 mkdir", err)
	}
	live := filepath.Join(dir, "live.db")
	if err := copyFile(refs.path(6), live); err != nil {
		fatal("cap05 copy", err)
	}

	var scriptOut strings.Builder
	script := procs.NewScript(&scriptOut)
	defer script.Close()

	sqlOpts := procs.SQLOptions{Path: live, JournalMode: sqlitex.JournalWAL, BusyTimeout: 5 * time.Second}
	writer, err := script.Spawn("writer", procs.RoleSQL, sqlOpts.Args()...)
	if err != nil {
		fatal("cap05 writer", err)
	}
	outsider, err := script.Spawn("outsider", procs.RoleSQL, sqlOpts.Args()...)
	if err != nil {
		fatal("cap05 outsider", err)
	}
	copier, err := script.Spawn("copier", procs.RoleCopier, "-db", live, "-out", dir)
	if err != nil {
		fatal("cap05 copier", err)
	}

	// Commit steps 7, 8, 9 into -wal (autocheckpoint=0, so live.db is still at step 6).
	for _, k := range []int{7, 8, 9} {
		if res, err := script.Do(writer, "recipe-session", strconv.Itoa(k)); err != nil || res.Code != 0 {
			fatal("cap05 commit", fmt.Errorf("%v %s", err, res.Outcome()))
		}
	}

	// Capture holds a read transaction at step 9 (read-lock slot 1 at end of step 9).
	opts := r.SQLiteOptions()
	opts.Conn.WALAutocheckpoint = 0
	db, err := sqlitex.Open(ctx, live, opts)
	if err != nil {
		fatal("cap05 open", err)
	}
	defer db.Close()
	conn, err := db.Conn(ctx)
	if err != nil {
		fatal("cap05 conn", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN"); err != nil {
		fatal("cap05 begin", err)
	}
	var seen int
	if err := conn.QueryRowContext(ctx, "PRAGMA user_version").Scan(&seen); err != nil {
		fatal("cap05 uv", err)
	}

	// Writer commits step 10 past the capture's read mark.
	if res, err := script.Do(writer, "recipe-session", "10"); err != nil || res.Code != 0 {
		fatal("cap05 step 10", fmt.Errorf("%v %s", err, res.Outcome()))
	}

	// Copy first half of live.db (step 6 bytes), run PASSIVE checkpoint (backfills steps 7..9
	// directly into live.db!), then copy second half of live.db (torn mix of step 6 and step 9),
	// then copy -wal (still un-restarted because of the held read txn).
	info, err := os.Stat(live)
	if err != nil {
		fatal("cap05 stat", err)
	}
	half := (info.Size() / 2 / pageSize) * pageSize
	if _, err := script.Do(copier, "copy-range", "db", "torn-base.db", "0", strconv.FormatInt(half, 10)); err != nil {
		fatal("cap05 copy first half", err)
	}
	ckptRes, err := script.Do(outsider, "checkpoint", sqlitex.CheckpointPassive)
	if err != nil || ckptRes.Code != 0 {
		fatal("cap05 checkpoint", fmt.Errorf("%v %s", err, ckptRes.Outcome()))
	}
	if _, err := script.Do(copier, "copy-range", "db", "torn-base.db", strconv.FormatInt(half, 10), "end"); err != nil {
		fatal("cap05 copy second half", err)
	}
	if _, err := script.Do(copier, "copy", "-wal", "torn-base.wal"); err != nil {
		fatal("cap05 copy wal", err)
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		fatal("cap05 commit", err)
	}

	rebuilt := filepath.Join(dir, "rebuilt.db")
	_, replayErr := walfile.ReplayFiles(filepath.Join(dir, "torn-base.db"), filepath.Join(dir, "torn-base.wal"), rebuilt)
	if replayErr != nil {
		report("CAP-05", "file+WAL copy under held read txn while PASSIVE checkpoint tears live.db mid-copy",
			"walfile.Replay still rebuilds exact E1 recipe step 10 because every backfilled page is in -wal",
			fmt.Sprintf("replay failed: %v (ckpt=%s)", replayErr, ckptRes.Value), false)
		return
	}
	k, integ, err := refs.matchRecipe(rebuilt, "E1", 6, 10)
	held := err == nil && integ == "ok" && k == 10
	report("CAP-05", "file+WAL copy under held read txn while PASSIVE checkpoint tears live.db mid-copy",
		"walfile.Replay rebuilds exact E1 recipe step 10 despite mid-copy PASSIVE backfill",
		fmt.Sprintf("ckpt=%s, rebuilt step=%d (E1), integrity=%s, err=%v", ckptRes.Value, k, integ, err), held)
}

// attackCAP06 runs must-be-able-to-fail negative controls: splitting sqlite_dbpage
// across two read transactions and copying file+WAL without a held read transaction
// across an outside TRUNCATE checkpoint.
func attackCAP06(ctx context.Context, r recipe.Recipe, refs *refSet, work string) {
	dir := filepath.Join(work, "cap06")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("cap06 mkdir", err)
	}

	// CAP-06a: sqlite_dbpage spanning two read transactions across a writer commit.
	liveA := filepath.Join(dir, "live-a.db")
	if err := copyFile(refs.path(6), liveA); err != nil {
		fatal("cap06a copy", err)
	}
	var scriptOut strings.Builder
	script := procs.NewScript(&scriptOut)
	defer script.Close()
	writerA, err := script.Spawn("writerA", procs.RoleSQL, procs.SQLOptions{Path: liveA, JournalMode: sqlitex.JournalWAL, BusyTimeout: 5 * time.Second}.Args()...)
	if err != nil {
		fatal("cap06a spawn", err)
	}
	opts := r.SQLiteOptions()
	opts.Conn.WALAutocheckpoint = 0
	dbA, err := sqlitex.Open(ctx, liveA, opts)
	if err != nil {
		fatal("cap06a open", err)
	}
	defer dbA.Close()
	connA, err := dbA.Conn(ctx)
	if err != nil {
		fatal("cap06a conn", err)
	}
	defer connA.Close()

	// Txn 1: read first half of step 6.
	if _, err := connA.ExecContext(ctx, "BEGIN"); err != nil {
		fatal("cap06a begin1", err)
	}
	var pages int
	if err := connA.QueryRowContext(ctx, "PRAGMA page_count").Scan(&pages); err != nil {
		fatal("cap06a page_count", err)
	}
	readRange := func(c *sql.Conn, first, last int) []byte {
		rows, err := c.QueryContext(ctx, "SELECT data FROM sqlite_dbpage WHERE pgno BETWEEN ? AND ? ORDER BY pgno", first, last)
		if err != nil {
			fatal("cap06a dbpage", err)
		}
		defer rows.Close()
		var out []byte
		for rows.Next() {
			var b []byte
			if err := rows.Scan(&b); err != nil {
				fatal("cap06a scan", err)
			}
			out = append(out, b...)
		}
		return out
	}
	img := readRange(connA, 1, pages/2)
	if _, err := connA.ExecContext(ctx, "COMMIT"); err != nil {
		fatal("cap06a commit1", err)
	}
	// Writer commits steps 7 and 8 (mutates B-tree leaves and header).
	for _, k := range []int{7, 8} {
		if res, err := script.Do(writerA, "recipe-session", strconv.Itoa(k)); err != nil || res.Code != 0 {
			fatal("cap06a step", fmt.Errorf("%v %s", err, res.Outcome()))
		}
	}
	// Txn 2: read second half from step 8.
	if _, err := connA.ExecContext(ctx, "BEGIN"); err != nil {
		fatal("cap06a begin2", err)
	}
	img = append(img, readRange(connA, pages/2+1, pages)...)
	if _, err := connA.ExecContext(ctx, "COMMIT"); err != nil {
		fatal("cap06a commit2", err)
	}
	splitPath := filepath.Join(dir, "split-dbpage.db")
	if err := walship.WriteDatabase(splitPath, img); err != nil {
		fatal("cap06a write", err)
	}
	kA, integA, errA := refs.matchRecipe(splitPath, "E1", 6, 8)
	// Must be detected as corrupt or not matching any recipe state.
	detectedA := errA != nil || integA != "ok" || kA < 0
	report("CAP-06a", "must-fail: sqlite_dbpage split across two read txns around steps 7-8",
		"inconsistent capture fails integrity_check or matches no recipe state",
		fmt.Sprintf("step=%d integrity=%q err=%v", kA, procs.IntegritySummary(integA), errA), detectedA)

	// CAP-06b: file + WAL copied WITHOUT a held read transaction across a TRUNCATE checkpoint.
	liveB := filepath.Join(dir, "live-b.db")
	if err := copyFile(refs.path(6), liveB); err != nil {
		fatal("cap06b copy", err)
	}
	writerB, err := script.Spawn("writerB", procs.RoleSQL, procs.SQLOptions{Path: liveB, JournalMode: sqlitex.JournalWAL, BusyTimeout: 5 * time.Second}.Args()...)
	if err != nil {
		fatal("cap06b writer", err)
	}
	copierB, err := script.Spawn("copierB", procs.RoleCopier, "-db", liveB, "-out", dir)
	if err != nil {
		fatal("cap06b copier", err)
	}
	if _, err := script.Do(copierB, "copy", "db", "unpinned-base.db"); err != nil {
		fatal("cap06b copy base", err)
	}
	// Writer commits 7, checkpoints TRUNCATE (folding 7 into live-b.db and clearing -wal), then commits 8.
	if res, err := script.Do(writerB, "recipe-session", "7"); err != nil || res.Code != 0 {
		fatal("cap06b step7", fmt.Errorf("%v %s", err, res.Outcome()))
	}
	if res, err := script.Do(writerB, "checkpoint", sqlitex.CheckpointTruncate); err != nil || res.Code != 0 {
		fatal("cap06b ckpt", fmt.Errorf("%v %s", err, res.Outcome()))
	}
	if res, err := script.Do(writerB, "recipe-session", "8"); err != nil || res.Code != 0 {
		fatal("cap06b step8", fmt.Errorf("%v %s", err, res.Outcome()))
	}
	if _, err := script.Do(copierB, "copy", "-wal", "unpinned-base.wal"); err != nil {
		fatal("cap06b copy wal", err)
	}
	unpinnedOut := filepath.Join(dir, "unpinned-rebuilt.db")
	_, repErrB := walfile.ReplayFiles(filepath.Join(dir, "unpinned-base.db"), filepath.Join(dir, "unpinned-base.wal"), unpinnedOut)
	detectedB := repErrB != nil
	gotB := fmt.Sprintf("replay err=%v", repErrB)
	if repErrB == nil {
		kB, integB, errB := refs.matchRecipe(unpinnedOut, "E1", 6, 8)
		detectedB = errB != nil || integB != "ok" || kB < 0
		gotB = fmt.Sprintf("step=%d integrity=%q err=%v", kB, procs.IntegritySummary(integB), errB)
	}
	report("CAP-06b", "must-fail: file+WAL copied without held read txn across TRUNCATE checkpoint",
		"replaying newer-generation WAL onto older base fails integrity_check or matches no recipe state", gotB, detectedB)
}

// attackCAP07 probes sqlitex.BackupWithSourceExec edge cases: pagesPerStep=0,
// cancelled context mid-backup, active write transaction on source (backup.c:347-351),
// and unconvertible arg in SourceExecer.
func attackCAP07(ctx context.Context, r recipe.Recipe, refs *refSet, work string) {
	dir := filepath.Join(work, "cap07")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("cap07 mkdir", err)
	}
	src := filepath.Join(dir, "src.db")
	if err := copyFile(refs.path(8), src); err != nil {
		fatal("cap07 copy", err)
	}
	opts := r.SQLiteOptions()
	opts.Conn.WALAutocheckpoint = 0
	opts.Conn.BusyTimeout = 0
	db, err := sqlitex.Open(ctx, src, opts)
	if err != nil {
		fatal("cap07 open", err)
	}
	defer db.Close()
	conn, err := db.Conn(ctx)
	if err != nil {
		fatal("cap07 conn", err)
	}
	defer conn.Close()

	// 1. pagesPerStep == 0
	_, errZero := sqlitex.Backup(ctx, conn, filepath.Join(dir, "zero.db"), 0, nil)

	// 2. Context canceled mid-backup -> must return context.Canceled and leave source usable.
	cctx, cancel := context.WithCancel(ctx)
	_, errCancel := sqlitex.Backup(cctx, conn, filepath.Join(dir, "cancel.db"), 2, func(step sqlitex.BackupStep) error {
		if step.Step == 2 {
			cancel()
		}
		return nil
	})
	cancelOK := errors.Is(errCancel, context.Canceled)

	// 3. Unconvertible argument passed to SourceExecer.
	_, errBadArg := sqlitex.BackupWithSourceExec(ctx, conn, filepath.Join(dir, "badarg.db"), 5, func(step sqlitex.BackupStep, exec sqlitex.SourceExecer) error {
		_, err := exec.ExecContext(ctx, "SELECT ?", struct{ X int }{1})
		return err
	})

	// 4. Backup called while source connection itself holds a write transaction -> SQLITE_BUSY (backup.c:347-351).
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		fatal("cap07 begin", err)
	}
	if _, err := conn.ExecContext(ctx, "UPDATE accounts SET balance = balance + 1 WHERE id = 1"); err != nil {
		fatal("cap07 update", err)
	}
	_, errInWrite := sqlitex.Backup(ctx, conn, filepath.Join(dir, "inwrite.db"), 5, nil)
	_, rbErr := conn.ExecContext(ctx, "ROLLBACK")
	inWriteBusy := sqlitex.IsBusy(errInWrite)

	held := errZero != nil && cancelOK && errBadArg != nil && inWriteBusy && rbErr == nil
	got := fmt.Sprintf("zeroErr=%t cancelErr=%v badArgErr=%t inWriteBusy=%t(%v) rollbackErr=%v",
		errZero != nil, errCancel, errBadArg != nil, inWriteBusy, errInWrite, rbErr)
	report("CAP-07", "sqlitex.BackupWithSourceExec edge cases (0 step, ctx cancel, bad arg, source in write txn)",
		"all four invalid/boundary conditions return expected errors and leave connection clean", got, held)
}

func main() {
	if procs.Serve(procs.Roles) || walfile.ServeLiveCopyChild(os.Args) {
		return
	}
	ctx := context.Background()
	sqlitex.MustPinCheckMemory(ctx)
	cli, err := sqlitex.LocateCLI(ctx)
	if err != nil {
		fatal("locate cli", err)
	}
	root, err := sqlitex.RepoRoot()
	if err != nil {
		fatal("repo root", err)
	}
	work := filepath.Join(root, "_work", "adv-ch09", "capture-falsify")
	if err := os.RemoveAll(work); err != nil {
		fatal("clean work", err)
	}
	if err := os.MkdirAll(work, 0o755); err != nil {
		fatal("mkdir work", err)
	}

	r := recipe.Default().WithSteps(24)
	refs, err := buildRefSet(ctx, r, filepath.Join(work, "refs"), 24)
	if err != nil {
		fatal("build refs", err)
	}

	attackCAP01(ctx, r, refs, work)
	attackCAP02(ctx, cli, work)
	attackCAP03(ctx, r, refs, work)
	attackCAP04(ctx, r, refs, work)
	attackCAP05(ctx, r, refs, work)
	attackCAP06(ctx, r, refs, work)
	attackCAP07(ctx, r, refs, work)
}
