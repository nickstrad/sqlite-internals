# Chunk 8 build notes (plain data for the prose pass)

Builder: Claude Opus 5.5, branch `chunk/ch08` (base: ch04 + ch05 merged), 2026-11-20/21. Machine:
linux/amd64, Go 1.26.8, ext4, 4 cores, 7.9 GiB shared with other agents, strace 6.8. Pinned tree at
`$SQLITE_SRC` (3.53.4). `RLIMIT_NOFILE` as the Go process sees it: soft 1048576, hard 1048576 (Go
raises the soft limit to the hard limit at start-up; both are 1048576 here).

Container: 8.1 and 8.2 were also run in the Debian `lab` container
(`docker compose run --rm -T lab go run ./ch08-connection-and-database-lifecycle/experiments/<name>`):
EXACT output (all non-`INFO:` lines) identical to the host's, including 8.1's strace part. 8.3 was
not run in the container (runtime). darwin: cross-built and cross-vetted only
(`agent_work/verify/crossbuild.sh` → `CROSSBUILD OK`), never run.

`go run ./cmd/course check sources ch08` passes (3 files: `main.c`, `prepare.c`, `status.c`,
copied whole from `$SQLITE_SRC/src/`); `check spans ch08` passes (34 spans); `check evidence
ch08` passes (each experiment run twice, identical to the evidence file).

## Corrections to the outline

1. **The reading path's `main.c openDatabase (3360–3736)` covers three functions.**
   `openDatabase` is 3360–3716; `sqlite3_open` is 3722–3728 and `sqlite3_open_v2` 3729–3736.
   SPANS.txt lists them separately. Every other outline span was exact (start line = symbol, end
   line = closing brace): `sqlite3BtreeOpen` 2532–2825, `sqlite3InitOne` 199–427, `sqlite3Init`
   438–464 / `sqlite3ReadSchema` 470–484 / `schemaIsValid` 492–533 (the outline's 438–533 is these
   three), `main.c` 1266–1486 (= `sqlite3Close` 1266–1322 … `sqlite3LeaveMutexAndCloseZombie`
   1375–1486), `status.c` 203–421 (`sqlite3_db_status64`), `os_unix.c` 2101–2109
   (`setPendingFd`), `wal.c` `sqlite3WalClose` 2507–2571.
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
3. **"Opening a database reads only the 100-byte header" — confirmed; "page-1 and schema-page reads
   at first statement" — confirmed and refined:** in WAL mode page 1 is read **twice** at the first
   statement. lockBtree reads page 1, sees header byte 19 = 2, opens the WAL and returns without
   keeping page 1 (btree.c 3333–3352: "the version of page 1 currently in the page1 buffer may not
   be the latest version - there may be a newer one in the log file"); `btreeBeginTrans` calls
   lockBtree again in a loop (btree.c 3711: `while( pBt->pPage1==0 && SQLITE_OK==(rc =
   lockBtree(pBt)) );`) and page 1 is read again, now through the WAL-aware pager. The `-wal` and
   `-shm` are opened between the two reads, at the first statement, not at open.
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
6. **The outline's R5 condition "a WAL database whose last connection has closed is a single, fully
   checkpointed file" has exceptions; 8.3 part B shows five of them and the source shows two more**
   (see the WAL-default evaluation below).
7. **`recipe.MatchStep` is too slow to call per file at fleet scale** (not an outline error, a
   finding for later chunks): every call re-verifies the step-hash cache by building a fresh step-0
   database (~100 ms). 8.3 calls `recipe.StepHashes` once and matches with `recipe.MatchHashes` +
   `recipe.ChangeCountersAgree` (the same logic as `MatchStep`). Matching 1,000 files went from
   ~110 s to under a second. The capstone (2,000 databases) should do the same.

Everything else in the chapter-8 section was confirmed: descriptor counts 1 / 2 / 3 (8.2 A), cache
releasable without closing while schema and statements stay (8.2 C), bounded descriptors and
memory under a K-connection manager, every reopen hash matching, every final file equal to the
recipe state (8.3 A).

## Shared packages (every change)

### New: `internal/procstat` (portable, standard library only)

- `DescriptorDir`: `/proc/self/fd` on Linux, `/dev/fd` elsewhere (`procstat_linux.go` /
  `procstat_other.go`). `TargetsSupported` is true only on Linux (a `/dev/fd` entry on macOS is not
  a symlink, so the file a descriptor points to cannot be read that way).
- `DescriptorCount()`: opens the descriptor directory, reads its entry names, and excludes the
  descriptor it used to read the directory (`dir.Fd()`), so two counts in a row are equal and
  opening N files adds exactly N. Fails on a non-numeric entry.
- `Descriptors()` → `[]Descriptor{FD, Target}` (Target from `readlink` on Linux, empty elsewhere;
  a descriptor closed between listing and readlink is skipped). `DescriptorsUnder(dir)` keeps those
  whose target is inside dir (returns `ErrTargetsUnsupported` off Linux). `RelativeTargets(dir, ds)`
  → sorted names relative to dir. Both resolve dir first (`filepath.Abs`, then
  `filepath.EvalSymlinks` when the directory exists), because `/proc/self/fd` targets are always
  absolute and symlink-free: a relative dir (`"_work/8.2"`) and `"/"` work (repair round; before it,
  both silently matched nothing).
- `Memory{RSS, Anon, File, Shmem}` (bytes; −1 = not available), `HasBreakdown()`. `ReadMemory()`:
  Linux parses `/proc/self/status` (`VmRSS`, `RssAnon`, `RssFile`, `RssShmem`; `ParseStatus`
  rejects a missing VmRSS, a duplicate field, a non-`kB` unit, negative or non-numeric sizes, and a
  KiB value whose byte count does not fit in int64, i.e. above `math.MaxInt64/1024`);
  elsewhere runs `ps -o rss= -p <pid>` (`ParsePSRSS` accepts exactly one non-negative integer, with
  the same int64 bound).
  `MemorySource` names the source.
- `FileLimit()` → soft and hard `RLIMIT_NOFILE` (`syscall.Getrlimit`, exists on linux and darwin).
- Mergeable with chunk 4's helper: chunk 4 keeps `readProcessMemory` inside
  `ch04-…/experiments/4.2-cache-budget/memory_{linux,other}.go` with the same fields (`RSS, Anon,
  File, Shmem`, −1 when unknown) and the same two sources. 4.2 could switch to
  `procstat.ReadMemory()` without changing its output. I did not edit chunk 4's experiment.
- Tests: open/close three files changes the count by exactly 3 and back; two counts in a row
  equal; `Descriptors` length = count; `DescriptorsUnder` ignores a sibling directory with the same
  prefix (`dir-sibling`); `DescriptorsUnder` with three relative spellings of one directory (after
  `t.Chdir`) and with `"/"` finds the open probe file; parsers reject 10 malformed status texts
  and 8 malformed ps outputs (including 2^53 and 2^54 KiB, which overflowed before the repair);
  `ReadMemory` > 0 and has the breakdown on Linux; `FileLimit` soft ≤ hard.

### Changed: `internal/sqlitex` (new file `placement.go`, additive)

- `SettingHome` (`SettingInFile` = "file", `SettingPerConnection` = "connection"),
  `SettingPlacement{Name, Pragma, Home, HeaderBytes}` with JSON tags, `SettingPlacements()` (14
  entries: page_size 16-17, journal_mode=wal 18-19, auto_vacuum 52-55,64-67, encoding 56-59,
  user_version 60-63, application_id 68-71, default_cache_size 48-51; per connection:
  journal_mode=delete|truncate|persist|memory|off, synchronous, foreign_keys, cache_size,
  busy_timeout, wal_autocheckpoint, secure_delete), `RollbackJournalModes()`,
  `SettingPlacementFor(name)`. 8.1 verifies every placement it can observe and fails if one
  differs. This is the machine-readable table the capstone's catalog can use. Test:
  `TestSettingPlacementsCoverEveryCourseSetting`.

### `internal/capture`: not changed

An exported `HotJournal` was added and then reverted: chapter 2 inlines `capture.go` whole, so any
change to that file breaks `cmd/course inline -check` for ch02. `connmgr` has its own 20-line
`hotJournal` with the same test (`hasHotJournal`, pager.c: the journal exists, is non-empty and its
first byte is non-zero). If ch02's chapter is regenerated later, the two copies could be merged.

### New: `internal/connmgr` (the 8.3 manager, reused by chunk 10a)

API:
- `New(Options{Capacity, PathFor func(id) (string, error), Open func(ctx, path) (*sqlitex.DB,
  error), OnEvict func(Eviction)})`. Capacity ≥ 1; PathFor and Open required.
- `Acquire(ctx, id) (*Handle, error)`; `Handle{ID, Path, DB}`; `Handle.Release()` (a second
  Release does nothing). Acquiring an id that is open returns the same `*sqlitex.DB` (hit; moves it
  to the front of the LRU list). Otherwise Acquire first checks ctx: an already-cancelled or
  expired ctx returns `ctx.Err()` before anything is evicted or opened (repair round; before it, a
  cancelled Acquire at capacity evicted a healthy idle database and then failed in Open). Then, if
  K are open, the least recently used entry with no outstanding handle is evicted; if every open
  entry has a handle, Acquire waits for a Release (or ctx). One mutex; opening, closing and hashing happen while holding it (simple and
  serialised; fine for the course, a known throughput limit).
- Eviction (capacity, `Evict(id)`, or `Close()`): remove from the LRU, `db.Close()`, then:
  - if Close returned an error: no file check; `Status` is not at rest with the one problem
    `close failed: <err>`;
  - else if `db.Stats().OpenConnections > 0` (database/sql still has a connection out: an unclosed
    `*sql.Rows`, `*sql.Tx` or `*sql.Conn`; `sql.DB.Close` closes only idle connections and returns
    nil): no file check; not at rest with the one problem `N connection(s) still open in this
    process: a statement, transaction or *sql.Conn was not closed, so database/sql closes the
    connection only when it is returned`. Skipping the file check here matters: `CheckAtRest`
    opens and closes the database file, and closing any descriptor drops every POSIX lock this
    process holds on that file, including the still-open connection's (adversarial ATR-02: before
    the repair, a rollback-mode eviction with rows open reported at rest AND let another process
    write while the rows were still being read);
  - else `CheckAtRest(path)`.
  At rest → the `Status` (sha256, size) is recorded for the id. Not at rest or check error → any
  earlier record is dropped (`EvictedNotAtRest` / `StatusFailures`). `OnEvict` receives `Eviction{ID, Path, Cause
  (capacity|explicit|shutdown), CloseErr, Status, StatusErr}` after the mutex is released, so a
  hook may call the manager.
- Reopen check: when an id with a record is opened again, `CheckAtRest` runs first; if the file is
  not at rest or its sha256 or size differs from the record, Acquire returns
  `*ChangedWhileClosedError{ID, Path, Recorded, Found}` (counted in `RefusedReopens`) and does not
  open it; if the check itself fails with an I/O error, Acquire returns it and counts
  `StatusFailures`. `Forget(id)` drops a record (for a deliberate external change, e.g. a restore).
  First opens (no record) are not checked. Counting (repair round): `Misses`, and exactly one of
  `VerifiedReopens` / `UnverifiedOpens`, are incremented only after `Open` succeeds, so
  `Misses == VerifiedReopens + UnverifiedOpens` always holds; a failed Open counts only
  `OpenFailures` (before the repair, the verified/unverified counter was bumped before Open ran).
- `Evict(id)` → `ErrNotOpen` / `ErrInUse`. `Close()` → `ErrInUse` (listing ids) if any handle is
  outstanding, `ErrClosed` the second time; evicts everything otherwise and joins close/check
  errors. `Stats()` (Open, MaxOpen, Hits, Misses, Evictions, EvictedAtRest, EvictedNotAtRest,
  VerifiedReopens, UnverifiedOpens, RefusedReopens, CapacityWaits, OpenFailures, CloseFailures,
  StatusFailures, DistinctDatabases), `Record(id)`, `OpenIDs()` (most recent first).
- `CheckAtRest(path) (Status, error)`: `Status{Path, AtRest, Problems, SHA256, Size}`. Problems,
  in order: "database file missing"; "-wal holds N bytes: the database file alone may lack
  committed transactions" (a zero-byte `-wal` is fine); "hot -journal (N bytes) …" (own
  `hotJournal`, the same test as `capture`'s); "-shm exists (N
  bytes): a connection is open or did not close cleanly". Only when there is none of these, the
  database file is opened once and probed for locks held by other processes: `fcntl(F_GETLK)` asks
  whether a write lock on SQLite's lock bytes (offset 0x40000000, length 512: PENDING, RESERVED
  and the 510-byte SHARED range; os_unix.c) would conflict. A conflict adds the problem "another
  process holds a SQLite lock on the database file: it has the database open" and the file is
  not hashed. Otherwise the same descriptor is hashed (sha256 of the whole file). F_GETLK never
  reports this process's own locks (POSIX locks belong to the process), so it sees other processes
  only. `syscall.FcntlFlock` and `F_GETLK` exist on linux and darwin (darwin: cross-built only).
  This is stricter than chunk 2's at-rest rule (`capture.CheckAtRest`) by the `-shm` rule the
  brief asked for and by the lock probe (repair round).
- Hazard (kept from §4.1): `CheckAtRest` opens and closes the database file. Closing any descriptor
  drops this process's POSIX locks on that file, so it must only run when this process has no
  other connection to the file. The manager runs it right after closing its only connection.
- What the at-rest check can and cannot see (corrected in the repair round; the earlier sentence
  "in WAL mode every open connection keeps the -shm" was FALSE):
  - A WAL connection with `PRAGMA locking_mode=EXCLUSIVE` that has run a statement keeps its
    wal-index in heap memory and creates **no** `-shm` (adversarial ATR-01b: `shm_exists=false`;
    test `TestCheckAtRestSeesALockHeldByAnotherProcess` asserts the absent -shm). The side-file
    rules miss it; the lock probe catches it, because it holds a lock on the database file.
  - A rollback-mode connection in another process inside a read transaction holds a SHARED lock:
    the lock probe catches it (ATR-01b's rollback reader).
  - Still invisible: a connection in another process that holds no lock at that moment — an
    idle rollback-mode connection, or any connection that has opened the file but not yet run a
    statement (adversarial ATR-01a case 3: at rest). "At rest" therefore means "complete now and
    nobody is using it now", not "nobody has it open"; a platform must also own the file
    (only the manager opens it).
  - Connections in this same process: handled by the `OpenConnections` rule above for the
    manager's own `*sqlitex.DB`; another `*sql.DB` in the same process holding the file is
    invisible to the lock probe (F_GETLK ignores own-process locks) and is excluded by the
    hazard rule above.
- Tests (`connmgr_test.go`, all on real sqlitex databases): LRU order with K=2 over
  a,b,a,c,d,a evicts b, a, c; stats; records after Close; refuse reopen after an outside write
  (then `Forget` allows it); refuse reopen when a `-shm` appeared; eviction while another
  connection is open reports exactly the two problems (-wal, -shm) and drops the record; waiting
  when all K are in use (ctx deadline, then success after Release; double Release harmless);
  Evict/Close while in use; same id shares one connection; bad id and junk file fail Acquire
  without leaking an entry and without touching Misses/UnverifiedOpens/VerifiedReopens;
  `CheckAtRest` cases (missing, plain, empty -wal, -wal with content, -shm only, cold journal,
  hot journal). Added in the repair round: `TestFailedReopenCountsOnlyAsOpenFailure` (a recorded
  id whose Open fails: Misses 1, OpenFailures 1, VerifiedReopens 0; then a good reopen gives
  VerifiedReopens 1 and Misses = Verified + Unverified); `TestAcquireWithCancelledContextEvictsNothing`;
  `TestEvictionWithRowsStillOpenSkipsTheFileCheck` (rollback mode, rows open: not at rest with the
  "1 connection(s) still open" problem, no sha256, no record, the rows still read their next row,
  at rest after rows.Close); `TestCheckAtRestSeesALockHeldByAnotherProcess` (re-executes the test
  binary as a helper process that opens the file with a plain driver connection, sets
  `locking_mode=EXCLUSIVE` and reads: no -shm, CheckAtRest reports exactly the lock problem; at
  rest after the helper exits). `go test -race -count=2` passes.

## Experiment 8.1 open-cost-and-where-settings-live

Run: `go run ./ch08-connection-and-database-lifecycle/experiments/8.1-open-cost-and-where-settings-live`
(from the repo root). Runtime ~4 s. Evidence: `evidence/8.1-open-cost-and-where-settings-live.txt`.
Flags: none.

Claims: (A) opening reads only the 100-byte header; the schema is read at the first statement, at a
cost that grows with schema size; (B) some settings are stored in the file and survive a copy, the
rest belong to the connection.

Part A, per schema of 1, 100 and 1,000 empty 4-column tables (built with `sqlitex.Open`, WAL, one
transaction, closed, at rest):
- `N table(s): file P pages` — 2, 105, 1036 pages.
- `SCHEMA_USED: 0 bytes after open, X bytes after the first statement` — X = 827, 26601, 251601
  (827 / 266 / 251 bytes per table). Measured in-process with a plain driver connection (no
  settings applied, because any settings pragma would itself load the schema). Portable.
- `INFO: first statement … took` — 0.7 ms, 1.5 ms, 11 ms here (timing, never checked).
- Linux only: the experiment re-runs itself as `child-open <path>` under strace (`open, openat,
  pread64, read, write, close`). The child prints `MARK open`, `MARK first statement`, `MARK
  close`, `MARK done` to stdout; the experiment splits the trace at those writes. Per phase it
  prints `K event(s): …` = the opens and reads of files in the work directory, in order (first 8
  shown), and `whole-page reads of the .db file: R, distinct pages D: <ranges>`.
  - open: `2 event(s): open schema-N.db, read 100 bytes at 0` for every N.
  - first statement: `read page 1, open schema-N.db-wal, open schema-N.db-shm, read page 1, read
    page …`. 1 table: 3 reads (pages 1, 1, 2). 100 tables: 7 reads, distinct 1-2,34,36,69,102. 1000
    tables: 38 reads, 37 distinct pages. The distinct pages are exactly the sqlite_schema b-tree
    pages listed by `dbstat` (1, 5, 36 pages) plus t0001's root page (page 2). Schema pages are not
    contiguous because the tables were created in one transaction, each root page allocated
    between schema pages.
  - close: `0 event(s): none` (no reads or opens; the side files are unlinked, which is not traced).
- Checks (each must hold for every N): open opens only the .db (by name) and reads only its
  100-byte header (one read at offset 0, length 100, returning 100 bytes); first-statement reads
  of the .db are all whole, page-aligned 4096-byte reads that returned 4096 bytes, and their
  pages = schema pages + root (the whole-read and returned-length conditions were added in the
  repair round; the output did not change); page 1 read exactly twice, first before the -wal
  open and again after it; no reads of -wal or -shm (the -wal is empty; the -shm is mmap'ed); -wal
  and -shm opened at the first statement. Then: SCHEMA_USED 0 after open for every N and strictly
  growing 1 < 100 < 1000.
- On non-Linux: one `LINUX-ONLY:` line, the SCHEMA_USED lines still print.

Part B, settings:
- `original.db` is created with `sqlitex.Open` with every value differing from SQLite's default:
  page_size 8192, auto_vacuum INCREMENTAL (2), encoding UTF-16le, application_id 1397836120 (SQIX),
  user_version 7, journal_mode wal; synchronous NORMAL (1), foreign_keys ON, cache_size −8000,
  busy_timeout 3000 ms, wal_autocheckpoint 200, secure_delete ON. A table and a row are written;
  the values are read back on that connection (the `original` column).
- `check ok: the original is at rest …`, `a plain copy … has the same sha256` (R5 in one line).
- `VACUUM INTO 'vacuum-into.db'` is run from a plain connection. `fresh.db` is a new file opened
  with a plain connection (the `default` column: what a connection gets when nothing is set).
- Table columns: setting, original, default, copy, VACUUM INTO, lives in. "lives in" is derived:
  `file` if the plain connection to the copy reads back the value set, else `connection`.
  Result: the six file settings survive the copy; the six connection settings read their defaults
  (synchronous 2 = FULL, foreign_keys 0, cache_size −2000, busy_timeout 0, wal_autocheckpoint
  1000, secure_delete 0). `VACUUM INTO` keeps all file settings except journal_mode, which reads
  `delete` (its output is a rollback-mode file; header bytes 18–19 = 1, read and checked by the
  experiment since the repair round).
- Checks: every set value differs from the default, and the observed home of every setting equals
  `sqlitex.SettingPlacements()`; VACUUM INTO keeps every file setting except journal_mode;
  `journal_mode=persist` on a new file reads `delete` after reopen; `default_cache_size=777` is in
  header bytes 48–51 of a copy and the copy's fresh connection has `cache_size 777`.
- `settings-json: {...}`: one line of JSON with `placements` (the declared table) and `observed`
  (per setting: home, set_on_original, fresh_connection_default, read_on_copy,
  read_on_vacuum_into, placement = that setting's entry from `sqlitex.SettingPlacementFor`).
  Machine-readable record for the capstone. (Before the repair round every `observed[].placement`
  was an empty object; review finding 2 / attack OPN-05.)

## Experiment 8.2 idle-cost

Run: `go run ./ch08-connection-and-database-lifecycle/experiments/8.2-idle-cost [-n 200] [-step 3]`
(`-n` must be ≥ 20: below that the fixed per-process memory growth exceeds the 256 KiB/connection
margin; see correction 4).
Runtime ~1.7 s at n=200, ~10 s at n=1000. Evidence at the defaults.

- Header lines: descriptor and memory sources (`/proc/self/fd`, `/proc/self/status` on Linux;
  `/dev/fd`, `ps -o rss= -p <pid>` elsewhere); INFO: the RLIMIT_NOFILE.
- Part A (EXACT): one plain connection per journal mode on a file with one table. Each line:
  state, descriptors above the baseline, `[names]` (Linux only), `side files:` present on disk.
  - delete: open 1 [t.db], after a read 1, inside a write transaction 2 [t.db t.db-journal],
    after the commit 1, after close 0.
  - wal: open 1 [t.db], after a read 3 [t.db t.db-shm t.db-wal], inside a write 3, after commit 3,
    after close 0 and no side files.
  - The "open, no statement yet = 1 in WAL mode" line is new data: the -wal and -shm are opened
    lazily at the first statement (same as 8.1).
  - On non-Linux: `LINUX-ONLY:` line for the names (counts still print).
- Part B: n copies of recipe step 3 (159744 bytes, 39 pages), each opened with `sqlitex.Open`
  defaults (cache_size −2000); per connection one full read of `documents` and one prepared
  statement kept open.
  - `descriptors held: 3n (3.00 per connection)`; check exactly 3n. Linux: `descriptor targets:
    db=n -wal=n -shm=n`.
  - `SCHEMA_USED per connection: 1347 bytes`, `STMT_USED per connection: 5008 bytes` (EXACT, all
    equal). INFO: CACHE_USED 52961 bytes per connection (path-length dependent, chunk 4 correction
    7, so INFO only).
  - INFO: sums, process memory before/after and the ratio (3.8–4.0×), the uncounted part per
    connection (164–174 KiB). Checks: growth ≥ sum; growth ≤ sum + 256 KiB per connection.
- Part C: `PRAGMA shrink_memory` on connection 0 → INFO shows its CACHE_USED 52961 → 5089 and
  connection 1's too. Checks: connection 0's shrink lowered every connection's CACHE_USED;
  shrinking the rest afterwards released nothing more; SCHEMA_USED and STMT_USED unchanged; every
  connection's CACHE_USED is under 1/8 of before (5089 is the Pager allocation + one page buffer).
  INFO: process memory fell only partly (200 conns: 54.0 → 47.4 MiB for 9.1 MiB of cache).
  Closing all: 0 descriptors and every file at rest.
- Part D (EXACT, beyond the outline's list but cheap): two plain connections to one WAL file:
  `3 [t.db t.db-shm t.db-wal]`, then `5 [t.db t.db t.db-shm t.db-wal t.db-wal]` (the -shm
  descriptor is shared per process per file: one `unixShmNode`), close connection 2 → `4 [t.db t.db
  t.db-shm t.db-wal]`: its database descriptor is parked, because connection 1 holds a SHARED lock
  on the database file for its whole life in WAL mode and closing a descriptor would drop the
  process's POSIX locks (`unixClose` → `setPendingFd`, os_unix.c 2341–2371, 2101–2109); close
  connection 1 → 0 (`closePendingFds` when the lock count reaches 0, 1471–1482). Consequence: with
  several connections per file (e.g. a database/sql pool), descriptors are not 3 per open
  connection.

## Experiment 8.3 bounded-manager (rung R5)

Run: `go run ./ch08-connection-and-database-lifecycle/experiments/8.3-bounded-manager` with flags
`-dbs 1000 -k 64 -ops 4000 -seed 1 -max-step 4 -write-percent 20 -hot-percent 50 -cache-size -256
-sample-every 10 -memory-every 100` (the defaults). Runtime 37–67 s here (two runs; the machine is
shared and evictions fsync). Evidence at the defaults. Peak memory: ~250 MiB in the unbounded
child. `-mode`, `-dir`, `-role` are internal (child processes).

Setup: one recipe step-0 database (`master.db`, sha256 7470585a…cdb9, printed) copied 1,000 times
into `bounded/` and 1,000 times into `unbounded/` (a copy is the R5 rebuild). Stream: PCG(seed, 83);
each op picks a database (50 % from the hot 5 % = 50 databases, else uniform) and is a write (20 %,
if the database is below step 4: `recipe.ApplyStep` of its next step) or a read (`readQuery`, which
also returns user_version). A model array holds each database's expected step.

Part A: each run in its own child process (clean descriptor and memory baselines); the parent
relays its lines.
- `operations: 4000 (3415 reads, 585 writes); distinct databases touched: 866`.
- bounded (K = 64): `hits 1198, misses (opens) 2802, evictions 2802 (at rest 2802, not at rest 0),
  reopens verified against the recorded sha256 1936, refused 0, first opens 866, peak open 64`.
  Evictions include the 64 at shutdown. 2802 opens = 866 first opens + 1936 verified reopens.
- `descriptors: 401 samples, peak 192 above the baseline; samples where descriptors != 3 x open
  connections: 0; after Close: 0` — sampled every 10 ops and at the end; the check is that every
  sample equals exactly 3 × currently open (stronger than the outline's ≤ 3K + c: the constant is
  0 between operations).
- `reads whose user_version disagreed with the model: 0`; `final files …: 970 exact bytes, 30
  equal except the change counter, 0 wrong`. "Equal except the change counter" = a session applied
  two steps on one connection (chunk 1 correction 1: in WAL mode the counter moves once per
  connection that writes page 1), accepted only when bytes 24–27 equal 92–95.
- INFO: peak memory growth 22.8 MiB against a budget K × (2 × 256 KiB cache + 256 KiB uncounted)
  + 32 MiB = 80 MiB (the INFO line now prints the two per-connection terms in KiB:
  `budget 64 x (2 x 256 KiB cache + 256 KiB) + 32.0 MiB = 80.0 MiB`; before the repair it printed
  them as `0.2 MiB`) (factor 2 = chunk 4's allocator finding; 256 KiB = 8.2's uncounted part); check
  peak ≤ budget. INFO: warm read ~0.13–0.15 ms, cold (evict another, open, verify hash, read)
  ~4.4–5.4 ms (SHAPE, never checked; the cold cost includes the evicted connection's close with
  checkpoint and fsync at synchronous=FULL).
- unbounded (capacity = 1000): `hits 3134, misses 866, evictions 866` (all at shutdown), `peak
  open 866`, descriptors peak 2598 = 3 × 866, 0 mismatches; final files 880 exact, 120 masked, 0
  wrong. INFO peak memory 247 MiB.
- Contrast checks: unbounded peak descriptors > 3K = 192; unbounded peak memory > bounded (SHAPE).
- `RESULT …` lines are the child's machine-readable summary; the parent prints them as INFO.

Part B: "when a closed WAL database is NOT a single file at rest" — each case on a fresh copy of
step 0, through a manager (K = 4) unless noted. Sizes are EXACT: one recipe step = 90672 bytes of
-wal (32-byte header + 22 frames × 4120), database 90112 bytes (22 pages) after checkpoint.
- B1 another process (child `-role reader`, an open connection after one read) holds the file
  when the manager evicts: `not at rest: -wal holds 90672 bytes …; -shm exists (32768 bytes) …`.
  After the child closes (it is now the last connection): at rest, step 1 (exact bytes).
- B2 rows left open: `h.DB.QueryContext` + one `Next`, handle released, evicted: not at rest
  (`sql.DB.Close` returns while a statement holds the connection; the SQLite connection stays
  open). Since the repair round the manager detects this itself and prints `eviction of db-0001:
  not at rest: 1 connection(s) still open in this process: a statement, transaction or *sql.Conn
  was not closed, so database/sql closes the connection only when it is returned`; it no longer
  runs the file check here (before: `-wal holds 90672 bytes …; -shm exists (32768 bytes) …`).
  After `rows.Close()`: at rest, step 1.
- B3 killed writer (child `-role writer` applies step 1, prints ready, is SIGKILLed by
  `Process.Kill`): -wal 90672 bytes + -shm. A copy of the database file alone = step 0 (exact): the
  committed step 1 is only in the -wal. One clean open + close (recovery, checkpoint, delete): at
  rest, step 1.
- B4 `SQLITE_FCNTL_PERSIST_WAL` set through `sqlitex.PersistWAL(…, 1)`: eviction leaves -wal 90672
  bytes and -shm → refused. A copy of the database file alone = step 1: the close DID checkpoint
  (wal.c 2533–2551: with bPersist the WAL is kept, truncated only if journal_size_limit ≥ 0, which
  it is not by default; `walIndexClose(pWal, isDelete=0)` keeps the -shm). The refusal is
  conservative. Checks: the eviction reports both `-wal holds` and `-shm exists` (the `-wal holds`
  half was added to the check in the repair round).
- B5 the file is renamed while its connection is open, then evicted: `not at rest: database file
  missing`; `left at the old name: -wal 90672 bytes` (a new check since the repair round: `check ok:
  B5: a -wal with content stays behind at the old name`); `moved.db: step 0 (exact bytes)` — the moved
  file passes every at-rest test but lacks step 1. Mechanism: `sqlite3PagerClose` passes no buffer
  to `sqlite3WalClose` when `databaseIsUnmoved` fails (pager.c 4232–4236, 4188–4205; `fileHasMoved`
  os_unix.c 1623–1632 compares the path's inode with the open descriptor's), so close skips the
  checkpoint and keeps the WAL under the old name.
- B6 an outside connection applies step 2 while the file is closed: the reopen is refused with
  `changed while closed: recorded sha256 6d8cfc77…, file now has sha256 69490a2d…` (check: the
  reopen check can fail).

## WAL-default evaluation for chapter 8 (ruling 10)

WAL is the right default for every part of this chapter, with these costs and conditions, all
measured:
- Cost per idle connection: 3 descriptors instead of 1 (8.2 A, B), opened lazily at the first
  statement. With one connection per file and K open, exactly 3K descriptors (8.3).
- At-rest after last close (R5) holds only when the closing connection is the last one in every
  process, it was not killed, it has no unfinished statement, the file was not moved or renamed
  while open, PERSIST_WAL is off, and the close's checkpoint succeeds. Measured failures: B1
  (another process), B2 (statement left open), B3 (kill), B4 (PERSIST_WAL), B5 (renamed). From the
  source only, not demonstrated: `SQLITE_DBCONFIG_NO_CKPT_ON_CLOSE` (pager.c 4232: `db->flags &
  SQLITE_NoCkptOnClose` → no buffer → no checkpoint; the driver does not expose `sqlite3_db_config`),
  and a checkpoint that returns an error (wal.c 2533–2537: `rc==SQLITE_OK` is required before the
  delete), e.g. an I/O error, or a connection that cannot take the EXCLUSIVE lock on the database
  file (wal.c 2526–2528; this is B1's mechanism).
- Copy-capture is safe only after the check: B3 shows the naive copy silently loses a committed
  transaction, B5 shows a file that looks at rest but is missing one. Rollback mode would not
  have B3's shape (a commit is in the database file when COMMIT returns), but has its own hot
  journal case (chunk 4).
- `VACUUM INTO` output is a rollback-mode file: a platform that rebuilds through `VACUUM INTO`
  (rung R2) must switch the result back to WAL (8.1 B).
No exception to WAL-as-default is recommended; the manager's at-rest rule (no -wal content, no
-shm, no hot journal) is what makes WAL databases safe to copy.

## Timeline data (for the chapter's Timeline section)

One WAL connection over its life (8.1 A, 8.2 A; Linux trace of the Go driver):
1. `sqlite3_open_v2`: `open t.db`, `pread64 100 bytes @0`. 1 descriptor. SCHEMA_USED 0. No lock,
   no -wal, no -shm.
2. First statement: `pread64 page 1` (header byte 19 = 2) → `open t.db-wal`, `open t.db-shm` (+ the
   -shm mapping) → `pread64 page 1` again → schema pages (1 for 1 table, 5 for 100, 36 for 1000) →
   the table's root page. 3 descriptors. SCHEMA_USED 827 / 26601 / 251601 bytes. First-statement
   time 0.7 / 1.5 / 11 ms here.
3. Idle: 3 descriptors; memory = CACHE_USED (52961 bytes after reading a 39-page file with
   cache_size −2000) + SCHEMA_USED (1347 for the recipe schema) + STMT_USED (5008 for one kept
   statement) + ~165–175 KiB not reported by the counters.
4. `PRAGMA shrink_memory`: CACHE_USED → 5089 (and every other connection's too, with this driver);
   schema and statements unchanged.
5. Write transaction: still 3 descriptors (rollback mode: 1 → 2 with the -journal).
6. Last close: PASSIVE checkpoint under an EXCLUSIVE lock, -wal and -shm unlinked (chunk 0 smoke
   trace: fsync -wal, copy frames to the db, ftruncate, fsync db, unlink -shm, unlink -wal). 0
   descriptors; the file is at rest and a copy equals it.
Descriptors over a manager's life: exactly 3 × open connections at every sample; peak 3K.

## Platform implications (facts)

- Descriptors per idle WAL database: 3 (1 before its first statement); per rollback database 1
  (2 during a write). Budget 3K + the process's own descriptors for K open databases; with two
  connections to one file in one process: 5, and a closed one stays parked (4) until the last
  connection to the file closes.
- Memory per idle connection with this driver: CACHE_USED (bounded by cache_size; with the shared
  group, the process total is Σ budgets — chunk 4) + SCHEMA_USED (~250 bytes per table) +
  STMT_USED (5 KB per kept statement here) + ~170 KiB uncounted. 1,000 idle connections to small
  databases: ~216 MiB RSS growth (8.2 at n=1000); with K = 64, 22.8 MiB peak (8.3).
- Cold open cost: one 100-byte read at open; the schema load at the first statement reads every
  sqlite_schema page (36 pages for 1,000 tables) and costs ~250 bytes of memory per table. Measured
  cold acquire (evict + close/checkpoint/fsync + open + hash check + read) ~4–5 ms vs warm ~0.15 ms.
- Before a file may be copied or moved: close every connection to it (in every process), then
  check no -wal content, no -shm, no hot journal (connmgr.CheckAtRest); never rename or move a file
  with a connection open (B5).
- Settings a rebuilt file carries: page_size, journal_mode=wal, auto_vacuum, encoding,
  user_version, application_id (and the deprecated default_cache_size). Settings the platform must
  store and reapply on every open: synchronous, foreign_keys, cache_size, busy_timeout,
  wal_autocheckpoint, secure_delete (and any rollback journal mode). `settings-json` in 8.1's
  evidence and `sqlitex.SettingPlacements()` are the machine-readable form. A rebuild through
  `VACUUM INTO` additionally needs journal_mode=wal reapplied.
- ATTACH paragraph (documentation only, not demonstrated): from the source inventory,
  `SQLITE_LIMIT_ATTACHED` defaults to 10 (`attach.c:146` checks `nDb >= LIMIT_ATTACHED+2`); per
  the SQLite docs a transaction across attached WAL databases is atomic per file only. Not run.

## Spans in SPANS.txt (facts read from the pinned source)

- `main.c 3360–3716 openDatabase`: allocates the `sqlite3` handle, parses the URI and flags
  (`sqlite3ParseUri`), opens the main database's B-tree (`sqlite3BtreeOpen`, ~3612), gets the
  schema objects for main and temp (empty, not loaded), registers built-in functions and
  auto-extensions, sets up lookaside (`setupLookaside`, ~3687) and the default autocheckpoint of
  1000 (~3690). No schema page is read here.
- `main.c 3722–3728 sqlite3_open`, `3729–3736 sqlite3_open_v2`: one-line wrappers that call
  `openDatabase` (the v2 form passes the caller's flags and VFS name).
- `btree.c 2532–2825 sqlite3BtreeOpen`: handles shared-cache lookup, allocates `BtShared`, opens
  the pager (`sqlite3PagerOpen`, which opens the database file descriptor), and reads the first 100
  bytes (`sqlite3PagerReadFileheader`) to learn the page size and reserve; installs the busy
  handler. This is the 100-byte read 8.1 sees at open.
- `btree.c 3282–3459 lockBtree`: the first real read of page 1 under a shared lock; validates the
  header; if byte 19 is 2 (WAL) and the WAL is not open, opens it (`sqlite3PagerOpenWal`) and
  returns without keeping page 1 (3333–3352), so its caller reads page 1 again.
- `btree.c 3598–3804 btreeBeginTrans`: starts a read or write transaction; the loop at 3711 calls
  `lockBtree` until page 1 is populated — the second page-1 read in 8.1.
- `prepare.c 96–189 sqlite3InitCallback`: called for each row of `SELECT * FROM sqlite_schema`;
  re-parses each CREATE statement to rebuild the in-memory Table/Index objects (the memory
  SCHEMA_USED counts).
- `prepare.c 199–427 sqlite3InitOne`: loads one database's schema: reads the meta values (schema
  cookie, file format, text encoding, default cache size) through `sqlite3BtreeGetMeta`, then runs
  the schema query that feeds `sqlite3InitCallback`.
- `prepare.c 438–464 sqlite3Init`: loads main first, then every attached database whose schema is
  not loaded.
- `prepare.c 470–484 sqlite3ReadSchema`: called by the parser on the first name lookup; calls
  `sqlite3Init` if the schema is not yet loaded — this is why the schema cost lands on the first
  statement.
- `prepare.c 323–333` (inside sqlite3InitOne): if the connection's cache_size is still unset,
  the absolute value of header bytes 48–51 (default_cache_size) becomes it, else
  SQLITE_DEFAULT_CACHE_SIZE (−2000).
- `prepare.c 492–533 schemaIsValid`: after a prepare error, compares each database's schema
  cookie with the loaded schema and resets a stale one.
- `main.c 1266–1486` close: `sqlite3Close` (1266–1322) disconnects virtual tables, and with
  `forceZombie` (v2) marks the connection a zombie instead of returning SQLITE_BUSY when statements
  or backups are still open; `sqlite3_close` (1363) passes 0, `sqlite3_close_v2` (1364) passes 1;
  `sqlite3LeaveMutexAndCloseZombie` (1375–1486) returns early while the connection is still in use
  and otherwise does the real teardown: closes every B-tree (→ pager → WAL close), frees schemas
  and lookaside. modernc's `conn.Close` calls `sqlite3_close_v2`. B2 is NOT this zombie path
  (corrected in the repair round): `(*sql.DB).Close` closes only the idle connections in its pool
  and never calls the driver's `Close` on a connection that a `*sql.Rows` has checked out
  (database/sql `sql.go` `(*DB).Close`). When `rows.Close()` runs later, database/sql closes the
  statement first and then, because the DB is closed, calls the driver's `Close` →
  `sqlite3_close_v2` on a connection with no open statements, so the zombie path is not taken.
  B2 shows the same hazard one layer up, in database/sql's pool: the SQLite connection stays open
  until the rows are closed.
- `main.c 772–878 setupLookaside`: allocates the connection's lookaside buffer (one block of
  sz × cnt bytes split into large and small slots); `global.c 218–224` sets the default 1200,40
  ("48KB of memory").
- `main.c 897–915 sqlite3_db_release_memory`: for every attached database calls
  `sqlite3PagerShrink`; `pragma.c 2439–2442` shows `PRAGMA shrink_memory` is exactly this call.
- `status.c 203–421 sqlite3_db_status64`: LOOKASIDE_USED reports slots; CACHE_USED sums
  `sqlite3PagerMemUsed` over the connection's pagers (`pager.c 6881–6887`: page-cache memory plus
  the Pager allocation, which contains the file names); SCHEMA_USED and STMT_USED measure by
  pretending to free every schema object / statement with `pnBytesFreed` set, counting bytes
  instead of freeing; hits/misses/writes/spill are pager counters.
- `pcache1.c 621–637 pcache1EnforceMaxPage`: recycles pages from the group's LRU list until the
  group's purgeable count is at most `nMaxPage`. `839–851 pcache1Shrink`: sets the group's
  `nMaxPage` to 0, runs `pcache1EnforceMaxPage`, restores it — with one shared group, every
  connection's unpinned pages go.
- `os_unix.c 246–250 UnixUnusedFd`: a parked descriptor (fd, open flags, next).
  `2101–2109 setPendingFd`: moves a closing file's descriptor onto the inode's unused list instead
  of closing it. `2341–2371 unixClose`: unlocks, and if the inode still has locks held by other
  connections in this process (`pInode->nLock`) parks the descriptor, because closing it would
  release the process's POSIX locks. `1471–1482 closePendingFds`: closes every parked descriptor
  once the lock count reaches 0 or the inode is released. `6369–6414 findReusableFd`: a later open
  of the same file with compatible flags reuses a parked descriptor.
- `os_unix.c 1623–1632 fileHasMoved`: true when the path no longer stats to the same inode as the
  open descriptor (or the file was unlinked). `pager.c 4188–4205 databaseIsUnmoved`: asks the VFS
  (`SQLITE_FCNTL_HAS_MOVED`) and returns SQLITE_READONLY_DBMOVED if moved.
- `pager.c 4222–4279 sqlite3PagerClose`: passes a buffer to `sqlite3WalClose` (allowing the
  checkpoint) only when `SQLITE_NoCkptOnClose` is off and the database has not moved; then
  closes the journal and the database descriptor and frees the cache.
- `wal.c 2507–2571 sqlite3WalClose`: if it can take an EXCLUSIVE lock on the database file (only
  possible when no other connection anywhere holds a lock), runs a PASSIVE checkpoint; if that
  succeeds, deletes the WAL unless PERSIST_WAL is set (then truncates it to 0 only if
  journal_size_limit ≥ 0); closes the wal-index (deleting the -shm only when deleting the WAL) and
  the WAL descriptor. `wal.c 1619–1630 walIndexClose`: unmaps the -shm, unlinking it when
  `isDelete`.

## Linux-only parts and the Mac route

- Linux-only: 8.1's strace read counts (prints `LINUX-ONLY:` with `docker compose run --rm lab go
  run ./ch08-…/experiments/8.1-open-cost-and-where-settings-live`); 8.2's descriptor file names
  (prints `LINUX-ONLY:`; the counts are portable via `/dev/fd`). Everything else is portable:
  SCHEMA_USED, the settings table and JSON, descriptor counts, memory via `ps`, all of 8.3 (child
  processes via re-exec, `Process.Kill`, rename, PERSIST_WAL).
- darwin: cross-built only. Untested assumptions there: `/dev/fd` lists the process's open
  descriptors (true on macOS's fdesc); `ps -o rss=` reports KiB; the at-rest and parking behaviour
  is the same `os_unix.c` code. The 8.2 memory margin (256 KiB/connection) was measured on Linux
  only.

## Surprises worth a sentence in the chapter

- `database/sql`'s `DB.Close` returns nil while a `*sql.Rows` is open, and the SQLite connection
  stays open (B2): an eviction must ensure no rows are outstanding, not just that Close returned.
- A renamed-while-open WAL database looks perfectly at rest at its new path and is missing data
  (B5).
- The unbounded manager in 8.3 needed 2598 descriptors and 247 MiB for 866 small databases; the
  bounded one 192 descriptors and 22.8 MiB, at ~30× the per-read latency on a miss.

## Repair round (2026-11-21) — what changed for the prose pass

The sections above are already corrected in place; `agent_work/ch08/REPAIR-REPORT.md` has the
full list. In short: connmgr counts opens only after success, refuses a cancelled ctx before
evicting, skips the at-rest check when Close failed or a connection is still checked out, and
probes SQLite's lock bytes with F_GETLK. Two earlier claims were FALSE and are gone: "every open
WAL connection keeps the -shm" (an exclusive-mode WAL connection has none) and "B2 is the
`sqlite3_close_v2` zombie path" (it is database/sql's pool deferral). The 8.2 margin is specific
to 8.2's workload (IDL-03a: ~320 KiB/connection with 1,000-table schemas). K above the
descriptor limit is a configuration error the manager does not repair on its own (MGR-03a); size
K ≤ (soft limit − reserve) / 3 in WAL mode, as 8.3's preflight does.
