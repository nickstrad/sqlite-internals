VERDICT: FAIL

1. **blocker** — `internal/repocheck/paths.go:177-200` (`SweepPaths`)
   - **What is wrong:** `SweepPaths` scans every tracked non-binary file outside `PathSweepExclude` (`agent_work`, `kb`, `STATE.md`) for `/home/<name>/` and `/Users/<name>/`, including verbatim upstream SQLite C source files copied into chapter directories under `SOURCES.sha256`. Upstream SQLite 3.53.4 comments in `pager.c`, `os_unix.c`, and `vfstrace.c` contain illustrative paths (`/home/bill/`, `/home/user1/`, `/Users/me/`, `/home/drh/`). Because `course check sources` enforces that these copies remain byte-identical to the pinned SQLite tree, they cannot be edited, so `course check paths` and `course check all` fail permanently on the current branch (and will also fail on future chapters copying those files). Scanning upstream C comments also injects upstream URLs (`bugzilla.mozilla.org`, `nabble.com`, `android-review.googlesource.com`, `nfs.sourceforge.net`, `github.com/rsc/fpfmt`) into the `EXTERNAL` report.
   - **How confirmed:**
     ```text
     $ go run ./cmd/course check paths
     ...
     FAIL check paths: 5 problem(s)
       ch04-pager-and-page-cache/pager.c:2551: mentions a machine-local path /home/bill/: **   "/home/bill/a.db-journal\x00/home/bill/b.db-journal\x00"
       ch07-vfs-and-filesystem-io/os_unix.c:4924: mentions a machine-local path /home/user1/: ** is "/home/user1/config.db" then the file that is created and mmapped
       ch07-vfs-and-filesystem-io/os_unix.c:4925: mentions a machine-local path /home/user1/: ** for shared memory will be called "/home/user1/config.db-shm".
       ch07-vfs-and-filesystem-io/os_unix.c:7323: mentions a machine-local path /Users/me/: **       For database path "/Users/me/foo.db"
       ch07-vfs-and-filesystem-io/vfstrace.c:236: mentions a machine-local path /home/drh/: **     /home/drh/xyzzy.txt -> xyzzy.txt
     exit status 1
     ```
     Confirmed identically via `export SQLITE_SRC=/root/Software/sqlite-src/3.53.4 && go run ./cmd/course check all -skip-evidence` (exited 1 on `check paths` while all other checks passed).
   - **Expected fix:** In `SweepPaths`, collect the set of upstream copied files listed in tracked `chNN-*/SOURCES.sha256` manifests (via `ParseSources`, already in `internal/repocheck/sources.go`, mapping each entry to `<chapterDir>/<path.Base(entry.Path)>`). Keep those files in `fileSet` so markdown links to them still resolve, but skip reading/scanning them for machine paths and external URLs. Add a unit test in `internal/repocheck/paths_test.go` proving that a file declared in `SOURCES.sha256` containing `/home/drh/` is skipped while an undeclared file containing `/home/drh/` still fails.

2. **should-fix** — `internal/repocheck/paths.go:114-131,196` (`fenceState.inCode` and `SweepPaths`)
   - **What is wrong:** Two CommonMark fence-tracking bugs leave `fence.inCode(line)` permanently `true` for the remainder of a markdown file, silently hiding every broken relative link after that point:
     1. Line 196 splits on `"\n"` without stripping `"\r"`, and line 118 tests `strings.Trim(trimmed, f.marker[:1]+" \t") == ""`. On a CRLF (`\r\n`) markdown file, a closing fence line ```` "```\r" ```` trims to `"\r" != ""`, so the fence never closes.
     2. Lines 123–129 open a backtick fence whenever `trimmed` starts with ```` ``` ````, even when the rest of the line contains backticks (a CommonMark inline code span such as ```` ```inline code``` and [broken](missing.md) ````). Per CommonMark §4.5, a backtick fence's info string may not contain backticks; opening a fence here leaves `f.marker` set until EOF.
   - **How confirmed:** Built `/tmp/opencode/course` (`go build -o /tmp/opencode/course ./cmd/course`) and ran `course check paths` on two temporary git repos:
     - `README.md` containing `# T\r\n\r\n` + ```` ```go\r\nfmt.Println()\r\n```\r\n\r\n[broken-after-crlf-fence](missing.md)\r\n ```` → `rc=0`, `PASS check paths: 3 tracked text file(s) free of machine-local paths and unresolved relative links`.
     - `README.md` containing ```` ```inline code``` and [broken1](missing1.md)\n\nNext paragraph [broken2](missing2.md)\n ```` → `rc=0`, `PASS check paths: 3 tracked text file(s) free of machine-local paths and unresolved relative links`.
   - **Expected fix:** Either parse markdown files with `github.com/yuin/goldmark` (already a direct dependency in `go.mod`) and inspect `*ast.Link` / `*ast.Image` nodes, or in `fenceState.inCode` strip trailing `\r` (`strings.TrimRight(line, "\r")`) and refuse to open a backtick fence if the info string after the opening backtick run contains `` ` ``. Add unit tests for both cases in `internal/repocheck/paths_test.go`.

3. **should-fix** — `internal/repocheck/paths.go:33-36,95-108` (`MarkdownLinks`)
   - **What is wrong:** The regexes in `MarkdownLinks` fail on four valid CommonMark constructs:
     1. `inlineCodePattern` (`` (`+)[^`]*?(`+) ``) cannot enforce matching backtick lengths in Go's RE2. On a double-backtick span enclosing a literal backtick (e.g. ``Use `` ` `` and see [broken](missing.md) and `other`.``), the first match consumes `` `` ` `` and the second consumes `` `` and see [broken](missing.md) and ` ``, erasing `[broken](missing.md)` so it is never checked.
     2. `inlineLinkPattern` only permits double-quoted titles (`"..."`), silently missing broken links with single-quoted titles (`[b](missing-sq.md 'title')`).
     3. Both `inlineLinkPattern` (`<?...>?`) and `angleLinkPattern` match `[a](<missing-angle.md>)`, duplicating the problem report.
     4. `referenceLinkPattern` matches `<?(\S+?)>?`, truncating valid angle-bracket reference targets containing spaces (`[ref]: <my file.md>`) to `my` and falsely failing even when `my file.md` exists.
   - **How confirmed:** Ran `/tmp/opencode/course check paths` on two temporary git repos:
     - `README.md` with ``Use `` ` `` and see [broken](missing.md) and `other`.`` → `rc=0`, `PASS check paths` (`missing.md` missed).
     - `README.md` with `[a](<missing-angle.md>) [b](missing-sq.md 'title') [c][ref]\n\n[ref]: <my file.md>` and tracked `my file.md` →
       ```text
       FAIL check paths: 3 problem(s)
         README.md:3: relative link missing-angle.md does not resolve in the repository (missing-angle.md)
         README.md:3: relative link missing-angle.md does not resolve in the repository (missing-angle.md)
         README.md:5: relative link my does not resolve in the repository (my)
       ```
       (`missing-angle.md` reported twice, `missing-sq.md` missed, valid `my file.md` falsely reported as `my`).
   - **Expected fix:** Prefer extracting links via `goldmark`'s AST (which fixes findings 2 and 3 together cleanly), or fix the regex/scanner to match exact backtick run lengths, drop `<?...>?` from `inlineLinkPattern`, accept single-quoted/parenthesized titles, and handle `<[^<>\n]+>` in `referenceLinkPattern`. Add unit tests covering all four cases.

4. **should-fix** — `internal/site/check.go:17,85-90,105,122-125` (`checkLink`)
   - **What is wrong:** When a rooted link contains a repeated slash after the base path (e.g. `/book//ch01-a/#no-such-anchor`), line 90 trims only one leading `/`, leaving `target` as `"/ch01-a/index.html"`. `filepath.Join(dir, "/ch01-a/index.html")` ignores the leading slash and `os.Stat` succeeds, so the link is not flagged as broken; then `ids["/ch01-a/index.html"]` misses the key `"ch01-a/index.html"` and lines 122–125 return `""`, silently skipping anchor verification. Additionally, `linkAttrPattern` on line 17 only matches double-quoted attributes, so raw HTML links using single quotes (`<a href='missing-single.html'>`) are never checked even though `html.WithUnsafe()` passes raw HTML through.
   - **How confirmed:** Ran `/tmp/opencode/course site -check -base /book/` on a temporary repo whose `README.md` contained `<a href="/book//ch01-a/#no-such-anchor">dbl</a>`, `<a href='missing-single.html'>sq</a>`, and `[empty]()`:
     ```text
     FAIL site -check: 3 problem(s)
       ch01-a/ch01-a.md: link to a path not in the repository: missing.go
       ch01-a/index.html: broken link missing.go
       index.html: empty link
     ```
     Neither the missing anchor behind `/book//ch01-a/#no-such-anchor` nor `missing-single.html` was reported.
   - **Expected fix:** Normalize `target` with `strings.TrimLeft(target, "/")` (or reject `strings.HasPrefix(target, "/")` after trimming `base`) so `target` always matches the relative slash-path keys in `ids`, and ensure non-empty fragments on HTML files are always checked. Also match single-quoted `href`/`src`/`id` attributes in `check.go`, and add regression tests in `internal/site/site_test.go`.

5. **nit** — `internal/site/markdown.go:156` (`includeMarkerPattern`)
   - **What is wrong:** `internal/repocheck/inline.go:12` supports `<!-- include: <path> lines=A-B -->`, but `includeMarkerPattern` in `internal/site/markdown.go:156` only matches `^<!--\s*include:\s*(\S+)\s*-->\s*$`. Any chapter using a line-range include marker will have it left as an invisible HTML comment instead of rendered as a `<p class="include">` source link.
   - **How confirmed:** Ran `/tmp/opencode/course site -out <tmp> -base /book/` on a fixture with `<!-- include: foo.go lines=1-2 -->`: `has class=include: False has raw comment: True`.
   - **Expected fix:** Allow optional `lines=(\d+)-(\d+)` in `includeMarkerPattern`, appending `#L<from>-L<to>` to the GitHub URL (and `:from-to` to the label) when present.

6. **nit** — `internal/site/site.go:215,232-249` (`load` / `stripFrontMatter`)
   - **What is wrong:** `load` runs `stripFrontMatter` unconditionally on all page kinds, not only `knowledgePage`. If `README.md` or another non-knowledge page begins with a thematic break `---` and contains another `---` later, everything between them (including `# Book Title`) is silently stripped.
   - **How confirmed:** Ran `/tmp/opencode/course site` on a fixture whose `README.md` was `---\n# Book Title\n\nSome intro\n---\n\nBody after rule\n`: `index.html` contained neither `Book Title` nor `Some intro` (`has Book Title: False has Some intro: False`).
   - **Expected fix:** Only call `stripFrontMatter` when `p.kind == knowledgePage` (keeping `\r\n` normalization if desired).

7. **nit** — `internal/site/site.go:113-117` (`Build`)
   - **What is wrong:** `for dir := path.Dir(name); dir != "."; dir = path.Dir(dir)` loops infinitely if a caller passes an absolute path in the exported `Options.Files` slice, because `path.Dir("/") == "/"`.
   - **How confirmed:** Code inspection of `internal/site/site.go:113-117` (`path.Dir` semantics on rooted paths).
   - **Expected fix:** Guard the loop with `dir != "." && dir != "/"` (and trim leading `/` or reject absolute entries in `Options.Files`).

8. **nit** — `cmd/course/main.go:31` (`usage`)
   - **What is wrong:** The CLI usage line for `check paths` still reads `verify no tracked file mentions a machine-local path`, omitting the new relative-link verification and external-link reporting added in this chunk.
   - **How confirmed:** Inspected `cmd/course/main.go:31` against `cmd/course/checks.go:152-159`.
   - **Expected fix:** Update the `check paths` description in `usage` to mention unresolved relative links and external-link reporting.

## Commands run

- `gofmt -l internal/site internal/repocheck cmd/course agent_work/pub/serve-check` — PASS (empty output).
- `go build ./... && go vet ./... && go test -v ./internal/site/... ./internal/repocheck/... ./cmd/course/...` — PASS (`internal/site` 10 tests, `internal/repocheck` 17 tests, `cmd/course` 2 tests).
- `go test -race ./internal/site/... ./internal/repocheck/... ./cmd/course/...` — PASS (0 races).
- `GOOS=darwin GOARCH=arm64 go build ./... && GOOS=darwin GOARCH=arm64 go vet ./...` — PASS.
- `go run ./cmd/course site -check -base /sqlite-internals/` — PASS (`23 page(s), 1045 link(s) resolve`, 2.6s).
- `go run ./cmd/course site -check` — PASS (`23 page(s), 1045 link(s) resolve`).
- `go run ./cmd/course site -out /tmp/opencode/pub-site1 -base /sqlite-internals/` twice into two directories + `diff -r` — PASS (`23 page(s)` written, byte-identical).
- `go run ./agent_work/pub/serve-check -dir /tmp/opencode/pub-site1 -base /sqlite-internals/ "" ch01-architecture-and-execution-path/ ch02-file-format-and-pages/ ch04-pager-and-page-cache/ ch05-wal-transactions-checkpoints/ ch07-vfs-and-filesystem-io/` — PASS (`fetched 24 URL(s)`, all 200).
- `go run ./cmd/course check paths` — **FAIL** (exit 1; 5 false-positive machine-path problems in unmodified copied SQLite C files `pager.c`, `os_unix.c`, `vfstrace.c`).
- `export SQLITE_SRC=/root/Software/sqlite-src/3.53.4 && go run ./cmd/course check all -skip-evidence` — **FAIL** (exit 1; `sources`, `spans`, `inline`, `gofmt` all PASS; `check paths` fails on the 5 copied C file hits above).
- `python3 -c 'import yaml; yaml.safe_load(open(".github/workflows/pages.yml"))'` + `gh api repos/actions/{checkout,setup-go,configure-pages,upload-pages-artifact,deploy-pages}/releases/latest` + `upload-pages-artifact/contents/action.yml` — PASS (valid YAML, current major versions verified: checkout v7, setup-go v7, configure-pages v6, upload-pages-artifact v5 with `include-hidden-files`, deploy-pages v5).
- Adversarial checks on `/tmp/opencode/course` across 8 synthetic git repositories — reproduced Findings 2, 3, 4, 5, and 6; verified non-ASCII heading slugs + TOC links pass `site -check`.
- Comment-policy check on all new `.go` files — PASS (only `//go:embed` directives present; zero prose comments).
