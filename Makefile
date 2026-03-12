# Build supervisord binary (default)
.PHONY: build
build:
	go build -o supervisord .

# Build the init container image for cloudphone-operator.
# Usage: make docker-build-init [IMAGE=your-registry/supervisord-init:latest]
IMAGE ?= supervisord-init:latest
.PHONY: docker-build-init
docker-build-init:
	docker build -f Dockerfile.init -t $(IMAGE) .
