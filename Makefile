BASE = cloudresty
NAME = $$(awk -F'/' '{print $$(NF-0)}' <<< $$PWD)
DOCKER_REPO = ${BASE}/${NAME}
DOCKER_TAG = test

.PHONY: build run shell clean

help: ## Show list of make targets and their description.
	@grep -E '^[%a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-15s\033[0m %s\n", $$1, $$2}'

build: ## Build a docker image locally. Requires /build/.netrc to be present.
	@docker build \
		--pull \
		--force-rm \
		--tag ${DOCKER_REPO}:${DOCKER_TAG} \
		--file build/Dockerfile .

run: ## Run docker image locally.
	@docker run \
		--rm \
		--name ${NAME} \
		--hostname ${NAME}-${DOCKER_TAG} \
		${DOCKER_REPO}:${DOCKER_TAG}

shell: ## Run docker image in a shell locally.
	@docker run \
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