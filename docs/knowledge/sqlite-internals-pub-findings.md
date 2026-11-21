---
title: "SQLite internals course: publishing the book as a GitHub Pages site"
summary: "How the sqlite-internals book is rendered and checked by its own Go command and deployed by a Pages workflow"
tags: [sqlite-internals,github-pages,static-site,goldmark]
updated: 2026-11-21
verified: 2026-11-21 — Linux amd64, Go 1.26, SQLite 3.53.4 via modernc.org/sqlite v1.60.1; evidence in the sqlite-internals repo
---

# SQLite internals course: publishing the book as a GitHub Pages site

How the sqlite-internals book is rendered and checked by its own Go command and deployed by a Pages workflow

Source: the `sqlite-internals` course repo (github.com/nickstrad/sqlite-internals), chunk `pub`. Working notes: `agent_work/pub/build-notes.md`; review and adversarial records in the same directory.


## How the book is published

- The site is built by this repo's own Go command: `go run ./cmd/course site -out _site -base /sqlite-internals/`. `go run ./cmd/course site -check -base /sqlite-internals/` renders into a temporary directory and fails on any broken internal link, missing anchor or duplicate id.
- `.github/workflows/pages.yml` runs the check and the build on every push to `main` and deploys with the GitHub Pages actions. Site: https://nickstrad.github.io/sqlite-internals/
- `README.md` is rendered as the home page; a generated "Reading path" section follows it with one link per chapter, its rung, and one anchored link per experiment. Each chapter, each `answers.md`, `OUTLINE.md` and every file in `docs/knowledge/` gets its own page. Code and evidence links point at the repository on GitHub.
- The only added dependency is `github.com/yuin/goldmark`. Code blocks are not syntax-highlighted (a deliberate choice, to keep the dependency set small).
- `go run ./cmd/course check paths` sweeps every tracked text file for machine-local paths and unresolved relative links.
- The link checker reads `href`, `src` and `id` only from real opening tags (a quote-aware scanner), so attribute-looking text inside code blocks is not mistaken for links. Known small limit: inside `script`/`style` bodies the end-tag search is not exact for some non-ASCII text; the generated pages contain no such elements.
