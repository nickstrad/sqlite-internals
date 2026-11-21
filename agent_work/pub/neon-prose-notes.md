# Neon prose pass notes — chunk `pub`

## What was written

### Code comments (comments only; comment-stripped code unchanged)
- `internal/site/site.go`: package doc comment; doc comments on `Options`, `Problem`, `Problem.String`, `Result`, `NormalizeBase`, and `Build`; why-comments on relative-path rejection in `Build`, `.nojekyll`, knowledge-only front-matter stripping, `stripFrontMatter`, `findRung`, `findExperiments`, and `resolve`.
- `internal/site/markdown.go`: comments on `markdown` (GFM + `html.WithUnsafe`), `parseMarkdown`, `plainText` (entity/escape decoding outside code spans), `githubSlug`, `slugger.slug`, `addExperimentLinks`, `rewriteLinks` (include markers with optional line ranges), and `proseOnly`.
- `internal/site/page.go`: doc comments on `Labels` and `UILabels`; why-comments on README slugger sharing for generated `Reading path`/`Reference` section IDs and on `tableOfContents` (minimum two entries). Kept `//go:embed` directives verbatim.
- `internal/site/check.go`: doc comments on `CheckResult` and `CheckSite`; why-comments on `rawTextElements`, `openingTagAttributes` (tag-scoped quote-aware scanning so unescaped single quotes in SQL code blocks, prose, and HTML comments never produce false IDs or links), and `checkLink`.
- `internal/site/site_test.go`: one-line comment on each of the 24 test functions.
- `internal/repocheck/paths.go`: doc comments on `ExternalLink`, `PathSweep`, `MarkdownLink`, `PublicReferences`, `IsPublicReference`, `MachinePaths`, `MarkdownLinks`, `ResolveRelativeLink`, `SweepPaths`, and `CheckPaths`; why-comments on `isBinary` (git's 8,000-byte NUL heuristic), `MachinePaths` word-boundary guard, `upstreamCopies` (`SOURCES.sha256` exemption), deleted-file filtering via `os.Lstat`, and `_test.go` exclusion from external URL reporting.
- `internal/repocheck/paths_test.go`: one-line comment on each of the 6 test functions.
- `cmd/course/site.go`: comments on `runSite`, `checkSite`, `siteFiles`, `prepareSiteDir` (symlink resolution + `os.SameFile` guard and `.nojekyll` check), and `resolveExisting`.
- `cmd/course/site_test.go`: one-line comment on `TestPrepareSiteDirRefusesRepositoryThroughSymlink` (added in the repair round).
- `agent_work/pub/serve-check/main.go`: package doc comment and comments on `http.StripPrefix` subpath serving and same-site BFS crawling.

### Repository front page / site home page
- `README.md`: rewritten from scratch as the book's front page and GitHub Pages home page. Uses short `# SQLite Internals` title (used as the site header and `<title>` suffix), explains the book and Rebuild Ladder (E1/E2/E3 equality levels), lists all ten chapters in order with relative links, rungs R0–R7 where they land (and notes Chapters 4 and 7 add no rung), and one verified sentence per chapter on what it proves; gives the pinned SQLite `3.53.4` details (`sqlite/PIN`), experiment setup and run commands (`go 1.26.0`, `fetch-source`, `build-tools`, `docker compose up -d seaweedfs` for Chapter 10 and Experiment 7.4 `-s3`, Linux-only parts and the `docker compose run --rm lab` macOS route), relative links to chapter evidence, `answers.md`, `OUTLINE.md`, `docs/course-source.md`, `docs/knowledge/`, `agent_work/INDEX.md`, and `cmd/course/`, and the published site URL `https://nickstrad.github.io/walfleet/` with `.github/workflows/pages.yml` running `go run ./cmd/course site`. Headings avoid literal `Reading path` and `Reference` so the site's appended generated sections keep `#reading-path` and `#reference` IDs cleanly.

## Verification run
- `gofmt -l internal/site internal/repocheck cmd/course agent_work/pub/serve-check`: empty output.
- `go run ./cmd/course check gofmt`: `PASS check gofmt: 285 Go file(s) formatted`.
- `go build ./... && go vet ./...`: PASS.
- `go test ./internal/site/... ./internal/repocheck/... ./cmd/course/...`: PASS.
- `GOOS=darwin GOARCH=arm64 go build ./... && GOOS=darwin GOARCH=arm64 go vet ./...`: PASS.
- `go run ./cmd/course site -check -base /sqlite-internals/`: `PASS site -check: 39 page(s), 1756 link(s) resolve`.
- `go run ./cmd/course site -check`: `PASS site -check: 39 page(s), 1756 link(s) resolve`.
- `go run ./cmd/course check paths`: `PASS check paths: 327 tracked text file(s) free of machine-local paths and unresolved relative links (0 binary file(s) and 27 upstream SQLite copies skipped, 7 external link(s) reported)`.
- `go run ./agent_work/pub/serve-check` against a temporary build under `/sqlite-internals/`: `fetched 40 URL(s)`, `PASS: every fetched URL returned 200`.
- Comment-only diff check (`git diff -U0 -- '*.go'`): every changed line in every modified `.go` file is a pure comment addition; zero code tokens changed.

## Open doubts
- On the published home page (`index.html`), `README.md` now includes the ten-chapter overview required by the brief (and needed for GitHub visitors) right above the site generator's auto-generated `Reading path` (which adds the per-experiment anchor links). The two complement each other (chapter one-line summaries above, full experiment anchor list below) and have distinct headings (`Chapters and rungs` vs `Reading path`) so no anchor IDs collide.

## Suspected defects
1. **Chapter 6 rung missing in the generated `Reading path` (`internal/site/site.go:284-285`):** `rungDeclarationPattern` and `rungIDPattern` expect `(rung R\d+)` with a closing parenthesis immediately after the rung number. Chapter 6 (`ch06-locking-and-concurrency/ch06-locking-and-concurrency.md:106`) writes `**The Rebuild Ladder (rung R4 — Live snapshot).**` with the name inside the parentheses, so `findRung` returns `""` and the generated `Reading path` on `index.html` omits the `<span class="rung">Rung R4 (Live snapshot)</span>` badge for Chapter 6 (rungs R0–R3 and R5–R7 render as expected). Not changed here per the comments-only rule.
2. **Raw-text element closing-tag scan (`internal/site/check.go:120-126`):** Already noted as a nit in `agent_work/pub/REVIEW-3.md` (`bytes.ToLower` byte-length shift on special Unicode runes and missing delimiter check after `</tagName>`). Not changed here per the comments-only rule.
