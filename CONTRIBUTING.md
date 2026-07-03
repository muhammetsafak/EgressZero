# Contributing

Thanks for your interest! Issues and pull requests are welcome.

## Development setup

Requirements: Go 1.24+ and (for end-to-end testing) Docker.

```sh
make test        # unit tests
make test-race   # with the race detector
make bench       # streaming benchmark (allocs/op must stay flat)
make lint        # go vet (+ staticcheck if installed)
make compose-up  # MinIO + proxy end-to-end stack on :8080
```

## Pull requests

- Keep the dependency footprint as is: standard library + `aws-sdk-go-v2` only.
- Run `gofmt`, `make lint` and `make test-race` before pushing — CI enforces all three.
- Behavior changes need a test in the handler matrix (`internal/proxy/proxy_test.go`).
- Anything that touches the streaming path must keep `BenchmarkGET` allocations flat and `TestMemoryCeiling` green.
- Design intent (streaming, header allowlist, error opacity, WriteTimeout=0) is documented in the README — please align with it or open an issue to discuss first.
