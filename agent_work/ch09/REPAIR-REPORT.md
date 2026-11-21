# Chapter 9 repair round (Opus)

Inputs: `REVIEW-1.md` (FAIL, findings 1-7), `adversarial/results.txt` (run 1: 5 BROKEN).
Code commits: `3e5411e` (walship), `2a4f3a7` (9.1/9.2 experiments and evidence). Notes and this
report are in the commit that follows them.

| Finding / attack | Outcome | Test added |
| --- | --- | --- |
| R1 blocker: resume with no WAL treats offline commits as "unchanged" → corrupt segment | **fixed** `3e5411e`: `classify` (now takes ctx, returns error) reads every page when there is no WAL now and none at the last shipped position and the shipper is not continuous; unchanged only if the pages hash to the last shipped state, else discontinuity snapshot | `TestResumeWithNoWALSeesOfflineCommits` (fails on the old code: "same WAL generation, segment none") |
| R2 should-fix: 9.1 part B checks | **fixed** `2a4f3a7`: (1) `report` checks the exact level set for every capture; (2) the plain copy is checked to be E1 to an older step ≥ 0 and < the committed one; (3) `VACUUM INTO` runs while the writer child holds `BEGIN IMMEDIATE` with `recipe-exec 10` uncommitted, and captures step 9 | experiment checks (9 → 15 checks) |
| R3 should-fix: segment WAL header and commit frames not verified before replay | **fixed** `3e5411e`: `VerifySegmentFrames` parses the 32-byte header with `walfile.Parse` (magic, page size, header checksum, version), requires its page size = `meta.PageSize`, and matches commit frames (non-zero commit size) to `meta.Commits` one-to-one in frame and `Pages`. `ship` now lists commit points before replaying | `TestSegmentHeaderAndCommitFramesAreVerifiedBeforeReplay` |
| R4 nit: tautological "WAL moved under the write lock" check | **fixed** `3e5411e`: removed; the page-hash `ErrMirrorDiverged` check stays | none (removed dead check) |
| R5 nit: `E1-after-VACUUM-INTO` not measured for non-VACUUM captures | **fixed** `2a4f3a7`: measured for every capture (VACUUM INTO of a scratch copy). It exposed a wrong claim: the backup API's canonical form differs from the source's at byte 43 (schema cookie: backup 1 vs source 4, backup.c:448), so backup is `E1-after-VACUUM-INTO no`. New check prints and asserts that | experiment check |
| R6 nit: `Prune` returns unattempted keys on a delete error | **fixed** `3e5411e`: returns `deleted[:i]` | `TestPruneReportsOnlyWhatItDeleted` |
| R7 nit: `DecodeSnapshot` shape checks; case-sensitive WAL check in `Open` | **fixed** `3e5411e`: page size power of two in range, `Pages > 0`, non-empty WAL generation must decode to 32 bytes; `strings.EqualFold` in `Open` | `TestDecodeSnapshotRefusesImpossibleShapes`, `TestOpenAcceptsJournalModeInAnyCase` |
| CAP-02a BROKEN (CLI `.backup` gets "database is locked") | **answered**, no code change: no course code or experiment claims CLI `.backup` never fails; build-notes correction 10 said this was "not reproduced" and gave the cause (a closing last connection's EXCLUSIVE lock in `sqlite3WalClose`). CAP-02a/b/c reproduce it and confirm that cause (0/40 with a persistent connection, deterministic failure under a held EXCLUSIVE lock, `.timeout` fixes it). The attack itself is right; correction 10 in build-notes is updated to "reproduced by the adversarial suite" | none (CLI behaviour, no course code) |
| CAP-03a BROKEN (same-connection write + ROLLBACK restarts the backup) | **answered**, no code change: the experiment claims and checks only same-connection **commits** (copied 5,10,15,20,25), which hold. A rollback restarting the backup is already in build-notes (`sqlite3BackupRestart` called on a WAL rollback, pager.c:3185). The attack expected "operations never restart", which nothing claimed. Correction 1 in build-notes now says "commits, not operations" and cites CAP-03a/b | none |
| CRASH-06 BROKEN (9.2 NORMAL rule "cut at 32" wrong when a reader prevents the WAL restart) | **fixed** `2a4f3a7`: real defect in the portable rule. New rule: if a checkpoint copied frames and the WAL kept its generation, the cut is the WAL end at that checkpoint (its WAL sync, wal.c:2277). Also new fact: a partial checkpoint does not sync the database file (wal.c:2326-2334), so the rule predicts unsynced database writes there. 9.2 gains a fourth model run (NORMAL, reader from step 5): strace agrees (cut 304912, database file 0 syncs / 15 writes after), model recovers step 7, and the same with the pre-checkpoint database file also recovers step 7 | experiment checks (47 → 53 checks) |
| SHIP-03d BROKEN (commit point on a non-commit frame passes Plan) | **fixed** with R3 | covered by `TestSegmentHeaderAndCommitFramesAreVerifiedBeforeReplay` |
| SHIP-03f BROKEN (damaged latest segment → silent resume at an older position) | **fixed** `3e5411e`: `resume` refuses with `ErrBrokenChain` when any damaged object's key position is past the last usable position | `TestResumeRefusesADamagedObjectPastTheLastPosition` |

All six new test functions were also run against the unrepaired code (a scratch worktree at
`563b6ee` with only `repair_test.go` added): every one fails there and passes now.

## Stale prose for Neon

Neon has not written comments or a chapter for ch09 yet, so no comment or chapter text is stale.
`build-notes.md` is updated in place. The facts that changed:

- **Correction 1 (9.1 backup restart):** same-connection *commits* do not restart a backup;
  a same-connection *rollback* does (pager.c:3185; CAP-03a/b).
- **Correction 3 / 9.1 part A table:** backup API `E1-after-VACUUM-INTO` is **no**. The canonical
  forms differ only at byte 43, the schema cookie (backup 1, source 4; canonical 2 vs 5), because
  backup.c:448 sets the destination cookie to its old value + 1. Backup reaches E2, not
  "E1 canonical". New evidence lines: `backup API after VACUUM INTO vs the source after VACUUM
  INTO: … (bytes differ only at [43])`, `schema cookie: backup 1, source 4`.
- **9.1 "E1 after VACUUM INTO" definition:** now measured for every capture (VACUUM INTO of a
  scratch copy), not copied from E3.
- **9.1 part B:** VACUUM INTO now runs while the writer holds an uncommitted write transaction
  (step 10) and captures step 9; summary "blocks the writer" now says "no (ran while the writer
  held its write transaction)". The plain-copy line now reads `… equals recipe step 6 (the writer
  has committed 7)`. Exact-level checks on every capture. 15 checks; evidence 104 lines; writer
  action numbers shifted (backup commits are writer steps 7-15).
- **Correction 4 / 9.2 rule:** the NORMAL cut is not always byte 32 (see CRASH-06 row). New fourth
  model run and its numbers are in the 9.2 section; the three original runs print a new line
  `the checkpoint copied 74 of 74 frames; … same WAL generation: false`; check labels now start
  with `synchronous=<level>`. 53 checks; evidence 126 lines.
- **Correction 10:** the CLI `.backup` "database is locked" is reproduced by CAP-02a (12/40 and 9/40
  in two runs) and its cause confirmed by CAP-02b/c. It is still not a course experiment.
- **walship:** `VerifySegmentFrames` also validates the WAL header and commit frames; `DecodeSnapshot`
  shape checks; `Open` journal mode is case-insensitive; resume detects offline commits with no
  WAL (page hash) and refuses a damaged object past the last usable position; `Prune` on a failed
  delete returns only what it deleted; the periodic-snapshot "WAL moved" check is removed (it
  could not fail); `ship` lists commit points before replaying.
- **BUILD-REPORT.md** (builder's file, not edited): line 18-19's "verify the whole chain before
  any page is applied" is now true for headers and commit points as well; lines 68/73 ("not
  reproduced") are superseded by the CAP-02 results.

## Commands run

| Command | Result |
| --- | --- |
| `gofmt -l .` | no repair-round file listed; the only output is 5 pre-existing files under `agent_work/probes/` (not touched); `course check gofmt`: PASS, 150 tracked Go files |
| `go build ./... && go vet ./...` | PASS |
| `go test ./...` | PASS (walship 25.8 s, includes the 6 new test functions) |
| new tests against the unrepaired code (scratch worktree at `563b6ee` + `repair_test.go`) | all 6 test functions FAIL there, as intended |
| `bash agent_work/verify/crossbuild.sh` | `CROSSBUILD OK` (darwin/arm64 + linux/amd64 build and vet; darwin: cross-built only) |
| 9.1, 9.2, 9.3, 9.4, each run twice | all exit 0; the two runs are identical; 9.1/9.2 evidence rewritten (104 / 126 lines), 9.3/9.4 unchanged and still identical to their evidence |
| `go run ./ch09-…/9.2-crash-versus-power-loss -portable` | `LINUX-ONLY:` lines (4 model runs + kill sweep), `CRASH VERSUS POWER LOSS OK` |
| `go run ./cmd/course check all` | exit 1. ch09: sources/spans pass and all 4 experiments PASS (2 runs identical to the evidence). Failures not from this round: (a) `check paths`: `_tools/bin/kb`, a binary still tracked on this branch (main stopped tracking it in `a15147b`; the merge fixes it); (b) `inline -check` ch02: 3 include blocks differ from `internal/dbfile/record.go`, `internal/equal/equal.go`, `internal/capture/capture.go`, none changed by this round; (c) 3.3 and 5.1 DIFF: `dbhash`/`showwal` are not built in this worktree (`NO-DBHASH`/`NO-SHOWWAL`), so it's the environment |
| `bash agent_work/ch09/adversarial/run-all.sh` (run 2, `results.txt`) | exit 0; 37 HELD, 3 BROKEN. SHIP-03d and SHIP-03f now HELD (Plan rejects "frame 1 has commit size 0…"; Open fails with "damaged object segments/000000000008 past the last usable position 7"). Still BROKEN: CAP-02a and CAP-03a (answered above: they disprove claims the course does not make). CRASH-06 is still BROKEN because **the attack itself is now wrong**: it compares strace against a hardcoded 32 (`copyFile(... walfile.HeaderSize)`, `FirstUnsynced == walfile.HeaderSize`) and never calls 9.2's rule, so it cannot change. 9.2's corrected rule gives 304912 for this workload, which matches strace in 9.2's new fourth run |
