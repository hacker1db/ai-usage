VERSION := $(shell git describe --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION)

.PHONY: build test check run tui report install install-proxy

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/ai-usage ./cmd/ai-usage
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/foundry-entra-proxy ./cmd/foundry-entra-proxy

test:
	go test -race ./...

# What CI should run: formatting, vet, race tests, the JS syntax check, and the stdlib-only rule.
check:
	@test -z "$$(gofmt -l .)" || (gofmt -l .; exit 1)
	go vet ./...
	go test -race ./...
	@! command -v node >/dev/null || node --check internal/web/static/app.js
	@test -z "$$(go list -deps -f '{{if not .Standard}}{{.ImportPath}}{{end}}' ./... | grep -v '^ai-usage')" || (echo "non-stdlib dependency found"; exit 1)

run: build
	./bin/ai-usage serve

tui: build
	./bin/ai-usage tui

report: build
	./bin/ai-usage report -print

install:
	scripts/install-dashboard.sh

install-proxy:
	scripts/install-proxy.sh
