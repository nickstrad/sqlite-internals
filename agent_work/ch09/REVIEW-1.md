VERDICT: FAIL

## Findings

1. **blocker** — `internal/walship/shipper.go:423-433` (`Shipper.classify`)
   - **What is wrong:** `classify` checks `if current == s.wal.Generation` before checking `!s.continuous`. When the last stored position has no WAL (`s.wal.Generation.None()`, i.e. `Header: ""` — the state immediately after `Open` on a closed database, or after a discontinuity snapshot taken on an empty/absent WAL), and commits are written and checkpointed away while the shipper is closed (last connection closes and deletes `-wal`, or an outside `TRUNCATE` empties `-wal`), `current` is also `Generation{}`. `current == s.wal.Generation` evaluates to `true`, `lastCommit(file) < s.wal.Frame` (`0 < 0`) is `false`, and `classify` returns `WALUnchanged`. `Cycle` takes no discontinuity snapshot, leaves `s.image` at the stale pre-shutdown state, and sets `s.continuous = true`. On the next commit, `classify` returns `WALNewGeneration` and `s.ship` replays the new WAL generation's frames onto the stale `s.image`, publishing a silently corrupted segment whose `AfterSHA256` records the corrupted image. `Catalog.Plan` and `Plan.Execute` then succeed and return a `SQLITE_CORRUPT` database.
   - **How confirmed:** Copied the unmodified module to `/tmp/opencode/walship-repro` and ran a unit test (`go test -v -run TestReproResumeWithNoWALSilentlyCorrupts ./internal/walship`) that opens a shipper at recipe step 2, closes it, commits step 3 on a fresh connection (which checkpoints and deletes `-wal` on close), reopens the shipper, runs `Cycle`, commits step 4, runs `Cycle`, restores the latest position, and runs `PRAGMA integrity_check`:
     ```
     === RUN   TestReproResumeWithNoWALSilentlyCorrupts
         review_repro_test.go:20: cycle 1 after step 3 committed+checkpointed while down: change=same WAL generation segment=false snapshot=false
         review_repro_test.go:23: cycle 2 after step 4: change=new WAL generation after the shipper's full checkpoint segment=true
         review_repro_test.go:45: restored position 3: user_version=4 sha256=c9d0cb54a6b8a18c want_step4=e6aa5f4e37fde0be integrity="error SQLITE_CORRUPT (11)"
         review_repro_test.go:48: CORRUPTED RESTORE: position 3 claims user_version=4, sha256=c9d0cb54a6b8a18c != recipe step 4 (e6aa5f4e37fde0be), integrity_check="error SQLITE_CORRUPT (11)"
     --- FAIL: TestReproResumeWithNoWALSilentlyCorrupts (1.78s)
     ```
   - **Expected fix:** Never treat `current.None()` (`""`) as an unforgeable WAL generation identity across `!s.continuous`. When `!s.continuous && current.None()`, either return `WALDiscontinuity` unconditionally (no WAL header is present after resume to prove the database file did not change while no read mark was held), or read the live database pages (`s.readPages(ctx)`) and treat it as `WALUnchanged` only if `SHA256Hex(image) == s.state`, else `WALDiscontinuity`. Add a regression test in `internal/walship/walship_test.go` covering offline commit + WAL deletion/truncation after an initial snapshot and after a discontinuity snapshot with no WAL.

2. **should-fix** — `ch09-backup-crash-recovery-integrity/experiments/9.1-capture-comparison/main.go:290-294, 304-308, 332-339, 440-441`
   - **What is wrong:** Three check gaps in Part B ("the same database while a writer child commits recipe steps"):
     1. `report()` (`main.go:290-294`) returns only `k == want` (where `stateOf` matches by `l.e3ExceptJournalMode`), never checking the claimed equality level (`l.e1` for pages and file+WAL, `l.e2` for backup API, `l.canonical` for `VACUUM INTO`). Part B would still pass if a live capture regressed from E1/E2 to E3.
     2. For `plain copy of the live file` (`main.go:307, 440`), `safe[methodCopy]` is `k == current`, and line 440 checks `!safe[methodCopy]` with the message `"the plain copy equals an older one"`. If the copy were corrupt or matched no step (`k == -1`), `!safe[methodCopy]` would still pass.
     3. For `VACUUM INTO` (`main.go:332-338`), `VACUUM INTO` completes before `x.commit()` is called. The writer is completely idle during `VACUUM INTO`, so the experiment never tests concurrency with an active writer (it would pass even if `VACUUM INTO` acquired an `EXCLUSIVE` lock). Unlike timing-dependent concurrency, this can be tested deterministically with `procs`: have `writer` run `begin immediate` + `recipe-exec 10` (holding the write lock with uncommitted step 10), run `VACUUM INTO` on the parent while the writer holds the write lock (proving it does not get `SQLITE_BUSY` and captures step 9), then `commit` on `writer`.
   - **How confirmed:** Inspected `ch09-backup-crash-recovery-integrity/experiments/9.1-capture-comparison/main.go:105-147, 290-344, 440-441` and `ch09-backup-crash-recovery-integrity/evidence/9.1-capture-comparison.txt:17-30, 84-92`:
     ```
     -- VACUUM INTO, then the writer commits
     step  4  writer     recipe-session 10                    -> SQLITE_OK
     VACUUM INTO                          equals recipe step 9 (want 9): E1 no   E2 no   E3 no   E3-except-journal_mode yes  E1-after-VACUUM-INTO yes
     ```
   - **Expected fix:** Check the exact expected `levels` struct in Part B (not only `k == want`); explicitly assert `k >= 0 && k < current && l.e1` for the stale live copy; and hold an open uncommitted write transaction (`begin immediate` + `recipe-exec`) on `writer` during `VACUUM INTO` so the experiment actually demonstrates non-blocking behavior under a writer.

3. **should-fix** — `internal/walship/apply.go:13-50` (`VerifySegmentFrames`) & `internal/walship/format.go:257-291` (`SegmentMeta.validate`)
   - **What is wrong:** `LoadCatalog` and `Catalog.Plan` are claimed to verify the whole chain before any page is applied (`agent_work/ch09/build-notes.md:210`, `BUILD-REPORT.md:18-19`). However, `VerifySegmentFrames` only reads bit 0 of `header[0:4]` and salts `header[16:24]`; it does not validate the 32-byte WAL header itself (magic `0x377f0682`/`0x377f0683`, format version `3007000`, header page size `== meta.PageSize`, header checksum bytes `24:32`), and it never verifies that each frame's `dbSize` field (`frame[4:8]`) agrees with `meta.Commits` (non-zero iff listed in `meta.Commits`, with `dbSize == c.Pages`). Consequently, a segment with a corrupted WAL header or forged `meta.Commits` pointing to a non-commit frame passes `LoadCatalog` (`Damaged == 0`) and `Catalog.Plan`, failing only during `Plan.Execute`.
   - **How confirmed:** Ran `go test -v -run TestReproMalformedHeaderAndCommitMetaPassPlan ./internal/walship` in `/tmp/opencode/walship-repro`:
     ```
     === RUN   TestReproMalformedHeaderAndCommitMetaPassPlan
         review_repro_test.go:83: corrupt header magic: damaged=0 planErr=<nil>
         review_repro_test.go:86: corrupt header magic: Execute failed with: segment segments/000000000003: replay of frames 1-18 found 0 valid frames, last commit at 0; want 18 and 18
         review_repro_test.go:104: forged non-commit frame 1: damaged=0 planErr=<nil>
         review_repro_test.go:107: forged non-commit frame 1: Execute failed with: segment segments/000000000003: replay of frames 1-1 found 1 valid frames, last commit at 0; want 1 and 1
     --- PASS: TestReproMalformedHeaderAndCommitMetaPassPlan (1.31s)
     ```
   - **Expected fix:** Validate the 32-byte WAL header fields (magic, version, page size, header checksum) and verify that the commit frames in `frames` (`binary.BigEndian.Uint32(frame[4:8])`) match `meta.Commits` 1-to-1 in frame number and `Pages` inside `VerifySegmentFrames` (so `LoadCatalog` classifies both malformations as `Damaged` and `Plan` rejects them before replay).

4. **nit** — `internal/walship/shipper.go:344-346` (`Shipper.snapshot`)
   - **What is wrong:** `Cycle` (`shipper.go:407`) passes the `data, file` read at line 371 into `s.snapshot(ctx, ReasonPeriodic, s.position, data, file)`. Inside `snapshot`, line 344 checks `if frame != s.wal.Frame || generationOf(data, file) != s.wal.Generation` with error `"the WAL moved under the write lock"`. Because `-wal` is never re-read, `data, file` are the exact values from which `s.wal` was just derived (either by `s.ship()` at line 476 or verified unchanged in `classify()`), making line 344 tautologically false (a check that cannot fail). Line 341 (`digest != s.state`) is genuine because `s.readPages(ctx)` reads live pages from SQLite.
   - **How confirmed:** Code inspection of `internal/walship/shipper.go:333-347, 370-413, 443-478`.
   - **Expected fix:** Re-read `-wal` (`s.readWAL()`) before checking `frame != s.wal.Frame || generationOf(data, file) != s.wal.Generation` for `ReasonPeriodic`, or remove the redundant check and keep the real `digest != s.state` check.

5. **nit** — `ch09-backup-crash-recovery-integrity/experiments/9.1-capture-comparison/main.go:125-127` (`run.compare`)
   - **What is wrong:** When `vacuumed == false`, `compare()` sets `result.canonical = result.e3` without running `VACUUM INTO` on `capture`. For `backup API` (where E1 is `no`, E2 is `yes`, E3 is `yes`), printing `E1-after-VACUUM-INTO yes` asserts a canonical-form byte comparison that was not performed on `a-backup.db` or `b-backup.db`.
   - **How confirmed:** Code inspection of `ch09-backup-crash-recovery-integrity/experiments/9.1-capture-comparison/main.go:119-127`.
   - **Expected fix:** Either run `VACUUM INTO` on `capture` when `!result.e1` and compare its bytes against `x.canonicalRef(k)`, or only report `E1-after-VACUUM-INTO` where it is actually measured.

6. **nit** — `internal/walship/catalog.go:285-289` (`Prune`)
   - **What is wrong:** If `store.Delete(ctx, key)` fails at index `i`, `Prune` returns `deleted, err` (the full planned list, including keys not yet deleted) instead of `deleted[:i], err`.
   - **How confirmed:** Code inspection of `internal/walship/catalog.go:285-290`.
   - **Expected fix:** Iterate with `for i, key := range deleted` and return `deleted[:i], err` on failure.

7. **nit** — `internal/walship/format.go:218-234` (`DecodeSnapshot`) & `internal/walship/shipper.go:128` (`Open`)
   - **What is wrong:** `DecodeSnapshot` accepts a 0-page empty snapshot (`Pages == 0`, `len(body) == 0`), any positive `PageSize` outside `walfile.MinPageSize..MaxPageSize`, and does not validate `meta.WAL.Generation.Bytes()` when `!meta.WAL.Generation.None()`. Separately, `walship.Open` checks `config.Options.File.JournalMode != sqlitex.JournalWAL` case-sensitively, whereas `sqlitex.Open` (`internal/sqlitex/open.go:250`) uses `strings.EqualFold` (so `"WAL"` fails with `ErrNotWAL`).
   - **How confirmed:** Code inspection of `internal/walship/format.go:218-234`, `internal/walship/shipper.go:128`, and `internal/sqlitex/open.go:250`.
   - **Expected fix:** Require `meta.Pages > 0`, `walfile.MinPageSize <= meta.PageSize <= walfile.MaxPageSize`, and valid `meta.WAL.Generation.Bytes()` (when non-empty) in `DecodeSnapshot`; use `strings.EqualFold(config.Options.File.JournalMode, sqlitex.JournalWAL)` in `walship.Open`.

## Verification of Outline Corrections

All 10 corrections recorded in `agent_work/ch09/build-notes.md` were checked against `/root/Software/sqlite-src/3.53.4`:
- **Correction 1 (9.1 backup remaining vs copied):** Confirmed in `src/backup.c:428-429` (`p->nRemaining = nSrcPage+1-p->iNext`), `688-715` (`backupUpdate`/`sqlite3BackupUpdate`), and `728-734` (`sqlite3BackupRestart`).
- **Correction 2 (`VACUUM INTO` journal mode):** Confirmed (`VACUUM INTO` produces a fresh rollback-mode file; E3 differs only in `journal_mode`, canonical forms match at E1).
- **Correction 3 (backup E2 bytes 27, 43, 95):** Confirmed inside declared ignore ranges `24-27, 40-43, 92-99`.
- **Correction 4 & 5 (9.2 portable sync rule & `synchronous=OFF`):** Confirmed in `src/pager.c:3658-3704`, `src/wal.h:25-26`, `src/wal.c:2277, 2333, 4121-4124`, and verified against strace (`OFF` leaves 44 unsynced database-file writes).
- **Correction 6 (9.2 FULL commit-sync kill recovers `acknowledged + 1`):** Confirmed in 9.2 evidence and code.
- **Correction 7 & 8 (9.4 slot 0 read mark, `nCkpt` / salt-1 per connection):** Confirmed in `src/wal.c:2152-2166, 2233-2261, 3875-3915, 4095-4096`.
- **Correction 9 (spans):** Confirmed via `go run ./cmd/course check spans` (24/24 PASS) and source inspection.
- **Correction 10 (CLI `.backup` busy observation):** Source explanation (`src/backup.c:361-364, 390`, `src/wal.c:2507-2571`) is accurate; honestly marked not reproduced.

## Commands run

| Command | Result |
| --- | --- |
| `export SQLITE_SRC=/root/Software/sqlite-src/3.53.4 && go build ./... && go vet ./...` | PASS (exit 0) |
| `GOOS=darwin GOARCH=arm64 go build ./... && GOOS=darwin GOARCH=arm64 go vet ./...` | PASS (exit 0) |
| `gofmt -l internal/walship internal/capture internal/dbfile internal/procs internal/sqlitex internal/trace ch09-backup-crash-recovery-integrity` | PASS (no output) |
| `go run ./cmd/course check sources` | PASS (`ch09-backup-crash-recovery-integrity: 2 copied file(s) match the pin`) |
| `go run ./cmd/course check spans` | PASS (`ch09-backup-crash-recovery-integrity: 24 span(s) start at their symbol`) |
| `go run ./cmd/course check gofmt` | PASS (`145 Go file(s) formatted`) |
| `go test -v ./internal/walship ./internal/capture ./internal/dbfile ./internal/procs ./internal/sqlitex ./internal/trace` | PASS (`walship` 34.3s, `capture` 3.3s, `dbfile` 35.0s, `procs` 2.7s, `sqlitex` 4.1s, `trace` 2.9s) |
| `go test -race ./internal/walship ./internal/capture ./internal/trace` | PASS (`walship` 74.5s, `capture` 4.2s, `trace` 3.3s) |
| `go run ./cmd/course check evidence ch09` | PASS (9.1, 9.2, 9.3, 9.4: 2 runs identical to stored evidence) |
| `go run ./ch09-backup-crash-recovery-integrity/experiments/9.2-crash-versus-power-loss -portable` | PASS (exit 0, `LINUX-ONLY:` lines printed, `CRASH VERSUS POWER LOSS OK`) |
| `git diff 19ed2d6..HEAD -- '*.go' \| grep -E '^\+.*(//\|/\*)'` | PASS (only string literal `"a//b"` in `store_test.go`, no comments) |
| `git diff 19ed2d6..HEAD -- . ':!agent_work' \| grep -E '^\+.*(/root/\|/home/\|/Users/)'` | PASS (none) |
| `go test -v -run 'TestRepro' ./internal/walship` (in `/tmp/opencode/walship-repro` copy of unmodified code) | Confirmed Findings #1 (`SQLITE_CORRUPT (11)`) and #3 (`damaged=0 planErr=<nil>`) |
