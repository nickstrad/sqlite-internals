# Chunk 9 build report — Chapter 9, backup, crash recovery and integrity

Builder: Claude Opus 5.5, branch `chunk/ch09`, 2026-11-21. Details for the prose pass:
`agent_work/ch09/build-notes.md`.

## What was built

- `internal/walship` (new shared package, rung R6, with tests): a small object-store interface
  (`PutIfAbsent`, `Get`, `List`, `Delete`; semantics written down in the notes for chapter 10's
  SeaweedFS adapter) and a local-directory implementation (`DirStore`, atomic put-if-absent by
  `link`); snapshot and segment objects (one atomic put each, JSON metadata + raw body, strict
  decoding); a shipper that holds a read mark, runs each cycle under `BEGIN IMMEDIATE`, reads
  committed frames from the `-wal` file into hash-chained segments (positions, salts/generation,
  first and last frame, seed and last WAL checksum, per-commit state hash and `user_version`,
  before/after state hash), runs its own PASSIVE checkpoint only after shipping, detects a WAL
  header change it cannot vouch for and takes a discontinuity snapshot, takes a periodic snapshot
  every *m* segments (checked against its replayed state), and resumes from the store; a catalog
  and planner that verify the whole chain (keys, body hashes, SQLite's checksum chain, state
  hashes, WAL continuity) before any page is applied; restore through chapter 5's
  `walfile.Replay`; `Prune`, `CopyStore`, `WriteDatabase`, `ReadPages`.
- Backward-compatible additions to shared packages (each with tests): `procs` SQL-role action
  `recipe-session`; `sqlitex.BackupWithSourceExec` + `SourceExecer`; `dbfile.RecordColumn`;
  `capture.VerifyImage`; `trace.SyncsOf`. Nothing else changed; `go.mod` unchanged.
- `ch09-backup-crash-recovery-integrity/`: `backup.c`, `vacuum.c` copied whole; `SOURCES.sha256`;
  `SPANS.txt` (24 spans: the outline's 7, `backup.c 688-734` split into its 3 functions, 15 more
  cited by the notes); experiments 9.1 capture-comparison, 9.2 crash-versus-power-loss
  (`-portable`), 9.3 integrity-check-limits, 9.4 point-in-time-rebuild; `evidence/` for all four.
- `agent_work/ch09/build-notes.md`, `INDEX.md`, this report;
  `docs/knowledge/sqlite-internals-ch09-findings.md`.

## Commands run

| command | result |
| --- | --- |
| `gofmt -l` on every file of this chunk; `go run ./cmd/course check gofmt` | nothing listed; PASS, 145 files |
| `go build ./... && go vet ./...` | pass |
| `go test ./...` | pass (`internal/walship` ~10-18 s, all packages ~40 s) |
| `GOOS=darwin GOARCH=arm64 go build ./... && go vet ./...`; walship test binary compile for darwin | pass |
| `go run ./cmd/course check sources` / `check spans` | PASS for every chapter (ch09: 2 copies, 24 spans) |
| `go run ./cmd/course check evidence ch09` (each experiment built once, run twice, compared with `evidence/`) | 4 × PASS: 2 runs identical to the evidence |
| `go run ./ch09-…/experiments/9.2-crash-versus-power-loss -portable` (the Mac code path on Linux) | exit 0, `CRASH VERSUS POWER LOSS OK`, LINUX-ONLY lines as designed |
| `docker compose run --rm -T lab go run ./ch09-…/experiments/<each>` (Debian lab container) | all 4 exit 0; output identical to the host evidence apart from `INFO:` lines (kill sweep included) |
| `go run ./cmd/course check paths` | FAIL on one pre-existing tracked file, `_tools/bin/kb` (a binary committed by the infra commit c51ff44, matched inside its bytes); nothing from this chunk |
| comment check: no `//` comment line added in any `.go` file by this chunk (`git diff 19ed2d6..HEAD`) | none |
| `agent_work/verify/crossbuild.sh` (linux/amd64 build+vet+test; darwin/arm64 and darwin/amd64 build+vet+test-compile; gofmt) | `CROSSBUILD OK` |

Runtimes (this VM, shared): 9.1 ~5 s, 9.2 ~23 s (≈3 s with `-portable`), 9.3 ~1.5 s, 9.4 ~4.5 s.
`_work/9.*` cleaned after the runs.

## Corrections to the outline (full text in build-notes.md)

1. 9.1: the backup's *remaining* count does not fall steadily with same-connection writes — it
   rose when those commits grew the database; the copied count (`pagecount − remaining`) is what
   advances steadily, and it falls back to the step size after another connection's commit.
2. 9.1 / §4.2: `VACUUM INTO` of a WAL database differs from its source in `journal_mode` (rollback
   file), so it reaches "E3 except journal_mode", E1 between canonical forms.
3. 9.1: backup-API E2 differences are bytes 27, 43, 95 (inside the declared ranges).
4. 9.2(b): the model is computed from the sync rule read from pager.c/wal.c (portable) and
   cross-checked against strace's record on Linux (they agree at all three levels).
5. 9.2(b): at `synchronous=OFF` the model does not apply — the database file is written by the
   checkpoint with no sync at all.
6. 9.2(a): recovered step is the last acknowledged step or one more (a kill inside a FULL commit
   sync keeps that commit).
7. 9.4: a read mark on slot 0 does not block a WAL restart; walship relies on this safely.
8. 9.4: the WAL checkpoint sequence and salt-1 + 1 are per-connection behaviour, not a restart
   counter.
9. Spans: all outline spans exact; `backup.c 688-734` is three functions.
10. The research report's CLI `.backup` "database is locked" under writers was not reproduced;
    the notes give the source-based cause for the adversarial pass.

## Left undone

- Not reproduced: CLI `.backup` "database is locked" (correction 10).
- From Neon's lists, not built: thousands of random-time `kill -9`s; a kill during recovery;
  cutting the WAL mid-frame in the 9.2 model; SIGKILL of the shipper at every point (unit tests
  cover close+reopen resume in both directions); a seeded random-offset sweep for 9.3.
- 9.1 is 450 lines (others 207-369); its reference-file and level-comparison helpers could move to
  a shared package if the chapter needs it shorter.
- darwin: cross-built and cross-vetted only, never run.
