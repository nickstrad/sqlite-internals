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
    sqlitec/      cgo binding to the pinned amalgamation (+ vfstrace, fault VFS)   built in chunk 0
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

GO_ACCESS_PLACEHOLDER

### 4.2 Determinism, and how a rebuilt database is verified

**Enforced in `internal/recipe` and `internal/sqlitec`, for every experiment that makes a
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

VFS_PLACEHOLDER

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

CHAPTERS_PLACEHOLDER
