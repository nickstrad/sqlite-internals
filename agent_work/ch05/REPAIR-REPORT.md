# Chapter 5 — repair round 1 report (Opus)

Inputs: `REVIEW-1.md` (VERDICT: FAIL, 9 findings), `adversarial/results-run1.txt` (21 HELD, 7
BROKEN), `build-notes.md`. Branch `chunk/ch05`, after merging main (chapters 1 and 2): before any
change the merged tree built, passed `go test ./...`, and reproduced 5.1-5.4 evidence.

Commits: `975454c` walfile fixes + tests; `ce3c4b5` experiment fixes + evidence; the report/notes
commit that contains this file.

## Findings and attacks

| id | outcome | test added |
| --- | --- | --- |
| R1 blocker — 5.2 never shows valid uncommitted frames; Part C check text wrong | fixed `ce3c4b5`: open transaction is now `UPDATE documents SET title = upper(title)` → 60 frames, all valid, 19 valid uncommitted after commit frame 41 (showwal: 0 mismatches). New checks: `ValidFrames == frames > LastCommitFrame`; applied + skipped == LastCommitFrame and the only ignored group is the uncommitted tail (fails if Replay used `Frames[:ValidFrames]`); Part C check now matches the printed reason | experiment checks (5.2) |
| R2 should-fix — `basePageSize` accepts invalid sizes; `0` overloaded as empty-base sentinel | fixed `975454c`: base page-size field must be a power of two in 512..65536 (`ErrBaseNotDatabase`); mismatch guard is `len(base) > 0` | `TestReplayValidatesBaseWithoutCommits` |
| R3 should-fix — 5.3 writer read after sampling leaks into the next row | fixed `ce3c4b5`: writer `count(*)` runs before each sample; no writer read on the row before a reader begins (`-`); new check that mark 22 is absent before the reader and present after | experiment check (5.3) |
| R4 should-fix — 5.4 wall-clock `waited` column and pass condition | fixed `ce3c4b5`: column and check removed; elapsed only on `INFO:` lines | — (no unit test possible; evidence EXACT twice) |
| R5 should-fix — `CompareWithShowwal` one-directional | fixed `975454c`: also reports walfile `checksum mismatch` at the first rejected frame when showwal says the checksum agrees | `TestCompareWithShowwalReportsWrongRejection` |
| R6 nit — `ReplayFiles` removes a `.replay-tmp` it did not create | fixed `975454c`: the temp file is opened in `ReplayFiles`; an existing one returns the `O_EXCL` error (`os.ErrExist`) and is left untouched | `TestReplayFilesKeepsForeignTempFile` |
| R7 nit — `CopyLiveFiles(nil)` error; `ReadLiveState` leaks copies on parse error | fixed `975454c`: early `nil, nil`; deferred removal | `TestCopyLiveFilesWithNoPairs`, `TestReadLiveStateRemovesCopiesOnError` |
| R8 nit — `HeadersAgree` ignores the `unused` field | fixed `975454c`: compares the raw 48-byte copies | `TestHeadersAgreeComparesUnusedField` |
| R9 nit — stray blank import line in 5.1 | fixed `ce3c4b5` | — |
| INS-05b BROKEN — torn `-shm` header undetected | fixed `975454c`: copy 1 zero + copy 2 valid → no error, `Initialized=false`, both headers decoded, `HeadersAgree()=false`. Now HELD (`hdr1.mxFrame=22`) | `TestParseIndexTornHeader` |
| REP-07a BROKEN — zero-commit WAL skips page-size and base-size validation | fixed `975454c`: validation runs before the zero-commit return. Now HELD | `TestReplayValidatesBaseWithoutCommits` |
| REP-07b BROKEN — pre-existing `.replay-tmp` deleted | fixed `975454c` (same as R6). Now HELD | `TestReplayFilesKeepsForeignTempFile` |
| REP-02 BROKEN — stale-generation base not rejected | answered: cannot be detected from base + WAL alone; the database file records no WAL generation and the WAL header has only its salts and its own sequence (build-notes correction 6, confirmed by REVIEW-1). Needs capture metadata → `internal/walship` (ch 9). The attack expects something the outline cannot deliver with these inputs | — |
| REP-05 BROKEN — `savepoint_ghost=true` | answered: not a replay defect. The same bytes are in SQLite's own result: `SAVEPOINT_SHOULD_VANISH` occurs 9× in the original after SQLite's TRUNCATE checkpoint, 9× in the WAL, 9× in our replay (E1 equal); 0 of 336 `events` rows contain it, `integrity_check ok`. Leftover cell bytes in unused page space that SQLite wrote in committed frames. The attack's raw-bytes test is wrong; the replay part of it (valid 10/10, E1 equal) held | — |
| CHK-03 BROKEN — "salt-1 increments on restart" | answered: outline wording, corrected in build-notes correction 1 (confirmed by REVIEW-1). The attack's own output shows it: shm salt-1 +1 right after TRUNCATE, the next writer (another connection, `nCkpt==0`) writes fresh random salts and checkpoint-seq 0→0 (wal.c:4087-4096). 5.4 already shows both cases | — |
| CHK-05 BROKEN — `mode=ro` "can checkpoint" | answered: it did not checkpoint. The RESTART checkpointer started earlier in the attack had already backfilled all 38 frames before it waited on the reader; the `mode=ro` call only reports nBackfill. Probe `agent_work/ch05/repair-probes/readonly-checkpoint`: `mode=ro` PASSIVE on an un-backfilled WAL → `disk I/O error (778)`, nBackfill stays 0; after the writer backfills, the same call returns `busy=0 log=38 checkpointed=38`, no error. The NOOP, close and E1 parts held | — |

All seven new unit tests in `internal/walfile/repair_test.go` were run against the pre-repair
code (a temporary worktree of `2ca7645`): all seven FAIL there and PASS now.

## Stale prose for Neon

The chapter `.md` does not exist yet. These comments describe code that changed. They are still
there, so check each one:

| file | what changed | new facts |
| --- | --- | --- |
| `internal/walfile/shm.go` `ParseIndex` doc | torn-header case added | copy 1 all zero + copy 2 with the version word → no error, `Initialized=false`, both headers and checkpoint info decoded (order from copy 2), `HeadersAgree()=false`; copy 1 zero + copy 2 garbage → `ErrIndexVersion`; both zero → uninitialized as before |
| `internal/walfile/shm.go` `HeadersAgree` doc | comparison | raw 48-byte copies, including the `unused` field (bytes 4-7 / 52-55), like `walIndexTryHdr`'s memcmp |
| `internal/walfile/replay.go` `Replay` doc | validation order | base page-size field must be a power of two in 512..65536; page-size mismatch and base-not-whole-pages are errors even when the WAL has no commit frame; an empty WAL checks the base against its own page size |
| `internal/walfile/replay.go` `ReplayFiles` doc | temp file | an existing `outPath.replay-tmp` → `os.ErrExist` error, file left as it was; the temp file is removed on error only if this call created it |
| `internal/walfile/showwal.go` `CompareWithShowwal` doc | second direction | also reports when walfile rejects the first invalid frame for a checksum mismatch but showwal says the checksum agrees |
| `internal/walfile/livecopy.go` `CopyLiveFiles` / `ReadLiveState` docs | edge cases | no pairs → `nil, nil` without a child; `ReadLiveState` removes its temp copies on every return, including errors |
| `ch05-…/experiments/5.4-checkpoint-modes/main.go` package doc (line 3) | "records … busy-timeout waits" | waits are no longer recorded as a result; only `INFO:` elapsed lines |
| `ch05-…/experiments/5.2-wal-replay/main.go` | no comment is wrong, but the open transaction changed | it is now `UPDATE documents SET title = upper(title)` alone; frames 42-60 valid but uncommitted; no checksum mismatch in 5.2 any more |
| `agent_work/ch05/build-notes.md` sections "Experiment 5.2" B/C, "Experiment 5.3" A/B, "Experiment 5.4" (`waited`, BOUND, Darwin note), `ReplayFiles`/`ParseIndex` bullets | superseded | see the appended section "Repair round 1" in build-notes.md (current numbers and rows) |

## Commands run (SQLITE_SRC set to the pinned tree)

| command | result |
| --- | --- |
| `go build ./... && go vet ./... && go test ./...` (merged tree, before changes) | PASS |
| `go run ./cmd/course check all` (merged tree, before changes) | sources, spans, inline, evidence 1.1-2.3 and 5.1-5.4 PASS |
| `gofmt -l internal ch05-* cmd agent_work/ch05` | empty. (`gofmt -l .` lists 5 files under `agent_work/probes/`, which came from main and are not touched here) |
| `go build ./... && go vet ./... && go test ./...` | PASS (walfile incl. 7 new tests) |
| `bash agent_work/verify/crossbuild.sh` | `CROSSBUILD OK` (incl. `check gofmt`: 101 files formatted) |
| 5.1, 5.2, 5.3, 5.4, each run twice | exit 0 each; the two runs are identical outside `INFO:` lines; evidence files rewritten for 5.2, 5.3, 5.4 (5.1 output unchanged, still equal to its evidence) |
| `go run ./cmd/course check all` | every check PASS (sources, spans, inline, evidence for 1.1-1.3, 2.1-2.3, 5.1-5.4: 2 runs identical, gofmt) **except `FAIL check paths: 1 problem(s)`**: `_tools/bin/kb`, a tracked binary main added in c51ff44. Not part of this chunk and not touched (kb is off-limits); it needs an allowlist entry or removal on main |
| `bash agent_work/ch05/adversarial/run-all.sh` | 24 HELD, 4 BROKEN (REP-02, REP-05, CHK-03, CHK-05 — all answered above); INS-05b, REP-07a, REP-07b now HELD; 21 s; output in `adversarial/results.txt` |
| `go run ./agent_work/ch05/repair-probes/readonly-checkpoint` | ro PASSIVE before backfill: `disk I/O error (778)`; after writer PASSIVE: `busy=0 log=38 checkpointed=38` |
| `_tools/bin/showwal` on 5.2's B capture | 60 frames, 0 `should be` lines |
| `sqlite3` (3.53.4, `/usr/local/bin`) on REP-05 replay | 0 rows with the ghost string; 336 rows `-v1-v2`; `integrity_check ok` |

No attack was edited. Attacks judged wrong or beyond the outline: REP-02 (it wants detection the
outline's inputs cannot support), REP-05 (it checks raw bytes, including unused space, that
SQLite itself leaves), CHK-03 (it checks the uncorrected outline wording), CHK-05 part c (it
misreads a result that only reports backfill another connection already did).
