# OUTLINE changes — revision 1 (7924ef7) to revision 2

Made under Nick's direction of 2026-11-20: trim to what a platform builder needs, existing
libraries for Go access, power loss at understanding level, the least knowledge that runs a
thousand-database platform well, and two reference books mined. The test applied to every item
was the North Star: does this help someone understand SQLite deterministically enough to hold
thousands of database files that can be remade as needed?

Old numbers are revision 1's. New numbers are revision 2's.

## Result

| Chapter | Rev 1 | Rev 2 | Ladder rung |
| --- | --- | --- | --- |
| 1 Architecture and execution path | 5 | 3 | R0 Recipe |
| 2 File format and pages | 5 | 3 | R1 Pages |
| 3 B-trees and page management | 5 | 3 | R2 Canonical form |
| 4 Pager and page cache | 5 | 2 | none (was "undo log") |
| 5 WAL, transactions, checkpoints | 6 | 4 | R3 Redo log |
| 6 Locking and concurrent access | 6 | 4 | R4 Live snapshot |
| 7 VFS and filesystem I/O | 5 | 2 | none (was "I/O log") |
| 8 Connection and database lifecycle | 6 | 3 | R5 At-rest state |
| 9 Backup, crash recovery, integrity | 6 | 4 | R6 Point in time |
| 10 Platform capstone | 6 | 4 | R7 Fleet |
| **Total** | **55** | **32** | 10 rungs became 8 |

Code estimate: about 13,000 lines of Go and 500 of C became about 8,600 lines of Go and no C.
Phase 2 chunks: 15 became 12.

## Cut

Experiments:

- **3.3 overflow-threshold** — the exact byte at which a row spills is arithmetic a platform never acts on; the page census (2.2) already follows overflow chains.
- **4.2 spill-before-commit** — cool to know. The one part that matters to WAL shipping (uncommitted frames can be in the WAL before commit) is now a case in 5.2.
- **7.3 files-and-directories** — as a separate experiment. Which files appear and vanish is seen in 7.1 and enforced in 8.3. The leftover research question about repeated directory syncs is dropped.
- **7.4 write-log-rebuild** and its ladder rung "I/O log" — needed a custom C VFS shim; a platform that ships the WAL does not capture at the VFS.
- **7.5 power-cut-shim** — deep power-loss simulation, cut by direction 4.
- **8.4 statements-and-schema-changes** — automatic re-prepare is not something the platform manages.
- **8.5 attach-limits-and-atomicity** — as an experiment. One paragraph from the documentation remains in chapter 8, labelled as not demonstrated.

Scope inside kept experiments:

- Power-cut runs inside old 9.4 and 10.4 (torn sector writes, cuts during checkpoint, hunting for a real corruption).
- The byte-identical WAL via SQLite's seeded PRNG (old 5.2) — needed a test-only C API. WALs are compared as frame sequences.
- Opcode counters and statement tracing (old 1.2), debug builds, `sqlite3_snapshot`, the WAL hook.
- Serialize, logical dump and session changesets as capture methods in old 9.1 (seven methods became five).
- Writer latency distributions (old 6.3); fault pairs in the relocation test (old 10.5).

Sections, packages and reading:

- The first-party cgo binding, the amalgamation build, and all three C shims (logging, fault, power-cut).
- `internal/journalfile` (Go journal parser and replayer) and `internal/capturevfs`.
- Ladder rung "undo log": the platform never replays a rollback journal itself.
- The block-device power-loss appendix (`dm-log-writes`, `dm-flakey`).
- The mattn-defaults aside in chapter 9.
- Reading: tokenizer, parser and code generator (ch 1); auto-vacuum internals and the body of `balance_nonroot` (ch 3); `test_demovfs.c`, `vfsstat.c`, `test6.c` (ch 7); `attach.c`, lookaside (ch 8); `memdb.c`, `sqlite3_rsync.c`, `dbhash.c` as reading (ch 9, 10); LiteFS notes (ch 10).

## Demoted or merged

- **1.2 explain-walk + 1.3 layer-crossings → 1.2 one-insert-through-the-layers.** One statement followed through every layer; it now also carries the Go-driver-versus-C-build comparison.
- **1.5 broken-recipes → second half of 1.3 recipe-rebuild.** Kept in full: the determinism rules are the North Star itself.
- **2.5 header-on-commit → 2.1.**
- **2.2 page-inspector + 2.3 overflow-and-freelist → 2.2 page-census.** Demoted from "decode every row of every kind of table" to "say what every page is for". Full record decoding is limited to the schema table and the recipe's own tables.
- **3.1 append-split + 3.2 middle-split-and-root-growth → 3.1 pages-per-insert.** Demoted from tracing which balance routine ran to counting which pages one insert changes, since changed pages are what a WAL shipper pays for.
- **4.1 journal-timeline + 4.3 hot-journal-rebuild → 4.1 atomic-commit-and-hot-journal.** SQLite does the recovery; the Go replayer is gone.
- **4.5 mmap-reads → one case in 4.2 cache-budget.**
- **5.2 wal-determinism → 5.1 wal-inspector.**
- **5.5 checkpoint-modes → 5.4**, trimmed to the four modes a platform issues.
- **5.6 commit-sync-levels → 7.2 io-cost-by-pattern.** One table of syncs per commit instead of two.
- **6.1 lock-bytes → 6.1 locks-belong-to-the-process.** Demoted from the table of byte offsets per lock level to the one hazard that follows from it.
- **6.2 busy-matrix + 6.3 busy-timeout-and-latency + 6.4 stale-snapshot → 6.2 busy-rules.** WAL mode in full, rollback mode as one contrast case.
- **8.2 descriptors-per-connection + 8.3 memory-per-connection → 8.2 idle-cost.**
- **9.2 backup-restarts → part of 9.1 capture-comparison.**
- **9.3 process-crash-recovery + 9.4 power-loss-is-different → 9.2 crash-versus-power-loss.** Power loss demoted to one file-level model (remove the WAL bytes written after the last sync, reopen, see which commits are gone) plus the platform implications. This is the whole of the power-loss material.
- **9.5 what-integrity-check-misses → 9.3**, three corruption sites instead of five.
- **10.1 bounded-fleet → a standing assertion in every chapter 10 experiment.** The bounds themselves are proven in 8.3.
- **10.3 snapshots-are-consistent → part of 10.4 rebuild-the-fleet.**
- **Chapter 7 as a whole:** from building VFS shims to reading and tracing the VFS with SQLite's own `vfstrace`.

Borderline keeps carry a "Why kept" line in OUTLINE.md: 1.2, 2.2, 3.1, 4.1, 5.4, 6.1, 7.2, 8.1, 9.3.

## Added or redone because of the two reference books

**"Production SQLite"** (221 pp). Mostly a general operations book: indexing, schema design,
JSON, full-text search, security, containers, ORMs, case studies. Those chapters do not serve
the North Star and nothing from them was added. Four things were:

1. **A capture method to test (6.4, 9.1).** Its "safe filesystem backup" procedure is: hold `BEGIN IMMEDIATE`, copy the database file and the `-wal`, commit. That is the cheapest live capture a platform could use, so 6.4 now tests a file-plus-WAL copy under a held transaction, and Astra checks the book's stated reason (that the lock stops checkpoints).
2. **Settings that do not travel with the file (8.1).** The book reports forgotten per-connection settings as a recurring production failure and notes that per-tenant databases mean per-database migrations. 8.1 now produces the table of what lives in the file versus in the connection, and the capstone catalog stores the second kind.
3. **Fleet-wide schema change (10.4).** A half-finished schema rollout is now part of the fleet rebuild: each rebuilt database must come back at the schema version the catalog records for it.
4. **Bounded restore (9.4).** From its description of Litestream's periodic snapshots: the shipper re-snapshots every *m* segments, and restoring from the newest or the oldest snapshot must give the same bytes.

It also confirmed two keeps. It states that WAL mode with `synchronous=NORMAL` syncs at every
commit and loses only uncommitted work; the Phase 1 probe counted zero syncs per commit. That
misstatement in a current book is why 7.2 and the model in 9.2 stay. And its list of per-database
things to monitor became one line of platform implications in chapters 5 and 10.

The book is not used as a source. Errors found while reading: wrong header offsets for the
payload fractions and counters, a wrong overflow threshold, "a commit frame has zeroed salts",
`flock` as the lock call, a `PRAGMA cache_shared` that does not exist, and the sync claim above.

**"SQLite Internals: How The World's Most Used Database Works"** (55 pp, unfinished). History,
then a restatement of the file-format and atomic-commit documentation with gaps marked TODO.
One addition: its description of the lock-byte page became an adversarial case in 2.2 (a file
over 1 GiB has one page the census must skip). Nothing else.

**What the books show can be cut.** Neither spends more than a page on bytecode, the parser or
B-tree balancing, and neither needs them to explain anything operational. That supports the
cuts in chapters 1 and 3. Neither treats power loss beyond "`synchronous` trades durability for
speed", which matches the level Nick asked for.

## The revised Go-access decision

No first-party cgo binding and no custom C. Go experiments use the stock driver
`modernc.org/sqlite v1.60.1`, which runs exactly SQLite 3.53.4 and needs no C compiler. Of the
three stock drivers it is the only one that, unmodified, has the page-level table functions
(`sqlite_dbpage`, `dbstat`), the backup API, per-connection memory counters and upstream's
default sync settings; mattn overrides `synchronous` and has no memory counters, and ncruces
replaces SQLite's Unix file layer with its own. VFS-level traces come from the pinned `sqlite3`
CLI's built-in `-vfstrace`, which is the real C code. Syscall traces and repeatable crashes come
from strace: its `--inject` option kills a writer on entry to a chosen `fsync`, and a probe run
for this revision gave identical outcomes on five of five runs at every kill point. The cost is
that the Go process runs a translation of the C rather than the C; experiment 1.2 compares the
two builds' I/O for the same statements, and later syscall claims repeat that comparison.
Nothing that remains in the outline needs a custom piece. The things only custom C could do
(an in-process VFS shim, a power-cut simulator, a write log) are cut. Probe details:
`docs/knowledge/library-route-probes.md`.

## Open questions for Nick

Answered by this direction:

| Rev 1 question | Answer |
| --- | --- |
| 3. Scale: 55 experiments | Trim: yes. 32 remain. |
| 4. cgo required | Existing libraries. No cgo or C is needed at all. |
| 5. Commit the generated amalgamation | No longer applies. |
| 6. Power-loss method | Understanding level. One model, no simulator. |
| 2. Who writes chapter prose | Answered by the Neon ruling of 2026-11-20 (Neon writes all prose and code comments and reviews Opus's code). Recorded in OUTLINE.md section 6. |

Still need a ruling (recommendations are in OUTLINE.md section 7):

- 1\. GitHub remote, or local only.
- 7\. Duplicate source copies versus symlinks.
- 8\. Shared packages versus self-contained chapters.
- 9\. Capstone scope: local stand-in store, or a real S3-compatible one.
- 10\. Linux only.
- 11\. A second SQLite version: now recommended as one adversarial case, not an experiment.
- 12\. Knowledge checks, and packaging for the `tutor` CLI.
- 13\. Where Astra's scripts are shown to readers.

New:

- 14\. The driver: modernc (recommended) or mattn if real C must run inside the Go process.
- 15\. Rollback-journal depth: one experiment left; the Go journal replayer is cut.
- 16\. No custom VFS is built. Does the platform plan include one?
- 17\. Comments in Astra's working scripts stay Astra's; Neon writes chapter text and experiment-code comments.

New: the cost of one Neon prose pass should be measured against the API budget before Phase 2
wave 2.
