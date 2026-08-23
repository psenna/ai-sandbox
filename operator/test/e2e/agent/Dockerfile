# Fixture image for the operator's kind-based e2e suite (operator/test/e2e):
# a stand-in for the real agent image, minimal enough to build offline (no
# DependaProxy involvement -- alpine's own package index) but honoring the
# same contract RenderPod relies on: ENTRYPOINT runs unconditionally
# (RenderPod sets `args` only, never `command`), and `claude -p <prompt>` is
# how the agent container is invoked. Never pushed anywhere; only
# `kind load docker-image`ed by hack/e2e-up.sh, tagged `:test` (never
# `:latest` -- see RenderPod's imagePullPolicy note in the e2e README).
#
# docker-cli is present so #24's specs can drive the rootless-podman engine's
# Docker-compatible API over $DOCKER_HOST -- the same client a real agent
# uses.
FROM alpine:3.21

RUN apk add --no-cache bash coreutils curl docker-cli \
	&& addgroup -g 1000 node \
	&& adduser -D -u 1000 -G node node

COPY entrypoint.sh /entrypoint.sh
COPY claude /usr/local/bin/claude
RUN chmod 0755 /entrypoint.sh /usr/local/bin/claude

USER 1000:1000
ENTRYPOINT ["/entrypoint.sh"]
