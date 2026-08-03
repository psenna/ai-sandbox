#!/bin/sh
# dind-init.sh — ai-sandbox DinD entrypoint override.
#
# Starts dockerd (args come from the `docker` service's compose `command:`), waits
# for it to come up, then blocks egress to the public npm registries so workload
# containers launched inside this daemon can fetch npm packages ONLY through
# DependaProxy. Without the block, an agent that overrides npm's registry would
# reach registry.npmjs.org directly.
#
# Why an entrypoint override: docker:27-dind's own entrypoint only generates TLS
# certs (skipped when DOCKER_TLS_CERTDIR="") and then execs dockerd. This script
# does the same, but keeps dockerd in the background while it installs the rules,
# then stays as the container's main process (forwarding signals to dockerd).
#
# The blocked host list is resolved to IPs at container start — restart the
# `docker` service to refresh it. See use-docker/SKILL.md for how the agent is
# expected to route npm through DependaProxy (this is the enforcement backstop).
set -eu

# Start dockerd with the compose command args (TCP + unix sockets), backgrounded
# so we can insert the iptables rules while it runs.
dockerd "$@" &
DPID=$!
trap 'kill -TERM "$DPID" 2>/dev/null || true; wait "$DPID" 2>/dev/null || true' TERM INT

# Wait for the daemon (docker info uses the unix socket --host=unix://...).
ready=0
i=0
while [ "$i" -lt 60 ]; do
  if docker info >/dev/null 2>&1; then ready=1; break; fi
  if ! kill -0 "$DPID" 2>/dev/null; then
    echo "dind-init: dockerd exited during startup" >&2
    exit 1
  fi
  i=$((i + 1))
  sleep 1
done
if [ "$ready" -ne 1 ]; then
  echo "dind-init: dockerd did not become ready within 60s" >&2
  exit 1
fi

# Public npm registries the sandbox must NOT reach directly. Extend this list if
# you add mirrors. Resolved at startup; restart the docker service to refresh.
NPM_HOSTS="registry.npmjs.org registry.npmjs.com registry.yarnpkg.com registry.npmmirror.com"

# Docker's DOCKER-USER chain is its reserved spot for operator rules — daemon
# network events do not flush it, and FORWARD already jumps to it.
iptables -N DOCKER-USER 2>/dev/null || true
if ! iptables -S FORWARD | grep -q -- '-j DOCKER-USER'; then
  iptables -A FORWARD -j DOCKER-USER
fi

blocked=0
failed=0
for host in $NPM_HOSTS; do
  # getent ahostsv4 prints one line per A record: "<ip> <canonical> ...".
  ips=$(getent ahostsv4 "$host" 2>/dev/null | awk '{print $1}' | sort -u)
  if [ -z "$ips" ]; then
    echo "dind-init: WARNING could not resolve $host (no block added)" >&2
    continue
  fi
  for ip in $ips; do
    # DOCKER-USER: catches bridged workload containers (the normal case).
    if iptables -I DOCKER-USER -d "$ip" -p tcp -m multiport --dports 80,443 -j REJECT 2>/dev/null; then
      blocked=$((blocked + 1))
    else
      failed=$((failed + 1))
    fi
    # OUTPUT: also catches --network host containers (they live in this netns).
    if iptables -I OUTPUT -d "$ip" -p tcp -m multiport --dports 80,443 -j REJECT 2>/dev/null; then
      blocked=$((blocked + 1))
    else
      failed=$((failed + 1))
    fi
  done
done

echo "dind-init: dockerd up; blocked $blocked npm egress IP(s)${failed:+ ($failed insertions failed)}" >&2

# Stay as the container's main process, forwarding termination to dockerd.
wait "$DPID"
