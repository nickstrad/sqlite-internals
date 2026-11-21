---
title: "Chapter 3: B-tree page changes, VACUUM INTO canonical form and logical hashes as measured at 3.53.4"
summary: "What a rebuild verifier must know: batching does not change WAL-mode bytes the way it does in rollback mode, VACUUM INTO is canonical only for the same schema creation order, WAL frames shipped can exceed pages changed, the file shrinks only with auto_vacuum, freed pages keep old bytes."
tags: [sqlite, btree, vacuum, dbhash, walfleet]
updated: 2026-11-21
verified: 2026-11-21 — Linux amd64, Go 1.26, SQLite 3.53.4 via modernc.org/sqlite v1.60.1; evidence in the walfleet repo
---

# Chapter 3: B-tree page changes, VACUUM INTO canonical form and logical hashes as measured at 3.53.4

What a rebuild verifier must know: batching does not change WAL-mode bytes the way it does in rollback mode, VACUUM INTO is canonical only for the same schema creation order, WAL frames shipped can exceed pages changed, the file shrinks only with auto_vacuum, freed pages keep old bytes.

Source: the `walfleet` repo (github.com/nickstrad/walfleet), chunk `ch03`. Working notes: `agent_work/ch03/build-notes.md`; review and adversarial records in the same directory.

## Corrections to the outline


### Correction 1

1. **"The same rows written in different … batches give different files" is false in WAL mode
   for append-only builds.** 3.3 part A: R0's final rows inserted in one transaction and inserted
   with one transaction per statement (same order, recipe settings: WAL, `secure_delete=ON`, one
   connection) give **byte-identical files** (sha256 `224810279caf4016…` both). The outline's
   evidence line "three different sha256 values before" does not hold for the outline's three
   ways (one transaction / one per statement / row by row in another order): there are two. What
   does give different bytes: insert order (the shuffled build, `dc2b3bbb…`) and history (R0
   itself, built by 31 transactions with updates and deletes, `24aa158f…`), and the replay of the
   capture (indexes created after the rows, `e3c97131…`). 3.3 reports 4 distinct sha256 among
   r0 / one-transaction / per-statement / shuffled / replayed. In **rollback mode** (part A2) the
   two batchings do differ, but only in the change counter: E1 differ, E2 equal, differing bytes
   [26 27 94 95], counters 3 and 1463. Why WAL hides it: the change counter is written only when
   page 1 is dirty and holds `dbFileVers + 1` (ch01 correction 1, ch02 correction 1), and
   append-only inserts leave no other trace of commit boundaries. Same finding as ch01
   correction 2, now for a whole build rather than two merged steps.

### Correction 2

2. **VACUUM INTO is a canonical form only for databases whose schema has the same creation order
   and the same schema cookie.** `sqlite3RunVacuum` (vacuum.c:143-427) creates the tables in
   `sqlite_schema` order (`SELECT sql FROM sqlite_schema WHERE type='table'…`, no ORDER BY, so
   rowid = creation order), copies each with `INSERT INTO … SELECT*FROM`, and copies the meta
   values with the schema cookie **+1** (`aCopy[]`: `BTREE_SCHEMA_VERSION, 1`). Measured in 3.3:
   - part C1: same content, tables `a` and `b` created in the other order: sources equal at E3,
     VACUUM INTO outputs differ at E1 and at E2 (3 of 3 pages).
   - part C2: a history that created and dropped a scratch table: sources equal at E3 (and dbhash
     equal), schema cookies of the canonical files 2 and 4, outputs differ only at byte 43 (E2
     equal).
   - part A: the backup-API copy of R0 has schema cookie 1 instead of 4, because
     `sqlite3_backup_step` sets the destination cookie to (old destination cookie + 1)
     (backup.c:448; the destination was empty, cookie 0). Its VACUUM INTO differs from R0's at
     byte 43 only (cookies 5 and 2). So "one sha256 after canonicalization" holds for the four
     builds and the replay, not for a backup copy.
   - part C3: an index created before vs after the rows canonicalizes to identical bytes.
   - VACUUM INTO also copies the default cache size (header bytes 48-51, `aCopy[]`
     `BTREE_DEFAULT_CACHE_SIZE`). Two files that differ only in `PRAGMA default_cache_size`
     canonicalize to different bytes (adversarial EQ-03). E3 now hashes `default_cache_size` too
     (seventh logical pragma), so E3 equality plus the same schema history does give E1-equal
     canonical forms.
   - part C1 (repair round): the R2 capture writes CREATE TABLE statements in `sqlite_schema` rowid
     order (creation order), so replaying the capture of a source whose tables were created out of
     name order canonicalizes to the source's own canonical bytes (E1 equal). The two sources'
     captures differ: the capture records creation order.
   Platform consequence: comparing canonical forms at E1 needs the same schema history (creation
   order and number of schema changes); otherwise compare canonical forms at E2, or at E3.

### Correction 3

3. **The outline's 3.3 evidence "dbhash agrees with our logical hash on equality" holds for the
   builds, but dbhash has two more blind spots than the research listed.** Reading
   tool/dbhash.c `hash_one_query` (281-360): each value is hashed as a type tag ("0".."4") then
   its bytes, with **no length** for TEXT/BLOB and **no separator** between rows or tables, and
   the table name is not hashed with the rows. So (a) `('a3','b')` and `('a','3b')` hash the same
   ("3a33b"), and (b) a row moved from the end of table `a` to the start of table `b` (same column
   count) hashes the same. Both run in 3.3 part B and in the unit tests: dbhash `equal`, ours
   `differ`. The research's four blind spots are confirmed as listed (implicit rowid, sqlite_sequence,
   user_version: dbhash equal; integer 1 vs real 1.0: dbhash differ, sqldiff's blind spot).

### Correction 4

4. **WAL frames shipped can exceed pages changed.** 3.1 part A: for all 2061 appends the WAL
   frames of the commit name exactly the pages whose bytes changed. 3.1 part B (a middle split):
   the commit wrote 6 frames `[1 2 3 4 5 8]` but only 5 pages changed `[1 2 4 5 8]`; page 3 was
   written by `balance_nonroot` with the same bytes it already had (sha256 `c0995adc833eadb1`
   before and after). A page SQLite marks writable is a frame, whatever its bytes. So "the number
   of pages a write dirties is the number of WAL frames it ships" is right if "dirties" means
   "SQLite wrote", not "bytes changed".

### Correction 5

5. **The first append split and the first depth change are the same insert.** The outline lists
   "the first append split … and the first depth change" as separate events. In a table that
   starts empty, the root leaf (page 2) overflows first: `balance()` calls `balance_deeper` (root
   content copied to a new page 3) and then, on the next loop iteration, `balance_quick` on page
   3 (new page 4 for the new row). 3.1 prints that event, then the first append split at depth 2
   (insert #9) and the second depth change (2→3, insert #2053).

### Correction 6

6. **Outline reading-path item 5, "balance_nonroot (8245–9027) — its header comment only"**: the
   header comment is lines 8204-8244, before the function's first line 8245. SPANS.txt lists both
   (`8204 8244 redistributes` and `8245 9027 balance_nonroot`). Outline item 1, `btreeInt.h
   149–228`: the comment it cites ends at 210; 211-228 are the `MX_CELL_SIZE` and `MX_CELL`
   macros. Kept as the outline has it. All other outline spans are exact.


### Correction 7

7. **"Most inserts change one page" holds for appends and for small rows, not in general**
   (adversarial SPL-01, SPL-03, SPL-05; now 3.1 part E). One row per transaction, WAL:
   - random keys, 1000-byte rows (4 per leaf), 250 inserts: one page 86, added a page 70, several
     pages without adding one 94 (balance_nonroot moving cells between siblings), 1..6 pages each.
   - random keys, 40-byte rows (about 85 per leaf), 600 inserts: one page 566 — most again. The
     rule depends on how many rows fit in a leaf.
   - ascending keys, 5000-byte rows (each overflows; max local 4061): one page 0 of 25; every
     insert writes the leaf, its overflow page(s) and page 1 (the file grows): 3..5 pages.
   - ascending keys into a WITHOUT ROWID table (an index b-tree), 400-byte rows: the two inserts
     that added a page at depth 2 changed 5 pages each. `balance_quick` requires a table leaf
     (`intKeyLeaf`, btree.c 9185-9189), so index b-trees always split through `balance_nonroot`.
     3.1's "an append split at depth 2 changes three pages" is about rowid tables only.

### Correction 8

8. **"The file never shrinks on its own" holds only with `auto_vacuum=NONE`** (SQLite's default,
   the recipe's; adversarial DEL-02; now 3.2 auto_vacuum part). 200 rows, DELETE of 150:
   NONE 52 → 52 pages (freelist 37), `PRAGMA incremental_vacuum` does nothing; FULL 53 → 16 pages
   at the DELETE's own commit, freelist 0; INCREMENTAL 53 → 53 (freelist 37), then
   `PRAGMA incremental_vacuum` → 16. (FULL and INCREMENTAL files start one page larger: the
   pointer-map page.)

### Correction 9

9. **"A freed page keeps its old bytes unless secure_delete is on" is true of the database file
   only.** With WAL, the frames the INSERT wrote stay in the `-wal` after the DELETE commits
   (adversarial DEL-05; now 3.2 WAL part): 40 rows inserted then deleted with secure_delete=ON
   and wal_autocheckpoint=0: all 40 readable in the `-wal` after the DELETE, still 40 after
   `wal_checkpoint(PASSIVE)` (it copies the zeroed pages into the database file but does not
   erase the WAL), 0 after `wal_checkpoint(TRUNCATE)`, 0 in the database file at rest.

### Correction 10

10. **The build round's 3.2 check "page_count grows only in a transaction that started with an
    empty freelist" was too strong** (adversarial DEL-06). It held for the 4-row batches, but the
    rule is per page allocation: a transaction that needs more pages than the freelist holds takes
    the freelist first and then grows the file in the same transaction (3.2 large-transaction
    part: freelist 2 → 0 and page_count 12 → 18 in one 32-row transaction). The check now reads
    "every transaction that added pages ended with an empty freelist".

Everything else in the chapter-3 section was confirmed. The [unprobed] claims of 3.1 ("an insert
that fills a leaf changes several", "the tree gains a level while the root keeps its page number")
hold; "most inserts change one page" holds with correction 7. The [probed] claims of 3.2 and 3.3
hold with corrections 1-3 and 8-10.

