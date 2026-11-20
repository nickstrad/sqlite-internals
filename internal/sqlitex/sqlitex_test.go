package sqlitex

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickstrad/sqlite-internals/sqlite"
)

func openTest(t *testing.T, options Options) *DB {
	t.Helper()
	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "test.db"), options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// TestPinMatchesDriver verifies that WritePinCheck prints the expected ok line for the pinned Go driver.
func TestPinMatchesDriver(t *testing.T) {
	db := openTest(t, DefaultOptions())
	var out bytes.Buffer
	if err := WritePinCheck(context.Background(), &out, db); err != nil {
		t.Fatal(err)
	}
	want := "sqlite_source_id: " + sqlite.Current().SourceID + " (pin 3.53.4 ok)\n"
	if out.String() != want {
		t.Fatalf("pin line %q, want %q", out.String(), want)
	}
}

// TestPinLineReportsMismatch confirms that an unknown source ID is flagged by PinLine and CheckSourceID.
func TestPinLineReportsMismatch(t *testing.T) {
	line := PinLine("2000-01-01 00:00:00 deadbeef")
	if !strings.Contains(line, "NOT the pin") {
		t.Fatalf("mismatch not reported: %q", line)
	}
	if err := CheckSourceID("x", "nope"); err == nil {
		t.Fatal("expected mismatch error")
	}
}

// TestSettingsAppliedOnEveryPooledConnection checks that custom ConnSettings reach all four pooled connections.
func TestSettingsAppliedOnEveryPooledConnection(t *testing.T) {
	ctx := context.Background()
	options := DefaultOptions()
	options.Conn.Synchronous = SyncNormal
	options.Conn.SecureDelete = true
	options.Conn.CacheSize = -1234
	options.Conn.WALAutocheckpoint = 77
	options.Conn.BusyTimeout = 250 * time.Millisecond
	db := openTest(t, options)
	db.SetMaxOpenConns(4)
	var conns []*sql.Conn
	for range 4 {
		conn, err := db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, conn)
	}
	for i, conn := range conns {
		got, err := ReadConnSettings(ctx, conn)
		if err != nil {
			t.Fatal(err)
		}
		want := options.Conn
		if got != want {
			t.Fatalf("connection %d: got %s want %s", i, got, want)
		}
	}
	for _, conn := range conns {
		conn.Close()
	}
}

// TestFileSettingsPersisted checks that page size and WAL header bytes are written to disk and mismatch on reopen fails.
func TestFileSettingsPersisted(t *testing.T) {
	ctx := context.Background()
	options := DefaultOptions()
	options.File.PageSize = 8192
	options.File.UserVersion = 7
	path := filepath.Join(t.TempDir(), "p.db")
	db, err := Open(ctx, path, options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE t(id INTEGER PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	got, err := ReadFileSettings(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if got != options.File {
		t.Fatalf("got %s want %s", got, options.File)
	}
	db.Close()
	header, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if header[16] != 0x20 || header[17] != 0x00 {
		t.Fatalf("page size bytes %x %x", header[16], header[17])
	}
	if header[18] != 2 || header[19] != 2 {
		t.Fatalf("WAL format bytes %d %d", header[18], header[19])
	}
	reopenOptions := DefaultOptions()
	if _, err := Open(ctx, path, reopenOptions); err == nil || !strings.Contains(err.Error(), "page_size") {
		t.Fatalf("expected page_size mismatch on reopen, got %v", err)
	}
}

// TestRollbackJournalModeIsPerConnection verifies that non-WAL journal modes are applied to every pooled connection.
func TestRollbackJournalModeIsPerConnection(t *testing.T) {
	ctx := context.Background()
	options := DefaultOptions()
	options.File.JournalMode = JournalTruncate
	db := openTest(t, options)
	db.SetMaxOpenConns(2)
	a, _ := db.Conn(ctx)
	b, _ := db.Conn(ctx)
	defer a.Close()
	defer b.Close()
	for _, conn := range []*sql.Conn{a, b} {
		var mode string
		if err := conn.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
			t.Fatal(err)
		}
		if mode != JournalTruncate {
			t.Fatalf("journal mode %s", mode)
		}
	}
}

// TestInvalidSettingsRejected checks that Open rejects an invalid page size or synchronous mode.
func TestInvalidSettingsRejected(t *testing.T) {
	options := DefaultOptions()
	options.File.PageSize = 1000
	if _, err := Open(context.Background(), filepath.Join(t.TempDir(), "x.db"), options); err == nil {
		t.Fatal("expected page size error")
	}
	options = DefaultOptions()
	options.Conn.Synchronous = "SOMETIMES"
	if _, err := Open(context.Background(), filepath.Join(t.TempDir(), "x.db"), options); err == nil {
		t.Fatal("expected synchronous error")
	}
}

// TestRawWrappers exercises db_status, file controls, checkpoint, backup, serialize/deserialize, and restore.
func TestRawWrappers(t *testing.T) {
	ctx := context.Background()
	options := DefaultOptions()
	options.SingleConnection = true
	dir := t.TempDir()
	db, err := Open(ctx, filepath.Join(dir, "raw.db"), options)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT)"); err != nil {
		t.Fatal(err)
	}
	for i := range 50 {
		if _, err := db.Exec("INSERT INTO t VALUES(?, ?)", i, strings.Repeat("x", 500)); err != nil {
			t.Fatal(err)
		}
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	status, err := ReadDBStatus(ctx, conn, DBStatusCacheUsed, false)
	if err != nil || status.Current <= 0 {
		t.Fatalf("db_status %+v %v", status, err)
	}
	version, err := DataVersion(ctx, conn, "main")
	if err != nil || version == 0 {
		t.Fatalf("data version %d %v", version, err)
	}
	persist, err := PersistWAL(ctx, conn, "main", -1)
	if err != nil || persist != 0 {
		t.Fatalf("persist wal %d %v", persist, err)
	}
	image, err := Serialize(ctx, conn)
	if err != nil || len(image)%4096 != 0 || len(image) == 0 {
		t.Fatalf("serialize %d %v", len(image), err)
	}
	result, err := Checkpoint(ctx, conn, "main", CheckpointNoop)
	if err != nil || result.LogFrames == 0 || result.Checkpointed != 0 {
		t.Fatalf("noop checkpoint %s %v", result, err)
	}
	result, err = Checkpoint(ctx, conn, "", CheckpointTruncate)
	if err != nil || result.Busy || result.LogFrames != 0 {
		t.Fatalf("truncate checkpoint %s %v", result, err)
	}
	if _, err := Checkpoint(ctx, conn, "main", "SOMETIMES"); err == nil {
		t.Fatal("expected error for unknown mode")
	}
	steps, err := Backup(ctx, conn, filepath.Join(dir, "copy.db"), 3, nil)
	if err != nil {
		t.Fatal(err)
	}
	last := steps[len(steps)-1]
	if last.More || last.Remaining != 0 || len(steps) < 2 || steps[0].PageCount != len(image)/4096 {
		t.Fatalf("backup steps %+v", steps)
	}
	copyDB, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "copy.db"))
	if err != nil {
		t.Fatal(err)
	}
	var count int
	if err := copyDB.QueryRow("SELECT count(*) FROM t").Scan(&count); err != nil || count != 50 {
		t.Fatalf("backup copy count %d %v", count, err)
	}
	copyDB.Close()
	memory, err := openMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer memory.Close()
	memoryConn, _ := memory.Conn(ctx)
	defer memoryConn.Close()
	if err := Deserialize(ctx, memoryConn, image); err != nil {
		t.Fatal(err)
	}
	if err := memoryConn.QueryRowContext(ctx, "SELECT count(*) FROM t").Scan(&count); err == nil {
		t.Fatalf("a WAL-format image unexpectedly opened in memory")
	}
	rollbackImage := append([]byte(nil), image...)
	rollbackImage[18], rollbackImage[19] = 1, 1
	second, err := openMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	secondConn, _ := second.Conn(ctx)
	defer secondConn.Close()
	if err := Deserialize(ctx, secondConn, rollbackImage); err != nil {
		t.Fatal(err)
	}
	if err := secondConn.QueryRowContext(ctx, "SELECT count(*) FROM t").Scan(&count); err != nil || count != 50 {
		t.Fatalf("deserialized count %d %v", count, err)
	}
	restored, err := openMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	restoredConn, _ := restored.Conn(ctx)
	defer restoredConn.Close()
	if err := Restore(ctx, restoredConn, filepath.Join(dir, "copy.db")); err != nil {
		t.Fatal(err)
	}
	if err := restoredConn.QueryRowContext(ctx, "SELECT count(*) FROM t").Scan(&count); err != nil || count != 50 {
		t.Fatalf("restored count %d %v", count, err)
	}
}

// TestBusyErrorCode confirms that a contending writer with zero busy timeout yields SQLITE_BUSY via IsBusy.
func TestBusyErrorCode(t *testing.T) {
	ctx := context.Background()
	options := DefaultOptions()
	options.Conn.BusyTimeout = 0
	path := filepath.Join(t.TempDir(), "busy.db")
	first, err := Open(ctx, path, options)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := Open(ctx, path, options)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	first.Exec("CREATE TABLE t(x)")
	tx, err := first.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec("INSERT INTO t VALUES(1)"); err != nil {
		t.Fatal(err)
	}
	_, err = second.Exec("INSERT INTO t VALUES(2)")
	if !IsBusy(err) {
		t.Fatalf("expected SQLITE_BUSY, got %v", err)
	}
	if _, ok := ErrorCode(nil); ok {
		t.Fatal("nil error has no code")
	}
}

// TestWorkDir verifies that WorkDir creates, cleans on reuse, and rejects unsafe directory names.
func TestWorkDir(t *testing.T) {
	base := t.TempDir()
	t.Setenv(WorkDirEnv, base)
	dir, err := WorkDir("1.2")
	if err != nil {
		t.Fatal(err)
	}
	if dir != filepath.Join(base, "1.2") {
		t.Fatalf("dir %s", dir)
	}
	os.WriteFile(filepath.Join(dir, "stale"), []byte("x"), 0o644)
	dir, err = WorkDir("1.2")
	if err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("work dir not wiped: %v", entries)
	}
	for _, bad := range []string{"", "..", "../x", "a/b", ".hidden"} {
		if _, err := WorkDir(bad); err == nil {
			t.Errorf("name %q accepted", bad)
		}
	}
}

// TestRepoRoot verifies that RepoRoot finds sqlite/PIN and rejects a directory outside the checkout.
func TestRepoRoot(t *testing.T) {
	root, err := RepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "sqlite", "PIN")); err != nil {
		t.Fatal(err)
	}
	if _, err := RepoRootFrom(t.TempDir()); err == nil {
		t.Fatal("temp dir is not the repo")
	}
}

// TestFileURIEscapes checks percent-escaping and opening a database in a directory containing spaces, '?', '#', and '%'.
func TestFileURIEscapes(t *testing.T) {
	uri, err := FileURI("/tmp/a b?c#d.db", nil)
	if err != nil {
		t.Fatal(err)
	}
	if uri != "file:/tmp/a%20b%3Fc%23d.db" {
		t.Fatalf("uri %s", uri)
	}
	dir := filepath.Join(t.TempDir(), "odd ?#% name")
	os.MkdirAll(dir, 0o755)
	db, err := OpenDefault(context.Background(), filepath.Join(dir, "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	if _, err := os.Stat(filepath.Join(dir, "x.db")); err != nil {
		t.Fatal(err)
	}
}

// TestLocateCLIRespectsEnvAndPin checks that LocateCLI fails on a missing $SQLITE3_BIN or a wrong source ID.
func TestLocateCLIRespectsEnvAndPin(t *testing.T) {
	ctx := context.Background()
	t.Setenv(CLIEnv, filepath.Join(t.TempDir(), "missing"))
	if _, err := LocateCLI(ctx); err == nil {
		t.Fatal("missing $SQLITE3_BIN accepted")
	}
	fake := filepath.Join(t.TempDir(), "sqlite3")
	os.WriteFile(fake, []byte("#!/bin/sh\necho '2000-01-01 00:00:00 notthepin'\n"), 0o755)
	t.Setenv(CLIEnv, fake)
	if _, err := LocateCLI(ctx); err == nil {
		t.Fatal("wrong source id accepted")
	}
}

// TestLocateCLIFindsPinnedBinary runs a query through the located pinned CLI when one is installed.
func TestLocateCLIFindsPinnedBinary(t *testing.T) {
	cli, err := LocateCLI(context.Background())
	if err != nil {
		t.Skipf("no pinned CLI here: %v", err)
	}
	out, err := cli.Exec(context.Background(), ":memory:", "SELECT 1+1;")
	if err != nil || strings.TrimSpace(out) != "2" {
		t.Fatalf("exec %q %v", out, err)
	}
}

// TestConcurrentOpenOfNewDatabase checks that four goroutines opening the same new file concurrently all succeed.
func TestConcurrentOpenOfNewDatabase(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "race.db")
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			db, err := OpenDefault(ctx, path)
			if err != nil {
				errs <- err
				return
			}
			db.Close()
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
