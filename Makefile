# Build supervisord binary (default)
.PHONY: build
build:
	go build -o supervisord .

# Build the init container image for cloudphone-operator.
# Context is the monorepo root (parent of this dir) so Dockerfile can COPY supervisord + cloudphone-nodeagent-api.
# Usage: make docker-build-init [IMAGE=your-registry/supervisord-init:latest] [REPO_ROOT=..]
IMAGE ?= supervisord-init:latest
REPO_ROOT ?= $(abspath ..)
.PHONY: docker-build-init
docker-build-init:
	cd $(REPO_ROOT) && docker build -f supervisord/Dockerfile.init -t $(IMAGE) .
