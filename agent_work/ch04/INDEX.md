# agent_work/ch04 — working scripts of chunk 4

| path | purpose | what it proved |
| --- | --- | --- |
| `agent_work/ch04/step-probe/main.go` | `go run ./agent_work/ch04/step-probe`: for recipe steps 1-30 in rollback mode, build the pre-step file, strace one child applying the step, count journal pwrites, database pwrites, syncs and unlinks | Every step makes 4 syncs at FULL and one unlink; step 3 journals 7 pages and writes 18 (chosen for 4.1), step 1 starts from a file with no freelist (4.1 part D2) |
| `agent_work/ch04/build-notes.md` | plain-data notes for the prose pass | — |
| `agent_work/ch04/BUILD-REPORT.md` | build report | — |
