VERDICT: PASS

Re-reviewed repair round 1 (`git diff 2ca7645..HEAD -- '*.go'`, commits `975454c`, `ce3c4b5`, `208d5ad`, `9b7515b`) with `SQLITE_SRC=/root/Software/sqlite-src/3.53.4`.

## Finding and attack verification

| Finding / Attack | Fixed? | How confirmed |
| --- | --- | --- |
| R1 (blocker) — 5.2 never exercises `ValidFrames > LastCommitFrame`; Part C check contradicts printed reason | Yes (`ce3c4b5`) | Read diff in `ch05-wal-transactions-checkpoints/experiments/5.2-wal-replay/main.go`; ran `5.2-wal-replay` and `go run ./cmd/course check evidence ch05` (2 runs match `evidence/5.2-wal-replay.txt`: 60 frames, valid 60, last commit frame 41, 19 valid uncommitted frames ignored with `"valid but uncommitted: no commit frame follows"`). Ran `_tools/bin/showwal _work/5.2/b-mid-transaction/capture-open-transaction/r.db-wal`: 60 frames, 0 `should be` mismatches. New assertions check `ValidFrames == len(Frames) > LastCommitFrame`, `AppliedFrames+SkippedBeyondSize == LastCommitFrame`, and `onlyUncommittedIgnored(report)` in both Parts B and C. |
| R2 (should-fix) — `basePageSize` accepts invalid page sizes; `0` overloaded as empty-base sentinel | Yes (`975454c`) | Read `internal/walfile/replay.go:97-99, 160-165`: validates power-of-two in `MinPageSize..MaxPageSize` (`ErrBaseNotDatabase`) and guards mismatch with `len(base) > 0 && walPageSize != 0`. `TestReplayValidatesBaseWithoutCommits` passes under `go test -v -race ./internal/walfile/...`. |
| R3 (should-fix) — 5.3 calls `count(ctx, writer)` after `take(...)`, leaking `-shm` read-mark updates into next row | Yes (`ce3c4b5`) | Read `ch05-wal-transactions-checkpoints/experiments/5.3-reader-pins-the-wal/main.go:169-297` and `evidence/5.3-reader-pins-the-wal.txt`: writer does not read on `after step 6` / `checkpointed` (`-`), explicit check `!hasMark(start.marks, snapshot) && hasMark(pinned.marks, snapshot)` confirms reader set mark 1 to 22, every commit row's marks match its own `mxFrame`, and `reader ended` matches `step 30 committed` (`0,22,555`). Verified via `check evidence ch05`. |
| R4 (should-fix) — 5.4 wall-clock `waited` column and pass condition in EXACT evidence | Yes (`ce3c4b5`) | Read `ch05-wal-transactions-checkpoints/experiments/5.4-checkpoint-modes/main.go` and `evidence/5.4-checkpoint-modes.txt`: `waited` field, column, and timing assertion removed; elapsed durations appear only on `INFO:` lines. Verified via `check evidence ch05`. |
| R5 (should-fix) — `CompareWithShowwal` checks checksums in one direction only | Yes (`975454c`) | Read `internal/walfile/showwal.go:103-105`: reports disagreement when `i == f.ValidFrames && w.Problem == FrameChecksumMismatch && s.ChecksumAgrees`. `TestCompareWithShowwalReportsWrongRejection` passes under `-race`. |
| R6 (nit) / REP-07b — `ReplayFiles` removes pre-existing `.replay-tmp` it did not create | Yes (`975454c`) | Read `internal/walfile/replay.go:224-240`: `os.OpenFile(..., O_EXCL)` runs before `writeSynced` and returns `os.ErrExist` without calling `os.Remove`. `TestReplayFilesKeepsForeignTempFile` passes under `-race`; `REP-07b` is `HELD` in `results-run2.txt`. |
| R7 (nit) — `CopyLiveFiles(ctx, nil)` fails; `ReadLiveState` leaks temp copies on parse error | Yes (`975454c`) | Read `internal/walfile/livecopy.go:100-102, 183-184`: returns `nil, nil` early for empty pairs and defers `os.Remove` on both copies. `TestCopyLiveFilesWithNoPairs` and `TestReadLiveStateRemovesCopiesOnError` pass under `-race`. |
| R8 (nit) — `HeadersAgree` omits the 4-byte `unused` field | Yes (`975454c`) | Read `internal/walfile/shm.go:139-141`: compares raw 48-byte header slices with `bytes.Equal`, matching `walIndexTryHdr`'s `memcmp`. `TestHeadersAgreeComparesUnusedField` passes under `-race`. |
| R9 (nit) — stray blank line in `5.1-wal-inspector/main.go` imports | Yes (`ce3c4b5`) | Read `ch05-wal-transactions-checkpoints/experiments/5.1-wal-inspector/main.go:7-10`: blank line removed. |
| INS-05b (BROKEN in run 1) — torn `-shm` header undetected when copy 1 is zero and copy 2 is populated | Yes (`975454c`) | Read `internal/walfile/shm.go:176-197`: copy-1-zero + copy-2-valid decodes both copies with `Initialized=false`, `HeadersAgree()==false`, `FramePage` returning `ErrFrameNotIndexed`; copy-1-zero + copy-2-garbage returns `ErrIndexVersion`. `TestParseIndexTornHeader` passes under `-race`; `INS-05b` is `HELD` in `results-run2.txt`. |
| REP-07a (BROKEN in run 1) — 0-commit WAL skips page-size and base-size validation | Yes (`975454c`) | Read `internal/walfile/replay.go:159-169`: validates `ErrPageSizeMismatch` and `ErrBaseSize` before the `LastCommitFrame == 0` early return. `TestReplayValidatesBaseWithoutCommits` passes under `-race`; `REP-07a` is `HELD` in `results-run2.txt`. |
| REP-02 (BROKEN in run 2) — stale-generation base rejection | Answered (stale EXPECT) | Confirmed: SQLite's base database header records no WAL generation/salt metadata (`build-notes.md` correction 6). Raw `Replay(gen0Base, gen2WAL)` cannot detect the mismatch and produces a malformed DB failing `integrity_check`, motivating capture metadata in Chapter 9. Attack updated in Part 2 to assert the corrected claim. |
| REP-05 (BROKEN in run 2) — `savepoint_ghost=true` raw-bytes check | Answered (flawed attack check) | Confirmed directly on `_work/adv-ch05/wal-replay-falsify/rep05/`: `SAVEPOINT_SHOULD_VANISH` appears 87 times in unused page space in both SQLite's own `TRUNCATE`-checkpointed `r.db` and `replayed.db` (E1 byte-identical), while `sqlite3` reports `0` matching rows in `events`, `336` `-v1-v2` rows, and `PRAGMA integrity_check` = `ok`. Attack updated in Part 2 to check SQL row absence, integrity, and E1 parity. |
| CHK-03 (BROKEN in run 2) — `"salt-1 increments on restart"` under cross-connection `TRUNCATE` | Answered (stale EXPECT) | Confirmed against `wal.c:4087-4096` and Experiment 5.4 (`build-notes.md` correction 1): cross-connection `TRUNCATE` advances `-shm` `salt-1` by `+1`, then the writer (whose per-connection `nCkpt == 0`) writes fresh random salts and `ckpt-seq 0->0`; same-connection `TRUNCATE` produces `salt-1 + 1` and `ckpt-seq 0->1`. Attack updated in Part 2 to test both cases. |
| CHK-05 (BROKEN in run 2) — `mode=ro` returned `checkpointed=38` | Answered (attack sequencing bug) | Confirmed via `go run ./agent_work/ch05/repair-probes/readonly-checkpoint`: on an un-backfilled WAL (`mxFrame=38, nBackfill=0`), `mode=ro` `PASSIVE` fails with `disk I/O error (778)` and leaves `nBackfill=0`; after a writer backfills all 38 frames, `mode=ro` `PASSIVE` performs no writes and reports `busy=0 log=38 checkpointed=38`. `CHK-05` had invoked `mode=ro` only after `cpBlock`'s `RESTART` had already backfilled all 38 frames. Attack updated in Part 2 to test `mode=ro` before backfill (must error, `nBackfill == 0`) and after backfill. |

## New findings

None. (Stale doc comments noted in `REPAIR-REPORT.md` — e.g., `5.4-checkpoint-modes/main.go:3` referencing `"busy-timeout waits"` — are tracked for the prose pass that follows this re-review.)

## Commands run

All commands run with `export SQLITE_SRC=/root/Software/sqlite-src/3.53.4`:

| Command | Result |
| --- | --- |
| `go build ./... && go vet ./... && go test -v -race ./internal/walfile/... && go test ./...` | PASS (30 walfile tests + fuzz seeds + all repo packages) |
| `bash agent_work/verify/crossbuild.sh` | `CROSSBUILD OK` (`linux/amd64`, `darwin/arm64`, `darwin/amd64`, `check gofmt`: 102 files formatted) |
| `go run ./cmd/course check sources ch05` | PASS (2 copied files match pin) |
| `go run ./cmd/course check spans ch05` | PASS (28 spans start at their symbol) |
| `go run ./cmd/course check evidence ch05` | PASS (5.1, 5.2, 5.3, 5.4: 2 runs each identical to stored evidence) |
| `go run ./ch05-wal-transactions-checkpoints/experiments/5.{1..4}-*` | PASS (`WAL INSPECTOR OK`, `WAL REPLAY OK`, `READER PINS THE WAL OK`, `CHECKPOINT MODES OK`) |
| `_tools/bin/showwal _work/5.2/b-mid-transaction/capture-open-transaction/r.db-wal` | 60 frames, 0 `should be` mismatches |
| `go run ./agent_work/ch05/repair-probes/readonly-checkpoint` | Before backfill: `disk I/O error (778)`, `nBackfill=0`; after writer backfill: `busy=0 log=38 checkpointed=38` |
| `grep -aob "SAVEPOINT_SHOULD_VANISH"` & `sqlite3` on `_work/adv-ch05/wal-replay-falsify/rep05/` | 87 raw matches in both `r.db` and `replayed.db`; 0 matching SQL rows, 336 `-v1-v2` rows, `integrity_check` = `ok` |
