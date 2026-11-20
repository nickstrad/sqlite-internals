package main

import (
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"math"
	"math/rand/v2"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickstrad/sqlite-internals/internal/dbfile"
	"github.com/nickstrad/sqlite-internals/internal/recipe"
	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
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

func newDB(ctx context.Context, path string, edit func(*sqlitex.Options), stmts ...string) error {
	opts := sqlitex.DefaultOptions()
	opts.SingleConnection = true
	opts.Conn.SecureDelete = true
	opts.File.JournalMode = sqlitex.JournalDelete
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
	return db.Close()
}

func sqliteIntegrity(ctx context.Context, path string) string {
	uri, err := sqlitex.FileURI(path, url.Values{"mode": {"ro"}})
	if err != nil {
		return "uri-err: " + err.Error()
	}
	db, err := sql.Open("sqlite", uri)
	if err != nil {
		return "open-err: " + err.Error()
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	rows, err := db.QueryContext(ctx, "PRAGMA integrity_check")
	if err != nil {
		return "query-err: " + err.Error()
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var l string
		if err := rows.Scan(&l); err != nil {
			return "scan-err: " + err.Error()
		}
		lines = append(lines, strings.Split(l, "\n")...)
	}
	if err := rows.Err(); err != nil {
		return "rows-err: " + err.Error()
	}
	if len(lines) > 2 {
		lines = append(lines[:2], fmt.Sprintf("(+%d)", len(lines)-2))
	}
	return strings.Join(lines, " / ")
}

// exerciseAll parses image through every public read path in internal/dbfile under recover().
func exerciseAll(image []byte) (panicked any, err error) {
	defer func() {
		if r := recover(); r != nil {
			panicked = r
		}
	}()
	f, err := dbfile.FromBytes(image)
	if err != nil {
		return nil, err
	}
	c, err := f.Census()
	if err != nil {
		return nil, err
	}
	if err := c.Complete(); err != nil {
		return nil, err
	}
	for _, t := range c.Trees {
		if t.Type == "table" {
			if err := f.WalkTable(t.RootPage, func(r dbfile.Row) error {
				for _, v := range r.Values {
					_ = v.String()
					if v.Class == dbfile.Text {
						_, _ = dbfile.DecodeText(v.Bytes, f.Header.TextEncoding)
					}
				}
				return nil
			}); err != nil {
				return nil, err
			}
		} else {
			w := dbfile.TreeWalker{
				File: f,
				OnCell: func(p *dbfile.BTreePage, cell dbfile.Cell) error {
					payload, _, err := f.Payload(p.Number, cell)
					if err != nil {
						return err
					}
					_, err = dbfile.DecodeRecord(payload)
					return err
				},
			}
			if err := w.Walk(t.RootPage); err != nil {
				return nil, err
			}
		}
	}
	return nil, nil
}

func main() {
	ctx := context.Background()
	repoRoot, err := sqlitex.RepoRoot()
	if err != nil {
		fatal("repo root", err)
	}
	scratch := filepath.Join(repoRoot, "_work", "adv-ch02", "corrupt-fuzz")
	if err := os.RemoveAll(scratch); err != nil {
		fatal("clean scratch", err)
	}
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		fatal("mkdir scratch", err)
	}

	// Build a rollback-mode 6-step recipe DB containing every page role:
	// table-interior, table-leaf, index-interior, index-leaf, overflow, freelist-trunk, freelist-leaf.
	r6Path := filepath.Join(scratch, "r6.db")
	if err := recipe.Default().WithJournalMode("delete").Build(ctx, r6Path, 10); err != nil {
		fatal("build r10", err)
	}
	r6Bytes, err := os.ReadFile(r6Path)
	if err != nil {
		fatal("read r10", err)
	}
	f6, err := dbfile.FromBytes(r6Bytes)
	if err != nil {
		fatal("parse r10", err)
	}
	c6, err := f6.Census()
	if err != nil {
		fatal("census r10", err)
	}
	pageSize := f6.PageSize()

	// Locate one representative page for each role.
	repPage := map[dbfile.Role]int{}
	for _, p := range c6.Pages {
		if p.Number == 1 {
			continue
		}
		if _, ok := repPage[p.Role]; !ok {
			repPage[p.Role] = p.Number
		}
	}

	// COR-01: Exhaustive single-byte corruption of every header byte across all page roles.
	{
		type span struct {
			label string
			start int
			count int
		}
		spans := []span{
			{"db-header", 0, 100},
			{"page1-btree-hdr", 100, 12},
		}
		for _, role := range []dbfile.Role{
			dbfile.RoleTableInterior, dbfile.RoleTableLeaf,
			dbfile.RoleIndexInterior, dbfile.RoleIndexLeaf,
			dbfile.RoleOverflow, dbfile.RoleFreelistTrunk,
		} {
			if pg, ok := repPage[role]; ok {
				hdrLen := 12
				if role == dbfile.RoleOverflow {
					hdrLen = 4
				} else if role == dbfile.RoleFreelistTrunk {
					hdrLen = 8
				}
				spans = append(spans, span{role.String(), (pg - 1) * pageSize, hdrLen})
			}
		}
		muts := []func(byte) byte{
			func(b byte) byte { return b ^ 0x01 },
			func(b byte) byte { return b ^ 0x80 },
			func(b byte) byte { return b ^ 0xff },
			func(b byte) byte { return 0x00 },
		}
		totalCases := 0
		panics := 0
		rejected := 0
		var panicSamples []string
		buf := make([]byte, len(r6Bytes))
		for _, sp := range spans {
			for off := sp.start; off < sp.start+sp.count; off++ {
				for mi, fn := range muts {
					copy(buf, r6Bytes)
					buf[off] = fn(buf[off])
					if buf[off] == r6Bytes[off] {
						buf[off] = 0x5a
					}
					totalCases++
					p, err := exerciseAll(buf)
					if p != nil {
						panics++
						if len(panicSamples) < 3 {
							panicSamples = append(panicSamples, fmt.Sprintf("%s+%d(m%d):%v", sp.label, off-sp.start, mi, p))
						}
					} else if err != nil {
						rejected++
					}
				}
			}
		}
		report("COR-01",
			fmt.Sprintf("exhaustive single-byte header mutations across DB header + 7 page roles (%d mutations)", totalCases),
			"0 panics across all single-byte header mutations",
			fmt.Sprintf("tested=%d, panics=%d (%v), rejected=%d, accepted=%d", totalCases, panics, panicSamples, rejected, totalCases-rejected-panics),
			panics == 0)
	}

	// COR-02: Targeted page-header byte corruptions where SQLite integrity_check fails —
	// does internal/dbfile also report an error? (Attacks 2.2 bullet: "one flipped byte in a page header
	// (the inspector must report an error, not panic)").
	{
		basePath := filepath.Join(scratch, "hdr-flip-base.db")
		if err := newDB(ctx, basePath, nil,
			"CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT)",
			"INSERT INTO t VALUES(1, 'aaaaaaaaaa'), (2, 'bbbbbbbbbb'), (3, 'cccccccccc'), (4, 'dddddddddd')",
			// Delete row 2 so page 2 has a real non-zero FirstFreeblock!
			"DELETE FROM t WHERE id = 2",
		); err != nil {
			fatal("build hdr-flip-base", err)
		}
		good, err := os.ReadFile(basePath)
		if err != nil {
			fatal("read hdr-flip-base", err)
		}
		p2Off := 4096
		// Verify page 2 has a freeblock.
		origFB := binary.BigEndian.Uint16(good[p2Off+1 : p2Off+3])
		origStart := binary.BigEndian.Uint16(good[p2Off+5 : p2Off+7])

		cases := []struct {
			name string
			edit func([]byte)
		}{
			{"byte7-fragmented=200", func(b []byte) { b[p2Off+7] = 200 }},
			{"byte1-2-zero-existing-freeblock", func(b []byte) { binary.BigEndian.PutUint16(b[p2Off+1:], 0) }},
			{"byte5-6-lower-content-start-by-20", func(b []byte) { binary.BigEndian.PutUint16(b[p2Off+5:], origStart-20) }},
		}
		var missed []string
		for i, tc := range cases {
			cp := append([]byte(nil), good...)
			tc.edit(cp)
			tp := filepath.Join(scratch, fmt.Sprintf("hdr-flip-%d.db", i+1))
			if err := os.WriteFile(tp, cp, 0o644); err != nil {
				fatal("write hdr-flip", err)
			}
			pan, dbErr := exerciseAll(cp)
			sqlIC := sqliteIntegrity(ctx, tp)
			if pan != nil || (sqlIC != "ok" && dbErr == nil) {
				missed = append(missed, fmt.Sprintf("%s(origFB=%d,dbfile_err=%v,sqlite=%q)", tc.name, origFB, dbErr, sqlIC))
			}
		}
		report("COR-02",
			"corrupt page-2 header fields (fragmented=200, zero real freeblock, lower content start) vs SQLite integrity_check",
			"inspector reports an error on every page-header corruption that SQLite integrity_check rejects",
			fmt.Sprintf("missed=%d (%v)", len(missed), missed),
			len(missed) == 0)
	}

	// COR-03a & COR-03b: Overflow chain corruptions (first/interior vs LAST overflow page next pointer).
	{
		basePath := filepath.Join(scratch, "ovfl-base.db")
		if err := newDB(ctx, basePath, nil,
			"CREATE TABLE t(id INTEGER PRIMARY KEY, b BLOB)",
			"INSERT INTO t VALUES(1, zeroblob(10000))", // 2 overflow pages (3, 4)
			"INSERT INTO t VALUES(2, zeroblob(10000))", // 2 overflow pages (5, 6)
		); err != nil {
			fatal("build ovfl-base", err)
		}
		good, err := os.ReadFile(basePath)
		if err != nil {
			fatal("read ovfl-base", err)
		}
		f, err := dbfile.FromBytes(good)
		if err != nil {
			fatal("parse ovfl-base", err)
		}
		var r1Ovfl, r2Ovfl []int
		_ = f.WalkTable(2, func(r dbfile.Row) error {
			if r.Rowid == 1 {
				r1Ovfl = r.OverflowPages
			} else if r.Rowid == 2 {
				r2Ovfl = r.OverflowPages
			}
			return nil
		})
		if len(r1Ovfl) != 2 || len(r2Ovfl) != 2 {
			fatal("unexpected ovfl chains", fmt.Errorf("r1=%v r2=%v", r1Ovfl, r2Ovfl))
		}
		// Find row 1's FirstOverflow offset on page 2.
		pg2, _ := f.BTreePage(2)
		c0, _ := pg2.Cell(0)
		c0OvflPtrOff := 4096 + c0.Offset + c0.Size - 4

		// COR-03a: First/interior overflow pointer corruptions (all must be rejected by dbfile).
		casesA := []struct {
			name string
			edit func([]byte)
		}{
			{"firstOvfl=0", func(b []byte) { binary.BigEndian.PutUint32(b[c0OvflPtrOff:], 0) }},
			{"firstOvfl=1", func(b []byte) { binary.BigEndian.PutUint32(b[c0OvflPtrOff:], 1) }},
			{"firstOvfl=999", func(b []byte) { binary.BigEndian.PutUint32(b[c0OvflPtrOff:], 999) }},
			{"firstOvfl-self-loop", func(b []byte) { binary.BigEndian.PutUint32(b[(r1Ovfl[0]-1)*4096:], uint32(r1Ovfl[0])) }},
			{"firstOvfl-early-end", func(b []byte) { binary.BigEndian.PutUint32(b[(r1Ovfl[0]-1)*4096:], 0) }},
			{"shared-ovfl-page", func(b []byte) { binary.BigEndian.PutUint32(b[(r1Ovfl[0]-1)*4096:], uint32(r2Ovfl[1])) }},
		}
		var badA []string
		for _, tc := range casesA {
			cp := append([]byte(nil), good...)
			tc.edit(cp)
			pan, err := exerciseAll(cp)
			if pan != nil || err == nil {
				badA = append(badA, fmt.Sprintf("%s(panic=%v,err=%v)", tc.name, pan, err))
			}
		}
		report("COR-03a",
			"corrupt first/interior overflow pointers (0, page 1, OOB, self-loop, early-end, cross-row share)",
			"dbfile rejects all 6 with an error and 0 panics",
			fmt.Sprintf("accepted_or_panicked=%d (%v)", len(badA), badA),
			len(badA) == 0)

		// COR-03b: Corrupt the LAST overflow page's next pointer (non-zero: self-loop and 999999).
		// SQLite's checkList follows next until 0 and rejects both; dbfile payload.go:59 skips checking last page's next.
		casesB := []struct {
			name string
			next uint32
		}{
			{"lastOvfl-next=self", uint32(r1Ovfl[1])},
			{"lastOvfl-next=999999", 999999},
		}
		var missedB []string
		for i, tc := range casesB {
			cp := append([]byte(nil), good...)
			binary.BigEndian.PutUint32(cp[(r1Ovfl[1]-1)*4096:], tc.next)
			tp := filepath.Join(scratch, fmt.Sprintf("ovfl-last-%d.db", i+1))
			if err := os.WriteFile(tp, cp, 0o644); err != nil {
				fatal("write ovfl-last", err)
			}
			pan, dbErr := exerciseAll(cp)
			sqlIC := sqliteIntegrity(ctx, tp)
			if pan != nil || (sqlIC != "ok" && dbErr == nil) {
				missedB = append(missedB, fmt.Sprintf("%s(dbfile_err=%v,sqlite=%q)", tc.name, dbErr, sqlIC))
			}
		}
		report("COR-03b",
			"corrupt LAST overflow page's next pointer (to self and to 999999) vs SQLite integrity_check",
			"dbfile rejects non-zero next pointer on last overflow page just as SQLite integrity_check does",
			fmt.Sprintf("missed=%d (%v)", len(missedB), missedB),
			len(missedB) == 0)
	}

	// COR-04: Freelist, B-tree child pointer, pointer-map, lock-byte, and depth>=20 structural corruptions.
	{
		// Base 1: normal rollback DB with freelist trunk + leaf + index + table-interior.
		basePath := filepath.Join(scratch, "struct-base.db")
		if err := newDB(ctx, basePath, func(o *sqlitex.Options) { o.File.PageSize = 512 },
			"CREATE TABLE t(id INTEGER PRIMARY KEY, k TEXT, b BLOB)",
			"CREATE INDEX idx_k ON t(k)",
			"WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i < 40) INSERT INTO t SELECT i, printf('k%03d',i), zeroblob(120) FROM n",
			"DELETE FROM t WHERE id > 20",
		); err != nil {
			fatal("build struct-base", err)
		}
		good, err := os.ReadFile(basePath)
		if err != nil {
			fatal("read struct-base", err)
		}
		f, err := dbfile.FromBytes(good)
		if err != nil {
			fatal("parse struct-base", err)
		}
		c, err := f.Census()
		if err != nil {
			fatal("census struct-base", err)
		}
		ps := 512
		trunkPg := c.FreelistTrunks[0]
		var tableIntPg, tableLeafPg, idxLeafPg int
		for _, p := range c.Pages {
			if p.Role == dbfile.RoleTableInterior && tableIntPg == 0 {
				tableIntPg = p.Number
			}
			if p.Role == dbfile.RoleTableLeaf && p.Number != 1 && tableLeafPg == 0 {
				tableLeafPg = p.Number
			}
			if p.Role == dbfile.RoleIndexLeaf && idxLeafPg == 0 {
				idxLeafPg = p.Number
			}
		}

		// Base 2: auto_vacuum=FULL DB (has pointer-map page 2).
		avPath := filepath.Join(scratch, "struct-av.db")
		if err := newDB(ctx, avPath, func(o *sqlitex.Options) {
			o.File.PageSize = 512
			o.File.AutoVacuum = sqlitex.AutoVacuumFull
		}, "CREATE TABLE t(id INTEGER PRIMARY KEY, b BLOB)", "INSERT INTO t VALUES(1, zeroblob(1000))"); err != nil {
			fatal("build struct-av", err)
		}
		avGood, err := os.ReadFile(avPath)
		if err != nil {
			fatal("read struct-av", err)
		}
		fAV, _ := dbfile.FromBytes(avGood)
		var avRoot int
		scAV, _ := fAV.Schema()
		avRoot = scAV[0].RootPage

		type structCase struct {
			name string
			base []byte
			edit func([]byte)
		}
		cases := []structCase{
			{"freelist-trunk-self-loop", good, func(b []byte) { binary.BigEndian.PutUint32(b[(trunkPg-1)*ps:], uint32(trunkPg)) }},
			{"freelist-leaf=0", good, func(b []byte) { binary.BigEndian.PutUint32(b[(trunkPg-1)*ps+8:], 0) }},
			{"freelist-leaf=1", good, func(b []byte) { binary.BigEndian.PutUint32(b[(trunkPg-1)*ps+8:], 1) }},
			{"freelist-leaf=tableLeaf", good, func(b []byte) { binary.BigEndian.PutUint32(b[(trunkPg-1)*ps+8:], uint32(tableLeafPg)) }},
			{"freelist-dup-leaf", good, func(b []byte) {
				leaf0 := binary.BigEndian.Uint32(b[(trunkPg-1)*ps+8:])
				binary.BigEndian.PutUint32(b[(trunkPg-1)*ps+12:], leaf0)
			}},
			{"freelist-count=0xffffffff", good, func(b []byte) { binary.BigEndian.PutUint32(b[36:], 0xffffffff) }},
			{"freelist-trunk-nonzero-count-zero", good, func(b []byte) { binary.BigEndian.PutUint32(b[36:], 0) }},
			{"btree-rightchild=0", good, func(b []byte) { binary.BigEndian.PutUint32(b[(tableIntPg-1)*ps+8:], 0) }},
			{"btree-rightchild=1", good, func(b []byte) { binary.BigEndian.PutUint32(b[(tableIntPg-1)*ps+8:], 1) }},
			{"btree-rightchild=self", good, func(b []byte) { binary.BigEndian.PutUint32(b[(tableIntPg-1)*ps+8:], uint32(tableIntPg)) }},
			{"btree-table-points-to-index-leaf", good, func(b []byte) { binary.BigEndian.PutUint32(b[(tableIntPg-1)*ps+8:], uint32(idxLeafPg)) }},
			{"autovac-freelist-points-to-ptrmap2", avGood, func(b []byte) {
				binary.BigEndian.PutUint32(b[32:], 2)
				binary.BigEndian.PutUint32(b[36:], 1)
			}},
			{"autovac-ovfl-points-to-ptrmap2", avGood, func(b []byte) {
				pg, _ := fAV.BTreePage(avRoot)
				cell, _ := pg.Cell(0)
				off := (avRoot-1)*512 + cell.Offset + cell.Size - 4
				binary.BigEndian.PutUint32(b[off:], 2)
			}},
		}
		// Also build a synthetic 22-level deep table-interior chain to trigger MaxTreeDepth (20).
		deepImg := make([]byte, 25*512)
		copy(deepImg, good[:512])
		// Single schema row on page 1? Reuse good's page 1 with rootpage of table t = 2.
		// Simpler: build a 2-page base and turn pages 2..23 into a 22-level chain of 0-cell table-interior pages!
		deepBasePath := filepath.Join(scratch, "deep-base.db")
		if err := newDB(ctx, deepBasePath, func(o *sqlitex.Options) { o.File.PageSize = 512 }, "CREATE TABLE t(id INTEGER PRIMARY KEY)"); err != nil {
			fatal("build deep-base", err)
		}
		deepBase, _ := os.ReadFile(deepBasePath)
		copy(deepImg, deepBase[:2*512])
		binary.BigEndian.PutUint32(deepImg[28:32], 25)
		for pno := 2; pno <= 24; pno++ {
			off := (pno - 1) * 512
			clear(deepImg[off : off+512])
			deepImg[off] = byte(dbfile.TableInterior)
			binary.BigEndian.PutUint16(deepImg[off+5:], 512) // cell content start = usableSize
			binary.BigEndian.PutUint32(deepImg[off+8:], uint32(pno+1))
		}
		// Page 25 is an empty table leaf
		off25 := 24 * 512
		deepImg[off25] = byte(dbfile.TableLeaf)
		binary.BigEndian.PutUint16(deepImg[off25+5:], 512)
		cases = append(cases, structCase{"btree-depth-23-exceeds-MaxTreeDepth", deepImg, func(b []byte) {}})

		var bad []string
		for _, tc := range cases {
			cp := append([]byte(nil), tc.base...)
			tc.edit(cp)
			pan, err := exerciseAll(cp)
			if pan != nil || err == nil {
				bad = append(bad, fmt.Sprintf("%s(panic=%v,err=%v)", tc.name, pan, err))
			}
		}
		report("COR-04",
			fmt.Sprintf("test %d freelist, B-tree child, pointer-map, cross-type, cycle, and depth>20 structural corruptions", len(cases)),
			"all structural corruptions return an error with 0 panics",
			fmt.Sprintf("tested=%d, failed=%d (%v)", len(cases), len(bad), bad),
			len(bad) == 0)
	}

	// COR-05a: Anti-OOM huge payload varint + malformed records (serial types 10, 11, huge, NaN, odd UTF-16).
	{
		var issues []string
		// 1. Huge payload size (0x7fffffff) on a 2-page file must fail fast without allocating 2 GiB.
		p := filepath.Join(scratch, "oom-base.db")
		if err := newDB(ctx, p, nil, "CREATE TABLE t(id INTEGER PRIMARY KEY, b BLOB)", "INSERT INTO t VALUES(1, 'hello')"); err != nil {
			fatal("build oom", err)
		}
		oomImg, _ := os.ReadFile(p)
		fOOM, _ := dbfile.FromBytes(oomImg)
		pg2, _ := fOOM.BTreePage(2)
		c0, _ := pg2.Cell(0)
		// Replace cell 0 at c0.Offset with varint(0x7fffffff) + varint(1) + minimal bytes + overflow ptr=2.
		// Write a fresh cell at offset 3000 and point cell pointer 0 to 3000.
		cellPos := 3000
		var cell []byte
		cell = dbfile.AppendVarint(cell, 0x7fffffff) // max allowed payload size (~2 GiB)
		cell = dbfile.AppendVarint(cell, 1)          // rowid 1
		// Local size for 0x7fffffff on 4096 table leaf:
		loc := dbfile.LocalPayloadSize(4096, dbfile.TableLeaf, 0x7fffffff)
		cell = append(cell, make([]byte, loc)...)
		cell = append(cell, 0, 0, 0, 2)
		if cellPos+len(cell) > 4096 {
			cellPos = 4096 - len(cell)
		}
		binary.BigEndian.PutUint16(oomImg[4096+5:], uint16(cellPos))
		binary.BigEndian.PutUint16(oomImg[4096+8:], uint16(cellPos))
		copy(oomImg[4096+cellPos:], cell)
		if pan, err := exerciseAll(oomImg); pan != nil || err == nil {
			issues = append(issues, fmt.Sprintf("huge-payload-2GiB(panic=%v,err=%v)", pan, err))
		}
		_ = c0

		// 2. Record decoder edge cases.
		if _, err := dbfile.DecodeRecord([]byte{0x02, 10}); err == nil {
			issues = append(issues, "serial-type-10-accepted")
		}
		if _, err := dbfile.DecodeRecord([]byte{0x02, 11}); err == nil {
			issues = append(issues, "serial-type-11-accepted")
		}
		hugeRec := dbfile.AppendVarint(nil, 10)
		hugeRec = dbfile.AppendVarint(hugeRec, 2*dbfile.MaxPayloadSize+100)
		if _, err := dbfile.DecodeRecord(hugeRec); err == nil {
			issues = append(issues, "huge-serial-type-accepted")
		}
		// IEEE 754 NaN float (serial type 7) must decode as NULL per sqlite3VdbeSerialGet.
		nanRec := []byte{0x02, 0x07, 0x7f, 0xf8, 0, 0, 0, 0, 0, 0}
		vals, err := dbfile.DecodeRecord(nanRec)
		if err != nil || len(vals) != 1 || vals[0].Class != dbfile.Null || math.IsNaN(vals[0].Float) {
			issues = append(issues, fmt.Sprintf("NaN-decode(vals=%+v,err=%v)", vals, err))
		}
		// Odd-length UTF-16 must return an error.
		if _, err := dbfile.DecodeText([]byte{0x41, 0x00, 0x42}, dbfile.TextEncodingUTF16L); err == nil {
			issues = append(issues, "odd-utf16-accepted")
		}

		report("COR-05a",
			"anti-OOM 2 GiB payload varint on tiny file + reserved serial types (10,11) + huge serial type + NaN->NULL + odd UTF-16",
			"huge payload rejected without OOM; invalid records rejected; NaN decodes as NULL; odd UTF-16 rejected",
			fmt.Sprintf("issues=%d (%v)", len(issues), issues),
			len(issues) == 0)
	}

	// COR-05b: Overlapping cell pointers (two cell pointers referencing the exact same cell offset).
	{
		p := filepath.Join(scratch, "overlap-cells.db")
		if err := newDB(ctx, p, nil,
			"CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT)",
			"INSERT INTO t VALUES(1, 'first-row'), (2, 'second-row')",
		); err != nil {
			fatal("build overlap", err)
		}
		img, _ := os.ReadFile(p)
		// Copy cell pointer 0 into cell pointer 1 so both point to the exact same bytes on page 2.
		img[4096+10] = img[4096+8]
		img[4096+11] = img[4096+9]
		if err := os.WriteFile(p, img, 0o644); err != nil {
			fatal("write overlap", err)
		}
		pan, dbErr := exerciseAll(img)
		sqlIC := sqliteIntegrity(ctx, p)
		held := pan == nil && dbErr != nil
		report("COR-05b",
			"duplicate cell pointer on page 2 (two cells overlapping the exact same byte range) vs SQLite integrity_check",
			"inspector reports an error on overlapping cells just as SQLite integrity_check does",
			fmt.Sprintf("dbfile_err=%v, panic=%v, sqlite_ic=%q", dbErr, pan, sqlIC),
			held)
	}

	// COR-06: 10,000-iteration deterministic seeded mutation storm on a multi-feature DB.
	// Scaled to 10,000 iterations so the group runs in a few seconds on 4 cores.
	{
		stormBasePath := filepath.Join(scratch, "storm-base.db")
		if err := newDB(ctx, stormBasePath, func(o *sqlitex.Options) {
			o.File.PageSize = 512
			o.File.AutoVacuum = sqlitex.AutoVacuumFull
		},
			"CREATE TABLE t(id INTEGER PRIMARY KEY, k TEXT, b BLOB)",
			"CREATE INDEX idx_k ON t(k)",
			"INSERT INTO t VALUES(1, 'a', zeroblob(1200))",
			"INSERT INTO t VALUES(2, 'b', zeroblob(600))",
			"INSERT INTO t VALUES(3, 'c', zeroblob(1200))",
			"DELETE FROM t WHERE id = 2",
		); err != nil {
			fatal("build storm-base", err)
		}
		base, _ := os.ReadFile(stormBasePath)
		rng := rand.New(rand.NewPCG(20261120, 2))
		const iters = 10000
		buf := make([]byte, len(base))
		panics := 0
		rejected := 0
		var firstPanic string
		for i := range iters {
			copy(buf, base)
			mode := rng.IntN(4)
			var sample []byte
			switch mode {
			case 0: // 1-bit flip in any page header or DB header
				pg := rng.IntN(len(base) / 512)
				off := pg*512 + rng.IntN(min(112, 512))
				if off < len(buf) {
					buf[off] ^= 1 << uint(rng.IntN(8))
				}
				sample = buf
			case 1: // 1..8 random byte overwrites anywhere
				k := 1 + rng.IntN(8)
				for range k {
					buf[rng.IntN(len(buf))] = byte(rng.IntN(256))
				}
				sample = buf
			case 2: // arbitrary truncation
				cut := rng.IntN(len(buf))
				sample = buf[:cut]
			case 3: // 4-byte aligned integer smash (page numbers / counts / offsets)
				off := (rng.IntN(len(buf) / 4)) * 4
				binary.BigEndian.PutUint32(buf[off:], rng.Uint32())
				sample = buf
			}
			p, err := exerciseAll(sample)
			if p != nil {
				panics++
				if firstPanic == "" {
					firstPanic = fmt.Sprintf("iter=%d mode=%d panic=%v", i, mode, p)
				}
			} else if err != nil {
				rejected++
			}
		}
		report("COR-06",
			fmt.Sprintf("%d-iteration seeded mutation storm (header bit-flips, multi-byte smashes, truncations, u32 smashes) on auto_vacuum+overflow+index DB", iters),
			"0 panics across all 10,000 corrupted inputs",
			fmt.Sprintf("iters=%d, panics=%d (%s), rejected=%d, accepted=%d", iters, panics, firstPanic, rejected, iters-rejected-panics),
			panics == 0)
	}
}
