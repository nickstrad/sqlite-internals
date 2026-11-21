VERDICT: FAIL

## Review-1 findings and Run-1 adversarial attacks

| Finding / Attack | Fixed? | How confirmed |
| --- | --- | --- |
| Review 1 / PATH-01 — `check paths` fails on upstream SQLite C copies | Yes | Read `ef9d046` (`upstreamCopies` skips files in tracked `SOURCES.sha256` while keeping them in `fileSet`); `TestSweepPathsSkipsUpstreamCopiesAndDeletedFiles` PASS; `go run ./cmd/course check paths` and `check all -skip-evidence` PASS on live repo (17 upstream copies skipped, 7 external links). |
| Review 2 / PATH-07a, PATH-07b — CRLF fence / inline triple-backtick / 5-space indent hide later links | Yes | Read `ef9d046` (replaced `fenceState` with goldmark AST parsing in `MarkdownLinks`); `TestMarkdownLinksAndFences` PASS; CLI probe on CRLF fenced repo flagged `README.md:7`; `PATH-07a`/`PATH-07b` HELD. |
| Review 3 / PATH-05a, PATH-05b, PATH-06 — inline backtick length, titles, multiline, angle dedup, spaced ref | Yes | Read `ef9d046` (goldmark AST + per-file destination dedup + `util.UnescapePunctuations`); `TestMarkdownLinksAndFences` PASS; `PATH-05a`/`PATH-05b`/`PATH-06` HELD; CLI probe on `[](missing)` and `![](missing)` reported exact line numbers. |
| Review 4 / LINK-04, LINK-10b — `/book//x/#a` skips anchor check; single-quoted attrs; duplicate `id` | Partial (introduced New Finding 1) | Read `73a8a3d`; `TrimLeft(target, "/")` and `.html` anchor fallback fix `/book//x/#a`, and `TestCheckSiteHTMLEdges` PASS (`LINK-04`/`LINK-10b` HELD), **but** regex-matching single-quoted `id`/`href`/`src` across raw HTML text outside tags introduced New Finding 1 below. |
| Review 5 — `<!-- include: p lines=A-B -->` not converted | Yes | Read `73a8a3d` (`includeMarkerPattern` accepts optional `lines=(\d+)-(\d+)`); `TestIncludeMarkerWithLineRange` PASS; CLI probe emitted `<p class="include"><a href=".../ch01-a/foo.go#L1-L2">ch01-a/foo.go:1-2</a></p>`. |
| Review 6 — front matter stripped on `README.md` / non-knowledge pages | Yes | Read `73a8a3d` (`stripFrontMatter` called only for `knowledgePage`); `TestFrontMatterOnlyOnKnowledgePages` PASS. |
| Review 7 — absolute `Options.Files` loops infinitely in `Build` | Yes | Read `73a8a3d` (`Build` rejects `path.IsAbs` and `..` prefixes); `TestBuildRejectsAbsoluteFiles` PASS. |
| Review 8 — stale CLI usage line for `check paths` | Yes | Inspected `cmd/course/main.go:31-32` and `TestRunsMustBePositive` output; usage now covers unresolved relative links and external link reporting. |
| LIVE-06b — `-out <symlink>/repo` wiped repository | Yes | Read `73a8a3d` (`resolveExisting`, `containsPath`, `sameFileAsAncestor`); `TestPrepareSiteDirRefusesRepositoryThroughSymlink` PASS; CLI probe with direct symlink to repo refused and kept `.git` intact; `LIVE-06b` HELD. |
| LINK-05, LINK-06 — `id`/`href` inside HTML comments | Yes | Read `73a8a3d` (`htmlCommentPattern.ReplaceAll` before scanning); `TestCheckSiteHTMLEdges` PASS; `LINK-05`/`LINK-06` HELD. |
| LINK-07 — `?query` on relative links | Yes | Read `73a8a3d` (strips `?query` for resolution, preserves on GitHub links); `TestQueryStringsAndEscapesInRelativeLinks` PASS; `LINK-07` HELD. |
| MD-01a, MD-01b, MD-01c — front matter unquoting, empty front matter, EOF `---` | Yes | Read `73a8a3d` (`unquoteYAMLScalar`, line-by-line `stripFrontMatter`); `TestStripFrontMatterEdges` PASS; all 3 HELD. |
| MD-02a, MD-02b — heading entities/code spans, punctuation-only headings | Yes | Read `73a8a3d` (`plainText` resolves escapes/entities outside `*ast.CodeSpan`, empty slug → `section`); `TestHeadingIDsFromEntitiesAndPunctuation` PASS; both HELD. |
| MD-04b — helper dir under `experiments/` listed | Yes | Read `73a8a3d` (`experimentIDPattern` `^\d+\.\d+$`); `TestOnlyNumberedExperimentDirectoriesAreListed` PASS; HELD. |
| MD-05a, MD-05b — back-referenced rung / rung in code block | Yes | Read `73a8a3d` (`proseOnly` blanks code/HTML blocks & code spans; `rungDeclarationPattern` preferred); `TestRungIgnoresCodeAndPrefersDeclaration` PASS; both HELD. |
| MD-07a — Answers link omitted when chapter has `<2` H2/H3 headings | Yes | Read `73a8a3d` (`internal/site/page.html` renders sidebar for `or .TOC .Answers`); `TestAnswersLinkWithoutTableOfContents` PASS; HELD. |
| PATH-03 — bare `/root`, `/home/x`, `/Users/x`, `/users/x/` | Yes (attack assertion bug) | Read `ef9d046` (`machinePathPattern` + `MachinePaths`); `TestMachinePathsWithoutTrailingSlash` PASS; `results-run2.txt` GOT shows all 4 detected in one per-line problem (`mentions a machine-local path /root, /home/alice, /Users/bob, /users/bob/: ...`); attack assertion updated in Part 2 to verify all 4 reported paths rather than `len(Problems) >= 4`. |
| PATH-08a — `..` traversal in `IsPublicReference` | Yes | Read `ef9d046` (`path.Clean` before prefix match); `TestIsPublicReferenceRejectsDotDot` PASS; HELD. |
| PATH-09b — link to tracked-but-deleted file | Yes | Read `ef9d046` (`os.Lstat` filter before building `fileSet`); `TestSweepPathsSkipsUpstreamCopiesAndDeletedFiles` PASS; HELD. |

## New findings

1. **blocker (new defect introduced in `73a8a3d`)** — `internal/site/check.go:17-18,53-65` (`CheckSite`)
   - **What is wrong:** Commit `73a8a3d` extended `idAttrPattern` and `linkAttrPattern` to allow whitespace around `=` and single-quoted values (`\sid\s*=\s*(?:"([^"]*)"|'([^']*)')` and `\s(?:href|src)\s*=\s*(?:"([^"]*)"|'([^']*)')`), and added duplicate-ID rejection, while continuing to scan the entire HTML document bytes rather than HTML opening tags. Goldmark's HTML renderer escapes `"` to `&quot;` inside rendered text and `<pre><code>` blocks (so double-quoted attributes never matched text/code before), **but it leaves single quotes `'` unescaped**. As a result, ordinary markdown prose and fenced/inline code blocks now trigger false `id`, `href`, and `src` matches:
     1. **False `duplicate id` failure on routine SQL code blocks (or prose):** In a SQLite book, any page containing two SQL statements with `WHERE id = 'alpha'` (or `WHERE id='1'`) fails `course site -check` (and blocks `.github/workflows/pages.yml` deployment) with `duplicate id "alpha"`.
     2. **False `broken link` failure on single-quoted `href='...'` / `src='...'` in code blocks:** A fenced code block showing HTML such as ```` ```html\n<a href='/book/single-in-code/'>code</a>\n``` ```` renders `&lt;a href='/book/single-in-code/'&gt;`, which `linkAttrPattern` treats as a real link and fails `course site -check` with `broken link /book/single-in-code/`.
     3. **Silent false negative on broken anchors:** If a page has a single `SELECT * FROM items WHERE id = 'alpha';` in a SQL code block and a broken anchor link `[broken anchor](#alpha)`, `idAttrPattern` adds `"alpha"` to `ids[rel]` and `course site -check` passes even though no DOM element defines `id="alpha"`.
   - **How confirmed:** Built `/tmp/opencode/pub-rereview-probe/course` from `HEAD` (`be8edef`) and ran `course site -check -base /book/` on three scratch repositories:
     - SQL code block containing `SELECT * FROM items WHERE id = 'alpha';` and `UPDATE items SET v = 2 WHERE id = 'alpha';`:
       ```text
       FAIL site -check: 1 problem(s)
         ch01-a/index.html: duplicate id "alpha"
       ```
     - Fenced ```` ```html ```` block containing `<a href='/book/single-in-code/'>code</a>`:
       ```text
       FAIL site -check: ...
         ch01-a/index.html: broken link /book/single-in-code/
       ```
     - Page with `[broken anchor](#alpha)` and a SQL code block containing `SELECT * FROM items WHERE id = 'alpha';`:
       ```text
       PASS site -check: 2 page(s), 6 link(s) resolve
       ```
   - **Expected fix:** Only extract `id`, `href`, and `src` attributes from actual HTML opening tags (not text nodes or `<pre><code>` contents where `<` is escaped to `&lt;`), skipping HTML comments `<!--...-->` (and handling quoted `>` inside attribute values, or tokenizing HTML tags cleanly). Add regression tests in `internal/site/site_test.go` covering repeated SQL `WHERE id = 'alpha'` in a fenced block, single-quoted `href='...'` in a fenced block, and a broken `#alpha` anchor alongside SQL `WHERE id = 'alpha'`.

## Commands run

- `export SQLITE_SRC=/root/Software/sqlite-src/3.53.4 && go build ./... && go vet ./... && go test ./...` — PASS (all packages ok).
- `bash agent_work/verify/crossbuild.sh` — PASS (`CROSSBUILD OK`: `linux/amd64`, `darwin/arm64`, `darwin/amd64` build/vet/test-compile, `check gofmt` 154 files).
- `go test -race -v ./internal/site/... ./internal/repocheck/... ./cmd/course/...` — PASS (0 races; 20 site tests, 20 repocheck tests, 3 course tests).
- `export SQLITE_SRC=/root/Software/sqlite-src/3.53.4 && go run ./cmd/course check paths` — PASS (`184 tracked text file(s)... 0 binary file(s) and 17 upstream SQLite copies skipped, 7 external link(s) reported`).
- `export SQLITE_SRC=/root/Software/sqlite-src/3.53.4 && go run ./cmd/course check all -skip-evidence` — PASS (`sources`, `spans`, `inline`, `gofmt`, `paths` all PASS).
- `go run ./cmd/course site -check -base /sqlite-internals/` and `go run ./cmd/course site -check` — PASS (`23 page(s), 1045 link(s) resolve` on both).
- `go run ./cmd/course site -out /tmp/opencode/pub-site1 -base /sqlite-internals/` (twice) + `diff -r` + `go run ./agent_work/pub/serve-check -dir /tmp/opencode/pub-site1 -base /sqlite-internals/ "" ch01... ch02... ch04... ch05... ch07...` — PASS (23 pages, byte-identical, 24 URLs 200).
- Chunk `pub` has no chapter experiments of its own.
- Comment-policy check on `git diff b146d5f..HEAD -- '*.go'` — PASS (zero comments added to non-adversarial `.go` files).
