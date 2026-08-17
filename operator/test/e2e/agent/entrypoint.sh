#!/bin/sh
# Minimal stand-in for the real agent image's entrypoint.sh: log the args
# it was invoked with (visible in `kubectl logs`, useful for diagnostics),
# then exec them. RenderPod sets container `args` only, never `command`, so
# this entrypoint runs unconditionally and must exec whatever args it is
# given -- exactly the contract the real agent image's entrypoint honors.
set -eu
echo "test-agent: args=$*" >&2
exec "$@"
