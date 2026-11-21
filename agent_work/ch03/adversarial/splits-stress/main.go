// Group 1 (splits-stress): Adversarial attacks on Chapter 3 Experiment 3.1
// (pages-per-insert), B-tree split arithmetic, WAL-frames vs changed-pages,
// index multiplication, WITHOUT ROWID appends, UPDATE overwrites, and
// equal.ChangedPages behavior on DELETE.
package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nickstrad/sqlite-internals/internal/dbfile"
	"github.com/nickstrad/sqlite-internals/internal/equal"
	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
)

const (
	pageSize      = 4096
	walHeaderSize = 32
	walFrameHead  = 24
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

func makeBlob(tag string, n int) []byte {
	unit := []byte(tag + " ")
	return bytes.Repeat(unit, n/len(unit)+1)[:n]
}

// tracker drives a single WAL connection with synchronous=OFF and
// wal_autocheckpoint=0, capturing WAL frames and a sqlite_dbpage manifest after
// every committed statement without ever opening the .db file with os.Open.
type tracker struct {
	path     string
	db       *sqlitex.DB
	conn     *sql.Conn
	file     *dbfile.File
	census   *dbfile.Census
	manifest equal.Manifest
	image    []byte
}

type execResult struct {
	changes     []equal.PageChange
	byteChanged []int
	walPages    []int
	before      *dbfile.File
	after       *dbfile.File
	beforePages int
	afterPages  int
	integrity   string
}

func openTracker(ctx context.Context, path string, setup ...string) *tracker {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		fatal("mkdir", err)
	}
	for _, suf := range []string{"", "-wal", "-shm", "-journal"} {
		_ = os.Remove(path + suf)
	}
	opts := sqlitex.DefaultOptions()
	opts.SingleConnection = true
	opts.File.JournalMode = sqlitex.JournalWAL
	opts.Conn.Synchronous = sqlitex.SyncOff
	opts.Conn.WALAutocheckpoint = 0
	db, err := sqlitex.Open(ctx, path, opts)
	if err != nil {
		fatal("open "+path, err)
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		fatal("conn", err)
	}
	t := &tracker{path: path, db: db, conn: conn}
	for _, s := range setup {
		if _, err := conn.ExecContext(ctx, s); err != nil {
			fatal(s, err)
		}
	}
	t.file, t.manifest = t.snapshot(ctx)
	return t
}

func (t *tracker) close() {
	if err := t.conn.Close(); err != nil {
		fatal("close conn", err)
	}
	if err := t.db.Close(); err != nil {
		fatal("close db", err)
	}
}

func (t *tracker) snapshot(ctx context.Context) (*dbfile.File, equal.Manifest) {
	res, err := sqlitex.Checkpoint(ctx, t.conn, "main", sqlitex.CheckpointTruncate)
	if err != nil || res.Busy {
		fatal("checkpoint", fmt.Errorf("%v %+v", err, res))
	}
	var count int
	if err := t.conn.QueryRowContext(ctx, "PRAGMA page_count").Scan(&count); err != nil {
		fatal("page_count", err)
	}
	image := make([]byte, 0, count*pageSize)
	rows, err := t.conn.QueryContext(ctx, "SELECT pgno, data FROM sqlite_dbpage ORDER BY pgno")
	if err != nil {
		fatal("sqlite_dbpage", err)
	}
	for rows.Next() {
		var pgno int64
		var data sql.RawBytes
		if err := rows.Scan(&pgno, &data); err != nil {
			fatal("scan dbpage", err)
		}
		image = append(image, data...)
	}
	if err := rows.Close(); err != nil {
		fatal("close dbpage", err)
	}
	f, err := dbfile.FromBytes(image)
	if err != nil {
		fatal("FromBytes", err)
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
		var sum string
		if t.image != nil && n*pageSize <= len(t.image) && bytes.Equal(page, t.image[(n-1)*pageSize:n*pageSize]) {
			sum = t.manifest.Entries[n-1].SHA256
		} else {
			sum = equal.SHA256Hex(page)
		}
		m.Entries[n-1] = equal.ManifestEntry{
			Page:   n,
			Role:   census.Page(n).Role.String(),
			SHA256: sum,
		}
	}
	t.image = image
	t.census = census
	return f, m
}

func readWALPages(dbPath string) ([]int, error) {
	data, err := os.ReadFile(dbPath + "-wal")
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, nil
	}
	if len(data) < walHeaderSize {
		return nil, fmt.Errorf("-wal too short: %d bytes", len(data))
	}
	magic := binary.BigEndian.Uint32(data[0:4])
	if magic != 0x377f0682 && magic != 0x377f0683 {
		return nil, fmt.Errorf("unexpected -wal magic %#x", magic)
	}
	frameSize := walFrameHead + pageSize
	if (len(data)-walHeaderSize)%frameSize != 0 {
		return nil, fmt.Errorf("-wal size %d not header + whole frames", len(data))
	}
	seen := map[int]bool{}
	var pages []int
	for off := walHeaderSize; off < len(data); off += frameSize {
		pg := int(binary.BigEndian.Uint32(data[off : off+4]))
		if !seen[pg] {
			seen[pg] = true
			pages = append(pages, pg)
		}
	}
	sort.Ints(pages)
	return pages, nil
}

func (t *tracker) exec(ctx context.Context, checkIntegrity bool, stmt string, args ...any) execResult {
	beforeFile := t.file
	beforeManifest := t.manifest
	beforePages := beforeFile.PageCount()
	if _, err := t.conn.ExecContext(ctx, stmt, args...); err != nil {
		fatal(stmt, err)
	}
	walPages, err := readWALPages(t.path)
	if err != nil {
		fatal("readWALPages", err)
	}
	afterFile, afterManifest := t.snapshot(ctx)
	changes := equal.ChangedPages(beforeManifest, afterManifest)

	// Also compute strictly byte-changed pages (ignoring role-only changes).
	beforeHash := map[int]string{}
	for _, e := range beforeManifest.Entries {
		beforeHash[e.Page] = e.SHA256
	}
	var byteChanged []int
	for _, e := range afterManifest.Entries {
		if prev, ok := beforeHash[e.Page]; !ok || prev != e.SHA256 {
			byteChanged = append(byteChanged, e.Page)
		}
	}
	sort.Ints(byteChanged)

	t.file, t.manifest = afterFile, afterManifest
	integrity := "skipped"
	if checkIntegrity {
		if err := t.conn.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil {
			fatal("integrity_check", err)
		}
	}
	return execResult{
		changes:     changes,
		byteChanged: byteChanged,
		walPages:    walPages,
		before:      beforeFile,
		after:       afterFile,
		beforePages: beforePages,
		afterPages:  afterFile.PageCount(),
		integrity:   integrity,
	}
}

func (t *tracker) rootOf(name string) int {
	schema, err := t.file.Schema()
	if err != nil {
		fatal("schema", err)
	}
	for _, e := range schema {
		if e.Name == name {
			return int(e.RootPage)
		}
	}
	fatal("rootOf", fmt.Errorf("object %q not found", name))
	return 0
}

func (t *tracker) depthOf(root int) int {
	deepest := 0
	for n := 1; n <= t.census.PageCount; n++ {
		info := t.census.Page(n)
		if info.Role != dbfile.RoleTableInterior && info.Role != dbfile.RoleTableLeaf &&
			info.Role != dbfile.RoleIndexInterior && info.Role != dbfile.RoleIndexLeaf {
			continue
		}
		d, pg := 1, n
		for pg != root && pg != 0 {
			pg = t.census.Page(pg).Parent
			d++
		}
		if pg == root && d > deepest {
			deepest = d
		}
	}
	return deepest
}

func isSubset(sub, super []int) bool {
	in := map[int]bool{}
	for _, x := range super {
		in[x] = true
	}
	for _, x := range sub {
		if !in[x] {
			return false
		}
	}
	return true
}

func subtractInts(a, b []int) []int {
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

func predictRowLeafSize(blobLen int) (smallCell, largeCell int) {
	serialBlob := uint64(12 + 2*blobLen)
	hdr := 1 + 1 + dbfile.VarintLength(serialBlob)
	payload := hdr + blobLen
	small := dbfile.VarintLength(uint64(payload)) + dbfile.VarintLength(1) + payload + 2
	large := dbfile.VarintLength(uint64(payload)) + dbfile.VarintLength(128) + payload + 2
	return small, large
}

func main() {
	ctx := context.Background()
	root, err := sqlitex.RepoRoot()
	if err != nil {
		fatal("RepoRoot", err)
	}
	scratch := filepath.Join(root, "_work", "adv-ch03", "splits-stress")
	if err := os.RemoveAll(scratch); err != nil {
		fatal("clean scratch", err)
	}
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		fatal("mkdir scratch", err)
	}

	// SPL-01: Random insert orders under 3 fixed seeds (OUTLINE 3.1 bullet 1 +
	// repair-round correction 7 / 3.1 part E).
	// Verifies the corrected rule: root constancy, integrity_check=ok, and
	// changed<=WAL hold across all random workloads; for 1000B rows (4/leaf),
	// <50% of inserts change 1 page (frequent splits and sibling rebalances),
	// while for small 40B rows (~85/leaf), >50% of inserts change 1 page again.
	{
		seeds := []uint64{11, 42, 99}
		const nRows = 250
		allRootConst := true
		allIntegrityOK := true
		allChangedInWAL := true
		allLargeUnderHalf := true
		var seedSummaries []string
		for _, seed := range seeds {
			dbPath := filepath.Join(scratch, fmt.Sprintf("spl01-seed-%d.db", seed))
			tr := openTracker(ctx, dbPath, "CREATE TABLE t(id INTEGER PRIMARY KEY, v BLOB)")
			rootPage := tr.rootOf("t")
			ids := make([]int64, nRows)
			for i := range ids {
				ids[i] = int64(i + 1)
			}
			rng := rand.New(rand.NewPCG(seed, 0))
			rng.Shuffle(len(ids), func(i, j int) { ids[i], ids[j] = ids[j], ids[i] })

			onePage, splits, walExtra, maxDepth := 0, 0, 0, 1
			for _, id := range ids {
				res := tr.exec(ctx, true, "INSERT INTO t(id, v) VALUES(?, ?)", id, makeBlob(fmt.Sprintf("r%04d", id), 1000))
				if len(res.changes) == 1 {
					onePage++
				}
				if res.afterPages > res.beforePages {
					splits++
				}
				if !isSubset(equal.PageNumbers(res.changes), res.walPages) {
					allChangedInWAL = false
				}
				if len(subtractInts(res.walPages, res.byteChanged)) > 0 {
					walExtra++
				}
				if tr.rootOf("t") != rootPage {
					allRootConst = false
				}
				if res.integrity != "ok" {
					allIntegrityOK = false
				}
				if d := tr.depthOf(rootPage); d > maxDepth {
					maxDepth = d
				}
			}
			if onePage*2 >= nRows {
				allLargeUnderHalf = false
			}
			seedSummaries = append(seedSummaries,
				fmt.Sprintf("seed=%d:1pg=%d/%d,splits=%d,wal>changed=%d,depth=%d,pages=%d",
					seed, onePage, nRows, splits, walExtra, maxDepth, tr.file.PageCount()))
			tr.close()
		}
		// Companion small-row random check (40B rows, ~85/leaf): >50% change 1 page.
		smallOnePage := 0
		{
			const nSmall = 250
			dbPath := filepath.Join(scratch, "spl01-small-seed-11.db")
			tr := openTracker(ctx, dbPath, "CREATE TABLE t(id INTEGER PRIMARY KEY, v BLOB)")
			rootPage := tr.rootOf("t")
			ids := make([]int64, nSmall)
			for i := range ids {
				ids[i] = int64(i + 1)
			}
			rng := rand.New(rand.NewPCG(11, 0))
			rng.Shuffle(len(ids), func(i, j int) { ids[i], ids[j] = ids[j], ids[i] })
			for _, id := range ids {
				res := tr.exec(ctx, true, "INSERT INTO t(id, v) VALUES(?, ?)", id, makeBlob(fmt.Sprintf("s%04d", id), 40))
				if len(res.changes) == 1 {
					smallOnePage++
				}
				if !isSubset(equal.PageNumbers(res.changes), res.walPages) {
					allChangedInWAL = false
				}
				if tr.rootOf("t") != rootPage {
					allRootConst = false
				}
				if res.integrity != "ok" {
					allIntegrityOK = false
				}
			}
			seedSummaries = append(seedSummaries, fmt.Sprintf("small40B-seed=11:1pg=%d/%d", smallOnePage, nSmall))
			tr.close()
		}
		held := allRootConst && allIntegrityOK && allChangedInWAL && allLargeUnderHalf && smallOnePage*2 > 250
		report("SPL-01",
			"random insert orders (3 fixed seeds x 250 rows of 1000B blobs + 250 rows of 40B blobs)",
			"root constant, integrity ok, changed<=WAL; <50% 1-page for 1000B (4/leaf) across all seeds and >50% 1-page for 40B (~85/leaf)",
			fmt.Sprintf("rootConst=%v integrity=%v changedInWAL=%v [%s]",
				allRootConst, allIntegrityOK, allChangedInWAL, strings.Join(seedSummaries, "; ")),
			held)
	}

	// SPL-02: Other non-overflowing row sizes + exact split-row arithmetic,
	// including the rowid-varint boundary (blob=1013 bytes) where 4 rows fit
	// when rowid<=127 (0 bytes unused!) and only 3 rows fit once rowid>=128.
	{
		sizes := []struct {
			name     string
			blobLen  int
			inserts  int
			rowidMul int64
		}{
			{"small-50B", 50, 140, 1},
			{"medium-400B", 400, 40, 1},
			{"pair-1900B", 1900, 15, 1},
			// Boundary case: payload = 4 + 1013 = 1017; cell(rowid<=127) = 2+1+1017+2 = 1022;
			// 4 * 1022 = 4088 (exact leaf space!). For rowid>=128: cell = 1023; 4 * 1023 = 4092 > 4088 -> 3/leaf.
			{"varint-boundary-1013B", 1013, 30, 10},
		}
		var details []string
		allMatched := true
		for _, tc := range sizes {
			dbPath := filepath.Join(scratch, "spl02-"+tc.name+".db")
			tr := openTracker(ctx, dbPath, "CREATE TABLE t(id INTEGER PRIMARY KEY, v BLOB)")
			small, large := predictRowLeafSize(tc.blobLen)
			// Simulate exact append-only leaf fill accounting for each rowid's varint width.
			const leafSpace = pageSize - 8
			used := 0
			var predSplits, gotSplits []int
			for i := 1; i <= tc.inserts; i++ {
				rowid := int64(i) * tc.rowidMul
				cellCost := small
				if rowid >= 128 {
					cellCost = large
				}
				if used+cellCost > leafSpace {
					predSplits = append(predSplits, i)
					used = cellCost
				} else {
					used += cellCost
				}
				res := tr.exec(ctx, false, "INSERT INTO t(id, v) VALUES(?, ?)", rowid, makeBlob("x", tc.blobLen))
				if res.afterPages > res.beforePages {
					gotSplits = append(gotSplits, i)
				}
			}
			var integrity string
			if err := tr.conn.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil {
				fatal("integrity", err)
			}
			tr.close()
			match := fmt.Sprint(predSplits) == fmt.Sprint(gotSplits) && integrity == "ok"
			if !match {
				allMatched = false
			}
			details = append(details, fmt.Sprintf("%s:pred=%v/got=%v", tc.name, predSplits, gotSplits))
		}
		report("SPL-02",
			"non-overflowing row sizes (50B, 400B, 1900B, and 1013B rowid-varint boundary where capacity drops 4->3 at rowid 128)",
			"exact predicted split insert numbers match observed splits for every size including cadence shift at rowid 128",
			strings.Join(details, "; "),
			allMatched)
	}

	// SPL-03: Overflowing row sizes (5000B = 1 overflow page, 10000B = 2 overflow pages).
	// Verifies the corrected claim (3.1 part E / correction 7): overflowing rows never
	// change 1 page (0/25); each writes leaf + overflow page(s) + page 1 (>=3 / >=4 pages).
	{
		cases := []struct {
			name    string
			blobLen int
			minExp  int
		}{
			{"ovf1-5000B", 5000, 3},
			{"ovf2-10000B", 10000, 4},
		}
		const nInserts = 25
		var parts []string
		allOK := true
		for _, tc := range cases {
			dbPath := filepath.Join(scratch, "spl03-"+tc.name+".db")
			tr := openTracker(ctx, dbPath, "CREATE TABLE t(id INTEGER PRIMARY KEY, v BLOB)")
			onePage, minChg, maxChg := 0, 999, 0
			walOK, intOK := true, true
			for i := 1; i <= nInserts; i++ {
				res := tr.exec(ctx, true, "INSERT INTO t(id, v) VALUES(?, ?)", int64(i), makeBlob("ovf", tc.blobLen))
				c := len(res.changes)
				if c == 1 {
					onePage++
				}
				if c < minChg {
					minChg = c
				}
				if c > maxChg {
					maxChg = c
				}
				if !isSubset(equal.PageNumbers(res.changes), res.walPages) {
					walOK = false
				}
				if res.integrity != "ok" {
					intOK = false
				}
			}
			tr.close()
			if onePage != 0 || minChg < tc.minExp || !walOK || !intOK {
				allOK = false
			}
			parts = append(parts, fmt.Sprintf("%s:1pg=%d/%d,min=%d,max=%d,walOK=%v,intOK=%v", tc.name, onePage, nInserts, minChg, maxChg, walOK, intOK))
		}
		report("SPL-03",
			"append 25 overflowing rows (5000B and 10000B > maxLocal 4061B) into a WAL table",
			"0/25 one-page inserts; every insert writes leaf + overflow(s) + page 1 (>=3 for 5000B, >=4 for 10000B), changed<=WAL, integrity ok",
			strings.Join(parts, "; "),
			allOK)
	}

	// SPL-04: Secondary index multiplication (0, 1, 2, 4 indexes) + index root invariance
	// + checking if index splits also emit unchanged-byte WAL frames.
	{
		idxCounts := []int{0, 1, 2, 4}
		const nRows = 180
		allModeOK := true
		allRootsConst := true
		allChangedInWAL := true
		allIntegrityOK := true
		var summaries []string
		for _, k := range idxCounts {
			dbPath := filepath.Join(scratch, fmt.Sprintf("spl04-idx-%d.db", k))
			setup := []string{
				"CREATE TABLE t(id INTEGER PRIMARY KEY, c1 TEXT, c2 TEXT, c3 TEXT, c4 TEXT, v BLOB)",
			}
			idxNames := []string{}
			for j := 1; j <= k; j++ {
				name := fmt.Sprintf("idx_c%d", j)
				idxNames = append(idxNames, name)
				setup = append(setup, fmt.Sprintf("CREATE INDEX %s ON t(c%d)", name, j))
			}
			tr := openTracker(ctx, dbPath, setup...)
			roots := map[string]int{"t": tr.rootOf("t")}
			for _, name := range idxNames {
				roots[name] = tr.rootOf(name)
			}
			hist := map[int]int{}
			maxChg, walExtra, maxIdxDepth := 0, 0, 1
			for n := 1; n <= nRows; n++ {
				c1 := fmt.Sprintf("a-%03d-%s", (n*37)%nRows, strings.Repeat("p", 40))
				c2 := fmt.Sprintf("b-%03d-%s", (n*53)%nRows, strings.Repeat("q", 40))
				c3 := fmt.Sprintf("c-%03d-%s", (n*71)%nRows, strings.Repeat("r", 40))
				c4 := fmt.Sprintf("d-%03d-%s", (n*89)%nRows, strings.Repeat("s", 40))
				res := tr.exec(ctx, false,
					"INSERT INTO t(id, c1, c2, c3, c4, v) VALUES(?, ?, ?, ?, ?, ?)",
					int64(n), c1, c2, c3, c4, makeBlob("i", 180))
				cnt := len(res.changes)
				hist[cnt]++
				if cnt > maxChg {
					maxChg = cnt
				}
				if !isSubset(equal.PageNumbers(res.changes), res.walPages) {
					allChangedInWAL = false
				}
				if len(subtractInts(res.walPages, res.byteChanged)) > 0 {
					walExtra++
				}
				for obj, r := range roots {
					if tr.rootOf(obj) != r {
						allRootsConst = false
					}
					if obj != "t" {
						if d := tr.depthOf(r); d > maxIdxDepth {
							maxIdxDepth = d
						}
					}
				}
			}
			var integrity string
			if err := tr.conn.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil {
				fatal("integrity", err)
			}
			if integrity != "ok" {
				allIntegrityOK = false
			}
			tr.close()
			mode, modeCnt := 0, 0
			for pg, cnt := range hist {
				if cnt > modeCnt {
					mode, modeCnt = pg, cnt
				}
			}
			if mode != 1+k {
				allModeOK = false
			}
			summaries = append(summaries,
				fmt.Sprintf("k=%d:mode=%d(%d/%d),max=%d,wal>chg=%d,idxDepth=%d",
					k, mode, modeCnt, nRows, maxChg, walExtra, maxIdxDepth))
		}
		held := allModeOK && allRootsConst && allChangedInWAL && allIntegrityOK
		report("SPL-04",
			"insert 180 rows with scrambled keys into tables with 0, 1, 2, and 4 secondary indexes",
			"mode changed pages = 1+k, all table & index roots constant across depth changes, changed<=WAL, integrity ok",
			fmt.Sprintf("rootsConst=%v integrity=%v changedInWAL=%v [%s]",
				allRootsConst, allIntegrityOK, allChangedInWAL, strings.Join(summaries, "; ")),
			held)
	}

	// SPL-05: WITHOUT ROWID sequential appends do NOT get balance_quick (btree.c:9185 checks intKeyLeaf).
	// Verifies the corrected claim (3.1 part E / correction 7): WITHOUT ROWID is an index
	// B-tree, so root stays constant, changed<=WAL, integrity ok, and every depth-2 append
	// split goes through balance_nonroot and changes >3 pages.
	{
		dbPath := filepath.Join(scratch, "spl05-without-rowid.db")
		tr := openTracker(ctx, dbPath, "CREATE TABLE t(k TEXT PRIMARY KEY, v BLOB) WITHOUT ROWID")
		rootPage := tr.rootOf("t")
		rootConst := true
		walOK := true
		var depth2Splits []int
		for i := 1; i <= 30; i++ {
			k := fmt.Sprintf("key-%06d", i)
			depthBefore := tr.depthOf(rootPage)
			res := tr.exec(ctx, true, "INSERT INTO t(k, v) VALUES(?, ?)", k, makeBlob(k, 400))
			if tr.rootOf("t") != rootPage {
				rootConst = false
			}
			if !isSubset(equal.PageNumbers(res.changes), res.walPages) {
				walOK = false
			}
			if res.integrity != "ok" {
				fatal("spl05 integrity", fmt.Errorf("%s", res.integrity))
			}
			if depthBefore >= 2 && res.afterPages > res.beforePages {
				depth2Splits = append(depth2Splits, len(res.changes))
			}
		}
		tr.close()
		allWide := len(depth2Splits) > 0
		for _, c := range depth2Splits {
			if c <= 3 {
				allWide = false
			}
		}
		report("SPL-05",
			"sequential ascending appends into a WITHOUT ROWID table (30 x 400B rows)",
			"root constant, changed<=WAL, integrity ok, and every depth-2 append split changes >3 pages (index B-tree uses balance_nonroot, not balance_quick)",
			fmt.Sprintf("rootConst=%v walOK=%v depth2_append_split_changed_pages=%v",
				rootConst, walOK, depth2Splits),
			rootConst && walOK && allWide)
	}

	// SPL-06: In-place UPDATE (btreeOverwriteCell) vs size-growing UPDATE on a full leaf.
	// Verifies build-notes claim on btree.c:9505/9557.
	{
		dbPath := filepath.Join(scratch, "spl06-update.db")
		tr := openTracker(ctx, dbPath, "CREATE TABLE t(id INTEGER PRIMARY KEY, v BLOB)")
		for i := 1; i <= 20; i++ {
			tr.exec(ctx, false, "INSERT INTO t(id, v) VALUES(?, ?)", int64(i*2), makeBlob(fmt.Sprintf("u%02d", i), 900))
		}
		// Same-size overwrite on a middle leaf (rowid 12): should touch exactly 1 page, 1 WAL frame, 0 splits.
		sameRes := tr.exec(ctx, true, "UPDATE t SET v = ? WHERE id = 12", makeBlob("SAME", 900))
		// Growing update on the same leaf (900B -> 1500B) pushes leaf over 4088B -> must split.
		growRes := tr.exec(ctx, true, "UPDATE t SET v = ? WHERE id = 12", makeBlob("GROW", 1500))
		tr.close()

		held := len(sameRes.changes) == 1 && len(sameRes.walPages) == 1 &&
			sameRes.afterPages == sameRes.beforePages && sameRes.integrity == "ok" &&
			len(growRes.changes) >= 3 && growRes.afterPages > growRes.beforePages && growRes.integrity == "ok"
		report("SPL-06",
			"in-place same-size UPDATE (900B->900B) vs growing UPDATE (900B->1500B) on a full leaf",
			"same-size UPDATE changes 1 page / 1 WAL frame (no split); growing UPDATE splits (>=3 pages)",
			fmt.Sprintf("same:changed=%d,wal=%d,pages=%d->%d; grow:changed=%d,wal=%d,pages=%d->%d",
				len(sameRes.changes), len(sameRes.walPages), sameRes.beforePages, sameRes.afterPages,
				len(growRes.changes), len(growRes.walPages), growRes.beforePages, growRes.afterPages),
			held)
	}

	// SPL-07a & SPL-07b: equal.ChangedPages vs WAL frames on DELETE with secure_delete=OFF.
	// Because freePage2 calls sqlite3PagerDontWrite, freed leaf pages keep identical bytes
	// (so they are NOT in the WAL frames) while their census role changes (table-leaf -> freelist-leaf),
	// so equal.ChangedPages reports pages that were NEVER written to the WAL.
	// We test both a leaf-aligned range (13..68) and a mid-leaf range (11..66, matching 3.2's shape).
	for _, tc := range []struct {
		id     string
		lo, hi int
		label  string
	}{
		{"SPL-07a", 13, 68, "leaf-aligned DELETE 13..68"},
		{"SPL-07b", 11, 66, "mid-leaf DELETE 11..66"},
	} {
		dbPath := filepath.Join(scratch, strings.ToLower(tc.id)+"-delete-wal-vs-changed.db")
		tr := openTracker(ctx, dbPath, "CREATE TABLE t(id INTEGER PRIMARY KEY, v BLOB)")
		if _, err := tr.conn.ExecContext(ctx, "PRAGMA secure_delete = OFF"); err != nil {
			fatal("secure_delete=OFF", err)
		}
		tx, err := tr.conn.BeginTx(ctx, nil)
		if err != nil {
			fatal("begin", err)
		}
		for i := 1; i <= 80; i++ {
			if _, err := tx.ExecContext(ctx, "INSERT INTO t(id, v) VALUES(?, ?)", int64(i), makeBlob("del", 1000)); err != nil {
				fatal("insert", err)
			}
		}
		if err := tx.Commit(); err != nil {
			fatal("commit", err)
		}
		tr.file, tr.manifest = tr.snapshot(ctx)

		res := tr.exec(ctx, true, fmt.Sprintf("DELETE FROM t WHERE id BETWEEN %d AND %d", tc.lo, tc.hi))
		changedPages := equal.PageNumbers(res.changes)
		notInWAL := subtractInts(changedPages, res.walPages)
		byteNotInWAL := subtractInts(res.byteChanged, res.walPages)
		tr.close()

		// Claim in 3.1: "every changed page is in the commit's WAL frames" (holds for inserts,
		// breaks for equal.ChangedPages on secure_delete=OFF DELETE when sqlite3PagerDontWrite skips freed leaves).
		held := len(notInWAL) == 0
		report(tc.id,
			fmt.Sprintf("%s (56 rows, freeing leaves) with secure_delete=OFF; compare equal.ChangedPages against commit WAL frames", tc.label),
			"every page reported by equal.ChangedPages is in the commit's WAL frames",
			fmt.Sprintf("ChangedPages=%d, byteChanged=%d, walFrames=%d, ChangedPages_not_in_WAL=%d, byteChanged_not_in_WAL=%d",
				len(changedPages), len(res.byteChanged), len(res.walPages), len(notInWAL), len(byteNotInWAL)),
			held)
	}
}
