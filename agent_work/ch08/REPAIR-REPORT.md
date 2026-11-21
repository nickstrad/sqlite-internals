# Chunk 8 repair round (Opus)

Inputs: `REVIEW-1.md` (VERDICT: FAIL, 5 should-fix + 5 nits), `adversarial/results.txt` (run 1:
20 HELD, 9 BROKEN). Commits: `b7e12b2` (connmgr), `3ff5e12` (procstat), `c2d56b5` (experiments
and evidence), plus the notes/report commit that holds this file.

## Findings and attacks

| finding / attack | outcome | test added |
| --- | --- | --- |
| Review 1 (should-fix): Unverified/VerifiedReopens counted before `Open`; reopen-check I/O error counted only as refused | fixed `b7e12b2`: `verifyBeforeOpen` returns `(verified, err)`; `Misses` and exactly one of Verified/Unverified move only after `Open` succeeds; a check I/O error counts `StatusFailures`, a changed file `RefusedReopens` | `TestPathErrorAndOpenError` now asserts Misses/Unverified/Verified = 0; new `TestFailedReopenCountsOnlyAsOpenFailure` |
| ATR-03 (BROKEN) | fixed `b7e12b2` (same fix) | same as above |
| Review 2 (should-fix): `observed[].placement` empty in 8.1 settings-json | fixed `c2d56b5`: `Placement: placement` filled; evidence regenerated | experiment check unchanged; JSON verified (12/12 observed placements name their setting) |
| OPN-05 (BROKEN) | fixed `c2d56b5` (same fix) | — |
| Review 3 (should-fix): `DescriptorsUnder`/`RelativeTargets` with relative dir or `/` | fixed `3ff5e12`: dir resolved with `filepath.Abs` + `EvalSymlinks`; no double separator for `/` | `TestDescriptorsUnderRelativeAndRootDirectories` (three relative spellings after `t.Chdir`, and `/`) |
| IDL-06b (BROKEN) | fixed `3ff5e12` (same fix) | same |
| IDL-06a (BROKEN): KiB × 1024 overflow in `ParseStatus` / `ParsePSRSS` | fixed `3ff5e12`: shared `kibToBytes` rejects values above `math.MaxInt64/1024` | 2^53 and 2^54 KiB cases added to `TestParseStatus` and `TestParsePSRSS` |
| Review 4 (should-fix): 8.1 discards `whole`; header read does not check `Result` or file name | fixed `c2d56b5`: first-statement check requires `whole`; open check requires `Result == 100` and `opens[0] == filepath.Base(path)` | experiment checks (output unchanged, both runs pass) |
| Review 5 (should-fix): build-notes claimed B2 is the `sqlite3_close_v2` zombie path | fixed in build-notes (Timeline, `main.c 1266–1486` entry): B2 is database/sql's pool deferral; the driver's `Close` → `sqlite3_close_v2` runs only after `rows.Close()`, on a connection with no open statements | — (notes only) |
| Review 6 (nit): eviction runs `CheckAtRest` after a failed `Close` | fixed `b7e12b2`: when `Close` fails, no file check; status not at rest with `close failed: …`; record dropped | not unit-tested: `sqlitex.DB.Close` cannot be made to fail without a fake driver; the branch is the same one exercised by the open-connection test |
| ATR-02 (BROKEN): rollback-mode eviction with rows open reported at rest, and `CheckAtRest`'s open/close dropped the live connection's POSIX lock | fixed `b7e12b2`: after `db.Close()`, if `db.Stats().OpenConnections > 0`, eviction skips `CheckAtRest` (so it never opens/closes the file and the lock survives) and reports `N connection(s) still open in this process …` | `TestEvictionWithRowsStillOpenSkipsTheFileCheck` (rollback mode: not at rest, no sha256, no record, the rows still read their next row, at rest after `rows.Close`) |
| ATR-01b (BROKEN): exclusive-mode WAL reader in another process makes no `-shm`, so `CheckAtRest` said at rest; the build-notes claim "every open WAL connection keeps the -shm" was false | fixed `b7e12b2`: when no side-file problem is found, `CheckAtRest` probes SQLite's lock bytes (0x40000000, 512 bytes) with `fcntl(F_GETLK)` on the descriptor it hashes; a conflicting lock adds "another process holds a SQLite lock on the database file: it has the database open". Notes corrected. | `TestCheckAtRestSeesALockHeldByAnotherProcess` (re-executes the test binary as a helper holding an exclusive-mode WAL connection; asserts no -shm and exactly the lock problem; at rest after the helper exits) |
| ATR-04 (BROKEN): `Acquire` with an already-cancelled ctx at capacity evicted the healthy idle entry first | fixed `b7e12b2`: after the hit check and before any eviction or open, `Acquire` returns `ctx.Err()` | `TestAcquireWithCancelledContextEvictsNothing` |
| Review 7 (nit): 8.1 claims header bytes 18–19 = 1 for the VACUUM INTO output without reading them | fixed `c2d56b5`: the experiment reads the VACUUM INTO file's header and checks bytes 18 and 19 are 1 | experiment check |
| Review 8 (nit): 8.2 fails at small `-n` | fixed `c2d56b5`: `-n` must be ≥ 20. Measured uncounted per connection: n=5 280 KiB (fails), n=10 212 KiB, n=20 186–194 KiB (3 runs). The fixed per-process part dominates at small n. | flag validation |
| Review 9 (nit): 8.3 INFO budget line rounds 256 KiB to `0.2 MiB` | fixed `c2d56b5`: the per-connection terms print in KiB (`64 x (2 x 256 KiB cache + 256 KiB) + 32.0 MiB = 80.0 MiB`) | INFO only |
| Review 10 (nit): 8.3 B4 checks only `-shm`; B5 indexes `Problems[0]` unguarded and never asserts the leftover -wal | fixed `c2d56b5`: B4 requires both `-wal holds` and `-shm exists`; B5 guards `len(Problems) > 0` and adds `check ok: B5: a -wal with content stays behind at the old name` | experiment checks |
| MGR-03a (BROKEN): `Capacity` above `RLIMIT_NOFILE`: when Open fails with SQLITE_CANTOPEN, the manager does not evict an idle entry and retry | answered, not changed. The same attack line shows the parts the outline cares about held: no descriptor leak on EMFILE (126 before and after), the open databases stay servable, and a manual `Evict` unblocks new opens. Automatic evict-and-retry would have to guess from SQLITE_CANTOPEN (the driver does not surface EMFILE) that the failure was descriptor exhaustion, and on any other CANTOPEN cause (missing directory, permissions) it would evict a healthy database for nothing. The outline's "K above the descriptor limit" is a configuration error, and the experiment catches it up front: 8.3 refuses to start when the soft limit is below `3*dbs + 64` (MGR-03b HELD). The capacity rule for Neon: K ≤ (soft limit − reserve) / 3 in WAL mode. | — |
| IDL-03a (BROKEN): 8.2's 256 KiB/connection uncounted margin exceeded with 1,000-table schemas (319–321 KiB) | answered, not changed: 8.2 states the margin for its own workload (3-table schema, 1 statement), and it holds there (both runs). The attack shows the uncounted part grows with schema size, which is a fact for the notes, not a defect. build-notes correction 4 now says the margin is workload-specific and quotes IDL-03a's numbers. | — |

All other run-1 attack lines were HELD. Run 2 (after the repair): 27 HELD, 2 BROKEN; the two BROKEN lines are MGR-03a and IDL-03a, both answered above (IDL-03a measured 318 KiB/conn on run 2). The attacks themselves were not edited, and none is wrong as an attack: IDL-03a and MGR-03a test expectations the outline does not set.

## Stale prose for Neon

No chapter or code comments exist yet for chunk 8, so nothing in a chapter or comment is stale.
Facts in `agent_work/ch08/build-notes.md` that changed (already corrected in that file):

- `internal/connmgr` Acquire: a cancelled/expired ctx fails before any eviction or open (new).
- Eviction: no file check when Close failed (`close failed: …`) or when database/sql still holds a
  connection (`N connection(s) still open in this process: …`); reason: the check's open/close
  would drop the live connection's POSIX locks (ATR-02).
- Stats: Misses and Verified/UnverifiedOpens are counted only after a successful Open, so
  `Misses == VerifiedReopens + UnverifiedOpens` always; a reopen-check I/O error counts
  `StatusFailures`.
- `CheckAtRest` has a fourth test, the F_GETLK lock probe on SQLite's lock bytes, run only when
  there is no side-file problem; new problem text "another process holds a SQLite lock on the
  database file: it has the database open".
- FALSE claim removed: "In WAL mode every open connection keeps the -shm". An exclusive-mode WAL
  connection has no -shm. What remains invisible: a connection in another process holding no lock
  (idle rollback-mode connection, or a connection that opened but has not run a statement).
- FALSE claim removed (Timeline, `main.c 1266–1486`): "B2 is this zombie path seen from Go". B2 is
  database/sql's pool deferral.
- 8.3 B2 output line changed to the "still open in this process" problem; B4 and B5 have stronger
  checks; B5 has one new `check ok` line; the INFO budget line prints KiB.
- 8.1: settings-json `observed[]` now carries each setting's `placement`; the VACUUM INTO header
  bytes 18–19 are read and checked by the experiment (no longer "seen in a probe"); the open and
  first-statement read checks are stricter (output unchanged).
- 8.2: `-n` must be ≥ 20; the 256 KiB margin is specific to 8.2's workload and grows with schema
  size (IDL-03a: ~320 KiB at 1,000 tables).
- `internal/procstat`: DescriptorsUnder/RelativeTargets resolve relative directories and `/`;
  parsers reject KiB values that overflow int64.

## Commands run

All from the worktree root with `SQLITE_SRC` set to the pinned 3.53.4 tree.

- `gofmt -l .`: nothing for chunk-8 files (it lists 5 older files under `agent_work/probes/`, not
  touched here).
- `go build ./... && go vet ./... && go test ./...`: PASS (all packages ok).
- `go test -race -count=2 ./internal/connmgr ./internal/procstat`: PASS.
- `bash agent_work/verify/crossbuild.sh`: `CROSSBUILD OK` (darwin/arm64 + linux/amd64 build and
  vet). darwin: cross-built only; the F_GETLK probe has not been run on darwin.
- Experiments, each run twice (`go run ./ch08-connection-and-database-lifecycle/experiments/<name>`):
  8.1 exit 0 ×2, non-INFO output identical; differs from the old evidence only in the
  settings-json line (placements filled) → evidence regenerated. 8.2 exit 0 ×2, non-INFO output
  identical to each other and to the existing evidence (unchanged). 8.3 exit 0 ×2, non-INFO
  identical; differs from the old evidence in the B2 eviction line and one new B5 check line →
  evidence regenerated.
- `go run ./ch08-…/experiments/8.2-idle-cost -n N`: N=5 fails the margin (now refused by the flag
  check, which requires ≥ 20); N=10 and N=20 (×3) pass.
- `go run ./cmd/course check all`: exit 1. Every chunk-8 check PASSES (sources: 3 files; spans:
  34; evidence: 8.1, 8.2, 8.3 each 2 runs identical to the evidence). The two failures predate
  this round and are outside chunk 8 (this round's diff touches neither): `inline -check` of
  `ch04-pager-and-page-cache.md` (4 blocks differ from ch04 code/evidence changed after the
  chapter's last inline), and `check paths` flagging the binary `_tools/bin/kb`, which is tracked
  on this branch (added in `c51ff44`, not tracked on main).
- `bash agent_work/ch08/adversarial/run-all.sh`: 27 HELD, 2 BROKEN (MGR-03a, IDL-03a: answered).
  Output in `agent_work/ch08/adversarial/results.txt` (run 1 stays in `results-run1.txt`).
