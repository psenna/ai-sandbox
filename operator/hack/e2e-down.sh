#!/bin/sh
set -eu
# Tears down the operator's e2e kind cluster and removes the kubeconfig
# hack/e2e-up.sh wrote. Idempotent: safe to run even if the cluster or
# kubeconfig is already gone.

E2E_CLUSTER="${E2E_CLUSTER:-ai-sandbox-e2e}"
E2E_KUBECONFIG="${E2E_KUBECONFIG:-$PWD/.e2e-kubeconfig}"

kind delete cluster --name "$E2E_CLUSTER" || true
rm -f "$E2E_KUBECONFIG"
echo "==> e2e cluster $E2E_CLUSTER torn down"
