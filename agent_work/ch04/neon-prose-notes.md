# Neon prose notes — chunk ch04

## What was written

### Deliverable A — Go code comments (comment-only edits)
- `internal/journalfile/journalfile.go`: package doc comment, doc comments on every exported identifier (`Magic`, `HeaderFieldsSize`, `NoSyncRecordCount`, `RecordOverhead`, `HeaderState` and all six state constants, `HeaderState.String`, `Header`, `Header.CountsRecords`, `Record`, `Record.ChecksumOK`, `Segment`, `Journal`, `PlaybackOutcome`, `ReadFile`, `Parse`, `Checksum`, `SuperJournalPage`, `Journal.LooksHot`, `Journal.RecordsPresent`, `Journal.ChecksumFailures`, `Journal.Playback`), and WHY comments citing `src/pager.c` line spans (`aJournalMagic`, `writeJournalHdr`, `readJournalHdr`, `journalHdrOffset`, `zeroJournalHdr`, `syncJournal`, `pager_cksum`, `hasHotJournal`, `pager_playback`, `pager_playback_one_page`, `pagerAddPageToRollbackJournal`).
- `internal/journalfile/journalfile_test.go`: one-line doc comment on each of the 13 `Test*` functions plus a WHY comment on why reading `-journal` beside an open SQLite connection is safe on POSIX.
- `ch04-pager-and-page-cache/experiments/4.1-atomic-commit-and-hot-journal/main.go`: package doc comment and WHY comments on `init()` (`runtime.LockOSThread`), marker files, `newStates` (`PAGER_GET_NOCONTENT`, `src/btree.c:6740`, `src/pager.c:5645-5656`, and `VACUUM INTO` canonical hashing), `outcomeOf`, `classify`, `extractWindow`, `expectedSequence`, `runKill`, `twinCheck`, `covered`, `partA`, `partC`, `partE`, and `partF`.
- `ch04-pager-and-page-cache/experiments/4.2-cache-budget/main.go`: package doc comment and WHY comments on `cachedPages` (`sqlite3PagerMemUsed`, `src/pager.c:6881-6887` path-length dependence), `budgetBytes` (`numberOfCachePages`, `src/pcache.c:277-292`), `buildSource`, `measure`, `runChild`, the `cache_size=100` vs `200` per-page measurement (`nPage+1 >= nMax`, `src/pcache1.c:902`), `ENABLE_MEMORY_MANAGEMENT` unified `PGroup` LRU (`src/pcache1.c:718-719`, `814-832`, `899-912`), and Part E `mmap` (`pagerFixMaplimit`, `src/pager.c:3579-3590`, `getPageMMap`, `src/pager.c:5687-5752`).
- `ch04-pager-and-page-cache/experiments/4.2-cache-budget/memory_linux.go`: preserved `//go:build linux` and added doc comment on `readProcessMemory`.
- `ch04-pager-and-page-cache/experiments/4.2-cache-budget/memory_other.go`: preserved `//go:build !linux` and added doc comment on `readProcessMemory`.

Verification: `gofmt -l` clean (`PASS check gofmt: 88 Go file(s) formatted`), `go build ./...`, `go vet ./...`, `go test ./internal/journalfile/...`, and `GOOS=darwin GOARCH=arm64 go build ./... && go vet ./...` all pass; `git diff` confirms zero non-comment token changes across all six `.go` files.

### Deliverable B — Chapter prose and answers
- `ch04-pager-and-page-cache/ch04-pager-and-page-cache.md`:
  - Title, pin block from `sqlite/PIN`, and "How to run the experiments" box (`fetch-source`, `build-tools`, **Linux only** notes and Docker `lab` commands for macOS, measured Linux platform disclosure).
  - All eight required sections (`1. Model` with architecture/state-machine diagram and WAL-default evaluation, `2. Question`, `3. Call path`, `4. Timeline`, `5. Lab`, `6. Evidence and failure cases` ending with `### What attacked this chapter` and `<!-- adversarial-results -->`, `7. Platform implications`, `8. Knowledge check`).
  - All 44 spans in `ch04-pager-and-page-cache/SPANS.txt` accounted for with `version-3.53.4` GitHub links and local copy references (`pager.c`, `pcache.c`, `pcache1.c`, `status.c`), plus `src/btree.c:6740` (verified in Chapter 2's copy) and `src/pcache1.c:717-724` / `814-832` (verified in this chapter's copy).
  - All eight corrections from `agent_work/ch04/build-notes.md` explained with evidence (freelist `PAGER_GET_NOCONTENT` bytes after rollback, 12-byte `journal-count` write and 4/3/0 sync counts, cold vs hot journals before `syncJournal`, WAL-switch rollback commit inside `sqlitex.Open`, `ENABLE_MEMORY_MANAGEMENT` unified `PGroup` LRU, `modernc.org/memory` ~2x RSS multiplier, `CACHE_USED` path-length dependence, and `N-1` cache capacity).
  - `internal/journalfile/journalfile.go`, Experiment 4.1 (`main.go` + evidence), and Experiment 4.2 (`main.go`, `memory_linux.go`, `memory_other.go` + evidence) inlined via `go run ./cmd/course inline` (7 include blocks; `inline -check` passes). Earlier shared packages (`sqlitex`, `trace`, `recipe`, `dbfile`) are linked, not inlined.
- `ch04-pager-and-page-cache/answers.md`: complete answers to all five knowledge-check questions.

## Open doubts

- **Include-marker path base.** As in Chapters 1 and 2, `internal/repocheck/inline.go` resolves `<!-- include: ... -->` targets relative to `filepath.Dir(markdownPath)` (`ch04-pager-and-page-cache/`), so the chapter uses `../internal/journalfile/journalfile.go`, `experiments/...`, and `evidence/...` rather than repo-root paths. `go run ./cmd/course inline -check` passes on all 7 blocks.
- **`internal/journalfile/journalfile_test.go` not inlined.** Following Chapters 1 and 2, `journalfile.go` is inlined in full in Section 5.1 while `journalfile_test.go` is linked with its `go test ./internal/journalfile/` command.
- **`check sources` / `check spans` in this shell.** `$SQLITE_SRC` / `_src/sqlite-3.53.4` is not present in this worktree, so `go run ./cmd/course check sources ch04` could not run here; every cited line number and excerpt was verified directly against the copied C files in `ch04-pager-and-page-cache/` (and `ch02-file-format-and-pages/btree.c` for line 6740).

## Suspected defects

None observed in `internal/journalfile/` or `ch04-pager-and-page-cache/experiments/`.
