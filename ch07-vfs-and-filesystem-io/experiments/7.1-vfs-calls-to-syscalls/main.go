// Experiment 7.1 runs the pinned sqlite3 C CLI with -vfstrace under Linux
// strace to merge VFS calls and kernel syscalls into a single ordered stream.
// At SQLite's default mmap_size=0, it verifies that every xRead/xWrite/xSync
// maps to the expected pread64, pwrite64, or fsync/fdatasync syscall, that no
// byte-moving syscall on database/sidecar files escapes the VFS, that directory
// syncs happen inside the first xSync of a new journal or WAL, and that
// -wal/-shm are created at first use and removed at last close.
//
// Run from the repo root (Linux; on macOS, use docker compose run --rm lab):
//
//	go run ./ch07-vfs-and-filesystem-io/experiments/7.1-vfs-calls-to-syscalls
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
	"github.com/nickstrad/sqlite-internals/internal/trace"
)

const packagePath = "ch07-vfs-and-filesystem-io/experiments/7.1-vfs-calls-to-syscalls"

const dbName = "t.db"

var statements = []struct {
	label string
	sql   string
}{
	{"set WAL mode", "PRAGMA journal_mode=WAL;"},
	{"set synchronous FULL", "PRAGMA synchronous=FULL;"},
	{"create a table", "CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT NOT NULL);"},
	{"commit one insert", "INSERT INTO t(id, v) VALUES(1, 'one');"},
	{"checkpoint", "PRAGMA wal_checkpoint(TRUNCATE);"},
	{"close", ""},
}

var unixFunction = map[string]string{
	"xOpen":                  "unixOpen",
	"xDelete":                "unixDelete",
	"xAccess":                "unixAccess",
	"xFullPathname":          "unixFullPathname",
	"xRandomness":            "unixRandomness",
	"xSleep":                 "unixSleep",
	"xCurrentTime":           "unixCurrentTime",
	"xCurrentTimeInt64":      "unixCurrentTimeInt64",
	"xClose":                 "unixClose",
	"xRead":                  "unixRead",
	"xWrite":                 "unixWrite",
	"xTruncate":              "unixTruncate",
	"xSync":                  "unixSync",
	"xFileSize":              "unixFileSize",
	"xLock":                  "unixLock",
	"xUnlock":                "unixUnlock",
	"xCheckReservedLock":     "unixCheckReservedLock",
	"xFileControl":           "unixFileControl",
	"xSectorSize":            "unixSectorSize",
	"xDeviceCharacteristics": "unixDeviceCharacteristics",
	"xShmMap":                "unixShmMap",
	"xShmLock":               "unixShmLock",
	"xShmBarrier":            "unixShmBarrier",
	"xShmUnmap":              "unixShmUnmap",
	"xFetch":                 "unixFetch",
	"xUnfetch":               "unixUnfetch",
}

// tracedSyscalls extends trace.DefaultSyscalls with stat and access families so
// xAccess (os_unix.c:6881-6903), xFileSize, and xFullPathname show their kernel
// calls. fchown is omitted because unixOpen only calls it when running as root.
var tracedSyscalls = append(append([]string(nil), trace.DefaultSyscalls...), "fstat", "newfstatat", "statx", "stat", "lstat", "access", "faccessat", "faccessat2")

// byteMovingSyscalls lists every traced canonical syscall that transfers file
// bytes, used to verify no database/journal/WAL/shm byte I/O escapes the VFS.
var byteMovingSyscalls = []string{"pread64", "pwrite64", "read", "write", "writev"}

var failures []string

func check(ok bool, claim string) {
	status := "ok"
	if !ok {
		status = "FAILED"
		failures = append(failures, claim)
	}
	fmt.Printf("check %s: %s\n", status, claim)
}

func fatal(what string, err error) {
	fmt.Printf("FATAL %s: %v\n", what, err)
	os.Exit(1)
}

func script() string {
	var b strings.Builder
	for i, s := range statements {
		fmt.Fprintf(&b, ".print STEP %d %s\n", i+1, s.label)
		if s.sql != "" {
			b.WriteString(s.sql + "\n")
		}
	}
	return b.String()
}

func printStatements() {
	fmt.Println("statements, one CLI session (.open --normal t.db first):")
	for i, s := range statements {
		if s.sql == "" {
			fmt.Printf("  STEP %d %s: end of input, the CLI closes the database\n", i+1, s.label)
			continue
		}
		fmt.Printf("  STEP %d %s: %s\n", i+1, s.label, s.sql)
	}
}

func main() {
	ctx := context.Background()
	sqlitex.MustPinCheckMemory(ctx)
	dir := sqlitex.MustWorkDir("7.1")
	fmt.Printf("INFO: work dir %s\n", sqlitex.DisplayPath(dir))
	cli := sqlitex.MustLocateCLI(ctx)
	printStatements()
	request := trace.CLITrace{Dir: dir, DB: filepath.Join(dir, dbName), SQL: script()}

	if !trace.Supported {
		portable(ctx, cli, request, dir)
		trace.PrintLinuxOnly("strace of the CLI (VFS calls merged with syscalls)", packagePath)
		finish()
		return
	}
	merged, result, err := trace.RunCLIMerged(ctx, cli, request, trace.Options{Syscalls: tracedSyscalls})
	if errors.Is(err, trace.ErrPtraceDenied) {
		fatal("strace", fmt.Errorf("%w (run: docker compose run --rm lab go run ./%s)", err, packagePath))
	}
	if err != nil {
		fatal("strace", err)
	}
	if result.ExitCode != 0 {
		fatal("cli", fmt.Errorf("exit %d: %s", result.ExitCode, result.Stderr))
	}
	analyze(merged.Relative(dir), dir)
	finish()
}

func finish() {
	if len(failures) > 0 {
		fmt.Printf("FAIL: %d claim(s) did not hold\n", len(failures))
		os.Exit(1)
	}
	fmt.Println("ALL CHECKS PASSED")
}

func portable(ctx context.Context, cli sqlitex.CLI, request trace.CLITrace, dir string) {
	vt, err := trace.RunCLIVFSTrace(ctx, cli, request)
	if err != nil {
		fatal("vfstrace", err)
	}
	calls := trace.RelativeVFS(dir, vt.Calls)
	fmt.Printf("VFS calls (CLI -vfstrace only, %d, xRandomness left out):\n", len(calls))
	for _, c := range calls {
		if c.Method == "xRandomness" {
			continue
		}
		fmt.Printf("  %s\n", c.String())
	}
	walOpen, walDelete, shmDelete := closeLifecycle(calls)
	check(walOpen, "the -wal file is opened through xOpen")
	check(walDelete && shmDelete, "at the last close the -wal is deleted (xDelete) and the -shm is unmapped with delFlag=1")
}

// closeLifecycle checks the VFS-only trace (used on non-Linux hosts) for WAL
// open, WAL deletion, and shm unmap with delFlag=1 (vfstrace.c:843).
func closeLifecycle(calls []trace.VFSCall) (walOpen, walDelete, shmDelete bool) {
	for _, c := range calls {
		switch {
		case c.Method == "xOpen" && c.File == dbName+"-wal":
			walOpen = true
		case c.Method == "xDelete" && strings.HasSuffix(c.File, dbName+"-wal"):
			walDelete = true
		case c.Method == "xShmUnmap" && slices.Contains(c.Args, "delFlag=1"):
			shmDelete = true
		}
	}
	return walOpen, walDelete, shmDelete
}

func onDisk(s trace.Syscall) bool {
	return s.Path == "." || s.Path == dbName || strings.HasPrefix(s.Path, dbName+"-")
}

func syscallLine(s trace.Syscall) string {
	return s.Canonical().String()
}

func eventLine(e trace.Event) string {
	switch e.Kind {
	case trace.EventVFS:
		return "VFS " + e.VFS.String()
	case trace.EventSyscall:
		if e.Enclosed >= 0 {
			return "    SYS " + syscallLine(*e.Syscall)
		}
		return "SYS " + syscallLine(*e.Syscall)
	case trace.EventOutput:
		return "OUT " + e.Output
	}
	return "?"
}

func analyze(m *trace.Merged, dir string) {
	relevant := onDisk
	var kept []int
	for i, e := range m.Events {
		if e.Kind == trace.EventSyscall && !relevant(*e.Syscall) {
			continue
		}
		kept = append(kept, i)
	}
	vfsCount, sysCount := 0, 0
	for _, i := range kept {
		switch m.Events[i].Kind {
		case trace.EventVFS:
			vfsCount++
		case trace.EventSyscall:
			sysCount++
		}
	}
	fmt.Println()
	fmt.Printf("== merged stream: %d VFS calls and %d file syscalls in one order ==\n", vfsCount, sysCount)
	fmt.Println("(VFS = a line of the CLI's -vfstrace; indented SYS = a syscall made while that VFS call ran;")
	fmt.Println(" unindented SYS = a syscall outside any printed VFS call; OUT = CLI output;")
	fmt.Println(" names normalized: open = open/openat, sync = fsync/fdatasync, stat = stat family, unlink = unlink/unlinkat)")
	randomness := 0
	for position, i := range kept {
		e := m.Events[i]
		// Omit xRandomness from the printed stream so evidence matches across
		// CLI builds that seed SQLite's PRNG at different points.
		if e.Kind == trace.EventVFS && e.VFS.Method == "xRandomness" {
			randomness++
			fmt.Printf("INFO: xRandomness at stream line %d: %s\n", position+1, e.VFS.String())
			continue
		}
		fmt.Println(eventLine(e))
	}
	fmt.Printf("(xRandomness: %d call(s), left out of the stream because where SQLite first asks the VFS for random bytes differs between CLI builds; seen: an installed CLI right after opening the database, a ./configure build of the pin right before the first journal header, whose checksum nonce is random)\n", randomness)

	within := func(i int) []trace.Syscall {
		var out []trace.Syscall
		for _, s := range m.SyscallsWithin(i) {
			if relevant(s) {
				out = append(out, s.Canonical())
			}
		}
		return out
	}
	// before collects unenclosed file syscalls immediately preceding an xOpen
	// event, because vfstraceOpen calls the underlying xOpen before printing
	// its trace line (vfstrace.c:890-893).
	before := func(i int) []trace.Syscall {
		var out []trace.Syscall
		for j := i - 1; j >= 0; j-- {
			e := m.Events[j]
			if e.Kind == trace.EventVFS {
				break
			}
			if e.Kind == trace.EventSyscall && e.Enclosed < 0 && relevant(*e.Syscall) {
				out = append([]trace.Syscall{e.Syscall.Canonical()}, out...)
			}
		}
		return out
	}

	type methodStats struct {
		calls    int
		syscalls map[string]int
	}
	stats := map[string]*methodStats{}
	writesOK, readsOK := 0, 0
	var writeProblems, readProblems, syncProblems []string
	var dirSyncs []string
	syncCalls := 0
	var walOpenedWithCreate, shmOpenedInMap, walUnlinkedInDelete, shmUnlinkedInUnmap bool
	syncsPerFile := map[string]int{}
	for i, e := range m.Events {
		if e.Kind != trace.EventVFS {
			continue
		}
		c := e.VFS
		calls := within(i)
		if c.Method == "xOpen" {
			calls = before(i)
		}
		st := stats[c.Method]
		if st == nil {
			st = &methodStats{syscalls: map[string]int{}}
			stats[c.Method] = st
		}
		st.calls++
		for _, s := range calls {
			st.syscalls[s.Name]++
		}
		switch c.Method {
		case "xWrite":
			if len(calls) == 1 && calls[0].Name == "pwrite64" && calls[0].Path == c.File && calls[0].Offset == c.Offset && calls[0].Length == c.Amount && calls[0].Result == c.Amount {
				writesOK++
			} else {
				writeProblems = append(writeProblems, fmt.Sprintf("%s encloses %v", c.String(), calls))
			}
		case "xRead":
			if len(calls) == 1 && calls[0].Name == "pread64" && calls[0].Path == c.File && calls[0].Offset == c.Offset && calls[0].Length == c.Amount {
				readsOK++
			} else {
				readProblems = append(readProblems, fmt.Sprintf("%s encloses %v", c.String(), calls))
			}
		case "xSync":
			syncCalls++
			own := 0
			for _, s := range calls {
				if s.Name != "sync" {
					continue
				}
				if s.Path == c.File {
					own++
					syncsPerFile[c.File]++
				} else if s.Path == "." {
					dirSyncs = append(dirSyncs, fmt.Sprintf("inside xSync(%s) number %d", c.File, syncsPerFile[c.File]))
				}
			}
			if own != 1 {
				syncProblems = append(syncProblems, fmt.Sprintf("%s encloses %d syncs of its own file", c.String(), own))
			}
		case "xOpen":
			if c.File == dbName+"-wal" {
				for _, s := range calls {
					if s.Name == "open" && s.Path == dbName+"-wal" && strings.Contains(s.Flags, "O_CREAT") {
						walOpenedWithCreate = true
					}
				}
			}
		case "xShmMap":
			for _, s := range calls {
				if s.Name == "open" && s.Path == dbName+"-shm" && strings.Contains(s.Flags, "O_CREAT") {
					shmOpenedInMap = true
				}
			}
		case "xDelete":
			for _, s := range calls {
				if s.Name == "unlink" && s.Path == dbName+"-wal" {
					walUnlinkedInDelete = true
				}
			}
		case "xShmUnmap":
			for _, s := range calls {
				if s.Name == "unlink" && s.Path == dbName+"-shm" {
					shmUnlinkedInUnmap = true
				}
			}
		}
	}

	var outsideIO []string
	for _, i := range kept {
		e := m.Events[i]
		if e.Kind != trace.EventSyscall {
			continue
		}
		name := trace.CanonicalName(e.Syscall.Name)
		if !slices.Contains(byteMovingSyscalls, name) {
			continue
		}
		if e.Enclosed < 0 {
			outsideIO = append(outsideIO, syscallLine(*e.Syscall)+" (outside any VFS call)")
			continue
		}
		method := m.Events[e.Enclosed].VFS.Method
		if method != "xRead" && method != "xWrite" && method != "xShmMap" {
			outsideIO = append(outsideIO, syscallLine(*e.Syscall)+" inside "+method)
		}
	}

	fmt.Println()
	fmt.Println("== each VFS method, the os_unix.c function behind it, and the syscalls seen while it ran ==")
	fmt.Println("(xOpen: syscalls just before its line, because vfstrace prints xOpen after the real open returns)")
	methods := make([]string, 0, len(stats))
	for method := range stats {
		methods = append(methods, method)
	}
	sort.Strings(methods)
	for _, method := range methods {
		st := stats[method]
		names := make([]string, 0, len(st.syscalls))
		for name := range st.syscalls {
			names = append(names, name)
		}
		sort.Strings(names)
		parts := make([]string, len(names))
		for i, name := range names {
			parts[i] = fmt.Sprintf("%s x%d", name, st.syscalls[name])
		}
		seen := strings.Join(parts, ", ")
		if seen == "" {
			seen = "no file syscall"
		}
		fmt.Printf("  %-24s %3d call(s)  %-26s -> %s\n", method, st.calls, unixFunction[method], seen)
	}

	fmt.Println()
	fmt.Println("== syncs ==")
	files := make([]string, 0, len(syncsPerFile))
	for f := range syncsPerFile {
		files = append(files, f)
	}
	sort.Strings(files)
	for _, f := range files {
		fmt.Printf("  %s: %d sync(s), each inside one xSync of that file\n", f, syncsPerFile[f])
	}
	fmt.Printf("  directory (.): %d sync(s), no VFS call names the directory:\n", len(dirSyncs))
	for _, d := range dirSyncs {
		fmt.Printf("    %s\n", d)
	}

	fmt.Println()
	entries, err := os.ReadDir(dir)
	if err != nil {
		fatal("read dir", err)
	}
	var left []string
	for _, e := range entries {
		left = append(left, e.Name())
	}
	fmt.Printf("files left in the directory after the CLI exits: %s\n", strings.Join(left, " "))
	fmt.Println()

	check(stats["xWrite"] != nil && len(writeProblems) == 0, fmt.Sprintf("each of the %d xWrite calls made exactly one pwrite64 to the same file, offset and length", writesOK))
	for _, p := range writeProblems {
		fmt.Printf("  %s\n", p)
	}
	check(stats["xRead"] != nil && len(readProblems) == 0, fmt.Sprintf("each of the %d xRead calls made exactly one pread64 of the same file, offset and length", readsOK))
	for _, p := range readProblems {
		fmt.Printf("  %s\n", p)
	}
	check(len(outsideIO) == 0, "every pread64/pwrite64/read/write/writev on the database, journal, WAL and shm files happened inside xRead, xWrite or xShmMap")
	for _, p := range outsideIO {
		fmt.Printf("  %s\n", p)
	}
	check(syncCalls > 0 && len(syncProblems) == 0, fmt.Sprintf("each of the %d xSync calls made exactly one fsync/fdatasync of its own file", syncCalls))
	for _, p := range syncProblems {
		fmt.Printf("  %s\n", p)
	}
	check(len(dirSyncs) == 2 && strings.Contains(dirSyncs[0], "-journal) number 1") && strings.Contains(dirSyncs[1], "-wal) number 1"), "the directory is synced twice, inside the first xSync of the new journal and of the new WAL (unixSync's one-time directory sync)")
	check(walOpenedWithCreate, "the -wal file is created (O_CREAT) at its first use, by xOpen")
	check(shmOpenedInMap, "the -shm file is created inside the first xShmMap")
	check(walUnlinkedInDelete && shmUnlinkedInUnmap, "at the last close xDelete unlinks the -wal and xShmUnmap unlinks the -shm")
	check(slices.Equal(left, []string{dbName}), "only the database file is left after the CLI exits")
}
