# Chapter 5 — adversarial results (orchestrator's record)

Run 1 — 2026-11-20, on the Opus build with Neon's comments, `run-all.sh`. Raw output: `results-run1.txt`.
21 HELD, 7 BROKEN. Every BROKEN line went to the repair round to be fixed or answered:

| Attack | What was tried | Expected | Got |
| --- | --- | --- | --- |
| INS-05b | torn -shm header with copy 1 (bytes 0..47) zeroed and copy 2 (bytes 48..95) populated (midpoint of walIndexWriteHdr) | ParseIndex detects the torn header (error or HeadersAgree()==false) | err=<nil> initialized=false headersAgree=true hdr0.mxFrame=0 hdr1.mxFrame=0 |
| REP-02 | replay generation-2 WAL (steps 6-8, ckpt-seq=1) onto stale generation-0 base (step 0) | stale-generation base mismatch is detected and rejected by walfile.Replay (OUTLINE 5.2) | Replay returned err=nil (wal_ckpt_seq=1 applied=61 out=323584B); opening result: uv=8 inspect_err=database disk image is malformed (11) integrity_check="*** in database main ***\nTree 3 page 46: btree |
| REP-05 | committed transaction with cache_size=5 spills/in-place overwrites + SAVEPOINT/ROLLBACK TO/RELEASE | walRewriteChecksums keeps full WAL valid, walfile.Replay matches SQLite TRUNCATE E1 without savepoint ghost | valid=10/10 applied=10 E1_equal=true savepoint_ghost=true |
| REP-07a | walfile.Replay with 0-commit header-only WAL paired with mismatched page size (4096 vs 8192) and non-page-multiple base | Replay rejects both with ErrPageSizeMismatch and ErrBaseSize even when LastCommitFrame==0 | mismatch_err=<nil> bad_base_len_err=<nil> |
| REP-07b | call walfile.ReplayFiles when outPath+".replay-tmp" already exists with sentinel content | ReplayFiles returns an error and preserves the pre-existing .replay-tmp file untouched | replay_err=open _work/adv-ch05/wal-replay-falsify/rep07b/out.db.replay-tmp: file exists tmp_still_exists=false tmp_preserved=false |
| CHK-03 | connection C runs TRUNCATE then connection W writes restarted WAL header; check OUTLINE 5.4 claim 'salt-1 increments on restart' | restarted WAL header salt-1 == prev_salt1 + 1 (OUTLINE 5.4 literal evidence claim) | before_salt1=0xc08ddcfd after_trunc_shm_salt1=0xc08ddcfe(+1:true) restarted_wal_salt1=0x1d025b82(+1:false) ckpt_seq=0->0 |
| CHK-05 | lock-free NOOP under simultaneous writer+checkpointer+reader; multi-conn close cleanup vs Replay E1; read-only checkpoint | NOOP returns immediately (<100ms, busy=0, accurate log); -wal persists until last close then auto-checkpoints E1-equal and deletes -wal/-shm; mode=ro cannot che | noop=busy=0 log=38 checkpointed=0(0ms,err=<nil>) wal_kept_1close=true cleaned_last=true E1=true ro_ckpt=busy=0 log=38 checkpointed=38(err=<nil>) |
