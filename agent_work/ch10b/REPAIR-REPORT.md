# ch10b repair round 1 — report

Inputs: `REVIEW-1.md` (FAIL: 3 blockers, 2 should-fix, 2 nits), `adversarial/results.txt` run 1
(23 HELD, 2 BROKEN: REL-04, REL-05c).

| Finding / attack | Fixed (commit) or answered | Test added |
| --- | --- | --- |
| REVIEW-1 #1 (blocker) / REL-04: rollback with the destination down wedges the relocation | Fixed, `8a4a34e`: `Rollback` records `rolled-back` right after the source's claim, then discards at the destination best effort (`Result.DiscardErr`). Also: a retry after a stop between the claim and the record (source owns the newer epoch) finishes as `rolled back`, not `superseded`. REL-04 now HELD. | `relocate.TestRollbackWhileTheDestinationIsDown`, `TestRollbackFinishesARollbackThatStoppedAfterTheClaim`, `TestRollbackStillReportsAThirdNodesTakeover`; 10.3 case 32 (rollback while b stays down). The first two fail on the old code. |
| REVIEW-1 #2 (blocker) / REL-05c: repeated `Activate` never repairs the catalog | Fixed, `84df1a9`: in the "this node already holds the epoch" branch, when the store ends at the fence, re-record the catalog from the fence snapshot (location, last_shipped P+1, schema_version); left alone when the node shipped past the fence. REL-05c now HELD. | `node.TestRepeatedActivateRepairsAnUnrecordedFence` (fails on the old code), `TestRepeatedActivateAfterLaterWritesKeepsTheCatalog` |
| REVIEW-1 #3 (blocker): 10.2 random kills land after the writes | Fixed, `a66c66e`: `randomKill` returns the write-loop duration (first command to last reply) and the delay is drawn in that window. The `-portable` run then still had 2 of 4 store-durable draws land after the last reply (seed 1 draws ~0.94/0.96 of the window), so a kill whose draw misses is recovered and checked, then redrawn (≤ 5 draws), and a new EXACT check requires every random kill to land mid-stream. Repair runs: 12 of 12 kills landed on draw 1, 6–11 of 12 acknowledged. | Experiment check `each of the 2 random kills landed while writes were in flight …` (no unit test: timing against a live process) |
| REVIEW-1 #4 (should-fix): 10.3 process leak on failed recovery; problems unchecked | Fixed, `a66c66e`: deferred kill of every remaining node process in `runCase` (`verify` clears the ones it stops); `check(len(env.Tracker.Problems) == 0, "no node reported a problem")`. | Experiment check line |
| REVIEW-1 #5 (should-fix): `Drain` drops eviction problems when at-rest was reached | Fixed, `84df1a9`: `Drain` returns an error naming the problems. | `node.TestDrainReportsAFailedAtRestRecord` (fails on the old code) |
| REVIEW-1 #6 (nit): user_version read as one byte | Fixed, `84df1a9`/`8a4a34e`/`a66c66e`: 4-byte big-endian decode in 10.3, `relocate_test.go`, `fence/inspect_test.go`. | — |
| REVIEW-1 #7 (nit): 10.4 "each owns 63" | Fixed, `a66c66e`: prints `62-63` when the split is uneven, a single number otherwise (2,000-database evidence line unchanged). Evidence re-captured. | — |

No finding or attack was judged wrong; no attack was edited. Adversarial run 2: 25 HELD, 0 BROKEN
(`adversarial/results.txt` overwritten by the run).

## Stale prose for Neon

No chapter or comments exist for this chunk's new code yet; no existing comment described changed
code. Facts in `build-notes.md` that changed (corrected in place there, summary in its new section
"Repair round 1"):

- `build-notes.md` invariant table and 10.3 section: 31 → **32** fault cases (`32 of 32 fault cases
  hold every invariant`); new case 32 = node b killed after step 4, rollback while b stays down;
  new output lines `node b is down and stays down during the recovery`, `recovery: the destination
  did not discard its staged copy: node process for "discard db-0001" is not running`, `node b
  restarted after the recovery`; a seventh verify part for case 32; new check `no node reported a
  problem`.
- Resume/rollback rules: rollback now records `rolled-back` before the destination discard; discard
  is best effort (`Result.DiscardErr`); a leftover `.staged` is harmless; a retried rollback after
  the source's claim finishes `rolled back`.
- Step 6 (`Activate`) table row: a repeated activation re-records the catalog when the store ends at
  the fence.
- `Drain` now fails when the final eviction reported problems.
- 10.2 part B: INFO line now `the write loop of an unkilled interactive run … took …` (write loop
  only, ~0.7–0.9 s, not ~1.3–2.2 s); kill lines now `random kill K (draw D) after …`; new check
  `each of the 2 random kills landed while writes were in flight (…at most 5 draws per kill)`; the
  violations check reads `after every kill at a random time …` (was `after each of 2 kills at a
  random time …`).
- 10.4 fleet line: `each owns 62-63` (was `each owns 63`).

## Commands run

- `gofmt -l .`: only pre-existing files under `agent_work/probes/` (not touched); `go run ./cmd/course
  check gofmt`: PASS (271 files).
- `go build ./... && go vet ./... && go test ./...`: PASS.
- New tests against the old code (patch reverted temporarily): `TestRepeatedActivateRepairsAnUnrecordedFence`,
  `TestDrainReportsAFailedAtRestRecord`, `TestRollbackWhileTheDestinationIsDown`,
  `TestRollbackFinishesARollbackThatStoppedAfterTheClaim` FAIL; with the fix PASS.
- `bash agent_work/verify/crossbuild.sh`: CROSSBUILD OK.
- `go run ./cmd/course check paths`: PASS.
- 10.2 ×2: exit 0, EXACT identical; `-portable` ×1: exit 0, `LINUX-ONLY:` for part A, all checks
  passed. Evidence re-captured.
- 10.3 ×2: exit 0, EXACT identical, 32 of 32. Evidence re-captured.
- 10.4 ×2: exit 0, EXACT identical (136 s, 150 s); only change vs. old evidence is the fleet line.
  Evidence re-captured.
- `bash agent_work/ch10b/adversarial/run-all.sh`: exit 0, 25 HELD, 0 BROKEN.
- `go run ./cmd/course check all`: exit 1 — every check PASS (sources, spans, inline, gofmt,
  paths, every experiment's evidence including 10.1–10.4 twice) except two DIFFs outside this chunk:
  ch03 3.3 (`NO-DBHASH`) and ch05 5.1 (`NO-SHOWWAL`), because this worktree had no `_tools/bin`.
  After `go run ./cmd/course build-tools`, `check evidence ch03` and `check evidence ch05` PASS
  (both experiments 2 runs identical to the evidence). Environment only, not a code issue.
