// Group 4 (atrest-and-connmgr-edge): adversarial attacks on internal/connmgr
// (CheckAtRest, Manager stats/eviction invariants, open-reader blind spots,
// and rollback-mode open-rows POSIX lock hazard).
package main

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/nickstrad/sqlite-internals/internal/connmgr"
	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
	_ "modernc.org/sqlite"
)

func fatal(what string, err error) {
	fmt.Fprintf(os.Stderr, "FATAL %s: %v\n", what, err)
	os.Exit(2)
}

func report(id, tried, expect, got string, held bool) {
	status := "BROKEN"
	if held {
		status = "HELD"
	}
	fmt.Printf("ATTACK %s | %s | EXPECT %s | GOT %s | %s\n", id, tried, expect, got, status)
}

func openPlain(ctx context.Context, path string) (*sql.DB, *sql.Conn) {
	uri, err := sqlitex.FileURI(path, nil)
	if err != nil {
		fatal("uri", err)
	}
	db, err := sql.Open("sqlite", uri)
	if err != nil {
		fatal("sql.Open", err)
	}
	db.SetMaxOpenConns(1)
	conn, err := db.Conn(ctx)
	if err != nil {
		fatal("db.Conn", err)
	}
	return db, conn
}

func closePlain(db *sql.DB, conn *sql.Conn) {
	if conn != nil {
		conn.Close()
	}
	if db != nil {
		db.Close()
	}
}

type childProc struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
}

func startChild(ctx context.Context, role, path string) *childProc {
	selfExe, err := os.Executable()
	if err != nil {
		fatal("executable", err)
	}
	cmd := exec.CommandContext(ctx, selfExe, role, path)
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		fatal("stdin", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		fatal("stdout", err)
	}
	if err := cmd.Start(); err != nil {
		fatal("start "+role, err)
	}
	sc := bufio.NewScanner(stdout)
	if !sc.Scan() || sc.Text() != "ready" {
		cmd.Process.Kill()
		fatal("child "+role, fmt.Errorf("expected ready, got %q err=%v", sc.Text(), sc.Err()))
	}
	return &childProc{cmd: cmd, stdin: stdin}
}

func (c *childProc) stop() {
	_ = c.stdin.Close()
	if err := c.cmd.Wait(); err != nil {
		fatal("child wait", err)
	}
}

func runChildTryWrite(ctx context.Context, path string) (ok bool, code int) {
	selfExe, err := os.Executable()
	if err != nil {
		fatal("executable", err)
	}
	out, err := exec.CommandContext(ctx, selfExe, "child-rollback-try-write", path).CombinedOutput()
	if err != nil {
		fatal("try-write child", fmt.Errorf("%v: %s", err, out))
	}
	line := strings.TrimSpace(string(out))
	if line == "WRITE_OK" {
		return true, 0
	}
	fmt.Sscanf(line, "WRITE_ERR code=%d", &code)
	return false, code
}

func runChildRole(role, path string) {
	ctx := context.Background()
	switch role {
	case "child-wal-reader-active":
		db, err := sqlitex.OpenDefault(ctx, path)
		if err != nil {
			fatal(role, err)
		}
		var n int
		if err := db.QueryRowContext(ctx, "SELECT count(*) FROM t").Scan(&n); err != nil {
			fatal(role+" query", err)
		}
		fmt.Println("ready")
		io.Copy(io.Discard, os.Stdin)
		db.Close()

	case "child-wal-reader-opened-only":
		db, conn := openPlain(ctx, path)
		fmt.Println("ready")
		io.Copy(io.Discard, os.Stdin)
		closePlain(db, conn)

	case "child-wal-exclusive-reader":
		db, conn := openPlain(ctx, path)
		if _, err := conn.ExecContext(ctx, "PRAGMA locking_mode=EXCLUSIVE"); err != nil {
			fatal(role+" exclusive", err)
		}
		var n int
		if err := conn.QueryRowContext(ctx, "SELECT count(*) FROM t").Scan(&n); err != nil {
			fatal(role+" query", err)
		}
		fmt.Println("ready")
		io.Copy(io.Discard, os.Stdin)
		closePlain(db, conn)

	case "child-rollback-reader":
		db, conn := openPlain(ctx, path)
		if _, err := conn.ExecContext(ctx, "BEGIN"); err != nil {
			fatal(role+" begin", err)
		}
		var n int
		if err := conn.QueryRowContext(ctx, "SELECT count(*) FROM t").Scan(&n); err != nil {
			fatal(role+" query", err)
		}
		fmt.Println("ready")
		io.Copy(io.Discard, os.Stdin)
		_, _ = conn.ExecContext(ctx, "COMMIT")
		closePlain(db, conn)

	case "child-rollback-try-write":
		opts := sqlitex.DefaultOptions()
		opts.File.JournalMode = sqlitex.JournalDelete
		opts.Conn.BusyTimeout = 0
		opts.SingleConnection = true
		db, err := sqlitex.Open(ctx, path, opts)
		if err != nil {
			code, _ := sqlitex.ErrorCode(err)
			fmt.Printf("WRITE_ERR code=%d\n", code)
			return
		}
		defer db.Close()
		_, err = db.ExecContext(ctx, "BEGIN IMMEDIATE; INSERT INTO t VALUES (999, 'intruder'); COMMIT;")
		if err != nil {
			code, _ := sqlitex.ErrorCode(err)
			fmt.Printf("WRITE_ERR code=%d\n", code)
			return
		}
		fmt.Println("WRITE_OK")

	default:
		fatal("unknown role", errors.New(role))
	}
}

func seedDB(ctx context.Context, path, journalMode string) {
	opts := sqlitex.DefaultOptions()
	opts.File.JournalMode = journalMode
	opts.SingleConnection = true
	db, err := sqlitex.Open(ctx, path, opts)
	if err != nil {
		fatal("seed "+path, err)
	}
	if _, err := db.ExecContext(ctx, "CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT)"); err != nil {
		fatal("seed ddl", err)
	}
	for i := 1; i <= 10; i++ {
		if _, err := db.ExecContext(ctx, "INSERT INTO t VALUES (?, ?)", i, fmt.Sprintf("row-%d", i)); err != nil {
			fatal("seed insert", err)
		}
	}
	if err := db.Close(); err != nil {
		fatal("seed close", err)
	}
}

func main() {
	if len(os.Args) == 3 && strings.HasPrefix(os.Args[1], "child-") {
		runChildRole(os.Args[1], os.Args[2])
		return
	}

	ctx := context.Background()
	repoRoot, err := sqlitex.RepoRoot()
	if err != nil {
		fatal("repo root", err)
	}
	scratch := filepath.Join(repoRoot, "_work", "adv-ch08", "atrest-and-connmgr-edge")
	if err := os.RemoveAll(scratch); err != nil {
		fatal("clean scratch", err)
	}
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		fatal("mkdir scratch", err)
	}

	// ATR-01a: Open WAL reader matrix (active reader with writes, active reader with 0-byte WAL, opened-only reader) (OUTLINE 8.3).
	{
		dir := filepath.Join(scratch, "atr01a")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fatal("mkdir atr01a", err)
		}
		pWrite := filepath.Join(dir, "w.db")
		pReadOnly := filepath.Join(dir, "r.db")
		pOpenedOnly := filepath.Join(dir, "o.db")
		seedDB(ctx, pWrite, sqlitex.JournalWAL)
		seedDB(ctx, pReadOnly, sqlitex.JournalWAL)
		seedDB(ctx, pOpenedOnly, sqlitex.JournalWAL)

		m, err := connmgr.New(connmgr.Options{
			Capacity: 4,
			PathFor:  func(id string) (string, error) { return filepath.Join(dir, id+".db"), nil },
			Open: func(ctx context.Context, path string) (*sqlitex.DB, error) {
				o := sqlitex.DefaultOptions()
				o.SingleConnection = true
				return sqlitex.Open(ctx, path, o)
			},
		})
		if err != nil {
			fatal("connmgr atr01a", err)
		}

		// Case 1: Active reader + write in WAL -> not at rest (2 problems: -wal, -shm); record dropped.
		hw, err := m.Acquire(ctx, "w")
		if err != nil {
			fatal("acq w", err)
		}
		if _, err := hw.DB.ExecContext(ctx, "INSERT INTO t VALUES (11, 'wal-only')"); err != nil {
			fatal("ins w", err)
		}
		hw.Release()
		chW := startChild(ctx, "child-wal-reader-active", pWrite)
		evW, err := m.Evict("w")
		if err != nil {
			fatal("evict w", err)
		}
		_, recW := m.Record("w")
		chW.stop()
		afterW, _ := connmgr.CheckAtRest(pWrite)

		// Case 2: Active reader + read-only manager (0-byte WAL) -> still not at rest (-shm exists).
		hr, err := m.Acquire(ctx, "r")
		if err != nil {
			fatal("acq r", err)
		}
		var cnt int
		if err := hr.DB.QueryRowContext(ctx, "SELECT count(*) FROM t").Scan(&cnt); err != nil {
			fatal("read r", err)
		}
		hr.Release()
		chR := startChild(ctx, "child-wal-reader-active", pReadOnly)
		evR, err := m.Evict("r")
		if err != nil {
			fatal("evict r", err)
		}
		chR.stop()
		afterR, _ := connmgr.CheckAtRest(pReadOnly)

		// Case 3: Opened-only reader (no statement yet, so no lock and no -shm) -> manager close DOES checkpoint -> at rest!
		ho, err := m.Acquire(ctx, "o")
		if err != nil {
			fatal("acq o", err)
		}
		if _, err := ho.DB.ExecContext(ctx, "INSERT INTO t VALUES (11, 'ckpts-ok')"); err != nil {
			fatal("ins o", err)
		}
		ho.Release()
		chO := startChild(ctx, "child-wal-reader-opened-only", pOpenedOnly)
		evO, err := m.Evict("o")
		if err != nil {
			fatal("evict o", err)
		}
		chO.stop()

		_ = m.Close()

		c1OK := !evW.Status.AtRest && len(evW.Status.Problems) == 2 && !recW && afterW.AtRest
		c2OK := !evR.Status.AtRest && len(evR.Status.Problems) == 1 && strings.Contains(evR.Status.Problems[0], "-shm exists") && afterR.AtRest
		c3OK := evO.Status.AtRest
		report("ATR-01a",
			"evict WAL DB while another process holds: (1) active reader + WAL writes, (2) active reader + 0-byte WAL, (3) opened-only connection (no statement)",
			"(1) not at rest (-wal & -shm, record dropped) then at rest on child close; (2) not at rest (-shm); (3) at rest (no lock held yet)",
			fmt.Sprintf("active_write=(at_rest=%v,probs=%d,rec=%v,after=%v) active_ro=(at_rest=%v,probs=%v,after=%v) opened_only=(at_rest=%v)",
				evW.Status.AtRest, len(evW.Status.Problems), recW, afterW.AtRest,
				evR.Status.AtRest, evR.Status.Problems, afterR.AtRest, evO.Status.AtRest),
			c1OK && c2OK && c3OK)
	}

	// ATR-01b: Blind spots of file-existence CheckAtRest: WAL locking_mode=EXCLUSIVE (no -shm!) and rollback-mode reader.
	{
		dir := filepath.Join(scratch, "atr01b")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fatal("mkdir atr01b", err)
		}
		walExclPath := filepath.Join(dir, "wal-excl.db")
		seedDB(ctx, walExclPath, sqlitex.JournalWAL)
		chExcl := startChild(ctx, "child-wal-exclusive-reader", walExclPath)
		stExcl, err := connmgr.CheckAtRest(walExclPath)
		if err != nil {
			fatal("CheckAtRest excl", err)
		}
		_, shmStatErr := os.Stat(walExclPath + "-shm")
		chExcl.stop()

		rbPath := filepath.Join(dir, "rb-read.db")
		seedDB(ctx, rbPath, sqlitex.JournalDelete)
		chRB := startChild(ctx, "child-rollback-reader", rbPath)
		stRB, err := connmgr.CheckAtRest(rbPath)
		if err != nil {
			fatal("CheckAtRest rb", err)
		}
		chRB.stop()

		// Post-repair CheckAtRest probes SQLite's lock region (0x40000000, 512 bytes) with fcntl(F_GETLK):
		// it catches both an exclusive-mode WAL reader (despite shm_exists=false) and an active rollback-mode reader.
		shmExists := !errors.Is(shmStatErr, os.ErrNotExist)
		report("ATR-01b",
			"call connmgr.CheckAtRest while another process holds an open WAL connection with PRAGMA locking_mode=EXCLUSIVE (and contrast rollback-mode reader)",
			"WAL exclusive-mode reader has no -shm yet CheckAtRest catches both it and the active rollback-mode reader via F_GETLK lock probe (AtRest=false for both)",
			fmt.Sprintf("wal_exclusive_reader=(AtRest=%v,shm_exists=%v) rollback_reader=(AtRest=%v)",
				stExcl.AtRest, shmExists, stRB.AtRest),
			!stExcl.AtRest && !shmExists && !stRB.AtRest)
	}

	// ATR-02: Rollback-mode open *sql.Rows eviction hazard — false AtRest AND POSIX lock destruction!
	{
		dir := filepath.Join(scratch, "atr02")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fatal("mkdir atr02", err)
		}
		rbPath := filepath.Join(dir, "rb-rows.db")
		seedDB(ctx, rbPath, sqlitex.JournalDelete)

		m, err := connmgr.New(connmgr.Options{
			Capacity: 2,
			PathFor:  func(id string) (string, error) { return rbPath, nil },
			Open: func(ctx context.Context, path string) (*sqlitex.DB, error) {
				o := sqlitex.DefaultOptions()
				o.File.JournalMode = sqlitex.JournalDelete
				o.SingleConnection = true
				return sqlitex.Open(ctx, path, o)
			},
		})
		if err != nil {
			fatal("connmgr atr02", err)
		}
		h, err := m.Acquire(ctx, "rb")
		if err != nil {
			fatal("acq rb", err)
		}
		rows, err := h.DB.QueryContext(ctx, "SELECT id, v FROM t ORDER BY id")
		if err != nil {
			fatal("query rb", err)
		}
		if !rows.Next() {
			fatal("rows.Next", rows.Err())
		}
		// Before eviction: another process MUST be blocked by our POSIX SHARED lock.
		writeBeforeOK, codeBefore := runChildTryWrite(ctx, rbPath)

		// Release handle while rows is still open, then Evict!
		h.Release()
		ev, err := m.Evict("rb")
		if err != nil {
			fatal("evict rb", err)
		}

		// After eviction (while rows is STILL open!): test if CheckAtRest's fileSHA256
		// (os.Open + Close in this process) silently stripped our POSIX SHARED lock!
		writeAfterOK, codeAfter := runChildTryWrite(ctx, rbPath)

		_ = rows.Close()
		_ = m.Close()

		// Expected safe behavior: Evict must not report AtRest=true while a connection is still open,
		// and must never drop the live connection's POSIX SHARED lock (so writeAfterOK must be false).
		held := !ev.Status.AtRest && !writeBeforeOK && !writeAfterOK
		report("ATR-02",
			"evict a rollback-mode DB from connmgr while a *sql.Rows cursor is still open, then attempt a write from a second process",
			"Evict reports not-at-rest and preserves the open cursor's POSIX SHARED lock (second process gets SQLITE_BUSY both before and after)",
			fmt.Sprintf("evict_at_rest=%v child_write_before=(ok=%v,code=%d) child_write_after_evict=(ok=%v,code=%d)",
				ev.Status.AtRest, writeBeforeOK, codeBefore, writeAfterOK, codeAfter),
			held)
	}

	// ATR-03: connmgr.Manager stats accounting invariant (UnverifiedOpens + VerifiedReopens == Misses) on Open failures.
	{
		dir := filepath.Join(scratch, "atr03")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fatal("mkdir atr03", err)
		}
		goodPath := filepath.Join(dir, "good.db")
		seedDB(ctx, goodPath, sqlitex.JournalWAL)
		junkPath := filepath.Join(dir, "junk.db")
		if err := os.WriteFile(junkPath, []byte(strings.Repeat("not a sqlite file ", 20)), 0o644); err != nil {
			fatal("write junk", err)
		}

		var failNextOpen atomic.Bool
		m, err := connmgr.New(connmgr.Options{
			Capacity: 2,
			PathFor:  func(id string) (string, error) { return filepath.Join(dir, id+".db"), nil },
			Open: func(ctx context.Context, path string) (*sqlitex.DB, error) {
				if failNextOpen.Load() {
					return nil, errors.New("simulated transient open failure (e.g. EMFILE)")
				}
				o := sqlitex.DefaultOptions()
				o.SingleConnection = true
				return sqlitex.Open(ctx, path, o)
			},
		})
		if err != nil {
			fatal("connmgr atr03", err)
		}

		// 1. Acquire unrecorded junk DB -> Open fails.
		_, junkErr := m.Acquire(ctx, "junk")

		// 2. Acquire and evict good DB -> records its at-rest SHA256.
		hg, err := m.Acquire(ctx, "good")
		if err != nil {
			fatal("acq good", err)
		}
		hg.Release()
		if _, err := m.Evict("good"); err != nil {
			fatal("evict good", err)
		}

		// 3. Reopen good DB while Open fails (CheckAtRest passes, Open fails).
		failNextOpen.Store(true)
		_, reopenErr := m.Acquire(ctx, "good")
		stats := m.Stats()
		_ = m.Close()

		// Invariant checked by 8.3/main.go:322: VerifiedReopens + UnverifiedOpens == Misses!
		// Because verifyBeforeOpen increments UnverifiedOpens/VerifiedReopens BEFORE Open runs,
		// failed Opens inflate both counters while Misses stays 1.
		invariantHolds := junkErr != nil && reopenErr != nil &&
			stats.Misses == 1 && stats.OpenFailures == 2 &&
			stats.UnverifiedOpens+stats.VerifiedReopens == stats.Misses
		report("ATR-03",
			"trigger Open failures on an unrecorded junk file and on a recorded DB reopen, then check connmgr.Stats()",
			"UnverifiedOpens + VerifiedReopens == Misses (failed opens increment only OpenFailures, not UnverifiedOpens or VerifiedReopens)",
			fmt.Sprintf("misses=%d open_failures=%d unverified_opens=%d verified_reopens=%d sum=%d",
				stats.Misses, stats.OpenFailures, stats.UnverifiedOpens, stats.VerifiedReopens,
				stats.UnverifiedOpens+stats.VerifiedReopens),
			invariantHolds)
	}

	// ATR-04: connmgr.Manager.Acquire with pre-cancelled context when at capacity.
	{
		dir := filepath.Join(scratch, "atr04")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fatal("mkdir atr04", err)
		}
		seedDB(ctx, filepath.Join(dir, "a.db"), sqlitex.JournalWAL)
		seedDB(ctx, filepath.Join(dir, "b.db"), sqlitex.JournalWAL)

		m, err := connmgr.New(connmgr.Options{
			Capacity: 1,
			PathFor:  func(id string) (string, error) { return filepath.Join(dir, id+".db"), nil },
			Open: func(ctx context.Context, path string) (*sqlitex.DB, error) {
				o := sqlitex.DefaultOptions()
				o.SingleConnection = true
				return sqlitex.Open(ctx, path, o)
			},
		})
		if err != nil {
			fatal("connmgr atr04", err)
		}
		ha, err := m.Acquire(ctx, "a")
		if err != nil {
			fatal("acq a", err)
		}
		ha.Release()

		cctx, cancel := context.WithCancel(ctx)
		cancel()
		_, acqErr := m.Acquire(cctx, "b")
		openAfter := m.OpenIDs()
		evictionsAfter := m.Stats().Evictions
		_ = m.Close()

		held := errors.Is(acqErr, context.Canceled) && evictionsAfter == 0 && len(openAfter) == 1 && openAfter[0] == "a"
		report("ATR-04",
			"call connmgr.Acquire(cancelledCtx, \"b\") when Capacity=1 holds healthy idle DB \"a\"",
			"fails immediately with context.Canceled without evicting healthy DB \"a\" (evictions=0, open=[a])",
			fmt.Sprintf("err=%v evictions=%d open_ids=%v", acqErr, evictionsAfter, openAfter),
			held)
	}

	// ATR-05: connmgr.CheckAtRest boundary cases (directory, header-only WAL, WAL without -shm, cold/hot/0-byte journal).
	{
		dir := filepath.Join(scratch, "atr05")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fatal("mkdir atr05", err)
		}

		// 1. Directory at DB path -> error (must not report AtRest).
		dirAsDB := filepath.Join(dir, "is-a-dir.db")
		if err := os.MkdirAll(dirAsDB, 0o755); err != nil {
			fatal("mkdir dirAsDB", err)
		}
		stDir, errDir := connmgr.CheckAtRest(dirAsDB)
		ok1 := errDir != nil && !stDir.AtRest

		// 2. 32-byte header-only -wal without -shm -> AtRest=false (conservative).
		pHdrWal := filepath.Join(dir, "hdrwal.db")
		seedDB(ctx, pHdrWal, sqlitex.JournalWAL)
		if err := os.WriteFile(pHdrWal+"-wal", make([]byte, 32), 0o644); err != nil {
			fatal("write hdrwal", err)
		}
		stHdrWal, errHdrWal := connmgr.CheckAtRest(pHdrWal)
		ok2 := errHdrWal == nil && !stHdrWal.AtRest && len(stHdrWal.Problems) == 1 && strings.Contains(stHdrWal.Problems[0], "-wal holds 32 bytes")

		// 3. Non-empty -wal with -shm removed -> still caught!
		pNoShm := filepath.Join(dir, "noshm.db")
		seedDB(ctx, pNoShm, sqlitex.JournalWAL)
		if err := os.WriteFile(pNoShm+"-wal", make([]byte, 4152), 0o644); err != nil {
			fatal("write noshm wal", err)
		}
		stNoShm, errNoShm := connmgr.CheckAtRest(pNoShm)
		ok3 := errNoShm == nil && !stNoShm.AtRest && strings.Contains(stNoShm.Problems[0], "-wal holds 4152 bytes")

		// 4. 0-byte -journal (TRUNCATE mode) and zero-header -journal (PERSIST mode) -> AtRest=true; non-zero header -> AtRest=false.
		pJrnl := filepath.Join(dir, "jrnl.db")
		seedDB(ctx, pJrnl, sqlitex.JournalDelete)
		if err := os.WriteFile(pJrnl+"-journal", nil, 0o644); err != nil {
			fatal("write 0-jrnl", err)
		}
		stZeroJrnl, errZeroJrnl := connmgr.CheckAtRest(pJrnl)
		if err := os.WriteFile(pJrnl+"-journal", make([]byte, 512), 0o644); err != nil {
			fatal("write cold-jrnl", err)
		}
		stColdJrnl, errColdJrnl := connmgr.CheckAtRest(pJrnl)
		hotBytes := make([]byte, 512)
		hotBytes[0] = 0xd9
		if err := os.WriteFile(pJrnl+"-journal", hotBytes, 0o644); err != nil {
			fatal("write hot-jrnl", err)
		}
		stHotJrnl, errHotJrnl := connmgr.CheckAtRest(pJrnl)
		ok4 := errZeroJrnl == nil && stZeroJrnl.AtRest &&
			errColdJrnl == nil && stColdJrnl.AtRest &&
			errHotJrnl == nil && !stHotJrnl.AtRest && strings.Contains(stHotJrnl.Problems[0], "hot -journal")

		report("ATR-05",
			"test connmgr.CheckAtRest on directory path, 32-byte header-only -wal, non-empty -wal without -shm, and 0-byte/cold/hot -journal",
			"directory returns error; 32B -wal and -wal without -shm report not-at-rest; 0-byte and zeroed -journal report at-rest; hot -journal reports not-at-rest",
			fmt.Sprintf("dir=(err=%v,at_rest=%v) hdr32_wal=(at_rest=%v) noshm_wal=(at_rest=%v) jrnl=(zero=%v,cold=%v,hot=%v)",
				errDir != nil, stDir.AtRest, stHdrWal.AtRest, stNoShm.AtRest, stZeroJrnl.AtRest, stColdJrnl.AtRest, stHotJrnl.AtRest),
			ok1 && ok2 && ok3 && ok4)
	}
}
