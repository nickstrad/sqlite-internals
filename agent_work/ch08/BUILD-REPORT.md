# Chunk 8 build report — Chapter 8, connection and database lifecycle

Builder: Claude Opus 5.5, branch `chunk/ch08` (base `f6df8c3` = ch04 + ch05 merged). Full facts for
the prose pass: `agent_work/ch08/build-notes.md`.

## What was built

| Item | Path |
| --- | --- |
| New shared package: open-descriptor count (`/proc/self/fd` on Linux, `/dev/fd` elsewhere), descriptor targets (Linux), process memory (`/proc/self/status` / `ps -o rss=`), RLIMIT_NOFILE; tested | `internal/procstat` |
| New shared package: bounded LRU connection manager (acquire/release by id, at most K open, LRU eviction of idle entries, on-evict hook with at-rest status and sha256, reopen refused when the file changed while closed, at-rest rule = no `-wal` content, no `-shm`, no hot journal); tested, `-race` clean | `internal/connmgr` |
| Additive change to a shared package: `SettingPlacements()` — which course setting lives in the file (with header bytes) and which per connection, JSON-tagged; tested | `internal/sqlitex/placement.go` |
| Chapter directory: `main.c`, `prepare.c`, `status.c` (whole copies), `SOURCES.sha256`, `SPANS.txt` (34 spans) | `ch08-connection-and-database-lifecycle/` |
| 8.1 open-cost-and-where-settings-live (strace phases on Linux; SCHEMA_USED; settings table across copy and VACUUM INTO; `settings-json` line) | `experiments/8.1-…`, `evidence/8.1-….txt` |
| 8.2 idle-cost (descriptors per state; N idle connections' counters vs process memory; shrink_memory; two connections to one file) | `experiments/8.2-idle-cost`, `evidence/8.2-idle-cost.txt` |
| 8.3 bounded-manager, rung R5 (1,000 databases, K = 64 vs unbounded, in child processes; part B: six cases where a closed WAL database is not a single file at rest) | `experiments/8.3-bounded-manager`, `evidence/8.3-bounded-manager.txt` |

`internal/capture` ends unchanged: an exported `HotJournal` was added in `fddb85b` and reverted
in `8beca4a`, because ch02 inlines `capture.go` whole and the change broke its `inline -check`.
No other shared package was touched. `go.mod` was not touched.

## Commands run

| Command | Result |
| --- | --- |
| `gofmt -l` on my files | nothing |
| `go build ./... && go vet ./... && go test ./...` | PASS |
| `go test -race -count=2 ./internal/connmgr` | PASS |
| `bash agent_work/verify/crossbuild.sh` (linux + darwin build/vet/test-compile, gofmt) | `CROSSBUILD OK` |
| `go run ./cmd/course check sources ch08` / `check spans ch08` | PASS (3 files) / PASS (34 spans) |
| `go run ./cmd/course check evidence ch08` (2 runs each) | PASS for 8.1, 8.2, 8.3 |
| 8.1 and 8.2 in the `lab` container | EXACT output identical to the host's |
| 8.3 in the container | not run (runtime) |
| `go run ./cmd/course check all -skip-evidence` | sources, spans, gofmt PASS; ch02 inline PASS after `8beca4a`; FAIL only on things already on the base: ch04 inline (4 blocks stale after ch04's repair) and `check paths` on the tracked binary `_tools/bin/kb` (commit `c51ff44`) |

Runtimes: 8.1 ~4 s; 8.2 ~2 s (n = 200); 8.3 37–67 s at the defaults (the machine is shared, and
every eviction fsyncs at synchronous=FULL). Peak memory: ~250 MiB, in 8.3's unbounded child.
`_work/` was cleaned.

## Corrections to the outline (details in build-notes.md)

1. `openDatabase` is 3360–3716; 3722–3736 is `sqlite3_open` / `sqlite3_open_v2`.
2. `cache_size` can come from the file: the deprecated `default_cache_size` (header bytes 48–51)
   seeds every new connection's cache_size (prepare.c 323–333).
3. In WAL mode page 1 is read twice at the first statement (lockBtree opens the WAL and returns;
   btreeBeginTrans calls it again). The -wal and -shm open at the first statement, not at open.
4. Process memory per idle connection is 3.8–4 × what CACHE_USED + SCHEMA_USED + STMT_USED report;
   ~165–175 KiB per connection is unreported (lookaside buffer, driver state). The stated margin
   is 256 KiB per connection.
5. With this driver, `PRAGMA shrink_memory` on one connection frees every connection's cache (one
   shared pcache group).
6. R5's "the last close leaves a single checkpointed file" fails when another process holds the
   file, when a statement is left open, when the process is killed, with PERSIST_WAL, or when the
   file is renamed while open (all five shown in 8.3 B). From the source only: NO_CKPT_ON_CLOSE
   and a failed checkpoint. A renamed file looks at rest and is missing a commit. A naive copy
   after a kill is missing a commit.
7. Finding for later chunks: `recipe.MatchStep` rebuilds a step-0 database on every call (~100 ms).
   At 1,000+ files, call `StepHashes` once and match with `MatchHashes`.

WAL as default: kept for the whole chapter. Its costs (3 descriptors per connection, the at-rest
conditions) are measured. No exception is recommended.

## Left undone / for Neon

- Neon's adversarial list was not built into the experiments: 8.1 missing file with and without
  create, non-database file (both checked once in a scratch probe; results in INDEX.md); 8.2 with
  1,000 connections against the descriptor limit; 8.3 kill the manager mid-stream, K = 1, K above
  the descriptor limit. 8.2 runs at `-n 1000` (checked: passes); 8.3 has flags for K and counts.
- The ATTACH paragraph is documentation-only, as the outline says.
- darwin: cross-built only; nothing was run on macOS.
