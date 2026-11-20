# Chapter 4 — Pager and page cache

| Pin field | Value |
| --- | --- |
| SQLite release | `3.53.4` (`2026-07-24 19:02:57 UTC`) |
| Git tag | `version-3.53.4` |
| Git commit (`github.com/sqlite/sqlite`) | `b09c88c14082339b66c7b7158d609a771e64ca69` |
| Fossil check-in (`sqlite_source_id()`) | `bf7c7f30031888f4e796e429ab3978879485813aaca6f641c7b33e4e09459bcc` |
| Go driver | `modernc.org/sqlite v1.60.1` (`modernc.org/libc v1.77.1`) |
| Copied C sources in this directory (`SOURCES.sha256`) | `pager.c`, `pcache.c`, `pcache1.c`, `status.c` |
| Linked C sources (in the pinned tree, not copied) | `btree.c`, `os_unix.c` |

> **How to run the experiments**
>
> - **Prerequisites:** Go 1.26 (`go.mod` specifies `go 1.26.0`).
> - **Fetch the pinned SQLite source tree** into `_src/sqlite-3.53.4` (or point `SQLITE_SRC` at an existing checkout of tag `version-3.53.4`):
>   ```text
>   go run ./cmd/course fetch-source
>   ```
> - **Build the pinned C tools** (`sqlite3`, `showwal`, `dbhash`) into `_tools/bin/` for Experiment 4.1's twin check with the C `sqlite3` CLI:
>   ```text
>   go run ./cmd/course build-tools
>   ```
> - **Run the experiments** from the repository root:
>   ```text
>   go run ./ch04-pager-and-page-cache/experiments/4.1-atomic-commit-and-hot-journal
>   go run ./ch04-pager-and-page-cache/experiments/4.2-cache-budget
>   ```
>   Scratch databases are written under `_work/4.1/` and `_work/4.2/`, which are git-ignored and safe to delete.
> - **Linux only notes and the Mac route:**
>   - **Experiment 4.1** uses Linux `strace` (including `--inject=...:signal=SIGKILL`) for Part A's traced children and Parts D, D2, E, and F (**Linux only**). On macOS, running the command directly executes Part A's portable header checks, Part B, Part C (decoding the live rollback journal with `internal/journalfile`), prints a `LINUX-ONLY:` notice, and commits the step in-process at `FULL`, `NORMAL`, and `OFF`. To run the full traced and crash-tested experiment on macOS, use the Linux `lab` container:
>     ```text
>     docker compose run --rm lab go run ./ch04-pager-and-page-cache/experiments/4.1-atomic-commit-and-hot-journal
>     ```
>   - **Experiment 4.2** is portable. On Linux it reads `/proc/self/status` for total resident memory (`VmRSS`) and its breakdown into anonymous (`RssAnon`), file-backed (`RssFile`), and shared (`RssShmem`) memory. On macOS it falls back to `ps -o rss= -p <pid>`, reporting total RSS only; the anonymous-versus-file-backed check in Part E is **Linux only** and prints the Docker command:
>     ```text
>     docker compose run --rm lab go run ./ch04-pager-and-page-cache/experiments/4.2-cache-budget
>     ```
>   - **Measured platform:** All captured evidence in this chapter was measured on Linux (`linux/amd64`, Go 1.26.8, strace 6.8, ext4) and verified inside the Debian `lab` container (whose `./configure`-built `sqlite3` CLI uses `fdatasync`, producing identical non-`INFO:` output). macOS (`darwin/arm64` and `darwin/amd64`) was cross-built and vetted only, not run.

---

## 1. Model

The **pager** sits between SQLite's B-tree layer and the **VFS** (Virtual File System, the thin layer that forwards file operations to the operating system). The B-tree thinks in fixed-size **pages** (4,096 bytes by default). The pager owns three jobs:

1. **Atomic commit and crash recovery.** A single SQL transaction modifies multiple pages. If the process dies halfway through writing them, the file on disk holds a mix of old and new pages (a **torn** database). In rollback-journal mode, the pager protects the database by writing the original **page images** into a sidecar **rollback journal** (`<name>-journal`) before modifying the database file, then removing or invalidating that journal as the single **commit point**.
2. **Page caching.** Reading a page from disk on every B-tree lookup is slow, and modifying pages in place before a transaction commits would corrupt the file on rollback. The pager holds recently used pages and modified (**dirty**) pages in an in-memory **page cache** (`PCache`, backed by the default allocator and LRU manager `PCache1`).
3. **File locking.** The pager acquires and releases locks on the database file to coordinate readers and writers across processes. Chapter 6 examines the lock byte ranges and concurrency rules; this chapter follows the locks only where they gate pager state transitions.

```text
  B-tree layer (btree.c)
       |  sqlite3PagerGet / sqlite3PagerWrite / CommitPhaseOne / CommitPhaseTwo
       v
+-------------------------------------------------------------------------+
| Pager (pager.c)                                                         |
|                                                                         |
|  State machine (Pager.eState):                                          |
|    OPEN ---> READER ---> WRITER_LOCKED ---> WRITER_CACHEMOD             |
|      ^         ^                                  |  syncJournal()      |
|      |         |  pager_end_transaction()         v                     |
|      |         +------ WRITER_FINISHED <--- WRITER_DBMOD                |
|      |                    (commit point)       (writes to .db)          |
|      +--- ERROR (on unrecoverable I/O failure)                          |
|                                                                         |
|  +-----------------------------+    +--------------------------------+  |
|  | Page cache (pcache.c)       |    | Rollback journal logic         |  |
|  |  - dirty list (sorted at    |    |  - pInJournal bitmap           |  |
|  |    commit)                  |    |  - writeJournalHdr / syncJournal| |
|  |  - pluggable backend        |    |  - pager_playback (hot journal)|  |
|  |    (pcache1.c: PGroup LRU)  |    |                                |  |
|  +--------------+--------------+    +---------------+----------------+  |
+-----------------|-----------------------------------|-------------------+
                  | readDbPage / pager_write_pagelist | write / sync / unlink
                  | (or getPageMMap -> sqlite3OsFetch)|
                  v                                   v
            +-----------+                       +------------------+
            |  t.db     |                       |  t.db-journal    |
            |  (pages)  |                       |  [header sector] |
            +-----------+                       |  [pgno|page|sum] |
                                                +------------------+
                  ^                                   ^
                  +------------ VFS (os_unix.c) ------+
                   pread64, pwrite64, fsync/fdatasync, unlink, fcntl, mmap
```

Key terms used throughout this chapter:

- **Rollback journal (`-journal`).** A companion file holding copies of pages as they existed *before* the current transaction began. Each journal segment starts on a **sector size** boundary (512 bytes on this Linux ext4 system) with a 28-byte header padded to one sector, followed by records of `[4-byte page number][page image][4-byte checksum]`.
- **Nonce.** A 32-bit random initializer written into each journal header (`cksumInit`, generated via `sqlite3_randomness`) and folded into every record's checksum so stale records left over from an older transaction cannot pass validation.
- **Hot journal versus cold journal.** A **hot journal** is a `-journal` left behind by a dead writer that SQLite must replay to restore the database on the next open. SQLite considers a journal hot when it exists, no process holds a `RESERVED` lock on the database, the database has more than 0 pages, and the journal's first byte is non-zero. A **cold journal** is a `-journal` whose size is 0 or whose first byte is `0x00`; SQLite ignores it.
- **`fsync` / `fdatasync`.** Operating-system calls that flush modified file data (and, for `fsync`, metadata) from the kernel buffer cache to durable storage. A **directory fsync** (`fsync` on `.`) flushes the directory entry so a newly created filename survives power loss.
- **Dirty page and spill.** A **dirty page** is a page modified in the page cache and not yet written to the database file. If a transaction dirties more pages than the cache can hold, the cache triggers a **spill** (`pagerStress`): it syncs the journal early and writes unreferenced dirty pages to the database file before `COMMIT`.
- **Memory-mapped I/O (`mmap`).** Setting `PRAGMA mmap_size` asks the operating system to map the database file into the process's address space. Reads then fetch page pointers straight from the kernel's page cache instead of copying pages into SQLite's heap cache.
- **RSS (`VmRSS`, `RssAnon`, `RssFile`, `RssShmem`).** Resident Set Size: the physical memory currently mapped into a process. On Linux, `/proc/self/status` splits `VmRSS` into **anonymous** heap memory (`RssAnon`), **file-backed** mapped pages (`RssFile`), and shared memory (`RssShmem`).

**WAL mode as the platform default.** Platform databases in this course run in WAL (Write-Ahead Logging) mode, covered in Chapter 5. Chapter 4 evaluates that default and records one planned exception plus two discoveries:

- **Planned exception (Experiment 4.1).** Chapter 4 is the rollback-journal chapter by subject, so Experiment 4.1 parts B–F run in rollback mode (`journal_mode=delete`, plus `truncate` and `persist` in part F). Even on a WAL-default platform, rollback mode still matters in two places that Part A measures directly: every newly created or imported database starts in `journal_mode=delete` until switched, and **the switch to WAL itself is a rollback-mode commit** (even `sqlitex.Open` on a brand-new WAL database creates, writes, syncs, and unlinks a `-journal` file).
- **WAL default kept (Experiment 4.2).** Every database in Experiment 4.2 runs in WAL mode (`sqlitex.DefaultOptions()`). The page cache operates identically in both modes, and WAL mode adds one measurable cost per open database file: a 32 KiB `-shm` memory mapping visible in file-backed RSS (2 MiB across 64 files).

**Relation to the Rebuild Ladder.** Chapter 4 adds no new rung to the Rebuild Ladder. Instead, it establishes two foundations that later rungs depend on: the rule that a rollback-mode database is its `.db` file *plus* its hot `-journal` (and why post-crash verification in rollback mode must compare at equality level E3 or canonical E1), and the per-connection memory model that sizes the bounded connection manager in Chapter 8 (rung R5) and the multi-database fleet in Chapter 10 (rung R7).

---

## 2. Question

What makes a multi-page rollback-journal commit all-or-nothing when the writer process dies at any write, sync, or unlink—and how much memory does each open database connection actually hold in this Go driver?

This chapter reuses four shared packages introduced in earlier chunks (linked rather than inlined):

- [`internal/sqlitex`](../internal/sqlitex/): opens SQLite with explicit file and connection settings, checks the pinned `sqlite_source_id()`, manages `_work/N.M/` directories, locates the pinned `sqlite3` CLI, and wraps `sqlite3_db_status` (`ReadDBStatus`).
- [`internal/trace`](../internal/trace/): runs child processes under Linux `strace`, parses syscalls, and injects `SIGKILL` on entry to a chosen syscall ordinal (`KillAt`).
- [`internal/recipe`](../internal/recipe/): builds the deterministic 30-step workload (`accounts`, `documents`, `events`, `events_by_account`), applies single steps, and matches file SHA-256 hashes against known states.
- [`internal/dbfile`](../internal/dbfile/): parses database files in plain Go and computes the page census (`Census().FreelistLeaves`), used here to identify pre-step freelist leaves.

Chapter 4 introduces one new shared package, `internal/journalfile`—a read-only rollback-journal inspector with no SQLite dependency—which is inlined in full and walked through in Section 5.1.

---

## 3. Call path

All source citations below are at tag `version-3.53.4`. Four files are copied whole into this chapter directory: [`pager.c`](pager.c), [`pcache.c`](pcache.c), [`pcache1.c`](pcache1.c), and [`status.c`](status.c) (verified by `SOURCES.sha256` against the pinned tree `$SQLITE_SRC` or `_src/sqlite-3.53.4`). `btree.c` (copied in Chapter 2) and `os_unix.c` (copied in Chapter 7) are linked in the pinned tree.

### 3.1 Pager state machine and locks

1. **State machine and constants** — [`src/pager.c:135-350`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L135-L350) (`eState` comment) and [`src/pager.c:351-357`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L351-L357) (`PAGER_OPEN`…`PAGER_ERROR`, local copy: [`pager.c`](pager.c)). Defines the seven pager states (`0` through `6`) and the invariants held in each:
   - `PAGER_OPEN` (`0`): file is open, no lock held, no transaction active.
   - `PAGER_READER` (`1`): `SHARED` lock held; pages may be read, none modified. Entered via `sqlite3PagerSharedLock`.
   - `PAGER_WRITER_LOCKED` (`2`): `RESERVED` lock held (in rollback mode) or WAL write lock held; journal not yet opened, no pages modified. Entered via `sqlite3PagerBegin`.
   - `PAGER_WRITER_CACHEMOD` (`3`): rollback journal is open and its first header is written; dirty pages exist in cache, but the database file itself has not been modified. Entered via `pager_open_journal`.
   - `PAGER_WRITER_DBMOD` (`4`): journal has been synced (`syncJournal`), and the pager is now permitted to write dirty pages to the database file.
   - `PAGER_WRITER_FINISHED` (`5`): all dirty pages have been written and synced to the database file (`sqlite3PagerCommitPhaseOne`), but the journal still exists.
   - `PAGER_ERROR` (`6`): an unrecoverable I/O error occurred.
2. **Lock-state tracking** — [`src/pager.c:360-406`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L360-L406) (`eLock` comment, local copy: [`pager.c`](pager.c)). `Pager.eLock` mirrors the lock held on the database file (`NO_LOCK`, `SHARED`, `RESERVED`, `EXCLUSIVE`, or `UNKNOWN_LOCK` after a failed unlock so the next reader rechecks for a hot journal).

### 3.2 Beginning a write transaction and building the journal

3. **`sqlite3PagerBegin`** — [`src/pager.c:5968-6032`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L5968-L6032) (local copy: [`pager.c`](pager.c)). Transitions `PAGER_READER -> PAGER_WRITER_LOCKED` (line 6018). In rollback mode it acquires a `RESERVED` lock (line 6002, or `EXCLUSIVE` immediately if requested); in WAL mode it acquires the WAL write lock (line 5995). It records `pPager->dbOrigSize`, the page count before the transaction begins.
4. **`pager_write` and `pager_open_journal`** — [`src/pager.c:6094-6173`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L6094-L6173) and [`src/pager.c:5877-5949`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L5877-L5949) (local copy: [`pager.c`](pager.c)). On the first page modification of a write transaction, `pager_write` calls `pager_open_journal` (line 6121), which allocates the `pInJournal` bitmap, opens `<db>-journal`, calls `writeJournalHdr` (line 5935), and sets `eState = PAGER_WRITER_CACHEMOD` (line 5945). `pager_write` then marks the page dirty (`sqlite3PcacheMakeDirty`, line 6128). If the page number is within `dbOrigSize` and not yet marked in `pInJournal` (lines 6135–6143), it calls `pagerAddPageToRollbackJournal`. Pages appended past `dbOrigSize` are **not** journaled (lines 6144–6147); they are only marked `PGHDR_NEED_SYNC` because rollback removes them by truncating the file back to `dbOrigSize`.
5. **`writeJournalHdr` and `journalHdrOffset`** — [`src/pager.c:1449-1571`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L1449-L1571) and [`src/pager.c:1366-1376`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L1366-L1376) (local copy: [`pager.c`](pager.c)). Every journal header begins on a sector boundary (`journalHdrOffset`). `writeJournalHdr` formats the 28-byte header (8-byte magic `aJournalMagic`, 4-byte record count, 4-byte random nonce `cksumInit` from `sqlite3_randomness`, 4-byte `dbOrigSize`, 4-byte `sectorSize`, 4-byte `pageSize`), pads it with zeros to a full sector, and writes it:
   ```c
   if( pPager->noSync || (pPager->journalMode==PAGER_JOURNALMODE_MEMORY)
    || (sqlite3OsDeviceCharacteristics(pPager->fd)&SQLITE_IOCAP_SAFE_APPEND)
   ){
     memcpy(zHeader, aJournalMagic, sizeof(aJournalMagic));
     put32bits(&zHeader[sizeof(aJournalMagic)], 0xffffffff);
   }else{
     memset(zHeader, 0, sizeof(aJournalMagic)+4);
   }
   ```
   That branch (lines 1495–1502) is why a rollback journal written under `synchronous=FULL` or `NORMAL` starts out **cold**: its first 12 bytes (magic and record count) are written as zeros until `syncJournal` runs. Under `synchronous=OFF` (`noSync`), SQLite writes the magic and `0xffffffff` ("infer the record count from file size") immediately, so the journal is hot as soon as the header hits the file.
6. **`pagerAddPageToRollbackJournal` and `pager_cksum`** — [`src/pager.c:6037-6085`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L6037-L6085) and [`src/pager.c:2251-2259`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L2251-L2259) (local copy: [`pager.c`](pager.c)). Appends one record to the journal in three separate `sqlite3OsWrite` calls: the 4-byte big-endian page number, the `pageSize`-byte original page image, and the 4-byte checksum (lines 6062–6067), advancing `journalOff` by `pageSize + 8` and setting the page's bit in `pInJournal`. `pager_cksum` starts from `cksumInit` and sums every 200th byte stepping down from `pageSize - 200` while `i > 0` (20 sampled bytes on a 4,096-byte page: offsets 3896, 3696, …, 96).

### 3.3 Two-phase commit: `syncJournal`, database writes, and the commit point

7. **`sqlite3PagerCommitPhaseOne`** — [`src/pager.c:6511-6730`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L6511-L6730) (local copy: [`pager.c`](pager.c)). In rollback mode, Phase One brings the database file up to the new state while keeping the journal intact: it updates the change counter on page 1 (journaling page 1 if not already journaled, lines 6620–6636), writes a super-journal name if multiple databases participate (line 6644), calls `syncJournal` (line 6658), writes every dirty page with `pager_write_pagelist` (line 6666/6696), truncates or extends the file if its size changed (line 6713), calls `sqlite3PagerSync` (line 6719), and sets `eState = PAGER_WRITER_FINISHED` (line 6727).
8. **`syncJournal`** — [`src/pager.c:4332-4441`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L4332-L4441) (local copy: [`pager.c`](pager.c)). Takes the `EXCLUSIVE` lock, probes 8 bytes at the next sector boundary and zeroes byte 0 there if a stale `PERSIST`-mode header follows (lines 4380–4385), then executes the exact sync sequence that distinguishes `FULL` from `NORMAL`:
   ```c
   if( pPager->fullSync && 0==(iDc&SQLITE_IOCAP_SEQUENTIAL) ){
     rc = sqlite3OsSync(pPager->jfd, pPager->syncFlags);
     if( rc!=SQLITE_OK ) return rc;
   }
   rc = sqlite3OsWrite(
       pPager->jfd, zHeader, sizeof(zHeader), pPager->journalHdr
   );
   if( rc!=SQLITE_OK ) return rc;
   ...
   rc = sqlite3OsSync(pPager->jfd, pPager->syncFlags|
     (pPager->syncFlags==SQLITE_SYNC_FULL?SQLITE_SYNC_DATAONLY:0)
   );
   ```
   In `FULL` mode (`fullSync` true, lines 4401–4420), SQLite syncs the journal *first* (flushing the records and, on the first sync of a new journal file, triggering the VFS directory sync in `unixSync`), writes the 12-byte `zHeader` (magic + `nRec`) at offset 0—making the journal hot—and then syncs the journal a second time. In `NORMAL` mode (`fullSync` false), SQLite skips the first sync, writes the 12-byte magic + `nRec` immediately, and performs one journal sync (plus the directory sync). In `OFF` mode (`noSync`), `syncJournal` issues no writes and no syncs. At line 4438 it sets `eState = PAGER_WRITER_DBMOD`.
9. **`pager_write_pagelist` and `sqlite3PagerSync`** — [`src/pager.c:4475-4556`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L4475-L4556) and [`src/pager.c:6446-6456`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L6446-L6456) (local copy: [`pager.c`](pager.c)). `sqlite3PcacheDirtyList` ([`src/pcache.c:818-824`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pcache.c#L818-L824), local copy: [`pcache.c`](pcache.c)) merge-sorts dirty pages by ascending page number, and `pager_write_pagelist` writes each page to `(pgno - 1) * pageSize`, counting each write in `aStat[PAGER_STAT_WRITE]`. `sqlite3PagerSync` sends `SQLITE_FCNTL_SYNC` and syncs the database file unless `noSync`.
10. **`sqlite3PagerCommitPhaseTwo`, `pager_end_transaction`, and `zeroJournalHdr`** — [`src/pager.c:6748-6786`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L6748-L6786), [`src/pager.c:2052-2180`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L2052-L2180), and [`src/pager.c:1399-1432`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L1399-L1432) (local copy: [`pager.c`](pager.c)). Phase Two calls `pager_end_transaction`, which executes the **commit point** according to `journal_mode` (lines 2086–2122):
    - `DELETE`: closes and deletes `-journal` (`sqlite3OsDelete`, line 2120).
    - `TRUNCATE`: truncates `-journal` to 0 bytes (`sqlite3OsTruncate`, line 2090), followed by `sqlite3OsSync` on the journal when `fullSync` is set (lines 2091–2098).
    - `PERSIST`: calls `zeroJournalHdr` (line 2104), overwriting the first 28 bytes of `-journal` with zeros and syncing the journal unless `noSync`.
    After finalizing the journal, `pager_end_transaction` destroys `pInJournal`, marks cached pages clean, drops the lock back to `SHARED`, and sets `eState = PAGER_READER`.

### 3.4 Hot-journal recovery and the `PAGER_GET_NOCONTENT` exception

11. **`sqlite3PagerSharedLock`, `hasHotJournal`, and `pagerSyncHotJournal`** — [`src/pager.c:5300-5507`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L5300-L5507), [`src/pager.c:5180-5271`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L5180-L5271), and [`src/pager.c:4094-4103`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L4094-L4103) (local copy: [`pager.c`](pager.c)). On the first read or write (`OPEN -> READER`), `sqlite3PagerSharedLock` takes a `SHARED` lock and calls `hasHotJournal`. `hasHotJournal` checks four conditions in order: (a) `<db>-journal` exists (`sqlite3OsAccess`, line 5196), (b) no process holds `RESERVED` (`sqlite3OsCheckReservedLock`, line 5209), (c) the database has more than 0 pages (if `nPage == 0`, it deletes the stale journal instead, lines 5224–5230), and (d) the first byte of `-journal` is non-zero (`sqlite3OsRead` of 1 byte at offset 0, lines 5243–5251). When `hasHotJournal` reports true, `sqlite3PagerSharedLock` jumps straight to an `EXCLUSIVE` lock (line 5354, bypassing `RESERVED` so other readers cannot see the torn database), syncs the hot journal (`pagerSyncHotJournal`, line 5400), and runs `pager_playback` (line 5402).
12. **`pager_playback`, `readJournalHdr`, and `pager_playback_one_page`** — [`src/pager.c:2864-3052`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L2864-L3052), [`src/pager.c:1590-1693`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L1590-L1693), and [`src/pager.c:2298-2500`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L2298-L2500) (local copy: [`pager.c`](pager.c)). `pager_playback` checks any super-journal footer (if named but missing, the multi-database commit already succeeded and rollback stops), loops over headers via `readJournalHdr` (stopping on bad magic or non-power-of-two geometry), truncates the database back to the first header's `dbOrigSize` (`pager_truncate`, line 2952), and calls `pager_playback_one_page` for each record. `pager_playback_one_page` stops on page `0` or `PAGER_SJ_PGNO`, skips pages larger than `dbOrigSize`, stops on a checksum mismatch, and otherwise writes the original page image back to the database file. Finally, `pager_playback` syncs the restored database and calls `pager_end_transaction` (which deletes the journal in `DELETE` mode).
13. **`getPageNormal` and `PAGER_GET_NOCONTENT`** — [`src/pager.c:5581-5683`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L5581-L5683) (local copy: [`pager.c`](pager.c)) and `allocateBtreePage` at [`src/btree.c:6740`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/btree.c#L6740) (in the pinned tree and Chapter 2's copy). When the B-tree pops a leaf page off the freelist, its old content is unused, so `allocateBtreePage` passes `PAGER_GET_NOCONTENT` to the pager. Inside `getPageNormal` (lines 5645–5661), SQLite zeroes the in-memory page instead of reading disk *and* sets the page's bit in `pInJournal`:
    ```c
    if( noContent ){
      sqlite3BeginBenignMalloc();
      if( pgno<=pPager->dbOrigSize ){
        TESTONLY( rc = ) sqlite3BitvecSet(pPager->pInJournal, pgno);
      }
    ...
    }
    ```
    When `pager_write` later marks that page dirty, `pInJournal` already has its bit set, so **no journal record is written for a reused freelist leaf**. If a crash happens after the database write reaches that page, hot-journal rollback restores the freelist trunk page (putting the page back on the freelist) but leaves the new bytes sitting inside that unused freelist leaf. Experiment 4.1 demonstrates this directly.

### 3.5 Page cache, spills, unified LRU, mmap, and `sqlite3_db_status`

14. **`PCache`, `numberOfCachePages`, dirty list, and fetch/stress** — [`src/pcache.c:41-54`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pcache.c#L41-L54) (`struct PCache`), [`src/pcache.c:195-259`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pcache.c#L195-L259) (`pcacheManageDirtyList`), [`src/pcache.c:277-292`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pcache.c#L277-L292) (`numberOfCachePages`), [`src/pcache.c:403-432`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pcache.c#L403-L432) (`sqlite3PcacheFetch`), [`src/pcache.c:445-490`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pcache.c#L445-L490) (`sqlite3PcacheFetchStress`), [`src/pcache.c:593-607`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pcache.c#L593-L607) (`sqlite3PcacheMakeDirty`), and [`src/pager.c:4655-4727`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L4655-L4727) (`pagerStress`, local copies: [`pcache.c`](pcache.c), [`pager.c`](pager.c)). `numberOfCachePages` translates `PRAGMA cache_size`: a positive `N` means `N` pages; a negative `N` means `(-1024 * N) / (szPage + szExtra)` pages (KiB budget). When a cache fills with dirty pages above `szSpill`, `sqlite3PcacheFetchStress` invokes `pagerStress`, which increments `PAGER_STAT_SPILL`, calls `syncJournal(pPager, 1)` (syncing the journal and starting a new header segment for subsequent writes), and writes an unreferenced dirty page out to the database file early.
15. **`pcache1` structures, unified `PGroup`, and the `N - 1` rule** — [`src/pcache1.c:117-127`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pcache1.c#L117-L127) (`struct PgHdr1`), [`src/pcache1.c:158-165`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pcache1.c#L158-L165) (`struct PGroup`), [`src/pcache1.c:175-203`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pcache1.c#L175-L203) (`struct PCache1`), [`src/pcache1.c:209-211`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pcache1.c#L209-L211) (`struct PgFreeslot`), [`src/pcache1.c:520-526`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pcache1.c#L520-L526) (`pcache1UnderMemoryPressure`), [`src/pcache1.c:621-637`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pcache1.c#L621-L637) (`pcache1EnforceMaxPage`), and [`src/pcache1.c:874-941`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pcache1.c#L874-L941) (`pcache1FetchStage2`, local copy: [`pcache1.c`](pcache1.c)). Two facts in `pcache1.c` explain the numbers Experiment 4.2 measures:
    - **Unified LRU under `ENABLE_MEMORY_MANAGEMENT` (`pcache1Init`, `pcache1.c:717-724`; `pcache1Cachesize`, `pcache1.c:814-832`; `pcache1FetchStage2`, `pcache1.c:899-916`).** When compiled with `SQLITE_ENABLE_MEMORY_MANAGEMENT`—which `modernc.org/sqlite v1.60.1` enables—`pcache1.separateCache` is `0`, so every open connection in the process shares one global `PGroup` and one LRU list. Each connection's `cache_size` adds its share `nMax` to `pGroup->nMaxPage`. When a cache needs a page and reaches its own limit, `pcache1FetchStage2` recycles `pGroup->lru.pLruPrev`, **whichever connection owns it**:
      ```c
      if( pCache->bPurgeable
       && !pGroup->lru.pLruPrev->isAnchor
       && ((pCache->nPage+1>=pCache->nMax) || pcache1UnderMemoryPressure(pCache))
      ){
        PCache1 *pOther;
        pPage = pGroup->lru.pLruPrev;
        pcache1RemoveFromHash(pPage, 0);
        pcache1PinPage(pPage);
        pOther = pPage->pCache;
      ```
      An active connection can therefore hold far more pages than its own `cache_size` by taking pages from idle connections, while the process-wide sum stays fixed at the sum of all open connections' budgets. Without `SQLITE_ENABLE_MEMORY_MANAGEMENT` (such as the C `sqlite3` CLI here), `separateCache` is `1`, giving each connection its own private `PGroup` and enforcing `cache_size` per connection.
    - **A budget of `N` pages holds `N - 1`.** The recycle condition at line 902 is `pCache->nPage + 1 >= pCache->nMax`. Once `nPage` reaches `nMax - 1`, every subsequent miss recycles an existing page instead of allocating a new one. Thus `cache_size=100` holds `99` pages, `cache_size=200` holds `199`, and `cache_size=-2000` (`nMax = 2048000 / (4096 + szExtra) = 483`) holds `482` pages.
16. **Memory-mapped reads (`mmap`)** — [`src/pager.c:3579-3590`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L3579-L3590) (`pagerFixMaplimit`) and [`src/pager.c:5687-5752`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L5687-L5752) (`getPageMMap`, local copy: [`pager.c`](pager.c)). Setting `PRAGMA mmap_size > 0` switches the pager's fetch method to `getPageMMap` and passes the limit to the VFS (`SQLITE_FCNTL_MMAP_SIZE`). On Unix (`unixFetch` -> `unixMapfile` in `src/os_unix.c`, linked in the pinned tree), `sqlite3OsFetch` returns a pointer directly into the `mmap` region and `pagerAcquireMapPage` wraps it in a lightweight page handle outside `pcache1`.
17. **Per-connection counters** — [`src/status.c:203-421`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/status.c#L203-L421) (`sqlite3_db_status64`, local copy: [`status.c`](status.c)), [`src/pager.c:6881-6887`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L6881-L6887) (`sqlite3PagerMemUsed`), and [`src/pager.c:6928-6946`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L6928-L6946) (`sqlite3PagerCacheStat`). `SQLITE_DBSTATUS_CACHE_USED` sums `sqlite3PagerMemUsed` across attached databases:
    ```c
    int sqlite3PagerMemUsed(Pager *pPager){
      int perPageSize = pPager->pageSize + pPager->nExtra
        + (int)(sizeof(PgHdr) + 5*sizeof(void*));
      return perPageSize*sqlite3PcachePagecount(pPager->pPCache)
               + sqlite3MallocSize(pPager)
               + pPager->pageSize;
    }
    ```
    Because `pPager` stores the database, journal, and WAL filenames inside its single heap allocation, `sqlite3MallocSize(pPager)`—and therefore raw `CACHE_USED` bytes—varies slightly with the database's path length. `SQLITE_DBSTATUS_CACHE_HIT`, `CACHE_MISS`, `CACHE_WRITE`, and `CACHE_SPILL` read `aStat[]` through `sqlite3PagerCacheStat`.

### Version notes

Behavior differences across SQLite releases relevant to the pager and page cache (sources: [`docs/knowledge/source-inventory-ch01-04.md`](../docs/knowledge/source-inventory-ch01-04.md), `changes.html`, `pgszchng2016.html`, and the copied source):

- **Default `cache_size = -2000` (3.12.0).** Release 3.12.0 changed the default page size from `1024` to `4096` and the default `cache_size` from `2000` pages to `-2000` (2,000 KiB = 2,048,000 bytes; `numberOfCachePages`, `src/pcache.c:277-292`).
- **`sqlite3_db_status` counters added over time (`changes.html`).** `CACHE_HIT` and `CACHE_MISS` were added in 3.7.9, `CACHE_WRITE` in 3.7.12, `CACHE_SPILL` in 3.23.0, and `TEMPBUF_SPILL` in 3.51.0 (present at `src/status.c:390`).
- **Hard heap limit (3.31.0).** `sqlite3_hard_heap_limit64` and `PRAGMA hard_heap_limit` were added in 3.31.0 (`changes.html`).
- **Journal header page-size field (pre-3.5.8).** Versions prior to 3.5.8 left the journal header's page-size field unset; `readJournalHdr` (`src/pager.c:1650`) still accommodates that legacy format.

---

### Experiment 4.1 — Atomic commit and hot journal (`4.1-atomic-commit-and-hot-journal`)

- **Claim:**
  1. New databases start in rollback-journal mode (`journal_mode=delete`, header bytes 18–19 = `1,1`), and switching a new database to WAL—including inside the platform opener `sqlitex.Open`—commits page 1 through a rollback `-journal`.
  2. In rollback mode, SQLite writes original page images to `-journal` and syncs the journal before any write reaches the database file: 4 syncs per commit at `synchronous=FULL` (`journal-sync`, `dir-sync`, 12-byte `journal-count`, `journal-sync`, database writes, `db-sync`, `journal-unlink`), 3 syncs at `NORMAL` (12-byte `journal-count` before the single `journal-sync` and `dir-sync`), and 0 syncs at `OFF`. The pinned C `sqlite3` CLI produces the exact same writes, syncs, and unlink in the same order.
  3. Killing the writer on entry to every write, sync, and unlink across `FULL`, `NORMAL`, and `OFF` always leaves a database that SQLite restores to the pre-step or post-step state—never a third state—with the flip occurring on the journal finalization syscall (`unlink` in `DELETE`, `ftruncate` in `TRUNCATE`, 28-byte header zero in `PERSIST`). **Outline correction:** at the byte level, a hot-journal rollback restores exact pre-step bytes whenever the pre-step file has no freelist (Part D2, step 1), but leaves new bytes inside reused freelist leaves when the transaction pops pages off the freelist via `PAGER_GET_NOCONTENT` (Part D, step 3, page 22); in every such case, `integrity_check` is `ok` and the `VACUUM INTO` canonical hash equals pre-step's.
  4. Deleting a hot journal or copying a crashed database without its hot journal either yields an unreadable malformed database or silently commits a transaction whose `COMMIT` never returned (Part E).
- **Why kept:** A WAL-mode platform still meets rollback mode. Every new database starts in it, the switch to WAL itself commits through a `-journal`, and a file moved without its `-journal` is a corrupt file. One full-depth experiment covers all of that; a Go journal replayer is not built because the platform never replays rollback journals itself.
- **Evidence form:** `EXACT` (every non-`INFO:` line matches byte for byte on repeated runs and inside the Debian `lab` container; `INFO:` lines carry work directory, sync syscall name, random per-run journal nonces, and runtimes).
- **Linux only (parts A traced children, D, D2, E, F):** On macOS run through Docker:
  ```text
  docker compose run --rm lab go run ./ch04-pager-and-page-cache/experiments/4.1-atomic-commit-and-hot-journal
  ```
- **Run command (Linux):**
  ```text
  go run ./ch04-pager-and-page-cache/experiments/4.1-atomic-commit-and-hot-journal
  ```
  Optional flags: `-step 3` (traced and killed recipe step), `-exact-step 1` (freelist-free step; `0` skips Part D2), `-workers 4` (parallel kill workers).

<!-- include: experiments/4.1-atomic-commit-and-hot-journal/main.go -->
```go
// Experiment 4.1 traces a multi-page rollback-journal commit through the
// kernel, decodes the live journal with internal/journalfile, kills the writer
// on entry to every write, sync, and unlink across synchronous=FULL/NORMAL/OFF,
// cross-checks the syscall stream against the pinned C CLI, demonstrates what
// happens when a hot journal is lost, and locates the exact commit point in
// DELETE, TRUNCATE, and PERSIST modes.
//
// Run from the repo root (Linux for strace parts; macOS runs parts A–C and an
// in-process fallback, or use docker compose run --rm lab):
//
//	go run ./ch04-pager-and-page-cache/experiments/4.1-atomic-commit-and-hot-journal
package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"github.com/nickstrad/sqlite-internals/internal/dbfile"
	"github.com/nickstrad/sqlite-internals/internal/journalfile"
	"github.com/nickstrad/sqlite-internals/internal/recipe"
	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
	"github.com/nickstrad/sqlite-internals/internal/trace"
)

const packagePath = "ch04-pager-and-page-cache/experiments/4.1-atomic-commit-and-hot-journal"

const (
	dbName      = "t.db"
	pageSize    = 4096
	markerBegin = "marker-begin"
	markerEnd   = "marker-end"
)

var (
	failures  []string
	stepFlag  = flag.Int("step", 3, "recipe step whose commit is traced and killed (the pre-step state is the step before it)")
	exactFlag = flag.Int("exact-step", 1, "second recipe step, killed at every point too, whose pre-step state has no freelist pages (0 skips it)")
	workers   = flag.Int("workers", 4, "kill runs executed in parallel")
)

// init pins the main goroutine to one OS thread so strace's per-thread syscall
// counter observes the child writer's SQLite syscalls in a deterministic order.
func init() {
	runtime.LockOSThread()
}

func check(ok bool, claim string) {
	status := "ok"
	if !ok {
		status = "FAILED"
		failures = append(failures, claim)
	}
	fmt.Printf("check %s: %s\n", status, claim)
}

func fatal(what string, err error) {
	fmt.Printf("FATAL %s: %v\n", what, err)
	os.Exit(1)
}

func rollbackRecipe() recipe.Recipe {
	return recipe.Default().WithJournalMode(sqlitex.JournalDelete)
}

func stepOptions(journalMode, synchronous string) sqlitex.Options {
	options := rollbackRecipe().SQLiteOptions()
	options.File.JournalMode = journalMode
	options.Conn.Synchronous = synchronous
	return options
}

// touch opens and reads an empty marker file so strace records an unambiguous
// open/openat boundary around the traced transaction without touching SQLite.
func touch(dir, name string) {
	if _, err := os.ReadFile(filepath.Join(dir, name)); err != nil {
		fatal("marker", err)
	}
}

func childStep(dir, journalMode, synchronous string, k int) {
	ctx := context.Background()
	step, err := rollbackRecipe().Step(k)
	if err != nil {
		fatal("child step", err)
	}
	db, err := sqlitex.Open(ctx, filepath.Join(dir, dbName), stepOptions(journalMode, synchronous))
	if err != nil {
		fatal("child open", err)
	}
	touch(dir, markerBegin)
	if err := recipe.ApplyStep(ctx, db, step); err != nil {
		fatal("child apply", err)
	}
	touch(dir, markerEnd)
	if err := db.Close(); err != nil {
		fatal("child close", err)
	}
}

func copyFile(from, to string) {
	data, err := os.ReadFile(from)
	if err != nil {
		fatal("copy", err)
	}
	if err := os.WriteFile(to, data, 0o644); err != nil {
		fatal("copy", err)
	}
}

func mkdir(dir string) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("mkdir", err)
	}
}

func prepareDir(dir, pre string) {
	mkdir(dir)
	if err := recipe.RemoveDatabase(filepath.Join(dir, dbName)); err != nil {
		fatal("remove", err)
	}
	copyFile(pre, filepath.Join(dir, dbName))
	for _, name := range []string{markerBegin, markerEnd} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
			fatal("marker", err)
		}
	}
}

type states struct {
	step, preStep  int
	prePath        string
	pre, post      []byte
	prePages       int
	postPages      int
	hashes         []recipe.FileHashes
	preFreeLeaves  []uint32
	preCanon       string
	postCanon      string
	stepStatements recipe.Step
}

func newStates(ctx context.Context, dir string, r recipe.Recipe, k int, hashes []recipe.FileHashes) *states {
	mkdir(dir)
	s := &states{step: k, preStep: k - 1, hashes: hashes, prePath: filepath.Join(dir, "pre.db")}
	var err error
	s.stepStatements, err = r.Step(k)
	if err != nil {
		fatal("step", err)
	}
	if err := r.Build(ctx, s.prePath, k-1); err != nil {
		fatal("build pre-step", err)
	}
	postPath := filepath.Join(dir, "post.db")
	if err := r.Build(ctx, postPath, k); err != nil {
		fatal("build post-step", err)
	}
	if s.pre, err = os.ReadFile(s.prePath); err != nil {
		fatal("read pre", err)
	}
	if s.post, err = os.ReadFile(postPath); err != nil {
		fatal("read post", err)
	}
	s.prePages, s.postPages = len(s.pre)/pageSize, len(s.post)/pageSize
	// Collect pre-step freelist leaves: when the B-tree reuses a freelist page,
	// it fetches with PAGER_GET_NOCONTENT (src/btree.c:6740), which marks the
	// page in pInJournal without writing a journal record (src/pager.c:5645-5656).
	// A hot-journal rollback therefore cannot restore those unused bytes.
	file, err := dbfile.FromBytes(s.pre)
	if err != nil {
		fatal("parse pre", err)
	}
	census, err := file.Census()
	if err != nil {
		fatal("census pre", err)
	}
	for _, leaf := range census.FreelistLeaves {
		s.preFreeLeaves = append(s.preFreeLeaves, uint32(leaf))
	}
	slices.Sort(s.preFreeLeaves)
	// Canonical hashes (VACUUM INTO) provide rung R2 logical-to-byte equality
	// when a rolled-back file differs only inside reused freelist leaves.
	s.preCanon = canonicalHashOfFile(ctx, s.prePath)
	s.postCanon = canonicalHashOfFile(ctx, postPath)
	return s
}

func (s *states) classifyImage(image []byte) string {
	switch {
	case bytes.Equal(image, s.pre):
		return "pre-step bytes"
	case bytes.Equal(image, s.post):
		return "post-step bytes"
	}
	return fmt.Sprintf("torn: %d pages, %d differ from pre-step, %d differ from post-step", len(image)/pageSize, len(differingPages(image, s.pre)), len(differingPages(image, s.post)))
}

func (s *states) matchFile(path string) recipe.Match {
	h, err := recipe.HashFile(path)
	if err != nil {
		fatal("hash", err)
	}
	return recipe.MatchHashes(s.hashes, h)
}

type outcomeKind int

const (
	outcomePre outcomeKind = iota
	outcomePreFreelist
	outcomePost
	outcomeThird
	outcomeUnreadable
)

type outcome struct {
	kind     outcomeKind
	pages    []uint32
	match    recipe.Match
	canonPre bool
}

func (s *states) outcomeOf(path, canon string) outcome {
	image, err := os.ReadFile(path)
	if err != nil {
		fatal("read reopened", err)
	}
	o := outcome{match: s.matchFile(path), canonPre: canon != "" && canon == s.preCanon}
	switch {
	case canon == "":
		o.kind = outcomeUnreadable
	case bytes.Equal(image, s.pre):
		o.kind = outcomePre
	case bytes.Equal(image, s.post):
		o.kind = outcomePost
	default:
		o.pages = differingPages(image, s.pre)
		o.kind = outcomeThird
		// A rolled-back database whose file size and VACUUM INTO canonical hash
		// equal pre-step, and whose only differing pages are pre-step freelist
		// leaves, is logically identical to pre-step (PAGER_GET_NOCONTENT effect).
		if len(image) == len(s.pre) && o.canonPre && subset(o.pages, s.preFreeLeaves) {
			o.kind = outcomePreFreelist
		}
	}
	return o
}

func (o outcome) before() bool {
	return o.kind == outcomePre || o.kind == outcomePreFreelist
}

func (o outcome) letter() byte {
	switch o.kind {
	case outcomePre:
		return 'B'
	case outcomePreFreelist:
		return 'b'
	case outcomePost:
		return 'A'
	}
	return '?'
}

func (o outcome) describe(s *states) string {
	switch o.kind {
	case outcomePre:
		return fmt.Sprintf("pre-step, exact bytes (recipe step %d)", s.preStep)
	case outcomePreFreelist:
		return fmt.Sprintf("pre-step except page(s) %s, which are freelist leaves in the pre-step file (unused bytes); VACUUM INTO output equals the pre-step's", pageList(o.pages))
	case outcomePost:
		return fmt.Sprintf("post-step, exact bytes (recipe step %d)", s.step)
	case outcomeUnreadable:
		return "unreadable"
	}
	if o.match.Found() {
		return "a third state: recipe " + o.match.String()
	}
	return fmt.Sprintf("a third state: differs from pre-step in page(s) %s, matches no recipe state", pageList(o.pages))
}

func subset(pages, set []uint32) bool {
	for _, p := range pages {
		if !slices.Contains(set, p) {
			return false
		}
	}
	return true
}

func pageList(pages []uint32) string {
	parts := make([]string, len(pages))
	for i, p := range pages {
		parts[i] = strconv.Itoa(int(p))
	}
	return "[" + strings.Join(parts, " ") + "]"
}

func pageAt(image []byte, offset int) []byte {
	if offset >= len(image) {
		return nil
	}
	return image[offset:min(offset+pageSize, len(image))]
}

func differingPages(a, b []byte) []uint32 {
	var pages []uint32
	for i := 0; i < max(len(a), len(b)); i += pageSize {
		if !bytes.Equal(pageAt(a, i), pageAt(b, i)) {
			pages = append(pages, uint32(i/pageSize+1))
		}
	}
	return pages
}

func canonicalHashOfFile(ctx context.Context, path string) string {
	db, err := sqlitex.Open(ctx, path, stepOptions(sqlitex.JournalDelete, sqlitex.SyncFull))
	if err != nil {
		fatal("open for VACUUM INTO", err)
	}
	defer db.Close()
	hash, err := canonicalHash(ctx, db.DB, path)
	if err != nil {
		fatal("VACUUM INTO", err)
	}
	return hash
}

func canonicalHash(ctx context.Context, db *sql.DB, path string) (string, error) {
	target := path + ".canonical"
	if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if _, err := db.ExecContext(ctx, "VACUUM INTO ?", target); err != nil {
		return "", err
	}
	defer os.Remove(target)
	return recipe.FileSHA256(target)
}

type op struct {
	call    trace.Syscall
	kind    string
	text    string
	ordinal int
}

func syncName(name string) bool {
	return name == "fsync" || name == "fdatasync"
}

func openName(name string) bool {
	return name == "open" || name == "openat"
}

func unlinkName(name string) bool {
	return name == "unlink" || name == "unlinkat"
}

func killable(name string) bool {
	return name == "pwrite64" || name == "ftruncate" || syncName(name) || unlinkName(name)
}

func describe(call trace.Syscall) string {
	switch {
	case call.Name == "pwrite64" || call.Name == "pread64":
		text := fmt.Sprintf("%s %s off=%d len=%d", call.Name, call.Path, call.Offset, call.Length)
		if call.Result >= 0 && call.Result != call.Length && !call.Unfinished {
			text += fmt.Sprintf(" = %d", call.Result)
		}
		return text
	case syncName(call.Name):
		return "sync " + call.Path
	case unlinkName(call.Name):
		return "unlink " + call.Path
	case call.Name == "ftruncate":
		return fmt.Sprintf("ftruncate %s len=%d", call.Path, call.Length)
	case openName(call.Name):
		return "open " + call.Path
	case call.Name == "fcntl" && call.Lock != nil:
		return fmt.Sprintf("fcntl %s %s %s start=%d len=%d", call.Path, call.Lock.Command, call.Lock.Type, call.Lock.Start, call.Lock.Length)
	case call.Name == "fcntl":
		return fmt.Sprintf("fcntl %s %s", call.Path, strings.SplitN(call.Flags, ",", 2)[0])
	}
	return call.Name + " " + call.Path
}

// classify maps each traced syscall in the commit window to its pager role:
// 512-byte header write (writeJournalHdr, src/pager.c:1449-1571), 12-byte
// magic+count update (syncJournal, src/pager.c:4377-4410), 28-byte header zero
// (zeroJournalHdr, src/pager.c:1399-1432), record writes, db page writes,
// syncs (including the directory sync on a newly created journal), and journal
// finalization.
func classify(call trace.Syscall) string {
	journal := dbName + "-journal"
	switch {
	case call.Name == "pwrite64" && call.Path == journal && call.Offset == 0 && call.Length == 12:
		return "journal-count"
	case call.Name == "pwrite64" && call.Path == journal && call.Offset == 0 && call.Length == 28:
		return "journal-zero-header"
	case call.Name == "pwrite64" && call.Path == journal && call.Offset == 0:
		return "journal-header"
	case call.Name == "pwrite64" && call.Path == journal:
		return "journal-record"
	case call.Name == "pwrite64" && call.Path == dbName:
		return "db-page"
	case syncName(call.Name) && call.Path == journal:
		return "journal-sync"
	case syncName(call.Name) && call.Path == ".":
		return "dir-sync"
	case syncName(call.Name) && call.Path == dbName:
		return "db-sync"
	case unlinkName(call.Name) && call.Path == journal:
		return "journal-unlink"
	case call.Name == "ftruncate" && call.Path == journal:
		return "journal-truncate"
	case call.Name == "ftruncate" && call.Path == dbName:
		return "db-truncate"
	case openName(call.Name) && call.Path == markerEnd:
		return "step-returned"
	}
	return "other"
}

type window struct {
	tid     int
	ops     []op
	targets []op
}

// extractWindow isolates the writer thread's syscalls between marker-begin and
// marker-end while counting each syscall name's 1-based ordinal from process
// start, which strace's inject=...:when=ordinal flag requires.
func extractWindow(log *trace.StraceLog, dir string) (window, error) {
	w := window{tid: -1}
	beginPath := filepath.Join(dir, markerBegin)
	endPath := filepath.Join(dir, markerEnd)
	for _, call := range log.Calls {
		if openName(call.Name) && call.Path == beginPath {
			w.tid = call.PID
			break
		}
	}
	if w.tid < 0 {
		return w, errors.New("begin marker not found in the trace")
	}
	ordinals := map[string]int{}
	inside, ended := false, false
	for _, call := range log.Calls {
		if call.PID != w.tid || ended {
			continue
		}
		ordinals[call.Name]++
		if openName(call.Name) && call.Path == beginPath && !inside {
			inside = true
			continue
		}
		if !inside {
			continue
		}
		if openName(call.Name) && call.Path == endPath {
			relative := trace.RelativeTo(dir, []trace.Syscall{call})[0]
			w.targets = append(w.targets, op{call: relative, kind: classify(relative), text: describe(relative), ordinal: ordinals[call.Name]})
			ended = true
			continue
		}
		if !trace.UnderDir(dir)(call) || strings.HasPrefix(filepath.Base(call.Path), "marker-") {
			continue
		}
		relative := trace.RelativeTo(dir, []trace.Syscall{call})[0]
		o := op{call: relative, kind: classify(relative), text: describe(relative), ordinal: ordinals[call.Name]}
		w.ops = append(w.ops, o)
		if killable(call.Name) {
			w.targets = append(w.targets, o)
		}
	}
	if !ended {
		return w, errors.New("end marker not found in the trace")
	}
	return w, nil
}

func collapse(ops []op) string {
	var parts []string
	last, count := "", 0
	flush := func() {
		if last == "" {
			return
		}
		if count > 1 {
			parts = append(parts, fmt.Sprintf("%s x%d", last, count))
		} else {
			parts = append(parts, last)
		}
	}
	for _, o := range ops {
		if o.kind == "other" || o.kind == "step-returned" {
			continue
		}
		if o.kind == last {
			count++
			continue
		}
		flush()
		last, count = o.kind, 1
	}
	flush()
	return strings.Join(parts, ", ")
}

func countKind(ops []op, kind string) int {
	n := 0
	for _, o := range ops {
		if o.kind == kind {
			n++
		}
	}
	return n
}

// expectedSequence builds the canonical commit syscall sequence for a given
// journal mode and synchronous level: FULL syncs the journal before and after
// writing magic+count (syncJournal, src/pager.c:4401-4420), NORMAL writes
// magic+count before its single journal sync, and OFF emits no syncs; TRUNCATE
// and PERSIST add a final journal sync at FULL (pager_end_transaction,
// src/pager.c:2086-2106).
func expectedSequence(journalMode, synchronous string, records, dbPages int) string {
	r := fmt.Sprintf("journal-record x%d", records)
	d := fmt.Sprintf("db-page x%d", dbPages)
	finish := map[string]string{
		sqlitex.JournalDelete:   "journal-unlink",
		sqlitex.JournalTruncate: "journal-truncate, journal-sync",
		sqlitex.JournalPersist:  "journal-zero-header, journal-sync",
	}[journalMode]
	switch synchronous {
	case sqlitex.SyncFull:
		return "journal-header, " + r + ", journal-sync, dir-sync, journal-count, journal-sync, " + d + ", db-sync, " + finish
	case sqlitex.SyncNormal:
		return "journal-header, " + r + ", journal-count, journal-sync, dir-sync, " + d + ", db-sync, " + finish
	}
	return "journal-header, " + r + ", " + d + ", " + finish
}

func syncTargets(ops []op) []string {
	var out []string
	for _, o := range ops {
		if syncName(o.call.Name) {
			out = append(out, o.call.Path)
		}
	}
	return out
}

func journalInfo(path string) (*journalfile.Journal, bool) {
	j, err := journalfile.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false
	}
	if err != nil {
		fatal("read journal", err)
	}
	return j, true
}

func journalSummary(j *journalfile.Journal, present bool) string {
	if !present {
		return "no -journal"
	}
	if j.Size == 0 {
		return "-journal 0 bytes (not hot)"
	}
	var b strings.Builder
	hot := "not hot"
	if j.LooksHot() {
		hot = "hot"
	}
	fmt.Fprintf(&b, "-journal %d bytes, first byte 0x%02x (%s)", j.Size, j.FirstByte, hot)
	if len(j.Segments) == 0 {
		return b.String()
	}
	h := j.Segments[0].Header
	fmt.Fprintf(&b, ", header %s", h.State)
	switch h.State {
	case journalfile.HeaderSynced:
		fmt.Fprintf(&b, " (record count %d)", h.RecordCount)
	case journalfile.HeaderNoSync:
		b.WriteString(" (record count 0xffffffff: read to end of file)")
	case journalfile.HeaderNotYetSynced:
		b.WriteString(" (magic and record count still zero)")
	case journalfile.HeaderZeroed:
		return b.String()
	}
	if h.State == journalfile.HeaderSynced || h.State == journalfile.HeaderNoSync || h.State == journalfile.HeaderNotYetSynced {
		fmt.Fprintf(&b, ", initial size %d pages, sector %d, page %d", h.InitialPages, h.SectorSize, h.PageSize)
	}
	fmt.Fprintf(&b, ", %d complete record(s) %s, %d checksum failure(s)", j.RecordsPresent(), recordPages(j), j.ChecksumFailures())
	if j.TrailingByte > 0 {
		fmt.Fprintf(&b, ", %d byte(s) of an incomplete record", j.TrailingByte)
	}
	return b.String()
}

func recordPages(j *journalfile.Journal) string {
	var pages []uint32
	for _, s := range j.Segments {
		for _, r := range s.Records {
			pages = append(pages, r.Page)
		}
	}
	return pageList(pages)
}

func playbackSummary(j *journalfile.Journal, present bool) string {
	if !present {
		return "nothing to roll back"
	}
	p := j.Playback()
	if !p.Hot {
		return "ignores it (not hot)"
	}
	if len(p.Pages) == 0 {
		return fmt.Sprintf("hot, but restores no page (%s)", p.StopReason)
	}
	return fmt.Sprintf("rolls back: writes pages %s from the journal, truncates to %d pages", pageList(p.Pages), p.TruncateTo)
}

type killResult struct {
	target        op
	landed        string
	landedOK      bool
	dbBefore      string
	dbBeforeImage []byte
	journal       *journalfile.Journal
	journalThere  bool
	integrity     string
	after         outcome
	journalAfter  string
	nonce         uint32
	duration      time.Duration
}

func killRun(ctx context.Context, dir string, s *states, journalMode, synchronous string, target op) *trace.Result {
	prepareDir(dir, s.prePath)
	command, err := trace.SelfCommand("child-step", dir, journalMode, synchronous, strconv.Itoa(s.step))
	if err != nil {
		fatal("self command", err)
	}
	run, err := trace.Run(ctx, trace.Options{Command: command, KillAt: &trace.KillAt{Syscalls: []string{target.call.Name}, When: target.ordinal}})
	if err != nil {
		fatal("strace kill run", err)
	}
	return run
}

// runKill runs one child process killed by SIGKILL on entry to target, inspects
// the resulting database and -journal files in plain Go before SQLite sees
// them, then reopens with sqlitex.Open to trigger sqlite3PagerSharedLock ->
// hasHotJournal -> pager_playback (src/pager.c:5300-5405) and classifies the
// recovered database.
func runKill(ctx context.Context, dir string, s *states, journalMode, synchronous string, target op) killResult {
	result := killResult{target: target}
	start := time.Now()
	run := killRun(ctx, dir, s, journalMode, synchronous, target)
	result.duration = time.Since(start)
	// Confirm strace intercepted the exact intended syscall on entry before the
	// kernel executed it (marked Unfinished and terminated by SIGKILL).
	for _, call := range run.Log.Calls {
		if call.Unfinished && call.Name == target.call.Name && trace.UnderDir(dir)(call) {
			result.landed = describe(trace.RelativeTo(dir, []trace.Syscall{call})[0])
		}
	}
	result.landedOK = run.Killed == "SIGKILL" && result.landed == target.text
	path := filepath.Join(dir, dbName)
	image, err := os.ReadFile(path)
	if err != nil {
		fatal("read killed db", err)
	}
	result.dbBeforeImage = image
	result.dbBefore = s.classifyImage(image)
	result.journal, result.journalThere = journalInfo(path + "-journal")
	if result.journalThere && len(result.journal.Segments) > 0 {
		result.nonce = result.journal.Segments[0].Header.Nonce
	}
	var canon string
	result.integrity, canon = reopen(ctx, path)
	result.after = s.outcomeOf(path, canon)
	after, there := journalInfo(path + "-journal")
	switch {
	case !there:
		result.journalAfter = "no -journal"
	case after.LooksHot():
		result.journalAfter = fmt.Sprintf("-journal still hot (%d bytes)", after.Size)
	default:
		result.journalAfter = fmt.Sprintf("cold -journal left (%d bytes, first byte 0x%02x)", after.Size, after.FirstByte)
	}
	return result
}

func reopen(ctx context.Context, path string) (string, string) {
	db, err := sqlitex.Open(ctx, path, stepOptions(sqlitex.JournalDelete, sqlitex.SyncFull))
	if err != nil {
		return "open failed: " + strings.ReplaceAll(err.Error(), path, filepath.Base(path)), ""
	}
	defer db.Close()
	integrity := integrityCheck(ctx, db.DB)
	canon, err := canonicalHash(ctx, db.DB, path)
	if err != nil {
		return integrity + "; VACUUM INTO failed: " + err.Error(), ""
	}
	return integrity, canon
}

func integrityCheck(ctx context.Context, db *sql.DB) string {
	rows, err := db.QueryContext(ctx, "PRAGMA integrity_check")
	if err != nil {
		return "integrity_check failed: " + err.Error()
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return "integrity_check failed: " + err.Error()
		}
		lines = append(lines, line)
	}
	if err := rows.Err(); err != nil {
		return "integrity_check failed: " + err.Error()
	}
	return strings.Join(lines, " | ")
}

func runKills(ctx context.Context, base string, s *states, journalMode, synchronous string, targets []op) []killResult {
	results := make([]killResult, len(targets))
	jobs := make(chan int)
	var wg sync.WaitGroup
	for range max(1, *workers) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				results[i] = runKill(ctx, filepath.Join(base, fmt.Sprintf("kill-%02d", i+1)), s, journalMode, synchronous, targets[i])
			}
		}()
	}
	for i := range targets {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	return results
}

func baseline(ctx context.Context, dir string, s *states, journalMode, synchronous string) (window, []byte) {
	prepareDir(dir, s.prePath)
	command, err := trace.SelfCommand("child-step", dir, journalMode, synchronous, strconv.Itoa(s.step))
	if err != nil {
		fatal("self command", err)
	}
	run, err := trace.Run(ctx, trace.Options{Command: command})
	if err != nil {
		fatal("strace baseline", err)
	}
	if run.ExitCode != 0 {
		fatal("baseline child", fmt.Errorf("exit %d: %s", run.ExitCode, run.Stderr))
	}
	w, err := extractWindow(run.Log, dir)
	if err != nil {
		fatal("baseline window", err)
	}
	image, err := os.ReadFile(filepath.Join(dir, dbName))
	if err != nil {
		fatal("read baseline db", err)
	}
	return w, image
}

func sqlLiteral(v any) string {
	switch x := v.(type) {
	case int64:
		return strconv.FormatInt(x, 10)
	case int:
		return strconv.Itoa(x)
	case string:
		return "'" + strings.ReplaceAll(x, "'", "''") + "'"
	case []byte:
		return "X'" + hex.EncodeToString(x) + "'"
	}
	fatal("literal", fmt.Errorf("unsupported argument type %T", v))
	return ""
}

func renderStatement(st recipe.Statement) string {
	var b strings.Builder
	arg := 0
	for _, r := range st.SQL {
		if r == '?' {
			b.WriteString(sqlLiteral(st.Args[arg]))
			arg++
			continue
		}
		b.WriteRune(r)
	}
	if arg != len(st.Args) {
		fatal("render", fmt.Errorf("%d placeholders for %d arguments in %q", arg, len(st.Args), st.SQL))
	}
	return b.String()
}

func cliScript(dir, synchronous string, step recipe.Step) string {
	o := stepOptions(sqlitex.JournalDelete, synchronous)
	var b strings.Builder
	fmt.Fprintf(&b, "PRAGMA journal_mode=%s;\nPRAGMA synchronous=%s;\nPRAGMA foreign_keys=ON;\nPRAGMA secure_delete=ON;\nPRAGMA cache_size=%d;\nPRAGMA busy_timeout=%d;\n", sqlitex.JournalDelete, synchronous, o.Conn.CacheSize, o.Conn.BusyTimeout.Milliseconds())
	fmt.Fprintf(&b, ".read '%s'\nBEGIN IMMEDIATE;\n", filepath.Join(dir, markerBegin))
	for _, st := range step.Statements {
		b.WriteString(renderStatement(st))
		b.WriteString(";\n")
	}
	fmt.Fprintf(&b, "COMMIT;\n.read '%s'\n", filepath.Join(dir, markerEnd))
	return b.String()
}

// twinCheck replays the same recipe step as literal SQL through the pinned C
// sqlite3 CLI under strace and checks that its writes, syncs, and unlinks match
// the translated Go driver in file, offset, length, and order.
func twinCheck(ctx context.Context, cli sqlitex.CLI, dir string, s *states, synchronous string, goWindow window) {
	prepareDir(dir, s.prePath)
	command := cli.Command(ctx, sqlitex.CLIRun{}).Args
	command[0] = cli.Path
	path := filepath.Join(dir, dbName)
	run, err := trace.Run(ctx, trace.Options{Command: command, Dir: dir, Stdin: strings.NewReader(sqlitex.OpenScript(path, cliScript(dir, synchronous, s.stepStatements)))})
	if err != nil {
		fatal("strace CLI", err)
	}
	if run.ExitCode != 0 {
		fatal("CLI", fmt.Errorf("exit %d: %s", run.ExitCode, run.Stderr))
	}
	cliWindow, err := extractWindow(run.Log, dir)
	if err != nil {
		fatal("CLI window", err)
	}
	goOps, cliOps := writesAndSyncs(goWindow.ops), writesAndSyncs(cliWindow.ops)
	same := slices.Equal(goOps, cliOps)
	if !same {
		for i := range max(len(goOps), len(cliOps)) {
			var g, c string
			if i < len(goOps) {
				g = goOps[i]
			}
			if i < len(cliOps) {
				c = cliOps[i]
			}
			if g != c {
				fmt.Printf("  twin #%d Go: %-45s CLI: %s\n", i+1, g, c)
			}
		}
	}
	verdict := "identical to the Go driver's"
	if !same {
		verdict = "DIFFERENT from the Go driver's"
	}
	fmt.Printf("twin check: the C CLI running the same step as literal SQL makes %d writes, syncs and unlinks, %s\n", len(cliOps), verdict)
	fmt.Printf("INFO: CLI sync syscall: %s\n", syncSyscallNames(cliWindow.ops))
	match := s.matchFile(path)
	check(same && match.Step == s.step && match.Exact, fmt.Sprintf("synchronous=%s: the C CLI makes the same writes, syncs and unlink in the same order and leaves the post-step bytes", synchronous))
}

func writesAndSyncs(ops []op) []string {
	var out []string
	for _, o := range ops {
		if killable(o.call.Name) {
			out = append(out, o.text)
		}
	}
	return out
}

func syncSyscallNames(ops []op) string {
	var names []string
	for _, o := range ops {
		if syncName(o.call.Name) && !slices.Contains(names, o.call.Name) {
			names = append(names, o.call.Name)
		}
	}
	if len(names) == 0 {
		return "(none)"
	}
	return strings.Join(names, ",")
}

type levelResult struct {
	level   string
	results []killResult
}

func checkBaseline(w window, image []byte, s *states, journalMode, synchronous string) {
	sequence := collapse(w.ops)
	records, dbPages := countKind(w.ops, "journal-record"), countKind(w.ops, "db-page")
	syncs := syncTargets(w.ops)
	fmt.Printf("sequence: %s\n", sequence)
	fmt.Printf("journaled pages %d (%d record writes), database page writes %d, syncs %d %v\n", records/3, records, dbPages, len(syncs), syncs)
	fmt.Printf("INFO: sync syscall used by the Go driver: %s\n", syncSyscallNames(w.ops))
	label := fmt.Sprintf("step %d, journal_mode=%s, synchronous=%s", s.step, journalMode, synchronous)
	check(sequence == expectedSequence(journalMode, synchronous, records, dbPages), label+": order is "+expectedSequence(journalMode, synchronous, records, dbPages))
	if journalMode == sqlitex.JournalDelete {
		wantSyncs := map[string]int{sqlitex.SyncFull: 4, sqlitex.SyncNormal: 3, sqlitex.SyncOff: 0}[synchronous]
		check(len(syncs) == wantSyncs, fmt.Sprintf("%s: %d syncs per commit (journal, directory and database syncs counted)", label, wantSyncs))
	}
	check(records%3 == 0 && records > 0, label+": each journal record is three pwrite64 calls: page number (4 bytes), original page image (4096), checksum (4)")
	check(bytes.Equal(image, s.post), label+": the traced commit leaves exactly the post-step bytes")
}

func runLevel(ctx context.Context, base string, s *states, synchronous string, cli sqlitex.CLI, haveCLI, verbose bool) levelResult {
	levelDir := filepath.Join(base, strings.ToLower(synchronous))
	if verbose {
		fmt.Printf("\n== D. step %d, synchronous=%s: one commit traced, then killed at every write and sync ==\n", s.step, synchronous)
	} else {
		fmt.Printf("\n-- step %d, synchronous=%s --\n", s.step, synchronous)
	}
	w, image := baseline(ctx, filepath.Join(levelDir, "baseline"), s, sqlitex.JournalDelete, synchronous)
	if verbose {
		fmt.Printf("the commit's syscalls on the database directory (writer thread, between the step's start and its return):\n")
		for i, o := range w.ops {
			fmt.Printf("  %3d %-16s %s\n", i+1, o.kind, o.text)
		}
	}
	checkBaseline(w, image, s, sqlitex.JournalDelete, synchronous)
	if haveCLI && verbose {
		twinCheck(ctx, cli, filepath.Join(levelDir, "twin"), s, synchronous, w)
	}

	fmt.Printf("kill points: %d (every pwrite64, sync and unlink of the commit, then the first syscall after the step returned)\n", len(w.targets))
	started := time.Now()
	results := runKills(ctx, levelDir, s, sqlitex.JournalDelete, synchronous, w.targets)
	fmt.Printf("INFO: %d kill runs took %s with %d workers\n", len(results), time.Since(started).Round(time.Millisecond), max(1, *workers))
	unlinkIndex := -1
	for i, r := range results {
		if r.target.kind == "journal-unlink" {
			unlinkIndex = i
		}
	}
	allLanded, neverThird, integrityOK, coveredOK := true, true, true, true
	flipOK := unlinkIndex >= 0
	exactOnly := true
	beforeExact, beforeFreelist, after := 0, 0, 0
	for i, r := range results {
		if verbose {
			fmt.Printf("kill %02d on entry to %-38s [%s]\n", i+1, r.target.text, r.target.kind)
			fmt.Printf("    on disk:  database %s\n", r.dbBefore)
			fmt.Printf("              %s\n", journalSummary(r.journal, r.journalThere))
			fmt.Printf("              SQLite on reopen %s\n", playbackSummary(r.journal, r.journalThere))
			fmt.Printf("    reopened: %s; integrity_check %s; %s\n", r.after.describe(s), r.integrity, r.journalAfter)
		}
		if r.journalThere && len(r.journal.Segments) > 0 {
			fmt.Printf("INFO: step %d %s kill %02d journal nonce 0x%08x, run %s\n", s.step, synchronous, i+1, r.nonce, r.duration.Round(time.Millisecond))
		}
		if !r.landedOK {
			allLanded = false
			fmt.Printf("    kill %02d landed on %q, wanted %q\n", i+1, r.landed, r.target.text)
		}
		if r.integrity != "ok" {
			integrityOK = false
		}
		switch r.after.kind {
		case outcomePre:
			beforeExact++
		case outcomePreFreelist:
			beforeFreelist++
			exactOnly = false
		case outcomePost:
			after++
		default:
			neverThird, exactOnly = false, false
		}
		if (i <= unlinkIndex && !r.after.before()) || (i > unlinkIndex && r.after.kind != outcomePost) {
			flipOK = false
		}
		if i <= unlinkIndex && !covered(r, s) {
			coveredOK = false
			fmt.Printf("    kill %02d: the journal does not cover every changed page of the database file\n", i+1)
		}
	}
	label := fmt.Sprintf("step %d, synchronous=%s", s.step, synchronous)
	fmt.Printf("outcomes: %d pre-step exact, %d pre-step except reused freelist leaves, %d post-step exact, %d other\n", beforeExact, beforeFreelist, after, len(results)-beforeExact-beforeFreelist-after)
	check(allLanded, label+": every kill landed on the intended syscall (strace saw it unfinished and the child died of SIGKILL)")
	check(neverThird, label+": every reopened database is the pre-step or the post-step state, never a third (pre-step allows reused freelist-leaf pages, see above)")
	if len(s.preFreeLeaves) == 0 {
		check(exactOnly, label+": the pre-step file has no freelist, and every reopened database equals the pre-step or post-step bytes exactly")
	}
	check(flipOK, label+": every kill up to and including the journal unlink gives pre-step; the kill after it gives post-step (the unlink is the commit point)")
	check(integrityOK, label+": integrity_check is ok after every reopen")
	check(coveredOK, label+": for every kill up to the commit point, whenever the database file differs from pre-step before reopen, the journal is hot and its records cover every changed page except pages appended past the original size (removed by truncation) and reused freelist leaves")
	return levelResult{level: synchronous, results: results}
}

// covered verifies that whenever a pre-commit crash leaves modified bytes in
// the database file, the journal is hot and accounts for every changed page:
// appended pages (> prePages) are removed by truncation (src/pager.c:2952),
// existing modified pages are listed in Playback().Pages, and reused freelist
// leaves were fetched with PAGER_GET_NOCONTENT (src/pager.c:5645-5656).
func covered(r killResult, s *states) bool {
	changed := differingPages(r.dbBeforeImage, s.pre)
	if len(changed) == 0 {
		return true
	}
	if !r.journalThere {
		return false
	}
	p := r.journal.Playback()
	if !p.Hot {
		return false
	}
	for _, page := range changed {
		switch {
		case int(page) > s.prePages:
			if p.TruncateTo != uint32(s.prePages) {
				return false
			}
		case slices.Contains(p.Pages, page), slices.Contains(s.preFreeLeaves, page):
		default:
			return false
		}
	}
	return true
}

func printSummary(s *states, levels []levelResult) {
	fmt.Printf("\n== summary for step %d: reopened state per kill point, in commit order ==\n", s.step)
	for _, level := range levels {
		var line strings.Builder
		for _, r := range level.results {
			line.WriteByte(r.after.letter())
		}
		fmt.Printf("synchronous=%-6s %s\n", level.level, line.String())
	}
	fmt.Printf("(B = pre-step exact bytes, b = pre-step except reused freelist leaves, A = post-step exact bytes, ? = anything else; the last B or b is the kill on entry to the journal unlink)\n")
}

func journalOpsIn(calls []trace.Syscall) []string {
	var ops []string
	for _, call := range calls {
		if !strings.HasSuffix(call.Path, "-journal") {
			continue
		}
		if openName(call.Name) || unlinkName(call.Name) || syncName(call.Name) || call.Name == "pwrite64" {
			ops = append(ops, describe(call))
		}
	}
	return ops
}

func walWritesIn(calls []trace.Syscall) int {
	n := 0
	for _, call := range calls {
		if strings.HasSuffix(call.Path, "-wal") && call.Name == "pwrite64" {
			n++
		}
	}
	return n
}

func segmentsByMarkers(log *trace.StraceLog, dir, prefix string, count int) [][]trace.Syscall {
	segments := make([][]trace.Syscall, count)
	current := -1
	for _, call := range log.Calls {
		if openName(call.Name) && strings.HasPrefix(call.Path, filepath.Join(dir, prefix)) {
			n, err := strconv.Atoi(strings.TrimPrefix(call.Path, filepath.Join(dir, prefix)))
			if err == nil {
				current = n - 1
			}
			continue
		}
		if current >= 0 && current < count && trace.UnderDir(dir)(call) {
			segments[current] = append(segments[current], trace.RelativeTo(dir, []trace.Syscall{call})[0])
		}
	}
	return segments
}

// partA shows where a WAL-default platform still meets rollback mode: new
// databases default to journal_mode=delete (header bytes 18-19 = 1,1), and
// switching a new file to WAL—including inside sqlitex.Open—writes page 1
// through a rollback -journal before WAL mode takes effect.
func partA(ctx context.Context, base string) {
	fmt.Printf("\n== A. where a WAL-default platform meets rollback mode ==\n")
	dir := filepath.Join(base, "a")
	mkdir(dir)
	path := filepath.Join(dir, "plain.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		fatal("open plain", err)
	}
	db.SetMaxOpenConns(1)
	var mode string
	if err := db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
		fatal("journal_mode", err)
	}
	fmt.Printf("a new file opened by the driver with no settings reports journal_mode=%s\n", mode)
	check(mode == sqlitex.JournalDelete, "a new database starts in rollback-journal mode (journal_mode=delete) until something switches it")
	if _, err := db.ExecContext(ctx, "CREATE TABLE t(x)"); err != nil {
		fatal("create", err)
	}
	if err := db.Close(); err != nil {
		fatal("close", err)
	}
	header := readHeaderBytes(path)
	fmt.Printf("after its first CREATE TABLE: header bytes 18,19 = %d,%d (1 = rollback journal)\n", header[18], header[19])
	check(header[18] == 1 && header[19] == 1, "the file records rollback mode in header bytes 18 and 19 (value 1)")
	db, err = sql.Open("sqlite", path)
	if err != nil {
		fatal("reopen plain", err)
	}
	db.SetMaxOpenConns(1)
	if err := db.QueryRowContext(ctx, "PRAGMA journal_mode=wal").Scan(&mode); err != nil {
		fatal("switch", err)
	}
	if err := db.Close(); err != nil {
		fatal("close", err)
	}
	header = readHeaderBytes(path)
	db, err = sql.Open("sqlite", path)
	if err != nil {
		fatal("reopen plain", err)
	}
	var again string
	if err := db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&again); err != nil {
		fatal("journal_mode", err)
	}
	if err := db.Close(); err != nil {
		fatal("close", err)
	}
	fmt.Printf("after PRAGMA journal_mode=wal: header bytes 18,19 = %d,%d; a fresh connection reports journal_mode=%s\n", header[18], header[19], again)
	check(header[18] == 2 && header[19] == 2 && again == sqlitex.JournalWAL, "switching to WAL rewrites bytes 18 and 19 to 2, and the mode persists in the file")

	if !trace.Supported {
		return
	}
	cases := []struct {
		role   string
		labels []string
	}{
		{"plain", []string{"CREATE TABLE (driver defaults, first write)"}},
		{"switch", []string{"PRAGMA journal_mode=wal on the new empty file", "CREATE TABLE after the switch"}},
		{"platform", []string{"sqlitex.Open of a new file with journal_mode=wal", "CREATE TABLE after it"}},
	}
	journalBySegment := map[string][][]string{}
	walBySegment := map[string][]int{}
	for _, c := range cases {
		target := filepath.Join(dir, c.role+".db")
		if err := recipe.RemoveDatabase(target); err != nil {
			fatal("remove", err)
		}
		for i := 1; i <= len(c.labels)+1; i++ {
			if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%s-marker-%d", c.role, i)), nil, 0o644); err != nil {
				fatal("marker", err)
			}
		}
		command, err := trace.SelfCommand("child-create", c.role, target)
		if err != nil {
			fatal("self command", err)
		}
		run, err := trace.Run(ctx, trace.Options{Command: command})
		if err != nil {
			fatal("strace create", err)
		}
		if run.ExitCode != 0 {
			fatal("create child", fmt.Errorf("exit %d: %s", run.ExitCode, run.Stderr))
		}
		segments := segmentsByMarkers(run.Log, dir, c.role+"-marker-", len(c.labels))
		for i, label := range c.labels {
			ops := journalOpsIn(segments[i])
			wal := walWritesIn(segments[i])
			journalBySegment[c.role] = append(journalBySegment[c.role], ops)
			walBySegment[c.role] = append(walBySegment[c.role], wal)
			fmt.Printf("%s: -journal %v; -wal writes %d\n", label, ops, wal)
		}
	}
	usesJournal := func(ops []string) bool {
		return len(ops) >= 2 && strings.HasPrefix(ops[0], "open ") && strings.HasPrefix(ops[len(ops)-1], "unlink ")
	}
	check(usesJournal(journalBySegment["plain"][0]), "a file created with driver defaults commits its first transaction through a -journal (opened, written, synced, unlinked)")
	check(usesJournal(journalBySegment["switch"][0]) && walBySegment["switch"][0] == 0, "the switch to WAL is itself a rollback-mode commit: it writes page 1 through a -journal")
	check(len(journalBySegment["switch"][1]) == 0 && walBySegment["switch"][1] > 0, "after the switch, commits go to the -wal and no -journal is opened")
	check(usesJournal(journalBySegment["platform"][0]) && len(journalBySegment["platform"][1]) == 0, "a WAL database created by the platform opener (sqlitex.Open) is also born through one -journal commit, then never uses one")
}

func readHeaderBytes(path string) []byte {
	data, err := os.ReadFile(path)
	if err != nil {
		fatal("read header", err)
	}
	if len(data) < 100 {
		fatal("read header", fmt.Errorf("file has %d bytes", len(data)))
	}
	return data[:100]
}

func childCreate(role, path string) {
	ctx := context.Background()
	dir := filepath.Dir(path)
	mark := func(i int) { touch(dir, fmt.Sprintf("%s-marker-%d", role, i)) }
	exec := func(db *sql.DB, statement string) {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			fatal(statement, err)
		}
	}
	switch role {
	case "plain", "switch":
		db, err := sql.Open("sqlite", path)
		if err != nil {
			fatal("plain open", err)
		}
		db.SetMaxOpenConns(1)
		mark(1)
		if role == "switch" {
			exec(db, "PRAGMA journal_mode=wal")
			mark(2)
			exec(db, "CREATE TABLE t(x)")
			mark(3)
		} else {
			exec(db, "CREATE TABLE t(x)")
			mark(2)
		}
		if err := db.Close(); err != nil {
			fatal("close", err)
		}
	case "platform":
		mark(1)
		db, err := sqlitex.Open(ctx, path, sqlitex.DefaultOptions())
		if err != nil {
			fatal("platform open", err)
		}
		mark(2)
		exec(db.DB, "CREATE TABLE t(x)")
		mark(3)
		if err := db.Close(); err != nil {
			fatal("close", err)
		}
	default:
		fatal("child-create", fmt.Errorf("unknown role %q", role))
	}
}

// partC inspects the rollback journal of an open, uncommitted transaction at
// synchronous=FULL using internal/journalfile. Reading -journal while the
// connection is open is safe because SQLite locks only the main database file.
func partC(ctx context.Context, base string, s *states) {
	fmt.Printf("\n== C. the journal of an open transaction, read before COMMIT ==\n")
	dir := filepath.Join(base, "c")
	prepareDir(dir, s.prePath)
	db, err := sqlitex.Open(ctx, filepath.Join(dir, dbName), stepOptions(sqlitex.JournalDelete, sqlitex.SyncFull))
	if err != nil {
		fatal("open", err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		fatal("begin", err)
	}
	if err := s.stepStatements.ExecIn(ctx, tx); err != nil {
		fatal("exec", err)
	}
	j, present := journalInfo(filepath.Join(dir, dbName+"-journal"))
	if !present {
		fatal("journal", errors.New("no -journal during the transaction"))
	}
	fmt.Printf("every statement of step %d executed, COMMIT not yet issued: %s\n", s.step, journalSummary(j, present))
	allOriginal := true
	for _, segment := range j.Segments {
		for _, r := range segment.Records {
			same := bytes.Equal(pageAt(s.pre, int(r.Page-1)*pageSize), r.Image)
			allOriginal = allOriginal && same && r.ChecksumOK()
			role := "b-tree or overflow page"
			if r.Page == 1 {
				role = "header and schema root"
			}
			image := "equals the pre-step page"
			if !same {
				image = "DIFFERS from the pre-step page"
			}
			fmt.Printf("  record at offset %6d: page %3d (%s), checksum %s, image %s\n", r.Offset, r.Page, role, okWord(r.ChecksumOK()), image)
		}
	}
	check(allOriginal, "every journal record holds the page's original (pre-step) image with a valid checksum")
	check(j.Segments[0].Header.State == journalfile.HeaderNotYetSynced && !j.LooksHot(), "before COMMIT (synchronous=FULL) the header's magic and record count are still zero, so the journal is not hot yet")
	check(j.Segments[0].Header.InitialPages == uint32(s.prePages), fmt.Sprintf("the header records the database size before the transaction (%d pages)", s.prePages))
	fmt.Printf("pre-step freelist leaf pages (never journaled when reused): %s\n", pageList(s.preFreeLeaves))
	if err := tx.Rollback(); err != nil {
		fatal("rollback", err)
	}
	if err := db.Close(); err != nil {
		fatal("close", err)
	}
	match := s.matchFile(filepath.Join(dir, dbName))
	fmt.Printf("after ROLLBACK and close: %s\n", map[bool]string{true: "pre-step exact bytes", false: "NOT the pre-step bytes"}[match.Step == s.preStep && match.Exact])
	check(match.Step == s.preStep && match.Exact, "a ROLLBACK by the live connection restores the pre-step bytes exactly (the reused freelist pages were never written to the file)")
	check(recipe.CheckAtRest(filepath.Join(dir, dbName)) == nil, "no -journal remains after ROLLBACK and close")
}

func okWord(ok bool) string {
	if ok {
		return "ok"
	}
	return "BAD"
}

func damageReport(ctx context.Context, path string, s *states) (string, outcome) {
	db, err := sqlitex.Open(ctx, path, stepOptions(sqlitex.JournalDelete, sqlitex.SyncFull))
	if err != nil {
		return "open failed: " + strings.ReplaceAll(err.Error(), path, filepath.Base(path)), s.outcomeOf(path, "")
	}
	integrity := integrityCheck(ctx, db.DB)
	counts := tableCounts(ctx, db.DB)
	canon, err := canonicalHash(ctx, db.DB, path)
	if err != nil {
		canon = ""
	}
	if err := db.Close(); err != nil {
		fatal("close", err)
	}
	return fmt.Sprintf("integrity_check: %s; %s", integrity, counts), s.outcomeOf(path, canon)
}

func tableCounts(ctx context.Context, db *sql.DB) string {
	var parts []string
	var userVersion int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&userVersion); err != nil {
		parts = append(parts, "user_version error: "+err.Error())
	} else {
		parts = append(parts, fmt.Sprintf("user_version=%d", userVersion))
	}
	for _, table := range []string{"accounts", "documents", "events"} {
		var n int
		if err := db.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&n); err != nil {
			parts = append(parts, fmt.Sprintf("count(%s) error: %v", table, err))
			continue
		}
		parts = append(parts, fmt.Sprintf("%s=%d", table, n))
	}
	return strings.Join(parts, " ")
}

func countsOf(ctx context.Context, path string) string {
	db, err := sqlitex.Open(ctx, path, stepOptions(sqlitex.JournalDelete, sqlitex.SyncFull))
	if err != nil {
		fatal("open", err)
	}
	defer db.Close()
	return tableCounts(ctx, db.DB)
}

func recreateKilled(ctx context.Context, dir string, s *states, target op) {
	run := killRun(ctx, dir, s, sqlitex.JournalDelete, sqlitex.SyncFull, target)
	if run.Killed != "SIGKILL" {
		fatal("kill", fmt.Errorf("child was not killed: exit %d", run.ExitCode))
	}
}

// partE demonstrates three failure cases of losing a hot journal: deleting it
// mid-write, copying the database file alone versus together with -journal, and
// deleting it after all database writes completed (silently committing a
// transaction whose COMMIT never returned).
func partE(ctx context.Context, base string, s *states, fullTargets []op) {
	fmt.Printf("\n== E. failure cases: losing the hot journal (step %d, synchronous=FULL) ==\n", s.step)
	fmt.Printf("contents: pre-step %s; post-step %s\n", countsOf(ctx, s.prePath), countsOf(ctx, filepath.Join(filepath.Dir(s.prePath), "post.db")))
	var dbWrites []op
	var dbSync op
	for _, t := range fullTargets {
		switch t.kind {
		case "db-page":
			dbWrites = append(dbWrites, t)
		case "db-sync":
			dbSync = t
		}
	}
	if len(dbWrites) < 2 || dbSync.kind == "" {
		fatal("failure cases", errors.New("the synchronous=FULL commit has too few database writes"))
	}
	middle := dbWrites[len(dbWrites)/2]
	fmt.Printf("crash state: killed on entry to %s (database write %d of %d)\n", middle.text, len(dbWrites)/2+1, len(dbWrites))

	deleted := filepath.Join(base, "e-deleted")
	recreateKilled(ctx, deleted, s, middle)
	image, err := os.ReadFile(filepath.Join(deleted, dbName))
	if err != nil {
		fatal("read", err)
	}
	j, present := journalInfo(filepath.Join(deleted, dbName+"-journal"))
	fmt.Printf("on disk: database %s\n         %s\n", s.classifyImage(image), journalSummary(j, present))

	copied := filepath.Join(base, "e-copied")
	original := filepath.Join(base, "e-with-journal")
	mkdir(copied)
	mkdir(original)
	copyFile(filepath.Join(deleted, dbName), filepath.Join(copied, dbName))
	copyFile(filepath.Join(deleted, dbName), filepath.Join(original, dbName))
	copyFile(filepath.Join(deleted, dbName+"-journal"), filepath.Join(original, dbName+"-journal"))

	if err := os.Remove(filepath.Join(deleted, dbName+"-journal")); err != nil {
		fatal("delete journal", err)
	}
	report, deletedOutcome := damageReport(ctx, filepath.Join(deleted, dbName), s)
	fmt.Printf("1. -journal deleted, then reopened: %s\n   state: %s\n", report, deletedOutcome.describe(s))
	check(deletedOutcome.kind == outcomeUnreadable || deletedOutcome.kind == outcomeThird, "deleting the hot journal before reopening leaves a damaged database that is neither the pre-step nor the post-step state")

	copyReport, copyOutcome := damageReport(ctx, filepath.Join(copied, dbName), s)
	fmt.Printf("2. the database file copied without its -journal, copy opened: %s\n   state: %s\n", copyReport, copyOutcome.describe(s))
	check(copyOutcome.kind == deletedOutcome.kind && copyReport == report && !copyOutcome.before(), "a copy made without the -journal is the same damaged database")
	originalReport, originalOutcome := damageReport(ctx, filepath.Join(original, dbName), s)
	fmt.Printf("   the same crashed bytes copied together with their -journal, opened: %s\n   state: %s\n", originalReport, originalOutcome.describe(s))
	check(originalOutcome.before(), "the same crashed file copied together with its -journal rolls back to pre-step")

	late := filepath.Join(base, "e-late")
	recreateKilled(ctx, late, s, dbSync)
	image, err = os.ReadFile(filepath.Join(late, dbName))
	if err != nil {
		fatal("read", err)
	}
	fmt.Printf("3. killed on entry to %s (every database page written, journal still hot): database %s\n", dbSync.text, s.classifyImage(image))
	if err := os.Remove(filepath.Join(late, dbName+"-journal")); err != nil {
		fatal("delete journal", err)
	}
	lateReport, lateOutcome := damageReport(ctx, filepath.Join(late, dbName), s)
	fmt.Printf("   -journal deleted, then reopened: %s\n   state: %s\n", lateReport, lateOutcome.describe(s))
	check(lateOutcome.kind == outcomePost, "deleting the journal after the last database write turns a commit that never returned into a committed one (post-step)")
}

// partF locates the exact commit-point syscall for DELETE (unlink), TRUNCATE
// (ftruncate to 0), and PERSIST (28-byte header zero write) by killing on entry
// to that syscall and on the syscall immediately after it
// (pager_end_transaction, src/pager.c:2086-2122).
func partF(ctx context.Context, base string, s *states) {
	fmt.Printf("\n== F. the commit point in DELETE, TRUNCATE and PERSIST journal modes (step %d, synchronous=FULL) ==\n", s.step)
	type row struct{ mode, point, at, after, left string }
	var rows []row
	for _, mode := range []string{sqlitex.JournalDelete, sqlitex.JournalTruncate, sqlitex.JournalPersist} {
		dir := filepath.Join(base, "f-"+mode)
		fmt.Printf("-- journal_mode=%s --\n", mode)
		w, image := baseline(ctx, filepath.Join(dir, "baseline"), s, mode, sqlitex.SyncFull)
		checkBaseline(w, image, s, mode, sqlitex.SyncFull)
		left, there := journalInfo(filepath.Join(dir, "baseline", dbName+"-journal"))
		leftText := journalSummary(left, there)
		commitKind := map[string]string{sqlitex.JournalDelete: "journal-unlink", sqlitex.JournalTruncate: "journal-truncate", sqlitex.JournalPersist: "journal-zero-header"}[mode]
		var commitPoint, next op
		for i, t := range w.targets {
			if t.kind == commitKind && i+1 < len(w.targets) {
				commitPoint, next = t, w.targets[i+1]
				break
			}
		}
		if commitPoint.kind == "" {
			check(false, mode+": commit point found in the trace")
			continue
		}
		at := runKill(ctx, filepath.Join(dir, "at"), s, mode, sqlitex.SyncFull, commitPoint)
		after := runKill(ctx, filepath.Join(dir, "after"), s, mode, sqlitex.SyncFull, next)
		rows = append(rows, row{mode, commitPoint.text, at.after.shortName(), after.after.shortName() + " (kill on " + next.kind + ")", leftText})
		check(at.landedOK && after.landedOK && at.after.before() && after.after.kind == outcomePost, mode+": a kill on entry to the commit point gives pre-step, the next kill gives post-step")
		if mode != sqlitex.JournalDelete {
			check(there && !left.LooksHot(), mode+": the journal file stays beside the database after a clean commit, and it is not hot")
		}
	}
	fmt.Printf("%-9s %-34s %-26s %-44s %s\n", "mode", "commit point (syscall)", "killed on entry", "killed on the next syscall", "-journal after a clean commit")
	for _, r := range rows {
		fmt.Printf("%-9s %-34s %-26s %-44s %s\n", r.mode, r.point, r.at, r.after, r.left)
	}
}

func (o outcome) shortName() string {
	switch o.kind {
	case outcomePre:
		return "before (pre-step)"
	case outcomePreFreelist:
		return "before (pre-step, logical)"
	case outcomePost:
		return "after (post-step)"
	}
	return "neither"
}

func portableLevels(ctx context.Context, base string, s *states) {
	for _, level := range []string{sqlitex.SyncFull, sqlitex.SyncNormal, sqlitex.SyncOff} {
		dir := filepath.Join(base, "portable-"+strings.ToLower(level))
		prepareDir(dir, s.prePath)
		db, err := sqlitex.Open(ctx, filepath.Join(dir, dbName), stepOptions(sqlitex.JournalDelete, level))
		if err != nil {
			fatal("open", err)
		}
		if err := recipe.ApplyStep(ctx, db, s.stepStatements); err != nil {
			fatal("apply", err)
		}
		if err := db.Close(); err != nil {
			fatal("close", err)
		}
		match := s.matchFile(filepath.Join(dir, dbName))
		check(match.Step == s.step && match.Exact, fmt.Sprintf("step %d, synchronous=%s: the step committed in process leaves the post-step bytes", s.step, level))
	}
}

func describeStates(s *states) {
	fmt.Println(s.stepStatements.Summary())
	fmt.Printf("pre-step state (after step %d): %d pages, sha256 %s, freelist leaves %s\n", s.preStep, s.prePages, s.hashes[s.preStep].Exact[:16], pageList(s.preFreeLeaves))
	fmt.Printf("post-step state (after step %d): %d pages, sha256 %s\n", s.step, s.postPages, s.hashes[s.step].Exact[:16])
	check(s.matchFile(s.prePath).Step == s.preStep && s.matchFile(filepath.Join(filepath.Dir(s.prePath), "post.db")).Step == s.step, fmt.Sprintf("the pre-step and post-step files equal the recipe's hashes for steps %d and %d", s.preStep, s.step))
}

func main() {
	if len(os.Args) == 6 && os.Args[1] == "child-step" {
		k, err := strconv.Atoi(os.Args[5])
		if err != nil {
			fatal("child step", err)
		}
		childStep(os.Args[2], os.Args[3], os.Args[4], k)
		return
	}
	if len(os.Args) == 4 && os.Args[1] == "child-create" {
		childCreate(os.Args[2], os.Args[3])
		return
	}
	flag.Parse()
	ctx := context.Background()
	started := time.Now()
	sqlitex.MustPinCheckMemory(ctx)
	work := sqlitex.MustWorkDir("4.1")
	base, err := filepath.EvalSymlinks(work)
	if err != nil {
		fatal("work dir", err)
	}
	fmt.Printf("INFO: work dir %s\n", displayPath(base))
	var cli sqlitex.CLI
	haveCLI := false
	if trace.Supported {
		located, cliErr := sqlitex.LocateCLI(ctx)
		cli, haveCLI = located, cliErr == nil
		if haveCLI {
			fmt.Println(cli.PinLine())
		} else {
			fmt.Printf("sqlite3 CLI: unavailable: %v\n", cliErr)
			failures = append(failures, "the pinned sqlite3 CLI is available")
		}
	}

	partA(ctx, base)

	r := rollbackRecipe()
	for _, k := range []int{*stepFlag, *exactFlag} {
		if k != 0 && (k < 1 || k > r.Steps) {
			fatal("step", fmt.Errorf("steps must be 1..%d, got %d", r.Steps, k))
		}
	}
	fmt.Printf("\n== B. the recipe step ==\n")
	fmt.Printf("recipe: %s\n", r)
	hashes, err := r.StepHashes(ctx)
	if err != nil {
		fatal("step hashes", err)
	}
	main := newStates(ctx, filepath.Join(base, fmt.Sprintf("step-%d", *stepFlag)), r, *stepFlag, hashes)
	describeStates(main)

	partC(ctx, base, main)

	if !trace.Supported {
		trace.PrintLinuxOnly("the strace of the commit, the kill at every write and sync, the failure cases and the commit-point table", packagePath)
		portableLevels(ctx, base, main)
		finish(started)
		return
	}

	levels := []string{sqlitex.SyncFull, sqlitex.SyncNormal, sqlitex.SyncOff}
	var mainResults []levelResult
	var fullTargets []op
	for _, level := range levels {
		result := runLevel(ctx, filepath.Join(base, fmt.Sprintf("step-%d", *stepFlag)), main, level, cli, haveCLI, true)
		mainResults = append(mainResults, result)
		if level == sqlitex.SyncFull {
			for _, kill := range result.results {
				fullTargets = append(fullTargets, kill.target)
			}
		}
	}
	printSummary(main, mainResults)

	if *exactFlag != 0 {
		fmt.Printf("\n== D2. a step whose pre-step file has no freelist: step %d, every kill point, all three levels ==\n", *exactFlag)
		exact := newStates(ctx, filepath.Join(base, fmt.Sprintf("step-%d", *exactFlag)), r, *exactFlag, hashes)
		describeStates(exact)
		var exactResults []levelResult
		for _, level := range levels {
			exactResults = append(exactResults, runLevel(ctx, filepath.Join(base, fmt.Sprintf("step-%d", *exactFlag)), exact, level, cli, false, false))
		}
		printSummary(exact, exactResults)
	}

	partE(ctx, base, main, fullTargets)
	partF(ctx, base, main)
	finish(started)
}

func finish(started time.Time) {
	fmt.Printf("INFO: runtime %s\n", time.Since(started).Round(100*time.Millisecond))
	fmt.Println()
	if len(failures) > 0 {
		fmt.Printf("ATOMIC COMMIT FAILED: %d claim(s) do not hold: %s\n", len(failures), strings.Join(failures, "; "))
		os.Exit(1)
	}
	fmt.Println("ATOMIC COMMIT OK")
}

func displayPath(path string) string {
	root, err := sqlitex.RepoRoot()
	if err != nil {
		return path
	}
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	relative, err := filepath.Rel(root, path)
	if err != nil || strings.HasPrefix(relative, "..") {
		return path
	}
	return filepath.ToSlash(relative)
}
```

#### Captured evidence (`4.1-atomic-commit-and-hot-journal.txt`)

<!-- include: evidence/4.1-atomic-commit-and-hot-journal.txt -->
```text
sqlite_source_id: 2026-07-24 19:02:57 bf7c7f30031888f4e796e429ab3978879485813aaca6f641c7b33e4e09459bcc (pin 3.53.4 ok)
INFO: work dir _work/4.1
sqlite3 CLI sqlite_source_id: 2026-07-24 19:02:57 bf7c7f30031888f4e796e429ab3978879485813aaca6f641c7b33e4e09459bcc (pin 3.53.4 ok)

== A. where a WAL-default platform meets rollback mode ==
a new file opened by the driver with no settings reports journal_mode=delete
check ok: a new database starts in rollback-journal mode (journal_mode=delete) until something switches it
after its first CREATE TABLE: header bytes 18,19 = 1,1 (1 = rollback journal)
check ok: the file records rollback mode in header bytes 18 and 19 (value 1)
after PRAGMA journal_mode=wal: header bytes 18,19 = 2,2; a fresh connection reports journal_mode=wal
check ok: switching to WAL rewrites bytes 18 and 19 to 2, and the mode persists in the file
CREATE TABLE (driver defaults, first write): -journal [open plain.db-journal pwrite64 plain.db-journal off=0 len=512 sync plain.db-journal pwrite64 plain.db-journal off=0 len=12 sync plain.db-journal unlink plain.db-journal]; -wal writes 0
PRAGMA journal_mode=wal on the new empty file: -journal [open switch.db-journal pwrite64 switch.db-journal off=0 len=512 sync switch.db-journal pwrite64 switch.db-journal off=0 len=12 sync switch.db-journal unlink switch.db-journal]; -wal writes 0
CREATE TABLE after the switch: -journal []; -wal writes 5
sqlitex.Open of a new file with journal_mode=wal: -journal [open platform.db-journal pwrite64 platform.db-journal off=0 len=512 sync platform.db-journal pwrite64 platform.db-journal off=0 len=12 sync platform.db-journal unlink platform.db-journal]; -wal writes 5
CREATE TABLE after it: -journal []; -wal writes 4
check ok: a file created with driver defaults commits its first transaction through a -journal (opened, written, synced, unlinked)
check ok: the switch to WAL is itself a rollback-mode commit: it writes page 1 through a -journal
check ok: after the switch, commits go to the -wal and no -journal is opened
check ok: a WAL database created by the platform opener (sqlitex.Open) is also born through one -journal commit, then never uses one

== B. the recipe step ==
recipe: seed=1 steps=30 journal_mode=delete
step 3: schema=0 inserts=53 updates=5 deletes=5 indexes=0
pre-step state (after step 2): 29 pages, sha256 b8a0bd0796442099, freelist leaves [22]
post-step state (after step 3): 39 pages, sha256 0ef90bd0e64af1e3
check ok: the pre-step and post-step files equal the recipe's hashes for steps 2 and 3

== C. the journal of an open transaction, read before COMMIT ==
every statement of step 3 executed, COMMIT not yet issued: -journal 29240 bytes, first byte 0x00 (not hot), header not-yet-synced (magic and record count still zero), initial size 29 pages, sector 512, page 4096, 7 complete record(s) [2 1 20 3 27 4 26], 0 checksum failure(s)
  record at offset    512: page   2 (b-tree or overflow page), checksum ok, image equals the pre-step page
  record at offset   4616: page   1 (header and schema root), checksum ok, image equals the pre-step page
  record at offset   8720: page  20 (b-tree or overflow page), checksum ok, image equals the pre-step page
  record at offset  12824: page   3 (b-tree or overflow page), checksum ok, image equals the pre-step page
  record at offset  16928: page  27 (b-tree or overflow page), checksum ok, image equals the pre-step page
  record at offset  21032: page   4 (b-tree or overflow page), checksum ok, image equals the pre-step page
  record at offset  25136: page  26 (b-tree or overflow page), checksum ok, image equals the pre-step page
check ok: every journal record holds the page's original (pre-step) image with a valid checksum
check ok: before COMMIT (synchronous=FULL) the header's magic and record count are still zero, so the journal is not hot yet
check ok: the header records the database size before the transaction (29 pages)
pre-step freelist leaf pages (never journaled when reused): [22]
after ROLLBACK and close: pre-step exact bytes
check ok: a ROLLBACK by the live connection restores the pre-step bytes exactly (the reused freelist pages were never written to the file)
check ok: no -journal remains after ROLLBACK and close

== D. step 3, synchronous=FULL: one commit traced, then killed at every write and sync ==
the commit's syscalls on the database directory (writer thread, between the step's start and its return):
    1 other            fcntl t.db F_SETLK F_RDLCK start=1073741824 len=1
    2 other            fcntl t.db F_SETLK F_RDLCK start=1073741826 len=510
    3 other            fcntl t.db F_SETLK F_UNLCK start=1073741824 len=1
    4 other            pread64 t.db off=24 len=16
    5 other            fcntl t.db F_SETLK F_WRLCK start=1073741825 len=1
    6 other            pread64 t.db off=4096 len=4096
    7 other            open t.db-journal
    8 other            fcntl t.db-journal F_SETFD
    9 journal-header   pwrite64 t.db-journal off=0 len=512
   10 journal-record   pwrite64 t.db-journal off=512 len=4
   11 journal-record   pwrite64 t.db-journal off=516 len=4096
   12 journal-record   pwrite64 t.db-journal off=4612 len=4
   13 other            pread64 t.db off=8192 len=4096
   14 other            pread64 t.db off=98304 len=4096
   15 journal-record   pwrite64 t.db-journal off=4616 len=4
   16 journal-record   pwrite64 t.db-journal off=4620 len=4096
   17 journal-record   pwrite64 t.db-journal off=8716 len=4
   18 other            pread64 t.db off=77824 len=4096
   19 journal-record   pwrite64 t.db-journal off=8720 len=4
   20 journal-record   pwrite64 t.db-journal off=8724 len=4096
   21 journal-record   pwrite64 t.db-journal off=12820 len=4
   22 journal-record   pwrite64 t.db-journal off=12824 len=4
   23 journal-record   pwrite64 t.db-journal off=12828 len=4096
   24 journal-record   pwrite64 t.db-journal off=16924 len=4
   25 other            pread64 t.db off=12288 len=4096
   26 other            pread64 t.db off=106496 len=4096
   27 journal-record   pwrite64 t.db-journal off=16928 len=4
   28 journal-record   pwrite64 t.db-journal off=16932 len=4096
   29 journal-record   pwrite64 t.db-journal off=21028 len=4
   30 journal-record   pwrite64 t.db-journal off=21032 len=4
   31 journal-record   pwrite64 t.db-journal off=21036 len=4096
   32 journal-record   pwrite64 t.db-journal off=25132 len=4
   33 other            pread64 t.db off=102400 len=4096
   34 journal-record   pwrite64 t.db-journal off=25136 len=4
   35 journal-record   pwrite64 t.db-journal off=25140 len=4096
   36 journal-record   pwrite64 t.db-journal off=29236 len=4
   37 other            fcntl t.db F_SETLK F_WRLCK start=1073741824 len=1
   38 other            fcntl t.db F_SETLK F_WRLCK start=1073741826 len=510
   39 other            pread64 t.db-journal off=29696 len=8 = 0
   40 journal-sync     sync t.db-journal
   41 other            open .
   42 other            fcntl . F_SETFD
   43 dir-sync         sync .
   44 other            close .
   45 journal-count    pwrite64 t.db-journal off=0 len=12
   46 journal-sync     sync t.db-journal
   47 db-page          pwrite64 t.db off=0 len=4096
   48 db-page          pwrite64 t.db off=4096 len=4096
   49 db-page          pwrite64 t.db off=8192 len=4096
   50 db-page          pwrite64 t.db off=12288 len=4096
   51 db-page          pwrite64 t.db off=77824 len=4096
   52 db-page          pwrite64 t.db off=86016 len=4096
   53 db-page          pwrite64 t.db off=102400 len=4096
   54 db-page          pwrite64 t.db off=106496 len=4096
   55 db-page          pwrite64 t.db off=118784 len=4096
   56 db-page          pwrite64 t.db off=122880 len=4096
   57 db-page          pwrite64 t.db off=126976 len=4096
   58 db-page          pwrite64 t.db off=131072 len=4096
   59 db-page          pwrite64 t.db off=135168 len=4096
   60 db-page          pwrite64 t.db off=139264 len=4096
   61 db-page          pwrite64 t.db off=143360 len=4096
   62 db-page          pwrite64 t.db off=147456 len=4096
   63 db-page          pwrite64 t.db off=151552 len=4096
   64 db-page          pwrite64 t.db off=155648 len=4096
   65 db-sync          sync t.db
   66 other            close t.db-journal
   67 journal-unlink   unlink t.db-journal
   68 other            fcntl t.db F_SETLK F_RDLCK start=1073741826 len=510
   69 other            fcntl t.db F_SETLK F_UNLCK start=1073741824 len=2
   70 other            fcntl t.db F_SETLK F_UNLCK start=0 len=0
sequence: journal-header, journal-record x21, journal-sync, dir-sync, journal-count, journal-sync, db-page x18, db-sync, journal-unlink
journaled pages 7 (21 record writes), database page writes 18, syncs 4 [t.db-journal . t.db-journal t.db]
INFO: sync syscall used by the Go driver: fsync
check ok: step 3, journal_mode=delete, synchronous=FULL: order is journal-header, journal-record x21, journal-sync, dir-sync, journal-count, journal-sync, db-page x18, db-sync, journal-unlink
check ok: step 3, journal_mode=delete, synchronous=FULL: 4 syncs per commit (journal, directory and database syncs counted)
check ok: step 3, journal_mode=delete, synchronous=FULL: each journal record is three pwrite64 calls: page number (4 bytes), original page image (4096), checksum (4)
check ok: step 3, journal_mode=delete, synchronous=FULL: the traced commit leaves exactly the post-step bytes
twin check: the C CLI running the same step as literal SQL makes 46 writes, syncs and unlinks, identical to the Go driver's
INFO: CLI sync syscall: fsync
check ok: synchronous=FULL: the C CLI makes the same writes, syncs and unlink in the same order and leaves the post-step bytes
kill points: 47 (every pwrite64, sync and unlink of the commit, then the first syscall after the step returned)
INFO: 47 kill runs took 2.814s with 4 workers
kill 01 on entry to pwrite64 t.db-journal off=0 len=512    [journal-header]
    on disk:  database pre-step bytes
              -journal 0 bytes (not hot)
              SQLite on reopen ignores it (not hot)
    reopened: pre-step, exact bytes (recipe step 2); integrity_check ok; cold -journal left (0 bytes, first byte 0x00)
kill 02 on entry to pwrite64 t.db-journal off=512 len=4    [journal-record]
    on disk:  database pre-step bytes
              -journal 512 bytes, first byte 0x00 (not hot), header not-yet-synced (magic and record count still zero), initial size 29 pages, sector 512, page 4096, 0 complete record(s) [], 0 checksum failure(s)
              SQLite on reopen ignores it (not hot)
    reopened: pre-step, exact bytes (recipe step 2); integrity_check ok; cold -journal left (512 bytes, first byte 0x00)
INFO: step 3 FULL kill 02 journal nonce 0xe0f43ab2, run 194ms
kill 03 on entry to pwrite64 t.db-journal off=516 len=4096 [journal-record]
    on disk:  database pre-step bytes
              -journal 516 bytes, first byte 0x00 (not hot), header not-yet-synced (magic and record count still zero), initial size 29 pages, sector 512, page 4096, 0 complete record(s) [], 0 checksum failure(s), 4 byte(s) of an incomplete record
              SQLite on reopen ignores it (not hot)
    reopened: pre-step, exact bytes (recipe step 2); integrity_check ok; cold -journal left (516 bytes, first byte 0x00)
INFO: step 3 FULL kill 03 journal nonce 0xefbe53c2, run 136ms
kill 04 on entry to pwrite64 t.db-journal off=4612 len=4   [journal-record]
    on disk:  database pre-step bytes
              -journal 4612 bytes, first byte 0x00 (not hot), header not-yet-synced (magic and record count still zero), initial size 29 pages, sector 512, page 4096, 0 complete record(s) [], 0 checksum failure(s), 4100 byte(s) of an incomplete record
              SQLite on reopen ignores it (not hot)
    reopened: pre-step, exact bytes (recipe step 2); integrity_check ok; cold -journal left (4612 bytes, first byte 0x00)
INFO: step 3 FULL kill 04 journal nonce 0xccf3e01f, run 132ms
kill 05 on entry to pwrite64 t.db-journal off=4616 len=4   [journal-record]
    on disk:  database pre-step bytes
              -journal 4616 bytes, first byte 0x00 (not hot), header not-yet-synced (magic and record count still zero), initial size 29 pages, sector 512, page 4096, 1 complete record(s) [2], 0 checksum failure(s)
              SQLite on reopen ignores it (not hot)
    reopened: pre-step, exact bytes (recipe step 2); integrity_check ok; cold -journal left (4616 bytes, first byte 0x00)
INFO: step 3 FULL kill 05 journal nonce 0xb99c46ea, run 113ms
kill 06 on entry to pwrite64 t.db-journal off=4620 len=4096 [journal-record]
    on disk:  database pre-step bytes
              -journal 4620 bytes, first byte 0x00 (not hot), header not-yet-synced (magic and record count still zero), initial size 29 pages, sector 512, page 4096, 1 complete record(s) [2], 0 checksum failure(s), 4 byte(s) of an incomplete record
              SQLite on reopen ignores it (not hot)
    reopened: pre-step, exact bytes (recipe step 2); integrity_check ok; cold -journal left (4620 bytes, first byte 0x00)
INFO: step 3 FULL kill 06 journal nonce 0xb6e389ff, run 233ms
kill 07 on entry to pwrite64 t.db-journal off=8716 len=4   [journal-record]
    on disk:  database pre-step bytes
              -journal 8716 bytes, first byte 0x00 (not hot), header not-yet-synced (magic and record count still zero), initial size 29 pages, sector 512, page 4096, 1 complete record(s) [2], 0 checksum failure(s), 4100 byte(s) of an incomplete record
              SQLite on reopen ignores it (not hot)
    reopened: pre-step, exact bytes (recipe step 2); integrity_check ok; cold -journal left (8716 bytes, first byte 0x00)
INFO: step 3 FULL kill 07 journal nonce 0x91a49b26, run 156ms
kill 08 on entry to pwrite64 t.db-journal off=8720 len=4   [journal-record]
    on disk:  database pre-step bytes
              -journal 8720 bytes, first byte 0x00 (not hot), header not-yet-synced (magic and record count still zero), initial size 29 pages, sector 512, page 4096, 2 complete record(s) [2 1], 0 checksum failure(s)
              SQLite on reopen ignores it (not hot)
    reopened: pre-step, exact bytes (recipe step 2); integrity_check ok; cold -journal left (8720 bytes, first byte 0x00)
INFO: step 3 FULL kill 08 journal nonce 0xa4f036a1, run 139ms
kill 09 on entry to pwrite64 t.db-journal off=8724 len=4096 [journal-record]
    on disk:  database pre-step bytes
              -journal 8724 bytes, first byte 0x00 (not hot), header not-yet-synced (magic and record count still zero), initial size 29 pages, sector 512, page 4096, 2 complete record(s) [2 1], 0 checksum failure(s), 4 byte(s) of an incomplete record
              SQLite on reopen ignores it (not hot)
    reopened: pre-step, exact bytes (recipe step 2); integrity_check ok; cold -journal left (8724 bytes, first byte 0x00)
INFO: step 3 FULL kill 09 journal nonce 0x14acca1c, run 183ms
kill 10 on entry to pwrite64 t.db-journal off=12820 len=4  [journal-record]
    on disk:  database pre-step bytes
              -journal 12820 bytes, first byte 0x00 (not hot), header not-yet-synced (magic and record count still zero), initial size 29 pages, sector 512, page 4096, 2 complete record(s) [2 1], 0 checksum failure(s), 4100 byte(s) of an incomplete record
              SQLite on reopen ignores it (not hot)
    reopened: pre-step, exact bytes (recipe step 2); integrity_check ok; cold -journal left (12820 bytes, first byte 0x00)
INFO: step 3 FULL kill 10 journal nonce 0x6cf94ba7, run 152ms
kill 11 on entry to pwrite64 t.db-journal off=12824 len=4  [journal-record]
    on disk:  database pre-step bytes
              -journal 12824 bytes, first byte 0x00 (not hot), header not-yet-synced (magic and record count still zero), initial size 29 pages, sector 512, page 4096, 3 complete record(s) [2 1 20], 0 checksum failure(s)
              SQLite on reopen ignores it (not hot)
    reopened: pre-step, exact bytes (recipe step 2); integrity_check ok; cold -journal left (12824 bytes, first byte 0x00)
INFO: step 3 FULL kill 11 journal nonce 0x4a66efe2, run 187ms
kill 12 on entry to pwrite64 t.db-journal off=12828 len=4096 [journal-record]
    on disk:  database pre-step bytes
              -journal 12828 bytes, first byte 0x00 (not hot), header not-yet-synced (magic and record count still zero), initial size 29 pages, sector 512, page 4096, 3 complete record(s) [2 1 20], 0 checksum failure(s), 4 byte(s) of an incomplete record
              SQLite on reopen ignores it (not hot)
    reopened: pre-step, exact bytes (recipe step 2); integrity_check ok; cold -journal left (12828 bytes, first byte 0x00)
INFO: step 3 FULL kill 12 journal nonce 0xd2fb7f7e, run 229ms
kill 13 on entry to pwrite64 t.db-journal off=16924 len=4  [journal-record]
    on disk:  database pre-step bytes
              -journal 16924 bytes, first byte 0x00 (not hot), header not-yet-synced (magic and record count still zero), initial size 29 pages, sector 512, page 4096, 3 complete record(s) [2 1 20], 0 checksum failure(s), 4100 byte(s) of an incomplete record
              SQLite on reopen ignores it (not hot)
    reopened: pre-step, exact bytes (recipe step 2); integrity_check ok; cold -journal left (16924 bytes, first byte 0x00)
INFO: step 3 FULL kill 13 journal nonce 0x4ea3f1cc, run 162ms
kill 14 on entry to pwrite64 t.db-journal off=16928 len=4  [journal-record]
    on disk:  database pre-step bytes
              -journal 16928 bytes, first byte 0x00 (not hot), header not-yet-synced (magic and record count still zero), initial size 29 pages, sector 512, page 4096, 4 complete record(s) [2 1 20 3], 0 checksum failure(s)
              SQLite on reopen ignores it (not hot)
    reopened: pre-step, exact bytes (recipe step 2); integrity_check ok; cold -journal left (16928 bytes, first byte 0x00)
INFO: step 3 FULL kill 14 journal nonce 0x47964e95, run 264ms
kill 15 on entry to pwrite64 t.db-journal off=16932 len=4096 [journal-record]
    on disk:  database pre-step bytes
              -journal 16932 bytes, first byte 0x00 (not hot), header not-yet-synced (magic and record count still zero), initial size 29 pages, sector 512, page 4096, 4 complete record(s) [2 1 20 3], 0 checksum failure(s), 4 byte(s) of an incomplete record
              SQLite on reopen ignores it (not hot)
    reopened: pre-step, exact bytes (recipe step 2); integrity_check ok; cold -journal left (16932 bytes, first byte 0x00)
INFO: step 3 FULL kill 15 journal nonce 0x5d95d9ef, run 162ms
kill 16 on entry to pwrite64 t.db-journal off=21028 len=4  [journal-record]
    on disk:  database pre-step bytes
              -journal 21028 bytes, first byte 0x00 (not hot), header not-yet-synced (magic and record count still zero), initial size 29 pages, sector 512, page 4096, 4 complete record(s) [2 1 20 3], 0 checksum failure(s), 4100 byte(s) of an incomplete record
              SQLite on reopen ignores it (not hot)
    reopened: pre-step, exact bytes (recipe step 2); integrity_check ok; cold -journal left (21028 bytes, first byte 0x00)
INFO: step 3 FULL kill 16 journal nonce 0x27ead1e3, run 104ms
kill 17 on entry to pwrite64 t.db-journal off=21032 len=4  [journal-record]
    on disk:  database pre-step bytes
              -journal 21032 bytes, first byte 0x00 (not hot), header not-yet-synced (magic and record count still zero), initial size 29 pages, sector 512, page 4096, 5 complete record(s) [2 1 20 3 27], 0 checksum failure(s)
              SQLite on reopen ignores it (not hot)
    reopened: pre-step, exact bytes (recipe step 2); integrity_check ok; cold -journal left (21032 bytes, first byte 0x00)
INFO: step 3 FULL kill 17 journal nonce 0x22351c50, run 109ms
kill 18 on entry to pwrite64 t.db-journal off=21036 len=4096 [journal-record]
    on disk:  database pre-step bytes
              -journal 21036 bytes, first byte 0x00 (not hot), header not-yet-synced (magic and record count still zero), initial size 29 pages, sector 512, page 4096, 5 complete record(s) [2 1 20 3 27], 0 checksum failure(s), 4 byte(s) of an incomplete record
              SQLite on reopen ignores it (not hot)
    reopened: pre-step, exact bytes (recipe step 2); integrity_check ok; cold -journal left (21036 bytes, first byte 0x00)
INFO: step 3 FULL kill 18 journal nonce 0x9f14b3de, run 60ms
kill 19 on entry to pwrite64 t.db-journal off=25132 len=4  [journal-record]
    on disk:  database pre-step bytes
              -journal 25132 bytes, first byte 0x00 (not hot), header not-yet-synced (magic and record count still zero), initial size 29 pages, sector 512, page 4096, 5 complete record(s) [2 1 20 3 27], 0 checksum failure(s), 4100 byte(s) of an incomplete record
              SQLite on reopen ignores it (not hot)
    reopened: pre-step, exact bytes (recipe step 2); integrity_check ok; cold -journal left (25132 bytes, first byte 0x00)
INFO: step 3 FULL kill 19 journal nonce 0x167388b7, run 298ms
kill 20 on entry to pwrite64 t.db-journal off=25136 len=4  [journal-record]
    on disk:  database pre-step bytes
              -journal 25136 bytes, first byte 0x00 (not hot), header not-yet-synced (magic and record count still zero), initial size 29 pages, sector 512, page 4096, 6 complete record(s) [2 1 20 3 27 4], 0 checksum failure(s)
              SQLite on reopen ignores it (not hot)
    reopened: pre-step, exact bytes (recipe step 2); integrity_check ok; cold -journal left (25136 bytes, first byte 0x00)
INFO: step 3 FULL kill 20 journal nonce 0xd22af796, run 196ms
kill 21 on entry to pwrite64 t.db-journal off=25140 len=4096 [journal-record]
    on disk:  database pre-step bytes
              -journal 25140 bytes, first byte 0x00 (not hot), header not-yet-synced (magic and record count still zero), initial size 29 pages, sector 512, page 4096, 6 complete record(s) [2 1 20 3 27 4], 0 checksum failure(s), 4 byte(s) of an incomplete record
              SQLite on reopen ignores it (not hot)
    reopened: pre-step, exact bytes (recipe step 2); integrity_check ok; cold -journal left (25140 bytes, first byte 0x00)
INFO: step 3 FULL kill 21 journal nonce 0x4b1efede, run 397ms
kill 22 on entry to pwrite64 t.db-journal off=29236 len=4  [journal-record]
    on disk:  database pre-step bytes
              -journal 29236 bytes, first byte 0x00 (not hot), header not-yet-synced (magic and record count still zero), initial size 29 pages, sector 512, page 4096, 6 complete record(s) [2 1 20 3 27 4], 0 checksum failure(s), 4100 byte(s) of an incomplete record
              SQLite on reopen ignores it (not hot)
    reopened: pre-step, exact bytes (recipe step 2); integrity_check ok; cold -journal left (29236 bytes, first byte 0x00)
INFO: step 3 FULL kill 22 journal nonce 0xb21e17f6, run 191ms
kill 23 on entry to sync t.db-journal                      [journal-sync]
    on disk:  database pre-step bytes
              -journal 29240 bytes, first byte 0x00 (not hot), header not-yet-synced (magic and record count still zero), initial size 29 pages, sector 512, page 4096, 7 complete record(s) [2 1 20 3 27 4 26], 0 checksum failure(s)
              SQLite on reopen ignores it (not hot)
    reopened: pre-step, exact bytes (recipe step 2); integrity_check ok; cold -journal left (29240 bytes, first byte 0x00)
INFO: step 3 FULL kill 23 journal nonce 0x5cf5a80c, run 125ms
kill 24 on entry to sync .                                 [dir-sync]
    on disk:  database pre-step bytes
              -journal 29240 bytes, first byte 0x00 (not hot), header not-yet-synced (magic and record count still zero), initial size 29 pages, sector 512, page 4096, 7 complete record(s) [2 1 20 3 27 4 26], 0 checksum failure(s)
              SQLite on reopen ignores it (not hot)
    reopened: pre-step, exact bytes (recipe step 2); integrity_check ok; cold -journal left (29240 bytes, first byte 0x00)
INFO: step 3 FULL kill 24 journal nonce 0xc7de1ee0, run 156ms
kill 25 on entry to pwrite64 t.db-journal off=0 len=12     [journal-count]
    on disk:  database pre-step bytes
              -journal 29240 bytes, first byte 0x00 (not hot), header not-yet-synced (magic and record count still zero), initial size 29 pages, sector 512, page 4096, 7 complete record(s) [2 1 20 3 27 4 26], 0 checksum failure(s)
              SQLite on reopen ignores it (not hot)
    reopened: pre-step, exact bytes (recipe step 2); integrity_check ok; cold -journal left (29240 bytes, first byte 0x00)
INFO: step 3 FULL kill 25 journal nonce 0xbe83245a, run 70ms
kill 26 on entry to sync t.db-journal                      [journal-sync]
    on disk:  database pre-step bytes
              -journal 29240 bytes, first byte 0xd9 (hot), header synced (record count 7), initial size 29 pages, sector 512, page 4096, 7 complete record(s) [2 1 20 3 27 4 26], 0 checksum failure(s)
              SQLite on reopen rolls back: writes pages [2 1 20 3 27 4 26] from the journal, truncates to 29 pages
    reopened: pre-step, exact bytes (recipe step 2); integrity_check ok; no -journal
INFO: step 3 FULL kill 26 journal nonce 0xcbdbbb84, run 130ms
kill 27 on entry to pwrite64 t.db off=0 len=4096           [db-page]
    on disk:  database pre-step bytes
              -journal 29240 bytes, first byte 0xd9 (hot), header synced (record count 7), initial size 29 pages, sector 512, page 4096, 7 complete record(s) [2 1 20 3 27 4 26], 0 checksum failure(s)
              SQLite on reopen rolls back: writes pages [2 1 20 3 27 4 26] from the journal, truncates to 29 pages
    reopened: pre-step, exact bytes (recipe step 2); integrity_check ok; no -journal
INFO: step 3 FULL kill 27 journal nonce 0xf1f366d6, run 117ms
kill 28 on entry to pwrite64 t.db off=4096 len=4096        [db-page]
    on disk:  database torn: 29 pages, 1 differ from pre-step, 17 differ from post-step
              -journal 29240 bytes, first byte 0xd9 (hot), header synced (record count 7), initial size 29 pages, sector 512, page 4096, 7 complete record(s) [2 1 20 3 27 4 26], 0 checksum failure(s)
              SQLite on reopen rolls back: writes pages [2 1 20 3 27 4 26] from the journal, truncates to 29 pages
    reopened: pre-step, exact bytes (recipe step 2); integrity_check ok; no -journal
INFO: step 3 FULL kill 28 journal nonce 0xa966d865, run 140ms
kill 29 on entry to pwrite64 t.db off=8192 len=4096        [db-page]
    on disk:  database torn: 29 pages, 2 differ from pre-step, 16 differ from post-step
              -journal 29240 bytes, first byte 0xd9 (hot), header synced (record count 7), initial size 29 pages, sector 512, page 4096, 7 complete record(s) [2 1 20 3 27 4 26], 0 checksum failure(s)
              SQLite on reopen rolls back: writes pages [2 1 20 3 27 4 26] from the journal, truncates to 29 pages
    reopened: pre-step, exact bytes (recipe step 2); integrity_check ok; no -journal
INFO: step 3 FULL kill 29 journal nonce 0x43944b46, run 268ms
kill 30 on entry to pwrite64 t.db off=12288 len=4096       [db-page]
    on disk:  database torn: 29 pages, 3 differ from pre-step, 15 differ from post-step
              -journal 29240 bytes, first byte 0xd9 (hot), header synced (record count 7), initial size 29 pages, sector 512, page 4096, 7 complete record(s) [2 1 20 3 27 4 26], 0 checksum failure(s)
              SQLite on reopen rolls back: writes pages [2 1 20 3 27 4 26] from the journal, truncates to 29 pages
    reopened: pre-step, exact bytes (recipe step 2); integrity_check ok; no -journal
INFO: step 3 FULL kill 30 journal nonce 0xeffa4cda, run 211ms
kill 31 on entry to pwrite64 t.db off=77824 len=4096       [db-page]
    on disk:  database torn: 29 pages, 4 differ from pre-step, 14 differ from post-step
              -journal 29240 bytes, first byte 0xd9 (hot), header synced (record count 7), initial size 29 pages, sector 512, page 4096, 7 complete record(s) [2 1 20 3 27 4 26], 0 checksum failure(s)
              SQLite on reopen rolls back: writes pages [2 1 20 3 27 4 26] from the journal, truncates to 29 pages
    reopened: pre-step, exact bytes (recipe step 2); integrity_check ok; no -journal
INFO: step 3 FULL kill 31 journal nonce 0x59ea3a01, run 82ms
kill 32 on entry to pwrite64 t.db off=86016 len=4096       [db-page]
    on disk:  database torn: 29 pages, 5 differ from pre-step, 13 differ from post-step
              -journal 29240 bytes, first byte 0xd9 (hot), header synced (record count 7), initial size 29 pages, sector 512, page 4096, 7 complete record(s) [2 1 20 3 27 4 26], 0 checksum failure(s)
              SQLite on reopen rolls back: writes pages [2 1 20 3 27 4 26] from the journal, truncates to 29 pages
    reopened: pre-step, exact bytes (recipe step 2); integrity_check ok; no -journal
INFO: step 3 FULL kill 32 journal nonce 0x174a2f5b, run 242ms
kill 33 on entry to pwrite64 t.db off=102400 len=4096      [db-page]
    on disk:  database torn: 29 pages, 6 differ from pre-step, 12 differ from post-step
              -journal 29240 bytes, first byte 0xd9 (hot), header synced (record count 7), initial size 29 pages, sector 512, page 4096, 7 complete record(s) [2 1 20 3 27 4 26], 0 checksum failure(s)
              SQLite on reopen rolls back: writes pages [2 1 20 3 27 4 26] from the journal, truncates to 29 pages
    reopened: pre-step except page(s) [22], which are freelist leaves in the pre-step file (unused bytes); VACUUM INTO output equals the pre-step's; integrity_check ok; no -journal
INFO: step 3 FULL kill 33 journal nonce 0xd6f07aec, run 166ms
kill 34 on entry to pwrite64 t.db off=106496 len=4096      [db-page]
    on disk:  database torn: 29 pages, 7 differ from pre-step, 11 differ from post-step
              -journal 29240 bytes, first byte 0xd9 (hot), header synced (record count 7), initial size 29 pages, sector 512, page 4096, 7 complete record(s) [2 1 20 3 27 4 26], 0 checksum failure(s)
              SQLite on reopen rolls back: writes pages [2 1 20 3 27 4 26] from the journal, truncates to 29 pages
    reopened: pre-step except page(s) [22], which are freelist leaves in the pre-step file (unused bytes); VACUUM INTO output equals the pre-step's; integrity_check ok; no -journal
INFO: step 3 FULL kill 34 journal nonce 0x03e146fa, run 182ms
kill 35 on entry to pwrite64 t.db off=118784 len=4096      [db-page]
    on disk:  database torn: 29 pages, 8 differ from pre-step, 10 differ from post-step
              -journal 29240 bytes, first byte 0xd9 (hot), header synced (record count 7), initial size 29 pages, sector 512, page 4096, 7 complete record(s) [2 1 20 3 27 4 26], 0 checksum failure(s)
              SQLite on reopen rolls back: writes pages [2 1 20 3 27 4 26] from the journal, truncates to 29 pages
    reopened: pre-step except page(s) [22], which are freelist leaves in the pre-step file (unused bytes); VACUUM INTO output equals the pre-step's; integrity_check ok; no -journal
INFO: step 3 FULL kill 35 journal nonce 0xda00d7a1, run 165ms
kill 36 on entry to pwrite64 t.db off=122880 len=4096      [db-page]
    on disk:  database torn: 30 pages, 9 differ from pre-step, 9 differ from post-step
              -journal 29240 bytes, first byte 0xd9 (hot), header synced (record count 7), initial size 29 pages, sector 512, page 4096, 7 complete record(s) [2 1 20 3 27 4 26], 0 checksum failure(s)
              SQLite on reopen rolls back: writes pages [2 1 20 3 27 4 26] from the journal, truncates to 29 pages
    reopened: pre-step except page(s) [22], which are freelist leaves in the pre-step file (unused bytes); VACUUM INTO output equals the pre-step's; integrity_check ok; no -journal
INFO: step 3 FULL kill 36 journal nonce 0x557c44a3, run 169ms
kill 37 on entry to pwrite64 t.db off=126976 len=4096      [db-page]
    on disk:  database torn: 31 pages, 10 differ from pre-step, 8 differ from post-step
              -journal 29240 bytes, first byte 0xd9 (hot), header synced (record count 7), initial size 29 pages, sector 512, page 4096, 7 complete record(s) [2 1 20 3 27 4 26], 0 checksum failure(s)
              SQLite on reopen rolls back: writes pages [2 1 20 3 27 4 26] from the journal, truncates to 29 pages
    reopened: pre-step except page(s) [22], which are freelist leaves in the pre-step file (unused bytes); VACUUM INTO output equals the pre-step's; integrity_check ok; no -journal
INFO: step 3 FULL kill 37 journal nonce 0x690c04f0, run 274ms
kill 38 on entry to pwrite64 t.db off=131072 len=4096      [db-page]
    on disk:  database torn: 32 pages, 11 differ from pre-step, 7 differ from post-step
              -journal 29240 bytes, first byte 0xd9 (hot), header synced (record count 7), initial size 29 pages, sector 512, page 4096, 7 complete record(s) [2 1 20 3 27 4 26], 0 checksum failure(s)
              SQLite on reopen rolls back: writes pages [2 1 20 3 27 4 26] from the journal, truncates to 29 pages
    reopened: pre-step except page(s) [22], which are freelist leaves in the pre-step file (unused bytes); VACUUM INTO output equals the pre-step's; integrity_check ok; no -journal
INFO: step 3 FULL kill 38 journal nonce 0x252769fa, run 137ms
kill 39 on entry to pwrite64 t.db off=135168 len=4096      [db-page]
    on disk:  database torn: 33 pages, 12 differ from pre-step, 6 differ from post-step
              -journal 29240 bytes, first byte 0xd9 (hot), header synced (record count 7), initial size 29 pages, sector 512, page 4096, 7 complete record(s) [2 1 20 3 27 4 26], 0 checksum failure(s)
              SQLite on reopen rolls back: writes pages [2 1 20 3 27 4 26] from the journal, truncates to 29 pages
    reopened: pre-step except page(s) [22], which are freelist leaves in the pre-step file (unused bytes); VACUUM INTO output equals the pre-step's; integrity_check ok; no -journal
INFO: step 3 FULL kill 39 journal nonce 0x99d792fa, run 192ms
kill 40 on entry to pwrite64 t.db off=139264 len=4096      [db-page]
    on disk:  database torn: 34 pages, 13 differ from pre-step, 5 differ from post-step
              -journal 29240 bytes, first byte 0xd9 (hot), header synced (record count 7), initial size 29 pages, sector 512, page 4096, 7 complete record(s) [2 1 20 3 27 4 26], 0 checksum failure(s)
              SQLite on reopen rolls back: writes pages [2 1 20 3 27 4 26] from the journal, truncates to 29 pages
    reopened: pre-step except page(s) [22], which are freelist leaves in the pre-step file (unused bytes); VACUUM INTO output equals the pre-step's; integrity_check ok; no -journal
INFO: step 3 FULL kill 40 journal nonce 0xca91e2d4, run 202ms
kill 41 on entry to pwrite64 t.db off=143360 len=4096      [db-page]
    on disk:  database torn: 35 pages, 14 differ from pre-step, 4 differ from post-step
              -journal 29240 bytes, first byte 0xd9 (hot), header synced (record count 7), initial size 29 pages, sector 512, page 4096, 7 complete record(s) [2 1 20 3 27 4 26], 0 checksum failure(s)
              SQLite on reopen rolls back: writes pages [2 1 20 3 27 4 26] from the journal, truncates to 29 pages
    reopened: pre-step except page(s) [22], which are freelist leaves in the pre-step file (unused bytes); VACUUM INTO output equals the pre-step's; integrity_check ok; no -journal
INFO: step 3 FULL kill 41 journal nonce 0xc21473df, run 168ms
kill 42 on entry to pwrite64 t.db off=147456 len=4096      [db-page]
    on disk:  database torn: 36 pages, 15 differ from pre-step, 3 differ from post-step
              -journal 29240 bytes, first byte 0xd9 (hot), header synced (record count 7), initial size 29 pages, sector 512, page 4096, 7 complete record(s) [2 1 20 3 27 4 26], 0 checksum failure(s)
              SQLite on reopen rolls back: writes pages [2 1 20 3 27 4 26] from the journal, truncates to 29 pages
    reopened: pre-step except page(s) [22], which are freelist leaves in the pre-step file (unused bytes); VACUUM INTO output equals the pre-step's; integrity_check ok; no -journal
INFO: step 3 FULL kill 42 journal nonce 0x387ac55b, run 204ms
kill 43 on entry to pwrite64 t.db off=151552 len=4096      [db-page]
    on disk:  database torn: 37 pages, 16 differ from pre-step, 2 differ from post-step
              -journal 29240 bytes, first byte 0xd9 (hot), header synced (record count 7), initial size 29 pages, sector 512, page 4096, 7 complete record(s) [2 1 20 3 27 4 26], 0 checksum failure(s)
              SQLite on reopen rolls back: writes pages [2 1 20 3 27 4 26] from the journal, truncates to 29 pages
    reopened: pre-step except page(s) [22], which are freelist leaves in the pre-step file (unused bytes); VACUUM INTO output equals the pre-step's; integrity_check ok; no -journal
INFO: step 3 FULL kill 43 journal nonce 0x1ca9acf4, run 115ms
kill 44 on entry to pwrite64 t.db off=155648 len=4096      [db-page]
    on disk:  database torn: 38 pages, 17 differ from pre-step, 1 differ from post-step
              -journal 29240 bytes, first byte 0xd9 (hot), header synced (record count 7), initial size 29 pages, sector 512, page 4096, 7 complete record(s) [2 1 20 3 27 4 26], 0 checksum failure(s)
              SQLite on reopen rolls back: writes pages [2 1 20 3 27 4 26] from the journal, truncates to 29 pages
    reopened: pre-step except page(s) [22], which are freelist leaves in the pre-step file (unused bytes); VACUUM INTO output equals the pre-step's; integrity_check ok; no -journal
INFO: step 3 FULL kill 44 journal nonce 0x4f892f8a, run 208ms
kill 45 on entry to sync t.db                              [db-sync]
    on disk:  database post-step bytes
              -journal 29240 bytes, first byte 0xd9 (hot), header synced (record count 7), initial size 29 pages, sector 512, page 4096, 7 complete record(s) [2 1 20 3 27 4 26], 0 checksum failure(s)
              SQLite on reopen rolls back: writes pages [2 1 20 3 27 4 26] from the journal, truncates to 29 pages
    reopened: pre-step except page(s) [22], which are freelist leaves in the pre-step file (unused bytes); VACUUM INTO output equals the pre-step's; integrity_check ok; no -journal
INFO: step 3 FULL kill 45 journal nonce 0xa4801c49, run 241ms
kill 46 on entry to unlink t.db-journal                    [journal-unlink]
    on disk:  database post-step bytes
              -journal 29240 bytes, first byte 0xd9 (hot), header synced (record count 7), initial size 29 pages, sector 512, page 4096, 7 complete record(s) [2 1 20 3 27 4 26], 0 checksum failure(s)
              SQLite on reopen rolls back: writes pages [2 1 20 3 27 4 26] from the journal, truncates to 29 pages
    reopened: pre-step except page(s) [22], which are freelist leaves in the pre-step file (unused bytes); VACUUM INTO output equals the pre-step's; integrity_check ok; no -journal
INFO: step 3 FULL kill 46 journal nonce 0x14c383bc, run 188ms
kill 47 on entry to open marker-end                        [step-returned]
    on disk:  database post-step bytes
              no -journal
              SQLite on reopen nothing to roll back
    reopened: post-step, exact bytes (recipe step 3); integrity_check ok; no -journal
outcomes: 32 pre-step exact, 14 pre-step except reused freelist leaves, 1 post-step exact, 0 other
check ok: step 3, synchronous=FULL: every kill landed on the intended syscall (strace saw it unfinished and the child died of SIGKILL)
check ok: step 3, synchronous=FULL: every reopened database is the pre-step or the post-step state, never a third (pre-step allows reused freelist-leaf pages, see above)
check ok: step 3, synchronous=FULL: every kill up to and including the journal unlink gives pre-step; the kill after it gives post-step (the unlink is the commit point)
check ok: step 3, synchronous=FULL: integrity_check is ok after every reopen
check ok: step 3, synchronous=FULL: for every kill up to the commit point, whenever the database file differs from pre-step before reopen, the journal is hot and its records cover every changed page except pages appended past the original size (removed by truncation) and reused freelist leaves

== D. step 3, synchronous=NORMAL: one commit traced, then killed at every write and sync ==
the commit's syscalls on the database directory (writer thread, between the step's start and its return):
    1 other            fcntl t.db F_SETLK F_RDLCK start=1073741824 len=1
    2 other            fcntl t.db F_SETLK F_RDLCK start=1073741826 len=510
    3 other            fcntl t.db F_SETLK F_UNLCK start=1073741824 len=1
    4 other            pread64 t.db off=24 len=16
    5 other            fcntl t.db F_SETLK F_WRLCK start=1073741825 len=1
    6 other            pread64 t.db off=4096 len=4096
    7 other            open t.db-journal
    8 other            fcntl t.db-journal F_SETFD
    9 journal-header   pwrite64 t.db-journal off=0 len=512
   10 journal-record   pwrite64 t.db-journal off=512 len=4
   11 journal-record   pwrite64 t.db-journal off=516 len=4096
   12 journal-record   pwrite64 t.db-journal off=4612 len=4
   13 other            pread64 t.db off=8192 len=4096
   14 other            pread64 t.db off=98304 len=4096
   15 journal-record   pwrite64 t.db-journal off=4616 len=4
   16 journal-record   pwrite64 t.db-journal off=4620 len=4096
   17 journal-record   pwrite64 t.db-journal off=8716 len=4
   18 other            pread64 t.db off=77824 len=4096
   19 journal-record   pwrite64 t.db-journal off=8720 len=4
   20 journal-record   pwrite64 t.db-journal off=8724 len=4096
   21 journal-record   pwrite64 t.db-journal off=12820 len=4
   22 journal-record   pwrite64 t.db-journal off=12824 len=4
   23 journal-record   pwrite64 t.db-journal off=12828 len=4096
   24 journal-record   pwrite64 t.db-journal off=16924 len=4
   25 other            pread64 t.db off=12288 len=4096
   26 other            pread64 t.db off=106496 len=4096
   27 journal-record   pwrite64 t.db-journal off=16928 len=4
   28 journal-record   pwrite64 t.db-journal off=16932 len=4096
   29 journal-record   pwrite64 t.db-journal off=21028 len=4
   30 journal-record   pwrite64 t.db-journal off=21032 len=4
   31 journal-record   pwrite64 t.db-journal off=21036 len=4096
   32 journal-record   pwrite64 t.db-journal off=25132 len=4
   33 other            pread64 t.db off=102400 len=4096
   34 journal-record   pwrite64 t.db-journal off=25136 len=4
   35 journal-record   pwrite64 t.db-journal off=25140 len=4096
   36 journal-record   pwrite64 t.db-journal off=29236 len=4
   37 other            fcntl t.db F_SETLK F_WRLCK start=1073741824 len=1
   38 other            fcntl t.db F_SETLK F_WRLCK start=1073741826 len=510
   39 other            pread64 t.db-journal off=29696 len=8 = 0
   40 journal-count    pwrite64 t.db-journal off=0 len=12
   41 journal-sync     sync t.db-journal
   42 other            open .
   43 other            fcntl . F_SETFD
   44 dir-sync         sync .
   45 other            close .
   46 db-page          pwrite64 t.db off=0 len=4096
   47 db-page          pwrite64 t.db off=4096 len=4096
   48 db-page          pwrite64 t.db off=8192 len=4096
   49 db-page          pwrite64 t.db off=12288 len=4096
   50 db-page          pwrite64 t.db off=77824 len=4096
   51 db-page          pwrite64 t.db off=86016 len=4096
   52 db-page          pwrite64 t.db off=102400 len=4096
   53 db-page          pwrite64 t.db off=106496 len=4096
   54 db-page          pwrite64 t.db off=118784 len=4096
   55 db-page          pwrite64 t.db off=122880 len=4096
   56 db-page          pwrite64 t.db off=126976 len=4096
   57 db-page          pwrite64 t.db off=131072 len=4096
   58 db-page          pwrite64 t.db off=135168 len=4096
   59 db-page          pwrite64 t.db off=139264 len=4096
   60 db-page          pwrite64 t.db off=143360 len=4096
   61 db-page          pwrite64 t.db off=147456 len=4096
   62 db-page          pwrite64 t.db off=151552 len=4096
   63 db-page          pwrite64 t.db off=155648 len=4096
   64 db-sync          sync t.db
   65 other            close t.db-journal
   66 journal-unlink   unlink t.db-journal
   67 other            fcntl t.db F_SETLK F_RDLCK start=1073741826 len=510
   68 other            fcntl t.db F_SETLK F_UNLCK start=1073741824 len=2
   69 other            fcntl t.db F_SETLK F_UNLCK start=0 len=0
sequence: journal-header, journal-record x21, journal-count, journal-sync, dir-sync, db-page x18, db-sync, journal-unlink
journaled pages 7 (21 record writes), database page writes 18, syncs 3 [t.db-journal . t.db]
INFO: sync syscall used by the Go driver: fsync
check ok: step 3, journal_mode=delete, synchronous=NORMAL: order is journal-header, journal-record x21, journal-count, journal-sync, dir-sync, db-page x18, db-sync, journal-unlink
check ok: step 3, journal_mode=delete, synchronous=NORMAL: 3 syncs per commit (journal, directory and database syncs counted)
check ok: step 3, journal_mode=delete, synchronous=NORMAL: each journal record is three pwrite64 calls: page number (4 bytes), original page image (4096), checksum (4)
check ok: step 3, journal_mode=delete, synchronous=NORMAL: the traced commit leaves exactly the post-step bytes
twin check: the C CLI running the same step as literal SQL makes 45 writes, syncs and unlinks, identical to the Go driver's
INFO: CLI sync syscall: fsync
check ok: synchronous=NORMAL: the C CLI makes the same writes, syncs and unlink in the same order and leaves the post-step bytes
kill points: 46 (every pwrite64, sync and unlink of the commit, then the first syscall after the step returned)
INFO: 46 kill runs took 2.876s with 4 workers
kill 01 on entry to pwrite64 t.db-journal off=0 len=512    [journal-header]
    on disk:  database pre-step bytes
              -journal 0 bytes (not hot)
              SQLite on reopen ignores it (not hot)
    reopened: pre-step, exact bytes (recipe step 2); integrity_check ok; cold -journal left (0 bytes, first byte 0x00)
kill 02 on entry to pwrite64 t.db-journal off=512 len=4    [journal-record]
    on disk:  database pre-step bytes
              -journal 512 bytes, first byte 0x00 (not hot), header not-yet-synced (magic and record count still zero), initial size 29 pages, sector 512, page 4096, 0 complete record(s) [], 0 checksum failure(s)
              SQLite on reopen ignores it (not hot)
    reopened: pre-step, exact bytes (recipe step 2); integrity_check ok; cold -journal left (512 bytes, first byte 0x00)
INFO: step 3 NORMAL kill 02 journal nonce 0xbad8982d, run 132ms
kill 03 on entry to pwrite64 t.db-journal off=516 len=4096 [journal-record]
    on disk:  database pre-step bytes
              -journal 516 bytes, first byte 0x00 (not hot), header not-yet-synced (magic and record count still zero), initial size 29 pages, sector 512, page 4096, 0 complete record(s) [], 0 checksum failure(s), 4 byte(s) of an incomplete record
              SQLite on reopen ignores it (not hot)
    reopened: pre-step, exact bytes (recipe step 2); integrity_check ok; cold -journal left (516 bytes, first byte 0x00)
INFO: step 3 NORMAL kill 03 journal nonce 0xd45ca09a, run 163ms
kill 04 on entry to pwrite64 t.db-journal off=4612 len=4   [journal-record]
    on disk:  database pre-step bytes
              -journal 4612 bytes, first byte 0x00 (not hot), header not-yet-synced (magic and record count still zero), initial size 29 pages, sector 512, page 4096, 0 complete record(s) [], 0 checksum failure(s), 4100 byte(s) of an incomplete record
              SQLite on reopen ignores it (not hot)
    reopened: pre-step, exact bytes (recipe step 2); integrity_check ok; cold -journal left (4612 bytes, first byte 0x00)
INFO: step 3 NORMAL kill 04 journal nonce 0xdbe5ef92, run 164ms
kill 05 on entry to pwrite64 t.db-journal off=4616 len=4   [journal-record]
    on disk:  database pre-step bytes
              -journal 4616 bytes, first byte 0x00 (not hot), header not-yet-synced (magic and record count still zero), initial size 29 pages, sector 512, page 4096, 1 complete record(s) [2], 0 checksum failure(s)
              SQLite on reopen ignores it (not hot)
    reopened: pre-step, exact bytes (recipe step 2); integrity_check ok; cold -journal left (4616 bytes, first byte 0x00)
INFO: step 3 NORMAL kill 05 journal nonce 0xc754555a, run 152ms
kill 06 on entry to pwrite64 t.db-journal off=4620 len=4096 [journal-record]
    on disk:  database pre-step bytes
              -journal 4620 bytes, first byte 0x00 (not hot), header not-yet-synced (magic and record count still zero), initial size 29 pages, sector 512, page 4096, 1 complete record(s) [2], 0 checksum failure(s), 4 byte(s) of an incomplete record
              SQLite on reopen ignores it (not hot)
    reopened: pre-step, exact bytes (recipe step 2); integrity_check ok; cold -journal left (4620 bytes, first byte 0x00)
INFO: step 3 NORMAL kill 06 journal nonce 0x504dd325, run 88ms
kill 07 on entry to pwrite64 t.db-journal off=8716 len=4   [journal-record]
    on disk:  database pre-step bytes
              -journal 8716 bytes, first byte 0x00 (not hot), header not-yet-synced (magic and record count still zero), initial size 29 pages, sector 512, page 4096, 1 complete record(s) [2], 0 checksum failure(s), 4100 byte(s) of an incomplete record
              SQLite on reopen ignores it (not hot)
    reopened: pre-step, exact bytes (recipe step 2); integrity_check ok; cold -journal left (8716 bytes, first byte 0x00)
INFO: step 3 NORMAL kill 07 journal nonce 0x9538b1cf, run 225ms
kill 08 on entry to pwrite64 t.db-journal off=8720 len=4   [journal-record]
    on disk:  database pre-step bytes
              -journal 8720 bytes, first byte 0x00 (not hot), header not-yet-synced (magic and record count still zero), initial size 29 pages, sector 512, page 4096, 2 complete record(s) [2 1], 0 checksum failure(s)
              SQLite on reopen ignores it (not hot)
    reopened: pre-step, exact bytes (recipe step 2); integrity_check ok; cold -journal left (8720 bytes, first byte 0x00)
INFO: step 3 NORMAL kill 08 journal nonce 0x4d5b32db, run 70ms
kill 09 on entry to pwrite64 t.db-journal off=8724 len=4096 [journal-record]
    on disk:  database pre-step bytes
              -journal 8724 bytes, first byte 0x00 (not hot), header not-yet-synced (magic and record count still zero), initial size 29 pages, sector 512, page 4096, 2 complete record(s) [2 1], 0 checksum failure(s), 4 byte(s) of an incomplete record
              SQLite on reopen ignores it (not hot)
    reopened: pre-step, exact bytes (recipe step 2); integrity_check ok; cold -journal left (8724 bytes, first byte 0x00)
INFO: step 3 NORMAL kill 09 journal nonce 0x86b228b0, run 102ms
kill 10 on entry to pwrite64 t.db-journal off=12820 len=4  [journal-record]
    on disk:  database pre-step bytes
              -journal 12820 bytes, first byte 0x00 (not hot), header not-yet-synced (magic and record count still zero), initial size 29 pages, sector 512, page 4096, 2 complete record(s) [2 1], 0 checksum failure(s), 4100 byte(s) of an incomplete record
              SQLite on reopen ignores it (not hot)
    reopened: pre-step, exact bytes (recipe step 2); integrity_check ok; cold -journal left (12820 bytes, first byte 0x00)
INFO: step 3 NORMAL kill 10 journal nonce 0xb8dcae80, run 189ms
kill 11 on entry to pwrite64 t.db-journal off=12824 len=4  [journal-record]
    on disk:  database pre-step bytes
              -journal 12824 bytes, first byte 0x00 (not hot), header not-yet-synced (magic and record count still zero), initial size 29 pages, sector 512, page 4096, 3 complete record(s) [2 1 20], 0 checksum failure(s)
              SQLite on reopen ignores it (not hot)
    reopened: pre-step, exact bytes (recipe step 2); integrity_check ok; cold -journal left (12824 bytes, first byte 0x00)
INFO: step 3 NORMAL kill 11 journal nonce 0x827fe55d, run 149ms
kill 12 on entry to pwrite64 t.db-journal off=12828 len=4096 [journal-record]
    on disk:  database pre-step bytes
              -journal 12828 bytes, first byte 0x00 (not hot), header not-yet-synced (magic and record count still zero), initial size 29 pages, sector 512, page 4096, 3 complete record(s) [2 1 20], 0 checksum failure(s), 4 byte(s) of an incomplete record
              SQLite on reopen ignores it (not hot)
    reopened: pre-step, exact bytes (recipe step 2); integrity_check ok; cold -journal left (12828 bytes, first byte 0x00)
INFO: step 3 NORMAL kill 12 journal nonce 0xdf1d48b8, run 82ms
kill 13 on entry to pwrite64 t.db-journal off=16924 len=4  [journal-record]
    on disk:  database pre-step bytes
              -journal 16924 bytes, first byte 0x00 (not hot), header not-yet-synced (magic and record count still zero), initial size 29 pages, sector 512, page 4096, 3 complete record(s) [2 1 20], 0 checksum failure(s), 4100 byte(s) of an incomplete record
              SQLite on reopen ignores it (not hot)
    reopened: pre-step, exact bytes (recipe step 2); integrity_check ok; cold -journal left (16924 bytes, first byte 0x00)
INFO: step 3 NORMAL kill 13 journal nonce 0xe2d897e4, run 164ms
kill 14 on entry to pwrite64 t.db-journal off=16928 len=4  [journal-record]
    on disk:  database pre-step bytes
              -journal 16928 bytes, first byte 0x00 (not hot), header not-yet-synced (magic and record count still zero), initial size 29 pages, sector 512, page 4096, 4 complete record(s) [2 1 20 3], 0 checksum failure(s)
              SQLite on reopen ignores it (not hot)
    reopened: pre-step, exact bytes (recipe step 2); integrity_check ok; cold -journal left (16928 bytes, first byte 0x00)
INFO: step 3 NORMAL kill 14 journal nonce 0xad3ecb79, run 111ms
kill 15 on entry to pwrite64 t.db-journal off=16932 len=4096 [journal-record]
    on disk:  database pre-step bytes
              -journal 16932 bytes, first byte 0x00 (not hot), header not-yet-synced (magic and record count still zero), initial size 29 pages, sector 512, page 4096, 4 complete record(s) [2 1 20 3], 0 checksum failure(s), 4 byte(s) of an incomplete record
              SQLite on reopen ignores it (not hot)
    reopened: pre-step, exact bytes (recipe step 2); integrity_check ok; cold -journal left (16932 bytes, first byte 0x00)
INFO: step 3 NORMAL kill 15 journal nonce 0xa58308c9, run 202ms
kill 16 on entry to pwrite64 t.db-journal off=21028 len=4  [journal-record]
    on disk:  database pre-step bytes
              -journal 21028 bytes, first byte 0x00 (not hot), header not-yet-synced (magic and record count still zero), initial size 29 pages, sector 512, page 4096, 4 complete record(s) [2 1 20 3], 0 checksum failure(s), 4100 byte(s) of an incomplete record
              SQLite on reopen ignores it (not hot)
    reopened: pre-step, exact bytes (recipe step 2); integrity_check ok; cold -journal left (21028 bytes, first byte 0x00)
INFO: step 3 NORMAL kill 16 journal nonce 0x07661664, run 187ms
kill 17 on entry to pwrite64 t.db-journal off=21032 len=4  [journal-record]
    on disk:  database pre-step bytes
              -journal 21032 bytes, first byte 0x00 (not hot), header not-yet-synced (magic and record count still zero), initial size 29 pages, sector 512, page 4096, 5 complete record(s) [2 1 20 3 27], 0 checksum failure(s)
              SQLite on reopen ignores it (not hot)
    reopened: pre-step, exact bytes (recipe step 2); integrity_check ok; cold -journal left (21032 bytes, first byte 0x00)
INFO: step 3 NORMAL kill 17 journal nonce 0x8eb943f0, run 108ms
kill 18 on entry to pwrite64 t.db-journal off=21036 len=4096 [journal-record]
    on disk:  database pre-step bytes
              -journal 21036 bytes, first byte 0x00 (not hot), header not-yet-synced (magic and record count still zero), initial size 29 pages, sector 512, page 4096, 5 complete record(s) [2 1 20 3 27], 0 checksum failure(s), 4 byte(s) of an incomplete record
              SQLite on reopen ignores it (not hot)
    reopened: pre-step, exact bytes (recipe step 2); integrity_check ok; cold -journal left (21036 bytes, first byte 0x00)
INFO: step 3 NORMAL kill 18 journal nonce 0x040f4014, run 220ms
kill 19 on entry to pwrite64 t.db-journal off=25132 len=4  [journal-record]
    on disk:  database pre-step bytes
              -journal 25132 bytes, first byte 0x00 (not hot), header not-yet-synced (magic and record count still zero), initial size 29 pages, sector 512, page 4096, 5 complete record(s) [2 1 20 3 27], 0 checksum failure(s), 4100 byte(s) of an incomplete record
              SQLite on reopen ignores it (not hot)
    reopened: pre-step, exact bytes (recipe step 2); integrity_check ok; cold -journal left (25132 bytes, first byte 0x00)
INFO: step 3 NORMAL kill 19 journal nonce 0x1dbba345, run 83ms
kill 20 on entry to pwrite64 t.db-journal off=25136 len=4  [journal-record]
    on disk:  database pre-step bytes
              -journal 25136 bytes, first byte 0x00 (not hot), header not-yet-synced (magic and record count still zero), initial size 29 pages, sector 512, page 4096, 6 complete record(s) [2 1 20 3 27 4], 0 checksum failure(s)
              SQLite on reopen ignores it (not hot)
    reopened: pre-step, exact bytes (recipe step 2); integrity_check ok; cold -journal left (25136 bytes, first byte 0x00)
INFO: step 3 NORMAL kill 20 journal nonce 0x26bc451a, run 130ms
kill 21 on entry to pwrite64 t.db-journal off=25140 len=4096 [journal-record]
    on disk:  database pre-step bytes
              -journal 25140 bytes, first byte 0x00 (not hot), header not-yet-synced (magic and record count still zero), initial size 29 pages, sector 512, page 4096, 6 complete record(s) [2 1 20 3 27 4], 0 checksum failure(s), 4 byte(s) of an incomplete record
              SQLite on reopen ignores it (not hot)
    reopened: pre-step, exact bytes (recipe step 2); integrity_check ok; cold -journal left (25140 bytes, first byte 0x00)
INFO: step 3 NORMAL kill 21 journal nonce 0x9edbbedd, run 162ms
kill 22 on entry to pwrite64 t.db-journal off=29236 len=4  [journal-record]
    on disk:  database pre-step bytes
              -journal 29236 bytes, first byte 0x00 (not hot), header not-yet-synced (magic and record count still zero), initial size 29 pages, sector 512, page 4096, 6 complete record(s) [2 1 20 3 27 4], 0 checksum failure(s), 4100 byte(s) of an incomplete record
              SQLite on reopen ignores it (not hot)
    reopened: pre-step, exact bytes (recipe step 2); integrity_check ok; cold -journal left (29236 bytes, first byte 0x00)
INFO: step 3 NORMAL kill 22 journal nonce 0x5f7dbf06, run 111ms
kill 23 on entry to pwrite64 t.db-journal off=0 len=12     [journal-count]
    on disk:  database pre-step bytes
              -journal 29240 bytes, first byte 0x00 (not hot), header not-yet-synced (magic and record count still zero), initial size 29 pages, sector 512, page 4096, 7 complete record(s) [2 1 20 3 27 4 26], 0 checksum failure(s)
              SQLite on reopen ignores it (not hot)
    reopened: pre-step, exact bytes (recipe step 2); integrity_check ok; cold -journal left (29240 bytes, first byte 0x00)
INFO: step 3 NORMAL kill 23 journal nonce 0x964e5825, run 92ms
kill 24 on entry to sync t.db-journal                      [journal-sync]
    on disk:  database pre-step bytes
              -journal 29240 bytes, first byte 0xd9 (hot), header synced (record count 7), initial size 29 pages, sector 512, page 4096, 7 complete record(s) [2 1 20 3 27 4 26], 0 checksum failure(s)
              SQLite on reopen rolls back: writes pages [2 1 20 3 27 4 26] from the journal, truncates to 29 pages
    reopened: pre-step, exact bytes (recipe step 2); integrity_check ok; no -journal
INFO: step 3 NORMAL kill 24 journal nonce 0x89806b2c, run 163ms
kill 25 on entry to sync .                                 [dir-sync]
    on disk:  database pre-step bytes
              -journal 29240 bytes, first byte 0xd9 (hot), header synced (record count 7), initial size 29 pages, sector 512, page 4096, 7 complete record(s) [2 1 20 3 27 4 26], 0 checksum failure(s)
              SQLite on reopen rolls back: writes pages [2 1 20 3 27 4 26] from the journal, truncates to 29 pages
    reopened: pre-step, exact bytes (recipe step 2); integrity_check ok; no -journal
INFO: step 3 NORMAL kill 25 journal nonce 0x0aeb35a4, run 100ms
kill 26 on entry to pwrite64 t.db off=0 len=4096           [db-page]
    on disk:  database pre-step bytes
              -journal 29240 bytes, first byte 0xd9 (hot), header synced (record count 7), initial size 29 pages, sector 512, page 4096, 7 complete record(s) [2 1 20 3 27 4 26], 0 checksum failure(s)
              SQLite on reopen rolls back: writes pages [2 1 20 3 27 4 26] from the journal, truncates to 29 pages
    reopened: pre-step, exact bytes (recipe step 2); integrity_check ok; no -journal
INFO: step 3 NORMAL kill 26 journal nonce 0xad4b27fb, run 163ms
kill 27 on entry to pwrite64 t.db off=4096 len=4096        [db-page]
    on disk:  database torn: 29 pages, 1 differ from pre-step, 17 differ from post-step
              -journal 29240 bytes, first byte 0xd9 (hot), header synced (record count 7), initial size 29 pages, sector 512, page 4096, 7 complete record(s) [2 1 20 3 27 4 26], 0 checksum failure(s)
              SQLite on reopen rolls back: writes pages [2 1 20 3 27 4 26] from the journal, truncates to 29 pages
    reopened: pre-step, exact bytes (recipe step 2); integrity_check ok; no -journal
INFO: step 3 NORMAL kill 27 journal nonce 0xeed0b805, run 149ms
kill 28 on entry to pwrite64 t.db off=8192 len=4096        [db-page]
    on disk:  database torn: 29 pages, 2 differ from pre-step, 16 differ from post-step
              -journal 29240 bytes, first byte 0xd9 (hot), header synced (record count 7), initial size 29 pages, sector 512, page 4096, 7 complete record(s) [2 1 20 3 27 4 26], 0 checksum failure(s)
              SQLite on reopen rolls back: writes pages [2 1 20 3 27 4 26] from the journal, truncates to 29 pages
    reopened: pre-step, exact bytes (recipe step 2); integrity_check ok; no -journal
INFO: step 3 NORMAL kill 28 journal nonce 0x9e3b3528, run 306ms
kill 29 on entry to pwrite64 t.db off=12288 len=4096       [db-page]
    on disk:  database torn: 29 pages, 3 differ from pre-step, 15 differ from post-step
              -journal 29240 bytes, first byte 0xd9 (hot), header synced (record count 7), initial size 29 pages, sector 512, page 4096, 7 complete record(s) [2 1 20 3 27 4 26], 0 checksum failure(s)
              SQLite on reopen rolls back: writes pages [2 1 20 3 27 4 26] from the journal, truncates to 29 pages
    reopened: pre-step, exact bytes (recipe step 2); integrity_check ok; no -journal
INFO: step 3 NORMAL kill 29 journal nonce 0xa4735b4d, run 318ms
kill 30 on entry to pwrite64 t.db off=77824 len=4096       [db-page]
    on disk:  database torn: 29 pages, 4 differ from pre-step, 14 differ from post-step
              -journal 29240 bytes, first byte 0xd9 (hot), header synced (record count 7), initial size 29 pages, sector 512, page 4096, 7 complete record(s) [2 1 20 3 27 4 26], 0 checksum failure(s)
              SQLite on reopen rolls back: writes pages [2 1 20 3 27 4 26] from the journal, truncates to 29 pages
    reopened: pre-step, exact bytes (recipe step 2); integrity_check ok; no -journal
INFO: step 3 NORMAL kill 30 journal nonce 0x32e99f4d, run 162ms
kill 31 on entry to pwrite64 t.db off=86016 len=4096       [db-page]
    on disk:  database torn: 29 pages, 5 differ from pre-step, 13 differ from post-step
              -journal 29240 bytes, first byte 0xd9 (hot), header synced (record count 7), initial size 29 pages, sector 512, page 4096, 7 complete record(s) [2 1 20 3 27 4 26], 0 checksum failure(s)
              SQLite on reopen rolls back: writes pages [2 1 20 3 27 4 26] from the journal, truncates to 29 pages
    reopened: pre-step, exact bytes (recipe step 2); integrity_check ok; no -journal
INFO: step 3 NORMAL kill 31 journal nonce 0xd436539c, run 302ms
kill 32 on entry to pwrite64 t.db off=102400 len=4096      [db-page]
    on disk:  database torn: 29 pages, 6 differ from pre-step, 12 differ from post-step
              -journal 29240 bytes, first byte 0xd9 (hot), header synced (record count 7), initial size 29 pages, sector 512, page 4096, 7 complete record(s) [2 1 20 3 27 4 26], 0 checksum failure(s)
              SQLite on reopen rolls back: writes pages [2 1 20 3 27 4 26] from the journal, truncates to 29 pages
    reopened: pre-step except page(s) [22], which are freelist leaves in the pre-step file (unused bytes); VACUUM INTO output equals the pre-step's; integrity_check ok; no -journal
INFO: step 3 NORMAL kill 32 journal nonce 0x5b4f98d8, run 197ms
kill 33 on entry to pwrite64 t.db off=106496 len=4096      [db-page]
    on disk:  database torn: 29 pages, 7 differ from pre-step, 11 differ from post-step
              -journal 29240 bytes, first byte 0xd9 (hot), header synced (record count 7), initial size 29 pages, sector 512, page 4096, 7 complete record(s) [2 1 20 3 27 4 26], 0 checksum failure(s)
              SQLite on reopen rolls back: writes pages [2 1 20 3 27 4 26] from the journal, truncates to 29 pages
    reopened: pre-step except page(s) [22], which are freelist leaves in the pre-step file (unused bytes); VACUUM INTO output equals the pre-step's; integrity_check ok; no -journal
INFO: step 3 NORMAL kill 33 journal nonce 0x1f5822f4, run 319ms
kill 34 on entry to pwrite64 t.db off=118784 len=4096      [db-page]
    on disk:  database torn: 29 pages, 8 differ from pre-step, 10 differ from post-step
              -journal 29240 bytes, first byte 0xd9 (hot), header synced (record count 7), initial size 29 pages, sector 512, page 4096, 7 complete record(s) [2 1 20 3 27 4 26], 0 checksum failure(s)
              SQLite on reopen rolls back: writes pages [2 1 20 3 27 4 26] from the journal, truncates to 29 pages
    reopened: pre-step except page(s) [22], which are freelist leaves in the pre-step file (unused bytes); VACUUM INTO output equals the pre-step's; integrity_check ok; no -journal
INFO: step 3 NORMAL kill 34 journal nonce 0xffe663d5, run 219ms
kill 35 on entry to pwrite64 t.db off=122880 len=4096      [db-page]
    on disk:  database torn: 30 pages, 9 differ from pre-step, 9 differ from post-step
              -journal 29240 bytes, first byte 0xd9 (hot), header synced (record count 7), initial size 29 pages, sector 512, page 4096, 7 complete record(s) [2 1 20 3 27 4 26], 0 checksum failure(s)
              SQLite on reopen rolls back: writes pages [2 1 20 3 27 4 26] from the journal, truncates to 29 pages
    reopened: pre-step except page(s) [22], which are freelist leaves in the pre-step file (unused bytes); VACUUM INTO output equals the pre-step's; integrity_check ok; no -journal
INFO: step 3 NORMAL kill 35 journal nonce 0x38796d7a, run 303ms
kill 36 on entry to pwrite64 t.db off=126976 len=4096      [db-page]
    on disk:  database torn: 31 pages, 10 differ from pre-step, 8 differ from post-step
              -journal 29240 bytes, first byte 0xd9 (hot), header synced (record count 7), initial size 29 pages, sector 512, page 4096, 7 complete record(s) [2 1 20 3 27 4 26], 0 checksum failure(s)
              SQLite on reopen rolls back: writes pages [2 1 20 3 27 4 26] from the journal, truncates to 29 pages
    reopened: pre-step except page(s) [22], which are freelist leaves in the pre-step file (unused bytes); VACUUM INTO output equals the pre-step's; integrity_check ok; no -journal
INFO: step 3 NORMAL kill 36 journal nonce 0x85d501ba, run 328ms
kill 37 on entry to pwrite64 t.db off=131072 len=4096      [db-page]
    on disk:  database torn: 32 pages, 11 differ from pre-step, 7 differ from post-step
              -journal 29240 bytes, first byte 0xd9 (hot), header synced (record count 7), initial size 29 pages, sector 512, page 4096, 7 complete record(s) [2 1 20 3 27 4 26], 0 checksum failure(s)
              SQLite on reopen rolls back: writes pages [2 1 20 3 27 4 26] from the journal, truncates to 29 pages
    reopened: pre-step except page(s) [22], which are freelist leaves in the pre-step file (unused bytes); VACUUM INTO output equals the pre-step's; integrity_check ok; no -journal
INFO: step 3 NORMAL kill 37 journal nonce 0xd776e25c, run 208ms
kill 38 on entry to pwrite64 t.db off=135168 len=4096      [db-page]
    on disk:  database torn: 33 pages, 12 differ from pre-step, 6 differ from post-step
              -journal 29240 bytes, first byte 0xd9 (hot), header synced (record count 7), initial size 29 pages, sector 512, page 4096, 7 complete record(s) [2 1 20 3 27 4 26], 0 checksum failure(s)
              SQLite on reopen rolls back: writes pages [2 1 20 3 27 4 26] from the journal, truncates to 29 pages
    reopened: pre-step except page(s) [22], which are freelist leaves in the pre-step file (unused bytes); VACUUM INTO output equals the pre-step's; integrity_check ok; no -journal
INFO: step 3 NORMAL kill 38 journal nonce 0xc6d7b100, run 165ms
kill 39 on entry to pwrite64 t.db off=139264 len=4096      [db-page]
    on disk:  database torn: 34 pages, 13 differ from pre-step, 5 differ from post-step
              -journal 29240 bytes, first byte 0xd9 (hot), header synced (record count 7), initial size 29 pages, sector 512, page 4096, 7 complete record(s) [2 1 20 3 27 4 26], 0 checksum failure(s)
              SQLite on reopen rolls back: writes pages [2 1 20 3 27 4 26] from the journal, truncates to 29 pages
    reopened: pre-step except page(s) [22], which are freelist leaves in the pre-step file (unused bytes); VACUUM INTO output equals the pre-step's; integrity_check ok; no -journal
INFO: step 3 NORMAL kill 39 journal nonce 0xb96f8c84, run 351ms
kill 40 on entry to pwrite64 t.db off=143360 len=4096      [db-page]
    on disk:  database torn: 35 pages, 14 differ from pre-step, 4 differ from post-step
              -journal 29240 bytes, first byte 0xd9 (hot), header synced (record count 7), initial size 29 pages, sector 512, page 4096, 7 complete record(s) [2 1 20 3 27 4 26], 0 checksum failure(s)
              SQLite on reopen rolls back: writes pages [2 1 20 3 27 4 26] from the journal, truncates to 29 pages
    reopened: pre-step except page(s) [22], which are freelist leaves in the pre-step file (unused bytes); VACUUM INTO output equals the pre-step's; integrity_check ok; no -journal
INFO: step 3 NORMAL kill 40 journal nonce 0xd349dbb4, run 278ms
kill 41 on entry to pwrite64 t.db off=147456 len=4096      [db-page]
    on disk:  database torn: 36 pages, 15 differ from pre-step, 3 differ from post-step
              -journal 29240 bytes, first byte 0xd9 (hot), header synced (record count 7), initial size 29 pages, sector 512, page 4096, 7 complete record(s) [2 1 20 3 27 4 26], 0 checksum failure(s)
              SQLite on reopen rolls back: writes pages [2 1 20 3 27 4 26] from the journal, truncates to 29 pages
    reopened: pre-step except page(s) [22], which are freelist leaves in the pre-step file (unused bytes); VACUUM INTO output equals the pre-step's; integrity_check ok; no -journal
INFO: step 3 NORMAL kill 41 journal nonce 0xa81d61ad, run 369ms
kill 42 on entry to pwrite64 t.db off=151552 len=4096      [db-page]
    on disk:  database torn: 37 pages, 16 differ from pre-step, 2 differ from post-step
              -journal 29240 bytes, first byte 0xd9 (hot), header synced (record count 7), initial size 29 pages, sector 512, page 4096, 7 complete record(s) [2 1 20 3 27 4 26], 0 checksum failure(s)
              SQLite on reopen rolls back: writes pages [2 1 20 3 27 4 26] from the journal, truncates to 29 pages
    reopened: pre-step except page(s) [22], which are freelist leaves in the pre-step file (unused bytes); VACUUM INTO output equals the pre-step's; integrity_check ok; no -journal
INFO: step 3 NORMAL kill 42 journal nonce 0x06a83b73, run 321ms
kill 43 on entry to pwrite64 t.db off=155648 len=4096      [db-page]
    on disk:  database torn: 38 pages, 17 differ from pre-step, 1 differ from post-step
              -journal 29240 bytes, first byte 0xd9 (hot), header synced (record count 7), initial size 29 pages, sector 512, page 4096, 7 complete record(s) [2 1 20 3 27 4 26], 0 checksum failure(s)
              SQLite on reopen rolls back: writes pages [2 1 20 3 27 4 26] from the journal, truncates to 29 pages
    reopened: pre-step except page(s) [22], which are freelist leaves in the pre-step file (unused bytes); VACUUM INTO output equals the pre-step's; integrity_check ok; no -journal
INFO: step 3 NORMAL kill 43 journal nonce 0x92cc927d, run 129ms
kill 44 on entry to sync t.db                              [db-sync]
    on disk:  database post-step bytes
              -journal 29240 bytes, first byte 0xd9 (hot), header synced (record count 7), initial size 29 pages, sector 512, page 4096, 7 complete record(s) [2 1 20 3 27 4 26], 0 checksum failure(s)
              SQLite on reopen rolls back: writes pages [2 1 20 3 27 4 26] from the journal, truncates to 29 pages
    reopened: pre-step except page(s) [22], which are freelist leaves in the pre-step file (unused bytes); VACUUM INTO output equals the pre-step's; integrity_check ok; no -journal
INFO: step 3 NORMAL kill 44 journal nonce 0x77ffa38d, run 224ms
kill 45 on entry to unlink t.db-journal                    [journal-unlink]
    on disk:  database post-step bytes
              -journal 29240 bytes, first byte 0xd9 (hot), header synced (record count 7), initial size 29 pages, sector 512, page 4096, 7 complete record(s) [2 1 20 3 27 4 26], 0 checksum failure(s)
              SQLite on reopen rolls back: writes pages [2 1 20 3 27 4 26] from the journal, truncates to 29 pages
    reopened: pre-step except page(s) [22], which are freelist leaves in the pre-step file (unused bytes); VACUUM INTO output equals the pre-step's; integrity_check ok; no -journal
INFO: step 3 NORMAL kill 45 journal nonce 0x7445db09, run 202ms
kill 46 on entry to open marker-end                        [step-returned]
    on disk:  database post-step bytes
              no -journal
              SQLite on reopen nothing to roll back
    reopened: post-step, exact bytes (recipe step 3); integrity_check ok; no -journal
outcomes: 31 pre-step exact, 14 pre-step except reused freelist leaves, 1 post-step exact, 0 other
check ok: step 3, synchronous=NORMAL: every kill landed on the intended syscall (strace saw it unfinished and the child died of SIGKILL)
check ok: step 3, synchronous=NORMAL: every reopened database is the pre-step or the post-step state, never a third (pre-step allows reused freelist-leaf pages, see above)
check ok: step 3, synchronous=NORMAL: every kill up to and including the journal unlink gives pre-step; the kill after it gives post-step (the unlink is the commit point)
check ok: step 3, synchronous=NORMAL: integrity_check is ok after every reopen
check ok: step 3, synchronous=NORMAL: for every kill up to the commit point, whenever the database file differs from pre-step before reopen, the journal is hot and its records cover every changed page except pages appended past the original size (removed by truncation) and reused freelist leaves

== D. step 3, synchronous=OFF: one commit traced, then killed at every write and sync ==
the commit's syscalls on the database directory (writer thread, between the step's start and its return):
    1 other            fcntl t.db F_SETLK F_RDLCK start=1073741824 len=1
    2 other            fcntl t.db F_SETLK F_RDLCK start=1073741826 len=510
    3 other            fcntl t.db F_SETLK F_UNLCK start=1073741824 len=1
    4 other            pread64 t.db off=24 len=16
    5 other            fcntl t.db F_SETLK F_WRLCK start=1073741825 len=1
    6 other            pread64 t.db off=4096 len=4096
    7 other            open t.db-journal
    8 other            fcntl t.db-journal F_SETFD
    9 journal-header   pwrite64 t.db-journal off=0 len=512
   10 journal-record   pwrite64 t.db-journal off=512 len=4
   11 journal-record   pwrite64 t.db-journal off=516 len=4096
   12 journal-record   pwrite64 t.db-journal off=4612 len=4
   13 other            pread64 t.db off=8192 len=4096
   14 other            pread64 t.db off=98304 len=4096
   15 journal-record   pwrite64 t.db-journal off=4616 len=4
   16 journal-record   pwrite64 t.db-journal off=4620 len=4096
   17 journal-record   pwrite64 t.db-journal off=8716 len=4
   18 other            pread64 t.db off=77824 len=4096
   19 journal-record   pwrite64 t.db-journal off=8720 len=4
   20 journal-record   pwrite64 t.db-journal off=8724 len=4096
   21 journal-record   pwrite64 t.db-journal off=12820 len=4
   22 journal-record   pwrite64 t.db-journal off=12824 len=4
   23 journal-record   pwrite64 t.db-journal off=12828 len=4096
   24 journal-record   pwrite64 t.db-journal off=16924 len=4
   25 other            pread64 t.db off=12288 len=4096
   26 other            pread64 t.db off=106496 len=4096
   27 journal-record   pwrite64 t.db-journal off=16928 len=4
   28 journal-record   pwrite64 t.db-journal off=16932 len=4096
   29 journal-record   pwrite64 t.db-journal off=21028 len=4
   30 journal-record   pwrite64 t.db-journal off=21032 len=4
   31 journal-record   pwrite64 t.db-journal off=21036 len=4096
   32 journal-record   pwrite64 t.db-journal off=25132 len=4
   33 other            pread64 t.db off=102400 len=4096
   34 journal-record   pwrite64 t.db-journal off=25136 len=4
   35 journal-record   pwrite64 t.db-journal off=25140 len=4096
   36 journal-record   pwrite64 t.db-journal off=29236 len=4
   37 other            fcntl t.db F_SETLK F_WRLCK start=1073741824 len=1
   38 other            fcntl t.db F_SETLK F_WRLCK start=1073741826 len=510
   39 db-page          pwrite64 t.db off=0 len=4096
   40 db-page          pwrite64 t.db off=4096 len=4096
   41 db-page          pwrite64 t.db off=8192 len=4096
   42 db-page          pwrite64 t.db off=12288 len=4096
   43 db-page          pwrite64 t.db off=77824 len=4096
   44 db-page          pwrite64 t.db off=86016 len=4096
   45 db-page          pwrite64 t.db off=102400 len=4096
   46 db-page          pwrite64 t.db off=106496 len=4096
   47 db-page          pwrite64 t.db off=118784 len=4096
   48 db-page          pwrite64 t.db off=122880 len=4096
   49 db-page          pwrite64 t.db off=126976 len=4096
   50 db-page          pwrite64 t.db off=131072 len=4096
   51 db-page          pwrite64 t.db off=135168 len=4096
   52 db-page          pwrite64 t.db off=139264 len=4096
   53 db-page          pwrite64 t.db off=143360 len=4096
   54 db-page          pwrite64 t.db off=147456 len=4096
   55 db-page          pwrite64 t.db off=151552 len=4096
   56 db-page          pwrite64 t.db off=155648 len=4096
   57 other            close t.db-journal
   58 journal-unlink   unlink t.db-journal
   59 other            fcntl t.db F_SETLK F_RDLCK start=1073741826 len=510
   60 other            fcntl t.db F_SETLK F_UNLCK start=1073741824 len=2
   61 other            fcntl t.db F_SETLK F_UNLCK start=0 len=0
sequence: journal-header, journal-record x21, db-page x18, journal-unlink
journaled pages 7 (21 record writes), database page writes 18, syncs 0 []
INFO: sync syscall used by the Go driver: (none)
check ok: step 3, journal_mode=delete, synchronous=OFF: order is journal-header, journal-record x21, db-page x18, journal-unlink
check ok: step 3, journal_mode=delete, synchronous=OFF: 0 syncs per commit (journal, directory and database syncs counted)
check ok: step 3, journal_mode=delete, synchronous=OFF: each journal record is three pwrite64 calls: page number (4 bytes), original page image (4096), checksum (4)
check ok: step 3, journal_mode=delete, synchronous=OFF: the traced commit leaves exactly the post-step bytes
twin check: the C CLI running the same step as literal SQL makes 41 writes, syncs and unlinks, identical to the Go driver's
INFO: CLI sync syscall: (none)
check ok: synchronous=OFF: the C CLI makes the same writes, syncs and unlink in the same order and leaves the post-step bytes
kill points: 42 (every pwrite64, sync and unlink of the commit, then the first syscall after the step returned)
INFO: 42 kill runs took 2.839s with 4 workers
kill 01 on entry to pwrite64 t.db-journal off=0 len=512    [journal-header]
    on disk:  database pre-step bytes
              -journal 0 bytes (not hot)
              SQLite on reopen ignores it (not hot)
    reopened: pre-step, exact bytes (recipe step 2); integrity_check ok; cold -journal left (0 bytes, first byte 0x00)
kill 02 on entry to pwrite64 t.db-journal off=512 len=4    [journal-record]
    on disk:  database pre-step bytes
              -journal 512 bytes, first byte 0xd9 (hot), header no-sync (record count 0xffffffff: read to end of file), initial size 29 pages, sector 512, page 4096, 0 complete record(s) [], 0 checksum failure(s)
              SQLite on reopen hot, but restores no page (end of journal)
    reopened: pre-step, exact bytes (recipe step 2); integrity_check ok; no -journal
INFO: step 3 OFF kill 02 journal nonce 0xdef789d9, run 168ms
kill 03 on entry to pwrite64 t.db-journal off=516 len=4096 [journal-record]
    on disk:  database pre-step bytes
              -journal 516 bytes, first byte 0xd9 (hot), header no-sync (record count 0xffffffff: read to end of file), initial size 29 pages, sector 512, page 4096, 0 complete record(s) [], 0 checksum failure(s), 4 byte(s) of an incomplete record
              SQLite on reopen hot, but restores no page (end of journal)
    reopened: pre-step, exact bytes (recipe step 2); integrity_check ok; no -journal
INFO: step 3 OFF kill 03 journal nonce 0x70145d63, run 199ms
kill 04 on entry to pwrite64 t.db-journal off=4612 len=4   [journal-record]
    on disk:  database pre-step bytes
              -journal 4612 bytes, first byte 0xd9 (hot), header no-sync (record count 0xffffffff: read to end of file), initial size 29 pages, sector 512, page 4096, 0 complete record(s) [], 0 checksum failure(s), 4100 byte(s) of an incomplete record
              SQLite on reopen hot, but restores no page (end of journal)
    reopened: pre-step, exact bytes (recipe step 2); integrity_check ok; no -journal
INFO: step 3 OFF kill 04 journal nonce 0x988862eb, run 154ms
kill 05 on entry to pwrite64 t.db-journal off=4616 len=4   [journal-record]
    on disk:  database pre-step bytes
              -journal 4616 bytes, first byte 0xd9 (hot), header no-sync (record count 0xffffffff: read to end of file), initial size 29 pages, sector 512, page 4096, 1 complete record(s) [2], 0 checksum failure(s)
              SQLite on reopen rolls back: writes pages [2] from the journal, truncates to 29 pages
    reopened: pre-step, exact bytes (recipe step 2); integrity_check ok; no -journal
INFO: step 3 OFF kill 05 journal nonce 0x079c54d5, run 114ms
kill 06 on entry to pwrite64 t.db-journal off=4620 len=4096 [journal-record]
    on disk:  database pre-step bytes
              -journal 4620 bytes, first byte 0xd9 (hot), header no-sync (record count 0xffffffff: read to end of file), initial size 29 pages, sector 512, page 4096, 1 complete record(s) [2], 0 checksum failure(s), 4 byte(s) of an incomplete record
              SQLite on reopen rolls back: writes pages [2] from the journal, truncates to 29 pages
    reopened: pre-step, exact bytes (recipe step 2); integrity_check ok; no -journal
INFO: step 3 OFF kill 06 journal nonce 0xf79a29d4, run 210ms
kill 07 on entry to pwrite64 t.db-journal off=8716 len=4   [journal-record]
    on disk:  database pre-step bytes
              -journal 8716 bytes, first byte 0xd9 (hot), header no-sync (record count 0xffffffff: read to end of file), initial size 29 pages, sector 512, page 4096, 1 complete record(s) [2], 0 checksum failure(s), 4100 byte(s) of an incomplete record
              SQLite on reopen rolls back: writes pages [2] from the journal, truncates to 29 pages
    reopened: pre-step, exact bytes (recipe step 2); integrity_check ok; no -journal
INFO: step 3 OFF kill 07 journal nonce 0x48c76a05, run 172ms
kill 08 on entry to pwrite64 t.db-journal off=8720 len=4   [journal-record]
    on disk:  database pre-step bytes
              -journal 8720 bytes, first byte 0xd9 (hot), header no-sync (record count 0xffffffff: read to end of file), initial size 29 pages, sector 512, page 4096, 2 complete record(s) [2 1], 0 checksum failure(s)
              SQLite on reopen rolls back: writes pages [2 1] from the journal, truncates to 29 pages
    reopened: pre-step, exact bytes (recipe step 2); integrity_check ok; no -journal
INFO: step 3 OFF kill 08 journal nonce 0x4c03710b, run 181ms
kill 09 on entry to pwrite64 t.db-journal off=8724 len=4096 [journal-record]
    on disk:  database pre-step bytes
              -journal 8724 bytes, first byte 0xd9 (hot), header no-sync (record count 0xffffffff: read to end of file), initial size 29 pages, sector 512, page 4096, 2 complete record(s) [2 1], 0 checksum failure(s), 4 byte(s) of an incomplete record
              SQLite on reopen rolls back: writes pages [2 1] from the journal, truncates to 29 pages
    reopened: pre-step, exact bytes (recipe step 2); integrity_check ok; no -journal
INFO: step 3 OFF kill 09 journal nonce 0x5f04fda7, run 232ms
kill 10 on entry to pwrite64 t.db-journal off=12820 len=4  [journal-record]
    on disk:  database pre-step bytes
              -journal 12820 bytes, first byte 0xd9 (hot), header no-sync (record count 0xffffffff: read to end of file), initial size 29 pages, sector 512, page 4096, 2 complete record(s) [2 1], 0 checksum failure(s), 4100 byte(s) of an incomplete record
              SQLite on reopen rolls back: writes pages [2 1] from the journal, truncates to 29 pages
    reopened: pre-step, exact bytes (recipe step 2); integrity_check ok; no -journal
INFO: step 3 OFF kill 10 journal nonce 0xc7f099c4, run 97ms
kill 11 on entry to pwrite64 t.db-journal off=12824 len=4  [journal-record]
    on disk:  database pre-step bytes
              -journal 12824 bytes, first byte 0xd9 (hot), header no-sync (record count 0xffffffff: read to end of file), initial size 29 pages, sector 512, page 4096, 3 complete record(s) [2 1 20], 0 checksum failure(s)
              SQLite on reopen rolls back: writes pages [2 1 20] from the journal, truncates to 29 pages
    reopened: pre-step, exact bytes (recipe step 2); integrity_check ok; no -journal
INFO: step 3 OFF kill 11 journal nonce 0xda3a6054, run 227ms
kill 12 on entry to pwrite64 t.db-journal off=12828 len=4096 [journal-record]
    on disk:  database pre-step bytes
              -journal 12828 bytes, first byte 0xd9 (hot), header no-sync (record count 0xffffffff: read to end of file), initial size 29 pages, sector 512, page 4096, 3 complete record(s) [2 1 20], 0 checksum failure(s), 4 byte(s) of an incomplete record
              SQLite on reopen rolls back: writes pages [2 1 20] from the journal, truncates to 29 pages
    reopened: pre-step, exact bytes (recipe step 2); integrity_check ok; no -journal
INFO: step 3 OFF kill 12 journal nonce 0xf1070e1b, run 111ms
kill 13 on entry to pwrite64 t.db-journal off=16924 len=4  [journal-record]
    on disk:  database pre-step bytes
              -journal 16924 bytes, first byte 0xd9 (hot), header no-sync (record count 0xffffffff: read to end of file), initial size 29 pages, sector 512, page 4096, 3 complete record(s) [2 1 20], 0 checksum failure(s), 4100 byte(s) of an incomplete record
              SQLite on reopen rolls back: writes pages [2 1 20] from the journal, truncates to 29 pages
    reopened: pre-step, exact bytes (recipe step 2); integrity_check ok; no -journal
INFO: step 3 OFF kill 13 journal nonce 0x20a0cd2b, run 85ms
kill 14 on entry to pwrite64 t.db-journal off=16928 len=4  [journal-record]
    on disk:  database pre-step bytes
              -journal 16928 bytes, first byte 0xd9 (hot), header no-sync (record count 0xffffffff: read to end of file), initial size 29 pages, sector 512, page 4096, 4 complete record(s) [2 1 20 3], 0 checksum failure(s)
              SQLite on reopen rolls back: writes pages [2 1 20 3] from the journal, truncates to 29 pages
    reopened: pre-step, exact bytes (recipe step 2); integrity_check ok; no -journal
INFO: step 3 OFF kill 14 journal nonce 0xa7a7e961, run 87ms
kill 15 on entry to pwrite64 t.db-journal off=16932 len=4096 [journal-record]
    on disk:  database pre-step bytes
              -journal 16932 bytes, first byte 0xd9 (hot), header no-sync (record count 0xffffffff: read to end of file), initial size 29 pages, sector 512, page 4096, 4 complete record(s) [2 1 20 3], 0 checksum failure(s), 4 byte(s) of an incomplete record
              SQLite on reopen rolls back: writes pages [2 1 20 3] from the journal, truncates to 29 pages
    reopened: pre-step, exact bytes (recipe step 2); integrity_check ok; no -journal
INFO: step 3 OFF kill 15 journal nonce 0x169b3c65, run 223ms
kill 16 on entry to pwrite64 t.db-journal off=21028 len=4  [journal-record]
    on disk:  database pre-step bytes
              -journal 21028 bytes, first byte 0xd9 (hot), header no-sync (record count 0xffffffff: read to end of file), initial size 29 pages, sector 512, page 4096, 4 complete record(s) [2 1 20 3], 0 checksum failure(s), 4100 byte(s) of an incomplete record
              SQLite on reopen rolls back: writes pages [2 1 20 3] from the journal, truncates to 29 pages
    reopened: pre-step, exact bytes (recipe step 2); integrity_check ok; no -journal
INFO: step 3 OFF kill 16 journal nonce 0x8f8ce055, run 216ms
kill 17 on entry to pwrite64 t.db-journal off=21032 len=4  [journal-record]
    on disk:  database pre-step bytes
              -journal 21032 bytes, first byte 0xd9 (hot), header no-sync (record count 0xffffffff: read to end of file), initial size 29 pages, sector 512, page 4096, 5 complete record(s) [2 1 20 3 27], 0 checksum failure(s)
              SQLite on reopen rolls back: writes pages [2 1 20 3 27] from the journal, truncates to 29 pages
    reopened: pre-step, exact bytes (recipe step 2); integrity_check ok; no -journal
INFO: step 3 OFF kill 17 journal nonce 0x94206aa5, run 200ms
kill 18 on entry to pwrite64 t.db-journal off=21036 len=4096 [journal-record]
    on disk:  database pre-step bytes
              -journal 21036 bytes, first byte 0xd9 (hot), header no-sync (record count 0xffffffff: read to end of file), initial size 29 pages, sector 512, page 4096, 5 complete record(s) [2 1 20 3 27], 0 checksum failure(s), 4 byte(s) of an incomplete record
              SQLite on reopen rolls back: writes pages [2 1 20 3 27] from the journal, truncates to 29 pages
    reopened: pre-step, exact bytes (recipe step 2); integrity_check ok; no -journal
INFO: step 3 OFF kill 18 journal nonce 0xe62c2204, run 156ms
kill 19 on entry to pwrite64 t.db-journal off=25132 len=4  [journal-record]
    on disk:  database pre-step bytes
              -journal 25132 bytes, first byte 0xd9 (hot), header no-sync (record count 0xffffffff: read to end of file), initial size 29 pages, sector 512, page 4096, 5 complete record(s) [2 1 20 3 27], 0 checksum failure(s), 4100 byte(s) of an incomplete record
              SQLite on reopen rolls back: writes pages [2 1 20 3 27] from the journal, truncates to 29 pages
    reopened: pre-step, exact bytes (recipe step 2); integrity_check ok; no -journal
INFO: step 3 OFF kill 19 journal nonce 0xedfe0c5c, run 226ms
kill 20 on entry to pwrite64 t.db-journal off=25136 len=4  [journal-record]
    on disk:  database pre-step bytes
              -journal 25136 bytes, first byte 0xd9 (hot), header no-sync (record count 0xffffffff: read to end of file), initial size 29 pages, sector 512, page 4096, 6 complete record(s) [2 1 20 3 27 4], 0 checksum failure(s)
              SQLite on reopen rolls back: writes pages [2 1 20 3 27 4] from the journal, truncates to 29 pages
    reopened: pre-step, exact bytes (recipe step 2); integrity_check ok; no -journal
INFO: step 3 OFF kill 20 journal nonce 0x522666de, run 230ms
kill 21 on entry to pwrite64 t.db-journal off=25140 len=4096 [journal-record]
    on disk:  database pre-step bytes
              -journal 25140 bytes, first byte 0xd9 (hot), header no-sync (record count 0xffffffff: read to end of file), initial size 29 pages, sector 512, page 4096, 6 complete record(s) [2 1 20 3 27 4], 0 checksum failure(s), 4 byte(s) of an incomplete record
              SQLite on reopen rolls back: writes pages [2 1 20 3 27 4] from the journal, truncates to 29 pages
    reopened: pre-step, exact bytes (recipe step 2); integrity_check ok; no -journal
INFO: step 3 OFF kill 21 journal nonce 0x2e6d506e, run 271ms
kill 22 on entry to pwrite64 t.db-journal off=29236 len=4  [journal-record]
    on disk:  database pre-step bytes
              -journal 29236 bytes, first byte 0xd9 (hot), header no-sync (record count 0xffffffff: read to end of file), initial size 29 pages, sector 512, page 4096, 6 complete record(s) [2 1 20 3 27 4], 0 checksum failure(s), 4100 byte(s) of an incomplete record
              SQLite on reopen rolls back: writes pages [2 1 20 3 27 4] from the journal, truncates to 29 pages
    reopened: pre-step, exact bytes (recipe step 2); integrity_check ok; no -journal
INFO: step 3 OFF kill 22 journal nonce 0xe585ae12, run 285ms
kill 23 on entry to pwrite64 t.db off=0 len=4096           [db-page]
    on disk:  database pre-step bytes
              -journal 29240 bytes, first byte 0xd9 (hot), header no-sync (record count 0xffffffff: read to end of file), initial size 29 pages, sector 512, page 4096, 7 complete record(s) [2 1 20 3 27 4 26], 0 checksum failure(s)
              SQLite on reopen rolls back: writes pages [2 1 20 3 27 4 26] from the journal, truncates to 29 pages
    reopened: pre-step, exact bytes (recipe step 2); integrity_check ok; no -journal
INFO: step 3 OFF kill 23 journal nonce 0x51e3305c, run 216ms
kill 24 on entry to pwrite64 t.db off=4096 len=4096        [db-page]
    on disk:  database torn: 29 pages, 1 differ from pre-step, 17 differ from post-step
              -journal 29240 bytes, first byte 0xd9 (hot), header no-sync (record count 0xffffffff: read to end of file), initial size 29 pages, sector 512, page 4096, 7 complete record(s) [2 1 20 3 27 4 26], 0 checksum failure(s)
              SQLite on reopen rolls back: writes pages [2 1 20 3 27 4 26] from the journal, truncates to 29 pages
    reopened: pre-step, exact bytes (recipe step 2); integrity_check ok; no -journal
INFO: step 3 OFF kill 24 journal nonce 0x3f9ee19b, run 175ms
kill 25 on entry to pwrite64 t.db off=8192 len=4096        [db-page]
    on disk:  database torn: 29 pages, 2 differ from pre-step, 16 differ from post-step
              -journal 29240 bytes, first byte 0xd9 (hot), header no-sync (record count 0xffffffff: read to end of file), initial size 29 pages, sector 512, page 4096, 7 complete record(s) [2 1 20 3 27 4 26], 0 checksum failure(s)
              SQLite on reopen rolls back: writes pages [2 1 20 3 27 4 26] from the journal, truncates to 29 pages
    reopened: pre-step, exact bytes (recipe step 2); integrity_check ok; no -journal
INFO: step 3 OFF kill 25 journal nonce 0x07109a10, run 125ms
kill 26 on entry to pwrite64 t.db off=12288 len=4096       [db-page]
    on disk:  database torn: 29 pages, 3 differ from pre-step, 15 differ from post-step
              -journal 29240 bytes, first byte 0xd9 (hot), header no-sync (record count 0xffffffff: read to end of file), initial size 29 pages, sector 512, page 4096, 7 complete record(s) [2 1 20 3 27 4 26], 0 checksum failure(s)
              SQLite on reopen rolls back: writes pages [2 1 20 3 27 4 26] from the journal, truncates to 29 pages
    reopened: pre-step, exact bytes (recipe step 2); integrity_check ok; no -journal
INFO: step 3 OFF kill 26 journal nonce 0x835f9040, run 271ms
kill 27 on entry to pwrite64 t.db off=77824 len=4096       [db-page]
    on disk:  database torn: 29 pages, 4 differ from pre-step, 14 differ from post-step
              -journal 29240 bytes, first byte 0xd9 (hot), header no-sync (record count 0xffffffff: read to end of file), initial size 29 pages, sector 512, page 4096, 7 complete record(s) [2 1 20 3 27 4 26], 0 checksum failure(s)
              SQLite on reopen rolls back: writes pages [2 1 20 3 27 4 26] from the journal, truncates to 29 pages
    reopened: pre-step, exact bytes (recipe step 2); integrity_check ok; no -journal
INFO: step 3 OFF kill 27 journal nonce 0x2e7cfb3b, run 177ms
kill 28 on entry to pwrite64 t.db off=86016 len=4096       [db-page]
    on disk:  database torn: 29 pages, 5 differ from pre-step, 13 differ from post-step
              -journal 29240 bytes, first byte 0xd9 (hot), header no-sync (record count 0xffffffff: read to end of file), initial size 29 pages, sector 512, page 4096, 7 complete record(s) [2 1 20 3 27 4 26], 0 checksum failure(s)
              SQLite on reopen rolls back: writes pages [2 1 20 3 27 4 26] from the journal, truncates to 29 pages
    reopened: pre-step, exact bytes (recipe step 2); integrity_check ok; no -journal
INFO: step 3 OFF kill 28 journal nonce 0x302d7013, run 196ms
kill 29 on entry to pwrite64 t.db off=102400 len=4096      [db-page]
    on disk:  database torn: 29 pages, 6 differ from pre-step, 12 differ from post-step
              -journal 29240 bytes, first byte 0xd9 (hot), header no-sync (record count 0xffffffff: read to end of file), initial size 29 pages, sector 512, page 4096, 7 complete record(s) [2 1 20 3 27 4 26], 0 checksum failure(s)
              SQLite on reopen rolls back: writes pages [2 1 20 3 27 4 26] from the journal, truncates to 29 pages
    reopened: pre-step except page(s) [22], which are freelist leaves in the pre-step file (unused bytes); VACUUM INTO output equals the pre-step's; integrity_check ok; no -journal
INFO: step 3 OFF kill 29 journal nonce 0x775ad0fc, run 193ms
kill 30 on entry to pwrite64 t.db off=106496 len=4096      [db-page]
    on disk:  database torn: 29 pages, 7 differ from pre-step, 11 differ from post-step
              -journal 29240 bytes, first byte 0xd9 (hot), header no-sync (record count 0xffffffff: read to end of file), initial size 29 pages, sector 512, page 4096, 7 complete record(s) [2 1 20 3 27 4 26], 0 checksum failure(s)
              SQLite on reopen rolls back: writes pages [2 1 20 3 27 4 26] from the journal, truncates to 29 pages
    reopened: pre-step except page(s) [22], which are freelist leaves in the pre-step file (unused bytes); VACUUM INTO output equals the pre-step's; integrity_check ok; no -journal
INFO: step 3 OFF kill 30 journal nonce 0xd925991e, run 163ms
kill 31 on entry to pwrite64 t.db off=118784 len=4096      [db-page]
    on disk:  database torn: 29 pages, 8 differ from pre-step, 10 differ from post-step
              -journal 29240 bytes, first byte 0xd9 (hot), header no-sync (record count 0xffffffff: read to end of file), initial size 29 pages, sector 512, page 4096, 7 complete record(s) [2 1 20 3 27 4 26], 0 checksum failure(s)
              SQLite on reopen rolls back: writes pages [2 1 20 3 27 4 26] from the journal, truncates to 29 pages
    reopened: pre-step except page(s) [22], which are freelist leaves in the pre-step file (unused bytes); VACUUM INTO output equals the pre-step's; integrity_check ok; no -journal
INFO: step 3 OFF kill 31 journal nonce 0x8d6c6f08, run 127ms
kill 32 on entry to pwrite64 t.db off=122880 len=4096      [db-page]
    on disk:  database torn: 30 pages, 9 differ from pre-step, 9 differ from post-step
              -journal 29240 bytes, first byte 0xd9 (hot), header no-sync (record count 0xffffffff: read to end of file), initial size 29 pages, sector 512, page 4096, 7 complete record(s) [2 1 20 3 27 4 26], 0 checksum failure(s)
              SQLite on reopen rolls back: writes pages [2 1 20 3 27 4 26] from the journal, truncates to 29 pages
    reopened: pre-step except page(s) [22], which are freelist leaves in the pre-step file (unused bytes); VACUUM INTO output equals the pre-step's; integrity_check ok; no -journal
INFO: step 3 OFF kill 32 journal nonce 0x982be1f0, run 380ms
kill 33 on entry to pwrite64 t.db off=126976 len=4096      [db-page]
    on disk:  database torn: 31 pages, 10 differ from pre-step, 8 differ from post-step
              -journal 29240 bytes, first byte 0xd9 (hot), header no-sync (record count 0xffffffff: read to end of file), initial size 29 pages, sector 512, page 4096, 7 complete record(s) [2 1 20 3 27 4 26], 0 checksum failure(s)
              SQLite on reopen rolls back: writes pages [2 1 20 3 27 4 26] from the journal, truncates to 29 pages
    reopened: pre-step except page(s) [22], which are freelist leaves in the pre-step file (unused bytes); VACUUM INTO output equals the pre-step's; integrity_check ok; no -journal
INFO: step 3 OFF kill 33 journal nonce 0x5cb602cb, run 161ms
kill 34 on entry to pwrite64 t.db off=131072 len=4096      [db-page]
    on disk:  database torn: 32 pages, 11 differ from pre-step, 7 differ from post-step
              -journal 29240 bytes, first byte 0xd9 (hot), header no-sync (record count 0xffffffff: read to end of file), initial size 29 pages, sector 512, page 4096, 7 complete record(s) [2 1 20 3 27 4 26], 0 checksum failure(s)
              SQLite on reopen rolls back: writes pages [2 1 20 3 27 4 26] from the journal, truncates to 29 pages
    reopened: pre-step except page(s) [22], which are freelist leaves in the pre-step file (unused bytes); VACUUM INTO output equals the pre-step's; integrity_check ok; no -journal
INFO: step 3 OFF kill 34 journal nonce 0x9158dce2, run 299ms
kill 35 on entry to pwrite64 t.db off=135168 len=4096      [db-page]
    on disk:  database torn: 33 pages, 12 differ from pre-step, 6 differ from post-step
              -journal 29240 bytes, first byte 0xd9 (hot), header no-sync (record count 0xffffffff: read to end of file), initial size 29 pages, sector 512, page 4096, 7 complete record(s) [2 1 20 3 27 4 26], 0 checksum failure(s)
              SQLite on reopen rolls back: writes pages [2 1 20 3 27 4 26] from the journal, truncates to 29 pages
    reopened: pre-step except page(s) [22], which are freelist leaves in the pre-step file (unused bytes); VACUUM INTO output equals the pre-step's; integrity_check ok; no -journal
INFO: step 3 OFF kill 35 journal nonce 0x2a3766f3, run 217ms
kill 36 on entry to pwrite64 t.db off=139264 len=4096      [db-page]
    on disk:  database torn: 34 pages, 13 differ from pre-step, 5 differ from post-step
              -journal 29240 bytes, first byte 0xd9 (hot), header no-sync (record count 0xffffffff: read to end of file), initial size 29 pages, sector 512, page 4096, 7 complete record(s) [2 1 20 3 27 4 26], 0 checksum failure(s)
              SQLite on reopen rolls back: writes pages [2 1 20 3 27 4 26] from the journal, truncates to 29 pages
    reopened: pre-step except page(s) [22], which are freelist leaves in the pre-step file (unused bytes); VACUUM INTO output equals the pre-step's; integrity_check ok; no -journal
INFO: step 3 OFF kill 36 journal nonce 0xf269851f, run 198ms
kill 37 on entry to pwrite64 t.db off=143360 len=4096      [db-page]
    on disk:  database torn: 35 pages, 14 differ from pre-step, 4 differ from post-step
              -journal 29240 bytes, first byte 0xd9 (hot), header no-sync (record count 0xffffffff: read to end of file), initial size 29 pages, sector 512, page 4096, 7 complete record(s) [2 1 20 3 27 4 26], 0 checksum failure(s)
              SQLite on reopen rolls back: writes pages [2 1 20 3 27 4 26] from the journal, truncates to 29 pages
    reopened: pre-step except page(s) [22], which are freelist leaves in the pre-step file (unused bytes); VACUUM INTO output equals the pre-step's; integrity_check ok; no -journal
INFO: step 3 OFF kill 37 journal nonce 0xe7b5a206, run 273ms
kill 38 on entry to pwrite64 t.db off=147456 len=4096      [db-page]
    on disk:  database torn: 36 pages, 15 differ from pre-step, 3 differ from post-step
              -journal 29240 bytes, first byte 0xd9 (hot), header no-sync (record count 0xffffffff: read to end of file), initial size 29 pages, sector 512, page 4096, 7 complete record(s) [2 1 20 3 27 4 26], 0 checksum failure(s)
              SQLite on reopen rolls back: writes pages [2 1 20 3 27 4 26] from the journal, truncates to 29 pages
    reopened: pre-step except page(s) [22], which are freelist leaves in the pre-step file (unused bytes); VACUUM INTO output equals the pre-step's; integrity_check ok; no -journal
INFO: step 3 OFF kill 38 journal nonce 0x947b1093, run 256ms
kill 39 on entry to pwrite64 t.db off=151552 len=4096      [db-page]
    on disk:  database torn: 37 pages, 16 differ from pre-step, 2 differ from post-step
              -journal 29240 bytes, first byte 0xd9 (hot), header no-sync (record count 0xffffffff: read to end of file), initial size 29 pages, sector 512, page 4096, 7 complete record(s) [2 1 20 3 27 4 26], 0 checksum failure(s)
              SQLite on reopen rolls back: writes pages [2 1 20 3 27 4 26] from the journal, truncates to 29 pages
    reopened: pre-step except page(s) [22], which are freelist leaves in the pre-step file (unused bytes); VACUUM INTO output equals the pre-step's; integrity_check ok; no -journal
INFO: step 3 OFF kill 39 journal nonce 0xf196e398, run 259ms
kill 40 on entry to pwrite64 t.db off=155648 len=4096      [db-page]
    on disk:  database torn: 38 pages, 17 differ from pre-step, 1 differ from post-step
              -journal 29240 bytes, first byte 0xd9 (hot), header no-sync (record count 0xffffffff: read to end of file), initial size 29 pages, sector 512, page 4096, 7 complete record(s) [2 1 20 3 27 4 26], 0 checksum failure(s)
              SQLite on reopen rolls back: writes pages [2 1 20 3 27 4 26] from the journal, truncates to 29 pages
    reopened: pre-step except page(s) [22], which are freelist leaves in the pre-step file (unused bytes); VACUUM INTO output equals the pre-step's; integrity_check ok; no -journal
INFO: step 3 OFF kill 40 journal nonce 0x6fd2da03, run 67ms
kill 41 on entry to unlink t.db-journal                    [journal-unlink]
    on disk:  database post-step bytes
              -journal 29240 bytes, first byte 0xd9 (hot), header no-sync (record count 0xffffffff: read to end of file), initial size 29 pages, sector 512, page 4096, 7 complete record(s) [2 1 20 3 27 4 26], 0 checksum failure(s)
              SQLite on reopen rolls back: writes pages [2 1 20 3 27 4 26] from the journal, truncates to 29 pages
    reopened: pre-step except page(s) [22], which are freelist leaves in the pre-step file (unused bytes); VACUUM INTO output equals the pre-step's; integrity_check ok; no -journal
INFO: step 3 OFF kill 41 journal nonce 0x05e488b1, run 222ms
kill 42 on entry to open marker-end                        [step-returned]
    on disk:  database post-step bytes
              no -journal
              SQLite on reopen nothing to roll back
    reopened: post-step, exact bytes (recipe step 3); integrity_check ok; no -journal
outcomes: 28 pre-step exact, 13 pre-step except reused freelist leaves, 1 post-step exact, 0 other
check ok: step 3, synchronous=OFF: every kill landed on the intended syscall (strace saw it unfinished and the child died of SIGKILL)
check ok: step 3, synchronous=OFF: every reopened database is the pre-step or the post-step state, never a third (pre-step allows reused freelist-leaf pages, see above)
check ok: step 3, synchronous=OFF: every kill up to and including the journal unlink gives pre-step; the kill after it gives post-step (the unlink is the commit point)
check ok: step 3, synchronous=OFF: integrity_check is ok after every reopen
check ok: step 3, synchronous=OFF: for every kill up to the commit point, whenever the database file differs from pre-step before reopen, the journal is hot and its records cover every changed page except pages appended past the original size (removed by truncation) and reused freelist leaves

== summary for step 3: reopened state per kill point, in commit order ==
synchronous=FULL   BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBbbbbbbbbbbbbbbA
synchronous=NORMAL BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBbbbbbbbbbbbbbbA
synchronous=OFF    BBBBBBBBBBBBBBBBBBBBBBBBBBBBbbbbbbbbbbbbbA
(B = pre-step exact bytes, b = pre-step except reused freelist leaves, A = post-step exact bytes, ? = anything else; the last B or b is the kill on entry to the journal unlink)

== D2. a step whose pre-step file has no freelist: step 1, every kill point, all three levels ==
step 1: schema=0 inserts=65 updates=6 deletes=0 indexes=0
pre-step state (after step 0): 4 pages, sha256 e551bbfca2c05cd9, freelist leaves []
post-step state (after step 1): 22 pages, sha256 6207fb74284d25e8
check ok: the pre-step and post-step files equal the recipe's hashes for steps 0 and 1

-- step 1, synchronous=FULL --
sequence: journal-header, journal-record x12, journal-sync, dir-sync, journal-count, journal-sync, db-page x22, db-sync, journal-unlink
journaled pages 4 (12 record writes), database page writes 22, syncs 4 [t.db-journal . t.db-journal t.db]
INFO: sync syscall used by the Go driver: fsync
check ok: step 1, journal_mode=delete, synchronous=FULL: order is journal-header, journal-record x12, journal-sync, dir-sync, journal-count, journal-sync, db-page x22, db-sync, journal-unlink
check ok: step 1, journal_mode=delete, synchronous=FULL: 4 syncs per commit (journal, directory and database syncs counted)
check ok: step 1, journal_mode=delete, synchronous=FULL: each journal record is three pwrite64 calls: page number (4 bytes), original page image (4096), checksum (4)
check ok: step 1, journal_mode=delete, synchronous=FULL: the traced commit leaves exactly the post-step bytes
kill points: 42 (every pwrite64, sync and unlink of the commit, then the first syscall after the step returned)
INFO: 42 kill runs took 2.442s with 4 workers
INFO: step 1 FULL kill 02 journal nonce 0x7c11adeb, run 219ms
INFO: step 1 FULL kill 03 journal nonce 0xf5ac25d6, run 84ms
INFO: step 1 FULL kill 04 journal nonce 0x92156157, run 142ms
INFO: step 1 FULL kill 05 journal nonce 0xb0407916, run 157ms
INFO: step 1 FULL kill 06 journal nonce 0xfbd85bf6, run 64ms
INFO: step 1 FULL kill 07 journal nonce 0xa2ecea52, run 87ms
INFO: step 1 FULL kill 08 journal nonce 0xf2cfb53d, run 116ms
INFO: step 1 FULL kill 09 journal nonce 0x397f533e, run 205ms
INFO: step 1 FULL kill 10 journal nonce 0x660841c0, run 120ms
INFO: step 1 FULL kill 11 journal nonce 0x76df42e5, run 68ms
INFO: step 1 FULL kill 12 journal nonce 0xb7701e63, run 105ms
INFO: step 1 FULL kill 13 journal nonce 0x11f4bcbf, run 111ms
INFO: step 1 FULL kill 14 journal nonce 0x95644f09, run 192ms
INFO: step 1 FULL kill 15 journal nonce 0x3ec385b1, run 162ms
INFO: step 1 FULL kill 16 journal nonce 0x3419d69e, run 186ms
INFO: step 1 FULL kill 17 journal nonce 0x66978411, run 122ms
INFO: step 1 FULL kill 18 journal nonce 0xac51b144, run 137ms
INFO: step 1 FULL kill 19 journal nonce 0x9b8749bc, run 126ms
INFO: step 1 FULL kill 20 journal nonce 0xea711dd4, run 121ms
INFO: step 1 FULL kill 21 journal nonce 0xab6f080b, run 187ms
INFO: step 1 FULL kill 22 journal nonce 0xa76bf054, run 210ms
INFO: step 1 FULL kill 23 journal nonce 0xcb74b902, run 159ms
INFO: step 1 FULL kill 24 journal nonce 0x4550e0e8, run 219ms
INFO: step 1 FULL kill 25 journal nonce 0x3e17c5af, run 80ms
INFO: step 1 FULL kill 26 journal nonce 0xa655de8d, run 294ms
INFO: step 1 FULL kill 27 journal nonce 0xd9f09d80, run 288ms
INFO: step 1 FULL kill 28 journal nonce 0x3427c17e, run 138ms
INFO: step 1 FULL kill 29 journal nonce 0x2182a097, run 103ms
INFO: step 1 FULL kill 30 journal nonce 0xa88db848, run 225ms
INFO: step 1 FULL kill 31 journal nonce 0x376d7fa8, run 140ms
INFO: step 1 FULL kill 32 journal nonce 0x92e9298b, run 188ms
INFO: step 1 FULL kill 33 journal nonce 0xa42aefae, run 230ms
INFO: step 1 FULL kill 34 journal nonce 0xba11f7c7, run 332ms
INFO: step 1 FULL kill 35 journal nonce 0x5c2bdf7c, run 234ms
INFO: step 1 FULL kill 36 journal nonce 0x24edef49, run 229ms
INFO: step 1 FULL kill 37 journal nonce 0xa6348b48, run 228ms
INFO: step 1 FULL kill 38 journal nonce 0xcffe6963, run 206ms
INFO: step 1 FULL kill 39 journal nonce 0x81518d52, run 304ms
INFO: step 1 FULL kill 40 journal nonce 0xfc6db764, run 176ms
INFO: step 1 FULL kill 41 journal nonce 0xe0735cff, run 288ms
outcomes: 41 pre-step exact, 0 pre-step except reused freelist leaves, 1 post-step exact, 0 other
check ok: step 1, synchronous=FULL: every kill landed on the intended syscall (strace saw it unfinished and the child died of SIGKILL)
check ok: step 1, synchronous=FULL: every reopened database is the pre-step or the post-step state, never a third (pre-step allows reused freelist-leaf pages, see above)
check ok: step 1, synchronous=FULL: the pre-step file has no freelist, and every reopened database equals the pre-step or post-step bytes exactly
check ok: step 1, synchronous=FULL: every kill up to and including the journal unlink gives pre-step; the kill after it gives post-step (the unlink is the commit point)
check ok: step 1, synchronous=FULL: integrity_check is ok after every reopen
check ok: step 1, synchronous=FULL: for every kill up to the commit point, whenever the database file differs from pre-step before reopen, the journal is hot and its records cover every changed page except pages appended past the original size (removed by truncation) and reused freelist leaves

-- step 1, synchronous=NORMAL --
sequence: journal-header, journal-record x12, journal-count, journal-sync, dir-sync, db-page x22, db-sync, journal-unlink
journaled pages 4 (12 record writes), database page writes 22, syncs 3 [t.db-journal . t.db]
INFO: sync syscall used by the Go driver: fsync
check ok: step 1, journal_mode=delete, synchronous=NORMAL: order is journal-header, journal-record x12, journal-count, journal-sync, dir-sync, db-page x22, db-sync, journal-unlink
check ok: step 1, journal_mode=delete, synchronous=NORMAL: 3 syncs per commit (journal, directory and database syncs counted)
check ok: step 1, journal_mode=delete, synchronous=NORMAL: each journal record is three pwrite64 calls: page number (4 bytes), original page image (4096), checksum (4)
check ok: step 1, journal_mode=delete, synchronous=NORMAL: the traced commit leaves exactly the post-step bytes
kill points: 41 (every pwrite64, sync and unlink of the commit, then the first syscall after the step returned)
INFO: 41 kill runs took 3.289s with 4 workers
INFO: step 1 NORMAL kill 02 journal nonce 0xada15aa8, run 246ms
INFO: step 1 NORMAL kill 03 journal nonce 0xa1e4fd21, run 147ms
INFO: step 1 NORMAL kill 04 journal nonce 0x24b50a4f, run 288ms
INFO: step 1 NORMAL kill 05 journal nonce 0x5a44005f, run 404ms
INFO: step 1 NORMAL kill 06 journal nonce 0x603091ee, run 294ms
INFO: step 1 NORMAL kill 07 journal nonce 0x43d5be83, run 225ms
INFO: step 1 NORMAL kill 08 journal nonce 0x22fc062f, run 155ms
INFO: step 1 NORMAL kill 09 journal nonce 0x7dad5b8b, run 296ms
INFO: step 1 NORMAL kill 10 journal nonce 0x1855e7d4, run 146ms
INFO: step 1 NORMAL kill 11 journal nonce 0x7e9db7a1, run 339ms
INFO: step 1 NORMAL kill 12 journal nonce 0xf3075a4b, run 271ms
INFO: step 1 NORMAL kill 13 journal nonce 0xdad51718, run 141ms
INFO: step 1 NORMAL kill 14 journal nonce 0xd885ab91, run 315ms
INFO: step 1 NORMAL kill 15 journal nonce 0x31e59d22, run 499ms
INFO: step 1 NORMAL kill 16 journal nonce 0x74fef789, run 146ms
INFO: step 1 NORMAL kill 17 journal nonce 0x99b440bd, run 276ms
INFO: step 1 NORMAL kill 18 journal nonce 0xf563a7f4, run 128ms
INFO: step 1 NORMAL kill 19 journal nonce 0xa4b1a1a1, run 172ms
INFO: step 1 NORMAL kill 20 journal nonce 0xcc4cb938, run 456ms
INFO: step 1 NORMAL kill 21 journal nonce 0x44fac056, run 159ms
INFO: step 1 NORMAL kill 22 journal nonce 0xc17023d2, run 184ms
INFO: step 1 NORMAL kill 23 journal nonce 0x0ac82838, run 193ms
INFO: step 1 NORMAL kill 24 journal nonce 0x458faa93, run 267ms
INFO: step 1 NORMAL kill 25 journal nonce 0x5c967480, run 310ms
INFO: step 1 NORMAL kill 26 journal nonce 0x86242d9b, run 295ms
INFO: step 1 NORMAL kill 27 journal nonce 0xa01a64bb, run 411ms
INFO: step 1 NORMAL kill 28 journal nonce 0xd7a6c9ed, run 317ms
INFO: step 1 NORMAL kill 29 journal nonce 0xf3de73c8, run 303ms
INFO: step 1 NORMAL kill 30 journal nonce 0x5bac35a4, run 199ms
INFO: step 1 NORMAL kill 31 journal nonce 0x83e5d3ee, run 132ms
INFO: step 1 NORMAL kill 32 journal nonce 0x9fc49219, run 295ms
INFO: step 1 NORMAL kill 33 journal nonce 0xe165b704, run 251ms
INFO: step 1 NORMAL kill 34 journal nonce 0xb5434bbd, run 194ms
INFO: step 1 NORMAL kill 35 journal nonce 0xfb27792f, run 216ms
INFO: step 1 NORMAL kill 36 journal nonce 0xc71e8e65, run 205ms
INFO: step 1 NORMAL kill 37 journal nonce 0x7828a20b, run 193ms
INFO: step 1 NORMAL kill 38 journal nonce 0x449d9fa2, run 184ms
INFO: step 1 NORMAL kill 39 journal nonce 0x8593aba8, run 162ms
INFO: step 1 NORMAL kill 40 journal nonce 0x6be0e5f4, run 343ms
outcomes: 40 pre-step exact, 0 pre-step except reused freelist leaves, 1 post-step exact, 0 other
check ok: step 1, synchronous=NORMAL: every kill landed on the intended syscall (strace saw it unfinished and the child died of SIGKILL)
check ok: step 1, synchronous=NORMAL: every reopened database is the pre-step or the post-step state, never a third (pre-step allows reused freelist-leaf pages, see above)
check ok: step 1, synchronous=NORMAL: the pre-step file has no freelist, and every reopened database equals the pre-step or post-step bytes exactly
check ok: step 1, synchronous=NORMAL: every kill up to and including the journal unlink gives pre-step; the kill after it gives post-step (the unlink is the commit point)
check ok: step 1, synchronous=NORMAL: integrity_check is ok after every reopen
check ok: step 1, synchronous=NORMAL: for every kill up to the commit point, whenever the database file differs from pre-step before reopen, the journal is hot and its records cover every changed page except pages appended past the original size (removed by truncation) and reused freelist leaves

-- step 1, synchronous=OFF --
sequence: journal-header, journal-record x12, db-page x22, journal-unlink
journaled pages 4 (12 record writes), database page writes 22, syncs 0 []
INFO: sync syscall used by the Go driver: (none)
check ok: step 1, journal_mode=delete, synchronous=OFF: order is journal-header, journal-record x12, db-page x22, journal-unlink
check ok: step 1, journal_mode=delete, synchronous=OFF: 0 syncs per commit (journal, directory and database syncs counted)
check ok: step 1, journal_mode=delete, synchronous=OFF: each journal record is three pwrite64 calls: page number (4 bytes), original page image (4096), checksum (4)
check ok: step 1, journal_mode=delete, synchronous=OFF: the traced commit leaves exactly the post-step bytes
kill points: 37 (every pwrite64, sync and unlink of the commit, then the first syscall after the step returned)
INFO: 37 kill runs took 2.512s with 4 workers
INFO: step 1 OFF kill 02 journal nonce 0xccb9b413, run 155ms
INFO: step 1 OFF kill 03 journal nonce 0x0e6862e8, run 152ms
INFO: step 1 OFF kill 04 journal nonce 0x60196006, run 256ms
INFO: step 1 OFF kill 05 journal nonce 0x189be526, run 112ms
INFO: step 1 OFF kill 06 journal nonce 0xaafdc307, run 126ms
INFO: step 1 OFF kill 07 journal nonce 0x916b6056, run 194ms
INFO: step 1 OFF kill 08 journal nonce 0x1f578149, run 194ms
INFO: step 1 OFF kill 09 journal nonce 0x89c4d975, run 144ms
INFO: step 1 OFF kill 10 journal nonce 0x3baca44f, run 122ms
INFO: step 1 OFF kill 11 journal nonce 0x684c2afc, run 108ms
INFO: step 1 OFF kill 12 journal nonce 0xab83dce2, run 179ms
INFO: step 1 OFF kill 13 journal nonce 0x80ee9036, run 508ms
INFO: step 1 OFF kill 14 journal nonce 0xbf335337, run 492ms
INFO: step 1 OFF kill 15 journal nonce 0x5a114a38, run 239ms
INFO: step 1 OFF kill 16 journal nonce 0xaff8786f, run 195ms
INFO: step 1 OFF kill 17 journal nonce 0xd1d9cfee, run 248ms
INFO: step 1 OFF kill 18 journal nonce 0x58e9ec98, run 136ms
INFO: step 1 OFF kill 19 journal nonce 0x459ee930, run 137ms
INFO: step 1 OFF kill 20 journal nonce 0xe0d098f5, run 266ms
INFO: step 1 OFF kill 21 journal nonce 0x6d1fad98, run 114ms
INFO: step 1 OFF kill 22 journal nonce 0x0427693b, run 203ms
INFO: step 1 OFF kill 23 journal nonce 0xd777a335, run 86ms
INFO: step 1 OFF kill 24 journal nonce 0x3d50d712, run 295ms
INFO: step 1 OFF kill 25 journal nonce 0xa021fa96, run 290ms
INFO: step 1 OFF kill 26 journal nonce 0x8b73692f, run 176ms
INFO: step 1 OFF kill 27 journal nonce 0xccd5d0be, run 97ms
INFO: step 1 OFF kill 28 journal nonce 0xaf9cfdda, run 349ms
INFO: step 1 OFF kill 29 journal nonce 0xec80da12, run 327ms
INFO: step 1 OFF kill 30 journal nonce 0x46b78445, run 249ms
INFO: step 1 OFF kill 31 journal nonce 0x51f56279, run 94ms
INFO: step 1 OFF kill 32 journal nonce 0xba9b86e3, run 167ms
INFO: step 1 OFF kill 33 journal nonce 0xba6d2f2c, run 236ms
INFO: step 1 OFF kill 34 journal nonce 0xb8a08afd, run 198ms
INFO: step 1 OFF kill 35 journal nonce 0x5c2bf5b7, run 229ms
INFO: step 1 OFF kill 36 journal nonce 0x73351f95, run 143ms
outcomes: 36 pre-step exact, 0 pre-step except reused freelist leaves, 1 post-step exact, 0 other
check ok: step 1, synchronous=OFF: every kill landed on the intended syscall (strace saw it unfinished and the child died of SIGKILL)
check ok: step 1, synchronous=OFF: every reopened database is the pre-step or the post-step state, never a third (pre-step allows reused freelist-leaf pages, see above)
check ok: step 1, synchronous=OFF: the pre-step file has no freelist, and every reopened database equals the pre-step or post-step bytes exactly
check ok: step 1, synchronous=OFF: every kill up to and including the journal unlink gives pre-step; the kill after it gives post-step (the unlink is the commit point)
check ok: step 1, synchronous=OFF: integrity_check is ok after every reopen
check ok: step 1, synchronous=OFF: for every kill up to the commit point, whenever the database file differs from pre-step before reopen, the journal is hot and its records cover every changed page except pages appended past the original size (removed by truncation) and reused freelist leaves

== summary for step 1: reopened state per kill point, in commit order ==
synchronous=FULL   BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBA
synchronous=NORMAL BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBA
synchronous=OFF    BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBA
(B = pre-step exact bytes, b = pre-step except reused freelist leaves, A = post-step exact bytes, ? = anything else; the last B or b is the kill on entry to the journal unlink)

== E. failure cases: losing the hot journal (step 3, synchronous=FULL) ==
contents: pre-step user_version=2 accounts=15 documents=7 events=108; post-step user_version=3 accounts=21 documents=10 events=147
crash state: killed on entry to pwrite64 t.db off=122880 len=4096 (database write 10 of 18)
on disk: database torn: 30 pages, 9 differ from pre-step, 9 differ from post-step
         -journal 29240 bytes, first byte 0xd9 (hot), header synced (record count 7), initial size 29 pages, sector 512, page 4096, 7 complete record(s) [2 1 20 3 27 4 26], 0 checksum failure(s)
1. -journal deleted, then reopened: open failed: open t.db: PRAGMA page_count: database disk image is malformed (11)
   state: unreadable
check ok: deleting the hot journal before reopening leaves a damaged database that is neither the pre-step nor the post-step state
2. the database file copied without its -journal, copy opened: open failed: open t.db: PRAGMA page_count: database disk image is malformed (11)
   state: unreadable
check ok: a copy made without the -journal is the same damaged database
   the same crashed bytes copied together with their -journal, opened: integrity_check: ok; user_version=2 accounts=15 documents=7 events=108
   state: pre-step except page(s) [22], which are freelist leaves in the pre-step file (unused bytes); VACUUM INTO output equals the pre-step's
check ok: the same crashed file copied together with its -journal rolls back to pre-step
3. killed on entry to sync t.db (every database page written, journal still hot): database post-step bytes
   -journal deleted, then reopened: integrity_check: ok; user_version=3 accounts=21 documents=10 events=147
   state: post-step, exact bytes (recipe step 3)
check ok: deleting the journal after the last database write turns a commit that never returned into a committed one (post-step)

== F. the commit point in DELETE, TRUNCATE and PERSIST journal modes (step 3, synchronous=FULL) ==
-- journal_mode=delete --
sequence: journal-header, journal-record x21, journal-sync, dir-sync, journal-count, journal-sync, db-page x18, db-sync, journal-unlink
journaled pages 7 (21 record writes), database page writes 18, syncs 4 [t.db-journal . t.db-journal t.db]
INFO: sync syscall used by the Go driver: fsync
check ok: step 3, journal_mode=delete, synchronous=FULL: order is journal-header, journal-record x21, journal-sync, dir-sync, journal-count, journal-sync, db-page x18, db-sync, journal-unlink
check ok: step 3, journal_mode=delete, synchronous=FULL: 4 syncs per commit (journal, directory and database syncs counted)
check ok: step 3, journal_mode=delete, synchronous=FULL: each journal record is three pwrite64 calls: page number (4 bytes), original page image (4096), checksum (4)
check ok: step 3, journal_mode=delete, synchronous=FULL: the traced commit leaves exactly the post-step bytes
check ok: delete: a kill on entry to the commit point gives pre-step, the next kill gives post-step
-- journal_mode=truncate --
sequence: journal-header, journal-record x21, journal-sync, dir-sync, journal-count, journal-sync, db-page x18, db-sync, journal-truncate, journal-sync
journaled pages 7 (21 record writes), database page writes 18, syncs 5 [t.db-journal . t.db-journal t.db t.db-journal]
INFO: sync syscall used by the Go driver: fsync
check ok: step 3, journal_mode=truncate, synchronous=FULL: order is journal-header, journal-record x21, journal-sync, dir-sync, journal-count, journal-sync, db-page x18, db-sync, journal-truncate, journal-sync
check ok: step 3, journal_mode=truncate, synchronous=FULL: each journal record is three pwrite64 calls: page number (4 bytes), original page image (4096), checksum (4)
check ok: step 3, journal_mode=truncate, synchronous=FULL: the traced commit leaves exactly the post-step bytes
check ok: truncate: a kill on entry to the commit point gives pre-step, the next kill gives post-step
check ok: truncate: the journal file stays beside the database after a clean commit, and it is not hot
-- journal_mode=persist --
sequence: journal-header, journal-record x21, journal-sync, dir-sync, journal-count, journal-sync, db-page x18, db-sync, journal-zero-header, journal-sync
journaled pages 7 (21 record writes), database page writes 18, syncs 5 [t.db-journal . t.db-journal t.db t.db-journal]
INFO: sync syscall used by the Go driver: fsync
check ok: step 3, journal_mode=persist, synchronous=FULL: order is journal-header, journal-record x21, journal-sync, dir-sync, journal-count, journal-sync, db-page x18, db-sync, journal-zero-header, journal-sync
check ok: step 3, journal_mode=persist, synchronous=FULL: each journal record is three pwrite64 calls: page number (4 bytes), original page image (4096), checksum (4)
check ok: step 3, journal_mode=persist, synchronous=FULL: the traced commit leaves exactly the post-step bytes
check ok: persist: a kill on entry to the commit point gives pre-step, the next kill gives post-step
check ok: persist: the journal file stays beside the database after a clean commit, and it is not hot
mode      commit point (syscall)             killed on entry            killed on the next syscall                   -journal after a clean commit
delete    unlink t.db-journal                before (pre-step, logical) after (post-step) (kill on step-returned)    no -journal
truncate  ftruncate t.db-journal len=0       before (pre-step, logical) after (post-step) (kill on journal-sync)     -journal 0 bytes (not hot)
persist   pwrite64 t.db-journal off=0 len=28 before (pre-step, logical) after (post-step) (kill on journal-sync)     -journal 29240 bytes, first byte 0x00 (not hot), header zeroed
INFO: runtime 23.2s

ATOMIC COMMIT OK
```

#### Reading the evidence (Parts A–D2)

Parts E and F are analyzed in Section 6 alongside the hot-journal failure cases. Here is what Parts A through D2 establish:

- **Lines 5–20 (Part A: where a WAL-default platform meets rollback mode).** Opening a new path with plain `sql.Open("sqlite", path)` reports `journal_mode=delete`, and its first `CREATE TABLE` writes `1,1` into header bytes 18–19. Running `PRAGMA journal_mode=wal` rewrites bytes 18–19 to `2,2` so a fresh connection reports `journal_mode=wal`. The three traced children prove **correction 4**: not only does a driver-default `CREATE TABLE` go through `plain.db-journal`, but `PRAGMA journal_mode=wal` on an empty file (`switch.db-journal`) and `sqlitex.Open` with `journal_mode=wal` (`platform.db-journal`) each run the exact same six `-journal` syscalls (`open`, `pwrite64 off=0 len=512`, `sync`, `pwrite64 off=0 len=12`, `sync`, `unlink`). The journal holds zero page records because page 1 is past the 0-page initial file size (`pager_write`, `src/pager.c:6139-6147`). Only *after* page 1 records format version `2` do commits move to `-wal` (`-journal []`; `-wal writes 5` for `sqlitex.Open`'s `application_id`/`user_version` commit and `4` for the subsequent `CREATE TABLE`).
- **Lines 22–27 (Part B: the recipe step).** Step 3 of the rollback-mode recipe (`seed=1`) executes 53 inserts, 5 updates, and 5 deletes, growing the database from 29 pages (`b8a0bd0796442099`, with freelist leaves `[22]`) to 39 pages (`0ef90bd0e64af1e3`).
- **Lines 29–44 (Part C: the live journal before `COMMIT`, portable).** After executing every statement of step 3 without committing, `internal/journalfile` reads `t.db-journal` (29,240 bytes = 512-byte header sector + 7 × 4,104-byte records). Its first byte is `0x00` (`not hot`, header `not-yet-synced`), initial size is `29` pages, sector size is `512` (the unix VFS advertises `SQLITE_IOCAP_POWERSAFE_OVERWRITE`, so the pager uses 512-byte sectors), and all 7 records (`[2 1 20 3 27 4 26]`) pass their checksums and match the exact pre-step pages byte for byte. Notice page `22` (the reused freelist leaf) is absent from the journal. Issuing `ROLLBACK` on the live connection restores `pre-step exact bytes` because page 22 was modified only in memory and never written to disk.
- **Lines 46–415 (Part D: step 3 at `synchronous=FULL`).** The baseline trace records 70 syscalls (examined line by line in Section 4): 7 journaled pages (21 record writes), 18 database page writes, and 4 syncs (`[t.db-journal . t.db-journal t.db]`). The twin check confirms the pinned C `sqlite3` CLI issues the exact same 46 writes, syncs, and unlink in the same order and produces identical post-step bytes. Killing on entry to each of the 47 kill points yields:
  - **Kills 01–25** (through entry to `journal-count` at offset 0 len 12): database untouched; journal first byte is `0x00` (cold); SQLite ignores the journal on reopen, leaving `pre-step, exact bytes` and a harmless cold `-journal` on disk.
  - **Kill 26** (entry to second `journal-sync`): `journal-count` has written magic `0xd9...` and record count `7`, turning the journal hot; database is still untouched; SQLite rolls back the 7 pages and deletes `-journal`, leaving `pre-step, exact bytes`.
  - **Kills 27–32** (database writes of pages 1, 2, 3, 4, 20, and entry to page 22 at offset `86016`): database is torn (1 to 5 pages differ from pre-step); hot journal restores all 7 original pages; `pre-step, exact bytes`.
  - **Kills 33–46** (after page 22 is written through entry to `unlink t.db-journal`): database is torn (or post-step bytes at kills 45–46); hot journal restores the 7 original pages and truncates to 29 pages; reopened database equals `pre-step except page(s) [22]`, `integrity_check ok`, and `VACUUM INTO` matches pre-step's canonical hash.
  - **Kill 47** (entry to `open marker-end`, immediately after `unlink`): no journal remains; reopened database equals `post-step, exact bytes`.
- **Lines 417–1117 (Part D: `NORMAL`, `OFF`, and the summary strip).**
  - `NORMAL` (46 kill points, 3 syncs `[t.db-journal . t.db]`, 45 twin-check ops identical to C CLI): moves `journal-count` ahead of the single journal sync, so the journal becomes hot at kill 24 (entry to `journal-sync`) rather than kill 26. Outcomes: 31 pre-step exact, 14 pre-step except page 22, 1 post-step exact.
  - `OFF` (42 kill points, 0 syncs `[]`, 41 twin-check ops identical to C CLI): writes magic + `0xffffffff` in the initial 512-byte header, so the journal is hot from kill 02 onward. Even with zero syncs, every process kill reopens to pre-step (28 exact, 13 except page 22) or post-step (1 exact) because completed writes survive a process crash in the OS kernel buffer cache.
  - Summary lines 1114–1116 show the single flip from `B`/`b` to `A` right after `unlink`:
    - `FULL`: 32 `B`, 14 `b`, 1 `A`
    - `NORMAL`: 31 `B`, 14 `b`, 1 `A`
    - `OFF`: 28 `B`, 13 `b`, 1 `A`
- **Lines 1119–1297 (Part D2: step 1 with no freelist).** Pre-step (after step 0) has 4 pages and `freelist leaves []`; step 1 grows it to 22 pages (4 journaled pages, 22 database writes—18 of them appended past page 4 and undone by truncation). Across all 42 (`FULL`), 41 (`NORMAL`), and 37 (`OFF`) kill points, every single reopened database matches pre-step or post-step **to the exact byte** (`41 B + 1 A`, `40 B + 1 A`, `36 B + 1 A`, zero `b` outcomes).

---

## 4. Timeline

One rollback-mode commit of recipe step 3 (`synchronous=FULL`, `journal_mode=delete`, Experiment 4.1 Part D lines 48–117) crosses every pager state in 70 traced syscalls on the database directory:

| Syscall # | Syscalls in the trace | Pager action | Pager state / lock |
| --- | --- | --- | --- |
| 1–3 | `fcntl F_RDLCK 1073741824 len=1`, `F_RDLCK 1073741826 len=510`, `F_UNLCK 1073741824` | Acquire `SHARED` lock (through the `PENDING` byte) | `OPEN -> READER` |
| 4 | `pread64 t.db off=24 len=16` | Read header change counter to check cache validity | `READER` |
| 5 | `fcntl F_WRLCK 1073741825 len=1` | Acquire `RESERVED` lock (`BEGIN IMMEDIATE`) | `-> WRITER_LOCKED` |
| 6 | `pread64 t.db off=4096 len=4096` | Read page 2 before modifying it | `WRITER_LOCKED` |
| 7–9 | `open t.db-journal`, `fcntl F_SETFD` (modernc), `pwrite64 off=0 len=512` | Create journal; write header with zeroed magic and count (`writeJournalHdr`) | `-> WRITER_CACHEMOD` |
| 10–36 | 21 × `pwrite64 t.db-journal` (4 B / 4,096 B / 4 B per page) interleaved with `pread64 t.db` | Journal original images of pages `2, 1, 20, 3, 27, 4, 26` (`pagerAddPageToRollbackJournal`) | `WRITER_CACHEMOD` |
| 37–38 | `fcntl F_WRLCK 1073741824 len=1`, `F_WRLCK 1073741826 len=510` | Upgrade `RESERVED -> PENDING -> EXCLUSIVE` for commit | `WRITER_CACHEMOD` |
| 39 | `pread64 t.db-journal off=29696 len=8 = 0` | Probe next sector boundary for any stale header (`syncJournal`) | `WRITER_CACHEMOD` |
| 40 | `fsync t.db-journal` | **Journal sync 1 (`FULL` only):** flush all 7 page records | `WRITER_CACHEMOD` |
| 41–44 | `open .`, `fcntl F_SETFD`, `fsync .`, `close .` | **Directory sync:** make the new `t.db-journal` directory entry durable | `WRITER_CACHEMOD` |
| 45 | `pwrite64 t.db-journal off=0 len=12` | Write 8-byte magic + 4-byte count (`7`): **journal becomes hot** | `WRITER_CACHEMOD` |
| 46 | `fsync t.db-journal` | **Journal sync 2:** make the hot header durable | `-> WRITER_DBMOD` |
| 47–64 | 18 × `pwrite64 t.db` at ascending offsets `0` … `155648` | Write dirty pages `1–4, 20, 22, 26–39` in sorted order (`pager_write_pagelist`) | `WRITER_DBMOD` |
| 65 | `fsync t.db` | **Database sync:** flush all 18 written pages to disk (`sqlite3PagerSync`) | `-> WRITER_FINISHED` |
| 66–67 | `close t.db-journal`, `unlink t.db-journal` | **Commit point (`DELETE` mode):** remove the hot journal (`pager_end_transaction`) | `-> READER` |
| 68–70 | `fcntl F_RDLCK 1073741826 len=510`, `F_UNLCK 1073741824 len=2`, `F_UNLCK 0 len=0` | Downgrade to `SHARED`, then release all locks | `-> OPEN` |

How the other settings alter this timeline (all verified in Experiment 4.1 Parts D and F):

- **`synchronous=NORMAL` (69 calls, 3 syncs).** Drops call #40 (the first journal sync). Call #45 (`journal-count`, 12 bytes at offset 0) runs right after the stale-header probe, followed by the single `fsync t.db-journal` and `fsync .`.
- **`synchronous=OFF` (61 calls, 0 syncs).** Drops all four syncs and the stale-header probe. Call #9 writes the magic and `0xffffffff` immediately, so call #45 disappears too.
- **`journal_mode=truncate` and `persist` at `FULL` (5 syncs).** Replace calls #66–67 (`close`, `unlink`) with either `ftruncate t.db-journal len=0` (`TRUNCATE`) or `pwrite64 t.db-journal off=0 len=28` (`PERSIST`), followed by a fifth sync (`fsync t.db-journal`) so the journal inactivation reaches disk (`pager_end_transaction`, `src/pager.c:2090-2105`).

Crash outcome by timeline window (`FULL`, `DELETE`):

- **Before #45 (`journal-count`):** `-journal` is cold (first byte `0x00`), `t.db` is untouched -> reopen ignores the journal -> **pre-step exact**.
- **From #45 through #67 (before `unlink` completes):** `-journal` is hot (`0xd9...`, count `7`), `t.db` is untouched, torn, or post-step -> reopen replays the 7 pages and truncates to 29 pages -> **pre-step** (exact before page 22 is written at #52; pre-step except freelist leaf 22 from #52 through #67).
- **After #67 (`unlink` completed):** no `-journal` exists, `t.db` holds all 18 new pages -> reopen sees **post-step exact**.

---

## 5. Lab

### 5.1 Shared package: `internal/journalfile`

Experiment 4.1 needs to inspect `-journal` files *before* SQLite reopens them—decoding header states, checking record checksums, and predicting what `pager_playback` will restore—without linking SQLite into the reader. `internal/journalfile` is a read-only Go parser that never modifies any file.

<!-- include: ../internal/journalfile/journalfile.go -->
```go
// Package journalfile implements a read-only inspector for SQLite rollback
// journal (-journal) files, using plain Go with no SQLite dependency.
//
// It decodes journal headers, page records, checksums, and super-journal
// trailers, and predicts which pages SQLite would restore during hot-journal
// playback. It never writes to a database or replays a journal itself.
package journalfile

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
)

// Magic is the 8-byte signature that identifies a live rollback journal header
// (aJournalMagic in src/pager.c:757).
var Magic = [8]byte{0xd9, 0xd5, 0x05, 0xf9, 0x20, 0xa1, 0x63, 0xd7}

// Journal format sizes and bounds matching src/pager.c:1449-1693.
const (
	// HeaderFieldsSize is the byte length of the six big-endian fields at the
	// start of a journal header (8-byte magic + five 4-byte integers).
	HeaderFieldsSize = 28
	// NoSyncRecordCount (0xffffffff) in a header's record-count field tells
	// SQLite to infer the record count from the file size (src/pager.c:1499).
	NoSyncRecordCount = 0xffffffff
	// RecordOverhead is the 8 bytes wrapping each journaled page image:
	// 4-byte page number before the image and 4-byte checksum after it
	// (pagerAddPageToRollbackJournal, src/pager.c:6062-6067).
	RecordOverhead     = 8
	pendingByte        = 0x40000000
	minSectorSize      = 32
	maxSectorSize      = 0x10000
	minPageSize        = 512
	maxPageSize        = 65536
	superJournalFooter = 16
)

// HeaderState classifies the 28-byte header at the start of a journal segment.
type HeaderState int

// Rollback journal header states recognized by parseHeader.
const (
	// HeaderSynced indicates a header carrying Magic and an explicit record
	// count, written by syncJournal (src/pager.c:4377-4410).
	HeaderSynced HeaderState = iota
	// HeaderNoSync indicates a header carrying Magic and NoSyncRecordCount,
	// written directly by writeJournalHdr when synchronous=OFF,
	// journal_mode=MEMORY, or SAFE_APPEND is active (src/pager.c:1495-1499).
	HeaderNoSync
	// HeaderNotYetSynced indicates a header whose first 12 bytes (magic and
	// record count) are still zero while the remaining fields are populated,
	// written by writeJournalHdr before syncJournal runs (src/pager.c:1500-1502).
	HeaderNotYetSynced
	// HeaderZeroed indicates all 28 header bytes are zero, as left after commit
	// in PERSIST mode by zeroJournalHdr (src/pager.c:1399-1432).
	HeaderZeroed
	// HeaderInvalid indicates bytes that are neither valid magic, nor zeroed,
	// nor a valid sector/page-size geometry (readJournalHdr, src/pager.c:1650-1672).
	HeaderInvalid
	// HeaderMissing indicates fewer than HeaderFieldsSize bytes were present.
	HeaderMissing
)

// String returns a short human-readable name for the header state.
func (s HeaderState) String() string {
	switch s {
	case HeaderSynced:
		return "synced"
	case HeaderNoSync:
		return "no-sync"
	case HeaderNotYetSynced:
		return "not-yet-synced"
	case HeaderZeroed:
		return "zeroed"
	case HeaderInvalid:
		return "invalid"
	case HeaderMissing:
		return "missing"
	}
	return fmt.Sprintf("HeaderState(%d)", int(s))
}

// Header holds the decoded fields of one sector-aligned rollback journal header
// (writeJournalHdr and readJournalHdr, src/pager.c:1449-1693).
type Header struct {
	Offset       int64
	State        HeaderState
	MagicOK      bool
	RecordCount  uint32
	Nonce        uint32
	InitialPages uint32
	SectorSize   uint32
	PageSize     uint32
}

// CountsRecords reports whether the header carries a valid magic and an
// explicit record count rather than the NoSyncRecordCount sentinel.
func (h Header) CountsRecords() bool {
	return h.MagicOK && h.RecordCount != NoSyncRecordCount
}

// Record holds one journaled page record: the 4-byte page number, the original
// page image, the stored 4-byte checksum, and the checksum recomputed from the
// segment's nonce and page bytes (src/pager.c:6037-6085).
type Record struct {
	Offset           int64
	Page             uint32
	StoredChecksum   uint32
	ComputedChecksum uint32
	Image            []byte
}

// ChecksumOK reports whether the record's stored checksum matches the checksum
// computed by Checksum.
func (r Record) ChecksumOK() bool {
	return r.StoredChecksum == r.ComputedChecksum
}

// Segment pairs one journal header with the complete page records that follow
// it. A transaction that spills dirty pages mid-transaction writes multiple
// segments, each starting on a sector boundary (syncJournal, src/pager.c:4423-4427).
type Segment struct {
	Header  Header
	Records []Record
}

// Journal is the decoded structure of a rollback journal file.
type Journal struct {
	Size         int64
	FirstByte    byte
	Segments     []Segment
	SuperJournal string
	TrailingByte int64
}

// PlaybackOutcome predicts what SQLite's pager_playback (src/pager.c:2864-3052)
// will do if it reopens the database beside this journal.
type PlaybackOutcome struct {
	Hot          bool
	TruncateTo   uint32
	Pages        []uint32
	SkippedPages []uint32
	StopReason   string
}

// ReadFile reads path from disk and parses it as a rollback journal.
func ReadFile(path string) (*Journal, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

// Parse decodes raw rollback-journal bytes into segments, records, and any
// trailing super-journal name without modifying the input slice.
func Parse(data []byte) (*Journal, error) {
	j := &Journal{Size: int64(len(data))}
	if len(data) > 0 {
		j.FirstByte = data[0]
	}
	end := int64(len(data))
	// Check for a multi-database super-journal footer at EOF first so its bytes
	// are excluded from record parsing (pager_playback, src/pager.c:2891).
	if name, start, ok := superJournalName(data); ok {
		j.SuperJournal = name
		end = start
	}
	if end < HeaderFieldsSize {
		if end > 0 {
			j.Segments = append(j.Segments, Segment{Header: Header{State: HeaderMissing}})
			j.TrailingByte = end
		}
		return j, nil
	}
	first := parseHeader(data, 0)
	if first.State == HeaderZeroed || first.State == HeaderInvalid {
		j.Segments = append(j.Segments, Segment{Header: first})
		return j, nil
	}
	// Geometry (sector size and page size) comes only from the first header;
	// SQLite stops playback if either is out of range or not a power of two
	// (readJournalHdr, src/pager.c:1650-1672).
	if !validGeometry(first.SectorSize, first.PageSize) {
		first.State = HeaderInvalid
		j.Segments = append(j.Segments, Segment{Header: first})
		return j, nil
	}
	sector := int64(first.SectorSize)
	pageSize := int64(first.PageSize)
	recordSize := pageSize + RecordOverhead
	offset := int64(0)
	header := first
	for {
		segment := Segment{Header: header}
		// Records begin one sector after the header (writeJournalHdr pads the
		// header out to a full sector, src/pager.c:1563-1568).
		position := offset + sector
		limit := int64(-1)
		if header.CountsRecords() {
			limit = int64(header.RecordCount)
		}
		for (limit < 0 || int64(len(segment.Records)) < limit) && position+recordSize <= end {
			segment.Records = append(segment.Records, parseRecord(data, position, pageSize, header.Nonce))
			position += recordSize
		}
		j.Segments = append(j.Segments, segment)
		if limit < 0 || int64(len(segment.Records)) < limit {
			j.TrailingByte = end - position
			break
		}
		// A subsequent segment header starts on the next sector boundary
		// (journalHdrOffset, src/pager.c:1366-1376).
		next := roundUp(position, sector)
		if next+sector > end {
			j.TrailingByte = end - position
			break
		}
		header = parseHeader(data, next)
		if header.State != HeaderSynced && header.State != HeaderNotYetSynced && header.State != HeaderNoSync {
			j.TrailingByte = end - position
			break
		}
		header.SectorSize = first.SectorSize
		header.PageSize = first.PageSize
		offset = next
	}
	return j, nil
}

func parseHeader(data []byte, offset int64) Header {
	h := Header{Offset: offset}
	fields := data[offset : offset+HeaderFieldsSize]
	h.MagicOK = bytes.Equal(fields[:8], Magic[:])
	h.RecordCount = binary.BigEndian.Uint32(fields[8:12])
	h.Nonce = binary.BigEndian.Uint32(fields[12:16])
	h.InitialPages = binary.BigEndian.Uint32(fields[16:20])
	h.SectorSize = binary.BigEndian.Uint32(fields[20:24])
	h.PageSize = binary.BigEndian.Uint32(fields[24:28])
	switch {
	case allZero(fields):
		h.State = HeaderZeroed
	case h.MagicOK && h.RecordCount == NoSyncRecordCount:
		h.State = HeaderNoSync
	case h.MagicOK:
		h.State = HeaderSynced
	case allZero(fields[:12]):
		h.State = HeaderNotYetSynced
	default:
		h.State = HeaderInvalid
	}
	return h
}

func parseRecord(data []byte, offset, pageSize int64, nonce uint32) Record {
	image := data[offset+4 : offset+4+pageSize]
	return Record{
		Offset:           offset,
		Page:             binary.BigEndian.Uint32(data[offset : offset+4]),
		StoredChecksum:   binary.BigEndian.Uint32(data[offset+4+pageSize : offset+8+pageSize]),
		ComputedChecksum: Checksum(nonce, image),
		Image:            image,
	}
}

// Checksum computes SQLite's rollback-journal page checksum: starting from
// nonce, it adds every 200th byte stepping down from len(image)-200 while the
// index is strictly positive (pager_cksum, src/pager.c:2251-2259).
func Checksum(nonce uint32, image []byte) uint32 {
	sum := nonce
	for i := len(image) - 200; i > 0; i -= 200 {
		sum += uint32(image[i])
	}
	return sum
}

// SuperJournalPage returns the reserved page number (PAGER_SJ_PGNO) that marks
// the start of a super-journal name record in the journal (the lock-byte page
// at byte offset 0x40000000, which can never be a real database page).
func SuperJournalPage(pageSize uint32) uint32 {
	if pageSize == 0 {
		return 0
	}
	return pendingByte/pageSize + 1
}

// superJournalName detects the optional multi-database super-journal footer
// written by writeSuperJournal: [4-byte PAGER_SJ_PGNO][name][4-byte len][4-byte sum][8-byte Magic].
func superJournalName(data []byte) (string, int64, bool) {
	size := int64(len(data))
	if size < superJournalFooter+4 || !bytes.Equal(data[size-8:], Magic[:]) {
		return "", 0, false
	}
	length := int64(binary.BigEndian.Uint32(data[size-16 : size-12]))
	stored := binary.BigEndian.Uint32(data[size-12 : size-8])
	nameStart := size - superJournalFooter - length
	if length <= 0 || nameStart < 4 {
		return "", 0, false
	}
	name := data[nameStart : size-superJournalFooter]
	var sum uint32
	for _, b := range name {
		sum += uint32(b)
	}
	if sum != stored {
		return "", 0, false
	}
	return string(bytes.TrimRight(name, "\x00")), nameStart - 4, true
}

func validGeometry(sector, pageSize uint32) bool {
	powerOfTwo := func(v uint32) bool { return v != 0 && v&(v-1) == 0 }
	return sector >= minSectorSize && sector <= maxSectorSize && powerOfTwo(sector) &&
		pageSize >= minPageSize && pageSize <= maxPageSize && powerOfTwo(pageSize)
}

func roundUp(offset, unit int64) int64 {
	if offset == 0 {
		return 0
	}
	return ((offset-1)/unit + 1) * unit
}

func allZero(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return false
		}
	}
	return true
}

// LooksHot reports the byte-level half of hasHotJournal (src/pager.c:5244-5251):
// a journal is hot only if it is non-empty and its first byte is non-zero.
// The lock and database-size checks in hasHotJournal depend on external state.
func (j *Journal) LooksHot() bool {
	return j.Size > 0 && j.FirstByte != 0
}

// RecordsPresent returns the total count of complete page records across all
// parsed segments.
func (j *Journal) RecordsPresent() int {
	n := 0
	for _, s := range j.Segments {
		n += len(s.Records)
	}
	return n
}

// ChecksumFailures returns the number of complete records whose stored checksum
// disagrees with Checksum.
func (j *Journal) ChecksumFailures() int {
	n := 0
	for _, s := range j.Segments {
		for _, r := range s.Records {
			if !r.ChecksumOK() {
				n++
			}
		}
	}
	return n
}

// Playback simulates the read loop of pager_playback (src/pager.c:2864-3052)
// and pager_playback_one_page (src/pager.c:2298-2500) to predict which pages
// SQLite will restore and what page count it will truncate the database to.
func (j *Journal) Playback() PlaybackOutcome {
	outcome := PlaybackOutcome{Hot: j.LooksHot()}
	if !outcome.Hot {
		outcome.StopReason = "not hot: empty file or first byte is zero"
		return outcome
	}
	if len(j.Segments) == 0 {
		outcome.StopReason = "no header"
		return outcome
	}
	first := j.Segments[0].Header
	// readJournalHdr returns SQLITE_DONE when the magic bytes do not match.
	if first.State != HeaderSynced && first.State != HeaderNoSync {
		outcome.StopReason = "first header is " + first.State.String()
		return outcome
	}
	// pager_playback truncates the database to the first header's dbOrigSize
	// before restoring pages (src/pager.c:2952).
	outcome.TruncateTo = first.InitialPages
	superPage := SuperJournalPage(first.PageSize)
	for _, segment := range j.Segments {
		h := segment.Header
		if !h.MagicOK {
			outcome.StopReason = fmt.Sprintf("header at offset %d has no magic", h.Offset)
			return outcome
		}
		count := len(segment.Records)
		if h.RecordCount != NoSyncRecordCount {
			if int64(h.RecordCount) > int64(count) {
				outcome.StopReason = fmt.Sprintf("header at offset %d counts %d records but only %d are complete", h.Offset, h.RecordCount, count)
				for _, r := range segment.Records {
					if stop := outcome.take(r, first.InitialPages, superPage); stop != "" {
						outcome.StopReason = stop
						return outcome
					}
				}
				return outcome
			}
			count = int(h.RecordCount)
		}
		for _, r := range segment.Records[:count] {
			if stop := outcome.take(r, first.InitialPages, superPage); stop != "" {
				outcome.StopReason = stop
				return outcome
			}
		}
	}
	outcome.StopReason = "end of journal"
	return outcome
}

// take applies pager_playback_one_page's per-record checks in SQLite's order
// (src/pager.c:2298-2500): page 0 or superPage stops playback, pages beyond
// initialPages are skipped, and a bad checksum stops playback.
func (o *PlaybackOutcome) take(r Record, initialPages, superPage uint32) string {
	if r.Page == 0 || r.Page == superPage {
		return fmt.Sprintf("record at offset %d has page number %d", r.Offset, r.Page)
	}
	if r.Page > initialPages {
		o.SkippedPages = append(o.SkippedPages, r.Page)
		return ""
	}
	if !r.ChecksumOK() {
		return fmt.Sprintf("record at offset %d (page %d) fails its checksum", r.Offset, r.Page)
	}
	o.Pages = append(o.Pages, r.Page)
	return ""
}
```

Walk-through of `internal/journalfile`:

- **Constants and `HeaderState` (`Magic`, `HeaderFieldsSize`, `NoSyncRecordCount`, `RecordOverhead`).** Encodes the 8-byte signature `aJournalMagic` (`d9 d5 05 f9 20 a1 63 d7`), the 28-byte header field block, the `0xffffffff` no-sync sentinel, and the 8-byte per-record wrapper (4-byte page number + 4-byte checksum). `parseHeader` classifies the first 28 bytes into six states: `HeaderZeroed` (all 28 bytes zero, `PERSIST` after commit), `HeaderNoSync` (magic + `0xffffffff`), `HeaderSynced` (magic + explicit count), `HeaderNotYetSynced` (first 12 bytes zero, rest populated—cold journal before `syncJournal`), `HeaderInvalid`, and `HeaderMissing` (fewer than 28 bytes).
- **Segment parsing (`Parse`, `validGeometry`, `roundUp`, `superJournalName`).** Checks first for a multi-database super-journal footer at EOF (`[PAGER_SJ_PGNO][name][4-byte len][4-byte byte-sum][Magic]`) and trims it off the record scan. Reads sector size and page size from header 0, rejecting non-power-of-two or out-of-range values just as `readJournalHdr` does (`src/pager.c:1650-1672`). Walks sector-aligned segments: records begin one sector after each header; a counted header owns `RecordCount` records and the next header begins at `roundUp(position, sector)` (`journalHdrOffset`, `src/pager.c:1366-1376`), while a `no-sync` or `not-yet-synced` header reads every complete record up to EOF, recording any incomplete tail in `TrailingByte`.
- **Checksum (`Checksum`).** Re-implements `pager_cksum` (`src/pager.c:2251-2259`): starts from `nonce` and sums `image[i]` for `i = len(image) - 200; i > 0; i -= 200`. Byte 0 is intentionally not included.
- **Hotness and playback prediction (`LooksHot`, `Playback`, `take`).** `LooksHot()` checks `Size > 0 && FirstByte != 0`—the byte-level half of `hasHotJournal` (`src/pager.c:5244-5251`). `Playback()` walks segments and applies `pager_playback_one_page`'s exact stop/skip rules (`src/pager.c:2298-2500`): stop on page `0` or `SuperJournalPage(pageSize)` (`262145` for 4,096-byte pages), skip pages `> InitialPages`, stop on the first bad checksum, and report `TruncateTo = first.InitialPages`.

Unit tests and a live test against an open SQLite transaction live in [`internal/journalfile/journalfile_test.go`](../internal/journalfile/journalfile_test.go) and run with:

```text
go test ./internal/journalfile/
```

---

### 5.2 Experiment 4.2 — Cache budget (`4.2-cache-budget`)

- **Claim:**
  1. One full scan reports exact per-page memory overhead (`4,352` bytes = 4,096-byte page + 256 bytes of headers in `sqlite3PagerMemUsed`, `src/pager.c:6881-6887`), a cache of `N` pages holds `N - 1` (`pcache1FetchStage2`, `src/pcache1.c:902`), a file larger than `cache_size` misses every page again on a second scan, and a cache larger than the file serves the second scan with zero new misses (Part A).
  2. **Outline correction (unified `PGroup` in this driver build):** `modernc.org/sqlite v1.60.1` is compiled with `SQLITE_ENABLE_MEMORY_MANAGEMENT=1`, so `cache_size` is **not** a per-connection cap; every open connection contributes its share (`482` pages at `cache_size=-2000`) to one process-wide LRU pool (`pcache1Init`, `src/pcache1.c:718-719`). An active connection takes pages from idle connections (holding up to all 1,004 pages of its file while idle connections drop to 0), while the sum across all open connections equals `open * 482` exactly.
  3. Total cached pages and process RSS grow linearly with open connections—whether those connections open `N` distinct files (Part B) or the same single file (Part C, proving connections do not share a cache). With this driver's `modernc.org/memory` power-of-two allocator, RSS growth is roughly `2x` summed `CACHE_USED` (each ~4.3 KiB page allocation occupies an 8 KiB slot).
  4. Visiting `N` files through an LRU pool capped at `K` open connections holds cached pages and process RSS flat past `K` (Part D).
  5. Enabling `PRAGMA mmap_size` to cover the file drops `CACHE_USED` near zero (from `482` to `2` pages) while total RSS still grows by the file's size—shifted from anonymous heap (`RssAnon`) to file-backed mapped pages (`RssFile`, Part E).
- **Evidence form:** `BOUND` and `SHAPE` for process RSS and pool bounds, with `EXACT` page counts and hit/miss/spill counters on every non-`INFO:` line.
- **Portable (Linux-only split in Part E):** Runs on Linux and macOS; the `RssFile`/`RssAnon` split in Part E requires `/proc/self/status` (**Linux only**; on macOS use `docker compose run --rm lab go run ./ch04-pager-and-page-cache/experiments/4.2-cache-budget`).
- **Run command:**
  ```text
  go run ./ch04-pager-and-page-cache/experiments/4.2-cache-budget
  ```
  Optional flags: `-n 64` (unbounded connection count), `-k 8` (pool cap), `-cache-size -2000`, `-rows 1000`.

<!-- include: experiments/4.2-cache-budget/main.go -->
```go
// Experiment 4.2 measures SQLite's page-cache memory budget and process RSS
// across five scenarios in WAL mode: one connection scanning twice at two
// cache_size settings (Part A), N open connections to N files (Part B), N open
// connections to one file (Part C), N files visited through a K-connection LRU
// pool (Part D), and memory-mapped reads via PRAGMA mmap_size (Part E).
//
// Run from the repo root (portable; the anonymous vs file-backed RSS split in
// Part E uses /proc/self/status on Linux):
//
//	go run ./ch04-pager-and-page-cache/experiments/4.2-cache-budget
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nickstrad/sqlite-internals/internal/recipe"
	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
)

const (
	pageSize  = 4096
	bodyBytes = 3500
	scanSQL   = "SELECT count(*), sum(length(body)) FROM t"
)

var (
	failures      []string
	connectionsN  = flag.Int("n", 64, "connections opened in the unbounded runs")
	boundK        = flag.Int("k", 8, "open connections allowed by the bounded LRU pool")
	cacheSizeFlag = flag.Int("cache-size", -2000, "PRAGMA cache_size for every connection (negative = KiB, positive = pages)")
	rowsFlag      = flag.Int("rows", 1000, "rows per database file; one row of 3500 bytes fills one 4096-byte leaf page")
)

type processMemory struct {
	RSS   int64
	Anon  int64
	File  int64
	Shmem int64
}

type connStat struct {
	CacheUsed int
	Hits      int
	Misses    int
	Spills    int
}

type checkpoint struct {
	Label   string
	Open    int
	Visited int
	Conns   []connStat
	Memory  processMemory
}

var perPage = 1

// cachedPages derives the cached page count from SQLITE_DBSTATUS_CACHE_USED
// (sqlite3PagerMemUsed, src/pager.c:6881-6887). Subtracting the single
// page-sized buffer and dividing by perPage discards the Pager struct's
// path-length-dependent allocation (as long as it stays below perPage), making
// page counts identical across host and container paths.
func cachedPages(cacheUsed int) int {
	return max(0, (cacheUsed-pageSize)/perPage)
}

func (c checkpoint) pagesTotal() int {
	total := 0
	for _, s := range c.Conns {
		total += cachedPages(s.CacheUsed)
	}
	return total
}

func (c checkpoint) pagesMinMax() (int, int) {
	lo, hi := c.cacheMinMax()
	return cachedPages(lo), cachedPages(hi)
}

func (c checkpoint) cacheTotal() int {
	total := 0
	for _, s := range c.Conns {
		total += s.CacheUsed
	}
	return total
}

func (c checkpoint) cacheMinMax() (int, int) {
	if len(c.Conns) == 0 {
		return 0, 0
	}
	lo, hi := c.Conns[0].CacheUsed, c.Conns[0].CacheUsed
	for _, s := range c.Conns {
		lo, hi = min(lo, s.CacheUsed), max(hi, s.CacheUsed)
	}
	return lo, hi
}

func check(ok bool, claim string) {
	status := "ok"
	if !ok {
		status = "FAILED"
		failures = append(failures, claim)
	}
	fmt.Printf("check %s: %s\n", status, claim)
}

func fatal(what string, err error) {
	fmt.Fprintf(os.Stderr, "FATAL %s: %v\n", what, err)
	fmt.Printf("FATAL %s: %v\n", what, err)
	os.Exit(1)
}

func options(cacheSize int) sqlitex.Options {
	o := sqlitex.DefaultOptions()
	o.SingleConnection = true
	o.Conn.CacheSize = cacheSize
	return o
}

// budgetBytes converts PRAGMA cache_size to bytes matching numberOfCachePages
// (src/pcache.c:277-292): negative values are KiB, positive values are pages.
func budgetBytes(cacheSize int) int {
	if cacheSize < 0 {
		return -1024 * cacheSize
	}
	return cacheSize * pageSize
}

func fileName(dir string, i int) string {
	return filepath.Join(dir, fmt.Sprintf("f-%03d.db", i))
}

// buildSource creates one deterministic WAL-mode database with rows of 3500
// PRNG bytes each so every row fills a single 4096-byte leaf page, giving a
// predictable multi-megabyte table that exceeds the default -2000 KiB cache.
func buildSource(ctx context.Context, path string, rows int) {
	if err := recipe.RemoveDatabase(path); err != nil {
		fatal("remove", err)
	}
	db, err := sqlitex.Open(ctx, path, options(-2000))
	if err != nil {
		fatal("create source", err)
	}
	if _, err := db.ExecContext(ctx, "CREATE TABLE t(id INTEGER PRIMARY KEY, body BLOB NOT NULL)"); err != nil {
		fatal("create table", err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		fatal("begin", err)
	}
	rng := rand.New(rand.NewPCG(4, 2))
	body := make([]byte, bodyBytes)
	for id := 1; id <= rows; id++ {
		for i := range body {
			body[i] = byte(rng.Uint32())
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO t(id, body) VALUES(?, ?)", id, body); err != nil {
			fatal("insert", err)
		}
	}
	if err := tx.Commit(); err != nil {
		fatal("commit", err)
	}
	if err := db.Close(); err != nil {
		fatal("close", err)
	}
	if err := recipe.CheckAtRest(path); err != nil {
		fatal("at rest", err)
	}
}

type handle struct {
	db   *sqlitex.DB
	conn *sql.Conn
}

func openHandle(ctx context.Context, path string, cacheSize int) handle {
	db, err := sqlitex.Open(ctx, path, options(cacheSize))
	if err != nil {
		fatal("open", err)
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		fatal("conn", err)
	}
	return handle{db: db, conn: conn}
}

func (h handle) close() {
	if err := h.conn.Close(); err != nil {
		fatal("close conn", err)
	}
	if err := h.db.Close(); err != nil {
		fatal("close db", err)
	}
}

func (h handle) scan(ctx context.Context) {
	var count, total int64
	if err := h.conn.QueryRowContext(ctx, scanSQL).Scan(&count, &total); err != nil {
		fatal("scan", err)
	}
}

func (h handle) stat(ctx context.Context) connStat {
	read := func(op sqlitex.DBStatusOp) int {
		s, err := sqlitex.ReadDBStatus(ctx, h.conn, op, false)
		if err != nil {
			fatal("db_status", err)
		}
		return s.Current
	}
	return connStat{CacheUsed: read(sqlitex.DBStatusCacheUsed), Hits: read(sqlitex.DBStatusCacheHit), Misses: read(sqlitex.DBStatusCacheMiss), Spills: read(sqlitex.DBStatusCacheSpill)}
}

// measure forces a Go GC and returns unused Go spans to the OS before sampling
// process memory so RSS deltas reflect SQLite's page cache and file mappings.
func measure(label string, open, visited int, stats []connStat) {
	runtime.GC()
	debug.FreeOSMemory()
	m, err := readProcessMemory()
	if err != nil {
		fatal("process memory", err)
	}
	line, err := json.Marshal(checkpoint{Label: label, Open: open, Visited: visited, Conns: stats, Memory: m})
	if err != nil {
		fatal("json", err)
	}
	fmt.Println(string(line))
}

func reportPoints(n int) []int {
	points := []int{1}
	for p := 2; p < n; p *= 2 {
		points = append(points, p)
	}
	if n > 1 {
		points = append(points, n)
	}
	return points
}

func childUnbounded(ctx context.Context, dir string, n, cacheSize int, oneFile bool) {
	measure("start", 0, 0, nil)
	var handles []handle
	points := reportPoints(n)
	for i := range n {
		path := fileName(dir, i)
		if oneFile {
			path = fileName(dir, 0)
		}
		h := openHandle(ctx, path, cacheSize)
		h.scan(ctx)
		handles = append(handles, h)
		if slices.Contains(points, i+1) {
			stats := make([]connStat, len(handles))
			for j, other := range handles {
				stats[j] = other.stat(ctx)
			}
			measure(fmt.Sprintf("after %d", i+1), len(handles), i+1, stats)
		}
	}
	for _, h := range handles {
		h.close()
	}
	measure("all closed", 0, n, nil)
}

func childBounded(ctx context.Context, dir string, n, k, cacheSize int) {
	measure("start", 0, 0, nil)
	var pool []handle
	points := reportPoints(n)
	for i := range n {
		if len(pool) == k {
			pool[0].close()
			pool = pool[1:]
		}
		h := openHandle(ctx, fileName(dir, i), cacheSize)
		h.scan(ctx)
		pool = append(pool, h)
		if slices.Contains(points, i+1) || i+1 == k {
			stats := make([]connStat, len(pool))
			for j, other := range pool {
				stats[j] = other.stat(ctx)
			}
			measure(fmt.Sprintf("after %d", i+1), len(pool), i+1, stats)
		}
	}
	for _, h := range pool {
		h.close()
	}
}

func childScans(ctx context.Context, dir string, cacheSize int) {
	h := openHandle(ctx, fileName(dir, 0), cacheSize)
	defer h.close()
	var stats []connStat
	for range 2 {
		h.scan(ctx)
		stats = append(stats, h.stat(ctx))
	}
	measure("scans", 1, 1, stats)
}

func childMmap(ctx context.Context, dir string, cacheSize int, mmapSize int64) {
	measure("start", 0, 0, nil)
	plain := openHandle(ctx, fileName(dir, 0), cacheSize)
	plain.scan(ctx)
	measure("plain", 1, 1, []connStat{plain.stat(ctx)})
	mapped := openHandle(ctx, fileName(dir, 1), cacheSize)
	var got int64
	if err := mapped.conn.QueryRowContext(ctx, fmt.Sprintf("PRAGMA mmap_size=%d", mmapSize)).Scan(&got); err != nil {
		fatal("mmap_size", err)
	}
	if got != mmapSize {
		fatal("mmap_size", fmt.Errorf("PRAGMA mmap_size returned %d, want %d", got, mmapSize))
	}
	mapped.scan(ctx)
	measure("mapped", 2, 2, []connStat{plain.stat(ctx), mapped.stat(ctx)})
	mapped.close()
	plain.close()
}

func childMain(args []string) {
	ctx := context.Background()
	atoi := func(s string) int {
		v, err := strconv.Atoi(s)
		if err != nil {
			fatal("child args", err)
		}
		return v
	}
	switch args[0] {
	case "files":
		childUnbounded(ctx, args[1], atoi(args[2]), atoi(args[3]), false)
	case "one-file":
		childUnbounded(ctx, args[1], atoi(args[2]), atoi(args[3]), true)
	case "bounded":
		childBounded(ctx, args[1], atoi(args[2]), atoi(args[3]), atoi(args[4]))
	case "scans":
		childScans(ctx, args[1], atoi(args[2]))
	case "mmap":
		childMmap(ctx, args[1], atoi(args[2]), int64(atoi(args[3])))
	default:
		fatal("child", fmt.Errorf("unknown role %q", args[0]))
	}
}

// runChild executes one measurement scenario in a fresh child process so every
// scenario starts from a clean RSS baseline and a fresh pcache1 PGroup.
func runChild(args ...string) []checkpoint {
	exe, err := os.Executable()
	if err != nil {
		fatal("executable", err)
	}
	cmd := exec.Command(exe, append([]string{"child"}, args...)...)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		fatal("child "+args[0], err)
	}
	var points []checkpoint
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		var c checkpoint
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			fatal("child output", fmt.Errorf("%w: %q", err, line))
		}
		points = append(points, c)
	}
	return points
}

func mib(b int64) string {
	if b < 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.1f MiB", float64(b)/(1<<20))
}

func memoryInfo(m processMemory) string {
	if m.Anon < 0 {
		return "RSS " + mib(m.RSS)
	}
	return fmt.Sprintf("RSS %s (anon %s, file %s, shmem %s)", mib(m.RSS), mib(m.Anon), mib(m.File), mib(m.Shmem))
}

func find(points []checkpoint, label string) checkpoint {
	for _, p := range points {
		if p.Label == label {
			return p
		}
	}
	fatal("checkpoint", fmt.Errorf("no checkpoint %q", label))
	return checkpoint{}
}

func printGrowth(points []checkpoint) {
	start := points[0]
	fmt.Printf("  %-12s %5s %18s %32s\n", "checkpoint", "open", "cached pages (sum)", "cached pages per connection min..max")
	for _, p := range points[1:] {
		lo, hi := p.pagesMinMax()
		fmt.Printf("  %-12s %5d %18d %21d..%-10d\n", p.Label, p.Open, p.pagesTotal(), lo, hi)
		fmt.Printf("INFO:   %-12s sum CACHE_USED %d bytes; %s, growth since start %s\n", p.Label, p.cacheTotal(), memoryInfo(p.Memory), mib(p.Memory.RSS-start.Memory.RSS))
	}
}

func lastWithOpen(points []checkpoint) checkpoint {
	for i := len(points) - 1; i >= 0; i-- {
		if points[i].Open > 0 {
			return points[i]
		}
	}
	fatal("checkpoint", errors.New("no checkpoint with open connections"))
	return checkpoint{}
}

func sumsMatch(points []checkpoint, perConnection int) bool {
	for _, p := range points {
		if p.pagesTotal() != p.Open*perConnection {
			return false
		}
	}
	return true
}

func growth(points []checkpoint, label string) int64 {
	return find(points, label).Memory.RSS - points[0].Memory.RSS
}

func main() {
	if len(os.Args) > 2 && os.Args[1] == "child" {
		childMain(os.Args[2:])
		return
	}
	flag.Parse()
	ctx := context.Background()
	started := time.Now()
	sqlitex.MustPinCheckMemory(ctx)
	if *connectionsN < 2 || *boundK < 1 || *boundK >= *connectionsN || *rowsFlag < 10 || *cacheSizeFlag == 0 {
		fatal("flags", errors.New("need n >= 2, 1 <= k < n, rows >= 10, cache-size != 0"))
	}
	work := sqlitex.MustWorkDir("4.2")
	n, k, cacheSize := *connectionsN, *boundK, *cacheSizeFlag
	budget := budgetBytes(cacheSize)
	source := fileName(work, 0)
	buildSource(ctx, source, *rowsFlag)
	info, err := os.Stat(source)
	if err != nil {
		fatal("stat", err)
	}
	for i := 1; i < n; i++ {
		data, err := os.ReadFile(source)
		if err != nil {
			fatal("read source", err)
		}
		if err := os.WriteFile(fileName(work, i), data, 0o644); err != nil {
			fatal("copy", err)
		}
	}
	fmt.Printf("\n== setup ==\n")
	fmt.Printf("database files: %d copies of one WAL-mode file, table t with %d rows of %d bytes, %d pages (%d bytes)\n", n, *rowsFlag, bodyBytes, info.Size()/pageSize, info.Size())
	fmt.Printf("every connection: sqlitex defaults (journal_mode=wal, synchronous=FULL) with cache_size=%d; budget %d bytes per connection\n", cacheSize, budget)
	fmt.Printf("scan: %s (reads every leaf page)\n", scanSQL)
	fmt.Printf("N=%d unbounded connections, K=%d in the bounded pool\n", n, k)
	fmt.Printf("INFO: process memory read from %s; runs in child processes so every measurement starts from a fresh process\n", memorySource)
	var compileMM int
	db, err := sqlitex.OpenMemory(ctx)
	if err != nil {
		fatal("open memory", err)
	}
	if err := db.QueryRowContext(ctx, "SELECT sqlite_compileoption_used('ENABLE_MEMORY_MANAGEMENT')").Scan(&compileMM); err != nil {
		fatal("compile option", err)
	}
	db.Close()
	fmt.Printf("driver compiled with ENABLE_MEMORY_MANAGEMENT: %d (1 = every connection's page cache is one process-wide LRU group, pcache1.c:718-719)\n", compileMM)

	fmt.Printf("\n== A. one connection, the same scan twice ==\n")
	// Measure exact bytes per cached page from sqlite3PagerMemUsed
	// (src/pager.c:6881-6887) by comparing two full caches 100 pages apart.
	// pcache1FetchStage2 recycles at nPage+1 >= nMax (src/pcache1.c:902), so
	// cache_size=100 holds 99 pages and cache_size=200 holds 199 pages; their
	// difference is still exactly 100 pages and cancels the fixed Pager overhead.
	small := runChild("scans", work, "100")[0].Conns[0].CacheUsed
	large := runChild("scans", work, "200")[0].Conns[0].CacheUsed
	if (large-small)%100 != 0 {
		fatal("per-page size", fmt.Errorf("CACHE_USED at cache_size=200 minus at 100 is %d, not a multiple of 100", large-small))
	}
	perPage = (large - small) / 100
	fmt.Printf("CACHE_USED counts %d bytes per cached page (measured: cache_size=200 minus cache_size=100, both full, divided by 100); the %d-byte page plus %d bytes of headers (sqlite3PagerMemUsed, pager.c:6881)\n", perPage, pageSize, perPage-pageSize)
	fmt.Printf("INFO: fixed part of CACHE_USED per connection %d bytes (the Pager allocation, which holds the file names, plus one page buffer)\n", small-cachedPages(small)*perPage)
	fmt.Printf("cache_size=100 holds %d pages, cache_size=200 holds %d pages\n", cachedPages(small), cachedPages(large))
	for _, size := range []int{cacheSize, -2 * int(info.Size()) / 1024} {
		scans := runChild("scans", work, strconv.Itoa(size))[0]
		fmt.Printf("cache_size=%d (budget %d bytes):\n", size, budgetBytes(size))
		for i, s := range scans.Conns {
			fmt.Printf("  after scan %d: cached pages=%d hit=%d miss=%d spill=%d\n", i+1, cachedPages(s.CacheUsed), s.Hits, s.Misses, s.Spills)
			fmt.Printf("INFO:   after scan %d: CACHE_USED=%d bytes\n", i+1, s.CacheUsed)
		}
		first, second := scans.Conns[0], scans.Conns[1]
		check(first.CacheUsed <= budgetBytes(size)*105/100, fmt.Sprintf("cache_size=%d: CACHE_USED stays within the budget plus 5%% (%d pages x %d bytes against %d bytes)", size, cachedPages(first.CacheUsed), perPage, budgetBytes(size)))
		if size == cacheSize {
			check(second.Misses-first.Misses >= int(info.Size()/pageSize)*9/10, "a file larger than the cache misses again on the second scan (the LRU evicted the pages the scan needs next)")
		} else {
			check(second.Misses == first.Misses && second.Hits > first.Hits, "a cache larger than the file serves the second scan from memory: no new misses")
		}
	}

	full := cachedPages(runChild("scans", work, strconv.Itoa(cacheSize))[0].Conns[0].CacheUsed)
	nLabel, halfLabel := fmt.Sprintf("after %d", n), fmt.Sprintf("after %d", reportPoints(n)[len(reportPoints(n))-2])

	fmt.Printf("\n== B. %d connections to %d files, all kept open ==\n", n, n)
	files := runChild("files", work, strconv.Itoa(n), strconv.Itoa(cacheSize))
	printGrowth(files)
	last := lastWithOpen(files)
	fmt.Printf("  at %s: connection 1 cached pages=%d hit=%d miss=%d; connection %d cached pages=%d hit=%d miss=%d\n", last.Label, cachedPages(last.Conns[0].CacheUsed), last.Conns[0].Hits, last.Conns[0].Misses, len(last.Conns), cachedPages(last.Conns[len(last.Conns)-1].CacheUsed), last.Conns[len(last.Conns)-1].Hits, last.Conns[len(last.Conns)-1].Misses)
	lo, hi := last.pagesMinMax()
	check(sumsMatch(files, full), fmt.Sprintf("the cached pages of all open connections add up to exactly (open connections) x %d, the pages one connection holds when it fills its budget", full))
	// With ENABLE_MEMORY_MANAGEMENT, pcache1Init sets separateCache=0
	// (src/pcache1.c:718-719), putting every open connection in one process-wide
	// PGroup LRU. Active scanners recycle idle connections' pages
	// (pcache1FetchStage2, src/pcache1.c:899-912) while the group total equals
	// open * perConnBudget (pcache1Cachesize, src/pcache1.c:814-832).
	if compileMM == 1 {
		check(hi > full && lo < full/10, fmt.Sprintf("in this driver build cache_size is not a per-connection cap: the connection that scanned last holds %d pages (its own budget is %d) while the idle connections were left with %d", hi, full, lo))
	} else {
		check(hi <= full, "every connection stays within its own budget")
	}
	rssN, rssHalf, cacheN := growth(files, nLabel), growth(files, halfLabel), int64(find(files, nLabel).cacheTotal())
	fmt.Printf("INFO: RSS growth at %s %s, at %s %s; RSS growth / summed CACHE_USED %.2f\n", nLabel, mib(rssN), halfLabel, mib(rssHalf), float64(rssN)/float64(cacheN))
	check(rssN >= cacheN*9/10, "process memory grows by at least the summed CACHE_USED of the open connections")
	half := find(files, halfLabel).Open
	perN, perHalf := float64(rssN)/float64(n), float64(rssHalf)/float64(half)
	fmt.Printf("INFO: RSS growth per open connection: %s at %d, %s at %d\n", mib(int64(perN)), n, mib(int64(perHalf)), half)
	check(perN >= perHalf*0.8 && perN <= perHalf*1.25, fmt.Sprintf("process memory grows linearly with open connections: growth per open connection at %d is within -20%%/+25%% of that at %d", n, half))

	fmt.Printf("\n== C. %d connections to one file, all kept open ==\n", n)
	oneFile := runChild("one-file", work, strconv.Itoa(n), strconv.Itoa(cacheSize))
	printGrowth(oneFile)
	check(sumsMatch(oneFile, full) && find(oneFile, nLabel).cacheTotal() == find(files, nLabel).cacheTotal(), "connections to one file do not share a cache: the totals equal the N-files run at every checkpoint")
	oneN := growth(oneFile, nLabel)
	check(oneN >= cacheN*9/10, "process memory grows by the summed CACHE_USED here too")

	fmt.Printf("\n== D. %d files visited through a pool of at most %d open connections (LRU close) ==\n", n, k)
	bounded := runChild("bounded", work, strconv.Itoa(n), strconv.Itoa(k), strconv.Itoa(cacheSize))
	printGrowth(bounded)
	kLabel := fmt.Sprintf("after %d", k)
	atK, atN := growth(bounded, kLabel), growth(bounded, nLabel)
	fmt.Printf("INFO: RSS growth at %s %s, at %s %s\n", kLabel, mib(atK), nLabel, mib(atN))
	check(find(bounded, nLabel).Open == k && find(bounded, nLabel).pagesTotal() <= k*full*101/100, fmt.Sprintf("with the pool, %d connections are open at the end and their cached pages stay within %d x %d (plus 1%%)", k, k, full))
	check(atN-atK <= atK/4, fmt.Sprintf("with the pool, process memory stays flat after %d connections: growth from %s to %s is at most a quarter of the growth up to %s", k, kLabel, nLabel, kLabel))
	check(float64(atN) <= float64(rssN)*float64(k)/float64(n)*1.5, fmt.Sprintf("the pooled run ends with about %d/%d of the unbounded run's memory growth (at most 1.5 x that fraction)", k, n))

	// PRAGMA mmap_size activates getPageMMap (pagerFixMaplimit,
	// src/pager.c:3579-3590; getPageMMap, src/pager.c:5687-5752), which serves
	// pages directly from the kernel's mapped file cache via sqlite3OsFetch and
	// pagerAcquireMapPage outside pcache1, so CACHE_USED stays near zero while
	// RssFile grows by the file size.
	fmt.Printf("\n== E. mmap ==\n")
	mmapSize := 2 * info.Size()
	fmt.Printf("connection 1: no mmap, scans f-000; connection 2: PRAGMA mmap_size=%d (twice the file), scans f-001\n", mmapSize)
	mapped := runChild("mmap", work, strconv.Itoa(cacheSize), strconv.FormatInt(mmapSize, 10))
	for _, p := range mapped[1:] {
		for i, s := range p.Conns {
			fmt.Printf("  %-7s connection %d: cached pages=%d hit=%d miss=%d\n", p.Label, i+1, cachedPages(s.CacheUsed), s.Hits, s.Misses)
			fmt.Printf("INFO:   %-7s connection %d: CACHE_USED=%d bytes\n", p.Label, i+1, s.CacheUsed)
		}
		fmt.Printf("INFO:   %-7s %s\n", p.Label, memoryInfo(p.Memory))
	}
	plain, withMap := find(mapped, "plain"), find(mapped, "mapped")
	mappedConn := withMap.Conns[1]
	check(mappedConn.CacheUsed*20 < plain.Conns[0].CacheUsed, "with mmap covering the file, the scan leaves CACHE_USED near zero (under 5% of the unmapped connection's)")
	rssDelta := withMap.Memory.RSS - plain.Memory.RSS
	fmt.Printf("INFO: the mapped scan added %s RSS (file %s)\n", mib(rssDelta), mib(info.Size()))
	check(rssDelta >= info.Size()*8/10, "the mapped scan still adds about the file's size to process RSS: the memory moved from SQLite's heap to mapped file pages")
	if withMap.Memory.File >= 0 {
		fileDelta, anonDelta := withMap.Memory.File-plain.Memory.File, withMap.Memory.Anon-plain.Memory.Anon
		fmt.Printf("INFO: file-backed RSS +%s, anonymous RSS +%s\n", mib(fileDelta), mib(anonDelta))
		check(fileDelta >= info.Size()*8/10 && anonDelta < info.Size()/4, "the added memory is file-backed (RssFile), not anonymous heap")
	} else {
		fmt.Println("LINUX-ONLY: the split of RSS into anonymous and file-backed memory (/proc/self/status) skipped on " + runtime.GOOS + "; run it with: docker compose run --rm lab go run ./ch04-pager-and-page-cache/experiments/4.2-cache-budget")
	}

	fmt.Printf("INFO: runtime %s\n", time.Since(started).Round(100*time.Millisecond))
	fmt.Println()
	if len(failures) > 0 {
		fmt.Printf("CACHE BUDGET FAILED: %d claim(s) do not hold: %s\n", len(failures), strings.Join(failures, "; "))
		os.Exit(1)
	}
	fmt.Println("CACHE BUDGET OK")
}
```

<!-- include: experiments/4.2-cache-budget/memory_linux.go -->
```go
//go:build linux

package main

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

const memorySource = "/proc/self/status"

// readProcessMemory parses /proc/self/status on Linux to report total resident
// memory (VmRSS) together with its anonymous, file-backed, and shared-memory
// breakdown so Part E can verify that mmap shifts pages from RssAnon to RssFile.
func readProcessMemory() (processMemory, error) {
	f, err := os.Open("/proc/self/status")
	if err != nil {
		return processMemory{}, err
	}
	defer f.Close()
	m := processMemory{RSS: -1, Anon: -1, File: -1, Shmem: -1}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		kib, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			continue
		}
		switch fields[0] {
		case "VmRSS:":
			m.RSS = kib * 1024
		case "RssAnon:":
			m.Anon = kib * 1024
		case "RssFile:":
			m.File = kib * 1024
		case "RssShmem:":
			m.Shmem = kib * 1024
		}
	}
	return m, scanner.Err()
}
```

<!-- include: experiments/4.2-cache-budget/memory_other.go -->
```go
//go:build !linux

package main

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
)

const memorySource = "ps -o rss= -p <pid>"

// readProcessMemory falls back to ps on non-Linux systems (such as macOS),
// reporting total RSS in bytes and leaving the anonymous/file-backed split
// unavailable (-1).
func readProcessMemory() (processMemory, error) {
	out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(os.Getpid())).Output()
	if err != nil {
		return processMemory{}, err
	}
	kib, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return processMemory{}, err
	}
	return processMemory{RSS: kib * 1024, Anon: -1, File: -1, Shmem: -1}, nil
}
```

#### Captured evidence (`4.2-cache-budget.txt`)

<!-- include: evidence/4.2-cache-budget.txt -->
```text
sqlite_source_id: 2026-07-24 19:02:57 bf7c7f30031888f4e796e429ab3978879485813aaca6f641c7b33e4e09459bcc (pin 3.53.4 ok)

== setup ==
database files: 64 copies of one WAL-mode file, table t with 1000 rows of 3500 bytes, 1004 pages (4112384 bytes)
every connection: sqlitex defaults (journal_mode=wal, synchronous=FULL) with cache_size=-2000; budget 2048000 bytes per connection
scan: SELECT count(*), sum(length(body)) FROM t (reads every leaf page)
N=64 unbounded connections, K=8 in the bounded pool
INFO: process memory read from /proc/self/status; runs in child processes so every measurement starts from a fresh process
driver compiled with ENABLE_MEMORY_MANAGEMENT: 1 (1 = every connection's page cache is one process-wide LRU group, pcache1.c:718-719)

== A. one connection, the same scan twice ==
CACHE_USED counts 4352 bytes per cached page (measured: cache_size=200 minus cache_size=100, both full, divided by 100); the 4096-byte page plus 256 bytes of headers (sqlite3PagerMemUsed, pager.c:6881)
INFO: fixed part of CACHE_USED per connection 5077 bytes (the Pager allocation, which holds the file names, plus one page buffer)
cache_size=100 holds 99 pages, cache_size=200 holds 199 pages
cache_size=-2000 (budget 2048000 bytes):
  after scan 1: cached pages=482 hit=7 miss=1005 spill=0
INFO:   after scan 1: CACHE_USED=2102741 bytes
  after scan 2: cached pages=482 hit=9 miss=2007 spill=0
INFO:   after scan 2: CACHE_USED=2102741 bytes
check ok: cache_size=-2000: CACHE_USED stays within the budget plus 5% (482 pages x 4352 bytes against 2048000 bytes)
check ok: a file larger than the cache misses again on the second scan (the LRU evicted the pages the scan needs next)
cache_size=-8032 (budget 8224768 bytes):
  after scan 1: cached pages=1004 hit=7 miss=1005 spill=0
INFO:   after scan 1: CACHE_USED=4374485 bytes
  after scan 2: cached pages=1004 hit=1011 miss=1005 spill=0
INFO:   after scan 2: CACHE_USED=4374485 bytes
check ok: cache_size=-8032: CACHE_USED stays within the budget plus 5% (1004 pages x 4352 bytes against 8224768 bytes)
check ok: a cache larger than the file serves the second scan from memory: no new misses

== B. 64 connections to 64 files, all kept open ==
  checkpoint    open cached pages (sum) cached pages per connection min..max
  after 1          1                482                   482..482       
INFO:   after 1      sum CACHE_USED 2102741 bytes; RSS 12.9 MiB (anon 5.7 MiB, file 7.2 MiB, shmem 0.0 MiB), growth since start 6.0 MiB
  after 2          2                964                     0..964       
INFO:   after 2      sum CACHE_USED 4205482 bytes; RSS 17.1 MiB (anon 9.8 MiB, file 7.3 MiB, shmem 0.0 MiB), growth since start 10.3 MiB
  after 4          4               1928                     0..1004      
INFO:   after 4      sum CACHE_USED 8410964 bytes; RSS 25.1 MiB (anon 17.7 MiB, file 7.4 MiB, shmem 0.0 MiB), growth since start 18.2 MiB
  after 8          8               3856                     0..1004      
INFO:   after 8      sum CACHE_USED 16821928 bytes; RSS 41.1 MiB (anon 33.2 MiB, file 7.9 MiB, shmem 0.0 MiB), growth since start 34.2 MiB
  after 16        16               7712                     0..1004      
INFO:   after 16     sum CACHE_USED 33643856 bytes; RSS 72.6 MiB (anon 64.3 MiB, file 8.3 MiB, shmem 0.0 MiB), growth since start 65.8 MiB
  after 32        32              15424                     0..1004      
INFO:   after 32     sum CACHE_USED 67287712 bytes; RSS 135.2 MiB (anon 126.4 MiB, file 8.8 MiB, shmem 0.0 MiB), growth since start 128.3 MiB
  after 64        64              30848                     0..1004      
INFO:   after 64     sum CACHE_USED 134575424 bytes; RSS 260.5 MiB (anon 250.4 MiB, file 10.1 MiB, shmem 0.0 MiB), growth since start 253.7 MiB
  all closed       0                  0                     0..0         
INFO:   all closed   sum CACHE_USED 0 bytes; RSS 32.2 MiB (anon 24.1 MiB, file 8.1 MiB, shmem 0.0 MiB), growth since start 25.3 MiB
  at after 64: connection 1 cached pages=0 hit=7 miss=1005; connection 64 cached pages=1004 hit=7 miss=1005
check ok: the cached pages of all open connections add up to exactly (open connections) x 482, the pages one connection holds when it fills its budget
check ok: in this driver build cache_size is not a per-connection cap: the connection that scanned last holds 1004 pages (its own budget is 482) while the idle connections were left with 0
INFO: RSS growth at after 64 253.7 MiB, at after 32 128.3 MiB; RSS growth / summed CACHE_USED 1.98
check ok: process memory grows by at least the summed CACHE_USED of the open connections
INFO: RSS growth per open connection: 4.0 MiB at 64, 4.0 MiB at 32
check ok: process memory grows linearly with open connections: growth per open connection at 64 is within -20%/+25% of that at 32

== C. 64 connections to one file, all kept open ==
  checkpoint    open cached pages (sum) cached pages per connection min..max
  after 1          1                482                   482..482       
INFO:   after 1      sum CACHE_USED 2102741 bytes; RSS 13.0 MiB (anon 5.8 MiB, file 7.2 MiB, shmem 0.0 MiB), growth since start 6.1 MiB
  after 2          2                964                     0..964       
INFO:   after 2      sum CACHE_USED 4205482 bytes; RSS 17.0 MiB (anon 9.8 MiB, file 7.2 MiB, shmem 0.0 MiB), growth since start 10.1 MiB
  after 4          4               1928                     0..1004      
INFO:   after 4      sum CACHE_USED 8410964 bytes; RSS 24.8 MiB (anon 17.6 MiB, file 7.2 MiB, shmem 0.0 MiB), growth since start 17.9 MiB
  after 8          8               3856                     0..1004      
INFO:   after 8      sum CACHE_USED 16821928 bytes; RSS 40.4 MiB (anon 33.1 MiB, file 7.3 MiB, shmem 0.0 MiB), growth since start 33.6 MiB
  after 16        16               7712                     0..1004      
INFO:   after 16     sum CACHE_USED 33643856 bytes; RSS 72.1 MiB (anon 64.5 MiB, file 7.6 MiB, shmem 0.0 MiB), growth since start 65.2 MiB
  after 32        32              15424                     0..1004      
INFO:   after 32     sum CACHE_USED 67287712 bytes; RSS 134.6 MiB (anon 126.5 MiB, file 8.1 MiB, shmem 0.0 MiB), growth since start 127.8 MiB
  after 64        64              30848                     0..1004      
INFO:   after 64     sum CACHE_USED 134575424 bytes; RSS 258.6 MiB (anon 250.5 MiB, file 8.1 MiB, shmem 0.0 MiB), growth since start 251.8 MiB
  all closed       0                  0                     0..0         
INFO:   all closed   sum CACHE_USED 0 bytes; RSS 32.2 MiB (anon 24.1 MiB, file 8.1 MiB, shmem 0.0 MiB), growth since start 25.4 MiB
check ok: connections to one file do not share a cache: the totals equal the N-files run at every checkpoint
check ok: process memory grows by the summed CACHE_USED here too

== D. 64 files visited through a pool of at most 8 open connections (LRU close) ==
  checkpoint    open cached pages (sum) cached pages per connection min..max
  after 1          1                482                   482..482       
INFO:   after 1      sum CACHE_USED 2102741 bytes; RSS 13.2 MiB (anon 5.8 MiB, file 7.4 MiB, shmem 0.0 MiB), growth since start 6.3 MiB
  after 2          2                964                     0..964       
INFO:   after 2      sum CACHE_USED 4205482 bytes; RSS 17.2 MiB (anon 9.8 MiB, file 7.4 MiB, shmem 0.0 MiB), growth since start 10.4 MiB
  after 4          4               1928                     0..1004      
INFO:   after 4      sum CACHE_USED 8410964 bytes; RSS 25.2 MiB (anon 17.7 MiB, file 7.4 MiB, shmem 0.0 MiB), growth since start 18.3 MiB
  after 8          8               3856                     0..1004      
INFO:   after 8      sum CACHE_USED 16821928 bytes; RSS 41.0 MiB (anon 33.2 MiB, file 7.8 MiB, shmem 0.0 MiB), growth since start 34.1 MiB
  after 16         8               3863                     0..1004      
INFO:   after 16     sum CACHE_USED 16852392 bytes; RSS 41.7 MiB (anon 33.5 MiB, file 8.2 MiB, shmem 0.0 MiB), growth since start 34.9 MiB
  after 32         8               3863                     0..1004      
INFO:   after 32     sum CACHE_USED 16852392 bytes; RSS 42.1 MiB (anon 33.8 MiB, file 8.3 MiB, shmem 0.0 MiB), growth since start 35.2 MiB
  after 64         8               3863                     0..1004      
INFO:   after 64     sum CACHE_USED 16852392 bytes; RSS 42.3 MiB (anon 34.0 MiB, file 8.3 MiB, shmem 0.0 MiB), growth since start 35.5 MiB
INFO: RSS growth at after 8 34.1 MiB, at after 64 35.5 MiB
check ok: with the pool, 8 connections are open at the end and their cached pages stay within 8 x 482 (plus 1%)
check ok: with the pool, process memory stays flat after 8 connections: growth from after 8 to after 64 is at most a quarter of the growth up to after 8
check ok: the pooled run ends with about 8/64 of the unbounded run's memory growth (at most 1.5 x that fraction)

== E. mmap ==
connection 1: no mmap, scans f-000; connection 2: PRAGMA mmap_size=8224768 (twice the file), scans f-001
  plain   connection 1: cached pages=482 hit=7 miss=1005
INFO:   plain   connection 1: CACHE_USED=2102741 bytes
INFO:   plain   RSS 13.0 MiB (anon 5.8 MiB, file 7.2 MiB, shmem 0.0 MiB)
  mapped  connection 1: cached pages=482 hit=7 miss=1005
INFO:   mapped  connection 1: CACHE_USED=2102741 bytes
  mapped  connection 2: cached pages=2 hit=7 miss=3
INFO:   mapped  connection 2: CACHE_USED=13781 bytes
INFO:   mapped  RSS 17.2 MiB (anon 6.1 MiB, file 11.1 MiB, shmem 0.0 MiB)
check ok: with mmap covering the file, the scan leaves CACHE_USED near zero (under 5% of the unmapped connection's)
INFO: the mapped scan added 4.2 MiB RSS (file 3.9 MiB)
check ok: the mapped scan still adds about the file's size to process RSS: the memory moved from SQLite's heap to mapped file pages
INFO: file-backed RSS +4.0 MiB, anonymous RSS +0.3 MiB
check ok: the added memory is file-backed (RssFile), not anonymous heap
INFO: runtime 5.8s

CACHE BUDGET OK
```

#### Reading the evidence

- **Lines 3–9 (Setup).** Builds one WAL-mode database (`1,000` rows × `3,500` bytes = `1,004` pages = `4,112,384` bytes, ~3.9 MiB) and copies it 64 times. Each connection opens with `sqlitex.DefaultOptions()` (`journal_mode=wal`, `synchronous=FULL`) and `cache_size=-2000` (`2,048,000`-byte budget). Line 9 prints `driver compiled with ENABLE_MEMORY_MANAGEMENT: 1`, announcing the unified `PGroup` mode (`src/pcache1.c:718-719`).
- **Lines 11–28 (Part A: single connection, page accounting, and LRU behavior).**
  - Subtracting `CACHE_USED` at `cache_size=100` from `cache_size=200` and dividing by `100` measures `4,352` bytes per cached page (`4,096` page bytes + `256` bytes of `PgHdr`/extra/pointers from `sqlite3PagerMemUsed`, `src/pager.c:6881-6887`). The fixed overhead (`5,077` bytes on this host) is the `Pager` struct (whose size includes the database, journal, and WAL paths—**correction 7**, why raw bytes are `INFO:` lines while page counts are `EXACT`) plus one 4,096-byte page buffer.
  - Line 14 proves **correction 8**: `cache_size=100 holds 99 pages, cache_size=200 holds 199 pages` (`pCache->nPage + 1 >= pCache->nMax`, `src/pcache1.c:902`).
  - At `cache_size=-2000`, scan 1 fills `482` pages (`hit=7 miss=1005 spill=0`, `CACHE_USED=2,102,741` bytes). Scan 2 ends at `hit=9 miss=2007`: because the 1,004-page table exceeds the 482-page cache, the LRU evicts every leaf page before the second scan reaches it again.
  - At `cache_size=-8032` (8,224,768 bytes, twice the file), scan 1 caches all `1,004` pages (`hit=7 miss=1005`), and scan 2 records `hit=1011 miss=1005`—zero new misses.
- **Lines 30–54 (Part B: 64 open connections to 64 files).**
  - At every checkpoint (`1, 2, 4, 8, 16, 32, 64` open connections), `cached pages (sum)` equals `open × 482` exactly (`482`, `964`, `1,928`, `3,856`, `7,712`, `15,424`, `30,848`).
  - Look at the `min..max` column (**correction 5**): after connection 2 scans, `min..max` is `0..964`; from connection 4 onward it is `0..1004`. Line 48 confirms `connection 1 cached pages=0` while `connection 64 cached pages=1004`. Because `ENABLE_MEMORY_MANAGEMENT` puts every connection in one `PGroup`, each scanning connection steals the LRU pages of idle connections until it holds the entire 1,004-page file.
  - Look at the `INFO:` RSS lines (**correction 6**): at 64 connections, summed `CACHE_USED` is `134,575,424` bytes (`128.3 MiB`), while RSS growth is `253.7 MiB` (`RSS growth / summed CACHE_USED = 1.98`, `4.0 MiB` per connection at both 32 and 64). That `~2x` factor is a property of `modernc.org/memory` v1.12.1, whose power-of-two allocator places each ~4.3 KiB page allocation into an 8 KiB slot; a C build using glibc `malloc` would show much smaller allocator overhead. File-backed RSS rises by ~2.9 MiB (from `7.2` to `10.1 MiB`), largely the 64 × 32 KiB (`2.0 MiB`) WAL `-shm` mappings.
  - Closing all 64 connections drops `sum CACHE_USED` to `0` and RSS from `260.5 MiB` to `32.2 MiB` (the remaining `25.3 MiB` is retained Go/driver allocator arenas).
- **Lines 56–75 (Part C: 64 open connections to one file).** Every `cached pages (sum)` and `CACHE_USED` line (`134,575,424` bytes at 64) is identical to Part B, and RSS growth (`251.8 MiB`) matches. Opening 64 connections to one database does not share a page cache; each connection adds its own `cache_size` share to the process total. Notice file-backed RSS rises by only `0.9 MiB` (`7.2` to `8.1 MiB`) instead of `2.9 MiB` because all 64 connections share one `-shm` mapping.
- **Lines 77–96 (Part D: 64 files visited through an 8-connection LRU pool).** Up to 8 connections, cached pages climb to `3,856` (`8 × 482`) and RSS growth reaches `34.1 MiB`. Across connections 9 through 64, closing the least recently used connection before opening the next keeps open connections at `8`, cached pages flat at `3,863` (within 1% of `3,856`), and RSS growth flat at `35.5 MiB`—`14%` of Part B's `253.7 MiB` (matching `8/64 = 12.5%` within the `1.5x` bound).
- **Lines 98–112 (Part E: `mmap`).** Connection 1 (no `mmap`) scans `f-000` and holds `482` cached pages (`CACHE_USED=2,102,741` bytes, `hit=7 miss=1005`, `RSS 13.0 MiB`). Connection 2 sets `PRAGMA mmap_size=8224768` (twice the file) and scans `f-001`: it holds only `2` cached pages (`CACHE_USED=13,781` bytes, `hit=7 miss=3`). Total RSS still rises by `+4.2 MiB` (the `3.9 MiB` file size), and `/proc/self/status` shows where it went: `file-backed RSS +4.0 MiB, anonymous RSS +0.3 MiB`. `mmap` does not eliminate memory usage—it moves the read cache from SQLite's private heap into the OS kernel's shared, reclaimable page cache.

---

## 6. Evidence and failure cases

### 6.1 Cold journals versus hot journals

Before `syncJournal` writes the 12-byte magic and record count at offset 0 (call #45 in Section 4), a rollback journal under `synchronous=FULL` or `NORMAL` starts with 12 zero bytes (`writeJournalHdr`, `src/pager.c:1500-1502`). Experiment 4.1 Part D proves what happens if the writer dies during that window (kills 01–25 at `FULL`, kills 01–22 at `NORMAL`):

- `t.db` still holds the exact pre-step bytes.
- `t.db-journal` has `first byte 0x00 (not hot)` and `header not-yet-synced`.
- On reopen, `hasHotJournal` (`src/pager.c:5244-5251`) reads byte 0, sees `0x00`, and ignores the file without deleting it (`cold -journal left (29240 bytes, first byte 0x00)`).

A cold `-journal` left beside a database is harmless because no write reached `t.db` before the header was activated. A platform check that asks "is this database at rest?" by testing only whether `<db>-journal` exists will misclassify a database with a cold journal; the right check is `journalfile.LooksHot()` (`size > 0 && firstByte != 0`), matching `hasHotJournal` and Chapter 2's `capture.CheckAtRest`.

### 6.2 Why hot-journal rollback is not always byte-exact (`PAGER_GET_NOCONTENT`)

The outline originally predicted that every kill point would reopen to either the exact pre-step SHA-256 or the exact post-step SHA-256. Experiment 4.1 disproved the raw-byte version of that claim when the pre-step file has a freelist, and confirmed it when the freelist is empty:

- **Step 3 (`freelist leaves [22]`):** At `FULL`, kills 01–32 reopen to `pre-step, exact bytes`. Kill 32 lands on entry to `pwrite64 t.db off=86016 len=4096`—the write of page 22 (`21 × 4096 = 86016`). From kill 33 through kill 46 (`unlink t.db-journal`), page 22's new bytes are on disk. Because `allocateBtreePage` (`src/btree.c:6740`) fetched page 22 with `PAGER_GET_NOCONTENT`, `getPageNormal` (`src/pager.c:5645-5656`) marked page 22 in `pInJournal` without writing a record for it. When SQLite replays the hot journal on reopen, it restores the freelist trunk page—putting page 22 back on the freelist—but never overwrites page 22's bytes. With `secure_delete=ON`, pre-step page 22 was all zeros, so the leftover bytes make the raw file SHA-256 differ from both pre-step and post-step (`14` kills at `FULL`, `14` at `NORMAL`, `13` at `OFF`). Every single one of those files passes `PRAGMA integrity_check`, has the exact pre-step size (`29` pages), differs from pre-step *only* in page 22, and produces the exact pre-step `VACUUM INTO` canonical hash (equality level E3 turned into E1).
- **Step 1 (`freelist leaves []`, Part D2):** With no freelist pages to reuse, every kill across all three `synchronous` levels (`42` at `FULL`, `41` at `NORMAL`, `37` at `OFF`) reopens to the exact pre-step or post-step bytes (`BBBB…BA`).

### 6.3 Losing a hot journal (Experiment 4.1 Part E)

Part E kills the `synchronous=FULL` step-3 writer at two points and shows what happens if an operator, backup script, or copy tool loses `-journal` (evidence lines 1299–1316):

| Case | Crash point before action | Action taken | Reopen result |
| --- | --- | --- | --- |
| **E1** | Killed on entry to db write 10 of 18 (`pwrite64 t.db off=122880 len=4096`); database torn (30 pages, 9 differ from pre, 9 differ from post); hot `-journal` present | Delete `t.db-journal`, reopen `t.db` | `open failed: open t.db: PRAGMA page_count: database disk image is malformed (11)` (`unreadable`) |
| **E2a** | Same torn database and hot `-journal` | Copy `t.db` alone to another directory, open copy | `open failed: open t.db: PRAGMA page_count: database disk image is malformed (11)` (`unreadable`) |
| **E2b** | Same torn database and hot `-journal` | Copy `t.db` **and** `t.db-journal` together, open copy | `integrity_check: ok; user_version=2 accounts=15 documents=7 events=108` (pre-step except freelist leaf 22) |
| **E3** | Killed on entry to `sync t.db` (all 18 database writes done, `COMMIT` never returned, hot `-journal` present) | Delete `t.db-journal`, reopen `t.db` | `integrity_check: ok; user_version=3 accounts=21 documents=10 events=147` (`post-step, exact bytes`) |

Case E3 is as dangerous as E1: deleting the hot journal after the last database write produces a clean `integrity_check: ok`, turning a transaction that died inside `COMMIT` and never acknowledged to the application into a committed transaction. With the journal kept (kill 45 in Part D), SQLite rolled that exact state back to pre-step.

### 6.4 The commit point across `DELETE`, `TRUNCATE`, and `PERSIST` (Experiment 4.1 Part F)

Part F traces step 3 at `synchronous=FULL` in all three disk-backed rollback modes and kills the writer on entry to the finalization syscall and on the syscall immediately after it (evidence lines 1318–1349):

| `journal_mode` | Syncs at `FULL` | Commit-point syscall | Killed on entry | Killed on next syscall | `-journal` left after clean commit |
| --- | --- | --- | --- | --- | --- |
| `delete` | `4` | `unlink t.db-journal` | pre-step (logical) | post-step (kill on `step-returned`) | no `-journal` |
| `truncate` | `5` | `ftruncate t.db-journal len=0` | pre-step (logical) | post-step (kill on `journal-sync`) | `-journal 0 bytes (not hot)` |
| `persist` | `5` | `pwrite64 t.db-journal off=0 len=28` | pre-step (logical) | post-step (kill on `journal-sync`) | `-journal 29240 bytes, first byte 0x00 (not hot), header zeroed` |

In all three modes, a kill on entry to the commit point rolls back to pre-step and a kill on the very next syscall opens to post-step. `TRUNCATE` and `PERSIST` avoid directory-entry creation/deletion overhead on every transaction, at the cost of a fifth sync (`fsync t.db-journal`) at `synchronous=FULL` (`pager_end_transaction`, `src/pager.c:2091-2105`) and leaving a cold `-journal` file beside the database.

### 6.5 Boundaries not exercised live here

Four behaviors discussed in this chapter are stated from the pinned source or unit tests rather than exercised live in Experiments 4.1–4.2:

- **Cache spill and multi-header rollback journals (`pagerStress`, `src/pager.c:4655-4727`).** Step 3 fits inside `cache_size=-2000` (`spill=0`). Multi-segment journals are unit-tested in `internal/journalfile/journalfile_test.go` (`TestMultipleSegments`) and were forced live in Chapter 2 Experiment 2.3 Part D2, not in 4.1.
- **Crash inside the WAL-switch commit of a brand-new file.** Experiment 4.1 Part A traces the `-journal` syscalls during `PRAGMA journal_mode=wal` and `sqlitex.Open` but does not inject a kill inside them; from `hasHotJournal` (`src/pager.c:5224-5230`), a crash there leaves a 0- or 1-page rollback-mode file.
- **Separate per-connection `PGroup` without `ENABLE_MEMORY_MANAGEMENT`.** The installed C `sqlite3` CLI lacks `ENABLE_MEMORY_MANAGEMENT` (`pcache1.c:720-721`), so each connection keeps its own private LRU. That behavior is read from `pcache1.c`, not measured via `sqlite3_db_status` (the CLI has one connection).
- **1,000 open connections.** Left to the adversarial pass (`-n 1000` in 4.2 takes roughly 4 GiB RSS with `modernc.org/memory`'s 8 KiB slot rounding).

### What attacked this chapter

<!-- adversarial-results -->

---

## 7. Platform implications

Every operational rule below is backed directly by this chapter's experiments:

1. **A hot `-journal` is part of the database; copy both or refuse (Experiment 4.1 Parts D and E).** Copying a crashed rollback-mode `.db` file without its hot `-journal` produced `database disk image is malformed (11)` (E2a), while copying both together recovered cleanly to pre-step (E2b). Backup, move, and capture tools must either copy `<db>` and `<db>-journal` together or refuse any file with a hot journal (as `capture.CheckAtRest` does in Chapter 2).
2. **Never delete a `-journal` file to "fix" a database (Experiment 4.1 Part E1, E3).** Removing a hot journal mid-write corrupts the database (`malformed (11)`). Removing it after the last database write silently turns an unacknowledged crashed commit into a committed transaction (`user_version=3` instead of `2`).
3. **Test for a *hot* journal, not merely for the `-journal` filename (Experiment 4.1 Parts D and F).** Cold journals (first byte `0x00`) remain after pre-sync crashes in `DELETE` mode and after every clean commit in `TRUNCATE` (0 bytes) and `PERSIST` (28 zeroed header bytes) modes. Use `size > 0 && firstByte != 0` (`journalfile.LooksHot`) to test whether a database is at rest.
4. **Rollback mode costs 3 (`NORMAL`) or 4 (`FULL`) syncs per commit and writes every modified page twice (Experiment 4.1 Part D).** Step 3 wrote 7 pages to `-journal`, synced twice (plus a directory sync at `FULL`), wrote 18 pages to `t.db`, and synced `t.db`. Chapter 1 showed WAL mode needed 1 sync per commit after the first; Chapter 7 measures the full I/O matrix across modes.
5. **Every newly created WAL database passes through one rollback-journal commit, and imported files start in rollback mode (Experiment 4.1 Part A).** Even `sqlitex.Open` with `journal_mode=wal` opens, syncs twice, and unlinks a `-journal` when initializing a brand-new file. A platform opener must explicitly switch new files to WAL and refuse to silently flip existing files whose mode disagrees with policy.
6. **Verify rollback-mode crash recovery at E3 (or canonical E1), not raw E1 (Experiment 4.1 Parts D and D2).** Because `PAGER_GET_NOCONTENT` skips journaling reused freelist leaves, a valid hot-journal rollback can leave non-zero bytes inside unused freelist leaves (14 of 47 kills in step 3). Compare rolled-back files with `VACUUM INTO` canonical SHA-256 or logical equality (E3), not raw file SHA-256 (E1).
7. **Size a multi-database Go service by its open-connection cap, not its file count (Experiment 4.2 Parts B, C, D).** Page-cache memory scales linearly with open connections—even when multiple connections open the same file (`134.6 MB` `CACHE_USED`, `~252–254 MiB` RSS for 64 connections at `cache_size=-2000`). An 8-connection LRU pool capped memory flat at `35.5 MiB` across all 64 files.
8. **Account for both `ENABLE_MEMORY_MANAGEMENT` pooling and the driver's `~2x` RSS multiplier (Experiment 4.2 Parts B and D).** With `modernc.org/sqlite`, `cache_size` adds to one process-wide `PGroup` budget (`open × 482` pages at `-2000`), so active connections automatically reuse idle connections' pages and closing an idle connection primarily frees its fixed overhead (~5 KiB `CACHE_USED`, 32 KiB `-shm` mapping, Go/driver structs) and shrinks the group cap. Budget roughly `2 × summed CACHE_USED` for process RSS (`~4.0 MiB` per connection at `-2000`) because `modernc.org/memory` rounds ~4.3 KiB page allocations into 8 KiB slots.
9. **Use `PRAGMA mmap_size` to move read-cache memory out of SQLite's heap into the kernel page cache (Experiment 4.2 Part E).** Covering the 3.9 MiB file with `mmap_size` cut `CACHE_USED` from `482` pages (`2.1 MB`) to `2` pages (`13.8 KB`) while shifting the `4.0 MiB` resident footprint into file-backed RSS (`RssFile`), which the OS shares across processes and reclaims under memory pressure.

---

## 8. Knowledge check

Answers are in [answers.md](answers.md).

1. In Experiment 4.1 Part D (`synchronous=FULL`), why did killing the writer during kills 01–25 leave a `-journal` file on disk that SQLite ignored and did *not* delete on reopen, whereas kills 26–46 triggered rollback and deleted `-journal`?
2. In Experiment 4.1 Part D (step 3), 14 kill points at `synchronous=FULL` reopened to a database whose raw SHA-256 matched neither pre-step nor post-step, yet Part D2 (step 1) reopened to exact pre-step or post-step bytes at every kill point. What exact SQLite mechanism caused the difference, which page differed in step 3, and how did the experiment verify the database was still logically pre-step?
3. Compare what happened in Experiment 4.1 Part E when `t.db-journal` was deleted after a crash at database write 10 of 18 (E1) versus after a crash on entry to `sync t.db` (E3). Why is E3 dangerous even though `PRAGMA integrity_check` returns `ok`?
4. In Experiment 4.2 Part B (`64` connections, `cache_size=-2000`), connection 1 held `0` cached pages and connection 64 held `1,004` cached pages—more than double its `482`-page budget—while the sum across all 64 connections was exactly `30,848` (`64 × 482`). Which compile option and `pcache1.c` mechanism caused this, and why did RSS grow by `253.7 MiB` when summed `CACHE_USED` was only `128.3 MiB`?
5. In Experiment 4.2 Part E, setting `PRAGMA mmap_size=8224768` cut `CACHE_USED` from `2,102,741` bytes (`482` pages) to `13,781` bytes (`2` pages), yet process RSS still grew by `4.2 MiB`. What happened to the memory, and how did `/proc/self/status` prove it?
