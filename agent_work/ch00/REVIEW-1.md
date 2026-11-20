VERDICT: FAIL

Reviewed commit `eab2401` (`git diff a7bb640..HEAD`, branch `chunk/ch00`) against `OUTLINE.md`, `/root/briefs/sqlite-p2/common.md`, `/root/briefs/sqlite-p2/chunk00-build.md`, `agent_work/ch00/build-notes.md`, and the pinned SQLite source `/root/Software/sqlite-src/3.53.4`. Line numbers below refer to commit `eab2401`.

## Findings

1. **blocker** — `internal/sqlitex/cli.go:140-142` (`OpenScript`)
   - **What is wrong:** `OpenScript` emits `.open --normal '` + `strings.ReplaceAll(dbPath, "'", "''")` + `'`. In SQLite 3.53.4's dot-command argument parser (`src/shell.c.in:8985-8998`, `parseDotCmdArgs`), single-quoted strings do **not** support SQL `''` quote doubling: the first `'` terminates the argument and the remainder is parsed as a second argument, breaking `CLI.Exec`, `trace.RunCLIVFSTrace`, and `trace.RunCLIMerged` for any path containing `'`.
   - **How confirmed:** Read `src/shell.c.in:8985-8998` (`while( z[h] && z[h]!=delim )` only checks `\\` when `delim=='"'`, then calls `resolve_backslashes` only for `delim=='"'`). Tested directly with the built CLI and in a sandboxed `git archive HEAD` test of `cli.Exec`:
     ```text
     $ printf ".open --normal '/tmp/opencode/ch00-review/a''b/test.db'\nCREATE TABLE t(x); SELECT 1;\n" | ./_tools/bin/sqlite3 -batch -bail -init /dev/null
     extra argument: "b/test.db"
     exit=1
     ---
     cli.Exec with single-quote path: out="" err=/usr/local/bin/sqlite3 : exit status 1: extra argument: "brien/t.db"
     ```
   - **Expected fix:** Quote `dbPath` with double quotes in `OpenScript`, escaping `\` as `\\` and `"` as `\"` (and reject paths containing `\n`/`\r`, or return an error). Add a unit test exercising a directory name with `'`, spaces, and `\` through `cli.Exec`.

2. **blocker** — `internal/sqlitex/open.go:47-54` (`FileURI`) and `internal/sqlitex/open.go:67-105` (`Open`)
   - **What is wrong:** `Open(ctx, ":memory:", opts)` passes `":memory:"` into `FileURI`, which calls `filepath.Abs(":memory:")` and silently creates a real database file literally named `:memory:` in the current working directory. `OUTLINE.md` §5 Experiment 1.1 explicitly specifies: "opens an in-memory database through `internal/sqlitex`", while `openMemory` (`raw.go:230`) is unexported.
   - **How confirmed:** Ran `OpenDefault(ctx, ":memory:")` inside a temp directory in a sandboxed `git archive HEAD` test:
     ```text
     $ go test -v -run TestReviewMemoryPathCreatesFile ./internal/sqlitex
     review_probe_test.go:94: CREATED LITERAL FILE /tmp/TestReviewMemoryPathCreatesFile3896714062/001/:memory: on disk!
     ```
   - **Expected fix:** Either handle `":memory:"` explicitly in `FileURI`/`DSN` (emitting `file::memory:` with query params, skipping `filepath.Abs`, and handling the fact that `PRAGMA journal_mode=wal` returns `memory` on an in-memory database) or export `OpenMemory` and reject `":memory:"` in `Open`/`FileURI` so it can never silently create a file on disk.

3. **blocker** — `internal/sqlitex/open.go:142-148, 289-291` (`configureConnection`, `compareFileSettings`)
   - **What is wrong:** When opening an **existing** database (`pageCount > 0`), `configureConnection` unconditionally executes `PRAGMA journal_mode=<options.File.JournalMode>`. Opening an existing WAL database with `JournalMode = JournalDelete` silently checkpoints and mutates the file header bytes 18–19 from `(2, 2)` to `(1, 1)` on disk instead of failing with a mismatch error (unlike `page_size`, `auto_vacuum`, `encoding`, and `application_id`). This also makes `compareFileSettings`'s `JournalMode` check (`open.go:289-291`) dead code because `configureConnection` has already overwritten it and checked the return value at line 146.
   - **How confirmed:** Sandboxed `git archive HEAD` test created a WAL DB (`DefaultOptions`), closed it, then opened it with `JournalMode = JournalDelete`:
     ```text
     $ go test -v -run TestReviewJournalModeMutation ./internal/sqlitex
     review_probe_test.go:55: before: header[18]=2 header[19]=2
     review_probe_test.go:66: MUTATED existing WAL db to DELETE without error! after: header[18]=1 header[19]=1
     ```
   - **Expected fix:** When `pageCount > 0`, query the current `PRAGMA journal_mode` first; if the existing DB is in WAL mode and the caller asked for a non-WAL mode (or vice versa), refuse with an existing-database mismatch error instead of silently converting header bytes 18–19 on disk. Only issue `PRAGMA journal_mode=<mode>` on an existing DB when switching among per-connection rollback modes (both current and requested are non-WAL), or on creation (`pageCount == 0`).

4. **blocker** — `internal/trace/strace.go:179-185` (`straceParser.line`, `<... resumed>`)
   - **What is wrong:** When `strace` splits an `<unfinished ...>` line after a comma (`100 openat(AT_FDCWD</w>, "t.db", <unfinished ...>`), `strings.TrimSuffix(rest, " <unfinished ...>")` leaves `pending.args` ending with `,` (no trailing space). Line 181 checks `!strings.HasSuffix(joined, ", ")` — which is true — and appends a second `, `, turning the joined argument string into `AT_FDCWD</w>, "t.db",, O_RDWR|O_CREAT, 0644`. That inserts an empty argument and shifts every subsequent argument index by 1, dropping `call.Flags` (`Flags=""`).
   - **How confirmed:** Sandboxed `git archive HEAD` test of `ParseStraceText`:
     ```text
     $ go test -v -run TestReviewUnfinishedTrailingComma ./internal/trace
     review_probe_test.go:35: call 0: Path="/w/t.db" Flags="" String="openat /w/t.db  = fd"
     ```
   - **Expected fix:** Trim trailing spaces/commas cleanly when joining `pending.args` and `args` (e.g., if `strings.HasSuffix(strings.TrimRight(joined, " "), ",")`, append `" "` instead of `", "` before `strings.TrimLeft(args, ", ")`), and add a regression test covering mid-argument-list `<unfinished ...>` splits.

5. **blocker** — `internal/trace/vfs.go:32` (`vfsCallLine`)
   - **What is wrong:** `vfsCallLine` uses a greedy `(.*)` before `\)(?: -> (.*))?$`. Any `vfstrace` output line whose result ends with `)` — notably `vfstrace_print_errcode` at `ext/misc/vfstrace.c:345` (`"%d (0x%x)"`) — is misparsed: `(.*)` swallows `) -> 9999 (0x270f` into the argument list, leaving `Result == ""` and corrupting the last parsed argument (`Offset == -1`).
   - **How confirmed:** Verified `ext/misc/vfstrace.c:345` and ran `ParseVFSLine("trace.xRead(t.db,n=100,ofst=0) -> 9999 (0x270f)")` in sandbox:
     ```text
     $ go test -v -run TestReviewVFSUnknownErrcodeWithParens ./internal/trace
     review_probe_test.go:10: ok=true Method="xRead" File="t.db" Args=["t.db" "n=100" "ofst=0) -> 9999 (0x270f"] Amount=100 Offset=-1 Result="" Detail=""
     ```
   - **Expected fix:** Split on `") -> "` first (or match balanced parentheses for the call arguments rather than greedy `(.*)` with an optional suffix) so `)` inside the result/detail cannot be swallowed into `Args`. Add a unit test for `%d (0x%x)` and paths containing `)` in `out="..."`.

6. **should-fix** — `internal/repocheck/sources.go:62-65` (`CheckSources`)
   - **What is wrong:** `CheckSources` ignores `os.IsNotExist(err)` when reading `<chapter>/SOURCES.sha256`. A chapter directory that omits `SOURCES.sha256` completely (and has no stray files at its root) silently passes `check sources` with 0 problems, contradicting `OUTLINE.md` §1 ("Every chapter directory has `SOURCES.sha256`") and Acceptance Checklist #1.
   - **How confirmed:** Sandboxed `git archive HEAD` test:
     ```text
     $ go test -v -run TestReviewMissingSourcesFilePasses ./internal/repocheck
     review_probe_test.go:17: missing SOURCES.sha256: problems=[] count=0 err=<nil>
     ```
   - **Expected fix:** Record a `Problem{chapter.Dir + "/SOURCES.sha256", "missing SOURCES.sha256"}` when `os.IsNotExist(err)` (unlike optional `SPANS.txt`, `SOURCES.sha256` is required per chapter).

7. **should-fix** — `cmd/course/checks.go:163-167` (`runCheck`, `case "all"`)
   - **What is wrong:** `check all` verifies `os.Stat(chapter.Markdown())` using the absolute path, then passes `relative(root, chapter.Markdown())` into `inlineOne`, which calls `repocheck.InlineFile(name, true)` → `os.ReadFile(name)` relative to the current working directory rather than `root`. Running `go run ./cmd/course check all` from any subdirectory fails on every chapter markdown file even though `main.go:40` explicitly resolves `root` via `sqlitex.RepoRoot()`.
   - **How confirmed:** Sandboxed `git archive HEAD` test running `go run ../cmd/course check all -skip-evidence` from `sqlite/` with a dummy `ch99-tmp/ch99-tmp.md`:
     ```text
     PASS check sources ch99-tmp: 0 copied file(s) match the pin
     PASS check spans ch99-tmp: 0 span(s) start at their symbol
     FAIL inline -check ch99-tmp/ch99-tmp.md: open ch99-tmp/ch99-tmp.md: no such file or directory
     ```
   - **Expected fix:** Pass `chapter.Markdown()` (the full path) to `InlineFile`, using `relative(root, ...)` only for the display label.

8. **should-fix** — `internal/trace/strace_linux_test.go:69-71, 103-105, 135-137`
   - **What is wrong:** `TestRunGoProcessUnderStrace`, `TestKillAtFirstSync`, and `TestRunCLIMergedLive` all call `t.Skipf("strace unusable here: %v", err)` for **any** error returned by `Run` or `RunCLIMerged`. Any regression in `ParseStraceText`, `MergeStdoutVFS` (e.g., stdout truncation), or `straceArgs` silently downgrades a test failure to a skipped test.
   - **How confirmed:** Code inspection of lines 69–71, 103–105, and 135–137.
   - **Expected fix:** Return a typed/sentinel error (e.g. `ErrPtraceDenied`) from `Run` specifically when strace fails due to ptrace permissions, skip only on `errors.Is(err, ErrPtraceDenied)`, and `t.Fatal(err)` on all other errors.

9. **should-fix** — `internal/repocheck/evidence.go:105-130` & `cmd/course/checks.go:128, 152`
   - **What is wrong:** Neither `CheckEvidence` nor `cmd/course` validates `runs >= 1`. Passing `-runs 0` (or negative) skips the execution loop entirely and reports `PASS: 0 runs identical to the evidence` even on an experiment that exits non-zero.
   - **How confirmed:** Sandboxed `git archive HEAD` test of `CheckEvidence(ctx, root, failingExp, 0)`:
     ```text
     $ go test -v -run TestReviewCheckEvidenceZeroRuns ./internal/repocheck
     review_probe_test.go:28: CheckEvidence(runs=0) on failing experiment: status="PASS" detail="0 runs identical to the evidence" passed=true
     ```
   - **Expected fix:** Reject `runs < 1` in `CheckEvidence` (returning status `ERROR` or error) and in `cmd/course check evidence` / `check all` (returning `usageError`).

10. **should-fix** — `internal/sqlitex/open.go:267-273` (`compareConnSettings`)
    - **What is wrong:** `ConnSettings.pragmas()` truncates `BusyTimeout` with `.Milliseconds()`, and `readConnSettings` reads back integer milliseconds, while `compareConnSettings` compares structs with `want != got` using raw `time.Duration`. Any sub-millisecond `BusyTimeout` (e.g., `1500 * time.Microsecond`) fails `Open` with a contradictory error printing identical `want` and `SQLite reports` strings (`busy_timeout=1ms`).
    - **How confirmed:** Sandboxed `git archive HEAD` test:
      ```text
      $ go test -v -run TestReviewSubMillisecondBusyTimeout ./internal/sqlitex
      review_probe_test.go:75: sub-ms BusyTimeout failed: open .../subms.db: per-connection settings differ after applying them: want ... busy_timeout=1ms, SQLite reports ... busy_timeout=1ms
      ```
    - **Expected fix:** Either normalize `want.BusyTimeout = time.Duration(want.BusyTimeout.Milliseconds()) * time.Millisecond` in `compareConnSettings` or reject non-millisecond durations in `ConnSettings.Validate()`.

11. **should-fix** — `internal/trace/strace.go:361-372` (`fillArgs` for `writev`/`readv`)
    - **What is wrong:** `writev`/`readv` parsing builds `call.Data` from `iov_base` entries but never computes `call.Length` (sum of `iov_len`), leaving `Length == -1`. `Syscall.String()` (`strace.go:67-68`) formats `writev`/`readv` with `len=%d`, producing `writev pipe:[9] len=-1 = 27`.
    - **How confirmed:** Sandboxed `git archive HEAD` test:
      ```text
      $ go test -v -run TestReviewWritevLengthAndString ./internal/trace
      review_probe_test.go:21: writev Length=-1 Result=27 String="writev pipe:[9] len=-1 = 27"
      ```
    - **Expected fix:** Parse and sum `iov_len=<n>` values into `call.Length` for `writev`/`readv`.

12. **should-fix** — `internal/trace/merge.go:63, 70` (`MergeStdoutVFS`)
    - **What is wrong:** (a) Failed stdout writes (`write(1, ...) = -1 EPIPE`) or unfinished killed writes are not filtered with `call.Succeeded()`, so failed writes still emit `EventVFS` / `EventOutput`. (b) `lineStartsVFS = strings.HasPrefix(data, "trace.x")` is only evaluated on the first fragment; if a write splits inside `"trace.x"`, the line is neither emitted as `EventVFS` (because `lineStartsVFS` is false) nor as `EventOutput` (because at `\n` `strings.HasPrefix(text, "trace.")` is true), silently dropping the VFS event and leaving enclosed syscalls unenclosed.
    - **How confirmed:** Sandboxed `git archive HEAD` test:
      ```text
      $ go test -v -run TestReviewMergeEdgeCases ./internal/trace
      review_probe_test.go:55: event 0: kind=SYS enclosed=-1 str="SYS fsync /w/t.db-wal = 0"
      review_probe_test.go:55: event 1: kind=VFS enclosed=-1 str="VFS xClose(t.db) -> SQLITE_OK"
      ```
    - **Expected fix:** Skip stdout writes where `!call.Succeeded()` (or `call.Result <= 0`), and when buffering a line whose prefix is still a prefix of `"trace.x"`, defer classification until `line.Len() >= len("trace.x")` or `\n`.

13. **nit** — `internal/sqlitex/open.go:209-225` (`readConnSettings`), `internal/trace/strace.go:278-282` (`pathForFD`), `internal/sqlitex/raw.go:33` (`withRawConn`)
    - **What is wrong:** (a) `readConnSettings` iterates a Go `map[string]*int64`, randomizing PRAGMA query order on every open (unlike `ReadFileSettings` right below it, which uses a slice). (b) `pathForFD` falls back across PIDs by iterating `p.fdPaths` map in random order, making multi-process traces without `-y` nondeterministic. (c) `withRawConn` and all wrappers in `raw.go` accept `ctx context.Context` and ignore it (`Backup`'s loop never checks `ctx.Err()`). (d) `internal/sqlitex/sqlitex_test.go` has no test covering `Options.ReadOnly = true` (tested in sandbox and works, but untested in the repo), and ignores errors on `db.Conn` / `os.WriteFile` / `os.MkdirAll` in several tests (`lines 125-126, 225, 240, 253, 278, 306, 343, 361`).
    - **Expected fix:** Use a slice in `readConnSettings`, sort or avoid cross-process map fallback in `pathForFD`, check `ctx.Err()` inside `Backup`'s loop, and add a `ReadOnly` test to `sqlitex_test.go`.

14. **nit** — `internal/repocheck/inline.go:89-185` (`FindIncludeBlocks`, `ProcessInline`)
    - **What is wrong:** Neither function tracks enclosing Markdown code fences, so a literal `<!-- include: foo.go -->` shown inside a fenced ```` ```markdown ```` documentation block is treated as a real include directive.
    - **How confirmed:** Sandboxed `git archive HEAD` test:
      ```text
      review_probe_test.go:50: inline on fenced example: err=<nil> problems=[ch01-x.md:4: open .../missing.go: no such file or directory]
      ```
    - **Expected fix:** Track open non-include fence state while scanning lines so markers inside existing fenced code blocks are ignored.

## Verification of builder's outline corrections (`build-notes.md`)

- **#1 (CLI `-vfstrace` writes to stdout):** Confirmed in `src/shell.c.in:12911-12916` (`vfstraceOut` writes to `p->out` and flushes) and `:13330-13332` (`data.out = stdout`).
- **#2 (`fdatasync` vs `fsync` across C builds):** Confirmed live: `_tools/bin/sqlite3` calls `fdatasync`; `SQLITE3_BIN=/usr/local/bin/sqlite3` calls `fsync` (and emits 58 VFS calls including `xRandomness=1` vs 57 for `_tools/bin/sqlite3`).
- **#3 (`open` vs `openat`):** Confirmed in live smoke output (`open` for modernc, `openat` for C CLI).
- **#4 (CLI probe-open skipped by `.open --normal`):** Confirmed in `src/shell.c.in:4229-4320, 4493, 10222` (though `OpenScript`'s single-quote escaping is broken — Finding 1).
- **#5 (`-cmd` + `-bail` exits early):** Confirmed in `src/shell.c.in:13535-13536` (`if( bail_on_error ) goto shell_main_exit;` without checking `rc`).
- **#6 (strace SIGKILL exit code `-1`):** Confirmed by `TestKillAtFirstSync`.
- **#7 (`Deserialize` of WAL-format image requires header bytes 18–19 = 1):** Confirmed by `TestRawWrappers`.
- **#8–#11 (SeaweedFS volume headroom, listing order, modernc custom VFS viability, Debian lab image):** Consistent with `compose.yaml`, `Dockerfile.lab`, and probe `RESULT.md` files (not re-run against Docker/SeaweedFS in this review).

## Commands run

- `git log --oneline a7bb640..HEAD && git diff a7bb640..HEAD --stat` — 15 commits, 64 files changed.
- `export SQLITE_SRC=/root/Software/sqlite-src/3.53.4 && go build ./... && go vet ./... && go test -v ./...` — PASS (`sqlite`, `internal/sqlitex`, `internal/trace`, `internal/repocheck`).
- `GOOS=darwin GOARCH=arm64 go build ./... && GOOS=darwin GOARCH=arm64 go vet ./... && GOOS=darwin GOARCH=amd64 go build ./... && GOOS=darwin GOARCH=amd64 go vet ./...` — PASS (`DARWIN OK`).
- `go test -race ./...` — PASS (no races).
- `./agent_work/verify/crossbuild.sh` — PASS (`CROSSBUILD OK`).
- `go run ./cmd/course check sources / spans / gofmt / prose a7bb640..HEAD / comments HEAD HEAD` — PASS (`check prose`: 0 prose lines introduced in range).
- `go run ./cmd/course check paths` — FAIL on 9 pre-existing lines in `OUTLINE.md` and `docs/knowledge/` (none introduced by `ch00`).
- `go run ./agent_work/ch00/smoke` x2 — `SMOKE OK`; two consecutive runs identical (excluding `INFO:` lines) and match `agent_work/ch00/smoke.txt` byte for byte.
- `SQLITE3_BIN=/usr/local/bin/sqlite3 go run ./agent_work/ch00/smoke` — `SMOKE OK`; differs from stored `smoke.txt` only on `fsync` vs `fdatasync` and `xRandomness=1` (58 vs 57 VFS calls), matching `build-notes.md`.
- `printf ".open --normal '/tmp/opencode/ch00-review/a''b/test.db'\n..." | ./_tools/bin/sqlite3 -batch -bail -init /dev/null` — `extra argument: "b/test.db"`, exit 1 (confirmed Finding 1).
- Sandboxed `git archive HEAD` adversarial test suites (`go test -v -run TestReview ./internal/sqlitex ./internal/trace ./internal/repocheck`) — confirmed Findings 1–7, 9–12, 14.
