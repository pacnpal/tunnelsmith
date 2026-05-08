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

.PHONY: all build test lint docker clean tidy

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

clean:
	rm -rf $(BIN_DIR)
	$(GO) clean -testcache

tidy:
	$(GO) mod tidy
