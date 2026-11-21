// Group 2 (delete-leaks): Adversarial attacks on Chapter 3 Experiment 3.2
// (delete-and-reuse): tenant-data leakage in file copies / sqlite3_backup /
// sqlite3_serialize / VACUUM INTO, auto_vacuum shrinking, secure_delete across
// overflow/index/UPDATE/DROP/truncate paths, secure_delete=FAST, -wal leakage
// under secure_delete=ON, and multi-page transactions starting with a non-empty
// freelist.
package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickstrad/sqlite-internals/internal/equal"
	"github.com/nickstrad/sqlite-internals/internal/recipe"
	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
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

func marker(prefix string, id int64) []byte {
	return []byte(fmt.Sprintf("%s-%06d", prefix, id))
}

func secretBlob(prefix string, id int64, size int) []byte {
	unit := append(marker(prefix, id), ' ')
	return bytes.Repeat(unit, size/len(unit)+1)[:size]
}

func countMarkers(data []byte, prefix string, first, last int64) int {
	found := 0
	for id := first; id <= last; id++ {
		if bytes.Contains(data, marker(prefix, id)) {
			found++
		}
	}
	return found
}

func mustRead(path string) []byte {
	b, err := os.ReadFile(path)
	if err != nil {
		fatal("read "+path, err)
	}
	return b
}

func cleanDB(path string) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		fatal("mkdir", err)
	}
	if err := recipe.RemoveDatabase(path); err != nil {
		fatal("remove", err)
	}
}

func openWith(ctx context.Context, path string, autoVacuum string, secureDelete bool) *sqlitex.DB {
	opts := sqlitex.DefaultOptions()
	opts.SingleConnection = true
	opts.File.AutoVacuum = autoVacuum
	opts.Conn.SecureDelete = secureDelete
	db, err := sqlitex.Open(ctx, path, opts)
	if err != nil {
		fatal("open "+path, err)
	}
	return db
}

func execBatch(ctx context.Context, db *sqlitex.DB, stmt string, rows [][]any) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		fatal("begin", err)
	}
	for _, args := range rows {
		if _, err := tx.ExecContext(ctx, stmt, args...); err != nil {
			fatal(stmt, err)
		}
	}
	if err := tx.Commit(); err != nil {
		fatal("commit", err)
	}
}

func pragmaInt(ctx context.Context, q sqlitex.Querier, name string) int {
	var v int
	if err := q.QueryRowContext(ctx, "PRAGMA "+name).Scan(&v); err != nil {
		fatal("PRAGMA "+name, err)
	}
	return v
}

func main() {
	ctx := context.Background()
	root, err := sqlitex.RepoRoot()
	if err != nil {
		fatal("RepoRoot", err)
	}
	scratch := filepath.Join(root, "_work", "adv-ch03", "delete-leaks")
	if err := os.RemoveAll(scratch); err != nil {
		fatal("clean scratch", err)
	}
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		fatal("mkdir scratch", err)
	}

	// DEL-01: Tenant data leakage in plain file copy, sqlite3_backup,
	// sqlite3_serialize, and VACUUM INTO with secure_delete=OFF vs ON
	// (OUTLINE 3.2 bullet 1).
	{
		type modeResult struct {
			src, fileCopy, backup, serialize, vacuumInto int
		}
		runMode := func(label string, secureDelete bool) modeResult {
			dir := filepath.Join(scratch, "del01-"+label)
			srcPath := filepath.Join(dir, "src.db")
			cleanDB(srcPath)

			// Populate 200 rows (1000B each) and delete 100 middle rows (51..150).
			db := openWith(ctx, srcPath, sqlitex.AutoVacuumNone, secureDelete)
			if _, err := db.ExecContext(ctx, "CREATE TABLE t(id INTEGER PRIMARY KEY, v BLOB)"); err != nil {
				fatal("create", err)
			}
			var rows [][]any
			for id := int64(1); id <= 200; id++ {
				rows = append(rows, []any{id, secretBlob("TENANT", id, 1000)})
			}
			execBatch(ctx, db, "INSERT INTO t(id, v) VALUES(?, ?)", rows)
			if _, err := db.ExecContext(ctx, "DELETE FROM t WHERE id BETWEEN 51 AND 150"); err != nil {
				fatal("delete", err)
			}
			if err := db.Close(); err != nil {
				fatal("close", err)
			}

			// 1. Source at rest and plain file copy.
			srcBytes := mustRead(srcPath)
			copyPath := filepath.Join(dir, "copy.db")
			if err := os.WriteFile(copyPath, srcBytes, 0o644); err != nil {
				fatal("write copy", err)
			}

			// 2. Online page-level backup (sqlite3_backup_step) and sqlite3_serialize.
			backupPath := filepath.Join(dir, "backup.db")
			cleanDB(backupPath)
			vacuumPath := filepath.Join(dir, "vacuum-into.db")
			cleanDB(vacuumPath)

			openDB, err := equal.OpenExisting(ctx, srcPath)
			if err != nil {
				fatal("OpenExisting", err)
			}
			conn, err := openDB.Conn(ctx)
			if err != nil {
				fatal("conn", err)
			}
			if _, err := sqlitex.Backup(ctx, conn, backupPath, -1, nil); err != nil {
				fatal("Backup", err)
			}
			serBytes, err := sqlitex.Serialize(ctx, conn)
			if err != nil {
				fatal("Serialize", err)
			}
			if _, err := conn.ExecContext(ctx, "VACUUM INTO ?", vacuumPath); err != nil {
				fatal("VACUUM INTO", err)
			}
			conn.Close()
			openDB.Close()

			return modeResult{
				src:        countMarkers(srcBytes, "TENANT", 51, 150),
				fileCopy:   countMarkers(mustRead(copyPath), "TENANT", 51, 150),
				backup:     countMarkers(mustRead(backupPath), "TENANT", 51, 150),
				serialize:  countMarkers(serBytes, "TENANT", 51, 150),
				vacuumInto: countMarkers(mustRead(vacuumPath), "TENANT", 51, 150),
			}
		}

		off := runMode("off", false)
		on := runMode("on", true)
		held := off.src > 0 && off.fileCopy == off.src && off.backup == off.src && off.serialize == off.src && off.vacuumInto == 0 &&
			on.src == 0 && on.fileCopy == 0 && on.backup == 0 && on.serialize == 0 && on.vacuumInto == 0
		report("DEL-01",
			"search deleted tenant markers (100 rows) in source, file copy, sqlite3_backup, sqlite3_serialize, and VACUUM INTO (secure_delete OFF vs ON)",
			"OFF leaks in src/copy/backup/serialize (>0) and not in VACUUM INTO (0); ON leaks 0 in all five",
			fmt.Sprintf("OFF(src=%d,copy=%d,backup=%d,serialize=%d,vacuumInto=%d); ON(src=%d,copy=%d,backup=%d,serialize=%d,vacuumInto=%d)",
				off.src, off.fileCopy, off.backup, off.serialize, off.vacuumInto,
				on.src, on.fileCopy, on.backup, on.serialize, on.vacuumInto),
			held)
	}

	// DEL-02: Space reclamation — VACUUM (auto_vacuum=NONE) vs auto_vacuum=FULL
	// and auto_vacuum=INCREMENTAL. Attacks the unqualified claim "the file never
	// shrinks on its own" (true only for auto_vacuum=NONE).
	{
		runAV := func(mode string) (pagesBefore, pagesAfterDel, freeAfterDel, pagesAfterAct int) {
			p := filepath.Join(scratch, "del02-"+strings.ToLower(mode)+".db")
			cleanDB(p)
			db := openWith(ctx, p, mode, false)
			if _, err := db.ExecContext(ctx, "CREATE TABLE t(id INTEGER PRIMARY KEY, v BLOB)"); err != nil {
				fatal("create", err)
			}
			var rows [][]any
			for id := int64(1); id <= 200; id++ {
				rows = append(rows, []any{id, secretBlob("AV", id, 1000)})
			}
			execBatch(ctx, db, "INSERT INTO t(id, v) VALUES(?, ?)", rows)
			pagesBefore = pragmaInt(ctx, db, "page_count")
			if _, err := db.ExecContext(ctx, "DELETE FROM t WHERE id > 50"); err != nil {
				fatal("delete", err)
			}
			pagesAfterDel = pragmaInt(ctx, db, "page_count")
			freeAfterDel = pragmaInt(ctx, db, "freelist_count")
			switch mode {
			case sqlitex.AutoVacuumNone:
				if _, err := db.ExecContext(ctx, "VACUUM"); err != nil {
					fatal("vacuum", err)
				}
			case sqlitex.AutoVacuumIncremental:
				if _, err := db.ExecContext(ctx, "PRAGMA incremental_vacuum"); err != nil {
					fatal("inc vacuum", err)
				}
			}
			pagesAfterAct = pragmaInt(ctx, db, "page_count")
			db.Close()
			return
		}
		nB, nD, nF, nV := runAV(sqlitex.AutoVacuumNone)
		fB, fD, fF, _ := runAV(sqlitex.AutoVacuumFull)
		iB, iD, iF, iV := runAV(sqlitex.AutoVacuumIncremental)

		// Corrected claim (3.2 auto_vacuum part / correction 8):
		// NONE never shrinks on DELETE (freelist>0) and shrinks on VACUUM;
		// FULL shrinks immediately on DELETE leaving freelist=0;
		// INCREMENTAL holds freed pages on freelist until PRAGMA incremental_vacuum.
		fullShrankOnDelete := fD < fB && fF == 0
		noneNeedsVacuum := nD == nB && nF > 0 && nV < nB
		incNeedsPragma := iD == iB && iF > 0 && iV < iB
		report("DEL-02",
			"DELETE 150 of 200 rows under auto_vacuum=NONE, FULL, and INCREMENTAL",
			"NONE never shrinks until VACUUM (freelist>0); FULL shrinks on DELETE (freelist=0); INCREMENTAL holds freelist until incremental_vacuum",
			fmt.Sprintf("NONE:%d->del:%d(free=%d)->vacuum:%d; FULL:%d->del:%d(free=%d); INC:%d->del:%d(free=%d)->inc_vac:%d (noneOK=%v incOK=%v fullShrank=%v)",
				nB, nD, nF, nV, fB, fD, fF, iB, iD, iF, iV, noneNeedsVacuum, incNeedsPragma, fullShrankOnDelete),
			noneNeedsVacuum && fullShrankOnDelete && incNeedsPragma)
	}

	// DEL-03: secure_delete=ON vs OFF across 5 distinct deletion/overwrite paths:
	// (1) overflow pages (10 KB blobs), (2) secondary index entries, (3) UPDATE
	// shrinking a blob in place, (4) DROP TABLE, (5) DELETE FROM t (truncate opt).
	{
		type pathCounts struct {
			overflow, index, updateShrink, dropTable, truncateDel int
		}
		measure := func(label string, secureDelete bool) pathCounts {
			dir := filepath.Join(scratch, "del03-"+label)
			runCase := func(name string, fn func(db *sqlitex.DB)) int {
				p := filepath.Join(dir, name+".db")
				cleanDB(p)
				db := openWith(ctx, p, sqlitex.AutoVacuumNone, secureDelete)
				fn(db)
				if err := db.Close(); err != nil {
					fatal("close", err)
				}
				return countMarkers(mustRead(p), "S3", 1, 20)
			}
			var c pathCounts
			// 1. Overflow pages: 20 rows of 10,000B blobs, delete rows 1..20.
			c.overflow = runCase("overflow", func(db *sqlitex.DB) {
				_, _ = db.ExecContext(ctx, "CREATE TABLE t(id INTEGER PRIMARY KEY, v BLOB)")
				var rows [][]any
				for id := int64(1); id <= 25; id++ {
					prefix := "KEEP"
					if id <= 20 {
						prefix = "S3"
					}
					rows = append(rows, []any{id, secretBlob(prefix, id, 10000)})
				}
				execBatch(ctx, db, "INSERT INTO t VALUES(?, ?)", rows)
				_, _ = db.ExecContext(ctx, "DELETE FROM t WHERE id <= 20")
			})
			// 2. Secondary index entries holding secret text (small table, large indexed text).
			c.index = runCase("index", func(db *sqlitex.DB) {
				_, _ = db.ExecContext(ctx, "CREATE TABLE t(id INTEGER PRIMARY KEY, s TEXT)")
				_, _ = db.ExecContext(ctx, "CREATE INDEX t_s ON t(s)")
				var rows [][]any
				for id := int64(1); id <= 60; id++ {
					prefix := "KEEP"
					if id <= 20 {
						prefix = "S3"
					}
					rows = append(rows, []any{id, string(secretBlob(prefix, id, 400))})
				}
				execBatch(ctx, db, "INSERT INTO t VALUES(?, ?)", rows)
				_, _ = db.ExecContext(ctx, "DELETE FROM t WHERE id <= 20")
			})
			// 3. UPDATE shrinking a 1000B secret blob to 4 bytes (creates freeblocks in live leaves).
			c.updateShrink = runCase("update", func(db *sqlitex.DB) {
				_, _ = db.ExecContext(ctx, "CREATE TABLE t(id INTEGER PRIMARY KEY, v BLOB)")
				var rows [][]any
				for id := int64(1); id <= 20; id++ {
					rows = append(rows, []any{id, secretBlob("S3", id, 1000)})
				}
				execBatch(ctx, db, "INSERT INTO t VALUES(?, ?)", rows)
				_, _ = db.ExecContext(ctx, "UPDATE t SET v = x'01020304' WHERE id <= 20")
			})
			// 4. DROP TABLE freeing every page of the table.
			c.dropTable = runCase("drop", func(db *sqlitex.DB) {
				_, _ = db.ExecContext(ctx, "CREATE TABLE keep(id INTEGER PRIMARY KEY, v BLOB)")
				_, _ = db.ExecContext(ctx, "INSERT INTO keep VALUES(1, x'00')")
				_, _ = db.ExecContext(ctx, "CREATE TABLE t(id INTEGER PRIMARY KEY, v BLOB)")
				var rows [][]any
				for id := int64(1); id <= 20; id++ {
					rows = append(rows, []any{id, secretBlob("S3", id, 1000)})
				}
				execBatch(ctx, db, "INSERT INTO t VALUES(?, ?)", rows)
				_, _ = db.ExecContext(ctx, "DROP TABLE t")
			})
			// 5. Unqualified DELETE FROM t (SQLite truncate optimization: sqlite3BtreeClearTable).
			c.truncateDel = runCase("truncate", func(db *sqlitex.DB) {
				_, _ = db.ExecContext(ctx, "CREATE TABLE t(id INTEGER PRIMARY KEY, v BLOB)")
				var rows [][]any
				for id := int64(1); id <= 20; id++ {
					rows = append(rows, []any{id, secretBlob("S3", id, 1000)})
				}
				execBatch(ctx, db, "INSERT INTO t VALUES(?, ?)", rows)
				_, _ = db.ExecContext(ctx, "DELETE FROM t")
			})
			return c
		}

		off := measure("off", false)
		on := measure("on", true)
		allOffLeak := off.overflow > 0 && off.index > 0 && off.updateShrink > 0 && off.dropTable > 0 && off.truncateDel > 0
		allOnClean := on.overflow == 0 && on.index == 0 && on.updateShrink == 0 && on.dropTable == 0 && on.truncateDel == 0
		report("DEL-03",
			"secure_delete OFF vs ON across 5 paths: overflow DELETE, index DELETE, shrinking UPDATE, DROP TABLE, unqualified DELETE FROM t",
			"OFF leaks secret markers on all 5 paths (>0); ON scrubs all 5 paths to 0 markers in at-rest .db",
			fmt.Sprintf("OFF(ovf=%d,idx=%d,upd=%d,drop=%d,trunc=%d); ON(ovf=%d,idx=%d,upd=%d,drop=%d,trunc=%d)",
				off.overflow, off.index, off.updateShrink, off.dropTable, off.truncateDel,
				on.overflow, on.index, on.updateShrink, on.dropTable, on.truncateDel),
			allOffLeak && allOnClean)
	}

	// DEL-04: PRAGMA secure_delete = FAST (the third SQLite mode).
	// FAST zeroes freed bytes only on pages already being written (live freeblocks
	// and updates), NOT on freelist leaves (it avoids the extra I/O).
	{
		p := filepath.Join(scratch, "del04-fast.db")
		cleanDB(p)
		db := openWith(ctx, p, sqlitex.AutoVacuumNone, false)
		if _, err := db.ExecContext(ctx, "PRAGMA secure_delete = FAST"); err != nil {
			fatal("secure_delete=FAST", err)
		}
		var modeVal int
		if err := db.QueryRowContext(ctx, "PRAGMA secure_delete").Scan(&modeVal); err != nil {
			fatal("read secure_delete", err)
		}
		if _, err := db.ExecContext(ctx, "CREATE TABLE t(id INTEGER PRIMARY KEY, v BLOB)"); err != nil {
			fatal("create", err)
		}
		var rows [][]any
		for id := int64(1); id <= 120; id++ {
			rows = append(rows, []any{id, secretBlob("FAST", id, 1000)})
		}
		execBatch(ctx, db, "INSERT INTO t VALUES(?, ?)", rows)
		// Shrink row 2 in-place (live page write) AND delete range 21..100 (creates freelist leaves).
		if _, err := db.ExecContext(ctx, "UPDATE t SET v = x'00' WHERE id = 2"); err != nil {
			fatal("update", err)
		}
		if _, err := db.ExecContext(ctx, "DELETE FROM t WHERE id BETWEEN 21 AND 100"); err != nil {
			fatal("delete", err)
		}
		if err := db.Close(); err != nil {
			fatal("close", err)
		}
		img := mustRead(p)
		updLeaked := countMarkers(img, "FAST", 2, 2)
		delLeaked := countMarkers(img, "FAST", 21, 100)
		// Held if FAST behaves as documented: scrubs live-page UPDATE (0) while still leaking freelist leaves (>0).
		held := modeVal == 2 && updLeaked == 0 && delLeaked > 0
		report("DEL-04",
			"PRAGMA secure_delete=FAST on live-page shrinking UPDATE + multi-page range DELETE",
			"mode=2, shrinking UPDATE scrubbed (0 markers), freelist-leaf DELETE still leaks (>0 markers)",
			fmt.Sprintf("pragma_secure_delete=%d, update_row2_markers=%d/1, range_delete_markers=%d/80",
				modeVal, updLeaked, delLeaked),
			held)
	}

	// DEL-05: -wal file exposure under secure_delete=ON!
	// Attacks 3.2 claim: "with secure_delete the deleted payload bytes are nowhere in the file."
	// Even with secure_delete=ON, the -wal file retains earlier INSERT frames containing
	// plaintext secrets until the WAL is truncated or overwritten (even after PASSIVE checkpoint).
	{
		p := filepath.Join(scratch, "del05-wal-exposure.db")
		cleanDB(p)
		opts := sqlitex.DefaultOptions()
		opts.SingleConnection = true
		opts.Conn.SecureDelete = true
		opts.Conn.WALAutocheckpoint = 0
		db, err := sqlitex.Open(ctx, p, opts)
		if err != nil {
			fatal("open", err)
		}
		conn, err := db.Conn(ctx)
		if err != nil {
			fatal("conn", err)
		}
		if _, err := conn.ExecContext(ctx, "CREATE TABLE t(id INTEGER PRIMARY KEY, v BLOB)"); err != nil {
			fatal("create", err)
		}
		var rows [][]any
		for id := int64(1); id <= 40; id++ {
			rows = append(rows, []any{id, secretBlob("WALKEY", id, 1000)})
		}
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			fatal("begin", err)
		}
		for _, r := range rows {
			if _, err := tx.ExecContext(ctx, "INSERT INTO t VALUES(?, ?)", r...); err != nil {
				fatal("insert", err)
			}
		}
		if err := tx.Commit(); err != nil {
			fatal("commit", err)
		}
		// Delete all 40 rows with secure_delete=ON.
		if _, err := conn.ExecContext(ctx, "DELETE FROM t"); err != nil {
			fatal("delete", err)
		}
		// Reading -wal is safe (no POSIX lock on -wal).
		walAfterDel := countMarkers(mustRead(p+"-wal"), "WALKEY", 1, 40)

		// PASSIVE checkpoint copies zeroed pages to .db but does NOT truncate -wal!
		if _, err := sqlitex.Checkpoint(ctx, conn, "main", sqlitex.CheckpointPassive); err != nil {
			fatal("passive ckpt", err)
		}
		walAfterPassive := countMarkers(mustRead(p+"-wal"), "WALKEY", 1, 40)

		// TRUNCATE checkpoint finally clears -wal.
		if _, err := sqlitex.Checkpoint(ctx, conn, "main", sqlitex.CheckpointTruncate); err != nil {
			fatal("truncate ckpt", err)
		}
		walAfterTrunc := countMarkers(mustRead(p+"-wal"), "WALKEY", 1, 40)

		conn.Close()
		db.Close()
		dbAtRest := countMarkers(mustRead(p), "WALKEY", 1, 40)

		// Corrected claim (3.2 -wal part / correction 9): secure_delete=ON scrubs the .db file
		// at rest (0/40), while -wal retains prior INSERT frames (40/40) after DELETE and even
		// after PASSIVE checkpoint until TRUNCATE clears -wal (0/40).
		matchesCorrected := walAfterDel == 40 && walAfterPassive == 40 && walAfterTrunc == 0 && dbAtRest == 0
		report("DEL-05",
			"INSERT + DELETE 40 secret rows with secure_delete=ON (wal_autocheckpoint=0); inspect -wal after DELETE, after PASSIVE checkpoint, after TRUNCATE, and .db at rest",
			"secure_delete=ON scrubs .db at rest (0/40), while -wal keeps 40/40 after DELETE and PASSIVE until TRUNCATE clears it (0/40)",
			fmt.Sprintf("wal_after_delete=%d/40, wal_after_PASSIVE=%d/40, wal_after_TRUNCATE=%d/40, db_at_rest=%d/40",
				walAfterDel, walAfterPassive, walAfterTrunc, dbAtRest),
			matchesCorrected)
	}

	// DEL-06: Multi-page transaction starting with a non-empty freelist.
	// Verifies the corrected 3.2 wording (step 4 + large-transaction part / correction 10):
	// reuse is per page allocation, so a transaction needing more pages than the freelist holds
	// exhausts the freelist (freelist_count -> 0) and grows page_count in the same transaction.
	{
		p := filepath.Join(scratch, "del06-multipage-tx.db")
		cleanDB(p)
		db := openWith(ctx, p, sqlitex.AutoVacuumNone, false)
		if _, err := db.ExecContext(ctx, "CREATE TABLE t(id INTEGER PRIMARY KEY, v BLOB)"); err != nil {
			fatal("create", err)
		}
		var rows [][]any
		for id := int64(1); id <= 40; id++ {
			rows = append(rows, []any{id, secretBlob("INIT", id, 1000)})
		}
		execBatch(ctx, db, "INSERT INTO t VALUES(?, ?)", rows)
		// Free ~3 leaves (ids 13..24).
		if _, err := db.ExecContext(ctx, "DELETE FROM t WHERE id BETWEEN 13 AND 24"); err != nil {
			fatal("delete", err)
		}
		freeBefore := pragmaInt(ctx, db, "freelist_count")
		pagesBefore := pragmaInt(ctx, db, "page_count")

		// One transaction inserts 32 rows (~8 leaves): more than freeBefore!
		var more [][]any
		for id := int64(101); id <= 132; id++ {
			more = append(more, []any{id, secretBlob("MORE", id, 1000)})
		}
		execBatch(ctx, db, "INSERT INTO t VALUES(?, ?)", more)
		freeAfter := pragmaInt(ctx, db, "freelist_count")
		pagesAfter := pragmaInt(ctx, db, "page_count")
		db.Close()

		exhaustedThenGrew := freeBefore > 0 && freeAfter == 0 && pagesAfter > pagesBefore
		report("DEL-06",
			"single transaction inserting 32 x 1000B rows (~8 pages) when freelist_count > 0",
			"per-allocation reuse: transaction exhausts freelist (freelist_count->0) and then grows page_count in the same commit",
			fmt.Sprintf("freelist %d->%d, page_count %d->%d (freelist exhausted mid-tx, then file grew by %d pages in same tx)",
				freeBefore, freeAfter, pagesBefore, pagesAfter, pagesAfter-pagesBefore),
			exhaustedThenGrew)
	}
}
