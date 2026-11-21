package catalog

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
)

func TestClaimBumpsTheEpochAndFencesOldRecords(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "catalog.db")
	c, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	settings := sqlitex.DefaultConnSettings()
	settings.Synchronous = sqlitex.SyncNormal
	if err := c.Register(ctx, "db-1", settings); err != nil {
		t.Fatal(err)
	}
	settings.CacheSize = -1
	if err := c.Register(ctx, "db-1", settings); err != nil {
		t.Fatal(err)
	}
	record, err := c.Get(ctx, "db-1")
	if err != nil || record.Epoch != 0 || record.Owner != "" || record.Settings.CacheSize != -2000 || record.Settings.Synchronous != sqlitex.SyncNormal {
		t.Fatalf("%+v %v", record, err)
	}
	other, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	first, err := c.Claim(ctx, "db-1", "a")
	if err != nil || first != 1 {
		t.Fatalf("%d %v", first, err)
	}
	if err := c.RecordShipped(ctx, "db-1", 1, 7, 3, "a:db-1.db"); err != nil {
		t.Fatal(err)
	}
	second, err := other.Claim(ctx, "db-1", "b")
	if err != nil || second != 2 {
		t.Fatalf("%d %v", second, err)
	}
	if err := c.RecordShipped(ctx, "db-1", 1, 8, 4, "a:db-1.db"); !errors.Is(err, ErrStaleEpoch) {
		t.Fatalf("stale epoch recorded: %v", err)
	}
	record, err = c.Get(ctx, "db-1")
	if err != nil || record.Owner != "b" || record.Epoch != 2 || record.LastShipped != 7 || record.SchemaVersion != 3 || record.Location != "" {
		t.Fatalf("%+v %v", record, err)
	}
	if _, err := c.Claim(ctx, "db-9", "a"); !errors.Is(err, ErrUnknown) {
		t.Fatalf("claim of unknown: %v", err)
	}
	if _, err := c.Get(ctx, "db-9"); !errors.Is(err, ErrUnknown) {
		t.Fatalf("get of unknown: %v", err)
	}
	if err := c.Register(ctx, "db-2", sqlitex.ConnSettings{Synchronous: "SOMETIMES"}); err == nil {
		t.Fatal("invalid settings registered")
	}
	if _, err := c.DB.ExecContext(ctx, "UPDATE databases SET settings = '{\"Synchronous\":\"FULL\",\"Extra\":1}' WHERE id = 'db-1'"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Get(ctx, "db-1"); err == nil {
		t.Fatal("unknown settings field accepted")
	}
}
