# Chunk 0 build report

Branch `chunk/ch00`, base `a7bb640`, builder Claude Opus 5.5, 2026-11-20. Details for the prose
pass: `agent_work/ch00/build-notes.md`. Nothing pushed or merged.

## Built (brief items 1–10)

| # | Item | Where | State |
| --- | --- | --- | --- |
| 1 | Module, pinned driver, ignores | `go.mod` (`go 1.26.0`, modernc.org/sqlite v1.60.1, libc v1.77.1), `go.sum`, `.gitignore` | done |
| 2 | Machine-readable pin | `sqlite/PIN`, `sqlite/pin.go` (embedded, parsed, tested against go.mod) | done |
| 3 | `internal/sqlitex` | explicit file + per-connection settings applied and verified on every pooled connection, single-connection option, pin check, raw wrappers (backup stepping, db_status, serialize/deserialize, DATA_VERSION, PERSIST_WAL, checkpoint busy/log/ckpt), error codes, work dir, repo root, pinned CLI locator/runner, tool locator | done; 1,079 lines vs the ~150–250 estimate (see notes for the split) |
| 4 | `internal/trace` | strace runner + parser (open/openat, pwrite64/write/writev, pread64, fsync/fdatasync, unlink/unlinkat, fcntl locks, ftruncate, mmap; fd→path), kill-at-syscall, CLI -vfstrace runner + parser (portable), merged VFS+syscall stream, `ErrLinuxOnly` + `LINUX-ONLY:` helper | done |
| 5 | `cmd/course` | fetch-source, build-tools, inline [-check], check sources/spans/comments/prose/evidence/gofmt/paths/all; logic in `internal/repocheck` with unit tests on temp dirs and temp git repos | done |
| 6 | Docker | `Dockerfile.lab` (Debian golang:1.26-bookworm, strace, build-essential, pinned tools baked in), `compose.yaml` (`lab` with SYS_PTRACE; `seaweedfs` 4.48, S3 on host port 18333 by default), `docker/seaweedfs/s3.json` | done, verified here |
| 7 | SeaweedFS S3 probe | `agent_work/probes/seaweedfs-s3/RESULT.md` | done |
| 8 | VFS viability probe | `agent_work/probes/vfs-viability/RESULT.md` | done |
| 9 | Cross-build script | `agent_work/verify/crossbuild.sh`, `README.md` | done |
| 10 | Smoke experiment | `agent_work/ch00/smoke/main.go`, `agent_work/ch00/smoke.txt` | done |

## Commands run

| Command | Result |
| --- | --- |
| `gofmt -l` (root module files) | clean |
| `go build ./... && go vet ./... && go test ./...` | pass |
| `go test -count=3 ./internal/...` | pass |
| `agent_work/verify/crossbuild.sh` | `CROSSBUILD OK` (darwin/arm64 + darwin/amd64 build, vet and test-compile pass; darwin never run) |
| `go run ./cmd/course fetch-source` | OK (6 s) |
| `go run ./cmd/course build-tools` | OK (2 min 08 s): sqlite3 reports the pin and has -vfstrace; showwal, dbhash built |
| `go run ./cmd/course check sources` | PASS (no chapter directories yet) |
| `go run ./cmd/course check all` | **FAIL, only `check paths`**: 9 pre-existing machine-local paths in OUTLINE.md (lines 53, 1008, 1019, 1020, 1082) and docs/knowledge (4 files, 1 line each). Not mine to edit (OUTLINE.md is off-limits; the research reports are prose). Everything else passes |
| `go run ./cmd/course check prose a7bb640..HEAD` | PASS (0 prose lines in range) |
| `go run ./agent_work/ch00/smoke` x2 | `SMOKE OK`, EXACT lines identical |
| `docker compose up -d seaweedfs` | OK (then stopped again with `docker compose stop seaweedfs`; volume kept) |
| `docker compose build lab` | OK (~5 min first build) |
| `docker compose run --rm lab go test ./internal/... ./sqlite/` | pass, live strace tests included |
| `docker compose run --rm lab go run ./cmd/course check all` (with the worktree's git dir mounted) | same as host |
| smoke in the container x2 | identical to each other and to the host output |
| S3 probe on host and in the container | identical semantics |
| VFS probe `writable` x2 on host, x1 in the container | identical |

## Corrections to the outline (full list with evidence in build-notes.md)

1. CLI `-vfstrace` writes to stdout, not stderr.
2. Default `./configure` builds of the pin call `fdatasync`; the installed CLI and the modernc
   driver call `fsync`. Same files, order and count.
3. modernc opens files with `open`, the C builds with `openat`.
4. The 3.53.4 shell probe-opens a database named on its command line (or via plain `.open`)
   read-only before the real open; the course passes `.open --normal` on stdin to avoid it.
5. `-bail` + `-cmd` silently skips the rest of the command line (`shell.c.in:13536`).
6. strace dies by SIGKILL itself (exit code -1 in Go), not "137".
7. A serialized WAL-mode image needs header bytes 18–19 set to 1 before `Deserialize` works.
8. SeaweedFS needs `-volume.max` headroom (7 volumes per bucket).
9. SeaweedFS listing is not pure byte order around `/`.
10. A writable custom VFS IS feasible on modernc v1.60.1 in pure Go (outline says not available).
11. The lab image uses Debian/glibc; Alpine/musl changes the CLI's syscalls.

## Decisions for the orchestrator / Nick

- **VFS verdict** (Nick's ruling): probe recommends option (1), real VFS experiments in
  chapter 7 — a Go forwarding VFS with call counts, deterministic Go fault injection, and a
  read-only remote-read demo — keeping CLI vfstrace + strace alongside. Risk: undocumented ccgo
  function-pointer convention and `go vet -unsafeptr=false` for that package.
- **S3 client:** aws-sdk-go-v2 (`service/s3` v1.116.0) recommended.
- **Twin check:** the smoke's twin preview already passes for one WAL insert; chapter 1.2
  must state which C build it compares against because of fsync vs fdatasync.
- **check paths** fails on OUTLINE.md and docs/knowledge until those mentions are removed.

## Not done / out of scope

- Per the brief: `internal/recipe`, chapter code/text, kb/ vendoring.
- The outline's chunk-0 row also lists a chapter template, Neon calling scripts and a Neon
  cost measurement; the brief did not include them, so they were not built.
- `build-tools` on macOS: written to be portable (`./configure`, `make`), not run on macOS.
- No darwin run of anything (cross-built only).
- The `fsync`/`fdatasync` choice of the installed `/usr/local/bin/sqlite3` was observed, not
  traced to its configure flags.
