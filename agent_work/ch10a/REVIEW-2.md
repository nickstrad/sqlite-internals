VERDICT: PASS

## Finding-by-finding verification

| Finding / Attack | Fixed? | How confirmed |
| --- | --- | --- |
| R1 [blocker] stale `n.atRest[id]` after reclaim (`node.go:333-336`) | Yes | Diff inspection (`Claim` deletes `n.atRest[id]` and removes `<id>.atrest`); `go test -v -race -count=1 ./ch10-multi-database-platform-capstone/service/node` (`TestReclaimAfterAnotherOwnerAdvancedTheDatabase` PASS). |
| R2 [blocker] catalog not updated by ships at open/evict/close (`node.go:187-196,267-275`) | Yes | Diff inspection (`openDatabase` and `evicted` call `n.recordShipped` whenever `shipped(report)` is true, reading `user_version` from SQLite on reopen or bytes 60–64 of the at-rest file header on eviction); `TestShipsOnEvictionCloseAndReopenUpdateTheCatalog` PASS with `-race`; attacks `CAT-01` and `CAT-02` HELD in `results-run2.txt`. |
| R3 [blocker] fenced `Refresh` caches partial chain (`fence.go:179-188`) | Yes | Diff inspection (`refreshLocked` builds `slots` and `end` in locals and assigns `a.slots, a.end, a.listed` only after the full chain passes); `TestListAfterFencedRefreshStaysFenced` PASS with `-race`. |
| R4 [should-fix] `Claim` fails on `ErrStaleEpoch` after fence accepted; stale `.owner` kept (`node.go:334,384`) | Yes | Diff inspection (`Claim` uses `n.recordShipped` ignoring `catalog.ErrStaleEpoch`, and deletes `<id>.owner` and `<id>.atrest` when clearing ownership); `TestClaimSucceedsOnceTheStoreAcceptedItsFence` and `TestFailedReclaimForgetsTheBeliefOnDisk` PASS with `-race`; attacks `FENCE-03a`, `FENCE-03b`, `CAT-03a` HELD in `results-run2.txt`. |
| R5 [should-fix] unknown `-mutant` runs `correct` (`model.go:17-23`, `main.go:63-66`) | Yes | Diff inspection (`model.ParseMutant` + `fatal("-mutant", err)`); `TestParseMutant` PASS; manual probe `10.1-one-writer -model-only -mutant bogus-mutant` printed `FATAL -mutant: model: unknown mutant "bogus-mutant" ...` and exited 1 (both real mutants still fail with exit 1). |
| R6 [should-fix] malformed envelopes put before `Decode` (`fence.go:266-269`) | Yes | Diff inspection (`Decode(slotKey, envelope)` runs before `a.Base.PutIfAbsent`); `TestMalformedPutNeverReachesTheStore` PASS with `-race`. |
| R7 [should-fix] printed model bound misdescribed `MaxLocal` (`main.go:61`) | Yes | Diff inspection (`main.go:61`, `evidence/10.1-one-writer.txt`, and `build-notes.md` now state that local commits stop at stream position 5 across all epochs and fences, not 5 commits per node); `course check evidence ch10` PASS (2 runs identical). |
| R8 [nit] empty database segment passes `Decode` (`fence.go:78,98-104`) | Yes | Diff inspection (`keyDatabase` enforces `dbs/<valid id>/log/<12 digits>` with non-empty valid segment); `TestDecodeRejectsMalformedKeys` PASS with `-race`. |
| R9 [nit] epoch 0 belief file; `pageSizeOf` divide-by-zero (`node.go:107,309-321`) | Yes | Diff inspection (`Start` rejects `epoch == 0`; `pageSizeOf` validates power-of-two size in `[512, 65536]` dividing `len(image)`); `TestStartRejectsBadRecords` and `TestPageSizeOfRejectsBadImages` PASS with `-race`; attack `CAT-03b` HELD in `results-run2.txt`. |
| R10 [nit] `close` with extra args exits `Serve` without closing (`serve.go:46`) | Yes | Diff inspection (`if fields[0] == "close" && len(fields) == 1`); live `-role node` probe fed `close extra` then `close`: first returned `refused close extra: unknown command` without exiting, second returned `ok close` and exited 0. |
| R11 [nit] fragile ack parsing, unprinted problems, missing `Damaged` check (`main.go:256-258,273-278,300`) | Yes | Diff inspection (unparsable `ok write ` calls `fatal`; `problems`/`overBounds` are printed before checks; audit check requires `len(restorable.Damaged) == 0`); experiment 10.1 run and `course check evidence ch10` PASS. |
| R12 [nit] `s3store.List` prefix validation; `faultstore.List` empty listing count (`s3store.go:89-93`, `faultstore.go:124-127`) | Yes | Diff inspection; `TestS3StoreSemantics` (live SeaweedFS) and `TestStaleListSkipsEmptyListings` PASS with `-race`. |
| Attack `MOD-05b` (bound description) | Answered (claim narrowed per R7) | `MaxLocal = 5` caps stream position across all epochs and fences; `main.go:61`, `evidence/10.1-one-writer.txt`, and `build-notes.md` now state this accurately, and the model remains sound under this bound. Updated `MOD-05b` in Part 2 to verify both boundaries (pos 4→5 allowed, pos 5/6 refused) and `ParseMutant`; full suite now 27/27 HELD (`results.txt`). |
| Attack `FENCE-03a` / `FENCE-03b` | Yes (via R4) | `results-run2.txt`: `FENCE-03a` HELD (`staleEpochErrs=0 failedClaimStillOwns=0`); `FENCE-03b` HELD (`claimErr=<nil> owned=(1,true)`). |
| Attack `CAT-01` / `CAT-02` | Yes (via R2) | `results-run2.txt`: `CAT-01` HELD (`catPos=2,catUV=1` for both); `CAT-02` HELD (`catPos=2,catUV=1`). |
| Attack `CAT-03a` / `CAT-03b` | Yes (via R4 / R9) | `results-run2.txt`: `CAT-03a` HELD (`ownedAfterRestart=false`); `CAT-03b` HELD (`startErr=... "epoch 0\n"`). |
| Attack `CAT-04b` (offline tamper across clean close + restart) | Yes | Diff inspection (`node.go:112-126,151-162,262-265` persists `<id>.atrest` on clean eviction/close, reloads on `Start`, removes once reopen check passes before opening the shipper so crashes while open remain post-crash); `TestRestartRefusesAFileChangedWhileShut` PASS with `-race`; `results-run2.txt`: `CAT-04b` HELD (`ErrChangedWhileShut`, `tamperPublishedToS3=false`). |
| Attack `CAT-05b` (memory growth on repeated commits) | Yes | Diff inspection (`fence.go:52-55,184,213-239,289` drops `Object`/`raw` bytes from `Adapter.slots` and reads from `Base.Get` on demand, checking metadata against the cached listing); `TestAdapterKeepsNoObjectBytes` PASS with `-race`; `results-run2.txt`: `CAT-05b` HELD (`peakRSS=3MiB/34MiB`, `memExceeded=0`). |

## New findings

None.

## Commands run

| Command | Result |
| --- | --- |
| `go build ./... && go vet ./... && go test ./...` | PASS |
| `bash agent_work/verify/crossbuild.sh` | PASS (`CROSSBUILD OK`: linux/amd64, darwin/arm64, darwin/amd64, `course check gofmt`) |
| `go test -v -race -count=1 ./internal/walship/ ./ch10-multi-database-platform-capstone/...` | PASS (all 8 packages including live SeaweedFS `TestS3StoreSemantics` and all 13 new regression tests) |
| `go run ./ch10-multi-database-platform-capstone/experiments/10.1-one-writer` | PASS (`all checks passed`, exit 0) |
| `go run ./cmd/course check sources ch10 && spans ch10 && paths && evidence ch10` | PASS (sources 1/1, spans 10/10, paths 314 clean, evidence 2 runs identical) |
| `10.1-one-writer -model-only -mutant bogus-mutant` / `store-accepts-every-put` / `resume-from-global-end` | `bogus-mutant` → `FATAL -mutant`, exit 1; both mutants → `1 check(s) FAILED`, exit 1 |
| `10.1-one-writer -role node` probe (`close extra` then `close`) | `refused close extra: unknown command`, stayed open, then `ok close`, exit 0 |
