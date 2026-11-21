# agent_work/fixeq index

| path | purpose | what it proved |
|---|---|---|
| `agent_work/fixeq/repro/main.go` | Stress reproducer. Mode 1 runs a raw `BEGIN` under a context whose `Done()` closes on its 2nd call, then checks whether the transaction is open. Mode 2 runs `LogicalHash` with Done-limits 1..80 and checks for a leaked transaction. Flags: `-n` iterations, `-procs` GOMAXPROCS, `-burn` CPU-burning goroutines. Run: `go run ./agent_work/fixeq/repro -n 2000 -procs 2 -burn 8` | modernc can report a `BEGIN` that succeeded as `context canceled`. Measured: 1-2 errors per 2000 runs across GOMAXPROCS 1/2/4, burn 0/8, and every one left the transaction open. Mode 2 never hit the window in 2000-run batches (the window is microseconds wide), so the deterministic proof is the unit test `internal/equal/latecancel_test.go`. |
