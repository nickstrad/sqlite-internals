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

---

## Closing rewrite (2026-11-21)

### What changed
- **`ch01-architecture-and-execution-path/ch01-architecture-and-execution-path.md`** (rewritten front to back for consistency with Chapter 5's voice and layout):
  - Updated the "How to run the experiments" box to list all three `go run` commands and record Debian `lab` container verification alongside Linux measurement and macOS cross-build status.
  - Defined every database-internals term at first use (`bytecode`, `opcode`, `register`, `cursor`, `page`, `leaf page`, `cell`, `freeblock`, `schema cookie`, `file change counter`, `version-valid-for`, `rowid`, `overflow page`, `freelist` trunk/leaf pages, `dirty page`, `atomic`, `PRAGMA`, `sidecar files`, `POSIX advisory locks`, `E1`, `sqlite_dbpage`, `dbstat`, `ANALYZE`, `xShmBarrier`, `xFileControl`, `SAVEPOINT`).
  - Rewrote patched-on paragraphs (Section 1 WAL-default explanation, Section 2 `sqlitex`/`trace` summary, Experiment 1.2 Claim, Section 5.1 package overview, Section 7 Platform implications) into short, plain sentences with a summary table at the end of Section 7 matching Chapter 5.
  - Standardized experiment metadata blocks (`Claim`, `Evidence form`, `Platform` / `Why kept` where applicable, `Run command`, `Captured output`, `**Reading the evidence:**`) across Experiments 1.1–1.3.
  - Added full `github.com/sqlite/sqlite/blob/version-3.53.4/...` links for all source spans, including `src/os.c:88-91`, `92-95`, `99-102`, `src/vdbeInt.h:26`, `src/os_unix.c:867-869`, `src/pager.c:3109-3110`, `3130-3143`, `3264`, and `src/btree.c:3181-3194`, `6740`, `6864-6874` (corrected from `6865-6876`).
  - Grounded every number strictly in the evidence files, adversarial `results.txt`, or pinned source (removed uncited "ChaCha20" mention and approximate "1.1 MiB"; expanded `DET-02` to name all 9 matching non-zero `cache_size` values vs `cache_size=0`).
  - Expanded `### What attacked this chapter` (`<!-- adversarial-results -->`) to document Run 1 (`results-run1.txt`, 37 HELD / 11 BROKEN), Run 2 (`results-run2.txt`, 37 HELD / 11 BROKEN after repair + foundation merge, including `PIN-04`, `DET-04`, and `DET-02` `cache_size=0`), and the final run (`results.txt`, 48 HELD / 0 BROKEN).
  - Re-inlined all 9 code and evidence blocks with `go run ./cmd/course inline`.
- **`ch01-architecture-and-execution-path/answers.md`**:
  - Added `version-3.53.4` GitHub links for every cited source span, aligned Q3/Q4/Q5 answers with the expanded questions (`ANALYZE`/`ENABLE_STAT4`, page-1-untouched WAL sessions and `cache_size=0`, and `secure_delete=FAST` vs `ON` with `src/btree.c:3181-3194` and `6864-6874`).
- **Code comments (comment-only edits, zero code-token changes)**:
  - `ch01-architecture-and-execution-path/experiments/1.3-recipe-rebuild/main.go`: fixed `secureDeleteFastVariant` comment line span from `src/btree.c:6865-6876` to `src/btree.c:6864-6874`.
  - `internal/recipe/recipe.go`: updated `SQLiteOptions` doc comment to reflect that `secure_delete=ON` zeroes freed freelist pages (leaves and trunks) and `SingleConnection` prevents pool churn from shifting the WAL change counter.

### Verification
- `go run ./cmd/course inline ch01-architecture-and-execution-path/ch01-architecture-and-execution-path.md` and `-check`: PASS (9 include blocks match).
- `go run ./cmd/course check gofmt`: PASS (102 Go files formatted); `gofmt -l internal/recipe ch01-architecture-and-execution-path`: empty.
- `go build ./... && go vet ./...` and `GOOS=darwin GOARCH=arm64 go build ./... && GOOS=darwin GOARCH=arm64 go vet ./...`: PASS.
- `go run ./cmd/course check sources ch01`, `check spans ch01`, `check paths`: all PASS.

### Open doubts
- `gofmt -l .` from the repo root still prints five untracked/Phase-1 files under `agent_work/probes/` (`determinism-rebuild/go/session/main.go`, `walreplay/main.go`, `go-access-vfs/cgo/cmd/fault/main.go`, `strace/main.go`, `vfs/ncruceslog/main.go`). Per the brief, `agent_work/` was not modified outside this notes file; `go run ./cmd/course check gofmt` passes on all 102 tracked Go files.
