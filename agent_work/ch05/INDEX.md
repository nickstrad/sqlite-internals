# agent_work/ch05 — working scripts of chunk 5

| path | purpose | what it proved |
| --- | --- | --- |
| `agent_work/ch05/shm-crosscheck/main.go` | `go run ./agent_work/ch05/shm-crosscheck <file-shm>`: decode a `-shm` copy with `walfile.ParseIndex` and compare every numeric field (both header copies, nBackfill, the five read marks, nBackfillAttempted) with `showshm` from the pinned tree (`showshm` is not built by `cmd/course build-tools`; built for this check with `gcc -o _tools/bin/showshm $SQLITE_SRC/tool/showshm.c`) | Agreement on a `-shm` captured in 5.2 part A (mxFrame 155, nBackfill 0, marks 0,132,-1,-1,-1) and on one made with a held reader and a PASSIVE checkpoint (mxFrame 12, nBackfill 7, marks 0,7,11,-1,-1): `SHM CROSSCHECK OK` both times |
| `agent_work/ch05/build-notes.md` | plain-data notes for the prose pass | — |
| `agent_work/ch05/BUILD-REPORT.md` | build report | — |
