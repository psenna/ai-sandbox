# syntax=docker/dockerfile:1
#
# The docker-operator's agent image: the repo-root Dockerfile's agent, minus the
# Kubernetes-operator-only skills, plus tmux.
#
# BUILT WITH REPO-ROOT CONTEXT so it can COPY the same real assets the root
# image uses (claude-code/*/SKILL.md, agent-context/CLAUDE.md, entrypoint.sh)
# without vendoring copies that would silently drift:
#
#   docker build -f docker-operator/agent/Dockerfile \
#     -t ghcr.io/psenna/ai-sandbox-docker-operator-agent:dev .
#
# Two deliberate differences from the root Dockerfile:
#
#  1. tmux. The operator runs `claude` inside a detachable tmux session so the
#     web UI terminal survives the operator/browser restarting; the WS bridge
#     is a `docker exec ... tmux attach -t main` (see internal/wsbridge).
#  2. Five skills, not eight. use-sandbox / freeze / unfreeze describe the
#     Kubernetes operator's sidecar control API and its snapshot-based
#     freeze/wake -- neither exists here, and a docker-operator agent that read
#     them would be actively misled. This image DOES bake store-file (the
#     docker-operator's centralized /workspace/store file store), which the
#     root image does not, since the k8s operator has no such store.
#     use-docker is the LOCAL fork
#     (docker-operator/agent/skills/use-docker/SKILL.md): each agent gets a
#     private dinernet, so the compose stack's static dependaproxy IP is
#     meaningless and the fork reads /workspace/dependaproxy-ip instead.
#
# CMD stays ["bash"]. The operator overrides Cmd at container-create time
# (tmux-boot.sh), which keeps this Dockerfile agnostic about which orchestrator
# runs it -- the same image still works under plain `docker run`.

FROM node:22-alpine

# Same package set as the root Dockerfile (git for git-proxy clone/fetch/push,
# ca-certificates, bash for the entrypoint, curl for the broker leg, docker-cli
# as a client-only talker to the per-agent rootless DinD sidecar), plus tmux.
RUN apk add --no-cache git ca-certificates bash curl docker-cli tmux && rm -rf /var/cache/apk/* && \
    npm install -g @anthropic-ai/claude-code

# Non-root runtime: node:22-alpine already ships a `node` user with uid 1000
# (HOME=/home/node), matching git-proxy's and dependaproxy's uid 1000 so shared
# volumes are read/writable by all of them. Reused, not recreated -- `adduser -D
# -u 1000` would collide with the built-in user. (USER node comes last so the
# COPY/chmod steps still run as root.)

# Bake the skills + the always-loaded CLAUDE.md; the entrypoint copies them into
# /workspace/.claude/skills/ and /workspace/CLAUDE.md at startup.
COPY claude-code/use-git-proxy/SKILL.md                /opt/skills/use-git-proxy/SKILL.md
COPY docker-operator/agent/skills/use-docker/SKILL.md  /opt/skills/use-docker/SKILL.md
COPY claude-code/use-dependaproxy/SKILL.md             /opt/skills/use-dependaproxy/SKILL.md
COPY claude-code/implement-issue/SKILL.md              /opt/skills/implement-issue/SKILL.md
COPY claude-code/store-file/SKILL.md                   /opt/skills/store-file/SKILL.md
COPY claude-code/agent-context/CLAUDE.md               /opt/agent-context/CLAUDE.md

COPY claude-code/entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

# tmux-boot.sh: the script the operator overrides the container's Cmd to at
# create time (internal/agent). Baked into the image rather than bind-mounted
# so the image is self-contained and the operator needs no host path -- a bind
# source is resolved by the DAEMON on the host, which a containerised operator
# cannot know. It is still only a Cmd override, so CMD ["bash"] below is
# untouched and a plain `docker run` of this image is unaffected.
COPY docker-operator/agent/tmux-boot.sh /usr/local/bin/tmux-boot.sh
RUN chmod +x /usr/local/bin/tmux-boot.sh

# Pre-create /workspace as node BEFORE switching USER: WORKDIR creates a
# missing directory as root regardless of the active USER (a known
# Docker/BuildKit quirk -- verified against this exact image: without this
# RUN, WORKDIR /workspace below still leaves it root:root). A fresh named
# volume mounted over an image path inherits that path's ownership via
# Docker's copy-up, so an un-chowned /workspace here means entrypoint.sh
# (running as node) fails at its very first `mkdir -p
# /workspace/.claude/skills/...` with EACCES on every agent's first boot,
# since each agent gets a genuinely fresh workspace volume by design.
RUN mkdir -p /workspace && chown node:node /workspace

USER node
# Create CLAUDE_CONFIG_DIR as the node user: same copy-up reasoning as
# /workspace above -- without this the claude-config volume is root-owned and
# Claude Code cannot write its session dir (EACCES on .../session-env).
RUN mkdir -p /home/node/.claude-sandbox
WORKDIR /workspace
ENTRYPOINT ["/entrypoint.sh"]
# Default: idle bash. The operator overrides Cmd at create time (task 9).
CMD ["bash"]
