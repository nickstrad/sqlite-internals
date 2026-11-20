# Probe result: custom VFS on modernc.org/sqlite v1.60.1, no cgo (chunk 0, 2026-11-20)

Plain data for the orchestrator. Module: `agent_work/probes/vfs-viability/go.mod` (nested
module; requires only `modernc.org/sqlite v1.60.1` and `modernc.org/libc v1.77.1`, the root
module's pins). Runs: linux/amd64 on this VM and inside the Debian lab container; darwin/arm64
and darwin/amd64: cross-built and cross-vetted only (not run).

| Command (from `agent_work/probes/vfs-viability/`) | Output |
| --- | --- |
| `go run ./readonly` | `run-readonly.txt` |
| `go run ./writable` (io_methods version 3) | `run-writable.txt` |
| `go run ./writable v1` (io_methods version 1) | printed only, see table (b) |
| container: `docker compose run --rm lab sh -c 'cd agent_work/probes/vfs-viability && go run ./writable'` | tail in `../seaweedfs-s3/run-lab-container.txt`; identical results |

Two consecutive host runs of `./writable` gave identical output.

## (a) `modernc.org/sqlite/vfs` — read-only Go `fs.FS`

API: `name, handle, err := vfs.New(fsys)`, then DSN `file:<name-inside-fsys>?vfs=<name>`,
`handle.Close()` after every database on it is closed. The package registers io_methods
version 1 (no xShm*, no xWrite). Reads arrive as `Seek(ofst, SeekStart)` + `io.ReadFull`, so the
`fs.File` must implement `io.Seeker`; size comes from `Stat()`.

| Case | Result |
| --- | --- |
| Logging wrapper over `os.DirFS`, rollback-mode db (62 pages, 500 rows, index), `SELECT v FROM t WHERE id=377` | correct row; 10 fs operations: Open db, Read 100 B @0, Open `-journal` (absent), Open `-wal` (absent), Read 4096 @0, Open `-journal`, Read 16 @24, Open `-wal`, Read 4096 @4096, Read 4096 @188416 |
| Same, `SELECT count(*) FROM t` | 500; 37 fs operations |
| `testing/fstest.MapFS` holding the file bytes (in-memory) | `integrity_check` = ok |
| HTTP range reader (`fs.FS` whose Open = HEAD, Read = `GET Range: bytes=a-b`, served by `http.ServeContent`) | point lookup correct with 10 HTTP requests; full count 500 with 37 requests. An S3 GET with a Range header is the same request shape (not run against SeaweedFS here) |
| WAL-format file (header bytes 18,19 = 2,2), fully checkpointed, no `-wal` | plain DSN: `unable to open database file (14)`; `mode=ro`: same error; `immutable=1`: works (500 rows); `mode=ro&immutable=1`: works |
| Same bytes with header bytes 18,19 rewritten to 1,1 | works without `immutable` |
| WAL db with 1 committed row only in `-wal` (writer still open), `immutable=1` | returns 50, not 51: `immutable=1` ignores the WAL; un-checkpointed commits are invisible |
| Any write (`INSERT`) through the fs VFS | fails with `out of memory (7)` — vfsOpen returns SQLITE_NOMEM for the journal; misleading code |

Limits: read-only; no WAL reading (needs `immutable=1` or a rewritten header, and only sees
checkpointed content); every page miss is one `Seek`+`Read` (no read-ahead), so a remote
backend pays one request per page miss plus one per Open/existence check. Code: our fs.FS
wrappers are plain Go (logging FS ~35 lines, HTTP FS ~70 lines), no unsafe.

## (b) Writable Go VFS registered with the translated library

Yes, without forking modernc. How (`shim/shim.go`, 320 lines including ~45 lines of fault
injection):

- `modernc.org/sqlite/lib` exports the C struct types `Tsqlite3_vfs`, `Tsqlite3_io_methods`,
  `Tsqlite3_file` and the functions `Xsqlite3_vfs_find`, `Xsqlite3_vfs_register`.
- A C function pointer in the translated code is the data word of a Go func value
  (ccgo's `__ccgo_fp`); call sites cast the `uintptr` back to a Go func type. So a top-level Go
  func with the exact translated signature (`func(*libc.TLS, uintptr, ...) int32`) stored as
  `(*[2]uintptr)(unsafe.Pointer(&anyFunc))[1]` in a struct field is callable by SQLite, and a
  real method is called by reinterpreting `&field` as `*func(...)`.
- The shim copies the default (`unix`) `Tsqlite3_vfs` into a Go global, sets its own name,
  `szOsFile = header + real szOsFile`, and overrides xOpen/xDelete/xAccess. xOpen calls the real
  xOpen on the memory after a small header, then points `pMethods` at a Go-global
  `Tsqlite3_io_methods` whose 18 slots forward to the real methods and log.
- Registration: `Xsqlite3_vfs_register(tls, &shimVFS, 0)`; select per connection with
  `?vfs=logshim` in the DSN.

| Case | Result |
| --- | --- |
| Rollback (DELETE) mode: create table, 50 one-row commits, close, reopen, count, integrity | 50 rows, ok. Write phase: xOpen=52 xWrite=508 xSync=153 xDelete=51 (3 syncs per commit: journal, journal, db) |
| WAL mode with io_methods version 3 (xShm* forwarded) | 50 rows, ok. Write phase: xOpen=3 xWrite=128 xSync=57 xShmMap=2 xShmLock=320 xTruncate=1 xShmUnmap=1 |
| WAL, two connections through the shim: reader holds a read txn while the writer commits 100 rows | reader snapshot 0 before and 0 during; 100 visible after its txn ends; `wal_checkpoint(TRUNCATE)` busy=0 |
| io_methods version 1 (no xShm*) asked for `journal_mode=WAL` | silently stays `delete` (PRAGMA journal_mode returns `delete`); everything else works |
| Fault injection: return SQLITE_IOERR (10) from the Nth xWrite / xSync during one INSERT | DELETE mode: xWrite #1-3 and xSync #1-3 all fail the INSERT with `disk I/O error (10)`, row absent after reopen, integrity ok. WAL mode: xWrite #1-2 and xSync #1 fail the INSERT (row absent, integrity ok); xWrite #3, xSync #2-3 never happen (a WAL commit is 2 writes + 1 sync at synchronous=FULL) |
| Determinism | two host runs byte-identical |
| linux/amd64 run, Debian container run | identical results |
| darwin/arm64, darwin/amd64 | build + `go vet -unsafeptr=false` pass; not run |

What WAL needs beyond rollback: io_methods iVersion >= 2 with xShmMap/xShmLock/xShmBarrier/
xShmUnmap (forwarding them to the real unix methods worked); version 3 adds xFetch/xUnfetch
(forwarded; mmap is off by default so not exercised).

## Unsafe / internal API it leans on

- `modernc.org/sqlite/lib` (exported but generated, "DO NOT EDIT"): struct layouts
  `Tsqlite3_vfs`, `Tsqlite3_io_methods`, `Tsqlite3_file`; `Xsqlite3_vfs_find`, `Xsqlite3_vfs_register`.
- ccgo's function-pointer representation (func value data word in a uintptr). Not a documented
  API; it is what modernc's own `vfs` package relies on (`vfs/patches64.go` writes Go funcs into
  io_methods slots by offset), so it changes only if ccgo changes its calling convention.
- `modernc.org/libc`: `TLS`, `CString`, `GoString`, `Bool32`, `NewTLS`.
- 34 `unsafe.Pointer` conversions in shim.go; `go vet` reports "possible misuse of
  unsafe.Pointer" 10 times. `go vet -unsafeptr=false ./...` passes. A course package with this
  code would need that vet flag (the crossbuild script vets with defaults).
- Only top-level funcs are stored (no closures), so the GC never needs to see them.
- Stability: tied to the generated lib of one driver version; the struct layouts follow
  sqlite3.h, which is stable across SQLite 3.x. A driver upgrade needs a re-run of this probe.

## Risk

- Low for correctness at the pinned version (all cases above pass, deterministic).
- Medium for maintenance: undocumented calling convention, vet exceptions, and every shim
  method is a hand-written signature that must match the translated one exactly (a mismatch is
  memory corruption, not a compile error).
- The shim runs inside the translated library: what it observes is the translated os_unix.c's
  VFS calls, the same calls the CLI's `-vfstrace` shows for the C build.

## Recommendation

**(1) Real VFS chapter experiments**, sized for a platform builder, in chapter 7 (and reused in 9):

1. A Go forwarding VFS (`internal/` package, ~270 lines without faults) that counts and logs
   VFS calls per commit for the in-process Go driver — the in-process twin of the CLI's
   `-vfstrace` (rollback vs WAL call counts, syncs per commit at each `synchronous` level).
2. Deterministic fault injection in that VFS: fail the Nth xWrite/xSync and show SQLite's
   recovery (row absent, integrity ok) in both journal modes — restores the "fail Nth write/sync"
   rung that revision 2 cut for lack of custom C.
3. A read-only remote-read VFS demo with `modernc.org/sqlite/vfs` + an HTTP/S3 range-reading
   `fs.FS` (request count per query; why `immutable=1` and checkpointed WAL are required) —
   the pattern behind "query a snapshot straight from object storage".

Keep the rev-2 tracing treatment (CLI `-vfstrace` + strace) alongside it for the C-level view;
the Go VFS adds what tracing cannot do (fault injection, in-process counts). If Nick prefers
minimum maintenance risk, (2) keep rev-2 tracing only; then experiment 3 above still works
because it uses only the public `vfs` package and no unsafe code.
