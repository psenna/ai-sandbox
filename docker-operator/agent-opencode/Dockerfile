# syntax=docker/dockerfile:1
#
# The docker-operator's OPENCODE agent image -- the opencode-harness sibling of
# docker-operator/agent/Dockerfile (Claude Code). Separate image per harness
# (#176/#177): the two share a base, the skills and the entrypoint, but not the
# CLI, its config dir, or its plugin system, so merging them would mean one
# image carrying two harnesses' installers and two sets of config.
#
# BUILT WITH REPO-ROOT CONTEXT, exactly like the Claude image:
#
#   docker build -f docker-operator/agent-opencode/Dockerfile \
#     -t ghcr.io/psenna/ai-sandbox-agent-opencode:dev .

ARG NPM_REGISTRY=https://registry.npmjs.org
ARG GOPROXY=https://proxy.golang.org,direct

# gopls, built in its own stage so the final image doesn't carry a Go
# toolchain -- same rationale as docker-operator/agent/Dockerfile: only the
# compiled binary (COPY --from, below) ends up in the final image.
FROM golang:1.26-alpine AS gopls-builder
ARG GOPROXY
ENV GOPROXY=${GOPROXY}
RUN go install golang.org/x/tools/gopls@v0.23.0

FROM node:22-alpine
# Re-declared: a build ARG declared before the first FROM is visible to every
# stage's FROM line, but a stage must re-declare it to use its value in a RUN.
ARG NPM_REGISTRY

# git for git-proxy clone/fetch/push, ca-certificates, bash for the entrypoint,
# curl for the broker leg + the opencode installer, libgcc/libstdc++/ripgrep
# for the native binary on musl, docker-cli as a client-only talker to the
# per-agent rootless DinD sidecar, tmux for the operator's detachable session.
# No python3/py3-pip here: that pair exists in the Claude image solely to run
# the baked security-guidance plugin's sg-python.sh hook, and this image
# installs no Claude Code plugins at all (opencode has no such plugin system
# to feed) -- see the "Do NOT install Claude Code plugins" scope note in #177.
RUN apk add --no-cache git ca-certificates bash curl libgcc libstdc++ ripgrep docker-cli tmux && \
    rm -rf /var/cache/apk/*

# gopls: copied from the builder stage above, root-owned like the other
# system binaries (docker-cli, tmux) already on PATH here.
COPY --from=gopls-builder /go/bin/gopls /usr/local/bin/gopls

# typescript-language-server + pyright (which ships `pyright-langserver`
# despite being a Python tool -- its core is a Node/TS implementation, so it
# needs no Python interpreter): installed globally as root, before USER node
# below, since node:22-alpine's default global npm prefix isn't
# node-writable. ARG NPM_REGISTRY as explained above; unset again after so it
# doesn't leak into a plain `docker run` of the image or a later npm use.
RUN npm config set registry "$NPM_REGISTRY" && \
    npm install -g typescript-language-server typescript pyright && \
    npm config delete registry

# A UTF-8 locale so tmux runs in UTF-8 mode -- same rationale as the Claude
# image: musl provides C.UTF-8 with no locale-gen needed.
ENV LANG=C.UTF-8

# Non-root runtime: node:22-alpine already ships a `node` user with uid 1000
# (HOME=/home/node), matching git-proxy's and dependaproxy's uid 1000 so shared
# volumes are read/writable by all of them. Reused, not recreated.

# Bake the skills + the always-loaded CLAUDE.md; the entrypoint copies them into
# /workspace/.claude/skills/ and /workspace/CLAUDE.md at startup. Same five
# skills as the Claude image (entrypoint.sh is shared verbatim, unforked --
# see below -- and these are the files it expects to find under /opt).
COPY docker-operator/agent/skills/use-git-proxy/SKILL.md  /opt/skills/use-git-proxy/SKILL.md
COPY claude-code/use-docker/SKILL.md                      /opt/skills/use-docker/SKILL.md
COPY claude-code/use-dependaproxy/SKILL.md                /opt/skills/use-dependaproxy/SKILL.md
COPY claude-code/implement-issue/SKILL.md                 /opt/skills/implement-issue/SKILL.md
COPY claude-code/store-file/SKILL.md                      /opt/skills/store-file/SKILL.md
COPY claude-code/agent-context/CLAUDE.md                  /opt/agent-context/CLAUDE.md

# entrypoint.sh: reused UNMODIFIED from the Claude image (#177 scope -- do not
# fork it). It is harness-agnostic by construction: the Claude-specific block
# near the end (seeding .claude.json) is guarded on `CLAUDE_CONFIG_DIR` being
# set, which this image deliberately never sets (see below), so that block is
# always skipped here and the script falls straight through to `exec "$@"`.
COPY claude-code/entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

# tmux-boot.sh is shared verbatim with the Claude Code image (docker-operator/agent/Dockerfile)
# -- it just execs a supervisor script inside tmux and is harness-agnostic via
# the AGENT_SUPERVISOR env var, which THIS image sets below.
COPY docker-operator/agent/tmux-boot.sh                       /usr/local/bin/tmux-boot.sh
COPY docker-operator/agent-opencode/opencode-supervisor.sh    /usr/local/bin/opencode-supervisor.sh
RUN chmod +x /usr/local/bin/tmux-boot.sh /usr/local/bin/opencode-supervisor.sh
# tmux-boot.sh defaults to claude-supervisor.sh; this is where THIS image says
# which harness its pane runs. An image-level property, so container spawn
# code (a later issue) needs no extra env var for it.
ENV AGENT_SUPERVISOR=opencode-supervisor.sh

# Pre-create /workspace as node BEFORE switching USER: WORKDIR creates a
# missing directory as root regardless of the active USER, and a fresh
# workspace volume's ownership is inherited from this path via Docker's
# copy-up -- same reasoning as docker-operator/agent/Dockerfile. Without this,
# entrypoint.sh (running as node) fails its first `mkdir -p
# /workspace/.claude/skills/...` with EACCES.
RUN mkdir -p /workspace && chown node:node /workspace

USER node
# opencode: install via the official curl install script (the only supported
# route -- there is no npm package to pin the way the Claude image pins its
# native installer). Run as `node` so it lands under /home/node and is usable
# by the runtime user. Download-then-run instead of `curl | bash`: a failed
# download must fail the build, not silently pipe EOF into bash.
RUN curl -fsSL https://opencode.ai/install -o /tmp/opencode-install.sh && \
    bash /tmp/opencode-install.sh && \
    rm -f /tmp/opencode-install.sh
# The installer's launcher dir is not on the base PATH.
ENV PATH="/home/node/.opencode/bin:${PATH}"
# The install was frozen at build time; keep that property -- for a
# distributed image, rebuilding IS the upgrade path (same rationale as the
# Claude image's DISABLE_AUTOUPDATER).
ENV OPENCODE_DISABLE_AUTOUPDATE=1

# No CLAUDE_CONFIG_DIR, no plugin bake, no settings.json here: opencode has
# its own config/auth model, not Claude Code's, and #177 explicitly scopes out
# the Claude Code plugin/marketplace steps. Setting CLAUDE_CONFIG_DIR would
# also re-enable the Claude-only onboarding-seed block in entrypoint.sh above,
# which this image has no use for and which the acceptance criteria require
# to stay inert.

WORKDIR /workspace
ENTRYPOINT ["/entrypoint.sh"]
# Default: idle bash. The operator overrides Cmd at container-create time.
CMD ["bash"]
