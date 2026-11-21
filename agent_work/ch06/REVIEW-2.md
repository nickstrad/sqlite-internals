VERDICT: PASS

## Re-review of REVIEW-1 findings and Run-1 BROKEN attacks

| Item | Fixed / Answered? | How confirmed |
| --- | --- | --- |
| REVIEW-1 #1 (blocker): `Lab.StillWaiting` 0 ns poll & 6.2 Part E waiter not contending | Fixed (`c2ab36f`) | Read `internal/procs/parent.go:256-286`, `internal/procs/lab.go:10,79-91`, `6.2-busy-rules/main.go:172-182`. `Call.AnsweredWithin(250ms)` waits non-destructively without killing the child; `Lab.StillWaiting` reports `no answer after 250ms` / `already answered`. Part E now sends `begin immediate` as the waiting call before `recipe-exec k+1` and `commit`. Ran `go test -race -count=1 -v ./internal/procs` (`TestAnsweredWithinWaitsWithoutKilling`, `TestLabStillWaitingCanFail`, `TestStillWaitingSeesImmediateBusy` all PASS) and `strace -f -e trace=fcntl` on `6.2-busy-rules`: 86 `EAGAIN`s on `-shm` byte 120 overall, with 12 `EAGAIN`s in every single one of Part E's 5 rounds (zero uncontended rounds). |
| REVIEW-1 #2 (should-fix): 6.1 `F_GETLK` "both hold SHARED" wording & unused `/proc/locks` | Fixed (`c2ab36f`) | Read `internal/procs/lab.go:114-179` and `6.1-locks-belong-to-the-process/main.go:42-54,73-135`. `Lab.Locks` now returns `LockView{Ranges, Proc, ProcListed}` with `ProcHolders(rangeName)` (covered by `TestLockViewProcHolders`, PASS). 6.1 reworded the portable `F_GETLK` database-file check to what `F_GETLK` proves and added 11 `/proc/locks:` checks on Linux asserting exact holder sets on `RESERVED`, `SHARED`, `WAL_WRITE_LOCK`, and `DMS` before and after each stray close. Verified via `check evidence ch06 -runs 2`. |
| REVIEW-1 #3 (should-fix): `copierSession.copyRange` wrote zero padding after short `ReadAt` | Fixed (`c2ab36f`) | Read `internal/procs/sqlrole.go:443-468`. Extracted `readRange` which slices `buffer[:n]` on short read/EOF; `copyRange` writes only `buffer` and reports `bytes start-(start+len(buffer))`. Confirmed by `TestReadRangeKeepsOnlyBytesRead` (PASS). |
| REVIEW-1 #4 (nit): `/proc/locks` parser rejected valid host lines (`<none>:0`, `MSNFS`, `UNKNOWN`) | Fixed (`c2ab36f`) | Read `internal/procs/proclocks.go:14,94-157`. Accepts `<none>:0` (`NoInodeField`), class `UNKNOWN`, mode `MSNFS`/`UNKNOWN`, type `NONE`, while continuing to reject malformed `<none>:1` and bad ranges. Confirmed by `TestParseProcLocksHostVariants` (PASS) and adversarial `PROCS-02c` (HELD, `parsed=3/3`). |
| REVIEW-1 #5 (nit): `Child.Stop` left child running on non-timeout error; `readReplies` leak | Fixed (`c2ab36f`) | Read `internal/procs/parent.go:37-38,97,139-143,327-330,353`. `Stop` calls `c.Kill()` on any `c.await` error; `Kill` closes `c.killed` via `sync.Once`, unblocking `readReplies`. Confirmed by `TestStopKillsChildWhenExitAnswerIsWrong` with `-race` (PASS). |
| REVIEW-1 #6 (nit): `build-notes.md` misstated 6.3 copy-opening order | Fixed (`a045be4`) | Read `agent_work/ch06/build-notes.md` (6.3 section): now accurately states Part A opens `main-only.db` and `torn.db` (separate files) before `StopChildren()` while Part B stops children first. |
| Attack `BUSY-09` (Run 1 BROKEN) | Fixed (`c2ab36f`) | Now HELD in `results-run2.txt` (`still_waiting_on_timeout0=false`, `eagain_on_byte120=82`); reproduced 86 `EAGAIN`s via direct `strace`. |
| Attack `PROCS-02c` (Run 1 BROKEN) | Fixed (`c2ab36f`) | Now HELD in `results-run2.txt` (`parsed=3/3 rejected=[]`). |
| Attack `LOCK-06` (Run 1 BROKEN) | Answered & claim corrected (`3a7ccf7`) | Experiment 6.1 added Part C (`walWithFrames`, steps 21–30) and `build-notes.md` Correction 1 narrowed the `SQLITE_PROTOCOL` claim to `WAL_READ_LOCK(0)` (empty/checkpointed WAL), citing `wal.c:3879` (`if( pWal->readLock==0 )`, verified in `$SQLITE_SRC/src/wal.c` and `SPANS.txt`). Confirmed 6.1 Part C passes identically across 2 runs. Attack EXPECT updated in Part 2 to assert the corrected claim. |
| Attack `LOCK-04b` (Run 1 BROKEN) | Answered & claim corrected (`3a7ccf7`) | Experiment 6.1 added Part D (`walContenderExits`, steps 31–38) showing that when the contender closes and exits before the holder commits, `sqlite3WalClose` (`wal.c:2507-2571`, verified in `$SQLITE_SRC`) deletes `-wal` and `-shm`, and the holder's `COMMIT` returns `SQLITE_OK` into the unlinked `-wal`, silently losing the contender's commit. Confirmed 6.1 Part D passes identically across 2 runs. Attack EXPECT updated in Part 2. |
| Attack `BUSY-05b` (Run 1 BROKEN) | Answered & claim narrowed (`a045be4`) | `build-notes.md` Correction 6 narrows "writer never blocks readers" in WAL mode to default `PRAGMA locking_mode=NORMAL` and documents `locking_mode=EXCLUSIVE` (`pager.c:5982-5988`, already in `SPANS.txt`) as the boundary where the writer holds the DB EXCLUSIVE lock. Attack EXPECT updated in Part 2. |
| Attack `SNAP-04` (Run 1 BROKEN) | Answered (`a045be4`) | Ran `go run ./agent_work/ch06/probes/snap04-restart` (`SNAP04 PROBE OK`): after step 6 (22 frames >= `wal_autocheckpoint=20`), WAL is fully checkpointed (`nBackfill=22 == mxFrame=22`), so the capture acquires `WAL_READ_LOCK(0)`. Step 7 restarts the WAL under the `WAL_READ_LOCK(0)` reader (`salt1` +1, `nBackfill=0`), which is safe because `walCheckpoint` needs `WAL_READ_LOCK(0)` exclusively to backfill (`wal.c:2254-2260`, verified in `$SQLITE_SRC`). Step 19 restarts the WAL a second time (`salt1` +2). Documented in `build-notes.md` Correction 7 and `SPANS.txt` (`wal.c` 2152–2166, 2254–2260). Attack EXPECT updated in Part 2. |

## New findings

None.

## Verification commands run

- `export SQLITE_SRC=/root/Software/sqlite-src/3.53.4`
- `go build ./... && go vet ./... && go test ./...` — PASS.
- `go test -race -count=1 ./internal/procs ./internal/sqlitex` — PASS (`internal/procs` 13.7s, `internal/sqlitex` 3.9s; all 7 new regression tests verified with `-v`).
- `bash agent_work/verify/crossbuild.sh` — `CROSSBUILD OK` (linux/amd64, darwin/arm64, darwin/amd64, `check gofmt` PASS).
- `go run ./cmd/course check sources ch06` — PASS (2 copied files match the pin).
- `go run ./cmd/course check spans ch06` — PASS (36 spans start at their symbol; manually verified all 4 newly added `wal.c` spans against `$SQLITE_SRC/src/wal.c`).
- `go run ./cmd/course check evidence ch06 -runs 2` — PASS (all four ch06 experiments 6.1–6.4 match stored evidence identically across 2 runs).
- `go run ./ch06-locking-and-concurrency/experiments/6.4-live-snapshot -hold immediate` — PASS (`LIVE SNAPSHOT OK`).
- `go run ./agent_work/ch06/probes/snap04-restart` — PASS (`SNAP04 PROBE OK`, confirmed `WAL_READ_LOCK(0)` and `salt1` +1 at step 7 and +2 at step 8).
- `strace -f -e trace=fcntl` on `6.2-busy-rules` — confirmed 86 `EAGAIN`s on `-shm` byte 120, with 12 `EAGAIN`s in each of Part E's 5 rounds.
- Comment and path check on `git diff b522308..HEAD`: zero stray comments in `.go` files; zero machine-specific paths outside `agent_work/`.
