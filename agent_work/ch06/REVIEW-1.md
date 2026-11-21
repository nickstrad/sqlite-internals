VERDICT: FAIL

## Findings

1. **blocker** — `ch06-locking-and-concurrency/experiments/6.2-busy-rules/main.go:99-103,111-115,171-182`, `internal/procs/lab.go:76-88`, `internal/procs/parent.go:237-258`, `internal/procs/sqlrole.go:230-241`

   **What is wrong:** `Lab.StillWaiting` is a vacuous zero-wait poll, and in Part E of `6.2-busy-rules` the waiting writer does not even contend on `-shm` byte 120 in several rounds even though the experiment reports `check ok: every waiting BEGIN IMMEDIATE was still waiting while the other writer held the lock, and every transaction committed`.

   - `Lab.StillWaiting` calls `call.Answered()` (`internal/procs/parent.go:237-258`), which runs an immediate non-blocking `select { case ... <-call.child.replies: ... default: return false, nil }` microseconds after `lab.Begin` writes JSON to the child's pipe. A separate OS process cannot read the pipe, run SQLite, and reply over the pipe in 0 ns, so `StillWaiting` prints `.. no answer yet` and returns `true` even if the child would immediately return `SQLITE_BUSY` (e.g., `busy_timeout=0`, or C3) or immediately succeed with no lock held.
   - In Part E (`6.2-busy-rules/main.go:171-182`), the parent immediately sends `commit` to `holder` right after that zero-wait poll, while `sqlSession.recipeStep` (`internal/procs/sqlrole.go:235`) calls `s.recipe.Step(k)` — which walks `Plan()` from step 0 through `k` (`internal/recipe/recipe.go:211-223`) — *before* executing `BEGIN IMMEDIATE` (`sqlrole.go:239`). As `k` grows, `holder` finishes `COMMIT` and releases `WAL_WRITE_LOCK` (`-shm` byte 120) before `waiter` even calls `BEGIN IMMEDIATE`.

   **How confirmed:** Built `6.2-busy-rules` and traced `fcntl` calls across three runs with `strace -f -e trace=fcntl`:
   ```
   go build -o /tmp/opencode/exp62 ./ch06-locking-and-concurrency/experiments/6.2-busy-rules
   strace -f -e trace=fcntl /tmp/opencode/exp62 > /tmp/opencode/exp62.out 2> /tmp/opencode/exp62.strace
   ```
   In runs 1 and 2, rounds 3, 4, and 5 of Part E (steps 11, 13, 15) had **zero** `EAGAIN` results on `l_start=120` — `holder` unlocked byte 120 before `waiter` ever tried to lock it (run 1 excerpt, Part E rounds 3–5):
   ```
   1552:[pid 2115200] fcntl(8, F_SETLK, {l_type=F_WRLCK, l_whence=SEEK_SET, l_start=120, l_len=1}) = 0
   1555:[pid 2115200] fcntl(8, F_SETLK, {l_type=F_UNLCK, l_whence=SEEK_SET, l_start=120, l_len=1}) = 0
   1560:[pid 2115204] fcntl(8, F_SETLK, {l_type=F_WRLCK, l_whence=SEEK_SET, l_start=120, l_len=1}) = 0
   1561:[pid 2115204] fcntl(8, F_SETLK, {l_type=F_UNLCK, l_whence=SEEK_SET, l_start=120, l_len=1}) = 0
   1566:[pid 2115204] fcntl(8, F_SETLK, {l_type=F_WRLCK, l_whence=SEEK_SET, l_start=120, l_len=1}) = 0
   1569:[pid 2115206] fcntl(8, F_SETLK, {l_type=F_UNLCK, l_whence=SEEK_SET, l_start=120, l_len=1}) = 0
   1574:[pid 2115200] fcntl(8, F_SETLK, {l_type=F_WRLCK, l_whence=SEEK_SET, l_start=120, l_len=1}) = 0
   1576:[pid 2115200] fcntl(8, F_SETLK, {l_type=F_UNLCK, l_whence=SEEK_SET, l_start=120, l_len=1}) = 0
   1581:[pid 2115200] fcntl(8, F_SETLK, {l_type=F_WRLCK, l_whence=SEEK_SET, l_start=120, l_len=1}) = 0
   1592:[pid 2115199] fcntl(8, F_SETLK, {l_type=F_UNLCK, l_whence=SEEK_SET, l_start=120, l_len=1}) = 0
   1597:[pid 2115206] fcntl(8, F_SETLK, {l_type=F_WRLCK, l_whence=SEEK_SET, l_start=120, l_len=1}) = 0
   ```
   In run 3 (with `-tt` timestamps), round 4 (step 13) unlocked at `01:16:37.965916` and the waiter's first `F_WRLCK` arrived 9 ms later at `01:16:37.974867` (`= 0`, zero `EAGAIN`). Yet all three runs printed `.. no answer yet` and `check ok: every waiting BEGIN IMMEDIATE was still waiting while the other writer held the lock, and every transaction committed`.

   **Expected fix:**
   - Add a non-destructive bounded wait on `Call` (e.g., `call.AnsweredWithin(d time.Duration) (bool, error)` using a `time.Timer` without killing the child on expiry) and have `Lab.StillWaiting` wait a short window (e.g., `50ms`, well above a local pipe + immediate `SQLITE_BUSY` round-trip and well below the 120 s busy timeout) before reporting `no answer yet`. That makes the check falsifiable (an immediately-returning or `busy_timeout=0` child answers inside the window and fails the check) and ensures a blocked waiter actually reaches SQLite's `fcntl` + busy-retry loop before `holder` commits.
   - Precompute/cache the recipe plan in `sqlSession` (or in Part E `Begin` with `begin immediate`, verify `StillWaiting`, commit `holder`, `Finish`, then `recipe-exec` + `commit`) so `waiter` does not burn milliseconds regenerating steps `0..k` before calling `BEGIN IMMEDIATE`.
   - Add a test in `internal/procs/procs_test.go` verifying that `AnsweredWithin` returns `true` for an immediately-returning action and `false` for a blocked action.

2. **should-fix** — `ch06-locking-and-concurrency/experiments/6.1-locks-belong-to-the-process/main.go:51-53,84-100`, `internal/procs/lab.go:111-124`

   **What is wrong:**
   - Line 88–89 checks `procs.ReadLocked(dbBefore, "SHARED") && procs.Unlocked(dbBefore, "PENDING", "RESERVED")` for the claim `"on the database file both processes hold only the SHARED read lock: WAL mode does not use RESERVED for writers"`. `dbBefore` comes from `fcntl(F_GETLK)`, which returns at most one conflicting holder per range and cannot verify that *both* processes hold `SHARED`. The exact same predicate remains `true` after step 13 (`holder stray-close db`), when `holder`'s `SHARED` lock is gone and only `contender` holds it.
   - `Lab.Locks` prints `/proc/locks` entries on Linux (`lab.go:120-122`) but discards them and returns only `[]RangeState`. None of 6.1's 20 `lab.Check` assertions inspect `/proc/locks`, so a broken `ReadProcLocks` or `FilterProcLocks` returning an empty slice would still pass every check in 6.1.

   **How confirmed:** Read `6.1-locks-belong-to-the-process/main.go:51-100`, `internal/procs/lab.go:111-150`, and `evidence/6.1-locks-belong-to-the-process.txt:64-97` (the `F_GETLK` database-file block at lines 64–67 is identical to lines 91–94 after `stray-close db`; only the `/proc/locks` block distinguishes them).

   **Expected fix:** Return the filtered `[]ProcLock` alongside `[]RangeState` (or provide a `/proc/locks` helper on `Lab` / `procs`) and, when `procs.ProcLocksSupported` is true, assert in 6.1 that `/proc/locks` lists both `holder` and `contender` on `SHARED` before step 13, only `contender` after step 13, and `holder`'s locks (`RESERVED`+`SHARED` in Part A; `WAL_WRITE_LOCK`+`WAL_READ_LOCK(0)`+`DMS` in Part B) before the stray close and absent after it. On non-Linux, ensure the `F_GETLK`-only check's wording matches what `F_GETLK` actually proves.

3. **should-fix** — `internal/procs/sqlrole.go:443-458`

   **What is wrong:** `copierSession.copyRange` discards the byte count returned by `in.ReadAt(buffer, start)` while ignoring `io.EOF`:
   ```go
   buffer := make([]byte, end-start)
   if _, err := in.ReadAt(buffer, start); err != nil && !errors.Is(err, io.EOF) {
       return "", err
   }
   ```
   If the live file shrinks between `in.Stat()` (line 426) and `in.ReadAt` (line 444) — e.g., during a `TRUNCATE` checkpoint or `VACUUM` — `ReadAt` returns `(n, io.EOF)` with `n < len(buffer)` and `buffer[n:]` zero-filled. `out.WriteAt(buffer, start)` then writes trailing zero bytes past the actual read and reports `bytes start-end`.

   **How confirmed:** Code inspection of `internal/procs/sqlrole.go:426-458`.

   **Expected fix:** Capture `n, err := in.ReadAt(buffer, start)`, slice `buffer = buffer[:n]`, and return `fmt.Sprintf("bytes %d-%d", start, start+int64(n))`.

4. **nit** — `internal/procs/proclocks.go:92-131`

   **What is wrong:** `ReadProcLocks()` strictly parses every line in system-wide `/proc/locks`. In Linux `fs/locks.c` (`lock_get_status`), valid entries from unrelated host processes can contain `<none>:0` in the `dev:inode` column (when `inode == NULL`), mode `MSNFS`, or class/mode `UNKNOWN`. Any single such lock anywhere on a reader's Linux host causes `ReadProcLocks()` to fail with `ErrProcLocksFormat` and aborts experiment 6.1 with `FATAL`.

   **How confirmed:** Code inspection of `internal/procs/proclocks.go:92-131` (lines 115–118 require three `hex:hex:dec` fields; lines 93 and 99 omit `UNKNOWN` and `MSNFS`).

   **Expected fix:** Accept `<none>:0` (setting `DevMajor=0, DevMinor=0, Inode=0`) and `MSNFS` / `UNKNOWN` in `ParseProcLockLine` while continuing to reject malformed lines.

5. **nit** — `internal/procs/parent.go:123-143,294-301`

   **What is wrong:** In `Child.Stop()`, if `c.await` returns a non-timeout error (such as `ErrStepMismatch`), `Stop()` closes `c.requests`, sets `c.stopped = true`, and returns `err` without calling `c.Kill()` or waiting for `<-c.exited`, which can leave the child running while turning subsequent `Stop()` calls into no-ops. Additionally, `readReplies` (`parent.go:136`) sends unconditionally to `c.replies` (buffer 1), so if a misbehaving child writes two or more unconsumed lines before exiting and the parent calls `Kill()`, the `readReplies` goroutine leaks blocked on the channel send.

   **How confirmed:** Code inspection of `internal/procs/parent.go:123-143,294-301`.

   **Expected fix:** Call `c.Kill()` on any `c.await` error in `Stop()` (matching `StartCommand` at lines 106–108), and select on a done/closed channel in `readReplies` when sending to `c.replies`.

6. **nit** — `agent_work/ch06/build-notes.md:404` vs `ch06-locking-and-concurrency/experiments/6.3-torn-copy/main.go:112,126,128`

   **What is wrong:** `build-notes.md:404` states `"The copies are opened by the parent only after the children closed"`, but in `6.3-torn-copy/main.go` Part A (`walMode`), `examine` opens `main-only.db` (line 112) and `torn.db` (line 126) before `lab.StopChildren()` (line 128); only Part B (`rollbackMode`, lines 142–143) closes the children first. Because the copies are distinct inodes from `r.db`, no locks on `r.db` are affected, so no claim is undermined, but the note misstates the code's order for the prose pass.

   **How confirmed:** Code inspection of `ch06-locking-and-concurrency/experiments/6.3-torn-copy/main.go:108-145` and `agent_work/ch06/build-notes.md:404`.

   **Expected fix:** Align `build-notes.md:404` with `6.3-torn-copy/main.go` (or stop children before calling `examine` in Part A2 and note A1 opens the separate copy).

## Notes on verified items

- **Outline corrections (`agent_work/ch06/build-notes.md` items 1–5):** All five were checked directly against `/root/Software/sqlite-src/3.53.4` (`src/os.h:98-166`, `src/os_unix.c:1842-2095,6833-6869`, `src/btree.c:3740-3741`, `src/wal.c:1764-1792,2254-2351,2963-3057,3259-3272,3699-3746,3875-3915`). All five are accurate.
- **SQLite constants & spans:** Every constant in `internal/sqlitex/codes.go` matches `src/sqlite.h.in:449-546`. All 32 start and end lines in `ch06-locking-and-concurrency/SPANS.txt` match the pinned source.
- **Conventions:** No comments in `.go` files (only `//go:build` directives); no machine-specific paths outside `agent_work/`; WAL default followed with rollback contrasts; `6.1` prints the `LINUX-ONLY:` banner on non-Linux and uses portable `F_GETLK` on darwin.

## Commands run

- `git diff 208d5ad..HEAD --stat` — 33 files changed, 13,267 insertions.
- `gofmt -l internal/procs internal/sqlitex ch06-locking-and-concurrency` — clean (no output).
- `go build ./... && go vet ./...` — pass.
- `GOOS=darwin GOARCH=arm64 go build ./... && GOOS=darwin GOARCH=arm64 go vet ./...` — pass.
- `GOOS=darwin GOARCH=arm64 go test -c ./internal/procs` and `./internal/sqlitex` — pass.
- `GOOS=freebsd GOARCH=amd64 go build ./internal/procs && GOOS=freebsd GOARCH=amd64 go vet ./internal/procs` — pass (`*_other.go` builds cleanly).
- `go test -race -count=1 ./internal/procs ./internal/sqlitex` — pass (`internal/procs` 7.1s, `internal/sqlitex` 3.2s).
- `go run ./cmd/course check sources ch06` — PASS (2 copied files match the pin).
- `go run ./cmd/course check spans ch06` — PASS (32 spans start at their symbol); also verified all 32 start and end lines with `sed` against `$SQLITE_SRC`.
- `go run ./cmd/course check evidence ch06 -runs 2` — PASS (all 4 experiments identical to stored evidence across 2 runs).
- `go run ./ch06-locking-and-concurrency/experiments/6.4-live-snapshot -hold immediate` — exit 0, `LIVE SNAPSHOT OK`.
- `git diff --name-only 208d5ad..HEAD -- '*.go' | xargs grep -n '//\|/\*'` — only 3 `//go:build` lines.
- `git diff --name-only 208d5ad..HEAD | grep -v '^agent_work/' | xargs grep -n '/root\|/home/\|/Users/'` — only upstream comments in unmodified `os_unix.c`.
- `strace -f -e trace=fcntl /tmp/opencode/exp62` (3 runs, 3rd with `-tt`) — confirmed Finding 1: Part E rounds 3–5 (runs 1–2) and round 4 (run 3) had zero `EAGAIN` on `-shm` byte 120 because `holder` unlocked before `waiter` attempted `F_WRLCK`.
