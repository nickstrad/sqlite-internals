# Neon prose pass notes — ch02 (Chapter 2 — Database file format and pages)

Author: Gemini 4 Neon, 2026-11-20.

## What was written

### Deliverable A — Go code comments (comments only)
Added package doc comments, Go-convention doc comments on every exported identifier, WHY/SQLite-source comments on non-obvious logic, and one-line test comments on every test/fuzz function across all 20 `.go` files introduced in chunk 2:

- `internal/dbfile/`: `errors.go`, `varint.go`, `header.go`, `file.go`, `btree.go`, `payload.go`, `record.go`, `tree.go`, `census.go`, `dbfile_test.go`
- `internal/equal/`: `equal.go`, `manifest.go`, `equal_test.go`
- `internal/capture/`: `capture.go`, `capture_test.go` (added in commit `33670d5` as the third chunk-2 shared package; commented so no non-Neon prose or undocumented exported identifiers remain)
- `ch02-file-format-and-pages/experiments/`:
  - `2.1-header-and-what-a-commit-changes/main.go`
  - `2.2-page-census/main.go`, `usage_unix.go`, `usage_other.go`
  - `2.3-page-manifest/main.go`

Verification:
- `gofmt -l` on all four trees: clean (no files listed); `go run ./cmd/course check gofmt`: PASS (76 files).
- Comment-stripped code comparison against `HEAD`: all 20 modified `.go` files identical outside non-directive comments.
- `go build` and `go vet` across `./internal/... ./cmd/... ./ch01-architecture-and-execution-path/... ./ch02-file-format-and-pages/...`: clean. (`go build ./...` hits untracked `agent_work/ch02/adversarial/header-falsify/main.go` already present in the worktree, untouched by this pass.)
- `go test ./internal/dbfile ./internal/equal ./internal/capture`: PASS.

### Deliverable B — Chapter markdown and knowledge-check answers
- `ch02-file-format-and-pages/ch02-file-format-and-pages.md`: complete 8-section chapter with pin block, run instructions, model diagram, WAL-default evaluation (ruling 10), source call path covering all 18 spans in `SPANS.txt`, version notes, Experiments 2.1, 2.2 (with outline **Why kept** line), and 2.3 (rung R1), all three new shared packages (`internal/dbfile`, `internal/equal`, `internal/capture`) inlined file by file with plain-words walk-throughs, failure-case analysis including `### What attacked this chapter` (`<!-- adversarial-results -->`), evidence-backed platform implications, and 5 knowledge-check questions.
- `ch02-file-format-and-pages/answers.md`: answers to all 5 knowledge-check questions grounded in exact source lines and evidence output.
- `go run ./cmd/course inline ch02-file-format-and-pages/ch02-file-format-and-pages.md` and `inline -check`: PASS (20 include blocks match their files).
- Self-containment check: zero `/root` paths or Leanpub citations in any chapter or Go file.

## Open doubts

1. **`internal/capture` scope in the brief.** The brief named `internal/dbfile` and `internal/equal` from the outline, while Opus extracted `internal/capture` in commit `33670d5` as a third shared package introduced by Chapter 2 (for rung R1, reused by R4 and R6). Following `OUTLINE.md` section 2 ("Shared packages are inlined once, in the chapter that introduces them") and the Neon prose rule, I commented `internal/capture/` and inlined `internal/capture/capture.go` in section 5.3.
2. **Linked `pager.c` spans.** `$SQLITE_SRC` / `_src/sqlite-3.53.4` was not present in this worktree, so `pager.c` descriptions rely on `ch02-file-format-and-pages/SPANS.txt`, `agent_work/ch02/build-notes.md`, and `docs/knowledge/source-inventory-ch01-04.md` without quoting verbatim C lines from `pager.c`.

## Suspected defects (code left untouched per rule)

1. **`internal/dbfile/payload.go:11` (`OverflowPage.OwnerRow`).** Exported struct field `OwnerRow int64` is never populated by `OverflowChain` (always 0). Minor dead field; does not break any test or experiment.
2. **`internal/dbfile/header.go:49-50` (`be64i`, `be64u`) and `dbfile_test.go:612` (`_ = sha256.Size`).** Unused helpers/blank identifier; harmless.
3. **`ch02-file-format-and-pages/experiments/2.3-page-manifest/main.go:188`.** `if len(entries) < len(lines)` is always true because `len(lines) == 3 + len(entries)` (3 header lines). Works for C1–C3 because C1 drops 1 entry and C2/C3 keep 270 entries, so slicing `pageBytes[:len(entries)*4096]` stays in bounds; an edit adding entries would panic.
4. **`internal/capture/capture.go:193-197`.** Checking `if _, err := os.Stat(outPath + suffix); err == nil` ignores non-`os.ErrNotExist` stat errors (for example, permission errors). Minor; not triggered in normal runs.
5. **`internal/dbfile/header.go:114-119`.** `Header.validate` rejects `TextEncoding > 3` and `SchemaFormat > 4` with `ErrNotADatabase`, whereas `lockBtree` itself does not validate those two offsets (the schema layer validates them later). Good defensive check for a standalone parser, just slightly earlier than `lockBtree`.
