# Probes: determinism, state capture and rebuild verification

Feasibility probes behind `docs/knowledge/determinism-and-rebuild.md`. All run against the
pinned SQLite 3.53.4 (`/usr/local/bin/sqlite3`, `/usr/local/lib/libsqlite3`). Scratch data goes
to `/tmp/sqlite-research-determinism-rebuild/` (delete it when done).

## One command

```bash
cd agent_work/probes/determinism-rebuild
GOPROXY=off ./run_all.sh          # outputs in /tmp/sqlite-research-determinism-rebuild/out/*.out
```

`run_all.sh` copies the pinned tree to `/tmp/sqlite-research-determinism-rebuild/src`, runs
`configure --enable-all` there and builds `sqlite3.c libsqlite3.a dbhash sqldiff sqlite3_rsync`
(about 3 minutes; it is the only way to get the session extension, which the installed library
lacks). Then it builds the Go helpers and runs every probe below.

## Individual probes

Environment for the Go helpers: `export PATH=/usr/local/go/bin:$PATH GOFLAGS=-buildvcs=false GOPROXY=off`.

| Probe | Run | Needs |
| --- | --- | --- |
| `seed.sql`, `phase2.sql` | fixed workloads (no nondeterministic functions) | |
| `hdr.py DB...` | print the variable header fields of each db | python3 |
| `pagediff.py A B [--ignore-header]` | page-by-page diff; `--ignore-header` masks bytes 24-27, 40-43, 92-99 of page 1 | python3 |
| `q1_build_determinism.sh` | Q1: same script twice under 7 configs; batching variants; header fields | |
| `q1b_freelist_secure_delete.sh` | Q1: batching diff is one stale freelist leaf; `secure_delete=ON` removes it | |
| `q1c_sql_nondeterminism.sh` | Q1: random/now/max-rowid/AUTOINCREMENT/ANALYZE/float/unordered-select; persistent vs per-connection pragmas | |
| `go/modernc_build` | Q1: `go run ./modernc_build ../seed.sql OUT.db` builds the same db through modernc.org/sqlite v1.58.0 | Go |
| `go/walreplay` | Q2/Q3c: `walreplay frames WAL`, `walreplay apply BASE WAL OUT` — WAL parser + replayer, no SQLite | Go |
| `q2_wal_determinism.sh` | Q2/Q3c: WAL bytes vs frame tuples across runs, sync settings, fixed PRNG seed, autocheckpoint; replay vs checkpoint | `$WALREPLAY` |
| `q3c_wal_tail.sh` | Q3c: uncommitted spilled frames, corrupted last frame (vs SQLite's recovery), page-1 fields | `$WALREPLAY` |
| `q3_capture_methods.sh` | Q3 a,b,d,e,g: cp, sqlite_dbpage, .backup, VACUUM INTO (canonicalization), .dump replay | |
| `q3_concurrent.sh` + `torn_copy.py` | Q3 under a writer: lost commits without -wal, torn copies, dbpage snapshot, .backup | python3 |
| `go/serialize` + `q3f_serialize.sh` | Q3f: sqlite3_serialize/deserialize vs file (cgo, installed lib) | `$SER` |
| `go/session` + `q3h_session.sh` | Q3h: session changeset recorded and applied (cgo, session-enabled `libsqlite3.a`) | `$SESSION`, `$BLD` |
| `q3i_rsync_recover.sh` | Q3i: sqlite3_rsync local→local, .recover on damaged db | `$BLD` |
| `q4_verification_ladder.sh` | Q4: sha256 / pagediff / dbhash / .sha3sum / integrity / fk on q3 artifacts; blind spots | `$BLD`, q3 run first |
| `q5_shm_peek.sh` | Q5: read mxFrame / nBackfill / read marks from `-shm` while a reader pins a snapshot | python3 |
| `q6_kill9.sh` | Q6: kill -9 a writer under synchronous FULL/OFF, rollback and WAL | python3 (links 3.53.4) |

Build commands for the Go helpers (from `go/`):

```bash
go build -o /tmp/sqlite-research-determinism-rebuild/walreplay ./walreplay
go build -o /tmp/sqlite-research-determinism-rebuild/serialize ./serialize
BLD=/tmp/sqlite-research-determinism-rebuild/bld
CGO_CFLAGS="-I$BLD" CGO_LDFLAGS="-L$BLD" go build -o /tmp/sqlite-research-determinism-rebuild/session ./session
```
