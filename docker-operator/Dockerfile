# syntax=docker/dockerfile:1
#
# The docker-operator binary's own image -- NOT the agent image
# (docker-operator/agent/Dockerfile is that one). This is the trusted control
# plane: unlike an agent, it holds real Docker daemon credentials (see
# docker-compose.yaml's docker.sock bind mount) so it can create, inspect and
# remove the sibling containers/volumes/networks that make up each agent --
# the same "manager holds the credential, the workload it manages does not"
# split the Kubernetes operator already uses between its manager and the
# agent container.
#
#   docker build -f docker-operator/Dockerfile -t docker-operator:dev .
#
# Multi-stage: the build stage needs the Go toolchain and module cache; the
# final image is just the static binary plus CA certificates (for a
# TLS-fronted Docker daemon or, later, an HTTPS registry -- this sandbox's own
# daemon is plain HTTP, but the image should not assume every deployment is).

ARG GO_VERSION=1.25
FROM golang:${GO_VERSION} AS build

# Overridable so a build behind an internal proxy (this repo's own
# DependaProxy included) doesn't need a Dockerfile fork -- see
# ai-sandbox/claude-code/use-dependaproxy/SKILL.md for why a real deployment
# would never need to touch this.
ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=${GOPROXY} GOFLAGS=-buildvcs=false CGO_ENABLED=0

WORKDIR /src
COPY docker-operator/go.mod docker-operator/go.sum ./
RUN go mod download
COPY docker-operator/ ./
RUN go build -trimpath -ldflags "-s -w" -o /out/docker-operator ./cmd/docker-operator

FROM alpine:3
RUN apk add --no-cache ca-certificates
COPY --from=build /out/docker-operator /usr/local/bin/docker-operator

# Deliberately root, unlike every other image in this repo. Once a container
# holds /var/run/docker.sock (docker-compose.yaml's bind mount), it can ask
# the host daemon to run an arbitrary container with an arbitrary bind mount
# of its own -- host-root-equivalent regardless of which uid inside THIS
# container is doing the asking. Dropping to a non-root uid here would not
# shrink that capability, and would instead reintroduce the exact
# host-docker-group-GID-matching problem the rest of this repo's images
# avoid entirely by never touching the socket. The uid that matters for
# blast radius is the AGENT's (docker-operator/agent/Dockerfile), which
# never gets a Docker credential at all.
ENTRYPOINT ["/usr/local/bin/docker-operator"]
