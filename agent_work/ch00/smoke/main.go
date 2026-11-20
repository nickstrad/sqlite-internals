package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
	"github.com/nickstrad/sqlite-internals/internal/trace"
)

const packagePath = "agent_work/ch00/smoke"

var failures []string

func check(ok bool, claim string) {
	status := "ok"
	if !ok {
		status = "FAILED"
		failures = append(failures, claim)
	}
	fmt.Printf("check %s: %s\n", status, claim)
}

func fatal(context string, err error) {
	fmt.Printf("FATAL %s: %v\n", context, err)
	os.Exit(1)
}

func smokeOptions() sqlitex.Options {
	options := sqlitex.DefaultOptions()
	options.SingleConnection = true
	options.Conn.WALAutocheckpoint = 0
	return options
}

func childInsert(path string) {
	runtime.LockOSThread()
	ctx := context.Background()
	db, err := sqlitex.Open(ctx, path, smokeOptions())
	if err != nil {
		fatal("child open", err)
	}
	if _, err := db.Exec("INSERT INTO t(id, v) VALUES(2, 'two')"); err != nil {
		fatal("child insert", err)
	}
	fmt.Println("child: insert committed")
	if err := db.Close(); err != nil {
		fatal("child close", err)
	}
}

func countRows(ctx context.Context, path string) (int, string, error) {
	db, err := sqlitex.Open(ctx, path, smokeOptions())
	if err != nil {
		return 0, "", err
	}
	defer db.Close()
	var count int
	var integrity string
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM t").Scan(&count); err != nil {
		return 0, "", err
	}
	if err := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil {
		return 0, "", err
	}
	return count, integrity, nil
}

func createBase(ctx context.Context, path string) error {
	for _, suffix := range []string{"", "-wal", "-shm"} {
		os.Remove(path + suffix)
	}
	db, err := sqlitex.Open(ctx, path, smokeOptions())
	if err != nil {
		return err
	}
	if _, err := db.Exec("CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT NOT NULL); INSERT INTO t VALUES(1, 'one')"); err != nil {
		db.Close()
		return err
	}
	return db.Close()
}

func isSync(call trace.Syscall) bool {
	return call.Name == "fsync" || call.Name == "fdatasync"
}

func syncSummary(calls []trace.Syscall) string {
	var parts []string
	for _, call := range calls {
		if isSync(call) {
			parts = append(parts, call.Name+"("+call.Path+")")
		}
	}
	return strings.Join(parts, " ")
}

func fileCalls(dir string, result *trace.Result) []trace.Syscall {
	interesting := trace.Named("openat", "open", "pwrite64", "pread64", "fsync", "fdatasync", "ftruncate", "unlink", "unlinkat", "close")
	return trace.RelativeTo(dir, result.Log.Filter(trace.Both(trace.UnderDir(dir), interesting)))
}

func straceParts(ctx context.Context, dir, dbPath string, cli sqlitex.CLI, haveCLI bool) {
	if _, err := trace.StracePath(); err != nil {
		fatal("strace", err)
	}
	command, err := trace.SelfCommand("child-insert", dbPath)
	if err != nil {
		fatal("self command", err)
	}
	if err := createBase(ctx, dbPath); err != nil {
		fatal("create base", err)
	}
	baseline, err := trace.Run(ctx, trace.Options{Command: command})
	if err != nil {
		fatal("strace baseline", err)
	}
	calls := fileCalls(dir, baseline)
	fmt.Printf("baseline insert under strace: exit=%d, %d file syscalls on the database files:\n", baseline.ExitCode, len(calls))
	for _, call := range calls {
		fmt.Println("  ", call)
	}
	syncs := 0
	for _, call := range calls {
		if isSync(call) {
			syncs++
		}
	}
	fmt.Printf("baseline sync calls: %d: %s\n", syncs, syncSummary(calls))
	count, integrity, err := countRows(ctx, dbPath)
	check(err == nil && count == 2 && integrity == "ok", fmt.Sprintf("baseline: rows after the insert = %d (want 2), integrity %s", count, integrity))

	if err := createBase(ctx, dbPath); err != nil {
		fatal("create base", err)
	}
	killed, err := trace.Run(ctx, trace.Options{
		Command:  command,
		Syscalls: []string{"fsync", "fdatasync", "pwrite64", "openat"},
		KillAt:   &trace.KillAt{Syscalls: []string{"fsync", "fdatasync"}, When: 1},
	})
	if err != nil {
		fatal("strace kill", err)
	}
	var last trace.Syscall
	for _, call := range trace.RelativeTo(dir, killed.Log.Calls) {
		if isSync(call) {
			last = call
		}
	}
	fmt.Printf("kill at the first sync: process killed by %s; last sync call: %s\n", killed.Killed, last)
	check(killed.Killed == "SIGKILL" && last.Unfinished, "the child was killed on entry to its first sync call")
	check(!strings.Contains(killed.Stdout, "insert committed"), "the child never reported the commit")
	walWrites := 0
	for _, call := range trace.RelativeTo(dir, killed.Log.Calls) {
		if call.Name == "pwrite64" && call.Path == "smoke.db-wal" {
			walWrites++
		}
	}
	fmt.Printf("WAL writes before the kill: %d\n", walWrites)
	count, integrity, err = countRows(ctx, dbPath)
	check(err == nil && count == 1 && integrity == "ok", fmt.Sprintf("after the kill and reopen: rows = %d (want 1, the insert is lost), integrity %s", count, integrity))

	if !haveCLI {
		return
	}
	if err := createBase(ctx, dbPath); err != nil {
		fatal("create base", err)
	}
	cliRun, err := trace.Run(ctx, trace.Options{
		Command: cli.Command(ctx, sqlitex.CLIRun{}).Args,
		Stdin:   strings.NewReader(sqlitex.OpenScript(dbPath, "PRAGMA synchronous=FULL; PRAGMA wal_autocheckpoint=0; INSERT INTO t(id, v) VALUES(2, 'two');")),
	})
	if err != nil {
		fatal("strace CLI", err)
	}
	cliCalls := fileCalls(dir, cliRun)
	fmt.Printf("INFO: CLI used for the twin preview: %s\n", cli.Path)
	fmt.Printf("twin preview, same insert by the C CLI: sync calls: %s\n", syncSummary(cliCalls))
	goWrites := writeShape(calls)
	cliWrites := writeShape(cliCalls)
	fmt.Printf("twin preview, Go writes:  %s\n", goWrites)
	fmt.Printf("twin preview, CLI writes: %s\n", cliWrites)
	check(goWrites == cliWrites, "the Go driver and the C CLI write the same byte ranges for this insert")
	check(syncTargets(calls) == syncTargets(cliCalls), "the Go driver and the C CLI sync the same files in the same order (sync call name ignored)")
}

func syncTargets(calls []trace.Syscall) string {
	var parts []string
	for _, call := range calls {
		if isSync(call) {
			parts = append(parts, call.Path)
		}
	}
	return strings.Join(parts, " ")
}

func writeShape(calls []trace.Syscall) string {
	var parts []string
	for _, call := range calls {
		if call.Name == "pwrite64" {
			parts = append(parts, fmt.Sprintf("%s@%d+%d", call.Path, call.Offset, call.Length))
		}
	}
	return strings.Join(parts, " ")
}

func main() {
	if len(os.Args) == 3 && os.Args[1] == "child-insert" {
		childInsert(os.Args[2])
		return
	}
	ctx := context.Background()
	dir := sqlitex.MustWorkDir("ch00-smoke")
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		fatal("work dir", err)
	}
	dbPath := filepath.Join(realDir, "smoke.db")
	db, err := sqlitex.Open(ctx, dbPath, smokeOptions())
	if err != nil {
		fatal("open", err)
	}
	sqlitex.MustPinCheck(ctx, db)
	fmt.Printf("INFO: work dir %s\n", realDir)
	fileSettings, err := sqlitex.ReadFileSettings(ctx, db)
	if err != nil {
		fatal("file settings", err)
	}
	connSettings, err := sqlitex.ReadConnSettings(ctx, db)
	if err != nil {
		fatal("conn settings", err)
	}
	fmt.Println("file settings:", fileSettings)
	fmt.Println("connection settings:", connSettings)
	check(fileSettings.JournalMode == sqlitex.JournalWAL, "the database is in WAL mode")
	db.Close()

	cli, cliErr := sqlitex.LocateCLI(ctx)
	haveCLI := cliErr == nil
	if haveCLI {
		fmt.Println(cli.PinLine())
	} else {
		fmt.Println("CLI: unavailable:", cliErr)
		failures = append(failures, "pinned CLI available")
	}

	if trace.Supported {
		straceParts(ctx, realDir, dbPath, cli, haveCLI)
	} else {
		trace.PrintLinuxOnly("strace of the insert, the kill at the first sync and the twin preview", packagePath)
		if err := createBase(ctx, dbPath); err != nil {
			fatal("create base", err)
		}
	}

	if haveCLI {
		vfs, err := trace.RunCLIVFSTrace(ctx, cli, trace.CLITrace{Dir: realDir, DB: "smoke.db", SQL: "SELECT count(*) FROM t;"})
		if err != nil {
			fatal("CLI vfstrace", err)
		}
		calls := trace.RelativeVFS(realDir, vfs.Calls)
		counts := map[string]int{}
		for _, call := range calls {
			counts[call.Method]++
		}
		var methods []string
		for method := range counts {
			methods = append(methods, fmt.Sprintf("%s=%d", method, counts[method]))
		}
		sort.Strings(methods)
		fmt.Printf("CLI -vfstrace of SELECT count(*): %d VFS calls (%s); output %q\n", len(calls), strings.Join(methods, " "), vfs.Output)
		for _, call := range calls {
			if call.Method == "xRead" || call.Method == "xOpen" || call.Method == "xShmMap" {
				fmt.Println("  ", call)
			}
		}
		check(len(vfs.Output) == 1 && strings.TrimSpace(vfs.Output[0]) == fmt.Sprint(expectedRows()), "the CLI reads the rows the Go side left")
	}

	if len(failures) > 0 {
		fmt.Printf("SMOKE FAILED: %d check(s): %s\n", len(failures), strings.Join(failures, "; "))
		os.Exit(1)
	}
	fmt.Println("SMOKE OK")
}

func expectedRows() int {
	if trace.Supported {
		return 2
	}
	return 1
}
