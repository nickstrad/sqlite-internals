# Chunk 0 Adversarial Tests (Foundation)

Adversarial test suite for Chunk 0 (`sqlite/`, `internal/sqlitex/`, `internal/trace/`,
`internal/repocheck/`, `cmd/course/`). Every attack prints one structured line per case
(`ATTACK <id> | <tried> | EXPECT <prediction> | GOT <observed> | HELD|BROKEN`) and exits 0
when the attack itself completes; `BROKEN` lines are findings, not harness errors.

Run all groups from the repository root:

```sh
./agent_work/ch00/adversarial/run-all.sh
```

Output is saved to `agent_work/ch00/adversarial/results.txt`. Scratch files live only under
`_work/adv-ch00/`. Iteration counts were kept compact to stay well under the 10-minute budget
(KILL-04: 5 runs at each of `K=1, 2, 3`; SET-01/SET-02: 50 concurrent pooled connections;
SET-05: 20 concurrent read-only connections).

## Attack table

| ID | Group | Claim attacked | Method | What `BROKEN` would mean |
| --- | --- | --- | --- | --- |
| `PIN-01` | `pin-check` | `sqlitex` rejects a Go driver wrapping a different SQLite release | Copy `sqlite/` and `internal/sqlitex/` into a scratch module under `_work/adv-ch00/`, require `modernc.org/sqlite v1.62.0`, run `MustPinCheckMemory` | Experiments could silently run against a newer SQLite release without failing loudly |
| `PIN-02` | `pin-check` | `sqlitex` rejects a different `modernc.org/sqlite` release even within SQLite 3.53.4 | Same scratch module requiring `modernc.org/sqlite v1.58.0` (older translation of 3.53.4) | Runtime pin check only validates `sqlite_source_id()` and misses `modernc.org/sqlite` release drift (`v1.58.0` vs `v1.60.1`) |
| `PIN-03` | `pin-check` | `$SQLITE3_BIN` with wrong source id fails without fallback | Point `$SQLITE3_BIN` at a fake script printing a bogus `sqlite_source_id()` and call `LocateCLI` | `LocateCLI` accepted an unpinned CLI or silently fell back to `_tools`/PATH |
| `PIN-04` | `pin-check` | `$COURSE_TOOLS_DIR/bin/sqlite3` with wrong source id fails without fallback to PATH | Put a fake `sqlite3` in `$COURSE_TOOLS_DIR/bin/` while a real pinned CLI sits on PATH | `LocateCLI` silently skipped a broken tools-dir binary and hid the mismatch |
| `PIN-05` | `pin-check` | `sqlite.ParsePin` rejects malformed or inconsistent `PIN` files | Pass tampered `PIN` texts (mismatched `fossil_id`, missing `driver_version`, duplicate `release`) | A corrupted `sqlite/PIN` could slip past parsing |
| `CHK-01` | `course-checks` | `course check comments` catches logic edits hidden inside comment diffs | Temp git repo: add 10 comment lines plus a 1-char `<` to `<=` mutation; run `course check comments` | A prose pass could silently alter experiment logic without detection |
| `CHK-02` | `course-checks` | `course check comments` allows pure comment edits | Temp git repo: add doc, line, multiline, and mid-expression `/* */` comments; run `course check comments` | Legitimate prose passes would suffer false-positive rejections |
| `CHK-03` | `course-checks` | `course check comments` catches altered compiler directives | Temp git repo: change `//go:build linux` to `//go:build !linux` during a comment edit | Build tags or `//go:embed` directives could be altered during a prose pass |
| `CHK-04` | `course-checks` | `course check prose` flags non-Neon Go comments | Temp git repo: non-Neon author commits a `// comment` in `lib.go`; run `course check prose` | Non-Neon comments could enter the repo undetected |
| `CHK-05` | `course-checks` | `course check prose` accepts `Gemini 4 Neon` comments and chapter prose | Temp git repo: `Gemini 4 Neon` commits Go comments and `ch01-demo.md` | Valid Neon prose would be wrongly rejected |
| `CHK-06` | `course-checks` | `course check prose` flags non-Neon edits inside a Neon multiline `/* */` comment | Temp git repo: Neon writes 3-line block comment; non-Neon edits interior line 2 | Partial edits to existing multiline comments would evade provenance checks |
| `CHK-07` | `course-checks` | `course check prose` does not exempt non-directive `//word:word` comments | Temp git repo: non-Neon author commits `//note:unauthorized prose` (no space after `//`) | `IsDirective` is overly broad and lets non-Neon prose bypass `check prose` via `//word:word` syntax |
| `CHK-08` | `course-checks` | `course inline -check` catches 1-byte drift in full and `lines=A-B` includes | Flip 1 byte inside full include block and inside `lines=5-7` block; run `course inline -check` | Chapter code listings could diverge from runnable files on disk |
| `CHK-09` | `course-checks` | `course inline -check` passes clean blocks and catches 1-byte trailing whitespace drift | Run `-check` on clean file, then append 1 trailing space to one line inside the fence | Whitespace drift in inlined code would go undetected |
| `CHK-10` | `course-checks` | `course inline` handles Go files containing triple backticks and catches 1-byte drift | Include a Go file with ```` ``` ```` inside a raw string; rewrite with `inline`, verify `-check`, then drift 1 byte | Files with raw-string backticks would corrupt markdown fences or break drift detection |
| `CHK-11` | `course-checks` | `course check sources` catches a 1-byte edit in a copied SQLite C file | Mock pinned tree with real `manifest.uuid`; verify clean pass, then change 1 byte in chapter copy | Copied C source files could silently diverge from the pinned SQLite tree |
| `CHK-12` | `course-checks` | `course check sources` catches tampered `SOURCES.sha256` and stray chapter-root files | Flip 1 hex char in `SOURCES.sha256`, and drop a stray `.go` file at chapter root | Manifest tampering or stray files in chapter roots would pass checks |
| `CHK-13` | `course-checks` | `course check spans` catches shifted spans, past-EOF ends, and identifier prefix collisions | Shift `unixLock` down 1 line, up 12 lines, set `end` > EOF, and point at `unixLockHelper` | Stale line citations in chapters would pass verification |
| `TRC-01` | `trace-parser` | `ParseStraceText` joins interleaved `<unfinished ...>` / `<... resumed>` calls (strace 6.8 two-space format) | Parse interleaved multi-PID `pwrite64` and `read` unfinished/resumed pairs | Multi-threaded Go traces would drop or corrupt syscall arguments/attribution |
| `TRC-02` | `trace-parser` | `ParseStraceText` joins `<unfinished ...>` when only a single space follows a comma | Parse `read(3</db/a.db>, <unfinished ...>` + `<... read resumed>"hello", 10) = 5` | Comma doubling shifts parsed arguments, losing `Data` and `Length` |
| `TRC-03` | `trace-parser` | Interrupted syscalls (`EINTR` and strace 6.8 `? ERESTARTSYS`) are not counted as succeeded | Parse `= -1 EINTR` and `= ? ERESTARTSYS (...)` followed by restarted `fsync = 0` | Interrupted syscalls (e.g., from Go `SIGURG` preemption) get double-counted as succeeded writes/syncs |
| `TRC-04` | `trace-parser` | Long paths (>600 bytes, exceeding `-s 512`) are not truncated or mis-attributed | Live `trace.Run` on a >600-byte nested path doing open, pwrite64, fsync, close | Deep working directories would silently disappear from `UnderDir` filters |
| `TRC-05` | `trace-parser` | Single-PID FD reuse after `close` updates path attribution (without `-y`) | Open fd 5 as `/db/a.db`, write, close, reopen fd 5 as `/tmp/other.log`, write and fsync | Writes to unrelated files reusing a closed FD would be mis-attributed to the database |
| `TRC-06` | `trace-parser` | Cross-thread FD close + reuse does not leave stale path attribution (without `-y`) | TID 100 opens fd 5 (`/db/a.db`); TID 101 closes and reopens fd 5 (`/tmp/other.log`); TID 100 writes fd 5 | In multi-threaded Go programs without `-y`, a thread closing an FD does not clear other threads' cached paths |
| `TRC-07` | `trace-parser` | `UnderDir` excludes non-database files, pipes, `/dev/null`, and sibling prefix dirs | Filter writes to pipe, `/dev/null`, `/tmp/other.log`, `/work/db2/sneaky.db`, and `../db2/escape.log` | Non-database I/O would pollute database syscall counts |
| `TRC-08` | `trace-parser` | Live strace 6.8 `fd</path>(deleted)` suffix on unlinked-while-open files is parsed | Child opens file, unlinks it, then pwrites, fsyncs, and closes under live `trace.Run` | `fdWithPath` fails to match `(deleted)`, setting `FD=-1, Path=""` and dropping all I/O on unlinked-open files |
| `TRC-09` | `trace-parser` | `MergeStdoutVFS` handles `"trace.x"` split across two stdout `write` calls | Feed `write(1, "trace.", 6)` + `write(1, "xSync(...)", 20)` + `fsync` + `write(1, " -> 0\n", 6)` | Fragmented stdout writes silently drop VFS events and fail to enclose their syscalls |
| `KILL-01` | `kill-at-syscall` | `KillAt` with `K` beyond the process's syscall count lets the process finish cleanly | Count baseline syncs `N`, run with `When: N+50` | Over-range kill index would falsely kill or fail the child |
| `KILL-02` | `kill-at-syscall` | `KillAt` with `K=1` on `fsync,fdatasync` kills on entry to the very first sync | Run writer child with `When: 1`; inspect killed signal, unfinished sync, acks, and DB integrity | Kill injection missed the first sync or corrupted the database |
| `KILL-03` | `kill-at-syscall` | `KillAt` with `K=1` on `pwrite64` kills on entry to the first page/header write | Run writer child with `When: 1` on `pwrite64`; inspect trace and DB integrity | Kill injection did not intercept the first `pwrite64` |
| `KILL-04` | `kill-at-syscall` | `KillAt` is deterministic across 5 runs at each of `K=1, 2, 3` | Run 5 fresh trials at `K=1`, `K=2`, `K=3` (15 runs) and compare full outcome signatures | Crash experiments in chapters 4 and 9 would be flaky/timing-dependent |
| `KILL-05` | `kill-at-syscall` | `trace.Run` rejects invalid `KillAt` (`When=0` or empty syscalls) | Call `trace.Run` with invalid `KillAt` configs | Invalid kill specs would pass malformed flags to `strace` |
| `SET-01` | `sqlitex-settings` | All 6 per-connection settings apply on every pooled WAL connection under concurrency | Barrier-synchronize 50 goroutines calling `db.Conn(ctx)` simultaneously, hold all 50 open, read back pragmas | Pooled connections would silently revert to SQLite defaults under concurrent pool growth |
| `SET-02` | `sqlitex-settings` | Non-persistent rollback `journal_mode=truncate` + 6 pragmas apply on 50 concurrent pooled conns | Same 50-connection barrier test with `JournalMode=truncate` | Pooled rollback-mode connections would silently fall back to `DELETE` journal mode |
| `SET-03` | `sqlitex-settings` | Opening a new pooled connection in WAL mode (`BusyTimeout=0`) succeeds during an active write tx | Hold an uncommitted `BEGIN IMMEDIATE` + `INSERT` on Conn 1; open Conn 2 from pool and `SELECT` | `configureConnection`'s `PRAGMA journal_mode=wal` would block new WAL readers with `SQLITE_BUSY` |
| `SET-04` | `sqlitex-settings` | `ConnSettings.Validate()` and `Open()` agree on sub-millisecond `BusyTimeout` (`500us`) | Call `Validate()` then `Open()` with `BusyTimeout: 500 * time.Microsecond` | `Validate()` accepts a duration that `Open()` truncates to 0ms and then rejects during read-back verification |
| `SET-05` | `sqlitex-settings` | `ReadOnly` pool applies all 6 per-connection settings across 20 concurrent conns and refuses writes | Barrier-open 20 `ReadOnly: true` connections, read back all 6 pragmas, and attempt `INSERT` | Read-only connections would skip per-connection pragmas or allow writes |
| `SET-06` | `sqlitex-settings` | `ImmediateTx=true` acquires write lock at `BeginTx` on pooled connections | Open 2 pooled connections with `BusyTimeout=0`; Conn 1 calls `BeginTx`, Conn 2 calls `BeginTx` | `_txlock=immediate` was not applied on pooled connections |
