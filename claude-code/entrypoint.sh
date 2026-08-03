#!/usr/bin/env bash
# Claude Code agent entrypoint for the ai-sandbox stack.
#
# 1. Route every github.com clone/fetch/push through git-proxy and attach the
#    agent Bearer. Claude Code shells out to `git`, so `insteadOf` + `extraHeader`
#    catch every git operation transparently — no need to teach the model a wrapper.
# 2. Drop the use-git-proxy + use-docker skills and the always-loaded CLAUDE.md
#    into /workspace so the agent knows its two execution surfaces (git via the
#    proxy, code via the rootless DinD daemon) and the no-direct-GitHub / no-PAT
#    rules.
# 3. Hand control to the compose `command` / `docker compose exec` args. With no
#    args (the default CMD is `bash`) the container idles so the operator can
#    `docker compose exec claude claude` for an interactive session, or
#    `docker compose exec claude claude -p "<prompt>"` for headless.
set -eu

: "${AGENT_TOKEN:?AGENT_TOKEN must be set (see .env)}"
: "${GITHUB_REPO:?GITHUB_REPO must be set (see .env)}"

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

# Route ALL npm through DependaProxy: write .npmrc from env (the token is never
# baked into the image — it comes from the compose environment at runtime).
#   - /home/node/.npmrc : the claude container's OWN npm (claude-code updates,
#     plugin installs, any direct npm the agent runs).
#   - /workspace/.npmrc: the shared-volume copy the use-docker skill tells the
#     agent to mount into DinD workload containers, run as the `node` user
#     (-v /workspace/.npmrc:/home/node/.npmrc:ro -u node).
# npm sends _authToken as `Authorization: Bearer <token>`, exactly DependaProxy's
# auth scheme.
: "${DEPENDAPROXY_URL:=http://dependaproxy:8080/npm}"
printf 'registry=%s\n' "$DEPENDAPROXY_URL" > /home/node/.npmrc
if [ -n "${DEPENDAPROXY_TOKEN:-}" ]; then
  # The auth entry is keyed on the registry HOST (npm matches by prefix).
  printf '//dependaproxy:8080/:_authToken=%s\n' "$DEPENDAPROXY_TOKEN" >> /home/node/.npmrc
else
  echo "entrypoint: WARNING DEPENDAPROXY_TOKEN is unset — npm installs against DependaProxy will get 401 (fill it in .env and set auth.token in dependaproxy.yaml)" >&2
fi
cp /home/node/.npmrc /workspace/.npmrc

# The compose `command` / `docker compose exec` args become $@. With no args the
# default CMD (`bash`) keeps the container alive for interactive `docker compose
# exec claude claude`. Pass `claude -p "<prompt>"` for a headless run.
exec "$@"