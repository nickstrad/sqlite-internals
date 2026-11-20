# Chunk 1 build report — Chapter 1: Architecture and execution path

Builder: Claude Opus 5.5, branch `chunk/ch01`, 2026-11-20. Details for the prose pass:
`agent_work/ch01/build-notes.md`.

## Built

| Item | Path | Commit |
| --- | --- | --- |
| Seeded deterministic workload generator + tests | `internal/recipe/` | 50aa0db |
| Copied sources, `SOURCES.sha256`, `SPANS.txt` (12 spans) | `ch01-architecture-and-execution-path/` | 3d14279 |
| Experiment 1.1 pin-check + evidence | `…/experiments/1.1-pin-check`, `…/evidence/1.1-pin-check.txt` | 4cd5836 |
| Experiment 1.2 one-insert (rollback and WAL, twin check, measured per-opcode call path) + evidence | `…/experiments/1.2-one-insert`, `…/evidence/1.2-one-insert.txt` | fceb251 |
| Experiment 1.3 recipe-rebuild (R0, five variants + a sixth source) + evidence | `…/experiments/1.3-recipe-rebuild`, `…/evidence/1.3-recipe-rebuild.txt` | f8a3f01 |
| Build notes, recipe probe, index | `agent_work/ch01/` | 37e01b6 |

Default recipe: seed 1, 30 steps, WAL; 270 pages; builds in about 1 s.

## Commands run

| Command | Result |
| --- | --- |
| `gofmt -l` on all chunk files | empty |
| `go build ./... && go vet ./... && go test ./...` | pass |
| `GOOS=darwin GOARCH=arm64 go build ./... && go vet ./...` (and darwin/amd64 vet) | pass |
| `bash agent_work/verify/crossbuild.sh` | `CROSSBUILD OK` |
| `go run ./cmd/course check all` (with `SQLITE_SRC` set) | sources PASS, spans PASS (12), evidence PASS for 1.1, 1.2, 1.3 (2 runs each identical to the evidence), gofmt PASS (38 files); paths FAIL only on the 9 pre-existing lines in OUTLINE.md and docs/knowledge (chunk 0 report lists them; not mine to edit) |
| `go run ./cmd/course check prose eab2401..HEAD` | PASS: 0 prose lines introduced |
| Each experiment with the `./configure` CLI (`SQLITE3_BIN=…/_tools/bin/sqlite3`) | EXACT output identical to the installed-CLI evidence |
| `docker compose run --rm lab` running all three experiments and `go test ./internal/recipe/` | all exit 0; EXACT output identical to the evidence; tests pass |

Runtimes: 1.1 about 1 s, 1.2 about 1.8 s, 1.3 about 9–11 s (two waits for a wall-clock second).
`_work/` cleaned afterwards.

## Corrections to the outline (details and evidence in build-notes.md)

1. In WAL mode the file change counter (bytes 24–27, 92–95) counts connection sessions, not
   commits. The recipe therefore defines its canonical build as one connection session per step,
   and `MatchStep` also reports matches that differ only there. A sixth source of variation,
   now shown as 1.3 variant 6.
2. Different transaction batches give identical files in WAL mode with `secure_delete=ON` (the
   recipe's setting). They differ with `secure_delete=OFF`, only in unused bytes inside b-tree
   pages, not freelist pages. They also differ in rollback mode, in the change counter.
3. An unordered `INSERT … SELECT` run twice gives identical files. It differs once the scan
   order changes (`reverse_unordered_selects`), and `ORDER BY` removes that.
4. `current_timestamp` has one-second resolution: two runs in the same second match.
5. Twin check: the Go driver makes one extra `fcntl(F_SETFD)` after each `O_CLOEXEC` open. This
   comes from musl's `open()` inside modernc's libc. All writes, syncs, reads, locks and unlinks
   match, as do the final file bytes.
6. "(page − 1) × 4096 = database-file write offset" holds in rollback mode only. In WAL mode the
   commit writes only to the `-wal` file. Both are shown.
7. The recipe always uses `secure_delete=ON`.
8. Span check: all outline spans were correct. `sqlite3_prepare_v2` (937–954) was added.

## Left undone

- No prose or comments (seat rule). Neon writes the chapter, the comments and the adversarial
  scripts.
- darwin: cross-built and cross-vetted only. The non-Linux path of 1.2 (portable parts plus the
  `LINUX-ONLY:` line) was never run.
