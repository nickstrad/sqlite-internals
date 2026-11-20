# Determinism, state capture and rebuild verification

Research for the course outline (topic `determinism-rebuild`), 2026-11-20. Every result below was
observed on the pin: SQLite 3.53.4 (git `b09c88c1…`, Fossil `bf7c7f30…`), the installed
`/usr/local/bin/sqlite3` CLI and `/usr/local/lib/libsqlite3`. Source references are
`file:line` in `/root/Software/sqlite-src/3.53.4`. Anything not verified is marked **UNVERIFIED**.

Probes: `agent_work/probes/determinism-rebuild/` (README there has the exact commands;
`GOPROXY=off ./run_all.sh` reruns everything). Hashes shown are the first 16 hex digits of sha256.

## Headline findings

1. **The same SQL script gives byte-identical files on every run.** This held for all 7
   configurations tested: rollback journal, WAL with checkpoint, both auto_vacuum modes, cache
   spill, and two page sizes. It also held across two different builds of 3.53.4 (the C CLI and
   the transpiled modernc Go driver). Nothing in the file depends on time, pid or randomness
   unless the SQL puts it there.
2. **The same logical rows written in different transaction shapes give different bytes.**
   The difference has three sources. Header counters (@24, @92) count commits. Stale
   **freelist-leaf** content depends on whether a page reached disk before it was freed.
   Insert order shapes the b-tree. `PRAGMA secure_delete=ON` removes the freelist source,
   leaving only header counters.
3. **`VACUUM INTO` is a canonicalizer.** Three physically different builds of the same content
   came out byte-identical after `VACUUM INTO`. So did a `.dump` replay, and a changeset-applied
   copy against its SQL-built twin.
4. **WAL bytes differ between runs only in salts and checksums.** The sequence of
   `(pgno, sha256(page), commit-size)` frame tuples is identical across runs and across
   `synchronous` settings. `.testctrl prng_seed N` (`SQLITE_TESTCTRL_PRNG_SEED`) makes the
   **whole WAL byte-identical**.
5. **Replaying committed WAL frames ourselves (Go, no SQLite) matches SQLite's own
   `wal_checkpoint(TRUNCATE)` byte for byte.** This held in every case tested, including
   uncommitted spilled frames at the tail and a corrupted last frame. In the corrupted-frame
   case, our result matched SQLite's own recovery of the same damaged WAL.

---

## Q1. Build determinism

**Probe:** `q1_build_determinism.sh` (plus `q1b_freelist_secure_delete.sh`,
`q1c_sql_nondeterminism.sh`, `go/modernc_build`). The workload is `seed.sql`: 2 tables and an
index, 2000 + 6000 rows from recursive CTEs, then updates and deletes. No nondeterministic
functions are used. Each variant is built twice into fresh files:
`{prelude; BEGIN; seed; COMMIT; postlude} | sqlite3 X.db`.

Observed:

```
delete        run1=737a77933eef826f run2=737a77933eef826f IDENTICAL size=1441792
wal           run1=949acda3348371ad run2=949acda3348371ad IDENTICAL   (+ wal_checkpoint(TRUNCATE), close)
wal_nockpt    run1=949acda3348371ad run2=949acda3348371ad IDENTICAL   (autocheckpoint=0; close checkpoints anyway)
autovac_full  run1=00a53312ca880296 run2=00a53312ca880296 IDENTICAL
autovac_incr  run1=bd185ef05e6ecff8 run2=bd185ef05e6ecff8 IDENTICAL   (+ PRAGMA incremental_vacuum)
spill_cache5  run1=29fae44e9bd31ffe run2=29fae44e9bd31ffe IDENTICAL   (cache_size=5: spills mid-txn)
page1024      run1=a79b9cdf67f35f2b run2=a79b9cdf67f35f2b IDENTICAL
modernc.org/sqlite v1.58.0 (bundles 3.53.4, same source id) vs CLI delete build: BYTE-IDENTICAL
```

**Different batching of the same logical rows:**

```
batch_onetxn      sha=737a77933eef826f  .sha3sum=72b4aa92d244b635
batch_autocommit  sha=be85fd6e0dea0929  .sha3sum=72b4aa92d244b635   (each seed statement its own txn)
batch_rowwise     sha=fe3243a7eccdabab  .sha3sum=72b4aa92d244b635   (single-row INSERTs, final values, customers in descending id)
onetxn vs autocommit:  page 1 bytes 27,95 (counters) + page 43 (142 ranges); ignoring header: page 43 only
onetxn vs rowwise:     352 vs 291 pages, every page differs (different tree shape, no freelist)
delete vs spill_cache5 (ignoring header): page 43 only
delete vs wal:         page 1 bytes 18-19 (+27,95) and page 43
```

Page 43 is the db's only **freelist leaf** (trunk 307 lists `[43]`). Freelist leaf content is
never rewritten, so it holds whatever the page last held *on disk*. Committing at a different
point, spilling, or going through the WAL changes that. With `PRAGMA secure_delete=ON` the
onetxn and autocommit files differ only in header bytes 24-27 and 92-95
(`q1b`: `differing pages: 0` with `--ignore-header`).

**Header fields that differ between runs, builds or versions** (from `hdr.py` plus source):

| Offset | Field | Behaviour observed / source |
| --- | --- | --- |
| 18, 19 | write/read version | 1 = rollback, 2 = WAL. Persistent; set by `journal_mode=WAL`. `backup.c:455` sets it on a WAL destination. |
| 24 | file change counter | Written as `dbFileVers+1` by `pager_write_changecounter` (`pager.c:3130-3142`). Counts commits in rollback mode: onetxn=1, autocommit=9, rowwise=6703. In WAL mode it stayed at 2 across 5 commits on one connection (q3c). The mechanism is **PLAUSIBLE, not verified**: `dbFileVers` is refreshed only when page 1 is read from storage. |
| 28 | in-header db size | Follows content: 352 vs 364 pages. |
| 32, 36 | freelist trunk/count | Follows history: 2 free pages after seed, 167 after phase2. |
| 40 | schema cookie | +1 per schema change (`build.c:2047` `sqlite3ChangeCookie`). `.backup` sets the destination's cookie to its old cookie + 1 (`backup.c:448`), so a backup into a new file has cookie 1. |
| 52, 64 | autovacuum root / incremental | Persistent (auto_vacuum). |
| 92 | version-valid-for | Same value as @24 (`pager.c:3141`). |
| 96 | SQLITE_VERSION_NUMBER | Last writer's library version (`pager.c:3142`): 3053004. A different SQLite version writing the same content changes @96 at least. Whether its b-tree bytes would also differ is **UNVERIFIED**; no second version was available offline. |

**SQL-level nondeterminism** (`q1c`, each case run twice into fresh dbs):

| Source | Result | Note |
| --- | --- | --- |
| `random()`, `randomblob()` | DIFFER | Fixable with `.testctrl prng_seed N` (see Q2). |
| `current_timestamp`, `date('now')` | DIFFER | Wall clock; pass times in as parameters instead. |
| rowid after max rowid (9223372036854775807) | DIFFER | Picks a random unused rowid. Observed `3989934225369470477` and `2476293308791150527` for the same row on two runs. |
| AUTOINCREMENT | IDENTICAL | Deterministic, but `sqlite_sequence` holds a high-water mark (3) that survives deletes. Content hashes ignore it (Q4). |
| unordered `SELECT` feeding `INSERT` | rowids change with query plan | No index: `1=b 2=a 3=c`. With an index on k: `1=a 2=b 3=c`. |
| `ANALYZE` | IDENTICAL | Same content gives the same stat1 at this version. stat4 was not built here. |
| float → text | IDENTICAL within a version | `CAST(0.1+0.2 AS TEXT)`=`0.30000000000000004`; `printf('%.17g',0.1+0.2)` returned `0.3`. Behaviour across versions is **UNVERIFIED**. |
| default collation / encoding | n/a | BINARY collation is deterministic. `PRAGMA encoding` is persistent and changes every text byte. |

**PRAGMAs stored in the file vs per connection** (set, close, reopen, read back):
`page_size=8192`, `auto_vacuum=2`, `user_version=7`, `application_id=42`, `journal_mode=wal`,
`encoding=UTF-16le` and `default_cache_size` (which leaks into `cache_size`=77) persisted.
`synchronous` (back to 2), `foreign_keys` (0) and `secure_delete` (0) did not. Neither did
`journal_mode` other than WAL, or `cache_size` itself (not tested separately). A
deterministic-build recipe must therefore set the per-connection ones on **every**
connection.

**Conclusion.** A "deterministic build recipe" for the course is: fixed SQL, explicit rowids,
`ORDER BY` on every `INSERT … SELECT`, no clock or random functions (or a seeded PRNG), fixed
persistent pragmas, and the **same transaction boundaries**. Add `secure_delete=ON` if
different batching must match except for header counters.

## Q2. WAL determinism

**Probe:** `q2_wal_determinism.sh` with `go/walreplay`. Each run does: seed, `TRUNCATE`
checkpoint, copy base, `wal_autocheckpoint=0`, then `phase2.sql` (5 transactions). It copies
`-wal` while the connection is open, then checkpoints with `TRUNCATE`.

```
full    wal-bytes:DIFFER    tuples:IDENTICAL frames=412  salt1=0xd38ef036/0xff7218af
normal  wal-bytes:DIFFER    tuples:IDENTICAL frames=412
off     wal-bytes:DIFFER    tuples:IDENTICAL frames=412
seeded  wal-bytes:IDENTICAL tuples:IDENTICAL frames=412  salt1=0x37795e87 both runs   (.testctrl prng_seed 12345)
tuples full vs normal: IDENTICAL; full vs off: IDENTICAL; WAL sizes equal (1697472)
differing bytes between two full-sync WALs, classified:
   walhdr:salt 8  walhdr:cksum 8  frame-salt 3296  frame-cksum 3279   (no page-data or pgno/commit byte differs)
ac100 (wal_autocheckpoint=100): tuples run1 vs run2 IDENTICAL, but differ from autocheckpoint=0;
   header ckptseq=2; 114 valid frames, then "frame 115: salt mismatch (stale frame from earlier WAL generation)"
```

**Where salts come from:**

- The first frame write after a reset calls `if( pWal->nCkpt==0 ) sqlite3_randomness(8, pWal->hdr.aSalt)`
  (`wal.c:4096`).
- A WAL restart, `walRestartHdr` (`wal.c:2152`), increments salt-1 and sets salt-2 from
  `sqlite3_randomness(4,…)`. That call is made at `wal.c:2364-2365` (checkpoint
  RESTART/TRUNCATE) and `wal.c:3883-3884` (`walRestartLog`).
- `sqlite3_randomness` (`random.c:59`) is a ChaCha20 PRNG. It is seeded once, at `random.c:106`,
  by `sqlite3OsRandomness(pVfs,44,…)`, which calls the default VFS `xRandomness`. For unix that
  is `unixRandomness` (`os_unix.c:7100`), reading `/dev/urandom` (`os_unix.c:7121`).
- `sqlite3OsRandomness` (`os.c:273-281`) skips the VFS entirely when
  `sqlite3Config.iPrngSeed != 0`. That field is set by `SQLITE_TESTCTRL_PRNG_SEED`
  (`main.c:4294`), which the CLI exposes as `.testctrl prng_seed` (`shell.c.in:11466`).

**Could a custom VFS `xRandomness` make the WAL byte-deterministic?** Yes, from source: the
PRNG's only external input is the default VFS's `xRandomness` (`random.c:106`). Note that it
reads the **default** VFS (`sqlite3_vfs_find(0)`), not the connection's VFS, and only once per
process. A deterministic VFS must therefore be registered as default before the first
randomness call. The probe used the test-control route instead (`seeded` row above). A Go
course can call `sqlite3_test_control(SQLITE_TESTCTRL_PRNG_SEED, …)` through cgo; whether the
chosen Go driver exposes it is that driver's question.

**What else could perturb the frame sequence** (from source, not provoked):

- `padToSectorBoundary` repeats the commit frame up to a sector boundary (`wal.c:4197`). It is
  disabled when the VFS reports `SQLITE_IOCAP_POWERSAFE_OVERWRITE` (`wal.c:1737-1739`), which
  unix does by default (`SQLITE_POWERSAFE_OVERWRITE` 1, `sqliteInt.h:374`; `os_unix.c:6104-6106`), so no padding was observed.
- Within an open transaction, a page written twice **overwrites its earlier frame in place**
  (`iReCksum`, `wal.c:4142-4160`). The checksums are rewritten at commit (`walRewriteChecksums`,
  `wal.c:3989`).
- Autocheckpoint and WAL restarts change frame numbering (`ac100` above).

**Conclusion.** Teach the WAL as "deterministic frame tuples plus random salts". Byte-level
WAL equality is achievable with a seeded PRNG.

## Q3. Capture methods

Source for a–g: `src.db` = seed + phase2, rollback mode, 1,490,944 bytes, 167 freelist pages
(`q3_capture_methods.sh`). The concurrency cases are in `q3_concurrent.sh`.

| # | Method | vs source bytes | Consistent with an active writer? | Rebuild equality |
| --- | --- | --- | --- | --- |
| a | `cp` of a quiescent db | BYTE-IDENTICAL | **No** (see below) | byte |
| b | `sqlite_dbpage` in one read txn (`writefile` per page, concatenated) | BYTE-IDENTICAL | **Yes**: reads through the pager, so it sees a WAL snapshot | byte (equals the checkpointed db) |
| c | base copy + replay of committed WAL frames (`walreplay`) | BYTE-IDENTICAL to SQLite's `wal_checkpoint(TRUNCATE)` result | Yes, if the WAL copy is bounded by a held read txn (Q5) | byte |
| d | `.backup` (`sqlite3_backup`, step 100) | differs only in header @24, @40, @92 (counter 14→1, cookie 3→1) | Restarts on outside writes. Without a busy handler it can fail with `database is locked` | byte except declared header bytes |
| e | `VACUUM INTO` | all 364 pages differ | Yes (single read txn) | logical; output is **canonical**: run1 = run2 |
| f | `sqlite3_serialize` | BYTE-IDENTICAL (rollback db) | Yes (pager snapshot, `memdb.c:821` uses `sqlite3PagerGet`) | byte (equals checkpointed db) |
| g | `.dump` → replay | all pages differ; replay1 = replay2 | dump runs in one read txn | logical; replay deterministic; `VACUUM INTO` of either side matches |
| h | session changeset → apply to base copy | 187 pages differ from the SQL-built twin | captured inside the writer's connection | logical; `VACUUM INTO` of both sides is BYTE-IDENTICAL |

### a. Plain file copy

- **Quiescent:** `cp` is byte-identical.
- **Lost commits:** take a WAL db, leave the writer open with `wal_autocheckpoint=0`, and copy
  only `.db`. The copy has `tables=0`: the whole seed is still in `-wal`. The same copy plus the
  saved `-wal` shows all 4800 orders.
- **Torn copy:** a 13 MB rollback-mode db; a writer rewrites all 12000 rows per transaction; a
  copier reads 4 KiB blocks with a 0.2 ms sleep (`torn_copy.py`).

  ```
  torn copies: 8 of 8 failed integrity_check
    e.g. "Tree 3010 page 3010 cell 0: invalid page number", "wrong # of entries in index tg",
         "Page 3027: never used", "Freelist: freelist leaf count too big";
    readable copies mixed 1–48 distinct generations
  ```

### b. sqlite_dbpage under a writer

A reader runs `BEGIN; SELECT count(*) FROM orders;`, then another process deletes 2400 orders,
then the reader dumps every page and commits. The capture has `integrity=ok orders=4800` while
the live db has 2400. The capture's `.sha3sum` equals that of a `VACUUM INTO` taken before the
write.

### c. Replaying WAL frames ourselves

The heart of the course. `go/walreplay/main.go` is about 150 lines of Go with no SQLite.
`walreplay apply BASE WAL OUT` was BYTE-IDENTICAL to SQLite's `wal_checkpoint(TRUNCATE)` output
for the `full`, `normal`, `off` and `seeded` runs, and for `ac100` when the base was the db file
copied at the same moment as the WAL.

What must be handled (all exercised by `q2`/`q3c`):

1. **WAL header (32 bytes):** magic, version 3007000, page size, checkpoint seq, salt-1,
   salt-2 and a checksum over bytes 0–23. That checksum seeds the frame chain.
2. **Checksum endianness:** magic `0x377f0682` means the u32 words are summed little-endian;
   `0x377f0683` means big-endian (`wal.c:4092`, `walChecksumBytes` `wal.c:856-901`). This VM
   writes `0x377f0682`. Stored values in headers are always big-endian.
3. **Cumulative checksums:** each frame's checksum continues from the previous one. It covers
   frame-header bytes 0–7 and then the page (`wal.c:1037-1040`). The first mismatch ends the log.
4. **Salts:** a frame whose salts differ from the header belongs to an older WAL generation, so
   stop there (`wal.c:1012`). Observed: `frame 115: salt mismatch` after an autocheckpoint
   restart. Frames from the old generation physically remain after the new ones.
5. **Frames after the last commit frame are ignored.** In `q3c` the WAL was copied
   mid-transaction with `cache_size=5`: 434 valid frames, last commit at 412, 22 spilled
   uncommitted frames ignored, result BYTE-IDENTICAL. After `ROLLBACK` the WAL file still holds
   those 434 frames. Only the wal-index `mxFrame` forgets them. A shipper that does not stop at
   the last commit **would apply rolled-back data**.
6. **In-place frame rewrites inside an open transaction** (`iReCksum`, Q2). The uncommitted tail
   can have a broken checksum chain. Observed: `frame 435: checksum mismatch` in a WAL taken
   mid-transaction. This is harmless if rule 5 is applied.
7. **Truncate the output to commit-frame db-size × page size.** SQLite does the same after a
   complete checkpoint (`wal.c:2328-2331`). Later frames of the same page win, so apply in frame
   order.
8. **Page 1 / header fields need no fix-ups.** Page 1 arrives as a frame image. SQLite's
   checkpoint copies frames verbatim (`wal.c:2310-2321`); it does not edit the header.
9. **The base must match the WAL generation.** `replay(original base, WAL after an
   autocheckpoint)` gave `wrong # of entries in index orders_cust`, a corrupt result. The right
   base is the db as of the WAL's last restart.
10. **Corruption:** one byte flipped in the last commit's page image. Our replay applied 339
    frames (the previous commit). SQLite opening base + the same damaged `-wal` and
    checkpointing produced a BYTE-IDENTICAL file, with integrity ok and the last transaction
    lost.

### d. `.backup`

- The shell loops `sqlite3_backup_step(p,100)` until the result is not `SQLITE_OK`
  (`shell.c.in:9128`). It installs no busy handler.
- The destination gets schema cookie = old + 1 (`backup.c:448`). Its change counter restarts
  from its own value, so a new file gets 1.
- Pages are otherwise identical: `differing pages: 0` with `--ignore-header`.
- Writes from another connection restart the backup (`sqlite3BackupRestart`, `backup.c:728`;
  called from `pager.c:1785,3185,6543`). Writes through the same connection are mirrored
  instead (`sqlite3BackupUpdate`).
- Under a writer committing every 20 ms (each commit a fresh `sqlite3` process), two probe runs
  gave different results. Run 1: 3 of 3 backups OK in 0.29–0.48 s. Run 2: 2 of 3 failed
  immediately with `Error: database is locked`. The likely cause is the writer process's close
  checkpointing and deleting the WAL; this is **PLAUSIBLE, not verified**. With back-to-back
  commits a backup finished in 2.2–2.6 s with integrity ok.
- Conclusion: the result is consistent when it finishes, but a course experiment needs
  `busy_timeout` and a retry loop.

### e. `VACUUM INTO`

- Not equal to the source (all pages differ). Deterministic: run1 = run2.
- It is a **canonical form**:
  - `VACUUM INTO` of the onetxn, autocommit and rowwise builds (3 different files, Q1) gave
    BYTE-IDENTICAL outputs.
  - `VACUUM INTO(.dump replay)` equals `VACUUM INTO(source)`.
  - `VACUUM INTO(changeset-applied)` equals `VACUUM INTO(SQL-built)`.
- A `.dump` replay is *not* equal to `VACUUM INTO` of the source (136 pages differ). The
  cause was not investigated (**UNVERIFIED**). It is not explicit-index order: `.dump` emits
  `CREATE INDEX orders_cust` after all 4116 INSERTs (dump.sql line 4121). One candidate is
  VACUUM's bulk transfer path versus row-by-row INSERT into the UNIQUE autoindex.
- The canonical form is not tested across SQLite versions (**UNVERIFIED**).

### f. serialize / deserialize

`go/serialize` (cgo against `/usr/local/lib`):

- Rollback db: serialize equals the file, BYTE-IDENTICAL.
- WAL db with un-checkpointed frames: serialize differs from the `.db` file at that moment but
  equals the db after `TRUNCATE` checkpoint. In other words it is a pager-level snapshot like
  `sqlite_dbpage`.
- `deserialize` into `:memory:` followed by serialize round-trips identically.

### g. `.dump` → replay

Logical only. The replay is deterministic (replay1 = replay2), and `sqldiff` shows no
difference from the source. `.dump` wraps everything in one transaction and emits
`PRAGMA foreign_keys=OFF`.

### h. Session changesets

- The installed library has no session extension (`nm` shows no `sqlite3session_*`). The probe
  builds the pinned tree with `configure --enable-all` (`SQLITE_ENABLE_SESSION`,
  `SQLITE_ENABLE_PREUPDATE_HOOK`) in `/tmp`.
- `go/session` records `phase2.sql` on A (559,545-byte changeset) and applies it to B, a base
  copy (rc=0, conflict handler ABORT).
- Result: same logical hash, 187 pages differ, and `VACUUM INTO` of both sides is
  BYTE-IDENTICAL. **Logical equality only.** A changeset carries row images keyed by primary key
  and nothing about page layout.

### i. sqlite3_rsync and .recover

- **`sqlite3_rsync`** (`tool/sqlite3_rsync.c`, protocol version 2, `:79`) "makes a copy of a
  live SQLite database using a bandwidth-efficient protocol, similar to rsync".
  - The replica sends page hashes (`REPLICA_HASH`); the origin sends pages that differ
    (`ORIGIN_PAGE`) and then a commit (`ORIGIN_TXN`). Protocol v2 adds finer-grained hash rounds
    (`ORIGIN_DETAIL`).
  - Both sides compute per-page `hash(data)` over `sqlite_dbpage`
    (`tool/sqlite3_rsync.c:1467,1482,1632`). `--wal-only` refuses non-WAL databases
    (`:1396-1397`, `:1855-1857`).
  - The help says one side is normally `USER@HOST:PATH`, but local→local works.
  - Observed: first sync sent 1,443,565 bytes. After both sides checkpointed, the replica
    differed from the origin only in page-1 bytes 27 and 95 (the change counter). A later
    incremental sync of a 49-row update sent 12,326 bytes ("speedup is 108.75"), and then the
    files were BYTE-IDENTICAL.
  - It is a page-level, consistent-snapshot replicator; equality is byte-level except for
    header counters.
- **`.recover`** (`ext/recover/`) reads every page it can parse, including from corrupt b-trees.
  It emits SQL that rebuilds a new db, putting orphan rows into `lost_and_found`.
  - Observed: 4 pages of a 352-page db zeroed, so `integrity_check` reports "database disk
    image is malformed". `.recover` then a replay gave 4789 of 4800 orders plus a
    `lost_and_found` table.
  - It is a salvage tool, not a capture method; the result is logical and possibly incomplete.

## Q4. Verification ladder

`q4_verification_ladder.sh` ran on the q3 artifacts. `dbhash`, `sqldiff` and `sqlite3_rsync`
all **build from the pinned tree** in a `/tmp` copy (`make dbhash sqldiff sqlite3_rsync`
after `configure --enable-all`).

```
file          sha256            pagediff(ignhdr) dbhash            .sha3sum          integrity fk_check
src.db        a858ff2406b0f362  pages:0          aac00d5335d47207  3cee40e375424030  ok        rows=106
a_copy.db     a858ff2406b0f362  pages:0          (same)            (same)            ok        rows=106
b_dbpage.db   a858ff2406b0f362  pages:0          (same)            (same)            ok        rows=106
d_backup.db   3c872420e887b0a7  pages:0          (same)            (same)            ok        rows=106
e_vac1.db     85d1dedbcd4eff2d  pages:364        (same)            (same)            ok        rows=106
g_replay1.db  a44c1bb636eb932e  pages:364        (same)            (same)            ok        rows=106
```

`fk_check` reports 106 rows in every file. `seed.sql` deletes customers that orders still
reference, so this is a property of the content, preserved by every method.

The ladder:

- **(i) sha256 of the file:** strictest. Achievable for cp, dbpage, serialize, WAL replay and
  `VACUUM INTO` vs `VACUUM INTO`.
- **(ii) page diff with a declared ignore-list:** page 1 bytes 24–27, 40–43 and 92–99.
  `.backup` passes at this level. Bytes 18–19 should *not* be ignored, because they distinguish
  WAL from rollback.
- **(iii) logical:** `dbhash` and `.sha3sum`, plus `sqldiff` and integrity/fk checks.

**Blind spots found** (they define what a trustworthy logical hash needs):

```
implicit rowid differs (1,2 vs 5,9):  dbhash equal=YES  .sha3sum equal=YES  sqldiff: DELETE … rowid=1 …
sqlite_sequence differs (1 vs 2):     dbhash equal=YES  .sha3sum equal=YES  sqldiff: UPDATE sqlite_sequence …
user_version differs:                 dbhash equal=YES  .sha3sum equal=YES
1 (INTEGER) vs 1.0 (REAL):            dbhash equal=no   .sha3sum equal=no   sqldiff: [] (no difference!)
```

`dbhash.c` hashes these parts. Tables are taken in `name COLLATE nocase` order with
`SELECT * FROM t` and **no ORDER BY**; the code relies on rowid/PK order (`:457-470`). That
excludes hidden rowids. Each value gets a type tag `0`–`4` (NULL, INTEGER as 8-byte BE, FLOAT
as 8-byte BE IEEE, TEXT, BLOB) (`:298-349`). The schema is `type,name,tbl_name,sql`
(`:479-481`). sqlite_stat1/4 and sqlite_sequence are excluded (help text `:370`).

**A Go "logical hash" is trustworthy if it:**

- tags every value with its storage class and uses a fixed byte encoding (as dbhash does);
- includes the rowid explicitly for rowid tables (`SELECT rowid, *`);
- orders rows by an explicit `ORDER BY` on the rowid or PK, not by relying on scan order;
- hashes `sqlite_schema` (type, name, tbl_name, sql) in a fixed order;
- includes `sqlite_sequence`, plus a declared set of persistent pragmas (`user_version`,
  `application_id`, `encoding`, `page_size`, `auto_vacuum`, journal mode via bytes 18–19);
- optionally adds `sqlite_stat1` contents.

Equality rule: logical hash equal **and** `integrity_check`=ok **and** `foreign_key_check`
unchanged.

## Q5. Continuous capture (WAL shipping) — native hooks at the pin

**Probe `q5_shm_peek.sh`.** It reads `-shm` with Python while connections are open:

```
after 2 commits:                               mxFrame=2 nBackfill=0 readMarks=[0, 1, -, -, -]
after 2 more commits (reader pinned at mark 2): mxFrame=4 nBackfill=0 readMarks=[0, 2, 3, -, -]
PRAGMA wal_checkpoint(PASSIVE) with reader:    0|4|2  -> nBackfill=2 (checkpoint stopped at the reader's mark)
after reader ends, PASSIVE again:              0|4|4  -> nBackfill=4
```

Native facilities, with exact references:

| Facility | Reference | Use for capture |
| --- | --- | --- |
| Reader marks cap checkpoints | `walCheckpoint` `wal.c:2199`; `mxSafeFrame` lowered to a busy reader's `aReadMark` at `wal.c:2233-2245` | Hold a read txn so frames beyond your copy point are not backfilled past it. Verified above. |
| WAL restart needs every reader off the log | `walRestartLog` `wal.c:3875`: exclusive `WAL_READ_LOCK(1..N)` before `walRestartHdr` | A held read txn also prevents the WAL from wrapping under the shipper. |
| Restart is visible as a salt change | `walRestartHdr` `wal.c:2152`: salt1+1, new salt2 | Shipper detects a new generation; observed `ckptseq=2` and stale-salt frames. |
| `PRAGMA wal_autocheckpoint=0` / `sqlite3_wal_autocheckpoint` | `sqlite.h.in:10040`; it wraps `sqlite3_wal_hook` | Shipper owns checkpoints. Per connection: every app connection must also disable it. |
| `sqlite3_wal_hook(db, cb, arg)` | `sqlite.h.in:10004`; invoked from `doWalCallbacks` `vdbeapi.c:745/757` with `sqlite3WalCallback` (`wal.c:4439`) giving the frame count | After each commit: (db name, frames in WAL). In-process trigger for "copy new frames". |
| `sqlite3_wal_checkpoint_v2(db, z, mode, &nLog, &nCkpt)` | `sqlite.h.in:10068`; modes `NOOP=-1, PASSIVE=0, FULL=1, RESTART=2, TRUNCATE=3` at `sqlite.h.in:10178-10182` | `SQLITE_CHECKPOINT_NOOP` **exists at this revision**: it reads nLog/nCkpt without work. |
| `SQLITE_FCNTL_CKPT_START` / `CKPT_DONE` | `sqlite.h.in:1280/1278`; issued at `wal.c:2285` and `wal.c:2324` around the backfill loop | A VFS shim sees checkpoint boundaries, for example to snapshot the db file. |
| `SQLITE_FCNTL_PERSIST_WAL` | `sqlite.h.in:1252`; consulted at close, `wal.c:2538` | Keep `-wal` after the last close (otherwise close checkpoints and deletes it; seen in Q1 `wal_nockpt`). |
| `SQLITE_FCNTL_DATA_VERSION` | `sqlite.h.in:1276` | Cheap "did anything commit" check. |
| `sqlite3_snapshot_get/open/cmp/recover` | `sqlite.h.in:11008…`; compiled only under `SQLITE_ENABLE_SNAPSHOT` (`main.c:5055-5190`) | **Not in the installed lib** (`nm -D` shows 0 snapshot symbols). Needs a custom build. |
| wal-index header in `-shm` | `struct WalIndexHdr` `wal.c:321-333` (48 bytes, 2 copies at 0 and 48, native byte order; `mxFrame` @16, `nPage` @20, salts @32); `WalCkptInfo` `wal.c:394-400` at 96 (`nBackfill` @96, `aReadMark[5]` @100, `WAL_NREADER` `wal.c:299`) | Read-only monitoring, verified above. Writers must never touch it. |

**Prior art** (fetched 2026-11-20; quotes from the pages):

- **Litestream** ([how it works](https://litestream.io/how-it-works/)):
  - It "starts a long-running read transaction to prevent any other process from checkpointing
    and restarting the WAL file". It then "continually reads new WAL pages and manually calls
    out to SQLite to perform checkpoints".
  - v0.3.x used random "generation" IDs and a directory of "shadow WAL" files. v0.5 "replaces
    both concepts with TXID-based LTX files", compacted in levels.
  - Restore fetches "the most recent snapshot that does not overshoot the requested restore
    point" and applies LTX files in TXID order.
  - That page does not cover how a WAL restart is detected (salt comparison). The salt
    mechanism above is SQLite's, and Litestream's use of it is **UNVERIFIED**.
- **LTX** ([superfly/ltx](https://github.com/superfly/ltx)):
  - Layout: a 100-byte header (MinTXID, MaxTXID, timestamp, PreApplyChecksum, Commit db size),
    a page block sorted by pgno with LZ4 payloads, a page index, and a 16-byte trailer
    (PostApplyChecksum, FileChecksum).
  - The file checksum is CRC-ISO-64.
- **LiteFS** ([ARCHITECTURE.md](https://github.com/superfly/litefs/blob/main/docs/ARCHITECTURE.md)):
  - A FUSE layer "intercepts file system calls to record transactions" and converts them to LTX.
  - It keeps a "rolling checksum of the entire database": the XOR of CRC64(pgno‖page) over all
    pages, updated incrementally.
  - This is a strong candidate for the course's own **incremental page-set hash**.
- **rqlite** ([design](https://rqlite.io/docs/design/),
  [non-deterministic](https://rqlite.io/docs/api/non-deterministic/)):
  - Statement-based: the Raft log "stores the set of committed SQLite commands".
  - It rewrites `RANDOM()`, `RANDOMBLOB(N)` and `now`-style functions into literals before
    logging. It does not cover defaults, triggers or generated columns, nor `RANDOM()` in
    ORDER BY.
  - This is a direct illustration of the Q1 SQL-nondeterminism list.
- **sqlite3_rsync:** in-tree, see Q3i.

## Q6. Crash vs power loss

**`kill -9`** (`q6_kill9.sh`): a writer prints each committed counter and is killed at
0.4–0.6 s. Three trials each:

```
DELETE sync=FULL  last acked 21/27/43     in db 21/27/43        integrity ok
DELETE sync=OFF   last acked 1743/2266/2603  in db = same       integrity ok
WAL    sync=FULL  last acked 77/112/136   in db 78/113/137      integrity ok
WAL    sync=OFF   last acked 4529/5024/6802  in db = same       integrity ok
```

No acknowledged commit was ever lost, **including with `synchronous=OFF`**. Committed-but-not-yet-acknowledged rows (db = acked+1) do appear.

- **What `kill -9` proves:** process-crash atomicity. Hot-journal rollback and WAL recovery
  work, and a commit is visible once `write()` returns. The OS page cache survives, so fsync is
  never tested.
- **What it cannot prove:** that `synchronous` settings matter. Proving that needs simulated
  loss of unsynced writes.

**In-tree crash simulation: `src/test6.c`.**

- It is a VFS wrapper that buffers every write in a list. On the Nth `xSync` of a named file it
  writes, drops or garbles each buffered write according to the sector size and device
  characteristics, then calls `exit(-1)` (`writeListSync` `:186`, `cfSync` `:469`, exit at
  `:346`).
- It compiles only under `#if SQLITE_TEST` (`:17`), allocates with `Tcl_AttemptAlloc`
  (`:160-166`), and is configured only through Tcl commands (`:717ff`). So it is **not usable
  outside the TCL harness as is**.
- Porting it to a standalone C or Go VFS would be modest work (replace the Tcl alloc and the
  parameter command). That estimate is **UNVERIFIED**.
- `test_vfs.c` (1697 lines) and `test_devsym.c` are also Tcl-bound test VFSes.

**Feasible on this VM:**

- **(1) A VFS shim that buffers writes until `xSync`,** then on a simulated "power cut" discards
  or tears unsynced writes and exits. Courses can write this in C via cgo, or in pure Go with a
  Go-VFS driver (driver choice is another topic). This is the most portable option.
- **(2) Block-level:** `modprobe -n -v dm-flakey` and `dm-log-writes` both resolve to
  `/lib/modules/6.8.0-139-generic/kernel/drivers/md/dm-{flakey,log-writes}.ko.zst`. They are
  present but **not loaded, and were not loaded** by this probe. `dmsetup` and `losetup` are
  installed, and `/proc/misc` lists device-mapper.
  - dm-log-writes records every write with flush/FUA marks, so a post-hoc replay to any flush
    point models power loss exactly. That needs a `replay-log` tool, which is **not installed**.
  - dm-flakey `drop_writes` is cruder.
  - Both need root and a loop device; that is fine on this disposable VM but heavier for
    readers.
- **(3) LazyFS** (a FUSE fs that drops unsynced data): **not installed**.

---

## Recommended capture ladder for the course

Ordered from simplest to continuous WAL shipping. "Provable" means demonstrated by a probe here.

| Rung | Capture | Deterministic rebuild by | Provable equality |
| --- | --- | --- | --- |
| 0 | **Rebuild from the recipe:** fixed SQL + rules from Q1 | rerun into a fresh file | **sha256 of the file**, same SQLite build (C or modernc). Across batching: page diff ignoring @24/@92 with `secure_delete=ON`. |
| 1 | **Quiescent `cp`** (all connections closed, `-wal` gone) | copy back | sha256. Teach the failure modes: lost commits without `-wal`, 8/8 torn copies under a writer. |
| 2 | **Logical dump** (`.dump` / ordered table dumps) | replay into a fresh db | Logical hash (Q4 Go design) + sqldiff. Replays are byte-deterministic, and `VACUUM INTO` of the replay matches `VACUUM INTO` of the source byte-for-byte. |
| 3 | **Canonical snapshot** via `VACUUM INTO` | none needed: the snapshot *is* the canonical form | **sha256 of canonical forms.** Makes logical equality byte-checkable. |
| 4 | **Online page snapshot:** `sqlite_dbpage` in a read txn, `sqlite3_serialize`, or `sqlite3_backup` | write pages to a file | sha256 equal to the source after a `TRUNCATE` checkpoint (dbpage, serialize). `.backup`: page diff with the declared header ignore-list. |
| 5 | **Change capture, logical:** session changesets | apply to a base copy | Logical, plus sha256 after `VACUUM INTO` on both sides. |
| 6 | **WAL frame capture + own replay** (the core build, `walreplay`) | base + committed frames, truncate to commit size | **sha256 equal to SQLite's own `wal_checkpoint(TRUNCATE)`**. Frame-tuple sequence identical across runs. Whole WAL byte-identical with a seeded PRNG. |
| 7 | **Continuous WAL shipping** (capstone) | snapshot + ordered segments; verify chain, salts and per-segment pre/post hashes (LTX-style) | sha256 at every shipped commit point vs a `TRUNCATE` checkpoint of the source. Rolling page-set hash (LiteFS XOR-of-CRC idea) for incremental checks. |

Crash rungs alongside the ladder: `kill -9` proves process-crash atomicity only. A
write-buffering VFS shim, or dm-log-writes, is needed to make any claim about `synchronous`.

## Open items / UNVERIFIED

- Cross-version byte equality (only 3.53.4 builds were available offline). `VACUUM INTO`'s
  canonical form across versions.
- Mechanism of the constant WAL-mode change counter (observed only).
- Litestream's WAL-restart detection mechanism.
- Why `.dump` replay and `VACUUM INTO` produce different page layouts.
- `.backup` lock failures: attributed to writer close-time checkpoints, not proven.

## INDEX rows

| path | purpose | chapter | made by | what it proved |
| --- | --- | --- | --- | --- |
| `probes/determinism-rebuild/run_all.sh` | Build pinned-tree tools + Go helpers, run every probe | determinism/rebuild | research agent determinism-rebuild, 2026-11-20 | All probes rerun from one command (exit 0) |
| `probes/determinism-rebuild/seed.sql`, `phase2.sql` | Fixed deterministic workloads | determinism/rebuild | same | Inputs for all probes |
| `probes/determinism-rebuild/hdr.py` | Dump variable db header fields | file format / determinism | same | Which header fields differ (@18/19/24/28/32/36/40/92/96) |
| `probes/determinism-rebuild/pagediff.py` | Page-by-page diff, optional header ignore-list | verification | same | Localizes byte diffs to page 1 counters and freelist leaf |
| `probes/determinism-rebuild/q1_build_determinism.sh` | Same script twice × 7 configs; batching variants | determinism | same | Byte-identical rebuilds in all configs; batching changes bytes only via counters, freelist and tree shape |
| `probes/determinism-rebuild/q1b_freelist_secure_delete.sh` | Freelist-leaf stale content vs secure_delete | determinism | same | secure_delete=ON makes onetxn and autocommit equal except header counters |
| `probes/determinism-rebuild/q1c_sql_nondeterminism.sh` | SQL nondeterminism sources; persistent vs per-connection pragmas | determinism | same | random/now/max-rowid differ; AUTOINCREMENT/ANALYZE/float stable; which pragmas persist |
| `probes/determinism-rebuild/go/modernc_build/main.go` | Build seed via modernc.org/sqlite v1.58.0 | determinism / drivers | same | modernc (3.53.4) output byte-identical to the CLI |
| `probes/determinism-rebuild/go/walreplay/main.go` | Parse WAL, replay committed frames onto base (no SQLite) | WAL / capstone | same | Replay byte-identical to SQLite TRUNCATE checkpoint, incl. uncommitted tail and corrupt frame |
| `probes/determinism-rebuild/q2_wal_determinism.sh` | WAL bytes vs frame tuples across runs/sync/seed/autockpt | WAL | same | Only salts and checksums differ; prng_seed gives a byte-identical WAL |
| `probes/determinism-rebuild/q3c_wal_tail.sh` | Uncommitted tail, corrupted frame, page-1 fields | WAL | same | Stop-at-last-commit rule needed; our replay equals SQLite recovery on a damaged WAL |
| `probes/determinism-rebuild/q3_capture_methods.sh` | cp / dbpage / backup / VACUUM INTO / dump on a quiescent db | capture | same | cp and dbpage byte-equal; backup differs at @24/@40/@92; VACUUM INTO canonical |
| `probes/determinism-rebuild/q3_concurrent.sh`, `torn_copy.py` | Captures under an active writer | capture / concurrency | same | Lost commits without -wal; 8/8 torn copies; dbpage snapshot consistent; backup can hit SQLITE_BUSY |
| `probes/determinism-rebuild/go/serialize/main.go`, `q3f_serialize.sh` | sqlite3_serialize/deserialize via cgo | capture | same | serialize equals the file (rollback) or the checkpointed db (WAL) |
| `probes/determinism-rebuild/go/session/main.go`, `q3h_session.sh` | Session changeset record/apply (session-enabled build) | capture | same | Changeset apply is logically equal; VACUUM INTO of both is byte-equal |
| `probes/determinism-rebuild/q3i_rsync_recover.sh` | sqlite3_rsync local sync; .recover on damaged db | capture / recovery | same | rsync replica equals origin except counter, then byte-equal; .recover salvages 4789/4800 rows |
| `probes/determinism-rebuild/q4_verification_ladder.sh` | sha256 / pagediff / dbhash / sha3sum / integrity on all captures; blind spots | verification | same | dbhash & .sha3sum ignore rowid, sqlite_sequence, user_version; sqldiff misses 1 vs 1.0 |
| `probes/determinism-rebuild/q5_shm_peek.sh` | Read mxFrame/nBackfill/read marks from -shm | continuous capture | same | A held read txn caps checkpoint backfill (nBackfill 2 of 4 until released) |
| `probes/determinism-rebuild/q6_kill9.sh` | kill -9 writer under sync FULL/OFF, rollback/WAL | durability | same | kill -9 never loses acked commits even with synchronous=OFF, so it cannot test fsync |
