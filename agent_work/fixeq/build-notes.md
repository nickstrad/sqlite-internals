# fixeq build notes: equal.LogicalHash must always end its read transaction

## Symptom
`internal/equal/repair_test.go` `TestLogicalHashEndsItsTransactionWhenTheContextEnds` failed once on a
loaded 4-core machine: `limit 2: the read transaction was left open: SQL logic error: cannot start
a transaction within a transaction (1)`.

## Cause (verified, and different from the brief's hypothesis)
The brief guessed that a late interrupt hits the closing `ROLLBACK`/`COMMIT`. That is not the cause.
The closing statement ran under `context.WithoutCancel(ctx)`, whose `Done()` returns nil, so the
driver starts no interrupt watcher for it. A watcher from an earlier statement also cannot reach
it, because each watcher is disarmed before its own call returns (see point 3).

The real cause: **modernc.org/sqlite v1.60.1 can return `ctx.Err()` from a statement that
succeeded.** For `LogicalHash`, that statement is the opening `BEGIN`. `LogicalHash` then returned
`begin read transaction: context canceled` without ending the transaction, because it assumed a
failed `BEGIN` opened nothing.

Evidence (module cache `modernc.org/sqlite@v1.60.1`):
1. `stmt.go:94-106` `(*stmt).exec`: if `ctx.Done() != nil`, it registers
   `defer interruptOnDone(ctx, s.c, &done)()`.
2. `sqlite.go:77-115` `interruptOnDone`: a goroutine selects on `ctx.Done()`. When the context
   ends, it CASes `done` 0→1 and calls `sqlite3_interrupt`. The returned cleanup function sets
   `done=1` and closes `donech`.
3. `stmt.go:108-111`: a second deferred function checks `if ctx != nil && atomic.LoadInt32(&done) != 0
   { r, err = nil, ctx.Err() }`. Deferred functions run last-in, first-out, so this check runs
   **before** the `interruptOnDone` cleanup. `done != 0` at that point therefore means the watcher
   fired. The watcher may have fired after `sqlite3_step` already returned `SQLITE_DONE`. The
   statement's effect (here, the open transaction) remains, yet the caller gets `ctx.Err()`.
4. The `interrupt` itself is harmless for later statements: `sqlite3_step` clears
   `isInterrupted` when `db->nVdbeActive==0` (`src/vdbeapi.c:804-805` in 3.53.4). The bug is only
   the error rewrite in point 3.
5. In the failing test, `limit 2` explains the timing. `database/sql` `(*Conn).grabConn` calls
   `ctx.Done()` once (call 1). The driver's pre-check `select` in `stmt.exec` calls it again
   (call 2, which closes the test context's channel), so `BEGIN` returns `context canceled` before
   it runs. That path is fine. The leak happens when the watcher goroutine's own `ctx.Done()` call
   is the one that closes the channel, at a moment when `BEGIN` has finished stepping but its
   deferred functions have not run. On an idle machine the goroutine is rarely scheduled inside
   that window; under load it can be. Which call number closes the channel depends on the test's
   `doneAfter` counter, so other limits can hit the same window.

`database/sql` transactions (`BeginTx`) are not affected the same way. modernc's `tx.exec`
(`tx.go:63-80`) passes `nil` for `done` and does not rewrite a successful result.

## Fix (internal/equal/logical.go:510-532)
- `LogicalHash` checks `ctx.Err()` first (line 511) and returns `begin read transaction: <err>` if
  the context has already ended.
- `BEGIN` runs under `context.WithoutCancel(ctx)` (lines 514-515). With a nil `Done()` the driver
  starts no watcher, so a `BEGIN` error is now genuine and means no transaction was opened. A
  deferred `BEGIN` takes no locks and returns immediately, so it loses nothing by not being
  cancellable.
- The walk, integrity_check and foreign_key_check still run under the caller's `ctx`, so
  cancellation still stops a long hash.
- Ending the transaction, also under `WithoutCancel`:
  - If the walk failed: `ROLLBACK`. If the `ROLLBACK` also fails, the function returns
    `errors.Join(walkErr, "end read transaction: <rollbackErr>")` (lines 519-524).
  - If the walk succeeded: `COMMIT`. If the `COMMIT` fails, it tries `ROLLBACK`, then returns
    `end read transaction: <commitErr>` (joined with the rollback error if that failed too)
    (lines 525-530).
  - So a transaction that cannot be ended always comes back as an error naming
    "end read transaction".
- Behavior change: when `COMMIT` fails, the function now returns `Logical{}` instead of the
  computed result plus the error. Every path that returns an error now returns `Logical{}`;
  `logicalHashInTx` already did this on its own errors.

## Regression test (internal/equal/latecancel_test.go)
`TestLogicalHashEndsItsTransactionWhenAStatementSucceedsButReportsCancellation`
- `lateCancelConnector` / `lateCancelConn` wrap the real modernc driver (`sqlite.Driver{}.Open`).
  `ExecContext` runs the statement. If it succeeded, the context is cancellable
  (`ctx.Done() != nil`) and the context has ended, it returns `ctx.Err()`. This is exactly the
  behavior at `stmt.go:108-111`, made deterministic. It counts each rewrite in
  `successesReportedAsErr`.
- The other driver interfaces (QueryerContext, ConnPrepareContext, ConnBeginTx, SessionResetter,
  Validator) pass straight through to the real connection.
- The test runs `LogicalHash` with the existing `newDoneAfter(limit)` context for limits 1..40.
  After each run it requires that a plain `BEGIN` succeeds on the same conn, i.e. no transaction
  was left open.
- It then checks the wrapper itself: a raw `BEGIN` under `newDoneAfter(1..4)` must produce at least
  one success reported as canceled, and in each such case the transaction must really be open
  (`ROLLBACK` succeeds).
- Without the fix it fails deterministically with the production message:
  `latecancel_test.go:85: limit 2: the read transaction was left open: SQL logic error: cannot
  start a transaction within a transaction (1)`. Verified by stashing the logical.go change and
  running the test; with the fix it passes.

## How it was reproduced
- Real driver, statistical: `go run ./agent_work/fixeq/repro -n 2000 -procs P -burn B` for
  P in {1,2,4} and B in {0,8}. Raw `BEGIN` under a context that closes on the 2nd `Done()` call
  returned an error in 0-2 of 2000 runs per configuration. **Every error left the transaction open.**
  The 2nd `Done()` call is the watcher goroutine's own, so any error there is a success rewritten
  as cancellation.
- The `LogicalHash`-level loop in the same program did not hit the window in 2000-run batches. The
  deterministic regression test above stands in for it.
- After the fix: `go test ./internal/equal -run EndsItsTransaction -count=30 -cpu 1,2,4` passed,
  90 runs of each test, at load average ~6-10.

## Same pattern elsewhere (grep -rn 'WithoutCancel|ROLLBACK|BEGIN|BeginTx' internal ch* cmd)
- `internal/procs/sqlrole.go:180` (`begin` action) and `:249-258` (`recipeStep`: `BEGIN IMMEDIATE`,
  then `ROLLBACK`/`COMMIT` under `ctx`) have the same shape. The only production caller,
  `internal/procs/protocol.go:138`, passes `context.Background()`. Its `Done()` is nil, so the
  driver starts no watcher and these calls can never report a false cancellation. That makes them
  latent, not real instances, so they are left unchanged. They would need the same treatment if
  `RunChild` were ever given a cancellable context: `ROLLBACK` under `WithoutCancel`, plus a
  rollback attempt after a failed `BEGIN IMMEDIATE`.
- `internal/recipe/apply.go:41` and every `BeginTx` in the experiments use `database/sql`
  transactions. modernc `tx.exec` (`tx.go:63-80`) does not rewrite successes, and `database/sql`
  rolls the transaction back itself when the context ends. Most callers also pass
  `context.Background()`. Not an instance.
- `ch03-…/3.3-same-rows-different-bytes/main.go:229-230` (`BEGIN` / deferred `COMMIT`) uses
  `context.Background()`. Not an instance.
- ch07 7.4's `context.WithTimeout` covers only `rangefs.EnsureBucket`, with no SQL transaction.
  Not an instance.

## Comments deleted or now stale
- Deleted, `internal/equal/logical.go` old lines 519-520: "End the transaction with WithoutCancel so
  a canceled caller context never leaks an open read transaction (and pinned WAL read mark) on
  conn." The code it described changed. Neon should write new comments for lines 511-517 (why
  `BEGIN` runs under `WithoutCancel` and the driver behavior at stmt.go:108-111) and lines 519-530
  (end paths).
- Still accurate: the `LogicalHash` doc comment at `internal/equal/logical.go:507-509`.
- `agent_work/ch03/adversarial/equal-falsify/main.go:615` (EQ-09 comment) describes cancellation
  mid-walk. It is still true as a description of that probe, but it predates this cause. It is
  working material, not course text.
- New file `internal/equal/latecancel_test.go` has no comments. Neon should add the test doc
  comment, following the repo_test.go style.

## Outline / evidence impact
None. `LogicalHash` gives the same output for a context that never ends. `go run ./cmd/course check
evidence ch03 -runs 2`: PASS for 3.1, 3.2 and 3.3 (3.3 needs a built dbhash; this worktree has
none, so the run used `COURSE_TOOLS_DIR` pointing at another worktree's `_tools`).
