# syntax=docker/dockerfile:1
#
# The agent image for the ai-sandbox stack: Claude Code (the official npm CLI) on
# Node, with git so it can clone/fetch/push through git-proxy. Backed by Ollama at
# runtime (configured via env in docker-compose.yaml, not baked in — no secrets in
# the image).
#
# DELIBERATELY SLIM — no python, no go, no yarn, no dev node tooling, no in-image
# ollama. The agent runs dev workloads (node/python/go runs, services) inside the
# rootless DinD daemon (see use-docker/SKILL.md), never directly on this container.
# Node remains only as Claude Code's own runtime.

FROM node:22-alpine

# git: clone/fetch/push (routed through git-proxy by the entrypoint).
# ca-certificates: HTTPS to github.com and ollama.com.
# bash: entrypoint. curl: the broker leg (the use-git-proxy skill drives it with curl).
# docker-cli: the client only (no daemon) — talks to the rootless DinD daemon in
# the separate `docker` service via DOCKER_HOST=tcp://docker:2375.
RUN apk add --no-cache git ca-certificates bash curl docker-cli && rm -rf /var/cache/apk/* && \
    npm install -g @anthropic-ai/claude-code

# Non-root runtime: node:22-alpine already ships a `node` user with uid 1000
# (HOME=/home/node), matching git-proxy's uid 1000 so any shared volume is
# read/writable by both. We reuse it instead of creating one — `adduser -D -u 1000`
# would collide with that built-in user. (USER node is set at the end so the
# COPY/chmod steps still run as root.)

# Bake the skills + the always-loaded CLAUDE.md; the entrypoint copies them into
# /workspace/.claude/skills/ and /workspace/CLAUDE.md at startup.
COPY claude-code/use-git-proxy/SKILL.md /opt/skills/use-git-proxy/SKILL.md
COPY claude-code/use-docker/SKILL.md    /opt/skills/use-docker/SKILL.md
COPY claude-code/implement-issue/SKILL.md /opt/skills/implement-issue/SKILL.md
COPY claude-code/agent-context/CLAUDE.md /opt/agent-context/CLAUDE.md

COPY claude-code/entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

USER node
# Create CLAUDE_CONFIG_DIR as the node user. The claude-config named volume is
# mounted here at runtime; when a fresh empty volume is mounted at a path that
# already exists in the image, Docker seeds the volume with the image dir's
# ownership. Without this the volume is root-owned and Claude Code can't write
# its session dir (the Bash tool fails with EACCES on .../session-env). The
# existing root-owned volume must be recreated once after this change — see
# the recovery note in the README / commit message.
RUN mkdir -p /home/node/.claude-sandbox
WORKDIR /workspace
ENTRYPOINT ["/entrypoint.sh"]
# Default: idle bash so the operator can `docker compose exec claude claude`.
CMD ["bash"]