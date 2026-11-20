# agent_work index

Every intermediate or working script produced on this project lives in this directory and
has a row here. Nothing lives only in /tmp or a shell history.

| Script | Purpose | Chapter | Made by | What it proved |
| --- | --- | --- | --- | --- |
| `fetch-pinned-sqlite.sh` | Clone SQLite tag `version-3.53.4` and check git SHA, Fossil `manifest.uuid`, and the installed CLI's source id against the pin | all (pin) | Fable orchestrator, 2026-11-20 | Tag `version-3.53.4` is git `b09c88c1…`, Fossil `bf7c7f30…`, and the installed `sqlite3` 3.53.4 CLI was built from that same check-in |
| `research-launch.sh` | Launch one headless research agent (`claude -p`) in its own worktree/branch with brief `/root/briefs/sqlite-research-<topic>.md`; writes pid, log and exit file under `/root/runs/` | Phase 1 research | Fable orchestrator, 2026-11-20 | Launch mechanics only (copy of `/root/runs/sqlite-research-launch.sh`) |
