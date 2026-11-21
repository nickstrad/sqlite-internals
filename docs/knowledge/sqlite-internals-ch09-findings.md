---
title: "sqlite-internals chapter 9: capture methods, crash versus power loss, integrity_check limits and point-in-time rebuild, as measured at 3.53.4"
summary: "Backup facts for a platform builder: backup remaining-count is not monotonic, VACUUM INTO of a WAL database yields a rollback-mode file, a killed process loses nothing at any synchronous level while a power cut at NORMAL loses commits since the last checkpoint, integrity_check misses changed unindexed values, WAL salts cannot count restarts."
tags: [sqlite, backup, crash-recovery, integrity, wal-shipping, sqlite-internals]
updated: 2026-11-21
verified: 2026-11-21 — Linux amd64, Go 1.26, SQLite 3.53.4 via modernc.org/sqlite v1.60.1; evidence in the sqlite-internals repo
---

# sqlite-internals chapter 9: capture methods, crash versus power loss, integrity_check limits and point-in-time rebuild, as measured at 3.53.4

Backup facts for a platform builder: backup remaining-count is not monotonic, VACUUM INTO of a WAL database yields a rollback-mode file, a killed process loses nothing at any synchronous level while a power cut at NORMAL loses commits since the last checkpoint, integrity_check misses changed unindexed values, WAL salts cannot count restarts.

Source: the `sqlite-internals` course repo (github.com/nickstrad/sqlite-internals), chunk `ch09`. Working notes: `agent_work/ch09/build-notes.md`; review and adversarial records in the same directory.

## Corrections to the outline


### Correction 1

1. **9.1: "the backup's remaining-page count falls steadily with same-connection writes" is not
   what the count does.** `sqlite3_backup_remaining` is `nSrcPage + 1 - iNext`
   (backup.c:428-429): it is recomputed from the *current* source size at every step. When the
   backup's own connection commits a recipe step, the database grows (100 → 108 → 118 pages in
   the run), so `remaining` went **up** (90 → 93 → 98) although the backup did not restart. What
   is steady is the copied count `pagecount − remaining` (= `iNext − 1`): 5, 10, 15, 20, 25 across
   the two same-connection commits, then **5 again** after the writer child's commit (restart).
   The experiment prints both columns and checks the copied count. Mechanism unchanged from the
   outline: same-connection commits go through `sqlite3BackupUpdate` → `backupUpdate`
   (backup.c:688-715), which re-copies already-copied pages (`iPage < p->iNext`) and leaves
   `iNext` alone; another connection's commit makes the source pager reset its cache
   (`pager_reset`, pager.c:1783-1787) which calls `sqlite3BackupRestart` → `iNext = 1`
   (backup.c:728-734). Precisely: same-connection **commits** do not restart the backup;
   a same-connection write transaction that is **rolled back** does (WAL rollback calls
   `sqlite3BackupRestart`, pager.c:3185). Adversarial CAP-03a measured copied 5, 10, then 5, 10
   after a same-connection write + ROLLBACK; CAP-03b: an empty `BEGIN IMMEDIATE; COMMIT` does not
   restart (5, 10, 15, 20). The experiment only shows commits; say "commits", not "operations".

### Correction 2

2. **9.1: `VACUUM INTO` of a WAL database is not E3-equal to its source under the course's E3
   definition.** `equal.LogicalPragmas` includes `journal_mode`; the `VACUUM INTO` file is a
   rollback-mode file (`journal_mode` `rollback` vs `wal`, already noted by chunk 3). Every other
   part of the logical hash is equal. The experiment reports "E3 except journal_mode" and "E1 after
   VACUUM INTO" (canonical form of the capture equals the canonical form of the recipe state) as
   the levels it reaches. Section 4.2's table ("VACUUM INTO: E3") should read "E3 except
   `journal_mode`; E1 between canonical forms".

### Correction 3

3. **9.1: the backup API's E2 difference is bytes 27, 43, 95 here** (change counter low byte, schema
   cookie low byte, version-valid-for low byte) — within the declared ignore list 24-27, 40-43,
   92-99, as section 4.2 says ("24, 40, 92" names the field starts). **Repair round:** the
   backup's canonical form (`VACUUM INTO` of it) is *not* E1 to the source's canonical form; they
   differ at byte 43 only (schema cookie 2 vs 5), because backup.c:448 sets the destination's
   cookie to its own old value + 1. So a backup API capture is E2, never "E1 after VACUUM INTO".

### Correction 4

4. **9.2(b): the model is portable, and is computed from the rule in the source, cross-checked by
   strace on Linux.** The outline says the model takes "the WAL bytes written after the WAL's last
   sync" from "the strace record of an uninterrupted run" — strace is Linux-only, but the brief
   requires the model half to be portable. The experiment therefore computes the cut from the
   sync rule read from `pager.c:3658-3704` and `wal.c` (below), and on Linux checks that strace's
   record of the same run gives exactly the same cut (`trace.SyncsOf`). Both agree at all three
   levels and in the fourth run added in the repair round (below). On a Mac the rule half runs
   and the strace half prints a `LINUX-ONLY:` line.
   **Repair round correction to the rule (adversarial CRASH-06):** the build's NORMAL rule ("cut at
   byte 32 unless the last event was a checkpoint") was true only for this workload, where the
   checkpoint copied every frame and the next commit restarted the WAL. When a reader stops the
   checkpoint short of the end, the WAL does not restart and the checkpoint's own WAL sync
   (wal.c:2277, which syncs the whole `-wal` before any database write) is the last sync: the cut
   is the WAL's end at the checkpoint (304912 bytes in the run), not 32. And because the database
   file is synced only when the checkpoint copied every frame (wal.c:2326-2334, `mxSafeFrame ==
   mxFrame`), a partial checkpoint leaves unsynced database-file writes at NORMAL too — but unlike
   OFF they are harmless: they are copies of WAL frames that were synced before they were written,
   and recovery reads those pages from the WAL. The corrected rule: NORMAL → no cut if the last
   event was a checkpoint; else, if a checkpoint copied frames and the WAL kept its generation
   after it, cut at the WAL end at that checkpoint; else cut at byte 32. The database file is
   unsynced (by rule) at OFF after any checkpoint and at any level after a partial checkpoint.

### Correction 5

5. **9.2(b) at `synchronous=OFF` the model does not apply at all.** With a checkpoint in the run,
   OFF leaves 44 `pwrite64`s to the database file and 306 to the `-wal` that were never synced (0
   syncs of either file in the whole run). The outline's model ("remove the WAL bytes after the
   last sync") assumes the database file is synced; at OFF it is not, so a power cut can leave any
   mixture of old and new database pages — outside what a file-level model can show. The
   experiment says so and checks that only OFF is in this situation. (The outline's claim for (b)
   names only NORMAL and FULL, so this is an addition, not a contradiction.)

### Correction 6

6. **9.2(a): "recovered step ≥ last acknowledged step" is right, and the excess is at most one.**
   Killing on entry to the commit's `fsync` at FULL recovers the step *being* committed (its frames
   and commit frame were already written into the page cache): e.g. `kill at fsync #3 …
   acknowledged 3, recovered step 4`. The check is "recovered = last acknowledged or last + 1".

### Correction 7

7. **9.4: the outline's "a held read mark so the WAL cannot restart" needs one qualification.** A
   read transaction that starts while the WAL is fully checkpointed uses read-lock slot 0 and does
   **not** block a restart (`walTryBeginRead`, wal.c:3020-3272: slot 0 when
   `nBackfill == mxFrame`; `walRestartLog`, wal.c:3875-3915, takes exclusive locks on slots 1-4
   only). `walship` relies on this deliberately: after the shipper's own checkpoint copies every
   shipped frame, the next writer restarts the WAL (8 restarts in 8 cycles in 9.4 phase 1) and
   nothing unshipped can be lost, because the restart needs `nBackfill == mxFrame` and the
   shipper's mark limits every checkpoint to frames it already shipped. Details under walship.

### Correction 8

8. **9.4: salts and the checkpoint sequence number are not a reliable restart counter across
   connections.** `nCkpt` (WAL header bytes 12-15) is a per-connection field (`pWal->nCkpt`,
   wal.c:535): it is loaded only during recovery (wal.c:1456) and incremented by `walRestartHdr`
   (wal.c:2156). A fresh connection that restarts the WAL writes checkpoint sequence 1, whatever
   the old header said; and a writer whose `nCkpt` is 0 randomises **both** salts
   (wal.c:4095-4096) instead of incrementing salt-1. So "salt-1 + 1" cannot be used to prove that
   exactly one restart happened. `walship` treats a changed header as continuous only when the
   shipper held its read mark the whole time and its last checkpoint copied every frame; otherwise
   it takes a new snapshot.

### Correction 9

9. **Span check: every outline span is exact** (start line holds the symbol, end line is the
   closing brace): wal.c 1390-1614, backup.c 323-591, vacuum.c 143-427, btree.c 11144-11295, wal.c
   2199-2395, pager.c 3658-3704. `backup.c 688-734` covers three functions; SPANS.txt lists
   `backupUpdate` 688-712, `sqlite3BackupUpdate` 713-715, `sqlite3BackupRestart` 728-734 (the
   lines between are its comment). 15 more spans the notes cite are added (list in SPANS.txt).

### Correction 10

10. **Research report's `.backup` "database is locked" observation: reproduced by the adversarial
    suite (CAP-02a/b/c), not by a course experiment.** CAP-02a: CLI `.backup` with no `.timeout`
    racing fresh `sqlite3` writer processes (each the last connection at its close): 12 of 40 (run
    1: 9 of 40) backups failed with `Error: database is locked`. CAP-02b: the same race with one
    persistent idle connection kept open (so no writer is ever the last connection and
    `sqlite3WalClose` never takes its EXCLUSIVE lock): 0 of 40. CAP-02c: an EXCLUSIVE lock held
    deterministically makes `.backup` with no timeout and `sqlitex.Backup` fail with SQLITE_BUSY,
    and `.timeout 5000` waits and succeeds once the lock is released. That confirms the cause
    below. Source facts for Neon: the CLI's `.backup` loops `sqlite3_backup_step(p,100)` with no
    busy handler (docs/knowledge/determinism-and-rebuild.md §d); `sqlite3_backup_step` opens a
    read transaction on the source (backup.c:361-364) and a write transaction on the destination
    (backup.c:390), either of which returns SQLITE_BUSY without a busy handler; a closing WAL
    writer that is the last connection takes an EXCLUSIVE lock on the database file to checkpoint
    and delete the WAL (`sqlite3WalClose`, wal.c:2507-2571, lines 2520-2529), which blocks a new
    read transaction. The research probe's writers were fresh `sqlite3` processes (each one the
    last connection at its close), which fits that cause; it was not demonstrated here (listed
    under "not done").

Everything else in the chapter-9 section was confirmed; the [unprobed] parts (9.2 model, 9.3,
9.4 as a whole) are now probed and hold as corrected above.

