# agent_work/ch02 — working scripts of chunk 2

| path | purpose | what it proved |
| --- | --- | --- |
| `agent_work/ch02/inspect/main.go` | `go run ./agent_work/ch02/inspect <db> [pages]`: print the decoded header, page count source, every b-tree, role counts, freelist and (with a second argument) one line per page from `internal/dbfile` | First run on the WAL R0: census 270 pages = 3+89+1+4+170+1+2, freelist trunk 230 with leaves 231, 232, no unaccounted page — the parser works on a real recipe file before the experiments were written |
| `agent_work/ch02/build-r0/main.go` | `go run ./agent_work/ch02/build-r0 <db> <steps> <wal\|delete>`: build the recipe to a step with `recipe.Build` | Produced the inputs for `inspect` while building |
| `agent_work/ch02/build-notes.md` | plain-data notes for the prose pass | — |
| `agent_work/ch02/repair-probe/cap04/main.go` | `go run ./agent_work/ch02/repair-probe/cap04`: a child holds an uncommitted write tx after `wal_checkpoint(TRUNCATE)` (`-wal` 0 bytes, `-shm` live) while the parent captures, rebuilds and compares | The capture is the committed state (balance 43047, integrity ok); E1-equal to the file after the child rolls back; a later commit is a later state. Answer to attack CAP-04 |
| `agent_work/ch02/REPAIR-REPORT.md` | repair round 1 report | — |
