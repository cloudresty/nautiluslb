BASE = cloudresty
NAME = $$(awk -F'/' '{print $$(NF-0)}' <<< $$PWD)
DOCKER_REPO = ${BASE}/${NAME}
DOCKER_TAG = test

# Version information
GIT_TAG := $(shell git describe --tags --exact-match 2>/dev/null || echo "")
GIT_COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_TIME := $(shell date -u '+%Y-%m-%d_%H:%M:%S_UTC')
VERSION := $(if $(GIT_TAG),$(GIT_TAG),$(shell git rev-parse --abbrev-ref HEAD 2>/dev/null || echo "unknown")-$(GIT_COMMIT))

.PHONY: build build-local run shell clean test docker-build

help: ## Show list of make targets and their description.
	@grep -E '^[%a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-15s\033[0m %s\n", $$1, $$2}'

build-local: ## Build the binary locally with version information.
	@echo "Building NautilusLB locally..."
	@echo "Version: $(VERSION)"
	@echo "Git Tag: $(GIT_TAG)"
	@echo "Git Commit: $(GIT_COMMIT)"
	@echo "Build Time: $(BUILD_TIME)"
	@cd app && go build -ldflags "\
		-X 'github.com/cloudresty/nautiluslb/version.Version=$(VERSION)' \
		-X 'github.com/cloudresty/nautiluslb/version.GitCommit=$(GIT_COMMIT)' \
		-X 'github.com/cloudresty/nautiluslb/version.BuildTime=$(BUILD_TIME)' \
		-X 'github.com/cloudresty/nautiluslb/version.GitTag=$(GIT_TAG)'" \
		-o ../nautiluslb .

build: ## Build a docker image locally with version information.
	@echo "Building Docker image with version information..."
	@echo "Version: $(VERSION)"
	@docker build \
		--platform linux/amd64 \
		--pull \
		--force-rm \
		--build-arg VERSION="$(VERSION)" \
		--build-arg GIT_COMMIT="$(GIT_COMMIT)" \
		--build-arg BUILD_TIME="$(BUILD_TIME)" \
		--build-arg GIT_TAG="$(GIT_TAG)" \
		--tag ${DOCKER_REPO}:${DOCKER_TAG} \
		--file build/Dockerfile .

docker-build: build ## Alias for build target.

test: ## Run all tests.
	@cd app && go test ./...

run: ## Run docker image locally.
	@docker run \
		--platform linux/amd64 \
		--rm \
		--name ${NAME} \
		--hostname ${NAME}-${DOCKER_TAG} \
		${DOCKER_REPO}:${DOCKER_TAG}

shell: ## Run docker image in a shell locally.
	@docker run \
		--platform linux/amd64 \
		--rm \
		--name ${NAME} \
		--hostname ${NAME}-${DOCKER_TAG} \
		--interactive \
		--tty \
		--volume $$(pwd)/app/config.yaml:/nautiluslb/config.yaml \
		--volume $$(pwd)/app/kubeconfig:/root/.kube/config \
		--entrypoint /bin/bash \
		${DOCKER_REPO}:${DOCKER_TAG}

clean: ## Remove all local docker images for the application.
	@if [[ $$(docker images --format '{{.Repository}}:{{.Tag}}' | grep ${DOCKER_REPO}) ]]; then docker rmi $$(docker images --format '{{.Repository}}:{{.Tag}}' | grep ${DOCKER_REPO}); else echo "INFO: No images found for '${DOCKER_REPO}'"; fi