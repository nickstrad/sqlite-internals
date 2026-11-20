package recipe

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"

	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
)

// Execer is the minimal subset of *sql.Tx, *sql.Conn, and *sql.DB needed to
// execute a Step's statements.
type Execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// TxBeginner is the subset of *sql.DB and *sql.Conn needed to wrap a Step in
// one transaction.
type TxBeginner interface {
	BeginTx(ctx context.Context, options *sql.TxOptions) (*sql.Tx, error)
}

// ExecIn runs every statement in s in order on ex without starting or
// committing a transaction. Callers use it when combining steps into a single
// transaction.
func (s Step) ExecIn(ctx context.Context, ex Execer) error {
	for i, statement := range s.Statements {
		if _, err := ex.ExecContext(ctx, statement.SQL, statement.Args...); err != nil {
			return fmt.Errorf("recipe step %d statement %d (%s): %w", s.Number, i, statement.SQL, err)
		}
	}
	return nil
}

// ApplyStep runs s as a single transaction on db, rolling back if any
// statement fails. With sqlitex's default _txlock=immediate, BeginTx acquires
// the write lock up front.
func ApplyStep(ctx context.Context, db TxBeginner, s Step) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("recipe step %d begin: %w", s.Number, err)
	}
	if err := s.ExecIn(ctx, tx); err != nil {
		tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("recipe step %d commit: %w", s.Number, err)
	}
	return nil
}

// ApplyStep generates step k of r and commits it as one transaction on db.
func (r Recipe) ApplyStep(ctx context.Context, db TxBeginner, k int) error {
	s, err := r.Step(k)
	if err != nil {
		return err
	}
	return ApplyStep(ctx, db, s)
}

// Open validates r and opens path with r's explicit SQLite options.
func (r Recipe) Open(ctx context.Context, path string) (*sqlitex.DB, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	return sqlitex.Open(ctx, path, r.SQLiteOptions())
}

// SideFileSuffixes lists the SQLite sidecar files that must be removed with a
// database or absent when the database is at rest.
var SideFileSuffixes = []string{"-wal", "-shm", "-journal"}

// RemoveDatabase deletes path and any SQLite sidecar files (-wal, -shm,
// -journal), ignoring files that do not exist.
func RemoveDatabase(path string) error {
	for _, suffix := range append([]string{""}, SideFileSuffixes...) {
		if err := os.Remove(path + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// CheckAtRest verifies that path exists and has no -wal, -shm, or -journal
// sidecar files alongside it. A closed WAL database leaves no sidecars once
// the last connection checkpoints and removes them on close.
func CheckAtRest(path string) error {
	if _, err := os.Stat(path); err != nil {
		return err
	}
	for _, suffix := range SideFileSuffixes {
		if _, err := os.Stat(path + suffix); err == nil {
			return fmt.Errorf("recipe: %s is not at rest: %s exists", path, path+suffix)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// ApplyInSession opens path, commits s in one transaction, closes the
// connection, and verifies the database is at rest.
//
// Opening a fresh connection per step makes the WAL file change counter
// (header bytes 24..27 and 92..95) advance by one per step: pagerWalFrames
// (src/pager.c:3264) writes dbFileVers+1 (src/pager.c:3135), and dbFileVers is
// refreshed only when page 1 is read from disk (src/pager.c:3109-3110).
func (r Recipe) ApplyInSession(ctx context.Context, path string, s Step) error {
	db, err := r.Open(ctx, path)
	if err != nil {
		return err
	}
	if err := ApplyStep(ctx, db, s); err != nil {
		db.Close()
		return err
	}
	if err := db.Close(); err != nil {
		return err
	}
	return CheckAtRest(path)
}

// Build creates a fresh database at path and applies steps 0..upTo using one
// connection session per step.
func (r Recipe) Build(ctx context.Context, path string, upTo int) error {
	return r.BuildEach(ctx, path, upTo, nil)
}

// BuildEach creates a fresh database at path, applies steps 0..upTo in
// separate sessions, and calls afterStep(k) after each step's file is closed
// and at rest.
func (r Recipe) BuildEach(ctx context.Context, path string, upTo int, afterStep func(k int) error) error {
	if upTo < 0 || upTo > r.Steps {
		return fmt.Errorf("recipe: build up to step %d out of range 0..%d", upTo, r.Steps)
	}
	plan, err := r.WithSteps(upTo).Plan()
	if err != nil {
		return err
	}
	if err := RemoveDatabase(path); err != nil {
		return err
	}
	for _, s := range plan {
		if err := r.ApplyInSession(ctx, path, s); err != nil {
			return err
		}
		if afterStep != nil {
			if err := afterStep(s.Number); err != nil {
				return err
			}
		}
	}
	return nil
}

// CurrentStep reports the last recipe step committed in q, or -1 if none of
// the three recipe tables exist yet. Because step 0 sets user_version = 0,
// checking sqlite_schema distinguishes an empty database from step 0.
func CurrentStep(ctx context.Context, q sqlitex.Querier) (int, error) {
	var tables int
	if err := q.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_schema WHERE type = 'table' AND name IN ('accounts', 'documents', 'events')").Scan(&tables); err != nil {
		return 0, err
	}
	if tables == 0 {
		return -1, nil
	}
	if tables != len(SchemaSQL) {
		return 0, fmt.Errorf("recipe: database has %d of the %d recipe tables", tables, len(SchemaSQL))
	}
	var version int
	if err := q.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return 0, err
	}
	return version, nil
}

// ApplyNext reads CurrentStep from db and commits the next step (current + 1),
// returning the step number just applied.
func (r Recipe) ApplyNext(ctx context.Context, db *sqlitex.DB) (int, error) {
	current, err := CurrentStep(ctx, db)
	if err != nil {
		return 0, err
	}
	next := current + 1
	if next > r.Steps {
		return current, fmt.Errorf("recipe: already at the last step %d", current)
	}
	return next, r.ApplyStep(ctx, db, next)
}
