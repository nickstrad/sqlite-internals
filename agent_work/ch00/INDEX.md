# agent_work/ch00 index

| Path | Purpose | What it proved |
| --- | --- | --- |
| `agent_work/ch00/smoke/main.go` | Chunk 0 smoke: sqlitex open + pin check, one WAL insert under strace, SIGKILL at the first sync, reopen, C CLI twin of the same insert, CLI -vfstrace of one SELECT | All chunk 0 pieces work together; kill at the first sync loses the insert with integrity ok; Go driver and C CLI write the same byte ranges and sync the same files in the same order (fsync vs fdatasync differs by build); output identical across runs and between host and Debian container |
| `agent_work/ch00/smoke.txt` | Captured smoke output (host) | EXACT lines stable across two runs |
| `agent_work/ch00/build-notes.md` | Plain-data notes for the prose pass | — |
| `agent_work/ch00/BUILD-REPORT.md` | Build report | — |
| `agent_work/verify/crossbuild.sh` | linux build+vet+test, darwin/arm64 + darwin/amd64 build+vet+test-compile, gofmt | Root module cross-builds and vets for darwin with no cgo |
| `agent_work/verify/README.md` | Verification command list | — |
| `agent_work/probes/seaweedfs-s3/awsprobe/main.go` | S3 semantics against SeaweedFS 4.48 with aws-sdk-go-v2: CRUD, read/list-after-write, list order, If-None-Match/If-Match incl. 2- and 8-writer races, multipart visibility | Conditional puts are enforced and atomic among concurrent writers; multipart invisible until Complete; listing is not byte-ordered when `/` meets bytes < 0x2f |
| `agent_work/probes/seaweedfs-s3/minioprobe/main.go` | Same conditional-put cases with minio-go | minio-go's SetMatchETag/SetMatchETagExcept behave the same against SeaweedFS |
| `agent_work/probes/seaweedfs-s3/weight/{aws,minio}/main.go` | Minimal programs for dependency and binary-size comparison | aws: 12 modules / 68 pkgs; minio: 19 modules / 42 pkgs; both cross-build for darwin without cgo |
| `agent_work/probes/seaweedfs-s3/RESULT.md`, `run-*.txt` | Result data and raw outputs | — |
| `agent_work/probes/vfs-viability/shim/shim.go` | Writable forwarding VFS in Go over modernc's default unix VFS, logging + fault injection | A Go VFS registers with the translated library (no cgo, no fork); works in rollback and WAL (io_methods v3) |
| `agent_work/probes/vfs-viability/writable/main.go` | Drives the shim: rollback, WAL, two-connection snapshot, xWrite/xSync fault injection, v1 vs v3 | WAL needs xShm*; v1 silently stays in delete mode; injected faults give SQLITE_IOERR with the row absent and integrity ok; deterministic |
| `agent_work/probes/vfs-viability/readonly/main.go` | modernc `vfs` (read-only fs.FS): logging FS, fstest.MapFS, HTTP range reader, WAL-format files | Point lookup = 10 fs ops / HTTP requests; WAL-format files need immutable=1 and then hide uncheckpointed frames; writes fail with SQLITE_NOMEM |
| `agent_work/probes/vfs-viability/RESULT.md`, `run-*.txt` | Result data and raw outputs | — |
