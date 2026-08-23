#!/bin/sh
set -eu
# Extracts every ```sh quickstart fenced block from operator/README.md, in
# document order, and runs them as ONE shell script against a real kind
# cluster -- so the quickstart in the README is executed as a documentation
# test (#35), not hand-kept-in-sync with a second copy that can silently
# drift.
#
# operator/README.md's `sh quickstart`-tagged fenced blocks are the single
# source of truth for what this script runs; it duplicates none of their
# content. Needs docker, kind, kubectl and helm on PATH.
#
# Like operator/hack/helm-kind-walkthrough.sh, this is only fully supported
# under IN_CONTAINER=1 (a CI runner, or any shell with all four tools
# already on PATH) -- docker:28-cli under E2E_TOOLBOX_RUN cannot address a
# kind API server the way this sandbox's nested DinD requires. Not required
# to pass locally; see operator/Makefile's quickstart-check target comment.
#
# Strict POSIX sh, no pipefail (dash on Ubuntu has no pipefail, same
# constraint hack/e2e-up.sh documents).

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
DOC="${QUICKSTART_DOC:-$REPO_ROOT/operator/README.md}"
CLUSTER="${QUICKSTART_CLUSTER:-ai-sandbox-quickstart}"
MIN_BLOCKS="${QUICKSTART_MIN_BLOCKS:-10}"
ARTIFACTS="${E2E_ARTIFACT_DIR:-$REPO_ROOT/operator/test/e2e/_artifacts}/quickstart"
SCRIPT="$(mktemp)"

# ---------------------------------------------------------------------------
# Diagnostics on any non-zero exit -- shape copied from
# hack/helm-kind-walkthrough.sh's own dump_diagnostics/finish/EXIT-trap
# idiom. EXIT is the one POSIX-portable trap (dash has no ERR trap).
#
# NOTE the Deployment name: the quickstart's `helm install
# ai-sandbox-operator ...` sets no fullnameOverride, so the chart's
# fullname helper resolves to the release name itself --
# "ai-sandbox-operator", NOT the "ai-sandbox-operator-controller-manager"
# that hack/helm-kind-walkthrough.sh uses (that one comes from
# test/e2e/manifests/helm-e2e-values.yaml's explicit fullnameOverride).
# Getting this wrong makes every kubectl below a silent no-op behind
# `|| true`, i.e. an empty diagnostics bundle exactly when it is needed.
# ---------------------------------------------------------------------------
OPERATOR_NS="ai-sandbox-operator-system"
OPERATOR_DEPLOY="ai-sandbox-operator"

dump_diagnostics() {
  mkdir -p "$ARTIFACTS"
  echo "---- operator deployment describe ----" | tee "$ARTIFACTS/deployment-describe.txt" >&2
  kubectl -n "$OPERATOR_NS" describe deployment "$OPERATOR_DEPLOY" \
    2>&1 | tee -a "$ARTIFACTS/deployment-describe.txt" >&2 || true
  echo "---- operator pod logs (tail 200) ----" | tee "$ARTIFACTS/operator-logs.txt" >&2
  kubectl -n "$OPERATOR_NS" logs "deployment/$OPERATOR_DEPLOY" --tail=200 \
    2>&1 | tee -a "$ARTIFACTS/operator-logs.txt" >&2 || true
  echo "---- sandbox custom resources ----" | tee "$ARTIFACTS/sandbox-crs.txt" >&2
  kubectl get sbenv,sbclass -A -o wide 2>&1 | tee -a "$ARTIFACTS/sandbox-crs.txt" >&2 || true
  echo "---- agent pod describe ----" | tee "$ARTIFACTS/agent-pod-describe.txt" >&2
  kubectl -n ai-sandbox-quickstart describe pod -l sandbox.psenna.dev/environment=hello \
    2>&1 | tee -a "$ARTIFACTS/agent-pod-describe.txt" >&2 || true
  echo "---- recent events (all namespaces, tail 80) ----" | tee "$ARTIFACTS/events.txt" >&2
  kubectl get events -A --sort-by=.lastTimestamp 2>&1 | tail -n 80 | tee -a "$ARTIFACTS/events.txt" >&2 || true
}

finish() {
  rc=$?
  if [ "$rc" -ne 0 ]; then
    echo "==> quickstart-check.sh FAILED (rc=$rc) -- dumping diagnostics" >&2
    dump_diagnostics
  fi
  kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
  rm -f "$SCRIPT"
  exit "$rc"
}
trap finish EXIT

# 1. Extract every ```<lang> quickstart fenced block, in document order.
awk -v min="$MIN_BLOCKS" '
  /^```[A-Za-z]*[ \t]+quickstart[ \t]*$/ { infence=1; n++; printf("\n# ---- quickstart block %d ----\n", n); next }
  infence && /^```[ \t]*$/               { infence=0; next }
  infence                                { print; next }
  END {
    if (infence)  { print "quickstart-check: unterminated fenced block" > "/dev/stderr"; exit 3 }
    if (n < min)  { printf("quickstart-check: found only %d `sh quickstart` blocks, expected at least %d -- did a doc restructure drop the tags?\n", n, min) > "/dev/stderr"; exit 3 }
    printf("quickstart-check: extracted %d blocks\n", n) > "/dev/stderr"
  }
' "$DOC" > "$SCRIPT"

# 2. Run them as ONE shell, from the repo root, so `export`s and cwd carry
#    across blocks exactly as they do for a reader pasting them into one
#    terminal.
echo "==> executing the quickstart, verbatim, from $DOC"
cd "$REPO_ROOT"
sh -eux "$SCRIPT"
echo "==> quickstart-check.sh: the documented quickstart ran end to end"
