package main

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/nickstrad/sqlite-internals/internal/procs"
	"github.com/nickstrad/sqlite-internals/internal/recipe"
	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
)

const roleTwoConn = "twoconn"

// twoConnSession holds two independent *sqlitex.DB handles (and two dedicated
// *sql.Conn connections) to the same database file inside ONE OS process.
type twoConnSession struct {
	path  string
	db1   *sqlitex.DB
	conn1 *sql.Conn
	db2   *sqlitex.DB
	conn2 *sql.Conn
}

func twoConnRole(ctx context.Context, args []string) (*procs.Actor, error) {
	fs := flag.NewFlagSet(roleTwoConn, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var dbPath, journal string
	fs.StringVar(&dbPath, "db", "", "")
	fs.StringVar(&journal, "journal", sqlitex.JournalDelete, "")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if dbPath == "" {
		return nil, errors.New("-db is required")
	}
	opts := recipe.Default().WithJournalMode(journal).SQLiteOptions()
	opts.Conn.BusyTimeout = 0
	opts.Conn.WALAutocheckpoint = 0
	opts.ImmediateTx = false

	db1, err := sqlitex.Open(ctx, dbPath, opts)
	if err != nil {
		return nil, fmt.Errorf("open db1: %w", err)
	}
	conn1, err := db1.Conn(ctx)
	if err != nil {
		db1.Close()
		return nil, fmt.Errorf("conn1: %w", err)
	}
	db2, err := sqlitex.Open(ctx, dbPath, opts)
	if err != nil {
		conn1.Close()
		db1.Close()
		return nil, fmt.Errorf("open db2: %w", err)
	}
	conn2, err := db2.Conn(ctx)
	if err != nil {
		db2.Close()
		conn1.Close()
		db1.Close()
		return nil, fmt.Errorf("conn2: %w", err)
	}

	s := &twoConnSession{path: dbPath, db1: db1, conn1: conn1, db2: db2, conn2: conn2}
	execOn := func(which int) procs.Handler {
		return func(ctx context.Context, a []string) (string, error) {
			c := s.conn1
			if which == 2 {
				c = s.conn2
			}
			if c == nil {
				return "", fmt.Errorf("conn%d already closed", which)
			}
			_, err := c.ExecContext(ctx, strings.Join(a, " "))
			return "", err
		}
	}
	queryOn := func(which int) procs.Handler {
		return func(ctx context.Context, a []string) (string, error) {
			c := s.conn1
			if which == 2 {
				c = s.conn2
			}
			if c == nil {
				return "", fmt.Errorf("conn%d already closed", which)
			}
			var v sql.NullString
			if err := c.QueryRowContext(ctx, strings.Join(a, " ")).Scan(&v); err != nil {
				return "", err
			}
			if !v.Valid {
				return "NULL", nil
			}
			return v.String, nil
		}
	}
	return &procs.Actor{
		Actions: map[string]procs.Handler{
			"exec1":  execOn(1),
			"exec2":  execOn(2),
			"query1": queryOn(1),
			"query2": queryOn(2),
			"close1": func(ctx context.Context, a []string) (string, error) {
				var e1, e2 error
				if s.conn1 != nil {
					e1 = s.conn1.Close()
					s.conn1 = nil
				}
				if s.db1 != nil {
					e2 = s.db1.Close()
					s.db1 = nil
				}
				return "", errors.Join(e1, e2)
			},
			"close2": func(ctx context.Context, a []string) (string, error) {
				var e1, e2 error
				if s.conn2 != nil {
					e1 = s.conn2.Close()
					s.conn2 = nil
				}
				if s.db2 != nil {
					e2 = s.db2.Close()
					s.db2 = nil
				}
				return "", errors.Join(e1, e2)
			},
			"stray-close": func(ctx context.Context, a []string) (string, error) {
				if len(a) != 1 {
					return "", errors.New("want target: db, -shm, -wal")
				}
				target := s.path
				if a[0] != "db" {
					target = s.path + a[0]
				}
				f, err := os.Open(target)
				if err != nil {
					return "", err
				}
				return "", f.Close()
			},
		},
		Close: func() error {
			var errs []error
			if s.conn1 != nil {
				errs = append(errs, s.conn1.Close())
			}
			if s.db1 != nil {
				errs = append(errs, s.db1.Close())
			}
			if s.conn2 != nil {
				errs = append(errs, s.conn2.Close())
			}
			if s.db2 != nil {
				errs = append(errs, s.db2.Close())
			}
			return errors.Join(errs...)
		},
	}, nil
}

// runTwoConnStraceChild runs as a standalone child under strace for LOCK-03a.
// It opens two connections in one process, acquires SHARED on conn1, writes a
// marker file, acquires SHARED on conn2, writes a second marker file, and exits.
func runTwoConnStraceChild(dbPath, markerBefore, markerAfter string) error {
	ctx := context.Background()
	opts := recipe.Default().WithJournalMode(sqlitex.JournalDelete).SQLiteOptions()
	opts.Conn.BusyTimeout = 0
	opts.ImmediateTx = false

	db1, err := sqlitex.Open(ctx, dbPath, opts)
	if err != nil {
		return err
	}
	defer db1.Close()
	c1, err := db1.Conn(ctx)
	if err != nil {
		return err
	}
	defer c1.Close()

	db2, err := sqlitex.Open(ctx, dbPath, opts)
	if err != nil {
		return err
	}
	defer db2.Close()
	c2, err := db2.Conn(ctx)
	if err != nil {
		return err
	}
	defer c2.Close()

	if _, err := c1.ExecContext(ctx, "BEGIN DEFERRED"); err != nil {
		return err
	}
	var n int
	if err := c1.QueryRowContext(ctx, "SELECT count(*) FROM lab").Scan(&n); err != nil {
		return err
	}

	// Bracket conn2's SHARED lock acquisition with sentinel file creations so
	// strace output unambiguously separates conn1's fcntl calls from conn2's.
	f1, err := os.Create(markerBefore)
	if err != nil {
		return err
	}
	f1.Close()

	if _, err := c2.ExecContext(ctx, "BEGIN DEFERRED"); err != nil {
		return err
	}
	if err := c2.QueryRowContext(ctx, "SELECT count(*) FROM lab").Scan(&n); err != nil {
		return err
	}

	f2, err := os.Create(markerAfter)
	if err != nil {
		return err
	}
	f2.Close()

	_, _ = c1.ExecContext(ctx, "COMMIT")
	_, _ = c2.ExecContext(ctx, "COMMIT")
	return nil
}

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

func mustCreateLabDB(ctx context.Context, path, journal string, extra ...string) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		fatal("mkdir", err)
	}
	stmts := append([]string{"CREATE TABLE lab(id INTEGER PRIMARY KEY, who TEXT NOT NULL)"}, extra...)
	if err := procs.CreateDatabase(ctx, path, journal, stmts...); err != nil {
		fatal("create "+path, err)
	}
}

// attackLock01 verifies that a stray open+close of -wal (as opposed to -shm)
// does not drop WAL writer locks.
func attackLock01(ctx context.Context, root string) {
	db := filepath.Join(root, "lock01", "lab.db")
	mustCreateLabDB(ctx, db, sqlitex.JournalWAL)

	script := procs.NewScript(io.Discard)
	defer script.Close()
	opts := procs.SQLOptions{Path: db, JournalMode: sqlitex.JournalWAL}
	holder, err := script.Spawn("holder", procs.RoleSQL, opts.Args()...)
	if err != nil {
		fatal("lock01 spawn holder", err)
	}
	contender, err := script.Spawn("contender", procs.RoleSQL, opts.Args()...)
	if err != nil {
		fatal("lock01 spawn contender", err)
	}

	// Write one row first so the -wal file exists on disk.
	if r, err := script.Do(holder, "begin", "immediate"); err != nil || !r.OK() {
		fatal("lock01 seed begin", fmt.Errorf("%v %v", err, r.Outcome()))
	}
	if r, err := script.Do(holder, "exec", "INSERT INTO lab VALUES (1, 'seed')"); err != nil || !r.OK() {
		fatal("lock01 seed insert", fmt.Errorf("%v %v", err, r.Outcome()))
	}
	if r, err := script.Do(holder, "commit"); err != nil || !r.OK() {
		fatal("lock01 seed commit", fmt.Errorf("%v %v", err, r.Outcome()))
	}

	if r, err := script.Do(holder, "begin", "immediate"); err != nil || !r.OK() {
		fatal("lock01 begin", fmt.Errorf("%v %v", err, r.Outcome()))
	}
	if r, err := script.Do(holder, "exec", "INSERT INTO lab VALUES (2, 'holder')"); err != nil || !r.OK() {
		fatal("lock01 insert", fmt.Errorf("%v %v", err, r.Outcome()))
	}
	stray, err := script.Do(holder, "stray-close", "-wal")
	if err != nil || !stray.OK() {
		fatal("lock01 stray-close -wal", fmt.Errorf("%v %v", err, stray.Outcome()))
	}
	states, err := procs.ProbeRanges(db+"-shm", procs.ShmLockRanges())
	if err != nil {
		fatal("lock01 probe", err)
	}
	refused, err := script.Do(contender, "begin", "immediate")
	if err != nil {
		fatal("lock01 contender", err)
	}
	commit, err := script.Do(holder, "commit")
	if err != nil {
		fatal("lock01 commit", err)
	}
	if err := script.Close(); err != nil {
		fatal("lock01 close", err)
	}
	insp, err := procs.InspectDatabase(ctx, db, sqlitex.JournalWAL, "SELECT count(*) FROM lab")
	if err != nil {
		fatal("lock01 inspect", err)
	}
	writeStillHeld := procs.WriteLockedBy(states, "WAL_WRITE_LOCK", holder.Pid)
	held := writeStillHeld && refused.Code == sqlitex.CodeBusy && commit.OK() && insp.Integrity == "ok" && insp.Values[0] == "2"
	report(
		"LOCK-01",
		"WAL holder does stray open+close of -wal file while holding write txn",
		"WAL_WRITE_LOCK on -shm stays held, contender gets SQLITE_BUSY (5), holder commits cleanly (2 rows)",
		fmt.Sprintf("WAL_WRITE_LOCK_held=%v contender=%s commit=%s integrity=%s rows=%s", writeStillHeld, refused.Outcome(), commit.Outcome(), insp.Integrity, insp.Values[0]),
		held,
	)
}

// attackLock02a tests whether SQLite 3.53.4's unixIsSharingShmNode (os_unix.c:2034-2041)
// catches a stray close of the main database file when another process attempts to
// switch journal_mode=DELETE (which requires an EXCLUSIVE lock on the db file).
func attackLock02a(ctx context.Context, root string) {
	db := filepath.Join(root, "lock02a", "lab.db")
	mustCreateLabDB(ctx, db, sqlitex.JournalWAL)

	script := procs.NewScript(io.Discard)
	defer script.Close()
	opts := procs.SQLOptions{Path: db, JournalMode: sqlitex.JournalWAL}
	holder, err := script.Spawn("holder", procs.RoleSQL, opts.Args()...)
	if err != nil {
		fatal("lock02a spawn holder", err)
	}
	contender, err := script.Spawn("contender", procs.RoleSQL, opts.Args()...)
	if err != nil {
		fatal("lock02a spawn contender", err)
	}

	if r, err := script.Do(holder, "begin", "immediate"); err != nil || !r.OK() {
		fatal("lock02a begin", fmt.Errorf("%v %v", err, r.Outcome()))
	}
	if r, err := script.Do(holder, "exec", "INSERT INTO lab VALUES (1, 'holder')"); err != nil || !r.OK() {
		fatal("lock02a insert", fmt.Errorf("%v %v", err, r.Outcome()))
	}
	if r, err := script.Do(holder, "stray-close", "db"); err != nil || !r.OK() {
		fatal("lock02a stray-close db", fmt.Errorf("%v %v", err, r.Outcome()))
	}
	// Contender tries to leave WAL mode (takes EXCLUSIVE lock on db file; SQLite 3.51+
	// checks unixIsSharingShmNode via DMS byte 128 on -shm).
	switchRes, err := script.Do(contender, "query", "PRAGMA journal_mode=delete")
	if err != nil {
		fatal("lock02a journal_mode", err)
	}
	commitRes, err := script.Do(holder, "commit")
	if err != nil {
		fatal("lock02a commit", err)
	}
	if err := script.Close(); err != nil {
		fatal("lock02a close", err)
	}
	held := switchRes.Code == sqlitex.CodeBusy && commitRes.OK()
	report(
		"LOCK-02a",
		"WAL holder stray-closes db file; contender attempts PRAGMA journal_mode=delete",
		"SQLite 3.53.4 unixIsSharingShmNode catches DMS lock on -shm and returns SQLITE_BUSY (5); holder commits OK",
		fmt.Sprintf("journal_mode_delete=%s holder_commit=%s", switchRes.Outcome(), commitRes.Outcome()),
		held,
	)
}

// attackLock02b tests what happens when a WAL *reader* does stray-close -shm:
// its WAL_READ_LOCK and DMS are dropped while its read transaction is still open,
// letting another process commit and checkpoint(TRUNCATE) right over its snapshot.
func attackLock02b(ctx context.Context, root string) {
	db := filepath.Join(root, "lock02b", "lab.db")
	mustCreateLabDB(ctx, db, sqlitex.JournalWAL, "INSERT INTO lab VALUES (1, 'seed')")

	script := procs.NewScript(io.Discard)
	defer script.Close()
	opts := procs.SQLOptions{Path: db, JournalMode: sqlitex.JournalWAL}
	reader, err := script.Spawn("reader", procs.RoleSQL, opts.Args()...)
	if err != nil {
		fatal("lock02b spawn reader", err)
	}
	writer, err := script.Spawn("writer", procs.RoleSQL, opts.Args()...)
	if err != nil {
		fatal("lock02b spawn writer", err)
	}

	// Put a frame in WAL so reader holds WAL_READ_LOCK(1) instead of (0).
	if r, err := script.Do(writer, "begin", "immediate"); err != nil || !r.OK() {
		fatal("lock02b w1", fmt.Errorf("%v %v", err, r.Outcome()))
	}
	if r, err := script.Do(writer, "exec", "INSERT INTO lab VALUES (2, 'before')"); err != nil || !r.OK() {
		fatal("lock02b w2", fmt.Errorf("%v %v", err, r.Outcome()))
	}
	if r, err := script.Do(writer, "commit"); err != nil || !r.OK() {
		fatal("lock02b w3", fmt.Errorf("%v %v", err, r.Outcome()))
	}

	// Reader pins snapshot at 2 rows.
	if r, err := script.Do(reader, "begin", "deferred"); err != nil || !r.OK() {
		fatal("lock02b r begin", fmt.Errorf("%v %v", err, r.Outcome()))
	}
	beforeCount, err := script.Do(reader, "query", "SELECT count(*) FROM lab")
	if err != nil || !beforeCount.OK() {
		fatal("lock02b r count", fmt.Errorf("%v %v", err, beforeCount.Outcome()))
	}

	// Before stray-close, TRUNCATE checkpoint must report busy=1.
	ckptBefore, err := script.Do(writer, "checkpoint", sqlitex.CheckpointTruncate)
	if err != nil {
		fatal("lock02b ckpt before", err)
	}

	// Reader stray-closes -shm, dropping its read mark lock!
	if r, err := script.Do(reader, "stray-close", "-shm"); err != nil || !r.OK() {
		fatal("lock02b stray-close", fmt.Errorf("%v %v", err, r.Outcome()))
	}

	// Writer commits new rows and runs TRUNCATE checkpoint.
	if r, err := script.Do(writer, "begin", "immediate"); err != nil || !r.OK() {
		fatal("lock02b w4", fmt.Errorf("%v %v", err, r.Outcome()))
	}
	if r, err := script.Do(writer, "exec", "INSERT INTO lab VALUES (3, 'after'), (4, 'after2')"); err != nil || !r.OK() {
		fatal("lock02b w5", fmt.Errorf("%v %v", err, r.Outcome()))
	}
	if r, err := script.Do(writer, "commit"); err != nil || !r.OK() {
		fatal("lock02b w6", fmt.Errorf("%v %v", err, r.Outcome()))
	}
	ckptAfter, err := script.Do(writer, "checkpoint", sqlitex.CheckpointTruncate)
	if err != nil {
		fatal("lock02b ckpt after", err)
	}

	// Reader queries again inside its "active" snapshot transaction!
	// Clear page cache effect by scanning a table page or querying freshly.
	afterCount, err := script.Do(reader, "query", "SELECT count(*) FROM lab")
	if err != nil {
		fatal("lock02b r after", err)
	}
	_, _ = script.Do(reader, "commit")
	_ = script.Close()

	blockedBefore := strings.HasPrefix(ckptBefore.Value, "busy=1")
	unblockedAfter := strings.HasPrefix(ckptAfter.Value, "busy=0")
	// Note: reader may still see 2 from its own page cache or see 4/error if it
	// reads from disk; either way, TRUNCATE succeeded while reader was open.
	held := blockedBefore && unblockedAfter
	report(
		"LOCK-02b",
		"WAL reader stray-closes -shm; writer commits and runs checkpoint(TRUNCATE)",
		"checkpoint(TRUNCATE) blocked (busy=1) before stray-close, succeeds (busy=0) after stray-close despite open read txn",
		fmt.Sprintf("before=(%s) after=(%s) reader_count_before=%s reader_count_after=%s", ckptBefore.Value, ckptAfter.Value, beforeCount.Value, afterCount.Outcome()),
		held,
	)
}

// attackLock03a tests two connections in ONE process (rollback mode):
//  1. Under strace (Linux), conn2 taking SHARED makes 0 fcntl(F_SETLK) syscalls.
//  2. Closing conn1 while conn2 still holds SHARED parks the fd (setPendingFd) so
//     conn2's POSIX lock survives and still blocks an external writer's COMMIT.
func attackLock03a(ctx context.Context, root string) {
	db := filepath.Join(root, "lock03a", "lab.db")
	mustCreateLabDB(ctx, db, sqlitex.JournalDelete, "INSERT INTO lab VALUES (1, 'seed')")

	// Part 1: strace fcntl count for second connection's SHARED lock (Linux only).
	straceDetail := "strace=skipped(non-linux)"
	straceOK := true
	if runtime.GOOS == "linux" {
		self, err := os.Executable()
		if err != nil {
			fatal("lock03a exe", err)
		}
		traceOut := filepath.Join(root, "lock03a", "strace.log")
		mBefore := filepath.Join(root, "lock03a", "marker.before")
		mAfter := filepath.Join(root, "lock03a", "marker.after")
		cmd := exec.CommandContext(ctx, "strace", "-f", "-e", "trace=fcntl,openat", "-o", traceOut,
			self, "twoconn-strace", db, mBefore, mAfter)
		if out, err := cmd.CombinedOutput(); err != nil {
			fatal("lock03a strace", fmt.Errorf("%v: %s", err, string(out)))
		}
		raw, err := os.ReadFile(traceOut)
		if err != nil {
			fatal("lock03a read trace", err)
		}
		inWindow := false
		conn1Locks, conn2Locks := 0, 0
		for _, line := range strings.Split(string(raw), "\n") {
			if strings.Contains(line, "marker.before") {
				inWindow = true
				continue
			}
			if strings.Contains(line, "marker.after") {
				inWindow = false
				continue
			}
			if strings.Contains(line, "fcntl(") && strings.Contains(line, "F_SETLK") {
				if inWindow {
					conn2Locks++
				} else {
					conn1Locks++
				}
			}
		}
		straceOK = conn1Locks > 0 && conn2Locks == 0
		straceDetail = fmt.Sprintf("conn1_F_SETLK=%d conn2_F_SETLK=%d", conn1Locks, conn2Locks)
	}

	// Part 2: close conn1 while conn2 still holds SHARED; verify kernel lock stays held.
	script := procs.NewScript(io.Discard)
	defer script.Close()
	dual, err := script.Spawn("dual", roleTwoConn, "-db", db, "-journal", sqlitex.JournalDelete)
	if err != nil {
		fatal("lock03a spawn dual", err)
	}
	extWriter, err := script.Spawn("ext", procs.RoleSQL, procs.SQLOptions{Path: db, JournalMode: sqlitex.JournalDelete}.Args()...)
	if err != nil {
		fatal("lock03a spawn ext", err)
	}
	if r, err := script.Do(dual, "exec1", "BEGIN DEFERRED"); err != nil || !r.OK() {
		fatal("lock03a b1", fmt.Errorf("%v %v", err, r.Outcome()))
	}
	if r, err := script.Do(dual, "query1", "SELECT count(*) FROM lab"); err != nil || !r.OK() {
		fatal("lock03a q1", fmt.Errorf("%v %v", err, r.Outcome()))
	}
	if r, err := script.Do(dual, "exec2", "BEGIN DEFERRED"); err != nil || !r.OK() {
		fatal("lock03a b2", fmt.Errorf("%v %v", err, r.Outcome()))
	}
	if r, err := script.Do(dual, "query2", "SELECT count(*) FROM lab"); err != nil || !r.OK() {
		fatal("lock03a q2", fmt.Errorf("%v %v", err, r.Outcome()))
	}
	// End conn1's txn and CLOSE conn1 completely while conn2 still holds SHARED.
	if r, err := script.Do(dual, "exec1", "COMMIT"); err != nil || !r.OK() {
		fatal("lock03a c1", fmt.Errorf("%v %v", err, r.Outcome()))
	}
	if r, err := script.Do(dual, "close1"); err != nil || !r.OK() {
		fatal("lock03a close1", fmt.Errorf("%v %v", err, r.Outcome()))
	}
	states, err := procs.ProbeRanges(db, procs.DatabaseLockRanges())
	if err != nil {
		fatal("lock03a probe", err)
	}
	sharedStillHeld := procs.ReadLocked(states, "SHARED")

	// External writer tries to commit; must get SQLITE_BUSY because conn2 still holds SHARED.
	if r, err := script.Do(extWriter, "begin", "immediate"); err != nil || !r.OK() {
		fatal("lock03a ext begin", fmt.Errorf("%v %v", err, r.Outcome()))
	}
	if r, err := script.Do(extWriter, "exec", "INSERT INTO lab VALUES (2, 'ext')"); err != nil || !r.OK() {
		fatal("lock03a ext ins", fmt.Errorf("%v %v", err, r.Outcome()))
	}
	extCommit, err := script.Do(extWriter, "commit")
	if err != nil {
		fatal("lock03a ext commit", err)
	}
	_, _ = script.Do(dual, "exec2", "COMMIT")
	extCommit2, err := script.Do(extWriter, "commit")
	if err != nil {
		fatal("lock03a ext commit2", err)
	}
	if err := script.Close(); err != nil {
		fatal("lock03a close", err)
	}

	held := straceOK && sharedStillHeld && extCommit.Code == sqlitex.CodeBusy && extCommit2.OK()
	report(
		"LOCK-03a",
		"two connections in one process (rollback): shared lock fcntl count + closing conn1 while conn2 holds SHARED",
		"conn2 issues 0 F_SETLK syscalls; closing conn1 parks fd so SHARED stays held and blocks external COMMIT (BUSY 5) until conn2 ends",
		fmt.Sprintf("%s shared_after_close1=%v ext_commit_during=%s ext_commit_after=%s", straceDetail, sharedStillHeld, extCommit.Outcome(), extCommit2.Outcome()),
		held,
	)
}

// attackLock03b tests intra-process writer contention on two connections in rollback mode.
func attackLock03b(ctx context.Context, root string) {
	db := filepath.Join(root, "lock03b", "lab.db")
	mustCreateLabDB(ctx, db, sqlitex.JournalDelete)

	script := procs.NewScript(io.Discard)
	defer script.Close()
	dual, err := script.Spawn("dual", roleTwoConn, "-db", db, "-journal", sqlitex.JournalDelete)
	if err != nil {
		fatal("lock03b spawn", err)
	}
	if r, err := script.Do(dual, "exec1", "BEGIN IMMEDIATE"); err != nil || !r.OK() {
		fatal("lock03b b1", fmt.Errorf("%v %v", err, r.Outcome()))
	}
	if r, err := script.Do(dual, "exec1", "INSERT INTO lab VALUES (1, 'c1')"); err != nil || !r.OK() {
		fatal("lock03b i1", fmt.Errorf("%v %v", err, r.Outcome()))
	}
	c2Begin, err := script.Do(dual, "exec2", "BEGIN IMMEDIATE")
	if err != nil {
		fatal("lock03b b2", err)
	}
	states, err := procs.ProbeRanges(db, procs.DatabaseLockRanges())
	if err != nil {
		fatal("lock03b probe", err)
	}
	c1Commit, err := script.Do(dual, "exec1", "COMMIT")
	if err != nil {
		fatal("lock03b c1", err)
	}
	if err := script.Close(); err != nil {
		fatal("lock03b close", err)
	}

	reservedHeld := procs.WriteLockedBy(states, "RESERVED", dual.Pid)
	held := c2Begin.Code == sqlitex.CodeBusy && reservedHeld && c1Commit.OK()
	report(
		"LOCK-03b",
		"two connections in one process (rollback): conn2 BEGIN IMMEDIATE while conn1 holds write lock",
		"unixInodeInfo enforces intra-process exclusion: conn2 gets SQLITE_BUSY (5), RESERVED stays held, conn1 commits OK",
		fmt.Sprintf("conn2=%s RESERVED_held=%v conn1_commit=%s", c2Begin.Outcome(), reservedHeld, c1Commit.Outcome()),
		held,
	)
}

// attackLock03c tests two connections in one process in WAL mode:
// conn2 refused while conn1 holds WAL_WRITE_LOCK, and closing conn2 does NOT drop
// conn1's -shm locks (unlike a stray os.Open+Close).
func attackLock03c(ctx context.Context, root string) {
	db := filepath.Join(root, "lock03c", "lab.db")
	mustCreateLabDB(ctx, db, sqlitex.JournalWAL)

	script := procs.NewScript(io.Discard)
	defer script.Close()
	dual, err := script.Spawn("dual", roleTwoConn, "-db", db, "-journal", sqlitex.JournalWAL)
	if err != nil {
		fatal("lock03c spawn dual", err)
	}
	ext, err := script.Spawn("ext", procs.RoleSQL, procs.SQLOptions{Path: db, JournalMode: sqlitex.JournalWAL}.Args()...)
	if err != nil {
		fatal("lock03c spawn ext", err)
	}
	if r, err := script.Do(dual, "exec1", "BEGIN IMMEDIATE"); err != nil || !r.OK() {
		fatal("lock03c b1", fmt.Errorf("%v %v", err, r.Outcome()))
	}
	if r, err := script.Do(dual, "exec1", "INSERT INTO lab VALUES (1, 'c1')"); err != nil || !r.OK() {
		fatal("lock03c i1", fmt.Errorf("%v %v", err, r.Outcome()))
	}
	c2Begin, err := script.Do(dual, "exec2", "BEGIN IMMEDIATE")
	if err != nil {
		fatal("lock03c b2", err)
	}
	// Closing conn2 inside SQLite must keep unixShmNode open and preserve conn1's locks.
	if r, err := script.Do(dual, "close2"); err != nil || !r.OK() {
		fatal("lock03c close2", fmt.Errorf("%v %v", err, r.Outcome()))
	}
	states, err := procs.ProbeRanges(db+"-shm", procs.ShmLockRanges())
	if err != nil {
		fatal("lock03c probe", err)
	}
	extRefused, err := script.Do(ext, "begin", "immediate")
	if err != nil {
		fatal("lock03c ext", err)
	}
	c1Commit, err := script.Do(dual, "exec1", "COMMIT")
	if err != nil {
		fatal("lock03c c1", err)
	}
	if err := script.Close(); err != nil {
		fatal("lock03c close", err)
	}

	writeHeld := procs.WriteLockedBy(states, "WAL_WRITE_LOCK", dual.Pid)
	held := c2Begin.Code == sqlitex.CodeBusy && writeHeld && extRefused.Code == sqlitex.CodeBusy && c1Commit.OK()
	report(
		"LOCK-03c",
		"two connections in one process (WAL): conn2 contention and closing conn2 while conn1 holds WAL_WRITE_LOCK",
		"conn2 gets SQLITE_BUSY (5); closing conn2 preserves conn1's -shm WAL_WRITE_LOCK and external contender still gets BUSY (5)",
		fmt.Sprintf("conn2=%s WAL_WRITE_LOCK_after_close2=%v ext=%s conn1_commit=%s", c2Begin.Outcome(), writeHeld, extRefused.Outcome(), c1Commit.Outcome()),
		held,
	)
}

// attackLock03d tests that a stray os.Open+Close inside a two-connection process
// wipes out the process's kernel locks for BOTH connections, admitting an external writer.
func attackLock03d(ctx context.Context, root string) {
	db := filepath.Join(root, "lock03d", "lab.db")
	mustCreateLabDB(ctx, db, sqlitex.JournalDelete)

	script := procs.NewScript(io.Discard)
	defer script.Close()
	dual, err := script.Spawn("dual", roleTwoConn, "-db", db, "-journal", sqlitex.JournalDelete)
	if err != nil {
		fatal("lock03d spawn dual", err)
	}
	ext, err := script.Spawn("ext", procs.RoleSQL, procs.SQLOptions{Path: db, JournalMode: sqlitex.JournalDelete}.Args()...)
	if err != nil {
		fatal("lock03d spawn ext", err)
	}
	if r, err := script.Do(dual, "exec1", "BEGIN IMMEDIATE"); err != nil || !r.OK() {
		fatal("lock03d b1", fmt.Errorf("%v %v", err, r.Outcome()))
	}
	if r, err := script.Do(dual, "exec1", "INSERT INTO lab VALUES (1, 'c1')"); err != nil || !r.OK() {
		fatal("lock03d i1", fmt.Errorf("%v %v", err, r.Outcome()))
	}
	if r, err := script.Do(dual, "exec2", "BEGIN DEFERRED"); err != nil || !r.OK() {
		fatal("lock03d b2", fmt.Errorf("%v %v", err, r.Outcome()))
	}
	if r, err := script.Do(dual, "query2", "SELECT count(*) FROM lab"); err != nil || !r.OK() {
		fatal("lock03d q2", fmt.Errorf("%v %v", err, r.Outcome()))
	}
	if r, err := script.Do(dual, "stray-close", "db"); err != nil || !r.OK() {
		fatal("lock03d stray", fmt.Errorf("%v %v", err, r.Outcome()))
	}
	states, err := procs.ProbeRanges(db, procs.DatabaseLockRanges())
	if err != nil {
		fatal("lock03d probe", err)
	}
	extBegin, err := script.Do(ext, "begin", "immediate")
	if err != nil {
		fatal("lock03d ext", err)
	}
	_, _ = script.Do(ext, "rollback")
	_, _ = script.Do(dual, "exec2", "ROLLBACK")
	_, _ = script.Do(dual, "exec1", "ROLLBACK")
	_ = script.Close()

	allUnlocked := procs.Unlocked(states)
	held := allUnlocked && extBegin.OK()
	report(
		"LOCK-03d",
		"two connections in one process (rollback): stray os.Open+Close on db drops kernel locks for both connections",
		"all db lock ranges unlocked and external writer admitted (SQLITE_OK)",
		fmt.Sprintf("all_unlocked=%v ext_begin=%s", allUnlocked, extBegin.Outcome()),
		held,
	)
}

// attackLock04a uses the C sqlite3 CLI as the second process against a Go holder in rollback mode.
func attackLock04a(ctx context.Context, cli sqlitex.CLI, root string) {
	db := filepath.Join(root, "lock04a", "lab.db")
	mustCreateLabDB(ctx, db, sqlitex.JournalDelete)

	script := procs.NewScript(io.Discard)
	defer script.Close()
	holder, err := script.Spawn("holder", procs.RoleSQL, procs.SQLOptions{Path: db, JournalMode: sqlitex.JournalDelete}.Args()...)
	if err != nil {
		fatal("lock04a spawn", err)
	}
	if r, err := script.Do(holder, "begin", "immediate"); err != nil || !r.OK() {
		fatal("lock04a begin", fmt.Errorf("%v %v", err, r.Outcome()))
	}
	if r, err := script.Do(holder, "exec", "INSERT INTO lab VALUES (1, 'go-holder')"); err != nil || !r.OK() {
		fatal("lock04a insert", fmt.Errorf("%v %v", err, r.Outcome()))
	}

	cliSQL := "PRAGMA busy_timeout=0;\nBEGIN IMMEDIATE;\nINSERT INTO lab VALUES (2, 'cli-contender');\nCOMMIT;\n"
	beforeRes, beforeErr := cli.Run(ctx, sqlitex.CLIRun{Stdin: strings.NewReader(sqlitex.OpenScript(db, cliSQL))})

	if r, err := script.Do(holder, "stray-close", "db"); err != nil || !r.OK() {
		fatal("lock04a stray", fmt.Errorf("%v %v", err, r.Outcome()))
	}
	afterRes, afterErr := cli.Run(ctx, sqlitex.CLIRun{Stdin: strings.NewReader(sqlitex.OpenScript(db, cliSQL))})
	holderCommit, err := script.Do(holder, "commit")
	if err != nil {
		fatal("lock04a commit", err)
	}
	if err := script.Close(); err != nil {
		fatal("lock04a close", err)
	}
	insp, err := procs.InspectDatabase(ctx, db, sqlitex.JournalDelete, "SELECT group_concat(who, ',') FROM (SELECT who FROM lab ORDER BY id)")
	if err != nil {
		fatal("lock04a inspect", err)
	}

	cliRefused := beforeErr != nil && strings.Contains(beforeRes.Stderr, "database is locked")
	cliAdmitted := afterErr == nil && afterRes.ExitCode == 0
	held := cliRefused && cliAdmitted && holderCommit.Code == sqlitex.CodeIOErrDeleteNoEnt && insp.Integrity == "ok" && insp.Values[0] == "go-holder"
	report(
		"LOCK-04a",
		"C sqlite3 CLI as contender vs Go holder in rollback mode across stray-close db",
		"CLI refused before stray-close (locked), CLI commits OK after, Go holder COMMIT gets IOERR_DELETE_NOENT (5898), final DB has only go-holder",
		fmt.Sprintf("cli_before_locked=%v cli_after_ok=%v holder_commit=%s integrity=%s rows=%s", cliRefused, cliAdmitted, holderCommit.Outcome(), insp.Integrity, insp.Values[0]),
		held,
	)
}

// attackLock04b uses the C sqlite3 CLI as the second process against a Go holder in WAL mode.
func attackLock04b(ctx context.Context, cli sqlitex.CLI, root string) {
	db := filepath.Join(root, "lock04b", "lab.db")
	mustCreateLabDB(ctx, db, sqlitex.JournalWAL)

	script := procs.NewScript(io.Discard)
	defer script.Close()
	holder, err := script.Spawn("holder", procs.RoleSQL, procs.SQLOptions{Path: db, JournalMode: sqlitex.JournalWAL}.Args()...)
	if err != nil {
		fatal("lock04b spawn", err)
	}
	if r, err := script.Do(holder, "begin", "immediate"); err != nil || !r.OK() {
		fatal("lock04b begin", fmt.Errorf("%v %v", err, r.Outcome()))
	}
	if r, err := script.Do(holder, "exec", "INSERT INTO lab VALUES (1, 'go-holder')"); err != nil || !r.OK() {
		fatal("lock04b insert", fmt.Errorf("%v %v", err, r.Outcome()))
	}

	cliSQL := "PRAGMA busy_timeout=0;\nBEGIN IMMEDIATE;\nINSERT INTO lab VALUES (2, 'cli-contender');\nCOMMIT;\n"
	beforeRes, beforeErr := cli.Run(ctx, sqlitex.CLIRun{Stdin: strings.NewReader(sqlitex.OpenScript(db, cliSQL))})

	if r, err := script.Do(holder, "stray-close", "db"); err != nil || !r.OK() {
		fatal("lock04b stray db", fmt.Errorf("%v %v", err, r.Outcome()))
	}
	afterDBRes, afterDBErr := cli.Run(ctx, sqlitex.CLIRun{Stdin: strings.NewReader(sqlitex.OpenScript(db, cliSQL))})

	if r, err := script.Do(holder, "stray-close", "-shm"); err != nil || !r.OK() {
		fatal("lock04b stray shm", fmt.Errorf("%v %v", err, r.Outcome()))
	}
	afterShmRes, afterShmErr := cli.Run(ctx, sqlitex.CLIRun{Stdin: strings.NewReader(sqlitex.OpenScript(db, cliSQL))})
	_, walStatErr := os.Stat(db + "-wal")
	_, shmStatErr := os.Stat(db + "-shm")
	walGone := errors.Is(walStatErr, os.ErrNotExist)
	shmGone := errors.Is(shmStatErr, os.ErrNotExist)

	holderCommit, err := script.Do(holder, "commit")
	if err != nil {
		fatal("lock04b commit", err)
	}
	if err := script.Close(); err != nil {
		fatal("lock04b close", err)
	}
	insp, err := procs.InspectDatabase(ctx, db, sqlitex.JournalWAL, "SELECT group_concat(who, ',') FROM (SELECT who FROM lab ORDER BY id)")
	if err != nil {
		fatal("lock04b inspect", err)
	}

	refusedBefore := beforeErr != nil && strings.Contains(beforeRes.Stderr, "database is locked")
	refusedAfterDB := afterDBErr != nil && strings.Contains(afterDBRes.Stderr, "database is locked")
	admittedAfterShm := afterShmErr == nil && afterShmRes.ExitCode == 0
	// Corrected claim (6.1 D / build-notes #1): the CLI commits and exits as the last
	// connection, deleting -wal and -shm; the Go holder's COMMIT then returns SQLITE_OK
	// into the unlinked -wal, silently losing the CLI's committed row.
	held := refusedBefore && refusedAfterDB && admittedAfterShm && walGone && shmGone && holderCommit.OK() && insp.Integrity == "ok" && insp.Values[0] == "go-holder"
	report(
		"LOCK-04b",
		"C sqlite3 CLI as contender vs Go holder in WAL mode across stray-close db and -shm",
		"CLI refused before and after stray-close db, commits OK after stray-close -shm and exits deleting -wal/-shm (6.1 D); Go holder COMMIT returns SQLITE_OK, final DB has only go-holder",
		fmt.Sprintf("before_locked=%v after_db_locked=%v after_shm_ok=%v wal_gone=%v shm_gone=%v holder_commit=%s integrity=%s rows=%s", refusedBefore, refusedAfterDB, admittedAfterShm, walGone, shmGone, holderCommit.Outcome(), insp.Integrity, insp.Values[0]),
		held,
	)
}

// startInteractiveCLI launches a persistent sqlite3 CLI subprocess driven via stdin/stdout pipes.
type interactiveCLI struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	reader *bufio.Reader
	stderr bytes.Buffer
}

func startInteractiveCLI(ctx context.Context, cli sqlitex.CLI, db string) (*interactiveCLI, error) {
	cmd := exec.CommandContext(ctx, cli.Path, "-batch", "-init", os.DevNull)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	ic := &interactiveCLI{cmd: cmd, stdin: stdin, reader: bufio.NewReader(stdout)}
	cmd.Stderr = &ic.stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	if _, err := io.WriteString(stdin, sqlitex.OpenScript(db, "SELECT 'CLI_READY';\n")); err != nil {
		cmd.Process.Kill()
		return nil, err
	}
	if err := ic.waitFor("CLI_READY", 10*time.Second); err != nil {
		cmd.Process.Kill()
		return nil, err
	}
	return ic, nil
}

func (ic *interactiveCLI) sendAndWait(sqlText, marker string, timeout time.Duration) error {
	if _, err := io.WriteString(ic.stdin, sqlText+"\nSELECT '"+marker+"';\n"); err != nil {
		return err
	}
	return ic.waitFor(marker, timeout)
}

func (ic *interactiveCLI) waitFor(marker string, timeout time.Duration) error {
	done := make(chan error, 1)
	go func() {
		for {
			line, err := ic.reader.ReadString('\n')
			if err != nil {
				done <- fmt.Errorf("read stdout waiting for %s: %w (stderr=%q)", marker, err, ic.stderr.String())
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
	case <-time.After(timeout):
		return fmt.Errorf("timeout waiting for %s (stderr=%q)", marker, ic.stderr.String())
	}
}

func (ic *interactiveCLI) close() error {
	_, _ = io.WriteString(ic.stdin, ".quit\n")
	ic.stdin.Close()
	return ic.cmd.Wait()
}

// attackLock04c tests the reverse direction: C sqlite3 CLI holds the write lock,
// Go child is the contender, and parent probes the lock with fcntl(F_GETLK).
func attackLock04c(ctx context.Context, cli sqlitex.CLI, root string) {
	checkMode := func(journal string) (bool, string) {
		db := filepath.Join(root, "lock04c-"+journal, "lab.db")
		mustCreateLabDB(ctx, db, journal)

		ic, err := startInteractiveCLI(ctx, cli, db)
		if err != nil {
			fatal("lock04c start cli "+journal, err)
		}
		if err := ic.sendAndWait("BEGIN IMMEDIATE;\nINSERT INTO lab VALUES (1, 'cli');", "CLI_LOCKED", 10*time.Second); err != nil {
			ic.cmd.Process.Kill()
			fatal("lock04c cli lock "+journal, err)
		}

		var lockOK bool
		if journal == sqlitex.JournalDelete {
			states, err := procs.ProbeRanges(db, procs.DatabaseLockRanges())
			if err != nil {
				ic.cmd.Process.Kill()
				fatal("lock04c probe db", err)
			}
			lockOK = procs.WriteLockedBy(states, "RESERVED", ic.cmd.Process.Pid)
		} else {
			states, err := procs.ProbeRanges(db+"-shm", procs.ShmLockRanges())
			if err != nil {
				ic.cmd.Process.Kill()
				fatal("lock04c probe shm", err)
			}
			lockOK = procs.WriteLockedBy(states, "WAL_WRITE_LOCK", ic.cmd.Process.Pid)
		}

		script := procs.NewScript(io.Discard)
		goChild, err := script.Spawn("go-contender", procs.RoleSQL, procs.SQLOptions{Path: db, JournalMode: journal}.Args()...)
		if err != nil {
			ic.cmd.Process.Kill()
			fatal("lock04c spawn go", err)
		}
		res, err := script.Do(goChild, "begin", "immediate")
		if err != nil {
			ic.cmd.Process.Kill()
			fatal("lock04c go begin", err)
		}
		if err := ic.sendAndWait("COMMIT;", "CLI_DONE", 10*time.Second); err != nil {
			ic.cmd.Process.Kill()
			fatal("lock04c cli commit", err)
		}
		_ = ic.close()
		_ = script.Close()

		ok := lockOK && res.Code == sqlitex.CodeBusy
		return ok, fmt.Sprintf("%s(cli_lock_seen=%v go=%s)", journal, lockOK, res.Outcome())
	}

	okDel, descDel := checkMode(sqlitex.JournalDelete)
	okWAL, descWAL := checkMode(sqlitex.JournalWAL)
	report(
		"LOCK-04c",
		"C sqlite3 CLI holds BEGIN IMMEDIATE while parent probes F_GETLK and Go child contends (rollback and WAL)",
		"parent F_GETLK reports CLI pid holding RESERVED (delete) and WAL_WRITE_LOCK (wal); Go contender gets SQLITE_BUSY (5) in both",
		descDel+" "+descWAL,
		okDel && okWAL,
	)
}

// attackLock05 attacks 6.1 A's clean integrity_check result: in 6.1 A both processes
// wrote 1 tiny row to the same single leaf page. Here both processes write multi-page
// indexed data across a stray close in rollback mode to test whether the file actually
// becomes CORRUPT (not merely losing a row with integrity_check=ok).
func attackLock05(ctx context.Context, root string) {
	db := filepath.Join(root, "lock05", "lab.db")
	if err := os.MkdirAll(filepath.Dir(db), 0o755); err != nil {
		fatal("lock05 mkdir", err)
	}
	if err := procs.CreateDatabase(ctx, db, sqlitex.JournalDelete,
		"CREATE TABLE items(id INTEGER PRIMARY KEY, tag TEXT NOT NULL, body TEXT NOT NULL)",
		"CREATE INDEX items_tag ON items(tag, id)",
	); err != nil {
		fatal("lock05 create", err)
	}

	script := procs.NewScript(io.Discard)
	defer script.Close()
	opts := procs.SQLOptions{Path: db, JournalMode: sqlitex.JournalDelete}
	holder, err := script.Spawn("holder", procs.RoleSQL, opts.Args()...)
	if err != nil {
		fatal("lock05 spawn holder", err)
	}
	contender, err := script.Spawn("contender", procs.RoleSQL, opts.Args()...)
	if err != nil {
		fatal("lock05 spawn contender", err)
	}

	// Holder begins and inserts 40 large rows (allocates multiple table + index pages).
	if r, err := script.Do(holder, "begin", "immediate"); err != nil || !r.OK() {
		fatal("lock05 h begin", fmt.Errorf("%v %v", err, r.Outcome()))
	}
	padH := strings.Repeat("H", 900)
	for i := 1; i <= 40; i++ {
		sqlStmt := fmt.Sprintf("INSERT INTO items VALUES (%d, 'h-%03d', '%s')", i, i, padH)
		if r, err := script.Do(holder, "exec", sqlStmt); err != nil || !r.OK() {
			fatal("lock05 h insert", fmt.Errorf("%v %v", err, r.Outcome()))
		}
	}

	// Stray close drops holder's locks.
	if r, err := script.Do(holder, "stray-close", "db"); err != nil || !r.OK() {
		fatal("lock05 stray", fmt.Errorf("%v %v", err, r.Outcome()))
	}

	// Contender enters and inserts 80 DIFFERENT large rows (allocating even more pages
	// and growing the database file larger than holder's page count), then commits.
	if r, err := script.Do(contender, "begin", "immediate"); err != nil || !r.OK() {
		fatal("lock05 c begin", fmt.Errorf("%v %v", err, r.Outcome()))
	}
	padC := strings.Repeat("C", 900)
	for i := 1001; i <= 1080; i++ {
		sqlStmt := fmt.Sprintf("INSERT INTO items VALUES (%d, 'c-%04d', '%s')", i, i, padC)
		if r, err := script.Do(contender, "exec", sqlStmt); err != nil || !r.OK() {
			fatal("lock05 c insert", fmt.Errorf("%v %v", err, r.Outcome()))
		}
	}
	cCommit, err := script.Do(contender, "commit")
	if err != nil {
		fatal("lock05 c commit", err)
	}
	hCommit, err := script.Do(holder, "commit")
	if err != nil {
		fatal("lock05 h commit", err)
	}
	if err := script.Close(); err != nil {
		fatal("lock05 close", err)
	}
	insp, err := procs.InspectDatabase(ctx, db, sqlitex.JournalDelete, "SELECT count(*) FROM items")
	if err != nil {
		fatal("lock05 inspect", err)
	}

	// 6.1 A's build-notes claim: "The final file passes integrity_check and holds only the holder's row."
	// If multi-page writes instead corrupt the database file (integrity_check != ok),
	// that falsifies the clean-integrity_check generalization and prints BROKEN.
	held := insp.OpenError == "" && insp.Integrity == "ok" && insp.Values[0] == "40"
	report(
		"LOCK-05",
		"rollback stray-close with multi-page indexed writes (holder 40 rows vs contender 80 rows)",
		"if 6.1 A's clean-file outcome generalized: integrity_check=ok with only holder's 40 rows",
		fmt.Sprintf("contender_commit=%s holder_commit=%s open_err=%q integrity=%s count=%s", cCommit.Outcome(), hCommit.Outcome(), insp.OpenError, procs.IntegritySummary(insp.Integrity), insp.Values[0]),
		held,
	)
}

// attackLock06 attacks 6.1 B's claim that WAL stray-close always fails the holder's
// COMMIT with SQLITE_PROTOCOL (15) so "the holder was told".
// In 6.1 B the WAL was empty/backfilled, so holder held WAL_READ_LOCK(0) and COMMIT
// ran walRestartLog -> walTryBeginRead -> SQLITE_PROTOCOL.
// Here we leave an uncheckpointed transaction in the WAL first, so holder holds
// WAL_READ_LOCK(1) and COMMIT appends directly via walFrames WITHOUT calling walRestartLog!
func attackLock06(ctx context.Context, root string) {
	db := filepath.Join(root, "lock06", "lab.db")
	mustCreateLabDB(ctx, db, sqlitex.JournalWAL)

	script := procs.NewScript(io.Discard)
	defer script.Close()
	opts := procs.SQLOptions{Path: db, JournalMode: sqlitex.JournalWAL, WALAutocheckpoint: 0}
	holder, err := script.Spawn("holder", procs.RoleSQL, opts.Args()...)
	if err != nil {
		fatal("lock06 spawn holder", err)
	}
	contender, err := script.Spawn("contender", procs.RoleSQL, opts.Args()...)
	if err != nil {
		fatal("lock06 spawn contender", err)
	}

	// Commit 1 seed transaction into WAL without checkpointing so the WAL is active
	// (mxFrame > 0, nBackfill == 0). Subsequent writers will NOT call walRestartLog.
	if r, err := script.Do(holder, "begin", "immediate"); err != nil || !r.OK() {
		fatal("lock06 seed b", fmt.Errorf("%v %v", err, r.Outcome()))
	}
	if r, err := script.Do(holder, "exec", "INSERT INTO lab VALUES (1, 'seed')"); err != nil || !r.OK() {
		fatal("lock06 seed i", fmt.Errorf("%v %v", err, r.Outcome()))
	}
	if r, err := script.Do(holder, "commit"); err != nil || !r.OK() {
		fatal("lock06 seed c", fmt.Errorf("%v %v", err, r.Outcome()))
	}

	// Holder opens write transaction on the non-empty WAL and inserts row 2.
	if r, err := script.Do(holder, "begin", "immediate"); err != nil || !r.OK() {
		fatal("lock06 h begin", fmt.Errorf("%v %v", err, r.Outcome()))
	}
	if r, err := script.Do(holder, "exec", "INSERT INTO lab VALUES (2, 'holder')"); err != nil || !r.OK() {
		fatal("lock06 h insert", fmt.Errorf("%v %v", err, r.Outcome()))
	}

	// Stray close of -shm releases holder's WAL_WRITE_LOCK.
	if r, err := script.Do(holder, "stray-close", "-shm"); err != nil || !r.OK() {
		fatal("lock06 stray", fmt.Errorf("%v %v", err, r.Outcome()))
	}

	// Contender enters, inserts row 3, and commits cleanly.
	if r, err := script.Do(contender, "begin", "immediate"); err != nil || !r.OK() {
		fatal("lock06 c begin", fmt.Errorf("%v %v", err, r.Outcome()))
	}
	if r, err := script.Do(contender, "exec", "INSERT INTO lab VALUES (3, 'contender')"); err != nil || !r.OK() {
		fatal("lock06 c insert", fmt.Errorf("%v %v", err, r.Outcome()))
	}
	cCommit, err := script.Do(contender, "commit")
	if err != nil {
		fatal("lock06 c commit", err)
	}

	// Holder now commits WITHOUT walRestartLog!
	hCommit, err := script.Do(holder, "commit")
	if err != nil {
		fatal("lock06 h commit", err)
	}
	if err := script.Close(); err != nil {
		fatal("lock06 close", err)
	}
	insp, err := procs.InspectDatabase(ctx, db, sqlitex.JournalWAL,
		"SELECT count(*) || ' row(s): ' || coalesce(group_concat(who, ','), '') FROM (SELECT who FROM lab ORDER BY id)")
	if err != nil {
		fatal("lock06 inspect", err)
	}

	// Corrected claim (6.1 C / build-notes #1): with uncheckpointed frames in the WAL,
	// holder reads through WAL_READ_LOCK(1) and skips walRestartLog (wal.c:3879), so both
	// COMMITs return SQLITE_OK and the holder overwrites the contender's committed row.
	held := cCommit.OK() && hCommit.OK() && insp.Integrity == "ok" && insp.Values[0] == "2 row(s): seed,holder"
	report(
		"LOCK-06",
		"WAL stray-close -shm when WAL already has uncheckpointed frames (no walRestartLog on commit)",
		"6.1 C / build-notes #1 corrected claim: both COMMITs return SQLITE_OK, integrity_check=ok, and contender's row is silently lost (seed,holder)",
		fmt.Sprintf("contender_commit=%s holder_commit=%s integrity=%s final=(%s)", cCommit.Outcome(), hCommit.Outcome(), procs.IntegritySummary(insp.Integrity), insp.Values[0]),
		held,
	)
}

func main() {
	if len(os.Args) == 5 && os.Args[1] == "twoconn-strace" {
		if err := runTwoConnStraceChild(os.Args[2], os.Args[3], os.Args[4]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		return
	}
	roles := map[string]procs.Role{
		procs.RoleSQL:    procs.SQLRole,
		procs.RoleCopier: procs.CopierRole,
		roleTwoConn:      twoConnRole,
	}
	if procs.Serve(roles) {
		return
	}

	ctx := context.Background()
	repoRoot, err := sqlitex.RepoRoot()
	if err != nil {
		fatal("repo root", err)
	}
	workRoot := filepath.Join(repoRoot, "_work", "adv-ch06", "locks-process")
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

	attackLock01(ctx, workRoot)
	attackLock02a(ctx, workRoot)
	attackLock02b(ctx, workRoot)
	attackLock03a(ctx, workRoot)
	attackLock03b(ctx, workRoot)
	attackLock03c(ctx, workRoot)
	attackLock03d(ctx, workRoot)
	attackLock04a(ctx, cli, workRoot)
	attackLock04b(ctx, cli, workRoot)
	attackLock04c(ctx, cli, workRoot)
	attackLock05(ctx, workRoot)
	attackLock06(ctx, workRoot)
}
