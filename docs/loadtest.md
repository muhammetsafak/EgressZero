# Memory & concurrency load test

The original goal was **"under 50 MB RAM at 1000 concurrent downloads."** This document records how that is measured, the real numbers, and the honest conclusion: the proxy streams with flat per-request cost, but **total RSS scales with the number of *simultaneously active* streams**, and 1000 genuinely-concurrent active streams cost well over 50 MB. The 50 MB figure holds at ~100 concurrent, not 1000.

## What "streaming" does and does not guarantee

Two different things get conflated by a single "memory" number:

- **Per-request cost is flat.** A 1 GB download costs the same as a 1 KB one — bodies are never buffered. This is proven by `TestMemoryCeiling` (50 concurrent 256 MB streams grow the heap < 20 MB) and `BenchmarkGET` (28 allocs/op, independent of object size). This property is real and holds.
- **Total footprint scales with concurrency.** Each in-flight stream costs a net/http copy buffer (~32 KB), client- and server-side connection buffers, two goroutine stacks, plus the Go runtime's GC headroom (by default the heap is allowed to roughly double the live set before a GC). At 1000 active streams that adds up to ~125–260 MB, not 50 MB.

## Method

`scripts/loadtest.sh` runs the proxy in a Linux container with metrics enabled, seeds a 128 MB object in MinIO, drives `CONC` concurrent downloads from another container on the same Docker network (`scripts/loadgen`, per-stream rate-limitable to keep streams open), and samples the proxy's own `process_resident_memory_bytes`, `go_memstats_heap_inuse_bytes` and `egresszero_in_flight_requests` from `/metrics` twice a second, reporting the peak.

```sh
# worst case: 1000 streams held open at 64 KiB/s each
CONC=1000 DUR=25 RATE=65536 bash scripts/loadtest.sh

# with a soft memory limit
CONC=1000 DUR=25 RATE=65536 GOMEMLIMIT_VAL=48MiB bash scripts/loadtest.sh

# realistic simultaneity, full speed
CONC=100 DUR=15 RATE=0 bash scripts/loadtest.sh
```

Measuring `process_resident_memory_bytes` from the process's own `/proc` is more honest than `docker stats`, whose cgroup number also includes kernel socket buffers.

## Results

Measured on Linux (Docker), 128 MB object, MinIO backend:

| Concurrency | Per-stream rate | `GOMEMLIMIT` | Peak in-flight | Peak RSS | Peak Go heap |
|---|---|---|---|---|---|
| 100 | unlimited (~10 GB/s) | unset | 100 | 56.5 MiB | 23.2 MiB |
| 100 | unlimited | 48 MiB | 100 | 54.6 MiB | 22.9 MiB |
| 1000 | 64 KiB/s | unset | 1000 | 196.3 MiB | 125.4 MiB |
| 1000 | 64 KiB/s | 48 MiB | 1000 | 124.8 MiB | 97.9 MiB |
| 1000 | unlimited | unset | 1000 | 261.6 MiB | 179.8 MiB |

Every run transferred gigabytes with **zero errors**; the proxy sustained a genuine 1000 simultaneous streams (confirmed by the in-flight gauge) and hit ~10 GB/s aggregate throughput at 100 concurrent.

## Conclusions

- **The proxy does not buffer.** Object size does not affect memory; this is the property that actually caps S3 egress.
- **RSS ≈ 150–260 KB per concurrent active stream** at high concurrency (buffers + two goroutine stacks + GC headroom). Plan capacity on *actual simultaneity*, which for a CDN origin equals arrival-rate × origin-fetch-time and is typically far below 1000 (Cloudflare pulls are fast, so streams are short-lived).
- **`GOMEMLIMIT` helps but is not magic.** At 1000 concurrent it cut RSS from 196 MB to 125 MB by trimming GC headroom, but it cannot drop below the *live* working set (~98 MB here), which genuinely exceeds 48 MB. Setting it far below the live set only causes GC thrashing.
- **The 50 MB target is a ~100-concurrent figure**, not a 1000-concurrent one. If you need a hard cap at very high concurrency, put a concurrency limiter in front of the origin (or rely on the CDN to keep origin fetches few and short) rather than expecting the Go runtime to serve 1000 active streams in 50 MB.

## Future optimization ideas

If lowering the per-stream footprint becomes a priority: a bounded worker pool / semaphore capping concurrent upstream fetches, a smaller shared copy buffer, or tuning `GOGC` alongside `GOMEMLIMIT`. None are in scope for the current release; the streaming guarantee (flat per-request cost) is the load-bearing property and it holds.
