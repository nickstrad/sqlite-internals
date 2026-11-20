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

- 2026-11-20: SQLite pin fixed: tag `version-3.53.4` = git `b09c88c1…` = Fossil
  `bf7c7f30…` = the installed sqlite3 CLI's source id. Pinned tree at
  /root/Software/sqlite-src/3.53.4 (re-create with agent_work/fetch-pinned-sqlite.sh).
  Scaffold commits f9ad78b, 9f32f63.

In progress:
- Phase 1: Fable orchestrator producing OUTLINE.md (per-chapter reading paths,
  copied-source-file lists, numbered Go experiments, state-rebuild through-line,
  driver/determinism decisions, Phase 2 chunking, open questions for Nick).
- Four headless Opus research agents (dispatched 2026-11-20 16:54 UTC), each in a worktree
  /root/Software/sqlite-internals-work/research-<topic> on branch research/<topic>, brief
  /root/briefs/sqlite-research-<topic>.md, log/pid/exit under /root/runs/sqlite-research-<topic>.*:
  source-inventory-a (ch 1-4), source-inventory-b (ch 5-9), go-access-vfs, determinism-rebuild.
  Each commits docs/knowledge/<report>.md + agent_work/probes/<topic>/ on its branch; the
  orchestrator merges them, folds their INDEX rows into agent_work/INDEX.md, then writes OUTLINE.md.

Next steps:
- Nick signs off on OUTLINE.md (Twin relays), then Phase 2 build per the outline.

Open decisions / blockers:
- GitHub remote for this repo, or local-only (Nick's call, listed in the outline).
- All Phase 2 work gated on Nick's outline sign-off.

Resume: a research run is alive if /root/runs/sqlite-research-<topic>.exit is absent and the
pid in .pid is running; if it died without a report, relaunch once with
/root/runs/sqlite-research-launch.sh <topic> claude-opus-5-5 (failure context appended to the brief).
Then read this file, then OUTLINE.md and /root/briefs/sqlite-internals-orchestrator-mission.md;
git log for what has landed. Orchestrator progress: /root/runs/sqlite-orchestrator-progress.txt.
