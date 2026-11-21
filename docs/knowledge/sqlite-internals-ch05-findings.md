---
title: "sqlite-internals chapter 5: WAL restart, salts, read marks and checkpoint modes as measured at 3.53.4"
summary: "What a WAL shipper or replayer must know: only TRUNCATE touches salts at checkpoint, every mode restarts at the next write, the new salt depends on the writing connection, a read-lock-0 reader blocks all backfill, read marks outlive readers."
tags: [sqlite, wal, checkpoint, sqlite-internals]
updated: 2026-11-21
verified: 2026-11-21 — Linux amd64, Go 1.26, SQLite 3.53.4 via modernc.org/sqlite v1.60.1; evidence in the sqlite-internals repo
---

# sqlite-internals chapter 5: WAL restart, salts, read marks and checkpoint modes as measured at 3.53.4

What a WAL shipper or replayer must know: only TRUNCATE touches salts at checkpoint, every mode restarts at the next write, the new salt depends on the writing connection, a read-lock-0 reader blocks all backfill, read marks outlive readers.

Source: the `sqlite-internals` course repo (github.com/nickstrad/walfleet), chunk `ch05`. Working notes: `agent_work/ch05/build-notes.md`; review and adversarial records in the same directory.

## Corrections to the outline

1. **"A restart is visible as new salts: salt-1 increments on restart" (5.4) is only half true.**
   - PASSIVE, FULL and RESTART never touch the salts or the WAL file. `walCheckpoint` calls
     `walRestartHdr` only for TRUNCATE (wal.c:2383). For every mode, the WAL restarts at the
     **next write** (`walRestartLog`, wal.c:3879-3896), and only if the WAL was completely
     backfilled (`nBackfill == mxFrame`, so `nBackfill > 0` at 3882) and no reader holds read
     locks 1-4. 5.4: after PASSIVE / FULL / RESTART with no reader, the WAL size, the file's
     salt-1 and the `-shm` salt-1 are all `same`; the next write shows `restarted yes`, salt-1 `+1`,
     checkpoint-seq `+1`.
   - The value written to the new WAL header comes from the **writing connection's own**
     `pWal->nCkpt` (wal.c:535, 4095-4096): if it is 0, the salts are re-randomized. `nCkpt` is per
     connection: it is set only by recovery from the WAL header (wal.c:1456) or by the
     connection's own `walRestartHdr` (wal.c:2156). So when connection C runs TRUNCATE and
     connection W (which never restarted the WAL) writes next, W's header has **fresh random
     salts and checkpoint-seq 0** (5.4: `TRUNCATE no reader … new value / same`). When the
     connection that ran TRUNCATE writes next, it is salt-1 `+1`, checkpoint-seq `+1` (5.4 last
     section). The `-shm` salt-1 is `+1` immediately after TRUNCATE in both cases (walRestartHdr
     writes the wal-index header).
   - Consequence for R6 (the shipper): detect a new WAL generation by **any change of
     (salt-1, salt-2)** in the WAL header, never by "salt-1 + 1", and not by the checkpoint
     sequence number.
2. **`RESTART` does not restart the WAL itself.** It backfills, then waits (busy handler) for
   readers on read locks 1..4 so that the *next writer* restarts (wal.c:2353-2392; the source
   comment at 2353-2356 says exactly this). The outline's wording "RESTART … restart" should say
   "makes sure the next write can restart".
3. **"A reader holds a snapshot by pinning a frame number" misses read lock 0 (5.3 part B,
   new).** A reader that begins when the WAL is fully backfilled (`nBackfill == mxFrame`) takes
   `WAL_READ_LOCK(0)` and reads only the database file (wal.c:3134-3163); it sets no read mark.
   Such a reader **does not** prevent the writer from restarting the WAL (walRestartLog only needs
   exclusive locks 1..4; 5.3 B: first write after it began: salt-1 `+1`, mxFrame 16), but it
   **blocks every backfill**: `walCheckpoint` must take `WAL_READ_LOCK(0)` exclusively before it
   copies a single page (wal.c:2259-2260), so `nBackfill` stays 0 while the WAL grows to 533
   frames / 2,195,992 bytes even though autocheckpoint ran on every commit past 200 frames. A
   stuck reader therefore grows the WAL without bound **whether or not it pinned a mark**.
4. **5.3 is EXACT throughout, not SHAPE for growth.** All step order is scripted (two
   connections in one goroutine, sampling through a child process, no sleeps), so the WAL size,
   mxFrame, nBackfill and read marks are identical on every run (two host runs, two container
   runs). The evidence file is checked as EXACT.
5. **Read marks outlive their readers.** After the reader ends, its value stays in
   `aReadMark[i]` (5.3 A: `reader ended … 0,22,555,…`); a mark is free when its lock (`-shm`
   byte 120+3+i) is free, and nothing in the `-shm` *contents* shows that. Decoding the marks
   tells you what snapshots exist or existed, not who is reading. (Who holds which lock is
   chapter 6, via `/proc/locks`.)
6. **"Replay a WAL onto a base from an earlier generation — must be detected" (Neon list for
   5.2) cannot be done from the two files alone.** Nothing in the database file records which
   WAL generation it has absorbed, and the WAL header carries only salts and its own sequence.
   `walfile.Replay` therefore cannot detect a stale base; detection needs capture metadata
   recorded at capture time (e.g. the WAL salts and the base's sha256 written together). Not
   built in this chunk (it belongs to `internal/walship`, chapter 9). The determinism report's
   Q-c item 9 shows such a replay gives a corrupt result.
7. Source-comment discrepancy for the chapter: `walDecodeFrame`'s comment says the checksum
   covers "the first 16 bytes of this frame-header" (wal.c:1033); the code checksums 8 bytes
   (wal.c:1038, and the same in `walEncodeFrame` at 985). The file-format comment at wal.c:65-68
   ("the first 8 bytes") is right. Cite the code.
8. The known `pager.c` 607-617 discrepancy is confirmed: that comment puts checkpoint sync flags
   in the low two bits and commit flags in bits 0x04/0x08; `wal.h:25-26` and the code
   (`pager.c:3692-3695`: `walSyncFlags = syncFlags<<2`, `|= syncFlags` only when `fullSync`) do
   the reverse: low bits = commit sync (`WAL_SYNC_FLAGS`), bits 2-3 = checkpoint sync
   (`CKPT_SYNC_FLAGS`).

Everything else in the chapter-5 section was confirmed (details per experiment). The one
[unprobed]-in-part claim (5.4) is corrected in items 1-2.

