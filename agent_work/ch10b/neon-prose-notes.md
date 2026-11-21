# Neon prose pass notes — chunk ch10b (Chapter 10 — Multi-database platform capstone)

Author: Gemini 4 Neon.

## What was written

### Deliverable A — Go code comments (comments only; 0 code diffs across all 38 `.go` files)
- **Updated stale 10a comments** for 10b's additions:
  - `ch10-multi-database-platform-capstone/service/catalog/catalog.go`: updated package doc comment to describe both the `databases` table and the `relocations` table (`relocation.go`).
  - `ch10-multi-database-platform-capstone/service/node/node.go`: updated package doc comment to document all four crash-surviving per-database sidecars (`.owner`, `.atrest`, `.sealed`, `.staged`) and relocation operations; updated `Config` (`AckBeforePut` mutant), `Claim` (clearing `.sealed`/`.staged`), and `Write` (sealed refusal + `AckBeforePut` note).
  - `ch10-multi-database-platform-capstone/service/node/process.go`: documented `ProcessConfig.AckBeforePut` and why `Args()` appends `-ack-before-put` only when set.
  - `ch10-multi-database-platform-capstone/service/node/serve.go`: documented dispatch of relocation/inspection commands through `handleRelocation` (`commands.go`).
- **Commented all new 10b Go files** (package doc comments, doc comments starting with every exported identifier, WHY comments citing pinned SQLite spans where relevant, and one-line comments on every `Test*` function):
  - `service/s3store/batch.go`, `batch_test.go`
  - `service/catalog/bulk.go`, `relocation.go`, `relocation_test.go`
  - `service/fence/inspect.go`, `inspect_test.go`
  - `service/node/relocate.go`, `commands.go`, `remote.go`, `kill.go`, `relocate_test.go`
  - `service/relocate/relocate.go`, `relocate_test.go`
  - `service/lab/lab.go`, `fleet.go`, `lab_test.go`
  - `experiments/10.2-acknowledged-writes-survive/main.go`
  - `experiments/10.3-interrupted-relocation/main.go`
  - `experiments/10.4-rebuild-the-fleet/main.go`

### Deliverable B — Chapter 10 and knowledge-check answers
- `ch10-multi-database-platform-capstone/ch10-multi-database-platform-capstone.md`:
  - Pin block matching `sqlite/PIN` (SQLite `3.53.4`, `version-3.53.4`, `modernc.org/sqlite v1.60.1`, SeaweedFS `4.48`, copied `wal.c`, linked `src/os_unix.c`) plus run prerequisites, Linux-only `strace` note with macOS Docker `lab` route for 10.2 Part A, and plain measurement-environment statement.
  - All eight required `## N. ` sections: Model (three guarantee layers kept separate, component diagram, durability model table, WAL default evaluation, R7, prior-art context), Question (all six capstone invariants as checkable properties), Call path (all `12` spans in `SPANS.txt` with GitHub links and C excerpts), Timeline (takeover timeline, seven-step relocation timeline, resume/rollback decision tree), Lab (inlining `../internal/walship/atrest.go` and every non-test `.go` file across all nine `service/` packages, plus Experiment 10.1 code, evidence, and line-by-line reading), Evidence and failure cases (Experiments 10.2, 10.3, 10.4 including both the `500`-DB default and `2,000`-DB evidence files, Version notes, and `### What attacked this chapter` covering both 10a's `27` attacks and 10b's `25` attacks directly after `<!-- adversarial-results -->`), Platform implications (all eight design questions tied to evidence, operational signals, and explicit out-of-scope list), and Knowledge check (5 questions).
  - Synchronized `30` include blocks with `go run ./cmd/course inline` and verified with `inline -check`.
- `ch10-multi-database-platform-capstone/answers.md`: five detailed answers tied to the pinned source, Go packages, evidence files, and adversarial results.

## Verification commands run

- `go run ./cmd/course inline ch10-multi-database-platform-capstone/ch10-multi-database-platform-capstone.md`: `PASS ... 30 include block(s), 30 rewritten from their files`
- `go run ./cmd/course inline -check ch10-multi-database-platform-capstone/ch10-multi-database-platform-capstone.md`: `PASS ... 30 include block(s) match their files`
- `go run ./cmd/course check sources ch10`: `PASS ... 1 copied file(s) match the pin`
- `go run ./cmd/course check spans ch10`: `PASS ... 12 span(s) start at their symbol`
- `go run ./cmd/course check gofmt`: `PASS ... 271 Go file(s) formatted`
- `go run ./cmd/course check paths`: `PASS ... 339 tracked file(s) free of machine-local paths`
- `gofmt -l ch10-multi-database-platform-capstone/`: empty
- `go build ./... && go vet ./...`: PASS
- Token-stream comment-strip comparison across all `38` `.go` files under `ch10-multi-database-platform-capstone/`: `0` code diffs, `0` missing/bad doc comments
- Grep for `/root/` in `ch10-multi-database-platform-capstone/`: no matches
- Grep for `^## [0-9]+\. ` in `ch10-multi-database-platform-capstone.md`: exactly `8` sections

## Open doubts

- None. All numbers in the chapter text were checked directly against the committed evidence files (for example, `306` replies in `evidence/10.2-acknowledged-writes-survive.txt`, `1,726` in `10.3`, `2,766` in `10.4`, and `11,025` in `10.4-2000`, resolving small discrepancies in `build-notes.md` in favor of the authoritative evidence files).
- Both the `500`-database default evidence file and the `2,000`-database evidence file are inlined in Section 6.3 so readers can inspect both runs byte for byte.

## Suspected defects

- None found. Every defect surfaced by `REVIEW-1.md` and the 10a/10b adversarial suites was already repaired and verified in `REVIEW-2.md` (`27/27` HELD in 10a, `25/25` HELD in 10b).
