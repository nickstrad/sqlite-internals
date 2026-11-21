---
title: "Chapter 6: POSIX lock loss, busy rules and live snapshots as measured at 3.53.4"
summary: "Locking facts for a platform builder: closing any descriptor to a file drops the process's locks on it and a transaction is lost, a busy timeout only helps a transaction that has not read yet, locking_mode=EXCLUSIVE breaks the writer-never-blocks-readers rule, a reader holds back a WAL restart only when it holds a read mark."
tags: [sqlite, locking, busy, wal, walfleet]
updated: 2026-11-21
verified: 2026-11-21 — Linux amd64, Go 1.26, SQLite 3.53.4 via modernc.org/sqlite v1.60.1; evidence in the walfleet repo
---

# Chapter 6: POSIX lock loss, busy rules and live snapshots as measured at 3.53.4

Locking facts for a platform builder: closing any descriptor to a file drops the process's locks on it and a transaction is lost, a busy timeout only helps a transaction that has not read yet, locking_mode=EXCLUSIVE breaks the writer-never-blocks-readers rule, a reader holds back a WAL restart only when it holds a read mark.

Source: the `walfleet` repo (github.com/nickstrad/walfleet), chunk `ch06`. Working notes: `agent_work/ch06/build-notes.md`; review and adversarial records in the same directory.

## Corrections to the outline


### Correction 1

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

### Correction 2

2. **6.1: "closing any descriptor to the file releases them" is per file.** POSIX locks are per
   (process, inode). In WAL mode the writer lock lives on the `-shm` file (byte 120), so a stray
   open+close of the **database file** drops only the holder's database-file lock (the SHARED
   read lock) and the second writer is **still refused**; only a stray close of the **`-shm`**
   file lets it in. The chapter should say: closing any descriptor of a file releases all of the
   process's locks on *that* file; in WAL mode the writer exclusion is on the `-shm` file.

### Correction 3

3. **6.2: a busy timeout turns the failure into a wait only for a transaction that has not read
   yet.** Confirmed: `BEGIN IMMEDIATE`, and a `BEGIN DEFERRED` whose first statement is a write,
   wait for the lock and then succeed. **Not** true for a deferred transaction that has already
   read: its first write returns `SQLITE_BUSY` (5) at once even with `busy_timeout=120000`
   (part C3), because `btreeBeginTrans` retries through the busy handler only while
   `inTransaction == TRANS_NONE` (btree.c:3740-3741); after the other writer commits, the same
   write returns `SQLITE_BUSY_SNAPSHOT` (517) at once (wal.c:3734-3737). The outline's sentence
   "a busy timeout turns that failure into a wait" needs this qualifier. The 517 half of the claim
   is confirmed exactly as written.

### Correction 4

4. **Span fix: `unixLock` (outline: `os_unix.c` 1843–2095).** Line 1843 is the second line of the
   function's doc comment (the comment opens at 1842); the function itself starts at 1866.
   SPANS.txt lists `1842 1865` (the doc comment with the transition chart) and `1866 2095`
   (`unixLock`). Every other span in the reading path was exact (start line holds the symbol, end
   line is the closing brace / end of the block).

### Correction 5

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


### Correction 6

6. **Repair round: `PRAGMA locking_mode=EXCLUSIVE` is outside "the writer never blocks
   readers"** (adversarial BUSY-05b). A WAL connection in `locking_mode=EXCLUSIVE` takes the
   database file's EXCLUSIVE lock at its first write (`sqlite3PagerBegin`, pager.c:5982-5988) and
   keeps it; a second process then cannot even open (`PRAGMA page_count: database is locked (5)`
   at the SQL role's open). No experiment of this chapter uses it; the claim is for the default
   `locking_mode=NORMAL` and the chapter should say so once.

### Correction 7

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

