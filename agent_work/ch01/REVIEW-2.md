VERDICT: PASS

Re-reviewed commit `c98bf12` (`git diff ac24187..HEAD -- '*.go'`, with repair commits `71d6f0a`, `8d5175d`, `12c7004`, `8f59174`, `c98bf12` on top of the foundation merge `a7f12ab`). Every finding from `REVIEW-1.md` and every formerly `BROKEN` code defect from `agent_work/ch01/adversarial/RESULTS.md` is fixed and verified, all evidence files reproduce identically, and no new blockers or real bugs were introduced.

## Verification of findings and formerly BROKEN attacks

| Finding / Attack | Fixed? | How confirmed |
| --- | --- | --- |
| R1 blocker: WAL `pwrite64` check negative slice bound and ignored leading writes (`1.2-one-insert/main.go:616, 746-756`) | Yes | Read diff: `expectedFreshWALWrites` builds the full expected list (`0+32` WAL header, then `offset+24` and `offset+24+4096` per frame) and `runMode` checks `slices.Equal(walWrites, expectedFreshWALWrites(frames))` with no slicing. `TestExpectedFreshWALWrites` passes for normal frames, `nil`, too-few writes, and extra leading writes. |
| R2 blocker: freelist/B-tree page classifier contradicts file format (`1.3-recipe-rebuild/main.go:344-521`) | Yes | Read diff: `readFreelist` separates trunks (with leaf counts) from leaves, validates file size, header page size, page bounds, leaf counts (`<= (pageSize-8)/4`), duplicate/cyclic pages, and header freelist total; `unusedBytes` marks only `8+4*leafCount..pageSize-1` unused on trunks, all bytes on leaves, rejects non-B-tree page types (`0x02`/`0x05`/`0x0a`/`0x0d`) as having no unused bytes, and enforces strictly ascending in-bounds freeblocks (`block >= previousEnd`, `size >= 4`); any parse error forces `OnlyUnused = false`. All 5 regression tests in `1.3-recipe-rebuild/main_test.go` pass. |
| R3 blocker: darwin path exits 1 on unused CLI, ignores `db.Close()`, lacks portable check (`1.2-one-insert/main.go:537-545, 730-744, 785-805`) | Yes | Read diff: `sqlitex.LocateCLI` is gated behind `if trace.Supported`; the `!trace.Supported` branch checks `db.Close()`, calls `recipe.CheckAtRest`, and runs `checkChangedPages` (`[1 2]` rollback, `[2]` WAL). `TestExpectedChangedPages` and `bash agent_work/verify/crossbuild.sh` (linux/amd64, darwin/arm64, darwin/amd64) pass. |
| R4 should-fix / DET-06: Correction 1 omits `pList->pgno == 1` (`agent_work/ch01/build-notes.md:458-478`, `1.3-recipe-rebuild/main.go:682-688, 724-750`, `1.2-one-insert/main.go:737-744`) | Yes | Read build-notes addendum ("Correction 1, restated"), verified 1.3 variant 6 new check (`page1-untouched` 3 commits: `identical`, evidence lines 124–125) and 1.2 WAL changed-pages check (`[2]`, evidence line 182). |
| R5 should-fix: `normalized()` drops errno/short transfers/lengths; twin check omits non-zero syncs and at-rest checks (`1.2-one-insert/main.go:328-375, 539, 561, 644, 662-663, 671, 684`) | Yes | Read diff: `normalized` appends `= ERRNO` on errors and `= N` on short transfers, `read`/`write` include `len=`, twin check requires `len(goSyncs) > 0` and `goFileHash != baseHash`, and `recipe.CheckAtRest` runs after Go child, CLI, and split runs. `TestNormalizedKeepsErrorsLengthsAndShortTransfers` passes and 1.2 evidence reproduces `pread64 t.db-journal off=9216 len=8 = 0`. |
| R6 nit: `:memory:` pool in 1.1 (`1.1-pin-check/main.go:127`) | Yes | Read diff: `db.SetMaxOpenConns(1)` added immediately after `sql.Open("sqlite", ":memory:")`. |
| R7 nit / API-09: `Validate` in `Step`/`BuildEach`, temp file cleanup on rename failure, ignored test errors (`internal/recipe/recipe.go:212-214`, `apply.go:136-138`, `hashes.go:390-393`, `recipe_test.go:82-89, 271-278`) | Yes | Read diff: `Step` and `BuildEach` call `r.Validate()` first; `writeHashCache` removes `temp.Name()` on `os.Rename` error; `TestSeedsDiffer` and `TestLongLivedConnectionDiffersOnlyInChangeCounterUnderWAL` check errors. `TestInvalidRecipeIsRejectedByStepAndBuild`, `TestWriteHashCacheRemovesTempFileWhenRenameFails`, and adversarial `API-09` (`HELD`) pass. |
| R8 nit: partial-page panic in `compareFiles`/`fileDiff.String()` and unchecked `conn.Close()` (`1.3-recipe-rebuild/main.go:60-62, 94-122, 203-205`) | Yes | Read diff: `diffImages` rounds page count up (`(max + pageSize - 1) / pageSize`) and slices safely via `pageSlice`; `fileDiff.String()` guards empty `DifferingPages`; `applyStatementsInSession` checks `conn.Close()`. `TestPartialPageDifferenceDoesNotPanic` passes. |
| API-02: `MatchStep` accepts disagreeing counters (`internal/recipe/hashes.go:227-254`) | Yes | Read diff: exported `ChangeCountersAgree` compares bytes 24–27 against 92–95; `MatchStep` rejects masked-only matches when they disagree. `TestMatchStepRejectsDisagreeingCounters` and adversarial `API-02` (`HELD`) pass. |
| API-04b: `StepHashes` trusts tampered/stale cache (`internal/recipe/recipe.go:31`, `hashes.go:138, 154, 261-375`) | Yes | Read diff: `FormatVersion = 2` appends a SHA-256 `checksum` over cache key, `File`/`Conn` settings, planned SQL+args, and all hashes; `readVerifiedCache` verifies the checksum and rebuilds step 0 in a temp dir before trusting the cache. `TestStepHashesRejectsTamperedAndStaleCaches` and adversarial `API-04b` (`HELD`) pass. |
| TWIN-10, DET-08c, DET-05d, DET-08a, DET-08b, DET-08d, DET-08e, DET-02 (`cs=0`), PIN-04, DET-04 | N/A (claim narrowing / stale attacks) | Confirmed in `1.2-one-insert` (`twinLimitAnalyze` demonstrates `ENABLE_STAT4` / `sqlite_stat4` divergence), `1.3-recipe-rebuild` (variant 4 FAST vs ON, variant 6 page-1-untouched sessions, variant 7 rolled-back `SAVEPOINT`), `build-notes.md` addendum, and `adversarial/results-run2.txt`. Prose and stale attack expectations to be updated in Parts 2–3. |

## New findings

No blockers or real bugs were introduced. Three minor non-blocking nits noted for future reference:

1. **nit** — `internal/recipe/hashes.go:270, 297-298` (`readHashCache`)
   - Local variable `var body strings.Builder` is written to on every hash line (`body.WriteString(line)`, `body.WriteString("\n")`) and never read or returned. Confirmed by inspection of `hashes.go:263-307`.
2. **nit** — `ch01-architecture-and-execution-path/experiments/1.3-recipe-rebuild/main.go:703` (`main`, variant 7 WAL check)
   - `onlyCounterBytes(walAborted.Page1Bytes)` returns `true` for an empty slice, so if page 1 ever stopped differing in WAL mode, the assertion claiming "and page 1's change counter moves" would still evaluate true. Confirmed by inspection of `onlyCounterBytes` (`main.go:132-145`) and line 703. (In the actual run `walAborted.Page1Bytes` is `[27 95]`, so the check holds on live output.)
3. **nit** — `ch01-architecture-and-execution-path/experiments/1.2-one-insert/main.go:830` (`statTables`)
   - `defer db.Close()` discards any close error on the read-only `sqlite_schema` inspection, unlike every `db.Close()` in `twinLimitAnalyze`. Confirmed by inspection of `main.go:825-848`.

## Commands run

All with `export SQLITE_SRC=/root/Software/sqlite-src/3.53.4`:

- `go build ./... && go vet ./... && go test -count=1 ./...` — PASS (`cmd/course`, `internal/recipe`, `internal/repocheck`, `internal/sqlitex`, `internal/trace`, `sqlite`, `1.2-one-insert`, `1.3-recipe-rebuild`).
- `go test -race -count=1 ./internal/recipe/... ./ch01-architecture-and-execution-path/...` — PASS.
- `bash agent_work/verify/crossbuild.sh` — `CROSSBUILD OK` (linux/amd64, darwin/arm64, darwin/amd64, `check gofmt` 54 files).
- `go run ./ch01-architecture-and-execution-path/experiments/1.1-pin-check` — exit 0.
- `go run ./ch01-architecture-and-execution-path/experiments/1.2-one-insert` — exit 0.
- `go run ./ch01-architecture-and-execution-path/experiments/1.3-recipe-rebuild` — exit 0.
- `go run ./cmd/course check sources ch01` — PASS (4 copied files match the pin).
- `go run ./cmd/course check spans ch01` — PASS (12 spans start at their symbol).
- `go run ./cmd/course check evidence ch01` — PASS (1.1, 1.2, 1.3: 2 runs each identical to evidence).
- `go run ./cmd/course check prose a7f12ab..HEAD` — PASS (0 prose lines introduced in repair range).
