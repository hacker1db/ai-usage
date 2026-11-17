VERSION := $(shell git describe --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION)

.PHONY: build test test-integration coverage check lint typecheck-js run tui report install-proxy

GOLANGCI_CONFIG := .github/linters/.golangci.yml
TYPESCRIPT_VERSION := 7.0.2
# The gateway parses untrusted input and handles credentials, so its unit tests have a floor.
GATEWAY_COVERAGE_MIN := 85

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/ai-usage ./cmd/ai-usage
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/foundry-entra-proxy ./cmd/foundry-entra-proxy
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/ai-gateway ./cmd/ai-gateway

test:
	go test -race ./...

# End to end: builds ai-gateway and ai-usage, runs them against fake upstreams on loopback, and,
# when test/sdk has node_modules (npm ci --prefix test/sdk), drives the gateway with the official
# OpenAI and Anthropic SDKs. The verbose log is kept as test-integration.log.
test-integration:
	AIGW_IT_ARTIFACTS=$(CURDIR)/test-artifacts go test -tags integration -race -count=1 -v ./test/integration/ > test-integration.log 2>&1; \
		status=$$?; cat test-integration.log; exit $$status

# Unit-test coverage report (coverage.html) and the gateway floor.
coverage:
	go test -race -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out -o coverage.html
	@go tool cover -func=coverage.out | tail -1
	@go run ./scripts/covfloor -profile coverage.out -pkg ai-usage/internal/gateway -min $(GATEWAY_COVERAGE_MIN)

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
