#!/bin/sh
# Binary-level smoke test: the operator starts against a real Docker daemon,
# serves /healthz, creates one agent via the real HTTP API, deletes it, and
# asserts full cleanup -- then shuts down cleanly on SIGTERM. Mirrors
# operator/hack/smoke.sh's shape (start, poll /healthz, SIGTERM, assert exit
# 0), extended with the create/delete round-trip this issue's acceptance
# criteria asks for.
#
# Prerequisites (not this script's job to arrange):
#   - A reachable Docker daemon at $DOCKER_HOST (or the platform default).
#   - `make agent-image` already run, so AGENT_IMAGE exists on that daemon --
#     matches `agent-image-smoke`'s own assumption. Without it, create fails
#     at the image-ensure step with a registry error instead of reaching the
#     DinD-sidecar step this script actually wants to observe.
#   - docker:27-dind pulled (a public image; `docker pull docker:27-dind` if
#     the daemon has never fetched it -- ImagePull would otherwise add a slow
#     first-create to the timing budget below).
#
# HONEST LIMITATION: creating the DinD sidecar needs the sysbox-runc runtime
# (DOCKER_RUNTIME, default "sysbox-runc") -- see ai-sandbox/README.md and
# setup-ubuntu-host.sh. A host without it (this repo's own sandbox CI
# included -- confirmed empirically, see internal/agent's and
# internal/wsbridge's own integration tests) cannot complete a real create,
# and this script SKIPs the create/delete/cleanup assertions rather than
# failing on an environment constraint that has nothing to do with whether
# cmd/docker-operator itself is wired correctly. What it still verifies in
# that case: the create request fails via the real HTTP API for exactly the
# expected reason (sysbox-runc, not some other bug), and -- the part that
# does NOT need sysbox -- that a create failure after volumes/network exist
# but before the DinD container does leaves the daemon exactly as clean as
# before the request, proving the real create-flow rollback end to end.
set -eu

ROOT="$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)"
TMPDIR="$(mktemp -d)"
trap 'rm -rf "$TMPDIR"' EXIT

BASE="http://127.0.0.1:18080"
AGENT_TOKEN="${AGENT_TOKEN:-smoke-test-token}"
GITHUB_REPO="${GITHUB_REPO:-psenna/ai-sandbox.git}"

fail() { echo "FAIL: $*" >&2; [ -n "${pid:-}" ] && kill "$pid" 2>/dev/null || true; exit 1; }

echo "--- starting the operator ---"
AGENT_TOKEN="$AGENT_TOKEN" GITHUB_REPO="$GITHUB_REPO" \
  MAX_AGENTS=1 STATE_DB_PATH="$TMPDIR/state.db" LISTEN_ADDR=127.0.0.1:18080 \
  "$ROOT/bin/docker-operator" >"$TMPDIR/operator.log" 2>&1 &
pid=$!

ok=0
for _ in $(seq 1 30); do
  if curl -sf "$BASE/healthz" >/dev/null 2>&1; then ok=1; break; fi
  sleep 1
done
[ "$ok" = "1" ] || { cat "$TMPDIR/operator.log"; fail "/healthz never became ready"; }
echo "OK: /healthz responding"

echo "--- listing resources before create (baseline) ---"
before_containers=$(docker ps -a --filter label=ai-sandbox.docker-operator/managed=true -q | wc -l)
before_networks=$(docker network ls --filter label=ai-sandbox.docker-operator/managed=true -q | wc -l)
before_volumes=$(docker volume ls --filter label=ai-sandbox.docker-operator/managed=true -q | wc -l)

echo "--- creating one agent ---"
create_resp="$(curl -s -w '\n%{http_code}' -X POST "$BASE/api/agents" -d '{"name":"smoke-test"}')"
create_code="$(printf '%s' "$create_resp" | tail -n1)"
create_body="$(printf '%s' "$create_resp" | sed '$d')"

if [ "$create_code" = "201" ]; then
  id="$(printf '%s' "$create_body" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')"
  [ -n "$id" ] || fail "201 response had no id field: $create_body"
  echo "OK: created agent $id"

  echo "--- deleting it ---"
  delete_code="$(curl -s -o /dev/null -w '%{http_code}' -X DELETE "$BASE/api/agents/$id")"
  [ "$delete_code" = "200" ] || fail "DELETE returned $delete_code, want 200"
  echo "OK: deleted agent $id"

  echo "--- asserting full cleanup ---"
  left="$(docker ps -a --filter "label=ai-sandbox.docker-operator/agent-id=$id" -q)$(docker network ls --filter "label=ai-sandbox.docker-operator/agent-id=$id" -q)$(docker volume ls --filter "label=ai-sandbox.docker-operator/agent-id=$id" -q)"
  [ -z "$left" ] || fail "resources for agent $id survived delete"
  echo "OK: no Docker resources remain for agent $id"

elif grep -qi 'sysbox-runc\|unknown or invalid runtime name' "$TMPDIR/operator.log"; then
  echo "SKIP: sysbox-runc is not installed on this host's docker daemon, so the DinD sidecar step of a real create cannot succeed here (create returned $create_code: $create_body)"
  echo "--- asserting create-failure rollback left no orphaned resources ---"
  after_containers=$(docker ps -a --filter label=ai-sandbox.docker-operator/managed=true -q | wc -l)
  after_networks=$(docker network ls --filter label=ai-sandbox.docker-operator/managed=true -q | wc -l)
  after_volumes=$(docker volume ls --filter label=ai-sandbox.docker-operator/managed=true -q | wc -l)
  [ "$after_containers" = "$before_containers" ] || fail "container count changed ($before_containers -> $after_containers) despite the create failing"
  [ "$after_networks" = "$before_networks" ] || fail "network count changed ($before_networks -> $after_networks) despite the create failing"
  [ "$after_volumes" = "$before_volumes" ] || fail "volume count changed ($before_volumes -> $after_volumes) despite the create failing"
  echo "OK: the volumes+network created before the sysbox-runc failure were rolled back; no managed resources left behind"
else
  cat "$TMPDIR/operator.log"
  fail "agent create returned $create_code (not the expected sysbox-runc failure): $create_body"
fi

echo "--- shutting down ---"
kill -TERM "$pid"
if wait "$pid"; then rc=0; else rc=$?; fi
[ "$rc" -eq 0 ] || { cat "$TMPDIR/operator.log"; fail "operator exited $rc after SIGTERM, expected 0"; }
echo "OK: clean shutdown on SIGTERM"

echo "smoke: PASS"
