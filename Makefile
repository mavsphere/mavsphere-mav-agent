APP_NAME    ?= mavsphere-agent
DOCKER_REPO ?= ghcr.io/mavsphere/agent

# Auto timestamp (UTC) and git short SHA if available
TIMESTAMP := $(shell date -u +%Y-%m-%d_%H%M%S)
GIT_SHA   := $(shell git rev-parse --short HEAD 2>/dev/null || echo nogit)

# Default suffix can be overridden:
SUFFIX ?= -$(TIMESTAMP)

.PHONY: build-suffixed build-ts build-release build-debug \
        build-linux-amd64 build-linux-arm64 \
        docker-push docker-build docker-login clean help

# ── Go builds ────────────────────────────────────────────────

# Build with a suffix so you don't overwrite previous binaries.
build-suffixed:
	@mkdir -p bin
	go build -o bin/$(APP_NAME)$(SUFFIX) ./cmd/mavagent
	@echo "Built bin/$(APP_NAME)$(SUFFIX)"

# Build with an auto timestamp, e.g. bin/mavsphere-agent-2025-09-18_161530
build-ts:
	@$(MAKE) build-suffixed SUFFIX=-$(TIMESTAMP)

# Release build with trimmed symbols (smaller binary)
build-release:
	@mkdir -p bin
	go build -ldflags "-s -w" -o bin/$(APP_NAME) ./cmd/mavagent
	@echo "Built release: bin/$(APP_NAME) (ldflags -s -w)"

# Debug build with GC/inline disabled (more debug-friendly)
build-debug:
	@mkdir -p bin
	go build -gcflags "all=-N -l" -o bin/$(APP_NAME)-debug ./cmd/mavagent
	@echo "Built debug: bin/$(APP_NAME)-debug (gcflags -N -l)"

# Cross-compile for linux/amd64
build-linux-amd64:
	@mkdir -p bin
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "-s -w" -o bin/$(APP_NAME)-linux-amd64 ./cmd/mavagent

# Cross-compile for linux/arm64
build-linux-arm64:
	@mkdir -p bin
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags "-s -w" -o bin/$(APP_NAME)-linux-arm64 ./cmd/mavagent

# ── Docker ───────────────────────────────────────────────────

# Build and push multi-arch image to GHCR (linux/amd64 + linux/arm64)
# Usage:
#   make docker-push                          # tags with git SHA + latest
#   make docker-push DOCKER_TAG=v1.2.0        # tags with v1.2.0 + latest
docker-push:
	docker buildx build \
		--platform linux/amd64,linux/arm64 \
		--tag $(DOCKER_REPO):$(or $(DOCKER_TAG),$(GIT_SHA)) \
		--tag $(DOCKER_REPO):latest \
		--push .
	@echo "Pushed $(DOCKER_REPO):$(or $(DOCKER_TAG),$(GIT_SHA)) and $(DOCKER_REPO):latest"

# Build single-arch locally without pushing (for testing)
# Note: --load only supports a single platform
docker-build:
	docker buildx build \
		--platform linux/arm64 \
		--tag $(DOCKER_REPO):$(or $(DOCKER_TAG),$(GIT_SHA)) \
		--tag $(DOCKER_REPO):latest \
		--load .
	@echo "Built $(DOCKER_REPO):$(or $(DOCKER_TAG),$(GIT_SHA)) (local only)"

# ── Auth ─────────────────────────────────────────────────────

docker-login:
	@echo "Logging in to GHCR..."
	@echo "$${GITHUB_TOKEN}" | docker login ghcr.io -u "$${GITHUB_ACTOR}" --password-stdin

# ── Housekeeping ─────────────────────────────────────────────

clean:
	rm -rf bin/

help:
	@echo ""
	@echo "  make build-release          Local Go build with -ldflags -s -w"
	@echo "  make build-linux-amd64      Cross-compile linux/amd64"
	@echo "  make build-linux-arm64      Cross-compile linux/arm64 (Raspberry Pi)"
	@echo ""
	@echo "  make docker-push            Multi-arch build + push to GHCR"
	@echo "  make docker-build           Single-arch local build (no push)"
	@echo "  make docker-login           Log in to GHCR (needs GITHUB_TOKEN + GITHUB_ACTOR)"
	@echo ""
	@echo "  DOCKER_REPO=$(DOCKER_REPO)"
	@echo "  DOCKER_TAG  (override: make docker-push DOCKER_TAG=v1.2.0)"
	@echo ""
