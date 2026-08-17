#!/bin/sh
set -eu
# Stands up the operator's e2e kind cluster: creates the cluster and builds
# the three test images in parallel, loads them into the cluster (no
# registry/ghcr.io credentials needed -- see `kind load docker-image`),
# deploys the operator + MinIO + the platform doubles, waits for everything
# to roll out, and writes a kubeconfig for the Ginkgo suite to use.
#
# Run via `make e2e-up` (from operator/), either wrapped inside the
# E2E_TOOLBOX_IMAGE (docker:28-cli, whose /bin/sh is ash) or -- under
# IN_CONTAINER=1, e.g. CI -- executed directly by the runner's own /bin/sh
# (dash on Ubuntu, which does NOT support `set -o pipefail`, unlike ash).
# This script must therefore stay strict POSIX `sh` with no `-o pipefail`
# and no other bashisms; it has no actual `cmd1 | cmd2` pipeline anywhere,
# so pipefail semantics were never needed here in the first place.
#
# Idempotent: any existing cluster of the same name is deleted first, so
# re-running this script is a full restart, not a no-op.

E2E_CLUSTER="${E2E_CLUSTER:-ai-sandbox-e2e}"
E2E_CNI="${E2E_CNI:-kindnet}"
KIND_NODE_IMAGE="${KIND_NODE_IMAGE:-kindest/node:v1.34.0}"
MINIO_IMAGE="${MINIO_IMAGE:-minio/minio:RELEASE.2025-09-07T16-13-09Z}"
E2E_KUBECONFIG="${E2E_KUBECONFIG:-$PWD/.e2e-kubeconfig}"
E2E_INTERNAL_KUBECONFIG="${E2E_INTERNAL_KUBECONFIG:-}"

OPERATOR_IMAGE="ai-sandbox-operator:e2e"
AGENT_IMAGE="ai-sandbox-e2e-agent:test"
DOUBLES_IMAGE="ai-sandbox-e2e-doubles:test"

if [ "$E2E_CNI" = "calico" ]; then
  KIND_CONFIG="test/e2e/manifests/kind-cluster-calico.yaml"
else
  KIND_CONFIG="test/e2e/manifests/kind-cluster.yaml"
fi

echo "==> deleting any existing cluster named $E2E_CLUSTER (idempotent restart)"
kind delete cluster --name "$E2E_CLUSTER" >/dev/null 2>&1 || true

echo "==> creating cluster + building images in parallel"
kind create cluster --name "$E2E_CLUSTER" --image "$KIND_NODE_IMAGE" --config "$KIND_CONFIG" --wait 120s &
kind_pid=$!

(
  set -eu
  echo "--> building $OPERATOR_IMAGE"
  docker buildx build --load -f Dockerfile -t "$OPERATOR_IMAGE" .
  echo "--> building $AGENT_IMAGE"
  docker buildx build --load -f test/e2e/agent/Dockerfile -t "$AGENT_IMAGE" test/e2e/agent
  echo "--> building $DOUBLES_IMAGE"
  docker buildx build --load -f test/e2e/doubles/Dockerfile -t "$DOUBLES_IMAGE" test/e2e/doubles
  echo "--> pulling $MINIO_IMAGE"
  docker pull "$MINIO_IMAGE"
) &
build_pid=$!

wait "$kind_pid"
echo "==> cluster ready"
wait "$build_pid"
echo "==> images ready"

if [ -n "$E2E_INTERNAL_KUBECONFIG" ]; then
  # This script itself is running inside a container (E2E_TOOLBOX_RUN),
  # sibling to the kind node container on the daemon it targets rather
  # than colocated on its network namespace -- as is always the case when
  # DOCKER_HOST points at a remote/nested DinD daemon (this sandbox; a
  # single-daemon CI runner is not in this situation, see the Makefile's
  # E2E_INTERNAL_KUBECONFIG default). `kind create cluster`'s own merged
  # kubeconfig addresses the API server at 127.0.0.1:<mapped-port>, which
  # is only reachable from the DAEMON's own host network namespace -- not
  # from this sibling container. Fix: join the "kind" docker network
  # (created by `kind create cluster` above, hence done only now) and use
  # the --internal kubeconfig (`https://<cluster>-control-plane:6443`),
  # resolvable by Docker's embedded DNS on that network, for every kubectl
  # call this script makes from here on.
  docker network connect kind "$(hostname)" 2>/dev/null || true
  export KUBECONFIG
  KUBECONFIG="$(mktemp)"
  kind get kubeconfig --name "$E2E_CLUSTER" --internal >"$KUBECONFIG"
fi

if [ "$E2E_CNI" = "calico" ]; then
  echo "==> applying Calico (UNTESTED end-to-end, see kind-cluster-calico.yaml)"
  kubectl apply -f https://raw.githubusercontent.com/projectcalico/calico/v3.30.2/manifests/calico.yaml
  kubectl -n kube-system rollout status ds/calico-node --timeout=300s
fi

echo "==> loading images into the cluster"
kind load docker-image --name "$E2E_CLUSTER" "$OPERATOR_IMAGE" "$AGENT_IMAGE" "$DOUBLES_IMAGE" "$MINIO_IMAGE"

echo "==> deploying operator + MinIO + platform doubles"
kubectl apply -k test/e2e/manifests/operator
kubectl apply -f test/e2e/manifests/minio.yaml -f test/e2e/manifests/doubles.yaml

echo "==> waiting for rollouts"
kubectl -n ai-sandbox-operator-system rollout status deployment/ai-sandbox-operator-controller-manager --timeout=300s
kubectl -n ai-sandbox-e2e rollout status deployment/minio --timeout=300s
kubectl -n ai-sandbox-e2e rollout status deployment/platform-doubles --timeout=300s
kubectl -n ai-sandbox-e2e wait --for=condition=complete job/minio-bootstrap --timeout=180s

echo "==> writing kubeconfig to $E2E_KUBECONFIG"
if [ -n "$E2E_INTERNAL_KUBECONFIG" ]; then
  kind get kubeconfig --name "$E2E_CLUSTER" --internal >"$E2E_KUBECONFIG"
else
  kind get kubeconfig --name "$E2E_CLUSTER" >"$E2E_KUBECONFIG"
fi

echo "==> e2e platform is up (cluster: $E2E_CLUSTER, cni: $E2E_CNI)"
