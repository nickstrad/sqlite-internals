# Chapter 1 — Architecture and execution path

| Pin field | Value |
| --- | --- |
| SQLite release | `3.53.4` (`2026-07-24 19:02:57 UTC`) |
| Git tag | `version-3.53.4` |
| Git commit (`github.com/sqlite/sqlite`) | `b09c88c14082339b66c7b7158d609a771e64ca69` |
| Fossil check-in (`sqlite_source_id()`) | `bf7c7f30031888f4e796e429ab3978879485813aaca6f641c7b33e4e09459bcc` |
| Go driver | `modernc.org/sqlite v1.60.1` (`modernc.org/libc v1.77.1`) |
| Copied C sources in this directory (`SOURCES.sha256`) | `prepare.c`, `vdbeapi.c`, `vdbeaux.c`, `os.c` |
| Linked C sources (in the pinned tree, not copied) | `vdbe.c` |

> **How to run the experiments**
>
> - **Prerequisites:** Go 1.26 (`go.mod` specifies `go 1.26.0`). On Linux, `strace` is required for syscall tracing; on macOS, use the Docker `lab` container below.
> - **Fetch the pinned SQLite source tree** into `_src/sqlite-3.53.4` (or point `SQLITE_SRC` at an existing checkout of tag `version-3.53.4`):
>   ```text
>   go run ./cmd/course fetch-source
>   ```
> - **Build the pinned C tools** (`sqlite3`, `showwal`, `dbhash`) into `_tools/bin/` for experiments that invoke the C CLI:
>   ```text
>   go run ./cmd/course build-tools
>   ```
> - **Run an experiment** from the repository root:
>   ```text
>   go run ./ch01-architecture-and-execution-path/experiments/1.1-pin-check
>   ```
>   Scratch databases are written under `_work/`, which is git-ignored and safe to delete.
> - **Linux-only experiments on macOS:** Experiments marked **Linux only** (in this chapter, the syscall and `-vfstrace` sections of Experiment 1.2) skip `strace` on non-Linux hosts and run in full inside the repository's Linux container:
>   ```text
>   docker compose run --rm lab go run ./ch01-architecture-and-execution-path/experiments/1.2-one-insert
>   ```
>   All syscall traces, VFS traces, and timings in this chapter were measured on Linux (`linux/amd64`, Go 1.26.8, `strace` 6.8, ext4). macOS (`darwin/arm64` and `darwin/amd64`) was cross-built and vetted only; the non-Linux fallback path of Experiment 1.2 was not run on macOS.

---

## 1. Model

SQLite is an embedded library, not a separate database server process. A Go program opens a database file directly inside its own address space and drives SQLite through function calls. Every SQL statement crosses six internal layers before bytes reach the operating system's filesystem.

```text
  Go program (database/sql + modernc.org/sqlite)
        |
        |  sqlite3_prepare_v2 / sqlite3_step / sqlite3_finalize
        v
  +---------------------------------------------------------------+
  | 1. Interface       C API entry points, connection mutexes     |
  +---------------------------------------------------------------+
        | (prepare)                      | (step)
        v                                |
  +--------------------------------+     |
  | 2. SQL Compiler                |     |
  |    Tokenizer -> Parser ->      |     |
  |    Query Planner -> Code Gen   |     |
  +--------------------------------+     |
        | emits bytecode                 |
        v                                v
  +---------------------------------------------------------------+
  | 3. VDBE (Virtual Database Engine)                             |
  |    Register-based bytecode interpreter (opcodes, cursors)     |
  +---------------------------------------------------------------+
        | sqlite3BtreeBeginTrans / sqlite3BtreeInsert / Commit
        v
  +---------------------------------------------------------------+
  | 4. B-tree                                                     |
  |    Tables and indexes laid out as trees of fixed-size pages   |
  +---------------------------------------------------------------+
        | sqlite3PagerSharedLock / Write / CommitPhaseOne/Two
        v
  +---------------------------------------------------------------+
  | 5. Pager + Page Cache  <--- WAL (-wal, -shm) or -journal      |
  |    Fixed-size page cache, atomic commits, crash recovery      |
  +---------------------------------------------------------------+
        | sqlite3OsRead / Write / Sync / Lock
        v
  +---------------------------------------------------------------+
  | 6. VFS (Virtual File System)                                  |
  |    OS abstraction (os_unix.c: pread64, pwrite64, fsync, fcntl)|
  +---------------------------------------------------------------+
        |
        v
  Operating System Kernel & Filesystem (.db, .db-wal, .db-shm, .db-journal)
```

Each layer owns a distinct job:

1. **Interface.** The public C functions (`sqlite3_prepare_v2`, `sqlite3_step`, `sqlite3_finalize`). They take the database connection's mutex, manage prepared statements, and translate internal status codes into return values.
2. **SQL Compiler.** The tokenizer splits SQL text into tokens; the parser checks grammar; the query planner chooses table and index scan order; the code generator emits a bytecode program. This course names the compiler and does not read its C source. A platform managing many database files cannot observe or replace the compiler, and no state-capture method depends on it.
3. **VDBE (Virtual Database Engine).** A register-based virtual machine that executes the compiled bytecode one instruction (called an **opcode**) at a time. A prepared statement (`sqlite3_stmt`) is a `Vdbe` struct holding its program, registers, and table cursors.
4. **B-tree.** Organizes every table and index as a balanced tree of fixed-size blocks called **pages** (4,096 bytes each by default, numbered starting at 1). Page 1 begins with a 100-byte database header containing persistent file settings and counters such as the **schema cookie** (incremented whenever tables or indexes change) and the **file change counter** (updated when the database is modified). Each table row is keyed by a 64-bit signed integer called a **rowid**. When a row is larger than one page can hold locally, its trailing bytes spill onto linked **overflow pages**; when pages are emptied by deletes, SQLite records them on a **freelist** for reuse.
5. **Pager and Page Cache.** Reads pages into memory, tracks which pages were modified ("dirty" pages), enforces locking between connections, and makes transactions atomic (all-or-nothing) through one of two logging mechanisms:
   - **Rollback journal (`journal_mode=delete`).** Before modifying a page in the database file, the pager copies the page's original bytes into a sibling `-journal` file and flushes it to durable storage. If the process dies mid-commit, the next opener reads the `-journal` and restores the original pages. Deleting the `-journal` file is the commit point.
   - **Write-ahead log (`journal_mode=wal`).** Instead of overwriting the main `.db` file during a commit, the pager appends each modified page to a sibling `-wal` file as a **frame** (a 24-byte frame header followed by the 4,096-byte page image) and indexes those frames in a shared-memory file (`-shm`). A transaction commits when its final frame records the database's new page count. Later, a **checkpoint** copies committed frames from `-wal` back into the main `.db` file.
6. **VFS (Virtual File System).** A table of function pointers (`xRead`, `xWrite`, `xSync`, `xLock`, `xShmLock`, and so on) separating SQLite's pager from the operating system. On Unix, the default VFS (`os_unix.c`) translates VFS calls into kernel system calls: `pread64` (positioned read), `pwrite64` (positioned write), `fsync` or `fdatasync` (flush dirty file data to durable storage), and `fcntl` (advisory **POSIX locks** on byte ranges of a file).

A platform holding thousands of SQLite files touches only two boundaries in that diagram: **connections** above the pager (SQL statements, transactions, per-connection settings) and **files** below the pager (the `.db` file and its sidecars on disk).

**WAL mode as the platform default.** Throughout this course, WAL mode (`journal_mode=wal`) is the default because it allows concurrent readers alongside a writer and turns committed state into an append-only stream of page frames. This chapter departs from WAL mode in two places, both on purpose:

- Experiment 1.2 runs the same `INSERT` in rollback mode (`journal_mode=delete`) and then in WAL mode so the reader can see commits move from overwriting `t.db` to appending frames to `t.db-wal`.
- Experiment 1.3 builds its canonical database in WAL mode and runs rollback-mode contrast variants to explain how the header change counter and transaction batching behave differently in the two modes.

**The Rebuild Ladder (rung R0).** Every chapter in this course advances a single operational goal: capture a database's durable state deterministically and rebuild a working copy that is provably equal to the original. This chapter builds rung **R0 (Recipe)**: a seeded, deterministic workload (`internal/recipe`) whose state after every numbered step *k* has a known SHA-256 hash. Every later chapter checks its snapshots, WAL replays, crash recoveries, and fleet rebuilds against those hashes.

---

## 2. Question

When a Go program commits one `INSERT`, which layer does what between `sqlite3_prepare_v2` and the bytes on disk, and which of those layers can a platform see from outside the library?

Before tracing that `INSERT`, the course must verify the exact SQLite builds it runs. Every experiment in this chapter imports two shared packages introduced by the foundation chunk:

- [`internal/sqlitex`](../internal/sqlitex/): opens SQLite through `modernc.org/sqlite` with explicit file and connection settings ([`settings.go`](../internal/sqlitex/settings.go), [`open.go`](../internal/sqlitex/open.go)), checks `sqlite_source_id()` against `sqlite/PIN` via `MustPinCheck` and `MustPinCheckMemory` ([`pin.go`](../internal/sqlitex/pin.go)), manages `_work/N.M/` scratch directories via `MustWorkDir` and `WorkBase` ([`workdir.go`](../internal/sqlitex/workdir.go)), and locates and runs the pinned `sqlite3` C CLI via `LocateCLI` and `OpenScript` ([`cli.go`](../internal/sqlitex/cli.go)).
- [`internal/trace`](../internal/trace/): runs a child command under Linux `strace` and parses its syscalls ([`strace.go`](../internal/trace/strace.go), [`strace_linux.go`](../internal/trace/strace_linux.go)), merges the CLI's `-vfstrace` output with `strace` into one ordered stream via `RunCLIMerged` ([`merge.go`](../internal/trace/merge.go), [`vfs.go`](../internal/trace/vfs.go)), and prints the Docker fallback message on non-Linux hosts via `Supported` and `PrintLinuxOnly` ([`linuxonly.go`](../internal/trace/linuxonly.go)).

### Experiment 1.1 — Pin check (`1.1-pin-check`)

- **Claim:** Both SQLite builds used by the course—the `modernc.org/sqlite` Go driver and the `sqlite3` C CLI—are SQLite 3.53.4 at the pinned Fossil check-in, include the required page-inspection virtual tables (`sqlite_dbpage`, `dbstat`) and upstream's WAL sync default (`DEFAULT_WAL_SYNCHRONOUS=2`, meaning `FULL`), and agree on all 15 listed file-format and I/O compile options.
- **Evidence form:** `EXACT` (all lines except `INFO:` lines must match identically on every run).

<!-- include: experiments/1.1-pin-check/main.go -->
```go
// Experiment 1.1 verifies that both SQLite builds used by the course—the
// modernc.org/sqlite Go driver and the pinned sqlite3 C CLI—run the pinned
// SQLite 3.53.4 check-in and share the required compile-time options.
//
// Run from the repo root:
//
//	go run ./ch01-architecture-and-execution-path/experiments/1.1-pin-check
package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strings"

	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
	"github.com/nickstrad/sqlite-internals/sqlite"
)

// requiredOptions lists the compile options later labs depend on: sqlite_dbpage
// and dbstat virtual tables for page inspection, and upstream's FULL sync
// default in WAL mode (DEFAULT_WAL_SYNCHRONOUS=2).
var requiredOptions = []string{
	"ENABLE_DBPAGE_VTAB",
	"ENABLE_DBSTAT_VTAB",
	"DEFAULT_WAL_SYNCHRONOUS=2",
}

// fileFormatAndIOOptions lists compile options that govern on-disk format,
// cache sizing, mmap limits, or sync defaults; both builds must agree on all
// of them.
var fileFormatAndIOOptions = []string{
	"DEFAULT_AUTOVACUUM",
	"DEFAULT_CACHE_SIZE",
	"DEFAULT_FILE_FORMAT",
	"DEFAULT_JOURNAL_SIZE_LIMIT",
	"DEFAULT_MMAP_SIZE",
	"DEFAULT_PAGE_SIZE",
	"DEFAULT_PCACHE_INITSZ",
	"DEFAULT_SECTOR_SIZE",
	"DEFAULT_SYNCHRONOUS",
	"DEFAULT_WAL_AUTOCHECKPOINT",
	"DEFAULT_WAL_SYNCHRONOUS",
	"DIRECT_OVERFLOW_READ",
	"MAX_MMAP_SIZE",
	"MAX_PAGE_SIZE",
	"TEMP_STORE",
}

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

func optionName(option string) string {
	name, _, _ := strings.Cut(option, "=")
	return name
}

func findOption(options []string, name string) (string, bool) {
	for _, option := range options {
		if optionName(option) == name {
			return option, true
		}
	}
	return "", false
}

func driverOptions(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, "PRAGMA compile_options")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var options []string
	for rows.Next() {
		var option string
		if err := rows.Scan(&option); err != nil {
			return nil, err
		}
		options = append(options, option)
	}
	return options, rows.Err()
}

// moduleVersion reads the linked Go module version from the binary's build
// info rather than go.mod so a replace directive or upgrade is caught at run
// time.
func moduleVersion(path string) string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "(no build info)"
	}
	for _, dep := range info.Deps {
		if dep.Path == path {
			if dep.Replace != nil {
				return dep.Version + " replaced by " + dep.Replace.Path + " " + dep.Replace.Version
			}
			return dep.Version
		}
	}
	return "(not linked)"
}

func main() {
	ctx := context.Background()
	pin := sqlite.Current()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		fatal("open :memory:", err)
	}
	defer db.Close()
	// Every course experiment prints sqlite_source_id() first and exits
	// non-zero if the linked build does not match sqlite/PIN.
	sqlitex.MustPinCheck(ctx, db)

	fmt.Printf("pin: release %s, tag %s, git %s, fossil %s\n", pin.Release, pin.Tag, pin.GitCommit, pin.FossilID)
	driverVersion := moduleVersion(pin.DriverModule)
	libcVersion := moduleVersion(pin.LibcModule)
	fmt.Printf("driver module linked: %s %s\n", pin.DriverModule, driverVersion)
	fmt.Printf("libc module linked:   %s %s\n", pin.LibcModule, libcVersion)
	check(driverVersion == pin.DriverVersion, fmt.Sprintf("the linked driver is %s %s", pin.DriverModule, pin.DriverVersion))
	check(libcVersion == pin.LibcVersion, fmt.Sprintf("the linked libc is %s %s", pin.LibcModule, pin.LibcVersion))

	var version string
	if err := db.QueryRowContext(ctx, "SELECT sqlite_version()").Scan(&version); err != nil {
		fatal("sqlite_version", err)
	}
	fmt.Printf("driver sqlite_version(): %s\n", version)
	check(version == pin.Release, "the driver reports release "+pin.Release)

	options, err := driverOptions(ctx, db)
	if err != nil {
		fatal("compile_options", err)
	}
	fmt.Printf("driver compile options (%d):\n", len(options))
	for _, option := range options {
		fmt.Printf("  %s\n", option)
	}
	for _, required := range requiredOptions {
		check(slices.Contains(options, required), "the driver is compiled with "+required)
	}

	// Smoke-test both page-inspection virtual tables on a 2-page database
	// (page 1 for sqlite_schema, page 2 for table t).
	var dbpagePages, dbstatRows int
	if _, err := db.ExecContext(ctx, "CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT); INSERT INTO t VALUES(1, 'one')"); err != nil {
		fatal("create", err)
	}
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_dbpage").Scan(&dbpagePages); err != nil {
		fatal("sqlite_dbpage", err)
	}
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM dbstat").Scan(&dbstatRows); err != nil {
		fatal("dbstat", err)
	}
	fmt.Printf("driver sqlite_dbpage rows: %d; dbstat rows: %d\n", dbpagePages, dbstatRows)
	check(dbpagePages == 2 && dbstatRows == 2, "sqlite_dbpage and dbstat answer queries in the driver (2 pages: schema and table t)")

	cli, err := sqlitex.LocateCLI(ctx)
	if err != nil {
		fmt.Printf("sqlite3 CLI: %v\n", err)
		failures = append(failures, "the pinned sqlite3 CLI is available")
		finish()
		return
	}
	fmt.Println(cli.PinLine())
	fmt.Printf("INFO: CLI %s (%s)\n", displayPath(cli.Path), cli.Origin)
	out, err := cli.Exec(ctx, ":memory:", "SELECT sqlite_version();\nPRAGMA compile_options;\n")
	if err != nil {
		fatal("CLI compile_options", err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	cliVersion, cliOptions := lines[0], lines[1:]
	fmt.Printf("CLI sqlite_version(): %s\n", cliVersion)
	check(cliVersion == pin.Release, "the CLI reports release "+pin.Release)
	for _, required := range requiredOptions {
		check(slices.Contains(cliOptions, required), "the CLI is compiled with "+required)
	}

	fmt.Println("file-format and I/O options, driver | CLI:")
	same := true
	for _, name := range fileFormatAndIOOptions {
		driverValue, inDriver := findOption(options, name)
		cliValue, inCLI := findOption(cliOptions, name)
		if !inDriver {
			driverValue = "(absent)"
		}
		if !inCLI {
			cliValue = "(absent)"
		}
		mark := "same"
		if driverValue != cliValue {
			mark = "DIFFERENT"
			same = false
		}
		fmt.Printf("  %-27s %-32s | %-32s %s\n", name, driverValue, cliValue, mark)
	}
	check(same, "the driver and the CLI agree on every file-format and I/O compile option listed")
	var onlyDriver, onlyCLI []string
	for _, option := range options {
		if !slices.Contains(cliOptions, option) {
			onlyDriver = append(onlyDriver, option)
		}
	}
	for _, option := range cliOptions {
		if !slices.Contains(options, option) {
			onlyCLI = append(onlyCLI, option)
		}
	}
	// Report build-specific differences on INFO lines so EXACT evidence stays
	// portable between the system CLI and the ./configure build-tools CLI.
	fmt.Printf("INFO: compile options only in the driver: %s\n", strings.Join(onlyDriver, " "))
	fmt.Printf("INFO: compile options only in this CLI: %s\n", strings.Join(onlyCLI, " "))
	finish()
}

func finish() {
	if len(failures) > 0 {
		fmt.Printf("PIN CHECK FAILED: %d claim(s) do not hold: %s\n", len(failures), strings.Join(failures, "; "))
		os.Exit(1)
	}
	fmt.Println("PIN CHECK OK")
}

func displayPath(path string) string {
	root, err := sqlitex.RepoRoot()
	if err != nil {
		return path
	}
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	relative, err := filepath.Rel(root, path)
	if err != nil || strings.HasPrefix(relative, "..") {
		return path
	}
	return filepath.ToSlash(relative)
}
```

Run from the repository root (requires the pinned CLI via `go run ./cmd/course build-tools` if not already on `PATH`):

```text
go run ./ch01-architecture-and-execution-path/experiments/1.1-pin-check
```

Captured evidence:

<!-- include: evidence/1.1-pin-check.txt -->
```text
sqlite_source_id: 2026-07-24 19:02:57 bf7c7f30031888f4e796e429ab3978879485813aaca6f641c7b33e4e09459bcc (pin 3.53.4 ok)
pin: release 3.53.4, tag version-3.53.4, git b09c88c14082339b66c7b7158d609a771e64ca69, fossil bf7c7f30031888f4e796e429ab3978879485813aaca6f641c7b33e4e09459bcc
driver module linked: modernc.org/sqlite v1.60.1
libc module linked:   modernc.org/libc v1.77.1
check ok: the linked driver is modernc.org/sqlite v1.60.1
check ok: the linked libc is modernc.org/libc v1.77.1
driver sqlite_version(): 3.53.4
check ok: the driver reports release 3.53.4
driver compile options (56):
  ATOMIC_INTRINSICS=1
  COMPILER=gcc-12.2.0
  DEFAULT_AUTOVACUUM
  DEFAULT_CACHE_SIZE=-2000
  DEFAULT_FILE_FORMAT=4
  DEFAULT_JOURNAL_SIZE_LIMIT=-1
  DEFAULT_MEMSTATUS=0
  DEFAULT_MMAP_SIZE=0
  DEFAULT_PAGE_SIZE=4096
  DEFAULT_PCACHE_INITSZ=20
  DEFAULT_RECURSIVE_TRIGGERS
  DEFAULT_SECTOR_SIZE=4096
  DEFAULT_SYNCHRONOUS=2
  DEFAULT_WAL_AUTOCHECKPOINT=1000
  DEFAULT_WAL_SYNCHRONOUS=2
  DEFAULT_WORKER_THREADS=0
  DIRECT_OVERFLOW_READ
  DISABLE_INTRINSIC
  ENABLE_COLUMN_METADATA
  ENABLE_DBPAGE_VTAB
  ENABLE_DBSTAT_VTAB
  ENABLE_FTS5
  ENABLE_GEOPOLY
  ENABLE_MATH_FUNCTIONS
  ENABLE_MEMORY_MANAGEMENT
  ENABLE_OFFSET_SQL_FUNC
  ENABLE_PREUPDATE_HOOK
  ENABLE_RBU
  ENABLE_RTREE
  ENABLE_SESSION
  ENABLE_SNAPSHOT
  ENABLE_STAT4
  ENABLE_UNLOCK_NOTIFY
  LIKE_DOESNT_MATCH_BLOBS
  MALLOC_SOFT_LIMIT=1024
  MAX_ATTACHED=10
  MAX_COLUMN=2000
  MAX_COMPOUND_SELECT=500
  MAX_DEFAULT_PAGE_SIZE=8192
  MAX_EXPR_DEPTH=1000
  MAX_FUNCTION_ARG=1000
  MAX_LENGTH=1000000000
  MAX_LIKE_PATTERN_LENGTH=50000
  MAX_MMAP_SIZE=0x7fff0000
  MAX_PAGE_COUNT=0xfffffffe
  MAX_PAGE_SIZE=65536
  MAX_SQL_LENGTH=1000000000
  MAX_TRIGGER_DEPTH=1000
  MAX_VARIABLE_NUMBER=32766
  MAX_VDBE_OP=250000000
  MAX_WORKER_THREADS=8
  MUTEX_PTHREADS
  SOUNDEX
  SYSTEM_MALLOC
  TEMP_STORE=1
  THREADSAFE=1
check ok: the driver is compiled with ENABLE_DBPAGE_VTAB
check ok: the driver is compiled with ENABLE_DBSTAT_VTAB
check ok: the driver is compiled with DEFAULT_WAL_SYNCHRONOUS=2
driver sqlite_dbpage rows: 2; dbstat rows: 2
check ok: sqlite_dbpage and dbstat answer queries in the driver (2 pages: schema and table t)
sqlite3 CLI sqlite_source_id: 2026-07-24 19:02:57 bf7c7f30031888f4e796e429ab3978879485813aaca6f641c7b33e4e09459bcc (pin 3.53.4 ok)
INFO: CLI /usr/local/bin/sqlite3 (PATH)
CLI sqlite_version(): 3.53.4
check ok: the CLI reports release 3.53.4
check ok: the CLI is compiled with ENABLE_DBPAGE_VTAB
check ok: the CLI is compiled with ENABLE_DBSTAT_VTAB
check ok: the CLI is compiled with DEFAULT_WAL_SYNCHRONOUS=2
file-format and I/O options, driver | CLI:
  DEFAULT_AUTOVACUUM          DEFAULT_AUTOVACUUM               | DEFAULT_AUTOVACUUM               same
  DEFAULT_CACHE_SIZE          DEFAULT_CACHE_SIZE=-2000         | DEFAULT_CACHE_SIZE=-2000         same
  DEFAULT_FILE_FORMAT         DEFAULT_FILE_FORMAT=4            | DEFAULT_FILE_FORMAT=4            same
  DEFAULT_JOURNAL_SIZE_LIMIT  DEFAULT_JOURNAL_SIZE_LIMIT=-1    | DEFAULT_JOURNAL_SIZE_LIMIT=-1    same
  DEFAULT_MMAP_SIZE           DEFAULT_MMAP_SIZE=0              | DEFAULT_MMAP_SIZE=0              same
  DEFAULT_PAGE_SIZE           DEFAULT_PAGE_SIZE=4096           | DEFAULT_PAGE_SIZE=4096           same
  DEFAULT_PCACHE_INITSZ       DEFAULT_PCACHE_INITSZ=20         | DEFAULT_PCACHE_INITSZ=20         same
  DEFAULT_SECTOR_SIZE         DEFAULT_SECTOR_SIZE=4096         | DEFAULT_SECTOR_SIZE=4096         same
  DEFAULT_SYNCHRONOUS         DEFAULT_SYNCHRONOUS=2            | DEFAULT_SYNCHRONOUS=2            same
  DEFAULT_WAL_AUTOCHECKPOINT  DEFAULT_WAL_AUTOCHECKPOINT=1000  | DEFAULT_WAL_AUTOCHECKPOINT=1000  same
  DEFAULT_WAL_SYNCHRONOUS     DEFAULT_WAL_SYNCHRONOUS=2        | DEFAULT_WAL_SYNCHRONOUS=2        same
  DIRECT_OVERFLOW_READ        DIRECT_OVERFLOW_READ             | DIRECT_OVERFLOW_READ             same
  MAX_MMAP_SIZE               MAX_MMAP_SIZE=0x7fff0000         | MAX_MMAP_SIZE=0x7fff0000         same
  MAX_PAGE_SIZE               MAX_PAGE_SIZE=65536              | MAX_PAGE_SIZE=65536              same
  TEMP_STORE                  TEMP_STORE=1                     | TEMP_STORE=1                     same
check ok: the driver and the CLI agree on every file-format and I/O compile option listed
INFO: compile options only in the driver: COMPILER=gcc-12.2.0 DEFAULT_MEMSTATUS=0 DISABLE_INTRINSIC ENABLE_COLUMN_METADATA ENABLE_GEOPOLY ENABLE_MEMORY_MANAGEMENT ENABLE_PREUPDATE_HOOK ENABLE_RBU ENABLE_SESSION ENABLE_SNAPSHOT ENABLE_STAT4 ENABLE_UNLOCK_NOTIFY LIKE_DOESNT_MATCH_BLOBS SOUNDEX
INFO: compile options only in this CLI: COMPILER=gcc-13.3.0 DQS=0 ENABLE_BYTECODE_VTAB ENABLE_EXPLAIN_COMMENTS ENABLE_FTS3 ENABLE_FTS4 ENABLE_PERCENTILE ENABLE_STMTVTAB ENABLE_UNKNOWN_SQL_FUNCTION STRICT_SUBTYPE
PIN CHECK OK
```

#### Reading the evidence

- **Lines 1–8 (pin and linked modules).** The first line prints `sqlite_source_id()` from an in-memory database opened through `database/sql` and checks it against `sqlite/PIN`. Lines 3–6 read Go's compiled-in `runtime/debug.ReadBuildInfo()` rather than `go.mod`, confirming the binary actually linked `modernc.org/sqlite v1.60.1` and `modernc.org/libc v1.77.1`, and that `sqlite_version()` returns `3.53.4`.
- **Lines 9–68 (driver compile options).** The Go driver reports 56 compile-time options. Lines 66–68 verify the three options later chapters rely on: `ENABLE_DBPAGE_VTAB` (reads raw pages via SQL), `ENABLE_DBSTAT_VTAB` (reports per-page B-tree statistics), and `DEFAULT_WAL_SYNCHRONOUS=2` (upstream's default `synchronous=FULL` in WAL mode).
- **Lines 69–70 (virtual table smoke test).** After creating table `t` and inserting one row, both `sqlite_dbpage` and `dbstat` return 2 rows: page 1 (`sqlite_schema`) and page 2 (table `t`).
- **Lines 71–77 (C CLI check).** `sqlitex.LocateCLI` finds the pinned C `sqlite3` binary (the `INFO:` line records its location and origin, which varies between a system-installed binary and `_tools/bin/sqlite3`). Its `sqlite_source_id()`, `sqlite_version()`, and three required compile options all match the pin.
- **Lines 78–94 (file-format and I/O parity).** Both builds agree on all 15 compile options that control file layout, page size (`4096`), cache defaults (`-2000`, meaning 2,000 KiB), mmap defaults (`0`), and sync defaults (`2` = `FULL`).
- **Lines 95–96 (`INFO:` differences between builds).** The driver and CLI are not compiled identically for non-storage features. Two differences are worth knowing: the driver includes `ENABLE_STAT4` (so `ANALYZE` can populate `sqlite_stat4` in the driver while the CLI cannot) and `ENABLE_SNAPSHOT` (compiled into the translated C even though the Go driver API does not expose `sqlite3_snapshot_*`), whereas the CLI includes `ENABLE_BYTECODE_VTAB`, `ENABLE_STMTVTAB`, and `ENABLE_EXPLAIN_COMMENTS` (so `bytecode()` and `sqlite_stmt` exist in the CLI, not in the driver). None of those differences affect file format or I/O.

---

## 3. Call path

This section follows one autocommit statement—`INSERT INTO t(id, v) VALUES(2, 'two')`—through the pinned C source (`version-3.53.4`). Four files are copied whole into this chapter directory (`prepare.c`, `vdbeapi.c`, `vdbeaux.c`, `os.c`); `vdbe.c` is linked in the pinned tree (`$SQLITE_SRC` or `_src/sqlite-3.53.4`) rather than copied.

### 3.1 Prepare: compiling SQL into a VDBE program

Preparation compiles SQL text into bytecode without writing to the database file (it reads the database only if the connection has not yet loaded `sqlite_schema`).

1. **`sqlite3_prepare_v2`** — [`src/prepare.c:937-954`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/prepare.c#L937-L954) (local copy: [`prepare.c`](prepare.c)). The public entry point forwards straight to `sqlite3LockAndPrepare` with `SQLITE_PREPARE_SAVESQL`:
   ```c
   rc = sqlite3LockAndPrepare(db,zSql,nBytes,SQLITE_PREPARE_SAVESQL,0,
                              ppStmt,pzTail);
   ```
   Saving the SQL text on the statement is what allows `sqlite3_step` to recompile the statement automatically if another connection changes the schema before execution.
2. **`sqlite3LockAndPrepare`** — [`src/prepare.c:836-875`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/prepare.c#L836-L875) (local copy: [`prepare.c`](prepare.c)). Enters the connection mutex (`sqlite3_mutex_enter(db->mutex)` at line 855) and every attached database's B-tree mutex (`sqlite3BtreeEnterAll` at line 856), then calls `sqlite3Prepare` in a retry loop (lines 857–866). If compilation returns `SQLITE_SCHEMA`, it discards the connection's cached schema (`sqlite3ResetOneSchema(db, -1)` at line 866) and retries once.
3. **`sqlite3Prepare`** — [`src/prepare.c:682-835`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/prepare.c#L682-L835) (local copy: [`prepare.c`](prepare.c)). Initializes a `Parse` context, checks that no shared-cache connection holds a schema lock (`sqlite3BtreeSchemaLocked` at line 752), and invokes the tokenizer and parser via `sqlite3RunParser` (line 779 or 786). Grammar actions emit bytecode instructions into a `Vdbe` object. Line 795 stores the original SQL text via `sqlite3VdbeSetSql`. On failure it validates the schema cookie (`schemaIsValid` at line 803); on success line 818 hands the `Vdbe` pointer back to the caller as `sqlite3_stmt *`.

### 3.2 Step: running bytecode and crossing into the B-tree

Execution begins only when the caller steps the prepared statement.

4. **`sqlite3_step`** — [`src/vdbeapi.c:919-966`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/vdbeapi.c#L919-L966) (local copy: [`vdbeapi.c`](vdbeapi.c)). Takes the connection mutex (line 929) and runs `sqlite3Step` inside a schema-retry loop:
   ```c
   while( (rc = sqlite3Step(v))==SQLITE_SCHEMA
          && cnt++ < SQLITE_MAX_SCHEMA_RETRY ){
     int savedPc = v->pc;
     rc = sqlite3Reprepare(v);
   ```
   If another connection altered the schema since `sqlite3_prepare_v2`, `sqlite3_step` calls `sqlite3Reprepare` (line 933), resets the statement (line 954), and runs it again up to `SQLITE_MAX_SCHEMA_RETRY` (50) times. The application never sees `SQLITE_SCHEMA` unless recompilation itself fails.
5. **`sqlite3Step`** — [`src/vdbeapi.c:777-912`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/vdbeapi.c#L777-L912) (local copy: [`vdbeapi.c`](vdbeapi.c)). Moves a `VDBE_READY_STATE` statement to `VDBE_RUN_STATE`, increments `db->nVdbeActive` and (for non-read-only statements) `db->nVdbeWrite` (lines 821–822), and sets the program counter `p->pc = 0` (line 824). If the statement is already in `VDBE_HALT_STATE`, line 852 auto-resets it first. It then dispatches to `sqlite3VdbeList` when the statement is an `EXPLAIN` (line 864) or runs the bytecode interpreter `sqlite3VdbeExec` (line 869). When an autocommit statement finishes with `SQLITE_DONE`, line 886 invokes `doWalCallbacks` (which triggers automatic WAL checkpoints when the threshold is reached).
6. **`OP_Transaction`** — [`src/vdbe.c:4102-4201`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/vdbe.c#L4102-L4201) (in the pinned tree). Starts a read (`P2=0`) or write (`P2!=0`) transaction on database `P1` by calling `sqlite3BtreeBeginTrans` (line 4128). If the lock is busy (`SQLITE_BUSY`), it saves the program counter and returns so the caller can retry. When `P5` is non-zero, it compares the file's schema cookie against the cookie compiled into the statement (`P3`); a mismatch expires the statement with `"database schema has changed"` (lines 4174 and 4191), driving the retry loop in `sqlite3_step`.
7. **`OP_Insert`** — [`src/vdbe.c:5748-5834`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/vdbe.c#L5748-L5834) (in the pinned tree). Takes the encoded record built by `OP_MakeRecord` (register `P2`) and the rowid (register `P3`) and inserts them into the B-tree through cursor `P1` via `sqlite3BtreeInsert` (line 5816). When flags in `P5` request it, lines 5802–5803 update the connection's change count and `last_insert_rowid`. `OP_Insert` does not write to disk directly; the B-tree calls `sqlite3PagerWrite` to mark the target page writable. In rollback mode, that first `sqlite3PagerWrite` opens `-journal` and saves the page's original bytes; in WAL mode, no file write happens until commit.

### 3.3 Halt and commit: crossing into the pager and VFS

8. **`sqlite3VdbeHalt`** — [`src/vdbeaux.c:3315-3517`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/vdbeaux.c#L3315-L3517) (local copy: [`vdbeaux.c`](vdbeaux.c)). Runs when a statement hits `OP_Halt` (or `OP_AutoCommit` for an explicit `COMMIT`). It closes all cursors (line 3339). If the connection is in autocommit mode and this statement is the last active writer (`db->autoCommit && db->nVdbeWrite==(p->readOnly==0)`, lines 3402–3403), it checks deferred foreign keys and calls `vdbeCommit` (line 3421). If `vdbeCommit` fails, line 3429 calls `sqlite3RollbackAll`. Otherwise (inside an explicit transaction or when another writer statement is still open), line 3464 closes only the statement's sub-transaction via `sqlite3VdbeCloseStatement`.
9. **`vdbeCommit`** — [`src/vdbeaux.c:2919-3175`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/vdbeaux.c#L2919-L3175) (local copy: [`vdbeaux.c`](vdbeaux.c)). Loops over every attached database with an active write transaction, acquires an `EXCLUSIVE` pager lock (`sqlite3PagerExclusiveLock` at line 2973), and checks `aMJNeeded` (lines 2954–2961) to count how many need a multi-file super-journal (`DELETE`, `PERSIST`, and `TRUNCATE` need one when two or more databases are written; `WAL` sets `0`). With one database (or in WAL mode), it runs two-phase commit: `sqlite3BtreeCommitPhaseOne` (line 3005) flushes dirty pages and syncs them; `sqlite3BtreeCommitPhaseTwo` (line 3020) deletes the rollback journal or releases WAL locks. When a statement is later destroyed, `sqlite3_finalize` ([`src/vdbeapi.c:105-124`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/vdbeapi.c#L105-L124), local copy [`vdbeapi.c`](vdbeapi.c)) resets and frees the `Vdbe`.
10. **The VFS wrappers** — [`src/os.c:88-102`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/os.c#L88-L102) (local copy: [`os.c`](os.c)). Everything above the VFS calls one-line wrappers that dispatch through the open file's method table (`pMethods`):
    ```c
    int sqlite3OsRead(sqlite3_file *id, void *pBuf, int amt, i64 offset){
      DO_OS_MALLOC_TEST(id);
      return id->pMethods->xRead(id, pBuf, amt, offset);
    }
    int sqlite3OsWrite(sqlite3_file *id, const void *pBuf, int amt, i64 offset){
      DO_OS_MALLOC_TEST(id);
      return id->pMethods->xWrite(id, pBuf, amt, offset);
    }
    int sqlite3OsTruncate(sqlite3_file *id, i64 size){
      return id->pMethods->xTruncate(id, size);
    }
    int sqlite3OsSync(sqlite3_file *id, int flags){
      DO_OS_MALLOC_TEST(id);
      return flags ? id->pMethods->xSync(id, flags) : SQLITE_OK;
    }
    ```
    Notice `sqlite3OsSync` (lines 99–102): when `flags` is `0` (as under `synchronous=OFF`), it returns `SQLITE_OK` without even entering the VFS. Below this table, `os_unix.c` issues `pread64`, `pwrite64`, `ftruncate`, and `fsync`/`fdatasync`.

### Version notes

Behavior differences across SQLite releases relevant to this chapter (sources: [`docs/knowledge/source-inventory-ch01-04.md`](../docs/knowledge/source-inventory-ch01-04.md) and the pinned source):

- **`sqlite3VdbeSerialPut()` removed (2022-04-02).** Record serialization was inlined into `OP_MakeRecord`, noted in the comment at [`src/vdbeaux.c:3856`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/vdbeaux.c#L3856) (local copy: [`vdbeaux.c`](vdbeaux.c)). Older references that trace record writes through `sqlite3VdbeSerialPut` are out of date.
- **`bytecode` and `tables_used` virtual tables (added in 3.32.0, 2020-05-22, `changes.html`).** Enabled in the C CLI (`ENABLE_BYTECODE_VTAB`) and absent in the Go driver.
- **`sqlite3_stmt_explain()` (added in 3.43.0, `changes.html`).** Switches a prepared statement between normal execution and `EXPLAIN` modes.
- **`sqlite3_stmt_scanstatus_v2()` in `EXPLAIN QUERY PLAN` (added in 3.41.0, `changes.html`).** Requires `SQLITE_ENABLE_STMT_SCANSTATUS`, which neither build enables.
- **3.53.4 baseline fixes.** Release 3.53.4 already includes `SQLITE_CHECKPOINT_NOOP` (3.51.0) and the WAL-reset corruption fix (3.51.3 / 3.53.0).

### Experiment 1.2 — One INSERT through the layers (`1.2-one-insert`)

- **Claim:** One committed `INSERT` crosses every layer, and each crossing is observable from outside: opcodes (`EXPLAIN`), changed pages (`sqlite_dbpage`), journal or WAL writes (pager), VFS calls (`-vfstrace`), and kernel syscalls (`strace`). In rollback mode, each `pwrite64` on `t.db` writes one whole changed page at offset `(page - 1) * 4096`. In WAL mode, the commit makes zero writes to `t.db` and appends a 24-byte frame header plus 4,096-byte page image per changed page to `t.db-wal`. The translated Go driver (`modernc.org/sqlite`) and the C CLI issue the same writes, syncs, reads, locks, opens, unlinks, and closes in the same order and leave byte-identical database files, differing only in extra `fcntl(F_SETFD)` calls issued by `modernc.org/libc`'s musl-derived `open()`.
- **Evidence form:** `EXACT` (all lines except `INFO:` lines).
- **Platform:** **Linux only** for `strace`, the twin check, and the `-vfstrace` merged trace. On macOS, running the experiment directly prints `EXPLAIN`, a `LINUX-ONLY:` line, and the changed page hashes in both modes; run the full experiment inside Docker with `docker compose run --rm lab go run ./ch01-architecture-and-execution-path/experiments/1.2-one-insert`. All syscall and VFS results below were measured on Linux only.
- **Why kept:** this is the one place the course earns the right to use a translated driver. Everything after relies on it.

<!-- include: experiments/1.2-one-insert/main.go -->
```go
// Experiment 1.2 traces one committed INSERT through every SQLite layer:
// VDBE bytecode (EXPLAIN), B-tree pages (sqlite_dbpage), pager and VFS calls
// (C CLI -vfstrace), and kernel syscalls (strace). It runs in both rollback
// (DELETE) and WAL modes and performs the twin check comparing the translated
// Go driver against the C CLI.
//
// Run from the repo root (Linux for strace; on macOS run inside Docker via
// docker compose run --rm lab go run ./ch01-architecture-and-execution-path/experiments/1.2-one-insert):
//
//	go run ./ch01-architecture-and-execution-path/experiments/1.2-one-insert
package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/nickstrad/sqlite-internals/internal/recipe"
	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
	"github.com/nickstrad/sqlite-internals/internal/trace"
)

const packagePath = "ch01-architecture-and-execution-path/experiments/1.2-one-insert"

// On-disk geometry and SQL shared by the rollback and WAL runs: 4096-byte
// pages, 32-byte WAL header, and 24-byte WAL frame header (src/wal.c:21-60).
const (
	pageSize       = 4096
	walHeaderSize  = 32
	walFrameHeader = 24
	createSQL      = "CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT NOT NULL); INSERT INTO t(id, v) VALUES(1, 'one')"
	insertSQL      = "INSERT INTO t(id, v) VALUES(2, 'two')"
	dbName         = "t.db"
)

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

func options(mode string) sqlitex.Options {
	o := sqlitex.DefaultOptions()
	o.File.JournalMode = mode
	o.SingleConnection = true
	return o
}

// cliPrelude translates sqlitex.Options into PRAGMA statements so the C CLI
// runs under the exact same connection settings as the Go driver.
func cliPrelude(o sqlitex.Options) string {
	onOff := func(b bool) string {
		if b {
			return "ON"
		}
		return "OFF"
	}
	c := o.Conn
	return fmt.Sprintf("PRAGMA journal_mode=%s;\nPRAGMA synchronous=%s;\nPRAGMA foreign_keys=%s;\nPRAGMA secure_delete=%s;\nPRAGMA cache_size=%d;\nPRAGMA wal_autocheckpoint=%d;\nPRAGMA busy_timeout=%d;\n",
		o.File.JournalMode, c.Synchronous, onOff(c.ForeignKeys), onOff(c.SecureDelete), c.CacheSize, c.WALAutocheckpoint, c.BusyTimeout.Milliseconds())
}

func markerPath(dir, name string) string {
	return filepath.Join(dir, "marker-"+name)
}

// createMarkers creates empty files that both the Go child and the CLI (.read)
// open immediately before and after a statement. Because CLI stdout is
// buffered, opening a named file puts an unbuffered syscall bracket in strace
// at the exact statement boundary.
func createMarkers(dir string, names ...string) {
	for _, name := range names {
		if err := os.WriteFile(markerPath(dir, name), nil, 0o644); err != nil {
			fatal("marker", err)
		}
	}
}

func touchMarker(dir, name string) {
	if _, err := os.ReadFile(markerPath(dir, name)); err != nil {
		fatal("marker", err)
	}
}

func createBase(ctx context.Context, path, mode string) {
	if err := recipe.RemoveDatabase(path); err != nil {
		fatal("remove", err)
	}
	db, err := sqlitex.Open(ctx, path, options(mode))
	if err != nil {
		fatal("create base", err)
	}
	if _, err := db.ExecContext(ctx, createSQL); err != nil {
		fatal("create base", err)
	}
	if err := db.Close(); err != nil {
		fatal("close base", err)
	}
	if err := recipe.CheckAtRest(path); err != nil {
		fatal("base at rest", err)
	}
}

// pageHashes reads every page through the sqlite_dbpage virtual table and
// returns its SHA-256 hex digest in page-number order.
func pageHashes(ctx context.Context, path, mode string) []string {
	db, err := sqlitex.Open(ctx, path, options(mode))
	if err != nil {
		fatal("open for sqlite_dbpage", err)
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, "SELECT pgno, data FROM sqlite_dbpage ORDER BY pgno")
	if err != nil {
		fatal("sqlite_dbpage", err)
	}
	defer rows.Close()
	var hashes []string
	for rows.Next() {
		var pgno int
		var data []byte
		if err := rows.Scan(&pgno, &data); err != nil {
			fatal("sqlite_dbpage scan", err)
		}
		if pgno != len(hashes)+1 {
			fatal("sqlite_dbpage", fmt.Errorf("page %d out of order", pgno))
		}
		sum := sha256.Sum256(data)
		hashes = append(hashes, hex.EncodeToString(sum[:]))
	}
	if err := rows.Err(); err != nil {
		fatal("sqlite_dbpage", err)
	}
	return hashes
}

func changedPages(before, after []string) []int {
	var changed []int
	for i := range max(len(before), len(after)) {
		if i >= len(before) || i >= len(after) || before[i] != after[i] {
			changed = append(changed, i+1)
		}
	}
	return changed
}

func short(hash string) string {
	if hash == "" {
		return "(none)"
	}
	return hash[:16]
}

// printExplain queries SQLite's bytecode listing for insertSQL, which
// sqlite3Step produces via sqlite3VdbeList (src/vdbeapi.c:863-865).
func printExplain(ctx context.Context, path string) {
	db, err := sqlitex.Open(ctx, path, options(sqlitex.JournalDelete))
	if err != nil {
		fatal("open for EXPLAIN", err)
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, "EXPLAIN "+insertSQL)
	if err != nil {
		fatal("EXPLAIN", err)
	}
	defer rows.Close()
	fmt.Printf("EXPLAIN %s\n", insertSQL)
	fmt.Printf("  %-4s %-14s %4s %4s %4s %-12s %s\n", "addr", "opcode", "p1", "p2", "p3", "p4", "p5")
	for rows.Next() {
		var addr, p1, p2, p3, p5 int
		var opcode string
		var p4, comment sql.NullString
		if err := rows.Scan(&addr, &opcode, &p1, &p2, &p3, &p4, &p5, &comment); err != nil {
			fatal("EXPLAIN scan", err)
		}
		fmt.Printf("  %-4d %-14s %4d %4d %4d %-12s %d\n", addr, opcode, p1, p2, p3, p4.String, p5)
	}
	if err := rows.Err(); err != nil {
		fatal("EXPLAIN", err)
	}
}

type walFrame struct {
	Index      int
	Page       uint32
	CommitSize uint32
}

// readWALFrames decodes the page number (bytes 0..3) and commit-size field
// (bytes 4..7) from every 24-byte frame header after the 32-byte WAL header.
func readWALFrames(path string) ([]walFrame, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var frames []walFrame
	for offset := walHeaderSize; offset+walFrameHeader+pageSize <= len(data); offset += walFrameHeader + pageSize {
		frames = append(frames, walFrame{
			Index:      len(frames) + 1,
			Page:       binary.BigEndian.Uint32(data[offset:]),
			CommitSize: binary.BigEndian.Uint32(data[offset+4:]),
		})
	}
	return frames, nil
}

// childInsert is the re-exec'd child entry point traced by strace. It locks
// its goroutine to one OS thread so strace sees its syscalls in order, brackets
// the INSERT with marker files, and reads the -wal frames before db.Close
// checkpoints and deletes the WAL.
func childInsert(dir, mode string) {
	runtime.LockOSThread()
	ctx := context.Background()
	path := filepath.Join(dir, dbName)
	db, err := sqlitex.Open(ctx, path, options(mode))
	if err != nil {
		fatal("child open", err)
	}
	touchMarker(dir, "1")
	if _, err := db.ExecContext(ctx, insertSQL); err != nil {
		fatal("child insert", err)
	}
	touchMarker(dir, "2")
	if mode == sqlitex.JournalWAL {
		// Read the -wal before closing, since closing the last WAL connection
		// checkpoints frames into t.db and deletes t.db-wal. Reading the -wal
		// (not t.db) avoids touching the POSIX locks held on t.db and t.db-shm.
		frames, err := readWALFrames(path + "-wal")
		if err != nil {
			fatal("child read wal", err)
		}
		for _, frame := range frames {
			fmt.Printf("frame %d page=%d commit_size=%d\n", frame.Index, frame.Page, frame.CommitSize)
		}
	}
	if err := db.Close(); err != nil {
		fatal("child close", err)
	}
}

func parseChildFrames(stdout string) []walFrame {
	var frames []walFrame
	for _, line := range strings.Split(stdout, "\n") {
		var frame walFrame
		if _, err := fmt.Sscanf(line, "frame %d page=%d commit_size=%d", &frame.Index, &frame.Page, &frame.CommitSize); err == nil {
			frames = append(frames, frame)
		}
	}
	return frames
}

func isMarkerOpen(call trace.Syscall, dir string) (string, bool) {
	if call.Name != "open" && call.Name != "openat" {
		return "", false
	}
	prefix := markerPath(dir, "")
	if !strings.HasPrefix(call.Path, prefix) {
		return "", false
	}
	return strings.TrimPrefix(call.Path, prefix), true
}

func isMarkerFile(call trace.Syscall, dir string) bool {
	return strings.HasPrefix(call.Path, markerPath(dir, ""))
}

var fileSyscalls = []string{"open", "openat", "close", "pread64", "pwrite64", "read", "write", "fsync", "fdatasync", "ftruncate", "unlink", "unlinkat", "fcntl", "mmap"}

func isDatabaseIO(call trace.Syscall, dir string) bool {
	if !slices.Contains(fileSyscalls, call.Name) || isMarkerFile(call, dir) {
		return false
	}
	return call.Path == dir || strings.HasPrefix(call.Path, dir+string(filepath.Separator))
}

// segmentCalls extracts all database-directory file syscalls strictly between
// the open of marker from and the open of marker to.
func segmentCalls(calls []trace.Syscall, dir, from, to string) ([]trace.Syscall, error) {
	start, end := -1, -1
	for i, call := range calls {
		name, ok := isMarkerOpen(call, dir)
		if !ok {
			continue
		}
		if name == from && start < 0 {
			start = i
		}
		if name == to && start >= 0 && end < 0 {
			end = i
		}
	}
	if start < 0 || end < 0 {
		return nil, fmt.Errorf("markers %s..%s not found in the trace", from, to)
	}
	var out []trace.Syscall
	for _, call := range calls[start+1 : end] {
		if isDatabaseIO(call, dir) {
			out = append(out, call)
		}
	}
	return trace.RelativeTo(dir, out), nil
}

func syncName(name string) bool {
	return name == "fsync" || name == "fdatasync"
}

// normalized canonicalizes syscall spellings that differ across libc or
// configure builds (open vs openat, unlink vs unlinkat, fsync vs fdatasync)
// so the twin check compares the actual file operations.
func normalized(call trace.Syscall) string {
	switch {
	case call.Name == "open" || call.Name == "openat":
		if call.Errno != "" {
			return "open " + call.Path + " = " + call.Errno
		}
		return "open " + call.Path
	case syncName(call.Name):
		return "sync " + call.Path
	case call.Name == "unlink" || call.Name == "unlinkat":
		return "unlink " + call.Path
	case call.Name == "pwrite64" || call.Name == "pread64":
		return fmt.Sprintf("%s %s off=%d len=%d", call.Name, call.Path, call.Offset, call.Length)
	case call.Name == "ftruncate":
		return fmt.Sprintf("ftruncate %s len=%d", call.Path, call.Length)
	case call.Name == "mmap":
		return fmt.Sprintf("mmap %s off=%d len=%d", call.Path, call.Offset, call.Length)
	case call.Name == "fcntl" && call.Lock != nil:
		return fmt.Sprintf("fcntl %s %s %s start=%d len=%d", call.Path, call.Lock.Command, call.Lock.Type, call.Lock.Start, call.Lock.Length)
	case call.Name == "fcntl":
		return fmt.Sprintf("fcntl %s %s", call.Path, call.Flags)
	}
	return call.Name + " " + call.Path
}

func normalizedAll(calls []trace.Syscall, keep func(trace.Syscall) bool) []string {
	var out []string
	for _, call := range calls {
		if keep == nil || keep(call) {
			out = append(out, normalized(call))
		}
	}
	return out
}

// isCloexecFcntl matches the extra fcntl(fd, F_SETFD, FD_CLOEXEC) call that
// modernc's musl-derived libc issues after every O_CLOEXEC open, whereas
// SQLite's C build relies on O_CLOEXEC directly.
func isCloexecFcntl(call trace.Syscall) bool {
	return call.Name == "fcntl" && call.Lock == nil && strings.HasPrefix(call.Flags, "F_SETFD")
}

func isWrite(call trace.Syscall) bool {
	return call.Name == "pwrite64" || call.Name == "write" || call.Name == "ftruncate"
}

func isSync(call trace.Syscall) bool {
	return syncName(call.Name)
}

func compareLists(label string, goList, cliList []string) bool {
	if slices.Equal(goList, cliList) {
		fmt.Printf("twin %s: identical (%d calls)\n", label, len(goList))
		return true
	}
	fmt.Printf("twin %s: DIFFERENT (Go %d calls, CLI %d calls)\n", label, len(goList), len(cliList))
	for i := range max(len(goList), len(cliList)) {
		var g, c string
		if i < len(goList) {
			g = goList[i]
		}
		if i < len(cliList) {
			c = cliList[i]
		}
		if g != c {
			fmt.Printf("  #%d Go: %-50s CLI: %s\n", i+1, g, c)
		}
	}
	return false
}

func syncCallNames(calls []trace.Syscall) string {
	var names []string
	for _, call := range calls {
		if isSync(call) && !slices.Contains(names, call.Name) {
			names = append(names, call.Name)
		}
	}
	return strings.Join(names, ",")
}

type phase struct {
	from, to  string
	statement string
	opcode    string
	path      string
}

// phases maps the three split statements (BEGIN IMMEDIATE, INSERT, COMMIT) to
// the opcodes and C call paths they isolate inside one autocommit INSERT.
func phases(mode string) []phase {
	commitPath := "sqlite3VdbeHalt -> vdbeCommit -> sqlite3BtreeCommitPhaseOne/PhaseTwo -> sqlite3PagerCommitPhaseOne/PhaseTwo"
	insertPath := "sqlite3BtreeInsert -> sqlite3PagerWrite"
	if mode == sqlitex.JournalWAL {
		commitPath = "sqlite3VdbeHalt -> vdbeCommit -> sqlite3BtreeCommitPhaseOne -> sqlite3PagerCommitPhaseOne -> pagerWalFrames -> sqlite3WalFrames"
	}
	return []phase{
		{"1", "2", "BEGIN IMMEDIATE", "OP_Transaction", "sqlite3BtreeBeginTrans -> sqlite3PagerSharedLock, sqlite3PagerBegin"},
		{"2", "3", insertSQL, "OP_Insert", insertPath},
		{"3", "4", "COMMIT", "OP_AutoCommit (OP_Halt in an autocommit INSERT)", commitPath},
	}
}

func splitScript(dir string, o sqlitex.Options) string {
	return cliPrelude(o) +
		".read '" + markerPath(dir, "1") + "'\nBEGIN IMMEDIATE;\n" +
		".read '" + markerPath(dir, "2") + "'\n" + insertSQL + ";\n" +
		".read '" + markerPath(dir, "3") + "'\nCOMMIT;\n" +
		".read '" + markerPath(dir, "4") + "'\n"
}

func autocommitScript(dir string, o sqlitex.Options) string {
	return cliPrelude(o) +
		".read '" + markerPath(dir, "1") + "'\n" + insertSQL + ";\n" +
		".read '" + markerPath(dir, "2") + "'\n"
}

type eventSegment struct {
	events []trace.Event
	calls  []trace.Syscall
}

func mergedSegment(merged *trace.Merged, dir, from, to string) (eventSegment, error) {
	start, end := -1, -1
	for i, event := range merged.Events {
		if event.Kind != trace.EventSyscall {
			continue
		}
		name, ok := isMarkerOpen(*event.Syscall, dir)
		if !ok {
			continue
		}
		if name == from && start < 0 {
			start = i
		}
		if name == to && start >= 0 && end < 0 {
			end = i
		}
	}
	if start < 0 || end < 0 {
		return eventSegment{}, fmt.Errorf("markers %s..%s not found in the merged trace", from, to)
	}
	var segment eventSegment
	for _, event := range merged.Events[start+1 : end] {
		switch event.Kind {
		case trace.EventVFS:
			segment.events = append(segment.events, event)
		case trace.EventSyscall:
			if isDatabaseIO(*event.Syscall, dir) {
				segment.events = append(segment.events, event)
				segment.calls = append(segment.calls, *event.Syscall)
			}
		}
	}
	segment.calls = trace.RelativeTo(dir, segment.calls)
	return segment, nil
}

func vfsLine(call trace.VFSCall, dir string) string {
	calls := trace.RelativeVFS(dir, []trace.VFSCall{call})
	return strings.TrimPrefix(strings.TrimSpace(calls[0].Raw), "trace.")
}

func runMode(ctx context.Context, base, mode string, cli sqlitex.CLI, haveCLI bool) {
	dir := filepath.Join(base, mode)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("mkdir", err)
	}
	path := filepath.Join(dir, dbName)
	createMarkers(dir, "1", "2", "3", "4")
	title := map[string]string{sqlitex.JournalDelete: "rollback journal (journal_mode=delete)", sqlitex.JournalWAL: "write-ahead log (journal_mode=wal, the platform default)"}[mode]
	fmt.Printf("\n== %s ==\n", title)

	createBase(ctx, path, mode)
	before := pageHashes(ctx, path, mode)
	baseHash, err := recipe.FileSHA256(path)
	if err != nil {
		fatal("hash", err)
	}
	fmt.Printf("base: table t with 1 row, %d pages, sha256 %s\n", len(before), short(baseHash))

	// On non-Linux hosts, strace is unavailable; still run the INSERT in
	// process so the reader sees which pages changed.
	if !trace.Supported {
		db, err := sqlitex.Open(ctx, path, options(mode))
		if err != nil {
			fatal("open", err)
		}
		if _, err := db.ExecContext(ctx, insertSQL); err != nil {
			fatal("insert", err)
		}
		db.Close()
		after := pageHashes(ctx, path, mode)
		changed := changedPages(before, after)
		printChanged(before, after, changed)
		return
	}

	command, err := trace.SelfCommand("child-insert", dir, mode)
	if err != nil {
		fatal("self command", err)
	}
	goRun, err := trace.Run(ctx, trace.Options{Command: command})
	if err != nil {
		fatal("strace Go child", err)
	}
	if goRun.ExitCode != 0 {
		fatal("Go child", fmt.Errorf("exit %d: %s", goRun.ExitCode, goRun.Stderr))
	}
	after := pageHashes(ctx, path, mode)
	goFileHash, err := recipe.FileSHA256(path)
	if err != nil {
		fatal("hash", err)
	}
	changed := changedPages(before, after)
	printChanged(before, after, changed)

	goCalls, err := segmentCalls(goRun.Log.Calls, dir, "1", "2")
	if err != nil {
		fatal("Go segment", err)
	}
	fmt.Printf("Go driver (modernc), file syscalls of the INSERT (%d):\n", len(goCalls))
	for _, call := range goCalls {
		fmt.Printf("  %s\n", call)
	}

	dbWrites := 0
	offsetsMatch := true
	written := map[int]int{}
	for _, call := range goCalls {
		if call.Name == "pwrite64" && call.Path == dbName {
			dbWrites++
			page := int(call.Offset/pageSize) + 1
			written[page]++
			if call.Offset%pageSize != 0 || call.Length != pageSize || !slices.Contains(changed, page) {
				offsetsMatch = false
			}
			fmt.Printf("  db write at offset %d = (page %d - 1) * %d\n", call.Offset, page, pageSize)
		}
	}
	if mode == sqlitex.JournalDelete {
		// In rollback mode, pager_write_pagelist writes each modified page
		// directly to t.db at offset (page - 1) * pageSize.
		everyChangedWrittenOnce := len(written) == len(changed)
		for _, page := range changed {
			if written[page] != 1 {
				everyChangedWrittenOnce = false
			}
		}
		check(dbWrites > 0 && offsetsMatch && everyChangedWrittenOnce, fmt.Sprintf("rollback mode: each pwrite64 on %s is one whole page at offset (page-1)*4096, and the pages written are exactly the changed pages %v", dbName, changed))
	} else {
		// In WAL mode, pagerWalFrames appends each modified page as a frame in
		// t.db-wal and leaves t.db untouched until a checkpoint runs.
		check(dbWrites == 0, fmt.Sprintf("WAL mode: the commit writes nothing to %s itself (database-file pwrite64 calls during the INSERT: %d)", dbName, dbWrites))
		frames := parseChildFrames(goRun.Stdout)
		var framePages []int
		for _, frame := range frames {
			fmt.Printf("  WAL frame %d: page %d, commit size %d, header at offset %d, page image at offset %d\n", frame.Index, frame.Page, frame.CommitSize, walHeaderSize+(frame.Index-1)*(walFrameHeader+pageSize), walHeaderSize+(frame.Index-1)*(walFrameHeader+pageSize)+walFrameHeader)
			framePages = append(framePages, int(frame.Page))
		}
		walWritesMatch := true
		var walWrites []string
		for _, call := range goCalls {
			if call.Name == "pwrite64" && call.Path == dbName+"-wal" {
				walWrites = append(walWrites, fmt.Sprintf("%d+%d", call.Offset, call.Length))
			}
		}
		var wantWALWrites []string
		for _, frame := range frames {
			offset := walHeaderSize + (frame.Index-1)*(walFrameHeader+pageSize)
			wantWALWrites = append(wantWALWrites, fmt.Sprintf("%d+%d", offset, walFrameHeader), fmt.Sprintf("%d+%d", offset+walFrameHeader, pageSize))
		}
		if len(walWrites) == 0 || !slices.Equal(walWrites[len(walWrites)-len(wantWALWrites):], wantWALWrites) {
			walWritesMatch = false
		}
		lastCommit := len(frames) > 0 && frames[len(frames)-1].CommitSize > 0
		check(slices.Equal(framePages, changed) && walWritesMatch && lastCommit, fmt.Sprintf("WAL mode: the commit appends one frame per changed page %v to %s-wal (frame header 24 bytes, then the page at offset 32 + (frame-1)*4120 + 24), the last frame carries the commit size", changed, dbName))
	}

	if !haveCLI {
		return
	}

	// Twin check: rebuild the base and run the exact same autocommit INSERT in
	// the C CLI under strace, then compare writes, syncs, other syscalls, and
	// final file bytes.
	createBase(ctx, path, mode)
	cliCommand := cli.Command(ctx, sqlitex.CLIRun{}).Args
	cliCommand[0] = cli.Path
	cliRun, err := trace.Run(ctx, trace.Options{Command: cliCommand, Dir: dir, Stdin: strings.NewReader(sqlitex.OpenScript(path, autocommitScript(dir, options(mode))))})
	if err != nil {
		fatal("strace CLI", err)
	}
	if cliRun.ExitCode != 0 {
		fatal("CLI", fmt.Errorf("exit %d: %s", cliRun.ExitCode, cliRun.Stderr))
	}
	cliFileHash, err := recipe.FileSHA256(path)
	if err != nil {
		fatal("hash", err)
	}
	cliCalls, err := segmentCalls(cliRun.Log.Calls, dir, "1", "2")
	if err != nil {
		fatal("CLI segment", err)
	}
	fmt.Printf("C CLI, file syscalls of the same INSERT (%d, open/openat shown as open, fsync/fdatasync as sync):\n", len(cliCalls))
	for _, line := range normalizedAll(cliCalls, nil) {
		fmt.Printf("  %s\n", line)
	}
	fmt.Printf("INFO: sync syscall used: Go driver %s, CLI %s (%s)\n", syncCallNames(goCalls), syncCallNames(cliCalls), displayPath(cli.Path))
	writesSame := compareLists("writes (file, offset, length, order)", normalizedAll(goCalls, isWrite), normalizedAll(cliCalls, isWrite))
	syncsSame := compareLists("syncs (file, order)", normalizedAll(goCalls, isSync), normalizedAll(cliCalls, isSync))
	check(writesSame && syncsSame, "twin check: the Go driver and the C CLI make the same writes (file, offset, length) and the same syncs, in the same order")
	goCloexec := normalizedAll(goCalls, isCloexecFcntl)
	cliCloexec := normalizedAll(cliCalls, isCloexecFcntl)
	fmt.Printf("twin difference: fcntl F_SETFD calls, Go driver %d [%s], CLI %d [%s]\n", len(goCloexec), strings.Join(goCloexec, "; "), len(cliCloexec), strings.Join(cliCloexec, "; "))
	notCloexec := func(call trace.Syscall) bool { return !isCloexecFcntl(call) }
	allSame := compareLists("every other file syscall (opens, reads, writes, syncs, locks, unlinks, closes)", normalizedAll(goCalls, notCloexec), normalizedAll(cliCalls, notCloexec))
	check(allSame, "apart from fcntl F_SETFD, every file syscall of the INSERT is the same in both builds (open vs openat and fsync vs fdatasync are not distinguished)")
	check(goFileHash == cliFileHash, fmt.Sprintf("the database file after the INSERT is byte-identical from both builds (sha256 %s)", short(goFileHash)))

	// Measured call path: split the INSERT into BEGIN IMMEDIATE, INSERT, and
	// COMMIT under CLI -vfstrace + strace so each VFS call and syscall is
	// attributed to the opcode that caused it, then verify the combined
	// syscalls equal the autocommit INSERT's.
	createBase(ctx, path, mode)
	merged, splitRun, err := trace.RunCLIMerged(ctx, cli, trace.CLITrace{Dir: dir, DB: path, SQL: splitScript(dir, options(mode))}, trace.Options{})
	if err != nil {
		fatal("CLI -vfstrace under strace", err)
	}
	if splitRun.ExitCode != 0 {
		fatal("CLI -vfstrace", fmt.Errorf("exit %d: %s", splitRun.ExitCode, splitRun.Stderr))
	}
	splitFileHash, err := recipe.FileSHA256(path)
	if err != nil {
		fatal("hash", err)
	}
	fmt.Println("call path, measured: the C CLI with -vfstrace under strace runs BEGIN IMMEDIATE; INSERT; COMMIT with a marker between statements.")
	fmt.Println("Each phase lists the VFS calls (VFS) and the syscalls each one made (SYS, indented under its VFS call).")
	var concatenated []trace.Syscall
	for _, p := range phases(mode) {
		segment, err := mergedSegment(merged, dir, p.from, p.to)
		if err != nil {
			fatal("merged segment", err)
		}
		concatenated = append(concatenated, segment.calls...)
		fmt.Printf("phase %s | opcode %s | %s\n", p.statement, p.opcode, p.path)
		vfsCount := 0
		for _, event := range segment.events {
			switch event.Kind {
			case trace.EventVFS:
				if event.VFS.Method == "xRandomness" {
					// Different CLI builds seed SQLite's PRNG at different
					// points, so keep xRandomness on an INFO line.
					fmt.Printf("INFO: phase %s: VFS %s (SQLite seeds its PRNG on first use; this CLI build first needs randomness here)\n", p.statement, vfsLine(*event.VFS, dir))
					continue
				}
				vfsCount++
				fmt.Printf("  VFS %s\n", vfsLine(*event.VFS, dir))
			case trace.EventSyscall:
				relative := trace.RelativeTo(dir, []trace.Syscall{*event.Syscall})[0]
				indent := "  "
				if event.Enclosed >= 0 {
					indent = "      "
				}
				fmt.Printf("%sSYS %s\n", indent, normalized(relative))
			}
		}
		if vfsCount == 0 {
			fmt.Println("  (no VFS calls)")
		}
	}
	check(slices.Equal(normalizedAll(concatenated, nil), normalizedAll(cliCalls, nil)), "the three phases together make exactly the file syscalls of the autocommit INSERT (so the per-opcode split above is the INSERT's own I/O)")
	check(splitFileHash == cliFileHash, "BEGIN IMMEDIATE; INSERT; COMMIT leaves the same file bytes as the autocommit INSERT")
}

func printChanged(before, after []string, changed []int) {
	fmt.Printf("pages changed by the INSERT (sha256 of each page through sqlite_dbpage, before -> after): %v\n", changed)
	for _, page := range changed {
		var b, a string
		if page <= len(before) {
			b = before[page-1]
		}
		if page <= len(after) {
			a = after[page-1]
		}
		fmt.Printf("  page %d: %s -> %s\n", page, short(b), short(a))
	}
}

func main() {
	if len(os.Args) == 4 && os.Args[1] == "child-insert" {
		childInsert(os.Args[2], os.Args[3])
		return
	}
	ctx := context.Background()
	sqlitex.MustPinCheckMemory(ctx)
	work := sqlitex.MustWorkDir("1.2")
	base, err := filepath.EvalSymlinks(work)
	if err != nil {
		fatal("work dir", err)
	}
	fmt.Printf("INFO: work dir %s\n", displayPath(base))
	cli, cliErr := sqlitex.LocateCLI(ctx)
	haveCLI := cliErr == nil
	if haveCLI {
		fmt.Println(cli.PinLine())
	} else {
		fmt.Printf("sqlite3 CLI: unavailable: %v\n", cliErr)
		failures = append(failures, "the pinned sqlite3 CLI is available")
	}

	explainPath := filepath.Join(base, "explain.db")
	createBase(ctx, explainPath, sqlitex.JournalDelete)
	printExplain(ctx, explainPath)

	if !trace.Supported {
		trace.PrintLinuxOnly("the strace of the Go driver and of the CLI, the twin check and the measured call path", packagePath)
	}
	for _, mode := range []string{sqlitex.JournalDelete, sqlitex.JournalWAL} {
		runMode(ctx, base, mode, cli, haveCLI)
	}

	fmt.Println()
	if len(failures) > 0 {
		fmt.Printf("ONE INSERT FAILED: %d claim(s) do not hold: %s\n", len(failures), strings.Join(failures, "; "))
		os.Exit(1)
	}
	fmt.Println("ONE INSERT OK")
}

func displayPath(path string) string {
	root, err := sqlitex.RepoRoot()
	if err != nil {
		return path
	}
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	relative, err := filepath.Rel(root, path)
	if err != nil || strings.HasPrefix(relative, "..") {
		return path
	}
	return filepath.ToSlash(relative)
}
```

Run from the repository root (Linux, or via `docker compose run --rm lab` on macOS):

```text
go run ./ch01-architecture-and-execution-path/experiments/1.2-one-insert
```

Captured evidence (measured on Linux):

<!-- include: evidence/1.2-one-insert.txt -->
```text
sqlite_source_id: 2026-07-24 19:02:57 bf7c7f30031888f4e796e429ab3978879485813aaca6f641c7b33e4e09459bcc (pin 3.53.4 ok)
INFO: work dir _work/1.2
sqlite3 CLI sqlite_source_id: 2026-07-24 19:02:57 bf7c7f30031888f4e796e429ab3978879485813aaca6f641c7b33e4e09459bcc (pin 3.53.4 ok)
EXPLAIN INSERT INTO t(id, v) VALUES(2, 'two')
  addr opcode           p1   p2   p3 p4           p5
  0    Init              0   14    0              0
  1    OpenWrite         0    2    0 2            0
  2    SoftNull          2    0    0              0
  3    String8           0    3    0 two          0
  4    Integer           2    1    0              0
  5    NotNull           1    7    0              0
  6    NewRowid          0    1    0              0
  7    MustBeInt         1    0    0              0
  8    HaltIfNull     1299    2    3 t.v          1
  9    NotExists         0   11    1              0
  10   Halt           1555    2    0 t.id         2
  11   MakeRecord        2    2    4 DB           0
  12   Insert            0    4    1 t            49
  13   Halt              0    0    0              0
  14   Transaction       0    1    1 0            1
  15   Goto              0    1    0              0

== rollback journal (journal_mode=delete) ==
base: table t with 1 row, 2 pages, sha256 0aec0e95a3f65e0c
pages changed by the INSERT (sha256 of each page through sqlite_dbpage, before -> after): [1 2]
  page 1: bb133eb73b58bce5 -> 0ba56047337c7717
  page 2: 5d68aacbd116afe8 -> b21207c95d70a25b
Go driver (modernc), file syscalls of the INSERT (33):
  fcntl t.db F_SETLK F_RDLCK start=1073741824 len=1 = 0
  fcntl t.db F_SETLK F_RDLCK start=1073741826 len=510 = 0
  fcntl t.db F_SETLK F_UNLCK start=1073741824 len=1 = 0
  pread64 t.db off=24 len=16 = 16
  fcntl t.db F_SETLK F_WRLCK start=1073741825 len=1 = 0
  pread64 t.db off=4096 len=4096 = 4096
  open t.db-journal O_RDWR|O_CREAT|O_LARGEFILE|O_NOFOLLOW|O_CLOEXEC = fd
  fcntl t.db-journal F_SETFD = 0
  pwrite64 t.db-journal off=0 len=512 = 512
  pwrite64 t.db-journal off=512 len=4 = 4
  pwrite64 t.db-journal off=516 len=4096 = 4096
  pwrite64 t.db-journal off=4612 len=4 = 4
  fcntl t.db F_SETLK F_WRLCK start=1073741824 len=1 = 0
  fcntl t.db F_SETLK F_WRLCK start=1073741826 len=510 = 0
  pwrite64 t.db-journal off=4616 len=4 = 4
  pwrite64 t.db-journal off=4620 len=4096 = 4096
  pwrite64 t.db-journal off=8716 len=4 = 4
  pread64 t.db-journal off=9216 len=8 = 0
  fsync t.db-journal = 0
  open . O_RDONLY|O_LARGEFILE|O_CLOEXEC = fd
  fcntl . F_SETFD = 0
  fsync . = 0
  close . = 0
  pwrite64 t.db-journal off=0 len=12 = 12
  fsync t.db-journal = 0
  pwrite64 t.db off=0 len=4096 = 4096
  pwrite64 t.db off=4096 len=4096 = 4096
  fsync t.db = 0
  close t.db-journal = 0
  unlink t.db-journal = 0
  fcntl t.db F_SETLK F_RDLCK start=1073741826 len=510 = 0
  fcntl t.db F_SETLK F_UNLCK start=1073741824 len=2 = 0
  fcntl t.db F_SETLK F_UNLCK start=0 len=0 = 0
  db write at offset 0 = (page 1 - 1) * 4096
  db write at offset 4096 = (page 2 - 1) * 4096
check ok: rollback mode: each pwrite64 on t.db is one whole page at offset (page-1)*4096, and the pages written are exactly the changed pages [1 2]
C CLI, file syscalls of the same INSERT (31, open/openat shown as open, fsync/fdatasync as sync):
  fcntl t.db F_SETLK F_RDLCK start=1073741824 len=1
  fcntl t.db F_SETLK F_RDLCK start=1073741826 len=510
  fcntl t.db F_SETLK F_UNLCK start=1073741824 len=1
  pread64 t.db off=24 len=16
  fcntl t.db F_SETLK F_WRLCK start=1073741825 len=1
  pread64 t.db off=4096 len=4096
  open t.db-journal
  pwrite64 t.db-journal off=0 len=512
  pwrite64 t.db-journal off=512 len=4
  pwrite64 t.db-journal off=516 len=4096
  pwrite64 t.db-journal off=4612 len=4
  fcntl t.db F_SETLK F_WRLCK start=1073741824 len=1
  fcntl t.db F_SETLK F_WRLCK start=1073741826 len=510
  pwrite64 t.db-journal off=4616 len=4
  pwrite64 t.db-journal off=4620 len=4096
  pwrite64 t.db-journal off=8716 len=4
  pread64 t.db-journal off=9216 len=8
  sync t.db-journal
  open .
  sync .
  close .
  pwrite64 t.db-journal off=0 len=12
  sync t.db-journal
  pwrite64 t.db off=0 len=4096
  pwrite64 t.db off=4096 len=4096
  sync t.db
  close t.db-journal
  unlink t.db-journal
  fcntl t.db F_SETLK F_RDLCK start=1073741826 len=510
  fcntl t.db F_SETLK F_UNLCK start=1073741824 len=2
  fcntl t.db F_SETLK F_UNLCK start=0 len=0
INFO: sync syscall used: Go driver fsync, CLI fsync (/usr/local/bin/sqlite3)
twin writes (file, offset, length, order): identical (10 calls)
twin syncs (file, order): identical (4 calls)
check ok: twin check: the Go driver and the C CLI make the same writes (file, offset, length) and the same syncs, in the same order
twin difference: fcntl F_SETFD calls, Go driver 2 [fcntl t.db-journal F_SETFD; fcntl . F_SETFD], CLI 0 []
twin every other file syscall (opens, reads, writes, syncs, locks, unlinks, closes): identical (31 calls)
check ok: apart from fcntl F_SETFD, every file syscall of the INSERT is the same in both builds (open vs openat and fsync vs fdatasync are not distinguished)
check ok: the database file after the INSERT is byte-identical from both builds (sha256 47bae5ed0c8207f2)
call path, measured: the C CLI with -vfstrace under strace runs BEGIN IMMEDIATE; INSERT; COMMIT with a marker between statements.
Each phase lists the VFS calls (VFS) and the syscalls each one made (SYS, indented under its VFS call).
phase BEGIN IMMEDIATE | opcode OP_Transaction | sqlite3BtreeBeginTrans -> sqlite3PagerSharedLock, sqlite3PagerBegin
  VFS xLock(t.db,SHARED) -> SQLITE_OK
      SYS fcntl t.db F_SETLK F_RDLCK start=1073741824 len=1
      SYS fcntl t.db F_SETLK F_RDLCK start=1073741826 len=510
      SYS fcntl t.db F_SETLK F_UNLCK start=1073741824 len=1
  VFS xAccess("t.db-journal",0) -> SQLITE_OK, out=0
  VFS xRead(t.db,n=16,ofst=24) -> SQLITE_OK
      SYS pread64 t.db off=24 len=16
  VFS xAccess("t.db-wal",0) -> SQLITE_OK, out=0
  VFS xFileSize(t.db) -> SQLITE_OK, size=8192
  VFS xLock(t.db,RESERVED) -> SQLITE_OK
      SYS fcntl t.db F_SETLK F_WRLCK start=1073741825 len=1
phase INSERT INTO t(id, v) VALUES(2, 'two') | opcode OP_Insert | sqlite3BtreeInsert -> sqlite3PagerWrite
  VFS xRead(t.db,n=4096,ofst=4096) -> SQLITE_OK
      SYS pread64 t.db off=4096 len=4096
  VFS xFileControl(t.db,HAS_MOVED) -> SQLITE_OK, 0
  SYS open t.db-journal
  VFS xOpen(t.db-journal,flags=0x806) -> SQLITE_OK
  VFS xDeviceCharacteristics(t.db) -> 0x00009000
  VFS xWrite(t.db-journal,n=512,ofst=0) -> SQLITE_OK
      SYS pwrite64 t.db-journal off=0 len=512
  VFS xWrite(t.db-journal,n=4,ofst=512) -> SQLITE_OK
      SYS pwrite64 t.db-journal off=512 len=4
  VFS xWrite(t.db-journal,n=4096,ofst=516) -> SQLITE_OK
      SYS pwrite64 t.db-journal off=516 len=4096
  VFS xWrite(t.db-journal,n=4,ofst=4612) -> SQLITE_OK
      SYS pwrite64 t.db-journal off=4612 len=4
phase COMMIT | opcode OP_AutoCommit (OP_Halt in an autocommit INSERT) | sqlite3VdbeHalt -> vdbeCommit -> sqlite3BtreeCommitPhaseOne/PhaseTwo -> sqlite3PagerCommitPhaseOne/PhaseTwo
  VFS xLock(t.db,EXCLUSIVE) -> SQLITE_OK
      SYS fcntl t.db F_SETLK F_WRLCK start=1073741824 len=1
      SYS fcntl t.db F_SETLK F_WRLCK start=1073741826 len=510
  VFS xWrite(t.db-journal,n=4,ofst=4616) -> SQLITE_OK
      SYS pwrite64 t.db-journal off=4616 len=4
  VFS xWrite(t.db-journal,n=4096,ofst=4620) -> SQLITE_OK
      SYS pwrite64 t.db-journal off=4620 len=4096
  VFS xWrite(t.db-journal,n=4,ofst=8716) -> SQLITE_OK
      SYS pwrite64 t.db-journal off=8716 len=4
  VFS xDeviceCharacteristics(t.db) -> 0x00009000
  VFS xRead(t.db-journal,n=8,ofst=9216) -> SQLITE_IOERR_SHORT_READ
      SYS pread64 t.db-journal off=9216 len=8
  VFS xSync(t.db-journal,FULL) -> 0
      SYS sync t.db-journal
      SYS open .
      SYS sync .
      SYS close .
  VFS xWrite(t.db-journal,n=12,ofst=0) -> SQLITE_OK
      SYS pwrite64 t.db-journal off=0 len=12
  VFS xSync(t.db-journal,FULL) -> 0
      SYS sync t.db-journal
  VFS xWrite(t.db,n=4096,ofst=0) -> SQLITE_OK
      SYS pwrite64 t.db off=0 len=4096
  VFS xWrite(t.db,n=4096,ofst=4096) -> SQLITE_OK
      SYS pwrite64 t.db off=4096 len=4096
  VFS xFileControl(t.db,SYNC) -> SQLITE_NOTFOUND
  VFS xSync(t.db,FULL) -> 0
      SYS sync t.db
  VFS xClose(t.db-journal) -> SQLITE_OK
      SYS close t.db-journal
  VFS xDelete("t.db-journal",0) -> SQLITE_OK
      SYS unlink t.db-journal
  VFS xFileControl(t.db,COMMIT_PHASETWO) -> SQLITE_NOTFOUND
  VFS xUnlock(t.db,SHARED) -> SQLITE_OK
      SYS fcntl t.db F_SETLK F_RDLCK start=1073741826 len=510
      SYS fcntl t.db F_SETLK F_UNLCK start=1073741824 len=2
  VFS xDeviceCharacteristics(t.db) -> 0x00009000
  VFS xUnlock(t.db,NONE) -> SQLITE_OK
      SYS fcntl t.db F_SETLK F_UNLCK start=0 len=0
check ok: the three phases together make exactly the file syscalls of the autocommit INSERT (so the per-opcode split above is the INSERT's own I/O)
check ok: BEGIN IMMEDIATE; INSERT; COMMIT leaves the same file bytes as the autocommit INSERT

== write-ahead log (journal_mode=wal, the platform default) ==
base: table t with 1 row, 2 pages, sha256 d0cb3c980847e49e
pages changed by the INSERT (sha256 of each page through sqlite_dbpage, before -> after): [2]
  page 2: 5d68aacbd116afe8 -> b21207c95d70a25b
Go driver (modernc), file syscalls of the INSERT (18):
  fcntl t.db-shm F_SETLK F_RDLCK start=123 len=1 = 0
  fcntl t.db-shm F_SETLK F_WRLCK start=120 len=1 = 0
  pread64 t.db off=4096 len=4096 = 4096
  fcntl t.db-shm F_SETLK F_UNLCK start=123 len=1 = 0
  fcntl t.db-shm F_SETLK F_WRLCK start=124 len=1 = 0
  fcntl t.db-shm F_SETLK F_UNLCK start=124 len=1 = 0
  fcntl t.db-shm F_SETLK F_RDLCK start=124 len=1 = 0
  pwrite64 t.db-wal off=0 len=32 = 32
  fsync t.db-wal = 0
  open . O_RDONLY|O_LARGEFILE|O_CLOEXEC = fd
  fcntl . F_SETFD = 0
  fsync . = 0
  close . = 0
  pwrite64 t.db-wal off=32 len=24 = 24
  pwrite64 t.db-wal off=56 len=4096 = 4096
  fsync t.db-wal = 0
  fcntl t.db-shm F_SETLK F_UNLCK start=120 len=1 = 0
  fcntl t.db-shm F_SETLK F_UNLCK start=124 len=1 = 0
check ok: WAL mode: the commit writes nothing to t.db itself (database-file pwrite64 calls during the INSERT: 0)
  WAL frame 1: page 2, commit size 2, header at offset 32, page image at offset 56
check ok: WAL mode: the commit appends one frame per changed page [2] to t.db-wal (frame header 24 bytes, then the page at offset 32 + (frame-1)*4120 + 24), the last frame carries the commit size
C CLI, file syscalls of the same INSERT (17, open/openat shown as open, fsync/fdatasync as sync):
  fcntl t.db-shm F_SETLK F_RDLCK start=123 len=1
  fcntl t.db-shm F_SETLK F_WRLCK start=120 len=1
  pread64 t.db off=4096 len=4096
  fcntl t.db-shm F_SETLK F_UNLCK start=123 len=1
  fcntl t.db-shm F_SETLK F_WRLCK start=124 len=1
  fcntl t.db-shm F_SETLK F_UNLCK start=124 len=1
  fcntl t.db-shm F_SETLK F_RDLCK start=124 len=1
  pwrite64 t.db-wal off=0 len=32
  sync t.db-wal
  open .
  sync .
  close .
  pwrite64 t.db-wal off=32 len=24
  pwrite64 t.db-wal off=56 len=4096
  sync t.db-wal
  fcntl t.db-shm F_SETLK F_UNLCK start=120 len=1
  fcntl t.db-shm F_SETLK F_UNLCK start=124 len=1
INFO: sync syscall used: Go driver fsync, CLI fsync (/usr/local/bin/sqlite3)
twin writes (file, offset, length, order): identical (3 calls)
twin syncs (file, order): identical (3 calls)
check ok: twin check: the Go driver and the C CLI make the same writes (file, offset, length) and the same syncs, in the same order
twin difference: fcntl F_SETFD calls, Go driver 1 [fcntl . F_SETFD], CLI 0 []
twin every other file syscall (opens, reads, writes, syncs, locks, unlinks, closes): identical (17 calls)
check ok: apart from fcntl F_SETFD, every file syscall of the INSERT is the same in both builds (open vs openat and fsync vs fdatasync are not distinguished)
check ok: the database file after the INSERT is byte-identical from both builds (sha256 6b96a8a5533900b8)
call path, measured: the C CLI with -vfstrace under strace runs BEGIN IMMEDIATE; INSERT; COMMIT with a marker between statements.
Each phase lists the VFS calls (VFS) and the syscalls each one made (SYS, indented under its VFS call).
phase BEGIN IMMEDIATE | opcode OP_Transaction | sqlite3BtreeBeginTrans -> sqlite3PagerSharedLock, sqlite3PagerBegin
  VFS xShmBarrier(t.db)
  VFS xShmLock(t.db,ofst=3(READ0),n=1,LOCK|SHARED) -> SQLITE_OK
      SYS fcntl t.db-shm F_SETLK F_RDLCK start=123 len=1
  VFS xShmBarrier(t.db)
  VFS xFileSize(t.db) -> SQLITE_OK, size=8192
  VFS xShmLock(t.db,ofst=0(WRITE),n=1,LOCK|EXCLUSIVE) -> SQLITE_OK
      SYS fcntl t.db-shm F_SETLK F_WRLCK start=120 len=1
phase INSERT INTO t(id, v) VALUES(2, 'two') | opcode OP_Insert | sqlite3BtreeInsert -> sqlite3PagerWrite
  VFS xRead(t.db,n=4096,ofst=4096) -> SQLITE_OK
      SYS pread64 t.db off=4096 len=4096
phase COMMIT | opcode OP_AutoCommit (OP_Halt in an autocommit INSERT) | sqlite3VdbeHalt -> vdbeCommit -> sqlite3BtreeCommitPhaseOne -> sqlite3PagerCommitPhaseOne -> pagerWalFrames -> sqlite3WalFrames
  VFS xShmLock(t.db,ofst=3(READ0),n=1,UNLOCK|SHARED) -> SQLITE_OK
      SYS fcntl t.db-shm F_SETLK F_UNLCK start=123 len=1
  VFS xShmLock(t.db,ofst=4(READ1),n=1,LOCK|EXCLUSIVE) -> SQLITE_OK
      SYS fcntl t.db-shm F_SETLK F_WRLCK start=124 len=1
  VFS xShmLock(t.db,ofst=4(READ1),n=1,UNLOCK|EXCLUSIVE) -> SQLITE_OK
      SYS fcntl t.db-shm F_SETLK F_UNLCK start=124 len=1
  VFS xShmLock(t.db,ofst=4(READ1),n=1,LOCK|SHARED) -> SQLITE_OK
      SYS fcntl t.db-shm F_SETLK F_RDLCK start=124 len=1
  VFS xShmBarrier(t.db)
  VFS xWrite(t.db-wal,n=32,ofst=0) -> SQLITE_OK
      SYS pwrite64 t.db-wal off=0 len=32
  VFS xSync(t.db-wal,FULL) -> 0
      SYS sync t.db-wal
      SYS open .
      SYS sync .
      SYS close .
  VFS xWrite(t.db-wal,n=24,ofst=32) -> SQLITE_OK
      SYS pwrite64 t.db-wal off=32 len=24
  VFS xWrite(t.db-wal,n=4096,ofst=56) -> SQLITE_OK
      SYS pwrite64 t.db-wal off=56 len=4096
  VFS xSync(t.db-wal,FULL) -> 0
      SYS sync t.db-wal
  VFS xShmBarrier(t.db)
  VFS xShmLock(t.db,ofst=0(WRITE),n=1,UNLOCK|EXCLUSIVE) -> SQLITE_OK
      SYS fcntl t.db-shm F_SETLK F_UNLCK start=120 len=1
  VFS xFileControl(t.db,COMMIT_PHASETWO) -> SQLITE_NOTFOUND
  VFS xShmLock(t.db,ofst=4(READ1),n=1,UNLOCK|SHARED) -> SQLITE_OK
      SYS fcntl t.db-shm F_SETLK F_UNLCK start=124 len=1
check ok: the three phases together make exactly the file syscalls of the autocommit INSERT (so the per-opcode split above is the INSERT's own I/O)
check ok: BEGIN IMMEDIATE; INSERT; COMMIT leaves the same file bytes as the autocommit INSERT

ONE INSERT OK
```

#### Reading the evidence

1. **The `EXPLAIN` bytecode listing (lines 4–21).** Each row is one VDBE instruction: `addr` (instruction index), `opcode` (operation name), and operands `p1` through `p5`. Execution starts at `0 Init 0 14` which jumps to `14 Transaction 0 1 1 0 1` (start a write transaction `p2=1` on database `p1=0` `main`, checking schema cookie `p3=1` because `p5=1`), then `15 Goto 0 1` jumps back to `1`:
   - `1 OpenWrite 0 2 0 2` opens write cursor `0` on root page `2` (table `t`, 2 columns).
   - `2 SoftNull 2`, `3 String8 0 3 0 two`, `4 Integer 2 1` load register `r2=NULL` (the `INTEGER PRIMARY KEY` column is stored as the rowid, not duplicated in the record), `r3='two'`, and rowid `r1=2`.
   - `5 NotNull 1 7` skips `6 NewRowid` because rowid `2` was supplied explicitly.
   - `7 MustBeInt 1` validates the rowid type; `8 HaltIfNull 1299 2 3 t.v 1` enforces `v TEXT NOT NULL`; `9 NotExists 0 11 1` checks that rowid `2` is not already in table `t` (otherwise `10 Halt 1555` raises a primary-key constraint error).
   - `11 MakeRecord 2 2 4 DB` packs registers `r2..r3` into a binary SQLite record in register `r4`.
   - `12 Insert 0 4 1 t 49` inserts record `r4` with rowid `r1` into cursor `0`. Flag `p5=49` is `1 + 16 + 32` (`OPFLAG_NCHANGE | OPFLAG_USESEEKRESULT | OPFLAG_LASTROWID`): reuse the seek position from `NotExists`, increment the change counter, and record `last_insert_rowid`.
   - `13 Halt` invokes `sqlite3VdbeHalt`, which commits the autocommit transaction.

2. **Rollback mode (`journal_mode=delete`, lines 23–104).**
   - **Changed pages (lines 25–27):** `[1 2]`. Page 2 changed because row 2 was inserted into table `t`'s leaf page. Page 1 changed because in rollback mode a commit increments the 4-byte file change counter (bytes 24–27) and the matching version-valid-for field (bytes 92–95) in page 1's header.
   - **Database writes (lines 54–55, 62–64):** `pwrite64 t.db off=0 len=4096` equals `(page 1 - 1) * 4096`, and `pwrite64 t.db off=4096 len=4096` equals `(page 2 - 1) * 4096`. Every write to `t.db` is one whole page, and the pages written are exactly `[1 2]`.
   - **Syncs (lines 47, 50, 53, 56):** 4 syncs in order: `fsync t.db-journal`, `fsync .` (the directory, making the newly created journal entry durable), `fsync t.db-journal` (after writing the 12-byte journal header with the page count), and `fsync t.db`.
   - **Twin check (lines 97–104):** The Go driver made 33 file syscalls; the C CLI made 31. Both made the exact same 10 writes (same file, offset, length, order), the exact same 4 syncs (same file, order), and the exact same 31 non-`F_SETFD` calls, and left byte-identical files (`47bae5ed0c8207f2`). The sole syscall difference (line 101) is two `fcntl(..., F_SETFD)` calls from `modernc.org/libc v1.77.1`: its musl-derived `open()` issues `fcntl(fd, F_SETFD, FD_CLOEXEC)` after opening a file with `O_CLOEXEC`, whereas `os_unix.c`'s `robust_open` relies on `O_CLOEXEC` directly. Note also that `./configure`-built CLIs call `fdatasync` where the Go driver and system CLI call `fsync`, and `openat` vs `open` varies by libc; the experiment normalizes those pairs and records the raw sync call name on the `INFO:` line (line 97).

3. **WAL mode (`journal_mode=wal`, the platform default, lines 177–228).**
   - **Changed pages (lines 179–180):** `[2]` only. Page 1 does not change during this `INSERT` in WAL mode.
   - **Database writes vs WAL writes (lines 189–202):** Zero `pwrite64` calls hit `t.db`. Because closing the base database checkpointed and deleted its WAL, this `INSERT` starts a fresh WAL: it writes the 32-byte WAL header at `t.db-wal off=0 len=32`, syncs `t.db-wal` and the directory `.`, appends frame 1's 24-byte header at `off=32 len=24` and page 2's 4,096-byte image at `off=56 len=4096`, and issues the commit sync `fsync t.db-wal`. Reading `t.db-wal` before `db.Close()` confirms `frame 1: page 2, commit size 2` (line 201).
   - **Syncs (lines 190, 193, 197):** 3 syncs total (`t.db-wal`, `.`, `t.db-wal`). The first two happen only because this is the first write to a brand-new WAL file; only the third sync is the per-commit sync at `synchronous=FULL`.
   - **Twin check (lines 221–228):** 18 Go syscalls vs 17 CLI syscalls (the single difference is `fcntl . F_SETFD`, line 225). Writes (3), syncs (3), all 17 other syscalls, and the closed database file bytes (`6b96a8a5533900b8`) match identically.
   - **Marker files and `xRandomness`:** Both processes bracket each statement by opening empty marker files (`marker-1`, `marker-2`, and so on) because CLI stdout is buffered; an `open`/`openat` syscall lands in `strace` at the exact statement boundary. Some CLI builds also emit `VFS xRandomness` on the first statement that needs SQLite's PRNG (to seed the rollback journal nonce or WAL salts); the experiment prints that on an `INFO:` line so the `EXACT` output matches across both the installed CLI and the `build-tools` CLI.

---

## 4. Timeline

In Experiment 1.2, the third run per mode splits the autocommit `INSERT` into `BEGIN IMMEDIATE;`, `INSERT INTO t(id, v) VALUES(2, 'two');`, and `COMMIT;` under the C CLI's `-vfstrace` plus `strace`. Lines 174–175 and 271–272 verify that the three phases concatenated produce the exact same syscalls and the exact same final file bytes as the single autocommit `INSERT`. That links each VFS call and kernel syscall by measurement—not guesswork—to the opcode that triggered it: `BEGIN IMMEDIATE` isolates `OP_Transaction`, `INSERT` isolates `OP_Insert`, and `COMMIT` isolates `OP_AutoCommit` → `sqlite3VdbeHalt` → `vdbeCommit` (the same commit path `OP_Halt` takes in an autocommit `INSERT`).

### 4.1 Rollback mode (`journal_mode=delete`)

POSIX byte-range locks in rollback mode sit on `t.db` at the 1 GiB lock-byte region: `1073741824` (`0x40000000`, `PENDING` byte), `1073741825` (`RESERVED` byte), and `1073741826..1073742335` (`len=510`, the `SHARED` range).

| Phase (opcode) | Layer path | VFS call (`-vfstrace`) | Kernel syscall(s) (`strace`) |
| --- | --- | --- | --- |
| `BEGIN IMMEDIATE` (`OP_Transaction`) | `sqlite3BtreeBeginTrans` → `sqlite3PagerSharedLock` | `xLock(t.db,SHARED)` | `fcntl t.db F_RDLCK 1073741824 len=1`, `F_RDLCK 1073741826 len=510`, `F_UNLCK 1073741824 len=1` |
| 〃 | Check for hot journal and verify cache against page 1 bytes 24–39 | `xAccess("t.db-journal")=0`, `xRead(t.db,n=16,ofst=24)`, `xAccess("t.db-wal")=0`, `xFileSize(t.db)=8192` | `pread64 t.db off=24 len=16` |
| 〃 | `sqlite3PagerBegin` (acquire writer intent) | `xLock(t.db,RESERVED)` | `fcntl t.db F_WRLCK 1073741825 len=1` |
| `INSERT` (`OP_Insert`) | `sqlite3BtreeInsert` → read page 2 → `sqlite3PagerWrite` | `xRead(t.db,n=4096,ofst=4096)` | `pread64 t.db off=4096 len=4096` |
| 〃 | `pager_open_journal` creates journal and writes 512-byte header | `xFileControl(HAS_MOVED)`, `xOpen(t.db-journal)`, `xDeviceCharacteristics`, `xWrite(t.db-journal,n=512,ofst=0)` | `open t.db-journal`, `pwrite64 t.db-journal off=0 len=512` |
| 〃 | Journal page 2's original image (4-byte page number + 4,096-byte page + 4-byte checksum) | `xWrite` ×3 (`ofst=512, 516, 4612`) | `pwrite64 t.db-journal off=512 len=4`, `off=516 len=4096`, `off=4612 len=4` |
| `COMMIT` (`OP_AutoCommit` / `OP_Halt`) | `sqlite3VdbeHalt` → `vdbeCommit` → `sqlite3PagerExclusiveLock` | `xLock(t.db,EXCLUSIVE)` | `fcntl t.db F_WRLCK 1073741824 len=1`, `F_WRLCK 1073741826 len=510` |
| 〃 | `sqlite3PagerCommitPhaseOne` → `pager_incr_changecounter` journals page 1 | `xWrite` ×3 (`ofst=4616, 4620, 8716`) | `pwrite64 t.db-journal off=4616 len=4`, `off=4620 len=4096`, `off=8716 len=4` |
| 〃 | `syncJournal`: check for stale next header, sync journal records and directory | `xRead(t.db-journal,n=8,ofst=9216)` → `SHORT_READ`, `xSync(t.db-journal,FULL)` | `pread64 t.db-journal off=9216 len=8`, `sync t.db-journal`, `open .`, `sync .`, `close .` |
| 〃 | `syncJournal`: write page count into journal header and sync header | `xWrite(t.db-journal,n=12,ofst=0)`, `xSync(t.db-journal,FULL)` | `pwrite64 t.db-journal off=0 len=12`, `sync t.db-journal` |
| 〃 | `pager_write_pagelist` writes modified pages 1 and 2 to `t.db`, then `sqlite3PagerSync` | `xWrite(t.db,4096,0)`, `xWrite(t.db,4096,4096)`, `xFileControl(SYNC)`, `xSync(t.db,FULL)` | `pwrite64 t.db off=0 len=4096`, `off=4096 len=4096`, `sync t.db` |
| 〃 | `sqlite3BtreeCommitPhaseTwo` → `pager_end_transaction` (**commit point**) | `xClose(t.db-journal)`, `xDelete("t.db-journal")` | `close t.db-journal`, `unlink t.db-journal` |
| 〃 | Drop exclusive lock back to `SHARED`, then `NONE` | `xFileControl(COMMIT_PHASETWO)`, `xUnlock(SHARED)`, `xUnlock(NONE)` | `fcntl t.db F_RDLCK 1073741826 len=510`, `F_UNLCK 1073741824 len=2`, `F_UNLCK 0 len=0` |

In rollback mode, `unlink t.db-journal` is the commit point: if the process dies before that `unlink`, the synced `-journal` remains on disk and the next connection rolls back both pages; after `unlink`, the new pages in `t.db` stand.

### 4.2 WAL mode (`journal_mode=wal`, the platform default)

POSIX byte-range locks in WAL mode sit on `t.db-shm`: byte `120` (`WRITE`), `121` (`CKPT`), `122` (`RECOVER`), and `123 + i` (`READ i`, five reader mark slots).

| Phase (opcode) | Layer path | VFS call (`-vfstrace`) | Kernel syscall(s) (`strace`) |
| --- | --- | --- | --- |
| `BEGIN IMMEDIATE` (`OP_Transaction`) | `sqlite3BtreeBeginTrans` → `walTryBeginRead` | `xShmBarrier`, `xShmLock(READ0, LOCK\|SHARED)`, `xShmBarrier`, `xFileSize(t.db)` | `fcntl t.db-shm F_RDLCK start=123 len=1` |
| 〃 | `sqlite3WalBeginWriteTransaction` | `xShmLock(WRITE, LOCK\|EXCLUSIVE)` | `fcntl t.db-shm F_WRLCK start=120 len=1` |
| `INSERT` (`OP_Insert`) | `sqlite3BtreeInsert` → `sqlite3PagerWrite` (no journal file in WAL mode) | `xRead(t.db,n=4096,ofst=4096)` | `pread64 t.db off=4096 len=4096` |
| `COMMIT` (`OP_AutoCommit` / `OP_Halt`) | `vdbeCommit` → `pagerWalFrames` → `sqlite3WalFrames` → `walRestartLog` moves reader mark from `READ0` to `READ1` | `xShmLock(READ0, UNLOCK\|SHARED)`, `xShmLock(READ1, LOCK\|EXCLUSIVE)`, `xShmLock(READ1, UNLOCK\|EXCLUSIVE)`, `xShmLock(READ1, LOCK\|SHARED)`, `xShmBarrier` | `fcntl t.db-shm F_UNLCK 123`, `F_WRLCK 124`, `F_UNLCK 124`, `F_RDLCK 124` |
| 〃 | Write and sync the 32-byte WAL header (first write of a new WAL) and sync directory `.` | `xWrite(t.db-wal,n=32,ofst=0)`, `xSync(t.db-wal,FULL)` | `pwrite64 t.db-wal off=0 len=32`, `sync t.db-wal`, `open .`, `sync .`, `close .` |
| 〃 | Append frame 1 (24-byte header + page 2 image) and sync (**commit point**) | `xWrite(t.db-wal,n=24,ofst=32)`, `xWrite(t.db-wal,n=4096,ofst=56)`, `xSync(t.db-wal,FULL)` | `pwrite64 t.db-wal off=32 len=24`, `off=56 len=4096`, `sync t.db-wal` |
| 〃 | Publish to wal-index and release `WRITE` and `READ1` locks | `xShmBarrier`, `xShmLock(WRITE, UNLOCK\|EXCLUSIVE)`, `xFileControl(COMMIT_PHASETWO)`, `xShmLock(READ1, UNLOCK\|SHARED)` | `fcntl t.db-shm F_UNLCK start=120 len=1`, `F_UNLCK start=124 len=1` |

Two structural differences stand out immediately: `OP_Insert` performs zero writes in WAL mode (only reading page 2 into the page cache), and `COMMIT` never touches `t.db` at all. All durability moves onto `t.db-wal`.

---

## 5. Lab

To verify snapshots, WAL replays, crash recoveries, and fleet rebuilds in chapters 2 through 10, every experiment needs a ground truth: a realistic workload whose database file has one known SHA-256 hash after every numbered step *k*. That is **Rebuild Ladder rung R0 (Recipe)**.

This chapter introduces the shared package `internal/recipe`, inlined in full below (its unit tests live in [`internal/recipe/recipe_test.go`](../internal/recipe/recipe_test.go) and run with `go test ./internal/recipe/`).

### 5.1 The `internal/recipe` package

A `Recipe` is identified by three values: a 64-bit PRNG seed (`DefaultSeed = 1`), a step count (`DefaultSteps = 30`), and a journal mode (`wal` by default, or `delete`).

- **Step 0** creates three tables—`accounts`, `documents`, and `events`—and sets `PRAGMA user_version = 0`. Every table declares `id INTEGER PRIMARY KEY` so Go supplies every rowid explicitly.
- **Steps 1 through 30** each seed a fresh `math/rand/v2` PCG generator from `(seed, k)` so step *k* depends only on the seed and its index, never on the total step count. Each step runs inserts across all three tables, balance updates, occasional document body replacements, periodic deletes (every 3rd step for events, every 4th step for documents), creates the index `events_by_account` at step 5, and ends with `PRAGMA user_version = k` inside the same transaction.
- **Overflow pages.** Document bodies range from `MinBodyBytes = 2000` to `MaxBodyBytes = 16000` bytes. On a 4,096-byte page, a table leaf row spills to overflow pages above 4,061 bytes of payload, so the workload exercises both inline rows and multi-page overflow chains.
- **Explicit settings and one session per step.** `SQLiteOptions()` sets `secure_delete=ON` and `SingleConnection=true` on top of `sqlitex.DefaultOptions()` (`page_size=4096`, `auto_vacuum=NONE`, `encoding=UTF-8`, course `application_id=1397836120`, `synchronous=FULL`, `foreign_keys=ON`, `cache_size=-2000`, `wal_autocheckpoint=1000`, `busy_timeout=5000ms`). `Build` opens and closes a fresh connection for every step (`ApplyInSession`). Section 6 proves why: in WAL mode, SQLite's file change counter counts connection sessions rather than commits.
- **Step hash cache and lookup.** `hashes.go` computes both the exact SHA-256 and a change-counter-masked SHA-256 (zeroing header bytes 24–27 and 92–95) after every step, caches the list under `_work/recipe-cache/`, and provides `MatchStep` so later chapters can identify which recipe step any at-rest database equals.

#### `internal/recipe/recipe.go`

<!-- include: ../internal/recipe/recipe.go -->
```go
// Package recipe generates and applies a seeded, deterministic SQLite workload.
//
// Every step k is one transaction ending in PRAGMA user_version = k. Running
// the same recipe produces byte-identical database files at rest, giving every
// chapter a known ground-truth SHA-256 hash after each step (Rebuild Ladder
// rung R0).
package recipe

import (
	"fmt"
	"math/rand/v2"
	"strings"

	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
)

// Default recipe parameters and bounds.
const (
	// DefaultSeed is the PRNG seed used by Default.
	DefaultSeed uint64 = 1
	// DefaultSteps is the number of workload steps after schema step 0.
	DefaultSteps = 30
	// IndexStep is the step that creates the secondary index on events.
	IndexStep = 5
	// MaxSteps caps the step count accepted by Validate.
	MaxSteps = 10000
)

// FormatVersion versions the on-disk step-hash cache. Bump it whenever the
// generated SQL, parameters, or SQLite options change.
const FormatVersion = 1

// Recipe identifies a deterministic workload by PRNG seed, step count, and
// journal mode ("wal" or "delete").
type Recipe struct {
	Seed        uint64
	Steps       int
	JournalMode string
}

// Default returns the standard course recipe: seed 1, 30 workload steps, and
// WAL mode (the platform default).
func Default() Recipe {
	return Recipe{Seed: DefaultSeed, Steps: DefaultSteps, JournalMode: sqlitex.JournalWAL}
}

// WithSteps returns a copy of r with Steps replaced by steps.
func (r Recipe) WithSteps(steps int) Recipe {
	r.Steps = steps
	return r
}

// WithJournalMode returns a copy of r with JournalMode replaced by mode.
func (r Recipe) WithJournalMode(mode string) Recipe {
	r.JournalMode = mode
	return r
}

// Validate reports an error if r's step count is out of range or its journal
// mode is neither WAL nor DELETE.
func (r Recipe) Validate() error {
	if r.Steps < 0 || r.Steps > MaxSteps {
		return fmt.Errorf("recipe: steps %d out of range 0..%d", r.Steps, MaxSteps)
	}
	switch r.JournalMode {
	case sqlitex.JournalWAL, sqlitex.JournalDelete:
		return nil
	}
	return fmt.Errorf("recipe: journal mode %q not supported (want %q or %q)", r.JournalMode, sqlitex.JournalWAL, sqlitex.JournalDelete)
}

// String formats r as a compact description for logs and evidence output.
func (r Recipe) String() string {
	return fmt.Sprintf("seed=%d steps=%d journal_mode=%s", r.Seed, r.Steps, r.JournalMode)
}

// SQLiteOptions returns the explicit file and connection settings used when
// building or opening a recipe database.
//
// SecureDelete is always enabled so freed space inside B-tree pages and
// freelist leaves is zeroed rather than keeping stale bytes from earlier
// histories. SingleConnection pins the pool to one SQLite connection so step
// execution never interleaves across connections.
func (r Recipe) SQLiteOptions() sqlitex.Options {
	options := sqlitex.DefaultOptions()
	options.File.JournalMode = r.JournalMode
	options.Conn.SecureDelete = true
	options.SingleConnection = true
	return options
}

// Statement is one parameterized SQL statement and its bound arguments.
type Statement struct {
	SQL  string
	Args []any
}

// StepKind classifies a statement inside a Step for summary counts and tests.
type StepKind int

// Statement kinds recorded alongside each generated Statement.
const (
	KindSchema StepKind = iota
	KindInsert
	KindUpdate
	KindDelete
	KindIndex
	KindUserVersion
)

// Step is one numbered transaction in a recipe plan. Number 0 creates the
// schema; numbers 1..N run workload statements.
type Step struct {
	Number     int
	Statements []Statement
	Kinds      []StepKind
}

// StepCounts summarizes how many statements of each kind appear in a Step.
type StepCounts struct {
	Schema  int
	Inserts int
	Updates int
	Deletes int
	Indexes int
}

// Counts tallies the non-metadata statements in s by StepKind.
func (s Step) Counts() StepCounts {
	var counts StepCounts
	for _, kind := range s.Kinds {
		switch kind {
		case KindSchema:
			counts.Schema++
		case KindInsert:
			counts.Inserts++
		case KindUpdate:
			counts.Updates++
		case KindDelete:
			counts.Deletes++
		case KindIndex:
			counts.Indexes++
		}
	}
	return counts
}

// Summary formats s's statement counts as a single human-readable line.
func (s Step) Summary() string {
	counts := s.Counts()
	return fmt.Sprintf("step %d: schema=%d inserts=%d updates=%d deletes=%d indexes=%d", s.Number, counts.Schema, counts.Inserts, counts.Updates, counts.Deletes, counts.Indexes)
}

func (s *Step) add(kind StepKind, sql string, args ...any) {
	s.Statements = append(s.Statements, Statement{SQL: sql, Args: args})
	s.Kinds = append(s.Kinds, kind)
}

// SchemaSQL holds the three CREATE TABLE statements executed in step 0. Every
// table uses an explicit INTEGER PRIMARY KEY so rowids are supplied by Go
// rather than chosen by SQLite.
var SchemaSQL = []string{
	"CREATE TABLE accounts(id INTEGER PRIMARY KEY, name TEXT NOT NULL, balance INTEGER NOT NULL)",
	"CREATE TABLE documents(id INTEGER PRIMARY KEY, account_id INTEGER NOT NULL, title TEXT NOT NULL, body BLOB NOT NULL)",
	"CREATE TABLE events(id INTEGER PRIMARY KEY, account_id INTEGER NOT NULL, kind TEXT NOT NULL, amount INTEGER NOT NULL, note TEXT NOT NULL)",
}

// IndexSQL is the secondary index created during step IndexStep.
const IndexSQL = "CREATE INDEX events_by_account ON events(account_id, id)"

var eventKinds = []string{"deposit", "withdraw", "transfer", "fee"}

// Bounds on generated document body sizes in bytes. On a 4096-byte page a
// table leaf row spills to overflow pages above 4061 bytes of payload, so this
// range exercises both inline rows and multi-page overflow chains.
const (
	MinBodyBytes = 2000
	MaxBodyBytes = 16000
)

type generator struct {
	seed          uint64
	nextAccountID int64
	nextDocID     int64
	nextEventID   int64
	accounts      []int64
	documents     []int64
	events        []int64
}

func newGenerator(seed uint64) *generator {
	return &generator{seed: seed, nextAccountID: 1, nextDocID: 1, nextEventID: 1}
}

// Plan generates steps 0 through r.Steps in order. Each step depends only on
// r.Seed and its step index, never on r.Steps.
func (r Recipe) Plan() ([]Step, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	g := newGenerator(r.Seed)
	steps := make([]Step, 0, r.Steps+1)
	for k := 0; k <= r.Steps; k++ {
		steps = append(steps, g.step(k))
	}
	return steps, nil
}

// Step generates and returns step k alone by walking the live-id state from
// step 0 through step k.
func (r Recipe) Step(k int) (Step, error) {
	if k < 0 || k > r.Steps {
		return Step{}, fmt.Errorf("recipe: step %d out of range 0..%d", k, r.Steps)
	}
	plan, err := r.WithSteps(k).Plan()
	if err != nil {
		return Step{}, err
	}
	return plan[k], nil
}

func userVersionSQL(k int) string {
	return fmt.Sprintf("PRAGMA user_version = %d", k)
}

func (g *generator) step(k int) Step {
	s := Step{Number: k}
	if k == 0 {
		for _, sql := range SchemaSQL {
			s.add(KindSchema, sql)
		}
		s.add(KindUserVersion, userVersionSQL(0))
		return s
	}
	// Seed each step from (seed, k) so every value comes from Go's deterministic
	// PRNG rather than SQL random() or wall-clock functions.
	rng := rand.New(rand.NewPCG(g.seed, uint64(k)))

	for range 4 + rng.IntN(5) {
		id := g.nextAccountID
		g.nextAccountID++
		g.accounts = append(g.accounts, id)
		s.add(KindInsert, "INSERT INTO accounts(id, name, balance) VALUES(?, ?, ?)", id, randomWord(rng, 6, 18), rng.Int64N(100000))
	}
	for range 2 + rng.IntN(4) {
		id := g.nextDocID
		g.nextDocID++
		g.documents = append(g.documents, id)
		s.add(KindInsert, "INSERT INTO documents(id, account_id, title, body) VALUES(?, ?, ?, ?)", id, pick(rng, g.accounts), randomWord(rng, 8, 40), randomBody(rng))
	}
	for range 30 + rng.IntN(31) {
		id := g.nextEventID
		g.nextEventID++
		g.events = append(g.events, id)
		s.add(KindInsert, "INSERT INTO events(id, account_id, kind, amount, note) VALUES(?, ?, ?, ?, ?)", id, pick(rng, g.accounts), eventKinds[rng.IntN(len(eventKinds))], rng.Int64N(20000)-10000, randomWord(rng, 10, 60))
	}

	for range 2 + rng.IntN(4) {
		s.add(KindUpdate, "UPDATE accounts SET balance = ? WHERE id = ?", rng.Int64N(100000), pick(rng, g.accounts))
	}
	if rng.IntN(2) == 0 {
		s.add(KindUpdate, "UPDATE documents SET body = ? WHERE id = ?", randomBody(rng), pick(rng, g.documents))
	}

	// Periodic deletes exercise freeblocks and freelist pages (and overflow
	// chain release when a document is removed).
	if k%3 == 0 {
		for range 4 + rng.IntN(8) {
			var id int64
			g.events, id = removeRandom(rng, g.events)
			s.add(KindDelete, "DELETE FROM events WHERE id = ?", id)
		}
	}
	if k%4 == 0 && len(g.documents) > 2 {
		var id int64
		g.documents, id = removeRandom(rng, g.documents)
		s.add(KindDelete, "DELETE FROM documents WHERE id = ?", id)
	}

	if k == IndexStep {
		s.add(KindIndex, IndexSQL)
	}
	// Record the step number in header bytes 60..63 inside the same transaction
	// so CurrentStep always agrees with the committed rows.
	s.add(KindUserVersion, userVersionSQL(k))
	return s
}

func pick(rng *rand.Rand, ids []int64) int64 {
	return ids[rng.IntN(len(ids))]
}

func removeRandom(rng *rand.Rand, ids []int64) ([]int64, int64) {
	i := rng.IntN(len(ids))
	id := ids[i]
	return append(ids[:i:i], ids[i+1:]...), id
}

const letters = "abcdefghijklmnopqrstuvwxyz"

func randomWord(rng *rand.Rand, minLength, maxLength int) string {
	n := minLength + rng.IntN(maxLength-minLength+1)
	var b strings.Builder
	b.Grow(n)
	for range n {
		b.WriteByte(letters[rng.IntN(len(letters))])
	}
	return b.String()
}

func randomBody(rng *rand.Rand) []byte {
	n := MinBodyBytes + rng.IntN(MaxBodyBytes-MinBodyBytes+1)
	body := make([]byte, n)
	for i := range body {
		body[i] = byte(rng.Uint32())
	}
	return body
}
```

#### `internal/recipe/apply.go`

<!-- include: ../internal/recipe/apply.go -->
```go
package recipe

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"

	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
)

// Execer is the minimal subset of *sql.Tx, *sql.Conn, and *sql.DB needed to
// execute a Step's statements.
type Execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// TxBeginner is the subset of *sql.DB and *sql.Conn needed to wrap a Step in
// one transaction.
type TxBeginner interface {
	BeginTx(ctx context.Context, options *sql.TxOptions) (*sql.Tx, error)
}

// ExecIn runs every statement in s in order on ex without starting or
// committing a transaction. Callers use it when combining steps into a single
// transaction.
func (s Step) ExecIn(ctx context.Context, ex Execer) error {
	for i, statement := range s.Statements {
		if _, err := ex.ExecContext(ctx, statement.SQL, statement.Args...); err != nil {
			return fmt.Errorf("recipe step %d statement %d (%s): %w", s.Number, i, statement.SQL, err)
		}
	}
	return nil
}

// ApplyStep runs s as a single transaction on db, rolling back if any
// statement fails. With sqlitex's default _txlock=immediate, BeginTx acquires
// the write lock up front.
func ApplyStep(ctx context.Context, db TxBeginner, s Step) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("recipe step %d begin: %w", s.Number, err)
	}
	if err := s.ExecIn(ctx, tx); err != nil {
		tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("recipe step %d commit: %w", s.Number, err)
	}
	return nil
}

// ApplyStep generates step k of r and commits it as one transaction on db.
func (r Recipe) ApplyStep(ctx context.Context, db TxBeginner, k int) error {
	s, err := r.Step(k)
	if err != nil {
		return err
	}
	return ApplyStep(ctx, db, s)
}

// Open validates r and opens path with r's explicit SQLite options.
func (r Recipe) Open(ctx context.Context, path string) (*sqlitex.DB, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	return sqlitex.Open(ctx, path, r.SQLiteOptions())
}

// SideFileSuffixes lists the SQLite sidecar files that must be removed with a
// database or absent when the database is at rest.
var SideFileSuffixes = []string{"-wal", "-shm", "-journal"}

// RemoveDatabase deletes path and any SQLite sidecar files (-wal, -shm,
// -journal), ignoring files that do not exist.
func RemoveDatabase(path string) error {
	for _, suffix := range append([]string{""}, SideFileSuffixes...) {
		if err := os.Remove(path + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// CheckAtRest verifies that path exists and has no -wal, -shm, or -journal
// sidecar files alongside it. A closed WAL database leaves no sidecars once
// the last connection checkpoints and removes them on close.
func CheckAtRest(path string) error {
	if _, err := os.Stat(path); err != nil {
		return err
	}
	for _, suffix := range SideFileSuffixes {
		if _, err := os.Stat(path + suffix); err == nil {
			return fmt.Errorf("recipe: %s is not at rest: %s exists", path, path+suffix)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// ApplyInSession opens path, commits s in one transaction, closes the
// connection, and verifies the database is at rest.
//
// Opening a fresh connection per step makes the WAL file change counter
// (header bytes 24..27 and 92..95) advance by one per step: pagerWalFrames
// (src/pager.c:3264) writes dbFileVers+1 (src/pager.c:3135), and dbFileVers is
// refreshed only when page 1 is read from disk (src/pager.c:3109-3110).
func (r Recipe) ApplyInSession(ctx context.Context, path string, s Step) error {
	db, err := r.Open(ctx, path)
	if err != nil {
		return err
	}
	if err := ApplyStep(ctx, db, s); err != nil {
		db.Close()
		return err
	}
	if err := db.Close(); err != nil {
		return err
	}
	return CheckAtRest(path)
}

// Build creates a fresh database at path and applies steps 0..upTo using one
// connection session per step.
func (r Recipe) Build(ctx context.Context, path string, upTo int) error {
	return r.BuildEach(ctx, path, upTo, nil)
}

// BuildEach creates a fresh database at path, applies steps 0..upTo in
// separate sessions, and calls afterStep(k) after each step's file is closed
// and at rest.
func (r Recipe) BuildEach(ctx context.Context, path string, upTo int, afterStep func(k int) error) error {
	if upTo < 0 || upTo > r.Steps {
		return fmt.Errorf("recipe: build up to step %d out of range 0..%d", upTo, r.Steps)
	}
	plan, err := r.WithSteps(upTo).Plan()
	if err != nil {
		return err
	}
	if err := RemoveDatabase(path); err != nil {
		return err
	}
	for _, s := range plan {
		if err := r.ApplyInSession(ctx, path, s); err != nil {
			return err
		}
		if afterStep != nil {
			if err := afterStep(s.Number); err != nil {
				return err
			}
		}
	}
	return nil
}

// CurrentStep reports the last recipe step committed in q, or -1 if none of
// the three recipe tables exist yet. Because step 0 sets user_version = 0,
// checking sqlite_schema distinguishes an empty database from step 0.
func CurrentStep(ctx context.Context, q sqlitex.Querier) (int, error) {
	var tables int
	if err := q.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_schema WHERE type = 'table' AND name IN ('accounts', 'documents', 'events')").Scan(&tables); err != nil {
		return 0, err
	}
	if tables == 0 {
		return -1, nil
	}
	if tables != len(SchemaSQL) {
		return 0, fmt.Errorf("recipe: database has %d of the %d recipe tables", tables, len(SchemaSQL))
	}
	var version int
	if err := q.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return 0, err
	}
	return version, nil
}

// ApplyNext reads CurrentStep from db and commits the next step (current + 1),
// returning the step number just applied.
func (r Recipe) ApplyNext(ctx context.Context, db *sqlitex.DB) (int, error) {
	current, err := CurrentStep(ctx, db)
	if err != nil {
		return 0, err
	}
	next := current + 1
	if next > r.Steps {
		return current, fmt.Errorf("recipe: already at the last step %d", current)
	}
	return next, r.ApplyStep(ctx, db, next)
}
```

#### `internal/recipe/hashes.go`

<!-- include: ../internal/recipe/hashes.go -->
```go
package recipe

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
	"github.com/nickstrad/sqlite-internals/sqlite"
)

// FileSHA256 returns the lowercase hex SHA-256 digest of the file at path.
// Never call it on a file currently held open by a SQLite connection in the
// same process: closing the extra file descriptor drops the process's POSIX
// locks on that file.
func FileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// ChangeCounterRanges lists the half-open byte offsets in page 1's 100-byte
// header updated by pager_write_changecounter (src/pager.c:3130-3143):
// bytes 24..27 (file change counter) and 92..95 (version-valid-for number).
var ChangeCounterRanges = [][2]int{{24, 28}, {92, 96}}

// MaskChangeCounter returns a copy of image with the change-counter header
// bytes zeroed so two WAL databases built with different connection session
// counts can still be compared byte for byte.
func MaskChangeCounter(image []byte) []byte {
	masked := append([]byte(nil), image...)
	for _, span := range ChangeCounterRanges {
		for i := span[0]; i < span[1] && i < len(masked); i++ {
			masked[i] = 0
		}
	}
	return masked
}

// FileHashes holds the SHA-256 digests of a database file both verbatim and
// with ChangeCounterRanges zeroed.
type FileHashes struct {
	Exact  string
	Masked string
}

// HashFile reads path from disk and returns its exact and masked SHA-256
// hashes. The database must already be closed.
func HashFile(path string) (FileHashes, error) {
	image, err := os.ReadFile(path)
	if err != nil {
		return FileHashes{}, err
	}
	return HashImage(image), nil
}

// HashImage returns the exact and change-counter-masked SHA-256 hashes of a
// database file image in memory.
func HashImage(image []byte) FileHashes {
	exact := sha256.Sum256(image)
	masked := sha256.Sum256(MaskChangeCounter(image))
	return FileHashes{Exact: hex.EncodeToString(exact[:]), Masked: hex.EncodeToString(masked[:])}
}

// ComputeStepHashes builds r inside dir from step 0 through r.Steps and
// returns the at-rest FileHashes after every step.
func (r Recipe) ComputeStepHashes(ctx context.Context, dir string) ([]FileHashes, error) {
	path := filepath.Join(dir, "recipe-step-hashes.db")
	defer RemoveDatabase(path)
	var hashes []FileHashes
	err := r.BuildEach(ctx, path, r.Steps, func(k int) error {
		h, err := HashFile(path)
		if err != nil {
			return err
		}
		hashes = append(hashes, h)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return hashes, nil
}

func (r Recipe) cacheKey() string {
	// Include FormatVersion and the pinned Fossil check-in so a generator or
	// SQLite upgrade never reuses stale hashes.
	return fmt.Sprintf("v%d-%s-seed%d-%s", FormatVersion, sqlite.Current().FossilID[:16], r.Seed, r.JournalMode)
}

// CacheDir returns the directory under the course work base where computed
// per-step hash lists are cached across experiment runs.
func CacheDir() (string, error) {
	base, err := sqlitex.WorkBase()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "recipe-cache"), nil
}

// CachePath returns the cache file path for r's seed, mode, format version,
// and pinned SQLite check-in.
func (r Recipe) CachePath() (string, error) {
	dir, err := CacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, r.cacheKey()+".txt"), nil
}

// StepHashes returns the per-step hashes for steps 0..r.Steps, reusing the
// cached file on disk when it already holds at least r.Steps+1 valid entries
// and rebuilding it atomically otherwise.
func (r Recipe) StepHashes(ctx context.Context) ([]FileHashes, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	cachePath, err := r.CachePath()
	if err != nil {
		return nil, err
	}
	if cached, err := readHashCache(cachePath); err == nil && len(cached) > r.Steps {
		return cached[:r.Steps+1], nil
	}
	dir := filepath.Dir(cachePath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	buildDir, err := os.MkdirTemp(dir, "build-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(buildDir)
	hashes, err := r.ComputeStepHashes(ctx, buildDir)
	if err != nil {
		return nil, err
	}
	if err := writeHashCache(cachePath, hashes); err != nil {
		return nil, err
	}
	return hashes, nil
}

// ExpectedHash returns the exact at-rest SHA-256 digest for step k of r.
func (r Recipe) ExpectedHash(ctx context.Context, k int) (string, error) {
	if k < 0 || k > r.Steps {
		return "", fmt.Errorf("recipe: step %d out of range 0..%d", k, r.Steps)
	}
	hashes, err := r.StepHashes(ctx)
	if err != nil {
		return "", err
	}
	return hashes[k].Exact, nil
}

// Match identifies which recipe step a database file equals. Step is -1 when
// no step matches; Exact is true for a verbatim SHA-256 match and false when
// only the change-counter-masked hash matched.
type Match struct {
	Step  int
	Exact bool
}

// Found reports whether m matched a step of the recipe.
func (m Match) Found() bool {
	return m.Step >= 0
}

// String describes m for experiment output.
func (m Match) String() string {
	switch {
	case !m.Found():
		return "no recipe step"
	case m.Exact:
		return fmt.Sprintf("step %d (exact bytes)", m.Step)
	default:
		return fmt.Sprintf("step %d (equal except the change counter at bytes 24-27 and 92-95)", m.Step)
	}
}

// MatchHashes searches hashes for h, preferring an exact byte match before
// falling back to a change-counter-masked match.
func MatchHashes(hashes []FileHashes, h FileHashes) Match {
	for k, candidate := range hashes {
		if candidate.Exact == h.Exact {
			return Match{Step: k, Exact: true}
		}
	}
	for k, candidate := range hashes {
		if candidate.Masked == h.Masked {
			return Match{Step: k}
		}
	}
	return Match{Step: -1}
}

// MatchStep checks that path is at rest, hashes it, and reports which step of
// r it equals.
func (r Recipe) MatchStep(ctx context.Context, path string) (Match, error) {
	if err := CheckAtRest(path); err != nil {
		return Match{Step: -1}, err
	}
	h, err := HashFile(path)
	if err != nil {
		return Match{Step: -1}, err
	}
	hashes, err := r.StepHashes(ctx)
	if err != nil {
		return Match{Step: -1}, err
	}
	return MatchHashes(hashes, h), nil
}

func isSHA256Hex(text string) bool {
	_, err := hex.DecodeString(text)
	return err == nil && len(text) == sha256.Size*2
}

func readHashCache(path string) ([]FileHashes, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var hashes []FileHashes
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 3 {
			return nil, fmt.Errorf("recipe cache %s: malformed line %q", path, scanner.Text())
		}
		k, err := strconv.Atoi(fields[0])
		if err != nil || k != len(hashes) {
			return nil, fmt.Errorf("recipe cache %s: line %q out of order", path, scanner.Text())
		}
		if !isSHA256Hex(fields[1]) || !isSHA256Hex(fields[2]) {
			return nil, fmt.Errorf("recipe cache %s: bad hash on line %q", path, scanner.Text())
		}
		hashes = append(hashes, FileHashes{Exact: fields[1], Masked: fields[2]})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return hashes, nil
}

func writeHashCache(path string, hashes []FileHashes) error {
	var b strings.Builder
	for k, h := range hashes {
		fmt.Fprintf(&b, "%d %s %s\n", k, h.Exact, h.Masked)
	}
	// Write to a sibling temp file and rename so concurrent readers never see
	// a partially written cache file.
	temp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-")
	if err != nil {
		return err
	}
	if _, err := temp.WriteString(b.String()); err != nil {
		temp.Close()
		os.Remove(temp.Name())
		return err
	}
	if err := temp.Close(); err != nil {
		os.Remove(temp.Name())
		return err
	}
	return os.Rename(temp.Name(), path)
}
```

### 5.2 Experiment 1.3 — Recipe rebuild, rung R0 (`1.3-recipe-rebuild`)

- **Claim:** A database built by a fixed, seeded workload with explicit settings is reproducible to the exact byte (equality level **E1**), so the recipe `(seed, step count, journal mode)` is a complete saved state with a unique SHA-256 hash after every step *k*. Breaking the determinism rules breaks byte equality in specific, observable ways (analyzed in Section 6).
- **Capture / Rebuild / Verified at:** Capture is the recipe `(seed=1, steps=30, journal_mode=wal)`. Rebuild runs the recipe into an empty file. Proven equal at **E1** (file SHA-256).
- **Evidence form:** `EXACT` (all lines except `INFO:` lines).
- **Platform:** Portable (runs natively on Linux and macOS without `strace`).

<!-- include: experiments/1.3-recipe-rebuild/main.go -->
```go
// Experiment 1.3 proves Rebuild Ladder rung R0: running the seeded recipe
// twice in different directories under different names produces byte-identical
// database files at every step. It then tests six sources of variation to show
// which determinism rules are required and where SQLite's behavior differs
// between WAL and rollback modes.
//
// Run from the repo root (portable; no strace needed):
//
//	go run ./ch01-architecture-and-execution-path/experiments/1.3-recipe-rebuild
package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/nickstrad/sqlite-internals/internal/recipe"
	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
)

const (
	pageSize     = 4096
	variantBase  = 6
	maxRowid     = "9223372036854775807"
	secondsTries = 3
)

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

type fileDiff struct {
	Equal          bool
	SizeA, SizeB   int
	DifferingPages []int
	Page1Bytes     []int
}

func (d fileDiff) String() string {
	if d.Equal {
		return "identical"
	}
	text := fmt.Sprintf("differ: first differing page %d, %d page(s) differ %v", d.DifferingPages[0], len(d.DifferingPages), limitPages(d.DifferingPages))
	if d.SizeA != d.SizeB {
		text += fmt.Sprintf(", sizes %d and %d pages", d.SizeA/pageSize, d.SizeB/pageSize)
	}
	if len(d.Page1Bytes) > 0 {
		text += fmt.Sprintf(", page 1 bytes %v", d.Page1Bytes)
	}
	return text
}

func limitPages(pages []int) string {
	if len(pages) <= 8 {
		return fmt.Sprint(pages)
	}
	return fmt.Sprintf("%v...", pages[:8])
}

// compareFiles compares two closed database files page by page (1-based) and
// records the differing byte offsets inside page 1 when page 1 differs.
func compareFiles(a, b string) fileDiff {
	dataA, err := os.ReadFile(a)
	if err != nil {
		fatal("read", err)
	}
	dataB, err := os.ReadFile(b)
	if err != nil {
		fatal("read", err)
	}
	d := fileDiff{Equal: bytes.Equal(dataA, dataB), SizeA: len(dataA), SizeB: len(dataB)}
	pages := max(len(dataA), len(dataB)) / pageSize
	for p := range pages {
		start, end := p*pageSize, (p+1)*pageSize
		var pageA, pageB []byte
		if end <= len(dataA) {
			pageA = dataA[start:end]
		}
		if end <= len(dataB) {
			pageB = dataB[start:end]
		}
		if bytes.Equal(pageA, pageB) {
			continue
		}
		d.DifferingPages = append(d.DifferingPages, p+1)
		if p == 0 && pageA != nil && pageB != nil {
			for i := range pageSize {
				if pageA[i] != pageB[i] {
					d.Page1Bytes = append(d.Page1Bytes, i)
				}
			}
		}
	}
	return d
}

// onlyChangeCounter reports whether two equal-sized files differ exclusively
// inside header bytes 24..27 (file change counter) and 92..95
// (version-valid-for), written by pager_write_changecounter (src/pager.c:3130-3143).
func onlyChangeCounter(d fileDiff) bool {
	if d.Equal || d.SizeA != d.SizeB || !slices.Equal(d.DifferingPages, []int{1}) {
		return false
	}
	for _, offset := range d.Page1Bytes {
		inCounter := false
		for _, span := range recipe.ChangeCounterRanges {
			if offset >= span[0] && offset < span[1] {
				inCounter = true
			}
		}
		if !inCounter {
			return false
		}
	}
	return true
}

func hashOf(path string) recipe.FileHashes {
	h, err := recipe.HashFile(path)
	if err != nil {
		fatal("hash", err)
	}
	return h
}

func pages(path string) int {
	info, err := os.Stat(path)
	if err != nil {
		fatal("stat", err)
	}
	return int(info.Size() / pageSize)
}

func rebuild(ctx context.Context, r recipe.Recipe, path string) []recipe.FileHashes {
	var hashes []recipe.FileHashes
	err := r.BuildEach(ctx, path, r.Steps, func(k int) error {
		hashes = append(hashes, hashOf(path))
		return nil
	})
	if err != nil {
		fatal("build "+path, err)
	}
	return hashes
}

func applyStatementsInSession(ctx context.Context, r recipe.Recipe, path string, pragmas []string, statements ...string) {
	db, err := r.Open(ctx, path)
	if err != nil {
		fatal("open", err)
	}
	// Pin a single *sql.Conn so any PRAGMAs apply to the exact connection that
	// runs the transaction.
	conn, err := db.Conn(ctx)
	if err != nil {
		fatal("conn", err)
	}
	for _, pragma := range pragmas {
		if _, err := conn.ExecContext(ctx, pragma); err != nil {
			fatal(pragma, err)
		}
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		fatal("begin", err)
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			fatal(statement, err)
		}
	}
	if err := tx.Commit(); err != nil {
		fatal("commit", err)
	}
	conn.Close()
	if err := db.Close(); err != nil {
		fatal("close", err)
	}
	if err := recipe.CheckAtRest(path); err != nil {
		fatal("at rest", err)
	}
}

func buildBase(ctx context.Context, r recipe.Recipe, path string) {
	if err := r.Build(ctx, path, variantBase); err != nil {
		fatal("base", err)
	}
}

type variant struct {
	name      string
	rule      string
	pragmas   []string
	statement []string
}

func runTwice(ctx context.Context, r recipe.Recipe, dir string, v variant) fileDiff {
	a := filepath.Join(dir, v.name+"-run1.db")
	b := filepath.Join(dir, v.name+"-run2.db")
	for _, path := range []string{a, b} {
		buildBase(ctx, r, path)
		applyStatementsInSession(ctx, r, path, v.pragmas, v.statement...)
	}
	d := compareFiles(a, b)
	fmt.Printf("INFO: %s sha256 run 1 %s, run 2 %s\n", v.name, hashOf(a).Exact, hashOf(b).Exact)
	return d
}

// timestampVariant demonstrates both sides of current_timestamp's one-second
// resolution: two runs inside the same wall-clock second produce identical
// files, while a third run after the second advances differs.
func timestampVariant(ctx context.Context, r recipe.Recipe, dir string) (sameSecond, nextSecond fileDiff) {
	statement := "INSERT INTO events(id, account_id, kind, amount, note) VALUES(900002, 1, 'fee', 0, current_timestamp)"
	a := filepath.Join(dir, "timestamp-run1.db")
	b := filepath.Join(dir, "timestamp-run2.db")
	c := filepath.Join(dir, "timestamp-run3.db")
	for attempt := range secondsTries {
		for _, path := range []string{a, b, c} {
			buildBase(ctx, r, path)
		}
		waitForNextSecond()
		started := time.Now().Unix()
		applyStatementsInSession(ctx, r, a, nil, statement)
		applyStatementsInSession(ctx, r, b, nil, statement)
		if time.Now().Unix() != started {
			fmt.Printf("INFO: timestamp attempt %d crossed a second boundary; retrying\n", attempt+1)
			continue
		}
		waitForNextSecond()
		applyStatementsInSession(ctx, r, c, nil, statement)
		fmt.Printf("INFO: timestamp sha256 run 1 %s, run 2 %s, run 3 %s\n", hashOf(a).Exact, hashOf(b).Exact, hashOf(c).Exact)
		return compareFiles(a, b), compareFiles(a, c)
	}
	fatal("timestamp variant", fmt.Errorf("could not run two inserts inside one wall-clock second in %d attempts", secondsTries))
	return fileDiff{}, fileDiff{}
}

func waitForNextSecond() {
	now := time.Now()
	time.Sleep(now.Truncate(time.Second).Add(time.Second).Sub(now) + 5*time.Millisecond)
}

// mergedWithNext selects steps 2+3 and 4+5 to merge into single transactions:
// step 3 deletes rows written in step 2, exposing stale unzeroed bytes when
// secure_delete=OFF.
var mergedWithNext = []int{2, 4}

func batchVariant(ctx context.Context, r recipe.Recipe, dir string, secureDelete bool) (fileDiff, unusedReport) {
	plan, err := r.WithSteps(variantBase).Plan()
	if err != nil {
		fatal("plan", err)
	}
	options := r.SQLiteOptions()
	options.Conn.SecureDelete = secureDelete
	label := fmt.Sprintf("%s-secure-delete-%v", r.JournalMode, secureDelete)
	a := filepath.Join(dir, "batches-"+label+"-one-per-step.db")
	b := filepath.Join(dir, "batches-"+label+"-merged.db")
	for _, path := range []string{a, b} {
		if err := recipe.RemoveDatabase(path); err != nil {
			fatal("remove", err)
		}
		db, err := sqlitex.Open(ctx, path, options)
		if err != nil {
			fatal("open", err)
		}
		merge := path == b
		for i := 0; i < len(plan); i++ {
			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				fatal("begin", err)
			}
			if err := plan[i].ExecIn(ctx, tx); err != nil {
				fatal("step", err)
			}
			if merge && slices.Contains(mergedWithNext, i) {
				i++
				if err := plan[i].ExecIn(ctx, tx); err != nil {
					fatal("step", err)
				}
			}
			if err := tx.Commit(); err != nil {
				fatal("commit", err)
			}
		}
		if err := db.Close(); err != nil {
			fatal("close", err)
		}
	}
	fmt.Printf("INFO: batches %s sha256 one-per-step %s, merged %s\n", label, hashOf(a).Exact, hashOf(b).Exact)
	d := compareFiles(a, b)
	return d, classifyDifferences(a, b, d)
}

func be16(data []byte, offset int) int {
	return int(data[offset])<<8 | int(data[offset+1])
}

func be32(data []byte, offset int) int {
	return int(data[offset])<<24 | int(data[offset+1])<<16 | int(data[offset+2])<<8 | int(data[offset+3])
}

// freelistPages walks SQLite's freelist trunk chain starting from header
// offset 32 (4-byte big-endian first trunk page number). Each trunk holds the
// next trunk pointer at offset 0, the leaf count at offset 4, and 4-byte leaf
// page numbers starting at offset 8.
func freelistPages(data []byte) []int {
	var free []int
	for trunk := be32(data, 32); trunk != 0 && len(free) <= len(data)/pageSize; trunk = be32(data, (trunk-1)*pageSize) {
		free = append(free, trunk)
		base := (trunk - 1) * pageSize
		for i := range be32(data, base+4) {
			free = append(free, be32(data, base+8+4*i))
		}
	}
	return free
}

// unusedBytes marks every byte offset on page that belongs to no live cell:
// all bytes of a freelist page, or on a B-tree page the unallocated gap
// between the cell-pointer array and the cell content area plus the bodies of
// all freeblocks.
func unusedBytes(data []byte, page int, free []int) []bool {
	unused := make([]bool, pageSize)
	if slices.Contains(free, page) {
		for i := range unused {
			unused[i] = true
		}
		return unused
	}
	content := data[(page-1)*pageSize : page*pageSize]
	header := 0
	if page == 1 {
		header = 100
	}
	headerSize := 8
	if content[header] == 0x02 || content[header] == 0x05 {
		headerSize = 12
	}
	cells := be16(content, header+3)
	contentStart := be16(content, header+5)
	if contentStart == 0 {
		contentStart = 65536
	}
	for i := header + headerSize + 2*cells; i < contentStart && i < pageSize; i++ {
		unused[i] = true
	}
	for block := be16(content, header+1); block != 0 && block+4 <= pageSize; block = be16(content, block) {
		size := be16(content, block+2)
		for i := block + 4; i < block+size && i < pageSize; i++ {
			unused[i] = true
		}
	}
	return unused
}

type unusedReport struct {
	OnlyUnused bool
	Kinds      []string
}

func classifyDifferences(a, b string, d fileDiff) unusedReport {
	dataA, err := os.ReadFile(a)
	if err != nil {
		fatal("read", err)
	}
	dataB, err := os.ReadFile(b)
	if err != nil {
		fatal("read", err)
	}
	report := unusedReport{OnlyUnused: len(dataA) == len(dataB)}
	if !report.OnlyUnused {
		return report
	}
	freeA, freeB := freelistPages(dataA), freelistPages(dataB)
	for _, page := range d.DifferingPages {
		unusedA := unusedBytes(dataA, page, freeA)
		unusedB := unusedBytes(dataB, page, freeB)
		base := (page - 1) * pageSize
		count := 0
		for i := range pageSize {
			if dataA[base+i] != dataB[base+i] {
				count++
				if !unusedA[i] || !unusedB[i] {
					report.OnlyUnused = false
				}
			}
		}
		kind := "b-tree page, unallocated space and freeblocks"
		if slices.Contains(freeA, page) || slices.Contains(freeB, page) {
			kind = "freelist page"
		}
		report.Kinds = append(report.Kinds, fmt.Sprintf("page %d: %d bytes (%s)", page, count, kind))
	}
	return report
}

// sessionVariant compares building steps 0..variantBase with one connection
// session per step against building the exact same steps on one long-lived
// connection. Under WAL mode only the header change counter differs because
// dbFileVers is refreshed on page-1 read, not on WAL commit (src/pager.c:3109-3110).
func sessionVariant(ctx context.Context, r recipe.Recipe, dir string) fileDiff {
	canonical := filepath.Join(dir, "sessions-"+r.JournalMode+"-one-per-step.db")
	if err := r.Build(ctx, canonical, variantBase); err != nil {
		fatal("build", err)
	}
	longLived := filepath.Join(dir, "sessions-"+r.JournalMode+"-one-for-all.db")
	if err := recipe.RemoveDatabase(longLived); err != nil {
		fatal("remove", err)
	}
	db, err := r.Open(ctx, longLived)
	if err != nil {
		fatal("open", err)
	}
	for k := 0; k <= variantBase; k++ {
		if err := r.ApplyStep(ctx, db, k); err != nil {
			fatal("step", err)
		}
	}
	if err := db.Close(); err != nil {
		fatal("close", err)
	}
	return compareFiles(canonical, longLived)
}

func main() {
	ctx := context.Background()
	sqlitex.MustPinCheckMemory(ctx)
	work := sqlitex.MustWorkDir("1.3")
	dir, err := filepath.EvalSymlinks(work)
	if err != nil {
		fatal("work dir", err)
	}
	fmt.Printf("INFO: work dir %s\n", displayPath(dir))

	r := recipe.Default()
	fmt.Printf("recipe: %s (index created at step %d, settings: %s; %s)\n", r, recipe.IndexStep, r.SQLiteOptions().File, r.SQLiteOptions().Conn)
	plan, err := r.Plan()
	if err != nil {
		fatal("plan", err)
	}
	for _, s := range plan {
		fmt.Printf("  %s\n", s.Summary())
	}

	fmt.Println("\n== R0: rebuild the recipe twice ==")
	if err := os.MkdirAll(filepath.Join(dir, "run-1"), 0o755); err != nil {
		fatal("mkdir", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "run-2", "elsewhere"), 0o755); err != nil {
		fatal("mkdir", err)
	}
	first := filepath.Join(dir, "run-1", "recipe.db")
	second := filepath.Join(dir, "run-2", "elsewhere", "another-name.db")
	started := time.Now()
	firstHashes := rebuild(ctx, r, first)
	fmt.Printf("INFO: one build of %d steps took %s\n", r.Steps, time.Since(started).Round(time.Millisecond))
	secondHashes := rebuild(ctx, r, second)
	fmt.Printf("run 1 (run-1/recipe.db): %d pages, sha256 %s\n", pages(first), hashOf(first).Exact)
	fmt.Printf("run 2 (run-2/elsewhere/another-name.db): %d pages, sha256 %s\n", pages(second), hashOf(second).Exact)
	check(hashOf(first) == hashOf(second), "two builds of the recipe in different directories under different names are byte-identical")
	fmt.Println("per-step state hashes (sha256 of the closed file after step k):")
	for k, h := range firstHashes {
		fmt.Printf("  step %2d %s\n", k, h.Exact)
	}
	check(slices.Equal(firstHashes, secondHashes), fmt.Sprintf("the per-step hash lists of the two runs are identical (%d states)", len(firstHashes)))
	distinct := map[string]bool{}
	for _, h := range firstHashes {
		distinct[h.Exact] = true
	}
	check(len(distinct) == len(firstHashes), "every step leaves a different file, so a hash names exactly one step")
	cached, err := r.StepHashes(ctx)
	if err != nil {
		fatal("step hashes", err)
	}
	check(slices.Equal(cached, firstHashes), "recipe.StepHashes (the cached list later chapters use) equals the list just built")
	middle := filepath.Join(dir, "run-1", "to-step-12.db")
	if err := r.Build(ctx, middle, 12); err != nil {
		fatal("build", err)
	}
	match, err := r.MatchStep(ctx, middle)
	if err != nil {
		fatal("match", err)
	}
	fmt.Printf("lookup: a database built to step 12 is %s\n", match)
	check(match == recipe.Match{Step: 12, Exact: true}, "recipe.MatchStep names the step a database equals")

	deleteRecipe := r.WithJournalMode(sqlitex.JournalDelete)
	deleteA := filepath.Join(dir, "run-1", "recipe-delete.db")
	deleteB := filepath.Join(dir, "run-2", "recipe-delete.db")
	deleteHashesA := rebuild(ctx, deleteRecipe, deleteA)
	deleteHashesB := rebuild(ctx, deleteRecipe, deleteB)
	fmt.Printf("same recipe in rollback mode (journal_mode=delete): sha256 %s\n", hashOf(deleteA).Exact)
	check(slices.Equal(deleteHashesA, deleteHashesB), "the rollback-mode recipe also rebuilds byte for byte at every step")
	modes := compareFiles(first, deleteA)
	fmt.Printf("WAL build vs rollback build of the same recipe: %s\n", modes)

	fmt.Printf("\n== broken variants: recipe to step %d, then one extra transaction, each run twice into fresh files ==\n", variantBase)
	randomDiff := runTwice(ctx, r, dir, variant{
		name:      "random",
		rule:      "no SQL random()",
		statement: []string{"INSERT INTO events(id, account_id, kind, amount, note) VALUES(900001, 1, 'fee', random(), 'random')"},
	})
	fmt.Printf("1. SQL random() in a value: %s\n", randomDiff)
	check(!randomDiff.Equal, "SQL random() makes two runs differ")

	sameSecond, nextSecond := timestampVariant(ctx, r, dir)
	fmt.Printf("2. current_timestamp in a value, two runs inside one wall-clock second: %s\n", sameSecond)
	fmt.Printf("   current_timestamp in a value, third run one second later: %s\n", nextSecond)
	check(sameSecond.Equal && !nextSecond.Equal, "current_timestamp makes runs differ once the wall clock's second changes (its resolution is one second, so two fast runs can still match)")

	unordered := "INSERT INTO archive(account_id, amount) SELECT account_id, amount FROM events WHERE amount > 0"
	ordered := unordered + " ORDER BY id"
	createArchive := "CREATE TABLE archive(id INTEGER PRIMARY KEY, account_id INTEGER NOT NULL, amount INTEGER NOT NULL)"
	unorderedSame := runTwice(ctx, r, dir, variant{name: "unordered", statement: []string{createArchive, unordered}})
	fmt.Printf("3. INSERT ... SELECT without ORDER BY (rowids from scan order), same plan twice: %s\n", unorderedSame)
	unorderedPlan := runTwiceWithPlanChange(ctx, r, dir, "unordered-plan", createArchive, unordered)
	fmt.Printf("   the same statement, run 2 with a different scan order (PRAGMA reverse_unordered_selects=ON): %s\n", unorderedPlan)
	orderedPlan := runTwiceWithPlanChange(ctx, r, dir, "ordered-plan", createArchive, ordered)
	fmt.Printf("   with ORDER BY id, run 2 with reverse_unordered_selects=ON: %s\n", orderedPlan)
	check(unorderedSame.Equal, "FINDING: an unordered INSERT ... SELECT run twice on the same build with the same plan gives identical files")
	check(!unorderedPlan.Equal && orderedPlan.Equal, "an unordered INSERT ... SELECT changes the file when the scan order changes; ORDER BY removes the dependence")

	batchesWAL, _ := batchVariant(ctx, r, dir, true)
	fmt.Printf("4. steps 0-%d on one connection, steps 2+3 and 4+5 each committed as one transaction instead of two:\n", variantBase)
	fmt.Printf("   WAL mode, recipe settings (secure_delete=ON): %s\n", batchesWAL)
	batchesWALOff, unusedWALOff := batchVariant(ctx, r, dir, false)
	fmt.Printf("   WAL mode, secure_delete=OFF: %s\n", batchesWALOff)
	for _, kind := range unusedWALOff.Kinds {
		fmt.Printf("     %s\n", kind)
	}
	batchesDelete, _ := batchVariant(ctx, deleteRecipe, dir, true)
	fmt.Printf("   rollback mode, secure_delete=ON: %s\n", batchesDelete)
	check(batchesWAL.Equal, "FINDING: in WAL mode on one connection with secure_delete=ON, different transaction batches give identical files")
	check(!batchesWALOff.Equal && unusedWALOff.OnlyUnused, "with secure_delete=OFF different batches differ, and only in bytes no cell uses (stale content left in unallocated space, freeblocks or freelist pages)")
	check(onlyChangeCounter(batchesDelete), "in rollback mode different batches differ, and with secure_delete=ON only in the change counter (it counts commits)")

	rowidDiff := runTwice(ctx, r, dir, variant{
		name: "rowid",
		statement: []string{
			"CREATE TABLE tickets(id INTEGER PRIMARY KEY, label TEXT NOT NULL)",
			"INSERT INTO tickets(id, label) VALUES(" + maxRowid + ", 'max')",
			"INSERT INTO tickets(label) VALUES('next')",
		},
	})
	fmt.Printf("5. an insert without an id after rowid %s (SQLite picks a random unused rowid): %s\n", maxRowid, rowidDiff)
	check(!rowidDiff.Equal, "rowid assignment past the maximum rowid makes two runs differ")

	fmt.Println("\n== a source of variation the outline did not list: connection sessions ==")
	walSessions := sessionVariant(ctx, r, dir)
	deleteSessions := sessionVariant(ctx, deleteRecipe, dir)
	fmt.Printf("6. WAL mode, steps 0-%d one connection per step vs one connection for all steps: %s\n", variantBase, walSessions)
	fmt.Printf("   rollback mode, the same comparison: %s\n", deleteSessions)
	check(onlyChangeCounter(walSessions), "FINDING: in WAL mode the number of connection sessions changes the file, and only the change counter (bytes 24-27 and 92-95)")
	check(deleteSessions.Equal, "in rollback mode the number of connection sessions does not change the file")

	fmt.Println()
	if len(failures) > 0 {
		fmt.Printf("RECIPE REBUILD FAILED: %d claim(s) do not hold: %s\n", len(failures), strings.Join(failures, "; "))
		os.Exit(1)
	}
	fmt.Println("RECIPE REBUILD OK")
}

func runTwiceWithPlanChange(ctx context.Context, r recipe.Recipe, dir, name string, statements ...string) fileDiff {
	a := filepath.Join(dir, name+"-run1.db")
	b := filepath.Join(dir, name+"-run2.db")
	buildBase(ctx, r, a)
	applyStatementsInSession(ctx, r, a, nil, statements...)
	buildBase(ctx, r, b)
	applyStatementsInSession(ctx, r, b, []string{"PRAGMA reverse_unordered_selects=ON"}, statements...)
	return compareFiles(a, b)
}

func displayPath(path string) string {
	root, err := sqlitex.RepoRoot()
	if err != nil {
		return path
	}
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	relative, err := filepath.Rel(root, path)
	if err != nil || strings.HasPrefix(relative, "..") {
		return path
	}
	return filepath.ToSlash(relative)
}
```

Run from the repository root:

```text
go run ./ch01-architecture-and-execution-path/experiments/1.3-recipe-rebuild
```

Captured evidence:

<!-- include: evidence/1.3-recipe-rebuild.txt -->
```text
sqlite_source_id: 2026-07-24 19:02:57 bf7c7f30031888f4e796e429ab3978879485813aaca6f641c7b33e4e09459bcc (pin 3.53.4 ok)
INFO: work dir _work/1.3
recipe: seed=1 steps=30 journal_mode=wal (index created at step 5, settings: page_size=4096 auto_vacuum=NONE encoding=UTF-8 application_id=1397836120 user_version=0 journal_mode=wal; synchronous=FULL foreign_keys=ON secure_delete=ON cache_size=-2000 wal_autocheckpoint=1000 busy_timeout=5000ms)
  step 0: schema=3 inserts=0 updates=0 deletes=0 indexes=0
  step 1: schema=0 inserts=65 updates=6 deletes=0 indexes=0
  step 2: schema=0 inserts=65 updates=3 deletes=0 indexes=0
  step 3: schema=0 inserts=53 updates=5 deletes=5 indexes=0
  step 4: schema=0 inserts=48 updates=6 deletes=1 indexes=0
  step 5: schema=0 inserts=39 updates=5 deletes=0 indexes=1
  step 6: schema=0 inserts=52 updates=3 deletes=10 indexes=0
  step 7: schema=0 inserts=46 updates=4 deletes=0 indexes=0
  step 8: schema=0 inserts=60 updates=2 deletes=1 indexes=0
  step 9: schema=0 inserts=53 updates=4 deletes=5 indexes=0
  step 10: schema=0 inserts=57 updates=5 deletes=0 indexes=0
  step 11: schema=0 inserts=50 updates=4 deletes=0 indexes=0
  step 12: schema=0 inserts=42 updates=5 deletes=11 indexes=0
  step 13: schema=0 inserts=50 updates=5 deletes=0 indexes=0
  step 14: schema=0 inserts=58 updates=4 deletes=0 indexes=0
  step 15: schema=0 inserts=49 updates=3 deletes=9 indexes=0
  step 16: schema=0 inserts=48 updates=5 deletes=1 indexes=0
  step 17: schema=0 inserts=67 updates=5 deletes=0 indexes=0
  step 18: schema=0 inserts=44 updates=6 deletes=7 indexes=0
  step 19: schema=0 inserts=54 updates=3 deletes=0 indexes=0
  step 20: schema=0 inserts=40 updates=3 deletes=1 indexes=0
  step 21: schema=0 inserts=45 updates=6 deletes=5 indexes=0
  step 22: schema=0 inserts=48 updates=5 deletes=0 indexes=0
  step 23: schema=0 inserts=50 updates=3 deletes=0 indexes=0
  step 24: schema=0 inserts=39 updates=4 deletes=7 indexes=0
  step 25: schema=0 inserts=49 updates=5 deletes=0 indexes=0
  step 26: schema=0 inserts=56 updates=3 deletes=0 indexes=0
  step 27: schema=0 inserts=39 updates=4 deletes=9 indexes=0
  step 28: schema=0 inserts=61 updates=5 deletes=1 indexes=0
  step 29: schema=0 inserts=44 updates=4 deletes=0 indexes=0
  step 30: schema=0 inserts=67 updates=4 deletes=9 indexes=0

== R0: rebuild the recipe twice ==
INFO: one build of 30 steps took 896ms
run 1 (run-1/recipe.db): 270 pages, sha256 365695c2ef73fee65082304bd6a6389161e329c276b0ea0e7001d9850cb4fc89
run 2 (run-2/elsewhere/another-name.db): 270 pages, sha256 365695c2ef73fee65082304bd6a6389161e329c276b0ea0e7001d9850cb4fc89
check ok: two builds of the recipe in different directories under different names are byte-identical
per-step state hashes (sha256 of the closed file after step k):
  step  0 bc7ece7b7dd702aa4c6bd7f8b6bd8fece69aaa792b098db34c36616f071a70e0
  step  1 738802a39252d85c8c8c85aa7d61d8cc2a9fc641f3b31bbd8befe4a5dcc97133
  step  2 8a35d3f789539eb161a525843af00301ff54eaece89be86f99e14b1610cc44a5
  step  3 0261c8e50c8b065876d6c97e8dba350e01c8268b1ed55bed76991667fbf6177c
  step  4 8d1353504ba05101e8f40d0e4a157d9fa1dbf7746cdc02e423659f78ff557e2a
  step  5 9a783bf363d34706d254d16aa3ab6526cde728e50693601ad01551c191cca168
  step  6 f8d0c5fd4aa4039f50ad5c4e9bac2e579a49069c3dfccb32fefe7da374252507
  step  7 729d61d9603306fc8e0108f342b07c8da014f85cf0f2e20d5f78d97e97f8f07f
  step  8 01a3d0b8d408ba9a5e9a5a42e3196ae816442ec83b53dd9e704d6c560b2e23ba
  step  9 1bcfad324da7c5f65f6585da2efb46a8bfd80527428478bcb321f646060c8f06
  step 10 2045356ca72a44ed84861aac5205e344daf695db1092cc2ce18c4afd95ca736c
  step 11 119b08501ea254ff6f19a5eff745e00d31662192543b57773a08784f9c86a47a
  step 12 6a09c5ce77b7fbdd5bd305d2b577bbdcec31cdde2d9b12c23edb47242dc62e76
  step 13 1442c94dc8aa874a5fbe924c81db51cfc957097948d7f2654d55ee49ec8537b1
  step 14 2b1045503ce63b18994ee31adb118d1f44872b31b33fcdf8ab804822506de3e3
  step 15 1a618335ef3e7f3b60c5bb738ee95ffdaebfaba8cb733538764e2a1b8cf7cb4d
  step 16 2bf0908354980fc0794c31be6de39234e26433123b7812bd859a463e959e0379
  step 17 4b6e4713a4093c08fac2efcbc848b8939b3d26e6e1425e76fe917c45952b52fc
  step 18 a5070333e85a4651119d9f5c010c6aa0fd4a552e7bcbd780a782a3d797b26440
  step 19 53a2dd7ea67b3bb5fc6d596c7f5d6d539405da617a6803406fb494470a1d5e24
  step 20 a69b18de710737c329d8dc0e250234bd5da4e89e180bef5c2fb8539b532e157a
  step 21 b2bd20e0f5701abc16ea89c0dfde8017989cff70acf2e727e1fefe65acdb4560
  step 22 fd03b1029f252a4f9d385298d8c276b4180d92bfc8ce5487c65fcefe26f84c76
  step 23 aa0838c676799453d29945ee5dc25803f3273332439892b714f7f3d26d0190dd
  step 24 a84e66e865ca93951624e5fc1da75e8c9611b147c2d3fcb4825388e1dc6320ec
  step 25 de90f5e9d7dbde1d14a3c5c7646325e8c1b0c30399cbda8d340cd4e0b9013ed8
  step 26 d448dbc804b6b2ef9f38523050fbecc5938c8228364bf8ee0bf48dea44bb2f6d
  step 27 58bc2a0b98f48f9b05afc74238c6b1489f9395c38fd5da142360beb035287a75
  step 28 6c15e4d39f7014155d63ac3d3b8cdbf3718007417df51c8c91a2d3dbe9669fcc
  step 29 fc8d112861eeccab0621a85970b1c7fd8697fac775545eff6b124f67ff0e81bd
  step 30 365695c2ef73fee65082304bd6a6389161e329c276b0ea0e7001d9850cb4fc89
check ok: the per-step hash lists of the two runs are identical (31 states)
check ok: every step leaves a different file, so a hash names exactly one step
check ok: recipe.StepHashes (the cached list later chapters use) equals the list just built
lookup: a database built to step 12 is step 12 (exact bytes)
check ok: recipe.MatchStep names the step a database equals
same recipe in rollback mode (journal_mode=delete): sha256 0b5b4111fb9946325ad7ffa859b68b1394b04f824a274b0d770697bd16b71d57
check ok: the rollback-mode recipe also rebuilds byte for byte at every step
WAL build vs rollback build of the same recipe: differ: first differing page 1, 1 page(s) differ [1], page 1 bytes [18 19 27 95]

== broken variants: recipe to step 6, then one extra transaction, each run twice into fresh files ==
INFO: random sha256 run 1 52403224e037bb2ee8c4cb4847aebff0862e823334368bcd1dc4ab0bffd1cefb, run 2 5258627236df3bcf017dca0bc7c433f072e79e2529ecd29606d3768f9d447bd3
1. SQL random() in a value: differ: first differing page 59, 1 page(s) differ [59]
check ok: SQL random() makes two runs differ
INFO: timestamp sha256 run 1 16dc578cec8994c6d57a0372e66fd8b26b9509363b19ccd4452c26184065fedf, run 2 16dc578cec8994c6d57a0372e66fd8b26b9509363b19ccd4452c26184065fedf, run 3 39a811f446fad2953f8a44be0976cd91f87717d66d37fc3f4e5d3e36b11c2a0b
2. current_timestamp in a value, two runs inside one wall-clock second: identical
   current_timestamp in a value, third run one second later: differ: first differing page 59, 1 page(s) differ [59]
check ok: current_timestamp makes runs differ once the wall clock's second changes (its resolution is one second, so two fast runs can still match)
INFO: unordered sha256 run 1 0b1cc51179d692e35f77084f33e6392f68af459677f03f61b7aaeda2e0d96273, run 2 0b1cc51179d692e35f77084f33e6392f68af459677f03f61b7aaeda2e0d96273
3. INSERT ... SELECT without ORDER BY (rowids from scan order), same plan twice: identical
   the same statement, run 2 with a different scan order (PRAGMA reverse_unordered_selects=ON): differ: first differing page 29, 1 page(s) differ [29]
   with ORDER BY id, run 2 with reverse_unordered_selects=ON: identical
check ok: FINDING: an unordered INSERT ... SELECT run twice on the same build with the same plan gives identical files
check ok: an unordered INSERT ... SELECT changes the file when the scan order changes; ORDER BY removes the dependence
INFO: batches wal-secure-delete-true sha256 one-per-step 50b1b004498b38967ea38d65657cb7fd39a206735ee24ed80189d73280e2d64e, merged 50b1b004498b38967ea38d65657cb7fd39a206735ee24ed80189d73280e2d64e
4. steps 0-6 on one connection, steps 2+3 and 4+5 each committed as one transaction instead of two:
   WAL mode, recipe settings (secure_delete=ON): identical
INFO: batches wal-secure-delete-false sha256 one-per-step d54533c2ebd389e9ae53009cf3fa8a0f60fbabf3081379d4dd1adf5875b8ef4b, merged e358971c4037b61c6bc8813ca8cd14334670892b821e34776f12b9230e6b279c
   WAL mode, secure_delete=OFF: differ: first differing page 24, 2 page(s) differ [24 28]
     page 24: 357 bytes (b-tree page, unallocated space and freeblocks)
     page 28: 1207 bytes (b-tree page, unallocated space and freeblocks)
INFO: batches delete-secure-delete-true sha256 one-per-step 575d9182d7fb04693ee2c9b68711b568851d3ebd3617bcd50cb443c8582b0aa8, merged f5f792baf06711182a7cc105ef079f2b1328b0c968bec0cb5df9b475674e0d95
   rollback mode, secure_delete=ON: differ: first differing page 1, 1 page(s) differ [1], page 1 bytes [27 95]
check ok: FINDING: in WAL mode on one connection with secure_delete=ON, different transaction batches give identical files
check ok: with secure_delete=OFF different batches differ, and only in bytes no cell uses (stale content left in unallocated space, freeblocks or freelist pages)
check ok: in rollback mode different batches differ, and with secure_delete=ON only in the change counter (it counts commits)
INFO: rowid sha256 run 1 0cbbd58632d00c62d8f4dbcb58c243b130f69be8a667af9d333fb0891aea0d58, run 2 a17ee7bfdd0e18e254e08bfe7b6dd400a076f0cbfecba51be3c26395591f8b8b
5. an insert without an id after rowid 9223372036854775807 (SQLite picks a random unused rowid): differ: first differing page 29, 1 page(s) differ [29]
check ok: rowid assignment past the maximum rowid makes two runs differ

== a source of variation the outline did not list: connection sessions ==
6. WAL mode, steps 0-6 one connection per step vs one connection for all steps: differ: first differing page 1, 1 page(s) differ [1], page 1 bytes [27 95]
   rollback mode, the same comparison: identical
check ok: FINDING: in WAL mode the number of connection sessions changes the file, and only the change counter (bytes 24-27 and 92-95)
check ok: in rollback mode the number of connection sessions does not change the file

RECIPE REBUILD OK
```

#### Reading the R0 evidence (lines 1–80)

- **Lines 3–34 (recipe plan).** Prints the recipe's persistent file settings and per-connection settings, followed by the 31 step summaries (step 0 with 3 `CREATE TABLE` statements; steps 1–30 with inserts, updates, periodic deletes, and the single `CREATE INDEX` at step 5).
- **Lines 36–40 (two independent builds).** Building the 30-step WAL recipe into `run-1/recipe.db` and `run-2/elsewhere/another-name.db` takes under a second (`896ms` on Linux here; `INFO:` line) and yields 270 pages (1,105,920 bytes) with identical SHA-256 `365695c2ef73fee65082304bd6a6389161e329c276b0ea0e7001d9850cb4fc89`. Directory path and file name do not enter the database bytes.
- **Lines 41–77 (per-step hashes and lookup).** Every one of the 31 step states (steps 0–30) matches identically between run 1 and run 2, all 31 hashes are distinct, and `recipe.StepHashes` matches the fresh build. Building a separate file to step 12 and passing it to `recipe.MatchStep` returns `step 12 (exact bytes)`.
- **Lines 78–80 (WAL vs rollback).** The same recipe in rollback mode (`journal_mode=delete`) also rebuilds byte for byte at every step (`0b5b4111fb9946325ad7ffa859b68b1394b04f824a274b0d770697bd16b71d57`). Comparing the closed WAL file against the closed rollback file shows only 4 differing bytes in the entire 1.1 MiB database, all on page 1: bytes `18` and `19` (file format write/read versions: `2` for WAL vs `1` for rollback) and bytes `27` and `95` (the low byte of the 4-byte file change counter and version-valid-for field).

---

## 6. Evidence and failure cases

The second half of Experiment 1.3 (lines 82–118 of `evidence/1.3-recipe-rebuild.txt`) tests six sources of variation against a 6-step WAL base database. Each test starts from fresh files and reports the first differing page (and differing page-1 byte offsets when page 1 changes). Three of the six tests uncovered behavior subtler than naive intuition predicts; the experiment flags those with `FINDING:`.

1. **SQL `random()` in a value (lines 83–85).** Inserting `random()` into `events.amount` makes run 1 and run 2 differ on page 59 (1 page). SQLite's internal ChaCha20 PRNG is seeded from the OS on first use, so any SQL `random()` or `randomblob()` breaks byte reproducibility.
2. **`current_timestamp` in a value (lines 86–89).** `current_timestamp` has one-second resolution (`YYYY-MM-DD HH:MM:SS`). Two runs executed inside the same wall-clock second produce `identical` files (line 87); a third run executed after the second boundary advances differs on page 59 (line 88). Fast tests can hide a clock dependency unless they span a second boundary.
3. **Unordered `INSERT ... SELECT` (lines 90–95).**
   - Running `INSERT INTO archive(account_id, amount) SELECT account_id, amount FROM events WHERE amount > 0` twice on the same build with the same schema produces `identical` files (`FINDING`, lines 91 and 94), because the query planner picks the same scan order both times and assigns rowids in that order.
   - Running run 2 with `PRAGMA reverse_unordered_selects=ON` flips the scan order without changing the query semantics, and the two files immediately differ on page 29 (line 92). In production, adding an index, running `ANALYZE`, or upgrading SQLite can change an unordered scan the same way.
   - Appending `ORDER BY id` makes the two files `identical` again even under `reverse_unordered_selects=ON` (line 93).
4. **Different transaction batches (lines 96–107).** Steps 0–6 are run on one connection, comparing one transaction per step against merging steps `2+3` and `4+5` into single transactions (step 3 deletes rows that step 2 inserted):
   - **WAL mode, `secure_delete=ON` (recipe settings):** `identical` (`FINDING`, lines 98 and 105). Because WAL mode on one connection does not increment the header change counter per commit, and `secure_delete=ON` zeroes freed bytes inside pages, merging those transactions leaves zero byte difference.
   - **WAL mode, `secure_delete=OFF`:** `differ: first differing page 24, 2 page(s) differ [24 28]` (lines 100–102): 357 bytes on page 24 and 1,207 bytes on page 28, every single one inside unused space on B-tree leaf pages (the unallocated gap between the cell-pointer array and cell content area, or freeblocks—not freelist pages). When `secure_delete` is off, deleted cell payloads linger as stale bytes whose exact values depend on transaction batching.
   - **Rollback mode, `secure_delete=ON`:** `differ: first differing page 1, 1 page(s) differ [1], page 1 bytes [27 95]` (lines 104 and 107). Rollback mode increments the change counter on every commit, so fewer commits leave a smaller counter.
5. **Omitting a rowid after the maximum 64-bit rowid `9223372036854775807` (lines 108–110).** Once a table contains rowid `9223372036854775807` (`2^63 - 1`), SQLite cannot increment past it and chooses a random unused rowid for the next insert without an explicit id. The two runs differ on page 29.
6. **Connection sessions in WAL mode (lines 112–116).** Comparing steps 0–6 built with one connection per step against one long-lived connection for all 7 steps:
   - **WAL mode:** `differ: first differing page 1, 1 page(s) differ [1], page 1 bytes [27 95]` (`FINDING`, lines 113 and 115). In WAL mode, `pagerWalFrames` writes `dbFileVers + 1` into the change counter, but `dbFileVers` is refreshed from page 1 only when page 1 is read from storage, not after each WAL commit. One long-lived connection therefore writes the same counter value on every commit, while each new connection session re-reads page 1 and advances it by one.
   - **Rollback mode:** `identical` (lines 114 and 116), because rollback mode counts commits rather than sessions.

### Determinism rules verified by Experiment 1.3

| Source of variation | Rule enforced in `internal/recipe` and `internal/sqlitex` | Evidence (Exp 1.3) |
| --- | --- | --- |
| SQLite build | Pin driver and CLI to one Fossil check-in; verify `sqlite_source_id()` on startup | Exp 1.1; header bytes 96–99 store library version |
| Persistent & per-connection PRAGMAs | Set `page_size`, `auto_vacuum`, `encoding`, `application_id`, `journal_mode` at creation; set `synchronous`, `foreign_keys`, `secure_delete`, `cache_size`, `wal_autocheckpoint`, `busy_timeout` on every open | Lines 3, 78–80 |
| Randomness | Generate values in Go from a seeded `math/rand/v2` PCG; ban SQL `random()` / `randomblob()` | Variant 1 (page 59 differs) |
| Wall clock | Pass timestamps as bound parameters; ban `current_timestamp` / `date('now')` | Variant 2 (page 59 differs after 1 s) |
| Query-plan row order | Always add `ORDER BY` to `INSERT ... SELECT` | Variant 3 (page 29 differs under reversed scan; identical with `ORDER BY id`) |
| Stale deleted bytes | Set `PRAGMA secure_delete=ON` whenever different histories or batchings must match | Variant 4 (pages 24 and 28 differ in unused leaf bytes when `OFF`; identical when `ON` in WAL) |
| Rowid selection | Declare explicit `INTEGER PRIMARY KEY` and supply every id | Variant 5 (page 29 differs past `9223372036854775807`) |
| WAL connection sessions | Canonical rebuilds use one session per step, or compare with header bytes 24–27 and 92–95 masked (`MatchStep`) | Variant 6 (WAL differs only at page 1 bytes 27 and 95; rollback identical) |

### What attacked this chapter

<!-- adversarial-results -->

---

## 7. Platform implications

Every operational rule below follows directly from one of this chapter's experiments:

1. **Verify the exact SQLite build and compile options at service startup (Experiment 1.1).** Two binaries reporting version `3.53.4` can still carry different compile options (`ENABLE_STAT4`, `DEFAULT_WAL_SYNCHRONOUS`, `ENABLE_DBPAGE_VTAB`). A platform should check `sqlite_source_id()` and required `PRAGMA compile_options` on startup instead of assuming the library matches what was tested.
2. **Design the platform around SQLite's two external boundaries: connections and files (Section 3, Experiment 1.2).** The tokenizer, parser, planner, and VDBE live inside the library and run per connection (`sqlite3_prepare_v2` compiles statements on one `sqlite3` handle and `sqlite3_step` auto-reprepares them on schema changes). A multi-database service does not inspect or replace those layers; it controls per-connection settings above the pager and owns the files and kernel I/O below the pager.
3. **Treat `.db` plus `-wal` as the live database unit, not `.db` alone (Experiment 1.2).** In WAL mode, a committed `INSERT` executes zero `pwrite64` calls on `t.db`; the new page lives exclusively in `t.db-wal` until a checkpoint copies it back. Copying only `.db` while a WAL connection is open silently drops committed transactions.
4. **Account for fresh-WAL creation syncs when opening and closing databases (Experiment 1.2).** Under `synchronous=FULL`, the first commit on a freshly opened WAL database pays 3 syncs (`t.db-wal` header, directory `.`, and `t.db-wal` commit frame), whereas rollback mode pays 4 syncs on every commit (`t.db-journal`, `.`, `t.db-journal`, `t.db`). Churning connections open and closed so the WAL is deleted and recreated on every write multiplies directory syncs.
5. **The translated Go driver (`modernc.org/sqlite`) matches the C build's storage I/O (Experiment 1.2).** Across both rollback and WAL modes, the Go driver and the C CLI executed identical writes, syncs, locks, reads, unlinks, and closes and produced byte-identical database files. The only difference—extra `fcntl(F_SETFD)` calls from `modernc.org/libc`—does not touch disk layout or lock protocol.
6. **Byte-level verification requires controlling every source of physical variation (Experiment 1.3).** Even with deterministic SQL, `secure_delete=OFF` leaks history-dependent stale bytes into unused regions of B-tree pages, and WAL mode advances the header change counter (bytes 24–27 and 92–95) per connection session rather than per commit. A platform that verifies rebuilt files by SHA-256 must either standardize session boundaries and enable `secure_delete=ON` or explicitly account for those 8 header bytes.

---

## 8. Knowledge check

Answers are in [answers.md](answers.md).

1. In `prepare.c` (`sqlite3_prepare_v2`) and `vdbeapi.c` (`sqlite3_step`), what happens if another connection alters a table's schema between `Prepare` and `Step`, and which flag makes that recovery possible?
2. In Experiment 1.2, how many `pwrite64` calls hit `t.db` during the committed `INSERT` in rollback mode versus WAL mode, and where do the new row's bytes go in each mode?
3. Why did the Go driver issue more syscalls than the C CLI in Experiment 1.2's twin check (33 vs 31 in rollback mode, 18 vs 17 in WAL mode), and why does that difference not affect the database file or locking?
4. Experiment 1.3 ran the same 7 recipe steps (0–6) in WAL mode once with a fresh connection per step and once on a single long-lived connection. Which exact bytes differed, and why did the same comparison produce `identical` files in rollback mode?
5. In Experiment 1.3 variant 4 (merging steps 2+3 and 4+5 into single transactions in WAL mode), why did `secure_delete=ON` produce `identical` files while `secure_delete=OFF` differed on pages 24 and 28?
