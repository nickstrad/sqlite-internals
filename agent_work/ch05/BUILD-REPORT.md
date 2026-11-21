# Chunk 5 build report

Builder: Claude Opus 5.5, branch `chunk/ch05`, 2026-11-20. Base: `7690293`.

## What was built

| Item | Where |
| --- | --- |
| Chapter directory | `ch05-wal-transactions-checkpoints/`: `wal.c`, `wal.h` (whole copies), `SOURCES.sha256`, `SPANS.txt` (28 spans), `experiments/`, `evidence/` |
| `internal/walfile` (+tests, fuzz target) | WAL parser (32-byte header, 24-byte frame headers, both checksum byte orders by magic, salts, running checksum chain, commit frames, SQLite's validity order); frame listing as (page, page sha256, commit size); byte comparison by region; frame replayer (base + WAL → file: committed frames in order, stop at the last valid commit, truncate to its size, ignored frames reported with reasons; files written via temp + fsync + rename); `-shm` wal-index decoder (both header copies, checksum, mxFrame, nPage, salts, nBackfill, read marks, nBackfillAttempted, page-number array); `showwal` output parser and comparison; child-process copier for live `-wal`/`-shm`/database files and a `ReadLiveState` sampler. Typed errors, never panics |
| Experiment 5.1 wal-inspector | WAL of five recipe steps copied live by a child: size formula, 94 frames, 5 commit frames = page counts, checksum chain, NOOP log, `-shm` header and page array, `showwal` agreement; second run differs only in salt/checksum bytes with equal frame sequences; NORMAL and OFF write the same frames |
| Experiment 5.2 wal-replay (R3) | quiet moment, mid-transaction with spilled frames, after rollback: replay E1-equal to SQLite's own TRUNCATE checkpoint (and to SQLite's recovery of the same copy with no `-shm` and with a stale `-shm`); ignored frames reported |
| Experiment 5.3 reader-pins-the-wal | per-commit table of WAL bytes, mxFrame, nBackfill, read marks, reader/writer counts under a held reader; restart after it ends; new part B: a read-lock-0 reader blocks all backfill |
| Experiment 5.4 checkpoint-modes | PASSIVE/FULL/RESTART/TRUNCATE × no reader / reader behind / reader current: result triple, wait, WAL size, file and `-shm` salts, and what the next write does; TRUNCATE by the writer itself |
| Notes | `agent_work/ch05/build-notes.md`; script indexed in `agent_work/ch05/INDEX.md` (`shm-crosscheck`) |

No existing shared package was changed; `go.mod` unchanged.

## Commands run

| Command | Result |
| --- | --- |
| `gofmt -l` on all chunk files | nothing listed (pass) |
| `go build ./... && go vet ./... && go test ./...` | pass |
| `bash agent_work/verify/crossbuild.sh` (linux/amd64, darwin/arm64, darwin/amd64 build + vet + test compile, `check gofmt`) | `CROSSBUILD OK` |
| `go run ./cmd/course check sources ch05` | PASS (2 files) |
| `go run ./cmd/course check spans ch05` | PASS (28 spans) |
| `go run ./cmd/course check evidence ch05` (2 runs each) | PASS for 5.1, 5.2, 5.3, 5.4 |
| the four experiments in the Debian `lab` container | exit 0, non-INFO output identical to the evidence |
| `go run ./agent_work/ch05/shm-crosscheck` on two real `-shm` files | `SHM CROSSCHECK OK` (walfile = `showshm` on every field) |
| `go run ./cmd/course check all` | ch05 parts pass (sources, spans, evidence for every chapter, gofmt 95 files). Failures not from this chunk: `inline -check` of the ch01 (8) and ch02 (11) chapter markdown (blocks out of date since those chunks' repair rounds; chapter markdown is Neon's) and `check paths` on the tracked binary `_tools/bin/kb` — the same failures the ch02 report lists |
| `go test -run '^$' -fuzz FuzzParseAndReplay -fuzztime 30s -fuzzminimizetime 0 ./internal/walfile` | 150,257 inputs, no failure |
| 5.1 with no `showwal` anywhere (`COURSE_TOOLS_DIR` pointing at an empty dir, trimmed `PATH`) | one `NO-SHOWWAL: … cross-check skipped` line in place of the two showwal lines, exit 0 |

Runtimes: 5.1 1-4 s, 5.2 2-6 s, 5.3 3-4 s, 5.4 4-5 s; `go test ./internal/walfile` about 1 s.
`_work/` cleaned after the runs.

## Corrections to the outline (details in build-notes.md)

1. "salt-1 increments on restart": PASSIVE/FULL/RESTART never change salts; the restart happens at
   the next write, and the new header has salt-1 + 1 and checkpoint-seq + 1 only when the
   connection writing it is the one that restarted the WAL; after another connection's
   TRUNCATE the next header has new random salts and that writer's own sequence (`nCkpt` is per
   connection). Detect generations by any salt change.
2. RESTART mode does not restart the WAL; it waits for readers so the next writer can.
3. A reader that begins on a fully checkpointed WAL holds read lock 0, pins no mark, does not stop
   restarts, and blocks every backfill (5.3 part B).
4. 5.3 is EXACT throughout (scripted order), not SHAPE.
5. Read marks keep their values after the reader ends; only the lock shows a live reader.
6. A stale-generation base cannot be detected from base + WAL alone; needs capture metadata
   (chapter 9).
7. `walDecodeFrame` comment says 16 header bytes are checksummed; code uses 8.
8. Confirmed: the `pager.c` 607-617 sync-flag comment is reversed relative to `wal.h` and the code.

## Left undone

- Neon's adversarial lists for 5.1-5.4 (except "different synchronous levels", which 5.1 part C
  covers, and the stale `-shm` case added to 5.2).
- Stale-base detection (correction 6): belongs to `internal/walship`.
- `showshm` is not built by `cmd/course build-tools`; built by hand for the one-off cross-check.
- darwin: cross-built and cross-vetted only, never run.
