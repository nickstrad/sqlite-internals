# Chapter 10 — Multi-database platform capstone (second half: relocation and experiments 10.2–10.4) — adversarial results (orchestrator's record)

Run 1 — 2026-11-21, on the Opus build, `run-all.sh`. Raw output: `results-run1.txt`.
23 HELD, 2 BROKEN. Every BROKEN line went to the repair round to be fixed or answered:

| Attack | What was tried | Expected | Got |
| --- | --- | --- | --- |
| REL-04 | kill destination b after step 4 (Rebuild) and call Rollback while b remains down | Rollback succeeds (a reclaims cleanly, relocations.state=rolled-back, and a->c relocation can start) | rbErr=relocate: roll back db-0001: discard at the destination: node process for "discard db-0001" is not running catOwner=a/e3 relState=running beginNextErr=catalog: a relocation of the database is al |
| REL-05c | fail b.Activate at recordShipped (SQLITE_BUSY) after PutIfAbsent+writeBelief, then unlock catalog and Resume | Resume completes (state=done) and repairs catalog to owner=b epoch=2 location=b:db-0001.db last_shipped=5 schema_version=3 | failedAtActivate=true resume=(rolled forward,<nil>) relState=done cat=(owner=b epoch=2 loc="" lastShipped=3 schemaVer=2) |

Run 2 — after the Opus repair round (31d6b86): 25 HELD, 0 BROKEN (`results-run2.txt`).
The repair report (`../REPAIR-REPORT.md`) says for each run-1 finding whether it was fixed or answered. Still BROKEN in run 2:

