package connmgr

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
)

var idPattern = regexp.MustCompile(`^[a-z0-9-]+$`)

func newTestManager(t *testing.T, capacity int, evictions *[]Eviction) (*Manager, string) {
	t.Helper()
	dir := t.TempDir()
	m, err := New(Options{
		Capacity: capacity,
		PathFor: func(id string) (string, error) {
			if !idPattern.MatchString(id) {
				return "", errors.New("bad id")
			}
			return filepath.Join(dir, id+".db"), nil
		},
		Open: func(ctx context.Context, path string) (*sqlitex.DB, error) {
			options := sqlitex.DefaultOptions()
			options.SingleConnection = true
			return sqlitex.Open(ctx, path, options)
		},
		OnEvict: func(e Eviction) {
			if evictions != nil {
				*evictions = append(*evictions, e)
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return m, dir
}

func use(t *testing.T, m *Manager, id string, sql string) {
	t.Helper()
	ctx := context.Background()
	h, err := m.Acquire(ctx, id)
	if err != nil {
		t.Fatalf("acquire %s: %v", id, err)
	}
	defer h.Release()
	if _, err := h.DB.ExecContext(ctx, sql); err != nil {
		t.Fatalf("%s: %s: %v", id, sql, err)
	}
}

func TestNewRejectsBadOptions(t *testing.T) {
	open := func(context.Context, string) (*sqlitex.DB, error) { return nil, nil }
	path := func(string) (string, error) { return "", nil }
	for name, options := range map[string]Options{
		"zero capacity": {Capacity: 0, PathFor: path, Open: open},
		"no path":       {Capacity: 1, Open: open},
		"no open":       {Capacity: 1, PathFor: path},
	} {
		if _, err := New(options); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestLRUEvictionKeepsAtMostCapacityOpen(t *testing.T) {
	var evictions []Eviction
	m, _ := newTestManager(t, 2, &evictions)
	for _, id := range []string{"a", "b", "a", "c", "d", "a"} {
		use(t, m, id, "CREATE TABLE IF NOT EXISTS t(x)")
		if open := len(m.OpenIDs()); open > 2 {
			t.Fatalf("%d open after %s", open, id)
		}
	}
	var order []string
	for _, e := range evictions {
		order = append(order, e.ID)
		if e.Cause != CauseCapacity || !e.Status.AtRest || e.CloseErr != nil || e.StatusErr != nil {
			t.Errorf("eviction %+v", e)
		}
	}
	if got := strings.Join(order, ","); got != "b,a,c" {
		t.Fatalf("eviction order %s, want b,a,c", got)
	}
	if got := strings.Join(m.OpenIDs(), ","); got != "a,d" {
		t.Fatalf("open %s, want a,d", got)
	}
	stats := m.Stats()
	if stats.Hits != 1 || stats.Misses != 5 || stats.MaxOpen != 2 || stats.VerifiedReopens != 1 || stats.UnverifiedOpens != 4 || stats.DistinctDatabases != 4 {
		t.Fatalf("stats %+v", stats)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if len(evictions) != 5 {
		t.Fatalf("%d evictions after close", len(evictions))
	}
	for _, id := range []string{"a", "b", "c", "d"} {
		record, ok := m.Record(id)
		if !ok || !record.AtRest || len(record.SHA256) != 64 {
			t.Errorf("%s record %+v %v", id, record, ok)
		}
	}
	if _, err := m.Acquire(context.Background(), "a"); !errors.Is(err, ErrClosed) {
		t.Fatalf("acquire after close: %v", err)
	}
}

func TestReopenRefusedWhenFileChangedWhileClosed(t *testing.T) {
	m, dir := newTestManager(t, 1, nil)
	use(t, m, "a", "CREATE TABLE t(x)")
	use(t, m, "b", "CREATE TABLE t(x)")
	ctx := context.Background()
	outside, err := sqlitex.OpenDefault(ctx, filepath.Join(dir, "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := outside.ExecContext(ctx, "INSERT INTO t VALUES(1)"); err != nil {
		t.Fatal(err)
	}
	if err := outside.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = m.Acquire(ctx, "a")
	var changed *ChangedWhileClosedError
	if !errors.As(err, &changed) || changed.ID != "a" || changed.Found.SHA256 == changed.Recorded.SHA256 || !changed.Found.AtRest {
		t.Fatalf("got %v", err)
	}
	if stats := m.Stats(); stats.RefusedReopens != 1 {
		t.Fatalf("stats %+v", stats)
	}
	if !m.Forget("a") {
		t.Fatal("no record to forget")
	}
	h, err := m.Acquire(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	h.Release()
}

func TestReopenRefusedWhenFileNotAtRest(t *testing.T) {
	m, dir := newTestManager(t, 1, nil)
	use(t, m, "a", "CREATE TABLE t(x)")
	use(t, m, "b", "CREATE TABLE t(x)")
	if err := os.WriteFile(filepath.Join(dir, "a.db-shm"), make([]byte, 32768), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := m.Acquire(context.Background(), "a")
	var changed *ChangedWhileClosedError
	if !errors.As(err, &changed) || changed.Found.AtRest {
		t.Fatalf("got %v", err)
	}
}

func TestEvictionReportsNotAtRestWhileAnotherConnectionIsOpen(t *testing.T) {
	m, dir := newTestManager(t, 1, nil)
	use(t, m, "a", "CREATE TABLE t(x)")
	ctx := context.Background()
	other, err := sqlitex.OpenDefault(ctx, filepath.Join(dir, "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.ExecContext(ctx, "SELECT count(*) FROM t"); err != nil {
		t.Fatal(err)
	}
	use(t, m, "a", "INSERT INTO t VALUES(1)")
	eviction, err := m.Evict("a")
	if err != nil {
		t.Fatal(err)
	}
	if eviction.Status.AtRest || len(eviction.Status.Problems) != 2 {
		t.Fatalf("eviction %+v", eviction)
	}
	if !strings.Contains(eviction.Status.Problems[0], "-wal holds") || !strings.Contains(eviction.Status.Problems[1], "-shm exists") {
		t.Fatalf("problems %q", eviction.Status.Problems)
	}
	if _, ok := m.Record("a"); ok {
		t.Fatal("record kept for a file not at rest")
	}
	if err := other.Close(); err != nil {
		t.Fatal(err)
	}
	status, err := CheckAtRest(filepath.Join(dir, "a.db"))
	if err != nil || !status.AtRest {
		t.Fatalf("after the other connection closed: %+v %v", status, err)
	}
	h, err := m.Acquire(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	h.Release()
	if stats := m.Stats(); stats.EvictedNotAtRest != 1 || stats.UnverifiedOpens != 2 {
		t.Fatalf("stats %+v", stats)
	}
}

func TestAcquireWaitsWhenEveryHandleIsInUse(t *testing.T) {
	m, _ := newTestManager(t, 1, nil)
	ctx := context.Background()
	held, err := m.Acquire(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if _, err := m.Acquire(short, "b"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
	if _, err := m.Evict("a"); !errors.Is(err, ErrInUse) {
		t.Fatalf("evict in use: %v", err)
	}
	if err := m.Close(); !errors.Is(err, ErrInUse) {
		t.Fatalf("close in use: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		h, err := m.Acquire(ctx, "b")
		if err == nil {
			h.Release()
		}
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	held.Release()
	held.Release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if stats := m.Stats(); stats.CapacityWaits != 2 || stats.Open != 1 {
		t.Fatalf("stats %+v", stats)
	}
	if _, err := m.Evict("a"); !errors.Is(err, ErrNotOpen) {
		t.Fatalf("evict a: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); !errors.Is(err, ErrClosed) {
		t.Fatalf("second close: %v", err)
	}
}

func TestSameIDSharesOneConnection(t *testing.T) {
	m, _ := newTestManager(t, 1, nil)
	ctx := context.Background()
	first, err := m.Acquire(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	second, err := m.Acquire(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	if first.DB != second.DB {
		t.Fatal("two connections for one id")
	}
	first.Release()
	if _, err := m.Evict("a"); !errors.Is(err, ErrInUse) {
		t.Fatalf("evict with one handle left: %v", err)
	}
	second.Release()
	if _, err := m.Evict("a"); err != nil {
		t.Fatal(err)
	}
}

func TestPathErrorAndOpenError(t *testing.T) {
	m, dir := newTestManager(t, 1, nil)
	ctx := context.Background()
	if _, err := m.Acquire(ctx, "Bad/ID"); err == nil {
		t.Fatal("bad id accepted")
	}
	if err := os.WriteFile(filepath.Join(dir, "junk.db"), []byte(strings.Repeat("not a database ", 20)), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Acquire(ctx, "junk"); err == nil {
		t.Fatal("junk file opened")
	}
	if stats := m.Stats(); stats.OpenFailures != 2 || stats.Open != 0 {
		t.Fatalf("stats %+v", stats)
	}
}

func TestCheckAtRest(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.db")
	status, err := CheckAtRest(path)
	if err != nil || status.AtRest || len(status.Problems) != 1 || status.Problems[0] != "database file missing" {
		t.Fatalf("missing: %+v %v", status, err)
	}
	if err := os.WriteFile(path, []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	status, err = CheckAtRest(path)
	if err != nil || !status.AtRest || status.Size != 3 || status.SHA256 != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Fatalf("plain: %+v %v", status, err)
	}
	write := func(name string, data []byte) {
		t.Helper()
		if err := os.WriteFile(path+name, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	remove := func(name string) {
		t.Helper()
		if err := os.Remove(path + name); err != nil {
			t.Fatal(err)
		}
	}
	write("-wal", nil)
	if status, err = CheckAtRest(path); err != nil || !status.AtRest {
		t.Fatalf("empty -wal: %+v %v", status, err)
	}
	write("-wal", []byte{1})
	if status, err = CheckAtRest(path); err != nil || status.AtRest || !strings.Contains(status.Problems[0], "-wal holds 1 bytes") {
		t.Fatalf("-wal with content: %+v %v", status, err)
	}
	remove("-wal")
	write("-shm", nil)
	if status, err = CheckAtRest(path); err != nil || status.AtRest || status.SHA256 != "" || !strings.Contains(status.Problems[0], "-shm exists (0 bytes)") {
		t.Fatalf("-shm: %+v %v", status, err)
	}
	remove("-shm")
	write("-journal", []byte{0, 1, 2})
	if status, err = CheckAtRest(path); err != nil || !status.AtRest {
		t.Fatalf("cold journal: %+v %v", status, err)
	}
	write("-journal", []byte{0xd9, 1, 2})
	if status, err = CheckAtRest(path); err != nil || status.AtRest || !strings.Contains(status.Problems[0], "hot -journal") {
		t.Fatalf("hot journal: %+v %v", status, err)
	}
	if !strings.HasPrefix(status.String(), "not at rest: hot -journal") {
		t.Fatalf("string %q", status.String())
	}
}
