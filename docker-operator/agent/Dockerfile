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

# NPM_REGISTRY / GOPROXY: build-time overrides for the two registries this
# Dockerfile's own LSP-binary installs use (below). Default to the public
# registries, which is what CI's unrestricted GitHub Actions runners reach
# directly (see docker-operator-agent-image.yml -- no proxy wired in there).
# A sandboxed dev build whose only egress is DependaProxy overrides both and
# adds the resolver entry the nested daemon can't provide on its own:
#   docker build \
#     --build-arg NPM_REGISTRY=http://dependaproxy:8080/npm \
#     --build-arg GOPROXY=http://dependaproxy:8080/goproxy \
#     --add-host dependaproxy:$(cat /workspace/dependaproxy-ip) ...
ARG NPM_REGISTRY=https://registry.npmjs.org
ARG GOPROXY=https://proxy.golang.org,direct

# gopls, built in its own stage so the final image doesn't carry a Go
# toolchain -- only the compiled binary (COPY --from, below) does. Alpine
# base to match the final image's arch/libc and keep this throwaway stage
# small under the QEMU-emulated arm64 leg of the multi-arch build. 1.26, not
# docker-operator's own 1.25 toolchain (Makefile's GOLANG_IMAGE): confirmed
# by a failed build that gopls@v0.23.0 requires go >= 1.26.0. Pinned (not
# @latest) for reproducibility, matching this repo's other tool-version pins
# (Makefile's GOLANGCI_LINT_VERSION/GOVULNCHECK_VERSION) -- bump deliberately.
FROM golang:1.26-alpine AS gopls-builder
ARG GOPROXY
ENV GOPROXY=${GOPROXY}
RUN go install golang.org/x/tools/gopls@v0.23.0

FROM node:22-alpine
# Re-declared: a build ARG declared before the first FROM is visible to every
# stage's FROM line, but a stage must re-declare it to use its value in a RUN
# (confirmed the hard way -- without this line, the npm install below ignored
# --build-arg NPM_REGISTRY and hit the public registry directly).
ARG NPM_REGISTRY

# git for git-proxy clone/fetch/push, ca-certificates, bash for the entrypoint
# + the Claude Code installer, curl for the broker leg + the installer,
# libgcc/libstdc++/ripgrep for the native binary on musl, docker-cli as a
# client-only talker to the per-agent rootless DinD sidecar, plus tmux.
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
#
# claude-supervisor.sh: what tmux-boot.sh actually runs as the pane's
# process. It auto-restarts `claude` on a non-zero exit (an accidental
# Ctrl+C, a transient crash) instead of leaving the web UI terminal on a dead
# pane -- see the script for the restart budget and tmux-boot.sh's
# remain-on-exit as the fallback when `claude` is genuinely broken.
COPY docker-operator/agent/tmux-boot.sh         /usr/local/bin/tmux-boot.sh
COPY docker-operator/agent/claude-supervisor.sh /usr/local/bin/claude-supervisor.sh
RUN chmod +x /usr/local/bin/tmux-boot.sh /usr/local/bin/claude-supervisor.sh

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
# CLAUDE_CONFIG_DIR must be set for the plugin bake below to land in the
# right place: it is NOT ~/.claude (claude's default), it's this directory --
# the one internal/agent/create.go's configMount mounts the per-agent
# claude-config VOLUME onto at runtime (CLAUDE_CONFIG_DIR is also set in the
# container's own Env there, to the same value; setting it here too just
# keeps a plain `docker run` of this image consistent).
#
# Baking plugins HERE, not under /workspace, matters for the same copy-up
# reasoning as /workspace/.claude-sandbox above: each agent's claude-config
# volume is fresh and EMPTY the first time it's mounted over this path, so
# Docker's copy-up seeds it with everything installed below -- once, on that
# first mount. Unlike /workspace's skills (which entrypoint.sh re-copies on
# every boot to pick up image updates), plugin state has to stay the user's
# own mutable, durable session state after that -- a plugin installed or
# removed inside a running agent must survive --continue/--resume, so nothing
# here re-syncs it on later boots.
ENV CLAUDE_CONFIG_DIR=/home/node/.claude-sandbox
# claude-plugins-official is Anthropic's official, curated marketplace
# (normally auto-added on first interactive launch -- a `docker build` RUN
# never "starts interactively", so it's added explicitly here).
# obra/superpowers-marketplace is third-party (not Anthropic-vetted): a
# widely used structured planning/TDD skills framework. Notably, this repo's
# own docs/superpowers/{plans,specs}/ already follow ITS output convention --
# baking it in makes that workflow available to every agent, not just
# whoever authored those docs locally.
#
# Every plugin below is pure skills/agents/commands or an LSP config (no MCP
# server needing credentials this sandbox doesn't have), and none of them
# talk to github.com/gitlab.com directly the way e.g. the official `github`
# plugin or commit-commands' `/commit-push-pr` would -- see the PR
# description for the plugins deliberately left out and why.
RUN claude plugin marketplace add anthropics/claude-plugins-official && \
    claude plugin marketplace add obra/superpowers-marketplace && \
    for p in security-guidance code-simplifier feature-dev claude-md-management \
             frontend-design modern-web-guidance \
             typescript-lsp gopls-lsp pyright-lsp; do \
      claude plugin install "$p@claude-plugins-official" -y; \
    done && \
    claude plugin install superpowers@superpowers-marketplace -y
WORKDIR /workspace
ENTRYPOINT ["/entrypoint.sh"]
# Default: idle bash. The operator overrides Cmd at create time (task 9).
CMD ["bash"]
