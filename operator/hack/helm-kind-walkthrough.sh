#!/bin/sh
set -eu
# Helm install/upgrade/uninstall/reinstall walkthrough for the operator
# chart (#34), run against a cluster hack/e2e-up.sh already brought up with
# E2E_DEPLOY=helm (`make E2E_DEPLOY=helm e2e-up`, which installs the release
# named "ai-sandbox-operator" from deploy/helm/ai-sandbox-operator with
# test/e2e/manifests/helm-e2e-values.yaml -- see that file for why its
# fullnameOverride makes the rendered Deployment name identical to the
# kustomize path's).
#
# Five steps, each asserting one acceptance criterion from #34's issue text:
#   1. `helm test` passes against the freshly-installed release.
#   2. Fresh install reconciles a real environment to Done -- reuses
#      test/e2e/lifecycle_test.go's existing "reaches Done" spec via
#      `ginkgo --focus`, rather than reimplementing that assertion here.
#   3. `helm upgrade` (a change that rolls the operator Deployment, e.g. a
#      verbosity/resource bump) does not disrupt an environment already
#      Running: same pod UID, same pod startTime, no new Warning events, and
#      it still reaches Done afterwards.
#   4. `helm uninstall` removes the chart's own objects (Deployment,
#      Service, RBAC) but preserves the CRDs, any still-running
#      SandboxEnvironment and its pod/PVC, and the default SandboxClass (if
#      the fixture created one -- `helm.sh/resource-policy: keep`).
#   5. `helm install` again (reinstall) resumes reconciling that
#      still-Running environment through to Done.
#
# Requires kubectl, helm AND go all on PATH -- unlike hack/e2e-up.sh/
# hack/e2e-down.sh/hack/e2e-collect.sh (kubectl/helm/kind only), step 2 shells
# out to `go run .../ginkgo`. This is true unconditionally on a GitHub
# Actions runner (IN_CONTAINER=1: actions/setup-go put go on PATH,
# azure/setup-helm put helm on PATH, hack/fetch-e2e-tools.sh put
# kind/kubectl on PATH -- see .github/workflows/helm.yml's
# kind-install-upgrade job) but is NOT true when `make helm-kind` wraps this
# script in E2E_TOOLBOX_RUN's docker:28-cli image (no Go there) -- that local/
# sandbox path is not required to work; see operator/Makefile's helm-kind
# target comment.
#
# Idempotent-ish: re-running against a cluster left over from a prior partial
# run may fail at the "expect gone"/"expect present" assertions if that
# prior run did not reach the matching step. `make helm-kind` is meant to
# follow a fresh `make E2E_DEPLOY=helm e2e-up` every time (see the kind
# cluster's own idempotent-restart behavior in hack/e2e-up.sh).

RELEASE="ai-sandbox-operator"
NAMESPACE="ai-sandbox-operator-system"
FULLNAME="ai-sandbox-operator-controller-manager"
CHART="deploy/helm/ai-sandbox-operator"
VALUES="test/e2e/manifests/helm-e2e-values.yaml"

E2E_SERVICES_NS="${E2E_SERVICES_NS:-ai-sandbox-e2e}"
E2E_BUCKET="${E2E_BUCKET:-sandbox-snapshots}"
E2E_AGENT_IMAGE="${E2E_AGENT_IMAGE:-ai-sandbox-e2e-agent:test}"
E2E_KUBECONFIG="${E2E_KUBECONFIG:-$PWD/.e2e-kubeconfig}"
# Generous relative to the sleep durations apply_class_and_env uses below:
# wait_phase only blocks until the condition is met (or this ceiling), so a
# larger timeout costs nothing when the phase transition is prompt.
E2E_PHASE_TIMEOUT="${E2E_PHASE_TIMEOUT:-400s}"

ENV_NS="helm-walkthrough"
CLASS_NAME="helm-walkthrough-class"

export KUBECONFIG="$E2E_KUBECONFIG"

# ---------------------------------------------------------------------------
# Diagnostics on any non-zero exit -- EXIT is the one POSIX-portable trap
# (dash, this script's likely /bin/sh, has no ERR trap), so this fires for
# both an explicit `fail`/`exit 1` below and any bare command failure under
# `set -e`.
# ---------------------------------------------------------------------------
dump_diagnostics() {
  echo "---- operator deployment describe ($NAMESPACE/$FULLNAME) ----" >&2
  kubectl -n "$NAMESPACE" describe deployment "$FULLNAME" >&2 2>&1 || true
  echo "---- operator pod logs (tail 200) ----" >&2
  kubectl -n "$NAMESPACE" logs "deployment/$FULLNAME" --tail=200 >&2 2>&1 || true
  echo "---- sandbox custom resources ----" >&2
  kubectl get sbenv,sbclass -A -o wide >&2 2>&1 || true
  echo "---- recent events (all namespaces) ----" >&2
  kubectl get events -A --sort-by=.lastTimestamp >&2 2>&1 | tail -n 60 >&2 || true
}

finish() {
  rc=$?
  if [ "$rc" -ne 0 ]; then
    echo "==> helm-kind-walkthrough.sh FAILED (rc=$rc) -- dumping diagnostics" >&2
    dump_diagnostics
  fi
  exit "$rc"
}
trap finish EXIT

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

ok() {
  echo "OK: $*"
}

# wait_phase polls a SandboxEnvironment's status.phase via `kubectl wait`
# (jsonpath support, kubectl >=1.23).
wait_phase() {
  ns="$1"; name="$2"; phase="$3"; timeout="$4"
  kubectl -n "$ns" wait "sandboxenvironment/$name" \
    --for="jsonpath={.status.phase}=$phase" --timeout="$timeout" \
    || fail "sandboxenvironment $ns/$name never reached phase $phase"
}

agent_pod_name() {
  ns="$1"; env_name="$2"
  kubectl -n "$ns" get pods \
    -l "sandbox.psenna.dev/environment=$env_name" \
    -o jsonpath='{.items[0].metadata.name}'
}

apply_class_and_env() {
  env_name="$1"; sleep_seconds="$2"
  kubectl get namespace "$ENV_NS" >/dev/null 2>&1 || kubectl create namespace "$ENV_NS"

  cat <<EOF | kubectl apply -f -
apiVersion: sandbox.psenna.dev/v1alpha1
kind: SandboxClass
metadata:
  name: $CLASS_NAME
spec:
  agent:
    image: $E2E_AGENT_IMAGE
    resources:
      requests: {cpu: 10m, memory: 64Mi}
  engine:
    type: none
  storage:
    workspace: {size: 128Mi}
    backend:
      type: s3
      s3:
        endpoint: http://minio.$E2E_SERVICES_NS.svc.cluster.local:9000
        bucket: $E2E_BUCKET
        credentialsSecretRef: {name: minio-creds, key: token}
  network:
    isolation: Open
  timeouts: {running: 1h, waiting: 1h, total: 2h}
EOF

  cat <<EOF | kubectl apply -f -
apiVersion: sandbox.psenna.dev/v1alpha1
kind: SandboxEnvironment
metadata:
  name: $env_name
  namespace: $ENV_NS
spec:
  classRef: {name: $CLASS_NAME}
  repo: psenna/e2e-fixture
  task:
    prompt: |
      SCRIPT:sleep $sleep_seconds
      SCRIPT:write hello.txt world
      SCRIPT:exit 0
EOF
}

echo "==================================================================="
echo "1. helm test"
echo "==================================================================="
helm test "$RELEASE" -n "$NAMESPACE" --logs
ok "helm test passed"

echo "==================================================================="
echo "2. fresh install reconciles to Done (ginkgo --focus)"
echo "==================================================================="
(
  cd test/e2e
  KUBECONFIG="$E2E_KUBECONFIG" \
  E2E_ARTIFACT_DIR="${E2E_ARTIFACT_DIR:-$PWD/_artifacts}" \
    go run github.com/onsi/ginkgo/v2/ginkgo \
      --focus "reaches Done for a script that writes a file and exits 0" \
      --procs=1 --timeout=5m -v .
) || fail "ginkgo focused 'reaches Done' spec did not pass against the Helm-installed operator"
ok "fresh Helm install reconciled a real environment to Done"

echo "==================================================================="
echo "3. helm upgrade does not disrupt a running environment"
echo "==================================================================="
UPGRADE_ENV="upgrade-probe"
# 180s: generous headroom for wait-Running + record-state + `helm upgrade
# --wait` (pod terminate/reschedule/probe-settle) + the post-upgrade
# assertions below to all complete before the script itself exits and the
# environment moves past Running.
apply_class_and_env "$UPGRADE_ENV" 180
wait_phase "$ENV_NS" "$UPGRADE_ENV" Running "$E2E_PHASE_TIMEOUT"
ok "environment $ENV_NS/$UPGRADE_ENV reached Running before the upgrade"

# The thing that must NOT change across the upgrade is the SANDBOX's own
# agent pod -- the operator Deployment is EXPECTED to roll (new operator
# pod, new args), but that must have zero effect on a pod the operator does
# not own via that Deployment.
AGENT_POD_BEFORE="$(agent_pod_name "$ENV_NS" "$UPGRADE_ENV")"
UID_BEFORE="$(kubectl -n "$ENV_NS" get pod "$AGENT_POD_BEFORE" -o jsonpath='{.metadata.uid}')"
START_BEFORE="$(kubectl -n "$ENV_NS" get pod "$AGENT_POD_BEFORE" -o jsonpath='{.status.startTime}')"
ENV_RV_BEFORE="$(kubectl -n "$ENV_NS" get "sandboxenvironment/$UPGRADE_ENV" -o jsonpath='{.metadata.resourceVersion}')"
WARN_BEFORE="$(kubectl -n "$ENV_NS" get events -o json | jq '[.items[] | select(.type=="Warning")] | length')"
echo "before upgrade: agentPod=$AGENT_POD_BEFORE uid=$UID_BEFORE startTime=$START_BEFORE env.resourceVersion=$ENV_RV_BEFORE warningEvents=$WARN_BEFORE"

# A change that rolls the operator Deployment (new operator pod) but touches
# nothing sandbox-related: -f re-supplies the e2e values (helm upgrade does
# NOT reuse the previous release's values unless --reuse-values is passed),
# the --set overrides bump logging.verbosity and the CPU limit.
helm upgrade "$RELEASE" "$CHART" \
  --namespace "$NAMESPACE" -f "$VALUES" \
  --set logging.verbosity=1 --set resources.limits.cpu=600m \
  --wait --timeout 300s
ok "helm upgrade completed and its own --wait rollout succeeded"

kubectl -n "$NAMESPACE" rollout status "deployment/$FULLNAME" --timeout=300s \
  || fail "operator deployment rollout did not settle after helm upgrade"

AGENT_POD_AFTER="$(agent_pod_name "$ENV_NS" "$UPGRADE_ENV")"
UID_AFTER="$(kubectl -n "$ENV_NS" get pod "$AGENT_POD_AFTER" -o jsonpath='{.metadata.uid}')"
START_AFTER="$(kubectl -n "$ENV_NS" get pod "$AGENT_POD_AFTER" -o jsonpath='{.status.startTime}')"
echo "after upgrade: agentPod=$AGENT_POD_AFTER uid=$UID_AFTER startTime=$START_AFTER"

[ "$AGENT_POD_AFTER" = "$AGENT_POD_BEFORE" ] || fail "the environment's agent pod NAME changed across the operator's helm upgrade ($AGENT_POD_BEFORE -> $AGENT_POD_AFTER) -- it should never have been touched"
[ "$UID_AFTER" = "$UID_BEFORE" ] || fail "the environment's agent pod UID changed across the operator's helm upgrade ($UID_BEFORE -> $UID_AFTER): the agent pod was recreated, but this upgrade touches nothing sandbox-related"
[ "$START_AFTER" = "$START_BEFORE" ] || fail "the environment's agent pod startTime changed across the operator's helm upgrade ($START_BEFORE -> $START_AFTER)"
ok "agent pod identity (name/UID/startTime) unchanged across the operator's helm upgrade"

PHASE_AFTER="$(kubectl -n "$ENV_NS" get "sandboxenvironment/$UPGRADE_ENV" -o jsonpath='{.status.phase}')"
[ "$PHASE_AFTER" = "Running" ] || fail "environment $ENV_NS/$UPGRADE_ENV left Running during the helm upgrade (now: $PHASE_AFTER) -- the upgrade must not disrupt it"
ok "environment $ENV_NS/$UPGRADE_ENV is still Running after the upgrade"

WARN_AFTER="$(kubectl -n "$ENV_NS" get events -o json | jq '[.items[] | select(.type=="Warning")] | length')"
[ "$WARN_AFTER" -le "$WARN_BEFORE" ] || fail "new Warning events appeared against $ENV_NS during the upgrade ($WARN_BEFORE -> $WARN_AFTER)"
ok "no new Warning events in $ENV_NS during the upgrade"

wait_phase "$ENV_NS" "$UPGRADE_ENV" Done "$E2E_PHASE_TIMEOUT"
ok "environment $ENV_NS/$UPGRADE_ENV went on to reach Done after the upgrade"

echo "==================================================================="
echo "4. helm uninstall preserves workloads"
echo "==================================================================="
SURVIVOR_ENV="uninstall-survivor"
# 240s: headroom for wait-Running + the uninstall assertions (several
# sequential kubectl gets) + `helm install` reinstall (--wait) to all
# complete before the script itself exits and the environment moves past
# Running.
apply_class_and_env "$SURVIVOR_ENV" 240
wait_phase "$ENV_NS" "$SURVIVOR_ENV" Running "$E2E_PHASE_TIMEOUT"
ok "environment $ENV_NS/$SURVIVOR_ENV reached Running before uninstall"

DEFAULT_CLASS_PRESENT=0
kubectl get sandboxclass default >/dev/null 2>&1 && DEFAULT_CLASS_PRESENT=1

helm uninstall "$RELEASE" -n "$NAMESPACE"
ok "helm uninstall completed"

if kubectl -n "$NAMESPACE" get "deployment/$FULLNAME" >/dev/null 2>&1; then
  fail "operator Deployment $NAMESPACE/$FULLNAME survived helm uninstall"
fi
ok "operator Deployment is gone"

if kubectl -n "$NAMESPACE" get "service/$FULLNAME-metrics" >/dev/null 2>&1; then
  fail "operator metrics Service $NAMESPACE/$FULLNAME-metrics survived helm uninstall"
fi
ok "operator metrics Service is gone"

if kubectl get clusterrole "$FULLNAME" >/dev/null 2>&1 || kubectl get clusterrolebinding "$FULLNAME" >/dev/null 2>&1; then
  fail "operator ClusterRole/ClusterRoleBinding survived helm uninstall"
fi
ok "operator ClusterRole/ClusterRoleBinding are gone"

if kubectl -n "$NAMESPACE" get "role/$FULLNAME-leader-election" >/dev/null 2>&1 || kubectl -n "$NAMESPACE" get "rolebinding/$FULLNAME-leader-election" >/dev/null 2>&1; then
  fail "leader-election Role/RoleBinding survived helm uninstall"
fi
ok "leader-election Role/RoleBinding are gone"

kubectl get crd sandboxclasses.sandbox.psenna.dev sandboxenvironments.sandbox.psenna.dev >/dev/null 2>&1 \
  || fail "the two SandboxClass/SandboxEnvironment CRDs did not survive helm uninstall"
ok "both CRDs survived uninstall (Helm never deletes crds/)"

SURVIVOR_PHASE="$(kubectl -n "$ENV_NS" get "sandboxenvironment/$SURVIVOR_ENV" -o jsonpath='{.status.phase}' 2>/dev/null || echo "MISSING")"
[ "$SURVIVOR_PHASE" != "MISSING" ] || fail "SandboxEnvironment $ENV_NS/$SURVIVOR_ENV was deleted by helm uninstall -- workloads it does not own must survive"
ok "SandboxEnvironment $ENV_NS/$SURVIVOR_ENV survived uninstall (phase: $SURVIVOR_PHASE)"

kubectl -n "$ENV_NS" get pod -l "sandbox.psenna.dev/environment=$SURVIVOR_ENV" --no-headers 2>/dev/null | grep -q . \
  || fail "the surviving environment's agent pod is gone after helm uninstall"
ok "the surviving environment's agent pod survived uninstall"

kubectl -n "$ENV_NS" get pvc -l "sandbox.psenna.dev/environment=$SURVIVOR_ENV" --no-headers 2>/dev/null | grep -q . \
  || fail "the surviving environment's workspace PVC is gone after helm uninstall"
ok "the surviving environment's workspace PVC survived uninstall"

if [ "$DEFAULT_CLASS_PRESENT" -eq 1 ]; then
  kubectl get sandboxclass default >/dev/null 2>&1 \
    || fail "the default SandboxClass was deleted by helm uninstall despite helm.sh/resource-policy: keep"
  ok "the default SandboxClass survived uninstall (resource-policy: keep)"
else
  echo "SKIP: values fixture has defaultClass.create=false -- no default SandboxClass to assert on"
fi

echo "==================================================================="
echo "5. helm install (reinstall) resumes the surviving environment"
echo "==================================================================="
helm install "$RELEASE" "$CHART" \
  --namespace "$NAMESPACE" --create-namespace \
  -f "$VALUES" --wait --timeout 300s
ok "helm reinstall completed"

kubectl -n "$NAMESPACE" rollout status "deployment/$FULLNAME" --timeout=300s \
  || fail "operator deployment did not become ready after reinstall"

wait_phase "$ENV_NS" "$SURVIVOR_ENV" Done "$E2E_PHASE_TIMEOUT"
ok "environment $ENV_NS/$SURVIVOR_ENV resumed under the reinstalled operator and reached Done"

echo "==================================================================="
echo "helm-kind-walkthrough.sh: ALL STEPS PASSED"
echo "==================================================================="
