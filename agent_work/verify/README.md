# agent_work/verify

| Command (repo root) | Checks | Exit |
| --- | --- | --- |
| `agent_work/verify/crossbuild.sh` | linux/amd64 `go build ./...` + `go vet ./...`; `go test ./...` on the host; darwin/arm64 and darwin/amd64 `go build`, `go vet`, and test-binary compile (`go test -run '^$' -exec /bin/true`); `go run ./cmd/course check gofmt` | 0 = all pass, 1 = any step failed (each step prints ok/FAILED) |
| `go run ./cmd/course check all` | sources, spans, inline -check, evidence (2 runs), gofmt, machine-local path sweep | 0/1 |
| `docker compose run --rm lab go test ./...` | unit tests inside the Linux lab image (strace available) | 0/1 |
| `docker compose run --rm lab go run ./cmd/course check all` | the repo checks inside the lab image | 0/1 |

Notes:
- Nested modules under `agent_work/probes/` are outside `./...` of the root module and are not built by crossbuild.sh.
- In a git worktree, git inside the lab container needs the main repository's `.git` mounted at the same path and the worktree mounted at its own path, e.g. `docker compose run --rm -v "$G:$G" -v "$PWD:$PWD" -w "$PWD" lab ...` with `G=<main checkout>/.git`. A normal clone needs nothing extra.
- Runtime on this VM: crossbuild.sh about 46 s with a warm build cache.
