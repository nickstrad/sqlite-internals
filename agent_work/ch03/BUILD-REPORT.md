# Chunk 3 build report — Chapter 3, B-trees and page management

Builder: Claude Opus 5.5, branch `chunk/ch03`, 2026-11-20. Details for the prose pass are in
`agent_work/ch03/build-notes.md`.

## What was built

| Path | What |
| --- | --- |
| `internal/equal/pages.go` (+ `pages_test.go`) | Level E2: page-by-page diff ignoring the declared header bytes 24-27, 40-43, 92-99 (`CompareImagesIgnoring`, `ComparePages`, `ComparePageFiles`, `ByteRange`, `DeclaredHeaderIgnore`) |
| `internal/equal/logical.go` (+ `logical_test.go`) | Level E3: the Go logical hash (schema, every row with explicit rowid, storage-class-tagged length-prefixed values, `sqlite_sequence`, persistent pragmas) plus integrity_check = ok and unchanged foreign_key_check; `WalkLogical` visitor reused by 3.3's capture |
| `internal/equal/changes.go` | `ChangedPages` between two manifests (used by 3.1) |
| `internal/equal/dbhash.go` | locate and run the pinned `dbhash` |
| `internal/equal/equal.go` | two new `Level` constants, two new `Result` fields, E2 output form (backward compatible) |
| `ch03-btrees-and-page-management/` | `btree.c`, `btreeInt.h` (whole, from the pin), `SOURCES.sha256`, `SPANS.txt` (22 spans), experiments 3.1, 3.2, 3.3, `evidence/` |
| `agent_work/ch03/` | `build-notes.md`, `INDEX.md`, this report |

No other shared package was changed. No go.mod change.

## Commands run

| Command | Result |
| --- | --- |
| `gofmt -l` on all changed/new files | empty |
| `go build ./... && go vet ./... && go test ./...` | pass |
| `agent_work/verify/crossbuild.sh` (linux/amd64 build+vet+test, darwin/arm64 and darwin/amd64 build+vet+test-compile, `check gofmt`) | `CROSSBUILD OK` |
| each experiment run twice by hand, non-INFO output compared | identical (3.1, 3.2, 3.3) |
| `go run ./cmd/course check evidence ch03` | PASS ×3 (2 runs each, identical to the evidence) |
| `go run ./cmd/course check sources ch03` | PASS (2 files match the pin) |
| `go run ./cmd/course check spans ch03` | PASS (22 spans) |
| `go run ./cmd/course check paths` | FAIL on 1 line in `_tools/bin/kb`, a binary tracked since commit c51ff44 (before this chunk); nothing from this chunk is flagged |
| `docker compose run --rm -T lab go run ./ch03-…/experiments/<name>` for all three | exit 0, EXACT output identical to the host |

Runtimes: 3.1 ≈ 31 s, 3.2 ≈ 3.5 s, 3.3 ≈ 6 s; all under 160 MB RSS. `_work/3.*` cleaned.

## Corrections to the outline (full evidence in build-notes.md)

1. In WAL mode, one transaction vs one transaction per statement gives **byte-identical** files
   for append-only builds; "three different sha256 values" for the three ways is false (two
   distinct). Insert order and history do change the bytes. In rollback mode the batching shows,
   but only in the change counter (E2 equal).
2. VACUUM INTO is canonical only for the same schema creation order and schema cookie: tables
   created in another order → different canonical files; a dropped scratch table or a backup-API
   copy (cookie set to old+1, backup.c:448) → canonical files differ at byte 43. The canonical
   file is in rollback mode.
3. dbhash has two blind spots beyond the four researched: values are not length-prefixed (`'a3','b'`
   = `'a','3b'`) and rows have no table boundary (a row moved between same-width tables). Our
   E3 hash tells all six apart (unit tests and 3.3 part B).
4. WAL frames shipped can exceed pages changed: a middle split wrote page 3 with identical bytes
   (6 frames, 5 changed pages). For 2061 appends and 200 indexed inserts the two sets are equal.
5. The first append split and the first depth change are the same insert in a table that starts
   empty (balance_deeper then balance_quick); 3.1 reports it, the first append split at depth 2,
   and the 2→3 depth change.
6. `balance_nonroot`'s header comment is at 8204-8244, before the outline's span 8245-9027; both
   listed. `btreeInt.h 149-228`: the comment ends at 210 (211-228 are the MX_CELL macros).

All [unprobed] claims of 3.1 hold, with the split points predicted by arithmetic and matched
exactly (first split at insert #5, every 4th after it, third level at insert #2053).

## Left undone

- sqldiff is not built by `build-tools`; its 1 vs 1.0 blind spot is cited from the research
  report, not re-run.
- The R2 dump in 3.3 refuses `sqlite_stat*` tables (only `sqlite_sequence` is handled).
- darwin: cross-built and cross-vetted only, never run.
- No prose or comments (seat rule); the chapter `.md` and `answers.md` are Neon's.
