# Chunk 2 build notes (plain data for the prose pass)

Builder: Claude Opus 5.5, branch `chunk/ch02`, 2026-11-20. Machine: linux/amd64, Go 1.26.8, ext4.
CLI used for the evidence files: the installed `sqlite3` on PATH (reports the pinned source id).
All three experiments were also run inside the Debian `lab` container
(`docker compose run --rm -T lab go run ./ch02-file-format-and-pages/experiments/<name>`), whose CLI
is a `./configure` build of the pin: EXACT output identical to the host's (same md5 of the
non-INFO lines for all three). darwin: cross-built and cross-vetted only, never run.

All source line numbers are at the pin (3.53.4). `go run ./cmd/course check spans ch02` passes
(18 spans); `check sources ch02` passes (3 files).

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

## Shared packages

No existing shared package was changed (`sqlitex`, `trace`, `recipe`, `repocheck` untouched).
Three new packages:

### internal/dbfile (read-only parser; no SQLite)

Files: `errors.go`, `varint.go`, `header.go`, `file.go`, `btree.go`, `payload.go`, `record.go`,
`tree.go`, `census.go`, `dbfile_test.go`. About 1,000 lines of code, 600 of tests.

Errors (`errors.go`): sentinels `ErrNotADatabase`, `ErrTruncated`, `ErrCorrupt`,
`ErrUnsupported`, `ErrPageOutOfFile`. `*FormatError{Kind, Page, Offset, Problem}` wraps one of
them (`errors.Is` works) and prints `<kind>: page N offset M: <problem>` (or `file offset M` when
there is no page). Every malformed input returns one of these; nothing panics (see tests).

Varints (`varint.go`): `Varint(b) (value, length, err)` — big-endian base-128, 1-9 bytes, the 9th
byte contributes all 8 bits (same as `sqlite3GetVarint`, util.c:1633). Error if the buffer ends
first. `AppendVarint`, `VarintLength` for tests and record building.

Header (`header.go`):
- Constants: `HeaderSize` 100, `MagicString` "SQLite format 3\0", `LockByteOffset` 1073741824,
  `MinPageSize` 512, `MaxPageSize` 65536, `MinUsableSize` 480, text encodings 1/2/3.
- `ParseHeader(b) (Header, error)`: decodes every field into `Header` (`Raw` keeps the 100 bytes).
  `DecodePageSize`: stored value 1 means 65536 (SQLite computes `(b16<<8)|(b17<<16)`,
  btree.c:3371, which gives 65536 for the bytes 00 01).
  Validation mirrors `lockBtree` (btree.c:3282-3459): magic; page size a power of two 512..65536;
  read version (byte 19) and write version (byte 18) each 1 or 2 (SQLite opens a file with write
  version > 2 read-only and refuses read version > 2; ours refuses both because a read-only
  parser has no use for the distinction); payload fractions exactly 64/32/32; usable size
  (page size − byte 20) ≥ 480; text encoding ≤ 3 (0 allowed: a header before the first schema
  write); schema format ≤ 4.
- Methods: `UsableSize`, `ChangeCountersAgree` (24-27 == 92-95), `InHeaderSizeValid` (size at
  28 non-zero and counters agree — the rule at btree.c:3300), `WALMode` (18 and 19 both 2),
  `AutoVacuum` (byte 52 non-zero), `LockBytePage` (1073741824/pageSize + 1), `EncodingName`,
  `AutoVacuumName` (NONE / FULL / INCREMENTAL from bytes 52 and 64).
- `Fields()` returns 23 `HeaderField{Offset, Size, Name, Value}` covering all 100 bytes exactly
  once (test `TestHeaderFieldsCoverEveryByte`); `HeaderFieldAt(offset)` names the field a byte
  belongs to. `FormatSQLiteVersion(3053004)` = "3.53.4".

File (`file.go`):
- `Open(path)`, `FromBytes(image)`, `New(io.ReaderAt, size)`. Reads the header, then:
  file size must be a multiple of the page size (`ErrTruncated` otherwise — correction 3);
  page count = file size / page size, unless the in-header size is valid, in which case it is
  used, and if it is larger than the file that is `ErrCorrupt` (SQLite: "database disk image is
  malformed", btree.c:3405-3412). `PageCountSource()` returns `"in-header size"` or
  `"file size"`.
- `Page(n)` returns a copy of page n (1-based); out of range → `ErrPageOutOfFile`.
- `LockBytePage()` is 0 when the file is not large enough to contain it.
- `PointerMapPage(n)`, `IsPointerMapPage(n)`: the `ptrmapPageno` formula (btree.c:1036-1048):
  one pointer-map page every `usable/5 + 1` pages starting at page 2, shifted by one if it
  would land on the lock-byte page. Only when auto-vacuum is on.
- **Process rule:** `Open` opens the database file with `os.Open`. Closing that descriptor
  drops every POSIX lock the process holds on the file, so never call it in a process that has a
  live SQLite connection to the same file (outline 4.1). All experiments read databases only
  after closing their connections, or from a child process.

B-tree pages (`btree.go`):
- `PageType` 0x02 index-interior, 0x05 table-interior, 0x0a index-leaf, 0x0d table-leaf
  (`decodeFlags`, btree.c:2028-2085, rejects every other byte the same way). `HeaderSize()` 8
  for leaves, 12 for interior pages (4-byte right child at offset 8).
- `ParseBTreePage(data, number, usable)` / `File.BTreePage(n)`: header at offset 100 on page 1,
  0 elsewhere. Reads first-freeblock (1-2), cell count (3-4), cell-content start (5-6; 0 means
  65536), fragmented bytes (7), right child (8-11). Checks: valid type; the cell-pointer array
  fits in the usable size; content start between the end of the pointer array and the usable
  size; every cell pointer between the content start and `usable−4` (leaves) or `usable−5`
  (interior) — the same range `integrity_check` prints ("Offset 9 out of range 2260..4092");
  freeblock chain ascending, inside the content area, each block ≥ 4 bytes, bounded length.
- `Unused()` = content start − end of pointer array + fragmented bytes + freeblock bytes. This
  is exactly `dbstat`'s `unused` (dbstat.c:396-505, `statDecodePage`).
- `Cell(i)` / `Cells()` → `Cell{Index, Offset, LeftChild, PayloadSize, Rowid, HasRowid,
  LocalSize, Local, FirstOverflow, Size}`. Table interior: child + rowid varint. Table leaf:
  payload-size varint, rowid varint, local payload, optional 4-byte first-overflow page. Index
  interior: child + payload size + payload + overflow. Index leaf: payload size + payload +
  overflow. Payload size > 0x7fffffff, a cell running past the usable size, or an overflowing
  cell whose overflow pointer is 0 → `ErrCorrupt`.
- `MaxLocalPayload(usable, type)`: table leaf `usable − 35` (4061 at 4096); index pages
  `(usable−12)*64/255 − 23` (1002). `MinLocalPayload` `(usable−12)*32/255 − 23` (489). From
  `lockBtree` btree.c:3441-3444 and `decodeFlags`.
- `LocalPayloadSize(usable, type, P)`: P if P ≤ maxLocal; else surplus = minLocal + (P −
  minLocal) % (usable − 4); local = surplus if surplus ≤ maxLocal else minLocal
  (`btreeParseCellAdjustSizeForOverflow`, btree.c:1178-1207).

Payload and overflow (`payload.go`):
- `File.OverflowChain(sourcePage, cell) ([]OverflowPage, error)`: the number of overflow pages
  is ceil((P − local) / (usable − 4)); each overflow page is a 4-byte next pointer plus up to
  `usable − 4` payload bytes. Each target must be inside the file, not page 1, not the lock-byte
  page, not a pointer-map page, not seen before in the chain (a loop is `ErrCorrupt`), and the
  chain must not end (next = 0) early. The last page's next pointer is not checked.
- `File.Payload(sourcePage, cell)` assembles local + overflow bytes; refuses a payload larger
  than the file (stops a corrupt varint from allocating gigabytes).

Records (`record.go`):
- `DecodeRecord(payload) ([]Value, error)`: header-size varint, serial-type varints, then the
  body. `Value{SerialType, Class, Int, Float, Bytes}`; `StorageClass` null/integer/real/text/blob.
- `SerialTypeLength`: 0→0, 1→1, 2→2, 3→3, 4→4, 5→6, 6→8, 7→8, 8,9→0 (constants 0 and 1),
  N≥12 even → blob (N−12)/2, N≥13 odd → text (N−13)/2. Types 10 and 11 are an error in ours;
  `sqlite3VdbeSerialGet` (vdbeaux.c:4123-4214) reads them as NULL (10 is internal-use, 11
  reserved; neither should appear in a file). Integers are big-endian two's complement.
- Type 7 (float) holding a NaN decodes as NULL, matching `serialGet` (vdbeaux.c:4103:
  `pMem->flags = IsNaN(x) ? MEM_Null : MEM_Real`).
- `DecodeText(raw, encoding)`: UTF-8 as is; UTF-16le/be decoded (test with a non-ASCII table
  name in both UTF-16 encodings).
- Note: an `INTEGER PRIMARY KEY` column is stored as NULL in the record; the value is the
  rowid. 2.2's row hash substitutes the rowid for a NULL in column 0 of the recipe tables (all
  three have `id INTEGER PRIMARY KEY` first). The parser itself does not read column affinity.

Trees and schema (`tree.go`):
- `TreeWalker{File, OnPage(page, parent, depth), OnCell(page, cell)}.Walk(root)`: depth-first in
  key order (each cell's left child before the cell, right child last). Refuses: a page seen
  twice in one tree, a table page in an index tree or the reverse, depth ≥ 20
  (`BTCURSOR_MAX_DEPTH`), child pointers to page 1, out of range, the lock-byte page or a
  pointer-map page.
- `File.WalkTable(root, visit(Row))`: every row of a table b-tree with its decoded values, leaf
  page and overflow page numbers.
- `File.Schema()`: walks page 1 (the `sqlite_schema` table) and returns `SchemaEntry{Type,
  Name, TableName, RootPage, SQL, Rowid}`; root page 0/NULL for views and triggers; a root page
  outside the file is `ErrCorrupt`.

Census (`census.go`):
- `Role`: unaccounted, table-interior, table-leaf, index-interior, index-leaf, overflow,
  freelist-trunk, freelist-leaf, pointer-map, lock-byte. `AllRoles`, `ParseRole`.
- `File.Census() (*Census, error)`: claims the lock-byte page (if in the file), then every
  pointer-map page (auto-vacuum only), then walks `sqlite_schema` and every tree with a root
  page, claiming each b-tree page and each overflow page, then walks the freelist from header
  offset 32. A page claimed twice is `ErrCorrupt` naming both claims ("page claimed twice: as
  table-leaf of "t" and as freelist-trunk"). The freelist walk: trunk = next(4) + leaf count(4) +
  leaf page numbers; leaf count > `usable/4 − 2` is corrupt (the reader limit
  `integrity_check` uses); the total (trunks + leaves) must equal header offset 36.
- `Census{PageSize, UsableSize, PageCount, Pages []PageInfo, Trees []Tree, Schema,
  FreelistTrunks, FreelistLeaves, Unaccounted}`. `PageInfo{Number, Role, Tree, Parent,
  CellCount, Payload, Unused, MaxPayload}` holds the `dbstat` columns: for b-tree pages `ncell`,
  `payload` (sum of local bytes), `unused`, `mx_payload` (largest total payload of a cell on the
  page; 0 on table-interior pages); for overflow pages `payload` = bytes used, `unused` =
  `usable − 4 − payload`, as `dbstat` computes them (dbstat.c:620-627). For trunk pages
  `CellCount` is the number of leaves listed.
- `Complete()` returns an error naming the unaccounted pages; `RoleCounts()`,
  `FreelistLength()`, `Page(n)`, `SortedTrees()`.
- Not checked (out of scope, `integrity_check` does it): key order within and across pages,
  fragmentation accounting, overlapping cells, pointer-map entries' contents. 2.2 F6 shows a
  case `integrity_check` catches through key order ("Rowid 21 out of order") that our census
  catches only indirectly (the overflow pages become unaccounted).

Tests (`go test ./internal/dbfile`, about 10-20 s): varint round trip (boundaries 0x7f/0x80,
2^56−1 → 8 bytes, 2^56 → 9 bytes, 2000 random values, truncated input); header vs pragmas at page
sizes 512, 1024, 4096, 65536 (65536 stored as 1); UTF-16le/be schema; census vs `dbstat` and
pragmas on the full recipe in WAL and rollback mode; row values vs SQLite `typeof`/`quote`
(all integer widths, 1.5, −0.0, 1e308, text, blob, zeroblob(10000) overflow, negative rowid);
local-payload boundary (blob 4057 → payload 4061 local; 4058 → 489 local + 1 overflow page;
8149 → 4061 local + exactly one full overflow page); freelist across trunks (page size 1024);
auto-vacuum FULL and INCREMENTAL pointer-map pages; the sparse > 1 GiB lock-byte file
(SQLite `integrity_check` = ok); rejection cases (empty, short header, bad magic, page size 3000
and 256, read version 3, fractions, reserved too large, partial page, in-header size beyond the
file; disagreeing counters fall back to the file size; bad page type; freelist pointing at a
table page); serial-type edge cases (NaN, 10, 11, every integer width, body overrun);
**flipped bytes**: 3000 single-bit flips (half in page headers, half anywhere) of a 6-step recipe
image plus 8 truncations, each fully parsed (census, every cell, every payload, every record)
under `recover`: no panic; 262 of 3000 rejected (the rest land in data bytes the parser does not
interpret). `FuzzParse` (native Go fuzzing, seeds = a 2-step recipe image): ran 30 s / 85,919
inputs with no failure using
`go test -run XXX -fuzz FuzzParse -fuzztime 30s -fuzzminimizetime 0 -parallel 2 ./internal/dbfile/`.
Without `-fuzzminimizetime 0` the fuzzer spends most of its time minimizing the 290 KB seed and
reports 0 execs/sec; that is the minimizer, not a hang.

### internal/equal (level E1; designed for E2 and E3 in chunk 3)

- `Level` (`E1Bytes` = 1; chunk 3 adds `E2Pages`, `E3Logical` as new constants) with `String()`.
- `Result{Level, Equal, SHA256A, SHA256B, SizeA, SizeB, PageSize, FirstDifferingPage,
  DifferingPages}` and `String()` ("E1 bytes: equal (sha256 …)" / "E1 bytes: differ, first
  differing page 3, 2 page(s) differ, sizes … and … bytes"). The `Level` field is in the
  result so E2/E3 results share the type.
- `SHA256Hex`, `ReaderSHA256`, `FileSHA256`, `CompareImages(a, b, pageSize)`,
  `CompareFiles(a, b, pageSize)`. E2 can be added as `CompareImagesIgnoring(a, b, pageSize,
  ranges)` beside these without changing them.
- Manifest: `ManifestEntry{Page, Role, SHA256}`, `Manifest{PageSize, Entries}`. Text format:
  line 1 `sqlite-internals page manifest v1`, line 2 `page-size N`, line 3 `pages N`, then one
  line per page `<number> <role> <sha256>`. `Text()`, `SHA256()` (of the text), `PageCount()`,
  `ParseManifest(r)` (strict: header lines, role `[a-z][a-z0-9-]*`, 64 hex digits, declared
  count = listed count).
- `CheckSequence()` → `*SequenceError{Line, Page, Problem}`: pages must be 1..N in order. It
  names the page: "missing (entry 100 lists page 101)", "listed twice (entries 50 and 51)",
  "out of order (entry 120 should be page 120)". Duplicates are reported before gaps.
- `CompareManifests(a, b) ManifestDiff{Equal, FirstDifferingPage, DifferingPages, Reason}`:
  entry by entry; reasons "page image differs", "role X vs Y", "entry N lists page A vs B",
  "only in the first/second manifest", page-size mismatch.
- The package does not import `dbfile`; roles are plain strings so a manifest can be parsed
  without the parser.

### internal/capture (rung R1; reused by R4 and R6)

- `CheckAtRest(dbPath) error` → `*NotAtRestError` (`errors.Is(err, ErrNotAtRest)`): refuses a
  `-wal` with size > 0 ("-wal holds N bytes of frames not yet checkpointed into the database
  file") and a hot `-journal` ("hot -journal (N bytes with a live header): a transaction was
  interrupted and the next open will roll it back"). See correction 5. Different from
  `recipe.CheckAtRest`, which refuses any side file at all (stricter, used for builds).
- `BuildManifest(*dbfile.File)`: census (must be complete) + sha256 of each page.
- `Pages(dbPath, dir) (Manifest, error)`: at-rest check; parse; refuses a file whose size is not
  page count × page size (a page capture could not rebuild it); writes `pages.bin` (every page
  image in order, each re-hashed against the manifest while writing) and `manifest.txt`; then
  checks again that the database is at rest and that its size and mtime did not change while it
  was read. Limitation: this detects a writer that started during the capture only through those
  checks; it is a tool for closed databases.
- `ReadManifest(dir)`, `Rebuild(dir, outPath) (Manifest, error)`: parse manifest,
  `CheckSequence`, valid page size, refuse if `outPath-wal`, `-journal` or `-shm` exists (SQLite
  would apply it to the rebuilt file), `pages.bin` size must equal pages × page size, every page
  image re-hashed against its manifest line (`*PageError` "capture page 7: image sha256 does not
  match the manifest"), written to `outPath.rebuild-tmp`, fsynced, renamed. On any error the temp
  file is removed and no output exists.
- Tests: capture → rebuild byte-identical, integrity ok, no temp file left; drop / duplicate /
  swap manifest entries, corrupt one page image, short pages file — each refused with the page
  named and no output written; not-at-rest refusals (WAL with content, hot journal) and the
  at-rest exceptions (zero-byte `-wal`, zeroed journal header); stale `-wal` next to the output.

## What each span does (facts read from the copied source)

- **btreeInt.h 1-228 (`BTrees`, `FORMAT`).** The authors' format description. Lines 1-44: a
  b-tree per table and index, cells, overflow. From 45 ("FORMAT DETAILS"): the file is pages
  numbered from 1, page size a power of two 512-65536; the 100-byte header field table with
  offsets; the page header layout (type, first freeblock, cell count, content start, fragmented
  bytes, right child); the cell layouts per page type; the varint encoding; freeblocks; overflow
  pages (4-byte next pointer then data). `SQLITE_FILE_HEADER` "SQLite format 3" is at 248-249
  (outside the span).
- **`newDatabase`, btree.c 3510-3547.** Runs when the first write transaction finds an empty
  file. Writes the magic (3524), the page size as `(pageSize>>8)&0xff` and `(pageSize>>16)&0xff`
  in bytes 16-17 (3526-3527 — that is how 65536 becomes 00 01), versions 1 and 1 (3528-3529;
  WAL sets them to 2 later), reserved bytes (3531), fractions 64/32/32 (3532-3534), zeroes bytes
  24-99 (3535), makes page 1 an empty table leaf for `sqlite_schema` (`zeroPage`, 3536), writes
  the auto-vacuum flags into 52 and 64 (3541-3542), and sets the in-header size byte 31 to 1
  (3545).
- **`lockBtree`, btree.c 3282-3459.** Runs at the first read of a connection. Takes the shared
  lock and reads page 1 (3290-3292). Page count = in-header size, unless it is 0 or bytes 24-27
  differ from 92-95, then the file size (3298-3302). Checks the magic (3314), versions (> 2 read
  version fails; > 2 write version opens read-only; read version 2 opens the WAL and returns to
  re-read page 1 from it, 3319-3355), fractions 64/32/32 (3365), page size (decoded at 3371, checked at 3374-3379: power of
  two, > 256, ≤ 65536), a page size different from the one assumed re-runs with the new size,
  in-header size > file size is corrupt unless `writable_schema` (3405-3412), usable size ≥ 480
  (3416), reads auto-vacuum from 52 and 64. Then computes maxLocal/minLocal (index pages) and
  maxLeaf/minLeaf (table leaves) from the usable size (3441-3444).
- **`decodeFlags`, btree.c 2028-2085.** Maps the page-type byte to the page's properties: 0x0d
  table leaf (intKey, leaf, parse with `btreeParseCellPtr`, local limits maxLeaf/minLeaf), 0x0a
  index leaf, 0x02 index interior (local limits maxLocal/minLocal, `btreeParseCellPtrIndex`),
  0x05 table interior (no payload, `btreeParseCellPtrNoPayload`). Any other value returns
  `SQLITE_CORRUPT_PAGE`. Interior pages get `childPtrSize = 4`.
- **`btreeInitPage`, btree.c 2218-2265.** Reads the page header at offset 100 on page 1 and 0
  elsewhere: calls `decodeFlags(data[0])`, sets where the cell pointer array starts (8 or 12
  bytes after the header start), reads the cell count from bytes 3-4 and fails if it exceeds
  `MX_CELL` ((pageSize−8)/6). Free space is computed lazily (`nFree = -1`); with
  `cell_size_check` on it also checks every cell's bounds.
- **`btreeParseCellPtr`, btree.c 1259-1346.** Table-leaf cell: reads the payload-size varint
  (at most 9 bytes, then masks to 32 bits), the rowid varint (unrolled), sets `nKey`,
  `nPayload`, `pPayload`; if the payload fits (`≤ maxLocal`) the whole cell is local and its size
  is header + payload (minimum 4); otherwise it calls `btreeParseCellAdjustSizeForOverflow`.
- **`btreeParseCellPtrIndex`, btree.c 1347-1385.** The same for index cells (and index interior
  cells after the 4-byte child pointer): payload-size varint, no rowid; the key is the payload.
- **`btreeParseCellAdjustSizeForOverflow`, btree.c 1178-1207.** The split rule: surplus =
  minLocal + (nPayload − minLocal) % (usableSize − 4); local = surplus if ≤ maxLocal, else
  minLocal; cell size = header + local + 4 (the overflow page number). The comment warns that
  changing it changes the file format. The rule keeps the overflow chain's last page as full as
  possible.
- **`sqlite3GetVarint`, util.c 1633-1781.** Decodes a 1-9 byte varint: fast paths for 1 and 2
  bytes, then an unrolled decoder; bytes 1-8 give 7 bits each (high bit = more), the 9th byte
  gives all 8 bits. Returns the number of bytes read.
- **`sqlite3VdbeSerialGet`, vdbeaux.c 4123-4214 (linked, not copied).** Turns one serial type
  plus its bytes into a value: 0 (and 10, 11) NULL; 1-4 big-endian signed 1-4 byte integers;
  5 six-byte; 6 eight-byte integer and 7 float through `serialGet` (a NaN float becomes NULL,
  4103); 8 and 9 the constants 0 and 1; even ≥ 12 blob of (N−12)/2 bytes; odd ≥ 13 text of
  (N−13)/2 bytes.
- **`getOverflowPage`, btree.c 5007-5062.** Returns the page after an overflow page. With
  auto-vacuum it first guesses `ovfl+1` (skipping pointer-map and lock-byte pages) and asks the
  pointer map whether that page's parent is this overflow page (5027-5039), which avoids reading
  the overflow page; otherwise it reads the first 4 bytes of the page (5051).
- **`allocateBtreePage`, btree.c 6514-6822.** Gets a page for a b-tree. If the freelist count at
  page1[36] is non-zero (6535) it decrements it (6572) and takes a page from the freelist
  starting at the trunk in page1[32] (6590): either the trunk itself when it has no leaves
  (6607-6617, page1[32] = the trunk's next pointer) or a leaf (optionally the one nearest to a
  wanted page number, 6693-6710). A reused leaf whose old content is not needed is fetched with
  `PAGER_GET_NOCONTENT` (6740). Otherwise it extends the file by one page, skipping the
  lock-byte page (6778-6779, 6796-6797), and writes the new size into page1[28] (6800).
- **`freePage2`, btree.c 6836-6969.** Puts a page on the freelist: increments page1[36]
  (6861-6862); with `secure_delete` zeroes the whole page (6864-6874); with auto-vacuum records
  it in the pointer map (6879-6880). If a trunk exists and has room it appends the page as a
  leaf (6929-6934) — "room" for the writer is fewer than `usableSize/4 − 8` leaves (6910), while
  readers accept up to `usableSize/4 − 2` (6906), a compatibility margin explained in the
  comment (6912-6923). Otherwise the freed page becomes the new first trunk: next = old trunk,
  leaf count 0, page1[32] = this page (6957-6959). 2.2 C shows the effect: at page size 1024
  full trunks hold 248 = 1024/4 − 8 leaves, and the newest trunk is first in the chain.
- **`pager_write_changecounter`, pager.c 3130-3143 (linked).** Writes `dbFileVers + 1` (the
  counter as last read from page 1) into bytes 24-27 and the same value into 92-95, and
  `SQLITE_VERSION_NUMBER` into 96-99. Unconditional: it does not check whether it already ran.
- **`readDbPage`, pager.c 3067-3120 (linked).** Reads a page from the WAL if it has a frame for
  it (3078-3082), else from the database file; when the page is page 1 it copies bytes 24-39 into
  `pPager->dbFileVers` (3093-3110). This is the only place (besides the rollback-mode write path)
  where the pager learns the current counter, which is why the WAL counter depends on when page
  1 was last read.
- **`pagerWalFrames`, pager.c 3225-3282 (linked).** Writes a commit's dirty pages to the WAL. It
  drops pages beyond the new database size (3253), calls `pager_write_changecounter` only if the
  first page of the (sorted) list is page 1 (3264), then `sqlite3WalFrames` (3265).
- **`pager_incr_changecounter`, pager.c 6363-6437 (linked).** The rollback-mode path: once per
  transaction (`changeCountDone`, 6389, 6426-6429) and only if the database has pages, it makes
  page 1 writable and calls `pager_write_changecounter` (6409); in direct mode (atomic-write
  builds only) it writes page 1 to the file at once. Called from commit phase one. Rollback
  mode's counter therefore advances once per committed write transaction.

## Experiment 2.1 header-and-what-a-commit-changes

Claim (outline, with correction 1): the 100-byte header can be decoded field by field and
matches SQLite; a commit in rollback mode advances bytes 24-27 and 92-95 by one; the in-header
size (28) is trusted only when those two agree; in WAL mode the counter and the file behave
differently, measured exactly.

Run: `go run ./ch02-file-format-and-pages/experiments/2.1-header-and-what-a-commit-changes`
(4.2 s, 18 MB RSS). Evidence `evidence/2.1-header-and-what-a-commit-changes.txt` (197 lines).
Exit 0, last line `HEADER OK`. Portable (no strace); the `.dbinfo` column needs the CLI.

How it reads page 1: "file" = the first 100 bytes of the database file read by a **child
process** (`<self> child-read-header <path>`, prints hex) so the experiment process never opens
the database file while it holds a SQLite connection (outline 4.1 rule). "current" = `SELECT data
FROM sqlite_dbpage WHERE pgno = 1` on the open connection: SQLite's current page 1, WAL included.

How to read the output:

- Line 1 pin line; line 2 the CLI's pin line (or `NO-CLI: … the .dbinfo cross-check is skipped`
  when no pinned CLI is found; the `.dbinfo` column then shows `?`). INFO lines: CLI path, work
  dir.
- **Part A.** `R0: seed=1 steps=30 journal_mode=wal, built to step 30; sha256 24aa158f…` — the
  ch01 R0 hash (`recipe.Build`, one session per step). Then the 23 header fields decoded by
  `dbfile`: offset, size, name, value. Key values: page size 4096; versions 2/2 (WAL); counter
  32; in-header size 270; freelist trunk 230, count 3; schema cookie 4 (step 0 creates three
  tables, step 5 an index — one increment each); schema format 4; user version 30 (the recipe's
  step); application id 1397836120 ("SQIX"); version-valid-for 32; version 3053004 (3.53.4).
  `page count 270, taken from the in-header size (in-header size valid: true)`. Then
  `check ok: opening R0, reading the pragmas and closing left it byte-identical and at rest`.
  Then the table `field / ours / PRAGMA / .dbinfo / match` — `-` means that oracle does not
  report the field; every row `true`. Four checks: all fields equal; bytes 96-99 = 3053004;
  bytes 18/19 = 2/2; counters agree so the in-header size is used.
- **Part B (rollback mode).** Base = rollback recipe to step 27. Steps 28, 29, 30 on one
  connection; after each commit: `check ok: … the file's page 1 equals SQLite's current page 1`
  and the list of changed header fields `bytes A-B name: old -> new (changed bytes …)`. Each
  commit: counter +1 (30→31→32→33), version-valid-for +1, in-header size grows (244→251→259→270),
  freelist trunk/count change as pages are freed and reused, user version +1 (the recipe writes
  it). Then `rollback mode: bytes 24-27 and 92-95 each advance by exactly one per commit, on one
  connection` and `closing the connection changed nothing in page 1`.
- **Part C (WAL).** Base = WAL recipe to step 27, counter 29. C1 one connection: per commit the
  `-wal` size (82432, 156592, 280192 — frames of 24 + 4096 bytes after a 32-byte header), the
  change in the current page 1 since the previous commit, and `file vs base: no header byte
  changed`. Step 28 moves the counter 29→30; steps 29 and 30 change size/freelist/user version
  but not the counter. After close: no `-wal`, and the file has counter 30 with everything else
  from step 30. C2: three single-row updates → `no header byte changed` each time, WAL +4120
  bytes per commit. C3: holder connection + a new writer per step → counter 30, 31, 32 in the
  current page 1, file still equal to the base, and after the holder closes the file has the last
  current page 1 and no `-wal`.
- **Part D (bytes 18-19).** Rollback base at step 3 (18,19 = 1,1). `PRAGMA journal_mode=WAL`
  on a plain driver connection, then close: bytes 18, 19 → 2, counter 6→7, valid-for 6→7 (the
  switch is itself a rollback-mode write transaction on page 1). `PRAGMA journal_mode=DELETE`:
  18, 19 → 1, counter 7→8. So a file's journal mode can be read from bytes 18-19 of a closed file.
- **Part E (in-header size).** Rollback base at step 3: 39 pages. Four edited copies, our page
  count vs SQLite `PRAGMA page_count` (read-only): E1 size 38 + counters agree → both 38 (from the
  in-header size); E2 size 38 + byte 95 changed → both 39 (file size); E3 size 44 + agree → ours
  `error: in-header size larger than the file`, SQLite `database disk image is malformed (11)`;
  E4 size 0 → both 39.
- **Part F (page size bytes).** 512 → `02 00`; 4096 → `10 00`; 65536 → `00 01` (stored 1),
  decoded 65536, SQLite reports 65536.

Outline claims: all confirmed for rollback mode; WAL behaviour as in correction 1.

## Experiment 2.2 page-census

Claim: every page has exactly one role and a Go program can find it from the bytes.

Run: `go run ./ch02-file-format-and-pages/experiments/2.2-page-census` (2.0 s, 20 MB RSS).
Evidence `evidence/2.2-page-census.txt` (388 lines). Exit 0, last line `CENSUS OK`. Portable.
Disk: part E creates a 1 GiB **sparse** file (196,608 bytes allocated here) and deletes it at
the end of the part; on a filesystem without sparse files it would allocate 1 GiB.

How to read the output:

- **Part A.** `page size 4096, usable 4096, page count 270 (from the in-header size)`. The four
  schema rows decoded from page 1 (rowid, type, name, tbl_name, rootpage, sql). The b-trees:
  `sqlite_schema` root 1 (1 page, 4 cells), `accounts` root 2 (3 pages, 184 cells = 183 rows +
  the 1 divider cell of the interior root), `documents` root 3 (70 b-tree pages, 170 overflow
  pages, 164 cells), `events` root 4 (18 pages, 1193 cells), `events_by_account` root 28 (5
  pages, 1177 cells — one per event row). Freelist: trunk 230, leaves 231 and 232. Role counts
  sum to 270; the census is complete. Then 270 page lines: `page role tree ncell payload unused
  mx_pay dbstat-verdict`; b-tree and overflow pages say `match`, freelist pages say `not a
  b-tree page (not in dbstat)` (dbstat lists only b-tree and overflow pages). Checks:
  page_count 270; freelist 3 = `freelist_count`; 267 pages match dbstat on all six columns.
  Overflow chains: first five documents rows with their body size, leaf page and chain (chains
  are not always ascending: `[9 8 23]` — pages reused from the freelist); 83 rows overflow, 170
  overflow pages, longest chain 3; check that the chains account for every overflow page. Row
  hashes: for each recipe table the number of rows and sha256 of the canonical rows, ours /
  SQLite's (183, 96, 1177 rows), equal. Canonical row = `row <rowid>|` then per column
  `<class>:<len>:<text>|` where integer is decimal, real is the 16-hex-digit IEEE bits, text and
  blob are hex; column 0 NULL → the rowid (INTEGER PRIMARY KEY).
- **Part B.** The limits at usable 4096: maxLocal 4061 (table leaf), minLocal 489, index maxLocal
  1002. Three rows: blob 4057 → payload 4061, all local, 0 overflow pages; blob 4058 → payload
  4062, local 489, 1 overflow page (3573 bytes on it); blob 8149 → payload 8153, local 4061, 1
  overflow page holding exactly 4092 bytes. Then census vs SQLite on that file (7 pages).
- **Part C.** Page size 1024 (trunk capacity usable/4 − 2 = 254). 600 rows of 1000-byte blobs
  inserted then deleted: trunks 502 (175 leaves), 252 (248), 3 (248), in chain order; 3 trunks +
  671 leaves = 674 free pages of 676; census vs SQLite.
- **Part D.** `auto_vacuum=FULL`, page size 1024: byte 52 = 3; pointer-map pages 2 and 207
  (every 1024/5 + 1 = 205 pages); census complete (408 pages, 406 match dbstat; the two
  pointer-map pages are not in dbstat).
- **Part E.** A 2-page database at page size 65536 is edited into a 16386-page file: in-header
  size 16386, page 3 a freelist trunk listing pages 4..16386 except 16385, the rest a hole.
  Lock-byte page = 1073741824/65536 + 1 = 16385. Census: 2 table leaves, 1 trunk, 16382 leaves,
  1 lock-byte page = 16386, complete. SQLite (read-only): `integrity_check=ok page_count=16386
  freelist_count=16383`. This answers the outline's "> 1 GiB" case without writing 1 GiB.
- **Part F.** A small rollback-mode file (6 pages: 1 schema leaf, 2 table leaf, 3-4 overflow,
  5 freelist trunk, 6 freelist leaf), nine damaged copies; per case our error and SQLite's
  verdict (`integrity_check` lines joined with ` / `, first four lines; or the open/query error).
  F1 magic → ours not-a-database, SQLite `file is not a database (26)`. F2 half a last page →
  ours truncated, SQLite ok (correction 3). F3 type byte 0x07 → both malformed. F4 cell count
  3000 → both malformed. F5 cell pointer 9 → ours "outside the cell content area 2260..4092",
  SQLite "Offset 9 out of range 2260..4092". F6 payload-size varint 0x7f → ours "2 page(s) have no
  role: [3 4]" (the cell no longer overflows), SQLite "Rowid 21 out of order" plus "never used"
  pages. F7 overflow page 3 → itself → ours "loops back to page 3", SQLite "2nd reference to page
  3". F8 freelist trunk → page 2 → ours "page claimed twice: as table-leaf of "t" and as
  freelist-trunk", SQLite freelist errors and "2nd reference to page 2". F9 freelist count + 1 →
  ours "header counts 3 freelist pages, the trunk chain holds 2", SQLite "Freelist: size is 2
  but should be 3". Every case: our parser returns an error and does not panic.

Outline claims: census sums to page_count — CONFIRMED; none twice / missed — CONFIRMED;
freelist length = freelist_count — CONFIRMED; ncell and unused = dbstat — CONFIRMED (also
payload and mx_payload); row hashes = SQLite — CONFIRMED. Neon's starting list: long freelist
(part C), local limit ± 1 (part B), flipped header byte (part F and the unit tests), > 1 GiB
lock-byte page (part E) are already shown; Neon can attack further.

## Experiment 2.3 page-manifest (rung R1)

Claim: the ordered list of page images is the complete state of a closed database; writing
the pages back rebuilds the file exactly; a database not at rest is refused.

Run: `go run ./ch02-file-format-and-pages/experiments/2.3-page-manifest` (4.9 s, 20 MB RSS).
Evidence `evidence/2.3-page-manifest.txt` (336 lines). Exit 0, last line `MANIFEST OK`.
Portable (child processes are the experiment binary itself).

How to read the output:

- **Part A.** Two builds of R0 in `run-1/` and `run-2/`, each captured with `capture.Pages`:
  270 pages, manifest sha256 `8d9ad6e1…f523` both times. Roles in the manifest. Capture files:
  `pages.bin` 1,105,920 bytes = 270 × 4096, and `manifest.txt`. The full manifest of run 1
  (3 header lines + 270 lines `page role sha256`). `run 1 vs run 2: manifests identical`.
  Freelist pages have distinct hashes (231, 232 are zeroed by `secure_delete`; leaves 231 and 232
  share the sha256 of a zero page `ad7facb2…`).
- **Part B.** `capture.Rebuild` from `run-1/capture` alone into `rebuilt/from-capture.db`;
  `E1 bytes: equal (sha256 24aa158f…)`; `recipe lookup of the rebuild: step 30 (exact bytes)`;
  `integrity_check of the rebuild: ok`; opening/closing the rebuild changed nothing. Then the
  manifest of the recipe after step 29 vs step 30: `first differing page 1 (page image
  differs), 30 page(s) differ` — the manifest comparison names the first differing page.
- **Part C.** Five damaged copies of the capture; each rebuild refused, no output file:
  C1 entry for page 100 dropped → "manifest entry 100: page 100: missing (entry 100 lists page
  101)"; C2 page 50 listed twice → "manifest entry 51: page 50: listed twice (entries 50 and
  51)"; C3 entries 120/121 swapped → "manifest entry 120: page 121: out of order (entry 120 should
  be page 120)"; C4 one byte of page 200's image flipped → "capture page 200: image sha256 does
  not match the manifest"; C5 images of pages 7 and 8 swapped → "capture page 7: …".
- **Part D1 (WAL).** Recipe to step 6; child process (`<self> child-leave-wal <db>`) commits
  `UPDATE accounts SET balance = balance + 1000 WHERE id = 1` and exits with status 3 without
  closing → `-wal 4152 bytes, -shm 32768 bytes`. Capture refused ("-wal holds 4152 bytes of
  frames not yet checkpointed into the database file"). The database file alone: sha256
  unchanged by the commit, account 1 balance 43047. SQLite opening the database (it reads the
  WAL): 44047; after its close no side files. Capture then succeeds (60 pages) and the file
  changed at the checkpoint.
- **Part D2 (rollback).** Rollback recipe to step 6; child (`child-leave-hot-journal`) with
  `cache_size=10` runs `UPDATE documents SET body = zeroblob(length(body))` in one transaction and
  exits before COMMIT → `-journal 203312 bytes`. Capture refused ("hot -journal (203312 bytes with
  a live header) …"). The database file alone: changed by the spill, `17 rows, 15 bodies all zero`
  (a torn state no commit ever produced). SQLite opening the database rolls the journal back:
  `17 rows, 0 bodies all zero`, no side files, and the file is byte-identical to before the
  transaction. Capture then succeeds.

Outline claims: rebuild sha256 = original — CONFIRMED; integrity_check ok — CONFIRMED;
manifests of two runs identical line for line — CONFIRMED; damaged captures name the page —
CONFIRMED; not-closed database refused — CONFIRMED with correction 5's definition.

## Timeline data (chapter section 4: which header bytes change on a commit)

Rollback mode, per committed write transaction (2.1 B, D; source `pager_incr_changecounter`):

| Bytes | Field | Changes when |
| --- | --- | --- |
| 24-27 | change counter | every commit: +1 (`dbFileVers + 1`, once per transaction) |
| 92-95 | version-valid-for | every commit: same value as 24-27 |
| 96-99 | library version | every commit: rewritten with 3053004 (same value here) |
| 28-31 | in-header size | when the file grows or shrinks (`allocateBtreePage` :6800) |
| 32-35, 36-39 | first trunk, freelist count | when pages are freed or reused (`freePage2`, `allocateBtreePage`) |
| 40-43 | schema cookie | on schema change (R0: 4 after 3 CREATE TABLE + 1 CREATE INDEX) |
| 60-63 | user_version | when the application sets it (the recipe: every step) |
| 18-19 | write/read version | on `PRAGMA journal_mode` WAL ↔ rollback (also +1 counter) |

The new page 1 reaches the database file at COMMIT (`pager_write_pagelist`); the journal holds
the old page 1.

WAL mode (2.1 C):
- At COMMIT the database file is not written at all. If page 1 is dirty, its new image is a
  frame in the `-wal`; that image's 24-27/92-95 = (counter when this connection last read page
  1) + 1. Commits that do not dirty page 1 change no header byte anywhere.
- At checkpoint (here: when the last connection closes) the database file's page 1 becomes the
  last page-1 frame.
- Sequence from the evidence, base counter 29: one connection, 3 page-1 commits → frames carry
  30, 30, 30; file stays 29 until close, then 30. Three connections → 30, 31, 32; file 29 until
  the last close, then 32.

## Platform implications (facts)

- The page is the unit: a manifest line per page, 4096 bytes; a WAL frame is 24 + 4096 bytes
  (seen in 2.1 C: +4120 bytes per single-page commit); R0 is 270 pages = 1,105,920 bytes.
- Page size is fixed at creation and stored in bytes 16-17 (65536 stored as 1); SQLite re-reads
  page 1 with the stored size if it opened with another (`lockBtree` :3389-3402).
- Header as a fingerprint: rollback mode — bytes 24-27 change on every commit (cheap "changed?"
  test for a closed file). WAL mode — they do not (correction 1); the file does not change until
  checkpoint; use `PRAGMA data_version` or the WAL. A page capture in WAL mode must happen after a
  checkpoint (or inside a read transaction, rung R4).
- Bytes 96-99 hold the version of the library that last wrote the file (3053004), so a byte hash
  is stable only within one library version (outline; not re-tested here — ruling 5, one version).
- At rest = no `-wal` content and no hot journal (2.3 D). A file copied without its `-wal` loses
  committed transactions (D1: balance 43047 vs 44047); a file copied while a hot journal exists
  is torn (D2: 15 of 17 bodies zeroed by an uncommitted transaction).
- Opening a WAL database with `mode=ro` and closing it leaves the `-wal` (and `-shm`) behind:
  the first run of 2.1 opened R0 read-only and `recipe.CheckAtRest` then failed with
  "r0.db-wal exists". A read-only connection cannot checkpoint and delete the WAL. 2.1 therefore
  reads the pragmas through a normal connection and checks the file is byte-identical
  afterwards. Platform rule: inspect WAL databases with normal connections, or expect side files.
- The census accounts for every byte of a stored file: 270 pages = 3 + 89 + 1 + 4 + 170 + 1 + 2
  by role; a platform can report per-file space by role (overflow pages are 63% of R0).
- Files > 1 GiB contain one page that is never used (the lock-byte page); a page accounting that
  does not know this reports one lost page.
- Freelist pages keep old bytes unless `secure_delete=ON` (the recipe sets it; 2.3's freelist
  leaves hash as zero pages).

## WAL-default evaluation (ruling 10) for this chapter

WAL as the platform default makes sense for chapter 2. Effects found:
1. R1 (page capture) cannot be taken from a WAL database while it has WAL content; the capture
   tool refuses (2.3 D1). A WAL platform must checkpoint (TRUNCATE) or close every connection
   before an R1 capture, or use R4. This is a constraint, not a reason to leave WAL.
2. The header counter is not a change detector in WAL mode (correction 1).
3. Rollback mode is used only where the subject is rollback or the file is synthetic: 2.1 B
   (rollback commit behaviour, as the outline asks), 2.1 D (switching modes), 2.1 E and 2.2 E/F
   (hand-edited files opened read-only by SQLite — a read-only open of a WAL file would leave
   side files, and the edited files must not be repaired by a WAL recovery), 2.3 D2 (hot
   journal). Every recipe/R0 database in this chapter is WAL.

## Linux-only and the Mac route

Nothing in chapter 2 is Linux-only: no strace, no `/proc`. Child processes are the experiment
binary (`trace.SelfCommand`). 2.2 prints disk blocks of the sparse file on an `INFO:` line using
`syscall.Stat_t` (`usage_unix.go`, all unix including darwin; `usage_other.go` prints "disk
usage unknown on this system"). On a Mac: run the same `go run` commands natively (needs a
filesystem with sparse files for 2.2 E — APFS has them — and the pinned CLI only for 2.1's
`.dbinfo` column), or `docker compose run --rm lab go run ./ch02-file-format-and-pages/
experiments/<name>`. darwin: cross-built and cross-vetted only (`agent_work/verify/
crossbuild.sh` passes), never run.

## Working scripts

Indexed in `agent_work/ch02/INDEX.md`: `inspect` (print header, trees, roles, optional per-page
table for any database file) and `build-r0` (build the recipe to step N in a mode).

## Not done / for later chunks

- No prose, no comments (seat rule).
- E2 and E3 in `internal/equal` (chunk 3).
- The parser does not verify key order, cell overlap or pointer-map contents (integrity_check
  territory; chapter 3/9).
- Neon's adversarial list for 2.1 (other page sizes, truncated file, WAL difference) is already
  covered in parts F, E and C; 2.2's list in parts B-F; 2.3's in parts C and D.
