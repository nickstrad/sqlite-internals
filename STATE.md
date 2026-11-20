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

Where each open chunk stands (worktrees `../sqlite-internals-work/<name>`, branches `chunk/<name>`):

| Chunk | Opus build | Neon prose | Neon review 1 | Neon adversarial | Opus repair | Neon re-review |
| --- | --- | --- | --- | --- | --- | --- |
| 7 | done | done (comments + chapter) | FAIL, done | done; runs 1 and 2 recorded | done (5449623) | **waiting for Neon** |
| 4 | done | done (comments + chapter) | FAIL, done | **partial**: 3 attack programs, no run-all.sh/README | done (075ed33) | waiting for Neon |
| 5 | done | **partial**: comments done, chapter text not written | FAIL, done | files complete, author run cut before its done marker; run 1 recorded | done (208d5ad) | waiting for Neon |
| 3 | done (fa19f6a) | **not started** | not started | not started | – | – |
| pub (Pages site generator + workflow) | done (5bf7893); not on main, workflow not triggered | README/home text not written | not started | not started | – | – |
| 6 | building (base: repaired chapter 5) | | | | | |
| 8 | building (base: repaired chapters 4 + 5 merged) | | | | | |
| 9, 10a, 10b | to build after 6 and 8 (Opus, not blocked) | | | | | |

In progress: Opus builds of chapters 6 and 8. Then 9 (needs 6), 10a (needs 8, 9), 10b.

Next steps when Neon credit is back, in this order (scripts in `agent_work/orch/`;
`agent_work/neon/neon-run.sh` picks the route itself: OpenRouter if it has budget, else the
direct key):
1. Chapter 7: `rereview.sh ch07 …` (same arguments as the brief `ch07-rereview` on the build
   machine) → `finalize.sh ch07 ch07-vfs-and-filesystem-io` → `land.sh`.
2. Chapter 4: finish the adversarial brief (run-all.sh, README, remaining attacks), run it,
   then `rereview.sh` → `finalize.sh` → `land.sh`.
3. Chapter 5: chapter-text pass (a brief like `ch07-prose2`: comments are done, write the
   chapter only), then `rereview.sh` → `finalize.sh` → `land.sh`.
4. Chapter 3: `neon-trio.sh` → `post-neon.sh` → `repair.sh` → `rereview.sh` → `finalize.sh` → `land.sh`.
5. The same full chain for 6, 8, 9, 10a (no chapter text), 10b, then pub (Neon writes
   README.md as the book's home page; review; adversarial on the site checker), the
   self-contained sweep, landing pub (this publishes the site), and
   the done file.
Branches built on unlanded chapters must merge main before their repair rounds.

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
