# Library-only Go route: two probes (outline revision, 2026-11-20)

Run on vm-1 against the pin (SQLite 3.53.4). Scripts: `agent_work/probes/outline-v2/`.
These back the decision in OUTLINE.md section 4.1 to use existing libraries only.

## 1. The unmodified modernc driver reaches what the kept experiments need

`modernc.org/sqlite v1.60.1` with `modernc.org/libc v1.77.1`, no cgo.
Command: `go run ./victim api <empty dir>` from the probe directory.

```
sqlite_source_id matches pin: true
sqlite_dbpage pages: 2
dbstat rows: 2
wal_checkpoint(PASSIVE) busy/log/ckpt: 0 3 3
db_status CACHE_USED bytes: 22927 <nil>
serialize bytes: 8192 <nil>
FCNTL_DATA_VERSION: 5 <nil>
backup step(1): more = true remaining = 1 of 2 <nil>
second writer error code: 5 (5 = SQLITE_BUSY)
```

How each is reached:

- `sqlite_dbpage`, `dbstat`, checkpoint counts: plain SQL.
- `Status` (`sqlite3_db_status`), `Serialize`, `FileControlDataVersion`, `NewBackup` with
  `Step` / `Remaining` / `PageCount` / `Finish`: methods on the driver's unexported connection
  type, called inside `(*sql.Conn).Raw` through a small interface assertion.
- Settings: DSN `_pragma=name(value)` (repeatable) and `_txlock=immediate`.
- Result codes: `errors.As(err, &*sqlite.Error)` then `Code()`.

Not exported by the driver (not needed by the revised outline): `sqlite3_status64`,
`sqlite3_stmt_status`, `sqlite3_trace_v2`, `sqlite3_wal_hook`, `sqlite3_snapshot_*`,
`SQLITE_DBCONFIG_NO_CKPT_ON_CLOSE`, `sqlite3_test_control`, custom writable VFS.

Also confirmed with the CLI: `PRAGMA wal_checkpoint(NOOP)` is accepted at the pin and returns
the frame count without checkpointing (`0|3|0`).

## 2. strace can kill a writer at an exact sync, repeatably

Command: `./crash-at-fsync.sh` (strace 6.8). The writer does five one-row WAL commits and
prints `ack k` after each. strace runs it with
`-f -e trace=fsync,fdatasync -e inject=fsync,fdatasync:signal=SIGKILL:when=K`.
The writer calls `runtime.LockOSThread()` first, because strace counts per thread.

```
baseline sync=NORMAL commits=5 fsync_calls=8
baseline sync=FULL   commits=5 fsync_calls=14
sync=FULL kill_at_sync=7:  acks=0 rows=0 integrity=ok   (5 of 5 runs identical)
sync=FULL kill_at_sync=8:  acks=0 rows=1 integrity=ok   (5 of 5)
sync=FULL kill_at_sync=9:  acks=1 rows=2 integrity=ok   (5 of 5)
sync=FULL kill_at_sync=10: acks=2 rows=3 integrity=ok   (5 of 5)
sync=NORMAL kill_at_sync=7,8: acks=5 rows=5 integrity=ok (5 of 5)
sync=NORMAL kill_at_sync=9,10: not reached; the run has only 8 sync calls
```

What this shows:

- The kill point is exact: every K gave the same outcome on all five runs. This replaces the
  first-party C fault shim from the Phase 1 research.
- The signal arrives on entry to the sync, before it runs. The commit whose sync was
  interrupted is still there after reopen (rows = acks + 1), because its frames were already
  written to the kernel. A process kill cannot lose a completed write, which is why it cannot
  stand in for a power cut.
- At `NORMAL` the five commits issue no sync at all: 8 sync calls happen with or without them
  (database creation, WAL creation, and the checkpoint at close). At `FULL` there are 6 more,
  about one per commit. Kills at K ≤ 6 land in setup, before the table exists.
- strace itself exits with status 137 when the tracee is killed this way; scripts must expect it.

Limits: `when=K` counts calls per traced thread, so the victim must keep its SQLite work on one
OS thread. Only the WAL case was run; rollback-journal mode is left to the chapter 4 build.
