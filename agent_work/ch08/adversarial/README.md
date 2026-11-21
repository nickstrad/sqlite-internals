# Chapter 8 Adversarial Tests (`ch08-connection-and-database-lifecycle`)

Author: Gemini 4 Neon.  
Run all groups from the repository root:

```bash
bash agent_work/ch08/adversarial/run-all.sh
```

Output is written to `agent_work/ch08/adversarial/results.txt`. All scratch files are isolated under `_work/adv-ch08/`.

## Scope and Scaling Notes

- **Skipped outline bullets:** None in Chapter 8 mention a second SQLite version / newer driver release (every "Astra will try" bullet for 8.1, 8.2, and 8.3 is covered, plus pin-enforcement checks in `OPN-06` and targeted attacks on `internal/connmgr`, `internal/procstat`, and 8.1's JSON output).
- **Iteration scaling:** `IDL-03` opens `N = 50` connections with 1,000-table schemas (instead of 200) to keep RSS and runtime modest while keeping total schema memory > 12 MiB above GC noise; `MGR-01` runs 5 `SIGKILL` rounds over 60 databases with `K = 12` (300 recovered DB checks total); `MGR-02a` runs Experiment 8.3 with `-dbs 40 -k 1 -ops 200` and `MGR-02b` runs 8 concurrent goroutines × 20 ops over 12 databases. Total suite runtime is well under 1 minute on 4 cores / 7 GiB (far below the 10-minute target).
- **Portability:** Pure Go, no cgo. Linux-only `Setrlimit` helpers in `idle-cost-limits` and `manager-kills-and-limits` use `rlimit_linux.go` / `rlimit_other.go` build tags, and strace / `/proc/self/fd` checks guard on `trace.Supported` / `procstat.TargetsSupported`, so `GOOS=darwin go build ./...` compiles cleanly.

## Attack Catalog

### Group 1: `open-settings-edge` (`agent_work/ch08/adversarial/open-settings-edge/main.go`)

| ID | Claim attacked | Method | What a BROKEN result would mean |
| --- | --- | --- | --- |
| `OPN-01` | 8.1 missing file with and without the create flag (OUTLINE 8.1). | Open nonexistent paths via plain driver with `mode=rw`, `mode=ro`, and `mode=rwc`; inspect SQLite error code and file creation timing. | `mode=rw`/`mode=ro` failed to reject missing files at open with `SQLITE_CANTOPEN` (14) or created files; or `mode=rwc` failed to create the file at open and populate pages on first DDL. |
| `OPN-02` | 8.1 non-database file error arrives at first statement, not at open (OUTLINE 8.1). | Open random 4 KiB bytes, ASCII text, a 50-byte truncated file, and a 0-byte empty file via plain driver and via `sqlitex.OpenDefault`. | Plain open rejected a non-DB file early or failed to return `SQLITE_NOTADB` (26) on first statement, `sqlitex.Open` failed to fail at open, or a 0-byte file failed to open cleanly. |
| `OPN-03a` | 8.1 open/first-statement read contrast in rollback mode (`DELETE`). | Trace plain open + first `SELECT` on a `DELETE`-mode DB under `strace`. | Open read more than 100 bytes, or first statement read page 1 more than once or opened `-wal`/`-shm` in rollback mode. |
| `OPN-03b` | 8.1 open/first-statement read behavior on a dirty WAL (killed writer). | `SIGKILL` a writer child leaving committed frames in `-wal`, then `strace` a fresh open + first `SELECT`. | Open touched `-wal`/`-shm` instead of reading only the 100-byte header, or first statement did not read `-wal` to recover committed frames (scoping 8.1's zero-WAL-read check to at-rest files). |
| `OPN-04a` | 8.1 / `sqlitex.SettingPlacements`: none of the 5 rollback journal modes survive reopen; WAL → `delete` resets header bytes 18–19. | Set each of `delete`, `truncate`, `persist`, `memory`, `off`, close and reopen; switch a WAL DB back to `delete` and inspect header bytes 18–19. | Any rollback journal mode persisted across reopen or WAL-to-delete failed to reset header bytes 18–19 to `1,1`. |
| `OPN-04b` | `build-notes.md` #2 unmeasured source claim: negative `PRAGMA default_cache_size=-500` becomes positive `+500` pages on reopen (`prepare.c:325`). | Set `PRAGMA default_cache_size=-500`, create a table, close, reopen plain connection, read header bytes 48–51 and `PRAGMA cache_size`. | `sqlite3InitOne` did not apply `sqlite3AbsInt32` or did not seed `cache_size` to `500` pages. |
| `OPN-04c` | 8.1 claim: "`VACUUM INTO` keeps every file setting except `journal_mode`" tested against `default_cache_size=777` (declared `SettingInFile`). | Create a DB with `PRAGMA default_cache_size=777`, run `VACUUM INTO`, inspect header bytes 48–51 and reopened `cache_size` on the output. | `VACUUM INTO` dropped `default_cache_size` (resetting header bytes 48–51 to `0` and `cache_size` to `-2000`), falsifying the unqualified claim that `VACUUM INTO` preserves every file setting except `journal_mode`. |
| `OPN-05` | 8.1 machine-readable `settings-json` output integrity. | Run Experiment 8.1, parse `settings-json:`, verify all 14 `placements` and all 12 `observed[*].placement` structs match `sqlitex.SettingPlacementFor`. | Experiment 8.1 emits invalid JSON or leaves `observed[*].placement` unpopulated/zeroed (`8.1/main.go:515` omits `Placement: placement`). |
| `OPN-06` | OUTLINE §1 item 4 ("must be able to fail"): all 3 ch08 experiments enforce the pinned SQLite check-in. | Copy minimal repo tree to `_work/adv-ch08/open-settings-edge/scratch-repo`, mutate `sqlite/PIN`, run 8.1, 8.2, and 8.3. | Any ch08 experiment exited 0 or failed to print the pin mismatch on line 1. |

### Group 2: `idle-cost-limits` (`agent_work/ch08/adversarial/idle-cost-limits/main.go`)

| ID | Claim attacked | Method | What a BROKEN result would mean |
| --- | --- | --- | --- |
| `IDL-01` | 8.2 1,000 WAL databases against the process descriptor limit (OUTLINE 8.2). | Spawn a child with `RLIMIT_NOFILE=1024` attempting to open 1,000 WAL DBs and run 1 query on each. | Descriptor exhaustion did not occur near `1024/3` (~335–341 connections) with `SQLITE_CANTOPEN` (14). |
| `IDL-02` | 8.2 exact 3×N descriptors and 256 KiB/conn uncounted memory margin at `n = 1000` (OUTLINE 8.2). | Run Experiment 8.2 with `-n 1000` under default `RLIMIT_NOFILE`. | Descriptor count deviated from 3,000 or the uncounted memory bound failed at 1,000 connections. |
| `IDL-03a` | 8.2 memory margin (`growth >= sum` and `uncounted <= 256 KiB/conn`) under large schemas (OUTLINE 8.2). | Open 50 WAL connections each with a 1,000-table schema (~251 KiB `SCHEMA_USED`) and 5 prepared statements; measure counters vs RSS growth. | 8.2's 256 KiB/conn uncounted memory margin breaks under large schemas due to allocator/driver overhead scaling with schema size. |
| `IDL-03b` | 8.2 `PRAGMA shrink_memory` drops cache across all connections while preserving large `SCHEMA_USED` and `STMT_USED`. | Run `PRAGMA shrink_memory` on connection 0 of the 50 large-schema connections and re-read all counters. | `shrink_memory` failed to drop `CACHE_USED` below 1/8 across all 50 connections or altered `SCHEMA_USED`/`STMT_USED`. |
| `IDL-04` | 8.2 `PRAGMA shrink_memory` scope when pages are dirty/pinned in an active write transaction. | Dirty every `documents` page inside `BEGIN IMMEDIATE`, call `PRAGMA shrink_memory`, then `ROLLBACK` and call `PRAGMA shrink_memory` again. | `shrink_memory` unexpectedly discarded dirty/pinned pages, or failed to reclaim cache after `ROLLBACK`. |
| `IDL-05a` | 8.2 descriptor parking and reuse in WAL mode (OUTLINE 8.2). | With conn 1 open on a WAL file, run 10 sequential open/close cycles (testing `findReusableFd`), then open 5 simultaneous connections, close 4, then close the last. | Sequential open/close leaked parked FDs past 4, or closing 4 of 5 simultaneous connections failed to park all 4 DB descriptors (`11 -> 7 -> 0`). |
| `IDL-05b` | 8.2 descriptor parking in rollback mode (`DELETE`): lock-dependent behavior. | Test opening/closing conn 2 while conn 1 is idle (`1 -> 2 -> 1`) vs while conn 1 holds a SHARED lock inside `BEGIN; SELECT` (`1 -> 2 -> 2 -> 1 -> 0`). | Rollback-mode descriptor parking did not track whether another connection in the process holds a POSIX lock (`pInode->nLock`). |
| `IDL-06a` | `internal/procstat` parser input validation (`ParseStatus`, `ParsePSRSS`). | Pass `9007199254740992` (2^53 KiB, fits in `int64` but overflows `kib * 1024` to negative) to both parsers. | `parseKiB` / `ParsePSRSS` silently overflow `int64` and return a negative RSS with `err == nil`. |
| `IDL-06b` | `internal/procstat.DescriptorsUnder` path handling (`"/"` and relative paths). | Call `DescriptorsUnder("/")` and `DescriptorsUnder(relativePath)` while a file inside `relativePath` is open. | `DescriptorsUnder` fails on `"/"` (`filepath.Clean("/") + "/"` produces `"//"`) or fails to resolve relative directories against absolute `/proc/self/fd` symlink targets. |

### Group 3: `manager-kills-and-limits` (`agent_work/ch08/adversarial/manager-kills-and-limits/main.go`)

| ID | Claim attacked | Method | What a BROKEN result would mean |
| --- | --- | --- | --- |
| `MGR-01` | 8.3 crash recovery: kill the bounded manager mid-stream and reopen everything (OUTLINE 8.3). | Run 5 rounds of a child bounded `connmgr` (60 DBs, `K=12`) logging acknowledged writes, send `SIGKILL` at 5 different progress points, inspect `CheckAtRest`, then reopen all 60 DBs in a fresh manager and verify `integrity_check`, recipe step, and final at-rest state. | Any DB failed `integrity_check`, lost an acknowledged write, landed outside a valid recipe state, or remained not-at-rest after recovery close. |
| `MGR-02a` | 8.3 bounded manager at `K = 1` (OUTLINE 8.3). | Run Experiment 8.3 with `-dbs 40 -k 1 -ops 200`. | Manager exceeded 1 open connection / 3 descriptors or failed any check at `K = 1`. |
| `MGR-02b` | `internal/connmgr` concurrency and `OnEvict` re-entrancy at `Capacity = 1`. | Drive 8 concurrent goroutines × 20 ops over 12 recipe DBs with `Capacity=1` and an `OnEvict` hook calling `Stats()` and `Record()`. | Concurrency at `K = 1` deadlocked, exceeded `MaxOpen=1`, failed to wait, or corrupted a DB. |
| `MGR-03a` | 8.3 / `internal/connmgr` when `K` exceeds `RLIMIT_NOFILE` (OUTLINE 8.3). | In a child with `RLIMIT_NOFILE=128`, run `connmgr` with `Capacity=200` over 80 WAL DBs (all open entries idle); check fd leaks on `EMFILE`, service of open DBs, automatic eviction vs wedging on new DBs, and manual `Evict`. | Setting `K > RLIMIT_NOFILE` leaks descriptors or wedges `Acquire` permanently on new DBs because `len(open) < Capacity` prevents evicting idle entries after `EMFILE`. |
| `MGR-03b` | 8.3 experiment preflight guard when `RLIMIT_NOFILE` is below `3*dbs + 64`. | Run Experiment 8.3 (`-dbs 50 -k 45`) in a child with `RLIMIT_NOFILE=128`. | 8.3 failed to detect an insufficient descriptor limit up front. |

### Group 4: `atrest-and-connmgr-edge` (`agent_work/ch08/adversarial/atrest-and-connmgr-edge/main.go`)

| ID | Claim attacked | Method | What a BROKEN result would mean |
| --- | --- | --- | --- |
| `ATR-01a` | 8.3 open-reader detection matrix (OUTLINE 8.3). | Evict WAL DBs while another process holds: (1) active reader + WAL writes, (2) active reader + 0-byte WAL, (3) opened-only connection with no statement yet. | Manager failed to flag (1) or (2) as not-at-rest (or failed to drop the record), or (3) failed to checkpoint when no lock was held. |
| `ATR-01b` | `build-notes.md` claim: "In WAL mode every open connection keeps the `-shm`, so the `-shm` rule catches open connections." | Open a WAL DB in another process with `PRAGMA locking_mode=EXCLUSIVE` + `SELECT` (no `-shm` created) and call `connmgr.CheckAtRest`; contrast with rollback-mode reader. | `CheckAtRest` returns `AtRest=true` on an open WAL connection using `locking_mode=EXCLUSIVE`, exposing a WAL-mode blind spot in the `-shm` rule. |
| `ATR-02` | `internal/connmgr` eviction safety with an unclosed `*sql.Rows` in rollback mode (same-process B2 variant). | Leave `*sql.Rows` open on a rollback-mode DB, release handle, call `m.Evict`, and test whether a second process can acquire a write lock before vs after `Evict`. | `Evict` falsely reports `AtRest=true` while the connection is open AND `CheckAtRest`'s `fileSHA256` (`os.Open` + `Close`) silently strips the open cursor's POSIX SHARED lock in the same process. |
| `ATR-03` | `internal/connmgr` stats accounting invariant (`UnverifiedOpens + VerifiedReopens == Misses`, checked by 8.3) under `Open` failures. | Trigger `Open` failures on an unrecorded junk file and on a recorded DB reopen; inspect `Stats()`. | `verifyBeforeOpen` increments `UnverifiedOpens` / `VerifiedReopens` before `Open` succeeds, breaking `UnverifiedOpens + VerifiedReopens == Misses` on any failed open. |
| `ATR-04` | `internal/connmgr.Acquire` behavior with a pre-cancelled context when at capacity. | Fill `Capacity=1` with healthy idle DB `"a"`, then call `Acquire(cancelledCtx, "b")`. | `Acquire` evicts healthy DB `"a"` before noticing `ctx` is already cancelled. |
| `ATR-05` | `connmgr.CheckAtRest` filesystem edge cases. | Test directory as DB path, 32-byte header-only `-wal`, non-empty `-wal` without `-shm`, and 0-byte / cold / hot `-journal`. | `CheckAtRest` misclassified a directory, header-only WAL, orphaned WAL without `-shm`, or cold/hot rollback journal. |
