VERDICT: FAIL

Reviewed commit `71f6ae8` (`git diff 7690293..HEAD`, branch `chunk/ch04`); line numbers below refer to `HEAD` (`71f6ae8`) before any concurrent prose-pass comment insertions.

## Findings

1. **blocker** — `internal/journalfile/journalfile.go:119-160, 292-308`
   - **What is wrong:** `Parse` checks only `end < HeaderFieldsSize` (28 bytes) for the first journal header and never checks `end < sector` (whereas segment 2+ enforces `next+sector > end` at line 158). For a journal truncated inside the first header sector (`28 <= end < first.SectorSize`, e.g., 100 bytes with valid 28-byte fields and `SectorSize = 512`), `position := offset + sector` exceeds `end`, so:
     1. `j.TrailingByte = end - position` becomes **negative** (`-412`) at line 154 (and also at line 159 when `RecordCount == 0`).
     2. `Parse` records segment 0 as `HeaderSynced` / `HeaderNoSync` / `HeaderNotYetSynced` instead of incomplete.
     3. `Playback()` sets `TruncateTo = first.InitialPages` and predicts a rollback/truncation, contradicting SQLite's `readJournalHdr` (`pager.c:1607-1610`: `if( pPager->journalOff+JOURNAL_HDR_SZ(pPager) > journalSize ) return SQLITE_DONE;`), which aborts before reading the header and before `pager_truncate` (`pager.c:2952`).
   - **How confirmed:** Created a 3-page (12288-byte) database with `sqlite3` and wrote a 100-byte hot journal (`Magic`, `RecordCount=1`, `Nonce=0x12345678`, `InitialPages=1`, `SectorSize=512`, `PageSize=4096`, zero-padded to 100 bytes), then reopened with `sqlite3`:
     ```
     short-journal test: sz_before= 12288 sz_after= 12288 out= '3\nok\n' journal_exists= False
     ```
     SQLite did not truncate the database to 1 page and kept all 3 pages intact; `Parse` on those 100 bytes produces `TrailingByte = -412`, `Header.State = HeaderSynced`, and `Playback().TruncateTo = 1`.
   - **Expected fix:** After reading and validating `first.SectorSize` / `first.PageSize`, check `if end < sector` and treat the first header as incomplete (`HeaderMissing`, `TrailingByte = end`, no playable segment) so `TrailingByte` is never negative and `Playback()` matches `pager.c:1608`. Guard `end - position` against negative values and add a unit test covering `HeaderFieldsSize <= len(data) < sectorSize`.

2. **blocker** — `internal/journalfile/journalfile.go:224-244`
   - **What is wrong:** `superJournalName` disagrees with `pager.c`'s `writeSuperJournal` (`pager.c:1709-1710, 1735-1737`) and `readSuperJournal` (`pager.c:1303-1349`) in two ways:
     1. **Wrong checksum signedness (line 238):** `pager.c:1709-1710` specifies that the super-journal name checksum sums each byte as a *signed* 8-bit integer (`cksum += zSuper[nSuper]`, `cksum -= zOut[u]`, where `char` is signed on Linux x86_64 and macOS). `journalfile.go:238` sums `uint32(b)` (unsigned), so it rejects genuine SQLite super-journal footers whenever the path contains any non-ASCII UTF-8 byte (`>= 0x80`).
     2. **Missing leading-NUL check (lines 235-243):** `pager.c:1337` rejects any super-journal name whose first byte is NUL (`zOut[0]==0`). `superJournalName` does not check `name[0] == 0`, so a footer with `name = {0,0,0,0}`, `length = 4`, `stored = 0`, and `Magic` passes, trims to `""`, and silently strips `length + 20` bytes from `end` while leaving `j.SuperJournal == ""`.
     3. *(Related note)* `Playback()` ignores `j.SuperJournal`; `pager.c:2891-2897` skips playback completely when a super-journal is named and absent on disk, so `Playback()` should at least note or surface that playback is conditional on the super-journal file existing.
   - **How confirmed:** Traced `sqlite3` committing an `ATTACH` two-DB transaction in `/tmp/opencode/café-f_6pfgpk/`:
     ```
     pwrite64(5, "\x00\x04\x00\x01", 4, 9216) = 4
     pwrite64(5, "\x2f\x74\x6d\x70\x2f\x6f\x70\x65\x6e\x63\x6f\x64\x65\x2f\x63\x61\x66\xc3\xa9\x2d\x66\x5f\x36\x70\x66\x67\x70\x6b\x2f\x61\x2e\x64\x62\x2d\x6d\x6a\x46\x34\x37\x43\x31\x36\x39\x36\x45", 45, 9220) = 45
     pwrite64(5, "\x00\x00\x00\x2d", 4, 9265) = 4
     pwrite64(5, "\x00\x00\x0d\x98", 4, 9269) = 4
     pwrite64(5, "\xd9\xd5\x05\xf9\x20\xa1\x63\xd7", 8, 9273) = 8
     SQLite wrote: 0x00000d98 (3480)
     unsigned_sum: 0x00000f98 (3992)
     signed_sum:   0x00000d98 (3480)
     ```
   - **Expected fix:** Reject `name[0] == 0` in `superJournalName`, compute the checksum using signed 8-bit bytes (`sum += uint32(int8(b))`) to match `pager.c:1709-1710` (and test with a non-ASCII UTF-8 name and a NUL-prefixed footer), and record the super-journal caveat in `PlaybackOutcome` when `j.SuperJournal != ""`.

3. **should-fix** — `ch04-pager-and-page-cache/experiments/4.2-cache-budget/main.go:63-65, 431-433, 470-478`
   - **What is wrong:** Flag validation at line 431 permits `-rows` down to `10` (`*rowsFlag < 10`), but lines 470-475 calibrate `perPage = (large - small) / 100` assuming both `cache_size=100` and `cache_size=200` fill completely (which requires > 200 pages). When run with `-rows 50`, both runs cache the entire 52-page file, so `large - small == 0`, `(large - small) % 100 == 0` passes, `perPage` becomes `0`, and `cachedPages(small)` panics with integer divide by zero.
   - **How confirmed:**
     ```
     $ go run ./ch04-pager-and-page-cache/experiments/4.2-cache-budget -n 4 -k 2 -rows 50
     CACHE_USED counts 0 bytes per cached page ...
     panic: runtime error: integer divide by zero
     ```
   - **Expected fix:** Guard `large <= small` before dividing (fail cleanly if `perPage <= 0`) and enforce a `-rows` floor large enough that the database exceeds both the `cache_size=200` calibration cache and the requested `-cache-size` budget (or reject incompatible `-rows` / `-cache-size` combinations at flag validation).

4. **should-fix** — `internal/journalfile/journalfile_test.go:257-273`
   - **What is wrong:** `liveJournal` calls `before, err := os.ReadFile(path)` at line 269 while `db` is still open on `path` in the same process. This violates OUTLINE §4.1 ("Never open and close the database file with `os.Open` in a process that also has a SQLite connection to it. Closing any descriptor drops that process's POSIX locks on the file."). Even though `db` is idle between transactions here (so rollback mode holds `NO_LOCK`), this sets a hazardous pattern in a shared package test. (Reading `path + "-journal"` at line 281 is safe because SQLite locks only the main database file.)
   - **How confirmed:** Inspection of `internal/journalfile/journalfile_test.go:257-273` against `OUTLINE.md:282-285`.
   - **Expected fix:** Close `db` after populating the table and reading `PRAGMA page_count`, read `before` with `os.ReadFile(path)`, and reopen `db` before starting the transaction.

5. **should-fix** — `ch04-pager-and-page-cache/experiments/4.1-atomic-commit-and-hot-journal/main.go:874, 1134-1137, 1236, 1448`
   - **What is wrong:** Four `check(...)` predicates are weaker than their printed claim strings (all four claims hold in the actual captured evidence, but each check could pass if the underlying property broke):
     - Line 874: `records%3 == 0 && records > 0` claims each journal record is three `pwrite64` calls of `(4, 4096, 4)` bytes, but checks only the count divisibility by 3, not the actual lengths of each triple.
     - Lines 1134-1137: `usesJournal` checks only `len(ops) >= 2 && HasPrefix(ops[0], "open ") && HasPrefix(ops[len(ops)-1], "unlink ")`, while the claim says `(opened, written, synced, unlinked)`; zero writes and zero syncs would still pass.
     - Line 1236: `allOriginal` starts `true` without checking `j.RecordsPresent() > 0`, so an empty journal would pass vacuously.
     - Line 1448: `describeStates` checks `match.Step == s.preStep` and `match.Step == s.step` without `&& match.Exact`; `recipe.MatchHashes` (`internal/recipe/hashes.go:199-210`) falls back to change-counter-masked matching when exact SHA-256 differs.
   - **How confirmed:** Inspection of `4.1/main.go` at lines 874, 1134-1140, 1220-1236, 1448 and `internal/recipe/hashes.go:199-211`.
   - **Expected fix:** Verify each `journal-record` triple has lengths `(4, pageSize, 4)`; verify `usesJournal` sees at least one `pwrite64` and one `sync`; require `j.RecordsPresent() > 0` in `partC`; require `.Exact` on both `matchFile` calls in `describeStates`.

6. **nit** — `ch04-pager-and-page-cache/experiments/4.1-atomic-commit-and-hot-journal/main.go:635-640, 1304-1309`
   - **What is wrong:** `runKill` checks `result.landed == target.text`, which does not distinguish two syscalls with identical formatted text in the same commit (such as the two `sync t.db-journal` calls at FULL kills 23 and 26, though the on-disk journal state distinguishes them in the evidence). `recreateKilled` (lines 1304-1309) checks only `run.Killed == "SIGKILL"` without confirming the unfinished syscall matched `target.text`.
   - **How confirmed:** Inspection of `4.1/main.go:630-641, 1304-1309` and `evidence/4.1-atomic-commit-and-hot-journal.txt:87, 93`.
   - **Expected fix:** Verify the unfinished call's per-thread ordinal matches `target.ordinal` in `runKill`, and assert the landed call in `recreateKilled`.

7. **nit** — `ch04-pager-and-page-cache/experiments/4.2-cache-budget/main.go:521`
   - **What is wrong:** `find(oneFile, nLabel).cacheTotal() == find(files, nLabel).cacheTotal()` compares raw `CACHE_USED` bytes instead of page counts, and only at `nLabel`. This works for `N <= 1000` only because `f-%03d.db` has constant filename length for `0..999`; for `N > 1000`, `sqlite3PagerMemUsed` varies with filename length (Correction 7).
   - **How confirmed:** Inspection of `4.2/main.go:128-130, 521` and `pager.c:6881-6887`.
   - **Expected fix:** Compare `pagesTotal()` across every checkpoint of `oneFile` and `files`.

8. **nit** — `agent_work/ch04/build-notes.md:202`
   - **What is wrong:** Cites `sqlite3_randomness, pager.c:1523`, whereas in the pinned `pager.c` `sqlite3_randomness` is on line `1508` (line 1523 is inside the `#ifdef SQLITE_DEBUG` comment block below it).
   - **How confirmed:** Read `ch04-pager-and-page-cache/pager.c:1506-1525`.
   - **Expected fix:** Update `pager.c:1523` to `pager.c:1508`.

## Verification of outline corrections (`agent_work/ch04/build-notes.md`)

All 8 corrections in `build-notes.md` were checked against the pinned 3.53.4 source and live runs and are **accurate**:
1. Freelist-leaf reuse via `PAGER_GET_NOCONTENT` (`btree.c:6740`, `pager.c:5553-5568, 5645-5656`) skips journaling, so hot-journal rollback leaves reused freelist-leaf bytes modified while preserving logical/canonical equality; byte-exact rollback holds when no freelist exists (step 1). Confirmed.
2. Rollback commit order includes the 12-byte `journal-count` header write (between the two journal syncs in FULL = 4 syncs; before the single journal sync in NORMAL = 3 syncs; OFF = 0 syncs). Confirmed (`pager.c:4377-4420` and 4.1 evidence).
3. Pre-COMMIT journal header has zeroed magic/count in FULL/NORMAL (`pager.c:1495-1502`), and `hasHotJournal` tests first byte != 0 (`pager.c:5244-5251`), leaving a harmless cold journal on pre-`journal-count` crashes. Confirmed.
4. `PRAGMA journal_mode=wal` on a new file commits page 1 through a rollback `-journal`. Confirmed (4.1 part A).
5. `modernc.org/sqlite` compiles with `SQLITE_ENABLE_MEMORY_MANAGEMENT` (`pcache1.c:718-719`), unifying all connections into one `PGroup` LRU (`pcache1.c:899-915`); the installed `sqlite3` CLI does not (`PRAGMA compile_options` has no `ENABLE_MEMORY_MANAGEMENT`). Confirmed. (Note: this also explains the `+7` pages in 4.2 part D after connection closes: closing a 0-page evicted connection shrinks `pGroup->nMaxPage` to `7*483 = 3381`, not `7*482 = 3374`, and the new connection adds `482`, giving `3863 = 3856 + 7`.)
6. RSS ≈ 2 × summed `CACHE_USED` under `modernc.org/memory`. Confirmed (ratio 1.98 in 4.2 part B).
7. `CACHE_USED` varies with path length via `sqlite3MallocSize(pPager)` (`pager.c:6881-6887`). Confirmed.
8. A cache of `N` pages holds `N-1` (`pcache1.c:902`: `pCache->nPage+1>=pCache->nMax`). Confirmed.

## Commands run

- `go test -v -race ./internal/journalfile/...` — PASS (`1.388s`, all 13 tests pass, no races)
- `go build ./... && go vet ./... && go test ./...` — PASS
- `GOOS=darwin GOARCH=arm64 go build ./... && GOOS=darwin GOARCH=arm64 go vet ./...` — PASS
- `GOOS=darwin GOARCH=amd64 go build ./... && GOOS=darwin GOARCH=amd64 go vet ./...` — PASS
- `go run ./cmd/course check sources ch04` — PASS (4 copied files match the pin)
- `go run ./cmd/course check spans ch04` — PASS (44 spans start at their symbol; also verified all 44 end lines against copied C files)
- `go run ./cmd/course check gofmt` — PASS (88 Go files formatted)
- `go run ./cmd/course check evidence ch04` — PASS (both 4.1 and 4.2 ran twice with EXACT output identical to stored evidence)
- `go run ./cmd/course check paths` — exit 1 solely on pre-existing binary `_tools/bin/kb`; grep confirmed zero `/root/` paths in `internal/journalfile/` or `ch04-pager-and-page-cache/`
- `go run -race ./ch04-pager-and-page-cache/experiments/4.1-atomic-commit-and-hot-journal -exact-step 0 -workers 4` — PASS (`ATOMIC COMMIT OK`, `50.5s`, zero data races)
- `go run -race ./ch04-pager-and-page-cache/experiments/4.2-cache-budget -n 8 -k 2` — zero data races (only the `k/n` RSS-ratio check failed under `-race` due to fixed race-detector overhead at small `n=8`)
- `go run ./ch04-pager-and-page-cache/experiments/4.2-cache-budget -n 4 -k 2 -rows 50` — PANIC (`integer divide by zero` in `main.cachedPages`, Finding 3)
- `sqlite3 :memory: "PRAGMA compile_options;" | grep -i memory` — confirmed CLI lacks `ENABLE_MEMORY_MANAGEMENT`
- Python + `sqlite3` / `strace` reproduction scripts in `/tmp/opencode` — confirmed Findings 1 (100-byte hot journal left 3-page DB untouched at 12288 bytes) and 2 (ATTACH commit in `/tmp/opencode/café-...` wrote signed-byte super-journal checksum `0x00000d98`, vs unsigned `0x00000f98`)
