BINARY      := tunnelsmith
PKG         := ./cmd/tunnelsmith
BIN_DIR     := bin
IMAGE       := tunnelsmith
IMAGE_TAG   := dev

VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT      ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE        ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS     := -s -w \
               -X main.version=$(VERSION) \
               -X main.commit=$(COMMIT) \
               -X main.date=$(DATE)

GO          ?= go
GOLANGCI    ?= golangci-lint

.PHONY: all build test lint docker test-integration clean tidy

all: build

build:
	mkdir -p $(BIN_DIR)
	$(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/$(BINARY) $(PKG)

test:
	$(GO) test ./... -count=1

lint:
	$(GOLANGCI) run ./...

# Docker image build is intentionally CI-only. See ADR-001.
# This target is invoked by the GitHub Actions workflow, not from a
# developer host. Keeping it here so CI has a single command to call.
docker:
	docker build \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		--build-arg DATE=$(DATE) \
		-t $(IMAGE):$(IMAGE_TAG) .

# Mullvad integration suite. Brings up deploy/docker-compose.mullvad.yml
# (gluetun in WireGuard mode + tunnelsmith) and runs scripts/verify-mullvad.sh
# against it. Per ADR-001 this target runs in CI, not on developer hosts;
# the integration job in .github/workflows/ci.yml gates on both
# MULLVAD_WIREGUARD_PRIVATE_KEY and MULLVAD_WIREGUARD_ADDRESSES being set
# as repo secrets. Locally, this target is the same skip-ok contract:
# without the secrets it logs a reason and exits 0.
test-integration:
	@if [ -z "$$MULLVAD_WIREGUARD_PRIVATE_KEY" ] || [ -z "$$MULLVAD_WIREGUARD_ADDRESSES" ]; then \
		echo "[skip-ok] make test-integration: MULLVAD_WIREGUARD_PRIVATE_KEY and MULLVAD_WIREGUARD_ADDRESSES not set; integration suite needs both per ADR-003"; \
		exit 0; \
	fi; \
	set -e; \
	trap 'docker compose -f deploy/docker-compose.mullvad.yml down --volumes --remove-orphans' EXIT; \
	docker compose -f deploy/docker-compose.mullvad.yml up -d --quiet-pull; \
	scripts/verify-mullvad.sh

clean:
	rm -rf $(BIN_DIR)
	$(GO) clean -testcache

tidy:
	$(GO) mod tidy
