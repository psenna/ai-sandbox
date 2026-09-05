#!/bin/sh
set -eu
# Extracts every ```sh quickstart fenced block from docker-operator/README.md,
# in document order, and runs them as ONE shell script against a real Docker
# daemon -- so the quickstart in the README is executed as a documentation
# test (#79), not hand-kept-in-sync with a second copy that can silently
# drift. Structure copied from operator/hack/quickstart-check.sh (same awk
# extraction, same diagnostics-on-failure/EXIT-trap idiom), simplified: no
# kind cluster, no helm -- just `docker compose` against whatever Docker
# daemon is already reachable.
#
# docker-operator/README.md's `sh quickstart`-tagged fenced blocks are the
# single source of truth for what this script runs; it duplicates none of
# their content. Needs docker and `docker compose` on PATH.
#
# Deliberately does NOT cover "create an agent" (the README's step 4): that
# needs sysbox-runc for the agent's own DinD sidecar, which a plain Docker
# host -- including the GitHub Actions runner this normally executes on --
# does not have (tracked in #80). Step 4 is documented but not tagged
# `quickstart` for exactly this reason, same as this module's own
# TestIntegrationCreateDelete skipping for the same missing runtime.
#
# Strict POSIX sh, no pipefail (dash on Ubuntu has no pipefail, same
# constraint operator/hack/quickstart-check.sh documents).

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
DOC="${QUICKSTART_DOC:-$REPO_ROOT/docker-operator/README.md}"
MIN_BLOCKS="${QUICKSTART_MIN_BLOCKS:-4}"
ARTIFACTS="${E2E_ARTIFACT_DIR:-$REPO_ROOT/docker-operator/.quickstart-artifacts}"
SCRIPT="$(mktemp)"

# ---------------------------------------------------------------------------
# Diagnostics on any non-zero exit -- same idiom as operator/hack's own
# dump_diagnostics/finish/EXIT-trap.
# ---------------------------------------------------------------------------
dump_diagnostics() {
  mkdir -p "$ARTIFACTS"
  echo "---- docker compose ps ----" | tee "$ARTIFACTS/compose-ps.txt" >&2
  docker compose ps -a 2>&1 | tee -a "$ARTIFACTS/compose-ps.txt" >&2 || true
  echo "---- docker-operator logs (tail 200) ----" | tee "$ARTIFACTS/operator-logs.txt" >&2
  docker compose logs --tail=200 docker-operator 2>&1 | tee -a "$ARTIFACTS/operator-logs.txt" >&2 || true
  echo "---- docker compose logs, everything (tail 100 each) ----" | tee "$ARTIFACTS/all-logs.txt" >&2
  docker compose logs --tail=100 2>&1 | tee -a "$ARTIFACTS/all-logs.txt" >&2 || true
}

finish() {
  rc=$?
  if [ "$rc" -ne 0 ]; then
    echo "==> quickstart-check.sh FAILED (rc=$rc) -- dumping diagnostics" >&2
    dump_diagnostics
  fi
  docker compose down -v >/dev/null 2>&1 || true
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

# 2. Run them as ONE shell, from docker-operator/ (the compose file's own
#    directory -- its relative paths like ../config.yaml are resolved
#    against that), so `export`s and cwd carry across blocks exactly as they
#    do for a reader pasting them into one terminal.
echo "==> executing the quickstart, verbatim, from $DOC"
cd "$REPO_ROOT/docker-operator"
sh -eux "$SCRIPT"
echo "==> quickstart-check.sh: the documented quickstart ran end to end"
