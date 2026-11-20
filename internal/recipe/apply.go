package recipe

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"

	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
)

type Execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

type TxBeginner interface {
	BeginTx(ctx context.Context, options *sql.TxOptions) (*sql.Tx, error)
}

func (s Step) ExecIn(ctx context.Context, ex Execer) error {
	for i, statement := range s.Statements {
		if _, err := ex.ExecContext(ctx, statement.SQL, statement.Args...); err != nil {
			return fmt.Errorf("recipe step %d statement %d (%s): %w", s.Number, i, statement.SQL, err)
		}
	}
	return nil
}

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

func (r Recipe) ApplyStep(ctx context.Context, db TxBeginner, k int) error {
	s, err := r.Step(k)
	if err != nil {
		return err
	}
	return ApplyStep(ctx, db, s)
}

func (r Recipe) Open(ctx context.Context, path string) (*sqlitex.DB, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	return sqlitex.Open(ctx, path, r.SQLiteOptions())
}

var SideFileSuffixes = []string{"-wal", "-shm", "-journal"}

func RemoveDatabase(path string) error {
	for _, suffix := range append([]string{""}, SideFileSuffixes...) {
		if err := os.Remove(path + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

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

func (r Recipe) Build(ctx context.Context, path string, upTo int) error {
	return r.BuildEach(ctx, path, upTo, nil)
}

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
