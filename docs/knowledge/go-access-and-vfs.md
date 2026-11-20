# Go access strategy and VFS feasibility (research for the outline)

Research agent findings, 2026-11-20. Everything below was run on vm-1 (Go 1.26.8, gcc 13.3,
Linux 6.8, 4 vCPU) against the pin, SQLite 3.53.4, source id
`2026-07-24 19:02:57 bf7c7f30031888f4e796e429ab3978879485813aaca6f641c7b33e4e09459bcc`.
Probes are in `agent_work/probes/go-access-vfs/`, and its `README.md` gives every run command.
Anything not run is marked **UNVERIFIED**.

## Recommendation (short)

- **Primary route: a first-party cgo package that compiles the amalgamation generated from the
  pinned tree.** It is the only route that gives all of the following at once: the exact pin;
  the real `os_unix.c` underneath every experiment; our own compile options (SNAPSHOT, DBPAGE,
  DBSTAT, SESSION, PREUPDATE_HOOK, `SQLITE_DEBUG` behind a build tag); every C API the course
  needs; and C VFS shims (the tree's own `ext/misc/vfstrace.c`, plus fault injection) with
  callbacks into Go. A usable wrapper is about 135 non-comment lines of Go. It was built and
  exercised here (`cgo/sqlitec`).
- **Exceptions:**
  - **CLI via os/exec** (`/usr/local/bin/sqlite3`, the pin) for the "second process" in
    multi-process experiments, for `.dbinfo`/`.filectrl`/`-vfstrace` demonstrations a reader
    can retype, and as a cross-check. It has `-vfstrace` built in.
  - **strace** of the Go process, used next to the VFS trace (not instead of it) in the
    durability chapters. Only strace shows the directory `fsync` and the real `fcntl` byte
    ranges.
  - **database/sql drivers** only where the experiment is *about* the Go ecosystem, for example
    "what mattn's defaults do to durability". If one is used, pin **mattn v1.14.52** (cgo,
    bundles the pin byte-for-byte) or **modernc v1.60.1**. Do not use ncruces for internals
    experiments: it never runs `os_unix.c` (`SQLITE_OS_OTHER=1`, with a Go VFS using OFD locks).
- **Layout:** a single `go.mod` at the repo root. The amalgamation is compiled in its own cgo
  package (`internal/sqlite/amalg`), apart from the wrapper. Editing a cgo package recompiles
  all of its C (75 s for the 9.5 MB amalgamation), but with the split, wrapper edits rebuild in
  3 s. Every experiment prints `sqlite3_sourceid()` and fails if it is not the pin.

## 1. Which SQLite each route runs

Probe: `version/*`, `latest/*`. Each route runs
`select sqlite_version(), sqlite_source_id()` plus `pragma compile_options`.

### Latest tags (proxy.golang.org reachable, checked 2026-11-20)

```
$ curl -s https://proxy.golang.org/<module>/@latest
github.com/mattn/go-sqlite3   v1.14.53  (2026-11-05)
modernc.org/sqlite            v1.62.0   (2026-11-07)
github.com/ncruces/go-sqlite3 v0.37.0   (2026-11-07)
```

At those tags **none of the three runs the pin**. All three run 3.54.1
(`2026-10-16 14:45:20 555e07ef…`):

```
== mattn (latest)   sqlite_version=3.54.1 matches_pin=false
== modernc (latest) sqlite_version=3.54.1 matches_pin=false
== ncruces (latest) sqlite_version=3.54.1 matches_pin=false
```

### Driver versions that bundle exactly 3.53.4 (found by grepping each tag's sources)

| Route | Version that gives the pin | How verified | Notes |
| --- | --- | --- | --- |
| mattn/go-sqlite3 (bundled) | **v1.14.50, .51, .52** (use v1.14.52) | `grep SQLITE_SOURCE_ID sqlite3-binding.h` + run | `sqlite3-binding.c` = official `sqlite3.c` wrapped in `#ifndef USE_LIBSQLITE3` (only the final `End of sqlite3.c` comment line differs). |
| mattn `-tags libsqlite3` | any version | run; `ldd` | Links `/usr/local/lib/libsqlite3` (the pin). The `.so` has **no SONAME**, so the binary records `NEEDED libsqlite3.so` and resolves to `/usr/local/lib/libsqlite3.so` (Ubuntu's 3.45 `libsqlite3.so.0` is never chosen). Static build: `CGO_LDFLAGS="-L<dir holding only libsqlite3.a> -lm" go build -tags libsqlite3` gives `ldd … \| grep -c sqlite` = 0 and the pinned source id. `/usr/local/include/sqlite3.h` is byte-identical to the pinned header. |
| modernc.org/sqlite | **v1.58.0 to v1.60.1** (use v1.60.1, with `modernc.org/libc v1.77.1` exactly as its go.mod requires) | `SQLITE_SOURCE_ID` in `lib/`, then run | Transpiled from `modernc.org/libsqlite3 v1.15.0` **with patches**: `sqlite_issue255.patch2` (155 changed lines in `src/os_unix.c`, an OFD-lock gate), `sqlite_issue221.patch` (267 lines, SEH emulation in `wal.c` under ccgo), plus small `volatile` patches. |
| ncruces/go-sqlite3 | **v0.35.5, v0.35.6** (use v0.35.6 **and** force `github.com/ncruces/go-sqlite3-wasm/v6 v6.3.35304`) | module `.mod` files, then run | Its build script downloads `sqlite-autoconf-3530400.tar.gz` and applies `busy_timeout.patch` and `vfs_find.patch` (removes SQLite's VFS registry: "Go handles VFS registration"). `go get` alone leaves the wasm module at 6.5.35401 (3.54.1) if it was in go.mod before; you must downgrade it explicitly. |
| first-party cgo | any | run | See 1d. |
| CLI (`/usr/local/bin/sqlite3`) | installed | `sqlite3 -json` from Go | Source id matches the pin. |

Pinned-version run output (`go run ./version/<route>`):

```
route=mattn    sqlite_version=3.53.4 matches_pin=true  compile_options(selected)=DEFAULT_SYNCHRONOUS=2 DEFAULT_WAL_SYNCHRONOUS=1 THREADSAFE=1
route=mattn -tags libsqlite3   matches_pin=true  DEFAULT_SYNCHRONOUS=2 DEFAULT_WAL_SYNCHRONOUS=2 ENABLE_DBPAGE_VTAB ENABLE_DBSTAT_VTAB THREADSAFE=1
route=modernc  sqlite_version=3.53.4 matches_pin=true  DEFAULT_SYNCHRONOUS=2 DEFAULT_WAL_SYNCHRONOUS=2 ENABLE_DBPAGE_VTAB ENABLE_DBSTAT_VTAB ENABLE_PREUPDATE_HOOK ENABLE_SESSION ENABLE_SNAPSHOT THREADSAFE=1
route=ncruces  sqlite_version=3.53.4 matches_pin=true  DEFAULT_SYNCHRONOUS=2 DEFAULT_WAL_SYNCHRONOUS=1 ENABLE_API_ARMOR ENABLE_PREUPDATE_HOOK THREADSAFE=0
route=cli      sqlite_version=3.53.4 matches_pin=true  ENABLE_DBPAGE_VTAB ENABLE_DBSTAT_VTAB   (no SNAPSHOT, SESSION, PREUPDATE_HOOK)
route=first-party-cgo  matches_pin=true  DEFAULT_SYNCHRONOUS=2 DEFAULT_WAL_SYNCHRONOUS=2 ENABLE_DBPAGE_VTAB ENABLE_DBSTAT_VTAB ENABLE_EXPLAIN_COMMENTS ENABLE_PREUPDATE_HOOK ENABLE_SESSION ENABLE_SNAPSHOT THREADSAFE=1
```

### Defaults that silently differ from upstream (matters for the durability chapters)

| Route | What differs | Evidence |
| --- | --- | --- |
| mattn (bundled) | Compiled with `-DSQLITE_DEFAULT_WAL_SYNCHRONOUS=1`. **On every connection it also runs `PRAGMA synchronous = NORMAL`** (default `synchronousMode := "NORMAL"`, sqlite3.go:1241, exec at :1906) and sets busy_timeout 5000 (:1231). This holds **even with `-tags libsqlite3`**. Override with the DSN `_sync=FULL`. | The vfstrace through mattn (probe `vfs/mattnshim`) shows the commit's two `xWrite`s to `-wal` and **no `xSync`**, both bundled and with libsqlite3. |
| ncruces | `SQLITE_DEFAULT_WAL_SYNCHRONOUS 1`, `SQLITE_THREADSAFE 0`, `SQLITE_DEFAULT_FOREIGN_KEYS 1`, `SQLITE_OMIT_DESERIALIZE`, `SQLITE_OMIT_SHARED_CACHE`, and **`SQLITE_OS_OTHER 1`** (`go-sqlite3-wasm/v6@v6.3.35304/build/sqlite_opt.h`, `sqlite_cfg.h`) | `samples/ncruceslog.txt`: the insert-commit has no xSync. |
| first-party cgo, CLI, modernc | Upstream defaults (`DEFAULT_WAL_SYNCHRONOUS=2`, i.e. FULL) | `samples/cgo-vfstrace.txt`: the insert-commit ends with `xSync(t.db-wal,FULL)`. |

### 1d. First-party cgo build of the pinned amalgamation

Commands (`gen-amalgamation.sh`):

```bash
cp -a "$SQLITE_SRC" /tmp/sqlite-research-go-access-vfs/tree
cd /tmp/sqlite-research-go-access-vfs/tree && ./configure && make sqlite3.c   # 23 s wall
sha256sum sqlite3.c sqlite3.h
b1dd5d74ec7f29055a6684fa06fb3c2f6821c87dd38f9a458dfd2e8a1db28189  sqlite3.c   (9,515,341 bytes)
919e7f2e8ed1d8f56ac17b412b8971c76aa5d1a879752cc6058f75e7d5910e1d  sqlite3.h
curl -o a.zip https://www.sqlite.org/2026/sqlite-amalgamation-3530400.zip   # HTTP 200, 2,946,650 bytes
sha256 a.zip = 1e71ddf93849c6a6ecf58b827c0692073d2dd7ee40196158068f7b29f422e87d
cmp official/sqlite3.c tree/sqlite3.c  -> identical
cmp official/sqlite3.h tree/sqlite3.h  -> identical
```

The tree-generated amalgamation is **byte-identical** to the official 3.53.4 zip, and
`SQLITE_SOURCE_ID` in `sqlite3.h:151` is the pin.

Package shape (built and run):

- `cgo/amalg/amalg.go`: 13 lines, only `#cgo CFLAGS` and `import "C"`. cgo compiles
  `sqlite3.c` from the directory. Flags used: `-O2 -DSQLITE_THREADSAFE=1 -DHAVE_USLEEP=1
  -DSQLITE_DQS=0 -DSQLITE_ENABLE_SNAPSHOT -DSQLITE_ENABLE_DBPAGE_VTAB
  -DSQLITE_ENABLE_DBSTAT_VTAB -DSQLITE_ENABLE_SESSION -DSQLITE_ENABLE_PREUPDATE_HOOK
  -DSQLITE_ENABLE_EXPLAIN_COMMENTS`, plus `-lm`.
- `cgo/amalg/debug.go`: `//go:build sqlite_debug` with `#cgo CFLAGS: -DSQLITE_DEBUG`. cgo merges
  it, and `pragma compile_options` then lists `DEBUG`.
- `cgo/sqlitec/sqlitec.go`: 168 lines (135 non-comment). Open, Close, Exec, Query
  (prepare_v2/step/column_*), CheckpointV2, FileControlInt, SnapshotGet/Open/Free, Serialize,
  DBStatus. `trace.go` (69 lines) handles the vfstrace/wal_hook callbacks into Go through
  `//export`.

Build times on vm-1 (4 vCPU):

| Build | Time |
| --- | --- |
| cold build of the amalgamation package | 80–89 s |
| `-tags sqlite_debug` | 93 s |
| no-change rebuild | 0.16 s |
| **editing any file of a cgo package that also holds `sqlite3.c`** | **75 s** |
| editing the wrapper after the split into `amalg` + `sqlitec` | 3.0 s |

For comparison, cold builds of the drivers: mattn 118 s, modernc 43 s, ncruces 48 s.

`go run ./cgo/cmd/version` output (every API it calls works):

```
wal_hook frames after insert: 4
count via old snapshot (expect 2): 2          <- sqlite3_snapshot_get / _open across a second connection's commit
count now (expect 3): 3
wal_checkpoint_v2(TRUNCATE): nLog=0 nCkpt=0 err=<nil>
FCNTL_DATA_VERSION: 7 <nil>
sqlite_dbpage: [[1 4096] [2 4096]] <nil>
dbstat: [[sqlite_schema 1 leaf] [t 2 leaf]] <nil>
serialize bytes: 8192
db_status CACHE_WRITE: 5
```

Note: `sqlite3_snapshot_get` outside a read transaction returns SQLITE_ERROR, as documented.

### 1e. CLI via os/exec (`version/cli`)

- `sqlite3 -json :memory: "select …"` parses with `encoding/json` and gives the pinned id.
- **The CLI has `-vfstrace`** (`shell.c.in:13193`, which INCLUDEs `ext/misc/vfstrace.c` at
  `shell.c.in:301`). Its trace goes to stderr and is the same vfstrace output as the cgo route,
  over the real os_unix.c.
- `.filectrl data_version` and `.filectrl persist_wal` work. `pragma wal_checkpoint(passive)`
  returns `busy|log|checkpointed` (`0|1|1`), the same numbers as `sqlite3_wal_checkpoint_v2`.
  `.stats on` prints the `sqlite3_status`/`db_status` counters.
- It cannot do: `ENABLE_SNAPSHOT` (not compiled in, and there is no shell command for it),
  session, preupdate hook, wal/commit hooks, custom VFS, fault injection, or `sqlite3_backup`
  step-by-step (`.backup` is all-at-once). It also cannot hold a transaction open across
  separate invocations. For that, keep one CLI on a stdin pipe. **Gotcha:** the CLI
  block-buffers stdout on a pipe, so you cannot wait on its output. The `locks` probe has the
  CLI run `.shell touch <file>` after its SQL and polls for that file.

## 2. API matrix

Verified by reading each pinned tag's source (mattn v1.14.52, modernc v1.60.1, ncruces v0.35.6)
and, where marked ✔︎run, by calling the API. "SQL" means the same information is available
through a pragma or table-valued function on every route.

| API / feature | mattn v1.14.52 | modernc v1.60.1 | ncruces v0.35.6 | first-party cgo | CLI |
| --- | --- | --- | --- | --- | --- |
| Raw conn through `database/sql` `Conn.Raw` | `*SQLiteConn` | `*sqlite.conn` (unexported type; call methods via interface assertion) | `driver.Conn.Raw() *sqlite3.Conn`, or use `sqlite3.Open` without database/sql | n/a (own API) | n/a |
| `sqlite3_backup_*` | `(*SQLiteConn).Backup` → Step/Remaining/PageCount/Finish | `NewBackup`/`NewRestore` → Step/Remaining/PageCount/Finish | `Backup`, `BackupInit` → step API | add 1 wrapper func (not written) | `.backup` (whole) |
| `sqlite3_wal_checkpoint_v2` (nLog, nCkpt) | ✘ API; SQL `pragma wal_checkpoint` | ✘ driver; ✔︎run via `lib.Xsqlite3_wal_checkpoint_v2` (`version/modernclib`: `nLog=3 nCkpt=3`) | `WALCheckpoint` → (nLog, nCkpt) ✔︎run | ✔︎run | SQL pragma |
| `sqlite3_wal_hook` | ✘ | ✘ driver (lib only) | `WALHook` | ✔︎run (`//export`) | ✘ |
| `sqlite3_snapshot_*` | ✘ not compiled | compiled (ENABLE_SNAPSHOT); ✘ driver; ✔︎run `lib.Xsqlite3_snapshot_get` rc=0 | ✘ not compiled, not exported | ✔︎run get/open/free | ✘ |
| `sqlite3_serialize/deserialize` | `Serialize`/`Deserialize` (default build) | `Serialize`/`Deserialize` | ✘ (`OMIT_DESERIALIZE`); `ext/serdes` emulates it via backup into a Go slice VFS | ✔︎run serialize | ✘ (`.dbtotxt` hex dump) |
| `sqlite3_db_status` | ✘ | `Status` | `Status` (db_status64) | ✔︎run | `.stats` |
| `sqlite3_status64` (global) | ✘ | lib only | ✘ | 1 line to add | `.stats` |
| `sqlite3_stmt_status` | internal use only | lib only | `(*Stmt).Status` | 1 line to add | `.stats` |
| `sqlite3_file_control` | `SetFileControlInt`/`Int64` (set only, **result not returned**, so DATA_VERSION cannot be read) | `FileControlPersistWAL`, `FileControlDataVersion` only | `FileControl`: PERSIST_WAL, DATA_VERSION, LOCKSTATE, RESERVE_BYTES, CHUNK_SIZE, … (no CKPT_*; those are VFS-side) | ✔︎run any int op | `.filectrl` |
| `sqlite3_trace_v2` | `SetTrace` (tag `sqlite_trace`) | lib only | `Trace` | add | `.trace` |
| `sqlite3_db_config` (NO_CKPT_ON_CLOSE) | ✘ | unexported `dbConfigBool` | `Config(DBCONFIG_NO_CKPT_ON_CLOSE, …)` | add | `.dbconfig` |
| `sqlite3_busy_handler` | ✘ (DSN `_busy_timeout`) | lib only (DSN `_busy_timeout`) | `BusyHandler`, `BusyTimeout` | add | `.timeout` |
| custom VFS (`sqlite3_vfs_register`) | ✘ Go API, but **✔︎run: a separate cgo package in the same binary can register a C VFS** (`vfs/mattnshim`) | `vfs` pkg is a **read-only `fs.FS`** VFS (`vfs/patches.go:5`); a writable C-style VFS through lib is theoretically possible (**UNVERIFIED**) | Go-only `vfs.Register` (no C VFS) | ✔︎run (vfstrace, fault) | `-vfstrace` only |
| `sqlite3_expanded_sql` / EXPLAIN | inside trace callback; EXPLAIN via SQL | lib; SQL | `ExpandedSQL`; SQL | add; SQL (`ENABLE_EXPLAIN_COMMENTS` on) | SQL, `.eqp` |
| `sqlite_dbpage` vtab | ✘ bundled (add `CGO_CFLAGS=-DSQLITE_ENABLE_DBPAGE_VTAB`, **UNVERIFIED**); ✔︎ with libsqlite3 (pin's lib has it) | ✔︎ compiled | ✘ | ✔︎run | ✔︎ |
| `dbstat` vtab | tag `sqlite_dbstat`; ✔︎ libsqlite3 | ✔︎ | ✘ | ✔︎run | ✔︎ |
| session extension | ✘ | compiled, lib only | ✘ | compiled (wrapper not written) | ✘ |
| update/commit/rollback hooks | Register{Update,Commit,Rollback}Hook; preupdate needs tag `sqlite_preupdate_hook` | Commit/Rollback/PreUpdate (no update hook) | Update/Commit/Rollback/PreUpdate | wal_hook ✔︎run; others: add | ✘ |

## 3. Custom / logging VFS from Go

All logging probes run the same sequence: open, `journal_mode=WAL`, `create table`, one
`insert` (commit), passive checkpoint, close.

### 3c. First-party cgo + the tree's `ext/misc/vfstrace.c` (✔︎ built, recommended)

- **The path is confirmed:** `ext/misc/vfstrace.c` (1211 lines; `vfstrace_register` at line
  1138). `ext/misc/vfslog.c` (760 lines) and `src/test_osinst.c`, `src/test_vfs.c`,
  `src/test6.c` (crash simulation) also exist. test6.c needs Tcl (`#include "tclsqlite.h"`, 61
  `Tcl_` uses).
- **Upstream quirk at the pin:** vfstrace.c does **not compile standalone**. It uses `u8` (lines
  143, 583) and `i64` (849, 860), which exist only when the shell INCLUDEs it. The fix is two
  typedefs before `#include` (`cgo/sqlitec/glue.c`).
- Output goes to Go through `xOut` → `//export goVfsTrace`. vfstrace emits each call as two
  fragments (the call, then ` -> rc\n`), so the Go side joins them into lines.
- **cgo rule hit:** a preamble in a file that has `//export` may contain only declarations.
  Definitions there are compiled twice (`multiple definition of vfstrace_register`). Put the C in
  a `.c` file.
- Size: glue.c is 22 lines, trace.go 69. Fidelity: **full**. os_unix.c does all I/O, locking,
  shm and mmap.
- Insert-commit and checkpoint excerpt (`samples/cgo-vfstrace.txt`, 133 lines in total):

```
insert     trace.xShmLock(t.db,ofst=0(WRITE),n=1,LOCK|EXCLUSIVE) -> SQLITE_OK
insert     trace.xWrite(t.db-wal,n=24,ofst=8272) -> SQLITE_OK
insert     trace.xWrite(t.db-wal,n=4096,ofst=8296) -> SQLITE_OK
insert     trace.xSync(t.db-wal,FULL) -> 0
insert     trace.xShmLock(t.db,ofst=0(WRITE),n=1,UNLOCK|EXCLUSIVE) -> SQLITE_OK
insert     trace.xFileControl(t.db,COMMIT_PHASETWO) -> SQLITE_NOTFOUND
checkpoint trace.xShmLock(t.db,ofst=1(CKPT),n=1,LOCK|EXCLUSIVE) -> SQLITE_OK
checkpoint trace.xSync(t.db-wal,FULL) -> 0
checkpoint trace.xFileControl(t.db,CKPT_START) -> SQLITE_NOTFOUND
checkpoint trace.xRead(t.db-wal,n=4096,ofst=56) -> SQLITE_OK
checkpoint trace.xWrite(t.db,n=4096,ofst=0) -> SQLITE_OK
checkpoint trace.xRead(t.db-wal,n=4096,ofst=8296) -> SQLITE_OK
checkpoint trace.xWrite(t.db,n=4096,ofst=4096) -> SQLITE_OK
checkpoint trace.xFileControl(t.db,CKPT_DONE) -> SQLITE_NOTFOUND
checkpoint trace.xTruncate(t.db,8192) -> 0
checkpoint trace.xSync(t.db,FULL) -> 0
checkpoint nLog=3 nCkpt=3 err=<nil>
close      trace.xFileControl(t.db,PERSIST_WAL,-1) -> SQLITE_OK, 0
close      trace.xShmUnmap(t.db,delFlag=1) -> SQLITE_OK
close      trace.xDelete("t.db-wal",0) -> SQLITE_OK
```

- vfstrace can be narrowed at runtime: `PRAGMA vfstrace('-all, +Lock,Unlock,ShmLock')`
  (documented in the file header). **UNVERIFIED** in a probe.
- **Fault injection** (`cgo/sqlitec/faultvfs.c`, 82 lines of C, a pass-through shim with
  io_methods v3 including xShm*/xFetch). Run of `go run ./cgo/cmd/fault`, which is
  deterministic (two runs gave identical output):

```
baseline WAL commit: err=<nil> xWrite=3 xSync=2
fail xWrite #1..#3 -> sqlite rc=10 (disk I/O error) ; after: integrity=ok rows=base
fail xSync  #1..#2 -> sqlite rc=10 (disk I/O error) ; after: integrity=ok rows=base
child _exit at xSync #1 -> child exit status 9 ; reopen: integrity=ok rows=base
child _exit at xSync #2 -> child exit status 9 ; reopen: integrity=ok rows=base,new
```

  The last line is the teaching point for chapters 9 and 10. A **process** crash before the
  final fsync still commits, because the frames are in the OS page cache and WAL recovery finds
  a valid commit frame. To simulate **power loss** (dropping writes not yet synced), the shim
  must buffer writes between xSync calls, as `src/test6.c` does. That is not built: estimated
  ~300 lines of C to port test6.c's model without Tcl (**UNVERIFIED** estimate). Another
  option: os_unix.c's `xSetSystemCall` can replace `pwrite64`, `ftruncate`, `fcntl`, and so on
  under the real VFS (syscall table at os_unix.c:428+, `unixSetSystemCall` at :731). But
  `fsync`/`fdatasync` are **not** in that table (`full_fsync` at :3778 calls them directly), so
  sync faults need the VFS shim.

### 3a. ncruces `vfs` package (✔︎ built, not recommended for internals)

- WAL/shm on Linux: `vfs.SupportsSharedMemory=true`, `SupportsFileLocking=true` (printed by
  the probe). It wraps the default as a shim: `vfs.Find("os")`, plus `util/vfsutil.Wrap*`
  helpers.
- Gotcha: the wrapper **must implement `vfs.VFSFilename.OpenFilename`**. A plain `Open` of
  the main db fails with "unable to open database file".
- **Visibility gap:** `vfs.SharedMemory` has only unexported methods (`shmMap`, `shmLock`,
  `shmUnmap`, `shmBarrier`, in `vfs/api.go`), so a wrapper can only forward the inner object.
  **xShmMap/xShmLock are invisible.** The probe logs each `SharedMemory()` lookup instead
  (one per shm operation, with no detail). File-controls are visible only through exported
  optional interfaces (CKPT_START/DONE, COMMIT_PHASETWO, … were logged; MMAP_SIZE, PDB,
  BUSYHANDLER details were not).
- **Fidelity: low for this course.** What runs underneath is ncruces's Go reimplementation
  (`vfs/os_unix.go`, `lock.go`, `shm_ofd.go`), not `os_unix.c`, which the wasm build compiles
  out (`SQLITE_OS_OTHER 1`). Same call sequence at the VFS level (see
  `samples/ncruceslog.txt`), but different code below it.
- 164 lines of Go. Fault injection in pure Go is easy (return an error from WriteAt or Sync).
  Deterministic: two runs were identical.

### 3b. modernc `vfs` package (inspected, not usable for logging writes)

`modernc.org/sqlite/vfs`: "Package vfs exposes a Go [fs.FS] to SQLite as a **read-only** VFS"
(`vfs/patches.go:5`, `func New(fs fs.FS)`). It cannot observe or log writes. A writable shim
would mean hand-building `sqlite3_vfs`/`sqlite3_io_methods` structs as `uintptr` memory with
`libc.TLS` callbacks through `modernc.org/sqlite/lib`. That is not attempted (**UNVERIFIED**
feasibility). The `modernclib` probe shows the style: even simple calls trip `go vet`
("possible misuse of unsafe.Pointer").

### 3d. strace (✔︎ built; complements, does not replace, 3c)

`strace/main.go` re-runs itself under
`strace -f -s 300 -e trace=openat,close,pread64,pwrite64,fsync,fdatasync,fcntl,unlink,unlinkat,ftruncate,write,mmap,munmap`.
The child writes each vfstrace line to fd 2, so strace records VFS calls and syscalls **in one
ordered stream**. The parent maps fds back to names. Excerpt (`samples/strace-merged.txt`):

```
  VFS trace.xShmLock(w.db,ofst=0(WRITE),n=1,LOCK|EXCLUSIVE) -> SQLITE_OK      (after:)
      SYS fcntl(w.db-shm F_SETLK, {l_type=F_WRLCK, l_whence=SEEK_SET, l_start=120, l_len=1}) = 0
      SYS pwrite64(w.db-wal n=24 off=8272) = 24
      SYS pwrite64(w.db-wal n=4096 off=8296) = 4096
      SYS fsync(w.db-wal ) = 0
  VFS trace.xSync(w.db-wal,FULL) -> 0
==== delete-mode-commit (first journal sync of a new journal)
      SYS fsync(d.db-journal ) = 0
      SYS openat(<dir>) = fd 6
      SYS fsync(<dir> ) = 0
      SYS close(<dir>)
  VFS trace.xSync(d.db-journal,FULL) -> 0
```

- **Only strace shows:** the directory fsync inside `unixSync` (os_unix.c:3941,
  `UNIXFILE_DIRSYNC`, set for new journals/WALs at :6777, `openDirectory` at :3874); the real
  lock byte ranges (the shm WRITE lock is byte 120 of `-shm`; database locks at
  1073741824 = PENDING_BYTE, then +1 RESERVED and +2..+511 SHARED); `fsync` versus `fdatasync`;
  the `mmap` of `-shm` (`len=32768`); and `unlink` of `-shm` on last close.
- **Only the VFS shows:** intent (xShmLock slot names like READ1/CKPT, xLock levels, the
  xSync flag FULL/NORMAL, xFileControl ops such as CKPT_START, PERSIST_WAL, COMMIT_PHASETWO),
  calls that issue no syscall (xShmBarrier, a shm lock already held in-process, xAccess
  results), and the return codes SQLite saw.
- strace cannot inject faults. It is deterministic here (two runs gave identical output). It
  needs `/usr/bin/strace` (present), and ptrace works as root on vm-1.

### VFS route comparison

| Route | Runs os_unix.c? | Sees xShm* | Sees real syscalls / dir fsync | Fault: fail Nth write/sync | Power-loss model | Code size | Deterministic |
| --- | --- | --- | --- | --- | --- | --- | --- |
| cgo + vfstrace.c (+ faultvfs.c) | **yes** | **yes** | no | **yes** (✔︎run) | possible, port test6.c (not built) | 22 C + 69 Go (+82 C fault) | yes (2 runs identical) |
| cgo + strace merged | **yes** | yes (via vfstrace) | **yes** | no | no | 148 Go | yes |
| mattn + separate cgo shim package | yes | yes | no | yes (same C) | same | 9 C + 26 Go | not repeated |
| ncruces Go VFS | **no** (Go reimplementation, OFD locks) | **no** | no | yes (pure Go) | easy in Go | 164 Go | yes |
| modernc `vfs` | n/a (read-only fs.FS) | no | no | no | no | – | – |
| CLI `-vfstrace` | yes | yes | no | no | no | 0 | not repeated |

## 4. Process-level concerns

Probe `locks`: the pinned CLI (POSIX `F_SETLK`) against each driver, in both directions, in
WAL and DELETE modes, with busy timeout 0:

```
mattn   wal    CLI holds write txn -> Go read: OK | Go write: database is locked
mattn   wal    Go holds write txn  -> CLI read: OK 1 | CLI write: … database is locked
modernc wal    … same pattern (Go write: database is locked (5) (SQLITE_BUSY))
ncruces wal    … same pattern (Go write: sqlite3: database is locked)
(identical results in delete mode; integrity_check ok and rows "cli,go2" in all six databases)
mattn   lock syscalls for open+WAL+create+insert: F_GETFD=3 F_GETFL=3 F_GETLK=2 F_SETLK=47
modernc lock syscalls for open+WAL+create+insert: F_GETFD=3 F_GETFL=3 F_GETLK=2 F_SETFD=7 F_SETLK=47
ncruces lock syscalls for open+WAL+create+insert: F_GETFD=3 F_GETFL=15 F_GETLK=4 F_OFD_SETLK=39 F_OFD_SETLKW=1 F_SETFL=12
```

- All three routes interoperate correctly with os_unix.c across processes. On Linux, OFD and
  POSIX locks conflict with each other. **But ncruces does its own locking:** OFD locks, 39
  lock calls instead of 47, and a blocking `F_OFD_SETLKW`. A chapter 6 trace of lock syscalls
  through ncruces would not match `os_unix.c` (`unixLock` :1866, `unixFileLock` :1802,
  `osSetPosixAdvisoryLock` :1737, `unixShmLock` :5284).
- modernc uses POSIX locks by default (same count as mattn). Its OFD mode is opt-in
  (`OFDLocking()` / `MODERNC_SQLITE_OFD_LOCK=1`) through its patch to os_unix.c.
- cgo routes: every blocked statement pins an OS thread (prior measurement: `kb show
  go-sqlite-drivers-cancellation-and-threads.md`). This does not matter for these small
  experiments.
- **POSIX-lock hazard to teach, not trip over:** any `os.Open(dbPath)`+`Close()` in the same
  Go process drops that process's POSIX locks on the file (documented in modernc's `ofd.go`
  and in os_unix.c's comments). Experiments that hash or copy the db file while a connection is
  open must do it in another process, or after close.
- **Crash experiments:** `kill -9` of a child works on every route (no driver obstacle found).
  For a *deterministic* crash point, use the cgo fault shim (`_exit(9)` at xSync #k, ✔︎run
  above) rather than timing a signal. Run the child as a separate binary or as `self child …`
  (both used in the probes).

## 5. Recommendation and module layout

1. **Primary:** a first-party cgo package (`internal/sqlite`) over the pinned amalgamation,
   regenerated by a script from the pinned tree and checked against the official zip's sha256
   (`b1dd5d74…` for sqlite3.c). Commit the script and the hash, not the 9.5 MB file, or commit
   it if offline reproducibility matters (Nick's call). Reasons: exact pin with an exact
   upstream file; os_unix.c underneath; every needed API (snapshot, wal hook,
   checkpoint_v2 counts, file_control reads, dbpage, dbstat, session); vfstrace and fault
   shims in C that call into Go; `-tags sqlite_debug` for assert-enabled runs.
2. **Exceptions:** the pinned CLI for second processes and reader-retypeable checks, and strace
   for syscall-level truth in chapters 9/10. The mattn defaults are a short "ecosystem pitfall"
   aside, pinned at v1.14.52 with `_sync=FULL` noted.
3. **Layout:** one `go.mod` at the repo root (no per-chapter modules). Chapter experiments are
   `cmd/chNN-*` or `chapters/NN/*/main.go` importing `internal/sqlite`.
   - `internal/sqlite/amalg` holds only `sqlite3.c`, `sqlite3.h` and the `#cgo CFLAGS`. This
     avoids 75 s rebuilds.
   - `internal/sqlite` holds the Go wrapper, the vfstrace glue and the fault shim (`-I../amalg`).
   - **Never link mattn and `amalg` into one binary.** Both define every `sqlite3_*` symbol, so
     the link fails. If a chapter needs mattn, make it a separate main package.
   - cgo is required (`CGO_ENABLED=1`, gcc). No `-tags` is needed for normal runs; `-tags
     sqlite_debug` for debug builds.
4. **Source-id discipline:** each experiment's first output line is
   `sqlite_source_id=<sqlite3_sourceid()>`, and it exits non-zero unless that equals the
   `pin.SourceID` constant (see `internal/pin/pin.go` in the probes). For the CLI, run
   `sqlite3 -json :memory: "select sqlite_source_id()"` once at startup.

## UNVERIFIED / not done

- Power-loss (drop unsynced writes) shim: design only. Size estimate ~300 lines of C, not
  measured.
- `PRAGMA vfstrace(...)` runtime filtering: read in the source header, not run.
- mattn bundled with `CGO_CFLAGS=-DSQLITE_ENABLE_DBPAGE_VTAB`: not run.
- A writable VFS through `modernc.org/sqlite/lib`: not attempted.
- Whether ncruces's "os" VFS fsyncs the directory on journal creation: not checked.
- Cross-compilation of the cgo route to other OS/arch: not checked. The course runs on Linux.

## INDEX rows

Chapter numbers follow `docs/course-source.md` (2 file format, 5 WAL, 6 locking, 7 VFS, 9 backup/crash, 10 capstone).


| path | purpose | chapter | made by | what it proved |
| --- | --- | --- | --- | --- |
| `probes/go-access-vfs/gen-amalgamation.sh` | Build sqlite3.c from a copy of the pinned tree, compare with the official zip, stage it and vfstrace.c for the cgo probes | all (pin) | research agent go-access-vfs, 2026-11-20 | `make sqlite3.c` at the pin is byte-identical to sqlite-amalgamation-3530400.zip (sha256 b1dd5d74…) |
| `probes/go-access-vfs/version/{mattn,modernc,ncruces}` | Print the version, source id and selected compile options through each driver at the pinned driver tags | all (driver choice) | research agent go-access-vfs, 2026-11-20 | mattn v1.14.52, modernc v1.60.1, ncruces v0.35.6 + wasm v6.3.35304 run the pin; mattn and ncruces default to WAL_SYNCHRONOUS=1 |
| `probes/go-access-vfs/latest/*` | Same version check at the latest driver tags | all (driver choice) | research agent go-access-vfs, 2026-11-20 | mattn v1.14.53, modernc v1.62.0 and ncruces v0.37.0 all run 3.54.1, not the pin |
| `probes/go-access-vfs/version/cli` | Drive the pinned CLI from Go (-json, -vfstrace, .filectrl, wal_checkpoint, .stats) | 2, 6 | research agent go-access-vfs, 2026-11-20 | The CLI matches the pin, has -vfstrace built in, and lacks SNAPSHOT/SESSION |
| `probes/go-access-vfs/version/modernclib` | Call modernc's transpiled C API directly | all (driver choice) | research agent go-access-vfs, 2026-11-20 | wal_checkpoint_v2 and snapshot_get are reachable through modernc/lib, in uintptr C style that go vet flags |
| `probes/go-access-vfs/cgo/amalg` + `cgo/sqlitec` | First-party cgo binding: pinned amalgamation with custom options, thin wrapper, vfstrace glue, fault shim | all (primary route) | research agent go-access-vfs, 2026-11-20 | 135 non-comment Go lines cover open/exec/query/checkpoint_v2/file_control/snapshot/serialize/db_status/wal_hook; the split package cuts rebuilds from 75 s to 3 s |
| `probes/go-access-vfs/cgo/cmd/version` | Exercise the custom-option APIs through the cgo binding | 2, 5, 9 | research agent go-access-vfs, 2026-11-20 | SNAPSHOT get/open across commits, wal_hook, checkpoint_v2, DATA_VERSION, sqlite_dbpage, dbstat, serialize and db_status all work; `-tags sqlite_debug` adds DEBUG |
| `probes/go-access-vfs/cgo/cmd/vfstrace` | Log every VFS call over os_unix.c for a WAL insert-commit-checkpoint-close | 5, 7 | research agent go-access-vfs, 2026-11-20 | Full trace including xShmLock slots and CKPT_START/DONE; vfstrace.c needs u8/i64 typedefs to compile standalone at 3.53.4 |
| `probes/go-access-vfs/cgo/cmd/fault` | Fail the Nth xWrite/xSync and `_exit(9)` at xSync #k through a C shim | 9, 10 | research agent go-access-vfs, 2026-11-20 | Deterministic fault injection over os_unix.c; a process crash before the last WAL fsync still commits (page cache), so power loss needs a write-buffering shim |
| `probes/go-access-vfs/vfs/ncruceslog` | Pure-Go logging VFS over ncruces's "os" VFS | 7 (rejected route) | research agent go-access-vfs, 2026-11-20 | Works for file I/O and locks but cannot see xShm*, needs OpenFilename, and runs no os_unix.c |
| `probes/go-access-vfs/vfs/mattnshim` | Register vfstrace from a separate cgo package into mattn's SQLite | appendix (ecosystem) | research agent go-access-vfs, 2026-11-20 | A C VFS can be added to mattn without a db handle; mattn forces synchronous=NORMAL, so the commit has no xSync |
| `probes/go-access-vfs/strace` | Merge the vfstrace and strace streams of one Go process into one ordered log | 7, 9 | research agent go-access-vfs, 2026-11-20 | Shows the directory fsync, real fcntl byte ranges and the -shm mmap that the VFS cannot see, in order with VFS calls |
| `probes/go-access-vfs/locks` | Cross-process lock interop of each driver with the CLI, and lock syscall counts | 6 | research agent go-access-vfs, 2026-11-20 | All drivers interoperate with os_unix.c; ncruces uses F_OFD_SETLK (its own locking) while mattn and modernc use F_SETLK |
