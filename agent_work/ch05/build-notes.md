# Chunk 5 build notes (plain data for the prose pass)

Builder: Claude Opus 5.5, branch `chunk/ch05`, 2026-11-20. Machine: linux/amd64, Go 1.26.8, ext4.
Pinned tree at `$SQLITE_SRC` (3.53.4). `showwal` used for the evidence: the `cmd/course
build-tools` build of the pin (placed in `_tools/bin`). All four experiments were also run inside
the Debian `lab` container (`docker compose run --rm -T lab go run ./ch05-wal-transactions-checkpoints/experiments/<name>`,
whose `showwal` is in `/opt/course-tools`): EXACT output identical to the host's for all four.
darwin: cross-built and cross-vetted only (`agent_work/verify/crossbuild.sh` → `CROSSBUILD OK`), never run.

All source line numbers are at the pin. `go run ./cmd/course check spans ch05` passes (28 spans);
`check sources ch05` passes (`wal.c`, `wal.h`). Every span the outline lists was exact (start line
holds the symbol, end line is the closing brace / end of the comment); nothing needed fixing.

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

## Shared packages

No existing shared package was changed (`sqlitex`, `trace`, `recipe`, `dbfile`, `equal`,
`capture`, `repocheck` untouched; `go.mod` untouched). One new package: `internal/walfile`
(1,195 lines of code + 898 lines of tests, no comments). It imports only the standard library
(`live_test.go` also imports `internal/sqlitex`).

### internal/walfile — WAL file (`wal.go`, `checksum.go`, `errors.go`)

- Constants: `HeaderSize` 32, `FrameHeaderSize` 24, `MagicLittleEndian` 0x377f0682,
  `MagicBigEndian` 0x377f0683, `FormatVersion` 3007000, `MinPageSize` 512, `MaxPageSize` 65536.
- `Checksum [2]uint32` (`String()` = two 8-digit hex words). `ChecksumBytes(bigEndianWords, data,
  seed)`: SQLite's running checksum (wal.c:856-912): for each pair of 32-bit words x0,x1:
  `s1 += x0 + s2; s2 += x1 + s1`. The words are read big-endian when the magic's low bit is 1,
  little-endian otherwise (the writer stores `WAL_MAGIC | SQLITE_BIGENDIAN`, wal.c:4092, i.e.
  its native order; all supported hosts are little-endian → 0x377f0682). Input must be a positive
  multiple of 8 bytes, else an error. Checksum *values* are always stored big-endian.
  Test vector from a real WAL: header bytes with salts c1ab489b b9fd0cf3 → 702faed4 1b248432.
- `Header{Magic, Version, PageSize, CheckpointSequence, Salt1, Salt2, Checksum}`,
  `BigEndianChecksums()`, `ChecksumByteOrder()`, `String()`.
- `Parse(data) (*File, error)` / `ParseFile(path)`. Mirrors `walIndexRecover` (wal.c:1390-1614):
  - 0 bytes → a `File` with no header and no frames (a legitimate state: after TRUNCATE).
  - 1-31 bytes, a magic other than 0x377f0682/3, a page size not a power of two in 512..65536,
    or a header checksum mismatch → `*HeaderError` wrapping `ErrWALIgnored`: SQLite ignores such
    a WAL as a whole (wal.c:1447-1467 `goto finished` with no frames).
  - version ≠ 3007000 → `ErrUnsupportedVersion` (SQLite returns SQLITE_CANTOPEN, wal.c:1472).
  - Frames: every whole frame slot `(size-32)/(page+24)` is listed; trailing bytes that do not
    make a whole frame are `PartialFrameBytes` (SQLite never reads them, wal.c:1489).
  - Validation in SQLite's order (`walDecodeFrame` wal.c:1000-1053): salts equal the header's
    → page number ≠ 0 → running checksum over frame-header bytes 0-7 and the page, seeded by the
    header checksum. The first failure ends the valid log; every later frame is
    `FrameAfterInvalid`. `ValidFrames` = number of valid frames; `LastCommitFrame` = last valid
    frame with a non-zero commit size.
- `Frame{Number, Offset, Page, CommitSize, Salt1, Salt2, StoredChecksum, PageSHA256,
  SaltsMatchHeader, Problem}`, `Valid()`, `IsCommit()`. `FrameProblem` values and their text:
  valid; salt mismatch (frame from an earlier WAL generation, or rewritten in place by an open
  transaction); page number 0; checksum mismatch (running checksum chain broken); follows an
  invalid frame.
- `File{Size, Header, ComputedChecksum, Frames, ValidFrames, LastCommitFrame,
  PartialFrameBytes}` with `PageSize()`, `FrameSize()`, `StopReason()`, `CommittedFrames()`,
  `UncommittedValidFrames()`, `Commits() []Commit{FirstFrame, LastFrame, Pages}` (+`Frames()`),
  `DatabasePages()` (commit size of the last commit), `PageImage(n)` (the page bytes of frame n),
  `Tuples(validOnly) []Tuple{Page, PageSHA256, CommitSize}` — the run-independent form of a WAL
  (outline §4.2). `EqualTuples(a, b) (bool, firstDifferingFrame)`.

### internal/walfile — byte comparison (`diff.go`)

- `Region` (header magic / version / page size / checkpoint sequence / salts / checksum; frame
  page number / commit size / salts / checksums; page data; bytes beyond the end of the shorter
  file). `RegionAt(offset, pageSize)`.
- `CompareBytes(a, b, pageSize) ByteComparison{SizeA, SizeB, DifferingBytes, ByRegion}`,
  `Identical()`, `OnlyIn(regions...)` (true only if sizes are equal and every differing byte is
  in the listed regions), `String()`.

### internal/walfile — replayer (`replay.go`)

- `Replay(base, wal []byte) ([]byte, ReplayReport, error)`:
  1. Base checks: empty is allowed; otherwise it must start with `SQLite format 3\0` and be ≥100
     bytes (`ErrBaseNotDatabase`).
  2. Parse the WAL. `ErrWALIgnored` → output = base unchanged, `WALIgnoredReason` set (what
     SQLite does). `ErrUnsupportedVersion` → error.
  3. No commit frame → output = base.
  4. Base page size (header bytes 16-17, 1 = 65536) must equal the WAL's (`ErrPageSizeMismatch`;
     SQLite: wal.c:4392 CORRUPT); base length must be a multiple of it (`ErrBaseSize`).
  5. A commit size that needs more than base + 65536 + frames × page bytes is refused
     (`ErrCommitSizeTooLarge`) — SQLite's own corruption test in `walCheckpoint` (wal.c:2288-2297);
     it also stops a hostile commit size from allocating gigabytes.
  6. Apply frames 1..LastCommitFrame in order (a later frame of a page wins); skip frames whose
     page is beyond the final commit size (`SkippedBeyondSize`; SQLite skips `iDbpage>mxPage`,
     wal.c:2312); then cut or zero-extend to commit size × page size (SQLite truncates the db
     after a complete checkpoint, wal.c:2328-2331).
- `ReplayReport{WALIgnoredReason, PageSize, FramesInFile, PartialFrameBytes, ValidFrames,
  AppliedFrames, Transactions, LastCommitFrame, DatabasePages, DistinctPages, SkippedBeyondSize,
  BaseBytes, OutputBytes, Ignored []IgnoredFrames{FirstFrame, LastFrame, Reason}}`,
  `IgnoredFrameCount()`, `Lines()` (the text the experiments print). Ignored groups: valid but
  uncommitted frames after the last commit ("valid but uncommitted: no commit frame follows");
  the first invalid frame with its problem; the rest ("follows an invalid frame (N of them
  carry other salts)"); trailing partial-frame bytes.
- `ReplayFiles(basePath, walPath, outPath)`: refuses if `outPath`, `-wal`, `-journal` or `-shm`
  next to it exists (`ErrOutputExists`: SQLite would apply a stale side file to the rebuild); a
  missing WAL file means "no frames"; writes `outPath.replay-tmp` with O_EXCL, fsyncs, renames;
  removes the temp file on any error.

### internal/walfile — wal-index / `-shm` decoder (`shm.go`)

Layout (wal.c:318-401, 474, 615-624; os_unix.c:4602-4603):

| bytes | field |
| --- | --- |
| 0-47 | WalIndexHdr copy 1: iVersion (3007000), unused, iChange (commit counter), isInit, bigEndCksum, szPage (u16; 65536 stored as 1), mxFrame, nPage, aFrameCksum[2], aSalt[2], aCksum[2] |
| 48-95 | WalIndexHdr copy 2 (written first, then a barrier, then copy 1: `walIndexWriteHdr` wal.c:942-954) |
| 96-135 | WalCkptInfo: nBackfill, aReadMark[0..4], aLock[8] (bytes 120-127 are the lock bytes), nBackfillAttempted, notUsed0 |
| 128 | the dead-man-switch lock byte (`UNIX_SHM_DMS`), inside nBackfillAttempted's word: locks are byte ranges, never written |
| 136.. | block 0's page-number array: 4062 u32s (frame 1 at 136), then from byte 16384 a 8192-slot u16 hash table; every later 32 KiB block: 4096 page numbers then the hash table |

- The wal-index is in the **host's native byte order** (wal.c:144-146), except the salts, which
  are copied as raw big-endian bytes from the WAL header. `ParseIndex` detects the order from the
  version word (3007000 little- or big-endian). Header checksum = the same algorithm over bytes
  0-39 in native order (`walIndexWriteHdr`).
- `ParseIndex(data)` / `ParseIndexFile(path)` → `*Index{Size, Initialized, BigEndian, Headers [2],
  Checkpoint}`; `<136` bytes → `ErrShortIndex`; an all-zero first header → `Initialized=false`
  (no error); any other version word → `ErrIndexVersion`. `Header()` (copy 1), `HeadersAgree()`,
  `ByteOrderName()`.
- `IndexHeader{Version, Change, IsInit, BigEndianChecksum, PageSizeField, MaxFrame, Pages,
  FrameChecksum, Salt1, Salt2, Checksum, ChecksumOK}`, `PageSize()` (same formula as
  `walPagesize`: `(f&0xfe00) + (f&1)<<16`).
- `CheckpointInfo{Backfill, ReadMarks [5], BackfillAttempted}`, `ReadMarksText()`, `String()`;
  `ReadMarkText(m)` prints 0xffffffff as `unused`.
- `FramePage(frame)` / `FramePages(maxFrame)`: the page number array entry for a frame (block =
  0 for frames 1..4062, else 1 + (f-4063)/4096; `ErrFrameNotIndexed` outside the file).
- Cross-checked against `showshm` from the pinned tree (`agent_work/ch05/shm-crosscheck`): equal
  on every numeric field of two real `-shm` files.

### internal/walfile — `showwal` cross-check (`showwal.go`)

- `ParseShowwal(text)` reads `showwal FILE` output: `Pagesize:`, `Available pages: 1..N` (N =
  whole frame slots, tool/showwal.c:567), and one-line frames `Frame %4d: %6d %6d
  0x%08x,%08x 0x%08x,%08x[ should be 0x…,…]` (tool/showwal.c:236). showwal continues its own
  checksum chain and appends ` should be …` when the stored checksum differs; it does not check
  salts.
- `(*File).CompareWithShowwal(listing) []string`: page size, frame count, and per frame page,
  commit size, salts and stored checksum must agree; a frame walfile calls valid must have no
  ` should be`.

### internal/walfile — copying live files from a child process (`livecopy.go`)

Why: a process that holds a SQLite connection must never open+close the database **or its
`-shm`**: closing any descriptor of a file drops all of that process's POSIX locks on it, and the
unix VFS keeps its WAL locks as `fcntl` locks on the `-shm` descriptor (os_unix.c:4703-4750,
`unixShmSystemLock`; lock bytes 120-128). The `-wal` itself carries no locks, but every copy goes
through the child for one simple rule.

- `ServeLiveCopyChild(os.Args) bool`: call first in `main` (and in `TestMain`). When the
  arguments are `walfile-live-copy-child <from> <to> …` it copies each pair, prints `copied N` or
  `missing` per pair (a missing source also removes an old copy at `<to>`), and exits.
- `CopyLiveFiles(ctx, []CopyPair{From, To}) ([]CopyResult{From, To, Present, Bytes}, error)`:
  re-runs `os.Executable()` in that mode (works under `go run` and `go test`).
- `DatabaseFileSet(db, dir, name)`: the three pairs db, `-wal`, `-shm`.
- `ReadLiveState(ctx, db, scratchDir) (LiveState{WALPresent, WALSize, WAL, WALProblem,
  IndexPresent, Index}, error)`: copies `-wal` and `-shm` through the child, parses both,
  removes the copies.

### Tests (`go test ./internal/walfile`, about 1 s)

Synthetic WALs built in the test in both byte orders and page sizes 512/4096/65536 (parse,
commits, size formula, page images, tuple hashes); relabelled byte order fails the header
checksum; empty and header-only files; every ignored-header case and the version error; stop at
the first invalid frame for checksum / salt / page-0 / partial frame; an older-generation tail
behind a new generation; replay in both orders, uncommitted tail, a commit that shrinks the
database (frames beyond the size skipped), no commit, ignored WAL, empty base; replay errors
(page size, not a database, base size, commit size too large, version); `ReplayFiles` (existing
output, stale side file, missing base, missing WAL, temp-file cleanup); byte comparison regions;
`EqualTuples`; wal-index both orders, 64 KiB page-size field, frames across the block boundary
(4062/4063), broken header checksum, zeroed and short and wrong-version files; `showwal` text
parsing and comparison; 3,000 mutated + 200 random inputs through every parser and the replayer
(no panic) and a `FuzzParseAndReplay` target. Live (`live_test.go`, real SQLite through the
modernc driver): child copies; a WAL with several commits parsed, its `-shm` agrees (mxFrame,
nPage, salts, page array), `NOOP` log = frames, `showwal` agrees (skipped when not found), and
the replay of the copies equals the file after SQLite's own `wal_checkpoint(TRUNCATE)`;
`ReadLiveState` before, during and after TRUNCATE.

## What each span does (facts read from the copied source)

- **wal.c 16-249 (`WAL`) — the format comment.** A WAL is a 32-byte header of eight big-endian
  u32 (magic, version 3007000, page size, checkpoint sequence, salt-1, salt-2, checksum-1,
  checksum-2) followed by frames of a 24-byte header (page number, commit size or 0, salt-1,
  salt-2, checksum-1, checksum-2) and one page. A frame is valid only if its salts match the
  header and its checksum matches the running checksum over the header and every frame up to it;
  a transaction commits when a frame with the commit marker is written. After each checkpoint
  salt-1 is incremented and salt-2 randomized so old and new frames can never both look valid.
- **wal.c 100-126 (`READER ALGORITHM`).** A reader looking for page P takes the last valid copy
  of P at or before a commit frame, else the database file. A read transaction records `mxFrame`
  when it starts and ignores everything appended later — that recorded number *is* its snapshot.
  The wal-index exists so that lookup does not scan the whole WAL.
- **wal.c 127-249 (`WAL-INDEX`).** The wal-index is shared memory, implemented by the unix VFS as
  the mmapped `-shm` file; it is transient, rebuilt from the WAL after a crash, and in native byte
  order. It is a header region and 32 KiB blocks, each with a page-number array (4062 entries in
  the first block, 4096 later) and a hash table of 8192 u16 slots keyed `(P*383) % 8192`. Readers
  with different `mxFrame` share it by ignoring entries above their own bound.
- **wal.c 294-401 (`WAL_WRITE_LOCK`, `WalIndexHdr` 321-333, `WalCkptInfo` 394-401).** Lock slots:
  0 = WRITE, 1 = CKPT, 2 = RECOVER, 3+i = READ(i), five read slots. `WalIndexHdr` holds iChange,
  mxFrame (last valid commit frame), nPage, the last frame's checksum, salts and a header
  checksum. `WalCkptInfo` holds nBackfill (frames already copied to the database),
  `aReadMark[5]`, the lock bytes and nBackfillAttempted. The comment states the rules: a reader
  holding READ(K) has a snapshot ≤ aReadMark[K]; a checkpoint may copy only frames ≤ every
  in-use mark; READ(0) readers ignore the WAL; a writer resets the WAL when nBackfill == mxFrame
  and no reader uses locks 1..4.
- **wal.c 477-491 (`WAL_FRAME_HDRSIZE`).** 24, 32, and `WAL_MAGIC 0x377f0682`.
- **wal.c 856-912 (`walChecksumBytes`).** The Fletcher-like sum over pairs of u32 words, seeded
  from the previous value; non-native order byte-swaps each word; a 64-byte unrolled path for
  native order. Input length must be a multiple of 8.
- **wal.c 942-954 (`walIndexWriteHdr`).** Sets isInit and the version, checksums the header, then
  writes copy 2, a memory barrier, copy 1 — readers compare both copies to detect a torn header.
- **wal.c 969-993 (`walEncodeFrame`).** Writes page number and commit size, copies the salts,
  extends the running checksum over the first 8 header bytes and the page, stores it big-endian.
  While a transaction is re-checksumming (`iReCksum`), salts and checksums are written as zeros
  and fixed at commit.
- **wal.c 1000-1053 (`walDecodeFrame`).** Rejects a frame whose salts differ, whose page number is
  0, or whose checksum does not continue the chain; otherwise returns page and commit size.
- **wal.c 1390-1614 (`walIndexRecover`).** Under exclusive locks, reads the WAL header (bad
  magic/page size/header checksum → no frames; wrong version → CANTOPEN), then reads frames in
  order until the first invalid one, appending each to the index; mxFrame/nPage follow the last
  commit frame. Then writes the header, sets nBackfill 0, aReadMark[1] = mxFrame (others unused)
  and logs "recovered N frames".
- **wal.c 2107-2125 (`walBusyLock`).** Takes an exclusive lock and retries through the busy
  handler while it gets SQLITE_BUSY — the only place a checkpoint waits.
- **wal.c 2152-2166 (`walRestartHdr`).** Restart: nCkpt+1, mxFrame 0, salt-1 + 1, salt-2 new
  random, header written, nBackfill 0, aReadMark[1] 0, marks 2-4 unused.
- **wal.c 2199-2395 (`walCheckpoint`).** If nBackfill < mxFrame: lowers `mxSafeFrame` to every
  read mark it cannot lock (each mark it can lock is moved up, 2233-2250); takes READ(0)
  exclusively (2260) — impossible while any READ(0) reader exists; checks the salts did not
  change; syncs the WAL; copies pages from frames nBackfill+1..mxSafeFrame (newest per page) into
  the database; if everything was copied, truncates the database to nPage pages and syncs it;
  stores nBackfill. For FULL and above: busy if not everything was copied (2361-2362); for
  RESTART/TRUNCATE: waits for read locks 1..4 (2367); TRUNCATE alone restarts the header and
  truncates the WAL to 0 (2383-2384).
- **wal.c 2401-2413 (`walLimitSize`).** Truncates the WAL to `journal_size_limit` bytes if it is
  larger; errors ignored.
- **wal.c 2507-2571 (`sqlite3WalClose`).** On the last close: takes the exclusive database lock,
  runs a checkpoint (2534), and if that succeeded deletes the `-wal` (2563) unless PERSIST_WAL
  (then truncates it to 0, 2554).
- **wal.c 3020-3272 (`walTryBeginRead`).** If the WAL is fully backfilled (nBackfill == mxFrame)
  the reader takes READ(0) shared and ignores the WAL (3134-3163). Otherwise it picks the largest
  mark ≤ mxFrame (3184), or sets a free slot to mxFrame (3196), takes READ(i) shared, then
  re-checks that the mark and header did not change (else retry). Its `minFrame` is nBackfill+1.
- **wal.c 3699-3746 (`sqlite3WalBeginWriteTransaction`).** Takes WRITE; if the wal-index header
  changed since this connection's read transaction began, releases it and returns
  SQLITE_BUSY_SNAPSHOT.
- **wal.c 3875-3915 (`walRestartLog`).** Before a writer appends: if it is a READ(0) reader and
  nBackfill > 0 (so the WAL was fully copied), takes exclusive READ(1..4); if no reader holds
  them, calls `walRestartHdr` so the frames go to the start of the file.
- **wal.c 4038-4266 (`walFrames`).** Calls walRestartLog; if mxFrame is 0 writes and (unless
  synchronous=OFF) syncs a new WAL header, with fresh random salts when nCkpt is 0; writes each
  dirty page as a frame (a page already written by this transaction is overwritten in place and
  checksums are recomputed at commit, 4143-4177); the last frame of a commit carries the database
  size; with synchronous FULL syncs the WAL at commit (padding to a sector only without
  POWERSAFE_OVERWRITE); appends to the index and, on commit, publishes the new header — from
  that moment new readers see the transaction.
- **wal.c 4301-4432 (`sqlite3WalCheckpoint`).** NOOP takes no locks and only reports. Others take
  CKPT; FULL/RESTART/TRUNCATE also take WRITE through the busy handler, falling back to PASSIVE
  (and reporting busy) if a writer holds it (4358-4363). Returns log = mxFrame and checkpointed =
  nBackfill.
- **wal.h 25-26 (`WAL_SYNC_FLAGS`, `CKPT_SYNC_FLAGS`).** Commit sync = low two bits; checkpoint
  sync = bits 2-3.
- **pager.c 607-617 (`walSyncFlags`).** The comment describing those bits the other way round
  (see correction 8).
- **pager.c 3225-3282 (`pagerWalFrames`).** Hands the pager's dirty list to `sqlite3WalFrames`
  with the database size when committing; writes the change counter only if page 1 is in the list
  (ch01/ch02 correction).
- **pager.c 3385-3413 (`pagerOpenWalIfPresent`).** A `-wal` next to the database switches the
  pager into WAL mode on open; an empty `-wal` with an empty database is deleted.
- **pager.c 3658-3704 (`sqlite3PagerSetFlags`).** `walSyncFlags = syncFlags<<2`, plus `syncFlags`
  in the low bits only for FULL/EXTRA: NORMAL syncs at checkpoints but not at commit.
- **main.c 2484-2496 (`sqlite3WalDefaultHook`).** Autocheckpoint: after a commit, if the WAL holds
  ≥ N frames (default 1000, main.c:3690), runs `sqlite3_wal_checkpoint` = PASSIVE, inside the
  committing connection.

## Experiment 5.1 wal-inspector

Run: `go run ./ch05-wal-transactions-checkpoints/experiments/5.1-wal-inspector` (repo root).
Runtime 1-4 s. Evidence `evidence/5.1-wal-inspector.txt` (150 lines). EXACT.

What it does: builds the recipe to step 0 (closed file), opens one connection (recipe settings,
`wal_autocheckpoint=0`, synchronous FULL), applies steps 1-5, reads `PRAGMA page_count` after
each, has a child process copy the database, `-wal` and `-shm` while the connection is open,
runs `wal_checkpoint(NOOP)`, closes. Repeats the build (run 2), and twice more with synchronous
NORMAL and OFF.

How to read the output:
- `WAL header: magic=0x377f0682 (little-endian checksums) version=3007000 page-size=4096
  checkpoint-seq=0` — the stable header fields. The salts and header checksum are on an `INFO:`
  line because they are random per run.
- `WAL size 387312 bytes = 32 + 94 frames x (24 + 4096)` and its check.
- The frame list: number, page, commit size, first 16 hex digits of the page image's sha256;
  `<- commit` marks the five frames with a non-zero size field. Each commit starts with page 1
  (every recipe step writes `user_version`).
- `checksum chain: 94 of 94 frames verify, stop reason: valid`.
- `transactions`: commit k = recipe step k: frames 1-22 (22), 23-40 (18), 41-58 (18), 59-83 (25),
  84-94 (11); commit sizes 22, 29, 39, 51, 51 pages, each equal to `PRAGMA page_count` after the
  step.
- `PRAGMA wal_checkpoint(NOOP) … busy=0 log=94 checkpointed=0`.
- `wal-index (-shm copy): 32768 bytes, little-endian, header copies agree: true, header checksum
  ok: true`; `mxFrame=94 nPage=51 page-size=4096 nBackfill=0 salts equal the WAL header: true`;
  page-number array equals the frames' pages.
- `showwal … lists 94 frames; disagreements with walfile: 0` (or a `NO-SHOWWAL:` line naming
  `go run ./cmd/course build-tools` when the tool is missing).
- Part B: the region table for run 1 vs run 2 — only `header salts`, `header checksum`, `frame
  salts`, `frame checksums` say `differs`; page data, page numbers, commit sizes, sizes `same`.
  INFO: byte counts (one run: header salts 8, header checksum 8, frame salts 752 = 94 × 8, frame
  checksums 751 — the 752nd checksum byte happened to be equal). Tuple sequences equal.
- Part C: NORMAL and OFF write 94 frames, 5 commits, sequences equal to FULL.

Claims confirmed: everything in the outline's 5.1 (file size formula, 5 commit frames, frame
count = NOOP log, listing = showwal, byte difference only in salt/checksum fields, equal frame
sequences); plus Neon's "different synchronous levels" item.

## Experiment 5.2 wal-replay (rung R3)

Run: `go run ./ch05-wal-transactions-checkpoints/experiments/5.2-wal-replay`. Runtime 2-6 s.
Evidence `evidence/5.2-wal-replay.txt` (63 lines). EXACT.

- **A, quiet moment.** Step 0 built; one connection (autocheckpoint 0) commits steps 1-8; child
  copies db (16,384 bytes) + `-wal` (638,632 bytes). The captured db alone is recipe step 0
  **byte for byte** (`step 0 (exact bytes)`): every change of 8 commits lives only in the WAL.
  Replay: 155 frames, 8 transactions, 79 distinct pages, 16,384 → 323,584 bytes, `ignored: none`.
  SQLite then runs `wal_checkpoint(TRUNCATE)` on the original and closes. `E1 bytes: equal
  (sha256 00eac4ca…)`. The replay opened by SQLite: `user_version 8`, `integrity_check ok`, file
  unchanged by the open. Recipe lookup: step 8 with the change-counter mask (one long-lived
  connection; ch01 correction 1).
- **B, mid-transaction.** Recipe built to step 8 (canonical); writer with `cache_size=10` commits
  steps 9-10 (`-wal` 41 frames), then opens a transaction with step 11's statements plus
  `UPDATE events SET note = upper(note)` and does not commit; the child copies db + `-wal`. The
  copy has 54 frames, last commit frame 41, 13 spilled frames. Replay report:
  `ignored frame 42: checksum mismatch (running checksum chain broken)` and `ignored frames 43-54
  (12): follows an invalid frame (0 of them carry other salts)`. Frame 42 fails because a page
  spilled at frame 42 was spilled again later in the same transaction and overwritten in place
  (wal.c:4143-4161), leaving its stored checksum stale until commit (`walRewriteChecksums`,
  4177). Then SQLite opens another copy of the same two files **with no `-shm`** (recovery
  rebuilds it): `user_version 10`, TRUNCATE → equal sha256 `fb68a521…`. And again with a **stale
  `-shm` from the part-A database** (mxFrame 155, other salts): same `user_version 10`, same
  sha256 — the first connection to open a WAL database truncates the `-shm` to 3 bytes and runs
  recovery (os_unix.c:4850-4900 `unixLockSharedMemory`), so a `-shm` is never trusted.
- **C, after rollback.** `ROLLBACK`: `-wal` still 222,512 bytes (54 frames), `-shm` mxFrame back
  to 41. A new child copy replays to the same report and the same bytes; SQLite's TRUNCATE
  checkpoint of the original after the rollback: equal; and the B replay equals the C replay.

Claims confirmed: E1 equality in all three outline cases (plus the stale-`-shm` case); the
replayer reports ignored frames and why. Not done here (Neon's list): flipped byte in the last
commit, earlier-generation base (correction 6), applying frames past the last commit.

## Experiment 5.3 reader-pins-the-wal

Run: `go run ./ch05-wal-transactions-checkpoints/experiments/5.3-reader-pins-the-wal`. Runtime
3-4 s. Evidence `evidence/5.3-reader-pins-the-wal.txt` (83 lines). EXACT (correction 4).

Setup: recipe to step 5 (closed). Writer: recipe settings with `wal_autocheckpoint=200`. Reader:
second connection, `_txlock` deferred, `BEGIN` + `SELECT count(*) FROM events`, held open. After
every commit a child copies `-wal` and `-shm`; columns: WAL bytes, mxFrame, nBackfill, read marks
[0..4], the reader's count inside its transaction, the writer's count.

- **A.** After step 6: mxFrame 22, marks `0,0,unused…`. Reader begins: marks `0,22,…` (it set mark
  1 to mxFrame). Steps 7-30: WAL 90,672 → 2,286,632 bytes, mxFrame 22 → 555, reader rows 251
  throughout, writer rows 251 → 1177; mark 1 stays 22; mark 2 follows the writer's own read
  snapshots (38, 61, …). nBackfill 0 until step 15 (first commit with mxFrame ≥ 200 → autocheckpoint),
  then 22 = the reader's mark for every later step. 533 frames could not be checkpointed.
  Reader ends: nothing changes in the `-shm` (`0,22,555,…` — marks outlive readers). PASSIVE:
  `busy=0 log=555 checkpointed=555`, nBackfill 555, WAL still 2,286,632 bytes. Step 31: mxFrame
  18, WAL file size unchanged, header checkpoint-seq 0 → 1, salt-1 +1, 18 valid frames of 555 in
  the file (the rest: old generation).
- **B (new).** After step 6, PASSIVE → nBackfill 22 = mxFrame. Reader begins: marks unchanged
  (READ(0)). Step 7: WAL restarts (salt-1 +1, mxFrame 16, size unchanged 90,672). Steps 7-30:
  nBackfill 0 on every row, mxFrame up to 533, WAL 2,195,992 bytes; reader rows 251. After the
  reader ends, PASSIVE copies all 533.

Claims confirmed: reader pins via a mark; nBackfill stops at the mark; no restart while pinned;
WAL grows; reader's count constant; after it ends one checkpoint completes and the next write
restarts at frame 1. Corrected/extended: corrections 3, 4, 5.

## Experiment 5.4 checkpoint-modes

Run: `go run ./ch05-wal-transactions-checkpoints/experiments/5.4-checkpoint-modes`. Runtime 4-5 s
(six checkpoints wait the 300 ms busy timeout). Evidence `evidence/5.4-checkpoint-modes.txt` (69
lines). EXACT except the `waited` column, which is a BOUND (elapsed ≥ 300 ms); elapsed times are
on `INFO:` lines.

Per case: copy of a closed step-5 template; writer (autocheckpoint 0) commits step 6 (22 frames)
and step 7 (38 frames); "reader behind" began between them (snapshot frame 22), "reader current"
after both (snapshot 38); a third connection with `busy_timeout=300ms` runs the checkpoint; then
the reader ends and the writer commits step 8.

First table (right after the checkpoint):

| mode | no reader | reader behind | reader current |
| --- | --- | --- | --- |
| PASSIVE | 0/38/38, no wait | 0/38/22, no wait | 0/38/38 |
| FULL | 0/38/38 | **1**/38/22, waited | 0/38/38 |
| RESTART | 0/38/38 | 1/38/22, waited | **1/38/38, waited** |
| TRUNCATE | **0/0/0, WAL 0 bytes, shm salt-1 +1** | 1/38/22, waited | 1/38/38, waited |

WAL bytes after every non-TRUNCATE case: 156,592 (unchanged). Second table (after the next
write): every case whose checkpoint copied all 38 frames (or TRUNCATE emptied the WAL)
`restarted yes` with mxFrame 23 (step 8's frames) at the start of the old file (23 valid / 38
frames, 156,592 bytes); "reader behind" cases appended (61 frames, 251,352 bytes, salts same).
TRUNCATE/no reader: 94,792 bytes = 32 + 23 frames, salt-1 `new value`, checkpoint-seq `same`
(correction 1). Last section: writer runs TRUNCATE itself, then writes: salt-1 +1, checkpoint-seq
0 → 1.

How to read `busy/log/checkpointed`: log = mxFrame (frames in the WAL), checkpointed = nBackfill
after the call; busy=1 means a FULL/RESTART/TRUNCATE could not finish (a reader, or a writer
holding WRITE). PASSIVE never sets busy for readers and never waits. A reader at the end of the
WAL does not limit copying but still blocks RESTART/TRUNCATE.

Darwin note: the `waited` BOUND assumes PASSIVE returns in < 300 ms; on a heavily loaded machine
that could fail (never seen here: 9-19 ms).

## Timeline data (chapter section 4)

- Frames per commit (5.1, recipe steps 1-5): 22, 18, 18, 25, 11; each commit's first frame is
  page 1 and its last frame carries the page count (22, 29, 39, 51, 51). Frame k sits at byte
  32 + (k-1) × 4120; its page at +24.
- Read marks and backfill under a held reader (5.3 A rows): `after step 6: 90672 22 0
  0,0,unused` → `reader began: 22 0 0,22` → … `step 14: 762232 185 0` → `step 15: 861112 209 22`
  (first autocheckpoint) → … `step 30: 2286632 555 22` → `reader ended: 555 22` → `PASSIVE: 555
  555 0,555,555` → `step 31: 2286632 18 0 0,0,unused` (restart).
- Restart: header salt-1 +1 and checkpoint-seq +1 when the restarting connection writes the
  header; random salts and the writer's own sequence otherwise (correction 1).
- Rollback timeline (5.2 C): spilled frames 42-54 in the file, mxFrame 41 in the `-shm`.

## Platform implications (facts)

1. **One stuck reader grows the WAL without bound** — whether it pinned a mark (5.3 A: 533 of 555
   frames uncheckpointable, 2.29 MB after 24 commits) or started on a fully checkpointed WAL
   (5.3 B: nBackfill 0 for all 533 frames). Autocheckpoint keeps running (and costing a WAL sync
   each time) and achieves nothing. The platform must bound read-transaction lifetime per database.
2. **The platform must own checkpoints.** Autocheckpoint is a PASSIVE checkpoint run by whichever
   connection commits once the WAL holds ≥ N frames (main.c:2484-2496), and any complete
   checkpoint lets the next writer restart the WAL (5.4: PASSIVE then write → restart). A WAL
   shipper that does not run the checkpoints itself sees generations change behind its back. Set
   `wal_autocheckpoint=0` on every application connection and checkpoint from the platform.
3. **The `-wal` is part of the database while it is open.** 5.2 A: after 8 commits the database
   file alone is still recipe step 0; 638,632 bytes of state are only in the `-wal`. A capture
   is base + WAL taken at the same moment (R3), or a checkpoint first.
4. **The `-shm` is not.** SQLite rebuilds it from the `-wal` (5.2 B, with no `-shm` and with a
   stale one from another database: same bytes). Never back it up, never ship it. Do not open and
   close it (or the database) in a process with a connection (os_unix.c:4703-4750): copy from a
   child or after close.
5. **Three numbers per database**: WAL size; `log` and `checkpointed` from a checkpoint (or the
   free NOOP): `log − checkpointed` is the backlog a reader is holding; and `busy` from a
   FULL/RESTART/TRUNCATE: 1 = someone is still reading or writing.
6. **A WAL file does not shrink after a checkpoint**; it is reused from frame 1 (5.3 A: still
   2,286,632 bytes after the restart). Only TRUNCATE (5.4) or `journal_size_limit` (not tested
   here) shrinks it; the last close deletes it.
7. **Generation change detection** for shipping: compare the WAL header salts; any change is a new
   generation (correction 1). Frames of the old generation stay in the file behind the new ones
   (5.3 A: 18 valid of 555; 5.4: 23 of 38) and fail the salt check.
8. **Rolled-back work stays in the `-wal` file** (5.2 C: 13 frames) until overwritten; a shipper
   must stop at the last commit frame, exactly as the replayer does.

## WAL-default evaluation (ruling 10) for this chapter

WAL is the subject of this chapter and is the default everywhere in it; no experiment uses
rollback mode. Nothing found argues against WAL as the platform default. What WAL obliges a
platform to own, with the evidence:

| Obligation | Evidence |
| --- | --- |
| Run checkpoints itself (autocheckpoint off on app connections) | 5.4 restart after any complete checkpoint; main.c:2484-2496 |
| Bound reader lifetime per database | 5.3 A and B |
| Treat `-wal` as part of the database while any connection is open; capture base + WAL together or checkpoint first | 5.2 A |
| Never capture or ship `-shm`; it is rebuilt | 5.2 B (none / stale) |
| Watch WAL size and the checkpoint triple | 5.3, 5.4 |
| Detect WAL generations by salt change | 5.4, correction 1 |
| Shared memory needed: no WAL on network filesystems | wal.c:130-133 (source; not tested) |

## Linux-only and the Mac route

Nothing in chapter 5 is Linux-only: no strace, no `/proc`; child processes are the experiment
binary; files are read with plain Go. On a Mac: run the same `go run` commands natively (5.1
needs `showwal` from `go run ./cmd/course build-tools` for its cross-check, else it prints
`NO-SHOWWAL:` and the evidence differs in that block only), or `docker compose run --rm lab go
run ./ch05-wal-transactions-checkpoints/experiments/<name>`. darwin: cross-built and
cross-vetted only. Not verified on darwin: that the modernc darwin build maps the `-shm` the
same way (os_unix.c is shared, so the layout should be identical, and both darwin arches are
little-endian so `ParseIndex` would read it the same); that a child process sees the mmapped
`-shm` contents through `read` on APFS (Linux page cache: yes); the 300 ms busy-wait BOUND on
macOS scheduling. The WAL magic would read 0x377f0683 on a big-endian host (none supported).

## Working scripts

Indexed in `agent_work/ch05/INDEX.md`: `shm-crosscheck` (walfile vs `showshm`).

## Not done / for later chunks

- No prose, no comments (seat rule).
- Neon's adversarial items not built: 5.1 other page sizes and older-generation tail (covered in
  unit tests, not in the experiment); 5.2 flipped byte in the last commit, stale-generation base,
  applying past the last commit; 5.3 several readers, half-read result set,
  `journal_size_limit`; 5.4 writer racing each mode, second connection checkpointing.
- Stale-base detection needs capture metadata (correction 6) — `internal/walship`, chapter 9.
- `showshm` is not part of `cmd/course build-tools`; if the chapter wants it as a cross-check,
  add it there (one more make target).
