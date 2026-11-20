# STATE — sqlite-internals

Goal: build Nick's "SQLite Internals — AI-Guided Source Study Course" (spec:
docs/course-source.md) as an experiment-driven course: per-chapter markdown with
inlined, runnable Go experiments. North Star (Nick, 2026-11-20): understand SQLite
deterministically, well enough to build platforms holding thousands of SQLite files that
can be remade as needed; state capture and deterministic rebuild is the through-line.
Plan: OUTLINE.md (revision 2). What changed from revision 1: OUTLINE-CHANGES.md.

Status: **Phase 2 (build) IN PROGRESS** since 2026-11-20. Nick signed off OUTLINE.md revision 2
with rulings (OUTLINE.md section 8). Chunks 0, 1 and 2 have landed. **BLOCKED on Neon credit since
2026-11-20 23:07 UTC** (see blockers): no prose, review or adversarial run can start. Opus-only
work continues.

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
- WAL is the platform default; per-chapter evaluation recorded here: (none yet).
- VFS viability verdict: **VIABLE** (probe `agent_work/probes/vfs-viability/RESULT.md`, chunk 0): a
  writable Go forwarding VFS registers with modernc in pure Go; read-only `fs.FS` VFS also works.
  Chapter 7 gains experiments 7.3 (Go VFS shim: call counts + fault injection) and 7.4
  (read-only snapshot VFS over S3 range reads). Risk noted: relies on ccgo's function-pointer
  convention; that package needs `go vet -unsafeptr=false`.
- S3 client for the capstone: aws-sdk-go-v2 (probe `agent_work/probes/seaweedfs-s3/RESULT.md`).
- Seats: Opus code; Neon all prose + code review + adversarial scripts; Fable orchestrates,
  runs the adversarial tests, accepts. Astra not used.
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

In progress (worktrees `../sqlite-internals-work/chNN`, branches `chunk/chNN`): chapter 7 — Opus
repair round, then Neon re-review (`agent_work/orch/rereview.sh`), `finalize.sh`, `land.sh`;
chapters 3, 4, 5 — Opus builds (branched from chapter 2's repaired branch; merge main before
their repair rounds), then `neon-trio.sh`, `post-neon.sh`, `repair.sh`, `rereview.sh`,
`finalize.sh`, `land.sh` each.

Next steps: after 5 lands, build 6 and 8 (8 also needs 4); then 9; 10a; 10b; publishing
(GitHub Pages workflow, README, self-contained sweep). Build briefs for every chunk are already
written (build machine: briefs directory `sqlite-p2/chunkNN-build.md`).

Open decisions / blockers:
- **Nick: Neon has no credit on either route.** OpenRouter key: $0.15 left of the $50 monthly
  limit, resets 2026-12-01. Direct Gemini key: "Your prepayment credits are depleted" (AI Studio
  prepay). Measured need: about $9–10 of Neon per chunk; 11 chunks still need Neon passes
  (3, 4, 5, 6, 7, 8, 9, 10a, 10b, publishing prose, plus finishing 4/5/7) — roughly $100–120.
  Either raise the OpenRouter key limit or top up the AI Studio prepay; nothing else is needed.
  Per ruling 12 no other model may write prose, review or adversarial tests, so those steps wait.
- Nick or Twin: the Claude sign-in on the build VM ends between 2026-11-25 and 2026-11-28.
- Neon budget: the OpenRouter key (shared with other projects) had $44.14 at Phase 2 start and
  was spent by the middle of chapter 7's first round (about $9 of Neon per chunk). Since then
  `agent_work/neon/neon-run.sh` runs Neon on the direct Gemini key (Nick's designated
  fallback); that spend is billed to Nick's Google project and is not visible from here.

Resume: read this file, OUTLINE.md (section 8 first), then `git log` and `git worktree list`.
Each run is launched with `agent_work/orch/launch.sh <name> <worktree> <opus|neon> <brief>`;
briefs are in /root/briefs/sqlite-p2/, logs and exit files are /root/runs/sqlite-p2-<name>.*
(a run is alive if its .pid process exists and there is no .exit file; relaunch the same
command if it died). Progress lines: /root/runs/sqlite-phase2-progress.txt.
