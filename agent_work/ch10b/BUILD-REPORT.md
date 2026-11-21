# Chunk 10b build report — Chapter 10 (second half): relocation, experiments 10.2–10.4

Builder: Claude Opus 5.5, branch `chunk/ch10b`, 2026-11-21. Details for review, attack and the
prose pass: `agent_work/ch10b/build-notes.md`.

## What was built

- **Relocation** with seven numbered steps (catalog fence, seal, final ship, rebuild, verify, store
  fence = activate, retire) so a fault can be injected after each, plus **resume** and **rollback**
  of an interrupted relocation decided from the catalog's `relocations` row and whether the
  destination's fence is in the store:
  - `service/relocate` (new): the coordinator `Run` / `Resume` / `Rollback`, `Participant`
    interface, `CrashAfter` and `SkipFence` (mutant) hooks, `AfterStep` hook for clients and probes.
  - `service/node/relocate.go` (new): `Seal`, `Drain`, `Stage`, `Activate` (with adopt of a fence
    already in the store), `Retire`, `Discard`, `Status`, `Inspect`; `commands.go` (line protocol),
    `remote.go` (the same operations over a node process, strict reply parsing, `ParseAck`),
    `kill.go`.
  - `service/catalog/relocation.go` (new): the `relocations` table and its four operations;
    `bulk.go`: `RegisterAll`, `All`.
- `service/fence/inspect.go` (new): rebuild every restore point of a database and name the last good
  one (gap, damaged object, or a state that is not a recipe state).
- `service/s3store/batch.go` (new): `WipeInBatches` (S3 multi-delete).
- `service/lab` (new): the experiment harness shared by 10.2–10.4 (reset, node processes, reply and
  bounds tracking, recipe matching, the checker's line protocol, fleet inspection, counting store,
  truncate-and-hash).
- Edits to 10a code (comments kept, none added or deleted): `node.go` (seal check in `Write`,
  `AckBeforePut` mutant field, `Claim` clears seal and staged file), `process.go` (mutant flag,
  appended only when set), `serve.go` (fallback to the new commands).
- Experiments **10.2 acknowledged-writes-survive**, **10.3 interrupted-relocation**, **10.4
  rebuild-the-fleet** with evidence; `evidence/10.4-rebuild-the-fleet-2000.txt` for the outline's
  2,000 databases; `SPANS.txt` + 2 spans (`walIndexRecover`, `walFrames`).
- `agent_work/ch10b/`: build-notes.md, INDEX.md, two probes, this report.

No `internal/` package changed; `go.mod` unchanged.

## Commands run

| command | result |
| --- | --- |
| `gofmt -l` on every file of this chunk; `go run ./cmd/course check gofmt` | nothing listed; PASS (245 files) |
| `go build ./... && go vet ./... && go test ./...` | pass |
| `go test -race -count=1 ./ch10-multi-database-platform-capstone/... ./internal/walship/` | pass (S3 tests ran against SeaweedFS, not skipped) |
| `bash agent_work/verify/crossbuild.sh` (linux + darwin arm64/amd64 build, vet, test compile, gofmt) | `CROSSBUILD OK` |
| `go run ./cmd/course check sources ch10` / `check spans ch10` / `check paths` | PASS (1 copy; 12 spans; 315 files clean) |
| `go run ./cmd/course check evidence ch10` (each experiment built once, run twice, compared) | PASS for 10.1, 10.2, 10.3, 10.4: 2 runs identical to the evidence |
| `go run …/10.2-acknowledged-writes-survive` | exit 0, ~40 s |
| `go run …/10.2-acknowledged-writes-survive -mutant ack-before-put` | 2 checks FAILED (violations found), exit 1, as designed |
| `go run …/10.3-interrupted-relocation` | exit 0, ~35 s, `31 of 31 fault cases hold every invariant`, self-test finds two writable nodes |
| `go run …/10.3-interrupted-relocation -mutant skip-fence -case 1` | case check FAILED, exit 1, as designed |
| `go run …/10.4-rebuild-the-fleet` (500 databases) | exit 0, 1 min 52 s – 1 min 57 s |
| `go run …/10.4-rebuild-the-fleet -databases 2000 -prefix exp-10-4-2000` | exit 0, 9 min 12 s (beside the evidence check); 2000 of 2000 E1, 13,000 restore points, 0 failures |
| each experiment with `COURSE_S3_ENDPOINT=http://127.0.0.1:1` | `STORE UNREACHABLE: …` after the pin line, exit 1 |
| unknown `-mutant` for 10.2 / 10.3 | `FATAL -mutant`, exit 1 |

Cleanup: `_work/` scratch of this chunk removed; S3 prefixes of this chunk wiped
(`agent_work/ch10b/probes/wipe`).

## Corrections to the outline (details in build-notes.md)

1. Bumping the epoch does not stop the source; a seal step was added, and the store fence is what
   binds a source that does not cooperate (7 steps instead of 6).
2. The fence point is the drained state; its position depends on failures (a failed final put turns
   the unshipped commits into one discontinuity snapshot).
3. A relocation keeps local-durable acknowledgements (the drain ships them); a takeover does not.
4. 10.2's syscall kills are at fsync only (pwrite64 is chapter 9's sweep).
5. 10.4 runs 500 databases by default (2,000 recorded separately); what was scaled is listed.
6. 10.4's byte counts are not stable run to run; the EXACT claim is on object counts (each slot read
   exactly twice by the rebuild).
7. The TRUNCATE checkpoint at quiesce finds 0 frames (the writers' close already checkpointed).
8. Store-durable recovery from the store alone is a takeover of every database (one epoch each).
9. Periodic snapshots, pruning and the position-1 lineage rule (10a's open items) were not needed by
   relocation or 10.4 and were not built; how they would fit is in the notes.

## Left undone

- 10.2's kill points cover fsync only; kills inside `claim`, eviction-specific and snapshot-specific
  kill points are left to the adversarial pass.
- No long-reader relocation test (design question 4 is answered from chapters 5 and 9).
- The Docker `lab` route for 10.2 part A was not run; darwin is cross-built only.
