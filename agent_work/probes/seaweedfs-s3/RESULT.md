# Probe result: SeaweedFS 4.48 S3 semantics (chunk 0, 2026-11-20)

Plain data for the orchestrator. Store: `docker compose up -d seaweedfs` (compose.yaml at repo root),
image `chrislusf/seaweedfs:4.48`, single container `server -s3`, credentials from
`docker/seaweedfs/s3.json` (access key `course`), host port `${COURSE_S3_HOST_PORT:-18333}`,
in-network endpoint `http://seaweedfs:8333`. Region string `us-east-1`, path-style addressing.

## Commands

| Command (from `agent_work/probes/seaweedfs-s3/`) | Output file |
| --- | --- |
| `go run ./awsprobe` (host, endpoint 127.0.0.1:18333) | `run-host-awsprobe.txt` |
| `docker compose run --rm lab sh -c 'cd agent_work/probes/seaweedfs-s3 && PROBE_BUCKET=course-probe-lab go run ./awsprobe && PROBE_BUCKET=course-probe-lab-minio go run ./minioprobe'` | `run-lab-container.txt` (also has the vfs-viability shim run in the container) |
| `go run ./minioprobe` (host) | printed only; same results as in the container |

Host run and container run printed identical semantics.

## Results

| Question | Result |
| --- | --- |
| Create bucket | OK; second create returns `BucketAlreadyOwnedByYou` |
| Put then get | body and ETag round-trip; ETag = MD5 hex of the body for single-part puts |
| Read-after-overwrite | 100 put-then-get rounds on one key: 0 stale reads |
| List-after-write | 50 put-then-list rounds: new key missing from the listing 0 times |
| List order | NOT plain byte order. Keys `k/a`, `k/a-`, `k/a/x` list as `k/a`, `k/a/x`, `k/a-` (byte order puts `k/a-` (0x2d) before `k/a/x` (0x2f)). SeaweedFS stores keys as filer paths and walks a "directory" `k/a/` right after the entry `k/a`. Keys that do not mix `/` with bytes below 0x2f at the same position listed in byte order (`k/1, k/10, k/2, k/A, k/_, k/a, k/b, k/é`). Pagination with MaxKeys=3 and continuation tokens: 4 pages, no duplicates, no gaps |
| Overwrite | new body visible immediately; ETag changes |
| `If-None-Match: *` on an existing key | `412 PreconditionFailed`; body unchanged |
| `If-None-Match: *` on an absent key | OK |
| `If-Match: <stale etag>` | `412 PreconditionFailed`; body unchanged |
| `If-Match: <current etag>` | OK; body replaced, new ETag |
| `If-Match` on an absent key | `412 PreconditionFailed` |
| Concurrent `If-None-Match: *`, 2 writers x 50 rounds | exactly one winner in 50/50 rounds; stored body is the winner's in 50/50 |
| Concurrent `If-None-Match: *`, 8 writers x 20 rounds | exactly one winner in 20/20 rounds (1 OK, 7 PreconditionFailed each round); winner's body stored in 20/20 |
| Concurrent `If-Match` on the same ETag, 2 writers x 50 rounds | exactly one winner in 50/50; winner's body stored in 50/50 |
| Concurrent `If-Match` on the same ETag, 8 writers x 20 rounds | exactly one winner in 20/20; winner's body stored in 20/20 |
| Multipart upload before Complete | GET `NoSuchKey`; object absent from ListObjectsV2; ListMultipartUploads shows 1 in-progress upload |
| Multipart Complete | object appears whole (5 MiB + 4 B part sizes, size 5242884) |
| Multipart Abort | object never visible (`NoSuchKey`) |
| CompleteMultipartUpload with `If-None-Match: *` over an existing key | `412 PreconditionFailed`; existing object unchanged |
| Delete then get | `NoSuchKey`; `If-None-Match: *` after delete succeeds |
| minio-go `SetMatchETagExcept("*")` / `SetMatchETag(etag)` | same 412/OK behaviour; concurrent 2 writers x 30 rounds: exactly one winner in 30/30 |

Concurrency here means separate goroutines, each with its own client (own HTTP connections),
released together from a channel. Not tested: separate processes on separate hosts, server
restarts mid-request, replication other than `000`.

## Operational facts found

- SeaweedFS creates a collection per bucket and grows 7 volumes for it on first write. With
  `-volume.max=20` the third bucket failed every PUT with HTTP 500 `InternalError`
  ("No writable volumes and no free volumes left"; seen in `docker compose logs seaweedfs`).
  compose.yaml now uses `-volume.max=100`. A platform should use one bucket with key prefixes
  rather than a bucket per database.
- aws-sdk-go-v2 retries the 500 three times before failing (`exceeded maximum number of attempts, 3`).
- Path-style addressing is required (`UsePathStyle: true` for aws-sdk-go-v2; minio-go uses
  path style automatically for an IP or non-AWS host).
- Ports in use by the other SeaweedFS on this VM (8080, 8333, 8888, 9333) are not published by
  this compose file; only the S3 port is published, default host port 18333.

## Go client choice

Measured with `weight/aws` and `weight/minio` (one ListBuckets call each, CGO_ENABLED=0):

| | aws-sdk-go-v2 (`service/s3` v1.116.0, core v1.48.0) | minio-go v7.4.1 |
| --- | --- | --- |
| Modules in the build (excluding our own) | 12, all `github.com/aws/*` (aws-sdk-go-v2 + smithy-go) | 19 (incl. golang.org/x/crypto, x/net, x/sys, x/text, klauspost/compress, klauspost/cpuid, minio/md5-simd, tinylib/msgp, yaml, ini, uuid, xid, xxhash, xxh3) |
| Non-std packages linked | 68 | 42 |
| Binary size linux/amd64 | 11,316,816 B | 10,623,908 B |
| Binary size darwin/arm64 | 10,815,474 B | 10,219,362 B |
| darwin/arm64 + linux/amd64 build without cgo | yes | yes |
| Conditional put API | typed fields `PutObjectInput.IfNoneMatch`, `PutObjectInput.IfMatch`, also on `CompleteMultipartUploadInput` | `PutObjectOptions.SetMatchETagExcept("*")`, `SetMatchETag(etag)` (documented in its source as a "MinIO specific extension") |
| Error code access | `smithy.APIError.ErrorCode()` -> `PreconditionFailed` | `minio.ToErrorResponse(err).Code` -> `PreconditionFailed` |
| Worked against SeaweedFS 4.48 for every case above | yes | yes (conditional cases only were run) |

Recommendation for the capstone: **aws-sdk-go-v2 (`github.com/aws/aws-sdk-go-v2/service/s3`)**.
Reasons: conditional writes are first-class typed request fields that mirror the S3 API
(including on CompleteMultipartUpload); one publisher for all 12 modules; no x/crypto or
assembly-heavy dependencies. Cost: more packages linked and ~0.6 MB larger binary.
Pin the versions above when chapter 10 adds the dependency.

## What the capstone can rely on (from this probe, SeaweedFS 4.48 only)

- Put-if-absent (`If-None-Match: *`) is atomic among concurrent writers: usable for ownership
  claims / leases and for "first writer of segment N wins".
- Compare-and-swap (`If-Match: <etag>`) is atomic among concurrent writers: usable for a
  manifest pointer that moves forward.
- Read-after-write and list-after-write were consistent in every round run.
- In-progress multipart uploads are invisible; Complete is all-or-nothing.
- Do not depend on byte-order listing when keys at one level mix `/` with `-`, `.` or other
  bytes below 0x2f; use fixed-width names (e.g. zero-padded segment numbers) under a prefix.
