# Build report — publishing chunk (`pub`)

Branch `chunk/pub`, base `84f4699`. Builder: Opus. Code only; prose and Go comments are Neon's.

## What was built
- `internal/site` + tests: a pure-Go static site generator (goldmark v1.8.7, GFM; no highlighting, see notes §1). Home = README + generated reading path (chapters, rungs, experiment anchors) + Reference (outline, knowledge pages); one page per chapter with Contents sidebar, Previous/Next, Answers link; answers pages; outline page; knowledge pages with front matter stripped; GitHub-compatible heading ids; relative links rewritten to site pages or GitHub blob/tree URLs; include markers turned into source links; per-experiment Code · Evidence line; one embedded CSS (light/dark, offline); deterministic output; `.nojekyll`.
- `go run ./cmd/course site [-out _site] [-base /sqlite-internals/] [-repo URL] [-branch main] [-check]`.
- `.github/workflows/pages.yml` (checkout v7, setup-go v7, configure-pages v6, upload-pages-artifact v5, deploy-pages v5; `pages: write`, `id-token: write`, concurrency `pages`).
- `_site/` in `.gitignore`.
- `course check paths` extended: binary files skipped, tracked-but-ignored files listed, `/root/`, `/home/<name>/`, `/Users/<name>/` fail, unresolved relative markdown links fail, external non-allowlisted links reported (not failing). Unit-tested.
- `agent_work/pub/serve-check`: local server + crawler under the base path.

Commits: `6716cfe` site, `eeb8db7` repocheck sweep, `00c7ce0` repocheck test-fixture exclusion, `2b04aa6` ci workflow, `2fdd26a` agent_work notes/index, then this report.

## Commands run
| command | result |
| --- | --- |
| `gofmt -l` on the chunk's files | nothing |
| `go build ./... && go vet ./... && go test ./...` | PASS (12 packages with tests ok) |
| `GOOS=darwin GOARCH=arm64 go build ./... && … go vet ./...` | PASS (darwin: cross-built only) |
| `GOOS=linux GOARCH=amd64 go build ./... && … go vet ./...` | PASS |
| `go run ./cmd/course check all -skip-evidence` (with `SQLITE_SRC` set) | PASS: sources, spans, inline, gofmt (90 files), paths (120 files) |
| `go run ./cmd/course site -check` and `-check -base /sqlite-internals/` | PASS: 14 pages, 517 links resolve |
| `go run ./cmd/course site -out _site -base /sqlite-internals/` twice, `diff -r` | identical |
| `go run ./agent_work/pub/serve-check -dir _site -base /sqlite-internals/ …` | PASS: 15 URLs, all 200 |
| `go run github.com/rhysd/actionlint/cmd/actionlint@latest .github/workflows/pages.yml` (v1.7.12) | no findings |

Evidence check not run: this chunk has no experiments. Nothing pushed; the workflow was not triggered.

## Corrections to the brief
1. `check paths` already used `git ls-files`; it tripped because `_tools/bin/kb` (18.6 MB binary) is **tracked** despite `/_tools/` in `.gitignore` (commit `c51ff44`). Now skipped as binary and reported as tracked-but-ignored. Whether to untrack it is an open decision for Nick/Fable.
2. `configure-pages` runs before the site steps so its `base_path` output feeds `-base`.

## Left undone / for others
- Neon: doc comments for the new exported API (`internal/site`, `internal/repocheck/paths.go`, `CheckPaths` lost its stale comment), README content for the home page (notes §5: current README says no chapters exist).
- `site -check` is not part of `course check all` (workflow runs it); one-line addition if wanted.
