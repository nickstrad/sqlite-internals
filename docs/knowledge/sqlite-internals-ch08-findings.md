---
title: "sqlite-internals chapter 8: open cost, idle cost and when a closed WAL database is at rest, as measured at 3.53.4"
summary: "Lifecycle facts for a platform builder: 3 descriptors per idle WAL connection opened at the first statement, process memory is about 4x the db_status counters, shrink_memory releases every connection's cache in the modernc build, a closed WAL database is not a single file at rest in five cases, default_cache_size travels in the file."
tags: [sqlite, connections, file-descriptors, memory, wal, sqlite-internals]
updated: 2026-11-21
verified: 2026-11-21 — Linux amd64, Go 1.26, SQLite 3.53.4 via modernc.org/sqlite v1.60.1; evidence in the sqlite-internals repo
---

# sqlite-internals chapter 8: open cost, idle cost and when a closed WAL database is at rest, as measured at 3.53.4

Lifecycle facts for a platform builder: 3 descriptors per idle WAL connection opened at the first statement, process memory is about 4x the db_status counters, shrink_memory releases every connection's cache in the modernc build, a closed WAL database is not a single file at rest in five cases, default_cache_size travels in the file.

Source: the `sqlite-internals` course repo (github.com/nickstrad/sqlite-internals), chunk `ch08`. Working notes: `agent_work/ch08/build-notes.md`; review and adversarial records in the same directory.

## Corrections to the outline


### Correction 1

1. **The reading path's `main.c openDatabase (3360–3736)` covers three functions.**
   `openDatabase` is 3360–3716; `sqlite3_open` is 3722–3728 and `sqlite3_open_v2` 3729–3736.
   SPANS.txt lists them separately. Every other outline span was exact (start line = symbol, end
   line = closing brace): `sqlite3BtreeOpen` 2532–2825, `sqlite3InitOne` 199–427, `sqlite3Init`
   438–464 / `sqlite3ReadSchema` 470–484 / `schemaIsValid` 492–533 (the outline's 438–533 is these
   three), `main.c` 1266–1486 (= `sqlite3Close` 1266–1322 … `sqlite3LeaveMutexAndCloseZombie`
   1375–1486), `status.c` 203–421 (`sqlite3_db_status64`), `os_unix.c` 2101–2109
   (`setPendingFd`), `wal.c` `sqlite3WalClose` 2507–2571.

### Correction 2

2. **"Some settings are stored in the file … the rest belong to the connection" has one exception
   the outline's table does not list: `cache_size` can come from the file.** The deprecated
   `PRAGMA default_cache_size=N` writes N into header bytes 48–51, and every new connection to that
   file then starts with `cache_size = N` (8.1 part B: `default_cache_size 777, cache_size 777,
   header bytes 48-51 = 777`). The driver is not built with `SQLITE_OMIT_DEPRECATED`. Mechanism:
   `sqlite3InitOne` at schema load (prepare.c 323–333), if the connection has not set cache_size,
   takes the absolute value of meta `BTREE_DEFAULT_CACHE_SIZE` (header 48–51) as the cache size
   (0 → `SQLITE_DEFAULT_CACHE_SIZE`). Note the absolute value: a negative (KiB) value stored there
   becomes a page count (read from the source, not measured). A platform
   that records per-connection settings must still set `cache_size` explicitly on every open,
   because a rebuilt or foreign file may carry a different starting value. Listed in
   `sqlitex.SettingPlacements()` as a file setting (`default_cache_size`, header bytes 48–51).

### Correction 3

3. **"Opening a database reads only the 100-byte header" — confirmed; "page-1 and schema-page reads
   at first statement" — confirmed and refined:** in WAL mode page 1 is read **twice** at the first
   statement. lockBtree reads page 1, sees header byte 19 = 2, opens the WAL and returns without
   keeping page 1 (btree.c 3333–3352: "the version of page 1 currently in the page1 buffer may not
   be the latest version - there may be a newer one in the log file"); `btreeBeginTrans` calls
   lockBtree again in a loop (btree.c 3711: `while( pBt->pPage1==0 && SQLITE_OK==(rc =
   lockBtree(pBt)) );`) and page 1 is read again, now through the WAL-aware pager. The `-wal` and
   `-shm` are opened between the two reads, at the first statement, not at open.

### Correction 4

4. **8.2 "the components sum to within a stated margin of the growth in process memory": the
   margin is large and one-sided.** With this driver, process memory grew by 3.8–4.0 × the sum of
   `CACHE_USED + SCHEMA_USED + STMT_USED` (n = 50, 200, 1000). The part the counters do not report
   was 164–174 KiB per connection (INFO lines). Known contributors: the per-connection lookaside
   buffer (default `1200,40`, global.c 218–224, "48KB of memory"; `LOOKASIDE_USED` reports slots,
   not bytes, and none of the three counters includes the buffer), driver and libc per-connection
   state, and the allocator overhead chunk 4 measured (correction 6 there: 4.3 KiB page
   allocations take 8 KiB slots). Not broken down further. The experiment states the margin as:
   growth ≥ sum of counters, and growth ≤ sum + 256 KiB per connection.
   **The 256 KiB margin is specific to 8.2's workload (a 3-table recipe schema, one prepared
   statement per connection) and is not a constant** (repair round): the adversarial attack
   IDL-03a opened 50 WAL connections each with a 1,000-table schema (SCHEMA_USED 251601 bytes
   per connection) and 5 prepared statements and measured 319–321 KiB per connection not reported
   by the counters (growth 36.9 MiB vs counter sum 21.3 MiB), so the uncounted part grows with the
   schema. A capacity plan must measure it for its own schema. It also has a fixed per-process
   part that dominates at small n (`-n 5`: 280 KiB per connection, fails; `-n 10`: 212; `-n 20`:
   186–194), so 8.2 now refuses `-n` below 20.

### Correction 5

5. **`PRAGMA shrink_memory` on one connection releases the page cache of every connection in the
   process with this driver.** 8.2 part C: after `shrink_memory` on connection 0 alone, all N
   connections' `CACHE_USED` fell from 52961 to 5089 bytes; running it on the other connections
   afterwards released nothing more. Mechanism: `shrink_memory` → `sqlite3_db_release_memory`
   (main.c 897–915) → `sqlite3PagerShrink` → `pcache1Shrink` (pcache1.c 839–851), which sets the
   **group's** `nMaxPage` to 0 and calls `pcache1EnforceMaxPage` (621–637); that loop recycles
   unpinned pages from the group's LRU list whichever cache owns them. The driver is built with
   `SQLITE_ENABLE_MEMORY_MANAGEMENT`, so every connection's cache is in one group (chunk 4
   correction 5). A C build without that option has one group per cache and would shrink only the
   calling connection (source only, not measured).

### Correction 6

6. **The outline's R5 condition "a WAL database whose last connection has closed is a single, fully
   checkpointed file" has exceptions; 8.3 part B shows five of them and the source shows two more**
   (see the WAL-default evaluation below).

### Correction 7

7. **`recipe.MatchStep` is too slow to call per file at fleet scale** (not an outline error, a
   finding for later chunks): every call re-verifies the step-hash cache by building a fresh step-0
   database (~100 ms). 8.3 calls `recipe.StepHashes` once and matches with `recipe.MatchHashes` +
   `recipe.ChangeCountersAgree` (the same logic as `MatchStep`). Matching 1,000 files went from
   ~110 s to under a second. The capstone (2,000 databases) should do the same.

Everything else in the chapter-8 section was confirmed: descriptor counts 1 / 2 / 3 (8.2 A), cache
releasable without closing while schema and statements stay (8.2 C), bounded descriptors and
memory under a K-connection manager, every reopen hash matching, every final file equal to the
recipe state (8.3 A).

