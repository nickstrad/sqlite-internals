# Chunk 1 build notes (plain data for the prose pass)

Builder: Claude Opus 5.5, branch `chunk/ch01`, 2026-11-20. Machine: linux/amd64, Go 1.26.8,
strace 6.8, ext4. CLI used for the evidence files: the installed `/usr/local/bin/sqlite3` (found
on PATH; it calls `fsync`). Also run with the `go run ./cmd/course build-tools` CLI (calls
`fdatasync`) and inside the Debian `lab` container: EXACT output identical in all three.
darwin: cross-built and cross-vetted only, never run.

All source line numbers are at the pin (3.53.4). Spans in `SPANS.txt` were checked against the
pinned tree (`go run ./cmd/course check spans ch01` passes) and the outline's numbers were all
correct; one span was added (`sqlite3_prepare_v2` 937–954, named in the chapter's question) and
`os.c` 88–102 is split into its three named wrappers too.

## Corrections to the outline

1. **In WAL mode the file change counter counts connection sessions, not commits.** Outline §4.2
   says header counters (bytes 24–27, 92–95) count commits; that is true in rollback mode only.
   In WAL mode, `pagerWalFrames` (pager.c:3264) calls `pager_write_changecounter`, which writes
   `dbFileVers + 1` (pager.c:3135), but `dbFileVers` is refreshed only when page 1 is read into
   the cache (`readDbPage`, pager.c:3109–3110), not after a WAL commit. On one long-lived
   connection every commit writes the same value; each new connection re-reads page 1 and moves
   it by one. Evidence: 1.3 variant 6 — WAL, steps 0–6 one connection per step vs one connection
   for all: `differ: first differing page 1, ... page 1 bytes [27 95]`; rollback mode, same
   comparison: `identical`. Consequence for the recipe: **the canonical build opens and closes
   one connection per step** (`recipe.Build`), and `recipe.MatchStep` also reports a match that
   differs only in those 8 bytes (see the recipe section). This is a sixth source of variation the
   outline's §4.2 table does not list.
2. **"Same rows in different transaction batches give different bytes" does not hold in WAL mode
   with the recipe's settings.** WAL mode, one connection, `secure_delete=ON`, steps 2+3 and 4+5
   each committed as one transaction instead of two: `identical`. The batching difference shows
   (a) with `secure_delete=OFF`: `differ: first differing page 24, 2 page(s) differ [24 28]`,
   and every differing byte lies in space no cell uses (page 24: 357 bytes, page 28: 1207 bytes,
   both b-tree leaf pages, the gap between the cell-pointer array and the cell content area), or
   (b) in rollback mode: `differ ... page 1 bytes [27 95]` (the change counter counts commits
   there). Which pages show stale bytes depends on which steps are merged: merging 3+4 and 5+6
   gave `identical` even with `secure_delete=OFF` (tried while building; the experiment uses
   2+3 and 4+5, where step 3's deletes free space that step 2 wrote in the same transaction).
   The stale bytes are NOT on freelist pages here (freelist of both files: pages 29, 36, 37);
   the determinism report described the freelist-leaf case.
3. **An unordered `INSERT … SELECT` run twice gives identical files.** With the same build and
   the same plan the scan order is the same, so the rowids match: `identical`. The smallest
   change that shows the rule is a different scan order: run 2 with
   `PRAGMA reverse_unordered_selects=ON` → `differ: first differing page 29, 1 page(s) differ`;
   with `ORDER BY id` the same pair is `identical`. (A new index, `ANALYZE` data, or another
   SQLite version can change a plan the same way; those were not run.)
4. **`current_timestamp` has one-second resolution.** Two runs inside one wall-clock second gave
   `identical` files; a third run after the second changed gave `differ: first differing page
   59`. The experiment waits for a second boundary to show both; it retries if the two "same
   second" inserts straddle a boundary (never needed in the runs here).
5. **The twin check is not identical in every syscall:** modernc's libc is a translation of musl,
   and musl's `open()` issues `fcntl(fd, F_SETFD, FD_CLOEXEC)` after every open that asks for
   `O_CLOEXEC` (modernc.org/libc v1.77.1 `ccgo_linux_amd64.go:24802`). The C build does not
   (os_unix.c `robust_open` only calls F_SETFD when `O_CLOEXEC` is undefined, :867–868). In the
   INSERT: Go driver 2 extra calls in rollback mode (`fcntl t.db-journal F_SETFD`,
   `fcntl . F_SETFD`), 1 in WAL mode (`fcntl . F_SETFD`). Everything else matches: same
   writes, syncs, reads, locks, opens (by path), unlinks and closes, in the same order. Already
   known from chunk 0: `open` vs `openat`, and `fsync` (Go, installed CLI) vs `fdatasync`
   (`./configure` CLI). The experiment prints the F_SETFD difference on a `twin difference:` line.
6. **The commit's write lands on the database file only in rollback mode.** The outline's
   evidence line "(changed page number − 1) × 4096 equals the pwrite64 offset on the database
   file" is right for rollback mode only; in WAL mode the INSERT makes zero writes to `t.db` and
   the page goes to `t.db-wal` (frame header at 32, page image at 56). Both are shown.
7. **Rung R0 evidence uses the recipe's settings, which include `secure_delete=ON`.** The outline
   lists `secure_delete=ON` only "where two different histories must match"; the recipe always
   sets it, because later chapters drive the same steps through different histories (child
   processes, crashes, checkpoints at different times). Measured while building (scratch probe
   `agent_work/ch01/recipe-probe`): with `secure_delete=ON` the WAL recipe's final file is the
   same whether built with `wal_autocheckpoint` 0, 1 or 1000, `cache_size=5`, or
   `synchronous=NORMAL` (exactly, with one session per step; apart from the change counter with
   one connection); with `secure_delete=OFF` it differs.

Nothing else in the chapter-1 section of the outline was false here. WAL as the default made
sense for every part of this chapter; the only rollback-mode parts are the ones the brief asks
for (1.2's rollback table, and the rollback variants in 1.3 used to explain the WAL findings).

## internal/recipe

Files: `recipe.go` (generation), `apply.go` (applying and building), `hashes.go` (hashes, cache,
lookup), `recipe_test.go`. No comments (seat rule).

### What a recipe is

- `Recipe{Seed uint64, Steps int, JournalMode string}`. `Default()` = seed 1, 30 steps, `wal`.
  `WithSteps`, `WithJournalMode` return modified copies. `Validate()`: steps 0..10000, journal
  mode `wal` or `delete` only.
- Step 0 is the schema: three `CREATE TABLE` statements (`SchemaSQL`) and
  `PRAGMA user_version = 0`. Steps 1..N are the workload. Every step is exactly one transaction
  and ends with `PRAGMA user_version = k` inside that transaction.
- Tables: `accounts(id INTEGER PRIMARY KEY, name TEXT, balance INTEGER)`,
  `documents(id INTEGER PRIMARY KEY, account_id, title TEXT, body BLOB)`,
  `events(id INTEGER PRIMARY KEY, account_id, kind TEXT, amount INTEGER, note TEXT)`.
- Each workload step k (rng = `rand.New(rand.NewPCG(seed, k))`): 4–8 account inserts; 2–5
  document inserts with a random body of 2000–16000 bytes (`MinBodyBytes`, `MaxBodyBytes`;
  above 4061 bytes a row overflows a 4096-byte page, so most bodies need overflow pages and some
  do not); 30–60 event inserts; 2–5 balance updates; with probability 1/2 one document body
  replaced; if k%3==0, 4–11 event deletes; if k%4==0 and more than 2 documents exist, one document
  delete (frees its overflow chain); at k = `IndexStep` (5) `CREATE INDEX events_by_account ON
  events(account_id, id)`.
- Every insert supplies its integer primary key (ids are counters per table, 1, 2, 3, …); every
  value is a bound parameter; no SQL function is used in any statement (the test bans `random`,
  `now`, `current_`, `select` in the SQL text).
- **Step k depends only on (seed, k)**: the generator walks steps 0..k in order keeping the live-id
  lists; the step count only says where to stop. `TestStepDoesNotDependOnStepCount` checks a
  7-step and a 25-step recipe agree on steps 0–7.
- `Step{Number, Statements []Statement{SQL, Args}, Kinds []StepKind}`; `Counts()`, `Summary()`
  (e.g. `step 3: schema=0 inserts=63 updates=4 deletes=11 indexes=0`).
- `SQLiteOptions()`: `sqlitex.DefaultOptions()` with the recipe's journal mode,
  `secure_delete=ON`, `SingleConnection`. Everything else is sqlitex's default (page 4096,
  auto_vacuum NONE, UTF-8, the course application_id, synchronous FULL, foreign_keys ON,
  cache_size -2000, wal_autocheckpoint 1000, busy_timeout 5 s).
- Size: default recipe = 270 pages, 1,105,920 bytes. Build time: about 0.9–1.2 s in WAL mode,
  0.7 s in rollback mode here (each WAL session pays the WAL-creation and directory syncs).

### Applying steps

- `Step.ExecIn(ctx, Execer)`: runs the statements in order on a `*sql.Tx` (or anything with
  `ExecContext`). Used to put several steps in one transaction (1.3 variant 4).
- `ApplyStep(ctx, TxBeginner, Step)`: begin, ExecIn, commit; rolls back on error. With the
  sqlitex default `_txlock=immediate` the BEGIN takes the write lock at once.
- `Recipe.ApplyStep(ctx, db, k)`: generate step k and apply it. `Recipe.ApplyNext(ctx, db)`:
  reads `CurrentStep` and applies the next one (for child processes that drive one step each).
- `CurrentStep(ctx, q)`: -1 if none of the three recipe tables exist; error if only some exist;
  otherwise `PRAGMA user_version`.
- `Recipe.Open(ctx, path)`: `sqlitex.Open` with `SQLiteOptions()`.
- `Recipe.ApplyInSession(ctx, path, step)`: open, apply one step, close, then `CheckAtRest`.
- `Recipe.Build(ctx, path, upTo)` / `BuildEach(..., afterStep)`: removes the database and its
  `-wal`, `-shm`, `-journal` files, then runs `ApplyInSession` for steps 0..upTo — one connection
  session per step (correction 1). `afterStep(k)` runs after step k's file is at rest.
- `RemoveDatabase(path)`, `CheckAtRest(path)` (error if any side file exists).

### Expected states and the lookup

- `HashFile(path)` → `FileHashes{Exact, Masked}`: sha256 of the file, and sha256 of the file with
  bytes 24–27 and 92–95 zeroed (`MaskChangeCounter`, `ChangeCounterRanges`). `HashImage` does the
  same for bytes in memory. `FileSHA256(path)` is the plain sha256.
- `Recipe.ComputeStepHashes(ctx, dir)`: builds the recipe in `dir` (one session per step) and
  hashes the closed file after every step; returns `Steps+1` entries.
- `Recipe.StepHashes(ctx)`: the same list, cached in `<work base>/recipe-cache/
  v<FormatVersion>-<first 16 of the Fossil id>-seed<seed>-<mode>.txt` (lines `k exact masked`).
  `<work base>` is `$COURSE_WORK_DIR` or `<repo>/_work`; the cache directory is not wiped by
  experiments (they wipe only `_work/N.M`). A cache with at least `Steps+1` lines is used as a
  prefix; a short or malformed cache is rebuilt and replaced (written to a temp file, renamed).
  Bump `FormatVersion` whenever the generator changes.
- `Recipe.ExpectedHash(ctx, k)`: exact hash after step k.
- `Recipe.MatchStep(ctx, path)` → `Match{Step, Exact}` — "which recipe step does this database
  equal?". The file must be at rest. Exact sha256 match first; otherwise a match on the masked
  hash (`Exact=false`: equal except the change counter, e.g. a database built by one long-lived
  WAL connection); otherwise `Step=-1`. `MatchHashes(list, h)` is the pure lookup.

### Tests (`go test ./internal/recipe/`, about 3–5 s)

Validate; plan determinism; step independent of step count; seeds differ (and step 0 does not);
plan shape (one index at step 5, user_version last, explicit ids, no SQL functions, bodies both
over and under 4061 bytes); two builds identical and at rest in both modes, integrity ok,
overflow pages present (via `dbstat`), journal mode as asked; per-step hashes equal separate
builds to each k and all distinct; long-lived connection equals the canonical build only in the
masked hash under WAL and exactly under DELETE; MatchStep exact / masked / no match / not at
rest and cache prefix reuse and corrupt-cache rebuild (with `COURSE_WORK_DIR` set to a temp dir);
ApplyNext from an empty file to the end; a failed step (re-applying step 1 hits its explicit
primary keys) leaves `CurrentStep` unchanged; MaskChangeCounter.

## Chapter directory

- Copied whole from `$SQLITE_SRC/src/`: `vdbeapi.c`, `vdbeaux.c`, `prepare.c`, `os.c`;
  `SOURCES.sha256` lists them; `go run ./cmd/course check sources ch01` passes.
- `SPANS.txt`: 12 spans; `check spans ch01` passes. `vdbe.c` is cited (two opcodes) but not copied,
  as the outline says.

## What each span does (facts read from the source)

- **`sqlite3_step`, vdbeapi.c 919–966.** The public entry point. It takes the connection mutex
  (:929) and calls `sqlite3Step` in a loop (:930): while the result is `SQLITE_SCHEMA` and fewer
  than `SQLITE_MAX_SCHEMA_RETRY` (50, vdbeInt.h:26) retries have happened, it recompiles the
  statement with `sqlite3Reprepare` (:933), resets it (:954) and steps again. So a schema change
  by another connection is handled inside one `sqlite3_step` call; the caller never sees it unless
  recompiling fails.
- **`sqlite3Step`, vdbeapi.c 777–912.** If the statement is in the READY state it checks whether
  it has expired (schema changed → `SQLITE_SCHEMA`), counts the statement as active and as a
  writer if it is not read-only (:822), and sets the program counter to 0 (:825); a statement
  already HALTed is reset first (auto-reset, :852). Then it either produces EXPLAIN rows with
  `sqlite3VdbeList` (:864) or runs the bytecode with `sqlite3VdbeExec` (:869). On
  `SQLITE_DONE` in autocommit mode it runs the WAL hook callbacks (`doWalCallbacks`, :886).
- **`sqlite3_prepare_v2`, prepare.c 937–954.** Calls `sqlite3LockAndPrepare` with
  `SQLITE_PREPARE_SAVESQL` (:950), which keeps the SQL text in the statement so it can be
  recompiled later (that is what makes the retry loop in `sqlite3_step` possible).
- **`sqlite3LockAndPrepare`, prepare.c 836–875.** Takes the connection mutex (:855) and enters
  every attached database's B-tree mutex (`sqlite3BtreeEnterAll`, :856), then calls
  `sqlite3Prepare` in a loop: retry on `SQLITE_ERROR_RETRY`, and retry once on `SQLITE_SCHEMA`
  after discarding the cached schema (`sqlite3ResetOneSchema`, :866).
- **`sqlite3Prepare`, prepare.c 682–835.** Sets up a `Parse` object, refuses if another shared-
  cache connection holds a schema lock (`sqlite3BtreeSchemaLocked`, :752), runs the tokenizer and
  parser (`sqlite3RunParser`, :779 or :786), whose grammar actions call the code generators that
  emit bytecode into a `Vdbe`. It stores the SQL text on the statement (`sqlite3VdbeSetSql`,
  :795), checks the schema cookie if compilation failed (`schemaIsValid`, :803), and on success
  hands the `Vdbe` back as the `sqlite3_stmt` (:818). Prepare does no file I/O of its own beyond
  loading the schema on first use; compiling and running are separate steps.
- **`OP_Transaction`, vdbe.c 4102–4201.** Starts a read (P2=0) or write (P2≠0) transaction on
  database P1 by calling `sqlite3BtreeBeginTrans` (:4128); on `SQLITE_BUSY` it saves the program
  counter and returns, so the statement can be stepped again. When P5 is set it compares the
  schema cookie read from the file with the one the statement was compiled against (P3) and
  expires the statement with "database schema has changed" (:4174, :4191). In this INSERT it is
  at address 14 with P1=0 P2=1 P3=1 P5=1 (write transaction on `main`, schema cookie 1).
- **`OP_Insert`, vdbe.c 5748–5834.** Takes the record built by `OP_MakeRecord` (register P2) and
  the rowid (register P3) and writes them into the B-tree through the cursor P1 with
  `sqlite3BtreeInsert` (:5816). It counts the change and sets `last_insert_rowid` when P5 asks
  (:5802–5803), and fires the pre-update and update hooks if registered. Nothing here touches a
  file directly; the B-tree asks the pager to make the page writable (`sqlite3PagerWrite`), and
  in rollback mode that is what first opens and writes the journal (1.2, INSERT phase).
- **`sqlite3VdbeHalt`, vdbeaux.c 3315–3517.** Runs when a statement finishes (`OP_Halt`, or
  `OP_AutoCommit` for COMMIT). It closes the statement's cursors (:3339) and decides the
  transaction's fate: if the connection is in autocommit mode and this is the last active
  writing statement, it checks deferred foreign keys and calls `vdbeCommit` (:3421); on a failed
  commit it rolls everything back (`sqlite3RollbackAll`, :3429). Otherwise it only releases or
  rolls back the statement's sub-transaction (`sqlite3VdbeCloseStatement`, :3464).
- **`vdbeCommit`, vdbeaux.c 2919–3175.** Commits every database with an open write transaction.
  It first takes an EXCLUSIVE lock on each (`sqlite3PagerExclusiveLock`, :2973 — the
  `xLock(t.db,EXCLUSIVE)` at the start of 1.2's rollback COMMIT phase) and counts how many need a
  super-journal (only DELETE/PERSIST/TRUNCATE journals, `aMJNeeded`, :2954). With one database
  (this course's case, and always in WAL mode) it does phase one then phase two
  (`sqlite3BtreeCommitPhaseOne` :3005, `sqlite3BtreeCommitPhaseTwo` :3020). With two or more
  rollback-journal databases it writes and syncs a super-journal naming every journal
  (:3035–3147) so the multi-file commit is atomic.
- **`sqlite3OsRead` / `sqlite3OsWrite` / `sqlite3OsTruncate` / `sqlite3OsSync`, os.c 88–102.**
  One-line wrappers: each calls the open file's method table (`id->pMethods->xRead`, `xWrite`,
  `xTruncate`, `xSync`). `sqlite3OsSync` skips the call when its flags are 0. This is the VFS
  boundary: everything above calls these, and the VFS below turns them into `pread64`,
  `pwrite64`, `ftruncate`, `fsync`/`fdatasync`.

Related lines outside the copied files that the 1.2 tables rely on (pinned tree):
pager.c `pager_write` → `pager_open_journal` :6121 (journal opened on the first page write);
`sqlite3PagerCommitPhaseOne` 6511–: `pagerWalFrames` :6558 (WAL), `pager_incr_changecounter`
:6636, `syncJournal` :6658, `pager_write_pagelist` :6666/:6696, `sqlite3PagerSync` :6719;
`syncJournal` 4332–: reads 8 bytes at the next journal-header offset to check for a stale header
(:4381, the `xRead(t.db-journal,n=8,ofst=9216) -> SQLITE_IOERR_SHORT_READ`), syncs the journal,
writes the 12-byte header with the record count, syncs again (FULL); wal.c `walRestartLog`
3875– releases READ0 and takes READ1 when the first write of a WAL generation begins
(:3902, :3907 → `walTryBeginRead`).

## Experiment 1.1 pin-check

Run: `go run ./ch01-architecture-and-execution-path/experiments/1.1-pin-check` (about 1 s).
Evidence: `evidence/1.1-pin-check.txt`. Exit 0, last line `PIN CHECK OK`.

What it does: opens `:memory:` with the plain driver (`sql.Open("sqlite", …)`), prints the pin
line, the PIN file's release/tag/git/Fossil ids, the module versions actually linked (from
`debug.ReadBuildInfo`: `modernc.org/sqlite v1.60.1`, `modernc.org/libc v1.77.1`),
`sqlite_version()` (3.53.4), all 56 driver compile options, checks the three required options,
creates a table and counts `sqlite_dbpage` rows (2) and `dbstat` rows (2). Then locates the CLI
(`sqlitex.LocateCLI`, must report the pin), prints its version and checks the same three options,
and prints 15 file-format/I/O options side by side.

Confirmed option names and values (both builds): `ENABLE_DBPAGE_VTAB`, `ENABLE_DBSTAT_VTAB`,
`DEFAULT_WAL_SYNCHRONOUS=2` (2 = FULL), also `DEFAULT_SYNCHRONOUS=2`. Same in both:
DEFAULT_AUTOVACUUM, DEFAULT_CACHE_SIZE=-2000, DEFAULT_FILE_FORMAT=4, DEFAULT_JOURNAL_SIZE_LIMIT=-1,
DEFAULT_MMAP_SIZE=0, DEFAULT_PAGE_SIZE=4096, DEFAULT_PCACHE_INITSZ=20, DEFAULT_SECTOR_SIZE=4096,
DEFAULT_WAL_AUTOCHECKPOINT=1000, DIRECT_OVERFLOW_READ, MAX_MMAP_SIZE=0x7fff0000,
MAX_PAGE_SIZE=65536, TEMP_STORE=1.

Options only in the driver (INFO line, depends on the CLI found): COMPILER=gcc-12.2.0,
DEFAULT_MEMSTATUS=0, DISABLE_INTRINSIC, ENABLE_COLUMN_METADATA, ENABLE_GEOPOLY,
ENABLE_MEMORY_MANAGEMENT, ENABLE_PREUPDATE_HOOK, ENABLE_RBU, ENABLE_SESSION, ENABLE_SNAPSHOT,
**ENABLE_STAT4**, ENABLE_UNLOCK_NOTIFY, LIKE_DOESNT_MATCH_BLOBS, SOUNDEX. Only in the installed
CLI: COMPILER=gcc-13.3.0, DQS=0, ENABLE_BYTECODE_VTAB, ENABLE_EXPLAIN_COMMENTS, ENABLE_FTS3/4,
ENABLE_PERCENTILE, ENABLE_STMTVTAB, ENABLE_UNKNOWN_SQL_FUNCTION, STRICT_SUBTYPE (the
`./configure` CLI also has HAVE_ISNAN). Surprises worth a sentence: the driver has STAT4 (the
planner can use `sqlite_stat4` after ANALYZE; the CLI cannot), and ENABLE_SNAPSHOT is compiled in
although the Go API does not expose `sqlite3_snapshot_*`. `bytecode()` is not in the driver.

## Experiment 1.2 one-insert

Run: `go run ./ch01-architecture-and-execution-path/experiments/1.2-one-insert` (about 1.8 s).
Evidence: `evidence/1.2-one-insert.txt`. Exit 0, last line `ONE INSERT OK`. On non-Linux it
prints the EXPLAIN listing, a `LINUX-ONLY:` line, and for each mode the base and the changed
pages (insert run in-process); darwin: cross-built only.

What it does, per journal mode (`delete` first, then `wal`), in `_work/1.2/<mode>/`:

1. Builds a base `t.db`: `CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT NOT NULL)` and row 1, with
   sqlitex defaults (synchronous FULL) in that journal mode. 2 pages.
2. Hashes every page through `sqlite_dbpage` (a fresh connection, closed after).
3. Re-runs itself as a child (`child-insert <dir> <mode>`, `runtime.LockOSThread`) under strace.
   The child opens the database, reads an empty marker file `marker-1`, runs
   `INSERT INTO t(id, v) VALUES(2, 'two')` (autocommit), reads `marker-2`, and in WAL mode reads
   the `-wal` file's frame headers and prints `frame N page=P commit_size=S` before closing.
   **Why marker files:** the CLI's stdout is buffered, so a printed marker does not land where
   the statement starts; opening a named file is a syscall at an exact point in both programs.
   Syscalls between the two marker opens, on files in the work directory, are the INSERT's I/O.
4. Hashes the pages again → changed pages.
5. Rebuilds the base and runs the pinned CLI under strace (no `-vfstrace`) with the same
   connection settings as `PRAGMA`s, `.read marker-1`, the INSERT, `.read marker-2`.
6. Twin check on the two marker segments (see correction 5 and the lines below).
7. Rebuilds the base and runs the CLI with `-vfstrace` under strace with the INSERT split into
   `BEGIN IMMEDIATE;` / `INSERT …;` / `COMMIT;` with markers 1–4 between them; prints each
   phase's VFS calls with the syscalls each one made, and checks that the three phases together
   make exactly the syscalls of the autocommit INSERT from step 5 and leave the same file bytes.
   This is what lets the table below attribute I/O to opcodes by measurement rather than by
   reading: BEGIN IMMEDIATE runs `OP_Transaction`, the INSERT inside the transaction runs
   `OP_Insert`, COMMIT runs `OP_AutoCommit` → `sqlite3VdbeHalt` → `vdbeCommit`, the same path
   the autocommit INSERT's `OP_Halt` takes.

xRandomness: the `./configure` CLI calls `xRandomness(44)` in the INSERT phase (rollback: the
journal header's random nonce) or the COMMIT phase (WAL: the salts) because that is its first use
of SQLite's PRNG; the installed CLI had already seeded it earlier, so it makes no such call here.
The experiment prints xRandomness only on `INFO:` lines so the EXACT output is the same for both
CLIs.

### EXPLAIN (EXACT, from the driver)

```
addr opcode         p1   p2   p3 p4    p5
0    Init            0   14    0        0
1    OpenWrite       0    2    0 2      0     cursor 0 on root page 2, 2 columns
2    SoftNull        2    0    0        0
3    String8         0    3    0 two    0
4    Integer         2    1    0        0     rowid 2 into r1
5    NotNull         1    7    0        0
6    NewRowid        0    1    0        0     (skipped: the id is given)
7    MustBeInt       1    0    0        0
8    HaltIfNull   1299    2    3 t.v    1     NOT NULL check
9    NotExists       0   11    1        0     seek rowid 2 (must not exist)
10   Halt         1555    2    0 t.id   2     PRIMARY KEY conflict exit
11   MakeRecord      2    2    4 DB     0     record (NULL, 'two') into r4
12   Insert          0    4    1 t      49
13   Halt            0    0    0        0     end: sqlite3VdbeHalt -> vdbeCommit
14   Transaction     0    1    1 0      1     write transaction, schema cookie 1
15   Goto            0    1    0        0
```

(The right-hand notes are mine, read from the operands; they are not in the output.)
Execution order: 0 → 14 → 15 → 1 … 13. `Insert` P5=49 = OPFLAG_NCHANGE|OPFLAG_LASTROWID|
OPFLAG_USESEEKRESULT (1+32+16).

### Rollback mode (journal_mode=delete) results

- Changed pages: `[1 2]` (page 1: change counter and version-valid-for; page 2: the table leaf).
- Go driver, 33 file syscalls between markers; CLI 31 (the two `fcntl F_SETFD`).
- Both database writes: `pwrite64 t.db off=0 len=4096` = (page 1 − 1) × 4096 and
  `off=4096` = (page 2 − 1) × 4096. Check: pages written = changed pages, each once. CONFIRMED.
- Syncs, in order, both builds: `t.db-journal`, `.` (the directory, new journal), `t.db-journal`,
  `t.db` — 4 syncs per committed INSERT at synchronous=FULL in rollback mode.
- Twin: writes identical (10 calls), syncs identical (4), every other call identical (31).
- File after the INSERT, Go vs CLI: byte-identical (sha256 47bae5ed0c8207f2…).

### WAL mode (the platform default) results

- Changed pages: `[2]` only (page 1 does not change in WAL mode for this INSERT).
- Go 18 calls, CLI 17. Zero writes to `t.db`. WAL writes: header `t.db-wal off=0 len=32` (new WAL
  after the base was checkpointed and deleted at close), sync `t.db-wal`, directory sync (new
  file), frame header `off=32 len=24`, page `off=56 len=4096`, sync `t.db-wal` (the commit).
  Child read back: `frame 1 page=2 commit_size=2`. Check CONFIRMED.
- Syncs: `t.db-wal`, `.`, `t.db-wal` — 3, of which only the last is the commit itself; the
  first two happen because this is the first write to a fresh WAL.
- Twin: writes identical (3), syncs identical (3), every other call identical (17).
- File after close, Go vs CLI: byte-identical (sha256 6b96a8a5533900b8…).

### Opcode → layer → VFS call → syscall (from the 1.2 run; C CLI, VFS calls from `-vfstrace`)

Rollback mode:

| Opcode (statement) | Layer path (source) | VFS call | Syscall(s) |
| --- | --- | --- | --- |
| OP_Transaction (BEGIN IMMEDIATE; in the autocommit INSERT, address 14) | VDBE → `sqlite3BtreeBeginTrans` → pager `sqlite3PagerSharedLock` | `xLock(t.db,SHARED)` | `fcntl F_SETLK F_RDLCK start=1073741824 len=1` (PENDING byte), `F_RDLCK start=1073741826 len=510` (shared range), `F_UNLCK start=1073741824 len=1` |
| 〃 | pager checks for a hot journal | `xAccess("t.db-journal")` → out=0 | (none shown; stat-type calls not traced) |
| 〃 | pager checks whether its cache is still valid | `xRead(t.db,n=16,ofst=24)` | `pread64 t.db off=24 len=16` |
| 〃 | pager checks for a WAL file | `xAccess("t.db-wal")` → out=0 | (none traced) |
| 〃 | | `xFileSize(t.db)` → 8192 | (fstat, not traced) |
| 〃 | `sqlite3PagerBegin` (write) | `xLock(t.db,RESERVED)` | `fcntl F_WRLCK start=1073741825 len=1` (RESERVED byte) |
| OP_Insert | VDBE → `sqlite3BtreeInsert` → `sqlite3PagerWrite` → `pager_write` | `xRead(t.db,n=4096,ofst=4096)` | `pread64 t.db off=4096 len=4096` (page 2) |
| 〃 | `pager_open_journal` | `xFileControl(HAS_MOVED)`, `xOpen(t.db-journal)`, `xDeviceCharacteristics` | `open t.db-journal` |
| 〃 | journal header | `xWrite(t.db-journal,n=512,ofst=0)` | `pwrite64 off=0 len=512` |
| 〃 | page 2's original image: page number, page, checksum | `xWrite` ×3 | `pwrite64 off=512 len=4`, `off=516 len=4096`, `off=4612 len=4` |
| OP_Halt (autocommit) / OP_AutoCommit (COMMIT) | `sqlite3VdbeHalt` → `vdbeCommit` → `sqlite3PagerExclusiveLock` | `xLock(t.db,EXCLUSIVE)` | `fcntl F_WRLCK start=1073741824 len=1`, `F_WRLCK start=1073741826 len=510` |
| 〃 | `sqlite3BtreeCommitPhaseOne` → `sqlite3PagerCommitPhaseOne` → `pager_incr_changecounter` journals page 1 | `xWrite` ×3 | `pwrite64 t.db-journal off=4616 len=4`, `off=4620 len=4096`, `off=8716 len=4` |
| 〃 | `syncJournal`: stale-header check | `xRead(t.db-journal,n=8,ofst=9216)` → SHORT_READ | `pread64 off=9216 len=8` |
| 〃 | `syncJournal`: sync records | `xSync(t.db-journal,FULL)` | `fsync t.db-journal`, then `open .`, `fsync .`, `close .` (directory sync for the new journal) |
| 〃 | `syncJournal`: record count into the header | `xWrite(t.db-journal,n=12,ofst=0)` | `pwrite64 off=0 len=12` |
| 〃 | `syncJournal`: sync header | `xSync(t.db-journal,FULL)` | `fsync t.db-journal` |
| 〃 | `pager_write_pagelist` | `xWrite(t.db,n=4096,ofst=0)`, `xWrite(t.db,n=4096,ofst=4096)` | `pwrite64 t.db off=0 len=4096`, `off=4096 len=4096` |
| 〃 | `sqlite3PagerSync` | `xFileControl(SYNC)` → NOTFOUND, `xSync(t.db,FULL)` | `fsync t.db` |
| 〃 | `sqlite3BtreeCommitPhaseTwo` → `pager_end_transaction` (DELETE mode) | `xClose(t.db-journal)`, `xDelete("t.db-journal")` | `close`, `unlink t.db-journal` |
| 〃 | unlock | `xFileControl(COMMIT_PHASETWO)`, `xUnlock(t.db,SHARED)`, `xUnlock(t.db,NONE)` | `fcntl F_RDLCK start=1073741826 len=510`, `F_UNLCK start=1073741824 len=2`, `F_UNLCK start=0 len=0` |

The commit point in rollback mode is the `unlink` of the journal: before it a crash leaves a hot
journal that rolls the change back; after it the new pages stand.

WAL mode:

| Opcode (statement) | Layer path | VFS call | Syscall(s) |
| --- | --- | --- | --- |
| OP_Transaction (BEGIN IMMEDIATE) | `sqlite3BtreeBeginTrans` → pager → `walTryBeginRead` | `xShmBarrier`, `xShmLock(READ0, LOCK SHARED)` | `fcntl t.db-shm F_RDLCK start=123 len=1` |
| 〃 | | `xShmBarrier`, `xFileSize(t.db)` | — |
| 〃 | `sqlite3WalBeginWriteTransaction` | `xShmLock(WRITE, LOCK EXCLUSIVE)` | `fcntl t.db-shm F_WRLCK start=120 len=1` |
| OP_Insert | `sqlite3BtreeInsert` → `sqlite3PagerWrite` | `xRead(t.db,n=4096,ofst=4096)` | `pread64 t.db off=4096 len=4096` (no journal in WAL mode) |
| OP_Halt / OP_AutoCommit | `vdbeCommit` → `sqlite3PagerCommitPhaseOne` → `pagerWalFrames` → `sqlite3WalFrames` → `walRestartLog` | `xShmLock(READ0, UNLOCK SHARED)`, `xShmLock(READ1, LOCK EXCLUSIVE)`, `xShmLock(READ1, UNLOCK EXCLUSIVE)`, `xShmLock(READ1, LOCK SHARED)`, `xShmBarrier` | `fcntl t.db-shm F_UNLCK 123`, `F_WRLCK 124`, `F_UNLCK 124`, `F_RDLCK 124` |
| 〃 | WAL header (first frame of a new WAL) | `xWrite(t.db-wal,n=32,ofst=0)` | `pwrite64 t.db-wal off=0 len=32` |
| 〃 | sync the new header | `xSync(t.db-wal,FULL)` | `fsync t.db-wal`, `open .`, `fsync .`, `close .` |
| 〃 | frame 1 = page 2, commit frame | `xWrite(t.db-wal,n=24,ofst=32)`, `xWrite(t.db-wal,n=4096,ofst=56)` | `pwrite64 off=32 len=24`, `off=56 len=4096` |
| 〃 | commit sync (synchronous=FULL) | `xSync(t.db-wal,FULL)` | `fsync t.db-wal` |
| 〃 | release the write lock and the reader slot | `xShmBarrier`, `xShmLock(WRITE, UNLOCK EXCLUSIVE)`, `xFileControl(COMMIT_PHASETWO)`, `xShmLock(READ1, UNLOCK SHARED)` | `fcntl t.db-shm F_UNLCK start=120`, `F_UNLCK start=124` |

Lock byte facts used above: rollback locks live on `t.db` at 1073741824 (0x40000000, PENDING),
+1 (RESERVED), +2..+511 (SHARED range, 510 bytes); WAL locks live on `t.db-shm` at 120 (WRITE),
121 (CKPT), 122 (RECOVER), 123 + i (READ i).

## Experiment 1.3 recipe-rebuild (rung R0)

Run: `go run ./ch01-architecture-and-execution-path/experiments/1.3-recipe-rebuild` (about 9–11 s
including the build; most of it is the two waits for a wall-clock second boundary and the WAL
builds). Evidence: `evidence/1.3-recipe-rebuild.txt`. Exit 0, last line `RECIPE REBUILD OK`.
Portable (no strace).

What it printed:

- The recipe line and the 31 step summaries.
- R0: two builds, `run-1/recipe.db` and `run-2/elsewhere/another-name.db`: both 270 pages, sha256
  `365695c2ef73fee65082304bd6a6389161e329c276b0ea0e7001d9850cb4fc89`. CONFIRMED equal.
- The 31 per-step hashes (state after step k, at rest); the second run's list is identical; all
  31 are distinct; `recipe.StepHashes` (cache) equals it; a database built to step 12 is looked
  up as `step 12 (exact bytes)`.
- Rollback-mode recipe: also rebuilds byte for byte at every step; sha256
  `0b5b4111fb9946325ad7ffa859b68b1394b04f824a274b0d770697bd16b71d57`. WAL vs rollback build of
  the same recipe: only page 1 differs, bytes 18, 19 (file format read/write version: 2 = WAL,
  1 = rollback) and 27, 95 (change counter).
- Variants (base = recipe to step 6, then one extra transaction in a new session; each run into
  fresh files):
  1. `random()` in a value: differ, first differing page 59, 1 page. CONFIRMED.
  2. `current_timestamp`: same second → identical; next second → differ, page 59. See correction 4.
  3. unordered `INSERT … SELECT`: identical when run twice (FINDING, correction 3); differ (page
     29) when run 2 scans in reverse; identical again with `ORDER BY`.
  4. batches: WAL + secure_delete=ON identical (FINDING, correction 2); WAL + secure_delete=OFF
     differ in pages 24 and 28, unused bytes only; rollback differ in the change counter only.
  5. rowid past 9223372036854775807: differ, page 29 (the new table's leaf). CONFIRMED.
  6. (not in the outline) connection sessions in WAL mode: differ only at bytes 27 and 95 of
     page 1; rollback: identical. Correction 1.
- Hashes of the random variants vary run to run and are on `INFO:` lines only; the "first
  differing page" lines are stable.

## Working scripts

`agent_work/ch01/recipe-probe/` (Go, `go run ./agent_work/ch01/recipe-probe`): builds the default
recipe in both modes and compares the canonical build with one-connection and one-session-per-step
builds under six connection settings. It found correction 1 and backs correction 7. Indexed in
`agent_work/ch01/INDEX.md`.

## Not done / for later chunks

- No chapter prose, no comments (seat rule).
- The 1.3 adversarial list (50 runs, different cache sizes, machine load, a sixth source) is
  Neon's; variant 6 already answers "a sixth source the outline missed".
- `recipe` has no option for `synchronous` or other connection settings: callers who need them
  open their own handle with modified `SQLiteOptions()` and call `ApplyStep`. Settings other than
  `secure_delete` did not change the at-rest bytes in the probe.

## Repair round 1 addendum (2026-11-20)

Full table and commands: `agent_work/ch01/REPAIR-REPORT.md`. Facts Neon needs:

### Correction 1, restated (review finding 4, DET-06)
- In WAL mode the change counter (bytes 24–27, copied to 92–95) is written only when a frame
  for page 1 is written: `if( pList->pgno==1 ) pager_write_changecounter(pList);`
  (pager.c:3264). The value is `dbFileVers + 1` (pager.c:3135). `dbFileVers` is refreshed only
  when page 1 is read through `readDbPage` (pager.c:3109-3110): a new connection, or page 1
  evicted from the cache and read again.
- So "the WAL counter counts connection sessions" is too broad. It counts sessions (and page-1
  re-reads) *whose commits write page 1*. Every recipe step writes page 1 (`PRAGMA
  user_version = k`), which is why the recipe shows one increment per session.
- Shown in 1.3 (new line under variant 6): recipe to step 6, then 3 commits of
  `UPDATE accounts SET balance = balance + 1 WHERE id = 1`, one connection per commit vs one
  connection for all three: `identical`. Also visible in 1.2: in WAL mode the INSERT changes only
  page 2 (new check line).
- Eviction also counts: with `cache_size=0` (and only 0 among 0, 1, 10, -2000) a WAL build
  differs from the canonical one in the counter alone, from step 0 on (step 0 counter 3 instead
  of 2), deterministically over 3 runs (`agent_work/ch01/repair-probe`, adversarial DET-02 run 2).
  In the session that creates the file, page 1 is re-read between the identity-pragma commit and
  the step-0 commit. Before the foundation repair (identity pragmas ran in rollback mode before
  the switch to WAL) DET-02 held. So `cache_size` is part of the recipe contract. The old note
  "settings other than secure_delete did not change the at-rest bytes" holds only for the values
  the probe tried.

### Foundation merge changed the bytes (no ch01 code involved)
- After merging main, sqlitex.Open on a new file runs layout pragmas, then `journal_mode`, then
  `application_id`/`user_version`. In WAL this removes one rollback-mode commit, so the WAL
  recipe's change counter is 2 lower at every step (byte 27: 0x20 instead of 0x22 at step 30;
  no other byte differs). All WAL recipe hashes changed: step 30 is now
  `24aa158fdf713808062d18b3c753ebe58c7de8edeceffbe90153828f817e5b74` (was `365695c2…`), step 0
  `7470585af706eb3de499f126d74bdc98396e1e9853ee11643e2e85f61ef0cdb9` (was `bc7ece7b…`).
  1.2 WAL base `5de4f3aefd26bcea` (was `d0cb3c980847e49e`), WAL after-INSERT `1afce8154bf3124c`
  (was `6b96a8a5533900b8`). Rollback-mode hashes are unchanged (`0b5b4111…`, `47bae5ed…`).
- The old step-hash cache key did not change across that foundation change, so an old cache
  would have answered with stale hashes. The cache is now checked (below).

### secure_delete must be exactly ON (DET-05d, DET-08a)
- btree.c:6865-6876: a freed page is zeroed only under `BTS_SECURE_DELETE` (full ON). FAST sets
  `BTS_OVERWRITE` only (btree.c:3186-3191), which zeroes freed cell space inside b-tree pages
  (btree.c:1992, 2282, 8372) but not freed pages.
- 1.3 new line under variant 4: steps 0–6, one transaction per step, FAST vs ON:
  `differ: first differing page 29, 3 page(s) differ [29 36 37]` — page 29: 4079 bytes (freelist
  leaf), page 36: 4067 bytes (freelist trunk, bytes after the leaf list), page 37: 4078 bytes
  (freelist leaf). All in unused space.
- FAST batching in that small variant (steps 2+3 and 4+5 merged) came out *identical*, so 1.3
  does not show FAST batching variance; DET-05d shows it at full scale (30 steps, 1 tx vs 31 tx:
  differ, 2 pages, first 29). I did not keep that line in 1.3 to avoid showing a misleading
  "identical".

### Rolled-back SAVEPOINT leaves bytes behind (DET-08b) — new source of variation, rule 7
- 1.3 new section, variant 7: recipe to step 6, one transaction committing one INSERT; run 2
  first inserts 10 documents of 12000 bytes (each 0xAB) inside `SAVEPOINT`, then
  `ROLLBACK TO` + `RELEASE`, then the same INSERT.
  - WAL, secure_delete=ON: differ, pages [1 29 37]; page 1 bytes 27 and 95 (counter);
    pages 29 and 37 are freelist leaf pages, 4092 and 4093 bytes differ.
  - rollback, secure_delete=ON: differ, pages [29 37], freelist leaves only.
- Why: a freelist leaf reused by an allocation is fetched with `PAGER_GET_NOCONTENT`
  (btree.c:6740), so its old content is not journaled; `ROLLBACK TO` puts the page back on the
  freelist but cannot restore bytes it never saved. In WAL the aborted allocation also dirtied
  page 1 (freelist count), so the commit writes the counter.
- A whole-transaction rollback (failed `ApplyStep`) leaves the file byte-identical (attack API-05).

### Twin-check limit: compile options (TWIN-10, DET-08c)
- 1.2 new section "a limit of the twin check": a 100-row table with an index, built twice by the
  Go driver (byte-identical); then `ANALYZE` in the driver and in the CLI.
  `ENABLE_STAT4 compiled in: Go driver 1, CLI 0`; driver file gets `sqlite_stat1` and
  `sqlite_stat4`, CLI file only `sqlite_stat1`; files differ. The twin claim holds for statements
  whose behaviour does not depend on compile options that differ between the two builds; 1.1's
  "file-format and I/O options" list does not include ENABLE_STAT4, but STAT4 does change file
  contents (via ANALYZE).

### Other sources, statement only (not demonstrated in an experiment)
- Pool churn (DET-08d): with `sqlitex.DefaultOptions()` (SingleConnection=false) a concurrent read
  that forces a second pooled connection changes the WAL counter (page 1 bytes 27, 95). Same
  mechanism as variant 6 (a second connection has its own `dbFileVers`). The recipe already sets
  `SingleConnection=true`; the rule is "build on one connection per session".
- Time zone (DET-08e): a fixed timestamp literal with the `'localtime'` modifier under TZ=UTC vs
  TZ=America/New_York gives different bytes (page 59). Same class as `current_timestamp`: no
  environment-dependent SQL functions/modifiers.
- Attack output for both: `agent_work/ch01/adversarial/results.txt`.

### 1.2 behaviour changes
- WAL write check now compares the full list: `0+32` (WAL header of the new -wal) then each
  frame's `24`-byte header and `4096`-byte page; nothing else allowed.
- The portable path (no strace) no longer looks for the CLI, closes with error check, checks the
  database is at rest, and checks the changed pages ([1 2] rollback, [2] WAL). New check lines
  print on Linux too (evidence lines 28 and 182).
- Normalized syscalls now carry `= ERRNO` when a call failed and `= N` when a read/write
  transferred fewer bytes than asked; `read`/`write` show `len=`. Visible change in evidence:
  `pread64 t.db-journal off=9216 len=8 = 0` (the hot-journal probe reads past end of file).
- The twin check now also requires at least one sync, a file that differs from the base, and
  `CheckAtRest` after the Go child, the CLI run and the split run.
- On darwin the `LINUX-ONLY:` line now reads "…the twin check, its ANALYZE limit and the
  measured call path". darwin: cross-built only.

### Recipe API changes
- `Step(k)` and `BuildEach` (hence `Build`) call `Validate` first.
- New exported `ChangeCountersAgree(image []byte) bool` (bytes 24–27 equal 92–95). `MatchStep`
  returns Step -1 for a masked-only match whose two counters disagree.
- Cache file format v2 (`FormatVersion = 2`): hash lines, then `checksum <sha256>` over the cache
  key, file and connection settings, every planned statement (SQL and args) and every hash line.
  Before trusting a cache, `StepHashes` checks the checksum and rebuilds step 0 in a temp dir and
  compares its hash (a few ms). A cache without checksum, with a wrong checksum, or with a wrong
  step 0 is rebuilt. Not detected: a cache whose later lines were changed *and* whose checksum
  was recomputed (deliberate forgery) — the checksum is for corruption and stale foundations.
- `writeHashCache` removes the temp file when the rename fails.
