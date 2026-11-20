# agent_work/ch01 — chunk 1 working files

| Path | Purpose | What it proved |
| --- | --- | --- |
| `agent_work/ch01/recipe-probe/main.go` | Builds the default recipe in WAL and rollback mode and compares the canonical build (one session per step) with builds on one long-lived connection and one session per step under six connection settings (recipe options, wal_autocheckpoint 0 and 1, cache_size 5, synchronous NORMAL, secure_delete OFF). Run: `go run ./agent_work/ch01/recipe-probe` | In WAL mode a long-lived connection differs from per-step sessions only in the change counter (bytes 24–27, 92–95); rollback mode does not; with secure_delete=ON checkpoint timing, cache size and synchronous do not change the at-rest bytes; secure_delete=OFF does. Led to `recipe.Build` using one session per step |
| `agent_work/ch01/repair-probe/main.go` | Repair round 1 probes. Default: builds the WAL recipe with cache_size 0, 1, 10, -2000 three times each and compares with `StepHashes`. `counters`: prints the change counter after each step for cache_size -2000 and 0. `cli-order`: builds step 0 with the pinned CLI using the old and the new foundation pragma order. Run: `go run ./agent_work/ch01/repair-probe [counters\|cli-order]` | cache_size=0 moves the WAL counter from step 0 (deterministic); 1, 10, -2000 do not. The CLI matches the Go step 0 only with journal_mode before application_id, so adversarial DET-04 replays the pre-repair order |
| `agent_work/ch01/build-notes.md` | Plain-data notes for the prose pass | — |
| `agent_work/ch01/BUILD-REPORT.md` | Chunk 1 build report | — |
