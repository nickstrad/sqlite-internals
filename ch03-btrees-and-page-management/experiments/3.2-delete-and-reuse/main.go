// Experiment 3.2 tracks page freeing, freelist reuse, file growth, VACUUM,
// auto_vacuum modes, and deleted-byte residue across secure_delete=OFF and
// secure_delete=ON in both the database file at rest and the -wal file.
package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nickstrad/sqlite-internals/internal/dbfile"
	"github.com/nickstrad/sqlite-internals/internal/recipe"
	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
)

const (
	pageSize    = 4096
	blobBytes   = 1000
	initialRows = 400
	deleteFirst = 99
	deleteLast  = 302
	reuseRows   = 100
	growBatches = 30
	rowsPerTx   = 4
)

var failures []string

func check(ok bool, claim string) {
	status := "ok"
	if !ok {
		status = "FAILED"
		failures = append(failures, claim)
	}
	fmt.Printf("check %s: %s\n", status, claim)
}

func fatal(what string, err error) {
	fmt.Printf("FATAL %s: %v\n", what, err)
	os.Exit(1)
}

func marker(prefix string, id int64) []byte {
	return []byte(fmt.Sprintf("%s-%06d", prefix, id))
}

func rowBlob(prefix string, id int64) []byte {
	unit := append(marker(prefix, id), ' ')
	return bytes.Repeat(unit, blobBytes/len(unit)+1)[:blobBytes]
}

type fileState struct {
	size      int64
	pages     int
	freelist  int
	trunks    []int
	leaves    []int
	census    *dbfile.Census
	image     []byte
	tableLeaf map[int][]int64
}

// readState inspects a closed, at-rest database file directly from disk so no
// open SQLite connection holds POSIX locks in this process.
func readState(path string) fileState {
	if err := recipe.CheckAtRest(path); err != nil {
		fatal("at rest", err)
	}
	image, err := os.ReadFile(path)
	if err != nil {
		fatal("read", err)
	}
	f, err := dbfile.FromBytes(image)
	if err != nil {
		fatal("parse", err)
	}
	census, err := f.Census()
	if err == nil {
		err = census.Complete()
	}
	if err != nil {
		fatal("census", err)
	}
	s := fileState{size: int64(len(image)), pages: f.PageCount(), freelist: census.FreelistLength(), trunks: census.FreelistTrunks, leaves: census.FreelistLeaves, census: census, image: image, tableLeaf: map[int][]int64{}}
	schema, err := f.Schema()
	if err != nil {
		fatal("schema", err)
	}
	for _, e := range schema {
		if e.Name != "t" {
			continue
		}
		err := f.WalkTable(int(e.RootPage), func(r dbfile.Row) error {
			s.tableLeaf[r.Page] = append(s.tableLeaf[r.Page], r.Rowid)
			return nil
		})
		if err != nil {
			fatal("walk", err)
		}
	}
	return s
}

func (s fileState) freePages() map[int]bool {
	out := map[int]bool{}
	for _, p := range s.trunks {
		out[p] = true
	}
	for _, p := range s.leaves {
		out[p] = true
	}
	return out
}

func (s fileState) summary() string {
	return fmt.Sprintf("file %d bytes = %d pages; freelist %d page(s) (trunk %s, %d leaves)", s.size, s.pages, s.freelist, ints(s.trunks), len(s.leaves))
}

func ints(xs []int) string {
	parts := make([]string, len(xs))
	for i, x := range xs {
		parts[i] = fmt.Sprint(x)
	}
	return "[" + strings.Join(parts, " ") + "]"
}

func compactPages(xs []int) string {
	if len(xs) == 0 {
		return "none"
	}
	sorted := append([]int(nil), xs...)
	sort.Ints(sorted)
	var parts []string
	start, prev := sorted[0], sorted[0]
	flush := func() {
		if start == prev {
			parts = append(parts, fmt.Sprint(start))
		} else {
			parts = append(parts, fmt.Sprintf("%d-%d", start, prev))
		}
	}
	for _, x := range sorted[1:] {
		if x == prev+1 {
			prev = x
			continue
		}
		flush()
		start, prev = x, x
	}
	flush()
	return strings.Join(parts, ",")
}

type finding struct {
	rowsFound  int
	rowsSought int
	byRole     map[string]int
	pages      map[int]bool
}

type markerRange struct {
	prefix      string
	first, last int64
}

var firstDeletion = []markerRange{{"SECRET", deleteFirst, deleteLast}}

var secondDeletion = []markerRange{{"NEW", 1001, 1000 + reuseRows}, {"GROW", 2001, 2000 + growBatches*rowsPerTx}}

func searchDeleted(s fileState) finding {
	return searchMarkers(s, firstDeletion)
}

func searchMarkers(s fileState, ranges []markerRange) finding {
	f := finding{byRole: map[string]int{}, pages: map[int]bool{}}
	for _, r := range ranges {
		for id := r.first; id <= r.last; id++ {
			f.rowsSought++
			if f.searchOne(s, marker(r.prefix, id)) {
				f.rowsFound++
			}
		}
	}
	return f
}

func (f finding) searchOne(s fileState, needle []byte) bool {
	found := false
	for offset := 0; ; {
		i := bytes.Index(s.image[offset:], needle)
		if i < 0 {
			return found
		}
		at := offset + i
		page := at/pageSize + 1
		if !f.pages[page] {
			f.pages[page] = true
			f.byRole[s.census.Page(page).Role.String()]++
		}
		found = true
		offset = at + len(needle)
	}
}

func (f finding) String() string {
	var roles []string
	for role, n := range f.byRole {
		roles = append(roles, fmt.Sprintf("%s=%d", role, n))
	}
	sort.Strings(roles)
	text := fmt.Sprintf("%d of %d deleted rows still readable in the file, on %d page(s)", f.rowsFound, f.rowsSought, len(f.pages))
	if len(roles) > 0 {
		text += ": " + strings.Join(roles, " ")
	}
	return text
}

func open(ctx context.Context, path string, secureDelete bool) *sqlitex.DB {
	options := sqlitex.DefaultOptions()
	options.SingleConnection = true
	options.Conn.SecureDelete = secureDelete
	db, err := sqlitex.Open(ctx, path, options)
	if err != nil {
		fatal("open", err)
	}
	return db
}

func session(ctx context.Context, path string, secureDelete bool, work func(db *sqlitex.DB)) {
	db := open(ctx, path, secureDelete)
	work(db)
	var integrity string
	if err := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil {
		fatal("integrity_check", err)
	}
	if integrity != "ok" {
		fatal("integrity_check", fmt.Errorf("%s", integrity))
	}
	if err := db.Close(); err != nil {
		fatal("close", err)
	}
}

func execAll(ctx context.Context, db *sqlitex.DB, statement string, rows [][]any) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		fatal("begin", err)
	}
	for _, args := range rows {
		if _, err := tx.ExecContext(ctx, statement, args...); err != nil {
			fatal(statement, err)
		}
	}
	if err := tx.Commit(); err != nil {
		fatal("commit", err)
	}
}

func pragmaInt(ctx context.Context, db *sqlitex.DB, name string) int {
	var v int
	if err := db.QueryRowContext(ctx, "PRAGMA "+name).Scan(&v); err != nil {
		fatal(name, err)
	}
	return v
}

func insertRows(prefix string, first, count int64) [][]any {
	var rows [][]any
	for id := first; id < first+count; id++ {
		rows = append(rows, []any{id, rowBlob(prefix, id)})
	}
	return rows
}

type outcome struct {
	afterDelete       finding
	afterReuse        finding
	afterSecondDelete finding
	afterVacuum       finding
	sizes             []int64
	deleteFrames      int
}

// walFrames computes the frame count from the -wal file length (32-byte header
// plus 24-byte frame header and 4096-byte page image per frame) before close
// checkpoints and deletes the WAL.
func walFrames(path string) int {
	info, err := os.Stat(path + "-wal")
	if err != nil {
		fatal("stat -wal", err)
	}
	if info.Size() < 32 || (info.Size()-32)%(24+pageSize) != 0 {
		fatal("-wal size", fmt.Errorf("%d bytes", info.Size()))
	}
	return int((info.Size() - 32) / (24 + pageSize))
}

func run(ctx context.Context, dir string, secureDelete bool) outcome {
	label := "OFF"
	if secureDelete {
		label = "ON"
	}
	path := filepath.Join(dir, "secure-delete-"+strings.ToLower(label), "t.db")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		fatal("mkdir", err)
	}
	fmt.Println()
	fmt.Printf("== secure_delete=%s (WAL; every step is its own connection, so the file is at rest and checkpointed when it is read)\n", label)
	var o outcome
	insert := "INSERT INTO t(id, v) VALUES(?, ?)"
	session(ctx, path, secureDelete, func(db *sqlitex.DB) {
		if _, err := db.ExecContext(ctx, "CREATE TABLE t(id INTEGER PRIMARY KEY, v BLOB)"); err != nil {
			fatal("create", err)
		}
		execAll(ctx, db, insert, insertRows("SECRET", 1, initialRows))
	})
	base := readState(path)
	o.sizes = append(o.sizes, base.size)
	fmt.Printf("1. %d rows of %d-byte blobs (each blob repeats a marker like %q): %s; %d table-leaf pages\n", initialRows, blobBytes, marker("SECRET", 150), base.summary(), len(base.tableLeaf))
	var held []int
	for page, ids := range base.tableLeaf {
		for _, id := range ids {
			if id >= deleteFirst && id <= deleteLast {
				held = append(held, page)
				break
			}
		}
	}

	// Under secure_delete=OFF, freePage2 (btree.c:6836-6969) calls
	// sqlite3PagerDontWrite (pager.c:6329-6339) for freed freelist leaves, so
	// they are not written to the WAL and retain their old bytes; freeSpace
	// (btree.c:1918-2014) likewise skips zeroing freeblocks on live leaves.
	session(ctx, path, secureDelete, func(db *sqlitex.DB) {
		if _, err := db.ExecContext(ctx, "DELETE FROM t WHERE id BETWEEN ? AND ?", deleteFirst, deleteLast); err != nil {
			fatal("delete", err)
		}
		o.deleteFrames = walFrames(path)
	})
	deleted := readState(path)
	o.sizes = append(o.sizes, deleted.size)
	fmt.Printf("2. DELETE FROM t WHERE id BETWEEN %d AND %d (%d rows, held on leaf pages %s): %s\n", deleteFirst, deleteLast, deleteLast-deleteFirst+1, compactPages(held), deleted.summary())
	free := deleted.freePages()
	var freedFromHeld []int
	for _, p := range held {
		if free[p] {
			freedFromHeld = append(freedFromHeld, p)
		}
	}
	fmt.Printf("   pages now on the freelist: %s; of them, former leaves of the deleted rows: %d\n", compactPages(append(append([]int(nil), deleted.trunks...), deleted.leaves...)), len(freedFromHeld))
	fmt.Printf("   the DELETE's commit wrote %d WAL frames\n", o.deleteFrames)
	check(deleted.size == base.size, "the file does not shrink on delete")
	check(deleted.freelist > 0 && len(freedFromHeld) > 0, "pages emptied by the delete moved to the freelist")
	o.afterDelete = searchDeleted(deleted)
	fmt.Printf("   search for the deleted markers: %s\n", o.afterDelete)

	session(ctx, path, secureDelete, func(db *sqlitex.DB) {
		execAll(ctx, db, insert, insertRows("NEW", 1001, reuseRows))
	})
	reused := readState(path)
	o.sizes = append(o.sizes, reused.size)
	var newPages, fromFreelist []int
	for page, ids := range reused.tableLeaf {
		for _, id := range ids {
			if id >= 1001 {
				newPages = append(newPages, page)
				if free[page] {
					fromFreelist = append(fromFreelist, page)
				}
				break
			}
		}
	}
	fmt.Printf("3. %d new rows (ids 1001..%d): %s\n", reuseRows, 1000+reuseRows, reused.summary())
	fmt.Printf("   leaf pages holding the new rows: %s; of them taken from the freelist: %d\n", compactPages(newPages), len(fromFreelist))
	check(reused.size == base.size, "the new rows fit in the file: its size is unchanged")
	check(len(newPages) > 0 && len(fromFreelist) == len(newPages), "every leaf page of the new rows is a former freelist page")
	o.afterReuse = searchDeleted(reused)
	fmt.Printf("   search for the deleted markers: %s\n", o.afterReuse)

	grewBeforeEmpty := false
	var growth []string
	session(ctx, path, secureDelete, func(db *sqlitex.DB) {
		next := int64(2001)
		for batch := 1; batch <= growBatches; batch++ {
			freeBefore := pragmaInt(ctx, db, "freelist_count")
			pagesBefore := pragmaInt(ctx, db, "page_count")
			execAll(ctx, db, insert, insertRows("GROW", next, rowsPerTx))
			next += rowsPerTx
			freeAfter := pragmaInt(ctx, db, "freelist_count")
			pagesAfter := pragmaInt(ctx, db, "page_count")
			if pagesAfter > pagesBefore && freeAfter > 0 {
				grewBeforeEmpty = true
			}
			if pagesAfter != pagesBefore || batch == 1 || freeBefore != freeAfter && freeAfter == 0 {
				growth = append(growth, fmt.Sprintf("batch %d: freelist %d->%d, page_count %d->%d", batch, freeBefore, freeAfter, pagesBefore, pagesAfter))
			}
		}
	})
	grown := readState(path)
	o.sizes = append(o.sizes, grown.size)
	fmt.Printf("4. %d more transactions of %d rows each: %s\n", growBatches, rowsPerTx, grown.summary())
	for i, line := range growth {
		if i < 4 || i == len(growth)-1 {
			fmt.Printf("   %s\n", line)
		} else if i == 4 {
			fmt.Printf("   ... (%d more batches that grew the file)\n", len(growth)-5)
		}
	}
	check(!grewBeforeEmpty, "page_count grows only once the freelist is used up: every transaction that added pages ended with an empty freelist")
	check(grown.size > base.size && grown.freelist == 0, "once the freelist is used up, the file grows")

	session(ctx, path, secureDelete, func(db *sqlitex.DB) {
		if _, err := db.ExecContext(ctx, "DELETE FROM t WHERE id >= 1001"); err != nil {
			fatal("delete", err)
		}
	})
	again := readState(path)
	o.sizes = append(o.sizes, again.size)
	fmt.Printf("5. DELETE FROM t WHERE id >= 1001: %s\n", again.summary())
	check(again.size == grown.size, "the file still does not shrink")
	o.afterSecondDelete = searchMarkers(again, secondDeletion)
	fmt.Printf("   search for the NEW- and GROW- markers just deleted: %s\n", o.afterSecondDelete)

	session(ctx, path, secureDelete, func(db *sqlitex.DB) {
		if _, err := db.ExecContext(ctx, "VACUUM"); err != nil {
			fatal("vacuum", err)
		}
	})
	vacuumed := readState(path)
	o.sizes = append(o.sizes, vacuumed.size)
	o.afterVacuum = searchMarkers(vacuumed, append(append([]markerRange(nil), firstDeletion...), secondDeletion...))
	fmt.Printf("6. VACUUM: %s\n", vacuumed.summary())
	fmt.Printf("   search for the deleted markers: %s\n", o.afterVacuum)
	check(vacuumed.freelist == 0 && vacuumed.pages <= again.pages-again.freelist, fmt.Sprintf("VACUUM is what returns space: %d pages -> %d, at least the %d freelist pages are gone", again.pages, vacuumed.pages, again.freelist))
	check(o.afterVacuum.rowsFound == 0, "after VACUUM no marker of a row deleted in step 2 or step 5 is left in the file")
	return o
}

func freshPath(dir, name string) string {
	path := filepath.Join(dir, name, "t.db")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		fatal("mkdir", err)
	}
	return path
}

func execOne(ctx context.Context, db *sqlitex.DB, statement string, args ...any) {
	if _, err := db.ExecContext(ctx, statement, args...); err != nil {
		fatal(statement, err)
	}
}

// largeTransaction verifies that allocateBtreePage (btree.c:6514-6822) pops
// freelist pages before extending the file on a per-allocation basis, so one
// transaction can drain the freelist and grow page_count in the same commit.
func largeTransaction(ctx context.Context, dir string) {
	fmt.Println()
	fmt.Println("== a transaction that needs more pages than the freelist holds (secure_delete=ON, WAL)")
	path := freshPath(dir, "large-transaction")
	insert := "INSERT INTO t(id, v) VALUES(?, ?)"
	var freeBefore, freeAfter, pagesBefore, pagesAfter int
	session(ctx, path, true, func(db *sqlitex.DB) {
		execOne(ctx, db, "CREATE TABLE t(id INTEGER PRIMARY KEY, v BLOB)")
		execAll(ctx, db, insert, insertRows("SECRET", 1, 40))
		execOne(ctx, db, "DELETE FROM t WHERE id <= 8")
		freeBefore, pagesBefore = pragmaInt(ctx, db, "freelist_count"), pragmaInt(ctx, db, "page_count")
		execAll(ctx, db, insert, insertRows("NEW", 101, 32))
		freeAfter, pagesAfter = pragmaInt(ctx, db, "freelist_count"), pragmaInt(ctx, db, "page_count")
	})
	fmt.Printf("40 rows, then DELETE of ids 1..8: freelist %d, page_count %d\n", freeBefore, pagesBefore)
	fmt.Printf("one transaction of 32 new rows (8 leaves' worth): freelist %d -> %d, page_count %d -> %d\n", freeBefore, freeAfter, pagesBefore, pagesAfter)
	check(freeBefore > 0 && freeAfter == 0 && pagesAfter > pagesBefore, "a transaction that needs more pages than the freelist holds takes the freelist pages first, then grows the file in the same transaction: so the rule is per page allocation, not per transaction")
}

// autoVacuumModes contrasts NONE (default, never shrinks without VACUUM), FULL
// (relocates pages and truncates during the DELETE's own commit), and
// INCREMENTAL (keeps freed pages on the freelist until PRAGMA incremental_vacuum).
func autoVacuumModes(ctx context.Context, dir string) {
	fmt.Println()
	fmt.Println("== auto_vacuum: the file shrinks on its own only when the database was created with auto_vacuum FULL or INCREMENTAL")
	type result struct{ created, afterDelete, freeAfterDelete, afterIncremental int64 }
	results := map[string]result{}
	for _, mode := range []string{sqlitex.AutoVacuumNone, sqlitex.AutoVacuumFull, sqlitex.AutoVacuumIncremental} {
		path := freshPath(dir, "auto-vacuum-"+strings.ToLower(mode))
		options := sqlitex.DefaultOptions()
		options.SingleConnection = true
		options.File.AutoVacuum = mode
		pages := func() int64 {
			if err := recipe.CheckAtRest(path); err != nil {
				fatal("at rest", err)
			}
			info, err := os.Stat(path)
			if err != nil {
				fatal("stat", err)
			}
			return info.Size() / pageSize
		}
		withDB := func(work func(db *sqlitex.DB)) {
			db, err := sqlitex.Open(ctx, path, options)
			if err != nil {
				fatal("open", err)
			}
			work(db)
			if err := db.Close(); err != nil {
				fatal("close", err)
			}
		}
		var r result
		withDB(func(db *sqlitex.DB) {
			execOne(ctx, db, "CREATE TABLE t(id INTEGER PRIMARY KEY, v BLOB)")
			execAll(ctx, db, "INSERT INTO t(id, v) VALUES(?, ?)", insertRows("SECRET", 1, 200))
		})
		r.created = pages()
		withDB(func(db *sqlitex.DB) {
			execOne(ctx, db, "DELETE FROM t WHERE id <= 150")
			r.freeAfterDelete = int64(pragmaInt(ctx, db, "freelist_count"))
		})
		r.afterDelete = pages()
		withDB(func(db *sqlitex.DB) {
			execOne(ctx, db, "PRAGMA incremental_vacuum")
		})
		r.afterIncremental = pages()
		results[mode] = r
		fmt.Printf("  auto_vacuum=%-11s 200 rows: %d pages; DELETE of 150 rows: %d pages, freelist %d; PRAGMA incremental_vacuum: %d pages\n", mode, r.created, r.afterDelete, r.freeAfterDelete, r.afterIncremental)
	}
	none, full, incremental := results[sqlitex.AutoVacuumNone], results[sqlitex.AutoVacuumFull], results[sqlitex.AutoVacuumIncremental]
	check(none.afterDelete == none.created && none.afterIncremental == none.created && none.freeAfterDelete > 0, "auto_vacuum=NONE (SQLite's default and the recipe's): the file never shrinks on its own; PRAGMA incremental_vacuum does nothing")
	check(full.afterDelete < full.created && full.freeAfterDelete == 0, "auto_vacuum=FULL: the DELETE's own commit moves pages and truncates the file, leaving no freelist")
	check(incremental.afterDelete == incremental.created && incremental.freeAfterDelete > 0 && incremental.afterIncremental < incremental.afterDelete, "auto_vacuum=INCREMENTAL: freed pages wait on the freelist until PRAGMA incremental_vacuum truncates the file")
}

func countMarkers(data []byte, r markerRange) int {
	found := 0
	for id := r.first; id <= r.last; id++ {
		if bytes.Contains(data, marker(r.prefix, id)) {
			found++
		}
	}
	return found
}

// walResidue shows that secure_delete=ON zeroes pages in the database file but
// does not erase earlier INSERT frames already sitting in -wal until the WAL
// is reset (for example by wal_checkpoint(TRUNCATE)).
func walResidue(ctx context.Context, dir string) {
	fmt.Println()
	fmt.Println("== secure_delete=ON and the -wal file (wal_autocheckpoint=0, one connection)")
	path := freshPath(dir, "wal-residue")
	options := sqlitex.DefaultOptions()
	options.SingleConnection = true
	options.Conn.SecureDelete = true
	options.Conn.WALAutocheckpoint = 0
	db, err := sqlitex.Open(ctx, path, options)
	if err != nil {
		fatal("open", err)
	}
	secrets := markerRange{"SECRET", 5001, 5040}
	inWAL := func() int {
		data, err := os.ReadFile(path + "-wal")
		if err != nil {
			fatal("read -wal", err)
		}
		return countMarkers(data, secrets)
	}
	checkpoint := func(mode string) {
		result, err := sqlitex.Checkpoint(ctx, db, "main", mode)
		if err != nil || result.Busy {
			fatal("checkpoint", fmt.Errorf("%v %+v", err, result))
		}
	}
	execOne(ctx, db, "CREATE TABLE t(id INTEGER PRIMARY KEY, v BLOB)")
	execAll(ctx, db, "INSERT INTO t(id, v) VALUES(?, ?)", insertRows(secrets.prefix, secrets.first, secrets.last-secrets.first+1))
	execOne(ctx, db, "DELETE FROM t")
	afterDelete := inWAL()
	checkpoint("PASSIVE")
	afterPassive := inWAL()
	checkpoint("TRUNCATE")
	afterTruncate := inWAL()
	if err := db.Close(); err != nil {
		fatal("close", err)
	}
	if err := recipe.CheckAtRest(path); err != nil {
		fatal("at rest", err)
	}
	image, err := os.ReadFile(path)
	if err != nil {
		fatal("read", err)
	}
	atRest := countMarkers(image, secrets)
	total := int(secrets.last - secrets.first + 1)
	fmt.Printf("%d rows inserted then deleted; deleted rows readable in the -wal: after the DELETE %d of %d, after wal_checkpoint(PASSIVE) %d, after wal_checkpoint(TRUNCATE) %d; in the database file at rest: %d\n", total, afterDelete, total, afterPassive, afterTruncate, atRest)
	check(afterDelete == total && afterPassive == total, "secure_delete zeroes the database pages, but the -wal still holds the frames the INSERT wrote: every deleted row stays readable there, even after a PASSIVE checkpoint copied the zeroed pages back")
	check(afterTruncate == 0 && atRest == 0, "only when the WAL is reset (here by wal_checkpoint(TRUNCATE)) are the deleted rows gone from the files on disk")
}

func main() {
	ctx := context.Background()
	sqlitex.MustPinCheckMemory(ctx)
	dir := sqlitex.MustWorkDir("3.2")
	fmt.Printf("INFO: work dir %s\n", displayPath(dir))
	fmt.Printf("settings: page_size=%d, journal_mode=wal, synchronous=FULL; files are read only after their connection closed\n", pageSize)
	off := run(ctx, dir, false)
	on := run(ctx, dir, true)
	fmt.Println()
	fmt.Println("== comparison")
	fmt.Printf("file sizes, secure_delete=OFF: %v\n", off.sizes)
	fmt.Printf("file sizes, secure_delete=ON:  %v\n", on.sizes)
	fmt.Printf("deleted rows readable after the delete: OFF %d, ON %d\n", off.afterDelete.rowsFound, on.afterDelete.rowsFound)
	fmt.Printf("deleted rows readable after the reuse:  OFF %d, ON %d\n", off.afterReuse.rowsFound, on.afterReuse.rowsFound)
	fmt.Printf("WAL frames written by the DELETE of %d rows: OFF %d, ON %d\n", deleteLast-deleteFirst+1, off.deleteFrames, on.deleteFrames)
	check(on.deleteFrames > off.deleteFrames, "secure_delete costs WAL frames: the zeroed pages are written, while without it freed leaf pages are not written at all")
	check(off.afterDelete.rowsFound > 0, "without secure_delete the deleted payload bytes are still in the file")
	check(off.afterDelete.byRole["freelist-leaf"] > 0, "without secure_delete freelist leaf pages keep their old content")
	check(off.afterSecondDelete.rowsFound > 0, "the second delete leaves its rows readable too without secure_delete")
	check(on.afterDelete.rowsFound == 0 && on.afterReuse.rowsFound == 0 && on.afterSecondDelete.rowsFound == 0, "with secure_delete the deleted payload bytes are nowhere in the database file at rest")
	check(fmt.Sprint(off.sizes) == fmt.Sprint(on.sizes), "secure_delete changes the bytes, not the layout: file sizes are the same at every step")
	largeTransaction(ctx, dir)
	autoVacuumModes(ctx, dir)
	walResidue(ctx, dir)
	fmt.Println()
	if len(failures) > 0 {
		fmt.Printf("DELETE AND REUSE FAILED: %d claim(s) did not hold\n", len(failures))
		for _, f := range failures {
			fmt.Printf("  - %s\n", f)
		}
		os.Exit(1)
	}
	fmt.Println("DELETE AND REUSE OK")
}

func displayPath(path string) string {
	root, err := sqlitex.RepoRoot()
	if err != nil {
		return path
	}
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	relative, err := filepath.Rel(root, path)
	if err != nil || strings.HasPrefix(relative, "..") {
		return path
	}
	return filepath.ToSlash(relative)
}
