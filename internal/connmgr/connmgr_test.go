package connmgr

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
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

// TestNewRejectsBadOptions verifies New rejects zero capacity or missing PathFor/Open callbacks.
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

// TestLRUEvictionKeepsAtMostCapacityOpen checks LRU eviction order, occupancy bounds, stats, and shutdown records for K=2.
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

// TestReopenRefusedWhenFileChangedWhileClosed verifies Acquire rejects an external write with ChangedWhileClosedError until Forget clears the record.
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

// TestReopenRefusedWhenFileNotAtRest verifies Acquire refuses a recorded database when a stray -shm file is present.
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

// TestEvictionReportsNotAtRestWhileAnotherConnectionIsOpen confirms eviction flags remaining -wal and -shm and drops any stored record.
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

// TestAcquireWaitsWhenEveryHandleIsInUse checks capacity waits, context deadlines, double Release safety, and ErrInUse/ErrNotOpen/ErrClosed.
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

// TestSameIDSharesOneConnection verifies concurrent Acquire calls for the same ID share one *sqlitex.DB and block Evict until all handles release.
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

// TestPathErrorAndOpenError checks that PathFor and Open failures increment OpenFailures without leaking entries or bumping miss counters.
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
	if stats := m.Stats(); stats.OpenFailures != 2 || stats.Open != 0 || stats.Misses != 0 || stats.UnverifiedOpens != 0 || stats.VerifiedReopens != 0 {
		t.Fatalf("stats %+v", stats)
	}
}

// TestFailedReopenCountsOnlyAsOpenFailure confirms a failed reopen leaves VerifiedReopens unchanged and preserves Misses == VerifiedReopens + UnverifiedOpens.
func TestFailedReopenCountsOnlyAsOpenFailure(t *testing.T) {
	dir := t.TempDir()
	var failOpen atomic.Bool
	m, err := New(Options{
		Capacity: 1,
		PathFor:  func(id string) (string, error) { return filepath.Join(dir, id+".db"), nil },
		Open: func(ctx context.Context, path string) (*sqlitex.DB, error) {
			if failOpen.Load() {
				return nil, errors.New("simulated open failure")
			}
			options := sqlitex.DefaultOptions()
			options.SingleConnection = true
			return sqlitex.Open(ctx, path, options)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	use(t, m, "a", "CREATE TABLE t(x)")
	if _, err := m.Evict("a"); err != nil {
		t.Fatal(err)
	}
	failOpen.Store(true)
	if _, err := m.Acquire(context.Background(), "a"); err == nil {
		t.Fatal("open failure not reported")
	}
	stats := m.Stats()
	if stats.Misses != 1 || stats.OpenFailures != 1 || stats.UnverifiedOpens != 1 || stats.VerifiedReopens != 0 || stats.RefusedReopens != 0 {
		t.Fatalf("after a failed reopen: %+v", stats)
	}
	failOpen.Store(false)
	use(t, m, "a", "SELECT 1")
	stats = m.Stats()
	if stats.Misses != 2 || stats.UnverifiedOpens+stats.VerifiedReopens != stats.Misses || stats.VerifiedReopens != 1 {
		t.Fatalf("after a good reopen: %+v", stats)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestAcquireWithCancelledContextEvictsNothing verifies Acquire returns ctx.Err() before evicting an idle database at capacity.
func TestAcquireWithCancelledContextEvictsNothing(t *testing.T) {
	m, _ := newTestManager(t, 1, nil)
	use(t, m, "a", "CREATE TABLE t(x)")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := m.Acquire(ctx, "b"); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
	if ids := m.OpenIDs(); len(ids) != 1 || ids[0] != "a" {
		t.Fatalf("open ids %v", ids)
	}
	if stats := m.Stats(); stats.Evictions != 0 || stats.OpenFailures != 0 {
		t.Fatalf("stats %+v", stats)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestEvictionWithRowsStillOpenSkipsTheFileCheck verifies eviction skips CheckAtRest when *sql.Rows keeps a rollback connection open, preserving its connection and reporting not-at-rest.
func TestEvictionWithRowsStillOpenSkipsTheFileCheck(t *testing.T) {
	dir := t.TempDir()
	m, err := New(Options{
		Capacity: 1,
		PathFor:  func(id string) (string, error) { return filepath.Join(dir, id+".db"), nil },
		Open: func(ctx context.Context, path string) (*sqlitex.DB, error) {
			options := sqlitex.DefaultOptions()
			options.File.JournalMode = sqlitex.JournalDelete
			options.SingleConnection = true
			return sqlitex.Open(ctx, path, options)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	use(t, m, "a", "CREATE TABLE t(x); INSERT INTO t VALUES (1), (2)")
	ctx := context.Background()
	h, err := m.Acquire(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	rows, err := h.DB.QueryContext(ctx, "SELECT x FROM t ORDER BY x")
	if err != nil {
		t.Fatal(err)
	}
	if !rows.Next() {
		t.Fatal(rows.Err())
	}
	h.Release()
	eviction, err := m.Evict("a")
	if err != nil {
		t.Fatal(err)
	}
	if eviction.Status.AtRest || eviction.Status.SHA256 != "" || len(eviction.Status.Problems) != 1 || !strings.Contains(eviction.Status.Problems[0], "1 connection(s) still open in this process") {
		t.Fatalf("eviction %+v", eviction)
	}
	if _, ok := m.Record("a"); ok {
		t.Fatal("record kept while the connection is still open")
	}
	if !rows.Next() {
		t.Fatalf("the open rows lost their connection: %v", rows.Err())
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	status, err := CheckAtRest(filepath.Join(dir, "a.db"))
	if err != nil || !status.AtRest {
		t.Fatalf("after rows.Close: %+v %v", status, err)
	}
	if stats := m.Stats(); stats.EvictedNotAtRest != 1 || stats.EvictedAtRest != 0 {
		t.Fatalf("stats %+v", stats)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
}

const lockHolderEnv = "CONNMGR_TEST_LOCK_HOLDER"

// TestLockHolderProcess is a helper child process that holds a WAL locking_mode=EXCLUSIVE connection until stdin closes.
func TestLockHolderProcess(t *testing.T) {
	path := os.Getenv(lockHolderEnv)
	if path == "" {
		t.Skip("helper process only")
	}
	ctx := context.Background()
	uri, err := sqlitex.FileURI(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", uri)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, "PRAGMA locking_mode=EXCLUSIVE"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := conn.QueryRowContext(ctx, "SELECT count(*) FROM t").Scan(&n); err != nil {
		t.Fatal(err)
	}
	os.Stdout.WriteString("ready\n")
	io.Copy(io.Discard, os.Stdin)
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestCheckAtRestSeesALockHeldByAnotherProcess confirms CheckAtRest detects a cross-process WAL locking_mode=EXCLUSIVE reader via F_GETLK even without -shm.
func TestCheckAtRestSeesALockHeldByAnotherProcess(t *testing.T) {
	m, dir := newTestManager(t, 1, nil)
	use(t, m, "a", "CREATE TABLE t(x)")
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "a.db")
	child := exec.Command(os.Args[0], "-test.run=^TestLockHolderProcess$")
	child.Env = append(os.Environ(), lockHolderEnv+"="+path)
	stdin, err := child.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || line != "ready\n" {
		stdin.Close()
		child.Wait()
		t.Fatalf("helper: %q %v", line, err)
	}
	status, err := CheckAtRest(path)
	stdin.Close()
	go io.Copy(io.Discard, stdout)
	if waitErr := child.Wait(); waitErr != nil {
		t.Fatalf("helper exit: %v", waitErr)
	}
	if err != nil {
		t.Fatal(err)
	}
	if _, statErr := os.Stat(path + "-shm"); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("an exclusive-mode WAL reader should leave no -shm: %v", statErr)
	}
	if status.AtRest || len(status.Problems) != 1 || !strings.Contains(status.Problems[0], "another process holds a SQLite lock") {
		t.Fatalf("while another process holds the file: %+v", status)
	}
	status, err = CheckAtRest(path)
	if err != nil || !status.AtRest {
		t.Fatalf("after the other process closed: %+v %v", status, err)
	}
}

// TestCheckAtRest covers missing files, plain files, empty and non-empty -wal, -shm, and cold versus hot rollback journals.
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
