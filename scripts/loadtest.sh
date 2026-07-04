#!/usr/bin/env bash
# Reproducible memory load test for EgressZero (see docs/loadtest.md).
#
# Brings up MinIO, seeds a large object, runs the proxy in a Linux
# container with metrics enabled, drives CONC concurrent rate-limited
# downloads from another container on the same network, and samples the
# proxy's real RSS (process_resident_memory_bytes), heap and in-flight
# gauge from /metrics. Prints the peak values, then tears everything
# down.
#
# Tunables (env vars): CONC, DUR, RATE (bytes/s per stream), OBJSIZE,
# GOMEMLIMIT_VAL.
set -euo pipefail
cd "$(dirname "$0")/.."

CONC=${CONC:-1000}
DUR=${DUR:-25}                       # seconds
RATE=${RATE:-65536}                  # 64 KiB/s per stream
OBJSIZE=${OBJSIZE:-134217728}        # 128 MiB
GOMEMLIMIT_VAL=${GOMEMLIMIT_VAL:-}   # e.g. 48MiB; empty = unset
NET=egresszero_default
METRICS_URL=http://localhost:19090/metrics

cleanup() {
  docker rm -f ez-load ez-loadgen >/dev/null 2>&1 || true
  docker compose down -v >/dev/null 2>&1 || true
  rm -f dist/loadgen
}
trap cleanup EXIT

echo ">> bringing up MinIO"
docker compose up -d minio minio-seed >/dev/null

echo ">> seeding ${OBJSIZE}-byte object"
docker run --rm --network "$NET" --entrypoint /bin/sh minio/mc -ec "
  for i in \$(seq 1 30); do mc alias set local http://minio:9000 minioadmin minioadmin && break; sleep 1; done
  head -c ${OBJSIZE} /dev/urandom | mc pipe local/demo/large.bin
"

echo ">> building proxy image"
docker build -q -t egresszero:loadtest . >/dev/null

echo ">> starting proxy (GOMEMLIMIT='${GOMEMLIMIT_VAL:-unset}')"
docker run -d --name ez-load --network "$NET" \
  -e S3_BUCKET=demo -e S3_ENDPOINT=http://minio:9000 -e S3_FORCE_PATH_STYLE=true \
  -e AWS_REGION=us-east-1 -e AWS_ACCESS_KEY_ID=minioadmin -e AWS_SECRET_ACCESS_KEY=minioadmin \
  -e METRICS_ADDR=:9090 ${GOMEMLIMIT_VAL:+-e GOMEMLIMIT=$GOMEMLIMIT_VAL} \
  -p 19090:9090 egresszero:loadtest >/dev/null
sleep 2

echo ">> building loadgen (linux/amd64 static)"
mkdir -p dist
docker run --rm -v "$PWD":/src -w /src -e CGO_ENABLED=0 -e GOOS=linux golang:1.26-alpine \
  go build -o dist/loadgen ./scripts/loadgen >/dev/null

echo ">> driving ${CONC} concurrent streams for ${DUR}s (rate ${RATE} B/s each)"
docker run -d --name ez-loadgen --network "$NET" -v "$PWD/dist/loadgen:/loadgen:ro" alpine:latest \
  /loadgen -url http://ez-load:8080/large.bin -c "$CONC" -d "${DUR}s" -rate "$RATE" >/dev/null

scrape() { curl -s "$METRICS_URL" | awk -v k="$1" '$1==k {print $2}'; }

peak_rss=0; peak_heap=0; peak_inflight=0
end=$(( $(date +%s) + DUR ))
while [ "$(date +%s)" -lt "$end" ]; do
  rss=$(scrape process_resident_memory_bytes || true)
  heap=$(scrape go_memstats_heap_inuse_bytes || true)
  inflight=$(scrape egresszero_in_flight_requests || true)
  [ -n "${rss:-}" ] && awk "BEGIN{exit !($rss>$peak_rss)}" && peak_rss=$rss
  [ -n "${heap:-}" ] && awk "BEGIN{exit !($heap>$peak_heap)}" && peak_heap=$heap
  [ -n "${inflight:-}" ] && awk "BEGIN{exit !($inflight>$peak_inflight)}" && peak_inflight=$inflight
  sleep 0.5
done

echo ">> loadgen summary:"; docker logs ez-loadgen 2>&1 | tail -1

mib() { awk "BEGIN{printf \"%.1f\", $1/1048576}"; }
echo
echo "================ RESULTS ================"
echo "concurrency target : ${CONC}"
echo "peak in-flight     : ${peak_inflight%.*}"
echo "peak process RSS   : $(mib "${peak_rss:-0}") MiB"
echo "peak Go heap inuse : $(mib "${peak_heap:-0}") MiB"
echo "GOMEMLIMIT         : ${GOMEMLIMIT_VAL:-unset}"
echo "========================================"
