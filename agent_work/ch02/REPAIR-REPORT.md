# Chapter 2 — repair round 1 (Opus)

Inputs: `REVIEW-1.md` (VERDICT: FAIL, findings 1–10), `adversarial/results-run1.txt` (22 HELD,
14 BROKEN). Repair commits: `4bedd95` (dbfile), `462841e` (capture, equal tests), `cf3a151`
(experiments, sqlitex, evidence), and the commit holding this report.

**WITHOUT ROWID decision (scope guard): handled, not a stated limitation.** The census and R1
capture now accept a `WITHOUT ROWID` table: its `sqlite_schema` row has `type = 'table'` and its
tree is an index b-tree, so its pages get the roles `index-interior` / `index-leaf` / `overflow`
and its `Tree.Type` stays `"table"`. Only an `index` entry whose root is a table page is refused.
`WalkTable` (row decoding) still refuses a WITHOUT ROWID root ("page N is the root of an index,
not a table"): decoding records of every table kind stays out of scope (OUTLINE 2.2 Why kept).
Census, `dbstat` comparison and capture/rebuild are tested on such a table.

## Findings and attacks

| id | outcome | test added |
| --- | --- | --- |
| R1 [blocker] WITHOUT ROWID rejected | fixed `4bedd95`: census rejects only an `index` entry with a table-page root | `TestWithoutRowidTableCensus` (census + dbstat on a WITHOUT ROWID table with overflow and a secondary index), `TestRejectsIndexEntryWithTableRoot` |
| R2 [blocker] `os.Open` descriptor held while SQLite connections are open (2.2 A–E, dbfile tests) | fixed `cf3a151`, `4bedd95`: 2.2 reads each database into memory (`os.ReadFile` + `dbfile.FromBytes`) before any connection opens; Part E (1 GiB sparse file) opens with `dbfile.Open`, runs the census, and closes before opening SQLite and before `os.Remove`. Tests: `openParsed` reads into memory; the sparse-file test closes its `File` before any query | none possible as a unit test (process-wide lock rule); checked by reading every `dbfile.Open`/`os.Open` call site in 2.1–2.3 and the tests |
| R3 [should-fix] free-space checks missing | fixed `4bedd95`: freeblocks must be ≥ 4 bytes apart (`btree.c:2146`); content start + fragmented + freeblock bytes ≤ usable size (`btree.c:2166`); plus `checkTreePage`'s coverage check (`btree.c:11076-11108`): cells and freeblocks must not share a byte, and the uncovered bytes of the content area must equal header byte 7 | `TestRejectsPageHeaderCorruptionLikeIntegrityCheck` (5 cases, each confirmed rejected by `integrity_check` inside the test) |
| R4 [should-fix] non-zero next on last overflow page | fixed `4bedd95` | `TestRejectsNonZeroNextOnLastOverflowPage` (self, page 2, 9999) |
| R5 [should-fix] 2.2 Part B checks constants | fixed `cf3a151`: asserts each row's parsed (payload, local, overflow pages, bytes on the last overflow page) | experiment check (evidence lines 316–319) |
| R6 [should-fix] `parseAll` overwrites `err` | fixed `cf3a151`: stops at the first error | experiment (no separate test) |
| R7.1 [should-fix] 2.1 A vacuous rows | fixed `cf3a151`: rows with no oracle print `n/a`; the check counts fields with an oracle (18 of 19 with the CLI; `version-valid-for` has none) and names the oracle set (`PRAGMA only, no CLI` without a CLI) | experiment |
| R7.2 2.1 C2 | fixed: asserts each UPDATE changed one row and grew the `-wal`, and that the checkpointed file differs from the base | experiment |
| R7.3 2.1 F | fixed: asserts `h.PageSize == size` and the stored bytes 16–17 (`1` for 65536) | experiment |
| R8.1 2.3 tautological guard | fixed: `len(entries)*4096 < len(pageBytes)` | experiment |
| R8.2 2.3 C refusals not checked for the page | fixed: each case names its expected substring (`page 100: missing`, `page 50: listed twice`, `page 121: out of order`, `capture page 200:`, `capture page 7:`) | experiment |
| R8.3 2.3 `queryString` swallows errors | fixed: `fatal` on query or close error; `mode=ro` passed as `url.Values` | experiment |
| R9.1 partial capture left on failure | fixed `462841e`: `Pages` removes an earlier `pages.bin`/`manifest.txt`, writes `*.capture-tmp`, renames only after the final at-rest/size/mtime checks, removes temps on failure | `TestFailedCaptureLeavesNoCapture` (a test hook writes a `-wal` mid-capture; the directory must end empty and `Rebuild` must refuse it) |
| R9.2 Rebuild gaps | fixed `462841e`: refuses a 0-page manifest; propagates non-`ErrNotExist` side-file stat errors; removes the temp file when the rename fails | 0-page case in `TestRebuildRefusesACaptureTheCensusContradicts`; the rename-failure and stat-error paths have no portable unit test |
| R10 nits | all fixed: unused `crypto/sha256` import removed; ignored errors checked in `dbfile_test.go`, `equal_test.go`, `capture_test.go`; `rows.Err()` checked in `readDBStat` and `TestRowsMatchSQLite`; 2.2 finds `documents` by name (`treeNamed`); 2.1 E uses `errors.Is(err, dbfile.ErrCorrupt)` | — |
| HDR-03 | **answered, and the experiment corrected** (`cf3a151`, 2.1 C4). The attack's EXPECT encodes the over-general claim ("one connection ⇒ constant counter"); its own result shows the true rule: the WAL commit writes `dbFileVers + 1`, where `dbFileVers` is refreshed only when page 1 is read into the cache (`readDbPage`, pager.c:3093-3110). `PRAGMA shrink_memory` evicts page 1 → counter advances each commit; a `cache_size=2` scan did not evict it → constant. 2.1 now shows both: C1 (cached, 30 30 30) and C4 (shrink_memory before each step, 30 31 32). The attack stays BROKEN because its expectation is the old claim | experiment 2.1 C4 |
| HDR-06 | **answered, no change.** The two oracles disagree with each other: `PRAGMA user_version` / `application_id` return signed −1, `.dbinfo` prints every field with `%u` (`shell.c.in:5561-5562`, `get4byteInt` returns `unsigned int`) → 4294967295. `dbfile` decodes both as `int32`, equal to PRAGMA. The attack's EXPECT (agree with both) cannot hold for any decoder. Note: 2.1 A compares against `.dbinfo` strings, so a negative value in R0 would show a mismatch; the recipe never stores one | — |
| HDR-08 | fixed `4bedd95`: `WALMode()` = byte 19 == 2, as `lockBtree` decides (btree.c:3341) | `TestWALModeFollowsReadVersion` |
| HDR-10 | fixed `cf3a151`: `sqlitex.ErrNoCLI` marks "nothing found"; 2.1 prints `NO-CLI:` only for that and fails (exit 1) when `$SQLITE3_BIN` names nothing or a found CLI has the wrong pin. Measured: missing `$SQLITE3_BIN` → exit 1; fake CLI with wrong id → exit 1; no CLI anywhere → `NO-CLI:`, `n/a` rows, exit 0 | `TestLocateCLIReportsErrNoCLIOnlyWhenNothingIsFound`, extended `TestLocateCLIRespectsEnvAndPin` |
| CEN-03, CAP-07 | fixed by R1 | as R1 |
| COR-02 (3 cases), COR-05b | fixed by R3 (coverage + free-space checks) | as R3 |
| COR-03b | fixed by R4 | as R4 |
| CAP-02a, CAP-02b | fixed `462841e`: `Rebuild` runs the page census on the rebuilt temp file before the rename and refuses when the census fails (02a: in-header size beyond the file) or its manifest differs from the input (02b: `capture page 1: ... role bogus-role vs table-leaf`) | `TestRebuildRefusesACaptureTheCensusContradicts` |
| CAP-02c | fixed by R9.2 | as R9.2 |
| CAP-05 | fixed by R9.1 | as R9.1 |
| CAP-04 | **answered, no change.** Correction 5 (accepted by the review) defines "at rest" as "the database file alone is complete": a 0-byte `-wal` means every committed frame is in the file, whoever is connected. The outline's concern is "a stale file"; the capture is not stale. Evidence, `agent_work/ch02/repair-probe/cap04` (child holds an uncommitted write tx after `wal_checkpoint(TRUNCATE)`, `-wal` 0 bytes, `-shm` 32768): capture succeeds; rebuilt balance 43047 = balance before the child, `integrity_check ok`; if the child then rolls back, the capture is E1-equal to the file; if it commits, the file moves to a later state (43146), as it would after capturing a closed file. A commit during the capture grows the `-wal` and is refused by the final check (CAP-05). Detecting a live connection would need lock probes on `-shm`, which from a process holding a connection would itself break §4.1; a 0-byte `-wal` + `-shm` also remains after `PERSIST_WAL` closes, which correction 5 treats as at rest | probe only |

Post-repair adversarial run (`results.txt`): **33 HELD, 3 BROKEN** (HDR-03, HDR-06, CAP-04 — all
answered above; each attack's expectation is what is wrong, not the code). Attacks were not edited.

## Stale prose for Neon

Chapter `ch02-file-format-and-pages/ch02-file-format-and-pages.md`:

- **All 11 include blocks are stale** (`go run ./cmd/course inline -check` lists them): 2.1, 2.2,
  2.3 `main.go`; `header.go`, `btree.go`, `payload.go`, `census.go`, `capture.go`; evidence 2.1,
  2.2, 2.3. Re-inline with `go run ./cmd/course inline <chapter.md>` after commenting.
- Line 198 (2.1 Claim) and Platform implication 3 (line ~5450): "commits that dirty page 1 on one
  connection ... hold that same counter ... advancing once per new connection" / "repeated
  page-1 writes on one connection hold the counter constant". New fact: the counter is
  `dbFileVers + 1`, and `dbFileVers` is refreshed whenever page 1 is read into the cache; on one
  connection the counter stays constant only while page 1 stays cached (C1); after
  `PRAGMA shrink_memory` it advances every commit (C4: 30, 31, 32). A new connection is one way
  to re-read page 1, not the rule.
- §4.2 WAL timeline table: add the C4 rows (steps 28/29/30 → 30/31/32, `-wal` 82,432 / 156,592 /
  280,192 B, file unchanged).
- "Reading the evidence" for 2.1 (lines ~1003–1012): line numbers shifted (C2 has 2 new checks,
  C4 is new, 23 lines); Part A now prints `n/a` for `version-valid-for` and the check reads "each
  of the 18 decoded header fields that has an oracle here ... 1 field(s) have none (n/a)"; F's
  checks now say "is stored as N".
- Line 20 / setup: a missing `$SQLITE3_BIN` or a CLI with the wrong pin now FAILS 2.1 (exit 1);
  `NO-CLI:` is printed only when no CLI is found at all.
- Line 1477 (`header.go`) and the `WALMode` doc comment (deleted): `WALMode()` is now
  `ReadVersion == 2` (byte 19 alone, `btree.c:3341`).
- Line 1993 (`btree.go`): `ParseBTreePage` now also rejects freeblocks less than 4 bytes apart,
  content start + fragmented + freeblock bytes > usable size, two cells/freeblocks sharing a byte,
  and a fragmented count (byte 7) different from the bytes no cell or freeblock covers. The
  comment "Walk the freeblock list verifying ascending non-overlapping offsets..." (kept) is now
  incomplete: the gap rule is ≥ 4 bytes.
- `payload.go`: the comment "Only interior links must be non-zero; the last overflow page's next
  field ... is not followed" was deleted with the code; the last page's next pointer must be 0.
- Line 2808 / `census.go`: a `table` entry may have an index-b-tree root (WITHOUT ROWID); only an
  `index` entry with a table root is refused.
- Line 3464 / `capture.go` doc comments on `Pages` and `Rebuild`: `Pages` writes
  `pages.bin.capture-tmp` / `manifest.txt.capture-tmp`, removes any earlier capture files, and
  renames only after the final checks (a failed capture leaves neither file); `Rebuild` refuses a
  0-page manifest and runs the page census on the rebuilt file, refusing when it fails or its
  roles/pages differ from the manifest (`capture page N: the manifest disagrees with the rebuilt
  file's census: role X vs Y`).
- 2.2 "Reading the evidence" line 4623 (Part B): four checks now (row count + one per row from
  parsed cells).
- §6.1 table row F6 (line 5410) and the F6 paragraph (line 5418, "what the census does not
  check"): ours is now `tree "t" (root 2): ... page 2 offset 7: 1699 bytes of the cell content
  area belong to no cell or freeblock, the header reports 0 fragmented bytes` — the same
  fragmentation SQLite reports. The list "does not verify ... cell-to-cell byte overlap,
  fragmentation sums" is false now; still not verified: key order, pointer-map entry contents.
- 2.3 "Reading the evidence" / §6.2: each C check now names the substring it requires.
- 2.2 experiment doc comment and `census()` comment-free helper: `census` reads the file into
  memory; new uncommented code: `treeNamed`, Part B `split`, Part E's explicit open/close.
- New uncommented code: `checkCoverage`, `byteRange` (btree.go); `writeCapture`,
  `captureTmpSuffix`, `afterCaptureWritten` (test hook), `checkRebuilt` (capture.go);
  `sqlitex.ErrNoCLI`; 2.1 `C4` block; new tests in dbfile, capture, sqlitex.
- `internal/capture` was not in the comment list and still needs comments.

## Commands run

| command | result |
| --- | --- |
| `gofmt -l internal ch02-file-format-and-pages cmd agent_work/ch02` | empty |
| `go build ./... && go vet ./... && go test ./...` | pass (dbfile 14.6 s; flipped-bytes test now rejects 319 of 3000, was 262) |
| new tests against the pre-repair `btree/payload/census/header.go` | fail as expected (WITHOUT ROWID, 5 page cases, 2 of 3 overflow cases) |
| `bash agent_work/verify/crossbuild.sh` | `CROSSBUILD OK` |
| `go run ./cmd/course check all` | sources ch01/ch02 PASS, spans PASS, gofmt PASS, evidence 2 runs identical PASS for all six experiments; **FAIL inline -check ch02** (11 stale include blocks, caused by this repair; Neon re-inlines) and **FAIL inline -check ch01** (8 problems, pre-existing: same result at `04655bd`) |
| `go run ./cmd/course check paths` (not part of `check all`'s output above) | FAIL, 1 problem: `_tools/bin/kb` (a binary committed by `c51ff44` infra, pre-existing; nothing from this repair) |
| each ch02 experiment run twice | 2.1 exit 0 (4–7 s), 2.2 exit 0 (≈2.8 s), 2.3 exit 0 (4–5 s); non-INFO output identical; evidence files updated |
| 2.1 with `SQLITE3_BIN=/nonexistent`, with a wrong-pin fake CLI, with no CLI | exit 1, exit 1, exit 0 with `NO-CLI:` |
| `bash agent_work/ch02/adversarial/run-all.sh` | 33 HELD, 3 BROKEN (HDR-03, HDR-06, CAP-04: answered) |
| `go run ./agent_work/ch02/repair-probe/cap04` | output quoted under CAP-04 |
