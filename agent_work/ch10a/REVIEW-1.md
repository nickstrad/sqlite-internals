VERDICT: FAIL

## Findings

1. **[blocker] `ch10-multi-database-platform-capstone/service/node/node.go:236-295` (`Node.Claim`) — stale `n.atRest[id]` breaks reopening after reclaim**
   - **What is wrong:** `Node.Claim` evicts `id` if open and deletes `n.fenced[id]` and `n.owned[id]`, but never deletes `n.atRest[id]`. If `id` was previously evicted cleanly on node `a` (recording `n.atRest[id] = sha256(old)`), then taken over and modified by node `b`, and later reclaimed by node `a`, `Claim` restores the newer image from the store and succeeds — but the next `Write`, `Read`, or `Ship` on `a` fails inside `openDatabase` (`node.go:134-141`) with `ErrChangedWhileShut` because `n.atRest[id]` still holds the pre-takeover digest. (`TestEvictionShipsAndVerifiesTheFileAtRest` missed this because nobody advanced `db-2` in the store before `a` reclaimed it, so the restored image had the same hash as the stale `n.atRest` entry.)
   - **How confirmed:** Built `10.1-one-writer` and drove two `-role node` processes against SeaweedFS (`capacity 1` on `a`): `a` claimed and wrote `db-1` then `db-2` (evicting `db-1`), `b` claimed and wrote `db-1` (advancing to position 4), then `a` reclaimed `db-1` and tried to write:
     ```
     ok claim db-1: epoch 3, restored position 4, fence at position 5, attempts 1
     refused write db-1 store-durable: error: connmgr: open db-1: the local database file changed while it was closed: db-1: at rest, 118784 bytes, sha256 69490a2d...
     ```
   - **Expected fix:** Call `delete(n.atRest, id)` in `Node.Claim` (alongside `delete(n.fenced, id)` and `delete(n.owned, id)`, and/or when writing the restored database image), and add a regression test where node `a` evicts `db-1`, node `b` claims and writes `db-1`, and node `a` reclaims and writes `db-1`.

2. **[blocker] `ch10-multi-database-platform-capstone/service/node/node.go:159-161,179-181` (`openDatabase`, `evicted`) — shipping during eviction or reopen never updates `Catalog.RecordShipped`**
   - **What is wrong:** Both `openDatabase` (line 159: `if _, err := shipper.Cycle(ctx)`) and `evicted` (line 180: `_, shipErr = c.shipper.Cycle(context.Background())`) ship newly committed frames or discontinuity snapshots to the store and discard the `CycleReport` without calling `n.config.Catalog.RecordShipped`. Consequently, after a clean eviction or `Node.Close()` of a database with `LocalDurable` writes (or after a post-crash reopen followed by a `Read` or `LocalDurable` write), the store holds the shipped position and new `user_version`, while the catalog permanently reports the older `last_shipped` and `schema_version`.
   - **How confirmed:** Ran `claim db-1`, `write db-1 local-durable`, `close` on a `-role node` process against SeaweedFS, then queried `catalog.db` and had node `b` claim `db-1`:
     ```
     ok claim db-1: epoch 1, restored position 0, fence at position 1, attempts 1
     ok write db-1 local-durable: acknowledged at epoch 1, user_version 1, committed locally (not yet in the store)
     ok close
     catalog row after close:
     db-1|a|1|1|0|a:db-1.db
     check what node b restores from store on claim:
     ok claim db-1: epoch 2, restored position 2, fence at position 3, attempts 1
     ```
     The store held position 2 (`user_version` 1), while the catalog still reported `last_shipped=1, schema_version=0`.
   - **Expected fix:** Whenever `shipper.Cycle` in `openDatabase` or `evicted` ships a segment or snapshot (`report.Segment != nil || report.Snapshot != nil`), call `n.config.Catalog.RecordShipped` with the new position and `user_version` (ignoring `catalog.ErrStaleEpoch`, as `shipLocked` does), and add a test checking the catalog row after evicting/closing a `LocalDurable` write and after post-crash reopen.

3. **[blocker] `ch10-multi-database-platform-capstone/service/fence/fence.go:169-183` (`Adapter.refreshLocked`) — caches partial chain and marks `a.listed = true` before returning `FencedError`**
   - **What is wrong:** Line 174 executes `a.slots, a.end, a.listed = map[int64]Slot{}, 0, true` before iterating over `chain`. When the loop encounters a slot with `slot.Epoch > a.Epoch`, it returns `FencedError` while leaving `a.listed = true`, `a.slots` populated with the earlier slots, and `a.end` set to the last pre-fence slot. Any subsequent `Adapter.List` call sees `a.listed == true` and returns the truncated pre-fence key list with `err == nil`, silently hiding the newer epoch instead of fencing the caller.
   - **How confirmed:** Code inspection of `fence.go:174-192`:
     - `Refresh` on epoch-1 adapter against `[slot 1 (epoch 1), slot 2 (epoch 2)]` sets `a.listed = true`, inserts slot 1, then returns `&FencedError{Holder: 2, Position: 2}` at slot 2.
     - Next call to `List(ctx, "")` checks `if !a.listed` (false) and returns `["snapshots/000000000001"], nil`.
   - **Expected fix:** Build `slots := map[int64]Slot{}` and `end := int64(0)` in local variables, returning `FencedError` immediately if any slot has `slot.Epoch > a.Epoch` without mutating `a.slots`, `a.end`, or `a.listed`; only assign `a.slots, a.end, a.listed = slots, end, true` after the entire chain passes. Add a unit test verifying that `List` after a fenced `Refresh` still returns `ErrFenced`.

4. **[should-fix] `ch10-multi-database-platform-capstone/service/node/node.go:245,292` (`Node.Claim`) — does not ignore `catalog.ErrStaleEpoch` after the fence is accepted, and leaves stale `.owner` file on failed re-claim**
   - **What is wrong:** After `adapter.PutIfAbsent` accepts the fence snapshot in the store, `Claim` writes the local DB, writes `<id>.owner`, sets `n.owned[id] = epoch`, and then returns `n.config.Catalog.RecordShipped(...)` directly (line 292). If another node bumped the catalog epoch between `Catalog.Claim` and `RecordShipped` (before fencing the store), `RecordShipped` returns `catalog.ErrStaleEpoch`, so `Claim` reports failure even though the store accepted its fence and `n.owned[id]` + `<id>.owner` are already set. This contradicts `shipLocked` (`node.go:380-382`) and build-notes correction 3 ("the store is the authority on acceptance"). Separately, line 245 deletes `n.owned[id]` without removing an existing `<id>.owner` file, so if a node re-claims a database it owned and the claim fails, the old belief file remains on disk.
   - **How confirmed:** Code inspection of `node.go:244-245,287-292` vs `node.go:379-382` and `agent_work/ch10a/build-notes.md:55-64,264-266`.
   - **Expected fix:** Ignore `catalog.ErrStaleEpoch` on line 292 (matching `shipLocked`), and remove any stale `<id>.owner` file when clearing `n.owned[id]` at the start of `Claim` (before calling `Catalog.Claim`).

5. **[should-fix] `ch10-multi-database-platform-capstone/experiments/10.1-one-writer/main.go:30,63` — unknown `-mutant` value silently runs the `correct` model and passes**
   - **What is wrong:** Neither `main.go` nor `model.Explore` validates the `Mutant` value. Because `model.Step` only compares against `StoreAcceptsEveryPut` and `ResumeFromGlobalEnd`, any misspelled or unknown `-mutant` flag silently executes the `correct` model, prints `check ok: model <typo>: ...`, and exits 0.
   - **How confirmed:**
     ```
     $ go run ./ch10-multi-database-platform-capstone/experiments/10.1-one-writer -model-only -mutant bogus-mutant
     ...
     bogus-mutant, depth 16, seed 1: 9595507962 interleavings explored, 44134 distinct states, 0 interleavings violate an invariant
     check ok: model bogus-mutant: no interleaving up to depth 16 and no random walk lets a fenced epoch get a commit accepted
     ...
     all checks passed
     EXIT=0
     ```
   - **Expected fix:** Add a `model.ParseMutant(string) (Mutant, error)` (or validate `*mutant` in `main.go`) and fail with a non-zero exit status on any value other than `correct`, `store-accepts-every-put`, or `resume-from-global-end`.

6. **[should-fix] `ch10-multi-database-platform-capstone/service/fence/fence.go:230-279` (`Adapter.PutIfAbsent`) — writes unvalidated snapshot/segment envelopes to the store before decoding them**
   - **What is wrong:** `PutIfAbsent` calls `a.Base.PutIfAbsent(ctx, slotKey, envelope)` at line 255 and only runs `Decode(slotKey, envelope)` at line 274. For `KindSnapshot` it never calls `walship.DecodeSnapshot` prior to upload, and for `KindSegment` it does not verify `meta.FirstPosition == first` prior to upload. Passing a malformed snapshot or mismatched-position segment writes a corrupt, immutable object into the S3 slot before returning `ErrDamaged`, poisoning the chain for all subsequent readers/claims.
   - **How confirmed:** Code inspection of `fence.go:231-278`: `walship.DecodeSnapshot` is not called before line 255, and `Decode(slotKey, envelope)` runs only after `a.Base.PutIfAbsent` returns `nil`.
   - **Expected fix:** Call `slot, err := Decode(slotKey, envelope)` before `a.Base.PutIfAbsent(ctx, slotKey, envelope)`, then reuse `slot` on success.

7. **[should-fix] `ch10-multi-database-platform-capstone/service/model/model.go:19,160,168` and `experiments/10.1-one-writer/main.go:61` — printed model bound misdescribes `MaxLocal`**
   - **What is wrong:** `main.go:61` (and `build-notes.md:360`) states the model allows "at most 3 epochs and 5 local commits per node". In `model.go:160`, `Fence` sets `n.Local = at` (the global stream position of the fence), and `Write` (`model.go:168`) checks `n.Local >= MaxLocal` (`5`). Thus `MaxLocal` caps the global stream position at 5 across all epochs and fences combined — not 5 local commits per node. For example, if epoch 1 fences at 1 and ships 2, and epoch 2 fences at 3 and ships 4, epoch 3 fences at 5 and cannot execute a single `Write`.
   - **How confirmed:** Code inspection of `model.go:160,168` and `evidence/10.1-one-writer.txt:4,84` (step 14: `B write: ok: local commit 5` after B only made 2 writes following its fence at 3).
   - **Expected fix:** Either update the description in `main.go:61` and `build-notes.md` to state the real bound ("stream positions up to 5") or track per-node commit counts separately if 5 writes per node is intended.

8. **[nit] `ch10-multi-database-platform-capstone/service/fence/fence.go:73,93-99` (`Decode` / `keyDatabase`) — empty database segment in slot key passes `Decode`**
   - **What is wrong:** `keyDatabase("dbs//log/000000000001")` returns `""`, and `SlotKey("", 1)` produces `"dbs//log/000000000001"`, so `key != SlotKey(keyDatabase(key), slot.First)` does not reject a key with an empty database id.
   - **How confirmed:** Code inspection of `fence.go:52-56,73,93-99`.
   - **Expected fix:** Reject keys where `db := keyDatabase(key); db == "" || walship.ValidateKey("dbs/"+db) != nil`.

9. **[nit] `ch10-multi-database-platform-capstone/service/node/node.go:105,228-234,267` — unchecked `epoch == 0` in belief file and potential divide-by-zero in `pageSizeOf`**
   - **What is wrong:** (a) `Start` (`node.go:105`) accepts `"epoch 0\n"` in a `.owner` file instead of failing at startup (deferring the failure to `fence.New`). (b) `Claim` (`node.go:267`) computes `len(image) / pageSizeOf(image)` without checking that `pageSizeOf(image)` is nonzero (a corrupt/zeroed 100-byte image from `Create` would panic with integer divide-by-zero).
   - **How confirmed:** Code inspection of `node.go:105,228-234,263-269`.
   - **Expected fix:** Reject `epoch == 0` when reading `.owner` files, and validate `pageSize` (power of two in `[512, 65536]` dividing `len(image)`) before dividing in `Claim`.

10. **[nit] `ch10-multi-database-platform-capstone/service/node/serve.go:46` — `close` with extra arguments exits `Serve` without closing `Node`**
    - **What is wrong:** `handle` only closes `n` when `fields[0] == "close" && len(fields) == 1`, returning `refused ...: unknown command` otherwise, but `Serve` line 46 checks only `if fields[0] == "close"` and returns `nil` without closing `n`.
    - **How confirmed:** Code inspection of `serve.go:46-49,78-82`.
    - **Expected fix:** Guard line 46 with `if fields[0] == "close" && len(fields) == 1`.

11. **[nit] `ch10-multi-database-platform-capstone/experiments/10.1-one-writer/main.go:246,267-268,279` — fragile ack parsing and unreported failure details**
    - **What is wrong:** (a) Line 246 uses `fmt.Sscanf` on `reply.Line`; if the `ok write` wording ever drifts, `n == 4` silently evaluates false, `acks` stays empty, and the ack verification loop in `verify()` passes vacuously. (b) Lines 267–268 check `len(problems) == 0` and `len(overBounds) == 0` without printing the contents if non-empty. (c) `verify()` calls `fence.Audit` but does not assert `len(restorable.Damaged) == 0` (corrupt WAL checksums in a pre-snapshot segment of an earlier epoch would not be caught by `RestoreAt(last.Last)`).
    - **How confirmed:** Code inspection of `main.go:245-252,267-268,279-330`.
    - **Expected fix:** Assert that every `ok write ` reply parses into an `ack` (or check `len(acks) > 0`), print `problems`/`overBounds` when non-empty, and include `len(restorable.Damaged) == 0` in the audit check.

12. **[nit] `ch10-multi-database-platform-capstone/service/s3store/s3store.go:88` and `service/faultstore/faultstore.go:124` — minor input/trigger inconsistencies**
    - **What is wrong:** (a) `s3store.List` does not validate `prefix` (unlike `walship.DirStore.List` and `s3store`'s other methods). (b) `faultstore.List` calls `s.match(true, prefix)` before checking `len(keys) > 0`, so an empty listing consumes the `Nth` trigger and logs to `Fired()` even though no key was hidden.
    - **How confirmed:** Code inspection of `s3store.go:88-105` vs `internal/walship/store.go:175-181`, and `faultstore.go:124-127`.
    - **Expected fix:** Validate non-empty prefixes in `s3store.List` with `walship.ValidateKey(strings.TrimSuffix(prefix, "/"))`, and check `len(keys) > 0` before matching `StaleList` in `faultstore.List` (or document empty-list counting).

## Verification of outline corrections (`build-notes.md`)

All six corrections in `agent_work/ch10a/build-notes.md` are technically sound and supported by the code, `agent_work/probes/seaweedfs-s3/RESULT.md`, and `internal/walship/shipper.go`:
1. Store enforces exclusive position slots (`If-None-Match: *`), not epoch comparisons; safety requires slot exclusivity plus the writer's append-after-own-last / stop-on-newer-epoch rule (both mutants prove necessity).
2. `walship.Open` resumes from `Catalog.LastPosition()` globally and is unfenced without `fence.Adapter`.
3. Ownership is lost when the newer epoch's fence is accepted in the store, not when the catalog bumps the epoch (note Finding 4 where `Node.Claim` violates its own rule by failing on `catalog.ErrStaleEpoch`).
4. Local-durable acknowledgements of an old owner can be lost on takeover; 10.1's no-loss invariant holds for store-durable.
5. 7 descriptors per open service database (3 connections × `.db` + 3 × `-wal` + 1 shared `-shm`), confirmed by `INFO:` bounds output (0 / 7 / 14).
6. SeaweedFS S3 + `faultstore` wrapper replaces the local directory stand-in per ruling 2.

## Commands run

| Command | Result |
| --- | --- |
| `go build ./... && go vet ./...` | PASS |
| `GOOS=darwin GOARCH=arm64 go build ./... && GOOS=darwin GOARCH=arm64 go vet ./...` | PASS |
| `gofmt -l ch10-multi-database-platform-capstone/ internal/walship/atrest.go internal/walship/atrest_test.go` | PASS (empty output) |
| `rg -n -g '*.go' '(//\|/\*)' ch10-multi-database-platform-capstone/ internal/walship/atrest.go internal/walship/atrest_test.go` | PASS (0 comments; only `http://127.0.0.1:1` URL literal in `s3store_test.go:56`) |
| `go test -v -race -count=1 ./internal/walship/ ./ch10-multi-database-platform-capstone/...` | PASS (all 8 packages, including `TestS3StoreSemantics` live against SeaweedFS) |
| `go run ./cmd/course check sources ch10` | PASS (1 copied file matches pin) |
| `go run ./cmd/course check spans ch10` | PASS (10 spans start at their symbol) |
| `go run ./cmd/course check paths` | PASS (313 tracked files free of machine-local paths) |
| `go run ./cmd/course check evidence ch10` | PASS (2 consecutive runs identical to `evidence/10.1-one-writer.txt`) |
| `go run ./ch10-multi-database-platform-capstone/experiments/10.1-one-writer -model-only -mutant store-accepts-every-put` | FAILED as expected, exit 1 (4-step violation) |
| `go run ./ch10-multi-database-platform-capstone/experiments/10.1-one-writer -model-only -mutant resume-from-global-end` | FAILED as expected, exit 1 (7-step violation) |
| `go run ./ch10-multi-database-platform-capstone/experiments/10.1-one-writer -model-only -mutant bogus-mutant` | Unexpected PASS, exit 0 (Finding 5) |
| `COURSE_S3_ENDPOINT=http://127.0.0.1:1 go run ./ch10-multi-database-platform-capstone/experiments/10.1-one-writer` | Printed `STORE UNREACHABLE: ...`, exit 1 |
| Two-process repro via built `10.1-one-writer -role node` against SeaweedFS (`repro-bug1`, `repro-bug2`) | Reproduced Finding 1 (`ErrChangedWhileShut` after reclaim) and Finding 2 (stale catalog after `LocalDurable` close) |
