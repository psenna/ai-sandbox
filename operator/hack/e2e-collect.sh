#!/bin/sh
set -eu
# Collects cluster-wide diagnostics for the operator's e2e suite. Meant to
# run with `if: always()` in CI (and any time locally after a failure) --
# every step is best-effort (`|| true`): this must produce SOMETHING useful
# even if the kind cluster never came up, the operator never started, or
# the Ginkgo suite never ran at all.
#
# Secrets are projected to NAME + KEY NAMES ONLY, never decoded values --
# matching the invariant internal/controller/secretleak_test.go protects on
# the operator side (see also test/e2e/diagnostics.go's DumpDiagnostics,
# this script's per-spec, in-process counterpart).

E2E_CLUSTER="${E2E_CLUSTER:-ai-sandbox-e2e}"
E2E_INTERNAL_KUBECONFIG="${E2E_INTERNAL_KUBECONFIG:-}"
DIR="${E2E_ARTIFACT_DIR:-$PWD/test/e2e/_artifacts}/cluster-collect"

mkdir -p "$DIR"
cd "$DIR"

# Same fix as hack/e2e-up.sh: when this script runs as a sibling container
# to the kind node (this sandbox), the default kubeconfig's 127.0.0.1
# addressing is unreachable -- join the kind network and use the internal,
# container-name-addressed kubeconfig for every kubectl call below.
if [ -n "$E2E_INTERNAL_KUBECONFIG" ]; then
  docker network connect kind "$(hostname)" 2>/dev/null || true
  KUBECONFIG="$(mktemp)"
  export KUBECONFIG
  kind get kubeconfig --name "$E2E_CLUSTER" --internal >"$KUBECONFIG" 2>/dev/null || true
fi

echo "==> cluster-info dump"
kubectl cluster-info dump --all-namespaces --output-directory "$DIR/cluster-dump" || true

echo "==> events"
kubectl get events -A --sort-by=.lastTimestamp >events.txt 2>&1 || true

echo "==> describe pods"
kubectl describe pods -A >pods-describe.txt 2>&1 || true

echo "==> sandbox custom resources"
kubectl get sbenv,sbclass -A -o yaml >sandbox-resources.yaml 2>&1 || true

echo "==> operator deployment logs"
kubectl -n ai-sandbox-operator-system logs deployment/ai-sandbox-operator-controller-manager --tail=-1 \
  >operator-manager.log 2>&1 || true
kubectl -n ai-sandbox-operator-system logs deployment/ai-sandbox-operator-controller-manager --tail=-1 --previous \
  >operator-manager.previous.log 2>&1 || true

echo "==> secrets (NAME + KEY NAMES ONLY -- never decoded values)"
# NOT `-o custom-columns=...,KEYS:.data` -- .data is a map of key->base64
# VALUE, so that projection would print every secret's (encoded, but real)
# values, not just its key names. A go-template map-range prints only the
# keys ($k), never $v, with no jq/external-tool dependency.
kubectl get secrets -A -o go-template='{{range .items}}{{.metadata.namespace}}{{"	"}}{{.metadata.name}}{{"	"}}{{range $k, $v := .data}}{{$k}}{{","}}{{end}}{{"\n"}}{{end}}' \
  >secrets-key-names.txt 2>&1 || true

echo "==> docker logs for the kind control-plane container"
docker logs "${E2E_CLUSTER}-control-plane" >kind-control-plane.log 2>&1 || true

echo "==> kind export logs"
kind export logs "$DIR/kind-logs" --name "$E2E_CLUSTER" || true

echo "==> diagnostics collected at $DIR"
