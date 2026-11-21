VERDICT: FAIL

## Findings

1. **should-fix** — `internal/connmgr/connmgr.go:166-179,192-207`
   - **What is wrong:** `verifyBeforeOpen` increments `m.stats.UnverifiedOpens` (line 195) or `m.stats.VerifiedReopens` (line 205) *before* `m.options.Open(ctx, path)` runs (line 170). Whenever `Open` fails (e.g., invalid/corrupt file, cancelled context, I/O error), `OpenFailures` increments and `Misses` does not, yet `UnverifiedOpens` or `VerifiedReopens` was already incremented. This corrupts `Stats()` and breaks the accounting invariant `Misses == UnverifiedOpens + VerifiedReopens` (which experiment 8.3 asserts at `8.3-bounded-manager/main.go:322`). In addition, if `CheckAtRest` returns an I/O error at line 199, `Acquire` counts it only as `RefusedReopens` without recording `StatusFailures`.
   - **How confirmed:** Read `internal/connmgr/connmgr.go:166-207` and `internal/connmgr/connmgr_test.go:275-290`:
     ```text
     166: if err := m.verifyBeforeOpen(id, path); err != nil {
     170: db, err := m.options.Open(ctx, path)
     171: if err != nil {
     172:     m.stats.OpenFailures++
     173:     return nil, fmt.Errorf("connmgr: open %s: %w", id, err)
     174: }
     175: m.stats.Misses++
     ...
     195:     m.stats.UnverifiedOpens++
     205: m.stats.VerifiedReopens++
     ```
     In `TestPathErrorAndOpenError`, `Acquire(ctx, "junk")` executes line 195 (`UnverifiedOpens` becomes 1) and then fails at line 170 (`OpenFailures` becomes 2, `Misses` stays 0), which the test misses because line 287 checks only `stats.OpenFailures != 2 || stats.Open != 0`.
   - **Expected fix:** Return `(verified bool, err error)` from `verifyBeforeOpen` without mutating `UnverifiedOpens`/`VerifiedReopens`, and increment the appropriate counter only after `m.options.Open` succeeds (right alongside `m.stats.Misses++`). Add assertions in `TestPathErrorAndOpenError` (and a failed-reopen test) checking `stats.Misses == 0 && stats.UnverifiedOpens == 0 && stats.VerifiedReopens == 0`.

2. **should-fix** — `ch08-connection-and-database-lifecycle/experiments/8.1-open-cost-and-where-settings-live/main.go:377,515` (and `ch08-connection-and-database-lifecycle/evidence/8.1-open-cost-and-where-settings-live.txt:71`)
   - **What is wrong:** The `observed` struct declares `Placement sqlitex.SettingPlacement \`json:"placement"\`` (line 377), but line 515 never initializes `Placement`. Every entry in the `observed` array of `settings-json` therefore serializes empty zero-value fields `"placement":{"name":"","pragma":"","home":""}`, polluting the machine-readable JSON intended for the capstone and contradicting `build-notes.md:233-235`.
   - **How confirmed:** Command:
     `cut -d' ' -f2- <<< "$(sed -n '71p' ch08-connection-and-database-lifecycle/evidence/8.1-open-cost-and-where-settings-live.txt)" | python3 -m json.tool`
     Output excerpt (repeated on all 12 entries):
     ```json
     {
         "setting": "page_size",
         "home": "file",
         "set_on_original": "8192",
         "fresh_connection_default": "4096",
         "read_on_copy": "8192",
         "read_on_vacuum_into": "8192",
         "placement": {
             "name": "",
             "pragma": "",
             "home": ""
         }
     }
     ```
   - **Expected fix:** Remove the dead `Placement` field from `observed` (the top-level `placements` array already carries the full `sqlitex.SettingPlacements()` table), or populate `Placement: placement` at line 515. Regenerate `ch08-connection-and-database-lifecycle/evidence/8.1-open-cost-and-where-settings-live.txt`.

3. **should-fix** — `internal/procstat/procstat.go:93,104`
   - **What is wrong:** `DescriptorsUnder(dir)` and `RelativeTargets(dir, descriptors)` build `prefix := filepath.Clean(dir) + string(filepath.Separator)` without resolving `dir` via `filepath.Abs`. On Linux, `os.Readlink("/proc/self/fd/N")` always returns an absolute path, so calling `DescriptorsUnder(".")` or `DescriptorsUnder("_work/8.2")` silently returns zero matches (`nil, nil`) and `RelativeTargets` leaves absolute paths unstripped. Additionally, `dir == "/"` produces `prefix == "//"`, matching nothing.
   - **How confirmed:** Read `internal/procstat/procstat.go:85-111`:
     ```text
     93:  prefix := filepath.Clean(dir) + string(filepath.Separator)
     96:  if strings.HasPrefix(d.Target, prefix) {
     104: prefix := filepath.Clean(dir) + string(filepath.Separator)
     ```
     `TestDescriptorsUnder` only tests with `t.TempDir()` (already absolute).
   - **Expected fix:** Resolve `abs, err := filepath.Abs(dir)` in `DescriptorsUnder` (avoiding a double separator when `abs == string(filepath.Separator)`), and resolve `filepath.Abs(dir)` (or use `filepath.Rel`) in `RelativeTargets`. Add a unit test in `procstat_test.go` exercising a relative directory path (e.g., via `t.Chdir`).

4. **should-fix** — `ch08-connection-and-database-lifecycle/experiments/8.1-open-cost-and-where-settings-live/main.go:294,297,299`
   - **What is wrong:** `pagesOf(reads)` (lines 172-183) computes `(pages []int, whole bool)` and silently skips any `.db` read that is not an aligned 4096-byte full read (`Offset%pageSize != 0 || Length != pageSize || Result != pageSize`), setting `whole = false`. Both call sites (lines 282 and 297) discard `whole` with `_`. Consequently, if the first statement issued additional partial, short, or unaligned reads of the `.db` file, line 299 would still report `check ok: ... the first statement reads exactly the sqlite_schema b-tree pages ... plus t0001's root page`. Similarly, line 294 checks `open.readsByDB[0].Offset == 0 && open.readsByDB[0].Length == 100` without verifying `open.readsByDB[0].Result == 100` (a 0-byte or short read would pass) or `open.opens[0] == filepath.Base(path)`.
   - **How confirmed:** Read `ch08-connection-and-database-lifecycle/experiments/8.1-open-cost-and-where-settings-live/main.go:172-183,282,294-300`:
     ```text
     294: check(len(open.opens) == 1 && strings.HasSuffix(open.opens[0], ".db") && len(open.readsByDB) == 1 && open.readsByDB[0].Offset == 0 && open.readsByDB[0].Length == 100 && len(open.otherReads) == 0,
     297: firstPages, _ := pagesOf(first.readsByDB)
     299: check(slices.Equal(sortedUnique(firstPages), sortedUnique(wantPages)),
     ```
   - **Expected fix:** Check `whole` at line 299 (`whole && slices.Equal(sortedUnique(firstPages), sortedUnique(wantPages))`), and check `open.readsByDB[0].Result == 100 && open.opens[0] == filepath.Base(path)` at line 294.

5. **should-fix** — `agent_work/ch08/build-notes.md:455`
   - **What is wrong:** Line 455 states: `modernc closes with sqlite3_close_v2; B2 is this zombie path seen from Go.` This is factually incorrect and contradicts both `8.3-bounded-manager/main.go:575` and `build-notes.md:325,504`. In B2, `(*sql.DB).Close()` closes only idle connections in `db.freeConn` and does **not** call `driver.Conn.Close()` (and therefore never calls `sqlite3_close_v2`) while `*sql.Rows` holds the connection checked out. When `rows.Close()` runs later, `database/sql` finalizes the statement first and only then calls `driver.Conn.Close()` → `sqlite3_close_v2` on a connection with zero open statements. Because the prose pass writes the chapter from `build-notes.md`, leaving this sentence uncorrected will introduce a false mechanism claim into Chapter 8.
   - **How confirmed:** Inspected Go stdlib `database/sql/sql.go:926-958` (`(*DB).Close`) and `650-685` (`finalClose`), plus `modernc.org/sqlite@v1.60.1/conn.go:1034-1036`:
     ```text
     sql.go:937: for _, dc := range db.freeConn { fns = append(fns, dc.closeDBLocked()) }
     sql.go:941: db.closed = true
     ```
     `(*sql.DB).Close()` never invokes `dc.finalClose()` on a busy connection held by `*sql.Rows`, and `finalClose()` closes all `openStmt` before invoking `ci.Close()` (`Xsqlite3_close_v2`).
   - **Expected fix:** Correct `agent_work/ch08/build-notes.md:455` to state that modernc calls `sqlite3_close_v2`, while B2 specifically demonstrates `database/sql`'s connection-pool deferral (which keeps the connection open until `rows.Close()` finalizes the statement and returns the connection, mirroring the same operational hazard one layer above `sqlite3_close_v2`).

6. **nit** — `internal/connmgr/connmgr.go:237-252`
   - **What is wrong:** If `e.db.Close()` returns a non-nil `CloseErr`, `evictLocked` still runs `CheckAtRest(e.path)` and, if `Status.AtRest` happens to be true (possible in rollback mode where no sidecar exists while idle), increments `EvictedAtRest` and records the file hash. Opening and closing `e.path` inside `CheckAtRest` when `e.db.Close()` failed also risks violating OUTLINE §4.1's POSIX lock-drop rule if the SQLite connection is still alive.
   - **How confirmed:** Read `internal/connmgr/connmgr.go:237-252`:
     ```text
     237: if err := e.db.Close(); err != nil {
     238:     eviction.CloseErr = err
     239:     m.stats.CloseFailures++
     240: }
     241: eviction.Status, eviction.StatusErr = CheckAtRest(e.path)
     ```
   - **Expected fix:** When `eviction.CloseErr != nil`, drop any stored record (`delete(m.records, e.id)`) and skip `CheckAtRest` (leaving `eviction.Status.AtRest == false` with a problem noting the close failure, or not recording it in `m.records`).

7. **nit** — `ch08-connection-and-database-lifecycle/experiments/8.1-open-cost-and-where-settings-live/main.go:524`
   - **What is wrong:** The check text states `VACUUM INTO keeps every file setting except journal_mode: its output is a rollback-mode file (header bytes 18-19 = 1)`, but the code never reads `vacuumPath`'s header bytes 18-19 (unlike `default_cache_size` at lines 547-554, which reads `header[48:52]`). It only checks `onVacuum["journal_mode"] == sqlitex.JournalDelete`.
   - **How confirmed:** Read `ch08-connection-and-database-lifecycle/experiments/8.1-open-cost-and-where-settings-live/main.go:518-524` and `build-notes.md:228` (`seen in a probe`).
   - **Expected fix:** Read the header of `vacuumPath` and assert `header[18] == 1 && header[19] == 1` in the check at line 524.

8. **nit** — `ch08-connection-and-database-lifecycle/experiments/8.2-idle-cost/main.go:317,417`
   - **What is wrong:** Flag validation allows `-n >= 2` (line 417), but the upper memory bound `uncounted <= int64(n)*uncountedBudget` has no constant term for fixed runtime/allocator growth, so small valid `-n` values fail the check.
   - **How confirmed:** Ran `go run ./ch08-connection-and-database-lifecycle/experiments/8.2-idle-cost -n 2`:
     ```text
     INFO: memory the three counters do not report: 0.7 MiB in total, 366 KiB per connection
     check ok: process memory grew by at least the sum of CACHE_USED + SCHEMA_USED + STMT_USED
     check FAILED: process memory grew by at most that sum + 256 KiB per connection (the stated margin for memory the counters do not report)
     1 check(s) FAILED
     exit status 1
     ```
     (`-n 10`, `-n 50`, and default `-n 200` all pass.)
   - **Expected fix:** Either raise the minimum `-n` validation (e.g., `-n >= 10` or `-n >= 20`) or include a small constant overhead allowance in the bound.

9. **nit** — `ch08-connection-and-database-lifecycle/experiments/8.3-bounded-manager/main.go:313`
   - **What is wrong:** The `INFO:` budget line formats 256 KiB via `mib(...)` (`%.1f MiB`), which rounds `0.25` to `0.2` and prints inconsistent-looking arithmetic: `budget 64 x (2 x 0.2 MiB cache + 0.2 MiB) + 32.0 MiB = 80.0 MiB` (`64 * 0.6 + 32 = 70.4`, not `80.0`).
   - **How confirmed:** Read `ch08-connection-and-database-lifecycle/evidence/8.3-bounded-manager.txt:15`:
     `INFO: bounded peak process-memory growth 22.6 MiB; budget 64 x (2 x 0.2 MiB cache + 0.2 MiB) + 32.0 MiB = 80.0 MiB`
   - **Expected fix:** Print the per-connection cache and uncounted terms in KiB (e.g., `%d KiB`).

10. **nit** — `ch08-connection-and-database-lifecycle/experiments/8.3-bounded-manager/main.go:622,637-638`
    - **What is wrong:** In B4 (line 622), the check claim states `keeps the -wal (with its bytes) and the -shm` while the boolean only checks `-shm exists` (not `-wal holds`). In B5 (line 637), `status.Problems[0]` is indexed without checking `len(status.Problems) > 0`, and line 638 claims `step 1 stayed in the -wal under the old name` without asserting `path(4)+"-wal"` is non-empty (it only prints `walSize` on line 636).
    - **How confirmed:** Read `ch08-connection-and-database-lifecycle/experiments/8.3-bounded-manager/main.go:622,636-638`.
    - **Expected fix:** Check both `-wal holds` and `-shm exists` in B4; guard `len(status.Problems) > 0` and assert non-empty `path(4)+"-wal"` in B5.

## Outline corrections audit

Checked all seven items under `## Corrections to the outline` in `agent_work/ch08/build-notes.md` against `$SQLITE_SRC` (`3.53.4`) and `internal/recipe`:
1. `main.c` `openDatabase` (3360-3716), `sqlite3_open` (3722-3728), `sqlite3_open_v2` (3729-3736) — **confirmed** (`main.c:3710-3736`).
2. `PRAGMA default_cache_size` (header bytes 48-51) seeding `cache_size` — **confirmed** (`prepare.c:323-332`, `pragma.c:553-585`).
3. WAL mode reading page 1 twice at first statement (`lockBtree` opens WAL and releases page 1; `btreeBeginTrans` loops) — **confirmed** (`btree.c:3333-3352`, `btree.c:3711`).
4. 8.2 memory growth margin (3.8-4.0x counter sum; ~165-175 KiB uncounted per connection) — **confirmed** by repeated runs of 8.2.
5. `PRAGMA shrink_memory` releasing all connections' unpinned pages under `SQLITE_ENABLE_MEMORY_MANAGEMENT` — **confirmed** (`pcache1.c:621-637,717-739,839-851`; `separateCache=0` also keeps `nInitPage=0`, so `pBulk` is unused).
6. Exceptions to R5 at-rest rule — **confirmed** (`pager.c:4222-4242`, `wal.c:2507-2571`), subject to Finding 5 on B2's mechanism in `build-notes.md:455`.
7. `recipe.MatchStep` overhead vs `StepHashes` + `MatchHashes` + `ChangeCountersAgree` — **confirmed** (`internal/recipe/hashes.go:201-261`).

## Commands run

- `git status && git log --oneline f6df8c3..HEAD && git diff f6df8c3..HEAD --stat`: PASS (clean working tree, 8 commits, 23 files added/modified; `internal/capture` unchanged at HEAD).
- `gofmt -l internal/procstat internal/connmgr internal/sqlitex ch08-connection-and-database-lifecycle`: PASS (no output).
- `go build ./... && go vet ./...`: PASS.
- `GOOS=darwin GOARCH=arm64 go build ./... && GOOS=darwin GOARCH=arm64 go vet ./...`: PASS.
- `go test -race -count=2 -v ./internal/procstat ./internal/connmgr ./internal/sqlitex`: PASS (all tests pass twice under `-race`).
- `export SQLITE_SRC=/root/Software/sqlite-src/3.53.4 && go run ./cmd/course check sources ch08`: PASS (`3 copied file(s) match the pin`).
- `export SQLITE_SRC=/root/Software/sqlite-src/3.53.4 && go run ./cmd/course check spans ch08`: PASS (`34 span(s) start at their symbol`).
- `go run ./ch08-connection-and-database-lifecycle/experiments/8.1-open-cost-and-where-settings-live` (2 runs, diffed non-`INFO:` lines against each other and `evidence/8.1-open-cost-and-where-settings-live.txt`): PASS (exit 0, identical).
- `go run ./ch08-connection-and-database-lifecycle/experiments/8.2-idle-cost` (2 runs, diffed non-`INFO:` lines against each other and `evidence/8.2-idle-cost.txt`): PASS (exit 0, identical).
- `go run ./ch08-connection-and-database-lifecycle/experiments/8.2-idle-cost -n 2` / `-n 10` / `-n 50`: FAIL at `-n 2` (Finding 8), PASS at `-n 10` and `-n 50`.
- `go run ./ch08-connection-and-database-lifecycle/experiments/8.3-bounded-manager` (2 runs, 73 s and 67 s, diffed non-`INFO:` lines against each other and `evidence/8.3-bounded-manager.txt`): PASS (exit 0, identical).
- `go run ./ch08-connection-and-database-lifecycle/experiments/8.3-bounded-manager -dbs 40 -k 1 -ops 200`: PASS (exit 0).
- `rg` checks on ch08 Go files and evidence outside `agent_work/`: PASS (no machine-specific paths, no cgo/unsafe, no comments other than `//go:build`).
