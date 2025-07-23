BASE = cloudresty
NAME = $$(awk -F'/' '{print $$(NF-0)}' <<< $$PWD)
DOCKER_REPO = ${BASE}/${NAME}
DOCKER_TAG = test

.PHONY: build build-local run shell clean test docker-build

help: ## Show list of make targets and their description.
	@grep -E '^[%a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-15s\033[0m %s\n", $$1, $$2}'

build-local: ## Build the binary locally.
	@echo "Building NautilusLB locally..."
	@cd app && go build -o ../nautiluslb .

build: ## Build a docker image locally.
	@echo "Building Docker image..."
	@docker build \
		--platform linux/amd64 \
		--pull \
		--force-rm \
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