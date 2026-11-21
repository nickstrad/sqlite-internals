// Command sqlitex-settings tests internal/sqlitex's per-connection and file
// settings enforcement under concurrency (50 simultaneous pooled connections),
// write-lock contention during pool growth, ReadOnly pools, ImmediateTx, and
// edge-case setting values.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nickstrad/walfleet/internal/sqlitex"
)

func sanitize(s string) string {
	s = strings.ReplaceAll(s, "|", "/")
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ; ")
	return strings.TrimSpace(s)
}

func report(id, tried, expect, got string, held bool) {
	verdict := "BROKEN"
	if held {
		verdict = "HELD"
	}
	fmt.Printf("ATTACK %s | %s | EXPECT %s | GOT %s | %s\n",
		id, sanitize(tried), sanitize(expect), sanitize(got), verdict)
}

func fatal(msg string, err error) {
	fmt.Fprintf(os.Stderr, "HARNESS ERROR: %s: %v\n", msg, err)
	os.Exit(2)
}

type rawPragmas struct {
	synchronous       int64
	foreignKeys       int64
	secureDelete      int64
	cacheSize         int64
	walAutocheckpoint int64
	busyTimeout       int64
	journalMode       string
}

func readRawPragmas(ctx context.Context, conn *sql.Conn) (rawPragmas, error) {
	var p rawPragmas
	queries := []struct {
		sql string
		dst any
	}{
		{"PRAGMA synchronous", &p.synchronous},
		{"PRAGMA foreign_keys", &p.foreignKeys},
		{"PRAGMA secure_delete", &p.secureDelete},
		{"PRAGMA cache_size", &p.cacheSize},
		{"PRAGMA wal_autocheckpoint", &p.walAutocheckpoint},
		{"PRAGMA busy_timeout", &p.busyTimeout},
		{"PRAGMA journal_mode", &p.journalMode},
	}
	for _, q := range queries {
		if err := conn.QueryRowContext(ctx, q.sql).Scan(q.dst); err != nil {
			return p, fmt.Errorf("%s: %w", q.sql, err)
		}
	}
	return p, nil
}

// runConcurrentPool opens n physical connections simultaneously, holds all n
// open at the same time, and reads back every pragma directly on each connection.
func runConcurrentPool(ctx context.Context, dbPath string, opts sqlitex.Options, n int, want rawPragmas) (openPeak, matched, connErrs, mismatches int, sampleErr string) {
	db, err := sqlitex.Open(ctx, dbPath, opts)
	if err != nil {
		return 0, 0, 1, 0, fmt.Sprintf("Open: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(n)
	db.SetMaxIdleConns(n)

	start := make(chan struct{})
	var acquiredWG sync.WaitGroup
	acquiredWG.Add(n)
	release := make(chan struct{})

	var wg sync.WaitGroup
	var errCount, matchCount, mismatchCount atomic.Int64
	var firstErrMu sync.Mutex
	var firstErr string
	recordErr := func(msg string) {
		firstErrMu.Lock()
		if firstErr == "" {
			firstErr = msg
		}
		firstErrMu.Unlock()
	}

	for i := range n {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			conn, cErr := db.Conn(ctx)
			if cErr != nil {
				errCount.Add(1)
				recordErr(fmt.Sprintf("conn[%d] acquire: %v", idx, cErr))
				acquiredWG.Done()
				return
			}
			got, qErr := readRawPragmas(ctx, conn)
			acquiredWG.Done()
			<-release
			conn.Close()

			if qErr != nil {
				errCount.Add(1)
				recordErr(fmt.Sprintf("conn[%d] query: %v", idx, qErr))
				return
			}
			gotNorm := got
			gotNorm.journalMode = strings.ToLower(gotNorm.journalMode)
			wantNorm := want
			wantNorm.journalMode = strings.ToLower(wantNorm.journalMode)
			if gotNorm != wantNorm {
				mismatchCount.Add(1)
				recordErr(fmt.Sprintf("conn[%d] got=%+v want=%+v", idx, gotNorm, wantNorm))
				return
			}
			matchCount.Add(1)
		}(i)
	}

	close(start)
	acquiredWG.Wait()
	stats := db.Stats()
	close(release)
	wg.Wait()

	return stats.OpenConnections, int(matchCount.Load()), int(errCount.Load()), int(mismatchCount.Load()), firstErr
}

func main() {
	repoRoot, err := sqlitex.RepoRoot()
	if err != nil {
		fatal("locate repo root", err)
	}
	workDir := filepath.Join(repoRoot, "_work", "adv-ch00", "sqlitex-settings")
	if err := os.RemoveAll(workDir); err != nil {
		fatal("clean work dir", err)
	}
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		fatal("mkdir work dir", err)
	}

	ctx := context.Background()

	// SET-01: 50 concurrent pooled connections in WAL mode with all non-default ConnSettings.
	opts01 := sqlitex.DefaultOptions()
	opts01.Conn = sqlitex.ConnSettings{
		Synchronous:       sqlitex.SyncNormal,
		ForeignKeys:       false,
		SecureDelete:      true,
		CacheSize:         -777,
		WALAutocheckpoint: 42,
		BusyTimeout:       3210 * time.Millisecond,
	}
	want01 := rawPragmas{
		synchronous:       1, // NORMAL
		foreignKeys:       0,
		secureDelete:      1,
		cacheSize:         -777,
		walAutocheckpoint: 42,
		busyTimeout:       3210,
		journalMode:       "wal",
	}
	// Seed the file first so concurrent opens hit a real populated database.
	dbPath01 := filepath.Join(workDir, "wal50.db")
	seedDB, err := sqlitex.Open(ctx, dbPath01, opts01)
	if err != nil {
		fatal("SET-01 seed open", err)
	}
	if _, err := seedDB.ExecContext(ctx, "CREATE TABLE t(id INTEGER PRIMARY KEY)"); err != nil {
		seedDB.Close()
		fatal("SET-01 seed create", err)
	}
	seedDB.Close()

	peak01, matched01, errs01, mis01, sample01 := runConcurrentPool(ctx, dbPath01, opts01, 50, want01)
	held01 := peak01 == 50 && matched01 == 50 && errs01 == 0 && mis01 == 0
	report(
		"SET-01",
		"50 concurrent pooled connections on WAL DB with all 6 non-default ConnSettings",
		"OpenConnections=50, 50/50 connections match all 6 per-connection pragmas + journal_mode=wal, 0 errors",
		fmt.Sprintf("openPeak=%d matched=%d/50 errs=%d mismatches=%d sampleErr=%q", peak01, matched01, errs01, mis01, sample01),
		held01,
	)

	// SET-02: 50 concurrent pooled connections with non-persistent rollback journal mode (TRUNCATE).
	opts02 := opts01
	opts02.File.JournalMode = sqlitex.JournalTruncate
	want02 := want01
	want02.journalMode = "truncate"
	dbPath02 := filepath.Join(workDir, "trunc50.db")
	seedDB2, err := sqlitex.Open(ctx, dbPath02, opts02)
	if err != nil {
		fatal("SET-02 seed open", err)
	}
	if _, err := seedDB2.ExecContext(ctx, "CREATE TABLE t(id INTEGER PRIMARY KEY)"); err != nil {
		seedDB2.Close()
		fatal("SET-02 seed create", err)
	}
	seedDB2.Close()

	peak02, matched02, errs02, mis02, sample02 := runConcurrentPool(ctx, dbPath02, opts02, 50, want02)
	held02 := peak02 == 50 && matched02 == 50 && errs02 == 0 && mis02 == 0
	report(
		"SET-02",
		"50 concurrent pooled connections with non-persistent journal_mode=truncate + all 6 non-default ConnSettings",
		"OpenConnections=50, 50/50 connections report journal_mode=truncate and all 6 pragmas, 0 errors",
		fmt.Sprintf("openPeak=%d matched=%d/50 errs=%d mismatches=%d sampleErr=%q", peak02, matched02, errs02, mis02, sample02),
		held02,
	)

	// SET-03: Opening a new pooled connection in WAL mode while another connection holds a write lock (BusyTimeout=0).
	opts03 := sqlitex.DefaultOptions()
	opts03.Conn.BusyTimeout = 0
	dbPath03 := filepath.Join(workDir, "wal-busy-open.db")
	db03, err := sqlitex.Open(ctx, dbPath03, opts03)
	if err != nil {
		fatal("SET-03 open", err)
	}
	db03.SetMaxOpenConns(4)
	if _, err := db03.ExecContext(ctx, "CREATE TABLE t(id INTEGER PRIMARY KEY); INSERT INTO t VALUES(1)"); err != nil {
		db03.Close()
		fatal("SET-03 seed", err)
	}
	writerConn, err := db03.Conn(ctx)
	if err != nil {
		db03.Close()
		fatal("SET-03 writerConn", err)
	}
	tx, err := writerConn.BeginTx(ctx, nil)
	if err != nil {
		writerConn.Close()
		db03.Close()
		fatal("SET-03 begin", err)
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO t VALUES(2)"); err != nil {
		tx.Rollback()
		writerConn.Close()
		db03.Close()
		fatal("SET-03 insert", err)
	}
	// While writerConn holds an open write transaction, acquire a brand-new pooled connection for reading.
	readerConn, readerConnErr := db03.Conn(ctx)
	var readCount int
	var readErr error
	if readerConnErr == nil {
		readErr = readerConn.QueryRowContext(ctx, "SELECT count(*) FROM t").Scan(&readCount)
		readerConn.Close()
	}
	tx.Rollback()
	writerConn.Close()
	db03.Close()
	held03 := readerConnErr == nil && readErr == nil && readCount == 1
	report(
		"SET-03",
		"open a new pooled connection in WAL mode (BusyTimeout=0) while another connection holds an active write transaction",
		"new pooled connection opens and reads snapshot (count=1) without SQLITE_BUSY",
		fmt.Sprintf("connErr=%v readErr=%v count=%d", readerConnErr, readErr, readCount),
		held03,
	)

	// SET-04: Sub-millisecond BusyTimeout accepted by Validate() vs truncated by Open().
	opts04 := sqlitex.DefaultOptions()
	opts04.Conn.BusyTimeout = 500 * time.Microsecond
	valErr := opts04.Conn.Validate()
	dbPath04 := filepath.Join(workDir, "subms.db")
	db04, openErr04 := sqlitex.Open(ctx, dbPath04, opts04)
	if db04 != nil {
		db04.Close()
	}
	// Either Validate rejects sub-millisecond durations upfront, or Open accepts what Validate accepted.
	held04 := (valErr != nil) || (openErr04 == nil)
	report(
		"SET-04",
		"ConnSettings with BusyTimeout=500us passed to Validate() and sqlitex.Open()",
		"Validate() rejects sub-millisecond BusyTimeout or Open() succeeds consistently (no post-apply mismatch)",
		fmt.Sprintf("validateErr=%v openErr=%v", valErr, openErr04),
		held04,
	)

	// SET-05: ReadOnly pool with 20 concurrent connections applies all 6 per-connection settings and refuses writes.
	opts05 := opts01
	opts05.ReadOnly = true
	peak05, matched05, errs05, mis05, sample05 := runConcurrentPool(ctx, dbPath01, opts05, 20, want01)
	roDB, err := sqlitex.Open(ctx, dbPath01, opts05)
	if err != nil {
		fatal("SET-05 reopen ro", err)
	}
	_, writeErr05 := roDB.ExecContext(ctx, "INSERT INTO t VALUES(999)")
	roDB.Close()
	held05 := peak05 == 20 && matched05 == 20 && errs05 == 0 && mis05 == 0 && writeErr05 != nil
	report(
		"SET-05",
		"20 concurrent pooled connections with ReadOnly=true and 6 non-default ConnSettings, plus write attempt",
		"OpenConnections=20, 20/20 match all 6 pragmas, and INSERT fails on ReadOnly DB",
		fmt.Sprintf("openPeak=%d matched=%d/20 errs=%d mismatches=%d writeErr=%v sampleErr=%q", peak05, matched05, errs05, mis05, writeErr05, sample05),
		held05,
	)

	// SET-06: ImmediateTx=true takes write lock at BeginTx (before any statement) across pooled connections.
	opts06 := sqlitex.DefaultOptions()
	opts06.ImmediateTx = true
	opts06.Conn.BusyTimeout = 0
	dbPath06 := filepath.Join(workDir, "immediate.db")
	db06, err := sqlitex.Open(ctx, dbPath06, opts06)
	if err != nil {
		fatal("SET-06 open", err)
	}
	defer db06.Close()
	db06.SetMaxOpenConns(2)
	if _, err := db06.ExecContext(ctx, "CREATE TABLE t(id INTEGER PRIMARY KEY)"); err != nil {
		fatal("SET-06 create", err)
	}
	c1, err := db06.Conn(ctx)
	if err != nil {
		fatal("SET-06 c1", err)
	}
	defer c1.Close()
	c2, err := db06.Conn(ctx)
	if err != nil {
		fatal("SET-06 c2", err)
	}
	defer c2.Close()
	tx1, err := c1.BeginTx(ctx, nil)
	if err != nil {
		fatal("SET-06 tx1", err)
	}
	defer tx1.Rollback()
	tx2, errTx2 := c2.BeginTx(ctx, nil)
	if tx2 != nil {
		tx2.Rollback()
	}
	held06 := errTx2 != nil && sqlitex.IsBusy(errTx2)
	report(
		"SET-06",
		"ImmediateTx=true with BusyTimeout=0: two pooled connections call BeginTx before executing any SQL",
		"second BeginTx fails immediately with SQLITE_BUSY",
		fmt.Sprintf("tx2Err=%v isBusy=%v", errTx2, sqlitex.IsBusy(errTx2)),
		held06,
	)
}
