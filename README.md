# EgressZero

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
- **Conditional requests** — `If-None-Match` / `If-Modified-Since` forwarded, clean `304 Not Modified` responses for cheap revalidation
- **Private buckets** — SigV4 signing via the official `aws-sdk-go-v2` credential chain (env vars, shared config, IMDS/IRSA)
- **S3-compatible endpoints** — Cloudflare R2, MinIO, Backblaze B2 via `S3_ENDPOINT`
- **Cache-Control injection** — override the upstream `Cache-Control` with `CACHE_CONTROL`
- **CDN-only protection** — optional shared-secret header check so nobody bypasses the CDN and re-bills your egress
- **No leaks** — S3 error details, request IDs and bucket names never reach clients; error responses are `no-store`
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
| `CACHE_CONTROL` | no | pass upstream through | Replaces `Cache-Control` on 200/206/304 responses |
| `PROXY_AUTH_SECRET` | no | disabled | Enables CDN-only protection (see below) |
| `PROXY_AUTH_HEADER` | no | `X-Proxy-Auth` | Header carrying the secret |
| `LOG_LEVEL` | no | `info` | `debug`, `info`, `warn`, `error` |
| `LOG_REQUESTS` | no | `false` | One structured JSON log line per request |
| `SHUTDOWN_TIMEOUT` | no | `15s` | Graceful-shutdown drain window |

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

- **Status mapping**: missing key → `404`, access denied → `403`, unsatisfiable range → `416`, precondition failed → `412`, upstream failure → `502`, upstream timeout → `504`, non-GET/HEAD → `405`. Error bodies are generic fixed strings with `Cache-Control: no-store`.
- **Keys**: the URL path is percent-decoded and used as the object key (leading `/` stripped, `S3_KEY_PREFIX` prepended). Paths containing `.` / `..` segments are rejected with `400`; embedded `//` in keys is preserved.
- **Headers forwarded**: `Content-Type`, `Content-Length`, `ETag`, `Last-Modified`, `Cache-Control`, `Content-Encoding`, `Content-Disposition`, `Content-Language`, `Expires`, `Content-Range`, plus `Accept-Ranges: bytes`. Nothing else (no `x-amz-*`, SSE or version headers).
- **Memory**: for a strict RSS ceiling in production set [`GOMEMLIMIT`](https://pkg.go.dev/runtime/debug#SetMemoryLimit) (e.g. `GOMEMLIMIT=40MiB`); the Go runtime then keeps heap churn under that bound at slightly higher GC cost.

## Limitations (by design, documented)

- `If-Range` is dropped (the S3 API has no equivalent); clients fall back to a full `200`. Cloudflare's internal chunking sends bare `Range` headers, so edge caching is unaffected.
- Multi-range requests (`bytes=0-1,5-9`) are passed to S3, which RFC-correctly answers with the full `200` body.
- `Range` is not forwarded on `HEAD` requests.
- Query strings are ignored entirely.

## Development

```sh
make test        # unit tests (full handler matrix)
make test-race   # with the race detector
make bench       # streaming benchmark (-benchmem; allocs/op must stay flat)
make lint        # go vet (+ staticcheck if installed)
make compose-up  # MinIO + proxy end-to-end stack
```

`go test ./internal/proxy/ -run TestMemoryCeiling` proves the streaming memory ceiling: 50 concurrent 256 MB downloads held mid-flight may not grow the heap by more than 20 MB.

## Future work

- Prometheus metrics (deliberately left out of the MVP)
- Request coalescing (`singleflight`) for cold-cache stampedes
- Rolling per-write deadlines via `http.NewResponseController` as an alternative to `WriteTimeout: 0`
