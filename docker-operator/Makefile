# ai-sandbox docker-operator Makefile.
#
# The host/agent has no Go toolchain, so every Go command runs inside a
# disposable golang container against the rootless DinD daemon
# (DOCKER_HOST=tcp://docker:2375). The daemon can only see /workspace, so we
# mount that and set the workdir to this directory's path inside it.
#
# IN_CONTAINER=1 makes DOCKER_RUN empty and runs the go commands directly --
# used by GitHub Actions and when make itself is already running inside the
# golang container (the agent container has no `make`).
#
# This is the operator/Makefile build pattern trimmed to the essentials; the
# e2e/kind/helm/envtest/controller-gen machinery there has no analog here.

GOLANG_IMAGE   ?= golang:1.25
NODE_IMAGE     ?= node:22
DOCKER         ?= docker
IN_CONTAINER   ?= 0

SHARED_DIR      ?= /workspace
# Deliberately distinct from the operator's .gocache-operator: both modules
# share SHARED_DIR, and a shared GOMODCACHE/GOCACHE between two independent
# modules only invites cross-contamination.
CACHE_SUBDIR    ?= .gocache-docker-operator
DEPENDAPROXY_IP ?= 172.23.0.10
GO_ENV_FILE     ?= $(SHARED_DIR)/go.env
SKILL_FORK      ?= agent/skills/use-docker/SKILL.md

GOLANGCI_LINT_VERSION ?= v2.12.2
GOVULNCHECK_VERSION   ?= v1.6.0
AGENT_IMAGE           ?= ghcr.io/psenna/ai-sandbox-docker-operator-agent:dev

REPO_SUBDIR := $(patsubst $(SHARED_DIR)/%,%,$(CURDIR))
CACHE_DIR   := /work/$(CACHE_SUBDIR)
GO_ENV_FLAG := $(if $(wildcard $(GO_ENV_FILE)),--env-file $(GO_ENV_FILE),)

ifeq ($(IN_CONTAINER),1)
DOCKER_RUN :=
NODE_RUN   :=
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
# web-test needs neither GOPROXY/DependaProxy routing nor a persistent
# module cache -- render.test.js uses only Node's own built-in test runner,
# no npm install at all -- so this wrapper is deliberately smaller than
# DOCKER_RUN.
NODE_RUN := $(DOCKER) run --rm -u $(shell id -u):$(shell id -g) \
	-v $(SHARED_DIR):/work -w /work/$(REPO_SUBDIR) \
	$(NODE_IMAGE)
endif

.PHONY: all build test vet fmt fmt-check lint vuln tidy skill-check dind-init-check agent-image agent-image-smoke web-test

all: vet fmt-check lint test skill-check dind-init-check web-test

build:
	$(DOCKER_RUN) go build -trimpath -o bin/docker-operator ./cmd/docker-operator

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

# The agent's use-docker skill is a deliberate FORK of
# claude-code/use-docker/SKILL.md: every agent gets a PRIVATE dinernet, so the
# compose stack's static dependaproxy address is meaningless here and the
# examples must read /workspace/dependaproxy-ip (written by entrypoint.sh from
# DEPENDAPROXY_DINERNET_IP) instead. Guard against a copy-paste refresh from
# upstream silently reintroducing the literal -- it would fail as a connect
# timeout at runtime, not an error.
skill-check:
	@test -f $(SKILL_FORK) || { echo "$(SKILL_FORK): missing"; exit 1; }
	@if grep -qF '172.23.0.10' $(SKILL_FORK); then \
		echo "$(SKILL_FORK): hardcoded dependaproxy IP 172.23.0.10 -- read /workspace/dependaproxy-ip instead"; \
		exit 1; \
	fi
	@echo "skill-check: OK -- no hardcoded dependaproxy IP in $(SKILL_FORK)"

# The agent image needs repo-root context (it COPYs claude-code/*/SKILL.md and
# docker-operator/agent/skills/use-docker/SKILL.md alike), so CURDIR (this
# directory) is wrong -- build from the parent, same as docker-compose.yaml's
# own `context: .` for the root image.
agent-image:
	$(DOCKER) build -f agent/Dockerfile -t $(AGENT_IMAGE) ..

# Verifies the built image's CONTENTS, not entrypoint.sh's runtime behaviour --
# --entrypoint bypasses it deliberately, since entrypoint.sh requires
# AGENT_TOKEN/GITHUB_REPO and a writable /workspace, neither of which this
# check is about.
agent-image-smoke:
	@echo "--- tmux is installed ---"
	docker run --rm --entrypoint tmux $(AGENT_IMAGE) -V
	@echo "--- /opt/skills holds exactly the 4 expected skills ---"
	@actual=$$(docker run --rm --entrypoint sh $(AGENT_IMAGE) -c 'ls -1 /opt/skills' | sort); \
	expected=$$(printf 'implement-issue\nuse-dependaproxy\nuse-docker\nuse-git-proxy'); \
	if [ "$$actual" != "$$expected" ]; then \
		echo "FAIL: /opt/skills mismatch"; \
		printf 'want:\n%s\ngot:\n%s\n' "$$expected" "$$actual"; \
		exit 1; \
	fi; \
	echo "OK: $$actual" | tr '\n' ' '; echo
	@echo "--- use-docker is the local fork, not the shared copy ---"
	@if docker run --rm --entrypoint sh $(AGENT_IMAGE) -c 'grep -qF 172.23.0.10 /opt/skills/use-docker/SKILL.md'; then \
		echo "FAIL: shared use-docker skill baked in, not the fork"; exit 1; \
	fi
	@echo "OK: use-docker is the fork"
	@echo "--- tmux-boot.sh is present and executable ---"
	docker run --rm --entrypoint sh $(AGENT_IMAGE) -c 'test -x /usr/local/bin/tmux-boot.sh'
	@echo "OK: /usr/local/bin/tmux-boot.sh"
	@echo "agent-image-smoke: PASS"

# scripts/dind-init.sh is the SINGLE source of truth; internal/agent embeds a
# copy because go:embed cannot reach outside its own package directory (and
# refuses symlinks). Fail loudly rather than let the two drift.
dind-init-check:
	@cmp -s ../scripts/dind-init.sh internal/agent/dind-init.sh || { \
		echo "internal/agent/dind-init.sh has drifted from ../scripts/dind-init.sh"; \
		diff -u ../scripts/dind-init.sh internal/agent/dind-init.sh || true; exit 1; }
	@echo "dind-init-check: OK"

# render.js is deliberately document/window-free (see its own doc comment),
# so its tests run directly under Node's built-in test runner -- no jsdom,
# no headless browser, no npm install at all. IN_CONTAINER=1 (CI) runs node
# directly, since ubuntu-latest already ships one; NODE_RUN wraps it locally.
web-test:
	$(NODE_RUN) node --test web/render.test.js
