# Chapter 9 — Backup, crash recovery, and integrity — adversarial results (orchestrator's record)

Run 1 — 2026-11-21, on the Opus build, `run-all.sh`. Raw output: `results-run1.txt`.
35 HELD, 5 BROKEN. Every BROKEN line went to the repair round to be fixed or answered:

| Attack | What was tried | Expected | Got |
| --- | --- | --- | --- |
| CAP-02a | CLI .backup (no .timeout) racing fresh sqlite3 writer processes (each closes as last connection) | 0 database-is-locked errors if CLI .backup needs no busy handler under WAL writers | ok=31/40 locked=9 other=0 (sample="Error: database is locked") |
| CAP-03a | same-connection write+ROLLBACK after backup step 2 (copied=10) | copied advances steadily [5,10,15,20] if same-connection operations never restart a backup | copied sequence = [5 10 5 10] (restarted=true via pager.c:3185) |
| CRASH-06 | synchronous=NORMAL commits after a reader-pinned PASSIVE checkpoint (no WAL restart) | 9.2 portable ruleCut(NORMAL)=32 matches strace FirstUnsynced across all WAL NORMAL workloads | strace FirstUnsynced=304912 (end7=304912) recovers step 7; 9.2 ruleCut=32 recovers step -1 (ckpt=busy=0 log=74 checkpointed=25 sameGen=true end7=304912 end9=494432) |
| SHIP-03d | tamper segment 4 metadata Commits[0].Frame to non-commit frame 1 (valid body hash & WAL checksum chain) | LoadCatalog/Plan(4) rejects non-commit CommitPoint.Frame before any page is applied | damaged=0 planErr=<nil> execErr=segment segments/000000000004: replay of frames 1-1 found 1 valid frames, last commit at 0; want 1 and 1 |
| SHIP-03f | corrupt latest segment 8 in store, then call walship.Open() to resume and ship step 9 | walship.Open() detects damaged latest segment on resume instead of silently rewinding to position 7 | openErr=<nil> resumedPos=7 nextCycleErr=<nil> (isErrExists=false) |

Run 2 — after the Opus repair round (f1884a6): 37 HELD, 3 BROKEN (`results-run2.txt`).
The repair report (`../REPAIR-REPORT.md`) says for each run-1 finding whether it was fixed or answered. Still BROKEN in run 2:

- CAP-02a — CLI .backup (no .timeout) racing fresh sqlite3 writer processes (each closes as last connection)
- CAP-03a — same-connection write+ROLLBACK after backup step 2 (copied=10)
- CRASH-06 — synchronous=NORMAL commits after a reader-pinned PASSIVE checkpoint (no WAL restart)
