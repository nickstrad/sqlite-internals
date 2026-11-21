package equal

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"path/filepath"
	"sync/atomic"
	"testing"

	"modernc.org/sqlite"
)

type lateCancelConnector struct {
	dsn                    string
	successesReportedAsErr atomic.Int64
}

func (c *lateCancelConnector) Connect(context.Context) (driver.Conn, error) {
	inner, err := (&sqlite.Driver{}).Open(c.dsn)
	if err != nil {
		return nil, err
	}
	return &lateCancelConn{Conn: inner, connector: c}, nil
}

func (c *lateCancelConnector) Driver() driver.Driver { return &sqlite.Driver{} }

type lateCancelConn struct {
	driver.Conn
	connector *lateCancelConnector
}

func (c *lateCancelConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	result, err := c.Conn.(driver.ExecerContext).ExecContext(ctx, query, args)
	if err != nil || ctx.Done() == nil {
		return result, err
	}
	select {
	case <-ctx.Done():
		c.connector.successesReportedAsErr.Add(1)
		return nil, ctx.Err()
	default:
		return result, nil
	}
}

func (c *lateCancelConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
}

func (c *lateCancelConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	return c.Conn.(driver.ConnPrepareContext).PrepareContext(ctx, query)
}

func (c *lateCancelConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	return c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
}

func (c *lateCancelConn) ResetSession(ctx context.Context) error {
	return c.Conn.(driver.SessionResetter).ResetSession(ctx)
}

func (c *lateCancelConn) IsValid() bool {
	return c.Conn.(driver.Validator).IsValid()
}

func TestLogicalHashEndsItsTransactionWhenAStatementSucceedsButReportsCancellation(t *testing.T) {
	dir := t.TempDir()
	path := buildDB(t, dir, "a.db", "CREATE TABLE t(id INTEGER PRIMARY KEY, v)", "INSERT INTO t VALUES (1, 'x'), (2, 'y')")
	connector := &lateCancelConnector{dsn: "file:" + filepath.ToSlash(path)}
	db := sql.OpenDB(connector)
	defer db.Close()
	ctx := context.Background()
	canceled := 0
	for limit := 1; limit <= 40; limit++ {
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
	if _, err := LogicalHash(ctx, conn); err != nil {
		t.Fatal(err)
	}
	reported := 0
	for limit := 1; limit <= 4; limit++ {
		before := connector.successesReportedAsErr.Load()
		_, beginErr := conn.ExecContext(newDoneAfter(limit), "BEGIN")
		_, rollbackErr := conn.ExecContext(ctx, "ROLLBACK")
		if connector.successesReportedAsErr.Load() > before {
			reported++
			if beginErr == nil || rollbackErr != nil {
				t.Fatalf("limit %d: BEGIN %v, ROLLBACK %v; want a canceled BEGIN with its transaction open", limit, beginErr, rollbackErr)
			}
		}
	}
	if reported == 0 {
		t.Fatal("the connector never reported a successful BEGIN as canceled")
	}
}
