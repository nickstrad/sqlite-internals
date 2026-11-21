# Chunk 10b build notes (plain data for review, attack and the prose pass)

Builder: Claude Opus 5.5, branch `chunk/ch10b` (cut from main after 10a landed, ea5d2c9),
2026-11-21. Machine: linux/amd64, 4 shared cores, 7.7 GiB RAM, descriptor soft limit 1,048,576,
Go 1.26, ext4 under `_work/`. Pinned tree `$SQLITE_SRC` = SQLite 3.53.4. Store: SeaweedFS 4.48
from `compose.yaml` (the running `sqlite-internals-seaweedfs-1` container, S3 on host port 18333),
bucket `course-ch10b` (unique to this chunk), one key prefix per experiment (`exp-10-2/`,
`exp-10-3/`, `exp-10-4/`; the unit test of the batch wipe uses `test-wipe-batches/`; a working probe
used `probe-writecost/`). darwin: cross-built and cross-vetted only, never run.

This chunk is the second half of chapter 10: relocation (with resume and rollback), experiments
10.2, 10.3, 10.4, and the raw material for the whole chapter. 10a's notes
(`agent_work/ch10a/build-notes.md`) stay the source for the service core, 10.1, and 10a's
corrections; this file repeats only what the chapter needs in one place and says where 10b changed
10a's code.

Every claim names the line of evidence or the test that shows it. C line numbers are at the pin.

## Corrections to the outline

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

2. **The fence point is the drained state, not always the source's last commit position.** When the
   final ship's put fails before reaching the store (10.3 cases 22 and 26, and
   `TestStoreFaultsOnTheFinalShipAndTheFence/fail-before-put`), the source's close has already
   checkpointed the unshipped frames into the database file. The retried drain therefore finds no
   WAL and ships the two unshipped commits as **one discontinuity snapshot** at position 4 (store
   `4-4 e1 snapshot`) instead of a segment 4-5; the fence point is position 4, the fence at 5. The
   data is identical (fence-point sha256 `d53fc07b1293…` in both shapes). The outline's "destination
   sha256 = source sha256 at the fenced commit" holds as **destination sha256 = drained sha256 = the
   store's state at the fence point**; the position number of that point depends on the failure.

3. **A takeover and a relocation differ on local-durable acknowledgements.** 10.1 (a takeover by
   `claim`) loses the old owner's unshipped local-durable acknowledgement. 10.3 (a relocation) keeps
   them: the drain ships them before the fence. Every 10.3 case acknowledges local-durable writes to
   the source (setup step 3, and the write after step 1) and every case ends with `final
   user_version` = number of acknowledged writes. A rollback also keeps them (the source drains
   before it re-claims).

4. **10.2's "killed at chosen syscall indices" uses fsync only.** The node is killed by strace on
   entry to its K-th `fsync`/`fdatasync`, K evenly spaced over the uninterrupted run's count (77 in
   local-durable mode, 85 in store-durable mode with the default flags; 4 points per mode,
   `-syscall-kills` to change). `pwrite64` kill points were not added: chapter 9 (9.2) already sweeps
   pwrite64 for a single SQLite writer, and the claim here is about the service around it. The
   killed fsync's file is printed (WAL files, the catalog's WAL, and once the node's directory).

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

6. **10.4's "rebuild time and bytes read as SHAPE": bytes are not stable, objects are.** Byte totals
   differ by ~100 bytes between runs (the JSON metadata inside each object stores WAL checksums and
   salts as decimal numbers, whose width varies). Bytes and time are therefore `INFO:` lines, and the
   EXACT claim is about objects: `the rebuild read each of the N slots it found exactly twice` — a
   measured inefficiency of `node.Claim` (its `Refresh` reads every slot to check the lineage, then
   `walship.LoadCatalog` reads every slot again to restore).

7. **"Record each database's sha256 after a `TRUNCATE` checkpoint" — the checkpoint finds nothing.**
   The writers' close already left every file at rest (the last connection's close checkpoints and
   deletes the WAL, wal.c 2528–2557). The experiment still opens each file, runs
   `PRAGMA wal_checkpoint(TRUNCATE)`, closes it and hashes it; the evidence line is
   `WAL frames found: 0`.

8. **Store-durable recovery from the store alone costs an epoch per database.** After the local files
   are deleted, the node has no belief files; it gets each database back with `claim`, which bumps
   the catalog epoch and writes a fence snapshot of the whole database. Recovery is therefore a
   takeover of every database (10.2 part A store-durable; 10.4 rebuild writes `500 fence objects`).

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

## Changes to shared and 10a code

`internal/` packages: **none changed.** `go.mod`: unchanged.

10a's service packages (commented by Neon; I kept every comment, added none, deleted none):

| File | Change | Why |
| --- | --- | --- |
| `service/node/node.go` | `Config.AckBeforePut bool` (new field); `Write` refuses a sealed database first (`n.refuseSealed(id)`); `Write` skips the ship when `AckBeforePut` (the 10.2 mutant); `Claim` also deletes `<id>.sealed` and `<id>.staged` | seal for relocation; the self-test mutant; a re-claim (rollback) must leave no seal |
| `service/node/process.go` | `ProcessConfig.AckBeforePut`; `Args()` appends `-ack-before-put` only when set (so 10.1's child flags are unchanged); `RunProcess` passes it to `Config` | 10.2 mutant |
| `service/node/serve.go` | the unknown-command fallback now calls `handleRelocation` (in `commands.go`), which ends in the same `refused … unknown command` line | new commands |

Comments my change makes **incomplete** (not deleted, listed for Neon): `node.go` package comment
"Ownership state on disk lives in two per-database sidecar files" — there are now also `<id>.sealed`
(written by `Seal`, removed by `Activate`/`Retire`/`Claim`) and `<id>.staged` (a restored image
waiting for `Activate`); the `Claim` doc comment lists what it clears (now also the seal and the
staged file); the `Write` doc comment "StoreDurable acknowledges only after shipping" is false only
for a node started with the `AckBeforePut` mutant. 10.1 was re-run after the changes: its evidence
is unchanged (`course check evidence ch10` compares it).

New files (no comments):

| File | Lines | What |
| --- | --- | --- |
| `service/catalog/relocation.go` | 159 | `relocations` table and `BeginRelocation`, `GetRelocation`, `AdvanceRelocation`, `FinishRelocation` |
| `service/catalog/bulk.go` | 60 | `RegisterAll` (one transaction) and `All` |
| `service/node/relocate.go` | 307 | `Seal`, `Drain`, `Stage`, `Activate`, `Retire`, `Discard`, `Status`, `Inspect` |
| `service/node/commands.go` | 64 | line-protocol commands for those |
| `service/node/remote.go` | 172 | `Remote` (the same operations over a node process), strict reply parsing, `ParseAck` |
| `service/node/kill.go` | 8 | `Process.Signal`, `Process.Reap` (kill without waiting, for a timer) |
| `service/relocate/relocate.go` | 286 | the coordinator: `Run`, `Resume`, `Rollback` |
| `service/fence/inspect.go` | 118 | `Inspect`: rebuild every restore point of one database, name the last good one |
| `service/s3store/batch.go` | 43 | `WipeInBatches` (S3 `DeleteObjects`, 1,000 keys per request) |
| `service/lab/lab.go`, `fleet.go` | 207, 253 | experiment harness: work dir + store + catalog reset, node processes, reply tracking (problems, bounds), recipe matching, checker line protocol, fleet inspection, counting store, truncate-and-hash |
| experiments 10.2 / 10.3 / 10.4 `main.go` | 390 / 453 / 441 | |
| tests | catalog 2, fence 1, lab 4, node 4, relocate 7, s3store 1 new test functions | |

## The service after 10b (component facts for the Model section's diagram)

```
client ──► node (router: id → connmgr handle; ownership table; ack mode; seal)
             ├─ connmgr (8.3): ≤ K writer connections, LRU eviction
             ├─ per open database: walship.Shipper (control + pin connections)
             │      └─ fence.Adapter (epoch e) ──► faultstore (optional) ──► s3store ──► SeaweedFS
             └─ catalog (one SQLite database shared by all nodes on the host):
                   databases(id, owner, epoch, location, last_shipped, schema_version, settings)
                   relocations(id, source, destination, from_epoch, to_epoch, step, position, sha256, state)
relocation coordinator ──► catalog (steps 1, 5, records) and the two nodes (steps 2–4, 6, 7)
checker (10.4) ──► store only (lists, rebuilds every restore point, matches recipe states)
```

Facts for the diagram:
- Each open database: 3 SQLite connections (writer + shipper control + shipper pin), 7 descriptors
  (10a correction 5).
- Files per database on a node: `<id>.db` (+ `-wal`, `-shm` while open), `<id>.owner` (belief:
  `epoch N`), `<id>.atrest` (sha256 after a clean close), `<id>.sealed` (during a relocation,
  `epoch N`), `<id>.staged` (destination, between steps 4 and 6).
- Store keys per database: `dbs/<id>/log/<12-digit position>`, one object per position, written once
  with `If-None-Match: *`, each object = one envelope line (`fenced-slot v1 epoch=… kind=… first=…
  last=…`) + the chapter 9 walship snapshot or segment.
- The coordinator's only durable state is the `relocations` row; a "coordinator crash" in 10.3 is
  modelled by dropping the coordinator's memory and starting a fresh one from that row.

## The three layers of guarantee (for the Model section; kept separate)

**SQLite's local guarantees** (per file, on one machine; shown in chapters 4–9, used here):
- Atomic commit and durability at `synchronous=FULL` in WAL mode: the commit's WAL frames are synced
  before the commit returns (wal.c `walFrames`, 4195–4213). 10.2 kills at every kind of fsync and
  every reopened database holds every local-durable acknowledgement.
- Recovery after a crash: the next open rebuilds the wal-index from the valid frames up to the last
  commit frame (`walIndexRecover`, wal.c 1390–1614). 10.2: `reopened 4 of 4`, integrity ok, a recipe
  state, after every kill.
- Snapshot isolation and one writer per file through POSIX locks on a local inode (os_unix.c
  1189–1342): what lets the shipper's pin cap checkpoints and its `BEGIN IMMEDIATE` cycle exclude the
  writer. Nothing in it reaches across machines.

**The coordination the service adds** (all in Go, all tested here or in 10a):
- Epochs in the catalog; position slots with put-if-absent; the writer rule "append after your own
  last object; stop when a newer epoch is listed" (10.1).
- Acknowledgement modes: local-durable (after the local commit at FULL) and store-durable (after the
  segment's put) (10.1, 10.2).
- The relocation protocol: catalog fence, seal, drain, rebuild, verify, store fence, retire; resume
  and rollback decided by whether the destination's fence is in the store (10.3).
- At-rest records, belief files, the seal file, the staged file (crash-consistent local state).
- The standing bound on descriptors and memory per node process (every experiment).

**What the store provides, as measured on SeaweedFS 4.48 (single server, this machine)**:
- Atomic put-if-absent: `If-None-Match: *` → exactly one winner among 2 and 8 concurrent writers
  (probe `agent_work/probes/seaweedfs-s3/RESULT.md`); every slot refusal in 10.1/10.3 relies on it.
- Read-after-write and list-after-write consistency (probe; 10.3's resume reads back fences it did
  not get a reply for, case 25 `fail-after-put` → `rolled forward`).
- Atomic single PUT (no partial objects): the faultstore's `partial-put` is a stress case for
  readers, not something the store was seen to do.
- Not provided: list order (sorted by the service), epoch comparison (correction 1 of 10a),
  multi-key transactions.
- Not measured, not claimed: AWS S3 behaviour, durability of the SeaweedFS volume, multi-server
  SeaweedFS, network partitions.

## The six capstone invariants as checkable properties

| # | Invariant (course doc) | Checkable property | Checked by |
| --- | --- | --- | --- |
| 1 | At most one authorized writer owns a database at a time | (a) In store order: once an epoch's object is accepted no older epoch's object is accepted; no two epochs accepted for overlapping positions; no accepted object overwritten. (b) At every relocation step boundary at most one node reports `writable` (owner, not sealed, not known fenced). | 10.1 (model: every interleaving to depth 16, 0 violations; mutants found; real processes agree). 10.3: `most writable nodes at any boundary 1` in all 32 cases; the `skip-fence` self-test finds 2. |
| 2 | No acknowledged durable write is lost under the stated durability model | For every database: recovered user_version ≥ the last user_version acknowledged in its mode. Local-durable: after a process kill and restart on the same files. Store-durable: after a process kill, deleting every local file and rebuilding from the store. Relocation: every acknowledgement (either mode) is in the final lineage. Takeover: store-durable kept, local-durable may be lost. | 10.2 (8 fsync kill points, 4 random kills, 0 violations; the `ack-before-put` mutant is caught). 10.3 (final user_version = acknowledged count in 32 cases). 10.1 (takeover). |
| 3 | Every published snapshot is internally consistent | Every restore point in the store (every position of every database) rebuilds, and the rebuilt bytes are a recipe state (sha256 equal to the recipe's step file, exactly or apart from the change counter with bytes 24–27 = 92–95). | 10.4: concurrent checker passes during the write stream and a final pass (`3248 restore points, 0 fail to rebuild` at 500 databases). 10.3: every store-durable acknowledgement's slot restores to its user_version. |
| 4 | A database can be reopened after process failure | After SIGKILL at any chosen fsync or random time: every database opens, `PRAGMA integrity_check` = ok, the file is a recipe state. | 10.2 (`reopened 4 of 4` on every kill line). 10.1 step 14 (reopen of a killed node's open database). 10.3 (killed source/destination restart and finish). |
| 5 | File descriptors and memory remain bounded | After every command of every node process: descriptors above the process's startup baseline ≤ 3 × 3 × K + 32 and RSS growth ≤ 3K × (2 × cache + 256 KiB) + 32 MiB (8.3's per-connection figures, 3 connections per open database). | Every experiment: 10.1 (15 commands), 10.2 (316 replies), 10.3 (~1,670 replies), 10.4 (~2,770 replies, plus the in-process rebuild and the checker process). |
| 6 | An interrupted migration cannot silently create two authoritative copies | For a crash or store fault at every step, after resume or rollback: the catalog owner, the newest slot's epoch and the relocation record agree; at most one node was writable at every boundary; the destination's image = the drained source state at the fence point (sha256); every acknowledged write is in the lineage and no refused write is (final user_version = acknowledged count); the owner's closed file = the store's last state. | 10.3 (32 fault cases, `32 of 32 fault cases hold every invariant`). |

## Experiment 10.2 acknowledged-writes-survive (EXACT, Linux part + portable part)

**Claim (as checked).** One service node owns 4 databases and keeps 2 open (so evictions happen on
every write). After the node process is killed — by strace on entry to a chosen fsync (Linux) or by
SIGKILL at a seeded random time (portable) — every database reopens with `integrity_check` ok as a
recipe state, and holds at least every write acknowledged in its mode: local-durable writes after a
restart on the node's own files; store-durable writes after the node's directory is deleted and every
database is rebuilt from the store alone. A node that acknowledges store-durable writes before the
put is caught.

**Run:** `docker compose up -d seaweedfs`, then
`go run ./ch10-multi-database-platform-capstone/experiments/10.2-acknowledged-writes-survive`.
Runtime 40 s (two runs: 39.9 s, ~40 s). Flags: `-databases 4 -writes 12 -syscall-kills 4
-random-kills 2 -seed 1 -capacity 2` (defaults); `-portable` skips part A on Linux; `-mutant
ack-before-put` makes every killed node the mutant (exits 1: shown below); `-bucket`, `-prefix`
(`course-ch10b`, `exp-10-2`). Internal: `-role node -name -work -fault -ack-before-put`. Scratch
`_work/10.2/` (catalog.db, `a/`), wiped per run; the S3 prefix is wiped with `WipeInBatches` per run.

**How a run works.** `setup`: wipe the work dir and the prefix, open a fresh catalog, register the
databases (`RegisterAll`), start node `a`, `claim` each database (fence snapshot at position 1 of
recipe step 0), close node `a` cleanly. Killed run: node `a` gets 12 commands `write db-000N <mode>`
round robin (N = 1..4). Part A runs it under `strace -f -e trace=fsync,fdatasync -e
inject=fsync,fdatasync:signal=SIGKILL:when=K` with the 12 commands on stdin (the node calls
`runtime.LockOSThread()`, so its SQLite calls run on one thread, and strace's per-thread counter is
that thread's). Part B starts the node interactively and a timer kills it. Acknowledgements are the
`ok write …` lines the node printed before it died (parsed strictly by `node.ParseAck`). Recovery:
local-durable — restart node `a` on its directory, `check` each database (opens it: SQLite recovery,
the shipper ships anything unshipped), close; store-durable — delete `_work/10.2/a/` entirely,
restart node `a`, `claim` each database (restore from the store), `check` each, close. Then the
parent hashes each closed file and matches it against the recipe's step files.

**How to read every line** (`evidence/10.2-acknowledged-writes-survive.txt`):
- `sqlite_source_id: … (pin 3.53.4 ok)`.
- `INFO: store …, work directory …` (ignored by the check).
- `store: bucket course-ch10b, prefix exp-10-2/; one node owns 4 databases and keeps at most 2 open;
  each killed run sends 12 writes round robin; settings synchronous=FULL …` — the catalog settings
  (`node.PlatformSettings()`).
- `recovery: …` — the two recovery modes.
- Part A, per mode: `<mode>, uninterrupted: 12 commands, 12 acknowledged, 77 fsync calls on the
  node's command thread (the process exits at the end of its input without closing)` (85 for
  store-durable: every write also checkpoints after its ship). `INFO: fsync calls on other threads:
  0`. `check ok: … every fsync runs on the command thread` (if another thread fsynced, the kill index
  would not be the command thread's).
- One line per kill point: `<mode>, kill at fsync K of T (of <file>) during command C (<command>): A
  acknowledged; reopened 4 of 4; recovered user_version r1,r2,r3,r4; acknowledged a1,a2,a3,a4`. K is
  spread evenly over 1..T. `<file>` is the base name of the fsynced file at the kill: `db-000N.db-wal`
  (the commit sync of `walFrames`, or the shipper checkpoint's WAL sync), `catalog.db-wal` (the
  catalog's commit after a ship — the write is in the store but not yet acknowledged), `a` (the node
  directory: SQLite syncs the directory after creating a `-wal` file). The recovered list must be ≥
  the acknowledged list element by element. Measured examples: local-durable K=77 during command 12:
  11 acknowledged, recovered `3,3,3,3` vs acknowledged `3,3,3,2` — the twelfth write (db-0004 step 3)
  was committed in the WAL before the kill but not acknowledged, and it survives (allowed: ≥).
  Store-durable K=29 `(of catalog.db-wal)` during command 4: db-0004's step 1 was already in the
  store, the node died updating the catalog before acknowledging; recovered `1,1,1,1`, acknowledged
  `1,1,1,0`.
- `check ok: <mode>: strace killed the node at each of the 4 fsync kill points` (result.Killed =
  SIGKILL for each) and `check ok: <mode>: after each kill at an fsync every database reopens
  (integrity ok, a recipe state) and holds at least every write acknowledged in this mode`.
- Part B: `INFO:` lines for the unkilled interactive run's write-loop time (`the write loop of an
  unkilled interactive run of 12 writes took …`: measured from the first write command to the last
  reply, excluding setup and the node's start and reap) and each random kill: `<mode>, random kill
  K (draw D) after <delay>: …` (delay, counts). The delay is drawn in (0, write-loop time). A draw
  whose kill came after the last reply (all 12 acknowledged) is still recovered and checked, then the
  kill is redrawn, at most 5 draws per kill. New EXACT check per mode: `check ok: <mode>: each of the
  2 random kills landed while writes were in flight (a kill after the last reply is recovered and
  checked too, then redrawn, at most 5 draws per kill)`. In the repair runs (2 default, 1
  `-portable`) all 12 random kills landed on draw 1 with 6–11 of 12 acknowledged. The violations
  check now reads `<mode>: after every kill at a random time every database reopens …` (it covers
  redraws too). Before the repair the delay was drawn over setup + writes (setup is 50–70 % of that),
  and most kills landed after the last write (REVIEW-1 finding 3); with the window fixed but no
  redraw, the `-portable` run still had 2 of 4 store-durable draws land after the last reply (seed 1
  draws ~0.94 and ~0.96 of the window), which is why the redraw and its check exist.
  `check ok: <mode>: an interactive run that is not killed acknowledges all 12 writes`; `check ok:
  <mode>: after every kill at a random time …`.
- Part C (self-test): node `a` started with `-ack-before-put`; `ok write db-0001 store-durable:
  acknowledged at epoch 1, user_version 1, stored through position 0` (position 0: nothing stored);
  the node is killed; store-durable recovery; `violation: db-0001 recovered step 0, acknowledged step
  1`; `check ok: self-test: the checker finds the store-durable acknowledgement the store never
  received` (requires exactly one violation).
- `check ok: no node reported a problem` (no `problem …` line from any node process);
  `check ok: after every command of every node process the descriptors and memory stayed within the
  standing bounds`; `INFO: N node replies carried a bounds check` (316 in the evidence run).

**The check can fail:** `-mutant ack-before-put` → `check FAILED: store-durable: after each kill at
an fsync …` (3 violations, e.g. `db-0004 recovered step 0, acknowledged step 1`) and the same for the
random kills; `2 check(s) FAILED`, exit 1. An unknown `-mutant` → `FATAL -mutant`, exit 1.

**Non-Linux path:** part A prints `LINUX-ONLY: killing the node at a chosen fsync with strace
skipped on darwin; run it with: docker compose run --rm lab go run
./ch10-multi-database-platform-capstone/experiments/10.2-acknowledged-writes-survive` and parts B and
C run; exit 0 when they pass. The same happens on Linux with `-portable`. darwin: cross-built only.

**What it does not show:** power loss (process kills only; chapter 9.2's model applies — at
`synchronous=FULL` a power cut loses no acknowledged local-durable commit if the disk honours fsync);
kills inside `claim` (setup and store-durable recovery run unkilled); concurrency (one node, one
command at a time).

## Experiment 10.3 interrupted-relocation (EXACT)

**Claim (as checked).** For a crash or a store fault at each numbered step of a relocation of one
database from node a to node b, with a client writing at every step boundary, and recovery by resume
(or by rollback where the fence is not yet in the store): the catalog, the newest store slot and the
relocation record name one owner; at no step boundary are two nodes writable; the copy that ends up
authoritative equals the drained source state at the fence point (sha256); every acknowledged write
is in the one lineage and no refused write is.

**Run:** `go run ./ch10-multi-database-platform-capstone/experiments/10.3-interrupted-relocation`.
Runtime ~35 s (32 cases + the self-test). Flags: `-case N` runs only case N; `-mutant skip-fence`
runs every case with a coordinator that neither seals the source nor puts the store fence (exits 1);
`-bucket`, `-prefix` (`course-ch10b`, `exp-10-3`), `-capacity 2`. Scratch `_work/10.3/`.

**Each case:** reset; start nodes a and b (one of them with a fault rule if the case has one); a
claims `db-0001` (fence at 1), writes steps 1 and 2 store-durable (positions 2, 3) and step 3
local-durable (not shipped). Then `Coordinator.Run(db-0001, a → b)`. After every completed step
(`AfterStep`) the experiment: asks both nodes `status db-0001`, records the catalog owner/epoch and
the store's end, then writes once store-durable to the node the catalog names and once local-durable
to the other node (the stale route), and (kill cases) kills the named node. After `Run` returns:
restart any dead node (without faults), then a **fresh** coordinator (no memory of the first) runs
`Resume` or `Rollback` from the catalog's `relocations` row. One more boundary (status, two writes)
after recovery. Then every node is closed and the store, catalog, files are checked.

**Fault cases (32; the case number is the evidence's):**

| Cases | Fault | Recovery |
| --- | --- | --- |
| 1–7 | the coordinator stops after step 1..7 (after the step's action, before it records the step; step 1's record is atomic with its action) | resume |
| 8–13 | the coordinator stops after step 1..6 | roll back (case 13: the fence is already in the store → it rolls forward, `ErrFenceInStore`) |
| 14–19 | node a killed after step 1, 2, 3; node b killed after step 4, after 6; node a killed after 6 | resume (case 18 is not an interruption: the relocation finishes, b restarts from its belief file) |
| 20–21 | node a killed after step 2; node b killed after step 4 | roll back |
| 22–29 | on the final segment (a's store, key `log/000000000004`) and on the fence (b's store, `log/000000000006`): `fail-before-put`, `fail-after-put`, `crash-before-put`, `crash-after-put` (the node process exits 3) | resume |
| 30–31 | `fail-before-put` on the final segment; `crash-before-put` on the fence | roll back |
| 32 | node b killed after step 4 and kept down through the recovery (restarted only afterwards) | roll back while b is down |

**Resume and rollback rules (`relocate.Resume`, `relocate.Rollback`):** read the row; list the chain.
Newest slot epoch > the relocation's epoch → someone else took over: finish as `superseded`, do
nothing. Newest = the relocation's epoch → the destination's fence is in the store (the put landed
even if nobody got the reply): roll **forward** only: the destination `Activate`s with adopt (reads
the fence snapshot from the store, checks its sha256 against the recorded H, installs it), then the
source retires. Otherwise (no fence): resume re-runs steps 2–7 (each is idempotent: seal rewrites the
file, drain of a closed database reopens, ships nothing new and closes, stage rewrites the staged
file, verify compares, activate puts the fence, retire deletes); rollback seals and drains the source
(ships anything unshipped while its epoch is still the newest in the store), lets the source `claim`
the database (catalog epoch + 1 owned by the source, fence at the end + 1, the image restored equals
the drained file), records the relocation `rolled-back` in the catalog, and only then asks the
destination to discard its staged file. The discard is best effort: if the destination is down, the
rollback still succeeds and returns the discard error in `Result.DiscardErr` (10.3 case 32 prints
`recovery: the destination did not discard its staged copy: node process for "discard db-0001" is
not running`). The leftover `<id>.staged` is harmless: the source's newer fence is already in the
store, `Activate` of the old epoch is refused, and a later `Stage` rewrites the file. A rollback that
stopped after the source's claim but before the catalog record (newest slot epoch > the relocation's
epoch, and the catalog row names the source as owner at that epoch) is finished on retry as
`rolled-back` (discarding at the destination again), not as `superseded`; a newer epoch owned by any
other node is still `superseded`. A zombie
destination still holding the relocation's epoch is fenced by the source's newer fence.

**How to read every line** (`evidence/10.3-interrupted-relocation.txt`):
- pin line; `INFO:` store and work dir; `store: bucket …, prefix exp-10-3/; nodes a (source) and b
  (destination) share the catalog; settings …`.
- `relocation steps:` and the seven `N (name)` lines (`relocate.Step.String`).
- `each case: …` — the scenario.
- Per case: `case N: <fault>; recovery: <resume|roll back>`.
  - `run: the coordinator stopped` (injected coordinator crash) | `run: step S failed: the node
    refused` (a `refused …` reply, e.g. the injected store error) | `run: step S failed: the node did
    not answer` (the node process died) | `run: finished without interruption (done)` (case 18).
  - `node X is down; restarted` for each dead node; case 32 prints `node b is down and stays down
    during the recovery`, then after the recovery line `recovery: the destination did not discard its
    staged copy: …` and `node b restarted after the recovery`.
  - `recovery: <rolled forward|rolled back|done>; relocation record: state <done|rolled-back>, step
    N; fence point: position P sha256 <12 hex>, fence at position P+1` — P and H are the drained state
    (for a rollback, the rollback's own drain); the fence is the destination's (resume) or the
    source's re-claim fence (rollback). The record's step is the last step recorded before the
    rollback (rollback does not advance it).
  - `after step N: catalog owner X epoch E; store ends at position Q (epoch E' kind); a <status>; b
    <status>` for each boundary reached before the fault; `after recovery: …` once. Status is
    `owner at epoch E, writable` | `owner at epoch E, sealed` | `not owner` | `fenced` | `down`.
  - `store: f-l eE kind; …` — every slot in the final store (read back from S3 with `fence.Audit`).
  - `final: catalog owner X epoch E, newest slot epoch E; A acknowledged (S store-durable), R refused;
    final user_version U (<recipe match>); most writable nodes at any boundary W`. R counts every
    refused or unanswered client write (stale route, sealed source, not-owner destination, dead
    node).
  - `check ok: case N: …` — all six parts held; a failing part prints `NOT: <part>` before the check.
    The six parts: the fault interrupted the relocation (or, case 18, did not); one owner (catalog
    owner = expected node, catalog epoch = newest slot epoch, relocation state = done or rolled-back);
    W ≤ 1; restore(P) and restore(fence) both hash to H; every store-durable acknowledgement's (epoch,
    position) is a slot of that epoch and restores to its user_version, the final state is a recipe
    state and U = A; the owner's closed file (connmgr's at-rest check) = restore of the last position.
    A seventh part, only for case 32: the rollback finished (`rolled back`) and reported the
    destination's discard failure.
- `32 of 32 fault cases hold every invariant`.
- Self-test: `== self-test: the same relocation without the seal and without the store fence ==`,
  `case 0: …`, its boundary lines (from step 6 on: `a owner at epoch 1, writable; b owner at epoch
  2, writable`), `the store does not hold one lineage: … epoch 2 starts at position 4 with a segment,
  not a fence snapshot`, then `check ok: self-test: without the fence the boundary check finds two
  writable nodes`.
- `check ok: after every command of every node process the descriptors and memory stayed within the
  standing bounds`; `check ok: no node reported a problem` (added in the repair round: any
  `problem …` line from a node process now fails 10.3, as in 10.2 and 10.4); `INFO: N node replies
  carried a bounds check`. A case whose recovery fails now also kills its node processes before the
  next case starts.

**The check can fail:** `-mutant skip-fence -case 1` → the case prints `after recovery: … a owner at
epoch 1, writable; b owner at epoch 2, writable` and `the store does not hold one lineage: … epoch 2
starts at position 4 with a segment, not a fence snapshot`, then `check FAILED: case 1: …`, `0 of 1
fault cases hold every invariant`, exit 1.
`-mutant bogus` → `FATAL -mutant`, exit 1.

**Observed behaviours worth a sentence:**
- A source killed between the catalog fence and the drain (cases 14, 15, 20) restarts with unshipped
  local commits in its WAL; its reopen ships them as a discontinuity snapshot (`4-4 e1 snapshot`),
  not a segment, and the drain then ships nothing: the fence point is position 4.
- `fail-after-put` on the fence (case 25) and `crash-after-put` (case 29): the fence is in the store,
  b got no success; resume sees the fence and adopts it — no second fence, no rollback possible.
- `crash-before-put` on the fence with rollback (case 31): b died before its put; the rollback's
  re-claim by a lands at position 6 with epoch 3; b, restarted, holds no belief.

## Experiment 10.4 rebuild-the-fleet, rung R7 (EXACT)

**Claim (as checked).** The store plus the catalog is a complete saved state of the fleet: after a
write stream over N databases (each claimed, given recipe steps 1–4 store-durable) and a schema
rollout (recipe step 5, `CREATE INDEX events_by_account ON events(account_id, id)`) applied to half
of them and stopped, quiescing, recording each database's sha256 after a TRUNCATE checkpoint and
deleting every local file, a new node with empty disks rebuilds every database byte-identical (E1) to
its recorded sha256, each at the schema version the catalog recorded (old = step 4, new = step 5);
every restore point published along the way — while writers ran and after the rebuild — rebuilds to
a recipe state.

**Run:** `go run ./ch10-multi-database-platform-capstone/experiments/10.4-rebuild-the-fleet`.
Defaults `-databases 500 -writers 8 -capacity 32 -checker-workers 4 -checker-pause 1s`; runtime
1 min 52 s to 1 min 57 s (three runs). The outline's size: `-databases 2000` (see below). Before anything, the experiment checks
that `3 × 3 × capacity + 32` fits the descriptor soft limit (`procstat.FileLimit`) and exits with
`FATAL descriptor limit` otherwise. Scratch `_work/10.4/` (catalog, `w1`…`w8`, `fresh/1`…`fresh/8`).

**Phases:** (1) reset + `RegisterAll`; start 8 writer node processes `w1`…`w8` (database i goes to
writer i mod 8) and one checker process (`-role checker`, line protocol `pass`/`close`, its own
bounds assertion with K = 0). (2) Write stream: each writer, in batches of `capacity` databases,
claims the batch, then writes steps 1–4 store-durable round robin over the batch. Meanwhile the
parent asks the checker for a `pass` every second: the checker lists `dbs/`, and for every database
`fence.Inspect` rebuilds every position in memory and matches it against the recipe step hashes.
(3) Rollout: each writer writes step 5 to the first half of its share. (4) Stop the checker loop.
(5) Quiesce: close every writer (final ships, at-rest checks), then for every catalog row open the
owner's file, `PRAGMA wal_checkpoint(TRUNCATE)`, close, sha256 (connmgr's at-rest check). (6) Delete
`w1`…`w8`. (7) Rebuild: 8 in-process nodes `fresh-1`…`fresh-8` (one new machine's worth of
directories) claim their share through a counting store wrapper; compare each rebuilt file with the
recorded sha256 and with the catalog's schema version and the last acknowledgement. (8) A final
checker pass over the whole store. (9) Close the checker.

**How to read every line** (`evidence/10.4-rebuild-the-fleet.txt`):
- pin line; `INFO: descriptor soft limit …; each writer's bound 320`; `INFO: store …`.
- `fleet: 500 databases, 8 writer processes (each owns 62-63 and keeps at most 32 open), one checker
  process; store: bucket course-ch10b, prefix exp-10-4/` (63 = the largest share; 500/8 = 62.5).
- `INFO: write stream took …`; `write stream: every database claimed and given recipe steps 1-4,
  each acknowledged store-durable`.
- `schema rollout: recipe step 5 (CREATE INDEX …) applied to 248 of 500 databases, then stopped`
  (248 = Σ floor(share/2) over the 8 shares of 63 and 62).
- `INFO:` rollout time, wait for the checker's last concurrent pass, and `the concurrent checker ran
  P passes … and rebuilt R restore points while the writers ran` (timing-dependent).
- `check ok: every restore point the checker found while the writers ran rebuilt to a recipe state`
  (P > 0 and no failure line).
- `INFO: writer wN: ok close; standing assertion: …` (8 lines).
- `quiesce: every writer closed; 500 databases recorded after a TRUNCATE checkpoint (WAL frames
  found: 0)` and its check.
- `every local file deleted (0 files left under the writers' directories)`; `check ok: the new node
  starts with empty disks`.
- `INFO: rebuild of 500 databases by 8 workers took …; standing assertion: …` (the parent's own
  assertion, K = 8).
- `rebuild read 5496 objects in 500 listings and wrote 500 fence objects` — one listing per database
  (`Refresh`), each slot read twice, one fence put per database. `INFO: rebuild read B bytes (b per
  database)` (≈ 384.5 MB, ≈ 769 KB per database at 500).
- `rebuilt: 500 of 500 byte-identical (E1) to the sha256 recorded after the TRUNCATE checkpoint; 252
  at recipe step 4, 248 at recipe step 5`, and three checks: E1 for all; every rebuilt database is a
  recipe state whose user_version = the catalog's `schema_version` = the last acknowledged
  user_version; the histogram has only steps 4 and 5, both non-empty (step 5 is the one with the
  index).
- `check ok: the rebuilding process stayed within the standing bounds`.
- `INFO: final checker pass took …`; `final checker pass: 500 databases, 3248 restore points, 0 fail
  to rebuild` (3248 = 252 × 6 + 248 × 7: claim fence, 4 segments, [rollout segment], rebuild fence);
  `INFO: the final pass read B bytes, the store's whole content` (≈ 297 MB); `check ok: after the
  rebuild every published restore point of every database rebuilds to a recipe state`.
- `check ok: the rebuild read each of the 2748 slots it found exactly twice (once to check the
  lineage, once to restore)` (5496 = 2 × (3248 − 500)).
- `INFO: checker: standing assertion: …`; `check ok: no node reported a problem and every command
  was acknowledged`; `check ok: after every command of every writer and checker command …`; `INFO:
  … replies …; total runtime …`.

**SHAPE facts (INFO lines of the evidence run, 500 databases):** write stream 54 s, rollout 11.6 s,
quiesce 2.0 s, rebuild 15.9 s (≈ 32 ms per database with 8 workers), final pass 16 s; rebuild read
≈ 769 KB per database, ≈ 1.3 × the database's share of the store (594 KB) because every slot is read
twice and the newest snapshot is the largest object.

**The 2,000-database run:** see `evidence/10.4-rebuild-the-fleet-2000.txt` and the section "2,000
databases" at the end of this file.

**Neon's attacks the code is ready for:** `fence.Inspect` names the last good restore point of a
database whose slot is deleted or corrupted (`TestInspectNamesTheLastGoodRestorePoint`: gap → `last
good restore point 2`, `position 4 follows 2`; corrupted object → last good 2); a partial put leaves a
damaged slot that `Inspect` reports (never a restore point) — and wedges that database's writer,
since the slot can never be rewritten (S3's single PUT is atomic, so this is a non-S3 failure mode);
an old writer still running during the rebuild is fenced by the rebuild's fence (10.1's mechanism).

## Relocation timeline (for the chapter's Timeline section)

From 10.3 case 7 (every boundary reached; the evidence lines `after step N: …`) and the store dump.
Before: a owns db-0001 at epoch 1; store slots 1 (e1 fence snapshot, step 0), 2 (e1 segment, step 1),
3 (e1 segment, step 2); a's file has step 3 committed locally (local-durable, not shipped).

| After step | Catalog (owner, epoch) | Node a (source) | Node b (destination) | Store end | Client writes at this boundary |
| --- | --- | --- | --- | --- | --- |
| 0 (before) | a, 1 | owner e1, writable; local step 3 unshipped | — | 3 (e1 segment) | — |
| 1 fence epoch | b, 2 | owner e1, **writable** (does not read the catalog) | not owner | 3 | to b (catalog owner) store-durable: refused, not the owner; to a local-durable: **accepted** (step 4) |
| 2 seal | b, 2 | owner e1, sealed | not owner | 3 | b refused (not owner); a refused (sealed) |
| 3 ship final | b, 2 | sealed; database closed at rest, sha256 H = state at P = 5 | not owner | 5 (e1 segment 4-5: steps 3 and 4) | both refused |
| 4 rebuild | b, 2 | sealed | not owner; `db-0001.staged` = restore(5) | 5 | both refused |
| 5 verify | b, 2 | sealed | staged (position 5, sha256 = H) | 5 | both refused |
| 6 activate | b, 2 (last shipped 6) | sealed (would be refused at slot 6 if it wrote) | **owner e2, writable**; local file = staged image | 6 (e2 fence snapshot = H) | b store-durable: accepted (position 7); a refused (sealed) |
| 7 retire | b, 2 | not owner; no files for db-0001 | owner e2 | 7 | a refused (not owner). No 10.3 case reaches this boundary with b alive: case 18 reaches it with b killed after step 6 (`after step 7: catalog owner b epoch 2; store ends at position 7 (epoch 2 segment); a not owner; b down`); b's write after step 7 is shown by `TestRelocationMovesOwnershipAndKeepsEveryWrite` (accepted at the next position) and by every case's `after recovery` write |

Relocation record per step: step 1 written with the catalog fence; P and H recorded at step 3;
state `done` after step 7. Writable nodes at every boundary: 1. The source's local-durable
acknowledgements (setup step 3, boundary-1 step 4) are both in the store by step 3.

## Spans in SPANS.txt (12; facts read from the pinned source)

`go run ./cmd/course check spans ch10` → `12 span(s) start at their symbol`. The 10 spans 10a added
were re-checked (unchanged); 10b adds the last two. Copied: `wal.c` only (unchanged); linked:
`os_unix.c`.

- **wal.c 2199–2395 `walCheckpoint`.** Copies WAL frames back into the database file. It first lowers
  `mxSafeFrame` to the read mark of any reader slot 1–4 still in use (2233–2251), takes read slot 0
  exclusively (2259–2260), syncs the WAL (2277) and copies each page up to `mxSafeFrame` (2304–2321);
  only when it copied everything does it truncate and sync the database file (2328–2334). The shipper
  relies on this: its held read mark caps every checkpoint at frames it has already shipped.
- **wal.c 2152–2166 `walRestartHdr`.** Resets the WAL header for reuse: increments `nCkpt`, sets
  `mxFrame` to 0, salt-1 + 1 and a new random salt-2, zeroes `nBackfill` and read mark 1, marks marks
  2–4 unused. Frames written after it start a new WAL generation, which is why walship compares WAL
  headers and why WAL files are never compared byte for byte.
- **os_unix.c 1189–1342 (Posix advisory locking comment, `unixFileId`, `unixInodeInfo`).** The
  comment says POSIX locks belong to a process and closing any descriptor of a file drops all of the
  process's locks on it, so SQLite keeps its own per-inode lock bookkeeping. The key is the local
  device and inode (`unixFileId`); nothing in it identifies a machine, which is why ownership across
  machines lives in the catalog epoch and the store's slots.
- **os_unix.c 1282–1296 `unixFileId`.** A struct of `dev` (device) and `ino` (inode number) — the
  identity of a file for locking. It is only meaningful inside one kernel.
- **os_unix.c 1323–1342 `unixInodeInfo`.** Per-inode lock state shared by all connections of the
  process: `nShared`, `nLock`, `eFileLock`, a list of descriptors whose close is deferred
  (`pUnused`), and the shared-memory node. It is why the `-shm` of a database is one descriptor per
  process (7 descriptors per open service database, not 9).
- **os_unix.c 1802–1833 `unixFileLock`.** Every database-file lock change ends in
  `osSetPosixAdvisoryLock` (an `fcntl` on the local descriptor), with a process-wide shortcut for the
  `unix-excl` VFS. Nothing survives the process.
- **os_unix.c 1866–2095 `unixLock`.** Moves a connection's lock through SHARED, RESERVED, PENDING and
  EXCLUSIVE using the lock bytes at `PENDING_BYTE`, consulting `unixInodeInfo` first. The shipper's
  `BEGIN IMMEDIATE` and the writer's transactions exclude each other through it, on one machine.
- **wal.c 2507–2571 `sqlite3WalClose`.** The last connection to close takes an EXCLUSIVE lock, runs
  a checkpoint and, unless persist-WAL is set, deletes the `-wal` (2528–2557) and the `-shm`. This is
  why an evicted or drained database is one file at rest (relocation step 3, 10.4's quiesce finds 0
  frames).
- **wal.c 3020–3272 `walTryBeginRead`.** Picks a read slot for a new read transaction: slot 0 when
  the WAL is fully backfilled, otherwise a slot whose mark is ≤ `mxFrame`. The shipper's pin is such
  a read transaction; its slot is what caps or blocks checkpoints.
- **wal.c 3875–3915 `walRestartLog`.** A writer restarts the WAL from the beginning only when every
  frame was backfilled and it can lock read slots 1–4; so nothing the shipper has not shipped can be
  overwritten.
- **wal.c 1390–1614 `walIndexRecover`** (new in 10b). Runs when a connection opens a WAL whose
  wal-index is not valid, as after a killed process. Under exclusive locks (1407) it reads the WAL
  header and checks its magic number and checksum (1445–1466; a bad header means the whole WAL is
  ignored), then walks the frames in order, verifying each frame's checksum chain with
  `walDecodeFrame` (1510) and stopping at the first invalid frame; `mxFrame` is set only at frames
  that carry a commit size (1516–1518). It writes the rebuilt header, sets `nBackfill` to 0 and resets
  the read marks (1572–1590). So a database reopened after 10.2's kills contains exactly the
  transactions whose commit frame was complete in the WAL, which is every acknowledged one.
- **wal.c 4038–4266 `walFrames`** (new in 10b). Appends a transaction's frames to the WAL. A new WAL
  gets its header written and synced first (4108–4124: the header sync is needed so a later
  out-of-order write cannot corrupt a restarted WAL). For a commit at `synchronous=FULL` it syncs the
  WAL after the last frame (4195–4213, padding to a sector boundary when the file system asks for it)
  before returning, which is the sync 10.2's local-durable kill points land on and why a
  local-durable acknowledgement survives a process kill.

## Platform implications: answers to the course doc's eight design questions

1. **How many connections and file descriptors can remain open?** Per open service database: 3
   SQLite connections and 7 descriptors (10a correction 5); per node K open databases (LRU). Bound
   per node process: `3 × 3 × K + 32` descriptors above its baseline. K must satisfy
   `9K + 32 ≤ soft limit` (10.4 checks this before starting; at K = 32 the bound is 320). Held in
   every reply of every experiment (10.2: 316, 10.3: ~1,670, 10.4: ~2,770 replies).
2. **How much cache memory is consumed per database?** `cache_size = -256` (256 KiB) per connection,
   3 connections per open database; the bound uses 8.3's figure per connection (2 × cache + 256 KiB)
   plus 32 MiB per process: 104 MiB for K = 32 (10.4 writers), 41 MiB for K = 4. The bound held in
   every sample. Per-database cost is paid only while the database is open; closed databases cost a
   file on disk and a catalog row.
3. **What is the write-ownership model?** One owner per database named by the catalog with an
   epoch; the owner's writes are accepted by the store only at the next free position after its own
   last object; a new owner's first object (a fence snapshot) takes that position. Ownership moves by
   takeover (`claim`, 10.1) or by relocation (seal, drain, fence; 10.3). The store's put-if-absent is
   the arbiter; the catalog is advisory until the fence lands.
4. **What happens when a reader blocks checkpoints?** The shipper's own pin is a reader that caps
   checkpoints at shipped frames on purpose (wal.c 2233–2251); writers run `wal_autocheckpoint=0`
   so only the shipper checkpoints, PASSIVE after every ship. A long user reader would cap those
   PASSIVE checkpoints too and the `-wal` would grow; 10b did **not** test a long reader (chapter 5
   section 7 and chapter 9 show the WAL growth; the relocation's drain would wait for the reader only
   at close, where the last-connection checkpoint needs the EXCLUSIVE lock). Signal to export: WAL
   frames vs checkpointed frames per cycle (`CycleReport.Checkpoint`).
5. **How are consistent snapshots produced?** Two ways: a shipper snapshot reads every page through
   `sqlite_dbpage` inside its `BEGIN IMMEDIATE` (chapter 9), and a fence snapshot is a store restore
   (snapshot + segments replayed in Go, checked by sha256 after every segment). 10.4: every published
   restore point (3,248 at 500 databases) rebuilds to a recipe state.
6. **How is a database moved between machines without two writers?** The seven-step relocation;
   the seal stops the cooperative source, the store fence stops any source; the destination writes
   only after its fence is accepted. 10.3: 32 fault cases, never two writable nodes; without the seal
   and fence (self-test) two nodes are writable at once.
7. **Which metadata belongs in a global control plane versus local SQLite files?** Control plane
   (catalog): owner, epoch, location, last shipped position, schema version, per-connection settings,
   relocation records (step, fence point, state). Local: the database file itself (and its file
   settings: page size, encoding, application_id, user_version, journal mode), plus the node's
   crash-surviving beliefs (`.owner`, `.atrest`, `.sealed`, `.staged`). The store: the lineage, which
   is the authority on what was accepted (a stale catalog `last_shipped` is corrected by the next
   ship; `ErrStaleEpoch` after an accepted put is ignored).
8. **What is the recovery path after an interrupted upload or ownership transfer?** Upload: a failed
   put leaves the shipper broken; the database is evicted and reopened from the store (ships what
   is unshipped, as a segment or a discontinuity snapshot); an ambiguous put (stored, error returned)
   is read back — identical bytes count as success (10a); a crash after a put is found by the next
   listing. Transfer: the relocation row says where it stopped; the store says whether the fence
   landed; fence present → roll forward (adopt), absent → resume or roll back; newer epoch present →
   superseded. 10.3 covers each step.

**Per-database signals worth exporting** (all available from this code): owner and epoch (catalog);
last shipped position and schema version (catalog); local commit position − last shipped position
(the unshipped lag; local-durable exposure); WAL frames and checkpointed frames of each cycle, and
whether the last checkpoint copied every shipped frame (`CycleReport.Checkpoint`, `FullBackfill`);
WAL change kind per cycle (same generation / new generation / discontinuity — discontinuities mean
full snapshots); at-rest sha256 at each clean close; relocation step and state; sealed / fenced
status; last good restore point and restore-point failures (`fence.Inspect`); store objects and bytes
per database (restore cost); per node process: descriptors and RSS growth against the bound.

**What this design does not handle** (for the chapter's list):
- Multiple machines: the catalog is one SQLite file on one host, shared through its locks; a real
  control plane would be a service with the same operations (and its own availability problem).
- Automatic failover: ownership moves only by an explicit claim or relocation; no leases, no
  timeouts, no failure detector.
- Concurrent relocations of one database are refused (`ErrRelocationRunning`), but nothing schedules
  or retries them; a coordinator that never comes back leaves the row `running` until someone calls
  resume or rollback.
- Periodic snapshots, compaction and pruning (correction 9): restore cost grows with the number of
  segments since the last snapshot.
- Garbage: a fenced node's orphaned local file stays until re-claimed or retired; nothing is ever
  deleted from the store.
- Non-atomic stores: a partial object in a slot wedges that database (the slot cannot be rewritten).
- Power loss: only process kills were injected; chapter 9.2's model applies.
- Clients: acknowledgements are per command; a client that never sees a reply cannot tell a failed
  write from an unacknowledged success (10.2's store-durable kill at `catalog.db-wal`).
- Security, quotas, many tenants per node, schema migrations beyond one recipe step, version
  upgrades of SQLite (byte hashes are only stable within one library version).

## WAL-default evaluation for this chapter (ruling 10)

WAL holds and is required by the design, not just its default: walship reads committed frames from
the `-wal` and pins a read mark (chapter 9), which rollback mode has no equivalent for; the catalog
also runs WAL (several node processes read it while one writes). Costs found in 10b, each measured:
fsyncs per store-durable write (85 for 12 writes in 10.2, including the shipper's checkpoint and the
catalog commit) make the fleet's write stream I/O-bound (10.4); a database's close deletes the WAL,
so a drained or evicted database is one file at rest — which relocation step 3 relies on — but a
drain whose put fails after the close turns the unshipped frames into a discontinuity snapshot
(correction 2). No experiment in 10b runs in rollback mode. No exception to WAL as default.

## Linux-only parts and the Mac route

- 10.2 part A (kills at a chosen fsync) needs strace and ptrace: Linux only. On other systems it
  prints the `LINUX-ONLY:` line (above) and parts B and C run; exit 0. Mac route: `docker compose up
  -d seaweedfs`, then `docker compose run --rm lab go run
  ./ch10-multi-database-platform-capstone/experiments/10.2-acknowledged-writes-survive` (the lab
  container has strace, `SYS_PTRACE`, and `COURSE_S3_ENDPOINT=http://seaweedfs:8333`). Not run in
  this chunk.
- 10.3 and 10.4 are portable (S3 over HTTP, SIGKILL via `os.Process.Kill`, descriptors via `/dev/fd`
  and memory via `ps` on macOS through `procstat`). 10.4's descriptor check reads the soft limit with
  `procstat.FileLimit`; Go raises the soft limit to the hard limit at startup on both systems.
- darwin: cross-built and cross-vetted only (`agent_work/verify/crossbuild.sh`), never run.

## Surprises worth a sentence in the chapter

- Bumping the epoch is not a fence: the source keeps writing (correction 1). The seal is a courtesy;
  the store fence is the guarantee.
- A failed final ship changes the shape of the lineage (correction 2): the close checkpoints the
  unshipped frames and the retry has to ship a full snapshot.
- 10.2's store-durable kill points land on the catalog's commit more often than on the database's:
  after a ship, the slowest durable step of a write is the control-plane update.
- The fleet rebuild reads every object twice (correction 6), and recovery from the store alone is a
  takeover of every database (correction 8).

## 2,000 databases (the outline's size for 10.4)

Command: `go run ./ch10-multi-database-platform-capstone/experiments/10.4-rebuild-the-fleet
-databases 2000 -prefix exp-10-4-2000` (run with `COURSE_WORK_DIR` pointing at a separate scratch
base so it could run beside the evidence check). Output: `evidence/10.4-rebuild-the-fleet-2000.txt`
(one run; not part of `course check evidence`, which runs the default 500). Exit 0, all checks
passed:
- `fleet: 2000 databases, 8 writer processes (each owns 250 and keeps at most 32 open)`;
  `schema rollout: … applied to 1000 of 2000 databases`.
- Concurrent checker: 15 passes, 33,731 restore points rebuilt while writers ran, 0 failures.
- `quiesce: … 2000 databases recorded after a TRUNCATE checkpoint (WAL frames found: 0)`.
- `rebuild read 22000 objects in 2000 listings and wrote 2000 fence objects`;
  `rebuilt: 2000 of 2000 byte-identical (E1) …; 1000 at recipe step 4, 1000 at recipe step 5`.
- `final checker pass: 2000 databases, 13000 restore points, 0 fail to rebuild` (13,000 = 1000 × 6
  + 1000 × 7); `the rebuild read each of the 11000 slots it found exactly twice`.
- Bounds held in all 11,025 replies, the in-process rebuild, and the checker.
- SHAPE (INFO, slowed by the evidence check running at the same time on the same 4 cores): write
  stream 3 min 38 s, rollout 49 s, quiesce 6 s, rebuild 1 min 28 s (44 ms per database with 8
  workers), final pass 2 min 7 s; rebuild read 1.54 GB (769 KB per database), the store held 1.19 GB;
  total 9 min 12 s; the parent process's peak RSS 62 MiB (`/usr/bin/time -v`).

## Repair round 1 (after REVIEW-1 and adversarial run 1)

What changed, as facts for the chapter (details are folded into the sections above):

- `relocate.Coordinator.Rollback`: order is now seal → drain → source `Claim` → catalog
  `FinishRelocation(rolled-back)` → destination `Discard` (best effort). `Result` has a new field
  `DiscardErr error` (nil when the discard worked). Before, a down destination made `Rollback` fail
  after the source had already re-claimed: the catalog said `owner=a epoch=3`, the relocation row
  stayed `running`, no new relocation could start, and a retry finished as `superseded` (REL-04).
  Now: `rolled back`, row `rolled-back`, a new relocation can begin. Test
  `TestRollbackWhileTheDestinationIsDown`; 10.3 case 32.
- `Rollback` retry after a stop between the source's claim and the catalog record: the newest slot
  epoch is above the relocation's and the catalog names the source as owner at that epoch → finished
  as `rolled back` (and discards again), not `superseded`. Test
  `TestRollbackFinishesARollbackThatStoppedAfterTheClaim`; a takeover by any other node is still
  `superseded` (`TestRollbackStillReportsAThirdNodesTakeover`).
- `node.Activate` repeated by the node that already holds the epoch: when the store still ends at
  the fence (P+1), it re-records the catalog (location `<node>:<id>.db`, last_shipped P+1,
  schema_version = the fence snapshot's user_version). Before, the row kept `location=""` and the
  old last_shipped/schema_version forever if the first activation's catalog update failed or the
  node died between writing `.owner` and the catalog (REL-05c: `loc="" lastShipped=3 schemaVer=2`
  after a `done` relocation). If the node has shipped past the fence, the row is not touched (its
  own writes recorded it). Tests `TestRepeatedActivateRepairsAnUnrecordedFence`,
  `TestRepeatedActivateAfterLaterWritesKeepsTheCatalog`.
- `node.Drain` returns an error (`node: drain <id>: the final ship left the state at rest but
  reported: …`) when the final eviction reached at-rest but reported a problem (e.g. the `.atrest`
  file or the catalog record failed); before, those problems were dropped and `Drain` returned
  success. Test `TestDrainReportsAFailedAtRestRecord`.
- 10.2 part B: random kill window and redraw (see the 10.2 section). 10.3: 32 cases, `no node
  reported a problem` check, processes of a failed case are killed. 10.4: `each owns 62-63` (a
  range only when the split is uneven; the 2,000-database line `each owns 250` is unchanged).
- Runtimes in the repair runs: 10.4 2 min 16 s and 2 min 30 s (`/usr/bin/time`); 10.2 and 10.3 were not re-timed.
