# Neon prose pass notes — Chapter 7 (`ch07-vfs-and-filesystem-io`)

Author: Gemini 4 Neon (Deliverable B — chapter markdown and knowledge-check answers; Deliverable A code comments were already committed at `2140ae4`, and no `.go` files were touched).

## What was written

1. **`ch07-vfs-and-filesystem-io/ch07-vfs-and-filesystem-io.md`**
   - Pin block matching `sqlite/PIN` (SQLite `3.53.4`, tag `version-3.53.4`, git `b09c88c14082339b66c7b7158d609a771e64ca69`, Fossil `bf7c7f30031888f4e796e429ab3978879485813aaca6f641c7b33e4e09459bcc`, `modernc.org/sqlite v1.60.1`, `modernc.org/libc v1.77.1`), prerequisites callout (Go 1.26, `fetch-source`, `build-tools`, `docker compose up -d seaweedfs`, Linux-only notes and `docker compose run --rm lab` Mac commands, Linux measurement caveat).
   - All 8 required sections (`## 1. Model` through `## 8. Knowledge check`), including:
     - Architecture text diagram covering the stock `unix` VFS, forwarding shims (`vfstrace.c`, `internal/govfs`), and remote read-only VFS (`modernc.org/sqlite/vfs` + `internal/rangefs`).
     - WAL-default evaluation (ruling 10) and Rebuild Ladder note, plus Version notes from `docs/knowledge/source-inventory-ch05-09.md` §7.9.
     - Verified source spans and GitHub links (`version-3.53.4`) across `src/sqlite.h.in`, `src/os.c`, `src/os.h`, `src/os_unix.c`, `ext/misc/vfstrace.c`, `src/pager.c`, and `src/wal.c`.
     - All four experiments (`7.1`, `7.2`, `7.3`, `7.4`) placed at their outlined positions with claims, evidence forms (`EXACT`), **Why kept** on 7.2, run commands, inlined code, inlined captured evidence (both HTTP and `-s3` evidence for 7.4), and line-by-line evidence readings incorporating all build-notes corrections (WAL/`NORMAL` checkpoint + restart syncs, 3/4 sync syscalls vs 2/3 `xSync` calls, `vfstrace` `FULL`-for-`NORMAL` flag bug, `xAccess`→`stat` + empty-file rule, `xRandomness` build-position difference, Go/CLI VFS call parity).
     - Full inlining and file-by-file walk-throughs of both new shared packages: `internal/govfs` (6 files, with a dedicated walk-through of `cfunc.go`, `unsafe`, `ccgo`'s `funcval` convention, why `go vet` passes cleanly, the C-memory allocation safety invariant, what would break it, and the three compile-time/runtime/AST signature guards) and `internal/rangefs` (3 files).
     - Section 6 contract ("What a custom VFS must honor"), failure cases, and `### What attacked this chapter` with `<!-- adversarial-results -->`.
     - Section 7 platform implications, every item explicitly tied to the supporting experiments.
     - Section 8 five knowledge-check questions.
2. **`ch07-vfs-and-filesystem-io/answers.md`**
   - Complete, evidence- and source-backed answers to all five knowledge-check questions.

## Verification run

- `go run ./cmd/course inline ch07-vfs-and-filesystem-io/ch07-vfs-and-filesystem-io.md`: `PASS` (18 include blocks rewritten from disk).
- `go run ./cmd/course inline -check ch07-vfs-and-filesystem-io/ch07-vfs-and-filesystem-io.md`: `PASS` (all 18 include blocks match their files byte for byte).
- Verified zero `.go` files or other tracked files were modified (`git status --short` shows only the new markdown files plus the pre-existing untracked `agent_work/ch07/adversarial/results-run1.txt`).
- Checked both markdown files for forbidden host paths (`/root`, `/usr/local`, hostnames) and banned book citations: zero matches.

## Open doubts

- **Upcoming repair round:** The brief notes that a repair round follows this pass. If that repair modifies any `.go` file under `internal/govfs/`, `internal/rangefs/`, or `ch07-vfs-and-filesystem-io/experiments/`, or changes any `evidence/*.txt` file, `go run ./cmd/course inline ch07-vfs-and-filesystem-io/ch07-vfs-and-filesystem-io.md` must be re-run (and any affected prose walk-throughs or evidence line readings updated if outputs change).
- **Source span check without `$SQLITE_SRC`:** `go run ./cmd/course check sources ch07` and `check spans ch07` require `$SQLITE_SRC` or `_src/sqlite-3.53.4` to be present; every span cited in the chapter was checked against `ch07-vfs-and-filesystem-io/SPANS.txt` and the four copied C files in `ch07-vfs-and-filesystem-io/`.
- **Both HTTP and `-s3` evidence inlined for Experiment 7.4:** Both `evidence/7.4-read-only-snapshot-vfs.txt` and `evidence/7.4-read-only-snapshot-vfs-s3.txt` are inlined (113 and 114 lines) so both routes are verifiable by `inline -check` and visible to readers.

## Suspected defects

- **`internal/govfs/govfs.go:130-136` (minor C-memory leak on error path):** `Register` allocates `cname` via `libc.CString(name)` before checking `sqlite3_vfs_find(tls, cname) != 0`; if the name is already registered, `cname` is not freed. Similarly, if `sqlite3_vfs_register` fails at line 171, `cname` and `address` are not freed, and in `methodTableFor` (lines 389–405) a concurrent first-open race for the same `(v.address, real)` pair discards the losing allocation without freeing it. None of these affect normal operation or tests, since VFS registration happens once per name.
- **No defects observed** in the four experiments' claims or evidence as currently captured.
