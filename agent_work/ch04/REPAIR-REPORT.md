# Chapter 4 — repair round 1 (Opus)

Inputs: `REVIEW-1.md` (Neon, VERDICT: FAIL), `adversarial/results-run1.txt` + `adversarial/RESULTS.md`
(12 BROKEN), `build-notes.md`. main (ch01, ch02) was already merged into the branch; the merged tree
built and passed `go test ./...` before any repair. Post-repair attack run: `adversarial/results-run2-repair.txt`.
New facts for Neon are appended to `build-notes.md` under "Repair round 1 (Opus)".

Commits: `7e3f353` (journalfile parser), `946753a` (4.1 / 4.2 checks and flags, 4.2 evidence),
`b757bb9` (dropped comments that no longer fit; moved new 4.2 helpers so they do not sit under
`budgetBytes`'s doc comment), `b84a6c4` (super-journal checksum follows the platform's C char
signedness; build-notes appendix), plus the commit holding this report.

## Findings and attacks

| id | outcome | test added |
| --- | --- | --- |
| R1 (blocker) short first header → negative `TrailingByte`, wrong truncation | **fixed** `7e3f353`: when the journal (minus any super-journal footer) is shorter than the first header's `SectorSize`, `Parse` marks it `HeaderMissing`, `TrailingByte = end`, no records; `Playback` predicts nothing (pager.c:1607-1610) | `TestJournalShorterThanItsFirstSector` (100, 28 cold, 28 hot, 511 bytes; 512 still synced), `TestTrailingByteNeverNegative` (every prefix of a 2-segment journal) |
| R2.1 (blocker) unsigned super-journal checksum | **fixed** `7e3f353` + `b84a6c4`: sums the name as C `char`s — signed except on linux/arm, arm64, ppc64(le), riscv64, s390x, where C `char` is unsigned (`cCharIsSigned`) | `TestSuperJournalChecksumFollowsCCharSignedness`, `TestSqliteWrittenSuperJournalChecksum` (Neon's real 45-byte `café` name, 0x0d98), `TestSuperJournalChecksumWhereCCharIsUnsigned` (flips the variable; 0x0f98) |
| R2.2 (blocker) NUL-leading name accepted | **fixed** `7e3f353`: `name[0]==0` is ignored (pager.c:1337); also names ≥ 513 bytes are ignored (`len >= nSuper`, nSuper = 1+512), and the name ends at its first NUL | `TestSuperJournalNameStartingWithNulIsIgnored`, `TestSuperJournalNameTooLongIsIgnored` |
| R2.3 Playback ignores super-journal condition | **fixed** `7e3f353`: `Playback()` refuses to predict a rollback when `SuperJournal != ""` (`StopReason` names it; no pages, `TruncateTo` 0); new `PlaybackIfSuperJournalExists()` gives the prediction for when the file exists | `TestPlaybackWithSuperJournalIsConditional` |
| R3 (should-fix) 4.2 `-rows 50` divide by zero | **fixed** `946753a`: flags need `rows >= 1.2 × max(200, budget pages)` and a budget ≥ 1 MiB; calibration fails cleanly if `CACHE_USED(200) <= CACHE_USED(100)`. `-rows 50` now prints `FATAL flags: -rows 50 is too small for -cache-size -2000: need rows >= 600 …` | none (experiment `main`; verified by running the flag combinations below) |
| R4 (should-fix) test reads the db file with `os.ReadFile` while a connection is open | **fixed** `7e3f353`: `liveJournal` closes the connection, reads the file, reopens | the test itself |
| R5 (should-fix) four weak 4.1 checks | **fixed** `946753a`: record triples checked as lengths (4, 4096, 4); `usesJournal` also needs a `pwrite64` and a `sync`; part C needs `RecordsPresent() > 0`; `describeStates` needs `.Exact` on both files. Output unchanged | none (experiment checks; 4.1 run twice) |
| R6 (nit) kill landing checked by text only | **fixed** `946753a`: `landedOn` requires the unfinished call's text AND its per-thread ordinal to equal the target's; `recreateKilled` uses it too. Found while fixing: the parsed log of a killed child can show the killed `pwrite64` as unfinished on two threads (the second with ordinal 1); the check now accepts the thread whose ordinal matches | none |
| R7 (nit) part C compares raw bytes at one checkpoint | **fixed** `946753a`: `pagesMatch` compares cached-page totals at every checkpoint | none |
| R8 (nit) build-notes `pager.c:1523` | **fixed**: now `pager.c:1508` | — |
| JRN-01 | **fixed** (= R1); now HELD | as R1 |
| JRN-02a | **fixed** (= R2.1/R2.2); now HELD | as R2 |
| JRN-02b | **fixed** (= R2.3); now HELD | as R2.3 |
| JRN-05b | **fixed** (= R1): fuzz `negativeTrailing` 221 → 0; now HELD | `TestTrailingByteNeverNegative` |
| CAC-01b | **fixed** `946753a`: a positive `cache_size` is a page count, so the budget check is now "the cache holds at most N pages" (the old byte test compared pages × 4352 with N × 4096 and failed for every positive size); part B's "not a per-connection cap" test is `hi > full && lo < full` (was `lo < full/10`, true only for large N). `-n 4 -k 2 -rows 600 -cache-size 500` now exits 0; now HELD | none |
| CAC-01c | **partly fixed, rest answered.** Fixed: part D's pool bound is now `K × (full+1)` pages, the sum of the open caches' `nMax` (the old "K × full + 1%" was a tolerance that fit only large caches; at defaults the measured 3863 ≤ 3864). Answered: `-cache-size -50` (11 pages, 50 KiB) is now rejected at flag validation, because with such tiny budgets per-connection fixed memory and allocator noise swamp the RSS checks the experiment exists to make. The attack still prints BROKEN (exit 1, `failed=none`) because it expects exit 0 | none |
| JRN-04 | **answered** — the claim needs a scope, not the code. "The journal before COMMIT is not hot and the database untouched" holds for a transaction that fits in the page cache (4.1's step 3: spill=0). With a 55-page UPDATE at cache_size=10, `pagerStress` spills: `syncJournal` writes magic + count (first byte 0xd9) and pages reach the db file before COMMIT. SQLite still recovers exact pre-tx bytes (JRN-05a, RCM-03b HELD). Recorded in build-notes | — |
| RCM-03a | **answered** — same scope: 4 / 3 / 0 syncs per commit are for a non-spilling transaction; with spills FULL made 20 and NORMAL 11, records and page writes interleave. Recorded in build-notes | — |
| RCM-06 | **answered — the build-notes claim (and the chapter sentence based on it) was wrong; no experiment claims it.** A file that shrank is truncated in `pager_end_transaction` AFTER the journal is finalized (pager.c:2155-2164); Phase One only extends (6704-6715). A kill on that `ftruncate` leaves logically post-step content in a file with trailing slack pages. 4.1's "never a third state" still holds logically. Recorded in build-notes; chapter correction listed below | — |
| CAC-01a | **answered** — correction 8 (N pages hold N−1) holds for N ≥ 10 (measured 10, 100, 200, 483). For N ≤ 2 the cache rests at N (1→1, 2→2; 0 and -1 → 0; -10 → 2) and mid-scan holds 4 pages pinned by the cursor. Huge sizes allocate lazily. 4.2 makes no tiny-N claim. Recorded in build-notes | — |
| CAC-02a | **answered** — a new fact; no experiment or note claimed otherwise. In this driver build `PRAGMA shrink_memory` on any one connection empties every connection's unpinned pages: `pcache1Shrink` zeroes the group's `nMaxPage` and evicts from the group-wide LRU (pcache1.c:839-851, 621-632). Same root cause as correction 5. Recorded in build-notes | — |
| CAC-04 | **answered** — 4.2 part E's files are checkpointed, so its claim holds for its setup. `getPageMMap` reads a page through the cache when the WAL has a frame for it (pager.c:5719-5726), so mmap does not help pages that sit in an uncheckpointed WAL. Recorded in build-notes | — |
| cache-limits `group rc=1` | **the attack itself is wrong.** Its `child-path-len` builds a 1500+-byte database path; SQLite's unix VFS limit is `MAX_PATHNAME` 512 (os_unix.c:180), so the open fails with `unable to open database file (14)` and the child exits 1. Not a ch04 defect. The attacks were not edited | — |

## Stale prose for Neon

Chapter `ch04-pager-and-page-cache/ch04-pager-and-page-cache.md`:
- `go run ./cmd/course check all`: `FAIL inline -check` with 4 stale include blocks, at lines 258
  (4.1 `main.go`), 3322 (`internal/journalfile/journalfile.go`), 3793 (4.2 `main.go`), 4486
  (`evidence/4.2-cache-budget.txt`). They need regenerating from the current files.
- Line 157 (item 7, `sqlite3PagerCommitPhaseOne`): "truncates or extends the file if its size
  changed (line 6713)" is wrong. Phase One only *extends* an undersized file (6704-6715). A file
  that shrank is truncated in `pager_end_transaction` after the commit point (2155-2164), so a
  crash there leaves a committed database with trailing slack pages (RCM-06).
- Line 243 (claim 2: "syncs the journal before any write reaches the database file: 4 syncs per
  commit…") and §6.1 (line 4628, cold journal before `syncJournal`): both hold only for a
  transaction that fits in the cache. With spills there are more syncs (FULL 20, NORMAL 11 in
  RCM-03a), the journal becomes hot before COMMIT, and the database file is written before COMMIT
  (JRN-04). §6.5's first bullet (spill not exercised live) can now cite JRN-04/JRN-05a/RCM-03a/b.
- Line 94 (glossary, mmap) and 4695 (lesson 9): mmap bypasses the cache only for pages with no
  WAL frame (CAC-04).
- Line 4610 / build-notes correction 8: "N pages hold N−1" applies to N ≥ 10; tiny caches behave as
  listed above (CAC-01a).
- Line 4581 and 4619 (part D): the evidence line now reads `… stay within 8 x (482 + 1), the sum of
  their cache sizes in pages`; "3,863 (within 1% of 3,856)" should become "3,863 ≤ 8 × 483 = 3,864".
- Line 3764: `HeaderMissing` is now also used when the journal is shorter than its first sector
  (decoded fields kept, `TrailingByte` = bytes present).
- Line 3765: the footer checksum is a sum of C `char`s (signed on amd64/darwin, unsigned on
  linux/arm64 etc.), NUL-leading names and names ≥ 513 bytes are ignored, and the name ends at its
  first NUL.
- Line 3767: `Playback()` now refuses to predict a rollback when a super-journal is named; the
  new `PlaybackIfSuperJournalExists()` gives the prediction for when it exists.
- Line 3791 (4.2 optional flags): `-rows` must be ≥ 1.2 × max(200, budget pages); `-cache-size`
  must be a budget of ≥ 1 MiB; positive `-cache-size` is checked in pages.
- A new fact for the cache section: `PRAGMA shrink_memory` on any connection empties every
  connection's cache in this driver build (CAC-02a).

Code comments (Go):
- `internal/journalfile/journalfile.go`: the `HeaderMissing` comment ("fewer than
  HeaderFieldsSize bytes") was deleted because it is no longer accurate. The `Playback` doc comment
  does not mention the super-journal refusal. Uncommented new code: the `maxSuperJournalLen`
  constant, `PlaybackIfSuperJournalExists`, `playback`, `cCharIsSigned`, `superJournalChecksum`, and
  the short-first-sector branch in `Parse`.
- 4.1 `main.go`: the comment inside `runKill` ("Confirm strace intercepted the exact intended
  syscall…") was deleted along with its code. Uncommented new code: `landedOn`,
  `recordTriplesWellFormed`. The `runKill` doc comment still applies.
- 4.2 `main.go`: uncommented new code: `calibrationPages`, `minBudgetBytes`, `minimumRows`,
  `withinBudget`, `pagesMatch`. The comment above the part A calibration still applies.
- Test file: the comment in `liveJournal` ("Reading the -journal file while the SQLite connection
  is open is safe…") still applies; the db file itself is now read with the connection closed.

## Commands run (final tree)

- `gofmt -l ch04-pager-and-page-cache internal cmd`: nothing printed. (`gofmt -l .` also lists five
  files under `agent_work/probes/`; those are not this chunk's files and I did not touch them.)
- `go build ./... && go vet ./... && go test ./...`: PASS (the new journalfile tests included).
- `bash agent_work/verify/crossbuild.sh`: `CROSSBUILD OK`.
- `go run ./cmd/course check all`: exit 1.
  - PASS: sources (ch01/02/04), spans (ch01/02/04), inline for ch01 and ch02, evidence for every
    experiment (4.1 and 4.2 each "2 runs identical to the evidence"), gofmt (91 files).
  - FAIL `inline -check` ch04: the 4 stale chapter include blocks listed above. Fixing them means
    editing the chapter, which is Neon's job.
  - FAIL `check paths`: `_tools/bin/kb:13297`, a tracked binary that came from main in `c51ff44`.
    It was already failing before this round and is not from this chunk.
- 4.1, run by hand twice: `ATOMIC COMMIT OK` both times, EXACT output identical to each other and
  to the stored evidence, which was left unchanged. About 15 s per run.
- 4.2, run by hand twice: `CACHE BUDGET OK` both times, EXACT output identical; the evidence was
  rewritten because one claim line changed. About 3 s per run. It also passed with
  `-n 4 -k 2 -rows 600 -cache-size 500`, `-n 16 -k 8 -rows 400 -cache-size -1024`,
  `-n 16 -k 8 -rows 1000 -cache-size 300` and `-n 8 -k 2`. It now rejects `-rows 50` and
  `-cache-size -50` with `FATAL flags:` lines.
- Adversarial: there is no `agent_work/ch04/adversarial/run-all.sh` (the suite is partial), so each
  group was run with `go run ./agent_work/ch04/adversarial/<group>`. Output is in
  `adversarial/results-run2-repair.txt`. 17 HELD, 7 BROKEN (JRN-04, RCM-03a, RCM-06, CAC-01a,
  CAC-01c, CAC-02a, CAC-04). Each of the 7 is answered in the table above; CAC-01c's code part was
  fixed. Run 1 had 12 HELD and 12 BROKEN. Group rc: journal-parser 0, rollback-crashes 0, cache-limits 1 (the attack's own
  over-long path; see the table).
