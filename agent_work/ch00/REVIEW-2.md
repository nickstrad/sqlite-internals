VERDICT: PASS

Re-reviewed commit `8a66cd7` (`git diff 53e4545..HEAD`) after Opus's repair round (`0df6d7d`, `6af6d41`, `191cdb3`, `262dac2`, `73d2572`, `8a66cd7`). Every finding from `REVIEW-1.md`, every formerly `BROKEN` attack from `agent_work/ch00/adversarial/RESULTS.md`, and the concurrent-creation race from `neon-prose-notes.md` is fixed and verified.

## Verification of findings and formerly BROKEN attacks

| Finding / Attack | Fixed? | How confirmed |
| --- | --- | --- |
| R1 blocker: `OpenScript` single-quote doubling (`internal/sqlitex/cli.go:167-175`) | Yes | Verified `dotCommandQuote` double-quotes and escapes `\`, `"`, `\n`, `\r` against `src/shell.c.in:4778-4805, 8985-8998`; `TestOpenScriptQuoting` and live `TestCLIExecQuotesAwkwardPaths` pass for `'`, space, `\`, `"`, and LF. |
| R2 blocker: `Open(":memory:")` creates file on disk (`internal/sqlitex/open.go:56-166`) | Yes | Read diff: `FileURI` rejects `""` and `:memory:`; `DSN` emits `file::memory:` and rejects `ReadOnly`; `Open` requires `SingleConnection` and `memory`/`off` journal mode; exported `MemoryPath`, `MemoryOptions`, `OpenMemory`. `TestMemoryPathNeverCreatesAFile` passes and confirms empty temp dir. |
| R3 blocker: existing WAL db silently converted (`internal/sqlitex/open.go:219-269`) | Yes | Read diff: when `pageCount > 0`, `applyFileSettings` checks `PRAGMA journal_mode` first and returns a mismatch error if `currentIsWAL != wantWAL`, before executing `file.journalPragma()`. `TestExistingWALDatabaseIsNotConverted` and `TestExistingRollbackDatabaseIsNotConvertedToWAL` pass and verify header bytes 18–19 are untouched. |
| R4 blocker / TRC-02: `<unfinished ...>` after comma doubles comma (`internal/trace/strace.go:200, 228-238`) | Yes | Read `joinSplitArgs` (trims `, ` from both halves before rejoining); `TestUnfinishedSplitAfterComma` and adversarial `TRC-01`/`TRC-02` pass (`HELD`). |
| R5 blocker: greedy `vfsCallLine` swallows `) -> 9999 (0x270f` (`internal/trace/vfs.go:34, 65-79, 118-126`) | Yes | Read `splitVFSResult` (splits on first `) -> `, else trims trailing `)`); `TestParseVFSLineResultWithParentheses` passes for `%d (0x%x)`, `out="/a)b -> (c)"`, void calls, and unterminated calls. |
| R6 should-fix: missing `SOURCES.sha256` passes (`internal/repocheck/sources.go:70-72`) | Yes | Read diff: `os.IsNotExist(err)` appends a `missing SOURCES.sha256` problem. `TestMissingSourcesFileIsAProblem` passes for both missing (problem) and empty (0 problems). |
| R7 should-fix: `check all` inline from subdirectory (`cmd/course/checks.go:61-67, 177`) | Yes | Read diff: `inlineOne` takes full `name` and relative `display`, and `check all` passes `chapter.Markdown()`. `TestInlineCheckFromAnotherDirectory` passes after `t.Chdir`. |
| R8 should-fix: live strace tests skip on any error (`internal/trace/linuxonly.go:14`, `strace_linux.go:102`, `strace_linux_test.go:73, 108, 142, 159-165`) | Yes | Read diff: `ErrPtraceDenied` wrapped on ptrace permission failures; `skipIfPtraceDenied` skips only on `errors.Is(err, ErrPtraceDenied)` and calls `t.Fatal` otherwise. All three live strace tests pass here without skipping. |
| R9 should-fix: `-runs 0` passes (`internal/repocheck/evidence.go:98-101`, `cmd/course/checks.go:138-140, 166-168`) | Yes | Read diff: `CheckEvidence` returns `Status: "ERROR"` for `runs < 1`; `cmd/course` exits 2 via `usageError`. `TestCheckEvidenceRejectsNonPositiveRuns` and `TestRunsMustBePositive` pass. |
| R10 should-fix / SET-04: sub-ms `BusyTimeout` (`internal/sqlitex/settings.go:171-173`) | Yes | Read diff: `ConnSettings.Validate` rejects `BusyTimeout%time.Millisecond != 0`. `TestSubMillisecondBusyTimeoutRejected` and adversarial `SET-04` pass (`HELD`). |
| R11 should-fix: `writev`/`readv` `Length == -1` (`internal/trace/strace.go:120-121, 394-400`) | Yes | Read diff: strips quoted strings before summing `iov_len=(\d+)`. `TestWritevLength` passes (`Length=23`, ignores `iov_len=99` inside `iov_base`). |
| R12 should-fix / TRC-09: failed stdout writes and split `trace.x` prefix (`internal/trace/merge.go:65-141`) | Yes | Read diff: skips `!call.Succeeded()`, slices short writes to `call.Result`, defers `lineUndecided` classification until `len >= len("trace.x")` or `\n`. `TestMergeSplitPrefixAndFailedWrites` and adversarial `TRC-09` pass (`HELD`). |
| R13 nit (a–d): map order, fd fallback, `Backup` ctx, `ReadOnly` test and checked test errors | Yes | Verified `readConnSettings` uses a slice (`open.go:319-336`), `fdPaths` is one shared table (`strace.go:138, 295-305`), `Backup` checks `ctx.Err()` before each step (`raw.go:151-154`, `TestBackupStopsOnCancelledContext`), and `TestReadOnlyOpen` plus error checks were added in `sqlitex_test.go`. |
| R14 nit: include markers inside other fences (`internal/repocheck/inline.go:16-30, 119-122, 151-155`) | Yes | Read `outerFenceEnd` (skips matching backtick/tilde fences); `TestIncludeMarkersInsideFencesAreIgnored` passes for 4-backtick and tilde fences. |
| PIN-02: driver v1.58.0 passes pin check (`internal/sqlitex/pin.go:41-99, 130-133`) | Yes | Read `CheckDriverModules` / `checkModuleVersions` / `moduleVersion`; called by `CheckPin` and `WritePinCheck` (and hence `MustPinCheck*`). `TestDriverModuleVersionCheck` and live adversarial `PIN-02` pass (`HELD`). |
| CHK-07: `//note:` prose treated as directive (`internal/repocheck/comments.go:13-44`) | Yes | Read `IsDirective`: allows only known `//go:` verbs, `//nolint:<list>`, `//line`, `//export`, `//extern`, `// +build`. `TestIsDirective` and adversarial `CHK-07` pass (`HELD`). |
| TRC-03: `? ERESTARTSYS` counted as success (`internal/trace/strace.go:119, 262-266`) | Yes | Read diff: sets `Result = -1`, `Errno = m[1]`, `Unfinished = false`. `TestRestartedSyscallIsNotASuccess` and adversarial `TRC-03` pass (`HELD`). |
| TRC-06: cross-thread fd reuse stale path (`internal/trace/strace.go:138, 286-305`) | Yes | Read diff: single shared `fdPaths map[int]string` updated on `open`/`openat`/`close`. `TestFDReuseAcrossThreadsWithoutAnnotations` and adversarial `TRC-05`/`TRC-06` pass (`HELD`). |
| TRC-08: `(deleted)` paths dropped (`internal/trace/strace.go:118`) | Yes | Read diff: `fdWithPath` accepts optional `(deleted)` suffix. `TestDeletedPathAnnotation`, adversarial `TRC-08` (`HELD`), and live smoke run (now showing `close smoke.db-shm = 0`, 34 syscalls) pass. |
| Suspected defect 1 (`neon-prose-notes.md`): concurrent creation `SQLITE_BUSY` (`internal/sqlitex/open.go:188-204, 219-269`) | Yes | Read diff: splits layout vs identity pragmas, stamps unstamped empty DB (`application_id == 0 && schema_version == 0`), retries `applyFileSettings` on `SQLITE_BUSY` until `BusyTimeout`. Verified `TestConcurrentOpenOfNewDatabase` 200/200, `TestUnstampedEmptyDatabaseIsStampedButUsedOneIsNot`, and `go test -race ./...`. |

## New findings

No blockers or should-fix defects were introduced by the repair. Three minor non-blocking edge-case notes for future reference:

1. **nit** — `internal/trace/strace.go:119` (`interrupted`)
   - `^\?\s+(E[A-Z0-9]+)` omits `_`, so if a traced syscall ever printed `? ERESTART_RESTARTBLOCK (...)` (typical of `nanosleep`/`poll`/`futex`, not SQLite's file syscalls), `Errno` would be truncated to `"ERESTART"` rather than `"ERESTART_RESTARTBLOCK"` (though `Result` is still `-1` and `Succeeded()` is still `false`).
2. **nit** — `internal/sqlitex/pin.go:93-95` (`moduleVersion`)
   - For a `replace` directive, `moduleVersion` checks `dep.Replace.Version` without checking `dep.Replace.Path == path`. Local path replacements have an empty `Version` and are caught (`TestDriverModuleVersionCheck`), but a remote fork replacement carrying the exact same version string `v1.60.1` would not be flagged.
3. **nit** — `internal/sqlitex/open.go:259` (`applyFileSettings`)
   - `unstamped = applicationID == 0 && schemaVersion == 0` assumes a stamped database has a non-zero `application_id` or at least one schema change. If a caller explicitly set `ApplicationID = 0`, created no tables, and bumped `user_version`, opening a second connection would reset `user_version` back to `options.File.UserVersion`. Course databases always use non-zero `CourseApplicationID`, so this does not affect course code.

## Commands run

- `export SQLITE_SRC=/root/Software/sqlite-src/3.53.4 && go build ./... && go vet ./... && go test -v -count=1 ./...` — PASS (`cmd/course`, `internal/repocheck`, `internal/sqlitex`, `internal/trace`, `sqlite`; all live strace tests ran and passed without skipping).
- `go test -race -count=1 ./...` — PASS.
- `go test -run TestConcurrentOpenOfNewDatabase -count=200 ./internal/sqlitex` — PASS (200/200 in 28.5s).
- `bash agent_work/verify/crossbuild.sh` — PASS (`CROSSBUILD OK`: linux/amd64, darwin/arm64, darwin/amd64, `check gofmt` 38 files).
- `go run ./agent_work/ch00/smoke` — PASS (`SMOKE OK`), byte-for-byte identical to `agent_work/ch00/smoke.txt` excluding `INFO:` lines.
- Adversarial attack groups (`pin-check`, `course-checks`, `trace-parser`, `kill-at-syscall`, `sqlitex-settings`) — 38 HELD, 0 BROKEN.
- `go run ./cmd/course check sources / spans / gofmt / prose 53e4545..HEAD` — PASS (`0` prose lines introduced in `53e4545..HEAD`).
