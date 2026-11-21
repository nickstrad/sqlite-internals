VERDICT: NITS

## Findings

1. **should-fix** — `ch03-btrees-and-page-management/experiments/3.1-pages-per-insert/main.go:478-484, 517`
   - **What is wrong:** The check at line 517 claims `"that one page is the table leaf the row went into (page 1 is not written)"`, but `ordinaryLeafOnly` (line 481) only checks `p.Type != dbfile.TableLeaf`. Page 1 (`sqlite_schema`) is also a `dbfile.TableLeaf`, so a non-split insert that modified only page 1 (or any unrelated table leaf) would still pass this check.
   - **Confirmed by:** Inspecting `main.go:478-484` alongside `evidence/3.1-pages-per-insert.txt:32`, which prints page 1's census role as `1(table-leaf)`. (In the actual run, page 1 is not written on ordinary inserts — e.g. insert #14 changes `[6(table-leaf)]` — so the claim holds on the data, but the assertion does not enforce it.)
   - **Expected fix:** Check `s.changes[0].Page != 1` and verify that the single changed page belongs to table `t` (e.g., `l.census.Page(s.changes[0].Page).Owner == "t"` or checking that `rowid` is present on that leaf).

2. **should-fix** — `ch03-btrees-and-page-management/experiments/3.1-pages-per-insert/main.go:655-676`
   - **What is wrong:** Part D's check at line 676 claims `"in rollback mode every commit also rewrites page 1 (change counter, bytes 24-27 and 92-95), so an ordinary insert changes two pages"`, but `ok` (line 661) only checks `!hasPage1 || s.integrity != "ok"`. It does not assert that non-split inserts changed exactly 2 pages (the printed histogram shows 10 inserts changed 2 pages, 1 changed 3, 1 changed 4, so the evidence supports it, but `check()` would still pass if ordinary inserts changed more than 2 pages), nor does it check that page 1's diff on non-split commits is restricted to bytes 24–27 and 92–95.
   - **Confirmed by:** Inspecting `main.go:655-676` and `evidence/3.1-pages-per-insert.txt:110-113`.
   - **Expected fix:** Require `len(s.changes) == 2` whenever `s.after.PageCount() == s.before.PageCount()` (and verify with `equal.ComparePages` on page 1 before/after that non-split commits touch only the declared header bytes on page 1).

3. **should-fix** — `ch03-btrees-and-page-management/experiments/3.1-pages-per-insert/main.go:580-588` (also `332`, `561`)
   - **What is wrong:** In Part B, `before, _ := s.before.Page(n)` and `after, _ := s.after.Page(n)` discard errors and never assert `bytes.Equal(before, after)`. If `extra` ever contained an out-of-range page number, both calls would return `nil, err` and both SHA-256 outputs would silently print `e3b0c44298fc1c14` (the digest of empty bytes) while the line 588 check still passed. (`p.Cells()` errors are also discarded at lines 332 and 561, though those pages were parsed earlier by `keysOf`.)
   - **Confirmed by:** Inspecting `main.go:580-588`; in the actual run, page 3 prints `c0995adc833eadb1` (real page bytes), not empty bytes.
   - **Expected fix:** Check both `Page(n)` errors with `fatal`, assert `bytes.Equal(before, after)` as part of the line 588 check, and handle `Cells()` errors at lines 332 and 561.

4. **should-fix** — `internal/equal/logical.go:414-423`
   - **What is wrong:** `LogicalHash(ctx, conn)` opens a transaction with `BEGIN`, runs `logicalHashInTx(ctx, conn)`, and closes it with `conn.ExecContext(ctx, "COMMIT")` using the same `ctx`. If `ctx` is canceled or times out mid-walk, the cleanup `COMMIT` fails immediately with `ctx.Err()` without reaching SQLite, leaving an open read transaction (and pinned WAL read mark) leaked on `conn`. Using `ROLLBACK` on error is also safer than `COMMIT`.
   - **Confirmed by:** Inspecting `internal/equal/logical.go:414-423`.
   - **Expected fix:** End the transaction using `context.WithoutCancel(ctx)` with `ROLLBACK` when `err != nil` and `COMMIT` when `err == nil`.

5. **should-fix** — `ch03-btrees-and-page-management/experiments/3.3-same-rows-different-bytes/main.go:215`
   - **What is wrong:** `dump()` constructs each `INSERT` statement via `strings.Replace(insertSQL(t), strings.TrimSuffix(strings.Repeat("?, ", len(values)), ", "), strings.Join(literals, ", "), 1)`. Because `strings.Replace` replaces the first match anywhere in the string, any valid SQLite table or column name containing `?` (for a 1-column `WITHOUT ROWID` table) or `?, ?` (for a 2-column table) has its identifier corrupted instead of the `VALUES(...)` placeholders.
   - **Confirmed by:** Inspecting `main.go:92-103, 210-216`. (Does not trigger on R0's schema, so stored evidence is unaffected, but this is reader-facing dump code.)
   - **Expected fix:** Factor a helper `insertStatement(t equal.TableInfo, exprs []string) string` used by both `insertSQL` and `dump` so `dump` builds the `VALUES(...)` clause directly without string replacement.

6. **should-fix** — `ch03-btrees-and-page-management/experiments/3.1-pages-per-insert/main.go:123-137` and `ch03-btrees-and-page-management/experiments/3.3-same-rows-different-bytes/main.go:68-75`
   - **What is wrong:** Neither `rows.Next()` loop checks `rows.Err()` after iteration. In `3.1` line 135 only `rows.Close()` is checked (which does not return iteration errors in `database/sql`); in `3.3` line 75 `rows.Close()`'s error is also ignored.
   - **Confirmed by:** Inspecting `3.1/main.go:118-140` and `3.3/main.go:67-75`.
   - **Expected fix:** Check `rows.Err()` after both loops and check `rows.Close()` in `3.3/main.go:75`.

7. **nit** — `internal/equal/logical.go:387-412`
   - **What is wrong:** `EncodeValue` is exported, but `streamEncoder.value` has no `default` case: passing an unsupported Go type (e.g., `int(1)` instead of `int64(1)`, or `bool`) silently returns an empty `[]byte{}` so distinct unsupported types encode identically. (`LogicalHash` itself is protected by `StorageClassOf` in `walkTable:250`, so this affects only direct callers of `EncodeValue`.) Also `EncodeValue` writes into a `strings.Builder` and converts `b.String()` back to `[]byte` where `bytes.Buffer` avoids the extra copy.
   - **Confirmed by:** Inspecting `internal/equal/logical.go:387-412`.
   - **Expected fix:** Either return `([]byte, error)` from `EncodeValue` or panic in `streamEncoder.value`'s `default` branch, and use `bytes.Buffer`.

8. **nit** — `internal/equal/logical.go:145, 181, 217`
   - **What is wrong:** `logicalTables` restricts `pragma_table_list` to `schema = 'main'`, but `tableColumns`/`primaryKeyColumns` call `pragma_table_info(?)` without specifying `'main'` and `TableSelect` queries `FROM "t"` unqualified. On a caller-supplied `*sql.Conn` that has a `TEMP` table shadowing a `main` table name, SQLite resolves both to `temp`.
   - **Confirmed by:**
     ```
     $ sqlite3 :memory: "CREATE TABLE t(a INT); CREATE TEMP TABLE t(x TEXT, y TEXT); SELECT 'default:', name FROM pragma_table_info('t'); SELECT 'main:', name FROM pragma_table_info('t', 'main');"
     default:|x
     default:|y
     main:|a
     ```
     (Does not affect `LogicalHashFile`, which opens a clean connection with no temp tables.)
   - **Expected fix:** Use `pragma_table_info(?, 'main')` and `FROM main."t"`.

9. **nit** — `ch03-btrees-and-page-management/experiments/3.2-delete-and-reuse/main.go:185-192, 396-402`
   - **What is wrong:** Step 5 deletes 220 `NEW-`/`GROW-` rows (`id >= 1001`, freeing 55 pages), and step 6 runs `VACUUM` and checks `o.afterVacuum.rowsFound == 0` using `searchDeleted`. However, `searchDeleted` only scans for `SECRET-000099..000302` (from step 2; by step 6 only the 2 live-leaf freeblocks still hold `SECRET-` bytes in `OFF`, and `ON` already had 0), never verifying that the `NEW-`/`GROW-` markers deleted in step 5 are gone after `VACUUM`. Also `finding.String()` (line 191) emits a trailing `: ` when `roles` is empty (`on 0 page(s): ` in `evidence/3.2-delete-and-reuse.txt:30, 41, 46, 59`).
   - **Confirmed by:** Inspecting `3.2/main.go:159-192, 381-403` and `evidence/3.2-delete-and-reuse.txt:30, 41, 46, 59`.
   - **Expected fix:** Include `NEW-` and `GROW-` deleted ranges in step 6's post-VACUUM search (or search them before and after VACUUM), and omit `: ` when `len(roles) == 0`.

10. **nit** — `ch03-btrees-and-page-management/experiments/3.3-same-rows-different-bytes/main.go:53, 597-617` and `internal/equal/logical.go:644-661`
    - **What is wrong:** (a) `row.columns []string` at `3.3/main.go:53` is unused. (b) In Part B (`3.3/main.go:597-617`), `check()` calls are printed inside the table loop, interleaving `check ok:` lines between table rows (`evidence/3.3-same-rows-different-bytes.txt:66-84`). (c) `CompareLogical` (`logical.go:644-649, 659`) checks `a.Pragmas` against `b` only (not pragmas present only in `b`) and compares `ForeignKeyCheck` via `strings.Join(..., "\n")` instead of `slices.Equal`.
    - **Confirmed by:** Inspecting the cited lines and `evidence/3.3-same-rows-different-bytes.txt:66-84`.
    - **Expected fix:** Remove unused `row.columns`, emit Part B's `check` lines after the table, and use `slices.Equal` in `CompareLogical`.

## Outline corrections check

All 6 corrections recorded in `agent_work/ch03/build-notes.md` were independently verified and are accurate:
1. **WAL append-only batching (`3.3` part A & A2):** Confirmed by inspecting the generated file headers in `_work/3.3/builds/`: both `one-transaction.db` and `per-statement.db` have `cc=2 sc=4 vvf=2` and identical SHA-256, whereas `rollback-one-transaction.db` and `rollback-per-statement.db` have `cc=3` vs `cc=1463` (E2 equal, differing only at `[26 27 94 95]`).
2. **VACUUM INTO canonical limits (`3.3` part A & C):** Confirmed in `$SQLITE_SRC/src/vacuum.c:298-323, 358-376` (tables created in `sqlite_schema` order without `ORDER BY`; `aCopy[]` copies `BTREE_SCHEMA_VERSION` with `+1`) and `$SQLITE_SRC/src/backup.c:448` (`sqlite3BtreeUpdateMeta(p->pDest, 1, p->iDestSchema+1)`).
3. **`dbhash` extra blind spots (`3.3` part B):** Confirmed in `$SQLITE_SRC/tool/dbhash.c:281-360, 456-484` (no length prefix for TEXT/BLOB, no row or table delimiter, `sqlite_*` tables and pragmas omitted, implicit rowids not selected).
4. **WAL frames shipped > changed pages (`3.1` part B):** Confirmed in `ch03-btrees-and-page-management/btree.c:8666-8671` (`balance_nonroot` calls `sqlite3PagerWrite(pNew->pDbPage)` on every reused sibling before `editPage`/`rebuildPage`, so page 3 is written as a WAL frame even with identical bytes).
5. **First append split = first depth change (`3.1` part A):** Confirmed (`balance_deeper` followed by `balance_quick` on insert #5).
6. **Reading-path spans (`SPANS.txt`):** Confirmed (`check spans ch03` passes all 22 spans; `balance_nonroot` comment is 8204–8244 before function start 8245).

## Commands run

- `git diff 7690293..HEAD --stat` — 20 files changed (`internal/equal/`, `ch03-btrees-and-page-management/`, `agent_work/ch03/`).
- `gofmt -l internal/equal ch03-btrees-and-page-management` — empty (pass).
- `GOOS=darwin GOARCH=arm64 go build ./... && GOOS=darwin GOARCH=arm64 go vet ./...` — pass.
- `go build ./... && go vet ./...` — pass.
- `go test -count=1 -race ./internal/equal/...` — `ok github.com/nickstrad/sqlite-internals/internal/equal 28.803s` (all 14 tests including all 6 blind-spot subtests pass with `-race`).
- `go test ./...` — all packages pass.
- `export SQLITE_SRC=/root/Software/sqlite-src/3.53.4 && go run ./cmd/course check sources ch03` — `PASS check sources ch03-btrees-and-page-management: 2 copied file(s) match the pin`.
- `export SQLITE_SRC=/root/Software/sqlite-src/3.53.4 && go run ./cmd/course check spans ch03` — `PASS check spans ch03-btrees-and-page-management: 22 span(s) start at their symbol`.
- `export SQLITE_SRC=/root/Software/sqlite-src/3.53.4 && go run ./cmd/course check evidence ch03` — PASS for `3.1-pages-per-insert`, `3.2-delete-and-reuse`, and `3.3-same-rows-different-bytes` (2 runs each, identical to stored evidence).
- `grep` for `/root|vm-1|nickstrad/Software` across `internal/equal/` and `ch03-btrees-and-page-management/` — no matches.
- `grep` for `//|/\*` across new `.go` files in `ch03-btrees-and-page-management/` and `internal/equal/` — no comments in any new code (`git diff 7690293..HEAD -- internal/equal/equal.go` confirms existing ch02 comments untouched).
- Ad-hoc probe via `/tmp/opencode/probe.go` (removed after run) verifying `modernc.org/sqlite@v1.60.1` scans `X''`/`zeroblob(0)` as typed `[]byte(nil)` (distinct from untyped `nil`), `-0.0` as `float64(-0)`, and `+"d"` on a `DATETIME` column as `string`.
- Ad-hoc Python header dump of `_work/3.3/builds/*.db` verifying change counters and schema cookies for Correction 1 & 2.
- Ad-hoc `sqlite3 :memory:` checks for `sqlite_sequence` replay, `pragma_table_info('t')` vs `pragma_table_info('t', 'main')`, and `INSERT INTO "accounts"(rowid, "id", ...)`.
