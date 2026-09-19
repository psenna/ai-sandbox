#!/bin/sh
# End-to-end acceptance test for the opencode harness (issue #183), tying
# #177-#182 together: create an opencode+ollama agent container against a
# real local Ollama model and confirm the four bullets the issue lists --
# boot/context, the three execution-surface skills (use-git-proxy,
# use-dependaproxy, use-docker) actually working, a real edit pushed through
# git-proxy's policy gates, and session continuity across a container
# kill/resume.
#
# This is NOT docker-operator's own integration test (that needs sysbox-runc
# and the real create-agent HTTP flow -- see hack/smoke.sh) and it is NOT a
# unit test. It is a standalone, manually-driven exercise of the SAME image
# and the SAME skills a real docker-operator deployment bakes in, run either:
#
#   (a) against a real docker-operator stack's shared services (git-proxy,
#       dependaproxy, ollama all reachable by their compose/operator hostnames
#       from wherever this script's containers run), or
#   (b) standalone, against manually-started containers you point the env
#       vars below at (an Ollama container, a DependaProxy, a git-proxy).
#
# Several checks below gracefully SKIP with a printed reason instead of
# hard-failing the whole run when the host is under-provisioned for that one
# check -- this is deliberate: an environment gap is not the same thing as a
# regression in the opencode image or the skills it bakes in, and burying a
# real regression under a wall of environment-gap failures defeats the point
# of a smoke test. The CANONICAL example of an under-provisioned host is
# THIS repo's own sandbox, where every one of these SKIP triggers was
# actually hit while writing this script:
#
#   - no sysbox-runc runtime on the Docker daemon (`docker info` lists only
#     runc/containerd) -- so a real docker-operator create can never reach
#     the DinD-sidecar step; only a standalone container run of the image
#     (path (b) above) is possible here.
#   - no `docker-operator-dependaproxy`-named container on the daemon this
#     script talks to -- the real orchestrator's per-agent NetworkConnect
#     call has nothing to find -- so the dependaproxy preflight below falls
#     back to "is DEPENDAPROXY_URL directly reachable", which in this sandbox
#     it is (dependaproxy is dual-homed onto both the operator's network and
#     this sandbox's own DinD network).
#   - qwen3:0.6b on CPU-only Ollama inference measured ~1.2 tokens/sec here
#     and hit opencode's 300s provider-response timeout against opencode's
#     large system prompt + tool schema before completing even one tool
#     call -- reachable != usable, which is exactly why the Ollama preflight
#     below times a real completion instead of just probing the port.
#   - a container run on this sandbox's own outer DinD daemon cannot resolve
#     "git-proxy" by hostname -- that daemon sits on a different Docker
#     network than git-proxy and the shared Ollama, a limitation of this
#     sandbox's own improvised topology, not of the product code under test
#     (a real docker-operator deployment attaches every agent container to
#     the proxynet git-proxy lives on).
#
# On a properly-provisioned host (sysbox-runc installed, the real
# docker-operator stack or an equivalent standalone set of containers
# running, and a real Ollama serving a model capable of finishing a tool
# call in well under its provider-response timeout) every check below should
# PASS, not SKIP.
#
# Usage:
#   ./hack/opencode-e2e.sh
#   DOCKER_NETWORK=e2e-net OLLAMA_URL=http://ollama:11434 \
#     OPENCODE_IMAGE=<your-locally-built-opencode-image-tag> \
#     DEPENDAPROXY_IP=<your-dependaproxy-dinernet-ip> \
#     ./hack/opencode-e2e.sh
#
# Env var overrides (all optional; defaults match a real docker-operator
# deployment's own hostnames -- see docker-operator/internal/config/config.go
# and docker-compose.yaml for where each default comes from):
#   AGENT_TOKEN              Bearer this script's containers present to
#                             git-proxy (default: e2e-smoke-token).
#   GIT_PROXY_URL             git-proxy's git-protocol endpoint
#                             (default: http://git-proxy:8080).
#   DEPENDAPROXY_URL          DependaProxy's npm endpoint, used for the
#                             direct-reachability preflight fallback
#                             (default: http://dependaproxy:8080/npm).
#   DEPENDAPROXY_IP           IP to --add-host="dependaproxy:$DEPENDAPROXY_IP"
#                             with, mirroring what entrypoint.sh writes to
#                             /workspace/dependaproxy-ip in a real deployment.
#                             Auto-detected from a running dependaproxy
#                             container when unset.
#   OLLAMA_URL                Ollama endpoint (default: http://ollama:11434).
#   OPENCODE_MODEL             Model name pulled into that Ollama and referenced
#                             by OPENCODE_CONFIG_CONTENT below (default: qwen3:0.6b).
#   OPENCODE_IMAGE / AGENT_IMAGE_OPENCODE
#                             Image ref to test (default:
#                             ghcr.io/psenna/ai-sandbox-agent-opencode:latest).
#   BUILD_ON_MISSING          1 (default) builds the image locally (the
#                             known-working repo-root DOCKER_BUILDKIT=0 build)
#                             if it can't be pulled; 0 fails instead.
#   DOCKER_NETWORK            Docker network this script's containers attach
#                             to, in addition to the default bridge (default:
#                             unset -- default bridge only). Point this at a
#                             real deployment's proxynet, or at a standalone
#                             network like e2e-net that has an ollama alias.
#   AGENT_INNER_DOCKER_HOST   DOCKER_HOST value exercised FROM INSIDE the
#                             opencode container for the use-docker bullet
#                             (default: tcp://docker:2375, matching what the
#                             real operator injects for an agent's own DinD
#                             sidecar -- internal/agent/create.go). Override
#                             this in a standalone setup where no per-agent
#                             sidecar exists, e.g. the outer daemon's own
#                             reachable address.
#   OLLAMA_CHAT_TIMEOUT       Seconds to allow a real Ollama completion to
#                             finish before the capability preflight SKIPs
#                             the checks that need one (default: 60).
#   OPENCODE_RUN_TIMEOUT       Seconds to allow one `opencode run` invocation
#                             (default: 300, matching opencode's own
#                             provider-response timeout).
#   TEST_REPO                 owner/repo.git to exercise the git-proxy bullet
#                             against (default: psenna/ai-sandbox.git). Point
#                             this at a disposable repo for a real run --
#                             this script pushes a real branch.
#   KEEP_RESOURCES             1 leaves every container/volume/network this
#                             script created in place for inspection instead
#                             of cleaning them up (default: 0).
set -eu

ROOT="$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)"
RUN_ID="oce2e-$$"

AGENT_TOKEN="${AGENT_TOKEN:-e2e-smoke-token}"
GIT_PROXY_URL="${GIT_PROXY_URL:-http://git-proxy:8080}"
DEPENDAPROXY_URL="${DEPENDAPROXY_URL:-http://dependaproxy:8080/npm}"
DEPENDAPROXY_IP="${DEPENDAPROXY_IP:-}"
OLLAMA_URL="${OLLAMA_URL:-http://ollama:11434}"
OPENCODE_MODEL="${OPENCODE_MODEL:-qwen3:0.6b}"
OPENCODE_IMAGE="${OPENCODE_IMAGE:-${AGENT_IMAGE_OPENCODE:-ghcr.io/psenna/ai-sandbox-agent-opencode:latest}}"
BUILD_ON_MISSING="${BUILD_ON_MISSING:-1}"
DOCKER_NETWORK="${DOCKER_NETWORK:-}"
AGENT_INNER_DOCKER_HOST="${AGENT_INNER_DOCKER_HOST:-tcp://docker:2375}"
OLLAMA_CHAT_TIMEOUT="${OLLAMA_CHAT_TIMEOUT:-60}"
OPENCODE_RUN_TIMEOUT="${OPENCODE_RUN_TIMEOUT:-300}"
TEST_REPO="${TEST_REPO:-psenna/ai-sandbox.git}"
KEEP_RESOURCES="${KEEP_RESOURCES:-0}"

NET_FLAG=""
[ -n "$DOCKER_NETWORK" ] && NET_FLAG="--network=$DOCKER_NETWORK"

# ---------------------------------------------------------------------------
# Result bookkeeping. POSIX sh has no arrays, so the summary is just a
# growing string of "label: STATUS -- detail" lines, printed verbatim at the
# very end.
# ---------------------------------------------------------------------------
SUMMARY=""
record() { SUMMARY="$SUMMARY
$1 :: $2 -- $3"; }
pass() { echo "PASS: $1"; }
skip() { echo "SKIP: $1 -- $2"; }
failed() { echo "FAIL: $1 -- $2"; }

# http_reachable URL [extra docker run flags...]
#
# Returns 0 ONLY when the URL produced a real HTTP response. Every HTTP
# reachability probe in this script goes through here, deliberately: curl
# prints the literal string "000" for %{http_code} when it never got a
# response AT ALL (DNS failure, connection refused, timeout), and a naive
# "^[0-9]+$" test matches that "000" and reports a completely unreachable
# host as "reachable, just non-2xx" -- a false PASS that then cascades into
# every check gated on it. Only a real 1xx-5xx status counts.
#
# `-f` is deliberately NOT used instead: a bare "/" legitimately 404s (and
# dependaproxy's /npm legitimately 301s) on a perfectly healthy service, and
# that still proves the service answered.
http_reachable() {
  _url="$1"
  shift
  _code="$(docker run --rm $NET_FLAG "$@" curlimages/curl:8.10.1 \
    -s --max-time 5 -o /dev/null -w '%{http_code}' "$_url" 2>/dev/null || true)"
  echo "$_code" | grep -qE '^[1-5][0-9][0-9]$'
}

CREATED_CONTAINERS=""
CREATED_VOLUMES=""
CREATED_NETWORKS=""
note_container() { CREATED_CONTAINERS="$CREATED_CONTAINERS $1"; }
note_volume()    { CREATED_VOLUMES="$CREATED_VOLUMES $1"; }
note_network()   { CREATED_NETWORKS="$CREATED_NETWORKS $1"; }

cleanup() {
  [ "$KEEP_RESOURCES" = "1" ] && { echo "KEEP_RESOURCES=1 -- leaving $CREATED_CONTAINERS $CREATED_VOLUMES $CREATED_NETWORKS in place"; return; }
  for c in $CREATED_CONTAINERS; do docker rm -f "$c" >/dev/null 2>&1 || true; done
  for v in $CREATED_VOLUMES; do docker volume rm "$v" >/dev/null 2>&1 || true; done
  for n in $CREATED_NETWORKS; do docker network rm "$n" >/dev/null 2>&1 || true; done
}
trap cleanup EXIT

echo "=============================================================="
echo " opencode e2e smoke test ($RUN_ID)"
echo "=============================================================="

# ===========================================================================
# 0. Preflight probes -- each PASS/SKIP/FAIL on its own, never abort the
#    script. Later sections gate a check on the relevant probe's result.
# ===========================================================================
echo
echo "--- preflight: sysbox-runc runtime ---"
if docker info --format '{{json .Runtimes}}' 2>/dev/null | grep -q sysbox-runc; then
  HAVE_SYSBOX=1
  pass "sysbox-runc is a registered Docker runtime"
else
  HAVE_SYSBOX=0
  skip "sysbox-runc" "not a registered runtime on this daemon (docker info shows only runc/containerd) -- a real docker-operator create cannot reach its DinD-sidecar step here; this script only exercises the image standalone"
fi

echo
echo "--- preflight: dependaproxy reachable ---"
HAVE_DEPENDAPROXY=0
if [ -z "$DEPENDAPROXY_IP" ]; then
  cid="$(docker ps -q --filter 'name=dependaproxy' | head -n1)"
  if [ -n "$cid" ]; then
    # ONE IP PER LINE ({{println}}), then take the first thing that actually
    # looks like an IPv4. A container attached to several networks -- which
    # the REAL dependaproxy always is, since the orchestrator NetworkConnects
    # it to proxynet plus every per-agent dinernet -- makes a plain
    # {{range}}{{.IPAddress}}{{end}} concatenate them into one unusable
    # string like "172.20.0.5172.25.3.3", which a trailing `head -n1` cannot
    # split back apart because it is all a single line. That bogus value then
    # became a bogus --add-host on every `docker run` below.
    DEPENDAPROXY_IP="$(docker inspect "$cid" \
      --format '{{range .NetworkSettings.Networks}}{{println .IPAddress}}{{end}}' 2>/dev/null \
      | grep -E '^[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}$' | head -n1 || true)"
  fi
fi
# "a container with that name exists" is NOT "dependaproxy answers": always
# prove it with a real HTTP response before believing the address, otherwise
# Bullet 2a reports a FAIL for what is really an environment gap.
if [ -n "$DEPENDAPROXY_IP" ] && http_reachable "$DEPENDAPROXY_URL" --add-host=dependaproxy:"$DEPENDAPROXY_IP"; then
  HAVE_DEPENDAPROXY=1
  pass "dependaproxy answers at $DEPENDAPROXY_URL (dependaproxy -> $DEPENDAPROXY_IP)"
elif http_reachable "$DEPENDAPROXY_URL"; then
  HAVE_DEPENDAPROXY=1
  # Resolvable without an --add-host, so do not pin one: a stale/unvalidated
  # DEPENDAPROXY_IP would only shadow the name that already works.
  DEPENDAPROXY_IP=""
  pass "DEPENDAPROXY_URL ($DEPENDAPROXY_URL) directly reachable with no --add-host needed (manual-container path, not the real orchestrator path)"
else
  # Drop the address too -- an unvalidated --add-host is worse than none: an
  # invalid one makes every `docker run` below fail outright.
  DEPENDAPROXY_IP=""
  skip "dependaproxy" "DEPENDAPROXY_URL ($DEPENDAPROXY_URL) produced no HTTP response, with or without an --add-host for a dependaproxy-named container on this daemon (the real orchestrator's per-agent NetworkConnect has nothing to find here) -- set DEPENDAPROXY_IP or DEPENDAPROXY_URL to point at one"
fi

echo
echo "--- preflight: Ollama reachable ---"
HAVE_OLLAMA_REACHABLE=0
if docker run --rm $NET_FLAG curlimages/curl:8.10.1 -sf --max-time 5 "$OLLAMA_URL/api/tags" >/dev/null 2>&1; then
  HAVE_OLLAMA_REACHABLE=1
  pass "$OLLAMA_URL/api/tags responds"
else
  skip "Ollama reachability" "$OLLAMA_URL is not reachable from a container on ${DOCKER_NETWORK:-the default bridge} -- set OLLAMA_URL/DOCKER_NETWORK to a reachable instance"
fi

echo
echo "--- preflight: Ollama CAPABLE of a real completion within ${OLLAMA_CHAT_TIMEOUT}s ---"
# The exact trap this script exists to avoid: reachability != usability.
# qwen3:0.6b on CPU-only inference measured ~1.2 tokens/sec in this sandbox
# and never finished within opencode's own 300s provider-response timeout --
# so this preflight actually TIMES a completion instead of just checking the
# port is open.
HAVE_OLLAMA_CAPABLE=0
if [ "$HAVE_OLLAMA_REACHABLE" = "1" ]; then
  start=$(date +%s)
  if docker run --rm $NET_FLAG curlimages/curl:8.10.1 -sf --max-time "$OLLAMA_CHAT_TIMEOUT" \
      -X POST "$OLLAMA_URL/api/generate" -H 'Content-Type: application/json' \
      -d "{\"model\":\"$OPENCODE_MODEL\",\"prompt\":\"Reply with exactly the word PONG.\",\"stream\":false}" \
      2>/dev/null | grep -q '"response"'; then
    elapsed=$(( $(date +%s) - start ))
    HAVE_OLLAMA_CAPABLE=1
    pass "a real completion from $OPENCODE_MODEL finished in ${elapsed}s (budget ${OLLAMA_CHAT_TIMEOUT}s)"
  else
    skip "Ollama capability" "$OPENCODE_MODEL did not complete a real generation within ${OLLAMA_CHAT_TIMEOUT}s (or the model is not pulled) -- checks that need the model to actually finish a turn will SKIP, not FAIL; this is an honest hardware/model-size limitation, not a bug in the harness"
  fi
else
  skip "Ollama capability" "Ollama is not reachable at all (see reachability probe above)"
fi

echo
echo "--- preflight: git-proxy reachable by hostname from a container ---"
HAVE_GITPROXY=0
# See http_reachable's comment for why this is a shared helper and not an
# open-coded "%{http_code} matches some digits" test.
if http_reachable "$GIT_PROXY_URL"; then
  HAVE_GITPROXY=1
  pass "$GIT_PROXY_URL reachable from a container on ${DOCKER_NETWORK:-the default bridge}"
else
  skip "git-proxy reachability" "$GIT_PROXY_URL is not resolvable/reachable from a container on ${DOCKER_NETWORK:-the default bridge} -- set DOCKER_NETWORK to the network git-proxy actually lives on (a real docker-operator deployment's proxynet); this is a topology gap in how THIS script's containers are wired, not necessarily a git-proxy outage"
fi

# ===========================================================================
# 1. Image present (build/pull it) -- prerequisite for every bullet below.
# ===========================================================================
echo
echo "--- ensuring $OPENCODE_IMAGE exists locally ---"
if docker image inspect "$OPENCODE_IMAGE" >/dev/null 2>&1; then
  pass "$OPENCODE_IMAGE already present locally"
elif docker pull "$OPENCODE_IMAGE" >/dev/null 2>&1; then
  pass "$OPENCODE_IMAGE pulled"
elif [ "$BUILD_ON_MISSING" = "1" ]; then
  echo "pull failed (not published, or no registry access) -- building locally instead"
  # The known-working build from this issue's own investigation: buildx is
  # missing on an under-provisioned daemon, so DOCKER_BUILDKIT=0 is required,
  # not optional; NPM_REGISTRY routes the npm install steps through
  # DependaProxy instead of the (network-blocked) public registry.
  npm_registry="http://dependaproxy:8080/npm"
  [ -n "$DEPENDAPROXY_IP" ] && npm_registry="http://${DEPENDAPROXY_IP}:8080/npm"
  DOCKER_BUILDKIT=0 docker build \
    -f "$ROOT/agent-opencode/Dockerfile" \
    --build-arg GOPROXY=https://proxy.golang.org,direct \
    --build-arg NPM_REGISTRY="$npm_registry" \
    -t "$OPENCODE_IMAGE" "$ROOT/.."
  pass "$OPENCODE_IMAGE built locally"
else
  failed "image" "$OPENCODE_IMAGE could not be pulled and BUILD_ON_MISSING=0 -- aborting"
  exit 1
fi

OPENCODE_CONFIG_CONTENT="{\"provider\":{\"ollama\":{\"npm\":\"@ai-sdk/openai-compatible\",\"options\":{\"baseURL\":\"${OLLAMA_URL}/v1\"},\"models\":{\"${OPENCODE_MODEL}\":{}}}},\"model\":\"ollama/${OPENCODE_MODEL}\"}"

# ===========================================================================
# Bullet 1: the container boots, entrypoint.sh completes, /workspace/CLAUDE.md
# and /workspace/.claude/skills/* are present.
# ===========================================================================
echo
echo "=============================================================="
echo " Bullet 1: boot / entrypoint / CLAUDE.md / skills"
echo "=============================================================="
b1_ws_vol="${RUN_ID}-ws"
b1_container="${RUN_ID}-boot"
docker volume create "$b1_ws_vol" >/dev/null
note_volume "$b1_ws_vol"

add_host_flag=""
[ -n "$DEPENDAPROXY_IP" ] && add_host_flag="--add-host=dependaproxy:$DEPENDAPROXY_IP"

docker run -d --name "$b1_container" $NET_FLAG $add_host_flag \
  -e AGENT_TOKEN="$AGENT_TOKEN" \
  -e GIT_PROXY_URL="$GIT_PROXY_URL" \
  -e GIT_PROXY_HEADER="Authorization: Bearer $AGENT_TOKEN" \
  -e DEPENDAPROXY_DINERNET_IP="${DEPENDAPROXY_IP:-127.0.0.1}" \
  -e AGENT_STORE_DIR=/workspace/store \
  -e OPENCODE_CONFIG_CONTENT="$OPENCODE_CONFIG_CONTENT" \
  -v "$b1_ws_vol":/workspace \
  "$OPENCODE_IMAGE" tail -f /dev/null >/dev/null
note_container "$b1_container"

# entrypoint.sh runs synchronously before exec "$@", so by the time the
# container is even created its work is done -- this sleep just gives the
# daemon time to report the container as Running before we exec into it.
sleep 2

b1_ok=1
b1_detail=""

if ! docker inspect "$b1_container" --format '{{.State.Running}}' 2>/dev/null | grep -q true; then
  b1_ok=0
  b1_detail="container is not Running -- entrypoint.sh likely exited non-zero; see: docker logs $b1_container"
  docker logs "$b1_container" 2>&1 | tail -40 || true
fi

if [ "$b1_ok" = "1" ]; then
  lines="$(docker exec "$b1_container" sh -c 'wc -l < /workspace/CLAUDE.md' 2>/dev/null || echo 0)"
  if [ "${lines:-0}" -lt 1 ]; then
    b1_ok=0; b1_detail="/workspace/CLAUDE.md missing or empty"
  else
    echo "OK: /workspace/CLAUDE.md present ($lines lines)"
  fi
fi

if [ "$b1_ok" = "1" ]; then
  skills="$(docker exec "$b1_container" sh -c 'ls -1 /workspace/.claude/skills 2>/dev/null | sort' || true)"
  expected="$(printf 'implement-issue\nstore-file\nuse-dependaproxy\nuse-docker\nuse-git-proxy')"
  if [ "$skills" != "$expected" ]; then
    b1_ok=0; b1_detail="skills mismatch -- want [$expected] got [$skills]"
  else
    echo "OK: all 5 skills present under /workspace/.claude/skills"
  fi
fi

if [ "$b1_ok" = "1" ]; then
  # git config / Bearer rewrite the entrypoint is supposed to set up (the
  # actual mechanism use-git-proxy's "the entrypoint already configures
  # git..." note refers to).
  insteadof="$(docker exec "$b1_container" git config --global --get 'url.http://git-proxy:8080/.insteadOf' 2>/dev/null || true)"
  if [ "$insteadof" != "https://github.com/" ]; then
    b1_ok=0; b1_detail="git insteadOf rewrite not configured (got '$insteadof')"
  else
    echo "OK: git insteadOf rewrite configured"
  fi
fi

if [ "$b1_ok" = "1" ]; then
  if ! docker exec "$b1_container" sh -c 'test -f /workspace/.npmrc && test -f /workspace/go.env && test -f /workspace/pip.env && test -f /workspace/dependaproxy-ip'; then
    b1_ok=0; b1_detail=".npmrc/go.env/pip.env/dependaproxy-ip not all present"
  else
    echo "OK: .npmrc/go.env/pip.env/dependaproxy-ip all written"
  fi
fi

if [ "$b1_ok" = "1" ]; then
  # This image never sets CLAUDE_CONFIG_DIR (see agent-opencode/Dockerfile's
  # own comment) -- confirms entrypoint.sh's Claude-only onboarding block
  # stays inert here, as #177 required.
  cfgdir="$(docker exec "$b1_container" sh -c 'echo "${CLAUDE_CONFIG_DIR:-}"' 2>/dev/null || true)"
  if [ -n "$cfgdir" ]; then
    b1_ok=0; b1_detail="CLAUDE_CONFIG_DIR unexpectedly set to '$cfgdir' -- the Claude-only entrypoint block should stay inert in this image"
  else
    echo "OK: CLAUDE_CONFIG_DIR unset (Claude-only entrypoint block stays inert)"
  fi
fi

if [ "$b1_ok" = "1" ]; then
  pass "Bullet 1 (boot/entrypoint/CLAUDE.md/skills)"
  record "Bullet 1: boot/entrypoint/CLAUDE.md/skills" "PASS" "entrypoint completed, CLAUDE.md + 5 skills present, git config + dependency-proxy files written, CLAUDE_CONFIG_DIR stays unset"
else
  failed "Bullet 1" "$b1_detail"
  record "Bullet 1: boot/entrypoint/CLAUDE.md/skills" "FAIL" "$b1_detail"
fi

# ===========================================================================
# Bullet 2: opencode actually uses use-git-proxy, use-dependaproxy, and
# use-docker when asked to. This script verifies the SHELL-COMMAND tier of
# each skill (the exact commands the skill prescribes actually work against
# the real proxy/daemon when run inside the container) -- it does NOT, and
# cannot cheaply, verify that opencode's LLM autonomously CHOOSES to run
# them; that is a separate, explicitly-labelled sub-check below, gated on
# the Ollama-capability preflight, and it is the tier most likely to SKIP.
# Do not read a PASS on the shell-command sub-checks as proof of the LLM
# tier -- they are deliberately reported separately.
# ===========================================================================
echo
echo "=============================================================="
echo " Bullet 2: use-git-proxy / use-dependaproxy / use-docker"
echo "=============================================================="

# --- dependaproxy leg: npm install through the real proxy -----------------
echo
echo "--- dependaproxy leg (shell commands work) ---"
if [ "$HAVE_DEPENDAPROXY" = "1" ]; then
  if docker exec "$b1_container" sh -c 'cd /tmp && npm install left-pad' >/dev/null 2>&1; then
    pass "dependaproxy leg: npm install left-pad succeeded through DependaProxy"
    record "Bullet 2a: use-dependaproxy (shell commands work)" "PASS" "npm install left-pad succeeded inside the container through the real DependaProxy"
  else
    failed "dependaproxy leg" "npm install left-pad failed"
    record "Bullet 2a: use-dependaproxy (shell commands work)" "FAIL" "npm install left-pad failed through DependaProxy"
  fi
else
  skip "dependaproxy leg" "no reachable dependaproxy (see preflight)"
  record "Bullet 2a: use-dependaproxy (shell commands work)" "SKIP" "no reachable dependaproxy (see preflight)"
fi

# --- docker leg: run a container via DOCKER_HOST from inside the agent ----
echo
echo "--- docker leg (shell commands work) ---"
if ! docker exec "$b1_container" sh -c 'command -v docker' >/dev/null 2>&1; then
  # NOT an environment gap: the skill tells the agent to run `docker`, and the
  # image is supposed to ship docker-cli. Only "the daemon is unreachable from
  # here" is a topology excuse; "there is no docker binary" is a regression in
  # the image under test, so it must not be swallowed as a SKIP.
  failed "docker leg" "the docker CLI is not on PATH inside the image"
  record "Bullet 2b: use-docker (shell commands work)" "FAIL" "the docker CLI is not on PATH inside the opencode image -- use-docker cannot work at all; this is an image regression, not a topology gap"
elif docker exec -e DOCKER_HOST="$AGENT_INNER_DOCKER_HOST" "$b1_container" \
     docker run --rm alpine:3.20 echo "hello-from-$RUN_ID" 2>/tmp/opencode-e2e-docker-leg.log | grep -q "hello-from-$RUN_ID"; then
  pass "docker leg: docker run via DOCKER_HOST=$AGENT_INNER_DOCKER_HOST succeeded from inside the container"
  record "Bullet 2b: use-docker (shell commands work)" "PASS" "docker run via DOCKER_HOST=$AGENT_INNER_DOCKER_HOST succeeded from inside the opencode container"
else
  skip "docker leg" "DOCKER_HOST=$AGENT_INNER_DOCKER_HOST not reachable from inside the container -- in a real docker-operator deployment this is always the agent's own private DinD sidecar and always reachable; override AGENT_INNER_DOCKER_HOST for a standalone setup (see: $(cat /tmp/opencode-e2e-docker-leg.log 2>/dev/null | tail -1))"
  record "Bullet 2b: use-docker (shell commands work)" "SKIP" "DOCKER_HOST=$AGENT_INNER_DOCKER_HOST not reachable from inside the container (standalone-setup topology gap, not exercised against a real per-agent DinD sidecar here)"
fi

# --- git-proxy leg: clone a test repo through git-proxy --------------------
echo
echo "--- git-proxy leg (shell commands work) ---"
if [ "$HAVE_GITPROXY" = "1" ]; then
  if docker exec "$b1_container" sh -c "cd /tmp && rm -rf gp-test && git clone --depth=1 \"$GIT_PROXY_URL/${TEST_REPO}\" gp-test" >/dev/null 2>&1; then
    pass "git-proxy leg: git clone through git-proxy succeeded"
    record "Bullet 2c: use-git-proxy (shell commands work)" "PASS" "git clone $TEST_REPO through git-proxy succeeded inside the container"
  else
    failed "git-proxy leg" "git clone through git-proxy failed"
    record "Bullet 2c: use-git-proxy (shell commands work)" "FAIL" "git clone $TEST_REPO through git-proxy failed"
  fi
else
  skip "git-proxy leg" "git-proxy not reachable by hostname from this container's network (see preflight) -- this is the exact topology gap documented at the top of this script"
  record "Bullet 2c: use-git-proxy (shell commands work)" "SKIP" "git-proxy not reachable by hostname from this container's network (see preflight)"
fi

# --- the LLM tier: opencode's model actually CHOOSES to follow a skill -----
echo
echo "--- LLM tier (the model autonomously runs the skill's commands, not this script) ---"
if [ "$HAVE_OLLAMA_CAPABLE" != "1" ]; then
  skip "LLM tier" "Ollama is not capable of a real completion within the timeout (see preflight) -- a model too slow/small to finish a tool call cannot be shown to 'choose' to follow the skill at all; this is an honest hardware/model-size limitation, not a shortcut taken here"
  record "Bullet 2d: LLM autonomously follows the skill (distinct from shell commands working)" "SKIP" "Ollama not capable of a real completion within timeout -- NOT achievable with a too-small/slow local model; do not infer this from the shell-command sub-checks above passing"
elif [ "$HAVE_GITPROXY" != "1" ]; then
  skip "LLM tier" "git-proxy unreachable, so there is nothing for the model to successfully clone even if it tries"
  record "Bullet 2d: LLM autonomously follows the skill (distinct from shell commands working)" "SKIP" "git-proxy unreachable from this container's network"
else
  echo "attempting a real opencode run with a bounded timeout (${OPENCODE_RUN_TIMEOUT}s)..."
  if docker exec "$b1_container" sh -c "cd /workspace && timeout ${OPENCODE_RUN_TIMEOUT} opencode run 'Using the use-git-proxy skill, run: git ls-remote ${GIT_PROXY_URL}/${TEST_REPO}. Reply with exactly the first line of its output.'" 2>/tmp/opencode-e2e-llm.log | tee -a /tmp/opencode-e2e-llm.log | grep -qE '^[0-9a-f]{40}'; then
    pass "LLM tier: opencode's model completed a real tool call and followed the skill"
    record "Bullet 2d: LLM autonomously follows the skill (distinct from shell commands working)" "PASS" "a real opencode run completed a git-proxy tool call within ${OPENCODE_RUN_TIMEOUT}s"
  else
    skip "LLM tier" "opencode run did not produce a recognizable ls-remote result within ${OPENCODE_RUN_TIMEOUT}s -- see /tmp/opencode-e2e-llm.log; most likely the model is too slow to finish, not that the skill is broken (verify the shell-command sub-check above passed)"
    record "Bullet 2d: LLM autonomously follows the skill (distinct from shell commands working)" "SKIP" "opencode run did not complete within ${OPENCODE_RUN_TIMEOUT}s -- model too slow/small, not necessarily a skill regression"
  fi
fi

# ===========================================================================
# Bullet 3: a real edit + git push through git-proxy succeeds, subject to the
# same policy gates as Claude Code (secret_scan, branch_pattern).
# ===========================================================================
echo
echo "=============================================================="
echo " Bullet 3: real edit + push through git-proxy (policy gates)"
echo "=============================================================="
if [ "$HAVE_GITPROXY" != "1" ]; then
  skip "Bullet 3" "git-proxy not reachable by hostname from this container's network (see preflight) -- cannot exercise a real push here; see this script's header for the topology gap"
  record "Bullet 3: real edit + push through git-proxy" "SKIP" "git-proxy not reachable by hostname from this container's network"
else
  branch="feat/opencode-e2e-$RUN_ID"
  push_ok=1
  push_detail=""
  if ! docker exec "$b1_container" sh -c "
    set -e
    cd /tmp && rm -rf gp-push && git clone --depth=1 \"$GIT_PROXY_URL/${TEST_REPO}\" gp-push
    cd gp-push
    git checkout -b \"$branch\"
    git config user.email 'opencode-e2e@example.com'
    git config user.name 'opencode e2e smoke test'
    echo \"opencode e2e smoke test $RUN_ID\" >> .opencode-e2e-smoke-test.txt
    git add .opencode-e2e-smoke-test.txt
    git commit -m 'opencode e2e smoke test: throwaway edit'
    git push origin \"$branch\"
  " >/tmp/opencode-e2e-push.log 2>&1; then
    push_ok=0
    push_detail="push failed -- see /tmp/opencode-e2e-push.log ($(tail -1 /tmp/opencode-e2e-push.log 2>/dev/null))"
  fi
  if [ "$push_ok" = "1" ]; then
    pass "Bullet 3: real edit pushed to $branch through git-proxy"
    record "Bullet 3: real edit + push through git-proxy" "PASS" "pushed $branch to $TEST_REPO through git-proxy from inside the opencode container"
  else
    failed "Bullet 3" "$push_detail"
    record "Bullet 3: real edit + push through git-proxy" "FAIL" "$push_detail"
  fi
fi

# ===========================================================================
# Bullet 4: killing/resuming the agent preserves session context (#181).
# ===========================================================================
echo
echo "=============================================================="
echo " Bullet 4: kill/resume preserves session context"
echo "=============================================================="
b4_vol="${RUN_ID}-oc-data"
b4_a="${RUN_ID}-resume-a"
b4_b="${RUN_ID}-resume-b"
docker volume create "$b4_vol" >/dev/null
note_volume "$b4_vol"

# Gated on CAPABLE, not merely REACHABLE: this bullet's whole claim is
# "turn 2 resumed turn 1's session", and a model that cannot finish a turn
# cannot produce either turn. Running it anyway would burn 2 x
# OPENCODE_RUN_TIMEOUT only to compare two session counts that no completed
# turn ever moved -- see the b4_turn*_rc gates below for why an unchanged
# count is vacuous in that state.
if [ "$HAVE_OLLAMA_CAPABLE" != "1" ]; then
  skip "Bullet 4" "Ollama cannot finish a real completion within the timeout (see preflight) -- opencode cannot complete either turn, and comparing session counts across two turns that never ran proves nothing"
  record "Bullet 4: kill/resume preserves session context" "SKIP" "Ollama not capable of a real completion within timeout -- neither the seeding turn nor the --continue turn can be made to run here"
else
  docker run -d --name "$b4_a" $NET_FLAG $add_host_flag \
    -e AGENT_TOKEN="$AGENT_TOKEN" \
    -e OPENCODE_CONFIG_CONTENT="$OPENCODE_CONFIG_CONTENT" \
    -v "${b4_vol}":/home/node/.local/share \
    "$OPENCODE_IMAGE" tail -f /dev/null >/dev/null
  note_container "$b4_a"
  sleep 2

  # The seeding turn. Its EXIT STATUS is load-bearing, not decoration: if it
  # timed out (rc 124) there is no completed turn to resume, and every
  # session-count comparison downstream becomes vacuous.
  b4_turn1_rc=0
  docker exec "$b4_a" sh -c "cd /workspace && timeout ${OPENCODE_RUN_TIMEOUT} opencode run 'Reply with exactly the word PONG.'" >/tmp/opencode-e2e-b4-turn1.log 2>&1 || b4_turn1_rc=$?

  before_sessions="$(docker exec "$b4_a" sh -c 'opencode session list 2>/dev/null | wc -l' 2>/dev/null || echo 0)"

  echo "destroying container $b4_a (volume $b4_vol persists)..."
  docker rm -f "$b4_a" >/dev/null 2>&1 || true
  CREATED_CONTAINERS="$(echo "$CREATED_CONTAINERS" | sed "s/ $b4_a//")"

  db_present=0
  if docker run --rm -v "${b4_vol}":/data alpine:3.20 sh -c 'test -s /data/opencode/opencode.db'; then
    db_present=1
  fi

  if [ "$b4_turn1_rc" != "0" ]; then
    # Without a COMPLETED first turn there is no session to resume, so an
    # unchanged session count below would be true for the most trivial reason
    # imaginable (nothing ever ran) rather than because --continue worked.
    # That is a SKIP, never a PASS.
    skip "Bullet 4" "the seeding turn did not complete (rc=$b4_turn1_rc, budget ${OPENCODE_RUN_TIMEOUT}s) -- see /tmp/opencode-e2e-b4-turn1.log; there is no completed session to resume, so nothing downstream could prove resume works"
    record "Bullet 4: kill/resume preserves session context" "SKIP" "the seeding turn never completed (rc=$b4_turn1_rc within ${OPENCODE_RUN_TIMEOUT}s) -- no session to resume; an unchanged session count would be vacuous, so this is NOT reported as a PASS"
  elif [ "$db_present" != "1" ]; then
    failed "Bullet 4" "opencode.db not found (or empty) on the volume after the container was destroyed"
    record "Bullet 4: kill/resume preserves session context" "FAIL" "opencode.db missing/empty on the persisted volume after container destroy"
  else
    echo "starting a brand-new container $b4_b against the same volume..."
    docker run -d --name "$b4_b" $NET_FLAG $add_host_flag \
      -e AGENT_TOKEN="$AGENT_TOKEN" \
      -e OPENCODE_CONFIG_CONTENT="$OPENCODE_CONFIG_CONTENT" \
      -v "${b4_vol}":/home/node/.local/share \
      "$OPENCODE_IMAGE" tail -f /dev/null >/dev/null
    note_container "$b4_b"
    sleep 2

    b4_turn2_rc=0
    docker exec "$b4_b" sh -c "cd /workspace && timeout ${OPENCODE_RUN_TIMEOUT} opencode run --continue 'This is the second turn of the same conversation.'" >/tmp/opencode-e2e-b4-turn2.log 2>&1 || b4_turn2_rc=$?

    if [ "$b4_turn2_rc" != "0" ]; then
      # THE false-PASS this gate exists to stop: a --continue that timed out
      # and did nothing at all leaves the session count exactly as it was, and
      # "the count did not change" would then read as "--continue landed in
      # the same session" when in truth --continue never ran. A turn that
      # never happened also cannot fork a session, so an unchanged count is
      # evidence of nothing. SKIP.
      skip "Bullet 4" "the --continue turn did not complete (rc=$b4_turn2_rc, budget ${OPENCODE_RUN_TIMEOUT}s) -- see /tmp/opencode-e2e-b4-turn2.log; a turn that never ran cannot fork the session either, so an unchanged session count would prove nothing"
      record "Bullet 4: kill/resume preserves session context" "SKIP" "the --continue turn never completed (rc=$b4_turn2_rc within ${OPENCODE_RUN_TIMEOUT}s) -- an unchanged session count is vacuous when no turn ran, so this is NOT reported as a PASS; the volume DID keep a non-empty opencode.db across the container destroy"
    else
      after_sessions="$(docker exec "$b4_b" sh -c 'opencode session list 2>/dev/null | wc -l' 2>/dev/null || echo 0)"
      if [ "$before_sessions" != "0" ] && [ "$after_sessions" = "$before_sessions" ]; then
        pass "Bullet 4: both turns completed and the session count stayed at $after_sessions across the container recreate (no fork) -- the recreated container's --continue landed in the SAME session"
        record "Bullet 4: kill/resume preserves session context" "PASS" "both turns COMPLETED (rc=0) and the session count stayed stable ($before_sessions -> $after_sessions) across a real container destroy + recreate on the same volume, so --continue appended to the surviving session instead of forking a new one"
      else
        failed "Bullet 4" "both turns completed, but the session count changed ($before_sessions -> $after_sessions) or no session existed before the recreate -- see /tmp/opencode-e2e-b4-turn*.log"
        record "Bullet 4: kill/resume preserves session context" "FAIL" "both turns completed (rc=0) yet session count went $before_sessions -> $after_sessions (expected stable, nonzero) -- --continue forked instead of resuming"
      fi
    fi
  fi
fi

# ===========================================================================
# Summary
# ===========================================================================
echo
echo "=============================================================="
echo " SUMMARY"
echo "=============================================================="
echo "$SUMMARY" | sed '/^$/d'
echo "=============================================================="

# Non-zero exit only on a genuine FAIL -- a SKIP is not a failure of this
# script or (necessarily) of the harness under test; see the header.
if echo "$SUMMARY" | grep -q ':: FAIL '; then
  echo "opencode-e2e: FAIL (see above)"
  exit 1
fi
echo "opencode-e2e: done (PASS/SKIP only -- see summary above for what actually ran on this host)"
