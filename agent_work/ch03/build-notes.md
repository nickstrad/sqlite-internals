# Chunk 3 build notes (plain data for the prose pass)

Builder: Claude Opus 5.5, branch `chunk/ch03`, 2026-11-20. Machine: linux/amd64, Go 1.26.8, ext4.
dbhash used for the evidence: `_tools/bin/dbhash`, a `./configure && make dbhash` build of the
pinned tree (the same binary `go run ./cmd/course build-tools` produces). No CLI is needed by any
chapter-3 experiment. darwin: cross-built and cross-vetted only, never run.
All three experiments were also run inside the Debian `lab` container
(`docker compose run --rm -T lab go run ./ch03-btrees-and-page-management/experiments/<name>`, its
dbhash in `/opt/course-tools/bin`): EXACT output identical to the host's for all three.

All source line numbers are at the pin (3.53.4). `go run ./cmd/course check spans ch03` passes (22
spans); `check sources ch03` passes (2 files: `btree.c`, `btreeInt.h`, copied whole from
`$SQLITE_SRC/src/`). `check evidence ch03` passes (each experiment run twice, identical to the
evidence apart from `INFO:` lines).

## Corrections to the outline

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
   Platform consequence: comparing canonical forms at E1 needs the same schema history (creation
   order and number of schema changes); otherwise compare canonical forms at E2, or at E3.
3. **The outline's 3.3 evidence "dbhash agrees with our logical hash on equality" holds for the
   builds, but dbhash has two more blind spots than the research listed.** Reading
   tool/dbhash.c `hash_one_query` (281-360): each value is hashed as a type tag ("0".."4") then
   its bytes, with **no length** for TEXT/BLOB and **no separator** between rows or tables, and
   the table name is not hashed with the rows. So (a) `('a3','b')` and `('a','3b')` hash the same
   ("3a33b"), and (b) a row moved from the end of table `a` to the start of table `b` (same column
   count) hashes the same. Both run in 3.3 part B and in the unit tests: dbhash `equal`, ours
   `differ`. The research's four blind spots are confirmed as listed (implicit rowid, sqlite_sequence,
   user_version: dbhash equal; integer 1 vs real 1.0: dbhash differ, sqldiff's blind spot).
4. **WAL frames shipped can exceed pages changed.** 3.1 part A: for all 2061 appends the WAL
   frames of the commit name exactly the pages whose bytes changed. 3.1 part B (a middle split):
   the commit wrote 6 frames `[1 2 3 4 5 8]` but only 5 pages changed `[1 2 4 5 8]`; page 3 was
   written by `balance_nonroot` with the same bytes it already had (sha256 `c0995adc833eadb1`
   before and after). A page SQLite marks writable is a frame, whatever its bytes. So "the number
   of pages a write dirties is the number of WAL frames it ships" is right if "dirties" means
   "SQLite wrote", not "bytes changed".
5. **The first append split and the first depth change are the same insert.** The outline lists
   "the first append split … and the first depth change" as separate events. In a table that
   starts empty, the root leaf (page 2) overflows first: `balance()` calls `balance_deeper` (root
   content copied to a new page 3) and then, on the next loop iteration, `balance_quick` on page
   3 (new page 4 for the new row). 3.1 prints that event, then the first append split at depth 2
   (insert #9) and the second depth change (2→3, insert #2053).
6. **Outline reading-path item 5, "balance_nonroot (8245–9027) — its header comment only"**: the
   header comment is lines 8204-8244, before the function's first line 8245. SPANS.txt lists both
   (`8204 8244 redistributes` and `8245 9027 balance_nonroot`). Outline item 1, `btreeInt.h
   149–228`: the comment it cites ends at 210; 211-228 are the `MX_CELL_SIZE` and `MX_CELL`
   macros. Kept as the outline has it. All other outline spans are exact.

Everything else in the chapter-3 section was confirmed. The [unprobed] claims of 3.1 ("most
inserts change one page", "an insert that fills a leaf changes several", "the tree gains a level
while the root keeps its page number") all hold; details below. The [probed] claims of 3.2 and
3.3 hold with corrections 1-3.

## Shared package changes (all in `internal/equal`; every other package untouched)

Backward compatible: nothing existing was removed or renamed. `equal.go` gained two constants,
two `Result` fields and two `String()` branches; everything else is new files.

### Level constants and `Result` (equal.go)

- `E2Pages = 2`, `E3Logical = 3`; `Level.String()` gives "E2 pages", "E3 logical".
- `Result` gained `Ignored []ByteRange` (the ranges an E2 comparison skipped) and
  `IgnoredDiffering []int` (file offsets inside those ranges where the two images differ; set
  only when the result is equal). `Result.String()` for such a result:
  `E2 pages: equal ignoring header bytes 24-27, 40-43, 92-99 (bytes differ only at [27 43 95])`.
  E1 results print exactly as before.

### E2 (pages.go)

- `ByteRange{Start, End}` is half-open `[Start, End)`; `String()` prints `24-27` or `43`.
  `DeclaredHeaderIgnore = {24,28}, {40,44}, {92,100}` (change counter; schema cookie;
  version-valid-for and library version). Bytes 18-19 are deliberately NOT ignored: they say WAL
  or rollback (research report Q4).
- `CompareImagesIgnoring(a, b, pageSize, ranges) (Result, error)`: page by page; a page that
  differs only inside ignored ranges counts as equal and its differing offsets are recorded; any
  other byte difference, or a size difference, makes the page differ. Errors: page size ≤ 0, a
  range with Start < 0 or End ≤ Start. The ranges are file offsets, so they only ever touch page 1.
- `ComparePages(a, b, pageSize)` = the declared list; `ComparePageFiles(a, b, pageSize)` reads two
  files. `FormatRanges`, `ValidateRanges` exported for printing and checks.

### Page changes between manifests (changes.go)

- `ChangedPages(before, after Manifest) []PageChange` — pages whose sha256 or role differ, plus
  pages present in only one manifest, sorted by page number. `PageChange{Page, RoleBefore,
  RoleAfter}`; `Added()`, `Removed()`; `String()` gives `4(new table-leaf)`,
  `2(table-leaf->table-interior)`, `6(table-leaf)`, `9(gone, was overflow)`.
  `FormatPageChanges`, `PageNumbers`. Used by 3.1; meant for ch05's "frames vs pages" too.

### E3, the Go logical hash (logical.go)

What is hashed, in this order, into one sha256 stream (`LogicalFormat` = "sqlite-internals logical
hash v1" is written first):

1. Schema: `SELECT type, name, tbl_name, sql FROM sqlite_schema ORDER BY type, name`. `rootpage`
   is left out (physical). `sql` NULL (autoindexes) is hashed as NULL. Each field is tagged and
   length-prefixed.
2. Tables: `pragma_table_list` rows with `schema='main'`, type `table` or `shadow`, excluding
   `sqlite_schema`, ordered by name. That includes `sqlite_sequence` and `sqlite_stat*` when they
   exist. Views and virtual tables contribute only their schema rows (a virtual table's rows
   depend on its module; its shadow tables are hashed as tables).
   Per table: name, rowid/without-rowid flag, column count and names, then every row:
   - rowid table: `SELECT <rowid alias>, +"c1", +"c2", … FROM "t" ORDER BY <rowid alias>`; the
     alias is the first of `rowid`, `_rowid_`, `oid` that is not a column name (error "columns
     named rowid, _rowid_ and oid hide the rowid" if all three are taken). The rowid is always
     hashed, which is what catches implicit-rowid differences.
   - WITHOUT ROWID table: `SELECT +"c1", … ORDER BY <primary key columns in pk order>`.
   - The unary `+` on each column is there so the driver sees an expression with no declared
     type: modernc converts TEXT in DATE/DATETIME/TIMESTAMP columns to `time.Time` when it knows
     the declared type, which would lose the storage class. With `+`, the Go type is exactly the
     storage class: nil → NULL, int64 → INTEGER, float64 → REAL, string → TEXT, []byte → BLOB
     (`StorageClassOf`; any other Go type is an error). Unit test checks this against `typeof()`
     including a DATETIME column holding text.
   - Value encoding (`EncodeValue`): tag byte `N`, `I` + 8-byte big-endian two's complement, `R` +
     8-byte big-endian IEEE-754 bits (so 1 vs 1.0 and 0.0 vs -0.0 differ), `T` / `B` + 8-byte
     length + bytes. Rows are `R` + value count; a table ends with `E` + row count. Length
     prefixes and separators are what close dbhash's two extra blind spots (correction 3).
3. Persistent pragmas, in `LogicalPragmas` order: `page_size`, `encoding`, `auto_vacuum` (none /
   full / incremental), `user_version`, `application_id`, `journal_mode` reduced to `wal` or
   `rollback` (bytes 18-19 are persistent; the rollback flavours are per connection).

Plus, outside the hash: `integrity_check` (lines joined with " / ") and `foreign_key_check` (one
sorted line per row: `c rowid=2 parent=p fkid=0`).

API:
- `WalkLogical(ctx, q RowsQuerier, LogicalVisitor)` walks exactly that content and calls
  `Schema(SchemaRow)`, `Table(TableInfo)`, `Row(TableInfo, []any)`, `TableDone(TableInfo, rows)`,
  `Pragma(name, value)` (all optional). `RowsQuerier` is satisfied by `*sql.DB`, `*sql.Conn`,
  `*sql.Tx`. 3.3 reuses it to write the dump and to read rows for the rebuilds, so the capture
  and the hash read the same content the same way. `TableInfo{Name, Kind, WithoutRowid,
  RowidColumn, Columns}`; `TableSelect` returns the SELECT used; `QuoteIdentifier` doubles `"`.
- `LogicalHash(ctx, *sql.Conn) (Logical, error)`: runs everything inside one `BEGIN … COMMIT`
  (a read transaction, so a concurrent writer cannot split the snapshot). `Logical{SHA256,
  SchemaSHA256, SchemaRows, Tables []{Name, Rows, SHA256}, Pragmas, Integrity,
  ForeignKeyCheck}`; `Summary()` prints it on one line; `TableByName`, `Pragma`.
- `LogicalHashFile(ctx, path)`: refuses a missing file (`ErrNoDatabase`, and does not create
  it), opens with `OpenExisting` = plain driver, `mode=rw` (not `ro`: a read-only open of a WAL
  database leaves `-wal`/`-shm` behind, ch02 platform note), one connection, no settings changed;
  hashes, closes. Test: the file's sha256 is unchanged and no side file is left.
- `CompareLogical(a, b) LogicalResult{Level E3Logical, Equal, SHA256A, SHA256B, Differences}`:
  equal only if the streams are equal AND both integrity checks are "ok" AND the two
  foreign_key_check lists are equal. `Differences` names what differs: `schema differs (4 vs 5
  rows)`, `table t differs (2 vs 2 rows)`, `table x only in the first`, `pragma user_version 1
  vs 2`, `integrity_check of the second: …`, `foreign_key_check changed (0 vs 1 rows)`.
  `String()`: `E3 logical: equal (logical sha256 …; integrity_check ok; foreign_key_check
  unchanged)` or `E3 logical: differ: <differences joined by "; ">`.
- `CompareLogicalFiles(ctx, a, b)`.

### dbhash runner (dbhash.go)

- `LocateDBHash()` = `sqlitex.LocateTool("dbhash")` ($COURSE_TOOLS_DIR/bin, `_tools/bin`, PATH),
  wrapped in `ErrNoDBHash`. `RunDBHash(ctx, tool, db)` returns the 40-hex SHA1 it prints. dbhash
  exits 0 even when it cannot open a file (it prints to stderr and continues, dbhash.c:440), so
  any stderr output is an error. dbhash opens databases read-write (dbhash.c:434-438, "so hot
  journals can recover"), so on a WAL database at rest it leaves no side files; 3.3 checks the
  file is still at rest after every run.

### Tests (`go test ./internal/equal`, about 7-9 s)

- E2: ignores exactly 24-27, 40-43, 92-99 (flips at 24, 27, 40, 43, 92, 99 → equal; flips at 18,
  19, 23, 28, 39, 44, 91, 100, and bytes 24/95 of later pages → differ, page named); size
  difference; page size 0 and empty range refused. A real `sqlite3_backup` copy is E1-different
  and E2-equal, all differing bytes inside the declared list.
- `ChangedPages`: added, removed, role change, identical manifests.
- E3 blind spots, each with our hash and (when dbhash is found) dbhash: implicit rowid (dbhash
  blind), sqlite_sequence (blind), user_version (blind), integer 1 vs real 1.0 (dbhash sees it),
  value boundary (blind), row moved between tables (blind). All six: ours differ. dbhash was found
  in every run here; without it the dbhash half is skipped with a log line.
- E3 ignores history: 300 rows inserted forward in one transaction, backward one per
  transaction, and with extra rows inserted then deleted: E1 differ, E3 equal.
- WITHOUT ROWID table with a quoted odd name, a table whose columns are named `rowid` and
  `_rowid_` (alias falls back to `oid`), and the refusal when all three are taken.
- Schema difference (an extra index), foreign_key_check difference (an orphan row inserted with
  foreign keys off; two orphan databases are equal to each other), integrity failure, the six
  pragmas present with the expected values (`journal_mode` wal, `application_id` 1397836120).
- Missing file refused and not created; hashing leaves the file and its side files unchanged.
- Storage classes vs `typeof()`; nine encodings (NULL, int 1, real 1, text "1", blob "1", -0.0,
  +0.0, empty text, empty blob) all distinct.
- Bug I hit while writing the tests (for Neon's review): calling `db.QueryRowContext` from inside
  a `WalkLogical` row callback on a `SetMaxOpenConns(1)` pool deadlocks (the walk holds the only
  connection). The test collects rows first. Callers must not query the same single-connection
  pool from inside the callbacks.

## What each span does (facts read from the copied source)

- **btreeInt.h 149-228 (`Cell`).** Cell content is stored at the end of the page and grows toward
  the start; the cell-pointer array follows the page header and is sorted by key. Unused space
  inside the content area is a linked list of freeblocks (each ≥ 4 bytes: 2-byte next offset,
  2-byte size); free groups of 3 bytes or fewer are "fragments", counted in header byte 7. Cell
  layout: 4-byte left child (interior only), payload-size varint, key varint (the rowid on table
  pages), payload, 4-byte first overflow page if it overflows. Overflow pages: 4-byte next pointer
  then data; freelist trunk: next trunk, leaf count, leaf page numbers. Lines 211-228 define
  `MX_CELL_SIZE` (page size − 8) and `MX_CELL` ((page size − 8)/6, assuming 6-byte minimum cells).
- **`sqlite3BtreeInsert`, btree.c 9409-9710.** Saves other cursors on the same tree (9439). If the
  key exists and the new cell has the same size it overwrites in place (`btreeOverwriteCell`,
  9505/9557) — no rebalancing, one page. Otherwise it seeks (`sqlite3BtreeTableMoveto`, 9513; the
  `BTREE_APPEND` hint lets an append skip the seek), builds the cell with `fillInCell` (9601,
  which allocates overflow pages for a large payload), makes the leaf writable
  (`sqlite3PagerWrite`, 9614), drops the old cell if replacing (9647), and inserts with
  `insertCellFast` (9656). If the cell did not fit, it is held as an "overflow cell" and
  `balance()` is called (9683) — only then. An insert that fits touches only its leaf.
- **`balance`, btree.c 9130-9259.** A loop from the current page up. Stops when the page has no
  overflow cell and its free space is at most 2/3 of the usable size (9143). Root page with an
  overflow → `balance_deeper` (9158), then the loop continues on the new child. Non-root page
  whose overflow cell is the last cell of a table leaf, whose parent is not page 1 and which is
  its parent's rightmost child → `balance_quick` (conditions 9185-9189, call 9206), the append case. Otherwise →
  `balance_nonroot` (9228), which may leave the parent over- or underfull, so the loop moves up
  one level and checks again.
- **`balance_quick`, btree.c 8007-8101.** Allocates one new page (8025), puts only the new cell
  on it (`rebuildPage`, 8045), builds a divider cell for the parent from the old page's number and
  its largest rowid (8081-8085), inserts it at the end of the parent (8089) and sets the parent's
  right-child pointer to the new page (8094). The full old page is not modified. Cost: new page +
  parent + page 1 (the database size in the header). Measured in 3.1: 3 pages, 3 WAL frames.
- **`balance_deeper`, btree.c 9049-9094.** Makes the root writable, allocates a child (9064),
  copies the root's content and its overflow cells into it (`copyNodeContent`, 9065; overflow cells 9082-9086),
  zeroes the root as an interior page of the same kind (9089) and points the root's right child
  at the new page (9090). The root keeps its page number; the tree is one level deeper and the
  child is balanced on the next loop iteration.
- **balance_nonroot header comment, btree.c 8204-8244.** Redistributes the cells of a page and up
  to two siblings (one on each side, or both on one side at the edge) so that all have about the
  same free space; may add or remove one or two siblings; inserts at most four dividers into the
  parent, which may then overflow or underflow — the caller (`balance`) fixes the parent next.
- **`balance_nonroot`, btree.c 8245-9027.** The general case (not read line by line in the
  chapter). It loads the siblings, removes their dividers from the parent, computes a new
  distribution, reuses or allocates pages, rewrites each page (`editPage`/`rebuildPage`) and
  inserts new dividers. In 3.1 part B it rewrote three leaves (one of them with identical bytes)
  and added one new page.
- **`sqlite3BtreeDelete`, btree.c 9841-10041.** Frees the cell's overflow chain
  (`BTREE_CLEAR_CELL`, 9947) and removes the cell (`dropCell`, 9948). If the cell was on an
  interior page, the largest cell of the left subtree's leaf replaces it (9979-9981). Then
  `balance` runs only if the leaf now has more than 2/3 of its usable space free (test 10002, calls 10007 and 10016):
  underfull leaves are merged into siblings by `balance_nonroot`, and emptied pages are freed.
- **`freePage2`, btree.c 6836-6969.** Increments the freelist count in page 1 (6860-6862). With
  `secure_delete` it zeroes the whole page (6864-6873). If the first trunk has room
  (fewer than usable/4 − 8 leaves — the "−8" kept for pre-3.6.0 readers, 6911-6928), the page
  becomes a leaf of that trunk; **without secure_delete it calls `sqlite3PagerDontWrite`
  (6934-6935), so a freed leaf page is not written at all and its old bytes stay in the file**.
  Otherwise the page becomes the new first trunk (6950-6956).
- **`allocateBtreePage`, btree.c 6514-6822.** If the freelist count (page 1 offset 36) is
  non-zero, takes a page from the freelist: decrements the count (6572), walks trunks, and either
  takes a leaf from the trunk (the one closest to `nearby` if given, 6690-6720) or the trunk
  itself when it has no leaves. Only when the freelist is empty does it grow the file
  (`pBt->nPage++`, 6778, skipping the lock-byte page; in-header size written at 6800). 3.2 shows the file grows only in
  transactions that start with an empty freelist.
- **`checkTreePage`, btree.c 10855-11118.** The invariants as checks, reached from
  `PRAGMA integrity_check`: the page parses (`btreeInitPage`, 10908) and its free space adds up
  (`btreeComputeFreeSpace`, 10914, "free space corruption"); every cell pointer is inside the
  content area ("Offset %u out of range", 10970) and every cell inside the page ("Extends off end
  of page"); keys are in order and within the bounds the parent gives ("Rowid %lld out of
  order", 10986); every child has the same depth ("Child page depth differs", 11018); overflow
  chains are walked (`checkList`); and a min-heap of cell and freeblock extents shows that no byte
  is used twice ("Multiple uses for byte", 11090) and that the uncovered bytes equal header byte 7
  ("Fragmentation of %u bytes reported as %u", 11105).
- **`insertCellFast`, btree.c 7428-7500.** A copy of `insertCell` used only by
  `sqlite3BtreeInsert` (comment 7418-7427: "so that this variant is only called from that one
  place, and is thus inlined"). If the cell plus its 2-byte pointer does not fit in the page's free
  space, it is stored as an overflow cell (`pPage->apOvfl`) for `balance` to place; otherwise
  `allocateSpace` (7472) finds room (freeblock first-fit, else the gap, defragmenting if needed).
- **`fillInCell`, btree.c 7074-7257.** Writes the payload-size and key varints and the payload
  into a cell buffer; when the payload exceeds the local limit, it allocates overflow pages one at
  a time (`allocateBtreePage`, 7212) and chains them.
- **`dropCell`, btree.c 7267-7309.** Removes cell `idx`: returns its bytes to the page with
  `freeSpace` (7292), decrements the cell count and shifts the cell-pointer array down.
- **`freeSpace`, btree.c 1918-2014.** Adds a byte range to the freeblock list, merging with
  neighbours, or extends the content area if the range starts it. With `secure_delete` (the
  `BTS_FAST_SECURE` flags) it first zeroes the freed bytes (1992-1996). That is why 3.2 finds
  deleted rows' markers in live table leaves (freeblocks) only with secure_delete off.
- **`clearCellOverflow` and `BTREE_CLEAR_CELL`, btree.c 6979-7045 and 7053-7059.** The macro
  parses the cell and, only when its local size differs from its payload size (it overflows),
  calls `clearCellOverflow`, which walks the overflow chain and frees each page with `freePage2`.
  (Older texts call this `clearCell()`.)
- **`sqlite3PagerDontWrite`, pager.c 6329-6339.** Marks a dirty page `PGHDR_DONT_WRITE` so the
  commit does not write it (the comment above says this can quadruple large DELETEs). Not used
  for temp files or inside savepoints. This is the mechanism behind 3.2's "5 frames vs 54 frames".
- **`sqlite3_backup_step`, backup.c 323-591.** Copies pages from source to destination; at the
  end it sets the destination's schema cookie to its old value + 1 (448), so other connections to
  the destination reload the schema. Source of the byte-43 difference in 3.3.
- **`sqlite3RunVacuum`, vacuum.c 143-427.** For VACUUM INTO: refuses an existing non-empty output
  file ("output file already exists"), attaches it, sets the same page size and auto-vacuum,
  creates the tables in `sqlite_schema` order (except `sqlite_sequence`), then the indexes, copies
  every table with `INSERT INTO … SELECT*FROM`, copies views/triggers/virtual tables as schema
  rows, copies meta values (schema cookie +1, default cache size, text encoding, user_version,
  application_id) and commits. The output pager uses the source's synchronous level. It does not
  copy the journal mode: the output is a rollback-mode file (3.3: bytes 18-19 = 1,1).
- **`hash_one_query`, tool/dbhash.c 281-360.** For each row and column: a tag character `0`-`4`
  (NULL, INTEGER, FLOAT, TEXT, BLOB), then 8 big-endian bytes for numbers or the raw bytes for
  text/blob — no length, no row or table boundary.
- **`main`, tool/dbhash.c 379-491.** Opens each file read-write; hashes every table whose name
  does not start with `sqlite_` (so `sqlite_sequence` and `sqlite_stat*` are skipped) with
  `SELECT * FROM t` and no ORDER BY (the comment relies on scan order being key order; the
  rowid is not selected, so implicit rowids are not hashed); then the schema
  `type, name, tbl_name, sql ORDER BY name COLLATE nocase`; prints a SHA1 per file. No pragma is
  hashed (user_version blind spot).

## Experiment 3.1 pages-per-insert

Claim: most inserts change one page; an insert that fills a leaf changes several; when the root
overflows the tree gains a level while the root keeps its page number; the changed pages are the
WAL frames shipped. Placed at steps 3 and 5.

Run: `go run ./ch03-btrees-and-page-management/experiments/3.1-pages-per-insert` (≈ 31 s here,
150 MB RSS). Evidence `evidence/3.1-pages-per-insert.txt` (115 lines). Exit 0, last line
`PAGES PER INSERT OK`. Portable (no CLI, no strace).

Method: one connection, WAL, `synchronous=OFF`, `wal_autocheckpoint=0`. After each one-row
autocommit INSERT: (1) read the `-wal` file's frame headers → the page numbers this commit
shipped (`readWALPages`: 32-byte WAL header, then frames of 24 + 4096 bytes, page number in the
first 4 bytes of each frame header; WAL magic checked); (2) `PRAGMA wal_checkpoint(TRUNCATE)`, so
the database file holds the state and the WAL is empty for the next commit; (3) read every page
through `sqlite_dbpage` into an image, parse it with `internal/dbfile`, run the census (must be
complete), build the manifest (page, role, sha256), compare with the previous manifest
(`equal.ChangedPages`); (4) `PRAGMA integrity_check`; (5) tree depth from the census parent links
and the root page from `sqlite_schema`.
- Why `sqlite_dbpage` and not the file: reading the database file with `os.Open` in a process
  that has a connection to it would drop that process's POSIX locks when the descriptor closes
  (outline 4.1). The `-wal` file is read directly: SQLite holds no POSIX lock on the `-wal` (WAL
  locks live in the `-shm`), so closing a descriptor on it drops nothing.
- Why `synchronous=OFF`: about 6000 fewer fsyncs; it changes no byte of any file (ch01 showed the
  recipe's file is the same under FULL and NORMAL; 3.3 shows the same sha256 for its builds under
  FULL and OFF — both were run while building).
- Speed: pages whose bytes equal the previous image reuse the previous sha256 (no SHA-NI on this
  CPU; hashing every page every time took 54 s and 4 GB before this and the streaming change).
  A check at the end confirms the last manifest equals `capture.BuildManifest` of the same image.

How to read the output:
- Line 1 pin; `INFO: work dir`; `settings:` line.
- **Part A, arithmetic (printed before any insert).** Row = `(id INTEGER PRIMARY KEY, v BLOB of
  1000 bytes)`, rowids 2, 4, 6, … (even, leaving gaps; not needed in A). Record header 4 bytes
  (1 header-size varint + 1 serial type 0 for the NULL that stands for the rowid alias + 2 for
  serial type 2012 = 12 + 2×1000); payload 1004 ≤ max local 4061, no overflow. Cell = 2
  (payload-size varint) + 1 or 2 (rowid varint) + 1004 = 1007 or 1008, +2 pointer = 1009/1010.
  Leaf space 4096 − 8 = 4088 → 4 rows per leaf (4 × 1010 = 4040 fits, 5 × 1009 = 5045 does not).
  Prediction: first split at insert #5 (rowid 10), then every 4 inserts (#4m + 1). Root as
  interior: 4096 − 12 = 4084 bytes; dividers are 4-byte child + varint key + 2-byte pointer = 7
  bytes for keys ≤ 127 (15 of them: keys 8..120) and 8 bytes after (497) = 4081 bytes; the 513th
  divider does not fit → third level at insert #2053 (rowid 4106).
- `table t: root page 2 …`, `2061 inserts done; final page count 520, depth 3`, the manifest
  check.
- Histogram: 1 page: 1546 inserts; 3 pages: 513; 4 pages: 1; 5 pages: 1. Splits: 515, at #5, #9,
  #13, …, #2061. Checks: first split at #5 as predicted; every split at #4m + 1; every non-split
  insert changes exactly one page and it is a table leaf (page 1 is not written in WAL mode);
  most inserts change one page (1546 of 2061).
- **First split, depth 1 → 2 (insert #5)**: pages `[1 2 3 4]`, WAL frames `[1 2 3 4]`. Layout:
  page 1 in-header size 2 → 4; page 2 table-leaf with keys 2,4,6,8 (content-start 68, unused 52) →
  table-interior with 1 cell (key 8), children [3 4], unused 4077; page 3 new, a copy of the old
  root (keys 2..8, unused 52); page 4 new, one cell (key 10), unused 3079.
- **First append split at depth 2 (insert #9)**: pages `[1 2 5]`: size 4 → 5; root gets divider
  16 (children [3 4 5]); page 5 new with key 18. The full leaf 4 is not in the list.
- Next append split (#13) `[1 2 6]`; an ordinary insert (#14) `[6]`: page 6 1 cell → 2 cells,
  content-start 3089 → 2082 (one 1007-byte cell further down), unused 3079 → 2070.
- **Depth 2 → 3 (insert #2053)**: pages `[1 2 516 517 518]`, same WAL frames. Root before: 512
  cells, keys 8..4096, unused 3 bytes (4084 − 4081, exactly the arithmetic). Root after: 1 cell
  (key 2064), children [517 518]. Page 516: the new leaf (key 4106). Page 517: interior, 257
  cells, keys 8..2056, children 3..260. Page 518: interior, 255 cells, keys 2072..4104, children
  261..516. 257 + 255 + 1 promoted = 513 dividers. So: `balance_quick` made leaf 516 and pushed
  divider 513 into the root; the root overflowed; `balance_deeper` copied it to a new page and
  `balance_nonroot` split that into two half-full interior pages. None of the 513 leaves changed.
- Checks: depth 3 at #2053 as predicted; the first split is the first depth change (4 pages);
  an append split at depth 2 changes three pages; root stays page 2 and equals
  `sqlite_schema.rootpage` throughout; integrity ok after all 2061 inserts; WAL frames = changed
  pages for every insert.
- `WAL frames for 2061 inserts: 3094 (12747280 bytes …); the table itself is 520 pages` — the
  WAL shipped about 6 times the table's size.
- **Part B (middle split)**: 20 appends (rowids 2..40) → 5 full leaves 3-7 under root 2 (layout
  printed). Insert rowid 11 into leaf 4 (keys 10..16, full): pages changed `[1 2 4 5 8]`, WAL
  frames `[1 2 3 4 5 8]`. After: leaf 4 keys 10,11,12,14; leaf 5 keys 16,18,20 (unused 1061);
  new leaf 8 keys 22,24 (unused 2070); root gets 5 dividers 8,14,20,24,32, children
  [3 4 5 8 6 7]. Page 3 was written with identical bytes (correction 4). Leaves in key order
  after: `3:4 4:4 5:3 8:2 6:4 7:4` — a middle insert leaves partly empty pages, an append does
  not.
- **Part C (two indexes)**: table `t(id, email, score, v)` with `t_email` and `t_score`; 200
  inserts with emails and scores in scrambled order: 3 pages: 188 inserts, 5: 10, 6: 2. Insert
  #50 changed `3(index-leaf) 4(index-leaf) 7(table-leaf)`. WAL frames = changed pages and
  integrity ok throughout.
- **Part D (rollback mode)**: same appends with `journal_mode=DELETE`: every commit includes page
  1 (change counter): 2 pages: 10 inserts, 3: 1 (append split), 4: 1 (first split).

Outline claims: all confirmed (corrections 4 and 5 refine the wording).

## Experiment 3.2 delete-and-reuse

Claim: deleting rows moves emptied pages to the freelist, later inserts reuse them before the
file grows, the file never shrinks on its own, a freed page keeps its old bytes unless
`secure_delete` is on. Placed at step 5.

Run: `go run ./ch03-btrees-and-page-management/experiments/3.2-delete-and-reuse` (≈ 3.5 s, 150 MB
RSS). Evidence `evidence/3.2-delete-and-reuse.txt` (75 lines). Exit 0, last line `DELETE AND
REUSE OK`. Portable.

Method: the same sequence twice, `secure_delete=OFF` then `ON`, each in its own directory. WAL,
`synchronous=FULL` (default). Every step is its own connection session; the file is read with
`os.ReadFile` + `dbfile.FromBytes` only after the connection closed and the file is at rest
(`recipe.CheckAtRest`). `integrity_check` = ok is required at the end of every session.
Each 1000-byte blob repeats a marker `SECRET-%06d` (initial rows), `NEW-…`, `GROW-…`, so the
search can tell which deleted row's bytes survive.

How to read the output (OFF block; the ON block has the same numbers except the marker search):
1. 400 rows: 417792 bytes = 102 pages (page 1, root, 100 leaves of 4 rows).
2. `DELETE … BETWEEN 99 AND 302` (204 rows on leaves 27-78; leaves 27 and 78 keep 2 rows each):
   size unchanged; freelist 50 pages: trunk 29 with 49 leaves (pages 29-78; all 50 were leaves of
   the deleted rows). `the DELETE's commit wrote 5 WAL frames` (ON: 54). Marker search: 199 of
   204 deleted rows readable, on 52 pages: 49 freelist leaves, 1 freelist trunk (its unused
   tail), 2 table leaves (the freeblocks in leaves 27 and 78). ON: 0.
3. 100 new rows (ids 1001-1100): size unchanged, freelist 25 (trunk 29, 24 leaves); new rows on
   pages 30 and 55-78, all 25 former freelist pages. Deleted rows still readable: 104 (ON: 0).
4. 30 transactions of 4 rows: batch 1 freelist 25 → 24 with page_count unchanged … batch 25
   freelist 1 → 0, page_count 102; batches 26-30 grow the file one page each (102 → 107).
   Check: page_count grew only in transactions that started with an empty freelist.
5. `DELETE FROM t WHERE id >= 1001` (220 rows): 107 pages, freelist 55 (trunk 102), size
   unchanged.
6. `VACUUM`: 208896 bytes = 51 pages (196 rows = 49 leaves + root + page 1), freelist 0, no
   deleted marker left. Check: pages ≤ pages − freelist before.
- Comparison: file sizes identical for OFF and ON at every step `[417792 417792 417792 438272
  438272 208896]`; deleted rows readable after the delete OFF 199 / ON 0, after the reuse OFF 104
  / ON 0; WAL frames of the delete OFF 5 / ON 54.

Why 5 vs 54: without secure_delete, a page freed as a freelist leaf is marked "don't write"
(`freePage2` → `sqlite3PagerDontWrite`), so the commit writes only the trunk, page 1, the root and
the two partial leaves; with it, every freed page is zeroed, so it is written. The same rule
explains the stale bytes: an unwritten page keeps what was on disk.

Outline claims: all confirmed. Added: the WAL-frame cost of secure_delete; where the stale bytes
are by page role; that the trunk page and partially emptied leaves also hold stale bytes.

## Experiment 3.3 same-rows-different-bytes (rung R2)

Claim (corrected): layout records history. The same final rows built with a different history or
insert order give different files; all have one logical hash; their ordered logical capture is
byte-identical; VACUUM INTO gives one sha256 for all of them and for the replayed capture — with
the limits of correction 2. Placed at step 6. Completes `internal/equal` (E2, E3).

Run: `go run ./ch03-btrees-and-page-management/experiments/3.3-same-rows-different-bytes` (≈ 6 s,
150 MB RSS). Evidence `evidence/3.3-same-rows-different-bytes.txt` (98 lines). Exit 0, last line
`SAME ROWS OK`. Portable; dbhash optional.

Capture / rebuild / canonical form: capture = an ordered SQL dump written in Go from
`equal.WalkLogical` (`capture/r0.sql`: file pragmas, `PRAGMA journal_mode=WAL`, BEGIN, CREATE TABLE
in name order, every row as `INSERT INTO "t"(rowid, "c1", …) VALUES(…)` in table-name and rowid
order with typed literals — integers in decimal, reals with the shortest round-trip form plus
`.0` when needed, text quoted with `''`, blobs `X'…'` — then `sqlite_sequence` rows if any, then
indexes, views and triggers, `user_version`, `application_id`, COMMIT); rebuild = executing that
script on a fresh file with the plain driver; canonical form = `VACUUM INTO`.
Limits of this capture, deliberately: tables named `sqlite_*` other than `sqlite_sequence`
(e.g. `sqlite_stat1`) are refused with an error; NaN cannot occur (SQLite stores NaN as NULL);
`-0.0` would be written as `-0.0` and SQLite may store it as 0 — not exercised.

How to read the output:
- Pin; `INFO:` work dir and dbhash path; `dbhash: found …` (without dbhash: one `NO-DBHASH:` line
  and the dbhash column prints `-`; the dbhash checks are skipped).
- **Part A.** R0 content: 4 schema statements, accounts 183, documents 96, events 1177 rows,
  user_version 30. `capture: capture/r0.sql, 2005315 bytes, 1468 lines, sha256 92d38985…` and its
  first 9 and last 4 lines. Builds (sizes): r0 1105920 (31 transactions with updates and
  deletes); one-transaction 1089536; per-statement 1089536; shuffled 1093632 (PCG seed 3, one
  transaction per row); replayed 1089536; backup 1105920. All builds use the recipe's options
  (WAL, secure_delete ON) with synchronous OFF (the sha256 values were the same with FULL).
- Table 1 (against r0): sha256 prefix; E1 (`differ (268 pages differ, sizes differ)` …); E2 the
  same except the backup (`equal except header bytes [27 43 95]`); E3 `equal` for all.
- Table 2: logical sha256 `1a656547…` for all six; dump sha256 `92d38985…` for all six; dbhash
  `9bee1689…` for all six; VACUUM INTO sha256 `927e39a4…` for five, `9324d944…` for the backup.
- Then: the R0 logical summary (pragmas: page_size 4096, UTF-8, none, 30, 1397836120, wal;
  integrity ok; 0 fk rows); 4 distinct file sha256; checks (correction 1); same logical hash;
  E3-equal; the capture is identical for every build ("the capture depends on content, not
  history"); one canonical sha256 for the five; the backup's canonical form differs at byte 43
  (cookies 5 and 2); dbhash agrees on equality; canonical form header: 1089536 bytes, bytes 18-19
  = 1,1, change counter 1, schema cookie 5; VACUUM INTO keeps rows, schema, user_version and
  application_id but its logical hash differs only in `journal_mode` (rollback).
- **Part A2.** Same two batchings in rollback mode: E1 differ (1 of 266 pages), E2 equal
  (`[26 27 94 95]`), change counters 3 and 1463.
- **Part B.** Six pairs, table `pair / ours / dbhash / our first difference`, one check line for
  dbhash's verdict and one for ours. sqldiff is not built by build-tools; its 1 vs 1.0 blind spot
  is cited from the research report, not re-run.
- **Part C.** The three canonical-form limits of correction 2, with dbhash of the sources (equal
  in all three).

Outline claims: E3 equal and one logical hash — CONFIRMED; one sha256 after canonicalization
shared by the replayed copy — CONFIRMED for the builds (correction 2 for backup copies and other
schema histories); "three different sha256 values before" — FALSE for the batching way
(correction 1); dbhash agrees on equality — CONFIRMED, with two new dbhash blind spots
(correction 3).

## Timeline data (chapter section 4: page layout before and after a split)

From 3.1 (page size 4096, 1007/1008-byte cells, 4 per leaf):

| Insert | Pages written (= WAL frames) | Page 2 (root) | Other pages |
| --- | --- | --- | --- |
| #1-#4 | [2] each | leaf, 1→4 cells (keys 2..8), unused 3079 → 52 | — |
| #5 (rowid 10) first split, depth 1→2 | [1 2 3 4] | leaf → interior, 1 divider (8), children [3 4] | 3 = copy of old root (4 cells, unused 52); 4 = new leaf (key 10, unused 3079); page 1 size 2→4 |
| #6-#8 | [4] each | — | page 4 fills to 4 cells |
| #9 (rowid 18) append split | [1 2 5] | 2 dividers (8,16), children [3 4 5] | 5 = new leaf (key 18); page 4 untouched; size 4→5 |
| #14 ordinary | [6] | — | page 6: 1→2 cells, content-start 3089→2082 |
| #2053 (rowid 4106) depth 2→3 | [1 2 516 517 518] | 512 dividers, unused 3 → 1 divider (2064), children [517 518] | 516 new leaf; 517 interior 257 cells (keys 8..2056); 518 interior 255 cells (keys 2072..4104); size 515→518 |

Middle split (3.1 B), rowid 11 into leaf 4 = [10,12,14,16]: written [1 2 3 4 5 8]; changed [1 2 4
5 8]; leaves after: 3 [2..8], 4 [10,11,12,14], 5 [16,18,20], 8 [22,24], 6, 7 unchanged; root
dividers 8,14,20,24,32.

Rollback mode (3.1 D): every commit also rewrites page 1 (2 pages per ordinary insert).

## Platform implications (facts)

- Pages per write = WAL frames shipped: 1 frame (4120 bytes) per ordinary append; 3 per append
  split; 4-5 at a depth change; a middle split 6 frames for 5 changed pages. 2061 appends of
  ~1 KB rows shipped 3094 frames = 12.7 MB for a 2.1 MB table (≈ 6×). With two secondary indexes
  the common insert ships 3 frames.
- Shipped ≥ changed: a frame can carry an unchanged page (correction 4). A WAL shipper must not
  assume every frame is a change, and a page-diff replicator (sqlite3_rsync style) can ship less.
- Rollback mode writes page 1 at every commit (2 pages per ordinary insert, plus the journal).
- Files do not shrink on delete; freed pages are reused before the file grows (3.2: 50 freed, 25
  reused, growth only after the freelist is empty); `VACUUM` returns the space (107 → 51 pages).
- `secure_delete=OFF`: 199 of 204 deleted rows are still readable in the file after the delete,
  104 after reuse — on freelist leaves, the trunk, and freeblocks of live leaves. Any page-level
  capture or backup of such a file carries deleted tenant data. `secure_delete=ON`: none, at a
  cost of more WAL frames for deletes (54 vs 5 for the same DELETE) and the same file size.
- Physical vs logical copies: the same rows give different files depending on insert order and
  history (3.3: 4 distinct sha256), but one logical hash, one dump, one dbhash and (with the same
  schema history) one VACUUM INTO sha256. A backup-API copy is E2-equal to its source, not E1.
- A canonical form for byte comparison: VACUUM INTO, but only between databases with the same
  schema creation order and schema cookie; otherwise compare canonical forms at E2 or at E3. The
  canonical file is in rollback mode (bytes 18-19 = 1,1); switching it to WAL rewrites bytes
  18-19 and the counter.
- R2 capture (ordered dump) is itself history-independent: same content → same 2,005,315-byte
  script (sha256 `92d38985…`). It is about 1.8× the database size here because blobs are hex.
- Use our E3 logical hash, not dbhash, to decide "same content": dbhash misses implicit rowids,
  `sqlite_sequence`, `user_version`, value boundaries and rows moved between tables.

## WAL-default evaluation (ruling 10) for this chapter

WAL as the platform default makes sense for chapter 3. Every experiment database is WAL except:
3.1 part D (rollback mode, to show page 1 rewritten at every commit) and 3.3 part A2 (rollback
mode, to show where the batching difference appears). Effects found:
1. WAL makes per-write cost visible and measurable as frames; 3.1 uses the frame list as its
   independent oracle for the changed-page sets (they agree for 2061 + 200 inserts).
2. WAL makes commit boundaries invisible in the file for append-only work (correction 1): good for
   reproducibility.
3. A page manifest of a WAL database needs a checkpoint (or `sqlite_dbpage`, which reads through
   the WAL); 3.1 checkpoints with TRUNCATE before every manifest, as the brief asked.
4. VACUUM INTO always writes a rollback-mode file; a platform that restores from the canonical
   form must switch it to WAL after the comparison, not before.
No place where WAL as default does not make sense.

## Linux-only and the Mac route

Nothing in chapter 3 is Linux-only: no strace, no `/proc`, no CLI. On a Mac run the same
`go run` commands natively (darwin: cross-built and cross-vetted only, never run). 3.3's dbhash
column needs `_tools/bin/dbhash` from `go run ./cmd/course build-tools` (needs a C compiler, Xcode
CLT); without it 3.3 prints `NO-DBHASH: …`, skips the dbhash checks and still exits 0 (its output
then differs from the evidence file in those lines). Or run in the lab container:
`docker compose run --rm lab go run ./ch03-btrees-and-page-management/experiments/<name>` (the
image has dbhash in `/opt/course-tools/bin`).

## Runtimes (this VM, 4 cores, shared)

3.1 ≈ 31 s (it was 54 s before the hash reuse; most of the rest is 2061 censuses, integrity
checks and `sqlite_dbpage` reads), 3.2 ≈ 3.5 s, 3.3 ≈ 6 s; `go run ./cmd/course check evidence
ch03` (six runs) 52 s. All under 160 MB RSS.

## Working scripts

None kept: every probe while building was a run of the experiments themselves. A CPU profile of
3.1 (pprof, temporary edit, reverted) found the sha256 cost that led to the hash reuse.
`agent_work/ch03/INDEX.md` lists this notes file.

## Not done / for later chunks

- No prose, no comments (seat rule).
- sqldiff is not part of `build-tools`; its 1 vs 1.0 blind spot is cited, not re-run.
- The R2 dump handles `sqlite_sequence` but refuses `sqlite_stat*` tables.
- Neon's adversarial starting list: 3.1 random insert orders / other row sizes / two indexes (part
  C already shows the index case); 3.2 stale bytes in a plain copy and a backup, VACUUM (part 6
  already shows VACUUM); 3.3 blind spots (part B already shows all four plus two more).
