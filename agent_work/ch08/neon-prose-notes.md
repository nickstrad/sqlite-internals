# Neon prose pass notes — Chapter 8 (`ch08-connection-and-database-lifecycle`)

## What was written

### Deliverable A — Go comments (comments only; token-for-token identical to `HEAD` after stripping non-directive comments)
- `internal/procstat/procstat.go`, `procstat_linux.go`, `procstat_other.go`, `procstat_test.go`: package doc comment, doc comments on every exported type/constant/variable/function/method, WHY comments on `dir.Fd()` exclusion, `directoryPrefix` normalization, and `kibToBytes` overflow checking, and one-line comments on all 9 tests.
- `internal/connmgr/atrest.go`, `connmgr.go`, `connmgr_test.go`: package doc comment, doc comments on every exported type/constant/variable/function/method, WHY and pinned-source comments on SQLite's POSIX lock range (`0x40000000`, 512 bytes), `CheckAtRest` open+close lock-drop hazard, `hotJournal`, `OpenConnections > 0` check skipping `CheckAtRest`, `ctx.Err()` pre-eviction check, post-`Open` counter accounting, and one-line comments on all 14 tests.
- `internal/sqlitex/placement.go` and `internal/sqlitex/sqlitex_test.go`: doc comments on every exported type/constant/function in `placement.go` (citing `src/prepare.c:323-333` for `default_cache_size`), and a one-line comment on `TestSettingPlacementsCoverEveryCourseSetting` (the one test added by this chunk).
- `ch08-connection-and-database-lifecycle/experiments/8.1-open-cost-and-where-settings-live/main.go`, `8.2-idle-cost/main.go`, `8.3-bounded-manager/main.go`: package doc comments and WHY / pinned-source comments on key functions and SQLite mechanisms.

### Deliverable B — Chapter markdown and knowledge-check answers
- `ch08-connection-and-database-lifecycle/ch08-connection-and-database-lifecycle.md`: complete chapter with pin block, run instructions (including Linux-only notes and macOS Docker route), all eight `## N. ` sections, WAL-default evaluation, Rebuild Ladder rung R5, verified C spans from `SPANS.txt`, outline corrections from `build-notes.md` and `REPAIR-REPORT.md`, 11 inlined code/evidence blocks (all 3 experiments, all 3 evidence files, and the two newly introduced shared packages `internal/procstat` and `internal/connmgr`), Section 6 `### What attacked this chapter` starting with `<!-- adversarial-results -->` and summarizing all 4 adversarial programs across runs 1, 2, and final (29 HELD, 0 BROKEN), Version notes, evidence-backed Platform implications (including the documentation-only `ATTACH` paragraph), and five knowledge-check questions.
- `ch08-connection-and-database-lifecycle/answers.md`: complete answers to all five knowledge-check questions.

## Verification performed
- Token-stream equality check against `HEAD` (stripping non-directive comments, matching `repocheck.SameCode`): all 12 modified `.go` files matched identically.
- `gofmt -l internal/connmgr internal/procstat internal/sqlitex ch08-connection-and-database-lifecycle`: empty output.
- `go build ./... && go vet ./... && go test -race ./internal/connmgr ./internal/procstat ./internal/sqlitex`: PASS.
- `go run ./cmd/course inline ch08-connection-and-database-lifecycle/ch08-connection-and-database-lifecycle.md` and `-check`: PASS (11/11 blocks match disk).
- `go run ./cmd/course check sources ch08` (3 files) and `check spans ch08` (34 spans): PASS.
- `/root/` sweep across `ch08-connection-and-database-lifecycle/` and all modified Go files: zero matches.
- Section count check: exactly eight `## N. ` headings; `<!-- adversarial-results -->` present at the top of `### What attacked this chapter`.

## Open doubts
- macOS (`darwin/arm64` and `darwin/amd64`) was cross-built and cross-vetted only during build/review, not executed; the chapter states plainly which measurements (strace phase traces, `/proc/self/fd` target names, and the 256 KiB/connection uncounted memory margin in 8.2) were measured on Linux only.

## Suspected defects
- None. All findings from `REVIEW-1.md` and Run 1 of the adversarial suite were already resolved or scoped in `REPAIR-REPORT.md` and verified in `REVIEW-2.md` and `agent_work/ch08/adversarial/results.txt`.
