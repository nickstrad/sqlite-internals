package sqlitex

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
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
	a, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	b, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
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
	memoryConn, err := memory.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
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
	secondConn, err := second.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
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
	restoredConn, err := restored.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
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
	if _, err := first.Exec("CREATE TABLE t(x)"); err != nil {
		t.Fatal(err)
	}
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
	if err := os.WriteFile(filepath.Join(dir, "stale"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
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
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
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
	if err := os.WriteFile(fake, []byte("#!/bin/sh\necho '2000-01-01 00:00:00 notthepin'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
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

// TestCLIExecQuotesAwkwardPaths checks that CLI.Exec handles paths with quotes, spaces, backslashes, and newlines.
func TestCLIExecQuotesAwkwardPaths(t *testing.T) {
	cli, err := LocateCLI(context.Background())
	if err != nil {
		t.Skipf("no pinned CLI here: %v", err)
	}
	for _, name := range []string{`o'brien`, `two words`, `back\slash`, `dq"uote`, "new\nline", `mix ' " \ end`} {
		dir := filepath.Join(t.TempDir(), name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "t.db")
		out, err := cli.Exec(context.Background(), path, "CREATE TABLE t(x); INSERT INTO t VALUES(42); SELECT x FROM t;")
		if err != nil || strings.TrimSpace(out) != "42" {
			t.Fatalf("%q: exec %q %v", name, out, err)
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("%q: %v", name, err)
		}
	}
}

// TestOpenScriptQuoting checks the exact double-quoted dot-command escaping produced by OpenScript.
func TestOpenScriptQuoting(t *testing.T) {
	got := OpenScript(`/a'b/c"d\e`+"\n"+`f.db`, "SELECT 1;")
	want := `.open --normal "/a'b/c\"d\\e\nf.db"` + "\nSELECT 1;"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

// TestMemoryPathNeverCreatesAFile verifies in-memory validation, OpenMemory settings, and that no :memory: file is created on disk.
func TestMemoryPathNeverCreatesAFile(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	t.Chdir(dir)
	if _, err := FileURI(MemoryPath, nil); err == nil {
		t.Fatal("FileURI accepted :memory:")
	}
	if db, err := OpenDefault(ctx, MemoryPath); err == nil {
		db.Close()
		t.Fatal("OpenDefault(:memory:) accepted WAL on an in-memory database")
	}
	options := MemoryOptions()
	options.SingleConnection = false
	if _, err := Open(ctx, MemoryPath, options); err == nil {
		t.Fatal("pooled in-memory database accepted")
	}
	db, err := OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE t(x); INSERT INTO t VALUES(1)"); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRow("SELECT count(*) FROM t").Scan(&count); err != nil || count != 1 {
		t.Fatalf("count %d %v", count, err)
	}
	settings, err := ReadFileSettings(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if settings.JournalMode != JournalMemory || settings.ApplicationID != CourseApplicationID || settings.PageSize != 4096 {
		t.Fatalf("in-memory settings %s", settings)
	}
	if err := WritePinCheck(ctx, io.Discard, db); err != nil {
		t.Fatal(err)
	}
	db.Close()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("in-memory open left files behind: %v", entries)
	}
}

// TestExistingWALDatabaseIsNotConverted ensures opening a WAL database with any rollback mode fails without mutating header bytes 18-19.
func TestExistingWALDatabaseIsNotConverted(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "w.db")
	db, err := OpenDefault(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE t(x)"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	for _, mode := range []string{JournalDelete, JournalTruncate, JournalPersist, JournalMemory, JournalOff} {
		options := DefaultOptions()
		options.File.JournalMode = mode
		if db, err := Open(ctx, path, options); err == nil {
			db.Close()
			t.Fatalf("%s: existing WAL database opened without a mismatch error", mode)
		} else if !strings.Contains(err.Error(), "journal_mode want "+mode+" got wal") {
			t.Fatalf("%s: %v", mode, err)
		}
		header, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if header[18] != 2 || header[19] != 2 {
			t.Fatalf("%s: header bytes 18-19 changed to %d %d", mode, header[18], header[19])
		}
	}
}

// TestExistingRollbackDatabaseIsNotConvertedToWAL ensures rollback databases refuse WAL conversion while allowing switches between rollback modes.
func TestExistingRollbackDatabaseIsNotConvertedToWAL(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "r.db")
	options := DefaultOptions()
	options.File.JournalMode = JournalDelete
	db, err := Open(ctx, path, options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE t(x)"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if db, err := OpenDefault(ctx, path); err == nil {
		db.Close()
		t.Fatal("existing rollback database converted to WAL")
	}
	options.File.JournalMode = JournalTruncate
	db, err = Open(ctx, path, options)
	if err != nil {
		t.Fatalf("switching between rollback modes: %v", err)
	}
	db.Close()
	header, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if header[18] != 1 || header[19] != 1 {
		t.Fatalf("header bytes 18-19 %d %d", header[18], header[19])
	}
}

// TestSubMillisecondBusyTimeoutRejected verifies Validate and Open reject BusyTimeout values that are not whole milliseconds.
func TestSubMillisecondBusyTimeoutRejected(t *testing.T) {
	settings := DefaultConnSettings()
	settings.BusyTimeout = 1500 * time.Microsecond
	if err := settings.Validate(); err == nil {
		t.Fatal("sub-millisecond busy timeout accepted")
	}
	options := DefaultOptions()
	options.Conn.BusyTimeout = 500 * time.Microsecond
	if _, err := Open(context.Background(), filepath.Join(t.TempDir(), "b.db"), options); err == nil || !strings.Contains(err.Error(), "whole number of milliseconds") {
		t.Fatalf("Open: %v", err)
	}
}

// TestReadOnlyOpen confirms Options.ReadOnly allows queries and rejects writes.
func TestReadOnlyOpen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ro.db")
	db, err := OpenDefault(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE t(x); INSERT INTO t VALUES(7)"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	options := DefaultOptions()
	options.ReadOnly = true
	ro, err := Open(ctx, path, options)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	var x int
	if err := ro.QueryRow("SELECT x FROM t").Scan(&x); err != nil || x != 7 {
		t.Fatalf("read %d %v", x, err)
	}
	if _, err := ro.Exec("INSERT INTO t VALUES(8)"); err == nil {
		t.Fatal("write through a read-only handle succeeded")
	}
}

// TestDriverModuleVersionCheck verifies version matching, replace directives, and missing-module detection in checkModuleVersions.
func TestDriverModuleVersionCheck(t *testing.T) {
	if err := CheckDriverModules(); err != nil {
		t.Fatal(err)
	}
	pin := sqlite.Current()
	good := []*debug.Module{{Path: pin.DriverModule, Version: pin.DriverVersion}, {Path: pin.LibcModule, Version: pin.LibcVersion}}
	if err := checkModuleVersions(good, pin); err != nil {
		t.Fatal(err)
	}
	older := []*debug.Module{{Path: pin.DriverModule, Version: "v1.58.0"}, {Path: pin.LibcModule, Version: pin.LibcVersion}}
	var mismatch *DriverModuleMismatchError
	if err := checkModuleVersions(older, pin); !errors.As(err, &mismatch) || mismatch.Got != "v1.58.0" {
		t.Fatalf("older driver: %v", err)
	}
	replaced := []*debug.Module{{Path: pin.DriverModule, Version: pin.DriverVersion, Replace: &debug.Module{Path: "../fork"}}, {Path: pin.LibcModule, Version: pin.LibcVersion}}
	if err := checkModuleVersions(replaced, pin); err == nil {
		t.Fatal("locally replaced driver accepted")
	}
	if err := checkModuleVersions(good[:1], pin); err == nil {
		t.Fatal("missing libc module accepted")
	}
}

// TestBackupStopsOnCancelledContext confirms Backup returns context.Canceled before stepping when ctx is already cancelled.
func TestBackupStopsOnCancelledContext(t *testing.T) {
	db := openTest(t, DefaultOptions())
	if _, err := db.Exec("CREATE TABLE t(x); INSERT INTO t VALUES(1)"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	cancel()
	if _, err := Backup(ctx, conn, filepath.Join(t.TempDir(), "copy.db"), 1, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("backup with a cancelled context: %v", err)
	}
}

// TestUnstampedEmptyDatabaseIsStampedButUsedOneIsNot verifies that an empty WAL file with application_id 0 is stamped while one with a schema is rejected.
func TestUnstampedEmptyDatabaseIsStampedButUsedOneIsNot(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	plain := func(path, statements string) {
		t.Helper()
		db, err := sql.Open("sqlite", "file:"+path)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if _, err := db.Exec(statements); err != nil {
			t.Fatal(err)
		}
	}
	empty := filepath.Join(dir, "empty.db")
	plain(empty, "PRAGMA page_size=4096; PRAGMA journal_mode=wal")
	db, err := OpenDefault(ctx, empty)
	if err != nil {
		t.Fatalf("unstamped empty WAL database: %v", err)
	}
	settings, err := ReadFileSettings(ctx, db)
	db.Close()
	if err != nil || settings.ApplicationID != CourseApplicationID {
		t.Fatalf("settings %s %v", settings, err)
	}
	used := filepath.Join(dir, "used.db")
	plain(used, "PRAGMA page_size=4096; PRAGMA journal_mode=wal; CREATE TABLE t(x)")
	if db, err := OpenDefault(ctx, used); err == nil {
		db.Close()
		t.Fatal("database with a schema and application_id 0 was stamped")
	} else if !strings.Contains(err.Error(), "application_id want") {
		t.Fatal(err)
	}
}

func TestDSNWithVFS(t *testing.T) {
	o := DefaultOptions()
	o.VFS = "unix-none"
	dsn, err := DSN("/x/t.db", o)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(dsn, "vfs=unix-none") || !strings.HasPrefix(dsn, "file:/x/t.db?") {
		t.Fatal(dsn)
	}
	o.VFS = ""
	dsn, err = DSN("/x/t.db", o)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(dsn, "vfs=") {
		t.Fatal(dsn)
	}
	dir := t.TempDir()
	_, err = Open(context.Background(), filepath.Join(dir, "w.db"), Options{File: DefaultFileSettings(), Conn: DefaultConnSettings(), SingleConnection: true, VFS: "unix-dotfile"})
	if err == nil || !strings.Contains(err.Error(), "SQLite reports delete") {
		t.Fatalf("WAL through unix-dotfile: %v", err)
	}
	file := DefaultFileSettings()
	file.JournalMode = JournalDelete
	db, err := Open(context.Background(), filepath.Join(dir, "v.db"), Options{File: file, Conn: DefaultConnSettings(), SingleConnection: true, VFS: "unix-dotfile"})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("CREATE TABLE t(x)"); err != nil {
		t.Fatal(err)
	}
}

func TestDisplayPath(t *testing.T) {
	root, err := RepoRoot()
	if err != nil {
		t.Skip(err)
	}
	if got := DisplayPath(filepath.Join(root, "internal", "sqlitex")); got != "internal/sqlitex" {
		t.Fatal(got)
	}
	outside := t.TempDir()
	resolved, _ := filepath.EvalSymlinks(outside)
	if got := DisplayPath(outside); got != outside && got != resolved {
		t.Fatal(got)
	}
}
