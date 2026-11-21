# agent_work/ch07 — working files of chunk 07 (Chapter 7, VFS and filesystem I/O)

| Path | Purpose | What it proved |
| --- | --- | --- |
| `agent_work/ch07/span-ends.sh` | Reads `file start symbol` lines on stdin and prints `file start end symbol`, where `end` is the first line at or after `start` that begins with `}` in `$SQLITE_SRC/file`. Used to compute the end line of every function span in `ch07-vfs-and-filesystem-io/SPANS.txt` | All 46 spans in SPANS.txt; `go run ./cmd/course check spans ch07` passes |
| `agent_work/ch07/build-notes.md` | Plain-data notes for the prose pass (Neon) | — |
| `agent_work/ch07/BUILD-REPORT.md` | What was built, commands run, corrections, open items | — |

One-off checks run during the build and not kept as files (results are in build-notes.md):
a scratch program printing the raw merged CLI stream (became 7.1); a strace of the installed CLI
with the stat/access syscalls traced (showed `xAccess` → `newfstatat`, `xFileSize` → `fstat`, and
`fchown` on new journal/WAL/shm files when running as root, which is why `fchown` is not traced);
a temporary mutation of the `xSync` signature in `internal/govfs` (the signature test failed as
it should, and the mismatched build still ran without crashing — see the notes).
