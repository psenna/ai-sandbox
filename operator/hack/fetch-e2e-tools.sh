#!/bin/sh
set -eu
# Fetches real kind, kubectl and helm binaries for the e2e suite, over the
# same curl-from-GitHub-releases route hack/fetch-envtest.sh already uses.
# Idempotent: skips each binary's download entirely once it is already
# executable at $DEST.
#
# helm (#34) is fetched here -- not just relied upon from PATH -- because
# hack/e2e-up.sh now shells out to `helm` when E2E_DEPLOY=helm, and that
# script may run wrapped inside E2E_TOOLBOX_RUN's docker:28-cli image, which
# has kubectl/kind (via this same script) but no helm at all. On a GitHub
# Actions runner (IN_CONTAINER=1) helm is already on PATH via
# azure/setup-helm, so this download is redundant there but harmless --
# $DEST is prepended onto PATH for those steps regardless (see the
# Makefile's e2e-up/helm-kind targets), and whichever helm resolves first
# wins.

KIND_VERSION="${1:?usage: fetch-e2e-tools.sh <kind-version> <kubectl-version> <helm-version> <dest-dir>}"
KUBECTL_VERSION="${2:?usage: fetch-e2e-tools.sh <kind-version> <kubectl-version> <helm-version> <dest-dir>}"
HELM_VERSION="${3:?usage: fetch-e2e-tools.sh <kind-version> <kubectl-version> <helm-version> <dest-dir>}"
DEST="${4:?usage: fetch-e2e-tools.sh <kind-version> <kubectl-version> <helm-version> <dest-dir>}"
ARCH="${E2E_TOOLS_ARCH:-amd64}"

if [ -x "$DEST/kind" ] && [ -x "$DEST/kubectl" ] && [ -x "$DEST/helm" ]; then
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

if [ ! -x "$DEST/helm" ]; then
  HELM_TARBALL="helm-${HELM_VERSION}-linux-${ARCH}.tar.gz"
  HELM_URL="https://get.helm.sh/${HELM_TARBALL}"
  echo "downloading $HELM_URL"
  TMPDIR="$(mktemp -d)"
  curl -fsSL -o "$TMPDIR/$HELM_TARBALL" "$HELM_URL"
  if curl -fsSL -o "$TMPDIR/$HELM_TARBALL.sha256sum" "$HELM_URL.sha256sum"; then
    EXPECTED="$(awk '{print $1}' "$TMPDIR/$HELM_TARBALL.sha256sum")"
    ACTUAL="$(sha256sum "$TMPDIR/$HELM_TARBALL" | cut -d' ' -f1)"
    if [ "$ACTUAL" != "$EXPECTED" ]; then
      echo "FAIL: sha256 mismatch for helm $HELM_VERSION/$ARCH" >&2
      echo "  expected: $EXPECTED" >&2
      echo "  actual:   $ACTUAL" >&2
      rm -rf "$TMPDIR"
      exit 1
    fi
    echo "checksum verified"
  else
    echo "WARNING: could not fetch helm checksum, skipping verification" >&2
  fi
  tar -xzf "$TMPDIR/$HELM_TARBALL" -C "$TMPDIR" "linux-${ARCH}/helm"
  mv "$TMPDIR/linux-${ARCH}/helm" "$DEST/helm"
  chmod +x "$DEST/helm"
  rm -rf "$TMPDIR"
  echo "installed helm at $DEST/helm"
fi

echo "e2e tools installed at $DEST"
