# Neon prose pass notes — Chapter 9 (Backup, crash recovery, and integrity)

Author: Gemini 4 Neon (closing prose pass, 2026-11-21).

## What was written

### Deliverable A — Go comments (22 files, comments only)
- **New shared package `internal/walship/`:** added package doc comment (`store.go`), Go doc comments on every exported type, constant, sentinel error, function, and method, plus concise WHY comments citing pinned SQLite source locations in `store.go`, `format.go`, `apply.go`, `catalog.go`, and `shipper.go`, and one-line comments on every test in `store_test.go`, `walship_test.go`, and `repair_test.go`.
- **Chapter 9 additions to earlier shared packages (existing earlier-chapter comments preserved untouched):**
  - `internal/capture/capture.go` (`VerifyImage`) and `capture_test.go` (`TestVerifyImageNamesChangedPages`)
  - `internal/dbfile/record.go` (`RecordColumn`) and `dbfile_test.go` (`TestRecordColumn`)
  - `internal/procs/sqlrole.go` (`recipeSession`) and `procs_test.go` (`TestRecipeSessionCommitsOnAFreshConnection`)
  - `internal/sqlitex/raw.go` (`SourceExecer`, `BackupWithSourceExec`) and `sqlitex_test.go` (`TestBackupWithSourceExecMirrorsSameConnectionWrites`)
  - `internal/trace/sync.go` (`SyncRecord`, `String`, `SyncsOf`) and `sync_test.go` (`TestSyncsOf`)
- **Chapter 9 experiments:** added package doc comments and WHY comments citing pinned source spans in `experiments/9.1-capture-comparison/main.go`, `9.2-crash-versus-power-loss/main.go`, `9.3-integrity-check-limits/main.go`, and `9.4-point-in-time-rebuild/main.go`.

### Deliverable B — Chapter text and knowledge-check answers
- `ch09-backup-crash-recovery-integrity/ch09-backup-crash-recovery-integrity.md`: complete chapter with pin block, prerequisites and Linux-only/macOS Docker instructions, all eight required `## N.` sections, text model diagram, WAL-default evaluation (ruling 10), Rebuild Ladder rung R6 placement, all 24 verified `SPANS.txt` source references with pinned GitHub links, Experiments 9.1–9.4 (code + run commands + captured evidence + line-by-line readings), full inlined walk-through of `internal/walship` in Section 5 (13 total include blocks rewritten and verified via `cmd/course inline`), Version notes, `### What attacked this chapter` (`<!-- adversarial-results -->` plus full Run 1 -> Repair -> Run 2 -> Run 3 account of all 40 attacks), evidence-backed Platform implications, and five Knowledge check questions.
- `ch09-backup-crash-recovery-integrity/answers.md`: five detailed answers tied to the cited C source lines, experiments, and adversarial results.

## Verification run
- `go run ./cmd/course inline ch09-backup-crash-recovery-integrity/ch09-backup-crash-recovery-integrity.md` and `inline -check`: PASS (13 include blocks match their files).
- `go run ./cmd/course check sources ch09`: PASS (2 copied files match pin).
- `go run ./cmd/course check spans ch09`: PASS (24 spans start at their symbol).
- `go run ./cmd/course check gofmt` & `gofmt -l` on all modified directories: PASS (0 unformatted files).
- `go build ./... && go vet ./...` (linux/amd64) and `GOOS=darwin GOARCH=arm64 go build ./... && go vet ./...`: PASS.
- Comment-stripped token comparison against `HEAD` across all 22 modified `.go` files: PASS (`22 Go files differ ONLY in comments`).
- Machine-path grep (`/root/|/home/|/Users/`) across `ch09-backup-crash-recovery-integrity/`, `internal/walship/`, and `git diff -- '*.go'`: 0 matches.
- Local markdown link check in `ch09-backup-crash-recovery-integrity.md`: 31/31 local links resolve.

## Discrepancies reconciled (evidence authoritative over notes)
- `agent_work/ch09/build-notes.md` (lines 604 and 731) states Experiment 9.2 Part A runs `30` kills. Counting the actual evidence file `ch09-backup-crash-recovery-integrity/evidence/9.2-crash-versus-power-loss.txt` shows **`36` kill runs** (`6` at `OFF`: 0 syncs + 6 pwrites; `11` at `NORMAL`: 5 syncs + 6 pwrites; `19` at `FULL`: 13 syncs + 6 pwrites). The chapter text uses the evidence file's authoritative count (`36`).

## Open doubts / Suspected defects (code left untouched per Deliverable A)
1. **Experiment 9.2 check label (`ch09-backup-crash-recovery-integrity/experiments/9.2-crash-versus-power-loss/main.go:332`, `REVIEW-2.md` nit 1):** Line 14 of the evidence prints `check ok: only synchronous=OFF leaves database-file writes that were never synced`, which predates the repair round's fourth model run (`NORMAL` with a reader-pinned partial checkpoint, where line 41 also reports `database file 0 sync(s); 15 write(s)`). The predicate (`level == "OFF"` inside `if outside != ""`) is valid, and Section 5.1 explicitly clarifies the label's exact meaning for readers (at `OFF` uniquely, the WAL frames behind those database writes were *also* never synced).
2. **`walship.Open` on an all-damaged store (`internal/walship/shipper.go:163-194`, `REVIEW-2.md` nit 2):** Because the damaged-tail check (`position > last`) lives inside `resume()`, which runs only when `catalog.LastPosition()` finds at least one intact object, calling `Open` on a store containing *only* damaged objects with a different `Origin` takes the fresh-store branch (with the same `Origin`, `PutIfAbsent` fails closed with `ErrExists`). Noted transparently in Section 5.2.
