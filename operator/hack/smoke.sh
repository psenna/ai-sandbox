#!/bin/sh
set -eu
# Binary-level smoke test: the manager starts outside a cluster, serves
# /healthz and /readyz, and exits 0 on SIGTERM. See issue #16 AC #3.

TMPDIR="$(mktemp -d)"
trap 'rm -rf "$TMPDIR"' EXIT

cat > "$TMPDIR/kubeconfig" <<EOF
apiVersion: v1
kind: Config
clusters: [{name: fake, cluster: {server: https://127.0.0.1:6443}}]
contexts: [{name: fake, context: {cluster: fake, user: fake}}]
current-context: fake
users: [{name: fake, user: {}}]
EOF

export KUBECONFIG="$TMPDIR/kubeconfig"

./bin/manager --leader-elect=false --metrics-bind-address=0 \
  --health-probe-bind-address=127.0.0.1:18081 &
pid=$!

ok=0
for i in $(seq 1 30); do
  if curl -sf http://127.0.0.1:18081/healthz >/dev/null 2>&1; then ok=1; break; fi
  sleep 1
done
[ "$ok" = "1" ] || { echo "FAIL: /healthz never became ready"; kill "$pid" 2>/dev/null || true; exit 1; }

curl -sf http://127.0.0.1:18081/readyz >/dev/null || { echo "FAIL: /readyz not OK"; kill "$pid" 2>/dev/null || true; exit 1; }

echo "OK: /healthz and /readyz responding"

kill -TERM "$pid"
if wait "$pid"; then rc=0; else rc=$?; fi

if [ "$rc" -ne 0 ]; then
  echo "FAIL: manager exited $rc after SIGTERM, expected 0"
  exit 1
fi
echo "OK: clean shutdown on SIGTERM"
