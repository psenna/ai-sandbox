# Environment

You are Claude Code running inside the `claude` container of the ai-sandbox
stack. You have TWO execution surfaces:

- **Git operations** (clone/fetch/push, PRs, CI, issues): go through git-proxy.
  See the `use-git-proxy` skill. Never talk to github.com directly.
- **Running code & service deps** (node, python, go, postgres, mysql, minio, …):
  use Docker, available via `$DOCKER_HOST` (already set in your environment —
  its value depends on which ai-sandbox deployment you're in; never hardcode
  it). See the `use-docker` skill. Do NOT run node/python/go directly on this
  container — always run them inside disposable containers launched against
  `$DOCKER_HOST`.
- **Dependencies (npm / pip / Go modules)**: always through DependaProxy, never
  the public registries — pass `NPM_CONFIG_REGISTRY`/`PIP_INDEX_URL`/`GOPROXY`
  straight through (already set correctly in your own environment; see the
  `use-docker` skill for the exact portable form and the mount/flag
  alternative). The public npm/pypi/Go registries are network-blocked by the
  sandbox; do not try to bypass the block. DependaProxy auth is disabled in this
  stack, so no token is ever needed for it.

## Docker rules (read before you `docker run`)

- `/workspace` is the ONLY shared exchange point with workload containers. Files
  you want a container to read/write must live under `/workspace`; mount it as
  `-v /workspace:/work` and `-w /work`.
- Do NOT bind-mount any other path (e.g. `/opt`, `/tmp`, `/home/node`) — the
  container answering `$DOCKER_HOST` cannot see your filesystem outside
  the shared `/workspace` volume. It will silently mount an empty path.
- Your working directory IS `/workspace`, so repo files are already shareable.
- File ownership is the one rule that is INVERTED between deployments, so
  there is no single `-u` that is right in both: the compose stack does no
  UID remapping (pass `-u "$(id -u):$(id -g)"`), while rootless podman under
  the operator maps container uid 0 to your own uid 1000 and container uid
  1000 to an unwritable subordinate uid (pass no `-u` at all). See the
  `use-docker` skill (File ownership) for the one-liner that picks correctly.
- The container/sidecar answering `$DOCKER_HOST` is rootless/isolated: it
  cannot reach git-proxy or its credentials. You will never receive the
  upstream GitHub PAT — do not attempt to obtain it.

## Credentials (read before you commit)

- You do NOT have the upstream GitHub PAT. You have only `AGENT_TOKEN`, a
  low-value Bearer that authenticates you to git-proxy. Never put any token in a
  commit message, file, or environment you write to disk.
- git-proxy runs `secret_scan` on every push: a push containing a secret (API
  key, PAT, private key) is rejected with a redacted reason. That rejection is
  correct — fix the change, do not try to circumvent it.
- Force-push to `main` and pushes outside `main` / `feat/*` are rejected by
  policy. Work on a `feat/*` branch.