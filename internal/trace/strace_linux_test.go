//go:build linux

package trace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
)

const helperEnv = "TRACE_TEST_HELPER_DB"

// TestMain dispatches into helperWriter when re-invoked as a traced child process.
func TestMain(m *testing.M) {
	if path := os.Getenv(helperEnv); path != "" {
		runtime.LockOSThread()
		helperWriter(path)
		return
	}
	os.Exit(m.Run())
}

func helperWriter(path string) {
	ctx := context.Background()
	options := sqlitex.DefaultOptions()
	options.SingleConnection = true
	db, err := sqlitex.Open(ctx, path, options)
	if err != nil {
		fmt.Println("open:", err)
		os.Exit(2)
	}
	if _, err := db.Exec("CREATE TABLE t(id INTEGER PRIMARY KEY)"); err != nil {
		fmt.Println("create:", err)
		os.Exit(2)
	}
	for i := 1; i <= 3; i++ {
		if _, err := db.Exec("INSERT INTO t VALUES(?)", i); err != nil {
			fmt.Println("insert:", err)
			os.Exit(2)
		}
		fmt.Println("ack", i)
	}
	db.Close()
}

func requireStrace(t *testing.T) {
	t.Helper()
	if _, err := StracePath(); err != nil {
		t.Skip(err)
	}
}

// TestRunGoProcessUnderStrace traces the Go helper child and checks for WAL pwrite64 and sync syscalls.
func TestRunGoProcessUnderStrace(t *testing.T) {
	requireStrace(t)
	dir := t.TempDir()
	command, err := SelfCommand("-test.run=^$")
	if err != nil {
		t.Fatal(err)
	}
	result, err := Run(context.Background(), Options{
		Command: command,
		Env:     append(os.Environ(), helperEnv+"="+filepath.Join(dir, "g.db")),
	})
	if err != nil {
		skipIfPtraceDenied(t, err)
	}
	if result.ExitCode != 0 || result.Killed != "" {
		t.Fatalf("exit %d killed %q stderr %s", result.ExitCode, result.Killed, result.Stderr)
	}
	calls := RelativeTo(dir, result.Log.Filter(UnderDir(dir)))
	var syncs, walWrites int
	for _, call := range calls {
		switch {
		case call.Name == "fsync" || call.Name == "fdatasync":
			syncs++
		case call.Name == "pwrite64" && call.Path == "g.db-wal":
			walWrites++
		}
	}
	if syncs == 0 || walWrites == 0 {
		t.Fatalf("syncs %d wal writes %d in:\n%s", syncs, walWrites, result.Text)
	}
	if !strings.Contains(result.Stdout, "ack 3") {
		t.Fatalf("stdout %q", result.Stdout)
	}
}

// TestKillAtFirstSync verifies that strace SIGKILL injection stops the writer on entry to its first sync.
func TestKillAtFirstSync(t *testing.T) {
	requireStrace(t)
	dir := t.TempDir()
	command, _ := SelfCommand("-test.run=^$")
	result, err := Run(context.Background(), Options{
		Command:  command,
		Env:      append(os.Environ(), helperEnv+"="+filepath.Join(dir, "g.db")),
		Syscalls: []string{"fsync", "fdatasync"},
		KillAt:   &KillAt{Syscalls: []string{"fsync", "fdatasync"}, When: 1},
	})
	if err != nil {
		skipIfPtraceDenied(t, err)
	}
	if result.Killed != "SIGKILL" || result.ExitCode != -1 {
		t.Fatalf("killed %q exit %d", result.Killed, result.ExitCode)
	}
	last := result.Log.Calls[len(result.Log.Calls)-1]
	if !last.Unfinished || (last.Name != "fsync" && last.Name != "fdatasync") {
		t.Fatalf("last call %+v", last)
	}
	if strings.Contains(result.Stdout, "ack") {
		t.Fatalf("writer acknowledged a commit before the first sync: %q", result.Stdout)
	}
}

// TestKillAtRejectsBadPoint ensures Run rejects a KillAt with When < 1 or no syscalls.
func TestKillAtRejectsBadPoint(t *testing.T) {
	requireStrace(t)
	_, err := Run(context.Background(), Options{Command: []string{"true"}, KillAt: &KillAt{When: 0}})
	if err == nil {
		t.Fatal("expected an error for an empty kill point")
	}
}

// TestRunCLIMergedLive runs the pinned CLI under strace and verifies that WAL xSync encloses fsync or fdatasync.
func TestRunCLIMergedLive(t *testing.T) {
	requireStrace(t)
	ctx := context.Background()
	cli, err := sqlitex.LocateCLI(ctx)
	if err != nil {
		t.Skip(err)
	}
	dir := t.TempDir()
	merged, _, err := RunCLIMerged(ctx, cli, CLITrace{Dir: dir, DB: "m.db", SQL: "PRAGMA journal_mode=wal; CREATE TABLE t(x); INSERT INTO t VALUES(1);"}, Options{})
	if err != nil {
		skipIfPtraceDenied(t, err)
	}
	foundSync := false
	for i, event := range merged.Events {
		if event.Kind == EventVFS && event.VFS.Method == "xSync" && event.VFS.File == "m.db-wal" {
			within := merged.SyscallsWithin(i)
			if len(within) == 0 || (within[0].Name != "fsync" && within[0].Name != "fdatasync") {
				t.Fatalf("xSync of the WAL encloses %+v", within)
			}
			foundSync = true
		}
	}
	if !foundSync {
		t.Fatal("no xSync of the WAL in the merged stream")
	}
}

func skipIfPtraceDenied(t *testing.T, err error) {
	t.Helper()
	if errors.Is(err, ErrPtraceDenied) {
		t.Skipf("strace unusable here: %v", err)
	}
	t.Fatal(err)
}
