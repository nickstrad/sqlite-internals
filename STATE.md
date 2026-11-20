# STATE — sqlite-internals

Goal: build Nick's "SQLite Internals — AI-Guided Source Study Course" (spec:
docs/course-source.md) as an experiment-driven course: per-chapter markdown with
inlined, runnable Go experiments proving how SQLite puts changes into durable
storage deterministically — centered on saving a database's state so the database
can be rebuilt — with Opus building the Go code and Astra adversarially testing it.
Plan: OUTLINE.md.

Status: Phase 1 (outline) COMPLETE 2026-11-20. Waiting for Nick's sign-off on OUTLINE.md.
HARD GATE: no Phase 2 work (no builders, no chapter text, no experiment code) until Twin
relays Nick's ruling in a follow-up brief file.

Done:
- Spec saved to docs/course-source.md; repo created (local git, no remote) — 97f4a47.
- SQLite pin fixed: tag `version-3.53.4` = git `b09c88c1…` = Fossil `bf7c7f30…` = the
  installed sqlite3 CLI's source id. Pinned tree at /root/Software/sqlite-src/3.53.4
  (re-create with agent_work/fetch-pinned-sqlite.sh) — f9ad78b.
- Four Opus research reports merged into docs/knowledge/ with their probes under
  agent_work/probes/ (all indexed in agent_work/INDEX.md): source-inventory-ch01-04,
  source-inventory-ch05-09, determinism-and-rebuild, go-access-and-vfs.
- OUTLINE.md written and committed: pin, repo layout, the Rebuild Ladder through-line,
  Go access / determinism / VFS decisions, 10 chapters with 55 numbered experiments,
  Phase 2 chunks and waves, 13 open questions for Nick. Commit SHA is in
  /root/runs/sqlite-outline-done.txt.

In progress: nothing. No agents are running.

Next steps (only after sign-off):
1. Apply any edits from Nick's ruling to OUTLINE.md.
2. Check all three usage gauges, then dispatch Chunk 0 (foundation) per OUTLINE.md section 6.
3. Continue wave by wave; update this file at every step.

Open decisions / blockers:
- Nick: sign-off on OUTLINE.md, and the 13 questions in its section 7 (GitHub remote or
  local-only; who writes chapter prose; experiment count; cgo; committing the amalgamation;
  power-loss method; and others).
- Nick or Twin: the Claude sign-in on this VM ends between 2026-11-25 and 2026-11-28
  (warning printed by every headless run). Renew with `claude auth login` before Phase 2
  runs past that date.
- Usage gauges were not readable from the headless Phase 1 session; check in the TUI before
  the first Phase 2 wave.

Resume: read this file, then OUTLINE.md and /root/briefs/sqlite-internals-orchestrator-mission.md;
`git log` for what has landed. Orchestrator progress log: /root/runs/sqlite-orchestrator-progress.txt.
Research briefs: /root/briefs/sqlite-research-*.md; their logs and exit files:
/root/runs/sqlite-research-*. Research worktrees and branches were removed after merge.
