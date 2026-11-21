# fixeq BUILD-REPORT

## Built
- Fix commit `b07eb31`: `internal/equal/logical.go` `LogicalHash` (lines 510-532).
  - Checks `ctx.Err()` before `BEGIN`.
  - Runs `BEGIN`, `COMMIT` and `ROLLBACK` under `context.WithoutCancel`.
  - Tries `ROLLBACK` when `COMMIT` fails.
  - Returns an `end read transaction: …` error whenever the transaction could not be ended.
- New regression test `internal/equal/latecancel_test.go`
  (`TestLogicalHashEndsItsTransactionWhenAStatementSucceedsButReportsCancellation`).
  - Wraps the real driver to report a statement that succeeded as `ctx.Err()` once the context
    has ended.
  - Fails without the fix with the production message (`limit 2: the read transaction was left
    open …`); passes with it.
- Reproducer `agent_work/fixeq/repro/` (indexed in `agent_work/fixeq/INDEX.md`). Cause, evidence
  and comment changes are in `agent_work/fixeq/build-notes.md`.

## Cause
The brief's hypothesis (a late interrupt hits the closing `ROLLBACK`/`COMMIT`) is not the cause.
modernc.org/sqlite v1.60.1 `(*stmt).exec` replaces the result with `ctx.Err()` whenever its
interrupt watcher fired, even after `sqlite3_step` finished. The deferred check at
`stmt.go:108-111` runs before the watcher is disarmed (`sqlite.go:77-115`). So `BEGIN` could
open the transaction yet return `context canceled`, and `LogicalHash` then returned without
ending it.

## Other instances
None real. `internal/procs/sqlrole.go:180,249-258` has the same shape, but it only ever receives
`context.Background()` (`internal/procs/protocol.go:138`), so it is latent and left unchanged.
`database/sql` `BeginTx` users are not affected (`tx.go:63-80` does not rewrite successes).

## Commands
- `go build ./... && go vet ./... && go test ./...`: PASS
- `go test ./internal/equal -run EndsItsTransaction -count=30 -cpu 1,2,4`: PASS (load average ~6-10)
- Without the fix, `go test ./internal/equal -run ReportsCancellation`: FAIL as expected
  (`latecancel_test.go:85: limit 2 …`)
- `bash agent_work/verify/crossbuild.sh`:
  - First run: printed `CROSSBUILD FAILED`. I filtered that run's output, so I did not capture
    which step failed. The standalone `go build && go vet && go test ./...` just before it passed.
  - Immediate rerun: exit 0, `CROSSBUILD OK` (linux/amd64, darwin/arm64, darwin/amd64
    build+vet+test-compile, gofmt).
  - Treat the first failure as an unidentified flake under load. It is unverified.
- `go run ./cmd/course check evidence ch03 -runs 2`: PASS for 3.1, 3.2 and 3.3.
  - This worktree has no built dbhash, so 3.3 first printed `NO-DBHASH`. The run used
    `COURSE_TOOLS_DIR=<another worktree>/_tools`.
  - Experiment output is unchanged.
- `go run ./agent_work/fixeq/repro -n 2000 -procs {1,2,4} -burn {0,8}`: raw `BEGIN` reported
  success as cancellation 0-2 times per 2000 runs, and every one left the transaction open.

## Corrections to the outline
None. This is a code fix only.

## Left undone
- Comments for the new code are Neon's job (listed in build-notes.md).
- `internal/procs/sqlrole.go` is not hardened against a future cancellable context (latent only).
- The `LogicalHash`-level loop in the reproducer did not hit the window statistically; the
  deterministic wrapper test is the proof.
