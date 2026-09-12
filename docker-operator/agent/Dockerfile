# syntax=docker/dockerfile:1
#
# The docker-operator's agent image: this is the ONLY Dockerfile in the repo
# that bakes any claude-code/*/SKILL.md file (the single-agent "compose
# stack" Dockerfile this once mirrored was retired -- see the repo-root
# docker-compose.yaml's header -- and the Kubernetes operator's own workload
# image is built elsewhere). tmux is the one thing genuinely specific to this
# image: the operator runs `claude` inside a detachable tmux session so the
# web UI terminal survives the operator/browser restarting; the WS bridge is
# a `docker exec ... tmux attach -t main` (see internal/wsbridge).
#
# BUILT WITH REPO-ROOT CONTEXT so it can COPY claude-code/*/SKILL.md,
# agent-context/CLAUDE.md and entrypoint.sh directly, without vendoring
# copies that would silently drift:
#
#   docker build -f docker-operator/agent/Dockerfile \
#     -t ghcr.io/psenna/ai-sandbox-docker-operator-agent:dev .
#
# Five skills, not eight: use-sandbox / freeze / unfreeze describe the
# Kubernetes operator's sidecar control API and its snapshot-based
# freeze/wake -- neither exists here, and a docker-operator agent that read
# them would be actively misled. This image DOES bake store-file (the
# docker-operator's centralized /workspace/store file store), which the k8s
# operator's image does not, since it has no such store.
#
# use-git-proxy is the ONE local fork left
# (docker-operator/agent/skills/use-git-proxy/SKILL.md): it ships in the
# separate git-proxy repository and is vendored here (claude-code/use-git-proxy/
# SKILL.md, untouched, for diffing against upstream on the next re-vendor);
# the fork is the same content trimmed of two sections that only make sense
# for a reader who does not already have the skill installed for them. Every
# other skill here is baked straight from claude-code/ with no fork: nothing
# else in this repo builds from those files, so there is no second copy to
# drift out of sync -- if you touch use-docker or use-dependaproxy, edit
# claude-code/{use-docker,use-dependaproxy}/SKILL.md directly.
#
# CMD stays ["bash"]. The operator overrides Cmd at container-create time
# (tmux-boot.sh), which keeps this Dockerfile agnostic about which orchestrator
# runs it -- the same image still works under plain `docker run`.

FROM node:22-alpine

# git for git-proxy clone/fetch/push, ca-certificates, bash for the entrypoint
# + the Claude Code installer, curl for the broker leg + the installer,
# libgcc/libstdc++/ripgrep for the native binary on musl, docker-cli as a
# client-only talker to the per-agent rootless DinD sidecar, plus tmux.
RUN apk add --no-cache git ca-certificates bash curl libgcc libstdc++ ripgrep docker-cli tmux && \
    rm -rf /var/cache/apk/*

# A UTF-8 locale so tmux runs in UTF-8 mode. tmux picks its charset from
# LC_ALL / LC_CTYPE / LANG; with none set the attach client falls back to a
# C/ASCII locale and renders every non-ASCII byte (ç, á, …) as `_` -- both in
# the pane and in what a web viewer types. `C.UTF-8` is the locale musl always
# provides (no locale-gen needed). The operator also injects LANG into the
# agent env and the terminal-attach exec, so this line is the belt-and-braces
# default that also covers a plain `docker run` of the image.
ENV LANG=C.UTF-8

# Non-root runtime: node:22-alpine already ships a `node` user with uid 1000
# (HOME=/home/node), matching git-proxy's and dependaproxy's uid 1000 so shared
# volumes are read/writable by all of them. Reused, not recreated -- `adduser -D
# -u 1000` would collide with the built-in user. (USER node comes last so the
# COPY/chmod steps still run as root.)

# Bake the skills + the always-loaded CLAUDE.md; the entrypoint copies them into
# /workspace/.claude/skills/ and /workspace/CLAUDE.md at startup.
COPY docker-operator/agent/skills/use-git-proxy/SKILL.md  /opt/skills/use-git-proxy/SKILL.md
COPY claude-code/use-docker/SKILL.md                      /opt/skills/use-docker/SKILL.md
COPY claude-code/use-dependaproxy/SKILL.md                /opt/skills/use-dependaproxy/SKILL.md
COPY claude-code/implement-issue/SKILL.md                 /opt/skills/implement-issue/SKILL.md
COPY claude-code/store-file/SKILL.md                      /opt/skills/store-file/SKILL.md
COPY claude-code/agent-context/CLAUDE.md                  /opt/agent-context/CLAUDE.md

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
# Create CLAUDE_CONFIG_DIR as the node user: same copy-up reasoning as
# /workspace above -- without this the claude-config volume is root-owned and
# Claude Code cannot write its session dir (EACCES on .../session-env).
RUN mkdir -p /home/node/.claude-sandbox
WORKDIR /workspace
ENTRYPOINT ["/entrypoint.sh"]
# Default: idle bash. The operator overrides Cmd at create time (task 9).
CMD ["bash"]
