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

---

## Closing rewrite (Gemini 4 Neon, 2026-11-21)

### What changed

1. **`ch07-vfs-and-filesystem-io/ch07-vfs-and-filesystem-io.md`**
   - **Pin block and prerequisites:** Added `src/sqliteInt.h` and `src/sqliteLimit.h` to the linked C sources row so every linked source file cited in the chapter is listed; listed all four `go run` commands in the run block.
   - **Unified voice and first-use definitions:** Removed meta-history sentences describing earlier outline assumptions, backward-compatible helper additions, "during development", and internal review-1 references. Defined key terms at first use across Sections 1–7 (`checkpoint`, `at-rest`, `sidecar`, `memory-mapped I/O`, `short read`, `salt`, `change counter`, `cgo`, `ccgo`, `funcval`, `ABI`, `AST`, `overflow pages`, `LRU`, `E1`, `write amplification`, `inode` quotas).
   - **Layout matching Chapters 1–2:** Moved `### 3.5 Version notes` to the end of Section 3 with a proper relative link to `../docs/knowledge/source-inventory-ch05-09.md`. Linked all inline C source spans (`src/sqliteInt.h:1113-1115`, `src/sqliteLimit.h:161`, `src/sqlite.h.in:1008-1018`, `src/sqlite.h.in:3922-3929`, `src/pager.c:1399-1415`, `1891-1895`, `2086-2098`, `3685-3698`, `5033-5056`, `5436`, `5687`, `src/os_unix.c:3533-3548`, `3738-3740`, `3916`, `4602`, `4603`, `ext/misc/vfstrace.c:711-716`, `1091-1101`) to `github.com/sqlite/sqlite` at `version-3.53.4` and verified every line range against the pinned tree.
   - **Evidence reading corrections and de-patching:** Fixed two line-number citations in Experiment 7.1's reading (`xFileControl(t.db,MMAP_SIZE,0)` is at lines 26, 28, 32, and 124 rather than line 31; page 1 read in STEP 3 is at lines 178–179 rather than 148–175). Clarified in Experiment 7.4's reading that line numbers refer to the HTTP evidence (`-s3` is offset by +1 after line 2). Re-integrated previously patched-on mentions of `7.1/main_test.go`, `7.2/main_test.go` + `COST-05`, and `7.3/main_test.go` into clean dedicated bullets. Tightened repetition across Sections 5.5, 6, and 7 and cleaned `### What attacked this chapter` to report purely factual adversarial findings (`34` attacks; Run 1 `26 HELD, 8 BROKEN` → 4 code fixes + 4 scope narrowings → final `34 HELD, 0 BROKEN`).
2. **`ch07-vfs-and-filesystem-io/answers.md`**
   - Standardized C source citations to `src/...` / `ext/misc/...` throughout, added `src/pager.c:3685-3698`, `src/pager.c:3687-3688`, `src/pager.c:5033-5056`, `src/wal.c:4038-4266`, and `src/sqlite.h.in:3922-3929` where referenced.
3. **Code comments only (zero logic changes)**
   - `internal/sqlitex/open.go`: updated `DSN` doc comment to include `vfs=<name>`.
   - `ch07-vfs-and-filesystem-io/experiments/7.1-vfs-calls-to-syscalls/main.go`: updated package doc comment to note default `mmap_size=0` and the widened byte-moving syscall check.
   - `ch07-vfs-and-filesystem-io/experiments/7.4-read-only-snapshot-vfs/main.go`: updated package doc comment to note the `PRAGMA cache_size=4` cache-eviction demonstration.
   - Re-ran `go run ./cmd/course inline` to synchronize the 2 updated inlined experiment blocks.

### Verification run

- `go run ./cmd/course inline ch07-vfs-and-filesystem-io/ch07-vfs-and-filesystem-io.md`: `PASS` (18 include blocks, 2 rewritten).
- `go run ./cmd/course inline -check ch07-vfs-and-filesystem-io/ch07-vfs-and-filesystem-io.md`: `PASS` (all 18 blocks match).
- `go run ./cmd/course check gofmt`: `PASS` (108 tracked Go files formatted; repo-wide `gofmt -l .` lists only 5 pre-existing probe files under `agent_work/probes/`).
- `go run ./cmd/course check paths`: `PASS` (145 tracked files free of machine-local paths).
- `go run ./cmd/course check sources ch07` and `check spans ch07`: `PASS` (4 copied files, 46 spans).
- `go build ./... && go vet ./...` and `GOOS=darwin GOARCH=arm64 go build ./... && GOOS=darwin GOARCH=arm64 go vet ./...`: `PASS`.
- `go test -count=1 ./internal/govfs ./internal/rangefs ./internal/sqlitex ./internal/trace ./ch07-vfs-and-filesystem-io/...`: `PASS`.

### Open doubts

- None. Every claim, line span, and number matches the pinned SQLite 3.53.4 source and captured evidence. Note that `gofmt -l .` still reports the 5 pre-existing probe files under `agent_work/probes/` (untouched per brief scope), while `go run ./cmd/course check gofmt` reports all 108 tracked Go files cleanly formatted.
