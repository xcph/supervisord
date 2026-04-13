# 私有仓库、buildx、buildkit-login（与 portgateway/Makefile 对齐；见 make help）。
# Init 镜像构建上下文为 monorepo 根目录（含 supervisord、cloudphone-nodeagent-api）。
# 推送内网示例: make buildkit-login REGISTRY_USER=... REGISTRY_PASS=... REGISTRY_ENDPOINT=registry.k8s.local:10443
#             && make docker-buildx IMG=registry.k8s.local:10443/k8s/supervisord-init:v0.1.0 PLATFORMS=linux/amd64,linux/arm64

IMG ?= supervisord-init:latest
# 兼容旧用法：README 中的 IMAGE= 会覆盖 IMG
ifneq ($(strip $(IMAGE)),)
override IMG := $(IMAGE)
endif
REPO_ROOT ?= $(abspath ..)

ifeq (,$(shell go env GOBIN))
GOBIN := $(shell go env GOPATH)/bin
else
GOBIN := $(shell go env GOBIN)
endif

CONTAINER_TOOL ?= docker

BUILDX_LOCAL_BUILDER ?= supervisord-local

BUILDX_PUSH_FLAG ?= --push
BUILDX_LOAD_FLAG ?= --load
LOCAL_PUSH ?= 0
BUILDX_LOCAL_OUTPUT_FLAG :=
ifeq ($(strip $(LOCAL_PUSH)),1)
  BUILDX_LOCAL_OUTPUT_FLAG := $(BUILDX_PUSH_FLAG)
else
  BUILDX_LOCAL_OUTPUT_FLAG := $(BUILDX_LOAD_FLAG)
endif

BUILDKIT_DOCKER_CONFIG_DIR ?= $(abspath $(CURDIR)/.docker-config.buildkit)
BUILDKIT_DOCKER_CONFIG_ENV := DOCKER_CONFIG="$(BUILDKIT_DOCKER_CONFIG_DIR)"

KEEP_BUILDX_BUILDER ?= 1

DOCKER_BUILDKIT ?= 1
export DOCKER_BUILDKIT

.PHONY: docker-buildx-builder-ensure
docker-buildx-builder-ensure: docker-buildx-buildkitd-effective
	@if ! $(BUILDKIT_DOCKER_CONFIG_ENV) $(CONTAINER_TOOL) buildx inspect supervisord-builder >/dev/null 2>&1; then \
		echo "buildx: creating builder supervisord-builder"; \
		$(BUILDKIT_DOCKER_CONFIG_ENV) $(CONTAINER_TOOL) buildx create --name supervisord-builder --driver docker-container --use --bootstrap \
			--buildkitd-config $(BUILDKITD_CONFIG_EFFECTIVE) $(BUILDX_DOCKER_CONTAINER_DRIVER_OPTS); \
	else \
		$(BUILDKIT_DOCKER_CONFIG_ENV) $(CONTAINER_TOOL) buildx use supervisord-builder; \
	fi

.PHONY: docker-buildx-builder-rm
docker-buildx-builder-rm:
	- $(BUILDKIT_DOCKER_CONFIG_ENV) $(CONTAINER_TOOL) buildx rm supervisord-builder 2>/dev/null || true

.PHONY: buildkit-login
buildkit-login: ## 写入独立 DOCKER_CONFIG（不污染 ~/.docker）。用法: make buildkit-login REGISTRY_USER=... REGISTRY_PASS=... REGISTRY_ENDPOINT=...
	@test -n "$(strip $(REGISTRY_USER))" || (echo "ERR: REGISTRY_USER is required" >&2; exit 2)
	@test -n "$(strip $(REGISTRY_PASS))" || (echo "ERR: REGISTRY_PASS is required" >&2; exit 2)
	@mkdir -p "$(BUILDKIT_DOCKER_CONFIG_DIR)"
	@REGISTRY_ENDPOINT="$(REGISTRY_ENDPOINT)" REGISTRY_USER="$(REGISTRY_USER)" REGISTRY_PASS="$(REGISTRY_PASS)" OUT_DIR="$(BUILDKIT_DOCKER_CONFIG_DIR)" \
	python3 -c 'import base64, json, os, pathlib, shutil; reg=os.environ["REGISTRY_ENDPOINT"]; user=os.environ["REGISTRY_USER"]; pw=os.environ["REGISTRY_PASS"]; out_dir=pathlib.Path(os.environ["OUT_DIR"]); out_dir.mkdir(parents=True, exist_ok=True); auth_b64=base64.b64encode(f"{user}:{pw}".encode("utf-8")).decode("ascii"); auths={reg: {"auth": auth_b64}, f"https://{reg}": {"auth": auth_b64}, f"http://{reg}": {"auth": auth_b64}}; cfg={"auths": auths}; p=out_dir/"config.json"; p.write_text(json.dumps(cfg, indent=2)+"\n", encoding="utf-8"); print(f"Wrote {p}"); cli_plugins=out_dir/"cli-plugins"; cli_plugins.mkdir(parents=True, exist_ok=True); src=shutil.which("docker-buildx"); dst=cli_plugins/"docker-buildx"; \
 (dst.exists() or dst.is_symlink()) and dst.unlink(); \
 src and dst.symlink_to(src) and print(f"Linked {dst} -> {src}")'

BUILDX_EXTRA_HOSTS ?=
BUILDX_HOST_IP ?=
BUILDX_HOST_NAME ?=
BUILDX_ADD_HOST_SINGLE := $(strip $(if $(and $(strip $(BUILDX_HOST_IP)),$(strip $(BUILDX_HOST_NAME))),--add-host=$(strip $(BUILDX_HOST_NAME)):$(strip $(BUILDX_HOST_IP)),))
BUILDX_ADD_HOST_MULTI := $(foreach h,$(BUILDX_EXTRA_HOSTS),$(if $(findstring :,$(h)),--add-host=$(h),))
BUILDX_ADD_HOST_FLAGS := $(strip $(BUILDX_ADD_HOST_SINGLE) $(BUILDX_ADD_HOST_MULTI))

REGISTRY_ENDPOINT ?= registry.k8s.local:10443
# 当 REGISTRY_ENDPOINT 为 IP:port 且仓库为明文 HTTP 时设为 1，在生成的 registry 段追加 http = true
REGISTRY_PLAIN_HTTP ?=

ifeq ($(origin BUILDX_LOCAL_BUILDER_NETWORK_HOST),undefined)
  ifneq ($(strip $(BUILDX_EXTRA_HOSTS))$(strip $(BUILDX_HOST_IP)),)
    BUILDX_LOCAL_BUILDER_NETWORK_HOST := 1
  else
    BUILDX_LOCAL_BUILDER_NETWORK_HOST := 0
  endif
endif
BUILDX_LOCAL_BUILDER_EFFECTIVE := $(BUILDX_LOCAL_BUILDER)$(if $(filter 1,$(strip $(BUILDX_LOCAL_BUILDER_NETWORK_HOST))),-nethost,)
BUILDX_DOCKER_CONTAINER_DRIVER_OPTS :=
ifeq ($(strip $(BUILDX_LOCAL_BUILDER_NETWORK_HOST)),1)
  BUILDX_DOCKER_CONTAINER_DRIVER_OPTS := --driver-opt network=host
endif

SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

BUILDKITD_CONFIG_SRC := $(abspath $(CURDIR)/buildkitd.toml)
BUILDKITD_CONFIG_EFFECTIVE := $(abspath $(CURDIR)/.buildkitd.effective.toml)

PLATFORM ?=
DOCKER_BUILD_PLATFORM_FLAGS :=
ifneq ($(strip $(PLATFORM)),)
  DOCKER_BUILD_PLATFORM_FLAGS := --platform=$(PLATFORM)
endif

DOCKER_BUILD_CONTEXT ?= $(REPO_ROOT)
DOCKERFILE_INIT ?= supervisord/Dockerfile.init
# 可选：传给 Dockerfile.init 的 Alpine apk 镜像主机（默认 Dockerfile 为 dl-cdn）
ALPINE_APK_REPO_HOST ?=
# 可选：构建期代理（亦可在 shell 中 export，由 buildx 子进程继承；此处显式 --build-arg 便于 make 传参）
DOCKER_INIT_BUILD_ARGS :=
ifneq ($(strip $(ALPINE_APK_REPO_HOST)),)
DOCKER_INIT_BUILD_ARGS += --build-arg ALPINE_APK_REPO_HOST=$(ALPINE_APK_REPO_HOST)
endif
ifneq ($(strip $(HTTP_PROXY)),)
DOCKER_INIT_BUILD_ARGS += --build-arg HTTP_PROXY=$(HTTP_PROXY)
endif
ifneq ($(strip $(HTTPS_PROXY)),)
DOCKER_INIT_BUILD_ARGS += --build-arg HTTPS_PROXY=$(HTTPS_PROXY)
endif
ifneq ($(strip $(NO_PROXY)),)
DOCKER_INIT_BUILD_ARGS += --build-arg NO_PROXY=$(NO_PROXY)
endif
ifneq ($(strip $(http_proxy)),)
DOCKER_INIT_BUILD_ARGS += --build-arg http_proxy=$(http_proxy)
endif
ifneq ($(strip $(https_proxy)),)
DOCKER_INIT_BUILD_ARGS += --build-arg https_proxy=$(https_proxy)
endif
ifneq ($(strip $(no_proxy)),)
DOCKER_INIT_BUILD_ARGS += --build-arg no_proxy=$(no_proxy)
endif

PLATFORMS ?= linux/arm64,linux/amd64,linux/s390x,linux/ppc64le
BUILDX_CACHE_DIR ?= $(HOME)/.cache/supervisord/buildx
BUILDX_CACHE_TO ?= type=local,dest=$(BUILDX_CACHE_DIR),mode=max
BUILDX_CACHE_IMPORT_FLAGS :=
ifdef BUILDX_CACHE_REF
BUILDX_CACHE_TO = type=registry,ref=$(BUILDX_CACHE_REF),mode=max
BUILDX_CACHE_IMPORT_FLAGS = --cache-from type=registry,ref=$(BUILDX_CACHE_REF)
else
ifneq ($(wildcard $(BUILDX_CACHE_DIR)/index.json),)
BUILDX_CACHE_IMPORT_FLAGS = --cache-from type=local,src=$(BUILDX_CACHE_DIR)
endif
endif
ifneq ($(SKIP_BUILDX_CACHE_IMPORT),)
BUILDX_CACHE_IMPORT_FLAGS :=
endif
ifneq ($(SKIP_BUILDX_CACHE),)
BUILDX_CACHE_IMPORT_FLAGS :=
BUILDX_CACHE_TO :=
endif
BUILDX_NO_CACHE_FLAGS :=
ifneq ($(BUILDX_NO_CACHE),)
BUILDX_NO_CACHE_FLAGS = --no-cache
endif
BUILDX_CACHE_WRITE_FLAGS :=
ifneq ($(strip $(BUILDX_CACHE_TO)),)
BUILDX_CACHE_WRITE_FLAGS = --cache-to $(BUILDX_CACHE_TO)
endif

EXTRA_TAGS ?=

.PHONY: all
all: build

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Build

.PHONY: build
build: ## go build -o supervisord .
	go build -o supervisord .

.PHONY: tidy
tidy: ## go mod tidy
	go mod tidy

.PHONY: test
test: ## go test ./...
	go test ./...

##@ Docker（buildkit-login、IMG、LOCAL_PUSH、docker-buildx）

.PHONY: docker-buildx-buildkitd-effective
docker-buildx-buildkitd-effective: ## 合并 buildkitd.toml；IP:port 追加 insecure；REGISTRY_PLAIN_HTTP=1 时追加 http=true
	@cp "$(BUILDKITD_CONFIG_SRC)" "$(BUILDKITD_CONFIG_EFFECTIVE)"
	@REG_EP='$(REGISTRY_ENDPOINT)'; \
	if echo "$$REG_EP" | grep -qE '^[0-9]{1,3}(\.[0-9]{1,3}){3}:[0-9]+$$'; then \
	  printf '\n# generated: REGISTRY_ENDPOINT=%s (IP pulls need matching [registry."..."] insecure)\n[registry."%s"]\n  insecure = true\n' "$$REG_EP" "$$REG_EP" >> "$(BUILDKITD_CONFIG_EFFECTIVE)"; \
	  if [ "$(strip $(REGISTRY_PLAIN_HTTP))" = "1" ]; then printf '  http = true\n' >> "$(BUILDKITD_CONFIG_EFFECTIVE)"; fi; \
	fi

.PHONY: docker-buildx-local-ensure
docker-buildx-local-ensure: docker-buildx-buildkitd-effective ## 创建/选用带 buildkitd.toml 的本地 buildx builder
	@PREV_EP="$(CURDIR)/.buildkitd.endpoint.prev"; \
	PREV_CFG="$(CURDIR)/.buildkitd.config.prev"; \
	CFG_HASH="$$( (sha256sum "$(BUILDKITD_CONFIG_EFFECTIVE)" 2>/dev/null || shasum -a 256 "$(BUILDKITD_CONFIG_EFFECTIVE)") | awk '{print $$1}' )"; \
	NEED_RECREATE=""; \
	BUILDER_EXISTS=""; \
	if $(BUILDKIT_DOCKER_CONFIG_ENV) $(CONTAINER_TOOL) buildx inspect $(BUILDX_LOCAL_BUILDER_EFFECTIVE) >/dev/null 2>&1; then \
	  BUILDER_EXISTS="1"; \
	fi; \
	if [ -f "$$PREV_EP" ] && [ "$$(cat "$$PREV_EP" 2>/dev/null)" != "$(REGISTRY_ENDPOINT)" ]; then \
	  NEED_RECREATE="REGISTRY_ENDPOINT changed"; \
	fi; \
	if [ -f "$$PREV_CFG" ] && [ "$$(cat "$$PREV_CFG" 2>/dev/null)" != "$$CFG_HASH" ]; then \
	  NEED_RECREATE="$${NEED_RECREATE:+$$NEED_RECREATE; }buildkitd config changed"; \
	fi; \
	if [ -n "$$BUILDER_EXISTS" ] && { [ ! -f "$$PREV_EP" ] || [ ! -f "$$PREV_CFG" ]; }; then \
	  NEED_RECREATE="$${NEED_RECREATE:+$$NEED_RECREATE; }missing local buildkitd state cache"; \
	fi; \
	if [ -n "$$NEED_RECREATE" ]; then \
	  echo "buildx: $$NEED_RECREATE, recreating builder $(BUILDX_LOCAL_BUILDER_EFFECTIVE)"; \
	  $(BUILDKIT_DOCKER_CONFIG_ENV) $(CONTAINER_TOOL) buildx rm $(BUILDX_LOCAL_BUILDER_EFFECTIVE) 2>/dev/null || true; \
	fi; \
	echo '$(REGISTRY_ENDPOINT)' > "$$PREV_EP"; \
	echo "$$CFG_HASH" > "$$PREV_CFG"
	@if [ "$(BUILDX_LOCAL_BUILDER_NETWORK_HOST)" = "1" ]; then \
		echo "buildx: using builder $(BUILDX_LOCAL_BUILDER_EFFECTIVE) with network=host"; \
	fi
	@if ! $(BUILDKIT_DOCKER_CONFIG_ENV) $(CONTAINER_TOOL) buildx inspect $(BUILDX_LOCAL_BUILDER_EFFECTIVE) >/dev/null 2>&1; then \
		$(BUILDKIT_DOCKER_CONFIG_ENV) $(CONTAINER_TOOL) buildx create --name $(BUILDX_LOCAL_BUILDER_EFFECTIVE) --driver docker-container \
			--buildkitd-config $(BUILDKITD_CONFIG_EFFECTIVE) $(BUILDX_DOCKER_CONTAINER_DRIVER_OPTS) --bootstrap --use; \
	else \
		$(BUILDKIT_DOCKER_CONFIG_ENV) $(CONTAINER_TOOL) buildx use $(BUILDX_LOCAL_BUILDER_EFFECTIVE); \
	fi

.PHONY: docker-build
docker-build: docker-buildx-local-ensure ## Init 镜像：单平台 buildx（IMG=...；可选 PLATFORM=、ALPINE_APK_REPO_HOST=、HTTP_PROXY=、HTTPS_PROXY=）
	cd $(DOCKER_BUILD_CONTEXT) && $(BUILDKIT_DOCKER_CONFIG_ENV) $(CONTAINER_TOOL) buildx build $(BUILDX_LOCAL_OUTPUT_FLAG) $(DOCKER_BUILD_PLATFORM_FLAGS) \
		$(BUILDX_ADD_HOST_FLAGS) $(DOCKER_INIT_BUILD_ARGS) -t ${IMG} -f $(DOCKERFILE_INIT) .

.PHONY: docker-build-init
docker-build-init: docker-build ## 与 docker-build 相同（兼容旧目标名）

.PHONY: docker-push
docker-push: ## 推送镜像（tag 已为仓库地址；或 buildkit-login + LOCAL_PUSH=1 在构建时推送）
	$(CONTAINER_TOOL) push ${IMG}

.PHONY: docker-buildx
docker-buildx: docker-buildx-buildkitd-effective ## 多平台构建并推送（需 buildkit-login；IMG=...；PLATFORMS 须覆盖目标节点架构）
	@$(MAKE) docker-buildx-builder-ensure KEEP_BUILDX_BUILDER="$(KEEP_BUILDX_BUILDER)" REGISTRY_ENDPOINT="$(REGISTRY_ENDPOINT)"
	cd $(DOCKER_BUILD_CONTEXT) && $(BUILDKIT_DOCKER_CONFIG_ENV) $(CONTAINER_TOOL) buildx build $(BUILDX_PUSH_FLAG) $(BUILDX_ADD_HOST_FLAGS) $(DOCKER_INIT_BUILD_ARGS) \
		--platform=$(PLATFORMS) --tag ${IMG} $(foreach t,$(EXTRA_TAGS),--tag $(t)) \
		$(BUILDX_CACHE_IMPORT_FLAGS) $(BUILDX_CACHE_WRITE_FLAGS) $(BUILDX_NO_CACHE_FLAGS) \
		-f $(DOCKERFILE_INIT) .
	@if [ "$(KEEP_BUILDX_BUILDER)" != "1" ]; then $(MAKE) docker-buildx-builder-rm; fi
