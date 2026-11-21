VERDICT: PASS

All 7 findings from `REVIEW-1.md` (3 blockers, 2 should-fix, 2 nits) and both formerly BROKEN adversarial attacks (`REL-04`, `REL-05c`) are cleanly fixed with no new defects introduced.

## Verification of REVIEW-1 findings and formerly BROKEN attacks

| Finding / attack | Fixed? | How confirmed |
| --- | --- | --- |
| REVIEW-1 #1 (blocker) / `REL-04`: `Coordinator.Rollback` wedged when destination is down | Yes (`8a4a34e`) | Inspected `service/relocate/relocate.go:255-300`: `Rollback` now calls `Catalog.FinishRelocation(..., RelocationRolledBack)` immediately after `source.Claim`, then runs `Destination.Discard` best-effort storing any error in `Result.DiscardErr`; retries after a stop between `source.Claim` and `FinishRelocation` match `reclaimedBySource` (`owner == r.Source && epoch == newest`) and finish as `rolled back` while third-node takeovers still return `ErrSupersededBy`. Verified `Node.Start`/`Stage`/`Claim`/`Retire` treat leftover `.staged` as harmless. Confirmed by `go test -v -race -count=1 ./ch10-multi-database-platform-capstone/service/relocate` (`TestRollbackWhileTheDestinationIsDown`, `TestRollbackFinishesARollbackThatStoppedAfterTheClaim`, `TestRollbackStillReportsAThirdNodesTakeover` all PASS), `10.3-interrupted-relocation` case 32 (`32 of 32`), and `adversarial/relocation-faults` (`REL-04`: `HELD`). |
| REVIEW-1 #2 (blocker) / `REL-05c`: repeated `Node.Activate` skipped `recordShipped` | Yes (`84df1a9`) | Inspected `service/node/relocate.go:187-198`: when `fenceSlot != nil` and `n.owned[id] == epoch`, if `chainEnd(chain) == position+1` it decodes `fenceSlot.Object` and re-runs `n.recordShipped(ctx, id, epoch, position+1, int(meta.UserVersion))`, while leaving the catalog untouched if `b` has already shipped writes past `position+1`. Confirmed by `go test -v -race -count=1 ./ch10-multi-database-platform-capstone/service/node` (`TestRepeatedActivateRepairsAnUnrecordedFence`, `TestRepeatedActivateAfterLaterWritesKeepsTheCatalog` both PASS) and `adversarial/relocation-faults` (`REL-05c`: `HELD`, catalog repaired to `owner=b epoch=2 loc="b:db-0001.db" lastShipped=5 schemaVer=3`). |
| REVIEW-1 #3 (blocker): `10.2` random kills landed after the write loop | Yes (`a66c66e`) | Inspected `experiments/10.2-acknowledged-writes-survive/main.go:142-165,281-307`: `randomKill` now times only the write loop (`writesStarted := time.Now()` after `env.Start` to `time.Since(writesStarted)` after the send loop) and returns `(o, writeElapsed)`. `randomTimeKills` samples `delay` in `(0, writeElapsed)`, checks every draw including misses, redraws up to 5 times per kill, and enforces `check(inFlight == *randomKills, ...)`. Confirmed by running `10.2` default (4/4 kills landed on draw 1 with 8–11 of 12 acked; EXACT matches `evidence/10.2-acknowledged-writes-survive.txt`) and `10.2 -portable` (4/4 kills landed on draw 1 with 8–10 of 12 acked; `LINUX-ONLY:` printed for part A; exit 0). |
| REVIEW-1 #4 (should-fix): `10.3` process leak on failed recovery and unchecked `env.Tracker.Problems` | Yes (`a66c66e`) | Inspected `experiments/10.3-interrupted-relocation/main.go:251-257,332,476`: `runCase` defers killing any non-nil process in `r.nodes` (`verify` clears `r.nodes[name] = nil` after clean stop), and `main` now asserts `check(len(env.Tracker.Problems) == 0, "no node reported a problem")`. Confirmed by running `10.3` (exit 0, `32 of 32 fault cases hold every invariant`, `check ok: no node reported a problem`, EXACT matches `evidence/10.3-interrupted-relocation.txt`). |
| REVIEW-1 #5 (should-fix): `Node.Drain` dropped eviction problems when `atRest` was set | Yes (`84df1a9`) | Inspected `service/node/relocate.go:106-108`: returns an error naming `problems` whenever `len(problems) > 0` even if `n.atRest[id]` was recorded. Confirmed by `go test -v -race -count=1 ./ch10-multi-database-platform-capstone/service/node` (`TestDrainReportsAFailedAtRestRecord` PASS, `a.TakeProblems()` empty). |
| REVIEW-1 #6 (nit): 1-byte `user_version` decode | Yes (`84df1a9`/`8a4a34e`/`a66c66e`) | Inspected `experiments/10.3-interrupted-relocation/main.go:384`, `service/relocate/relocate_test.go:145`, and `service/fence/inspect_test.go:66`; all three decode `binary.BigEndian.Uint32(...[60:64])`. Grepped `ch10-multi-database-platform-capstone` for `\[63\]` — only `service/lab/fleet.go:28` remains, which already decodes all 4 big-endian bytes. |
| REVIEW-1 #7 (nit): `10.4` printed `each owns 63` for uneven 500/8 split | Yes (`a66c66e`) | Inspected `experiments/10.4-rebuild-the-fleet/main.go:354-359`: prints `fewest-most` when uneven (`62-63` for 500/8) and a single count when even. Confirmed by running `10.4` (`fleet: 500 databases, 8 writer processes (each owns 62-63 and keeps at most 32 open)...`, exit 0, EXACT matches `evidence/10.4-rebuild-the-fleet.txt`). |

## New findings

None.

## Adversarial suite status

All 25 attacks in `agent_work/ch10b/adversarial/` are `HELD` (`25 HELD, 0 BROKEN`); no attack needed updating (`run-all.sh` and `README.md` are complete).

## Commands run

- `git diff c8ddc05..HEAD -- '*.go'`: reviewed every Go change from the repair round (zero comments added in new/modified ch10b Go code).
- `go build ./... && go vet ./... && go test ./...`: PASS.
- `bash agent_work/verify/crossbuild.sh`: `CROSSBUILD OK` (`linux/amd64`, `darwin/arm64`, `darwin/amd64`, `check gofmt`).
- `go run ./cmd/course check gofmt` / `check paths` / `check sources ch10` / `check spans ch10`: all PASS.
- `go test -v -race -count=1 ./ch10-multi-database-platform-capstone/service/relocate ./ch10-multi-database-platform-capstone/service/node ./ch10-multi-database-platform-capstone/service/fence`: PASS.
- `go run ./ch10-multi-database-platform-capstone/experiments/10.2-acknowledged-writes-survive`: exit 0, EXACT matches `evidence/10.2-acknowledged-writes-survive.txt`, all 4 random kills landed on draw 1.
- `COURSE_WORK_DIR=/tmp/opencode/ch10b-rereview/10.2-port go run ./ch10-multi-database-platform-capstone/experiments/10.2-acknowledged-writes-survive -portable -prefix rereview-10-2-port`: exit 0, `LINUX-ONLY:` for part A, all 4 random kills landed on draw 1.
- `go run ./ch10-multi-database-platform-capstone/experiments/10.3-interrupted-relocation`: exit 0, `32 of 32 fault cases hold every invariant`, EXACT matches `evidence/10.3-interrupted-relocation.txt`.
- `go run ./ch10-multi-database-platform-capstone/experiments/10.4-rebuild-the-fleet`: exit 0 (1m42s), `each owns 62-63`, EXACT matches `evidence/10.4-rebuild-the-fleet.txt`.
- `bash agent_work/ch10b/adversarial/run-all.sh`: exit 0 (98s), 25 HELD, 0 BROKEN (`agent_work/ch10b/adversarial/results.txt` updated).
