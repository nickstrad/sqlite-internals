---
title: "sqlite-internals capstone core (10a): single-writer fencing on an S3 store that cannot check epochs, as measured on SeaweedFS"
summary: "Ownership facts for a multi-database SQLite platform: the store only enforces write-once positions so fencing needs a writer rule too, ownership is lost when the new owner's fence lands in the store, a takeover can lose local-durable acknowledgements but not store-durable ones, each open database costs 7 descriptors in the service."
tags: [sqlite, capstone, fencing, s3, seaweedfs, sqlite-internals]
updated: 2026-11-21
verified: 2026-11-21 — Linux amd64, Go 1.26, SQLite 3.53.4 via modernc.org/sqlite v1.60.1; evidence in the sqlite-internals repo
---

# sqlite-internals capstone core (10a): single-writer fencing on an S3 store that cannot check epochs, as measured on SeaweedFS

Ownership facts for a multi-database SQLite platform: the store only enforces write-once positions so fencing needs a writer rule too, ownership is lost when the new owner's fence lands in the store, a takeover can lose local-durable acknowledgements but not store-durable ones, each open database costs 7 descriptors in the service.

Source: the `sqlite-internals` course repo (github.com/nickstrad/sqlite-internals), chunk `ch10a`. Working notes: `agent_work/ch10a/build-notes.md`; review and adversarial records in the same directory.

## Corrections to the outline


### Correction 1

1. **"The store rejects segments from older epochs" — an S3 store cannot compare epochs; what it
   enforces is exclusive position slots, and that alone is not enough.** SeaweedFS (like S3) offers
   two conditional writes: `If-None-Match: *` (create only) and `If-Match: <etag>` (replace only
   that version). Neither can read a number inside an object and compare it with a number inside
   another. The design (section "The fencing scheme" below) therefore makes the store enforce one
   thing: **each stream position can be written once** (`PutObject` with `If-None-Match: *` on the
   key `dbs/<id>/log/<12-digit position>`). A new owner's first act is to occupy the next free
   position with a *fence* object; the old owner always writes at "my last position + 1", which is
   exactly the fenced position, so the store refuses it with 412 PreconditionFailed. The epoch is
   written into every object's envelope and is how the writer *interprets* a refusal (holder epoch >
   mine → fenced). Two things are needed together, and 10.1's self-tests show each one is
   necessary:
   - remove the store's check (plain overwriting PUT): the checker finds violations
     (`self-test store-accepts-every-put: 451413240 of 3536900716 interleavings violate an
     invariant`, shortest 4 steps);
   - keep the store's check but let a restarted writer resume from the store's global end instead
     of its own epoch's chain (what walship's resume does on its own, see correction 2): violations
     again (`self-test resume-from-global-end: 170698606 of 11458358774 …`, shortest 7 steps: the
     stale node appends at a free position *after* the new owner's objects and the store accepts it).
   So the claim holds as: **slot exclusivity in the store + the writer rule "append only directly
   after your own last object, and stop if a newer epoch is listed"**. The chapter should say the
   store enforces exclusivity and the epoch makes the refusal meaningful; it should not say the
   store checks epochs.

### Correction 2

2. **walship's resume (chapter 9) is not fenced by itself.** `walship.Open` on a store that already
   holds objects resumes at `Catalog.LastPosition()` — the newest position of *any* writer — and its
   first cycle publishes a discontinuity snapshot of the local file at the next position. Pointed at
   a shared store, a restarted stale owner would thereby append its own local state after the new
   owner's lineage, and put-if-absent would accept it (the slot is free). That is exactly mutant 2
   above. The service never hands walship the raw store: it hands it a `fence.Adapter` whose `List`
   fails with `FencedError` when any slot holds a newer epoch, so `walship.Open` fails instead of
   resuming. Shown in the real run (step 13: the restarted node b is refused with `fenced: db-0002
   position 3 is held by epoch 2, this writer has epoch 1`) and in test
   `TestRestartedNodeBelievesItsClaimUntilTheStoreSaysOtherwise`.

### Correction 3

3. **"A node that lost ownership cannot get another commit accepted" needs a precise "lost".** The
   catalog moves first (the claim bumps the epoch in the control-plane SQLite database), the store
   second (the fence put). Between the two, the old owner's segments are still accepted, and that
   is correct: they come *before* the fence in the one lineage, and the new owner's fence then lands
   after them (its put at the old end fails with "exists", it re-lists and fences at the new end —
   the model's `fence-with-stale-list` action, and real step 4 with `attempts 2`). The invariant the
   model checks is therefore stated in store order: *once an epoch's object is accepted, no object
   of an older epoch is accepted*, plus *no two epochs are accepted for overlapping positions* and
   *no accepted object is overwritten*. Ownership is lost when the fence is accepted, not when the
   catalog changes.

### Correction 4

4. **Local-durable acknowledgements of the old owner are lost by a takeover** — as the outline's
   durability model implies but does not say. Real step 3 acknowledges user_version 2 at
   `synchronous=FULL` (local-durable); node b's claim restores the store's last position (2,
   user_version 1) and fences after it; the evidence line `db-0001 local-durable acknowledgement at
   epoch 1 of user_version 2: in the store false` and the check `the local-durable acknowledgement
   … was never shipped, and the takeover lost it, as that mode allows`. So 10.1's invariant ("no
   acknowledged write is lost") holds for store-durable only; the chapter must name the mode.

### Correction 5

5. **Descriptor bound per open database is 7, not 3** (8.3 measured 3 per *connection*). A
   service-held database has three SQLite connections: the writer (connmgr's) and walship's two
   (control and pin). Measured: 3 × `.db` + 3 × `-wal` + 1 `-shm` (the `-shm` is one descriptor per
   process per file, shared by the connections through `unixInodeInfo`/`unixShmNode`) = 7. Evidence
   INFO lines show 0 / 7 / 14 above the baseline with 0 / 1 / 2 databases open; a probe listed them
   (`agent_work/ch10a/INDEX.md`). The standing assertion uses 8.3's per-connection figure as an
   upper bound: `3 descriptors × 3 connections × K + 32 reserve` (68 for K = 4) and 8.3's memory
   formula per connection (`connections × (2 × cache + 256 KiB) + 32 MiB`, 41 MiB for K = 4 at
   `cache_size = -256`: 12 × 768 KiB + 32 MiB).

### Correction 6

6. **Section 4.3's "fault injection in the object-store stand-in" and the outline's "local directory
   standing in for object storage" are replaced by ruling 2**: the store is SeaweedFS S3; faults are
   a Go wrapper (`service/faultstore`) around the real client. `walship.DirStore` is kept only as the
   unit-test double.

Everything else the outline says for 10.1 was confirmed: zero violating interleavings with the
store's check (`correct, depth 16, seed 1: 9595507962 interleavings explored, 44134 distinct
states, 0 interleavings violate an invariant`), the count is printed, the test fails when the
store's check is removed, and the real two-process run agrees with the model at every step
(`the real processes and the model agree at every command step (13 of 13)`). 10.1 was [unprobed]
and is now probed.

