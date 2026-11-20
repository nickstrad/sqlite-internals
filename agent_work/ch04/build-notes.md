# Chunk 4 build notes (plain data for the prose pass)

Builder: Claude Opus 5.5, branch `chunk/ch04`, 2026-11-20. Machine: linux/amd64, Go 1.26.8,
strace 6.8, ext4, 4 cores, 7 GiB. CLI used for the evidence files: the installed `sqlite3` on PATH
(reports the pinned source id; calls `fsync`). Both experiments were also run inside the Debian
`lab` container (`docker compose run --rm -T lab go run ./ch04-pager-and-page-cache/experiments/<name>`),
whose CLI is a `./configure` build of the pin that calls `fdatasync`: EXACT output (all non-`INFO:`
lines) identical to the host's for both experiments. darwin: cross-built and cross-vetted only,
never run.

All source line numbers are at the pin (3.53.4). `go run ./cmd/course check spans ch04` passes
(44 spans); `check sources ch04` passes (4 files: `pager.c`, `pcache.c`, `pcache1.c`, `status.c`,
copied whole from `$SQLITE_SRC/src/`). Every span the outline's reading path gives was exact
(start line has the symbol, end line is the closing brace or the end of the comment); nothing had
to be fixed. Spans were added for every function the experiments' output names (list below).

## Corrections to the outline

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
6. **Process memory is about twice the summed `CACHE_USED` with this driver.** 4.2 part B: RSS
   growth at 64 connections 252.9 MiB for 128.3 MiB of summed `CACHE_USED` (ratio 1.97, INFO line;
   ratio stable across runs). Mechanism: modernc's C library allocates through
   `modernc.org/memory` v1.12.1, whose allocator uses power-of-two slots up to 16 KiB
   (`maxSlotSizeLog = pageSizeLog - 2` with `pageSizeLog = 16`; slot `log =
   bits.Len(roundup(size, 16) - 1)`), so each cache page allocation of about 4.3 KiB (4096-byte page
   + PgHdr1 + extra) takes an 8 KiB slot. This is a property of the driver, not of SQLite; with the
   C library (glibc malloc) the overhead would be small. The check in 4.2 only claims "RSS grows by
   at least the summed CACHE_USED".
7. **`CACHE_USED` depends on the database's path length.** `sqlite3PagerMemUsed` adds
   `sqlite3MallocSize(pPager)`, and the Pager allocation holds the database, journal and WAL file
   names (`pager.c:6881-6887`). Running 4.2 inside the container (paths `/work/4.2/…`) gave
   `CACHE_USED` 126 bytes smaller per connection than on the host. The experiment therefore prints
   cached pages (path independent) on EXACT lines and raw bytes on INFO lines. Per cached page
   `CACHE_USED` counts 4352 bytes (4096 + 256 of headers; measured from cache_size 200 minus 100);
   the fixed part was 5077 bytes on the host (Pager allocation 981 bytes + one 4096-byte buffer).
8. **A cache of N pages holds N−1.** `cache_size=100` held 99 pages and `cache_size=200` held 199
   (4.2 part A). `pcache1FetchStage2` recycles once `nPage+1 >= nMax` (`pcache1.c:899-901`).
   `cache_size=-2000` gives `nMax` = 2048000 / (4096 + szExtra) = 483 → 482 pages held.

Nothing else in the chapter-4 section of the outline was false here. The two failure cases
("Astra will try" items 1-2) and `synchronous=OFF` (item 3) are demonstrated inside 4.1 as the brief
asked.

## Shared packages

### New: `internal/journalfile` (read-only rollback-journal inspector; no SQLite)

Files: `journalfile.go` (about 330 lines), `journalfile_test.go` (about 330 lines). It decodes a
`-journal` for display. It is NOT a replayer: it never writes, and SQLite does every rollback in
the experiments. `Playback()` only predicts which pages a hot rollback would restore, following
the reading rules of `pager_playback` / `pager_playback_one_page`, so the experiment can print
what SQLite is about to do before it reopens the file.

- `Magic` = `d9 d5 05 f9 20 a1 63 d7` (`aJournalMagic`, pager.c:757). Constants:
  `HeaderFieldsSize` 28, `NoSyncRecordCount` 0xffffffff, `RecordOverhead` 8.
- `HeaderState`: `synced` (magic present, record count N), `no-sync` (magic present, count
  0xffffffff), `not-yet-synced` (first 12 bytes zero, rest of the header present — written by
  `writeJournalHdr` in sync mode before `syncJournal`), `zeroed` (all 28 bytes zero — PERSIST
  mode after commit, `zeroJournalHdr`), `invalid`, `missing` (file shorter than 28 bytes).
- `Header{Offset, State, MagicOK, RecordCount, Nonce, InitialPages, SectorSize, PageSize}` — the
  six fields of the journal header in order: magic (8), record count (4), nonce = checksum
  initializer (4), database size in pages before the transaction (4), sector size (4), page size
  (4); all big-endian. `CountsRecords()`: magic present and count ≠ 0xffffffff.
- `Record{Offset, Page, StoredChecksum, ComputedChecksum, Image}`; `ChecksumOK()`. A record is
  page number (4) + page image (page size) + checksum (4).
- `Checksum(nonce, image)`: nonce plus every 200th byte counting down from `pageSize-200` while
  the index is > 0 (`pager_cksum`, pager.c:2251-2259). For a 4096-byte page it samples bytes
  3896, 3696, …, 96 (20 bytes). Test `TestChecksumSamplesEvery200thByteFromTheEnd`.
- `Parse(data)` / `ReadFile(path)` → `Journal{Size, FirstByte, Segments, SuperJournal,
  TrailingByte}`. Layout rules: the first header is at 0; geometry (sector size, page size) comes
  from the first header and must be a power of two in range (same test as `readJournalHdr`,
  pager.c:1650-1672); records start one sector after the header; a header that counts N records
  owns exactly N records and the next header starts at the next sector boundary
  (`journalHdrOffset`, pager.c:1366-1376); a no-sync or not-yet-synced header owns every complete
  record to the end of the file. Only complete records are listed; `TrailingByte` counts the bytes
  of an incomplete last record. A super-journal name at the end (4-byte length, 4-byte sum, magic)
  is detected and reported.
- `LooksHot()`: size > 0 and first byte ≠ 0 — the half of `hasHotJournal` (pager.c:5180-5271)
  that depends on the journal's bytes. (The other half — no other process holds RESERVED, and the
  database has more than 0 pages — depends on locks and the database file.)
- `Playback()` → `PlaybackOutcome{Hot, TruncateTo, Pages, SkippedPages, StopReason}`: not hot →
  nothing; first header not `synced`/`no-sync` → nothing (`readJournalHdr` returns DONE on a magic
  mismatch); otherwise per segment the counted records (`no-sync`: all complete records); per
  record, in `pager_playback_one_page` order: page 0 or the super-journal page number
  (`PENDING_BYTE/pageSize + 1` = 262145 for 4096) → stop; page > initial size → skipped; bad
  checksum → stop; else restored. A counted record that is not complete → stop (SQLite's short
  read). `TruncateTo` = the first header's initial size (`pager_truncate` at pager.c:2952).
- Tests (`go test ./internal/journalfile/`, 0.1 s): synthetic journals for every state, multiple
  segments, super-journal name, invalid geometry, the skip/stop rules; and a live test that opens a
  rollback-mode database, runs an UPDATE+INSERT inside an open transaction and reads the
  `-journal` before COMMIT, at FULL (state `not-yet-synced`, not hot) and OFF (state `no-sync`,
  playback lists every record), checking that every record's image equals that page in a copy of
  the file taken before the transaction (reading the journal while the connection is open is safe:
  SQLite holds no POSIX lock on the journal, only on the database file).

### Changes to existing shared packages

None. `sqlitex`, `trace`, `recipe` and `dbfile` are used as they are (`dbfile.FromBytes` +
`Census().FreelistLeaves` for the freelist list; `recipe.StepHashes`, `MatchHashes`, `HashFile`;
`trace.Run` with `KillAt`; `sqlitex.ReadDBStatus`). go.mod unchanged.

## Experiment 4.1 atomic-commit-and-hot-journal

Run: `go run ./ch04-pager-and-page-cache/experiments/4.1-atomic-commit-and-hot-journal`
(flags: `-step 3`, `-exact-step 1` (0 skips part D2), `-workers 4`). Runtime about 14-15 s here
(309 strace'd child runs, 4 in parallel). Evidence: `evidence/4.1-atomic-commit-and-hot-journal.txt`
(1,337 lines), exit 0, last line `ATOMIC COMMIT OK`. EXACT; identical on two consecutive runs and in
the container. `INFO:` lines: work dir, sync syscall names, per-kill journal nonce (random per
run: `sqlite3_randomness`, pager.c:1523) and run time, runtime.

### How it works

- The program re-runs itself as a child (`child-step <dir> <journal_mode> <synchronous> <k>`). The
  child opens `t.db` with the recipe's options (rollback journal, `secure_delete=ON`,
  `cache_size=-2000`, …) and the requested `synchronous`, reads an empty file `marker-begin`,
  applies recipe step k in one transaction (`recipe.ApplyStep`: `BEGIN IMMEDIATE` through the
  sqlitex `_txlock=immediate` DSN, the statements, `PRAGMA user_version = k`, COMMIT), reads
  `marker-end`, closes. `runtime.LockOSThread()` runs in `init`, so the whole child runs on one OS
  thread and strace's per-thread syscall counter sees the SQLite calls in order.
- Baseline: the child runs under strace (`trace.Run`) on a fresh copy of the pre-step file. The
  "window" is every syscall of the child's thread between opening `marker-begin` and opening
  `marker-end`, on files in the work directory, marker files excluded. Each call gets a kind
  (`journal-header` = pwrite at journal offset 0 with length 512; `journal-count` = pwrite at 0
  with length 12; `journal-zero-header` = pwrite at 0 with length 28; `journal-record`;
  `db-page`; `journal-sync`, `dir-sync`, `db-sync`; `journal-unlink`; `journal-truncate`; `other`)
  and its ordinal among calls of the same name in that thread since the process started.
- Kill points: every `pwrite64`, `fsync`/`fdatasync`, `unlink` and `ftruncate` in the window, plus
  the `open` of `marker-end` (the first syscall after the step returned). For each, a fresh copy of
  the pre-step file and `strace -e inject=<name>:signal=SIGKILL:when=<ordinal>` (`trace.KillAt`).
  The run is accepted only if strace reports the child killed by SIGKILL and the unfinished call
  is exactly the intended one (same name, file, offset, length) — check line `every kill landed on
  the intended syscall`.
- After each kill, BEFORE reopening, the parent (which has no SQLite connection to that file)
  reads the database file and the `-journal` with plain Go: the database is classified as
  `pre-step bytes`, `post-step bytes` or `torn: P pages, X differ from pre-step, Y differ from
  post-step`; the journal is decoded with `internal/journalfile`; the predicted SQLite action is
  printed. Then it reopens with `sqlitex.Open` (the first read takes the shared lock and runs
  `sqlite3PagerSharedLock` → `hasHotJournal` → `pager_playback`), runs `PRAGMA integrity_check`,
  `VACUUM INTO` a scratch file (canonical hash), closes, and classifies the result.
- Outcome classes: `pre-step, exact bytes`; `pre-step except page(s) [..], which are freelist
  leaves in the pre-step file (unused bytes); VACUUM INTO output equals the pre-step's` (same size,
  every differing page in the pre-step freelist-leaf list, canonical hash equal); `post-step, exact
  bytes`; anything else is "a third state" (never happened).
- The pre-step and post-step files are built by `recipe.Build` (one session per step, rollback
  mode); the check line confirms they equal the recipe's cached hashes for steps k-1 and k.
- Twin check (verbose levels only, needs the CLI): the same step is rendered as literal SQL
  (integers, `'text'`, `X'hex'` blobs) and run by the pinned CLI under strace with
  `.read marker-begin` / `.read marker-end` around `BEGIN IMMEDIATE; …; COMMIT;`. The list of
  writes, syncs and unlinks (sync syscall name normalized to `sync`) must be identical to the Go
  driver's and the file must equal the post-step bytes.

### What it printed, section by section, and how to read it

**Part A, where a WAL-default platform meets rollback mode.**
- `a new file opened by the driver with no settings reports journal_mode=delete` — plain
  `sql.Open("sqlite", path)`, no pragmas.
- `after its first CREATE TABLE: header bytes 18,19 = 1,1` — write/read format version 1 = rollback.
- `after PRAGMA journal_mode=wal: header bytes 18,19 = 2,2; a fresh connection reports
  journal_mode=wal` — WAL is stored in the file; rollback modes are not (ch00: per connection).
- Three straced children, journal syscalls per marked segment (Linux only):
  `CREATE TABLE (driver defaults, first write): -journal [open plain.db-journal pwrite64
  plain.db-journal off=0 len=512 sync plain.db-journal pwrite64 plain.db-journal off=0 len=12 sync
  plain.db-journal unlink plain.db-journal]; -wal writes 0`; `PRAGMA journal_mode=wal on the new
  empty file: -journal [same six calls on switch.db-journal]; -wal writes 0`; `CREATE TABLE after
  the switch: -journal []; -wal writes 5`; `sqlitex.Open of a new file with journal_mode=wal:
  -journal [same six calls]; -wal writes 5` (the 5 WAL writes there are sqlitex's
  application_id/user_version commit); `CREATE TABLE after it: -journal []; -wal writes 4`.
  The journals here hold no records: on a 0-page file page 1 is beyond the original size, so it is
  not journaled (`pager_write`, pager.c:6139-6147); the journal is still opened, given a header,
  synced and deleted. Four check lines (correction 4).

**Part B, the recipe step.** `recipe: seed=1 steps=30 journal_mode=delete`; `step 3: schema=0
inserts=53 updates=5 deletes=5 indexes=0`; `pre-step state (after step 2): 29 pages, sha256
b8a0bd0796442099, freelist leaves [22]`; `post-step state (after step 3): 39 pages, sha256
0ef90bd0e64af1e3`.

**Part C, the journal of an open transaction (portable; runs on macOS too).** All statements of
step 3 executed on a live connection, COMMIT not issued, then the `-journal` read:
`-journal 29240 bytes, first byte 0x00 (not hot), header not-yet-synced (magic and record count
still zero), initial size 29 pages, sector 512, page 4096, 7 complete record(s) [2 1 20 3 27 4 26],
0 checksum failure(s)`. Then one line per record (`record at offset 512: page 2 …, checksum ok,
image equals the pre-step page`; offsets 512, 4616, 8720, … step 4104 = 4 + 4096 + 4). Reading:
29240 = 512 (header sector) + 7 × 4104. The records are in the order the pages were first
modified: page 2 (accounts table leaf), page 1 (header — the change counter and freelist/user
version changes, journaled at commit time by `pager_incr_changecounter` or earlier when page 1
changes), then the others. Page 22 (freelist leaf reused by the step) is NOT in the journal. The
sector size is 512, not the compile-time 4096 default, because the unix VFS reports
POWERSAFE_OVERWRITE and the pager then uses 512 (`setSectorSize`, pager.c; not in SPANS). After
ROLLBACK: `pre-step exact bytes`, no `-journal`.

**Part D, step 3 at FULL, NORMAL and OFF.** For each level:
1. The numbered syscall list of the commit (70 calls at FULL). Read it as the timeline (next
   section).
2. `sequence:` the collapsed kinds (correction 2), `journaled pages 7 (21 record writes), database
   page writes 18, syncs 4 [t.db-journal . t.db-journal t.db]` (NORMAL: `syncs 3 [t.db-journal .
   t.db]`; OFF: `syncs 0 []`).
3. Twin check: `the C CLI running the same step as literal SQL makes 46 writes, syncs and unlinks,
   identical to the Go driver's` (NORMAL 45, OFF 41).
4. `kill points: 47` (NORMAL 46, OFF 42). Then per kill point five lines:
   `kill NN on entry to <syscall> [kind]`; `on disk: database …`; the journal summary; `SQLite on
   reopen …` (prediction from the inspector); `reopened: <outcome>; integrity_check ok; <journal
   after reopen>`.
5. `outcomes: 32 pre-step exact, 14 pre-step except reused freelist leaves, 1 post-step exact, 0
   other` (NORMAL 31/14/1, OFF 28/13/1) and six check lines.

Key kill points at FULL (numbers are kill indexes in the evidence):
- 1 (journal header write): journal 0 bytes, not hot → pre exact, a 0-byte cold journal is left.
- 2-22 (record writes): journal grows record by record, header still `not-yet-synced` → SQLite
  ignores it; database file untouched → pre exact; cold journal left.
- 23 (journal sync), 24 (directory sync), 25 (header count write): all records complete, header
  still zero → not hot → pre exact, cold journal left.
- 26 (second journal sync): header now `synced (record count 7)`, first byte 0xd9 → hot; database
  still untouched; rollback rewrites the 7 original pages (identical) → pre exact, journal deleted.
- 27-44 (database page writes, ascending offsets 0, 4096, 8192, 12288, 77824 (page 20), 86016
  (page 22), …, 155648 (page 39)): torn database, hot journal → rollback; 27-32 pre exact; 33-44
  pre except page 22 (kill 32 is on entry to the write of page 22; from kill 33 on that write has
  completed, and the rollback cannot undo it).
- 45 (database sync): database file is already the post-step bytes, journal hot → rolled back to
  pre (except page 22).
- 46 (journal unlink): same as 45 — the unlink had not happened → rolled back.
- 47 (`open marker-end`, the step has returned): no journal → post exact.
The flip from B/b to A happens between kill 46 (entry to `unlink t.db-journal`) and kill 47.
That unlink is the commit point.

NORMAL differs only in order: kills 23 (`journal-count`) is the first point where the header has
its magic; it is written before the only journal sync, so at kill 24 (journal sync) the journal is
already hot. OFF: the header has magic and 0xffffffff from the first write, so from kill 2 on the
journal is hot (`hot, but restores no page (end of journal)` until the first record is complete),
and there is no sync anywhere.

Summary lines (EXACT):
```
synchronous=FULL   BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBbbbbbbbbbbbbbbA
synchronous=NORMAL BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBbbbbbbbbbbbbbbA
synchronous=OFF    BBBBBBBBBBBBBBBBBBBBBBBBBBBBbbbbbbbbbbbbbA
```

**Part D2, step 1 (pre-step has no freelist).** Compact per level (no per-kill dump): sequence
`journal-header, journal-record x12, journal-sync, dir-sync, journal-count, journal-sync, db-page
x22, db-sync, journal-unlink` (FULL), 4 journaled pages, 22 database writes (pre-step 4 pages →
post-step 22 pages; 18 of the writes are appended pages, not journaled, removed by the rollback's
truncation to 4 pages). 42 / 41 / 37 kill points; every one reopens to exactly pre or post.
Summary: `BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBA` (FULL), 40 B + A (NORMAL), 36 B + A (OFF).

**Part E, failure cases (step 3, FULL).** Expected contents line: pre-step `user_version=2
accounts=15 documents=7 events=108`; post-step `user_version=3 accounts=21 documents=10
events=147`. Crash state: killed on entry to `pwrite64 t.db off=122880 len=4096` (database write
10 of 18): `database torn: 30 pages, 9 differ from pre-step, 9 differ from post-step`, journal hot.
1. Journal deleted, reopened: `open failed: open t.db: PRAGMA page_count: database disk image is
   malformed (11)` — state `unreadable`. The torn file cannot even be opened by sqlitex (its first
   read fails). Check: neither pre nor post.
2. The database file copied alone to another directory and opened: the same error, same state.
   The same crashed bytes copied together with their `-journal`: `integrity_check: ok;
   user_version=2 accounts=15 documents=7 events=108`, state pre-step (except page 22). So the
   `-journal` is part of the database: copy both or neither.
3. Killed on entry to `sync t.db` (every database page written): database = post-step bytes;
   journal deleted, reopened: `integrity_check: ok; user_version=3 … events=147`, state post-step
   exact. A commit that never returned to the application (the process died inside COMMIT) became
   durable-looking and committed; with the journal present it would have been rolled back. Losing
   a journal is silent corruption in one case and a silent wrong outcome in the other.

**Part F, the commit point by journal mode (step 3, FULL).**
```
mode      commit point (syscall)             killed on entry            killed on the next syscall                   -journal after a clean commit
delete    unlink t.db-journal                before (pre-step, logical) after (post-step) (kill on step-returned)    no -journal
truncate  ftruncate t.db-journal len=0       before (pre-step, logical) after (post-step) (kill on journal-sync)     -journal 0 bytes (not hot)
persist   pwrite64 t.db-journal off=0 len=28 before (pre-step, logical) after (post-step) (kill on journal-sync)     -journal 29240 bytes, first byte 0x00 (not hot), header zeroed
```
"pre-step, logical" = pre-step except the reused freelist page 22. Sequences: TRUNCATE and PERSIST
are DELETE's sequence with the last item replaced by `journal-truncate, journal-sync` or
`journal-zero-header, journal-sync` (5 syncs per commit at FULL: the extra one after finalizing,
`pager_end_transaction` pager.c:2086-2104; for TRUNCATE the sync is only done when `fullSync`).
Brief said include only if cheap: it cost 9 extra child runs (about 2 s).

### Claims confirmed / corrected

- In rollback mode the original page images are written to the journal, and the journal is synced
  (FULL, NORMAL), before any change reaches the database file: CONFIRMED (every journal record
  precedes the first `db-page`; part C shows the records equal the original pages).
- The commit point is the removal of the journal: CONFIRMED in DELETE mode (flip between the kill
  on `unlink` and the next one); TRUNCATE: the `ftruncate` to 0; PERSIST: the 28-byte zero write.
- A process killed anywhere leaves a database SQLite restores to exactly before or after:
  CONFIRMED logically everywhere and byte-exactly when no freelist page is reused; CORRECTED at the
  byte level (correction 1).
- fsync counts 3 (NORMAL) and 4 (FULL): CONFIRMED; OFF: 0. The claim still holds at OFF for a
  process kill (every OFF kill reopens to pre or post).

## Experiment 4.2 cache-budget

Run: `go run ./ch04-pager-and-page-cache/experiments/4.2-cache-budget` (flags: `-n 64`, `-k 8`,
`-cache-size -2000`, `-rows 1000`). Runtime about 3 s; peak about 260 MiB RSS in the unbounded
child. Evidence: `evidence/4.2-cache-budget.txt`, exit 0, last line `CACHE BUDGET OK`. EXACT lines
identical on three consecutive runs and in the container; RSS values are on `INFO:` lines (BOUND
and SHAPE checks use them). Also run with `-n 5 -k 2` and `-n 200 -k 8`: `CACHE BUDGET OK`. The
1,000-connection case is left to the adversarial pass (`-n 1000`; expect about 4 GB RSS with this
driver because of correction 6 — too much for a shared 7 GiB machine, so not run here).

Setup: one WAL-mode database (the platform default) built with sqlitex defaults: table
`t(id INTEGER PRIMARY KEY, body BLOB NOT NULL)`, 1000 rows of 3500 PRNG bytes (one row per leaf
page), 1004 pages, 4,112,384 bytes; copied N times (`_work/4.2/f-000.db` …). Every connection:
sqlitex defaults (`journal_mode=wal`, `synchronous=FULL`) with `cache_size=-2000` (2,048,000 bytes).
Scan: `SELECT count(*), sum(length(body)) FROM t` (touches every leaf page). Every scenario runs in
its own child process (the binary re-run with `child <role> …`) so each starts from a fresh
process; the child prints one JSON checkpoint per measurement (per-connection `CACHE_USED`, hit,
miss, spill from `sqlite3_db_status`, plus process memory after `runtime.GC()` and
`debug.FreeOSMemory()`).

Process memory: Linux reads `/proc/self/status` (`VmRSS`, `RssAnon`, `RssFile`, `RssShmem`) in
`memory_linux.go`; other systems run `ps -o rss= -p <pid>` (total RSS only) in `memory_other.go`.

What it printed and how to read it:
- Setup: `driver compiled with ENABLE_MEMORY_MANAGEMENT: 1` (correction 5).
- A: `CACHE_USED counts 4352 bytes per cached page …`; `cache_size=100 holds 99 pages,
  cache_size=200 holds 199 pages` (correction 8). `cache_size=-2000`: after scan 1 `cached
  pages=482 hit=7 miss=1005 spill=0`, after scan 2 `hit=9 miss=2007`: a file larger than the cache
  misses every page again (LRU). `cache_size=-8032` (twice the file): 1004 pages cached, scan 2
  `hit=1011 miss=1005` (no new misses). Cached pages = (CACHE_USED − 4096) / 4352, rounded down;
  valid while the Pager allocation is under 4352 bytes (paths shorter than about 1 KB).
- B (64 files): table of checkpoints after 1, 2, 4, …, 64 open connections: sum of cached pages
  482, 964, 1928, …, 30848 = open × 482 exactly; per-connection min..max 0..1004 from 4 on.
  INFO lines show RSS (12.9 MiB after 1 → 260 MiB after 64; anon grows; file-backed grows about 3 MiB,
  mostly the `-shm` mappings, 32 KiB per WAL database = 2 MiB for 64). Checks: sum exact; not a per-connection cap
  (correction 5); RSS growth ≥ summed CACHE_USED; growth per open connection at 64 within
  −20 %/+25 % of that at 32 (measured 4.0 MiB per connection at both, INFO line).
- C (64 connections to ONE file): identical page table to B. Connections to the same file do not
  share a page cache (shared-cache mode is off); memory is per connection, not per file.
- D (pool of 8, LRU close, 64 files visited once each): page sum 3856 at 8, then 3863 at 16, 32,
  64 (7 pages above 8 × 482 after the pool starts closing connections; not traced further; within
  the 1 % allowance). INFO: RSS growth 34.3 MiB at 8 and 35.0 MiB at 64 (flat). Checks: 8 open at
  the end; growth after K at most a quarter of the growth up to K; pooled growth at most 1.5 ×
  (8/64) of the unbounded growth (measured 0.14).
- E (mmap): connection 1 without mmap scans f-000 (482 pages, miss=1005); connection 2 with
  `PRAGMA mmap_size=8224768` (twice the file) scans f-001: `cached pages=2 hit=7 miss=3`. INFO: RSS
  +4.2 MiB, file-backed +4.0 MiB, anonymous +0.3 MiB. Checks: CACHE_USED under 5 % of the unmapped
  connection's; RSS still grows by ≥ 0.8 × file size; the growth is file-backed. With mmap the
  pager reads pages through `getPageMMap` → `sqlite3OsFetch` (`unixFetch` → `unixMapfile`); the
  page objects come from `pagerAcquireMapPage`, outside the page cache, so `CACHE_USED` does not
  count them; the kernel's page cache (shared between processes) holds the data.

Claims: per-connection memory bounded by `cache_size` — CORRECTED (correction 5, process-wide
bound); total grows linearly with open connections unless bounded — CONFIRMED (B, C); stays flat
past K when bounded — CONFIRMED (D); with mmap `CACHE_USED` falls and the memory appears as mapped
file pages — CONFIRMED (E).

## What each span does (facts read from the copied source)

pager.c:
- **135-350 state comment (`eState`).** Defines the seven pager states OPEN, READER,
  WRITER_LOCKED, WRITER_CACHEMOD, WRITER_DBMOD, WRITER_FINISHED, ERROR, the diagram, and which
  function performs each transition (OPEN→READER `sqlite3PagerSharedLock`, READER→WRITER_LOCKED
  `sqlite3PagerBegin`, →WRITER_CACHEMOD `pager_open_journal`, →WRITER_DBMOD `syncJournal`,
  →WRITER_FINISHED `sqlite3PagerCommitPhaseOne`, any writer state→READER `pager_end_transaction`).
  For each state it lists the invariants: e.g. in WRITER_CACHEMOD the journal is open and pages
  may be modified in the cache but the database file has not been written.
- **351-357 `PAGER_OPEN`…`PAGER_ERROR`.** The seven state constants 0-6.
- **360-406 lock comment (`eLock`).** `eLock` mirrors the lock held on the database file:
  NO_LOCK, SHARED, RESERVED, EXCLUSIVE, plus UNKNOWN_LOCK after a failed unlock, which forces
  the next reader to recheck for a hot journal.
- **1366-1376 `journalHdrOffset`.** Rounds the current journal offset up to the next multiple of
  the sector size: every journal header starts on a sector boundary.
- **1399-1432 `zeroJournalHdr`.** PERSIST mode's commit: overwrite the first 28 bytes of the
  journal with zeros (or truncate to 0 when asked or when `journal_size_limit` is 0), sync unless
  `noSync`, then truncate to `journal_size_limit` if the journal is larger.
- **1449-1571 `writeJournalHdr`.** Builds the header in a buffer: magic + record count
  0xffffffff when `noSync` / MEMORY / SAFE_APPEND, otherwise those 12 bytes zero; a fresh random
  nonce (`sqlite3_randomness`); `dbOrigSize`; sector size; page size; zero padding; writes it at
  the next header offset.
- **1590-1693 `readJournalHdr`.** Reads the header at the next sector boundary; returns DONE if the
  file is too short or (for a hot journal) the magic does not match; reads count, nonce, initial
  size; from the first header also sector and page size, and stops (DONE) if either is not a
  power of two in range — the sign of a crash before the header was synced.
- **2052-2180 `pager_end_transaction`.** Finishes a write transaction by journal mode: TRUNCATE →
  truncate to 0 (+ sync if `fullSync`); PERSIST (or exclusive locking) → `zeroJournalHdr`; DELETE
  → close and `sqlite3OsDelete` the journal. Then clears the in-journal bitmap, marks cache pages
  clean, truncates the file if it shrank, sends `SQLITE_FCNTL_COMMIT_PHASETWO`, and drops back to
  a SHARED lock and the READER state.
- **2251-2259 `pager_cksum`.** The record checksum: nonce + every 200th byte of the page, counting
  down from `pageSize-200`.
- **2298-2500 `pager_playback_one_page`.** Reads one record (page number, page, checksum); page 0
  or the super-journal page → DONE; page beyond the database size → skip; bad checksum → DONE
  (rest of the journal ignored); otherwise writes the original page back to the database file
  (and to the cache if the page is there).
- **2864-3052 `pager_playback`.** Rolls back a journal: checks for a super-journal (if named but
  missing, the journal is not hot); loops over headers (`readJournalHdr`); a 0xffffffff count
  means "all records to end of file"; on the first header truncates the database to its original
  size; plays each record back; then syncs the database, finishes with `pager_end_transaction`
  (which deletes the journal in DELETE mode) and logs `SQLITE_NOTICE_RECOVER_ROLLBACK`.
- **4094-4103 `pagerSyncHotJournal`.** Before playing back a hot journal left by another
  process, syncs it (unless `noSync`) and records its size, so playback works from durable bytes.
- **4332-4441 `syncJournal`.** The journal half of a commit: takes EXCLUSIVE, writes a zero byte
  over a stale header that may follow (left by PERSIST mode), in FULL mode syncs the journal, then
  writes magic + record count into the header (12 bytes at offset 0), then syncs again (the only
  sync in NORMAL); state → WRITER_DBMOD. Seen in 4.1 as `pread64 t.db-journal off=29696 len=8 = 0`
  (the stale-header probe), `sync`, `pwrite64 off=0 len=12`, `sync`.
- **4475-4556 `pager_write_pagelist`.** Writes each dirty page to `(pgno-1)*pageSize` of the
  database file in list order (the list is sorted by page number, so offsets ascend), updates the
  change counter on page 1, counts each write in `aStat[PAGER_STAT_WRITE]`, and feeds the backup
  API.
- **4655-4727 `pagerStress`.** Called by the page cache when it needs to evict a dirty page in the
  middle of a transaction (a spill): counts a SPILL, in rollback mode syncs the journal first if
  needed (`syncJournal(pPager, 1)`, which also starts a new journal header) and writes the page to
  the database file early. Not triggered by 4.1's step (spill=0); this is how a transaction larger
  than the cache reaches the file before COMMIT, and why a journal can have several headers.
- **5180-5271 `hasHotJournal`.** A journal is hot when it exists, no process holds RESERVED on the
  database, the database has more than 0 pages, and the journal's first byte is non-zero. A
  journal beside a 0-page database is deleted instead.
- **5300-5507 `sqlite3PagerSharedLock`.** OPEN → READER: takes SHARED; if `hasHotJournal`, takes
  EXCLUSIVE directly (never RESERVED, so other readers cannot slip in), opens the journal, syncs it
  (`pagerSyncHotJournal`) and calls `pager_playback`; then checks the 16 bytes at offset 24
  (change counter etc.) to decide whether its cache is still valid; opens the WAL if one exists.
- **5581-5683 `getPageNormal`.** Fetch from the cache; a hit increments HIT; a miss allocates a
  page (calling `sqlite3PcacheFetchStress` if the cache is full) and reads it from the file
  (MISS). With `PAGER_GET_NOCONTENT` it zeroes the page instead of reading it and marks it as
  already journaled (5645-5656) — the source of correction 1.
- **5687-5752 `getPageMMap`.** With mmap enabled and no WAL frame for the page, gets a pointer
  into the mapping (`sqlite3OsFetch`) and wraps it in a page object (`pagerAcquireMapPage`)
  instead of copying into the cache.
- **5877-5949 `pager_open_journal`.** On the first write of a transaction: creates the in-journal
  bitmap, opens the journal file (`SQLITE_OPEN_MAIN_JOURNAL`, or an in-memory journal for
  journal_mode=MEMORY), writes the first header; state → WRITER_CACHEMOD.
- **5968-6032 `sqlite3PagerBegin`.** READER → WRITER_LOCKED: in rollback mode takes RESERVED (and
  EXCLUSIVE at once if asked); in WAL mode takes the WAL write lock; records the original size
  `dbOrigSize`. In 4.1 this is `fcntl t.db F_SETLK F_WRLCK start=1073741825 len=1` (call 5).
- **6037-6085 `pagerAddPageToRollbackJournal`.** Writes one record: page number (4 bytes),
  original page (page size), checksum (4); marks the page NEED_SYNC; advances the journal offset
  by `pageSize+8`; sets the page's bit in `pInJournal`. The three `pwrite64` calls per record in
  4.1.
- **6094-6173 `pager_write`.** Makes a page writable: opens the journal if needed, marks the page
  dirty, and journals its original image if it is within the original size and not already in
  the journal; pages beyond the original size are only marked NEED_SYNC (they are removed by
  truncation on rollback).
- **6446-6456 `sqlite3PagerSync`.** Sends `SQLITE_FCNTL_SYNC` to the VFS, then syncs the database
  file unless `noSync` (the `db-sync`).
- **6511-6730 `sqlite3PagerCommitPhaseOne`.** The commit up to the commit point: in WAL mode
  writes frames; in rollback mode increments the change counter (journaling page 1), writes a
  super-journal name if needed, `syncJournal`, `pager_write_pagelist`, extends or truncates the
  file, `sqlite3PagerSync`; state → WRITER_FINISHED. The database is fully written and synced but
  the journal still exists.
- **6748-6786 `sqlite3PagerCommitPhaseTwo`.** Increments the data version and calls
  `pager_end_transaction`, whose journal deletion/truncation/zeroing is the commit point.
- **6881-6887 `sqlite3PagerMemUsed`.** `CACHE_USED` for one pager: (page size + nExtra +
  sizeof(PgHdr) + 5 pointers) × pages in the cache + the Pager allocation's size + one page size.
- **6928-6946 `sqlite3PagerCacheStat`.** Returns (and optionally resets) the HIT, MISS, WRITE and
  SPILL counters behind `SQLITE_DBSTATUS_CACHE_HIT/MISS/WRITE/SPILL`.
- **3579-3590 `pagerFixMaplimit`.** Applies `mmap_size`: turns `bUseFetch` on when the size is >
  0, switches the page getter (`getPageMMap`), and passes the limit to the VFS with
  `SQLITE_FCNTL_MMAP_SIZE`.

pcache.c:
- **41-54 `struct PCache`.** The pager's view of its cache: the dirty list (head, tail, last synced
  page), the summed reference count, `szCache` (the cache_size setting), `szSpill`, page and extra
  sizes, the stress callback (`pagerStress`) and the pluggable cache handle.
- **195-259 `pcacheManageDirtyList`.** Adds or removes a page at the head of the dirty list (kept
  in least-recently-dirtied order) and keeps `pSynced` pointing at the oldest page that can be
  written without a journal sync.
- **277-292 `numberOfCachePages`.** A positive cache_size is a page count; a negative one is KiB:
  pages = (−1024 × cache_size) / (page size + extra size).
- **403-432 `sqlite3PcacheFetch`.** Asks the pluggable cache (`xFetch`, pcache1 by default) for a
  page; whether it may allocate depends on `eCreate` (purgeable caches with dirty pages may only
  create when it is cheap).
- **445-490 `sqlite3PcacheFetchStress`.** When the cache is above `szSpill`, picks a dirty page
  that needs no journal sync (or any unreferenced dirty page) and calls `xStress` (`pagerStress`)
  to write it out, then fetches with permission to recycle.
- **593-607 `sqlite3PcacheMakeDirty`.** Marks a page dirty and adds it to the dirty list.
- **818-824 `sqlite3PcacheDirtyList`.** Returns the dirty pages sorted by page number (merge sort)
  for writing.

pcache1.c:
- **117-127 `struct PgHdr1`.** One cached page: the page buffer and extra, its page number, the
  hash chain, the owning cache (`pCache` — in the unified mode a page can change owner), and the
  LRU links.
- **158-165 `struct PGroup`.** A group of caches sharing one LRU: `nMaxPage` (sum of members'
  maxima), `nMinPage`, `mxPinned`, `nPurgeable`, the LRU anchor. One per process when
  `ENABLE_MEMORY_MANAGEMENT` is defined (correction 5).
- **175-203 `struct PCache1`.** One connection's cache: its group, `szPage`, `szExtra`, `nMax`
  (cache_size in pages), `n90pct`, page count, hash table, free list.
- **209-211 `struct PgFreeslot`.** A free slot in the optional start-time page-cache buffer
  (SQLITE_CONFIG_PAGECACHE); not used by the driver here.
- **520-526 `pcache1UnderMemoryPressure`.** True when the soft heap limit says the heap is nearly
  full (or the slab is exhausted); then fetches recycle instead of allocating.
- **621-637 `pcache1EnforceMaxPage`.** Evicts LRU pages from the whole group until the group is
  within `nMaxPage`.
- **874-941 `pcache1FetchStage2`.** Allocating a new page: refuse if too many are pinned; if this
  cache is at `nMax−1` or memory is under pressure, recycle the group's LRU page (possibly owned by
  another connection, 899-908); otherwise allocate.

status.c:
- **203-421 `sqlite3_db_status64`.** The per-connection counters: LOOKASIDE_*, CACHE_USED (sum of
  `sqlite3PagerMemUsed` over the connection's databases; CACHE_USED_SHARED divides by the number
  of connections sharing a b-tree), SCHEMA_USED, STMT_USED, CACHE_HIT/MISS/WRITE/SPILL
  (`sqlite3PagerCacheStat`), DEFERRED_FKS, TEMPBUF_SPILL.

## Timeline data (chapter section 4): one commit in rollback mode, synchronous=FULL

From 4.1 part D (step 3), calls numbered as in the evidence, with pager state and lock:

| # | Syscall | What | Pager state / lock |
| --- | --- | --- | --- |
| 1-3 | `fcntl F_RDLCK 1073741824 len=1`, `F_RDLCK 1073741826 len=510`, `F_UNLCK 1073741824` | SHARED lock (via PENDING byte) | OPEN → READER |
| 4 | `pread64 t.db off=24 len=16` | change counter check: is the cache still valid? | READER |
| 5 | `fcntl F_WRLCK 1073741825 len=1` | RESERVED (BEGIN IMMEDIATE) | → WRITER_LOCKED |
| 6 | `pread64 t.db off=4096 len=4096` | read page 2 | |
| 7-9 | `open t.db-journal`, `fcntl F_SETFD` (modernc only), `pwrite64 off=0 len=512` | journal created, header with zero magic/count | → WRITER_CACHEMOD |
| 10-36 | 21 × `pwrite64 t.db-journal` (4 / 4096 / 4 per page) interleaved with `pread64 t.db` | original images of pages 2, 1, 20, 3, 27, 4, 26 | WRITER_CACHEMOD |
| 37-38 | `fcntl F_WRLCK 1073741824 len=1`, `F_WRLCK 1073741826 len=510` | EXCLUSIVE (commit) | |
| 39 | `pread64 t.db-journal off=29696 len=8 = 0` | stale-header probe at the next sector | |
| 40 | `fsync t.db-journal` | journal sync 1 (FULL only) | |
| 41-44 | `open .`, `fcntl F_SETFD`, `fsync .`, `close .` | directory sync: the new journal's name is durable | |
| 45 | `pwrite64 t.db-journal off=0 len=12` | magic + record count 7: journal becomes hot | |
| 46 | `fsync t.db-journal` | journal sync 2 | → WRITER_DBMOD |
| 47-64 | 18 × `pwrite64 t.db` at ascending offsets 0 … 155648 | pages 1-4, 20, 22, 26-39 | WRITER_DBMOD |
| 65 | `fsync t.db` | database sync | → WRITER_FINISHED |
| 66-67 | `close t.db-journal`, `unlink t.db-journal` | **commit point** | → READER |
| 68-70 | `fcntl F_RDLCK 1073741826 len=510`, `F_UNLCK 1073741824 len=2`, `F_UNLCK 0 len=0` | back to SHARED, then unlocked | → OPEN |

NORMAL: drop #40; #45 comes right after the last record, then one journal sync, then the
directory sync. OFF: no syncs at all, header written with magic and 0xffffffff at #9, no #45.
TRUNCATE/PERSIST: #66-67 replaced by `ftruncate t.db-journal len=0` or `pwrite64 off=0 len=28`,
each followed by `fsync t.db-journal` (FULL).

Crash outcome by window (FULL): before #45 → journal not hot, database untouched, pre-step; #45
to #67 → journal hot, rolled back to pre-step; after #67 → post-step.

## Platform implications (facts for section 7)

- A `-journal` beside a database is part of it while it is hot. 4.1 E: copying the database alone
  gave `database disk image is malformed`; copying both gave the pre-step state. Backup, move,
  capture and rebuild code must copy `t.db` and `t.db-journal` together or refuse a file with a
  hot journal (ch02's capture refuses: hot test = size > 0 and first byte ≠ 0).
- Deleting a hot journal is never a fix: torn file (unreadable) or a commit that never returned
  turned into a committed one (4.1 E3).
- A cold journal (first byte 0) can remain after a crash and after reopening; it is harmless, but
  an "is this file at rest" check that only looks for the file name will flag it. The hot test is
  the right one.
- Rollback mode costs 3 (NORMAL) or 4 (FULL) syncs per commit, plus writing every changed page
  twice (journal + database); WAL mode in ch01 needed 1 sync per commit after the first. Chapter 7
  measures the costs.
- Every new SQLite file, including those the WAL opener creates, has at least one rollback-journal
  commit (correction 4). Imported or CLI-created files stay in rollback mode until switched; the
  opener must switch and must refuse (as sqlitex does) to silently flip an existing file.
- After a rollback-mode crash, verify against expected state at E3 / canonical E1, not raw E1
  (correction 1).
- Memory: with this driver, page-cache memory = (open connections) × (cache_size budget), pooled
  across the process (correction 5), and real RSS ≈ 2 × that (correction 6). 64 connections at the
  default `-2000` cost about 250 MiB RSS; a pool of 8 kept it at about 35 MiB regardless of how many
  files were visited. Size a fleet process by its open-connection cap, not by its file count.
- `cache_size` set per connection is still the knob; an idle connection's cache is reclaimed by
  active ones in this build, so closing idle connections mainly frees their fixed overhead
  (~5 KB `CACHE_USED`, the `-shm` mapping, Go and driver structures) — the cache pages were
  already being reused.
- `mmap_size` moves the read cache out of SQLite's heap into the kernel page cache (file-backed
  RSS, shared between processes and reclaimable by the kernel): `CACHE_USED` 2 pages instead of
  482 for a full scan; RSS still grows by the file size (file-backed).

## WAL-default evaluation (ruling 10) for this chapter

- 4.1 is rollback mode by subject (the planned exception). The platform database itself stays
  WAL; 4.1 part A shows the two places a WAL platform still meets rollback mode: files created or
  imported without the WAL switch, and the switch commit itself.
- 4.2 runs every database in WAL mode (sqlitex defaults). WAL as default made sense for every part
  of 4.2: the page cache works the same; each WAL database adds a 32 KiB `-shm` mapping per open
  connection's file (visible as file-backed RSS growth: about 3 MiB for 64 files, of which 2 MiB is 64 × 32 KiB).
- No place found where WAL as the default does not make sense.

## Linux-only and the Mac route

- 4.1: strace parts (part A's three traced children, parts D, D2, E, F) are Linux-only. On other
  systems it prints part A's portable checks (journal_mode, header bytes), part B, part C (the
  journal of an open transaction, decoded by `internal/journalfile` — fully portable), then one
  line `LINUX-ONLY: the strace of the commit, the kill at every write and sync, the failure cases
  and the commit-point table skipped on darwin; run it with: docker compose run --rm lab go run
  ./ch04-pager-and-page-cache/experiments/4.1-atomic-commit-and-hot-journal`, then commits the step
  in process at FULL, NORMAL and OFF and checks the post-step bytes; exit 0. darwin: cross-built
  only.
- 4.2: portable. Process memory comes from `ps -o rss= -p <pid>` on macOS (total RSS only). The
  anonymous/file-backed split in part E needs `/proc/self/status`; on macOS that check is replaced
  by a `LINUX-ONLY:` line naming the Docker command, and the total-RSS mmap check still runs. On
  macOS the RSS numbers will differ (different allocator page sizes); the EXACT lines (pages,
  counters) should not, except that `CACHE_USED`-derived pages assume 64-bit pointers. darwin:
  cross-built only.

## Spans added beyond the outline's reading path

`journalHdrOffset`, `zeroJournalHdr`, `writeJournalHdr`, `readJournalHdr`, `pager_cksum`,
`pager_playback_one_page`, `pagerSyncHotJournal`, `syncJournal`, `pager_write_pagelist`,
`getPageNormal`, `getPageMMap`, `pager_open_journal`, `sqlite3PagerBegin`,
`pagerAddPageToRollbackJournal`, `pager_write`, `sqlite3PagerSync`, `sqlite3PagerCommitPhaseTwo`,
`sqlite3PagerMemUsed`, `sqlite3PagerCacheStat`, `pagerFixMaplimit` (pager.c); `PCache`,
`pcacheManageDirtyList`, `numberOfCachePages`, `sqlite3PcacheFetch`, `sqlite3PcacheFetchStress`,
`sqlite3PcacheMakeDirty`, `sqlite3PcacheDirtyList` (pcache.c); `PgHdr1`, `PGroup`, `PCache1`,
`PgFreeslot`, `pcache1UnderMemoryPressure`, `pcache1EnforceMaxPage`, `pcache1FetchStage2`
(pcache1.c). The outline's "pager.c 135-406" is listed as two spans (135-350 state comment,
360-406 lock comment) plus 351-357; "pcache.c sqlite3PcacheFetch and the dirty list" is the
`PCache`, `pcacheManageDirtyList`, `sqlite3PcacheFetch`, `MakeDirty`, `DirtyList` spans;
"pcache1.c structures (117-211)" is the four struct spans; "status.c 203-421" is
`sqlite3_db_status64` exactly.

Cited but not copied: `btree.c:6740` (NOCONTENT on freelist reuse; copied in ch02's directory),
`sqlite3BtreeSetVersion` (btree.c), os_unix.c `unixSync`/`unixFetch`/`unixMapfile` (chapter 7
copies os_unix.c), `setSectorSize` (pager.c, in the copied file but not in SPANS).

## Working scripts

`agent_work/ch04/step-probe/` (Go): for each recipe step 1-30 in rollback mode, straces one child
applying that step and counts journal writes, database writes, syncs and unlinks. Used to choose
step 3 (7 journaled pages, 18 database writes, a delete step that also reuses a freelist page)
and step 1 (no freelist in its pre-step file). Indexed in `agent_work/ch04/INDEX.md`.

## Not done / for later chunks

- No chapter prose, no comments (seat rule).
- A kill-at-syscall demonstration of correction 4 (crash inside the WAL switch of a new file) was
  not run; the file would be at most one page in rollback mode.
- Spill (`pagerStress`, multiple journal headers) is read in the source but not exercised: the
  recipe step fits in the cache. A transaction larger than `cache_size` would show a second header
  in the journal; `internal/journalfile` handles multiple segments (unit test), never seen live.
- The CLI (separate page caches) was not measured in 4.2; correction 5's statement about C builds
  without `ENABLE_MEMORY_MANAGEMENT` is from the source.
- 1,000 connections (outline's adversarial item) not run here: about 4 GB RSS with this driver.
