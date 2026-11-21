# Chunk 6 build notes (plain data for the prose pass)

Builder: Claude Opus 5.5, branch `chunk/ch06`, 2026-11-20. Machine: linux/amd64, Go 1.26.8, ext4.
Pinned tree at `$SQLITE_SRC` (3.53.4). All four experiments were also run inside the Debian `lab`
container (`docker compose run --rm -T lab go run ./ch06-locking-and-concurrency/experiments/<name>`):
output identical to the host's apart from `INFO:` lines (pids), including the `/proc/locks` listing
(the container's overlay filesystem and pid namespace give the same lines).
darwin: cross-built and cross-vetted only (`agent_work/verify/crossbuild.sh` → `CROSSBUILD OK`), never run.

`go run ./cmd/course check spans ch06` passes (36 spans after the repair round); `check sources ch06` passes (`os_unix.c`,
`os.h`); `check evidence ch06 -runs 2` passes for all four experiments.

## Corrections to the outline

1. **6.1: what happens after the stray close is a lost transaction, and which one depends on the
   mode.** The outline's claim (second process refused before the stray close, admitted after it,
   while the first still believes it holds the lock) is confirmed in both modes. The experiment
   goes one step further and commits both transactions; the outcome is exact and repeatable:
   - **Rollback mode:** the second process's COMMIT returns `SQLITE_OK`; the holder's COMMIT
     writes its pages and then returns `SQLITE_IOERR_DELETE_NOENT` (5898), because both processes
     used the same `-journal` path and the second one already deleted it (`unixDelete`,
     os_unix.c:6833-6869, ENOENT → 6847). The final file passes `integrity_check` and holds only
     the holder's row: **the commit that returned `SQLITE_OK` is silently lost**, and the
     transaction that reported an error is the one that persisted.
   - **WAL mode:** the second process's COMMIT returns `SQLITE_OK`; the holder's COMMIT returns
     `SQLITE_PROTOCOL` (15) after about 10 s. Its `walRestartLog` (wal.c:3875-3915) drops read
     lock 0 and calls `walTryBeginRead` with `useWal=1`, which keeps the holder's stale cached
     header; every attempt finds the shared header changed (memcmp at wal.c:3261-3265) and
     retries; after `WAL_RETRY_PROTOCOL_LIMIT` (100, wal.c:2963) retries it returns
     `SQLITE_PROTOCOL` (wal.c:3053-3056), with sleeps growing to 323 ms each (the comment at
     wal.c:3040-3048 says "less than 10 seconds" in total). The final file passes
     `integrity_check` and holds only the second process's row: the holder's transaction is
     lost, but the holder was told. The source comment calls this case "some other process has
     gone rogue" (wal.c:2992-2997).
     **Repair round correction (adversarial LOCK-06, LOCK-04b): the SQLITE_PROTOCOL outcome is
     only one of three WAL outcomes, and the other two are silent.** `walRestartLog` does
     anything only when the holder reads through `WAL_READ_LOCK(0)` (`if( pWal->readLock==0 )`,
     wal.c:3879), i.e. its transaction started on an empty or fully checkpointed WAL — the case
     of 6.1 B.
     - **6.1 C, WAL already holding a commit no checkpoint copied** (`wal_autocheckpoint=0`, the
       holder commits a `seed` row first): the holder reads through `WAL_READ_LOCK(1)` (byte 124,
       seen by F_GETLK and `/proc/locks`). Stray close of `-shm` only; the second process is
       admitted and commits (`SQLITE_OK`); the holder's COMMIT **also returns `SQLITE_OK`** at
       once — no `walRestartLog`, no header re-check, its frames are written after its own stale
       `mxFrame`, over the second process's frames. The second process, still open, then reads
       `2 row(s): seed,holder`: its own committed row is gone. Final file: `integrity_check ok;
       2 row(s): seed,holder`. **A commit that returned SQLITE_OK is lost and neither process was
       told.**
     - **6.1 D, the second process exits before the holder commits** (stray close of `db` and
       `-shm`, empty WAL as in B): the second process commits (`SQLITE_OK`) and closes; seeing no
       other process's locks it closes as the last connection (`sqlite3WalClose`: EXCLUSIVE lock on
       the database file wal.c:2529, checkpoint, `walIndexClose(pWal, isDelete)` 2559 and delete of
       the WAL 2563, whose result is ignored) — afterwards `lab.db-wal is gone and lab.db-shm is
       gone`. The holder's COMMIT returns `SQLITE_OK`; afterwards `lab.db-wal` is still gone (the
       holder wrote into the deleted file it still had open). Final file: `integrity_check ok;
       1 row(s): holder`. Again a commit reported as `SQLITE_OK` is lost silently. (Mechanism of
       the holder's side read from the source, not traced: its stale mapping shows the WAL fully
       backfilled, so `walRestartLog` restarts the log instead of looping, and its own close later
       checkpoints from the deleted WAL into the database file.) The adversarial LOCK-04b used the
       C `sqlite3` CLI as the second process — a new process per statement batch, so it always
       closes first — and got the same result.
     So the chapter must not say "in WAL mode the holder is told": in WAL mode a broken lock can
     lose either process's commit, and in two of the three scripted cases nobody gets an error.
2. **6.1: "closing any descriptor to the file releases them" is per file.** POSIX locks are per
   (process, inode). In WAL mode the writer lock lives on the `-shm` file (byte 120), so a stray
   open+close of the **database file** drops only the holder's database-file lock (the SHARED
   read lock) and the second writer is **still refused**; only a stray close of the **`-shm`**
   file lets it in. The chapter should say: closing any descriptor of a file releases all of the
   process's locks on *that* file; in WAL mode the writer exclusion is on the `-shm` file.
3. **6.2: a busy timeout turns the failure into a wait only for a transaction that has not read
   yet.** Confirmed: `BEGIN IMMEDIATE`, and a `BEGIN DEFERRED` whose first statement is a write,
   wait for the lock and then succeed. **Not** true for a deferred transaction that has already
   read: its first write returns `SQLITE_BUSY` (5) at once even with `busy_timeout=120000`
   (part C3), because `btreeBeginTrans` retries through the busy handler only while
   `inTransaction == TRANS_NONE` (btree.c:3740-3741); after the other writer commits, the same
   write returns `SQLITE_BUSY_SNAPSHOT` (517) at once (wal.c:3734-3737). The outline's sentence
   "a busy timeout turns that failure into a wait" needs this qualifier. The 517 half of the claim
   is confirmed exactly as written.
4. **Span fix: `unixLock` (outline: `os_unix.c` 1843–2095).** Line 1843 is the second line of the
   function's doc comment (the comment opens at 1842); the function itself starts at 1866.
   SPANS.txt lists `1842 1865` (the doc comment with the transition chart) and `1866 2095`
   (`unixLock`). Every other span in the reading path was exact (start line holds the symbol, end
   line is the closing brace / end of the block).
5. **6.4 surprise (not a correction, but the chapter's model must allow it): a checkpoint's
   `nBackfill` does not mean the database file holds that state.** After the capture's read
   transaction pinned frame 61 and the writer committed step 9, a PASSIVE checkpoint reported
   `checkpointed=61`, yet the database file's page 1 was still recipe step 5's page 1 (change
   counter 7, user_version 5, 51 pages — read from `_work/6.4/copy.db`). The checkpoint iterator
   yields each page once, at its newest frame in the WAL (`walIteratorNext`, wal.c:1764-1792), and
   `walCheckpoint` skips that page when its newest frame is beyond `mxSafeFrame` (wal.c:2312-2314).
   Page 1 was rewritten by step 9 (frame > 61), so no version of page 1 was copied. Between
   checkpoints the database file is a mixture of states that only the WAL makes whole. This is
   why the 6.3 main-file-only copy (no reader, complete checkpoint) is a consistent older state,
   and why 6.4's database-file copy alone is torn while the file + WAL pair replays correctly.

6. **Repair round: `PRAGMA locking_mode=EXCLUSIVE` is outside "the writer never blocks
   readers"** (adversarial BUSY-05b). A WAL connection in `locking_mode=EXCLUSIVE` takes the
   database file's EXCLUSIVE lock at its first write (`sqlite3PagerBegin`, pager.c:5982-5988) and
   keeps it; a second process then cannot even open (`PRAGMA page_count: database is locked (5)`
   at the SQL role's open). No experiment of this chapter uses it; the claim is for the default
   `locking_mode=NORMAL` and the chapter should say so once.
7. **Repair round: a read transaction holds back a WAL restart only if it is not on
   `WAL_READ_LOCK(0)`** (adversarial SNAP-04, confirmed by `agent_work/ch06/probes/snap04-restart`).
   A read transaction that starts on a fully checkpointed WAL takes `WAL_READ_LOCK(0)` (it reads
   the database file only). The next writer's `walRestartLog` takes `WAL_READ_LOCK(1..4)`
   exclusively, succeeds, and restarts the WAL under the open reader: probe output `after step 6
   (autocheckpoint 20) mxFrame=22 … nBackfill=22`, the capture's `user-version` 6, `-shm` byte 123
   read-locked, then `after step 7 mxFrame=16 salt1=…dd nBackfill=0` (salt-1 +1). That is safe for
   the reader (it never looks at the WAL), and while it lasts no checkpoint can backfill, because
   backfill needs `WAL_READ_LOCK(0)` exclusively (wal.c:2259-2260) — so the reader still pins the
   database file. It matters for any later "hold a read mark so the WAL cannot restart" design
   (OUTLINE R6 `internal/walship`): the mark must be taken while the WAL has un-checkpointed frames.

Everything else in the chapter-6 section was confirmed; details per experiment below. The two
[unprobed] parts are now probed: 6.1's hazard (confirmed, extended by corrections 1-2) and 6.2's
result codes (confirmed, qualified by correction 3); 6.4's "under a writer" part is confirmed.

## Shared packages

### Changes to existing packages (backward compatible, with tests)

- `internal/sqlitex/codes.go` (new file): constants `CodeOK` 0, `CodeError` 1, `CodeReadOnly` 8,
  `CodeIOErr` 10, `CodeCorrupt` 11, `CodeCantOpen` 14, `CodeProtocol` 15, `CodeNotADB` 26,
  `CodeLockedSharedCache` 262, `CodeLockedVTab` 518, `CodeBusyRecovery` 261, `CodeBusySnapshot`
  517, `CodeBusyTimeout` 773, `CodeIOErrShortRead` 522, `CodeIOErrWrite` 778, `CodeIOErrLock`
  3850, `CodeIOErrShmLock` 5130, `CodeIOErrDeleteNoEnt` 5898 (values from sqlite.h.in 449-546).
  `ResultCodeName(code)`: the `SQLITE_…` name; an unknown extended code of a known primary code
  prints `SQLITE_IOERR+extended(N)`; anything else `SQLITE_code_N`. The driver's own
  `sqlite.ErrorCodeString` has no entry for `SQLITE_BUSY_SNAPSHOT`, hence the table.
- `internal/sqlitex/display.go` (new file): `DisplayPath(path)` — the path relative to the repo
  root with forward slashes, or only its base name if it is outside the repo. Experiments print
  their work dir with it so no evidence file carries a machine path (earlier chapters each had a
  private `displayPath`).
- `internal/sqlitex/codes_test.go`: names, the 517/261/5898/778 values, `DisplayPath` inside and
  outside the repo.
- No other shared package changed (`recipe`, `capture`, `equal`, `walfile`, `dbfile`, `trace`,
  `repocheck` untouched). `go.mod` untouched.

### New package `internal/procs` (about 1,840 lines of code, 610 of tests; no comments)

What it is for: a multi-process lab where every interleaving is scripted. The experiment binary
re-runs itself as named child processes; the parent sends each child numbered steps and waits for
each answer, so "who holds which lock when" is part of the script and never a matter of timing.

**Protocol (`protocol.go`).**
- A child is the same executable started as `<exe> procs-child <role> <role args…>`
  (`ChildMarker`). Requests arrive on file descriptor 3 and answers go out on descriptor 4, one
  JSON object per line; the child's stdout and stderr stay free (they are captured by the parent
  for error reports).
- `Request{Step, Action, Args}`; `Result{Step, Action, Code, Error, Value}`. `Code` is 0 on
  success, the SQLite **extended** result code when the action's error wraps a driver error
  (`sqlitex.ErrorCode`), and `CodeNotSQLite` (-1) for any other error. `OK()`, `CodeName()`,
  `Outcome()` (`SQLITE_OK`, `SQLITE_OK <value>`, `SQLITE_BUSY (5)`, or `ERROR <text>`).
- `Role func(ctx, args) (*Actor, error)`; `Actor{Actions map[string]Handler, Close}`;
  `Handler func(ctx, args) (string, error)`. `ResultFor` turns a handler's return into a Result.
- `Serve(roles) bool` — call first in `main` (and in `TestMain`): when the process was started as
  a child it runs `RunChild` and exits (0 after a clean `exit`, 2 on error, 3 if the pipes are
  missing); otherwise it returns false.
- `RunChild(ctx, roles, role, args, requests, replies)`: starts the role, sends a `ready` answer
  (step 0) carrying its pid, then answers requests in order. `exit` closes the actor (its error is
  the answer's error and the exit status). Unknown action → an error Result, the child keeps
  going. Malformed request line, unknown role, role that fails to start, or the request pipe
  closing before `exit` (the parent died) → the child exits with an error.

**Parent side (`parent.go`).**
- `StartChild(name, role, args…)` re-runs `os.Executable()` (works under `go run` and `go test`);
  `StartCommand` takes an explicit executable. It waits for `ready` and checks that the pid in it
  is the process it started. `Child{Name, Role, Pid, Timeout}` (default `DefaultTimeout` 30 s).
- `child.Do(step, action, args…)` = `Start` + `Wait`. `Start` sends one request and returns a
  `*Call`; only one request may be outstanding per child (`ErrBusyChild`). `call.Wait()` /
  `WaitFor(d)`; `call.Answered()` polls without blocking; **repair round:**
  `call.AnsweredWithin(d)` waits up to d for the answer and, unlike `WaitFor`, does not kill the
  child when d passes (used by `Lab.StillWaiting`).
- Failures are loud: the answer channel closing → `ErrChildDied` with the exit status and the
  last 8 KiB of the child's output; no answer within the timeout → the child is killed and
  `ErrTimeout`; an answer for another step → `ErrStepMismatch`; using a stopped child →
  `ErrStopped`. `Stop()` sends `exit`, waits for the answer and the process exit, and reports a
  close error or a non-zero exit; stopping with a request outstanding kills the child and
  returns `ErrBusyChild`. `Kill()`. **Repair round:** `Stop` kills the child when the answer to
  `exit` is any error (not only a timeout), and the reply reader stops once the child is killed
  (it no longer blocks forever on an answer nobody reads).

**Script and Lab (`script.go`, `lab.go`).**
- `Script` numbers steps globally (step 1, 2, … across all children), spawns named children
  (`Spawn`, duplicate names refused), maps a pid back to a name (`NameOfPid`), prints one
  transcript line per answered step: `step %2d  <child, 10 wide> <action and args, 36 wide> ->
  <outcome>`, and stops every child in reverse order on `Close` (errors joined).
- `Lab` wraps a Script for experiments: `Check(ok, claim)` prints `check ok: …` / `check FAILED:
  …`; `Report(title)` prints `<TITLE> OK` or `<TITLE> FAILED: N claim(s) did not hold` plus the
  list and exits 1; `Fatal` kills every child, prints `FATAL …`, exits 1. `Do` (any outcome is
  returned), `Must` (fatal unless SQLITE_OK), `Begin` (sends, prints `.. sent, not waiting for
  the answer`), `StillWaiting(call)` (prints `.. no answer after 250ms` or `.. already answered`, returns
  true while unanswered), `Finish(call)`, `StopChildren`, `SpawnSQL(name, SQLOptions)`,
  `Locks(label, path, ranges)` (prints the F_GETLK probe and, on Linux, the `/proc/locks` lines).
  **Repair round:** `StillWaiting` now waits up to `StillWaitingWindow` (250 ms) for an answer
  (`call.AnsweredWithin`) and prints `.. no answer after 250ms` or `.. already answered`; an
  action that answers at once (e.g. `BEGIN IMMEDIATE` with `busy_timeout=0` → BUSY) is reported
  as answered, so the check can fail. `Locks` returns a `LockView{Ranges, Proc, ProcListed}`:
  the F_GETLK states plus, on Linux, the children's `/proc/locks` entries on that file;
  `view.ProcHolders(rangeName)` maps pid → `READ`/`WRITE` for every entry covering the whole
  range (a merged or to-EOF entry counts).

**Roles (`sqlrole.go`).** `Roles` = `{"sql": SQLRole, "copier": CopierRole}`.
- `SQLOptions{Path, JournalMode, BusyTimeout, WALAutocheckpoint, PagesOut}` and `Args()`. The SQL
  role opens the file with the recipe's options (page size, encoding, application id, secure
  delete, journal mode as given) plus the given busy timeout and `wal_autocheckpoint`, with
  `_txlock` left deferred, and holds **one** `*sql.Conn` for its whole life so every action runs
  on the same SQLite connection.
- Actions: `exec SQL…`, `query SQL…` (first column of the first row, `NULL` for null), `begin
  deferred|immediate|exclusive` (raw `BEGIN …`), `commit`, `rollback`, `timeout MS` (`PRAGMA
  busy_timeout`), `checkpoint MODE` (`busy=… log=… checkpointed=…`), `recipe-exec K` (step K's
  statements inside the current transaction), `recipe-step K` (`BEGIN IMMEDIATE`, step K, `COMMIT`;
  rolls back on error), `user-version` (the recipe step the connection sees), `stray-close
  db|-wal|-shm|-journal` (**the hazard**: `os.Open` + `Close` of that file inside the process that
  holds the connection), `dbpage-write FIRST LAST` (reads pages FIRST..LAST with `SELECT data FROM
  sqlite_dbpage WHERE pgno = ?` on the held connection — so inside its open transaction — and
  writes them at their offsets in the `-pages-out` file, then fsyncs it).
- Copier role (`-db`, `-out`; holds no SQLite connection): `copy SOURCE NAME` (whole file; a
  missing source answers `missing` and removes an old copy), `copy-range SOURCE NAME START
  END|end` (plain `ReadAt` of that byte range of the live file, written at the same offset of
  the target, so a copy can be taken in pieces at scripted moments). **Repair round:** if the
  file shrank between its size check and the read, only the bytes actually read are written and
  the answer `bytes START-(START+n)` reports them. Targets must be plain names
  inside `-out`.

**Lock observation (`locks.go`, `lockprobe*.go`, `proclocks*.go`).**
- Byte ranges from the source: `DatabaseLockRanges()` = PENDING 0x40000000 (1 byte), RESERVED
  0x40000001 (1), SHARED 0x40000002 (510) (os.h:160-166); `ShmLockRanges()` = WAL_WRITE_LOCK
  120, WAL_CKPT_LOCK 121, WAL_RECOVER_LOCK 122, WAL_READ_LOCK(0..4) 123-127, DMS 128
  (os_unix.c:4602-4603, wal.c:294-299).
- `ProbeLock(path, start, length)` (linux and darwin): opens the file and asks the kernel
  `fcntl(F_GETLK)` whether a write lock on that range would conflict; the answer names the
  conflicting lock's type and pid. It runs in the **parent**, which holds no SQLite connection,
  so opening and closing the file there releases nothing. Limits: it reports one conflicting lock
  per range, so for a byte read-locked by several processes the holder named is arbitrary — the
  experiments therefore print only `read-locked` for read locks and name the holder only for
  write locks (a write lock has exactly one holder). SQLite's own `unixIsSharingShmNode`
  (os_unix.c:4679-4695) uses the same `F_GETLK` call on the DMS byte.
- `/proc/locks` (Linux only): `ParseProcLocks` / `ParseProcLockLine` follow the kernel's line
  format: `ID: [->] CLASS MODE TYPE PID MAJ:MIN:INODE START END|EOF` (major/minor hex, inode
  decimal; `->` marks a blocked waiter). Malformed lines are errors (`ErrProcLocksFormat`).
  **Repair round:** lines that the kernel prints for other processes are accepted too
  (`fs/locks.c` `lock_get_status`): `<none>:0` instead of `MAJ:MIN:INODE` (device and inode 0),
  mode `MSNFS` or `UNKNOWN`, class `UNKNOWN`, type `NONE`; one such lock anywhere on a reader's
  host used to abort 6.1.
  `FilterProcLocks(locks, inode, pids)` keeps the children's locks on one file (matched by inode
  number and pid, not device, so an overlay filesystem's device numbering cannot matter; whether
  the device numbers would differ inside Docker was not checked).
  `DescribeProcLocks` names each range (a lock whose range spans several named ranges — the
  kernel merges adjacent locks of one process — is printed as `(A+B)`).
  `ReadProcLocks`/`FileInode` return `ErrNoProcLocks` elsewhere; `ProcLocksSupported`.

**Database helpers (`database.go`).** `CreateDatabase(ctx, path, journal, statements…)` (removes
old files, creates with recipe options, runs the statements, closes, checks at rest).
`InspectDatabase(ctx, path, journal, queries…)` — opens a closed file, runs `integrity_check` and
the queries, closes; SQL errors become text (`error SQLITE_CORRUPT (11)`) instead of failing, so a
torn copy can be described. `IntegrityProblems` / `IntegritySummary`.

**Tests (`go test ./internal/procs`, under 1 s).** Code names; the in-process child loop (ready,
echo, failing and unknown actions, exit, unknown role, role that fails to start, malformed
request, pipe closed without exit); real child processes (round trip, step 0 refused, use after
stop, close error reported, a child that exits with status 7, a role that fails to start, a
timeout that kills a sleeping child, an async call that is not answered yet and blocks a second
request, Stop with a pending request, an answer for the wrong step); the transcript format;
Lab's check/report exit code; a child that takes an `fcntl` write lock on byte 120 of a file is
seen by `ProbeLock` (and in `/proc/locks` on Linux) and **a stray open+close in that child drops
it**; `/proc/locks` parsing of four real-format lines and 17 malformed ones; integrity summary;
the copier's torn-copy semantics and refusals; two SQL children across processes (BUSY 5 on
`BEGIN IMMEDIATE`, BUSY_SNAPSHOT 517 after the other commits, NULL, bad arguments,
checkpoint, `dbpage-write`), and `SQLOptions` round trip. Repair round added: `AnsweredWithin`
false for a sleeping action and true for an immediate one (child not killed); `Lab.StillWaiting`
false for an immediate answer and true for a blocked one; with real SQL children, `StillWaiting`
false for `BEGIN IMMEDIATE` at `busy_timeout=0` (BUSY 5); `Stop` after a wrong exit answer
returns `ErrStepMismatch` with the child already gone; five host-variant `/proc/locks` lines
parsed and three bad `<none>` lines rejected; `readRange` keeps only the bytes read;
`LockView.ProcHolders` (exact, merged, to-EOF entries).

## What each span does (facts read from the copied/linked source)

- **os.h 98-166 (`NO_LOCK` … `SHARED_SIZE`).** Five lock levels: NO_LOCK 0, SHARED 1, RESERVED 2,
  PENDING 3, EXCLUSIVE 4. The comment explains the byte-range scheme inherited from Windows:
  SHARED locks one byte (Win95) or a range, RESERVED and PENDING each lock one designated byte,
  EXCLUSIVE locks the whole shared range; the pager never stores data in these bytes.
- **os.h 160-166 (`PENDING_BYTE`).** PENDING_BYTE = 0x40000000 (the first byte past 1 GiB),
  RESERVED_BYTE = +1, SHARED_FIRST = +2, SHARED_SIZE = 510. The page containing these bytes is
  never used by the database.
- **os_unix.c 1189-1342 (`Posix Advisory Locking` comment, `unixFileId`, `unixInodeInfo`).** POSIX
  locks belong to the process: a lock set through one descriptor overrides locks the same process
  set through another, and closing **any** descriptor of the file releases all of the process's
  locks on it. So SQLite keeps one `unixInodeInfo` per inode (keyed by device + inode) holding the
  process-wide lock state (`nShared`, `eFileLock`, `nLock`) and a list of descriptors whose close
  must wait (`pUnused`); fcntl is called only when the process-level state changes.
- **os_unix.c 1323-1342 (`unixInodeInfo`).** The fields: `nShared`, `nLock`, `eFileLock`,
  `bProcessLock`, `pUnused`, `nRef`, `pShmNode` (the shared-memory node of the same inode).
- **os_unix.c 1471-1482 (`closePendingFds`).** Closes every descriptor parked on the inode's
  `pUnused` list; called when the last lock on the inode is released.
- **os_unix.c 1842-1865 (doc comment of `unixLock`).** The allowed transitions:
  UNLOCKED→SHARED, SHARED→RESERVED, SHARED→EXCLUSIVE, RESERVED→(PENDING)→EXCLUSIVE,
  PENDING→EXCLUSIVE; the routine only raises a lock.
- **os_unix.c 1866-2095 (`unixLock`).** SHARED: read-lock the PENDING byte, read-lock the whole
  510-byte SHARED range, release the PENDING byte. RESERVED: write-lock the RESERVED byte.
  EXCLUSIVE from RESERVED: first write-lock the PENDING byte (PENDING state: no new SHARED locks),
  then write-lock the SHARED range; if that fails the PENDING lock is kept. Within one process the
  inode's counters decide and no fcntl is made if another connection of the process already holds
  a compatible lock. 6.1's `/proc/locks` lines show exactly this: holder READ 1073741826-1073742335
  (SHARED) and WRITE 1073741825 (RESERVED).
- **os_unix.c 2034-2041 (`unixIsSharingShmNode` check).** Since 3.51.0: an EXCLUSIVE request on a
  WAL database (made when the last connection closes or leaves WAL mode, before deleting the WAL
  and `-shm`) returns SQLITE_BUSY if another process still holds the `-shm` DMS lock — the sign
  that database-file locks were broken by a rogue `close(open(db))`.
- **os_unix.c 2101-2109 (`setPendingFd`).** Moves a closing file's descriptor onto the inode's
  `pUnused` list instead of closing it, while other connections of the process still hold locks.
- **os_unix.c 4602-4603 (`UNIX_SHM_BASE`, `UNIX_SHM_DMS`).** The `-shm` lock bytes start at
  (22+8)*4 = 120; the dead-man switch is byte 128.
- **os_unix.c 4679-4695 (`unixIsSharingShmNode`).** `fcntl(F_GETLK)` for a write lock on the DMS
  byte; true if any other process holds a lock there.
- **os_unix.c 4703-4786 (`unixShmSystemLock`).** The fcntl call on the `-shm` descriptor for a lock
  byte range (`F_RDLCK`/`F_WRLCK`/`F_UNLCK`), with the per-process bookkeeping of which bytes are
  held.
- **os_unix.c 6833-6869 (`unixDelete`).** `unlink`; ENOENT → `SQLITE_IOERR_DELETE_NOENT`, other
  failures → `SQLITE_IOERR_DELETE`; then a directory fsync if asked.
- **wal.c 294-299 (`WAL_WRITE_LOCK`).** WAL lock slots: WRITE 0, CKPT 1, RECOVER 2, READ(i) = 3+i,
  `WAL_NREADER` = 8-3 = 5. Slot n is `-shm` byte 120+n.
- **wal.c 1764-1792 (`walIteratorNext`).** Returns the next page number in increasing order and,
  for it, the newest frame among the segments — each page once.
- **wal.c 2199-2395 (`walCheckpoint`), 2312-2314 (skip test).** A page is copied only if its
  (newest) frame is after nBackfill, at or before `mxSafeFrame` (the oldest reader's mark) and
  within the database size; otherwise skipped (correction 5).
- **wal.c 2963 (`WAL_RETRY_PROTOCOL_LIMIT`)** = 100; **2985-2997** the comment: after too many
  retries `walTryBeginRead` returns SQLITE_PROTOCOL, meaning another process is not honouring the
  locking protocol.
- **wal.c 3020-3272 (`walTryBeginRead`), 3053-3056, 3261-3265.** Counts retries; past 5 it sleeps
  (growing delays), past 100 returns SQLITE_PROTOCOL. After taking a read lock it re-checks that
  the read mark and the shared wal-index header still equal its cached copy; if not it unlocks
  and returns WAL_RETRY.
- **wal.c 3699-3746 (`sqlite3WalBeginWriteTransaction`), 3734-3737.** Takes WAL_WRITE_LOCK
  exclusively (no busy handler here); fails with SQLITE_BUSY if held. Then, if the shared
  wal-index header differs from the header this connection's read transaction started with,
  releases the lock and returns SQLITE_BUSY_SNAPSHOT.
- **wal.c 3875-3915 (`walRestartLog`).** Before a writer's first frame: a READ(0) writer on a
  fully backfilled WAL restarts it; then it re-enters a read transaction with `useWal=1` — the path
  that loops into SQLITE_PROTOCOL in 6.1 B. The whole body is under `if( pWal->readLock==0 )`
  (3879): a writer on READ(1..4) skips it, which is why 6.1 C commits without any check.
- **wal.c 2152-2166 (`walRestartHdr`).** Restart: salt-1 + 1, new random salt-2, `mxFrame` 0,
  `nBackfill` 0, read mark 1 = 0, marks 2-4 unused.
- **wal.c 2254-2260.** Backfill needs `WAL_READ_LOCK(0)` exclusively (`walBusyLock(…,
  WAL_READ_LOCK(0), 1)`): a reader on READ(0) blocks every checkpoint copy (correction 7).
- **wal.c 2507-2571 (`sqlite3WalClose`).** The last connection takes EXCLUSIVE on the database
  file (2529), checkpoints, closes the wal-index deleting `-shm` (2559) and deletes the WAL (2563,
  result ignored) — 6.1 D.
- **pager.c 5968-6032 (`sqlite3PagerBegin`).** WAL: (only with `locking_mode=EXCLUSIVE` take the
  database EXCLUSIVE lock first, 5982-5988) then `sqlite3WalBeginWriteTransaction`; the comment says
  the busy handler is not invoked there, the upper layer may call it. Rollback: RESERVED lock, and
  for `exFlag` (BEGIN EXCLUSIVE) wait for EXCLUSIVE with the busy handler.
- **btree.c 3598-3804 (`btreeBeginTrans`), 3740-3741.** Takes the read lock (`lockBtree`) and, for a
  write, `sqlite3PagerBegin`; the whole attempt is retried while the result is SQLITE_BUSY **and the
  b-tree had no transaction (TRANS_NONE)** and the busy handler says retry. A connection already in
  a read transaction gets SQLITE_BUSY immediately (correction 3).
- **build.c 5245-5274 (`sqlite3BeginTransaction`).** BEGIN DEFERRED generates no
  `OP_Transaction` (nothing is locked until the first statement); IMMEDIATE generates
  `OP_Transaction p2=1` (write), EXCLUSIVE `p2=2`, on every attached database.
- **main.c 1730-1792 (`sqliteDefaultBusyCallback`, `sqlite3InvokeBusyHandler`).** The default
  busy handler sleeps 1, 2, 5, 10, 15, 20, 25, 25, 25, 50, 50, then 100 ms per retry until the next
  sleep would pass `busy_timeout`; returning 0 ends the retries with SQLITE_BUSY. The invoker
  counts calls and stops for good after the handler returns 0 (`nBusy = -1`).

## Experiment 6.1 locks-belong-to-the-process

Run: `go run ./ch06-locking-and-concurrency/experiments/6.1-locks-belong-to-the-process`.
Runtime about 12 s (10 s of it is part B's PROTOCOL retry loop). Evidence 204 lines. EXACT.
(Repair round: parts C and D added, `/proc/locks` checks added; 43 checks on Linux, 32 elsewhere
— the 11 `/proc/locks:` checks run only where `/proc/locks` exists.)

Setup per part: a fresh `lab.db` (`CREATE TABLE lab(id INTEGER PRIMARY KEY, who TEXT NOT
NULL)`), two SQL children `holder` and `contender`, busy timeout 0, the parent holds no connection.

How to read the output:
- Line 1 pin; `INFO:` lines: work dir, GOOS, pids. On non-Linux one `LINUX-ONLY:` line says the
  `/proc/locks` listing is skipped (everything else runs).
- `step N  <child> <action> -> <outcome>` — the script; outcomes are extended result codes.
- `<file> locks (fcntl F_GETLK from the parent)` — one line per named range: `unlocked`,
  `read-locked`, or `write-locked by <child>`. `<file> locks in /proc/locks` — the children's
  kernel lock entries on that inode: child, class (POSIX), type, byte range, range name. Sorted.
- **A. rollback mode.** Steps 1-2: holder `BEGIN IMMEDIATE` + insert. Locks: RESERVED byte
  1073741825 write-locked by holder; SHARED 1073741826-1073742335 read-locked (`/proc/locks`:
  holder READ on the SHARED range, holder WRITE on RESERVED); PENDING unlocked. Step 3: contender
  `BEGIN IMMEDIATE` → `SQLITE_BUSY (5)`. Step 4: holder `stray-close db`. Locks: all unlocked,
  `/proc/locks` none. Step 5: the holder still counts 1 row (its own uncommitted insert). Step 6:
  contender `BEGIN IMMEDIATE` → OK; step 7 insert; locks: RESERVED write-locked by contender. Step
  8: contender COMMIT → OK. Step 9: holder COMMIT → `SQLITE_IOERR_DELETE_NOENT (5898)`. Then
  `after both processes closed: integrity_check ok; 1 row(s): holder`.
- **B. WAL mode.** Step numbers continue (one script per run). Steps 10-11 as steps 1-2 above. `-shm` locks: WAL_WRITE_LOCK byte 120 write-locked by
  holder, WAL_READ_LOCK(0) byte 123 read-locked, DMS byte 128 read-locked, the rest unlocked;
  `/proc/locks`: contender READ 128 (DMS), holder READ 123, holder READ 128, holder WRITE 120.
  Database file: SHARED range read-locked by **both** (each WAL connection keeps a SHARED lock),
  PENDING and RESERVED unlocked. Step 12: contender refused (5). Step 13: holder `stray-close db`:
  `-shm` locks unchanged; database file now only the contender's SHARED lock. Step 14: contender
  still refused (5). Step 15: holder `stray-close -shm`: every `-shm` lock of the holder gone; only
  the contender's DMS read lock remains. Step 16: holder still sees its row. Step 17: contender
  admitted (OK), 18 insert, 19 COMMIT OK. Step 20: holder COMMIT → `SQLITE_PROTOCOL (15)`. Final:
  `integrity_check ok; 1 row(s): contender`.
  Repair round: the F_GETLK check on the database file now reads "the SHARED range is
  read-locked and PENDING and RESERVED are free" (F_GETLK names one holder per range, so it
  cannot show that *both* hold SHARED); the `/proc/locks:` checks show it: both read-lock SHARED
  and DMS before step 13, only the contender holds SHARED after the stray close of `db`, after
  the stray close of `-shm` the contender's DMS read lock is the only `-shm` lock. Part A's new
  `/proc/locks:` checks: holder alone on RESERVED (WRITE) and SHARED (READ) before; no locks at
  all after the stray close; after the contender's BEGIN only the contender on SHARED.
- **C. WAL with a commit already in the WAL** (steps 21-30): holder autocommits `seed`, then
  BEGIN IMMEDIATE + insert `holder`; `-shm`: WAL_WRITE_LOCK by holder, **WAL_READ_LOCK(1) byte 124**
  read-locked, READ(0) free (`/proc/locks`: holder READ 124, WRITE 120, both READ 128). Step 24
  contender refused (5). Step 25 holder `stray-close -shm` → only the contender's DMS lock left.
  Steps 26-28 contender BEGIN IMMEDIATE / insert / COMMIT all OK. Step 29 holder COMMIT →
  `SQLITE_OK`. Step 30 contender reads `2 row(s): seed,holder`. Final `integrity_check ok; 2
  row(s): seed,holder`. See correction 1.
- **D. WAL, the contender exits first** (steps 31-38): holder BEGIN IMMEDIATE + insert, stray
  close of `db` and of `-shm`; contender BEGIN / insert / COMMIT all OK; the contender is stopped
  (printed line: `contender closed its connection and exited cleanly; then lab.db-wal is gone and
  lab.db-shm is gone`); step 38 holder COMMIT → `SQLITE_OK`; `after the holder's commit:
  lab.db-wal is gone`; final `integrity_check ok; 1 row(s): holder`. See correction 1.
- Last line `LOCKS BELONG TO THE PROCESS OK`.

Claims confirmed: locks are byte-range locks on the database file (rollback) and on the `-shm`
file (WAL), held by the process; a stray close releases them; second writer refused before and
admitted after; first still believes it holds the lock. Corrected/extended: corrections 1-2.
Neon's list (two connections in one process; the CLI as the second process) was covered by the
adversarial pass (LOCK-03a-d, LOCK-04a-c), not by this experiment.

## Experiment 6.2 busy-rules

Run: `go run ./ch06-locking-and-concurrency/experiments/6.2-busy-rules`. Runtime about 3 s.
Evidence 145 lines. EXACT. (Repair round: runtime about 5 s, each of the 7 `StillWaiting` calls
waits 250 ms.)

Each part uses a fresh `lab.db` with one `seed` row (`INSERT INTO lab(who) VALUES …`) and two SQL
children. The script's per-step deadline is 30 s (`stepDeadline`) while the "generous" busy
timeout is 120,000 ms, so any statement that waited out its busy timeout would make the run fail
with a timeout — that is how "returns at once" is checked without measuring time.

Result table (every cell is one transcript line; codes exact):

| part | case | statement | result |
| --- | --- | --- | --- |
| A WAL, timeout 0 | reader open (BEGIN + count=1) | writer BEGIN IMMEDIATE / INSERT / COMMIT | OK / OK / OK |
| A | same reader | count again | 1 (snapshot) |
| A | writer holds an open write txn | new reader BEGIN + count | OK, 2 (last committed) |
| A | reader open | writer BEGIN EXCLUSIVE / INSERT / COMMIT, reader reads in between | all OK |
| B WAL, timeout 0 | writer-1 holds a write txn | writer-2 BEGIN DEFERRED / count / INSERT | OK / OK 1 / BUSY (5) |
| B | same | writer-2 BEGIN IMMEDIATE | BUSY (5) |
| C1 WAL, 120 s | writer-1 holds | writer-2 BEGIN IMMEDIATE (sent) | no answer after 250ms; OK after writer-1 COMMIT |
| C2 | writer-1 holds | writer-2 BEGIN DEFERRED, then INSERT (sent) | no answer after 250ms; OK after COMMIT |
| C3 | writer-2 BEGIN DEFERRED + count=5; then writer-1 holds | writer-2 INSERT | BUSY (5) at once |
| C3 | writer-1 commits | writer-2 same INSERT | BUSY_SNAPSHOT (517) at once |
| C3 | writer-2 ROLLBACK | BEGIN IMMEDIATE / INSERT / COMMIT | OK / OK / OK |
| D rollback, timeout 0 | reader open (SHARED) | writer BEGIN IMMEDIATE / INSERT | OK / OK |
| D | same | writer COMMIT | BUSY (5) |
| D | reader ends | reader new count (autocommit) | BUSY (5) (writer kept PENDING) |
| D | | writer COMMIT again | OK; reader then counts 2 |
| E WAL, 120 s | recipe steps 6-15, two writers alternating | each `begin immediate` sent while the other holds step k | every one "no answer after 250ms", then OK |

Part E reading (repair round): `writer-1 begin immediate` + `recipe-exec 6` (holds the lock with
step 6's writes); `writer-2 begin immediate` sent → `no answer after 250ms` → `writer-1 commit` →
`writer-2 begin immediate -> SQLITE_OK` → `writer-2 recipe-exec 7` → `writer-2 commit`; writer-2
then begins step 8 itself and the roles swap, five rounds (steps 6-15). Before the repair the
waiting call was `recipe-step k+1`, which regenerated recipe steps 0..k before its BEGIN, so in
some rounds the holder had committed before the waiter ever asked for the lock (review finding 1,
strace showed no EAGAIN on byte 120 in those rounds). `user-version` of the last writer: 15. After both
close: `the closed file equals recipe step 15 (equal except the change counter at bytes 24-27 and
92-95)` — `recipe.MatchStep` with the change-counter mask (two long-lived connections; ch01
correction). Last line `BUSY RULES OK` (18 checks).

Claims confirmed: readers never block the writer and the writer never blocks readers in WAL mode;
a second writer gets SQLITE_BUSY at a point fixed by its BEGIN mode (at BEGIN for IMMEDIATE, at
the first write for DEFERRED); a busy timeout turns that into a wait (qualified: correction 3);
517 at once whatever the timeout, and the write succeeds after restarting; in rollback mode the
reader blocks the writer's commit; with a timeout and IMMEDIATE every transaction commits and the
final database equals the recipe state. Also shown: BEGIN EXCLUSIVE = BEGIN IMMEDIATE in WAL mode;
rollback-mode PENDING blocks new readers after a failed COMMIT.
Limit of the "still waiting" evidence (repair round): `no answer after 250ms` proves the
statement had not returned 250 ms after it was sent while the other writer still held the lock; an
immediate answer (BUSY at `busy_timeout=0`, or success) arrives well inside that window and makes
the check fail (unit test with real SQL children; adversarial BUSY-09 now HELD). The driver still
exposes no busy-handler hook; the adversarial BUSY-09 strace of the repaired 6.2 counted 86
`EAGAIN` results on `-shm` byte 120 (was 15-24), i.e. every waiting step retried the lock.
Neon's list not built: three processes, the CLI as a participant, 100 re-runs per cell.

## Experiment 6.3 torn-copy

Run: `go run ./ch06-locking-and-concurrency/experiments/6.3-torn-copy`. Runtime about 2-3 s.
Evidence 52 lines. EXACT.

- **A (WAL).** Recipe built to step 5 (closed). Writer child (`wal_autocheckpoint=0`) commits steps
  6-8, `checkpoint PASSIVE` → `busy=0 log=61 checkpointed=61`, commits 9-10. Copier copies the
  database file (323,584 bytes) and the `-wal` (251,352 bytes) between commits.
  - **A1** `copy main-only.db: 323584 bytes (79 pages)`, `integrity_check: ok`, `user_version: 8;
    50 accounts, 336 events, 25 documents`, `equals: step 8 (equal except the change counter …)`.
    The file alone is a valid database of the last checkpoint (step 8), silently missing commits
    9-10.
  - **A2** the writer reports half the file (204,800 bytes = 50 pages); the copier copies bytes
    0-204,800; the writer checkpoints (`busy=0 log=40 checkpointed=40`: steps 9-10's 40 frames go
    into the file, which grows to 100 pages); the copier copies 204,800-end. `copy torn.db: 409600
    bytes (100 pages)`, `integrity_check: 25 problem(s), the first 3: wrong # of entries in index
    events_by_account / row 335 missing from index events_by_account / row 336 …`,
    `user_version: 8; 50 accounts, 336 events, 25 documents`, `equals: no recipe step`.
- **B (rollback contrast).** Recipe in `journal_mode=delete` built to step 5. Half = 102,400 bytes
  (25 of 50 pages); copy the first half, the writer commits step 6, copy the rest. `copy torn.db:
  245760 bytes (60 pages)`, `integrity_check: 6 problem(s)` (first: `Freelist: freelist leaf count
  too big on page 35`, `Freelist: invalid page number 52`, `Tree 3 page 24 cell 0: overflow list
  length is 1 but should be 3`), `user_version: 5; 31 accounts, 251 events, 15 documents`, `equals:
  no recipe step`.
- `TORN COPY OK` (5 checks). Each copy's sha256 is checked unchanged by the inspection. In part A
  the parent opens the copies `main-only.db` and `torn.db` while the writer and copier children are
  still running (they are stopped afterwards); in part B the children are stopped first. The copies
  are separate files, so opening them touches no lock of `r.db` (review finding 6 corrected this
  note, which said the copies were always opened after the children closed).

Claims confirmed: a copy taken while the file is written is not a database (both torn copies fail
`integrity_check` here); a copy of the main file alone in WAL mode opens cleanly but equals an
older state (the last checkpoint). Because the interleaving is scripted, the experiment shows one
specific torn copy per mode, not a rate. Revision 1's probe (8 of 8 naive copies failed) was
timing-based; this one is exact. Neon's "find a copy that happens to pass": A1 is exactly such a
copy (taken between commits, no checkpoint running) — consistent and stale.

## Experiment 6.4 live-snapshot (rung R4)

Run: `go run ./ch06-locking-and-concurrency/experiments/6.4-live-snapshot [-hold read|immediate]`.
Default `-hold read` (the evidence). Runtime about 3 s. Evidence 68 lines. EXACT.

Setup: recipe built to step 5 (closed); children `writer` (`wal_autocheckpoint=0`), `capture`
(SQL role with `-pages-out _work/6.4/snapshot.db`), `copier`. The writer commits steps 6-8.

- **Method 1.** capture `begin deferred`, `user-version` → 8 (this first read starts the read
  transaction and pins the snapshot), `PRAGMA page_count` → 79. Then `dbpage-write 1 20`,
  `21 40`, `41 60`, `61 79`; between chunks the writer commits steps 9, 10, 11 and runs a PASSIVE
  checkpoint, bracketed by NOOP checkpoints that read nBackfill (`checkpoint NOOP → busy=0 log=84
  checkpointed=0`, `PASSIVE → … checkpointed=61`, …). So the first checkpoint copies 61 frames
  into the database file **while the page capture is half done**; later ones stop at 61, the
  capture's mark. `user-version` at the end still 8; commit.
- **Method 2.** A new read transaction (`user-version` → 11, the last commit). The writer reports
  half the file (221,184 bytes); copier copies bytes 0-221,184 of the database file; the writer
  commits step 12 and checkpoints (nBackfill 61 → 116, i.e. frames written **into the file while
  it is being copied**); copier copies the rest (438,272 bytes); the writer commits step 13; the
  copier copies the `-wal` (688,072 bytes, 167 frames). The capture's transaction still sees 11;
  commit. Writer sees 13.
- **Checks after every process closed.** `page capture: 79 pages, manifest sha256 fdacd7ac…`
  (chapter 2's `capture.Pages` on `snapshot.db`, then `capture.Rebuild` → `rebuilt.db`).
  `rebuilt from the page capture: sha256 1558477d…, integrity_check ok, user_version 8, equals
  recipe step 8 (…change counter…)`. `reference (recipe to step 8 on one connection, then
  checkpoint)`: the recipe built to 5, one connection applies 6-8, `wal_checkpoint(TRUNCATE)`,
  close → the same sha256. `E1 rebuilt vs reference: E1 bytes: equal (sha256 1558477d…)`.
  Then `walfile.ReplayFiles(copy.db, copy.wal)`: `frames in file 167, valid 167, committed
  transactions 8, last commit frame 167 (database size 124 pages)`, `applied 167 frames (92
  distinct pages, …); base 438272 bytes -> output 507904 bytes`, `ignored: none`; `rebuilt from
  the file + -wal copy: … integrity_check ok, user_version 13, equals recipe step 13`. And `the
  database-file copy on its own: … integrity_check 10 problem(s), the first: Freelist: …,
  user_version 5, equals recipe no recipe step` (correction 5 explains user_version 5).
- `LIVE SNAPSHOT OK` (10 checks).

Why the reference is "one connection applies 6-8": the writer child applied 6-8 on one long-lived
connection, which leaves a change counter different from a per-step build (ch01 correction 1);
replaying the same history on one connection reproduces the bytes exactly, so the claim is E1,
not only the masked match.

`-hold immediate` (exposed for the adversarial pass; output not stored as evidence): the capture
uses `BEGIN IMMEDIATE`; between chunks and halves the writer only checkpoints (it cannot commit).
Run here once: every check passes; method 1 equal at E1 to step 8; PASSIVE checkpoints **do run**
under the held `BEGIN IMMEDIATE` (61 frames copied into the file during the page capture); the
second transaction starts on a fully backfilled WAL, so method 2 sees no file change and the
database-file copy alone already equals step 8; the replay gives step 8. That is: under `BEGIN
IMMEDIATE` a checkpoint by another connection can still write into the database file during a
file + WAL copy; the capture stays valid only because the WAL is copied after the file and replayed
whole. The torn-base check is skipped in this mode (nothing tears).

Claims confirmed: all pages read inside one read transaction form the snapshot's database
(integrity ok, E1-equal to the recipe run to exactly that step) while a writer commits and
checkpoints; a plain copy of the database file and then its `-wal`, taken while a transaction is
held, replays to a recipe state at or after the snapshot (here exactly the last commit before the
`-wal` copy) even though the file copy itself is torn. Neon's list: two read transactions spanning
a capture, checkpoint during the copy by another connection (done here by the writer's
connection), `-hold immediate` (flag provided), slow reader growing the WAL (ch05 5.3).

## Timeline data (chapter section 4)

Rollback mode, lock bytes of the database file (6.1 A, 6.2 D):

| moment | holder / writer | other process |
| --- | --- | --- |
| BEGIN IMMEDIATE + first write | SHARED range read (1073741826-1073742335) + RESERVED write (1073741825) | can read (SHARED); BEGIN IMMEDIATE → BUSY |
| COMMIT while a reader holds SHARED | takes PENDING (write 1073741824), fails EXCLUSIVE → BUSY, keeps PENDING | existing reader continues; new readers → BUSY |
| reader gone, COMMIT again | EXCLUSIVE (write on SHARED range), writes db, deletes journal, unlocks | — |

WAL mode, lock bytes of the `-shm` file and the database file (6.1 B, 6.2 A-C):

| moment | writer | reader / other |
| --- | --- | --- |
| every open connection | DMS byte 128 read; database SHARED range read | same |
| read transaction on a fully checkpointed / empty WAL | WAL_READ_LOCK(0) byte 123 read | — |
| read transaction with un-checkpointed frames | WAL_READ_LOCK(i) byte 123+i read, aReadMark[i] = its mxFrame (ch05 5.3) | — |
| BEGIN IMMEDIATE | WAL_WRITE_LOCK byte 120 write (plus its read lock) | readers start and commit freely; another writer → BUSY or waits |
| COMMIT | frames appended, wal-index header published, byte 120 released | readers that began earlier keep their snapshot |
| deferred reader writes after another commit | — | BUSY_SNAPSHOT 517 |

Byte numbers: PENDING 1073741824, RESERVED 1073741825, SHARED 1073741826-1073742335; `-shm`
120 WRITE, 121 CKPT, 122 RECOVER, 123-127 READ(0-4), 128 DMS.

## Platform implications (facts)

1. **One writer per database is SQLite's rule; the platform decides who.** Two writers on one
   database serialize on one lock byte (`-shm` 120 in WAL); with `BEGIN IMMEDIATE` and a busy
   timeout they queue and all commit (6.2 E); with a deferred transaction that read first, no
   timeout helps (6.2 C3: BUSY at once, then 517).
2. **`BEGIN IMMEDIATE` for writers.** It is the only mode whose BUSY can be waited out by the busy
   timeout in every case, and it fails at BEGIN — before any work — when it fails (6.2 B, C1).
3. **Never touch a database file or its `-shm` with ordinary file calls in a serving process.**
   One `os.Open`+`Close` releases the process's locks on that file (6.1); the result is a lost
   commit that was reported as successful (rollback mode A; WAL mode C and D) or a lost
   transaction reported as SQLITE_PROTOCOL after ~10 s (WAL mode B, only when the holder's
   transaction began on an empty or fully checkpointed WAL). Hashing, copying, `stat`-by-open, log shipping and backup
   code must run in another process or after the last connection closed.
4. **SQLite's locks work between processes on one machine and nowhere else.** They are kernel
   POSIX advisory locks on one inode (6.1's `/proc/locks`, F_GETLK); WAL also needs the shared
   `-shm` mapping. A platform that moves a database between machines must move ownership, not
   share the file.
5. **A capture must go through SQLite's locking, not around it.** Plain copies of a live file are
   torn (6.3 A2, B; 6.4's file copy alone); a main-file-only copy in WAL mode is a stale but valid
   database (6.3 A1) — the most dangerous result because nothing reports it. Two safe live captures
   (R4): every page through `sqlite_dbpage` in one read transaction (E1-exact to the snapshot), or
   file then `-wal` copied while a transaction is held, rebuilt by WAL replay (state ≥ snapshot).
6. **A held read transaction is cheap for the capture and costly for the database**: checkpoints
   stop at its mark (6.4: 61 for the whole capture), so the WAL grows while it lasts (ch05 5.3).
7. **Watch for SQLITE_PROTOCOL and SQLITE_IOERR_DELETE_NOENT in logs**: on a local filesystem both
   are signs that something broke the locks (6.1). Their absence proves nothing: 6.1 C and D lose
   a commit with every statement returning SQLITE_OK.

## WAL-default evaluation (ruling 10) for this chapter

WAL is the default in every experiment; rollback mode appears only as the contrast the outline
lists (6.1 A, 6.2 D, 6.3 B). Nothing found argues against WAL as the platform default; the
evidence favours it for concurrency:

| Question | WAL | rollback | Evidence |
| --- | --- | --- | --- |
| Does a reader block a commit? | no | yes (BUSY at COMMIT, then PENDING blocks new readers) | 6.2 A vs D |
| Does a writer block readers? | no | yes once it reaches PENDING/EXCLUSIVE | 6.2 A vs D |
| Writer lock lives on | `-shm` byte 120 | database file RESERVED byte | 6.1 |
| Stray close of the database file breaks writer exclusion? | no (only a `-shm` close does) | yes | 6.1 B vs A |
| Outcome of broken locks | depends on state: holder told with SQLITE_PROTOCOL (B, WAL empty/checkpointed); or both commits OK and one silently lost (C, WAL with frames; D, other process exits first) | a commit reported OK is lost, the failed one persists | 6.1 |
| Safe live capture without blocking the writer | yes (R4, both methods) | no (a reader blocks commits for the whole capture) | 6.4, 6.2 D |

WAL-specific obligations added by this chapter: the `-shm` file is part of the locking and must be
treated like the database file (no stray opens in a serving process); WAL needs shared memory on
one host.

## Linux-only and the Mac route

- Linux-only: the `/proc/locks` listing in 6.1 (`proclocks_linux.go`). On other systems 6.1 prints
  `LINUX-ONLY: the /proc/locks listing (the fcntl F_GETLK probe and every behavioural check still
  run) skipped on darwin; run it with: docker compose run --rm lab go run
  ./ch06-locking-and-concurrency/experiments/6.1-locks-belong-to-the-process` and runs everything
  else, exit 0 if the claims hold.
- Mac-native: the `fcntl(F_GETLK)` probe (`lockprobe_unix.go`, linux || darwin) shows the same lock
  table on macOS; every behavioural check (refused/admitted, result codes, final rows) is portable.
  POSIX advisory lock semantics (per process, released by closing any descriptor of the file) are
  the same on macOS, and the modernc darwin build runs the same `os_unix.c` logic.
  **darwin: cross-built only** — not verified: the F_GETLK pid field on APFS, that the PROTOCOL
  retry loop takes the same ~10 s, that `unixDelete` reports ENOENT the same way.
- 6.2, 6.3, 6.4 use nothing Linux-specific.
- Lab container: all four run there with identical EXACT output (see top).

## Working scripts

Repair round: `agent_work/ch06/probes/snap04-restart/main.go`
(`go run ./agent_work/ch06/probes/snap04-restart`, about 1 s; work dir `_work/adv-ch06-probe-snap04`)
shows the WAL restart under a READ(0) reader (correction 7). Otherwise none beyond the experiments;
`agent_work/ch06/INDEX.md` lists the notes and the report. One-off
checks done by hand during the build (no script kept): reading the header fields of
`_work/6.4/copy.db` (change counter 7, page count 51, user_version 5) for correction 5.

## Not done / for later chunks

- No prose, no comments (seat rule).
- Neon's adversarial items not built: 6.1 two connections in one process, CLI as the second
  process; 6.2 three processes, CLI participant, 100 re-runs; 6.3 hunting for a passing torn copy;
  6.4 capture spanning two read transactions, `-hold immediate` beyond the one run noted above.
- The busy-wait itself cannot be observed inside the driver (no `sqlite3_busy_handler` access);
  6.2's waits are evidenced by ordering, not by a busy-handler count.
