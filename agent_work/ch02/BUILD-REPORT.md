# Chunk 2 build report

Builder: Claude Opus 5.5, branch `chunk/ch02`, 2026-11-20. Base: `8f59174`.

## What was built

| Item | Where |
| --- | --- |
| Chapter directory | `ch02-file-format-and-pages/`: `btreeInt.h`, `btree.c`, `util.c` (whole copies), `SOURCES.sha256`, `SPANS.txt` (18 spans), `experiments/`, `evidence/` |
| `internal/dbfile` (+tests, fuzz target) | read-only parser: header, page count rule, b-tree pages, cells and local/overflow split, varints, records, schema table, overflow chains, freelist trunks/leaves, pointer-map pages (auto-vacuum handled), lock-byte page, page census with roles and `dbstat` columns; typed errors, never panics |
| `internal/equal` (+tests) | level E1: sha256, page diff with first differing page, page manifest (text format, parse, sequence check naming the page, manifest compare); `Level`/`Result` shaped for E2/E3 |
| `internal/capture` (+tests) | new shared package for rung R1: at-rest check (WAL content, hot journal), page capture (`pages.bin` + `manifest.txt`), verified rebuild |
| Experiment 2.1 header-and-what-a-commit-changes | header vs PRAGMA and CLI `.dbinfo`; rollback commits; WAL commits (one connection, no page-1 writes, new connections); bytes 18-19; in-header size rule; page sizes 512/4096/65536 |
| Experiment 2.2 page-census | R0 census vs `dbstat`, pragmas and SELECT row hashes; local limit ±1; multi-trunk freelist; auto-vacuum pointer maps; > 1 GiB sparse file with the lock-byte page; nine damaged files |
| Experiment 2.3 page-manifest (R1) | two identical manifests; rebuild E1-equal, integrity ok, recipe step 30; five damaged captures refused naming the page; WAL-with-content and hot-journal databases refused, with the stale/torn content shown |
| Notes | `agent_work/ch02/build-notes.md`; scripts indexed in `agent_work/ch02/INDEX.md` |

No existing shared package was changed.

## Commands run

| Command | Result |
| --- | --- |
| `gofmt -l` on all chunk files | nothing listed (pass) |
| `go build ./... && go vet ./... && go test ./...` | pass |
| `bash agent_work/verify/crossbuild.sh` (linux/amd64, darwin/arm64, darwin/amd64 build + vet + test compile, `check gofmt`) | `CROSSBUILD OK` |
| `go run ./cmd/course check sources ch02` | PASS (3 files) |
| `go run ./cmd/course check spans ch02` | PASS (18 spans) |
| `go run ./cmd/course check evidence ch02` (2 runs each) | PASS for 2.1, 2.2, 2.3 |
| each experiment run twice by hand, EXACT output compared | identical (2.1 197 lines, 2.2 388, 2.3 336) |
| the three experiments in the Debian `lab` container | exit 0, non-INFO output identical to the evidence (md5) |
| 2.1 with `SQLITE3_BIN=/nonexistent` | `NO-CLI:` line, `.dbinfo` column `?`, exit 0 |
| `go test -fuzz FuzzParse -fuzztime 30s -fuzzminimizetime 0 ./internal/dbfile/` | 85,919 inputs, no failure |
| `go run ./cmd/course check all` | ch02 parts pass. Two FAILs not from this chunk: `inline -check` of the ch01 chapter (8 blocks out of date since ch01's repair round; chapter markdown is Neon's) and `check paths` on `_tools/bin/kb` (a local binary) |

Runtimes: 2.1 4.2 s, 2.2 2.0 s, 2.3 4.9 s; `go test` of the three new packages about 10 s.
`_work/` cleaned after the runs.

## Corrections to the outline (details in build-notes.md)

1. WAL mode: the header is not a per-commit fingerprint and the database file does not change
   at commit; the counter is written only with page 1 and equals (counter when this connection
   last read page 1) + 1. Commits that skip page 1 change no header byte at all.
2. The probe's "counter constant on one connection" is true after the first page-1 commit only.
3. A file with a partial last page: SQLite says `integrity_check ok`; our parser refuses it
   (deliberately stricter).
4. Span `btreeParseCellPtr 1259-1385` covers two functions; split in SPANS.txt. Four spans added.
5. "Not at rest" defined precisely: `-wal` size > 0 or a hot `-journal` (first byte non-zero).
6. An interrupted rollback transaction leaves a hot journal only after a sync (commit or cache
   spill); 2.3 makes one with a cache spill.

## Left undone

- E2 and E3 (chunk 3, by design).
- The parser does not check key order, cell overlap or pointer-map entries (integrity_check's job).
- Prose, comments, knowledge-check questions: Neon's.
