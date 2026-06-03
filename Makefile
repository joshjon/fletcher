GO     ?= go
BIN    := bin/fletcher
PKG    := github.com/joshjon/fletcher

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -s -w \
  -X $(PKG)/internal/buildinfo.Version=$(VERSION) \
  -X $(PKG)/internal/buildinfo.Commit=$(COMMIT) \
  -X $(PKG)/internal/buildinfo.Date=$(DATE)

BUILD_FLAGS := -trimpath -ldflags "$(LDFLAGS)"

.PHONY: help build build-linux build-linux-amd64 build-linux-arm64 \
	test test-integration lint fmt check cover generate generate-check \
	tools clean

help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-22s\033[0m %s\n", $$1, $$2}'

## --- Build ---

build: ## Build the local fletcher binary
	CGO_ENABLED=0 $(GO) build $(BUILD_FLAGS) -o $(BIN) ./cmd/fletcher

build-linux: build-linux-amd64 build-linux-arm64 ## Cross-compile both Linux targets

build-linux-amd64: ## Cross-compile linux/amd64
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 $(GO) build $(BUILD_FLAGS) -o bin/fletcher-linux-amd64 ./cmd/fletcher

build-linux-arm64: ## Cross-compile linux/arm64
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 $(GO) build $(BUILD_FLAGS) -o bin/fletcher-linux-arm64 ./cmd/fletcher

## --- Test ---

test: ## Run unit tests with the race detector
	$(GO) test -race -count=1 ./...

test-integration: ## Run integration tests (build tag: integration)
	$(GO) test -race -count=1 -tags=integration ./...

cover: ## Run tests with coverage; emit HTML report
	$(GO) test -race -count=1 -coverprofile=coverage.out ./...
	$(GO) tool cover -html=coverage.out -o coverage.html
	@echo "coverage HTML: coverage.html"

## --- Quality gates ---

lint: ## Run golangci-lint
	$(GO) tool golangci-lint run

fmt: ## Auto-format source via gofumpt + goimports
	$(GO) tool golangci-lint fmt

check: lint test generate-check ## Full local gate: lint + tests + generated drift

## --- Codegen ---

generate: ## Run all code generators (sqlc, buf, mockery) — added per-phase
	@echo "no generators registered yet"

generate-check: ## Fail if 'make generate' would modify the working tree
	@before=$$(git status --porcelain); \
	$(MAKE) --no-print-directory generate >/dev/null; \
	after=$$(git status --porcelain); \
	if [ "$$before" != "$$after" ]; then \
		echo "ERROR: 'make generate' modified the working tree. Run 'make generate' and commit."; \
		git status --short; \
		exit 1; \
	fi

## --- Tooling ---

tools: ## Print pinned tool list (resolved via 'go tool')
	@$(GO) tool

clean: ## Remove build & coverage artifacts
	rm -rf bin/ dist/ coverage.out coverage.html
