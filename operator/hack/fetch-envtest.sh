#!/bin/sh
set -eu
# Fetches real kube-apiserver/etcd/kubectl binaries for envtest, bypassing
# setup-envtest (both its releases need Go 1.26 and a denied x/mod version --
# see operator/README.md). Idempotent and checksum-verified.

VERSION="${1:?usage: fetch-envtest.sh <k8s-version> <dest-dir>}"
DEST="${2:?usage: fetch-envtest.sh <k8s-version> <dest-dir>}"
ARCH="${ENVTEST_ARCH:-amd64}"

TARGET_DIR="$DEST/k8s/$VERSION"
if [ -x "$TARGET_DIR/kube-apiserver" ]; then
  echo "envtest assets already present at $TARGET_DIR"
  exit 0
fi

ASSET="envtest-v${VERSION}-linux-${ARCH}.tar.gz"
URL="https://github.com/kubernetes-sigs/controller-tools/releases/download/envtest-v${VERSION}/${ASSET}"

# Known-good checksum for v1.35.0/linux-amd64, confirmed during planning.
# For other versions/arches, verify the sha512 against
# https://raw.githubusercontent.com/kubernetes-sigs/controller-tools/main/envtest-releases.yaml
# before trusting it, and update this case statement.
EXPECTED_SHA512=""
if [ "$VERSION" = "1.35.0" ] && [ "$ARCH" = "amd64" ]; then
  EXPECTED_SHA512="130369c16f076e724d089189afaede960316f5f5dea6cf57be7a4fc6f09c77342893192509790e4056e116e232dff832ed863f5bd55dcb55d38f3ab834828a11"
fi

mkdir -p "$TARGET_DIR"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

echo "downloading $URL"
curl -fsSL -o "$TMP/$ASSET" "$URL"

if [ -n "$EXPECTED_SHA512" ]; then
  ACTUAL_SHA512="$(sha512sum "$TMP/$ASSET" | cut -d' ' -f1)"
  if [ "$ACTUAL_SHA512" != "$EXPECTED_SHA512" ]; then
    echo "FAIL: sha512 mismatch for $ASSET" >&2
    echo "  expected: $EXPECTED_SHA512" >&2
    echo "  actual:   $ACTUAL_SHA512" >&2
    exit 1
  fi
  echo "checksum verified"
else
  echo "WARNING: no known checksum for $VERSION/$ARCH, skipping verification" >&2
fi

tar -xzf "$TMP/$ASSET" -C "$TMP"
mkdir -p "$TARGET_DIR"
mv "$TMP"/controller-tools/envtest/* "$TARGET_DIR/"
chmod +x "$TARGET_DIR"/*

echo "envtest assets installed at $TARGET_DIR"
echo "KUBEBUILDER_ASSETS=$TARGET_DIR"
