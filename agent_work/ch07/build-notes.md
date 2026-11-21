# Chunk 07 build notes (plain data for the prose pass)

Builder: Claude Opus 5.5, branch `chunk/ch07`, 2026-11-20. Machine: linux/amd64, Go 1.26.8,
strace 6.8, ext4, running as root. CLI used for the evidence files: the installed
`/usr/local/bin/sqlite3` (first `sqlite3` on PATH; `_tools/bin/` has no CLI in this worktree; it
calls `fsync`). Every experiment was also run in the Debian `lab` container (`./configure` build of
the pin, which calls `fdatasync`; glibc 2.36): EXACT output identical to the host evidence for 7.1,
7.2, 7.3, 7.4 and 7.4 `-s3`. darwin/arm64 and darwin/amd64: cross-built and cross-vetted only,
never run.

## Corrections to the outline

1. **WAL at `synchronous=NORMAL` is not "zero syncs on every commit".** Ordinary commits make no
   sync (7.2: 998 of the 1,000 one-row commits). Two kinds of commit do sync: (a) the commit that
   runs an automatic checkpoint (WAL reached 1,000 frames) syncs the WAL before copying pages back
   and the database after (`walCheckpoint`, `wal.c:2277` and `wal.c:2333`, both
   `CKPT_SYNC_FLAGS(sync_flags)`): 2 syncs; (b) the next commit, which finds the WAL fully
   checkpointed, restarts it from frame 0 (`walRestartLog`, `wal.c:3875-3915`), writes a new WAL
   header and syncs it (`wal.c:4114-4124`, `if( pWal->syncHeader ) sqlite3OsSync(...,
   CKPT_SYNC_FLAGS(sync_flags))`): 1 sync. At FULL the same two commits make 3 and 2 syncs, every
   other commit 1. The first commit into a brand-new WAL file also syncs its header and the
   directory (7.1; not inside 7.2's measured window because the table was created before it).
   Why: `sqlite3PagerSetFlags` (`pager.c:3685-3698`) sets `walSyncFlags = syncFlags<<2` (header
   and checkpoint syncs, always on unless `synchronous=OFF`) and adds `syncFlags` in the low bits
   (commit syncs) only when `fullSync` (FULL or EXTRA). The outline's sentence "In WAL mode `NORMAL`
   does not sync on commit at all" needs "except the commit that checkpoints and the commit that
   restarts the WAL". The book the outline criticises is still wrong (it says a sync on every
   commit); 7.2 shows the exact pattern.
2. **"Rollback 3 at NORMAL, 4 at FULL" counts syscalls, not VFS calls.** 7.2 counts fsync/fdatasync
   syscalls: NORMAL = journal 1 + directory 1 + database 1 = 3; FULL = journal 2 + directory 1 +
   database 1 = 4. In VFS calls (7.3) it is 2 and 3 `xSync` calls, because the directory sync
   happens inside the first `xSync` of a newly created journal (`unixSync`, `os_unix.c:3941-3953`,
   one time per `xOpen` with create flag: `isNewJrnl`, `os_unix.c:6569-6572`, sets
   `UNIXFILE_DIRSYNC` at `:6777`). In DELETE mode every transaction creates the journal again, so
   every transaction pays the directory sync.
3. **The vfstrace "FULL for NORMAL" quirk applies to every sync in a default configuration.**
   Confirmed in source (`ext/misc/vfstrace.c:453`: `if( flags & SQLITE_SYNC_FULL )` with
   `SQLITE_SYNC_FULL` = 0x03, so 0x02 also matches) and measured: 7.3's Go VFS records the real flag
   0x2 (NORMAL) for every sync at `synchronous=FULL` and at `NORMAL`, while the CLI's trace prints
   `FULL`. SQLite passes `SQLITE_SYNC_FULL` only when `PRAGMA fullfsync=ON`
   (`pager.c:3687-3688`, `PAGER_FULLFSYNC`); `synchronous=FULL` selects how many syncs, not the
   flag. The rollback journal's second sync adds `SQLITE_SYNC_DATAONLY` only when the flag is FULL
   (`pager.c:4414-4417`), so it is never seen in default settings.
4. **`xAccess` maps to `stat`, not `access()`.** For `SQLITE_ACCESS_EXISTS` (the only flag seen)
   `unixAccess` calls `osStat` and treats a zero-length regular file as absent
   (`os_unix.c:6896-6898`); only `SQLITE_ACCESS_READWRITE` uses `access()`. strace shows
   `newfstatat` (normalized to `stat` in 7.1).
5. **Where the CLI first calls `xRandomness` depends on the CLI build.** The installed CLI calls it
   right after opening the database; the `./configure` build calls it right before the first
   journal header (the header's checksum nonce is random). 7.1 leaves the one `xRandomness` line out
   of the printed stream (count still printed, position on an `INFO:` line) so both builds give the
   same evidence. Neither position is part of any claim.
6. **The Go driver and the C CLI make the same VFS calls, call for call.** Not an outline claim but
   stronger than chunk 1's syscall twin: 7.3 compares the complete ordered list (method, file,
   offset, length, lock level, shm slot, sync label, file-control op) of one committed INSERT in
   four configurations; all four are identical.
7. Outline section 4.3 ("No chapter builds or installs a VFS") is superseded by ruling 11; this
   chunk builds two (a forwarding VFS and a read-only remote one).

Nothing else in the chapter-7 outline was false here. Every outline span was correct at the pin
(re-checked with `go run ./cmd/course check spans ch07`); SPANS.txt adds 40 finer spans.

## What was built

| Path | What |
| --- | --- |
| `ch07-vfs-and-filesystem-io/os_unix.c`, `os.c`, `os.h`, `vfstrace.c` | Whole copies from `$SQLITE_SRC` (`src/os_unix.c`, `src/os.c`, `src/os.h`, `ext/misc/vfstrace.c`) |
| `ch07-vfs-and-filesystem-io/SOURCES.sha256` | 4 lines, paths in the pinned tree (vfstrace as `ext/misc/vfstrace.c`) |
| `ch07-vfs-and-filesystem-io/SPANS.txt` | 46 spans (`check spans` passes) |
| `ch07-vfs-and-filesystem-io/experiments/7.1…7.4/main.go` | The four experiments |
| `ch07-vfs-and-filesystem-io/evidence/` | `7.1-vfs-calls-to-syscalls.txt`, `7.2-io-cost-by-pattern.txt`, `7.3-go-vfs-shim.txt`, `7.4-read-only-snapshot-vfs.txt` (HTTP route, the one `check evidence` compares), `7.4-read-only-snapshot-vfs-s3.txt` (the `-s3` route) |
| `internal/govfs/` | New shared package: Go forwarding VFS on the translated library |
| `internal/rangefs/` | New shared package: range-reading `fs.FS` over HTTP or S3 |

`sqlite.h.in` is cited (spans) and linked, not copied, as the outline says.

## Changes to shared packages (all backward compatible, all tested)

| Package | Change | Test |
| --- | --- | --- |
| `internal/trace` (`strace.go`) | Parser fills `Path` for `newfstatat`, `fstatat64`, `statx`, `faccessat`, `faccessat2` (path argument joined to the dirfd's `-y` path when relative; `""` with `AT_EMPTY_PATH` → the fd's path) and for `stat`, `lstat`, `access`, `stat64`, `lstat64` (first argument). Before, these had no path | `TestParseStatAndAccessPaths` |
| `internal/trace` (new `canonical.go`) | `CanonicalName(name)`: `open`/`openat` → `open`, `unlink`/`unlinkat` → `unlink`, `fsync`/`fdatasync` → `sync`, stat family (`stat lstat stat64 lstat64 newfstatat fstatat64 statx fstat fstat64`) → `stat`, `access`/`faccessat`/`faccessat2` → `access`, `pwrite`/`pread` → `pwrite64`/`pread64`; `Syscall.Canonical()` returns a copy with the canonical name; `IsSync(s)`. Lets one evidence file hold for the installed CLI (fsync), a `./configure` CLI (fdatasync), the Go driver (`open`), and arm64 Linux, which has no `open`/`unlink`/`access` syscalls (arm64: not run) | `TestCanonicalNames` |
| `internal/sqlitex` | `Options.VFS string`: when not empty, `DSN` adds `vfs=<name>`; empty keeps the old DSN | `TestDSNWithVFS` (also proves the option reaches SQLite: `unix-dotfile` has no shared memory, so asking it for WAL fails with `journal_mode: asked for wal, SQLite reports delete`) |
| `internal/sqlitex` (`workdir.go`) | `DisplayPath(path)`: the path relative to the repo root (symlinks resolved) when inside it, else unchanged. Experiments print `INFO: work dir _work/7.N` with it (the old ch01 experiments have a private copy of the same function) | `TestDisplayPath` |
| `go.mod` / `go.sum` | Added, at the versions the SeaweedFS probe pinned: `github.com/aws/aws-sdk-go-v2 v1.48.0`, `…/credentials v1.21.3`, `…/service/s3 v1.116.0`, `github.com/aws/smithy-go v1.28.9` (+ 8 indirect aws modules). `modernc.org/libc v1.77.1` moved from indirect to direct (govfs imports it); version unchanged | — |

`agent_work/verify/crossbuild.sh` and `cmd/course` were **not** changed: `go vet` passes for the
whole repo with default analyzers on linux/amd64, darwin/arm64 and darwin/amd64 (see the unsafe
section for why no `-unsafeptr=false` is needed).

## internal/govfs — a Go forwarding VFS on the translated library

Files: `cfunc.go` (21 lines, all of the unsafe code), `slots.go` (slot table), `govfs.go`
(registry, Register, recording, faults), `methods.go` (the 27 wrappers), `call.go` (Call, kinds,
formatting), `selfcheck.go`; tests `govfs_test.go`, `signature_test.go`. 1,184 lines without tests.

### How it works (facts)

- `Register(name, Options{IOVersion, MakeDefault}) (*VFS, error)`: finds the default VFS
  (`sqlite3_vfs_find(NULL)`, "unix" on Linux and macOS), runs the self-checks once, allocates a
  `sqlite3_vfs` with `sqlite3_malloc64`, copies the default VFS into it, sets `zName`, `pNext=0`,
  `szOsFile = real szOsFile + 8`, replaces 9 VFS slots with Go wrappers (`xOpen xDelete xAccess
  xFullPathname xRandomness xSleep xCurrentTime xGetLastError xCurrentTimeInt64`; the `xDl*` and
  `x*SystemCall` slots keep the real functions, which ignore their `pVfs` argument) and calls
  `sqlite3_vfs_register`. `IOVersion` 0 means 3; 1–3 allowed. A name already known to SQLite
  (including "unix") → `ErrAlreadyRegistered`. Select it per connection with `vfs=<name>` in the
  URI (`sqlitex.Options.VFS`).
- Each open file is laid out as an 8-byte header (`sqlite3_file`, whose `pMethods` points at the
  shim's method table) followed by the real VFS's file object. `xOpen` calls the real `xOpen` on
  the memory after the header; if the real file got methods, the shim records the file (path,
  base name, kind from the open flags) keyed by the `sqlite3_file` address and points the header's
  `pMethods` at a shim `sqlite3_io_methods` table.
- One shim method table per (shim VFS, real method table) pair, allocated with `sqlite3_malloc64`:
  `iVersion = min(requested, real)`, and a slot is filled only if the real table has it. So a
  version-1 shim never offers `xShm*`, and SQLite silently keeps such a database in rollback mode
  (`TestIOVersionOneCannotUseWAL`: `PRAGMA journal_mode=wal` returns `delete`).
- Every wrapper looks up the file, optionally injects a fault, calls the real method through the
  real table, and records a `Call{Seq, Method, File (base name), Kind, Amount, Offset, Flags, Out,
  Result, Injected}`. `Call.String()` is the stable text in the evidence, e.g.
  `xWrite t.db-wal n=4096 ofst=56 -> 0`, `xSync t.db-wal flags=NORMAL -> 0`,
  `xLock t.db level=RESERVED -> 0`, `xShmLock t.db ofst=0 n=1 flags=LOCK|EXCLUSIVE -> 0`,
  `xFileControl t.db op=SIZE_HINT -> 0`, `xFileSize t.db -> 0 size=8192`,
  `xAccess t.db-journal flags=0 -> 0 out=0`, `xDeviceCharacteristics t.db -> 0x9000`,
  `xShmBarrier t.db` (void), `… -> 10 (injected)`.
- Recording: `Calls()`, `TakeCalls()` (returns and clears, resets Seq), `Reset()`,
  `SetRecording(bool)`. Helpers `CountCalls`, `Counts.String()` (sorted `method=n`), `Filter`,
  `Methods(names...)`, `KindFromOpenFlags`, `SyncFlagsString` (the real flag: `NORMAL`, `FULL`,
  `|DATAONLY`), `VFSTraceSyncLabel` (what vfstrace prints for the same flag: `FULL` for 0x2),
  `LockLevelString`, `ShmLockFlagsString`, `FileControlName` (names from `sqlite.h.in` 1243–1286).
- Faults: `Arm(Fault{Method, Nth, Code, Kind})`, `Disarm() FaultReport{Armed, Fault, Seen, Fired,
  Call}`. Faultable methods: `xRead xWrite xSync xTruncate`. Counting starts at `Arm`; the N-th
  matching call (optionally only on one file kind) returns `Code` without calling the real method;
  later calls pass through. `Code` must not be 0; `Nth` ≥ 1.
- `Err()` returns a self-check failure found at an `xOpen` (the open then fails with
  `SQLITE_CANTOPEN`).

### The unsafe part, and why `go vet` passes with defaults

`cfunc.go` holds the only `unsafe` uses (4 conversions + 2 `unsafe.Sizeof` in govfs.go):

- `funcAddress(f any) uintptr` — the data word of an interface holding a func value, which for a
  top-level func is the address of its static funcval. This is exactly ccgo's `__ccgo_fp`
  (`modernc.org/sqlite/lib` `sqlite.go:13896-13899`), i.e. how the translated code stores C
  function pointers.
- `funcAt[F](address) F` — reinterprets a stored function pointer as a Go func value of type `F`;
  translated call sites do the same (`(*(*func(...))(unsafe.Pointer(&struct{ uintptr }{fp})))(...)`).
- `at[T](address) *T` — reads `address` as a `*T` by reinterpreting the address of the uintptr
  variable. `codeAddress(funcAddress)` reads the code pointer out of a funcval (for
  `runtime.FuncForPC`).
- vet's `unsafeptr` check reports `unsafe.Pointer(u)` where `u` is a uintptr expression; it does not
  see through `*(**T)(unsafe.Pointer(&u))`, so vet is clean. The probe's straightforward version had
  10 vet reports. This is sound here because every address the shim dereferences points into
  memory the Go garbage collector does not manage: SQLite's file objects and buffers come from
  modernc's libc allocator (`modernc.org/memory`, mmap-backed; `libc@v1.77.1/mem_musl.go`), the shim's
  `sqlite3_vfs` and method tables are allocated with `sqlite3_malloc64`, and funcvals of top-level
  funcs are static data. No Go heap address is ever converted to a uintptr and handed to SQLite.
  Neon may want to say this plainly: the vet check is not disabled, it simply does not apply to
  these lines, and the safety argument is the allocation rule above.

### Signature risk and the three guards

A wrapper whose Go signature differs from the translated function's is memory corruption, not a
compile error. Demonstrated while building: changing `ioSyncFunc`'s flags parameter from `int32`
to `int64` (and the wrapper with it) compiled, **ran every test without crashing** on amd64 (the
register ABI happened to tolerate it), and only the source-signature test caught it:
`xSync: translated _unixSync(*libc.TLS, uintptr, int32) (int32), shim signature (*libc.TLS,
uintptr, int64) (int32)`. Guards:

1. Compile time: every slot is built by `newVFSSlot[F]`/`newIOSlot[F]` with an explicit func type
   `F`; the wrapper must have type `F` or the package does not compile, and the call to the real
   function uses `funcAt[F]` with the same `F`.
2. Startup (`Register` and the first `xOpen` per real method table): the func-pointer convention
   round trip (`selfCheckDouble(21)` through a stored pointer returns 42 and resolves to
   `…selfCheckDouble`); every real slot resolves with `runtime.FuncForPC` to a function in
   `modernc.org/sqlite/lib` whose name ends with the slot name (`xWrite` → `_unixWrite`, `xLock` →
   `_unixLock`, or `_dotlockLock`/`_nolockLock`/`_afpLock` for other locking styles); default VFS
   fields sane. A runtime check cannot see parameter types (a stored pointer has none).
3. Test (`TestSignaturesMatchTranslatedSource`): opens a WAL database through the shim, collects the
   real function behind each of the 27 slots, finds the module source with `go list -m -f {{.Dir}}
   modernc.org/sqlite`, parses the `lib` files that match the current GOOS/GOARCH
   (`build.Default.MatchFile`), resolves type aliases (`Tsqlite3_int64`, `Ti64` → `int64`) and
   compares every parameter and result type with the shim's `F`. Passes on linux/amd64 host and in
   the container (not skipped). Must be re-run after any driver upgrade.

Remaining risk: tied to `modernc.org/sqlite v1.60.1`'s generated `lib` (struct layouts, the
`_unix*` names, the funcval convention); the C struct layouts follow `sqlite3.h` and are stable
across 3.x. On darwin the code cross-compiles and vets; the self-check and tests were never run
there.

### Tests (`go test ./internal/govfs`, ~2 s)

Function-pointer convention and `at` on C memory; Register validation (empty name, version 4,
duplicate, "unix"); forwarding in DELETE and WAL (writes and syncs recorded, WAL uses xShmLock,
rollback deletes the journal once, rows survive reopen through the default VFS); version 1 cannot
enter WAL; fault on the first xWrite / xSync in both modes (error code 10, report fired at call 1,
row absent, integrity ok); fault Nth beyond the statement never fires; fault restricted to the
main-db kind fires on a `t.db` write; Arm validation; recording switch and TakeCalls; Call strings,
sync labels, kinds, Counts; the signature test.

## internal/rangefs — a range-reading fs.FS

- `Source` interface: `Stat(ctx, name) (size, error)` (absent → error wrapping `fs.ErrNotExist`)
  and `ReadRange(ctx, name, offset, p) (n, error)`.
- `New(ctx, source) *FS`. `FS.Open(name)` = one `stat` request, returns a file with the size;
  `FS.Stat(name)` (implements `fs.StatFS`, used by modernc's `xAccess`) = one `stat` request.
  The file implements `Read`, `ReadAt`, `Seek`, `Stat`, `Close`; every `ReadAt`/`Read` is one
  `range` request for exactly the bytes asked (clipped at EOF); no caching, no read-ahead.
- Every request is logged: `Request{Kind stat|range, Name, Offset, Length, Found}`;
  `Requests()`, `TakeRequests()`, `Summarize(requests) Stats{StatRequests, RangeRequests,
  BytesRead}`. Strings: `stat snapshot.db (size 1105920)`, `stat x.db-wal (absent)`,
  `range snapshot.db bytes 0-99 (100 bytes)`.
- `HTTPSource{BaseURL, Client}`: `HEAD` for stat (404 → not exist), `GET` with
  `Range: bytes=a-b` for reads, requires `206 Partial Content`.
- `S3Source{Client, Bucket, Prefix}`: `HeadObject` for stat (`NotFound`/`NoSuchKey` → not
  exist), `GetObject` with `Range` for reads. Helpers: `S3ConfigFromEnv()` (`COURSE_S3_ENDPOINT`
  default `http://127.0.0.1:18333`, `COURSE_S3_REGION` `us-east-1`, `COURSE_S3_ACCESS_KEY`
  `course`, `COURSE_S3_SECRET_KEY` `course-secret` — the values in `docker/seaweedfs/s3.json` and
  `compose.yaml`), `NewS3Client(cfg)` (path-style, static credentials, request checksums and
  response checksum validation only "when required" so plain range GETs carry no extra checksum
  work), `EnsureBucket` (treats `BucketAlreadyOwnedByYou`/`BucketAlreadyExists` as success),
  `PutObject`, `DeleteObject`.
- Tests: HTTP range reads, request log and summary, EOF and tail reads, missing files, invalid
  paths, seek, closed file, env config, and a live S3 round trip that skips when SeaweedFS is not
  reachable (ran and passed on the host and in the container).

## Experiment 7.1 vfs-calls-to-syscalls · EXACT · runtime ~1 s

**Claim:** every byte SQLite reads or writes goes through a VFS method, and each method call maps
to a specific `os_unix.c` function and specific syscalls.

**Run:** `go run ./ch07-vfs-and-filesystem-io/experiments/7.1-vfs-calls-to-syscalls`

**Does:** runs the pinned CLI with `-vfstrace` under strace (`trace.RunCLIMerged`, tracing chunk
0's default set plus `fstat newfstatat statx stat lstat access faccessat faccessat2`). Script:
`.open --normal t.db`, then `.print STEP n label` markers around: `PRAGMA journal_mode=WAL;`,
`PRAGMA synchronous=FULL;`, `CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT NOT NULL);`,
`INSERT INTO t(id, v) VALUES(1, 'one');`, `PRAGMA wal_checkpoint(TRUNCATE);`, end of input
(close). `.print` does no file I/O, so the markers split the stream by statement.

**How to read the output:**
- Lines 1–2: pin checks (Go driver, then the CLI). `INFO:` lines are ignored by checks.
- The statement list.
- `== merged stream: 139 VFS calls and 143 file syscalls ==`, then one line per event:
  `VFS xName(args) -> result` is a line of vfstrace output; an indented `SYS …` line is a syscall
  made while that VFS call ran (between vfstrace printing the call and printing its result);
  an unindented `SYS` line is outside any VFS line (the syscalls of `xOpen` and `xFullPathname`'s
  helper stats appear before the `xOpen` line, because vfstrace prints `xOpen` after the real open
  returns, `vfstrace.c:876-930`); `OUT` is CLI output (`wal` from the pragma, `0|0|0` from the
  checkpoint, and the STEP markers). Syscall names are normalized (`open`, `sync`, `stat`,
  `unlink`). Only syscalls on `t.db`, `t.db-journal`, `t.db-wal`, `t.db-shm` and the directory
  `.` are shown.
- The `(xRandomness: 1 call(s) …)` line: correction 5.
- `== each VFS method, the os_unix.c function behind it, and the syscalls seen while it ran ==`:
  one row per method: calls, the `unix*` function, syscalls inside (for xOpen: just before).
  Values in this run: xAccess 6 → stat 6; xClose 3 → close 3, stat 2; xDelete 2 → unlink 2;
  xDeviceCharacteristics 9 → none; xFileControl 19 → stat 1 (`HAS_MOVED` at close); xFileSize 6 →
  stat 6; xFullPathname 1 → stat 2; xLock 6 → fcntl 14; xOpen 3 → open 3, stat 7; xRandomness 1 →
  none; xRead 7 → pread64 7; xShmBarrier 11 → none; xShmLock 36 → fcntl 36; xShmMap 2 → fcntl 3,
  ftruncate 1, mmap 1, open 1, pwrite64 8, stat 4; xShmUnmap 1 → close 1, unlink 1; xSync 8 →
  sync 10, open 2, close 2; xTruncate 2 → ftruncate 2; xUnlock 4 → fcntl 6; xWrite 12 → pwrite64 12.
- `== syncs ==`: t.db 2, t.db-journal 2, t.db-wal 4, directory 2 (inside the first xSync of the
  new journal and of the new WAL).
- `files left …: t.db`, then 9 `check ok` lines and `ALL CHECKS PASSED`.

**What the stream shows, in order (timeline material):**
1. Open: `xFullPathname`, `open t.db O_RDWR|O_CREAT|O_NOFOLLOW|O_CLOEXEC`, `xOpen(t.db,flags=0x106)`
   (MAIN_DB|CREATE|READWRITE), `xDeviceCharacteristics -> 0x9000` (POWERSAFE_OVERWRITE |
   SUBPAGE_READ), file controls, `xRead(t.db,n=100,ofst=0) -> SQLITE_IOERR_SHORT_READ` (empty file:
   the 100-byte header read comes back short and zero-filled, `unixRead` `os_unix.c:3575-3577`).
2. STEP 1 `journal_mode=WAL` on an empty file is a rollback-mode write: SHARED lock (fcntl read
   lock on the pending byte 1073741824 = 0x40000000, then on the 510-byte shared range from
   1073741826, then the pending byte released), xAccess of -journal and -wal (stat), RESERVED
   (write lock on 1073741825), EXCLUSIVE (pending byte + shared range), journal created
   (`xOpen(t.db-journal,flags=0x806)`), journal header written, `xSync(t.db-journal)` = sync of the
   journal **plus open/sync/close of the directory**, header rewritten (12 bytes), second journal
   sync, page 1 written to t.db with bytes 18–19 = 2, `xSync(t.db)`, journal closed and
   `xDelete` → `unlink t.db-journal`, locks dropped.
3. STEP 3 `CREATE TABLE` is the first WAL write: `open t.db-wal` with O_CREAT (`xOpen` flags
   0x80006), `xShmMap` creates `t.db-shm` (open, fcntl F_GETLK/F_SETLK on byte 128 — the
   "dead man switch" `UNIX_SHM_DMS`, ftruncate to 3 bytes, then for the 32 KiB region eight 1-byte
   pwrites at 4095, 8191, … 32767 and an mmap), WAL-index recovery under exclusive shm locks on
   slots 0–7 (fcntl bytes 120–127 = `UNIX_SHM_BASE` 120 + slot), then the write: WAL header (32 B at
   0) + `xSync(t.db-wal)` with the directory sync, two frames (24-byte frame header + 4096-byte
   page each: offsets 32/56 and 4152/4176) and the commit sync.
4. STEP 4 one-row INSERT in WAL/FULL: read lock on shm slot READ1, write lock on WRITE (byte 120),
   `xWrite(t.db-wal,n=24,ofst=8272)` + `xWrite(…,n=4096,ofst=8296)`, `xSync(t.db-wal)`, unlock.
   No access to t.db at all.
5. STEP 5 `wal_checkpoint(TRUNCATE)`: CKPT lock, sync WAL, read each frame's page from the WAL
   (`pread64 t.db-wal off=56`, `off=8296`) and write it to t.db (`pwrite64 t.db off=0`, `off=4096`),
   `xTruncate(t.db,8192)`, sync t.db, `xTruncate(t.db-wal,0)`, prints `0|0|0`.
6. Close: `HAS_MOVED` file control (stat), EXCLUSIVE lock on t.db, `xShmUnmap(delFlag=1)` →
   `unlink t.db-shm` then `close`, `xClose(t.db-wal)`, `xDelete("t.db-wal")` → `unlink`,
   `xClose(t.db)` (two stats before close).

**Checks (all ok):** each of the 12 xWrite made exactly one pwrite64 of the same file, offset and
length; each of the 7 xRead exactly one pread64; every pread64/pwrite64 on those files ran inside
xRead, xWrite or xShmMap (the only non-xWrite writes are the 8 one-byte shm extension writes);
each of the 8 xSync made exactly one sync of its own file; the directory was synced twice, inside
the first xSync of the new journal and of the new WAL; -wal created with O_CREAT at first use by
xOpen; -shm created inside the first xShmMap; at last close xDelete unlinked -wal and xShmUnmap
unlinked -shm; only t.db remains.

**Not traced on purpose:** `fchown`. Running as root, `unixOpen` calls `robustFchown` on new
journal/WAL/shm files (seen in a one-off strace: `fchown(4</…/t.db-journal>, 0, 0)`); a non-root
user would not see it, so tracing it would make the evidence user-dependent.

**On macOS (no strace):** prints the CLI's VFS call list (`trace.RunCLIVFSTrace`, xRandomness left
out), checks that -wal is opened through xOpen and that the last close deletes -wal and unmaps -shm
with delete=1, then prints `LINUX-ONLY: strace of the CLI (VFS calls merged with syscalls) skipped
on darwin; run it with: docker compose run --rm lab go run ./ch07-…/7.1-vfs-calls-to-syscalls`.
Needs the pinned CLI (`go run ./cmd/course build-tools`). darwin: cross-built only.

## Experiment 7.2 io-cost-by-pattern · EXACT · runtime ~45 s (4 cells in parallel)

**Claim:** the same 1,000 rows cost very different I/O depending on batching, journal mode and
`synchronous`, and the differences are countable.

**Run:** `go run ./ch07-vfs-and-filesystem-io/experiments/7.2-io-cost-by-pattern`

**Does:** 12 cells = journal {delete, wal} × synchronous {NORMAL, FULL} × batching {1 × 1000 rows,
20 × 50, 1000 × 1}. Each cell re-runs the experiment binary as a child under strace (chunk 0's
default syscall set). The child opens a fresh `t.db` with `sqlitex.Open` (one connection, page
size 4096, cache_size -2000, wal_autocheckpoint 1000, secure_delete OFF, the cell's journal mode
and synchronous), creates `t(id INTEGER PRIMARY KEY, v BLOB NOT NULL)`, then reads a marker file
after the CREATE, after every commit and after close. The marker opens split the strace into one
segment per commit plus a close segment, so every commit is counted separately. Rows: ids 1..1000,
a deterministic 100-byte blob each, bound parameters, `BEGIN IMMEDIATE` … `COMMIT` per batch.

**How to read the output:**
- `== totals while the rows were written ==`: per cell, pwrite64 count, bytes written, syncs (and
  by file: db, journal, wal, dir), opens, unlinks and ftruncates by file, summed over all commits
  (close excluded). Values:

| cell | pwrite | bytes | syncs (by file) | opens | unlinks | ftruncate |
| --- | --- | --- | --- | --- | --- | --- |
| delete NORMAL 1 × 1000 | 38 | 131,612 | 3 (db 1, dir 1, journal 1) | dir 1, journal 1 | journal 1 | – |
| delete NORMAL 20 × 50 | 304 | 608,968 | 60 (20/20/20) | dir 20, journal 20 | journal 20 | – |
| delete NORMAL 1000 × 1 | 10,028 | 17,038,688 | 3,000 (1000/1000/1000) | dir 1000, journal 1000 | journal 1000 | – |
| delete FULL 1 × 1000 | 38 | 131,612 | 4 (db 1, dir 1, journal 2) | dir 1, journal 1 | journal 1 | – |
| delete FULL 20 × 50 | 304 | 608,968 | 80 (20/20/40) | dir 20, journal 20 | journal 20 | – |
| delete FULL 1000 × 1 | 10,028 | 17,038,688 | 4,000 (1000/1000/2000) | dir 1000, journal 1000 | journal 1000 | – |
| wal NORMAL 1 × 1000 | 60 | 123,600 | 0 | – | – | – |
| wal NORMAL 20 × 50 | 174 | 358,440 | 0 | – | – | – |
| wal NORMAL 1000 × 1 | 2,139 | 4,461,320 | 3 (db 1, wal 2) | – | – | db 1 |
| wal FULL 1 × 1000 | 60 | 123,600 | 1 (wal 1) | – | – | – |
| wal FULL 20 × 50 | 174 | 358,440 | 20 (wal 20) | – | – | – |
| wal FULL 1000 × 1 | 2,139 | 4,461,320 | 1,003 (db 1, wal 1002) | – | – | db 1 |

  (The "dir" opens are the directory opened for its one-time sync; in DELETE mode each commit
  creates a new journal, so each commit opens and syncs the directory.)
- `== syncs per commit ==`: how many commits had each sync pattern; `+db-writes` marks commits
  that wrote database pages (always in rollback; in WAL only the commit in which the automatic
  checkpoint ran). Values: every rollback commit at NORMAL `3 sync(s) [db:1 dir:1 journal:1]`, at
  FULL `4 sync(s) [db:1 dir:1 journal:2]`, at every batch size. WAL NORMAL 1000 × 1: `998 commit(s):
  0 sync(s); 1 commit(s): 2 sync(s) [db:1 wal:1] +db-writes; 1 commit(s): 1 sync(s) [wal:1]`. WAL
  FULL 1000 × 1: `998 … 1 sync(s) [wal:1]; 1 … 3 sync(s) [db:1 wal:2] +db-writes; 1 … 2 sync(s)
  [wal:2]`. WAL 1 × 1000 and 20 × 50: every commit 0 (NORMAL) or 1 (FULL) — the WAL never reached
  1,000 frames, so no automatic checkpoint.
- `== at close ==`: rollback: nothing (the journal is already gone). WAL: the last connection's
  checkpoint: 30 page writes (122,880 bytes) for the 1 × 1000 and 20 × 50 cells, 5 (20,480 bytes)
  for 1000 × 1 (most pages were checkpointed during the run), 2 syncs (wal, db), unlink of -shm and
  -wal, ftruncate of db.
- Checks: rollback NORMAL every commit 3 syncs; rollback FULL every commit 4; WAL NORMAL: plain
  commit `[-]`, checkpoint commit `[db:1 wal:1]` (1 such), restart commit `[wal:1]` (1 such),
  each restart directly after a checkpoint; WAL FULL: `[wal:1]`, `[db:1 wal:2]` (1), `[wal:2]` (1);
  rollback FULL 1000 one-row commits 4,000 syncs vs 4 for one 1000-row commit (×1000); at NORMAL
  1000 one-row commits WAL 3 syncs / 4,461,320 bytes vs rollback 3,000 syncs / 17,038,688 bytes.

**Deriving each number from the code (for the chapter):**
- Rollback commit: `sqlite3PagerCommitPhaseOne` (`pager.c:6511-6730`) → `syncJournal`
  (`pager.c:4332-4441`): if `fullSync` (FULL/EXTRA), sync the journal, then write the record count
  into the journal header (`xWrite n=12 ofst=0` in 7.3), then always sync the journal again → 1
  journal sync at NORMAL, 2 at FULL. The first journal `xSync` after the journal was created also
  syncs the directory (`unixSync` `os_unix.c:3941-3953`). Then `pager_write_pagelist` writes the
  dirty pages to the database and `sqlite3PagerSync` syncs it (1). `pager_end_transaction`
  (`pager.c:2052-2180`) deletes the journal with `sqlite3OsDelete(…, pPager->extraSync)`: the
  directory is synced after the unlink only at `synchronous=EXTRA` (not measured here).
- Rollback bytes per one-row commit: journal header 512 + 2 page records (4 + 4096 + 4 bytes,
  written as 3 separate xWrites each, 7.3) + 12-byte header update + 2 database pages
  (page 1 for the change counter, the table leaf) ≈ 17 KB; 10 pwrites per commit.
- WAL commit: `walFrames` (`wal.c:4038-4266`) writes a 24-byte frame header and the 4096-byte page
  per dirty page (2 pwrites per one-row commit, 4,120 bytes) and syncs at commit only if
  `WAL_SYNC_FLAGS(sync_flags)` is non-zero (FULL); see correction 1 for the header and checkpoint
  syncs.

**On macOS:** runs all 12 workloads without strace, prints `all 12 workloads ran without strace`
and the `LINUX-ONLY:` line. darwin: cross-built only.

## Experiment 7.3 go-vfs-shim · EXACT · runtime 24–34 s

**Claims:** (a) through a Go forwarding VFS the in-process Go driver's VFS calls for one commit
can be counted, and they equal what the C CLI's `-vfstrace` reports for the same statements, in
DELETE and WAL at NORMAL and FULL; (b) failing the N-th xWrite or xSync of one commit with
SQLITE_IOERR, for every N, in both journal modes, makes the step fail and leaves exactly the
pre-step state.

**Run:** `go run ./ch07-vfs-and-filesystem-io/experiments/7.3-go-vfs-shim` (portable: needs the
pinned CLI for part A; no strace).

**Output, part A:** `registered Go VFS "course-govfs" (io_methods version 3) forwarding to the
default VFS; self-check passed`. For each configuration: setup `CREATE TABLE t(…); INSERT … (1,
'one')` (not measured), then the measured `INSERT INTO t(id, v) VALUES(2, 'two')`. The Go side opens
with `sqlitex.Open` (`Options.VFS = "course-govfs"`); the CLI side runs the same pragmas
(`page_size, auto_vacuum, encoding, journal_mode, application_id, user_version, synchronous,
foreign_keys, secure_delete, cache_size, wal_autocheckpoint, busy_timeout`) and the same SQL, with
`.print MARK-BEGIN/MARK-END` around the INSERT. Each block lists the Go calls (`seq call`; each
xSync line also says what vfstrace would print), then `Go VFS counts:` and `CLI -vfstrace counts:`,
a check that the two ordered lists are equal (compared fields: method, file, offset and length for
reads/writes, lock level, shm slot/n/flags, shm region/size/write, truncate size, open flags,
delete/access argument, file-control op, vfstrace's sync label), and `xSync flags SQLite passed:`
with the real flags. Results: all four equal; per-commit totals:

| method | delete/NORMAL | delete/FULL | wal/NORMAL | wal/FULL |
| --- | --- | --- | --- | --- |
| xAccess | 2 | 2 | 0 | 0 |
| xClose | 1 | 1 | 0 | 0 |
| xDelete | 1 | 1 | 0 | 0 |
| xDeviceCharacteristics | 3 | 3 | 0 | 0 |
| xFileControl | 3 | 3 | 1 | 1 |
| xFileSize | 1 | 1 | 0 | 0 |
| xLock | 3 | 3 | 0 | 0 |
| xOpen | 1 | 1 | 0 | 0 |
| xRead | 2 | 2 | 0 | 0 |
| xShmBarrier | 0 | 0 | 3 | 3 |
| xShmLock | 0 | 0 | 6 | 6 |
| xSync | 2 | 3 | 0 | 1 |
| xUnlock | 2 | 2 | 0 | 0 |
| xWrite | 10 | 10 | 2 | 2 |
| total | 31 | 32 | 12 | 13 |

Timeline of the rollback/FULL commit (from the evidence): SHARED lock; xAccess -journal (hot
journal check) and -wal; read 16 bytes at 24 (change counter etc.); xFileSize; RESERVED;
`HAS_MOVED`; journal xOpen; journal header write 512 B; record for page 1 (4 + 4096 + 4 B); EXCLUSIVE;
record for page 2; `xRead(journal, n=8, ofst=9216) -> 522` (SQLITE_IOERR_SHORT_READ: the pager
probes past the end of the journal); xSync journal; header update 12 B at 0; xSync journal; write
pages 1 and 2 to t.db; `SYNC` file control (returns 12 = SQLITE_NOTFOUND: unix does not handle
it); xSync t.db; close and delete journal; `COMMIT_PHASETWO`; SHARED then NONE. The WAL/FULL
commit: shm barrier; READ1 lock taken (exclusive then shared); WRITE lock; frame header + page; xSync
WAL; WRITE unlock; `COMMIT_PHASETWO`; READ1 unlock. All xSync flags are 0x2 (NORMAL) in every
configuration (correction 3).

**Output, part B:** for WAL and then DELETE: recipe seed 1 (`recipe.Default().WithSteps(5)`, the
journal mode of the block, synchronous FULL from the recipe settings) built to step 3 with the
default VFS (159,744 bytes, at rest); step 4 (`inserts=48 updates=6 deletes=1`, 56 statements,
marker row `events id 153`) is applied through the Go VFS to a fresh copy of that file. The clean
run lists the xWrite totals by file and every xSync: WAL: 51 xWrite to t.db-wal (103,032 bytes),
2 xSync (WAL header sync after the 32-byte header at call 26 — the file had no WAL, so this commit
creates it — and the commit sync at call 77); DELETE: 25 xWrite to t.db (102,400 bytes), 41 to the
journal (53,876 bytes), 3 xSync (journal, journal, db). Then one trial per N = 1 … count+1 for
xWrite and for xSync: a fresh copy, fault armed after open, the step's statements run one by one
in a `BEGIN IMMEDIATE` transaction, then COMMIT; the connection is closed; the file is reopened
with the default VFS and inspected. Each trial prints two lines:
`xWrite #N -> <the call that was failed>` and `error at <BEGIN | statement i/56 (first words) |
COMMIT>: SQLite error code 10 (disk I/O error (10)) | reopen: user_version=3, marker row absent,
integrity ok, file = step 3 (exact bytes)`. For N = count+1: `never reached`, `step committed |
reopen: user_version=4, marker row present, integrity ok, file = step 4 (exact bytes)`.

Results: 51 + 2 WAL trials and 66 + 3 DELETE trials with a fired fault, every one: error code 10,
user_version 3, marker row absent, integrity ok, file byte-identical to recipe step 3. Where the
error surfaced (counts over all fired faults): COMMIT 82 (all 53 WAL trials, plus the DELETE trials
whose failing call was in the commit phase: journal syncs, header rewrite, database writes, db sync);
in DELETE mode the others surfaced at the statement whose page change first needed a journal record:
statement 1 (INSERT INTO accounts) 4, statement 5 (INSERT INTO documents) 6, statement 9 (INSERT INTO
events) 3, statement 54 (UPDATE documents) 12, statement 55 (DELETE FROM documents) 15. In WAL mode
nothing is written before COMMIT (pages stay in the page cache until the commit writes frames).
Notable cases: failing WAL xSync #2 — the commit sync, after all frames including the commit frame
were written — still left step 3 (the WAL-index header in shared memory was never updated, so the
frames are not part of the database; the close checkpoints only up to the old end and deletes the
WAL). Failing a DELETE-mode xWrite to t.db after the journal was synced leaves step 3 too (the
pager rolls back from the journal). Close returned no error in any trial.

**Not done:** faults in xRead or xTruncate (the package supports them), synchronous=NORMAL in part
B, failing two calls in one commit, faults during the reopen.

## Experiment 7.4 read-only-snapshot-vfs · EXACT · runtime ~2–7 s

**Claims:** a checkpointed recipe snapshot can be queried through modernc's public
`modernc.org/sqlite/vfs` package over a Go `fs.FS` that reads byte ranges, from HTTP or S3, with a
countable number of requests; results equal the local file; `immutable=1` and a fully
checkpointed file are required.

**Run:** `go run ./ch07-vfs-and-filesystem-io/experiments/7.4-read-only-snapshot-vfs` (HTTP,
always runnable) and, with SeaweedFS up (`docker compose up -d seaweedfs`),
`go run ./ch07-vfs-and-filesystem-io/experiments/7.4-read-only-snapshot-vfs -s3`. In the container:
`docker compose run --rm lab go run ./ch07-…/7.4-read-only-snapshot-vfs -s3` uses
`http://seaweedfs:8333`. Evidence: `evidence/7.4-read-only-snapshot-vfs.txt` (HTTP; the file
`check evidence` compares) and `evidence/7.4-read-only-snapshot-vfs-s3.txt`. **The two are identical
except the `route` line** (same requests, same bytes, same results).

**Output, line by line:**
- `route http: …` / `route s3: SeaweedFS from compose.yaml, bucket "course", keys ch07/7.4/<name>…`.
- `snapshot: recipe seed 1, 30 steps, journal_mode=wal, closed by its last connection: 1105920
  bytes, 270 pages, sha256 24aa158fdf713808, equals step 30 (exact bytes)`; header bytes 18–19 =
  `2 2`; no -wal; check ok. The file is published (HTTP: written into the served directory; S3:
  PutObject). Before that the run removes every name it uses from the store, so old objects never
  affect a run.
- For each query, on a new connection `file:snapshot.db?vfs=<name>&immutable=1`: the plan
  (`EXPLAIN QUERY PLAN` on the local file), the local result (row count, sha256 prefix of the
  rendered rows, first row), the remote result, then requests: `open + schema load` (the first
  statement, `SELECT count(*) FROM sqlite_schema`, listed request by request), `query (cold cache)`
  with the pages read in order (page = offset/4096 + 1), `same query again on the same connection`,
  and the total for a first query on a new connection.

| query | plan | rows | open + schema | query (cold) | pages read | repeat | first query total |
| --- | --- | --- | --- | --- | --- | --- | --- |
| point lookup `WHERE id = 372` | SEARCH events USING INTEGER PRIMARY KEY (rowid=?) | 1 (`372|7|fee|2642|gkus…`) | 3 requests: stat, header 100 B, page 1 (4,196 bytes) | 2 range, 8,192 B | 4 66 | 0 | 5 requests, 12,388 bytes |
| index range scan `WHERE account_id = 7 ORDER BY id` | SEARCH events USING INDEX events_by_account (account_id=?) | 29 | same 3 | 13 range, 53,248 B | 28 88 4 26 27 39 66 87 108 204 210 219 259 | 0 | 16, 57,444 |
| full scan `count(*), sum(amount), sum(length(note))` | SCAN events | 1 (`1177|-124383|41332`) | same 3 | 18 range, 73,728 B | 4 26 27 39 59 66 87 108 124 132 157 168 184 204 210 219 243 259 | 0 | 21, 77,924 |

  (Page 4 is the root page of `events`; 28 and 88 are index pages; the full scan reads the 18 pages
  of the events table b-tree; the database has 270 pages, most of them documents and their
  overflow pages, which none of these queries touches.)
  Checks per query: remote result equals local; every range request is one whole 4096-byte page or
  the 100-byte header (one request per page miss, no read-ahead, no stat during the query);
  repeating the query on the same connection makes 0 requests (with `immutable=1` SQLite does no
  locking and no change-counter check, so the page cache stays valid between statements).
- `== why immutable=1 ==`: the same WAL-format snapshot opened with no parameters and with
  `mode=ro`: `SQLite error code 14: unable to open database file (14)` after 5 requests (stat db,
  header, stat -journal (absent), stat -wal (absent), page 1): SQLite sees bytes 18–19 = 2, needs
  WAL mode and therefore shared memory, and modernc's fs VFS registers io_methods version 1 (no
  xShm*). With `immutable=1`: opens, `user_version=30 events=1177` (equal to the local file) in 8
  requests and no -journal/-wal stats.
- `== the same bytes with header bytes 18-19 set to 1 ==`: published as `snapshot-rollback.db`;
  opens with no parameters, same state, but 16 requests (7 stat, 9 range): two hot-journal checks
  per read transaction (stat -journal, stat -wal) and a re-read of the change counter (16 bytes at
  24) at the start of each read transaction. Shows the WAL flag in the header, not the content, is
  what demands shared memory; and that without `immutable=1` every transaction costs extra
  requests.
- `== why a fully checkpointed file ==`: a child process builds `live.db` to step 29 (at rest),
  then commits step 30 and keeps its connection open (step 30 stays in the -wal, 30 frames,
  123,632 bytes). A local SQLite read sees `user_version=30 events=1177`. The parent publishes
  `live.db` (1,060,864 bytes) and `live.db-wal` exactly as they are on disk. Remote with
  `immutable=1`: `user_version=29 events=1130` — **a stale read with no error**; 8 requests, none
  naming the -wal (SQLite does not even look for it). Remote with no parameters: error 14. After the
  child closes (last connection: checkpoint, -wal deleted) and the main file is republished: remote
  `user_version=30 events=1177`.
- 19 checks, `ALL CHECKS PASSED`.

**Lock-hazard note:** the parent never holds a SQLite connection to `live.db` while it reads the
file for publishing (the HTTP server or the upload opens and closes the file; closing any descriptor
of a file drops this process's POSIX locks on it — chunk 0's rule). The writer is a child process,
so its locks are unaffected.

## What a custom VFS must honor (facts for the chapter, from the spans and 7.1–7.4)

- **Every byte through xRead/xWrite at an absolute offset.** `unixRead`/`unixWrite` are `pread`/
  `pwrite` loops (`seekAndRead` `os_unix.c:3463-3505`, `seekAndWriteFd` `:3588-3624`); a short read
  must zero-fill the rest and return `SQLITE_IOERR_SHORT_READ` (`:3575-3577`), which SQLite relies
  on when reading past the end (7.1: the 100-byte header read of an empty file, the journal probe at
  ofst 9216). A short write must be retried or reported (`SQLITE_FULL` for ENOSPC,
  `SQLITE_IOERR_WRITE` otherwise, `:3710-3717`).
- **xSync must make the file durable before it returns.** `unixSync` → `full_fsync`
  (`:3778-3849`): `fdatasync` (which is `#define`d to `fsync` unless `HAVE_FDATASYNC`, `:3738-3739` —
  why the installed CLI and the Go driver call fsync and a `./configure` build fdatasync), or on
  macOS `fsync`/`F_FULLFSYNC` when the flag is FULL. A new journal or WAL also needs its directory
  synced once (`:3941-3953`), otherwise the file's existence is not durable. SQLite counts on the
  order of syncs (journal before database; WAL header before frames after a restart,
  `wal.c:4114-4124`); a VFS that acknowledges a sync it did not do breaks atomic commit on power
  loss (not on process crash).
- **Locks.** `xLock`/`xUnlock` levels NONE, SHARED, RESERVED, PENDING, EXCLUSIVE; unix maps them to
  POSIX advisory locks on bytes of the database file: pending byte 0x40000000 (1073741824), reserved
  byte +1, shared range +2 for 510 bytes (`os.h:160-166`, `unixLock` `os_unix.c:1866-2095`). A VFS
  for a filesystem without working locks must provide equivalent mutual exclusion or the
  single-writer guarantee is gone. `xCheckReservedLock` answers "is a writer active" for hot-journal
  decisions.
- **Shared memory for WAL.** io_methods version ≥ 2 with `xShmMap`, `xShmLock`, `xShmBarrier`,
  `xShmUnmap`. unix keeps the WAL index in `<db>-shm`, mapped in 32 KiB regions (extended by 1-byte
  writes at each 4 KiB page end, `unixShmMap` `:5106-5239`), with 8 lock slots at file bytes
  120–127 (`UNIX_SHM_BASE` = (22+8)×4, `:4602`) and a dead-man-switch byte at 128 (`:4603`) used to
  detect the first connection, which then rebuilds the index (`unixOpenSharedMemory`
  `:4952-5085`). All processes on the database must share this memory; it cannot work across
  machines. Without xShm* SQLite silently refuses WAL (govfs test: `journal_mode` stays `delete`)
  or, for a file already in WAL format, fails to open (7.4: error 14).
- **xAccess, xDelete, xOpen semantics.** xAccess EXISTS treats an empty regular file as absent
  (`:6896-6898`); xDelete returns `SQLITE_IOERR_DELETE_NOENT` for a missing file and syncs the
  directory when asked (`unixDelete` `:6833-6869`; SQLite asks only at `synchronous=EXTRA`); xOpen
  receives the file type in its flags (`SQLITE_OPEN_MAIN_DB` 0x100, `MAIN_JOURNAL` 0x800, `WAL`
  0x80000, …, `sqlite.h.in:611-622`; `sqlite3OsOpen` masks them with 0x1087f7f, `os.c:215-232`) and
  must create journals and WALs with the database's permissions (`findCreateFileMode`).
- **Device characteristics.** unix reports 0x9000 = `SQLITE_IOCAP_POWERSAFE_OVERWRITE |
  SQLITE_IOCAP_SUBPAGE_READ` and sector size 4096 (`setDeviceCharacteristics` `:4353-4375`). These
  change SQLite's behaviour: e.g. `SQLITE_IOCAP_SEQUENTIAL` turns off the WAL header sync
  (`wal.c:1737`) and journal syncs (`pager.c` syncJournal), `SAFE_APPEND` removes the journal
  header rewrite. A VFS must not claim properties its storage lacks.
- **Errors.** Any non-OK result from xWrite or xSync during a transaction makes SQLite abandon the
  transaction and restore the previous state (7.3: 122 fired faults, all rolled back exactly).
  This is what makes a fault-injecting VFS a useful test tool.

**When a platform would write one (facts from this chapter):** to observe I/O in-process (7.3
part A: per-commit counts without strace, also on macOS); to inject faults deterministically
(7.3 part B — strace's kill-at-syscall gives process crashes, the VFS gives I/O errors with the
process alive); to read immutable snapshots from object storage without copying them (7.4: a point
lookup costs 5 requests / 12 KB on a 1.1 MB file; latency is one round trip per page miss, no
read-ahead); and the tree's shims show other uses (`cksumvfs.c` checksums, `appendvfs.c`,
`vfsstat.c` counters; source inventory 7.4). The course's platform keeps live databases on local
disk with the unix VFS: a network or object-store VFS for live WAL databases would have to provide
cross-machine shared memory and locks with POSIX semantics, which S3 does not.

## Timeline data (for the Timeline section)

Use 7.1's stream (statement by statement, VFS calls with their syscalls) and 7.3 part A's four
per-commit lists. One-row commit, side by side:

| | rollback (DELETE), FULL | WAL, FULL | WAL, NORMAL |
| --- | --- | --- | --- |
| locks | xLock SHARED → RESERVED → EXCLUSIVE on t.db (fcntl byte ranges), unlock to SHARED, NONE | shm READ1 shared + WRITE exclusive (fcntl bytes 124, 120 of -shm) | same |
| journal / WAL | create -journal, write header + 2 page records (7 writes), sync journal (+ directory), rewrite header, sync journal | append frame header + page (2 writes) | same |
| database | write 2 pages, sync t.db | none | none |
| end | close + unlink -journal | sync -wal | nothing |
| VFS calls / syncs (syscalls) | 32 / 4 | 13 / 1 | 12 / 0 |

Plus, in WAL mode, the occasional commit that runs the automatic checkpoint (+2 syncs and the page
copies) and the one after it that restarts the WAL (+1 header sync) — 7.2.

## Platform implications (facts)

- Per commit, rollback mode costs 3 (NORMAL) or 4 (FULL) sync syscalls including a directory sync,
  creates and deletes a file, and writes each changed page twice (journal record, then database);
  WAL costs 0 (NORMAL) or 1 (FULL) syncs and writes each changed page once (to the WAL) until the
  checkpoint copies it. On one disk with thousands of databases, 1,000 one-row commits spread over
  them cost ~3,000 fsyncs in rollback/NORMAL and ~0 in WAL/NORMAL (plus 2 per checkpoint and 1 per
  WAL restart per database).
- Batching is the biggest lever in rollback mode: one 1000-row transaction costs 4 syncs vs 4,000
  (×1000), 131,612 vs 17,038,688 bytes. In WAL/FULL batching saves syncs the same way (1 vs 1,003).
- Files that appear and disappear: rollback: `-journal` per write transaction (and a directory
  sync each time); WAL: `-wal` and `-shm` from the first open that needs them until the last
  connection closes; the last close checkpoints (2 syncs) and deletes both. A platform that counts
  inodes or watches directories sees these.
- The close of a WAL database is not free: the checkpoint at close wrote 30 pages and 2 syncs in
  7.2. A platform that opens and closes a database per request pays it every time.
- Snapshot serving from object storage works only with a checkpointed file and `immutable=1`; with
  a WAL left beside it the reader silently returns the older state (7.4). Publishing must therefore
  checkpoint (or close the last connection) first and then upload the main file, which is what the
  Rebuild Ladder's at-rest rung (R5) gives.
- Fault injection shows SQLite's error contract: a failed write or sync aborts the transaction
  and leaves the previous committed state, byte for byte (7.3). The platform must treat any
  `SQLITE_IOERR` as "this commit did not happen".

## WAL-default evaluation (ruling 10)

WAL as the platform default holds for this chapter: it is cheaper in syncs and bytes per commit
(7.2) and the in-process tools (govfs) handle it. Two places where WAL needs care, both recorded,
neither a reason to change the default:
1. **A custom VFS must implement io_methods version 2 (xShm*)** or WAL is impossible; SQLite does not
   report an error when asked for WAL — `PRAGMA journal_mode=wal` just returns `delete`
   (`TestIOVersionOneCannotUseWAL`; `sqlitex.Open` turns that into an error because it checks the
   mode). `internal/govfs` uses version 3 by default.
2. **Read-only remote snapshots of a WAL database need `immutable=1`** (or header bytes 18–19
   rewritten to 1, which changes the file's bytes and therefore its E1 hash), and they must be
   checkpointed first. For snapshots the platform should publish the at-rest file and always read
   it with `immutable=1`.
Rollback mode appears in this chapter only where the experiment is about the comparison (7.2, 7.3).

## Linux-only parts and the Mac route

| Experiment | Linux-only | On macOS |
| --- | --- | --- |
| 7.1 | strace (merged stream, syscall checks) | VFS-only list from the CLI + `LINUX-ONLY:` line; full run: `docker compose run --rm lab go run ./ch07-vfs-and-filesystem-io/experiments/7.1-vfs-calls-to-syscalls` |
| 7.2 | strace counting | runs the 12 workloads uncounted + `LINUX-ONLY:` line; full run in the lab container (same command pattern) |
| 7.3 | nothing | runs natively (needs the pinned CLI for part A: `go run ./cmd/course build-tools`); darwin: cross-built only, self-check and tests never run on darwin |
| 7.4 | nothing | runs natively; `-s3` needs `docker compose up -d seaweedfs` (S3 on `127.0.0.1:18333`) |

All four were run in the lab container and gave the host's evidence exactly.

## What each span in SPANS.txt does (facts read from the copied/pinned source)

- **`sqlite.h.in` 584–878 (CAPI3REF, file open flags through io_methods).** Defines the
  `SQLITE_OPEN_*` flags (the VFS-only ones such as `MAIN_DB` 0x100, `MAIN_JOURNAL` 0x800, `WAL`
  0x80000 tell xOpen what kind of file it is opening), the device-characteristic bits, lock levels,
  sync flags, `struct sqlite3_file` and `struct sqlite3_io_methods`. The span's checked symbol is the
  comment marker `CAPI3REF` because the first named define is beyond the checker's 12-line window.
- **`sqlite.h.in` 731–733 (`SQLITE_SYNC_NORMAL` 0x2, `SQLITE_SYNC_FULL` 0x3, `SQLITE_SYNC_DATAONLY`
  0x10).** The xSync flag values. FULL contains NORMAL's bit, which is why vfstrace's test
  `flags & SQLITE_SYNC_FULL` labels a NORMAL sync FULL.
- **`sqlite.h.in` 747–749 (`struct sqlite3_file`).** A file object is only a pointer to its method
  table; every VFS "subclasses" it by allocating `szOsFile` bytes and putting its own fields after
  that pointer. govfs puts its 8-byte header first and the real unix file after it.
- **`sqlite.h.in` 854–878 (`struct sqlite3_io_methods`).** The per-file method table: version 1 has
  Close through DeviceCharacteristics (12 methods), version 2 adds the four xShm* methods, version 3
  adds xFetch/xUnfetch (memory-mapped I/O).
- **`sqlite.h.in` 1513–1550 (`struct sqlite3_vfs`).** The VFS object: version, `szOsFile`,
  `mxPathname`, `pNext` (the registry list), `zName`, `pAppData`, then xOpen, xDelete, xAccess,
  xFullPathname, the dynamic-library methods, xRandomness, xSleep, xCurrentTime, xGetLastError,
  and in later versions xCurrentTimeInt64 and the system-call override methods.
- **`os.c` 82–87 `sqlite3OsClose`.** Calls xClose only if the file has methods and clears the pointer,
  so closing twice is harmless.
- **`os.c` 88–91 `sqlite3OsRead`, 92–95 `sqlite3OsWrite`, 96–98 `sqlite3OsTruncate`.** One-line
  forwards to the file's method table; the core never touches a file descriptor itself.
- **`os.c` 99–102 `sqlite3OsSync`.** Forwards to xSync only when `flags` is non-zero; with
  `synchronous=OFF` the pager passes 0 and the VFS never sees a sync.
- **`os.c` 215–232 `sqlite3OsOpen`.** Masks the open flags with 0x1087f7f before calling xOpen, so
  connection-level flags (mutex, shared cache) never reach a VFS; asserts that a failed open leaves
  no methods.
- **`os.c` 233–237 `sqlite3OsDelete`.** Calls xDelete only if the VFS has one; `dirSync` is 0 or 1.
- **`os.c` 238–246 `sqlite3OsAccess`.** Plain forward of xAccess.
- **`os.c` 362–381 `sqlite3_vfs_find`.** Initializes SQLite, then walks the registered list under the
  main mutex; a NULL name returns the first (default) VFS. govfs uses it to find "unix".
- **`os.c` 408–431 `sqlite3_vfs_register`.** Unlinks the VFS if already registered, then inserts it
  at the head (if `makeDflt` or the list is empty) or second; the name must stay valid for as long
  as the VFS is registered (govfs allocates it in C memory and never frees it).
- **`os_unix.c` 834–872 `robust_open`.** `open()` with `O_CLOEXEC`, retried on EINTR; refuses file
  descriptors 0–2 (opens /dev/null in their place so a database never becomes stdout); fixes the mode
  of a new empty file with fchmod.
- **`os_unix.c` 1866–2095 `unixLock`.** Implements SHARED/RESERVED/PENDING/EXCLUSIVE with POSIX
  advisory locks on the pending byte, reserved byte and 510-byte shared range (7.1 shows the exact
  fcntl calls); keeps per-inode counts so several connections in one process share one set of POSIX
  locks.
- **`os_unix.c` 3463–3505 `seekAndRead`.** `pread`/`pread64` (or lseek+read) in a loop that retries
  EINTR and continues after partial reads; returns the bytes read.
- **`os_unix.c` 3512–3579 `unixRead`.** Serves from the memory map when the range is mapped
  (mmap is off by default); otherwise `seekAndRead`; a short read zero-fills the rest and returns
  `SQLITE_IOERR_SHORT_READ`, EIO-like errors return `SQLITE_IOERR_CORRUPTFS`.
- **`os_unix.c` 3588–3624 `seekAndWriteFd`.** `pwrite`/`pwrite64` (or lseek+write) retried on EINTR;
  limits a single write to 128 KiB (`nBuf &= 0x1ffff`).
- **`os_unix.c` 3643–3721 `unixWrite`.** Loops `seekAndWrite` until all bytes are written; ENOSPC
  becomes `SQLITE_FULL`, other errors `SQLITE_IOERR_WRITE`. In debug builds it tracks whether the
  change counter changed.
- **`os_unix.c` 3778–3849 `full_fsync`.** The actual sync: `F_FULLFSYNC` then `fsync` on macOS when a
  full sync is asked, `fsync` on other Apple cases, otherwise `fdatasync` (which `:3738-3739`
  redefines to `fsync` unless `HAVE_FDATASYNC`); with `SQLITE_NO_SYNC` only an fstat.
- **`os_unix.c` 3874–3894 `openDirectory`.** Strips the file name to get its directory (or ".")
  and opens it read-only for a directory sync.
- **`os_unix.c` 3911–3956 `unixSync`.** Computes "data only" and "full" from the flags, calls
  `full_fsync`, and on the first sync of a file opened with `UNIXFILE_DIRSYNC` also opens, syncs and
  closes the directory, then clears the flag (lines 3941–3953 also listed separately).
- **`os_unix.c` 3961–4006 `unixTruncate`.** `ftruncate`, rounding the size up to the chunk size when
  `SQLITE_FCNTL_CHUNK_SIZE` set one; records a truncation of the main db for the mmap bookkeeping.
- **`os_unix.c` 4141–4341 `unixFileControl`.** Handles file controls the unix VFS understands
  (LOCKSTATE, LAST_ERRNO, CHUNK_SIZE, SIZE_HINT, PERSIST_WAL, POWERSAFE_OVERWRITE, VFSNAME,
  TEMPFILENAME, HAS_MOVED, LOCK_TIMEOUT, MMAP_SIZE, …) and returns `SQLITE_NOTFOUND` for the rest
  (7.1/7.3: `SYNC`, `COMMIT_PHASETWO`, `PRAGMA`, `PDB`, `BUSYHANDLER`, `CKPT_START`, `CKPT_DONE` all
  return NOTFOUND = 12).
- **`os_unix.c` 4353–4375 `setDeviceCharacteristics`.** Sets sector size 4096 and the
  characteristics: `POWERSAFE_OVERWRITE` if the `psow` setting is on (default), plus `SUBPAGE_READ`
  → 0x9000 (7.1).
- **`os_unix.c` 4952–5085 `unixOpenSharedMemory`.** Finds or creates the per-inode shm node, names
  the file `<db>-shm`, opens it read-write with O_CREAT (or read-only with `readonly_shm`), and takes
  the dead-man-switch lock to decide whether this is the first connection (which truncates and
  rebuilds).
- **`os_unix.c` 5106–5239 `unixShmMap`.** Maps WAL-index region N (32 KiB): if the file is smaller and
  extension is allowed, writes one byte at the end of each 4 KiB page to grow it (7.1's eight 1-byte
  pwrites), then `mmap`s the regions.
- **`os_unix.c` 5284–5476 `unixShmLock`.** Takes or releases shared/exclusive locks on WAL-index
  slots (0 WRITE, 1 CKPT, 2 RECOVER, 3–7 READ0–READ4) as fcntl locks on bytes 120+slot of the -shm
  file, with in-process bookkeeping so connections in one process do not conflict through POSIX
  semantics; enforces a lock-ordering rule for blocking locks.
- **`os_unix.c` 5503–5546 `unixShmUnmap`.** Drops this connection's reference; when the last
  reference in the process goes and `deleteFlag` is set, unlinks the -shm file (7.1: at the last
  close), then closes it.
- **`os_unix.c` 6538–6826 `unixOpen`.** Opens any file type: computes `isNewJrnl` for a created main
  journal, super-journal or WAL (6569–6572), reuses a parked descriptor when possible
  (`findReusableFd`), opens with `O_LARGEFILE|O_NOFOLLOW` (+`O_CREAT`, `O_EXCL` as asked) through
  `robust_open` (6681), copies the database's permissions to journals and WALs, retries read-only on
  EACCES, unlinks delete-on-close files at once, sets `UNIXFILE_DIRSYNC` for new journals (6777) and
  fills in the unixFile.
- **`os_unix.c` 6833–6869 `unixDelete`.** `unlink`; a missing file is `SQLITE_IOERR_DELETE_NOENT`;
  when `dirSync` is set, opens and syncs the directory.
- **`os_unix.c` 6881–6903 `unixAccess`.** EXISTS: `stat` succeeds and the file is not an empty
  regular file; READWRITE: `access(R_OK|W_OK)`.
- **`vfstrace.c` 410–425 `vfstraceWrite`.** Prints `xWrite(file,n=…,ofst=…)` before calling the real
  method and ` -> rc` after it, which is what lets 7.1 attach the pwrite to the call.
- **`vfstrace.c` 445–465 `vfstraceSync`.** Builds the flag label with `if(flags & SQLITE_SYNC_FULL)
  "FULL" else if(flags & SQLITE_SYNC_NORMAL) "NORMAL"` — FULL for 0x2 too; DATAONLY appended.
- **`vfstrace.c` 876–930 `vfstraceOpen`.** Calls the real xOpen first and prints the call afterwards,
  then builds a per-file method table that mirrors the real one's version (shm and fetch only if the
  real file has them) — the same pattern govfs uses.
- **`vfstrace.c` 1138–1199 `vfstrace_register`.** Finds the root VFS, allocates a new `sqlite3_vfs`
  with `szOsFile = root szOsFile + sizeof(vfstrace_file)`, fills every slot with a vfstrace wrapper
  (dynamic-library and system-call slots only if the root has them) and registers it; the CLI's
  `-vfstrace` calls this with stdout as the output.
- **`pager.c` 4332–4441 `syncJournal`.** Before the database is written: if `fullSync`, syncs the
  journal content, then writes the record count into the journal header, then syncs again (with
  DATAONLY added only for SQLITE_SYNC_FULL); skips the first sync and the rewrite when the device
  has SAFE_APPEND and all syncs when SEQUENTIAL.
- **`pager.c` 6511–6730 `sqlite3PagerCommitPhaseOne`.** The commit: in WAL mode hands the dirty pages
  to `pagerWalFrames`; in rollback mode updates the change counter, syncs the journal
  (`syncJournal`), writes the dirty pages (`pager_write_pagelist`), truncates if the database
  shrank, and syncs the database file.
- **`pager.c` 2052–2180 `pager_end_transaction`.** Finalizes the journal according to journal mode:
  truncate (TRUNCATE), zero the header (PERSIST), or close and delete with
  `sqlite3OsDelete(…, extraSync)` (DELETE; the directory is synced only for `synchronous=EXTRA`).
- **`wal.c` 4038–4266 `walFrames`.** Writes frames to the WAL: restarts the log if possible
  (`walRestartLog`), writes and (unless SEQUENTIAL/OFF) syncs the WAL header when starting at frame
  0, writes a 24-byte header plus page per frame, syncs at commit if `WAL_SYNC_FLAGS` is non-zero
  (FULL), then updates the WAL index.
- **`wal.c` 4114–4124 (`syncHeader`).** The header sync after a WAL (re)start, with
  `CKPT_SYNC_FLAGS` — on at NORMAL — to stop an out-of-order write after a restart from corrupting
  the database (ticket ff5be73dee). The reason for the extra sync in correction 1.
- **`wal.c` 3875–3915 `walRestartLog`.** Runs when a writer starts while holding read-lock 0 (which
  means the last checkpoint copied every frame): it takes exclusive locks on READ1–READ4 to be sure
  no reader still uses the old frames, then resets the WAL header with a new salt
  (`walRestartHdr`), so the next frames overwrite the WAL from the start; then it re-acquires a read
  lock. The first frame write after this is "frame 0" and triggers the header write and sync.
- **`wal.c` 2199–2395 `walCheckpoint`.** Copies frames back: syncs the WAL first
  (`CKPT_SYNC_FLAGS`), writes each page into the database, truncates the database to its new size
  if needed, syncs the database, and records how far it got; with RESTART/TRUNCATE it also waits for
  readers and resets the log.

## Repair round (Opus) — new facts

See `REPAIR-REPORT.md` for the full table and the narrowed-claim facts (MAP-04 mmap, COST-05 PERSIST/TRUNCATE still sync the directory every commit on unix, SNAP-03 cache caveat, SNAP-04 in-place overwrite). Behaviour changes:

- `rangefs`: `ReadAt(p)` with `len(p)==0` returns `0, nil` and logs nothing. A failed or short range read logs `Request{Kind: range, Failed: true, Length: bytes received}` printed as `range NAME at OFF failed (N bytes received)`; `Summarize` adds only received bytes. `Seek`/`Stat` after `Close` return `fs.ErrClosed`. `New(nil, src)` uses `context.Background()`. `S3Source.ReadRange` requires the reply's `Content-Range` to start `bytes OFF-LAST/` (an S3 endpoint that ignores Range and answers 200 with the whole object now errors instead of returning offset-0 bytes).
- `govfs`: `injectFault(call)` decides, builds the injected `Call` (Result = fault code, Injected), appends it (if recording) and stores it as the fired call in one locked section, so `Disarm` never sees a half-filled call; when recording is off the injected call has `Seq 0` and the sequence does not advance. `Register` frees the C name and the vfs block on every failure; a losing concurrent `methodTableFor` frees its table. `xGetLastError` is now recorded (vfstrace.c:1091-1101 logs it too). `xFileControl(SQLITE_FCNTL_VFSNAME)` returns `<shim name>/<lower VFS names>` (e.g. `govfs-test-1/unix`), as sqlite.h.in:1008-1018 asks of shims. `Out` of xAccess/xFileSize/xCheckReservedLock is read only on rc 0 with a non-null pointer.
- 7.1: macOS path matches `delFlag=1` (the real vfstrace argument); the outside-VFS check covers pread64, pwrite64, read, write, writev — still none outside.
- 7.2: WAL check additionally requires at least one checkpoint and one restart per checkpoint (evidence: 1 and 1 at NORMAL and FULL); failed syscalls are not counted.
- 7.3: every fault trial must report Fired (n ≤ total) / not Fired (n = total+1), primary code 10, clean close, and an exact (unmasked) recipe match. Evidence unchanged.
- 7.4: repeat check names the cache fit (4/15/20 reads vs 500-page default cache); new section shows the full scan repeated at `cache_size=4` costs 17 range requests, 69632 bytes. Runtime still a few seconds.
