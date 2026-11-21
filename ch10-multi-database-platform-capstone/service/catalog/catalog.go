// Package catalog owns the platform's control-plane metadata in a single
// WAL-mode SQLite database shared by all nodes on the host.
//
// Each row maps a database id to its current owner node, monotonically
// increasing ownership epoch, local file location, last shipped stream
// position, schema version (the recipe's user_version), and per-connection
// SQLite settings (from Chapter 8). A Claim increments the epoch atomically in
// one SQLite write transaction so concurrent claims receive distinct epochs.
// Because SQLite locks work only within one host (src/os_unix.c:1189-1342), a
// multi-host platform would replace this file with a networked control-plane
// service exposing the same operations.
package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
)

// Sentinel errors returned by Catalog lookups and epoch-checked updates.
var (
	// ErrUnknown reports that no row exists for the requested database id.
	ErrUnknown = errors.New("catalog: unknown database")
	// ErrStaleEpoch reports that RecordShipped targeted an epoch that is no
	// longer current in the catalog row.
	ErrStaleEpoch = errors.New("catalog: the epoch is no longer current")
)

const schema = `CREATE TABLE IF NOT EXISTS databases (
	id TEXT PRIMARY KEY,
	owner TEXT NOT NULL DEFAULT '',
	epoch INTEGER NOT NULL DEFAULT 0,
	location TEXT NOT NULL DEFAULT '',
	last_shipped INTEGER NOT NULL DEFAULT 0,
	schema_version INTEGER NOT NULL DEFAULT 0,
	settings TEXT NOT NULL
) STRICT`

// Record is one database's control-plane row in the catalog.
type Record struct {
	ID            string
	Owner         string
	Epoch         uint64
	Location      string
	LastShipped   int64
	SchemaVersion int
	Settings      sqlitex.ConnSettings
}

// Catalog wraps the shared SQLite handle that stores the databases table.
type Catalog struct {
	DB *sqlitex.DB
}

// Options returns the SQLite settings used for the catalog database: WAL mode,
// synchronous=FULL, 5-second busy timeout (from sqlitex.DefaultOptions), a
// single pooled connection, and a 256 KiB page cache.
func Options() sqlitex.Options {
	options := sqlitex.DefaultOptions()
	options.SingleConnection = true
	options.Conn.CacheSize = -256
	return options
}

// Open opens or creates the catalog SQLite database at path and ensures the
// databases table exists.
func Open(ctx context.Context, path string) (*Catalog, error) {
	db, err := sqlitex.Open(ctx, path, Options())
	if err != nil {
		return nil, err
	}
	if _, err := db.ExecContext(ctx, schema); err != nil {
		return nil, errors.Join(err, db.Close())
	}
	return &Catalog{DB: db}, nil
}

// Close closes the underlying catalog database connection.
func (c *Catalog) Close() error { return c.DB.Close() }

// Register inserts a database row with validated per-connection settings if id
// is not already present; existing rows are left unchanged.
func (c *Catalog) Register(ctx context.Context, id string, settings sqlitex.ConnSettings) error {
	if err := settings.Validate(); err != nil {
		return err
	}
	encoded, err := json.Marshal(settings)
	if err != nil {
		return err
	}
	_, err = c.DB.ExecContext(ctx, "INSERT INTO databases (id, settings) VALUES (?, ?) ON CONFLICT (id) DO NOTHING", id, string(encoded))
	return err
}

// Get loads the catalog Record for id, strictly decoding and validating its
// stored per-connection settings JSON.
func (c *Catalog) Get(ctx context.Context, id string) (Record, error) {
	record := Record{ID: id}
	var settings string
	err := c.DB.QueryRowContext(ctx, "SELECT owner, epoch, location, last_shipped, schema_version, settings FROM databases WHERE id = ?", id).
		Scan(&record.Owner, &record.Epoch, &record.Location, &record.LastShipped, &record.SchemaVersion, &settings)
	if errors.Is(err, sql.ErrNoRows) {
		return record, fmt.Errorf("%w: %s", ErrUnknown, id)
	}
	if err != nil {
		return record, err
	}
	decoder := json.NewDecoder(strings.NewReader(settings))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record.Settings); err != nil {
		return record, fmt.Errorf("catalog: settings of %s: %w", id, err)
	}
	return record, record.Settings.Validate()
}

// Claim assigns ownership of id to node, increments the epoch atomically,
// clears location until the new owner fences the store, and returns the new
// epoch. Claim is a takeover: it does not contact the previous owner.
func (c *Catalog) Claim(ctx context.Context, id, node string) (uint64, error) {
	var epoch uint64
	err := c.DB.QueryRowContext(ctx, "UPDATE databases SET owner = ?, epoch = epoch + 1, location = '' WHERE id = ? RETURNING epoch", node, id).Scan(&epoch)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("%w: %s", ErrUnknown, id)
	}
	return epoch, err
}

// RecordShipped updates last_shipped, schema_version, and location only if the
// catalog row still holds epoch; otherwise it returns ErrStaleEpoch. Callers
// ignore ErrStaleEpoch after a store-accepted put because the store, not the
// catalog, is the authority on whether a write was accepted into the lineage.
func (c *Catalog) RecordShipped(ctx context.Context, id string, epoch uint64, position int64, schemaVersion int, location string) error {
	result, err := c.DB.ExecContext(ctx, "UPDATE databases SET last_shipped = ?, schema_version = ?, location = ? WHERE id = ? AND epoch = ?",
		position, schemaVersion, location, id, epoch)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return fmt.Errorf("%w: %s epoch %d", ErrStaleEpoch, id, epoch)
	}
	return nil
}
