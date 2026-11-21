---
title: "Chapter 10 capstone (10b): relocation and experiments 10.2–10.4"
summary: "Seven-step relocation with resume/rollback, acknowledged writes surviving kills, and a 500/2000-database fleet rebuild from SeaweedFS; see agent_work/ch10b/build-notes.md and the chapter"
tags: [sqlite, walfleet, capstone, relocation, seaweedfs]
updated: 2026-11-21
verified: 2026-11-21 — Linux amd64, Go 1.26, SQLite 3.53.4 via modernc.org/sqlite v1.60.1; evidence in the walfleet repo
---

# Chapter 10 capstone (10b): relocation and experiments 10.2–10.4

Seven-step relocation with resume/rollback, acknowledged writes surviving kills, and a 500/2000-database fleet rebuild from SeaweedFS; see agent_work/ch10b/build-notes.md and the chapter

Source: the `walfleet` repo (github.com/nickstrad/walfleet), chunk `ch10b`. Working notes: `agent_work/ch10b/build-notes.md`; review and adversarial records in the same directory.

## Corrections to the outline


### Correction 1

1. **"Fence the source by bumping the epoch, ship the final segment" — bumping the epoch alone does
   not stop the source.** The catalog epoch is a number in the control-plane database; the source
   node does not read it before a write, and the store keeps accepting the source's appends until
   the destination's fence object lands (10a correction 3). Measured: in every 10.3 case the
   boundary line after step 1 reads `catalog owner b epoch 2; store ends at position 3 (epoch 1
   segment); a owner at epoch 1, writable`, and the client's local-durable write to the source at
   that boundary is accepted and later shipped by the drain (store `4-5 e1 segment`). The relocation
   therefore has a **seal** step (2) in which the source refuses writes (`ErrSealed`), before the
   final ship (3); and the store fence (step 6) is what binds a source that does not cooperate (a
   sealed source that ignored the seal would be refused at slot 6, exactly as in 10.1). The outline's
   six actions become seven numbered steps:

   | Step | Name in code (`relocate.Step`) | Who acts | What happens |
   | --- | --- | --- | --- |
   | 1 | `FenceEpoch` | coordinator | one catalog transaction: owner := destination, epoch := epoch + 1, and a `relocations` row (source, destination, from/to epoch, step 1, state running) |
   | 2 | `SealSource` | source | writes `<id>.sealed`; every later `write` is refused |
   | 3 | `ShipFinal` | source | opens the database if needed (ships anything unshipped), evicts it: final cycle, close (SQLite checkpoints and deletes `-wal`/`-shm`), at-rest sha256 = shipped state; returns position P and sha256 H; the coordinator records P and H |
   | 4 | `Rebuild` | destination | lists the chain with an adapter at the new epoch (fenced if anything newer is there), restores the image at the chain end into `<id>.staged` |
   | 5 | `Verify` | coordinator | staged position = P and staged sha256 = H, else `ErrVerifyFailed` |
   | 6 | `Activate` | destination | puts the fence snapshot (the staged image) at P+1 with the new epoch; on acceptance renames the image into place, writes `<id>.owner`, records the catalog. A repeated `Activate` on a node that already holds the epoch (`Adopted: true`) re-records the catalog row (location, last_shipped = P+1, schema_version = the fence snapshot's user_version) when the store still ends at the fence, so an activation whose catalog update failed, or that crashed between `.owner` and the catalog, is repaired by `Resume`; if the node has shipped past the fence, the row is left alone |
   | 7 | `RetireSource` | source | evicts, deletes `<id>.db`, `.owner`, `.sealed`, `.atrest`, `.staged` |


### Correction 2

2. **The fence point is the drained state, not always the source's last commit position.** When the
   final ship's put fails before reaching the store (10.3 cases 22 and 26, and
   `TestStoreFaultsOnTheFinalShipAndTheFence/fail-before-put`), the source's close has already
   checkpointed the unshipped frames into the database file. The retried drain therefore finds no
   WAL and ships the two unshipped commits as **one discontinuity snapshot** at position 4 (store
   `4-4 e1 snapshot`) instead of a segment 4-5; the fence point is position 4, the fence at 5. The
   data is identical (fence-point sha256 `d53fc07b1293…` in both shapes). The outline's "destination
   sha256 = source sha256 at the fenced commit" holds as **destination sha256 = drained sha256 = the
   store's state at the fence point**; the position number of that point depends on the failure.


### Correction 3

3. **A takeover and a relocation differ on local-durable acknowledgements.** 10.1 (a takeover by
   `claim`) loses the old owner's unshipped local-durable acknowledgement. 10.3 (a relocation) keeps
   them: the drain ships them before the fence. Every 10.3 case acknowledges local-durable writes to
   the source (setup step 3, and the write after step 1) and every case ends with `final
   user_version` = number of acknowledged writes. A rollback also keeps them (the source drains
   before it re-claims).


### Correction 4

4. **10.2's "killed at chosen syscall indices" uses fsync only.** The node is killed by strace on
   entry to its K-th `fsync`/`fdatasync`, K evenly spaced over the uninterrupted run's count (77 in
   local-durable mode, 85 in store-durable mode with the default flags; 4 points per mode,
   `-syscall-kills` to change). `pwrite64` kill points were not added: chapter 9 (9.2) already sweeps
   pwrite64 for a single SQLite writer, and the claim here is about the service around it. The
   killed fsync's file is printed (WAL files, the catalog's WAL, and once the node's directory).


### Correction 5

5. **10.4 runs 500 databases by default, not 2,000.** Measured cost is I/O-bound (several fsyncs per
   acknowledged write: the WAL commit at `synchronous=FULL`, the shipper's checkpoint, the catalog
   update): 200 databases took 54 s with 4 writers, 500 take 1 min 52 s with 8 writers. The
   default is `-databases 500 -writers 8 -capacity 32`; the 2,000-database run is
   `go run ./ch10-…/experiments/10.4-rebuild-the-fleet -databases 2000` and its output is
   `evidence/10.4-rebuild-the-fleet-2000.txt` (see the 10.4 section for its numbers and runtime).
   What was scaled, and how: the count (flag); the write stream is 8 writer processes, each owning
   1/8 of the fleet (the outline says "a write stream" without a process count); the checker pauses
   1 s between passes while writers run (`-checker-pause`); restore points are rebuilt **in memory**
   and matched by sha256 against the recipe's step files, not written to a scratch directory (the
   rebuild of the fleet itself writes every database to disk); the fleet rebuild uses 8 in-process
   rebuild workers of one new node (`fresh-1` … `fresh-8` directories under `_work/10.4/fresh/`).


### Correction 6

6. **10.4's "rebuild time and bytes read as SHAPE": bytes are not stable, objects are.** Byte totals
   differ by ~100 bytes between runs (the JSON metadata inside each object stores WAL checksums and
   salts as decimal numbers, whose width varies). Bytes and time are therefore `INFO:` lines, and the
   EXACT claim is about objects: `the rebuild read each of the N slots it found exactly twice` — a
   measured inefficiency of `node.Claim` (its `Refresh` reads every slot to check the lineage, then
   `walship.LoadCatalog` reads every slot again to restore).


### Correction 7

7. **"Record each database's sha256 after a `TRUNCATE` checkpoint" — the checkpoint finds nothing.**
   The writers' close already left every file at rest (the last connection's close checkpoints and
   deletes the WAL, wal.c 2528–2557). The experiment still opens each file, runs
   `PRAGMA wal_checkpoint(TRUNCATE)`, closes it and hashes it; the evidence line is
   `WAL frames found: 0`.


### Correction 8

8. **Store-durable recovery from the store alone costs an epoch per database.** After the local files
   are deleted, the node has no belief files; it gets each database back with `claim`, which bumps
   the catalog epoch and writes a fence snapshot of the whole database. Recovery is therefore a
   takeover of every database (10.2 part A store-durable; 10.4 rebuild writes `500 fence objects`).


### Correction 9

9. **10a's open items — periodic snapshots and pruning, and the "chain starts at position 1" rule —
   were not needed and not built.** Relocation needs none of them: its fence snapshot is a full
   snapshot at P+1, so every relocation resets the restore cost. In 10.4 every chain has 6 or 7 slots
   (claim fence, 4 segments, maybe the rollout segment, the rebuild fence), so reopen and rebuild read
   few objects. How they would fit, for the chapter's "does not handle" list: a compaction snapshot
   can be put at `end + 1` in the writer's own epoch (the lineage rule already accepts a same-epoch
   snapshot); walship's `SnapshotEvery` cannot be used as is, because it writes its periodic snapshot
   at the *same* position as the last segment and the slot scheme allows one object per position;
   pruning slots before the newest snapshot needs the "starts at position 1" rule replaced by "starts
   at the floor the catalog records", otherwise a deleted prefix and a pruned prefix look alike.

Everything else the outline says for 10.2–10.4 was confirmed (details per experiment below). All
three were [unprobed] and are now probed.

