package dbfile

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickstrad/sqlite-internals/internal/recipe"
	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
)

func newDatabase(t *testing.T, edit func(*sqlitex.Options), statements ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "t.db")
	options := sqlitex.DefaultOptions()
	options.SingleConnection = true
	options.Conn.SecureDelete = true
	if edit != nil {
		edit(&options)
	}
	ctx := context.Background()
	db, err := sqlitex.Open(ctx, path, options)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range statements {
		if _, err := db.ExecContext(ctx, s); err != nil {
			db.Close()
			t.Fatalf("%s: %v", s, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := recipe.CheckAtRest(path); err != nil {
		t.Fatal(err)
	}
	return path
}

func openParsed(t *testing.T, path string) *File {
	t.Helper()
	f, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func queryInt(t *testing.T, path, query string) int64 {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var v int64
	if err := db.QueryRow(query).Scan(&v); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return v
}

func TestVarintRoundTrip(t *testing.T) {
	cases := []struct {
		value  uint64
		length int
	}{
		{0, 1}, {0x7f, 1}, {0x80, 2}, {0x3fff, 2}, {0x4000, 3}, {1<<56 - 1, 8}, {1 << 56, 9}, {math.MaxUint64, 9}, {uint64(1) << 63, 9},
	}
	for _, c := range cases {
		encoded := AppendVarint(nil, c.value)
		if len(encoded) != c.length {
			t.Errorf("%d encodes to %d bytes, want %d", c.value, len(encoded), c.length)
		}
		got, n, err := Varint(append(encoded, 0xff, 0xff))
		if err != nil || got != c.value || n != c.length {
			t.Errorf("%d decoded as %d (%d bytes, %v)", c.value, got, n, err)
		}
		if _, _, err := Varint(encoded[:len(encoded)-1]); err == nil {
			t.Errorf("%d: truncated varint accepted", c.value)
		}
	}
	rng := rand.New(rand.NewPCG(7, 7))
	for range 2000 {
		v := rng.Uint64() >> rng.IntN(64)
		got, _, err := Varint(AppendVarint(nil, v))
		if err != nil || got != v {
			t.Fatalf("%d -> %d %v", v, got, err)
		}
	}
}

func TestHeaderMatchesPragmasAtEveryPageSize(t *testing.T) {
	for _, size := range []int{512, 1024, 4096, 65536} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			path := newDatabase(t, func(o *sqlitex.Options) { o.File.PageSize = size; o.File.UserVersion = 7 },
				"CREATE TABLE t(a INTEGER PRIMARY KEY, b)", "INSERT INTO t VALUES(1, 'x')")
			f := openParsed(t, path)
			h := f.Header
			if h.PageSize != size || f.PageCount() != int(queryInt(t, path, "PRAGMA page_count")) {
				t.Fatalf("page size %d count %d", h.PageSize, f.PageCount())
			}
			stored := binary.BigEndian.Uint16(h.Raw[16:18])
			if size == 65536 && stored != 1 {
				t.Fatalf("65536 stored as %d", stored)
			}
			if h.UserVersion != 7 || h.ApplicationID != sqlitex.CourseApplicationID || !h.WALMode() || h.SQLiteVersion != 3053004 {
				t.Fatalf("header %+v", h)
			}
			if h.SchemaCookie != uint32(queryInt(t, path, "PRAGMA schema_version")) {
				t.Fatal("schema cookie")
			}
			if f.PageCountSource() != PageCountFromHeader {
				t.Fatal(f.PageCountSource())
			}
		})
	}
}

func TestUTF16SchemaDecodes(t *testing.T) {
	for _, enc := range []string{"UTF-16le", "UTF-16be"} {
		path := newDatabase(t, func(o *sqlitex.Options) { o.File.Encoding = enc }, "CREATE TABLE ŝtuff(a INTEGER PRIMARY KEY, b TEXT)")
		f := openParsed(t, path)
		schema, err := f.Schema()
		if err != nil {
			t.Fatal(err)
		}
		if len(schema) != 1 || schema[0].Name != "ŝtuff" || !strings.HasPrefix(schema[0].SQL, "CREATE TABLE ŝtuff") {
			t.Fatalf("%s: %+v", enc, schema)
		}
	}
}

type dbstatRow struct {
	Name, Type                     string
	Cells, Payload, Unused, MaxPay int
}

func readDBStat(t *testing.T, path string) map[int]dbstatRow {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query("SELECT pageno, name, pagetype, ncell, payload, unused, mx_payload FROM dbstat")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[int]dbstatRow{}
	for rows.Next() {
		var n int
		var r dbstatRow
		if err := rows.Scan(&n, &r.Name, &r.Type, &r.Cells, &r.Payload, &r.Unused, &r.MaxPay); err != nil {
			t.Fatal(err)
		}
		out[n] = r
	}
	return out
}

func DBStatType(r Role) string {
	switch r {
	case RoleTableInterior, RoleIndexInterior:
		return "internal"
	case RoleTableLeaf, RoleIndexLeaf:
		return "leaf"
	case RoleOverflow:
		return "overflow"
	}
	return ""
}

func checkCensusAgainstSQLite(t *testing.T, path string) *Census {
	t.Helper()
	f := openParsed(t, path)
	c, err := f.Census()
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Complete(); err != nil {
		t.Fatal(err)
	}
	if c.PageCount != int(queryInt(t, path, "PRAGMA page_count")) {
		t.Fatal("page count")
	}
	if c.FreelistLength() != int(queryInt(t, path, "PRAGMA freelist_count")) {
		t.Fatalf("freelist %d", c.FreelistLength())
	}
	stat := readDBStat(t, path)
	btreePages := 0
	for _, p := range c.Pages {
		kind := DBStatType(p.Role)
		if kind == "" {
			if _, ok := stat[p.Number]; ok {
				t.Fatalf("page %d (%s) is in dbstat", p.Number, p.Role)
			}
			continue
		}
		btreePages++
		s, ok := stat[p.Number]
		got := dbstatRow{p.Tree, kind, p.CellCount, p.Payload, p.Unused, int(p.MaxPayload)}
		if !ok || s != got {
			t.Fatalf("page %d: ours %+v dbstat %+v", p.Number, got, s)
		}
	}
	if btreePages != len(stat) {
		t.Fatalf("dbstat has %d pages, census %d", len(stat), btreePages)
	}
	return c
}

func TestCensusMatchesSQLiteOnRecipe(t *testing.T) {
	for _, mode := range []string{"wal", "delete"} {
		path := filepath.Join(t.TempDir(), "r.db")
		r := recipe.Default().WithJournalMode(mode)
		if err := r.Build(context.Background(), path, r.Steps); err != nil {
			t.Fatal(err)
		}
		c := checkCensusAgainstSQLite(t, path)
		counts := c.RoleCounts()
		if counts[RoleOverflow] == 0 || counts[RoleFreelistLeaf]+counts[RoleFreelistTrunk] == 0 || counts[RoleIndexLeaf] == 0 {
			t.Fatalf("%s: recipe should have overflow, freelist and index pages: %v", mode, counts)
		}
	}
}

func TestRowsMatchSQLite(t *testing.T) {
	path := newDatabase(t, nil,
		"CREATE TABLE v(id INTEGER PRIMARY KEY, a, b, c)",
		"INSERT INTO v VALUES(1, NULL, 0, 1)",
		"INSERT INTO v VALUES(2, -1, 127, -128)",
		"INSERT INTO v VALUES(3, 32767, -32768, 8388607)",
		"INSERT INTO v VALUES(4, 2147483647, -2147483648, 140737488355327)",
		"INSERT INTO v VALUES(5, 9223372036854775807, -9223372036854775808, 1.5)",
		"INSERT INTO v VALUES(6, 'text', x'00ff', '')",
		"INSERT INTO v VALUES(7, zeroblob(10000), -0.0, 1e308)",
		"INSERT INTO v VALUES(-9, 'neg', 2, 3)")
	f := openParsed(t, path)
	schema, err := f.Schema()
	if err != nil {
		t.Fatal(err)
	}
	var ours []string
	err = f.WalkTable(schema[0].RootPage, func(r Row) error {
		parts := []string{fmt.Sprint(r.Rowid)}
		for _, v := range r.Values[1:] {
			parts = append(parts, v.Class.String()+":"+canonical(v))
		}
		ours = append(ours, strings.Join(parts, "|"))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	db, _ := sql.Open("sqlite", "file:"+path+"?mode=ro")
	defer db.Close()
	rows, err := db.Query("SELECT id, typeof(a), quote(a), typeof(b), quote(b), typeof(c), quote(c) FROM v ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var theirs []string
	for rows.Next() {
		var id int64
		cols := make([]string, 6)
		if err := rows.Scan(&id, &cols[0], &cols[1], &cols[2], &cols[3], &cols[4], &cols[5]); err != nil {
			t.Fatal(err)
		}
		theirs = append(theirs, fmt.Sprintf("%d|%s:%s|%s:%s|%s:%s", id, cols[0], cols[1], cols[2], cols[3], cols[4], cols[5]))
	}
	if strings.Join(ours, "\n") != strings.Join(theirs, "\n") {
		t.Fatalf("ours:\n%s\ntheirs:\n%s", strings.Join(ours, "\n"), strings.Join(theirs, "\n"))
	}
}

func canonical(v Value) string {
	switch v.Class {
	case Null:
		return "NULL"
	case Integer:
		return fmt.Sprint(v.Int)
	case Real:
		s := fmt.Sprintf("%.17g", v.Float)
		switch v.Float {
		case 1.5:
			s = "1.5"
		case 1e308:
			s = "1.0e+308"
		case 0:
			s = "0.0"
		}
		return s
	case Text:
		return "'" + string(v.Bytes) + "'"
	}
	return fmt.Sprintf("X'%X'", v.Bytes)
}

func TestLocalPayloadBoundary(t *testing.T) {
	maxLocal := MaxLocalPayload(4096, TableLeaf)
	if maxLocal != 4061 || MinLocalPayload(4096) != 489 || MaxLocalPayload(4096, IndexLeaf) != 1002 {
		t.Fatalf("limits %d %d %d", maxLocal, MinLocalPayload(4096), MaxLocalPayload(4096, IndexLeaf))
	}
	path := newDatabase(t, nil,
		"CREATE TABLE b(id INTEGER PRIMARY KEY, v BLOB)",
		"INSERT INTO b VALUES(1, zeroblob(4057))",
		"INSERT INTO b VALUES(2, zeroblob(4058))",
		"INSERT INTO b VALUES(3, zeroblob(4092 + 4057))")
	f := openParsed(t, path)
	sizes := map[int64][2]uint64{}
	err := f.WalkTable(2, func(r Row) error {
		sizes[r.Rowid] = [2]uint64{uint64(len(r.Values[1].Bytes)), uint64(len(r.OverflowPages))}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if sizes[1] != [2]uint64{4057, 0} || sizes[2] != [2]uint64{4058, 1} || sizes[3] != [2]uint64{8149, 1} {
		t.Fatalf("%v", sizes)
	}
	if LocalPayloadSize(4096, TableLeaf, 4061) != 4061 || LocalPayloadSize(4096, TableLeaf, 4062) != 489 || LocalPayloadSize(4096, TableLeaf, 8153) != 4061 {
		t.Fatal("local payload formula")
	}
	checkCensusAgainstSQLite(t, path)
}

func TestFreelistAcrossTrunks(t *testing.T) {
	path := newDatabase(t, func(o *sqlitex.Options) { o.File.PageSize = 1024 },
		"CREATE TABLE big(id INTEGER PRIMARY KEY, v BLOB)",
		"WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i < 600) INSERT INTO big SELECT i, zeroblob(1000) FROM n ORDER BY i",
		"DELETE FROM big")
	c := checkCensusAgainstSQLite(t, path)
	if len(c.FreelistTrunks) < 2 {
		t.Fatalf("trunks %v", c.FreelistTrunks)
	}
}

func TestAutoVacuumPointerMapPages(t *testing.T) {
	for _, mode := range []string{"FULL", "INCREMENTAL"} {
		path := newDatabase(t, func(o *sqlitex.Options) { o.File.AutoVacuum = mode; o.File.PageSize = 512 },
			"CREATE TABLE big(id INTEGER PRIMARY KEY, v BLOB)",
			"WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i < 300) INSERT INTO big SELECT i, zeroblob(400) FROM n ORDER BY i")
		f := openParsed(t, path)
		if f.Header.AutoVacuumName() != mode {
			t.Fatal(f.Header.AutoVacuumName())
		}
		c := checkCensusAgainstSQLite(t, path)
		perMap := 512/5 + 1
		if c.Page(2).Role != RolePointerMap || c.Page(2+perMap).Role != RolePointerMap || c.RoleCounts()[RolePointerMap] < 2 {
			t.Fatalf("pointer map pages: %v", c.RoleCounts())
		}
	}
}

func buildSparseLockByteFile(t *testing.T) (string, int) {
	t.Helper()
	path := newDatabase(t, func(o *sqlitex.Options) { o.File.PageSize = 65536; o.File.JournalMode = "delete" },
		"CREATE TABLE t(a INTEGER PRIMARY KEY, b)", "INSERT INTO t VALUES(1, 'x')")
	image, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	const pageSize = 65536
	lock := LockByteOffset/pageSize + 1
	pages := lock + 1
	trunk := 3
	var leaves []int
	for n := 4; n <= pages; n++ {
		if n != lock {
			leaves = append(leaves, n)
		}
	}
	binary.BigEndian.PutUint32(image[28:], uint32(pages))
	binary.BigEndian.PutUint32(image[32:], uint32(trunk))
	binary.BigEndian.PutUint32(image[36:], uint32(1+len(leaves)))
	trunkPage := make([]byte, pageSize)
	binary.BigEndian.PutUint32(trunkPage[4:], uint32(len(leaves)))
	for i, leaf := range leaves {
		binary.BigEndian.PutUint32(trunkPage[8+4*i:], uint32(leaf))
	}
	out, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	if _, err := out.Write(image[:2*pageSize]); err != nil {
		t.Fatal(err)
	}
	if _, err := out.WriteAt(trunkPage, int64(trunk-1)*pageSize); err != nil {
		t.Fatal(err)
	}
	if err := out.Truncate(int64(pages) * pageSize); err != nil {
		t.Fatal(err)
	}
	return path, lock
}

func TestLockBytePageInLargeSparseFile(t *testing.T) {
	path, lock := buildSparseLockByteFile(t)
	f := openParsed(t, path)
	if f.FileSize() <= LockByteOffset || f.LockBytePage() != lock {
		t.Fatalf("size %d lock %d", f.FileSize(), f.LockBytePage())
	}
	c, err := f.Census()
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Complete(); err != nil {
		t.Fatal(err)
	}
	if c.Page(lock).Role != RoleLockByte || c.FreelistLength() != int(queryInt(t, path, "PRAGMA freelist_count")) {
		t.Fatal("lock byte page or freelist")
	}
	db, _ := sql.Open("sqlite", "file:"+path+"?mode=ro")
	defer db.Close()
	var result string
	if err := db.QueryRow("PRAGMA integrity_check").Scan(&result); err != nil || result != "ok" {
		t.Fatalf("integrity_check %q %v", result, err)
	}
}

func TestRejectsMalformedFiles(t *testing.T) {
	path := newDatabase(t, nil, "CREATE TABLE t(a INTEGER PRIMARY KEY, b)", "INSERT INTO t VALUES(1, 'x')")
	good, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	mutate := func(edit func([]byte) []byte) []byte {
		return edit(append([]byte(nil), good...))
	}
	cases := []struct {
		name  string
		image []byte
		want  error
	}{
		{"empty", nil, ErrTruncated},
		{"short header", good[:50], ErrTruncated},
		{"bad magic", mutate(func(b []byte) []byte { b[0] = 'X'; return b }), ErrNotADatabase},
		{"page size 3000", mutate(func(b []byte) []byte { binary.BigEndian.PutUint16(b[16:], 3000); return b }), ErrNotADatabase},
		{"page size 256", mutate(func(b []byte) []byte { binary.BigEndian.PutUint16(b[16:], 256); return b }), ErrNotADatabase},
		{"read version 3", mutate(func(b []byte) []byte { b[19] = 3; return b }), ErrNotADatabase},
		{"fractions", mutate(func(b []byte) []byte { b[21] = 65; return b }), ErrNotADatabase},
		{"reserved too large", mutate(func(b []byte) []byte { b[20] = 255; binary.BigEndian.PutUint16(b[16:], 512); return b[:1024] }), ErrNotADatabase},
		{"partial page", good[:len(good)-1], ErrTruncated},
		{"header size beyond file", mutate(func(b []byte) []byte { binary.BigEndian.PutUint32(b[28:], 99); return b }), ErrCorrupt},
	}
	for _, c := range cases {
		_, err := FromBytes(c.image)
		if !errors.Is(err, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, err, c.want)
		}
	}
	disagree := mutate(func(b []byte) []byte { binary.BigEndian.PutUint32(b[28:], 99); b[95]++; return b })
	f, err := FromBytes(disagree)
	if err != nil || f.PageCount() != len(good)/4096 || f.PageCountSource() != PageCountFromFileSize {
		t.Fatalf("disagreeing counters: %v %d", err, f.PageCount())
	}
	badType := mutate(func(b []byte) []byte { b[4096] = 0x07; return b })
	f, _ = FromBytes(badType)
	if _, err := f.Census(); !errors.Is(err, ErrCorrupt) || !strings.Contains(err.Error(), "page 2") {
		t.Fatalf("bad page type: %v", err)
	}
	loop := mutate(func(b []byte) []byte {
		binary.BigEndian.PutUint32(b[32:], 2)
		binary.BigEndian.PutUint32(b[36:], 1)
		return b
	})
	f, _ = FromBytes(loop)
	if _, err := f.Census(); !errors.Is(err, ErrCorrupt) || !strings.Contains(err.Error(), "claimed twice") {
		t.Fatalf("freelist over a table page: %v", err)
	}
}

func censusEverything(f *File) error {
	c, err := f.Census()
	if err != nil {
		return err
	}
	for _, p := range c.Pages {
		if p.Role == RoleTableLeaf || p.Role == RoleIndexLeaf || p.Role == RoleIndexInterior {
			page, err := f.BTreePage(p.Number)
			if err != nil {
				return err
			}
			cells, err := page.Cells()
			if err != nil {
				return err
			}
			for _, cell := range cells {
				payload, _, err := f.Payload(p.Number, cell)
				if err != nil {
					return err
				}
				if _, err := DecodeRecord(payload); err != nil {
					return err
				}
			}
		}
	}
	return c.Complete()
}

func parseNeverPanics(t *testing.T, image []byte, label string) (err error) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("%s: panic: %v", label, r)
		}
	}()
	f, err := FromBytes(image)
	if err != nil {
		return err
	}
	return censusEverything(f)
}

func TestFlippedBytesReturnErrorsNotPanics(t *testing.T) {
	path := filepath.Join(t.TempDir(), "r.db")
	r := recipe.Default().WithSteps(6)
	if err := r.Build(context.Background(), path, 6); err != nil {
		t.Fatal(err)
	}
	good, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := parseNeverPanics(t, good, "original"); err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewPCG(2, 2))
	rejected := 0
	runs := 3000
	if testing.Short() {
		runs = 300
	}
	for i := range runs {
		image := append([]byte(nil), good...)
		var offset int
		if i%2 == 0 {
			page := rng.IntN(len(good) / 4096)
			offset = page*4096 + rng.IntN(64)
			if page == 0 {
				offset = rng.IntN(200)
			}
		} else {
			offset = rng.IntN(len(good))
		}
		image[offset] ^= byte(1 << rng.IntN(8))
		if parseNeverPanics(t, image, fmt.Sprintf("flip at %d", offset)) != nil {
			rejected++
		}
	}
	for _, cut := range []int{0, 1, 99, 100, 4095, 4097, len(good) - 4096, len(good) - 1} {
		parseNeverPanics(t, good[:cut], fmt.Sprintf("cut at %d", cut))
	}
	if rejected == 0 {
		t.Fatal("no flipped byte was rejected")
	}
	t.Logf("%d of %d flipped files rejected", rejected, runs)
}

func FuzzParse(f *testing.F) {
	dir := f.TempDir()
	path := filepath.Join(dir, "r.db")
	if err := recipe.Default().WithSteps(2).Build(context.Background(), path, 2); err != nil {
		f.Fatal(err)
	}
	good, err := os.ReadFile(path)
	if err != nil {
		f.Fatal(err)
	}
	f.Add(good, 0, byte(0))
	f.Add(good, 100, byte(0x10))
	f.Add(good, 4096+3, byte(0xff))
	f.Fuzz(func(t *testing.T, image []byte, offset int, mask byte) {
		image = append([]byte(nil), image...)
		if len(image) > 0 {
			offset = ((offset % len(image)) + len(image)) % len(image)
			image[offset] ^= mask
		}
		parseNeverPanics(t, image, "fuzz")
	})
}

func TestHeaderFieldsCoverEveryByte(t *testing.T) {
	covered := make([]int, HeaderSize)
	for _, field := range (Header{}).Fields() {
		for i := field.Offset; i < field.Offset+field.Size; i++ {
			covered[i]++
		}
	}
	for i, n := range covered {
		if n != 1 {
			t.Fatalf("byte %d covered %d times", i, n)
		}
	}
	if f, ok := HeaderFieldAt(27); !ok || f.Name != "file change counter" {
		t.Fatal(f)
	}
	_ = sha256.Size
}

func TestDecodeValueEdgeCases(t *testing.T) {
	nan := make([]byte, 8)
	binary.BigEndian.PutUint64(nan, math.Float64bits(math.NaN()))
	if v, err := DecodeValue(7, nan); err != nil || v.Class != Null || v.SerialType != 7 {
		t.Fatalf("NaN: %+v %v", v, err)
	}
	for _, reserved := range []uint64{10, 11} {
		if _, err := DecodeValue(reserved, nil); err == nil {
			t.Fatalf("serial type %d accepted", reserved)
		}
	}
	cases := []struct {
		t    uint64
		body []byte
		want int64
	}{
		{1, []byte{0xff}, -1}, {2, []byte{0x80, 0x00}, -32768}, {3, []byte{0x7f, 0xff, 0xff}, 8388607},
		{5, []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xfe}, -2}, {8, nil, 0}, {9, nil, 1},
	}
	for _, c := range cases {
		v, err := DecodeValue(c.t, c.body)
		if err != nil || v.Class != Integer || v.Int != c.want {
			t.Fatalf("type %d: %+v %v", c.t, v, err)
		}
	}
	if _, err := DecodeRecord([]byte{0x03, 0x01, 0x01}); err == nil {
		t.Fatal("record body overrun accepted")
	}
	if _, err := DecodeRecord([]byte{0x09, 0x00}); err == nil {
		t.Fatal("record header larger than payload accepted")
	}
}
