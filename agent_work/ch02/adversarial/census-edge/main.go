package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickstrad/walfleet/internal/dbfile"
	"github.com/nickstrad/walfleet/internal/recipe"
	"github.com/nickstrad/walfleet/internal/sqlitex"
	_ "modernc.org/sqlite"
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

type statRow struct {
	Tree    string
	Type    string
	Cells   int
	Payload int
	Unused  int
	MaxPay  int
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

func openRaw(path string, readOnly bool) (*sql.DB, error) {
	q := url.Values{}
	if readOnly {
		q.Set("mode", "ro")
	}
	uri, err := sqlitex.FileURI(path, q)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", uri)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

func newDB(ctx context.Context, path string, edit func(*sqlitex.Options), stmts ...string) error {
	opts := sqlitex.DefaultOptions()
	opts.SingleConnection = true
	opts.Conn.SecureDelete = true
	if edit != nil {
		edit(&opts)
	}
	db, err := sqlitex.Open(ctx, path, opts)
	if err != nil {
		return err
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			db.Close()
			return fmt.Errorf("%s: %w", s, err)
		}
	}
	if err := db.Close(); err != nil {
		return err
	}
	return recipe.CheckAtRest(path)
}

func readDBStat(ctx context.Context, db *sql.DB) (map[int]statRow, error) {
	rows, err := db.QueryContext(ctx, "SELECT pageno, name, pagetype, ncell, payload, unused, mx_payload FROM dbstat")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int]statRow{}
	for rows.Next() {
		var n int
		var r statRow
		if err := rows.Scan(&n, &r.Tree, &r.Type, &r.Cells, &r.Payload, &r.Unused, &r.MaxPay); err != nil {
			return nil, err
		}
		out[n] = r
	}
	return out, rows.Err()
}

func queryInt(ctx context.Context, db *sql.DB, q string) (int, error) {
	var v int
	return v, db.QueryRowContext(ctx, q).Scan(&v)
}

func queryIntegrity(ctx context.Context, path string) (string, error) {
	db, err := openRaw(path, true)
	if err != nil {
		return "", err
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, "PRAGMA integrity_check")
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var l string
		if err := rows.Scan(&l); err != nil {
			return "", err
		}
		lines = append(lines, strings.Split(l, "\n")...)
	}
	if len(lines) > 2 {
		lines = append(lines[:2], fmt.Sprintf("(+%d more)", len(lines)-2))
	}
	return strings.Join(lines, " / "), rows.Err()
}

// verifyCensusAgainstSQLite parses path with internal/dbfile, checks completeness,
// and compares page_count, freelist_count, and every dbstat column against SQLite.
func verifyCensusAgainstSQLite(ctx context.Context, path string, readOnlyOpen bool) (*dbfile.Census, []string, error) {
	f, err := dbfile.Open(path)
	if err != nil {
		return nil, nil, err
	}
	c, err := f.Census()
	f.Close()
	if err != nil {
		return nil, nil, err
	}
	var diffs []string
	if err := c.Complete(); err != nil {
		diffs = append(diffs, "incomplete: "+err.Error())
	}
	db, err := openRaw(path, readOnlyOpen)
	if err != nil {
		return c, nil, err
	}
	defer db.Close()
	pc, err := queryInt(ctx, db, "PRAGMA page_count")
	if err != nil {
		return c, nil, err
	}
	fc, err := queryInt(ctx, db, "PRAGMA freelist_count")
	if err != nil {
		return c, nil, err
	}
	if pc != c.PageCount {
		diffs = append(diffs, fmt.Sprintf("page_count dbfile=%d sqlite=%d", c.PageCount, pc))
	}
	if fc != c.FreelistLength() {
		diffs = append(diffs, fmt.Sprintf("freelist dbfile=%d sqlite=%d", c.FreelistLength(), fc))
	}
	stat, err := readDBStat(ctx, db)
	if err != nil {
		return c, nil, err
	}
	compared := 0
	for _, p := range c.Pages {
		kind := dbstatType(p.Role)
		if kind == "" {
			if _, ok := stat[p.Number]; ok {
				diffs = append(diffs, fmt.Sprintf("page %d (%s) unexpectedly in dbstat", p.Number, p.Role))
			}
			continue
		}
		compared++
		want, ok := stat[p.Number]
		got := statRow{p.Tree, kind, p.CellCount, p.Payload, p.Unused, int(p.MaxPayload)}
		if !ok {
			diffs = append(diffs, fmt.Sprintf("page %d missing from dbstat", p.Number))
		} else if got != want {
			diffs = append(diffs, fmt.Sprintf("page %d: dbfile=%+v dbstat=%+v", p.Number, got, want))
		}
	}
	if compared != len(stat) {
		diffs = append(diffs, fmt.Sprintf("dbstat count=%d vs census btree+overflow=%d", len(stat), compared))
	}
	return c, diffs, nil
}

func hashTableOurs(f *dbfile.File, root int) (int, string, error) {
	h := sha256.New()
	rows := 0
	err := f.WalkTable(root, func(r dbfile.Row) error {
		rows++
		fmt.Fprintf(h, "row %d|", r.Rowid)
		for i, v := range r.Values {
			if i == 0 && v.Class == dbfile.Null {
				v = dbfile.Value{Class: dbfile.Integer, Int: r.Rowid}
			}
			switch v.Class {
			case dbfile.Null:
				fmt.Fprintf(h, "null|")
			case dbfile.Integer:
				fmt.Fprintf(h, "int:%d|", v.Int)
			case dbfile.Real:
				fmt.Fprintf(h, "real:%016x|", math.Float64bits(v.Float))
			case dbfile.Text:
				s, err := dbfile.DecodeText(v.Bytes, f.Header.TextEncoding)
				if err != nil {
					return err
				}
				fmt.Fprintf(h, "text:%x|", []byte(s))
			case dbfile.Blob:
				fmt.Fprintf(h, "blob:%x|", v.Bytes)
			}
		}
		h.Write([]byte("\n"))
		return nil
	})
	return rows, hex.EncodeToString(h.Sum(nil)), err
}

func hashTableSQLite(ctx context.Context, db *sql.DB, table string, cols []string) (int, string, error) {
	var sels []string
	for _, col := range cols {
		sels = append(sels, fmt.Sprintf("typeof(%s), %s", col, col))
	}
	q := fmt.Sprintf("SELECT rowid, %s FROM %s ORDER BY rowid", strings.Join(sels, ", "), table)
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return 0, "", err
	}
	defer rows.Close()
	h := sha256.New()
	count := 0
	for rows.Next() {
		var rowid int64
		vals := make([]any, 2*len(cols))
		dest := []any{&rowid}
		for i := range vals {
			dest = append(dest, &vals[i])
		}
		if err := rows.Scan(dest...); err != nil {
			return 0, "", err
		}
		count++
		fmt.Fprintf(h, "row %d|", rowid)
		for i := range cols {
			cls := vals[2*i].(string)
			v := vals[2*i+1]
			switch cls {
			case "null":
				fmt.Fprintf(h, "null|")
			case "integer":
				fmt.Fprintf(h, "int:%d|", v.(int64))
			case "real":
				fmt.Fprintf(h, "real:%016x|", math.Float64bits(v.(float64)))
			case "text":
				fmt.Fprintf(h, "text:%x|", []byte(v.(string)))
			case "blob":
				fmt.Fprintf(h, "blob:%x|", v.([]byte))
			}
		}
		h.Write([]byte("\n"))
	}
	return count, hex.EncodeToString(h.Sum(nil)), rows.Err()
}

func main() {
	ctx := context.Background()
	repoRoot, err := sqlitex.RepoRoot()
	if err != nil {
		fatal("repo root", err)
	}
	scratch := filepath.Join(repoRoot, "_work", "adv-ch02", "census-edge")
	if err := os.RemoveAll(scratch); err != nil {
		fatal("clean scratch", err)
	}
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		fatal("mkdir scratch", err)
	}
	cli, err := sqlitex.LocateCLI(ctx)
	if err != nil {
		fatal("locate cli", err)
	}

	// CEN-01: Multi-trunk freelist at page_size=512 + partial reuse + reader-max trunk boundary (usable/4 - 2 vs -1).
	{
		p := filepath.Join(scratch, "multi-trunk.db")
		err := newDB(ctx, p, func(o *sqlitex.Options) { o.File.PageSize = 512 },
			"CREATE TABLE big(id INTEGER PRIMARY KEY, v BLOB)",
			"WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i < 650) INSERT INTO big SELECT i, zeroblob(450) FROM n ORDER BY i",
			"DELETE FROM big",
			"WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i < 200) INSERT INTO big SELECT i, zeroblob(450) FROM n ORDER BY i",
		)
		if err != nil {
			fatal("build multi-trunk", err)
		}
		c, diffs, err := verifyCensusAgainstSQLite(ctx, p, false)
		if err != nil {
			fatal("verify multi-trunk", err)
		}

		// Now craft a valid rollback DB at page_size=512 whose trunk has EXACTLY maxLeaves = 512/4 - 2 = 126 leaves,
		// and a corrupt copy with 127 leaves (1 over reader max).
		maxLeaves := 512/4 - 2 // 126
		craftTrunk := func(name string, leafCount int) (string, error) {
			cp := filepath.Join(scratch, name)
			if err := newDB(ctx, cp, func(o *sqlitex.Options) {
				o.File.PageSize = 512
				o.File.JournalMode = sqlitex.JournalDelete
			}, "CREATE TABLE t(id INTEGER PRIMARY KEY)"); err != nil {
				return "", err
			}
			base, err := os.ReadFile(cp)
			if err != nil {
				return "", err
			}
			// Pages: 1=schema, 2=table t, 3=trunk, 4..(3+leafCount)=leaves.
			totalPages := 3 + leafCount
			img := make([]byte, totalPages*512)
			copy(img, base[:2*512])
			binary.BigEndian.PutUint32(img[28:32], uint32(totalPages))
			binary.BigEndian.PutUint32(img[32:36], 3)
			binary.BigEndian.PutUint32(img[36:40], uint32(1+leafCount))
			trunk := img[2*512 : 3*512]
			binary.BigEndian.PutUint32(trunk[4:8], uint32(leafCount))
			// Write leaf pointers up to what fits in page (for 126, 8+126*4=512 exact; for 127 we only write 126 entries).
			fit := (512 - 8) / 4
			for i := 0; i < min(leafCount, fit); i++ {
				binary.BigEndian.PutUint32(trunk[8+4*i:], uint32(4+i))
			}
			return cp, os.WriteFile(cp, img, 0o644)
		}

		pMax, err := craftTrunk("trunk-max.db", maxLeaves)
		if err != nil {
			fatal("craft max", err)
		}
		_, maxDiffs, maxErr := verifyCensusAgainstSQLite(ctx, pMax, true)
		maxIC, maxICErr := queryIntegrity(ctx, pMax)

		pOver, err := craftTrunk("trunk-over.db", maxLeaves+1)
		if err != nil {
			fatal("craft over", err)
		}
		fOver, err := dbfile.Open(pOver)
		if err != nil {
			fatal("open over", err)
		}
		_, overErr := fOver.Census()
		fOver.Close()
		overIC, _ := queryIntegrity(ctx, pOver)

		held := len(c.FreelistTrunks) >= 3 && len(diffs) == 0 &&
			maxErr == nil && len(maxDiffs) == 0 && maxICErr == nil && maxIC == "ok" &&
			overErr != nil && overIC != "ok"
		report("CEN-01",
			"512-byte multi-trunk freelist with partial reuse + synthetic trunk at reader max (126) and max+1 (127)",
			">=3 trunks match dbstat after reuse; 126-leaf trunk accepted by both; 127-leaf trunk rejected by both",
			fmt.Sprintf("trunks=%d diffs=%d, max126=(dbfile_err=%v diffs=%d sqlite=%q), over127=(dbfile_err=%v sqlite=%q)",
				len(c.FreelistTrunks), len(diffs), maxErr, len(maxDiffs), maxIC, overErr != nil, overIC),
			held)
	}

	// CEN-02: Local/overflow boundaries on TableLeaf (512, 4096, 65536) AND IndexLeaf + IndexInterior (4096).
	{
		var issues []string
		// TableLeaf boundaries across 3 page sizes
		for _, ps := range []int{512, 4096, 65536} {
			maxL := dbfile.MaxLocalPayload(ps, dbfile.TableLeaf)
			minL := dbfile.MinLocalPayload(ps)
			perOvfl := ps - 4
			// Row schema: (id INTEGER PRIMARY KEY, b BLOB) -> id stored as serial type 0 (1 byte).
			// Record header size: 1 (hdrlen) + 1 (null) + varintLen(2*blobLen+12).
			blobForPayload := func(wantPayload int) int {
				for hdr := 3; hdr <= 6; hdr++ {
					bl := wantPayload - hdr
					if bl < 0 {
						continue
					}
					st := uint64(2*bl + 12)
					stLen := dbfile.VarintLength(st)
					hSize := 1 + 1 + stLen // wait: hdrlen itself is 1 byte (since <=127)
					if hSize == hdr {
						return bl
					}
				}
				return -1
			}
			targets := []struct {
				label     string
				payload   int
				wantLocal int
				wantOvfl  int
			}{
				{"maxLocal", maxL, maxL, 0},
				{"maxLocal+1", maxL + 1, minL, 1},
				{"maxLocal+1ovfl", maxL + perOvfl, maxL, 1},
				{"maxLocal+1ovfl+1", maxL + perOvfl + 1, minL, 2},
			}
			p := filepath.Join(scratch, fmt.Sprintf("lim-tbl-%d.db", ps))
			stmts := []string{"CREATE TABLE t(id INTEGER PRIMARY KEY, b BLOB)"}
			for i, tgt := range targets {
				bl := blobForPayload(tgt.payload)
				if bl < 0 {
					fatal("blobForPayload", fmt.Errorf("ps=%d payload=%d", ps, tgt.payload))
				}
				stmts = append(stmts, fmt.Sprintf("INSERT INTO t VALUES(%d, zeroblob(%d))", i+1, bl))
			}
			if err := newDB(ctx, p, func(o *sqlitex.Options) { o.File.PageSize = ps }, stmts...); err != nil {
				fatal("build lim-tbl", err)
			}
			f, err := dbfile.Open(p)
			if err != nil {
				fatal("open lim-tbl", err)
			}
			err = f.WalkTable(2, func(row dbfile.Row) error {
				tgt := targets[row.Rowid-1]
				pg, err := f.BTreePage(row.Page)
				if err != nil {
					return err
				}
				for ci := range pg.CellCount {
					cell, err := pg.Cell(ci)
					if err != nil {
						return err
					}
					if cell.Rowid == row.Rowid {
						if int(cell.PayloadSize) != tgt.payload || cell.LocalSize != tgt.wantLocal || len(row.OverflowPages) != tgt.wantOvfl {
							issues = append(issues, fmt.Sprintf("ps=%d %s: got pay=%d loc=%d ovfl=%d want %d/%d/%d",
								ps, tgt.label, cell.PayloadSize, cell.LocalSize, len(row.OverflowPages), tgt.payload, tgt.wantLocal, tgt.wantOvfl))
						}
					}
				}
				return nil
			})
			f.Close()
			if err != nil {
				issues = append(issues, fmt.Sprintf("ps=%d walk err: %v", ps, err))
			}
			_, diffs, err := verifyCensusAgainstSQLite(ctx, p, false)
			if err != nil || len(diffs) > 0 {
				issues = append(issues, fmt.Sprintf("ps=%d census err=%v diffs=%v", ps, err, diffs))
			}
		}

		// IndexLeaf AND IndexInterior overflow boundaries at page_size=4096 (index maxLocal=1002, minLocal=489).
		// Insert enough 1200-byte and 5200-byte indexed blobs so the index grows an interior page whose
		// divider cells ALSO overflow.
		idxPath := filepath.Join(scratch, "lim-idx.db")
		idxStmts := []string{
			"CREATE TABLE t(id INTEGER PRIMARY KEY, k BLOB)",
			"CREATE INDEX idx_k ON t(k)",
			// Exact index maxLocal boundary (index record: hdr + k + rowid; ~1002 bytes):
			"INSERT INTO t VALUES(1, zeroblob(996))",
			"INSERT INTO t VALUES(2, zeroblob(997))",
			"INSERT INTO t VALUES(3, zeroblob(998))",
			"INSERT INTO t VALUES(4, zeroblob(999))",
			// Force index interior page + overflow on both index-leaf and index-interior cells:
			"WITH RECURSIVE n(i) AS (SELECT 10 UNION ALL SELECT i+1 FROM n WHERE i < 40) INSERT INTO t SELECT i, printf('%04d', i) || zeroblob(1500) FROM n",
			"WITH RECURSIVE n(i) AS (SELECT 100 UNION ALL SELECT i+1 FROM n WHERE i < 115) INSERT INTO t SELECT i, printf('%04d', i) || zeroblob(5200) FROM n",
		}
		if err := newDB(ctx, idxPath, nil, idxStmts...); err != nil {
			fatal("build lim-idx", err)
		}
		cIdx, idxDiffs, err := verifyCensusAgainstSQLite(ctx, idxPath, false)
		if err != nil {
			issues = append(issues, fmt.Sprintf("idx census err=%v", err))
		}
		if len(idxDiffs) > 0 {
			issues = append(issues, fmt.Sprintf("idx diffs=%v", idxDiffs))
		}
		// Verify we actually exercised index-interior overflow cells.
		idxInteriorOvfl := 0
		if cIdx != nil {
			for _, p := range cIdx.Pages {
				if p.Role == dbfile.RoleOverflow && p.Tree == "idx_k" {
					parentRole := cIdx.Page(p.Parent).Role
					if parentRole == dbfile.RoleIndexInterior {
						idxInteriorOvfl++
					}
				}
			}
		}
		if idxInteriorOvfl == 0 {
			issues = append(issues, "did not trigger index-interior overflow")
		}

		report("CEN-02",
			"local/overflow boundaries (maxLocal, +1, +1ovfl, +1ovfl+1) on TableLeaf (512/4096/65536) and IndexLeaf/IndexInterior (4096)",
			"exact local/overflow splits hold across all 3 page sizes and index interior/leaf overflow matches dbstat",
			fmt.Sprintf("issues=%d, idx_interior_ovfl_pages=%d (%v)", len(issues), idxInteriorOvfl, issues),
			len(issues) == 0)
	}

	// CEN-03: WITHOUT ROWID table (completely valid SQLite database).
	// Attacks 2.2's claim that dbfile can census every valid SQLite database.
	{
		p := filepath.Join(scratch, "without-rowid.db")
		err := newDB(ctx, p, nil,
			"CREATE TABLE wr(k TEXT PRIMARY KEY, v BLOB) WITHOUT ROWID",
			"CREATE INDEX wr_len ON wr(length(v))",
			"INSERT INTO wr VALUES('alpha', zeroblob(5000))",
			"INSERT INTO wr VALUES('beta', x'deadbeef')",
			"INSERT INTO wr VALUES('gamma', zeroblob(100))",
		)
		if err != nil {
			fatal("build without-rowid", err)
		}
		ic, err := queryIntegrity(ctx, p)
		if err != nil {
			fatal("ic without-rowid", err)
		}
		c, diffs, cenErr := verifyCensusAgainstSQLite(ctx, p, false)
		held := ic == "ok" && cenErr == nil && len(diffs) == 0 && c != nil
		report("CEN-03",
			"census a valid SQLite database containing a WITHOUT ROWID table (with overflow and secondary index)",
			"dbfile.Census succeeds, Complete()==nil, and matches dbstat (SQLite integrity_check=ok)",
			fmt.Sprintf("sqlite_ic=%q, census_err=%v, diffs=%v", ic, cenErr, diffs),
			held)
	}

	// CEN-04: Multi-page sqlite_schema (page 1 becomes table-interior) + overflowing sqlite_schema row
	// + VIEW, TRIGGER, AUTOINCREMENT (sqlite_sequence), UNIQUE (sqlite_autoindex_* with NULL sql), empty tables.
	{
		p := filepath.Join(scratch, "big-schema.db")
		stmts := []string{
			"CREATE TABLE seq_t(id INTEGER PRIMARY KEY AUTOINCREMENT, u TEXT UNIQUE)",
			"INSERT INTO seq_t(u) VALUES('a'), ('b')",
			"CREATE TABLE empty_t(id INTEGER PRIMARY KEY, x TEXT)",
			"CREATE VIEW v_seq AS SELECT id, u FROM seq_t",
			"CREATE TRIGGER tr_seq AFTER INSERT ON seq_t BEGIN SELECT 1; END",
		}
		// One table whose CREATE TABLE SQL exceeds 5000 bytes so sqlite_schema itself overflows!
		var cols []string
		for i := 1; i <= 180; i++ {
			cols = append(cols, fmt.Sprintf("long_column_name_%03d INTEGER DEFAULT %d", i, i))
		}
		stmts = append(stmts, fmt.Sprintf("CREATE TABLE wide_ddl(id INTEGER PRIMARY KEY, %s)", strings.Join(cols, ", ")))
		// 180 more tables so sqlite_schema (page 1) splits into table-interior (0x05) with multiple leaf pages.
		for i := 1; i <= 180; i++ {
			stmts = append(stmts, fmt.Sprintf("CREATE TABLE tbl_%03d(id INTEGER PRIMARY KEY, v TEXT)", i))
		}
		if err := newDB(ctx, p, nil, stmts...); err != nil {
			fatal("build big-schema", err)
		}
		c, diffs, err := verifyCensusAgainstSQLite(ctx, p, false)
		if err != nil {
			report("CEN-04",
				"census DB with multi-page sqlite_schema (page 1 interior), overflowing schema DDL, views/triggers/autoindex/sqlite_sequence/empty tables",
				"dbfile.Census accounts for all pages and matches dbstat",
				fmt.Sprintf("census_err=%v", err),
				false)
		} else {
			p1Role := c.Page(1).Role
			schemaOvfl := 0
			for _, pg := range c.Pages {
				if pg.Role == dbfile.RoleOverflow && pg.Tree == "sqlite_schema" {
					schemaOvfl++
				}
			}
			held := len(diffs) == 0 && p1Role == dbfile.RoleTableInterior && schemaOvfl > 0
			report("CEN-04",
				"census DB with multi-page sqlite_schema (page 1 interior), overflowing schema DDL, views/triggers/autoindex/sqlite_sequence/empty tables",
				"page 1 is table-interior, sqlite_schema has overflow page(s), census is complete and matches dbstat",
				fmt.Sprintf("pages=%d, page1=%s, schema_ovfl=%d, schema_rows=%d, diffs=%d (%v)",
					c.PageCount, p1Role, schemaOvfl, len(c.Schema), len(diffs), diffs),
				held)
		}
	}

	// CEN-05: Non-zero ReservedBytes (16 bytes at page_size=4096, and 32 bytes at page_size=512 -> usableSize=480).
	{
		var issues []string
		for _, tc := range []struct {
			name string
			ps   int
			res  int
			av   string
		}{
			{"ps4096-res16", 4096, 16, "FULL"},
			{"ps512-res32-minUsable480", 512, 32, "NONE"},
		} {
			p := filepath.Join(scratch, tc.name+".db")
			sqlScript := fmt.Sprintf(`
PRAGMA page_size = %d;
PRAGMA auto_vacuum = %s;
.filectrl reserve_bytes %d
VACUUM;
CREATE TABLE t(id INTEGER PRIMARY KEY, k TEXT, b BLOB);
CREATE INDEX idx_k ON t(k);
WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i < 40)
  INSERT INTO t SELECT i, printf('key-%%04d', i), zeroblob(4200) FROM n;
DELETE FROM t WHERE id %% 3 = 0;
`, tc.ps, tc.av, tc.res)
			if _, err := cli.Exec(ctx, p, sqlScript); err != nil {
				fatal("cli reserve_bytes "+tc.name, err)
			}
			f, err := dbfile.Open(p)
			if err != nil {
				issues = append(issues, fmt.Sprintf("%s: open err=%v", tc.name, err))
				continue
			}
			gotRes := f.Header.ReservedBytes
			gotUsable := f.UsableSize()
			f.Close()
			if gotRes != tc.res || gotUsable != tc.ps-tc.res {
				issues = append(issues, fmt.Sprintf("%s: res=%d usable=%d", tc.name, gotRes, gotUsable))
			}
			_, diffs, err := verifyCensusAgainstSQLite(ctx, p, false)
			if err != nil || len(diffs) > 0 {
				issues = append(issues, fmt.Sprintf("%s: census err=%v diffs=%v", tc.name, err, diffs))
			}
		}
		report("CEN-05",
			"databases with non-zero reserved bytes (4096/res=16/auto_vacuum=FULL and 512/res=32/usable=480) via CLI .filectrl",
			"dbfile decodes reserved bytes and usable size, and census matches dbstat on all columns",
			fmt.Sprintf("issues=%d (%v)", len(issues), issues),
			len(issues) == 0)
	}

	// CEN-06: > 1 GiB sparse file at page_size=4096 (lock-byte page = 262145, 250+ freelist trunks)
	// WITH a live overflow page placed AT page 262146 (lockBytePage + 1, straddling 1 GiB!).
	{
		const pageSize = 4096
		const lockPage = dbfile.LockByteOffset/pageSize + 1 // 262145
		const totalPages = lockPage + 1                     // 262146
		p := filepath.Join(scratch, "large-4k-straddle.db")
		// Build a 3-page rollback DB: page 1 = schema, page 2 = table leaf, page 3 = overflow page of row 1.
		err := newDB(ctx, p, func(o *sqlitex.Options) {
			o.File.PageSize = pageSize
			o.File.JournalMode = sqlitex.JournalDelete
		},
			"CREATE TABLE t(id INTEGER PRIMARY KEY, b BLOB)",
			"INSERT INTO t VALUES(1, zeroblob(5000))",
		)
		if err != nil {
			fatal("build large-4k base", err)
		}
		base, err := os.ReadFile(p)
		if err != nil {
			fatal("read large-4k base", err)
		}
		if len(base) != 3*pageSize {
			fatal("unexpected base size", fmt.Errorf("got %d want %d", len(base), 3*pageSize))
		}
		// Verify page 2 cell 0 points to overflow page 3, and relocate page 3 -> page 262146!
		f0, err := dbfile.FromBytes(base)
		if err != nil {
			fatal("parse base", err)
		}
		pg2, err := f0.BTreePage(2)
		if err != nil {
			fatal("pg2", err)
		}
		cell0, err := pg2.Cell(0)
		if err != nil || cell0.FirstOverflow != 3 {
			fatal("cell0", fmt.Errorf("err=%v ovfl=%d", err, cell0.FirstOverflow))
		}
		// Locate the 4-byte overflow pointer inside cell 0 (last 4 bytes of cell).
		ovflPtrOff := pageSize + cell0.Offset + cell0.Size - 4
		if binary.BigEndian.Uint32(base[ovflPtrOff:ovflPtrOff+4]) != 3 {
			fatal("ovflPtrOff mismatch", fmt.Errorf("got %d", binary.BigEndian.Uint32(base[ovflPtrOff:ovflPtrOff+4])))
		}
		ovflPageData := append([]byte(nil), base[2*pageSize:3*pageSize]...)
		// Patch cell 0's FirstOverflow from 3 -> totalPages (262146 = lockPage + 1)!
		binary.BigEndian.PutUint32(base[ovflPtrOff:ovflPtrOff+4], uint32(totalPages))

		// Free pages: 3 .. lockPage-1 (262142 pages total). Pack into multi-trunk freelist!
		// Writer limit at 4096 is 1016 leaves per trunk; reader limit is 1022. Use 1016.
		const leavesPerTrunk = 1016
		firstFree := 3
		lastFree := lockPage - 1
		totalFree := lastFree - firstFree + 1

		binary.BigEndian.PutUint32(base[28:32], uint32(totalPages))
		binary.BigEndian.PutUint32(base[32:36], uint32(firstFree))
		binary.BigEndian.PutUint32(base[36:40], uint32(totalFree))

		out, err := os.OpenFile(p, os.O_WRONLY|os.O_TRUNC, 0)
		if err != nil {
			fatal("open large-4k", err)
		}
		if _, err := out.Write(base[:2*pageSize]); err != nil {
			out.Close()
			fatal("write p1-p2", err)
		}
		// Write freelist trunks sparsely: each trunk sits at `cur`, lists `cur+1 .. cur+n` as leaves.
		trunkBuf := make([]byte, pageSize)
		trunkCount := 0
		for cur := firstFree; cur <= lastFree; {
			trunkCount++
			clear(trunkBuf)
			remAfterTrunk := lastFree - cur
			nLeaves := min(remAfterTrunk, leavesPerTrunk)
			nextTrunk := cur + 1 + nLeaves
			if nextTrunk > lastFree {
				nextTrunk = 0
			}
			binary.BigEndian.PutUint32(trunkBuf[0:4], uint32(nextTrunk))
			binary.BigEndian.PutUint32(trunkBuf[4:8], uint32(nLeaves))
			for i := 0; i < nLeaves; i++ {
				binary.BigEndian.PutUint32(trunkBuf[8+4*i:], uint32(cur+1+i))
			}
			if _, err := out.WriteAt(trunkBuf, int64(cur-1)*pageSize); err != nil {
				out.Close()
				fatal("write trunk", err)
			}
			if nextTrunk == 0 {
				break
			}
			cur = nextTrunk
		}
		// Write the relocated overflow page at page 262146 (past 1 GiB!).
		if _, err := out.WriteAt(ovflPageData, int64(totalPages-1)*pageSize); err != nil {
			out.Close()
			fatal("write ovfl past 1GiB", err)
		}
		if err := out.Close(); err != nil {
			fatal("close large-4k", err)
		}

		// Parse and census with dbfile, and read row 1 via WalkTable.
		fLarge, err := dbfile.Open(p)
		if err != nil {
			fatal("dbfile open large-4k", err)
		}
		cLarge, cenErr := fLarge.Census()
		var compErr error
		var walkLen int
		var walkOvfl []int
		if cenErr == nil {
			compErr = cLarge.Complete()
			_ = fLarge.WalkTable(2, func(row dbfile.Row) error {
				walkLen = len(row.Values[1].Bytes)
				walkOvfl = row.OverflowPages
				return nil
			})
		}
		fLarge.Close()

		// Cross-check with SQLite (read-only).
		ic, icErr := queryIntegrity(ctx, p)
		ro, err := openRaw(p, true)
		if err != nil {
			fatal("open large-4k ro", err)
		}
		var sqlLen int
		sqlErr := ro.QueryRowContext(ctx, "SELECT length(b) FROM t WHERE id = 1").Scan(&sqlLen)
		ro.Close()

		os.Remove(p) // free the sparse file immediately
		held := cenErr == nil && compErr == nil && cLarge.Page(lockPage).Role == dbfile.RoleLockByte &&
			cLarge.Page(totalPages).Role == dbfile.RoleOverflow &&
			walkLen == 5000 && len(walkOvfl) == 1 && walkOvfl[0] == totalPages &&
			icErr == nil && ic == "ok" && sqlErr == nil && sqlLen == 5000
		report("CEN-06",
			"sparse >1 GiB 4 KiB-page DB (262146 pages, 258 freelist trunks) with live overflow page at lockByte+1 (262146)",
			"dbfile marks 262145 lock-byte, 262146 overflow, Complete()==nil, reads 5000-byte blob; SQLite integrity_check=ok",
			fmt.Sprintf("trunks=%d cenErr=%v compErr=%v lockRole=%v p262146=%v walkLen=%d walkOvfl=%v sqlite_ic=%q sqlLen=%d",
				trunkCount, cenErr, compErr,
				func() string {
					if cLarge == nil {
						return "?"
					}
					return cLarge.Page(lockPage).Role.String()
				}(),
				func() string {
					if cLarge == nil {
						return "?"
					}
					return cLarge.Page(totalPages).Role.String()
				}(),
				walkLen, walkOvfl, ic, sqlLen),
			held)
	}

	// CEN-07: All 8 page sizes (512..65536) full census + dbstat + WalkTable row-hash parity.
	{
		sizes := []int{512, 1024, 2048, 4096, 8192, 16384, 32768, 65536}
		var bad []string
		for _, ps := range sizes {
			p := filepath.Join(scratch, fmt.Sprintf("all-ps-%d.db", ps))
			err := newDB(ctx, p, func(o *sqlitex.Options) { o.File.PageSize = ps },
				"CREATE TABLE items(id INTEGER PRIMARY KEY, a, b, c)",
				"CREATE INDEX idx_a ON items(a)",
				"INSERT INTO items VALUES(-5, NULL, 0, 1)",
				"INSERT INTO items VALUES(1, -128, 32767, 8388607)",
				"INSERT INTO items VALUES(2, 2147483647, 140737488355327, -9223372036854775808)",
				"INSERT INTO items VALUES(3, -0.0, 1e308, 'utf8-π-text')",
				"INSERT INTO items VALUES(4, x'00ff10', zeroblob(70000), '')",
				"WITH RECURSIVE n(i) AS (SELECT 10 UNION ALL SELECT i+1 FROM n WHERE i < 80) INSERT INTO items SELECT i, printf('k-%03d', i), i*i, zeroblob(300) FROM n",
				"DELETE FROM items WHERE id BETWEEN 20 AND 50",
			)
			if err != nil {
				fatal("build all-ps", err)
			}
			_, diffs, err := verifyCensusAgainstSQLite(ctx, p, false)
			if err != nil || len(diffs) > 0 {
				bad = append(bad, fmt.Sprintf("ps=%d census err=%v diffs=%d", ps, err, len(diffs)))
				continue
			}
			f, err := dbfile.Open(p)
			if err != nil {
				bad = append(bad, fmt.Sprintf("ps=%d reopen err=%v", ps, err))
				continue
			}
			nOurs, hOurs, err := hashTableOurs(f, 2)
			f.Close()
			if err != nil {
				bad = append(bad, fmt.Sprintf("ps=%d walk err=%v", ps, err))
				continue
			}
			db, err := openRaw(p, false)
			if err != nil {
				fatal("open all-ps sqlite", err)
			}
			nSQL, hSQL, err := hashTableSQLite(ctx, db, "items", []string{"id", "a", "b", "c"})
			db.Close()
			if err != nil || nOurs != nSQL || hOurs != hSQL {
				bad = append(bad, fmt.Sprintf("ps=%d hash ours=(%d,%s) sql=(%d,%s) err=%v", ps, nOurs, hOurs[:8], nSQL, hSQL[:8], err))
			}
		}
		report("CEN-07",
			"full census + dbstat + WalkTable row-hash parity across all 8 valid page sizes (512..65536)",
			"all 8 page sizes match dbstat on all columns and produce identical row hashes to SQLite",
			fmt.Sprintf("checked 8 sizes, failures=%d (%v)", len(bad), bad),
			len(bad) == 0)
	}

	// CEN-08: UTF-16le/be non-ASCII schema & rows + STRICT table + ALTER TABLE ADD COLUMN DEFAULT.
	{
		var issues []string
		for _, enc := range []string{"UTF-16le", "UTF-16be"} {
			p := filepath.Join(scratch, "feat-"+enc+".db")
			err := newDB(ctx, p, func(o *sqlitex.Options) { o.File.Encoding = enc },
				"CREATE TABLE café(id INTEGER PRIMARY KEY, label TEXT, amt INT) STRICT",
				"CREATE INDEX idx_café ON café(label)",
				"INSERT INTO café VALUES(1, 'crème brûlée \U0001D11E', 42)",
				"ALTER TABLE café ADD COLUMN tag TEXT DEFAULT 'legacy'",
				"INSERT INTO café VALUES(2, 'matcha \U00010400', 99, 'new')",
			)
			if err != nil {
				fatal("build feat-"+enc, err)
			}
			_, diffs, err := verifyCensusAgainstSQLite(ctx, p, false)
			if err != nil || len(diffs) > 0 {
				issues = append(issues, fmt.Sprintf("%s census err=%v diffs=%v", enc, err, diffs))
				continue
			}
			f, err := dbfile.Open(p)
			if err != nil {
				issues = append(issues, fmt.Sprintf("%s open err=%v", enc, err))
				continue
			}
			var decodedLabels []string
			var colCounts []int
			err = f.WalkTable(2, func(r dbfile.Row) error {
				colCounts = append(colCounts, len(r.Values))
				s, err := dbfile.DecodeText(r.Values[1].Bytes, f.Header.TextEncoding)
				if err != nil {
					return err
				}
				decodedLabels = append(decodedLabels, s)
				return nil
			})
			f.Close()
			if err != nil {
				issues = append(issues, fmt.Sprintf("%s walk err=%v", enc, err))
				continue
			}
			if len(decodedLabels) != 2 || decodedLabels[0] != "crème brûlée \U0001D11E" || decodedLabels[1] != "matcha \U00010400" {
				issues = append(issues, fmt.Sprintf("%s decoded labels=%q", enc, decodedLabels))
			}
			// Row 1 was inserted before ALTER TABLE ADD COLUMN, so SQLite omits column 3 on disk (3 values);
			// row 2 has all 4 values. Confirm dbfile decodes both without error.
			if len(colCounts) != 2 || colCounts[0] != 3 || colCounts[1] != 4 {
				issues = append(issues, fmt.Sprintf("%s colCounts=%v want [3 4]", enc, colCounts))
			}
		}
		report("CEN-08",
			"UTF-16le and UTF-16be DBs with non-ASCII table/index/surrogate-pair text, STRICT table, and ALTER TABLE ADD COLUMN",
			"census matches dbstat, DecodeText decodes non-ASCII & emoji UTF-16 accurately, pre-ALTER rows decode cleanly",
			fmt.Sprintf("issues=%d (%v)", len(issues), issues),
			len(issues) == 0)
	}
}
