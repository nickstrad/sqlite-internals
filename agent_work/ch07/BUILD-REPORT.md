# Chunk 07 build report — Chapter 7, VFS and filesystem I/O

Builder: Claude Opus 5.5, branch `chunk/ch07`, 2026-11-20. Details for the prose pass are in
`agent_work/ch07/build-notes.md`.

## What was built

- `ch07-vfs-and-filesystem-io/`: whole copies of `src/os_unix.c`, `src/os.c`, `src/os.h`,
  `ext/misc/vfstrace.c`; `SOURCES.sha256` (4 entries); `SPANS.txt` (46 spans: the outline's reading
  path re-verified — all correct — plus finer spans for every function the notes cite).
- Experiments (all EXACT, first line the pin check, evidence in `evidence/`):
  - **7.1 vfs-calls-to-syscalls** — CLI `-vfstrace` under strace in one stream; every xRead/xWrite
    is one pread64/pwrite64 of the same file, offset, length; every xSync one sync of its own file;
    the directory sync only inside the first xSync of a new journal/WAL; -wal/-shm created at first
    use and removed at last close. ~1 s.
  - **7.2 io-cost-by-pattern** — 12 cells (DELETE/WAL × NORMAL/FULL × 1/20/1000 commits for 1,000
    rows), counted per commit from strace of the Go driver. ~45 s.
  - **7.3 go-vfs-shim** — `internal/govfs`: the Go driver's per-commit VFS calls equal the CLI's
    `-vfstrace` call for call in all four configurations; failing every N-th xWrite/xSync of a
    recipe commit (122 fired faults over both journal modes) always leaves the exact pre-step recipe
    state. 24–34 s.
  - **7.4 read-only-snapshot-vfs** — a checkpointed 30-step recipe snapshot queried through
    `modernc.org/sqlite/vfs` + `internal/rangefs` over HTTP (default) and S3/SeaweedFS (`-s3`):
    requests and bytes per query, results equal to the local file, the `immutable=1` failure and the
    silent stale read with a live -wal. 2–7 s. Evidence for both routes (identical except the route
    line).
- New shared packages: `internal/govfs` (Go forwarding VFS, recording, fault injection, self-check,
  source-signature test) and `internal/rangefs` (range-reading `fs.FS`, HTTP and S3 sources).
- Shared package changes (backward compatible, tested): `internal/trace` (paths for stat/access
  syscalls; `CanonicalName`, `Syscall.Canonical`, `IsSync`); `internal/sqlitex` (`Options.VFS`,
  `DisplayPath`). `go.mod`: aws-sdk-go-v2 v1.48.0, credentials v1.21.3, service/s3 v1.116.0,
  smithy-go v1.28.9 (probe-pinned versions); `modernc.org/libc` became a direct requirement (same
  version).
- `agent_work/ch07/`: `build-notes.md`, `INDEX.md`, `span-ends.sh`, this report.

## Commands run

| Command | Result |
| --- | --- |
| `gofmt -l` over the repo's Go files; `go run ./cmd/course check gofmt` | clean; PASS (71 files) |
| `go build ./... && go vet ./... && go test ./...` | pass |
| `agent_work/verify/crossbuild.sh` | `CROSSBUILD OK` (linux/amd64 build+vet+test, darwin/arm64 and darwin/amd64 build+vet+test-compile, gofmt) — `go vet` with **default** analyzers everywhere; no `-unsafeptr=false` needed, so the script and `cmd/course` were not changed |
| `go run ./cmd/course check sources ch07` / `check spans ch07` | PASS (4 files) / PASS (46 spans) |
| `go run ./cmd/course check evidence ch07 -runs 2` | PASS for 7.1, 7.2, 7.3, 7.4 (2 runs identical to the evidence), run after the last code change |
| `go run …/7.4-read-only-snapshot-vfs -s3` twice | identical; identical to the HTTP route apart from the route line |
| `docker compose run --rm lab go run ./ch07-…/<each experiment>` (+ `7.4 -s3`) | exit 0; EXACT output identical to the host evidence for all five runs (container CLI is a `./configure` build using fdatasync; glibc 2.36) |
| `docker compose run --rm lab go test ./internal/govfs ./internal/rangefs ./internal/trace ./internal/sqlitex` | pass; the source-signature test and the live S3 test ran (not skipped) |
| `go run ./cmd/course check paths` | FAIL on 1 pre-existing tracked file, `_tools/bin/kb` (a binary added by the earlier infra commit c51ff44, not by this chunk); no ch07 file is reported |
| Signature-mismatch mutation of `internal/govfs` (temporary) | the source-signature test failed as intended; the mismatched build ran without crashing (recorded as the reason the test exists) |

`_work/` was cleaned at the end. SeaweedFS was started with `docker compose up -d seaweedfs` for the
`-s3` runs and left running.

## Corrections to the outline (details and evidence in build-notes.md)

1. WAL at `synchronous=NORMAL` does sync on two kinds of commit: the one that runs an automatic
   checkpoint (WAL + database, 2) and the next one, which restarts the WAL and syncs its new header
   (1) — `wal.c:4114-4124`, `pager.c:3685-3698`. 998 of 1,000 one-row commits made 0 syncs.
2. "Rollback 3 at NORMAL, 4 at FULL" are sync syscalls including the directory sync; in VFS calls it
   is 2 and 3 xSync.
3. vfstrace labels every default-configuration sync FULL: SQLite passes `SQLITE_SYNC_NORMAL` at
   `synchronous=FULL` too; `SQLITE_SYNC_FULL` only with `PRAGMA fullfsync=ON`.
4. `xAccess` (EXISTS) is a `stat`, not `access()`, and treats an empty file as absent.
5. The CLI's single `xRandomness` call happens at different points in the installed and the
   `./configure` CLI builds; 7.1 leaves it out of the printed stream.
6. Section 4.3's "no chapter builds a VFS" is superseded by ruling 11 (known).

## Left undone / open

- darwin: cross-built and cross-vetted only. govfs's startup self-check (function names behind the
  real slots) and its tests have never run on darwin; the names there may differ for non-POSIX
  locking styles (the check accepts any `modernc.org/sqlite/lib` function ending with the slot name).
- arm64 Linux (e.g. Docker on Apple silicon) was not run; `trace.CanonicalName` covers its
  `openat`/`unlinkat`/`newfstatat`/`faccessat` names, so 7.1/7.2 evidence should hold there, unverified.
- 7.3 part B covers xWrite and xSync at `synchronous=FULL`; xRead/xTruncate faults and NORMAL were
  not run (the package supports them).
- Neon's adversarial list from the outline (7.1: Go-driver twin under strace, killed process leaving
  -wal/-shm, vfstrace FULL/NORMAL against the real flag — the last one is already shown in 7.3; 7.2:
  CLI recomputation, `synchronous=OFF`, 20-run stability) was not built here.
- The knowledge-store step (ruling 6) is left to the landing.
