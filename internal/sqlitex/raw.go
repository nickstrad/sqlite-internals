package sqlitex

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// DBStatusOp selects a per-connection counter read by sqlite3_db_status
// (status.c in the pinned tree).
type DBStatusOp = sqlite.DBStatusOp

// Per-connection status counters exposed through ReadDBStatus.
const (
	DBStatusLookasideUsed = sqlite.DBStatusLookasideUsed
	DBStatusCacheUsed     = sqlite.DBStatusCacheUsed
	DBStatusSchemaUsed    = sqlite.DBStatusSchemaUsed
	DBStatusStmtUsed      = sqlite.DBStatusStmtUsed
	DBStatusCacheHit      = sqlite.DBStatusCacheHit
	DBStatusCacheMiss     = sqlite.DBStatusCacheMiss
	DBStatusCacheWrite    = sqlite.DBStatusCacheWrite
	DBStatusCacheSpill    = sqlite.DBStatusCacheSpill
)

// Primary SQLite lock-contention result codes checked by experiments.
const (
	CodeBusy   = sqlite3.SQLITE_BUSY
	CodeLocked = sqlite3.SQLITE_LOCKED
)

func withRawConn[T any](ctx context.Context, conn *sql.Conn, capability string, use func(T) error) error {
	return conn.Raw(func(driverConn any) error {
		typed, ok := driverConn.(T)
		if !ok {
			return fmt.Errorf("driver connection %T does not provide %s", driverConn, capability)
		}
		return use(typed)
	})
}

// DBStatus holds the current value and high-water mark returned by sqlite3_db_status.
type DBStatus struct {
	Current   int
	Highwater int
}

// ReadDBStatus calls sqlite3_db_status on conn for op. When reset is true,
// SQLite resets the high-water mark to the current value.
func ReadDBStatus(ctx context.Context, conn *sql.Conn, op DBStatusOp, reset bool) (DBStatus, error) {
	var status DBStatus
	err := withRawConn(ctx, conn, "sqlite3_db_status", func(c interface {
		Status(sqlite.DBStatusOp, bool) (int, int, error)
	}) error {
		var err error
		status.Current, status.Highwater, err = c.Status(op, reset)
		return err
	})
	return status, err
}

// Serialize calls sqlite3_serialize on conn and returns a byte slice holding
// the entire "main" database image.
func Serialize(ctx context.Context, conn *sql.Conn) ([]byte, error) {
	var image []byte
	err := withRawConn(ctx, conn, "sqlite3_serialize", func(c interface{ Serialize() ([]byte, error) }) error {
		var err error
		image, err = c.Serialize()
		return err
	})
	return image, err
}

// Deserialize calls sqlite3_deserialize to load image into conn as an in-memory
// database. A serialized WAL-mode image has header bytes 18 and 19 set to 2 and
// fails on query (SQLITE_CANTOPEN, code 14) unless those two bytes are rewritten
// to 1 on a fresh connection first.
func Deserialize(ctx context.Context, conn *sql.Conn, image []byte) error {
	return withRawConn(ctx, conn, "sqlite3_deserialize", func(c interface{ Deserialize([]byte) error }) error {
		return c.Deserialize(image)
	})
}

// DataVersion queries SQLITE_FCNTL_DATA_VERSION on schema. SQLite increments
// this counter whenever another connection modifies the database file.
func DataVersion(ctx context.Context, conn *sql.Conn, schema string) (uint32, error) {
	var version uint32
	err := withRawConn(ctx, conn, "SQLITE_FCNTL_DATA_VERSION", func(c interface {
		FileControlDataVersion(string) (uint32, error)
	}) error {
		var err error
		version, err = c.FileControlDataVersion(schema)
		return err
	})
	return version, err
}

// PersistWAL calls SQLITE_FCNTL_PERSIST_WAL on schema. Pass mode -1 to query
// the current flag, 1 to keep the -wal file on last close, or 0 to restore
// normal removal on last close.
func PersistWAL(ctx context.Context, conn *sql.Conn, schema string, mode int) (int, error) {
	var result int
	err := withRawConn(ctx, conn, "SQLITE_FCNTL_PERSIST_WAL", func(c interface {
		FileControlPersistWAL(string, int) (int, error)
	}) error {
		var err error
		result, err = c.FileControlPersistWAL(schema, mode)
		return err
	})
	return result, err
}

// BackupStep records the progress returned after one sqlite3_backup_step call.
type BackupStep struct {
	Step      int
	More      bool
	Remaining int
	PageCount int
}

func backupDestinationURI(path string) (string, error) {
	return FileURI(path, url.Values{})
}

// Backup copies the database open on source to destinationPath using SQLite's
// online backup API (sqlite3_backup_init/step/finish, backup.c). Pass
// pagesPerStep = -1 to copy every page in one step; afterStep runs after each
// step so experiments can observe restarts or interleave writes. Backup checks
// ctx.Err() before each step and finishes the handle before returning on cancellation.
func Backup(ctx context.Context, source *sql.Conn, destinationPath string, pagesPerStep int, afterStep func(BackupStep) error) ([]BackupStep, error) {
	var after func(BackupStep, SourceExecer) error
	if afterStep != nil {
		after = func(step BackupStep, _ SourceExecer) error { return afterStep(step) }
	}
	return BackupWithSourceExec(ctx, source, destinationPath, pagesPerStep, after)
}

// SourceExecer executes SQL statements on the raw driver connection underlying
// a *sql.Conn while Conn.Raw holds that connection locked (used by
// BackupWithSourceExec to run same-connection writes between backup steps).
type SourceExecer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

type rawExecer struct {
	conn driver.ExecerContext
}

func (e rawExecer) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	named := make([]driver.NamedValue, len(args))
	for i, arg := range args {
		value, err := driver.DefaultParameterConverter.ConvertValue(arg)
		if err != nil {
			return nil, fmt.Errorf("argument %d: %w", i+1, err)
		}
		named[i] = driver.NamedValue{Ordinal: i + 1, Value: value}
	}
	return e.conn.ExecContext(ctx, query, named)
}

// BackupWithSourceExec copies the database open on source to destinationPath
// using SQLite's online backup API (sqlite3_backup_step, src/backup.c:323-591).
// After each step it invokes afterStep with the step summary and a SourceExecer
// bound to the same raw SQLite connection—needed because *sql.Conn is locked
// inside Conn.Raw, so same-connection commits that trigger sqlite3BackupUpdate
// (src/backup.c:688-715) must go through the raw driver connection.
func BackupWithSourceExec(ctx context.Context, source *sql.Conn, destinationPath string, pagesPerStep int, afterStep func(BackupStep, SourceExecer) error) ([]BackupStep, error) {
	if pagesPerStep == 0 {
		return nil, errors.New("pagesPerStep must be non-zero; use -1 for all pages in one step")
	}
	destination, err := backupDestinationURI(destinationPath)
	if err != nil {
		return nil, err
	}
	var steps []BackupStep
	err = withRawConn(ctx, source, "sqlite3_backup", func(c interface {
		NewBackup(string) (*sqlite.Backup, error)
		driver.ExecerContext
	}) error {
		backup, err := c.NewBackup(destination)
		if err != nil {
			return err
		}
		execer := rawExecer{conn: c}
		for {
			if err := ctx.Err(); err != nil {
				backup.Finish()
				return err
			}
			more, stepErr := backup.Step(int32(pagesPerStep))
			if stepErr != nil {
				backup.Finish()
				return stepErr
			}
			step := BackupStep{Step: len(steps) + 1, More: more, Remaining: backup.Remaining(), PageCount: backup.PageCount()}
			steps = append(steps, step)
			if afterStep != nil {
				if err := afterStep(step, execer); err != nil {
					backup.Finish()
					return err
				}
			}
			if !more {
				return backup.Finish()
			}
		}
	})
	return steps, err
}

// Restore copies the database at sourcePath into destination in a single
// sqlite3_backup_step(-1) call.
func Restore(ctx context.Context, destination *sql.Conn, sourcePath string) error {
	source, err := backupDestinationURI(sourcePath)
	if err != nil {
		return err
	}
	return withRawConn(ctx, destination, "sqlite3_backup (restore)", func(c interface {
		NewRestore(string) (*sqlite.Backup, error)
	}) error {
		restore, err := c.NewRestore(source)
		if err != nil {
			return err
		}
		if _, err := restore.Step(-1); err != nil {
			restore.Finish()
			return err
		}
		return restore.Finish()
	})
}

// WAL checkpoint modes accepted by PRAGMA wal_checkpoint(<mode>).
// NOOP was added in SQLite 3.51.0 and reports log/checkpointed counts without copying frames.
const (
	CheckpointNoop     = "NOOP"
	CheckpointPassive  = "PASSIVE"
	CheckpointFull     = "FULL"
	CheckpointRestart  = "RESTART"
	CheckpointTruncate = "TRUNCATE"
)

// CheckpointResult holds the three columns returned by PRAGMA wal_checkpoint:
// whether a lock blocked full progress (Busy), total frames in the WAL (LogFrames),
// and frames copied back into the database file (Checkpointed).
type CheckpointResult struct {
	Busy         bool
	LogFrames    int
	Checkpointed int
}

// String formats CheckpointResult as "busy=B log=L checkpointed=C".
func (r CheckpointResult) String() string {
	busy := 0
	if r.Busy {
		busy = 1
	}
	return fmt.Sprintf("busy=%d log=%d checkpointed=%d", busy, r.LogFrames, r.Checkpointed)
}

// Checkpoint runs PRAGMA <schema>.wal_checkpoint(<mode>) on q, defaulting to
// schema "main" when schema is empty.
func Checkpoint(ctx context.Context, q Querier, schema, mode string) (CheckpointResult, error) {
	mode = strings.ToUpper(mode)
	switch mode {
	case CheckpointNoop, CheckpointPassive, CheckpointFull, CheckpointRestart, CheckpointTruncate:
	default:
		return CheckpointResult{}, fmt.Errorf("unknown checkpoint mode %q", mode)
	}
	if schema == "" {
		schema = "main"
	}
	if strings.ContainsAny(schema, "\"'`[]; ") {
		return CheckpointResult{}, fmt.Errorf("invalid schema name %q", schema)
	}
	var busy, logFrames, checkpointed int
	statement := fmt.Sprintf("PRAGMA %s.wal_checkpoint(%s)", schema, mode)
	if err := q.QueryRowContext(ctx, statement).Scan(&busy, &logFrames, &checkpointed); err != nil {
		return CheckpointResult{}, fmt.Errorf("%s: %w", statement, err)
	}
	return CheckpointResult{Busy: busy != 0, LogFrames: logFrames, Checkpointed: checkpointed}, nil
}

// ErrorCode extracts the extended SQLite result code from err if it wraps a
// *sqlite.Error.
func ErrorCode(err error) (int, bool) {
	var sqliteErr *sqlite.Error
	if errors.As(err, &sqliteErr) {
		return sqliteErr.Code(), true
	}
	return 0, false
}

// PrimaryErrorCode returns the low 8 bits of SQLite's result code, stripping
// any extended error detail (such as SQLITE_BUSY_SNAPSHOT -> SQLITE_BUSY).
func PrimaryErrorCode(err error) (int, bool) {
	code, ok := ErrorCode(err)
	return code & 0xff, ok
}

// IsBusy reports whether err carries primary SQLite result code SQLITE_BUSY.
func IsBusy(err error) bool {
	code, ok := PrimaryErrorCode(err)
	return ok && code == CodeBusy
}

func openMemory(ctx context.Context) (*sql.DB, error) {
	connector, err := sqlite.NewConnector("file::memory:")
	if err != nil {
		return nil, err
	}
	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}
