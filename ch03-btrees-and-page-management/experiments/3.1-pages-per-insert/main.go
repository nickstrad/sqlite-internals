package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nickstrad/sqlite-internals/internal/capture"
	"github.com/nickstrad/sqlite-internals/internal/dbfile"
	"github.com/nickstrad/sqlite-internals/internal/equal"
	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
)

const (
	pageSize      = 4096
	blobBytes     = 1000
	rowidStep     = 2
	walHeaderSize = 32
	walFrameHead  = 24
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

func rowBlob(rowid int64) []byte {
	label := fmt.Sprintf("row %06d ", rowid)
	return bytes.Repeat([]byte(label), blobBytes/len(label)+1)[:blobBytes]
}

type lab struct {
	path     string
	mode     string
	db       *sqlitex.DB
	conn     *sql.Conn
	file     *dbfile.File
	manifest equal.Manifest
	image    []byte
	census   *dbfile.Census
}

type step struct {
	number    int
	rowid     int64
	changes   []equal.PageChange
	walPages  []int
	before    *dbfile.File
	after     *dbfile.File
	depth     int
	rootPage  int
	integrity string
}

func openLab(ctx context.Context, path, mode string, statements ...string) *lab {
	options := sqlitex.DefaultOptions()
	options.SingleConnection = true
	options.File.JournalMode = mode
	options.Conn.Synchronous = sqlitex.SyncOff
	options.Conn.WALAutocheckpoint = 0
	db, err := sqlitex.Open(ctx, path, options)
	if err != nil {
		fatal("open "+filepath.Base(path), err)
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		fatal("conn", err)
	}
	l := &lab{path: path, mode: mode, db: db, conn: conn}
	for _, s := range statements {
		if _, err := conn.ExecContext(ctx, s); err != nil {
			fatal(s, err)
		}
	}
	l.file, l.manifest = l.snapshot(ctx)
	return l
}

func (l *lab) close() {
	if err := l.conn.Close(); err != nil {
		fatal("close conn", err)
	}
	if err := l.db.Close(); err != nil {
		fatal("close db", err)
	}
}

func (l *lab) snapshot(ctx context.Context) (*dbfile.File, equal.Manifest) {
	if l.mode == "wal" {
		result, err := sqlitex.Checkpoint(ctx, l.conn, "main", "TRUNCATE")
		if err != nil || result.Busy {
			fatal("checkpoint", fmt.Errorf("%v %+v", err, result))
		}
	}
	var count int
	if err := l.conn.QueryRowContext(ctx, "PRAGMA page_count").Scan(&count); err != nil {
		fatal("page_count", err)
	}
	image := make([]byte, 0, count*pageSize)
	rows, err := l.conn.QueryContext(ctx, "SELECT pgno, data FROM sqlite_dbpage ORDER BY pgno")
	if err != nil {
		fatal("sqlite_dbpage", err)
	}
	expected := int64(1)
	for rows.Next() {
		var pgno int64
		var data sql.RawBytes
		if err := rows.Scan(&pgno, &data); err != nil {
			fatal("sqlite_dbpage", err)
		}
		if pgno != expected || len(data) != pageSize {
			fatal("sqlite_dbpage", fmt.Errorf("page %d (%d bytes), want page %d", pgno, len(data), expected))
		}
		expected++
		image = append(image, data...)
	}
	if err := rows.Close(); err != nil {
		fatal("sqlite_dbpage", err)
	}
	if int(expected-1) != count {
		fatal("sqlite_dbpage", fmt.Errorf("%d pages, page_count says %d", expected-1, count))
	}
	f, err := dbfile.FromBytes(image)
	if err != nil {
		fatal("parse image", err)
	}
	census, err := f.Census()
	if err == nil {
		err = census.Complete()
	}
	if err != nil {
		fatal("census", err)
	}
	m := equal.Manifest{PageSize: pageSize, Entries: make([]equal.ManifestEntry, count)}
	for n := 1; n <= count; n++ {
		page := image[(n-1)*pageSize : n*pageSize]
		sum := ""
		if l.image != nil && n*pageSize <= len(l.image) && bytes.Equal(page, l.image[(n-1)*pageSize:n*pageSize]) {
			sum = l.manifest.Entries[n-1].SHA256
		} else {
			sum = equal.SHA256Hex(page)
		}
		m.Entries[n-1] = equal.ManifestEntry{Page: n, Role: census.Page(n).Role.String(), SHA256: sum}
	}
	l.image = image
	l.census = census
	return f, m
}

func (l *lab) verifyManifest() bool {
	full, err := capture.BuildManifest(l.file)
	if err != nil {
		fatal("manifest", err)
	}
	return full.Text() == l.manifest.Text()
}

func readWALPages(path string) ([]int, error) {
	data, err := os.ReadFile(path + "-wal")
	if err != nil {
		return nil, err
	}
	if len(data) < walHeaderSize {
		return nil, fmt.Errorf("-wal holds %d bytes, less than its header", len(data))
	}
	magic := binary.BigEndian.Uint32(data[0:4])
	if magic != 0x377f0682 && magic != 0x377f0683 {
		return nil, fmt.Errorf("-wal magic %#x", magic)
	}
	frameSize := walFrameHead + pageSize
	if (len(data)-walHeaderSize)%frameSize != 0 {
		return nil, fmt.Errorf("-wal size %d is not a header plus whole frames", len(data))
	}
	var pages []int
	for off := walHeaderSize; off < len(data); off += frameSize {
		pages = append(pages, int(binary.BigEndian.Uint32(data[off:off+4])))
	}
	return pages, nil
}

func (l *lab) integrity(ctx context.Context) string {
	var result string
	if err := l.conn.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&result); err != nil {
		fatal("integrity_check", err)
	}
	return result
}

func (l *lab) rootOf(table string) int {
	schema, err := l.file.Schema()
	if err != nil {
		fatal("schema", err)
	}
	for _, e := range schema {
		if e.Name == table {
			return int(e.RootPage)
		}
	}
	fatal("schema", fmt.Errorf("no table %q", table))
	return 0
}

func (l *lab) depth(root int) int {
	deepest := 0
	for n := 1; n <= l.census.PageCount; n++ {
		info := l.census.Page(n)
		if info.Role != dbfile.RoleTableInterior && info.Role != dbfile.RoleTableLeaf && info.Role != dbfile.RoleIndexInterior && info.Role != dbfile.RoleIndexLeaf {
			continue
		}
		d, page := 1, n
		for page != root && page != 0 {
			page = l.census.Page(page).Parent
			d++
		}
		if page == root {
			deepest = max(deepest, d)
		}
	}
	return deepest
}

func (l *lab) exec(ctx context.Context, number int, rowid int64, table, statement string, args ...any) step {
	before := l.file
	if _, err := l.conn.ExecContext(ctx, statement, args...); err != nil {
		fatal(statement, err)
	}
	s := step{number: number, rowid: rowid, before: before}
	if l.mode == "wal" {
		pages, err := readWALPages(l.path)
		if err != nil {
			fatal("read -wal", err)
		}
		s.walPages = pages
	}
	file, manifest := l.snapshot(ctx)
	s.changes = equal.ChangedPages(l.manifest, manifest)
	s.after = file
	l.file, l.manifest = file, manifest
	s.rootPage = l.rootOf(table)
	s.depth = l.depth(s.rootPage)
	s.integrity = l.integrity(ctx)
	return s
}

func (s step) pages() []int { return equal.PageNumbers(s.changes) }

func sameInts(a, b []int) bool {
	x := append([]int(nil), a...)
	y := append([]int(nil), b...)
	sort.Ints(x)
	sort.Ints(y)
	if len(x) != len(y) {
		return false
	}
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

func subtract(a, b []int) []int {
	in := map[int]bool{}
	for _, x := range b {
		in[x] = true
	}
	var out []int
	for _, x := range a {
		if !in[x] {
			out = append(out, x)
		}
	}
	return out
}

func keysOf(p *dbfile.BTreePage) string {
	cells, err := p.Cells()
	if err != nil {
		fatal("cells", err)
	}
	if len(cells) == 0 {
		return "none"
	}
	key := func(c dbfile.Cell) string {
		if c.HasRowid {
			return fmt.Sprint(c.Rowid)
		}
		return "?"
	}
	if len(cells) <= 6 {
		var keys []string
		for _, c := range cells {
			keys = append(keys, key(c))
		}
		return strings.Join(keys, ",")
	}
	return key(cells[0]) + ".." + key(cells[len(cells)-1])
}

func describePage(f *dbfile.File, n int) string {
	if f == nil || n > f.PageCount() {
		return "(not in the file)"
	}
	if n == 1 {
		return fmt.Sprintf("page 1 header: in-header size %d, freelist %d; sqlite_schema leaf with %d cell(s)", f.Header.DatabaseSize, f.Header.FreelistCount, mustPage(f, 1).CellCount)
	}
	p, err := f.BTreePage(n)
	if err != nil {
		return "not a b-tree page"
	}
	text := fmt.Sprintf("%s cells=%d keys=%s content-start=%d unused=%d", p.Type, p.CellCount, keysOf(p), p.CellContentStart, p.Unused())
	if !p.Type.IsLeaf() {
		cells, _ := p.Cells()
		var children []string
		for _, c := range cells {
			children = append(children, fmt.Sprint(c.LeftChild))
		}
		children = append(children, fmt.Sprint(p.RightChild))
		if len(children) > 8 {
			children = append(children[:3], "...", children[len(children)-1])
		}
		text += " children=[" + strings.Join(children, " ") + "]"
	}
	return text
}

func mustPage(f *dbfile.File, n int) *dbfile.BTreePage {
	p, err := f.BTreePage(n)
	if err != nil {
		fatal("page", err)
	}
	return p
}

func printLayout(s step) {
	for _, c := range s.changes {
		fmt.Printf("    page %d before: %s\n", c.Page, describePage(s.before, c.Page))
		fmt.Printf("    page %d after:  %s\n", c.Page, describePage(s.after, c.Page))
	}
}

func formatInts(xs []int) string {
	parts := make([]string, len(xs))
	for i, x := range xs {
		parts[i] = fmt.Sprint(x)
	}
	return "[" + strings.Join(parts, " ") + "]"
}

func printStep(label string, s step) {
	frames := "WAL frames for this commit: " + formatInts(s.walPages)
	if s.walPages == nil {
		frames = "no WAL (rollback mode)"
	}
	fmt.Printf("%s: insert #%d (rowid %d): %d page(s) changed %s; %s; depth %d; root page %d\n",
		label, s.number, s.rowid, len(s.changes), equal.FormatPageChanges(s.changes), frames, s.depth, s.rootPage)
}

type arithmetic struct {
	payload      int
	cellSmall    int
	cellLarge    int
	rowsPerLeaf  int
	dividersFit  int
	firstSplit   int
	deeperInsert int
}

func predict() arithmetic {
	usable := pageSize
	serialBlob := uint64(12 + 2*blobBytes)
	header := 1 + 1 + dbfile.VarintLength(serialBlob)
	headerVarint := dbfile.VarintLength(uint64(header))
	if headerVarint != 1 {
		fatal("arithmetic", errors.New("record header longer than 127 bytes"))
	}
	payload := header + blobBytes
	maxLocal := dbfile.MaxLocalPayload(usable, dbfile.TableLeaf)
	fmt.Println("arithmetic (from cell size and page size, before inserting anything):")
	fmt.Printf("  row = (id INTEGER PRIMARY KEY, v BLOB of %d bytes); the id is the rowid, so the record stores it as NULL\n", blobBytes)
	fmt.Printf("  record header = 1 (header-size varint) + 1 (serial type 0, NULL) + %d (serial type %d = blob of %d bytes) = %d bytes\n", dbfile.VarintLength(serialBlob), serialBlob, blobBytes, header)
	fmt.Printf("  payload = %d header + %d body = %d bytes; table-leaf max local payload at usable size %d is %d, so no overflow page\n", header, blobBytes, payload, usable, maxLocal)
	cell := func(rowid uint64) int {
		return dbfile.VarintLength(uint64(payload)) + dbfile.VarintLength(rowid) + payload
	}
	small, large := cell(127), cell(128)
	fmt.Printf("  cell = %d (payload-size varint) + rowid varint (1 byte for rowid <= 127, 2 bytes for 128..16383) + %d = %d or %d bytes, plus a 2-byte cell pointer = %d or %d\n", dbfile.VarintLength(uint64(payload)), payload, small, large, small+2, large+2)
	space := usable - 8
	perLeaf := space / (large + 2)
	if space/(small+2) != perLeaf {
		fatal("arithmetic", errors.New("rows per leaf depends on the rowid size"))
	}
	fmt.Printf("  a leaf page other than page 1 has %d - 8 (leaf header) = %d bytes for pointers and cells\n", usable, space)
	fmt.Printf("  rows per leaf = floor(%d / %d) = %d (%d x %d = %d fits; %d x %d = %d does not)\n", space, large+2, perLeaf, perLeaf, large+2, perLeaf*(large+2), perLeaf+1, small+2, (perLeaf+1)*(small+2))
	fmt.Printf("  prediction: inserts 1..%d fill the root leaf; insert #%d (rowid %d) does not fit, so it is the first split, and every %d inserts after it another leaf fills: splits at inserts #%d, #%d, #%d, ... (#%d m + 1)\n",
		perLeaf, perLeaf+1, rowidStep*(perLeaf+1), perLeaf, perLeaf+1, 2*perLeaf+1, 3*perLeaf+1, perLeaf)
	interiorSpace := usable - 12
	used, fit := 0, 0
	for leaf := 1; ; leaf++ {
		key := uint64(rowidStep * perLeaf * leaf)
		size := 4 + dbfile.VarintLength(key) + 2
		if used+size > interiorSpace {
			break
		}
		used += size
		fit++
	}
	small1 := 0
	for leaf := 1; leaf <= fit; leaf++ {
		if rowidStep*perLeaf*leaf <= 127 {
			small1++
		}
	}
	deeper := perLeaf*(fit+1) + 1
	fmt.Printf("  each split adds one divider cell to the root: 4-byte child page number + varint of the left leaf's largest rowid, plus a 2-byte pointer\n")
	fmt.Printf("  the root as an interior page has %d - 12 = %d bytes; %d dividers of 7 bytes (keys <= 127) + %d of 8 bytes = %d bytes fit, one more does not\n", usable, interiorSpace, small1, fit-small1, used)
	fmt.Printf("  prediction: divider %d arrives with the split at insert #%d (rowid %d); the root overflows and the tree gains a third level\n", fit+1, deeper, rowidStep*deeper)
	return arithmetic{payload: payload, cellSmall: small, cellLarge: large, rowsPerLeaf: perLeaf, dividersFit: fit, firstSplit: perLeaf + 1, deeperInsert: deeper}
}

func partA(ctx context.Context, dir string) {
	fmt.Println()
	fmt.Println("== A. appends: one row per transaction into an empty table, a page manifest after every insert (WAL, checkpoint before each manifest)")
	a := predict()
	l := openLab(ctx, filepath.Join(dir, "append.db"), "wal", "CREATE TABLE t(id INTEGER PRIMARY KEY, v BLOB)")
	defer l.close()
	root := l.rootOf("t")
	fmt.Printf("table t: root page %d (sqlite_schema.rootpage), page count %d\n", root, l.file.PageCount())
	total := a.deeperInsert + 2*a.rowsPerLeaf
	interesting := map[int]bool{a.firstSplit: true, a.firstSplit + a.rowsPerLeaf: true, a.firstSplit + 2*a.rowsPerLeaf: true, a.firstSplit + 2*a.rowsPerLeaf + 1: true}
	kept := map[int]step{}
	histogram := map[int]int{}
	var splits []int
	ordinaryOnePage, ordinaryLeafOnly := true, true
	walMatches, integrityOK, rootConstant := true, true, true
	var walMismatch []string
	firstDeeper := map[int]step{}
	prevDepth, frames, inserts := 1, 0, 0
	for n := 1; n <= total; n++ {
		rowid := int64(rowidStep * n)
		s := l.exec(ctx, n, rowid, "t", "INSERT INTO t(id, v) VALUES(?, ?)", rowid, rowBlob(rowid))
		inserts++
		frames += len(s.walPages)
		histogram[len(s.changes)]++
		if s.integrity != "ok" {
			integrityOK = false
		}
		if s.rootPage != root {
			rootConstant = false
		}
		if !sameInts(s.walPages, s.pages()) {
			walMatches = false
			if len(walMismatch) < 5 {
				walMismatch = append(walMismatch, fmt.Sprintf("insert #%d: WAL %s vs changed %s", s.number, formatInts(s.walPages), formatInts(s.pages())))
			}
		}
		if s.after.PageCount() > s.before.PageCount() {
			splits = append(splits, s.number)
		} else {
			if len(s.changes) != 1 {
				ordinaryOnePage = false
			} else if p, err := s.after.BTreePage(s.changes[0].Page); err != nil || p.Type != dbfile.TableLeaf {
				ordinaryLeafOnly = false
			}
		}
		if s.depth > prevDepth {
			if _, seen := firstDeeper[s.depth]; !seen {
				firstDeeper[s.depth] = s
			}
		}
		prevDepth = s.depth
		if interesting[n] {
			kept[n] = s
		}
	}
	fmt.Printf("%d inserts done; final page count %d, depth %d\n", total, l.file.PageCount(), prevDepth)
	check(l.verifyManifest(), "the last manifest equals capture.BuildManifest of the same image (unchanged pages reuse the previous sha256 only to save time)")

	var counts []int
	for k := range histogram {
		counts = append(counts, k)
	}
	sort.Ints(counts)
	fmt.Println("changed pages per insert -> number of inserts:")
	for _, k := range counts {
		fmt.Printf("  %d page(s): %d insert(s)\n", k, histogram[k])
	}
	predictedSplits := true
	for i, n := range splits {
		if n != a.firstSplit+i*a.rowsPerLeaf {
			predictedSplits = false
		}
	}
	fmt.Printf("splits (inserts that added pages): %d, at inserts #%d, #%d, #%d, ..., #%d\n", len(splits), splits[0], splits[1], splits[2], splits[len(splits)-1])
	check(len(splits) > 0 && splits[0] == a.firstSplit, fmt.Sprintf("the first split happens at insert #%d (rowid %d), as the arithmetic predicts", a.firstSplit, rowidStep*a.firstSplit))
	check(predictedSplits, fmt.Sprintf("every split happens exactly where predicted: insert #%d m + 1, every %d inserts", a.rowsPerLeaf, a.rowsPerLeaf))
	check(ordinaryOnePage, "every insert that does not split changes exactly one page")
	check(ordinaryLeafOnly, "that one page is the table leaf the row went into (page 1 is not written)")
	check(histogram[1] > inserts/2, fmt.Sprintf("most inserts change one page (%d of %d)", histogram[1], inserts))

	fmt.Println()
	printStep("first split, depth 1 -> 2", firstDeeper[2])
	printLayout(firstDeeper[2])
	second := kept[a.firstSplit+a.rowsPerLeaf]
	printStep("first append split at depth 2", second)
	printLayout(second)
	third := kept[a.firstSplit+2*a.rowsPerLeaf]
	printStep("next append split", third)
	ordinary := kept[a.firstSplit+2*a.rowsPerLeaf+1]
	printStep("an ordinary insert", ordinary)
	printLayout(ordinary)
	deeper, ok := firstDeeper[3]
	check(ok && deeper.number == a.deeperInsert, fmt.Sprintf("the tree gains its third level at insert #%d (rowid %d), as the arithmetic predicts", a.deeperInsert, rowidStep*a.deeperInsert))
	if ok {
		printStep("depth 2 -> 3", deeper)
		printLayout(deeper)
	}
	check(firstDeeper[2].number == a.firstSplit && len(firstDeeper[2].changes) == 4, "the first split is also the first depth change: the root, a new page holding the root's old cells, a new leaf, and page 1")
	check(len(second.changes) == 3, "an append split at depth 2 changes three pages: page 1 (database size), the parent (one new divider) and the new leaf; the full leaf is not rewritten")
	check(rootConstant, fmt.Sprintf("the root stays page %d through both depth changes and always equals sqlite_schema.rootpage", root))
	check(integrityOK, fmt.Sprintf("integrity_check = ok after every one of the %d inserts", inserts))
	for _, line := range walMismatch {
		fmt.Printf("  %s\n", line)
	}
	check(walMatches, "for every insert the WAL frames of its commit name exactly the pages the manifest shows changed: pages changed = frames shipped")
	fmt.Printf("WAL frames for %d inserts: %d (%d bytes of frames at %d bytes each); the table itself is %d pages\n", inserts, frames, frames*(walFrameHead+pageSize), walFrameHead+pageSize, l.file.PageCount())
}

func partB(ctx context.Context, dir string) {
	fmt.Println()
	fmt.Println("== B. a middle insert into a full leaf")
	l := openLab(ctx, filepath.Join(dir, "middle.db"), "wal", "CREATE TABLE t(id INTEGER PRIMARY KEY, v BLOB)")
	defer l.close()
	var last step
	for n := 1; n <= 20; n++ {
		rowid := int64(rowidStep * n)
		last = l.exec(ctx, n, rowid, "t", "INSERT INTO t(id, v) VALUES(?, ?)", rowid, rowBlob(rowid))
	}
	fmt.Printf("20 appends (rowids 2..40): page count %d, depth %d\n", l.file.PageCount(), last.depth)
	root := mustPage(l.file, l.rootOf("t"))
	fmt.Printf("  root: %s\n", describePage(l.file, root.Number))
	cells, _ := root.Cells()
	for _, c := range cells {
		fmt.Printf("  leaf page %d: %s\n", c.LeftChild, describePage(l.file, int(c.LeftChild)))
	}
	fmt.Printf("  leaf page %d: %s\n", root.RightChild, describePage(l.file, int(root.RightChild)))
	rowid := int64(11)
	s := l.exec(ctx, 21, rowid, "t", "INSERT INTO t(id, v) VALUES(?, ?)", rowid, rowBlob(rowid))
	printStep("middle split", s)
	printLayout(s)
	leaves := 0
	for _, c := range s.changes {
		if c.Page == 1 {
			continue
		}
		if p, err := s.after.BTreePage(c.Page); err == nil && p.Type == dbfile.TableLeaf {
			leaves++
		}
	}
	check(leaves >= 2, fmt.Sprintf("a split in the middle rewrites the full leaf and its siblings (%d leaf pages changed), not just one new page", leaves))
	extra := subtract(s.walPages, s.pages())
	check(len(subtract(s.pages(), s.walPages)) == 0, "every changed page is in the commit's WAL frames")
	fmt.Printf("WAL frames whose page bytes did not change: %s\n", formatInts(extra))
	for _, n := range extra {
		before, _ := s.before.Page(n)
		after, _ := s.after.Page(n)
		fmt.Printf("  page %d: sha256 before %s, after %s\n", n, equal.SHA256Hex(before)[:16], equal.SHA256Hex(after)[:16])
	}
	check(len(extra) > 0, "here the WAL ships more frames than pages changed: balance_nonroot rewrote a sibling leaf with the same cells it already held, and a written page is a shipped frame even when its bytes are unchanged")
	check(s.integrity == "ok", "integrity_check = ok after the middle split")
	var unused []string
	walker := dbfile.TreeWalker{File: s.after, OnPage: func(p *dbfile.BTreePage, parent, depth int) error {
		if p.Type.IsLeaf() {
			unused = append(unused, fmt.Sprintf("%d:%d", p.Number, p.CellCount))
		}
		return nil
	}}
	if err := walker.Walk(l.rootOf("t")); err != nil {
		fatal("walk", err)
	}
	fmt.Printf("leaves after the middle split (page:cells, in key order): %s\n", strings.Join(unused, " "))
}

func partC(ctx context.Context, dir string) {
	fmt.Println()
	fmt.Println("== C. the same kind of insert into a table with two indexes")
	l := openLab(ctx, filepath.Join(dir, "indexed.db"), "wal",
		"CREATE TABLE t(id INTEGER PRIMARY KEY, email TEXT, score INTEGER, v BLOB)",
		"CREATE INDEX t_email ON t(email)",
		"CREATE INDEX t_score ON t(score)")
	defer l.close()
	histogram := map[int]int{}
	var roleCounts []string
	const inserts = 200
	ok := true
	for n := 1; n <= inserts; n++ {
		rowid := int64(n)
		email := fmt.Sprintf("user%03d@example.com", (n*37)%inserts)
		score := (n * 53) % 97
		s := l.exec(ctx, n, rowid, "t", "INSERT INTO t(id, email, score, v) VALUES(?, ?, ?, ?)", rowid, email, score, rowBlob(rowid)[:200])
		histogram[len(s.changes)]++
		if !sameInts(s.walPages, s.pages()) || s.integrity != "ok" {
			ok = false
		}
		if n == 50 {
			for _, c := range s.changes {
				roleCounts = append(roleCounts, c.String())
			}
		}
	}
	var counts []int
	for k := range histogram {
		counts = append(counts, k)
	}
	sort.Ints(counts)
	fmt.Println("changed pages per insert -> number of inserts:")
	most, mostCount := 0, 0
	for _, k := range counts {
		fmt.Printf("  %d page(s): %d insert(s)\n", k, histogram[k])
		if histogram[k] > mostCount {
			most, mostCount = k, histogram[k]
		}
	}
	fmt.Printf("insert #50 changed: %s\n", strings.Join(roleCounts, " "))
	check(most == 3, "with two indexes the common insert changes three pages: the table leaf and one leaf in each index")
	check(ok, "WAL frames equal the changed pages and integrity_check = ok for all 200 inserts")
}

func partD(ctx context.Context, dir string) {
	fmt.Println()
	fmt.Println("== D. the same appends in rollback mode (journal_mode=DELETE)")
	l := openLab(ctx, filepath.Join(dir, "rollback.db"), "delete", "CREATE TABLE t(id INTEGER PRIMARY KEY, v BLOB)")
	defer l.close()
	histogram := map[string]int{}
	ok := true
	for n := 1; n <= 12; n++ {
		rowid := int64(rowidStep * n)
		s := l.exec(ctx, n, rowid, "t", "INSERT INTO t(id, v) VALUES(?, ?)", rowid, rowBlob(rowid))
		hasPage1 := len(s.changes) > 0 && s.changes[0].Page == 1
		key := fmt.Sprintf("%d page(s), page 1 among them: %v", len(s.changes), hasPage1)
		histogram[key]++
		if !hasPage1 || s.integrity != "ok" {
			ok = false
		}
		if n <= 2 {
			printStep("rollback", s)
		}
	}
	var keys []string
	for k := range histogram {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("  %s: %d insert(s)\n", k, histogram[k])
	}
	check(ok, "in rollback mode every commit also rewrites page 1 (change counter, bytes 24-27 and 92-95), so an ordinary insert changes two pages")
}

func main() {
	ctx := context.Background()
	sqlitex.MustPinCheckMemory(ctx)
	dir := sqlitex.MustWorkDir("3.1")
	fmt.Printf("INFO: work dir %s\n", displayPath(dir))
	fmt.Printf("settings: page_size=%d, journal_mode=wal (part D: delete), synchronous=OFF (no effect on file bytes; it only skips fsync), wal_autocheckpoint=0, one connection, a PRAGMA wal_checkpoint(TRUNCATE) after every insert\n", pageSize)
	partA(ctx, dir)
	partB(ctx, dir)
	partC(ctx, dir)
	partD(ctx, dir)
	fmt.Println()
	if len(failures) > 0 {
		fmt.Printf("PAGES PER INSERT FAILED: %d claim(s) did not hold\n", len(failures))
		for _, f := range failures {
			fmt.Printf("  - %s\n", f)
		}
		os.Exit(1)
	}
	fmt.Println("PAGES PER INSERT OK")
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
