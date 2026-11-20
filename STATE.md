# STATE — sqlite-internals

Goal: build Nick's "SQLite Internals — AI-Guided Source Study Course" (spec:
docs/course-source.md) as an experiment-driven course: per-chapter markdown with
inlined, runnable Go experiments proving how SQLite puts changes into durable
storage deterministically — centered on saving a database's state so the database
can be rebuilt — with Opus building the Go code and Astra adversarially testing it.

Status: Phase 1 (outline) — Fable orchestrator commissioned 2026-11-20.
HARD GATE: no chapter building until Nick signs off on OUTLINE.md.

Done:
- 2026-11-20: Twin read Nick's Google Doc (1Pu-RHTNmXu0ldjE7hPGbzxHmjXG3cn4tdBtKM58pa1A)
  and saved the faithful spec to docs/course-source.md; repo created (local git,
  no remote); orchestrator mission at /root/briefs/sqlite-internals-orchestrator-mission.md.

In progress:
- Phase 1: Fable orchestrator producing OUTLINE.md (per-chapter reading paths,
  copied-source-file lists, numbered Go experiments, state-rebuild through-line,
  driver/determinism decisions, Phase 2 chunking, open questions for Nick).

Next steps:
- Nick signs off on OUTLINE.md (Twin relays), then Phase 2 build per the outline.

Open decisions / blockers:
- GitHub remote for this repo, or local-only (Nick's call, listed in the outline).
- All Phase 2 work gated on Nick's outline sign-off.

Resume: read this file, then OUTLINE.md and /root/briefs/sqlite-internals-orchestrator-mission.md;
git log for what has landed. Orchestrator progress: /root/runs/sqlite-orchestrator-progress.txt.
