# ai-sandbox operator Makefile.
#
# The host/agent has no Go toolchain, so every Go command runs inside a
# disposable golang container against the rootless DinD daemon
# (DOCKER_HOST=tcp://docker:2375). The daemon can only see /workspace, so we
# mount that and set the workdir to this directory's path inside it.
#
# IN_CONTAINER=1 makes DOCKER_RUN empty and runs the go commands directly --
# used by GitHub Actions and when make itself is already running inside the
# golang container (the agent container has no `make`).

GOLANG_IMAGE   ?= golang:1.25
DOCKER         ?= docker
IN_CONTAINER   ?= 0

SHARED_DIR      ?= /workspace
CACHE_SUBDIR    ?= .gocache-operator
DEPENDAPROXY_IP ?= 172.23.0.10
GO_ENV_FILE     ?= $(SHARED_DIR)/go.env
IMG             ?= ghcr.io/psenna/ai-sandbox-operator:dev

GOLANGCI_LINT_VERSION    ?= v2.12.2
GOVULNCHECK_VERSION      ?= v1.6.0
KUSTOMIZE_VERSION        ?= v5.8.1

# When invoking `make IN_CONTAINER=1 envtest-assets|test-envtest` manually
# from outside (the agent has no host `make`, so this means running make
# itself inside a golang container), mount SHARED_DIR at its own path -- e.g.
# `-v /workspace:/workspace -w /workspace/ai-sandbox/operator` -- NOT
# remapped to /work the way DOCKER_RUN's own wrapping does for every other
# target. IN_CONTAINER=1 skips that /work rewrite entirely (see
# ENVTEST_TARGET_DIR below), matching how CI invokes it with no SHARED_DIR
# concept at all.
ENVTEST_K8S_VERSION ?= 1.35.0
ENVTEST_DIR         ?= $(SHARED_DIR)/.envtest

REPO_SUBDIR := $(patsubst $(SHARED_DIR)/%,%,$(CURDIR))
CACHE_DIR   := /work/$(CACHE_SUBDIR)
GO_ENV_FLAG := $(if $(wildcard $(GO_ENV_FILE)),--env-file $(GO_ENV_FILE),)

# hack/tools is a separate Go module (never referenced from operator/go.mod)
# that builds controller-gen. Its own go.mod pins golang.org/x/mod above
# every version DependaProxy's CVE gate denies; that pin only resolves
# against a local module cache, not the public proxy. When
# /workspace/gomodcache/cache/download exists (this sandbox), prefer it,
# falling back to DependaProxy. On a normal CI runner (no such path) this
# evaluates empty and hack/tools builds against the ordinary Go proxy chain.
X_MOD_CACHE   := $(SHARED_DIR)/gomodcache/cache/download
comma         := ,
TOOLS_GOPROXY := $(if $(wildcard $(X_MOD_CACHE)/golang.org/x/mod/@v),file:///work/gomodcache/cache/download$(comma)http://dependaproxy:8080/goproxy,)

# ENVTEST_DIR is a host-side path (default under SHARED_DIR). When
# DOCKER_RUN wraps commands in a container, SHARED_DIR is mounted at /work,
# so the in-container path must be rewritten; when IN_CONTAINER=1 there is
# no such wrapping (CI passes its own ENVTEST_DIR, e.g. $PWD/.envtest) and
# the path is used as-is.
ENVTEST_TARGET_DIR := $(if $(filter 1,$(IN_CONTAINER)),$(ENVTEST_DIR),/work/$(patsubst $(SHARED_DIR)/%,%,$(ENVTEST_DIR)))

ifeq ($(IN_CONTAINER),1)
DOCKER_RUN :=
else
ifeq ($(REPO_SUBDIR),$(CURDIR))
$(error CURDIR ($(CURDIR)) is not under SHARED_DIR ($(SHARED_DIR)) -- set SHARED_DIR=, or run with IN_CONTAINER=1)
endif
DOCKER_RUN := $(DOCKER) run --rm -u $(shell id -u):$(shell id -g) \
	-v $(SHARED_DIR):/work -w /work/$(REPO_SUBDIR) \
	--add-host=dependaproxy:$(DEPENDAPROXY_IP) $(GO_ENV_FLAG) \
	-e HOME=$(CACHE_DIR) -e GOCACHE=$(CACHE_DIR)/build \
	-e GOMODCACHE=$(CACHE_DIR)/mod -e GOBIN=$(CACHE_DIR)/bin \
	-e PATH=$(CACHE_DIR)/bin:/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
	-e GOFLAGS=-buildvcs=false -e GOTOOLCHAIN=local \
	$(GOLANG_IMAGE)
endif

.PHONY: all build test vet fmt fmt-check lint vuln tidy manifests generate \
	kustomize-build docker-build cross smoke clean envtest-assets test-envtest

all: vet fmt-check lint test

build:
	$(DOCKER_RUN) go build -trimpath -o bin/manager ./cmd

test:
	$(DOCKER_RUN) sh -c 'CGO_ENABLED=1 go test -race -count=1 -coverprofile=cover.out ./...'

vet:
	$(DOCKER_RUN) go vet ./...

fmt:
	$(DOCKER_RUN) gofmt -w .

fmt-check:
	$(DOCKER_RUN) sh -c 'test -z "$$(gofmt -l .)" || { echo "gofmt drift:"; gofmt -l .; exit 1; }'

tidy:
	$(DOCKER_RUN) go mod tidy

lint:
	$(DOCKER_RUN) sh -c 'command -v golangci-lint >/dev/null || \
		go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION); \
		golangci-lint run ./...'

vuln:
	$(DOCKER_RUN) sh -c 'command -v govulncheck >/dev/null || \
		go install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION); \
		govulncheck ./...'

# bin/controller-gen is a real file target (not phony): built from the
# hack/tools module, which is never referenced from operator/go.mod. See
# operator/README.md for why go install controller-gen@version is a dead
# end here (it transitively needs golang.org/x/mod, denied by DependaProxy's
# CVE gate at every version, and go install can't route around that pin the
# way a module's own go.mod requires can).
bin/controller-gen:
	$(DOCKER_RUN) sh -c '$(if $(TOOLS_GOPROXY),GOPROXY="$(TOOLS_GOPROXY)",) \
	  go -C hack/tools build -mod=readonly -o ../../bin/controller-gen \
	     sigs.k8s.io/controller-tools/cmd/controller-gen'

manifests: bin/controller-gen
	$(DOCKER_RUN) ./bin/controller-gen crd paths="./..." \
	  output:crd:artifacts:config=config/crd/bases
	# rbac generator restored in #18 once +kubebuilder:rbac markers exist

generate: bin/controller-gen
	$(DOCKER_RUN) ./bin/controller-gen object:headerFile="hack/boilerplate.go.txt" paths="./..."

envtest-assets:
	$(DOCKER_RUN) ./hack/fetch-envtest.sh $(ENVTEST_K8S_VERSION) $(ENVTEST_TARGET_DIR)

test-envtest: envtest-assets
	$(DOCKER_RUN) sh -c 'CGO_ENABLED=1 \
	  KUBEBUILDER_ASSETS=$(ENVTEST_TARGET_DIR)/k8s/$(ENVTEST_K8S_VERSION) \
	  go test -race -count=1 -coverprofile=cover.out ./...'

kustomize-build:
	$(DOCKER_RUN) sh -c 'command -v kustomize >/dev/null || \
		go install sigs.k8s.io/kustomize/kustomize/v5@$(KUSTOMIZE_VERSION); \
		kustomize build config/default'

cross:
	$(DOCKER_RUN) sh -c 'for a in amd64 arm64; do echo "==> linux/$$a"; \
		CGO_ENABLED=0 GOOS=linux GOARCH=$$a go build -trimpath -o bin/manager-linux-$$a ./cmd; done'

docker-build:
	$(DOCKER) build --add-host=dependaproxy:$(DEPENDAPROXY_IP) \
		--build-arg GOPROXY=http://dependaproxy:8080/goproxy -t $(IMG) .

smoke: build
	$(DOCKER_RUN) ./hack/smoke.sh

clean:
	$(DOCKER_RUN) rm -rf bin cover.out cover.html
