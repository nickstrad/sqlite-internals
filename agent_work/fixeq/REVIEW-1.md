VERDICT: PASS

## Findings

1. **Cause verified** — `modernc.org/sqlite@v1.60.1/stmt.go:95-123`, `sqlite.go:77-116`, `tx.go:61-79`, SQLite `src/vdbeapi.c:804-806`
   - `(*conn).ExecContext` (`conn.go:1420-1427`) calls `(*conn).exec` (`conn.go:1155-1168`), which calls `(*stmt).exec` (`stmt.go:95`).
   - When `ctx.Done() != nil`, `(*stmt).exec` registers `defer interruptOnDone(ctx, s.c, &done)()` (`stmt.go:99-106`) and then a second `defer` (`stmt.go:109-123`).
   - Because defers run last-in, first-out, the second defer checks `atomic.LoadInt32(&done) != 0` before `interruptOnDone`'s cleanup sets `done = 1` (`sqlite.go:108-115`). If the watcher goroutine (`sqlite.go:92-105`) fires after `sqlite3_step` already completed `BEGIN`, the transaction stays open while `(*stmt).exec` rewrites the return value to `nil, ctx.Err()` (`stmt.go:110-112`).
   - SQLite resets `db->u1.isInterrupted` to `0` at the start of `sqlite3_step` whenever `db->nVdbeActive == 0` (`src/vdbeapi.c:804-806`), so a late interrupt does not break the next statement. `(*tx).exec` (`tx.go:61-79`) passes `nil` for `done` and does not rewrite a successful result.
   - Minor note in `agent_work/fixeq/build-notes.md`: its cited external line ranges are off by one line (`stmt.go:94-106,108-111`, `sqlite.go:77-115`, `tx.go:63-80`, `vdbeapi.c:804-805` vs actual `95-107,109-112`, `77-116`, `61-79`, `804-806`). The mechanism itself is accurate.

2. **Fix closes the bug without new defects** — `internal/equal/logical.go:510-532`
   - Line 511 rejects an already-canceled context before opening a transaction.
   - Lines 514-517 run deferred `BEGIN` under `context.WithoutCancel(ctx)`. Its `Done()` is `nil`, so `(*stmt).exec` starts no watcher and cannot rewrite a successful `BEGIN` into `ctx.Err()`. A deferred `BEGIN` takes no locks and does no I/O, so losing cancellation during `BEGIN` is safe.
   - Line 518 runs `logicalHashInTx(ctx, conn)` under the caller's `ctx`, so a long hash still cancels.
   - Lines 519-530 end the transaction under `uncancelable`: `ROLLBACK` on walk error (joining a rollback error onto `err` so `errors.Is` still matches the original cause), and `COMMIT` with `ROLLBACK` fallback on walk success. Every error path returns `Logical{}`.
   - Checked `internal/procs/sqlrole.go:180,249-258` and `internal/procs/protocol.go:138`: the only production caller passes `context.Background()`, confirming the remaining sites are latent.

3. **Regression test fails on the old code and passes on the fix** — `internal/equal/latecancel_test.go:14-118`
   - All six driver interfaces asserted on `c.Conn` (`ExecerContext`, `QueryerContext`, `ConnPrepareContext`, `ConnBeginTx`, `SessionResetter`, `Validator`) are implemented by `modernc.org/sqlite`'s `*conn` (`conn.go:1046,1056,1400,1410,1420,1430`).
   - Running `latecancel_test.go` against an unmodified `main` checkout fails deterministically with the production error:
     `latecancel_test.go:85: limit 2: the read transaction was left open: SQL logic error: cannot start a transaction within a transaction (1)`.
   - On the fix, the test passes and its self-check (`latecancel_test.go:103-117`) confirms the wrapper triggers on a real `BEGIN` that leaves a transaction open. No data races under `-race`.

## Commands run

- `go build ./... && go vet ./... && go test -v ./internal/equal/` — PASS (all tests in `internal/equal` passed; `dbhash` sub-assertions skipped where binary is absent)
- `go test -race -count=5 ./internal/equal/ -run EndsItsTransaction` — PASS
- Copied `internal/equal/latecancel_test.go` onto a clean temporary archive of `main` and ran `go test -v ./internal/equal/ -run TestLogicalHashEndsItsTransactionWhenAStatementSucceedsButReportsCancellation` — FAIL as expected at `latecancel_test.go:85` (`limit 2: the read transaction was left open: SQL logic error: cannot start a transaction within a transaction (1)`)
