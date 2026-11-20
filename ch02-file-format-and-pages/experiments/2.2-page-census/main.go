// Experiment 2.2 runs internal/dbfile's complete page census on the closed R0
// database, validates every B-tree and overflow page against SQLite's dbstat
// virtual table and every table's rows against SELECT, exercises the local
// payload boundary, multi-trunk freelists, auto-vacuum pointer-map pages, and
// the 1 GiB lock-byte page, and tests nine damaged files against integrity_check.
//
// Run from the repo root:
//
//	go run ./ch02-file-format-and-pages/experiments/2.2-page-census
package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/nickstrad/sqlite-internals/internal/dbfile"
	"github.com/nickstrad/sqlite-internals/internal/recipe"
	"github.com/nickstrad/sqlite-internals/internal/sqlitex"

	_ "modernc.org/sqlite"
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

func must[T any](value T, err error) func(string) T {
	return func(what string) T {
		if err != nil {
			fatal(what, err)
		}
		return value
	}
}

func openSQLite(path string, readOnly bool) *sql.DB {
	query := url.Values{}
	if readOnly {
		query.Set("mode", "ro")
	}
	uri := must(sqlitex.FileURI(path, query))("uri")
	db := must(sql.Open("sqlite", uri))("open")
	db.SetMaxOpenConns(1)
	return db
}

func queryInt(ctx context.Context, db *sql.DB, query string) int {
	var v int
	if err := db.QueryRowContext(ctx, query).Scan(&v); err != nil {
		fatal(query, err)
	}
	return v
}

func closeAtRest(db *sql.DB, path string) {
	if err := db.Close(); err != nil {
		fatal("close", err)
	}
	if err := recipe.CheckAtRest(path); err != nil {
		fatal("at rest", err)
	}
}

type statRow struct {
	Tree    string
	Type    string
	Cells   int
	Payload int
	Unused  int
	MaxPay  int
}

func readDBStat(ctx context.Context, db *sql.DB) map[int]statRow {
	rows, err := db.QueryContext(ctx, "SELECT pageno, name, pagetype, ncell, payload, unused, mx_payload FROM dbstat")
	if err != nil {
		fatal("dbstat", err)
	}
	defer rows.Close()
	out := map[int]statRow{}
	for rows.Next() {
		var n int
		var r statRow
		if err := rows.Scan(&n, &r.Tree, &r.Type, &r.Cells, &r.Payload, &r.Unused, &r.MaxPay); err != nil {
			fatal("dbstat row", err)
		}
		out[n] = r
	}
	if err := rows.Err(); err != nil {
		fatal("dbstat rows", err)
	}
	return out
}

func dbstatType(r dbfile.Role) string {
	switch r {
	case dbfile.RoleTableInterior, dbfile.RoleIndexInterior:
		return "internal"
	case dbfile.RoleTableLeaf, dbfile.RoleIndexLeaf:
		return "leaf"
	case dbfile.RoleOverflow:
		return "overflow"
	}
	return ""
}

type comparison struct {
	compared   int
	mismatches []string
	notInStat  int
}

func compareWithDBStat(c *dbfile.Census, stat map[int]statRow, printPages bool) comparison {
	var result comparison
	if printPages {
		fmt.Printf("  %5s  %-15s %-18s %5s %7s %7s %7s  %s\n", "page", "role", "tree", "ncell", "payload", "unused", "mx_pay", "dbstat")
	}
	for _, p := range c.Pages {
		kind := dbstatType(p.Role)
		verdict := "not a b-tree page (not in dbstat)"
		if kind == "" {
			if _, listed := stat[p.Number]; listed {
				verdict = "LISTED BY DBSTAT"
				result.mismatches = append(result.mismatches, fmt.Sprintf("page %d (%s) is in dbstat", p.Number, p.Role))
			}
			result.notInStat++
		} else {
			ours := statRow{p.Tree, kind, p.CellCount, p.Payload, p.Unused, int(p.MaxPayload)}
			theirs, listed := stat[p.Number]
			result.compared++
			switch {
			case !listed:
				verdict = "MISSING FROM DBSTAT"
				result.mismatches = append(result.mismatches, fmt.Sprintf("page %d missing from dbstat", p.Number))
			case ours != theirs:
				verdict = fmt.Sprintf("DIFFERS: %+v", theirs)
				result.mismatches = append(result.mismatches, fmt.Sprintf("page %d: ours %+v dbstat %+v", p.Number, ours, theirs))
			default:
				verdict = "match"
			}
		}
		if printPages {
			fmt.Printf("  %5d  %-15s %-18s %5d %7d %7d %7d  %s\n", p.Number, p.Role, p.Tree, p.CellCount, p.Payload, p.Unused, p.MaxPayload, verdict)
		}
	}
	if result.compared != len(stat) {
		result.mismatches = append(result.mismatches, fmt.Sprintf("dbstat lists %d pages, the census has %d b-tree and overflow pages", len(stat), result.compared))
	}
	return result
}

func printRoleCounts(c *dbfile.Census) int {
	counts := c.RoleCounts()
	total := 0
	var parts []string
	for _, r := range dbfile.AllRoles {
		total += counts[r]
		parts = append(parts, fmt.Sprintf("%s=%d", r, counts[r]))
	}
	fmt.Printf("  roles: %s; sum %d\n", strings.Join(parts, " "), total)
	return total
}

func census(path string) (*dbfile.File, *dbfile.Census) {
	image := must(os.ReadFile(path))("read " + filepath.Base(path))
	f := must(dbfile.FromBytes(image))("parse " + filepath.Base(path))
	c := must(f.Census())("census " + filepath.Base(path))
	return f, c
}

func treeNamed(c *dbfile.Census, name string) dbfile.Tree {
	for _, t := range c.Trees {
		if t.Name == name {
			return t
		}
	}
	fatal("tree "+name, errors.New("not in the schema"))
	return dbfile.Tree{}
}

func canonicalWrite(h hash.Hash, class string, text string) {
	fmt.Fprintf(h, "%s:%d:%s|", class, len(text), text)
}

func ourValue(h hash.Hash, v dbfile.Value) {
	switch v.Class {
	case dbfile.Null:
		canonicalWrite(h, "null", "")
	case dbfile.Integer:
		canonicalWrite(h, "integer", fmt.Sprint(v.Int))
	case dbfile.Real:
		canonicalWrite(h, "real", fmt.Sprintf("%016x", math.Float64bits(v.Float)))
	case dbfile.Text:
		canonicalWrite(h, "text", hex.EncodeToString(v.Bytes))
	default:
		canonicalWrite(h, "blob", hex.EncodeToString(v.Bytes))
	}
}

func sqliteValue(h hash.Hash, class string, value any) {
	switch class {
	case "null":
		canonicalWrite(h, "null", "")
	case "integer":
		canonicalWrite(h, "integer", fmt.Sprint(value.(int64)))
	case "real":
		canonicalWrite(h, "real", fmt.Sprintf("%016x", math.Float64bits(value.(float64))))
	case "text":
		canonicalWrite(h, "text", hex.EncodeToString([]byte(value.(string))))
	default:
		canonicalWrite(h, "blob", hex.EncodeToString(value.([]byte)))
	}
}

type tableHash struct {
	rows int
	sum  string
}

// ourTableHash walks a table B-tree directly from raw bytes and hashes its
// canonical rows; column 0 NULL is replaced by the rowid because SQLite stores
// an INTEGER PRIMARY KEY column as NULL in the record and keeps its value in
// the cell's rowid varint.
func ourTableHash(f *dbfile.File, root int) tableHash {
	h := sha256.New()
	rows := 0
	err := f.WalkTable(root, func(r dbfile.Row) error {
		rows++
		fmt.Fprintf(h, "row %d|", r.Rowid)
		for i, v := range r.Values {
			if i == 0 && v.Class == dbfile.Null {
				v = dbfile.Value{Class: dbfile.Integer, Int: r.Rowid}
			}
			ourValue(h, v)
		}
		h.Write([]byte("\n"))
		return nil
	})
	if err != nil {
		fatal("walk table", err)
	}
	return tableHash{rows, hex.EncodeToString(h.Sum(nil))}
}

func sqliteTableHash(ctx context.Context, db *sql.DB, table string, columns []string) tableHash {
	var selects []string
	for _, c := range columns {
		selects = append(selects, fmt.Sprintf("typeof(%s), %s", c, c))
	}
	rows, err := db.QueryContext(ctx, fmt.Sprintf("SELECT rowid, %s FROM %s ORDER BY rowid", strings.Join(selects, ", "), table))
	if err != nil {
		fatal("select "+table, err)
	}
	defer rows.Close()
	h := sha256.New()
	count := 0
	for rows.Next() {
		var rowid int64
		cells := make([]any, 2*len(columns))
		targets := []any{&rowid}
		for i := range cells {
			targets = append(targets, &cells[i])
		}
		if err := rows.Scan(targets...); err != nil {
			fatal("scan", err)
		}
		count++
		fmt.Fprintf(h, "row %d|", rowid)
		for i := range columns {
			sqliteValue(h, cells[2*i].(string), cells[2*i+1])
		}
		h.Write([]byte("\n"))
	}
	if err := rows.Err(); err != nil {
		fatal("rows", err)
	}
	return tableHash{count, hex.EncodeToString(h.Sum(nil))}
}

var recipeColumns = map[string][]string{
	"accounts":  {"id", "name", "balance"},
	"documents": {"id", "account_id", "title", "body"},
	"events":    {"id", "account_id", "kind", "amount", "note"},
}

func partA(ctx context.Context, dir string) {
	fmt.Println()
	fmt.Println("== A. census of the R0 database (recipe default, WAL, 30 steps, closed)")
	r := recipe.Default()
	path := filepath.Join(dir, "r0.db")
	if err := r.Build(ctx, path, r.Steps); err != nil {
		fatal("build", err)
	}
	f, c := census(path)
	fmt.Printf("  page size %d, usable %d, page count %d (from the %s)\n", c.PageSize, c.UsableSize, c.PageCount, f.PageCountSource())
	fmt.Println("  schema table (page 1), decoded by our record reader:")
	for _, e := range c.Schema {
		fmt.Printf("    rowid %d: type=%s name=%s tbl_name=%s rootpage=%d sql=%q\n", e.Rowid, e.Type, e.Name, e.TableName, e.RootPage, e.SQL)
	}
	fmt.Println("  b-trees walked from those roots:")
	for _, t := range c.SortedTrees() {
		fmt.Printf("    %-18s %-5s root %3d: %3d b-tree page(s), %3d overflow page(s), %4d cell(s)\n", t.Name, t.Type, t.RootPage, t.Pages, t.Overflow, t.Cells)
	}
	fmt.Printf("  freelist from header offset 32: trunk(s) %v, leaves %v\n", c.FreelistTrunks, c.FreelistLeaves)
	total := printRoleCounts(c)
	check(total == c.PageCount, fmt.Sprintf("the census sums to page_count %d", c.PageCount))
	check(c.Complete() == nil, "every page has exactly one role: none counted twice (the census fails on a second claim), none missed")

	db := openSQLite(path, false)
	pageCount := queryInt(ctx, db, "PRAGMA page_count")
	freelist := queryInt(ctx, db, "PRAGMA freelist_count")
	stat := readDBStat(ctx, db)
	fmt.Println("  per-page census against dbstat:")
	cmp := compareWithDBStat(c, stat, true)
	check(pageCount == c.PageCount, fmt.Sprintf("PRAGMA page_count = %d", pageCount))
	check(freelist == c.FreelistLength(), fmt.Sprintf("freelist length %d = PRAGMA freelist_count %d", c.FreelistLength(), freelist))
	check(len(cmp.mismatches) == 0, fmt.Sprintf("all %d b-tree and overflow pages match dbstat in tree, type, ncell, payload, unused and mx_payload; the %d other pages are not in dbstat", cmp.compared, cmp.notInStat))
	for _, m := range cmp.mismatches {
		fmt.Println("    MISMATCH", m)
	}

	fmt.Println("  overflow chains of the documents table:")
	var chains, overflowPages, longest int
	var shown int
	err := f.WalkTable(treeNamed(c, "documents").RootPage, func(row dbfile.Row) error {
		if len(row.OverflowPages) == 0 {
			return nil
		}
		chains++
		overflowPages += len(row.OverflowPages)
		longest = max(longest, len(row.OverflowPages))
		if shown < 5 {
			shown++
			fmt.Printf("    row %d: body %d bytes, leaf page %d, overflow chain %v\n", row.Rowid, len(row.Values[3].Bytes), row.Page, row.OverflowPages)
		}
		return nil
	})
	if err != nil {
		fatal("documents", err)
	}
	fmt.Printf("    %d rows overflow, %d overflow pages in all, longest chain %d pages\n", chains, overflowPages, longest)
	check(overflowPages == c.RoleCounts()[dbfile.RoleOverflow], "the chains followed from the documents rows hold every overflow page in the file")

	fmt.Println("  row hashes, our parser vs SQLite (SELECT ... ORDER BY rowid, every value tagged with typeof()):")
	for _, e := range c.Schema {
		columns, isRecipe := recipeColumns[e.Name]
		if !isRecipe {
			continue
		}
		ours := ourTableHash(f, e.RootPage)
		theirs := sqliteTableHash(ctx, db, e.Name, columns)
		fmt.Printf("    %-9s rows %4d / %4d  sha256 %s / %s\n", e.Name, ours.rows, theirs.rows, ours.sum[:16], theirs.sum[:16])
		check(ours == theirs, fmt.Sprintf("table %s: same rows and values, read from the bytes and through SQLite", e.Name))
	}
	closeAtRest(db, path)
}

func newDB(ctx context.Context, path string, edit func(*sqlitex.Options), statements ...string) {
	options := sqlitex.DefaultOptions()
	options.SingleConnection = true
	options.Conn.SecureDelete = true
	if edit != nil {
		edit(&options)
	}
	db := must(sqlitex.Open(ctx, path, options))("open " + filepath.Base(path))
	for _, s := range statements {
		if _, err := db.ExecContext(ctx, s); err != nil {
			fatal(s, err)
		}
	}
	if err := db.Close(); err != nil {
		fatal("close", err)
	}
	if err := recipe.CheckAtRest(path); err != nil {
		fatal("at rest", err)
	}
}

func checkAgainstSQLite(ctx context.Context, path string, c *dbfile.Census, label string) {
	db := openSQLite(path, false)
	pageCount := queryInt(ctx, db, "PRAGMA page_count")
	freelist := queryInt(ctx, db, "PRAGMA freelist_count")
	cmp := compareWithDBStat(c, readDBStat(ctx, db), false)
	closeAtRest(db, path)
	check(pageCount == c.PageCount && freelist == c.FreelistLength() && c.Complete() == nil && len(cmp.mismatches) == 0,
		fmt.Sprintf("%s: census complete, page_count %d and freelist_count %d agree, %d pages match dbstat", label, pageCount, freelist, cmp.compared))
	for _, m := range cmp.mismatches {
		fmt.Println("    MISMATCH", m)
	}
}

func partB(ctx context.Context, dir string) {
	fmt.Println()
	fmt.Println("== B. a payload exactly at the local limit, and one byte over (table leaf, usable size 4096)")
	maxLocal, minLocal := dbfile.MaxLocalPayload(4096, dbfile.TableLeaf), dbfile.MinLocalPayload(4096)
	fmt.Printf("  table leaf: maxLocal = usable-35 = %d, minLocal = (usable-12)*32/255-23 = %d; index: maxLocal = (usable-12)*64/255-23 = %d\n", maxLocal, minLocal, dbfile.MaxLocalPayload(4096, dbfile.IndexLeaf))
	path := filepath.Join(dir, "limit.db")
	newDB(ctx, path, nil,
		"CREATE TABLE b(id INTEGER PRIMARY KEY, v BLOB)",
		"INSERT INTO b VALUES(1, zeroblob(4057))",
		"INSERT INTO b VALUES(2, zeroblob(4058))",
		"INSERT INTO b VALUES(3, zeroblob(8149))")
	f, c := census(path)
	type split struct {
		payload    uint64
		local      int
		overflow   int
		lastFilled int
	}
	splits := map[int64]split{}
	err := f.WalkTable(2, func(row dbfile.Row) error {
		page := must(f.BTreePage(row.Page))("page")
		for i := range page.CellCount {
			cell := must(page.Cell(i))("cell")
			if cell.Rowid != row.Rowid {
				continue
			}
			fmt.Printf("  row %d: blob %d bytes, payload %d bytes (record header 4 + blob), local %d, overflow pages %d\n", row.Rowid, len(row.Values[1].Bytes), cell.PayloadSize, cell.LocalSize, len(row.OverflowPages))
			chain := must(f.OverflowChain(row.Page, cell))("chain")
			s := split{payload: cell.PayloadSize, local: cell.LocalSize, overflow: len(chain)}
			if len(chain) > 0 {
				s.lastFilled = chain[len(chain)-1].Payload
			}
			splits[row.Rowid] = s
		}
		return nil
	})
	if err != nil {
		fatal("walk", err)
	}
	check(len(splits) == 3, "limit.db holds the three rows 1, 2 and 3")
	check(splits[1] == split{4061, 4061, 0, 0}, "row 1: payload 4061 (= maxLocal) stays entirely on the leaf")
	check(splits[2] == split{4062, minLocal, 1, 4062 - minLocal}, fmt.Sprintf("row 2: payload 4062 keeps only minLocal %d bytes on the leaf and spills %d to one overflow page", minLocal, 4062-minLocal))
	check(splits[3] == split{8153, 4061, 1, 4092}, "row 3: payload 8153 keeps 4061 on the leaf so its overflow exactly fills one page (4092 bytes)")
	checkAgainstSQLite(ctx, path, c, "local limit")
}

func partC(ctx context.Context, dir string) {
	fmt.Println()
	fmt.Println("== C. a freelist longer than one trunk page")
	path := filepath.Join(dir, "freelist.db")
	newDB(ctx, path, func(o *sqlitex.Options) { o.File.PageSize = 1024 },
		"CREATE TABLE big(id INTEGER PRIMARY KEY, v BLOB)",
		"WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i < 600) INSERT INTO big SELECT i, zeroblob(1000) FROM n ORDER BY i",
		"DELETE FROM big")
	_, c := census(path)
	fmt.Printf("  page size 1024: a trunk holds at most usable/4-2 = %d leaf numbers\n", c.UsableSize/4-2)
	for _, trunk := range c.FreelistTrunks {
		fmt.Printf("  trunk page %d lists %d leaves, next trunk is the next in this list\n", trunk, c.Page(trunk).CellCount)
	}
	fmt.Printf("  %d trunk(s) + %d leaves = %d free pages of %d\n", len(c.FreelistTrunks), len(c.FreelistLeaves), c.FreelistLength(), c.PageCount)
	printRoleCounts(c)
	check(len(c.FreelistTrunks) >= 2, "the freelist spans more than one trunk page")
	checkAgainstSQLite(ctx, path, c, "long freelist")
}

func partD(ctx context.Context, dir string) {
	fmt.Println()
	fmt.Println("== D. auto_vacuum=FULL adds pointer-map pages (the course runs auto_vacuum=NONE; the parser still accounts for them)")
	path := filepath.Join(dir, "autovacuum.db")
	newDB(ctx, path, func(o *sqlitex.Options) { o.File.PageSize = 1024; o.File.AutoVacuum = sqlitex.AutoVacuumFull },
		"CREATE TABLE big(id INTEGER PRIMARY KEY, v BLOB)",
		"WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i < 400) INSERT INTO big SELECT i, zeroblob(900) FROM n ORDER BY i")
	f, c := census(path)
	var maps []int
	for _, p := range c.Pages {
		if p.Role == dbfile.RolePointerMap {
			maps = append(maps, p.Number)
		}
	}
	fmt.Printf("  header byte 52 (largest root page) = %d, so auto-vacuum is on (%s)\n", f.Header.LargestRootPage, f.Header.AutoVacuumName())
	fmt.Printf("  pointer-map pages: one every usable/5+1 = %d pages from page 2: %v\n", c.UsableSize/5+1, maps)
	printRoleCounts(c)
	check(slices.Equal(maps, []int{2, 2 + c.UsableSize/5 + 1}), "pointer-map pages are pages 2 and 2 + usable/5 + 1 in this 408-page file")
	checkAgainstSQLite(ctx, path, c, "auto_vacuum=FULL")
}

func partE(ctx context.Context, dir string) {
	fmt.Println()
	fmt.Println("== E. a file larger than 1 GiB: the page holding byte offset 1073741824 (the lock-byte page) is never used")
	// Construct a > 1 GiB sparse file at page size 65536 so the 1 GiB
	// lock-byte page (1073741824/65536 + 1 = 16385) lies inside the database
	// without allocating 1 GiB of disk blocks; page 3 lists all remaining pages
	// except 16385 as freelist leaves so SQLite's integrity_check passes.
	path := filepath.Join(dir, "large.db")
	newDB(ctx, path, func(o *sqlitex.Options) { o.File.PageSize = 65536; o.File.JournalMode = sqlitex.JournalDelete },
		"CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT)", "INSERT INTO t VALUES(1, 'x')")
	const pageSize = 65536
	lock := dbfile.LockByteOffset/pageSize + 1
	pages := lock + 1
	image := must(os.ReadFile(path))("read")
	var leaves []int
	for n := 4; n <= pages; n++ {
		if n != lock {
			leaves = append(leaves, n)
		}
	}
	binary.BigEndian.PutUint32(image[28:], uint32(pages))
	binary.BigEndian.PutUint32(image[32:], 3)
	binary.BigEndian.PutUint32(image[36:], uint32(1+len(leaves)))
	trunk := make([]byte, pageSize)
	binary.BigEndian.PutUint32(trunk[4:], uint32(len(leaves)))
	for i, leaf := range leaves {
		binary.BigEndian.PutUint32(trunk[8+4*i:], uint32(leaf))
	}
	out := must(os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0))("open for edit")
	_, err := out.Write(image[:2*pageSize])
	if err == nil {
		_, err = out.WriteAt(trunk, 2*pageSize)
	}
	if err == nil {
		err = out.Truncate(int64(pages) * pageSize)
	}
	if err == nil {
		err = out.Close()
	}
	if err != nil {
		fatal("write sparse file", err)
	}
	fmt.Printf("  built by editing a 2-page database (page size 65536): in-header size %d pages, page 3 a freelist trunk listing pages 4..%d except %d; the rest of the file is a hole (sparse)\n", pages, pages, lock)
	fmt.Printf("  file size %d bytes (1 GiB = 1073741824); lock-byte page = 1073741824/65536 + 1 = %d\n", int64(pages)*pageSize, lock)
	fmt.Printf("INFO: blocks on disk: %s\n", diskUsage(path))
	f := must(dbfile.Open(path))("parse " + filepath.Base(path))
	c, err := f.Census()
	if closeErr := f.Close(); closeErr != nil {
		fatal("close parsed file", closeErr)
	}
	if err != nil {
		fatal("census "+filepath.Base(path), err)
	}
	printRoleCounts(c)
	check(c.Page(lock).Role == dbfile.RoleLockByte && c.Complete() == nil, fmt.Sprintf("page %d is counted as the lock-byte page, not as lost", lock))
	db := openSQLite(path, true)
	var integrity string
	if err := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil {
		fatal("integrity_check", err)
	}
	freelist := queryInt(ctx, db, "PRAGMA freelist_count")
	pageCount := queryInt(ctx, db, "PRAGMA page_count")
	closeAtRest(db, path)
	fmt.Printf("  SQLite: integrity_check=%s page_count=%d freelist_count=%d\n", integrity, pageCount, freelist)
	check(integrity == "ok" && pageCount == c.PageCount && freelist == c.FreelistLength(), "SQLite accepts the file: integrity_check ok, and page_count and freelist_count equal the census")
	if err := os.Remove(path); err != nil {
		fatal("remove large file", err)
	}
}

type corruption struct {
	name string
	edit func(image []byte)
}

func partF(ctx context.Context, dir string) {
	fmt.Println()
	fmt.Println("== F. what the inspector must reject: one damaged structure per file, our error vs SQLite's integrity_check")
	// Each case damages one structure in a 6-page rollback-mode copy; rollback
	// mode and mode=ro keep SQLite from altering the damaged file on open.
	base := filepath.Join(dir, "reject-base.db")
	newDB(ctx, base, func(o *sqlitex.Options) { o.File.JournalMode = sqlitex.JournalDelete },
		"CREATE TABLE t(id INTEGER PRIMARY KEY, v BLOB)",
		"INSERT INTO t VALUES(1, zeroblob(10000))",
		"INSERT INTO t VALUES(2, 'two')",
		"INSERT INTO t VALUES(3, zeroblob(10000))",
		"DELETE FROM t WHERE id = 3")
	good := must(os.ReadFile(base))("read")
	_, c := census(base)
	fmt.Printf("  base: %d pages; table t root 2; roles: ", c.PageCount)
	var roles []string
	for _, p := range c.Pages {
		roles = append(roles, fmt.Sprintf("%d=%s", p.Number, p.Role))
	}
	fmt.Println(strings.Join(roles, " "))
	page2 := 4096
	cellPointer := page2 + 8
	firstCell := int(binary.BigEndian.Uint16(good[cellPointer:]))
	overflowOfRow1 := 0
	for _, p := range c.Pages {
		if p.Role == dbfile.RoleOverflow && overflowOfRow1 == 0 {
			overflowOfRow1 = p.Number
		}
	}
	cases := []corruption{
		{"magic: byte 0 changed", func(b []byte) { b[0] = 'X' }},
		{"truncated: last page (a freelist leaf) cut in half", nil},
		{"page 2 type byte 0x0d -> 0x07", func(b []byte) { b[page2] = 0x07 }},
		{"page 2 cell count 2 -> 3000", func(b []byte) { binary.BigEndian.PutUint16(b[page2+3:], 3000) }},
		{"page 2 first cell pointer -> 9", func(b []byte) { binary.BigEndian.PutUint16(b[cellPointer:], 9) }},
		{"row 1 payload size varint -> 0x7f", func(b []byte) { b[page2+firstCell] = 0x7f }},
		{fmt.Sprintf("overflow page %d points back to itself", overflowOfRow1), func(b []byte) {
			binary.BigEndian.PutUint32(b[(overflowOfRow1-1)*4096:], uint32(overflowOfRow1))
		}},
		{"freelist trunk (header 32) -> page 2, a table page", func(b []byte) { binary.BigEndian.PutUint32(b[32:], 2) }},
		{"freelist count (header 36) + 1", func(b []byte) { b[39]++ }},
	}
	for i, k := range cases {
		image := append([]byte(nil), good...)
		if k.edit == nil {
			image = image[:len(image)-2048]
		} else {
			k.edit(image)
		}
		path := filepath.Join(dir, fmt.Sprintf("reject-%d.db", i+1))
		if err := os.WriteFile(path, image, 0o644); err != nil {
			fatal("write", err)
		}
		ours := parseAll(image)
		theirs := sqliteVerdict(ctx, path)
		fmt.Printf("  F%d %s\n    ours:   %s\n    SQLite: %s\n", i+1, k.name, ours, theirs)
		check(ours != "accepted" && !strings.HasPrefix(ours, "PANIC"), fmt.Sprintf("F%d: our parser returns an error, without panicking", i+1))
		if k.edit == nil {
			check(strings.HasSuffix(theirs, ": ok"), "F2: SQLite rounds a partial last page up and reads the missing bytes as zeros, so it reports ok; our parser is stricter and refuses the file")
		}
	}
}

func parseAll(image []byte) (verdict string) {
	defer func() {
		if r := recover(); r != nil {
			verdict = fmt.Sprintf("PANIC %v", r)
		}
	}()
	f, err := dbfile.FromBytes(image)
	if err != nil {
		return "error: " + err.Error()
	}
	c, err := f.Census()
	if err == nil {
		err = c.Complete()
	}
	if err == nil {
		for _, e := range c.Schema {
			if e.Type == "table" && e.RootPage > 0 {
				if err = f.WalkTable(e.RootPage, func(dbfile.Row) error { return nil }); err != nil {
					break
				}
			}
		}
	}
	if err != nil {
		return "error: " + err.Error()
	}
	return "accepted"
}

func sqliteVerdict(ctx context.Context, path string) string {
	db := openSQLite(path, true)
	defer db.Close()
	rows, err := db.QueryContext(ctx, "PRAGMA integrity_check")
	if err != nil {
		return "error: " + err.Error()
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return "error: " + err.Error()
		}
		lines = append(lines, strings.Split(line, "\n")...)
	}
	if err := rows.Err(); err != nil {
		return "error: " + err.Error()
	}
	if len(lines) > 4 {
		lines = append(lines[:4], fmt.Sprintf("(+%d more lines)", len(lines)-4))
	}
	return "integrity_check: " + strings.Join(lines, " / ")
}

func main() {
	ctx := context.Background()
	sqlitex.MustPinCheckMemory(ctx)
	dir := sqlitex.MustWorkDir("2.2")
	fmt.Printf("INFO: work dir %s\n", displayPath(dir))
	partA(ctx, dir)
	partB(ctx, dir)
	partC(ctx, dir)
	partD(ctx, dir)
	partE(ctx, dir)
	partF(ctx, dir)
	fmt.Println()
	if len(failures) > 0 {
		fmt.Printf("CENSUS FAILED: %d claim(s) did not hold\n", len(failures))
		for _, f := range failures {
			fmt.Printf("  - %s\n", f)
		}
		os.Exit(1)
	}
	fmt.Println("CENSUS OK")
}

var errNoUsage = errors.New("disk usage unknown on this system")

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
