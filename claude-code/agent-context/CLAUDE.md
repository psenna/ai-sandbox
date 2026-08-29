# Environment

You are Claude Code running inside the `claude` container of the ai-sandbox
stack. You have TWO execution surfaces:

- **Git operations** (clone/fetch/push, PRs, CI, issues): go through git-proxy.
  See the `use-git-proxy` skill. Never talk to github.com directly.
- **Running code & service deps** (node, python, go, postgres, mysql, minio, …):
  use Docker, available via `DOCKER_HOST=tcp://docker:2375`. See the `use-docker`
  skill. Do NOT run node/python/go directly on this container — always run them
  inside disposable Docker containers launched against the DinD daemon.
- **Dependencies (npm / pip / Go modules)**: always through DependaProxy
  (`http://dependaproxy:8080/{npm,pypi,goproxy}`), never the public registries —
  see the `use-dependaproxy` skill (validation gates / what a 403 means /
  installing a committed lockfile) and `use-docker` (the mount/flag for workload
  containers). The public npm/pypi/Go registries are network-blocked by the
  sandbox; do not try to bypass the block. DependaProxy auth is disabled in this
  stack, so the generated `/workspace/.npmrc`, `/workspace/pip.env`, and
  `/workspace/go.env` carry no token.

## Docker rules (read before you `docker run`)

- `/workspace` is the ONLY shared exchange point with workload containers. Files
  you want a container to read/write must live under `/workspace`; mount it as
  `-v /workspace:/work` and `-w /work`.
- Do NOT bind-mount any other path (e.g. `/opt`, `/tmp`, `/home/node`) — the
  Docker daemon is in a separate container and cannot see your filesystem outside
  the shared `/workspace` volume. It will silently mount an empty path.
- Your working directory IS `/workspace`, so repo files are already shareable.
- Run workload containers as **uid 1000** (`-u node` for node images, or
  `-u "$(id -u):$(id -g)"`) so files they write under `/workspace` stay owned by
  you. Root-run containers leave root-owned files you cannot delete — see the
  `use-docker` skill (File ownership).
- The Docker daemon is rootless/isolated: it cannot reach git-proxy or its
  credentials. You will never receive the upstream GitHub PAT — do not attempt to
  obtain it.

## Credentials (read before you commit)

- You do NOT have the upstream GitHub PAT. You have only `AGENT_TOKEN`, a
  low-value Bearer that authenticates you to git-proxy. Never put any token in a
  commit message, file, or environment you write to disk.
- git-proxy runs `secret_scan` on every push: a push containing a secret (API
  key, PAT, private key) is rejected with a redacted reason. That rejection is
  correct — fix the change, do not try to circumvent it.
- Force-push to `main` and pushes outside `main` / `feat/*` are rejected by
  policy. Work on a `feat/*` branch.