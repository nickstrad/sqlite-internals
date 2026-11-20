# Chapter 1 adversarial results (orchestrator's record)

Run 1 — 2026-11-20, build 563721c with Neon's comments, `run-all.sh`, 162 s.
Raw output: `results-run1.txt`. 37 HELD, 11 BROKEN:

| Attack | Finding | Disposition |
| --- | --- | --- |
| TWIN-10 | `ANALYZE` on an indexed table: the Go driver (built with ENABLE_STAT4) writes one more page than the pinned C CLI (no STAT4); files differ | real divergence between the two builds; the twin claim must be narrowed to statements that do not depend on compile options, and the chapter must say so |
| DET-05d, DET-08a | `secure_delete=FAST` is not equivalent to `ON`: different bytes, and batching shows again | new determinism rule: `secure_delete` must be exactly ON |
| DET-06 | WAL change counter does not move when a session's commits never dirty page 1: "counts connection sessions" is too broad | narrow the statement (matches review finding 4) |
| DET-08b | a rolled-back savepoint with large inserts leaves different bytes than never running it | new source of variation: aborted work |
| DET-08c | `ANALYZE`: Go vs CLI files differ (STAT4) | same root as TWIN-10 |
| DET-08d | database/sql pool churn (a second pooled connection) changes the WAL-mode change counter | recipe must run on a single connection; state it |
| DET-08e | SQL `localtime` modifier depends on TZ | new source of variation: time zone |
| API-02 | `MatchStep` accepts a file whose two counters disagree | to repair |
| API-04b | `StepHashes` trusts a tampered cache | to repair |
| API-09 | `Step(0)` on an invalid Recipe is not validated | to repair |
