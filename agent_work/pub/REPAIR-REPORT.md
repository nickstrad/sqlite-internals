# Chunk pub — repair round report (Opus)

Inputs: REVIEW-1.md (VERDICT FAIL, 8 findings), adversarial/results-run1.txt (24 BROKEN of 46).
Commits: `ef9d046` repocheck: parse markdown links with goldmark and skip upstream copies in check paths;
`73a8a3d` site: fix link check, front matter, rung, slug and -out symlink edge cases; plus this report commit.
After repair the adversarial suite reports **45 HELD, 1 BROKEN** (PATH-03, answered below).

## Findings and attacks

| id | outcome | test added |
| --- | --- | --- |
| Review 1 (blocker) — check paths fails on upstream SQLite comments | fixed `ef9d046`: files named in any tracked `SOURCES.sha256` are not scanned (listed as INFO), still valid link targets | `TestSweepPathsSkipsUpstreamCopiesAndDeletedFiles` |
| Review 2 — CRLF fence / backtick info string hide later links | fixed `ef9d046`: markdown links now extracted from goldmark's AST (no line-based fence tracking) | `TestMarkdownLinksAndFences` (rewritten) |
| Review 3 — code-span length, single-quote/paren titles, angle dup, ref with spaces | fixed `ef9d046` (same goldmark change; one report per unresolved destination per file) | `TestMarkdownLinksAndFences` |
| Review 4 — `/book//x/#a` skips anchor check; single-quoted attrs | fixed `73a8a3d` (`TrimLeft` of `/`, single-quoted attribute regex) | `TestCheckSiteHTMLEdges` |
| Review 5 (nit) — `include: p lines=A-B` not converted | fixed `73a8a3d`: label `p:A-B`, target `#LA-LB` | `TestIncludeMarkerWithLineRange` |
| Review 6 (nit) — front matter stripped on README | fixed `73a8a3d`: only on knowledge pages | `TestFrontMatterOnlyOnKnowledgePages` |
| Review 7 (nit) — absolute `Options.Files` loops forever | fixed `73a8a3d`: `Build` returns an error | `TestBuildRejectsAbsoluteFiles` |
| Review 8 (nit) — usage line for check paths | fixed `ef9d046` | — (string) |
| LIVE-06b — `-out <symlink>/repo` wiped the repo | fixed `73a8a3d`: symlinks resolved on both sides + `os.SameFile` against repo and its parents | `TestPrepareSiteDirRefusesRepositoryThroughSymlink` |
| LINK-04 single-quoted href/src | fixed `73a8a3d` | `TestCheckSiteHTMLEdges` |
| LINK-05 id inside HTML comment | fixed `73a8a3d`: comments removed before scanning | `TestCheckSiteHTMLEdges` |
| LINK-06 href inside HTML comment | fixed `73a8a3d` (same) | `TestCheckSiteHTMLEdges` |
| LINK-07 `?query` on relative links | fixed `73a8a3d` (query ignored for resolution, kept on GitHub URLs) | `TestQueryStringsAndEscapesInRelativeLinks` |
| LINK-10b duplicate DOM id | fixed `73a8a3d`: `duplicate id "<id>"` problem | `TestCheckSiteHTMLEdges` |
| MD-01a quote inside quoted title | fixed `73a8a3d`: one quote pair removed, YAML unescaping | `TestStripFrontMatterEdges` |
| MD-01b empty front matter | fixed `73a8a3d`: closing `---` is first line equal to `---` | `TestStripFrontMatterEdges` |
| MD-01c closing `---` at EOF | fixed `73a8a3d` | `TestStripFrontMatterEdges` |
| MD-02a entity in heading → wrong slug | fixed `73a8a3d`: escapes/entities decoded outside code spans | `TestHeadingIDsFromEntitiesAndPunctuation` |
| MD-02b punctuation-only heading → empty id | fixed `73a8a3d`: id `section`, `section-1`, … | `TestHeadingIDsFromEntitiesAndPunctuation` |
| MD-04b helper dir listed as experiment | fixed `73a8a3d`: only `N.M-*` dirs | `TestOnlyNumberedExperimentDirectoriesAreListed` |
| MD-05a back-referenced rung wins | fixed `73a8a3d`: bold declaration preferred | `TestRungIgnoresCodeAndPrefersDeclaration` |
| MD-05b rung inside code block | fixed `73a8a3d`: code blocks/spans/HTML blocks blanked first | `TestRungIgnoresCodeAndPrefersDeclaration` |
| MD-07a Answers link missing when <2 headings | fixed `73a8a3d`: sidebar shown for TOC or Answers | `TestAnswersLinkWithoutTableOfContents` |
| PATH-01 real repo fails check paths | fixed `ef9d046` (= review 1); `check paths` now PASS | as review 1 |
| PATH-03 bare `/root`, `/home/x`, `/Users/x`, `/users/x/` | **fixed `ef9d046`, attack still prints BROKEN — attack is wrong in its counting**: all four paths are detected, but the sweep reports one problem per line listing every path on it (the design, already tested before). The run's own GOT text shows it: `problems=1 ([docs/bare.md:1: mentions a machine-local path /root, /home/alice, /Users/bob, /users/bob/: …])`. The attack requires `len(Problems) >= 4` | `TestMachinePathsWithoutTrailingSlash` |
| PATH-05a/b double-backtick code spans | fixed `ef9d046` (goldmark) | `TestMarkdownLinksAndFences` |
| PATH-06 title forms, multiline, dup, ref with spaces | fixed `ef9d046` (goldmark) | `TestMarkdownLinksAndFences` |
| PATH-07a/b inline triple backticks, 5-space indented fence | fixed `ef9d046` (goldmark) | `TestMarkdownLinksAndFences` |
| PATH-08a `..` in allowlisted URL | fixed `ef9d046`: `path.Clean` before prefix match | `TestIsPublicReferenceRejectsDotDot` |
| PATH-09b link to tracked-but-deleted file | fixed `ef9d046`: files missing on disk are not link targets | `TestSweepPathsSkipsUpstreamCopiesAndDeletedFiles` |

## Stale prose for Neon

No comments or chapter exist for this chunk yet, so nothing in a `.go` comment or chapter is stale.
build-notes.md has been corrected in place (§2 `-out` safety and runtime, §3 page count, front matter,
rung detection, experiment directories, Contents/Answers sidebar, heading ids, link table, include
markers, §8 check-paths behaviour, API, summary line, correction 1 now resolved on main) and has a new
§9 with the remaining changes. Facts that changed:
- `internal/site/check.go`: single-quoted attrs; HTML comments ignored; duplicate ids fail; `?query` ignored; repeated `/` after base handled.
- `internal/site/site.go`: front matter only on knowledge pages, new closing rule, quote handling; rung = bold declaration first, code excluded; only `N.M-*` experiment dirs; `?query` kept on GitHub links; absolute `Files` rejected.
- `internal/site/markdown.go`: heading text decodes escapes/entities outside code spans; empty slug → `section`; `include … lines=A-B`.
- `internal/site/page.html`: sidebar shown when there is a TOC **or** an Answers link.
- `cmd/course/site.go`: `-out` containment test resolves symlinks and uses `os.SameFile`.
- `internal/repocheck/paths.go`: goldmark link extraction, `MarkdownLinks(source []byte) []MarkdownLink`, `fenceState` removed, `PathSweep.UpstreamCopies`, broader machine-path pattern, deleted files excluded, `IsPublicReference` cleans the path.
- `cmd/course`: check-paths summary line now `(<b> binary file(s) and <u> upstream SQLite copies skipped, <e> external link(s) reported)`; new INFO line per upstream copy; usage line updated.
- build-notes §7 shows the build-time run (14 pages, 517 links, before the main merge); now 23 pages, 1045 links (§9).

## Commands run

| command | result |
| --- | --- |
| `gofmt -l internal cmd agent_work/pub` | empty |
| `go build ./... && go vet ./... && go test ./...` | PASS (all packages ok) |
| `bash agent_work/verify/crossbuild.sh` | CROSSBUILD OK (darwin/arm64 and linux/amd64 build+vet, check gofmt 154 files) |
| `SQLITE_SRC=… go run ./cmd/course check all` | exit 1, **not caused by this chunk**: sources, spans, inline, gofmt, **paths PASS**; experiments all PASS except (a) 5.1-wal-inspector DIFF: `showwal` is not built in this worktree (`NO-SHOWWAL… run: go run ./cmd/course build-tools`) — environment; (b) 7.3-go-vfs-shim BUILD-FAIL from a module-cache race (`…x/sys/@v/v0.48.0.partial: no such file or directory`, a concurrent download by another process); `course check evidence ch07` re-run: 7.1–7.4 all PASS, 2 runs identical |
| `go run ./cmd/course check paths` | PASS: 184 text files, 0 binary, 17 upstream copies skipped, 7 external links |
| `course site -out _site -base /sqlite-internals/` twice (two dirs) + `diff -r` | 23 pages each, identical |
| `course site -check -base /sqlite-internals/` and `course site -check` | PASS: 23 page(s), 1045 link(s) resolve (both) |
| pre-repair binary vs repaired binary, same repo, `diff -r` of output | identical (no rendering change on the real book) |
| `go run ./agent_work/pub/serve-check -dir _site -base /sqlite-internals/ "" ch01… ch02… ch04… ch05… ch07…` | fetched 24 URLs, all 200 |
| `bash agent_work/pub/adversarial/run-all.sh` | 45 HELD, 1 BROKEN (PATH-03, answered above); results in `adversarial/results.txt`, 23 s |
