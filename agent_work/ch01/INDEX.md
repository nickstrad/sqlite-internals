# agent_work/ch01 — chunk 1 working files

| Path | Purpose | What it proved |
| --- | --- | --- |
| `agent_work/ch01/recipe-probe/main.go` | Builds the default recipe in WAL and rollback mode and compares the canonical build (one session per step) with builds on one long-lived connection and one session per step under six connection settings (recipe options, wal_autocheckpoint 0 and 1, cache_size 5, synchronous NORMAL, secure_delete OFF). Run: `go run ./agent_work/ch01/recipe-probe` | In WAL mode a long-lived connection differs from per-step sessions only in the change counter (bytes 24–27, 92–95); rollback mode does not; with secure_delete=ON checkpoint timing, cache size and synchronous do not change the at-rest bytes; secure_delete=OFF does. Led to `recipe.Build` using one session per step |
| `agent_work/ch01/build-notes.md` | Plain-data notes for the prose pass | — |
| `agent_work/ch01/BUILD-REPORT.md` | Chunk 1 build report | — |
