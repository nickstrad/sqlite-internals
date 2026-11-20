# Chunk 1 repair report (round 1)

Inputs: `agent_work/ch01/REVIEW-1.md` (FAIL, 8 findings), `agent_work/ch01/adversarial/results-run1.txt`
(11 BROKEN), `adversarial/RESULTS.md`. Repair base: `a7f12ab` (ch01 merged with the repaired
foundation from main).

Step 0, the merged tree: build, vet and tests passed unchanged. 1.1 reproduced its evidence. 1.2 and 1.3
did not: every WAL-mode hash moved. The cause is the foundation's new pragma order on a new file
(layout, then `journal_mode`, then `application_id`/`user_version`). In WAL mode that removes one
rollback-mode commit, so the change counter is 2 lower. A byte compare of the old and new step-30 files
shows only byte 27 (and 95) differ (0x22 → 0x20). Rollback-mode hashes did not change. Evidence was
regenerated after the fixes below.

Commits: `71d6f0a` (recipe), `8d5175d` (1.1, 1.2 and the 1.2 evidence), `12c7004` (1.3 and its
evidence), plus the commit that adds this report.

## Findings and attacks

| id | outcome | regression test |
| --- | --- | --- |
| R1 blocker: WAL write check slices with a negative bound and ignores earlier writes | fixed `8d5175d`: `expectedFreshWALWrites` builds the full list (`0+32` WAL header of the new -wal, then each frame's 24-byte header and 4096-byte page) and the check compares the whole list with `slices.Equal` (no slicing, extra writes fail) | `1.2-one-insert/main_test.go` `TestExpectedFreshWALWrites` (too few writes and an extra leading write must not match) |
| R2 blocker: freelist/b-tree classifier contradicts the file format | fixed `12c7004`: `readFreelist` separates trunks (with leaf counts) from leaves, bounds-checks trunk/leaf numbers and leaf counts, detects pages listed twice (cycles), and checks the header's freelist count and page size. On a trunk only bytes `8+4*leafCount..` count as unused. A page that is not a b-tree page (type byte not 0x02/0x05/0x0a/0x0d) has no unused bytes. Freeblocks must lie in the content area in ascending order with size >= 4, so the walk always ends. Classifier errors make `OnlyUnused` false | `1.3-recipe-rebuild/main_test.go` `TestFreelistTrunkMetadataIsNotUnused`, `TestFreelistRejectsMalformedChains`, `TestNonBtreePageIsNeverUnused`, `TestCyclicFreeblocksTerminate`, `TestFreeblocksAndGapAreUnused` |
| R3 blocker: darwin path exits 1 on an unused CLI; unchecked close; no portable check | fixed `8d5175d`: the CLI is located only when `trace.Supported`. The portable branch checks `db.Close()` and `CheckAtRest`, and `checkChangedPages` checks [1 2] in rollback mode and [2] in WAL mode. That check runs on Linux too | `TestExpectedChangedPages`. The darwin run is cross-built only, not executed |
| R4 should-fix: Correction 1 omits `pgno==1` | answered in notes (no code involved): build-notes addendum "Correction 1, restated", shown by the new 1.3 line under variant 6 (`identical`) and by 1.2's WAL changed-pages check. I did not touch the original Correction 1 text; the addendum supersedes it | n/a (experiment check) |
| R5 should-fix: `normalized` drops errno/results/lengths; no sync or at-rest checks | fixed `8d5175d`: errno on every call (`= ERRNO`), short transfers (`= N`), and `len=` on read/write. The twin check requires >= 1 sync and a file that differs from the base. `CheckAtRest` runs after the Go child, the CLI run and the split run | `TestNormalizedKeepsErrorsLengthsAndShortTransfers` |
| R6 nit: `:memory:` pool in 1.1 | fixed `8d5175d` (`db.SetMaxOpenConns(1)`) | none (one-line change) |
| R7 nit: Validate in Step/BuildEach, temp file on rename failure, ignored test errors | fixed `71d6f0a` | `TestInvalidRecipeIsRejectedByStepAndBuild`, `TestWriteHashCacheRemovesTempFileWhenRenameFails`; `TestSeedsDiffer` and `TestLongLivedConnection…` now check errors |
| R8 nit: partial page panic, unchecked `conn.Close` | fixed `12c7004`: page count rounds up, a partial last page is compared, `String()` guards empty `DifferingPages`, `conn.Close()` checked | `TestPartialPageDifferenceDoesNotPanic` |
| API-02 disagreeing counters match | fixed `71d6f0a`: `MatchStep` rejects a masked-only match when bytes 24–27 differ from 92–95 (new `ChangeCountersAgree`). `TestMatchStepAndCache` now moves both counters together, as SQLite does | `TestMatchStepRejectsDisagreeingCounters`; attack now HELD |
| API-04b tampered cache trusted | fixed `71d6f0a`: cache v2 has a checksum line (key, settings, every planned statement and hash). Step 0 is rebuilt and compared before the cache is used. This also catches stale caches from foundation changes like this merge, which left the old cache key unchanged | `TestStepHashesRejectsTamperedAndStaleCaches` (no checksum, old checksum, recomputed checksum with wrong step 0, one tampered later step); attack now HELD |
| API-09 `Step(0)` on an invalid recipe | fixed `71d6f0a` | `TestInvalidRecipeIsRejectedByStepAndBuild`; attack now HELD |
| TWIN-10, DET-08c ANALYZE differs (STAT4) | real divergence between the builds, not a defect. Now demonstrated in 1.2, new section "a limit of the twin check": both copies start identical, then ANALYZE in each build. The driver writes `sqlite_stat1` and `sqlite_stat4`, the CLI only `sqlite_stat1`, and the files differ. The check fails if the CLI has the same STAT4 setting but the files differ. Attacks stay BROKEN by design | experiment check |
| DET-05d, DET-08a secure_delete=FAST | claim too broad. Demonstrated in 1.3 (variant 4, new line): FAST vs ON on the same history differ on pages [29 36 37], all in unused space of freelist pages. Source: btree.c:6865-6876 zeroes freed pages only for full ON. FAST *batching* was identical in 1.3's small variant, so only DET-05d's output backs "FAST batching differs" | experiment check |
| DET-06 WAL counter without page 1 | claim too broad (= R4). Demonstrated in 1.3: 3 commits that never write page 1, per-session vs one session: identical | experiment check |
| DET-08b rolled-back savepoint | new source of variation. Demonstrated in 1.3, new variant 7, in both modes (facts in build-notes addendum) | experiment check |
| DET-08d pool churn | one-line statement backed by the attack's output: same mechanism as variant 6. The recipe sets `SingleConnection` | n/a |
| DET-08e localtime | one-line statement backed by the attack's output: environment-dependent SQL, same class as variant 2 | n/a |
| run 2, PIN-04 (new BROKEN) | attack expectation is stale, not a defect. With `driver_version=v1.99.9`, 1.1 still exits 1, but the repaired foundation's `MustPinCheck` (via `CheckDriverModules`) stops first with `FAIL: this binary links modernc.org/sqlite v1.60.1, the pin is v1.99.9`. 1.1's own `check FAILED: the linked driver is …` line is never reached. Reproduced in a scratch copy | n/a |
| run 2, DET-04 (new BROKEN) | the attack is wrong now. Its CLI builder hard-codes the pre-repair order (`application_id` before `journal_mode`). The repair probe `cli-order` shows the CLI step 0 equals the Go step 0 with the new order and differs with the old one | n/a |
| run 2, DET-02 (new BROKEN, cs=0 in WAL) | real, newly exposed by the foundation order, not a ch01 code defect. `cache_size=0` changes only the WAL counter, from step 0 on, deterministically. Values 1, 10 and -2000 do not (repair probe). The recipe pins `cache_size=-2000`. Recorded for Neon as "cache_size is part of the contract" | n/a |

## Stale prose for Neon

The chapter (`ch01-architecture-and-execution-path.md`, line numbers at HEAD) needs re-inlining
(`course inline`): `check inline` reports 8 stale blocks at lines 121, 572, 1355, 1721, 2045, 2243,
2536 and 3151. The new facts are in `build-notes.md` → "Repair round 1 addendum".

Chapter statements made stale:
- 101: 1.3 now also has the FAST-vs-ON line, a page-1-untouched line, and variant 7 (rolled-back SAVEPOINT).
- 490–494: line numbers of the 1.1 evidence are unchanged. The statement on 494, "None of those
  differences affect file format or I/O", is false for `ENABLE_STAT4`: ANALYZE writes `sqlite_stat4`
  only in the driver, and the files differ (1.2, new section).
- 567–570 (1.2 claim/platform): the twin claim holds for statements that do not depend on compile
  options that differ between the builds (STAT4/ANALYZE shown). The macOS path prints `EXPLAIN`,
  `LINUX-ONLY:` (text now mentions "its ANALYZE limit"), and changed pages with a new check line. It no
  longer reports or fails on a missing CLI.
- 1534 / 1584 / 1654 and every WAL hash quoted: WAL base is now `5de4f3aefd26bcea`, WAL after-INSERT
  `1afce8154bf3124c`. Rollback hashes are unchanged.
- 1645–1654: 1.2 evidence line numbers shifted. New check lines at 28 and 182 (changed pages). WAL
  frame check text changed (now names the WAL header write and "nothing else"). `pread64
  t.db-journal off=9216 len=8` now prints `= 0` (short read: probing past the end of the journal).
  The ANALYZE section is at 276–282.
- 1716: "in WAL mode, SQLite's file change counter counts connection sessions rather than commits".
  It counts only sessions (or page-1 re-reads) whose commits write page 1. Every recipe step writes page 1.
- 1717: the cache is now v2 with a checksum and a step-0 rebuild check. A tampered or stale cache
  is rebuilt.
- 3190–3224, 3276: all WAL step hashes changed (step 0 `7470585a…`, step 30 `24aa158f…`). The 1.3
  evidence line numbers shifted after variant 4.
- 3293: wording holds, but add that it needs secure_delete exactly ON (FAST differs).
- 3298 and 3329 ("advances … per connection session rather than per commit"): too broad (see 1716). It
  can also move inside one session when page 1 is evicted and re-read (cache_size=0).
- 3305–3312 (determinism rules table): add rows for **secure_delete exactly ON** (variant 4 FAST
  line), **no rolled-back savepoints inside recipe transactions** (variant 7), **one connection per
  session / no pool churn** (DET-08d), **no environment-dependent SQL such as `'localtime'`**
  (DET-08e), **compile options that change contents (STAT4 + ANALYZE) differ between builds**
  (1.2), and **cache_size is part of the recipe** (DET-02 run 2). Row 3312 should say "sessions
  whose commits write page 1".
- 3316 `<!-- adversarial-results -->`: run 2 results are in `adversarial/results.txt` (see below).
- 3324: fine. 3328 ("matches the C build's storage I/O"): add the STAT4/ANALYZE limit.
- 3339: syscall counts unchanged (33/31, 18/17).
- 3341: holds. FAST would also differ.

Code comments made stale (code changed under them, comment kept):
- `internal/recipe/apply.go:107` (`ApplyInSession`): the counter advances per step only because every
  step writes page 1.
- `internal/recipe/hashes.go:127` (`StepHashes`): "valid entries" now means a matching checksum and a
  matching fresh step 0.
- `1.2-one-insert/main.go:325` (`normalized`): it now also appends errno, short-transfer results and
  read/write lengths.
- `1.3-recipe-rebuild/main.go:3` (package doc, "six sources of variation"): there are now seven
  sources and two extra lines (FAST vs ON, page-1-untouched sessions).
- Deleted, because their code was rewritten: the doc comments of `freelistPages` and `unusedBytes`
  (now `readFreelist` and `unusedBytes`, uncommented). New code (`readFreelist`, `classifyImages`,
  `diffImages`, `secureDeleteFastVariant`, `sessionsWithoutPage1`, `abortedSavepointVariant`,
  `twinLimitAnalyze`, `statTables`, `checkChangedPages`, `expectedFreshWALWrites`,
  `ChangeCountersAgree`, `readVerifiedCache`, `cacheChecksum`, `freshStepZeroHash`) has no comments.
- `agent_work/ch01/build-notes.md`: Correction 1, the "settings other than secure_delete" line,
  and all quoted WAL hashes are superseded by the addendum.

## Commands run (final state)

All with `SQLITE_SRC=/root/Software/sqlite-src/3.53.4`.
- `gofmt -l cmd internal sqlite ch01-architecture-and-execution-path agent_work/ch01`: nothing.
- `go build ./... && go vet ./... && go test -count=1 ./...`: PASS (recipe, both new experiment test
  packages, cmd/course, repocheck, sqlitex, trace, sqlite).
- `bash agent_work/verify/crossbuild.sh`: `CROSSBUILD OK`.
- `go run ./cmd/course check all`: exit 1. sources PASS, spans PASS, evidence PASS (1.1, 1.2 and 1.3:
  2 runs identical to the evidence), gofmt PASS (50 files). **inline FAIL**: 8 chapter blocks are
  stale (listed above; Neon re-inlines). **paths FAIL**: 1 problem, inside the binary `_tools/bin/kb`
  (line 13297), which arrived with the main merge and is not ch01's.
- Experiments, twice each, EXACT output identical between runs and written to `evidence/`:
  1.1 exit 0 (0.3–1.2 s), 1.2 exit 0 (1.2–2.4 s), 1.3 exit 0 (10.2–11.9 s; was ~10 s before the new
  variants).
- `bash agent_work/ch01/adversarial/run-all.sh` (unedited; 115 s): 37 HELD, 11 BROKEN. Now HELD:
  API-02, API-04b, API-09. Still BROKEN and answered as claim corrections, now demonstrated or
  stated: TWIN-10, DET-05d, DET-06, DET-08a–e. Newly BROKEN since run 1 because of the
  foundation merge: PIN-04 (stale output expectation), DET-04 (attack replays the old pragma
  order), DET-02 cs=0 (real; recorded as a rule). `results-run1.txt` is kept; `results.txt` is run 2.
- `go run ./agent_work/ch01/repair-probe` (default, `counters`, `cli-order`): outputs quoted in the
  table and in the build-notes addendum.
