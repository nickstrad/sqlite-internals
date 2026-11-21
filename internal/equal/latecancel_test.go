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

// lateCancelConnector wraps modernc.org/sqlite to deterministically trigger
// a race that is microseconds wide in the real driver: a statement succeeds in
// SQLite, the context ends right afterward, and the driver returns ctx.Err().
type lateCancelConnector struct {
	dsn                    string
	successesReportedAsErr atomic.Int64
}

// Connect opens a real modernc.org/sqlite connection and wraps it in lateCancelConn.
func (c *lateCancelConnector) Connect(context.Context) (driver.Conn, error) {
	inner, err := (&sqlite.Driver{}).Open(c.dsn)
	if err != nil {
		return nil, err
	}
	return &lateCancelConn{Conn: inner, connector: c}, nil
}

// Driver returns the underlying modernc.org/sqlite driver.
func (c *lateCancelConnector) Driver() driver.Driver { return &sqlite.Driver{} }

// lateCancelConn forwards driver calls to modernc.org/sqlite and rewrites
// succeeded ExecContext calls to ctx.Err() when ctx has ended.
type lateCancelConn struct {
	driver.Conn
	connector *lateCancelConnector
}

// ExecContext runs the statement and, if it succeeded under a cancellable
// context that has now ended, returns ctx.Err() while keeping the SQLite effect.
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

// QueryContext delegates directly to the wrapped modernc.org/sqlite connection.
func (c *lateCancelConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
}

// PrepareContext delegates directly to the wrapped modernc.org/sqlite connection.
func (c *lateCancelConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	return c.Conn.(driver.ConnPrepareContext).PrepareContext(ctx, query)
}

// BeginTx delegates directly to the wrapped modernc.org/sqlite connection.
func (c *lateCancelConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	return c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
}

// ResetSession delegates directly to the wrapped modernc.org/sqlite connection.
func (c *lateCancelConn) ResetSession(ctx context.Context) error {
	return c.Conn.(driver.SessionResetter).ResetSession(ctx)
}

// IsValid delegates directly to the wrapped modernc.org/sqlite connection.
func (c *lateCancelConn) IsValid() bool {
	return c.Conn.(driver.Validator).IsValid()
}

// TestLogicalHashEndsItsTransactionWhenAStatementSucceedsButReportsCancellation confirms LogicalHash never leaks a read transaction when ExecContext reports ctx.Err() after a statement succeeds.
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
