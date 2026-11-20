VERDICT: FAIL

Reviewed commit `563721c` (`git diff eab2401..563721c`) covering `internal/recipe/`, `ch01-architecture-and-execution-path/experiments/`, `ch01-architecture-and-execution-path/evidence/`, `SOURCES.sha256`, `SPANS.txt`, and `agent_work/ch01/build-notes.md`.

## Findings

### 1. [blocker] `ch01-architecture-and-execution-path/experiments/1.2-one-insert/main.go:553` — Slice-bounds panic in WAL `pwrite64` check when `0 < len(walWrites) < len(wantWALWrites)`
- **What is wrong:** Line 553 checks:
  ```go
  if len(walWrites) == 0 || !slices.Equal(walWrites[len(walWrites)-len(wantWALWrites):], wantWALWrites) {
      walWritesMatch = false
  }
  ```
  If `len(walWrites)` is non-zero and smaller than `len(wantWALWrites)` (for example, 1 write recorded when 2 frame writes are expected), `len(walWrites) - len(wantWALWrites)` is negative and panics with `slice bounds out of range` instead of failing the `check`. In addition, taking only the tail slice ignores all preceding WAL writes, so unexpected writes before the frames are not caught (nor is the WAL header write `0+32` verified for a freshly created WAL).
- **How confirmed:** Code inspection of `git show 563721c:ch01-architecture-and-execution-path/experiments/1.2-one-insert/main.go` lines 541–557.
- **Expected fix:** Guard `len(wantWALWrites) == 0 || len(walWrites) < len(wantWALWrites)` before slicing, and verify the full `walWrites` sequence (for a fresh WAL: `0+32` followed by `wantWALWrites`, or explicitly allow only an optional leading `0+32` header write).

### 2. [blocker] `ch01-architecture-and-execution-path/experiments/1.3-recipe-rebuild/main.go:296-340,360-379` — Freelist and B-tree page parser contradicts SQLite's file format
- **What is wrong:** `classifyDifferences` uses `freelistPages` and `unusedBytes` to prove the variant-4 claim that `secure_delete=OFF` differences occur only in unused bytes, but the parser has three format bugs:
  1. `freelistPages` appends both freelist **trunk** pages and freelist **leaf** pages to `free`, and `unusedBytes` (lines 310–315) marks all 4096 bytes of every page in `free` as unused. Per SQLite's format (`fileformat2.html`), a freelist trunk page contains live metadata in bytes `0..8+4*leafCount-1` (next trunk pointer, leaf count, leaf page numbers); only bytes `8+4*leafCount..pageSize-1` are unused. Differing freelist trunk pointers or leaf lists would falsely pass `OnlyUnused = true`.
  2. `unusedBytes` (lines 316–339) assumes every non-freelist page is a B-tree page without checking that `content[header]` is one of `{0x02, 0x05, 0x0a, 0x0d}`. If an overflow page (or pointer-map page) differed, arbitrary payload bytes would be parsed as B-tree header fields (`freeblock`, `ncell`, `contentStart`) and `classifyDifferences` (line 374) would mislabel it `"b-tree page, unallocated space and freeblocks"`. Moreover, the freeblock walk (`for block := be16(content, header+1); block != 0 && block+4 <= pageSize; block = be16(content, block)`) has no strict monotonicity check (`next > block`) and can loop forever on cyclic/non-B-tree bytes.
  3. `freelistPages` (lines 298–303) has no bounds checks on `trunk` (`1 <= trunk <= pageCount`) or `leafCount` (`<= (pageSize-8)/4`), risking an out-of-bounds panic on malformed input.
- **How confirmed:** Code inspection of `git show 563721c:ch01-architecture-and-execution-path/experiments/1.3-recipe-rebuild/main.go` lines 296–380. (Independent inspection of `_work/1.3/batches-wal-secure-delete-false-*.db` confirmed that the stored evidence's two differing pages happen to be valid B-tree leaves: `page 24: type=0x0d freeblock=0 ncell=1 contentStart=3399`, 357 diffs in `[10..368]`; `page 28: type=0x0a freeblock=2292 ncell=251 contentStart=1742`, 1207 diffs in `[530..1741]`, so the stored claim holds for those files even though the parser does not enforce the format.)
- **Expected fix:** Separate freelist trunk pages from leaf pages (marking only bytes `8+4*leafCount..pageSize-1` unused on trunk pages and all bytes unused on leaf pages), bounds-check `trunk` and `leafCount`, validate `content[header]` against `{0x02, 0x05, 0x0a, 0x0d}` (returning all-false unused and reporting non-B-tree pages accurately otherwise), and enforce `next > block` when walking freeblocks.

### 3. [blocker] `ch01-architecture-and-execution-path/experiments/1.2-one-insert/main.go:468-481,674-698` — Non-Linux (`!trace.Supported`, darwin) path fails with exit 1 on an unused CLI and ignores `db.Close()` error
- **What is wrong:** `common.md` requires that an experiment needing a Linux-only mechanism must, on non-Linux systems, print a `LINUX-ONLY:` line, run every portable part, and **exit 0**. In `main()`, lines 674–681 call `sqlitex.LocateCLI(ctx)` unconditionally and append `"the pinned sqlite3 CLI is available"` to `failures` on error, causing line 697 to exit 1. On macOS, `/usr/bin/sqlite3` is Apple's system SQLite (wrong source ID), so any Mac user who has not built `_tools/bin/sqlite3` gets exit 1 even though `runMode` returns at line 480 when `!trace.Supported` and never uses `cli`. Additionally, on line 476 (`!trace.Supported`), `db.Close()` discards its error return (unlike every other `db.Close()` in the file), and the portable branch performs no `check(...)` assertions on `changed`.
- **How confirmed:** Code inspection of `main.go:468-481,674-698`, and running with a wrong-version CLI:
  ```
  SQLITE3_BIN=/tmp/opencode/fake-sqlite3 go run ./ch01-architecture-and-execution-path/experiments/1.2-one-insert
  -> exit 1
  sqlite3 CLI: unavailable: sqlite3 CLI /tmp/opencode/fake-sqlite3 reports sqlite_source_id "2020-01-01 ...", the pin is "2026-07-24 ..."
  ONE INSERT FAILED: 1 claim(s) do not hold: the pinned sqlite3 CLI is available
  ```
- **Expected fix:** Gate `sqlitex.LocateCLI(ctx)` (and the CLI failure) behind `if trace.Supported`, check `if err := db.Close(); err != nil { fatal("close", err) }` on line 476, and assert the portable changed-page check (`len(changed) > 0` or expected changed pages per mode) in the `!trace.Supported` branch so the portable path both checks its claim and exits 0 on darwin.

### 4. [should-fix] `agent_work/ch01/build-notes.md:16-27` — Correction 1 overstates WAL change-counter behavior by omitting the `pList->pgno == 1` condition
- **What is wrong:** Correction 1 states: "In WAL mode the file change counter counts connection sessions, not commits... `pagerWalFrames` (pager.c:3264) calls `pager_write_changecounter`, which writes `dbFileVers + 1` (pager.c:3135)... On one long-lived connection every commit writes the same value; each new connection re-reads page 1 and moves it by one." This omits that `pager.c:3264` is conditional:
  ```c
  if( pList->pgno==1 ) pager_write_changecounter(pList);
  ```
  In WAL mode, a connection session that commits without dirtying page 1 does **not** increment the change counter at all—as Experiment 1.2 WAL mode itself demonstrates (`pages changed by the INSERT: [2]`, page 1 untouched on a brand-new connection). Conversely, on one long-lived connection, `dbFileVers` is also refreshed if page 1 is evicted or invalidated and re-read through `readDbPage` (for example after another connection writes). Experiment 1.3 variant 6 advances the counter per session specifically because every recipe step executes `PRAGMA user_version = k`, dirtying page 1.
- **How confirmed:** Read `/root/Software/sqlite-src/3.53.4/src/pager.c:3093-3143,3264` and compared against `ch01-architecture-and-execution-path/evidence/1.2-one-insert.txt:179` (`pages changed ...: [2]`) and `recipe-probe` header dump (`wal.db cc=34` vs `walrecipe-options-one.db cc=4`).
- **Expected fix:** Qualify Correction 1 in `build-notes.md` (and `BUILD-REPORT.md` if appropriate): in WAL mode, the change counter is written only on commits that already dirty page 1 (`pager.c:3264`), and uses `dbFileVers + 1` where `dbFileVers` is refreshed only when page 1 is read via `readDbPage` (new connection or cache reload), not after a WAL commit.

### 5. [should-fix] `ch01-architecture-and-execution-path/experiments/1.2-one-insert/main.go:292-315,495,574,587-596,606` — `normalized()` discards syscall errors/results and `write`/`read` lengths; twin check does not assert non-zero syncs or at-rest state
- **What is wrong:**
  1. `normalized(call)` only records `call.Errno` for `open`/`openat`; for `pwrite64`, `pread64`, `ftruncate`, `fsync`/`fdatasync`, `unlink`, and `fcntl`, `call.Errno` and `call.Result` are ignored. A failed syscall in one build and a succeeding one in the other would normalize to the same string. Also `write` and `read` (despite `isWrite` including `"write"`) drop `call.Length`.
  2. `check(writesSame && syncsSame, ...)` at line 589 does not verify `len(goSyncs) > 0`; if both builds silently ran without syncs, `compareLists` would report `identical (0 calls)` and pass.
  3. After `childInsert`, `cliRun`, and `splitRun` (lines 495, 574, 606), `t.db` is hashed without calling `recipe.CheckAtRest(path)`. In WAL mode, if close failed to checkpoint in both Go and CLI, `goFileHash == cliFileHash` could match stale base files.
- **How confirmed:** Code inspection of `git show 563721c:ch01-architecture-and-execution-path/experiments/1.2-one-insert/main.go` lines 292–338, 494–499, 574–609.
- **Expected fix:** Include `call.Errno` (when non-empty) in `normalized()` for all syscalls and include `len=%d` for `write`/`read`; require `len(goSyncs) > 0` in the twin check (and `goFileHash != baseHash`); call `recipe.CheckAtRest(path)` before hashing `t.db` after each run.

### 6. [nit] `ch01-architecture-and-execution-path/experiments/1.1-pin-check/main.go:107-152` — `:memory:` database opened without `db.SetMaxOpenConns(1)`
- **What is wrong:** `sql.Open("sqlite", ":memory:")` uses `database/sql`'s multi-connection pool without `db.SetMaxOpenConns(1)`. It works here only because all queries are strictly sequential after `rows.Close()`. Readers copying this snippet into concurrent code or interleaved queries would silently get a second empty in-memory database.
- **How confirmed:** Code inspection of `git show 563721c:ch01-architecture-and-execution-path/experiments/1.1-pin-check/main.go` lines 107–152.
- **Expected fix:** Add `db.SetMaxOpenConns(1)` right after `sql.Open("sqlite", ":memory:")`.

### 7. [nit] `internal/recipe/recipe.go:163-172`, `apply.go:104-111`, `hashes.go:242`, `recipe_test.go:78-79,257-258` — Minor validation, temp-file cleanup, and test error-handling gaps
- **What is wrong:**
  1. `Recipe.Step(k)` (`recipe.go:163-172`) and `Recipe.BuildEach(..., upTo, ...)` (`apply.go:104-111`) call `r.WithSteps(k).Plan()` without calling `r.Validate()` first, so a `Recipe` with `Steps > MaxSteps` (e.g. `20000`) is accepted by `Step(0)` and `Build(ctx, path, 5)` even though `Validate()` rejects it.
  2. In `writeHashCache` (`hashes.go:242`), if `os.Rename(temp.Name(), path)` fails, the temp file `temp.Name()` is left behind.
  3. `recipe_test.go:78-79` (`TestSeedsDiffer`) and `:257-258` (`TestLongLivedConnectionDiffersOnlyInChangeCounterUnderWAL`) discard errors from `Plan()` and `HashFile()` with `_`.
- **How confirmed:** Code inspection of `git show 563721c:internal/recipe/{recipe.go,apply.go,hashes.go,recipe_test.go}`.
- **Expected fix:** Call `if err := r.Validate(); err != nil { return ... }` at the start of `Step` and `BuildEach`; remove `temp.Name()` when `os.Rename` returns an error; check errors in `recipe_test.go` with `if err != nil { t.Fatal(err) }`.

### 8. [nit] `ch01-architecture-and-execution-path/experiments/1.3-recipe-rebuild/main.go:47-59,78-100,175` — Partial-page panic risk in `compareFiles`/`fileDiff.String()` and unchecked `conn.Close()`
- **What is wrong:** `compareFiles` computes `pages := max(len(dataA), len(dataB)) / pageSize` (integer division), which ignores trailing bytes when a file size is not a multiple of `pageSize`. If two files differed only in trailing bytes, `d.Equal` would be `false` with `len(d.DifferingPages) == 0`, causing `d.DifferingPages[0]` in `fileDiff.String()` (line 51) to panic. Separately, `applyStatementsInSession` ignores the error from `conn.Close()` at line 175.
- **How confirmed:** Code inspection of `git show 563721c:ch01-architecture-and-execution-path/experiments/1.3-recipe-rebuild/main.go` lines 47–59, 77–100, 175.
- **Expected fix:** Round `pages` up (`(max(len(dataA), len(dataB)) + pageSize - 1) / pageSize`), guard `len(d.DifferingPages) == 0` in `fileDiff.String()`, and check `if err := conn.Close(); err != nil { fatal("conn close", err) }`.

## Commands run

All checks were run against commit `563721c` with `export SQLITE_SRC=/root/Software/sqlite-src/3.53.4`:

| Command | Result |
| --- | --- |
| `gofmt -l internal/recipe ch01-architecture-and-execution-path agent_work/ch01` | Empty (pass) |
| `go build ./... && go vet ./...` | Pass |
| `GOOS=darwin GOARCH=arm64 go build ./... && GOOS=darwin GOARCH=arm64 go vet ./...` | Pass |
| `bash agent_work/verify/crossbuild.sh` | `CROSSBUILD OK` (linux/amd64, darwin/arm64, darwin/amd64, gofmt) |
| `go test -v -race ./internal/recipe/...` | PASS (all 12 tests, 12.059s) |
| `go run ./cmd/course check sources ch01` | `PASS: 4 copied file(s) match the pin` |
| `go run ./cmd/course check spans ch01` | `PASS: 12 span(s) start at their symbol` (all 12 start and closing lines also verified directly in `$SQLITE_SRC`) |
| `go run ./cmd/course check prose eab2401..HEAD` | `PASS: 0 prose line(s) introduced in range` |
| `go run ./ch01-architecture-and-execution-path/experiments/1.1-pin-check` (×2, diff non-`INFO:` vs stored evidence & each other) | Both exit 0; 0 diff |
| `go run ./ch01-architecture-and-execution-path/experiments/1.2-one-insert` (×2, diff non-`INFO:` vs stored evidence & each other) | Both exit 0; 0 diff |
| `go run ./ch01-architecture-and-execution-path/experiments/1.3-recipe-rebuild` (×2, diff non-`INFO:` vs stored evidence & each other) | Both exit 0; 0 diff |
| `go run ./cmd/course check evidence ch01` | PASS for 1.1, 1.2, 1.3 (2 runs each identical to evidence) |
| `COURSE_WORK_DIR=/tmp/opencode/ch01-alt-work go run ./ch01-architecture-and-execution-path/experiments/1.3-recipe-rebuild` (cold cache, then warm cache) | Both exit 0; 0 non-`INFO:` diff against stored evidence |
| `SQLITE3_BIN=/tmp/opencode/fake-sqlite3 go run ./ch01-architecture-and-execution-path/experiments/1.1-pin-check` | Exit 1 (`PIN CHECK FAILED: ... the pinned sqlite3 CLI is available`) |
| `SQLITE3_BIN=/tmp/opencode/fake-sqlite3 go run ./ch01-architecture-and-execution-path/experiments/1.2-one-insert` | Exit 1 (`ONE INSERT FAILED: ... the pinned sqlite3 CLI is available`) |
| `go run ./agent_work/ch01/recipe-probe` + Python inspection of `_work/ch01-recipe-probe/wal*.db` headers and `_work/1.3/batches-wal-secure-delete-false-*.db` pages 24 and 28 | Confirmed `wal*.db` change counters (`cc=34` per-session vs `cc=4` one-conn) and confirmed pages 24 (`0x0d`) and 28 (`0x0a`) diffs lie in unallocated B-tree space |
| `grep` for `/root`, `//`, `/*` in `ch01-architecture-and-execution-path/**/*.go`, `*.txt`, and `internal/recipe/` | 0 matches |
