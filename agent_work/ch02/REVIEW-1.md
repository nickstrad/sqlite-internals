VERDICT: FAIL

Review of chunk `ch02` (`8f59174..7560be0`, covering `internal/dbfile/`, `internal/equal/`, `internal/capture/`, and `ch02-file-format-and-pages/`). All 6 outline corrections in `agent_work/ch02/build-notes.md` were checked against the pinned SQLite 3.53.4 source and live runs and are **correct**. Evidence files for 2.1, 2.2, and 2.3 reproduce identically across consecutive runs. However, the code has two blockers (a real parser bug on valid SQLite files and a direct violation of OUTLINE §4.1's descriptor rule across experiment 2.2) plus several parser edge-case bugs and weak/vacuous experiment checks.

## Findings

### 1. [blocker] `internal/dbfile/census.go:204` — `WITHOUT ROWID` tables are falsely rejected as `ErrCorrupt`
- **What is wrong:** `walkTree` asserts `if depth == 0 && p.Type.IsTable() != (t.Type == "table")`, assuming every `sqlite_schema` entry with `type = 'table'` is stored in a table B-tree (`0x05`/`0x0d`). Per SQLite's file format, a `WITHOUT ROWID` table has `type = 'table'` in `sqlite_schema` and a non-zero `rootpage`, but its B-tree is an **index B-tree** (`0x02`/`0x0a`). Because `capture.Pages` calls `f.Census()`, this also causes rung R1 capture to fail on any valid SQLite database containing a `WITHOUT ROWID` table.
- **How confirmed:**
  ```
  $ sqlite3 /tmp/opencode/without_rowid.db "CREATE TABLE kv(k TEXT PRIMARY KEY, v TEXT) WITHOUT ROWID; INSERT INTO kv VALUES('a', '1'), ('b', '2');"
  $ sqlite3 /tmp/opencode/without_rowid.db "PRAGMA integrity_check;"
  ok
  $ sqlite3 /tmp/opencode/without_rowid.db "SELECT pageno, name, pagetype, ncell FROM dbstat;"
  1|sqlite_schema|leaf|1
  2|kv|leaf|2
  $ go run ./agent_work/ch02/inspect /tmp/opencode/without_rowid.db
  ...
  census: tree "kv" (root 2): database file is malformed: page 2: root of table "kv" is a index-leaf page
  exit status 1
  ```
- **Expected fix:** Only reject `p.Type.IsTable()` when `t.Type == "index"` (an index cannot have a table B-tree root; a `table` entry may have either a table B-tree or an index B-tree). `TreeWalker` (`tree.go:19, 30`) already guarantees all pages within a single tree share the root's `IsTable()` flag. Add a unit test covering a `WITHOUT ROWID` table (including census vs `dbstat`).

### 2. [blocker] `ch02-file-format-and-pages/experiments/2.2-page-census/main.go:293-355, 403-422, 433-442, 452-464, 508-524` — `dbfile.Open` (`os.Open`) held open concurrently with SQLite connections in the same process
- **What is wrong:** Parts A, B, C, D, and E each call `f, c := census(path)` (which opens `path` via `os.Open`) with `defer f.Close()`, and then call `openSQLite(path, ...)` in the same process while `f` is still open. In Part A (`350-355`), `f` is actively read while `db` is open. In Part E (`522`), `os.Remove(path)` unlinks the 1 GiB sparse file before `defer f.Close()` runs. This directly violates OUTLINE §4.1 ("Never open and close the database file with `os.Open` in a process that also has a SQLite connection to it") and contradicts `build-notes.md:130` ("All experiments read databases only after closing their connections, or from a child process"). Readers copying experiment 2.2 into real code will copy the exact POSIX lock-drop bug taught in Chapter 6. (The same overlap exists in `internal/dbfile/dbfile_test.go:109-121, 188-202, 251-269, 413-432` via `openParsed` + `t.Cleanup`.)
- **How confirmed:** Code inspection of `2.2-page-census/main.go`:
  - `partA`: `f, c := census(path); defer f.Close()` (293-294) -> `db := openSQLite(path, false)` (309) -> `ourTableHash(f, ...)` (350) -> `closeAtRest(db, path)` (355).
  - `partB`: `census(path); defer f.Close()` (403-404) -> `checkAgainstSQLite(ctx, path, ...)` (422).
  - `partC`: `census(path); defer f.Close()` (433-434) -> `checkAgainstSQLite(...)` (442).
  - `partD`: `census(path); defer f.Close()` (452-453) -> `checkAgainstSQLite(...)` (464).
  - `partE`: `census(path); defer f.Close()` (508-509) -> `openSQLite(path, true)` (512) -> `os.Remove(path)` (522).
- **Expected fix:** Complete all `dbfile.File` reads and explicitly call `f.Close()` *before* opening any SQLite connection on the same path (and before `os.Remove(path)` in Part E). In Part A, compute `ourTableHash` and overflow stats before closing `f`, then open `db` for pragmas, `dbstat`, and `sqliteTableHash`. Apply the same non-overlapping discipline in `internal/dbfile/dbfile_test.go`.

### 3. [should-fix] `internal/dbfile/btree.go:113-130, 150-153` — Free-space corruption checks from `btreeComputeFreeSpace` omitted, allowing `Unused() > UsableSize`
- **What is wrong:** `btree.c:2146` requires `next >= pc + size + 4` (adjacent freeblocks or gaps < 4 bytes are invalid) and `btree.c:2166` requires `CellContentStart + FragmentedBytes + FreeblockBytes <= usableSize`. `ParseBTreePage` only checks `next < previousEnd` (allowing 0-byte gaps between freeblocks) and never checks that total free space fits in `usableSize`, so a corrupt `FragmentedBytes` byte produces `Unused() > usableSize` without error.
- **How confirmed:**
  1. Set byte offset 7 (`FragmentedBytes`) on empty page 2 to `200`:
     ```
     $ sqlite3 frag_bad.db "PRAGMA integrity_check;"
     *** in database main ***
     Tree 2 page 2: free space corruption
     $ go run ./agent_work/ch02/inspect frag_bad.db pages | tail -n 1
     2 table-leaf t ncell=0 payload=0 unused=4288 mx=0
     ```
  2. Two adjacent 4-byte freeblocks (gap 0) on page 2:
     ```
     $ sqlite3 fb_bad.db "PRAGMA integrity_check;"
     *** in database main ***
     Tree 2 page 2: free space corruption
     $ go run ./agent_work/ch02/inspect fb_bad.db pages | tail -n 1
     2 table-leaf t ncell=1 payload=14 unused=4070 mx=14
     ```
- **Expected fix:** In `ParseBTreePage`, check `if previousEnd > 0 && next < previousEnd+4` for freeblocks, and check `if p.CellContentStart+p.FragmentedBytes+p.FreeblockBytes() > usableSize` -> return `ErrCorrupt`. Add regression tests in `dbfile_test.go`.

### 4. [should-fix] `internal/dbfile/payload.go:54-62` — Non-zero `Next` pointer on the last overflow page is ignored
- **What is wrong:** SQLite's file format specifies that the last overflow page's 4-byte next pointer is `0`, and `checkList` (`btree.c:10729-10783`) rejects a non-zero next pointer. `OverflowChain` only checks `i < count-1 && next == 0` and silently accepts an arbitrary non-zero next pointer on the final overflow page.
- **How confirmed:** Wrote `9999` into bytes `0..3` of page 3 (the sole overflow page of a 5000-byte blob):
  ```
  $ sqlite3 ovfl_bad.db "PRAGMA integrity_check;"
  *** in database main ***
  Tree 2 page 2 cell 0: invalid page number 9999
  $ go run ./agent_work/ch02/inspect ovfl_bad.db pages | tail -n 2
  roles map[table-leaf:2 overflow:1] freelist [] [] unaccounted []
  3 overflow t ncell=0 payload=4092 unused=0 mx=0
  ```
- **Expected fix:** When `i == count-1 && page.Next != 0`, return `corruptAt(page.Number, 0, "last overflow page of cell %d has non-zero next pointer %d", c.Index, page.Next)`. Add a unit test.

### 5. [should-fix] `ch02-file-format-and-pages/experiments/2.2-page-census/main.go:405-422` — Part B assertions check hardcoded math instead of parsed database cells
- **What is wrong:** In Part B (`limit.db`), the `WalkTable` loop prints each row's `PayloadSize`, `LocalSize`, and `len(row.OverflowPages)`, but lines 419–421 only call `check(dbfile.LocalPayloadSize(4096, dbfile.TableLeaf, 4061) == 4061, ...)` (and `4062`, `8153`) — pure arithmetic on constants. Those three checks would still pass even if `limit.db` had 0 rows or wrong blob sizes; only `checkAgainstSQLite` at line 422 touches the database, and it does not verify that the three rows actually hit the exact boundary values.
- **How confirmed:** Code inspection of `2.2-page-census/main.go:405-422`.
- **Expected fix:** Capture each row's `(cell.PayloadSize, cell.LocalSize, len(row.OverflowPages))` during `WalkTable` and assert row 1 = `(4061, 4061, 0)`, row 2 = `(4062, minLocal, 1)`, row 3 = `(8153, 4061, 1)` directly.

### 6. [should-fix] `ch02-file-format-and-pages/experiments/2.2-page-census/main.go:609-613` — `parseAll` overwrites `err` in loop without breaking
- **What is wrong:**
  ```go
  for _, e := range c.Schema {
      if e.Type == "table" && e.RootPage > 0 {
          err = f.WalkTable(e.RootPage, func(dbfile.Row) error { return nil })
      }
  }
  ```
  If an earlier table fails `WalkTable` and a later table succeeds, `err` is overwritten with `nil` and `parseAll` reports `"accepted"`. This was only hidden in Part F because `reject-base.db` has one table.
- **How confirmed:** Code inspection of `2.2-page-census/main.go:608-618`.
- **Expected fix:** Break out of the loop as soon as `err != nil`.

### 7. [should-fix] `ch02-file-format-and-pages/experiments/2.1-header-and-what-a-commit-changes/main.go:244, 274-278, 367-386, 517` — Weak/vacuous checks in Parts A, C2, and F
- **What is wrong:**
  1. **Part A (`244, 274-278`):** `version-valid-for` has `pragma = "-"` and `cliName = ""` (`cliValue = "-"`), so `match` is vacuously `true` with no oracle checking it. When no pinned CLI is available, 11 of 19 rows have no oracle (`pragma="-"`, `cliValue="?"`) yet line 278 still prints `check ok: every decoded header field equals SQLite's own report of it`.
  2. **Part C2 (`367-386`):** Only checks `unchanged && bytes.Equal(afterClose, baseHeader)`. It never asserts that the `UPDATE`s modified any row, grew `-wal`, or changed the closed database file's SHA-256; if `WHERE id = 1` matched 0 rows, the check would still pass.
  3. **Part F (`517`):** `check(fmt.Sprint(h.PageSize) == pragma, ...)` does not assert `h.PageSize == size` nor (for `65536`) that bytes `16..17` store `1`. If `PRAGMA page_size=65536` silently fell back to `4096`, both sides would report `"4096"` and pass.
- **How confirmed:** Code inspection plus `SQLITE3_BIN=/nonexistent go run ./ch02-file-format-and-pages/experiments/2.1-header-and-what-a-commit-changes`:
  ```
  NO-CLI: $SQLITE3_BIN=/nonexistent does not name a file; the .dbinfo cross-check is skipped
  ...
  check ok: every decoded header field equals SQLite's own report of it
  ```
  And `sqlite3 basic.db .dbinfo` confirms `.dbinfo` does not emit `version-valid-for`.
- **Expected fix:** In Part A, print `n/a` (not `true`) for rows with no active oracle, and qualify the check text when `cli == nil`. In Part C2, assert `walSize(path) > 0` after commits and that the closed file's SHA-256 differs from `base`'s SHA-256 while the 100-byte header is identical. In Part F, assert `h.PageSize == size` and (for `size == 65536`) `binary.BigEndian.Uint16(raw[16:18]) == 1`.

### 8. [should-fix] `ch02-file-format-and-pages/experiments/2.3-page-manifest/main.go:188-190, 205, 261-274, 316, 345` — Tautological slice guard, unverified page-naming claim, and error-swallowing `!=` checks
- **What is wrong:**
  1. **Line 188:** After `lines = append(lines[:3:3], entries...)`, `len(lines)` is always `len(entries)+3`, so `if len(entries) < len(lines)` is always true. Any edit that adds an entry (true duplicate insertion instead of replacement) would panic on `pageBytes[:len(entries)*4096]`.
  2. **Line 205:** Part C claims "the refusal names the page", yet `check` only tests `err != nil && errors.Is(statErr, os.ErrNotExist)` without asserting that `err.Error()` contains the expected page number (`page 100`, `page 50`, `page 121`, `page 200`, `page 7`).
  3. **Lines 261–274, 316, 345:** `queryString` returns `"error: " + err.Error()` on SQL failure, and D1/D2 check `staleBalance != liveBalance` and `tornView != recovered`. If one query fails, `"error: ..." != "44047"` vacuously passes. `queryString` also manually appends `"?mode=ro"` instead of passing `url.Values{"mode": {"ro"}}` to `sqlitex.FileURI`, and discards `db.Close()`'s error.
- **How confirmed:** Code inspection of `2.3-page-manifest/main.go`.
- **Expected fix:** Guard with `len(entries)*4096 < len(pageBytes)`; add expected error substring (`wantPage`) per Part C case and assert `strings.Contains(err.Error(), c.want)`; have `queryString` call `fatal` on query/close error (or return `(string, error)`) and pass `url.Values{"mode": {"ro"}}` to `sqlitex.FileURI`.

### 9. [should-fix] `internal/capture/capture.go:119-135, 193-197, 237-241` — Partial capture left on disk on failure; small `Rebuild` cleanup gaps
- **What is wrong:**
  1. In `Pages()`, if `CheckAtRest` or the size/mtime check fails at lines 125–134 (or `os.WriteFile` fails at line 122), `pages.bin` (and `manifest.txt`) are left behind in `dir` even though `Pages()` returns an error. A caller seeing an error still has a potentially torn capture directory on disk.
  2. In `Rebuild()`, if `os.Rename(tmp, outPath)` fails (line 241), `tmp` is not removed; lines 193–197 ignore non-`ErrNotExist` `os.Stat` errors on side files; and a 0-page manifest is accepted and produces a 0-byte output file.
- **How confirmed:** Code inspection of `internal/capture/capture.go`.
- **Expected fix:** Clean up `pages.bin` and `manifest.txt` on any post-write failure in `Pages()` (or write temp files and rename after the final check). In `Rebuild()`, remove `tmp` if `os.Rename` fails, propagate non-`ErrNotExist` side-file `Stat` errors, and reject `m.PageCount() == 0`.

### 10. [nit] Test hygiene and minor code-clarity items
- `internal/dbfile/dbfile_test.go:612`: unused import workaround (`import "crypto/sha256"` + `_ = sha256.Size`); remove the import and blank assignment.
- `internal/dbfile/dbfile_test.go:171, 268, 283, 427, 472, 481`: ignored errors on `sql.Open` and `FromBytes`, and missing `rows.Err()` checks in `readDBStat` and `TestRowsMatchSQLite`.
- `internal/equal/equal_test.go:40-41` and `internal/capture/capture_test.go:77, 98-103, 126-151`: ignored errors on `os.WriteFile`, `os.OpenFile`, `WriteAt`, `Truncate`, `Remove`.
- `ch02-file-format-and-pages/experiments/2.2-page-census/main.go:325`: hardcodes `c.SortedTrees()[2]` for `documents` instead of looking it up by `Name == "documents"`.
- `ch02-file-format-and-pages/experiments/2.1-header-and-what-a-commit-changes/main.go:475, 491`: uses `strings.Contains(err.Error(), dbfile.ErrCorrupt.Error())` / `err != nil` instead of `errors.Is(err, dbfile.ErrCorrupt)`.

## Outline corrections verification (`agent_work/ch02/build-notes.md`)

1. **WAL header change counter / file unchanged at commit** (`pager.c:3093-3110, 3135, 3264`): confirmed in pinned source and 2.1 Part C.
2. **Counter constant after first page-1 commit on one connection**: confirmed (`29 -> 30 -> 30 -> 30` in 2.1 C1).
3. **Partial last page accepted by SQLite, rejected by `dbfile`**: confirmed (`pager.c:3351` rounds `(n + pageSize - 1) / pageSize`; 2.2 F2).
4. **Span `btreeParseCellPtr` split into two functions + 4 added spans**: confirmed (`btree.c:1259-1346`, `1347-1385`; `check spans ch02` passes 18/18).
5. **"Not at rest" definition (`-wal` size > 0 or hot `-journal` with first byte != 0)**: confirmed against `hasHotJournal` (`pager.c:5180-5252`).
6. **Hot journal requires journal sync (commit or cache spill)**: confirmed (`pager.c:1494-1502, 4377`; 2.3 D2).

## Commands run

Note: During this review, concurrent uncommitted edits from another session (comment edits in `internal/dbfile/*.go` and an in-progress `agent_work/ch02/adversarial/header-falsify`) appeared in the worktree; all verification below was checked against Opus's committed packages (`7560be0`), and `HEAD:internal/dbfile/errors.go` was verified `gofmt`-clean.

| Command | Result |
| --- | --- |
| `go build ./... && go vet ./... && GOOS=darwin GOARCH=arm64 go build ./... && GOOS=darwin GOARCH=arm64 go vet ./...` (before concurrent edits) | PASS (exit 0) |
| Targeted `linux/amd64` and `darwin/arm64` `go build` + `go vet` on `./internal/dbfile/... ./internal/equal/... ./internal/capture/... ./ch02-file-format-and-pages/...` | PASS (exit 0) |
| `go test -race ./internal/dbfile/... ./internal/equal/... ./internal/capture/...` | PASS (`dbfile` 33.1s, `equal` 1.0s, `capture` 2.8s) |
| `go run ./cmd/course check sources ch02` | PASS (3 copied files match pin) |
| `go run ./cmd/course check spans ch02` | PASS (18 spans start at symbol) |
| `go run ./cmd/course check evidence ch02` | PASS (2.1, 2.2, 2.3: 2 runs identical to evidence) |
| Manual 2-run comparison of 2.1, 2.2, 2.3 (`go run ./ch02-file-format-and-pages/experiments/<name>`) + `diff` against stored evidence | PASS (run1 == run2 byte-for-byte; non-INFO lines identical to evidence) |
| `SQLITE3_BIN=/nonexistent go run ./ch02-file-format-and-pages/experiments/2.1-header-and-what-a-commit-changes` | Exit 0, `NO-CLI:` line printed (exposed Finding 7) |
| `rg '/root\|vm-1\|nickstrad\.local' internal/dbfile internal/equal internal/capture ch02-file-format-and-pages` | Clean (no machine-specific paths) |
| Adversarial probes in `/tmp/opencode/` (`WITHOUT ROWID`, `FragmentedBytes` overflow, adjacent freeblocks, non-zero last overflow `Next`, multi-page `sqlite_schema`, views/triggers/`sqlite_sequence`, `.dbinfo`) | Exposed Findings 1, 3, 4, 7; multi-page `sqlite_schema` (100 tables, 4 pages) and views/triggers/`sqlite_sequence` passed |
