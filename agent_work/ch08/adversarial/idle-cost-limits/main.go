// Group 2 (idle-cost-limits): adversarial attacks on Experiment 8.2
// (idle-cost) and internal/procstat.
package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"

	"github.com/nickstrad/sqlite-internals/internal/procstat"
	"github.com/nickstrad/sqlite-internals/internal/recipe"
	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
	_ "modernc.org/sqlite"
)

const uncountedBudget = 256 * 1024

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

func fdCount() int {
	n, err := procstat.DescriptorCount()
	if err != nil {
		fatal("DescriptorCount", err)
	}
	return n
}

func settledMemory() procstat.Memory {
	runtime.GC()
	debug.FreeOSMemory()
	m, err := procstat.ReadMemory()
	if err != nil {
		fatal("ReadMemory", err)
	}
	return m
}

func readDBStat(ctx context.Context, conn *sql.Conn, op sqlitex.DBStatusOp) int64 {
	st, err := sqlitex.ReadDBStatus(ctx, conn, op, false)
	if err != nil {
		fatal("ReadDBStatus", err)
	}
	return int64(st.Current)
}

func childFDExhaust(dir string, target int, limit uint64) {
	if err := setNoFileLimit(limit); err != nil {
		fatal("setNoFileLimit", err)
	}
	ctx := context.Background()
	type held struct {
		db   *sql.DB
		conn *sql.Conn
	}
	var conns []held
	defer func() {
		for _, c := range conns {
			closePlain(c.db, c.conn)
		}
	}()
	var failErr error
	var failCode int
	var peakFDs int
	for i := range target {
		if n, err := procstat.DescriptorCount(); err == nil && n > peakFDs {
			peakFDs = n
		}
		p := filepath.Join(dir, fmt.Sprintf("w-%04d.db", i))
		uri, err := sqlitex.FileURI(p, nil)
		if err != nil {
			failErr = err
			break
		}
		db, err := sql.Open("sqlite", uri)
		if err != nil {
			failErr = err
			break
		}
		db.SetMaxOpenConns(1)
		conn, err := db.Conn(ctx)
		if err != nil {
			db.Close()
			failErr = err
			failCode, _ = sqlitex.ErrorCode(err)
			break
		}
		var n int
		if err := conn.QueryRowContext(ctx, "SELECT count(*) FROM t").Scan(&n); err != nil {
			conn.Close()
			db.Close()
			failErr = err
			failCode, _ = sqlitex.ErrorCode(err)
			break
		}
		conns = append(conns, held{db: db, conn: conn})
	}
	if n, err := procstat.DescriptorCount(); err == nil && n > peakFDs {
		peakFDs = n
	}
	errText := "none"
	if failErr != nil {
		errText = strings.ReplaceAll(failErr.Error(), "\n", " ")
	}
	fmt.Printf("EXHAUST opened=%d fds=%d limit=%d code=%d err=%s\n", len(conns), peakFDs, limit, failCode, errText)
}

func main() {
	if len(os.Args) == 5 && os.Args[1] == "child-fd-exhaust" {
		target, err := strconv.Atoi(os.Args[3])
		if err != nil {
			fatal("parse target", err)
		}
		limit, err := strconv.ParseUint(os.Args[4], 10, 64)
		if err != nil {
			fatal("parse limit", err)
		}
		childFDExhaust(os.Args[2], target, limit)
		return
	}

	ctx := context.Background()
	repoRoot, err := sqlitex.RepoRoot()
	if err != nil {
		fatal("repo root", err)
	}
	scratch := filepath.Join(repoRoot, "_work", "adv-ch08", "idle-cost-limits")
	if err := os.RemoveAll(scratch); err != nil {
		fatal("clean scratch", err)
	}
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		fatal("mkdir scratch", err)
	}

	// IDL-01: 1,000 WAL databases against a low process descriptor limit (RLIMIT_NOFILE=1024) (OUTLINE 8.2).
	{
		if !rlimitSupported {
			report("IDL-01",
				"open 1,000 WAL databases under RLIMIT_NOFILE=1024",
				"Linux-only Setrlimit check (skipped on non-Linux)",
				"skipped on "+runtime.GOOS,
				true)
		} else {
			dir := filepath.Join(scratch, "idl01")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				fatal("mkdir idl01", err)
			}
			src := filepath.Join(dir, "template.db")
			opts := sqlitex.DefaultOptions()
			opts.SingleConnection = true
			db, err := sqlitex.Open(ctx, src, opts)
			if err != nil {
				fatal("create template", err)
			}
			if _, err := db.ExecContext(ctx, "CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT); INSERT INTO t VALUES (1, 'ok')"); err != nil {
				fatal("seed template", err)
			}
			if err := db.Close(); err != nil {
				fatal("close template", err)
			}
			img, err := os.ReadFile(src)
			if err != nil {
				fatal("read template", err)
			}
			for i := range 1000 {
				if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("w-%04d.db", i)), img, 0o644); err != nil {
					fatal("write copy", err)
				}
			}
			selfExe, err := os.Executable()
			if err != nil {
				fatal("executable", err)
			}
			cmd := exec.CommandContext(ctx, selfExe, "child-fd-exhaust", dir, "1000", "1024")
			out, err := cmd.CombinedOutput()
			if err != nil {
				fatal("child-fd-exhaust", fmt.Errorf("%v: %s", err, out))
			}
			line := strings.TrimSpace(string(out))
			var opened, fds, limit, code int
			var errMsg string
			if idx := strings.Index(line, "err="); idx >= 0 {
				errMsg = line[idx+4:]
				fmt.Sscanf(line[:idx], "EXHAUST opened=%d fds=%d limit=%d code=%d", &opened, &fds, &limit, &code)
			}
			// With 3 FDs per WAL DB and RLIMIT_NOFILE=1024, max open connections is floor((1024 - baseline)/3) ~ 335..341.
			held := opened > 300 && opened <= 342 && (code&0xff) == 14
			report("IDL-01",
				"attempt to open 1,000 WAL databases in a child process with RLIMIT_NOFILE=1024",
				"exhausts file descriptors near 1024/3 (~335-341 connections) and fails with SQLITE_CANTOPEN(14) well before 1,000",
				fmt.Sprintf("opened=%d/1000 peak_fds=%d/%d sqlite_code=%d err=%q", opened, fds, limit, code, errMsg),
				held)
		}
	}

	// IDL-02: Experiment 8.2 at -n 1000 under default limit (OUTLINE 8.2).
	{
		expWork := filepath.Join(scratch, "exp82-n1000")
		cmd := exec.CommandContext(ctx, "go", "run", "./ch08-connection-and-database-lifecycle/experiments/8.2-idle-cost", "-n", "1000")
		cmd.Dir = repoRoot
		cmd.Env = append(os.Environ(), "COURSE_WORK_DIR="+expWork)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		err := cmd.Run()
		exitCode := 0
		if err != nil {
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				exitCode = ee.ExitCode()
			} else {
				fatal("run 8.2 -n 1000", err)
			}
		}
		outStr := stdout.String()
		has3000 := strings.Contains(outStr, "descriptors held: 3000 (3.00 per connection)")
		passed := exitCode == 0 && strings.Contains(outStr, "all checks passed")
		var memInfo string
		for _, l := range strings.Split(outStr, "\n") {
			if strings.Contains(l, "memory the three counters do not report:") {
				memInfo = strings.TrimSpace(l)
			}
		}
		report("IDL-02",
			"run Experiment 8.2 with -n 1000 idle WAL connections under default RLIMIT_NOFILE",
			"exits 0 with exactly 3000 descriptors held and passes the 256 KiB/conn uncounted memory margin",
			fmt.Sprintf("exit=%d exact_3000_fds=%v all_passed=%v (%s)", exitCode, has3000, passed, memInfo),
			passed && has3000)
	}

	// IDL-03: Large schemas (50 connections x 1,000 tables each + 5 prepared statements) (OUTLINE 8.2).
	{
		dir := filepath.Join(scratch, "idl03")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fatal("mkdir idl03", err)
		}
		const nConns = 50
		const nTables = 1000
		src := filepath.Join(dir, "large-schema.db")
		opts := sqlitex.DefaultOptions()
		opts.SingleConnection = true
		sdb, err := sqlitex.Open(ctx, src, opts)
		if err != nil {
			fatal("create large schema", err)
		}
		tx, err := sdb.BeginTx(ctx, nil)
		if err != nil {
			fatal("begin large schema", err)
		}
		for i := 1; i <= nTables; i++ {
			q := fmt.Sprintf("CREATE TABLE t%04d(id INTEGER PRIMARY KEY, name TEXT NOT NULL, amount INTEGER NOT NULL, note TEXT)", i)
			if _, err := tx.ExecContext(ctx, q); err != nil {
				fatal("create table", err)
			}
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO t0001 VALUES (1, 'a', 10, 'n')"); err != nil {
			fatal("seed t0001", err)
		}
		if err := tx.Commit(); err != nil {
			fatal("commit large schema", err)
		}
		if err := sdb.Close(); err != nil {
			fatal("close large schema", err)
		}
		img, err := os.ReadFile(src)
		if err != nil {
			fatal("read large schema", err)
		}
		paths := make([]string, nConns)
		for i := range nConns {
			paths[i] = filepath.Join(dir, fmt.Sprintf("ls-%03d.db", i))
			if err := os.WriteFile(paths[i], img, 0o644); err != nil {
				fatal("copy large schema", err)
			}
		}

		type holder struct {
			db    *sqlitex.DB
			conn  *sql.Conn
			stmts []*sql.Stmt
		}
		baseFDs := fdCount()
		baseMem := settledMemory()
		holders := make([]holder, nConns)
		for i, p := range paths {
			o := sqlitex.DefaultOptions()
			o.SingleConnection = true
			db, err := sqlitex.Open(ctx, p, o)
			if err != nil {
				fatal("open ls", err)
			}
			conn, err := db.Conn(ctx)
			if err != nil {
				fatal("conn ls", err)
			}
			var c int
			if err := conn.QueryRowContext(ctx, "SELECT count(*) FROM t0001").Scan(&c); err != nil {
				fatal("query ls", err)
			}
			stmts := make([]*sql.Stmt, 5)
			for j := range 5 {
				st, err := conn.PrepareContext(ctx, fmt.Sprintf("SELECT id, name, amount FROM t%04d WHERE id = ?", j+1))
				if err != nil {
					fatal("prepare ls", err)
				}
				stmts[j] = st
			}
			holders[i] = holder{db: db, conn: conn, stmts: stmts}
		}
		fdsHeld := fdCount() - baseFDs
		afterMem := settledMemory()
		growth := afterMem.RSS - baseMem.RSS

		var sumCache, sumSchema, sumStmt int64
		for _, h := range holders {
			sumCache += readDBStat(ctx, h.conn, sqlitex.DBStatusCacheUsed)
			sumSchema += readDBStat(ctx, h.conn, sqlitex.DBStatusSchemaUsed)
			sumStmt += readDBStat(ctx, h.conn, sqlitex.DBStatusStmtUsed)
		}
		sumTotal := sumCache + sumSchema + sumStmt
		uncounted := growth - sumTotal
		uncountedPerConn := uncounted / int64(nConns)

		// IDL-03a: Check 8.2's memory bound (growth >= sum && uncounted <= 256 KiB/conn) under large 1,000-table schemas.
		memBoundHeld := fdsHeld == 3*nConns && growth >= sumTotal && uncounted <= int64(nConns)*uncountedBudget
		report("IDL-03a",
			fmt.Sprintf("open %d WAL connections each with %d-table schema and 5 prepared statements, check 8.2's 256 KiB/conn uncounted memory margin", nConns, nTables),
			"descriptors = 3*N, RSS growth >= sum(CACHE+SCHEMA+STMT), and uncounted <= 256 KiB/conn",
			fmt.Sprintf("fds=%d/%d schema_per_conn=%dB cache_per_conn=%dB stmt_per_conn=%dB sum=%.1fMiB rss_growth=%.1fMiB uncounted_per_conn=%.0fKiB",
				fdsHeld, 3*nConns, sumSchema/nConns, sumCache/nConns, sumStmt/nConns,
				float64(sumTotal)/(1<<20), float64(growth)/(1<<20), float64(uncountedPerConn)/1024),
			memBoundHeld)

		// IDL-03b: PRAGMA shrink_memory on conn 0 drops cache across all 50 connections without touching large SCHEMA_USED or STMT_USED.
		if _, err := holders[0].conn.ExecContext(ctx, "PRAGMA shrink_memory"); err != nil {
			fatal("shrink ls", err)
		}
		var afterCache, afterSchema, afterStmt int64
		for _, h := range holders {
			afterCache += readDBStat(ctx, h.conn, sqlitex.DBStatusCacheUsed)
			afterSchema += readDBStat(ctx, h.conn, sqlitex.DBStatusSchemaUsed)
			afterStmt += readDBStat(ctx, h.conn, sqlitex.DBStatusStmtUsed)
		}
		shrinkHeld := afterSchema == sumSchema && afterStmt == sumStmt && afterCache*8 < sumCache
		report("IDL-03b",
			"run PRAGMA shrink_memory on connection 0 across 50 connections with 1,000-table schemas",
			"CACHE_USED drops to < 1/8 across all 50 connections while large SCHEMA_USED and STMT_USED remain unchanged",
			fmt.Sprintf("cache_sum=%d->%d schema_unchanged=%v(%d) stmt_unchanged=%v(%d)",
				sumCache, afterCache, afterSchema == sumSchema, afterSchema, afterStmt == sumStmt, afterStmt),
			shrinkHeld)

		for _, h := range holders {
			for _, st := range h.stmts {
				st.Close()
			}
			h.conn.Close()
			h.db.Close()
		}
	}

	// IDL-04: PRAGMA shrink_memory with dirty/pinned pages in an active write transaction.
	{
		dir := filepath.Join(scratch, "idl04")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fatal("mkdir idl04", err)
		}
		p := filepath.Join(dir, "pinned.db")
		if err := recipe.Default().Build(ctx, p, 3); err != nil {
			fatal("build recipe", err)
		}
		opts := sqlitex.DefaultOptions()
		opts.SingleConnection = true
		db, err := sqlitex.Open(ctx, p, opts)
		if err != nil {
			fatal("open idl04", err)
		}
		conn, err := db.Conn(ctx)
		if err != nil {
			fatal("conn idl04", err)
		}
		var cnt int
		if err := conn.QueryRowContext(ctx, "SELECT count(*) FROM documents").Scan(&cnt); err != nil {
			fatal("scan docs", err)
		}
		// Start a write transaction and dirty every document page so pages are pinned in pcache.
		if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
			fatal("begin", err)
		}
		if _, err := conn.ExecContext(ctx, "UPDATE documents SET body = body || '!'"); err != nil {
			fatal("update docs", err)
		}
		cacheDirty := readDBStat(ctx, conn, sqlitex.DBStatusCacheUsed)
		if _, err := conn.ExecContext(ctx, "PRAGMA shrink_memory"); err != nil {
			fatal("shrink while dirty", err)
		}
		cacheAfterDirtyShrink := readDBStat(ctx, conn, sqlitex.DBStatusCacheUsed)
		if _, err := conn.ExecContext(ctx, "ROLLBACK"); err != nil {
			fatal("rollback", err)
		}
		if _, err := conn.ExecContext(ctx, "PRAGMA shrink_memory"); err != nil {
			fatal("shrink after rollback", err)
		}
		cacheAfterRollbackShrink := readDBStat(ctx, conn, sqlitex.DBStatusCacheUsed)
		conn.Close()
		db.Close()

		// Dirty/pinned pages cannot be discarded by shrink_memory; only after ROLLBACK does cache drop < 1/8.
		held := cacheAfterDirtyShrink*8 >= cacheDirty && cacheAfterRollbackShrink*8 < cacheDirty
		report("IDL-04",
			"call PRAGMA shrink_memory while an active write transaction holds dirty pages, then after ROLLBACK",
			"dirty/pinned pages resist shrink_memory (CACHE_USED stays >= 1/8); after ROLLBACK shrink_memory drops CACHE_USED < 1/8",
			fmt.Sprintf("dirty=%dB after_shrink_while_dirty=%dB after_rollback_and_shrink=%dB",
				cacheDirty, cacheAfterDirtyShrink, cacheAfterRollbackShrink),
			held)
	}

	// IDL-05: Descriptor parking deep dive (multi-connection same-file in one process) (OUTLINE 8.2).
	{
		dir := filepath.Join(scratch, "idl05")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fatal("mkdir idl05", err)
		}

		// IDL-05a: WAL mode — sequential reuse (10 open/close cycles) and simultaneous parking (5 conns -> close 4 -> close 1).
		walPath := filepath.Join(dir, "park-wal.db")
		wOpts := sqlitex.DefaultOptions()
		wOpts.SingleConnection = true
		initDB, err := sqlitex.Open(ctx, walPath, wOpts)
		if err != nil {
			fatal("init park wal", err)
		}
		if _, err := initDB.ExecContext(ctx, "CREATE TABLE t(x); INSERT INTO t VALUES(1)"); err != nil {
			fatal("seed park wal", err)
		}
		initDB.Close()

		base := fdCount()
		db1, c1 := openPlain(ctx, walPath)
		var dummy int
		if err := c1.QueryRowContext(ctx, "SELECT count(*) FROM t").Scan(&dummy); err != nil {
			fatal("read c1", err)
		}
		fdsC1 := fdCount() - base

		maxDuringSeq := 0
		maxAfterSeqClose := 0
		for range 10 {
			db2, c2 := openPlain(ctx, walPath)
			if err := c2.QueryRowContext(ctx, "SELECT count(*) FROM t").Scan(&dummy); err != nil {
				fatal("read seq", err)
			}
			maxDuringSeq = max(maxDuringSeq, fdCount()-base)
			closePlain(db2, c2)
			maxAfterSeqClose = max(maxAfterSeqClose, fdCount()-base)
		}

		// Open 4 more simultaneous connections (5 total).
		type pair struct {
			db *sql.DB
			c  *sql.Conn
		}
		var extra []pair
		for range 4 {
			d, c := openPlain(ctx, walPath)
			if err := c.QueryRowContext(ctx, "SELECT count(*) FROM t").Scan(&dummy); err != nil {
				fatal("read sim", err)
			}
			extra = append(extra, pair{d, c})
		}
		fdsFive := fdCount() - base
		for _, p := range extra {
			closePlain(p.db, p.c)
		}
		fdsAfterCloseFour := fdCount() - base
		closePlain(db1, c1)
		fdsAfterAll := fdCount() - base

		// 1 conn = 3; sequential open/close reuses the single parked db fd -> peak 5, after close 4 (not 13).
		// 5 simultaneous = 1 shm + 5 db + 5 wal = 11; close 4 -> 1 shm + 5 db (4 parked) + 1 wal = 7; close last -> 0.
		held5a := fdsC1 == 3 && maxDuringSeq == 5 && maxAfterSeqClose == 4 &&
			fdsFive == 11 && fdsAfterCloseFour == 7 && fdsAfterAll == 0
		report("IDL-05a",
			"WAL same-file multi-connection: 10 sequential open/close cycles while conn1 stays open, then 5 simultaneous -> close 4 -> close last",
			"findReusableFd caps sequential cycles at 5 open / 4 parked (not 13); 5 simultaneous yield 11 -> 7 parked -> 0 on last close",
			fmt.Sprintf("c1=%d seq_peak=%d seq_after=%d five=%d after_close_4=%d after_last=%d",
				fdsC1, maxDuringSeq, maxAfterSeqClose, fdsFive, fdsAfterCloseFour, fdsAfterAll),
			held5a)

		// IDL-05b: Rollback mode (DELETE) — parking only happens when another connection in the process holds a POSIX lock.
		rbPath := filepath.Join(dir, "park-delete.db")
		rbOpts := sqlitex.DefaultOptions()
		rbOpts.File.JournalMode = sqlitex.JournalDelete
		rbOpts.SingleConnection = true
		rInit, err := sqlitex.Open(ctx, rbPath, rbOpts)
		if err != nil {
			fatal("init park rb", err)
		}
		if _, err := rInit.ExecContext(ctx, "CREATE TABLE t(x); INSERT INTO t VALUES(1)"); err != nil {
			fatal("seed park rb", err)
		}
		rInit.Close()

		base = fdCount()
		rb1, rc1 := openPlain(ctx, rbPath)
		if err := rc1.QueryRowContext(ctx, "SELECT count(*) FROM t").Scan(&dummy); err != nil {
			fatal("rb read1", err)
		}
		rbIdle1 := fdCount() - base
		rb2, rc2 := openPlain(ctx, rbPath)
		if err := rc2.QueryRowContext(ctx, "SELECT count(*) FROM t").Scan(&dummy); err != nil {
			fatal("rb read2", err)
		}
		rbTwo := fdCount() - base
		closePlain(rb2, rc2)
		rbAfterIdleClose := fdCount() - base

		// Now hold a SHARED lock on rc1 inside an explicit read transaction, then open and close rb3.
		if _, err := rc1.ExecContext(ctx, "BEGIN"); err != nil {
			fatal("rb begin", err)
		}
		if err := rc1.QueryRowContext(ctx, "SELECT count(*) FROM t").Scan(&dummy); err != nil {
			fatal("rb tx read", err)
		}
		rb3, rc3 := openPlain(ctx, rbPath)
		if err := rc3.QueryRowContext(ctx, "SELECT count(*) FROM t").Scan(&dummy); err != nil {
			fatal("rb read3", err)
		}
		closePlain(rb3, rc3)
		rbAfterLockedClose := fdCount() - base
		if _, err := rc1.ExecContext(ctx, "COMMIT"); err != nil {
			fatal("rb commit", err)
		}
		rbAfterCommit := fdCount() - base
		closePlain(rb1, rc1)
		rbAfterLast := fdCount() - base

		held5b := rbIdle1 == 1 && rbTwo == 2 && rbAfterIdleClose == 1 &&
			rbAfterLockedClose == 2 && rbAfterCommit == 1 && rbAfterLast == 0
		report("IDL-05b",
			"rollback-mode (DELETE) same-file descriptor parking: conn1 idle vs conn1 holding SHARED lock during read transaction",
			"idle conn1 does not park conn2's fd (1->2->1); locked conn1 DOES park conn3's fd (2) until COMMIT releases lock (1->0)",
			fmt.Sprintf("idle_seq=%d->%d->%d locked_after_close=%d after_commit=%d after_last=%d",
				rbIdle1, rbTwo, rbAfterIdleClose, rbAfterLockedClose, rbAfterCommit, rbAfterLast),
			held5b)
	}

	// IDL-06: internal/procstat parser overflow & path edge cases.
	{
		// IDL-06a: int64 * 1024 overflow in ParseStatus and ParsePSRSS.
		const overflowNegKiB = "9007199254740992"   // 2^53; fits in int64, *1024 wraps to -2^63
		const overflowZeroKiB = "18014398509481984" // 2^54; fits in int64, *1024 wraps to 0
		stMem, stErr := procstat.ParseStatus("VmRSS: " + overflowZeroKiB + " kB\nRssAnon: " + overflowNegKiB + " kB\n")
		psMem, psErr := procstat.ParsePSRSS(overflowNegKiB + "\n")
		held6a := stErr != nil && psErr != nil
		report("IDL-06a",
			"pass int64*1024 overflowing KiB values (2^54 and 2^53) to procstat.ParseStatus and procstat.ParsePSRSS",
			"both parsers reject overflowing KiB sizes with an error instead of wrapping to 0 or negative bytes",
			fmt.Sprintf("ParseStatus=(rss=%d,anon=%d,err=%v) ParsePSRSS=(rss=%d,err=%v)", stMem.RSS, stMem.Anon, stErr, psMem.RSS, psErr),
			held6a)

		// IDL-06b: DescriptorsUnder("/") and DescriptorsUnder with a relative path (Linux only).
		if !procstat.TargetsSupported {
			report("IDL-06b",
				"call procstat.DescriptorsUnder(\"/\") and DescriptorsUnder with a relative directory path",
				"Linux-only /proc/self/fd target check (skipped on non-Linux)",
				"skipped on "+runtime.GOOS,
				true)
		} else {
			relDir := filepath.Join(scratch, "idl06-rel")
			if err := os.MkdirAll(relDir, 0o755); err != nil {
				fatal("mkdir idl06-rel", err)
			}
			f, err := os.Create(filepath.Join(relDir, "probe.txt"))
			if err != nil {
				fatal("create probe", err)
			}
			defer f.Close()

			rootMatches, rootErr := procstat.DescriptorsUnder("/")
			relPath, err := filepath.Rel(repoRoot, relDir)
			if err != nil {
				fatal("rel path", err)
			}
			// Run relative lookup from repoRoot.
			origWD, _ := os.Getwd()
			_ = os.Chdir(repoRoot)
			relMatches, relErr := procstat.DescriptorsUnder(relPath)
			_ = os.Chdir(origWD)

			held6b := rootErr == nil && len(rootMatches) > 0 && relErr == nil && len(relMatches) == 1
			report("IDL-06b",
				"call procstat.DescriptorsUnder(\"/\") and procstat.DescriptorsUnder(relativePath) while a file is open inside relativePath",
				"DescriptorsUnder(\"/\") finds open files under root (>0) and DescriptorsUnder(relativePath) resolves relative dir to find probe.txt (1)",
				fmt.Sprintf("root_matches=%d(err=%v) rel_path=%q rel_matches=%d(err=%v)",
					len(rootMatches), rootErr, relPath, len(relMatches), relErr),
				held6b)
		}
	}
}
