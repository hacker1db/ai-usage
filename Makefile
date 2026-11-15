VERSION := $(shell git describe --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION)

.PHONY: build test check lint typecheck-js run tui report install-proxy

GOLANGCI_CONFIG := .github/linters/.golangci.yml
TYPESCRIPT_VERSION := 7.0.2

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/ai-usage ./cmd/ai-usage
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/foundry-entra-proxy ./cmd/foundry-entra-proxy

test:
	go test -race ./...

# What CI should run: formatting, vet, race tests, the JS syntax check, the stdlib-only rule, and
# govulncheck when it is installed (go install golang.org/x/vuln/cmd/govulncheck@latest).
check:
	@test -z "$$(gofmt -l .)" || (gofmt -l .; exit 1)
	go vet ./...
	go test -race ./...
	@! command -v node >/dev/null || node --check internal/web/static/app.js
	@test -z "$$(go list -deps -f '{{if not .Standard}}{{.ImportPath}}{{end}}' ./... | grep -v '^ai-usage')" || (echo "non-stdlib dependency found"; exit 1)
	@if command -v govulncheck >/dev/null; then govulncheck ./...; else echo "govulncheck not installed; skipping"; fi

# Static analysis: golangci-lint (the config CI's super-linter uses) and the app.js type check.
lint: typecheck-js
	golangci-lint run --config $(GOLANGCI_CONFIG) ./...
	golangci-lint fmt --diff --config $(GOLANGCI_CONFIG) ./...

typecheck-js:
	npx -y -p typescript@$(TYPESCRIPT_VERSION) tsc --noEmit --allowJs --checkJs --strict \
		--target es2022 --lib es2022,dom,dom.iterable internal/web/static/app.js

run: build
	./bin/ai-usage serve

tui: build
	./bin/ai-usage tui

report: build
	./bin/ai-usage report -print

install-proxy:
	scripts/install-proxy.sh
