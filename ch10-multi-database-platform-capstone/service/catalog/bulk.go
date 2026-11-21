package catalog

import (
	"context"
	"encoding/json"

	"github.com/nickstrad/walfleet/internal/sqlitex"
)

// RegisterAll validates settings once and inserts every id into databases in a
// single SQLite transaction, leaving any existing rows unchanged.
func (c *Catalog) RegisterAll(ctx context.Context, ids []string, settings sqlitex.ConnSettings) error {
	if err := settings.Validate(); err != nil {
		return err
	}
	encoded, err := json.Marshal(settings)
	if err != nil {
		return err
	}
	tx, err := c.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, "INSERT INTO databases (id, settings) VALUES (?, ?) ON CONFLICT (id) DO NOTHING", id, string(encoded)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// All returns every registered database Record ordered by id, validating each
// row's stored settings JSON through Get.
func (c *Catalog) All(ctx context.Context) ([]Record, error) {
	rows, err := c.DB.QueryContext(ctx, "SELECT id FROM databases ORDER BY id")
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	records := make([]Record, 0, len(ids))
	for _, id := range ids {
		record, err := c.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, nil
}
