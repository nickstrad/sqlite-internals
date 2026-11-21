# Chunk 10a build notes (plain data for review, attack and the prose pass)

Builder: Claude Opus 5.5, branch `chunk/ch10a` (cut from main at 5495d45, chapters 1–9 landed),
2026-11-21. Machine: linux/amd64, Go 1.26, ext4 under `_work/`, 4 shared cores. Pinned tree
`$SQLITE_SRC` = SQLite 3.53.4. Store: SeaweedFS 4.48 from `compose.yaml` (`docker compose up -d
seaweedfs`, S3 on host port 18333), bucket `course-ch10a` (unique to this chunk), one key prefix per
use (`exp-10-1/` for the experiment, `test-s3store/` for the unit test). darwin: cross-built and
cross-vetted only (`agent_work/verify/crossbuild.sh` → `CROSSBUILD OK`), never run.

This chunk is the first half of chapter 10: the service core and experiment 10.1. No chapter text.
Chunk 10b adds relocation, experiments 10.2–10.4 and the chapter.

Chapter directory `ch10-multi-database-platform-capstone/`: `wal.c` (whole copy), `SOURCES.sha256`,
`SPANS.txt` (10 spans), `service/` (7 Go packages), `experiments/10.1-one-writer/main.go`,
`evidence/10.1-one-writer.txt`. Linked, not copied: `os_unix.c` (as the outline says).

Every claim below names the line of evidence or the test that shows it. Line numbers of the C
source are at the pin.

## Corrections to the outline

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
4. **Local-durable acknowledgements of the old owner are lost by a takeover** — as the outline's
   durability model implies but does not say. Real step 3 acknowledges user_version 2 at
   `synchronous=FULL` (local-durable); node b's claim restores the store's last position (2,
   user_version 1) and fences after it; the evidence line `db-0001 local-durable acknowledgement at
   epoch 1 of user_version 2: in the store false` and the check `the local-durable acknowledgement
   … was never shipped, and the takeover lost it, as that mode allows`. So 10.1's invariant ("no
   acknowledged write is lost") holds for store-durable only; the chapter must name the mode.
5. **Descriptor bound per open database is 7, not 3** (8.3 measured 3 per *connection*). A
   service-held database has three SQLite connections: the writer (connmgr's) and walship's two
   (control and pin). Measured: 3 × `.db` + 3 × `-wal` + 1 `-shm` (the `-shm` is one descriptor per
   process per file, shared by the connections through `unixInodeInfo`/`unixShmNode`) = 7. Evidence
   INFO lines show 0 / 7 / 14 above the baseline with 0 / 1 / 2 databases open; a probe listed them
   (`agent_work/ch10a/INDEX.md`). The standing assertion uses 8.3's per-connection figure as an
   upper bound: `3 descriptors × 3 connections × K + 32 reserve` (68 for K = 4) and 8.3's memory
   formula per connection (`connections × (2 × cache + 256 KiB) + 32 MiB`, 41 MiB for K = 4 at
   `cache_size = -256`: 12 × 768 KiB + 32 MiB).
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

## Shared packages (every change)

| Package | Change | Why | Test |
| --- | --- | --- | --- |
| `internal/walship` | **new file** `atrest.go`: `func (s *Shipper) AdoptAtRest(ctx) (bool, error)` | see below; a new file because `shipper.go` is inlined byte-for-byte in chapter 9 | `atrest_test.go`: `TestAdoptAtRestSkipsTheReopenSnapshot`, `TestAdoptAtRestRefusesAChangedFileOrUnshippedFrames` |

Nothing else changed. `internal/connmgr` is **not** changed: a `BeforeClose` hook was written and
reverted, because `connmgr.go` is inlined byte-for-byte in chapter 8 (`check inline` would fail).
`go.mod` unchanged (aws-sdk-go-v2 was already required by `internal/rangefs`).

**`AdoptAtRest` — what it does and why it is needed.** When the service evicts a database it closes
all connections; the last close checkpoints and deletes the `-wal` (`sqlite3WalClose`, wal.c
2507–2571, lines 2528–2557: PASSIVE checkpoint under an EXCLUSIVE lock, then `isDelete = 1` unless
PERSIST_WAL). On the next open, `walship.Open` resumes with its last shipped WAL position, which
names the old WAL generation; the live file has no WAL, the shipper held no read mark in between,
so `classify` returns `WALDiscontinuity` ("the WAL header changed while the shipper held no read
mark") and the first cycle uploads a **full snapshot**. Correct, but every reopen of an evicted
database would cost one whole-database upload. `AdoptAtRest` runs under `BEGIN IMMEDIATE` on the
shipper's control connection: if the `-wal` holds no committed frame (`lastCommit(file) == 0`:
missing, empty, or no commit frame) **and** every page read through `sqlite_dbpage` hashes to the
shipper's last shipped state, it sets the shipper's WAL position to "no WAL", marks it continuous and
fully backfilled, and returns true; otherwise it changes nothing and returns false. A failure marks
the shipper broken (like `Cycle`). Why it is safe: the file *is* the shipped state (hash equal);
the next WAL generation starts at frame 1, which `Plan` accepts as a new generation (seed = the new
header's checksum); the pin taken in `Open` is on read slot 0 when there is no WAL, which blocks
every backfill (wal.c 2259–2260 takes slot 0 exclusively to backfill), so no frame can be
checkpointed away before the next cycle ships it. Test 1: a shipper ships step 1, closes (file at
rest); a plain reopen's first cycle publishes a discontinuity snapshot (the cost); a reopen with
`AdoptAtRest` publishes no snapshot, the next commit ships as a segment with `FirstFrame 1` and
change `WALNewGeneration`, every position 1–4 restores to the expected user_version, and the closed
file equals the restored last position. Test 2: a file changed while closed (step 1 applied
outside) is not adopted, a `-wal` with commit frames is not adopted, and the next cycle takes a
discontinuity snapshot as before.

## The service (`ch10-multi-database-platform-capstone/service/`, 7 packages)

Line counts (non-test / test): s3store 126/59, faultstore 129/105, fence 306/221, catalog 117/71,
bounds 98/49, model 429/99, node 697 (node.go 449, serve.go 85, process.go 163)/234; experiment
main.go 361; walship/atrest.go 33/161. Total non-test Go of this chunk ≈ 2,300 lines — above the
brief's ~1,100 target (see BUILD-REPORT). No comments in any file.

### Component map (for the Model section's diagram)

```
client ──► node (router: id → connmgr handle; ownership table; ack mode)
             ├─ connmgr (8.3): ≤ K writer connections, LRU eviction
             ├─ per open database: walship.Shipper (control + pin connections)
             │      └─ fence.Adapter (epoch e) ──► faultstore (optional) ──► s3store ──► SeaweedFS
             └─ catalog (one SQLite database, shared by all nodes)
```

### `s3store` — the walship.Store contract on SeaweedFS S3

- `Connect(ctx, rangefs.S3Config, bucket, prefix) (*Store, error)`: builds the aws-sdk-go-v2 client
  with `rangefs.NewS3Client` (path-style, the probe's recommended client), creates the bucket if
  needed (`rangefs.EnsureBucket`) under a 10-second deadline. Any failure → `ErrUnreachable`
  wrapped with the endpoint and the hint `start it with: docker compose up -d seaweedfs`. The prefix
  must be a valid walship key; every key is stored as `<prefix>/<key>`.
- `PutIfAbsent`: `PutObject` with `IfNoneMatch: "*"`; error code `PreconditionFailed` →
  `walship.ErrExists`. This is the probe's measured atomic put-if-absent (exactly one winner among 2
  and 8 concurrent writers, `agent_work/probes/seaweedfs-s3/RESULT.md`).
- `Get`: `NoSuchKey`/`NotFound` → `walship.ErrNotFound`; refuses a body shorter than
  Content-Length.
- `List(prefix)`: `ListObjectsV2` paginator; strips the store prefix; returns keys **in the store's
  order** (no promise — the probe showed SeaweedFS does not list in byte order when `/` mixes with
  lower bytes; walship and fence sort themselves, and every key here is fixed-width).
- `Delete` (S3 delete is idempotent), `Wipe(ctx)` (delete every key under the prefix; used at the
  start of each run so evidence starts from an empty prefix).
- What it relies on from the probe and nothing more: atomic `If-None-Match: *`, read-after-write,
  list-after-write (both measured consistent on SeaweedFS 4.48, single server). It does **not**
  rely on list order or on `If-Match`. Not covered: multi-host clients, server restarts mid-request
  (as in the probe).
- Hazard handled in `fence`, not here: the SDK retries a PUT on a 500 or a dropped connection. If
  the first attempt was stored and the retry gets 412, the caller sees "exists" for its own object.
  `fence.Adapter.PutIfAbsent` re-reads the slot and treats byte-identical content as success (test
  `TestRetriedIdenticalPutSucceeds`).
- Test `TestS3StoreSemantics` (skips with a message when the store is not running): put, second put
  → `ErrExists`, get, missing get → `ErrNotFound`, list by prefix, invalid key rejected before any
  request, delete twice, wipe count, closed port → `ErrUnreachable`.

### `faultstore` — fault injection around the real store

`New(base, rules...)`; `Rule{Fault, Match (substring of the key or list prefix), Nth (1-based count
of matching calls)}`; the rule fires once, on the Nth matching call. `ParseRule("fault:match:nth")`
(strict: three fields, known fault, positive integer). `Fired()` lists what fired. Faults:

| Fault | What the wrapper does | Models |
| --- | --- | --- |
| `fail-before-put` | returns `ErrInjected`, stores nothing | a put that never reached the store |
| `fail-after-put` | stores, then returns `ErrInjected` | an ambiguous put (stored, client saw an error) |
| `partial-put` | stores the first half of the bytes, returns `ErrInjected` | what a store *without* atomic put would leave; S3 single PUTs are atomic (probe), so this is a stress case for the readers |
| `crash-before-put` / `crash-after-put` | calls `Crash` (default `os.Exit(3)`) before / after the real put | process death on either side of the put |
| `stale-list` | `List` omits the lexicographically last key once | delayed list visibility |

The store underneath stays real. 10.1 uses `stale-list:/log/:1` on node b; the others are for 10b
(10.2–10.4) and are covered by `TestPutFaults`, `TestCrashFaultsCallTheHook`,
`TestStaleListHidesTheNewestKeyOnce`, `TestParseRule`. A `stale-list` rule counts only non-empty
listings (an empty listing has nothing to hide, so it neither consumes the Nth count nor appears
in `Fired()`; test `TestStaleListSkipsEmptyListings`). `s3store.List` validates a non-empty prefix
like the other methods.

### `fence` — the fencing scheme (object keys and conditional put)

**Keys.** One key space per database: `dbs/<id>/log/<position as 12 decimal digits>`. Every object
that advances a database's stream goes to the key of its *first* position: a snapshot at position
p → `log/p`; a segment covering positions p..q → `log/p`. Fixed width, so byte order = numeric
order, and no key mixes `/` with bytes below 0x2f at one level (the probe's listing caveat).

**Envelope.** Each slot object is one line `fenced-slot v1 epoch=<e> kind=<snapshot|segment>
first=<p> last=<q>` followed by the unchanged walship object (chapter 9's format). `Decode` is
strict: the line must re-encode to itself (no padding, no extra fields), epoch ≥ 1, `first` must
equal the key's position, `last ≥ first`, the kind must be snapshot or segment, and the embedded
walship object must decode (`DecodeSnapshot`/`DecodeSegment`) with the same positions. Anything
else is `ErrDamaged` (12 malformations in `TestEnvelopeRoundTripAndStrictDecode`).

**The rule the store enforces.** `PutIfAbsent` (`If-None-Match: *`) on the slot key: a position can
be written once. Nothing is ever overwritten or deleted by a writer (`Adapter.Delete` refuses).

**The rule each writer follows** (`Adapter`, one per database per epoch):
- `Refresh(ctx)`: list `dbs/<id>/log/`, read and decode every slot, check the lineage
  (`CheckLineage`: starts with a snapshot at position 1; each slot starts at the previous slot's
  `last + 1`; epochs never decrease; a slot whose epoch is higher than its predecessor's must be a
  snapshot, i.e. every epoch begins with a fence). If any slot has an epoch higher than the
  adapter's → `*FencedError{Database, Epoch, Holder, Position}` (`errors.Is(err, ErrFenced)`).
  Otherwise the adapter remembers the chain end. The adapter's cached view is replaced only after
  the whole chain passed: a fenced `Refresh` leaves the adapter unlisted, so a later `List`/`Put`
  re-lists and is fenced again (repair: before, a fenced refresh left a truncated pre-fence view
  that `List` returned with no error). The cache keeps only each slot's metadata (epoch, kind,
  first, last), never the object bytes: `Get` reads the object from the store and decodes it each
  time, and refuses (`ErrDamaged`) an object whose epoch or range differ from the listing
  (repair: before, every shipped segment stayed in RAM for as long as the database was open —
  attack CAT-05b measured 37 MB of growth for 180 × 256 KiB commits on a 276 KiB database).
- `PutIfAbsent(walship key, object)` (called by walship's shipper or by the claim): the object must
  start at exactly `end + 1` (else `ErrOutOfOrder`, nothing sent). The envelope is decoded and
  validated (`Decode`: snapshot object decodes and its position matches; segment range matches)
  **before** anything is sent, so a malformed object is refused with `ErrDamaged` and never
  becomes an immutable slot. Then the conditional put of the
  envelope. On "exists": read the holder; identical bytes → success (own retried put); holder epoch
  higher → `FencedError`; otherwise `walship.ErrExists` with the holder's epoch (a lower or equal
  epoch got there first — the caller re-lists).
- `List`/`Get` give walship the view it expects (`snapshots/<p>`, `segments/<p>`) from the cached
  listing; `List` lists the store only the first time (or after `Refresh`), so a claim computes its
  image and its fence position from **one** listing (this was a bug found by the real run during
  the build: with re-listing inside `LoadCatalog`, the claim's fence position came from one listing
  and the adapter's end from another, and the fence was refused as out of order — safe but wrong).

**Why this enforces one writer.** By induction over positions: every accepted object at position
p > 1 was put by a writer whose own previous object ends at p − 1, or by a new epoch's fence placed
after the end it listed. When epoch e+1's fence is accepted at F, any epoch-e writer's next put is
at its own end + 1 ≤ F (its own objects are all before F, since F was free when e+1 listed the end
and positions are filled contiguously), so it lands on an occupied slot and is refused; a writer
of epoch e that restarts lists the slots and sees epoch e+1 → fenced before it writes. A stale
listing cannot break this: a fence placed after a stale end lands on an occupied slot and is
refused ("exists"), and the claim retries with a fresh listing. The model checks exactly this
argument mechanically (10.1 part A).

**`Chain(ctx, base, id)`** (lineage-checked list of slots, any epoch), **`Audit(ctx, base, id)`**
(chain + a walship catalog through an adapter with epoch `MaxUint64`, i.e. a reader that is never
fenced and never writes), **`RestoreAt(catalog, position)`** (plan + execute). Used by 10.1's
verification and by tests.

Tests: `TestAdapterAppendsOnlyAtTheEnd` (empty chain refuses position 2; snapshot 1, segment 2–4;
overlapping put at 4 refused; list keys; get by kind; refresh end 4; delete refused),
`TestNewerEpochFencesTheOlderWriter` (put refused with `Holder 2, Epoch 1, Position 2`; restarted
epoch-1 adapter fenced at refresh; epoch 2 fenced by epoch 3), `TestRetriedIdenticalPutSucceeds`,
`TestLowerEpochHolderIsReportedAsExists`, `TestCheckLineage` (5 broken lineages),
`TestNewRejectsBadIdentity`; added in repair: `TestListAfterFencedRefreshStaysFenced`,
`TestMalformedPutNeverReachesTheStore`, `TestDecodeRejectsMalformedKeys` (a slot key must be
`dbs/<valid id>/log/<12 digits>`: `dbs//log/…` is damaged), `TestAdapterKeepsNoObjectBytes`.

### `catalog` — the control plane (one SQLite database, WAL)

Table `databases(id TEXT PRIMARY KEY, owner, epoch, location, last_shipped, schema_version, settings
TEXT) STRICT`. Opened with `sqlitex.DefaultOptions()` (WAL, `synchronous=FULL`, busy timeout 5 s),
single connection, `cache_size=-256`. Shared by every node process through SQLite's own locking (all
nodes are on one machine in this chunk; a real control plane would be a service — the chapter
should say so).

- `Register(id, sqlitex.ConnSettings)`: insert if absent; settings validated and stored as JSON.
  These are the per-connection settings from 8.1 (`synchronous`, `foreign_keys`, `secure_delete`,
  `cache_size`, `wal_autocheckpoint`, `busy_timeout`); the file settings live in the file.
- `Get(id) Record{ID, Owner, Epoch, Location, LastShipped, SchemaVersion, Settings}`: settings
  decoded strictly (unknown JSON field → error) and validated.
- `Claim(id, node) (epoch)`: `UPDATE … SET owner = ?, epoch = epoch + 1, location = '' … RETURNING
  epoch` — one statement in one immediate transaction, so two concurrent claims get two different
  epochs. A claim is a takeover: it does not ask the old owner.
- `RecordShipped(id, epoch, position, schemaVersion, location)`: `UPDATE … WHERE id = ? AND epoch
  = ?`; 0 rows → `ErrStaleEpoch`. The node ignores `ErrStaleEpoch` after a store-accepted ship
  (the store is the authority on acceptance; see correction 3).
- "Schema version" here = the database's `user_version`, which the course recipe sets to the step
  number (so 10.4's "old or new schema" is the recipe step). Chunk 10b may redefine it.
- Test `TestClaimBumpsTheEpochAndFencesOldRecords` (two handles on one file; idempotent register;
  epochs 1, 2; stale `RecordShipped` refused; unknown id; invalid settings; unknown JSON field).

### `bounds` — the standing assertion

`Bounds{OpenDatabases K, ConnectionsPerDatabase, CacheBytes, ReserveDescriptors}`;
`DescriptorLimit() = 3 × connections × K + reserve`; `MemoryLimit() = K × connections × (2 ×
CacheBytes + 256 KiB) + 32 MiB` (8.3's per-connection terms: factor 2 = chunk 4's allocator
finding, 256 KiB = 8.2's uncounted part, 32 MiB = 8.3's constant). `CacheBytes(cache_size,
page_size)` (negative = KiB). `Start(b)` records the descriptor count (`procstat.DescriptorCount`)
and settled RSS (GC + `FreeOSMemory`, `procstat.ReadMemory`) as the baseline; `Check()` samples
both, keeps `Peak` and `Checks`, returns `ErrExceeded` when either is over its limit; `Summary()`
prints the limits (stable text). Portable: `/proc/self/fd` + `/proc/self/status` on Linux,
`/dev/fd` + `ps` on macOS (procstat, chunk 8). Every node process calls `Check()` after every
command and reports `bounds descriptors=<n> memory=<bytes> ok|EXCEEDED`; 10b's experiments are
meant to call the same helper. Tests: `TestLimits`, `TestCheckFailsWhenDescriptorsExceedTheBound`
(4 open files against a limit of 1 → `ErrExceeded`; back within bounds after closing).

### `node` — router, connection manager, capture, ownership, acknowledgement modes

`Start(Config{Name, Dir, Catalog, Store, Capacity, File, Create})`. One `sync.Mutex` serialises
every public call (simple; a throughput limit, like connmgr's). Ownership beliefs are files
`<Dir>/<id>.owner` holding `epoch N\n` (strictly parsed at start; epoch 0 is refused), so a
restarted node believes what it believed before the crash. At-rest records are files
`<Dir>/<id>.atrest` holding the 64-hex sha256 of the file a clean eviction or close left (strictly
parsed at start), so the changed-while-shut check also covers a node restart after a clean close
(repair: before, the record lived only in memory and an offline change made between a clean close
and a restart was published to the store as a discontinuity snapshot — attack CAT-04b). The record
is deleted once the reopen check passes (before the shipper opens), so a crash while open leaves no
record and the next start treats the file as post-crash (ships what is unshipped).

- **Router**: `Write(ctx, id, mode, apply)`, `Ship(ctx, id)`, `Read(ctx, id, read)` route by
  database id to the connmgr handle, after checking this node's ownership table (not owner →
  `ErrNotOwner`; known fenced → the stored `FencedError`). Cluster-level routing (which node owns
  id) is a catalog lookup (`catalog.Get(id).Owner`); 10.1's parent sends commands to nodes directly
  (it must, to send writes to the stale owner on purpose).
- **Connection manager**: `internal/connmgr` unchanged, capacity K. Its `Open` hook is
  `openDatabase`: refuse if not owner; refuse if the local file is missing (`ErrMissingLocalFile` —
  otherwise SQLite would create an empty database and walship would ship it as a discontinuity
  snapshot, publishing data loss); if this node recorded an at-rest sha256 for the id, refuse a
  file that is not at rest or has another hash (`ErrChangedWhileShut`); read the settings from the
  catalog; open the shipper on a `fence.Adapter` for the node's epoch (this listing is where a
  restarted stale owner is fenced); `AdoptAtRest`; one `Cycle` (ships anything committed locally
  but not shipped, e.g. after a kill); then open the writer connection with the catalog's settings
  (`SingleConnection`, `_txlock=immediate`).
- **Eviction** (`OnEvict`, after connmgr closed the writer): connmgr's own at-rest check runs while
  the shipper is still open, sees `-wal`/`-shm` and reports "not at rest" **without opening the
  file** (its early return), so chapter 6's close-drops-locks hazard does not arise; connmgr
  therefore counts every node eviction as `EvictedNotAtRest` and keeps no record (measured:
  `Evictions 2, EvictedAtRest 0, EvictedNotAtRest 2`). The node then runs a final `Cycle` (unless
  fenced or broken), closes the shipper (the last connection: checkpoint + delete `-wal`/`-shm`),
  runs `connmgr.CheckAtRest` itself and requires the file's sha256 to equal the shipper's last
  shipped state hash; it records that hash for the reopen check. Any failure becomes a "problem"
  line (`TakeProblems`). So at every clean eviction the node proves **local file = last stored
  state, byte for byte**.
- **Capture**: `internal/walship` per open database, `SnapshotEvery = 0` (no periodic snapshots in
  this chunk; the fence snapshots are the only snapshots). The writer's `wal_autocheckpoint` is 0
  in the platform settings: only the shipper checkpoints.
- **Catalog after every ship**: every place that ships updates the catalog row
  (`RecordShipped(id, epoch, position, user_version)`, ignoring `ErrStaleEpoch`): a store-durable
  write and `Ship` (as before), and — repair — the `Cycle` in `openDatabase` when it shipped a
  segment or snapshot (user_version read on the new writer connection) and the final `Cycle` at
  eviction/close (user_version read from the closed file's header, bytes 60–64). Before, a
  local-durable write shipped at eviction, close or post-crash reopen left the catalog at the older
  position and schema version (review finding 2, attacks CAT-01, CAT-02).
- **Ownership**: `Claim(ctx, id)`: evict if open; forget the old ownership both in memory and on
  disk (delete `<id>.owner` and `<id>.atrest` — repair: before, the in-memory belief was dropped but
  the file stayed, so a failed re-claim followed by a restart resurrected the old epoch, attack
  CAT-03a; and a stale at-rest record from an earlier eviction made the next open after a
  re-claim of a database another node had advanced fail with `ErrChangedWhileShut`, review
  finding 1); catalog `Claim` → epoch e; adapter(e); up to 3
  attempts of: `Refresh` (fenced → fail); the image at the chain end (empty chain → `Create` builds
  it in a scratch file, which must be at rest); a fence snapshot = walship snapshot object with
  `Position end+1`, reason `fence`, no WAL position, the image; `PutIfAbsent` through the adapter
  ("exists" → next attempt; fenced → fail). On success: remove local files, `walship.WriteDatabase`
  the image (fsynced), write the belief file, `RecordShipped(id, e, end+1, user_version,
  "<node>:<id>.db")` — `ErrStaleEpoch` here is ignored: once the store accepted the fence this node
  is the owner until a newer fence is accepted (correction 3); before the repair `Claim` returned
  the error while keeping the ownership and belief file (review finding 4, attacks FENCE-03a/b).
  The fence snapshot's page size is validated (power of two in 512–65536 dividing the image
  length) before it is used. Returns `ClaimResult{Epoch, RestoredFrom, Fence, Attempts}`. A crash between
  the fence put and the belief file leaves the database fenced by an epoch nobody believes in; the
  next claim recovers it (liveness, not safety). When a node learns it is fenced
  (`FencedError` from a put or a listing) it records it, drops the epoch, deletes the belief file
  and evicts the database; the local file stays and is replaced by the next claim.
- **Acknowledgement modes** (explicit per write): `LocalDurable` — acknowledged after the local
  commit; refused with `ErrNotLocallyDurable` unless the catalog's settings say
  `synchronous=FULL` (test `TestLocalDurableNeedsSynchronousFull`). `StoreDurable` — acknowledged
  only after the shipper's `Cycle` put the segment holding the commit; the `Ack` carries the stored
  position. A refused ship returns an error: the write is **committed locally but not
  acknowledged** (real step 5). After any non-fence ship failure the node marks the shipper broken
  and evicts it, so the next use reopens and resumes from the store.
- `Serve` (line protocol for a node process; `ready`, then per command one `ok …`/`refused …` line,
  optional `problem …` lines, one `bounds …` line; `close` also prints the assertion summary),
  `Explain(err)` (stable text: the `FencedError` message, `not the owner`, or `error: …`),
  `StartProcess`/`Send`/`Kill`/`Close` (drive a node child process; 10b reuses them),
  `ProcessConfig`/`RunProcess` (the node process: S3 store, optional fault rule, shared catalog,
  recipe workload; each `write` applies the next recipe step).
- Tests (DirStore double, in-process nodes): `TestTakeoverFencesTheOldOwner` (claim, store and
  local writes, takeover restores position 2 and fences at 3, stale store write refused with holder
  2 at position 3, stale local write refused, belief file deleted, new owner writes position 4,
  the owner's closed file equals `RestoreAt(4)`, catalog owner b epoch 2 last shipped 4 schema
  2); `TestRestartedNodeBelievesItsClaimUntilTheStoreSaysOtherwise`;
  `TestEvictionShipsAndVerifiesTheFileAtRest` (K = 1: eviction ships a local-durable write as a
  segment; reopen continues at position 3 with no snapshot thanks to `AdoptAtRest`; an outside
  write while closed → `ErrChangedWhileShut`; deleted file → `ErrMissingLocalFile`; re-claim
  restores and fences); `TestLocalDurableNeedsSynchronousFull`. `go test -race` passes.

### `model` — the ownership state machine

State: catalog epoch; store slots (position → `{epoch, first, last, snapshot}`); per node `{up,
role (idle|claiming|owner|fenced|stuck), epoch (volatile), belief (persisted epoch), local
(persisted local commit count), shipped (volatile)}`; the ordered list of accepted puts. Bounds:
2 nodes, ≤ 3 epochs, local commits only up to stream position 5 (`MaxLocal` bounds the node's
`local` position, which a fence sets to the fence position — so it counts positions across all
epochs and fences, not commits per node; e.g. a node that fences at 5 cannot write at all), ≤ 16
accepted puts. `ParseMutant` refuses any name but the three models (10.1 exits 1 on an unknown
`-mutant`; before the repair a typo silently ran `correct`). Actions per node (16 in all):

| Action | Enabled when | Effect (mirrors the real code) |
| --- | --- | --- |
| claim | up, not claiming/owner, epoch < 3 | catalog epoch +1; node claiming with that epoch (belief unchanged, as the real belief file is only rewritten after the fence) |
| fence / fence-with-stale-list | claiming (stale: store not empty) | list the chain (stale: without its newest object); newer epoch listed → idle; else put a snapshot at end+1: accepted → owner, belief = epoch, local = shipped = that position; slot held by a newer epoch → idle; else stays claiming ("retry") |
| write | owner, local < 5 | local +1 (a local commit; a local-durable ack) |
| ship | owner, local > shipped | put positions shipped+1..local: accepted → shipped = local (store-durable acks); held by newer epoch → fenced, belief cleared; else stuck |
| crash | up | down; role, epoch, shipped lost; belief and local kept |
| restart / restart-with-stale-list | down | up; no belief → idle; else owner at the belief epoch, list the chain: newer epoch → fenced, belief cleared; else shipped = chain end |

Invariants (checked after every step; history is in the state, so memoising on states is sound):
(1) no two accepted puts of different epochs cover overlapping positions; (2) no put of epoch e is
accepted after a put of a higher epoch was; (3) every accepted put is still the object in its slot.
Mutants: `store-accepts-every-put` (the store overwrites instead of refusing),
`resume-from-global-end` (restart resumes at the store's end regardless of epoch and treats the
local file as one new position — walship's own resume behaviour).

`Explore(mutant, depth, seed, walks, walkLength) Report`: (a) **every interleaving up to `depth`**:
a depth-first count over all enabled actions, memoised on (state, remaining depth), which yields the
exact number of action sequences of length ≤ depth (a sequence ends early only at a violation or
when nothing is enabled) and how many reach a violation; (b) breadth-first search over distinct
states up to `depth` (count, and the **shortest** violating sequence with its replay); (c) `walks`
seeded random walks of `walkLength` steps past the exhaustive depth. The seed (PCG) permutes the
action order of (a)/(b) and drives (c); counts do not depend on it (`TestCountsDoNotDependOnSeed`).
`Step`, `Replay`, `ParseScript` ("A claim, B fence-with-stale-list, …"), `State.ChainLines`,
`State.Violation`. Runtime at depth 16: ~0.5 s per model, ~100 MB peak RSS for the run.

## Experiment 10.1 one-writer (EXACT)

**Claim (as checked).** With position slots written by put-if-absent in the store and writers that
append only after their own last object, once a newer epoch's object is accepted no older epoch
gets an object accepted, no two epochs are accepted for overlapping positions, and nothing accepted
is overwritten — in every interleaving of claim, fence, write, ship, crash and restart (with stale
listings) of two nodes up to depth 16 — and two real service processes on SeaweedFS behave as the
model predicts in a stale-owner scenario, including a stale owner that is killed and restarted
believing it still owns the database. Store-durable acknowledgements survive the takeover;
local-durable ones from the old owner do not.

**Run:** `docker compose up -d seaweedfs`, then `go run ./ch10-multi-database-platform-capstone/experiments/10.1-one-writer`.
Runtime 3.5–6.5 s (two runs: 4.8 s and 3.5 s wall, peak RSS 70–250 MB including `go run`'s
build). Flags: `-depth 16 -seed 1 -walks 2000 -walk-length 60` (defaults); `-mutant
<correct|store-accepts-every-put|resume-from-global-end>` runs part A's main check against a mutant
(the check then FAILS and the experiment exits 1 — the "can fail" demonstration; the other mutant
still runs as a self-test); `-model-only` runs part A only and needs no store; `-bucket`, `-prefix`,
`-capacity` (defaults `course-ch10a`, `exp-10-1`, 4); `-role node -name -work -fault` are internal
(the node child process). Scratch: `_work/10.1/` (catalog.db, `a/`, `b/`), wiped at start; the S3
prefix is wiped at start.

**Store unreachable** (e.g. `COURSE_S3_ENDPOINT=http://127.0.0.1:1`): after the pin line, one line
`STORE UNREACHABLE: the S3 store is unreachable at http://127.0.0.1:1 (start it with: docker compose
up -d seaweedfs): operation error S3: CreateBucket, exceeded maximum number of attempts, 3, …
connection refused`, exit 1, in 0.1 s.

**How to read every line** (evidence file `evidence/10.1-one-writer.txt`; `INFO:` lines vary and
are ignored by the evidence check):

- `sqlite_source_id: … (pin 3.53.4 ok)` — the pin check.
- Part A header, `model: …` and `invariants …` — what is modelled (2 nodes, the 8 action kinds per
  node, bounds) and the three invariants.
- `correct, depth 16, seed 1: 9595507962 interleavings explored, 44134 distinct states, 0
  interleavings violate an invariant` — (a) and (b) above for the real design.
- `seeded random walks: 2000 walks, 120000 steps, 0 reached a violation` — (c); 2000 × 60 steps,
  none ended early.
- `check ok: model correct: no interleaving up to depth 16 and no random walk lets a fenced epoch
  get a commit accepted`.
- `self-test store-accepts-every-put: 451413240 of 3536900716 interleavings violate an invariant`,
  then the shortest violating interleaving (4 steps): `B claim` (epoch 1), `B fence` (fence at 1),
  `A claim` (epoch 2), `A fence-with-stale-list` (A's listing misses B's fence, A puts at 1, the
  store overwrites) → "the accepted object of epoch 1 at position 1 was overwritten". The totals
  differ from the correct model's because a mutant changes which actions are enabled and
  exploration stops at a violation.
- `self-test resume-from-global-end: 170698606 of 11458358774 …` (7 steps): B owns epoch 1 and
  fences at 1; A claims epoch 2 and fences at 2; B crashes; B restarts and resumes at the store's
  end 2; B ships position 3 → accepted although epoch 2 is already accepted → "epoch 1 was accepted
  at positions 3-3 after epoch 2 had been accepted".
- `check ok: self-test: with … the checker finds the violation` (two lines).
- Part B header; `store: bucket course-ch10a, prefix exp-10-1/; catalog: db-0001, db-0002
  registered with synchronous=FULL foreign_keys=ON secure_delete=ON cache_size=-256
  wal_autocheckpoint=0 busy_timeout=5000ms; …` — the per-connection settings stored in the catalog.
- `node b's store wrapper hides the newest slot from its first listing (fault stale-list:/log/:1)`.
- 15 steps. For each: `step N: node X: <command>`; for commands, the node's reply line (`ok …` or
  `refused …`); `INFO: <node> bounds descriptors=<above baseline> memory=<bytes> ok`; then one
  `model <db>: <action>: <outcome>; …` line per database the step touches. The reply and the model
  must agree (same ok/refused; same fence position and number of fence attempts; same last stored
  position). The steps:
  1. a claims db-0001: epoch 1, empty store → recipe step 0 built locally, fence snapshot at 1.
  2. a writes store-durable: recipe step 1, acknowledged after the segment is stored at 2.
  3. a writes local-durable: step 2 committed at FULL and acknowledged; not in the store.
  4. b claims db-0001: catalog epoch 2. b's first listing misses slot 2 (the fault), so b restores
     position 1 and puts its fence at 2 — refused, slot 2 holds epoch 1 (lower, so "exists", not
     fenced); second attempt lists 1–2, restores position 2 (step 1; a's local step 2 is not there),
     fences at 3: `epoch 2, restored position 2, fence at position 3, attempts 2`. The model's
     `fence-with-stale-list: refused: position 2 holds epoch 1, retry` then `fence: ok: fence at 3`.
  5. a (stale, does not know) writes store-durable: commits step 3 locally, ships positions 3–4
     (its local steps 2 and 3) at slot 3 → refused: `fenced: db-0001 position 3 is held by epoch 2,
     this writer has epoch 1`. Not acknowledged.
  6. a writes local-durable: refused at once (a recorded the fence; it no longer owns db-0001).
  7. b writes store-durable: step 2 on b's copy, stored at 4.
  8. b claims db-0002 (epoch 1, fence 1); 9. b writes store-durable (stored at 2);
  10. a claims db-0002 (epoch 2, restored 2, fence 3, one attempt: no fault on a).
  11. b is killed (SIGKILL) — b still believes it owns db-0002 at epoch 1 (belief file) and owns
      db-0001 at epoch 2; the model crashes B in both databases.
  12. b restarts (new process, same directory, no fault). Model: db-0001 B resumes its own chain at
      4; db-0002 B lists, sees epoch 2, becomes fenced.
  13. b writes db-0002 store-durable: refused — opening the database lists the slots through b's
      epoch-1 adapter, which sees epoch 2 at position 3. The local file is never written.
  14. b writes db-0001 store-durable: the database was open when b was killed (`-wal` and `-shm`
      left); reopen recovers it, the first cycle finds nothing unshipped, step 3 is stored at 5.
  15. a writes db-0002 store-durable: step 2 on a's copy, stored at 4.
- `check ok: the real processes and the model agree at every command step (13 of 13)`.
- `node a: ok close; standing assertion: 8 checks, descriptors above baseline <= 68 (3 x 3
  connections x 4 open databases + 32 reserve), memory growth <= 41 MiB` (7 commands + close) and
  the same for b with `3 checks` (b's restarted process counts from its restart: 2 commands +
  close).
- `check ok: no node reported a problem …`; `check ok: after each of the 15 commands the node's
  descriptors and memory stayed within the standing bounds` (13 commands + 2 closes). INFO values
  in the run: descriptors 0, 7 or 14 above the baseline (7 per open database), memory growth
  1.3–7 MB.
- Verification, per database: `<db> store: …` (read back from S3 with `fence.Chain`) and `<db>
  model: …` (the model's store after the same steps) — must be identical:
  db-0001 `positions 1-1 epoch 1 snapshot; positions 2-2 epoch 1 segment; positions 3-3 epoch 2
  snapshot; positions 4-4 epoch 2 segment; positions 5-5 epoch 2 segment`; db-0002 the same up to 4.
  `check ok: … one lineage, epochs never decreasing`.
- `<db> catalog: owner b, epoch 2, last shipped 5, schema version 3, location b:db-0001.db; restore
  of position 5: step 3 (exact bytes)` — the catalog row, and the restored last position matched
  against the recipe's step hashes (db-0002: owner a, epoch 2, last shipped 4, schema version 2,
  step 2 exact bytes). Check: catalog epoch = newest slot epoch, last shipped = last position,
  schema version = matched step, state is a recipe state (exact, or equal except the change
  counter with bytes 24–27 = 92–95).
- `check ok: <db>: the owner's closed local file is byte-identical (E1) to the state rebuilt from
  the store` — `connmgr.CheckAtRest` sha256 of the owner's file vs sha256 of the restored image.
- One line per acknowledgement: `… store-durable acknowledgement at epoch E of user_version V: in
  the store true` + a check that the slot of that epoch at the acknowledged position restores to
  that user_version; `db-0001 local-durable acknowledgement at epoch 1 of user_version 2: in the
  store false` + `check ok: … was never shipped, and the takeover lost it, as that mode allows`.
- `all checks passed`, exit 0.

**Two consecutive runs**: identical apart from `INFO:` lines (`go run ./cmd/course check evidence
ch10` → `PASS … 2 runs identical to the evidence`).

**What the evidence does and does not show.** Part A is a model: it shows the protocol is safe
within its bounds (2 nodes, 3 epochs, 5 commits, depth 16; deeper bugs are only sampled by the
walks). Part B is one scripted interleaving on real processes and a real store, chosen to hit the
stale owner, the stale listing and the restarted stale owner; it shows the code behaves as the
model at those steps, not in every interleaving. Concurrency in the real run is sequential (the
parent waits for each reply); the store's atomicity under truly concurrent puts is the probe's
result (exactly one winner among 2 and 8 concurrent writers), not re-measured here.

## Spans in SPANS.txt (facts read from the pinned source)

- **wal.c 2199–2395 `walCheckpoint`** (copied). Backfills WAL frames into the database file. It
  first computes `mxSafeFrame`: starting from the last frame (2233), for each reader slot 1–4 whose
  read mark is lower it tries the slot's lock; if a reader holds it, `mxSafeFrame` drops to that
  reader's mark (2235–2251). It then takes read slot 0 exclusively (2259–2260), checks the WAL was
  not restarted meanwhile (salt compare, 2272), syncs the WAL (2277), and copies each frame's page
  (≤ `mxSafeFrame`, after `nBackfill`) to the database file (2304–2321). Only if it copied up to the
  last frame does it truncate and sync the database file (2328–2334); it then stores the new
  `nBackfill` (2337). RESTART/TRUNCATE additionally wait for readers and call `walRestartHdr`
  (2358–2384). What the shipper relies on: a held read mark caps every checkpoint at frames the
  shipper already shipped, and a reader on slot 0 blocks every backfill.
- **wal.c 2152–2166 `walRestartHdr`** (copied). Resets the in-memory WAL header for a restart:
  `nCkpt++` (a per-connection counter, chapter 9 correction 8), `mxFrame = 0`, salt-1 + 1, salt-2
  = a new random value, writes the wal-index header, sets `nBackfill` and `nBackfillAttempted` to 0,
  read mark 1 to 0 and marks 2–4 unused. After it, frames are written from the start of the file
  with new salts — the "new WAL generation" walship sees; frames of the old generation can no
  longer be read through the wal-index.
- **os_unix.c 1189–1342** ("Posix Advisory Locking" comment, `unixFileId`, `unixInodeInfo`;
  linked). The comment says POSIX locks are per process: a lock set through one descriptor
  overrides the same process's locks through another descriptor on the same file, and closing any
  descriptor releases all of the process's locks on that file; SQLite therefore keys its own lock
  bookkeeping by inode (`st_dev`, `st_ino`). `unixFileId` (1282–1296) is that key: device and
  inode number. `unixInodeInfo` (1323–1342) holds per-inode lock counts (`nShared`, `nLock`,
  `eFileLock`), deferred-close descriptors (`pUnused`) and the shared-memory node. Why this cannot
  be cross-machine ownership: the key is a local device and inode, the lock lives in the local
  kernel and dies with the process; another machine has its own kernel, inode and lock table, and
  network filesystems' lock behaviour is outside what this code can see (SQLite's
  `useovernet.html` warns against it). Ownership across machines therefore lives in the catalog's
  epoch and the store's slots.
- **os_unix.c 1802–1833 `unixFileLock`**. Every database-file lock change ends in
  `osSetPosixAdvisoryLock(pFile->h, pLock, …)`, an `fcntl` on the local descriptor (with a
  process-wide exclusive variant for the `unix-excl` VFS, 1807–1821). Nothing in it identifies a
  machine or survives the process.
- **os_unix.c 1866–2095 `unixLock`**. Implements SHARED → RESERVED → PENDING → EXCLUSIVE on the
  lock bytes at `PENDING_BYTE` using `unixFileLock`, with the in-process bookkeeping in
  `unixInodeInfo` consulted first (the comment block at the top of the span lists the transitions).
- **wal.c 2507–2571 `sqlite3WalClose`**. When a connection closes and can take an EXCLUSIVE lock on
  the database file (it is the last one), it runs a PASSIVE checkpoint and, unless PERSIST_WAL is
  set, marks the WAL for deletion (2528–2557); then closes the wal-index (deleting the `-shm` if
  deleting) and deletes the `-wal`. This is why an evicted database is a single file at rest — and
  why walship's resume saw "a WAL header change without a read mark" on every reopen until
  `AdoptAtRest`.
- **wal.c 3020–3272 `walTryBeginRead`**. Chooses a read slot for a new read transaction: slot 0 when
  the WAL is fully backfilled (`nBackfill == mxFrame`, readers use the database file only), else a
  slot whose mark is ≤ `mxFrame`, updating it if possible. The shipper's pin is such a read
  transaction; its slot is what limits `walCheckpoint`'s `mxSafeFrame` or, on slot 0, blocks
  backfill.
- **wal.c 3875–3915 `walRestartLog`**. A writer about to append restarts the WAL (via
  `walRestartHdr`) only when every frame was backfilled and it can lock read slots 1–4
  exclusively. With the shipper's mark ≤ the last shipped frame, a restart can only happen after
  everything shipped was copied — nothing unshipped is lost (chapter 9's argument, reused).

`go run ./cmd/course check spans ch10` → 10 spans PASS; `check sources ch10` → 1 copied file PASS.
Every outline span was exact (start line = symbol or comment start, end line = closing brace/`};`).

## Timeline data (for the chapter's Timeline section)

The relocation timeline itself is 10b's. What this chunk measured is the takeover (claim) timeline,
from 10.1 part B steps 3–5 (db-0001), state of both nodes, catalog and store after each event:

| # | Event | Node a | Node b | Catalog (owner, epoch) | Store slots |
| --- | --- | --- | --- | --- | --- |
| 0 | after step 2 | owner e1, local step 1, shipped 2 | — | a, 1 | 1 e1 snap (step 0), 2 e1 seg (step 1) |
| 1 | a commits step 2 (local-durable ack) | local step 2, shipped 2 | — | a, 1 | unchanged |
| 2 | b: catalog claim | unchanged (does not know) | claiming e2 | b, 2 | unchanged |
| 3 | b: listing (stale: sees slot 1 only) | | end = 1 | b, 2 | unchanged |
| 4 | b: fence put at 2 | | refused: slot 2 held by e1 → retry | b, 2 | unchanged |
| 5 | b: listing (fresh) → end 2; restore position 2 (step 1) | | image = step 1 | b, 2 | unchanged |
| 6 | b: fence put at 3 | | accepted; local file = step 1; belief e2 | b, 2 (last shipped 3) | + 3 e2 snap (step 1) |
| 7 | a commits step 3, ships 3–4 at slot 3 | refused (holder e2) → fenced, belief deleted, evicted; local file has steps 2–3 (orphaned) | | b, 2 | unchanged |
| 8 | b commits step 2, ships | | shipped 4 | b, 2 (last shipped 4) | + 4 e2 seg (step 2) |

Lost: a's local-durable step 2 (acknowledged) and a's step 3 (committed locally, not
acknowledged). Kept: everything store-durable.

## Platform implications (facts)

- **Three layers of guarantee, kept separate.** SQLite (per file, per machine): atomic commit,
  snapshot isolation, one writer per file through POSIX locks on a local inode (os_unix.c
  1189–1342). The service: epochs in the catalog, fence objects, the append-after-own-last rule,
  acknowledgement modes. The store: put-if-absent atomic among concurrent writers, read-after-write
  and list-after-write (measured on SeaweedFS 4.48 single server, probe), no list order.
- **Fencing costs**: a claim = one listing + one Get per slot + a restore (replay of every segment
  since the last snapshot) + one snapshot put of the whole database. A takeover of a 22-page
  database took well under a second here (not timed as evidence).
- **Every open database costs 3 connections and 7 descriptors** in this design (writer, shipper
  control, shipper pin). K = 1,000 open databases would need 7,000 descriptors plus reserve; size K
  ≤ (soft limit − reserve) / 7 (8.3's rule with 7 instead of 3).
- **Reopen cost**: listing + Get of every slot of the database (the shipper's resume loads the whole
  chain) + a full page read for `AdoptAtRest`. Without periodic snapshots or pruning (none in this
  chunk) the chain — and so the reopen and claim cost — grows with every shipped segment. 10b
  should add periodic fence-style snapshots and pruning of slots before the newest snapshot
  (pruning breaks `CheckLineage`'s "starts at position 1" rule, which 10b must relax to "starts with
  a snapshot").
- **Local-durable is a per-machine promise.** A takeover loses local-durable writes that were not
  shipped (10.1 step 3). Store-durable writes are never lost by a takeover (10.1 checks).
  A store-durable write that is refused is committed locally but not acknowledged; the local file
  of a fenced node must be discarded, not shipped (the next claim replaces it).
- **A refused put is information**: holder epoch higher → fenced (stop); lower or equal → the
  listing was stale (re-list and retry); identical bytes → the client's own retried put (success).
- **Signals worth exporting per database** (available from this chunk's code): epoch and owner
  (catalog), last shipped position and schema version (catalog), WAL change kind and checkpoint
  result of each cycle (`walship.CycleReport`), at-rest sha256 at eviction (node), bounds samples
  (node process). 
- **What this chunk does not handle** (for the chapter's list): relocation as a planned move with a
  final ship from the source (10b); periodic snapshots and pruning; multiple machines (all nodes
  share one catalog file on one host); catalog high availability; garbage from refused writes
  (none is stored — a refused put stores nothing — but a fenced node's local file remains until
  re-claimed); leases or timeouts (ownership changes only by explicit claim; a crashed owner is
  never taken over automatically).

## WAL-default evaluation for this chapter (ruling 10)

WAL is required, not just the default, for this design: walship reads committed frames from the
`-wal` and holds a read mark (chapter 9), which rollback mode does not have. The catalog database
is also a platform database and runs in WAL (`sqlitex.DefaultOptions`, `synchronous=FULL`): several
node processes read it while one claims, and WAL lets readers proceed during a claim. Costs found
here: 7 descriptors per open service database (correction 5); the close-time checkpoint deletes the
`-wal`, which made every reopen look like a discontinuity until `AdoptAtRest` (a WAL-specific cost,
measured in `TestAdoptAtRestSkipsTheReopenSnapshot`); `wal_autocheckpoint` must be 0 on the writer
so only the shipper checkpoints (set in `PlatformSettings`). No exception to WAL-as-default.

## Linux-only parts and the Mac route

Nothing in 10a is Linux-only: S3 over HTTP, SQLite through the Go driver, `Process.Kill` (SIGKILL on
both), descriptor counts via `/dev/fd` and memory via `ps` on macOS (procstat). So there is no
`LINUX-ONLY:` line. Mac route: `docker compose up -d seaweedfs` (Docker Desktop publishes port
18333), then the same `go run` command; without Docker, `-model-only` runs part A. Or run inside the
lab container: `docker compose run --rm lab go run ./ch10-multi-database-platform-capstone/experiments/10.1-one-writer`
(the container gets `COURSE_S3_ENDPOINT=http://seaweedfs:8333` from compose.yaml). darwin:
cross-built and cross-vetted only, never run; the container route was not run in this chunk.

## Surprises worth a sentence in the chapter

- The first real run found a bug the unit tests had not: the claim computed its fence position from
  one listing while walship's catalog load re-listed, so with a stale first listing the fence was
  refused as out of order. Safe (nothing was written) but the takeover failed. Fixed by listing
  once per claim attempt.
- walship's resume, built for one shipper per database, is exactly the stale-owner bug when two
  nodes share a store (correction 2) — the model's second mutant is that behaviour.
- connmgr's at-rest check is harmless but blind inside the node (the shipper's `-shm` makes every
  eviction "not at rest" before the file is opened); the node's own check after the shipper closes
  is the one that proves local = stored.

## Repair round 1 (facts that changed; details are folded into the sections above)

- 10.1 output changed in three EXACT lines (evidence re-captured, two runs identical):
  line 4 now reads `… at most 3 epochs, and a node's local commits stop at stream position 5 (the
  bound counts positions across all epochs and fences, not commits per node)` (was `at most 3
  epochs and 5 local commits per node`, which was wrong: see the `model` section); the two
  `check ok: db-000N: the store holds exactly the slots the model predicts …` lines end with
  `, no damaged object` (the check now also requires `fence.Audit`'s walship catalog to have no
  damaged object). All other EXACT lines are unchanged.
- 10.1 now exits 1 with `FATAL -mutant: model: unknown mutant "<name>" …` on an unknown `-mutant`,
  exits 1 (`FATAL parse acknowledgement`) if an `ok write` reply cannot be parsed, and prints each
  node problem (`problem: …`) and over-bounds line (`over bounds: …`) before the corresponding check
  (none are printed in a passing run).
- A node process's `close` with extra arguments is now just an unknown command; `Serve` keeps running
  (before, it returned without closing the node).
- New node files in the node directory: `<id>.atrest` (see `node`). A node directory after a clean
  close holds `<id>.db`, `<id>.owner` and `<id>.atrest` per owned, closed database.
- Adversarial run after the repair: 26 HELD, 1 BROKEN (MOD-05b — the attack's expectation was the
  old, wrong description of the bound; the code is unchanged and the description is corrected).
