#!/bin/sh
set -eu
# Fetches real kind and kubectl binaries for the e2e suite, over the same
# curl-from-GitHub-releases route hack/fetch-envtest.sh already uses.
# Idempotent: skips the download entirely once $DEST/kind is executable.

KIND_VERSION="${1:?usage: fetch-e2e-tools.sh <kind-version> <kubectl-version> <dest-dir>}"
KUBECTL_VERSION="${2:?usage: fetch-e2e-tools.sh <kind-version> <kubectl-version> <dest-dir>}"
DEST="${3:?usage: fetch-e2e-tools.sh <kind-version> <kubectl-version> <dest-dir>}"
ARCH="${E2E_TOOLS_ARCH:-amd64}"

if [ -x "$DEST/kind" ] && [ -x "$DEST/kubectl" ]; then
  echo "e2e tools already present at $DEST"
  exit 0
fi

mkdir -p "$DEST"

if [ ! -x "$DEST/kind" ]; then
  KIND_URL="https://github.com/kubernetes-sigs/kind/releases/download/${KIND_VERSION}/kind-linux-${ARCH}"
  echo "downloading $KIND_URL"
  curl -fsSL -o "$DEST/kind" "$KIND_URL"
  chmod +x "$DEST/kind"
  echo "installed kind at $DEST/kind"
fi

if [ ! -x "$DEST/kubectl" ]; then
  KUBECTL_URL="https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/${ARCH}/kubectl"
  KUBECTL_SHA_URL="${KUBECTL_URL}.sha256"
  echo "downloading $KUBECTL_URL"
  curl -fsSL -o "$DEST/kubectl" "$KUBECTL_URL"
  if curl -fsSL -o "$DEST/kubectl.sha256" "$KUBECTL_SHA_URL"; then
    EXPECTED="$(cat "$DEST/kubectl.sha256")"
    ACTUAL="$(sha256sum "$DEST/kubectl" | cut -d' ' -f1)"
    rm -f "$DEST/kubectl.sha256"
    if [ "$ACTUAL" != "$EXPECTED" ]; then
      echo "FAIL: sha256 mismatch for kubectl $KUBECTL_VERSION/$ARCH" >&2
      echo "  expected: $EXPECTED" >&2
      echo "  actual:   $ACTUAL" >&2
      rm -f "$DEST/kubectl"
      exit 1
    fi
    echo "checksum verified"
  else
    echo "WARNING: could not fetch kubectl checksum, skipping verification" >&2
  fi
  chmod +x "$DEST/kubectl"
  echo "installed kubectl at $DEST/kubectl"
fi

echo "e2e tools installed at $DEST"
