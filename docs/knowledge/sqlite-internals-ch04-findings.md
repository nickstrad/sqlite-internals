---
title: "sqlite-internals chapter 4: rollback journal, hot journals and page-cache budgets as measured at 3.53.4"
summary: "Pager facts for a platform builder: a journal written before COMMIT is not hot, a WAL database is still born through a -journal, cache_size is a share of a group limit in the modernc build, process memory is about twice CACHE_USED, a cache of N pages holds N-1."
tags: [sqlite, pager, rollback-journal, page-cache, sqlite-internals]
updated: 2026-11-21
verified: 2026-11-21 — Linux amd64, Go 1.26, SQLite 3.53.4 via modernc.org/sqlite v1.60.1; evidence in the sqlite-internals repo
---

# sqlite-internals chapter 4: rollback journal, hot journals and page-cache budgets as measured at 3.53.4

Pager facts for a platform builder: a journal written before COMMIT is not hot, a WAL database is still born through a -journal, cache_size is a share of a group limit in the modernc build, process memory is about twice CACHE_USED, a cache of N pages holds N-1.

Source: the `sqlite-internals` course repo (github.com/nickstrad/walfleet), chunk `ch04`. Working notes: `agent_work/ch04/build-notes.md`; review and adversarial records in the same directory.

## Corrections to the outline


### Correction 1

1. **"The reopened file's sha256 equals the pre-step hash or the post-step hash, never a third
   value" is false at the byte level when the transaction reuses freelist pages.** It holds
   logically, and it holds byte for byte when the pre-step file has no freelist.
   - Evidence (4.1, step 3, pre-step = recipe step 2, whose page 22 is a freelist leaf): at
     `synchronous=FULL`, kills 33 to 46 (from the first database write that touches page 22 up to
     and including the kill on the journal unlink) reopen to a file equal to the pre-step file in
     every page except page 22. Summary line: `synchronous=FULL   BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBbbbbbbbbbbbbbbA`
     (B = pre-step exact, b = pre-step except reused freelist leaves, A = post-step exact). Same
     shape at NORMAL (14 b) and OFF (13 b). Every `b` file: `integrity_check ok`, same file size
     as pre-step, the only differing page is in the pre-step file's freelist-leaf list (our
     `dbfile` census), and `VACUUM INTO` of the reopened file has the same sha256 as `VACUUM INTO`
     of the pre-step file (rung R2's canonical form, E3 turned into E1).
   - Mechanism (source): when the B-tree takes a page off the freelist it fetches it with
     `PAGER_GET_NOCONTENT` (`btree.c:6740`: `noContent = !btreeGetHasContent(pBt, *pPgno)?
     PAGER_GET_NOCONTENT : 0;`). `getPageNormal` then sets the page's bit in `pInJournal` without
     writing a journal record (`pager.c:5645-5656`, comment at 5553-5562), because the old content
     of a free page does not matter. A hot-journal rollback therefore cannot restore those bytes;
     the page goes back on the freelist (its trunk page is restored from the journal) but keeps the
     new bytes. With `secure_delete=ON` the pre-step freelist leaf held zeros, so the difference is
     visible.
   - The same file is byte-exact in two other cases: kills before the first database write (no
     page changed yet), and a ROLLBACK by the live connection (4.1 part C: `pre-step exact bytes`,
     because the reused page was only in the cache and never written to the file).
   - Shown exactly as the outline says on a step whose pre-step file has no freelist: 4.1 part D2,
     step 1 (pre-step = step 0, 4 pages, freelist `[]`): every kill at all three levels reopens to
     exactly the pre-step or post-step bytes (`BBBB…BA` on all three lines), check line
     `the pre-step file has no freelist, and every reopened database equals the pre-step or
     post-step bytes exactly`.
   - Consequence for the platform: after a crash in rollback mode, compare to an expected state at
     E3 (or E1 of the `VACUUM INTO` canonical form), not E1 of the raw file. ch01 found the same
     NOCONTENT effect for a rolled-back SAVEPOINT (ch01 notes, "Rolled-back SAVEPOINT leaves bytes
     behind").

### Correction 2

2. **The outline's call order "journal write → journal fsync → directory fsync → database write →
   database fsync → journal unlink" is the NORMAL order with one detail missing, and FULL adds a
   step.** Measured (step 3, 7 journaled pages, 18 database writes):
   - FULL: `journal-header, journal-record x21, journal-sync, dir-sync, journal-count, journal-sync, db-page x18, db-sync, journal-unlink` — 4 syncs.
   - NORMAL: `journal-header, journal-record x21, journal-count, journal-sync, dir-sync, db-page x18, db-sync, journal-unlink` — 3 syncs.
   - OFF: `journal-header, journal-record x21, db-page x18, journal-unlink` — 0 syncs.
   The missing detail is `journal-count`: a 12-byte write at offset 0 of the journal that puts the
   magic number and the record count into the header. In FULL mode it sits between two journal
   syncs; in NORMAL mode it comes before the only journal sync. The research probe's counts (3 at
   NORMAL, 4 at FULL) are CONFIRMED; OFF gives 0. The directory sync happens once, at the first sync
   of a newly created journal (`unixSync` DIRSYNC flag, as chapter 7's inventory says).

### Correction 3

3. **The `-journal` written before COMMIT is not hot.** With `synchronous` FULL or NORMAL the
   journal header is first written with its magic and record count set to zero
   (`writeJournalHdr`, `pager.c:1495-1502`: `memset(zHeader, 0, sizeof(aJournalMagic)+4)` unless
   `noSync`); `syncJournal` writes them at commit (`pager.c:4377-4378` builds them, `4408-4410`
   writes them). `hasHotJournal` treats a
   journal whose first byte is 0 as not hot (`pager.c:5244-5251`). So a crash before
   `journal-count` leaves a "cold" journal that SQLite ignores and does not delete: 4.1 shows
   `cold -journal left (N bytes, first byte 0x00)` after reopening for kills 1-25 at FULL (and
   1-22 at NORMAL). The database file is untouched in all those cases, so ignoring the journal is
   correct. With `synchronous=OFF` the header carries the magic and `0xffffffff` (meaning "count
   the records from the file size") from the start, so the journal is hot as soon as its header
   exists (OFF kills 2-22: `hot, but restores no page` or a partial list; all reopen to pre-step).
   A platform that checks "is this database at rest" must therefore treat "a `-journal` exists"
   and "a hot journal exists" differently (ch02's correction 5 already defines at-rest by the hot
   test).

### Correction 4

4. **Even a database created by the platform's WAL opener is born through a `-journal`.** The
   brief says new databases start in rollback mode until switched. True (4.1 part A), and more:
   the switch itself is a rollback-mode commit. `PRAGMA journal_mode=wal` on a new empty file
   writes page 1 (bytes 18 and 19 set to 2) through a rollback journal: 4.1 part A shows
   `open switch.db-journal, pwrite64 off=0 len=512, sync, pwrite64 off=0 len=12, sync, unlink` for
   the PRAGMA and `-journal []` for the CREATE TABLE after it. `sqlitex.Open` with
   `journal_mode=wal` on a new file does exactly the same (`platform.db-journal` opened, synced
   twice, unlinked, inside `sqlitex.Open`). Mechanism: the WAL is opened only after page 1 records
   version 2, and that write is a normal pager write transaction in rollback mode
   (`sqlite3BtreeSetVersion`, btree.c; not copied). A crash inside `sqlitex.Open` of a brand-new
   file can therefore leave a `-journal` beside a WAL-to-be database (the file is then still 0 or
   1 page in rollback mode). Not demonstrated with a kill.

### Correction 5

5. **`cache_size` is not a per-connection cap in this driver build; it is each connection's share
   of one process-wide pool.** The outline's claim "per-connection cache bytes ≤ cache_size budget
   plus a stated overhead" and "`cache_size` is a budget per connection, not per process" need
   restating for modernc:
   - The Go driver is compiled with `SQLITE_ENABLE_MEMORY_MANAGEMENT` (4.2 setup line, value 1;
     ch01 found it among the driver-only options). With that option every page cache in the
     process belongs to one `PGroup` with one LRU list (`pcache1Init`, `pcache1.c:718-719`:
     `#if defined(SQLITE_ENABLE_MEMORY_MANAGEMENT) pcache1.separateCache = 0;`). The group's limit
     `nMaxPage` is the sum of every open cache's `nMax` (`struct PGroup`, `pcache1.c:158-165`;
     `pcache1Cachesize` 814-832 adds each cache's share). When a cache is full,
     `pcache1FetchStage2` recycles the group's least recently used page, whichever connection owns
     it (`pcache1.c:899-912`: `pPage = pGroup->lru.pLruPrev; … pOther = pPage->pCache;`).
   - Measured (4.2 part B, 64 connections to 64 files, each `cache_size=-2000` = 482 cached pages
     when full): after the second connection scans, connection 1 holds 0 pages and connection 2
     holds 964; from 4 connections on, the most recent scanner holds all 1004 pages of its file and
     every idle connection holds 0. The SUM is always exactly (open connections) × 482 pages.
   - So with this driver: the bound that holds is the process total, Σ over open connections of
     each one's budget. An active connection can use far more than its own `cache_size`; idle
     connections give their pages up. The C library built without that option (the CLI here: no
     ENABLE_MEMORY_MANAGEMENT) uses a separate group per cache (`separateCache` true,
     `pcache1.c:720-721`), so each connection is held to its own budget. Not measured on the CLI
     (it has one connection and no `sqlite3_db_status` access from Go); stated from the source.
   - For platform sizing the conclusion the outline wanted still holds: memory = open
     connections × budget (B, C), and a connection cap bounds it (D).

### Correction 6

6. **Process memory is about twice the summed `CACHE_USED` with this driver.** 4.2 part B: RSS
   growth at 64 connections 252.9 MiB for 128.3 MiB of summed `CACHE_USED` (ratio 1.97, INFO line;
   ratio stable across runs). Mechanism: modernc's C library allocates through
   `modernc.org/memory` v1.12.1, whose allocator uses power-of-two slots up to 16 KiB
   (`maxSlotSizeLog = pageSizeLog - 2` with `pageSizeLog = 16`; slot `log =
   bits.Len(roundup(size, 16) - 1)`), so each cache page allocation of about 4.3 KiB (4096-byte page
   + PgHdr1 + extra) takes an 8 KiB slot. This is a property of the driver, not of SQLite; with the
   C library (glibc malloc) the overhead would be small. The check in 4.2 only claims "RSS grows by
   at least the summed CACHE_USED".

### Correction 7

7. **`CACHE_USED` depends on the database's path length.** `sqlite3PagerMemUsed` adds
   `sqlite3MallocSize(pPager)`, and the Pager allocation holds the database, journal and WAL file
   names (`pager.c:6881-6887`). Running 4.2 inside the container (paths `/work/4.2/…`) gave
   `CACHE_USED` 126 bytes smaller per connection than on the host. The experiment therefore prints
   cached pages (path independent) on EXACT lines and raw bytes on INFO lines. Per cached page
   `CACHE_USED` counts 4352 bytes (4096 + 256 of headers; measured from cache_size 200 minus 100);
   the fixed part was 5077 bytes on the host (Pager allocation 981 bytes + one 4096-byte buffer).

### Correction 8

8. **A cache of N pages holds N−1.** `cache_size=100` held 99 pages and `cache_size=200` held 199
   (4.2 part A). `pcache1FetchStage2` recycles once `nPage+1 >= nMax` (`pcache1.c:899-901`).
   `cache_size=-2000` gives `nMax` = 2048000 / (4096 + szExtra) = 483 → 482 pages held.

Nothing else in the chapter-4 section of the outline was false here. The two failure cases
("Astra will try" items 1-2) and `synchronous=OFF` (item 3) are demonstrated inside 4.1 as the brief
asked.

