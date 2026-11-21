package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickstrad/sqlite-internals/internal/equal"
)

// TestDumpReplaysOddValuesAndNames verifies R2 dump/replay/VACUUM INTO on embedded NULs, ? identifiers, IPK rowid, and creation order.
func TestDumpReplaysOddValuesAndNames(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	source := newDB(ctx, filepath.Join(dir, "source.db"),
		`CREATE TABLE zebra(id INTEGER PRIMARY KEY, v TEXT)`,
		`CREATE TABLE "t?"("?" TEXT PRIMARY KEY) WITHOUT ROWID`,
		`CREATE TABLE "pair"("?, ?" TEXT, "x" TEXT)`,
		`CREATE TABLE ipk(rowid INTEGER PRIMARY KEY, _rowid_ TEXT, oid TEXT)`,
		`CREATE TABLE alpha(id INTEGER PRIMARY KEY, v TEXT)`,
		`INSERT INTO zebra VALUES(1, 'head' || char(0) || 'tail'), (2, char(0)), (3, 'it''s' || char(0)), (4, '')`,
		`INSERT INTO "t?" VALUES('val'), ('?')`,
		`INSERT INTO "pair" VALUES('?', '?, ?')`,
		`INSERT INTO ipk VALUES(7, 'a', 'b')`,
		`INSERT INTO alpha VALUES(1, 'x')`)
	script := dump(ctx, source)
	if !strings.Contains(script, `'head'||char(0)||'tail'`) {
		t.Fatal("embedded NUL not written as char(0)")
	}
	if strings.Index(script, "CREATE TABLE zebra") > strings.Index(script, "CREATE TABLE alpha") {
		t.Fatal("tables not in sqlite_schema order")
	}
	replayed := filepath.Join(dir, "replayed.db")
	replay(ctx, script, replayed)
	result, _, _, err := equal.CompareLogicalFiles(ctx, source, replayed)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Equal {
		t.Fatal(result)
	}
	if again := dump(ctx, replayed); again != script {
		t.Fatal("the replayed copy dumps differently")
	}
	canonicalSource, canonicalReplay := filepath.Join(dir, "source-vacuum.db"), filepath.Join(dir, "replayed-vacuum.db")
	vacuumInto(ctx, source, canonicalSource)
	vacuumInto(ctx, replayed, canonicalReplay)
	e1, err := equal.CompareFiles(canonicalSource, canonicalReplay, pageSize)
	if err != nil {
		t.Fatal(err)
	}
	if !e1.Equal {
		t.Fatal(e1)
	}
}

// TestSQLLiteralSplitsTextAtNUL checks quoting and char(0) splitting for strings with and without NUL bytes.
func TestSQLLiteralSplitsTextAtNUL(t *testing.T) {
	cases := map[string]string{
		"plain":        "'plain'",
		"it's":         "'it''s'",
		"\x00":         "''||char(0)||''",
		"a\x00b\x00'c": "'a'||char(0)||'b'||char(0)||'''c'",
	}
	for in, want := range cases {
		if got := sqlLiteral(in); got != want {
			t.Errorf("sqlLiteral(%q) = %s, want %s", in, got, want)
		}
	}
}
