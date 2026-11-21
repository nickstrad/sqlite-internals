---
title: "sqlite-internals chapter 7: VFS calls, sync counts and a pure-Go VFS on modernc, as measured at 3.53.4"
summary: "VFS facts for a platform builder: WAL NORMAL is not zero syncs on every commit, rollback sync counts are syscalls not VFS calls, xAccess maps to stat, the Go driver and C CLI make the same VFS calls, a custom VFS needs io_methods v2 for WAL."
tags: [sqlite, vfs, fsync, modernc, sqlite-internals]
updated: 2026-11-21
verified: 2026-11-21 — Linux amd64, Go 1.26, SQLite 3.53.4 via modernc.org/sqlite v1.60.1; evidence in the sqlite-internals repo
---

# sqlite-internals chapter 7: VFS calls, sync counts and a pure-Go VFS on modernc, as measured at 3.53.4

VFS facts for a platform builder: WAL NORMAL is not zero syncs on every commit, rollback sync counts are syscalls not VFS calls, xAccess maps to stat, the Go driver and C CLI make the same VFS calls, a custom VFS needs io_methods v2 for WAL.

Source: the `sqlite-internals` course repo (github.com/nickstrad/sqlite-internals), chunk `ch07`. Working notes: `agent_work/ch07/build-notes.md`; review and adversarial records in the same directory.

## Corrections to the outline

1. **WAL at `synchronous=NORMAL` is not "zero syncs on every commit".** Ordinary commits make no
   sync (7.2: 998 of the 1,000 one-row commits). Two kinds of commit do sync: (a) the commit that
   runs an automatic checkpoint (WAL reached 1,000 frames) syncs the WAL before copying pages back
   and the database after (`walCheckpoint`, `wal.c:2277` and `wal.c:2333`, both
   `CKPT_SYNC_FLAGS(sync_flags)`): 2 syncs; (b) the next commit, which finds the WAL fully
   checkpointed, restarts it from frame 0 (`walRestartLog`, `wal.c:3875-3915`), writes a new WAL
   header and syncs it (`wal.c:4114-4124`, `if( pWal->syncHeader ) sqlite3OsSync(...,
   CKPT_SYNC_FLAGS(sync_flags))`): 1 sync. At FULL the same two commits make 3 and 2 syncs, every
   other commit 1. The first commit into a brand-new WAL file also syncs its header and the
   directory (7.1; not inside 7.2's measured window because the table was created before it).
   Why: `sqlite3PagerSetFlags` (`pager.c:3685-3698`) sets `walSyncFlags = syncFlags<<2` (header
   and checkpoint syncs, always on unless `synchronous=OFF`) and adds `syncFlags` in the low bits
   (commit syncs) only when `fullSync` (FULL or EXTRA). The outline's sentence "In WAL mode `NORMAL`
   does not sync on commit at all" needs "except the commit that checkpoints and the commit that
   restarts the WAL". The book the outline criticises is still wrong (it says a sync on every
   commit); 7.2 shows the exact pattern.
2. **"Rollback 3 at NORMAL, 4 at FULL" counts syscalls, not VFS calls.** 7.2 counts fsync/fdatasync
   syscalls: NORMAL = journal 1 + directory 1 + database 1 = 3; FULL = journal 2 + directory 1 +
   database 1 = 4. In VFS calls (7.3) it is 2 and 3 `xSync` calls, because the directory sync
   happens inside the first `xSync` of a newly created journal (`unixSync`, `os_unix.c:3941-3953`,
   one time per `xOpen` with create flag: `isNewJrnl`, `os_unix.c:6569-6572`, sets
   `UNIXFILE_DIRSYNC` at `:6777`). In DELETE mode every transaction creates the journal again, so
   every transaction pays the directory sync.
3. **The vfstrace "FULL for NORMAL" quirk applies to every sync in a default configuration.**
   Confirmed in source (`ext/misc/vfstrace.c:453`: `if( flags & SQLITE_SYNC_FULL )` with
   `SQLITE_SYNC_FULL` = 0x03, so 0x02 also matches) and measured: 7.3's Go VFS records the real flag
   0x2 (NORMAL) for every sync at `synchronous=FULL` and at `NORMAL`, while the CLI's trace prints
   `FULL`. SQLite passes `SQLITE_SYNC_FULL` only when `PRAGMA fullfsync=ON`
   (`pager.c:3687-3688`, `PAGER_FULLFSYNC`); `synchronous=FULL` selects how many syncs, not the
   flag. The rollback journal's second sync adds `SQLITE_SYNC_DATAONLY` only when the flag is FULL
   (`pager.c:4414-4417`), so it is never seen in default settings.
4. **`xAccess` maps to `stat`, not `access()`.** For `SQLITE_ACCESS_EXISTS` (the only flag seen)
   `unixAccess` calls `osStat` and treats a zero-length regular file as absent
   (`os_unix.c:6896-6898`); only `SQLITE_ACCESS_READWRITE` uses `access()`. strace shows
   `newfstatat` (normalized to `stat` in 7.1).
5. **Where the CLI first calls `xRandomness` depends on the CLI build.** The installed CLI calls it
   right after opening the database; the `./configure` build calls it right before the first
   journal header (the header's checksum nonce is random). 7.1 leaves the one `xRandomness` line out
   of the printed stream (count still printed, position on an `INFO:` line) so both builds give the
   same evidence. Neither position is part of any claim.
6. **The Go driver and the C CLI make the same VFS calls, call for call.** Not an outline claim but
   stronger than chunk 1's syscall twin: 7.3 compares the complete ordered list (method, file,
   offset, length, lock level, shm slot, sync label, file-control op) of one committed INSERT in
   four configurations; all four are identical.
7. Outline section 4.3 ("No chapter builds or installs a VFS") is superseded by ruling 11; this
   chunk builds two (a forwarding VFS and a read-only remote one).

Nothing else in the chapter-7 outline was false here. Every outline span was correct at the pin
(re-checked with `go run ./cmd/course check spans ch07`); SPANS.txt adds 40 finer spans.

