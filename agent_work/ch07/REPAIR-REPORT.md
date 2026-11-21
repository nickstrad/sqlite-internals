# Chapter 7 — repair round report (Opus)

Merged tree (main with chapters 1–2 + chunk/ch07 at 4ad93c3) was verified first: build, vet, tests and all experiments (2 runs each) passed before any change.

Commits: `1bfdfac` (rangefs), `6d1c483` (govfs), `dcdf3bd` (experiments + evidence), plus this report commit.

## Findings and attacks

| id | outcome | test added |
| --- | --- | --- |
| R1 [blocker] 7.1 `portable()` matches `delete=1` | fixed `dcdf3bd`: check extracted to `closeLifecycle`, matches the `delFlag=1` argument; check text now says `delFlag=1` | `7.1.../main_test.go` `TestCloseLifecycleMatchesRealVFSTraceLines`, `TestCloseLifecycleRejectsKeptShm` (fed real `-vfstrace` lines through `trace.ParseVFSLine`; fail on the old code) |
| R2.1 zero-length `ReadAt` logs a bogus range | fixed `1bfdfac`: `len(p)==0` returns `0, nil` with no request | `rangefs/edge_test.go` `TestZeroLengthReadAtLogsNothing` |
| R2.2 failed/short `ReadRange` counted as `want` bytes | fixed `1bfdfac`: failed or short reads log `Request{Failed: true, Length: n}` (bytes actually received); `String()` prints `range NAME at OFF failed (N bytes received)` | `TestFailedRangeCountsNoBytes`, `TestShortRangeCountsReceivedBytes` |
| R2.3 `Seek`/`Stat` after `Close`; nil ctx | fixed `1bfdfac`: both return `fs.ErrClosed`; `New(nil, …)` uses `context.Background()` | `TestSeekAndStatAfterClose`, `TestNilContextDefaults` |
| R3 7.3 fault checks can pass vacuously | fixed `dcdf3bd`: `trialHeld` requires `Fired` for n≤total and `!Fired` for n=total+1; rollback requires primary code 10, `closeErr==""`, `match.Exact`; commit requires `match.Exact`, `closeErr==""`; claims require `total>0` and `len(goCalls)>0` | `7.3.../main_test.go` `TestTrialHeld` |
| R4.1 split-lock race in fault recording; seq advanced when not recording | fixed `6d1c483`: `inject`+`injected` replaced by `injectFault(call)`, which matches, builds the full injected `Call`, appends it and sets `fired` under one `v.mu` hold; seq only advances when recording | `govfs/inject_test.go` `TestInjectedCallIsCompleteAtDisarm`, `TestInjectedCallWithoutRecording`, `TestDisarmRacingInjectSeesWholeCall` (run under `-race`) |
| R4.2 C memory leaks in `Register` / `methodTableFor` | fixed `6d1c483`: `Register` frees `cname` (`libc.Xfree`) and the `sqlite3_vfs` block (`sqlite3_free`) on every failure return; the losing `methodTableFor` goroutine frees its table | none: no allocator hook to observe frees without the `memgrind` build tag; covered by existing `TestRegisterValidation` path still passing |
| R5.1 WAL check passes with 0 checkpoints | fixed `dcdf3bd`: `classifyWALCommits` returns ok only if `checkpoints>0 && restarts==checkpoints` | `7.2.../main_test.go` `TestClassifyWALCommits` |
| R5.2 `counts.add` counts failed syscalls | fixed `dcdf3bd`: `!s.Succeeded()` skipped | `TestCountsSkipFailedSyscalls` |
| R6 dead code | fixed: `registry.checked`, `pointerSize`, 7.1 `stream`, `onDisk`'s unused param, 7.2 `counts.reads` / `measurement.commits`, `s3Publisher.endpoint` removed | n/a |
| R6 missing `rows.Err()` in 7.4 `localResult` | fixed `dcdf3bd` | n/a (experiment) |
| R6 S3 `ContentRange` / SNAP-06 | fixed `1bfdfac`: `S3Source.ReadRange` requires `Content-Range` to start `bytes OFF-LAST/`, else error and n=0 | `TestS3RejectsIgnoredRange`, `TestS3RejectsWrongContentRange`, `TestS3AcceptsMatchingContentRange` (httptest S3 endpoint); live SeaweedFS test still passes |
| R6 `f` not closed in `TestS3RoundTripLive` | fixed `1bfdfac` | n/a |
| R6 7.1 `outsideIO` ignores read/write/writev | fixed `dcdf3bd`: checks pread64, pwrite64, read, write, writev (all traced ones that move bytes); still 0 outside; check text updated | n/a (experiment; evidence line changed) |
| R6 7.4 `RangeRequests>0`, `os.IsNotExist(walErr)` | fixed `dcdf3bd` | n/a |
| R6 govfs null guards | fixed `6d1c483`: `Out` read only when `rc==0` and the out-pointer is non-zero (`xAccess`, `xFileSize`, `xCheckReservedLock`) | n/a |
| R6 `vfsGetLastError` not recorded | fixed `6d1c483`: reviewer is right, `vfstrace.c:1091-1101` does log it; now recorded as `xGetLastError` (Amount = nByte). 7.3 output unchanged (never called in its runs) | n/a |
| SNAP-07 | fixed (R2) | see R2 |
| SNAP-06 | fixed (R6 S3) | see above |
| GOVFS-06 VFSNAME | fixed `6d1c483`: `sqlite.h.in:1008-1018` says VFSNAME returns "the names of all VFS shims and the final bottom-level VFS"; govfs now replaces the result with `<shim name>/<lower result>` (sqlite3_malloc'd, old string freed); returns `<shim name>` alone if the lower VFS gave NULL | `govfs/vfsname_test.go` `TestFileControlVFSNameIncludesShim` (`govfs-test-N/unix`); attack now HELD |
| MAP-08 | fixed by R1. The attack still prints BROKEN because it re-implements the OLD check itself (`strings.Contains(c.Raw, "delete=1")`, `vfs-syscall-map/main.go:755`) instead of calling 7.1; it cannot turn HELD. Its own `matched_delFlag=1:true` confirms the new match | R1 tests |
| MAP-04 mmap | answered (claim scope, no code defect): facts for Neon below. 7.1 runs at the default `mmap_size=0` (its evidence shows `xFileControl(t.db,MMAP_SIZE,0)`) | n/a |
| COST-05 | answered: the attack's own expectation is wrong; facts below. delete/EXTRA part HELD (5 syncs/commit, 2 dir) | n/a |
| SNAP-03 | answered + demonstrated `dcdf3bd`: 7.4 repeat claim now states and checks that the reads so far fit in the page cache (default 500 pages), and a new section repeats the full scan with `PRAGMA cache_size=4`: 17 range requests on the repeat (same as the attack) | experiment check |
| SNAP-04 | answered (documented SQLite behaviour, not demonstrated): facts below | n/a |

## Stale prose for Neon

Include blocks (`go run ./cmd/course inline ch07-.../ch07-vfs-and-filesystem-io.md` regenerates them; the walk-throughs around them need the facts):

- `ch07…md:212` block of 7.1 `main.go`: new `byteMovingSyscalls`; `portable()` now calls `closeLifecycle(calls)` matching `delFlag=1` (lines ~385/390 of the chapter show the old `delete=1`); `type stream` gone; `onDisk` is now `func onDisk(s trace.Syscall) bool`; `outsideIO` (chapter ~589) checks all byte-moving syscalls; check text (chapter 655) now `every pread64/pwrite64/read/write/writev …`.
- `:679` 7.1 evidence (line 342 check text above; also chapter line 1022 quotes it).
- `:1105` 7.2 `main.go`: `counts.reads` and `measurement.commits` removed; `counts.add` skips failed syscalls; new `classifyWALCommits` requires ≥1 checkpoint and one restart per checkpoint.
- `:1675` `govfs/cfunc.go`: `pointerSize` and its comment removed.
- `:1873` `govfs/govfs.go`: `registry.checked` removed; `Register` frees `cname`/`address` on failure; `inject`/`injected` replaced by `injectFault(c Call)` + `appendLocked`; `methodTableFor` frees the losing allocation.
- `:2289` `govfs/methods.go`: `vfsGetLastError` now records (the comment "forwards without recording, matching vfstrace.c" was wrong and is deleted; chapter line 2568 repeats it — vfstrace.c:1091-1101 does log `xGetLastError`); new `prependVFSName` for `SQLITE_FCNTL_VFSNAME`; null/rc guards on `Out`; fault paths use `injectFault`.
- `:3067` 7.3 `main.go`: `outcome.stepCode`, `rolledBack`, `committed`, `trialHeld`; stricter checks (above). Evidence unchanged.
- `:4142` `rangefs/rangefs.go`: `Request.Failed`; failed range format; zero-length `ReadAt`; Seek/Stat closed checks; nil ctx default. The kept `FS` doc comment ("each Read or ReadAt on an open file issues one Source.ReadRange call") is now inexact: a zero-length read issues none.
- `:4481` `rangefs/s3.go`: `ReadRange` verifies `Content-Range` (its doc comment does not mention it; the `http.go` one does for 206). Walk-through at 4637.
- `:4647` 7.4 `main.go` and `:5273`/`:5392` 7.4 evidence (http and s3): repeat check text now ends `; the N reads so far fit in the 500-page cache` (N = 4, 15, 20 incl. the 100-byte header read); new section `== immutable=1 keeps cached pages valid, not cached: the full scan again with PRAGMA cache_size=4 ==` printing `17 requests (0 stat, 17 range), 69632 bytes`; `s3Publisher.endpoint` removed; final republish check also requires the `-wal` to be gone. The 7.4 claim (4643) and 6.x/7 sections that say a repeat costs nothing need the cache caveat.

New facts for narrowed statements:

- **MAP-04 (mmap).** "Every xRead is one pread64" holds at `mmap_size=0`, the default (`SQLITE_DEFAULT_MMAP_SIZE 0`, `sqliteInt.h:1113-1115`; 7.1 evidence shows `MMAP_SIZE,0`). With `PRAGMA mmap_size>0` the pager gets pages through `xFetch` (`pager.c` `USEFETCH`, `getPageMMap` at 5687) and `unixRead` itself copies from the mapping with `memcpy` when `offset < mmapSize` (`os_unix.c` unixRead, 3512 + lines 24-31 of the function) — no syscall. Attack run: 18 `xFetch`, 3 `xRead`, one `xRead` with 0 `pread64`, 2 `pread64` total for a 100-row scan. So: "every byte goes through a VFS method" stays true; "each xRead → one pread64" needs "with mmap off (the default)".
- **COST-05 (other journal modes).** delete/EXTRA: 5 syncs/commit `[db:1 dir:2 journal:2]` — the 2nd dir sync is `unixDelete`'s `dirSync` (`pager.c:2120` passes `extraSync`). PERSIST and TRUNCATE do NOT save the directory sync on unix: `pager_unlock` closes the journal whenever the lock drops unless the VFS reports `SQLITE_IOCAP_UNDELETABLE_WHEN_OPEN` (`pager.c:1891-1895`; unix does not), the next transaction reopens it with `SQLITE_OPEN_CREATE`, so `isNewJrnl` (`os_unix.c:6569-6573`) re-arms `UNIXFILE_DIRSYNC` (`:6777`) and the first `xSync` syncs the directory again. They do save the unlink. Measured (20 commits): persist/NORMAL 80 syncs `db:20 dir:20 journal:40` (journal 2/commit: commit sync + `zeroJournalHdr` sync, `pager.c:1399-1414`), 0 unlinks; truncate/FULL 100 syncs `db:20 dir:20 journal:60` (3/commit: the extra one is the sync after `ftruncate` when `fullSync`, `pager.c:2086-2097`), 0 unlinks.
- **SNAP-03.** `immutable=1` makes the pager act like a temp file (`pager.c:5033-5056`: no locks, pretend EXCLUSIVE) so the change-counter check that discards the cache (`pager.c:5436`, skipped for `tempFile`) never runs. That keeps cached pages valid; it does not keep them cached. Default cache is `-2000` KiB = 500 pages at 4096 (`sqliteLimit.h:161`); with `cache_size=4` the 18-page scan repeated costs 17 range requests (shown in 7.4 now).
- **SNAP-04.** Overwriting the served object in place under an open `immutable=1` connection breaks it: `sqlite.h.in:3922-3929` — "Setting the immutable property on a database file that does in fact change can result in incorrect query results and/or SQLITE_CORRUPT errors". Attack: same connection kept old cached page 1 (`user_version=29`) and read new pages → `database disk image is malformed (11)`, integrity_check reports invalid page numbers. Narrowed statement: publish each snapshot under a new key (or replace the object only when no connection has it open) and open a new connection to see it; 7.4's republish section already opens a fresh connection.

## Commands run (final tree)

- `gofmt -l internal ch07-vfs-and-filesystem-io cmd` — empty. (Repo-wide `gofmt -l .` lists 5 files under `agent_work/probes/`, pre-existing, not tracked by `check gofmt`.)
- `go build ./... && go vet ./... && go test -count=1 ./...` — PASS.
- `go test -race -count=1 ./internal/govfs ./internal/rangefs` — PASS.
- `bash agent_work/verify/crossbuild.sh` — `CROSSBUILD OK`.
- `go run ./cmd/course check all` — exit 1: all sources/spans/evidence (every experiment 2 runs identical to evidence, ch01, ch02, ch07)/gofmt PASS; FAIL `inline -check` ch07 with the 12 stale include blocks listed above (chapter is Neon's); FAIL `check paths` on the git-ignored `_tools/bin/kb` binary only (pre-existing, same as before the repair).
- Experiments, each run twice: 7.1, 7.2, 7.3 via `check evidence` (2 runs identical) plus one manual run each; 7.4 http via `check evidence`; `7.4 -s3` run twice, identical, written to `evidence/7.4-read-only-snapshot-vfs-s3.txt`.
- `bash agent_work/ch07/adversarial/run-all.sh` — 29 HELD, 5 BROKEN: MAP-04, COST-05, SNAP-03, SNAP-04 (claim-scope, answered above; the attacks keep their unnarrowed expectations), MAP-08 (attack replays the old check; see table). GOVFS-06, SNAP-06, SNAP-07 now HELD.
