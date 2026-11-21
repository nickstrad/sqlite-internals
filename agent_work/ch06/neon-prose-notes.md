# Neon prose pass notes — chunk ch06 (Chapter 6 — Locking and concurrent access)

## What was written

### Deliverable A — Go code comments (comments only; zero code tokens changed)
- `internal/sqlitex/codes.go`, `internal/sqlitex/display.go`, `internal/sqlitex/codes_test.go`: doc comments on all exported result-code constants, `ResultCodeName`, and `DisplayPath`, plus `sqlite.h.in:449-546` / `src/os_unix.c` / `src/wal.c` references and one-line comments on both tests.
- `internal/procs/` (all 14 `.go` files: `protocol.go`, `parent.go`, `script.go`, `lab.go`, `sqlrole.go`, `database.go`, `locks.go`, `lockprobe.go`, `lockprobe_unix.go`, `lockprobe_other.go`, `proclocks.go`, `proclocks_linux.go`, `proclocks_other.go`, `procs_test.go`): package doc comment on `protocol.go`, doc comments on every exported type, constant, sentinel error, function, and method, WHY comments citing pinned SQLite source spans (`src/os.h:160-166`, `src/os_unix.c:1189-1255,4602-4603,4679-4695`, `src/wal.c:294-299`), preserved all `//go:build` directives verbatim, and one-line comments on all 28 tests in `procs_test.go`.
- `ch06-locking-and-concurrency/experiments/` (all 4 `main.go` files: `6.1-locks-belong-to-the-process`, `6.2-busy-rules`, `6.3-torn-copy`, `6.4-live-snapshot`): package doc comments and explanatory comments on key functions and constants citing verified SQLite spans.

### Deliverable B — Chapter prose and knowledge-check answers
- `ch06-locking-and-concurrency/ch06-locking-and-concurrency.md`: complete Chapter 6 with pin block (`3.53.4`, `version-3.53.4`, `modernc.org/sqlite v1.60.1`), run instructions (including Linux-only `/proc/locks` note and `docker compose run --rm lab` macOS route), all eight required `## N. ` sections, text diagram, verified source citations from `SPANS.txt` (36 spans), Version notes, Experiment 6.1 (with **Why kept** line and all four parts A–D), Experiment 6.2 (with result matrix and `StillWaitingWindow` explanation), full inlined walk-through of `internal/procs` (13 non-test files), Experiment 6.3, Experiment 6.4 (Rung R4, including `nBackfill` skipping and `-hold immediate` and `WAL_READ_LOCK(0)` restart notes), `### What attacked this chapter` directly after `<!-- adversarial-results -->` covering all 5 scripts and runs 1–3 (43/6 → 45/4 → 49/0), Platform implications tied to evidence, and five Knowledge check questions. All 21 `<!-- include: ... -->` blocks filled and verified via `go run ./cmd/course inline` and `inline -check`.
- `ch06-locking-and-concurrency/answers.md`: five complete answers matching the questions in Section 8.

## Verification performed
- `gofmt -l internal/procs internal/sqlitex ch06-locking-and-concurrency`: empty (clean).
- Comment-only diff against `HEAD` for all 21 modified `.go` files: zero non-comment token changes; all `//go:build` directives preserved.
- `go build ./... && go vet ./...` and `GOOS=darwin GOARCH=arm64 go build ./... && GOOS=darwin GOARCH=arm64 go vet ./...`: pass.
- `go test -v -count=1 ./internal/procs ./internal/sqlitex`: all tests pass.
- `go run ./cmd/course inline -check ch06-locking-and-concurrency/ch06-locking-and-concurrency.md`: PASS (21 include blocks match their files).
- `go run ./cmd/course check sources ch06`: PASS (2 copied files match the pin).
- `go run ./cmd/course check spans ch06`: PASS (36 spans start at their symbol).
- Path/link/forbidden-string audit: zero `/root/`, `/home/`, `/Users/`, or Leanpub book references in chapter text, answers, or comments; zero broken local markdown links.

## Open doubts
- None. All seven corrections and refinements from `build-notes.md`, `REVIEW-1.md`, `REPAIR-REPORT.md`, `REVIEW-2.md`, and `adversarial/results.txt` are reflected accurately, and macOS cross-build-only status is stated plainly.

## Suspected defects
- None found. Every review finding and adversarial failure from Run 1 was resolved or answered prior to this closing prose pass.
