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

# The DinD daemon's own compose-network IP -- needed only by E2E_TOOLBOX_RUN,
# which launches a container that itself needs to reach DOCKER_HOST by the
# "docker" name (a fresh container has no /etc/hosts entry for it the way
# this repo's own dev containers do).
DOCKER_HOST_IP ?= 172.23.0.2

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

# operator/test/e2e is a kind-based end-to-end suite, and a separate Go
# module (see test/e2e/go.mod's header comment for why). E2E_BIN_DIR and
# E2E_KUBECONFIG default under SHARED_DIR directly (NOT under CURDIR), the
# same way ENVTEST_DIR does, so they land outside the repo tree and need no
# .gitignore entry; E2E_ARTIFACT_DIR is the one path that DOES land inside
# the repo (test/e2e/_artifacts), and is gitignored accordingly.
KIND_VERSION       ?= v0.30.0
KIND_NODE_IMAGE    ?= kindest/node:v1.34.0
KUBECTL_VERSION    ?= v1.34.0
# Matches HELM_IMAGE's pin below (#34) -- fetched into E2E_BIN_DIR alongside
# kind/kubectl so hack/e2e-up.sh's E2E_DEPLOY=helm path and
# hack/helm-kind-walkthrough.sh always have a helm binary on PATH, even
# wrapped inside E2E_TOOLBOX_RUN's docker:28-cli image (which has neither).
HELM_VERSION       ?= v3.16.4
MINIO_IMAGE        ?= minio/minio:RELEASE.2025-09-07T16-13-09Z
# #24: the in-cluster pull-through registry cache the rootless-podman engine
# specs pull real workload images through (test/e2e/manifests/registry-cache.yaml).
REGISTRY_IMAGE     ?= registry:2.8.3
E2E_CLUSTER        ?= ai-sandbox-e2e
# --- storage-conformance (operator/internal/storage's s3 leg; see that
# package's doc.go) -- a plain MinIO server for a Go client library's own
# tests, deliberately NOT the #22 kind-cluster MinIO pattern
# (test/e2e/manifests/minio.yaml), which is for testing the operator inside
# a real cluster. ---------------------------------------------------------
MINIO_TEST_NETWORK   ?= ai-sandbox-operator-minio-test
MINIO_TEST_CONTAINER ?= ai-sandbox-operator-minio
MINIO_TEST_USER      ?= sandboxtest
MINIO_TEST_PASSWORD  ?= sandboxtest123
E2E_CNI            ?= kindnet
# hack/e2e-up.sh joins the kind docker network and addresses the API server
# via the node container's own name when this is set, instead of the
# 127.0.0.1:<mapped-port> address `kind create cluster` merges by default.
# Needed whenever the process talking to DOCKER_HOST is NOT colocated with
# the daemon's own network namespace -- true of E2E_TOOLBOX_RUN/E2E_GO_RUN
# in this sandbox (a remote/nested DinD daemon) always, and false on a
# normal single-daemon CI runner (IN_CONTAINER=1, no such wrapping) always
# -- hence defaulting on IN_CONTAINER, not hardcoded either way.
E2E_INTERNAL_KUBECONFIG ?= $(if $(filter 1,$(IN_CONTAINER)),,1)
E2E_TOOLBOX_IMAGE  ?= docker:28-cli
E2E_BIN_DIR        ?= $(SHARED_DIR)/.e2e-bin
E2E_KUBECONFIG     ?= $(SHARED_DIR)/.e2e-kubeconfig
E2E_ARTIFACT_DIR   ?= $(CURDIR)/test/e2e/_artifacts
# The wake/restore suite (#29) added several long specs that pushed the full
# suite well past the old 15m budget: never-leaks runs three freeze->wake
# cycles (~9m, Serial), and the corrupted/truncated/freeze-failed/backend-out
# specs form a Serial chain. A local run is ~17m; CI's faster hardware lands
# right at 15m, so the prior --timeout=15m aborted the suite mid-run (the
# e2e-run step exited non-zero after exactly 15m). 25m leaves ~8m of headroom
# for runner variance and modest future growth without cutting the
# acceptance-criteria coverage (the cycle counts are the point of never-leaks).
#
# Raised again to 32m for #33: four new "operator metrics" specs (a real
# freeze/wake/probe cycle, a Serial slot-saturation-and-drain pass, plus two
# fast ones) add several more minutes on top of the above, and #25's ~8m of
# headroom was consumed -- confirmed by a CI run of PR #51's head that hit
# the prior 25m ginkgo timeout mid-suite. Same reasoning as #25's bump:
# leave headroom for runner variance and near-term growth, not just today's
# measured total.
#
# Raised again to 45m for #24: three real image pulls through a cold
# pull-through cache (alpine, postgres:18-alpine, node:22-alpine) dominate the
# new engine_test.go specs, on top of the existing suite's total above.
E2E_TIMEOUT        ?= 45m
E2E_PROCS          ?= 4
E2E_SEED           ?= 1
# Lets CI split the suite into parallel jobs (e.g. --focus-file=... on one
# runner, the complementary --skip-file=... on another) without editing
# GINKGO_FLAGS itself. Empty by default: a plain `make e2e-run` still runs
# every spec.
E2E_GINKGO_EXTRA_FLAGS ?=
GINKGO_FLAGS       ?= --timeout=$(E2E_TIMEOUT) --procs=$(E2E_PROCS) --seed=$(E2E_SEED) --flake-attempts=1 --fail-fast=false --keep-going --poll-progress-after=60s --poll-progress-interval=30s -v $(E2E_GINKGO_EXTRA_FLAGS)

# Pass-through overrides for test/e2e/harness.go's LoadConfig (E2E_* env
# vars); empty defaults are fine -- LoadConfig treats "" as unset and falls
# back to its own built-in default. Declared here (rather than left as bare
# ambient-environment pass-through) so `-e NAME=$(NAME)` in E2E_GO_RUN below
# does not depend on GNU Make's command-line-variable-export semantics.
#
# E2E_HOST is the exception to "empty default is fine": harness.LoadConfig
# defaults an unset E2E_HOST to 127.0.0.1, which is correct for the normal
# single-daemon CI runner (IN_CONTAINER=1 -> E2E_INTERNAL_KUBECONFIG empty:
# the ginkgo process is NOT on the kind network, it reaches the cluster's
# NodePorts through the host's port-forwards at 127.0.0.1) but WRONG for the
# nested sandbox runner (E2E_INTERNAL_KUBECONFIG=1: the ginkgo container is
# launched on --network kind, where 127.0.0.1 is the ginkgo container itself,
# not the cluster). On the kind network the NodePorts are reachable at the
# kind node's hostname, <E2E_CLUSTER>-control-plane, resolvable via Docker
# DNS -- see the E2E_GO_RUN comment below. So default E2E_HOST to that node
# hostname exactly when E2E_INTERNAL_KUBECONFIG is set (the nested case), and
# leave it empty (-> 127.0.0.1) for CI. Overridable on the make command line.
E2E_HOST           ?= $(if $(filter 1,$(E2E_INTERNAL_KUBECONFIG)),$(E2E_CLUSTER)-control-plane,)
E2E_MINIO_PORT     ?=
E2E_BROKER_PORT    ?=
E2E_MODEL_PORT     ?=
E2E_S3PROXY_PORT   ?=
E2E_OPERATOR_NS    ?=
E2E_SERVICES_NS    ?=
E2E_AGENT_IMAGE    ?=
E2E_BUCKET         ?=
E2E_FIXTURE_BUCKET ?=
E2E_BROKER_TOKEN   ?=
E2E_PHASE_TIMEOUT  ?=
E2E_POD_TIMEOUT    ?=
E2E_POLL           ?=
E2E_KEEP           ?= 0

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

# Same rewrite as ENVTEST_TARGET_DIR, applied to the three e2e paths that
# may be SHARED_DIR/CURDIR-anchored host paths: under E2E_TOOLBOX_RUN/
# E2E_GO_RUN's /work mount they must be rewritten; under IN_CONTAINER=1
# (CI, no such mount) they are used as-is.
E2E_BIN_TARGET_DIR      := $(if $(filter 1,$(IN_CONTAINER)),$(E2E_BIN_DIR),/work/$(patsubst $(SHARED_DIR)/%,%,$(E2E_BIN_DIR)))
E2E_KUBECONFIG_TARGET    := $(if $(filter 1,$(IN_CONTAINER)),$(E2E_KUBECONFIG),/work/$(patsubst $(SHARED_DIR)/%,%,$(E2E_KUBECONFIG)))
E2E_ARTIFACT_TARGET_DIR  := $(if $(filter 1,$(IN_CONTAINER)),$(E2E_ARTIFACT_DIR),/work/$(patsubst $(SHARED_DIR)/%,%,$(E2E_ARTIFACT_DIR)))

ifeq ($(IN_CONTAINER),1)
DOCKER_RUN :=
E2E_TOOLBOX_RUN :=
E2E_GO_RUN :=
MINIO_TEST_RUN :=
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

# E2E_TOOLBOX_RUN: docker:28-cli (bundles buildx + the docker CLI) for
# anything needing `docker buildx build`, `kind`, or `kubectl` -- i.e.
# hack/e2e-up.sh, hack/e2e-down.sh, hack/e2e-collect.sh. DOCKER_HOST is set
# INSIDE the container so its own nested docker/kind/kubectl invocations
# reach the DinD daemon; --add-host resolves the "docker" name a fresh
# container otherwise has no /etc/hosts entry for. PATH is prefixed with
# the fetched kind/kubectl binaries' rewritten location.
E2E_TOOLBOX_RUN := $(DOCKER) run --rm -u $(shell id -u):$(shell id -g) \
	-v $(SHARED_DIR):/work -w /work/$(REPO_SUBDIR) \
	--add-host=docker:$(DOCKER_HOST_IP) -e DOCKER_HOST=tcp://docker:2375 \
	-e E2E_CLUSTER=$(E2E_CLUSTER) -e E2E_CNI=$(E2E_CNI) \
	-e KIND_NODE_IMAGE=$(KIND_NODE_IMAGE) -e MINIO_IMAGE=$(MINIO_IMAGE) \
	-e REGISTRY_IMAGE=$(REGISTRY_IMAGE) \
	-e E2E_KUBECONFIG=$(E2E_KUBECONFIG_TARGET) -e E2E_ARTIFACT_DIR=$(E2E_ARTIFACT_TARGET_DIR) \
	-e E2E_INTERNAL_KUBECONFIG=$(E2E_INTERNAL_KUBECONFIG) \
	-e HOME=$(CACHE_DIR) \
	-e PATH=$(E2E_BIN_TARGET_DIR):/usr/local/bin:/usr/bin:/bin \
	$(E2E_TOOLBOX_IMAGE)

# E2E_GO_RUN: golang:1.25 on the kind docker network (`--network kind`, so
# it can resolve `<cluster>-control-plane` and reach its NodePorts) for
# `make e2e-run`'s ginkgo invocation. Same GOCACHE/GOMODCACHE/GOPROXY
# conventions as DOCKER_RUN, plus TOOLS_GOPROXY (reused verbatim) for
# resolving ginkgo through the file-cache/DependaProxy chain.
E2E_GO_RUN := $(DOCKER) run --rm -u $(shell id -u):$(shell id -g) \
	--network kind \
	-v $(SHARED_DIR):/work -w /work/$(REPO_SUBDIR)/test/e2e \
	--add-host=dependaproxy:$(DEPENDAPROXY_IP) $(GO_ENV_FLAG) \
	-e HOME=$(CACHE_DIR) -e GOCACHE=$(CACHE_DIR)/build \
	-e GOMODCACHE=$(CACHE_DIR)/mod -e GOBIN=$(CACHE_DIR)/bin \
	-e PATH=$(CACHE_DIR)/bin:/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
	-e GOFLAGS=-buildvcs=false -e GOTOOLCHAIN=local \
	$(if $(TOOLS_GOPROXY),-e GOPROXY=$(TOOLS_GOPROXY),) \
	-e KUBECONFIG=$(E2E_KUBECONFIG_TARGET) -e E2E_ARTIFACT_DIR=$(E2E_ARTIFACT_TARGET_DIR) \
	-e E2E_HOST=$(E2E_HOST) -e E2E_MINIO_PORT=$(E2E_MINIO_PORT) -e E2E_BROKER_PORT=$(E2E_BROKER_PORT) -e E2E_MODEL_PORT=$(E2E_MODEL_PORT) -e E2E_S3PROXY_PORT=$(E2E_S3PROXY_PORT) \
	-e E2E_OPERATOR_NS=$(E2E_OPERATOR_NS) -e E2E_SERVICES_NS=$(E2E_SERVICES_NS) -e E2E_AGENT_IMAGE=$(E2E_AGENT_IMAGE) \
	-e E2E_BUCKET=$(E2E_BUCKET) -e E2E_FIXTURE_BUCKET=$(E2E_FIXTURE_BUCKET) -e E2E_BROKER_TOKEN=$(E2E_BROKER_TOKEN) \
	-e E2E_PHASE_TIMEOUT=$(E2E_PHASE_TIMEOUT) -e E2E_POD_TIMEOUT=$(E2E_POD_TIMEOUT) -e E2E_POLL=$(E2E_POLL) \
	$(GOLANG_IMAGE)

# MINIO_TEST_RUN: like DOCKER_RUN, but joins MINIO_TEST_NETWORK (a
# user-defined bridge network) so it can address the sibling MinIO
# container started by `make minio-up` by container name -- this sandbox's
# DinD daemon is remote (no local docker socket), so two containers can only
# reach each other through a shared user-defined network, the same
# constraint E2E_GO_RUN's `--network kind` already documents.
MINIO_TEST_RUN := $(DOCKER) run --rm -u $(shell id -u):$(shell id -g) \
	--network $(MINIO_TEST_NETWORK) \
	-v $(SHARED_DIR):/work -w /work/$(REPO_SUBDIR) \
	--add-host=dependaproxy:$(DEPENDAPROXY_IP) $(GO_ENV_FLAG) \
	-e HOME=$(CACHE_DIR) -e GOCACHE=$(CACHE_DIR)/build \
	-e GOMODCACHE=$(CACHE_DIR)/mod -e GOBIN=$(CACHE_DIR)/bin \
	-e PATH=$(CACHE_DIR)/bin:/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
	-e GOFLAGS=-buildvcs=false -e GOTOOLCHAIN=local \
	-e SANDBOX_TEST_MINIO_ENDPOINT=http://$(MINIO_TEST_CONTAINER):9000 \
	-e SANDBOX_TEST_MINIO_ACCESS_KEY=$(MINIO_TEST_USER) -e SANDBOX_TEST_MINIO_SECRET_KEY=$(MINIO_TEST_PASSWORD) \
	$(GOLANG_IMAGE)
endif

.PHONY: all build test vet fmt fmt-check lint vuln tidy manifests generate \
	crd-docs crd-docs-check \
	kustomize-build docker-build cross smoke clean envtest-assets test-envtest \
	e2e-tools e2e-up e2e-run e2e-down e2e e2e-tidy e2e-vet e2e-collect \
	minio-up minio-down test-minio storage-conformance \
	helm-crds helm-crds-check helm-rbac-check helm-lint helm-template helm-kind \
	quickstart-check

all: vet fmt-check lint test

build:
	$(DOCKER_RUN) go build -trimpath -o bin/manager ./cmd
	$(DOCKER_RUN) go build -trimpath -o bin/sandboxctl ./cmd/sandboxctl

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
	$(DOCKER_RUN) ./bin/controller-gen crd rbac:roleName=manager-role paths="./..." \
	  output:crd:artifacts:config=config/crd/bases \
	  output:rbac:artifacts:config=config/rbac

generate: bin/controller-gen
	$(DOCKER_RUN) ./bin/controller-gen object:headerFile="hack/boilerplate.go.txt" paths="./..."

# --- CRD reference doc (docs/crd-reference.md) ----------------------------
#
# crd-docs regenerates docs/crd-reference.md from config/crd/bases/*.yaml --
# controller-gen's own output from the Go doc comments and +kubebuilder
# markers on api/v1alpha1. The full chain is therefore
#   Go doc comment -> controller-gen -> config/crd/bases -> this markdown,
# and CI (.github/workflows/operator.yml, `api` job) runs
# `make manifests generate crd-docs` followed by a git diff --exit-code, so
# the doc cannot drift from the types. Same "generated artifact is committed
# and CI re-derives it" shape as `manifests`/`generate` themselves.
#
# NOTE: this target does NOT depend on bin/controller-gen -- it reads the
# COMMITTED CRD YAML, so it needs only the operator module's own deps and
# runs in a plain golang container with no hack/tools build.
CRD_FILES ?= config/crd/bases/sandbox.psenna.dev_sandboxclasses.yaml \
             config/crd/bases/sandbox.psenna.dev_sandboxenvironments.yaml

crd-docs:
	$(DOCKER_RUN) sh -c 'go run ./hack/crdref $(CRD_FILES) > docs/crd-reference.md'

# crd-docs-check fails if the committed docs/crd-reference.md differs from a
# fresh render -- the same shape as helm-crds-check/helm-rbac-check above.
crd-docs-check:
	@$(DOCKER_RUN) sh -c 'go run ./hack/crdref $(CRD_FILES) > /tmp/crd-reference.md; \
	  if ! diff -u docs/crd-reference.md /tmp/crd-reference.md; then \
	    echo "crd-docs-check: docs/crd-reference.md has drifted from config/crd/bases -- run '"'"'make crd-docs'"'"'" >&2; \
	    exit 1; \
	  fi'

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
		CGO_ENABLED=0 GOOS=linux GOARCH=$$a go build -trimpath -o bin/manager-linux-$$a ./cmd; \
		CGO_ENABLED=0 GOOS=linux GOARCH=$$a go build -trimpath -o bin/sandboxctl-linux-$$a ./cmd/sandboxctl; done'

docker-build:
	$(DOCKER) build --add-host=dependaproxy:$(DEPENDAPROXY_IP) \
		--build-arg GOPROXY=http://dependaproxy:8080/goproxy -t $(IMG) .

smoke: build
	$(DOCKER_RUN) ./hack/smoke.sh

clean:
	$(DOCKER_RUN) rm -rf bin cover.out cover.html

# --- e2e (operator/test/e2e; see that package's doc.go) ---------------------

e2e-tools:
	$(DOCKER_RUN) ./hack/fetch-e2e-tools.sh $(KIND_VERSION) $(KUBECTL_VERSION) $(HELM_VERSION) $(E2E_BIN_TARGET_DIR)

e2e-up: e2e-tools
	E2E_CLUSTER=$(E2E_CLUSTER) E2E_CNI=$(E2E_CNI) KIND_NODE_IMAGE=$(KIND_NODE_IMAGE) \
	MINIO_IMAGE=$(MINIO_IMAGE) REGISTRY_IMAGE=$(REGISTRY_IMAGE) E2E_KUBECONFIG=$(E2E_KUBECONFIG_TARGET) \
	E2E_INTERNAL_KUBECONFIG=$(E2E_INTERNAL_KUBECONFIG) \
	PATH="$(E2E_BIN_TARGET_DIR):$$PATH" \
	$(E2E_TOOLBOX_RUN) ./hack/e2e-up.sh

# `go run`, NOT `go install`, so the ginkgo CLI resolves through the pinned
# module graph -- exactly like this repo's README already documents for
# controller-gen/golangci-lint.
#
# `cd test/e2e &&` and the explicit E2E_*/KUBECONFIG env prefix are both
# REQUIRED, not redundant, under IN_CONTAINER=1: E2E_GO_RUN is empty there
# (no docker wrapping), so nothing else supplies test/e2e as the working
# directory or maps the Makefile's E2E_KUBECONFIG variable onto the
# KUBECONFIG env var client-go actually reads (os.Getenv("KUBECONFIG") in
# harness.go). Under the non-IN_CONTAINER path this is harmless
# duplication: `cd` only affects the host shell before docker is invoked
# (E2E_GO_RUN's own -w already sets the container's cwd), and the env
# prefix only affects the host shell's environment, which docker run does
# not inherit without an explicit -e (already present on E2E_GO_RUN).
e2e-run:
	cd test/e2e && \
	KUBECONFIG=$(E2E_KUBECONFIG_TARGET) E2E_ARTIFACT_DIR=$(E2E_ARTIFACT_TARGET_DIR) \
	E2E_HOST=$(E2E_HOST) E2E_MINIO_PORT=$(E2E_MINIO_PORT) E2E_BROKER_PORT=$(E2E_BROKER_PORT) E2E_MODEL_PORT=$(E2E_MODEL_PORT) E2E_S3PROXY_PORT=$(E2E_S3PROXY_PORT) \
	E2E_OPERATOR_NS=$(E2E_OPERATOR_NS) E2E_SERVICES_NS=$(E2E_SERVICES_NS) E2E_AGENT_IMAGE=$(E2E_AGENT_IMAGE) \
	E2E_BUCKET=$(E2E_BUCKET) E2E_FIXTURE_BUCKET=$(E2E_FIXTURE_BUCKET) E2E_BROKER_TOKEN=$(E2E_BROKER_TOKEN) \
	E2E_PHASE_TIMEOUT=$(E2E_PHASE_TIMEOUT) E2E_POD_TIMEOUT=$(E2E_POD_TIMEOUT) E2E_POLL=$(E2E_POLL) \
	$(E2E_GO_RUN) go run github.com/onsi/ginkgo/v2/ginkgo $(GINKGO_FLAGS) .

e2e-down:
	E2E_CLUSTER=$(E2E_CLUSTER) E2E_KUBECONFIG=$(E2E_KUBECONFIG_TARGET) \
	PATH="$(E2E_BIN_TARGET_DIR):$$PATH" \
	$(E2E_TOOLBOX_RUN) ./hack/e2e-down.sh

e2e-collect:
	E2E_CLUSTER=$(E2E_CLUSTER) E2E_ARTIFACT_DIR=$(E2E_ARTIFACT_TARGET_DIR) \
	E2E_INTERNAL_KUBECONFIG=$(E2E_INTERNAL_KUBECONFIG) \
	PATH="$(E2E_BIN_TARGET_DIR):$$PATH" \
	$(E2E_TOOLBOX_RUN) ./hack/e2e-collect.sh

# up -> run -> down, tearing down even on a test failure and propagating its
# exit code. E2E_KEEP=1 skips the final teardown, for debugging.
e2e: e2e-up
	@rc=0; \
	$(MAKE) IN_CONTAINER=$(IN_CONTAINER) e2e-run || rc=$$?; \
	if [ "$(E2E_KEEP)" != "1" ]; then \
		$(MAKE) IN_CONTAINER=$(IN_CONTAINER) e2e-down; \
	else \
		echo "E2E_KEEP=1: leaving cluster $(E2E_CLUSTER) up for debugging"; \
	fi; \
	exit $$rc

e2e-tidy:
	$(DOCKER_RUN) sh -c 'cd test/e2e && $(if $(TOOLS_GOPROXY),GOPROXY="$(TOOLS_GOPROXY)",) go mod tidy'
	$(DOCKER_RUN) sh -c 'cd test/e2e/doubles && GOPROXY=off go mod tidy'

e2e-vet:
	$(DOCKER_RUN) sh -c 'cd test/e2e && $(if $(TOOLS_GOPROXY),GOPROXY="$(TOOLS_GOPROXY)",) go vet ./...'
	$(DOCKER_RUN) sh -c 'cd test/e2e/doubles && GOPROXY=off go vet ./...'

# --- storage-conformance (internal/storage's s3 leg against a real MinIO) --

minio-up:
	$(DOCKER) network create $(MINIO_TEST_NETWORK) >/dev/null 2>&1 || true
	$(DOCKER) rm -f $(MINIO_TEST_CONTAINER) >/dev/null 2>&1 || true
	$(DOCKER) run -d --name $(MINIO_TEST_CONTAINER) --network $(MINIO_TEST_NETWORK) \
		-e MINIO_ROOT_USER=$(MINIO_TEST_USER) -e MINIO_ROOT_PASSWORD=$(MINIO_TEST_PASSWORD) \
		$(MINIO_IMAGE) server /data
	@echo "waiting for $(MINIO_TEST_CONTAINER) to become healthy..."
	$(DOCKER) run --rm --network $(MINIO_TEST_NETWORK) curlimages/curl:8.11.1 sh -c \
		'for i in $$(seq 1 30); do curl -sf http://$(MINIO_TEST_CONTAINER):9000/minio/health/live && exit 0; sleep 1; done; echo "MinIO did not become healthy in time" >&2; exit 1'

minio-down:
	$(DOCKER) rm -f $(MINIO_TEST_CONTAINER) >/dev/null 2>&1 || true
	$(DOCKER) network rm $(MINIO_TEST_NETWORK) >/dev/null 2>&1 || true

# --- Helm chart (operator/deploy/helm/ai-sandbox-operator; see #34) --------

HELM_IMAGE     ?= alpine/helm:3.16.4
HELM_CHART_DIR ?= deploy/helm/ai-sandbox-operator
HELM_RUN       := $(if $(filter 1,$(IN_CONTAINER)),,$(DOCKER) run --rm -u $(shell id -u):$(shell id -g) \
	-v $(SHARED_DIR):/work -w /work/ai-sandbox/operator $(HELM_IMAGE))

# helm-crds copies config/crd/bases/*.yaml -- the controller-gen-generated
# source of truth -- verbatim into the chart's crds/ directory. Helm 3
# installs everything under crds/ automatically on `helm install` and never
# upgrades or deletes it (see
# https://helm.sh/docs/chart_best_practices/custom_resource_definitions/), so
# the chart carries its own copy rather than templating the CRDs; this
# target is how that copy is refreshed after any `make manifests` run.
helm-crds:
	cp $(CURDIR)/config/crd/bases/*.yaml $(CURDIR)/$(HELM_CHART_DIR)/crds/

# helm-crds-check fails if the chart's crds/ copy has drifted from
# config/crd/bases -- the CI-asserted guarantee that the chart can never ship
# CRDs the Go types disagree with.
helm-crds-check:
	@rc=0; \
	for f in $(CURDIR)/config/crd/bases/*.yaml; do \
		b=$$(basename $$f); \
		if ! diff -u "$$f" "$(CURDIR)/$(HELM_CHART_DIR)/crds/$$b" >/dev/null 2>&1; then \
			echo "helm-crds-check: $(HELM_CHART_DIR)/crds/$$b has drifted from config/crd/bases/$$b -- run 'make helm-crds'" >&2; \
			diff -u "$(CURDIR)/$(HELM_CHART_DIR)/crds/$$b" "$$f" >&2 || true; \
			rc=1; \
		fi; \
	done; \
	exit $$rc

# helm-rbac-check fails if the chart's ClusterRole rules have drifted from
# config/rbac/role.yaml (also controller-gen-generated, from the same
# +kubebuilder:rbac markers). Both files are static YAML (no Helm templating
# inside the `rules:` block itself), so a plain `rules:`-to-end-of-file
# extraction is enough -- deliberately awk/sed, not a YAML-aware tool, to
# keep this dependency-free inside the golang:1.25 container this runs in.
helm-rbac-check:
	@src=$$(mktemp); dst=$$(mktemp); \
	awk '/^rules:/{f=1} f && !/^{{/{print}' $(CURDIR)/config/rbac/role.yaml > $$src; \
	awk '/^rules:/{f=1} f && !/^{{/{print}' $(CURDIR)/$(HELM_CHART_DIR)/templates/clusterrole.yaml > $$dst; \
	if ! diff -u $$src $$dst; then \
		echo "helm-rbac-check: $(HELM_CHART_DIR)/templates/clusterrole.yaml's rules: block has drifted from config/rbac/role.yaml (the source of truth generated by 'make manifests')" >&2; \
		rm -f $$src $$dst; exit 1; \
	fi; \
	rm -f $$src $$dst

helm-lint:
	$(HELM_RUN) lint --strict $(HELM_CHART_DIR)

helm-template:
	$(HELM_RUN) template op $(HELM_CHART_DIR) --include-crds

# helm-kind runs hack/helm-kind-walkthrough.sh -- the install/upgrade/
# uninstall/reinstall assertions -- against a cluster hack/e2e-up.sh already
# brought up with E2E_DEPLOY=helm (see the kind-install-upgrade job in
# .github/workflows/helm.yml). Unlike e2e-up/e2e-down/e2e-collect above, the
# walkthrough script also needs a Go toolchain (it runs the existing Ginkgo
# lifecycle spec via `go run .../ginkgo --focus=...`) alongside
# kubectl/helm, so it is wrapped the same way as those (E2E_TOOLBOX_RUN under
# local/sandbox use) but is only fully supported IN_CONTAINER=1 (a CI runner,
# or any shell with go/kubectl/helm/kind already on PATH) -- docker:28-cli
# has no Go. Not required to pass in this sandbox; see hack/
# helm-kind-walkthrough.sh's own header comment.
helm-kind:
	E2E_CLUSTER=$(E2E_CLUSTER) E2E_KUBECONFIG=$(E2E_KUBECONFIG_TARGET) \
	E2E_ARTIFACT_DIR=$(E2E_ARTIFACT_TARGET_DIR) \
	PATH="$(E2E_BIN_TARGET_DIR):$$PATH" \
	$(E2E_TOOLBOX_RUN) ./hack/helm-kind-walkthrough.sh

# Runs internal/storage's tests (both the fs leg, always, and the s3 leg,
# provided SANDBOX_TEST_MINIO_ENDPOINT is reachable -- see MINIO_TEST_RUN)
# without managing the MinIO container's lifecycle; pair with a prior
# `make minio-up`, or use `make storage-conformance` for the full cycle.
test-minio:
	$(MINIO_TEST_RUN) sh -c 'CGO_ENABLED=1 go test -race -count=1 ./internal/storage/...'

# up -> test -> down, tearing down even on a test failure and propagating
# its exit code -- same shape as the `e2e` target above.
storage-conformance: minio-up
	@rc=0; \
	$(MAKE) IN_CONTAINER=$(IN_CONTAINER) test-minio || rc=$$?; \
	$(MAKE) IN_CONTAINER=$(IN_CONTAINER) minio-down; \
	exit $$rc

# quickstart-check runs hack/quickstart-check.sh, which EXTRACTS the
# `sh quickstart`-tagged fenced code blocks from operator/README.md and
# executes them verbatim against a real kind cluster -- #35's "the
# quickstart is executed in CI as a documentation test, so it cannot rot
# silently". There is no second copy of the commands to keep in sync: the
# doc IS the script.
#
# Like helm-kind above, this is only fully supported IN_CONTAINER=1 (a CI
# runner, or any shell with docker/kind/kubectl/helm on PATH) --
# docker:28-cli under E2E_TOOLBOX_RUN cannot address a kind API server the
# way this sandbox's nested DinD requires. Not required to pass locally.
quickstart-check:
	E2E_ARTIFACT_DIR=$(E2E_ARTIFACT_TARGET_DIR) \
	PATH="$(E2E_BIN_TARGET_DIR):$$PATH" \
	$(E2E_TOOLBOX_RUN) ./hack/quickstart-check.sh
