---
title: "sqlite-internals chapter 6: locking and concurrent-access facts measured at 3.53.4"
summary: "A stray close of a database (or -shm) descriptor loses a transaction in both journal modes; in WAL the writer lock is -shm byte 120; a deferred transaction that already read gets BUSY at once whatever the busy timeout; a reader-limited checkpoint leaves the database file in a mixed state; both R4 live captures work."
tags: [sqlite, locking, wal, concurrency, sqlite-internals]
updated: 2026-11-20
verified: 2026-11-20 — Linux amd64 (host and Debian lab container), Go 1.26, SQLite 3.53.4 via modernc.org/sqlite v1.60.1; darwin cross-built only; evidence in the sqlite-internals repo
---

# sqlite-internals chapter 6: locking and concurrent-access facts measured at 3.53.4

Source: the `sqlite-internals` course repo, chunk `ch06`. Working notes:
`agent_work/ch06/build-notes.md`. Experiments: `ch06-locking-and-concurrency/experiments/6.1`-`6.4`,
all scripted through `internal/procs` (child processes driven step by step, no sleeps).

## Findings

1. **A stray `os.Open`+`Close` of a file in a process that holds a SQLite connection drops that
   process's locks on that file — and a transaction is lost.** Rollback mode: the other process's
   COMMIT returns SQLITE_OK but its row is gone; the holder's COMMIT writes its pages and returns
   SQLITE_IOERR_DELETE_NOENT (5898) because the shared `-journal` was already deleted. WAL mode:
   the holder's COMMIT returns SQLITE_PROTOCOL (15) after about 10 s of retries (wal.c:3053,
   limit 100 at 2963) and its row is lost. Both files pass `integrity_check`.
2. **WAL writer exclusion is on the `-shm` file, byte 120.** A stray close of the database file
   does not let a second writer in (it only drops the SHARED read lock every WAL connection keeps
   on the database file); a stray close of the `-shm` does. Lock bytes: database PENDING
   1073741824, RESERVED 1073741825, SHARED 1073741826-1073742335 (the whole range, read-locked);
   `-shm` 120 WRITE, 121 CKPT, 122 RECOVER, 123-127 READ(0-4), 128 DMS.
3. **A busy timeout only helps a transaction that has not read yet.** BEGIN IMMEDIATE, or a
   deferred transaction whose first statement writes, waits and succeeds. A deferred transaction
   that already read gets SQLITE_BUSY at once even with a 120 s timeout (btree.c:3740-3741:
   retries only at TRANS_NONE), then SQLITE_BUSY_SNAPSHOT (517) once the other writer commits.
4. **Rollback mode: a COMMIT refused by a reader keeps PENDING**, so new readers get SQLITE_BUSY
   until the writer retries and succeeds. WAL: readers and the writer never block each other;
   BEGIN EXCLUSIVE behaves like BEGIN IMMEDIATE.
5. **`nBackfill` (the `checkpointed` count) does not mean the database file holds that state.**
   A checkpoint limited by a reader's mark skips every page whose newest WAL frame is past the
   mark (wal.c:2312), even if an older frame of that page is within it. Measured: checkpointed=61
   (step 8) while page 1 in the file was still step 5's. Between checkpoints the database file
   alone can be a mixture of states.
6. **Live capture (rung R4) works two ways:** every page through `sqlite_dbpage` inside one read
   transaction is byte-identical (E1) to the recipe replayed on one connection to the snapshot
   step and checkpointed, while a writer commits and checkpoints between chunks; and a copy of the
   database file and then its `-wal`, taken while a transaction is held, replays to the last
   commit before the `-wal` copy even though the file copy alone is torn by a concurrent
   checkpoint. PASSIVE checkpoints by another connection still run under a held BEGIN IMMEDIATE.
7. **Observing locks portably:** `fcntl(F_GETLK)` from a process without a connection reports the
   conflicting lock's type and pid (measured on Linux; same API on macOS, cross-built only; one holder per probe, so name holders only
   for write locks). `/proc/locks` lists every lock on Linux; the experiments match entries by inode
   number and pid, not device (not compared against `stat` under overlay filesystems).
