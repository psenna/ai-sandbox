#!/usr/bin/env bash
# Claude Code agent entrypoint for the ai-sandbox stack.
#
# 1. Route every github.com clone/fetch/push through git-proxy and attach the
#    agent Bearer. Claude Code shells out to `git`, so `insteadOf` + `extraHeader`
#    catch every git operation transparently — no need to teach the model a wrapper.
# 2. Drop the use-git-proxy / use-docker / use-dependaproxy skills and the
#    always-loaded CLAUDE.md into /workspace so the agent knows its two execution
#    surfaces (git via the proxy, code via the rootless DinD daemon), how to get
#    dependencies (only via DependaProxy), and the no-direct-GitHub / no-PAT
#    rules.
# 3. Hand control to the compose `command` / `docker compose exec` args. With no
#    args (the default CMD is `bash`) the container idles so the operator can
#    `docker compose exec claude claude` for an interactive session, or
#    `docker compose exec claude claude -p "<prompt>"` for headless.
set -eu

: "${AGENT_TOKEN:?AGENT_TOKEN must be set (see .env)}"

# GITHUB_REPO is optional and unused here: nothing in this script clones a
# repository -- whoever drives the agent runs the first `git clone` in the
# terminal, through the git-proxy rewrite configured below. When it IS set
# (the compose stack and the Kubernetes operator both set it) it rides in the
# environment as a hint of which repo the agent is here to work on; the
# docker-operator populates it per agent.
: "${GITHUB_REPO:=}"

# Rewrite https://github.com/<anything> -> http://git-proxy:8080/<anything> so all
# git traffic flows through the proxy.
git config --global url."http://git-proxy:8080/".insteadOf "https://github.com/"
# Attach the agent Bearer to every request to the proxy host. (The proxy is plain
# HTTP on the compose network, so no TLS/sslVerify concerns.)
git config --global http."http://git-proxy:8080/".extraHeader "Authorization: Bearer ${AGENT_TOKEN}"

# Make the use-git-proxy skill available to this project workspace. Claude Code
# auto-loads skills from .claude/skills/ in the project (cwd) directory.
mkdir -p /workspace/.claude/skills/use-git-proxy
cp /opt/skills/use-git-proxy/SKILL.md /workspace/.claude/skills/use-git-proxy/SKILL.md

# Make the agent aware of its rootless DinD environment. CLAUDE.md is always
# loaded by Claude Code (project root); the use-docker skill is on-demand.
cp /opt/agent-context/CLAUDE.md /workspace/CLAUDE.md
mkdir -p /workspace/.claude/skills/use-docker
cp /opt/skills/use-docker/SKILL.md /workspace/.claude/skills/use-docker/SKILL.md

# use-dependaproxy: installing language deps (incl. a committed lockfile) when
# the only route to a registry is the DependaProxy instance -- the validation
# gates, per-ecosystem client config, and the reversible /pypi/upstream/ lock
# rewrite for `uv sync --frozen`. On-demand.
mkdir -p /workspace/.claude/skills/use-dependaproxy
cp /opt/skills/use-dependaproxy/SKILL.md /workspace/.claude/skills/use-dependaproxy/SKILL.md

# use-sandbox / freeze / unfreeze are Kubernetes-operator-only (the sidecar
# control API and its snapshot-based freeze/wake). The docker-operator agent
# image (docker-operator/agent/Dockerfile) does not bake them in, so each is
# copied only if actually present -- guarding this loop is what lets a
# trimmed image still boot instead of dying in entrypoint.sh before exec
# "$@" runs. In the root image all three files exist, so this changes
# nothing there.

# use-sandbox: the ai-sandbox operator's sidecar control API (declare a
# wait, report a result, leave a progress breadcrumb) -- only relevant when
# this container is running inside an operator-managed SandboxEnvironment
# pod, but harmless to always drop in (on-demand skill, only loaded when
# needed).
if [ -f /opt/skills/use-sandbox/SKILL.md ]; then
  mkdir -p /workspace/.claude/skills/use-sandbox
  cp /opt/skills/use-sandbox/SKILL.md /workspace/.claude/skills/use-sandbox/SKILL.md
fi

# freeze: what actually happens when the agent declares a wait (snapshot,
# teardown, slot release) and what to check on resume -- same on-demand,
# harmless-if-irrelevant rationale as use-sandbox above.
if [ -f /opt/skills/freeze/SKILL.md ]; then
  mkdir -p /workspace/.claude/skills/freeze
  cp /opt/skills/freeze/SKILL.md /workspace/.claude/skills/freeze/SKILL.md
fi

# unfreeze: the counterpart to freeze -- what a wake restored, what it did
# not (cold image cache, containers gone), and what to re-establish, in
# order. Only relevant to a pod that was woken, so equally on-demand.
if [ -f /opt/skills/unfreeze/SKILL.md ]; then
  mkdir -p /workspace/.claude/skills/unfreeze
  cp /opt/skills/unfreeze/SKILL.md /workspace/.claude/skills/unfreeze/SKILL.md
fi

# implement-issue: drive a GitHub issue from spec to merged PR with a tiered
# model pipeline (Opus plans, Sonnet implements, Opus validates & fixes).
mkdir -p /workspace/.claude/skills/implement-issue
cp /opt/skills/implement-issue/SKILL.md /workspace/.claude/skills/implement-issue/SKILL.md

# Route ALL npm/pip/go through DependaProxy: write the config files from env.
# The use-docker skill tells the agent to pass these into DinD workload
# containers. DependaProxy auth is DISABLED in this stack (see dependaproxy.yaml:
# Go's module client refuses credentials over plaintext HTTP), so the configs
# carry no token.
#   - /home/node/.npmrc + /workspace/.npmrc : npm registry.
#   - /workspace/pip.env                    : pip (PIP_INDEX_URL + PIP_TRUSTED_HOST;
#     pip 26.x ignores bind-mounted pip.conf files, so we use env vars).
#   - /workspace/go.env                     : go (GOPROXY; no userinfo — go refuses
#     to send credentials over HTTP).
#   - /workspace/dependaproxy-ip            : the dinernet-side IP of the
#     DependaProxy container, spliced into --add-host by workload containers
#     (the nested daemon has no DNS for sibling names). See use-docker.
: "${DEPENDAPROXY_URL:=http://dependaproxy:8080/npm}"
: "${DEPENDAPROXY_PYPI_URL:=http://dependaproxy:8080/pypi}"
: "${DEPENDAPROXY_GOPROXY_URL:=http://dependaproxy:8080/goproxy}"
# The DinD daemon cannot resolve the compose/service name "dependaproxy", so
# every workload container needs --add-host=dependaproxy:<ip>. In the root
# compose stack that IP is pinned statically in docker-compose.yaml. The
# docker-operator instead gives each agent a PRIVATE dinernet and connects the
# shared dependaproxy container to it at create time, so the address is
# per-agent and is injected here as DEPENDAPROXY_DINERNET_IP. Unset (the
# compose case) falls back to the static address, exactly like the three URLs
# above.
: "${DEPENDAPROXY_DINERNET_IP:=172.23.0.10}"

# --- npm ---
printf 'registry=%s\n' "$DEPENDAPROXY_URL" > /home/node/.npmrc
cp /home/node/.npmrc /workspace/.npmrc

# --- pip ---
# PIP_TRUSTED_HOST is required for the plain-HTTP index URL.
printf 'PIP_INDEX_URL=%s/simple\n' "$DEPENDAPROXY_PYPI_URL" > /workspace/pip.env
printf 'PIP_TRUSTED_HOST=dependaproxy\n' >> /workspace/pip.env

# --- go ---
# Go still verifies module checksums against sum.golang.org directly (that host
# is intentionally not blocked by dind-init.sh).
printf 'GOPROXY=%s\n' "$DEPENDAPROXY_GOPROXY_URL" > /workspace/go.env

# --- dependaproxy host IP ---
# A bare IP plus a newline, so the use-docker skill can splice it straight into
# a run: --add-host="dependaproxy:$(cat /workspace/dependaproxy-ip)".
printf '%s\n' "$DEPENDAPROXY_DINERNET_IP" > /workspace/dependaproxy-ip

# The compose `command` / `docker compose exec` args become $@. With no args the
# default CMD (`bash`) keeps the container alive for interactive `docker compose
# exec claude claude`. Pass `claude -p "<prompt>"` for a headless run.
exec "$@"