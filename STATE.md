# STATE — sqlite-internals

Goal: build Nick's "SQLite Internals — AI-Guided Source Study Course" (spec:
docs/course-source.md) as an experiment-driven course: per-chapter markdown with
inlined, runnable Go experiments. North Star (Nick, 2026-11-20): understand SQLite
deterministically, well enough to build platforms holding thousands of SQLite files that
can be remade as needed; state capture and deterministic rebuild is the through-line.
Plan: OUTLINE.md (revision 2). What changed from revision 1: OUTLINE-CHANGES.md.

Status: Phase 1b (outline revision) COMPLETE 2026-11-20. Waiting for Nick's sign-off on the
revised OUTLINE.md.
HARD GATE: no Phase 2 work (no builders, no chapter text, no experiment code) until Twin
relays Nick's sign-off in a follow-up brief file.

Done:
- Spec saved to docs/course-source.md; repo created (local git, no remote) — 97f4a47.
- SQLite pin fixed: tag `version-3.53.4` = git `b09c88c1…` = Fossil `bf7c7f30…` = the
  installed sqlite3 CLI's source id. Pinned tree at /root/Software/sqlite-src/3.53.4
  (re-create with agent_work/fetch-pinned-sqlite.sh) — f9ad78b.
- Four Opus research reports merged into docs/knowledge/ with their probes under
  agent_work/probes/ (indexed in agent_work/INDEX.md).
- OUTLINE.md revision 1 (55 experiments, first-party cgo) — 7924ef7.
- Phase 1b, under Nick's direction of 2026-11-20: OUTLINE.md revision 2 — 32 experiments on an
  8-rung Rebuild Ladder; Go access through existing libraries only (modernc.org/sqlite
  v1.60.1, the pinned CLI's -vfstrace, strace), no cgo and no C; power loss at understanding
  level (one model in experiment 9.2); both reference books mined; Neon seat ruling recorded in
  section 6; Phase 2 in 12 chunks. Two probes back the library decision:
  agent_work/probes/outline-v2, written up in docs/knowledge/library-route-probes.md.
  Commit SHA is in /root/runs/sqlite-outline-v2-done.txt.

In progress: nothing. No agents are running.

Next steps (only after sign-off):
1. Apply any edits from Nick's ruling to OUTLINE.md.
2. Check the Claude, Codex and Muse usage gauges and the OpenRouter budget for Neon, then
   dispatch Chunk 0 (foundation) per OUTLINE.md section 6.
3. Continue wave by wave; update this file at every step.

Open decisions / blockers:
- Nick: sign-off on OUTLINE.md revision 2, and the open questions in its section 7 (also
  listed at the end of OUTLINE-CHANGES.md): GitHub remote; source copies; shared packages;
  capstone store; Linux only; second SQLite version; tutor packaging; where Astra's scripts
  are shown; and four new ones — the driver (modernc recommended), rollback-journal depth,
  no custom VFS, comments in Astra's scripts.
- Nick or Twin: the Claude sign-in on this VM ends between 2026-11-25 and 2026-11-28
  (warning printed by every headless run). Renew with `claude auth login` before Phase 2
  runs past that date.
- Usage gauges were not readable from the headless sessions; check in the TUI before the
  first Phase 2 wave.
- Neon runs on an OpenRouter key with a $50 monthly limit ($44.26 left on 2026-11-20, resets
  2026-12-01); measure one prose pass in chunk 0 before dispatching wave 2.

Resume: read this file, then OUTLINE.md, OUTLINE-CHANGES.md and
/root/briefs/sqlite-neon-ruling.md; `git log` for what has landed. Progress logs:
/root/runs/sqlite-orchestrator-progress.txt (Phase 1) and
/root/runs/sqlite-revision-progress.txt (Phase 1b). Research briefs:
/root/briefs/sqlite-research-*.md; their logs and exit files: /root/runs/sqlite-research-*.
Reference books (read-only): /root/briefs/refdocs/. No worktrees or branches are open.
