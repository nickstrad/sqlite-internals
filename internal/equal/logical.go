package equal

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"math"
	"net/url"
	"os"
	"slices"
	"sort"
	"strings"

	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
)

const LogicalFormat = "sqlite-internals logical hash v1"

var LogicalPragmas = []string{"page_size", "encoding", "auto_vacuum", "user_version", "application_id", "journal_mode", "default_cache_size"}

type SchemaRow struct {
	Type  string
	Name  string
	Table string
	SQL   sql.NullString
}

type TableInfo struct {
	Name         string
	Kind         string
	WithoutRowid bool
	RowidColumn  string
	Columns      []string
}

type LogicalVisitor struct {
	Schema    func(SchemaRow) error
	TableDone func(TableInfo, int) error
	Table     func(TableInfo) error
	Row       func(TableInfo, []any) error
	Pragma    func(name, value string) error
}

func QuoteIdentifier(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

type RowsQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func WalkLogical(ctx context.Context, q RowsQuerier, v LogicalVisitor) error {
	if err := walkSchema(ctx, q, v); err != nil {
		return err
	}
	tables, err := logicalTables(ctx, q)
	if err != nil {
		return err
	}
	for _, t := range tables {
		if err := walkTable(ctx, q, t, v); err != nil {
			return err
		}
	}
	for _, name := range LogicalPragmas {
		value, err := pragmaValue(ctx, q, name)
		if err != nil {
			return err
		}
		if v.Pragma != nil {
			if err := v.Pragma(name, value); err != nil {
				return err
			}
		}
	}
	return nil
}

func walkSchema(ctx context.Context, q RowsQuerier, v LogicalVisitor) error {
	rows, err := q.QueryContext(ctx, "SELECT type, name, tbl_name, sql FROM main.sqlite_schema ORDER BY type, name")
	if err != nil {
		return fmt.Errorf("read sqlite_schema: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var r SchemaRow
		if err := rows.Scan(&r.Type, &r.Name, &r.Table, &r.SQL); err != nil {
			return fmt.Errorf("read sqlite_schema: %w", err)
		}
		if v.Schema != nil {
			if err := v.Schema(r); err != nil {
				return err
			}
		}
	}
	return rows.Err()
}

func logicalTables(ctx context.Context, q RowsQuerier) ([]TableInfo, error) {
	rows, err := q.QueryContext(ctx, "SELECT name, type, wr FROM pragma_table_list WHERE schema = 'main' AND type IN ('table', 'shadow') AND name NOT IN ('sqlite_schema', 'sqlite_master') ORDER BY name")
	if err != nil {
		return nil, fmt.Errorf("list tables: %w", err)
	}
	var tables []TableInfo
	for rows.Next() {
		var t TableInfo
		var withoutRowid int64
		if err := rows.Scan(&t.Name, &t.Kind, &withoutRowid); err != nil {
			rows.Close()
			return nil, fmt.Errorf("list tables: %w", err)
		}
		t.WithoutRowid = withoutRowid != 0
		tables = append(tables, t)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range tables {
		columns, err := tableColumns(ctx, q, tables[i].Name)
		if err != nil {
			return nil, err
		}
		tables[i].Columns = columns
		if !tables[i].WithoutRowid {
			alias, err := rowidAlias(ctx, q, tables[i].Name, columns)
			if err != nil {
				return nil, err
			}
			tables[i].RowidColumn = alias
		}
	}
	return tables, nil
}

func tableColumns(ctx context.Context, q RowsQuerier, table string) ([]string, error) {
	rows, err := q.QueryContext(ctx, "SELECT name FROM pragma_table_info(?, 'main') ORDER BY cid", table)
	if err != nil {
		return nil, fmt.Errorf("columns of %q: %w", table, err)
	}
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		columns = append(columns, name)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(columns) == 0 {
		return nil, fmt.Errorf("table %q has no columns", table)
	}
	return columns, nil
}

func rowidAlias(ctx context.Context, q RowsQuerier, table string, columns []string) (string, error) {
	used := map[string]bool{}
	for _, c := range columns {
		used[strings.ToLower(c)] = true
	}
	for _, alias := range []string{"rowid", "_rowid_", "oid"} {
		if !used[alias] {
			return alias, nil
		}
	}
	ipk, err := integerPrimaryKeyColumn(ctx, q, table)
	if err != nil {
		return "", err
	}
	if ipk != "" {
		return ipk, nil
	}
	return "", fmt.Errorf("table %q: columns named rowid, _rowid_ and oid hide the rowid", table)
}

func integerPrimaryKeyColumn(ctx context.Context, q RowsQuerier, table string) (string, error) {
	keys, err := primaryKeyColumns(ctx, q, table)
	if err != nil || len(keys) != 1 {
		return "", err
	}
	var declared string
	if err := q.QueryRowContext(ctx, "SELECT type FROM pragma_table_info(?, 'main') WHERE pk = 1", table).Scan(&declared); err != nil {
		return "", fmt.Errorf("primary key of %q: %w", table, err)
	}
	if !strings.EqualFold(declared, "INTEGER") {
		return "", nil
	}
	var pkIndexes int
	if err := q.QueryRowContext(ctx, "SELECT count(*) FROM pragma_index_list(?, 'main') WHERE origin = 'pk'", table).Scan(&pkIndexes); err != nil {
		return "", fmt.Errorf("indexes of %q: %w", table, err)
	}
	if pkIndexes > 0 {
		return "", nil
	}
	return keys[0], nil
}

func primaryKeyColumns(ctx context.Context, q RowsQuerier, table string) ([]string, error) {
	rows, err := q.QueryContext(ctx, "SELECT name FROM pragma_table_info(?, 'main') WHERE pk > 0 ORDER BY pk", table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

func TableSelect(ctx context.Context, q RowsQuerier, t TableInfo) (string, error) {
	var selected, order []string
	if t.WithoutRowid {
		keys, err := primaryKeyColumns(ctx, q, t.Name)
		if err != nil {
			return "", err
		}
		if len(keys) == 0 {
			return "", fmt.Errorf("WITHOUT ROWID table %q has no primary key columns", t.Name)
		}
		for _, k := range keys {
			order = append(order, QuoteIdentifier(k))
		}
	} else {
		selected = append(selected, QuoteIdentifier(t.RowidColumn))
		order = append(order, QuoteIdentifier(t.RowidColumn))
	}
	for _, c := range t.Columns {
		selected = append(selected, "+"+QuoteIdentifier(c))
	}
	return fmt.Sprintf("SELECT %s FROM main.%s ORDER BY %s", strings.Join(selected, ", "), QuoteIdentifier(t.Name), strings.Join(order, ", ")), nil
}

func walkTable(ctx context.Context, q RowsQuerier, t TableInfo, v LogicalVisitor) error {
	query, err := TableSelect(ctx, q, t)
	if err != nil {
		return err
	}
	if v.Table != nil {
		if err := v.Table(t); err != nil {
			return err
		}
	}
	rows, err := q.QueryContext(ctx, query)
	if err != nil {
		return fmt.Errorf("read table %q: %w", t.Name, err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		width := len(t.Columns)
		if !t.WithoutRowid {
			width++
		}
		values := make([]any, width)
		pointers := make([]any, width)
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			return fmt.Errorf("read table %q: %w", t.Name, err)
		}
		for i, value := range values {
			if _, err := StorageClassOf(value); err != nil {
				return fmt.Errorf("table %q row %d column %d: %w", t.Name, count+1, i, err)
			}
		}
		count++
		if v.Row != nil {
			if err := v.Row(t, values); err != nil {
				return err
			}
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read table %q: %w", t.Name, err)
	}
	if v.TableDone != nil {
		return v.TableDone(t, count)
	}
	return nil
}

func StorageClassOf(value any) (string, error) {
	switch value.(type) {
	case nil:
		return "null", nil
	case int64:
		return "integer", nil
	case float64:
		return "real", nil
	case string:
		return "text", nil
	case []byte:
		return "blob", nil
	}
	return "", fmt.Errorf("value of Go type %T has no SQLite storage class", value)
}

func pragmaValue(ctx context.Context, q RowsQuerier, name string) (string, error) {
	var value any
	if err := q.QueryRowContext(ctx, "PRAGMA main."+name).Scan(&value); err != nil {
		return "", fmt.Errorf("PRAGMA %s: %w", name, err)
	}
	text := fmt.Sprint(value)
	if b, ok := value.([]byte); ok {
		text = string(b)
	}
	switch name {
	case "journal_mode":
		if strings.EqualFold(text, "wal") {
			return "wal", nil
		}
		return "rollback", nil
	case "auto_vacuum":
		switch text {
		case "0":
			return "none", nil
		case "1":
			return "full", nil
		case "2":
			return "incremental", nil
		}
	}
	return text, nil
}

type LogicalTableDigest struct {
	Name   string
	Rows   int
	SHA256 string
}

type LogicalPragma struct {
	Name  string
	Value string
}

type Logical struct {
	SHA256          string
	SchemaSHA256    string
	SchemaRows      int
	Tables          []LogicalTableDigest
	Pragmas         []LogicalPragma
	Integrity       string
	ForeignKeyCheck []string
}

func (l Logical) TableByName(name string) (LogicalTableDigest, bool) {
	for _, t := range l.Tables {
		if t.Name == name {
			return t, true
		}
	}
	return LogicalTableDigest{}, false
}

func (l Logical) Pragma(name string) (string, bool) {
	for _, p := range l.Pragmas {
		if p.Name == name {
			return p.Value, true
		}
	}
	return "", false
}

func (l Logical) Summary() string {
	var tables []string
	for _, t := range l.Tables {
		tables = append(tables, fmt.Sprintf("%s=%d", t.Name, t.Rows))
	}
	var pragmas []string
	for _, p := range l.Pragmas {
		pragmas = append(pragmas, p.Name+"="+p.Value)
	}
	return fmt.Sprintf("logical sha256 %s; schema rows %d; table rows %s; %s; integrity_check %s; foreign_key_check %d row(s)", l.SHA256, l.SchemaRows, strings.Join(tables, " "), strings.Join(pragmas, " "), l.Integrity, len(l.ForeignKeyCheck))
}

type streamEncoder struct {
	w io.Writer
}

func (e streamEncoder) tag(t byte) {
	e.w.Write([]byte{t})
}

func (e streamEncoder) length(n int) {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(n))
	e.w.Write(b[:])
}

func (e streamEncoder) bytes(tag byte, b []byte) {
	e.tag(tag)
	e.length(len(b))
	e.w.Write(b)
}

func (e streamEncoder) text(tag byte, s string) { e.bytes(tag, []byte(s)) }

func (e streamEncoder) value(value any) error {
	switch x := value.(type) {
	case nil:
		e.tag('N')
	case int64:
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], uint64(x))
		e.tag('I')
		e.w.Write(b[:])
	case float64:
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], math.Float64bits(x))
		e.tag('R')
		e.w.Write(b[:])
	case string:
		e.text('T', x)
	case []byte:
		e.bytes('B', x)
	default:
		return fmt.Errorf("value of Go type %T has no SQLite storage class", value)
	}
	return nil
}

func EncodeValue(value any) []byte {
	var b bytes.Buffer
	e := streamEncoder{&b}
	if err := e.value(value); err != nil {
		b.Reset()
		e.text('U', fmt.Sprintf("%T", value))
		e.text('v', fmt.Sprintf("%#v", value))
	}
	return b.Bytes()
}

func LogicalHash(ctx context.Context, conn *sql.Conn) (Logical, error) {
	if _, err := conn.ExecContext(ctx, "BEGIN"); err != nil {
		return Logical{}, fmt.Errorf("begin read transaction: %w", err)
	}
	result, err := logicalHashInTx(ctx, conn)
	end := "COMMIT"
	if err != nil {
		end = "ROLLBACK"
	}
	if _, endErr := conn.ExecContext(context.WithoutCancel(ctx), end); endErr != nil && err == nil {
		err = fmt.Errorf("end read transaction: %w", endErr)
	}
	return result, err
}

func logicalHashInTx(ctx context.Context, conn *sql.Conn) (Logical, error) {
	var result Logical
	whole := sha256.New()
	all := streamEncoder{whole}
	all.text('V', LogicalFormat)
	schema := sha256.New()
	var table hash.Hash
	var tableEncoder streamEncoder
	visitor := LogicalVisitor{
		Schema: func(r SchemaRow) error {
			for _, e := range []streamEncoder{all, {schema}} {
				e.tag('S')
				e.text('t', r.Type)
				e.text('n', r.Name)
				e.text('b', r.Table)
				if r.SQL.Valid {
					e.text('q', r.SQL.String)
				} else {
					e.tag('N')
				}
			}
			result.SchemaRows++
			return nil
		},
		Table: func(t TableInfo) error {
			table = sha256.New()
			tableEncoder = streamEncoder{table}
			for _, e := range []streamEncoder{all, tableEncoder} {
				e.text('T', t.Name)
				if t.WithoutRowid {
					e.tag('w')
				} else {
					e.tag('r')
				}
				e.length(len(t.Columns))
				for _, c := range t.Columns {
					e.text('c', c)
				}
			}
			return nil
		},
		Row: func(t TableInfo, values []any) error {
			for _, e := range []streamEncoder{all, tableEncoder} {
				e.tag('R')
				e.length(len(values))
				for _, value := range values {
					if err := e.value(value); err != nil {
						return err
					}
				}
			}
			return nil
		},
		TableDone: func(t TableInfo, rows int) error {
			for _, e := range []streamEncoder{all, tableEncoder} {
				e.tag('E')
				e.length(rows)
			}
			result.Tables = append(result.Tables, LogicalTableDigest{Name: t.Name, Rows: rows, SHA256: hex.EncodeToString(table.Sum(nil))})
			return nil
		},
		Pragma: func(name, value string) error {
			all.tag('P')
			all.text('n', name)
			all.text('v', value)
			result.Pragmas = append(result.Pragmas, LogicalPragma{Name: name, Value: value})
			return nil
		},
	}
	if err := WalkLogical(ctx, conn, visitor); err != nil {
		return Logical{}, err
	}
	result.SHA256 = hex.EncodeToString(whole.Sum(nil))
	result.SchemaSHA256 = hex.EncodeToString(schema.Sum(nil))
	integrity, err := integrityCheck(ctx, conn)
	if err != nil {
		return Logical{}, err
	}
	result.Integrity = integrity
	fk, err := foreignKeyCheck(ctx, conn)
	if err != nil {
		return Logical{}, err
	}
	result.ForeignKeyCheck = fk
	return result, nil
}

func integrityCheck(ctx context.Context, q RowsQuerier) (string, error) {
	rows, err := q.QueryContext(ctx, "PRAGMA main.integrity_check")
	if err != nil {
		return "", fmt.Errorf("integrity_check: %w", err)
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return "", err
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, " / "), rows.Err()
}

func foreignKeyCheck(ctx context.Context, q RowsQuerier) ([]string, error) {
	rows, err := q.QueryContext(ctx, "PRAGMA main.foreign_key_check")
	if err != nil {
		return nil, fmt.Errorf("foreign_key_check: %w", err)
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var table, parent string
		var rowid sql.NullInt64
		var fkid int64
		if err := rows.Scan(&table, &rowid, &parent, &fkid); err != nil {
			return nil, err
		}
		row := "NULL"
		if rowid.Valid {
			row = fmt.Sprint(rowid.Int64)
		}
		lines = append(lines, fmt.Sprintf("%s rowid=%s parent=%s fkid=%d", table, row, parent, fkid))
	}
	sort.Strings(lines)
	return lines, rows.Err()
}

var ErrNoDatabase = errors.New("database file does not exist")

func OpenExisting(ctx context.Context, path string) (*sql.DB, error) {
	return openExisting(ctx, path, "rw")
}

func openForReading(ctx context.Context, path string) (*sql.DB, error) {
	busy, err := HasLiveSideFile(path)
	if err != nil {
		return nil, err
	}
	if busy {
		return openExisting(ctx, path, "ro")
	}
	return openExisting(ctx, path, "rw")
}

func HasLiveSideFile(path string) (bool, error) {
	for _, suffix := range []string{"-wal", "-journal"} {
		info, err := os.Stat(path + suffix)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return false, err
		}
		if info.Size() > 0 {
			return true, nil
		}
	}
	return false, nil
}

func openExisting(ctx context.Context, path, mode string) (*sql.DB, error) {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%s: %w", path, ErrNoDatabase)
		}
		return nil, err
	}
	if info.IsDir() {
		return nil, fmt.Errorf("%s is a directory", path)
	}
	uri, err := sqlitex.FileURI(path, url.Values{"mode": {mode}})
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", uri)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func LogicalHashFile(ctx context.Context, path string) (Logical, error) {
	db, err := openForReading(ctx, path)
	if err != nil {
		return Logical{}, err
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		db.Close()
		return Logical{}, err
	}
	result, err := LogicalHash(ctx, conn)
	if closeErr := conn.Close(); closeErr != nil && err == nil {
		err = closeErr
	}
	if closeErr := db.Close(); closeErr != nil && err == nil {
		err = closeErr
	}
	return result, err
}

type LogicalResult struct {
	Level       Level
	Equal       bool
	SHA256A     string
	SHA256B     string
	Differences []string
}

func (r LogicalResult) String() string {
	if r.Equal {
		return fmt.Sprintf("%s: equal (logical sha256 %s; integrity_check ok; foreign_key_check unchanged)", r.Level, r.SHA256A)
	}
	return fmt.Sprintf("%s: differ: %s", r.Level, strings.Join(r.Differences, "; "))
}

func CompareLogical(a, b Logical) LogicalResult {
	r := LogicalResult{Level: E3Logical, SHA256A: a.SHA256, SHA256B: b.SHA256}
	if a.SchemaSHA256 != b.SchemaSHA256 {
		r.Differences = append(r.Differences, fmt.Sprintf("schema differs (%d vs %d rows)", a.SchemaRows, b.SchemaRows))
	}
	names := map[string]bool{}
	for _, t := range a.Tables {
		names[t.Name] = true
	}
	for _, t := range b.Tables {
		names[t.Name] = true
	}
	var sorted []string
	for name := range names {
		sorted = append(sorted, name)
	}
	sort.Strings(sorted)
	for _, name := range sorted {
		ta, okA := a.TableByName(name)
		tb, okB := b.TableByName(name)
		switch {
		case !okA:
			r.Differences = append(r.Differences, fmt.Sprintf("table %s only in the second", name))
		case !okB:
			r.Differences = append(r.Differences, fmt.Sprintf("table %s only in the first", name))
		case ta.SHA256 != tb.SHA256:
			r.Differences = append(r.Differences, fmt.Sprintf("table %s differs (%d vs %d rows)", name, ta.Rows, tb.Rows))
		}
	}
	for _, p := range a.Pragmas {
		other, ok := b.Pragma(p.Name)
		if !ok || other != p.Value {
			r.Differences = append(r.Differences, fmt.Sprintf("pragma %s %s vs %s", p.Name, p.Value, other))
		}
	}
	for _, p := range b.Pragmas {
		if _, ok := a.Pragma(p.Name); !ok {
			r.Differences = append(r.Differences, fmt.Sprintf("pragma %s  vs %s", p.Name, p.Value))
		}
	}
	if a.SHA256 != b.SHA256 && len(r.Differences) == 0 {
		r.Differences = append(r.Differences, "logical streams differ")
	}
	if a.Integrity != "ok" {
		r.Differences = append(r.Differences, "integrity_check of the first: "+a.Integrity)
	}
	if b.Integrity != "ok" {
		r.Differences = append(r.Differences, "integrity_check of the second: "+b.Integrity)
	}
	if !slices.Equal(a.ForeignKeyCheck, b.ForeignKeyCheck) {
		r.Differences = append(r.Differences, fmt.Sprintf("foreign_key_check changed (%d vs %d rows)", len(a.ForeignKeyCheck), len(b.ForeignKeyCheck)))
	}
	r.Equal = len(r.Differences) == 0
	return r
}

func CompareLogicalFiles(ctx context.Context, a, b string) (LogicalResult, Logical, Logical, error) {
	la, err := LogicalHashFile(ctx, a)
	if err != nil {
		return LogicalResult{}, Logical{}, Logical{}, err
	}
	lb, err := LogicalHashFile(ctx, b)
	if err != nil {
		return LogicalResult{}, Logical{}, Logical{}, err
	}
	return CompareLogical(la, lb), la, lb, nil
}
