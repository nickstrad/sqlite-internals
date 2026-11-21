package equal

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
)

func buildDB(t *testing.T, dir, name string, statements ...string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	ctx := context.Background()
	options := sqlitex.DefaultOptions()
	options.SingleConnection = true
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
	return path
}

func logicalOf(t *testing.T, path string) Logical {
	t.Helper()
	l, err := LogicalHashFile(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func dbhashOf(t *testing.T, path string) (string, bool) {
	t.Helper()
	tool, err := LocateDBHash()
	if err != nil {
		return "", false
	}
	h, err := RunDBHash(context.Background(), tool, path)
	if err != nil {
		t.Fatal(err)
	}
	return h, true
}

type blindSpot struct {
	name          string
	a, b          []string
	wantDiffer    string
	dbhashIsBlind bool
}

var blindSpots = []blindSpot{
	{
		name:          "implicit rowid",
		a:             []string{"CREATE TABLE t(v TEXT)", "INSERT INTO t(rowid, v) VALUES(1, 'x'), (2, 'y')"},
		b:             []string{"CREATE TABLE t(v TEXT)", "INSERT INTO t(rowid, v) VALUES(5, 'x'), (9, 'y')"},
		wantDiffer:    "table t differs",
		dbhashIsBlind: true,
	},
	{
		name:          "sqlite_sequence",
		a:             []string{"CREATE TABLE t(id INTEGER PRIMARY KEY AUTOINCREMENT, v TEXT)", "INSERT INTO t(v) VALUES('x')"},
		b:             []string{"CREATE TABLE t(id INTEGER PRIMARY KEY AUTOINCREMENT, v TEXT)", "INSERT INTO t(v) VALUES('x')", "INSERT INTO t(v) VALUES('y')", "DELETE FROM t WHERE id = 2"},
		wantDiffer:    "table sqlite_sequence differs",
		dbhashIsBlind: true,
	},
	{
		name:          "user_version",
		a:             []string{"CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT)", "INSERT INTO t VALUES(1, 'x')", "PRAGMA user_version = 1"},
		b:             []string{"CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT)", "INSERT INTO t VALUES(1, 'x')", "PRAGMA user_version = 2"},
		wantDiffer:    "pragma user_version 1 vs 2",
		dbhashIsBlind: true,
	},
	{
		name:          "integer 1 vs real 1.0",
		a:             []string{"CREATE TABLE t(id INTEGER PRIMARY KEY, v)", "INSERT INTO t VALUES(1, 1)"},
		b:             []string{"CREATE TABLE t(id INTEGER PRIMARY KEY, v)", "INSERT INTO t VALUES(1, 1.0)"},
		wantDiffer:    "table t differs",
		dbhashIsBlind: false,
	},
	{
		name:          "value boundary",
		a:             []string{"CREATE TABLE t(id INTEGER PRIMARY KEY, a TEXT, b TEXT)", "INSERT INTO t VALUES(1, 'a3', 'b')"},
		b:             []string{"CREATE TABLE t(id INTEGER PRIMARY KEY, a TEXT, b TEXT)", "INSERT INTO t VALUES(1, 'a', '3b')"},
		wantDiffer:    "table t differs",
		dbhashIsBlind: true,
	},
	{
		name:          "row moved between tables",
		a:             []string{"CREATE TABLE a(v TEXT)", "CREATE TABLE b(v TEXT)", "INSERT INTO a(rowid, v) VALUES(1, 'x'), (2, 'y')", "INSERT INTO b(rowid, v) VALUES(1, 'z')"},
		b:             []string{"CREATE TABLE a(v TEXT)", "CREATE TABLE b(v TEXT)", "INSERT INTO a(rowid, v) VALUES(1, 'x')", "INSERT INTO b(rowid, v) VALUES(1, 'y'), (2, 'z')"},
		wantDiffer:    "table a differs",
		dbhashIsBlind: true,
	},
}

func TestLogicalHashTellsApartTheBlindSpots(t *testing.T) {
	for _, spot := range blindSpots {
		t.Run(spot.name, func(t *testing.T) {
			dir := t.TempDir()
			a := buildDB(t, dir, "a.db", spot.a...)
			b := buildDB(t, dir, "b.db", spot.b...)
			result := CompareLogical(logicalOf(t, a), logicalOf(t, b))
			if result.Equal || !strings.Contains(result.String(), spot.wantDiffer) {
				t.Fatalf("want a difference naming %q, got %s", spot.wantDiffer, result)
			}
			again := CompareLogical(logicalOf(t, a), logicalOf(t, a))
			if !again.Equal {
				t.Fatalf("a database differs from itself: %s", again)
			}
			hashA, okA := dbhashOf(t, a)
			hashB, okB := dbhashOf(t, b)
			if !okA || !okB {
				t.Log("dbhash not available; the dbhash half of this case is skipped")
				return
			}
			if (hashA == hashB) != spot.dbhashIsBlind {
				t.Fatalf("dbhash equal=%v, want %v", hashA == hashB, spot.dbhashIsBlind)
			}
		})
	}
}

func TestLogicalHashIgnoresPhysicalHistory(t *testing.T) {
	dir := t.TempDir()
	schema := "CREATE TABLE t(id INTEGER PRIMARY KEY, v BLOB)"
	var forward, backward, history []string
	forward = append(forward, schema, "BEGIN")
	backward = append(backward, schema)
	history = append(history, schema)
	for i := 1; i <= 300; i++ {
		forward = append(forward, "INSERT INTO t VALUES("+strconv.Itoa(i)+", zeroblob(500))")
		backward = append(backward, "INSERT INTO t VALUES("+strconv.Itoa(301-i)+", zeroblob(500))")
		history = append(history, "INSERT INTO t VALUES("+strconv.Itoa(i)+", zeroblob(500))", "INSERT INTO t VALUES("+strconv.Itoa(1000+i)+", zeroblob(900))")
	}
	forward = append(forward, "COMMIT")
	history = append(history, "DELETE FROM t WHERE id > 1000")
	a := buildDB(t, dir, "forward.db", forward...)
	b := buildDB(t, dir, "backward.db", backward...)
	c := buildDB(t, dir, "history.db", history...)
	for _, other := range []string{b, c} {
		bytesResult, err := CompareFiles(a, other, 4096)
		if err != nil {
			t.Fatal(err)
		}
		if bytesResult.Equal {
			t.Fatalf("%s: expected different bytes", other)
		}
		result, la, lb, err := CompareLogicalFiles(context.Background(), a, other)
		if err != nil {
			t.Fatal(err)
		}
		if !result.Equal || la.SHA256 != lb.SHA256 {
			t.Fatalf("%s: %s", other, result)
		}
	}
}

func TestLogicalHashCoversWithoutRowidAndOddNames(t *testing.T) {
	dir := t.TempDir()
	a := buildDB(t, dir, "a.db",
		`CREATE TABLE "odd ""name"""(k TEXT PRIMARY KEY, v) WITHOUT ROWID`,
		`INSERT INTO "odd ""name""" VALUES('b', 2), ('a', 1)`,
		`CREATE TABLE r(rowid TEXT, _rowid_ TEXT, v)`,
		`INSERT INTO r(oid, rowid, _rowid_, v) VALUES(7, 'x', 'y', 1)`)
	b := buildDB(t, dir, "b.db",
		`CREATE TABLE "odd ""name"""(k TEXT PRIMARY KEY, v) WITHOUT ROWID`,
		`INSERT INTO "odd ""name""" VALUES('a', 1), ('b', 3)`,
		`CREATE TABLE r(rowid TEXT, _rowid_ TEXT, v)`,
		`INSERT INTO r(oid, rowid, _rowid_, v) VALUES(8, 'x', 'y', 1)`)
	la, lb := logicalOf(t, a), logicalOf(t, b)
	result := CompareLogical(la, lb)
	if result.Equal || !strings.Contains(result.String(), `table odd "name" differs`) || !strings.Contains(result.String(), "table r differs") {
		t.Fatal(result)
	}
	if table, ok := la.TableByName(`odd "name"`); !ok || table.Rows != 2 {
		t.Fatal(la.Tables)
	}
	c := buildDB(t, dir, "c.db", `CREATE TABLE r(rowid, _rowid_, oid)`)
	if _, err := LogicalHashFile(context.Background(), c); err == nil || !strings.Contains(err.Error(), "hide the rowid") {
		t.Fatal(err)
	}
}

func TestLogicalHashSeesSchemaPragmasIntegrityAndForeignKeys(t *testing.T) {
	dir := t.TempDir()
	base := []string{"CREATE TABLE p(id INTEGER PRIMARY KEY)", "CREATE TABLE c(id INTEGER PRIMARY KEY, p INTEGER REFERENCES p(id))", "INSERT INTO p VALUES(1)", "INSERT INTO c VALUES(1, 1)"}
	a := buildDB(t, dir, "a.db", base...)
	withIndex := buildDB(t, dir, "index.db", append(append([]string{}, base...), "CREATE INDEX c_p ON c(p)")...)
	result := CompareLogical(logicalOf(t, a), logicalOf(t, withIndex))
	if result.Equal || !strings.Contains(result.String(), "schema differs") {
		t.Fatal(result)
	}
	orphan := buildDB(t, dir, "orphan.db", append(append([]string{}, base...), "PRAGMA foreign_keys = OFF", "INSERT INTO c VALUES(2, 9)", "PRAGMA foreign_keys = ON")...)
	lo := logicalOf(t, orphan)
	if len(lo.ForeignKeyCheck) != 1 || lo.ForeignKeyCheck[0] != "c rowid=2 parent=p fkid=0" {
		t.Fatal(lo.ForeignKeyCheck)
	}
	orphanTwin := buildDB(t, dir, "orphan2.db", append(append([]string{}, base...), "PRAGMA foreign_keys = OFF", "INSERT INTO c VALUES(2, 9)", "PRAGMA foreign_keys = ON")...)
	if r := CompareLogical(lo, logicalOf(t, orphanTwin)); !r.Equal {
		t.Fatal(r)
	}
	la := logicalOf(t, a)
	if r := CompareLogical(la, lo); r.Equal || !strings.Contains(r.String(), "foreign_key_check changed (0 vs 1 rows)") {
		t.Fatal(r)
	}
	for _, name := range LogicalPragmas {
		if _, ok := la.Pragma(name); !ok {
			t.Fatalf("pragma %s missing", name)
		}
	}
	if v, _ := la.Pragma("journal_mode"); v != "wal" {
		t.Fatal(v)
	}
	if v, _ := la.Pragma("application_id"); v != "1397836120" {
		t.Fatal(v)
	}
	broken := la
	broken.Integrity = "*** in database main ***"
	if r := CompareLogical(la, broken); r.Equal || !strings.Contains(r.String(), "integrity_check of the second") {
		t.Fatal(r)
	}
}

func TestLogicalHashFileRefusesMissingFileAndLeavesFileUnchanged(t *testing.T) {
	dir := t.TempDir()
	if _, err := LogicalHashFile(context.Background(), filepath.Join(dir, "missing.db")); !errors.Is(err, ErrNoDatabase) {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "missing.db")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("LogicalHashFile created the file")
	}
	a := buildDB(t, dir, "a.db", "CREATE TABLE t(id INTEGER PRIMARY KEY, v)", "INSERT INTO t VALUES(1, 'x')")
	before, err := FileSHA256(a)
	if err != nil {
		t.Fatal(err)
	}
	logicalOf(t, a)
	after, err := FileSHA256(a)
	if err != nil || before != after {
		t.Fatal("hashing changed the file", err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(a + suffix); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s left behind", suffix)
		}
	}
}

func TestStorageClassesAndEncoding(t *testing.T) {
	dir := t.TempDir()
	a := buildDB(t, dir, "a.db", "CREATE TABLE t(id INTEGER PRIMARY KEY, d DATETIME, r REAL, b BLOB, n)",
		"INSERT INTO t VALUES(1, '2026-11-20 10:00:00', 1, x'00ff', NULL)",
		"INSERT INTO t VALUES(2, 5, -0.0, 'text', 9223372036854775807)")
	ctx := context.Background()
	db, err := OpenExisting(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var classes []string
	var rows [][]any
	var info TableInfo
	err = WalkLogical(ctx, db, LogicalVisitor{Row: func(table TableInfo, values []any) error {
		info = table
		rows = append(rows, values)
		for _, v := range values {
			class, err := StorageClassOf(v)
			if err != nil {
				return err
			}
			classes = append(classes, class)
		}
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	var typeofs []string
	for _, values := range rows {
		for i := range values {
			column := info.RowidColumn
			if i > 0 {
				column = info.Columns[i-1]
			}
			var typeOf string
			if err := db.QueryRowContext(ctx, "SELECT typeof("+QuoteIdentifier(column)+") FROM t WHERE rowid = ?", values[0]).Scan(&typeOf); err != nil {
				t.Fatal(err)
			}
			typeofs = append(typeofs, typeOf)
		}
	}
	if strings.Join(classes, ",") != strings.Join(typeofs, ",") {
		t.Fatalf("classes %v, typeof %v", classes, typeofs)
	}
	if strings.Join(classes, ",") != "integer,integer,text,real,blob,null,integer,integer,integer,real,text,integer" {
		t.Fatal(classes)
	}
	encode := func(v any) []byte {
		encoded, err := EncodeValue(v)
		if err != nil {
			t.Fatal(err)
		}
		return encoded
	}
	cases := map[string][]byte{
		"null":     encode(nil),
		"int 1":    encode(int64(1)),
		"real 1":   encode(float64(1)),
		"text 1":   encode("1"),
		"blob 1":   encode([]byte("1")),
		"real -0":  encode(math.Copysign(0, -1)),
		"real +0":  encode(float64(0)),
		"empty":    encode(""),
		"emptyblb": encode([]byte{}),
	}
	seen := map[string]string{}
	for name, encoded := range cases {
		if other, ok := seen[string(encoded)]; ok {
			t.Fatalf("%s and %s encode the same", name, other)
		}
		seen[string(encoded)] = name
	}
}
