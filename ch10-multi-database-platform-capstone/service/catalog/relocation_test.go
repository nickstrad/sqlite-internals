package catalog

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickstrad/walfleet/internal/sqlitex"
)

// TestRelocationRecordMovesForwardOnly exercises BeginRelocation guards, monotonic and idempotent AdvanceRelocation rules, FinishRelocation terminal states, and starting a follow-up relocation.
func TestRelocationRecordMovesForwardOnly(t *testing.T) {
	ctx := context.Background()
	c, err := Open(ctx, filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Register(ctx, "db-1", sqlitex.DefaultConnSettings()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.BeginRelocation(ctx, "db-1", "a", "b"); !errors.Is(err, ErrNotTheOwner) {
		t.Fatalf("relocating an unclaimed database: %v", err)
	}
	if _, err := c.Claim(ctx, "db-1", "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.BeginRelocation(ctx, "db-1", "b", "a"); !errors.Is(err, ErrNotTheOwner) {
		t.Fatalf("relocating from a node that is not the owner: %v", err)
	}
	if _, err := c.BeginRelocation(ctx, "db-1", "a", "a"); err == nil {
		t.Fatal("relocating to the same node was accepted")
	}
	r, err := c.BeginRelocation(ctx, "db-1", "a", "b")
	if err != nil {
		t.Fatal(err)
	}
	if r.FromEpoch != 1 || r.ToEpoch != 2 || r.Step != 1 || r.State != RelocationRunning {
		t.Fatalf("%+v", r)
	}
	record, err := c.Get(ctx, "db-1")
	if err != nil || record.Owner != "b" || record.Epoch != 2 || record.Location != "" {
		t.Fatalf("%+v %v", record, err)
	}
	if _, err := c.BeginRelocation(ctx, "db-1", "b", "c"); !errors.Is(err, ErrRelocationRunning) {
		t.Fatalf("second relocation while one runs: %v", err)
	}
	if err := c.AdvanceRelocation(ctx, "db-1", 3, 0, ""); !errors.Is(err, ErrRelocationStepGoes) {
		t.Fatalf("skipping step 2: %v", err)
	}
	if err := c.AdvanceRelocation(ctx, "db-1", 2, 0, ""); err != nil {
		t.Fatal(err)
	}
	digest := strings.Repeat("ab", 32)
	if err := c.AdvanceRelocation(ctx, "db-1", 3, 7, "not-hex"); err == nil {
		t.Fatal("a malformed sha256 was recorded")
	}
	if err := c.AdvanceRelocation(ctx, "db-1", 3, 7, digest); err != nil {
		t.Fatal(err)
	}
	if err := c.AdvanceRelocation(ctx, "db-1", 2, 0, ""); err != nil {
		t.Fatal(err)
	}
	got, err := c.GetRelocation(ctx, "db-1")
	if err != nil || got.Step != 3 || got.Position != 7 || got.SHA256 != digest {
		t.Fatalf("%+v %v", got, err)
	}
	if err := c.FinishRelocation(ctx, "db-1", "bogus"); err == nil {
		t.Fatal("a bogus final state was accepted")
	}
	if err := c.FinishRelocation(ctx, "db-1", RelocationDone); err != nil {
		t.Fatal(err)
	}
	if err := c.FinishRelocation(ctx, "db-1", RelocationDone); !errors.Is(err, ErrNoRelocation) {
		t.Fatalf("finishing twice: %v", err)
	}
	if err := c.AdvanceRelocation(ctx, "db-1", 4, 0, ""); !errors.Is(err, ErrRelocationStepGoes) {
		t.Fatalf("advancing a finished relocation: %v", err)
	}
	if _, err := c.GetRelocation(ctx, "db-2"); !errors.Is(err, ErrNoRelocation) {
		t.Fatalf("unknown relocation: %v", err)
	}
	if _, err := c.BeginRelocation(ctx, "db-1", "b", "a"); err != nil {
		t.Fatalf("a new relocation after the first finished: %v", err)
	}
}

// TestRegisterAllAndAll checks bulk transactional registration preserves existing rows, All returns sorted validated records, and invalid settings are rejected.
func TestRegisterAllAndAll(t *testing.T) {
	ctx := context.Background()
	c, err := Open(ctx, filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Register(ctx, "db-2", sqlitex.DefaultConnSettings()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Claim(ctx, "db-2", "a"); err != nil {
		t.Fatal(err)
	}
	if err := c.RegisterAll(ctx, []string{"db-3", "db-1", "db-2"}, sqlitex.DefaultConnSettings()); err != nil {
		t.Fatal(err)
	}
	records, err := c.All(ctx)
	if err != nil || len(records) != 3 || records[0].ID != "db-1" || records[1].Owner != "a" || records[1].Epoch != 1 || records[2].Epoch != 0 {
		t.Fatalf("%+v %v", records, err)
	}
	bad := sqlitex.DefaultConnSettings()
	bad.Synchronous = "SOMETIMES"
	if err := c.RegisterAll(ctx, []string{"db-4"}, bad); err == nil {
		t.Fatal("invalid settings were registered")
	}
}
