BINARY := egresszero
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo devel)
LDFLAGS := -s -w -X github.com/muhammetsafak/egresszero/internal/version.version=$(VERSION)

.PHONY: build test test-race bench lint docker compose-up tidy

build:
	CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" -o $(BINARY) ./cmd/egresszero

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
	docker build --build-arg VERSION=$(VERSION) -t egresszero:latest .

compose-up:
	docker compose up --build

tidy:
	go mod tidy
