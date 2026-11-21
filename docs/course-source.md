# SQLite Internals — AI-Guided Source Study Course

Source: Nick's original course specification, copied 2026-11-20. This file is the faithful text of that document and is the course spec.

Subtitle: A source-verified, experiment-driven curriculum for building platforms that manage many SQLite databases.

## Purpose

Develop a working understanding of SQLite's internals so that architectural decisions about large numbers of database files, connections, caching, durability, concurrency, backup, and migration are grounded in how SQLite actually works. This is not a course in implementing a SQL engine from scratch.

## Learning method

Use a pinned SQLite source revision, official SQLite documentation, and tests as the source of truth. AI acts as a guide: tracing mechanisms, explaining C functions, drawing architecture and protocol diagrams, designing short Go experiments, and reviewing observed results. Avoid generic explanations that cannot be tied to actual code.

Each chapter follows this sequence: (1) mental model and system boundaries; (2) a concrete question or operation; (3) source-level call path with file names and functions; (4) page, lock, or I/O timeline as appropriate; (5) a reproducible Go lab; (6) observable evidence and failure cases; (7) implications for a many-database platform; (8) a short knowledge check.

## Verification contract

Pin a specific SQLite release or commit and record it in every chapter. Link directly to the relevant source lines and official documentation. Distinguish confirmed implementation behavior from inference. Verify all API names and compile/run instructions. Do not invent call chains. Prefer a small runnable experiment over extensive untested sample code. Explain differences between SQLite versions when they materially affect behavior.

## Course chapters

### 1. Architecture and execution path
Trace sqlite3_prepare_v2, sqlite3_step, and sqlite3_finalize; understand the parser, planner, VDBE, B-tree, pager, and VFS boundaries. Sources: arch.html, vdbe.c, vdbeapi.c. Lab: use EXPLAIN and a Go program to observe SQL execution.

### 2. Database file format and pages
Read the database header, page sizes, record encoding, overflow pages, and freelists. Sources: fileformat2.html, btree.c, btreeInt.h. Lab: write a minimal read-only page inspector and validate it against SQLite-generated files.

### 3. B-trees and page management
Follow insertion, cell placement, page splitting, overflow handling, and page reuse. Sources: btree.c and btreeInt.h. Lab: force page splits and compare page layouts before and after.

### 4. Pager and page cache
Understand pager states, dirty pages, cache eviction, memory budgets, mmap, and sync boundaries. Sources: pager.c, pcache.c, pcache1.c. Lab: measure cache and memory behavior with bounded and unbounded connection counts.

### 5. WAL, transactions, and checkpoints
Inspect WAL headers, frames, commit markers, reader snapshots, and checkpoint behavior. Sources: wal.html, walformat.html, wal.c, pager.c. Lab: build a WAL frame inspector and measure WAL growth under a long-lived reader.

### 6. Locking and concurrent access
Study reader/writer interactions, busy handlers, locking modes, and process boundaries. Sources: wal.c, os_unix.c, isolation.html. Lab: run multiple processes and record contention, busy results, and writer latency.

### 7. VFS and filesystem I/O
Trace open, read, write, sync, locking, and file deletion through the VFS. Sources: vfs.html, os_unix.c, os.h. Lab: build or adapt a logging VFS and compare observed I/O under different transaction patterns.

### 8. Connection and database lifecycle
Study open/close cost, prepared statements, ATTACH, per-connection state, file descriptors, and idle database policies. Sources: main.c, attach.c and SQLite API documentation. Lab: benchmark opening and querying hundreds or thousands of SQLite files under a bounded connection manager.

### 9. Backup, crash recovery, and integrity
Understand consistent snapshots, backup API, checkpoint interactions, power-loss assumptions, and integrity checks. Sources: backup.html, pager.c, wal.c. Lab: test process-crash recovery and compare backup approaches; do not equate process crashes with power-loss durability.

### 10. Multi-database platform capstone
Design a Go service that creates and routes to many SQLite files, bounds open connections and cache memory, tracks per-file ownership, snapshots consistently, and supports safe relocation. Separate SQLite's local guarantees from distributed coordination and object-storage durability. Lab: build a small working service and test invariants under faults.

## Platform design questions

How many connections and file descriptors can remain open? How much cache memory is consumed per database? What is the write-ownership model? What happens when a reader blocks checkpoints? How are consistent snapshots produced? How is a database moved between machines without two writers? Which metadata belongs in a global control plane versus local SQLite files? What is the recovery path after an interrupted upload or ownership transfer?

## Capstone invariants

At most one authorized writer owns a database at a time. No acknowledged durable write is lost under the stated durability model. Every published snapshot is internally consistent. A database can be reopened after process failure. File descriptors and memory remain bounded. An interrupted migration cannot silently create two authoritative copies. Test these with deterministic state-machine tests where practical, plus process-level fault injection.

## AI tutor prompt (from the doc)

Act as a SQLite internals instructor and source-code reviewer. Use the official SQLite repository at a clearly stated pinned revision. Teach one chapter at a time. First provide a concise mechanism explanation and an architecture diagram. Then trace the real code with exact source paths, function names, and links. State what is verified and what is inferred. Provide one short, runnable Go experiment with expected observations and a way to inspect actual behavior. Include concurrency, durability, or failure scenarios when relevant. Conclude with implications for a platform managing many SQLite database files and five comprehension questions. Do not introduce unrelated database internals or long implementation exercises. Wait for my observations before advancing.

## Primary references

- SQLite architecture: https://www.sqlite.org/arch.html
- Database file format: https://www.sqlite.org/fileformat2.html
- WAL: https://www.sqlite.org/wal.html
- WAL format: https://www.sqlite.org/walformat.html
- VFS: https://www.sqlite.org/vfs.html
- Backup API: https://www.sqlite.org/backup.html
- Isolation: https://www.sqlite.org/isolation.html
- SQLite source mirror: https://github.com/sqlite/sqlite

## Reading strategy (from the doc)

Make the AI/source curriculum primary. Use the unfinished Leanpub SQLite Internals book only where its chapters are complete and useful; use Inside SQLite as a historical architectural supplement, not an authority on modern WAL behavior. Read Database Internals selectively for B-trees, storage layout, buffering, and durability concepts. Optimize for a verified mental model and repeatable experiments rather than page count.
