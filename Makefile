BINARY := egresszero

.PHONY: build test test-race bench lint docker compose-up tidy

build:
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o $(BINARY) ./cmd/egresszero

test:
	go test ./...

test-race:
	go test -race ./...

bench:
	go test -bench=. -benchmem -run='^$$' ./internal/proxy/

lint:
	go vet ./...
	@if command -v staticcheck >/dev/null 2>&1; then staticcheck ./...; else echo "staticcheck not installed, skipping"; fi

docker:
	docker build -t egresszero:latest .

compose-up:
	docker compose up --build

tidy:
	go mod tidy
