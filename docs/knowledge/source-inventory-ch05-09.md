# Source inventory, chapters 5–9 (pinned SQLite 3.53.4)

Research for the course outline. Not chapter text.

- **Pin:** the pinned tree (`$SQLITE_SRC` or `_src/sqlite-3.53.4`), tag `version-3.53.4`, git `b09c88c1…`, Fossil
  `bf7c7f30…`. All paths below are relative to that tree. Line numbers come from
  `agent_work/probes/source-inventory-b/locate.py` (it finds the column-0 definition header and
  the next column-0 `}`) or from `grep -n`. A "start–end" range covers the whole definition,
  including the return-type line when that line is separate.
- **Verified how:** `L` = located with `locate.py`/`grep -n` and read at the pin.
  `R` = behavior confirmed by running a probe (named in that row). `D` = official docs page
  fetched 2026-11-20 (every sqlite.org URL below returned HTTP 200).
- **Installed library at this pin** (`/usr/local/lib/libsqlite3.so.3.53.4`, `pragma
  compile_options` on `/usr/local/bin/sqlite3`): `DEFAULT_SYNCHRONOUS=2`,
  `DEFAULT_WAL_SYNCHRONOUS=2`, `DEFAULT_WAL_AUTOCHECKPOINT=1000`, `MAX_ATTACHED=10`,
  `DEFAULT_JOURNAL_SIZE_LIMIT=-1`, `ENABLE_DBPAGE_VTAB`, `ENABLE_DBSTAT_VTAB`,
  `ENABLE_BYTECODE_VTAB`, `ENABLE_STMTVTAB`. **Not** enabled: `ENABLE_SNAPSHOT`, `ENABLE_SESSION`,
  `ENABLE_PREUPDATE_HOOK`, `ENABLE_SETLK_TIMEOUT`, `ENABLE_FILESTAT`, `SQLITE_DEBUG`.
  `nm -D` confirms that `sqlite3_snapshot_get`, `sqlite3changeset_start`, `sqlite3_preupdate_hook`
  and `sqlite3_unlock_notify` are **absent**. `sqlite3_setlk_timeout` is exported, but without
  `ENABLE_SETLK_TIMEOUT` it only range-checks `ms` and returns OK (`src/main.c:1877-1906`).
  **Outline consequence:** labs that use snapshots, sessions or blocking locks need their own
  build (see §9 build notes) or a Go driver that bundles its own amalgamation with those flags.

---

## 5. WAL, transactions, checkpoints

### 5.1 Symbols

| Function / structure | File | Lines | Role | Verified |
|---|---|---|---|---|
| WAL file-format comment | src/wal.c | 16–249 | Format: header, frames, checksums, salts. Also READER ALGORITHM (100) and WAL-INDEX FORMAT (127) | L |
| `WAL_WRITE_LOCK` / `WAL_CKPT_LOCK` / `WAL_RECOVER_LOCK` / `WAL_READ_LOCK(I)` / `WAL_NREADER` | src/wal.c | 294 / 296 / 297 / 298 / 299 | shm lock slots 0, 1, 2, 3+I; `WAL_NREADER = SQLITE_SHM_NLOCK-3 = 5` | L |
| `struct WalIndexHdr` | src/wal.c | 321–333 | `iChange`, `mxFrame`, `nPage`, `aFrameCksum`, `aSalt`, `aCksum`; two copies at the start of -shm | L |
| `struct WalCkptInfo` | src/wal.c | 394–400 | `nBackfill`, `aReadMark[5]`, `aLock[8]`, `nBackfillAttempted` | L |
| `READMARK_NOT_USED` | src/wal.c | 401 | `0xffffffff` | L |
| `WAL_FRAME_HDRSIZE` / `WAL_HDRSIZE` / `WAL_MAGIC` | src/wal.c | 477 / 480 / 491 | 24 / 32 / `0x377f0682` (+1 = big-endian checksums) | L, R (showwal) |
| `struct Wal` | src/wal.c | 511–554 | Per-connection WAL handle | L |
| `walIndexPage` / `walCkptInfo` / `walIndexHdr` | src/wal.c | 805–815 / 820–824 / 829–833 | Map and access -shm regions | L |
| `walChecksumBytes` | src/wal.c | 856–912 | Fletcher-like cumulative checksum, native or big-endian | L |
| `walIndexWriteHdr` | src/wal.c | 942–954 | Write both header copies, with a memory barrier between them | L |
| `walEncodeFrame` | src/wal.c | 969–993 | Frame header: pgno, `nTruncate` (db size, **nonzero ⇒ commit frame**), salts, cumulative cksum | L, R |
| `walDecodeFrame` | src/wal.c | 1000–1053 | Validate salt and checksum on read/recovery | L |
| `walLockShared` / `walUnlockShared` / `walLockExclusive` / `walUnlockExclusive` | src/wal.c | 1085–1097 / 1098–1106 / 1107–1121 / 1122–1131 | Wrap `sqlite3OsShmLock` | L |
| `walHashGet` / `walIndexAppend` | src/wal.c | 1173–1195 / 1301–1377 | Hash table: page → frame | L |
| `walIndexRecover` | src/wal.c | 1390–1614 | Rebuild -shm by scanning -wal (locks `WAL_ALL_BUT_WRITE`…read slots at 1407) | L |
| `sqlite3WalOpen` | src/wal.c | 1647–1745 | Opens -wal (`sqlite3OsOpen` at 1726) | L |
| `sqlite3WalLimit` | src/wal.c | 1750–1752 | `journal_size_limit` for WAL | L |
| `walIteratorInit` | src/wal.c | 1954–2017 | Frames ordered by page for backfill | L |
| `walEnableBlocking` / `sqlite3WalWriteLock` | src/wal.c | 2039–2048 / 2067–2084 | SETLK_TIMEOUT helpers | L |
| `walBusyLock` | src/wal.c | 2107–2125 | Exclusive lock retried through `xBusy` (2117) | L |
| `walRestartHdr` | src/wal.c | 2152–2166 | WAL reset: `nCkpt++`, `mxFrame=0`, salt1+1, new salt2, `nBackfill=0`, read marks reset | L |
| `walCheckpoint` | src/wal.c | 2199–2395 | Computes `mxSafeFrame` from read marks (2228–2246), backfills to db, fsyncs WAL then db, RESTART/TRUNCATE tail (2353–2384) | L |
| `walLimitSize` | src/wal.c | 2401–2413 | Truncate -wal to `journal_size_limit` | L |
| `sqlite3WalClose` | src/wal.c | 2507–2571 | On close: EXCLUSIVE db lock ⇒ PASSIVE checkpoint ⇒ unlink -wal unless PERSIST_WAL (2538–2563) | L, R |
| `walIndexTryHdr` / `walIndexReadHdr` | src/wal.c | 2590–2640 / 2660–2763 | Read the -shm header; falls back to recovery (2730) | L |
| `walBeginShmUnreliable` | src/wal.c | 2792–2937 | Read-only/unreliable -shm path | L |
| `walTryBeginRead` | src/wal.c | 3020–3272 | Choose/claim `aReadMark[i]`; snapshot = `mxFrame`; `SQLITE_PROTOCOL` after `WAL_RETRY_PROTOCOL_LIMIT` (3053) | L |
| `sqlite3WalSnapshotRecover` | src/wal.c | 3342–3367 | `sqlite3_snapshot_recover` backend | L |
| `walBeginReadTransaction` / `sqlite3WalBeginReadTransaction` | src/wal.c | 3374–3477 / 3493–3500 | Read-txn entry; snapshot-open check at 3386–3389 (`ENABLE_SNAPSHOT` only) | L |
| `sqlite3WalEndReadTransaction` | src/wal.c | 3506–3515 | Drop read lock | L |
| `walFindFrame` / `sqlite3WalFindFrame` | src/wal.c | 3525–3627 / 3640–3651 | Newest frame ≤ snapshot for a page | L |
| `sqlite3WalBeginWriteTransaction` | src/wal.c | 3699–3746 | `WAL_WRITE_LOCK`; **`SQLITE_BUSY_SNAPSHOT`** if header changed since read began (3734–3736) | L |
| `sqlite3WalEndWriteTransaction` | src/wal.c | 3752–3760 | Release write lock | L |
| `walRestartLog` | src/wal.c | 3875–3915 | At first write: if fully backfilled and no reader uses the WAL, reset to frame 0 (`walRestartHdr` at 3896) | L |
| `walWriteToLog` / `walWriteOneFrame` | src/wal.c | 3938–3958 / 3963–3979 | `pwrite` frame header + page; sync at `iSyncPoint` (3953) | L, R |
| `walFrames` / `sqlite3WalFrames` | src/wal.c | 4038–4266 / 4275–4289 | Append frames; commit sync (4195–4212); index append (4238/4244); header publish (4259) | L, R |
| `sqlite3WalCheckpoint` | src/wal.c | 4301–4432 | Lock `CKPT`; for FULL+ take `WRITE` via `walBusyLock` (4358); NOOP skips work (4341) | L |
| `sqlite3WalCallback` | src/wal.c | 4439–4446 | Frames written by last commit (feeds wal_hook) | L |
| `sqlite3WalExclusiveMode` / `sqlite3WalHeapMemory` | src/wal.c | 4472–4509 / 4516–4518 | `locking_mode=EXCLUSIVE` ⇒ heap wal-index, no -shm | L |
| `sqlite3WalSnapshotGet` / `sqlite3WalSnapshotOpen` / `sqlite3WalSnapshotCheck` | src/wal.c | 4525–4545 / 4549–4567 / 4597–4613 | Snapshot = copy of `WalIndexHdr` | L |
| `WAL_SYNC_FLAGS(X)` / `CKPT_SYNC_FLAGS(X)` | src/wal.h | 25 / 26 | Low 2 bits = commit sync; bits 2–3 = checkpoint sync | L |
| `pagerUseWal` | src/pager.c | 829 (macro) | `pWal!=0` | L |
| `sqlite3PagerSetFlags` | src/pager.c | 3658–3704 | Maps `synchronous` / `fullfsync` / `checkpoint_fullfsync` to `syncFlags` and `walSyncFlags` (3692–3698) | L, R |
| `pagerUndoCallback` / `pagerRollbackWal` | src/pager.c | 3158–3188 / 3193–3214 | WAL rollback = discard cached pages | L |
| `pagerWalFrames` | src/pager.c | 3225–3282 | Pager → `sqlite3WalFrames` (3265); feeds backup (3270) | L |
| `pagerBeginReadTransaction` | src/pager.c | 3292–3313 | → `sqlite3WalBeginReadTransaction` (3306) | L |
| `pagerOpenWalIfPresent` | src/pager.c | 3385–3413 | -wal exists ⇒ open WAL; empty -wal + db size 0 ⇒ delete (3402) | L |
| `sqlite3PagerCheckpoint` | src/pager.c | 7556–7585 | → `sqlite3WalCheckpoint` (7577) | L |
| `pagerOpenWal` / `sqlite3PagerOpenWal` / `sqlite3PagerCloseWal` | src/pager.c | 7627–7659 / 7677–7705 / 7716–7752 | WAL open/close on journal-mode change | L |
| `sqlite3PagerJournalSizeLimit` | src/pager.c | 7519–7525 | Also calls `sqlite3WalLimit` | L |
| `sqlite3PagerSnapshotGet` / `sqlite3PagerSnapshotOpen` | src/pager.c | 7786–7792 / 7799–7810 | Snapshot glue | L |
| `sqlite3WalDefaultHook` | src/main.c | 2484–2496 | Autocheckpoint: if frames ≥ N ⇒ `sqlite3_wal_checkpoint` (2492, PASSIVE) | L |
| `sqlite3_wal_autocheckpoint` / `sqlite3_wal_hook` | src/main.c | 2510–2525 / 2531–2556 | Autocheckpoint is just a wal_hook (2519); default 1000 set in `openDatabase` (3690) | L |
| `sqlite3_wal_checkpoint_v2` / `sqlite3_wal_checkpoint` / `sqlite3Checkpoint` | src/main.c | 2561–2623 / 2631–2635 / 2658–2682 | Public API → per-db checkpoint (2609, 2671) | L |
| `sqlite3_snapshot_get` / `_open` / `_recover` / `_free` | src/main.c | 5060–5095 / 5100–5150 / 5156–5182 / 5187–5189 | Inside `#ifdef SQLITE_ENABLE_SNAPSHOT` (5055–5190); **not in the installed lib** | L, R (`nm`) |
| `doWalCallbacks` | src/vdbeapi.c | 745–… (call at 886) | After commit: `xWalCallback(db, zDb, nFrame)` (757) | L |
| `OP_Checkpoint` | src/vdbe.c | doc 8004; case 8015–8041 | `PRAGMA wal_checkpoint` → `sqlite3Checkpoint` (8029) | L |
| `OP_JournalMode` | src/vdbe.c | 8054–8147 | `PRAGMA journal_mode` switch | L |
| `PragTyp_WAL_CHECKPOINT` / `PragTyp_WAL_AUTOCHECKPOINT` / `PragTyp_JOURNAL_SIZE_LIMIT` / `PragTyp_SYNCHRONOUS` | src/pragma.c | 2393 / 2421 / 780 / 1132 | Pragma cases; `NOOP` keyword parsed at 2404 | L |
| `SQLITE_CHECKPOINT_NOOP..TRUNCATE` | src/sqlite.h.in | 10178–10182 | NOOP=-1, PASSIVE=0, FULL=1, RESTART=2, TRUNCATE=3; mode docs 10074–10108 | L, D |
| `SQLITE_FCNTL_PERSIST_WAL` | src/sqlite.h.in | 1252 | Keep -wal after last close; handled at `src/os_unix.c:4183` | L |
| `SQLITE_DBCONFIG_NO_CKPT_ON_CLOSE` | src/sqlite.h.in | 2705 | → `SQLITE_NoCkptOnClose` (`src/sqliteInt.h:1849`), tested at `src/pager.c:4235` | L, R |
| WAL2 / `BEGIN CONCURRENT` | — | — | **Absent at this pin.** `grep -i wal2` over `src/` finds nothing; `parse.y` has no CONCURRENT transtype (only DEFERRED default at 186). They live on separate branches. | L |

### 5.2 Call paths

**Commit of an autocommit write in WAL mode**
1. `sqlite3_step` (`src/vdbeapi.c:919-966`) → `sqlite3Step` (777–912) → `sqlite3VdbeExec` (`vdbeapi.c:869`).
2. `OP_Halt` → `sqlite3VdbeHalt` (`src/vdbe.c:1354`). For explicit `COMMIT`: `OP_AutoCommit` → `sqlite3VdbeHalt` (`vdbe.c:4042`).
3. `sqlite3VdbeHalt` → `vdbeCommit` (`src/vdbeaux.c:3421`) → `sqlite3BtreeCommitPhaseOne` (`vdbeaux.c:3005`; single-file case).
4. `sqlite3BtreeCommitPhaseOne` → `sqlite3PagerCommitPhaseOne` (`src/btree.c:4330`) → `pagerWalFrames(pPager, pList, dbSize, 1)` (`src/pager.c:6558`).
5. `pagerWalFrames` → `sqlite3WalFrames` (`pager.c:3265`, passes `walSyncFlags`) → `walFrames` (`src/wal.c:4285`).
6. `walFrames` → `walRestartLog` (`wal.c:4079`). If the WAL is empty, it writes the 32-byte WAL header and syncs it with `CKPT_SYNC_FLAGS` (`wal.c:4122`).
7. `walFrames` → `walWriteOneFrame` per page (`wal.c:4168`) → `walEncodeFrame` + `walWriteToLog` (`wal.c:3973-3977`). The last frame carries `nTruncate` = db size, which marks the commit.
8. Commit sync if `WAL_SYNC_FLAGS(sync_flags)` is set (`wal.c:4195-4212`). Optional sector padding repeats the last frame.
9. `walIndexAppend` per frame (`wal.c:4238`, `4244`), then `walIndexWriteHdr` (`wal.c:4259`). New readers see the commit from this point.
10. Back in `sqlite3BtreeCommitPhaseTwo` (`vdbeaux.c:3020`) → `sqlite3PagerCommitPhaseTwo` (`btree.c:4416`).
11. `sqlite3Step` → `doWalCallbacks` (`vdbeapi.c:886`) → `sqlite3PagerWalCallback` (`vdbeapi.c:754`) → `db->xWalCallback` (`vdbeapi.c:757`), by default `sqlite3WalDefaultHook` → `sqlite3_wal_checkpoint` (`src/main.c:2492`) ⇒ PASSIVE autocheckpoint.

**Read transaction / snapshot**
1. `OP_Transaction` → `sqlite3BtreeBeginTrans` (`src/vdbe.c:4128`) → `btreeBeginTrans` → `lockBtree` (`src/btree.c:3711`) → `sqlite3PagerSharedLock` (`btree.c:3290`).
2. `sqlite3PagerSharedLock` → `pagerOpenWalIfPresent` (`pager.c:5482`) → `sqlite3PagerOpenWal` (`pager.c:3405`) → `pagerOpenWal` (`pager.c:7695`) → `sqlite3WalOpen` (`pager.c:7646`).
3. `sqlite3PagerSharedLock` → `pagerBeginReadTransaction` (`pager.c:5490`) → `sqlite3WalBeginReadTransaction` (`pager.c:3306`) → `walBeginReadTransaction` (`wal.c:3496`) → `walTryBeginRead` (`wal.c:3412`).
4. `walTryBeginRead` → `walIndexReadHdr` (`wal.c:3082`) → `walIndexTryHdr` (`wal.c:2700`). If the header is bad: `walIndexRecover` (`wal.c:2730`).
5. `walTryBeginRead` picks the `aReadMark[i]` closest to `mxFrame` (comment at 3171) and holds `WAL_READ_LOCK(i)` shared. That frame bound is the reader's snapshot.
6. Page read: `pager.c:3078` / `5720` → `sqlite3WalFindFrame` → `walFindFrame` (`wal.c:3647`).

**Checkpoint (API or pragma)**
1. `sqlite3_wal_checkpoint_v2` → `sqlite3Checkpoint` (`main.c:2609`); `PRAGMA wal_checkpoint` → `OP_Checkpoint` → `sqlite3Checkpoint` (`vdbe.c:8029`).
2. `sqlite3Checkpoint` → `sqlite3BtreeCheckpoint` (`main.c:2671`) → `sqlite3PagerCheckpoint` (`btree.c:11349`) → `sqlite3WalCheckpoint` (`pager.c:7577`).
3. `sqlite3WalCheckpoint`: CKPT lock; for FULL/RESTART/TRUNCATE, `walBusyLock(WAL_WRITE_LOCK)` (`wal.c:4358`); → `walCheckpoint` (`wal.c:4395`).
4. `walCheckpoint`: `mxSafeFrame` limited by read marks (`wal.c:2233-2246`, busy-waits via `walBusyLock` 2239) → `walIteratorInit` (2255) → fsync WAL (`2277`, CKPT flags) → `SQLITE_FCNTL_CKPT_START` (2285) → copy pages → `SQLITE_FCNTL_CKPT_DONE` (2324) → truncate db if shrunk (2331) → fsync db (2333).
5. RESTART/TRUNCATE: wait for all readers (`walBusyLock(WAL_READ_LOCK(1), WAL_NREADER-1)` at 2367); TRUNCATE also `walRestartHdr` + `sqlite3OsTruncate(-wal, 0)` (2383–2384).
6. The WAL is *reused from frame 0* only by the next writer's `walRestartLog` (`wal.c:4079` → `3896`). A reader holding a read mark prevents it, which is the mechanism behind "WAL growth under a long-lived reader".

**Close**: `sqlite3PagerClose` → `sqlite3WalClose` (`pager.c:4240`; skipped checkpoint when `SQLITE_NoCkptOnClose`, `pager.c:4235`) → EXCLUSIVE db lock → PASSIVE checkpoint (`wal.c:2533-2535`) → `walLimitSize(0)` or `sqlite3OsDelete(-wal)` (2554/2563). Observed: close without `no_ckpt_on_close` unlinks `-wal` and `-shm` (probe `fsync-per-commit.sh`, `wal+close` rows).

### 5.3 `synchronous` in WAL mode (code + probe)
`walSyncFlags = syncFlags<<2` (checkpoint bits, set unless OFF), plus `|= syncFlags` only when `fullSync` (FULL/EXTRA) (`pager.c:3692-3695`). `checkpoint_fullfsync` forces `SQLITE_SYNC_FULL<<2` (3697).
**Caveat for the outline:** the comment at `pager.c:607-617` says the low bits are for checkpoints and the high bits for commits. That is the reverse of `wal.h:25-26` and of the code, so cite the code.

| Level (WAL) | Per-commit fsync (steady state) | First commit into a fresh -wal | Checkpoint | Probe |
|---|---|---|---|---|
| OFF | 0 | 0 | 0 | `fsync-per-commit.sh` |
| NORMAL | **0** | fsync -wal (header) + fsync dir | fsync -wal, then db | `wal-steady-sync.sh` |
| FULL / EXTRA | **1** (-wal) | + the header and dir fsyncs above | fsync -wal, then db | `wal-steady-sync.sh` |

### 5.4 Docs
https://sqlite.org/wal.html (incl. #walresetbug), https://sqlite.org/walformat.html,
https://sqlite.org/fileformat2.html (§WAL), https://sqlite.org/c3ref/wal_checkpoint_v2.html,
https://sqlite.org/c3ref/c_checkpoint_full.html, https://sqlite.org/c3ref/wal_hook.html,
https://sqlite.org/c3ref/wal_autocheckpoint.html, https://sqlite.org/c3ref/snapshot_get.html,
https://sqlite.org/c3ref/snapshot_open.html, https://sqlite.org/pragma.html (wal_checkpoint,
wal_autocheckpoint, journal_size_limit, synchronous), https://sqlite.org/c3ref/c_dbconfig_defensive.html.

### 5.5 Reading path
1. `src/wal.c:16-249`: format comment (file format, reader algorithm, wal-index).
2. `src/wal.c:294-401`: lock slots, `WalIndexHdr`, `WalCkptInfo`.
3. `src/wal.c:856-1053`: checksum, encode/decode frame.
4. `src/wal.c:4038-4266` `walFrames`, with `src/pager.c:3225-3282` `pagerWalFrames`.
5. `src/wal.c:3020-3272` `walTryBeginRead` (read marks), then `3699-3746` (BUSY_SNAPSHOT).
6. `src/wal.c:2199-2395` `walCheckpoint`, then `4301-4432` `sqlite3WalCheckpoint` (modes).
7. `src/wal.c:2152-2166` + `3875-3915`: reset/restart and the salt change.
8. `src/wal.c:1390-1614` `walIndexRecover`.
9. `src/main.c:2484-2525` autocheckpoint hook; `src/pager.c:3658-3704` sync flags.

### 5.6 Files to copy
| File | Lines | Bytes |
|---|---|---|
| src/wal.c | 4645 | 177747 |
| src/wal.h | 160 | 6127 |
| src/pager.c | 7880 | 304085 (cite only the WAL sections; whole file because the lab cites both WAL and journal paths) |

### 5.7 Observation hooks
- Files: `-wal` (size = 32 + n·(24+pgsz)), `-shm` (32 KiB regions). Strace: `pwrite64` at WAL offsets 0, 32, 56, 4152, 4176… (probe `wal-steady-sync.sh`).
- `PRAGMA wal_checkpoint[(PASSIVE|FULL|RESTART|TRUNCATE|NOOP)]` returns `busy, log, checkpointed`. NOOP gives a cheap frame count.
- `sqlite3_wal_hook` (frames per commit), `PRAGMA wal_autocheckpoint`, `PRAGMA journal_size_limit`.
- Built tools: `showwal FILE` (header + frames; verified prints magic/salts/checksums), `showshm`.
- `sqlite_dbpage` vtab (installed lib has `ENABLE_DBPAGE_VTAB`) reads pages through the WAL snapshot.
- `sqlite3_file_control(SQLITE_FCNTL_PERSIST_WAL)`, `.dbconfig no_ckpt_on_close on` in the CLI.

### 5.8 Version notes
- 3.51.0 added `SQLITE_CHECKPOINT_NOOP` and `PRAGMA wal_checkpoint=NOOP` (changes.html).
- 3.51.3 and 3.53.0 fix the "WAL-reset database corruption bug" (wal.html#walresetbug). The pin (3.53.4) includes the fix; older 3.x builds in Go drivers may not.
- 3.50.2: no more checksum-less frames after a savepoint rollback with spilled pages.

---

## 6. Locking and concurrency

### 6.1 Symbols

| Function / structure | File | Lines | Role | Verified |
|---|---|---|---|---|
| `NO_LOCK..EXCLUSIVE_LOCK` | src/os.h | 98–102 | 0..4 | L |
| `PENDING_BYTE` / `RESERVED_BYTE` / `SHARED_FIRST` / `SHARED_SIZE` | src/os.h | 160 (or `sqlite3PendingByte`, 162) / 164 / 165 / 166 | 0x40000000, +1, +2, 510 bytes | L |
| `struct UnixUnusedFd` | src/os_unix.c | 246–250 | fd parked until it can be closed or reused | L |
| `struct unixFile` | src/os_unix.c | 257–312 | Per-open-file state (`h`, `eFileLock`, `pInode`, `pShm`, `ctrlFlags`) | L |
| `unixPosixAdvisoryLocks` | src/os_unix.c | 638–707 (stub 709) | Reads `/proc/PID/fdinfo` → "AAA/B.CCC.DDDDD" lock map; **only** with `SQLITE_DEBUG`/`ENABLE_FILESTAT` | L |
| "Posix Advisory Locking" comment | src/os_unix.c | 1189–~1278 | Why POSIX locks are per-process, the inode table, and deferred close | L |
| `struct unixFileId` / `struct unixInodeInfo` | src/os_unix.c | 1282–1296 / 1323–1342 | Key (dev, ino); `nShared`, `eFileLock`, `bProcessLock`, `pUnused` | L |
| `closePendingFds` / `releaseInodeInfo` / `findInodeInfo` | src/os_unix.c | 1471–1482 / 1490–1516 / 1527–1618 | The inode-sharing workaround | L |
| `unixCheckReservedLock` | src/os_unix.c | 1677–1716 | Probes RESERVED_BYTE | L |
| `osSetPosixAdvisoryLock` | src/os_unix.c | 1737–1779 | `F_SETLK`, or `F_SETLKW` with timeout when SETLK_TIMEOUT is set | L |
| `unixFileLock` | src/os_unix.c | 1802–1833 | Single fcntl; `bProcessLock` shortcut; `F_SETLKW` at 1827 | L |
| `unixLock` (+ doc 1843–1865) | src/os_unix.c | 1866–2095 | SHARED→RESERVED→PENDING→EXCLUSIVE byte-range logic; broken-lock check `unixIsSharingShmNode` (2034–2041) | L |
| `setPendingFd` | src/os_unix.c | 2101–2109 | Defer `close()` while other fds on the inode hold locks | L |
| `posixUnlock` / `unixUnlock` | src/os_unix.c | 2124–2275 / 2284–2289 | Downgrade/release | L |
| `unixClose` | src/os_unix.c | 2341–2371 | Close with pending-fd logic | L |
| `struct unixShmNode` / `struct unixShm` | src/os_unix.c | 4556–4575 / 4590–4597 | Shared -shm state per inode / per connection | L |
| `UNIX_SHM_BASE` / `UNIX_SHM_DMS` | src/os_unix.c | 4602 / 4603 | Lock bytes at -shm offset 120..127, DMS = 128 | L |
| `unixIsSharingShmNode` | src/os_unix.c | 4679–4695 | Detects locks broken by a stray `close()` | L |
| `unixShmSystemLock` | src/os_unix.c | 4703–4786 | fcntl on -shm bytes | L |
| `unixLockSharedMemory` | src/os_unix.c | 4850–4915 | Dead-man switch: WRLCK DMS ⇒ first opener truncates -shm (4893/4912) | L |
| `unixOpenSharedMemory` / `unixShmMap` / `unixShmLock` / `unixShmBarrier` / `unixShmUnmap` | src/os_unix.c | 4952–5085 / 5106–5239 / 5284–5476 / 5484–5494 / 5503–5546 | xShm* methods | L |
| `walLockShared` / `walLockExclusive` | src/wal.c | 1085–1097 / 1107–1121 | → `sqlite3OsShmLock` (1088/1110) | L |
| `WAL_*_LOCK` slots | src/wal.c | 294–299 | WRITE 0, CKPT 1, RECOVER 2, READ0..4 = 3..7 | L |
| `walBusyLock` | src/wal.c | 2107–2125 | Only WAL-level busy loop (checkpoint) | L |
| `SQLITE_BUSY_RECOVERY` return | src/wal.c | 3113 | Reader blocked by recovery | L |
| `sqliteDefaultBusyCallback` | src/main.c | 1730–1771 | Sleeps 1,2,5,10,15,20,25,25,25,50,50,100 ms until `busyTimeout` | L |
| `sqlite3InvokeBusyHandler` | src/main.c | 1782–1792 | Calls the handler with a count | L |
| `sqlite3_busy_handler` / `sqlite3_busy_timeout` / `sqlite3_setlk_timeout` | src/main.c | 1798–1816 / 1855–1872 / 1877–1906 | API | L |
| `btreeInvokeBusyHandler` | src/btree.c | 2504–2509 | Installed into the pager at `btree.c:2693` | L |
| `btreeBeginTrans` / `sqlite3BtreeBeginTrans` | src/btree.c | 3598–3804 / 3805–3826 | Busy retry loop `while BUSY && TRANS_NONE && btreeInvokeBusyHandler` (3740–3741) | L |
| `querySharedCacheTableLock` | src/btree.c | 329–380 | Source of `SQLITE_LOCKED_SHAREDCACHE` (355/376) | L |
| `pagerUnlockDb` / `pagerLockDb` / `pager_wait_on_lock` | src/pager.c | 1133–1149 / 1161–1173 / 3992–4009 | Busy loop for SHARED/EXCLUSIVE (4007) | L |
| `sqlite3PagerSetBusyHandler` | src/pager.c | 3769–3781 | Busy-invocation table in comment 3758 | L |
| `sqlite3PagerBegin` | src/pager.c | 5968–6032 | WAL: `sqlite3WalBeginWriteTransaction` (5995); rollback: RESERVED then EXCLUSIVE if `exFlag` (6003–6004) | L |
| `sqlite3BeginTransaction` | src/build.c | 5245–5274 | DEFERRED emits nothing; IMMEDIATE ⇒ `OP_Transaction p2=1`; EXCLUSIVE ⇒ p2=2 (5269) | L |
| `OP_Transaction` | src/vdbe.c | doc 4067; case 4102–4201 | Begin txn + schema-cookie check | L |
| `OP_AutoCommit` | src/vdbe.c | 4013–4065 | COMMIT/ROLLBACK; BUSY ⇒ retry (4042) | L |
| `PragTyp_LOCKING_MODE` / `PragTyp_LOCK_STATUS` | src/pragma.c | 687 / 2743 (DEBUG/TEST only, guard 2738) | Pragmas | L |
| `SQLITE_BUSY_RECOVERY/SNAPSHOT/TIMEOUT`, `SQLITE_LOCKED_SHAREDCACHE` | src/sqlite.h.in | 544 / 545 / 546 / 542 | Extended codes | L |
| OFD locks | — | — | **Not used.** No `F_OFD_*` anywhere in os_unix.c; POSIX `F_SETLK`/`F_SETLKW` only | L |

### 6.2 Call paths
**Rollback-mode write lock:** `OP_Transaction p2≥1` → `sqlite3BtreeBeginTrans` (`vdbe.c:4128`) → `btreeBeginTrans` → `lockBtree` (3711, SHARED via `sqlite3PagerSharedLock` → `pager_wait_on_lock(SHARED_LOCK)` `pager.c:5318`) → `sqlite3PagerBegin` (`btree.c:3717`) → `pagerLockDb(RESERVED)` / `pager_wait_on_lock(EXCLUSIVE)` (`pager.c:6003-6004`) → `sqlite3OsLock` (`os.c:107`) → `unixLock` → `unixFileLock` (`os_unix.c:1980..2059`) → `osSetPosixAdvisoryLock` (1815/1830) → `fcntl(F_SETLK)`. EXCLUSIVE at commit time comes from `pager_wait_on_lock(EXCLUSIVE_LOCK)` in `sqlite3PagerExclusiveLock` (`pager.c:6479`).

**WAL write lock:** `sqlite3PagerBegin` → `sqlite3WalBeginWriteTransaction` (`pager.c:5995`) → `walLockExclusive(WAL_WRITE_LOCK)` → `sqlite3OsShmLock` (`wal.c:1110`) → `unixShmLock` → `unixShmSystemLock(F_WRLCK, ofst+UNIX_SHM_BASE)` (`os_unix.c:5450`). On failure, BUSY bubbles up to `btreeBeginTrans`, which loops on `btreeInvokeBusyHandler` (`btree.c:3741`) → `sqlite3InvokeBusyHandler` (2508) → `sqliteDefaultBusyCallback`. The pager comment says the busy handler is not invoked inside the WAL layer for the write lock (`pager.c:5991-5994`).

**`SQLITE_BUSY_SNAPSHOT`:** a deferred read txn is upgraded to a write after another writer committed. `sqlite3WalBeginWriteTransaction` compares the header (`wal.c:3734-3736`). Retrying with the busy handler cannot help (the txn is no longer TRANS_NONE, `btree.c:3740`), so the application must roll back and restart. That is why `BEGIN IMMEDIATE` is recommended for writers.

**BEGIN modes in WAL:** EXCLUSIVE differs from IMMEDIATE only when `locking_mode=EXCLUSIVE` (`pager.c:5982-5988`). Otherwise both take just `WAL_WRITE_LOCK`, and readers continue.

### 6.3 Docs
https://sqlite.org/lockingv3.html, https://sqlite.org/isolation.html, https://sqlite.org/wal.html
(concurrency, §"Sometimes Queries Return SQLITE_BUSY"), https://sqlite.org/walformat.html (§shm
locks), https://sqlite.org/lang_transaction.html, https://sqlite.org/c3ref/busy_handler.html,
https://sqlite.org/c3ref/busy_timeout.html, https://sqlite.org/c3ref/setlk_timeout.html,
https://sqlite.org/rescode.html, https://sqlite.org/pragma.html#pragma_locking_mode,
https://sqlite.org/sharedcache.html (pointer only), https://sqlite.org/howtocorrupt.html §2.

### 6.4 Reading path
1. `src/os.h:98-166`: lock levels and byte ranges.
2. `src/os_unix.c:1189-1342`: the POSIX-locks-are-broken comment and the inode structs.
3. `src/os_unix.c:1843-2095` `unixLock` (doc + body), then `2124-2275` `posixUnlock`.
4. `src/os_unix.c:2101-2109`, `1471-1482`: deferred close (`setPendingFd`/`closePendingFds`).
5. `src/wal.c:294-299`, `1085-1131`: WAL lock slots and wrappers.
6. `src/os_unix.c:4556-4603`, `5284-5476`: -shm lock implementation and DMS.
7. `src/pager.c:5968-6032` `sqlite3PagerBegin`; `src/build.c:5245-5274` BEGIN modes.
8. `src/btree.c:3598-3804` (busy retry), `src/main.c:1730-1792` (default busy callback).
9. `src/wal.c:3699-3746` BUSY_SNAPSHOT; `src/wal.c:3020-3272` reader retries/PROTOCOL.

### 6.5 Files to copy
| File | Lines | Bytes |
|---|---|---|
| src/os_unix.c | 8589 | 295468 |
| src/os.h | 225 | 9393 |
| src/wal.c (shared with ch5) | 4645 | 177747 |

### 6.6 Observation hooks
- `/proc/locks` and `/proc/<pid>/fdinfo/<fd>` (`lock:` lines) show POSIX byte ranges on the db (0x40000000…) and -shm (120–128).
- strace: `fcntl(fd, F_SETLK, {l_type=F_RDLCK|F_WRLCK, l_start=1073741824…})`.
- Return codes: `sqlite3_extended_errcode` ⇒ 5 (BUSY), 517 (BUSY_SNAPSHOT), 261 (BUSY_RECOVERY), 773 (BUSY_TIMEOUT).
- `sqlite3_txn_state` (`main.c:1328-1352`), `PRAGMA locking_mode`, `PRAGMA busy_timeout`.
- `SQLITE_FCNTL_LOCKSTATE` and `PRAGMA lock_status` exist only in DEBUG/TEST builds. `SQLITE_FCNTL_FILESTAT` (JSON incl. "pal" lock map) needs DEBUG or `ENABLE_FILESTAT`. Neither is in the installed lib.

### 6.7 Version notes
- 3.50.0 added `sqlite3_setlk_timeout()` (effective only with `SQLITE_ENABLE_SETLK_TIMEOUT`); 3.50.1/3.50.2 fixed it.
- 3.51.0 adds "resistance to corruption caused by breaking POSIX advisory locks using close()": `unixIsSharingShmNode` check in `unixLock` (`os_unix.c:2034-2041`). 3.51.2 fixed a deadlock in it.

---

## 7. VFS and filesystem I/O

### 7.1 Symbols

| Function / structure | File | Lines | Role | Verified |
|---|---|---|---|---|
| `SQLITE_OPEN_*` flags | src/sqlite.h.in | doc 584; 611 MAIN_DB … 617 SUPER_JOURNAL … 622 WAL | File-type bits passed to xOpen (MAIN_DB, TEMP_DB, TRANSIENT_DB, MAIN_JOURNAL, TEMP_JOURNAL, SUBJOURNAL, SUPER_JOURNAL, WAL) | L |
| `SQLITE_IOCAP_*` | src/sqlite.h.in | doc 632; 671 ATOMIC … 683 POWERSAFE_OVERWRITE, 685 BATCH_ATOMIC, 686 SUBPAGE_READ | Device characteristics | L |
| `SQLITE_SYNC_FULL` / `SQLITE_SYNC_DATAONLY` | src/sqlite.h.in | 732 / 733 | xSync flags | L |
| `struct sqlite3_file` | src/sqlite.h.in | 747–749 | Just `pMethods` (subclassed) | L |
| `struct sqlite3_io_methods` | src/sqlite.h.in | 854–878 | v1: Close…DeviceCharacteristics; v2: Shm*; v3: Fetch/Unfetch | L |
| `SQLITE_FCNTL_*` | src/sqlite.h.in | doc 881; defines 1243–1286 | LOCKSTATE=1, PERSIST_WAL=10, PRAGMA=14, HAS_MOVED=20, SYNC=21, COMMIT_PHASETWO=22, WAL_BLOCK=24, JOURNAL_POINTER=28, PDB=30, DATA_VERSION=35, CKPT_DONE=37, RESERVE_BYTES=38, CKPT_START=39, BLOCK_ON_CONNECT=44, FILESTAT=45 | L |
| `struct sqlite3_vfs` | src/sqlite.h.in | doc 1343; 1513–1550 | VFS object (iVersion 3) | L |
| `SQLITE_SHM_NLOCK` | src/sqlite.h.in | 1611 | 8 | L |
| `sqlite3OsClose/Read/Write/Truncate/Sync/FileSize/Lock/Unlock/CheckReservedLock` | src/os.c | 82–87 / 88–91 / 92–95 / 96–98 / 99–102 / 103–106 / 107–111 / 112–115 / 116–119 | Thin wrappers over `pMethods` | L |
| `sqlite3OsFileControl` / `sqlite3OsFileControlHint` | src/os.c | 129–156 / 157–159 | Fault-injection aware fcntl | L |
| `sqlite3OsSectorSize` / `sqlite3OsDeviceCharacteristics` | src/os.c | 161–164 / 165–168 | | L |
| `sqlite3OsShmLock` / `sqlite3OsShmMap` / `sqlite3OsFetch` | src/os.c | 170–172 / 179–188 / 193–196 | | L |
| `sqlite3OsOpen` / `sqlite3OsDelete` / `sqlite3OsAccess` | src/os.c | 215–232 / 233–237 / 238–246 | `xOpen` masks flags; `xDelete(dirSync)` | L |
| `sqlite3OsOpenMalloc` | src/os.c | 308–332 | Allocate+open (used for super-journal) | L |
| `sqlite3_vfs_find` / `sqlite3_vfs_register` / `sqlite3_vfs_unregister` | src/os.c | 362–381 / 408–431 / 436–447 | VFS registry | L |
| `robust_open` | src/os_unix.c | 834–872 | `open()` retrying EINTR, refusing fds 0–2 | L |
| `robust_ftruncate` | src/os_unix.c | 999–1012 | | L |
| `seekAndRead` / `unixRead` | src/os_unix.c | 3463–3505 / 3512–3579 | `pread`; short read ⇒ zero-fill + `SQLITE_IOERR_SHORT_READ` | L, R |
| `seekAndWriteFd` / `seekAndWrite` / `unixWrite` | src/os_unix.c | 3588–3624 / 3634–3636 / 3643–3721 | `pwrite` | L, R |
| `full_fsync` | src/os_unix.c | 3778–3849 | `fsync`/`fdatasync`, or `F_FULLFSYNC` on macOS | L |
| `openDirectory` | src/os_unix.c | 3874–3894 | Opens the parent dir for dirsync (syscall slot `osOpenDirectory`, 518) | L |
| `unixSync` | src/os_unix.c | 3911–3956 | `full_fsync(h)`; then **one-time** directory fsync if `UNIXFILE_DIRSYNC` (3941–3953) | L, R |
| `unixTruncate` / `unixFileSize` / `fcntlSizeHint` | src/os_unix.c | 3961–4006 / 4011–4033 / 4049–4110 | `ftruncate`, `fstat`, chunk-size extension | L |
| `unixFileControl` | src/os_unix.c | 4141–4341 | Cases 4145–4281: atomic-write trio, LOCKSTATE, CHUNK_SIZE, SIZE_HINT, PERSIST_WAL, POWERSAFE_OVERWRITE, VFSNAME, TEMPFILENAME, HAS_MOVED, LOCK_TIMEOUT, BLOCK_ON_CONNECT, MMAP_SIZE, EXTERNAL_READER, FILESTAT | L |
| `setDeviceCharacteristics` / `unixSectorSize` / `unixDeviceCharacteristics` | src/os_unix.c | 4353–4375 / 4461–4465 / 4480–4484 | Non-QNX: sector 4096, `POWERSAFE_OVERWRITE` if `psow`, + `SUBPAGE_READ` | L, R (0x9000) |
| `unixMapfile` / `unixFetch` | src/os_unix.c | 5677–5699 / 5714–5738 | mmap I/O (`PRAGMA mmap_size`) | L |
| `fillInUnixFile` | src/os_unix.c | 6080–6257 | Sets up `unixFile`, `findInodeInfo`, method table | L |
| `unixTempFileDir` / `unixGetTempname` | src/os_unix.c | 6283–6303 / 6310–6342 | Temp dir search, `etilqs_` names | L |
| `findReusableFd` | src/os_unix.c | 6369–6414 | Reuse a parked fd for the same inode | L |
| `findCreateFileMode` | src/os_unix.c | 6458–6514 | -wal/-journal inherit db mode/owner | L |
| `unixOpen` | src/os_unix.c | 6538–6826 | `isNewJrnl` (6569–6572) ⇒ `UNIXFILE_DIRSYNC` (6777); `robust_open` (6681) | L, R |
| `unixDelete` | src/os_unix.c | 6833–6869 | `unlink`, then dir fsync if `dirSync` (6856) | L |
| `unixAccess` / `unixFullPathname` / `unixRandomness` / `unixSleep` | src/os_unix.c | 6881–6903 / 7008–7033 / 7100–7136 / 7147–7172 | | L |
| `sqlite3_os_init` | src/os_unix.c | 8448–8572 | Registers "unix", "unix-dotfile", "unix-excl", "unix-none", … | L |
| `pager_write_pagelist` | src/pager.c | 4475–4556 | Writes dirty pages to db file | L |
| `syncJournal` | src/pager.c | 4332–4441 | Journal header/record syncs (`DATAONLY` 4416–4417) | L |

### 7.2 Call paths
- **Open main db:** `openDatabase` → `sqlite3BtreeOpen` (`main.c:3612`) → `sqlite3PagerOpen` (`btree.c:2682`) → `sqlite3OsOpen` (`pager.c:4994`) → `unixOpen` → `robust_open` (`os_unix.c:6681`) → `fillInUnixFile` → `findInodeInfo`.
- **Open journal:** `pager_open_journal` (`pager.c:5877-5949`) → `sqlite3OsOpen` (flag MAIN_JOURNAL) → `unixOpen` sets DIRSYNC (6777). The first `xSync` on it fsyncs the directory (`unixSync` 3941). Seen as `fsync(<dir>)` after the first journal fsync (probe).
- **Write/commit (rollback):** `sqlite3PagerCommitPhaseOne` → `syncJournal` → `sqlite3OsSync(jfd)` (`pager.c:4404/4416`) → `pager_write_pagelist` → `sqlite3OsWrite` → `unixWrite` → `seekAndWriteFd` → `pwrite64`; then `sqlite3OsSync(fd)` (`pager.c:6453`); then `pager_end_transaction` → `sqlite3OsDelete(jrnl, extraSync)` (`pager.c:2120`) → `unixDelete` → `unlink` (+ dir fsync only for `synchronous=EXTRA`, confirmed by probe).
- **WAL open:** `sqlite3WalOpen` → `sqlite3OsOpen` (`wal.c:1726`, flag WAL); `-shm` opened lazily: `sqlite3OsShmMap` → `unixShmMap` → `unixOpenSharedMemory` (`os_unix.c:5122`) → `robust_open(zShm)` (5033).

### 7.3 Per-commit syscalls by journal mode (probe `fsync-per-commit.sh`, single-row INSERT, ext4, installed CLI)
| Mode | sync=NORMAL | sync=FULL | sync=EXTRA |
|---|---|---|---|
| DELETE | fsync jrnl, fsync dir, fsync db, unlink jrnl | + 2nd fsync jrnl (header) | FULL + fsync dir after unlink |
| TRUNCATE | as DELETE but ftruncate jrnl | + fsync jrnl after truncate | = FULL |
| PERSIST | as DELETE; header zeroed + fsync jrnl | same + header fsync | = FULL |
| WAL (steady) | none | fsync -wal | fsync -wal |
Every rollback-mode row shows a directory fsync after the first journal fsync. `isNewJrnl` is computed from the open-with-CREATE flag, not from whether the file already existed (`os_unix.c:6565-6572`), so each fresh `xOpen` of a journal arms `UNIXFILE_DIRSYNC` once. The probe runs one commit per process. Whether a long-lived TRUNCATE/PERSIST connection keeps the journal fd open across transactions (and so skips the dir fsync) is UNVERIFIED.

### 7.4 VFS shims shipped at this revision
| File | Lines | Bytes | What it does | How to use from an app | Verified |
|---|---|---|---|---|---|
| ext/misc/vfstrace.c | 1211 | 45640 | strace-like log of **every** VFS and io method, incl. fcntl names and shm | `vfstrace_register(name, base, xOut, arg, makeDflt)` (1138); compiled into the CLI (`src/shell.c.in:301`), so **`sqlite3 -vfstrace db` works with the installed CLI** | L, R |
| ext/misc/vfslog.c | 760 | 22849 | Logs VFS calls into a CSV-ish file | `sqlite3_register_vfslog(zArg)` (755); not loadable | L |
| ext/misc/vfsstat.c | 825 | 25024 | Counts I/O by file type; exposes `vfsstat` vtab | Loadable extension `sqlite3_vfsstat_init` (806) | L |
| ext/misc/tmstmpvfs.c | 1042 | 34432 | Shim that stamps timestamps into page reserve bytes | Loadable (`sqlite3_tmstmpvfs_init`, 1029) or `sqlite3_register_tmstmpvfs` (1006) | L |
| ext/misc/cksumvfs.c | 847 | 27736 | Per-page checksums in 8 reserve bytes | Loadable (`sqlite3_cksumvfs_init`, 832) or register (808) | L |
| ext/misc/appendvfs.c | 672 | 22694 | DB appended to another file (e.g. an executable) | Loadable (`sqlite3_appendvfs_init`, 649) | L |
| src/test_demovfs.c | 683 | 21408 | Minimal standalone unix VFS (no shm ⇒ no WAL) | `sqlite3_demovfs()` (620) returns a vfs to register; Tcl glue under `SQLITE_TEST` | L |
| src/test_vfs.c | 1697 | 50336 | Tcl-scriptable fault/shm VFS for the test suite | Tcl only (`Sqlitetestvfs_Init`, 1690) | L |
| src/test_journal.c | 869 | 27173 | Test VFS asserting journal-before-db write ordering | Test harness only; no public registration function found | L |
| src/test_multiplex.c | 1369 | 46436 | Splits a db into chunk files | `sqlite3_multiplex_initialize(base, makeDflt)` (1150) | L |
| src/test_osinst.c | 1221 | 35607 | Binary-log instrumentation VFS + `vfslog` vtab reader | `sqlite3_vfslog_new` (709) / `_finalize` (698) | L |
| src/test_devsym.c, src/test_syscall.c, src/test_quota.c, src/test_onefile.c | 525/761/1966/831 | 15390/21224/58072/24384 | Device simulation, syscall override, quota, single-file | Test/demo | L |
| ext/misc/memvfs.c | — | — | **Does not exist at this pin.** The in-memory VFS is built in: `src/memdb.c` ("memdb", `sqlite3MemdbInit` 923–936) | — | L |
| src/test_vfstrace.c | — | — | **Not at this path.** vfstrace lives in `ext/misc/vfstrace.c` (its own comment still says "test_vfstrace.c") | — | L |

**Best base for a course "logging VFS":** `ext/misc/vfstrace.c`. It is the only shim that wraps all v3 methods including Shm*/Fetch. It has a single registration function, needs no Tcl, and is already proven in the installed CLI. Write a Go lab around the `-vfstrace` output, or port its pattern. Pitfall to note in the lab: it prints `FULL` for `SQLITE_SYNC_NORMAL`, because it tests `flags & SQLITE_SYNC_FULL` (0x03), which is also nonzero for 0x02 (`ext/misc/vfstrace.c:453-454`). Use `vfsstat.c` as the second example (counters plus a vtab, loadable). `test_demovfs.c` is the best "write a VFS from scratch" reading.

### 7.5 Docs
https://sqlite.org/vfs.html, https://sqlite.org/c3ref/vfs.html, https://sqlite.org/c3ref/io_methods.html,
https://sqlite.org/c3ref/file.html, https://sqlite.org/c3ref/c_fcntl_begin_atomic_write.html,
https://sqlite.org/c3ref/c_iocap_atomic.html, https://sqlite.org/c3ref/c_open_autoproxy.html,
https://sqlite.org/c3ref/file_control.html, https://sqlite.org/c3ref/vfs_find.html,
https://sqlite.org/psow.html, https://sqlite.org/atomiccommit.html, https://sqlite.org/tempfiles.html,
https://sqlite.org/uri.html (`vfs=`, `psow=`), https://sqlite.org/loadext.html.

### 7.6 Reading path
1. `src/sqlite.h.in:584-878`: open flags, IOCAP, sqlite3_file, io_methods.
2. `src/sqlite.h.in:1343-1550`: sqlite3_vfs.
3. `src/os.c:82-246`: wrappers (shows what the core calls).
4. `src/os_unix.c:6538-6826` `unixOpen` + `834-872` `robust_open`.
5. `src/os_unix.c:3463-3721` read/write; `3778-3956` `full_fsync`/`unixSync`; `6833-6869` `unixDelete`.
6. `src/os_unix.c:4141-4484` file control + device characteristics.
7. `ext/misc/vfstrace.c` (whole) as the shim pattern.
8. `src/test_demovfs.c` as the minimal VFS.

### 7.7 Files to copy
| File | Lines | Bytes |
|---|---|---|
| src/os_unix.c (shared with ch6) | 8589 | 295468 |
| src/os.c | 447 | 14033 |
| src/os.h | 225 | 9393 |
| ext/misc/vfstrace.c | 1211 | 45640 |
| ext/misc/vfsstat.c | 825 | 25024 |
| src/test_demovfs.c | 683 | 21408 |
| (header excerpt) src/sqlite.h.in is 11369 lines / 554586 bytes. Cite lines 584–1611 instead of copying. | | |

### 7.8 Observation hooks
- `sqlite3 -vfstrace FILE` (installed CLI; sample: `xDeviceCharacteristics → 0x00009000` = PSOW|SUBPAGE_READ; `xRead(n=100,ofst=0)` header read at open).
- CLI `.vfsinfo`, `.vfslist`, `.vfsname`, `.filectrl` (`shell.c.in:3798`, `3968-3970`).
- strace `-y -e trace=openat,pread64,pwrite64,fsync,fdatasync,unlink,ftruncate,fcntl`.
- `PRAGMA mmap_size`, `PRAGMA temp_store`, `SQLITE_FCNTL_VFSNAME`, `SQLITE_FCNTL_POWERSAFE_OVERWRITE`, `SQLITE_FCNTL_CHUNK_SIZE`, `SQLITE_FCNTL_SIZE_HINT` (visible in vfstrace output).

### 7.9 Version notes
- 3.47.1/3.48.0 added `SQLITE_IOCAP_SUBPAGE_READ`, so unix reports 0x9000, not 0x1000.
- vfstrace now sits in `ext/misc/` and is built into the CLI (`-vfstrace`). Older write-ups say `src/test_vfstrace.c`. UNVERIFIED in which release it moved: the pinned clone is shallow, so `git log` cannot show it.
- 3.53.0 added the browser "opfs-wl" VFS (not relevant on Linux).

---

## 8. Connection and database lifecycle

### 8.1 Symbols

| Function / structure | File | Lines | Role | Verified |
|---|---|---|---|---|
| `setupLookaside` | src/main.c | 772–878 | Per-connection lookaside; default `1200,40` (`src/global.c:222`; "30 1200-byte + 93 128-byte slots", 214–216) | L |
| `sqlite3_db_release_memory` / `sqlite3_db_cacheflush` / `sqlite3_db_config` | src/main.c | 897–915 / 921–945 / 950–1038 | Shrink caches, flush dirty pages, config (NO_CKPT_ON_CLOSE at 992) | L |
| `disconnectAllVtab` / `connectionIsBusy` | src/main.c | 1221–1246 / 1252–1261 | Close prerequisites | L |
| `sqlite3Close` | src/main.c | 1266–1322 | `forceZombie`: v1 returns BUSY if statements are open; v2 marks a zombie | L |
| `sqlite3_close` / `sqlite3_close_v2` | src/main.c | 1363 / 1364 | One-liners → `sqlite3Close(db,0/1)` | L |
| `sqlite3_txn_state` | src/main.c | 1328–1352 | NONE/READ/WRITE per schema | L |
| `sqlite3LeaveMutexAndCloseZombie` | src/main.c | 1375–1486 | Real teardown: `sqlite3BtreeClose` per db (1407), schema free, lookaside free | L |
| `sqlite3RollbackAll` | src/main.c | 1495–1538 | | L |
| `sqlite3_limit` | src/main.c | 3030–3077 | Includes `SQLITE_LIMIT_ATTACHED` (=7, `sqlite.h.in:4416`) | L |
| `sqlite3ParseUri` | src/main.c | 3105–3336 | URI → flags/VFS (`vfs=`, `mode=`, `cache=`, `immutable`, `nolock`) | L |
| `openDatabase` | src/main.c | 3360–3716 | ParseUri (3596) → BtreeOpen (3612) → SchemaGet (3622/3627) → `SQLITE_STATE_OPEN` (3637) → builtin funcs (3647) → auto-ext (3660) → lookaside (3687) → autocheckpoint (3690) | L |
| `sqlite3_open` / `sqlite3_open_v2` | src/main.c | 3722–3728 / 3729–3736 | → `openDatabase` (3726/3735) | L |
| `sqlite3_file_control` | src/main.c | 4193–4241 | | L |
| `sqlite3_db_filename` / `sqlite3_db_readonly` | src/main.c | 5027–5037 / 5043–5053 | | L |
| `sqlite3_db_status64` / `sqlite3_db_status` | src/status.c | 203–421 / 426–446 | Per-connection counters (`SQLITE_DBSTATUS_*` 0..13, `sqlite.h.in:9197-9210`) | L |
| `sqlite3BtreeOpen` | src/btree.c | 2532–2825 | Opens pager (2682), reads **only the 100-byte header** (`sqlite3PagerReadFileheader`, 2686), installs busy handler (2693); shared-cache lookup | L, R (vfstrace `xRead n=100`) |
| `sqlite3BtreeClose` | src/btree.c | 2921–2978 | → `sqlite3PagerClose` (2960) | L |
| `sqlite3BtreeSetCacheSize` | src/btree.c | 2987–2994 | | L |
| `lockBtree` | src/btree.c | 3282–3459 | First real read of page 1; WAL detection (`sqlite3PagerOpenWal` 3343) | L |
| `sqlite3BtreeGetMeta` | src/btree.c | 10442–10466 | Schema cookie etc. | L |
| `sqlite3BtreeSchema` | src/btree.c | 11386–11396 | Schema object hung off BtShared | L |
| `sqlite3PagerOpen` / `sqlite3PagerClose` | src/pager.c | 4781–5129 / 4222–4279 | Opens db fd (4994); close → `sqlite3WalClose` (4240) | L |
| `sqlite3PagerShrink` / `sqlite3PagerFlush` | src/pager.c | 3603–3605 / 4732–4747 | Back `db_release_memory` / `db_cacheflush` | L |
| `sqlite3InitCallback` | src/prepare.c | 96–189 | Parses each `sqlite_schema` row | L |
| `sqlite3InitOne` | src/prepare.c | 199–427 | Reads meta (288) and sets `schema_cookie` (293); runs `SELECT * FROM sqlite_schema` | L |
| `sqlite3Init` | src/prepare.c | 438–464 | main (449) then attached (456) | L |
| `sqlite3ReadSchema` | src/prepare.c | 470–484 | **Lazy load trigger**: called by the parser on first name lookup (e.g. `build.c:420`) | L |
| `schemaIsValid` | src/prepare.c | 492–533 | After a prepare error: compare cookie (521–523) | L |
| `sqlite3SchemaToIndex` | src/prepare.c | 542–567 | Schema* → db index | L |
| `sqlite3Prepare` / `sqlite3LockAndPrepare` / `sqlite3Reprepare` | src/prepare.c | 682–835 / 836–875 / 886–914 | Prepare; re-prepare on SQLITE_SCHEMA | L |
| `sqlite3_prepare_v2` / `_v3` | src/prepare.c | 937–954 / 955–976 | | L |
| `sqlite3SchemaClear` / `sqlite3SchemaGet` | src/callback.c | 495–524 / 530–547 | | L |
| `sqlite3ResetOneSchema` / `sqlite3ResetAllSchemasOfConnection` | src/build.c | 626–644 / 650–669 | | L |
| `OP_Transaction` cookie check | src/vdbe.c | 4102–4201 (cookie mismatch 4186–4192) | Mismatch ⇒ reset schema, `SQLITE_SCHEMA` | L |
| `sqlite3_step` retry | src/vdbeapi.c | 919–966 (`sqlite3Reprepare` 933; `SQLITE_MAX_SCHEMA_RETRY`=50, `vdbeInt.h:26`) | Transparent reprepare for v2/v3 statements | L |
| `sqlite3_finalize` | src/vdbeapi.c | 105–124 | Can finish a zombie close (121) | L |
| `attachFunc` | src/attach.c | 74–283 | Limit check (146), `sqlite3BtreeOpen` (195), safety_level (224) | L |
| `detachFunc` / `codeAttach` / `sqlite3Detach` / `sqlite3Attach` | src/attach.c | 293–349 / 355–422 / 429–442 / 449–462 | | L |
| `struct UnixUnusedFd`, `findReusableFd`, `setPendingFd`, `closePendingFds` | src/os_unix.c | 246–250, 6369–6414, 2101–2109, 1471–1482 | fds parked on close if the inode has locks; reused by a later `unixOpen` (6631) | L |
| `unixTempFileDir` / `unixGetTempname` | src/os_unix.c | 6283–6303 / 6310–6342 | Temp files (opened and then unlinked) | L |
| `sqlite3_release_memory` | src/malloc.c | 23–33 | Global | L |

### 8.2 Call paths
- **Open (no I/O beyond the header):** `sqlite3_open_v2` → `openDatabase` (`main.c:3735`) → `sqlite3ParseUri` (3596) → `sqlite3BtreeOpen` (3612) → `sqlite3PagerOpen` (`btree.c:2682`) → `sqlite3OsOpen` (`pager.c:4994`) → `unixOpen`; then `sqlite3PagerReadFileheader` (`btree.c:2686`) reads 100 bytes. **No schema read and no lock at open.** Probe: vfstrace shows `xOpen`, `xRead(n=100,ofst=0)`, then nothing until the first statement.
- **First statement (lazy schema):** `sqlite3_prepare_v2` → `sqlite3LockAndPrepare` → `sqlite3Prepare` → parser → `sqlite3ReadSchema` (`prepare.c:475`) → `sqlite3Init` → `sqlite3InitOne` (449) → `OP_Transaction`-like read of meta (288) → `sqlite3InitCallback` per schema row.
- **Schema change by another connection:** `OP_Transaction` reads the cookie via `sqlite3BtreeBeginTrans(&iMeta)` (`vdbe.c:4128`) → mismatch ⇒ `sqlite3ResetOneSchema` + `SQLITE_SCHEMA` (4186–4192) → `sqlite3_step` → `sqlite3Reprepare` (`vdbeapi.c:933`), up to 50 tries.
- **Close:** `sqlite3_close_v2` → `sqlite3Close(db,1)` (`main.c:1364`) → zombie if statements remain → `sqlite3LeaveMutexAndCloseZombie` (1320, or later from `sqlite3_finalize` `vdbeapi.c:121`) → `sqlite3BtreeClose` (1407) → `sqlite3PagerClose` (`btree.c:2960`) → `sqlite3WalClose` (`pager.c:4240`; checkpoint + unlink in WAL mode, §5).
- **ATTACH:** `ATTACH` → `codeAttach` → `attachFunc` → `nDb >= LIMIT_ATTACHED+2` check (`attach.c:146`) → `sqlite3BtreeOpen` (195). Schema is again lazy: loaded by `sqlite3Init`'s loop (`prepare.c:456`).

### 8.3 File descriptors per connection (probe `fds-per-connection.sh`)
| Mode | Idle after a read | Inside write txn | After commit |
|---|---|---|---|
| DELETE | 1 (db) | 2 (db, -journal) | 1 |
| WAL | 3 (db, -wal, -shm) | 3 | 3 |
Notes from the source: temp files and statement journals (`SQLITE_OPEN_SUBJOURNAL`/`TEMP_*`) add transient fds. Several connections to the same file in one process share one `unixInodeInfo`, and fds closed while that inode holds locks are parked in `pUnused` (`setPendingFd`). This "fd leak until the last lock is released" is worth one paragraph in the lab on bounded fd budgets.

### 8.4 Docs
https://sqlite.org/c3ref/open.html, https://sqlite.org/c3ref/close.html, https://sqlite.org/c3ref/prepare.html,
https://sqlite.org/c3ref/txn_state.html, https://sqlite.org/lang_attach.html, https://sqlite.org/limits.html,
https://sqlite.org/c3ref/c_limit_attached.html, https://sqlite.org/c3ref/db_status.html,
https://sqlite.org/c3ref/c_dbstatus_options.html, https://sqlite.org/malloc.html (lookaside),
https://sqlite.org/c3ref/db_release_memory.html, https://sqlite.org/c3ref/db_cacheflush.html,
https://sqlite.org/tempfiles.html, https://sqlite.org/uri.html, https://sqlite.org/rescode.html#schema.

### 8.5 Reading path
1. `src/main.c:3360-3736`: `openDatabase` top to bottom.
2. `src/btree.c:2532-2825` `sqlite3BtreeOpen` (stop after 2693).
3. `src/prepare.c:438-533`: `sqlite3Init`, `sqlite3ReadSchema`, `schemaIsValid`.
4. `src/prepare.c:199-427` `sqlite3InitOne`.
5. `src/vdbe.c:4067-4201` `OP_Transaction` (cookie check) + `src/vdbeapi.c:919-966` retry.
6. `src/attach.c:74-349` attach/detach.
7. `src/main.c:1266-1486` close and zombie.
8. `src/main.c:772-945` lookaside, release_memory, cacheflush; `src/status.c:203-421`.
9. `src/os_unix.c:246-250`, `2101-2109`, `6369-6414`: fd parking/reuse.

### 8.6 Files to copy
| File | Lines | Bytes |
|---|---|---|
| src/main.c | 5242 | 171374 |
| src/attach.c | 631 | 19217 |
| src/prepare.c | 1093 | 37771 |
| src/status.c | 446 | 14468 |

### 8.7 Observation hooks
- `sqlite3_db_status(SQLITE_DBSTATUS_CACHE_USED|SCHEMA_USED|STMT_USED|LOOKASIDE_*|CACHE_HIT/MISS/WRITE/SPILL)`; `sqlite3_status64(SQLITE_STATUS_MEMORY_USED)`.
- `/proc/<pid>/fd` count (probe), `ls -l` for -wal/-shm/-journal, `lsof`.
- `PRAGMA database_list` (`pragma.c:1436`), `PRAGMA schema_version`, `PRAGMA cache_size`, `PRAGMA shrink_memory` (2439).
- `sqlite_stmt` vtab (`ENABLE_STMTVTAB` is in the installed lib): open statements and their memory.
- vfstrace shows the lazy schema load (the first SHARED lock and the page-1 read happen at the first statement, not at open).

### 8.8 Version notes
- 3.49.0 added `SQLITE_DBCONFIG_ENABLE_ATTACH_CREATE` / `ENABLE_ATTACH_WRITE`, which gate ATTACH creating or writing files. Relevant to the many-files capstone.
- 3.53.0 added `SQLITE_PREPARE_FROM_DDL` (vtab use). 3.49.1 hardened `SQLITE_DBCONFIG_LOOKASIDE`.

---

## 9. Backup, crash recovery, integrity

### 9.1 Symbols

| Function / structure | File | Lines | Role | Verified |
|---|---|---|---|---|
| `struct sqlite3_backup` | src/backup.c | 21–42 | `iNext`, `bDestLocked`, `isAttached`, `pNext` (list on source pager) | L |
| `findBtree` / `setDestPgsz` / `checkReadTransaction` | src/backup.c | 83–107 / 113–115 / 123–129 | Init helpers (dest must not be in a read txn) | L |
| `sqlite3_backup_init` | src/backup.c | 139–219 | `iNext=1` (197) | L |
| `isFatalError` | src/backup.c | 226–228 | BUSY/LOCKED are retryable | L |
| `backupOnePage` | src/backup.c | 235–288 | Copy one source page to N dest pages (handles page-size mismatch) | L |
| `backupTruncateFile` / `attachBackupObject` | src/backup.c | 298–305 / 311–318 | Register on source pager (`sqlite3PagerBackupPtr`, 314) | L |
| `sqlite3_backup_step` | src/backup.c | 323–591 | Source read txn (362), dest write txn `BeginTrans(pDest,2)` (390), copy, commit dest | L |
| `sqlite3_backup_finish` / `_remaining` / `_pagecount` | src/backup.c | 596–646 / 652–660 / 666–674 | | L |
| `backupUpdate` / `sqlite3BackupUpdate` | src/backup.c | 688–712 / 713–715 | **Same-process writes through the source pager**: re-copy pages already copied (`< iNext`) | L |
| `sqlite3BackupRestart` | src/backup.c | 728–734 | **Writes by another connection/process**: `iNext=1`, restart | L |
| `sqlite3BtreeCopyFile` | src/backup.c | 745–793 | Whole-file copy used by VACUUM | L |
| `sqlite3Vacuum` / `sqlite3RunVacuum` | src/vacuum.c | 105–138 / 143–427 | ATTACH temp/INTO target (226), copy via SQL, `sqlite3BtreeCopyFile` (379) unless INTO; schema cookie +1 (359) | L |
| `struct MemStore` / `struct MemFile` | src/memdb.c | 70–82 / 85–89 | memdb VFS state | L |
| `memdbLock` / `memdbOpen` | src/memdb.c | 368–421 / 542–619 | | L |
| `sqlite3_serialize` / `sqlite3_deserialize` | src/memdb.c | 750–837 / 841–910 | Copy-out (any db, via page reads) / install a buffer as a memdb | L |
| `sqlite3MemdbInit` | src/memdb.c | 923–936 | Registers "memdb" VFS | L |
| `writeJournalHdr` / `readJournalHdr` / `writeSuperJournal` | src/pager.c | 1449–1571 / 1590–1693 / 1715–1778 | Journal format and super-journal name record | L |
| `pager_end_transaction` | src/pager.c | 2052–2180 | Finalize journal (delete 2120 / truncate / zero header) | L |
| `pager_playback_one_page` | src/pager.c | 2298–2500 | Restore one page; `sqlite3BackupUpdate` (2447) | L |
| `pager_delsuper` | src/pager.c | 2579–2695 | Delete super-journal when no child journal still points at it (2684) | L |
| `pager_playback` | src/pager.c | 2864–3052 | Hot-journal rollback; → `pager_delsuper` (3037) | L |
| `pager_write_pagelist` | src/pager.c | 4475–4556 | Feeds backup (4542) | L |
| `hasHotJournal` | src/pager.c | 5180–5271 | Journal exists + non-empty + no RESERVED holder + db non-empty | L |
| `sqlite3PagerSharedLock` | src/pager.c | 5300–5507 | Hot journal ⇒ EXCLUSIVE ⇒ open jrnl (5381) ⇒ `pager_playback` (5402) | L |
| `sqlite3PagerCommitPhaseOne` / `PhaseTwo` / `sqlite3PagerRollback` | src/pager.c | 6511–6730 / 6748–6786 / 6814–6858 | Commit sync sequence; rollback playback (6845) | L |
| `walIndexRecover` | src/wal.c | 1390–1614 | WAL crash recovery: scan frames, stop at the first bad checksum/salt; the last commit frame becomes `mxFrame` | L |
| `checkPtrmap` / `checkList` / `checkTreePage` | src/btree.c | 10691–10713 / 10720–10784 / 10855–11118 | Integrity-check workers | L |
| `sqlite3BtreeIntegrityCheck` | src/btree.c | 11144–11295 | Page-level check | L |
| `OP_IntegrityCk` | src/vdbe.c | doc 7244; case 7263–7299 | → `sqlite3BtreeIntegrityCheck` (7284) | L |
| `PragTyp_INTEGRITY_CHECK` | src/pragma.c | 1695 (emits `OP_IntegrityCk` 1782) | integrity_check / quick_check codegen (index/row checks are SQL-level) | L |
| `fullfsync` / `checkpoint_fullfsync` | tool/mkpragmatab.tcl | 52 / 57 | PragTyp_FLAG ⇒ `PAGER_FULLFSYNC` / `PAGER_CKPT_FULLFSYNC` (`pager.h:121-122`) | L |

### 9.2 Call paths
- **Backup, source written by the same process/pager:** writer commit → `pager_write_pagelist` → `sqlite3BackupUpdate` (`pager.c:4542`) or WAL `pagerWalFrames` → `sqlite3BackupUpdate` (3270) → `backupUpdate` re-copies pages `< iNext` (`backup.c:688-712`). The backup continues without a restart.
- **Backup, source written by a different connection/process:** the next source read txn detects the change → `pager_reset` (`pager.c:1783-1787`) → `sqlite3BackupRestart` (1785) → `iNext=1`, so the backup restarts from page 1. Also on WAL rollback (`pagerUndoCallback`, 3185) and on a commit that writes no pages (6543). A backup of a busy multi-writer db may therefore never finish when it runs in small steps. Lab hook: `sqlite3_backup_step(-1)` copies in one read txn.
- **VACUUM INTO:** `sqlite3Vacuum` → `OP_Vacuum` (`vdbe.c:8158`) → `sqlite3RunVacuum` → `ATTACH %Q` (`vacuum.c:226`) → source read txn (270) → `INSERT INTO vacuum_db.x SELECT * FROM main.x` → **no** `sqlite3BtreeCopyFile` back (379 is skipped for INTO). The result is a consistent, compacted snapshot in a single read txn.
- **Rollback-journal recovery:** first reader: `sqlite3PagerSharedLock` → `hasHotJournal` (`pager.c:5328`) → EXCLUSIVE lock → `sqlite3OsOpen(journal)` (5381) → `pager_playback(pPager,1)` (5402) → `pager_playback_one_page` per record → `pager_delsuper` (3037) when a super-journal is named.
- **WAL recovery:** first reader after a crash: `walIndexReadHdr` → bad/zero -shm header → `walIndexRecover` (`wal.c:2730`). The -shm is also reset by the DMS logic in `unixLockSharedMemory` (`os_unix.c:4893`) when no other process has it open. A torn tail is ignored, because frames after the last valid commit frame are not part of `mxFrame`.
- **Integrity:** `PRAGMA integrity_check` → `sqlite3Pragma` case 1695 → `OP_IntegrityCk` (1782) → `sqlite3BtreeIntegrityCheck` (`vdbe.c:7284`) → `checkTreePage`/`checkList`/`checkPtrmap`.

### 9.3 fsyncs at commit (probe `fsync-per-commit.sh`; Linux, fullfsync has no effect except on macOS `F_FULLFSYNC` in `full_fsync` 3778–3849)
- DELETE, NORMAL: journal, dir, db (3). FULL adds a journal-header fsync (4). EXTRA adds a dir fsync after the unlink (5).
- WAL: NORMAL ⇒ 0 per commit (durability is lost on power failure, not on process crash). FULL ⇒ 1 (`-wal`). Checkpoint ⇒ `-wal` then db.
- Outline must state: a process crash loses nothing that was committed, at any `synchronous` level (data is in the OS page cache). Only power loss or a kernel crash exposes the `synchronous`/fsync choices. This matches the spec's "do not equate process crashes with power-loss durability".

### 9.4 Tools in the tree (built from a copy with `agent_work/probes/source-inventory-b/build-tools.sh`; default `./configure`, gcc 13.3)
| Tool | Source (lines / bytes) | Purpose | `make` target | Built? |
|---|---|---|---|---|
| sqlite3 CLI (`.backup`, `.recover`, `.dump`, `.dbinfo`, `.dbtotxt`, `.clone`) | src/shell.c.in | CLI; `.recover` uses ext/recover | `sqlite3` | OK (7.2 MB) |
| sqldiff | tool/sqldiff.c (2060 / 66853) | SQL diff of two dbs | `sqldiff` | OK |
| dbhash | tool/dbhash.c (491 / 15873) | SHA1 of logical content (layout-independent) | `dbhash` | OK; R: same hash before/after VACUUM INTO |
| sqlite3_rsync | tool/sqlite3_rsync.c (2430 / 76558) | Replicate a live db (local or via ssh) consistently | `sqlite3_rsync` | OK; R: local copy, hash equal |
| showdb | tool/showdb.c (1420 / 39605) | Page/btree dump of a db file | `showdb` | OK |
| showwal | tool/showwal.c (639 / 16857) | WAL header/frame decoder | `showwal` | OK; R: decoded header |
| showjournal | tool/showjournal.c (138 / 3648) | Rollback journal decoder | `showjournal` | OK |
| showshm | tool/showshm.c | -shm decoder | `showshm` | OK |
| dbtotxt | tool/dbtotxt.c (194 / 6077) | Hex text dump (pairs with CLI `.open --hexdb`) | `dbtotxt` | OK |
| dbdump | ext/misc/dbdump.c (724 / 21805) | Library form of `.dump` | `dbdump` | OK |
| sqlite3_expert | ext/expert | Index advisor (not ch9) | `sqlite3_expert` | OK |
| showstat4 | tool/showstat4.c | stat4 viewer (not ch9) | `showstat4` | OK |
| sqlite3_analyzer | tool/spaceanal.tcl (917 / 31021) | Space usage report | `sqlite3_analyzer` | **FAIL**: needs Tcl (`TCL_CONFIG_SH must be set`) |
| changeset | ext/session/changeset.c; lib ext/session/sqlite3session.c (7554 / 240921) | Changeset inspect/apply | `changeset` | **FAIL** with default configure (session types undeclared); needs `./configure --session` (or `--all`). UNVERIFIED with the flag |
| recover lib | ext/recover/sqlite3recover.c (2925 / 90436), dbdata.c (1023 / 29595) | Salvage from corrupt dbs | in CLI (`.recover`) | OK; R: `.recover` emits INSERTs |
| sqlar | ext/misc/sqlar.c (126 / 3898) | SQL Archive compress functions (CLI `-A`) | built into CLI | L only (not a ch9 tool; low value) |

### 9.5 Docs
https://sqlite.org/backup.html, https://sqlite.org/c3ref/backup_finish.html, https://sqlite.org/lang_vacuum.html
(#vacuuminto), https://sqlite.org/c3ref/serialize.html, https://sqlite.org/c3ref/deserialize.html,
https://sqlite.org/atomiccommit.html, https://sqlite.org/howtocorrupt.html, https://sqlite.org/psow.html,
https://sqlite.org/walformat.html, https://sqlite.org/wal.html, https://sqlite.org/pragma.html
(integrity_check, quick_check, synchronous, fullfsync, checkpoint_fullfsync),
https://sqlite.org/recovery.html, https://sqlite.org/rsync.html, https://sqlite.org/sqldiff.html,
https://sqlite.org/dbhash.html, https://sqlite.org/sessionintro.html, https://sqlite.org/inmemorydb.html.

### 9.6 Reading path
1. https://sqlite.org/atomiccommit.html (the model), then `src/pager.c:5180-5271` `hasHotJournal`.
2. `src/pager.c:5300-5507` `sqlite3PagerSharedLock` (hot-journal branch) → `2864-3052` `pager_playback`.
3. `src/pager.c:2579-2695` `pager_delsuper` (multi-db commits).
4. `src/wal.c:1390-1614` `walIndexRecover` (WAL counterpart).
5. `src/backup.c:323-591` `sqlite3_backup_step`, then `688-734` update/restart.
6. `src/vacuum.c:143-427` `sqlite3RunVacuum` (INTO branch).
7. `src/memdb.c:750-910` serialize/deserialize.
8. `src/btree.c:11144-11295` + `src/vdbe.c:7263-7299` integrity check.
9. https://sqlite.org/howtocorrupt.html as the closing threat model.

### 9.7 Files to copy
| File | Lines | Bytes |
|---|---|---|
| src/backup.c | 794 | 28036 |
| src/vacuum.c | 429 | 16554 |
| src/memdb.c | 937 | 27270 |
| tool/dbhash.c | 491 | 15873 |
| tool/showwal.c | 639 | 16857 |
| tool/showjournal.c | 138 | 3648 |
| (pager.c / wal.c are already copied for ch5) | | |

### 9.8 Observation hooks
- `sqlite3_backup_remaining` / `_pagecount`; restart is visible as `remaining` jumping back up.
- `PRAGMA integrity_check(N)`, `PRAGMA quick_check`, `dbhash`, `sqldiff`.
- Hot journal on disk: `*-journal` with non-zero header after `kill -9`; `showjournal`. WAL after `kill -9`: `-wal` + stale `-shm`; `showwal` shows the frames beyond the last commit that recovery will ignore.
- strace for fsync order (probes); `sqlite3 -vfstrace` for `xSync` order (mind the NORMAL→"FULL" label bug, §7.4).
- `sqlite3_serialize` size = page_count·page_size; `sqlite_dbpage` vtab for raw page compares.

### 9.9 Version notes
- 3.47.0 added `sqlite3_rsync`; 3.50.0 dropped its WAL-only requirement; 3.50.1 fixed a last-page off-by-one; 3.53.0 added `-p/--port`.
- 3.53.0: `VACUUM INTO 'file:x?reserve=N'` sets reserve bytes. 3.40.0: VACUUM INTO honors `PRAGMA synchronous`.
- 3.44.0+: integrity_check also runs vtab `xIntegrity` (FTS/RTREE). 3.41.0: deserialized dbs report filename "".

---

## Cross-chapter notes for the outline
1. Snapshot, session and blocking-lock labs need a custom build. The installed lib lacks `ENABLE_SNAPSHOT`, `ENABLE_SESSION` and `ENABLE_SETLK_TIMEOUT`. `build-tools.sh` shows a scratch-copy configure/make works in under a few minutes on this VM. `--session`/`--all` are the configure switches (`./configure --help`).
2. The code is the authority for the sync-bit layout. The `pager.c:607-617` comment is inverted relative to `wal.h:25-26`.
3. `sqlite3 -vfstrace` already exists in the installed CLI, so the ch7 lab can start from observation before any C code.

## INDEX rows
| path | purpose | chapter | made by | what it proved |
| --- | --- | --- | --- | --- |
| `probes/source-inventory-b/locate.py` | Print start–end lines of C functions/structs/#defines in the pinned tree | 5–9 | Opus research agent (source-inventory-b), 2026-11-20 | All line ranges in `docs/knowledge/source-inventory-ch05-09.md` were produced or cross-checked with it |
| `probes/source-inventory-b/build-tools.sh` | Copy the pin to /tmp, `./configure`, `make` each ch9 tool, report OK/FAIL | 9 | Opus research agent (source-inventory-b), 2026-11-20 | sqlite3, sqldiff, dbhash, sqlite3_rsync, showdb, showwal, showjournal, showshm, showstat4, dbtotxt, dbdump, sqlite3_expert build with default configure; sqlite3_analyzer needs Tcl; changeset needs `--session` |
| `probes/source-inventory-b/tools-smoke.sh` | Run showwal, VACUUM INTO + dbhash, sqldiff, sqlite3_rsync, .recover, integrity_check on a tiny db | 9 | Opus research agent (source-inventory-b), 2026-11-20 | Tools work at the pin; dbhash equal across VACUUM INTO and rsync copy; showwal decodes magic/salts/checksums |
| `probes/source-inventory-b/fsync-per-commit.sh` | strace -y fsync/unlink/ftruncate for one commit per journal_mode × synchronous | 5, 7, 9 | Opus research agent (source-inventory-b), 2026-11-20 | Per-mode sync sequence in §7.3/§9.3; dir fsync on new journal; close-time WAL checkpoint + unlink |
| `probes/source-inventory-b/wal-steady-sync.sh` | strace fsync + WAL pwrite offsets for 2 commits, synchronous NORMAL vs FULL | 5 | Opus research agent (source-inventory-b), 2026-11-20 | WAL+NORMAL: 0 fsync per steady-state commit; FULL: 1; fresh WAL: header fsync + dir fsync; frame offsets 32/56/4152/4176 |
| `probes/source-inventory-b/fds-per-connection.sh` | List /proc fds held by one CLI connection in DELETE vs WAL mode | 8 | Opus research agent (source-inventory-b), 2026-11-20 | DELETE: 1 fd idle / 2 in write txn; WAL: 3 fds (db, -wal, -shm) persistently |
