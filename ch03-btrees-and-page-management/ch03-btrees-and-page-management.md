# Chapter 3 — B-trees and page management

| Pin field | Value |
| --- | --- |
| SQLite release | `3.53.4` (`2026-07-24 19:02:57 UTC`) |
| Git tag | `version-3.53.4` |
| Git commit (`github.com/sqlite/sqlite`) | `b09c88c14082339b66c7b7158d609a771e64ca69` |
| Fossil check-in (`sqlite_source_id()`) | `bf7c7f30031888f4e796e429ab3978879485813aaca6f641c7b33e4e09459bcc` |
| Go driver | `modernc.org/sqlite v1.60.1` (`modernc.org/libc v1.77.1`) |
| Copied C sources in this directory (`SOURCES.sha256`) | `btree.c`, `btreeInt.h` |
| Linked C sources (in the pinned tree, not copied) | `pager.c`, `backup.c`, `vacuum.c`, `tool/dbhash.c` |

> **How to run the experiments**
>
> - **Prerequisites:** Go 1.26 (`go.mod` specifies `go 1.26.0`).
> - **Fetch the pinned SQLite source tree** into `_src/sqlite-3.53.4` (or point `SQLITE_SRC` at an existing checkout of tag `version-3.53.4`):
>   ```text
>   go run ./cmd/course fetch-source
>   ```
> - **Build the pinned C tools** (`sqlite3`, `showwal`, `dbhash`) into `_tools/bin/` for Experiment 3.3's `dbhash` cross-check column (no `sqlite3` CLI is needed by any Chapter 3 experiment; without `_tools/bin/dbhash`, Experiment 3.3 prints a `NO-DBHASH:` line, skips the `dbhash` checks, and still exits 0):
>   ```text
>   go run ./cmd/course build-tools
>   ```
> - **Run the experiments** from the repository root:
>   ```text
>   go run ./ch03-btrees-and-page-management/experiments/3.1-pages-per-insert
>   go run ./ch03-btrees-and-page-management/experiments/3.2-delete-and-reuse
>   go run ./ch03-btrees-and-page-management/experiments/3.3-same-rows-different-bytes
>   ```
>   Scratch databases are written under `_work/3.1/`, `_work/3.2/`, and `_work/3.3/`, which are git-ignored and safe to delete.
> - **Cross-platform notes and macOS:** Nothing in Chapter 3 is Linux-only (no `strace`, no `/proc`, no CLI). On macOS, run the same `go run` commands directly (building `_tools/bin/dbhash` requires a C compiler such as Xcode Command Line Tools), or run inside the repository's Linux `lab` container, which includes `dbhash` in `/opt/course-tools/bin`:
>   ```text
>   docker compose run --rm lab go run ./ch03-btrees-and-page-management/experiments/3.1-pages-per-insert
>   docker compose run --rm lab go run ./ch03-btrees-and-page-management/experiments/3.2-delete-and-reuse
>   docker compose run --rm lab go run ./ch03-btrees-and-page-management/experiments/3.3-same-rows-different-bytes
>   ```
> - **Measured platform:** All captured evidence in this chapter was measured on Linux (`linux/amd64`, Go 1.26.8, ext4). All three experiments were also run inside the Debian `lab` container during the build round with identical non-`INFO:` output (the repair round re-ran on the Linux host). macOS (`darwin/arm64` and `darwin/amd64`) was cross-built and vetted only, not run.

---

## 1. Model

Every SQLite table and index is stored as a **B-tree**: a balanced search tree whose nodes are fixed-size **pages** (4,096 bytes by default). A B-tree starts at a single **root page** whose page number is recorded in `sqlite_schema.rootpage` and never moves even as the tree grows deeper. When a tree has height 1, the root page itself is a **leaf page** holding rows or index keys. When the root fills up, SQLite copies its contents to a new child page, turns the root into an **interior page** holding divider keys and child page pointers, and increases the tree's depth by one.

SQLite uses two kinds of B-tree:

- **Table B-tree (`intKey`, B+-tree).** Used by every ordinary (rowid) table. Keys are 64-bit signed integers called **rowids**. Row payloads live **only on leaf pages** (`0x0d`). Interior pages (`0x05`) store no row data—only 4-byte child page numbers and integer **divider keys** (each giving the largest rowid in its left child), plus a 4-byte **rightmost child pointer** in the page header. Because interior cells are tiny (7 or 8 bytes for small keys), a single 4,096-byte interior page holds hundreds of children.
- **Index B-tree.** Used by every secondary index and by `WITHOUT ROWID` tables. Keys are arbitrary binary records, and both **index-leaf** (`0x0a`) and **index-interior** (`0x02`) pages store full key payloads. Index B-trees do not use SQLite's append-only leaf-splitting shortcut (`balance_quick`), so every split redistributes cells across siblings.

Inside a single B-tree page, two regions grow toward each other from opposite ends:

```text
Single B-tree page (4,096 bytes; page 1 starts at offset 100 after the file header)
+-------------------------------------------------------------------------+
| B-tree page header (8 B on a leaf, 12 B on an interior page)            |
|   [0]    page type (0x0d table-leaf, 0x05 table-interior, 0x0a/0x02)    |
|   [1..2] first freeblock offset (0 if none)                             |
|   [3..4] cell count (nCell)                                             |
|   [5..6] start of cell content area (content-start; 0 means 65,536)     |
|   [7]    fragmented free bytes (sum of 1..3-byte gaps)                  |
|   [8..11] rightmost child page number (interior pages only)             |
+-------------------------------------------------------------------------+
| Cell pointer array: nCell x 2-byte offsets, sorted by key               |
|   grows DOWNWARD  |                                                     |
|                   v                                                     |
|                  ... unallocated space (contiguous gap) ...             |
|                   ^                                                     |
|   grows UPWARD    |                                                     |
| Cell content area: variable-length cells + freeblocks (>= 4 B each)     |
|   (cells need not be in key order; cell pointers keep them sorted)      |
+-------------------------------------------------------------------------+

Tree growth and page recycling
  Root page (fixed page number in sqlite_schema.rootpage)
    |
    +--> interior pages (dividers + child page numbers)
           |
           +--> leaf pages (cells) ---> overflow page chain (if payload > max local)

  Freelist (header offset 32 = first trunk, offset 36 = total free pages):
    page 1[32] -> freelist-trunk -> next trunk -> ...
                    |-- array of 4-byte freelist-leaf page numbers
                    |-- allocateBtreePage pops leaves first, then the trunk
                    +-- file grows only when freelist_count == 0
```

Key terms used in this chapter:

- **Cell and cell pointer.** A **cell** is the serialized on-disk entry for one row (on a table leaf), one divider (on a table interior page), or one key (on an index page). Each cell is addressed by a 2-byte big-endian offset in the **cell pointer array**, which is always kept sorted by key so binary search works without moving cell bytes.
- **Freeblock and fragment.** Deleting a cell returns its bytes inside the cell content area to a sorted linked list of **freeblocks** (each at least 4 bytes: a 2-byte next-freeblock offset and a 2-byte size). Leftover gaps of 1 to 3 bytes are too small for a freeblock header; they are called **fragments** and summed in page header byte 7. If a new cell fits in total free space but not in any single freeblock or the middle gap, SQLite compacts (**defragments**) the page.
- **Overflow cell and overflow page.** These are two different things. An **overflow cell** (`MemPage.apOvfl`) is an in-memory slot holding a cell that did not fit onto a full B-tree page during an insert, triggering `balance()` to split or redistribute the page. An **overflow page** is an on-disk page holding the tail of a single row whose payload exceeds the page's local limit (`4,061` bytes on a 4,096-byte table leaf), linked in a chain with a 4-byte next-page pointer at the start of each page.
- **Freelist trunk and freelist leaf.** When rows are deleted and a page becomes empty (under `auto_vacuum=NONE`, SQLite's default), the file does not shrink. The emptied page joins the **freelist**: a linked list of **freelist-trunk** pages (starting at file header offset 32) where each trunk holds an array of 4-byte page numbers pointing to **freelist-leaf** pages. Subsequent allocations pop pages from the freelist before extending the file.
- **WAL frame and checkpoint.** In Write-Ahead Logging (**WAL**) mode, committing a transaction appends a 24-byte frame header plus a full 4,096-byte page image—one **WAL frame** (`4,120` bytes)—to the `-wal` file for every page SQLite marked dirty. A **checkpoint** (`PRAGMA wal_checkpoint(...)`) copies committed frames back into the main `.db` file; `PASSIVE` copies without blocking writers or resetting `-wal`, while `TRUNCATE` resets `-wal` to zero bytes once no reader needs it.
- **`fsync` and `PRAGMA synchronous`.** `fsync` flushes modified file data and metadata from the OS buffer cache to durable storage. `PRAGMA synchronous=OFF` skips `fsync` calls without changing any bytes written to the database file or the WAL frame sequence.
- **`PRAGMA secure_delete`.** Per-connection setting (default `OFF`). When `OFF`, SQLite skips zeroing deleted cells' freeblocks on live pages and marks freed freelist-leaf pages "don't write", leaving old row bytes intact on disk. When `ON`, SQLite zeroes freed bytes and whole freed pages before writing them. (`FAST` mode zeroes live-page deletions while leaving freelist leaves unwritten.)
- **`PRAGMA auto_vacuum` and `VACUUM` / `VACUUM INTO`.** `auto_vacuum` is a persistent file-header setting chosen before the first table is created: `NONE` (0, default) puts freed pages on the freelist and never shrinks the file; `FULL` (1) maintains **pointer-map** pages and relocates pages to truncate the file at every deleting commit; `INCREMENTAL` (2) keeps freed pages on the freelist until `PRAGMA incremental_vacuum` moves and truncates them. `VACUUM` rebuilds the database into a compact file with zero freelist pages; `VACUUM INTO 'path'` writes that rebuilt copy to a new file.
- **Change counter and schema cookie.** Two 4-byte big-endian fields in the 100-byte database header: the **file change counter** (bytes 24–27, paired with **version-valid-for** at bytes 92–95) increments when page 1 is written on a commit; the **schema cookie** (bytes 40–43) increments on every DDL statement (`CREATE`, `DROP`, `ALTER`) so other connections know to reload `sqlite_schema`.
- **Logical hash and canonical form.** A **logical hash** (equality level **E3**) hashes a database's schema, rows (with explicit rowids and storage classes), and persistent pragmas independently of physical page layout. A **canonical form** is a deterministic physical rewrite—here `VACUUM INTO`—that maps logically equivalent databases with the same schema history to byte-identical files so they can be verified at **E1** (SHA-256).

**WAL mode as the platform default.** Platform databases in this course run in WAL mode (`journal_mode=wal`), and WAL makes sense as the default throughout Chapter 3. Every experiment database runs in WAL mode with two deliberate rollback-mode (`journal_mode=delete`) exceptions:

- **Experiment 3.1 Part D** runs 12 appends in rollback mode to demonstrate that rollback commits rewrite page 1 on every transaction (advancing header bytes 24–27 and 92–95), turning every ordinary 1-page WAL insert into a 2-page rollback write.
- **Experiment 3.3 Part A2** compares one-transaction versus per-statement batching in rollback mode to show where commit boundaries *do* appear in the file (header bytes 24–27 and 92–95, E2-equal), contrasting with WAL mode where append-only commit boundaries leave no byte difference at all.

One WAL-mode consequence also appears in Experiment 3.3: `VACUUM INTO` always creates a **rollback-mode** file (header bytes 18–19 equal `1, 1`). A platform that restores or compares canonical files must run E1 comparisons before switching the restored file back to WAL mode.

**The Rebuild Ladder (rung R2).** Chapter 1 built rung **R0 (Recipe)** and Chapter 2 built rung **R1 (Pages)** with equality level **E1 bytes** (file SHA-256). Chapter 3 builds rung **R2 (Canonical form)**: capture a database's logical content as an ordered SQL script (`capture/r0.sql`), replay it into a fresh file, prove the rebuild equal to the original at level **E3 logical**, and prove that `VACUUM INTO` brings both to identical bytes at **E1** (within the exact schema-history boundaries measured in Experiment 3.3). This chapter also completes `internal/equal` with levels **E2 pages** (page-by-page comparison ignoring header bytes 24–27, 40–43, and 92–99) and **E3 logical**.

---

## 2. Question

How many pages does one insert or delete change, where do freed pages go, and why do two databases holding the exact same rows end up with different bytes on disk?

The experiments in this chapter reuse four shared packages from earlier chunks (linked rather than inlined):

- [`internal/sqlitex`](../internal/sqlitex/): opens SQLite with explicit file and connection settings, checks the pinned `sqlite_source_id()`, manages `_work/N.M/` scratch directories, runs `PRAGMA wal_checkpoint`, wraps `sqlite3_backup` (`Backup`), and locates built C tools (`LocateTool`).
- [`internal/recipe`](../internal/recipe/): builds the deterministic 30-step R0 database (`accounts`, `documents`, `events`, and `events_by_account`), checks that a database is at rest (`CheckAtRest`), and removes scratch databases (`RemoveDatabase`).
- [`internal/dbfile`](../internal/dbfile/): parses database files in plain Go (header, B-tree pages, cells, varint lengths, local payload limits, table walks, and full page census).
- [`internal/capture`](../internal/capture/): builds rung R1 per-page hash manifests (`BuildManifest`), used in Experiment 3.1 to cross-check incremental SHA-256 reuse.

Chapter 2 introduced `internal/equal` with level E1 and manifests in [`internal/equal/equal.go`](../internal/equal/equal.go) and [`internal/equal/manifest.go`](../internal/equal/manifest.go). Chapter 3 completes `internal/equal` by adding two `Level` constants (`E2Pages`, `E3Logical`) and two `Result` fields (`Ignored`, `IgnoredDiffering`) to `equal.go`, plus four new files—`changes.go`, `pages.go`, `logical.go`, and `dbhash.go`—which are inlined once and walked through in Section 5.1.

---

## 3. Call path

All source citations below are at tag `version-3.53.4`. Two files are copied whole into this chapter directory: [`btree.c`](btree.c) and [`btreeInt.h`](btreeInt.h) (verified by `SOURCES.sha256` against the pinned tree `$SQLITE_SRC` or `_src/sqlite-3.53.4`). Four additional files cited for specific functions—`pager.c`, `backup.c`, `vacuum.c`, and `tool/dbhash.c`—are linked in the pinned tree. Every span below is listed in [`SPANS.txt`](SPANS.txt) and verified by `go run ./cmd/course check spans ch03`.

### 3.1 Cells, freeblocks, and page space

1. **Cell and page layout specification** — [`src/btreeInt.h:149-228`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/btreeInt.h#L149-L228) (local copy: [`btreeInt.h`](btreeInt.h)). Lines 149–215 specify how cells grow upward from the end of the page while the sorted cell-pointer array grows downward from the page header, how unused bytes inside the cell content area form freeblocks (4 bytes or larger: 2-byte next offset, 2-byte size) and fragments (1–3 bytes, summed at page header byte 7), how 1-to-9-byte big-endian varints encode lengths and rowids, and how overflow pages and freelist trunks are laid out. Lines 222–229 define `MX_CELL_SIZE` (`pageSize - 8`) and `MX_CELL` (`(pageSize - 8) / 6`, assuming a 4-byte minimum cell plus 2-byte pointer).

### 3.2 Insert path and splitting (`sqlite3BtreeInsert` to `balance`)

2. **`sqlite3BtreeInsert`** — [`src/btree.c:9409-9710`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/btree.c#L9409-L9710) (local copy: [`btree.c`](btree.c)). The spine of every row or index insert:
   - Saves other cursors open on the same B-tree (line 9439).
   - Fast path for same-size replacement: if the key already exists and the new cell has the exact same size, `btreeOverwriteCell` (lines 9505 and 9557) overwrites the payload in place—one page, no rebalancing.
   - Otherwise seeks the target leaf (`sqlite3BtreeTableMoveto`, line 9513; the `BTREE_APPEND` hint skips the seek on sequential appends), serializes the cell with `fillInCell` (line 9601), marks the target leaf writable with `sqlite3PagerWrite` (line 9614), clears and drops any replaced cell (`BTREE_CLEAR_CELL`, line 9622; `dropCell`, line 9647), and inserts the cell with `insertCellFast` (line 9656).
   - Calls `balance()` (line 9683) **only** if `insertCellFast` could not fit the cell onto the page and queued it as an overflow cell (`pPage->nOverflow > 0`). When the cell fits, `sqlite3BtreeInsert` returns having marked only that one leaf page dirty.
3. **`fillInCell`** — [`src/btree.c:7074-7257`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/btree.c#L7074-L7257) (local copy: [`btree.c`](btree.c)). Writes the payload-length and key varints plus the local payload into a cell buffer. Whenever payload size exceeds the page's local limit (`4,061` bytes on a 4,096-byte table leaf), `fillInCell` allocates overflow pages one at a time via `allocateBtreePage` (line 7212) and chains them with 4-byte next-page pointers.
4. **`insertCellFast`** — [`src/btree.c:7428-7500`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/btree.c#L7428-L7500) (local copy: [`btree.c`](btree.c)). A dedicated copy of `insertCell` called only from `sqlite3BtreeInsert` so the compiler inlines it (comment at lines 7418–7427). If `sz + 2 > pPage->nFree`, it stores the cell in `pPage->apOvfl` for `balance()`. Otherwise it calls `allocateSpace` (line 7472), which searches the freeblock list first-fit (`pageFindSlot`), takes bytes from the middle gap, or compacts the page (`defragmentPage`), then shifts the cell-pointer array and inserts the 2-byte pointer.
5. **`balance`** — [`src/btree.c:9130-9259`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/btree.c#L9130-L9259) (local copy: [`btree.c`](btree.c)). Loops upward from the modified page toward the root until the current page has no overflow cells and its free space is at most two-thirds of usable size (`pPage->nOverflow==0 && pPage->nFree*3 <= (int)pCur->pBt->usableSize*2`, line 9143). At each iteration it dispatches to one of three routines:
   - Root page with an overflow cell → **`balance_deeper`** (line 9158), then continues the loop on the new child page.
   - Non-root rightmost table leaf appending one cell at the end → **`balance_quick`** (lines 9185–9206):
     ```c
     if( pPage->intKeyLeaf
      && pPage->nOverflow==1
      && pPage->aiOvfl[0]==pPage->nCell
      && pParent->pgno!=1
      && pParent->nCell==iIdx
     ){
       ...
       rc = balance_quick(pParent, pPage, aBalanceQuickSpace);
     }
     ```
     Notice `pPage->intKeyLeaf`: **`balance_quick` runs only on rowid table leaves**. Index B-trees and `WITHOUT ROWID` tables have `intKeyLeaf == 0`, so their append splits always fall through to `balance_nonroot`.
   - Every other overfull or underfull non-root page → **`balance_nonroot`** (line 9228), then steps up to `pParent` (`pCur->iPage--`, line 9249) to check whether inserting or removing dividers made the parent overflow or underflow.
6. **`balance_deeper`** — [`src/btree.c:9049-9094`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/btree.c#L9049-L9094) (local copy: [`btree.c`](btree.c)). Grows the tree by one level while keeping the root's page number unchanged:
   ```c
   rc = sqlite3PagerWrite(pRoot->pDbPage);
   if( rc==SQLITE_OK ){
     rc = allocateBtreePage(pBt,&pChild,&pgnoChild,pRoot->pgno,0);
     copyNodeContent(pRoot, pChild, &rc);
   ...
   zeroPage(pRoot, pChild->aData[0] & ~PTF_LEAF);
   put4byte(&pRoot->aData[pRoot->hdrOffset+8], pgnoChild);
   ```
   It allocates a new child page (line 9064), copies all cells and overflow cells from `pRoot` to `pChild` (lines 9065 and 9082–9086), clears `pRoot` as an interior page of the same B-tree kind (`~PTF_LEAF`, line 9089), and installs `pChild` as `pRoot`'s rightmost child (line 9090). The next loop iteration of `balance()` then splits `pChild` (calling `balance_quick` on a first table-leaf overflow, or `balance_nonroot` when a full interior root splits).
7. **`balance_quick`** — [`src/btree.c:8007-8101`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/btree.c#L8007-L8101) (local copy: [`btree.c`](btree.c)). The sequential-append fast path. It allocates one new leaf page (`allocateBtreePage`, line 8025), places only the single overflow cell on it (`rebuildPage`, line 8045), reads the largest rowid from the old leaf's last cell to build a divider cell (lines 8081–8085), inserts that divider at the end of `pParent` (`insertCell`, line 8089), and points `pParent`'s rightmost child at the new leaf (line 8094). **The full old leaf is never rewritten.** At depth 2, an append split modifies exactly three pages: page 1 (in-header `page_count`), the parent interior page, and the new leaf.
8. **`balance_nonroot` header comment and routine** — [`src/btree.c:8204-8244`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/btree.c#L8204-L8244) (comment) and [`src/btree.c:8245-9027`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/btree.c#L8245-L9027) (function, local copy: [`btree.c`](btree.c)). Redistributes cells across the target page and up to two siblings (one on each side, or two on one side at an edge) so all participating pages end with roughly equal free space, adding or freeing up to two pages and updating at most four dividers in `pParent`. Internally it marks every reused sibling writable via `sqlite3PagerWrite` before rebuilding or editing it—which means a sibling whose cells end up unchanged still gets written as a WAL frame (observed in Experiment 3.1 Part B).

### 3.3 Delete, page freeing, and reuse

9. **`sqlite3BtreeDelete`, `BTREE_CLEAR_CELL`, `clearCellOverflow`, `dropCell`, and `freeSpace`** — [`src/btree.c:9841-10041`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/btree.c#L9841-L10041), [`src/btree.c:7053-7059`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/btree.c#L7053-L7059), [`src/btree.c:6979-7045`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/btree.c#L6979-L7045), [`src/btree.c:7267-7309`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/btree.c#L7267-L7309), and [`src/btree.c:1918-2014`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/btree.c#L1918-L2014) (local copy: [`btree.c`](btree.c)). `sqlite3BtreeDelete` expands `BTREE_CLEAR_CELL` (line 9947), which checks whether the cell overflows and calls `clearCellOverflow` to walk and free its overflow chain with `freePage2`. It then calls `dropCell` (line 9948), which shifts the cell-pointer array and calls `freeSpace` (line 7292) to link the cell's bytes into the freeblock list (merging adjacent freeblocks). With `secure_delete` enabled (`BTS_FAST_SECURE` flags), `freeSpace` zeroes those bytes (lines 1992–1996); with `secure_delete=OFF`, the old bytes remain inside the live leaf's freeblock. If the deleted cell was on an interior page (an index B-tree), SQLite replaces it with the largest cell from its left subtree's leaf (lines 9979–9981). Finally, `sqlite3BtreeDelete` invokes `balance()` only if the page now has more than two-thirds of its usable space free (line 10002, calls at lines 10007 and 10016), letting `balance_nonroot` merge underfull siblings and free emptied pages.
10. **`freePage2` and `sqlite3PagerDontWrite`** — [`src/btree.c:6836-6969`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/btree.c#L6836-L6969) (local copy: [`btree.c`](btree.c)) and [`src/pager.c:6329-6339`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/pager.c#L6329-L6339) (in the pinned tree). `freePage2` increments the freelist count at page 1 offset 36 (lines 6860–6862). If `BTS_SECURE_DELETE` is set, it marks the freed page writable and zeroes all `pageSize` bytes (lines 6864–6874). If an existing trunk has room (`nLeaf < usableSize/4 - 8`, line 6910), `freePage2` appends the freed page number to that trunk:
    ```c
    rc = sqlite3PagerWrite(pTrunk->pDbPage);
    if( rc==SQLITE_OK ){
      put4byte(&pTrunk->aData[4], nLeaf+1);
      put4byte(&pTrunk->aData[8+nLeaf*4], iPage);
      if( pPage && (pBt->btsFlags & BTS_SECURE_DELETE)==0 ){
        sqlite3PagerDontWrite(pPage->pDbPage);
      }
      rc = btreeSetHasContent(pBt, iPage);
    }
    ```
    With `secure_delete=OFF`, `sqlite3PagerDontWrite` sets `PGHDR_DONT_WRITE`, telling the pager not to write the freed leaf page at commit at all. That single optimization both keeps deleted row bytes sitting untouched on disk and cuts the WAL frames written by Experiment 3.2's `DELETE` from `54` frames (`ON`) down to `5` frames (`OFF`). When no trunk exists or the first trunk is full, the freed page becomes the new first trunk (lines 6950–6959), overwriting its first 8 bytes (next trunk pointer and leaf count `0`) while leaving its remaining bytes untouched under `secure_delete=OFF`.
11. **`allocateBtreePage`** — [`src/btree.c:6514-6822`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/btree.c#L6514-L6822) (local copy: [`btree.c`](btree.c)). Checks the freelist count at page 1 offset 36 (line 6535). Whenever `nFree > 0`, it decrements the count (line 6572), reads the trunk head at offset 32, and pops either a leaf page from the trunk (picking the one closest to `nearby` when requested, lines 6690–6720) or the trunk itself when its leaf count hits zero. Only when the freelist is empty does it increment `pBt->nPage` (line 6778, skipping the 1 GiB lock-byte page) and write the new in-header database size at page 1 offset 28 (line 6800). Because this check runs per page allocation, a single transaction that needs more pages than the freelist holds pops every freelist page first and then extends the file during the same commit (Experiment 3.2).

### 3.4 Invariants, backup, `VACUUM INTO`, and `dbhash`

12. **`checkTreePage`** — [`src/btree.c:10855-11118`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/btree.c#L10855-L11118) (local copy: [`btree.c`](btree.c)). The core of `PRAGMA integrity_check`: parses each page (`btreeInitPage`, line 10908), checks free-space arithmetic (`btreeComputeFreeSpace`, line 10914), verifies every cell pointer lies inside the content area (line 10970) and does not run off the page, checks key ordering against parent divider bounds (line 10986), confirms all children share the same tree depth (line 11018), walks overflow chains (`checkList`), and uses a min-heap of cell and freeblock extents to confirm no byte is used twice (line 11090) and uncovered bytes match header byte 7 (line 11105).
13. **`sqlite3_backup_step`** — [`src/backup.c:323-591`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/backup.c#L323-L591) (in the pinned tree). Copies pages verbatim from source to destination. At completion, line 448 updates the destination's schema cookie to `iDestSchema + 1` so other connections to the destination reload their schema cache. For an empty destination file (`iDestSchema == 0`), the backup ends up with schema cookie `1` regardless of the source's schema cookie.
14. **`sqlite3RunVacuum`** — [`src/vacuum.c:143-427`](https://github.com/sqlite/sqlite/blob/version-3.53.4/src/vacuum.c#L143-L427) (in the pinned tree). For `VACUUM INTO`, refuses an existing non-empty target file (`"output file already exists"`), attaches the target, copies `page_size` and `auto_vacuum`, recreates tables in `sqlite_schema` rowid order (creation order, with no `ORDER BY`), copies every table's rows via `INSERT INTO ... SELECT * FROM`, recreates indexes and other schema objects, copies persistent header meta values (`BTREE_SCHEMA_VERSION` incremented by `+1`, `BTREE_DEFAULT_CACHE_SIZE`, text encoding, `user_version`, `application_id`), and commits. It does **not** copy WAL mode: the output file always has header bytes 18–19 set to `1, 1` (rollback mode).
15. **`hash_one_query` and `main` in `dbhash`** — [`tool/dbhash.c:281-360`](https://github.com/sqlite/sqlite/blob/version-3.53.4/tool/dbhash.c#L281-L360) and [`tool/dbhash.c:379-491`](https://github.com/sqlite/sqlite/blob/version-3.53.4/tool/dbhash.c#L379-L491) (in the pinned tree). `main` opens each database read-write (lines 433–438, so hot journals can recover, and prints an error to stderr while still exiting 0 if open fails at line 440), skips tables whose names start with `sqlite_` (`sqlite_sequence`, `sqlite_stat*`), runs `SELECT * FROM t` without `ORDER BY` or explicit `rowid`, and hashes no file-header pragmas. `hash_one_query` writes a 1-byte type tag (`'0'`..`'4'`) followed by 8 big-endian bytes for numbers or raw bytes for TEXT/BLOB—with **no length prefix** and **no row or table delimiter**. Experiment 3.3 Part B proves all six resulting blind spots.

### 3.5 Version notes

- **`insertCellFast` vs `insertCell`.** At SQLite 3.53.4, `sqlite3BtreeInsert` calls `insertCellFast` (`src/btree.c:7428-7500`, comment at lines 7418–7427), a single-caller copy of `insertCell` split out for inlining; older texts name `insertCell` on this path (source: `docs/knowledge/source-inventory-ch01-04.md`, section 3.7).
- **`BTREE_CLEAR_CELL` and `clearCellOverflow` vs `clearCell()`.** The older `clearCell()` function does not exist in 3.53.4; `sqlite3BtreeDelete` expands `BTREE_CLEAR_CELL` (`src/btree.c:7053-7059`), which calls `clearCellOverflow` (`src/btree.c:6979-7045`) only when a cell overflows (source: `docs/knowledge/source-inventory-ch01-04.md`, section 3.7).
- **Freelist trunk capacity (`usableSize/4 - 8`).** A freelist trunk has room for `usableSize/4 - 2` leaf pointers, but `freePage2` (`src/btree.c:6910-6928`) deliberately caps it at `usableSize/4 - 8` so database files written by modern SQLite remain readable by versions older than 3.6.0, which had a bounds-check bug on the last six slots (source: comment in `btree.c:6914-6928`).

### 3.6 Experiment 3.1 — Pages per insert (`3.1-pages-per-insert`)

- **Claim (narrowed by review and adversarial testing):** On ascending appends of non-overflowing rows into a rowid table in WAL mode, most inserts change one page (the leaf holding the new rowid, never page 1); an insert that fills a leaf changes several pages (3 pages on a depth-2 append split via `balance_quick`, 4 on the first split/depth change 1→2, 5 on the depth change 2→3, and 5 pages / 6 WAL frames on a middle split via `balance_nonroot`); the root keeps its page number equal to `sqlite_schema.rootpage` across depth changes; and every changed page appears in its commit's WAL frames (though `balance_nonroot` can also ship an unchanged sibling page as an extra frame). Adding secondary indexes multiplies the common per-insert page count by `1 + k`, rollback mode adds page 1 to every commit, and "most inserts change one page" stops holding for random-key inserts into few-row leaves, overflowing payloads, or `WITHOUT ROWID` append splits.
- **Evidence form:** `EXACT`
- **Why kept:** A platform that ships WAL frames pays per changed page. Revision 1 had two experiments tracing which balance routine runs; that attribution is cut and only the page cost is kept.

<!-- include: experiments/3.1-pages-per-insert/main.go -->
```go
// Experiment 3.1 measures how many pages and WAL frames each single-row INSERT
// modifies across five workloads: ascending appends through two depth changes
// (Part A), a middle split into a full leaf (Part B), a table with two secondary
// indexes (Part C), rollback-journal mode (Part D), and four boundary workloads
// showing where "most inserts change one page" stops holding (Part E).
package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nickstrad/sqlite-internals/internal/capture"
	"github.com/nickstrad/sqlite-internals/internal/dbfile"
	"github.com/nickstrad/sqlite-internals/internal/equal"
	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
)

const (
	pageSize      = 4096
	blobBytes     = 1000
	rowidStep     = 2
	walHeaderSize = 32
	walFrameHead  = 24
)

var failures []string

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

func rowBlob(rowid int64) []byte {
	return sizedBlob(rowid, blobBytes)
}

func sizedBlob(rowid int64, size int) []byte {
	label := fmt.Sprintf("row %06d ", rowid)
	return bytes.Repeat([]byte(label), size/len(label)+1)[:size]
}

type lab struct {
	path     string
	mode     string
	db       *sqlitex.DB
	conn     *sql.Conn
	file     *dbfile.File
	manifest equal.Manifest
	image    []byte
	census   *dbfile.Census
}

type step struct {
	number    int
	rowid     int64
	changes   []equal.PageChange
	walPages  []int
	before    *dbfile.File
	after     *dbfile.File
	depth     int
	rootPage  int
	integrity string
}

func openLab(ctx context.Context, path, mode string, statements ...string) *lab {
	options := sqlitex.DefaultOptions()
	options.SingleConnection = true
	options.File.JournalMode = mode
	// SyncOff skips fsync across ~3,000 single-row commits without altering any
	// file byte or WAL frame sequence; WALAutocheckpoint=0 keeps each commit's
	// frames in -wal until snapshot() reads them and runs TRUNCATE.
	options.Conn.Synchronous = sqlitex.SyncOff
	options.Conn.WALAutocheckpoint = 0
	db, err := sqlitex.Open(ctx, path, options)
	if err != nil {
		fatal("open "+filepath.Base(path), err)
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		fatal("conn", err)
	}
	l := &lab{path: path, mode: mode, db: db, conn: conn}
	for _, s := range statements {
		if _, err := conn.ExecContext(ctx, s); err != nil {
			fatal(s, err)
		}
	}
	l.file, l.manifest = l.snapshot(ctx)
	return l
}

func (l *lab) close() {
	if err := l.conn.Close(); err != nil {
		fatal("close conn", err)
	}
	if err := l.db.Close(); err != nil {
		fatal("close db", err)
	}
}

func (l *lab) snapshot(ctx context.Context) (*dbfile.File, equal.Manifest) {
	if l.mode == "wal" {
		result, err := sqlitex.Checkpoint(ctx, l.conn, "main", "TRUNCATE")
		if err != nil || result.Busy {
			fatal("checkpoint", fmt.Errorf("%v %+v", err, result))
		}
	}
	var count int
	if err := l.conn.QueryRowContext(ctx, "PRAGMA page_count").Scan(&count); err != nil {
		fatal("page_count", err)
	}
	image := make([]byte, 0, count*pageSize)
	// Read raw pages through sqlite_dbpage instead of os.Open: opening and
	// closing the database file in the same process that holds a SQLite
	// connection drops that process's POSIX locks (see Chapter 6).
	rows, err := l.conn.QueryContext(ctx, "SELECT pgno, data FROM sqlite_dbpage ORDER BY pgno")
	if err != nil {
		fatal("sqlite_dbpage", err)
	}
	expected := int64(1)
	for rows.Next() {
		var pgno int64
		var data sql.RawBytes
		if err := rows.Scan(&pgno, &data); err != nil {
			fatal("sqlite_dbpage", err)
		}
		if pgno != expected || len(data) != pageSize {
			fatal("sqlite_dbpage", fmt.Errorf("page %d (%d bytes), want page %d", pgno, len(data), expected))
		}
		expected++
		image = append(image, data...)
	}
	if err := rows.Err(); err != nil {
		fatal("sqlite_dbpage", err)
	}
	if err := rows.Close(); err != nil {
		fatal("sqlite_dbpage", err)
	}
	if int(expected-1) != count {
		fatal("sqlite_dbpage", fmt.Errorf("%d pages, page_count says %d", expected-1, count))
	}
	f, err := dbfile.FromBytes(image)
	if err != nil {
		fatal("parse image", err)
	}
	census, err := f.Census()
	if err == nil {
		err = census.Complete()
	}
	if err != nil {
		fatal("census", err)
	}
	m := equal.Manifest{PageSize: pageSize, Entries: make([]equal.ManifestEntry, count)}
	for n := 1; n <= count; n++ {
		page := image[(n-1)*pageSize : n*pageSize]
		sum := ""
		// Reuse the previous manifest's SHA-256 when a page's bytes are unchanged;
		// verifyManifest() checks the final manifest against capture.BuildManifest.
		if l.image != nil && n*pageSize <= len(l.image) && bytes.Equal(page, l.image[(n-1)*pageSize:n*pageSize]) {
			sum = l.manifest.Entries[n-1].SHA256
		} else {
			sum = equal.SHA256Hex(page)
		}
		m.Entries[n-1] = equal.ManifestEntry{Page: n, Role: census.Page(n).Role.String(), SHA256: sum}
	}
	l.image = image
	l.census = census
	return f, m
}

func (l *lab) verifyManifest() bool {
	full, err := capture.BuildManifest(l.file)
	if err != nil {
		fatal("manifest", err)
	}
	return full.Text() == l.manifest.Text()
}

// readWALPages parses the 32-byte WAL header and each 24-byte frame header to
// list the page numbers written by the most recent commit. Reading -wal with
// os.ReadFile is safe while SQLite is open because WAL POSIX locks live on -shm.
func readWALPages(path string) ([]int, error) {
	data, err := os.ReadFile(path + "-wal")
	if err != nil {
		return nil, err
	}
	if len(data) < walHeaderSize {
		return nil, fmt.Errorf("-wal holds %d bytes, less than its header", len(data))
	}
	magic := binary.BigEndian.Uint32(data[0:4])
	if magic != 0x377f0682 && magic != 0x377f0683 {
		return nil, fmt.Errorf("-wal magic %#x", magic)
	}
	frameSize := walFrameHead + pageSize
	if (len(data)-walHeaderSize)%frameSize != 0 {
		return nil, fmt.Errorf("-wal size %d is not a header plus whole frames", len(data))
	}
	var pages []int
	for off := walHeaderSize; off < len(data); off += frameSize {
		pages = append(pages, int(binary.BigEndian.Uint32(data[off:off+4])))
	}
	return pages, nil
}

func (l *lab) integrity(ctx context.Context) string {
	var result string
	if err := l.conn.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&result); err != nil {
		fatal("integrity_check", err)
	}
	return result
}

func (l *lab) rootOf(table string) int {
	schema, err := l.file.Schema()
	if err != nil {
		fatal("schema", err)
	}
	for _, e := range schema {
		if e.Name == table {
			return int(e.RootPage)
		}
	}
	fatal("schema", fmt.Errorf("no table %q", table))
	return 0
}

func (l *lab) depth(root int) int {
	deepest := 0
	for n := 1; n <= l.census.PageCount; n++ {
		info := l.census.Page(n)
		if info.Role != dbfile.RoleTableInterior && info.Role != dbfile.RoleTableLeaf && info.Role != dbfile.RoleIndexInterior && info.Role != dbfile.RoleIndexLeaf {
			continue
		}
		d, page := 1, n
		for page != root && page != 0 {
			page = l.census.Page(page).Parent
			d++
		}
		if page == root {
			deepest = max(deepest, d)
		}
	}
	return deepest
}

func (l *lab) exec(ctx context.Context, number int, rowid int64, table, statement string, args ...any) step {
	before := l.file
	if _, err := l.conn.ExecContext(ctx, statement, args...); err != nil {
		fatal(statement, err)
	}
	s := step{number: number, rowid: rowid, before: before}
	if l.mode == "wal" {
		pages, err := readWALPages(l.path)
		if err != nil {
			fatal("read -wal", err)
		}
		s.walPages = pages
	}
	file, manifest := l.snapshot(ctx)
	s.changes = equal.ChangedPages(l.manifest, manifest)
	s.after = file
	l.file, l.manifest = file, manifest
	s.rootPage = l.rootOf(table)
	s.depth = l.depth(s.rootPage)
	s.integrity = l.integrity(ctx)
	return s
}

func (s step) pages() []int { return equal.PageNumbers(s.changes) }

// leafHoldsRowid verifies that page is a user table leaf (not page 1, whose
// B-tree is sqlite_schema) containing a cell with the given rowid.
func leafHoldsRowid(f *dbfile.File, page int, rowid int64) bool {
	if page == 1 {
		return false
	}
	p, err := f.BTreePage(page)
	if err != nil || p.Type != dbfile.TableLeaf {
		return false
	}
	cells, err := p.Cells()
	if err != nil {
		fatal("cells", err)
	}
	for _, c := range cells {
		if c.HasRowid && c.Rowid == rowid {
			return true
		}
	}
	return false
}

func sameInts(a, b []int) bool {
	x := append([]int(nil), a...)
	y := append([]int(nil), b...)
	sort.Ints(x)
	sort.Ints(y)
	if len(x) != len(y) {
		return false
	}
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

func subtract(a, b []int) []int {
	in := map[int]bool{}
	for _, x := range b {
		in[x] = true
	}
	var out []int
	for _, x := range a {
		if !in[x] {
			out = append(out, x)
		}
	}
	return out
}

func keysOf(p *dbfile.BTreePage) string {
	cells, err := p.Cells()
	if err != nil {
		fatal("cells", err)
	}
	if len(cells) == 0 {
		return "none"
	}
	key := func(c dbfile.Cell) string {
		if c.HasRowid {
			return fmt.Sprint(c.Rowid)
		}
		return "?"
	}
	if len(cells) <= 6 {
		var keys []string
		for _, c := range cells {
			keys = append(keys, key(c))
		}
		return strings.Join(keys, ",")
	}
	return key(cells[0]) + ".." + key(cells[len(cells)-1])
}

func describePage(f *dbfile.File, n int) string {
	if f == nil || n > f.PageCount() {
		return "(not in the file)"
	}
	if n == 1 {
		return fmt.Sprintf("page 1 header: in-header size %d, freelist %d; sqlite_schema leaf with %d cell(s)", f.Header.DatabaseSize, f.Header.FreelistCount, mustPage(f, 1).CellCount)
	}
	p, err := f.BTreePage(n)
	if err != nil {
		return "not a b-tree page"
	}
	text := fmt.Sprintf("%s cells=%d keys=%s content-start=%d unused=%d", p.Type, p.CellCount, keysOf(p), p.CellContentStart, p.Unused())
	if !p.Type.IsLeaf() {
		cells, err := p.Cells()
		if err != nil {
			fatal("cells", err)
		}
		var children []string
		for _, c := range cells {
			children = append(children, fmt.Sprint(c.LeftChild))
		}
		children = append(children, fmt.Sprint(p.RightChild))
		if len(children) > 8 {
			children = append(children[:3], "...", children[len(children)-1])
		}
		text += " children=[" + strings.Join(children, " ") + "]"
	}
	return text
}

func mustPage(f *dbfile.File, n int) *dbfile.BTreePage {
	p, err := f.BTreePage(n)
	if err != nil {
		fatal("page", err)
	}
	return p
}

func printLayout(s step) {
	for _, c := range s.changes {
		fmt.Printf("    page %d before: %s\n", c.Page, describePage(s.before, c.Page))
		fmt.Printf("    page %d after:  %s\n", c.Page, describePage(s.after, c.Page))
	}
}

func formatInts(xs []int) string {
	parts := make([]string, len(xs))
	for i, x := range xs {
		parts[i] = fmt.Sprint(x)
	}
	return "[" + strings.Join(parts, " ") + "]"
}

func printStep(label string, s step) {
	frames := "WAL frames for this commit: " + formatInts(s.walPages)
	if s.walPages == nil {
		frames = "no WAL (rollback mode)"
	}
	fmt.Printf("%s: insert #%d (rowid %d): %d page(s) changed %s; %s; depth %d; root page %d\n",
		label, s.number, s.rowid, len(s.changes), equal.FormatPageChanges(s.changes), frames, s.depth, s.rootPage)
}

type arithmetic struct {
	payload      int
	cellSmall    int
	cellLarge    int
	rowsPerLeaf  int
	dividersFit  int
	firstSplit   int
	deeperInsert int
}

// predict computes leaf capacity, first split, and root interior capacity from
// SQLite's cell format (btreeInt.h:149-228) before any row is inserted.
func predict() arithmetic {
	usable := pageSize
	serialBlob := uint64(12 + 2*blobBytes)
	header := 1 + 1 + dbfile.VarintLength(serialBlob)
	headerVarint := dbfile.VarintLength(uint64(header))
	if headerVarint != 1 {
		fatal("arithmetic", errors.New("record header longer than 127 bytes"))
	}
	payload := header + blobBytes
	maxLocal := dbfile.MaxLocalPayload(usable, dbfile.TableLeaf)
	fmt.Println("arithmetic (from cell size and page size, before inserting anything):")
	fmt.Printf("  row = (id INTEGER PRIMARY KEY, v BLOB of %d bytes); the id is the rowid, so the record stores it as NULL\n", blobBytes)
	fmt.Printf("  record header = 1 (header-size varint) + 1 (serial type 0, NULL) + %d (serial type %d = blob of %d bytes) = %d bytes\n", dbfile.VarintLength(serialBlob), serialBlob, blobBytes, header)
	fmt.Printf("  payload = %d header + %d body = %d bytes; table-leaf max local payload at usable size %d is %d, so no overflow page\n", header, blobBytes, payload, usable, maxLocal)
	cell := func(rowid uint64) int {
		return dbfile.VarintLength(uint64(payload)) + dbfile.VarintLength(rowid) + payload
	}
	small, large := cell(127), cell(128)
	fmt.Printf("  cell = %d (payload-size varint) + rowid varint (1 byte for rowid <= 127, 2 bytes for 128..16383) + %d = %d or %d bytes, plus a 2-byte cell pointer = %d or %d\n", dbfile.VarintLength(uint64(payload)), payload, small, large, small+2, large+2)
	space := usable - 8
	perLeaf := space / (large + 2)
	if space/(small+2) != perLeaf {
		fatal("arithmetic", errors.New("rows per leaf depends on the rowid size"))
	}
	fmt.Printf("  a leaf page other than page 1 has %d - 8 (leaf header) = %d bytes for pointers and cells\n", usable, space)
	fmt.Printf("  rows per leaf = floor(%d / %d) = %d (%d x %d = %d fits; %d x %d = %d does not)\n", space, large+2, perLeaf, perLeaf, large+2, perLeaf*(large+2), perLeaf+1, small+2, (perLeaf+1)*(small+2))
	fmt.Printf("  prediction: inserts 1..%d fill the root leaf; insert #%d (rowid %d) does not fit, so it is the first split, and every %d inserts after it another leaf fills: splits at inserts #%d, #%d, #%d, ... (#%d m + 1)\n",
		perLeaf, perLeaf+1, rowidStep*(perLeaf+1), perLeaf, perLeaf+1, 2*perLeaf+1, 3*perLeaf+1, perLeaf)
	interiorSpace := usable - 12
	used, fit := 0, 0
	for leaf := 1; ; leaf++ {
		key := uint64(rowidStep * perLeaf * leaf)
		size := 4 + dbfile.VarintLength(key) + 2
		if used+size > interiorSpace {
			break
		}
		used += size
		fit++
	}
	small1 := 0
	for leaf := 1; leaf <= fit; leaf++ {
		if rowidStep*perLeaf*leaf <= 127 {
			small1++
		}
	}
	deeper := perLeaf*(fit+1) + 1
	fmt.Printf("  each split adds one divider cell to the root: 4-byte child page number + varint of the left leaf's largest rowid, plus a 2-byte pointer\n")
	fmt.Printf("  the root as an interior page has %d - 12 = %d bytes; %d dividers of 7 bytes (keys <= 127) + %d of 8 bytes = %d bytes fit, one more does not\n", usable, interiorSpace, small1, fit-small1, used)
	fmt.Printf("  prediction: divider %d arrives with the split at insert #%d (rowid %d); the root overflows and the tree gains a third level\n", fit+1, deeper, rowidStep*deeper)
	return arithmetic{payload: payload, cellSmall: small, cellLarge: large, rowsPerLeaf: perLeaf, dividersFit: fit, firstSplit: perLeaf + 1, deeperInsert: deeper}
}

func partA(ctx context.Context, dir string) {
	fmt.Println()
	fmt.Println("== A. appends: one row per transaction into an empty table, a page manifest after every insert (WAL, checkpoint before each manifest)")
	a := predict()
	l := openLab(ctx, filepath.Join(dir, "append.db"), "wal", "CREATE TABLE t(id INTEGER PRIMARY KEY, v BLOB)")
	defer l.close()
	root := l.rootOf("t")
	fmt.Printf("table t: root page %d (sqlite_schema.rootpage), page count %d\n", root, l.file.PageCount())
	total := a.deeperInsert + 2*a.rowsPerLeaf
	interesting := map[int]bool{a.firstSplit: true, a.firstSplit + a.rowsPerLeaf: true, a.firstSplit + 2*a.rowsPerLeaf: true, a.firstSplit + 2*a.rowsPerLeaf + 1: true}
	kept := map[int]step{}
	histogram := map[int]int{}
	var splits []int
	ordinaryOnePage, ordinaryLeafOnly := true, true
	walMatches, integrityOK, rootConstant := true, true, true
	var walMismatch []string
	firstDeeper := map[int]step{}
	prevDepth, frames, inserts := 1, 0, 0
	for n := 1; n <= total; n++ {
		rowid := int64(rowidStep * n)
		s := l.exec(ctx, n, rowid, "t", "INSERT INTO t(id, v) VALUES(?, ?)", rowid, rowBlob(rowid))
		inserts++
		frames += len(s.walPages)
		histogram[len(s.changes)]++
		if s.integrity != "ok" {
			integrityOK = false
		}
		if s.rootPage != root {
			rootConstant = false
		}
		if !sameInts(s.walPages, s.pages()) {
			walMatches = false
			if len(walMismatch) < 5 {
				walMismatch = append(walMismatch, fmt.Sprintf("insert #%d: WAL %s vs changed %s", s.number, formatInts(s.walPages), formatInts(s.pages())))
			}
		}
		if s.after.PageCount() > s.before.PageCount() {
			splits = append(splits, s.number)
		} else {
			if len(s.changes) != 1 {
				ordinaryOnePage = false
			} else if !leafHoldsRowid(s.after, s.changes[0].Page, s.rowid) {
				ordinaryLeafOnly = false
			}
		}
		if s.depth > prevDepth {
			if _, seen := firstDeeper[s.depth]; !seen {
				firstDeeper[s.depth] = s
			}
		}
		prevDepth = s.depth
		if interesting[n] {
			kept[n] = s
		}
	}
	fmt.Printf("%d inserts done; final page count %d, depth %d\n", total, l.file.PageCount(), prevDepth)
	check(l.verifyManifest(), "the last manifest equals capture.BuildManifest of the same image (unchanged pages reuse the previous sha256 only to save time)")

	var counts []int
	for k := range histogram {
		counts = append(counts, k)
	}
	sort.Ints(counts)
	fmt.Println("changed pages per insert -> number of inserts:")
	for _, k := range counts {
		fmt.Printf("  %d page(s): %d insert(s)\n", k, histogram[k])
	}
	predictedSplits := true
	for i, n := range splits {
		if n != a.firstSplit+i*a.rowsPerLeaf {
			predictedSplits = false
		}
	}
	fmt.Printf("splits (inserts that added pages): %d, at inserts #%d, #%d, #%d, ..., #%d\n", len(splits), splits[0], splits[1], splits[2], splits[len(splits)-1])
	check(len(splits) > 0 && splits[0] == a.firstSplit, fmt.Sprintf("the first split happens at insert #%d (rowid %d), as the arithmetic predicts", a.firstSplit, rowidStep*a.firstSplit))
	check(predictedSplits, fmt.Sprintf("every split happens exactly where predicted: insert #%d m + 1, every %d inserts", a.rowsPerLeaf, a.rowsPerLeaf))
	check(ordinaryOnePage, "every insert that does not split changes exactly one page")
	check(ordinaryLeafOnly, "that one page is the table leaf that now holds the new rowid, never page 1 (page 1 is not written)")
	check(histogram[1] > inserts/2, fmt.Sprintf("most inserts change one page (%d of %d)", histogram[1], inserts))

	fmt.Println()
	printStep("first split, depth 1 -> 2", firstDeeper[2])
	printLayout(firstDeeper[2])
	second := kept[a.firstSplit+a.rowsPerLeaf]
	printStep("first append split at depth 2", second)
	printLayout(second)
	third := kept[a.firstSplit+2*a.rowsPerLeaf]
	printStep("next append split", third)
	ordinary := kept[a.firstSplit+2*a.rowsPerLeaf+1]
	printStep("an ordinary insert", ordinary)
	printLayout(ordinary)
	deeper, ok := firstDeeper[3]
	check(ok && deeper.number == a.deeperInsert, fmt.Sprintf("the tree gains its third level at insert #%d (rowid %d), as the arithmetic predicts", a.deeperInsert, rowidStep*a.deeperInsert))
	if ok {
		printStep("depth 2 -> 3", deeper)
		printLayout(deeper)
	}
	check(firstDeeper[2].number == a.firstSplit && len(firstDeeper[2].changes) == 4, "the first split is also the first depth change: the root, a new page holding the root's old cells, a new leaf, and page 1")
	check(len(second.changes) == 3, "an append split at depth 2 changes three pages: page 1 (database size), the parent (one new divider) and the new leaf; the full leaf is not rewritten")
	check(rootConstant, fmt.Sprintf("the root stays page %d through both depth changes and always equals sqlite_schema.rootpage", root))
	check(integrityOK, fmt.Sprintf("integrity_check = ok after every one of the %d inserts", inserts))
	for _, line := range walMismatch {
		fmt.Printf("  %s\n", line)
	}
	check(walMatches, "for every insert the WAL frames of its commit name exactly the pages the manifest shows changed: pages changed = frames shipped")
	fmt.Printf("WAL frames for %d inserts: %d (%d bytes of frames at %d bytes each); the table itself is %d pages\n", inserts, frames, frames*(walFrameHead+pageSize), walFrameHead+pageSize, l.file.PageCount())
}

func partB(ctx context.Context, dir string) {
	fmt.Println()
	fmt.Println("== B. a middle insert into a full leaf")
	l := openLab(ctx, filepath.Join(dir, "middle.db"), "wal", "CREATE TABLE t(id INTEGER PRIMARY KEY, v BLOB)")
	defer l.close()
	var last step
	for n := 1; n <= 20; n++ {
		rowid := int64(rowidStep * n)
		last = l.exec(ctx, n, rowid, "t", "INSERT INTO t(id, v) VALUES(?, ?)", rowid, rowBlob(rowid))
	}
	fmt.Printf("20 appends (rowids 2..40): page count %d, depth %d\n", l.file.PageCount(), last.depth)
	root := mustPage(l.file, l.rootOf("t"))
	fmt.Printf("  root: %s\n", describePage(l.file, root.Number))
	cells, err := root.Cells()
	if err != nil {
		fatal("cells", err)
	}
	for _, c := range cells {
		fmt.Printf("  leaf page %d: %s\n", c.LeftChild, describePage(l.file, int(c.LeftChild)))
	}
	fmt.Printf("  leaf page %d: %s\n", root.RightChild, describePage(l.file, int(root.RightChild)))
	// Insert rowid 11 into full leaf 4 (keys 10..16), forcing balance_nonroot
	// (btree.c:8204-9027) to redistribute cells across sibling leaves.
	rowid := int64(11)
	s := l.exec(ctx, 21, rowid, "t", "INSERT INTO t(id, v) VALUES(?, ?)", rowid, rowBlob(rowid))
	printStep("middle split", s)
	printLayout(s)
	leaves := 0
	for _, c := range s.changes {
		if c.Page == 1 {
			continue
		}
		if p, err := s.after.BTreePage(c.Page); err == nil && p.Type == dbfile.TableLeaf {
			leaves++
		}
	}
	check(leaves >= 2, fmt.Sprintf("a split in the middle rewrites the full leaf and its siblings (%d leaf pages changed), not just one new page", leaves))
	// balance_nonroot calls sqlite3PagerWrite on every reused sibling page before
	// rewriting it (btree.c:8245-9027), so a sibling whose rebuilt cells match its
	// old bytes still enters the WAL as a frame even though its SHA-256 is unchanged.
	extra := subtract(s.walPages, s.pages())
	check(len(subtract(s.pages(), s.walPages)) == 0, "every changed page is in the commit's WAL frames")
	fmt.Printf("WAL frames whose page bytes did not change: %s\n", formatInts(extra))
	unchanged := true
	for _, n := range extra {
		before, err := s.before.Page(n)
		if err != nil {
			fatal("page before", err)
		}
		after, err := s.after.Page(n)
		if err != nil {
			fatal("page after", err)
		}
		if !bytes.Equal(before, after) {
			unchanged = false
		}
		fmt.Printf("  page %d: sha256 before %s, after %s\n", n, equal.SHA256Hex(before)[:16], equal.SHA256Hex(after)[:16])
	}
	check(len(extra) > 0 && unchanged, "here the WAL ships more frames than pages changed: balance_nonroot rewrote a sibling leaf with the same cells it already held, and a written page is a shipped frame even when its bytes are unchanged")
	check(s.integrity == "ok", "integrity_check = ok after the middle split")
	var unused []string
	walker := dbfile.TreeWalker{File: s.after, OnPage: func(p *dbfile.BTreePage, parent, depth int) error {
		if p.Type.IsLeaf() {
			unused = append(unused, fmt.Sprintf("%d:%d", p.Number, p.CellCount))
		}
		return nil
	}}
	if err := walker.Walk(l.rootOf("t")); err != nil {
		fatal("walk", err)
	}
	fmt.Printf("leaves after the middle split (page:cells, in key order): %s\n", strings.Join(unused, " "))
}

func partC(ctx context.Context, dir string) {
	fmt.Println()
	fmt.Println("== C. the same kind of insert into a table with two indexes")
	l := openLab(ctx, filepath.Join(dir, "indexed.db"), "wal",
		"CREATE TABLE t(id INTEGER PRIMARY KEY, email TEXT, score INTEGER, v BLOB)",
		"CREATE INDEX t_email ON t(email)",
		"CREATE INDEX t_score ON t(score)")
	defer l.close()
	histogram := map[int]int{}
	var roleCounts []string
	const inserts = 200
	ok := true
	for n := 1; n <= inserts; n++ {
		rowid := int64(n)
		email := fmt.Sprintf("user%03d@example.com", (n*37)%inserts)
		score := (n * 53) % 97
		s := l.exec(ctx, n, rowid, "t", "INSERT INTO t(id, email, score, v) VALUES(?, ?, ?, ?)", rowid, email, score, rowBlob(rowid)[:200])
		histogram[len(s.changes)]++
		if !sameInts(s.walPages, s.pages()) || s.integrity != "ok" {
			ok = false
		}
		if n == 50 {
			for _, c := range s.changes {
				roleCounts = append(roleCounts, c.String())
			}
		}
	}
	var counts []int
	for k := range histogram {
		counts = append(counts, k)
	}
	sort.Ints(counts)
	fmt.Println("changed pages per insert -> number of inserts:")
	most, mostCount := 0, 0
	for _, k := range counts {
		fmt.Printf("  %d page(s): %d insert(s)\n", k, histogram[k])
		if histogram[k] > mostCount {
			most, mostCount = k, histogram[k]
		}
	}
	fmt.Printf("insert #50 changed: %s\n", strings.Join(roleCounts, " "))
	check(most == 3, "with two indexes the common insert changes three pages: the table leaf and one leaf in each index")
	check(ok, "WAL frames equal the changed pages and integrity_check = ok for all 200 inserts")
}

func partD(ctx context.Context, dir string) {
	fmt.Println()
	fmt.Println("== D. the same appends in rollback mode (journal_mode=DELETE)")
	l := openLab(ctx, filepath.Join(dir, "rollback.db"), "delete", "CREATE TABLE t(id INTEGER PRIMARY KEY, v BLOB)")
	defer l.close()
	histogram := map[string]int{}
	ok, ordinaryTwo, counterOnly := true, true, true
	counterBytes := []equal.ByteRange{{Start: 24, End: 28}, {Start: 92, End: 96}}
	for n := 1; n <= 12; n++ {
		rowid := int64(rowidStep * n)
		s := l.exec(ctx, n, rowid, "t", "INSERT INTO t(id, v) VALUES(?, ?)", rowid, rowBlob(rowid))
		hasPage1 := len(s.changes) > 0 && s.changes[0].Page == 1
		key := fmt.Sprintf("%d page(s), page 1 among them: %v", len(s.changes), hasPage1)
		histogram[key]++
		if !hasPage1 || s.integrity != "ok" {
			ok = false
		}
		if s.after.PageCount() == s.before.PageCount() {
			if len(s.changes) != 2 || !leafHoldsRowid(s.after, s.changes[1].Page, rowid) {
				ordinaryTwo = false
			}
			before, err := s.before.Page(1)
			if err != nil {
				fatal("page 1 before", err)
			}
			after, err := s.after.Page(1)
			if err != nil {
				fatal("page 1 after", err)
			}
			r, err := equal.CompareImagesIgnoring(before, after, pageSize, counterBytes)
			if err != nil {
				fatal("compare page 1", err)
			}
			if !r.Equal || len(r.IgnoredDiffering) == 0 {
				counterOnly = false
			}
		}
		if n <= 2 {
			printStep("rollback", s)
		}
	}
	var keys []string
	for k := range histogram {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("  %s: %d insert(s)\n", k, histogram[k])
	}
	check(ok, "in rollback mode every commit also rewrites page 1")
	check(ordinaryTwo, "an insert that adds no page changes exactly two pages: page 1 and the table leaf that holds the new rowid")
	check(counterOnly, "on those inserts page 1 differs only in the change counter (bytes 24-27) and version-valid-for (bytes 92-95)")
}

type workload struct {
	name   string
	table  string
	create string
	keys   []int64
	size   int
}

type workloadResult struct {
	inserts        int
	onePage        int
	addedPages     int
	rebalanced     int
	minChanged     int
	maxChanged     int
	depth          int
	rowsPerLeaf    string
	walCovers      bool
	integrityOK    bool
	splitsAtDepth2 []int
}

func shuffledKeys(n int, seed uint64) []int64 {
	keys := make([]int64, n)
	for i := range keys {
		keys[i] = int64(i + 1)
	}
	rng := rand.New(rand.NewPCG(seed, 0))
	rng.Shuffle(n, func(i, j int) { keys[i], keys[j] = keys[j], keys[i] })
	return keys
}

func ascendingKeys(n int) []int64 {
	keys := make([]int64, n)
	for i := range keys {
		keys[i] = int64(i + 1)
	}
	return keys
}

func runWorkload(ctx context.Context, dir string, w workload) workloadResult {
	l := openLab(ctx, filepath.Join(dir, w.table+".db"), "wal", w.create)
	defer l.close()
	r := workloadResult{minChanged: -1, walCovers: true, integrityOK: true}
	insert := fmt.Sprintf("INSERT INTO %s(k, v) VALUES(?, ?)", w.table)
	prevDepth := 1
	for i, key := range w.keys {
		s := l.exec(ctx, i+1, key, w.table, insert, key, sizedBlob(key, w.size))
		changed := len(s.changes)
		r.inserts++
		switch {
		case s.after.PageCount() > s.before.PageCount():
			r.addedPages++
			if prevDepth == 2 && s.depth == 2 {
				r.splitsAtDepth2 = append(r.splitsAtDepth2, changed)
			}
		case changed == 1:
			r.onePage++
		default:
			r.rebalanced++
		}
		if r.minChanged < 0 || changed < r.minChanged {
			r.minChanged = changed
		}
		r.maxChanged = max(r.maxChanged, changed)
		if len(subtract(s.pages(), s.walPages)) > 0 {
			r.walCovers = false
		}
		if s.integrity != "ok" {
			r.integrityOK = false
		}
		prevDepth = s.depth
		r.depth = s.depth
	}
	var leaves []int
	for n := 1; n <= l.census.PageCount; n++ {
		role := l.census.Page(n).Role
		if role == dbfile.RoleTableLeaf || role == dbfile.RoleIndexLeaf {
			if n == 1 {
				continue
			}
			p := mustPage(l.file, n)
			leaves = append(leaves, p.CellCount)
		}
	}
	sort.Ints(leaves)
	if len(leaves) > 0 {
		r.rowsPerLeaf = fmt.Sprintf("%d leaves holding %d..%d rows", len(leaves), leaves[0], leaves[len(leaves)-1])
	}
	return r
}

func partE(ctx context.Context, dir string) {
	fmt.Println()
	fmt.Println("== E. where \"most inserts change one page\" stops holding (one row per transaction, WAL, the same manifest after every insert)")
	// Exercise four boundary workloads: random keys with few vs many rows per
	// leaf, overflowing payloads (fillInCell allocates overflow pages at
	// btree.c:7212), and WITHOUT ROWID (an index B-tree, excluded from
	// balance_quick by the pPage->intKeyLeaf guard at btree.c:9185-9189).
	workloads := []workload{
		{"random keys, 1000-byte rows (4 per leaf)", "random_large", "CREATE TABLE random_large(k INTEGER PRIMARY KEY, v BLOB)", shuffledKeys(250, 11), 1000},
		{"random keys, 40-byte rows (about 85 per leaf)", "random_small", "CREATE TABLE random_small(k INTEGER PRIMARY KEY, v BLOB)", shuffledKeys(600, 11), 40},
		{"ascending keys, 5000-byte rows (each overflows)", "overflow", "CREATE TABLE overflow(k INTEGER PRIMARY KEY, v BLOB)", ascendingKeys(25), 5000},
		{"ascending keys, 400-byte rows, WITHOUT ROWID", "without_rowid", "CREATE TABLE without_rowid(k INTEGER PRIMARY KEY, v BLOB) WITHOUT ROWID", ascendingKeys(30), 400},
	}
	results := make([]workloadResult, len(workloads))
	for i, w := range workloads {
		r := runWorkload(ctx, dir, w)
		results[i] = r
		fmt.Printf("  %s: %d inserts; one page: %d; added a page: %d; several pages, none added: %d; pages changed per insert %d..%d; final depth %d, %s\n",
			w.name, r.inserts, r.onePage, r.addedPages, r.rebalanced, r.minChanged, r.maxChanged, r.depth, r.rowsPerLeaf)
		if len(r.splitsAtDepth2) > 0 {
			byCount := map[int]int{}
			for _, n := range r.splitsAtDepth2 {
				byCount[n]++
			}
			var parts []string
			for n := 1; n <= r.maxChanged; n++ {
				if byCount[n] > 0 {
					parts = append(parts, fmt.Sprintf("%d pages: %d", n, byCount[n]))
				}
			}
			fmt.Printf("    inserts that added a page at depth 2, by pages changed: %s\n", strings.Join(parts, ", "))
		}
	}
	large, small, overflow, withoutRowid := results[0], results[1], results[2], results[3]
	check(large.onePage*2 < large.inserts && large.rebalanced > 0, fmt.Sprintf("random keys with 4 rows per leaf: only %d of %d inserts change one page; %d split and %d more change several pages without adding one (balance_nonroot moves cells between siblings)", large.onePage, large.inserts, large.addedPages, large.rebalanced))
	check(small.onePage*2 > small.inserts, fmt.Sprintf("random keys with small rows: most inserts change one page again (%d of %d): the rule depends on how many rows fit in a leaf, not on the key order alone", small.onePage, small.inserts))
	check(overflow.onePage == 0 && overflow.minChanged >= 3, fmt.Sprintf("a row larger than the leaf's local limit also writes its overflow pages and page 1 (the file grows): no insert changes one page, each changes %d..%d", overflow.minChanged, overflow.maxChanged))
	allWide := len(withoutRowid.splitsAtDepth2) > 0
	for _, n := range withoutRowid.splitsAtDepth2 {
		if n <= 3 {
			allWide = false
		}
	}
	check(allWide, "a WITHOUT ROWID table is an index b-tree, and balance_quick serves table leaves only: its append splits at depth 2 go through balance_nonroot and change more than three pages")
	covers, integrity := true, true
	for _, r := range results {
		covers = covers && r.walCovers
		integrity = integrity && r.integrityOK
	}
	check(covers, "in all four, every changed page is in its commit's WAL frames")
	check(integrity, "integrity_check = ok after every insert of all four")
}

func main() {
	ctx := context.Background()
	sqlitex.MustPinCheckMemory(ctx)
	dir := sqlitex.MustWorkDir("3.1")
	fmt.Printf("INFO: work dir %s\n", displayPath(dir))
	fmt.Printf("settings: page_size=%d, journal_mode=wal (part D: delete), synchronous=OFF (no effect on file bytes; it only skips fsync), wal_autocheckpoint=0, one connection, a PRAGMA wal_checkpoint(TRUNCATE) after every insert\n", pageSize)
	partA(ctx, dir)
	partB(ctx, dir)
	partC(ctx, dir)
	partD(ctx, dir)
	partE(ctx, dir)
	fmt.Println()
	if len(failures) > 0 {
		fmt.Printf("PAGES PER INSERT FAILED: %d claim(s) did not hold\n", len(failures))
		for _, f := range failures {
			fmt.Printf("  - %s\n", f)
		}
		os.Exit(1)
	}
	fmt.Println("PAGES PER INSERT OK")
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

Run from the repository root:

```text
go run ./ch03-btrees-and-page-management/experiments/3.1-pages-per-insert
```

Captured evidence:

<!-- include: evidence/3.1-pages-per-insert.txt -->
```text
sqlite_source_id: 2026-07-24 19:02:57 bf7c7f30031888f4e796e429ab3978879485813aaca6f641c7b33e4e09459bcc (pin 3.53.4 ok)
INFO: work dir _work/3.1
settings: page_size=4096, journal_mode=wal (part D: delete), synchronous=OFF (no effect on file bytes; it only skips fsync), wal_autocheckpoint=0, one connection, a PRAGMA wal_checkpoint(TRUNCATE) after every insert

== A. appends: one row per transaction into an empty table, a page manifest after every insert (WAL, checkpoint before each manifest)
arithmetic (from cell size and page size, before inserting anything):
  row = (id INTEGER PRIMARY KEY, v BLOB of 1000 bytes); the id is the rowid, so the record stores it as NULL
  record header = 1 (header-size varint) + 1 (serial type 0, NULL) + 2 (serial type 2012 = blob of 1000 bytes) = 4 bytes
  payload = 4 header + 1000 body = 1004 bytes; table-leaf max local payload at usable size 4096 is 4061, so no overflow page
  cell = 2 (payload-size varint) + rowid varint (1 byte for rowid <= 127, 2 bytes for 128..16383) + 1004 = 1007 or 1008 bytes, plus a 2-byte cell pointer = 1009 or 1010
  a leaf page other than page 1 has 4096 - 8 (leaf header) = 4088 bytes for pointers and cells
  rows per leaf = floor(4088 / 1010) = 4 (4 x 1010 = 4040 fits; 5 x 1009 = 5045 does not)
  prediction: inserts 1..4 fill the root leaf; insert #5 (rowid 10) does not fit, so it is the first split, and every 4 inserts after it another leaf fills: splits at inserts #5, #9, #13, ... (#4 m + 1)
  each split adds one divider cell to the root: 4-byte child page number + varint of the left leaf's largest rowid, plus a 2-byte pointer
  the root as an interior page has 4096 - 12 = 4084 bytes; 15 dividers of 7 bytes (keys <= 127) + 497 of 8 bytes = 4081 bytes fit, one more does not
  prediction: divider 513 arrives with the split at insert #2053 (rowid 4106); the root overflows and the tree gains a third level
table t: root page 2 (sqlite_schema.rootpage), page count 2
2061 inserts done; final page count 520, depth 3
check ok: the last manifest equals capture.BuildManifest of the same image (unchanged pages reuse the previous sha256 only to save time)
changed pages per insert -> number of inserts:
  1 page(s): 1546 insert(s)
  3 page(s): 513 insert(s)
  4 page(s): 1 insert(s)
  5 page(s): 1 insert(s)
splits (inserts that added pages): 515, at inserts #5, #9, #13, ..., #2061
check ok: the first split happens at insert #5 (rowid 10), as the arithmetic predicts
check ok: every split happens exactly where predicted: insert #4 m + 1, every 4 inserts
check ok: every insert that does not split changes exactly one page
check ok: that one page is the table leaf that now holds the new rowid, never page 1 (page 1 is not written)
check ok: most inserts change one page (1546 of 2061)

first split, depth 1 -> 2: insert #5 (rowid 10): 4 page(s) changed [1(table-leaf) 2(table-leaf->table-interior) 3(new table-leaf) 4(new table-leaf)]; WAL frames for this commit: [1 2 3 4]; depth 2; root page 2
    page 1 before: page 1 header: in-header size 2, freelist 0; sqlite_schema leaf with 1 cell(s)
    page 1 after:  page 1 header: in-header size 4, freelist 0; sqlite_schema leaf with 1 cell(s)
    page 2 before: table-leaf cells=4 keys=2,4,6,8 content-start=68 unused=52
    page 2 after:  table-interior cells=1 keys=8 content-start=4091 unused=4077 children=[3 4]
    page 3 before: (not in the file)
    page 3 after:  table-leaf cells=4 keys=2,4,6,8 content-start=68 unused=52
    page 4 before: (not in the file)
    page 4 after:  table-leaf cells=1 keys=10 content-start=3089 unused=3079
first append split at depth 2: insert #9 (rowid 18): 3 page(s) changed [1(table-leaf) 2(table-interior) 5(new table-leaf)]; WAL frames for this commit: [1 2 5]; depth 2; root page 2
    page 1 before: page 1 header: in-header size 4, freelist 0; sqlite_schema leaf with 1 cell(s)
    page 1 after:  page 1 header: in-header size 5, freelist 0; sqlite_schema leaf with 1 cell(s)
    page 2 before: table-interior cells=1 keys=8 content-start=4091 unused=4077 children=[3 4]
    page 2 after:  table-interior cells=2 keys=8,16 content-start=4086 unused=4070 children=[3 4 5]
    page 5 before: (not in the file)
    page 5 after:  table-leaf cells=1 keys=18 content-start=3089 unused=3079
next append split: insert #13 (rowid 26): 3 page(s) changed [1(table-leaf) 2(table-interior) 6(new table-leaf)]; WAL frames for this commit: [1 2 6]; depth 2; root page 2
an ordinary insert: insert #14 (rowid 28): 1 page(s) changed [6(table-leaf)]; WAL frames for this commit: [6]; depth 2; root page 2
    page 6 before: table-leaf cells=1 keys=26 content-start=3089 unused=3079
    page 6 after:  table-leaf cells=2 keys=26,28 content-start=2082 unused=2070
check ok: the tree gains its third level at insert #2053 (rowid 4106), as the arithmetic predicts
depth 2 -> 3: insert #2053 (rowid 4106): 5 page(s) changed [1(table-leaf) 2(table-interior) 516(new table-leaf) 517(new table-interior) 518(new table-interior)]; WAL frames for this commit: [1 2 516 517 518]; depth 3; root page 2
    page 1 before: page 1 header: in-header size 515, freelist 0; sqlite_schema leaf with 1 cell(s)
    page 1 after:  page 1 header: in-header size 518, freelist 0; sqlite_schema leaf with 1 cell(s)
    page 2 before: table-interior cells=512 keys=8..4096 content-start=1039 unused=3 children=[3 4 5 ... 515]
    page 2 after:  table-interior cells=1 keys=2064 content-start=4090 unused=4076 children=[517 518]
    page 516 before: (not in the file)
    page 516 after:  table-leaf cells=1 keys=4106 content-start=3088 unused=3078
    page 517 before: (not in the file)
    page 517 after:  table-interior cells=257 keys=8..2056 content-start=2569 unused=2043 children=[3 4 5 ... 260]
    page 518 before: (not in the file)
    page 518 after:  table-interior cells=255 keys=2072..4104 content-start=2566 unused=2044 children=[261 262 263 ... 516]
check ok: the first split is also the first depth change: the root, a new page holding the root's old cells, a new leaf, and page 1
check ok: an append split at depth 2 changes three pages: page 1 (database size), the parent (one new divider) and the new leaf; the full leaf is not rewritten
check ok: the root stays page 2 through both depth changes and always equals sqlite_schema.rootpage
check ok: integrity_check = ok after every one of the 2061 inserts
check ok: for every insert the WAL frames of its commit name exactly the pages the manifest shows changed: pages changed = frames shipped
WAL frames for 2061 inserts: 3094 (12747280 bytes of frames at 4120 bytes each); the table itself is 520 pages

== B. a middle insert into a full leaf
20 appends (rowids 2..40): page count 7, depth 2
  root: table-interior cells=4 keys=8,16,24,32 content-start=4076 unused=4056 children=[3 4 5 6 7]
  leaf page 3: table-leaf cells=4 keys=2,4,6,8 content-start=68 unused=52
  leaf page 4: table-leaf cells=4 keys=10,12,14,16 content-start=68 unused=52
  leaf page 5: table-leaf cells=4 keys=18,20,22,24 content-start=68 unused=52
  leaf page 6: table-leaf cells=4 keys=26,28,30,32 content-start=68 unused=52
  leaf page 7: table-leaf cells=4 keys=34,36,38,40 content-start=68 unused=52
middle split: insert #21 (rowid 11): 5 page(s) changed [1(table-leaf) 2(table-interior) 4(table-leaf) 5(table-leaf) 8(new table-leaf)]; WAL frames for this commit: [1 2 3 4 5 8]; depth 2; root page 2
    page 1 before: page 1 header: in-header size 7, freelist 0; sqlite_schema leaf with 1 cell(s)
    page 1 after:  page 1 header: in-header size 8, freelist 0; sqlite_schema leaf with 1 cell(s)
    page 2 before: table-interior cells=4 keys=8,16,24,32 content-start=4076 unused=4056 children=[3 4 5 6 7]
    page 2 after:  table-interior cells=5 keys=8,14,20,24,32 content-start=4071 unused=4049 children=[3 4 5 8 6 7]
    page 4 before: table-leaf cells=4 keys=10,12,14,16 content-start=68 unused=52
    page 4 after:  table-leaf cells=4 keys=10,11,12,14 content-start=68 unused=52
    page 5 before: table-leaf cells=4 keys=18,20,22,24 content-start=68 unused=52
    page 5 after:  table-leaf cells=3 keys=16,18,20 content-start=1075 unused=1061
    page 8 before: (not in the file)
    page 8 after:  table-leaf cells=2 keys=22,24 content-start=2082 unused=2070
check ok: a split in the middle rewrites the full leaf and its siblings (3 leaf pages changed), not just one new page
check ok: every changed page is in the commit's WAL frames
WAL frames whose page bytes did not change: [3]
  page 3: sha256 before c0995adc833eadb1, after c0995adc833eadb1
check ok: here the WAL ships more frames than pages changed: balance_nonroot rewrote a sibling leaf with the same cells it already held, and a written page is a shipped frame even when its bytes are unchanged
check ok: integrity_check = ok after the middle split
leaves after the middle split (page:cells, in key order): 3:4 4:4 5:3 8:2 6:4 7:4

== C. the same kind of insert into a table with two indexes
changed pages per insert -> number of inserts:
  3 page(s): 188 insert(s)
  5 page(s): 10 insert(s)
  6 page(s): 2 insert(s)
insert #50 changed: 3(index-leaf) 4(index-leaf) 7(table-leaf)
check ok: with two indexes the common insert changes three pages: the table leaf and one leaf in each index
check ok: WAL frames equal the changed pages and integrity_check = ok for all 200 inserts

== D. the same appends in rollback mode (journal_mode=DELETE)
rollback: insert #1 (rowid 2): 2 page(s) changed [1(table-leaf) 2(table-leaf)]; no WAL (rollback mode); depth 1; root page 2
rollback: insert #2 (rowid 4): 2 page(s) changed [1(table-leaf) 2(table-leaf)]; no WAL (rollback mode); depth 1; root page 2
  2 page(s), page 1 among them: true: 10 insert(s)
  3 page(s), page 1 among them: true: 1 insert(s)
  4 page(s), page 1 among them: true: 1 insert(s)
check ok: in rollback mode every commit also rewrites page 1
check ok: an insert that adds no page changes exactly two pages: page 1 and the table leaf that holds the new rowid
check ok: on those inserts page 1 differs only in the change counter (bytes 24-27) and version-valid-for (bytes 92-95)

== E. where "most inserts change one page" stops holding (one row per transaction, WAL, the same manifest after every insert)
  random keys, 1000-byte rows (4 per leaf): 250 inserts; one page: 86; added a page: 70; several pages, none added: 94; pages changed per insert 1..6; final depth 2, 71 leaves holding 2..4 rows
    inserts that added a page at depth 2, by pages changed: 5 pages: 40, 6 pages: 29
  random keys, 40-byte rows (about 85 per leaf): 600 inserts; one page: 566; added a page: 7; several pages, none added: 27; pages changed per insert 1..6; final depth 2, 8 leaves holding 43..85 rows
    inserts that added a page at depth 2, by pages changed: 5 pages: 1, 6 pages: 5
  ascending keys, 5000-byte rows (each overflows): 25 inserts; one page: 0; added a page: 25; several pages, none added: 0; pages changed per insert 3..5; final depth 2, 7 leaves holding 1..4 rows
    inserts that added a page at depth 2, by pages changed: 3 pages: 15, 4 pages: 5
  ascending keys, 400-byte rows, WITHOUT ROWID: 30 inserts; one page: 23; added a page: 3; several pages, none added: 4; pages changed per insert 1..5; final depth 2, 4 leaves holding 4..9 rows
    inserts that added a page at depth 2, by pages changed: 5 pages: 2
check ok: random keys with 4 rows per leaf: only 86 of 250 inserts change one page; 70 split and 94 more change several pages without adding one (balance_nonroot moves cells between siblings)
check ok: random keys with small rows: most inserts change one page again (566 of 600): the rule depends on how many rows fit in a leaf, not on the key order alone
check ok: a row larger than the leaf's local limit also writes its overflow pages and page 1 (the file grows): no insert changes one page, each changes 3..5
check ok: a WITHOUT ROWID table is an index b-tree, and balance_quick serves table leaves only: its append splits at depth 2 go through balance_nonroot and change more than three pages
check ok: in all four, every changed page is in its commit's WAL frames
check ok: integrity_check = ok after every insert of all four

PAGES PER INSERT OK
```

**Reading the evidence:**

- **Header and arithmetic (lines 1–17).** Line 1 confirms the pinned check-in `bf7c7f30…59bcc`. Before inserting any row, Part A derives the exact split schedule from SQLite's cell format: each `(id INTEGER PRIMARY KEY, v BLOB)` row stores a 4-byte record header (`1` header-size varint + `1` serial type `0` for the rowid-alias `NULL` + `2` bytes for serial type `2012`, a 1,000-byte blob) plus 1,000 payload bytes = `1,004` bytes (`<= 4,061` max local, so zero overflow pages). Adding the 2-byte payload-size varint, 1- or 2-byte rowid varint, and 2-byte cell pointer gives `1,009` or `1,010` bytes per row. A 4,096-byte leaf has `4,088` bytes after its 8-byte header, fitting `floor(4088 / 1010) = 4` rows (`4 × 1010 = 4040`; 5 rows would need `5,045`). That predicts splits at inserts `#5`, `#9`, `#13`, …, `#4m + 1`. For the interior root (`4,096 - 12 = 4,084` bytes), 15 dividers of 7 bytes (keys `<= 127`: `8, 16, …, 120`) plus 497 dividers of 8 bytes take `4,081` bytes (`3` bytes left), predicting that divider `513` at insert `#2053` (rowid `4106`) will push the tree to depth 3.
- **Part A histogram and exact split match (lines 18–30).** After `2,061` appends, the table has `520` pages at depth `3`, and `verifyManifest` confirms the incrementally hashed manifest matches a full `capture.BuildManifest` byte for byte. Of `2,061` inserts, `1,546` changed 1 page, `513` changed 3 pages, `1` changed 4 pages, and `1` changed 5 pages. Every one of the `515` splits occurred at `#4m + 1`, and every non-split insert modified only the single table leaf holding the new rowid—never page 1.
- **First split = first depth change, append splits, and depth 2→3 (lines 32–69).** **Correction to the outline:** in a table that starts empty, the first append split and the first depth change are the **same insert** (`#5`, rowid `10`): `balance()` calls `balance_deeper` to copy root page 2's four cells onto new page 3 and turn page 2 into an interior page, then loops once and calls `balance_quick` on page 3 to allocate new leaf 4 for rowid `10` (4 pages changed: `[1 2 3 4]`). At depth 2, insert `#9` (rowid `18`) takes `balance_quick` alone and changes 3 pages (`[1 2 5]`): full leaf 4 is untouched. At insert `#2053` (rowid `4106`), root page 2 starts with `512` cells and `unused=3` bytes—matching the `4,084 - 4,081 = 3` arithmetic to the byte—and splits via `balance_quick` (leaf `516`) + `balance_deeper` + `balance_nonroot` (new interior pages `517` with `257` cells and `518` with `255` cells, promoting divider `2064` to root page `2`; `257 + 255 + 1 = 513` dividers). None of the 513 existing leaves changed, root stayed page `2`, and `2,061` single-row inserts shipped `3,094` WAL frames (`12,747,280` bytes at `4,120` bytes/frame)—roughly 6× the final 520-page (`2.1 MB`) table.
- **Part B — middle split and unchanged WAL frame (lines 71–96).** After 20 appends (5 full leaves `3..7` under root `2`), inserting rowid `11` into full leaf `4` triggers `balance_nonroot`. Five pages change bytes: `[1 2 4 5 8]` (page 1 size `7 -> 8`, root `2` gets five dividers `8,14,20,24,32`, leaf `4` holds `10,11,12,14`, leaf `5` drops to 3 rows `16,18,20`, new leaf `8` takes `22,24`). **Correction to the outline:** the commit wrote **6** WAL frames `[1 2 3 4 5 8]`, including left sibling page `3` whose SHA-256 was `c0995adc833eadb1` before and `c0995adc833eadb1` after. Because `balance_nonroot` marks every participating sibling writable with `sqlite3PagerWrite`, a page SQLite writes enters the WAL even if its rebuilt bytes are unchanged. Also notice the leaf counts after the split (`3:4 4:4 5:3 8:2 6:4 7:4`): middle inserts leave partly filled leaves, unlike sequential appends.
- **Part C — two secondary indexes (lines 98–105).** Adding `t_email` and `t_score` shifts the mode from `1` page to `3` pages (`188` of `200` inserts changed 3 pages, `10` changed 5, `2` changed 6). Insert `#50` modified `3(index-leaf) 4(index-leaf) 7(table-leaf)`—one leaf per B-tree.
- **Part D — rollback mode (lines 107–115).** Under `journal_mode=DELETE`, every single commit includes page `1` (`10` inserts changed 2 pages, `1` changed 3, `1` changed 4). Every non-growing insert modified exactly page `1` and the leaf holding the rowid, and `equal.CompareImagesIgnoring` proves page 1 differed only inside bytes `24-27` (change counter) and `92-95` (version-valid-for).
- **Part E — where "most inserts change one page" stops holding (lines 117–131).** **Correction to the outline (from adversarial testing):**
  - Random keys with 1,000-byte rows (4/leaf): only `86` of `250` inserts changed one page; `70` added a page (5 pages `40` times, 6 pages `29` times at depth 2) and `94` more changed several pages without adding one because `balance_nonroot` shifted cells between partly empty siblings.
  - Random keys with 40-byte rows (~85/leaf): `566` of `600` inserts changed one page again (`7` splits, `27` rebalances). The one-page rule depends on rows per leaf, not key order alone.
  - Ascending 5,000-byte rows (each exceeding `4,061` max local): `0` of `25` inserts changed one page; every insert allocated an overflow page and updated page 1 (`3..5` pages each).
  - Ascending 400-byte rows into a `WITHOUT ROWID` table: depth-2 append splits changed `5` pages (2 of 2 splits), never 3, because `WITHOUT ROWID` is an index B-tree excluded from `balance_quick` (`btree.c:9185-9189`).

---

## 4. Timeline

The table below lays out the exact page transitions observed in Experiment 3.1 (4,096-byte pages, 1,007/1,008-byte cells, 4 rows per leaf, WAL mode):

| Insert | Pages changed (`WAL frames`) | Page 2 (root) | Other pages |
| --- | --- | --- | --- |
| `#1`–`#4` (rowids `2..8`) | `[2]` each (`[2]`) | `table-leaf`, `1 -> 4` cells, `unused` `3079 -> 52` | none (page 1 untouched) |
| `#5` (rowid `10`), depth `1 -> 2` | `[1 2 3 4]` (`[1 2 3 4]`) | `table-leaf -> table-interior`, `1` divider (`8`), `children=[3 4]`, `unused=4077` | `3`: copy of old root (`2,4,6,8`, `unused=52`); `4`: new leaf (`10`, `unused=3079`); `1`: size `2 -> 4` |
| `#6`–`#8` (rowids `12..16`) | `[4]` each (`[4]`) | untouched | `4` fills from `1` to `4` cells (`unused` `3079 -> 52`) |
| `#9` (rowid `18`), append split | `[1 2 5]` (`[1 2 5]`) | `2` dividers (`8,16`), `children=[3 4 5]`, `unused=4070` | `5`: new leaf (`18`, `unused=3079`); `4`: untouched; `1`: size `4 -> 5` |
| `#14` (rowid `28`), ordinary | `[6]` (`[6]`) | untouched | `6`: `1 -> 2` cells (`26,28`), `content-start` `3089 -> 2082`, `unused` `3079 -> 2070` |
| `#2053` (rowid `4106`), depth `2 -> 3` | `[1 2 516 517 518]` (`[1 2 516 517 518]`) | `512` cells (`unused=3`) `->` `1` divider (`2064`), `children=[517 518]` | `516`: new leaf (`4106`); `517`: interior `257` cells (`8..2056`); `518`: interior `255` cells (`2072..4104`); `1`: size `515 -> 518` |

```text
Depth 1 (after #4):      Depth 2 (after #5):          Depth 3 (after #2053):
  [page 2: leaf]           [page 2: interior]           [page 2: interior]
   keys 2,4,6,8             divider: 8                   divider: 2064
                            /        \                   /              \
                    [page 3: leaf]  [page 4: leaf]  [page 517: int]  [page 518: int]
                     2,4,6,8         10              257 dividers     255 dividers
                                                     children 3..260  children 261..516
```

Now contrast the append split at `#9` (`balance_quick`, 3 pages written, 3 WAL frames) with the middle split in Part B (`balance_nonroot`, insert `#21`, rowid `11` into full leaf `4`):

```text
Before insert #21 (rowid 11):
  page 2 (root): dividers [8, 16, 24, 32], children [3, 4, 5, 6, 7]
    leaf 3: [2, 4, 6, 8]      unused=52
    leaf 4: [10, 12, 14, 16]  unused=52   <-- target leaf is full
    leaf 5: [18, 20, 22, 24]  unused=52
    leaf 6: [26, 28, 30, 32]  unused=52
    leaf 7: [34, 36, 38, 40]  unused=52

After insert #21 (rowid 11):
  WAL frames written: [1, 2, 3, 4, 5, 8]  (6 frames)
  Pages with changed bytes: [1, 2, 4, 5, 8] (5 pages; leaf 3 sha256 c0995adc833eadb1 unchanged)
  page 2 (root): dividers [8, 14, 20, 24, 32], children [3, 4, 5, 8, 6, 7]
    leaf 3: [2, 4, 6, 8]      unused=52   (written to WAL, identical bytes)
    leaf 4: [10, 11, 12, 14]  unused=52
    leaf 5: [16, 18, 20]      unused=1061
    leaf 8: [22, 24]          unused=2070 (new page; page 1 size 7 -> 8)
    leaf 6, 7: untouched
```

In rollback mode (`journal_mode=DELETE`, Part D), the same timeline adds page `1` to every single row: an ordinary append changes `[1, 2]` (2 pages instead of 1) because commit phase one increments the file change counter (bytes `24-27`) and version-valid-for (bytes `92-95`) on page `1`.

---

## 5. Lab

### 5.1 Completing `internal/equal` (page changes, E2, E3, and `dbhash`)

Chapter 2 introduced `internal/equal` with level **E1 bytes** (`CompareImages`, `CompareFiles`) and per-page manifests in [`internal/equal/equal.go`](../internal/equal/equal.go) and [`internal/equal/manifest.go`](../internal/equal/manifest.go). Chapter 3 adds the `E2Pages` and `E3Logical` constants and `Ignored` / `IgnoredDiffering` fields to `equal.go`, plus four new files in `internal/equal/`:

- **`changes.go`** compares two per-page manifests and returns sorted `PageChange` entries (`ChangedPages`). A page is reported only when its SHA-256 changed or it was added or removed—not when its census role changed with identical bytes (which happens to freed leaves under `secure_delete=OFF` because `freePage2` calls `sqlite3PagerDontWrite`).

<!-- include: ../internal/equal/changes.go -->
```go
package equal

import (
	"fmt"
	"sort"
	"strings"
)

// PageChange describes a single page whose bytes differed between two manifests
// or that existed in only one manifest, together with its census roles before
// and after the operation.
type PageChange struct {
	Page       int
	RoleBefore string
	RoleAfter  string
}

// Added reports whether the page did not exist in the before manifest.
func (c PageChange) Added() bool { return c.RoleBefore == "" }

// Removed reports whether the page no longer exists in the after manifest.
func (c PageChange) Removed() bool { return c.RoleAfter == "" }

// String formats c for experiment logs, showing role transitions as well as
// newly allocated and removed pages.
func (c PageChange) String() string {
	switch {
	case c.Added():
		return fmt.Sprintf("%d(new %s)", c.Page, c.RoleAfter)
	case c.Removed():
		return fmt.Sprintf("%d(gone, was %s)", c.Page, c.RoleBefore)
	case c.RoleBefore != c.RoleAfter:
		return fmt.Sprintf("%d(%s->%s)", c.Page, c.RoleBefore, c.RoleAfter)
	}
	return fmt.Sprintf("%d(%s)", c.Page, c.RoleAfter)
}

// ChangedPages compares two per-page hash manifests and returns every page
// whose SHA-256 changed or that was added or removed, sorted by page number.
func ChangedPages(before, after Manifest) []PageChange {
	byPage := func(m Manifest) map[int]ManifestEntry {
		out := make(map[int]ManifestEntry, len(m.Entries))
		for _, e := range m.Entries {
			out[e.Page] = e
		}
		return out
	}
	a, b := byPage(before), byPage(after)
	pages := map[int]bool{}
	for p := range a {
		pages[p] = true
	}
	for p := range b {
		pages[p] = true
	}
	var changes []PageChange
	for p := range pages {
		ea, inA := a[p]
		eb, inB := b[p]
		// Only report byte changes (or added/removed pages). When secure_delete
		// is OFF, freePage2 (btree.c:6934-6935) calls sqlite3PagerDontWrite
		// (pager.c:6329-6339), so a freed leaf changes census role from
		// table-leaf to freelist-leaf without being written to disk or the WAL.
		if inA && inB && ea.SHA256 == eb.SHA256 {
			continue
		}
		changes = append(changes, PageChange{Page: p, RoleBefore: ea.Role, RoleAfter: eb.Role})
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].Page < changes[j].Page })
	return changes
}

// FormatPageChanges formats a slice of PageChange entries inside square brackets.
func FormatPageChanges(changes []PageChange) string {
	parts := make([]string, len(changes))
	for i, c := range changes {
		parts[i] = c.String()
	}
	return "[" + strings.Join(parts, " ") + "]"
}

// PageNumbers extracts the sorted 1-based page numbers from changes.
func PageNumbers(changes []PageChange) []int {
	out := make([]int, len(changes))
	for i, c := range changes {
		out[i] = c.Page
	}
	return out
}
```

- **`pages.go`** implements equality level **E2 pages**: page-by-page comparison ignoring a declared list of file byte ranges (`DeclaredHeaderIgnore`: bytes `24-27`, `40-43`, `92-99`). Header bytes `18-19` are deliberately **not** ignored because they distinguish WAL mode (`2, 2`) from rollback mode (`1, 1`). When two images match outside the ignored ranges, `Result.IgnoredDiffering` records every offset inside those ranges where bytes differed.

<!-- include: ../internal/equal/pages.go -->
```go
package equal

import (
	"bytes"
	"fmt"
	"os"
	"sort"
	"strings"
)

// ByteRange is a half-open file byte offset range [Start, End) ignored during
// an E2Pages comparison.
type ByteRange struct {
	Start int
	End   int
}

// Contains reports whether file offset lies within [r.Start, r.End).
func (r ByteRange) Contains(offset int) bool { return offset >= r.Start && offset < r.End }

// String formats r using inclusive byte offsets (for example "24-27" or "43").
func (r ByteRange) String() string {
	if r.End-r.Start == 1 {
		return fmt.Sprintf("%d", r.Start)
	}
	return fmt.Sprintf("%d-%d", r.Start, r.End-1)
}

// DeclaredHeaderIgnore lists the volatile page-1 header byte ranges ignored by
// E2Pages: bytes 24-27 (file change counter, pager.c:3130-3143), bytes 40-43
// (schema cookie, incremented by sqlite3_backup_step at backup.c:448 and by
// sqlite3RunVacuum at vacuum.c:143-427), and bytes 92-99 (version-valid-for
// and SQLite library version). Header bytes 18-19 (write/read format version)
// are deliberately NOT ignored because they distinguish WAL from rollback mode.
var DeclaredHeaderIgnore = []ByteRange{{24, 28}, {40, 44}, {92, 100}}

// FormatRanges formats a slice of ByteRange values as a comma-separated list.
func FormatRanges(ranges []ByteRange) string {
	parts := make([]string, len(ranges))
	for i, r := range ranges {
		parts[i] = r.String()
	}
	return strings.Join(parts, ", ")
}

func formatOffsets(offsets []int) string {
	parts := make([]string, len(offsets))
	for i, o := range offsets {
		parts[i] = fmt.Sprintf("%d", o)
	}
	return "[" + strings.Join(parts, " ") + "]"
}

// ValidateRanges verifies that every range has Start >= 0 and End > Start.
func ValidateRanges(ranges []ByteRange) error {
	for _, r := range ranges {
		if r.Start < 0 || r.End <= r.Start {
			return fmt.Errorf("invalid ignored byte range %d..%d", r.Start, r.End)
		}
	}
	return nil
}

func ignored(ranges []ByteRange, offset int) bool {
	for _, r := range ranges {
		if r.Contains(offset) {
			return true
		}
	}
	return false
}

// CompareImagesIgnoring compares raw database images a and b page by page at
// level E2Pages, allowing byte differences only at file offsets covered by
// ignore. When the images are equal outside ignore, Result.IgnoredDiffering
// records every file offset inside ignore where a and b differed.
func CompareImagesIgnoring(a, b []byte, pageSize int, ignore []ByteRange) (Result, error) {
	if err := ValidateRanges(ignore); err != nil {
		return Result{}, err
	}
	if pageSize <= 0 {
		return Result{}, fmt.Errorf("page size %d: an E2 comparison needs the page size", pageSize)
	}
	ranges := append([]ByteRange(nil), ignore...)
	sort.Slice(ranges, func(i, j int) bool { return ranges[i].Start < ranges[j].Start })
	r := Result{Level: E2Pages, SHA256A: SHA256Hex(a), SHA256B: SHA256Hex(b), SizeA: int64(len(a)), SizeB: int64(len(b)), PageSize: pageSize, Ignored: ranges}
	pages := (max(len(a), len(b)) + pageSize - 1) / pageSize
	for p := range pages {
		pageA, pageB := pageOf(a, p, pageSize), pageOf(b, p, pageSize)
		if bytes.Equal(pageA, pageB) {
			continue
		}
		if len(pageA) != len(pageB) || !equalOutside(pageA, pageB, p*pageSize, ranges, &r.IgnoredDiffering) {
			if r.FirstDifferingPage == 0 {
				r.FirstDifferingPage = p + 1
			}
			r.DifferingPages++
		}
	}
	r.Equal = r.DifferingPages == 0 && r.SizeA == r.SizeB
	if !r.Equal {
		// Only report IgnoredDiffering when every difference fell inside ignore.
		r.IgnoredDiffering = nil
	}
	return r, nil
}

func equalOutside(pageA, pageB []byte, base int, ranges []ByteRange, differing *[]int) bool {
	var inIgnored []int
	for i := range pageA {
		if pageA[i] == pageB[i] {
			continue
		}
		if !ignored(ranges, base+i) {
			return false
		}
		inIgnored = append(inIgnored, base+i)
	}
	*differing = append(*differing, inIgnored...)
	return true
}

// ComparePages compares raw database images a and b at level E2Pages using
// DeclaredHeaderIgnore.
func ComparePages(a, b []byte, pageSize int) (Result, error) {
	return CompareImagesIgnoring(a, b, pageSize, DeclaredHeaderIgnore)
}

// ComparePageFiles reads database files a and b from disk and compares them
// with ComparePages. Do not call on files open by SQLite in the same process.
func ComparePageFiles(a, b string, pageSize int) (Result, error) {
	imageA, err := os.ReadFile(a)
	if err != nil {
		return Result{}, err
	}
	imageB, err := os.ReadFile(b)
	if err != nil {
		return Result{}, err
	}
	return ComparePages(imageA, imageB, pageSize)
}
```

- **`logical.go`** implements equality level **E3 logical** and the reusable `WalkLogical` visitor (also used by Experiment 3.3's R2 dump so capture and verification read the exact same content the exact same way). Five design details in `logical.go` came directly from SQLite internals and adversarial testing:
  1. Every query qualifies `main` explicitly (`main.sqlite_schema`, `pragma_table_info(?, 'main')`, `pragma_index_list(?, 'main')`, `FROM main."t"`, `PRAGMA main.*`) so a `TEMP` table on a caller's connection cannot shadow a `main` table.
  2. Every column in `TableSelect` is prefixed with unary `+` (`+"c"`) so `modernc.org/sqlite` sees an expression with no declared column type and returns raw SQLite storage classes (`nil`, `int64`, `float64`, `string`, `[]byte`) instead of converting `DATETIME` text into `time.Time`.
  3. For rowid tables, `rowidAlias` picks the first unused name among `rowid`, `_rowid_`, `oid`, falling back to an exact `INTEGER PRIMARY KEY` column (with no `pk`-origin autoindex, excluding `INTEGER PRIMARY KEY DESC`, `INT PRIMARY KEY`, and composite keys) before refusing a genuinely hidden rowid.
  4. `LogicalHash` checks `ctx.Err()` first, then runs both `BEGIN` and the closing statement (`ROLLBACK` on error, or `COMMIT` with a `ROLLBACK` fallback on success) under `context.WithoutCancel(ctx)` while the walk itself runs under `ctx`, so a canceled context never leaks an open transaction or pinned WAL read mark. `LogicalHashFile` checks `HasLiveSideFile`: it opens `mode=ro` when a non-empty `-wal` or `-journal` exists (so closing does not checkpoint and delete the `-wal`), and `mode=rw` when the file is at rest (so an at-rest WAL file does not gain leftover sidecars).
  5. Every value is tagged and length-prefixed (`N`, `I`, `R`, `T`, `B`; unsupported Go types passed directly to `EncodeValue` are tagged `U` + type + value), and all seven persistent pragmas—including `default_cache_size` (header bytes `48-51`, copied by `VACUUM INTO`)—are hashed in `LogicalPragmas` order, paired with `PRAGMA main.integrity_check` and sorted `PRAGMA main.foreign_key_check`.

<!-- include: ../internal/equal/logical.go -->
```go
package equal

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"math"
	"net/url"
	"os"
	"slices"
	"sort"
	"strings"

	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
)

// LogicalFormat is written at the start of the E3Logical SHA-256 stream so
// future format revisions cannot collide with v1 digests.
const LogicalFormat = "sqlite-internals logical hash v1"

// LogicalPragmas lists the seven persistent file-level PRAGMA values hashed by
// E3Logical in fixed order. It includes default_cache_size (header bytes 48-51)
// because sqlite3RunVacuum (vacuum.c:143-427) copies it to the canonical file.
var LogicalPragmas = []string{"page_size", "encoding", "auto_vacuum", "user_version", "application_id", "journal_mode", "default_cache_size"}

// SchemaRow represents one logical entry from main.sqlite_schema (omitting the
// physical rootpage column).
type SchemaRow struct {
	Type  string
	Name  string
	Table string
	SQL   sql.NullString
}

// TableInfo describes a user or shadow table in main walked by WalkLogical.
type TableInfo struct {
	Name         string
	Kind         string
	WithoutRowid bool
	RowidColumn  string
	Columns      []string
}

// LogicalVisitor receives callbacks in deterministic order during WalkLogical.
// Any callback field may be nil.
type LogicalVisitor struct {
	Schema    func(SchemaRow) error
	TableDone func(TableInfo, int) error
	Table     func(TableInfo) error
	Row       func(TableInfo, []any) error
	Pragma    func(name, value string) error
}

// QuoteIdentifier returns name as a double-quoted SQLite identifier with
// internal double quotes doubled.
func QuoteIdentifier(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// RowsQuerier is the query subset of *sql.DB, *sql.Conn, and *sql.Tx required
// by WalkLogical and TableSelect.
type RowsQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// WalkLogical visits the main database's schema rows, every table and shadow
// table's rows in explicit key order, and every persistent pragma in
// LogicalPragmas. Callers on a single-connection pool must not run nested
// queries on that same pool inside callbacks.
func WalkLogical(ctx context.Context, q RowsQuerier, v LogicalVisitor) error {
	if err := walkSchema(ctx, q, v); err != nil {
		return err
	}
	tables, err := logicalTables(ctx, q)
	if err != nil {
		return err
	}
	for _, t := range tables {
		if err := walkTable(ctx, q, t, v); err != nil {
			return err
		}
	}
	for _, name := range LogicalPragmas {
		value, err := pragmaValue(ctx, q, name)
		if err != nil {
			return err
		}
		if v.Pragma != nil {
			if err := v.Pragma(name, value); err != nil {
				return err
			}
		}
	}
	return nil
}

func walkSchema(ctx context.Context, q RowsQuerier, v LogicalVisitor) error {
	// Qualify main.sqlite_schema explicitly so a caller's TEMP objects never shadow main.
	// Omit rootpage because page numbers are physical layout, not logical content.
	rows, err := q.QueryContext(ctx, "SELECT type, name, tbl_name, sql FROM main.sqlite_schema ORDER BY type, name")
	if err != nil {
		return fmt.Errorf("read sqlite_schema: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var r SchemaRow
		if err := rows.Scan(&r.Type, &r.Name, &r.Table, &r.SQL); err != nil {
			return fmt.Errorf("read sqlite_schema: %w", err)
		}
		if v.Schema != nil {
			if err := v.Schema(r); err != nil {
				return err
			}
		}
	}
	return rows.Err()
}

func logicalTables(ctx context.Context, q RowsQuerier) ([]TableInfo, error) {
	// Include both ordinary tables and shadow tables (such as FTS5 backing tables),
	// as well as sqlite_sequence and sqlite_stat* when present; exclude sqlite_schema
	// because walkSchema already handles it.
	rows, err := q.QueryContext(ctx, "SELECT name, type, wr FROM pragma_table_list WHERE schema = 'main' AND type IN ('table', 'shadow') AND name NOT IN ('sqlite_schema', 'sqlite_master') ORDER BY name")
	if err != nil {
		return nil, fmt.Errorf("list tables: %w", err)
	}
	var tables []TableInfo
	for rows.Next() {
		var t TableInfo
		var withoutRowid int64
		if err := rows.Scan(&t.Name, &t.Kind, &withoutRowid); err != nil {
			rows.Close()
			return nil, fmt.Errorf("list tables: %w", err)
		}
		t.WithoutRowid = withoutRowid != 0
		tables = append(tables, t)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range tables {
		columns, err := tableColumns(ctx, q, tables[i].Name)
		if err != nil {
			return nil, err
		}
		tables[i].Columns = columns
		if !tables[i].WithoutRowid {
			alias, err := rowidAlias(ctx, q, tables[i].Name, columns)
			if err != nil {
				return nil, err
			}
			tables[i].RowidColumn = alias
		}
	}
	return tables, nil
}

func tableColumns(ctx context.Context, q RowsQuerier, table string) ([]string, error) {
	rows, err := q.QueryContext(ctx, "SELECT name FROM pragma_table_info(?, 'main') ORDER BY cid", table)
	if err != nil {
		return nil, fmt.Errorf("columns of %q: %w", table, err)
	}
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		columns = append(columns, name)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(columns) == 0 {
		return nil, fmt.Errorf("table %q has no columns", table)
	}
	return columns, nil
}

func rowidAlias(ctx context.Context, q RowsQuerier, table string, columns []string) (string, error) {
	used := map[string]bool{}
	for _, c := range columns {
		used[strings.ToLower(c)] = true
	}
	for _, alias := range []string{"rowid", "_rowid_", "oid"} {
		if !used[alias] {
			return alias, nil
		}
	}
	// If columns claim all three built-in aliases, the rowid is still accessible
	// when one column is a true INTEGER PRIMARY KEY alias.
	ipk, err := integerPrimaryKeyColumn(ctx, q, table)
	if err != nil {
		return "", err
	}
	if ipk != "" {
		return ipk, nil
	}
	return "", fmt.Errorf("table %q: columns named rowid, _rowid_ and oid hide the rowid", table)
}

func integerPrimaryKeyColumn(ctx context.Context, q RowsQuerier, table string) (string, error) {
	keys, err := primaryKeyColumns(ctx, q, table)
	if err != nil || len(keys) != 1 {
		return "", err
	}
	var declared string
	if err := q.QueryRowContext(ctx, "SELECT type FROM pragma_table_info(?, 'main') WHERE pk = 1", table).Scan(&declared); err != nil {
		return "", fmt.Errorf("primary key of %q: %w", table, err)
	}
	// Only an exact INTEGER declaration (not INT) can alias the rowid.
	if !strings.EqualFold(declared, "INTEGER") {
		return "", nil
	}
	// INTEGER PRIMARY KEY DESC creates a separate autoindex (origin = 'pk') and
	// does NOT alias the rowid; reject tables that have a pk-origin index.
	var pkIndexes int
	if err := q.QueryRowContext(ctx, "SELECT count(*) FROM pragma_index_list(?, 'main') WHERE origin = 'pk'", table).Scan(&pkIndexes); err != nil {
		return "", fmt.Errorf("indexes of %q: %w", table, err)
	}
	if pkIndexes > 0 {
		return "", nil
	}
	return keys[0], nil
}

func primaryKeyColumns(ctx context.Context, q RowsQuerier, table string) ([]string, error) {
	rows, err := q.QueryContext(ctx, "SELECT name FROM pragma_table_info(?, 'main') WHERE pk > 0 ORDER BY pk", table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

// TableSelect builds the deterministic SELECT statement used to read table t
// from main in explicit key order (by rowid for rowid tables, or by primary key
// columns for WITHOUT ROWID tables).
func TableSelect(ctx context.Context, q RowsQuerier, t TableInfo) (string, error) {
	var selected, order []string
	if t.WithoutRowid {
		keys, err := primaryKeyColumns(ctx, q, t.Name)
		if err != nil {
			return "", err
		}
		if len(keys) == 0 {
			return "", fmt.Errorf("WITHOUT ROWID table %q has no primary key columns", t.Name)
		}
		for _, k := range keys {
			order = append(order, QuoteIdentifier(k))
		}
	} else {
		selected = append(selected, QuoteIdentifier(t.RowidColumn))
		order = append(order, QuoteIdentifier(t.RowidColumn))
	}
	// Prefix every column with unary '+' to strip its declared column type:
	// modernc.org/sqlite otherwise converts TEXT in DATE/DATETIME/TIMESTAMP
	// columns to time.Time, losing the underlying SQLite storage class.
	for _, c := range t.Columns {
		selected = append(selected, "+"+QuoteIdentifier(c))
	}
	return fmt.Sprintf("SELECT %s FROM main.%s ORDER BY %s", strings.Join(selected, ", "), QuoteIdentifier(t.Name), strings.Join(order, ", ")), nil
}

func walkTable(ctx context.Context, q RowsQuerier, t TableInfo, v LogicalVisitor) error {
	query, err := TableSelect(ctx, q, t)
	if err != nil {
		return err
	}
	if v.Table != nil {
		if err := v.Table(t); err != nil {
			return err
		}
	}
	rows, err := q.QueryContext(ctx, query)
	if err != nil {
		return fmt.Errorf("read table %q: %w", t.Name, err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		width := len(t.Columns)
		if !t.WithoutRowid {
			width++
		}
		values := make([]any, width)
		pointers := make([]any, width)
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			return fmt.Errorf("read table %q: %w", t.Name, err)
		}
		for i, value := range values {
			if _, err := StorageClassOf(value); err != nil {
				return fmt.Errorf("table %q row %d column %d: %w", t.Name, count+1, i, err)
			}
		}
		count++
		if v.Row != nil {
			if err := v.Row(t, values); err != nil {
				return err
			}
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read table %q: %w", t.Name, err)
	}
	if v.TableDone != nil {
		return v.TableDone(t, count)
	}
	return nil
}

// StorageClassOf maps a scanned Go value to its SQLite storage class name
// ("null", "integer", "real", "text", or "blob") and rejects any other Go type.
func StorageClassOf(value any) (string, error) {
	switch value.(type) {
	case nil:
		return "null", nil
	case int64:
		return "integer", nil
	case float64:
		return "real", nil
	case string:
		return "text", nil
	case []byte:
		return "blob", nil
	}
	return "", fmt.Errorf("value of Go type %T has no SQLite storage class", value)
}

func pragmaValue(ctx context.Context, q RowsQuerier, name string) (string, error) {
	var value any
	if err := q.QueryRowContext(ctx, "PRAGMA main."+name).Scan(&value); err != nil {
		return "", fmt.Errorf("PRAGMA %s: %w", name, err)
	}
	text := fmt.Sprint(value)
	if b, ok := value.([]byte); ok {
		text = string(b)
	}
	switch name {
	case "journal_mode":
		// Only the WAL vs rollback distinction persists in header bytes 18-19;
		// rollback submodes (delete, truncate, persist) are per-connection.
		if strings.EqualFold(text, "wal") {
			return "wal", nil
		}
		return "rollback", nil
	case "auto_vacuum":
		switch text {
		case "0":
			return "none", nil
		case "1":
			return "full", nil
		case "2":
			return "incremental", nil
		}
	}
	return text, nil
}

// LogicalTableDigest holds the row count and per-table SHA-256 computed during
// LogicalHash so CompareLogical can name the exact table that differed.
type LogicalTableDigest struct {
	Name   string
	Rows   int
	SHA256 string
}

// LogicalPragma holds one persistent file-level pragma name and normalized value.
type LogicalPragma struct {
	Name  string
	Value string
}

// Logical holds the full E3 logical fingerprint of a database, including its
// overall SHA-256, schema and per-table digests, persistent pragmas, and the
// results of PRAGMA main.integrity_check and PRAGMA main.foreign_key_check.
type Logical struct {
	SHA256          string
	SchemaSHA256    string
	SchemaRows      int
	Tables          []LogicalTableDigest
	Pragmas         []LogicalPragma
	Integrity       string
	ForeignKeyCheck []string
}

// TableByName looks up a table's digest in l by name.
func (l Logical) TableByName(name string) (LogicalTableDigest, bool) {
	for _, t := range l.Tables {
		if t.Name == name {
			return t, true
		}
	}
	return LogicalTableDigest{}, false
}

// Pragma looks up a persistent pragma value recorded in l by name.
func (l Logical) Pragma(name string) (string, bool) {
	for _, p := range l.Pragmas {
		if p.Name == name {
			return p.Value, true
		}
	}
	return "", false
}

// Summary formats l as a single human-readable line for experiment output.
func (l Logical) Summary() string {
	var tables []string
	for _, t := range l.Tables {
		tables = append(tables, fmt.Sprintf("%s=%d", t.Name, t.Rows))
	}
	var pragmas []string
	for _, p := range l.Pragmas {
		pragmas = append(pragmas, p.Name+"="+p.Value)
	}
	return fmt.Sprintf("logical sha256 %s; schema rows %d; table rows %s; %s; integrity_check %s; foreign_key_check %d row(s)", l.SHA256, l.SchemaRows, strings.Join(tables, " "), strings.Join(pragmas, " "), l.Integrity, len(l.ForeignKeyCheck))
}

type streamEncoder struct {
	w io.Writer
}

func (e streamEncoder) tag(t byte) {
	e.w.Write([]byte{t})
}

func (e streamEncoder) length(n int) {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(n))
	e.w.Write(b[:])
}

func (e streamEncoder) bytes(tag byte, b []byte) {
	e.tag(tag)
	e.length(len(b))
	e.w.Write(b)
}

func (e streamEncoder) text(tag byte, s string) { e.bytes(tag, []byte(s)) }

func (e streamEncoder) value(value any) error {
	switch x := value.(type) {
	case nil:
		e.tag('N')
	case int64:
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], uint64(x))
		e.tag('I')
		e.w.Write(b[:])
	case float64:
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], math.Float64bits(x))
		e.tag('R')
		e.w.Write(b[:])
	case string:
		e.text('T', x)
	case []byte:
		e.bytes('B', x)
	default:
		return fmt.Errorf("value of Go type %T has no SQLite storage class", value)
	}
	return nil
}

// EncodeValue encodes a single value using the tagged, length-prefixed framing
// of E3Logical: 'N' (NULL), 'I' + 8-byte big-endian int64, 'R' + 8-byte
// big-endian IEEE-754 bits, 'T'/'B' + 8-byte length + bytes. Length prefixes
// prevent boundary collisions such as ('a3','b') vs ('a','3b') that dbhash
// misses (tool/dbhash.c:281-360). Values with no SQLite storage class are
// tagged with 'U' + Go type + 'v' + Go representation so direct callers never
// see empty or colliding encodings.
func EncodeValue(value any) []byte {
	var b bytes.Buffer
	e := streamEncoder{&b}
	if err := e.value(value); err != nil {
		b.Reset()
		e.text('U', fmt.Sprintf("%T", value))
		e.text('v', fmt.Sprintf("%#v", value))
	}
	return b.Bytes()
}

// LogicalHash computes the E3Logical fingerprint of conn's main database
// inside a single read transaction so concurrent writers cannot split the
// snapshot.
func LogicalHash(ctx context.Context, conn *sql.Conn) (Logical, error) {
	// Check cancellation up front, then run deferred BEGIN without cancellation.
	// modernc.org/sqlite can return ctx.Err() after a statement already succeeded
	// (stmt.go checks its watcher flag before disarming it), which would leave an
	// open transaction behind an error. A deferred BEGIN takes no locks.
	if err := ctx.Err(); err != nil {
		return Logical{}, fmt.Errorf("begin read transaction: %w", err)
	}
	uncancelable := context.WithoutCancel(ctx)
	if _, err := conn.ExecContext(uncancelable, "BEGIN"); err != nil {
		return Logical{}, fmt.Errorf("begin read transaction: %w", err)
	}
	result, err := logicalHashInTx(ctx, conn)
	// End the transaction without cancellation so a canceled caller context never
	// leaks an open read transaction or pins a WAL read mark on conn.
	if err != nil {
		if _, rollbackErr := conn.ExecContext(uncancelable, "ROLLBACK"); rollbackErr != nil {
			return Logical{}, errors.Join(err, fmt.Errorf("end read transaction: %w", rollbackErr))
		}
		return Logical{}, err
	}
	// If COMMIT fails, roll back before returning so conn never stays inside a
	// transaction.
	if _, commitErr := conn.ExecContext(uncancelable, "COMMIT"); commitErr != nil {
		if _, rollbackErr := conn.ExecContext(uncancelable, "ROLLBACK"); rollbackErr != nil {
			return Logical{}, fmt.Errorf("end read transaction: %w", errors.Join(commitErr, rollbackErr))
		}
		return Logical{}, fmt.Errorf("end read transaction: %w", commitErr)
	}
	return result, nil
}

func logicalHashInTx(ctx context.Context, conn *sql.Conn) (Logical, error) {
	var result Logical
	whole := sha256.New()
	all := streamEncoder{whole}
	all.text('V', LogicalFormat)
	schema := sha256.New()
	var table hash.Hash
	var tableEncoder streamEncoder
	visitor := LogicalVisitor{
		Schema: func(r SchemaRow) error {
			for _, e := range []streamEncoder{all, {schema}} {
				e.tag('S')
				e.text('t', r.Type)
				e.text('n', r.Name)
				e.text('b', r.Table)
				if r.SQL.Valid {
					e.text('q', r.SQL.String)
				} else {
					e.tag('N')
				}
			}
			result.SchemaRows++
			return nil
		},
		Table: func(t TableInfo) error {
			table = sha256.New()
			tableEncoder = streamEncoder{table}
			for _, e := range []streamEncoder{all, tableEncoder} {
				e.text('T', t.Name)
				if t.WithoutRowid {
					e.tag('w')
				} else {
					e.tag('r')
				}
				e.length(len(t.Columns))
				for _, c := range t.Columns {
					e.text('c', c)
				}
			}
			return nil
		},
		Row: func(t TableInfo, values []any) error {
			for _, e := range []streamEncoder{all, tableEncoder} {
				e.tag('R')
				e.length(len(values))
				for _, value := range values {
					if err := e.value(value); err != nil {
						return err
					}
				}
			}
			return nil
		},
		TableDone: func(t TableInfo, rows int) error {
			for _, e := range []streamEncoder{all, tableEncoder} {
				e.tag('E')
				e.length(rows)
			}
			result.Tables = append(result.Tables, LogicalTableDigest{Name: t.Name, Rows: rows, SHA256: hex.EncodeToString(table.Sum(nil))})
			return nil
		},
		Pragma: func(name, value string) error {
			all.tag('P')
			all.text('n', name)
			all.text('v', value)
			result.Pragmas = append(result.Pragmas, LogicalPragma{Name: name, Value: value})
			return nil
		},
	}
	if err := WalkLogical(ctx, conn, visitor); err != nil {
		return Logical{}, err
	}
	result.SHA256 = hex.EncodeToString(whole.Sum(nil))
	result.SchemaSHA256 = hex.EncodeToString(schema.Sum(nil))
	integrity, err := integrityCheck(ctx, conn)
	if err != nil {
		return Logical{}, err
	}
	result.Integrity = integrity
	fk, err := foreignKeyCheck(ctx, conn)
	if err != nil {
		return Logical{}, err
	}
	result.ForeignKeyCheck = fk
	return result, nil
}

func integrityCheck(ctx context.Context, q RowsQuerier) (string, error) {
	rows, err := q.QueryContext(ctx, "PRAGMA main.integrity_check")
	if err != nil {
		return "", fmt.Errorf("integrity_check: %w", err)
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return "", err
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, " / "), rows.Err()
}

func foreignKeyCheck(ctx context.Context, q RowsQuerier) ([]string, error) {
	rows, err := q.QueryContext(ctx, "PRAGMA main.foreign_key_check")
	if err != nil {
		return nil, fmt.Errorf("foreign_key_check: %w", err)
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var table, parent string
		var rowid sql.NullInt64
		var fkid int64
		if err := rows.Scan(&table, &rowid, &parent, &fkid); err != nil {
			return nil, err
		}
		row := "NULL"
		if rowid.Valid {
			row = fmt.Sprint(rowid.Int64)
		}
		lines = append(lines, fmt.Sprintf("%s rowid=%s parent=%s fkid=%d", table, row, parent, fkid))
	}
	sort.Strings(lines)
	return lines, rows.Err()
}

// ErrNoDatabase is returned when OpenExisting or LogicalHashFile is given a
// path that does not exist.
var ErrNoDatabase = errors.New("database file does not exist")

// OpenExisting opens an existing database file in read-write mode without
// creating missing files or running any PRAGMA changes.
func OpenExisting(ctx context.Context, path string) (*sql.DB, error) {
	return openExisting(ctx, path, "rw")
}

func openForReading(ctx context.Context, path string) (*sql.DB, error) {
	busy, err := HasLiveSideFile(path)
	if err != nil {
		return nil, err
	}
	// If a live -wal or -journal exists, open read-only so closing does not
	// checkpoint the WAL into the main file and delete -wal. When the file is
	// at rest, open read-write because a read-only open of an at-rest WAL file
	// leaves a -wal/-shm sidecar behind.
	if busy {
		return openExisting(ctx, path, "ro")
	}
	return openExisting(ctx, path, "rw")
}

// HasLiveSideFile reports whether path has a non-empty -wal or -journal file
// beside it.
func HasLiveSideFile(path string) (bool, error) {
	for _, suffix := range []string{"-wal", "-journal"} {
		info, err := os.Stat(path + suffix)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return false, err
		}
		if info.Size() > 0 {
			return true, nil
		}
	}
	return false, nil
}

func openExisting(ctx context.Context, path, mode string) (*sql.DB, error) {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%s: %w", path, ErrNoDatabase)
		}
		return nil, err
	}
	if info.IsDir() {
		return nil, fmt.Errorf("%s is a directory", path)
	}
	uri, err := sqlitex.FileURI(path, url.Values{"mode": {mode}})
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", uri)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// LogicalHashFile opens path on a dedicated connection without altering its
// settings, computes LogicalHash, and closes the connection.
func LogicalHashFile(ctx context.Context, path string) (Logical, error) {
	db, err := openForReading(ctx, path)
	if err != nil {
		return Logical{}, err
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		db.Close()
		return Logical{}, err
	}
	result, err := LogicalHash(ctx, conn)
	if closeErr := conn.Close(); closeErr != nil && err == nil {
		err = closeErr
	}
	if closeErr := db.Close(); closeErr != nil && err == nil {
		err = closeErr
	}
	return result, err
}

// LogicalResult summarizes an E3Logical comparison between two Logical digests.
type LogicalResult struct {
	Level       Level
	Equal       bool
	SHA256A     string
	SHA256B     string
	Differences []string
}

// String formats r as a single-line E3 verdict naming any differences found.
func (r LogicalResult) String() string {
	if r.Equal {
		return fmt.Sprintf("%s: equal (logical sha256 %s; integrity_check ok; foreign_key_check unchanged)", r.Level, r.SHA256A)
	}
	return fmt.Sprintf("%s: differ: %s", r.Level, strings.Join(r.Differences, "; "))
}

// CompareLogical compares two Logical fingerprints at level E3Logical. Two
// databases are equal only when their logical hash streams match, both pass
// integrity_check ("ok"), and their sorted foreign_key_check lists are equal.
func CompareLogical(a, b Logical) LogicalResult {
	r := LogicalResult{Level: E3Logical, SHA256A: a.SHA256, SHA256B: b.SHA256}
	if a.SchemaSHA256 != b.SchemaSHA256 {
		r.Differences = append(r.Differences, fmt.Sprintf("schema differs (%d vs %d rows)", a.SchemaRows, b.SchemaRows))
	}
	names := map[string]bool{}
	for _, t := range a.Tables {
		names[t.Name] = true
	}
	for _, t := range b.Tables {
		names[t.Name] = true
	}
	var sorted []string
	for name := range names {
		sorted = append(sorted, name)
	}
	sort.Strings(sorted)
	for _, name := range sorted {
		ta, okA := a.TableByName(name)
		tb, okB := b.TableByName(name)
		switch {
		case !okA:
			r.Differences = append(r.Differences, fmt.Sprintf("table %s only in the second", name))
		case !okB:
			r.Differences = append(r.Differences, fmt.Sprintf("table %s only in the first", name))
		case ta.SHA256 != tb.SHA256:
			r.Differences = append(r.Differences, fmt.Sprintf("table %s differs (%d vs %d rows)", name, ta.Rows, tb.Rows))
		}
	}
	for _, p := range a.Pragmas {
		other, ok := b.Pragma(p.Name)
		if !ok || other != p.Value {
			r.Differences = append(r.Differences, fmt.Sprintf("pragma %s %s vs %s", p.Name, p.Value, other))
		}
	}
	for _, p := range b.Pragmas {
		if _, ok := a.Pragma(p.Name); !ok {
			r.Differences = append(r.Differences, fmt.Sprintf("pragma %s  vs %s", p.Name, p.Value))
		}
	}
	if a.SHA256 != b.SHA256 && len(r.Differences) == 0 {
		r.Differences = append(r.Differences, "logical streams differ")
	}
	if a.Integrity != "ok" {
		r.Differences = append(r.Differences, "integrity_check of the first: "+a.Integrity)
	}
	if b.Integrity != "ok" {
		r.Differences = append(r.Differences, "integrity_check of the second: "+b.Integrity)
	}
	if !slices.Equal(a.ForeignKeyCheck, b.ForeignKeyCheck) {
		r.Differences = append(r.Differences, fmt.Sprintf("foreign_key_check changed (%d vs %d rows)", len(a.ForeignKeyCheck), len(b.ForeignKeyCheck)))
	}
	r.Equal = len(r.Differences) == 0
	return r
}

// CompareLogicalFiles hashes database files a and b with LogicalHashFile and
// compares the resulting Logical digests.
func CompareLogicalFiles(ctx context.Context, a, b string) (LogicalResult, Logical, Logical, error) {
	la, err := LogicalHashFile(ctx, a)
	if err != nil {
		return LogicalResult{}, Logical{}, Logical{}, err
	}
	lb, err := LogicalHashFile(ctx, b)
	if err != nil {
		return LogicalResult{}, Logical{}, Logical{}, err
	}
	return CompareLogical(la, lb), la, lb, nil
}
```

- **`dbhash.go`** locates and invokes the pinned `dbhash` binary as an independent cross-check, passing a `file:…?mode=ro` URI whenever `HasLiveSideFile` is true and turning any stderr output into a Go error (because `dbhash` exits `0` after printing open errors to stderr at `tool/dbhash.c:440`).

<!-- include: ../internal/equal/dbhash.go -->
```go
package equal

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"strings"

	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
)

// ErrNoDBHash is returned by LocateDBHash when the pinned dbhash binary is not
// available on the system.
var ErrNoDBHash = errors.New("dbhash not found")

// LocateDBHash finds the dbhash binary built from the pinned SQLite tree.
func LocateDBHash() (string, error) {
	path, err := sqlitex.LocateTool("dbhash")
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrNoDBHash, err)
	}
	return path, nil
}

// RunDBHash executes the pinned dbhash tool against dbPath and returns its
// 40-character hexadecimal SHA-1 digest.
func RunDBHash(ctx context.Context, tool, dbPath string) (string, error) {
	target := dbPath
	busy, err := HasLiveSideFile(dbPath)
	if err != nil {
		return "", err
	}
	// dbhash opens files read-write by default (tool/dbhash.c:433-438) with
	// SQLITE_OPEN_URI enabled. When a live -wal or -journal exists, pass a
	// mode=ro URI so closing dbhash does not checkpoint and delete the WAL.
	if busy {
		target, err = sqlitex.FileURI(dbPath, url.Values{"mode": {"ro"}})
		if err != nil {
			return "", err
		}
	}
	cmd := exec.CommandContext(ctx, tool, target)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("dbhash %s: %w: %s", dbPath, err, strings.TrimSpace(stderr.String()))
	}
	// dbhash exits 0 even when a database cannot be opened, logging the error
	// to stderr and continuing (tool/dbhash.c:440), so treat any stderr as failure.
	if stderr.Len() > 0 {
		return "", fmt.Errorf("dbhash %s: %s", dbPath, strings.TrimSpace(stderr.String()))
	}
	fields := strings.Fields(stdout.String())
	if len(fields) < 2 || len(fields[0]) != 40 {
		return "", fmt.Errorf("dbhash %s: unexpected output %q", dbPath, stdout.String())
	}
	return fields[0], nil
}
```

### 5.2 Experiment 3.2 — Delete and reuse (`3.2-delete-and-reuse`)

- **Claim (narrowed by review and adversarial testing):** With `auto_vacuum=NONE` (SQLite's default), deleting rows moves emptied pages to the freelist and never shrinks the file; later inserts reuse freelist pages before growing the file on a per-page-allocation basis; `VACUUM` returns the space; and freed pages (as well as freeblocks on live leaves and the unused tail of a freelist trunk) keep old row bytes unless `secure_delete=ON`—which zeroes the database file at rest at the cost of extra WAL frames, while leaving prior `INSERT` frames readable in `-wal` until the WAL is reset (for example by `wal_checkpoint(TRUNCATE)`). By contrast, `auto_vacuum=FULL` shrinks on the `DELETE`'s own commit, and `auto_vacuum=INCREMENTAL` shrinks on `PRAGMA incremental_vacuum`.
- **Evidence form:** `EXACT`

<!-- include: experiments/3.2-delete-and-reuse/main.go -->
```go
// Experiment 3.2 tracks page freeing, freelist reuse, file growth, VACUUM,
// auto_vacuum modes, and deleted-byte residue across secure_delete=OFF and
// secure_delete=ON in both the database file at rest and the -wal file.
package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nickstrad/sqlite-internals/internal/dbfile"
	"github.com/nickstrad/sqlite-internals/internal/recipe"
	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
)

const (
	pageSize    = 4096
	blobBytes   = 1000
	initialRows = 400
	deleteFirst = 99
	deleteLast  = 302
	reuseRows   = 100
	growBatches = 30
	rowsPerTx   = 4
)

var failures []string

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

func marker(prefix string, id int64) []byte {
	return []byte(fmt.Sprintf("%s-%06d", prefix, id))
}

func rowBlob(prefix string, id int64) []byte {
	unit := append(marker(prefix, id), ' ')
	return bytes.Repeat(unit, blobBytes/len(unit)+1)[:blobBytes]
}

type fileState struct {
	size      int64
	pages     int
	freelist  int
	trunks    []int
	leaves    []int
	census    *dbfile.Census
	image     []byte
	tableLeaf map[int][]int64
}

// readState inspects a closed, at-rest database file directly from disk so no
// open SQLite connection holds POSIX locks in this process.
func readState(path string) fileState {
	if err := recipe.CheckAtRest(path); err != nil {
		fatal("at rest", err)
	}
	image, err := os.ReadFile(path)
	if err != nil {
		fatal("read", err)
	}
	f, err := dbfile.FromBytes(image)
	if err != nil {
		fatal("parse", err)
	}
	census, err := f.Census()
	if err == nil {
		err = census.Complete()
	}
	if err != nil {
		fatal("census", err)
	}
	s := fileState{size: int64(len(image)), pages: f.PageCount(), freelist: census.FreelistLength(), trunks: census.FreelistTrunks, leaves: census.FreelistLeaves, census: census, image: image, tableLeaf: map[int][]int64{}}
	schema, err := f.Schema()
	if err != nil {
		fatal("schema", err)
	}
	for _, e := range schema {
		if e.Name != "t" {
			continue
		}
		err := f.WalkTable(int(e.RootPage), func(r dbfile.Row) error {
			s.tableLeaf[r.Page] = append(s.tableLeaf[r.Page], r.Rowid)
			return nil
		})
		if err != nil {
			fatal("walk", err)
		}
	}
	return s
}

func (s fileState) freePages() map[int]bool {
	out := map[int]bool{}
	for _, p := range s.trunks {
		out[p] = true
	}
	for _, p := range s.leaves {
		out[p] = true
	}
	return out
}

func (s fileState) summary() string {
	return fmt.Sprintf("file %d bytes = %d pages; freelist %d page(s) (trunk %s, %d leaves)", s.size, s.pages, s.freelist, ints(s.trunks), len(s.leaves))
}

func ints(xs []int) string {
	parts := make([]string, len(xs))
	for i, x := range xs {
		parts[i] = fmt.Sprint(x)
	}
	return "[" + strings.Join(parts, " ") + "]"
}

func compactPages(xs []int) string {
	if len(xs) == 0 {
		return "none"
	}
	sorted := append([]int(nil), xs...)
	sort.Ints(sorted)
	var parts []string
	start, prev := sorted[0], sorted[0]
	flush := func() {
		if start == prev {
			parts = append(parts, fmt.Sprint(start))
		} else {
			parts = append(parts, fmt.Sprintf("%d-%d", start, prev))
		}
	}
	for _, x := range sorted[1:] {
		if x == prev+1 {
			prev = x
			continue
		}
		flush()
		start, prev = x, x
	}
	flush()
	return strings.Join(parts, ",")
}

type finding struct {
	rowsFound  int
	rowsSought int
	byRole     map[string]int
	pages      map[int]bool
}

type markerRange struct {
	prefix      string
	first, last int64
}

var firstDeletion = []markerRange{{"SECRET", deleteFirst, deleteLast}}

var secondDeletion = []markerRange{{"NEW", 1001, 1000 + reuseRows}, {"GROW", 2001, 2000 + growBatches*rowsPerTx}}

func searchDeleted(s fileState) finding {
	return searchMarkers(s, firstDeletion)
}

func searchMarkers(s fileState, ranges []markerRange) finding {
	f := finding{byRole: map[string]int{}, pages: map[int]bool{}}
	for _, r := range ranges {
		for id := r.first; id <= r.last; id++ {
			f.rowsSought++
			if f.searchOne(s, marker(r.prefix, id)) {
				f.rowsFound++
			}
		}
	}
	return f
}

func (f finding) searchOne(s fileState, needle []byte) bool {
	found := false
	for offset := 0; ; {
		i := bytes.Index(s.image[offset:], needle)
		if i < 0 {
			return found
		}
		at := offset + i
		page := at/pageSize + 1
		if !f.pages[page] {
			f.pages[page] = true
			f.byRole[s.census.Page(page).Role.String()]++
		}
		found = true
		offset = at + len(needle)
	}
}

func (f finding) String() string {
	var roles []string
	for role, n := range f.byRole {
		roles = append(roles, fmt.Sprintf("%s=%d", role, n))
	}
	sort.Strings(roles)
	text := fmt.Sprintf("%d of %d deleted rows still readable in the file, on %d page(s)", f.rowsFound, f.rowsSought, len(f.pages))
	if len(roles) > 0 {
		text += ": " + strings.Join(roles, " ")
	}
	return text
}

func open(ctx context.Context, path string, secureDelete bool) *sqlitex.DB {
	options := sqlitex.DefaultOptions()
	options.SingleConnection = true
	options.Conn.SecureDelete = secureDelete
	db, err := sqlitex.Open(ctx, path, options)
	if err != nil {
		fatal("open", err)
	}
	return db
}

func session(ctx context.Context, path string, secureDelete bool, work func(db *sqlitex.DB)) {
	db := open(ctx, path, secureDelete)
	work(db)
	var integrity string
	if err := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil {
		fatal("integrity_check", err)
	}
	if integrity != "ok" {
		fatal("integrity_check", fmt.Errorf("%s", integrity))
	}
	if err := db.Close(); err != nil {
		fatal("close", err)
	}
}

func execAll(ctx context.Context, db *sqlitex.DB, statement string, rows [][]any) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		fatal("begin", err)
	}
	for _, args := range rows {
		if _, err := tx.ExecContext(ctx, statement, args...); err != nil {
			fatal(statement, err)
		}
	}
	if err := tx.Commit(); err != nil {
		fatal("commit", err)
	}
}

func pragmaInt(ctx context.Context, db *sqlitex.DB, name string) int {
	var v int
	if err := db.QueryRowContext(ctx, "PRAGMA "+name).Scan(&v); err != nil {
		fatal(name, err)
	}
	return v
}

func insertRows(prefix string, first, count int64) [][]any {
	var rows [][]any
	for id := first; id < first+count; id++ {
		rows = append(rows, []any{id, rowBlob(prefix, id)})
	}
	return rows
}

type outcome struct {
	afterDelete       finding
	afterReuse        finding
	afterSecondDelete finding
	afterVacuum       finding
	sizes             []int64
	deleteFrames      int
}

// walFrames computes the frame count from the -wal file length (32-byte header
// plus 24-byte frame header and 4096-byte page image per frame) before close
// checkpoints and deletes the WAL.
func walFrames(path string) int {
	info, err := os.Stat(path + "-wal")
	if err != nil {
		fatal("stat -wal", err)
	}
	if info.Size() < 32 || (info.Size()-32)%(24+pageSize) != 0 {
		fatal("-wal size", fmt.Errorf("%d bytes", info.Size()))
	}
	return int((info.Size() - 32) / (24 + pageSize))
}

func run(ctx context.Context, dir string, secureDelete bool) outcome {
	label := "OFF"
	if secureDelete {
		label = "ON"
	}
	path := filepath.Join(dir, "secure-delete-"+strings.ToLower(label), "t.db")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		fatal("mkdir", err)
	}
	fmt.Println()
	fmt.Printf("== secure_delete=%s (WAL; every step is its own connection, so the file is at rest and checkpointed when it is read)\n", label)
	var o outcome
	insert := "INSERT INTO t(id, v) VALUES(?, ?)"
	session(ctx, path, secureDelete, func(db *sqlitex.DB) {
		if _, err := db.ExecContext(ctx, "CREATE TABLE t(id INTEGER PRIMARY KEY, v BLOB)"); err != nil {
			fatal("create", err)
		}
		execAll(ctx, db, insert, insertRows("SECRET", 1, initialRows))
	})
	base := readState(path)
	o.sizes = append(o.sizes, base.size)
	fmt.Printf("1. %d rows of %d-byte blobs (each blob repeats a marker like %q): %s; %d table-leaf pages\n", initialRows, blobBytes, marker("SECRET", 150), base.summary(), len(base.tableLeaf))
	var held []int
	for page, ids := range base.tableLeaf {
		for _, id := range ids {
			if id >= deleteFirst && id <= deleteLast {
				held = append(held, page)
				break
			}
		}
	}

	// Under secure_delete=OFF, freePage2 (btree.c:6836-6969) calls
	// sqlite3PagerDontWrite (pager.c:6329-6339) for freed freelist leaves, so
	// they are not written to the WAL and retain their old bytes; freeSpace
	// (btree.c:1918-2014) likewise skips zeroing freeblocks on live leaves.
	session(ctx, path, secureDelete, func(db *sqlitex.DB) {
		if _, err := db.ExecContext(ctx, "DELETE FROM t WHERE id BETWEEN ? AND ?", deleteFirst, deleteLast); err != nil {
			fatal("delete", err)
		}
		o.deleteFrames = walFrames(path)
	})
	deleted := readState(path)
	o.sizes = append(o.sizes, deleted.size)
	fmt.Printf("2. DELETE FROM t WHERE id BETWEEN %d AND %d (%d rows, held on leaf pages %s): %s\n", deleteFirst, deleteLast, deleteLast-deleteFirst+1, compactPages(held), deleted.summary())
	free := deleted.freePages()
	var freedFromHeld []int
	for _, p := range held {
		if free[p] {
			freedFromHeld = append(freedFromHeld, p)
		}
	}
	fmt.Printf("   pages now on the freelist: %s; of them, former leaves of the deleted rows: %d\n", compactPages(append(append([]int(nil), deleted.trunks...), deleted.leaves...)), len(freedFromHeld))
	fmt.Printf("   the DELETE's commit wrote %d WAL frames\n", o.deleteFrames)
	check(deleted.size == base.size, "the file does not shrink on delete")
	check(deleted.freelist > 0 && len(freedFromHeld) > 0, "pages emptied by the delete moved to the freelist")
	o.afterDelete = searchDeleted(deleted)
	fmt.Printf("   search for the deleted markers: %s\n", o.afterDelete)

	session(ctx, path, secureDelete, func(db *sqlitex.DB) {
		execAll(ctx, db, insert, insertRows("NEW", 1001, reuseRows))
	})
	reused := readState(path)
	o.sizes = append(o.sizes, reused.size)
	var newPages, fromFreelist []int
	for page, ids := range reused.tableLeaf {
		for _, id := range ids {
			if id >= 1001 {
				newPages = append(newPages, page)
				if free[page] {
					fromFreelist = append(fromFreelist, page)
				}
				break
			}
		}
	}
	fmt.Printf("3. %d new rows (ids 1001..%d): %s\n", reuseRows, 1000+reuseRows, reused.summary())
	fmt.Printf("   leaf pages holding the new rows: %s; of them taken from the freelist: %d\n", compactPages(newPages), len(fromFreelist))
	check(reused.size == base.size, "the new rows fit in the file: its size is unchanged")
	check(len(newPages) > 0 && len(fromFreelist) == len(newPages), "every leaf page of the new rows is a former freelist page")
	o.afterReuse = searchDeleted(reused)
	fmt.Printf("   search for the deleted markers: %s\n", o.afterReuse)

	grewBeforeEmpty := false
	var growth []string
	session(ctx, path, secureDelete, func(db *sqlitex.DB) {
		next := int64(2001)
		for batch := 1; batch <= growBatches; batch++ {
			freeBefore := pragmaInt(ctx, db, "freelist_count")
			pagesBefore := pragmaInt(ctx, db, "page_count")
			execAll(ctx, db, insert, insertRows("GROW", next, rowsPerTx))
			next += rowsPerTx
			freeAfter := pragmaInt(ctx, db, "freelist_count")
			pagesAfter := pragmaInt(ctx, db, "page_count")
			if pagesAfter > pagesBefore && freeAfter > 0 {
				grewBeforeEmpty = true
			}
			if pagesAfter != pagesBefore || batch == 1 || freeBefore != freeAfter && freeAfter == 0 {
				growth = append(growth, fmt.Sprintf("batch %d: freelist %d->%d, page_count %d->%d", batch, freeBefore, freeAfter, pagesBefore, pagesAfter))
			}
		}
	})
	grown := readState(path)
	o.sizes = append(o.sizes, grown.size)
	fmt.Printf("4. %d more transactions of %d rows each: %s\n", growBatches, rowsPerTx, grown.summary())
	for i, line := range growth {
		if i < 4 || i == len(growth)-1 {
			fmt.Printf("   %s\n", line)
		} else if i == 4 {
			fmt.Printf("   ... (%d more batches that grew the file)\n", len(growth)-5)
		}
	}
	check(!grewBeforeEmpty, "page_count grows only once the freelist is used up: every transaction that added pages ended with an empty freelist")
	check(grown.size > base.size && grown.freelist == 0, "once the freelist is used up, the file grows")

	session(ctx, path, secureDelete, func(db *sqlitex.DB) {
		if _, err := db.ExecContext(ctx, "DELETE FROM t WHERE id >= 1001"); err != nil {
			fatal("delete", err)
		}
	})
	again := readState(path)
	o.sizes = append(o.sizes, again.size)
	fmt.Printf("5. DELETE FROM t WHERE id >= 1001: %s\n", again.summary())
	check(again.size == grown.size, "the file still does not shrink")
	o.afterSecondDelete = searchMarkers(again, secondDeletion)
	fmt.Printf("   search for the NEW- and GROW- markers just deleted: %s\n", o.afterSecondDelete)

	session(ctx, path, secureDelete, func(db *sqlitex.DB) {
		if _, err := db.ExecContext(ctx, "VACUUM"); err != nil {
			fatal("vacuum", err)
		}
	})
	vacuumed := readState(path)
	o.sizes = append(o.sizes, vacuumed.size)
	o.afterVacuum = searchMarkers(vacuumed, append(append([]markerRange(nil), firstDeletion...), secondDeletion...))
	fmt.Printf("6. VACUUM: %s\n", vacuumed.summary())
	fmt.Printf("   search for the deleted markers: %s\n", o.afterVacuum)
	check(vacuumed.freelist == 0 && vacuumed.pages <= again.pages-again.freelist, fmt.Sprintf("VACUUM is what returns space: %d pages -> %d, at least the %d freelist pages are gone", again.pages, vacuumed.pages, again.freelist))
	check(o.afterVacuum.rowsFound == 0, "after VACUUM no marker of a row deleted in step 2 or step 5 is left in the file")
	return o
}

func freshPath(dir, name string) string {
	path := filepath.Join(dir, name, "t.db")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		fatal("mkdir", err)
	}
	return path
}

func execOne(ctx context.Context, db *sqlitex.DB, statement string, args ...any) {
	if _, err := db.ExecContext(ctx, statement, args...); err != nil {
		fatal(statement, err)
	}
}

// largeTransaction verifies that allocateBtreePage (btree.c:6514-6822) pops
// freelist pages before extending the file on a per-allocation basis, so one
// transaction can drain the freelist and grow page_count in the same commit.
func largeTransaction(ctx context.Context, dir string) {
	fmt.Println()
	fmt.Println("== a transaction that needs more pages than the freelist holds (secure_delete=ON, WAL)")
	path := freshPath(dir, "large-transaction")
	insert := "INSERT INTO t(id, v) VALUES(?, ?)"
	var freeBefore, freeAfter, pagesBefore, pagesAfter int
	session(ctx, path, true, func(db *sqlitex.DB) {
		execOne(ctx, db, "CREATE TABLE t(id INTEGER PRIMARY KEY, v BLOB)")
		execAll(ctx, db, insert, insertRows("SECRET", 1, 40))
		execOne(ctx, db, "DELETE FROM t WHERE id <= 8")
		freeBefore, pagesBefore = pragmaInt(ctx, db, "freelist_count"), pragmaInt(ctx, db, "page_count")
		execAll(ctx, db, insert, insertRows("NEW", 101, 32))
		freeAfter, pagesAfter = pragmaInt(ctx, db, "freelist_count"), pragmaInt(ctx, db, "page_count")
	})
	fmt.Printf("40 rows, then DELETE of ids 1..8: freelist %d, page_count %d\n", freeBefore, pagesBefore)
	fmt.Printf("one transaction of 32 new rows (8 leaves' worth): freelist %d -> %d, page_count %d -> %d\n", freeBefore, freeAfter, pagesBefore, pagesAfter)
	check(freeBefore > 0 && freeAfter == 0 && pagesAfter > pagesBefore, "a transaction that needs more pages than the freelist holds takes the freelist pages first, then grows the file in the same transaction: so the rule is per page allocation, not per transaction")
}

// autoVacuumModes contrasts NONE (default, never shrinks without VACUUM), FULL
// (relocates pages and truncates during the DELETE's own commit), and
// INCREMENTAL (keeps freed pages on the freelist until PRAGMA incremental_vacuum).
func autoVacuumModes(ctx context.Context, dir string) {
	fmt.Println()
	fmt.Println("== auto_vacuum: the file shrinks on its own only when the database was created with auto_vacuum FULL or INCREMENTAL")
	type result struct{ created, afterDelete, freeAfterDelete, afterIncremental int64 }
	results := map[string]result{}
	for _, mode := range []string{sqlitex.AutoVacuumNone, sqlitex.AutoVacuumFull, sqlitex.AutoVacuumIncremental} {
		path := freshPath(dir, "auto-vacuum-"+strings.ToLower(mode))
		options := sqlitex.DefaultOptions()
		options.SingleConnection = true
		options.File.AutoVacuum = mode
		pages := func() int64 {
			if err := recipe.CheckAtRest(path); err != nil {
				fatal("at rest", err)
			}
			info, err := os.Stat(path)
			if err != nil {
				fatal("stat", err)
			}
			return info.Size() / pageSize
		}
		withDB := func(work func(db *sqlitex.DB)) {
			db, err := sqlitex.Open(ctx, path, options)
			if err != nil {
				fatal("open", err)
			}
			work(db)
			if err := db.Close(); err != nil {
				fatal("close", err)
			}
		}
		var r result
		withDB(func(db *sqlitex.DB) {
			execOne(ctx, db, "CREATE TABLE t(id INTEGER PRIMARY KEY, v BLOB)")
			execAll(ctx, db, "INSERT INTO t(id, v) VALUES(?, ?)", insertRows("SECRET", 1, 200))
		})
		r.created = pages()
		withDB(func(db *sqlitex.DB) {
			execOne(ctx, db, "DELETE FROM t WHERE id <= 150")
			r.freeAfterDelete = int64(pragmaInt(ctx, db, "freelist_count"))
		})
		r.afterDelete = pages()
		withDB(func(db *sqlitex.DB) {
			execOne(ctx, db, "PRAGMA incremental_vacuum")
		})
		r.afterIncremental = pages()
		results[mode] = r
		fmt.Printf("  auto_vacuum=%-11s 200 rows: %d pages; DELETE of 150 rows: %d pages, freelist %d; PRAGMA incremental_vacuum: %d pages\n", mode, r.created, r.afterDelete, r.freeAfterDelete, r.afterIncremental)
	}
	none, full, incremental := results[sqlitex.AutoVacuumNone], results[sqlitex.AutoVacuumFull], results[sqlitex.AutoVacuumIncremental]
	check(none.afterDelete == none.created && none.afterIncremental == none.created && none.freeAfterDelete > 0, "auto_vacuum=NONE (SQLite's default and the recipe's): the file never shrinks on its own; PRAGMA incremental_vacuum does nothing")
	check(full.afterDelete < full.created && full.freeAfterDelete == 0, "auto_vacuum=FULL: the DELETE's own commit moves pages and truncates the file, leaving no freelist")
	check(incremental.afterDelete == incremental.created && incremental.freeAfterDelete > 0 && incremental.afterIncremental < incremental.afterDelete, "auto_vacuum=INCREMENTAL: freed pages wait on the freelist until PRAGMA incremental_vacuum truncates the file")
}

func countMarkers(data []byte, r markerRange) int {
	found := 0
	for id := r.first; id <= r.last; id++ {
		if bytes.Contains(data, marker(r.prefix, id)) {
			found++
		}
	}
	return found
}

// walResidue shows that secure_delete=ON zeroes pages in the database file but
// does not erase earlier INSERT frames already sitting in -wal until the WAL
// is reset (for example by wal_checkpoint(TRUNCATE)).
func walResidue(ctx context.Context, dir string) {
	fmt.Println()
	fmt.Println("== secure_delete=ON and the -wal file (wal_autocheckpoint=0, one connection)")
	path := freshPath(dir, "wal-residue")
	options := sqlitex.DefaultOptions()
	options.SingleConnection = true
	options.Conn.SecureDelete = true
	options.Conn.WALAutocheckpoint = 0
	db, err := sqlitex.Open(ctx, path, options)
	if err != nil {
		fatal("open", err)
	}
	secrets := markerRange{"SECRET", 5001, 5040}
	inWAL := func() int {
		data, err := os.ReadFile(path + "-wal")
		if err != nil {
			fatal("read -wal", err)
		}
		return countMarkers(data, secrets)
	}
	checkpoint := func(mode string) {
		result, err := sqlitex.Checkpoint(ctx, db, "main", mode)
		if err != nil || result.Busy {
			fatal("checkpoint", fmt.Errorf("%v %+v", err, result))
		}
	}
	execOne(ctx, db, "CREATE TABLE t(id INTEGER PRIMARY KEY, v BLOB)")
	execAll(ctx, db, "INSERT INTO t(id, v) VALUES(?, ?)", insertRows(secrets.prefix, secrets.first, secrets.last-secrets.first+1))
	execOne(ctx, db, "DELETE FROM t")
	afterDelete := inWAL()
	checkpoint("PASSIVE")
	afterPassive := inWAL()
	checkpoint("TRUNCATE")
	afterTruncate := inWAL()
	if err := db.Close(); err != nil {
		fatal("close", err)
	}
	if err := recipe.CheckAtRest(path); err != nil {
		fatal("at rest", err)
	}
	image, err := os.ReadFile(path)
	if err != nil {
		fatal("read", err)
	}
	atRest := countMarkers(image, secrets)
	total := int(secrets.last - secrets.first + 1)
	fmt.Printf("%d rows inserted then deleted; deleted rows readable in the -wal: after the DELETE %d of %d, after wal_checkpoint(PASSIVE) %d, after wal_checkpoint(TRUNCATE) %d; in the database file at rest: %d\n", total, afterDelete, total, afterPassive, afterTruncate, atRest)
	check(afterDelete == total && afterPassive == total, "secure_delete zeroes the database pages, but the -wal still holds the frames the INSERT wrote: every deleted row stays readable there, even after a PASSIVE checkpoint copied the zeroed pages back")
	check(afterTruncate == 0 && atRest == 0, "only when the WAL is reset (here by wal_checkpoint(TRUNCATE)) are the deleted rows gone from the files on disk")
}

func main() {
	ctx := context.Background()
	sqlitex.MustPinCheckMemory(ctx)
	dir := sqlitex.MustWorkDir("3.2")
	fmt.Printf("INFO: work dir %s\n", displayPath(dir))
	fmt.Printf("settings: page_size=%d, journal_mode=wal, synchronous=FULL; files are read only after their connection closed\n", pageSize)
	off := run(ctx, dir, false)
	on := run(ctx, dir, true)
	fmt.Println()
	fmt.Println("== comparison")
	fmt.Printf("file sizes, secure_delete=OFF: %v\n", off.sizes)
	fmt.Printf("file sizes, secure_delete=ON:  %v\n", on.sizes)
	fmt.Printf("deleted rows readable after the delete: OFF %d, ON %d\n", off.afterDelete.rowsFound, on.afterDelete.rowsFound)
	fmt.Printf("deleted rows readable after the reuse:  OFF %d, ON %d\n", off.afterReuse.rowsFound, on.afterReuse.rowsFound)
	fmt.Printf("WAL frames written by the DELETE of %d rows: OFF %d, ON %d\n", deleteLast-deleteFirst+1, off.deleteFrames, on.deleteFrames)
	check(on.deleteFrames > off.deleteFrames, "secure_delete costs WAL frames: the zeroed pages are written, while without it freed leaf pages are not written at all")
	check(off.afterDelete.rowsFound > 0, "without secure_delete the deleted payload bytes are still in the file")
	check(off.afterDelete.byRole["freelist-leaf"] > 0, "without secure_delete freelist leaf pages keep their old content")
	check(off.afterSecondDelete.rowsFound > 0, "the second delete leaves its rows readable too without secure_delete")
	check(on.afterDelete.rowsFound == 0 && on.afterReuse.rowsFound == 0 && on.afterSecondDelete.rowsFound == 0, "with secure_delete the deleted payload bytes are nowhere in the database file at rest")
	check(fmt.Sprint(off.sizes) == fmt.Sprint(on.sizes), "secure_delete changes the bytes, not the layout: file sizes are the same at every step")
	largeTransaction(ctx, dir)
	autoVacuumModes(ctx, dir)
	walResidue(ctx, dir)
	fmt.Println()
	if len(failures) > 0 {
		fmt.Printf("DELETE AND REUSE FAILED: %d claim(s) did not hold\n", len(failures))
		for _, f := range failures {
			fmt.Printf("  - %s\n", f)
		}
		os.Exit(1)
	}
	fmt.Println("DELETE AND REUSE OK")
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

Run from the repository root:

```text
go run ./ch03-btrees-and-page-management/experiments/3.2-delete-and-reuse
```

Captured evidence:

<!-- include: evidence/3.2-delete-and-reuse.txt -->
```text
sqlite_source_id: 2026-07-24 19:02:57 bf7c7f30031888f4e796e429ab3978879485813aaca6f641c7b33e4e09459bcc (pin 3.53.4 ok)
INFO: work dir _work/3.2
settings: page_size=4096, journal_mode=wal, synchronous=FULL; files are read only after their connection closed

== secure_delete=OFF (WAL; every step is its own connection, so the file is at rest and checkpointed when it is read)
1. 400 rows of 1000-byte blobs (each blob repeats a marker like "SECRET-000150"): file 417792 bytes = 102 pages; freelist 0 page(s) (trunk [], 0 leaves); 100 table-leaf pages
2. DELETE FROM t WHERE id BETWEEN 99 AND 302 (204 rows, held on leaf pages 27-78): file 417792 bytes = 102 pages; freelist 50 page(s) (trunk [29], 49 leaves)
   pages now on the freelist: 29-78; of them, former leaves of the deleted rows: 50
   the DELETE's commit wrote 5 WAL frames
check ok: the file does not shrink on delete
check ok: pages emptied by the delete moved to the freelist
   search for the deleted markers: 199 of 204 deleted rows still readable in the file, on 52 page(s): freelist-leaf=49 freelist-trunk=1 table-leaf=2
3. 100 new rows (ids 1001..1100): file 417792 bytes = 102 pages; freelist 25 page(s) (trunk [29], 24 leaves)
   leaf pages holding the new rows: 30,55-78; of them taken from the freelist: 25
check ok: the new rows fit in the file: its size is unchanged
check ok: every leaf page of the new rows is a former freelist page
   search for the deleted markers: 104 of 204 deleted rows still readable in the file, on 27 page(s): freelist-leaf=24 freelist-trunk=1 table-leaf=2
4. 30 more transactions of 4 rows each: file 438272 bytes = 107 pages; freelist 0 page(s) (trunk [], 0 leaves)
   batch 1: freelist 25->24, page_count 102->102
   batch 25: freelist 1->0, page_count 102->102
   batch 26: freelist 0->0, page_count 102->103
   batch 27: freelist 0->0, page_count 103->104
   ... (2 more batches that grew the file)
   batch 30: freelist 0->0, page_count 106->107
check ok: page_count grows only once the freelist is used up: every transaction that added pages ended with an empty freelist
check ok: once the freelist is used up, the file grows
5. DELETE FROM t WHERE id >= 1001: file 438272 bytes = 107 pages; freelist 55 page(s) (trunk [102], 54 leaves)
check ok: the file still does not shrink
   search for the NEW- and GROW- markers just deleted: 217 of 220 deleted rows still readable in the file, on 55 page(s): freelist-leaf=54 freelist-trunk=1
6. VACUUM: file 208896 bytes = 51 pages; freelist 0 page(s) (trunk [], 0 leaves)
   search for the deleted markers: 0 of 424 deleted rows still readable in the file, on 0 page(s)
check ok: VACUUM is what returns space: 107 pages -> 51, at least the 55 freelist pages are gone
check ok: after VACUUM no marker of a row deleted in step 2 or step 5 is left in the file

== secure_delete=ON (WAL; every step is its own connection, so the file is at rest and checkpointed when it is read)
1. 400 rows of 1000-byte blobs (each blob repeats a marker like "SECRET-000150"): file 417792 bytes = 102 pages; freelist 0 page(s) (trunk [], 0 leaves); 100 table-leaf pages
2. DELETE FROM t WHERE id BETWEEN 99 AND 302 (204 rows, held on leaf pages 27-78): file 417792 bytes = 102 pages; freelist 50 page(s) (trunk [29], 49 leaves)
   pages now on the freelist: 29-78; of them, former leaves of the deleted rows: 50
   the DELETE's commit wrote 54 WAL frames
check ok: the file does not shrink on delete
check ok: pages emptied by the delete moved to the freelist
   search for the deleted markers: 0 of 204 deleted rows still readable in the file, on 0 page(s)
3. 100 new rows (ids 1001..1100): file 417792 bytes = 102 pages; freelist 25 page(s) (trunk [29], 24 leaves)
   leaf pages holding the new rows: 30,55-78; of them taken from the freelist: 25
check ok: the new rows fit in the file: its size is unchanged
check ok: every leaf page of the new rows is a former freelist page
   search for the deleted markers: 0 of 204 deleted rows still readable in the file, on 0 page(s)
4. 30 more transactions of 4 rows each: file 438272 bytes = 107 pages; freelist 0 page(s) (trunk [], 0 leaves)
   batch 1: freelist 25->24, page_count 102->102
   batch 25: freelist 1->0, page_count 102->102
   batch 26: freelist 0->0, page_count 102->103
   batch 27: freelist 0->0, page_count 103->104
   ... (2 more batches that grew the file)
   batch 30: freelist 0->0, page_count 106->107
check ok: page_count grows only once the freelist is used up: every transaction that added pages ended with an empty freelist
check ok: once the freelist is used up, the file grows
5. DELETE FROM t WHERE id >= 1001: file 438272 bytes = 107 pages; freelist 55 page(s) (trunk [102], 54 leaves)
check ok: the file still does not shrink
   search for the NEW- and GROW- markers just deleted: 0 of 220 deleted rows still readable in the file, on 0 page(s)
6. VACUUM: file 208896 bytes = 51 pages; freelist 0 page(s) (trunk [], 0 leaves)
   search for the deleted markers: 0 of 424 deleted rows still readable in the file, on 0 page(s)
check ok: VACUUM is what returns space: 107 pages -> 51, at least the 55 freelist pages are gone
check ok: after VACUUM no marker of a row deleted in step 2 or step 5 is left in the file

== comparison
file sizes, secure_delete=OFF: [417792 417792 417792 438272 438272 208896]
file sizes, secure_delete=ON:  [417792 417792 417792 438272 438272 208896]
deleted rows readable after the delete: OFF 199, ON 0
deleted rows readable after the reuse:  OFF 104, ON 0
WAL frames written by the DELETE of 204 rows: OFF 5, ON 54
check ok: secure_delete costs WAL frames: the zeroed pages are written, while without it freed leaf pages are not written at all
check ok: without secure_delete the deleted payload bytes are still in the file
check ok: without secure_delete freelist leaf pages keep their old content
check ok: the second delete leaves its rows readable too without secure_delete
check ok: with secure_delete the deleted payload bytes are nowhere in the database file at rest
check ok: secure_delete changes the bytes, not the layout: file sizes are the same at every step

== a transaction that needs more pages than the freelist holds (secure_delete=ON, WAL)
40 rows, then DELETE of ids 1..8: freelist 2, page_count 12
one transaction of 32 new rows (8 leaves' worth): freelist 2 -> 0, page_count 12 -> 18
check ok: a transaction that needs more pages than the freelist holds takes the freelist pages first, then grows the file in the same transaction: so the rule is per page allocation, not per transaction

== auto_vacuum: the file shrinks on its own only when the database was created with auto_vacuum FULL or INCREMENTAL
  auto_vacuum=NONE        200 rows: 52 pages; DELETE of 150 rows: 52 pages, freelist 37; PRAGMA incremental_vacuum: 52 pages
  auto_vacuum=FULL        200 rows: 53 pages; DELETE of 150 rows: 16 pages, freelist 0; PRAGMA incremental_vacuum: 16 pages
  auto_vacuum=INCREMENTAL 200 rows: 53 pages; DELETE of 150 rows: 53 pages, freelist 37; PRAGMA incremental_vacuum: 16 pages
check ok: auto_vacuum=NONE (SQLite's default and the recipe's): the file never shrinks on its own; PRAGMA incremental_vacuum does nothing
check ok: auto_vacuum=FULL: the DELETE's own commit moves pages and truncates the file, leaving no freelist
check ok: auto_vacuum=INCREMENTAL: freed pages wait on the freelist until PRAGMA incremental_vacuum truncates the file

== secure_delete=ON and the -wal file (wal_autocheckpoint=0, one connection)
40 rows inserted then deleted; deleted rows readable in the -wal: after the DELETE 40 of 40, after wal_checkpoint(PASSIVE) 40, after wal_checkpoint(TRUNCATE) 0; in the database file at rest: 0
check ok: secure_delete zeroes the database pages, but the -wal still holds the frames the INSERT wrote: every deleted row stays readable there, even after a PASSIVE checkpoint copied the zeroed pages back
check ok: only when the WAL is reset (here by wal_checkpoint(TRUNCATE)) are the deleted rows gone from the files on disk

DELETE AND REUSE OK
```

**Reading the evidence:**

- **Initial state and `DELETE` under `secure_delete=OFF` (lines 5–12).** Inserting `400` rows of 1,000-byte blobs (each repeating a marker such as `SECRET-000150`) produces a `417,792`-byte (`102`-page) file: page 1, root page 2, and `100` table leaves of 4 rows each. Deleting ids `99..302` (`204` rows, originally spanning leaves `27..78`) leaves the file size unchanged at `417,792` bytes (`102` pages) and moves `50` pages (`29..78`, all former leaves of the deleted rows) onto the freelist: trunk `[29]` plus `49` freelist leaves. Scanning the closed file finds **`199` of `204` deleted rows still readable** across `52` pages: `49` freelist leaves, `1` freelist trunk (whose bytes after offset `8 + 49×4` were never overwritten), and `2` live table leaves (`27` and `78`, which each kept 2 surviving rows plus freeblocks holding the deleted rows' bytes). That `DELETE` commit wrote only **`5` WAL frames** (page 1, root 2, trunk 29, and the two partial leaves 27 and 78): `freePage2` marked all 49 freelist leaves `PGHDR_DONT_WRITE` via `sqlite3PagerDontWrite`.
- **Freelist reuse, growth, second delete, and `VACUUM` (lines 13–33).** Inserting `100` new rows (`ids 1001..1100`) keeps the file at `417,792` bytes (`102` pages) and drops the freelist from `50` to `25`: all `25` new leaf pages (`30, 55-78`) came straight from the freelist, while `104` of the `204` deleted rows remain readable on `27` pages (`24` freelist leaves, `1` trunk, `2` live leaves). Next, `30` batches of 4 rows drain the remaining `25` freelist pages one per batch (`batch 1`: freelist `25->24`, `page_count 102->102`; `batch 25`: `1->0`, `102->102`), and only `batches 26..30` grow the file (`102->107` pages, `438,272` bytes). Deleting `id >= 1001` (`220` rows) leaves `107` pages with `55` on the freelist (`trunk [102]`, `54` leaves) and `217` of `220` `NEW-`/`GROW-` markers still readable across all `55` freelist pages. Finally, `VACUUM` shrinks the file from `107` pages (`438,272` bytes) to `51` pages (`208,896` bytes: 49 leaves for 196 surviving rows + root + page 1), freelist `0`, and `0` of all `424` deleted rows readable anywhere in the file.
- **`secure_delete=ON` and comparison (lines 35–76).** Every file size across all six steps is identical to `OFF` (`[417792 417792 417792 438272 438272 208896]`): `secure_delete` changes bytes, not layout. Zero deleted rows are readable in the database file at rest after step 2 (`0 of 204`), step 3 (`0 of 204`), step 5 (`0 of 220`), or step 6 (`0 of 424`). The cost shows up directly in the WAL: the step-2 `DELETE` wrote **`54` WAL frames** instead of `5` (`49` extra frames for the zeroed freelist leaves).
- **Large transaction exceeding the freelist (lines 78–81; correction 10).** Starting with `freelist=2`, `page_count=12`, inserting `32` rows (8 leaves' worth) in a single transaction ends with `freelist=0` and `page_count=18`. `allocateBtreePage` pops freelist pages first on a per-allocation basis, then grows the file in the same commit once the freelist hits zero.
- **`auto_vacuum` modes (lines 83–89; correction 8).** Deleting `150` of `200` rows: `NONE` stays `52 -> 52` pages (`freelist 37`), and `PRAGMA incremental_vacuum` does nothing (`52`); `FULL` starts one page larger for its pointer-map page (`53`) and shrinks immediately on the `DELETE`'s commit to `16` pages (`freelist 0`); `INCREMENTAL` stays `53 -> 53` (`freelist 37`) until `PRAGMA incremental_vacuum` truncates it to `16` pages.
- **`secure_delete=ON` and the `-wal` file (lines 91–94; correction 9).** Inserting then deleting `40` secret rows with `secure_delete=ON` and `wal_autocheckpoint=0` leaves **`40` of `40` deleted rows readable inside `-wal`** after the `DELETE` commits—and still `40` of `40` after `PRAGMA wal_checkpoint(PASSIVE)` (which copies the zeroed pages back to `.db` without erasing earlier `INSERT` frames in `-wal`). Only after `PRAGMA wal_checkpoint(TRUNCATE)` resets `-wal` does the count drop to `0` on disk (`0` in `-wal` and `0` in `.db` at rest).

---

## 6. Evidence and failure cases

### 6.1 Experiment 3.3 — Same rows, different bytes, rung R2 (`3.3-same-rows-different-bytes`)

- **Claim (corrected and narrowed):** Physical layout records history, not just logical content. Different insert orders or update/delete histories over the same final rows produce different database files (`E1` and `E2` differ), while append-only batching in WAL mode produces byte-identical files (commit boundaries show up only in rollback mode's change counter, `E2`-equal). Every build shares one `E3` logical hash, one deterministic ordered logical capture (`capture/r0.sql`), and one `VACUUM INTO` SHA-256 (`E1` between canonical forms) alongside the replayed capture—provided the databases share the same schema creation order and schema cookie (a `sqlite3_backup` copy has schema cookie `1` instead of `4`, so its canonical file differs at byte `43`). Our `E3` hash closes six blind spots where `dbhash` (5 of 6) or `sqldiff` (1 of 6) fail.
- **Evidence form:** `EXACT`
- **Capture / rebuild / canonical form (rung R2):** capture = ordered SQL script emitted by `equal.WalkLogical` with `CREATE` statements in `main.sqlite_schema` rowid order (`capture/r0.sql`); rebuild = replaying that script into a fresh file; canonical form = `VACUUM INTO`.

<!-- include: experiments/3.3-same-rows-different-bytes/main.go -->
```go
// Experiment 3.3 implements Rebuild Ladder rung R2 (canonical logical capture
// and replay) and tests E1, E2, and E3 equality across different build
// histories, a sqlite3_backup copy, dbhash blind spots, and VACUUM INTO's
// canonical-form boundaries.
package main

import (
	"context"
	"database/sql"
	"encoding/hex"
	"fmt"
	"math"
	"math/rand/v2"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/nickstrad/sqlite-internals/internal/equal"
	"github.com/nickstrad/sqlite-internals/internal/recipe"
	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
)

const (
	pageSize    = 4096
	shuffleSeed = 3
)

var failures []string

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

func must[T any](value T, err error) func(string) T {
	return func(what string) T {
		if err != nil {
			fatal(what, err)
		}
		return value
	}
}

type row struct {
	table  equal.TableInfo
	values []any
}

type content struct {
	schema      []string
	rows        []row
	userVersion int64
}

func readContent(ctx context.Context, path string) content {
	db := must(equal.OpenExisting(ctx, path))("open " + filepath.Base(path))
	defer db.Close()
	var c content
	rows := must(db.QueryContext(ctx, "SELECT sql FROM sqlite_schema WHERE sql IS NOT NULL AND name NOT LIKE 'sqlite\\_%' ESCAPE '\\' ORDER BY rowid"))("schema")
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			fatal("schema", err)
		}
		c.schema = append(c.schema, s)
	}
	if err := rows.Err(); err != nil {
		fatal("schema", err)
	}
	if err := rows.Close(); err != nil {
		fatal("schema", err)
	}
	err := equal.WalkLogical(ctx, db, equal.LogicalVisitor{Row: func(t equal.TableInfo, values []any) error {
		if strings.HasPrefix(t.Name, "sqlite_") {
			return fmt.Errorf("table %s: this lab copies user tables only", t.Name)
		}
		c.rows = append(c.rows, row{table: t, values: values})
		return nil
	}})
	if err != nil {
		fatal("read rows", err)
	}
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&c.userVersion); err != nil {
		fatal("user_version", err)
	}
	return c
}

func insertSQL(t equal.TableInfo) string {
	width := len(t.Columns)
	if !t.WithoutRowid {
		width++
	}
	marks := make([]string, width)
	for i := range marks {
		marks[i] = "?"
	}
	return insertStatement(t, marks)
}

func rowidName(name string) string {
	switch strings.ToLower(name) {
	case "rowid", "_rowid_", "oid":
		return name
	}
	return equal.QuoteIdentifier(name)
}

// insertStatement builds an INSERT statement directly from the table's column
// list and value expressions (either "?" placeholders or SQL literals), avoiding
// string replacement that would corrupt identifiers containing "?".
func insertStatement(t equal.TableInfo, values []string) string {
	var names []string
	if !t.WithoutRowid {
		names = append(names, rowidName(t.RowidColumn))
	}
	for _, c := range t.Columns {
		names = append(names, equal.QuoteIdentifier(c))
	}
	return fmt.Sprintf("INSERT INTO %s(%s) VALUES(%s)", equal.QuoteIdentifier(t.Name), strings.Join(names, ", "), strings.Join(values, ", "))
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func execOrDie(ctx context.Context, e execer, query string, args ...any) {
	if _, err := e.ExecContext(ctx, query, args...); err != nil {
		fatal(query, err)
	}
}

func openBuild(ctx context.Context, path, mode string) *sqlitex.DB {
	if err := recipe.RemoveDatabase(path); err != nil {
		fatal("remove", err)
	}
	options := recipe.Default().WithJournalMode(mode).SQLiteOptions()
	options.Conn.Synchronous = sqlitex.SyncOff
	return must(sqlitex.Open(ctx, path, options))("open " + filepath.Base(path))
}

func buildOneTransaction(ctx context.Context, path, mode string, c content) {
	db := openBuild(ctx, path, mode)
	tx := must(db.BeginTx(ctx, nil))("begin")
	for _, s := range c.schema {
		execOrDie(ctx, tx, s)
	}
	for _, r := range c.rows {
		execOrDie(ctx, tx, insertSQL(r.table), r.values...)
	}
	execOrDie(ctx, tx, fmt.Sprintf("PRAGMA user_version = %d", c.userVersion))
	if err := tx.Commit(); err != nil {
		fatal("commit", err)
	}
	if err := db.Close(); err != nil {
		fatal("close", err)
	}
}

func buildPerStatement(ctx context.Context, path, mode string, c content, order []int) {
	db := openBuild(ctx, path, mode)
	for _, s := range c.schema {
		execOrDie(ctx, db, s)
	}
	for _, i := range order {
		r := c.rows[i]
		execOrDie(ctx, db, insertSQL(r.table), r.values...)
	}
	execOrDie(ctx, db, fmt.Sprintf("PRAGMA user_version = %d", c.userVersion))
	if err := db.Close(); err != nil {
		fatal("close", err)
	}
}

// sqlLiteral renders a scanned SQLite storage-class value as a SQL literal
// that round-trips with the exact same storage class. Embedded NUL bytes in
// TEXT are split with ||char(0)|| because SQLite's parser stops at raw NULs.
func sqlLiteral(value any) string {
	switch v := value.(type) {
	case nil:
		return "NULL"
	case int64:
		return strconv.FormatInt(v, 10)
	case float64:
		switch {
		case math.IsInf(v, 1):
			return "1e999"
		case math.IsInf(v, -1):
			return "-1e999"
		}
		text := strconv.FormatFloat(v, 'g', -1, 64)
		if !strings.ContainsAny(text, ".eEn") {
			text += ".0"
		}
		return text
	case string:
		parts := strings.Split(v, "\x00")
		for i, part := range parts {
			parts[i] = "'" + strings.ReplaceAll(part, "'", "''") + "'"
		}
		return strings.Join(parts, "||char(0)||")
	case []byte:
		return "X'" + strings.ToUpper(hex.EncodeToString(v)) + "'"
	}
	fatal("literal", fmt.Errorf("unsupported value %T", value))
	return ""
}

// dump emits rung R2's deterministic SQL capture using equal.WalkLogical.
// CREATE statements follow main.sqlite_schema rowid order (creation order) so
// a replayed database matches its source's VACUUM INTO bytes (vacuum.c:143-427
// copies tables in sqlite_schema order).
func dump(ctx context.Context, path string) string {
	db := must(equal.OpenExisting(ctx, path))("open " + filepath.Base(path))
	defer db.Close()
	conn := must(db.Conn(ctx))("conn")
	defer conn.Close()
	execOrDie(ctx, conn, "BEGIN")
	defer conn.ExecContext(ctx, "COMMIT")
	var tables, indexes, others []string
	var body strings.Builder
	var sequence []string
	pragmas := map[string]string{}
	schema := must(conn.QueryContext(ctx, "SELECT type, name, sql FROM main.sqlite_schema WHERE sql IS NOT NULL ORDER BY rowid"))("schema")
	for schema.Next() {
		var kind, name, statement string
		if err := schema.Scan(&kind, &name, &statement); err != nil {
			fatal("schema", err)
		}
		if strings.HasPrefix(name, "sqlite_") {
			continue
		}
		switch kind {
		case "table":
			tables = append(tables, statement+";")
		case "index":
			indexes = append(indexes, statement+";")
		default:
			others = append(others, statement+";")
		}
	}
	if err := schema.Err(); err != nil {
		fatal("schema", err)
	}
	if err := schema.Close(); err != nil {
		fatal("schema", err)
	}
	err := equal.WalkLogical(ctx, conn, equal.LogicalVisitor{
		Row: func(t equal.TableInfo, values []any) error {
			literals := make([]string, len(values))
			for i, v := range values {
				literals[i] = sqlLiteral(v)
			}
			line := insertStatement(t, literals) + ";\n"
			switch {
			case t.Name == "sqlite_sequence":
				sequence = append(sequence, line)
			case strings.HasPrefix(t.Name, "sqlite_"):
				return fmt.Errorf("table %s: this capture handles sqlite_sequence only", t.Name)
			default:
				body.WriteString(line)
			}
			return nil
		},
		Pragma: func(name, value string) error {
			pragmas[name] = value
			return nil
		},
	})
	if err != nil {
		fatal("dump", err)
	}
	var out strings.Builder
	fmt.Fprintf(&out, "PRAGMA page_size=%s;\nPRAGMA auto_vacuum=%s;\nPRAGMA encoding='%s';\n", pragmas["page_size"], strings.ToUpper(pragmas["auto_vacuum"]), pragmas["encoding"])
	if pragmas["journal_mode"] == "wal" {
		out.WriteString("PRAGMA journal_mode=WAL;\n")
	}
	out.WriteString("BEGIN;\n")
	for _, s := range tables {
		out.WriteString(s + "\n")
	}
	out.WriteString(body.String())
	if len(sequence) > 0 {
		out.WriteString("DELETE FROM sqlite_sequence;\n")
		for _, s := range sequence {
			out.WriteString(s)
		}
	}
	for _, s := range append(indexes, others...) {
		out.WriteString(s + "\n")
	}
	fmt.Fprintf(&out, "PRAGMA user_version=%s;\nPRAGMA application_id=%s;\nCOMMIT;\n", pragmas["user_version"], pragmas["application_id"])
	return out.String()
}

func replay(ctx context.Context, script, path string) {
	if err := recipe.RemoveDatabase(path); err != nil {
		fatal("remove", err)
	}
	uri := must(sqlitex.FileURI(path, url.Values{}))("uri")
	db := must(sql.Open("sqlite", uri))("open")
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, script); err != nil {
		fatal("replay", err)
	}
	if err := db.Close(); err != nil {
		fatal("close", err)
	}
}

func vacuumInto(ctx context.Context, source, target string) {
	if err := recipe.RemoveDatabase(target); err != nil {
		fatal("remove", err)
	}
	db := must(equal.OpenExisting(ctx, source))("open " + filepath.Base(source))
	if _, err := db.ExecContext(ctx, "VACUUM INTO ?", target); err != nil {
		fatal("vacuum into", err)
	}
	if err := db.Close(); err != nil {
		fatal("close", err)
	}
}

func backupCopy(ctx context.Context, source, target string) {
	if err := recipe.RemoveDatabase(target); err != nil {
		fatal("remove", err)
	}
	db := must(equal.OpenExisting(ctx, source))("open " + filepath.Base(source))
	conn := must(db.Conn(ctx))("conn")
	if _, err := sqlitex.Backup(ctx, conn, target, -1, nil); err != nil {
		fatal("backup", err)
	}
	conn.Close()
	if err := db.Close(); err != nil {
		fatal("close", err)
	}
}

func short(h string) string {
	if len(h) > 16 {
		return h[:16]
	}
	return h
}

type build struct {
	name      string
	how       string
	path      string
	sha       string
	logical   equal.Logical
	dumpSHA   string
	dbhash    string
	canonical string
	canonSHA  string
}

func atRest(path string) {
	if err := recipe.CheckAtRest(path); err != nil {
		fatal("at rest", err)
	}
}

func locateDBHash() string {
	tool, err := equal.LocateDBHash()
	if err != nil {
		fmt.Printf("NO-DBHASH: %v; the dbhash column is skipped (build it with: go run ./cmd/course build-tools)\n", err)
		return ""
	}
	fmt.Printf("INFO: dbhash %s\n", displayPath(tool))
	fmt.Println("dbhash: found (built from the pinned tree; it prints a SHA1 of the schema and every row it reads)")
	return tool
}

func dbhashOf(ctx context.Context, tool, path string) string {
	if tool == "" {
		return "-"
	}
	h := must(equal.RunDBHash(ctx, tool, path))("dbhash " + filepath.Base(path))
	atRest(path)
	return h
}

func distinct(values []string) int {
	seen := map[string]bool{}
	for _, v := range values {
		seen[v] = true
	}
	return len(seen)
}

func partA(ctx context.Context, dir, tool string) content {
	fmt.Println()
	fmt.Println("== A. the same final rows built four ways, then captured as an ordered logical dump and replayed (rung R2)")
	r0 := filepath.Join(dir, "builds", "r0.db")
	if err := os.MkdirAll(filepath.Dir(r0), 0o755); err != nil {
		fatal("mkdir", err)
	}
	r := recipe.Default()
	if err := r.Build(ctx, r0, r.Steps); err != nil {
		fatal("build R0", err)
	}
	c := readContent(ctx, r0)
	perTable := map[string]int{}
	for _, row := range c.rows {
		perTable[row.table.Name]++
	}
	fmt.Printf("content of R0 (recipe seed 1, 30 steps of inserts, updates and deletes): %d schema statements, rows accounts=%d documents=%d events=%d, user_version %d\n", len(c.schema), perTable["accounts"], perTable["documents"], perTable["events"], c.userVersion)
	builds := []*build{{name: "r0", how: "the recipe: 31 transactions with updates and deletes, index created at step 5"}}
	builds[0].path = r0
	oneTx := filepath.Join(dir, "builds", "one-transaction.db")
	buildOneTransaction(ctx, oneTx, "wal", c)
	builds = append(builds, &build{name: "one-transaction", how: "schema and every row in one transaction, tables in order, ids ascending", path: oneTx})
	perStatement := filepath.Join(dir, "builds", "per-statement.db")
	inOrder := make([]int, len(c.rows))
	for i := range inOrder {
		inOrder[i] = i
	}
	buildPerStatement(ctx, perStatement, "wal", c, inOrder)
	builds = append(builds, &build{name: "per-statement", how: "the same order, one transaction per statement", path: perStatement})
	shuffled := filepath.Join(dir, "builds", "shuffled.db")
	order := append([]int(nil), inOrder...)
	rng := rand.New(rand.NewPCG(shuffleSeed, 0))
	rng.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
	buildPerStatement(ctx, shuffled, "wal", c, order)
	builds = append(builds, &build{name: "shuffled", how: fmt.Sprintf("one transaction per row, rows of all tables in a shuffled order (PCG seed %d)", shuffleSeed), path: shuffled})

	captureDir := filepath.Join(dir, "capture")
	if err := os.MkdirAll(captureDir, 0o755); err != nil {
		fatal("mkdir", err)
	}
	script := dump(ctx, r0)
	dumpPath := filepath.Join(captureDir, "r0.sql")
	if err := os.WriteFile(dumpPath, []byte(script), 0o644); err != nil {
		fatal("write dump", err)
	}
	lines := strings.Split(strings.TrimSuffix(script, "\n"), "\n")
	fmt.Printf("capture: %s, %d bytes, %d lines, sha256 %s\n", "capture/r0.sql", len(script), len(lines), equal.SHA256Hex([]byte(script)))
	for i, line := range lines {
		if i < 9 || i >= len(lines)-4 {
			if len(line) > 140 {
				line = line[:140] + "..."
			}
			fmt.Printf("  %s\n", line)
		} else if i == 9 {
			fmt.Printf("  ... (%d INSERT lines)\n", len(lines)-13)
		}
	}
	replayed := filepath.Join(dir, "builds", "replayed.db")
	replay(ctx, string(must(os.ReadFile(dumpPath))("read dump")), replayed)
	builds = append(builds, &build{name: "replayed", how: "the capture file replayed into an empty file (indexes created after the rows)", path: replayed})
	backup := filepath.Join(dir, "builds", "backup.db")
	backupCopy(ctx, r0, backup)
	builds = append(builds, &build{name: "backup", how: "sqlite3_backup of R0 (a page copy, for the E2 row)", path: backup})

	canonicalDir := filepath.Join(dir, "canonical")
	if err := os.MkdirAll(canonicalDir, 0o755); err != nil {
		fatal("mkdir", err)
	}
	for _, b := range builds {
		atRest(b.path)
		b.sha = must(equal.FileSHA256(b.path))("hash")
		b.logical = must(equal.LogicalHashFile(ctx, b.path))("logical hash " + b.name)
		atRest(b.path)
		b.dumpSHA = equal.SHA256Hex([]byte(dump(ctx, b.path)))
		b.dbhash = dbhashOf(ctx, tool, b.path)
		b.canonical = filepath.Join(canonicalDir, b.name+".db")
		vacuumInto(ctx, b.path, b.canonical)
		atRest(b.path)
		atRest(b.canonical)
		b.canonSHA = must(equal.FileSHA256(b.canonical))("hash")
		if again := must(equal.FileSHA256(b.path))("hash"); again != b.sha {
			fatal("hash", fmt.Errorf("%s changed while it was hashed", b.name))
		}
	}
	fmt.Println("builds:")
	for _, b := range builds {
		info := must(os.Stat(b.path))("stat")
		fmt.Printf("  %-16s %s; %d bytes\n", b.name, b.how, info.Size())
	}
	fmt.Println("each build against R0 (E1 = file sha256, E2 = page diff ignoring header bytes 24-27, 40-43, 92-99, E3 = logical hash + integrity_check + foreign_key_check):")
	fmt.Printf("  %-16s %-16s %-44s %-44s %s\n", "build", "sha256", "E1 vs r0", "E2 vs r0", "E3 vs r0")
	var shas, logicals, dumps, hashes, canon []string
	for _, b := range builds {
		e1 := must(equal.CompareFiles(r0, b.path, pageSize))("E1")
		e2 := must(equal.ComparePageFiles(r0, b.path, pageSize))("E2")
		e3 := equal.CompareLogical(builds[0].logical, b.logical)
		fmt.Printf("  %-16s %-16s %-44s %-44s %s\n", b.name, short(b.sha), verdict(e1), verdict(e2), logicalVerdict(e3))
		if b.name != "backup" {
			shas = append(shas, b.sha)
		}
		logicals = append(logicals, b.logical.SHA256)
		dumps = append(dumps, b.dumpSHA)
		hashes = append(hashes, b.dbhash)
		canon = append(canon, b.canonSHA)
		if b.name == "backup" {
			check(!e1.Equal && e2.Equal, fmt.Sprintf("the backup copy differs from R0 in bytes (%s) but is equal at E2: only declared header bytes differ", e2))
		}
	}
	fmt.Println("the same builds, logically and canonically:")
	fmt.Printf("  %-16s %-16s %-16s %-40s %s\n", "build", "logical sha256", "dump sha256", "dbhash", "VACUUM INTO sha256")
	for _, b := range builds {
		fmt.Printf("  %-16s %-16s %-16s %-40s %s\n", b.name, short(b.logical.SHA256), short(b.dumpSHA), b.dbhash, short(b.canonSHA))
	}
	fmt.Printf("logical hash of R0: %s\n", builds[0].logical.Summary())
	fmt.Printf("distinct file sha256 among r0, one-transaction, per-statement, shuffled, replayed: %d\n", distinct(shas))
	check(builds[1].sha == builds[2].sha, "in WAL mode one transaction and one transaction per statement give byte-identical files: commit boundaries alone leave no trace when rows are only appended")
	check(builds[3].sha != builds[1].sha && builds[0].sha != builds[1].sha && builds[0].sha != builds[3].sha, "insert order (shuffled) and history (R0's updates and deletes) each give a different file")
	check(distinct(logicals) == 1, "every build has the same logical hash")
	allE3 := true
	for _, b := range builds {
		if !equal.CompareLogical(builds[0].logical, b.logical).Equal {
			allE3 = false
		}
	}
	check(allE3, "every build is equal to R0 at E3 (logical hash, integrity_check ok, foreign_key_check unchanged)")
	check(distinct(dumps) == 1, "the ordered logical capture is itself identical for every build: it depends on content and the schema's creation order, not on insert order, batching or updates")
	check(distinct(canon[:5]) == 1, "after VACUUM INTO every build, the replayed copy included, has the same sha256 (E1 between canonical forms)")
	// sqlite3_backup_step sets the destination schema cookie to old+1
	// (backup.c:448; 0+1=1 on a fresh file), and sqlite3RunVacuum copies the
	// source schema cookie +1 (vacuum.c:143-427), leaving byte 43 differing.
	backupCanon := builds[5].canonical
	ce1 := must(equal.CompareFiles(builds[0].canonical, backupCanon, pageSize))("E1")
	ce2 := must(equal.ComparePageFiles(builds[0].canonical, backupCanon, pageSize))("E2")
	fmt.Printf("canonical form of the backup vs of R0: E1 %s; E2 %s; schema cookies %s\n", verdict(ce1), verdict(ce2), cookies(builds[0].canonical, backupCanon))
	check(!ce1.Equal && ce2.Equal && fmt.Sprint(ce2.IgnoredDiffering) == "[43]", "except the backup copy: the backup API sets the copy's schema cookie to (old destination cookie + 1) = 1 (backup.c:448), VACUUM INTO carries the cookie + 1, so its canonical form differs at byte 43 only")
	if tool != "" {
		check(distinct(hashes) == 1, "dbhash from the pinned tree also calls every build equal: it agrees with our logical hash on equality")
	}
	fmt.Printf("canonical form: %s\n", canonicalHeader(builds[0].canonical))
	ok := true
	for _, b := range builds {
		l := must(equal.LogicalHashFile(ctx, b.canonical))("logical hash")
		if l.SHA256 == b.logical.SHA256 {
			ok = false
		}
		withoutMode := sameExceptJournalMode(b.logical, l)
		if !withoutMode {
			ok = false
		}
	}
	check(ok, "VACUUM INTO keeps every row, the schema and user_version/application_id, but writes a rollback-mode file: its logical hash differs from the source only in journal_mode")
	return c
}

func rollbackPair(ctx context.Context, dir string, c content) {
	fmt.Println()
	fmt.Println("== A2. the same two batchings in rollback mode (journal_mode=DELETE)")
	one := filepath.Join(dir, "builds", "rollback-one-transaction.db")
	per := filepath.Join(dir, "builds", "rollback-per-statement.db")
	buildOneTransaction(ctx, one, "delete", c)
	order := make([]int, len(c.rows))
	for i := range order {
		order[i] = i
	}
	buildPerStatement(ctx, per, "delete", c, order)
	atRest(one)
	atRest(per)
	e1 := must(equal.CompareFiles(one, per, pageSize))("E1")
	e2 := must(equal.ComparePageFiles(one, per, pageSize))("E2")
	ia, ib := must(os.ReadFile(one))("read"), must(os.ReadFile(per))("read")
	fmt.Printf("one transaction vs one per statement: E1 %s; E2 %s; change counters %d and %d\n", verdict(e1), verdict(e2), be32(ia[24:28]), be32(ib[24:28]))
	check(!e1.Equal && e2.Equal, "in rollback mode the batching shows, but only in the change counter (bytes 24-27 and 92-95 count commits): E2 equal")
}

func sameExceptJournalMode(a, b equal.Logical) bool {
	diff := equal.CompareLogical(a, b)
	return len(diff.Differences) == 1 && strings.HasPrefix(diff.Differences[0], "pragma journal_mode")
}

func canonicalHeader(path string) string {
	image := must(os.ReadFile(path))("read")
	return fmt.Sprintf("%d bytes, header bytes 18-19 = %d,%d (1 = rollback journal), change counter %d, schema cookie %d",
		len(image), image[18], image[19], be32(image[24:28]), be32(image[40:44]))
}

func be32(b []byte) uint32 {
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}

func verdict(r equal.Result) string {
	switch {
	case r.Equal && len(r.IgnoredDiffering) > 0:
		return fmt.Sprintf("equal except header bytes %v", r.IgnoredDiffering)
	case r.Equal:
		return "equal"
	case r.SizeA != r.SizeB:
		return fmt.Sprintf("differ (%d pages differ, sizes differ)", r.DifferingPages)
	}
	return fmt.Sprintf("differ (%d of %d pages)", r.DifferingPages, r.SizeA/int64(r.PageSize))
}

func logicalVerdict(r equal.LogicalResult) string {
	if r.Equal {
		return "equal"
	}
	return "differ: " + strings.Join(r.Differences, "; ")
}

func newDB(ctx context.Context, path string, statements ...string) string {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		fatal("mkdir", err)
	}
	if err := recipe.RemoveDatabase(path); err != nil {
		fatal("remove", err)
	}
	options := sqlitex.DefaultOptions()
	options.SingleConnection = true
	options.Conn.SecureDelete = true
	db := must(sqlitex.Open(ctx, path, options))("open")
	for _, s := range statements {
		execOrDie(ctx, db, s)
	}
	if err := db.Close(); err != nil {
		fatal("close", err)
	}
	atRest(path)
	return path
}

type pair struct {
	name   string
	a, b   []string
	ours   string
	dbhash bool
}

// partB exercises six logical-difference pairs where weaker tools fail,
// including dbhash's unframed value concatenation and missing table boundaries
// in tool/dbhash.c:281-360 and tool/dbhash.c:379-491.
func partB(ctx context.Context, dir, tool string) {
	fmt.Println()
	fmt.Println("== B. pairs that differ in content where a weaker logical hash says equal")
	pairs := []pair{
		{"implicit rowids (1,2 vs 5,9)", []string{"CREATE TABLE t(v TEXT)", "INSERT INTO t(rowid, v) VALUES(1, 'x'), (2, 'y')"}, []string{"CREATE TABLE t(v TEXT)", "INSERT INTO t(rowid, v) VALUES(5, 'x'), (9, 'y')"}, "table t differs", true},
		{"sqlite_sequence (1 vs 2)", []string{"CREATE TABLE t(id INTEGER PRIMARY KEY AUTOINCREMENT, v TEXT)", "INSERT INTO t(v) VALUES('x')"}, []string{"CREATE TABLE t(id INTEGER PRIMARY KEY AUTOINCREMENT, v TEXT)", "INSERT INTO t(v) VALUES('x')", "INSERT INTO t(v) VALUES('y')", "DELETE FROM t WHERE id = 2"}, "table sqlite_sequence differs", true},
		{"user_version (1 vs 2)", []string{"CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT)", "INSERT INTO t VALUES(1, 'x')", "PRAGMA user_version = 1"}, []string{"CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT)", "INSERT INTO t VALUES(1, 'x')", "PRAGMA user_version = 2"}, "pragma user_version 1 vs 2", true},
		{"integer 1 vs real 1.0", []string{"CREATE TABLE t(id INTEGER PRIMARY KEY, v)", "INSERT INTO t VALUES(1, 1)"}, []string{"CREATE TABLE t(id INTEGER PRIMARY KEY, v)", "INSERT INTO t VALUES(1, 1.0)"}, "table t differs", false},
		{"value boundary ('a3','b' vs 'a','3b')", []string{"CREATE TABLE t(id INTEGER PRIMARY KEY, a TEXT, b TEXT)", "INSERT INTO t VALUES(1, 'a3', 'b')"}, []string{"CREATE TABLE t(id INTEGER PRIMARY KEY, a TEXT, b TEXT)", "INSERT INTO t VALUES(1, 'a', '3b')"}, "table t differs", true},
		{"row moved between tables", []string{"CREATE TABLE a(v TEXT)", "CREATE TABLE b(v TEXT)", "INSERT INTO a(rowid, v) VALUES(1, 'x'), (2, 'y')", "INSERT INTO b(rowid, v) VALUES(1, 'z')"}, []string{"CREATE TABLE a(v TEXT)", "CREATE TABLE b(v TEXT)", "INSERT INTO a(rowid, v) VALUES(1, 'x')", "INSERT INTO b(rowid, v) VALUES(1, 'y'), (2, 'z')"}, "table a differs", true},
	}
	fmt.Printf("  %-40s %-6s %-6s %s\n", "pair", "ours", "dbhash", "our first difference")
	type claim struct {
		ok   bool
		text string
	}
	var claims []claim
	for i, p := range pairs {
		a := newDB(ctx, filepath.Join(dir, "pairs", fmt.Sprintf("%d-a.db", i+1)), p.a...)
		b := newDB(ctx, filepath.Join(dir, "pairs", fmt.Sprintf("%d-b.db", i+1)), p.b...)
		result, _, _, err := equal.CompareLogicalFiles(ctx, a, b)
		if err != nil {
			fatal("compare", err)
		}
		dbhashVerdict := "-"
		if tool != "" {
			ha, hb := dbhashOf(ctx, tool, a), dbhashOf(ctx, tool, b)
			dbhashVerdict = sameWord(ha == hb)
			claims = append(claims, claim{(ha == hb) == p.dbhash, fmt.Sprintf("%s: dbhash says %s", p.name, dbhashVerdict)})
		}
		first := ""
		if len(result.Differences) > 0 {
			first = result.Differences[0]
		}
		fmt.Printf("  %-40s %-6s %-6s %s\n", p.name, sameWord(result.Equal), dbhashVerdict, first)
		claims = append(claims, claim{!result.Equal && strings.Contains(first, p.ours), fmt.Sprintf("%s: our logical hash tells the two apart", p.name)})
	}
	for _, c := range claims {
		check(c.ok, c.text)
	}
	fmt.Println("  sqldiff is not built by build-tools; the research probe found it reports integer 1 and real 1.0 as equal (docs/knowledge/determinism-and-rebuild.md, Q4)")
}

func sameWord(equalValues bool) string {
	if equalValues {
		return "equal"
	}
	return "differ"
}

// partC tests the three boundaries of VACUUM INTO as a canonical form
// (vacuum.c:143-427): table creation order, prior schema-cookie increments,
// and index creation timing.
func partC(ctx context.Context, dir, tool string) {
	fmt.Println()
	fmt.Println("== C. where VACUUM INTO stops being a canonical form")
	cases := []struct {
		name string
		a, b []string
	}{
		{"tables created in another order", []string{"CREATE TABLE a(id INTEGER PRIMARY KEY, v TEXT)", "CREATE TABLE b(id INTEGER PRIMARY KEY, v TEXT)", "INSERT INTO a VALUES(1, 'x')", "INSERT INTO b VALUES(1, 'y')"},
			[]string{"CREATE TABLE b(id INTEGER PRIMARY KEY, v TEXT)", "CREATE TABLE a(id INTEGER PRIMARY KEY, v TEXT)", "INSERT INTO a VALUES(1, 'x')", "INSERT INTO b VALUES(1, 'y')"}},
		{"a table created and dropped in the history", []string{"CREATE TABLE a(id INTEGER PRIMARY KEY, v TEXT)", "INSERT INTO a VALUES(1, 'x')"},
			[]string{"CREATE TABLE a(id INTEGER PRIMARY KEY, v TEXT)", "CREATE TABLE scratch(x)", "DROP TABLE scratch", "INSERT INTO a VALUES(1, 'x')"}},
		{"an index created before vs after the rows", []string{"CREATE TABLE a(id INTEGER PRIMARY KEY, v TEXT)", "CREATE INDEX a_v ON a(v)", "INSERT INTO a VALUES(1, 'x'), (2, 'y')"},
			[]string{"CREATE TABLE a(id INTEGER PRIMARY KEY, v TEXT)", "INSERT INTO a VALUES(1, 'x'), (2, 'y')", "CREATE INDEX a_v ON a(v)"}},
	}
	for i, c := range cases {
		a := newDB(ctx, filepath.Join(dir, "limits", fmt.Sprintf("%d-a.db", i+1)), c.a...)
		b := newDB(ctx, filepath.Join(dir, "limits", fmt.Sprintf("%d-b.db", i+1)), c.b...)
		e3, _, _, err := equal.CompareLogicalFiles(ctx, a, b)
		if err != nil {
			fatal("compare", err)
		}
		ca, cb := strings.TrimSuffix(a, ".db")+"-vacuum.db", strings.TrimSuffix(b, ".db")+"-vacuum.db"
		vacuumInto(ctx, a, ca)
		vacuumInto(ctx, b, cb)
		e1 := must(equal.CompareFiles(ca, cb, pageSize))("E1")
		e2 := must(equal.ComparePageFiles(ca, cb, pageSize))("E2")
		fmt.Printf("  %s: sources at E3: %s; VACUUM INTO outputs at E1: %s; at E2: %s; schema cookies %s\n", c.name, logicalVerdict(e3), verdict(e1), verdict(e2), cookies(ca, cb))
		if tool != "" {
			fmt.Printf("    dbhash of the sources: %s\n", sameWord(dbhashOf(ctx, tool, a) == dbhashOf(ctx, tool, b)))
		}
		switch i {
		case 0:
			check(e3.Equal && !e1.Equal && !e2.Equal, "same content, tables created in another order: logically equal, but VACUUM INTO copies tables in sqlite_schema order, so the canonical files differ beyond the header")
			scriptA, scriptB := dump(ctx, a), dump(ctx, b)
			rb := strings.TrimSuffix(b, ".db") + "-replayed.db"
			replay(ctx, scriptB, rb)
			crb := strings.TrimSuffix(rb, ".db") + "-vacuum.db"
			vacuumInto(ctx, rb, crb)
			replayE1 := must(equal.CompareFiles(cb, crb, pageSize))("E1")
			fmt.Printf("    captures of the two sources: %s; the second source's capture replayed and canonicalized vs the second source's canonical form at E1: %s\n", sameWord(scriptA == scriptB), verdict(replayE1))
			check(scriptA != scriptB && replayE1.Equal, "the capture writes CREATE TABLE in sqlite_schema order, so it records the creation order too: the replay of a source canonicalizes to the source's own canonical bytes")
		case 1:
			check(e3.Equal && !e1.Equal && e2.Equal, "a schema change that left no trace in the content still raises the schema cookie, and VACUUM INTO carries the cookie (+1): the canonical files differ only at bytes 40-43")
		case 2:
			check(e3.Equal && e1.Equal, "an index built before or after the rows canonicalizes to the same bytes: VACUUM INTO rebuilds every index")
		}
	}
}

func cookies(a, b string) string {
	ia, ib := must(os.ReadFile(a))("read"), must(os.ReadFile(b))("read")
	return fmt.Sprintf("%d and %d", be32(ia[40:44]), be32(ib[40:44]))
}

func main() {
	ctx := context.Background()
	sqlitex.MustPinCheckMemory(ctx)
	dir := sqlitex.MustWorkDir("3.3")
	fmt.Printf("INFO: work dir %s\n", displayPath(dir))
	tool := locateDBHash()
	c := partA(ctx, dir, tool)
	rollbackPair(ctx, dir, c)
	partB(ctx, dir, tool)
	partC(ctx, dir, tool)
	fmt.Println()
	if len(failures) > 0 {
		fmt.Printf("SAME ROWS FAILED: %d claim(s) did not hold\n", len(failures))
		for _, f := range failures {
			fmt.Printf("  - %s\n", f)
		}
		os.Exit(1)
	}
	fmt.Println("SAME ROWS OK")
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

Run from the repository root:

```text
go run ./ch03-btrees-and-page-management/experiments/3.3-same-rows-different-bytes
```

Captured evidence:

<!-- include: evidence/3.3-same-rows-different-bytes.txt -->
```text
sqlite_source_id: 2026-07-24 19:02:57 bf7c7f30031888f4e796e429ab3978879485813aaca6f641c7b33e4e09459bcc (pin 3.53.4 ok)
INFO: work dir _work/3.3
INFO: dbhash _tools/bin/dbhash
dbhash: found (built from the pinned tree; it prints a SHA1 of the schema and every row it reads)

== A. the same final rows built four ways, then captured as an ordered logical dump and replayed (rung R2)
content of R0 (recipe seed 1, 30 steps of inserts, updates and deletes): 4 schema statements, rows accounts=183 documents=96 events=1177, user_version 30
capture: capture/r0.sql, 2005315 bytes, 1468 lines, sha256 92d3898538e25714f5943079b3b5046b31b4f637430abbc1aa5d642a2d95c687
  PRAGMA page_size=4096;
  PRAGMA auto_vacuum=NONE;
  PRAGMA encoding='UTF-8';
  PRAGMA journal_mode=WAL;
  BEGIN;
  CREATE TABLE accounts(id INTEGER PRIMARY KEY, name TEXT NOT NULL, balance INTEGER NOT NULL);
  CREATE TABLE documents(id INTEGER PRIMARY KEY, account_id INTEGER NOT NULL, title TEXT NOT NULL, body BLOB NOT NULL);
  CREATE TABLE events(id INTEGER PRIMARY KEY, account_id INTEGER NOT NULL, kind TEXT NOT NULL, amount INTEGER NOT NULL, note TEXT NOT NULL);
  INSERT INTO "accounts"(rowid, "id", "name", "balance") VALUES(1, 1, 'wxhyvco', 37188);
  ... (1455 INSERT lines)
  CREATE INDEX events_by_account ON events(account_id, id);
  PRAGMA user_version=30;
  PRAGMA application_id=1397836120;
  COMMIT;
builds:
  r0               the recipe: 31 transactions with updates and deletes, index created at step 5; 1105920 bytes
  one-transaction  schema and every row in one transaction, tables in order, ids ascending; 1089536 bytes
  per-statement    the same order, one transaction per statement; 1089536 bytes
  shuffled         one transaction per row, rows of all tables in a shuffled order (PCG seed 3); 1093632 bytes
  replayed         the capture file replayed into an empty file (indexes created after the rows); 1089536 bytes
  backup           sqlite3_backup of R0 (a page copy, for the E2 row); 1105920 bytes
each build against R0 (E1 = file sha256, E2 = page diff ignoring header bytes 24-27, 40-43, 92-99, E3 = logical hash + integrity_check + foreign_key_check):
  build            sha256           E1 vs r0                                     E2 vs r0                                     E3 vs r0
  r0               24aa158fdf713808 equal                                        equal                                        equal
  one-transaction  224810279caf4016 differ (268 pages differ, sizes differ)      differ (268 pages differ, sizes differ)      equal
  per-statement    224810279caf4016 differ (268 pages differ, sizes differ)      differ (268 pages differ, sizes differ)      equal
  shuffled         dc2b3bbb223253be differ (270 pages differ, sizes differ)      differ (270 pages differ, sizes differ)      equal
  replayed         e3c97131367e136d differ (262 pages differ, sizes differ)      differ (262 pages differ, sizes differ)      equal
  backup           a268f05a569a3c07 differ (1 of 270 pages)                      equal except header bytes [27 43 95]         equal
check ok: the backup copy differs from R0 in bytes (E2 pages: equal ignoring header bytes 24-27, 40-43, 92-99 (bytes differ only at [27 43 95])) but is equal at E2: only declared header bytes differ
the same builds, logically and canonically:
  build            logical sha256   dump sha256      dbhash                                   VACUUM INTO sha256
  r0               3de6a8870f70a57c 92d3898538e25714 9bee16896cb395b91df1ef4de2cbf4095ab44d13 927e39a45becbb84
  one-transaction  3de6a8870f70a57c 92d3898538e25714 9bee16896cb395b91df1ef4de2cbf4095ab44d13 927e39a45becbb84
  per-statement    3de6a8870f70a57c 92d3898538e25714 9bee16896cb395b91df1ef4de2cbf4095ab44d13 927e39a45becbb84
  shuffled         3de6a8870f70a57c 92d3898538e25714 9bee16896cb395b91df1ef4de2cbf4095ab44d13 927e39a45becbb84
  replayed         3de6a8870f70a57c 92d3898538e25714 9bee16896cb395b91df1ef4de2cbf4095ab44d13 927e39a45becbb84
  backup           3de6a8870f70a57c 92d3898538e25714 9bee16896cb395b91df1ef4de2cbf4095ab44d13 9324d9446d407e55
logical hash of R0: logical sha256 3de6a8870f70a57c3fe28ce66a186325180876f77a1358c1717dbc4bc26cf2f0; schema rows 4; table rows accounts=183 documents=96 events=1177; page_size=4096 encoding=UTF-8 auto_vacuum=none user_version=30 application_id=1397836120 journal_mode=wal default_cache_size=-2000; integrity_check ok; foreign_key_check 0 row(s)
distinct file sha256 among r0, one-transaction, per-statement, shuffled, replayed: 4
check ok: in WAL mode one transaction and one transaction per statement give byte-identical files: commit boundaries alone leave no trace when rows are only appended
check ok: insert order (shuffled) and history (R0's updates and deletes) each give a different file
check ok: every build has the same logical hash
check ok: every build is equal to R0 at E3 (logical hash, integrity_check ok, foreign_key_check unchanged)
check ok: the ordered logical capture is itself identical for every build: it depends on content and the schema's creation order, not on insert order, batching or updates
check ok: after VACUUM INTO every build, the replayed copy included, has the same sha256 (E1 between canonical forms)
canonical form of the backup vs of R0: E1 differ (1 of 266 pages); E2 equal except header bytes [43]; schema cookies 5 and 2
check ok: except the backup copy: the backup API sets the copy's schema cookie to (old destination cookie + 1) = 1 (backup.c:448), VACUUM INTO carries the cookie + 1, so its canonical form differs at byte 43 only
check ok: dbhash from the pinned tree also calls every build equal: it agrees with our logical hash on equality
canonical form: 1089536 bytes, header bytes 18-19 = 1,1 (1 = rollback journal), change counter 1, schema cookie 5
check ok: VACUUM INTO keeps every row, the schema and user_version/application_id, but writes a rollback-mode file: its logical hash differs from the source only in journal_mode

== A2. the same two batchings in rollback mode (journal_mode=DELETE)
one transaction vs one per statement: E1 differ (1 of 266 pages); E2 equal except header bytes [26 27 94 95]; change counters 3 and 1463
check ok: in rollback mode the batching shows, but only in the change counter (bytes 24-27 and 92-95 count commits): E2 equal

== B. pairs that differ in content where a weaker logical hash says equal
  pair                                     ours   dbhash our first difference
  implicit rowids (1,2 vs 5,9)             differ equal  table t differs (2 vs 2 rows)
  sqlite_sequence (1 vs 2)                 differ equal  table sqlite_sequence differs (1 vs 1 rows)
  user_version (1 vs 2)                    differ equal  pragma user_version 1 vs 2
  integer 1 vs real 1.0                    differ differ table t differs (1 vs 1 rows)
  value boundary ('a3','b' vs 'a','3b')    differ equal  table t differs (1 vs 1 rows)
  row moved between tables                 differ equal  table a differs (2 vs 1 rows)
check ok: implicit rowids (1,2 vs 5,9): dbhash says equal
check ok: implicit rowids (1,2 vs 5,9): our logical hash tells the two apart
check ok: sqlite_sequence (1 vs 2): dbhash says equal
check ok: sqlite_sequence (1 vs 2): our logical hash tells the two apart
check ok: user_version (1 vs 2): dbhash says equal
check ok: user_version (1 vs 2): our logical hash tells the two apart
check ok: integer 1 vs real 1.0: dbhash says differ
check ok: integer 1 vs real 1.0: our logical hash tells the two apart
check ok: value boundary ('a3','b' vs 'a','3b'): dbhash says equal
check ok: value boundary ('a3','b' vs 'a','3b'): our logical hash tells the two apart
check ok: row moved between tables: dbhash says equal
check ok: row moved between tables: our logical hash tells the two apart
  sqldiff is not built by build-tools; the research probe found it reports integer 1 and real 1.0 as equal (docs/knowledge/determinism-and-rebuild.md, Q4)

== C. where VACUUM INTO stops being a canonical form
  tables created in another order: sources at E3: equal; VACUUM INTO outputs at E1: differ (3 of 3 pages); at E2: differ (3 of 3 pages); schema cookies 3 and 3
    dbhash of the sources: equal
check ok: same content, tables created in another order: logically equal, but VACUUM INTO copies tables in sqlite_schema order, so the canonical files differ beyond the header
    captures of the two sources: differ; the second source's capture replayed and canonicalized vs the second source's canonical form at E1: equal
check ok: the capture writes CREATE TABLE in sqlite_schema order, so it records the creation order too: the replay of a source canonicalizes to the source's own canonical bytes
  a table created and dropped in the history: sources at E3: equal; VACUUM INTO outputs at E1: differ (1 of 2 pages); at E2: equal except header bytes [43]; schema cookies 2 and 4
    dbhash of the sources: equal
check ok: a schema change that left no trace in the content still raises the schema cookie, and VACUUM INTO carries the cookie (+1): the canonical files differ only at bytes 40-43
  an index created before vs after the rows: sources at E3: equal; VACUUM INTO outputs at E1: equal; at E2: equal; schema cookies 3 and 3
    dbhash of the sources: equal
check ok: an index built before or after the rows canonicalizes to the same bytes: VACUUM INTO rebuilds every index

SAME ROWS OK
```

**Reading the evidence:**

- **Part A — R0, five rebuilds, and rung R2 (lines 6–59).** R0 (recipe seed 1, 30 steps) has `4` schema statements, `accounts=183`, `documents=96`, `events=1177`, and `user_version=30`. Its rung R2 logical capture `capture/r0.sql` is `2,005,315` bytes (`1,468` lines, sha256 `92d38985…`, about 1.8× R0's `1,105,920`-byte size because blobs are hex-encoded). Comparing all six builds against R0:
  - **Correction 1 (WAL append-only batching):** `one-transaction` and `per-statement` both produce `1,089,536` bytes with the **exact same SHA-256** (`224810279caf4016`). Commit boundaries alone leave no trace in a WAL-mode file when rows are only appended, because WAL mode only writes page 1's change counter when page 1 is already dirty. Among the five non-backup builds (`r0`, `one-transaction`, `per-statement`, `shuffled`, `replayed`), there are **`4` distinct file SHA-256s**: history (R0's updates/deletes, `24aa158f…`), append order (`22481027…`), shuffled order (`dc2b3bbb…`, `1,093,632` bytes), and post-load index creation (`e3c97131…`, `1,089,536` bytes).
  - **Level E2 (`backup`):** `sqlite3_backup` of R0 (`a268f05a…`) differs from R0 on `1 of 270 pages` at E1, and passes **E2** (`equal except header bytes [27 43 95]`): only the change counter, schema cookie, and version-valid-for differ.
  - **Level E3 and R2 capture:** All six builds share the exact same logical SHA-256 (`3de6a8870f70a57c…`), the exact same `capture/r0.sql` SHA-256 (`92d3898538e25714…`), and the exact same `dbhash` SHA-1 (`9bee16896cb395b91df1ef4de2cbf4095ab44d13`).
  - **`VACUUM INTO` canonicalization and the backup exception (correction 2):** All five non-backup builds—including `replayed`—canonicalize to one SHA-256 (`927e39a45becbb84`, `1,089,536` bytes, rollback header `1,1`, change counter `1`, schema cookie `5`). The backup copy canonicalizes to `9324d9446d407e55`, differing from R0's canonical form only at byte `43` (`E2` equal): `sqlite3_backup_step` set the backup's schema cookie to `0 + 1 = 1` (`backup.c:448`), and `VACUUM INTO` carried each source's cookie `+1` (`4 -> 5` vs `1 -> 2`). Every canonical file differs from its WAL source at E3 only in `pragma journal_mode` (`wal` vs `rollback`).
- **Part A2 — rollback-mode batching (lines 61–63).** Repeating `one-transaction` vs `per-statement` in `journal_mode=DELETE` produces two files that differ at E1 (`1 of 266 pages`) and match at E2 (`equal except header bytes [26 27 94 95]`), with change counters `3` vs `1463` counting the commits.
- **Part B — six blind spots of weaker logical hashes (lines 65–85; correction 3).** Our E3 logical hash distinguishes all six pairs, naming the exact table or pragma that differs, while `dbhash` calls **five of the six** `equal`:
  1. `implicit rowids (1,2 vs 5,9)` — `dbhash` blind (`SELECT *` omits hidden rowid).
  2. `sqlite_sequence (1 vs 2)` — `dbhash` blind (skips `sqlite_*` tables).
  3. `user_version (1 vs 2)` — `dbhash` blind (hashes no file-header pragmas).
  4. `integer 1 vs real 1.0` — `dbhash` distinguishes it, whereas `sqldiff` reports them equal (from `docs/knowledge/determinism-and-rebuild.md`, Q4; `sqldiff` is not built by `build-tools`).
  5. `value boundary ('a3','b' vs 'a','3b')` — `dbhash` blind (`hash_one_query` in `tool/dbhash.c:281-360` hashes `"3a33b"` for both because TEXT has no length prefix).
  6. `row moved between tables` — `dbhash` blind (no row or table delimiter between tables `a` and `b`).
- **Part C — where `VACUUM INTO` stops being a canonical form (lines 87–98; correction 2).**
  1. **Tables created in another order:** `CREATE TABLE a` then `b` vs `b` then `a` are E3-equal (and `dbhash`-equal), but `VACUUM INTO` copies tables in `sqlite_schema` rowid order, so the canonical files differ at E1 **and** E2 (`3 of 3 pages`, both schema cookie `3`). Because our R2 `dump()` writes `CREATE TABLE` in `sqlite_schema` rowid order, the two sources' captures differ, and replaying the second source's capture and running `VACUUM INTO` matches the second source's canonical form at E1 (`equal`).
  2. **A dropped scratch table in history:** Creating and dropping `scratch` leaves no logical trace (E3-equal, `dbhash`-equal), but raises the schema cookie, so `VACUUM INTO` outputs differ at byte `43` (`schema cookies 2 and 4`, E1 differ, E2 equal).
  3. **Index created before vs after rows:** E3-equal and canonical E1-equal (`schema cookies 3 and 3`), because `VACUUM INTO` always rebuilds indexes from scratch.

### 6.2 Summary of outline corrections

Ten outline assumptions were refined by the build, review, and adversarial runs and are now stated in narrowed form throughout this chapter:

1. **WAL append-only batching** (`3.3` Part A/A2): one transaction vs per-statement produces identical bytes in WAL mode; commit boundaries appear only in rollback mode's change counter (E2-equal).
2. **`VACUUM INTO` canonical limits** (`3.3` Part A/C, `EQ-03`, `R2-02`): canonical E1 equality requires the same table creation order, the same schema cookie (backup copies reset it to `1`), and the same `default_cache_size` (now included in E3).
3. **`dbhash` extra blind spots** (`3.3` Part B): `dbhash` also misses unframed value boundaries (`'a3','b'` vs `'a','3b'`) and rows moved between tables.
4. **WAL frames `>=` changed pages** (`3.1` Part B): `balance_nonroot` can rewrite a sibling leaf with identical bytes, shipping 6 WAL frames for 5 byte-changed pages.
5. **First split = first depth change** (`3.1` Part A): on an empty table, insert `#5` runs both `balance_deeper` and `balance_quick`.
6. **Source spans** (`SPANS.txt`): `balance_nonroot`'s header comment is `btree.c:8204-8244` before the function at `8245-9027`; `btreeInt.h:149-228` covers the comment (`149-215`) plus `MX_CELL_SIZE`/`MX_CELL` (`222-229`).
7. **Scope of "most inserts change one page"** (`3.1` Part E, `SPL-01`/`SPL-03`/`SPL-05`): breaks for random keys in few-row leaves (`86/250`), overflowing rows (`0/25`), and `WITHOUT ROWID` append splits (`5` pages at depth 2 via `balance_nonroot`).
8. **Scope of "file never shrinks on its own"** (`3.2`, `DEL-02`): holds only for `auto_vacuum=NONE`; `FULL` shrinks at the `DELETE` commit and `INCREMENTAL` on `PRAGMA incremental_vacuum`.
9. **Scope of `secure_delete=ON`** (`3.2`, `DEL-05`): scrubs the database file at rest (`0/40`), but `-wal` retains prior `INSERT` frames (`40/40` even after `PASSIVE` checkpoint) until reset by `TRUNCATE`.
10. **Per-allocation freelist reuse** (`3.2`, `DEL-06`): one large transaction can exhaust the freelist and grow `page_count` in the same commit (`freelist 2 -> 0`, `page_count 12 -> 18`).

### What attacked this chapter

<!-- adversarial-results -->

Four adversarial programs (`30` attacks total, driven by [`run-all.sh`](../agent_work/ch03/adversarial/run-all.sh)) attacked this chapter's claims, `internal/equal`, and Experiments 3.1–3.3 against the pinned SQLite 3.53.4 build:

- [`splits-stress`](../agent_work/ch03/adversarial/splits-stress/main.go) (`SPL-01`–`SPL-07b`, 8 attacks): tested random insert orders across 3 fixed PCG seeds (`11`, `42`, `99`) with 1,000-byte and 40-byte blobs (`SPL-01`); verified exact cell-size split arithmetic across `50B`, `400B`, `1900B`, and the `1013B` rowid-varint boundary where leaf capacity shifts from 4 to 3 at rowid `128` (`SPL-02`); tested overflowing `5000B` and `10000B` rows (`SPL-03`); scaled secondary indexes across `0, 1, 2, 4` indexes (`SPL-04`, mode `1+k`, constant roots); tested `WITHOUT ROWID` ascending appends (`SPL-05`); verified same-size in-place `UPDATE` (`1` page) vs growing `UPDATE` (`5` pages, `6` WAL frames) (`SPL-06`); and tested `equal.ChangedPages` on leaf-aligned and mid-leaf `DELETE`s under `secure_delete=OFF` (`SPL-07a`, `SPL-07b`).
- [`delete-leaks`](../agent_work/ch03/adversarial/delete-leaks/main.go) (`DEL-01`–`DEL-06`, 6 attacks): checked deleted tenant markers across source, plain file copy, `sqlite3_backup`, `sqlite3_serialize`, and `VACUUM INTO` under `OFF` (`95, 95, 95, 95, 0`) vs `ON` (`0` in all five) (`DEL-01`); tested `auto_vacuum=NONE`, `FULL`, and `INCREMENTAL` (`DEL-02`); tested `secure_delete` `OFF` vs `ON` across overflow deletes, index deletes, shrinking updates, `DROP TABLE`, and unqualified `DELETE FROM t` (`DEL-03`); tested `PRAGMA secure_delete=FAST` (`mode=2`, scrubbing live-page updates `0/1` while leaving `38/80` markers on freelist leaves, `DEL-04`); tested `-wal` residue with `secure_delete=ON` after `DELETE`, `PASSIVE`, and `TRUNCATE` (`DEL-05`); and tested a single 32-row transaction exhausting a 2-page freelist and growing `page_count` from `12` to `18` (`DEL-06`).
- [`equal-falsify`](../agent_work/ch03/adversarial/equal-falsify/main.go) (`EQ-01`–`EQ-09`, 9 attacks): tested 13 logical-difference pairs against `LogicalHash` (`0/13` missed) and `dbhash` (`10/13` blind) (`EQ-01`); tested 13 distinct values (including `DATETIME`, `±Inf`, `INT64_MIN/MAX`, embedded NUL), FTS5 shadow tables, and `WITHOUT ROWID` foreign-key orphans (`EQ-02`); tested `PRAGMA default_cache_size` vs `VACUUM INTO` (`EQ-03`); tested `CREATE TABLE t(rowid INTEGER PRIMARY KEY, _rowid_ TEXT, oid TEXT)` (`EQ-04`); tested `LogicalHashFile` and `RunDBHash` on an uncheckpointed WAL snapshot (`.db` + `-wal`) (`EQ-05`); tested E2 edge cases (`EQ-06`); tested `RunDBHash` on paths with spaces, missing files, and corrupt files (`EQ-07`); tested `TEMP` table shadowing and `EncodeValue` on unsupported Go types (`EQ-08`); and tested mid-walk context cancellation on `LogicalHash` (`EQ-09`).
- [`r2-canonical-and-pin`](../agent_work/ch03/adversarial/r2-canonical-and-pin/main.go) (`R2-01`–`R2-06`, 7 attacks): tested WAL one-tx vs per-statement E1 equality under secondary indexes, `cache_size=4` spills, and mixed `INSERT`+`DELETE`+re-`INSERT` (`R2-01`); tested R2 dump/replay/`VACUUM INTO` on tables created out of alphabetical order (`zebra` before `alpha`, `R2-02`); tested R2 dump/replay on `AUTOINCREMENT`/`sqlite_sequence`, `WITHOUT ROWID`, `DATETIME`, `±Inf`, quotes/newlines, empty blobs, embedded NULs (`char(0)`), and `?`-containing identifiers (`R2-03a`, `R2-03b`); verified `VACUUM INTO` header preservation and existing-target refusal (`R2-04`); verified pin-mismatch failure on all three experiments in a scratch repo (`R2-05`); and verified Experiment 3.3's `NO-DBHASH` graceful fallback (`R2-06`).

**Run 1** ([`results-run1.txt`](../agent_work/ch03/adversarial/results-run1.txt)) produced **16 HELD, 14 BROKEN**.

**Code defects exposed and fixed in repair:**

- `SPL-07b`: `equal.ChangedPages` previously reported pages whose census role changed with identical bytes (`17` pages reported vs `5` byte changes and `5` WAL frames on a `secure_delete=OFF` `DELETE`); fixed to report only SHA-256 changes and added/removed pages (`internal/equal/changes.go`).
- `EQ-03`: `PRAGMA default_cache_size` (header bytes `48-51`, copied by `VACUUM INTO`) was missing from `LogicalPragmas`, allowing E3-equal databases with identical schema history to canonicalize differently; added as the seventh pragma in `LogicalPragmas`.
- `EQ-04`: `rowidAlias` rejected `CREATE TABLE t(rowid INTEGER PRIMARY KEY, _rowid_ TEXT, oid TEXT)`; fixed with `integerPrimaryKeyColumn` fallback (accepting exact `INTEGER PRIMARY KEY` without a `pk`-origin autoindex, while still rejecting `DESC`, `INT`, and composite keys).
- `EQ-05`: `LogicalHashFile` and `RunDBHash` opened uncheckpointed WAL snapshots read-write and checkpointed/deleted `-wal` on close; fixed with `HasLiveSideFile` opening live snapshots `mode=ro` (leaving `.db` and `-wal` byte-identical).
- `EQ-08`: `WalkLogical` lacked `main.` qualification (allowing `TEMP` tables to shadow `main`), and `EncodeValue` returned empty bytes for unsupported Go types; fixed by qualifying every query on `main` and encoding unsupported types with tag `'U'` + type + value.
- `EQ-09`: canceling `ctx` mid-walk caused `LogicalHash`'s cleanup `COMMIT` to fail with `ctx.Err()`, leaking an open read transaction on `conn`; fixed by ending the transaction with `context.WithoutCancel(ctx)` (`ROLLBACK` on error, `COMMIT` on success).
- `R2-02` and `R2-03b`: Experiment 3.3's `dump()` previously sorted `CREATE TABLE` alphabetically (breaking canonical E1 match when tables were created out of name order), emitted raw NUL bytes inside SQL string literals, and used `strings.Replace` on `?` (corrupting identifiers containing `?`); fixed to emit schema statements in `main.sqlite_schema` rowid order, split NUL bytes into `'a'||char(0)||'b'`, and build `INSERT` statements directly with `insertStatement`.

**Claims narrowed in the experiments and this chapter (rather than changing SQLite behavior):**

- `SPL-01`, `SPL-03`, `SPL-05`: "most inserts change one page" and "depth-2 append splits change 3 pages" are now explicitly scoped in Experiment 3.1 Part E (random keys with 4 rows/leaf: `86/250`; overflowing rows: `0/25`; `WITHOUT ROWID` depth-2 append splits: `5` pages via `balance_nonroot`).
- `DEL-02`: "the file never shrinks on its own" is scoped to `auto_vacuum=NONE` (Experiment 3.2 `auto_vacuum` section).
- `DEL-05`: `secure_delete=ON` scrubs the database file at rest, not prior `INSERT` frames sitting in `-wal` until `TRUNCATE` resets the WAL (Experiment 3.2 `-wal` section).
- `DEL-06`: freelist reuse is per page allocation, not per transaction (Experiment 3.2 step 4 and large-transaction section).

**Run 2** ([`results-run2.txt`](../agent_work/ch03/adversarial/results-run2.txt), after repair with attacks unedited) moved to **22 HELD, 8 BROKEN** (the 6 narrowed claims plus `R2-02` and `R2-03b`, whose script kept a frozen pre-repair copy of `dump()`). After refreshing `r2-canonical-and-pin` to mirror the repaired `dump()` and aligning the 6 narrowed expectations, **Run 3** ([`results-run3.txt`](../agent_work/ch03/adversarial/results-run3.txt) and [`results.txt`](../agent_work/ch03/adversarial/results.txt)) passed **30 HELD, 0 BROKEN**.

---

## 7. Platform implications

Every operational rule below is backed directly by this chapter's experiments and adversarial checks:

1. **WAL shipping bandwidth is driven by dirtied pages, not row size (Experiment 3.1 Parts A, B, C, and E; `SPL-01`–`SPL-06`).** Every modified page ships as a `4,120`-byte WAL frame. `2,061` single-row appends of 1 KB rows shipped `3,094` frames (`12.7 MB`) for a `520`-page (`2.1 MB`) table—roughly `6×` write amplification. Adding `k` secondary indexes multiplies the common per-insert frame count to `1 + k` (`3` frames for 2 indexes in Part C; `1 + k` through `k = 4` in `SPL-04`). Random-key inserts into few-row leaves (`86/250` one-page in Part E), overflowing rows (`3..5` pages every insert), and `WITHOUT ROWID` splits (`5` pages at depth 2) amplify frame counts further.
2. **WAL frames shipped can exceed byte-changed pages (Experiment 3.1 Part B).** `balance_nonroot` wrote page `3` as a 6th WAL frame even though its bytes did not change (`c0995adc833eadb1` before and after). A WAL shipper must expect redundant page frames, while a page-diff replicator (`sqlite3_rsync` style) can skip pages whose hash did not change.
3. **Rollback mode adds page 1 to every commit (Experiment 3.1 Part D).** Under `journal_mode=DELETE`, every single commit rewrites page 1 (bytes `24-27` and `92-95`), doubling the database-file page writes on an ordinary insert before even counting the rollback journal.
4. **Databases do not shrink on delete under default `auto_vacuum=NONE` (Experiment 3.2, `DEL-02`).** Deleting `204` of `400` rows left the file at `417,792` bytes with `50` freelist pages; those pages were reused by later inserts before the file grew, and only `VACUUM` shrank the file (`107 -> 51` pages). Choose `auto_vacuum=FULL` (shrinks on `DELETE`, `53 -> 16` pages) or `INCREMENTAL` (shrinks on `PRAGMA incremental_vacuum`) at database creation time if automatic reclamation is required; otherwise schedule offline or `VACUUM INTO` compaction.
5. **`secure_delete=OFF` leaks deleted tenant data into page-level backups, and `secure_delete=ON` still leaves deleted rows in `-wal` until reset (Experiment 3.2, `DEL-01`, `DEL-03`, `DEL-04`, `DEL-05`).** Under `secure_delete=OFF`, `199` of `204` deleted rows remained readable in freelist leaves, the freelist trunk, and live-leaf freeblocks—and survived verbatim in plain file copies, `sqlite3_backup`, and `sqlite3_serialize` (`95/100` in `DEL-01`). Turning `secure_delete=ON` eliminated all residue in the `.db` file at rest (and in every backup of it), at a cost of `54` WAL frames vs `5` for the same 204-row `DELETE`—yet all `40` deleted rows stayed readable in `-wal` even after `PASSIVE` checkpoint until `wal_checkpoint(TRUNCATE)` reset the WAL. Multi-tenant deletion policies must combine `secure_delete=ON` with WAL truncation (and retention/rotation of shipped WAL segments).
6. **Choose E1, E2, or E3 to match the capture method, and do not trust `dbhash` for logical equality (Experiment 3.3 Parts A, A2, B, and C; `EQ-01`, `EQ-03`, `EQ-05`).** Different insert orders or update/delete histories produce physically different files (`4` distinct SHA-256s in Part A), a `sqlite3_backup` copy matches its source at **E2** (ignoring bytes `24-27`, `40-43`, `92-99`), and logical rebuilds match at **E3**. Use our E3 logical hash rather than `dbhash` (which missed `5` of `6` blind spots in Part B and `10` of `13` in `EQ-01`). When hashing an uncheckpointed WAL snapshot, open it read-only (`LogicalHashFile` and `RunDBHash` now do this via `HasLiveSideFile`) so inspection does not mutate `.db` and delete `-wal`.
7. **Know the exact limits of `VACUUM INTO` as a canonicalizer (Experiment 3.3 Parts A and C, `R2-02`, `R2-04`).** `VACUUM INTO` gives E1-identical canonical files across different write histories and R2 replays only when both databases share the same table creation order, the same schema cookie (a `sqlite3_backup` copy or a dropped scratch table shifts byte `43`, making the canonical files E2-equal instead of E1-equal), and the same `default_cache_size`. Because `VACUUM INTO` writes a rollback-mode file (`bytes 18-19 = 1,1`), compare canonical SHA-256s *before* switching a restored copy back to WAL mode.

---

## 8. Knowledge check

Answers are in [answers.md](answers.md).

1. In Experiment 3.1 Part A, why did the first split (insert `#5`) change 4 pages while the second split (insert `#9`) changed only 3 pages, and why did `WITHOUT ROWID` depth-2 append splits in Part E change 5 pages instead of 3?
2. In Experiment 3.1 Part B, inserting rowid `11` into full leaf `4` changed the bytes of `5` pages (`[1 2 4 5 8]`), yet the commit wrote `6` WAL frames (`[1 2 3 4 5 8]`). Why did page `3` enter the WAL with an identical SHA-256 (`c0995adc833eadb1`), and what does that mean for a platform shipping WAL frames versus a page-diff replicator?
3. In Experiment 3.2, why did deleting `204` rows write `5` WAL frames under `secure_delete=OFF` versus `54` WAL frames under `secure_delete=ON`, and why were `40` of `40` deleted rows still readable on disk after `DELETE` and `PRAGMA wal_checkpoint(PASSIVE)` even with `secure_delete=ON`?
4. In Experiment 3.3 Part A, `one-transaction` and `per-statement` produced byte-identical files in WAL mode (`224810279caf4016`), whereas in Part A2 (rollback mode) they differed on 1 of 266 pages (`E2` equal at bytes `[26 27 94 95]`). Why did commit boundaries disappear in WAL mode, and why did `backup`'s `VACUUM INTO` canonical file still differ from R0's canonical file at byte `43`?
5. Name three logical differences that SQLite's `dbhash` tool misses in Experiment 3.3 Part B, explain the exact cause in `tool/dbhash.c` for each, and explain how `internal/equal/logical.go` closes them.
