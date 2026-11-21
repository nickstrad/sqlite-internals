# Chunk 10a build report — Chapter 10 (first half): service core and experiment 10.1

Builder: Claude Opus 5.5, branch `chunk/ch10a`, 2026-11-21. Details for review and the prose pass:
`agent_work/ch10a/build-notes.md`.

## What was built

- `ch10-multi-database-platform-capstone/service/` (7 packages, with tests, no comments):
  - `s3store` — `walship.Store` on SeaweedFS S3 with aws-sdk-go-v2 (the probe's choice):
    put-if-absent by `If-None-Match: *`, strict get, paginated list (no order promised), delete,
    wipe; one clear `ErrUnreachable` error when the store is down.
  - `faultstore` — a Go wrapper around any store with selectable faults (fail before/after put,
    partial put, crash before/after put, stale list), the real store underneath.
  - `fence` — the store-enforced fencing scheme: one key per stream position
    (`dbs/<id>/log/<position>`), written once with put-if-absent; an epoch envelope on every
    object; an adapter per database and epoch that appends only after its own last object and
    reports `FencedError` when a newer epoch holds the next position or is listed; lineage check;
    audit and restore helpers.
  - `catalog` — the control plane, one SQLite database in WAL mode: id → owner, epoch, location,
    last shipped position, schema version, per-connection settings (8.1); claim bumps the epoch;
    shipped-position updates are refused for a stale epoch.
  - `bounds` — the standing assertion (descriptors and memory, bounds from 8.3), called by every
    node process after every command.
  - `node` — router + `internal/connmgr` (unchanged) + `internal/walship` capture per open
    database through the fence adapter + ownership (claim = catalog epoch, restore, fence
    snapshot, belief file) + the two acknowledgement modes (local-durable at `synchronous=FULL`,
    store-durable after the segment put); a line protocol and a process driver for multi-process
    experiments (reused by 10b).
  - `model` — the ownership state machine: 2 nodes, catalog, store; exhaustive exploration of
    every interleaving up to a depth (memoised count, breadth-first shortest counterexample),
    seeded random walks; two mutants as self-tests.
- `internal/walship/atrest.go` (new file, shared package, backward compatible, with tests):
  `Shipper.AdoptAtRest` — resume a closed, checkpointed file without uploading a snapshot.
- Experiment `10.1-one-writer` and its evidence; `wal.c` copied; `SOURCES.sha256`; `SPANS.txt`
  (10 spans).
- `agent_work/ch10a/build-notes.md`, `INDEX.md`, this report.

## Commands run

| command | result |
| --- | --- |
| `gofmt -l` on every file of this chunk | nothing listed |
| `go build ./... && go vet ./...` | pass |
| `go test ./...` (inside `agent_work/verify/crossbuild.sh`) and `go test -count=1 ./internal/walship/ ./ch10-multi-database-platform-capstone/...` | pass (s3store test ran against SeaweedFS, not skipped) |
| `go test -race -count=1 ./ch10-multi-database-platform-capstone/service/...` | pass |
| `bash agent_work/verify/crossbuild.sh` (linux + darwin arm64/amd64 build, vet, test compile; `check gofmt`) | `CROSSBUILD OK` |
| `go run ./cmd/course check sources` / `check spans` | PASS for every chapter (ch10: 1 copy, 10 spans) |
| `go run ./cmd/course check evidence ch10` (built once, run twice, compared with the evidence) | PASS: 2 runs identical to the evidence |
| `go run ./cmd/course check paths` | PASS |
| `go run ./ch10-…/experiments/10.1-one-writer -model-only -mutant store-accepts-every-put` | main check FAILED, exit 1 (the check can fail) |
| the same with `-mutant resume-from-global-end` | FAILED, exit 1 |
| `COURSE_S3_ENDPOINT=http://127.0.0.1:1 go run …/10.1-one-writer` | one `STORE UNREACHABLE: …` line after the pin line, exit 1 |

Experiment 10.1 runtime: 3.5–6.5 s. SeaweedFS: the already running `sqlite-internals-seaweedfs-1`
container (compose project of the main checkout, same compose.yaml), bucket `course-ch10a`.

## Corrections to the outline (details in build-notes.md)

1. The store cannot check epochs; it enforces exclusive position slots. Safety needs that plus the
   writer rule "append after your own last object; stop when a newer epoch is listed" — each half
   is shown necessary by a mutant.
2. walship's resume on a shared store is the stale-owner bug; the service wraps the store in the
   fence adapter.
3. "Lost ownership" = the new epoch's fence accepted in the store, not the catalog change.
4. Local-durable acknowledgements of an old owner are lost by a takeover (measured); 10.1's
   no-loss invariant holds for store-durable.
5. 7 descriptors per open service database (3 connections), not 3.
6. The object store is SeaweedFS (ruling 2), the directory store is the unit-test double only.

## Shared-package changes

Only `internal/walship/atrest.go` + `atrest_test.go` (new files). A `connmgr` hook was written and
reverted because `connmgr.go` is inlined in chapter 8; `walship/shipper.go` is inlined in chapter
9, hence the new file. `go.mod` unchanged.

## Left undone / for 10b and the reviewers

- Size: non-test Go of this chunk is about 2,300 lines (service 1,902, experiment 361,
  walship 33), above the brief's ~1,100 target. The largest parts are the model (429) and the node
  (697 with its process driver, which 10b reuses).
- No periodic snapshots and no pruning in the service (`SnapshotEvery = 0`); claim and reopen cost
  grow with the chain. `CheckLineage` requires the chain to start at position 1, which pruning
  would break; 10b must relax it.
- All nodes share one catalog file on one machine; no leases or automatic takeover.
- The lab-container route and darwin were not run (darwin cross-built and cross-vetted only).
- `faultstore`'s put faults are unit-tested but not used by 10.1 (they are for 10.2–10.4).
