# Probes: Go access strategy and VFS feasibility

Findings: `docs/knowledge/go-access-and-vfs.md`. One Go module (`go.mod` here) pins the driver
versions that bundle SQLite 3.53.4. `latest/` is a separate nested module at the newest driver
tags, kept only to show that they run 3.54.1.

Setup, once per checkout (all commands from this directory):

```bash
export PATH=/usr/local/go/bin:$PATH GOFLAGS=-buildvcs=false
./gen-amalgamation.sh   # builds sqlite3.c from the pinned tree into cgo/amalg/ (gitignored),
                        # checks it against the official zip, copies ext/misc/vfstrace.c
```

`cgo/...`, `strace/` and `vfs/mattnshim/` need that step. The first build of `cgo/amalg` takes
about 80 s on vm-1. Later builds reuse the cache.

| Probe | Run command | What it shows |
| --- | --- | --- |
| `gen-amalgamation.sh` | `./gen-amalgamation.sh` | `make sqlite3.c` in a copy of the pinned tree gives a file byte-identical to sqlite-amalgamation-3530400.zip |
| `version/mattn` | `go run ./version/mattn`; `go run -tags libsqlite3 ./version/mattn`; static: `mkdir -p /tmp/sl && ln -sf /usr/local/lib/libsqlite3.a /tmp/sl/ && CGO_LDFLAGS="-L/tmp/sl -lm" go build -tags libsqlite3 ./version/mattn` | mattn v1.14.52 (bundled) and `-tags libsqlite3` (dynamic or static) run the pinned source id |
| `version/modernc` | `go run ./version/modernc` | modernc v1.60.1 runs the pinned source id |
| `version/ncruces` | `go run ./version/ncruces` | ncruces v0.35.6 + go-sqlite3-wasm v6.3.35304 runs the pinned source id |
| `version/cli` | `go run ./version/cli` | os/exec route: `-json`, `-vfstrace`, `.filectrl`, `pragma wal_checkpoint` through the pinned CLI |
| `version/modernclib` | `go run ./version/modernclib` | calling modernc's transpiled C API directly (`wal_checkpoint_v2`, `snapshot_get`) without the driver |
| `latest/*` | `cd latest && go run ./mattn; go run ./modernc; go run ./ncruces` | the latest tags (mattn v1.14.53, modernc v1.62.0, ncruces v0.37.0) all run 3.54.1, not the pin |
| `cgo/amalg` + `cgo/sqlitec` | (library) | first-party cgo binding: pinned amalgamation with custom options, a thin wrapper, vfstrace, and a fault shim |
| `cgo/cmd/version` | `go run ./cgo/cmd/version`; `go run -tags sqlite_debug ./cgo/cmd/version` | source id, compile options (SNAPSHOT, DBPAGE, DBSTAT, SESSION, PREUPDATE, DEBUG), wal_hook, snapshot_get/open, checkpoint_v2, DATA_VERSION, sqlite_dbpage, dbstat, serialize, db_status |
| `cgo/cmd/vfstrace` | `go run ./cgo/cmd/vfstrace` | every VFS call (vfstrace over os_unix.c) for WAL setup, insert-commit, checkpoint and close (sample: `samples/cgo-vfstrace.txt`) |
| `cgo/cmd/fault` | `go run ./cgo/cmd/fault` | fail the Nth xWrite or xSync, and a child `_exit(9)` at xSync #k, followed by an integrity check after reopen |
| `vfs/ncruceslog` | `go run ./vfs/ncruceslog` | a pure-Go logging VFS over ncruces's "os" VFS. xShm* calls are invisible to it (sample: `samples/ncruceslog.txt`) |
| `vfs/mattnshim` | `go run ./vfs/mattnshim/cmd`; `go run -tags libsqlite3 ./vfs/mattnshim/cmd` | vfstrace registered by a separate cgo package into mattn's SQLite through `?vfs=trace` |
| `strace` | `go run ./strace` | merged stream of VFS calls (vfstrace) and the real syscalls (strace), including the directory fsync (sample: `samples/strace-merged.txt`) |
| `locks` | `go run ./locks` | cross-process interop of each driver with the CLI in WAL and DELETE modes, and which lock syscalls each driver makes (F_SETLK vs F_OFD_SETLK) |
