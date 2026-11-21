VERDICT: FAIL

Reviewed commit `9979669` (`git diff 8f59174..HEAD`) in `/root/Software/sqlite-internals-work/ch07`. Line numbers below refer to commit `9979669`.

## Findings

### 1. [blocker] `ch07-vfs-and-filesystem-io/experiments/7.1-vfs-calls-to-syscalls/main.go:159` — macOS / non-Linux `portable()` path unconditionally fails

- **What is wrong:** When `!trace.Supported` (macOS / non-Linux), `portable()` checks whether `-shm` was unmapped with delete enabled using `strings.Contains(c.Raw, "delete=1")`. SQLite's `ext/misc/vfstrace.c:843` prints `delFlag=%d`, never `delete=%d` (`trace.xShmUnmap(t.db,delFlag=1) -> SQLITE_OK`). Consequently `shmDelete` is always `false`, line 164 prints `check FAILED: at the last close the -wal is deleted (xDelete) and the -shm is unmapped with delete=1`, and `finish()` exits with status 1, breaking `common.md`'s cross-platform requirement that portable parts pass and exit 0 on macOS.
- **How confirmed:**
  - `ch07-vfs-and-filesystem-io/vfstrace.c:843`:
    `vfstrace_printf(pInfo, "%s.xShmUnmap(%s,delFlag=%d)", pInfo->zVfsName, p->zFName, delFlag);`
  - `ch07-vfs-and-filesystem-io/evidence/7.1-vfs-calls-to-syscalls.txt:291`:
    `VFS xShmUnmap(t.db,delFlag=1) -> SQLITE_OK`
  - Direct CLI reproduction:
    ```
    $ printf '.open --normal /tmp/opencode/test-71.db\nPRAGMA journal_mode=WAL;\nCREATE TABLE t(x);\n' | sqlite3 -vfstrace 2>&1 | grep -E 'xShmUnmap|xDelete'
    trace.xDelete("/tmp/opencode/test-71.db-journal",0) -> SQLITE_OK
    trace.xShmUnmap(test-71.db,delFlag=1) -> SQLITE_OK
    trace.xDelete("/tmp/opencode/test-71.db-wal",0) -> SQLITE_OK
    ```
- **Expected fix:** Match `delFlag=1` (e.g. `len(c.Args) > 1 && c.Args[1] == "delFlag=1"` or `strings.Contains(c.Raw, "delFlag=1")`), update the check text to `delFlag=1`, and add a unit test exercising `portable()`'s check logic against actual `-vfstrace` lines so non-Linux code paths are tested on Linux.

### 2. [should-fix] `internal/rangefs/rangefs.go:148-166` — `(*file).ReadAt` logs bogus zero-length range requests and overcounts `BytesRead` on error

- **What is wrong:**
  1. When `ReadAt(p, offset)` is called with `len(p) == 0` and `offset < f.size`, `want` is `0`. `HTTPSource.ReadRange` (`http.go:53-55`) and `S3Source.ReadRange` (`s3.go:120-122`) both short-circuit and return `0, nil` without issuing any request, yet `rangefs.go:160` still appends `Request{Kind: KindRange, Name: f.name, Offset: offset, Length: 0}`. `Request.String()` (`rangefs.go:42`) then prints an invalid range (`bytes 0--1 (0 bytes)`), and `Summarize` counts a network request that never occurred. (`Read` guards `len(p) == 0` at line 174; `ReadAt` does not.)
  2. Line 160 logs `Length: want` before checking `err` or `n != want`. If `ReadRange` fails or returns a short read, `Stats.BytesRead` still adds `want` bytes.
  3. `(*file).Seek` (`rangefs.go:185`) and `(*file).Stat` (`rangefs.go:144`) do not reject calls after `Close()`, and `New` (`rangefs.go:83`) stores a `nil` `ctx` without defaulting to `context.Background()`, which panics in `http.NewRequestWithContext`.
- **How confirmed:** Code inspection of `internal/rangefs/rangefs.go:41-42,83-85,144-202`, `internal/rangefs/http.go:52-55`, and `internal/rangefs/s3.go:119-122`.
- **Expected fix:** Guard `if len(p) == 0 { return 0, nil }` in `ReadAt` (after `f.closed` and `offset < 0` checks); log `Length: int64(n)` (actual bytes read); check `f.closed` in `Seek`/`Stat`; default `ctx == nil` to `context.Background()` in `New`; add unit tests for zero-length `ReadAt`, failed `ReadRange` byte accounting, and `Seek` after `Close`.

### 3. [should-fix] `ch07-vfs-and-filesystem-io/experiments/7.3-go-vfs-shim/main.go:557,579-587` (and `:312`) — fault-injection checks can silently accept unfired faults, masked-only matches, close errors, or wrong error codes

- **What is wrong:**
  1. In `partB`, lines 579–587 branch on `if report.Fired` instead of asserting `report.Fired == (n <= total)`. If an armed fault for `n <= total` failed to fire and step 4 committed, control would enter the `else` branch, `allRolledBack` would remain `true`, and line 596 would still report `check ok` for all `total` calls.
  2. Line 580 checks `o.stepErr != "" && !o.rowPresent && o.integrity == "ok" && o.match.Step == faultStep-1 && o.userVersion == faultStep-1`, omitting `o.match.Exact` (so a change-counter drift matching only via `MatchHashes`'s masked fallback would still pass, contradicting the "exact bytes" claim), `o.closeErr == ""`, and checking that the error code is `SQLITE_IOERR` (`10`). Line 586 similarly omits `o.match.Exact` and `o.closeErr == ""`.
  3. Neither `partB` (line 557: `total > 0`) nor `partA` (line 312: `len(goCalls) > 0`) guards against empty slices passing vacuously.
- **How confirmed:** Code inspection of `ch07-vfs-and-filesystem-io/experiments/7.3-go-vfs-shim/main.go:312,554-597` and `internal/recipe/hashes.go:175-211` (`Match{Step, Exact}`).
- **Expected fix:** In `partB`, require `total > 0`, assert `report.Fired` for `n <= total` and `!report.Fired` for `n == total+1`, require `o.match.Exact`, `o.closeErr == ""`, and `sqlitex.PrimaryErrorCode(stepErr) == int(govfs.CodeIOErr)` on fired trials (and `o.match.Exact && o.closeErr == ""` on `n == total+1`). In `partA`, require `len(goCalls) > 0`.

### 4. [should-fix] `internal/govfs/govfs.go:101-145,244-290,338-356` — split-lock race in fault recording and C memory leaks on error/race paths

- **What is wrong:**
  1. `inject` (`govfs.go:251-267`) sets `v.fired = &Call{}` (zero value) and releases `v.mu`, then `injected` -> `record` (`govfs.go:244-249,275-282`) re-acquires `v.mu` to write `*v.fired = c`. If `Disarm()` runs in between, it returns `FaultReport{Fired: true, Call: Call{}}` with an empty `Call` and clears `v.fired`. Also, when `v.recording == false`, `record` still increments `v.seq` on injected calls even though nothing is appended to `v.calls`.
  2. In `Register` (`govfs.go:101-145`), `cname` allocated by `libc.CString(name)` is never freed on any error return (`sqlite3_vfs_find != 0` — hit on every duplicate registration including `TestRegisterValidation` — `checkRealVFS`, `checkFunctionPointerConvention`, `malloc64` failure, or `sqlite3_vfs_register` failure), and `address` is leaked if `sqlite3_vfs_register` fails. In `methodTableFor` (`govfs.go:338-356`), a goroutine losing the race on `registry.tables[key]` returns `existing.address` without freeing its own `sqlite3_malloc64` allocation.
- **How confirmed:** Code inspection of `git show 9979669:internal/govfs/govfs.go` lines 101–145, 244–290, and 338–356.
- **Expected fix:** Merge `inject` and `injected` into a single locked `maybeInject(c Call) (int32, bool)` that populates `v.fired`, `v.seq`, and `v.calls` atomically under `v.mu`. Free `cname` (`libc.Xfree`) and `address` (`sqlite3.Xsqlite3_free`) on all error and race-loser paths.

### 5. [should-fix] `ch07-vfs-and-filesystem-io/experiments/7.2-io-cost-by-pattern/main.go:249-271,424-443` — WAL checkpoint/restart check can pass with 0 checkpoints, and `counts.add` ignores failed syscalls

- **What is wrong:**
  1. In `report` (`main.go:424-443`), `check(ok, ...)` asserts that every commit fits one of `plain`, `checkpoint`, or `restart` and prints `(%d such)` for `checkpoints` and `restarts`, but never asserts `checkpoints > 0 && restarts == checkpoints`. If `wal_autocheckpoint` were misconfigured and never fired, `ok` would still be `true` and print `check ok: ... (0 such) ... (0 such)`.
  2. In `counts.add` (`main.go:249-271`), `open`, `unlink`, `sync`, and `ftruncate` are counted without checking `s.Succeeded()`, so a failing syscall (e.g. `ENOENT`) on a matching path would be miscounted as a file create/delete/sync.
- **How confirmed:** Code inspection of `ch07-vfs-and-filesystem-io/experiments/7.2-io-cost-by-pattern/main.go:249-271,424-443`.
- **Expected fix:** Check `ok && checkpoints > 0 && restarts == checkpoints` at line 442, and skip `!s.Succeeded()` in `counts.add`.

### 6. [nit] Minor quality, dead-code, and check-tightening issues

- **Dead code / unused fields:**
  - `internal/govfs/govfs.go:71,77`: `registry.checked map[uintptr]error` is initialized and never used (`checkedTables` in `selfcheck.go` replaced it).
  - `internal/govfs/cfunc.go:21`: `const pointerSize = unsafe.Sizeof(uintptr(0))` is unused.
  - `ch07-vfs-and-filesystem-io/experiments/7.1-vfs-calls-to-syscalls/main.go:167-172`: `type stream` is unused; `onDisk(dir string)` ignores `dir`.
  - `ch07-vfs-and-filesystem-io/experiments/7.2-io-cost-by-pattern/main.go:214,280`: `counts.reads` and `measurement.commits` are populated and never read.
  - `ch07-vfs-and-filesystem-io/experiments/7.4-read-only-snapshot-vfs/main.go:105`: `s3Publisher.endpoint` is stored and never read.
- **Missing `rows.Err()` check:**
  - `ch07-vfs-and-filesystem-io/experiments/7.4-read-only-snapshot-vfs/main.go:302-310` (`localResult`): the `EXPLAIN QUERY PLAN` loop does not check `rows.Err()` after `rows.Next()` (unlike `render` at line 280).
- **`rangefs` S3 range verification & test hygiene:**
  - `internal/rangefs/s3.go:119-133`: unlike `HTTPSource.ReadRange` (which rejects anything other than `206 Partial Content`), `S3Source.ReadRange` does not check `out.ContentRange != nil`; an S3 endpoint that ignored `Range` and returned `200 OK` would silently serve bytes from offset 0.
  - `internal/rangefs/rangefs_test.go:153`: `f` from `fsys.Open("a.db")` is never closed (`defer f.Close()`).
- **Tightening 7.1 and 7.4 checks:**
  - `ch07-vfs-and-filesystem-io/experiments/7.1-vfs-calls-to-syscalls/main.go:349-352`: `outsideIO` checks only `pread64`/`pwrite64`, ignoring `read`/`write`/`writev` even though `DefaultSyscalls` traces them; including them directly checks the broader claim ("every byte SQLite reads or writes goes through a VFS method").
  - `ch07-vfs-and-filesystem-io/experiments/7.4-read-only-snapshot-vfs/main.go:388,588`: require `queryStats.RangeRequests > 0` at line 388 so cold-cache checks cannot pass vacuously with 0 reads, and assert `os.IsNotExist(walErr)` at line 588.
- **`govfs` null-pointer guards & recording consistency:**
  - `internal/govfs/methods.go:52,157,184`: `vfsAccess`, `ioFileSize`, and `ioCheckReservedLock` dereference `pResOut`/`pSize` without checking `!= 0` (and `ioFileSize`/`ioCheckReservedLock` dereference even when `rc != 0`). `vfsGetLastError` (`methods.go:80-82`) is the only wrapped VFS method not recorded (whereas `vfstrace.c:1096` logs it).
- **How confirmed:** Code inspection of commit `9979669` at the listed file:line locations.
- **Expected fix:** Remove unused types/fields/constants, add `rows.Err()`, check `out.ContentRange != nil` in `S3Source.ReadRange`, close `f` in `TestS3RoundTripLive`, and tighten the 7.1/7.4/govfs checks above.

## Outline corrections checked (`agent_work/ch07/build-notes.md`)

All 7 builder corrections in `agent_work/ch07/build-notes.md` were verified against `/root/Software/sqlite-src/3.53.4`:
1. **WAL `synchronous=NORMAL` checkpoint & restart syncs:** Confirmed (`pager.c:3685-3698` sets `walSyncFlags = syncFlags<<2` even at NORMAL; `wal.c:2277,2333` sync WAL and DB during `walCheckpoint`; `wal.c:4114-4124` syncs WAL header after `walRestartLog`).
2. **Rollback 3/4 sync syscalls vs 2/3 `xSync` calls:** Confirmed (`os_unix.c:3941-3953` performs the one-time directory sync inside `unixSync` for newly created journals).
3. **`vfstrace` prints `FULL` for `SQLITE_SYNC_NORMAL` (0x2):** Confirmed (`ext/misc/vfstrace.c:453` tests `flags & SQLITE_SYNC_FULL` where `SQLITE_SYNC_FULL == 0x03`, matching `0x02`; `pager.c:3687-3690` passes `SQLITE_SYNC_NORMAL` unless `PAGER_FULLFSYNC` is set; `pager.c:4416-4417` adds `DATAONLY` only for `SQLITE_SYNC_FULL`).
4. **`xAccess` (`SQLITE_ACCESS_EXISTS`) maps to `stat`, treating 0-byte regular files as absent:** Confirmed (`os_unix.c:6895-6901`).
5. **`xRandomness` placement differs across CLI builds:** Confirmed by inspection of 7.1 logic and evidence; omitting the single `xRandomness` line from the printed stream while still reporting its count and `INFO:` position is honest.
6. **Go driver vs C CLI VFS call equality (7.3 part A):** Confirmed by running 7.3; all 4 configurations match call-for-call. Also verified in `modernc.org/sqlite@v1.60.1/lib` that `darwin/arm64` and `darwin/amd64` share `_posixIoMethods` (`sqlite_g_000000000001feab.go`) and `_aVfs[0]` `_unix*` slots (`sqlite_g_0000000000000003.go`), and that all 7 unwrapped `unixDl*` and `unix*SystemCall` functions in `os_unix.c` ignore `pVfs`.
7. **Section 4.3 superseded by ruling 11:** Confirmed in `OUTLINE.md` section 8 ruling 11.

Note on conventions at commit `9979669`: no cgo, no comments in Opus's committed Go files, no machine-specific paths outside `agent_work/` (only upstream SQLite C comments inside copied `os_unix.c`/`vfstrace.c`), WAL default respected and evaluated.

## Commands run

- `export SQLITE_SRC=/root/Software/sqlite-src/3.53.4 && gofmt -l internal/govfs internal/rangefs internal/sqlitex internal/trace ch07-vfs-and-filesystem-io` — PASS (empty output).
- `GOOS=darwin GOARCH=arm64 go build ./... && GOOS=darwin GOARCH=arm64 go vet ./...` — PASS.
- `go build ./... && go vet ./...` — PASS.
- `go test -v ./internal/govfs ./internal/rangefs ./internal/sqlitex ./internal/trace` — PASS (all tests including `TestSignaturesMatchTranslatedSource` and live `TestS3RoundTripLive` ran and passed).
- `go test -race ./internal/govfs ./internal/rangefs ./internal/sqlitex ./internal/trace` — PASS.
- `agent_work/verify/crossbuild.sh` — PASS (`CROSSBUILD OK`: linux/amd64, darwin/arm64, darwin/amd64 build+vet+test-compile, gofmt).
- `go run ./cmd/course check sources ch07` — PASS (`4 copied file(s) match the pin`).
- `go run ./cmd/course check spans ch07` — PASS (`46 span(s) start at their symbol`).
- `go run ./cmd/course check gofmt` — PASS (`71 Go file(s) formatted`).
- `go run ./cmd/course check paths` — FAIL on pre-existing `_tools/bin/kb` only; zero ch07 files reported.
- `go run ./cmd/course check evidence ch07 -runs 2` — PASS (`7.1`, `7.2`, `7.3`, `7.4`: 2 consecutive runs identical to stored evidence).
- `go run ./ch07-vfs-and-filesystem-io/experiments/7.4-read-only-snapshot-vfs -s3` (run twice and diffed against `evidence/7.4-read-only-snapshot-vfs-s3.txt` and HTTP evidence) — PASS (identical across two runs; matches stored `-s3` evidence; differs from HTTP evidence only on the `route` line).
- `printf '.open --normal /tmp/opencode/test-71.db\nPRAGMA journal_mode=WAL;\nCREATE TABLE t(x);\n' | sqlite3 -vfstrace 2>&1 | grep -E 'xShmUnmap|xDelete'` — confirmed `vfstrace` emits `delFlag=1`, proving Finding 1.
- `git diff 8f59174..HEAD` scans for Go comments, cgo imports, and machine-specific paths — clean at commit `9979669`.
