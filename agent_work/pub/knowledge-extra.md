
## How the book is published

- The site is built by this repo's own Go command: `go run ./cmd/course site -out _site -base /sqlite-internals/`. `go run ./cmd/course site -check -base /sqlite-internals/` renders into a temporary directory and fails on any broken internal link, missing anchor or duplicate id.
- `.github/workflows/pages.yml` runs the check and the build on every push to `main` and deploys with the GitHub Pages actions. Site: https://nickstrad.github.io/sqlite-internals/
- `README.md` is rendered as the home page; a generated "Reading path" section follows it with one link per chapter, its rung, and one anchored link per experiment. Each chapter, each `answers.md`, `OUTLINE.md` and every file in `docs/knowledge/` gets its own page. Code and evidence links point at the repository on GitHub.
- The only added dependency is `github.com/yuin/goldmark`. Code blocks are not syntax-highlighted (a deliberate choice, to keep the dependency set small).
- `go run ./cmd/course check paths` sweeps every tracked text file for machine-local paths and unresolved relative links.
- The link checker reads `href`, `src` and `id` only from real opening tags (a quote-aware scanner), so attribute-looking text inside code blocks is not mistaken for links. Known small limit: inside `script`/`style` bodies the end-tag search is not exact for some non-ASCII text; the generated pages contain no such elements.
