# STATE — sqlite-internals

Goal: build Nick's "SQLite Internals — AI-Guided Source Study Course" (spec:
docs/course-source.md) as an experiment-driven course: per-chapter markdown with
inlined, runnable Go experiments. North Star (Nick, 2026-11-20): understand SQLite
deterministically, well enough to build platforms holding thousands of SQLite files that
can be remade as needed; state capture and deterministic rebuild is the through-line.
Plan: OUTLINE.md (revision 2). What changed from revision 1: OUTLINE-CHANGES.md.

Status: **Phase 2 (build) IN PROGRESS** since 2026-11-20. Neon answered again at 08:00 UTC on 2026-11-21 and the 10b chain was resumed (the note below describes the stop at 07:27; the spending cap may bind again). Chunk 0, chapters 1–9 and the capstone
service core (10a) have landed. **BLOCKED on Neon since 2026-11-21 07:27 UTC**: the direct Gemini
route answers "Your project is temporarily rate-limited to prevent exceeding its monthly
spending cap" and OpenRouter is empty until 2026-12-01 (see blockers). Everything left needs
Neon: 10b review, attacks, re-review and the chapter 10 text; the publishing chunk's README and
comments.

Rulings applied (details: OUTLINE.md section 8):
- Upstream github.com/nickstrad/sqlite-internals (public); every chunk lands on main and is pushed.
- Docker for services; SeaweedFS is the capstone's S3 store. Shared `internal/` packages.
- Cross-platform: must build and run on macOS; Linux-only tooling gated, `lab` Docker service
  as the Mac route; darwin cross-build + vet verify every chunk.
- No second SQLite version (future note: a version-upgrade experiment could be added later).
- Knowledge in two stores kept in sync: vm-1 `kb`, and this repo (`docs/knowledge/` indexed by
  the vendored `kb/`, location `.kb/`). `agent_work/kb-sync.sh`.
- Go for all code and scripts; working scripts in `agent_work/` (Go or bash), indexed.
- Driver modernc.org/sqlite v1.60.1, no cgo/C. Rollback experiment 4.1 at full depth.
- WAL is the platform default; per-chapter evaluation:
  - Chapter 3: WAL holds. All experiment databases are WAL except 3.1 part D and 3.3 part A2
    (rollback mode shown as the contrast). WAL frames serve as the independent record of which
    pages a write changed.
  - Chapter 4: 4.1 is rollback mode by subject (the planned exception, ruling 9). The platform
    stays WAL; 4.1 shows the two places a WAL platform still meets rollback mode: files created
    or imported without the WAL switch, and the switch commit itself. 4.2 runs in WAL.
  - Chapter 3: WAL holds. All experiment databases are WAL except 3.1 part D and 3.3 part A2
    (rollback mode shown as the contrast). WAL frames serve as the independent record of which
    pages a write changed.
  - Chapter 4: 4.1 is rollback mode by subject (the planned exception, ruling 9). The platform
    stays WAL; 4.1 shows the two places a WAL platform still meets rollback mode: files created
    or imported without the WAL switch, and the switch commit itself. 4.2 runs in WAL.
  - Chapter 5: WAL holds as the default. What it obliges a platform to own (each tied to an
    experiment in the chapter): run checkpoints itself, bound reader lifetime, treat `-wal` as
    part of the database while any connection is open, never ship `-shm`, watch WAL size,
    detect WAL generations by salt change. Exception from the source (not tested): no WAL on
    network filesystems (shared memory needed).
  - Chapter 6: WAL holds and the evidence favours it for concurrency; rollback mode appears
    only as the contrast (6.1 A, 6.2 D, 6.3 B). Caveat recorded in the chapter:
    `locking_mode=EXCLUSIVE` breaks "the writer never blocks readers".
  - Chapter 7: WAL holds. Care points: a custom VFS must implement the shared-memory methods
    (io_methods version 2) or `journal_mode=wal` silently stays `delete`.
  - Chapter 9: WAL holds; no experiment runs in rollback mode. WAL allows live capture
    without stopping the writer (9.1) and segment shipping (9.4). Recorded conditions: at
    `synchronous=NORMAL` a power cut loses the commits since the last checkpoint (FULL loses
    none); `VACUUM INTO` of a WAL database yields a rollback-mode file.
  - Chapter 8: WAL holds, with measured costs and conditions: 3 descriptors per idle connection
    instead of 1, and a closed WAL database is NOT a single file at rest in five cases (another
    process has it open, a statement left open, a killed writer, persist-WAL, a rename while
    open) — the chapter's `CheckAtRest` is the rule before any copy.
- VFS viability verdict: **VIABLE** (probe `agent_work/probes/vfs-viability/RESULT.md`, chunk 0): a
  writable Go forwarding VFS registers with modernc in pure Go; read-only `fs.FS` VFS also works.
  Chapter 7 gains experiments 7.3 (Go VFS shim: call counts + fault injection) and 7.4
  (read-only snapshot VFS over S3 range reads). Risk noted: relies on ccgo's function-pointer
  convention; that package needs `go vet -unsafeptr=false`.
- S3 client for the capstone: aws-sdk-go-v2 (probe `agent_work/probes/seaweedfs-s3/RESULT.md`).
- Seats: Opus writes code only. **Neon makes ALL prose — the chapter text and the Go code
  comments** — and reviews Opus's code and writes the adversarial tests. Fable orchestrates,
  runs the adversarial tests, accepts. Astra not used.
- Pipeline (Nick, 2026-11-20 prose-ordering and closing-rewrite notes). Every chapter's FINAL
  step before landing is Neon's unreviewed closing prose; no chapter lands without it.
  - Late ordering (chapters 3, 5, 6, 8, 9, 10a, 10b): Opus build → Neon review + Neon
    adversarial → Opus repair → Neon re-review → **Neon prose once, at the end (chapter text +
    comments on the finished code)** → accept → land. Scripts: `agent_work/orch/stageA.sh`
    then `stageB.sh … late`.
  - Old ordering (chapters 1, 2, 7, and 4 whose prose ran before review): they close with a
    separate **Neon closing rewrite** (`stageB.sh … closing`). Chapters 1 and 2 landed without
    it and get it retroactively as follow-up commits on main.
- Docker fallback (Nick, 2026-11-20): where no generic approach works on both systems, the
  experiment runs in the Docker lab and the chapter presents Linux behaviour first. Uses: (none
  recorded yet; chapter 7's macOS delFlag mismatch got a generic fix in its repair).
- Open questions 7, 12, 13: outline recommendations applied (real source copies checked against
  the pin; five questions per chapter with answers in a separate file, no tutor packaging;
  failure-cases sections link the adversarial scripts).
- Addendum: final publishing chunk (GitHub Pages via Actions,
  https://nickstrad.github.io/sqlite-internals/); self-contained rule (no build-machine
  references in anything a reader sees; sweep before publishing lands).

Done:
- Phase 1 and 1b: research reports, probes, OUTLINE.md revision 2 — a7bb640.
- Phase 2 setup: kb vendored under `kb/` (upstream SHA in `agent_work/kb-vendor.sha`), in-repo
  kb location `.kb/`, orchestration and Neon scripts (`agent_work/orch`, `agent_work/neon`),
  rulings recorded in OUTLINE.md section 8.

Chunk status. Pipeline per chunk: Opus build → Neon review + Neon adversarial → one Opus
repair → Neon re-review → Neon closing prose → acceptance gate → merge and push. Chunks 0, 1, 2,
4 and 7 ran the older order (prose before review) and closed with a Neon closing rewrite.

| Chunk | State | Review | Adversarial | Landed |
| --- | --- | --- | --- | --- |
| 0 foundation | done | Neon FAIL → repair → PASS | 38 held / 0 broken | merged cf58cb8 |
| 1 chapter 1 | done | Neon FAIL → one Opus repair → PASS | 48 held / 0 broken after repair (run 1: 11 broken; claims narrowed) | merged 2dcf743 |
| 2 chapter 2 | done | Neon FAIL → one Opus repair → PASS | 36 held / 0 broken after repair (run 1: 14 broken) | merged 68afd03 |
| 3 chapter 3 | done | Neon NITS → one Opus repair → PASS | 30 held / 0 broken after repair (run 1: 14 broken) | merged 4e1e290 |
| 4 chapter 4 | done | Neon FAIL → one Opus repair → NITS | 32 held / 0 broken after repair (run 1: 12 broken) | merged f500664 |
| 5 chapter 5 | done | Neon FAIL → one Opus repair → PASS | 28 held / 0 broken after repair (run 1: 7 broken) | merged 2629fec |
| 6 chapter 6 | done | Neon FAIL → one Opus repair → PASS | 49 held / 0 broken after repair (run 1: 6 broken) | merged e4af9e0 |
| 7 chapter 7 | done | Neon FAIL → one Opus repair → PASS | 34 held / 0 broken after repair | merged 751e198 |
| 8 chapter 8 | done | Neon FAIL → one Opus repair → PASS | 29 held / 0 broken after repair (run 1: 9 broken) | merged 3f54929 |
| 9 chapter 9 | done | Neon FAIL → one Opus repair → NITS | 40 held / 0 broken after repair (run 1: 5 broken) | merged a64e85e |
| 10a capstone service core | done | Neon FAIL → one Opus repair → PASS | 27 held / 0 broken after repair (run 1: 9 broken) | merged 3b2ef92 |

Chapters 1 and 2 also received their retroactive Neon closing rewrites (66fca8e, dd5a9b4, merged).
A separate small fix (`equal.LogicalHash` always ends its read transaction) is merged, with a
Neon review PASS (`agent_work/fixeq/`).

Open chunks (worktrees `../sqlite-internals-work/<name>`, branches `chunk/<name>`; chain logs
`sqlite-p2-<name>-chainA.log` / `-chainB.log` in the run directory; a finished part A leaves
`sqlite-p2-<name>.stageA`, a finished part B `sqlite-p2-<name>-accept.txt`):

| Chunk | State (2026-11-21 06:10 UTC) |
| --- | --- |
| 10b | built (Opus, 04c6ff9, main merged in); part A stopped at its first step: Neon review not written, Neon adversarial suite partly written and committed as partial. Resume: `agent_work/orch/stageA.sh ch10b …` from `review` (same arguments as in `sqlite-p2-ch10b-chainA.log`'s launch, see the progress file), then `stageB.sh ch10b … late` |
| pub | review 1 FAIL → repair → review 2 FAIL (new blocker in `internal/site/check.go`) → arbitration by Muse Wonder Full: FAIL STANDS (`agent_work/pub/ARBITRATION.md` on the branch) → Opus repair round 2 scoped to the arbiter's checklist running, done, then Neon re-review 3: NITS, 48 held / 0 broken; part B (Neon writes README as the book home + comments) and landing come last, after the self-contained sweep |

Closing prose record (commit of each chapter's closing Neon pass): chapter 5 — 394203d (single late prose pass); chapter 7 — c645979 (closing rewrite); chapter 3 — e71fa8c (single late prose pass); chapter 4 — a9fff51 (closing rewrite); chapter 6 — d394e64, chapter 8 — dfd1dfd and chapter 9 — b58a1a0 (single late prose passes); 10a — 09037a8 (comments only; no chapter text in that chunk); chapter 3 — e71fa8c (single late prose pass); chapter 4 — a9fff51 (closing rewrite); chapter 6 — d394e64, chapter 8 — dfd1dfd and chapter 9 — b58a1a0 (single late prose passes); 10a — 09037a8 (comments only; no chapter text in that chunk); chapter 1 — 66fca8e and chapter 2 — dd5a9b4 (retroactive closing rewrites, merged on main).

Next steps, in order: 10b build → part A → part B (chapter 10 text and comments) → land; then pub part B (README as the book home, comments), the self-contained sweep, landing pub (this publishes the site), and the done file.

Open decisions / blockers:
- **Nick: Neon is stopped by the Gemini project's monthly spending cap** (reached 2026-11-21
  07:27 UTC; the prepay credit is not the limit, the cap is — it was $100 on 2026-11-20).
  Raise the project's spend cap in AI Studio (or raise the OpenRouter key limit); nothing else
  is needed. Still to run on Neon: for 10b a review, the rest of the attack suite, a re-review
  and the chapter 10 prose pass; for the publishing chunk the README/home page and comments —
  about 6 runs, roughly $15–25 at the measured rate. Per ruling 12 no other model may do this
  work, so it waits. `agent_work/neon/probe.sh` (with OpenCode on PATH) says when a route is back.
- Nick or Twin: the Claude sign-in on the build VM ends between 2026-11-25 and 2026-11-28.

Resume: read this file, OUTLINE.md (section 8 first), then `git log` and `git worktree list`.
Each run is launched with `agent_work/orch/launch.sh <name> <worktree> <opus|neon> <brief>`;
briefs are in /root/briefs/sqlite-p2/, logs and exit files are /root/runs/sqlite-p2-<name>.*
(a run is alive if its .pid process exists and there is no .exit file; relaunch the same
command if it died). Progress lines: /root/runs/sqlite-phase2-progress.txt.
