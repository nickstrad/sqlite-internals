# Neon prose notes — chunk ch01

## What was written

### Deliverable A — Go code comments (comment-only edits)
- `internal/recipe/recipe.go`: package doc comment, doc comments on all exported constants, variables, types, functions, and methods, and WHY comments on PRNG seeding, periodic deletes, and `PRAGMA user_version`.
- `internal/recipe/apply.go`: doc comments on all exported interfaces, variables, functions, and methods, including the `src/pager.c:3109-3110` / `:3135` / `:3264` reason `ApplyInSession` opens one connection per step.
- `internal/recipe/hashes.go`: doc comments on all exported variables, types, functions, and methods, noting the POSIX-lock hazard on `FileSHA256`, `pager_write_changecounter` (`src/pager.c:3130-3143`) for `ChangeCounterRanges`, cache-key versioning, and atomic temp-file rename.
- `internal/recipe/recipe_test.go`: one-line doc comment on each of the 12 `Test*` functions.
- `ch01-architecture-and-execution-path/experiments/1.1-pin-check/main.go`: package doc comment and WHY comments on required/compared options, `debug.ReadBuildInfo`, virtual table smoke test, and `INFO:` diff lines.
- `ch01-architecture-and-execution-path/experiments/1.2-one-insert/main.go`: package doc comment and WHY comments on WAL geometry (`src/wal.c:21-60`), marker files, `EXPLAIN` (`src/vdbeapi.c:863-865`), `childInsert` thread pinning and pre-close WAL read, syscall normalization, musl `fcntl F_SETFD`, rollback vs WAL write checks, twin check, and `xRandomness` `INFO:` handling.
- `ch01-architecture-and-execution-path/experiments/1.3-recipe-rebuild/main.go`: package doc comment and WHY comments on page diffing, `onlyChangeCounter` (`src/pager.c:3130-3143`), single-conn PRAGMA pinning, `current_timestamp` one-second resolution, `mergedWithNext`, freelist walking, unused-byte classification, and WAL session counting (`src/pager.c:3109-3110`).

Verification: `gofmt -l` clean (`PASS check gofmt: 38 Go file(s) formatted`), `go build ./...`, `go vet ./...`, `GOOS=darwin GOARCH=arm64 go build ./...`, `go test ./internal/recipe/ ./internal/repocheck/` all pass; diff of non-comment lines shows zero code-token changes.

### Deliverable B — Chapter prose and answers
- `ch01-architecture-and-execution-path/ch01-architecture-and-execution-path.md`:
  - Title, pin block from `sqlite/PIN`, and "How to run the experiments" box (`fetch-source`, `build-tools`, Docker `lab` route for macOS).
  - All eight required sections (`1. Model` with architecture diagram, `2. Question`, `3. Call path`, `4. Timeline`, `5. Lab`, `6. Evidence and failure cases` ending with `### What attacked this chapter` and `<!-- adversarial-results -->`, `7. Platform implications`, `8. Knowledge check`).
  - All 12 `SPANS.txt` spans cited with `version-3.53.4` GitHub links and local copy references (`vdbeapi.c:105-124` and `vdbeaux.c:3856` also directly verified in the copied chapter files).
  - All seven corrections from `agent_work/ch01/build-notes.md` reflected in the text (WAL change counter counting sessions, WAL + `secure_delete=ON` batching equality vs `OFF` unused B-tree leaf bytes, unordered `INSERT ... SELECT` needing a plan change, `current_timestamp` 1-second resolution, musl `fcntl F_SETFD` in the twin check, WAL zero writes to `t.db`, and `secure_delete=ON` in the recipe).
  - `internal/recipe` (`recipe.go`, `apply.go`, `hashes.go`) and all three experiments + evidence files inlined via `go run ./cmd/course inline` (9 include blocks; `inline -check` passes). `internal/sqlitex` and `internal/trace` summarized and linked without inlining.
- `ch01-architecture-and-execution-path/answers.md`: complete answers to the five knowledge-check questions.

## Open doubts

- **Include-marker paths relative to chapter directory vs repo root.** The brief's example showed a repo-root path (`<!-- include: ch01-architecture-and-execution-path/experiments/1.1-pin-check/main.go -->`), whereas `internal/repocheck/inline.go` resolves `include.Target` relative to `filepath.Dir(markdownPath)`. Include markers in `ch01-architecture-and-execution-path.md` therefore use paths relative to `ch01-architecture-and-execution-path/` (`experiments/...`, `evidence/...`, `../internal/recipe/...`) so `go run ./cmd/course inline` and `inline -check` pass cleanly.
- **`internal/recipe/recipe_test.go` not inlined.** The three package implementation files (`recipe.go`, `apply.go`, `hashes.go`) are inlined in full in Section 5; `recipe_test.go` is linked with its `go test ./internal/recipe/` command rather than inlined. If the course wants test files inlined too, one more include marker can be added in Section 5.1.
- **`internal/sqlitex` and `internal/trace` comments on this branch.** This worktree (`ch01`) does not yet have the chunk 0 prose commit merged in, so `internal/sqlitex/` and `internal/trace/` are currently uncommented here. Per the brief, neither package was edited.

## Suspected defects

None observed in `internal/recipe/` or `ch01-architecture-and-execution-path/experiments/`.
