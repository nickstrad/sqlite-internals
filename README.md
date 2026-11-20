# SQLite Internals — experiment-driven source study

A course on how SQLite 3.53.4 actually works, built around Go experiments that prove each
claim. Its through-line: capture a database's durable state deterministically, rebuild a
working database from it, and show the rebuild is equal to the original.

- Course spec: [docs/course-source.md](docs/course-source.md)
- Plan for the build: [OUTLINE.md](OUTLINE.md) (awaiting sign-off; no chapters exist yet)
- Current state and how to resume: [STATE.md](STATE.md)
- Research behind the outline: [docs/knowledge/](docs/knowledge/)
- Every working script, indexed: [agent_work/INDEX.md](agent_work/INDEX.md)

Pinned source: tag `version-3.53.4`, git `b09c88c14082339b66c7b7158d609a771e64ca69`. Fetch and
verify it with `agent_work/fetch-pinned-sqlite.sh`.
