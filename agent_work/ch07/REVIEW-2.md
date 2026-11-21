VERDICT: PASS

Re-reviewed the repair round commits (`git diff 4ad93c3..HEAD`, `1bfdfac`, `6d1c483`, `dcdf3bd`, `5449623`, `39409a7`) in `/root/Software/sqlite-internals-work/ch07` against `REVIEW-1.md`, `adversarial/RESULTS.md`, `REPAIR-REPORT.md`, and `/root/Software/sqlite-src/3.53.4`.

## Finding and adversarial attack verification

| Finding / Attack | Fixed? | How confirmed |
| --- | --- | --- |
| R1 [blocker] 7.1 `portable()` matches `delete=1` instead of `delFlag=1` | Yes (`dcdf3bd`) | Diff `7.1/main.go:167-184` extracts `closeLifecycle(calls)` matching `slices.Contains(c.Args, "delFlag=1")` and updates the check text; `TestCloseLifecycleMatchesRealVFSTraceLines` and `TestCloseLifecycleRejectsKeptShm` pass against real `-vfstrace` lines via `trace.ParseVFSLine`. |
| R2.1 `rangefs` zero-length `ReadAt` logs bogus `bytes 0--1` range | Yes (`1bfdfac`) | Diff `internal/rangefs/rangefs.go:193-195` returns `0, nil` without calling `ReadRange` or logging; `TestZeroLengthReadAtLogsNothing` passes. |
| R2.2 `rangefs` failed/short `ReadRange` counts `want` in `BytesRead` | Yes (`1bfdfac`) | Diff `internal/rangefs/rangefs.go:42,54-56,200-208` adds `Request.Failed`, logs `Length: int64(n)` on error/short read, and formats `range NAME at OFF failed (N bytes received)`; `TestFailedRangeCountsNoBytes` and `TestShortRangeCountsReceivedBytes` pass. |
| R2.3 `rangefs` `Seek`/`Stat` after `Close`; nil `ctx` in `New` | Yes (`1bfdfac`) | Diff `internal/rangefs/rangefs.go:108-110,177-179,228-230` defaults `nil` ctx to `context.Background()` and guards `f.closed` with `fs.ErrClosed`; `TestSeekAndStatAfterClose` and `TestNilContextDefaults` pass. |
| R3 7.3 fault checks can pass vacuously or accept masked/wrong-code outcomes | Yes (`dcdf3bd`) | Diff `7.3/main.go:326,378,387-400,604,614-630` adds `outcome.stepCode`, `rolledBack()`, `committed()`, and `trialHeld()`, requiring `Fired == (n <= total)`, primary code `10`, `closeErr == ""`, `match.Exact`, `total > 0`, and `len(goCalls) > 0`; `TestTrialHeld` passes and 7.3 evidence reproduces identically. |
| R4.1 `govfs` split-lock race in fault recording and seq bump when not recording | Yes (`6d1c483`) | Diff `internal/govfs/govfs.go:299-336` merges `inject`/`injected` into locked `injectFault(c Call)` + `appendLocked`; `TestInjectedCallIsCompleteAtDisarm`, `TestInjectedCallWithoutRecording`, and `TestDisarmRacingInjectSeesWholeCall` pass under `go test -race`. |
| R4.2 `govfs` C memory leaks on `Register` error paths and `methodTableFor` race loser | Yes (`6d1c483`) | Diff `internal/govfs/govfs.go:132-142,185,404-406` frees `cname` (`libc.Xfree`) and `address` (`sqlite3.Xsqlite3_free`) via LIFO `defer` (executing before `tls.Close()`) on every failed `Register` return, and frees losing `methodTableFor` allocations; `go test -race ./internal/govfs` passes. |
| R5.1 7.2 WAL check can pass with 0 checkpoints | Yes (`dcdf3bd`) | Diff `7.2/main.go:296-312,451-456` extracts `classifyWALCommits` requiring `ok && checkpoints > 0 && restarts == checkpoints`; `TestClassifyWALCommits` passes and 7.2 evidence reproduces identically. |
| R5.2 7.2 `counts.add` counts failed syscalls | Yes (`dcdf3bd`) | Diff `7.2/main.go:262` skips `!s.Succeeded()`; `TestCountsSkipFailedSyscalls` passes and 7.2 evidence reproduces identically. |
| R6 dead code (`registry.checked`, `pointerSize`, `stream`, `onDisk` param, `counts.reads`, `measurement.commits`, `s3Publisher.endpoint`) | Yes (`1bfdfac`, `6d1c483`, `dcdf3bd`) | Confirmed removed in diff across `govfs.go`, `cfunc.go`, `7.1/main.go`, `7.2/main.go`, `7.4/main.go`. |
| R6 missing `rows.Err()` in 7.4 `localResult` | Yes (`dcdf3bd`) | Diff `7.4/main.go:323-325` checks `rows.Err()`. |
| R6 / SNAP-06 `S3Source.ReadRange` does not verify `Content-Range` | Yes (`1bfdfac`) | Diff `internal/rangefs/s3.go:151-159` requires `out.ContentRange` to start with `bytes OFF-LAST/`; `TestS3RejectsIgnoredRange`, `TestS3RejectsWrongContentRange`, `TestS3AcceptsMatchingContentRange`, live `TestS3RoundTripLive`, and adversarial `SNAP-06` (HELD in run 2) all pass. |
| R6 unclosed `f` in `TestS3RoundTripLive` | Yes (`1bfdfac`) | Diff `internal/rangefs/rangefs_test.go:162` adds `defer f.Close()`. |
| R6 7.1 `outsideIO` ignores `read`/`write`/`writev` | Yes (`dcdf3bd`) | Diff `7.1/main.go:77,367,441` checks `byteMovingSyscalls` (`pread64`, `pwrite64`, `read`, `write`, `writev`) and updates check text and evidence; 7.1 passes identically. |
| R6 7.4 `RangeRequests > 0` and `os.IsNotExist(walErr)` | Yes (`dcdf3bd`) | Diff `7.4/main.go:405,652` adds both assertions; 7.4 HTTP and `-s3` pass identically. |
| R6 `govfs` null/rc guards and `vfsGetLastError` recording | Yes (`6d1c483`) | Diff `internal/govfs/methods.go:62-64,93-97,172-174,201-203` guards `Out` reads on `rc == 0 && ptr != 0` and records `xGetLastError` (matching `vfstrace.c:1091-1101`). |
| SNAP-07 `rangefs` edge cases | Yes (`1bfdfac`) | Fixed by R2; `SNAP-07` is HELD in `results-run2.txt` and covered by `internal/rangefs/edge_test.go`. |
| GOVFS-06 `SQLITE_FCNTL_VFSNAME` shim stack identity | Yes (`6d1c483`) | Diff `internal/govfs/methods.go:213-215,287-301` prepends `<shimName>/` via `prependVFSName` using `sqlite3_malloc64`/`sqlite3_free` per `sqlite.h.in:1008-1018`; `TestFileControlVFSNameIncludesShim` passes and `GOVFS-06` is HELD in `results-run2.txt`. |
| MAP-08 7.1 `portable()` macOS check | Yes (`dcdf3bd`) | Fixed by R1 (`closeLifecycle` matches `delFlag=1`). Run 2 still printed BROKEN only because the attack script replayed the old `delete=1` check inline (`vfs-syscall-map/main.go:755`); its `GOT` output already confirms `matched_delFlag=1:true`. Will be updated in Part 3. |
| MAP-04 `PRAGMA mmap_size>0` bypasses `xRead` -> `pread64` | Answered (scope) | Confirmed in source (`sqliteInt.h:1113-1115` `SQLITE_DEFAULT_MMAP_SIZE 0`, `pager.c:5687` `getPageMMap`, `os_unix.c:3533-3548` `unixRead` memcpy). No code defect in 7.1 (which runs at default `mmap_size=0`). Chapter prose and attack expectation to be narrowed in Part 3. |
| COST-05 `PERSIST`/`TRUNCATE` still sync directory on unix | Answered (scope) | Confirmed in source (`pager.c:1891-1895` closes journal on unlock without `SQLITE_IOCAP_UNDELETABLE_WHEN_OPEN`, `os_unix.c:6569-6572,6777` re-arms `UNIXFILE_DIRSYNC`, `pager.c:1399-1415,2086-2122`). No code defect in 7.2. Chapter prose and attack expectation to be updated in Part 3. |
| SNAP-03 `immutable=1` repeat query with `cache_size=4` | Answered + demonstrated (`dcdf3bd`) | Confirmed in source (`pager.c:5033-5056,5436`, `sqliteLimit.h:161`) and now demonstrated directly in 7.4 (`smallCache`: 17 range requests, 69,632 bytes) with cache-fit check in `runQuery`. Chapter prose and attack expectation to be updated in Part 3. |
| SNAP-04 overwriting served object in place behind open `immutable=1` | Answered (scope) | Confirmed in `sqlite.h.in:3922-3929` (explicitly warns against mutating an immutable file in place -> `SQLITE_CORRUPT`). No code defect in 7.4 (which opens a fresh connection after republish). Chapter prose and attack expectation to be updated in Part 3. |

## New findings

None.

## Commands run

- `export SQLITE_SRC=/root/Software/sqlite-src/3.53.4 && go build ./... && go vet ./... && go test -count=1 ./...` — PASS (all packages including new `7.1`, `7.2`, `7.3`, `govfs`, and `rangefs` unit tests).
- `go test -race -count=1 ./internal/govfs ./internal/rangefs ./internal/trace ./internal/sqlitex` — PASS.
- `bash agent_work/verify/crossbuild.sh` — PASS (`CROSSBUILD OK`: linux/amd64, darwin/arm64, darwin/amd64 build+vet+test-compile, `check gofmt` 108 files).
- `go run ./cmd/course check sources ch07` — PASS (`4 copied file(s) match the pin`).
- `go run ./cmd/course check spans ch07` — PASS (`46 span(s) start at their symbol`).
- `go run ./cmd/course check evidence ch07 -runs 1` — PASS (`7.1`, `7.2`, `7.3`, `7.4` all identical to stored evidence).
- `go run ./ch07-vfs-and-filesystem-io/experiments/7.4-read-only-snapshot-vfs -s3` diffed against `ch07-vfs-and-filesystem-io/evidence/7.4-read-only-snapshot-vfs-s3.txt` — PASS (byte-identical).
