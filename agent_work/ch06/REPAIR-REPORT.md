# Chunk 6 — repair round 1 (Opus)

Inputs: `REVIEW-1.md` (VERDICT: FAIL, findings 1-6), `adversarial/results-run1.txt` (43 HELD, 6 BROKEN).
Result after repair: every review finding fixed. Of the six BROKEN attacks, BUSY-09 and PROCS-02c now
HELD. LOCK-04b and LOCK-06 found real errors in the outline claim; the experiment now shows what is
true, but those two attacks still print BROKEN because they test the old claim. BUSY-05b and SNAP-04
are answered without code changes. Adversarial re-run: 45 HELD, 4 BROKEN (those four).

Commits: `c2ab36f` procs fixes + 6.1 `/proc/locks` checks + 6.2 part E; `3a7ccf7` 6.1 parts C and D;
the report/notes commit is the one printed as `REPAIR-DONE`.

| finding / attack | fixed (commit) or answered (why) | test added |
| --- | --- | --- |
| Review 1 (blocker): `StillWaiting` was a 0 ns poll; 6.2 E waiter not contending | fixed `c2ab36f`: `Call.AnsweredWithin(d)` (bounded wait, never kills the child); `Lab.StillWaiting` waits `StillWaitingWindow` = 250 ms and prints `.. no answer after 250ms`; 6.2 part E sends `begin immediate` as the waiting call and runs `recipe-exec k+1` + `commit` after it is admitted. Adversarial BUSY-09 strace now counts 86 EAGAIN on byte 120 (was 15-24). | `TestAnsweredWithinWaitsWithoutKilling`, `TestLabStillWaitingCanFail`, `TestStillWaitingSeesImmediateBusy` (real SQL children, `busy_timeout=0` → reported as answered, BUSY 5) |
| Review 2 (should-fix): 6.1 "both processes hold SHARED" not provable by F_GETLK; `/proc/locks` never asserted | fixed `c2ab36f`: `Lab.Locks` returns `LockView{Ranges, Proc, ProcListed}` with `ProcHolders(range)`; 6.1 adds 11 `/proc/locks:` checks (Linux only) for holder/contender on RESERVED, SHARED, WAL_WRITE_LOCK, DMS before and after each stray close; the F_GETLK database-file check is reworded to what it proves | `TestLockViewProcHolders` |
| Review 3 (should-fix): copier `copy-range` wrote zero padding after a short read | fixed `c2ab36f`: `readRange` returns `buffer[:n]`; answer reports `bytes START-(START+n)` | `TestReadRangeKeepsOnlyBytesRead` |
| Review 4 (nit): `/proc/locks` parser rejected valid host lines | fixed `c2ab36f`: accepts `<none>:0`, mode `MSNFS`/`UNKNOWN`, class `UNKNOWN`, type `NONE` | `TestParseProcLocksHostVariants` (5 accepted, 3 bad `<none>` lines rejected) |
| Review 5 (nit): `Stop` left the child running on a non-timeout error; `readReplies` could leak | fixed `c2ab36f`: `Stop` calls `Kill` on any error from the exit answer; `Kill` closes a `killed` channel that `readReplies` selects on | `TestStopKillsChildWhenExitAnswerIsWrong`. No test for the goroutine leak: draining the channel in a test would unblock the old code too, so a test could not tell the two apart |
| Review 6 (nit): build-notes said 6.3 opens copies after the children closed | fixed in build-notes (6.3 section): part A opens them while the children run, part B after; the copies are separate files | — (notes only) |
| LOCK-06 (BROKEN) | **real; outline claim corrected** `3a7ccf7`: new 6.1 part C. If the WAL already holds frames, the holder reads through WAL_READ_LOCK(1). `walRestartLog` is skipped (wal.c:3879), both COMMITs return SQLITE_OK, and the second process's commit is overwritten. The still-open second process then reads `seed,holder`. The attack still prints BROKEN because it expects the old SQLITE_PROTOCOL claim. | experiment checks (6.1 C, 7 checks) |
| LOCK-04b (BROKEN) | **real; outline claim corrected** `3a7ccf7`: new 6.1 part D reproduces it with Go children. The second process exits first and closes as the last connection, deleting `-wal` and `-shm`. The holder's COMMIT then returns SQLITE_OK and only its row survives. The CLI in the attack always exits after its batch, which is why it hit this case. The attack still prints BROKEN for the same reason as LOCK-06. | experiment checks (6.1 D, 5 checks) |
| BUSY-09 (BROKEN) | fixed with review 1 → now HELD (`still_waiting_on_timeout0=false`, `eagain_on_byte120=86`) | as review 1 |
| PROCS-02c (BROKEN) | fixed with review 4 → now HELD (`parsed=3/3`) | as review 4 |
| BUSY-05b (BROKEN) | **answered, no code change.** The attack tests a boundary case the outline never covers: with `PRAGMA locking_mode=EXCLUSIVE`, a WAL writer takes and keeps the database file's EXCLUSIVE lock (pager.c:5982-5988). No experiment uses that mode; the claim is for the default `locking_mode=NORMAL`. Recorded as build-notes correction 6 so the chapter qualifies it once. | — |
| SNAP-04 (BROKEN) | **answered: the attack's salt-1 +1 expectation is wrong; every other part HELD.** At step 6, 22 frames ≥ autocheckpoint 20 trigger a full checkpoint. The capture then begins on a fully checkpointed WAL and takes WAL_READ_LOCK(0). The writer's step 7 restarts the WAL under it (salt-1 +1, nBackfill 0, which explains `pinned_nBackfill=0` against `startMx=22`). Step 19 restarts it again (+1), so the total is +2. Confirmed by `agent_work/ch06/probes/snap04-restart` (`after step 7 mxFrame=16 salt1=…dd nBackfill=0`). The capture stays E1-exact because a READ(0) reader blocks all backfill (wal.c:2259-2260). Recorded as build-notes correction 7, with its impact on OUTLINE R6. | probe (working script) |

## Stale prose for Neon

Neon has written no comments or chapter yet, so nothing in a `.go` comment or chapter file is stale. The
facts in `build-notes.md` that changed are listed below; build-notes.md has been corrected in place,
with each change marked **Repair round**:

- `build-notes.md`, correction 1 (WAL bullet): "the holder was told" holds only for part B, where
  the holder's transaction began on an empty or fully checkpointed WAL. Part C (WAL already holding
  frames) and part D (the other process exits first) lose a commit that returned SQLITE_OK, and
  neither process gets an error.
- New corrections 6 (`locking_mode=EXCLUSIVE` boundary) and 7 (a READ(0) reader does not stop a
  WAL restart; relevant to OUTLINE R6 "holds a read mark so the WAL cannot restart").
- `internal/procs` API: `Call.AnsweredWithin`; `Lab.StillWaiting` waits 250 ms and prints
  `.. no answer after 250ms`; `Lab.Locks` returns `LockView` / `ProcHolders`. The parser now accepts
  host variants. `copy-range` reports the bytes it actually read. `Stop` kills the child on any
  failed exit, and the reply reader stops on kill. The test list is extended.
- 6.1: runtime ~12 s, evidence 204 lines, 43 checks on Linux (32 elsewhere). The database-file
  F_GETLK check is reworded. Parts C (steps 21-30) and D (steps 31-38) are new, and the `/proc/locks:`
  checks are listed.
- 6.2: evidence 145 lines, runtime ~5 s. Part E's waiting call is `begin immediate` (then
  `recipe-exec`, `commit`), and the transcript says `no answer after 250ms` (not `no answer yet`). The
  "limit of the still-waiting evidence" paragraph has been rewritten.
- 6.3: the copies in part A are opened before the children stop (review 6).
- Platform implications 3 and 7, and the WAL-default table row "Outcome of broken locks".
- Span facts: added `walRestartLog`'s `readLock==0` guard (3879), `walRestartHdr` 2152-2166, the
  backfill READ(0) lock 2254-2260 and `sqlite3WalClose` 2507-2571. All four are new entries in
  `SPANS.txt` (36 spans, `check spans ch06` PASS).
- 6.1 "Neon's list not built" now points to the adversarial LOCK-03/LOCK-04 coverage.

## Commands run

| command | result |
| --- | --- |
| `gofmt -l internal ch06-locking-and-concurrency agent_work/ch06` | clean (whole-tree `gofmt -l .` also lists 5 pre-existing files under `agent_work/probes/`, not this chunk's) |
| `go build ./... && go vet ./... && go test ./...` | pass (`internal/procs` ~3.4 s) |
| `bash agent_work/verify/crossbuild.sh` | `CROSSBUILD OK` |
| `go run ./cmd/course check all` | every check PASS except two that come from this worktree's environment, not this change: (1) `DIFF 5.1-wal-inspector` (`NO-SHOWWAL`: this worktree has no `_tools/bin/showwal`); rerun with `COURSE_TOOLS_DIR` pointing at a pinned build of the tools (`check evidence ch05 -runs 2`) → all four ch05 PASS. (2) `FAIL check paths: _tools/bin/kb` — that binary is tracked on this branch since c51ff44, and main stopped tracking it in a15147b; it goes away when main is merged. All four ch06 experiments PASS ("2 runs identical to the evidence"), and so do sources, spans and gofmt. |
| 6.1 twice | exit 0 both, EXACT identical, `LOCKS BELONG TO THE PROCESS OK`, ~12 s each; evidence updated |
| 6.2 twice | exit 0 both, EXACT identical, `BUSY RULES OK`, ~5 s each; evidence updated |
| 6.3 twice | exit 0 both, identical, unchanged from stored evidence (`TORN COPY OK`) |
| 6.4 twice | exit 0 both, identical, unchanged from stored evidence (`LIVE SNAPSHOT OK`) |
| `go run ./cmd/course check spans ch06` | PASS, 36 spans |
| `bash agent_work/ch06/adversarial/run-all.sh` | 129 s; 45 HELD, 4 BROKEN (LOCK-04b, LOCK-06, BUSY-05b, SNAP-04, each answered above); `results.txt` is this run (run 1 is kept in `results-run1.txt`) |
| `go run ./agent_work/ch06/probes/snap04-restart` | `SNAP04 PROBE OK`; output quoted in build-notes correction 7 |

Attacks that are themselves wrong: SNAP-04 (salt-1 must be +2, not +1, because the WAL restarts under a
READ(0) reader). LOCK-04b and LOCK-06 are correct attacks against a claim that turned out false. Their
EXPECT lines encode the old claim, so they will keep printing BROKEN until Neon updates them.
BUSY-05b tests a mode the outline does not cover.
