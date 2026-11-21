# STATE — sqlite-internals

Goal: build Nick's "SQLite Internals — AI-Guided Source Study Course" (spec:
docs/course-source.md) as an experiment-driven course: per-chapter markdown with
inlined, runnable Go experiments. North Star (Nick, 2026-11-20): understand SQLite
deterministically, well enough to build platforms holding thousands of SQLite files that
can be remade as needed; state capture and deterministic rebuild is the through-line.
Plan: OUTLINE.md (revision 2). What changed from revision 1: OUTLINE-CHANGES.md.

Status: **Phase 2 (build) IN PROGRESS** since 2026-11-20. Nick signed off OUTLINE.md revision 2
with rulings (OUTLINE.md section 8). Chunks 0, 1 and 2 have landed. Neon credit was restored
2026-11-20 23:30 UTC (direct Gemini route; OpenRouter stays empty until 2026-12-01) and all Neon
stages are running again since 2026-11-21 00:45 UTC.

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
  - Chapter 4: 4.1 is rollback mode by subject (the planned exception, ruling 9). The platform
    stays WAL; 4.1 shows the two places a WAL platform still meets rollback mode: files created
    or imported without the WAL switch, and the switch commit itself. 4.2 runs in WAL.
  - Chapter 5: WAL holds as the default. What it obliges a platform to own (each tied to an
    experiment in the chapter): run checkpoints itself, bound reader lifetime, treat `-wal` as
    part of the database while any connection is open, never ship `-shm`, watch WAL size,
    detect WAL generations by salt change. Exception from the source (not tested): no WAL on
    network filesystems (shared memory needed).
  - Chapter 7: WAL holds. Care points: a custom VFS must implement the shared-memory methods
    (io_methods version 2) or `journal_mode=wal` silently stays `delete`.
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

Chunk status (build → Neon prose → Neon review → repair → re-review → adversarial → accept → land):

| Chunk | State | Review | Adversarial | Landed |
| --- | --- | --- | --- | --- |
| 0 foundation | done | Neon FAIL → repair → PASS | 38 held / 0 broken | merged cf58cb8 |
| 1 chapter 1 | done | Neon FAIL → one Opus repair → PASS | 48 held / 0 broken after repair (run 1: 11 broken; claims narrowed) | merged 2dcf743 |
| 2 chapter 2 | done | Neon FAIL → one Opus repair → PASS | 36 held / 0 broken after repair (run 1: 14 broken) | merged 68afd03 |
| 2, 3, 4, 5, 7, 6, 8, 9, 10a, 10b, publishing | not started | | | |

Where each open chunk stands (worktrees `../sqlite-internals-work/<name>`, branches
`chunk/<name>`; chain logs `sqlite-p2-<name>-chainA.log` / `-chainB.log` in the run directory;
a finished part A leaves `sqlite-p2-<name>.stageA`, a finished part B `sqlite-p2-<name>-accept.txt`):

| Chunk | State (2026-11-21 01:45 UTC) |
| --- | --- |
| 3 | part A: review NITS, run 1 16 held / 14 broken; Opus repair running |
| 6 | part A: review FAIL, run 1 43 held / 6 broken; Opus repair running |
| 8 | part A running (review + adversarial) |
| 9 | built (e825529, base = 6 + 3 merged); part A not started |
| 10a, 10b | to build after 8 and 9 finish part A |
| pub | site generator + workflow built on `chunk/pub` (5bf7893), not on main; README/home text, review, adversarial still to do; lands last after the self-contained sweep |
| 1 closing rewrite | done (66fca8e, merged) |
| 2 closing rewrite | running in the ch02 worktree (stageB closing) |

Closing prose record (commit of each chapter's closing Neon pass): chapter 5 — 394203d (single late prose pass); chapter 7 — c645979 (closing rewrite); chapter 4 — a9fff51 (closing rewrite); chapter 1 — 66fca8e (retroactive closing rewrite, merged on main).

Next steps, in order: land 4, 3, 6, 8 as their chains finish (each branch merges main before
its part B); run part A for 6 and 8; closing rewrites for 1 and 2; build 10a and 10b; chain 9,
10a, 10b; then pub, the self-contained sweep, and the done file.

Open decisions / blockers:
- Neon spend: the direct Gemini route has $125 prepay (added 2026-11-20) under a $100 monthly
  spend cap of which $23.33 was used. At about $9–10 per chunk the remaining chunks need roughly
  $100, so the cap (about $77 left) may bind before the build ends. Owner: Nick (raise the cap
  if Neon starts failing again). The balance is not visible from the build machine.
- Nick or Twin: the Claude sign-in on the build VM ends between 2026-11-25 and 2026-11-28.

Resume: read this file, OUTLINE.md (section 8 first), then `git log` and `git worktree list`.
Each run is launched with `agent_work/orch/launch.sh <name> <worktree> <opus|neon> <brief>`;
briefs are in /root/briefs/sqlite-p2/, logs and exit files are /root/runs/sqlite-p2-<name>.*
(a run is alive if its .pid process exists and there is no .exit file; relaunch the same
command if it died). Progress lines: /root/runs/sqlite-phase2-progress.txt.
