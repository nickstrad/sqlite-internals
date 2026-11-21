package equal

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
)

func TestEncodeValueRefusesTypesWithoutAStorageClass(t *testing.T) {
	for _, v := range []any{int(1), true, int32(1), float32(1), struct{}{}} {
		if encoded, err := EncodeValue(v); err == nil || encoded != nil {
			t.Fatalf("%T: encoded %q, err %v", v, encoded, err)
		}
	}
}

func TestLogicalHashUsesAnIntegerPrimaryKeyNamedRowid(t *testing.T) {
	dir := t.TempDir()
	a := buildDB(t, dir, "a.db", "CREATE TABLE t(rowid INTEGER PRIMARY KEY, _rowid_ TEXT, oid TEXT)", "INSERT INTO t VALUES(5, 'x', 'y')")
	b := buildDB(t, dir, "b.db", "CREATE TABLE t(rowid INTEGER PRIMARY KEY, _rowid_ TEXT, oid TEXT)", "INSERT INTO t VALUES(6, 'x', 'y')")
	la, lb := logicalOf(t, a), logicalOf(t, b)
	if r := CompareLogical(la, lb); r.Equal || !strings.Contains(r.String(), "table t differs") {
		t.Fatal(r)
	}
	db, err := OpenExisting(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var info TableInfo
	var rows [][]any
	if err := WalkLogical(context.Background(), db, LogicalVisitor{Row: func(table TableInfo, values []any) error {
		info = table
		rows = append(rows, values)
		return nil
	}}); err != nil {
		t.Fatal(err)
	}
	if info.RowidColumn != "rowid" || len(rows) != 1 || rows[0][0] != int64(5) || rows[0][1] != int64(5) {
		t.Fatal(info, rows)
	}
	for _, schema := range []string{
		"CREATE TABLE t(rowid INTEGER PRIMARY KEY DESC, _rowid_, oid)",
		"CREATE TABLE t(rowid INT PRIMARY KEY, _rowid_, oid)",
		"CREATE TABLE t(rowid INTEGER, _rowid_, oid, PRIMARY KEY(rowid, oid))",
	} {
		c := buildDB(t, t.TempDir(), "c.db", schema)
		if _, err := LogicalHashFile(context.Background(), c); err == nil || !strings.Contains(err.Error(), "hide the rowid") {
			t.Fatalf("%s: %v", schema, err)
		}
	}
}

func TestLogicalHashReadsMainWhenTempShadowsIt(t *testing.T) {
	dir := t.TempDir()
	a := buildDB(t, dir, "a.db", "CREATE TABLE t(a INTEGER)", "INSERT INTO t VALUES(1), (2)")
	want := logicalOf(t, a)
	ctx := context.Background()
	db, err := OpenExisting(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	for _, s := range []string{"CREATE TEMP TABLE t(x TEXT, y TEXT)", "INSERT INTO temp.t VALUES('p', 'q')", "CREATE TEMP TABLE only_temp(z)"} {
		if _, err := conn.ExecContext(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	got, err := LogicalHash(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	if r := CompareLogical(want, got); !r.Equal || got.SHA256 != want.SHA256 {
		t.Fatal(r)
	}
}

type doneAfter struct {
	context.Context
	calls  int
	limit  int
	closed chan struct{}
}

func newDoneAfter(limit int) *doneAfter {
	return &doneAfter{Context: context.Background(), limit: limit, closed: make(chan struct{})}
}

func (c *doneAfter) Done() <-chan struct{} {
	c.calls++
	if c.calls == c.limit {
		close(c.closed)
	}
	return c.closed
}

func (c *doneAfter) Err() error {
	select {
	case <-c.closed:
		return context.Canceled
	default:
		return nil
	}
}

func TestLogicalHashEndsItsTransactionWhenTheContextEnds(t *testing.T) {
	dir := t.TempDir()
	a := buildDB(t, dir, "a.db", "CREATE TABLE t(id INTEGER PRIMARY KEY, v)", "CREATE TABLE u(id INTEGER PRIMARY KEY, v)",
		"WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 300) INSERT INTO t SELECT i, zeroblob(50) FROM n", "INSERT INTO u SELECT id, id FROM t")
	ctx := context.Background()
	db, err := OpenExisting(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	canceled := 0
	for limit := 1; limit <= 80; limit++ {
		conn, err := db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := LogicalHash(newDoneAfter(limit), conn); err != nil {
			canceled++
		}
		if _, err := conn.ExecContext(ctx, "BEGIN"); err != nil {
			t.Fatalf("limit %d: the read transaction was left open: %v", limit, err)
		}
		if _, err := conn.ExecContext(ctx, "ROLLBACK"); err != nil {
			t.Fatal(err)
		}
		conn.Close()
	}
	if canceled == 0 {
		t.Fatal("no run was canceled")
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "CREATE TABLE r(rowid, _rowid_, oid)"); err != nil {
		t.Fatal(err)
	}
	if _, err := LogicalHash(ctx, conn); err == nil {
		t.Fatal("want the hidden-rowid refusal")
	}
	if _, err := conn.ExecContext(ctx, "BEGIN"); err != nil {
		t.Fatalf("a failed walk left the read transaction open: %v", err)
	}
	if _, err := conn.ExecContext(ctx, "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
}

func TestLogicalHashSeesDefaultCacheSize(t *testing.T) {
	dir := t.TempDir()
	a := buildDB(t, dir, "a.db", "CREATE TABLE t(id INTEGER PRIMARY KEY)")
	b := buildDB(t, dir, "b.db", "CREATE TABLE t(id INTEGER PRIMARY KEY)", "PRAGMA default_cache_size = 2000")
	if r := CompareLogical(logicalOf(t, a), logicalOf(t, b)); r.Equal || !strings.Contains(r.String(), "pragma default_cache_size") {
		t.Fatal(r)
	}
}

func TestHashingALiveWALSnapshotChangesNoFile(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	source := buildDB(t, dir, "source.db", "CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT)", "INSERT INTO t VALUES(1, 'in the file')")
	snapshot := filepath.Join(dir, "snapshot.db")
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(snapshot, data, 0o644); err != nil {
		t.Fatal(err)
	}
	options := sqlitex.DefaultOptions()
	options.SingleConnection = true
	options.Conn.WALAutocheckpoint = 0
	db, err := sqlitex.Open(ctx, source, options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO t VALUES(2, 'only in the wal')"); err != nil {
		t.Fatal(err)
	}
	wal, err := os.ReadFile(source + "-wal")
	if err != nil || len(wal) == 0 {
		t.Fatal(len(wal), err)
	}
	if err := os.WriteFile(snapshot+"-wal", wal, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	live, err := HasLiveSideFile(snapshot)
	if err != nil || !live {
		t.Fatal(live, err)
	}
	dbBefore, _ := FileSHA256(snapshot)
	walBefore, _ := FileSHA256(snapshot + "-wal")
	unchanged := func(what string) {
		t.Helper()
		dbAfter, err := FileSHA256(snapshot)
		if err != nil || dbAfter != dbBefore {
			t.Fatalf("%s changed the database file: %v", what, err)
		}
		walAfter, err := FileSHA256(snapshot + "-wal")
		if err != nil || walAfter != walBefore {
			t.Fatalf("%s changed or removed the -wal: %v", what, err)
		}
	}
	got := logicalOf(t, snapshot)
	unchanged("LogicalHashFile")
	if table, ok := got.TableByName("t"); !ok || table.Rows != 2 {
		t.Fatal(got.Tables)
	}
	if r := CompareLogical(logicalOf(t, source), got); !r.Equal {
		t.Fatal(r)
	}
	if hashSnapshot, ok := dbhashOf(t, snapshot); ok {
		unchanged("RunDBHash")
		if hashSource, _ := dbhashOf(t, source); hashSource != hashSnapshot {
			t.Fatal("dbhash of the live snapshot differs from dbhash of the checkpointed source")
		}
	}
}
