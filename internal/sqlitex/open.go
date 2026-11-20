package sqlitex

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"modernc.org/sqlite"
)

// Querier is satisfied by *sql.DB, *sql.Conn, and *sql.Tx so helpers that read
// pragmas or run checkpoints work on any of them.
type Querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// DB wraps *sql.DB with the filesystem path and Options used to open it.
type DB struct {
	*sql.DB
	Path    string
	Options Options
}

// configuredConnector intercepts every physical connection database/sql opens
// so per-connection pragmas are applied uniformly across the pool.
type configuredConnector struct {
	base    driver.Connector
	options Options
}

// Driver returns the underlying modernc.org/sqlite driver.
func (c *configuredConnector) Driver() driver.Driver { return c.base.Driver() }

// Connect opens a raw connection and applies file and per-connection pragmas before returning it to the pool.
func (c *configuredConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.base.Connect(ctx)
	if err != nil {
		return nil, err
	}
	if err := configureConnection(ctx, conn, c.options); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

// FileURI converts a filesystem path and query parameters into an SQLite file
// URI with percent-escaped special characters (such as spaces, '?', and '#').
// It rejects empty paths and MemoryPath so in-memory opens never create a file on disk.
func FileURI(path string, query url.Values) (string, error) {
	if path == "" || path == MemoryPath {
		return "", fmt.Errorf("FileURI needs a filesystem path, got %q; use OpenMemory for an in-memory database", path)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	// Go formats an empty-host file URL as "file:///path"; SQLite expects "file:/path".
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(absolute), RawQuery: query.Encode()}
	return strings.Replace(uri.String(), "file://", "file:", 1), nil
}

// DSN builds the modernc.org/sqlite data source name for path and options,
// encoding _txlock=immediate and mode=ro when requested. MemoryPath becomes
// "file::memory:" (with ?_txlock=immediate if set) and rejects ReadOnly.
func DSN(path string, options Options) (string, error) {
	query := url.Values{}
	if options.ImmediateTx {
		query.Set("_txlock", "immediate")
	}
	if path == MemoryPath {
		if options.ReadOnly {
			return "", errors.New("an in-memory database cannot be opened read-only")
		}
		if len(query) == 0 {
			return "file::memory:", nil
		}
		return "file::memory:?" + query.Encode(), nil
	}
	if options.ReadOnly {
		query.Set("mode", "ro")
	}
	return FileURI(path, query)
}

// Open validates options, opens path through modernc.org/sqlite, applies file
// and per-connection pragmas on every physical connection, and verifies that an
// existing database matches the requested file settings without converting it
// into or out of WAL mode. For MemoryPath it requires SingleConnection and
// JournalMemory or JournalOff. On a new or unstamped empty database, file
// settings run in layout -> journal_mode -> identity order, retrying on
// SQLITE_BUSY up to Conn.BusyTimeout.
func Open(ctx context.Context, path string, options Options) (*DB, error) {
	if err := options.File.Validate(); err != nil {
		return nil, err
	}
	if err := options.Conn.Validate(); err != nil {
		return nil, err
	}
	if path == MemoryPath {
		if err := validateMemoryOptions(options); err != nil {
			return nil, err
		}
	}
	dsn, err := DSN(path, options)
	if err != nil {
		return nil, err
	}
	base, err := sqlite.NewConnector(dsn)
	if err != nil {
		return nil, err
	}
	db := sql.OpenDB(&configuredConnector{base: base, options: options})
	if options.SingleConnection {
		db.SetMaxOpenConns(1)
		db.SetMaxIdleConns(1)
		db.SetConnMaxLifetime(0)
		db.SetConnMaxIdleTime(0)
	}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	if !options.ReadOnly {
		actual, err := ReadFileSettings(ctx, db)
		if err != nil {
			db.Close()
			return nil, err
		}
		if err := compareFileSettings(options.File, actual); err != nil {
			db.Close()
			return nil, fmt.Errorf("open %s: %w", path, err)
		}
	}
	return &DB{DB: db, Path: path, Options: options}, nil
}

// OpenDefault opens path with DefaultOptions.
func OpenDefault(ctx context.Context, path string) (*DB, error) {
	return Open(ctx, path, DefaultOptions())
}

// MemoryPath is the special path accepted by Open and DSN for an in-memory database.
const MemoryPath = ":memory:"

// MemoryOptions returns Options suited to an in-memory database: DefaultOptions
// with JournalMemory and SingleConnection enabled so every query shares one database.
func MemoryOptions() Options {
	options := DefaultOptions()
	options.File.JournalMode = JournalMemory
	options.SingleConnection = true
	return options
}

// OpenMemory opens a single-connection in-memory database configured with MemoryOptions.
func OpenMemory(ctx context.Context) (*DB, error) {
	return Open(ctx, MemoryPath, MemoryOptions())
}

func validateMemoryOptions(options Options) error {
	if !options.SingleConnection {
		return errors.New("an in-memory database needs SingleConnection: every pooled connection to file::memory: gets its own private database")
	}
	switch strings.ToLower(options.File.JournalMode) {
	case JournalMemory, JournalOff:
		return nil
	}
	return fmt.Errorf("an in-memory database supports journal_mode %s or %s, not %s", JournalMemory, JournalOff, options.File.JournalMode)
}

func configureConnection(ctx context.Context, conn driver.Conn, options Options) error {
	execer, ok := conn.(driver.ExecerContext)
	if !ok {
		return errors.New("driver connection does not implement ExecerContext")
	}
	queryer, ok := conn.(driver.QueryerContext)
	if !ok {
		return errors.New("driver connection does not implement QueryerContext")
	}
	run := func(statement string) error {
		if _, err := execer.ExecContext(ctx, statement, nil); err != nil {
			return fmt.Errorf("%s: %w", statement, err)
		}
		return nil
	}
	// Apply busy_timeout first so the remaining pragmas wait for locks instead of failing immediately.
	pragmas := options.Conn.pragmas()
	if err := run(pragmas[0]); err != nil {
		return err
	}
	if !options.ReadOnly {
		// Retry file setup on SQLITE_BUSY so concurrent openers of a brand-new file settle cleanly.
		deadline := time.Now().Add(options.Conn.BusyTimeout)
		for {
			err := applyFileSettings(ctx, run, queryer, options.File)
			if err == nil {
				break
			}
			if !IsBusy(err) || time.Now().After(deadline) {
				return err
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(10 * time.Millisecond):
			}
		}
	}
	for _, statement := range pragmas[1:] {
		if err := run(statement); err != nil {
			return err
		}
	}
	actual, err := readConnSettings(ctx, func(query string) (int64, error) {
		return driverQueryInt(ctx, queryer, query)
	})
	if err != nil {
		return err
	}
	return compareConnSettings(options.Conn, actual)
}

// applyFileSettings configures an empty file in layout -> journal_mode -> identity
// order, refuses to convert an existing database into or out of WAL mode, and
// stamps an empty database whose application_id and schema_version are still 0.
func applyFileSettings(ctx context.Context, run func(string) error, queryer driver.QueryerContext, file FileSettings) error {
	pageCount, err := driverQueryInt(ctx, queryer, "PRAGMA page_count")
	if err != nil {
		return err
	}
	if pageCount == 0 {
		for _, statement := range file.layoutPragmas() {
			if err := run(statement); err != nil {
				return err
			}
		}
	} else {
		current, err := driverQueryString(ctx, queryer, "PRAGMA journal_mode")
		if err != nil {
			return err
		}
		currentIsWAL := strings.EqualFold(current, JournalWAL)
		wantWAL := strings.EqualFold(file.JournalMode, JournalWAL)
		if currentIsWAL != wantWAL {
			return fmt.Errorf("existing database does not match the requested file settings: journal_mode want %s got %s (Open does not convert an existing database into or out of WAL)",
				strings.ToLower(file.JournalMode), strings.ToLower(current))
		}
	}
	mode, err := driverQueryString(ctx, queryer, file.journalPragma())
	if err != nil {
		return err
	}
	if !strings.EqualFold(mode, file.JournalMode) {
		return fmt.Errorf("journal_mode: asked for %s, SQLite reports %s", file.JournalMode, mode)
	}
	unstamped := pageCount == 0
	if !unstamped {
		applicationID, err := driverQueryInt(ctx, queryer, "PRAGMA application_id")
		if err != nil {
			return err
		}
		schemaVersion, err := driverQueryInt(ctx, queryer, "PRAGMA schema_version")
		if err != nil {
			return err
		}
		unstamped = applicationID == 0 && schemaVersion == 0
	}
	if unstamped {
		for _, statement := range file.identityPragmas() {
			if err := run(statement); err != nil {
				return err
			}
		}
	}
	return nil
}

func driverQueryValue(ctx context.Context, queryer driver.QueryerContext, query string) (driver.Value, error) {
	rows, err := queryer.QueryContext(ctx, query, nil)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", query, err)
	}
	defer rows.Close()
	values := make([]driver.Value, len(rows.Columns()))
	if err := rows.Next(values); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%s: no rows", query)
		}
		return nil, fmt.Errorf("%s: %w", query, err)
	}
	if len(values) == 0 {
		return nil, fmt.Errorf("%s: no columns", query)
	}
	return values[0], nil
}

func driverQueryInt(ctx context.Context, queryer driver.QueryerContext, query string) (int64, error) {
	value, err := driverQueryValue(ctx, queryer, query)
	if err != nil {
		return 0, err
	}
	number, ok := value.(int64)
	if !ok {
		return 0, fmt.Errorf("%s: expected an integer, got %T", query, value)
	}
	return number, nil
}

func driverQueryString(ctx context.Context, queryer driver.QueryerContext, query string) (string, error) {
	value, err := driverQueryValue(ctx, queryer, query)
	if err != nil {
		return "", err
	}
	switch typed := value.(type) {
	case string:
		return typed, nil
	case []byte:
		return string(typed), nil
	}
	return "", fmt.Errorf("%s: expected text, got %T", query, value)
}

func readConnSettings(ctx context.Context, queryInt func(string) (int64, error)) (ConnSettings, error) {
	var settings ConnSettings
	var synchronous, foreignKeys, secureDelete, cacheSize, autocheckpoint, busyTimeout int64
	queries := []struct {
		query  string
		target *int64
	}{
		{"PRAGMA synchronous", &synchronous},
		{"PRAGMA foreign_keys", &foreignKeys},
		{"PRAGMA secure_delete", &secureDelete},
		{"PRAGMA cache_size", &cacheSize},
		{"PRAGMA wal_autocheckpoint", &autocheckpoint},
		{"PRAGMA busy_timeout", &busyTimeout},
	}
	for _, q := range queries {
		value, err := queryInt(q.query)
		if err != nil {
			return settings, err
		}
		*q.target = value
	}
	settings.Synchronous = synchronousName(synchronous)
	settings.ForeignKeys = foreignKeys == 1
	settings.SecureDelete = secureDelete == 1
	settings.CacheSize = int(cacheSize)
	settings.WALAutocheckpoint = int(autocheckpoint)
	settings.BusyTimeout = time.Duration(busyTimeout) * time.Millisecond
	return settings, nil
}

// ReadConnSettings queries SQLite for the live per-connection pragma values on q.
func ReadConnSettings(ctx context.Context, q Querier) (ConnSettings, error) {
	return readConnSettings(ctx, func(query string) (int64, error) {
		var value int64
		err := q.QueryRowContext(ctx, query).Scan(&value)
		return value, err
	})
}

// ReadFileSettings queries SQLite for the persistent header settings of the
// database attached to q.
func ReadFileSettings(ctx context.Context, q Querier) (FileSettings, error) {
	var settings FileSettings
	var pageSize, autoVacuum int64
	scans := []struct {
		query  string
		target any
	}{
		{"PRAGMA page_size", &pageSize},
		{"PRAGMA auto_vacuum", &autoVacuum},
		{"PRAGMA encoding", &settings.Encoding},
		{"PRAGMA application_id", &settings.ApplicationID},
		{"PRAGMA user_version", &settings.UserVersion},
		{"PRAGMA journal_mode", &settings.JournalMode},
	}
	for _, scan := range scans {
		if err := q.QueryRowContext(ctx, scan.query).Scan(scan.target); err != nil {
			return settings, fmt.Errorf("%s: %w", scan.query, err)
		}
	}
	settings.PageSize = int(pageSize)
	settings.AutoVacuum = autoVacuumName(autoVacuum)
	return settings, nil
}

func compareConnSettings(want, got ConnSettings) error {
	want.Synchronous = strings.ToUpper(want.Synchronous)
	if want != got {
		return fmt.Errorf("per-connection settings differ after applying them: want %s, SQLite reports %s", want, got)
	}
	return nil
}

// compareFileSettings skips user_version because workload recipes advance it
// as a step counter on existing files.
func compareFileSettings(want, got FileSettings) error {
	var differences []string
	if want.PageSize != got.PageSize {
		differences = append(differences, fmt.Sprintf("page_size want %d got %d", want.PageSize, got.PageSize))
	}
	if !strings.EqualFold(want.AutoVacuum, got.AutoVacuum) {
		differences = append(differences, fmt.Sprintf("auto_vacuum want %s got %s", want.AutoVacuum, got.AutoVacuum))
	}
	if !strings.EqualFold(want.Encoding, got.Encoding) {
		differences = append(differences, fmt.Sprintf("encoding want %s got %s", want.Encoding, got.Encoding))
	}
	if want.ApplicationID != got.ApplicationID {
		differences = append(differences, fmt.Sprintf("application_id want %d got %d", want.ApplicationID, got.ApplicationID))
	}
	if !strings.EqualFold(want.JournalMode, got.JournalMode) {
		differences = append(differences, fmt.Sprintf("journal_mode want %s got %s", want.JournalMode, got.JournalMode))
	}
	if len(differences) > 0 {
		return fmt.Errorf("existing database does not match the requested file settings: %s", strings.Join(differences, "; "))
	}
	return nil
}
