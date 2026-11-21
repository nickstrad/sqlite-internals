// Group 3 (equal-falsify): Adversarial attacks on internal/equal (E2 pages.go,
// E3 logical.go, and dbhash.go): 13 blind-spot pairs vs dbhash, tricky SQLite
// values/storage classes/FTS5/WITHOUT ROWID FKs, default_cache_size blind spot
// vs VACUUM INTO, false refusal of ROWID INTEGER PRIMARY KEY tables, silent
// file mutation by LogicalHashFile/RunDBHash on uncheckpointed WAL databases,
// E2 range/boundary edge cases, and dbhash runner edge cases.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/nickstrad/sqlite-internals/internal/equal"
	"github.com/nickstrad/sqlite-internals/internal/recipe"
	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
	"github.com/nickstrad/sqlite-internals/internal/trace"
)

const childCopyFlag = "child-copy-wal"

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

func childCopyMain(srcDB, dstDB string) {
	for _, suf := range []string{"", "-wal"} {
		data, err := os.ReadFile(srcDB + suf)
		if err != nil {
			fmt.Fprintf(os.Stderr, "child read %s: %v\n", srcDB+suf, err)
			os.Exit(1)
		}
		if err := os.WriteFile(dstDB+suf, data, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "child write %s: %v\n", dstDB+suf, err)
			os.Exit(1)
		}
	}
}

func copyViaChild(srcDB, dstDB string) error {
	args, err := trace.SelfCommand(childCopyFlag, srcDB, dstDB)
	if err != nil {
		return err
	}
	out, err := exec.Command(args[0], args[1:]...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func buildCustomDB(ctx context.Context, path string, opts sqlitex.Options, stmts ...string) string {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		fatal("mkdir", err)
	}
	if err := recipe.RemoveDatabase(path); err != nil {
		fatal("remove", err)
	}
	opts.SingleConnection = true
	db, err := sqlitex.Open(ctx, path, opts)
	if err != nil {
		fatal("open "+path, err)
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			db.Close()
			fatal(s, err)
		}
	}
	if err := db.Close(); err != nil {
		fatal("close "+path, err)
	}
	return path
}

func buildDefaultDB(ctx context.Context, path string, stmts ...string) string {
	return buildCustomDB(ctx, path, sqlitex.DefaultOptions(), stmts...)
}

func vacuumInto(ctx context.Context, src, dst string) {
	if err := recipe.RemoveDatabase(dst); err != nil {
		fatal("remove", err)
	}
	db, err := equal.OpenExisting(ctx, src)
	if err != nil {
		fatal("OpenExisting", err)
	}
	if _, err := db.ExecContext(ctx, "VACUUM INTO ?", dst); err != nil {
		db.Close()
		fatal("VACUUM INTO", err)
	}
	if err := db.Close(); err != nil {
		fatal("close", err)
	}
}

func main() {
	if len(os.Args) == 4 && os.Args[1] == childCopyFlag {
		childCopyMain(os.Args[2], os.Args[3])
		return
	}

	ctx := context.Background()
	root, err := sqlitex.RepoRoot()
	if err != nil {
		fatal("RepoRoot", err)
	}
	scratch := filepath.Join(root, "_work", "adv-ch03", "equal-falsify")
	if err := os.RemoveAll(scratch); err != nil {
		fatal("clean scratch", err)
	}
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		fatal("mkdir scratch", err)
	}

	dbhashTool, dbhashErr := equal.LocateDBHash()
	hasDBHash := dbhashErr == nil

	// EQ-01: 13 blind-spot pairs (OUTLINE 3.3 bullet + 7 extra) comparing
	// equal.LogicalHash vs pinned dbhash.
	{
		type pairSpec struct {
			name   string
			optsA  sqlitex.Options
			optsB  sqlitex.Options
			stmtsA []string
			stmtsB []string
		}
		def := sqlitex.DefaultOptions()
		optAppID := def
		optAppID.File.ApplicationID = 424242
		optAVFull := def
		optAVFull.File.AutoVacuum = sqlitex.AutoVacuumFull
		optPage8k := def
		optPage8k.File.PageSize = 8192
		optUTF16 := def
		optUTF16.File.Encoding = "UTF-16le"
		optDel := def
		optDel.File.JournalMode = sqlitex.JournalDelete

		baseOneRow := []string{"CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT)", "INSERT INTO t VALUES(1, 'x')"}
		specs := []pairSpec{
			{"implicit-rowid", def, def,
				[]string{"CREATE TABLE t(v TEXT)", "INSERT INTO t(rowid, v) VALUES(1,'x'),(2,'y')"},
				[]string{"CREATE TABLE t(v TEXT)", "INSERT INTO t(rowid, v) VALUES(5,'x'),(9,'y')"}},
			{"sqlite_sequence", def, def,
				[]string{"CREATE TABLE t(id INTEGER PRIMARY KEY AUTOINCREMENT, v TEXT)", "INSERT INTO t(v) VALUES('x')"},
				[]string{"CREATE TABLE t(id INTEGER PRIMARY KEY AUTOINCREMENT, v TEXT)", "INSERT INTO t(v) VALUES('x'),('y')", "DELETE FROM t WHERE id=2"}},
			{"user_version", def, def,
				append(append([]string{}, baseOneRow...), "PRAGMA user_version=1"),
				append(append([]string{}, baseOneRow...), "PRAGMA user_version=2")},
			{"int1-vs-real1.0", def, def,
				[]string{"CREATE TABLE t(id INTEGER PRIMARY KEY, v)", "INSERT INTO t VALUES(1, 1)"},
				[]string{"CREATE TABLE t(id INTEGER PRIMARY KEY, v)", "INSERT INTO t VALUES(1, 1.0)"}},
			{"value-boundary", def, def,
				[]string{"CREATE TABLE t(id INTEGER PRIMARY KEY, a TEXT, b TEXT)", "INSERT INTO t VALUES(1, 'a3', 'b')"},
				[]string{"CREATE TABLE t(id INTEGER PRIMARY KEY, a TEXT, b TEXT)", "INSERT INTO t VALUES(1, 'a', '3b')"}},
			{"row-moved-table", def, def,
				[]string{"CREATE TABLE a(v TEXT)", "CREATE TABLE b(v TEXT)", "INSERT INTO a(rowid,v) VALUES(1,'x'),(2,'y')", "INSERT INTO b(rowid,v) VALUES(1,'z')"},
				[]string{"CREATE TABLE a(v TEXT)", "CREATE TABLE b(v TEXT)", "INSERT INTO a(rowid,v) VALUES(1,'x')", "INSERT INTO b(rowid,v) VALUES(1,'y'),(2,'z')"}},
			{"application_id", def, optAppID, baseOneRow, baseOneRow},
			{"auto_vacuum", def, optAVFull, baseOneRow, baseOneRow},
			{"page_size", def, optPage8k, baseOneRow, baseOneRow},
			{"encoding", def, optUTF16, baseOneRow, baseOneRow},
			{"journal_mode", def, optDel, baseOneRow, baseOneRow},
			{"sqlite_stat1-analyze", def, def,
				append(append([]string{}, baseOneRow...), "CREATE INDEX t_v ON t(v)"),
				append(append([]string{}, baseOneRow...), "CREATE INDEX t_v ON t(v)", "ANALYZE")},
			{"view-trigger-ddl", def, def,
				append(append([]string{}, baseOneRow...), "CREATE VIEW v AS SELECT id FROM t"),
				append(append([]string{}, baseOneRow...), "CREATE VIEW v AS SELECT id, v FROM t")},
		}

		var missed []string
		var dbhashBlind []string
		for i, sp := range specs {
			pa := buildCustomDB(ctx, filepath.Join(scratch, "eq01", fmt.Sprintf("%02d-a.db", i)), sp.optsA, sp.stmtsA...)
			pb := buildCustomDB(ctx, filepath.Join(scratch, "eq01", fmt.Sprintf("%02d-b.db", i)), sp.optsB, sp.stmtsB...)
			res, _, _, err := equal.CompareLogicalFiles(ctx, pa, pb)
			if err != nil {
				fatal("CompareLogical "+sp.name, err)
			}
			if res.Equal {
				missed = append(missed, sp.name)
			}
			if hasDBHash {
				ha, errA := equal.RunDBHash(ctx, dbhashTool, pa)
				hb, errB := equal.RunDBHash(ctx, dbhashTool, pb)
				if errA == nil && errB == nil && ha == hb {
					dbhashBlind = append(dbhashBlind, sp.name)
				}
			}
		}
		report("EQ-01",
			"13 logical-difference pairs (6 from 3.3 + application_id, auto_vacuum, page_size, encoding, journal_mode, ANALYZE/sqlite_stat1, VIEW DDL) across LogicalHash vs dbhash",
			"LogicalHash distinguishes all 13 pairs (0 missed)",
			fmt.Sprintf("LogicalHash_missed=%v (0/13), dbhash_present=%v, dbhash_blind_to=%d:%v",
				missed, hasDBHash, len(dbhashBlind), dbhashBlind),
			len(missed) == 0)
	}

	// EQ-02: Tricky values, storage classes, FTS5 shadow tables, and WITHOUT ROWID FK check.
	{
		dir := filepath.Join(scratch, "eq02")
		// 1. Seven distinct storage-class values in an untyped column must yield 7 distinct hashes.
		valExprs := []struct {
			name string
			expr string
		}{
			{"null", "NULL"},
			{"int1", "1"},
			{"real1", "1.0"},
			{"text1", "'1'"},
			{"blob1", "x'31'"},
			{"empty_text", "''"},
			{"empty_blob", "x''"},
			{"pos_inf", "1e999"},
			{"neg_inf", "-1e999"},
			{"int64_min", "-9223372036854775808"},
			{"int64_max", "9223372036854775807"},
			{"nul_text_b", "'a' || char(0) || 'b'"},
			{"nul_text_c", "'a' || char(0) || 'c'"},
		}
		seenHash := map[string]string{}
		var collisions []string
		for _, v := range valExprs {
			p := buildDefaultDB(ctx, filepath.Join(dir, "val-"+v.name+".db"),
				"CREATE TABLE t(id INTEGER PRIMARY KEY, d DATETIME, v)",
				"INSERT INTO t VALUES(1, '2026-11-21 12:00:00', "+v.expr+")")
			lh, err := equal.LogicalHashFile(ctx, p)
			if err != nil {
				fatal("LogicalHash "+v.name, err)
			}
			if prev, dup := seenHash[lh.SHA256]; dup {
				collisions = append(collisions, v.name+"=="+prev)
			}
			seenHash[lh.SHA256] = v.name
		}

		// 2. Check -0.0 vs +0.0 in REAL column (note SQLite may normalize -0.0 to 0.0 on disk).
		pPos0 := buildDefaultDB(ctx, filepath.Join(dir, "pos0.db"), "CREATE TABLE t(id INTEGER PRIMARY KEY, r REAL)", "INSERT INTO t VALUES(1, 0.0)")
		pNeg0 := buildDefaultDB(ctx, filepath.Join(dir, "neg0.db"), "CREATE TABLE t(id INTEGER PRIMARY KEY, r REAL)", "INSERT INTO t VALUES(1, -0.0)")
		rZero, _, _, err := equal.CompareLogicalFiles(ctx, pPos0, pNeg0)
		if err != nil {
			fatal("zero compare", err)
		}

		// 3. FTS5 virtual table + shadow tables.
		ftsStatus := "unsupported"
		ftsOK := true
		pFTS1 := filepath.Join(dir, "fts1.db")
		pFTS2 := filepath.Join(dir, "fts2.db")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fatal("mkdir", err)
		}
		testFTSDB, err := sqlitex.Open(ctx, pFTS1, sqlitex.DefaultOptions())
		if err != nil {
			fatal("open fts", err)
		}
		_, ftsCreateErr := testFTSDB.ExecContext(ctx, "CREATE VIRTUAL TABLE docs USING fts5(body)")
		if ftsCreateErr == nil {
			_, _ = testFTSDB.ExecContext(ctx, "INSERT INTO docs VALUES('hello world')")
			testFTSDB.Close()
			buildDefaultDB(ctx, pFTS2, "CREATE VIRTUAL TABLE docs USING fts5(body)", "INSERT INTO docs VALUES('hello sqlite')")
			rf, l1, _, err := equal.CompareLogicalFiles(ctx, pFTS1, pFTS2)
			if err != nil {
				ftsOK = false
				ftsStatus = "err:" + err.Error()
			} else if rf.Equal {
				ftsOK = false
				ftsStatus = "BLIND(equal)"
			} else {
				ftsStatus = fmt.Sprintf("differ(tables=%d)", len(l1.Tables))
			}
		} else {
			testFTSDB.Close()
		}

		// 4. WITHOUT ROWID foreign-key orphan (PRAGMA foreign_key_check yields NULL rowid).
		pWRClean := buildDefaultDB(ctx, filepath.Join(dir, "wr-clean.db"),
			"CREATE TABLE p(k TEXT PRIMARY KEY) WITHOUT ROWID",
			"CREATE TABLE c(k TEXT PRIMARY KEY, pk TEXT REFERENCES p(k)) WITHOUT ROWID",
			"INSERT INTO p VALUES('a')", "INSERT INTO c VALUES('c1', 'a')")
		pWROrphan := buildDefaultDB(ctx, filepath.Join(dir, "wr-orphan.db"),
			"CREATE TABLE p(k TEXT PRIMARY KEY) WITHOUT ROWID",
			"CREATE TABLE c(k TEXT PRIMARY KEY, pk TEXT REFERENCES p(k)) WITHOUT ROWID",
			"PRAGMA foreign_keys = OFF",
			"INSERT INTO p VALUES('a')", "INSERT INTO c VALUES('c1', 'a')", "INSERT INTO c VALUES('c2', 'missing')",
			"PRAGMA foreign_keys = ON")
		rWR, _, lWRO, err := equal.CompareLogicalFiles(ctx, pWRClean, pWROrphan)
		if err != nil {
			fatal("WR fk compare", err)
		}
		wrFKOK := !rWR.Equal && len(lWRO.ForeignKeyCheck) == 1 && strings.Contains(lWRO.ForeignKeyCheck[0], "rowid=NULL")

		held := len(collisions) == 0 && ftsOK && wrFKOK
		report("EQ-02",
			"13 distinct values (incl. DATETIME, +-Inf, INT64_MIN/MAX, embedded NUL), FTS5 shadow tables, WITHOUT ROWID FK orphan, and +-0.0",
			"all 13 values distinct (0 collisions), FTS5 shadow tables hashed and distinguished, WITHOUT ROWID FK orphan reports rowid=NULL",
			fmt.Sprintf("collisions=%v, fts5=%s, wrFK=%v(%v), pos0_vs_neg0_equal=%v",
				collisions, ftsStatus, wrFKOK, lWRO.ForeignKeyCheck, rZero.Equal),
			held)
	}

	// EQ-03: PRAGMA default_cache_size (header bytes 48-51) blind spot in LogicalHash vs VACUUM INTO!
	// Two DBs identical in schema order, schema cookie, rows, and all 6 LogicalPragmas,
	// differing ONLY in persistent PRAGMA default_cache_size (0 vs 2000).
	{
		dir := filepath.Join(scratch, "eq03")
		stmtsA := []string{
			"PRAGMA default_cache_size = 0",
			"CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT)",
			"INSERT INTO t VALUES(1, 'hello')",
		}
		stmtsB := []string{
			"PRAGMA default_cache_size = 2000",
			"CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT)",
			"INSERT INTO t VALUES(1, 'hello')",
		}
		pa := buildDefaultDB(ctx, filepath.Join(dir, "a.db"), stmtsA...)
		pb := buildDefaultDB(ctx, filepath.Join(dir, "b.db"), stmtsB...)
		e3, _, _, err := equal.CompareLogicalFiles(ctx, pa, pb)
		if err != nil {
			fatal("eq03 E3", err)
		}
		va, vb := filepath.Join(dir, "a-vac.db"), filepath.Join(dir, "b-vac.db")
		vacuumInto(ctx, pa, va)
		vacuumInto(ctx, pb, vb)
		e1, err := equal.CompareFiles(va, vb, 4096)
		if err != nil {
			fatal("eq03 E1", err)
		}
		e2, err := equal.ComparePageFiles(va, vb, 4096)
		if err != nil {
			fatal("eq03 E2", err)
		}
		// Claim: E3 equality (with identical schema creation order & cookie) implies identical VACUUM INTO bytes;
		// AND E3 covers persistent pragmas. default_cache_size breaks both!
		consistent := !(e3.Equal && !e1.Equal)
		report("EQ-03",
			"two DBs identical in schema order, cookie, rows, and all 6 LogicalPragmas, differing only in persistent PRAGMA default_cache_size (0 vs 2000)",
			"if E3 says equal (same schema order & cookie), VACUUM INTO canonical forms match at E1 (and E2)",
			fmt.Sprintf("E3_equal=%v, VACUUM_INTO_E1_equal=%v(diffPages=%d), VACUUM_INTO_E2_equal=%v (bytes 48-51 copied by vacuum.c, not hashed by E3)",
				e3.Equal, e1.Equal, e1.DifferingPages, e2.Equal),
			consistent)
	}

	// EQ-04: False refusal in LogicalHash for CREATE TABLE t(rowid INTEGER PRIMARY KEY, _rowid_ TEXT, oid TEXT).
	// Here `rowid` IS the integer primary key (NOT hidden!), yet rowidAlias() refuses it.
	{
		dir := filepath.Join(scratch, "eq04")
		pIPK := buildDefaultDB(ctx, filepath.Join(dir, "ipk-rowid.db"),
			"CREATE TABLE t(rowid INTEGER PRIMARY KEY, _rowid_ TEXT, oid TEXT)",
			"INSERT INTO t VALUES(7, 'a', 'b')")
		pHidden := buildDefaultDB(ctx, filepath.Join(dir, "hidden-rowid.db"),
			"CREATE TABLE t(rowid TEXT, _rowid_ TEXT, oid TEXT)",
			"INSERT INTO t VALUES('x', 'y', 'z')")

		_, errIPK := equal.LogicalHashFile(ctx, pIPK)
		_, errHidden := equal.LogicalHashFile(ctx, pHidden)
		// Expected: pIPK succeeds (rowid IS the INTEGER PRIMARY KEY, not hidden), pHidden fails cleanly.
		held := errIPK == nil && errHidden != nil && strings.Contains(errHidden.Error(), "hide the rowid")
		report("EQ-04",
			"LogicalHashFile on CREATE TABLE t(rowid INTEGER PRIMARY KEY, _rowid_ TEXT, oid TEXT) vs hidden-rowid table t(rowid TEXT, _rowid_ TEXT, oid TEXT)",
			"IPK-rowid table hashes cleanly (rowid is NOT hidden); text-rowid table rejected with 'hide the rowid'",
			fmt.Sprintf("ipk_rowid_err=%v; hidden_rowid_err=%v", errIPK, errHidden),
			held)
	}

	// EQ-05: LogicalHashFile and RunDBHash silently mutate a WAL database that has an
	// uncheckpointed -wal file because both open with mode=rw and close cleanly.
	{
		dir := filepath.Join(scratch, "eq05")
		livePath := filepath.Join(dir, "live.db")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fatal("mkdir", err)
		}
		opts := sqlitex.DefaultOptions()
		opts.SingleConnection = true
		opts.Conn.WALAutocheckpoint = 0
		db, err := sqlitex.Open(ctx, livePath, opts)
		if err != nil {
			fatal("open live", err)
		}
		if _, err := db.ExecContext(ctx, "CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT)"); err != nil {
			fatal("create", err)
		}
		if _, err := db.ExecContext(ctx, "INSERT INTO t VALUES(1, 'in-wal-only')"); err != nil {
			fatal("insert", err)
		}

		// Copy live.db + live.db-wal from a child process (avoiding same-process POSIX lock drop).
		snapLH := filepath.Join(dir, "snap-lh.db")
		snapDBH := filepath.Join(dir, "snap-dbh.db")
		if err := copyViaChild(livePath, snapLH); err != nil {
			fatal("child copy lh", err)
		}
		if err := copyViaChild(livePath, snapDBH); err != nil {
			fatal("child copy dbh", err)
		}
		db.Close()

		shaBeforeLH, err := equal.FileSHA256(snapLH)
		if err != nil {
			fatal("sha before lh", err)
		}
		walStatBefore, err := os.Stat(snapLH + "-wal")
		if err != nil {
			fatal("stat wal before", err)
		}

		if _, err := equal.LogicalHashFile(ctx, snapLH); err != nil {
			fatal("LogicalHashFile on uncheckpointed", err)
		}
		shaAfterLH, err := equal.FileSHA256(snapLH)
		if err != nil {
			fatal("sha after lh", err)
		}
		_, walErrAfterLH := os.Stat(snapLH + "-wal")
		lhMutated := shaBeforeLH != shaAfterLH || errors.Is(walErrAfterLH, os.ErrNotExist)

		dbhMutated := false
		dbhInfo := "skipped"
		if hasDBHash {
			shaBeforeDBH, _ := equal.FileSHA256(snapDBH)
			if _, err := equal.RunDBHash(ctx, dbhashTool, snapDBH); err != nil {
				fatal("RunDBHash on uncheckpointed", err)
			}
			shaAfterDBH, _ := equal.FileSHA256(snapDBH)
			_, walErrAfterDBH := os.Stat(snapDBH + "-wal")
			dbhMutated = shaBeforeDBH != shaAfterDBH || errors.Is(walErrAfterDBH, os.ErrNotExist)
			dbhInfo = fmt.Sprintf("db_changed=%v,wal_removed=%v", shaBeforeDBH != shaAfterDBH, errors.Is(walErrAfterDBH, os.ErrNotExist))
		}

		// Build-notes / logical_test claim: "hashing leaves the file and its side files unchanged."
		// That holds ONLY when the file is already at rest; on an uncheckpointed WAL snapshot,
		// mode=rw silently checkpoints into .db and deletes -wal!
		report("EQ-05",
			"run LogicalHashFile and RunDBHash on an uncheckpointed WAL database (.db + non-empty -wal)",
			"read-only hashing leaves .db bytes and -wal file unchanged",
			fmt.Sprintf("wal_before=%dB; LogicalHashFile(db_changed=%v,wal_removed=%v); RunDBHash(%s)",
				walStatBefore.Size(), shaBeforeLH != shaAfterLH, errors.Is(walErrAfterLH, os.ErrNotExist), dbhInfo),
			!lhMutated && !dbhMutated)
	}

	// EQ-06: E2 (internal/equal/pages.go) boundary and invariant checks:
	// WAL vs rollback (bytes 18-19 NOT ignored), backup into pre-populated destination,
	// overlapping/unsorted ignore ranges, page-2 ignore range, non-page-multiple, empty, invalid args.
	{
		dir := filepath.Join(scratch, "eq06")
		stmts := []string{"CREATE TABLE t(id INTEGER PRIMARY KEY, v BLOB)", "INSERT INTO t VALUES(1, zeroblob(2000))"}
		optsWAL := sqlitex.DefaultOptions()
		optsDel := sqlitex.DefaultOptions()
		optsDel.File.JournalMode = sqlitex.JournalDelete
		pWAL := buildCustomDB(ctx, filepath.Join(dir, "wal.db"), optsWAL, stmts...)
		pDel := buildCustomDB(ctx, filepath.Join(dir, "del.db"), optsDel, stmts...)

		// 1. WAL vs rollback must DIFFER at E2 (bytes 18-19 are NOT ignored).
		rMode, err := equal.ComparePageFiles(pWAL, pDel, 4096)
		if err != nil {
			fatal("ComparePageFiles mode", err)
		}

		// 2. Backup into a pre-populated destination (destination cookie 3 -> 4 vs source cookie 1).
		pPrePop := buildCustomDB(ctx, filepath.Join(dir, "prepop.db"), optsWAL,
			"CREATE TABLE old1(x)", "CREATE TABLE old2(x)", "DROP TABLE old1")
		srcDB, err := equal.OpenExisting(ctx, pWAL)
		if err != nil {
			fatal("OpenExisting", err)
		}
		srcConn, err := srcDB.Conn(ctx)
		if err != nil {
			fatal("conn", err)
		}
		if _, err := sqlitex.Backup(ctx, srcConn, pPrePop, -1, nil); err != nil {
			fatal("Backup prepop", err)
		}
		srcConn.Close()
		srcDB.Close()
		rPrePop, err := equal.ComparePageFiles(pWAL, pPrePop, 4096)
		if err != nil {
			fatal("ComparePageFiles prepop", err)
		}

		// 3. Overlapping + unsorted ranges must not duplicate IgnoredDiffering offsets.
		imgA := make([]byte, 8192)
		imgB := make([]byte, 8192)
		imgB[26] = 1
		imgB[4098] = 2
		rOverlap, err := equal.CompareImagesIgnoring(imgA, imgB, 4096, []equal.ByteRange{
			{Start: 4096, End: 4100}, {Start: 24, End: 28}, {Start: 25, End: 30},
		})
		if err != nil {
			fatal("CompareImagesIgnoring overlap", err)
		}
		overlapOK := rOverlap.Equal && fmt.Sprint(rOverlap.IgnoredDiffering) == "[26 4098]"

		// 4. Empty images + non-page-multiple images + invalid inputs.
		rEmpty, errE := equal.ComparePages(nil, nil, 4096)
		shortA := make([]byte, 100)
		shortB := make([]byte, 100)
		shortB[25] = 9
		rShort, errS := equal.ComparePages(shortA, shortB, 4096)
		_, errNegRange := equal.CompareImagesIgnoring(imgA, imgB, 4096, []equal.ByteRange{{Start: -1, End: 5}})
		_, errRevRange := equal.CompareImagesIgnoring(imgA, imgB, 4096, []equal.ByteRange{{Start: 10, End: 5}})
		_, errZeroPS := equal.ComparePages(imgA, imgB, 0)
		_, errNegPS := equal.ComparePages(imgA, imgB, -4096)

		held := !rMode.Equal && rMode.FirstDifferingPage == 1 &&
			rPrePop.Equal && len(rPrePop.IgnoredDiffering) > 0 &&
			overlapOK &&
			errE == nil && rEmpty.Equal &&
			errS == nil && rShort.Equal &&
			errNegRange != nil && errRevRange != nil && errZeroPS != nil && errNegPS != nil

		report("EQ-06",
			"E2 ComparePages/CompareImagesIgnoring: WAL vs rollback, backup into pre-populated DB, overlapping/page-2 ranges, empty/short images, invalid args",
			"WAL!=rollback at E2, pre-populated backup E2-equal, overlapping ranges deduplicate offsets [26 4098], invalid args error",
			fmt.Sprintf("walVsDel_eq=%v, prepopBackup_eq=%v(offs=%v), overlapOK=%v(offs=%v), emptyOK=%v, shortOK=%v, badArgsRejected=%v",
				rMode.Equal, rPrePop.Equal, rPrePop.IgnoredDiffering, overlapOK, rOverlap.IgnoredDiffering,
				errE == nil && rEmpty.Equal, errS == nil && rShort.Equal,
				errNegRange != nil && errRevRange != nil && errZeroPS != nil && errNegPS != nil),
			held)
	}

	// EQ-07: dbhash runner (internal/equal/dbhash.go) edge cases:
	// path with spaces, missing file (dbhash exits 0 with stderr!), corrupt file.
	{
		if !hasDBHash {
			report("EQ-07",
				"RunDBHash edge cases (path with spaces, missing file, corrupt non-SQLite file)",
				"dbhash binary available to test",
				"dbhash not found: "+dbhashErr.Error(),
				false)
		} else {
			dir := filepath.Join(scratch, "eq07 with spaces")
			spaceDB := buildDefaultDB(ctx, filepath.Join(dir, "my db file.db"),
				"CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT)", "INSERT INTO t VALUES(1, 'ok')")
			hSpace, errSpace := equal.RunDBHash(ctx, dbhashTool, spaceDB)
			_, errMissing := equal.RunDBHash(ctx, dbhashTool, filepath.Join(dir, "does-not-exist.db"))
			corruptPath := filepath.Join(dir, "corrupt.db")
			if err := os.WriteFile(corruptPath, []byte("not a sqlite database at all!"), 0o644); err != nil {
				fatal("write corrupt", err)
			}
			_, errCorrupt := equal.RunDBHash(ctx, dbhashTool, corruptPath)

			held := errSpace == nil && len(hSpace) == 40 && errMissing != nil && errCorrupt != nil
			report("EQ-07",
				"RunDBHash on path containing spaces, missing file (dbhash exits 0 + stderr), and corrupt non-SQLite file",
				"space path returns 40-hex SHA1; missing file and corrupt file both return errors",
				fmt.Sprintf("space_len=%d(err=%v), missing_err=%v, corrupt_err=%v",
					len(hSpace), errSpace, errMissing != nil, errCorrupt != nil),
				held)
		}
	}

	// EQ-08: Caller-supplied *sql.Conn with a TEMP table shadowing a main table
	// (logical.go:145,181,217 omits 'main' qualifier) + EncodeValue silent empty
	// output on unsupported Go types (logical.go:387-412).
	{
		dir := filepath.Join(scratch, "eq08")
		p := buildDefaultDB(ctx, filepath.Join(dir, "shadow.db"),
			"CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT)",
			"INSERT INTO t VALUES(1, 'from-main')")
		cleanHash, err := equal.LogicalHashFile(ctx, p)
		if err != nil {
			fatal("LogicalHashFile clean", err)
		}
		db, err := equal.OpenExisting(ctx, p)
		if err != nil {
			fatal("OpenExisting", err)
		}
		conn, err := db.Conn(ctx)
		if err != nil {
			fatal("conn", err)
		}
		if _, err := conn.ExecContext(ctx, "CREATE TEMP TABLE t(a TEXT, b TEXT)"); err != nil {
			fatal("create temp", err)
		}
		if _, err := conn.ExecContext(ctx, "INSERT INTO temp.t VALUES('from-temp', 'shadow')"); err != nil {
			fatal("insert temp", err)
		}
		shadowHash, shadowErr := equal.LogicalHash(ctx, conn)
		conn.Close()
		db.Close()

		// Also check EncodeValue on distinct unsupported Go types (int vs bool).
		encInt := equal.EncodeValue(int(1))
		encBool := equal.EncodeValue(true)
		encCollides := len(encInt) == 0 && len(encBool) == 0

		shadowMatchesMain := shadowErr == nil && shadowHash.SHA256 == cleanHash.SHA256
		report("EQ-08",
			"LogicalHash(ctx, conn) with TEMP TABLE t shadowing main.t + EncodeValue on unsupported types int(1) and bool(true)",
			"LogicalHash hashes main.t identically regardless of TEMP table; EncodeValue does not silently collapse unsupported types to empty []byte",
			fmt.Sprintf("shadow_err=%v, shadow_matches_main=%v, EncodeValue(int(1))=%x EncodeValue(true)=%x (empty_collision=%v)",
				shadowErr, shadowMatchesMain, encInt, encBool, encCollides),
			shadowMatchesMain && !encCollides)
	}

	// EQ-09: Context cancellation mid-walk in LogicalHash(ctx, conn) leaks an open
	// read transaction on conn because cleanup COMMIT uses the canceled ctx
	// (logical.go:414-423).
	{
		dir := filepath.Join(scratch, "eq09")
		p := buildDefaultDB(ctx, filepath.Join(dir, "cancel.db"),
			"CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT)",
			"INSERT INTO t VALUES(1, 'a'), (2, 'b')")
		db, err := equal.OpenExisting(ctx, p)
		if err != nil {
			fatal("OpenExisting", err)
		}
		conn, err := db.Conn(ctx)
		if err != nil {
			fatal("conn", err)
		}
		// Start a real LogicalHash on a large enough table or cancel right as WalkLogical runs.
		// Even simpler: insert 5000 rows so a short timeout cancels mid-walk after BEGIN succeeds.
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			fatal("begin fill", err)
		}
		for i := 3; i <= 5000; i++ {
			if _, err := tx.ExecContext(ctx, "INSERT INTO t VALUES(?, 'payload')", i); err != nil {
				fatal("fill", err)
			}
		}
		if err := tx.Commit(); err != nil {
			fatal("commit fill", err)
		}

		// Cancel during LogicalHash: run with a canceled context after BEGIN? We can't hook
		// LogicalHash directly, so retry with microsecond timeouts until LogicalHash fails
		// with context.DeadlineExceeded after BEGIN succeeded.
		leakedTx := false
		hitCancelAfterBegin := false
		var detail string
		for _, us := range []int{50, 100, 200, 400, 800, 1500, 3000, 6000} {
			cctx, cancel := context.WithTimeout(ctx, time.Duration(us)*time.Microsecond)
			_, lhErr := equal.LogicalHash(cctx, conn)
			cancel()
			if lhErr == nil {
				continue
			}
			// Test whether conn was left inside an open transaction!
			_, beginErr := conn.ExecContext(ctx, "BEGIN")
			if beginErr != nil && strings.Contains(beginErr.Error(), "within a transaction") {
				hitCancelAfterBegin = true
				leakedTx = true
				detail = fmt.Sprintf("timeout=%dus lhErr=%v next_BEGIN=%v", us, lhErr, beginErr)
				_, _ = conn.ExecContext(ctx, "ROLLBACK")
				break
			}
			if beginErr == nil {
				_, _ = conn.ExecContext(ctx, "ROLLBACK")
				if !strings.Contains(lhErr.Error(), "begin read transaction") {
					hitCancelAfterBegin = true
					detail = fmt.Sprintf("timeout=%dus lhErr=%v next_BEGIN=ok (tx cleanly closed)", us, lhErr)
					break
				}
			}
		}
		conn.Close()
		db.Close()
		if detail == "" {
			detail = "no mid-walk timeout triggered"
		}
		report("EQ-09",
			"cancel context mid-walk during LogicalHash(ctx, conn) and immediately issue BEGIN on the same conn",
			"cleanup ends the read transaction even when ctx is canceled, so subsequent BEGIN succeeds",
			fmt.Sprintf("hitMidWalk=%v, leakedTx=%v (%s)", hitCancelAfterBegin, leakedTx, detail),
			hitCancelAfterBegin && !leakedTx)
	}
}
