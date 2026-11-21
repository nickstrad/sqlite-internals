# agent_work/ch10b — working files of chunk 10b

| path | purpose | what it proved |
| --- | --- | --- |
| `agent_work/ch10b/build-notes.md` | plain-data notes for review, attack and the chapter 10 prose pass: corrections to the outline, the relocation protocol, the three layers of guarantee, the six invariants as properties, experiments 10.2–10.4 (claim, command, every output line), spans, relocation timeline, the eight design questions, signals, limits, WAL evaluation, Mac route | — |
| `agent_work/ch10b/BUILD-REPORT.md` | build report: what was built, commands run with pass/fail, corrections, what is left | — |
| `agent_work/ch10b/probes/size/main.go` | build the recipe to steps 0, 4, 5, 10, 20 and print file size and build time | step 4 = 204 KB, step 5 = 204 KB (the index fits in free pages), step 0 = 16 KB; sized 10.4's store use (~0.6 MB of objects per database) |
| `agent_work/ch10b/probes/writecost/main.go` | one in-process node on SeaweedFS: claim 20 databases, 4 store-durable writes each, time them, CPU profile to `/tmp/writecost.pprof` | claim ≈ 71 ms, store-durable write ≈ 46 ms in-process, CPU ≈ 28 % of wall time: the write path is I/O-bound (fsyncs, S3 round trips), which is why 10.4 scales with writer processes and why its default is 500 databases |
| `agent_work/ch10b/probes/wipe/main.go` | delete every object under the given prefixes of bucket `course-ch10b` (cleanup after runs) | — |
