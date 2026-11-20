package sqlitex

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

type DBStatusOp = sqlite.DBStatusOp

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

type DBStatus struct {
	Current   int
	Highwater int
}

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

func Serialize(ctx context.Context, conn *sql.Conn) ([]byte, error) {
	var image []byte
	err := withRawConn(ctx, conn, "sqlite3_serialize", func(c interface{ Serialize() ([]byte, error) }) error {
		var err error
		image, err = c.Serialize()
		return err
	})
	return image, err
}

func Deserialize(ctx context.Context, conn *sql.Conn, image []byte) error {
	return withRawConn(ctx, conn, "sqlite3_deserialize", func(c interface{ Deserialize([]byte) error }) error {
		return c.Deserialize(image)
	})
}

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

type BackupStep struct {
	Step      int
	More      bool
	Remaining int
	PageCount int
}

func backupDestinationURI(path string) (string, error) {
	return FileURI(path, url.Values{})
}

func Backup(ctx context.Context, source *sql.Conn, destinationPath string, pagesPerStep int, afterStep func(BackupStep) error) ([]BackupStep, error) {
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
	}) error {
		backup, err := c.NewBackup(destination)
		if err != nil {
			return err
		}
		for {
			more, stepErr := backup.Step(int32(pagesPerStep))
			if stepErr != nil {
				backup.Finish()
				return stepErr
			}
			step := BackupStep{Step: len(steps) + 1, More: more, Remaining: backup.Remaining(), PageCount: backup.PageCount()}
			steps = append(steps, step)
			if afterStep != nil {
				if err := afterStep(step); err != nil {
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

const (
	CheckpointNoop     = "NOOP"
	CheckpointPassive  = "PASSIVE"
	CheckpointFull     = "FULL"
	CheckpointRestart  = "RESTART"
	CheckpointTruncate = "TRUNCATE"
)

type CheckpointResult struct {
	Busy         bool
	LogFrames    int
	Checkpointed int
}

func (r CheckpointResult) String() string {
	busy := 0
	if r.Busy {
		busy = 1
	}
	return fmt.Sprintf("busy=%d log=%d checkpointed=%d", busy, r.LogFrames, r.Checkpointed)
}

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

func ErrorCode(err error) (int, bool) {
	var sqliteErr *sqlite.Error
	if errors.As(err, &sqliteErr) {
		return sqliteErr.Code(), true
	}
	return 0, false
}

func PrimaryErrorCode(err error) (int, bool) {
	code, ok := ErrorCode(err)
	return code & 0xff, ok
}

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
