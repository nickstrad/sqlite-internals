# Chapter 6 — Locking and concurrent access — adversarial results (orchestrator's record)

Run 1 — 2026-11-21, on the Opus build, `run-all.sh`. Raw output: `results-run1.txt`.
43 HELD, 6 BROKEN. Every BROKEN line went to the repair round to be fixed or answered:

| Attack | What was tried | Expected | Got |
| --- | --- | --- | --- |
| LOCK-04b | C sqlite3 CLI as contender vs Go holder in WAL mode across stray-close db and -shm | CLI refused before and after stray-close db, commits OK after stray-close -shm; Go holder COMMIT returns SQLITE_PROTOCOL (15), final DB has cli-contender | before_locked=true after_db_locked=true after_shm_ok=true holder_commit=SQLITE_OK integrity=ok rows=go-holder |
| LOCK-06 | WAL stray-close -shm when WAL already has uncheckpointed frames (no walRestartLog on commit) | 6.1 B / build-notes #1 claim: holder COMMIT fails with SQLITE_PROTOCOL (15) and contender's commit survives | contender_commit=SQLITE_OK holder_commit=SQLITE_OK integrity=ok final=(2 row(s): seed,holder) |
| BUSY-05b | WAL writer sets PRAGMA locking_mode=EXCLUSIVE and commits; 2nd process attempts read | unqualified WAL claim: writer never blocks readers (SQLITE_OK) | locking_mode=exclusive reader_open_failed=procs: child exited: child reader (sql) during step 0 "ready": exit status 2; output: procs: child sql: start role: open /root/Software/sqlite-internals-work/ |
| BUSY-09 | falsify Lab.StillWaiting with busy_timeout=0 child + strace EAGAIN count on -shm byte 120 in 6.2-busy-rules | StillWaiting returns false for immediate busy_timeout=0 failure and 6.2 generates >=10 EAGAINs on byte 120 | still_waiting_on_timeout0=true res0=SQLITE_BUSY (5) eagain_on_byte120=24(want>=10) |
| SNAP-04 | slow sqlite_dbpage capture at step 6 while writer with wal_autocheckpoint=20 commits steps 7-18 | WAL grows monotonically past autocheckpoint=20, nBackfill stays pinned at reader mark, capture equals step 6 (E1), salt-1 increments after reader ends | monotonic=true mxFrame=22->257 pinned_nBackfill=0 e1_step6=true ckpt_after=(busy=0 log=257 checkpointed=257) salt1=0x2b0f7a04->0x2b0f7a06 |
| PROCS-02c | ParseProcLockLine on valid Linux fs/locks.c host variants (<none>:0, MSNFS, UNKNOWN) | ReadProcLocks/ParseProcLockLine tolerates valid unrelated host /proc/locks entries without aborting 6.1 | parsed=0/3 rejected=[none-inode,msnfs-mode,unknown-class] |

Run 2 — after the Opus repair round (a045be4): 45 HELD, 4 BROKEN (`results-run2.txt`).
The repair report (`../REPAIR-REPORT.md`) says for each run-1 finding whether it was fixed or answered. Still BROKEN in run 2:

- LOCK-04b — C sqlite3 CLI as contender vs Go holder in WAL mode across stray-close db and -shm
- LOCK-06 — WAL stray-close -shm when WAL already has uncheckpointed frames (no walRestartLog on commit)
- BUSY-05b — WAL writer sets PRAGMA locking_mode=EXCLUSIVE and commits; 2nd process attempts read
- SNAP-04 — slow sqlite_dbpage capture at step 6 while writer with wal_autocheckpoint=20 commits steps 7-18
