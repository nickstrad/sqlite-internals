---
title: "Chapter 1: determinism rules and the Go-vs-C twin check, as measured"
summary: "What makes a SQLite file reproducible to the byte on modernc v1.60.1 and where the Go driver's I/O differs from the C CLI: WAL change counter moves only when page 1 is written, secure_delete must be ON not FAST, ANALYZE differs by STAT4, rolled-back savepoints and cache_size=0 change bytes."
tags: [sqlite, determinism, wal, modernc, walfleet]
updated: 2026-11-20
verified: 2026-11-20 — Linux amd64, Go 1.26, SQLite 3.53.4 via modernc.org/sqlite v1.60.1; evidence in the walfleet repo
---

# Chapter 1: determinism rules and the Go-vs-C twin check, as measured

What makes a SQLite file reproducible to the byte on modernc v1.60.1 and where the Go driver's I/O differs from the C CLI: WAL change counter moves only when page 1 is written, secure_delete must be ON not FAST, ANALYZE differs by STAT4, rolled-back savepoints and cache_size=0 change bytes.

Source: the `walfleet` repo (github.com/nickstrad/walfleet), chunk `ch01`. Working notes: `agent_work/ch01/build-notes.md`; review and adversarial records in the same directory.

## Corrections to the outline

1. **In WAL mode the file change counter counts connection sessions, not commits.** Outline §4.2
   says header counters (bytes 24–27, 92–95) count commits; that is true in rollback mode only.
   In WAL mode, `pagerWalFrames` (pager.c:3264) calls `pager_write_changecounter`, which writes
   `dbFileVers + 1` (pager.c:3135), but `dbFileVers` is refreshed only when page 1 is read into
   the cache (`readDbPage`, pager.c:3109–3110), not after a WAL commit. On one long-lived
   connection every commit writes the same value; each new connection re-reads page 1 and moves
   it by one. Evidence: 1.3 variant 6 — WAL, steps 0–6 one connection per step vs one connection
   for all: `differ: first differing page 1, ... page 1 bytes [27 95]`; rollback mode, same
   comparison: `identical`. Consequence for the recipe: **the canonical build opens and closes
   one connection per step** (`recipe.Build`), and `recipe.MatchStep` also reports a match that
   differs only in those 8 bytes (see the recipe section). This is a sixth source of variation the
   outline's §4.2 table does not list.
2. **"Same rows in different transaction batches give different bytes" does not hold in WAL mode
   with the recipe's settings.** WAL mode, one connection, `secure_delete=ON`, steps 2+3 and 4+5
   each committed as one transaction instead of two: `identical`. The batching difference shows
   (a) with `secure_delete=OFF`: `differ: first differing page 24, 2 page(s) differ [24 28]`,
   and every differing byte lies in space no cell uses (page 24: 357 bytes, page 28: 1207 bytes,
   both b-tree leaf pages, the gap between the cell-pointer array and the cell content area), or
   (b) in rollback mode: `differ ... page 1 bytes [27 95]` (the change counter counts commits
   there). Which pages show stale bytes depends on which steps are merged: merging 3+4 and 5+6
   gave `identical` even with `secure_delete=OFF` (tried while building; the experiment uses
   2+3 and 4+5, where step 3's deletes free space that step 2 wrote in the same transaction).
   The stale bytes are NOT on freelist pages here (freelist of both files: pages 29, 36, 37);
   the determinism report described the freelist-leaf case.
3. **An unordered `INSERT … SELECT` run twice gives identical files.** With the same build and
   the same plan the scan order is the same, so the rowids match: `identical`. The smallest
   change that shows the rule is a different scan order: run 2 with
   `PRAGMA reverse_unordered_selects=ON` → `differ: first differing page 29, 1 page(s) differ`;
   with `ORDER BY id` the same pair is `identical`. (A new index, `ANALYZE` data, or another
   SQLite version can change a plan the same way; those were not run.)
4. **`current_timestamp` has one-second resolution.** Two runs inside one wall-clock second gave
   `identical` files; a third run after the second changed gave `differ: first differing page
   59`. The experiment waits for a second boundary to show both; it retries if the two "same
   second" inserts straddle a boundary (never needed in the runs here).
5. **The twin check is not identical in every syscall:** modernc's libc is a translation of musl,
   and musl's `open()` issues `fcntl(fd, F_SETFD, FD_CLOEXEC)` after every open that asks for
   `O_CLOEXEC` (modernc.org/libc v1.77.1 `ccgo_linux_amd64.go:24802`). The C build does not
   (os_unix.c `robust_open` only calls F_SETFD when `O_CLOEXEC` is undefined, :867–868). In the
   INSERT: Go driver 2 extra calls in rollback mode (`fcntl t.db-journal F_SETFD`,
   `fcntl . F_SETFD`), 1 in WAL mode (`fcntl . F_SETFD`). Everything else matches: same
   writes, syncs, reads, locks, opens (by path), unlinks and closes, in the same order. Already
   known from chunk 0: `open` vs `openat`, and `fsync` (Go, installed CLI) vs `fdatasync`
   (`./configure` CLI). The experiment prints the F_SETFD difference on a `twin difference:` line.
6. **The commit's write lands on the database file only in rollback mode.** The outline's
   evidence line "(changed page number − 1) × 4096 equals the pwrite64 offset on the database
   file" is right for rollback mode only; in WAL mode the INSERT makes zero writes to `t.db` and
   the page goes to `t.db-wal` (frame header at 32, page image at 56). Both are shown.
7. **Rung R0 evidence uses the recipe's settings, which include `secure_delete=ON`.** The outline
   lists `secure_delete=ON` only "where two different histories must match"; the recipe always
   sets it, because later chapters drive the same steps through different histories (child
   processes, crashes, checkpoints at different times). Measured while building (scratch probe
   `agent_work/ch01/recipe-probe`): with `secure_delete=ON` the WAL recipe's final file is the
   same whether built with `wal_autocheckpoint` 0, 1 or 1000, `cache_size=5`, or
   `synchronous=NORMAL` (exactly, with one session per step; apart from the change counter with
   one connection); with `secure_delete=OFF` it differs.

Nothing else in the chapter-1 section of the outline was false here. WAL as the default made
sense for every part of this chapter; the only rollback-mode parts are the ones the brief asks
for (1.2's rollback table, and the rollback variants in 1.3 used to explain the WAL findings).

