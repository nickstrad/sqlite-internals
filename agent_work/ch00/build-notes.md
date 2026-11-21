# Chunk 0 build notes (plain data for the prose pass)

Builder: Claude Opus 5.5, branch `chunk/ch00`, 2026-11-20. Machine: linux/amd64, Go 1.26.8,
strace 6.8, gcc 13.3, Docker. darwin: cross-built and cross-vetted only, never run.

## Corrections to the outline

1. **The CLI's VFS trace goes to stdout, not stderr.** `shell.c.in:12911` `vfstraceOut` writes with
   `cli_puts(z, p->out)` and `fflush` after every fragment; `p->out` is stdout
   (`shell.c.in:13331`). Each VFS call is written as two or three fragments: the call text
   before the real method runs (`ext/misc/vfstrace.c:420`), then ` -> rc` (and details) after
   it. docs/knowledge/go-access-and-vfs.md §1e says stderr. `internal/trace` parses stdout.
2. **Two C builds of the same pin sync differently.** The installed `/usr/local/bin/sqlite3`
   calls `fsync`; a default `./configure && make` of the pinned tree (what
   `go run ./cmd/course build-tools` produces, on glibc and on musl) calls `fdatasync`. The
   modernc Go driver calls `fsync`. Same files, same order, same count in every case (smoke
   output). Any chapter that names the sync syscall must say which build it ran.
3. **modernc's Go driver opens files with `open`, not `openat`** (linux/amd64, flags
   `O_RDWR|O_CREAT|O_LARGEFILE|O_NOFOLLOW|O_CLOEXEC`); the C builds use `openat`. strace filters
   must include both.
4. **The 3.53.4 CLI opens a database named on its command line twice.** Before the real open
   the shell calls `isScriptFile` → `isDatabaseFile` (`shell.c.in:4229-4320`), which does a
   read-only `sqlite3_open_v2` + `SELECT count(*) FROM sqlite_schema`; `.open FILE` does the same
   through `deduceDatabaseType` (`shell.c.in:4258`, called from `open_db` at `:4495`). In WAL mode
   that probe maps the `-shm` and writes its 8 allocation bytes a second time, so a strace of
   the CLI shows extra syscalls that are not part of the statement. The probe is skipped by
   `.open --normal FILE` (`shell.c.in:10222` sets `SHELL_OPEN_NORMAL`). `sqlitex.OpenScript`
   and every CLI runner in `internal/trace` therefore pass the database as
   `.open --normal '<path>'` on stdin. With that, the Go driver's and the CLI's writes for one
   WAL insert are identical (smoke "twin preview").
5. **`-cmd` + `-bail` drops the rest of the command line.** `shell.c.in:13536`:
   `rc = runOneSqlLine(...); if( bail_on_error ) goto shell_main_exit;` exits after any SQL
   `-cmd`, so `sqlite3 -bail -cmd "PRAGMA x" db "INSERT ..."` exits 0 without running the
   INSERT. The course's CLI runner always passes `-batch -bail -init /dev/null` and never uses
   `-cmd`.
6. **strace exits by SIGKILL itself, not with status 137.** When the tracee is killed by an
   injected SIGKILL, strace re-raises the signal: Go's `ProcessState.ExitCode()` is `-1`. The
   "137" in docs/knowledge/library-route-probes.md is the shell's 128+9 rendering. `trace.Result`
   reports `ExitCode -1` and `Killed "SIGKILL"`.
7. **A serialized WAL-mode database does not open after `Deserialize`** unless header bytes
   18 and 19 are rewritten from 2 to 1. Query after deserializing the WAL-format image:
   `unable to open database file (14)`. After that failure, a second `Deserialize` on the same
   connection also failed; a fresh connection with the patched image works
   (`TestRawWrappers`).
8. **SeaweedFS needs volume headroom per bucket.** Each bucket is a collection that grows 7
   volumes on first write; with `-volume.max=20` the third bucket fails every PUT with HTTP 500.
   compose.yaml uses `-volume.max=100`. Details in `agent_work/probes/seaweedfs-s3/RESULT.md`.
9. **SeaweedFS ListObjectsV2 is not pure byte order** when `/` and bytes below 0x2f meet at the
   same key position (`k/a`, `k/a/x`, `k/a-`). Same RESULT.md.
10. **A custom writable VFS IS possible on modernc v1.60.1 without cgo or a fork** (the
    outline's §4.1/§4.3 and docs/knowledge say not available / UNVERIFIED). Built and run:
    `agent_work/probes/vfs-viability/RESULT.md`. The orchestrator records the verdict.
11. **The lab image is Debian (glibc), not Alpine.** An Alpine (musl) build of the CLI writes
    stdout with `writev` (the merge parser now handles `writev` too), opens with `open`, and
    musl's `pwrite` issues `fcntl(F_GETFL)` before every write. Those differences would make the
    container's traces differ from a typical Linux server, so `Dockerfile.lab` uses
    `golang:1.26-bookworm`. The smoke output inside the container is identical to the host's.

## Repo files added

| Path | What |
| --- | --- |
| `go.mod`, `go.sum` | module `github.com/nickstrad/walfleet`, `go 1.26.0`, requires `modernc.org/sqlite v1.60.1` (pulls `modernc.org/libc v1.77.1`) |
| `.gitignore` | adds `/_work/`, `/_src/`, `/_tools/` |
| `sqlite/PIN` | key=value lines: release, release_date, tag, git_commit, fossil_id, source_id, driver_module, driver_version, libc_module, libc_version, git_repo |
| `sqlite/pin.go` | package `sqlite`: `//go:embed PIN`; `ParsePin`, `Current()`, `Pin.SourceDirName()` ("sqlite-3.53.4") |
| `internal/sqlitex/` | open with explicit settings, pin check, raw driver wrappers, work dir, CLI locator/runner |
| `internal/trace/` | strace runner/parser, kill-at-syscall, CLI vfstrace runner/parser, merged stream, LINUX-ONLY helper |
| `internal/repocheck/` | logic of every `cmd/course` check (unit-tested) |
| `cmd/course/` | the repo tool |
| `Dockerfile.lab`, `.dockerignore`, `compose.yaml`, `docker/seaweedfs/s3.json` | lab container and S3 store |

Sizes (lines incl. blank, no comments anywhere): sqlite 83 (+54 test); sqlitex 1079 (+401
test); trace 1116 (+389 test); repocheck 1224 (+520 test); cmd/course 563. The brief estimated
150–250 lines for sqlitex; it is larger because it also holds the CLI locator/runner (163),
work-dir/repo-root helpers (98) and per-connection verification of every setting. If chapter 1
inlines it, `open.go`, `settings.go` and `pin.go` (576) are the core; `raw.go`, `cli.go` and
`workdir.go` could be shown in the chapters that first use them.

## sqlite/PIN and package `sqlite`

- `Current()` parses the embedded PIN once per call and panics if it is malformed (a broken PIN
  is a build defect). `ParsePin` rejects lines without `=`, duplicate keys, missing required
  keys, and a `source_id` that does not end with `" " + fossil_id`.
- Test `TestPinMatchesGoMod` reads `../go.mod` and fails if it does not require
  `driver_module driver_version`; it also compares against `debug.ReadBuildInfo` when present.

## internal/sqlitex

### Settings (`settings.go`)

- `FileSettings{PageSize, AutoVacuum, Encoding, ApplicationID, UserVersion, JournalMode}` —
  persistent settings stored in the file. `DefaultFileSettings()`: 4096, NONE, UTF-8,
  `CourseApplicationID` (0x53514958 = "SQIX" = 1397836120), user_version 0, journal mode `wal`.
- `ConnSettings{Synchronous, ForeignKeys, SecureDelete, CacheSize, WALAutocheckpoint,
  BusyTimeout}` — reset on every open. `DefaultConnSettings()`: FULL, ON, OFF, -2000, 1000, 5s.
  Synchronous FULL is upstream's default and the modernc default; WAL+NORMAL is a common
  production choice, so experiments that teach sync cost should set it explicitly.
- `Options{File, Conn, SingleConnection, ImmediateTx, ReadOnly}`; `DefaultOptions()` sets
  `ImmediateTx: true` (DSN `_txlock=immediate`, so `BEGIN` takes the write lock up front).
- `Validate()` on both: page size power of two 512–65536; known auto_vacuum, encoding,
  journal mode, synchronous names; non-negative busy timeout and autocheckpoint.
- `String()` on both prints the settings in one line (used in evidence output).

### Open (`open.go`)

- `Open(ctx, path, options) (*DB, error)`: validates, builds the DSN, wraps
  `sqlite.NewConnector(dsn)` in `configuredConnector`, `sql.OpenDB`, Ping, then (unless
  ReadOnly) reads the file settings back and fails if page_size, auto_vacuum, encoding,
  application_id or journal mode differ from the request (`user_version` is not compared on an
  existing file, because recipes change it). `DB` embeds `*sql.DB` and keeps `Path` and `Options`.
- `configuredConnector.Connect` runs on EVERY physical connection database/sql opens:
  1. `PRAGMA busy_timeout=N` first, so the rest can wait for locks;
  2. unless ReadOnly: if `PRAGMA page_count` is 0 (new file), the creation pragmas in order
     page_size, auto_vacuum, encoding, application_id, user_version (page size, auto_vacuum and
     encoding only take effect before the first write);
  3. unless ReadOnly: `PRAGMA journal_mode=<mode>` on every connection, failing if SQLite reports
     another mode (WAL is persistent in the file; DELETE/TRUNCATE/PERSIST/MEMORY/OFF are per
     connection, so a pooled connection would otherwise silently use DELETE —
     `TestRollbackJournalModeIsPerConnection`);
  4. synchronous, foreign_keys, secure_delete, cache_size, wal_autocheckpoint;
  5. reads all six per-connection values back through the raw driver connection and fails the
     Connect if any differs (`compareConnSettings`).
- `SingleConnection` sets MaxOpenConns=1, MaxIdleConns=1, no lifetime limits.
- `FileURI(path, query)` makes `file:/abs/path?query` with the path percent-escaped
  (`TestFileURIEscapes`: a directory named `odd ?#% name` works). `DSN` adds `_txlock=immediate`
  and `mode=ro` as asked.
- `ReadFileSettings(ctx, q)`, `ReadConnSettings(ctx, q)` read the live values from any
  `Querier` (`*sql.DB`, `*sql.Conn`, `*sql.Tx`).
- `TestConcurrentOpenOfNewDatabase`: four goroutines opening the same new file at once all
  succeed.

### Pin check (`pin.go`)

- `SourceID(ctx, q)`, `CheckPin(ctx, q)`, `CheckSourceID(what, id)` (returns
  `*PinMismatchError`), `PinLine(id)`.
- Pin line format (EXACT, first output line of every experiment):
  `sqlite_source_id: <id> (pin 3.53.4 ok)` or
  `sqlite_source_id: <id> (NOT the pin 3.53.4, want <pin id>)`.
- `MustPinCheck(ctx, q)` prints the line to stdout and exits 1 (after `FAIL: ...` on stderr) on a
  mismatch. `MustPinCheckMemory(ctx)` does it on a throwaway `:memory:` connection.

### Raw driver wrappers (`raw.go`)

All take a `*sql.Conn` and reach the driver connection through `Conn.Raw` with an interface
assertion (`withRawConn`); an unexpected driver type gives an error naming the capability.

- `ReadDBStatus(ctx, conn, op, reset) (DBStatus{Current, Highwater})` — `sqlite3_db_status`.
  `DBStatusOp` is an alias of modernc's type; constants re-exported: LookasideUsed, CacheUsed,
  SchemaUsed, StmtUsed, CacheHit, CacheMiss, CacheWrite, CacheSpill.
- `Serialize`, `Deserialize` — see correction 7.
- `DataVersion(ctx, conn, schema)` — `SQLITE_FCNTL_DATA_VERSION`; `PersistWAL(ctx, conn,
  schema, mode)` — `SQLITE_FCNTL_PERSIST_WAL` (mode -1 queries). These are the only two file
  controls the driver exposes.
- `Backup(ctx, source, destPath, pagesPerStep, afterStep)` — `sqlite3_backup_init/step/finish`;
  calls `afterStep(BackupStep{Step, More, Remaining, PageCount})` after every step so an
  experiment can write to the source between steps; `pagesPerStep` -1 = all. The driver opens
  its own destination connection and closes it in `Finish`. `Restore(ctx, dest, srcPath)` is the
  reverse (`NewRestore`, one step).
- `Checkpoint(ctx, q, schema, mode)` runs `PRAGMA <schema>.wal_checkpoint(<MODE>)` and returns
  `CheckpointResult{Busy, LogFrames, Checkpointed}` (the busy/log/ckpt triple of
  `sqlite3_wal_checkpoint_v2`). Modes NOOP, PASSIVE, FULL, RESTART, TRUNCATE. After
  `TRUNCATE` the result was `busy=0 log=0 checkpointed=0`; `NOOP` on a WAL with frames
  returned log>0, checkpointed=0.
- `ErrorCode(err)` (extended code via `*sqlite.Error`), `PrimaryErrorCode` (`& 0xff`),
  `IsBusy`. `TestBusyErrorCode`: second writer with busy_timeout 0 gets SQLITE_BUSY.

### Work dir and repo root (`workdir.go`)

- `RepoRoot()` walks up from the working directory to the `go.mod` whose module line is
  `github.com/nickstrad/walfleet` (so nested probe modules are skipped).
- `WorkBase()`: `$COURSE_WORK_DIR` (absolute) or `<repo>/_work`. `WorkDir(name)` removes and
  recreates `<base>/<name>`; names must match `^[A-Za-z0-9][A-Za-z0-9._-]*$` and not contain
  `..` (so `1.2`, `ch00-smoke` are fine; `../x`, `a/b`, `.hidden` are refused).
  `MustWorkDir` exits 1 on error.

### Pinned CLI (`cli.go`)

- Lookup order: `$SQLITE3_BIN` (an unusable value is an error, no fallback), then
  `$COURSE_TOOLS_DIR/bin/sqlite3` or `<repo>/_tools/bin/sqlite3`, then `sqlite3` on PATH. The
  FIRST candidate that exists is used and must report the pinned `sqlite_source_id()`;
  a mismatch is an error, never a silent fallback to the next candidate.
- Every run passes `-batch -bail -init /dev/null` (os.DevNull), so a user's `~/.sqliterc`
  cannot change results.
- `CLI.Run(ctx, CLIRun{Args, Stdin, Dir, Env})` → `CLIResult{Stdout, Stderr, ExitCode}`;
  `CLI.Command` returns the `*exec.Cmd` (used to run the CLI under strace).
  `CLI.Exec(ctx, dbPath, sql)` sends `OpenScript(dbPath, sql)` on stdin.
- `OpenScript(path, sql)` = `.open --normal '<path>'` + newline + sql (see correction 4).
- `LocateTool(name)` finds `showwal`, `dbhash` the same way (no version check: those tools
  print no source id). `ToolsDir()` = `$COURSE_TOOLS_DIR` or `<repo>/_tools`.
- `MustLocateCLI` prints `sqlite3 CLI sqlite_source_id: ... (pin 3.53.4 ok)` and exits 1 on error.

## internal/trace

### strace parser (`strace.go`, portable)

- Input: `strace -f -y -q -s N -o file` output (every line starts with the pid).
- `ParseStrace(r)` / `ParseStraceText(s)` → `StraceLog{Calls []Syscall, Exits []ProcessExit}`.
- `Syscall{Index, PID, Name, FD, Path, Offset, Length, Result, ResultText, Errno, Flags, Lock,
  Data, Truncated, Unfinished, Raw}`. Index is the order of syscall ENTRY.
- Handles `<unfinished ...>` / `<... name resumed>` pairs from other threads (joined into one
  call at its entry position), `= ?` (killed inside the call → `Unfinished`), `+++ exited with
  N +++`, `+++ killed by SIG... +++`, `--- SIG... ---` (skipped), `-1 ENOENT (...)` → Errno.
- Path: from `-y` annotations (`3</path>`); without `-y`, from an fd table built from
  `open`/`openat` results and cleared on `close`. Relative `openat`/`unlinkat` paths are joined
  to the `AT_FDCWD</cwd>` or dirfd path.
- Per syscall: open/openat (Path, Flags), pwrite64/pread64 (Length, Offset), write/read
  (Data = unescaped string, Length), writev/readv (Data = concatenated iov_base strings),
  fsync/fdatasync/close (FD, Path), ftruncate (Length = new size), mmap (Length, Flags, FD/Path,
  Offset), fcntl (Flags = command; `Lock{Command, Type, Whence, Start, Length}` for struct
  flock args), unlink/unlinkat (Path).
- `Syscall.String()` is a stable one-line form for evidence, e.g.
  `pwrite64 smoke.db-wal off=56 len=4096 = 4096`, `fcntl t.db F_SETLK F_RDLCK start=1073741824
  len=1 = 0`, `fsync smoke.db-wal = ? (process killed)`, `openat x.db O_RDWR|... = fd` (fd
  numbers are left out of the string because they vary).
- Helpers: `Filter`, `UnderDir(dir)`, `Named(names...)`, `Both(a, b)`, `RelativeTo(dir, calls)`,
  `KilledBy()`. `SplitArgs` splits at top-level commas respecting quotes, braces, brackets and
  `fd<path>` annotations; `Unquote` undoes strace's C escapes (`\n \t \" \\ \ooo \xhh`) and
  reports a trailing `...` as truncated.

### Runner (`strace_linux.go`; `strace_other.go` returns `ErrLinuxOnly`)

- `Run(ctx, Options{Command, Dir, Env, Stdin, Syscalls, StringLimit, KillAt})` → `Result{Log,
  Stdout, Stderr, ExitCode, Killed, Text}`. Runs `strace -f -y -q -s <limit, default 512> -o
  <temp> -e trace=<list> [-e inject=<syscalls>:signal=SIGKILL:when=K] -- <command>`.
  `DefaultSyscalls`: openat, open, close, pwrite64, write, writev, pread64, read, fsync,
  fdatasync, unlink, unlinkat, fcntl, ftruncate, mmap, munmap. A non-zero exit of the traced
  command is not an error (it is in `ExitCode`/`Killed`); a missing strace or an empty trace with
  a ptrace permission message is.
- `KillAt{Syscalls, When}`: SIGKILL on entry to the When-th call of those syscalls. strace
  counts per traced thread, so a Go victim must `runtime.LockOSThread()` before its SQLite work
  (the smoke child and `TestKillAtFirstSync` do).
- `SelfCommand(args...)` = `os.Executable()` + args (re-run the experiment binary as a child;
  works under `go run`).
- `Supported` is true on linux, false elsewhere.

### VFS trace (`vfs.go`, portable)

- `ParseVFSLine(line)` → `VFSCall{Index, Method, File, Args, Amount (n= / xTruncate size),
  Offset (ofst=, or the xShmLock slot offset), Flag (xSync FULL/NORMAL, xLock level,
  xFileControl op, xShmLock LOCK|EXCLUSIVE...), ShmSlot (WRITE, CKPT, READ1...), Result,
  Detail (text after the first comma of the result, e.g. `size=0`, `out=0`), Complete, Raw}`.
  Lines are `trace.xName(args) -> result[, detail]`; calls without a result (xShmBarrier,
  xRandomness) have empty Result.
- `ParseVFSTrace(r)` → `VFSTrace{Calls, Output, Other}`: `Output` holds the CLI's query output
  lines (they share stdout with the trace), `Other` holds `trace.` lines that are not calls
  (`trace.enabled_for("unix")`).
- `RunCLIVFSTrace(ctx, cli, CLITrace{Dir, DB, SQL})`: `sqlite3 -batch -bail -init /dev/null
  -vfstrace` with `.open --normal 'DB'` + SQL on stdin; portable (works on macOS given a CLI).
  `RelativeVFS(dir, calls)` strips the directory from file names and args.
- vfstrace prints a file's base name for file methods and full paths for xAccess/xDelete/
  xFullPathname; `RelativeVFS` makes both stable.

### Merged stream (`merge.go` portable; `RunCLIMerged` linux)

- `RunCLIMerged(ctx, cli, CLITrace, Options)` runs the CLI with `-vfstrace` under strace
  (always tracing write and writev; `-s 4096`), then `MergeStdoutVFS(log, 1, include)`.
- Merge rule: writes to fd 1 are reassembled into lines; a line starting `trace.x` opens a VFS
  event at the position of its FIRST fragment; syscalls until the line's newline are attached
  to it (`Event.Enclosed` = index of that VFS event); other lines become `EventOutput`.
  `Event.String()`: `VFS xSync(t.db-wal,FULL) -> 0` / `    SYS fsync t.db-wal = 0` (indented
  when enclosed) / `OUT wal`. `SyscallsWithin(i)`, `VFSCalls()`, `Relative(dir)`.
- Observed: xOpen's openat appears BEFORE the xOpen line (vfstrace prints xOpen after the real
  open returns), so it is not enclosed; xRandomness prints in one fragment, so its
  `/dev/urandom` read is not enclosed either. xSync encloses the fsync and, for a new
  journal/WAL, the directory open+fsync+close (the directory fsync is visible only in strace).
- A truncated fd-1 write is an error ("raise the string limit").

### LINUX-ONLY (`linuxonly.go`)

- `LinuxOnlyMessage(what, packagePath)` =
  `LINUX-ONLY: <what> skipped on <GOOS>; run it with: docker compose run --rm lab go run ./<packagePath>`.
  `WriteLinuxOnly`, `PrintLinuxOnly` print it.

## internal/repocheck and cmd/course

Run from the repo root: `go run ./cmd/course <command>`. Exit 0 = pass, 1 = a check failed,
2 = usage error. Each check prints `PASS <check>: <summary>` or `FAIL <check>: N problem(s)`
followed by one indented line per problem (`<file>:<line>: <message>`).

| Command | What it does |
| --- | --- |
| `fetch-source [-dest DIR]` | `git -c advice.detachedHead=false clone --quiet --depth 1 --single-branch --branch version-3.53.4 https://github.com/sqlite/sqlite <dest>.partial`, checks `manifest.uuid` = fossil_id and `git rev-parse HEAD` = git_commit, renames into place (default `_src/sqlite-3.53.4`). An existing dest is verified instead. 6 s here |
| `build-tools [-reuse] [-j N]` | copies the pinned tree (`$SQLITE_SRC`, else `_src/sqlite-3.53.4`, verified by manifest.uuid; `.git` skipped) to `<tools>/build/sqlite-3.53.4`, runs `./configure`, `make -jN sqlite3 showwal dbhash` (log `<tools>/build/build.log`), copies the three binaries to `<tools>/bin/`, checks the CLI's source id and that `-vfstrace` prints `trace.x` lines. `<tools>` = `$COURSE_TOOLS_DIR` or `_tools`. 2 min 08 s here (4 cores, `-j4`). Needs a C compiler and make (Xcode CLT on macOS; not run on macOS) |
| `inline [-check] <chapter.md>...` | marker `<!-- include: rel/path -->` or `<!-- include: rel/path lines=A-B -->` (path relative to the markdown file) must be IMMEDIATELY followed by a fenced block. `inline` rewrites the block body from the file (adds a fence if missing; language `go` for .go else `text`, an existing info string is kept; the fence grows to 4+ backticks when the content contains ```); `-check` fails on any byte difference and prints the first differing line. A file without a final newline is compared as if it had one |
| `check sources [chNN]` | per chapter dir `chNN-*`: each `SOURCES.sha256` line (`<sha256>  <path in pinned tree>`) must match the pinned file AND the copy at the chapter root (same base name); flags malformed lines, two entries with one base name, unlisted files at the chapter root (allowed: `<dir>.md`, `answers.md`, `SOURCES.sha256`, `SPANS.txt`), and any `.go` file at the chapter root |
| `check spans [chNN]` | `SPANS.txt` lines `file start end symbol` (`#` comments allowed): the symbol must appear in lines start..min(start+11, end) of the pinned file (the window the old outline-span-check.sh used); identifiers match on word boundaries (`unixLock` does not match `unixLockHelper`), other symbols (e.g. `balance(`) as substrings; `end` must not exceed the file length. On failure it reports the first other line that mentions the symbol |
| `check comments <old> <new>` | every tracked `.go` file outside `kb/` at either revision: added or removed files fail; otherwise both versions are tokenized with go/scanner, comments dropped except directives (`//go:...`, `//line `, `//export `, `//extern `, `// +build`, and Go's `//name:value` form), auto-inserted semicolons normalized, and the token streams must be equal; both versions must parse. Prints the first differing token |
| `check prose [-exclude PREFIX]... <base>..<head>` | at `<head>`: every non-directive comment line of tracked `.go` files outside `kb/`, and every non-blank line of `chNN-*/chNN-*.md` and `chNN-*/answers.md` except include-block fences and content, is blamed with `git blame --porcelain -w -M -C <base>..<head>`; lines last changed inside the range must have an author name starting `Gemini 4 Neon`; lines older than `<base>` (blame boundary) are counted and not checked |
| `check evidence [chNN] [-runs N]` | each `chNN-*/experiments/N.M-name/` is built once (`go build -buildvcs=false`) and run N times (default 2) from the repo root; stdout minus `INFO:` lines must equal `chNN-*/evidence/N.M-name.txt` minus `INFO:` lines. Statuses PASS, DIFF (first differing line), FAIL (non-zero exit + stderr tail), BUILD-FAIL, MISSING (no evidence file), LINUX-ONLY (on non-Linux, output differs and contains a `LINUX-ONLY:` line; counts as pass). Experiments sorted numerically (1.2 < 1.10) |
| `check gofmt` | `go/format.Source` of every tracked `.go` file outside `kb/` and outside nested modules (dirs with their own go.mod, i.e. the Phase 1 probes) |
| `check paths` | every tracked file outside `agent_work/`, `kb/`, `STATE.md` must not contain the machine-local path fragment (slash, `root`, slash) |
| `check all [-runs N] [-skip-evidence]` | sources, spans, `inline -check` of every existing chapter markdown, evidence, gofmt, paths |

Why tokens and not go/printer for `check comments`: go/printer keeps line breaks from the
original positions, so deleting a comment line between two statements makes the printer emit
a blank line that the comment-free original did not have — a false "code changed". Token
streams ignore layout and still catch every code change. (Reasoned from go/printer's
position-based line breaking; not demonstrated with a failing printer run.)

Current repo state for `check all`: sources/spans/evidence pass with "no chapter directories
yet"; gofmt passes (30 files); **paths FAILS on 9 pre-existing lines** in OUTLINE.md (53, 1008,
1019, 1020, 1082) and docs/knowledge/{determinism-and-rebuild.md:6, go-access-and-vfs.md:92,
source-inventory-ch01-04.md:8, source-inventory-ch05-09.md:5}. I may not edit OUTLINE.md and
did not edit the research reports.

Tests (`go test ./...`): temp dirs for sources/spans/inline/evidence (a throwaway module with
four experiments: PASS, DIFF, MISSING, FAIL), temp git repos with authored commits for
comments/prose/gofmt/paths, canned strace/vfstrace text in `internal/trace/testdata/`, and live
Linux tests (`strace_linux_test.go`) that re-run the test binary as a writer under strace,
kill it at the first sync, and merge a real CLI trace. Live tests skip when strace or the
pinned CLI is unavailable.

## Docker

- `Dockerfile.lab`: `golang:1.26-bookworm` + strace, build-essential, git;
  `git config --system safe.directory '*'` (the bind-mounted repo belongs to another uid);
  `GOTOOLCHAIN=local`, `GOFLAGS=-buildvcs=false`. At image build it runs this repo's own
  `cmd/course fetch-source -dest /opt/sqlite-src` and `build-tools` into
  `/opt/course-tools` (so the CLI in the image is a default `./configure` build of the pin),
  then removes the build copy and `.git`. Runtime env: `SQLITE_SRC=/opt/sqlite-src`,
  `COURSE_TOOLS_DIR=/opt/course-tools`, `COURSE_WORK_DIR=/work`. Image build: ~5 min first time
  (clone + 3 min tool build).
- `compose.yaml` (project name `sqlite-internals`):
  - `lab`: `cap_add: [SYS_PTRACE]`, repo bind-mounted at `/src`, named volumes `lab-work:/work`
    (container-local filesystem for fsync/lock behaviour), `go-mod-cache:/go/pkg/mod`,
    `go-build-cache:/go/build-cache` (`GOCACHE`), S3 env `COURSE_S3_ENDPOINT=http://seaweedfs:8333`,
    `COURSE_S3_REGION=us-east-1`, `COURSE_S3_ACCESS_KEY=course`, `COURSE_S3_SECRET_KEY=course-secret`.
  - `seaweedfs`: `chrislusf/seaweedfs:4.48`, `server -dir=/data -ip.bind=0.0.0.0
    -master.volumeSizeLimitMB=64 -volume.max=100 -s3 -s3.config=/etc/seaweedfs/s3.json`, only
    the S3 port published: `${COURSE_S3_HOST_PORT:-18333}:8333`; named volume `seaweedfs-data`
    (compose-scoped: `sqlite-internals_seaweedfs-data`, distinct from the VM's other
    `seaweedfs-data`).
- Verified here: `docker compose up -d seaweedfs` (container running; S3 answers on 127.0.0.1:18333 and on seaweedfs:8333 from the lab);
  `docker compose run --rm lab go test ./internal/... ./sqlite/` all pass including the live
  strace tests (ptrace works with SYS_PTRACE under the default seccomp profile);
  `docker compose run --rm lab go run ./agent_work/ch00/smoke` twice → identical, and identical
  to the host output.
- Worktree caveat (this VM only): `.git` of a worktree points outside the mount, so git-based
  checks inside the container need
  `docker compose run --rm -v "$G:$G" -v "$PWD:$PWD" -w "$PWD" lab ...` with `G` = the main
  checkout's `.git`. With that, `check all` in the container gave the same result as on the
  host (gofmt 30 files pass; paths fails on the same 9 pre-existing lines). A normal clone needs nothing extra.
- Files written into `/src` from the container are owned by root on a Linux host.

## Smoke experiment

Command: `go run ./agent_work/ch00/smoke` (repo root). Runtime 0.56 s. Output:
`agent_work/ch00/smoke.txt`. Two consecutive runs identical apart from the `INFO:` line; the
Debian container's output is identical too. Exit 0 and last line `SMOKE OK`.

What it does and printed:

1. Opens `_work/ch00-smoke/smoke.db` through `sqlitex.Open` (defaults + SingleConnection +
   wal_autocheckpoint 0); first line `sqlite_source_id: 2026-07-24 19:02:57 bf7c7f30…bcc
   (pin 3.53.4 ok)`; prints the read-back settings: `page_size=4096 auto_vacuum=NONE
   encoding=UTF-8 application_id=1397836120 user_version=0 journal_mode=wal` and
   `synchronous=FULL foreign_keys=ON secure_delete=OFF cache_size=-2000 wal_autocheckpoint=0
   busy_timeout=5000ms`.
2. Locates the pinned CLI (here `_tools/bin/sqlite3`, the build-tools build).
3. Baseline: recreates the db with one row, re-runs itself as `child-insert` (LockOSThread,
   one INSERT, close) under strace. 33 file syscalls on the db files, in order: open db, read
   header (100 B) and page 1; open `-wal`, open `-shm`, ftruncate `-shm` to 3, 8 one-byte
   pwrites at 4095…32767 (allocating the 32 KiB shm region), read pages 1–2; WAL header
   pwrite (32 B @0), **fsync -wal, open+fsync+close of the directory**, frame header (24 B @32)
   and page (4096 B @56), **fsync -wal** (the commit); at close: **fsync -wal**, read the frame
   back, write page 2 to the db, ftruncate db to 8192, **fsync db**, unlink `-shm`, unlink `-wal`.
   5 syncs: `fsync(smoke.db-wal) fsync(.) fsync(smoke.db-wal) fsync(smoke.db-wal)
   fsync(smoke.db)`. Rows after: 2, integrity ok.
4. Kill at the first sync (`inject=fsync,fdatasync:signal=SIGKILL:when=1`): killed by SIGKILL
   on entry to `fsync smoke.db-wal` (the WAL-header sync), never printed its commit line, 1 WAL
   write (the 32-byte header) had happened. After reopen: 1 row (the insert is lost),
   integrity ok.
5. Twin preview: the same INSERT by the C CLI (`PRAGMA synchronous=FULL; PRAGMA
   wal_autocheckpoint=0;` first) under strace: identical pwrite list (file, offset, length) and
   identical sync targets; only the call name differs (`fdatasync` for the build-tools CLI,
   `fsync` for the installed CLI and the Go driver).
6. CLI `-vfstrace` of `SELECT count(*) FROM t;`: 57 VFS calls (xAccess=2 xClose=2 xDelete=1
   xDeviceCharacteristics=5 xFileControl=8 xFileSize=4 xFullPathname=1 xLock=2 xOpen=2 xRead=4
   xShmBarrier=6 xShmLock=16 xShmMap=2 xShmUnmap=1 xUnlock=1), output `2`. (The installed CLI
   gives 58: one extra xRandomness.)

On non-Linux the strace steps are replaced by one `LINUX-ONLY:` line and the CLI step expects
1 row (darwin: cross-built only).

## Probes

- SeaweedFS S3: `agent_work/probes/seaweedfs-s3/RESULT.md`. Conditional puts (`If-None-Match:
  *`, `If-Match`) enforced, atomic among 2 and 8 concurrent writers in every round; multipart
  invisible until Complete; listing order caveat; client recommendation aws-sdk-go-v2.
- VFS viability: `agent_work/probes/vfs-viability/RESULT.md`. Read-only `fs.FS` VFS works
  (logging, in-memory, HTTP range reads; WAL files need `immutable=1` and only show
  checkpointed data). A writable Go forwarding VFS with xShm* works in rollback and WAL mode,
  including deterministic Go fault injection, with 34 unsafe.Pointer uses and a `go vet
  -unsafeptr=false` requirement. Recommendation (1): real VFS experiments in chapter 7.

## Verification commands run (all on this VM)

| Command | Result |
| --- | --- |
| `gofmt -l` over root-module files | empty |
| `go build ./... && go vet ./... && go test ./...` | pass |
| `agent_work/verify/crossbuild.sh` | `CROSSBUILD OK` (linux/amd64 build+vet+test; darwin/arm64 and darwin/amd64 build+vet+test-compile; gofmt) — 46 s |
| `go test -count=3 ./internal/...` | pass three times |
| `go run ./cmd/course fetch-source` / `build-tools` | OK / OK (sqlite3 7,220,312 B, showwal 5,051,664 B, dbhash 5,042,296 B) |
| `go run ./cmd/course check all` | FAIL only on the 9 pre-existing machine-local paths listed above |
| `go run ./cmd/course check prose a7bb640..HEAD` | PASS: 0 prose lines in range; 189 lines predate a7bb640 (Phase 1 probe comments) |
| `go run ./cmd/course check comments a7bb640 HEAD` | FAIL as expected: every chunk-0 Go file is "added" (the check is for prose passes) |
| `docker compose build lab`; `docker compose up -d seaweedfs` | OK |
| `docker compose run --rm lab go test ./internal/... ./sqlite/` | pass (live strace tests included) |
| smoke on host x2, in container x2 | identical, `SMOKE OK` |

## Repair round 1 addendum (facts that changed; details in REPAIR-REPORT.md)

- `OpenScript(path, sql)` = `.open --normal "<path>"` with `\`→`\\`, `"`→`\"`, LF→`\n`, CR→`\r`.
  The 3.53.4 dot-command parser does not support `''` doubling inside single quotes
  (`shell.c.in:8985-8998`); double-quoted arguments go through `resolve_backslashes` (`:4778`).
- In-memory: `sqlitex.OpenMemory(ctx)` = `Open(ctx, ":memory:", MemoryOptions())`, where
  MemoryOptions = DefaultOptions with journal_mode `memory` and SingleConnection. Every pooled
  connection to `file::memory:` would get its own private database, and an in-memory database
  reports journal_mode `memory` even when asked for `wal`, so WAL is refused there. It still gets
  page_size 4096 and application_id SQIX.
- Existing databases: `Open` refuses to switch an existing file into or out of WAL. Error:
  `existing database does not match the requested file settings: journal_mode want delete got wal (…)`.
  Switching between rollback modes (delete/truncate/persist/memory/off) on an existing rollback
  file still works and stays per connection. Experiments about rollback mode must create their
  own file in that mode; they cannot reopen a WAL platform database as rollback.
- Creation order on an empty file: page_size, auto_vacuum, encoding → `journal_mode=<mode>`
  (switching to WAL writes page 1 with bytes 18-19 = 2) → application_id, user_version. An empty
  database that has not been stamped yet (application_id 0, schema_version 0) gets stamped by
  whichever connection opens it. While two connections race to switch a new file into WAL,
  SQLite returns SQLITE_BUSY at once without calling the busy handler; Open retries this step
  every 10 ms until BusyTimeout. Measured: 1000/1000 concurrent-open runs pass, worst case 0.40 s.
- BusyTimeout must be a whole number of milliseconds (`PRAGMA busy_timeout` takes ms).
- Pin check: `CheckPin`/`WritePinCheck`/`MustPinCheck*` also compare the linked
  `modernc.org/sqlite` and `modernc.org/libc` versions (runtime/debug build info) against
  `driver_version`/`libc_version`. Driver v1.58.0 wraps the same SQLite 3.53.4 (same
  source_id), so before this change it passed. `go test` binaries record no dependency list, so
  there the check is skipped (and `TestPinMatchesGoMod`'s build-info comparison never runs).
- strace parser: `= ? ERESTARTSYS (…)` → Errno ERESTARTSYS, Result -1, not Unfinished (the
  call was interrupted and restarted, and strace prints the restart as a new call).
  `fd</path>(deleted)` is accepted. Seen live: modernc unlinks `-shm` before closing it on last
  close (`unlink smoke.db-shm` then `close(6</…smoke.db-shm>(deleted))`), so the smoke
  baseline now lists 34 calls including `close smoke.db-shm = 0`. Without `-y`, one fd table
  is shared across all pids/tids. writev/readv Length = sum of iov_len. An `<unfinished ...>`
  split after a comma no longer doubles the comma.
- vfstrace lines: arguments end at the first `) -> `. Results may contain parentheses
  (`9999 (0x270f)` from `vfstrace_print_errcode`, `ext/misc/vfstrace.c:345`).
- Merge: failed writes to fd 1 are ignored, and a short write counts only its returned bytes. A
  VFS line is recognised as soon as its first 7 bytes `trace.x` have arrived, even if they
  arrive in two writes.
- `trace.ErrPtraceDenied`: `Run` wraps it when strace cannot ptrace. Live tests skip only on it.
- repocheck: a chapter without `SOURCES.sha256` fails `check sources` (an empty file is fine).
  `-runs` < 1 is a usage error (exit 2). `check all` works from any directory. Include markers
  inside other ``` / ~~~ fences are ignored. Directives are only `//go:<known verb>`,
  `//nolint:<a,b>`, `//line`, `//export`, `//extern`, `// +build`, so `//note:…` counts as prose.
