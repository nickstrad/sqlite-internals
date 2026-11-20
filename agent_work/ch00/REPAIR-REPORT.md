# Chunk 0 repair report (round 1)

Inputs: `agent_work/ch00/REVIEW-1.md` (verdict FAIL, 14 findings),
`agent_work/ch00/adversarial/results-run1.txt` (8 BROKEN). Repair base: `53e4545`.
Every finding and every BROKEN attack showed a real defect; none was answered without a code change.

## Findings and attacks

| id | outcome | regression test |
| --- | --- | --- |
| R1 blocker: `OpenScript` single-quote doubling | fixed `0df6d7d`: the path is double-quoted, with `\`, `"`, LF and CR escaped (`shell.c.in:8985-8998` + `resolve_backslashes` at `:4778`) | `TestOpenScriptQuoting`, `TestCLIExecQuotesAwkwardPaths` (live CLI; paths with `'`, space, `\`, `"`, newline) |
| R2 blocker: `Open(":memory:")` creates a file | fixed `0df6d7d`: new `MemoryPath`, `MemoryOptions()`, `OpenMemory(ctx)`; `Open(":memory:")` uses `file::memory:` and requires SingleConnection and journal_mode memory/off; `FileURI` rejects `""` and `:memory:` | `TestMemoryPathNeverCreatesAFile` |
| R3 blocker: existing WAL db silently converted | fixed `0df6d7d`: an existing database is never converted into or out of WAL (error `journal_mode want X got Y`); switching between rollback modes still works. The concurrent-creation race this exposed was fixed in `262dac2` (see below) | `TestExistingWALDatabaseIsNotConverted`, `TestExistingRollbackDatabaseIsNotConvertedToWAL` |
| R4 blocker: `<unfinished ...>` after a comma doubles the comma | fixed `6af6d41` (`joinSplitArgs`) | `TestUnfinishedSplitAfterComma` |
| R5 blocker: greedy `vfsCallLine` swallows `) -> 9999 (0x270f` | fixed `6af6d41`: args end at the first `) -> `, else at the trailing `)` | `TestParseVFSLineResultWithParentheses` |
| R6 should-fix: missing `SOURCES.sha256` passes | fixed `191cdb3` | `TestMissingSourcesFileIsAProblem` |
| R7 should-fix: `check all` from a subdirectory | fixed `191cdb3`: `InlineFile` gets the full path, label stays relative | `cmd/course` `TestInlineCheckFromAnotherDirectory` |
| R8 should-fix: live strace tests skip on any error | fixed `6af6d41`: new `trace.ErrPtraceDenied` (wrapped by `Run`); tests skip only on it, fail otherwise | covered by the three live tests (all PASS here, none skipped) |
| R9 should-fix: `-runs 0` passes | fixed `191cdb3`: `CheckEvidence` returns `ERROR`; `check evidence`/`check all` exit 2 | `TestCheckEvidenceRejectsNonPositiveRuns`, `TestRunsMustBePositive` |
| R10 should-fix: sub-ms BusyTimeout | fixed `0df6d7d`: `ConnSettings.Validate` rejects non-whole milliseconds | `TestSubMillisecondBusyTimeoutRejected` |
| R11 should-fix: writev/readv Length -1 | fixed `6af6d41`: Length = sum of `iov_len` (quoted data ignored when matching) | `TestWritevLength` |
| R12 should-fix: merge with failed writes / split `trace.x` prefix | fixed `6af6d41`: failed stdout writes skipped, partial writes cut to Result bytes, a line is classified once 7 bytes or a newline are known | `TestMergeSplitPrefixAndFailedWrites` |
| R13 nit (a) map order in `readConnSettings` | fixed `0df6d7d` (slice) | existing settings tests |
| R13 nit (b) `pathForFD` map fallback | fixed `6af6d41`: one shared fd table, deterministic (same change as TRC-06) | `TestFDReuseAcrossThreadsWithoutAnnotations` |
| R13 nit (c) ctx ignored | partly fixed `0df6d7d`: `Backup` checks `ctx.Err()` before each step. `withRawConn` still cannot use ctx because `sql.Conn.Raw` takes none | `TestBackupStopsOnCancelledContext` |
| R13 nit (d) no ReadOnly test, ignored test errors | fixed `0df6d7d` | `TestReadOnlyOpen`; ignored errors now checked |
| R14 nit: include markers inside other fences | fixed `191cdb3`: backtick and tilde fences that are not include blocks are skipped | `TestIncludeMarkersInsideFencesAreIgnored` |
| PIN-02 driver v1.58.0 passes | fixed `0df6d7d`: `CheckDriverModules()` compares the linked `modernc.org/sqlite` and `modernc.org/libc` versions (build info; replace directives use the replacement version) with sqlite/PIN; called by `CheckPin`, `WritePinCheck`, hence `MustPinCheck*` | `TestDriverModuleVersionCheck` (attack PIN-02 now HELD) |
| CHK-07 `//note:` prose passes | fixed `191cdb3`: `IsDirective` accepts only known `//go:` verbs, `//nolint:<list>`, `//line`, `//export`, `//extern`, `// +build` | `TestIsDirective` cases |
| TRC-02 | fixed `6af6d41` (same as R4) | `TestUnfinishedSplitAfterComma` |
| TRC-03 `? ERESTARTSYS` counted as success | fixed `6af6d41`: Errno=ERESTARTSYS, Result=-1, not Unfinished | `TestRestartedSyscallIsNotASuccess` |
| TRC-06 stale fd after cross-thread reuse | fixed `6af6d41` | `TestFDReuseAcrossThreadsWithoutAnnotations` |
| TRC-08 `(deleted)` paths dropped | fixed `6af6d41` | `TestDeletedPathAnnotation` |
| TRC-09 split `trace.` prefix | fixed `6af6d41` (same as R12) | `TestMergeSplitPrefixAndFailedWrites` |
| SET-04 | fixed `0df6d7d` (same as R10) | `TestSubMillisecondBusyTimeoutRejected` |
| Neon's suspected defect 1 (`neon-prose-notes.md`): flaky SQLITE_BUSY in `TestConcurrentOpenOfNewDatabase` | fixed `262dac2`. My R3 fix first made it worse (300-run stress: `journal_mode want wal got delete`, then `application_id want … got 0`). Now: layout pragmas, then journal_mode, then application_id/user_version; a still-unstamped database (application_id 0 and schema_version 0) is stamped by whichever connection sees it; the file-settings step is retried on SQLITE_BUSY until the busy timeout | `TestConcurrentOpenOfNewDatabase` 1000/1000 and `-race -count=3`; `TestUnstampedEmptyDatabaseIsStampedButUsedOneIsNot` |

Every new test except the R8 change was confirmed to fail on the pre-repair code (detached worktree at `53e4545` with the new test files copied in).

## Stale prose for Neon

No chapter `.md` exists yet. I deleted two comments whose code changed: the `configureConnection`
comment "Run journal_mode on every connection: WAL persists …", and the `IsDirective` doc
comment (`internal/repocheck/comments.go`). The `pathForFD` comment ("falls back across sibling
threads' tables") went too. New code has no comments. The following comments (line numbers at
HEAD) and notes now describe less than the code does, or describe it wrongly:

- `internal/sqlitex/settings.go:1` package doc: there is now in-memory support (`OpenMemory`) and a driver-module pin check.
- `internal/sqlitex/open.go:54` `FileURI`: rejects `""` and `:memory:`.
- `internal/sqlitex/open.go:69` `DSN`: `:memory:` becomes `file::memory:[?_txlock=immediate]`; ReadOnly with `:memory:` is an error.
- `internal/sqlitex/open.go:91` `Open`: never converts an existing database into or out of WAL; `:memory:` needs SingleConnection and journal memory/off; on a new file the order is page_size/auto_vacuum/encoding → journal_mode → application_id/user_version; an unstamped empty database is stamped; SQLITE_BUSY during this step is retried until BusyTimeout.
- New, undocumented exports: `MemoryPath`, `MemoryOptions`, `OpenMemory`, `DriverModuleMismatchError`, `CheckDriverModules`, `trace.ErrPtraceDenied`.
- `internal/sqlitex/settings.go:163` `ConnSettings.Validate`: also rejects BusyTimeout that is not a whole number of milliseconds.
- `internal/sqlitex/pin.go:14` `PinMismatchError` / `:34` `CheckPin` / `:121` `WritePinCheck` / `:145` `MustPinCheckMemory`: they now also fail with `*DriverModuleMismatchError` when the linked modernc.org/sqlite or modernc.org/libc version differs from sqlite/PIN. The check is skipped when the binary records no dependency list (`go test` binaries record none; `go run`/`go build` binaries do).
- `internal/sqlitex/cli.go:162` `OpenScript`: the path is now double-quoted with backslash escapes (`.open --normal "<path>"`).
- `internal/sqlitex/raw.go:130` `Backup`: stops with `ctx.Err()` before each step.
- `internal/trace/strace.go:26` `Syscall`: writev/readv now have Length (sum of iov_len); `? ERRNO` results give Errno and Result -1, and are not Unfinished.
- `internal/trace/strace.go:141` `ParseStrace`: without `-y`, one fd table shared by all traced pids/tids (last open/close wins); `fd</path>(deleted)` is accepted.
- `internal/trace/merge.go:75` `MergeStdoutVFS`: failed stdout writes are ignored, partial writes count only Result bytes, and a line becomes a VFS event once its first 7 bytes (`trace.x`) are known, even across writes.
- `internal/trace/linuxonly.go:53` / `strace_linux.go` `Run`: the ptrace failure is now `ErrPtraceDenied` (same message text).
- `internal/repocheck/sources.go:62` `CheckSources`: a missing `SOURCES.sha256` is a problem.
- `internal/repocheck/evidence.go:94` `CheckEvidence`: runs < 1 → status ERROR.
- `internal/repocheck/inline.go:112` `FindIncludeBlocks` / `:142` `ProcessInline`: markers inside other ``` or ~~~ fences are ignored.
- `agent_work/ch00/build-notes.md` lines 27-29, 188, 249 (`.open --normal '<path>'`), line 117 ("journal_mode on every connection"), line 141 ("throwaway `:memory:` connection" is still true for the pin check, which uses a plain driver connection), line 86 (the `debug.ReadBuildInfo` comparison in `TestPinMatchesGoMod` never runs under `go test`, because test binaries record no deps), lines 201-208, 258, 286, 289-293, 311: see the addendum appended to build-notes.md.
- `agent_work/ch00/neon-prose-notes.md` "Suspected defects" 1: fixed (`262dac2`).

## Commands run (final state, HEAD after `73d2572`)

- `gofmt -l cmd internal sqlite agent_work/ch00`: nothing. (`gofmt -l .` also lists 5 files under `agent_work/probes/`, untouched since before this chunk.)
- `go build ./... && go vet ./... && go test -count=1 ./...`: PASS (cmd/course, repocheck, sqlitex, trace, sqlite).
- `go test -race -count=3 ./internal/sqlitex`, `go test -race ./internal/...`: PASS.
- `go test -run TestConcurrentOpenOfNewDatabase -count=1000 ./internal/sqlitex`: PASS (slowest run 0.40 s in a 200-run sample).
- `bash agent_work/verify/crossbuild.sh`: `CROSSBUILD OK`.
- `go run ./cmd/course check all`: sources/spans/evidence PASS (no chapters or experiments yet), gofmt PASS, **paths FAIL**: 9 lines in `OUTLINE.md` (53, 1008, 1019, 1020, 1082) and `docs/knowledge/` mention `/root/...`. All of them predate this chunk and are in files I may not edit.
- `go run ./cmd/course check prose 53e4545..HEAD`: PASS (0 prose lines introduced). `check comments 53e4545 HEAD` fails as expected, because this repair changes code.
- `go run ./agent_work/ch00/smoke` twice: exit 0 both times, `SMOKE OK`. EXACT output identical between the runs. It differs from the old `smoke.txt` by one line, `close smoke.db-shm = 0` (34 file syscalls, up from 33): the -shm file is unlinked before its fd is closed, and strace prints `close(6</…/smoke.db-shm>(deleted))`, which the old parser dropped (TRC-08). I confirmed this with a direct strace of the child. `smoke.txt` was updated in `73d2572`.
- `bash agent_work/ch00/adversarial/run-all.sh` (run twice): 38 HELD, 0 BROKEN. `results.txt` was updated; `results-run1.txt` is kept. None of the attacks was wrong.
