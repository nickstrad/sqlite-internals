# Chapter 3 repair round 1 — report

Repairer: Claude Opus 5.5, branch `chunk/ch03`, 2026-11-21. Inputs: `REVIEW-1.md` (verdict NITS,
6 should-fix + 4 nits) and adversarial run 1 (`adversarial/results-run1.txt`, 14 BROKEN).
Commits of this round:

- `8ba6e76` equal: harden the logical hash, dbhash runner and page changes after review
- `6850eee` ch03: correct experiments 3.1-3.3 after review and adversarial run 1 (code + evidence)
- `0a540bc` equal: keep EncodeValue's signature and tag unsupported types; add 3.3 dump tests
- the commit carrying this report and the build-notes update

## Findings and attacks

| Id | Outcome | Test added |
| --- | --- | --- |
| Review 1 (3.1 non-split check accepts any table leaf, page 1 included) | fixed `6850eee`: the check now requires the one changed page to be a table leaf other than page 1 whose cells contain the inserted rowid (`leafHoldsRowid`) | experiment check (3.1 part A, part D) |
| Review 2 (3.1 part D does not enforce two pages / counter-only page-1 diff) | fixed `6850eee`: two new checks — every non-growing insert changes exactly page 1 + the leaf holding the rowid; page 1 differs only inside bytes 24-27, 92-95 (`CompareImagesIgnoring`) | experiment checks (3.1 part D) |
| Review 3 (3.1 part B discards `Page`/`Cells` errors, no bytes-equal assertion) | fixed `6850eee`: errors are fatal, the check asserts `bytes.Equal(before, after)` for every extra frame; `Cells()` errors at the old lines 332/561 handled | experiment check (3.1 part B) |
| Review 4 (`LogicalHash` leaks its read transaction on ctx cancel) | fixed `8ba6e76`: transaction ends with `context.WithoutCancel(ctx)`, ROLLBACK on error, COMMIT on success | `TestLogicalHashEndsItsTransactionWhenTheContextEnds` (ctx ending after 1..80 `Done()` calls, and a failing walk) |
| Review 5 (3.3 dump corrupts identifiers containing `?`) | fixed `6850eee`: `insertStatement(t, values)` builds the line from the column list and literals; `insertSQL` uses it with `?` marks | `TestDumpReplaysOddValuesAndNames` (3.3 `main_test.go`) |
| Review 6 (missing `rows.Err()` in 3.1 snapshot and 3.3 readContent) | fixed `6850eee` | — (error path only) |
| Review 7 nit (`EncodeValue` collapses unsupported types to empty bytes; strings.Builder) | fixed differently `0a540bc`: signature kept (`[]byte`), unsupported types encode as tag `U` + type name + `%#v`, never empty, never equal to a storage-class encoding; `bytes.Buffer`. Not an error return because the adversarial attack EQ-08 calls `EncodeValue` with one result: an error return stops the whole equal-falsify group from compiling (and `run-all.sh`'s `set -e` then skips the r2 group). The hash path still refuses such values (`StorageClassOf`, and the internal encoder returns an error) | `TestEncodeValueKeepsTypesWithoutAStorageClassApart` |
| Review 8 nit (TEMP shadows `main` in the walk) | fixed `8ba6e76`: `main.sqlite_schema`, `pragma_table_info(?, 'main')`, `FROM main."t"`, `PRAGMA main.*` including integrity/foreign-key checks | `TestLogicalHashReadsMainWhenTempShadowsIt` |
| Review 9 nit (3.2 VACUUM search ignores NEW-/GROW- markers; trailing ": ") | fixed `6850eee`: step 5 searches the 220 deleted NEW-/GROW- rows (OFF 217, ON 0); step 6 searches all 424 deleted rows; no trailing ": " | experiment checks (3.2) |
| Review 10 nit (unused `row.columns`; interleaved part B checks; `CompareLogical` one-sided pragmas, string-joined fk compare) | fixed `8ba6e76`, `6850eee` | — |
| SPL-01 (random keys: 86/250 one-page) | real: the claim "most inserts change one page" is not general. Experiment corrected `6850eee`: 3.1 part E shows random keys with 4 rows/leaf (86 of 250) vs small rows (566 of 600); build-notes correction 7 | experiment checks (3.1 part E) |
| SPL-03 (overflowing rows: 0/25 one-page) | real, same claim scope: 3.1 part E shows 0 of 25, 3..5 pages each | experiment check (3.1 part E) |
| SPL-05 (WITHOUT ROWID append split ≠ 3 pages) | not a code defect — 3.1's three-page check is on a rowid table and `balance_quick` requires a table leaf. Scope made explicit: 3.1 part E shows WITHOUT ROWID splits at depth 2 change 5 pages; correction 7 | experiment check (3.1 part E) |
| SPL-07b (`ChangedPages` reports role-only changes not in WAL) | fixed `8ba6e76`: a page counts as changed only when its bytes change (or it appears/disappears) | `TestChangedPages` (role change with same sha not reported) |
| DEL-02 (auto_vacuum FULL shrinks) | real scope error in "the file never shrinks on its own": holds for auto_vacuum NONE only. 3.2 auto_vacuum part added `6850eee`; correction 8 | experiment checks (3.2) |
| DEL-05 (secure_delete=ON rows still in `-wal`) | real: the claim held for the database file only. 3.2 `-wal` part added `6850eee`: 40/40 after DELETE and PASSIVE, 0 after TRUNCATE; correction 9 | experiment checks (3.2) |
| DEL-06 (growth while freelist non-empty in one transaction) | real wording error in the 3.2 check. Reworded ("every transaction that added pages ended with an empty freelist") and a large-transaction part added `6850eee` (freelist 2→0, page_count 12→18); correction 10 | experiment check (3.2) |
| EQ-03 (default_cache_size: E3 equal, VACUUM INTO differs) | fixed `8ba6e76`: `default_cache_size` is the seventh logical pragma | `TestLogicalHashSeesDefaultCacheSize` |
| EQ-04 (IPK named `rowid` refused) | fixed `8ba6e76`: fall back to the INTEGER PRIMARY KEY column (single pk, declared INTEGER, no pk-origin index) | `TestLogicalHashUsesAnIntegerPrimaryKeyNamedRowid` (+ DESC / INT / composite still refused) |
| EQ-05 (LogicalHashFile / RunDBHash checkpoint a live WAL snapshot) | fixed `8ba6e76`: `HasLiveSideFile`; a database with a non-empty `-wal`/`-journal` is opened `mode=ro` (dbhash gets a `file:…?mode=ro` URI). Side effect: a `-shm` is created if missing | `TestHashingALiveWALSnapshotChangesNoFile` |
| EQ-08 (TEMP shadow + EncodeValue collision) | fixed (review 8 and 7 rows above) | as above |
| EQ-09 (ctx cancel leaks transaction) | fixed (review 4 row) | as above |
| R2-02 (dump sorts tables by name, replay canonicalizes differently) | fixed `6850eee`: the dump writes CREATE statements in `sqlite_schema` rowid order; 3.3 part C1 now shows the replay of an out-of-order source canonicalizes to the source's own bytes | experiment check (3.3 C1), `TestDumpReplaysOddValuesAndNames` |
| R2-03b (embedded NUL; `?` identifiers) | fixed `6850eee`: NUL written as `'a'||char(0)||'b'`; insertStatement (review 5) | `TestDumpReplaysOddValuesAndNames`, `TestSQLLiteralSplitsTextAtNUL` |

Also found while repairing: my first version of the context test raced (the driver calls `Done()`
from its own goroutine); fixed before commit with an atomic counter and `sync.Once`; 15 `-race`
runs of `./internal/equal` passed.

## Stale prose for Neon

Neon has not written comments or a chapter for ch03 yet, so no chapter or comment text is stale.
Facts in `build-notes.md` that changed (the notes are updated in place; search "repair round"):

- `internal/equal`: `ChangedPages` = byte changes only; seven logical pragmas
  (`default_cache_size` added); every query reads `main` explicitly; IPK fallback for the rowid
  alias; `EncodeValue` tags unsupported Go types with `U`; `LogicalHash` ends its transaction
  despite cancellation (ROLLBACK on error); `HasLiveSideFile` and read-only opens in
  `LogicalHashFile` / `RunDBHash`; `CompareLogical` compares pragmas both ways.
- 3.1: non-split check wording ("the table leaf that now holds the new rowid, never page 1");
  part D has three checks; new part E and correction 7; evidence 133 lines.
- 3.2: step-4 check wording (correction 10); step-5 and step-6 marker searches (424 rows); new
  parts (large transaction, auto_vacuum, `-wal` residue); corrections 8-10; evidence 96 lines.
- 3.3: logical sha256 of every build is now `3de6a887…` (was `1a656547…`); the R0 summary lists
  `default_cache_size=-2000`; the dump keeps creation order and writes NULs as `char(0)`; the
  capture check wording ("depends on content and the schema's creation order…"); part B checks
  follow the table; part C1 has a replay line and check; correction 2 extended
  (default_cache_size, creation order in the capture); evidence 100 lines. Dump sha256
  `92d38985…`, file and VACUUM INTO sha256 values are unchanged.

## Commands run

All on linux/amd64, `SQLITE_SRC` set to the pinned tree, after the last code commit `0a540bc`.

- `gofmt -l internal ch03-btrees-and-page-management` — empty. `course check gofmt` — PASS (96 files).
- `go build ./... && go vet ./... && go test -count=1 ./...` — all pass (equal 13.7 s, dbfile
  32 s, 3.3 `main_test.go` 0.5 s, every other package ok).
- `go test -count=1 -race ./internal/equal` — 15 consecutive runs pass (after the test-context
  race fix); the new context test also ran 6 more times under `-race` on its own.
- `bash agent_work/verify/crossbuild.sh` — `CROSSBUILD OK` (darwin/arm64 and linux/amd64 build +
  vet). darwin: cross-built only, never run.
- Each experiment run twice by hand (`go run ./ch03-btrees-and-page-management/experiments/<x>`):
  exit 0 both times, output identical apart from `INFO:` lines; first run copied to the evidence
  file. Last lines `PAGES PER INSERT OK`, `DELETE AND REUSE OK`, `SAME ROWS OK`. Runtimes here:
  3.1 ≈ 21 s, 3.2 ≈ 5 s, 3.3 ≈ 6-11 s.
- `go run ./cmd/course check all`:
  - PASS: gofmt; sources ch02, ch03; spans ch01 (12), ch02 (18), ch03 (22); evidence — every
    experiment of ch01, ch02 and ch03 "2 runs identical to the evidence" (3.1, 3.2, 3.3 included).
  - FAIL `inline -check` ch01 (8 problems) and ch02 (12 problems): those chapters' code blocks
    differ from this branch's ch01/ch02 code, which is older than main's (the branch predates
    main's chapter-1 closing rewrite). This round touched no ch01/ch02 file
    (`git diff fbe4a3a..HEAD` lists only ch03 and `internal/equal`). Pre-existing, outside this
    chunk.
  - FAIL `check paths` (1 problem): `_tools/bin/kb`, a binary committed by `c51ff44` (infra:
    vendor kb, before this chunk), contains a machine-local path. Pre-existing, outside this chunk.
- `bash agent_work/ch03/adversarial/run-all.sh` (attacks unedited; output in
  `adversarial/results.txt`, run-1 output kept in `results-run1.txt`): 22 HELD, 8 BROKEN (run 1:
  16 HELD, 14 BROKEN). Now HELD: SPL-07b, EQ-03, EQ-04, EQ-05, EQ-08, EQ-09. Still BROKEN:
  - SPL-01, SPL-03, SPL-05, DEL-02, DEL-05, DEL-06: these attacks' EXPECT clauses encode the
    unqualified outline/build-round wording, so they stay BROKEN by design. Their GOT values match
    what the corrected experiments now show and check (3.1 part E; 3.2 large-transaction,
    auto_vacuum and `-wal` parts; corrections 7-10). Answered by correcting the claims, not
    the code: SQLite behaves as the attacks measured.
  - R2-02, R2-03b: **these attacks are stale.** `r2-canonical-and-pin/main.go` line 85: "r2Dump
    mirrors 3.3-same-rows-different-bytes/main.go's dump() verbatim". It tests a frozen copy of
    the build-round dump (name-ordered CREATE TABLE, raw NUL, `strings.Replace` of `?`), not the
    experiment's code. The repaired `dump()` handles the same inputs:
    `TestDumpReplaysOddValuesAndNames` uses the attack's cases (`'head'||char(0)||'tail'`, table
    `"t?"("?" TEXT PRIMARY KEY) WITHOUT ROWID`, a `"?, ?"` column, zebra created before alpha),
    and its replay is E3-equal and its VACUUM INTO is E1-equal to the source's. 3.3 part C1 now
    checks the creation-order case in the evidence. Neon must refresh the mirror (or call the
    experiment's code) for these two attacks to measure the repaired dump.
