# agent_work/ch10a — working files of chunk 10a

| path | purpose | what it proved |
| --- | --- | --- |
| `agent_work/ch10a/build-notes.md` | plain-data notes for review, attack and the prose pass: corrections to the outline, the fencing scheme, every package and exported function, experiment 10.1 (claim, command, every output line), spans, timeline, platform facts, WAL evaluation, Mac route | — |
| `agent_work/ch10a/BUILD-REPORT.md` | build report: what was built, commands run with pass/fail, corrections, what is left | — |

No working scripts were kept. Three throwaway probes were run as temporary test files and deleted
(their results are in build-notes.md):

| probe (not kept) | what it proved |
| --- | --- |
| model depth sweep (`Explore` at depths 6–18 for the correct model and both mutants, with timing) | depth 16 explores 9,595,507,962 interleavings over 44,134 states in ~0.5 s; both mutants are caught by depth 8; chose depth 16 as the default |
| descriptor listing of a node with one open database (`procstat.DescriptorsUnder`) | 7 descriptors per open service database: 3 × `.db`, 3 × `-wal`, 1 `-shm`; the catalog adds 3 |
| connmgr stats after node evictions | connmgr counts every node eviction as `EvictedNotAtRest` (the shipper's `-shm` is present at its check, which returns before opening the file); the node's own check after closing the shipper is the real at-rest proof |
