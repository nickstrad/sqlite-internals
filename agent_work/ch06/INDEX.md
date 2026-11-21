# agent_work/ch06 — working files of chunk 6

| path | purpose | what it proved |
| --- | --- | --- |
| `agent_work/ch06/build-notes.md` | plain-data notes for the prose pass: per experiment the claim, run command, output and how to read it; per span what the C code does; timeline, platform facts, WAL evaluation, Mac route; corrections to the outline | — |
| `agent_work/ch06/BUILD-REPORT.md` | build report: what was built, commands run with pass/fail, corrections, what is left | — |
| `agent_work/ch06/REPAIR-REPORT.md` | repair round 1: each review finding and BROKEN attack, fixed (commit) or answered; stale prose list; commands run | — |
| `agent_work/ch06/probes/snap04-restart/main.go` | `go run ./agent_work/ch06/probes/snap04-restart`: a reader that begins on a fully checkpointed WAL, then one writer commit, printing the `-shm` header and checkpoint info | the reader holds WAL_READ_LOCK(0) and the WAL restarts under it (salt-1 +1, nBackfill 0); explains SNAP-04's salt-1 +2 (build-notes correction 7) |

Apart from the one repair-round probe above, every probe of this chunk became part of an experiment or of the `internal/procs` tests.
