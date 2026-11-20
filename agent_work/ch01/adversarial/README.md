# Chapter 1 Adversarial Tests (`ch01-architecture-and-execution-path`)

Author: Gemini 4 Neon.  
Run all groups from the repository root:

```bash
bash agent_work/ch01/adversarial/run-all.sh
```

Output is written to `agent_work/ch01/adversarial/results.txt`. All scratch files are isolated under `_work/adv-ch01/`.

## Scope and Scaling Notes

- **Skipped outline bullet:** Any bullet involving a second SQLite version or newer driver release (e.g., building the recipe with SQLite 3.54.1) was skipped per Nick's direction. Instead, pin-enforcement failure paths are exercised by mutating `sqlite/PIN` in a scratch copy under `_work/adv-ch01/` and using `SQLITE3_BIN` wrapper scripts.
- **Iteration scaling:** None needed. `DET-01` runs all 50 full 30-step WAL builds (parallelized across 4 workers with background CPU and disk I/O load), keeping the full suite well under the 10-minute target on 4 cores / 7 GiB.

## Attack Catalog

### Group 1: `pin-falsify` (`agent_work/ch01/adversarial/pin-falsify/main.go`)

| ID | Claim attacked | Method | What a BROKEN result would mean |
| --- | --- | --- | --- |
| `PIN-01` | 1.1 refuses to run on a mismatched build and prints `sqlite_source_id:` on line 1 (OUTLINE §1, 1.1). | Copy minimal repo tree to `_work/adv-ch01/pin-falsify/scratch-repo`, mutate `fossil_id`/`source_id` in `sqlite/PIN`, run `1.1-pin-check`. | Experiment 1.1 failed to exit non-zero or failed to report the pin mismatch on line 1. |
| `PIN-02` | 1.2 refuses to run on a mismatched build (OUTLINE §1, 1.1). | Run `1.2-one-insert` inside the mutated-`PIN` scratch repo. | Experiment 1.2 bypassed pin verification or exited 0 on the wrong build. |
| `PIN-03` | 1.3 refuses to run on a mismatched build (OUTLINE §1, 1.1). | Run `1.3-recipe-rebuild` inside the mutated-`PIN` scratch repo. | Experiment 1.3 bypassed pin verification or exited 0 on the wrong build. |
| `PIN-04` | 1.1 checks the linked Go driver version against `sqlite/PIN` (1.1). | Keep `source_id` intact in scratch `sqlite/PIN` but set `driver_version=v1.99.9`; run `1.1-pin-check`. | `debug.ReadBuildInfo()` driver-version check in 1.1 is dead or fails silently. |
| `PIN-05` | 1.1 and 1.2 reject a `sqlite3` CLI with a wrong `sqlite_source_id()` (OUTLINE §1, 1.1, 1.2). | Point `SQLITE3_BIN` to a wrapper returning a bogus `sqlite_source_id()` and run 1.1 and 1.2. | `sqlitex.LocateCLI` or the experiments accepted a wrong-version CLI or skipped CLI verification without failing. |
| `PIN-06` | 1.1 verifies required CLI compile options (`ENABLE_DBPAGE_VTAB`, `ENABLE_DBSTAT_VTAB`, `DEFAULT_WAL_SYNCHRONOUS=2`). | Wrap the real CLI via `SQLITE3_BIN` to strip `ENABLE_DBPAGE_VTAB` from `PRAGMA compile_options`; run 1.1. | 1.1 does not actually enforce required compile options on the CLI. |
| `PIN-07` | 1.1 detects file-format/I/O compile-option drift between Go driver and CLI. | Wrap the real CLI via `SQLITE3_BIN` to rewrite `DEFAULT_PAGE_SIZE=4096` to `8192`; run 1.1. | 1.1's side-by-side option comparison failed to catch a divergent I/O option. |
| `PIN-08` | `modernc.org/sqlite` keeps upstream runtime defaults without `sqlitex` PRAGMAs (OUTLINE §4.1). | Open a WAL DB via raw `sql.Open("sqlite", path)` and query `synchronous`, `busy_timeout`, `foreign_keys`, `page_size`. | The chosen driver silently overrides upstream defaults (e.g. setting `synchronous=NORMAL` or a busy timeout). |

### Group 2: `twin-io` (`agent_work/ch01/adversarial/twin-io/main.go`)

| ID | Claim attacked | Method | What a BROKEN result would mean |
| --- | --- | --- | --- |
| `TWIN-01` | WAL steady-state commit writes only to `-wal` (0 writes to `.db`), syncs once at `FULL`, and Go == CLI (1.2, build-notes #6). | Pre-create WAL with commit 1, then trace commit 2 under strace in Go and CLI; compare writes, syncs, non-`F_SETFD` syscalls, and file sha256. | Steady-state WAL commit touched `t.db`, wrote unexpected WAL offsets, issued wrong sync count, or diverged between Go and CLI. |
| `TWIN-02` | WAL steady-state commit at `synchronous=NORMAL` performs 0 syncs and 0 `.db` writes, Go == CLI (OUTLINE intro & 1.2). | Same as `TWIN-01` with `synchronous=NORMAL`. | WAL `NORMAL` commit synced or touched `t.db`, or Go and CLI diverged. |
| `TWIN-03` | Multi-page overflow `INSERT` in rollback mode writes page-aligned `(page-1)*4096` chunks and matches Go == CLI (1.2). | Insert a deterministic 10 KB BLOB in `delete` mode; verify `pwrite64` offsets against changed pages and compare Go vs CLI. | Multi-page/overflow writes broke page-offset correspondence or diverged between Go and CLI. |
| `TWIN-04` | Multi-page overflow `INSERT` in WAL mode writes 0 pages to `.db`, appends multiple frames to `-wal`, Go == CLI (1.2). | Insert a deterministic 10 KB BLOB in `wal` mode; compare Go vs CLI under strace and closed DB hashes. | WAL overflow commit touched `t.db` or diverged between Go and CLI. |
| `TWIN-05` | Twin I/O parity holds on `DELETE` freeing an overflow chain (`secure_delete=ON`, rollback mode) (1.2). | Delete a 10 KB BLOB row under strace in Go and CLI; compare writes, syncs, non-`F_SETFD` syscalls, and sha256. | Freeing/zeroing overflow pages causes I/O or file-byte divergence between modernc and C SQLite. |
| `TWIN-06` | Twin I/O parity holds on explicit `ROLLBACK` of a multi-page write transaction (rollback mode) (1.2). | Run `BEGIN IMMEDIATE; INSERT (10KB); ROLLBACK;` under strace in Go and CLI. | Rollback journal playback diverges between modernc and C SQLite. |
| `TWIN-07` | Twin I/O parity holds on `CREATE INDEX` in WAL mode (1.2). | Create an index on a 100-row table under strace in Go and CLI. | DDL/index-build I/O or resulting bytes diverge between modernc and C SQLite. |
| `TWIN-08` | Twin I/O parity holds on `PRAGMA wal_checkpoint(TRUNCATE)` (1.2). | Write a WAL frame with `wal_autocheckpoint=0`, then trace `PRAGMA wal_checkpoint(TRUNCATE)` in Go and CLI. | Checkpoint/truncate syscalls or final DB bytes diverge between modernc and C SQLite. |
| `TWIN-09` | Twin I/O parity holds on `VACUUM` of a fragmented table (1.2). | Delete half of a 100-row table, then trace `VACUUM` in Go and CLI; compare DB-dir syscalls and final sha256. | `VACUUM` writes/syncs or repacked file bytes diverge between modernc and C SQLite. |
| `TWIN-10` | Universal twin parity claim ("Go driver and C build do the same I/O" / byte-identical files) tested on `ANALYZE` (1.2, §4.1). | Trace `ANALYZE` on an indexed 100-row table in Go (`ENABLE_STAT4`) vs C CLI (no `STAT4`). | `BROKEN` proves the twin parity claim is NOT universal: `ANALYZE` writes `sqlite_stat4` only in Go, causing different writes, syncs, and DB files even at the exact pinned check-in. |
| `TWIN-11` | Twin I/O parity tested with `PRAGMA mmap_size=268435456` + `SELECT` + `INSERT` (1.2, §4.1). | Enable `mmap_size`, scan table, and insert a row under strace in Go and CLI. | `BROKEN` shows modernc's translated VFS handles `mmap` differently from C `os_unix.c` (e.g. extra/missing `mmap` or `pread64` calls). |
| `TWIN-12` | Must-be-able-to-fail check on twin comparison (1.2). | Run Go at `synchronous=FULL` vs CLI at `synchronous=NORMAL` on rollback `INSERT`. | Twin comparison failed to detect an injected sync-count divergence. |

### Group 3: `recipe-determinism` (`agent_work/ch01/adversarial/recipe-determinism/main.go`)

| ID | Claim attacked | Method | What a BROKEN result would mean |
| --- | --- | --- | --- |
| `DET-01` | Default 30-step WAL recipe is reproducible across working dirs, filenames, load, and 50 runs (1.3). | Run 50 full 30-step builds (2 sequential with `os.Chdir` + relative paths, 48 concurrent across spaces/deep paths/unicode/symlinks) under CPU+IO stress; check every step 0..30. | Nondeterminism leaks into the canonical recipe from paths, concurrency, or system load. |
| `DET-02` | `cache_size` does not affect at-rest recipe bytes (1.3, build-notes #7). | Build full 30-step recipe with `cache_size` in `{1, 2, 5, 10, 50, 0, -10, -100, -2000, -64000}` in WAL and DELETE modes. | Cache spilling (`pagerStress`) or cache sizing changes page allocation or final bytes. |
| `DET-03a` | Per-connection pragmas (`synchronous`, `wal_autocheckpoint`, `mmap_size`, `temp_store`, `threads`) do not alter at-rest recipe bytes when `secure_delete=ON` (build-notes #7). | Build full 30-step WAL recipe across 11 pragma variations and compare exact sha256. | One of those runtime pragmas leaks into the on-disk database bytes. |
| `DET-03b` | `PRAGMA locking_mode=EXCLUSIVE` per session in WAL mode closes cleanly at rest and matches canonical sha256. | Build full 30-step WAL recipe setting `locking_mode=EXCLUSIVE` on each session. | Exclusive locking alters WAL teardown/checkpointing or leaves side files / different bytes. |
| `DET-03c` | `journal_mode=truncate` and `persist` produce identical `.db` bytes to `journal_mode=delete`. | Build full 30-step recipe in `truncate` and `persist` modes and compare `.db` sha256 against `delete`. | Rollback journal cleanup mode alters `.db` file bytes. |
| `DET-04` | Same recipe built by pinned C `sqlite3` CLI matches Go driver at every step 0..30 (OUTLINE §3, §4.1, 1.3). | Export all 31 steps of `recipe.Default()` as literal SQL (hex BLOBs) and execute session-by-session via the C CLI in both WAL and DELETE modes. | Go driver and C CLI diverge on the course's own R0 workload. |
| `DET-05a` | 1.3 Finding 2: WAL + 1 connection + `secure_delete=ON` gives identical bytes across different transaction batches (build-notes #2). | Merge all 31 steps (0..30) into 1 transaction vs 31 transactions on 1 WAL connection (`secure_delete=ON`). | Finding 2 only held for the small 6-step pair in 1.3 and fails over the full 30-step workload. |
| `DET-05b` | 1.3 Finding 2 tested at the opposite extreme: autocommit per statement (~1,700 tx) vs 31 tx. | Execute every statement of steps 0..30 as autocommit on 1 WAL connection (`secure_delete=ON`) vs 31 transactions. | Statement-level commit boundaries change bytes even in WAL mode with `secure_delete=ON`. |
| `DET-05c` | 1.3 Finding 2 under heavy mid-recipe checkpointing (`wal_autocheckpoint=1` + autocommit). | Repeat `DET-05b` with `wal_autocheckpoint=1`. | Frequent mid-workload WAL checkpoints alter final at-rest bytes even with `secure_delete=ON`. |
| `DET-05d` | `PRAGMA secure_delete=FAST` tested as a weaker substitute for `ON` across batchings. | Compare 1-tx vs 31-tx builds on 1 WAL connection with `secure_delete=FAST`. | `BROKEN` demonstrates that `secure_delete=FAST` does NOT protect determinism across batchings (stale bytes remain). |
| `DET-06` | Build-notes #1 / 1.3 Finding 6: "in WAL mode the file change counter counts connection sessions, not commits." | Compare 5 WAL sessions vs 1 WAL connection each inserting 1 row on page 2 without touching page 1 (`user_version`, schema, or page count). | `BROKEN` refines the claim: in WAL mode the change counter only advances on sessions that dirty page 1, not on every writing session. |
| `DET-07a` | Concurrent reader pinning WAL checkpoints (`secure_delete=ON`) does not change final at-rest bytes. | Hold a read transaction on a 2nd connection across steps 5..25 while building steps 0..30 (`autockpt=1`, `secure_delete=ON`). | Deferred checkpointing under a concurrent reader alters at-rest bytes. |
| `DET-07b` | Same reader-pinned WAL build with `secure_delete=OFF`. | Repeat `DET-07a` with `secure_delete=OFF`. | `BROKEN` shows delayed checkpoints under `secure_delete=OFF` change final bytes. |
| `DET-08a` | Missed source candidate: `PRAGMA secure_delete=FAST` vs `ON` on canonical 30-step recipe. | Build canonical 30-step WAL recipe with `secure_delete=FAST` and compare to `ON`. | `BROKEN` shows `FAST` leaves unzeroed bytes that differ from `ON`. |
| `DET-08b` | Missed source candidate: aborted `SAVEPOINT` + `ROLLBACK TO` inside a committed transaction (`secure_delete=ON`). | Run identical committed inserts, but run 2 inserts 10×12 KB rows inside a savepoint and rolls back to it before committing. | `BROKEN` shows an unlisted determinism breaker: rolled-back savepoints leave extra allocated freelist pages even with `secure_delete=ON`. |
| `DET-08c` | Missed source candidate: `ANALYZE` on step-10 recipe DB (Go twice, and Go vs C CLI). | Run `ANALYZE` twice in Go (`ENABLE_STAT4`) and once in C CLI (no `STAT4`) on identical step-10 DBs. | `BROKEN` shows `ANALYZE` breaks cross-build reproducibility (and possibly intra-build reproducibility if `STAT4` sampling varies). |
| `DET-08d` | Missed source candidate: `sqlitex.DefaultOptions()` leaves `SingleConnection=false`, allowing `database/sql` pool churn in WAL mode. | Build steps 0..10 using `sqlitex.DefaultOptions()` with vs without a concurrent open `Rows` cursor forcing a 2nd pooled connection. | `BROKEN` shows that omitting `SingleConnection=true` causes non-deterministic WAL change counters under concurrent reads. |
| `DET-08e` | Missed source candidate: `TZ` environment variable with SQL `'localtime'` modifier on a fixed timestamp literal. | Insert `datetime('2026-01-01 12:00:00', 'localtime')` under `TZ=UTC` vs `TZ=America/New_York`. | `BROKEN` documents an environment-dependent SQL variation source not listed in OUTLINE §4.2. |

### Group 4: `recipe-api-abuse` (`agent_work/ch01/adversarial/recipe-api-abuse/main.go`)

| ID | Claim attacked | Method | What a BROKEN result would mean |
| --- | --- | --- | --- |
| `API-01` | `recipe.MatchStep` detects 1-bit tampering outside the change counter (1.3, `internal/recipe`). | Flip 1 bit in header byte 68 (`user_version`) and 1 bit in page 10 of a step-10 DB; call `MatchStep`. | `MatchStep` falsely matched a tampered database file. |
| `API-02` | `recipe.MatchStep` / `MaskChangeCounter` handling of corrupted, disagreeing `change_counter` (24–27) vs `version_valid_for` (92–95). | Overwrite bytes 24–27 with `0xDEADBEEF` and bytes 92–95 with `0x00000001`; call `MatchStep`. | `BROKEN` exposes that `MaskChangeCounter` blindly zeros both spans without checking they agree, so a DB with inconsistent header counters still reports a masked match. |
| `API-03` | `recipe.CheckAtRest` and `MatchStep` refuse non-at-rest databases (`-wal`, `-shm`, `-journal`). | Call both on an open WAL DB with uncommitted checkpoint and on a DB with an orphan `-journal` file. | `recipe` silently hashes a live or incomplete database file. |
| `API-04a` | `recipe.StepHashes` recovers from corrupt, out-of-order, or truncated cache files (`internal/recipe`). | Write malformed cache files under isolated `COURSE_WORK_DIR` and call `StepHashes`. | Cache corruption breaks `StepHashes` instead of triggering a clean rebuild. |
| `API-04b` | `recipe.StepHashes` behavior on a syntactically valid but poisoned cache (all-zero SHA-256 hashes). | Write a 31-line well-formed cache of `0000...` hashes and call `StepHashes`. | `BROKEN` documents that `recipe-cache` trusts syntax alone and has no integrity check against a poisoned cache file. |
| `API-05` | Failed `recipe.ApplyStep` rolls back atomically leaving `CurrentStep` and at-rest bytes unchanged (`internal/recipe`). | Re-apply step 1 on a step-1 DB (PK conflict) in WAL and DELETE modes; check error, `CurrentStep`, and file sha256. | A failed step mutates `user_version` or file bytes (e.g. change counter) despite rollback. |
| `API-06` | `sqlitex.Open` rejects existing DBs with mismatched persistent file settings (`page_size`, `auto_vacuum`, `application_id`). | Open a recipe DB requesting `page_size=8192`, `auto_vacuum=FULL`, and wrong `application_id`. | `sqlitex.Open` silently ignores persistent file-setting mismatches. |
| `API-07` | `recipe.CurrentStep` distinguishes empty (-1) vs partial schema (error), and `ApplyNext` refuses past max step. | Test empty DB, DB with 2 of 3 tables, and `ApplyNext` at final step. | `CurrentStep` or `ApplyNext` misclassifies partial schemas or overruns the recipe. |
| `API-08` | `Recipe.Validate` rejects invalid bounds/modes and `Step(k)` is independent of `r.Steps`. | Pass negative/huge steps and `memory` mode to `Validate`; compare `Step(8)` from 8-step vs 30-step recipes. | Invalid recipes pass validation or step generation depends on total step count. |
| `API-09` | `Recipe.Step(0)` and `Recipe.Build(..., 0)` enforce `Recipe.Validate()` when `Steps > MaxSteps`. | Call `Step(0)` and `Build(..., 0)` on `Recipe{Seed: 1, Steps: MaxSteps+1, JournalMode: "wal"}`. | `BROKEN` shows `Step` and `BuildEach` bypass `r.Validate()` by replacing `r.Steps` via `WithSteps(k)` before calling `Plan()`. |
