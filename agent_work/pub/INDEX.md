# agent_work/pub — working scripts of the publishing chunk

| path | purpose | what it proved |
| --- | --- | --- |
| `agent_work/pub/serve-check/main.go` | `go run ./agent_work/pub/serve-check -dir _site -base /sqlite-internals/ [start pages…]`: serve a built site under the base path on a random localhost port and crawl every same-site link | All 15 URLs of the built site (home, 2 chapters, 2 answers pages, outline, 8 knowledge pages, style.css) return 200 when served under `/sqlite-internals/` |
| `agent_work/pub/build-notes.md` | plain-data notes for the prose pass | — |
| `agent_work/pub/BUILD-REPORT.md` | build report | — |
