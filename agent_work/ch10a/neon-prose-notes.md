# Neon prose pass notes — chunk ch10a

Closing prose pass (Deliverable A — code comments only; chapter 10 markdown text is deferred to chunk ch10b).

## What was written

Added package doc comments, exported identifier doc comments (Go convention), non-obvious WHY comments with pinned SQLite source locations (`src/wal.c`, `src/os_unix.c`), and one-line test comments across all 20 Go files in scope:

- `internal/walship/atrest.go`, `atrest_test.go`: documented `Shipper.AdoptAtRest` (why `sqlite3WalClose` at `src/wal.c:2507-2571` deletes `-wal` on last close, how read slot 0 blocks backfill at `src/wal.c:2259-2260`, and why the page hash check skips discontinuity snapshots safely) and both tests.
- `ch10-multi-database-platform-capstone/service/s3store/s3store.go`, `s3store_test.go`: documented S3 translation, the three SeaweedFS properties relied on (`If-None-Match: "*"`, read-after-write, list-after-write), no listing-order assumption, and SDK retry handling deferred to `fence.Adapter`.
- `ch10-multi-database-platform-capstone/service/faultstore/faultstore.go`, `faultstore_test.go`: documented all six fault modes, `Rule`/`ParseRule`, `Store`, and why empty listings do not consume a `StaleList` trigger.
- `ch10-multi-database-platform-capstone/service/fence/fence.go`, `fence_test.go`: documented the two halves of the fencing rule (S3 write-once slot exclusivity plus the writer's epoch check and contiguous append rule), strict envelope decoding, local pre-put validation, metadata-only caching (`withoutBytes`), single-listing claim consistency, atomic `refreshLocked` cache replacement, and `Audit`/`RestoreAt`.
- `ch10-multi-database-platform-capstone/service/catalog/catalog.go`, `catalog_test.go`: documented the shared WAL control-plane table, single-host SQLite lock scope (`src/os_unix.c:1189-1342`), atomic `Claim`, and why callers ignore `ErrStaleEpoch` after store acceptance.
- `ch10-multi-database-platform-capstone/service/bounds/bounds.go`, `bounds_test.go`: documented the standing descriptor and RSS formulas carried from Chapters 4 and 8, including the 3 connections / 7 actual descriptors per service database (`unixInodeInfo`, `src/os_unix.c:1323-1342`).
- `ch10-multi-database-platform-capstone/service/model/model.go`, `model_test.go`: documented the finite-state ownership model, the `MaxLocal` stream-position bound (across all epochs and fences, not per-node commits), the two broken-fencing mutants (`StoreAcceptsEveryPut`, `ResumeFromGlobalEnd`), why `Accepted` lives in `State` for sound memoization, and all actions/invariants.
- `ch10-multi-database-platform-capstone/service/node/node.go`, `serve.go`, `process.go`, `node_test.go`, `repair_test.go`: documented router, `connmgr` integration, `<id>.owner` and `<id>.atrest` sidecar lifecycles, reopen/eviction/close catalog updates, why `evicted` runs its own `CheckAtRest` after closing the shipper (`src/wal.c:2507-2571`, `src/os_unix.c:1189-1342`), `LocalDurable` vs `StoreDurable`, strict `close` handling in `Serve`, and the `Process` child driver.
- `ch10-multi-database-platform-capstone/experiments/10.1-one-writer/main.go`: documented Part A (exhaustive model + two mutant self-tests), Part B (15-step two-process stale-owner scenario on SeaweedFS), `len(restorable.Damaged) == 0` audit verification, and why `-role node` dispatches before stdout pin output.

## Verification

- Token-stream comparison against `HEAD` (non-comment tokens): all 20 modified Go files have identical code tokens and every exported symbol and `Test*` function has a doc comment.
- `gofmt -l` on all 20 files: empty output; `go run ./cmd/course check gofmt`: PASS (245 files).
- `go build ./... && go vet ./...` and `GOOS=darwin GOARCH=arm64 go build ./... && GOOS=darwin GOARCH=arm64 go vet ./...`: PASS.
- `go test -count=1 ./internal/walship/ ./ch10-multi-database-platform-capstone/...`: PASS (all 8 packages).
- `go run ./cmd/course check paths`: PASS (314 tracked files, zero machine-local paths).
- `go run ./cmd/course check sources ch10 && go run ./cmd/course check spans ch10`: PASS (1 copy, 10 spans).

## Open doubts

None. All facts match `agent_work/ch10a/build-notes.md`, `REPAIR-REPORT.md`, `REVIEW-2.md`, `evidence/10.1-one-writer.txt`, and `SPANS.txt`.

## Suspected defects

None.
