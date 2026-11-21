VERDICT: FAIL

1. **blocker** — `ch10-multi-database-platform-capstone/service/relocate/relocate.go:277-285`
   - **What is wrong:** `Coordinator.Rollback` executes `source.Claim(ctx, id)` (line 277) — which atomically bumps `catalog.databases` back to `owner=source, epoch=to_epoch+1` and puts the new epoch's fence snapshot in S3 — *before* calling `c.Node(r.Destination).Discard(ctx, id)` (line 281) and *before* calling `c.Catalog.FinishRelocation(ctx, id, catalog.RelocationRolledBack)` (line 285). When the destination node is down during rollback (a primary operational reason to roll back a relocation), `Destination.Discard` fails and `Rollback` returns an error without marking the relocation finished. This leaves the control plane wedged: `catalog.databases` already has `owner=a, epoch=3`, `relocations.state` stays `running` (so `BeginRelocation` for a replacement target fails with `catalog: a relocation of the database is already running`), and retrying `Rollback` later sees `newest (3) > r.ToEpoch (2)` and finishes as `superseded` (`ErrSupersededBy`) while leaving the destination's `.staged` file orphaned. Experiment `10.3-interrupted-relocation` missed this because `runCase` (`10.3/main.go:273-278`) always restarts dead nodes before calling `recovery.Rollback`.
   - **How confirmed:** Code inspection of `service/relocate/relocate.go:254-286` and `experiments/10.3-interrupted-relocation/main.go:273-284`, and `go run ./agent_work/ch10b/adversarial/relocation-faults` (`REL-04`):
     ```
     ATTACK REL-04 | kill destination b after step 4 (Rebuild) and call Rollback while b remains down | EXPECT Rollback succeeds (a reclaims cleanly, relocations.state=rolled-back, and a->c relocation can start) | GOT rbErr=relocate: roll back db-0001: discard at the destination: node process for "discard db-0001" is not running catOwner=a/e3 relState=running beginNextErr=catalog: a relocation of the database is already running: db-0001 retryRollback=(superseded,relocate: a newer epoch than the relocation's is in the store: epoch 3, relocation epoch 2) bStagedOrphaned=true | BROKEN
     ```
   - **Fix expected:** Once `source.Claim` succeeds, `source`'s new fence in S3 already prevents the destination from activating `r.ToEpoch`, so `.staged` on the destination is harmless local garbage. Call `FinishRelocation(ctx, id, catalog.RelocationRolledBack)` immediately after `source.Claim` succeeds, and make `Destination.Discard` best-effort (do not fail rollback or leave `relocations.state = running` if the destination is unreachable). Add a unit test in `service/relocate/relocate_test.go` (and ideally a 10.3 case) rolling back while the destination remains down.

2. **blocker** — `ch10-multi-database-platform-capstone/service/node/relocate.go:184-188,236-242`
   - **What is wrong:** `Node.Activate` writes `<id>.owner` (`writeBelief`, line 236), sets `n.owned[id] = epoch` (line 241), and only then calls `n.recordShipped(ctx, id, epoch, shipped, int(userVersion))` (line 242). If `recordShipped` fails (e.g., `SQLITE_BUSY` on `catalog.db`) — or if the destination node crashes between `writeBelief` (line 236) and `recordShipped` (line 242) and restarts (`Start` reloads `n.owned[id] = epoch` from `<id>.owner`) — then when `Resume` → `adopt` retries `b.Activate`, lines 185–188 see `fenceSlot != nil` and `n.owned[id] == epoch` and return `Activated{Fence: position+1, Adopted: true}, nil` without calling `n.recordShipped`. `Resume` then marks the relocation `done` while `catalog.databases` permanently retains `location=""`, stale `last_shipped`, and stale `schema_version` until a future write occurs on `b`.
   - **How confirmed:** Code inspection of `service/node/relocate.go:184-196,236-242`, and `go run ./agent_work/ch10b/adversarial/relocation-faults` (`REL-05c`):
     ```
     ATTACK REL-05c | fail b.Activate at recordShipped (SQLITE_BUSY) after PutIfAbsent+writeBelief, then unlock catalog and Resume | EXPECT Resume completes (state=done) and repairs catalog to owner=b epoch=2 location=b:db-0001.db last_shipped=5 schema_version=3 | GOT failedAtActivate=true resume=(rolled forward,<nil>) relState=done cat=(owner=b epoch=2 loc="" lastShipped=3 schemaVer=2) | BROKEN
     ```
   - **Fix expected:** In the `fenceSlot != nil && owned == epoch` branch (lines 185–188), check the catalog row (or read `user_version` from the decoded fence snapshot) and call `n.recordShipped` whenever `record.Location == ""` or `record.LastShipped < position+1` (taking care not to overwrite `last_shipped`/`schema_version` backwards if `b` has already shipped writes past `position+1`, as in 10.3 cases 7 and 19). Add a test in `service/node/relocate_test.go` covering `Activate` retry after a failed `recordShipped` or restart between `writeBelief` and `recordShipped`.

3. **blocker** — `ch10-multi-database-platform-capstone/experiments/10.2-acknowledged-writes-survive/main.go:142-163,279-298`
   - **What is wrong:** `randomTimeKills` measures `elapsed` around the entire `x.randomKill(mode, time.Hour)` call (lines 280–282), which includes `x.setup()` (wiping `_work/10.2/`, batch-wiping S3, creating the catalog, starting node `a`, claiming 4 databases with 4 S3 fence puts, and stopping node `a`), `x.env.Start()`, the 12 writes, and `Reap()`. Inside `randomKill` (lines 143–148), however, `time.AfterFunc(delay, ...)` starts *after* `x.setup()` and `x.env.Start()`. Because `setup()` accounts for ~50–70% of `elapsed`, `delay := time.Duration(rng.Int64N(int64(elapsed)))` frequently exceeds the entire write loop: all 12 writes complete before the timer fires, `timer.Stop()` cancels it, and line 158 kills an idle process while the check still prints `check ok: after each of 2 kills at a random time...`. In the committed `evidence/10.2-acknowledged-writes-survive.txt`, **0 of 2** `store-durable` random kills interrupted the writes; in `-portable` mode (the sole crash suite on macOS), **4 of 4** random kills completed all 12 writes before the timer fired.
   - **How confirmed:** Code inspection of `10.2/main.go:142-163,279-298`, stored `evidence/10.2-acknowledged-writes-survive.txt:32-34`, two default runs, and a `-portable` run:
     ```
     # Stored evidence (lines 32-34):
     INFO: store-durable, an unkilled interactive run of 12 writes took 2.246s (12 acknowledged)
     INFO: store-durable, random kill 1 after 2.153s: 12 acknowledged; reopened 4; recovered 3,3,3,3; acknowledged 3,3,3,3
     INFO: store-durable, random kill 2 after 2.102s: 12 acknowledged; reopened 4; recovered 3,3,3,3; acknowledged 3,3,3,3

     # Fresh -portable run (all 4 random kills missed the write window):
     INFO: local-durable, an unkilled interactive run of 12 writes took 2.073s (12 acknowledged)
     INFO: local-durable, random kill 1 after 1.29s: 12 acknowledged; ...
     INFO: local-durable, random kill 2 after 1.166s: 12 acknowledged; ...
     INFO: store-durable, an unkilled interactive run of 12 writes took 1.335s (12 acknowledged)
     INFO: store-durable, random kill 1 after 1.279s: 12 acknowledged; ...
     INFO: store-durable, random kill 2 after 1.249s: 12 acknowledged; ...
     ```
   - **Fix expected:** Measure only the write-loop duration (from right after `x.env.Start` until the last `Send` returns, excluding `x.setup()` and `Reap()`) and return it from `randomKill`. Sample `delay` inside `(0, writeElapsed)` so random kills actually land inside the write loop, and update the `INFO:` line so it reports the actual write-loop duration rather than `setup + writes`.

4. **should-fix** — `ch10-multi-database-platform-capstone/experiments/10.3-interrupted-relocation/main.go:285-288,442-448`
   - **What is wrong:** Two issues in `10.3/main.go`:
     1. **Process leak on failed recovery (`lines 285-288`):** `runCase` starts child node processes `a` and `b` (line 250) and stops them only inside `r.verify` (lines 303–307). If `recovery.Rollback` or `recovery.Resume` returns an error (line 287), `runCase` returns `false` without stopping or killing `r.nodes`, leaking live node processes holding open files under `_work/10.3/` across the next case's `env.Reset`.
     2. **Unchecked node problems (`lines 442-448`):** Unlike `10.2/main.go:382` and `10.4/main.go:433`, `10.3/main.go` prints `env.Tracker.Problems` (lines 442–444) but never asserts `len(env.Tracker.Problems) == 0`. A node reporting a background `problem ...` line during eviction or close would not fail the experiment.
   - **How confirmed:** Code inspection of `10.3/main.go:239-307,442-454` compared with `10.2/main.go:376-384` and `10.4/main.go:427-435`.
   - **Fix expected:** Add a `defer` in `runCase` that kills and reaps any remaining non-nil processes in `r.nodes` (clearing `r.nodes[name] = nil` when `verify` stops them cleanly), and add `check(len(env.Tracker.Problems) == 0, "no node reported a problem")` alongside the bounds check (re-capturing `evidence/10.3-interrupted-relocation.txt`).

5. **should-fix** — `ch10-multi-database-platform-capstone/service/node/relocate.go:95-106`
   - **What is wrong:** `Node.Drain` saves `before := len(n.problems)`, calls `n.evictIfOpen(id)`, copies `n.problems[before:]`, and truncates `n.problems = n.problems[:before]`. In `Node.evicted` (`service/node/node.go:328-341`), `n.atRest[id]` is set *before* `n.writeFile(id, ".atrest", ...)` and *before* `n.recordShipped(...)`. If either of those fails, `evicted` appends to `n.problems` while `n.atRest[id]` is already set; `Drain` then sees `ok == true` on line 102 and returns `nil`, silently discarding those problems so neither the caller nor `TakeProblems()` ever sees them.
   - **How confirmed:** Code inspection of `service/node/relocate.go:95-106` and `service/node/node.go:321-341`.
   - **Fix expected:** Treat `len(problems) > 0` as a drain error (or do not drop `n.problems[before:]` when returning `nil`).

6. **nit** — `ch10-multi-database-platform-capstone/experiments/10.3-interrupted-relocation/main.go:358` (also `service/relocate/relocate_test.go:144`, `service/fence/inspect_test.go:64`)
   - **What is wrong:** Reads SQLite's 4-byte big-endian `user_version` header field (bytes 60–63) as a single byte `int(restore(ack.Position)[63])`, which silently truncates values `>= 256` mod 256 and sets a misleading example for readers copying experiment code (compare `service/node/node.go:355` and `service/lab/fleet.go:28`).
   - **How confirmed:** Code inspection of `10.3/main.go:358`, `service/relocate/relocate_test.go:144`, `service/fence/inspect_test.go:64`.
   - **Fix expected:** Decode `binary.BigEndian.Uint32(img[60:64])` (or call `env.MatchImage(restore(ack.Position))` in `10.3/main.go:358`).

7. **nit** — `ch10-multi-database-platform-capstone/experiments/10.4-rebuild-the-fleet/main.go:354`
   - **What is wrong:** Prints `fleet: 500 databases, 8 writer processes (each owns 63 and keeps at most 32 open)` using `len(f.shares[0])`; because `500 / 8 = 62.5`, four writers own 63 and four own 62 (`8 * 63 = 504 != 500`).
   - **How confirmed:** Code inspection of `10.4/main.go:349-355` and `evidence/10.4-rebuild-the-fleet.txt:4`.
   - **Fix expected:** Change the wording to `each owns up to %d` (or show min–max when uneven).

## Notes on outline corrections and conventions

- All 9 outline corrections in `agent_work/ch10b/build-notes.md` were checked against the code, the pinned SQLite 3.53.4 source (`src/wal.c:1390-1614` `walIndexRecover`, `src/wal.c:4038-4266` `walFrames`, `src/wal.c:2507-2571` `sqlite3WalClose`), and live runs; all 9 corrections are factually accurate.
- Conventions checked: zero comments in newly added Go files, WAL default throughout, no cgo, no machine-local paths outside `agent_work/`, `darwin/arm64` and `linux/amd64` build+vet pass, `SPANS.txt` (12 spans) and `SOURCES.sha256` pass, `10.1`–`10.4` EXACT output matches stored evidence and is identical across two consecutive runs.

## Commands run

- `docker compose ps`: SeaweedFS 4.48 (`sqlite-internals-seaweedfs-1`) up on port 18333.
- `go run ./cmd/course check sources ch10`: `PASS check sources ch10-multi-database-platform-capstone: 1 copied file(s) match the pin`.
- `go run ./cmd/course check spans ch10`: `PASS check spans ch10-multi-database-platform-capstone: 12 span(s) start at their symbol`.
- `go run ./cmd/course check paths`: `PASS check paths: 339 tracked file(s) free of machine-local paths`.
- `go run ./cmd/course check gofmt`: `PASS check gofmt: 271 Go file(s) formatted`.
- `GOOS=darwin GOARCH=arm64 go build ./... && GOOS=darwin GOARCH=arm64 go vet ./...`: PASS.
- `GOOS=linux GOARCH=amd64 go build ./... && GOOS=linux GOARCH=amd64 go vet ./...`: PASS.
- `go test -race -count=1 ./ch10-multi-database-platform-capstone/...`: PASS (all 9 service packages, S3 tests executed against SeaweedFS).
- `git diff efad893..HEAD --name-only --diff-filter=A -- 'ch10-multi-database-platform-capstone/*.go' 'ch10-multi-database-platform-capstone/**/*.go' | xargs grep -nE '(//|/\*)'`: no matches (zero comments in new Go files).
- `go run ./ch10-multi-database-platform-capstone/experiments/10.1-one-writer`: exit 0, EXACT output matches `evidence/10.1-one-writer.txt`.
- `go run ./ch10-multi-database-platform-capstone/experiments/10.2-acknowledged-writes-survive` (2 runs): exit 0 both runs, EXACT output identical across runs and matches `evidence/10.2-acknowledged-writes-survive.txt`; `INFO:` lines showed 3 of 4 random kills in each run finished all 12 writes before the timer fired.
- `COURSE_WORK_DIR=/tmp/opencode/review-10-2-port go run ./ch10-multi-database-platform-capstone/experiments/10.2-acknowledged-writes-survive -portable -prefix review-10-2-port`: exit 0, prints `LINUX-ONLY:` for part A; 4 of 4 random kills finished all 12 writes before the timer fired.
- `COURSE_WORK_DIR=/tmp/opencode/review-10-2-mut go run ./ch10-multi-database-platform-capstone/experiments/10.2-acknowledged-writes-survive -mutant ack-before-put -syscall-kills 2 -random-kills 1 -prefix review-10-2-mut`: exit 1, `2 check(s) FAILED` (violations detected).
- `go run ./ch10-multi-database-platform-capstone/experiments/10.3-interrupted-relocation` (2 runs): exit 0 both runs, EXACT output identical across runs and matches `evidence/10.3-interrupted-relocation.txt`.
- `go run ./ch10-multi-database-platform-capstone/experiments/10.3-interrupted-relocation -mutant skip-fence -case 1`: exit 1, `1 check(s) FAILED` (two writable nodes + broken lineage detected).
- `go run ./ch10-multi-database-platform-capstone/experiments/10.4-rebuild-the-fleet` (2 runs, default 500 databases): exit 0 both runs (~2m30s each), EXACT output identical across runs and matches `evidence/10.4-rebuild-the-fleet.txt`.
- `go run ./agent_work/ch10b/adversarial/relocation-faults`: confirmed `REL-04` (`BROKEN`) and `REL-05c` (`BROKEN`).
