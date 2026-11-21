# Chapter 5 — Neon prose pass notes

Author: Gemini 4 Neon. Closing prose pass on the post-repair, post-adversarial state of chunk `ch05` (`ch05-wal-transactions-checkpoints`).

## What was written

### Deliverable A — Go code comments (comments only)

Updated stale comments from the pre-repair pass and added missing test comments across 9 files (comment-stripped code verified unchanged via `git diff`, `gofmt`, `go build ./...`, `go vet ./...`, `go test ./internal/walfile/...`, and `GOOS=darwin GOARCH=arm64 go build/vet`):

- `internal/walfile/errors.go` — updated `ErrBaseNotDatabase`, `ErrBaseSize`, and `ErrFrameNotIndexed` doc comments to cover invalid base page-size bytes, empty-WAL alignment checks, and torn wal-index lookups.
- `internal/walfile/replay.go` — updated `Replay` and `ReplayFiles` doc comments (and added a short WHY comment before base validation) to match pre-zero-commit base validation and `O_EXCL` preservation of foreign `.replay-tmp` files.
- `internal/walfile/shm.go` — updated `HeadersAgree` (raw 48-byte comparison including the 4-byte `unused` field, matching `walIndexTryHdr` at `src/wal.c:2619`) and `ParseIndex` (torn copy-1-zero / copy-2-valid header handling, plus inline WHY comment referencing `walIndexWriteHdr` at `src/wal.c:942-954`).
- `internal/walfile/showwal.go` — updated `CompareWithShowwal` doc comment to document bidirectional checksum discrepancy checks.
- `internal/walfile/livecopy.go` — updated `CopyLiveFiles` (empty pairs return `nil, nil` without spawning a child) and `ReadLiveState` (deferred temp-copy removal on every return path).
- `internal/walfile/repair_test.go` — added a one-line doc comment to each of the 7 repair tests (`TestParseIndexTornHeader`, `TestHeadersAgreeComparesUnusedField`, `TestReplayValidatesBaseWithoutCommits`, `TestReplayFilesKeepsForeignTempFile`, `TestCompareWithShowwalReportsWrongRejection`, `TestCopyLiveFilesWithNoPairs`, `TestReadLiveStateRemovesCopiesOnError`).
- `ch05-wal-transactions-checkpoints/experiments/5.2-wal-replay/main.go` — expanded the spill comment in `partBC` to explain why `UPDATE documents SET title = upper(title)` keeps all 19 spilled frames checksum-valid so stopping at `LastCommitFrame` on valid uncommitted frames is exercised.
- `ch05-wal-transactions-checkpoints/experiments/5.3-reader-pins-the-wal/main.go` — added a WHY comment in `partA` explaining why `writer` does not run `countQuery` before `openReader` and why `count(ctx, writer)` runs before `take(...)` inside the loop.
- `ch05-wal-transactions-checkpoints/experiments/5.4-checkpoint-modes/main.go` — removed stale `"busy-timeout waits"` from the package doc comment (elapsed durations appear on `INFO:` lines only).

All other files under `internal/walfile/` (`checksum.go`, `diff.go`, `wal.go`, `live_test.go`, `walfile_test.go`) and `experiments/5.1-wal-inspector/main.go` already met Deliverable A standards and were left untouched.

### Deliverable B — Chapter markdown and answers

- `ch05-wal-transactions-checkpoints/ch05-wal-transactions-checkpoints.md`:
  - Pin block matching `sqlite/PIN` and "How to run the experiments" callout matching Chapters 1 and 2 (noting nothing in Chapter 5 is Linux-only, how to run natively or via `docker compose run --rm lab`, and that evidence was measured on Linux while macOS was cross-built/vetted only).
  - All 8 required sections (`## 1. Model` through `## 8. Knowledge check`), with text diagrams, verified `SPANS.txt` source citations and GitHub links at `version-3.53.4`, version notes from `docs/knowledge/source-inventory-ch05-09.md`, WAL-default evaluation and platform obligations table, and the **Why kept** line on Experiment 5.4.
  - All 8 outline corrections from `agent_work/ch05/build-notes.md` and `REPAIR-REPORT.md` incorporated cleanly (per-connection `pWal->nCkpt` restart salts, `RESTART` waiting for the next writer, `WAL_READ_LOCK(0)` blocking all backfill while allowing restart, Experiment 5.3 `EXACT` throughout, read marks outliving readers, raw `base + WAL` unable to detect stale-generation bases, `walDecodeFrame` 8-vs-16-byte comment discrepancy, and `pager.c:607-617` reversed sync-flag comment).
  - `internal/walfile` (all 8 non-test `.go` files) inlined once in Section 5.1; all 4 experiments and their 4 evidence files inlined via `<!-- include: ... -->` (`16` include blocks verified with `go run ./cmd/course inline` and `inline -check`).
  - Section 6 ends with `### What attacked this chapter`, opening with `<!-- adversarial-results -->` and summarizing all 4 attack scripts (`28` attacks), Run 1 (`21 HELD, 7 BROKEN`), the 3 code fixes (`INS-05b`, `REP-07a`, `REP-07b`), Run 2 (`24 HELD, 4 BROKEN`), the 4 narrowed claims / clarified checks (`REP-02`, `REP-05`, `CHK-03`, `CHK-05`), and the final run (`28 HELD, 0 BROKEN`).
- `ch05-wal-transactions-checkpoints/answers.md` — full answers to all five knowledge-check questions.

## Verification commands run

- `go run ./cmd/course inline ch05-wal-transactions-checkpoints/ch05-wal-transactions-checkpoints.md` — PASS (`16` include blocks rewritten)
- `go run ./cmd/course inline -check ch05-wal-transactions-checkpoints/ch05-wal-transactions-checkpoints.md` — PASS (`16` include blocks match)
- `go run ./cmd/course check sources ch05` — PASS (`2` copied files match pin)
- `go run ./cmd/course check spans ch05` — PASS (`28` spans verified)
- `go run ./cmd/course check evidence ch05` — PASS (5.1, 5.2, 5.3, 5.4: 2 runs each identical to evidence)
- `go run ./cmd/course check gofmt` — PASS (`102` Go files formatted); `gofmt -l internal/walfile ch05-wal-transactions-checkpoints` printed nothing
- `go build ./... && go vet ./... && go test ./internal/walfile/...` — PASS
- `GOOS=darwin GOARCH=arm64 go build ./... && GOOS=darwin GOARCH=arm64 go vet ./...` — PASS
- Grep for machine-specific home/root path prefixes across `ch05-wal-transactions-checkpoints/` and `internal/walfile/` — 0 matches
- Relative markdown link check on `ch05-wal-transactions-checkpoints/ch05-wal-transactions-checkpoints.md` — 0 missing targets

## Open doubts

- None. Every claim, number, and span in the chapter matches the final captured evidence files, adversarial results, and pinned SQLite 3.53.4 source.

## Suspected defects

- None in `internal/walfile` or Experiments 5.1–5.4.
