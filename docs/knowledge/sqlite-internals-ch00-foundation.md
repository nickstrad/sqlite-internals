---
title: "sqlite-internals chunk 0: foundation findings (modernc driver, CLI tracing, strace, SeaweedFS, Go VFS)"
summary: "What building the course foundation established: CLI -vfstrace goes to stdout, fsync vs fdatasync differs by build, the 3.53.4 shell probe-opens databases, a writable Go VFS works on modernc without cgo, SeaweedFS S3 quirks."
tags: [sqlite, go, modernc, strace, vfs, seaweedfs, sqlite-internals]
updated: 2026-11-20
verified: 2026-11-20 — Linux amd64, Go 1.26, SQLite 3.53.4 via modernc.org/sqlite v1.60.1; evidence in the sqlite-internals repo
---

# sqlite-internals chunk 0: foundation findings (modernc driver, CLI tracing, strace, SeaweedFS, Go VFS)

What building the course foundation established: CLI -vfstrace goes to stdout, fsync vs fdatasync differs by build, the 3.53.4 shell probe-opens databases, a writable Go VFS works on modernc without cgo, SeaweedFS S3 quirks.

Source: the `sqlite-internals` course repo (github.com/nickstrad/walfleet), chunk `ch00`. Working notes: `agent_work/ch00/build-notes.md`; review and adversarial records in the same directory.

## Corrections to the outline

1. **The CLI's VFS trace goes to stdout, not stderr.** `shell.c.in:12911` `vfstraceOut` writes with
   `cli_puts(z, p->out)` and `fflush` after every fragment; `p->out` is stdout
   (`shell.c.in:13331`). Each VFS call is written as two or three fragments: the call text
   before the real method runs (`ext/misc/vfstrace.c:420`), then ` -> rc` (and details) after
   it. docs/knowledge/go-access-and-vfs.md §1e says stderr. `internal/trace` parses stdout.
2. **Two C builds of the same pin sync differently.** The installed `/usr/local/bin/sqlite3`
   calls `fsync`; a default `./configure && make` of the pinned tree (what
   `go run ./cmd/course build-tools` produces, on glibc and on musl) calls `fdatasync`. The
   modernc Go driver calls `fsync`. Same files, same order, same count in every case (smoke
   output). Any chapter that names the sync syscall must say which build it ran.
3. **modernc's Go driver opens files with `open`, not `openat`** (linux/amd64, flags
   `O_RDWR|O_CREAT|O_LARGEFILE|O_NOFOLLOW|O_CLOEXEC`); the C builds use `openat`. strace filters
   must include both.
4. **The 3.53.4 CLI opens a database named on its command line twice.** Before the real open
   the shell calls `isScriptFile` → `isDatabaseFile` (`shell.c.in:4229-4320`), which does a
   read-only `sqlite3_open_v2` + `SELECT count(*) FROM sqlite_schema`; `.open FILE` does the same
   through `deduceDatabaseType` (`shell.c.in:4258`, called from `open_db` at `:4495`). In WAL mode
   that probe maps the `-shm` and writes its 8 allocation bytes a second time, so a strace of
   the CLI shows extra syscalls that are not part of the statement. The probe is skipped by
   `.open --normal FILE` (`shell.c.in:10222` sets `SHELL_OPEN_NORMAL`). `sqlitex.OpenScript`
   and every CLI runner in `internal/trace` therefore pass the database as
   `.open --normal '<path>'` on stdin. With that, the Go driver's and the CLI's writes for one
   WAL insert are identical (smoke "twin preview").
5. **`-cmd` + `-bail` drops the rest of the command line.** `shell.c.in:13536`:
   `rc = runOneSqlLine(...); if( bail_on_error ) goto shell_main_exit;` exits after any SQL
   `-cmd`, so `sqlite3 -bail -cmd "PRAGMA x" db "INSERT ..."` exits 0 without running the
   INSERT. The course's CLI runner always passes `-batch -bail -init /dev/null` and never uses
   `-cmd`.
6. **strace exits by SIGKILL itself, not with status 137.** When the tracee is killed by an
   injected SIGKILL, strace re-raises the signal: Go's `ProcessState.ExitCode()` is `-1`. The
   "137" in docs/knowledge/library-route-probes.md is the shell's 128+9 rendering. `trace.Result`
   reports `ExitCode -1` and `Killed "SIGKILL"`.
7. **A serialized WAL-mode database does not open after `Deserialize`** unless header bytes
   18 and 19 are rewritten from 2 to 1. Query after deserializing the WAL-format image:
   `unable to open database file (14)`. After that failure, a second `Deserialize` on the same
   connection also failed; a fresh connection with the patched image works
   (`TestRawWrappers`).
8. **SeaweedFS needs volume headroom per bucket.** Each bucket is a collection that grows 7
   volumes on first write; with `-volume.max=20` the third bucket fails every PUT with HTTP 500.
   compose.yaml uses `-volume.max=100`. Details in `agent_work/probes/seaweedfs-s3/RESULT.md`.
9. **SeaweedFS ListObjectsV2 is not pure byte order** when `/` and bytes below 0x2f meet at the
   same key position (`k/a`, `k/a/x`, `k/a-`). Same RESULT.md.
10. **A custom writable VFS IS possible on modernc v1.60.1 without cgo or a fork** (the
    outline's §4.1/§4.3 and docs/knowledge say not available / UNVERIFIED). Built and run:
    `agent_work/probes/vfs-viability/RESULT.md`. The orchestrator records the verdict.
11. **The lab image is Debian (glibc), not Alpine.** An Alpine (musl) build of the CLI writes
    stdout with `writev` (the merge parser now handles `writev` too), opens with `open`, and
    musl's `pwrite` issues `fcntl(F_GETFL)` before every write. Those differences would make the
    container's traces differ from a typical Linux server, so `Dockerfile.lab` uses
    `golang:1.26-bookworm`. The smoke output inside the container is identical to the host's.


## Probe verdicts recorded with this chunk

- Custom VFS on modernc v1.60.1: viable in pure Go. Details: `agent_work/probes/vfs-viability/RESULT.md`.
- SeaweedFS 4.48 S3 semantics and the Go S3 client choice (aws-sdk-go-v2): `agent_work/probes/seaweedfs-s3/RESULT.md`.
- Docker: `compose.yaml` services `lab` (Linux toolchain with strace) and `seaweedfs` (S3 on host port 18333 by default).
