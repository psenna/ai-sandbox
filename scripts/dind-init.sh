#!/bin/sh
# dind-init.sh — ai-sandbox DinD entrypoint override.
#
# Starts dockerd (args come from the `docker` service's compose `command:`), waits
# for it to come up, then locks down this daemon's outbound HTTP(S) egress:
#
#   1. Default-deny ports 80/443, with explicit allows for (a) this agent's
#      own private dinernet subnet -- so dependaproxy and any other internal
#      service stay reachable -- and (b) an operator-configured list of
#      container-registry hosts, so `docker pull`/`docker run` inside this
#      sandbox (and any nested workload container it launches) can only reach
#      an approved set of image registries.
#   2. On top of that, an explicit denylist for the public npm/pypi/Go
#      registries, so workload containers fetch dependencies ONLY through
#      DependaProxy. This is redundant with the default-deny above (those
#      hosts are not on the registry allowlist either) but stays explicit: it
#      documents intent and still blocks them even if an operator's registry
#      allowlist accidentally includes a package-registry host.
#
# Why an entrypoint override: docker:27-dind's own entrypoint only generates TLS
# certs (skipped when DOCKER_TLS_CERTDIR="") and then execs dockerd. This script
# does the same, but keeps dockerd in the background while it installs the rules,
# then stays as the container's main process (forwarding signals to dockerd).
#
# Every hostname below (denylist and allowlist alike) is resolved to IPs ONCE
# at container start — restart the `docker` service to refresh. This is a
# sharper limitation for the allowlist than the denylist: a CDN-backed
# registry (Docker Hub and GHCR both front blob storage with a CDN whose IPs
# rotate) can start failing pulls after the resolved IPs go stale, not just
# under-block. See use-docker/SKILL.md for the operator-facing writeup of both
# lists. sum.golang.org is intentionally NOT blocked: Go verifies module
# checksums against it directly (it returns hashes, not module content).
set -eu

# A prior dockerd in THIS SAME container can be killed abruptly (a host power
# loss/hard reboot, an OOM kill) with no chance to remove its own pidfile.
# /var/run/docker.pid then lingers on the container's writable layer across a
# restart (only `docker rm` clears it), and the next dockerd refuses to start,
# misreading the stale file as "another instance is still running" --
# "failed to start daemon ...: process with PID N is still running" -- even
# though nothing is. Since this script is the only thing that ever starts
# dockerd in this container, any pidfile found here is necessarily stale by
# definition: removing it first is always safe, first boot included (rm -f
# no-ops when there is nothing to remove).
rm -f /var/run/docker.pid

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

# Pre-pull the tiny image the sidecar's own healthcheck runs
# (dindSmokeTestImage in internal/agent/create.go -- kept in sync with this
# literal by hand, cross-referenced in both places since dockerd's own args
# leave no clean way to pass one value into both a Go string and this shell
# script). Without this, the FIRST healthcheck attempt(s) right after this
# container starts would have to pull it under Docker's Healthcheck.Timeout,
# and a cold pull can exceed that on a slow network -- tolerated by the
# StartPeriod/Retries budget, but there is no reason to pay it every time
# when a one-shot pull here avoids it entirely.
SMOKE_TEST_IMAGE="busybox:1.36.1"
if ! docker pull "$SMOKE_TEST_IMAGE" >/dev/null 2>&1; then
  echo "dind-init: WARNING could not pre-pull $SMOKE_TEST_IMAGE; the healthcheck may be slow or fail while it retries the pull on its own" >&2
fi

# Docker's DOCKER-USER chain is its reserved spot for operator rules — daemon
# network events do not flush it, and FORWARD already jumps to it.
iptables -N DOCKER-USER 2>/dev/null || true
if ! iptables -S FORWARD | grep -q -- '-j DOCKER-USER'; then
  iptables -A FORWARD -j DOCKER-USER
fi

# --- Container-registry allowlist (default-deny 80/443, explicit allows) ---
#
# iptables cannot tell a `docker pull` apart from any other HTTP(S) request to
# the same host, so "only allow approved registries" necessarily means "deny
# ALL outbound 80/443 except an explicit allowlist" -- there is no way to
# scope this to image pulls alone. DOCKER-USER catches bridged workload
# containers (including ones the nested dockerd creates on its own bridge,
# inside this same network namespace); OUTPUT also catches this container's
# own traffic (dockerd's own pulls) and any --network host workload.
#
# iptables -I always inserts at the TOP of the chain, so rules added later in
# this script end up checked FIRST. Insertion order below is chosen so the
# final precedence (top to bottom) is: npm/pypi/Go denylist (added last, at
# the bottom of the script) > registry allowlist > dinernet-subnet allow >
# this catch-all deny (added first, right here).
#
# Unlike the per-host loops below, a failed catch-all-deny insertion is fatal
# (no `|| true`, so `set -e` aborts the script): silently continuing without
# it would leave this sandbox's registry access fully open instead of
# default-deny, which is worse than the sidecar failing to come up.
for chain in DOCKER-USER OUTPUT; do
  iptables -I "$chain" -p tcp -m multiport --dports 80,443 -j REJECT
done

# Allow this agent's own private dinernet subnet, so sibling containers on it
# (dependaproxy, joined dynamically at create time; any future internal
# service) stay reachable under the default-deny above.
dinernet_cidr=$(ip -4 -o addr show scope global | awk '{print $4}' | head -n1)
if [ -n "$dinernet_cidr" ]; then
  for chain in DOCKER-USER OUTPUT; do
    iptables -I "$chain" -d "$dinernet_cidr" -p tcp -m multiport --dports 80,443 -j ACCEPT
  done
else
  echo "dind-init: WARNING could not determine this container's own subnet; internal traffic (e.g. dependaproxy) may be unreachable" >&2
fi

# Approved container-registry hosts -- what `docker pull`/`docker run` may
# reach. AGENT_ALLOWED_REGISTRY_HOSTS (space-separated), when set, REPLACES
# this default entirely (it is not merged/appended) -- set it to add a
# private registry or trim this deployment down to only what it actually
# uses. Docker Hub and GHCR are both CDN-fronted for blob storage, which is
# why each has more than one hostname below.
default_registry_hosts="docker.io registry-1.docker.io auth.docker.io index.docker.io production.cloudflare.docker.com ghcr.io pkg-containers.githubusercontent.com quay.io mcr.microsoft.com registry.k8s.io gcr.io k8s.gcr.io"
registry_hosts="${AGENT_ALLOWED_REGISTRY_HOSTS:-$default_registry_hosts}"

allowed=0
allow_failed=0
for host in $registry_hosts; do
  ips=$(getent ahostsv4 "$host" 2>/dev/null | awk '{print $1}' | sort -u)
  if [ -z "$ips" ]; then
    echo "dind-init: WARNING could not resolve allowed registry host $host (no allow rule added)" >&2
    continue
  fi
  for ip in $ips; do
    if iptables -I DOCKER-USER -d "$ip" -p tcp -m multiport --dports 80,443 -j ACCEPT 2>/dev/null; then
      allowed=$((allowed + 1))
    else
      allow_failed=$((allow_failed + 1))
    fi
    if iptables -I OUTPUT -d "$ip" -p tcp -m multiport --dports 80,443 -j ACCEPT 2>/dev/null; then
      allowed=$((allowed + 1))
    else
      allow_failed=$((allow_failed + 1))
    fi
  done
done
echo "dind-init: allowed $allowed registry-host IP(s) for image pulls${allow_failed:+ ($allow_failed insertions failed)}" >&2

# --- Public package-registry denylist (npm/pypi/Go), forcing DependaProxy ---
#
# Extend these lists if you add mirrors. Resolved at startup; restart the
# docker service to refresh.
NPM_HOSTS="registry.npmjs.org registry.npmjs.com registry.yarnpkg.com registry.npmmirror.com"
PYPI_HOSTS="pypi.org files.pythonhosted.org pypi.python.org"
GO_HOSTS="proxy.golang.org goproxy.io goproxy.cn"

blocked=0
failed=0
for host in $NPM_HOSTS $PYPI_HOSTS $GO_HOSTS; do
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

echo "dind-init: dockerd up; blocked $blocked registry egress IP(s)${failed:+ ($failed insertions failed)}" >&2

# Stay as the container's main process, forwarding termination to dockerd.
wait "$DPID"
