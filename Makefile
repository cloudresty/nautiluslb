BASE = cloudresty
NAME = $$(awk -F'/' '{print $$(NF-0)}' <<< $$PWD)
DOCKER_REPO = ${BASE}/${NAME}
DOCKER_TAG = test
GOVULNCHECK_VERSION = v1.8.0
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
VERSION_PKG = github.com/cloudresty/nautiluslb/internal/version
LDFLAGS = -X $(VERSION_PKG).Version=$(VERSION) -X $(VERSION_PKG).Commit=$(COMMIT) -X $(VERSION_PKG).BuildDate=$(BUILD_DATE)

.PHONY: help build build-local run shell clean test lint vuln e2e docker-build

help: ## Show list of make targets and their description.
	@grep -E '^[%a-zA-Z0-9_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-15s\033[0m %s\n", $$1, $$2}'

build-local: ## Build the binary locally.
	@echo "Building NautilusLB locally..."
	@cd app && go build -ldflags "$(LDFLAGS)" -o ../nautiluslb ./cmd/nautiluslb

build: ## Build a docker image locally.
	@echo "Building Docker image..."
	@docker build \
		--platform linux/amd64 \
		--pull \
		--force-rm \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		--build-arg BUILD_DATE=$(BUILD_DATE) \
		--tag ${DOCKER_REPO}:${DOCKER_TAG} \
		--file build/Dockerfile .

docker-build: build ## Alias for build target.

test: ## Run all unit tests with the race detector.
	@cd app && go test -race ./...

lint: ## Run golangci-lint (v2) on the Go module.
	@cd app && golangci-lint run

vuln: ## Scan the Go module for known vulnerabilities.
	@cd app && go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...

e2e: ## Run the kind-based end-to-end test (needs docker, kind, kubectl). KEEP=1 keeps the cluster.
	@./test/e2e/run.sh

run: ## Run docker image locally.
	@docker run \
		--platform linux/amd64 \
		--rm \
		--name ${NAME} \
		--hostname ${NAME}-${DOCKER_TAG} \
		${DOCKER_REPO}:${DOCKER_TAG}

shell: ## Open a shell in the builder stage (the runtime image is distroless and has no shell).
	@docker build \
		--target builder \
		--tag ${DOCKER_REPO}:${DOCKER_TAG}-builder \
		--file build/Dockerfile .
	@docker run \
		--rm \
		--name ${NAME}-builder \
		--hostname ${NAME}-${DOCKER_TAG} \
		--interactive \
		--tty \
		--volume $$(pwd)/app/config.yaml:/src/config.yaml:ro \
		--entrypoint /bin/bash \
		${DOCKER_REPO}:${DOCKER_TAG}-builder

clean: ## Remove all local docker images for the application.
	@if [[ $$(docker images --format '{{.Repository}}:{{.Tag}}' | grep ${DOCKER_REPO}) ]]; then docker rmi $$(docker images --format '{{.Repository}}:{{.Tag}}' | grep ${DOCKER_REPO}); else echo "INFO: No images found for '${DOCKER_REPO}'"; fi