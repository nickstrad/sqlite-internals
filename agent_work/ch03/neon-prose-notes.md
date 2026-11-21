# Neon closing prose pass notes — ch03 (Chapter 3 — B-trees and page management)

## What was written

### Deliverable A — Go code comments (comments only; token streams verified identical against `HEAD`)
- `internal/equal/equal.go`: added doc comments for `E2Pages`, `E3Logical`, and updated `Result` doc comment for `Ignored` and `IgnoredDiffering`.
- `internal/equal/changes.go`: doc comments on `PageChange`, `Added`, `Removed`, `String`, `ChangedPages`, `FormatPageChanges`, `PageNumbers`, plus WHY comment citing `freePage2` (`btree.c:6934-6935`) and `sqlite3PagerDontWrite` (`pager.c:6329-6339`).
- `internal/equal/pages.go`: doc comments on `ByteRange`, `Contains`, `String`, `DeclaredHeaderIgnore`, `FormatRanges`, `ValidateRanges`, `CompareImagesIgnoring`, `ComparePages`, `ComparePageFiles`, plus WHY comments citing `pager.c:3130-3143`, `backup.c:448`, `vacuum.c:143-427`, and why header bytes 18–19 are not ignored.
- `internal/equal/logical.go`: doc comments on every exported identifier (`LogicalFormat`, `LogicalPragmas`, `SchemaRow`, `TableInfo`, `LogicalVisitor`, `QuoteIdentifier`, `RowsQuerier`, `WalkLogical`, `TableSelect`, `StorageClassOf`, `LogicalTableDigest`, `LogicalPragma`, `Logical`, `TableByName`, `Pragma`, `Summary`, `EncodeValue`, `LogicalHash`, `ErrNoDatabase`, `OpenExisting`, `HasLiveSideFile`, `LogicalHashFile`, `LogicalResult`, `String`, `CompareLogical`, `CompareLogicalFiles`) and WHY comments on `main.` qualification, unary `+`, IPK rowid fallback, `context.WithoutCancel(ctx)`, `HasLiveSideFile` `mode=ro` vs at-rest `mode=rw`, and length-prefixed framing (`tool/dbhash.c:281-360`).
- `internal/equal/dbhash.go`: doc comments on `ErrNoDBHash`, `LocateDBHash`, `RunDBHash`, plus WHY comments citing `tool/dbhash.c:433-440`.
- `internal/equal/pages_test.go`, `internal/equal/logical_test.go`, `internal/equal/repair_test.go`: one-line comment per test function.
- `ch03-btrees-and-page-management/experiments/3.1-pages-per-insert/main.go`, `3.2-delete-and-reuse/main.go`, `3.3-same-rows-different-bytes/main.go`: package doc comments and WHY comments citing pinned `btreeInt.h`, `btree.c`, `pager.c`, `backup.c`, `vacuum.c`, and `tool/dbhash.c` spans.
- `ch03-btrees-and-page-management/experiments/3.3-same-rows-different-bytes/main_test.go`: one-line comment per test function.

### Deliverable B — Chapter prose and answers
- `ch03-btrees-and-page-management/ch03-btrees-and-page-management.md`: complete chapter matching the pin block, "How to run the experiments" callout, exact eight `## N. ` sections, text diagrams in Sections 1 and 4, all 22 verified source spans from `SPANS.txt` with `version-3.53.4` GitHub links and local-copy mentions, Version notes, **Why kept** line on Experiment 3.1, WAL-default evaluation (ruling 10), all 10 verified outline corrections incorporated in place and summarized in Section 6.2, full closing account after `<!-- adversarial-results -->` under `### What attacked this chapter` (all 4 attack programs, Run 1 16/14, Run 2 22/8, Run 3 30/0), evidence-tied platform implications in Section 7, and 5 knowledge-check questions in Section 8. All 10 include blocks (`3.1`, `3.2`, `3.3` code and evidence + `internal/equal/{changes,pages,logical,dbhash}.go`) populated via `go run ./cmd/course inline` and verified with `inline -check`.
- `ch03-btrees-and-page-management/answers.md`: complete, evidence-grounded answers to all 5 knowledge-check questions.

## Verification run
- `go run ./cmd/course inline ch03-btrees-and-page-management/ch03-btrees-and-page-management.md` — PASS (10 include blocks rewritten).
- `go run ./cmd/course inline -check ch03-btrees-and-page-management/ch03-btrees-and-page-management.md` — PASS (10 include blocks match).
- `go run ./cmd/course check sources ch03` — PASS (2 copied files match the pin).
- `go run ./cmd/course check spans ch03` — PASS (22 spans start at their symbol).
- `go run ./cmd/course check gofmt` — PASS (155 Go files formatted); `gofmt -l internal/equal ch03-btrees-and-page-management` empty.
- `go build ./... && go vet ./...` and `GOOS=darwin GOARCH=arm64 go build ./... && GOOS=darwin GOARCH=arm64 go vet ./...` — PASS.
- `go test -count=1 ./internal/equal ./ch03-btrees-and-page-management/...` — PASS.
- Comment-stripped token comparison against `HEAD` on all 12 modified `.go` files — all 12 identical.
- Machine-path grep (`/root|vm-1`) across `ch03-btrees-and-page-management/` and `internal/equal/` — 0 matches.

## Open doubts

None. Every claim, number, and span in the chapter matches the post-repair evidence files, `SPANS.txt`, the copied C sources, and the final adversarial run (`30 HELD, 0 BROKEN`).

## Suspected defects

None found in the repaired code.
