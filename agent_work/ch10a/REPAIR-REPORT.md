# Chunk ch10a — repair round 1 report (Opus)

Inputs: `REVIEW-1.md` (FAIL, 12 findings), `adversarial/results-run1.txt` (9 BROKEN).
Commits: `ddf45fd` fence, `0976264` node, `dcbf272` 10.1 + model + evidence, `02b9f58` stores,
then this report and the build-notes update.

| Finding / attack | Outcome | Test added |
| --- | --- | --- |
| R1 [blocker] stale `n.atRest` after reclaim | fixed `0976264`: `Claim` deletes the in-memory record and the `<id>.atrest` file | `TestReclaimAfterAnotherOwnerAdvancedTheDatabase` (fails on old code) |
| R2 [blocker] catalog not updated by ships at open/evict/close | fixed `0976264`: `openDatabase` and `evicted` call `recordShipped` when the cycle shipped a segment or snapshot (ignores `ErrStaleEpoch`) | `TestShipsOnEvictionCloseAndReopenUpdateTheCatalog` (fails on old code) |
| R3 [blocker] fenced `Refresh` leaves a truncated cached view | fixed `ddf45fd`: build in locals, assign only after the whole chain passed | `TestListAfterFencedRefreshStaysFenced` (fails on old code) |
| R4 [should-fix] `Claim` fails on `ErrStaleEpoch` after its fence was accepted; `.owner` left after clearing ownership | fixed `0976264`: `recordShipped` ignores `ErrStaleEpoch`; `Claim` deletes `<id>.owner` (and `.atrest`) when it clears ownership | `TestClaimSucceedsOnceTheStoreAcceptedItsFence`, `TestFailedReclaimForgetsTheBeliefOnDisk` (both fail on old code) |
| R5 [should-fix] unknown `-mutant` runs `correct` | fixed `dcbf272`: `model.ParseMutant`; 10.1 exits 1 (`FATAL -mutant: …`) | `TestParseMutant`; manual run `-mutant bogus-mutant` → exit 1 |
| R6 [should-fix] malformed objects put before decoding | fixed `ddf45fd`: `Decode` of the envelope before `Base.PutIfAbsent` | `TestMalformedPutNeverReachesTheStore` (fails on old code) |
| R7 [should-fix] printed model bound wrong | fixed `dcbf272`: wording only (bound = stream position ≤ 5); evidence re-captured | — (text) |
| R8 [nit] empty database segment passes `Decode` | fixed `ddf45fd`: `keyDatabase` requires `dbs/<valid id>/log/…` | `TestDecodeRejectsMalformedKeys` (fails on old code) |
| R9 [nit] epoch 0 belief; page-size divide by zero | fixed `0976264`: `Start` refuses epoch 0; `pageSizeOf` validates (power of two, 512–65536, divides length) | `TestStartRejectsBadRecords`, `TestPageSizeOfRejectsBadImages` |
| R10 [nit] `close x` exits `Serve` without closing | fixed `0976264` (`serve.go`): exit only on bare `close` | none (one-line guard, no Serve test harness) |
| R11 [nit] fragile ack parsing, unprinted problems, no damaged check | fixed `dcbf272`: unparsable `ok write` → FATAL; problems/over-bounds printed; audit check requires `len(Damaged)==0` | — (experiment; passing output unchanged except the check text) |
| R12 [nit] `s3store.List` prefix; stale-list counts empty listings | fixed `02b9f58` | s3store test (invalid prefix, live S3), `TestStaleListSkipsEmptyListings` |
| MOD-05b | **answered**: the code does what it should; the expectation came from the wrong description (R7). `MaxLocal` caps the node's stream position, and the model is still sound under this bound. The printed line and notes now say so. The attack still prints BROKEN because it tests for the old wording, so the attack itself is now wrong. Not edited. | — |
| FENCE-03a | fixed by R4 (`0976264`); run 2: `staleEpochErrs=0 failedClaimStillOwns=0` HELD | as R4 |
| FENCE-03b | fixed by R4; run 2: `claimErr=<nil> … owned=(1,true)` HELD | `TestClaimSucceedsOnceTheStoreAcceptedItsFence` |
| CAT-01 | fixed by R2; run 2: catalog = store (pos 2, uv 1) for both, HELD | as R2 |
| CAT-02 | fixed by R2; run 2 HELD | as R2 (reopen part) |
| CAT-03a | fixed by R4; run 2: `ownedAfterRestart=false` HELD | `TestFailedReclaimForgetsTheBeliefOnDisk` |
| CAT-03b | fixed by R9; run 2 HELD | `TestStartRejectsBadRecords` |
| CAT-04b | fixed `0976264`: the at-rest record is persisted as `<id>.atrest` at a clean eviction/close and loaded by `Start`. It is deleted once the reopen check passes, before the shipper opens, so a crash while open still counts as post-crash. Run 2: `ErrChangedWhileShut`, `tamperPublishedToS3=false` HELD | `TestRestartRefusesAFileChangedWhileShut` (fails on old code) |
| CAT-05b | fixed `ddf45fd`: the real defect was that `fence.Adapter` kept every slot's object bytes in its cache for as long as the database was open. The cache now keeps metadata only, and `Get` reads from the store and checks the object against the listing. Run 2: peak growth 3 MiB of 34 MiB (was 37 MB), HELD | `TestAdapterKeepsNoObjectBytes` |

## Stale prose for Neon

Neon has written no comments or chapter yet. The facts in `build-notes.md` that changed are
corrected in place, and the section "Repair round 1" at its end summarises them:
- `fence`: cache replaced only after a full chain passes; cache holds no object bytes (`Get` reads
  the store each time, `ErrDamaged` if the object differs from the listing); envelopes validated
  before the put; slot keys must be `dbs/<valid id>/log/<12 digits>`; four new tests.
- `faultstore`: `stale-list` counts only non-empty listings. `s3store.List` validates prefixes.
- `node`: `.owner` epoch 0 refused; new `<id>.atrest` record files (format, lifecycle); catalog
  updated after every ship, including the open-time cycle and the final eviction/close cycle
  (user_version from the file header); `Claim` deletes `.owner`/`.atrest` when it clears ownership
  and ignores `ErrStaleEpoch` after its fence is accepted; page-size validation.
- `model`: the bound is "local commits up to stream position 5", not "5 local commits per node";
  `ParseMutant`.
- 10.1: line 4 of the output and the two "store holds exactly the slots …" check lines changed
  (quoted in build-notes); new FATAL paths; `close` with arguments is an unknown command.

## Commands run

| Command | Result |
| --- | --- |
| `gofmt -l .` | only 5 pre-existing files under `agent_work/probes/` (not this chunk); `course check gofmt` PASS (245 files) |
| `go build ./... && go vet ./... && go test ./...` | PASS |
| `go test -race -count=1 ./ch10-multi-database-platform-capstone/...` | PASS (live SeaweedFS, `TestS3StoreSemantics` ran, not skipped) |
| each new regression test against the pre-repair `fence.go`/`node.go` | FAIL as expected (R1, R2, R3, R4×2, R6, R8, CAT-03b, CAT-04b) |
| `bash agent_work/verify/crossbuild.sh` | CROSSBUILD OK |
| `go run ./…/10.1-one-writer` ×2 | exit 0 both, 0 FAILED checks, EXACT output identical; evidence re-captured |
| `… -model-only -mutant bogus-mutant` | `FATAL -mutant: …`, exit 1 |
| `… -model-only -mutant store-accepts-every-put` | `1 check(s) FAILED`, exit 1 (as designed) |
| `bash agent_work/ch10a/adversarial/run-all.sh` | 26 HELD, 1 BROKEN (MOD-05b, answered above); `results.txt` updated |
| `go run ./cmd/course check all` | exit 1: 60 PASS, including 10.1 (2 runs identical) and gofmt/paths. 2 DIFF in other chapters: 3.3 (`NO-DBHASH`) and 5.1 (`NO-SHOWWAL`), because `dbhash`/`showwal` are not built on this VM (`_tools/bin` missing in this worktree and in the main checkout). This is environmental and unrelated to ch10a. |
