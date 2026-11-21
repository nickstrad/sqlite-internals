---
title: "sqlite-internals chapter 9: backup, crash recovery, integrity and point-in-time capture measured at 3.53.4"
summary: "Backup-API remaining count rises when the source grows; VACUUM INTO of a WAL database is a rollback-mode file; process crashes lose nothing at any synchronous level while the power-cut model loses the commits since the last checkpoint at NORMAL; integrity_check misses unindexed values and freelist bytes; a WAL shipper with a held read mark restores every commit byte for byte."
tags: [sqlite, backup, wal, durability, integrity, walship, sqlite-internals]
updated: 2026-11-21
verified: 2026-11-21 — Linux amd64 (host and Debian lab container), Go 1.26, SQLite 3.53.4 via modernc.org/sqlite v1.60.1; darwin cross-built only; evidence in the sqlite-internals repo
---

# sqlite-internals chapter 9: backup, crash recovery, integrity and point-in-time capture measured at 3.53.4

Source: the `sqlite-internals` course repo, chunk `ch09`. Working notes:
`agent_work/ch09/build-notes.md`. Experiments: `ch09-backup-crash-recovery-integrity/experiments/9.1`-`9.4`;
shared package `internal/walship` (rung R6).

## Findings

1. **Backup API progress:** `sqlite3_backup_remaining` is recomputed from the current source size
   (backup.c:428-429), so it rises when the backup's own connection commits and grows the
   database (90 → 93 → 98) even though nothing restarted. Track `pagecount − remaining` (= pages
   copied): it advanced 5 per step through two own-connection commits and fell to 5 after one
   commit by another connection (restart, `sqlite3BackupRestart`). With another connection
   committing after every step the backup never finished (8 steps). The finished copy is E2-equal
   to its recipe state (differs at bytes 27, 43, 95).
2. **`VACUUM INTO` of a WAL database writes a rollback-mode file**: the course's E3 logical hash
   (which includes `journal_mode`) differs in that pragma only; canonical forms compare at E1.
3. **A plain copy of a live WAL database file (no `-wal`) is a valid, stale database** (equalled
   the step before the writer's last commit), and nothing reports it.
4. **Process crash vs power cut (WAL, one writer connection, steps 4-11, checkpoint after 7):**
   the files a killed process leaves recover to the last acknowledged step at OFF, NORMAL and FULL;
   30 strace kill points (every sync, 6 spread pwrite64s) never gave a state outside the recipe,
   and a kill inside a FULL commit sync keeps that commit (recovered = acknowledged + 1). The
   power-cut model (drop every byte written after the file's last sync): NORMAL keeps only the
   restarted WAL's 32-byte header after the checkpoint and loses steps 8-11; FULL loses nothing;
   OFF writes the database file at checkpoint with no sync at all, so the model does not apply.
   The sync rule (pager.c:3658-3704, wal.h:25-26): OFF never syncs; NORMAL syncs the WAL at
   checkpoints and on every new WAL header; FULL also at every commit. strace agreed with the rule
   at all three levels.
5. **`integrity_check` does not prove content:** a flipped byte in an unindexed column and in a
   freelist leaf both pass integrity_check and quick_check; quick_check also misses a row/index
   mismatch (it skips index checks). Only a per-page hash manifest caught all four sites tried.
   The "row N" in "row N missing from index" is a scan counter, not the rowid.
6. **The WAL checkpoint sequence number and salt-1 + 1 are per-connection behaviour**, not a
   restart counter: a fresh connection that restarts the WAL writes sequence 1, and a writer
   whose `nCkpt` is 0 randomises both salts (wal.c:4095-4096).
7. **Continuous WAL shipping (R6) works with a held read mark plus shipper-owned checkpoints:**
   every cycle under `BEGIN IMMEDIATE`, read the `-wal` from the file (SQLite locks nothing on the
   `-wal`, so reading it in the owning process is safe — a stray close of the `-wal` kept the
   writer lock, a stray close of the `-shm` did not), ship, PASSIVE checkpoint, re-pin. A pin on
   read slot 0 does not block a WAL restart, but a restart then loses nothing. All 17 positions of
   the 9.4 run restored byte-identical to the recipe; an outside TRUNCATE checkpoint while the
   shipper was down cost exactly the two commits it folded away, detected via the WAL header.
