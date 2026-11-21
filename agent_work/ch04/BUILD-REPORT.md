# Chunk 4 build report — Chapter 4, Pager and page cache

Builder: Claude Opus 5.5, branch `chunk/ch04`, 2026-11-20. Full facts for the prose pass:
`agent_work/ch04/build-notes.md`.

## What was built

| Path | What |
| --- | --- |
| `internal/journalfile/` | New shared package: read-only rollback-journal inspector (header state, records, checksums, predicted hot-journal playback). Not a replayer. Unit tests plus a live test against a real open transaction. |
| `ch04-pager-and-page-cache/{pager.c,pcache.c,pcache1.c,status.c}` | Whole copies from the pinned tree; `SOURCES.sha256` lists them |
| `ch04-pager-and-page-cache/SPANS.txt` | 44 spans (all outline spans exact; 34 added for functions the experiments name) |
| `experiments/4.1-atomic-commit-and-hot-journal/` | Full-depth rollback-journal experiment: where a WAL platform meets rollback mode; the journal of an open transaction; strace of one recipe step at FULL/NORMAL/OFF; kill on entry to every write, sync and unlink (309 kill runs), journal decoded before every reopen; twin check with the C CLI; step-1 variant with exact bytes; failure cases (journal deleted, file copied without journal, journal deleted after the last database write); DELETE/TRUNCATE/PERSIST commit-point table |
| `experiments/4.2-cache-budget/` (`main.go`, `memory_linux.go`, `memory_other.go`) | Per-connection `CACHE_USED`, hit/miss, process memory (`/proc/self/status`; `ps -o rss=` elsewhere) for N connections to N files, N to one file, a K-connection LRU pool, and mmap. N, K, cache_size, rows are flags (64, 8, -2000, 1000) |
| `evidence/4.1-…txt`, `evidence/4.2-…txt` | Captured output |
| `agent_work/ch04/` | `build-notes.md`, `INDEX.md`, `step-probe/` |

No changes to existing shared packages; go.mod unchanged. No chapter markdown, no comments.

## Commands run

| Command | Result |
| --- | --- |
| `go build ./... && go vet ./... && go test ./...` | pass |
| `agent_work/verify/crossbuild.sh` (linux/amd64 build+vet+test, darwin arm64/amd64 build+vet+test-compile, gofmt) | `CROSSBUILD OK` |
| `go run ./cmd/course check sources ch04` | PASS (4 files) |
| `go run ./cmd/course check spans ch04` | PASS (44 spans) |
| `go run ./cmd/course check evidence ch04` | PASS both experiments (2 runs each identical to the evidence) |
| `go run ./cmd/course check gofmt` | PASS |
| `go run ./cmd/course check paths` | FAIL on one pre-existing tracked file, `_tools/bin/kb` (a binary added by commit c51ff44, not this chunk); no ch04 file flagged |
| 4.1 run twice on the host | identical EXACT output, exit 0, ~15 s |
| 4.2 run three times on the host | identical EXACT output, exit 0, ~3 s; also `-n 5 -k 2` and `-n 200 -k 8` pass |
| both experiments in the `lab` container (`docker compose run --rm -T lab go run …`) | EXACT output identical to the host (container CLI uses `fdatasync`) |

## Corrections to the outline (details and evidence in build-notes.md)

1. A crashed rollback-mode commit reopens to pre-step or post-step **logically**, but not always
   byte for byte: pages taken off the freelist are never journaled (`PAGER_GET_NOCONTENT`,
   btree.c:6740, pager.c:5645-5656), so a rollback leaves their new bytes in place (14 of 47 kill
   points at FULL). Byte-exact when the pre-step file has no freelist (step-1 variant: all kill
   points exact). Platform consequence: verify after a rollback-mode crash at E3 / canonical E1.
2. The commit order includes a 12-byte header write (magic + record count) that makes the journal
   hot; FULL = journal sync, dir sync, header write, journal sync, db writes, db sync, unlink (4
   syncs); NORMAL = header write before the single journal sync (3 syncs); OFF = 0 syncs.
   Counts 3/4 CONFIRMED.
3. The journal written before COMMIT is not hot (zero magic) at FULL/NORMAL; a crash there leaves a
   cold `-journal` that SQLite ignores and does not delete.
4. Even a WAL database created by `sqlitex.Open` is born through one rollback-journal commit (the
   switch to WAL writes page 1 through a `-journal`).
5. In this driver build `cache_size` is not a per-connection cap: `ENABLE_MEMORY_MANAGEMENT` puts
   every connection's cache in one process-wide LRU; an active connection takes idle ones' pages.
   The process total is exactly (open connections) × budget.
6. RSS ≈ 2 × summed `CACHE_USED` with this driver (modernc.org/memory's power-of-two slots put each
   ~4.3 KiB page in an 8 KiB slot).
7. `CACHE_USED` depends on the database path length; 4.2 prints pages on EXACT lines.
8. A cache of N pages holds N−1.

## Left undone

- 1,000-connection run (adversarial pass; about 4 GB RSS with this driver).
- No kill demonstration of a crash inside the WAL switch of a new file (correction 4 is shown by
  strace only).
- Cache spill / multi-header journals not exercised live (unit-tested in `journalfile`).
- darwin: cross-built and cross-vetted only, never run.
- `_work/4.1`, `_work/4.2` and the probe's scratch directory were removed after the final runs.
