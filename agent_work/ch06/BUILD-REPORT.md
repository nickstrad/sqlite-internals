# Chunk 6 build report — Chapter 6, locking and concurrent access

Builder: Claude Opus 5.5, branch `chunk/ch06`, 2026-11-20. Details for the prose pass:
`agent_work/ch06/build-notes.md`.

## What was built

- `internal/procs` (new shared package, with tests): child-process roles started from the
  experiment binary, numbered step barriers over pipes (fd 3/4, JSON lines), per-step results
  with SQLite extended result codes, loud failure on a child that dies, times out, answers the
  wrong step or is used after stop; a `Script`/`Lab` layer for transcripts and checks; an SQL role
  (one held connection: begin/commit/rollback, exec/query, recipe steps, checkpoints, the
  stray-close hazard, `sqlite_dbpage` page capture) and a copier role (whole or byte-range copies
  of live files); lock observation with `fcntl(F_GETLK)` (linux and darwin) and a `/proc/locks`
  parser (Linux).
- `internal/sqlitex` (backward-compatible additions, with tests): `ResultCodeName` and result-code
  constants (BUSY_SNAPSHOT 517, PROTOCOL 15, IOERR_DELETE_NOENT 5898, …); `DisplayPath`.
- `ch06-locking-and-concurrency/`: `os_unix.c`, `os.h` copied whole; `SOURCES.sha256`; `SPANS.txt`
  (32 spans); experiments 6.1 locks-belong-to-the-process, 6.2 busy-rules, 6.3 torn-copy,
  6.4 live-snapshot (`-hold read|immediate`); `evidence/` for all four.
- `docs/knowledge/sqlite-internals-ch06-findings.md`; `agent_work/ch06/build-notes.md`, `INDEX.md`.
- No other shared package changed; `go.mod` unchanged.

## Commands run

| command | result |
| --- | --- |
| `gofmt -l` on every file of this chunk | nothing listed (repo-wide `go run ./cmd/course check gofmt`: PASS, 123 files) |
| `go build ./... && go vet ./...` | pass |
| `go test -count=1 ./...` | pass (all packages; `internal/procs` 0.8 s) |
| `agent_work/verify/crossbuild.sh` (darwin/arm64 and linux/amd64 build + vet) | `CROSSBUILD OK` |
| `go run ./cmd/course check sources ch06` | PASS, 2 copied files |
| `go run ./cmd/course check spans ch06` | PASS, 32 spans |
| `go run ./cmd/course check evidence ch06 -runs 2` | PASS, all four experiments identical to their evidence on 2 runs |
| each experiment in the `lab` container (`docker compose run --rm -T lab go run …`) | exit 0, output identical to the host's except `INFO:` lines |
| `go run ./cmd/course check paths` | FAIL only on `_tools/bin/kb` (tracked binary added on main by c51ff44; pre-existing, not this chunk) |
| `go run ./ch06-locking-and-concurrency/experiments/6.4-live-snapshot -hold immediate` | exit 0, every check ok (not stored as evidence) |

Runtimes: 6.1 about 11 s (10 s is SQLite's own PROTOCOL retry loop), 6.2 about 3 s, 6.3 about 2 s,
6.4 about 3 s.

## Corrections to the outline (details in build-notes.md)

1. 6.1: after the stray close a transaction is lost. Rollback mode: the other process's commit
   returns SQLITE_OK and is lost; the holder's commit returns SQLITE_IOERR_DELETE_NOENT (5898) and
   persists. WAL: the holder's commit returns SQLITE_PROTOCOL (15) after about 10 s and is lost.
2. 6.1: "closing any descriptor to the file releases them" holds per file. In WAL mode a stray
   close of the database file does not break writer exclusion; a stray close of the `-shm` does.
3. 6.2: a busy timeout turns BUSY into a wait only for a transaction that has not read yet; a
   deferred transaction that has read gets SQLITE_BUSY at once whatever the timeout
   (btree.c:3740-3741).
4. Span: `unixLock` is 1866-2095 (its doc comment 1842-1865); the outline's 1843 is inside the
   comment.
5. 6.4 observation: a checkpoint's `checkpointed` count does not mean the database file holds
   that state; pages whose newest frame is past a reader's mark are skipped (wal.c:2312).

## Left undone

- Neon's adversarial lists for 6.1-6.4 (two connections in one process, the CLI as a participant,
  three processes, 100 re-runs, captures spanning two read transactions, more `-hold immediate`
  cases) — for the adversarial pass. `-hold` is exposed for it.
- darwin was cross-built and cross-vetted only; nothing was run on macOS.
- The busy-handler wait is shown by ordering (`no answer yet` while the other writer holds the
  lock), not by a busy-handler call count; the driver exposes no busy-handler hook.
