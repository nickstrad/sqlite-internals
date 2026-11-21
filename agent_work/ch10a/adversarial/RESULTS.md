# Chapter 10a — capstone service core and experiment 10.1 — adversarial results (orchestrator's record)

Run 1 — 2026-11-21, on the Opus build, `run-all.sh`. Raw output: `results-run1.txt`.
18 HELD, 9 BROKEN. Every BROKEN line went to the repair round to be fixed or answered:

| Attack | What was tried | Expected | Got |
| --- | --- | --- | --- |
| MOD-05b | probe model bound '5 local commits per node': A commits 4 writes (pos 2..5), B fences at 6 and tries its 1st write | B can perform at least 1 local commit under '5 local commits per node' | B.Local=6, lastStep="13. B write                      refused: not enabled (owner)" |
| FENCE-03a | 16 rounds of concurrent a.Claim vs b.Claim on real SeaweedFS S3 | all 16 lineages pass fence.Audit and no failed Claim leaves node.Owned(id)==true | auditFailures=0/16 staleEpochErrs=16 failedClaimStillOwns=16 |
| FENCE-03b | deterministic Claim race: B bumps catalog epoch after A's fence Put succeeds, before A's RecordShipped | A's Claim either succeeds (ignoring ErrStaleEpoch like shipLocked) or cleans up owned/.owner on error | claimErr=catalog: the epoch is no longer current: race-det epoch 1 fencePos=1 owned=(1,true) beliefExists=true |
| CAT-01 | LocalDurable writes shipped during LRU eviction (db-1) and Node.Close (db-2), then compare catalog vs S3 | catalog last_shipped and schema_version match S3 (pos=2, uv=1) for both databases | db-1(storePos=2,storeUV=1 vs catPos=1,catUV=0); db-2(storePos=2,storeUV=1 vs catPos=1,catUV=0) |
| CAT-02 | SIGKILL owner with unshipped LocalDurable write, restart, open via Read() so openDatabase ships snapshot | catalog last_shipped and schema_version match S3 (pos=2, uv=1) | readUV=1 storePos=2 storeKind=snapshot storeUV=1 vs catPos=1 catUV=0 |
| CAT-03a | re-Claim fails on fail-before-put after clearing in-memory n.owned[id], then restart node | belief file removed when Claim clears ownership so restart does not resurrect old epoch 1 | reClaimErr=true ownedAfterFail=false ownedAfterRestart=true(epoch=1) |
| CAT-03b | node.Start with belief file containing 'epoch 0\n' (invalid ownership epoch) | node.Start rejects epoch 0 belief file with an error | startErr=<nil> ownedZero=true(epoch=0) |
| CAT-04b | cleanly close node, tamper db-restart.db offline, restart node, then Write(StoreDurable) | restarted node detects offline tamper (ErrChangedWhileShut) rather than publishing tampered data to S3 | writeErr=recipe step 2 statement 0 (INSERT INTO accounts(id, name, balance) VALUES(?, ?, ?)): constraint failed: UNIQUE constraint failed: accounts.id (1555) ackPos=0 slots=3 tamperPublishedToS3=true |
| CAT-05b | 180 in-place 256 KiB StoreDurable UPDATEs on 1 tiny open DB (K=1, limit 34 MiB) + bounds must-fail check | memory stays <= MemoryLimit (34 MiB) on 1 tiny DB and tight-limit control returns ErrExceeded | dbFileKiB=276 peakRSS=74MiB/34MiB memExceeded=6(err=bounds exceeded: 0 descriptors above the baseline (limit 41), memory growth 37945344 bytes (limit 35913728)) mustFailDetected=true |

Run 2 — after the Opus repair round (cfceace): 26 HELD, 1 BROKEN (`results-run2.txt`).
The repair report (`../REPAIR-REPORT.md`) says for each run-1 finding whether it was fixed or answered. Still BROKEN in run 2:

- MOD-05b — probe model bound '5 local commits per node': A commits 4 writes (pos 2..5), B fences at 6 and tries its 1st write
