---
title: "sqlite-internals chapter 2: database header and page facts measured at 3.53.4"
summary: "What a plain-Go reader of SQLite files must know: in WAL mode the header change counter is written only with page 1; at-rest means no -wal content and no hot journal; WITHOUT ROWID roots are index pages; SQLite accepts a partial last page."
tags: [sqlite, file-format, wal, sqlite-internals]
updated: 2026-11-20
verified: 2026-11-20 — Linux amd64, Go 1.26, SQLite 3.53.4 via modernc.org/sqlite v1.60.1; evidence in the sqlite-internals repo
---

# sqlite-internals chapter 2: database header and page facts measured at 3.53.4

What a plain-Go reader of SQLite files must know: in WAL mode the header change counter is written only with page 1; at-rest means no -wal content and no hot journal; WITHOUT ROWID roots are index pages; SQLite accepts a partial last page.

Source: the `sqlite-internals` course repo (github.com/nickstrad/walfleet), chunk `ch02`. Working notes: `agent_work/ch02/build-notes.md`; review and adversarial records in the same directory.

## Corrections to the outline

1. **WAL mode: the header is not a per-commit fingerprint, and the database file does not change
   at commit.** Outline section 7 (Platform implications) says "the header is a cheap
   fingerprint; bytes 24 and 92 matter to anything that caches or copies files". That holds in
   rollback mode only. Measured in 2.1 part C:
   - C1, one connection, three commits that each write page 1: the page-1 image SQLite serves
     (`sqlite_dbpage`) shows counter 29 -> 30 at the first commit and then **30 at every later
     commit**; bytes 92-95 the same. The database file's page 1 does not change at all until the
     checkpoint at close (`file vs base: no header byte changed` after each commit). After the
     close the file has counter 30, not 32.
   - C2, three commits that do not write page 1 (`UPDATE accounts ... WHERE id = 1`): **no header
     byte changes**, before or after the checkpoint. The WAL grows 4152 -> 8272 -> 12392 bytes
     (one 4120-byte frame per commit). So a WAL-mode database can change without any change to
     its header, even after checkpoint.
   - C3, each commit on a new connection while an idle second connection keeps the WAL alive:
     counter 30, 31, 32. The counter advances once per *connection that writes page 1*.
   - Mechanism (source): `pagerWalFrames` calls `pager_write_changecounter` only when the first
     page in the commit's list is page 1 (pager.c:3264); `pager_write_changecounter` writes
     `dbFileVers + 1` (pager.c:3135); `dbFileVers` is refreshed only when page 1 is read through
     `readDbPage` (pager.c:3093-3110). This is the ch01 correction 1 restated in header terms, now
     shown with the database file and the current page 1 side by side.
   - Platform consequence: to detect "this database changed" in WAL mode use `PRAGMA
     data_version` (per connection), or the WAL itself; never bytes 24-27 of the file.
2. **The research probe's "counter stays constant across commits on one connection" is only
   true for commits that write page 1 after the first.** The first commit on a connection that
   writes page 1 does advance it (29 -> 30 in C1). Stated precisely in correction 1.
3. **A truncated file: SQLite accepts a partial last page; our parser refuses it.** 2.2 F2: the
   last page (a freelist leaf) cut in half → SQLite `integrity_check: ok` (it rounds the file
   size up to whole pages and reads the missing bytes as zeros; `lockBtree` uses
   `sqlite3PagerPagecount`, and the in-header size 6 is not larger than the rounded file size 6).
   `internal/dbfile` returns `ErrTruncated` ("file size 22528 is not a multiple of the page size
   4096"). Deliberate: a byte-level capture of such a file could not rebuild it, so the inspector
   is stricter than SQLite here. Not an outline error, but the outline's "what the inspector must
   reject" should say the inspector is stricter than SQLite on purpose.
4. **The outline's reading-path span `btreeParseCellPtr` (1259-1385) covers two functions.**
   `btreeParseCellPtr` is 1259-1346 and `btreeParseCellPtrIndex` is 1347-1385. SPANS.txt lists
   them separately. All other outline spans are exact (start line has the symbol, end line is the
   closing brace). `btreeInt.h 1-228` is listed twice: `1 228 BTrees` (the span checker needs the
   symbol in the first 12 lines; line 12 reads "This file implements an external (disk-based)
   database using BTrees") and `45 228 FORMAT` (the "FORMAT DETAILS" heading at line 45).
   Spans added that the outline does not list: `btreeParseCellAdjustSizeForOverflow` 1178-1207
   (the local/overflow formula), `readDbPage` pager.c 3067-3120, `pagerWalFrames` pager.c
   3225-3282, `pager_incr_changecounter` pager.c 6363-6437 (all three needed to explain the WAL
   counter behaviour in 2.1).
5. **"Capture a database that is not closed (a `-wal` exists) and confirm the tool refuses"**:
   refined. "Not at rest" is: a `-wal` with size > 0, or a hot `-journal` (exists, size > 0,
   first byte non-zero, the same test as `hasHotJournal`, pager.c:5180, which reads one byte and
   treats 0 as not hot). A zero-byte `-wal` (left by `journal_mode=WAL` with `PERSIST_WAL` or
   after `wal_checkpoint(TRUNCATE)`), and a `-journal` of size 0 (TRUNCATE mode) or with a
   zeroed header (PERSIST mode), are at rest: the database file alone is complete.
6. **An interrupted rollback-mode transaction does not by itself leave a hot journal.** With
   `synchronous` FULL or NORMAL the journal header is written with its magic zeroed
   (pager.c:1494-1501, `memset(zHeader, 0, ...)` unless noSync, MEMORY mode or SAFE_APPEND); the
   magic is written only when the journal is synced (pager.c:4377) — at COMMIT or when dirty
   pages spill to the database file mid-transaction. So 2.3 D2 makes the hot journal with a
   cache spill (`cache_size=10`, update every document body, exit before COMMIT): the database
   file is then changed (15 of 17 bodies already zero in the file alone) and the journal is hot.
   A child that exits before any spill leaves the database file untouched and a non-hot journal.

Everything else in the chapter-2 section was confirmed (details per experiment below). The two
[unprobed] claims of 2.2 (census sums to `page_count`, no page twice or missed, freelist length
= `freelist_count`, per-page `ncell`/`unused` = `dbstat`, row hashes = SQLite) all hold.

