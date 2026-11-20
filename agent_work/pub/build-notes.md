# Publishing chunk (`pub`) — build notes for Neon

Working file, not course text. Plain data: what was built, how it runs, what it printed.
Branch `chunk/pub`. Spec: OUTLINE.md §8 rows "A, B" and "C" plus the publishing brief.

## 1. What exists after this chunk

| path | what it is |
| --- | --- |
| `internal/site/site.go` | page discovery, chapter/experiment/rung model, link resolution, `Build` |
| `internal/site/markdown.go` | goldmark setup, GitHub-compatible heading ids, plain-text extraction, experiment "Code · Evidence" lines, include-marker rewriting, link rewriting |
| `internal/site/page.go` | the UI label set `UILabels`, page view model, HTML rendering through `page.html` |
| `internal/site/check.go` | `CheckSite`: scans the rendered HTML for every `href`/`src` and `id`, fails on broken internal links and missing anchors |
| `internal/site/page.html` | the one HTML template (embedded) |
| `internal/site/style.css` | the one stylesheet (embedded, copied to `style.css` in the output) |
| `internal/site/site_test.go` | unit tests on a fixture repo |
| `cmd/course/site.go` | the `site` subcommand |
| `internal/repocheck/paths.go` | the extended path sweep (`SweepPaths`, `CheckPaths`) |
| `internal/repocheck/paths_test.go` | its unit tests |
| `.github/workflows/pages.yml` | the Pages workflow |
| `.gitignore` | adds `_site/` |
| `agent_work/pub/serve-check/main.go` | throwaway local server + crawler used to verify the built site |

New dependency: `github.com/yuin/goldmark v1.8.7` (direct). goldmark itself has no dependencies.

### Syntax highlighting: not added, and why
Chroma (via `goldmark-highlighting`) pulls in `github.com/alecthomas/chroma/v2` and
`github.com/dlclark/regexp2`, adds several MB of lexer tables to the `course` binary, and
inserts inline `<span style=…>` markup into every one of the ~70 code blocks (the two chapters
inline whole Go files; ch02 alone is 5,772 markdown lines). The course is otherwise a
one-dependency-per-need module. Code blocks are rendered as plain `<pre><code class="language-go">`
(goldmark adds the `language-xxx` class from the fence info), so highlighting can be added later
without changing the markdown. Decision: no highlighting.

## 2. How the site is built and checked

Commands (run from the repo root):

```text
go run ./cmd/course site -out _site -base /sqlite-internals/     # build
go run ./cmd/course site -check -base /sqlite-internals/         # acceptance check
go run ./cmd/course site -check                                  # same check with base "/"
```

Flags of `course site`:

| flag | default | meaning |
| --- | --- | --- |
| `-out DIR` | `_site` | output directory (relative to the current directory) |
| `-base PATH` | `/` | URL path the site is served under; normalized to `/x/` form (`sqlite-internals`, `/sqlite-internals`, `/sqlite-internals/` are the same) |
| `-repo URL` | `https://github.com/nickstrad/sqlite-internals` | repository used for code/evidence links |
| `-branch NAME` | `main` | branch used in those links |
| `-check` | off | render into a temporary directory, run the link check, delete the directory; exit 1 on any problem |

Exit codes follow the rest of `cmd/course`: 0 pass, 1 check failure or error, 2 bad arguments.

Output lines:
- build: `PASS site: <n> page(s) written to <out> (base <base>)`, preceded by `WARN site: <file>: <problem>` for each link that could not be resolved (the build still writes the site; `-check` is the gate).
- check: `PASS site -check: <pages> page(s), <links> link(s) resolve` or `FAIL site -check: <n> problem(s)` followed by one indented line per problem.

### Which files the site is built from
`git ls-files --cached --others --exclude-standard`, minus paths missing on disk. So: tracked
files plus new not-yet-committed files that are not git-ignored. Ignored files (`_src/`, `_work/`,
`_tools/`…) never become pages and are never valid link targets — the same view a reader has
on GitHub. A link to an ignored file is reported as a broken link.

### Safety of `-out`
`-out` is wiped and rewritten on each build, but only if it is empty, does not exist, or holds a
`.nojekyll` from an earlier build; any other non-empty directory is refused with
`refusing to replace <dir>: it is not empty and holds no .nojekyll from an earlier site build`.
A directory that contains the repository (e.g. `-out .`) is refused:
`refusing to write the site into <dir>: it contains the repository`.

### Determinism
No timestamps, no map-order dependence (every list is sorted). Verified: two builds of the real
repo with `-base /sqlite-internals/` compared with `diff -r` → identical. Unit test
`TestDeterministicOutput` builds the fixture twice and compares every file byte for byte.
Runtime of `site -check` on the current repo: ~0.6 s wall.

## 3. Output layout

```text
_site/
  .nojekyll
  style.css
  index.html                                   home: README.md + Reading path + Reference
  chNN-slug/index.html                         one per chapter: chNN-slug/chNN-slug.md
  chNN-slug/answers/index.html                 chNN-slug/answers.md (if present)
  outline/index.html                           OUTLINE.md
  knowledge/<file-stem>/index.html             each docs/knowledge/*.md
```

Today: 14 pages (home, 2 chapters, 2 answers pages, outline, 8 knowledge pages) + `style.css` + `.nojekyll`.
All internal links are absolute under the base (`/sqlite-internals/ch02-…/#anchor`).

### Page discovery rules
- Chapters: top-level directories named `chNN-<slug>` (regex `^ch\d\d-[A-Za-z0-9._-]+$`) that contain `chNN-<slug>/chNN-<slug>.md`. Ordered by directory name. Title = text of the first `# ` heading.
- Answers: `chNN-<slug>/answers.md` if it exists. Rendered as its own page, never inline.
- Knowledge pages: every `docs/knowledge/*.md`. YAML front matter (`---` … `---` at the very top) is stripped. Title = first `# ` heading; if there is none, the front matter `title:` value; else the file name.
- `OUTLINE.md` → `outline/`.
- Any other markdown file (e.g. `STATE.md`, `docs/course-source.md`, `agent_work/INDEX.md`) is not a page; links to it go to GitHub.

### Home page (`index.html`)
1. The rendered `README.md`.
2. A generated section with heading **Reading path** (`id="reading-path"`): an ordered list of chapters (link to the chapter page, then `Rung <id> (<name>)` if the chapter names one), each with a nested list of its experiments linking to the experiment heading anchor in the chapter page.
3. A generated section with heading **Reference** (`id="reference"`): **How this book was planned** → `outline/`, then each knowledge page by title, in file-name order.

Anchor ids for these two headings come from the same slugger as the README headings, so a README
heading literally called "Reading path" would push the generated one to `reading-path-1` rather
than collide.

### Rung detection
In the chapter markdown: the first match of `Rebuild Ladder (rung R<n>)` gives the rung id; then
the first `**R<n> (<name>)**` gives its name. Today: ch01 → `R0 (Recipe)`, ch02 → `R1 (Pages)`.
A chapter without the phrase `Rebuild Ladder (rung …)` shows no rung. (The first plain mention of
"rung" in ch02 is R4, in a forward reference, which is why the id comes from the "Rebuild Ladder
(rung …)" phrase and not from the first "rung".)

### Experiment detection
- A level-3 heading whose text is `Experiment N.M …`, optionally preceded by a section number
  (`### 5.2 Experiment 1.3 — …`), is the experiment's section. Headings that merely mention an
  experiment (`### Determinism rules verified by Experiment 1.3`) do not match. The first such
  heading per id wins.
- The heading's directory is `chNN-…/experiments/N.M-*` (first match by name).
- A directory `experiments/N.M-*` with no matching heading is still listed in the reading path, by directory name, linking to the top of the chapter page.
- Reading-path entries show the heading text from the word "Experiment" onward, code spans flattened to text (e.g. `Experiment 2.2 — Page census (2.2-page-census)`).
- Right after each experiment heading the page gets a generated line `Code · Evidence` (`<p class="experiment-links">`): **Code** → GitHub `chNN-…/experiments/N.M-x/main.go`, **Evidence** → GitHub `chNN-…/evidence/N.M-x.txt`. Each link appears only if that file exists.

### Chapter page
- Sidebar **Contents**: every level-2 and level-3 heading of the chapter (level 3 indented), linking to its anchor; below it an **Answers** link to the answers page if one exists. On screens narrower than 60rem the sidebar sits above the text.
- **Previous** / **Next** links (chapter titles) above and below the body.
- The chapter body.
- The chapter's own `[answers.md](answers.md)` link in section 8 becomes a link to the answers page.

### Answers page
A link back to the chapter (the chapter title as link text) above the body. No sidebar.

### Outline and knowledge pages
Sidebar **Contents** of their level-2/3 headings when there are at least two such headings.

### Heading ids
GitHub-compatible (github-slugger algorithm): lower-case; keep letters, digits, combining marks,
`_` and `-`; each space becomes `-`; everything else (punctuation, `.`, `—`, `` ` ``, `/`, `(`) is
dropped; duplicates get `-1`, `-2`, … So anchors written for GitHub's renderer work unchanged:
`#8-phase-2-rulings-nick-2026-11-20`, `chapter-1--architecture-and-execution-path`,
`#51-the-internaldbfile-package`. Heading text is the plain text including code-span contents.

### Link rewriting (markdown links and images)
| link in markdown | becomes |
| --- | --- |
| `http(s)://…`, `mailto:`, any scheme, `//host` | unchanged |
| `#anchor` | unchanged (checked against the page's ids) |
| relative path to a rendered page's source (`answers.md`, `../ch02-…/ch02-….md#x`, `../OUTLINE.md`, `../../README.md`, `docs/knowledge/x.md`) | site URL of that page + fragment |
| relative path to a chapter directory (`../ch01-…/`) | that chapter's page |
| relative path to any other existing file (`.go`, `.c`, `.h`, `.txt`, `SOURCES.sha256`, `STATE.md`, anything in `agent_work/`, `internal/`, `cmd/`, `docs/`) | `https://github.com/nickstrad/sqlite-internals/blob/main/<path>` + fragment |
| relative path to an existing directory (`../internal/trace/`) | `…/tree/main/<path>` |
| absolute path (`/x`), path leaving the repo, path to a missing or ignored file | left unchanged and reported as a problem (WARN on build, FAIL on `-check`) |

### Include markers
`<!-- include: path -->` lines (the markers `course inline` maintains) are replaced by a small
line `<p class="include">` holding a link whose text is the repo-relative path
(e.g. `internal/dbfile/errors.go`) and whose target is that file on GitHub. The code block that
follows the marker is rendered normally. Other HTML comments (e.g. `<!-- adversarial-results -->`)
pass through as HTML comments, invisible.

### Markdown dialect
goldmark with the GFM extension set (tables, strikethrough, autolinks via linkify, task lists)
and raw HTML passed through (`html.WithUnsafe`, as GitHub does for its own content). Tables scroll
horizontally inside the text column; code blocks scroll horizontally.

### Style
One stylesheet, embedded, no fonts or CDNs: system font stack, ~46rem text measure, sticky
sidebar on wide screens, light and dark palettes via `prefers-color-scheme`. The site works
offline (no external requests at all; only the code/evidence/source links point to GitHub).

## 4. Every UI label in the chrome

Defined once in `internal/site/page.go` (`UILabels`); no other fixed text is generated.

| label | where it appears |
| --- | --- |
| `Contents` | sidebar heading and its `aria-label` (chapter, outline, knowledge pages) |
| `Previous` | small caption above the previous chapter's title (chapter pages) |
| `Next` | small caption above the next chapter's title (chapter pages) |
| `Reading path` | generated section heading on the home page |
| `Reference` | generated section heading on the home page |
| `How this book was planned` | home page link text for the `OUTLINE.md` page |
| `Answers` | sidebar link to the chapter's answers page |
| `Code` | experiment line link to `main.go` |
| `Evidence` | experiment line link to the evidence `.txt` |
| `Rung` | prefix before the rung id in the reading path (`Rung R1 (Pages)`) |

Other generated text, all taken from the markdown: book title in the header and in every
`<title>` (= README's first `# ` heading), page titles, chapter titles in Previous/Next and on
answers pages, experiment headings, knowledge-page titles, the `·` separator between Code and
Evidence, and the include-marker path.

## 5. What the README must contain for the home page to read well

The generator renders `README.md` as the book's front page and appends the reading path after it.
- The first `# ` heading is the **book title**: it is shown in the header of every page and in every browser tab title (`<page title> · <book title>`). Make it the book's name, short.
- Do not hand-write a chapter list or an experiment list: the generated **Reading path** follows the README and is always current. A short paragraph pointing to it is fine (`[the reading path](#reading-path)` resolves).
- Links in README are relative to the repo root: `ch01-…/ch01-….md` → chapter page; `OUTLINE.md` → outline page; `docs/knowledge/x.md` → knowledge page; anything else (e.g. `STATE.md`, `agent_work/INDEX.md`, `docs/course-source.md`, a directory) → GitHub. Every relative link must resolve or `site -check` fails.
- Avoid a README heading literally named "Reading path" or "Reference" (the generated sections would get `-1` ids).
- Today's README says "no chapters exist yet" and points to `agent_work/fetch-pinned-sqlite.sh`; both read wrong on the published home page (chapters exist; `go run ./cmd/course fetch-source` is the reader-facing command). Not changed here — README belongs to Neon.

## 6. The Pages workflow (`.github/workflows/pages.yml`)

Triggers: push to `main`, `workflow_dispatch`. Permissions: `contents: read`, `pages: write`,
`id-token: write`. Concurrency group `pages`, `cancel-in-progress: false` (GitHub's recommendation:
let an in-flight deployment finish).

Job `build` (ubuntu-latest): `actions/checkout@v7` → `actions/setup-go@v7` with `go-version-file: go.mod`
→ `actions/configure-pages@v6` (id `pages`) → `go run ./cmd/course site -check -base "<base_path>/"`
→ `go run ./cmd/course site -out _site -base "<base_path>/"` → `actions/upload-pages-artifact@v5`
with `path: _site`, `include-hidden-files: true` (so `.nojekyll` ships; v5 excludes dot-files by default).
Job `deploy` (needs build, environment `github-pages`, url from the step output): `actions/deploy-pages@v5`.

Deviation from the brief's step order: `configure-pages` runs *before* the two `site` steps,
because its `base_path` output (`/sqlite-internals` for this repo, `""` with a custom domain) is
what the site is built under. The check and the build therefore always use the base the site is
actually served at.

Action majors checked on 2026-11-20 with `gh api repos/actions/<name>/releases/latest`:
checkout v7.0.1, setup-go v7.1.0, configure-pages v6.0.0, upload-pages-artifact v5.0.0,
deploy-pages v5.0.1. Input names verified against each action's `action.yml`
(`go-version-file`; `path`, `include-hidden-files`) and the `base_path` output of configure-pages.

The site build needs only Go and git: no SQLite tree, no strace, no Docker, no network beyond the
Go module download of goldmark.

Validation: `go run github.com/rhysd/actionlint/cmd/actionlint@latest .github/workflows/pages.yml`
(actionlint v1.7.12, built from source) → no findings, exit 0. Not pushed, not triggered.

## 7. Local verification (commands and results)

```text
rm -rf _site
go run ./cmd/course site -out _site -base /sqlite-internals/
  PASS site: 14 page(s) written to _site (base /sqlite-internals/)
go run ./cmd/course site -check -base /sqlite-internals/
  PASS site -check: 14 page(s), 517 link(s) resolve
go run ./cmd/course site -check
  PASS site -check: 14 page(s), 517 link(s) resolve
go run ./agent_work/pub/serve-check -dir _site -base /sqlite-internals/ "" \
    ch01-architecture-and-execution-path/ ch02-file-format-and-pages/ ch02-file-format-and-pages/answers/
  200 /sqlite-internals/
  200 /sqlite-internals/ch01-architecture-and-execution-path/
  200 /sqlite-internals/ch01-architecture-and-execution-path/answers/
  200 /sqlite-internals/ch02-file-format-and-pages/
  200 /sqlite-internals/ch02-file-format-and-pages/answers/
  200 /sqlite-internals/knowledge/<each of 8>/
  200 /sqlite-internals/outline/
  200 /sqlite-internals/style.css
  fetched 15 URL(s)
  PASS: every fetched URL returned 200
go run ./cmd/course site -out /tmp/x -base /sqlite-internals/ && diff -r _site /tmp/x   → identical
```

`serve-check` serves the directory with `net/http.FileServer` behind `StripPrefix("/sqlite-internals")`
on a random localhost port (the shape GitHub Pages serves), starts from the given pages, and
follows every same-site `href`/`src` until no new URL is found. It does not fetch GitHub URLs.

Spot checks of GitHub targets on the live repo: `…/blob/main/ch01-…/evidence/1.1-pin-check.txt` → 200.
(`…/blob/main/internal/dbfile` → 301: a directory under `/blob/` redirects, which is why
directories are emitted as `/tree/`.)

Rendering facts of the current chapters: 0 "raw HTML omitted" markers; include markers ch01 = 9,
ch02 = 20 → all converted; every experiment heading got its Code · Evidence line (6 experiments).

## 8. `course check paths` (addendum C)

### Behaviour
- Files examined: `git ls-files` (tracked files only; untracked files are never read, even if present). Paths under `agent_work/`, `kb/` and the file `STATE.md` are skipped (unchanged `PathSweepExclude`).
- Binary files (a NUL byte in the first 8,000 bytes, git's own heuristic) are skipped and listed as `INFO check paths: binary file skipped: <path>`.
- Tracked files that `.gitignore` also matches are listed as `INFO check paths: tracked although .gitignore matches it: <path>`.
- **FAIL** on a machine path in any line of any text file: `/root/`, `/home/<name>/`, `/Users/<name>/` (name = `[A-Za-z0-9._-]+`), when not preceded by a letter, digit, `.`, `_`, `~` or `-` (so `https://example.org/root/x` and `a/root/b` do not match; `"/root/x"`, `` `/home/alice/` ``, `file:///root/x` do). `/home/<name>/` written literally with angle brackets does not match.
- **FAIL** on a relative link in a markdown file that does not resolve to a tracked file or a directory holding tracked files: inline links `[t](path)`, `[t](<path with spaces>)`, links with a title, and reference definitions `[x]: path`. Links inside fenced code blocks (``` or ~~~, also inside `>` quotes) and inline code spans are ignored. Absolute paths (`/etc/hosts`) and paths leaving the repo fail.
- **Report, not fail**: every `http(s)://` URL in a reader-facing text file whose target is not one of: `github.com/nickstrad/sqlite-internals…`, `nickstrad.github.io/sqlite-internals…`, `sqlite.org` (+ subdomains), `github.com/sqlite/sqlite…`, `pkg.go.dev`, `go.dev`, `golang.org` (+ subdomains, e.g. `proxy.golang.org`), `modernc.org`, `gitlab.com/cznic/…` (the driver's source). Printed as `EXTERNAL <url> (<n> occurrence(s), first at <file>:<line>)`, sorted by URL. URLs inside `_test.go` files are not reported (test fixtures).
- Summary line: `PASS check paths: <n> tracked text file(s) free of machine-local paths and unresolved relative links (<b> binary file(s) skipped, <e> external link(s) reported)`.

Exported API (new): `SweepPaths(root) (PathSweep, error)`, `PathSweep{Problems, Checked, Binary, External, IgnoredButTracked}`, `ExternalLink{URL, Where, Count}`, `PublicReferences`, `IsPublicReference(url)`, `MachinePaths(line)`, `MarkdownLinks(line)`, `ResolveRelativeLink(from, link, files, dirs)`. `CheckPaths(root)` keeps its old signature and returns the sweep's problems and checked count. The old doc comment on `CheckPaths` in `sweep.go` was removed (it described the old behaviour); the function now lives in `paths.go` and needs a new comment.

### Result on this branch
```text
INFO check paths: binary file skipped: _tools/bin/kb
INFO check paths: tracked although .gitignore matches it: _tools/bin/kb
EXTERNAL http://seaweedfs:8333 (1 occurrence(s), first at compose.yaml:15)
EXTERNAL https://github.com/rsc/fpfmt (1 occurrence(s), first at ch02-file-format-and-pages/util.c:778)
EXTERNAL https://github.com/superfly/litefs/blob/main/docs/ARCHITECTURE.md (1 occurrence(s), first at docs/knowledge/determinism-and-rebuild.md:430)
EXTERNAL https://github.com/superfly/ltx (1 occurrence(s), first at docs/knowledge/determinism-and-rebuild.md:425)
EXTERNAL https://litestream.io/how-it-works/ (1 occurrence(s), first at docs/knowledge/determinism-and-rebuild.md:415)
EXTERNAL https://rqlite.io/docs/api/non-deterministic/ (1 occurrence(s), first at docs/knowledge/determinism-and-rebuild.md:436)
EXTERNAL https://rqlite.io/docs/design/ (1 occurrence(s), first at docs/knowledge/determinism-and-rebuild.md:435)
PASS check paths: 120 tracked text file(s) free of machine-local paths and unresolved relative links (1 binary file(s) skipped, 7 external link(s) reported)
```
All seven external links are public references (a compose-internal service name, an upstream
comment in a copied SQLite source file, and cited prior art in a knowledge report). None needs action.

## Corrections to the brief

1. **"`check paths` walks the filesystem and trips on ignored binaries such as `_tools/bin/*`" — not so.** `CheckPaths` already used `git ls-files`. It failed because `_tools/bin/kb` (an 18.6 MB ELF binary) **is tracked**: committed in `c51ff44` ("infra: vendor kb, …") even though `.gitignore` has `/_tools/`. The binary happens to contain the bytes `/root/`. Fix made here: binary files are skipped (and listed), and tracked-but-ignored files are listed. **Open decision for Nick/Fable:** whether `_tools/bin/kb` should be untracked (`git rm --cached _tools/bin/kb`) — not done here because it was committed deliberately with the kb vendoring and `kb/` is out of bounds for builders. If it stays, every clone of the public repo downloads an 18.6 MB Linux binary.
2. Workflow step order: `configure-pages` moved before the site steps to obtain `base_path` (section 6).

## Surprises / notes
- The README links to `agent_work/fetch-pinned-sqlite.sh` in text (code span, not a link) and says "no chapters exist yet"; see section 5.
- `course check all` does not run `site -check`; the workflow runs it. Adding it to `check all` is a one-line change if wanted.
- `docs/knowledge/*.md` pages are published; they contain build-machine-flavoured wording such as "the installed `/usr/local/bin/sqlite3` CLI" (not a home path, so not flagged). Whether knowledge reports belong on the public site at all is a content decision; the brief asked for them.
