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

# Bundled Firecracker VMM + guest kernel (gitignored; embedded at build time).
FC_VERSION   ?= v1.16.0
FC_KERNEL    ?= vmlinux-5.10.225
FC_KERNEL_CI ?= v1.11
VMM_ASSETS   := internal/runtime/firecrackerdriver/vmm/assets

# Guest agent (the microVM init): built from cmd/fletcher-guest for the target
# arch and embedded. build-guest($arch) builds it into the embed tree.
GUEST_ASSETS := internal/runtime/firecrackerdriver/guestagent/assets
HOST_GOARCH  := $(shell $(GO) env GOARCH)
HOST_OS      := $(shell uname -s)
define build-guest
	@mkdir -p $(GUEST_ASSETS)/$(1)
	CGO_ENABLED=0 GOOS=linux GOARCH=$(1) $(GO) build -trimpath -ldflags "-s -w" \
		-o $(GUEST_ASSETS)/$(1)/fletcher-guest ./cmd/fletcher-guest
endef

.PHONY: help build build-guest build-guest-all build-linux build-linux-amd64 \
	build-linux-arm64 build-darwin build-darwin-amd64 build-darwin-arm64 \
	cross-check test test-integration lint fmt check cover generate \
	generate-check tools clean image image-amd64 image-arm64 install fetch-vmm

help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-22s\033[0m %s\n", $$1, $$2}'

## --- Build ---

build: build-guest ## Build the local fletcher binary
	CGO_ENABLED=0 $(GO) build $(BUILD_FLAGS) -o $(BIN) ./cmd/fletcher

build-guest: ## Build the microVM guest init for the host arch into the embed tree
	$(call build-guest,$(HOST_GOARCH))

build-guest-all: ## Build the guest init for every release arch (used by goreleaser)
	$(call build-guest,amd64)
	$(call build-guest,arm64)

fetch-vmm: ## Download the bundled Firecracker VMM + guest kernel (needed before building the firecracker runtime)
	@scripts/fetch-vmm.sh "$(FC_VERSION)" "$(FC_KERNEL)" "$(FC_KERNEL_CI)" "$(VMM_ASSETS)"

PREFIX ?= /usr/local

ifeq ($(HOST_OS),Linux)
install: build ## Developer convenience - mirrors scripts/install.sh using local files. End users use scripts/install.sh instead.
	@if ! id -u fletcher >/dev/null 2>&1; then \
		echo "==> creating fletcher system user"; \
		sudo useradd --system --home-dir /var/lib/fletcher --shell /usr/sbin/nologin fletcher; \
	fi
	sudo install -d -m 0700 -o fletcher -g fletcher /var/lib/fletcher /etc/fletcher
	@if getent group kvm >/dev/null 2>&1 && ! id -nG fletcher 2>/dev/null | grep -qw kvm; then \
		echo "==> adding fletcher to the kvm group (needed for the Firecracker runtime)"; \
		sudo usermod -aG kvm fletcher; \
	fi
	sudo install $(BIN) $(PREFIX)/bin/fletcher
	sudo install -m 0644 init/fletcher.service /etc/systemd/system/
	sudo systemctl daemon-reload
	@if [ -n "$$USER" ] && [ "$$USER" != "root" ] && ! id -nG "$$USER" 2>/dev/null | grep -qw fletcher; then \
		echo "==> adding $$USER to the fletcher group (needed to talk to the daemon socket)"; \
		sudo usermod -aG fletcher "$$USER"; \
		echo "==> the fletcher CLI activates this group for you automatically; if a command still reports no socket access, log out and back in (or run 'newgrp fletcher')"; \
	fi
	@if systemctl is-active --quiet fletcher; then \
		echo "==> fletcher is running; restarting"; \
		sudo systemctl restart fletcher; \
	else \
		echo "==> installed. start with: fletcher daemon enable"; \
	fi
else
install: build ## Developer convenience - install the client to $(PREFIX)/bin (macOS: client only, no daemon)
	sudo install -d $(PREFIX)/bin
	sudo install $(BIN) $(PREFIX)/bin/fletcher
	@echo "==> installed the fletcher client to $(PREFIX)/bin (on the default PATH). Connect it: fletcher login <token>"
endif

build-linux: build-linux-amd64 build-linux-arm64 ## Cross-compile both Linux targets

build-linux-amd64: ## Cross-compile linux/amd64
	$(call build-guest,amd64)
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 $(GO) build $(BUILD_FLAGS) -o bin/fletcher-linux-amd64 ./cmd/fletcher

build-linux-arm64: ## Cross-compile linux/arm64
	$(call build-guest,arm64)
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 $(GO) build $(BUILD_FLAGS) -o bin/fletcher-linux-arm64 ./cmd/fletcher

build-darwin: build-darwin-amd64 build-darwin-arm64 ## Cross-compile both macOS client targets (slim: no bundled VMM)

build-darwin-amd64: ## Cross-compile darwin/amd64 (client)
	GOOS=darwin GOARCH=amd64 CGO_ENABLED=0 $(GO) build $(BUILD_FLAGS) -o bin/fletcher-darwin-amd64 ./cmd/fletcher

build-darwin-arm64: ## Cross-compile darwin/arm64 (client)
	GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 $(GO) build $(BUILD_FLAGS) -o bin/fletcher-darwin-arm64 ./cmd/fletcher

cross-check: ## Verify the CLI still cross-compiles to macOS, catching non-linux stub drift
	GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 $(GO) build -o /dev/null ./cmd/fletcher
	GOOS=darwin GOARCH=amd64 CGO_ENABLED=0 $(GO) build -o /dev/null ./cmd/fletcher

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

lint: ## Run golangci-lint and buf lint
	$(GO) tool golangci-lint run
	$(GO) tool buf lint

fmt: ## Auto-format source via gofumpt + goimports
	$(GO) tool golangci-lint fmt

check: lint test generate-check cross-check ## Full local gate: lint + tests + generated drift + macOS cross-build

## --- Codegen ---

SQLITE_DIR     := internal/sqlite
MIGRATIONS_DIR := $(SQLITE_DIR)/migrations
SCHEMA_FILE    := $(SQLITE_DIR)/schema.sql

generate: ## Run all code generators (buf, schema mirror, sqlc, mockery)
	$(GO) tool buf generate
	@printf '%s\n' '-- AUTO-GENERATED by `make generate` from $(MIGRATIONS_DIR)/*.up.sql. DO NOT EDIT.' > $(SCHEMA_FILE)
	@for f in $(MIGRATIONS_DIR)/*.up.sql; do \
		printf '\n-- File: %s\n' "$$(basename $$f)" >> $(SCHEMA_FILE); \
		cat $$f >> $(SCHEMA_FILE); \
	done
	cd $(SQLITE_DIR) && $(GO) tool sqlc generate

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

## --- Release ---

release-snapshot: ## Build a goreleaser snapshot (no publish, no tag)
	goreleaser release --clean --snapshot --skip=publish

release-check: ## Validate .goreleaser.yaml without building
	goreleaser check

clean: ## Remove build & coverage artifacts
	rm -rf bin/ dist/ coverage.out coverage.html

## --- Images ---

IMAGE_NAME ?= fletcher-base
IMAGE_TAG  ?= dev
IMAGE_DIR  := images/fletcher-base

image: ## Build the fletcher-base OCI image for the host architecture
	docker build -t $(IMAGE_NAME):$(IMAGE_TAG) $(IMAGE_DIR)

image-amd64: ## Build fletcher-base for linux/amd64 (cross-platform)
	docker buildx build --platform linux/amd64 -t $(IMAGE_NAME):$(IMAGE_TAG)-amd64 --load $(IMAGE_DIR)

image-arm64: ## Build fletcher-base for linux/arm64 (cross-platform)
	docker buildx build --platform linux/arm64 -t $(IMAGE_NAME):$(IMAGE_TAG)-arm64 --load $(IMAGE_DIR)
