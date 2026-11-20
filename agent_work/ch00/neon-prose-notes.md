# Neon prose pass notes — ch00 (Foundation)

## What was written

Added package doc comments, Go doc comments on every exported identifier (and interface methods on unexported helper types), explanatory block comments citing the pinned SQLite source (`$SQLITE_SRC` or `_src/sqlite-3.53.4`) and build-notes findings, and one-line test comments across all 29 Go files in scope:

- `sqlite/` (2 files): `pin.go`, `pin_test.go`
- `internal/sqlitex/` (7 files): `settings.go`, `open.go`, `pin.go`, `raw.go`, `workdir.go`, `cli.go`, `sqlitex_test.go`
- `internal/trace/` (8 files): `linuxonly.go`, `strace.go`, `strace_linux.go`, `strace_other.go`, `vfs.go`, `merge.go`, `trace_test.go`, `strace_linux_test.go`
- `internal/repocheck/` (9 files): `repo.go`, `sources.go`, `spans.go`, `inline.go`, `comments.go`, `prose.go`, `evidence.go`, `sweep.go`, `repocheck_test.go`
- `cmd/course/` (3 files): `main.go`, `checks.go`, `source.go`

No code tokens, directives (`//go:embed`, `//go:build`), evidence files, or off-limits files were modified.

## Verification run

- Token-stream comparison against `HEAD` (same logic as `repocheck.SameCode`, preserving directives): all 29 modified `.go` files have identical token streams.
- AST check: every exported declaration and test function has a doc comment.
- `gofmt -l .`: printed none of the 29 edited files.
- `go run ./cmd/course check gofmt`: `PASS check gofmt: 30 Go file(s) formatted`.
- `go build ./... && go vet ./...`: passed.
- `GOOS=darwin GOARCH=arm64 go build ./... && GOOS=darwin GOARCH=arm64 go vet ./...`: passed.
- `go run ./cmd/course check paths`: zero issues in any edited file (only the 9 pre-existing lines in `OUTLINE.md` and `docs/knowledge/`).
- `go test -count=1 ./sqlite/... ./internal/...`: passed (see Suspected defects below for one intermittent failure observed on an earlier run of `TestConcurrentOpenOfNewDatabase`).

## Open doubts

- The brief referenced "section 8 (rulings)" of `OUTLINE.md`, but `OUTLINE.md` in this worktree ends at section 7 (line 1130). All rulings recorded in section 6 and in `agent_work/ch00/build-notes.md` were followed.
- `internal/sqlitex/open.go` and `internal/sqlitex/raw.go` cite SQLite header byte offsets (bytes 18–19 for WAL/rollback format, bytes 68–71 for `application_id`, bytes 96–99 for library version) and `shell.c.in` / `ext/misc/vfstrace.c` line numbers from `agent_work/ch00/build-notes.md` and `OUTLINE.md`.

## Suspected defects

1. **Flaky `SQLITE_BUSY` in `sqlitex.Open` under concurrent creation (`internal/sqlitex/open.go`, `TestConcurrentOpenOfNewDatabase`).**
   During verification, one run of `go test ./internal/sqlitex` failed in `TestConcurrentOpenOfNewDatabase` with:
   `open .../race.db: PRAGMA journal_mode=wal: database is locked (5) (SQLITE_BUSY)` (failed in 0.13s, well under the 5-second `busy_timeout`; a subsequent `-count=10` run passed 10/10).
   Likely cause: when multiple goroutines open a brand-new database simultaneously, `configureConnection` runs `PRAGMA page_count`, creation pragmas, and `PRAGMA journal_mode=wal` outside any serialized lock or retry loop; when two connections race on switching an empty file into WAL mode, SQLite can encounter a lock-upgrade deadlock (`SQLITE_BUSY` returned immediately without invoking the busy handler).

2. **Minor unused parameters (non-functional):**
   - `internal/sqlitex/open.go`: `readConnSettings(ctx context.Context, ...)` does not use `ctx`.
   - `internal/repocheck/spans.go`: `CheckSpan(tree string, span Span, lines []string)` does not use `tree`.
   - `internal/repocheck/prose.go`: `markdownProseLines(path string, src []byte)` does not use `path`.
