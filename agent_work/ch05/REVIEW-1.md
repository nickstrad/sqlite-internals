VERDICT: FAIL

Reviewed commit `3efc5b1` (`git diff 7690293..HEAD`). All `file:line` references below are at `HEAD` (`3efc5b1`).

## Findings

1. **blocker** — `ch05-wal-transactions-checkpoints/experiments/5.2-wal-replay/main.go:220-257` (and `ch05-wal-transactions-checkpoints/evidence/5.2-wal-replay.txt:26-31, 49-54`)
   - **What is wrong:** Experiment 5.2 never exercises stopping at `LastCommitFrame` when valid uncommitted frames follow it (`ValidFrames > LastCommitFrame`), and its Part C check message contradicts the replay report printed directly above it.
     - In Part A, `ValidFrames == 155 == LastCommitFrame`.
     - In Parts B and C, running both step 11 and `UPDATE events SET note = upper(note)` with `cache_size=10` spills frames 42–54 and then overwrites page 2 in place at frame 42 (`wal.c:4147-4163`), breaking the running checksum at the very first spilled frame. Thus `ValidFrames == 41 == LastCommitFrame`, and the replay report prints `ignored frame 42: checksum mismatch (running checksum chain broken)` followed by `ignored frames 43-54 (12): follows an invalid frame`.
     - Directly underneath that output, line 247 asserts `check ok: the replayer ignores the rolled-back frames because no commit frame follows them` — false for this WAL, because frame 42 was rejected by its checksum, not because no commit frame followed valid frames.
     - Consequently, if `walfile.Replay` had a bug that applied `file.Frames[:file.ValidFrames]` instead of `file.CommittedFrames()`, every check in Experiment 5.2 would still pass. The outline claim ("stopping at the last commit frame") is not proven against valid uncommitted frames.
   - **How confirmed:**
     ```text
     $ go run ./ch05-wal-transactions-checkpoints/experiments/5.2-wal-replay > /dev/null
     $ _tools/bin/showwal _work/5.2/b-mid-transaction/capture-open-transaction/r.db-wal | tail -n 15
     Frame   40:     97      0 0xab850d53,328c3ccc 0x5b0e9dbb,fc253318
     Frame   41:    100    100 0xab850d53,328c3ccc 0xf3417fdb,c3a6a96a
     Frame   42:      2      0 0xab850d53,328c3ccc 0xbfcaa157,a760eb89 should be 0xbccc5041,6e07ba40
     Frame   43:    101      0 0xab850d53,328c3ccc 0x27cfb638,5a4b2e0c
     ...
     Frame   54:     89      0 0xab850d53,328c3ccc 0x6004c23a,be1b953c
     $ cmp _work/5.2/b-mid-transaction/capture-open-transaction/r.db-wal _work/5.2/b-mid-transaction/capture-after-rollback/r.db-wal
     (exit 0 — Part B and Part C WAL files are byte-identical)
     ```
     `evidence/5.2-wal-replay.txt:50-54`:
     ```text
       frames in file 54, valid 41, committed transactions 2, last commit frame 41 (database size 100 pages)
       ...
       ignored frame 42: checksum mismatch (running checksum chain broken)
       ignored frames 43-54 (12): follows an invalid frame (0 of them carry other salts)
     check ok: the replayer ignores the rolled-back frames because no commit frame follows them
     ```
   - **Fix expected:** Change the open transaction in Parts B/C (or add a case) so that it spills frames *without* overwriting the first spilled frame in place, yielding `captured.ValidFrames > captured.LastCommitFrame` and triggering `ReplayReport`'s `"valid but uncommitted: no commit frame follows"` path. Assert `captured.ValidFrames > captured.LastCommitFrame` explicitly, and align the check text with the actual ignore reason printed by `ReplayReport`.

2. **should-fix** — `internal/walfile/replay.go:77-89, 146-148`
   - **What is wrong:** `basePageSize(base)` returns `(0, nil)` for an empty `base`, and `Replay` uses `pageSize != 0` at line 146 as the "base is non-empty" check. If a non-empty `base` starts with `"SQLite format 3\x00"` (≥ 100 bytes) and has corrupt page-size bytes `0x00 0x00` at offsets 16–17, `basePageSize` returns `(0, nil)` without error, and line 146 skips the `ErrPageSizeMismatch` check — silently replaying the WAL onto a corrupt base with page size 0. Likewise, when `file.LastCommitFrame == 0`, `basePageSize` accepts any invalid non-power-of-two page size in bytes 16–17.
   - **How confirmed:** Read `internal/walfile/replay.go:77-89`:
     ```go
     size := int(base[16])<<8 | int(base[17])
     if size == 1 {
         size = 65536
     }
     return size, nil
     ```
     and line 146: `if pageSize != 0 && pageSize != walPageSize`. Neither validates `size` against `MinPageSize..MaxPageSize` and power-of-two, and `0` is overloaded as the empty-base sentinel.
   - **Fix expected:** Validate `size` in `basePageSize` (`size < MinPageSize || size > MaxPageSize || size&(size-1) != 0` returns `ErrBaseNotDatabase`), guard line 146 with `len(base) > 0 && pageSize != walPageSize`, and add a unit test in `internal/walfile/walfile_test.go` for a non-empty base with bytes 16–17 zeroed.

3. **should-fix** — `ch05-wal-transactions-checkpoints/experiments/5.3-reader-pins-the-wal/main.go:157-175, 195-202` (and `agent_work/ch05/build-notes.md:418, 423`)
   - **What is wrong:** `take(ctx, path, scratch)` is called *before* `count(ctx, writer)` on the same table row (`after step 6` at lines 157–158 and the commit loop at lines 172–174). Because `count(ctx, writer)` runs `SELECT count(*) FROM events` on `writer`, it executes `walTryBeginRead` (`wal.c:3182-3205`) and updates `aReadMark` in `-shm` *after* that row was sampled, leaking its side effect onto the *next* row:
     - At line 158 (`after step 6`), `count(ctx, writer)` writes `aReadMark[1] = 22` *before* `openReader` runs on line 159. When `openReader` starts, slot 1 already holds `22`, so `reader` merely takes a shared lock on slot 1 instead of setting mark 1. Line 163's `hasMark(pinned.marks, snapshot)` would pass even if `openReader` never started a read transaction, and `build-notes.md:418` inaccurately states `"Reader begins: marks 0,22,… (it set mark 1 to mxFrame)"`.
     - At line 174 (`step 30 committed`), `count(ctx, writer)` advances `aReadMark[2]` from `525` to `555` after `s` was captured. Consequently `reader ended` (line 201) prints `0,22,555` right after `step 30 committed`'s `0,22,525`, making the table look as if ending the reader mutated `-shm` — directly contradicting `build-notes.md:423` ("Reader ends: nothing changes in the `-shm`").
   - **How confirmed:** Traced `5.3-reader-pins-the-wal/main.go:157-202` against `ch05-wal-transactions-checkpoints/wal.c:3182-3205`, and compared with `evidence/5.3-reader-pins-the-wal.txt:6-10, 32, 37`:
     ```text
     after step 6                 90672       22         0  0,0,unused,unused,unused           -         251
     reader began                 90672       22         0  0,22,unused,unused,unused         251         251
     ...
     step 30 committed          2286632      555        22  0,22,525,unused,unused         251        1177
     ...
     reader ended               2286632      555        22  0,22,555,unused,unused           -        1177
     ```
   - **Fix expected:** Do not query `writer` between `take` (after step 6) and `openReader` (print `-` for writer rows on `after step 6`, or defer querying `writer` until after `reader` has pinned mark 1) so `reader` actually sets `aReadMark[1] = 22`. Inside the commit loop (lines 168–182), call `writerRows := count(ctx, writer)` *before* `take(...)` so every row's read marks match that row's actual post-step state and `reader ended` shows the exact same read marks (`0,22,555`) as `step 30 committed`.

4. **should-fix** — `ch05-wal-transactions-checkpoints/experiments/5.4-checkpoint-modes/main.go:162-165, 245-250, 287-292` (and `agent_work/ch05/build-notes.md:470-471`)
   - **What is wrong:** `o.waited = o.elapsed >= checkpointerBusyMs*time.Millisecond` (300 ms) makes wall-clock duration both a pass condition (`check(waitedOK, ...)` at line 292) and a non-`INFO:` column (`waited`) in EXACT evidence. That violates `OUTLINE.md:158` ("timings themselves are never the pass condition") and `common.md:48-49` ("no timestamps, pids, temp paths or durations in EXACT output; print those only on lines prefixed `INFO:`"). Even on this idle Linux ext4 VM, `FULL no reader` already took `83ms` and `RESTART no reader` took `67ms` due to two `fsync`s; under moderate disk load or macOS APFS `F_FULLFSYNC`, any non-waiting checkpoint can exceed 300 ms and fail both the check and the EXACT evidence diff. (`build-notes.md:470-471` also understates the margin by citing only `PASSIVE` at 9–19 ms and ignoring `FULL no reader` at 83 ms.)
   - **How confirmed:** Read `5.4-checkpoint-modes/main.go:162-165, 245-250, 287-292` and `evidence/5.4-checkpoint-modes.txt:10-33`:
     ```text
     FULL      no reader       busy=0 log=38 checkpointed=38  no         156592       38        38         same         same
     INFO: FULL no reader elapsed 83ms
     ...
     RESTART   no reader       busy=0 log=38 checkpointed=38  no         156592       38        38         same         same
     INFO: RESTART no reader elapsed 67ms
     ```
   - **Fix expected:** Remove the wall-clock `waited` column from the EXACT table and drop the `o.elapsed >= 300ms` pass condition; keep elapsed durations strictly on `INFO:` lines (relying on the exact, deterministic `busy=0`/`busy=1` result in the table and assertions). Update `build-notes.md` to match.

5. **should-fix** — `internal/walfile/showwal.go:90-92`
   - **What is wrong:** `(*File).CompareWithShowwal` checks checksum agreement in only one direction (`if i < f.ValidFrames && !s.ChecksumAgrees`). If `walfile` wrongly rejects frame `i == f.ValidFrames` with `FrameChecksumMismatch` while `showwal` reports `s.ChecksumAgrees == true` (e.g., a checksum bug in `walfile` causing `ValidFrames == 0`), `CompareWithShowwal` reports zero problems. Because all frames before `f.ValidFrames` had matching checksums, `showwal`'s running checksum entering frame `f.ValidFrames` equals `walfile`'s, so `w.Problem == FrameChecksumMismatch && s.ChecksumAgrees` is a genuine disagreement.
   - **How confirmed:** Read `internal/walfile/showwal.go:76-95` and `/root/Software/sqlite-src/3.53.4/tool/showwal.c:226-256`.
   - **Fix expected:** Add `if w.Problem == FrameChecksumMismatch && s.ChecksumAgrees` to report a disagreement, and cover it in `TestCompareWithShowwal`.

6. **nit** — `internal/walfile/replay.go:198-202`
   - **What is wrong:** In `ReplayFiles`, if `writeSynced(temp, out)` fails because `os.OpenFile(..., O_EXCL)` returned `os.ErrExist` for a pre-existing `outPath + ".replay-tmp"`, line 200 unconditionally calls `os.Remove(temp)` and deletes a file it did not create.
   - **How confirmed:** Read `internal/walfile/replay.go:198-224`.
   - **Fix expected:** Create the temporary file with `os.CreateTemp(filepath.Dir(outPath), filepath.Base(outPath)+".replay-*")` (or only remove `temp` after `os.OpenFile` succeeds).

7. **nit** — `internal/walfile/livecopy.go:97-100, 166-178`
   - **What is wrong:** (a) `CopyLiveFiles(ctx, nil)` fails with `live copy child: 1 result lines for 0 files` because `strings.Split("", "\n")` returns `[""]`. (b) In `ReadLiveState`, if `ParseIndexFile(indexCopy)` returns an error at line 170, the early return skips removing `walCopy` and `indexCopy`.
   - **How confirmed:** Read `internal/walfile/livecopy.go:81-119, 149-180`.
   - **Fix expected:** Return `nil, nil` early when `len(pairs) == 0` in `CopyLiveFiles`, and defer removal of `walCopy` and `indexCopy` in `ReadLiveState`.

8. **nit** — `internal/walfile/shm.go:89-91`
   - **What is wrong:** `(*Index).HeadersAgree()` compares `x.Headers[0] == x.Headers[1]`, which omits the 4-byte `unused` field (`wal.c:323`, bytes 4–7 vs 52–55). If both header copies have invalid checksums (`ChecksumOK == false`, equal stored `Checksum`) and differ only in bytes 4–7 vs 52–55, `HeadersAgree()` returns `true` whereas SQLite's `memcmp(&h1, &h2, sizeof(h1))` (`wal.c:2922`) treats them as unequal.
   - **How confirmed:** Read `internal/walfile/shm.go:89-91, 138-154` and `ch05-wal-transactions-checkpoints/wal.c:321-333`.
   - **Fix expected:** Compare the raw 48-byte slices (`bytes.Equal(x.data[:IndexHeaderSize], x.data[IndexHeaderSize:2*IndexHeaderSize])` when `x.Initialized && len(x.data) >= 2*IndexHeaderSize`) or decode `Unused` into `IndexHeader`.

9. **nit** — `ch05-wal-transactions-checkpoints/experiments/5.1-wal-inspector/main.go:5`
   - **What is wrong:** Stray blank line between `"context"` and `"fmt"` in the standard library import group.
   - **How confirmed:** Read `5.1-wal-transactions-checkpoints/experiments/5.1-wal-inspector/main.go:3-11`.
   - **Fix expected:** Remove the blank line.

## Outline corrections check (`agent_work/ch05/build-notes.md`)

All 8 corrections to the outline were verified against the pinned SQLite 3.53.4 source (`ch05-wal-transactions-checkpoints/wal.c`, `wal.h`, `$SQLITE_SRC/src/pager.c`) and confirmed accurate:
1. Salt/sequence on restart (`wal.c:535, 1456, 2156, 2383, 3879-3896, 4087-4096`): confirmed — `pWal->nCkpt` is per connection and only modified in `walIndexRecover` and `walRestartHdr`; `walFrames` re-randomizes salts when `pWal->nCkpt == 0`.
2. `RESTART` mode (`wal.c:2353-2389`): confirmed — waits for read locks 1..4 so the next writer restarts; does not call `walRestartHdr`.
3. Read lock 0 (`wal.c:3134-3163, 2259-2260, 3885`): confirmed — allows writer WAL restart (only locks 1..4) while blocking all backfill (`walCheckpoint` requires exclusive `WAL_READ_LOCK(0)`).
4. 5.3 determinism: confirmed — scripted order produces identical EXACT output across runs.
5. Read marks outlive readers: confirmed in `wal.c`, though Experiment 5.3's table currently obscures this at `reader ended` due to Finding 3.
6. Stale-generation base detection: confirmed — database file header stores no WAL salt/generation metadata.
7. `walDecodeFrame` comment (`wal.c:1033` vs `1038`): confirmed — comment says 16 bytes, code checksums 8 bytes.
8. `pager.c:607-617` sync-flag comment vs `wal.h:25-26` and `pager.c:3692-3695`: confirmed — comment is reversed relative to code.

## Commands run

All commands run with `export SQLITE_SRC=/root/Software/sqlite-src/3.53.4`:

| Command | Result |
| --- | --- |
| `git diff 7690293..HEAD --stat` | 26 files changed, 9232 insertions(+) |
| `go build ./... && go vet ./... && go test -v -race ./internal/walfile/...` | PASS (23 tests + fuzz seeds, 7.72s) |
| `GOOS=darwin GOARCH=arm64 go build ./... && GOOS=darwin GOARCH=arm64 go vet ./...` | PASS (`darwin/arm64 ok`) |
| `go run ./cmd/course check sources ch05` | PASS (2 copied files match pin) |
| `go run ./cmd/course check spans ch05` | PASS (28 spans start at their symbol) |
| `go run ./cmd/course check evidence ch05` | PASS (5.1, 5.2, 5.3, 5.4: 2 runs each identical to stored evidence) |
| `gofmt -l internal/walfile ch05-wal-transactions-checkpoints agent_work/ch05` | empty (PASS) |
| `rg '/root\|home/\|Users/' internal/walfile ch05-wal-transactions-checkpoints` | no matches |
| `rg '//\|/\*' internal/walfile ch05-wal-transactions-checkpoints/experiments` | no matches (at `HEAD` before concurrent prose-pass edits in working tree) |
| `COURSE_TOOLS_DIR=/tmp/opencode/empty-tools PATH=/usr/local/go/bin:/usr/bin:/bin go run ./ch05-wal-transactions-checkpoints/experiments/5.1-wal-inspector` | prints `NO-SHOWWAL: ...; cross-check skipped`, `WAL INSPECTOR OK`, exit 0 |
| `_tools/bin/showwal _work/5.2/b-mid-transaction/capture-open-transaction/r.db-wal \| tail -n 20` | Frame 42 checksum mismatch (`should be 0xbccc5041,6e07ba40`); Frames 43–54 carry matching salts |
| `cmp _work/5.2/b-mid-transaction/capture-open-transaction/r.db-wal _work/5.2/b-mid-transaction/capture-after-rollback/r.db-wal` | identical (exit 0) |
