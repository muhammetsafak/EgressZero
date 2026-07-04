# EgressZero

[![CI](https://github.com/muhammetsafak/EgressZero/actions/workflows/ci.yml/badge.svg)](https://github.com/muhammetsafak/EgressZero/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/muhammetsafak/egresszero.svg)](https://pkg.go.dev/github.com/muhammetsafak/egresszero)
[![Go Report Card](https://goreportcard.com/badge/github.com/muhammetsafak/egresszero)](https://goreportcard.com/report/github.com/muhammetsafak/egresszero)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

A high-performance, lightweight Go reverse proxy for AWS S3 and S3-compatible object storage, built to sit behind Cloudflare (or any CDN) so that files are cached at the edge and **S3 egress costs drop to ~zero**.

```
client ──► Cloudflare edge ──► EgressZero (origin) ──► S3 / R2 / MinIO / B2
                 │
                 └── cache HIT: served from the edge, S3 never billed
```

The proxy streams objects straight from S3 to the client — bodies are never buffered in memory, so a 1 GB video download costs the proxy a few kilobytes of RAM. Cache-critical headers (`ETag`, `Cache-Control`, `Content-Type`, `Content-Length`, `Last-Modified`, …) pass through untouched, and requests to private buckets are signed with AWS Signature V4 behind the scenes.

## Features

- **True streaming** — constant memory per request regardless of object size; allocations per request are flat (verified by tests and benchmarks)
- **Range requests** — `Range` is forwarded to S3, `206 Partial Content` with `Content-Range` comes back (video seeking, CDN chunked caching)
- **Conditional requests** — `If-None-Match` / `If-Modified-Since` forwarded, clean `304 Not Modified` responses for cheap revalidation; `If-Range` honored (serve the range only if the validator still matches, else the full body) for correct resumable downloads
- **Private buckets** — SigV4 signing via the official `aws-sdk-go-v2` credential chain (env vars, shared config, IMDS/IRSA)
- **S3-compatible endpoints** — Cloudflare R2, MinIO, Backblaze B2 via `S3_ENDPOINT`
- **Cache-Control injection** — override the upstream `Cache-Control` with `CACHE_CONTROL`
- **CDN-only protection** — optional shared-secret header check so nobody bypasses the CDN and re-bills your egress
- **No leaks** — S3 error details, request IDs and bucket names never reach clients; error responses are `no-store`
- **Optional Prometheus metrics** — request/duration/bytes, in-flight gauge, S3 first-byte latency and upstream errors, on a separate listener so `/metrics` stays off the CDN
- **Request coalescing** — concurrent identical revalidations (`If-None-Match`) and HEADs collapse into one S3 call, blunting cold-cache stampedes without ever multiplexing a body stream
- **Single static binary** — distroless Docker image, configured entirely by environment variables

## Quickstart

```sh
docker run -p 8080:8080 \
  -e S3_BUCKET=my-bucket \
  -e AWS_REGION=eu-central-1 \
  -e AWS_ACCESS_KEY_ID=... \
  -e AWS_SECRET_ACCESS_KEY=... \
  -e CACHE_CONTROL="public, max-age=31536000, immutable" \
  ghcr.io/muhammetsafak/egresszero   # or: docker build -t egresszero . && docker run ... egresszero
```

`GET /path/to/object` maps to `s3://my-bucket/path/to/object`. Only `GET` and `HEAD` are served. `GET /healthz` is a dependency-free health probe (never touches S3, exempt from auth).

No Docker? Prebuilt static binaries for Linux, macOS and Windows are attached to every [release](https://github.com/muhammetsafak/EgressZero/releases), or install from source:

```sh
go install github.com/muhammetsafak/egresszero/cmd/egresszero@latest
```

## Configuration

| Env var | Required | Default | Description |
|---|---|---|---|
| `S3_BUCKET` | **yes** | — | Bucket to serve objects from |
| `AWS_REGION` | no | SDK chain, else `us-east-1` | Region for SigV4 (R2 users: `auto`) |
| `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` | no | SDK default chain | Read by the SDK; IMDS/IRSA also work |
| `S3_ENDPOINT` | no | real AWS S3 | Custom endpoint URL (R2/MinIO/B2) |
| `S3_FORCE_PATH_STYLE` | no | `false` | Path-style addressing (`true` for MinIO) |
| `S3_KEY_PREFIX` | no | `""` | Prepended verbatim to every object key (include the trailing `/` yourself) |
| `LISTEN_ADDR` | no | `:8080` | HTTP listen address |
| `METRICS_ADDR` | no | disabled | Bind a **separate** listener serving Prometheus metrics at `/metrics` (e.g. `:9090`). Kept off the main listener so it is never reachable through the CDN. Empty disables metrics |
| `CACHE_CONTROL` | no | pass upstream through | Replaces `Cache-Control` on 200/206/304 responses |
| `NOT_FOUND_CACHE_CONTROL` | no | `no-store` | `Cache-Control` for 404 responses (e.g. `public, max-age=60`) so the edge absorbs repeat lookups of missing keys. Beware: newly uploaded objects stay invisible for up to that TTL. Other errors are always `no-store` |
| `PROXY_AUTH_SECRET` | no | disabled | Enables CDN-only protection (see below) |
| `PROXY_AUTH_HEADER` | no | `X-Proxy-Auth` | Header carrying the secret |
| `LOG_LEVEL` | no | `info` | `debug`, `info`, `warn`, `error` |
| `LOG_REQUESTS` | no | `false` | One structured JSON log line per request |
| `SHUTDOWN_TIMEOUT` | no | `15s` | Graceful-shutdown drain window |
| `WRITE_IDLE_TIMEOUT` | no | `2m` | Disconnect a client that has not accepted any body bytes for this long (rolling deadline; slow-but-alive clients are unaffected). `0` disables |
| `COALESCE` | no | `true` | Collapse concurrent identical revalidations and HEADs into one S3 call. `false` disables |

Startup fails fast and reports **all** configuration errors at once.

## Cloudflare setup

1. **DNS**: point a proxied (orange-cloud) record at the server running EgressZero.
2. **Cache Rule**: *Rules → Cache Rules* — match your hostname, set **Eligible for cache**, and leave Edge TTL on **respect origin** so the `CACHE_CONTROL` you configure on the proxy governs edge caching. For immutable assets `public, max-age=31536000, immutable` is ideal.
3. **CDN-only protection** (recommended): set `PROXY_AUTH_SECRET` on the proxy, then add a *Transform Rule → Modify Request Header*: set `X-Proxy-Auth` to the same secret. Anyone hitting the origin directly (without the header Cloudflare adds) gets `403`, so the CDN cannot be bypassed to re-bill your S3 egress. Also consider firewalling the origin to [Cloudflare IP ranges](https://www.cloudflare.com/ips/).
4. **Query strings are ignored** by the proxy — configure your cache key accordingly (e.g. *ignore query string*) to avoid fragmenting the cache.
5. Free/Pro plans cap cacheable objects at 512 MB; larger files need chunked caching (Enterprise) or R2.

## Examples

**Cloudflare R2**

```sh
S3_BUCKET=assets \
S3_ENDPOINT=https://<account-id>.r2.cloudflarestorage.com \
AWS_REGION=auto \
AWS_ACCESS_KEY_ID=<r2-key> AWS_SECRET_ACCESS_KEY=<r2-secret> \
./egresszero
```

**MinIO (local development)**

```sh
docker compose up --build
curl -i http://localhost:8080/hello.txt
curl -i -H "Range: bytes=0-99" http://localhost:8080/sample.bin       # 206
curl -i -H 'If-None-Match: "<etag>"' http://localhost:8080/hello.txt  # 304
```

The bundled `docker-compose.yml` starts MinIO, seeds a `demo` bucket and runs the proxy against it.

## Behavior details

- **Status mapping**: missing key → `404`, access denied → `403`, unsatisfiable range → `416`, precondition failed → `412`, upstream failure → `502`, upstream timeout → `504`, non-GET/HEAD → `405`. Error bodies are generic fixed strings with `Cache-Control: no-store` (404 can opt into negative caching via `NOT_FOUND_CACHE_CONTROL`).
- **Keys**: the URL path is percent-decoded and used as the object key (leading `/` stripped, `S3_KEY_PREFIX` prepended). Paths containing `.` / `..` segments are rejected with `400`; embedded `//` in keys is preserved.
- **Headers forwarded**: `Content-Type`, `Content-Length`, `ETag`, `Last-Modified`, `Cache-Control`, `Content-Encoding`, `Content-Disposition`, `Content-Language`, `Expires`, `Content-Range`, plus `Accept-Ranges: bytes`. Nothing else (no `x-amz-*`, SSE or version headers).
- **Memory**: per-request cost is flat (a 1 GB download costs the same as a 1 KB one), but total RSS scales with the number of *simultaneously active* streams — roughly 150–260 KB each at high concurrency. Budget on real simultaneity, not peak request count. `GOMEMLIMIT` trims the runtime's GC headroom (it cut RSS ~35% at 1000 concurrent in testing) but cannot go below the live working set. See [docs/loadtest.md](docs/loadtest.md) for the reproducible benchmark and measured numbers.
- **Stalled clients**: the server's `WriteTimeout` is deliberately 0 (a fixed value would kill long downloads); instead, a rolling per-write deadline (`WRITE_IDLE_TIMEOUT`, default 2m) disconnects clients that stop accepting bytes while leaving slow-but-active downloads untouched.
- **Coalescing**: with `COALESCE` on (default), concurrent identical revalidations (`If-None-Match`/`If-Modified-Since`) and HEADs collapse into a single S3 call — the shared result is always body-less (a 304 or metadata), so nothing is buffered. If a coalesced revalidation turns out to be a `200` (the object changed), the body is **not** shared: the request that made the call streams it, and the others each issue their own GET, so a stream is never multiplexed across clients and the memory ceiling holds. Full-body cache-miss downloads are never coalesced, for the same reason. The shared upstream call is detached from any single client's context, so one client disconnecting does not fail the others.

## Limitations (by design, documented)

- Multi-range requests (`bytes=0-1,5-9`) are passed to S3, which RFC-correctly answers with the full `200` body.
- `Range` is not forwarded on `HEAD` requests.
- Query strings are ignored entirely.

## Metrics

Set `METRICS_ADDR` (e.g. `:9090`) to expose Prometheus metrics at `/metrics` on a dedicated listener — keep that port private (bind it to an internal interface or firewall it), never proxy it through Cloudflare. Exposed series:

| Metric | Type | Labels |
|---|---|---|
| `egresszero_requests_total` | counter | `method`, `status` |
| `egresszero_request_duration_seconds` | histogram | `method` |
| `egresszero_response_bytes_total` | counter | — |
| `egresszero_in_flight_requests` | gauge | — |
| `egresszero_upstream_request_duration_seconds` | histogram | — (S3 time to first byte) |
| `egresszero_upstream_errors_total` | counter | `status` (4xx/5xx and 499) |
| `egresszero_coalesced_requests_total` | counter | — (requests served from a shared upstream call) |

Standard Go runtime and process collectors (`go_*`, `process_*`) are included. Health probes to `/healthz` are excluded from request metrics.

## Development

```sh
make test        # unit tests (full handler matrix)
make test-race   # with the race detector
make bench       # streaming benchmark (-benchmem; allocs/op must stay flat)
make lint        # go vet (+ staticcheck if installed)
make compose-up  # MinIO + proxy end-to-end stack
```

`go test ./internal/proxy/ -run TestMemoryCeiling` proves the streaming memory ceiling: 50 concurrent 256 MB downloads held mid-flight may not grow the heap by more than 20 MB. For a full end-to-end memory/concurrency benchmark (up to 1000 simultaneous streams against MinIO, with real RSS numbers), see [docs/loadtest.md](docs/loadtest.md) and `scripts/loadtest.sh`.

## License

[MIT](LICENSE)
