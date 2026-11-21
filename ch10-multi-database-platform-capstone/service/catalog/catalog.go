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

var (
	ErrUnknown    = errors.New("catalog: unknown database")
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

type Record struct {
	ID            string
	Owner         string
	Epoch         uint64
	Location      string
	LastShipped   int64
	SchemaVersion int
	Settings      sqlitex.ConnSettings
}

type Catalog struct {
	DB *sqlitex.DB
}

func Options() sqlitex.Options {
	options := sqlitex.DefaultOptions()
	options.SingleConnection = true
	options.Conn.CacheSize = -256
	return options
}

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

func (c *Catalog) Close() error { return c.DB.Close() }

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

func (c *Catalog) Claim(ctx context.Context, id, node string) (uint64, error) {
	var epoch uint64
	err := c.DB.QueryRowContext(ctx, "UPDATE databases SET owner = ?, epoch = epoch + 1, location = '' WHERE id = ? RETURNING epoch", node, id).Scan(&epoch)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("%w: %s", ErrUnknown, id)
	}
	return epoch, err
}

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
