VERDICT: PASS

Re-review of chunk `ch02` after Opus repair round 1 (`04655bd..HEAD`, commits `4bedd95`, `462841e`, `cf3a151`, `7690293`, `5e430c5`). Every finding from `REVIEW-1.md` (1–10) and every formerly BROKEN adversarial attack (11 fixed in code, 3 answered where the attack's expectation encoded a stale/over-general claim or mutually contradictory oracles) was verified against the diff, unit tests, cross-build, and live experiment runs. No new defects were introduced.

## Finding and attack verification

| Finding / Attack | Fixed? | How confirmed |
| --- | --- | --- |
| R1 [blocker] `WITHOUT ROWID` rejected (`census.go:233`) | Yes | Diff: `census.go:233` now rejects only `p.Type.IsTable() && t.Type != "table"`. `go test -count=1 -v -run 'TestWithoutRowid\|TestRejectsIndex' ./internal/dbfile` PASS; adversarial `CEN-03` and `CAP-07` HELD in `results-run2.txt`. |
| R2 [blocker] `os.Open` descriptor held concurrently with SQLite connections | Yes | Diff + grep across `ch02-file-format-and-pages/` and `internal/dbfile/`: `2.2-page-census/main.go` `census()` uses `os.ReadFile` + `dbfile.FromBytes`; Part E explicitly closes `dbfile.Open` before `openSQLite` and `os.Remove`; `2.1/main.go:230` closes `dbfile.Open` before `r.Open`; `dbfile_test.go` `openParsed` uses `os.ReadFile` + `FromBytes` and `TestLockBytePageInLargeSparseFile` explicitly closes `f` before `sql.Open`. Zero overlapping descriptors remain. |
| R3 [should-fix] `ParseBTreePage` missing free-space and coverage checks (`btree.go:147-201`) | Yes | Diff: 4-byte minimum freeblock gap (`previousEnd > 0 && next < previousEnd+4`), total free space bound (`CellContentStart + FragmentedBytes + FreeblockBytes <= usableSize`), and `checkCoverage()` checking double-used bytes and exact fragmented byte sum. Verified `c.Size >= 4` and `offset + c.Size <= usableSize` invariants hold for all cell types. `TestRejectsPageHeaderCorruptionLikeIntegrityCheck` (5 cases cross-checked against `PRAGMA integrity_check`) PASS; `COR-02` and `COR-05b` HELD. |
| R4 [should-fix] Non-zero `Next` on last overflow page (`payload.go:67-69`) | Yes | Diff: rejects `i == count-1 && next != 0` with `corruptAt(from, 0, ...)`. `TestRejectsNonZeroNextOnLastOverflowPage` (self, page 2, 9999) PASS; `COR-03b` HELD. |
| R5 [should-fix] 2.2 Part B checked constants instead of parsed cells (`2.2/main.go:427-458`) | Yes | Diff: records each row's parsed `(PayloadSize, LocalSize, len(chain), lastFilled)` from `WalkTable` + `OverflowChain` and asserts `len(splits) == 3` and exact splits for rows 1, 2, 3. Confirmed via `go run ./cmd/course check evidence ch02`. |
| R6 [should-fix] 2.2 `parseAll` overwrote `err` in loop (`2.2/main.go:656-658`) | Yes | Diff: breaks on first `err != nil`. |
| R7.1 [should-fix] 2.1 Part A vacuous oracle rows (`2.1/main.go:284-309`) | Yes | Diff + live run with `SQLITE3_BIN=/nonexistent` and normal CLI: prints `n/a` when no oracle applies (`version-valid-for` with CLI; 11 fields without CLI), counts checked vs unchecked fields, and names the active oracle set. |
| R7.2 [should-fix] 2.1 Part C2 weak check (`2.1/main.go:403-425`) | Yes | Diff + live run: asserts each `UPDATE` affected 1 row and strictly grew `-wal`, and asserts post-checkpoint `equal.FileSHA256(path) != equal.FileSHA256(base)`. |
| R7.3 [should-fix] 2.1 Part F weak check (`2.1/main.go:583-587`) | Yes | Diff + live run: asserts `h.PageSize == size`, `fmt.Sprint(h.PageSize) == pragma`, and stored bytes 16–17 equal `wantStored` (`1` for 65536). |
| R8.1 [should-fix] 2.3 Part C tautological slice guard (`2.3/main.go:199`) | Yes | Diff: `if len(entries)*4096 < len(pageBytes)`. |
| R8.2 [should-fix] 2.3 Part C did not verify page-naming error (`2.3/main.go:176-180, 216`) | Yes | Diff + live run: every case specifies `want` substring (`page 100: missing`, `page 50: listed twice`, `page 121: out of order`, `capture page 200:`, `capture page 7:`) and checks `strings.Contains(err.Error(), c.want)`. |
| R8.3 [should-fix] 2.3 `queryString` swallowed errors and bypassed `url.Values` (`2.3/main.go:277-293`) | Yes | Diff: passes `url.Values{"mode": {"ro"}}` to `sqlitex.FileURI`, calls `fatal` on query error and on `db.Close()` error. |
| R9.1 [should-fix] `capture.Pages` left partial capture on failure (`capture.go:142-192`) | Yes | Diff: removes earlier `manifest.txt`/`pages.bin`, writes `*.capture-tmp`, runs `CheckAtRest` + size/mtime check via `writeCapture`, removes temps on failure, renames only on success. `TestFailedCaptureLeavesNoCapture` PASS; `CAP-05` HELD. |
| R9.2 [should-fix] `capture.Rebuild` cleanup/0-page/stat gaps (`capture.go:254-343`) | Yes | Diff: rejects 0-page manifest, propagates non-`ErrNotExist` sidecar `Stat` errors, runs `checkRebuilt` (census + `CompareManifests`) before rename, removes `tmp` on check or rename failure. `TestRebuildRefusesACaptureTheCensusContradicts` PASS; `CAP-02a`, `CAP-02b`, `CAP-02c` HELD. |
| R10 [nit] Test hygiene & clarity items | Yes | Diff: removed unused `crypto/sha256` in `dbfile_test.go`; checked ignored errors in `dbfile_test.go`, `equal_test.go`, `capture_test.go`; added `rows.Err()` checks; added `treeNamed(c, "documents")` in 2.2; switched 2.1 E to `errors.Is(err, dbfile.ErrCorrupt)`. |
| HDR-08 (`WALMode` on `write=1, read=2`) | Yes | Diff (`header.go:161`): `WALMode() bool { return h.ReadVersion == 2 }`, matching `lockBtree` (`src/btree.c:3341`, confirmed in pinned source). `TestWALModeFollowsReadVersion` PASS; `HDR-08` HELD. |
| HDR-10 (wrong-pin or missing `$SQLITE3_BIN` in 2.1) | Yes | Diff (`sqlitex/cli.go:114-117`, `2.1/main.go:599-609`): `sqlitex.ErrNoCLI` isolates "no CLI found"; wrong-pin or missing `$SQLITE3_BIN` fails 2.1 with exit 1. Confirmed via `TestLocateCLIReportsErrNoCLIOnlyWhenNothingIsFound`, `TestLocateCLIRespectsEnvAndPin`, `SQLITE3_BIN=/nonexistent` (exit 1), and `HDR-10` HELD. |
| CEN-03, CAP-07 (`WITHOUT ROWID` census & capture) | Yes | Fixed by R1; both HELD in `results-run2.txt`. |
| COR-02, COR-05b (page header & cell overlap corruption) | Yes | Fixed by R3; both HELD in `results-run2.txt`. |
| COR-03b (non-zero next on last overflow page) | Yes | Fixed by R4; HELD in `results-run2.txt`. |
| CAP-02a, CAP-02b, CAP-02c, CAP-05 (capture/rebuild integrity & cleanup) | Yes | Fixed by R9; all 4 HELD in `results-run2.txt`. |
| HDR-03 (WAL one-connection counter under `PRAGMA shrink_memory`) | Answered (claim corrected) | Repair added 2.1 Part C4 proving the true rule: WAL commit writes `dbFileVers + 1`, refreshed whenever page 1 is read into the page cache (`readDbPage`, `pager.c:3093-3110`). `PRAGMA shrink_memory` evicts page 1 and advances the counter on every commit (30, 31, 32). Attack's EXPECT asserts the old over-general rule; chapter and attack to be updated in Part 3. |
| HDR-06 (negative `user_version` / `application_id` vs `.dbinfo`) | Answered (oracle disagreement) | Verified in pinned `src/shell.c.in:5561-5562`: `.dbinfo` reads every 4-byte field with `get4byteInt` (`unsigned int`) and prints `%u` (`4294967295`), while `PRAGMA` returns signed `-1` (matching `dbfile`'s `int32`). No single decoder can match both strings; attack EXPECT to be updated in Part 3. |
| CAP-04 (capture with live connection + 0-byte `-wal` + `-shm`) | Answered (definition of "at rest") | Ran `go run ./agent_work/ch02/repair-probe/cap04`: rebuilt capture has `integrity_check ok` and matches the committed pre-tx balance (`43047`), excluding the uncommitted `+99` write; if the child rolls back, capture is E1-equal to the final file; a concurrent commit grows `-wal` and is refused (CAP-05). Consistent with Correction 5's definition of at rest (file alone is complete); chapter and attack to be aligned in Part 3. |

## New findings

None.

## Commands run

| Command | Result |
| --- | --- |
| `go build ./... && go vet ./... && go test ./...` | PASS |
| `bash agent_work/verify/crossbuild.sh` | `CROSSBUILD OK` (`linux/amd64`, `darwin/arm64`, `darwin/amd64`, `gofmt`) |
| `go test -count=1 -v ./internal/capture ./internal/equal ./internal/sqlitex` | PASS (all tests) |
| `go test -count=1 -v -run 'TestWithoutRowid\|TestRejectsIndex\|TestRejectsPageHeader\|TestRejectsNonZeroNext\|TestWALMode\|TestLockBytePage\|TestRowsMatch\|TestRejectsMalformed\|TestHeaderFields' ./internal/dbfile` | PASS (all 9 targeted suites) |
| `go run ./cmd/course check sources ch02 && go run ./cmd/course check spans ch02 && go run ./cmd/course check evidence ch02` | PASS (3 sources, 18 spans, 2.1/2.2/2.3 2 runs identical to evidence) |
| `SQLITE3_BIN=/nonexistent go run ./ch02-file-format-and-pages/experiments/2.1-header-and-what-a-commit-changes` | Exit 1 (`check FAILED: ... $SQLITE3_BIN=/nonexistent does not name a file`) |
| `go run ./agent_work/ch02/repair-probe/cap04` | Confirmed: rollback case E1-equal (`43047`, `integrity_check ok`); commit case rebuilt=`43047`, final=`43146` |
