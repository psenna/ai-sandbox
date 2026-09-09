# syntax=docker/dockerfile:1
#
# The agent image for the ai-sandbox stack: Claude Code (the official native
# binary, installed with Anthropic's installer — the npm route is deprecated)
# with git so it can clone/fetch/push through git-proxy. Backed by Ollama at
# runtime (configured via env in docker-compose.yaml, not baked in — no secrets in
# the image).
#
# DELIBERATELY SLIM — no python, no go, no yarn, no dev node tooling, no in-image
# ollama. The agent runs dev workloads (node/python/go runs, services) inside the
# rootless DinD daemon (see use-docker/SKILL.md), never directly on this container.
# The node base is kept for its uid-1000 `node` user (HOME=/home/node) layout;
# Claude Code itself is the native binary and no longer needs Node.

FROM node:22-alpine

# git: clone/fetch/push (routed through git-proxy by the entrypoint).
# ca-certificates: HTTPS to github.com, ollama.com, and downloads.claude.ai
# (the Claude Code installer, at build time).
# bash: entrypoint + the Claude Code installer. curl: the broker leg (the
# use-git-proxy skill drives it with curl) + the installer download.
# libgcc/libstdc++/ripgrep: runtime deps of the native binary on musl (docs'
# Alpine section); USE_BUILTIN_RIPGREP=0 below selects the system ripgrep.
# docker-cli: the client only (no daemon) — talks to the rootless DinD daemon in
# the separate `docker` service via DOCKER_HOST=tcp://docker:2375.
RUN apk add --no-cache git ca-certificates bash curl libgcc libstdc++ ripgrep docker-cli && \
    rm -rf /var/cache/apk/*

# Non-root runtime: node:22-alpine already ships a `node` user with uid 1000
# (HOME=/home/node), matching git-proxy's uid 1000 so any shared volume is
# read/writable by both. We reuse it instead of creating one — `adduser -D -u 1000`
# would collide with that built-in user. (USER node is set at the end so the
# COPY/chmod steps still run as root.)

# Bake the skills + the always-loaded CLAUDE.md; the entrypoint copies them into
# /workspace/.claude/skills/ and /workspace/CLAUDE.md at startup.
COPY claude-code/use-git-proxy/SKILL.md /opt/skills/use-git-proxy/SKILL.md
COPY claude-code/use-docker/SKILL.md    /opt/skills/use-docker/SKILL.md
COPY claude-code/use-dependaproxy/SKILL.md /opt/skills/use-dependaproxy/SKILL.md
COPY claude-code/use-sandbox/SKILL.md   /opt/skills/use-sandbox/SKILL.md
COPY claude-code/freeze/SKILL.md        /opt/skills/freeze/SKILL.md
COPY claude-code/unfreeze/SKILL.md      /opt/skills/unfreeze/SKILL.md
COPY claude-code/implement-issue/SKILL.md /opt/skills/implement-issue/SKILL.md
COPY claude-code/agent-context/CLAUDE.md /opt/agent-context/CLAUDE.md

COPY claude-code/entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

# Pre-create /workspace as node BEFORE switching USER: WORKDIR creates a missing
# directory as root regardless of the active USER (a known Docker/BuildKit quirk
# -- the operator image, docker-operator/agent/Dockerfile, carries the same fix).
# Without it a FRESH workspace volume is seeded root-owned and entrypoint.sh
# (running as node) dies at its first `mkdir -p /workspace/.claude/skills/...`
# with EACCES on every agent's first boot.
RUN mkdir -p /workspace && chown node:node /workspace

USER node
# Claude Code: install it the official way (the npm route is deprecated) —
# Anthropic's installer fetches the native binary and sets up the launcher.
# Run as `node` so everything lands under /home/node (the .local/bin/claude
# launcher + .local/share/claude/versions/) and is usable by the runtime user.
# Download-then-run instead of `curl | bash`: a failed download must fail the
# build, not silently pipe EOF into bash. To pin a version, pass it as the
# script's argument (e.g. `bash /tmp/claude-install.sh 1.2.3`); default is latest.
RUN curl -fsSL https://claude.ai/install.sh -o /tmp/claude-install.sh && \
    bash /tmp/claude-install.sh && \
    rm -f /tmp/claude-install.sh
# ~/.local/bin is the installer's launcher dir and is not on the base PATH.
ENV PATH="/home/node/.local/bin:${PATH}"
# The npm package was frozen at build time; keep that property. The native
# installer auto-updates in the background by default — for a distributed image,
# rebuilding IS the upgrade path, so the runtime must not swap versions itself.
ENV DISABLE_AUTOUPDATER=1
# musl: use the system ripgrep (installed above) instead of the bundled one.
ENV USE_BUILTIN_RIPGREP=0
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