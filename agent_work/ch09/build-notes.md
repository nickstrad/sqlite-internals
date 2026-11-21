# Chunk 9 build notes (plain data for the prose pass)

Builder: Claude Opus 5.5, branch `chunk/ch09`, 2026-11-21. Machine: linux/amd64, Go 1.26, strace
6.8, ext4 under `_work/`. Pinned tree `$SQLITE_SRC` = SQLite 3.53.4. darwin: cross-built and
cross-vetted only, never run. Every experiment: run from the repo root, EXACT evidence, two
consecutive runs identical (`go run ./cmd/course check evidence ch09` → 4 × PASS).

Chapter directory `ch09-backup-crash-recovery-integrity/`: `backup.c`, `vacuum.c` (whole copies,
`SOURCES.sha256`), `SPANS.txt` (24 spans), `experiments/9.1`–`9.4`, `evidence/`. Linked, not
copied (as the outline says): `pager.c`, `wal.c`, `wal.h`, `btree.c`, `pragma.c`.

## Corrections to the outline

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
   (backup.c:728-734).
2. **9.1: `VACUUM INTO` of a WAL database is not E3-equal to its source under the course's E3
   definition.** `equal.LogicalPragmas` includes `journal_mode`; the `VACUUM INTO` file is a
   rollback-mode file (`journal_mode` `rollback` vs `wal`, already noted by chunk 3). Every other
   part of the logical hash is equal. The experiment reports "E3 except journal_mode" and "E1 after
   VACUUM INTO" (canonical form of the capture equals the canonical form of the recipe state) as
   the levels it reaches. Section 4.2's table ("VACUUM INTO: E3") should read "E3 except
   `journal_mode`; E1 between canonical forms".
3. **9.1: the backup API's E2 difference is bytes 27, 43, 95 here** (change counter low byte, schema
   cookie low byte, version-valid-for low byte) — within the declared ignore list 24-27, 40-43,
   92-99, as section 4.2 says ("24, 40, 92" names the field starts).
4. **9.2(b): the model is portable, and is computed from the rule in the source, cross-checked by
   strace on Linux.** The outline says the model takes "the WAL bytes written after the WAL's last
   sync" from "the strace record of an uninterrupted run" — strace is Linux-only, but the brief
   requires the model half to be portable. The experiment therefore computes the cut from the
   sync rule read from `pager.c:3658-3704` and `wal.c` (below), and on Linux checks that strace's
   record of the same run gives exactly the same cut (`trace.SyncsOf`). Both agree at all three
   levels. On a Mac the rule half runs and the strace half prints a `LINUX-ONLY:` line.
5. **9.2(b) at `synchronous=OFF` the model does not apply at all.** With a checkpoint in the run,
   OFF leaves 44 `pwrite64`s to the database file and 306 to the `-wal` that were never synced (0
   syncs of either file in the whole run). The outline's model ("remove the WAL bytes after the
   last sync") assumes the database file is synced; at OFF it is not, so a power cut can leave any
   mixture of old and new database pages — outside what a file-level model can show. The
   experiment says so and checks that only OFF is in this situation. (The outline's claim for (b)
   names only NORMAL and FULL, so this is an addition, not a contradiction.)
6. **9.2(a): "recovered step ≥ last acknowledged step" is right, and the excess is at most one.**
   Killing on entry to the commit's `fsync` at FULL recovers the step *being* committed (its frames
   and commit frame were already written into the page cache): e.g. `kill at fsync #3 …
   acknowledged 3, recovered step 4`. The check is "recovered = last acknowledged or last + 1".
7. **9.4: the outline's "a held read mark so the WAL cannot restart" needs one qualification.** A
   read transaction that starts while the WAL is fully checkpointed uses read-lock slot 0 and does
   **not** block a restart (`walTryBeginRead`, wal.c:3020-3272: slot 0 when
   `nBackfill == mxFrame`; `walRestartLog`, wal.c:3875-3915, takes exclusive locks on slots 1-4
   only). `walship` relies on this deliberately: after the shipper's own checkpoint copies every
   shipped frame, the next writer restarts the WAL (8 restarts in 8 cycles in 9.4 phase 1) and
   nothing unshipped can be lost, because the restart needs `nBackfill == mxFrame` and the
   shipper's mark limits every checkpoint to frames it already shipped. Details under walship.
8. **9.4: salts and the checkpoint sequence number are not a reliable restart counter across
   connections.** `nCkpt` (WAL header bytes 12-15) is a per-connection field (`pWal->nCkpt`,
   wal.c:535): it is loaded only during recovery (wal.c:1456) and incremented by `walRestartHdr`
   (wal.c:2156). A fresh connection that restarts the WAL writes checkpoint sequence 1, whatever
   the old header said; and a writer whose `nCkpt` is 0 randomises **both** salts
   (wal.c:4095-4096) instead of incrementing salt-1. So "salt-1 + 1" cannot be used to prove that
   exactly one restart happened. `walship` treats a changed header as continuous only when the
   shipper held its read mark the whole time and its last checkpoint copied every frame; otherwise
   it takes a new snapshot.
9. **Span check: every outline span is exact** (start line holds the symbol, end line is the
   closing brace): wal.c 1390-1614, backup.c 323-591, vacuum.c 143-427, btree.c 11144-11295, wal.c
   2199-2395, pager.c 3658-3704. `backup.c 688-734` covers three functions; SPANS.txt lists
   `backupUpdate` 688-712, `sqlite3BackupUpdate` 713-715, `sqlite3BackupRestart` 728-734 (the
   lines between are its comment). 15 more spans the notes cite are added (list in SPANS.txt).
10. **Research report's `.backup` "database is locked" observation: not reproduced in this
    chunk.** Source facts for Neon: the CLI's `.backup` loops `sqlite3_backup_step(p,100)` with no
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

## Shared packages

### Changes to existing packages (all backward compatible, each with tests)

| Package | Change | Why |
| --- | --- | --- |
| `internal/procs` (`sqlrole.go`) | new SQL-role action `recipe-session K`: opens a **fresh** connection with the role's settings (`SingleConnection`, `_txlock=immediate`), commits recipe step K, closes it; the role's own held connection stays open | a writer whose every step is its own connection session reproduces the canonical recipe's change counter (ch01 correction 1), so restored states can be compared at E1 with `recipe.StepHashes`; the held connection keeps the WAL alive (the closing session is never the last connection, so it does not checkpoint). Test `TestRecipeSessionCommitsOnAFreshConnection`: steps 2 and 3 committed this way leave a non-empty `-wal`, `user-version` 3, re-applying 3 fails, a bad argument is `CodeNotSQLite`, and after the child exits the file equals recipe step 3 **exactly** |
| `internal/sqlitex` (`raw.go`) | `BackupWithSourceExec(ctx, source, dest, pagesPerStep, afterStep func(BackupStep, SourceExecer) error)`; `SourceExecer` interface (`ExecContext(ctx, query, args...)`); `Backup` now calls it (same behaviour) | inside `Conn.Raw` the `*sql.Conn` is locked, so a write "through the backup's own connection" must use the raw driver connection; the execer converts args with `driver.DefaultParameterConverter` and calls the driver's `ExecContext`. Test `TestBackupWithSourceExecMirrorsSameConnectionWrites`: an UPDATE at step 2 through the execer; copied count never falls; the copy holds the update; an unconvertible argument is an error |
| `internal/dbfile` (`record.go`) | `RecordColumn(payload, column) (start, length, serialType, err)` — byte range of one column inside a record | 9.3 flips one byte inside a chosen column. Errors on a header size outside the payload, a reserved serial type, a column past the end or a body shorter than the column. Test `TestRecordColumn` |
| `internal/capture` (`capture.go`) | `VerifyImage(manifest, image) ([]int, error)` — sha256 of every page vs the manifest, returns differing page numbers; errors on page size ≤ 0, an out-of-sequence manifest, or an image whose size is not `pages × page size` | `BuildManifest` runs a full census and refuses a structurally damaged file (it did in 9.3: "46 bytes of the cell content area belong to no cell"), so the verifier a platform runs on a suspect file must hash pages without parsing them. Test `TestVerifyImageNamesChangedPages` |
| `internal/trace` (`sync.go`, portable) | `SyncsOf(calls, path) SyncRecord{Path, Syncs, LastSync, Unsynced, FirstUnsynced, HasUnsynced, UnfinishedSeen}` and `String()` | which writes to a file came after its last successful `fsync`/`fdatasync` (writes = pwrite64, pwrite, write, writev, ftruncate; failed calls ignored; an ftruncate counts at its new length; an unfinished call is noted, not counted). Test `TestSyncsOf` |

No other shared package changed. `go.mod` untouched.

### New package `internal/walship` (about 1,420 lines incl. 710 of tests; no comments)

What it is: rung R6. A shipper that runs in the process that owns the database, captures a base
snapshot and then every committed WAL frame into numbered, hash-chained segments in an object
store, checkpoints only what it has shipped, and a restore that rebuilds any commit position from
a snapshot plus segments, replaying through chapter 5's `walfile.Replay`.

**Store interface (`store.go`) — the contract chapter 10 implements with SeaweedFS S3.**

```
type Store interface {
	PutIfAbsent(ctx, key string, data []byte) error   // ErrExists if the key exists
	Get(ctx, key string) ([]byte, error)              // ErrNotFound
	List(ctx, prefix string) ([]string, error)
	Delete(ctx, key string) error
}
```

Semantics walship relies on (write these into chapter 10's S3 adapter):
- **PutIfAbsent is atomic and exclusive**: readers see the whole object or none of it; if the key
  exists the call fails with `ErrExists` and the stored object is unchanged. S3: `PutObject` with
  `If-None-Match: *` (SeaweedFS 4.48: 412 PreconditionFailed on an existing key, exactly one winner
  among 2 and 8 concurrent writers — `agent_work/probes/seaweedfs-s3/RESULT.md`). Objects are never
  overwritten; there is no update operation.
- **Read-after-write and list-after-write**: after PutIfAbsent returns nil, Get and List see the
  object (SeaweedFS: 0 stale reads in 100 rounds, 0 missing in 50 list rounds).
- **List returns every key with the prefix in no promised order**; walship sorts keys itself and
  never relies on store order (SeaweedFS lists `k/a`, `k/a/x`, `k/a-` out of byte order). Keys are
  `snapshots/NNNNNNNNNNNN` and `segments/NNNNNNNNNNNN` (12 fixed-width decimal digits), so byte
  order and numeric order agree anyway and no key mixes `/` with bytes below 0x2f.
- **Delete is idempotent** (deleting a missing key returns nil, like S3 DeleteObject); used only by
  `Prune` and by tests/experiments that damage a store copy.
- Keys: `^[a-z0-9][a-z0-9_-]*(/[a-z0-9][a-z0-9_-]*)*$` (`ValidateKey`, `ErrInvalidKey`); so no
  `..`, no leading or trailing `/`, no empty segment, no dot-files.
- Context cancellation is checked before each call.

`DirStore` (local directory, used here): `NewDirStore(root)`; PutIfAbsent writes a temp file
under `<root>/.tmp/`, fsyncs it, `os.Link`s it to the final path (link fails with EEXIST if the key
exists → `ErrExists`; this is the atomic put-if-absent), removes the temp, fsyncs the directory.
Get = read file. List walks the tree, skipping dot-directories, returns sorted keys. Delete removes
the file and fsyncs the directory. Test `TestDirStoreSemantics` (exists, not found, list prefix,
no temp files left, delete twice, put after delete, nine invalid keys, cancelled context).

**Objects (`format.go`).** One object per snapshot or segment, so one atomic put each:
line 1 `walship snapshot v1` / `walship segment v1`, line 2 the metadata as one JSON object
(decoded with `DisallowUnknownFields`, exactly one value), then the raw body. Metadata records
`body_bytes` and `body_sha256`; decoding refuses a body of another size or hash
(`ErrMalformedObject`). Error texts carry no salt- or checksum-derived numbers, so they are
stable evidence.

- `SnapshotMeta{Position, Reason (initial|periodic|discontinuity), PageSize, Pages, UserVersion,
  StateSHA256, WAL{Generation, Frame, Checksum}, BodyBytes, BodySHA256}`; body = the database
  image (all pages). Decoding also checks `Pages × PageSize = len(body)` and
  `StateSHA256 = sha256(body)`.
- `SegmentMeta{FirstPosition, LastPosition, Generation, FirstFrame, LastFrame, SeedChecksum,
  LastChecksum, PageSize, BeforeSHA256, AfterSHA256, Commits[]{Position, Frame, Pages,
  UserVersion, StateSHA256}, BodyBytes, BodySHA256}`; body = the raw WAL frames FirstFrame..LastFrame
  exactly as SQLite wrote them (24-byte frame headers with their salts and cumulative checksums,
  plus page images). Decoding checks: generation header is 32 bytes; page size in range; body =
  (LastFrame−FirstFrame+1) × (page size + 24); one commit point per position, positions
  consecutive, commit frames strictly increasing, the last frame is a commit frame, `AfterSHA256`
  = the last commit point's state hash.
- `Generation{Header}` = the 32-byte WAL header (hex). Two WAL "generations" are different files'
  worth of frames separated by a restart (new salts). `GenerationNames` maps generations to
  `WAL generation #n` in order of appearance, because salts are random per run and may not appear
  in EXACT evidence.
- **Position** = the commit's place in the shipped stream. The first snapshot gets `Config.Origin`
  (9.4 uses 2 = the recipe step it starts from); every shipped commit is the next position; a
  discontinuity snapshot takes the next position too (it stands for "whatever happened since").
  `UserVersion` (header bytes 60-63 of the state's page 1) is recorded for every commit point;
  9.4 maps positions to recipe steps through it (the recipe sets `user_version = k` in step k).
- **State hash** ("page-set hash" in the outline) = sha256 of the whole database image at that
  commit — byte-for-byte what the at-rest file would be after a checkpoint, so it is directly
  comparable with `recipe.StepHashes`. `BeforeSHA256` of a segment = the state before its first
  commit, `AfterSHA256` = after its last: segment *n*'s before = segment *n−1*'s after (or the
  snapshot's state), which is the hash chain.

**Frames and replay (`apply.go`).**
- `VerifySegmentFrames(meta, frames)`: re-walks SQLite's own WAL checksum chain from
  `SeedChecksum` (the cumulative checksum of the frame before FirstFrame, or the WAL header's
  checksum for frame 1) with `walfile.ChecksumBytes`, checks each frame's salts equal its
  generation's, page number ≠ 0, stored checksum = computed, and that the chain ends at
  `LastChecksum` (`ErrChecksumChain`).
- `StandaloneWAL(meta, frames, throughFrame)`: builds a self-contained WAL file — the
  generation's real 32-byte header followed by frames FirstFrame..throughFrame with their
  checksums **re-chained from the header's checksum** (the original chain started earlier in the
  generation, so the frames would not validate on their own). `walfile.Parse` accepts it with
  every frame valid (test `TestStandaloneWALIsAValidWAL`).
- `ApplySegment(image, meta, frames, throughFrame)`: verify, build the standalone WAL, run
  **`walfile.Replay(image, wal)`** (chapter 5's replayer: applies committed frames in order, skips
  pages beyond the commit size, truncates/extends to the commit size), and check that the replay
  saw exactly the expected number of valid frames with the last one a commit.

**Catalog and restore (`catalog.go`).**
- `LoadCatalog(ctx, store)`: lists both prefixes, gets and decodes every object. A key whose name
  disagrees with its metadata position, a malformed object, or a segment whose frames fail the
  checksum chain goes into `Damaged{Key, Kind, Problem}` instead of the usable lists.
- `Catalog.Plan(target)` / `PlanFrom(snapshotPosition, target)`: newest usable snapshot ≤ target
  (or the one named); then segments starting exactly at position+1, +…; each must continue the
  previous state (`BeforeSHA256` = previous state hash) and the previous WAL position (same
  generation: `FirstFrame` = previous frame + 1 and `SeedChecksum` = previous `LastChecksum`; new
  generation: `FirstFrame` = 1 and seed = that header's checksum). The target must be one of the
  segment's commit points (positions exist only at commits, so a restore "inside a transaction" is
  impossible by construction). Errors: `ErrNoSnapshot`, `ErrBrokenChain` ("no segment starts at
  position N", "the segment starting at position N is damaged: …", "its before hash … is not the
  previous state …"), `ErrNoCommit`. **All of this runs before any page is applied.**
- `Plan.Execute()`: copies the snapshot image and applies each segment with `ApplySegment`
  (through the target's commit frame for the last one), checking the state hash after every
  segment and at the target (`ErrBrokenChain` on mismatch).
- `Prune(ctx, store, keepFrom)`: keeps the newest snapshot ≤ keepFrom and everything after it;
  deletes older snapshots and segments that end at or before it; returns the deleted keys
  (test `TestPruneKeepsWhatRestoresNeed`: snapshots 2,4,6,8; prune at 7 deletes snapshots 2,4 and
  segments 3-6; positions 6-9 still restore; 5 now fails with `ErrNoSnapshot`).
- `CopyStore(ctx, from, to)`, `WriteDatabase(path, image)` (refuses an existing file or any
  `-wal`/`-shm`/`-journal` beside it; fsyncs).

**The shipper (`shipper.go`).** `Open(ctx, Config{Database, Store, Options, SnapshotEvery,
Origin})` opens the database with the given file settings (must be WAL, else `ErrNotWAL`),
forcing a pool (not a single connection), deferred transactions and `wal_autocheckpoint=0` on its
own connections. It holds two connections: **control** and **pin**.

- **Pin = the held read mark.** `BEGIN` + `SELECT count(*) FROM sqlite_schema` on the pin
  connection starts a read transaction and keeps it open between cycles.
- **Every cycle runs under the write lock**: `BEGIN IMMEDIATE` on the control connection, so no
  commit can happen while the shipper reads the `-wal`, ships, checkpoints and re-pins; then
  `COMMIT` (nothing was written). A writer that wants to commit meanwhile waits in its busy
  timeout. Cost: the writer is blocked for one cycle (a file read, one store put, one checkpoint).
- **Reading the `-wal` in the owning process is safe**: SQLite takes no POSIX locks on the
  `-wal` file (locks live on the database file and the `-shm`), so an `os.ReadFile` of the `-wal`
  and its close drop nothing. Shown by test `TestReadingTheWALInAProcessWithAConnectionKeepsItsLocks`:
  a child holding `BEGIN IMMEDIATE` does a stray open+close of its `-wal`; a second child's
  `BEGIN IMMEDIATE` still gets SQLITE_BUSY (5); after a stray close of the `-shm` the second child
  gets SQLITE_OK (chapter 6's hazard).
- **Cycle(ctx) → CycleReport{Change, Reason, Segment, Snapshot, Checkpoint, FullBackfill}**:
  1. read the `-wal`, parse with `walfile.Parse` (missing or 0-byte file = no generation).
  2. classify: same header as the last shipped position → check the shipped frame still carries
     the shipped checksum and the WAL still holds at least that many committed frames
     (`WALUnchanged`, else discontinuity). Different header → `WALNewGeneration` only if the
     shipper has held its pin continuously since the last cycle **and** (the old position had no
     WAL, or the last shipper checkpoint copied every shipped frame); otherwise
     `WALDiscontinuity`.
  3. discontinuity → take a snapshot (reason `discontinuity`, position = last + 1) and ship
     nothing; else ship frames (last shipped + 1 .. last commit frame in the file) as one segment,
     computing each commit point's state by replaying the segment onto the current image.
  4. end the pin, run `PRAGMA wal_checkpoint(PASSIVE)` on the pin connection, record
     `FullBackfill = !busy && log == checkpointed == frames shipped in this generation`, re-pin.
  5. if `SnapshotEvery` segments were shipped since the last snapshot, take a periodic snapshot.
- **Snapshots** read every page with `SELECT pgno, data FROM sqlite_dbpage('main') ORDER BY
  pgno` on the control connection inside its `BEGIN IMMEDIATE` (`ReadPages`, exported; refuses a
  gap in page numbers or a page of the wrong size). Under the write lock that is exactly the last
  commit in the `-wal`. A periodic snapshot must hash to the shipper's replayed state at the same
  WAL position, else `ErrMirrorDiverged` — a built-in check of the replay against SQLite's own
  pages on every periodic snapshot.
- **Why no frame can be lost while the pin is held** (the argument the design rests on; facts
  from wal.c): a checkpoint never copies past the mark of a reader holding slot ≥ 1
  (`mxSafeFrame`, wal.c:2233-2251); the pin is taken right after the shipper shipped everything
  in the `-wal` under the write lock, so every mark it can hold is ≤ the frames shipped. A restart
  needs `nBackfill == mxFrame` and exclusive locks on slots 1-4 (`walRestartLog`, wal.c:3875-3915),
  so it can only happen when every frame is already copied, i.e. shipped. A pin on slot 0 (taken
  when the WAL was fully copied) does not stop a restart but does stop every backfill, because
  backfilling takes slot 0 exclusively (wal.c:2259-2260); so at most one restart can happen
  between two cycles and it loses nothing. Without the pin (the shipper stopped or crashed) none
  of this holds, hence the discontinuity rule.
- **Resume**: `Open` on a store that already has objects loads the catalog, restores the newest
  position (snapshot + segments) as the current image, takes the last WAL position from the last
  segment (or snapshot), and marks itself not continuous. If the `-wal` still has that generation
  with the shipped frame intact, shipping continues where it stopped (test
  `TestResumeInTheSameGenerationContinues`: a reader at step 3 keeps the WAL from restarting, the
  shipper is closed after shipping 4, steps 5 and 6 are committed, the reopened shipper ships 5-6
  as one segment, every position 2-6 restores to the recipe exactly). If the header changed
  (test `TestResumeAfterARestartTakesASnapshot`, and 9.4 phase 2), it takes a discontinuity
  snapshot.
- Two shippers on one store would collide on put-if-absent only when they pick the same key;
  walship assumes one shipper per database (ownership and fencing are chapter 10's job).
- A cycle that fails marks the shipper broken; later cycles refuse until it is reopened (and so
  resumes from the store).

Tests (`go test ./internal/walship`, about 10-18 s): ship every commit of steps 3-12 in batches
and restore every position from every snapshot (E1 vs `recipe.StepHashes`); nothing new ships
nothing; resume in the same generation; resume after a restart; an outside TRUNCATE checkpoint
while the shipper is closed forces a discontinuity snapshot at the next position that holds step
6, the next segment holds step 7, and planning from the first snapshot across the gap is
`ErrBrokenChain`; damaged stores (missing segment, swapped segment objects, segments re-keyed with
consistent metadata, flipped body byte, flipped byte with a recomputed body hash → caught by the
WAL checksum chain); standalone WAL validity; prune; store semantics; key parsing; strict object
decoding (8 malformations); the `-wal` lock test.

## What each span does (facts read from the copied/linked source)

- **wal.c 1390-1614 `walIndexRecover`.** Rebuilds the wal-index from the `-wal` file when the
  `-shm` header is missing or invalid (first open after a crash). Takes the exclusive
  checkpoint/recover locks, reads the 32-byte header and ignores the whole WAL if the magic, page
  size or header checksum is wrong (wal.c:1447-1467) and refuses a version other than 3007000
  (1472). Then decodes frames in order with `walDecodeFrame`, stops at the first frame whose salt,
  page number or cumulative checksum fails, and sets `mxFrame`/`nPage` only at commit frames
  (1517-1519), so a torn or uncommitted tail is simply not part of the database. Finally resets
  `nBackfill` and the read marks and logs `SQLITE_NOTICE_RECOVER_WAL` when frames were recovered.
- **backup.c 323-591 `sqlite3_backup_step`.** Returns SQLITE_BUSY at once if the source
  connection is itself inside a write transaction (backup.c:347-351). Opens a read transaction on
  the source if none is open and remembers to close it at the end (361-364, 573-577), so each call
  of a stepped backup is its own read transaction. Takes a write transaction on the destination
  (390), copies up to nPage pages starting at `iNext` (415-425), recomputes `nPagecount` and
  `nRemaining = nSrcPage + 1 - iNext` (427-429), and attaches itself to the source pager so later
  writes reach it (`attachBackupObject`, 432). When done it sets the destination schema cookie to
  its old value + 1 (448), sets the WAL version bytes if the destination is WAL (454-456),
  truncates the destination and commits it.
- **backup.c 688-712 `backupUpdate`, 713-715 `sqlite3BackupUpdate`.** Called from the source
  pager whenever *its own* connection writes a page (WAL mode: `pagerWalFrames`, pager.c:3270;
  rollback: pager.c:4542). If that page was already copied (`iPage < p->iNext`) it is copied
  again into the destination; `iNext` does not change. That is why same-connection writes do not
  restart a backup.
- **backup.c 728-734 `sqlite3BackupRestart`.** Sets `iNext = 1` for every backup attached to the
  pager. Called from `pager_reset` (pager.c:1785), which runs when the pager finds the file changed
  by another connection, and on a WAL rollback (3185) and a commit that writes no pages (6543).
- **vacuum.c 143-427 `sqlite3RunVacuum`.** For `VACUUM INTO 'file'` (pOut ≠ NULL): refuses
  inside a transaction or with statements running; attaches the output as a new database
  (`ATTACH %Q AS vacuum_<random>`, 226) with `synchronous=OFF` for the copy; begins a **read**
  transaction on the source for INTO (`sqlite3BtreeBeginTrans(pMain, pOut==0 ? 2 : 0, 0)`, 270 —
  a write transaction only for plain VACUUM); recreates tables, indexes and the rest in
  `sqlite_schema` order and copies rows with `INSERT INTO … SELECT * FROM …`; copies the meta
  values with the schema cookie + 1; and for INTO skips `sqlite3BtreeCopyFile` (379-381), so the
  source file is untouched. The output is a fresh, compact, rollback-mode file.
- **btree.c 11144-11295 `sqlite3BtreeIntegrityCheck`.** Walks the freelist (`checkList` from the
  page-1 header fields at 32 and 36, 11206-11210), then every b-tree root with `checkTreePage`
  (page headers, cell bounds, key order, overflow chains, free-space accounting), then reports any
  page that nothing references ("Page N: never used"). It checks structure and accounting; it
  never reads the bytes of a freelist leaf page or compares row values with anything. Row-versus-
  index comparisons are generated separately in pragma.c (below).
- **pragma.c 1850-1857 and 2084-2090.** In `PRAGMA integrity_check`, register 7 counts rows as
  the table is scanned (`OP_AddImm 7, 1` at 1857); the message "row N missing from index X" puts
  that counter, not the rowid, in N (2084-2090). 9.3 shows it: the changed row has rowid 76 and the
  message says "row 66". `quick_check` skips the index checks (`if( !isQuick )`, pragma.c:2058).
- **wal.c 2199-2395 `walCheckpoint`.** Computes `mxSafeFrame` = the lowest read mark held by a
  reader in slots 1-4 (2233-2251), backfills frames up to it while holding read-lock slot 0
  exclusively (2259-2260), **syncs the WAL first** (2277) and the database file after writing
  (2333), and truncates the database file when everything was copied. RESTART/TRUNCATE then wait
  for readers and reset the header (`walRestartHdr`, 2383; TRUNCATE also truncates the `-wal` to
  0 bytes). Its sync flags are `CKPT_SYNC_FLAGS` (bits 2-3).
- **pager.c 3658-3704 `sqlite3PagerSetFlags`.** OFF → `noSync`, `syncFlags = 0`. NORMAL and
  above → `syncFlags = SQLITE_SYNC_NORMAL` (or FULL with `fullfsync`). `walSyncFlags =
  syncFlags << 2` (the checkpoint/header bits) for every level except OFF, and `| syncFlags` (the
  commit bits) only at FULL and EXTRA. With `wal.h:25-26` (`WAL_SYNC_FLAGS(X) = X & 3`,
  `CKPT_SYNC_FLAGS(X) = (X >> 2) & 3`) that gives the rule 9.2 uses: OFF never syncs; NORMAL syncs
  the WAL at checkpoints and when writing a new WAL header; FULL also syncs at every commit.
- **wal.c 4038-4266 `walFrames`.** Restarts the log if possible (`walRestartLog`, 4079); when it
  writes frame 1 it writes the 32-byte header with fresh salts if `nCkpt == 0` (4095-4096) and
  **syncs the header** if `syncHeader` (4121-4124, `CKPT_SYNC_FLAGS`); at a commit with
  `WAL_SYNC_FLAGS ≠ 0` it syncs after the commit frame (4196-4213).
- **wal.c 3938-3960 `walWriteToLog`.** Splits a frame write around a sync point when sector
  padding is on (not used on unix with powersafe overwrite).
- **wal.c 2152-2166 `walRestartHdr`.** Increments `nCkpt`, sets `mxFrame = 0`, salt-1 + 1 and a
  new random salt-2, writes the wal-index header, sets `nBackfill = 0` and the read marks.
- **wal.c 3875-3915 `walRestartLog`.** Called by the writer before its first frame: if its read
  lock is slot 0 and `nBackfill > 0`, it tries exclusive locks on read slots 1-4; if it gets them
  it restarts the header (new frames overwrite the WAL from the start), otherwise the WAL keeps
  growing. Then it re-takes a read lock.
- **wal.c 3020-3272 `walTryBeginRead`.** A reader uses slot 0 (read the database file only) when
  the WAL is fully backfilled; otherwise it uses or claims a slot 1-4 whose mark is its `mxFrame`.
- **wal.c 2507-2571 `sqlite3WalClose`.** On close, if an EXCLUSIVE lock on the database file can be
  obtained (no other connection), checkpoints, and deletes the WAL unless `PERSIST_WAL`.
- **backup.c 21-42 `struct sqlite3_backup`, 226-228 `isFatalError` (BUSY and LOCKED are not
  fatal), 652-660 `sqlite3_backup_remaining`, 666-674 `sqlite3_backup_pagecount`.**
- **pager.c 1783-1787 `pager_reset`** (bumps `iDataVersion`, restarts attached backups, clears the
  cache), **3225-3282 `pagerWalFrames`** (writes the change counter when page 1 is in the commit,
  then forwards every frame's page to `sqlite3BackupUpdate`).

## Experiment 9.1 capture-comparison

Run: `go run ./ch09-backup-crash-recovery-integrity/experiments/9.1-capture-comparison`.
Runtime ~5 s. Evidence 93 lines. Portable (no Linux-only part).

Setup: reference files `_work/9.1/ref/step-NN.db` for steps 0-22 (one canonical build, a copy
after each step). Comparisons: E1 `equal.CompareFiles`, E2 `equal.ComparePageFiles` (declared
header ignore list), E3 `equal.CompareLogicalFiles`, "E3 except journal_mode" (every logical
difference mentions `journal_mode`), "E1 after VACUUM INTO" (for a VACUUM INTO capture: its bytes
vs `VACUUM INTO` of the reference; for others the same as E3).

Part A (quiet database = recipe step 6, closed copy): five captures — closed-file copy, backup API
(one step, `-1`), `VACUUM INTO`, every page via `sqlite_dbpage` inside one read transaction, file
+ `-wal` copied by a child process inside a read transaction then `walfile.ReplayFiles`. Printed
lines `<method> vs the source: E1 … E2 … E3 … E3-except-journal_mode … E1-after-VACUUM-INTO …`:

| method | E1 | E2 | E3 | E3 ex. journal_mode | E1 canonical |
| --- | --- | --- | --- | --- | --- |
| closed-file copy | yes | yes | yes | yes | yes |
| backup API | no | yes | yes | yes | yes |
| VACUUM INTO | no | no | no | yes | yes |
| pages in a read transaction | yes | yes | yes | yes | yes |
| file + WAL under a held transaction | yes | yes | yes | yes | yes |

Then `backup API: E2 pages: equal ignoring header bytes 24-27, 40-43, 92-99 (bytes differ only at
[27 43 95])` and `VACUUM INTO: E3 differs only in journal_mode: rollback for the copy, wal for the
source`.

Part B (the same database live; writer child `procs` SQL role committing each step with
`recipe-session`, i.e. one fresh connection per step; `wal_autocheckpoint=0`; the parent holds one
capture connection). Each line `<method> equals recipe step K (want W): <levels>`:
- plain copy of the live database file (child copy, no `-wal`), after the writer committed 7:
  **equals step 6** (want 7) — a valid but stale database; its commits are in the `-wal`.
- pages via `sqlite_dbpage` in one read transaction; the writer commits 8 between the two halves:
  **step 7**, E1.
- file copy, writer commits 9, `-wal` copy, all inside one read transaction (it saw 8), then WAL
  replay: **step 9**, E1 (the last commit before the `-wal` copy, ≥ the snapshot 8).
- `VACUUM INTO`, then the writer commits 10: **step 9**, E3 except journal_mode, E1 canonical.
- backup API, 5 pages per step (lines `backup step N: remaining R of P, copied C; …`): step 1
  copied 5 of 100; after step 2 the backup's own connection commits step 11 (through
  `BackupWithSourceExec`), after step 3 it commits 12; copied goes 5, 10, 15, 20, 25 while
  remaining goes 95, 90, 93, 98, 93 (page count 100, 108, 118); after step 5 the writer child
  commits 13; step 6 copied **5** (restart), remaining 119 of 124; it then runs to step 30
  (remaining 0). Result: **step 13**, E2.
- backup API, 5 pages per step, writer child commits after every step (steps 14-21); gave up after
  8 steps: remaining 119, 127, 139, 155, 157, 165, 166, 179 — **never finished** (each step
  starts again at page 1 and the source grows).
- Summary table (method | strongest level | safe under writer | blocks the writer):
  closed-file copy E1, **no** (needs the database closed; the live copy was stale);
  backup API E2, yes (when it finishes; commits ran between steps);
  VACUUM INTO "E3 but journal_mode; E1 canonical", yes, "no (commit right after; one statement,
  not interleaved)"; pages in a read transaction E1, yes, no (a commit ran inside the read
  transaction); file + WAL under a held transaction "E1 after WAL replay", yes, no.
- 9 checks, `CAPTURE COMPARISON OK`.

How to read "blocks the writer": in WAL mode none of the methods took the write lock; for the
three methods that hold a read transaction across the writer's commit this was shown directly.
For VACUUM INTO and a single backup step the writer was not interleaved *inside* the one call; the
source says both hold only a read transaction on the source (vacuum.c:270, backup.c:361-364).

## Experiment 9.2 crash-versus-power-loss

Run: `go run ./ch09-backup-crash-recovery-integrity/experiments/9.2-crash-versus-power-loss`
(flag `-portable` runs the non-strace path on Linux, to check the Mac route). Runtime ~23 s on
Linux (the kill sweep is ~40 traced child runs), ~3 s with `-portable`. Evidence 108 lines.

Writer child (`child-writer` mode of the same binary): `runtime.LockOSThread()`, opens the recipe
database (built to step 3, closed) on **one** connection with the given `synchronous` and
`wal_autocheckpoint=0`, commits steps 4-11, after each commit parses its own `-wal` (lock-safe,
see walship) and prints `commit K end=<offset of the end of the last commit frame> generation=<salt-1>`
and `ack K`; after step 7 runs `wal_checkpoint(PASSIVE)` and prints it; then `os.Exit(0)` **without
closing** (so the files are left as a crashed process leaves them). Child output is parsed by the
parent, not printed (salts are random).

States are matched with `recipe.MatchStep` on the closed, recovered file. Because the writer used
one long connection, every state after the first commit prints `(equal except the change counter
at bytes 24-27 and 92-95)` — ch01 correction 1; the masked match is unique per step (each step
sets a different `user_version`).

**Part B — model (portable; strace cross-check on Linux).** For each level OFF, NORMAL, FULL:
- `the writer acknowledged steps 4-11; checkpoint 7 busy=0 log=74 checkpointed=74`.
- **process crash**: copy the database and `-wal` as left (no `-shm`), reopen (recovery), close,
  identify: **step 11 at all three levels**, integrity ok. Completed writes are in the kernel's
  page cache; a process crash cannot lose them.
- `strace: -wal …` / `strace: database file …` (Linux only): number of syncs of that file in the
  whole run, number of writes after the last sync, lowest offset among them:
  OFF: `-wal 0 sync(s); 306 write(s) after the last one, the lowest at offset 0`; database `0
  sync(s); 44 write(s) …`. NORMAL: `-wal 3 sync(s); 156 write(s) after the last one, the lowest at
  offset 32`; database `1 sync(s); no write after the last one`. FULL: `-wal 11 sync(s); no write
  after the last one`; database `1 sync(s)`.
  Where the syncs come from: NORMAL's 3 = WAL header at creation (`walFrames` 4121-4124), the
  checkpoint's WAL sync (2277), the header of the restarted WAL; its database sync is the
  checkpoint's (2333). FULL's 11 = those 3 + 8 commits. Plus one directory fsync (on WAL creation)
  at NORMAL and FULL, which part A counts (5 and 13 sync calls).
- check: the strace record agrees with the rule (cut offset and "database file unsynced").
- **power-cut model** (the simplest outcome: nothing unsynced survives): copy the database file and
  the `-wal` truncated at the cut, no `-shm`, reopen, identify.
  - OFF: `not applicable at OFF: the checkpoint wrote the database file and nothing was ever
    synced, so a power cut can leave any mix of old and new database pages` (correction 5).
  - NORMAL: `-wal kept up to byte 32 of 321392` (only the restarted WAL's header was synced after
    the checkpoint) → **step 7**, integrity ok; `acknowledged commits past the cut: 4` (steps 8-11).
  - FULL: `-wal kept up to byte 321392 of 321392` (nothing to remove) → **step 11**; past the cut: 0.
  - checks: recovered = last acknowledged − commits past the cut; NORMAL loses exactly the commits
    since the last sync (the checkpoint after 7) and stays consistent; FULL loses none.
- The rule (portable, from pager.c:3658-3704 + wal.h:25-26 + walFrames/walCheckpoint): OFF → cut
  at 0 and the database file is unsynced if a checkpoint ran; NORMAL → no cut if the last event
  was a checkpoint, else cut at byte 32 (the last synced thing is a WAL header: the new file's, or
  the restarted one's); FULL → no cut. Commits counted as lost are those of the final WAL
  generation whose commit frame ends past the cut (older generations were fully checkpointed
  before the restart).
- Why truncating at the cut is the right copy even when unsynced writes overwrote older bytes:
  after a restart the new frames overwrite old-generation frames; the model drops those old bytes
  too, but recovery would stop at them anyway because their salts are the old generation's
  (`walDecodeFrame` salt check). Not separately demonstrated.

**Part A — kill at a syscall (Linux only).** For each level: count the uninterrupted run's sync
calls and pwrite64 calls on the writer's thread (the tid with the most pwrite64s); kill (strace
`inject=…:signal=SIGKILL:when=i`) on entry to every sync (OFF 0, NORMAL 5, FULL 13) and to 6
pwrite64 indices spread over 1..357 (1, 72, 143, 214, 285, 357). Each line: `kill at <syscall> #i
(<the call that was interrupted, as strace printed it>): acknowledged A, recovered <match>,
integrity_check ok`. Examples: `kill at fsync #1 (fsync r.db-wal …)`: acknowledged 3, recovered 3
(the WAL-header sync of the first commit); `#2 (fsync . …)`: the directory fsync; NORMAL `#4
(fsync r.db …)`: the checkpoint's database sync, acknowledged 7, recovered 7; FULL `#3 (fsync
r.db-wal)`: acknowledged 3, **recovered 4** (step 4's frames were complete in the page cache when
its commit sync was interrupted); `pwrite64 #1` is the first `-shm` allocation write (`r.db-shm
off=4095 len=1`). 30 kill runs, every one: killed by SIGKILL, integrity ok, recovered = last
acknowledged or that + 1, never a state outside the recipe. 47 checks in all,
`CRASH VERSUS POWER LOSS OK`.

**Scope statement facts for Neon (the outline's text, still true):** the model shows one outcome
of a power cut — the one where nothing unsynced survives. It does not simulate torn writes,
reordered writes or disks that acknowledge a sync they did not do; SQLite's defences against
those (per-frame cumulative checksums and salts, wal.c 856-1053 and the recovery loop above) are
read, not tested. This is the whole of the course's power-loss material (Nick's direction 4).
Added fact: at OFF a checkpoint writes the database file without any sync, so even the database
file itself is outside the model.

## Experiment 9.3 integrity-check-limits

Run: `go run ./ch09-backup-crash-recovery-integrity/experiments/9.3-integrity-check-limits`.
Runtime ~1.5 s. Evidence 24 lines. Portable.

Setup: recipe built to step 12 (118 pages, closed); page manifest (chapter 2 format, census roles)
and E3 logical hash of the original. Sites found with `dbfile` (deterministic): the first two
leaf pages of the `events` table and the first freelist leaf. Each site is one byte XORed in a
fresh copy; each copy is checked with `PRAGMA integrity_check`, `PRAGMA quick_check`, the E3
logical hash, and `capture.VerifyImage` against the original manifest. The file's sha256 is
checked unchanged by the SQLite checks.

| one byte flipped in | integrity_check | quick_check | E3 logical | page manifest |
| --- | --- | --- | --- | --- |
| b-tree page header (cell count, page 26 header byte 4) | 2 problems: "Fragmentation of 46 bytes reported as 0 on page 26" … | error ("*** in database main ***" …) | differs | page 26 |
| unindexed column (`events.note`, page 27, row 76, `q` → `Q`) | **ok** | **ok** | differs | page 27 |
| indexed column (`events.account_id`, page 27, row 76, 8 → 9) | 1 problem: "row 66 missing from index events_by_account" | **ok** | differs | page 27 |
| freelist leaf page (page 24, byte 2048) | **ok** | **ok** | **equal** | page 24 |

Readings: integrity_check catches structure (header) and an index that disagrees with its table;
it does not catch a changed byte in a value no index covers, nor anything on a freelist page.
quick_check also misses the index disagreement (it skips index checks, pragma.c:2058). The E3
logical hash catches every content change but not the freelist byte (no content). Only the
per-page hash manifest catches all four. "row 66" is the scan counter, not the rowid 76 (pragma.c
1857/2084-2090). 5 checks, `INTEGRITY CHECK LIMITS OK`.

Why kept (outline): "the restore passed integrity_check" is the usual proof offered for a rebuilt
database, and it is not one.

## Experiment 9.4 point-in-time-rebuild (rung R6)

Run: `go run ./ch09-backup-crash-recovery-integrity/experiments/9.4-point-in-time-rebuild`.
Runtime ~4-5 s. Evidence 123 lines. Portable.

Setup: recipe built to step 2; children `writer` and `outsider` (procs SQL role, each holds one
connection, `wal_autocheckpoint=0`, busy timeout 5 s); the writer commits every step with
`recipe-session` (fresh connection per step, so states equal the canonical recipe at E1). The
parent runs `walship.Open` (DirStore `_work/9.4/store`, `SnapshotEvery 3`, `Origin 2`).

- `shipper opened: snapshot snapshots/000000000002 (initial): position 2, 29 pages, user_version
  2, state 69490a2dc9c1980f, after frame 0 of no WAL`.
- **Phase 1**: batches {3}, {4,5}, {6}, {7,8,9}, {10}, {11,12}, {13}, {14}; one cycle after each.
  Every cycle prints `wal: new WAL generation after the shipper's full checkpoint`, the segment
  (`positions 3-3, frames 1-18 of WAL generation #1, user_version 3-3, before … after …`), the
  checkpoint (`busy=0 log=18 checkpointed=18; every shipped frame copied: true`), and after every
  third segment a periodic snapshot (positions 6 and 12). Checks: each batch is one segment ending
  at its last step, full backfill each time, 8 segments from 8 WAL generations (the WAL restarted
  after every shipper checkpoint — "across checkpoints").
- **Phase 2**: shipper closed; writer commits 15, 16; outsider `checkpoint TRUNCATE` → `busy=0
  log=0 checkpointed=0` (15-16 copied into the database file, WAL emptied); writer commits 17;
  shipper reopened (`resumed from the store at position 14`); cycle: `wal: discontinuity: the WAL
  header changed while the shipper held no read mark`, no segment, `snapshot
  snapshots/000000000015 (discontinuity): position 15, 162 pages, user_version 17`. Then 18 →
  segment 16 (WAL generation #10), 19-20 → segment 17-18.
- **Restore** (after every process closed): `store: 4 snapshot(s) at positions [2 6 12 15], 10
  segment(s), 0 damaged object(s)`. One line per position 2-18: `position P: snapshot S + N
  segment(s) -> user_version K, sha256 X, recipe step K sha256 Y, E1 equal true; older snapshots: a
  give the same bytes, b cannot reach it`. All 17 positions are byte-identical to the recipe's
  at-rest state for their step; every older snapshot that can reach the position gives the same
  bytes; snapshots before the discontinuity "cannot reach" positions 15-18 (the planner refuses at
  the gap). Worst case from the newest snapshot: 3 segments (= `SnapshotEvery`).
- `recipe steps with no restore point: [15 16]` — committed and checkpointed away while the
  shipper was down; the discontinuity snapshot holds step 17.
- `restored.db (position 18): integrity_check ok, equals recipe step 20 (exact bytes); the live
  database after every process closed equals recipe step 20 (exact bytes)`.
- **Damaged stores** (copies): segment 7 deleted → restore to 9: `segment chain is broken: no
  segment starts at position 7`; segments 10 and 11 swapped → restore to 11: `… the segment
  starting at position 10 is damaged: malformed walship object: key names first position 10,
  metadata says 11`; one byte of segment 13 flipped → restore to 13: `… damaged: malformed walship
  object: body sha256 does not match its metadata`. Each refused while planning, before any page
  is applied. 34 checks, `POINT IN TIME OK`.

Neon's adversarial list (outline "Astra will try") and where it stands: kill the capture process
and resume — covered in unit tests by close+reopen (resume in the same generation continues;
after a restart it snapshots), not by SIGKILL at every point; outside checkpoint behind its back —
9.4 phase 2; corrupt a segment — 9.4 and tests (also a byte flip with a recomputed body hash,
caught by SQLite's own checksum chain); restore inside a transaction — impossible by construction
(positions exist only at commit frames; `ErrNoCommit`).

## Timeline data (chapter section 4)

**A backup racing a writer (9.1 part B, 5 pages per step):**

| backup step | page count | copied (iNext−1) | remaining | event after the step |
| --- | --- | --- | --- | --- |
| 1 | 100 | 5 | 95 | — |
| 2 | 100 | 10 | 90 | backup's own connection commits step 11 (db grows to 108) |
| 3 | 108 | 15 | 93 | own connection commits step 12 (db grows to 118) |
| 4 | 118 | 20 | 98 | — |
| 5 | 118 | 25 | 93 | **writer child** commits step 13 |
| 6 | 124 | 5 | 119 | restarted from page 1 |
| 30 | 124 | 124 | 0 | done; the copy equals step 13 at E2 |

**A kill at each sync of a commit (9.2 part A, FULL, steps 4-11, checkpoint after 7):** sync #1
`fsync r.db-wal` (new WAL header) → recovered 3; #2 `fsync .` (directory) → 3; #3-#6 `fsync
r.db-wal` (commit syncs of steps 4-7) → recovered 4, 5, 6, 7 (acknowledged 3, 4, 5, 6: each kill
inside a commit sync keeps that commit); #7 `fsync r.db-wal` (checkpoint's WAL sync) → 7; #8
`fsync r.db` (checkpoint's database sync) → 7; #9 `fsync r.db-wal` (restarted WAL's header) → 7;
#10-#13 (commit syncs of 8-11) → 8, 9, 10, 11. NORMAL: #1 WAL header → 3, #2 directory → 3, #3
checkpoint WAL sync → 7, #4 checkpoint database sync → 7, #5 restarted header → 7.

**A shipper cycle (walship):** `BEGIN IMMEDIATE` (control) → read `-wal` → put segment → `COMMIT`
pin → `wal_checkpoint(PASSIVE)` → `BEGIN` + read (new pin) → [periodic snapshot] → `COMMIT`
control. The writer's next commit restarts the WAL (new generation).

## Platform implications (facts)

1. **Choose the capture by the equality it reaches and the concurrency it allows** (9.1): pages
   in one read transaction (E1, no lock taken from the writer); file then `-wal` under a held read
   transaction + replay (E1); `VACUUM INTO` (logical, rollback-mode output, E1 between canonical
   forms); backup API (E2; restarts on every commit by another connection and, under a steady
   writer, may never finish — 8 steps, never under 119 pages remaining); a plain copy of a live
   WAL database file is a valid but **stale** database (step 6 while the writer was at 7) —
   nothing reports it.
2. **State durability in terms of sync, not "committed"** (9.2): a process crash lost nothing at
   any level (step 11 recovered at OFF, NORMAL and FULL; 30 kill points, never a state outside
   the recipe). A power cut keeps only synced bytes: at NORMAL the model lost the 4 acknowledged
   commits after the last checkpoint; at FULL none; at OFF the database file itself was written
   without a sync, so the outcome is undefined.
3. **A machine losing power takes the unsynced tail of every database on it at once**: at NORMAL
   that tail is everything since each database's last checkpoint (here 4 commits, 321,360 WAL
   bytes). So the acknowledgement rule — after a local FULL sync (each commit costs one more
   fsync: 11 vs 3 WAL syncs in the run) or after the segment is in the store — is a platform
   decision (chapter 10's local-durable vs store-durable modes).
4. **Integrity checks do not prove content** (9.3): `integrity_check` passed a changed value and a
   changed freelist page; `quick_check` also passed an index/table mismatch. Prove a restore with
   hashes: E1 against a recorded state hash (walship records one per commit) or a page manifest.
5. **Continuous capture (R6) needs three things from SQLite and one from the store** (9.4):
   automatic checkpoints off on every writer connection; the shipper's read mark held between
   cycles and its own checkpoints run only after shipping; the WAL read from the file (lock-safe);
   and an object store with atomic put-if-absent, read-after-write, and no reliance on list order.
6. **Any checkpoint the shipper does not own, while it is not running, costs restore points**
   (9.4 phase 2: steps 15 and 16 have none); the shipper detects it and re-bases with a snapshot,
   so the chain never silently skips commits. Snapshot every *m* segments bounds restore work
   (worst case 3 segments with *m* = 3).
7. **The shipper briefly blocks the writer**: each cycle holds the write lock while it reads the
   `-wal`, puts one object, and checkpoints. Cycle length is the store's put latency plus a
   checkpoint.

## WAL-default evaluation (ruling 10) for this chapter

WAL is the default in every experiment; no experiment runs in rollback mode. Findings:

| Question | WAL | Evidence |
| --- | --- | --- |
| Live capture without stopping the writer | yes, three ways at E1 or E2 | 9.1 B |
| Point-in-time capture to every commit | yes, only because WAL frames are full page images with commit markers | 9.4 |
| Process crash | loses nothing at any `synchronous` level | 9.2 B, A |
| Power cut at NORMAL | loses the commits since the last checkpoint/header sync | 9.2 B |
| Power cut at FULL | loses nothing acknowledged; costs one WAL sync per commit | 9.2 B |
| A plain copy of the database file | stale (misses the `-wal`), not torn, in the measured case | 9.1 B (6.3 shows torn copies) |

Exception to record: **`VACUUM INTO` produces a rollback-mode file** from a WAL source; a
platform that restores from that canonical form must switch it to WAL after comparing (chunk 3
found the same). Recommendation recorded for Nick: WAL + `synchronous=FULL` (or NORMAL +
acknowledge after the store has the segment) for platform databases; nothing here argues against
WAL as the default.

## Linux-only and the Mac route

- Linux-only: 9.2's strace cross-check of the synced bytes (part B) and the whole kill sweep
  (part A). On other systems 9.2 prints `LINUX-ONLY: the strace confirmation of the synced bytes
  (the model below uses the rule read from wal.c and pager.c) skipped on <GOOS>; run it with:
  docker compose run --rm lab go run ./ch09-backup-crash-recovery-integrity/experiments/9.2-crash-versus-power-loss`
  (once per level) and `LINUX-ONLY: killing the writer at a chosen syscall with strace skipped …`,
  runs the process-crash half and the model from the rule, and exits 0 if they hold. Verified on
  Linux with `-portable` (same output minus the strace lines; exit 0). darwin: cross-built only —
  not run; the rule half uses nothing Linux-specific (the child is re-executed with `os/exec`).
- 9.1, 9.3, 9.4 and `internal/walship` use nothing Linux-specific. `DirStore` uses `os.Link` for
  put-if-absent (supported on APFS). darwin: cross-built only.
- Lab container: see BUILD-REPORT.md for the run of all four experiments inside
  `docker compose run --rm lab`.

## Working scripts

None beyond the experiments; `agent_work/ch09/INDEX.md` lists the notes and the report. One-off
checks done by hand during the build: reading `pragma.c` for the "row N" counter; reading wal.c
for `nCkpt` (correction 8).

## Not done / for later chunks

- No prose, no comments (seat rule).
- Not reproduced: the CLI `.backup` "database is locked" (correction 10 gives the source-based
  cause for Neon to test).
- Not built from Neon's lists: thousands of random-time `kill -9`s; a kill during a checkpoint is
  covered (FULL/NORMAL syncs #7-#9 / #3-#5 are inside the checkpoint), a kill during recovery is
  not; cutting the WAL in the middle of a frame in the model; SIGKILL of the shipper at every
  point (unit tests use close+reopen); a seeded random-offset sweep for 9.3.
- 9.1 is the longest experiment (450 lines with blank lines); its helpers (reference files and the
  level comparison) could move to a shared package if the chapter needs it shorter.
