# OUTLINE — SQLite Internals, experiment-driven build

Status: Phase 1 deliverable, awaiting Nick's sign-off. Nothing in Phase 2 starts before that.

Course spec: [docs/course-source.md](docs/course-source.md). Research behind this outline (all
run against the pinned source on this machine, 2026-11-20):

- [docs/knowledge/source-inventory-ch01-04.md](docs/knowledge/source-inventory-ch01-04.md) — function spans and call paths, chapters 1–4
- [docs/knowledge/source-inventory-ch05-09.md](docs/knowledge/source-inventory-ch05-09.md) — the same for chapters 5–9, plus fsync and file-descriptor probes
- [docs/knowledge/determinism-and-rebuild.md](docs/knowledge/determinism-and-rebuild.md) — what makes a database file reproducible, and every capture method tested
- [docs/knowledge/go-access-and-vfs.md](docs/knowledge/go-access-and-vfs.md) — how Go reaches SQLite and how a VFS shim is built

Every probe script those reports ran is in `agent_work/probes/` and indexed in
[agent_work/INDEX.md](agent_work/INDEX.md).

Two labels are used on experiments below:

- **[probed]** — a Phase 1 probe already demonstrated the claim on this machine.
- **[unprobed]** — the claim comes from the source or the official docs and has not been run
  yet. The Opus builder confirms it first and reports back if it is false; the chapter is then
  corrected rather than the evidence bent.

---

## 1. Pinned SQLite revision

**Pin: SQLite 3.53.4**, source tag `version-3.53.4`.

| Identifier | Value |
| --- | --- |
| Release | 3.53.4, dated 2026-07-24 19:02:57 UTC |
| Git commit (github.com/sqlite/sqlite) | `b09c88c14082339b66c7b7158d609a771e64ca69` |
| Fossil check-in (`manifest.uuid`, also `sqlite_source_id()`) | `bf7c7f30031888f4e796e429ab3978879485813aaca6f641c7b33e4e09459bcc` |
| Local source tree | `/root/Software/sqlite-src/3.53.4` (re-create with `agent_work/fetch-pinned-sqlite.sh`) |

**Why this revision.** The `sqlite3` CLI already installed here reports exactly that Fossil
check-in, and the tag `version-3.53.4` on the official GitHub mirror has the same
`manifest.uuid`. So "the installed binary" and "a source tag" are the same pin, and there is no
choice to make between them: the course pins to the **source tag** and treats the installed CLI
as one convenient build of it. Newer releases exist (3.54.0 and 3.54.1 are tagged), but moving
to them would mean replacing the system binary for no teaching gain, and 3.53.4 already
includes the WAL-reset corruption fix (3.51.3 / 3.53.0) and `SQLITE_CHECKPOINT_NOOP` (3.51.0).

**The pin is the source, not any one binary.** The installed CLI and the installed
`/usr/local/lib/libsqlite3` are the same check-in built with different options, and neither has
snapshot, session or debug support. The course therefore compiles its own library from the
pinned tree (section 4.1) with a fixed, recorded option list. Experiments run against that build.

**How each chapter records the pin.**

1. Every chapter `.md` opens with a pin block: release, tag, git commit, Fossil check-in, and
   the sha256 of the generated amalgamation `sqlite3.c`.
2. Every chapter directory has `SOURCES.sha256`: one line per copied SQLite file with its
   path in the pinned tree and its sha256. A repo script checks each copy against the pinned
   tree, so a copied file can never drift from the pin.
3. Every source reference in the text is `file:start-end` at the pin plus a link of the form
   `https://github.com/sqlite/sqlite/blob/version-3.53.4/src/wal.c#L2199-L2395`.
4. Every experiment prints `sqlite_source_id()` as its first output line and exits non-zero
   if it is not the pinned check-in. A reader on a different build finds out at once.
5. Where behavior differs across versions, the chapter has a short "Version notes" list drawn
   from the research reports (each item cites the changelog entry or the source comment).

---

## 2. Repo layout

```
sqlite-internals/
  OUTLINE.md  README.md  STATE.md  go.mod            one Go module for the whole course
  docs/
    course-source.md                                 Nick's spec
    knowledge/                                       research reports and durable findings
  sqlite/
    PIN                                              tag, git commit, Fossil id, amalgamation sha256
    build-amalgamation.sh                            pinned tree -> sqlite3.c / sqlite3.h
  internal/
    sqlite/       cgo binding to the pinned build (+ vfstrace and fault shims)      chunk 0
    sqlite/amalg/ sqlite3.c, sqlite3.h and the compile flags, nothing else          chunk 0
    recipe/       seeded, deterministic workload generator                          ch 1
    dbfile/       read-only database file parser (header, pages, cells, freelist)   ch 2
    equal/        the equality ladder: sha256, page diff, logical hash              ch 2–3
    journalfile/  rollback-journal parser and replayer                              ch 4
    walfile/      WAL parser, checksum chain, frame replayer                        ch 5
    procs/        child-process roles and step barriers for multi-process labs      ch 6
    capturevfs/   Go side of the write-log and power-cut VFS shims                  ch 7
    walship/      snapshot + WAL-segment capture and restore                        ch 9
  ch01-architecture-and-execution-path/
    ch01-architecture-and-execution-path.md          the chapter
    SOURCES.sha256
    vdbeapi.c  vdbe.c  vdbeaux.c  prepare.c ...       copies of the SQLite files it cites
    experiments/
      1.1-pin-check/main.go
      1.2-explain-walk/main.go
      ...
    evidence/                                        captured output of each experiment
  ch02-file-format-and-pages/                        same shape for every chapter
  ch03-btrees-and-page-management/
  ch04-pager-and-page-cache/
  ch05-wal-transactions-checkpoints/
  ch06-locking-and-concurrency/
  ch07-vfs-and-filesystem-io/
  ch08-connection-and-database-lifecycle/
  ch09-backup-crash-recovery-integrity/
  ch10-multi-database-platform-capstone/
  agent_work/
    INDEX.md                                         every script: name, purpose, chapter, maker, what it proved
    probes/                                          Phase 1 feasibility probes
    ch01/ ... ch10/                                  Phase 2 working scripts and Astra's adversarial scripts
```

Rules that implement Nick's specs 3, 5 and 6:

- **Chapter file name = directory name + `.md`.** One chapter per directory.
- **Copied SQLite files sit at the chapter directory's root**, under their real names, whole
  and unmodified. A file cited by two chapters is copied into both (for example `btree.c` in
  chapters 2 and 3, `pager.c` in 4 and 5). Very large files cited for a single function are
  linked instead of copied; each chapter's list below says which.
- **Experiments are numbered `N.M`** and live in `experiments/N.M-name/main.go`. Run command,
  from the repo root, is always `go run ./chNN-…/experiments/N.M-name` (flags listed in the
  chapter). Scratch databases go under `_work/N.M/`, which is git-ignored and safe to delete.
- **The chapter inlines the Go code** of each experiment at the point in the reading where it
  belongs, followed by the run command and the expected evidence. A repo check compares each
  inlined block with the file on disk byte for byte, so text and code cannot diverge.
- **Shared packages are inlined once**, in the chapter that introduces them (the table above
  names the owner). Later chapters import them and link back.
- **`agent_work/` holds every working script** anyone writes: probes, one-off checks, Astra's
  fault-injection and falsification scripts. Each gets an INDEX row in the same commit. The
  finished experiments are course content and live in the chapter; everything used to get
  there lives in `agent_work/`.

Evidence is stated in one of three forms, and each experiment says which:

- **EXACT** — a hash, byte count, frame count or return code that must match on every run.
- **BOUND** — an inequality that must hold (for example "open file descriptors ≤ 3 × cap + 8").
- **SHAPE** — a trend that must hold (for example "WAL size grows while the reader is open and
  stops growing after it closes"). Used only where timing or the allocator makes exact numbers
  impossible; timings themselves are never the pass condition.

---

## 3. The through-line: the Rebuild Ladder

Nick's first spec asks for one arc: capture a database's durable state deterministically,
rebuild a working database from it, and prove the rebuild by experiment. The course calls this
arc the **Rebuild Ladder**. Each chapter adds one rung: a way to capture state, a rebuild from
that capture, and a check that the rebuild equals the original.

**What "equal" means.** Three levels, used throughout, implemented once in `internal/equal`:

| Level | Check | When it is the right claim |
| --- | --- | --- |
| **E1 bytes** | sha256 of the database file | Physical captures: page copies, WAL replay, I/O log replay |
| **E2 pages** | Page-by-page diff, ignoring a declared list of header bytes: 24–27 (change counter), 40–43 (schema cookie), 92–99 (version-valid-for, library version) | The backup API, which rewrites those fields |
| **E3 logical** | A Go logical hash (schema, every row with explicit rowid, every value tagged with its storage class, `sqlite_sequence`, persistent pragmas) plus `integrity_check` = ok plus unchanged `foreign_key_check` | Captures that repack the file: dumps, changesets, `VACUUM INTO` |

E3 can be turned into a byte check: `VACUUM INTO` both sides and compare sha256. Phase 1 probes
showed `VACUUM INTO` gives identical bytes for the same logical content however it was built.

The Go logical hash is ours because the in-tree tools have blind spots that the probes found:
`dbhash` ignores implicit rowids, `sqlite_sequence` and `user_version`; `sqldiff` reports
integer `1` and real `1.0` as equal.

**The ladder.**

| Rung | Chapter | Capture | Rebuild | Proven equal at |
| --- | --- | --- | --- | --- |
| R0 Recipe | 1 | The seeded SQL workload itself | Run it again into an empty file | E1 [probed] |
| R1 Pages | 2 | Every page of a closed database, read by our own parser, plus a per-page hash manifest | Write the pages back in order | E1 |
| R2 Canonical form | 3 | Logical content; `VACUUM INTO` as the canonical physical form | Replay rows into a fresh file | E3, and E1 between canonical forms [probed] |
| R3 Undo log | 4 | The rollback journal (original page images) | Play the journal back onto the half-written file in Go | E1 against the pre-transaction file [unprobed] |
| R4 Redo log | 5 | Base file + committed WAL frames | Apply frames in Go, stop at the last commit, truncate to the commit size | E1 against SQLite's own `wal_checkpoint(TRUNCATE)` [probed] |
| R5 Live snapshot | 6 | All pages read inside one read transaction while writers run | Write the pages out | E1 against the state at the reader's snapshot |
| R6 I/O log | 7 | Every write and sync the VFS sees, in order | Replay the writes into empty files | E1 for the database and the WAL; also the base for power-cut tests [probed in part] |
| R7 At-rest state | 8 | The single file left after the last connection closes | Copy it | E1; hash recorded on close and checked on reopen |
| R8 Point in time | 9 | One snapshot plus WAL segments shipped as they are written, across checkpoints | Restore to any commit *k* | E1 against the known state after commit *k* |
| R9 Fleet | 10 | R8 for every database a service owns, plus ownership records | Wipe a node and rebuild every database from the store | E1 per database, under injected faults |

**Why this order holds up.**

- R0 comes first because every later rung needs a ground truth. A deterministic workload gives
  a known state after every commit *k*, with a known hash. Later chapters can then say exactly
  which state a crashed, restored or relocated database must equal. Probes confirmed the same
  script gives byte-identical files across seven configurations and two builds of 3.53.4.
- R1 before R4: a WAL frame is a page image, so the page reader must exist before the frame
  reader.
- R3 and R4 are the two logs SQLite itself uses to rebuild a database after a crash. Replaying
  each one in Go, and matching SQLite's own result byte for byte, is the strongest evidence the
  course can give that the reader understands the format.
- R5 and R7 are the two safe moments to capture a live database without reading the log:
  inside a read transaction, and after the last close. R6 shows the same state as the
  filesystem sees it and supplies the fault-injection tool that R8 and R9 need.
- R8 is R1 + R4 made continuous. R9 is R8 with ownership, which is where SQLite's guarantees
  stop and the platform's begin.

**Determinism rules** (section 4.2) apply to every rung. Without them "rebuilt equal" would be
luck.

---

## 4. Key technical decisions

### 4.1 How Go reaches SQLite

**Decision: one first-party cgo package over the pinned amalgamation is the primary route.**
The amalgamation generated from the pinned tree (`./configure && make sqlite3.c`) is
byte-identical to the official `sqlite-amalgamation-3530400.zip` (sha256 of `sqlite3.c`:
`b1dd5d74ec7f29055a6684fa06fb3c2f6821c87dd38f9a458dfd2e8a1db28189`) [probed]. A working wrapper
is about 135 lines of Go [probed].

Why not a stock driver as the primary route [all probed]:

| Route | Runs exactly 3.53.4? | Runs the real `os_unix.c`? | Problem for this course |
| --- | --- | --- | --- |
| mattn/go-sqlite3 (cgo) | Only at v1.14.52; latest is 3.54.1 | Yes | Forces `synchronous=NORMAL` and a 5 s busy timeout on every connection; no snapshot, WAL hook, `db_status` or checkpoint counts |
| modernc.org/sqlite (transpiled) | Only at v1.60.1 | A transpiled copy | Low-level APIs reachable only through its internal `lib` package; its `vfs` package is read-only |
| ncruces/go-sqlite3 (wasm) | Only at v0.35.6 with a forced dependency downgrade | **No** — replaces it with a Go layer using different lock calls | Chapters 6 and 7 teach `os_unix.c`; traces through this driver would not match the source |
| First-party cgo on the pinned amalgamation | **Yes, by construction** | **Yes** | Needs gcc and a one-time ~85 s compile |

Where each route is used:

| Experiment type | Route | Why |
| --- | --- | --- |
| Everything by default (chapters 1–10) | `internal/sqlite` (first-party cgo) | Exact pin; our compile options (`ENABLE_SNAPSHOT`, `ENABLE_SESSION`, `ENABLE_PREUPDATE_HOOK`, `ENABLE_DBPAGE_VTAB`, `ENABLE_DBSTAT_VTAB`, `ENABLE_STMTVTAB`, `ENABLE_EXPLAIN_COMMENTS`); every C API the labs need; C VFS shims that call back into Go |
| Reading files (page inspector, WAL inspector, journal and WAL replay, hashing) | Plain Go, no SQLite at all | The point of those labs is that our reader agrees with SQLite's writer |
| The "other process" in multi-process labs; checks a reader can retype | The pinned `sqlite3` CLI via `os/exec` | Independent build of the same check-in; has `-vfstrace`, `.dbinfo`, `.filectrl` |
| Syscall ground truth (fsync order, directory fsync, lock byte ranges) | `strace` on the Go process, merged with the VFS trace | Only strace sees the directory fsync and the real `fcntl` ranges |
| One short aside in chapter 9 on driver defaults | mattn v1.14.52 in its own `main` package | Shows that a driver's defaults change durability; never linked into the same binary as our package (duplicate symbols) |
| In-tree tools as cross-checks (`showwal`, `showdb`, `dbhash`, `sqldiff`, `sqlite3_rsync`) | Built from the pinned tree by a repo script | All build with a plain `./configure` [probed]; they check our Go parsers and hashes |

Module layout: **one `go.mod` at the repo root.** The amalgamation sits alone in
`internal/sqlite/amalg` so that editing the wrapper does not recompile SQLite (75 s drops to
3 s) [probed]. `-tags sqlite_debug` gives an assert-enabled build for chapters that want
`PRAGMA vdbe_trace`.

Three rules the probes showed are needed:

- **Never open and close the database file with `os.Open` in a process that also has a
  SQLite connection to it.** Closing any descriptor drops that process's POSIX locks on the
  file. Experiments hash or copy database files only after closing the connection, or from a
  child process. Chapter 6 teaches this hazard on purpose.
- **Crash points are chosen by the fault shim, not by timing a signal.** The shim exits the
  process at write or sync number *k*, which makes every crash experiment repeatable [probed].
- **Each experiment prints `sqlite_source_id()` first and fails if it is not the pin.**

### 4.2 Determinism, and how a rebuilt database is verified

**Enforced in `internal/recipe` and `internal/sqlite`, for every experiment that makes a
byte-level claim:**

| Source of variation | Rule | Basis |
| --- | --- | --- |
| Library build | One build: the pinned amalgamation with the recorded option list. Every experiment checks `sqlite_source_id()`. | Header byte 96 stores the writer's version; other versions are untested |
| Persistent file settings | Set explicitly on creation: `page_size=4096`, `auto_vacuum=NONE`, `encoding='UTF-8'`, `application_id` (one fixed course value), `user_version` (the recipe's step number), journal mode | These are stored in the file [probed] |
| Per-connection settings | Set on every connection: `synchronous`, `foreign_keys`, `secure_delete`, `cache_size`, `wal_autocheckpoint`, `busy_timeout` | These reset on every open [probed] |
| Data | Generated in Go from a seeded PRNG (`math/rand/v2` PCG with a fixed seed); values are bound as parameters | No SQL `random()`, `randomblob()`, `current_timestamp`, `date('now')` [probed: each one breaks equality] |
| Row identity | Every table has an explicit integer primary key and every insert supplies it | Rowid choice past the maximum is random [probed] |
| Row order | Every `INSERT … SELECT` has `ORDER BY` | Unordered inserts follow the query plan [probed] |
| Transaction boundaries | Part of the recipe: step *k* is exactly one transaction | Same rows in different batches give different bytes [probed] |
| Freed pages | `secure_delete=ON` where two different histories must match | Freelist leaves keep stale bytes otherwise [probed] |
| WAL salts | Seed SQLite's PRNG with `sqlite3_test_control(SQLITE_TESTCTRL_PRNG_SEED, …)` where the WAL file itself must match byte for byte; otherwise compare the sequence of (page number, page hash, commit size) | Salts and checksums are the only run-to-run difference in a WAL [probed] |
| Process and clock | One writer goroutine per database; no wall-clock values stored; multi-process labs use step barriers, not sleeps | Interleavings become part of the script |
| Filesystem | Experiments run on the local ext4 volume under `_work/`; the chapter states this | fsync behavior is filesystem-specific |

**Verification.** The three equality levels in section 3. The rule for choosing: claim the
strongest level the capture method can reach, and say why it cannot reach the next one.

| Capture method | Strongest level reached | Evidence |
| --- | --- | --- |
| Recipe re-run, same boundaries | E1 | [probed] |
| File copy of a closed database | E1 | [probed] |
| Pages via `sqlite_dbpage` or `sqlite3_serialize` | E1 | [probed] |
| Our WAL replay vs SQLite's checkpoint | E1 | [probed], including an uncommitted tail and a corrupted last frame |
| Backup API | E2 (differs only at header bytes 24, 40, 92) | [probed] |
| `VACUUM INTO`, dump and replay, session changesets | E3; E1 after canonicalizing both sides | [probed] |
| Copy of a live database | None: 8 of 8 copies taken under a writer failed `integrity_check` | [probed]; taught as the failure case |

Byte hashes alone would reject correct logical rebuilds, and logical diffs alone would accept a
physically wrong page copy. The course needs all three and teaches when each applies.

### 4.3 Which chapters need a custom VFS

| Chapter | Needs a VFS shim? | What for |
| --- | --- | --- |
| 1 | Logging only (reuses the chunk-0 shim) | One experiment shows a statement crossing the VFS boundary |
| 2, 3 | No | The file on disk is the evidence |
| 4 | Logging, plus the exit-at-sync fault | Journal and sync order; a repeatable crash mid-commit |
| 5 | Logging, plus the exit-at-sync fault | fsync per commit by `synchronous` level; checkpoint I/O |
| 6 | Logging of lock calls, plus strace | Lock calls and the real byte ranges |
| 7 | **Yes — this is the chapter.** Logging, write-log capture, and the power-cut shim | The VFS itself |
| 8 | Logging only | Lazy schema load, what close does |
| 9 | **Yes** — exit-at-sync and power-cut shims from chapter 7 | Crash versus power loss |
| 10 | **Yes** — same shims, plus fault injection in the object-store stand-in (plain Go) | Invariants under faults |

**How a "Go VFS" is feasible.** The VFS methods are C function pointers, so the shim's method
table is C. The course uses three small C shims compiled into `internal/sqlite`, each
wrapping the real `unix` VFS so that `os_unix.c` still does all the work:

1. **Logging:** SQLite's own `ext/misc/vfstrace.c` from the pinned tree, with its output
   callback pointed at an exported Go function. 22 lines of C glue and 69 of Go [probed]. It
   sees every method including shared-memory locks and file controls. Two quirks the chapter
   states: the file needs two typedefs to compile outside the shell, and it prints `FULL` for
   a `NORMAL` sync.
2. **Fault:** a pass-through shim (82 lines of C) that fails, or exits the process at, the
   Nth write or sync [probed, repeatable].
3. **Write-log and power-cut** (built in chapter 7): a pass-through shim that hands every
   write, truncate, sync and delete to Go. Go keeps the ordered log (rung R6). In power-cut
   mode the shim holds writes back until the file is synced and, at the chosen point, drops or
   tears the unsynced ones and exits. The model follows the in-tree `src/test6.c`, which
   cannot be used directly because it is bound to the Tcl test harness. **[unprobed]** —
   designed in Phase 1, not built; the estimate is about 300 lines of C plus the Go side.

A VFS written entirely in Go is possible only with the ncruces driver, which does not run
`os_unix.c` and cannot see shared-memory locking, so it is not used.

Kernel-level power-loss tools (`dm-log-writes`, `dm-flakey`) are present on this VM but not
loaded. The course does not depend on them: they need root, a loop device and a replay tool
that is not installed. They are listed as an optional appendix for chapter 9 (see open
question 6).

---

## 5. Chapters

Each chapter follows the course doc's eight-step sequence. The section list under each chapter
uses these step names:

1. **Model** — mental model and system boundaries, with a diagram
2. **Question** — the concrete question or operation
3. **Call path** — source-level path with files, functions and line spans at the pin
4. **Timeline** — page, lock or I/O timeline
5. **Lab** — the Go experiments, each inlined at its place in the reading
6. **Evidence and failure cases**
7. **Platform implications** — for a service managing many database files
8. **Knowledge check** — five questions

Experiments appear inside steps 3–6 wherever the reading reaches the thing they prove; the
"placed at" note on each experiment says where. Line spans below come from the research
inventories and are re-checked by script when the chapter is built.

### Chapter 1 — Architecture and execution path

Directory `ch01-architecture-and-execution-path/`, file `ch01-architecture-and-execution-path.md`.

**Question.** When a Go program runs one `INSERT` and one `SELECT`, which layer does what
between `sqlite3_prepare_v2` and the bytes on disk, and how can each handoff be observed from
outside the library?

**Reading path** (in order):

1. https://www.sqlite.org/arch.html — name the layers
2. `vdbeapi.c` `sqlite3_step` → `sqlite3Step` (919–966, 777–912)
3. `prepare.c` `sqlite3LockAndPrepare` → `sqlite3Prepare` (836–875, 682–835)
4. `tokenize.c` `sqlite3RunParser` (600–763); `parse.y` 172–181 for the EXPLAIN flag
5. `build.c` `sqlite3FinishCoding` (141–278) — where `OP_Init`, `OP_Transaction`, `OP_Halt` come from
6. https://www.sqlite.org/opcode.html, then each opcode of the lab's programs in `vdbe.c`
7. `vdbe.c` `OP_Transaction` (4102–4201), `OP_Insert` (5748–5834) — the B-tree boundary
8. `vdbeaux.c` `sqlite3VdbeHalt` → `vdbeCommit` (3315–3517, 2919–3175) — the pager boundary
9. `os.c` `sqlite3OsRead/Write/Sync` (88–102) — the VFS boundary
10. https://www.sqlite.org/c3ref/stmt_status.html and `/c3ref/trace_v2.html`

**Copied into the chapter directory:** `vdbeapi.c`, `vdbe.c`, `vdbeaux.c`, `prepare.c`,
`tokenize.c`, `parse.y`, `vdbevtab.c`, `os.c`. **Linked, not copied:** `build.c` (one function
cited), `select.c`, `where.c`, `insert.c`.

**Sections.**

1. Model — the layer diagram (interface, tokenizer/parser, code generator, VDBE, B-tree, pager, VFS) and what each owns.
2. Question — one `INSERT`, one `SELECT`, followed end to end. Experiment 1.1 here.
3. Call path — prepare, step, finalize with spans; version note that record encoding moved into `OP_MakeRecord` in 2022. Experiment 1.2 after the prepare path.
4. Timeline — the opcode sequence of the `INSERT` laid against layer crossings. Experiment 1.3.
5. Lab — the deterministic workload that every later chapter reuses. Experiment 1.4.
6. Evidence and failure cases — what breaks determinism. Experiment 1.5.
7. Platform implications — statements are compiled per connection; the layers a platform can observe or replace (VFS) versus those it cannot.
8. Knowledge check.

**Experiments.**

**1.1 pin-check** · EXACT · placed at step 2 · [probed]
- Claim: this course's library is SQLite 3.53.4 at the pinned check-in, built with the recorded options.
- Does: opens an in-memory database through `internal/sqlite`; reads `sqlite_version()`, `sqlite_source_id()`, `PRAGMA compile_options`; runs the installed CLI for the same two values.
- Evidence: both source ids equal `bf7c7f30…59bcc`; the option list equals the list in `sqlite/PIN`.
- Astra will try: build with a different amalgamation or a changed flag and confirm the experiment fails loudly; confirm every other experiment refuses to run on the wrong build.

**1.2 explain-walk** · EXACT · placed at step 3 · [probed for the hooks, unprobed for the exact listing]
- Claim: `prepare` produces a VDBE program and `step` only executes it; the program for a point `SELECT` and for an `INSERT` can be read back opcode by opcode.
- Does: prepares both statements; lists the programs through the `bytecode()` table function and `EXPLAIN`; steps them; reads `sqlite3_stmt_status` (`VM_STEP`, `FULLSCAN_STEP`) and installs `sqlite3_trace_v2` for statement, row and profile events.
- Evidence: the opcode lists (exact at the pin, stored in `evidence/`); `VM_STEP` equals the number of opcodes executed, which the reader can count from the listing; the trace shows prepare producing no row events and each `step` producing one.
- Astra will try: add an index and confirm the program changes while the result does not; check the stored listing against the pinned CLI's `EXPLAIN`.

**1.3 layer-crossings** · EXACT · placed at step 4 · [probed for vfstrace]
- Claim: one committed `INSERT` crosses every layer, and each crossing is visible: opcodes (VDBE), a changed page (B-tree), journal and database writes (pager), `xWrite` and `xSync` (VFS).
- Does: runs one `INSERT` in rollback-journal mode with the logging VFS on; records the opcode list, the page that changed (page hashes before and after, through `sqlite_dbpage`), and the VFS call log.
- Evidence: one table of opcode → changed page number → VFS calls with file and offset; the changed page number times 4096 minus 4096 equals the `xWrite` offset on the database file.
- Astra will try: repeat in WAL mode and confirm the database-file write disappears from the commit (it moves to the WAL), which the chapter text must predict.

**1.4 recipe-rebuild (rung R0)** · EXACT · placed at step 5 · [probed]
- Claim: a database built by a fixed, seeded workload is reproducible to the byte; the workload is therefore a complete saved state.
- Does: introduces `internal/recipe`: a seeded generator producing a schema and *N* numbered transactions (inserts, updates, deletes, one index, rows large enough to overflow). Runs it twice into two files, closing each; hashes both. Prints the hash after each step *k* for a short run.
- Captures / rebuilds: capture is the recipe (seed, step count); rebuild is running it again.
- Evidence: the two sha256 values are equal; the per-step hash list is stored and is identical on a second run.
- Astra will try: run under different working directories, file names, `cache_size` values and machine load; run 50 times; build once in rollback mode and once in WAL mode with a final checkpoint and confirm the files differ only where the chapter says (bytes 18–19 and the pages the research report names).

**1.5 broken-recipes** · EXACT · placed at step 6 · [probed]
- Claim: each determinism rule in section 4.2 is necessary: dropping any one makes two runs differ.
- Does: runs five variants twice each: SQL `random()`; `current_timestamp`; an unordered `INSERT … SELECT` with and without an index; the same rows in different transaction batches; rowid assignment past the maximum rowid.
- Evidence: for each variant the two hashes differ (or, for the index case, the rowids differ), with the first differing page reported; the unmodified recipe still matches.
- Astra will try: find a sixth source the outline missed (for example `PRAGMA encoding`, floating-point text, `ANALYZE`) and show whether it breaks equality.

**Feeds the through-line.** R0. This chapter creates the ground truth every later rung is
checked against: the state after step *k* has a known hash.

---

### Chapter 2 — Database file format and pages

Directory `ch02-file-format-and-pages/`, file `ch02-file-format-and-pages.md`.

**Question.** Given only the bytes of a database file, can a Go program with no SQLite in it
recover the header, every page's role, and every row, and agree with SQLite on all of it?

**Reading path:**

1. https://www.sqlite.org/fileformat2.html — header and B-tree page sections
2. `btreeInt.h` 1–228 — the format as the authors describe it
3. `btree.c` `newDatabase` (3510–3547) — header bytes written at creation
4. `btree.c` `lockBtree` (3282–3459) — header bytes checked at open
5. `btree.c` `decodeFlags`, `btreeInitPage` (2028–2085, 2218–2265) — page header
6. `btree.c` `btreeParseCellPtr`, `btreeParseCellPtrIndex` (1259–1385) — cells and the local/overflow split
7. `util.c` `sqlite3GetVarint`, `putVarint64` (1633–1781, 1579–1602)
8. `vdbeaux.c` serial types (3850–3903, `sqlite3VdbeSerialGet` 4123–4214); `vdbe.c` `OP_MakeRecord` (3469–3785)
9. `btree.c` `getOverflowPage`, `accessPayload` (5007–5062, 5125–5326)
10. `btree.c` `allocateBtreePage`, `freePage2` (6514–6822, 6836–6969); `pager.c` `pager_write_changecounter` (3130–3143)

**Copied:** `btreeInt.h`, `btree.c`, `util.c`, `vdbeaux.c`, `dbpage.c`, `dbstat.c`.
**Linked:** `pager.c` (copied in chapter 4), `vdbe.c` (copied in chapter 1).

**Sections.**

1. Model — a file is an array of fixed-size pages; page 1 carries the 100-byte header; page roles (table/index, interior/leaf, overflow, freelist trunk/leaf, pointer map).
2. Question — decode the R0 database from raw bytes.
3. Call path — where SQLite writes and checks each header field; page and cell parsing. Experiments 2.1 and 2.2 sit here, each right after the code it mirrors.
4. Timeline — which header bytes change on a commit (change counter, version-valid-for, size, freelist fields).
5. Lab — the read-only page inspector (the course doc's lab). Experiments 2.2–2.4.
6. Evidence and failure cases — Experiment 2.5; what the inspector must reject.
7. Platform implications — page size is fixed at creation; the header is a cheap fingerprint; why bytes 24 and 92 matter to anything that caches or copies files.
8. Knowledge check.

**Experiments.**

**2.1 header-reader** · EXACT · placed at step 3 · [probed for the field list]
- Claim: the 100-byte header can be decoded field by field, and every field matches what SQLite reports.
- Does: `internal/dbfile` parses the header of the R0 database; compares each field with the matching pragma (`page_size`, `page_count`, `freelist_count`, `schema_version`, `user_version`, `application_id`, `encoding`, `auto_vacuum`) and with the CLI's `.dbinfo`.
- Evidence: a field table with "ours / SQLite's / match"; all rows match. Byte 96 reads 3053004.
- Astra will try: feed it databases with other page sizes (512, 65536, stored as 1), WAL-mode files, UTF-16 files, and a truncated file.

**2.2 page-inspector** · EXACT · placed at step 3 and 5 · [unprobed; the oracles are probed]
- Claim: every B-tree page can be decoded (header, cell pointer array, cells, varints, record serial types) into the same rows SQL returns.
- Does: walks the table and index B-trees from `sqlite_schema` on page 1; decodes every cell into typed values; compares rows with `SELECT` output, and page type, cell count and unused bytes with `dbstat`, and raw page bytes with `sqlite_dbpage`.
- Evidence: row count and a row-by-row hash equal SQLite's for every table; per-page `ncell` and `unused` equal `dbstat`'s for every page.
- Astra will try: generated schemas with every storage class, negative and 8-byte integers, empty strings, NULLs, `WITHOUT ROWID` tables, and index pages; a fuzzer that mutates one byte of a page and checks the inspector reports an error rather than panicking.

**2.3 overflow-and-freelist** · EXACT · placed at step 5 · [unprobed]
- Claim: payloads larger than the local limit continue on a linked chain of overflow pages, and deleted pages are tracked on a trunk/leaf freelist whose length the header records.
- Does: follows the overflow chain of the recipe's large rows and reassembles the payload; walks the freelist from header offset 32; accounts for every page in the file as exactly one of: B-tree, overflow, freelist, (pointer map if enabled).
- Evidence: reassembled payload hash equals `SELECT` value hash; freelist length equals `PRAGMA freelist_count`; the page census sums to `page_count` with no page counted twice or missed.
- Astra will try: an `auto_vacuum` database (pointer-map pages appear); a payload exactly at the local limit and one byte over; a freelist longer than one trunk page.

**2.4 page-manifest (rung R1)** · EXACT · placed at step 5 · [probed: closed-file copy and page export are byte-identical]
- Claim: the ordered list of page images is the complete state of a closed database; writing those pages back rebuilds the file exactly.
- Does: reads every page of the closed R0 database with `internal/dbfile`; writes a capture (one manifest line per page: number, role, sha256; plus the page images); rebuilds a new file from the capture alone; opens the rebuild with SQLite and runs `integrity_check`. Introduces `internal/equal` level E1.
- Captures / rebuilds: capture = page images + manifest; rebuild = pages written in order.
- Evidence: sha256 of rebuild equals sha256 of original; `integrity_check` = ok; the manifest for two runs of the recipe is identical line for line.
- Astra will try: drop, duplicate or reorder a page in the capture and confirm the verifier names the page; capture a database that is not closed (a `-wal` exists) and confirm the tool refuses rather than producing a stale file.

**2.5 header-on-commit** · EXACT · placed at step 4 and 6 · [probed]
- Claim: a commit changes the change counter (24) and version-valid-for (92) on page 1, and the in-header size (28) is trusted only when those two agree.
- Does: hashes the manifest before and after one more recipe step; lists the changed pages and the changed bytes of page 1.
- Evidence: bytes 24–27 and 92–95 each advance by one in rollback mode; only the pages the step touched have new hashes.
- Astra will try: WAL mode, where the research probe saw the counter stay constant across commits on one connection — the chapter must state and show that difference.

**Feeds the through-line.** R1, and the page reader that R3, R4, R5 and R8 all reuse.
`internal/equal` starts here with E1.

---

### Chapter 3 — B-trees and page management

Directory `ch03-btrees-and-page-management/`, file `ch03-btrees-and-page-management.md`.

**Question.** What happens to the pages when an insert does not fit, and why do two databases
with the same rows end up with different bytes?

**Reading path:**

1. `btreeInt.h` 149–228 — cells and overflow, as a refresher
2. `btree.c` `sqlite3BtreeInsert` (9409–9710) — the spine
3. `fillInCell` (7074–7257) — where overflow chains are created
4. `insertCellFast` → `allocateSpace` → `pageFindSlot` / `defragmentPage` (7428–7500, 1819–1903, 1747–1804, 1613–1731)
5. `balance` (9130–9259) — the dispatcher and its three cases
6. `balance_quick`, `balance_deeper` (8007–8101, 9049–9094)
7. `balance_nonroot` (8245–9027) with `editPage` / `rebuildPage` (7873–7980, 7644–7711)
8. `sqlite3BtreeDelete` → `dropCell` → `freeSpace`; then `freePage2` / `allocateBtreePage` — page reuse
9. `autoVacuumCommit`, `incrVacuumStep`, `relocatePage`
10. `checkTreePage` (10855–11118) — the invariants written as checks

**Copied:** `btree.c`, `btreeInt.h`, `btree.h`.

Version notes for this chapter: the insert path calls `insertCellFast`, not `insertCell`;
`clearCell` is now the `BTREE_CLEAR_CELL` macro and `clearCellOverflow`. Older books show the
old names.

**Sections.**

1. Model — table B-trees (rowid keys, data in leaves) versus index B-trees; a page as a cell-pointer array growing down and cell content growing up.
2. Question — insert until the page is full, then one more.
3. Call path — `sqlite3BtreeInsert` to `balance`. Experiments 3.1 and 3.2 after the balance cases they exercise.
4. Timeline — page layout before and after each kind of split, drawn from real page dumps.
5. Lab — force splits and compare layouts (the course doc's lab). Experiments 3.1–3.4.
6. Evidence and failure cases — Experiment 3.5: layout depends on history.
7. Platform implications — file size and fragmentation depend on write history; `VACUUM` cost; why a physical copy and a logical copy of the same data differ; what `secure_delete` buys.
8. Knowledge check.

**Experiments.**

**3.1 append-split** · EXACT · placed at step 3 · [unprobed]
- Claim: appending past a full rightmost leaf takes the cheap path (`balance_quick`): one new leaf, one divider added to the parent, the old leaf untouched.
- Does: inserts rows with increasing rowids one at a time; after each insert takes a page manifest; stops at the first insert that changes the page count; prints the layout of the pages involved before and after, using the chapter 2 inspector.
- Evidence: the exact rowid at which the split happens (computable from cell size and page size, and the chapter shows the arithmetic); the old leaf's cell content hash is unchanged; the new leaf holds exactly one cell.
- Astra will try: other row sizes and page sizes and check the predicted split row; a debug build (`-tags sqlite_debug`) with a breakpoint-free check that `balance_quick` ran (coverage counter or trace line).

**3.2 middle-split-and-root-growth** · EXACT · placed at step 3 · [unprobed]
- Claim: an insert into the middle of a full leaf redistributes cells across siblings (`balance_nonroot`), and when the root itself overflows the tree grows one level while the root keeps its page number (`balance_deeper`).
- Does: fills a table, then inserts into the middle of the key range; separately grows a table until depth goes from 1 to 2 to 3, recording depth, page count and the root page number.
- Evidence: sibling pages' cell counts before and after; root page number constant across depth changes and equal to `sqlite_schema.rootpage`; `integrity_check` = ok at every step.
- Astra will try: random insert orders under a fixed seed; index B-trees; confirm `dbstat` agrees with the inspector at every step.

**3.3 overflow-threshold** · EXACT · placed at step 5 · [unprobed]
- Claim: a table-leaf payload stays on the page up to a fixed limit (usable size − 35, so 4061 bytes at this page size) and spills to an overflow chain one byte later.
- Does: inserts payloads of sizes around the limit; counts overflow pages for each using the chapter 2 walker.
- Evidence: zero overflow pages at the limit, one at limit + 1; local portion and chain length match the formula in `btreeInt.h` and `btreeParseCellPtr`.
- Astra will try: index entries (different limit), other page sizes, reserved bytes per page.

**3.4 delete-and-reuse** · EXACT · placed at step 5 · [probed for the freelist and stale-content behavior]
- Claim: deleting rows moves emptied pages to the freelist, later inserts reuse them before the file grows, and a freed page keeps its old bytes unless `secure_delete` is on.
- Does: deletes a key range, records `freelist_count` and file size; inserts new rows and records which page numbers they land on; searches freelist leaves for deleted row content with and without `secure_delete`.
- Evidence: file size unchanged across delete and re-insert; new rows occupy former freelist pages; deleted payload bytes found in the file without `secure_delete` and absent with it.
- Astra will try: `auto_vacuum=FULL` and `incremental_vacuum` (the file shrinks instead); confirm stale bytes also survive in a plain file copy — a privacy point for backups.

**3.5 same-rows-different-bytes (rung R2)** · EXACT · placed at step 6 · [probed]
- Claim: physical layout records history, not just content. The same rows written in different orders or batches give different files, an equal logical hash, and identical bytes after `VACUUM INTO`.
- Does: builds the same final content three ways (one transaction; one per statement; row by row in another order); compares them at E1, E2 and E3; runs `VACUUM INTO` on each and compares again; replays an ordered logical dump into a fresh file and canonicalizes that too. Completes `internal/equal` with the page diff (E2) and the Go logical hash (E3).
- Captures / rebuilds: capture = ordered logical content; rebuild = replay into a fresh file; canonical form = `VACUUM INTO`.
- Evidence: three different sha256 values before; one logical hash; one sha256 after canonicalization, shared by the replayed copy; `dbhash` from the pinned tree agrees with our logical hash on equality.
- Astra will try: the known blind spots — two databases differing only in implicit rowids, in `sqlite_sequence`, in `user_version`, or in integer 1 versus real 1.0 — and confirm our hash tells them apart where `dbhash` or `sqldiff` does not.

**Feeds the through-line.** R2. It settles what "equal" can mean, so later chapters claim the
right level for each capture method.

---

### Chapter 4 — Pager and page cache

Directory `ch04-pager-and-page-cache/`, file `ch04-pager-and-page-cache.md`.

**Question.** Between "the B-tree changed a page in memory" and "the change is safe on disk",
what does the pager write, in what order, and how much memory does it hold while doing it?

**Reading path:**

1. https://www.sqlite.org/atomiccommit.html
2. `pager.c` 135–406 — the state machine comment; `PAGER_*` states at 351–357
3. `doc/pager-invariants.txt`
4. `sqlite3PagerSharedLock`, `hasHotJournal` (5300–5507, 5180–5271)
5. `sqlite3PagerBegin` → `pager_write` → `pager_open_journal` / `writeJournalHdr`
6. `sqlite3PagerCommitPhaseOne` (6511–6730) with `syncJournal` and `pager_write_pagelist`
7. `pager_end_transaction`, `pager_playback` (2052–2180, 2864–3052)
8. `pcache.c` `sqlite3PcacheFetch`, `sqlite3PcacheFetchStress`, the dirty list; `pager.c` `pagerStress` (4655–4727)
9. `pcache1.c` structures (117–211), `pcache1FetchStage2`, `pcache1EnforceMaxPage`
10. `getPageMMap`; `os_unix.c` `unixFetch` / `unixMapfile`; `status.c` 203–421

**Copied:** `pager.c`, `pager.h`, `pcache.c`, `pcache.h`, `pcache1.c`, `status.c`,
`doc/pager-invariants.txt`. **Linked:** `os_unix.c` (copied in chapter 6).

**Sections.**

1. Model — the pager as transaction manager and cache; its seven states; rollback-journal mode only (WAL is chapter 5).
2. Question — one multi-page transaction, from first page write to commit.
3. Call path — begin, write, commit phase one and two, with the state at each line. Experiment 4.1.
4. Timeline — journal and database writes and syncs for one commit; the same timeline with a cache spill. Experiment 4.2.
5. Lab — cache and memory with bounded and unbounded connection counts (the course doc's lab). Experiments 4.4 and 4.5.
6. Evidence and failure cases — a crash mid-transaction and the hot journal. Experiment 4.3.
7. Platform implications — per-connection cache is the main memory cost of an open database; `cache_size` is a budget per connection, not per process; spill means uncommitted data can reach the database file; mmap changes who owns the memory.
8. Knowledge check.

**Experiments.**

**4.1 journal-timeline** · EXACT · placed at step 3 · [probed for the sync order]
- Claim: in rollback-journal mode the original page images are written and synced to the journal before any change reaches the database file, and the commit point is the removal of the journal.
- Does: runs one recipe step with the logging VFS and strace merged; while the transaction is open (before commit, from a child process) parses the `-journal` file with `internal/journalfile` (header, page records, checksums).
- Evidence: call order journal write → journal fsync → directory fsync → database write → database fsync → journal unlink; each journal record's page image hash equals the pre-transaction manifest hash for that page; fsync counts 3 at `NORMAL` and 4 at `FULL`, as the research probe measured.
- Astra will try: `journal_mode` `TRUNCATE` and `PERSIST`; `synchronous=OFF` (no syncs — the chapter must predict it); compare against `showjournal` from the pinned tree.

**4.2 spill-before-commit** · EXACT · placed at step 4 · [unprobed]
- Claim: when dirty pages exceed the cache, the pager writes uncommitted pages into the database file before commit; the journal is what keeps that safe.
- Does: runs a transaction touching far more pages than `cache_size` allows; from the VFS log shows database-file writes before the commit statement; reads `SQLITE_DBSTATUS_CACHE_SPILL`.
- Evidence: database writes appear before `COMMIT` in the log; the spill counter is above zero; with a large cache the same transaction shows no early writes and a zero counter.
- Astra will try: `PRAGMA cache_spill=OFF`; confirm a second process reading during the open transaction never sees the spilled data (it is blocked or sees the old state, by lock rules chapter 6 explains).

**4.3 hot-journal-rebuild (rung R3)** · EXACT · placed at step 6 · [unprobed — the journal replay in Go is new; SQLite's own recovery is documented behavior]
- Claim: after a crash mid-transaction the journal alone is enough to rebuild the pre-transaction database, byte for byte; SQLite does this on the next open, and a short Go program can do the same.
- Does: takes a manifest of the database; starts the spilling transaction of 4.2 in a child process that the fault shim exits at a chosen sync, leaving a hot journal and a half-written database file. Then, on two copies of that wreckage: (a) opens one with SQLite, which rolls back; (b) plays the journal back onto the other with `internal/journalfile` (restore each valid record, truncate to the size in the journal header).
- Captures / rebuilds: capture = the journal's original page images; rebuild = playback onto the damaged file.
- Evidence: sha256 of (a), of (b) and of the pre-transaction file are all equal; the damaged file's hash differs from all three.
- Astra will try: every crash point (each write and sync index in turn) and confirm the result is always exactly the pre-transaction state or exactly the post-transaction state, never a third; a journal with a torn final record; deleting the journal before reopening, which must produce a detectably corrupt or silently wrong database — the chapter's warning about never deleting `-journal` files.

**4.4 cache-budget** · BOUND and SHAPE · placed at step 5 · [probed for the counters]
- Claim: page-cache memory is per connection and bounded by `cache_size`; total memory grows linearly with open connections unless connections are bounded.
- Does: opens *N* connections (to *N* files, then to one file) and scans each; reads `SQLITE_DBSTATUS_CACHE_USED`, hit, miss and write counters per connection and `sqlite3_status64(MEMORY_USED)` for the process; repeats with a cap of *K* open connections managed LRU.
- Evidence: per-connection cache bytes ≤ `cache_size` budget plus a stated overhead; process total grows with *N* when unbounded and stays flat past *K* when bounded; cache hits rise and misses stop on a repeated scan.
- Astra will try: tiny and huge `cache_size`; `PRAGMA shrink_memory`; `sqlite3_db_release_memory`; `soft_heap_limit`; check the bound holds for 1,000 connections.

**4.5 mmap-reads** · EXACT counts, SHAPE for memory · placed at step 5 · [unprobed]
- Claim: with `mmap_size` set, reads come from a memory map instead of `pread`, and that memory is the kernel's page cache rather than SQLite's heap.
- Does: scans the same table with `mmap_size=0` and with a map covering the file; counts `pread64` calls under strace and `xRead` versus `xFetch` in the VFS log; reads `CACHE_USED`.
- Evidence: `pread64` count drops to near zero with mmap; `xFetch` calls appear; SQLite heap use for the cache is lower.
- Astra will try: a file larger than `mmap_size`; writes with mmap on (writes still go through `pwrite`); a truncated file under an active map as the documented hazard.

**Feeds the through-line.** R3: the first log the reader replays by hand. It also explains why
a copy of a database taken without its journal is not a valid capture.

---

### Chapter 5 — WAL, transactions, and checkpoints

Directory `ch05-wal-transactions-checkpoints/`, file `ch05-wal-transactions-checkpoints.md`.

**Question.** In WAL mode, where exactly is a committed transaction, what makes a reader see a
stable snapshot, and what must a checkpoint do before the WAL can be reused?

**Reading path:**

1. https://www.sqlite.org/wal.html, then https://www.sqlite.org/walformat.html
2. `wal.c` 16–249 — the format comment: file format, reader algorithm, wal-index
3. `wal.c` 294–401 — lock slots, `WalIndexHdr`, `WalCkptInfo`
4. `wal.c` 856–1053 — checksum, encode and decode frame
5. `wal.c` `walFrames` (4038–4266) with `pager.c` `pagerWalFrames` (3225–3282)
6. `wal.c` `walTryBeginRead` (3020–3272) — read marks; 3699–3746 — `SQLITE_BUSY_SNAPSHOT`
7. `wal.c` `walCheckpoint` (2199–2395), `sqlite3WalCheckpoint` (4301–4432) — modes
8. `wal.c` 2152–2166 and 3875–3915 — restart and the salt change
9. `wal.c` `walIndexRecover` (1390–1614)
10. `main.c` 2484–2525 — the autocheckpoint hook; `pager.c` 3658–3704 — sync flags

**Copied:** `wal.c`, `wal.h`, `pager.c`. **Linked:** `main.c` (copied in chapter 8).

Notes the chapter must carry: the comment at `pager.c` 607–617 describes the sync bits the
wrong way round relative to `wal.h` 25–26 and the code, so the chapter cites the code. WAL2
and `BEGIN CONCURRENT` are not in this release. `SQLITE_CHECKPOINT_NOOP` exists since 3.51.0.

**Sections.**

1. Model — the WAL as a redo log of page images; the wal-index (`-shm`) as its lookup table; commit = a frame with a non-zero size field.
2. Question — five commits, one long reader, one checkpoint.
3. Call path — commit in WAL mode; begin-read; checkpoint. Experiments 5.1 and 5.2 after the frame format.
4. Timeline — frames appended per commit; read marks; backfill; restart. Experiment 5.4.
5. Lab — the WAL frame inspector and WAL growth under a long-lived reader (the course doc's lab). Experiments 5.1, 5.3, 5.4.
6. Evidence and failure cases — Experiments 5.5 and 5.6.
7. Platform implications — one stuck reader makes the WAL grow without bound; who owns checkpoints; `synchronous` and what it does and does not protect; why `-wal` and `-shm` are part of the database while it is open.
8. Knowledge check.

**Experiments.**

**5.1 wal-inspector** · EXACT · placed at step 3 and 5 · [probed]
- Claim: a WAL file is a 32-byte header plus frames of (24-byte header + page image), linked by a running checksum; a transaction is committed exactly when its last frame carries the database size.
- Does: `internal/walfile` parses a WAL left by five recipe steps (checkpoint on close disabled so the file survives); verifies the checksum chain and salts; lists frames with page number, commit size and page hash.
- Evidence: file size = 32 + frames × (24 + 4096); number of commit frames = 5; frame count equals the `log` column of `PRAGMA wal_checkpoint(NOOP)`; listing equals `showwal` from the pinned tree.
- Astra will try: a big-endian-checksum WAL (magic `…83`), other page sizes, a WAL whose tail holds frames from an older generation.

**5.2 wal-determinism** · EXACT · placed at step 3 · [probed]
- Claim: two runs of the same workload write the same frames in the same order; the files differ only in the random salts and the checksums that depend on them, and with SQLite's PRNG seeded they are identical.
- Does: runs the recipe twice in WAL mode; compares the WAL files byte for byte and as (page number, page hash, commit size) sequences; repeats with `SQLITE_TESTCTRL_PRNG_SEED`.
- Evidence: byte comparison fails only inside salt and checksum fields; frame sequences are equal; seeded runs have equal sha256.
- Astra will try: different `synchronous` levels (sequence must not change); different `wal_autocheckpoint` thresholds (sequence splits across generations — must be reported as such).

**5.3 wal-replay (rung R4)** · EXACT · placed at step 5 · [probed]
- Claim: a base database file plus the committed frames of its WAL is the complete state; applying those frames in order, stopping at the last commit frame and truncating to its size, gives the same file SQLite's own checkpoint gives.
- Does: copies base and WAL from a child process at a quiet moment; rebuilds with `internal/walfile` (no SQLite); separately lets SQLite run `wal_checkpoint(TRUNCATE)` on the original; compares. Repeats with the WAL copied mid-transaction (uncommitted frames at the tail) and after a rollback.
- Captures / rebuilds: capture = base file + WAL frames up to the last commit; rebuild = frame replay.
- Evidence: sha256 of our rebuild equals sha256 of SQLite's checkpointed file in all three cases; the replayer reports how many tail frames it ignored and why.
- Astra will try: flip one byte in the last commit's frames (the rebuild must equal SQLite's own recovery of the same damaged WAL, one transaction short); replay a WAL onto a base from an earlier generation (must be detected or shown corrupt by `integrity_check` — the chapter's rule that base and WAL travel together); apply frames past the last commit and show the result contains rolled-back data.

**5.4 reader-pins-the-wal** · EXACT for marks and counts, SHAPE for growth · placed at step 4 and 5 · [probed for the read-mark behavior]
- Claim: a reader holds a snapshot by pinning a frame number; a checkpoint cannot copy frames past the oldest pinned mark, and the WAL cannot restart while any reader is on it, so the WAL grows for as long as that reader stays open.
- Does: opens a reader and leaves its transaction open; a writer commits in a loop with autocheckpoint on; samples WAL size and decodes `mxFrame`, `nBackfill` and the read marks from the `-shm` file; ends the reader and samples again.
- Evidence: WAL size rises monotonically while the reader is open; `nBackfill` stays at the reader's mark; the reader's row count never changes; after the reader ends, one checkpoint brings `nBackfill` to `mxFrame` and the next write restarts the WAL at frame 1.
- Astra will try: several readers at different marks; a reader that only holds a prepared statement mid-`step`; `journal_size_limit` and `TRUNCATE` to show when the file actually shrinks.

**5.5 checkpoint-modes** · EXACT · placed at step 6 · [probed in part]
- Claim: `PASSIVE`, `FULL`, `RESTART`, `TRUNCATE` and `NOOP` differ in what they wait for and what they leave behind, and a restart is visible as new salts.
- Does: runs each mode with and without a blocking reader; records the (busy, log, checkpointed) result, WAL file size, and the header salts before and after.
- Evidence: a result table per mode; `PASSIVE` with a reader returns partial progress and not busy; `TRUNCATE` leaves a zero-length WAL; `NOOP` changes nothing; salt-1 increments on restart.
- Astra will try: a writer racing each mode; `SQLITE_DBCONFIG_NO_CKPT_ON_CLOSE` and `SQLITE_FCNTL_PERSIST_WAL` on last close.

**5.6 commit-sync-levels** · EXACT · placed at step 6 · [probed]
- Claim: in WAL mode `synchronous=NORMAL` does not fsync on commit and `FULL` fsyncs the WAL once per commit; checkpoints fsync the WAL and then the database at both levels.
- Does: counts fsync calls per commit and per checkpoint under strace and the VFS log at `OFF`, `NORMAL`, `FULL`.
- Evidence: steady-state fsyncs per commit: 0, 0, 1. First commit into a new WAL adds a header sync and a directory sync.
- Astra will try: confirm that killing the process right after a `NORMAL` commit loses nothing (process crash) — and note that this says nothing about power loss, which chapter 9 tests properly.

**Feeds the through-line.** R4, the core of the course: the reader's own program turns "base +
log" into the same bytes SQLite produces. Experiment 5.4 supplies the mechanism (a held read
mark) that makes continuous capture safe in R8.

---

### Chapter 6 — Locking and concurrent access

Directory `ch06-locking-and-concurrency/`, file `ch06-locking-and-concurrency.md`.

**Question.** When two processes use one database, who waits, who gets `SQLITE_BUSY`, and when
is it safe to read every page of a database that another process is writing?

**Reading path:**

1. https://www.sqlite.org/lockingv3.html and https://www.sqlite.org/isolation.html
2. `os.h` 98–166 — lock levels and the byte ranges
3. `os_unix.c` 1189–1342 — the comment on POSIX lock semantics, and the inode structures
4. `os_unix.c` `unixLock` (1843–2095), `posixUnlock` (2124–2275)
5. `os_unix.c` 2101–2109, 1471–1482 — deferred close of descriptors
6. `wal.c` 294–299, 1085–1131 — WAL lock slots and wrappers
7. `os_unix.c` 4556–4603, 5284–5476 — the `-shm` lock implementation
8. `pager.c` `sqlite3PagerBegin` (5968–6032); `build.c` 5245–5274 — `BEGIN` modes
9. `btree.c` 3598–3804 — busy retry; `main.c` 1730–1792 — the default busy callback
10. `wal.c` 3699–3746 — `SQLITE_BUSY_SNAPSHOT`

**Copied:** `os_unix.c`, `os.h`, `wal.c`. **Linked:** `pager.c`, `btree.c`, `main.c`, `build.c`.

Facts the chapter states: Linux builds use POSIX advisory locks, not OFD locks; in WAL mode
`BEGIN EXCLUSIVE` behaves like `BEGIN IMMEDIATE`; `sqlite3_setlk_timeout` needs a compile
option the course build can add if the chapter wants blocking locks.

**Sections.**

1. Model — five lock levels on three byte ranges in rollback mode; eight lock slots in the `-shm` file in WAL mode; locks belong to the process, not the connection.
2. Question — two processes, one database, every combination of read and write.
3. Call path — `unixLock` transitions; WAL lock wrappers; where the busy handler is and is not called. Experiment 6.1.
4. Timeline — lock timelines for writer-versus-reader in both journal modes. Experiments 6.2 and 6.3.
5. Lab — multiple processes; record contention, busy results and writer latency (the course doc's lab). Experiments 6.2–6.4.
6. Evidence and failure cases — Experiments 6.5 and 6.6.
7. Platform implications — one writer per database is SQLite's rule, so the platform must decide who that writer is; `busy_timeout` is a queue of unknown length; `BEGIN IMMEDIATE` for writers; never touch the database file with ordinary file calls in the serving process.
8. Knowledge check.

All multi-process experiments use `internal/procs`: the experiment binary re-runs itself in
named child roles, and the parent drives them through numbered steps over pipes. The
interleaving is part of the script, so outcomes are exact rather than timing-dependent.

**Experiments.**

**6.1 lock-bytes** · EXACT · placed at step 3 · [probed for the observation method]
- Claim: SQLite's lock levels are byte-range locks at fixed offsets (1073741824 and up on the database file; bytes 120–127 of the `-shm` file), and each level change is one `fcntl` call.
- Does: one child walks SHARED → RESERVED → EXCLUSIVE in rollback mode and reader/writer/checkpointer in WAL mode, pausing at each step; the parent reads `/proc/locks` and the child's strace and VFS log.
- Evidence: a table of level → byte range → lock type, matching the constants in `os.h` and `wal.c`; one `F_SETLK` per transition in the strace.
- Astra will try: two connections in one process (the locks are shared — no second `fcntl`); the pinned CLI as the second process.

**6.2 busy-matrix** · EXACT · placed at step 4 and 5 · [probed for cross-process interop]
- Claim: which side gets `SQLITE_BUSY`, and at which statement, is determined by journal mode and `BEGIN` mode; it can be tabulated.
- Does: for rollback and WAL modes × `BEGIN DEFERRED/IMMEDIATE/EXCLUSIVE` × (reader first, writer first), runs two children through a scripted interleaving with `busy_timeout=0`; records each statement's extended result code.
- Evidence: the full matrix, stored in `evidence/`; in WAL mode readers never block the writer and the writer never blocks readers; in rollback mode a reader blocks the writer's commit.
- Astra will try: three processes; the CLI as one participant; every cell re-run 100 times to confirm it is not timing-dependent.

**6.3 busy-timeout-and-latency** · EXACT for outcomes, SHAPE for latency · placed at step 5 · [unprobed]
- Claim: a busy timeout turns immediate failures into waits; writer latency under contention is the holder's remaining transaction time, and deferred transactions can still fail at once on upgrade.
- Does: *W* writer processes each run a fixed number of scripted transactions against one database, with timeout 0 and with a generous timeout, using `BEGIN IMMEDIATE` and then `BEGIN DEFERRED`; records busy counts and per-transaction latency.
- Evidence: with a timeout and `IMMEDIATE`, busy count is 0 and every transaction commits; with timeout 0 the busy count is above 0; the final database equals the recipe's expected state (logical hash) in the all-commit case; latency distribution widens with *W*.
- Astra will try: a deadlock-shaped case (two deferred readers both upgrading) and confirm one gets `SQLITE_BUSY` without the handler being called.

**6.4 stale-snapshot** · EXACT · placed at step 5 · [unprobed; the code path is cited]
- Claim: in WAL mode a read transaction that later tries to write fails with `SQLITE_BUSY_SNAPSHOT` (517) if anyone committed after its snapshot, and no timeout helps.
- Does: child A begins and reads; child B commits; A attempts a write with a long busy timeout.
- Evidence: A gets extended code 517 immediately; after A restarts its transaction the same write succeeds.
- Astra will try: `BEGIN IMMEDIATE` for A (no failure); many iterations.

**6.5 torn-copy** · EXACT · placed at step 6 · [probed: 8 of 8 naive copies failed]
- Claim: copying a database file while it is being written does not give a database; copying it without its WAL silently loses commits.
- Does: a writer child runs the recipe continuously; the parent's helper child copies the file with plain reads at scripted points; checks each copy with `integrity_check` and the logical hash against the recipe's known states.
- Evidence: copies taken under the writer fail `integrity_check` or match no recipe state; a copy of the main file alone in WAL mode opens cleanly but equals an older state than the last committed one.
- Astra will try: find a copy timing that happens to pass and show it still is not guaranteed (the chapter's claim is "not safe", not "always corrupt").

**6.6 live-snapshot (rung R5)** · EXACT · placed at step 6 · [probed: page capture in a read transaction is byte-identical on a quiet database; under a writer it is unprobed]
- Claim: all pages read inside a single read transaction form a consistent database equal to the state at that transaction's snapshot, even while a writer keeps committing.
- Does: a writer child commits recipe steps without pause; the capture child begins a read transaction in WAL mode, notes the recipe step it sees (`user_version`), reads every page through `sqlite_dbpage`, writes them out as a chapter 2 capture, and rebuilds a file from it.
- Captures / rebuilds: capture = pages as of one snapshot; rebuild = pages written in order.
- Evidence: the rebuild passes `integrity_check`, and its sha256 equals the file produced by running the recipe to exactly that step and checkpointing (the known state *k*).
- Astra will try: rollback-journal mode (the reader blocks the writer instead — consistent but not concurrent); a capture that spans two read transactions (must be shown inconsistent); a reader slow enough to make the WAL grow, tying back to 5.4.

**Feeds the through-line.** R5: the first capture taken from a live database, and the proof of
why a capture must go through SQLite's locking rather than around it.

---

### Chapter 7 — VFS and filesystem I/O

Directory `ch07-vfs-and-filesystem-io/`, file `ch07-vfs-and-filesystem-io.md`.

**Question.** What exactly does SQLite ask the operating system to do, in what order, and
what is left on disk if the machine stops between two of those requests?

**Reading path:**

1. https://www.sqlite.org/vfs.html
2. `sqlite.h.in` 584–878 — open flags, device characteristics, `sqlite3_file`, `sqlite3_io_methods`
3. `sqlite.h.in` 1343–1550 — `sqlite3_vfs`
4. `os.c` 82–246 — the wrappers the core calls
5. `os_unix.c` `unixOpen` (6538–6826), `robust_open` (834–872)
6. `os_unix.c` read and write (3463–3721); `full_fsync`, `unixSync` (3778–3956); `unixDelete` (6833–6869)
7. `os_unix.c` file control and device characteristics (4141–4484)
8. `ext/misc/vfstrace.c` — whole file, as the shim pattern
9. `test_demovfs.c` — the smallest complete VFS
10. https://www.sqlite.org/psow.html and https://www.sqlite.org/atomiccommit.html section on sync assumptions

**Copied:** `os_unix.c`, `os.c`, `os.h`, `vfstrace.c`, `vfsstat.c`, `test_demovfs.c`.
**Linked:** `sqlite.h.in` (very large; lines 584–1611 cited), `test6.c` (the crash-simulation
model the power-cut shim follows).

**Sections.**

1. Model — the VFS as the only door to the OS: one object per filesystem, one per open file; a shim is a VFS that forwards to another.
2. Question — trace one commit in each journal mode through the VFS to syscalls.
3. Call path — `sqlite3OsWrite` → `unixWrite` → `pwrite`; `sqlite3OsSync` → `unixSync` → `full_fsync`; open flags by file type. Experiment 7.1.
4. Timeline — VFS calls and syscalls side by side for a commit. Experiment 7.2.
5. Lab — the logging VFS; I/O under different transaction patterns (the course doc's lab). Experiments 7.2 and 7.3.
6. Evidence and failure cases — Experiments 7.4 and 7.5.
7. Platform implications — the VFS is where a platform can observe, meter, capture or redirect I/O without changing SQLite; what a network or object-store filesystem would have to honor (sync, locks, shared memory); cost per commit by mode.
8. Knowledge check.

**Experiments.**

**7.1 trace-a-commit** · EXACT · placed at step 3 · [probed]
- Claim: every byte SQLite reads or writes goes through a VFS method, and each method call maps to a specific function in `os_unix.c` and a specific syscall.
- Does: registers the vfstrace shim over the `unix` VFS; opens a database, sets WAL mode, creates a table, commits one insert, checkpoints, closes; captures the VFS log and strace in one ordered stream.
- Evidence: the stored call list (about 130 lines); each `xWrite` paired with a `pwrite64` of the same file, offset and length; `xSync` paired with `fsync` or `fdatasync`; the `-wal` deletion at close.
- Astra will try: compare against the pinned CLI's `-vfstrace` for the same statements; check the documented quirk that vfstrace prints `FULL` for a `NORMAL` sync against the real flag value.

**7.2 io-by-transaction-pattern** · EXACT · placed at step 4 and 5 · [probed for per-commit sync counts]
- Claim: the same 1,000 rows cost very different I/O depending on batching, journal mode and `synchronous`, and the differences are countable.
- Does: writes the rows as one transaction, as 1,000 transactions, and in batches of 50; in `DELETE`, `TRUNCATE` and `WAL` modes; at `NORMAL` and `FULL`; counts writes, bytes written, syncs, and file creates/deletes from the VFS log.
- Evidence: a stored table of exact counts; syncs per transaction match the research measurements (rollback 3 at `NORMAL`, 4 at `FULL`; WAL 0 and 1); the chapter derives each number from the code path.
- Astra will try: recompute every cell from strace alone; add `PERSIST` mode and `synchronous=EXTRA`; check counts are identical across 20 runs.

**7.3 files-and-directories** · EXACT · placed at step 5 · [probed in part; one case left unverified by research]
- Claim: creating or deleting a journal is a directory change that needs its own sync; the WAL and shared-memory files appear at first use and disappear at last close unless told to persist.
- Does: logs opens, deletes and directory syncs across a connection's life in each journal mode; repeats with `SQLITE_FCNTL_PERSIST_WAL`; settles the open research question of whether a long-lived `TRUNCATE` or `PERSIST` connection repeats the directory sync after its first transaction.
- Evidence: directory fsync follows the first journal sync; `-wal` and `-shm` exist only between first WAL use and last close; the answer to the open question, with the trace that shows it.
- Astra will try: two connections closing in either order; a crashed process leaving `-wal` and `-shm` behind and the next open cleaning up.

**7.4 write-log-rebuild (rung R6)** · EXACT · placed at step 6 · [unprobed — the capture shim is designed, not built]
- Claim: the ordered log of VFS writes, truncates and deletes is a complete record of durable state: replaying it into an empty directory reproduces the database file and the WAL exactly.
- Does: builds the capture shim (C pass-through, log kept in Go by `internal/capturevfs`); runs the recipe under it with SQLite's PRNG seeded; replays the log with plain file calls into an empty directory; also replays only a prefix that ends at a sync.
- Captures / rebuilds: capture = (file, offset, bytes, operation, sync marks) in order; rebuild = replay.
- Evidence: sha256 of the replayed database and WAL equal the originals; two runs produce identical logs; a prefix ending at any commit's sync, once opened by SQLite, equals the recipe's state for that commit.
- Astra will try: replay prefixes ending at every sync point and at every single write, and classify each result as "equals state *k*" or "recovers to state *k*" — never anything else.

**7.5 power-cut-shim** · EXACT · placed at step 6 · [unprobed — designed after `test6.c`, not built]
- Claim: a process crash and a power cut are different events. After a process crash every completed write survives; after a power cut only synced writes are guaranteed. A shim that holds writes back until sync can show the difference.
- Does: extends the capture shim with a power-cut mode: writes are buffered per file and released on that file's sync; at a chosen sync or write index the shim drops (or tears, by sector) everything unsynced and exits the process. Demonstrates on one WAL commit at `synchronous=NORMAL` and `FULL`, next to the plain exit-at-sync fault from chunk 0.
- Evidence: plain process exit after the commit returns: the row is present at both levels. Power cut at the same point: the row is absent at `NORMAL`, present at `FULL`; the database passes `integrity_check` in every case.
- Astra will try: tearing writes at sector boundaries; cutting during a checkpoint; `synchronous=OFF` in rollback mode, where the claim is that corruption becomes possible — Astra must either produce a corrupt database or report that it could not.

**Feeds the through-line.** R6: state as the filesystem sees it, which is also the instrument
(prefix replay, power cut) that turns later durability claims into experiments.

---

### Chapter 8 — Connection and database lifecycle

Directory `ch08-connection-and-database-lifecycle/`, file `ch08-connection-and-database-lifecycle.md`.

**Question.** What does it cost to open, hold open, and close a database, and what state is a
database in after its last connection closes?

**Reading path:**

1. https://www.sqlite.org/c3ref/open.html and `/c3ref/close.html`
2. `main.c` `openDatabase` (3360–3736)
3. `btree.c` `sqlite3BtreeOpen` (2532–2825)
4. `prepare.c` `sqlite3Init`, `sqlite3ReadSchema`, `schemaIsValid` (438–533); `sqlite3InitOne` (199–427)
5. `vdbe.c` `OP_Transaction` (4067–4201) — the schema cookie check; `vdbeapi.c` 919–966 — the retry
6. `attach.c` 74–349 — attach and detach
7. `main.c` 1266–1486 — close, and the zombie state
8. `main.c` 772–945 — lookaside, release memory, cache flush; `status.c` 203–421
9. `os_unix.c` 246–250, 2101–2109, 6369–6414 — descriptor parking and reuse
10. `wal.c` `sqlite3WalClose` — checkpoint and WAL deletion at last close

**Copied:** `main.c`, `attach.c`, `prepare.c`, `status.c`. **Linked:** `btree.c`, `vdbe.c`,
`os_unix.c`, `wal.c`.

**Sections.**

1. Model — a connection owns a schema cache, a page cache per attached file, prepared statements and descriptors; none of it is shared across connections.
2. Question — open 1,000 database files and query each once.
3. Call path — open, first statement (schema load), close. Experiment 8.1.
4. Timeline — descriptors and memory over a connection's life. Experiments 8.2 and 8.3.
5. Lab — open and query hundreds or thousands of files under a bounded connection manager (the course doc's lab). Experiment 8.6.
6. Evidence and failure cases — Experiments 8.4 and 8.5.
7. Platform implications — the numbers a capacity plan needs: descriptors and bytes per idle database, cost of a cold open, what eviction must do before it may copy or move a file.
8. Knowledge check.

**Experiments.**

**8.1 open-is-lazy** · EXACT · placed at step 3 · [probed]
- Claim: `sqlite3_open_v2` reads only the 100-byte header; the schema is read and parsed at the first statement, and its cost grows with schema size.
- Does: logs VFS reads during open and during the first statement, for schemas of 1, 100 and 1,000 tables; reads `SQLITE_DBSTATUS_SCHEMA_USED` after each.
- Evidence: one 100-byte read at open; page-1 and schema-page reads at first statement; schema memory rises with table count (exact byte values stored for the course build).
- Astra will try: a missing file with and without the create flag; a file that is not a database (error arrives at first statement, not at open).

**8.2 descriptors-per-connection** · EXACT · placed at step 4 · [probed]
- Claim: an idle connection holds 1 descriptor in rollback mode (2 inside a write transaction) and 3 in WAL mode; closing one of two connections to the same file in one process may keep its descriptor parked until the other releases its locks.
- Does: counts `/proc/self/fd` entries per state for one and for two same-file connections.
- Evidence: the counts 1 / 2 / 3; the parked descriptor visible after the first close and gone after the second connection ends its transaction.
- Astra will try: 1,000 WAL databases against the process descriptor limit; temp files and statement journals as extra transient descriptors.

**8.3 memory-per-connection** · BOUND · placed at step 4 · [probed for the counters]
- Claim: an idle connection's memory is schema + page cache + lookaside + statements, each separately measurable, and most of it can be released without closing.
- Does: for *N* idle connections reads `CACHE_USED`, `SCHEMA_USED`, `STMT_USED`, lookaside counters and process `MEMORY_USED`; applies `PRAGMA shrink_memory` and `sqlite3_db_release_memory`.
- Evidence: components sum to within a stated margin of the process total; cache component drops to near zero after release while schema and statements stay.
- Astra will try: large schemas, many cached statements, `cache_size` extremes.

**8.4 statements-and-schema-changes** · EXACT · placed at step 6 · [unprobed]
- Claim: a prepared statement survives a schema change by being recompiled transparently on its next step; the recompile is counted.
- Does: prepares a statement on connection A; connection B adds a column; A steps again; reads `SQLITE_STMTSTATUS_REPREPARE` and the schema cookie.
- Evidence: reprepare counter goes 0 → 1; cookie advances by one; results are correct.
- Astra will try: dropping the table the statement uses (error surfaces); measuring prepare versus step cost to justify statement caching.

**8.5 attach-limits-and-atomicity** · EXACT · placed at step 6 · [unprobed]
- Claim: `ATTACH` is bounded (10 by default) and gives a cross-file transaction that is atomic in rollback mode through a super-journal but only per-file atomic in WAL mode.
- Does: attaches up to the limit and one more; commits one transaction across two files and has the fault shim exit the process between the two files' commits, in rollback mode and in WAL mode; reopens both.
- Evidence: the eleventh attach fails; after the crash, rollback mode shows both files changed or neither; WAL mode shows one changed and one not.
- Astra will try: every crash index across the two-file commit; `SQLITE_LIMIT_ATTACHED` lowered at runtime.

**8.6 bounded-manager (rung R7)** · BOUND, with EXACT hashes · placed at step 5 · [probed for the close behavior; the manager is new]
- Claim: a manager that keeps at most *K* connections open serves any number of database files with bounded descriptors and memory; and a WAL database whose last connection has closed is a single, fully checkpointed file that a plain copy captures exactly.
- Does: creates 1,000 small recipe databases; serves a seeded stream of queries and writes through an LRU manager capped at *K*; samples descriptors and memory; on each eviction closes the connection, checks no `-wal` or `-shm` remains, records the file's sha256; on each reopen checks the hash first. Runs the same stream unbounded for contrast.
- Captures / rebuilds: capture = the file at rest after last close; rebuild = copy.
- Evidence: descriptors ≤ 3*K* + a stated constant and memory ≤ *K* × per-connection budget + constant throughout; every reopen hash matches; every database's final content equals the recipe's expected state; cold-open versus warm latency reported as SHAPE.
- Astra will try: kill the manager mid-stream and reopen everything; a reader left open so close does not checkpoint (the `-wal` remains — the manager must detect this and not treat the file as at rest); *K* = 1 and *K* above the descriptor limit.

**Feeds the through-line.** R7: the cheapest correct capture there is, and the precise
condition under which it is correct.

---

### Chapter 9 — Backup, crash recovery, and integrity

Directory `ch09-backup-crash-recovery-integrity/`, file `ch09-backup-crash-recovery-integrity.md`.

**Question.** Which ways of saving a live database give a copy that can be trusted, what
survives a process crash versus a power cut, and how is a restored database proven to be
the right one?

**Reading path:**

1. https://www.sqlite.org/backup.html, `/howtocorrupt.html`, `/atomiccommit.html`
2. `pager.c` `hasHotJournal` (5180–5271); `sqlite3PagerSharedLock` (5300–5507) → `pager_playback` (2864–3052)
3. `pager.c` `pager_delsuper` (2579–2695) — multi-file commits
4. `wal.c` `walIndexRecover` (1390–1614)
5. `backup.c` `sqlite3_backup_step` (323–591); update and restart (688–734)
6. `vacuum.c` `sqlite3RunVacuum` (143–427) — the `INTO` branch
7. `memdb.c` 750–910 — serialize and deserialize
8. `btree.c` 11144–11295 and `vdbe.c` 7263–7299 — integrity check
9. `tool/dbhash.c`, `tool/sqlite3_rsync.c` — how the in-tree tools compare and replicate
10. `wal.c` 2199–2395 again, for how a reader's mark limits a checkpoint

**Copied:** `backup.c`, `vacuum.c`, `memdb.c`, `dbhash.c`, `showwal.c`, `showjournal.c`.
**Linked:** `pager.c`, `wal.c` (copied in chapters 4 and 5), `sqlite3_rsync.c`, `sqldiff.c`.

**Sections.**

1. Model — three families of capture (physical pages, logical content, logs) and two families of failure (process crash, power loss).
2. Question — save a database that is being written, crash it, restore it, prove it.
3. Call path — backup step and restart; `VACUUM INTO`; recovery on open for both journal modes. Experiments 9.1 and 9.2.
4. Timeline — a backup racing a writer; a crash at each sync of a commit.
5. Lab — process-crash recovery and a comparison of backup approaches (the course doc's lab). Experiments 9.1–9.3.
6. Evidence and failure cases — Experiments 9.4, 9.5, 9.6.
7. Platform implications — choose a capture method by the equality level and the concurrency it gives; state the durability model in terms of sync, not of "committed"; integrity checks do not detect every corruption.
8. Knowledge check.

**Experiments.**

**9.1 capture-comparison** · EXACT · placed at step 3 and 5 · [probed on a quiet database; in part under a writer]
- Claim: each capture method reaches a specific equality level, and only some are safe while a writer is active.
- Does: runs seven captures of the same database — closed-file copy, backup API, `VACUUM INTO`, `sqlite3_serialize`, page read in a read transaction, ordered logical dump, session changeset applied to a base — first on a quiet database, then with a writer child committing recipe steps.
- Captures / rebuilds: each method's own.
- Evidence: one table — method × (E1, E2, E3, safe under writer, blocks writer); matches section 4.2; every capture taken under the writer equals some exact recipe state *k*.
- Astra will try: catch any method returning a state that is not a recipe state; the research observation that `.backup` sometimes failed with "database is locked" under writers — reproduce it, find the cause, and have the chapter state it.

**9.2 backup-restarts** · EXACT · placed at step 3 · [unprobed; the code path is cited]
- Claim: a stepped backup restarts from page one when another connection writes to the source, and does not when the write comes through the same connection that is the backup source.
- Does: steps a backup a few pages at a time; interleaves scripted writes from the source connection and then from another connection; records `sqlite3_backup_remaining` after each step.
- Evidence: `remaining` falls steadily with same-connection writes and jumps back up with other-connection writes; with a writer faster than the step size, the backup never finishes within the scripted bound; the finished backups equal known recipe states at E2.
- Astra will try: step sizes from 1 to −1 (all at once); rollback versus WAL source.

**9.3 process-crash-recovery** · EXACT · placed at step 5 · [probed: kill −9 never lost an acknowledged commit]
- Claim: after a process crash at any point, the reopened database equals exactly one recipe state, and that state is at or after the last commit the writer acknowledged — at every `synchronous` level.
- Does: a writer child runs recipe steps, reporting each acknowledged step to the parent; the fault shim exits it at write or sync index *i*; the parent reopens, runs `integrity_check`, and matches the logical hash against the recipe's state list. Sweeps *i* over a full commit, in rollback and WAL modes, at `OFF`, `NORMAL`, `FULL`.
- Evidence: for every *i*: integrity ok; recovered state *k* ≥ last acknowledged step; never a state outside the list. The chapter says plainly that this proves atomicity under process failure and nothing about power loss.
- Astra will try: real `kill -9` at random times (thousands of iterations) in addition to the indexed sweep; crashing during recovery itself; crashing during a checkpoint.

**9.4 power-loss-is-different** · EXACT · placed at step 6 · [unprobed — depends on the 7.5 shim]
- Claim: under simulated power loss, `synchronous=FULL` keeps every acknowledged commit; WAL with `NORMAL` may lose the most recent acknowledged commits but stays consistent; the result is always a recipe state, earlier than or equal to the last acknowledged one.
- Does: the 9.3 sweep, with the power-cut shim dropping unsynced writes at index *i*.
- Evidence: per mode and level, the gap between last acknowledged step and recovered step: 0 for `FULL`; ≥ 0 and sometimes > 0 for WAL `NORMAL`; integrity ok throughout. Includes the short mattn aside: its default connection settings put an application in the `NORMAL` row without saying so.
- Astra will try: torn sector writes; power cut during checkpoint and during hot-journal rollback; rollback mode with `synchronous=OFF`, hunting for an actual corruption to show.

**9.5 what-integrity-check-misses** · EXACT · placed at step 6 · [unprobed]
- Claim: `PRAGMA integrity_check` verifies structure, not content: it catches a damaged page header or index mismatch but not a changed byte inside a row's payload; a page-hash manifest catches both.
- Does: flips one byte in turn in a B-tree page header, a cell pointer, an unindexed column's payload, an indexed column's payload, and a freelist leaf; runs `integrity_check`, `quick_check`, and the chapter 2 manifest verifier on each.
- Evidence: a table of corruption site × detected by which check; the manifest detects all five; `integrity_check` misses the unindexed payload and the freelist leaf.
- Astra will try: a seeded sweep over random byte offsets, reporting the fraction each check detects; the in-tree checksum VFS (`cksumvfs.c`) as an alternative detector.

**9.6 point-in-time-rebuild (rung R8)** · EXACT · placed at step 6 · [unprobed as a whole; every mechanism it relies on is probed]
- Claim: one snapshot plus the WAL frames written after it, captured continuously and across checkpoints, can rebuild the database as of any commit; the rebuild is byte-identical to the recipe's state for that commit.
- Does: introduces `internal/walship`. A capture process takes a base snapshot, disables automatic checkpoints, holds a read mark so the WAL cannot restart underneath it, copies newly committed frames into numbered segments (each recording first and last commit, salts, and the page-set hash before and after), and performs checkpoints itself only after shipping. A writer child runs recipe steps throughout. Restore picks a target step, applies segments in order with the chapter 5 replayer, and verifies.
- Captures / rebuilds: capture = snapshot + ordered segments + per-segment hashes; rebuild = snapshot, then segments up to the target.
- Evidence: for every step *k* in the run, restore-to-*k* has sha256 equal to the recipe's checkpointed state *k*; a missing or reordered segment is detected by the hash chain before any page is applied.
- Astra will try: kill the capture process at every point and resume it; let an outside connection checkpoint behind its back (must be detected through the salt change and force a new snapshot); corrupt a segment; restore to a step inside a multi-statement transaction (must be impossible — only commit points exist).

**Feeds the through-line.** R8: the complete answer to "save the state so the database can be
rebuilt", for a live database, to any commit, with the proof built in.

---

### Chapter 10 — Multi-database platform capstone

Directory `ch10-multi-database-platform-capstone/`, file `ch10-multi-database-platform-capstone.md`.

**Question.** Can a small Go service own thousands of SQLite files — bounded in memory and
descriptors, one writer per database, every database rebuildable from saved state — and keep
its invariants when processes die and transfers are interrupted?

**Reading path** (this chapter reads the course's own earlier chapters as much as SQLite):

1. The course doc's "Platform design questions" and "Capstone invariants"
2. Chapter 8 sections 4 and 7 — per-database cost; chapter 5 section 7 — checkpoint ownership
3. `wal.c` `walCheckpoint` (2199–2395), `walRestartHdr` (2152–2166) — what the shipper relies on
4. `main.c` 2484–2525 — the WAL hook the service uses as its "commit happened" signal
5. `os_unix.c` 1189–1342 — why locks cannot serve as cross-machine ownership
6. `tool/sqlite3_rsync.c` — SQLite's own replication protocol, as a comparison design
7. https://www.sqlite.org/useovernet.html and `/howtocorrupt.html` section 2
8. Prior art, read for design only: Litestream "How it works", the LTX file format, LiteFS architecture notes (sources in the research report)

**Copied:** `wal.c`, `main.c`, `sqlite3_rsync.c`. **Linked:** `os_unix.c`.

**The service** (`ch10-…/service/`, about 1,500–2,000 lines of Go, built on the earlier
`internal/` packages):

- **Router and connection manager** — the 8.6 manager: at most *K* open databases, LRU eviction, at-rest hash on close.
- **Ownership** — a control-plane catalog (itself one SQLite database) mapping database id → owner node, epoch, location, last shipped commit. A node writes a database only while it holds the current epoch; the store rejects segments from older epochs.
- **Capture** — `internal/walship` per open database: snapshot, segments, shipper-owned checkpoints.
- **Store** — a local directory standing in for object storage, behind a small interface (put-if-absent, get, list) with injectable faults: failed put, partial put, delayed visibility, crash before or after put.
- **Relocation** — fence the source (bump epoch), ship the final segment, rebuild at the destination, verify the hash, activate, retire the source.
- **Durability model, stated in the chapter:** a write is acknowledged either after local commit at `synchronous=FULL` ("local-durable") or after its segment is in the store ("store-durable"); each invariant test names the mode it assumes.

What the chapter keeps separate, as the course doc requires: SQLite's local guarantees
(atomic commit, snapshot isolation, single writer per file on one machine), the coordination
the service adds (epochs, fencing), and what the store must provide (atomic put, read after
write). The stand-in store models these; the chapter does not claim real object-storage
durability.

**Sections.**

1. Model — the three layers of guarantee and the component diagram.
2. Question — the six capstone invariants, each restated as a checkable property.
3. Call path — the SQLite mechanisms each component depends on, by reference to earlier chapters.
4. Timeline — a relocation, step by step, with the state of both nodes and the store at each step.
5. Lab — build and run the service. Experiments 10.1–10.3.
6. Evidence and failure cases — fault injection. Experiments 10.4–10.6.
7. Platform implications — answers to the course doc's eight design questions, each tied to the experiment that supports it; a list of what this design does not handle.
8. Knowledge check.

**Experiments.**

**10.1 bounded-fleet** · BOUND · placed at step 5 · [unprobed]
- Claim (invariant: descriptors and memory remain bounded): the service serves 2,000 databases through *K* connections within fixed descriptor and memory limits, capture included.
- Does: a seeded request stream over 2,000 databases; samples descriptors and memory every *n* requests.
- Evidence: both stay under bounds computed from *K* and stated in the chapter; every request is answered correctly (checked against the recipe).
- Astra will try: skewed and uniform streams; *K* at its minimum; a slow store.

**10.2 one-writer** · EXACT · placed at step 5 · [unprobed]
- Claim (invariant: at most one authorized writer per database): with epochs checked by the store, a node that lost ownership cannot get another commit accepted, whatever the interleaving.
- Does: a deterministic state-machine test: two node models, the catalog and the store are stepped by a seeded scheduler through every interleaving of claim, write, ship, fence and crash up to a bounded depth; then the same scenarios on two real service processes.
- Evidence: zero interleavings in which two epochs' segments are both accepted for the same commit range; the count of interleavings explored; the real-process runs agree with the model.
- Astra will try: remove the store's epoch check and confirm the test finds the violation (the test must be able to fail); clock-free stale-owner scenarios; a node that crashes and comes back believing it is still the owner.

**10.3 snapshots-are-consistent** · EXACT · placed at step 5 · [unprobed]
- Claim (invariant: every published snapshot is internally consistent): every snapshot and every snapshot + segment prefix in the store rebuilds to a database that passes `integrity_check` and equals a recipe state.
- Does: while the service runs a write stream, a checker process continuously lists the store, rebuilds every published restore point into a scratch directory, and verifies it.
- Evidence: zero failures over the run; number of restore points checked.
- Astra will try: publish during checkpoints and evictions; inject partial puts and confirm they are never listed as restore points.

**10.4 acknowledged-writes-survive** · EXACT · placed at step 6 · [unprobed]
- Claim (invariants: no acknowledged durable write is lost under the stated model; a database can be reopened after process failure): after any process kill, every database reopens, and each holds at least every write acknowledged under its mode.
- Does: clients log acknowledgements; the service process is killed at fault-shim indices and at random times; on restart each database's state is matched to the recipe and compared with the acknowledgement log. In store-durable mode the local files are deleted before restart, so recovery is from the store alone.
- Evidence: recovered step ≥ last acknowledged step for every database, in both modes; integrity ok. With the power-cut shim, the same holds for local-durable mode only at `synchronous=FULL` — shown, not assumed.
- Astra will try: kills during eviction, during snapshot, during catalog updates; acknowledging before the segment put completes (must be caught as a violation of store-durable mode).

**10.5 interrupted-relocation** · EXACT · placed at step 6 · [unprobed]
- Claim (invariant: an interrupted migration cannot silently create two authoritative copies): with a crash or store fault injected at every step of a relocation, the catalog names exactly one owner at all times, any write accepted during the move ends up in exactly one lineage, and the destination's rebuilt file equals the source at the fence point.
- Does: runs the relocation with a fault at each numbered step in turn (and each pair of steps), with a client writing throughout; after each run resumes or rolls back the relocation and checks ownership, the acknowledgement log, and file hashes.
- Evidence: for every fault point: one owner; destination sha256 = source sha256 at the fenced commit; no acknowledged write missing; no write present that was refused.
- Astra will try: skip the fence step and show two writers appear (the test must be able to fail); an interrupted upload left in the store; relocating a database with an open long reader.

**10.6 rebuild-the-fleet (rung R9)** · EXACT · placed at step 6 · [unprobed]
- Claim: the store plus the catalog is a complete saved state of the whole fleet: a new node with empty disks can rebuild every database, and each is byte-identical to the checkpointed original.
- Does: runs a write stream over all databases, quiesces, records each database's sha256 after a `TRUNCATE` checkpoint, deletes every local file, starts a fresh node, rebuilds all databases from the store, compares.
- Captures / rebuilds: capture = everything in the store; rebuild = every database, from nothing.
- Evidence: 2,000 of 2,000 hashes equal; rebuild time and bytes read reported as SHAPE.
- Astra will try: delete or corrupt one segment of one database and confirm exactly that database is reported as unrecoverable past the gap, with the last good restore point named; rebuild while the old node is still running and confirm fencing stops the old node.

**Feeds the through-line.** R9. The ladder ends with the whole platform rebuilt from saved
state, with the invariants tested under faults rather than asserted.

---

## 6. Phase 2 build order and chunking

Nothing here starts until Nick signs off on this outline.

**Roles, per Nick's spec 2 and the standing protocol.**

| Step | Who | Output |
| --- | --- | --- |
| Build | Fresh Opus agent, own worktree, brief as a file | Experiment code, shared package code, captured evidence, `agent_work/chNN/` working scripts |
| Adversarial test | Fresh Astra agent | Scripts in `agent_work/chNN/` that try to falsify each experiment's claim (the "Astra will try" lines above are its starting list), plus a findings report |
| Repair | Fresh Opus agent, one round | Fixes for Astra's findings, or a correction to the claim |
| Chapter text | Fresh Opus agent (see open question 2) | The chapter `.md` with inlined code, run commands, evidence, source spans; copied SQLite files; `SOURCES.sha256` |
| Review gate | Fable (orchestrator dispatches a fresh Fable reviewer) | PASS / NITS / FAIL against the checklist below |
| Review of Astra's test scripts, when needed | Opus | Per protocol |
| Standoff or second FAIL | Muse Wonder Full | Final verdict, per protocol |

**Review checklist for every chapter** (the gate before merge to main):

1. Pin block present; `SOURCES.sha256` verifies against the pinned tree.
2. Every cited `file:start-end` is re-checked by script: the named function is at those lines.
3. Every experiment runs from a clean checkout with the printed command; EXACT evidence matches on two consecutive runs; BOUND and SHAPE evidence holds on three.
4. Inlined code equals the files on disk.
5. Every **[unprobed]** claim is now either confirmed with evidence or corrected in the text; anything still inferred is labelled as inference.
6. Astra's findings are each fixed or answered; its scripts are in `agent_work/` and indexed.
7. All eight sections present; five knowledge-check questions; platform implications tied to evidence.
8. `STATE.md` and `agent_work/INDEX.md` updated in the same commits.

**Chunks.** Sizes are rough: lines of Go (shared packages included) and separate agent runs.

| Chunk | Contents | Depends on | Est. Go lines | Runs (build / test / repair / text / review) |
| --- | --- | --- | --- | --- |
| 0 Foundation | `sqlite/` pin and amalgamation script; `internal/sqlite` binding with vfstrace and fault shims; chapter template; repo checks (source copies, inlined code, span checker); script that builds the in-tree tools | — | 900 + 150 C + 300 shell | 1 / 1 / 1 / — / 1 |
| 1 | Chapter 1, `internal/recipe` | 0 | 700 | 1 / 1 / 1 / 1 / 1 |
| 2 | Chapter 2, `internal/dbfile`, `internal/equal` (E1) | 1 | 1,200 | 1 / 1 / 1 / 1 / 1 |
| 3 | Chapter 3, `internal/equal` (E2, E3) | 2 | 900 | 1 / 1 / 1 / 1 / 1 |
| 4 | Chapter 4, `internal/journalfile` | 2 | 1,000 | 1 / 1 / 1 / 1 / 1 |
| 5 | Chapter 5, `internal/walfile` | 2 | 1,100 | 1 / 1 / 1 / 1 / 1 |
| 7a | Chapter 7 experiments 7.1–7.3 | 1 | 400 | 1 / 1 / 1 / — / — |
| 7b | Capture and power-cut shims, 7.4–7.5, chapter 7 text | 7a | 500 + 350 C | 1 / 1 / 1 / 1 / 1 |
| 6 | Chapter 6, `internal/procs` | 5 | 1,100 | 1 / 1 / 1 / 1 / 1 |
| 8 | Chapter 8 | 4, 5 | 900 | 1 / 1 / 1 / 1 / 1 |
| 9a | Chapter 9 experiments 9.1–9.3, 9.5 | 5, 6, 7b | 800 | 1 / 1 / 1 / — / — |
| 9b | `internal/walship`, 9.4, 9.6, chapter 9 text | 9a | 700 | 1 / 1 / 1 / 1 / 1 |
| 10a | Service core: manager, catalog, store with faults, 10.1 | 8, 9b | 1,200 | 1 / 1 / 1 / — / — |
| 10b | Capture integration, relocation, state-machine model, 10.2–10.3 | 10a | 1,000 | 1 / 1 / 1 / — / — |
| 10c | Fault experiments 10.4–10.6, chapter 10 text | 10b | 800 | 1 / 1 / 1 / 1 / 1 |

Totals: about 13,000 lines of Go, 500 of C, and roughly 65 agent runs if every repair round is
used (15 builds, 15 Astra passes, up to 15 repairs, 10 chapter texts, 11 review gates).

**Waves** (chunks inside a wave touch disjoint directories and packages and run in parallel):

1. Chunk 0
2. Chunk 1
3. Chunk 2
4. Chunks 3, 4, 5, 7a
5. Chunks 6, 7b, 8
6. Chunk 9a, then 9b
7. Chunks 10a, 10b, 10c in sequence

**Run rules.** One fresh agent per run, headless, brief as a file, own worktree, exit file and
log under `/root/runs/`. Chains continue without a human turn (build → Astra → repair → text →
review); a failed run is diagnosed and re-dispatched once, and a second failure stops that
chain and is reported. `STATE.md` is updated at every step. Usage gauges for all three pools
are checked before each wave.

**Risks carried into Phase 2**, in order of how much they could change the plan:

1. The power-cut shim (7.5) is designed but not built. Experiments 9.4 and part of 10.4 depend on it. If it proves harder than the estimate, those experiments fall back to the write-log prefix replay of 7.4, which models the same loss without a live shim.
2. The journal replayer (4.3) and the live-snapshot-under-writer check (6.6) are unprobed. Both follow documented formats and behavior; each chunk confirms its claim before writing text.
3. Exact evidence (opcode listings, byte counts) is tied to the course build's compile options. Chunk 0 freezes that list in `sqlite/PIN`; changing it later invalidates stored evidence.
4. Chapter 10 is the largest and least constrained piece. It is split in three and its model test (10.2) is written before the fault experiments so the invariants exist as code first.

---

## 7. Open questions and decisions for Nick

Each has a recommendation; the outline above assumes the recommendation.

1. **GitHub remote, or local only?** The repo is local with no remote. Recommendation: stay local until chunk 0 and chapter 1 have passed review, then decide. Nothing is pushed without your word.
2. **Who writes the chapter prose?** Your spec assigns Go code to Opus and adversarial testing to Astra; it does not name the prose author. Recommendation: a fresh Opus agent writes each chapter after the Astra pass, and Fable reviews it (the standing Opus-reviewed-by-Fable pairing). Alternative: Fable writes prose and another seat reviews.
3. **Scale: 55 experiments.** The course doc says one short experiment per chapter; your build spec asks for experiments at each point in the text. The outline has five or six per chapter. Recommendation: keep all 55, with one per chapter marked as the ladder rung. If you want it smaller, the cuts with least loss are 1.5, 3.3, 4.5, 6.3, 7.3, 8.4, 8.5 and 9.2.
4. **cgo is required.** Readers need gcc, and the first build compiles SQLite for about 85 seconds. The pure-Go drivers cannot give the exact pin with the real `os_unix.c` and VFS shims. Recommendation: accept cgo.
5. **Commit the generated amalgamation (`sqlite3.c`, 9.5 MB) to the repo?** Recommendation: yes. It is public domain and byte-identical to the official release file, and a clone then builds with no network and no SQLite build tools. Alternative: commit only the script and the hash.
6. **Power loss: simulated in a VFS shim only, or also at the block-device level?** Recommendation: shim only for the course. The kernel route (`dm-log-writes`) would mean loading a kernel module on this VM, installing a replay tool, and asking readers for root. I will not load kernel modules without your say-so. Should it be an optional appendix?
7. **Duplicate source copies.** Read literally, spec 5 puts `btree.c` in two chapter directories, `pager.c` in two, `wal.c` in three, `os_unix.c` in two (about 2.5 MB of duplicates). Recommendation: real copies as specified, checked against the pin by script. Alternative: one copy plus symlinks.
8. **Shared packages versus self-contained chapters.** Later experiments reuse earlier code (the page reader, the WAL replayer). Recommendation: shared `internal/` packages, each inlined in full once in the chapter that introduces it. Alternative: every chapter carries its own copy, which is simpler to read alone but duplicates code that then drifts.
9. **Capstone scope.** Two service processes on this one machine, and a local directory with injected faults standing in for object storage; no real S3, no real network. The chapter says so and does not claim object-storage durability. Is that the right boundary, or do you want a real S3-compatible store (for example MinIO) in the loop?
10. **Linux only.** Experiments use `/proc`, strace, POSIX lock behavior and ext4 sync behavior. Recommendation: state Linux as a requirement; macOS differences go in version notes, untested.
11. **Second SQLite version.** Whether another SQLite version would produce the same bytes from the same recipe is unverified. Recommendation: one optional experiment in chapter 3 that builds the recipe with 3.54.1 and reports the difference, labelled as a version note. Include it?
12. **Knowledge checks and the tutor.** The course doc's tutor prompt ends each chapter with five questions and waits for the reader's observations. Recommendation: each chapter ends with five questions, and answers sit in a separate file in the chapter directory. Should this course also be packaged later for the `tutor` CLI that already hosts a "SQLite Systems" course on this machine? Phase 2 as outlined produces plain markdown only.
13. **Where Astra's scripts are shown to readers.** They live in `agent_work/chNN/` per spec 3. Recommendation: each chapter's "failure cases" section names the scripts that attacked it and what they found, with links, without inlining them.

**Operational note, not a design question.** Every headless Claude run today printed: "this
sign-in ends between 2026-11-25 and 2026-11-28". Phase 2 will run past that window. Someone
needs to run `claude auth login` (or `claude setup-token`) on this VM before then. Usage gauges
could not be read from a headless session in Phase 1; they need a look in the TUI before the
first Phase 2 wave.
