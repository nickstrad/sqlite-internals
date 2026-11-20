# OUTLINE — SQLite Internals, experiment-driven build

Status: **revision 2, signed off by Nick on 2026-11-20 and in build (Phase 2).** Nick's sign-off
came with rulings that amend this outline; they are listed in
[section 8](#8-phase-2-rulings-nick-2026-11-20) and **win wherever the text above them
disagrees**. What changed from revision 1 (commit 7924ef7), and why, is in
[OUTLINE-CHANGES.md](OUTLINE-CHANGES.md).

**North Star (Nick, 2026-11-20).** The course explains how to understand SQLite's architecture
deterministically, well enough to build platforms that hold thousands of SQLite database files
which can be remade as needed. Capturing a database's state and rebuilding it deterministically
is the through-line. Every experiment below is kept because a platform builder needs it; where
a keep is borderline, the chapter says why in a line marked **Why kept**. The course asks for
the least SQLite internals knowledge that runs such a platform well, not everything there is to
know.

Course spec: [docs/course-source.md](docs/course-source.md). Research behind this outline (all
run against the pinned source on this machine, 2026-11-20):

- [docs/knowledge/source-inventory-ch01-04.md](docs/knowledge/source-inventory-ch01-04.md) — function spans and call paths, chapters 1–4
- [docs/knowledge/source-inventory-ch05-09.md](docs/knowledge/source-inventory-ch05-09.md) — the same for chapters 5–9, plus fsync and file-descriptor probes
- [docs/knowledge/determinism-and-rebuild.md](docs/knowledge/determinism-and-rebuild.md) — what makes a database file reproducible, and every capture method tested
- [docs/knowledge/go-access-and-vfs.md](docs/knowledge/go-access-and-vfs.md) — how Go reaches SQLite, driver by driver
- [docs/knowledge/library-route-probes.md](docs/knowledge/library-route-probes.md) — the two probes behind this revision's Go-access decision

Every probe script is in `agent_work/probes/` and indexed in
[agent_work/INDEX.md](agent_work/INDEX.md).

Two labels are used on experiments below:

- **[probed]** — a Phase 1 probe already demonstrated the claim on this machine.
- **[unprobed]** — the claim comes from the source or the official docs and has not been run
  yet. The Opus builder confirms it first and reports back if it is false; the chapter is then
  corrected rather than the evidence bent.

**Reference books.** Two books were read for this revision: "Production SQLite" (Leanpub,
2026-07-28, 221 pp) and "SQLite Internals: How The World's Most Used Database Works" (Leanpub,
2023-01-02, 55 pp). Neither is cited as an authority in any chapter. Both restate the official
documentation, the second is unfinished, and the first contains errors that this course's
experiments contradict (it says WAL mode with `synchronous=NORMAL` syncs at every commit; the
Phase 1 probe counted zero syncs per commit). What they contributed is listed in
OUTLINE-CHANGES.md.

---

## 1. Pinned SQLite revision

**Pin: SQLite 3.53.4**, source tag `version-3.53.4`.

| Identifier | Value |
| --- | --- |
| Release | 3.53.4, dated 2026-07-24 19:02:57 UTC |
| Git commit (github.com/sqlite/sqlite) | `b09c88c14082339b66c7b7158d609a771e64ca69` |
| Fossil check-in (`manifest.uuid`, also `sqlite_source_id()`) | `bf7c7f30031888f4e796e429ab3978879485813aaca6f641c7b33e4e09459bcc` |
| Local source tree | `$SQLITE_SRC`, else `_src/sqlite-3.53.4` in the repo (fetch with `go run ./cmd/course fetch-source`) |

**Why this revision.** The `sqlite3` CLI already installed here reports exactly that Fossil
check-in, and the tag `version-3.53.4` on the official GitHub mirror has the same
`manifest.uuid`. Newer releases exist (3.54.0 and 3.54.1), but moving to them would mean
replacing the system binary for no teaching gain, and 3.53.4 already includes the WAL-reset
corruption fix (3.51.3 / 3.53.0) and `SQLITE_CHECKPOINT_NOOP` (3.51.0).

**The pin is the source, and three builds of it are used** (section 4.1 explains the choice):

| Build | What it is | Reports the pinned source id? |
| --- | --- | --- |
| `modernc.org/sqlite v1.60.1` | The Go driver every in-process experiment uses. A machine translation of the 3.53.4 C source to Go, no C compiler needed | Yes [probed] |
| `sqlite3` CLI | The C build of the same check-in, installed here; a repo script rebuilds it from the pinned tree elsewhere | Yes [probed] |
| In-tree tools (`showwal`, `dbhash`) | Built from the pinned tree by a repo script, as cross-checks | Built from the pinned tree [probed] |

The course does not compile its own SQLite library and has no C code of its own.

**How each chapter records the pin.**

1. Every chapter `.md` opens with a pin block: release, tag, git commit, Fossil check-in, and
   the Go driver's module version (its content hash is in `go.sum`).
2. Every chapter directory has `SOURCES.sha256`: one line per copied SQLite file with its
   path in the pinned tree and its sha256. A repo script checks each copy against the pinned
   tree, so a copied file can never drift from the pin.
3. Every source reference in the text is `file:start-end` at the pin plus a link of the form
   `https://github.com/sqlite/sqlite/blob/version-3.53.4/src/wal.c#L2199-L2395`.
4. Every experiment prints `sqlite_source_id()` as its first output line and exits non-zero
   if it is not the pinned check-in. Experiments that run the CLI check its id the same way.
5. Where behavior differs across versions, the chapter has a short "Version notes" list drawn
   from the research reports (each item cites the changelog entry or the source comment).

---

## 2. Repo layout

```
sqlite-internals/
  OUTLINE.md  OUTLINE-CHANGES.md  README.md  STATE.md
  go.mod  go.sum                                      one Go module; pins modernc.org/sqlite v1.60.1
  docs/
    course-source.md                                 Nick's spec
    knowledge/                                       research reports and durable findings
  sqlite/
    PIN                                              tag, git commit, Fossil id, driver version
    build-tools.sh                                   pinned tree -> sqlite3 CLI, showwal, dbhash
  internal/
    sqlitex/      open with explicit settings, pin check, driver helpers (plain Go)  chunk 0
    trace/        strace runner and parser, kill-at-syscall, CLI -vfstrace runner    chunk 0
    recipe/       seeded, deterministic workload generator                           ch 1
    dbfile/       read-only database file parser (header, pages, cells, freelist)    ch 2
    equal/        the equality ladder: sha256, page diff, logical hash               ch 2–3
    walfile/      WAL parser, checksum chain, frame replayer                         ch 5
    procs/        child-process roles and step barriers for multi-process labs       ch 6
    walship/      snapshot + WAL-segment capture and restore                         ch 9
  ch01-architecture-and-execution-path/
    ch01-architecture-and-execution-path.md          the chapter
    SOURCES.sha256
    vdbeapi.c  vdbeaux.c  prepare.c  os.c             copies of the SQLite files it cites
    experiments/
      1.1-pin-check/main.go
      1.2-one-insert/main.go
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
    neon/                                            scripts that call Gemini 4 Neon (section 6)
    ch01/ ... ch10/                                  Phase 2 working scripts and Astra's adversarial scripts
```

Rules that implement Nick's specs 3, 5 and 6:

- **Chapter file name = directory name + `.md`.** One chapter per directory.
- **Copied SQLite files sit at the chapter directory's root**, under their real names, whole
  and unmodified. A file cited by two chapters is copied into both. Very large files cited for
  a single function are linked instead of copied; each chapter's list below says which.
- **Experiments are numbered `N.M`** and live in `experiments/N.M-name/main.go`. Run command,
  from the repo root, is always `go run ./chNN-…/experiments/N.M-name` (flags listed in the
  chapter). Scratch databases go under `_work/N.M/`, which is git-ignored and safe to delete.
- **The chapter inlines the Go code** of each experiment at the point in the reading where it
  belongs, followed by the run command and the expected evidence. A repo check compares each
  inlined block with the file on disk byte for byte, so text and code cannot diverge.
- **Shared packages are inlined once**, in the chapter that introduces them (the table above
  names the owner). Later chapters import them and link back.
- **`agent_work/` holds every working script** anyone writes: probes, one-off checks, Astra's
  falsification scripts, the Neon calling scripts. Each gets an INDEX row in the same commit.

Evidence is stated in one of three forms, and each experiment says which:

- **EXACT** — a hash, byte count, frame count or return code that must match on every run.
- **BOUND** — an inequality that must hold (for example "open file descriptors ≤ 3 × cap + 8").
- **SHAPE** — a trend that must hold (for example "WAL size grows while the reader is open and
  stops growing after it closes"). Used only where timing or the allocator makes exact numbers
  impossible; timings themselves are never the pass condition.

---

## 3. The through-line: the Rebuild Ladder

One arc runs through the course: capture a database's durable state deterministically, rebuild
a working database from it, and prove the rebuild by experiment. The course calls this the
**Rebuild Ladder**. A rung is a way to capture state, a rebuild from that capture, and a check
that the rebuild equals the original. Eight of the ten chapters add a rung. Chapters 4 and 7
add none; they supply what the rungs after them depend on (what makes a commit atomic, and
what each commit costs in I/O).

**What "equal" means.** Three levels, used throughout, implemented once in `internal/equal`:

| Level | Check | When it is the right claim |
| --- | --- | --- |
| **E1 bytes** | sha256 of the database file | Physical captures: page copies, WAL replay, file copies at rest |
| **E2 pages** | Page-by-page diff, ignoring a declared list of header bytes: 24–27 (change counter), 40–43 (schema cookie), 92–99 (version-valid-for, library version) | The backup API, which rewrites those fields |
| **E3 logical** | A Go logical hash (schema, every row with explicit rowid, every value tagged with its storage class, `sqlite_sequence`, persistent pragmas) plus `integrity_check` = ok plus unchanged `foreign_key_check` | Captures that repack the file: logical replay, `VACUUM INTO` |

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
| R3 Redo log | 5 | Base file + committed WAL frames | Apply frames in Go, stop at the last commit, truncate to the commit size | E1 against SQLite's own `wal_checkpoint(TRUNCATE)` [probed] |
| R4 Live snapshot | 6 | All pages read inside one read transaction while writers run | Write the pages out | E1 against the state at the reader's snapshot |
| R5 At-rest state | 8 | The single file left after the last connection closes | Copy it | E1; hash recorded on close and checked on reopen |
| R6 Point in time | 9 | One snapshot plus WAL segments shipped as they are written, across checkpoints | Restore to any commit *k* | E1 against the known state after commit *k* |
| R7 Fleet | 10 | R6 for every database a service owns, plus ownership records | Wipe a node and rebuild every database from the store | E1 per database, with process kills and store faults injected |

**Why this order holds up.**

- R0 comes first because every later rung needs a ground truth. A deterministic workload gives
  a known state after every commit *k*, with a known hash. Later chapters can then say exactly
  which state a crashed, restored or relocated database must equal. Probes confirmed the same
  script gives byte-identical files across seven configurations and two builds of 3.53.4.
- R1 before R3: a WAL frame is a page image, so the page reader must exist before the frame
  reader.
- R3 is the log a WAL-mode platform lives on. Replaying it in Go, and matching SQLite's own
  checkpoint byte for byte, is the strongest evidence the course can give that the reader
  understands the format.
- R4 and R5 are the two safe moments to capture a live database without reading the log:
  inside a read transaction, and after the last close.
- R6 is R1 + R3 made continuous. R7 is R6 with ownership, which is where SQLite's guarantees
  stop and the platform's begin.

**Determinism rules** (section 4.2) apply to every rung. Without them "rebuilt equal" would be
luck.

---

## 4. Key technical decisions

### 4.1 How Go reaches SQLite

**Decision (Nick's direction 3): existing libraries only. No first-party cgo binding, no
amalgamation build, no custom C.** Each kind of experiment uses the existing tool that shows
its subject most directly:

| Experiment type | Route | Why |
| --- | --- | --- |
| Everything that runs SQL from Go (chapters 1–10) | `modernc.org/sqlite v1.60.1` through `database/sql`, with `modernc.org/libc v1.77.1` as its `go.mod` requires | Runs exactly the pin; needs no C compiler; compiled with the two table functions the labs read pages through (`sqlite_dbpage`, `dbstat`); keeps upstream defaults; exposes the backup API, `sqlite3_db_status`, serialize and file-control through `Conn.Raw` [all probed] |
| Reading files (page inspector, WAL inspector, WAL replay, hashing) | Plain Go, no SQLite at all | The point of those labs is that our reader agrees with SQLite's writer |
| What SQLite asks of the VFS (`xWrite`, `xSync`, `xShmLock`, file controls) | The pinned `sqlite3` CLI with its built-in `-vfstrace`, run from Go with `os/exec` | The CLI is the C build of the pin, so the trace is the real `os_unix.c`; it sees shared-memory locks; it costs no code [probed] |
| What reaches the kernel (write offsets, fsync order, directory fsync, lock byte ranges) | `strace` on the Go process or on the CLI | Only strace sees the directory fsync and the real `fcntl` ranges [probed] |
| Repeatable process crashes | `strace --inject=…:signal=SIGKILL:when=K`, which kills the writer on entry to its *K*-th `fsync` or `pwrite64` | No shim needed. Five runs at each of six kill points gave identical outcomes [probed 2026-11-20, `agent_work/probes/outline-v2`] |
| The "other process" in multi-process labs | Go child processes; the CLI where an independent build is wanted | Both use the same POSIX locks and interoperate [probed] |
| Cross-checks of our Go parsers and hashes | `showwal` and `dbhash`, built from the pinned tree by `sqlite/build-tools.sh` | Both build with a plain `./configure` [probed] |

**Why modernc and not the other two stock drivers** [facts probed in Phase 1]:

| Driver | Runs exactly 3.53.4? | What rules it in or out for this course |
| --- | --- | --- |
| `modernc.org/sqlite` (translated to Go) | At v1.58.0–v1.60.1 | **Chosen.** Has `sqlite_dbpage` and `dbstat`; default `synchronous` is upstream's (FULL in WAL mode); files it writes are byte-identical to the CLI's; same POSIX lock calls as a C build |
| `mattn/go-sqlite3` (cgo, bundles the real C amalgamation) | At v1.14.50–v1.14.52 | Runs `PRAGMA synchronous=NORMAL` and a 5-second busy timeout on every connection unless the DSN overrides them; no `sqlite3_db_status`, which the memory experiments need; `sqlite_dbpage` is not in its default build; needs gcc |
| `ncruces/go-sqlite3` (WebAssembly) | At v0.35.6 with a forced dependency downgrade | Replaces `os_unix.c` with its own Go file layer using different lock calls; has neither `sqlite_dbpage` nor `dbstat` |

**The cost of this choice, stated plainly.** The code running inside a Go experiment is a
translation of `os_unix.c`, `pager.c` and the rest, not the C itself, and it carries two small
upstream patches (an optional lock mode that is off by default, and exception handling in
`wal.c`). The chapters read the C source. Two rules keep the two honest with each other:

- **The twin check.** Experiment 1.2 runs the same statements through the Go driver and
  through the C-built CLI and compares what reaches the kernel: the same writes to the same
  files at the same offsets, and the same syncs in the same order. Any later experiment whose
  claim is about a syscall sequence repeats that comparison. If the two ever differ, the
  chapter reports the difference instead of hiding it.
- **Claims about VFS calls are shown on the CLI**, whose `-vfstrace` output is the C code.

**What existing libraries cannot do, and what the outline does about each.** No custom piece
is needed for any experiment that remains.

| Not available without custom C | Was used for | Now |
| --- | --- | --- |
| A VFS shim inside the Go process | In-process VFS logs in most chapters | CLI `-vfstrace` for the VFS view, strace for the Go process |
| A fault shim that exits at the *N*-th write or sync | Crash experiments | strace `--inject` (same repeatability, zero code) |
| A shim that holds back unsynced writes (power-cut simulation) | Old experiments 7.5, 9.4, part of 10.4 | Cut, per Nick's direction 4. One understanding-level model remains in experiment 9.2 and uses only file operations |
| A shim that logs every write for replay | Old rung "I/O log" (7.4) | Cut |
| `sqlite3_test_control` (seeding SQLite's PRNG) | Byte-identical WAL files | Cut. WAL files are compared as sequences of (page number, page hash, commit size), which is the claim that matters |
| `sqlite3_snapshot_*`, `sqlite3_wal_hook` | Considered for continuous capture | Not needed: a held read transaction pins the WAL, and the shipper reads the WAL file itself |
| `SQLITE_DBCONFIG_NO_CKPT_ON_CLOSE` | Keeping a WAL file to inspect | A child process copies the `-wal` while the connection is still open |
| `sqlite3_status64` (process-wide memory), `sqlite3_stmt_status`, `sqlite3_trace_v2`, debug builds | Memory totals, opcode counters | Process memory is read from `/proc`; per-connection memory from `sqlite3_db_status`; the opcode-counter experiment is cut |

If Nick later wants real C running inside the Go process, the smallest step is swapping the
driver for mattn v1.14.52 with `_synchronous` and `_busy_timeout` set in the DSN. The memory
experiments would then lose their per-connection counters. This is open question 14.

Module layout: **one `go.mod` at the repo root.** `internal/sqlitex` is about 150 lines of
plain Go: open a database with every setting spelled out, check the pin, and wrap the driver
calls reached through `Conn.Raw`.

Three rules the probes showed are needed:

- **Never open and close the database file with `os.Open` in a process that also has a
  SQLite connection to it.** Closing any descriptor drops that process's POSIX locks on the
  file. Experiments hash or copy database files only after closing the connection, or from a
  child process. Chapter 6 teaches this hazard on purpose.
- **Crash points are chosen by syscall index, not by timing a signal.** The writer child pins
  its goroutine to one OS thread so that strace's counter sees its syscalls in order [probed].
- **Each experiment prints `sqlite_source_id()` first and fails if it is not the pin.**

### 4.2 Determinism, and how a rebuilt database is verified

**Enforced in `internal/recipe` and `internal/sqlitex`, for every experiment that makes a
byte-level claim:**

| Source of variation | Rule | Basis |
| --- | --- | --- |
| Library build | One driver version, fixed in `go.mod`; the CLI at the same check-in. Every experiment checks `sqlite_source_id()`. | Header bytes 96–99 store the writer's version, so files written by two versions differ by construction |
| Persistent file settings | Set explicitly on creation: `page_size=4096`, `auto_vacuum=NONE`, `encoding='UTF-8'`, `application_id` (one fixed course value), `user_version` (the recipe's step number), journal mode | These are stored in the file [probed] |
| Per-connection settings | Set on every connection: `synchronous`, `foreign_keys`, `secure_delete`, `cache_size`, `wal_autocheckpoint`, `busy_timeout` | These reset on every open [probed] |
| Data | Generated in Go from a seeded PRNG (`math/rand/v2` PCG with a fixed seed); values are bound as parameters | No SQL `random()`, `randomblob()`, `current_timestamp`, `date('now')` [probed: each one breaks equality] |
| Row identity | Every table has an explicit integer primary key and every insert supplies it | Rowid choice past the maximum is random [probed] |
| Row order | Every `INSERT … SELECT` has `ORDER BY` | Unordered inserts follow the query plan [probed] |
| Transaction boundaries | Part of the recipe: step *k* is exactly one transaction | Same rows in different batches give different bytes [probed] |
| Freed pages | `secure_delete=ON` where two different histories must match | Freelist leaves keep stale bytes otherwise [probed] |
| WAL salts | WAL files are compared as the sequence of (page number, page hash, commit size), never byte for byte | Salts and the checksums that depend on them are the only run-to-run difference in a WAL [probed] |
| Process and clock | One writer goroutine per database; no wall-clock values stored; multi-process labs use step barriers, not sleeps | Interleavings become part of the script |
| Filesystem | Experiments run on the local ext4 volume under `_work/`; the chapter states this | fsync behavior is filesystem-specific |

**Verification.** The three equality levels in section 3. The rule for choosing: claim the
strongest level the capture method can reach, and say why it cannot reach the next one.

| Capture method | Strongest level reached | Evidence |
| --- | --- | --- |
| Recipe re-run, same boundaries | E1 | [probed] |
| File copy of a closed database | E1 | [probed] |
| Pages via `sqlite_dbpage` | E1 | [probed] |
| Our WAL replay vs SQLite's checkpoint | E1 | [probed], including an uncommitted tail and a corrupted last frame |
| Backup API | E2 (differs only at header bytes 24, 40, 92) | [probed] |
| `VACUUM INTO`, logical replay | E3; E1 after canonicalizing both sides | [probed] |
| Copy of a live database | None: 8 of 8 copies taken under a writer failed `integrity_check` | [probed]; taught as the failure case |

Byte hashes alone would reject correct logical rebuilds, and logical diffs alone would accept a
physically wrong page copy. The course needs all three and teaches when each applies.

### 4.3 The VFS, without building one

No chapter builds or installs a VFS. The table shows where each chapter gets its view of I/O.

| Chapter | View of I/O | What for |
| --- | --- | --- |
| 1 | strace on the Go process; CLI `-vfstrace` once | One statement crossing every layer; the twin check |
| 2, 3 | None | The file on disk is the evidence |
| 4 | strace, with kill-at-syscall | Journal and sync order; a repeatable crash mid-commit |
| 5 | None beyond file reads | The WAL and `-shm` files are the evidence |
| 6 | `/proc/locks`; strace for lock calls | Who holds which lock |
| 7 | **CLI `-vfstrace` and strace in one ordered stream — this is the chapter** | VFS calls mapped to syscalls; I/O cost per commit |
| 8 | strace for reads at open; `/proc/self/fd` | Lazy schema load, descriptors |
| 9 | strace, with kill-at-syscall | Process crash, and the record of which writes were synced |
| 10 | Process kills; fault injection in the object-store stand-in (plain Go) | Invariants under faults |

Chapter 7 still teaches what a VFS is and where a platform could put one. It reads
`ext/misc/vfstrace.c` as the pattern for a shim and says what a custom VFS would have to honor.
It does not write one, because a platform that keeps each database on local disk and ships the
WAL does not need one.

**Power loss** is covered at the level Nick set in direction 4. The course keeps the
distinction between a process crash and a power cut, shows how many syncs each setting buys
per commit (7.2), shows with one file-level model what a power cut can take away at each
`synchronous` level (9.2), and states what that means for a machine holding thousands of
databases (chapters 9 and 10). There is no power-cut simulator, no torn-write injection and no
block-device tooling.

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

Experiment count: **32** (revision 1 had 55). Per chapter: 3, 3, 3, 2, 4, 4, 2, 3, 4, 4.

### Chapter 1 — Architecture and execution path

Directory `ch01-architecture-and-execution-path/`, file `ch01-architecture-and-execution-path.md`.

**Question.** When a Go program commits one `INSERT`, which layer does what between
`sqlite3_prepare_v2` and the bytes on disk, and which of those layers can a platform see from
outside the library?

**Reading path** (in order):

1. https://www.sqlite.org/arch.html — name the layers
2. `vdbeapi.c` `sqlite3_step` → `sqlite3Step` (919–966, 777–912)
3. `prepare.c` `sqlite3LockAndPrepare` → `sqlite3Prepare` (836–875, 682–835) — prepare compiles, step runs
4. https://www.sqlite.org/opcode.html — only the opcodes of the lab's one `INSERT`
5. `vdbe.c` `OP_Transaction` (4102–4201), `OP_Insert` (5748–5834) — the B-tree boundary
6. `vdbeaux.c` `sqlite3VdbeHalt` → `vdbeCommit` (3315–3517, 2919–3175) — the pager boundary
7. `os.c` `sqlite3OsRead/Write/Sync` (88–102) — the VFS boundary

**Copied into the chapter directory:** `vdbeapi.c`, `vdbeaux.c`, `prepare.c`, `os.c`.
**Linked, not copied:** `vdbe.c` (two opcodes cited).

The parser, tokenizer, code generator and query planner are named in the model and not read.
A platform cannot observe or replace them, and nothing on the ladder depends on them.

**Sections.**

1. Model — the layer diagram (interface, compiler, VDBE, B-tree, pager, VFS) and what each owns.
2. Question — one `INSERT`, followed end to end. Experiment 1.1 here.
3. Call path — prepare, step, commit, with spans. Experiment 1.2.
4. Timeline — the opcodes of the `INSERT` laid against layer crossings and syscalls.
5. Lab — the deterministic workload that every later chapter reuses. Experiment 1.3.
6. Evidence and failure cases — what breaks determinism (second half of 1.3).
7. Platform implications — statements are compiled per connection; the two layers a platform touches are the file (everything below the pager) and the connection (everything above it).
8. Knowledge check.

**Experiments.**

**1.1 pin-check** · EXACT · placed at step 2 · [probed]
- Claim: every build the course uses is SQLite 3.53.4 at the pinned check-in.
- Does: opens an in-memory database through `internal/sqlitex`; reads `sqlite_version()`, `sqlite_source_id()`, `PRAGMA compile_options`; runs the CLI for the same values.
- Evidence: both source ids equal `bf7c7f30…59bcc`; the driver's options include `ENABLE_DBPAGE_VTAB`, `ENABLE_DBSTAT_VTAB` and `DEFAULT_WAL_SYNCHRONOUS=2`.
- Astra will try: raise the driver version in a scratch module and confirm the experiment fails loudly; confirm every other experiment refuses to run on the wrong build.

**1.2 one-insert-through-the-layers** · EXACT · placed at step 3 and 4 · [probed for strace and `-vfstrace`; unprobed for the twin comparison]
- Claim: one committed `INSERT` crosses every layer, and each crossing is visible from outside: opcodes (VDBE), a changed page (B-tree), journal and database writes (pager), `xWrite` and `xSync` (VFS), `pwrite64` and `fsync` (kernel). The Go driver and the C build do the same I/O.
- Does: runs one `INSERT` in rollback-journal mode from Go under strace; records the `EXPLAIN` listing, the page that changed (page hashes before and after, through `sqlite_dbpage`), and the syscalls. Runs the same statements in the CLI with `-vfstrace` under strace. Compares the two syscall lists (the twin check, section 4.1).
- Evidence: one table of opcode → changed page number → VFS call → syscall with file and offset; (changed page number − 1) × 4096 equals the `pwrite64` offset on the database file; the Go and CLI syscall lists match in file, offset, length and order for writes and syncs.
- Astra will try: repeat in WAL mode and confirm the database-file write disappears from the commit (it moves to the WAL), which the chapter text must predict; hunt for any statement where the two builds' I/O differs.
- Why kept: this is the one place the course earns the right to use a translated driver. Everything after relies on it.

**1.3 recipe-rebuild (rung R0)** · EXACT · placed at step 5 and 6 · [probed]
- Claim: a database built by a fixed, seeded workload is reproducible to the byte, so the workload is a complete saved state; and each determinism rule in section 4.2 is necessary.
- Does: introduces `internal/recipe`: a seeded generator producing a schema and *N* numbered transactions (inserts, updates, deletes, one index, rows large enough to overflow). Runs it twice into two files, closing each; hashes both; prints the hash after each step *k*. Then runs five broken variants twice each: SQL `random()`; `current_timestamp`; an unordered `INSERT … SELECT`; the same rows in different transaction batches; rowid assignment past the maximum rowid.
- Captures / rebuilds: capture is the recipe (seed, step count); rebuild is running it again.
- Evidence: the two sha256 values are equal and the per-step hash list is identical on a second run; for each broken variant the two hashes differ, with the first differing page reported.
- Astra will try: different working directories, file names, `cache_size` values and machine load; 50 runs; a sixth source of variation the outline missed; the recipe built with the newest driver release (SQLite 3.54.1) in a scratch module, to show which bytes a library upgrade changes.

**Feeds the through-line.** R0. This chapter creates the ground truth every later rung is
checked against: the state after step *k* has a known hash.

---

### Chapter 2 — Database file format and pages

Directory `ch02-file-format-and-pages/`, file `ch02-file-format-and-pages.md`.

**Question.** Given only the bytes of a database file, can a Go program with no SQLite in it
read the header, say what every page is for, and rebuild the file from its pages?

**Reading path:**

1. https://www.sqlite.org/fileformat2.html — header and B-tree page sections
2. `btreeInt.h` 1–228 — the format as the authors describe it
3. `btree.c` `newDatabase` (3510–3547) — header bytes written at creation
4. `btree.c` `lockBtree` (3282–3459) — header bytes checked at open
5. `btree.c` `decodeFlags`, `btreeInitPage` (2028–2085, 2218–2265) — page header
6. `btree.c` `btreeParseCellPtr` (1259–1385) — cells and the local/overflow split
7. `util.c` `sqlite3GetVarint` (1633–1781); `vdbeaux.c` `sqlite3VdbeSerialGet` (4123–4214) — enough record decoding to read the schema table
8. `btree.c` `getOverflowPage` (5007–5062); `allocateBtreePage`, `freePage2` (6514–6822, 6836–6969)
9. `pager.c` `pager_write_changecounter` (3130–3143)

**Copied:** `btreeInt.h`, `btree.c`, `util.c`. **Linked:** `vdbeaux.c`, `pager.c`.

**Sections.**

1. Model — a file is an array of fixed-size pages; page 1 carries the 100-byte header; page roles (table/index, interior/leaf, overflow, freelist trunk/leaf, pointer map).
2. Question — decode the R0 database from raw bytes.
3. Call path — where SQLite writes and checks each header field; page and cell parsing. Experiment 2.1.
4. Timeline — which header bytes change on a commit.
5. Lab — the read-only page inspector (the course doc's lab). Experiments 2.2 and 2.3.
6. Evidence and failure cases — what the inspector must reject.
7. Platform implications — the page is the unit of everything later (cache, WAL frame, backup step); page size is fixed at creation; the header is a cheap fingerprint; bytes 24 and 92 matter to anything that caches or copies files; bytes 96–99 mean a byte hash is only stable within one library version.
8. Knowledge check.

**Experiments.**

**2.1 header-and-what-a-commit-changes** · EXACT · placed at step 3 and 4 · [probed]
- Claim: the 100-byte header can be decoded field by field and matches what SQLite reports; a commit in rollback mode advances the change counter (24) and version-valid-for (92), and the in-header size (28) is trusted only when those two agree.
- Does: `internal/dbfile` parses the header of the R0 database; compares each field with the matching pragma (`page_size`, `page_count`, `freelist_count`, `schema_version`, `user_version`, `application_id`, `encoding`, `auto_vacuum`) and with the CLI's `.dbinfo`; runs one more recipe step and lists the bytes of page 1 that changed.
- Evidence: a field table with "ours / SQLite's / match", all rows matching; byte 96 reads 3053004; bytes 24–27 and 92–95 each advance by one.
- Astra will try: other page sizes (512, and 65536 stored as 1); a truncated file; WAL mode, where the research probe saw the counter stay constant across commits on one connection — the chapter must state and show that difference.

**2.2 page-census** · EXACT · placed at step 5 · [unprobed; the oracles are probed]
- Claim: every page in the file has exactly one role, and a Go program can find it: walk each B-tree from the roots listed in the schema table, follow overflow chains, walk the freelist from header offset 32.
- Does: decodes the schema table on page 1 (the only records the inspector must fully decode) to get root pages; walks every B-tree; follows the overflow chains of the recipe's large rows; walks the freelist trunk and leaf pages; compares page type, cell count and unused bytes with `dbstat`, and the recipe tables' rows with `SELECT`.
- Evidence: the census sums to `page_count` with no page counted twice or missed; freelist length equals `PRAGMA freelist_count`; per-page `ncell` and `unused` equal `dbstat`'s; row hashes for the recipe tables equal SQLite's.
- Astra will try: a freelist longer than one trunk page; a payload exactly at the local limit and one byte over; one flipped byte in a page header (the inspector must report an error, not panic); a file larger than 1 GiB, where one page at the lock-byte offset is never used and must not be counted as lost.
- Why kept: the census is what lets a platform account for every byte of a file it stores, and the page walk is reused by R1, R3, R4 and R6. Decoding every record type in every kind of table is not needed for that and is out of scope.

**2.3 page-manifest (rung R1)** · EXACT · placed at step 5 · [probed: closed-file copy and page export are byte-identical]
- Claim: the ordered list of page images is the complete state of a closed database; writing those pages back rebuilds the file exactly.
- Does: reads every page of the closed R0 database with `internal/dbfile`; writes a capture (one manifest line per page: number, role, sha256; plus the page images); rebuilds a new file from the capture alone; opens the rebuild with SQLite and runs `integrity_check`. Introduces `internal/equal` level E1.
- Captures / rebuilds: capture = page images + manifest; rebuild = pages written in order.
- Evidence: sha256 of rebuild equals sha256 of original; `integrity_check` = ok; the manifest for two runs of the recipe is identical line for line.
- Astra will try: drop, duplicate or reorder a page in the capture and confirm the verifier names the page; capture a database that is not closed (a `-wal` exists) and confirm the tool refuses rather than producing a stale file.

**Feeds the through-line.** R1, and the page reader that R3, R4 and R6 reuse.
`internal/equal` starts here with E1.

---

### Chapter 3 — B-trees and page management

Directory `ch03-btrees-and-page-management/`, file `ch03-btrees-and-page-management.md`.

**Question.** How many pages does one insert or delete change, where do freed pages go, and
why do two databases with the same rows end up with different bytes?

**Reading path:**

1. `btreeInt.h` 149–228 — cells and overflow, as a refresher
2. `btree.c` `sqlite3BtreeInsert` (9409–9710) — the spine
3. `balance` (9130–9259) — the dispatcher and its three cases, read for what each case costs in pages
4. `balance_quick`, `balance_deeper` (8007–8101, 9049–9094) — the two short cases
5. `balance_nonroot` (8245–9027) — its header comment only
6. `sqlite3BtreeDelete`, then `freePage2` / `allocateBtreePage` — page reuse
7. `checkTreePage` (10855–11118) — the invariants written as checks

**Copied:** `btree.c`, `btreeInt.h`.

Version notes for this chapter: the insert path calls `insertCellFast`, not `insertCell`;
older books show the old name.

**Sections.**

1. Model — table B-trees (rowid keys, data in leaves) versus index B-trees; a page as a cell-pointer array growing down and cell content growing up.
2. Question — insert until the page is full, then one more.
3. Call path — `sqlite3BtreeInsert` to `balance`. Experiment 3.1.
4. Timeline — page layout before and after a split, drawn from real page dumps.
5. Lab — force splits and compare layouts (the course doc's lab). Experiments 3.1 and 3.2.
6. Evidence and failure cases — Experiment 3.3: layout depends on history.
7. Platform implications — the number of pages a write dirties is the number of WAL frames it ships; file size and fragmentation depend on write history; a file does not shrink on delete; `VACUUM` cost; what `secure_delete` buys; why a physical copy and a logical copy of the same data differ.
8. Knowledge check.

**Experiments.**

**3.1 pages-per-insert** · EXACT · placed at step 3 and 5 · [unprobed]
- Claim: most inserts change one page; an insert that fills a leaf changes several (the new leaf, the old one or its siblings, the parent), and when the root overflows the tree gains a level while the root keeps its page number.
- Does: inserts rows one at a time, taking a page manifest after each; reports the set of changed pages per insert; prints the layout of the pages involved in the first append split, the first middle split and the first depth change, using the chapter 2 inspector.
- Evidence: the changed-page count per insert (1 for ordinary inserts; the exact sets at each split); the rowid of the first append split, with the arithmetic from cell size and page size; root page number constant across depth changes and equal to `sqlite_schema.rootpage`; `integrity_check` = ok at every step.
- Astra will try: random insert orders under a fixed seed; other row sizes and the predicted split row; a table with two indexes, to show the per-insert page count multiply.
- Why kept: a platform that ships WAL frames pays per changed page. Revision 1 had two experiments tracing which balance routine runs; that attribution is cut and only the page cost is kept.

**3.2 delete-and-reuse** · EXACT · placed at step 5 · [probed for the freelist and stale-content behavior]
- Claim: deleting rows moves emptied pages to the freelist, later inserts reuse them before the file grows, the file never shrinks on its own, and a freed page keeps its old bytes unless `secure_delete` is on.
- Does: deletes a key range, records `freelist_count` and file size; inserts new rows and records which page numbers they land on; searches freelist leaves for deleted row content with and without `secure_delete`.
- Evidence: file size unchanged across delete and re-insert; new rows occupy former freelist pages; deleted payload bytes found in the file without `secure_delete` and absent with it.
- Astra will try: confirm stale bytes also survive in a plain file copy and in a page-level backup — a point about tenant data in backups; `VACUUM` as the way to return the space.

**3.3 same-rows-different-bytes (rung R2)** · EXACT · placed at step 6 · [probed]
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

**Question.** What makes a commit all-or-nothing when the process dies halfway, and how much
memory does each open database hold?

**Reading path:**

1. https://www.sqlite.org/atomiccommit.html
2. `pager.c` 135–406 — the state machine comment; `PAGER_*` states at 351–357
3. `sqlite3PagerSharedLock`, `hasHotJournal` (5300–5507, 5180–5271)
4. `sqlite3PagerCommitPhaseOne` (6511–6730)
5. `pager_end_transaction`, `pager_playback` (2052–2180, 2864–3052)
6. `pcache.c` `sqlite3PcacheFetch` and the dirty list; `pager.c` `pagerStress` (4655–4727)
7. `pcache1.c` structures (117–211); `status.c` 203–421

**Copied:** `pager.c`, `pcache.c`, `pcache1.c`, `status.c`.

**Sections.**

1. Model — the pager as transaction manager and cache; rollback-journal mode only (WAL is chapter 5).
2. Question — one multi-page transaction, from first page write to commit, killed at each step.
3. Call path — begin, write, commit phase one and two. Experiment 4.1.
4. Timeline — journal and database writes and syncs for one commit.
5. Lab — cache and memory with bounded and unbounded connection counts (the course doc's lab). Experiment 4.2.
6. Evidence and failure cases — the hot journal; what a deleted journal does.
7. Platform implications — a `-journal` file next to a database is part of that database; per-connection cache is the main memory cost of an open database; `cache_size` is a budget per connection, not per process; mmap moves the memory from SQLite's heap to the kernel.
8. Knowledge check.

**Experiments.**

**4.1 atomic-commit-and-hot-journal** · EXACT · placed at step 3 and 6 · [probed for the sync order and for kill-at-syscall in WAL mode; unprobed in rollback mode]
- Claim: in rollback-journal mode the original page images are written and synced to the journal before any change reaches the database file, and the commit point is the removal of the journal. A process killed anywhere in that sequence leaves a database that SQLite restores, on the next open, to exactly the state before or exactly the state after.
- Does: runs one recipe step from a Go child under strace and records the order of writes, syncs and the unlink. Then kills the child on entry to each sync and each write in turn (`strace --inject`), reopens the database each time, and hashes it.
- Evidence: call order journal write → journal fsync → directory fsync → database write → database fsync → journal unlink; fsync counts 3 at `NORMAL` and 4 at `FULL`, as the research probe measured; for every kill point the reopened file's sha256 equals the pre-step hash or the post-step hash, never a third value.
- Astra will try: delete the `-journal` before reopening (the result must be detectably corrupt or silently wrong — the chapter's warning about never deleting or losing journal files); copy the database without its hot journal; `synchronous=OFF` (no syncs, and the claim still holds for a process kill).
- Why kept: a WAL-mode platform still meets rollback mode. Every new database starts in it, and a file moved without its `-journal` is a corrupt file. One experiment covers that. Revision 1's journal parser and Go replayer (old rung "undo log") are cut: the platform never replays a journal itself.

**4.2 cache-budget** · BOUND and SHAPE · placed at step 5 · [probed for the counters]
- Claim: page-cache memory is per connection and bounded by `cache_size`; total memory grows linearly with open connections unless connections are bounded.
- Does: opens *N* connections (to *N* files, then to one file) and scans each; reads `SQLITE_DBSTATUS_CACHE_USED`, hit and miss counters per connection, and process memory from `/proc/self/status`; repeats with a cap of *K* open connections managed LRU; repeats once with `mmap_size` covering the file.
- Evidence: per-connection cache bytes ≤ `cache_size` budget plus a stated overhead; process total grows with *N* when unbounded and stays flat past *K* when bounded; with mmap, `CACHE_USED` falls and the memory appears as mapped file pages instead.
- Astra will try: tiny and huge `cache_size`; `PRAGMA shrink_memory`; `PRAGMA soft_heap_limit`; check the bound holds for 1,000 connections.

**Feeds the through-line.** No rung. The chapter supplies two things the later rungs stand on:
the rule that a database in rollback mode is its file plus its journal, and the memory figure
per open database that the bounded manager (R5) and the fleet (R7) are sized by.

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
5. `wal.c` `walFrames` (4038–4266)
6. `wal.c` `walTryBeginRead` (3020–3272) — read marks
7. `wal.c` `walCheckpoint` (2199–2395), `sqlite3WalCheckpoint` (4301–4432) — modes
8. `wal.c` 2152–2166 and 3875–3915 — restart and the salt change
9. `wal.c` `walIndexRecover` (1390–1614)

**Copied:** `wal.c`, `wal.h`. **Linked:** `pager.c`, `main.c`.

Notes the chapter must carry: the comment at `pager.c` 607–617 describes the sync bits the
wrong way round relative to `wal.h` 25–26 and the code, so the chapter cites the code. WAL2
and `BEGIN CONCURRENT` are not in this release.

**Sections.**

1. Model — the WAL as a redo log of page images; the wal-index (`-shm`) as its lookup table; commit = a frame with a non-zero size field.
2. Question — five commits, one long reader, one checkpoint.
3. Call path — commit in WAL mode; begin-read; checkpoint. Experiment 5.1 after the frame format.
4. Timeline — frames appended per commit; read marks; backfill; restart. Experiment 5.3.
5. Lab — the WAL frame inspector and WAL growth under a long-lived reader (the course doc's lab). Experiments 5.1–5.3.
6. Evidence and failure cases — Experiment 5.4.
7. Platform implications — one stuck reader makes the WAL grow without bound; the platform must own checkpoints; `-wal` is part of the database while it is open and `-shm` is not (it is rebuilt); the three numbers to watch per database (WAL size, and the busy/log/checkpointed result of a checkpoint).
8. Knowledge check.

**Experiments.**

**5.1 wal-inspector** · EXACT · placed at step 3 and 5 · [probed]
- Claim: a WAL file is a 32-byte header plus frames of (24-byte header + page image), linked by a running checksum; a transaction is committed exactly when its last frame carries the database size; two runs of the same workload write the same frames in the same order and differ only in the random salts and the checksums that depend on them.
- Does: `internal/walfile` parses a WAL left by five recipe steps (a child process copies the `-wal` while the connection is still open); verifies the checksum chain and salts; lists frames with page number, commit size and page hash; runs the recipe a second time and compares the two WALs byte for byte and as (page number, page hash, commit size) sequences.
- Evidence: file size = 32 + frames × (24 + 4096); number of commit frames = 5; frame count equals the `log` column of `PRAGMA wal_checkpoint(NOOP)`; listing equals `showwal` from the pinned tree; the byte comparison fails only inside salt and checksum fields and the frame sequences are equal.
- Astra will try: other page sizes; a WAL whose tail holds frames from an older generation; different `synchronous` levels (the frame sequence must not change).

**5.2 wal-replay (rung R3)** · EXACT · placed at step 5 · [probed]
- Claim: a base database file plus the committed frames of its WAL is the complete state; applying those frames in order, stopping at the last commit frame and truncating to its size, gives the same file SQLite's own checkpoint gives.
- Does: copies base and WAL from a child process at a quiet moment; rebuilds with `internal/walfile` (no SQLite); separately lets SQLite run `wal_checkpoint(TRUNCATE)` on the original; compares. Repeats with the WAL copied mid-transaction (uncommitted frames at the tail, including a transaction large enough to spill frames into the WAL before it commits) and after a rollback.
- Captures / rebuilds: capture = base file + WAL frames up to the last commit; rebuild = frame replay.
- Evidence: sha256 of our rebuild equals sha256 of SQLite's checkpointed file in all three cases; the replayer reports how many tail frames it ignored and why.
- Astra will try: flip one byte in the last commit's frames (the rebuild must equal SQLite's own recovery of the same damaged WAL, one transaction short); replay a WAL onto a base from an earlier generation (must be detected — the chapter's rule that base and WAL travel together); apply frames past the last commit and show the result contains rolled-back data.

**5.3 reader-pins-the-wal** · EXACT for marks and counts, SHAPE for growth · placed at step 4 and 5 · [probed for the read-mark behavior]
- Claim: a reader holds a snapshot by pinning a frame number; a checkpoint cannot copy frames past the oldest pinned mark, and the WAL cannot restart while any reader is on it, so the WAL grows for as long as that reader stays open.
- Does: opens a reader and leaves its transaction open; a writer commits in a loop with autocheckpoint on; samples WAL size and decodes `mxFrame`, `nBackfill` and the read marks from the `-shm` file; ends the reader and samples again.
- Evidence: WAL size rises monotonically while the reader is open; `nBackfill` stays at the reader's mark; the reader's row count never changes; after the reader ends, one checkpoint brings `nBackfill` to `mxFrame` and the next write restarts the WAL at frame 1.
- Astra will try: several readers at different marks; a reader that only holds a result set half read; `journal_size_limit` and `TRUNCATE` to show when the file actually shrinks.

**5.4 checkpoint-modes** · EXACT · placed at step 6 · [probed in part]
- Claim: `PASSIVE`, `FULL`, `RESTART` and `TRUNCATE` differ in what they wait for and what they leave behind, and a restart is visible as new salts.
- Does: runs each mode with and without a blocking reader; records the (busy, log, checkpointed) result, WAL file size, and the header salts before and after.
- Evidence: a result table per mode; `PASSIVE` with a reader returns partial progress and not busy; `TRUNCATE` leaves a zero-length WAL; salt-1 increments on restart.
- Astra will try: a writer racing each mode; a checkpoint issued by a second connection while the first believes it owns checkpointing.
- Why kept: the R6 shipper has to own checkpoints and has to notice when anything else restarted the WAL. The salt change is how it notices.

**Feeds the through-line.** R3, the core of the course: the reader's own program turns "base +
log" into the same bytes SQLite produces. Experiment 5.3 supplies the mechanism (a held read
mark) that makes continuous capture safe in R6.

---

### Chapter 6 — Locking and concurrent access

Directory `ch06-locking-and-concurrency/`, file `ch06-locking-and-concurrency.md`.

**Question.** When two processes use one database, who waits, who gets `SQLITE_BUSY`, and when
is it safe to copy a database that another process is writing?

**Reading path:**

1. https://www.sqlite.org/lockingv3.html and https://www.sqlite.org/isolation.html
2. `os.h` 98–166 — lock levels and the byte ranges
3. `os_unix.c` 1189–1342 — the comment on POSIX lock semantics, and the inode structures
4. `os_unix.c` `unixLock` (1843–2095) — read for the levels, not line by line
5. `os_unix.c` 2101–2109, 1471–1482 — deferred close of descriptors
6. `wal.c` 294–299 — WAL lock slots
7. `pager.c` `sqlite3PagerBegin` (5968–6032) — `BEGIN` modes
8. `btree.c` 3598–3804 — busy retry; `main.c` 1730–1792 — the default busy callback
9. `wal.c` 3699–3746 — `SQLITE_BUSY_SNAPSHOT`

**Copied:** `os_unix.c`, `os.h`. **Linked:** `wal.c`, `pager.c`, `btree.c`, `main.c`.

Facts the chapter states: Linux builds use POSIX advisory locks, which belong to the process
and not to the connection or the descriptor; in WAL mode `BEGIN EXCLUSIVE` behaves like
`BEGIN IMMEDIATE`.

**Sections.**

1. Model — lock levels in rollback mode; lock slots in the `-shm` file in WAL mode; locks belong to the process.
2. Question — two processes, one database.
3. Call path — lock transitions; where the busy handler is and is not called. Experiment 6.1.
4. Timeline — lock timelines for writer-versus-reader in WAL mode, with rollback mode as the contrast. Experiment 6.2.
5. Lab — multiple processes; record contention and busy results (the course doc's lab). Experiments 6.2 and 6.3.
6. Evidence and failure cases — Experiment 6.4.
7. Platform implications — one writer per database is SQLite's rule, so the platform must decide who that writer is; SQLite's locks work between processes on one machine and nowhere else; `BEGIN IMMEDIATE` for writers; never touch the database file with ordinary file calls in the serving process; a capture must go through SQLite's locking, not around it.
8. Knowledge check.

All multi-process experiments use `internal/procs`: the experiment binary re-runs itself in
named child roles, and the parent drives them through numbered steps over pipes. The
interleaving is part of the script, so outcomes are exact rather than timing-dependent.

**Experiments.**

**6.1 locks-belong-to-the-process** · EXACT · placed at step 3 · [probed for the observation method; the hazard is documented, unprobed here]
- Claim: SQLite's locks are byte-range locks on the database file (and on the `-shm` file in WAL mode), held by the process; closing any descriptor to the file, even one SQLite never saw, releases them.
- Does: one child takes a write lock in rollback mode and pauses; the parent reads `/proc/locks` and confirms a second process cannot write. The child then opens and closes the database file with plain `os.Open`; the parent reads `/proc/locks` again and the second process tries again.
- Evidence: the lock is listed for the child's pid before the stray close and gone after it; the second writer is refused before and admitted after, while the first still believes it holds the lock.
- Astra will try: the same in WAL mode on the `-shm` locks; two connections in one process (the locks are shared — no second lock call); the CLI as the second process.
- Why kept: this is the hazard a Go platform is most likely to hit by accident (hashing or copying a file it also has open). Revision 1's full table of byte offsets per lock level is cut to what this experiment needs.

**6.2 busy-rules** · EXACT · placed at step 4 and 5 · [probed for cross-process interop; unprobed for the result codes]
- Claim: in WAL mode readers never block the writer and the writer never blocks readers; a second writer gets `SQLITE_BUSY` at a point fixed by its `BEGIN` mode; a busy timeout turns that failure into a wait; and a read transaction that tries to write after someone else committed fails with `SQLITE_BUSY_SNAPSHOT` (517) at once, whatever the timeout.
- Does: runs two children through scripted interleavings: reader with writer; writer with writer under `BEGIN DEFERRED` and `BEGIN IMMEDIATE`, with `busy_timeout` 0 and then generous; a deferred reader upgrading after another commit. Records each statement's extended result code. Runs the reader-with-writer case once in rollback mode as the contrast.
- Evidence: the result table, stored in `evidence/`; with a timeout and `IMMEDIATE`, every transaction commits and the final database equals the recipe's expected state; code 517 arrives immediately and the same write succeeds after the transaction restarts; in rollback mode the reader blocks the writer's commit.
- Astra will try: three processes; the CLI as one participant; every cell re-run 100 times to confirm it is not timing-dependent.

**6.3 torn-copy** · EXACT · placed at step 5 · [probed: 8 of 8 naive copies failed]
- Claim: copying a database file while it is being written does not give a database; copying it without its WAL silently loses commits.
- Does: a writer child runs the recipe continuously; a helper child copies the file with plain reads at scripted points; checks each copy with `integrity_check` and the logical hash against the recipe's known states.
- Evidence: copies taken under the writer fail `integrity_check` or match no recipe state; a copy of the main file alone in WAL mode opens cleanly but equals an older state than the last committed one.
- Astra will try: find a copy timing that happens to pass and show it still is not guaranteed (the chapter's claim is "not safe", not "always corrupt").

**6.4 live-snapshot (rung R4)** · EXACT · placed at step 6 · [probed: page capture in a read transaction is byte-identical on a quiet database; under a writer it is unprobed]
- Claim: all pages read inside a single read transaction form a consistent database equal to the state at that transaction's snapshot, even while a writer keeps committing. A plain copy of the database file and its `-wal`, taken while a transaction is held open, is also a valid capture.
- Does: a writer child commits recipe steps without pause; the capture child begins a read transaction in WAL mode, notes the recipe step it sees (`user_version`), reads every page through `sqlite_dbpage`, writes them out as a chapter 2 capture, and rebuilds a file from it. Second method: with the read transaction still held, a helper copies the database file and then the `-wal`; the pair is rebuilt with the chapter 5 replayer.
- Captures / rebuilds: capture = pages as of one snapshot (or file + WAL under a held transaction); rebuild = pages written in order (or frame replay).
- Evidence: the page capture passes `integrity_check`, and its sha256 equals the file produced by running the recipe to exactly that step and checkpointing; the file-plus-WAL capture rebuilds to a recipe state at or after the snapshot step.
- Astra will try: a capture that spans two read transactions (must be shown inconsistent); the file-plus-WAL copy while another connection checkpoints during the copy; the same copy holding `BEGIN IMMEDIATE` instead of a read transaction, which is the procedure "Production SQLite" gives — find out whether a checkpoint can still run under it and whether the capture is still valid; a reader slow enough to make the WAL grow, tying back to 5.3.

**Feeds the through-line.** R4: the first capture taken from a live database, and the proof of
why a capture must go through SQLite's locking rather than around it.

---

### Chapter 7 — VFS and filesystem I/O

Directory `ch07-vfs-and-filesystem-io/`, file `ch07-vfs-and-filesystem-io.md`.

**Question.** What exactly does SQLite ask the operating system to do for one commit, in what
order, and what does each choice of journal mode and `synchronous` cost?

**Reading path:**

1. https://www.sqlite.org/vfs.html
2. `sqlite.h.in` 584–878 — open flags, `sqlite3_file`, `sqlite3_io_methods`
3. `os.c` 82–246 — the wrappers the core calls
4. `os_unix.c` `unixOpen` (6538–6826)
5. `os_unix.c` read and write (3463–3721); `full_fsync`, `unixSync` (3778–3956); `unixDelete` (6833–6869)
6. `ext/misc/vfstrace.c` — as the pattern for a shim: a VFS that forwards to another
7. https://www.sqlite.org/atomiccommit.html — the section on what SQLite assumes a sync does

**Copied:** `os_unix.c`, `os.c`, `os.h`, `vfstrace.c`. **Linked:** `sqlite.h.in` (very large).

**Sections.**

1. Model — the VFS as the only door to the OS: one object per filesystem, one per open file; a shim is a VFS that forwards to another.
2. Question — trace one commit in each journal mode through the VFS to syscalls.
3. Call path — `sqlite3OsWrite` → `unixWrite` → `pwrite`; `sqlite3OsSync` → `unixSync` → `full_fsync`; open flags by file type. Experiment 7.1.
4. Timeline — VFS calls and syscalls side by side for a commit.
5. Lab — the logging VFS, adapted rather than built: SQLite's own `vfstrace`, as shipped in the CLI (the course doc's lab allows "build or adapt"). Experiments 7.1 and 7.2.
6. Evidence and failure cases — what a sync does and does not promise; the difference between a process crash and a power cut, stated here and demonstrated in 9.2.
7. Platform implications — I/O cost per commit by mode, multiplied by thousands of databases on one disk; which files appear and disappear and when; what a network or object-store filesystem would have to honor (sync, locks, shared memory) and why the course's platform keeps files local instead; when a platform would write its own VFS.
8. Knowledge check.

**Experiments.**

**7.1 vfs-calls-to-syscalls** · EXACT · placed at step 3 and 4 · [probed]
- Claim: every byte SQLite reads or writes goes through a VFS method, and each method call maps to a specific function in `os_unix.c` and a specific syscall.
- Does: runs the CLI with `-vfstrace` under strace so that VFS calls and syscalls land in one ordered stream: open a database, set WAL mode, create a table, commit one insert, checkpoint, close.
- Evidence: the stored call list; each `xWrite` paired with a `pwrite64` of the same file, offset and length; `xSync` paired with `fsync` or `fdatasync`; the directory fsync that only strace shows; the `-wal` and `-shm` files created at first use and deleted at last close.
- Astra will try: the same statements through the Go driver under strace (the twin check); a killed process leaving `-wal` and `-shm` behind and the next open cleaning up; the documented quirk that vfstrace prints `FULL` for a `NORMAL` sync, checked against the real flag.

**7.2 io-cost-by-pattern** · EXACT · placed at step 5 · [probed for per-commit sync counts]
- Claim: the same 1,000 rows cost very different I/O depending on batching, journal mode and `synchronous`, and the differences are countable. In WAL mode `NORMAL` does not sync on commit at all and `FULL` syncs the WAL once per commit.
- Does: writes the rows as one transaction, as 1,000 transactions, and in batches of 50; in `DELETE` and `WAL` modes; at `NORMAL` and `FULL`; counts writes, bytes written, syncs, and file creates and deletes from strace on the Go process.
- Evidence: a stored table of exact counts; syncs per transaction match the research measurements (rollback 3 at `NORMAL`, 4 at `FULL`; WAL 0 and 1); the chapter derives each number from the code path.
- Astra will try: recompute every cell from the CLI under strace; `synchronous=OFF`; check counts are identical across 20 runs.
- Why kept: this is the cost model for a disk shared by thousands of databases, and the zero in the WAL/`NORMAL` cell is the fact that experiment 9.2 builds on. A current production book states that cell wrongly, which is a reason to measure it in front of the reader.

**Feeds the through-line.** No rung. The chapter supplies the instrument the later chapters
use (the syscall record, and the kill at a chosen syscall) and the per-commit cost that the
durability choice in chapters 9 and 10 trades against.

---

### Chapter 8 — Connection and database lifecycle

Directory `ch08-connection-and-database-lifecycle/`, file `ch08-connection-and-database-lifecycle.md`.

**Question.** What does it cost to open, hold open, and close a database, and what state is a
database in after its last connection closes?

**Reading path:**

1. https://www.sqlite.org/c3ref/open.html and `/c3ref/close.html`
2. `main.c` `openDatabase` (3360–3736)
3. `btree.c` `sqlite3BtreeOpen` (2532–2825)
4. `prepare.c` `sqlite3Init`, `sqlite3ReadSchema` (438–533); `sqlite3InitOne` (199–427)
5. `main.c` 1266–1486 — close
6. `status.c` 203–421 — what `sqlite3_db_status` counts
7. `os_unix.c` 2101–2109 — descriptor parking
8. `wal.c` `sqlite3WalClose` — checkpoint and WAL deletion at last close

**Copied:** `main.c`, `prepare.c`, `status.c`. **Linked:** `btree.c`, `os_unix.c`, `wal.c`.

**Sections.**

1. Model — a connection owns a schema cache, a page cache, prepared statements and descriptors; none of it is shared across connections.
2. Question — open 1,000 database files and query each once.
3. Call path — open, first statement (schema load), close. Experiment 8.1.
4. Timeline — descriptors and memory over a connection's life. Experiment 8.2.
5. Lab — open and query hundreds or thousands of files under a bounded connection manager (the course doc's lab). Experiment 8.3.
6. Evidence and failure cases — a connection that does not close cleanly; a reader that keeps the WAL alive.
7. Platform implications — the numbers a capacity plan needs: descriptors and bytes per idle database, cost of a cold open, what eviction must do before it may copy or move a file; which settings a rebuilt file carries and which the platform must reapply. One paragraph, from the documentation and not demonstrated: `ATTACH` is limited (10 by default) and a transaction across attached WAL-mode files is atomic per file only, so the platform should not use it to span databases.
8. Knowledge check.

**Experiments.**

**8.1 open-cost-and-where-settings-live** · EXACT · placed at step 3 · [probed]
- Claim: opening a database reads only the 100-byte header; the schema is read and parsed at the first statement, at a cost that grows with schema size. Some settings are stored in the file and survive a rebuild; the rest belong to the connection and are gone at every open.
- Does: counts reads (strace) during open and during the first statement, for schemas of 1, 100 and 1,000 tables; reads `SQLITE_DBSTATUS_SCHEMA_USED` after each. Then sets every setting the course uses, closes, copies the file, opens the copy and reads each setting back.
- Evidence: one 100-byte read at open; page-1 and schema-page reads at first statement; schema memory rises with table count; a two-column table of settings: in the file (`page_size`, `journal_mode=WAL`, `auto_vacuum`, `encoding`, `user_version`, `application_id`) versus per connection (`synchronous`, `foreign_keys`, `cache_size`, `busy_timeout`, `wal_autocheckpoint`, `secure_delete`).
- Astra will try: a missing file with and without the create flag; a file that is not a database (the error arrives at first statement, not at open).
- Why kept: the second half is what a platform must store outside the file to remake a database that behaves the same. "Production SQLite" reports forgotten per-connection settings as a recurring production failure; here it is a table the reader produces.

**8.2 idle-cost** · EXACT for descriptors, BOUND for memory · placed at step 4 · [probed]
- Claim: an idle connection holds 1 descriptor in rollback mode (2 inside a write transaction) and 3 in WAL mode; its memory is schema + page cache + statements, each separately measurable, and the cache part can be released without closing.
- Does: counts `/proc/self/fd` entries per state; for *N* idle connections reads `CACHE_USED`, `SCHEMA_USED`, `STMT_USED` and process memory; applies `PRAGMA shrink_memory`.
- Evidence: the counts 1 / 2 / 3; the components sum to within a stated margin of the growth in process memory; the cache component drops to near zero after release while schema and statements stay.
- Astra will try: 1,000 WAL databases against the process descriptor limit; large schemas; two connections to one file in one process, where a closed connection's descriptor stays parked until the other releases its locks.

**8.3 bounded-manager (rung R5)** · BOUND, with EXACT hashes · placed at step 5 · [probed for the close behavior; the manager is new]
- Claim: a manager that keeps at most *K* connections open serves any number of database files with bounded descriptors and memory; and a WAL database whose last connection has closed is a single, fully checkpointed file that a plain copy captures exactly.
- Does: creates 1,000 small recipe databases; serves a seeded stream of queries and writes through an LRU manager capped at *K*; samples descriptors and memory; on each eviction closes the connection, checks no `-wal` or `-shm` remains, records the file's sha256; on each reopen checks the hash first. Runs the same stream unbounded for contrast.
- Captures / rebuilds: capture = the file at rest after last close; rebuild = copy.
- Evidence: descriptors ≤ 3*K* + a stated constant and memory ≤ *K* × per-connection budget + constant throughout; every reopen hash matches; every database's final content equals the recipe's expected state; cold-open versus warm latency reported as SHAPE.
- Astra will try: kill the manager mid-stream and reopen everything; a reader left open so close does not checkpoint (the `-wal` remains — the manager must detect this and not treat the file as at rest); *K* = 1 and *K* above the descriptor limit.

**Feeds the through-line.** R5: the cheapest correct capture there is, and the precise
condition under which it is correct.

---

### Chapter 9 — Backup, crash recovery, and integrity

Directory `ch09-backup-crash-recovery-integrity/`, file `ch09-backup-crash-recovery-integrity.md`.

**Question.** Which ways of saving a live database give a copy that can be trusted, what
survives a process crash and what can a power cut take, and how is a restored database proven
to be the right one?

**Reading path:**

1. https://www.sqlite.org/backup.html, `/howtocorrupt.html`, `/atomiccommit.html`
2. `wal.c` `walIndexRecover` (1390–1614)
3. `backup.c` `sqlite3_backup_step` (323–591); update and restart (688–734)
4. `vacuum.c` `sqlite3RunVacuum` (143–427) — the `INTO` branch
5. `btree.c` 11144–11295 — integrity check
6. `wal.c` 2199–2395 again, for how a reader's mark limits a checkpoint
7. `pager.c` 3658–3704 — sync flags by `synchronous` level

**Copied:** `backup.c`, `vacuum.c`. **Linked:** `pager.c`, `wal.c`, `btree.c`.

**Sections.**

1. Model — three families of capture (physical pages, logical content, logs) and two families of failure (process crash, power loss).
2. Question — save a database that is being written, crash it, restore it, prove it.
3. Call path — backup step and restart; `VACUUM INTO`; WAL recovery on open. Experiment 9.1.
4. Timeline — a backup racing a writer; a kill at each sync of a commit.
5. Lab — process-crash recovery and a comparison of backup approaches (the course doc's lab). Experiments 9.1 and 9.2.
6. Evidence and failure cases — Experiments 9.3 and 9.4.
7. Platform implications — choose a capture method by the equality level and the concurrency it gives; state the durability model in terms of sync, not of "committed"; a machine losing power takes the unsynced tail of every database on it at once, so the acknowledgement rule (after local sync, or after the store has it) is a platform decision; integrity checks do not prove content.
8. Knowledge check.

**Experiments.**

**9.1 capture-comparison** · EXACT · placed at step 3 and 5 · [probed on a quiet database; in part under a writer]
- Claim: each capture method reaches a specific equality level, and only some are safe while a writer is active. The backup API starts over when another connection writes, so under a steady writer it may never finish.
- Does: runs five captures of the same database — closed-file copy, backup API, `VACUUM INTO`, page read in a read transaction, file plus WAL under a held transaction — first on a quiet database, then with a writer child committing recipe steps. Steps the backup a few pages at a time with scripted writes from the same connection and from another, recording the remaining-page count.
- Captures / rebuilds: each method's own.
- Evidence: one table — method × (E1, E2, E3, safe under writer, blocks writer); it matches section 4.2; every capture taken under the writer equals some exact recipe state *k*; the backup's remaining count falls steadily with same-connection writes and jumps back up with other-connection writes.
- Astra will try: catch any method returning a state that is not a recipe state; the research observation that `.backup` sometimes failed with "database is locked" under writers — reproduce it, find the cause, and have the chapter state it.

**9.2 crash-versus-power-loss** · EXACT · placed at step 5 · [probed for the process-kill half; the model half is unprobed]
- Claim: after a process kill at any point, the reopened database equals exactly one recipe state at or after the last commit the writer acknowledged, at every `synchronous` level, because completed writes survive in the kernel. A power cut is different: only synced writes are guaranteed. In WAL mode at `NORMAL` that means the most recent acknowledged commits can be lost while the database stays consistent; at `FULL` none are.
- Does: (a) a writer child runs recipe steps in WAL mode, reporting each acknowledged step; strace kills it on entry to sync or write number *i*; the parent reopens, runs `integrity_check`, and matches the state against the recipe's list. Sweeps *i* over several commits at `OFF`, `NORMAL` and `FULL`. (b) The model: from the strace record of an uninterrupted run, find the WAL bytes written after the WAL's last sync; make a copy of the database and WAL with those bytes removed, which is the simplest thing a power cut can leave; open the copy and identify its state.
- Evidence: (a) for every *i*: integrity ok; recovered step ≥ last acknowledged step; never a state outside the list. (b) at `NORMAL` the recovered step is earlier than the last acknowledged step, by exactly the commits since the last sync; at `FULL` it equals the last acknowledged step; integrity ok in both.
- Astra will try: real `kill -9` at random times, thousands of iterations; a kill during a checkpoint; a kill during recovery itself; in the model, cutting the WAL in the middle of a frame.
- Scope, stated in the chapter: the model shows one outcome of a power cut, the one where nothing unsynced survives. It does not simulate torn or reordered writes or disks that lie about syncs. How SQLite defends against those (frame checksums, salts) is read in `wal.c`, not tested. This is the whole of the course's power-loss material, per Nick's direction 4.

**9.3 integrity-check-limits** · EXACT · placed at step 6 · [unprobed]
- Claim: `PRAGMA integrity_check` verifies structure, not content: it catches a damaged page header or an index that disagrees with its table, but not a changed byte inside a row's payload. A page-hash manifest catches all of them.
- Does: flips one byte in turn in a B-tree page header, in an unindexed column's payload, and in a freelist leaf; runs `integrity_check`, `quick_check`, and the chapter 2 manifest verifier on each.
- Evidence: a table of corruption site × detected by which check; the manifest detects all three; `integrity_check` misses the unindexed payload and the freelist leaf.
- Astra will try: a seeded sweep over random byte offsets, reporting the fraction each check detects.
- Why kept: "the restore passed `integrity_check`" is the usual proof offered for a rebuilt database, and it is not one. The ladder's hashes are.

**9.4 point-in-time-rebuild (rung R6)** · EXACT · placed at step 6 · [unprobed as a whole; every mechanism it relies on is probed]
- Claim: one snapshot plus the WAL frames written after it, captured continuously and across checkpoints, can rebuild the database as of any commit; the rebuild is byte-identical to the recipe's state for that commit.
- Does: introduces `internal/walship`. A capture process takes a base snapshot, disables automatic checkpoints, holds a read mark so the WAL cannot restart underneath it, copies newly committed frames from the WAL file into numbered segments (each recording first and last commit, salts, and the page-set hash before and after), and performs checkpoints itself only after shipping. A writer child runs recipe steps throughout. Restore picks a target step, applies segments in order with the chapter 5 replayer, and verifies. The capture process takes a fresh snapshot every *m* segments, so a restore never replays more than *m*.
- Captures / rebuilds: capture = snapshot + ordered segments + per-segment hashes; rebuild = snapshot, then segments up to the target.
- Evidence: for every step *k* in the run, restore-to-*k* has sha256 equal to the recipe's checkpointed state *k*; restoring *k* from the newest snapshot before it and from the oldest give the same bytes; a missing or reordered segment is detected by the hash chain before any page is applied.
- Astra will try: kill the capture process at every point and resume it; let an outside connection checkpoint behind its back (must be detected through the salt change and force a new snapshot); corrupt a segment; restore to a step inside a multi-statement transaction (must be impossible — only commit points exist).

**Feeds the through-line.** R6: the complete answer to "save the state so the database can be
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
4. `os_unix.c` 1189–1342 — why locks cannot serve as cross-machine ownership
5. https://www.sqlite.org/useovernet.html and `/howtocorrupt.html` section 2
6. Prior art, read for design only: Litestream "How it works" and the LTX file format (sources in the research report)

**Copied:** `wal.c`. **Linked:** `os_unix.c`.

**The service** (`ch10-…/service/`, about 1,200–1,500 lines of Go, built on the earlier
`internal/` packages):

- **Router and connection manager** — the 8.3 manager: at most *K* open databases, LRU eviction, at-rest hash on close.
- **Ownership** — a control-plane catalog (itself one SQLite database) mapping database id → owner node, epoch, location, last shipped commit, schema version, and the per-connection settings from 8.1. A node writes a database only while it holds the current epoch; the store rejects segments from older epochs.
- **Capture** — `internal/walship` per open database: snapshot, segments, shipper-owned checkpoints.
- **Store** — a local directory standing in for object storage, behind a small interface (put-if-absent, get, list) with injectable faults: failed put, partial put, delayed visibility, crash before or after put.
- **Relocation** — fence the source (bump epoch), ship the final segment, rebuild at the destination, verify the hash, activate, retire the source.
- **Durability model, stated in the chapter:** a write is acknowledged either after local commit at `synchronous=FULL` ("local-durable") or after its segment is in the store ("store-durable"); each invariant test names the mode it assumes. What a power cut does to each mode is stated from experiment 9.2, not re-simulated.

What the chapter keeps separate, as the course doc requires: SQLite's local guarantees
(atomic commit, snapshot isolation, single writer per file on one machine), the coordination
the service adds (epochs, fencing), and what the store must provide (atomic put, read after
write). The stand-in store models these; the chapter does not claim real object-storage
durability.

Bounded descriptors and memory (a capstone invariant) are checked as a standing assertion
inside every experiment in this chapter, using the bounds from 8.3, with 2,000 databases.

**Sections.**

1. Model — the three layers of guarantee and the component diagram.
2. Question — the six capstone invariants, each restated as a checkable property.
3. Call path — the SQLite mechanisms each component depends on, by reference to earlier chapters.
4. Timeline — a relocation, step by step, with the state of both nodes and the store at each step.
5. Lab — build and run the service. Experiment 10.1.
6. Evidence and failure cases — fault injection. Experiments 10.2–10.4.
7. Platform implications — answers to the course doc's eight design questions, each tied to the experiment that supports it; the per-database signals worth exporting (WAL size, checkpoint result, last shipped commit, at-rest hash); a list of what this design does not handle.
8. Knowledge check.

**Experiments.**

**10.1 one-writer** · EXACT · placed at step 5 · [unprobed]
- Claim (invariant: at most one authorized writer per database): with epochs checked by the store, a node that lost ownership cannot get another commit accepted, whatever the interleaving.
- Does: a deterministic state-machine test: two node models, the catalog and the store are stepped by a seeded scheduler through every interleaving of claim, write, ship, fence and crash up to a bounded depth; then the stale-owner scenario on two real service processes.
- Evidence: zero interleavings in which two epochs' segments are both accepted for the same commit range; the count of interleavings explored; the real-process run agrees with the model.
- Astra will try: remove the store's epoch check and confirm the test finds the violation (the test must be able to fail); a node that crashes and comes back believing it is still the owner.

**10.2 acknowledged-writes-survive** · EXACT · placed at step 6 · [unprobed]
- Claim (invariants: no acknowledged durable write is lost under the stated model; a database can be reopened after process failure): after any process kill, every database reopens, and each holds at least every write acknowledged under its mode.
- Does: clients log acknowledgements; the service process is killed at chosen syscall indices and at random times; on restart each database's state is matched to the recipe and compared with the acknowledgement log. In store-durable mode the local files are deleted before restart, so recovery is from the store alone.
- Evidence: recovered step ≥ last acknowledged step for every database, in both modes; integrity ok.
- Astra will try: kills during eviction, during snapshot, during catalog updates; acknowledging before the segment put completes (must be caught as a violation of store-durable mode).

**10.3 interrupted-relocation** · EXACT · placed at step 6 · [unprobed]
- Claim (invariant: an interrupted migration cannot silently create two authoritative copies): with a crash or store fault injected at every step of a relocation, the catalog names exactly one owner at all times, any write accepted during the move ends up in exactly one lineage, and the destination's rebuilt file equals the source at the fence point.
- Does: runs the relocation with a fault at each numbered step in turn, with a client writing throughout; after each run resumes or rolls back the relocation and checks ownership, the acknowledgement log, and file hashes.
- Evidence: for every fault point: one owner; destination sha256 = source sha256 at the fenced commit; no acknowledged write missing; no write present that was refused.
- Astra will try: skip the fence step and show two writers appear (the test must be able to fail); an interrupted upload left in the store; relocating a database with an open long reader.

**10.4 rebuild-the-fleet (rung R7)** · EXACT · placed at step 6 · [unprobed]
- Claim (invariant: every published snapshot is internally consistent): the store plus the catalog is a complete saved state of the whole fleet. A new node with empty disks can rebuild every database, each byte-identical to the checkpointed original, and every restore point published along the way rebuilds to a recipe state.
- Does: runs a write stream over 2,000 databases while a checker process lists the store and rebuilds every published restore point into a scratch directory. Partway through, a schema change is rolled out as one more recipe step, database by database, and stopped halfway. Then: quiesce, record each database's sha256 after a `TRUNCATE` checkpoint, delete every local file, start a fresh node, rebuild all databases from the store, compare.
- Captures / rebuilds: capture = everything in the store; rebuild = every database, from nothing.
- Evidence: 2,000 of 2,000 hashes equal; zero restore points that fail to rebuild; every rebuilt database has the schema version the catalog records for it, old or new; rebuild time and bytes read reported as SHAPE.
- Astra will try: delete or corrupt one segment of one database and confirm exactly that database is reported as unrecoverable past the gap, with the last good restore point named; inject partial puts and confirm they are never listed as restore points; rebuild while the old node is still running and confirm fencing stops the old node.

**Feeds the through-line.** R7. The ladder ends with the whole platform rebuilt from saved
state, with the invariants tested under faults rather than asserted.

---

## 6. Phase 2 build order and chunking

Nothing here starts until Nick signs off on this outline and Twin relays it.

**Seats for this project (Nick's ruling of 2026-11-20,
scoped to this repo only and in force unchanged).**

- **Opus** writes the Go code: the logic, with no prose in it.
- **Astra** writes the adversarial test scripts.
- **Gemini 4 Neon** writes all prose: every chapter's markdown text and every comment in the
  Go experiment code. Neon also reviews Opus's code; in this project the Opus review seat is
  Neon, not Fable.
- **Fable** is the orchestrator: briefs, acceptance, landing. Fable checks that no non-Neon
  prose reaches a chapter or a code comment.
- **Neon access:** primary route is the OpenRouter API, model `google/gemini-4-neon`, key
  `OPENROUTER_API_KEY` from the build machine's secrets. Fallback is the
  direct Gemini API key (billing enabled
  2026-11-20; verified working). OpenCode 1.19.6 is installed and defaults to Neon; agents
  using it source both secrets files first. Not through Antigravity. Every script that calls
  Neon is saved under `agent_work/neon/` and indexed.

| Step | Who | Output |
| --- | --- | --- |
| Build | Fresh Opus agent, own worktree, brief as a file | Experiment code and shared package code without comments, captured evidence, a plain-data notes file for the prose pass (what each function does, what each run printed), `agent_work/chNN/` working scripts |
| Adversarial test | Fresh Astra agent | Scripts in `agent_work/chNN/` that try to falsify each experiment's claim (the "Astra will try" lines above are its starting list), plus a findings report |
| Repair | Fresh Opus agent, one round | Fixes for Astra's findings, or a correction to the claim |
| Code review | Neon | PASS / NITS / FAIL on Opus's code against the experiment's claim and evidence |
| Prose pass | Neon | The chapter `.md` (with inlined code, run commands, evidence, source spans) and every comment in that chapter's Go code |
| Acceptance gate | Fable (orchestrator) | PASS / NITS / FAIL against the checklist below; lands on PASS |
| Standoff or second FAIL | Muse Wonder Full | Final verdict, per the standing protocol |

**Acceptance checklist for every chapter** (the gate before merge to main):

1. Pin block present; `SOURCES.sha256` verifies against the pinned tree.
2. Every cited `file:start-end` is re-checked by script: the named function is at those lines.
3. Every experiment runs from a clean checkout with the printed command; EXACT evidence matches on two consecutive runs; BOUND and SHAPE evidence holds on three.
4. Inlined code equals the files on disk.
5. Every **[unprobed]** claim is now either confirmed with evidence or corrected in the text; anything still inferred is labelled as inference.
6. Astra's findings are each fixed or answered; its scripts are in `agent_work/` and indexed.
7. All eight sections present; five knowledge-check questions; platform implications tied to evidence.
8. **Prose provenance.** The Go code with comments stripped is identical to what Opus built and Neon reviewed (checked by script), so the Neon pass changed comments only; the chapter text is Neon's output as saved by the calling script; nothing else edited either.
9. Every borderline keep carries its "Why kept" reason in the chapter.
10. `STATE.md` and `agent_work/INDEX.md` updated in the same commits.

**Chunks.** Sizes are rough: lines of Go (shared packages included) and separate agent runs.
There is no C.

| Chunk | Contents | Depends on | Est. Go lines | Runs (build / Astra / repair / Neon review / Neon prose / gate) |
| --- | --- | --- | --- | --- |
| 0 Foundation | `go.mod` with the pinned driver; `internal/sqlitex`; `internal/trace` (strace runner, kill-at-syscall, CLI `-vfstrace` runner); `sqlite/build-tools.sh`; chapter template; repo checks (source copies, inlined code, span checker, comment-strip comparison); Neon calling scripts | — | 700 + 300 shell | 1 / 1 / 1 / 1 / 1 (comments only) / 1 |
| 1 | Chapter 1, `internal/recipe` | 0 | 600 | 1 / 1 / 1 / 1 / 1 / 1 |
| 2 | Chapter 2, `internal/dbfile`, `internal/equal` (E1) | 1 | 900 | 1 / 1 / 1 / 1 / 1 / 1 |
| 3 | Chapter 3, `internal/equal` (E2, E3) | 2 | 600 | 1 / 1 / 1 / 1 / 1 / 1 |
| 4 | Chapter 4 | 2 | 400 | 1 / 1 / 1 / 1 / 1 / 1 |
| 5 | Chapter 5, `internal/walfile` | 2 | 900 | 1 / 1 / 1 / 1 / 1 / 1 |
| 7 | Chapter 7 | 1 | 300 | 1 / 1 / 1 / 1 / 1 / 1 |
| 6 | Chapter 6, `internal/procs` | 5 | 700 | 1 / 1 / 1 / 1 / 1 / 1 |
| 8 | Chapter 8 | 4, 5 | 600 | 1 / 1 / 1 / 1 / 1 / 1 |
| 9 | Chapter 9, `internal/walship` | 5, 6 | 900 | 1 / 1 / 1 / 1 / 1 / 1 |
| 10a | Service core: manager, catalog, store with faults, the ownership model test, 10.1 | 9 | 1,100 | 1 / 1 / 1 / 1 / — / — |
| 10b | Relocation, fault experiments 10.2–10.4, chapter 10 text | 10a | 900 | 1 / 1 / 1 / 1 / 1 / 1 |

Totals: about 8,600 lines of Go and no C (revision 1: about 13,000 and 500), and at most 70
agent runs if every repair round is used (12 builds, 12 Astra passes, up to 12 repairs, 12
Neon reviews, 11 Neon prose passes, 11 gates). Revision 1 had 15 chunks; the added runs per
chunk are the Neon review the ruling requires.

**Waves** (chunks inside a wave touch disjoint directories and packages and run in parallel):

1. Chunk 0
2. Chunk 1
3. Chunk 2
4. Chunks 3, 4, 5, 7
5. Chunks 6, 8
6. Chunk 9
7. Chunks 10a, then 10b

**Run rules.** One fresh agent per run, headless, brief as a file, own worktree, exit file and
log kept outside the repo. Chains continue without a human turn (build → Astra → repair → Neon
review → Neon prose → gate); a failed run is diagnosed and re-dispatched once, and a second
failure stops that chain and is reported. `STATE.md` is updated at every step. Usage gauges
for the Claude, Codex and Muse pools, and the remaining OpenRouter budget for Neon, are
checked before each wave.

**Risks carried into Phase 2**, in order of how much they could change the plan:

1. The twin check (1.2) is unprobed. If the translated driver's I/O differs from the C build's for some statement, the chapters that count syscalls take their counts from the CLI and say so. If it differs widely, the fallback is the mattn driver (open question 14).
2. The power-loss model in 9.2 and the live-snapshot-under-writer check in 6.4 are unprobed. Both follow documented formats and behavior; each chunk confirms its claim before any text is written.
3. Exact evidence (opcode listings, byte counts) is tied to one driver version. Chunk 0 freezes it in `go.mod` and `sqlite/PIN`; changing it later invalidates stored evidence.
4. Neon writes all prose through an API with a monthly budget ($44.26 of $50 remained on the OpenRouter key on 2026-11-20; it resets 2026-12-01). Eleven prose passes over chapters with inlined code are large calls. Chunk 0 measures the cost of one pass before wave 2 is dispatched.
5. Chapter 10 is the largest and least constrained piece. It is split in two and its model test (10.1) is written before the fault experiments so the invariants exist as code first.

---

## 7. Open questions and decisions for Nick

**Answered by Nick's direction of 2026-11-20** (numbers are revision 1's):

- **3. Scale.** Trim: yes. 55 experiments became 32. Every cut is listed in OUTLINE-CHANGES.md.
- **4. cgo.** Existing libraries only. The outline now needs no cgo and no C at all (section 4.1).
- **5. Committing the generated amalgamation.** No longer applies: nothing compiles it.
- **6. Power loss.** Understanding level only. One model in experiment 9.2, the sync counts in 7.2, and the platform implications. No simulator, no block-device tooling.
- **2. Who writes the prose.** Answered by the Neon ruling of the same day: Neon writes all prose and code comments and reviews Opus's code (section 6).

**Still open.** Each has a recommendation; the outline above assumes the recommendation.

1. **GitHub remote, or local only?** The repo is local with no remote. Recommendation: stay local until chunk 0 and chapter 1 have passed the gate, then decide. Nothing is pushed without your word.
7. **Duplicate source copies.** Read literally, spec 5 puts `btree.c` in two chapter directories, `wal.c` in two, `os_unix.c` in two. Recommendation: real copies as specified, checked against the pin by script. Alternative: one copy plus symlinks.
8. **Shared packages versus self-contained chapters.** Later experiments reuse earlier code (the page reader, the WAL replayer). Recommendation: shared `internal/` packages, each inlined in full once in the chapter that introduces it.
9. **Capstone scope.** Two service processes on this one machine, and a local directory with injected faults standing in for object storage; no real S3, no real network. Is that the right boundary, or do you want a real S3-compatible store (for example MinIO) in the loop?
10. **Linux only.** Experiments use `/proc`, strace and POSIX lock behavior. Recommendation: state Linux as a requirement.
11. **A second SQLite version.** Revision 1 proposed an optional experiment building the recipe with 3.54.1. Recommendation now: not an experiment. It is one adversarial case in 1.3 and one sentence in chapter 2 (the header records the writer's version, so byte hashes are only stable within one library version; across an upgrade the platform compares at E2 or E3).
12. **Knowledge checks and the tutor.** Recommendation: each chapter ends with five questions, answers in a separate file. Should this course also be packaged later for the `tutor` CLI? Phase 2 as outlined produces plain markdown only.
13. **Where Astra's scripts are shown to readers.** Recommendation: each chapter's "failure cases" section names the scripts that attacked it and what they found, with links, without inlining them.

**New in this revision.**

14. **The driver.** The outline uses modernc (a translation of the pinned C to Go, no compiler needed) because it is the only stock driver with everything the kept experiments use. If you would rather have the real C running inside the Go process, mattn is the alternative; it needs gcc and loses the per-connection memory counters. Recommendation: modernc, with the twin check in 1.2 as the safeguard.
15. **Rollback-journal depth.** One experiment remains (4.1: SQLite's own recovery from a hot journal). The Go journal parser and replayer are cut. Say so if you want the replayer back.
16. **No custom VFS.** Chapter 7 reads and traces the VFS and does not build one. If your platform plans include a VFS of your own (for example one backed by object storage), say so: that would add one experiment back, and it is the one place where a small custom piece or a second driver would be needed.
17. **Comments in Astra's scripts.** The Neon ruling covers chapter text and comments in the Go experiment code. The outline assumes Astra's scripts under `agent_work/` keep Astra's own comments, since they are working files and not course text.

**Operational note, not a design question.** Every headless Claude run on 2026-11-20 printed:
"this sign-in ends between 2026-11-25 and 2026-11-28". Phase 2 will run past that window.
Someone needs to run `claude auth login` (or `claude setup-token`) on this VM before then.
Usage gauges could not be read from a headless session in Phase 1; they need a look in the TUI
before the first Phase 2 wave.

---

## 8. Phase 2 rulings (Nick, 2026-11-20)

These govern the build. Where an earlier section says otherwise, this section wins.

| # | Ruling | Effect on the outline |
| --- | --- | --- |
| 1 | The repo has an upstream, `github.com/nickstrad/sqlite-internals` (public). Each chunk merges to `main` and is pushed once its review and tests pass. | Open question 1 closed. |
| 2 | Services run in Docker. The capstone's object store is **SeaweedFS** (S3-compatible) from `compose.yaml`. | Replaces the "local directory standing in for object storage" in chapter 10 and open question 9. Fault injection moves into a Go wrapper around the S3 client; the store itself is real. |
| 3 | Shared packages across chapters. | Open question 8 closed as recommended. |
| 4 | Cross-platform: the platform targets Linux, the repo must also build and run on macOS. Linux-only tooling (strace, `/proc`) is build-tag gated and labelled, with the `lab` Docker service as the way to run it from a Mac. Every chunk is verified with a darwin cross-build and vet. | Replaces open question 10 ("Linux only"). |
| 5 | No second SQLite version. | Open question 11 dropped; the "newest driver release" adversarial case in 1.3 is dropped with it. |
| 6 | Knowledge is kept in two places and kept in sync: the `kb` knowledge store on the build machine, and this repo (`docs/knowledge/`, with the `kb` tool vendored whole under `kb/`). | Adds a knowledge step to every chunk landing. |
| 7 | Code and scripts are Go. Working scripts (probes, one-off checks) live in `agent_work/` with the index current, in Go or bash. | `sqlite/build-tools.sh` and the repo check scripts of section 6 become the Go tool `cmd/course`. |
| 8 | Driver: `modernc.org/sqlite`. No cgo, no custom C. | Open question 14 closed as recommended. |
| 9 | Rollback-journal material stays deep enough to be understood: experiment 4.1 runs at full depth. | Open question 15: no Go journal replayer, but 4.1 is not a sketch. |
| 10 | Working assumption: platform databases run in WAL mode unless that does not make sense. Each chapter evaluates this and records any exception. | The recipe and every "platform database" default to WAL; chapter 4 is the rollback-mode chapter by subject. |
| 11 | If a custom VFS is viable on this stack, chapter 7 gets a real VFS treatment with experiments, sized by the minimal-knowledge rule. **Verdict (probe `agent_work/probes/vfs-viability`, 2026-11-20): viable.** A writable forwarding VFS can be registered with the translated library in pure Go (about 300 lines, no cgo), and the driver's own `vfs` package serves a read-only database from any Go `fs.FS`. | Replaces section 4.3's "no chapter builds a VFS" and open question 16. Chapter 7 keeps 7.1 and 7.2 and adds **7.3 go-vfs-shim** (a Go forwarding VFS, `internal/govfs`: per-commit VFS call counts in-process, and a deterministic fault at the N-th `xWrite`/`xSync` with SQLite's recovery shown in both journal modes) and **7.4 read-only-snapshot-vfs** (query a checkpointed snapshot through an `fs.FS` that reads byte ranges from the S3 store; requests per query; why `immutable=1` and a checkpointed file are required). Experiment count becomes 34. |
| 12 | Seats: **Gemini 4 Neon** writes all prose (chapter text and Go comments), reviews Opus's code, and designs and writes the adversarial scripts and tests. **Astra is not used.** Opus writes the code. Fable orchestrates, runs the adversarial tests, and does final acceptance. | Every "Astra will try" line in section 5 is Neon's starting list. The chunk table's Astra column is a Neon run. Per chunk: Opus build → Neon prose → Neon code review → one Opus repair round if needed → Neon re-review → Neon adversarial scripts, run by Fable → acceptance → merge and push. |
| 13 | Open questions 7, 12 and 13 take the outline's own recommendations: real source copies checked against the pin; five knowledge-check questions per chapter with answers in a separate file (no tutor packaging in Phase 2); each chapter's failure-cases section names the adversarial scripts that attacked it and what they found, with links. | Closed. |
| A, B | The repo is public and has GitHub Pages. A final publishing chunk, after chapter 10, adds an Actions workflow that renders the chapters into a navigable site at `https://nickstrad.github.io/sqlite-internals/`. | New last chunk. |
| C | The book and all reader-facing docs stand alone in this repo: links resolve inside the repo or its Pages site, and nothing a reader sees refers to the build machine. | The pinned source tree is found through `$SQLITE_SRC` or `_src/`, fetched by `go run ./cmd/course fetch-source`. A sweep of the tree runs before the publishing chunk lands. |

Chunk list as built: 0 (foundation and infrastructure), 1, 2, then 3, 4, 5, 7, then 6, 8, then
9, 10a, 10b, then publishing.
