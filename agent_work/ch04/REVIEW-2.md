VERDICT: NITS

Re-reviewed repair commits `7e3f353`, `946753a`, `b757bb9`, `b84a6c4`, and `075ed33` (`git diff 9f08af5..HEAD`, branch `chunk/ch04`), plus adversarial run 2 (`agent_work/ch04/adversarial/results-run2.txt`, which also includes the new `cache-scale` group added in `5319cce`).

All 8 findings from `REVIEW-1.md` (including both blockers R1 and R2) and all 12 Run-1 `BROKEN` attacks are fixed or answered with source/experimental evidence. No regressions were introduced. Builds, vet, unit tests (with `-race`), cross-builds (`linux/amd64`, `darwin/arm64`, `darwin/amd64`), `check sources ch04`, `check spans ch04`, `check gofmt`, and `check evidence ch04` (both 4.1 and 4.2 run twice identically to stored evidence) all pass.

## Findings & Run-1 attacks verification

| Finding / Attack | Fixed? | How confirmed |
| --- | --- | --- |
| R1 (blocker) / `JRN-01`, `JRN-05b` — sub-sector first header caused negative `TrailingByte` and predicted truncation | **Fixed** (`7e3f353`) | Read `internal/journalfile/journalfile.go:193-199`: when `end < sector`, `first.State = HeaderMissing`, `TrailingByte = end`, no records, `Playback()` returns `TruncateTo = 0` and no pages (matching `readJournalHdr`, `pager.c:1607-1610`). Confirmed by `go test -v -race ./internal/journalfile/...` (`TestJournalShorterThanItsFirstSector`, `TestTrailingByteNeverNegative`), `JRN-01` (`HELD`: `hot TrailingByte=100 cold TrailingByte=28 Playback.TruncateTo=0`), and `JRN-05b` (`HELD`: `5000 iterations: panics=0 negativeTrailing=0`). |
| R2.1 (blocker) / `JRN-02a` — unsigned super-journal checksum rejected non-ASCII UTF-8 names | **Fixed** (`7e3f353`, `b84a6c4`) | Read `internal/journalfile/journalfile.go:320-332`: `superJournalChecksum` sums C `char` bytes according to platform signedness (`cCharIsSigned`: signed on `linux/amd64` and `darwin/*`, unsigned on `linux/arm`, `arm64`, `ppc64(le)`, `riscv64`, `s390x`). Confirmed by `TestSuperJournalChecksumFollowsCCharSignedness`, `TestSqliteWrittenSuperJournalChecksum` (`0x0d98`), `TestSuperJournalChecksumWhereCCharIsUnsigned` (`0x0f98`), and `JRN-02a` (`HELD`: `SQLite recognized non-ASCII=true, journalfile.SuperJournal=".../café-mj01"`). |
| R2.2 (blocker) / `JRN-02a` — NUL-leading super-journal name accepted | **Fixed** (`7e3f353`) | Read `internal/journalfile/journalfile.go:307-317`: rejects `length > maxSuperJournalLen` (512) and `name[0] == 0` (`pager.c:1315,1337`), truncates at first NUL. Confirmed by `TestSuperJournalNameStartingWithNulIsIgnored`, `TestSuperJournalNameTooLongIsIgnored`, and `JRN-02a` (`HELD`: `NUL-leading rejected=true (trailing=22)`). |
| R2.3 / `JRN-02b` — `Playback()` predicted unconditional rollback when a super-journal is named | **Fixed** (`7e3f353`) | Read `internal/journalfile/journalfile.go:390-401`: `Playback()` returns `Hot: true`, empty `Pages`, `TruncateTo: 0`, and a `StopReason` naming the super-journal; `PlaybackIfSuperJournalExists()` returns the conditional playback plan. Confirmed by `TestPlaybackWithSuperJournalIsConditional` and `JRN-02b` (`HELD`). |
| R3 (should-fix) — 4.2 `-rows 50` divide-by-zero panic | **Fixed** (`946753a`) | Read `4.2-cache-budget/main.go:145-152, 486-491, 535-537`: enforces `rows >= 1.2 * max(200, budgetPages)`, `budget >= 1 MiB`, and `large > small`. Confirmed live: `-n 4 -k 2 -rows 50` exits 1 cleanly with `FATAL flags: -rows 50 is too small for -cache-size -2000: need rows >= 600 ...`. |
| R4 (should-fix) — `liveJournal` called `os.ReadFile` on DB while SQLite connection was open | **Fixed** (`7e3f353`) | Read `internal/journalfile/journalfile_test.go:281-292`: closes `db`, reads `before` with `os.ReadFile(path)`, reopens `db`, registers `t.Cleanup`. Passes `go test -race`. |
| R5 (should-fix) — four weak 4.1 `check(...)` predicates | **Fixed** (`946753a`) | Read `4.1/main.go:525-541, 934, 1203-1208, 1295, 1552-1553`: `recordTriplesWellFormed` verifies `(4, pageSize, 4)` lengths; `usesJournal` requires `pwrite64 ` and `sync `; `partC` initializes `allOriginal := j.RecordsPresent() > 0`; `describeStates` checks `.Exact` on both matches. Confirmed by `check evidence ch04` (2 runs of 4.1 identical to evidence). |
| R6 (nit) — 4.1 kill landing checked by text only, `recreateKilled` unchecked | **Fixed** (`946753a`) | Read `4.1/main.go:697, 1379-1405`: `landedOn` matches both `target.text` and per-PID `target.ordinal` alongside `run.Killed == "SIGKILL"`, and is used by both `runKill` and `recreateKilled`. Confirmed by 2 clean runs of 4.1. |
| R7 (nit) — 4.2 Part C compared raw `CACHE_USED` bytes at one checkpoint | **Fixed** (`946753a`) | Read `4.2/main.go:458-468, 593`: `pagesMatch(oneFile, files)` compares `pagesTotal()` across every checkpoint. Confirmed by `check evidence ch04`. |
| R8 (nit) — `build-notes.md` cited `pager.c:1523` for `sqlite3_randomness` | **Fixed** (`b84a6c4`) | Read `agent_work/ch04/build-notes.md:202`: updated to `pager.c:1508`. |
| `CAC-01b` — positive `-cache-size 500` failed 4.2 budget and Part B checks | **Fixed** (`946753a`) | Read `4.2/main.go:154-159, 550-554, 578`: positive `cache_size` checked in pages (`cachedPages <= cacheSize`), Part B uses `hi > full && lo < full`. Confirmed live (`-n 4 -k 2 -rows 600 -cache-size 500` → `CACHE BUDGET OK`, exit 0) and `CAC-01b` (`HELD`). |
| `CAC-01c` — Part D `K*full*1.01` bound and tiny `-cache-size -50` | **Fixed + Answered** (`946753a`) | Part D bound fixed to `k * (full + 1)` (`4.2/main.go:603`, matching `PGroup.nMaxPage`); tiny budgets `< 1 MiB` cleanly rejected at flag validation (`4.2/main.go:489-491`, confirmed live: `-cache-size -50` prints `FATAL flags:`). Attack EXPECT updated in Part 2. |
| `JRN-04`, `RCM-03a`, `RCM-06`, `CAC-01a`, `CAC-02a`, `CAC-04` | **Answered** (`075ed33`, `b84a6c4`) | Claims narrowed/corrected in `REPAIR-REPORT.md` and `build-notes.md:794-834` with source lines (`pager.c:2155-2164, 5719-5726, 6704-6715`; `pcache1.c:621-632, 839-851`) and measurements; chapter prose updates are queued for the upcoming prose pass. Attacks updated in Part 2 to assert the corrected/narrowed claims. |
| `CAC-05` (`cache-limits` rc=1) | **Attack bug** | `REPAIR-REPORT.md` confirmed the attack's ~1,550-byte path exceeded SQLite's unix VFS `MAX_PATHNAME = 512` (`os_unix.c:180`). Fixed in Part 2 to test a valid ~460-byte path (< 512 B) and verify `cachedPages` invariance alongside raw `CACHE_USED` growth (~3× path length). |

## New findings

1. **nit** — `ch04-pager-and-page-cache/experiments/4.2-cache-budget/main.go:483, 605`
   - **What is wrong:** Flag validation accepts `-k 1` (and `-k 2` at the minimum `-cache-size -1024`), where one-time Go runtime / SQLite initialization RSS (~2–3 MiB) is large relative to `K` connections' cache RSS, causing Part D's proportionality check `float64(atN) <= float64(rssN)*float64(k)/float64(n)*1.5` (line 605) to fail even when pooled RSS is flat.
   - **How confirmed:**
     ```
     $ go run ./ch04-pager-and-page-cache/experiments/4.2-cache-budget -n 64 -k 1
     INFO: RSS growth at after 64 252.9 MiB ...
     INFO: RSS growth at after 1 6.0 MiB, at after 64 6.7 MiB
     check ok: with the pool, process memory stays flat after 1 connections ...
     check FAILED: the pooled run ends with about 1/64 of the unbounded run's memory growth (at most 1.5 x that fraction)
     ```
     (`6.7 MiB > 1.5 × (1/64) × 252.9 = 5.93 MiB`; `-n 64 -k 2 -cache-size -1024 -rows 320` similarly gives `7.3 MiB > 6.31 MiB`). Not a blocker: defaults (`-n 64 -k 8`), `-n 64 -k 2`, and 4× scale (`SCL-06`: `-n 256 -k 16 -cache-size -1024 -rows 320`) all pass with `CACHE BUDGET OK`.
   - **Suggested follow-up (non-blocking):** Either require `k >= 2` (or `k * budgetBytes >= 4 MiB`) in flag validation or include a fixed startup-overhead term in line 605.

2. **nit** — `agent_work/ch04/build-notes.md:104` (prose-pass scoping note from `SCL-07`)
   - **What is wrong:** `build-notes.md:104` still states `"The SUM is always exactly (open connections) × 482 pages."` Repair round 1 already updated Part D (`4.2/main.go:603`, `build-notes.md:785-789`) to the true group ceiling `open × (full + 1) = pGroup->nMaxPage`. As `SCL-07` confirms on 16 open WAL connections (`cache_size=-2000`, `full=482`, `nMax=483`), `open × 482` (`7,712`) holds on a single first-pass sweep (Part B), whereas a second round-robin scan across evicted connections (`nPage=0 < nMax`) allocates up to `pGroup->nMaxPage = 16 × 483 = 7,728` (`+16` pages) before `pcache1Unpin` (`pcache1.c:1096`) trims excess pages.
   - **How confirmed:** `SCL-07` in `agent_work/ch04/adversarial/results-run2.txt` (`pass1=7712 (0..1004) pass2=7728 (0..1004) pass2Max=7728 (group nMaxPage=7728, delta=+16 pages)`) and `ch04-pager-and-page-cache/pcache1.c:902, 1096`.
   - **Suggested follow-up (prose pass):** Qualify the Part B / correction 5 prose so readers see both facts: a first sweep across `N` open connections sums to `N × (nMax - 1)`, while repeated sweeps or pool churn reach the group cap `N × nMax = pGroup->nMaxPage`. Also note for the prose pass the `SCL-05` Linux accounting observation: when one process opens many `mmap`-ed connections to the *same* file, `/proc/self/status` `RssFile` sums resident PTEs per VMA (`+401 MiB` for 1,000 VMAs of one 408 KiB file), whereas `/proc/self/smaps_rollup` `Pss_File` reflects the shared kernel page cache (`~408 KiB`).

## Verification commands run

- `go build ./... && go vet ./... && go test ./...` — PASS
- `go test -v -race ./internal/journalfile/...` — PASS (21 tests, 0 races)
- `bash agent_work/verify/crossbuild.sh` — `CROSSBUILD OK` (`linux/amd64`, `darwin/arm64`, `darwin/amd64`, `check gofmt`: 92 files formatted)
- `go run ./cmd/course check sources ch04` — PASS (4 copied files match pin 3.53.4)
- `go run ./cmd/course check spans ch04` — PASS (44 spans start at their symbol)
- `go run ./cmd/course check evidence ch04` — PASS (both 4.1 and 4.2 ran twice with EXACT output identical to stored evidence)
- `go run ./ch04-pager-and-page-cache/experiments/4.2-cache-budget -n 4 -k 2 -rows 50` — exit 1 (`FATAL flags:`, no panic)
- `go run ./ch04-pager-and-page-cache/experiments/4.2-cache-budget -n 16 -k 8 -rows 300 -cache-size -50` — exit 1 (`FATAL flags:`)
- `go run ./ch04-pager-and-page-cache/experiments/4.2-cache-budget -n 4 -k 2 -rows 600 -cache-size 500` — `CACHE BUDGET OK` (exit 0)
- `go run ./ch04-pager-and-page-cache/experiments/4.2-cache-budget -n 64 -k 1` — reproduces nit 1 (`6.7 MiB > 5.93 MiB`)
- `go run ./ch04-pager-and-page-cache/experiments/4.2-cache-budget -n 64 -k 2` — `CACHE BUDGET OK` (exit 0)
