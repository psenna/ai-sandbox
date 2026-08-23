---
name: use-docker
description: Use when the agent needs to run code or stand up service dependencies (postgres, mysql, minio, redis, etc.) inside containers. Every ai-sandbox deployment gives you a Docker-compatible API at $DOCKER_HOST; this skill gives the canonical run pattern, per-language one-liners, the /workspace sharing rule, and the security boundary. Use it any time a task says to run, build, test, or execute code in node/python/go (or any other image), or to launch a database/cache/service for development. NEVER run node/python/go directly on the agent container — always run them inside a container launched against this daemon.
---

# Use Docker

You are running inside an ai-sandbox agent container. A **Docker-compatible
API** is available at `$DOCKER_HOST` (already set in your environment — see
"Which daemon am I talking to?" below). Use it to run code in any language
and to stand up service dependencies — without installing anything on the
agent itself.

The agent container intentionally has **no python, no go, no yarn, and no dev
node** — only the Node runtime Claude Code itself runs on. Every dev task
(`node script.js`, `python script.py`, `go test`, `npm install`, running a
database) happens inside a container launched against `$DOCKER_HOST`. Do not
try to run dev tooling directly on the agent; it is not there by design.

## Which daemon am I talking to?

This same skill (and the same agent image) runs in two different
deployments of ai-sandbox. You do not need to figure out which one you are
in — `$DOCKER_HOST` is already set correctly for you, and the `docker` CLI
works identically against either. It is documented here so you understand
*why* a couple of rules below differ by deployment.

| | compose stack | ai-sandbox-operator (Kubernetes) |
|---|---|---|
| `DOCKER_HOST` | `tcp://docker:2375` | `tcp://127.0.0.1:2375` |
| What answers it | a separate rootless Docker-in-Docker container (`docker` service), reachable over the compose network | a rootless-podman sidecar in the **same pod** as this agent container, speaking podman's Docker-compatible API on pod loopback |
| Present at all? | only when `engine.type` isn't disabled for this stack | only when the `SandboxClass` selects `engine.type: rootless-podman` (the CRD default) — a class using `engine.type: none` has no engine sidecar and no `$DOCKER_HOST` at all |

Never hardcode either value — read `$DOCKER_HOST` (`docker` does this for
you automatically) so your commands work in whichever deployment launched
you.

**"compose is not available."** Neither deployment ships `docker compose`/
`docker-compose` — only the plain `docker` CLI is installed. If a task needs
several linked services, launch them individually and connect them by
container/network name (see "Standing up a service dependency" below);
don't reach for a compose file.

## The one rule that matters most: `/workspace` is the only shared path

Whichever deployment you're in, the engine answering `$DOCKER_HOST` runs in
a **different container** from this one — a separate `docker` service in
the compose stack, a separate sidecar container (sharing only the pod, not
the filesystem) under the operator. Either way, it cannot see your
filesystem except for the shared `/workspace` volume. So:

- **Files you want a container to read or write MUST live under `/workspace`.**
- Mount the shared volume into every workload: `-v /workspace:/work`, and set the
  workdir: `-w /work`.
- **Never bind-mount any other path** (e.g. `-v /opt:/x`, `-v /tmp:/x`,
  `-v /home/node:/x`). The engine will silently mount an **empty** directory and
  your command will fail or see no files. There is no error — it just looks empty.

Your working directory IS `/workspace` (the repo is cloned here), so repo files
are already shareable.

## Canonical run pattern

```sh
docker run --rm -v /workspace:/work -w /work <image> <command>
```

`--rm` removes the container after it exits (keep workloads ephemeral unless you
need a named volume for state). `-v /workspace:/work -w /work` makes your files
appear at `/work` inside the container and sets the working directory there.

## Per-language one-liners

npm, pip, and Go modules MUST all go through DependaProxy (see *Registries →
DependaProxy* below). The public npm/pypi/Go registries are network-blocked from
every workload container, so a fetch that bypasses the proxy fails with a
connection error — do not try to work around the block.

**Set `$U` once before the commands below.** The two deployments map container
UIDs to host UIDs in *opposite* directions, so there is no single `-u` value
that is correct in both — see *File ownership* below for why. This one-liner
picks the right one:

```sh
# Operator (rootless podman) maps container root -> your uid, so ask for NO -u.
# Compose (DinD) does no remapping, so ask for your uid explicitly.
case "$DOCKER_HOST" in
  tcp://127.0.0.1:*) U="" ;;
  *)                 U="-u $(id -u):$(id -g)" ;;
esac
```

```sh
# Node (use a fresh container for isolation, not the agent's node). $U makes
# files written under /workspace belong to the agent — see *File ownership*.
docker run --rm $U -v /workspace:/work -w /work node:22-alpine node script.js

# npm install/test: NPM_CONFIG_REGISTRY is already set correctly in YOUR OWN
# process environment (compose stack and operator sandbox alike) — pass it
# through with a bare `-e NAME` (no `=value`), which forwards your current
# value rather than requiring you to know it. No /home/node/.npmrc needed at
# all with this form, whichever uid the container ends up running as.
docker run --rm $U -v /workspace:/work -w /work -e NPM_CONFIG_REGISTRY \
  node:22-alpine sh -c 'npm install && npm test'

# Python: PIP_INDEX_URL is set the same portable way. PIP_TRUSTED_HOST is not
# guaranteed to be pre-set in every deployment — pip refuses a plain-HTTP
# index without it, so derive it from the URL's host when PIP_INDEX_URL is
# http:// (a compose-stack agent can instead pass --env-file /work/pip.env,
# which the entrypoint generates with both values already computed).
docker run --rm -v /workspace:/work -w /work python:3-alpine python script.py
docker run --rm -v /workspace:/work -w /work \
  -e PIP_INDEX_URL -e PIP_TRUSTED_HOST="$(echo "$PIP_INDEX_URL" | sed -E 's#https?://([^/]+).*#\1#')" \
  python:3-alpine sh -c 'pip install -r requirements.txt && python script.py'

# Go: GOPROXY is set the same portable way in every deployment. Keep
# module/build caches on the shared volume so they persist. Go still
# verifies module checksums against sum.golang.org directly (that host is
# intentionally not blocked).
docker run --rm -v /workspace:/work -w /work \
  -e GOPROXY -e GOMODCACHE=/work/.gocache/mod -e GOCACHE=/work/.gocache/build \
  golang:1-alpine go test ./...
docker run --rm -v /workspace:/work -w /work \
  -e GOPROXY golang:1-alpine go build -o /work/app .
```

Note: a workload's installed dependencies (e.g. `node_modules/`, a Python venv)
persist only if they are written under `/workspace` (the shared volume) OR a named
volume you create. State written elsewhere inside the container is lost when `--rm`
removes it. For Go, set `GOMODCACHE`/`GOCACHE` under `/work` if you want build
caches to persist.

## Registries → DependaProxy (mandatory, for package dependencies)

Every **dependency fetch** (npm/pip/Go module) in a workload container goes
through the DependaProxy service — supply-chain-safe: each package is
validated and served only if it matches the stored hash.

- **The public registries are blocked at the network layer** (npm hosts
  `registry.npmjs.org`/`.com`, `registry.yarnpkg.com`,
  `registry.npmmirror.com`; the pypi hosts `pypi.org`,
  `files.pythonhosted.org`, `pypi.python.org`; the Go hosts
  `proxy.golang.org`, `goproxy.io`, `goproxy.cn`). Even if you override a
  registry, the fetch fails with a connection error — do not try to work
  around the block.
- **The portable way to reach it: pass the env var through, don't hardcode
  the URL.** `NPM_CONFIG_REGISTRY`, `PIP_INDEX_URL`, and `GOPROXY` are
  already correct in your own process environment in every deployment — see
  the one-liners above. This is the form to reach for by default; it needs
  no deployment-specific knowledge at all.
- **Compose-stack-only alternative.** The compose stack's entrypoint also
  writes three ready-to-mount artifacts to `/workspace` (DependaProxy auth
  is disabled in this stack, so they carry no token): `/workspace/.npmrc`
  (mount as `/home/node/.npmrc:ro` and run as `node`), `/workspace/pip.env`
  and `/workspace/go.env` (pass via `--env-file`). These, plus
  `--add-host=dependaproxy:172.23.0.10` (the nested DinD daemon cannot
  resolve the compose service name `dependaproxy`), are needed only if you
  prefer the file-based form over the portable `-e VAR` form above. **The
  operator deployment does not generate these files or this static IP at
  all** — the portable form is the only one that works there.

A package blocked by DependaProxy's validation (default: published less than 7 days
ago) returns a 403 — read the error and pick a different version; the block is
intentional.

### The registry-mirror caveat (operator deployment, container *images*)

The paragraph above is about **package dependencies** — it has nothing to
do with the base **container image** a `docker run`/`docker pull` fetches
(`node:22-alpine`, `postgres:18-alpine`, …). DependaProxy cannot serve
container images at all (it has no OCI/distribution endpoint). Under the
operator deployment with `network.isolation: Restricted`, image pulls are
instead routed transparently through a `SandboxClass`-configured pull-through
registry mirror (`services.registryMirror`) — you do not do anything
differently to use it, `docker pull <image>` and `docker run <image>` just
work if the class has one configured. **If image pulls hang or time out
under the operator deployment, the class most likely has no
`registryMirror` configured at all** — that is a cluster-admin-level fix
(`SandboxClass.spec.services.registryMirror`), not something fixable from
inside the sandbox. The compose stack has no equivalent restriction: its
DinD daemon reaches public registries directly, so this caveat is
operator-only.

### Storage-driver performance (operator deployment)

The operator's rootless-podman engine uses the native `overlay` storage
driver by default — fast, and what you almost certainly have. If image
pulls or container startup feel unusually slow under the operator
deployment, the class may have explicitly selected `storageDriver: vfs`
(copy-on-write emulated with real copies — very slow, and only ever
selected deliberately, never automatically). That, too, is a class-level
setting outside the sandbox's control, not something to work around from
inside a workload container.

## File ownership (uid 1000, and where it comes from differs by deployment)

Workload images (node, python, go) default to running as **root**. Whether
that leaves you with root-owned files under `/workspace` depends on which
deployment you're in — this is the one rule that is *inverted* between the
two:

- **Compose stack: you must ask for uid 1000 explicitly.** Its DinD daemon
  does not remap container users at all — a container that runs as root
  writes root-owned files on the host, and because the agent itself runs as
  uid 1000, it can read them but **cannot delete them** (e.g. `rm -rf
  node_modules` fails with "Permission denied").
- **Operator deployment: do NOT pass `-u` (or pass `-u 0`).** The podman
  sidecar itself runs as uid 1000, and rootless podman puts every workload
  container in a user namespace that maps **container uid 0 to that same
  real uid 1000**, and container uids `1..65535` to a *subordinate* range
  from `/etc/subuid` (typically starting at 100000). So a container you ran
  with no `-u` at all (or explicitly `-u 0`) writes files under `/workspace`
  already owned by uid 1000 — the agent.

**Do not reach for `-u node` / `-u "$(id -u):$(id -g)"` as a "portable
habit": under the operator deployment it is the thing that breaks
ownership.** Asking for container uid 1000 there maps to host uid ~100999,
so the files land owned by a subordinate uid the agent can read but
**cannot write or delete** — exactly the failure the same flag *prevents*
in the compose stack. Use the `$U` one-liner from "Per-language one-liners"
above, which resolves to `-u $(id -u):$(id -g)` on compose and to nothing
under the operator.

Cleaning up files you can no longer delete works the same way in both, and
for the same reason — run the `rm` as whichever container uid owns them:

```sh
# compose stack, files left root-owned by a container you ran without -u:
docker run --rm -v /workspace:/work alpine rm -rf /work/node_modules
# operator deployment, files left subuid-owned by a container you ran with
# -u 1000: re-enter as container root, which owns that whole subuid range.
docker run --rm -u 0 -v /workspace:/work alpine rm -rf /work/node_modules
```

## Standing up a service dependency (postgres, mysql, minio, redis, …)

Run the service detached, then connect from another workload container by
name — both deployments give workload containers their own default network
where containers reach each other by container name. **These are NOT part
of the base compose stack, nor of the sandbox pod's own containers** — you
launch them on demand inside whichever engine answers `$DOCKER_HOST`.

```sh
# Start postgres (detached; keeps running). Give it a named volume for data.
docker run -d --name pg \
  -e POSTGRES_PASSWORD=secret \
  -v pgdata:/var/lib/postgresql/data \
  postgres:16

# Connect from a throwaway container on the same network (linked by name):
docker run --rm --link pg postgres:16 \
  psql -h pg -U postgres -c '\l'
```

Under the operator deployment, containers on different `docker network
create`d networks are isolated from each other the way you'd expect;
put containers that need to talk to each other on the same
`--network <name>` (see the postgres example in the acceptance-test spec
`test/e2e/engine_test.go`, "runs a container, bind-mounts the workspace,
and starts a postgres service container", for a full worked example
including `docker network create`).

### MySQL

```sh
docker run -d --name mysql \
  -e MYSQL_ROOT_PASSWORD=secret \
  -e MYSQL_DATABASE=app \
  -v mysqldata:/var/lib/mysql \
  mysql:8

# Connect:
docker run --rm --link mysql mysql:8 \
  mysql -h mysql -uroot -psecret -e 'SHOW DATABASES;'
```

### MinIO (S3-compatible object storage)

```sh
docker run -d --name minio \
  -e MINIO_ROOT_USER=accesskey \
  -e MINIO_ROOT_PASSWORD=secretkey \
  -v miniodata:/data \
  minio/minio:latest server /data --console-address ":9001"

# From a workload container, point any S3 client at http://minio:9000:
docker run --rm --link minio minio/mc:latest \
  sh -c 'mc alias set local http://minio:9000 accesskey secretkey && mc mb local/app'
```

Workload containers you launch are on the engine's own default network; they
reach each other by container name (`pg`, `mysql`, `minio`, …). In the
compose stack they are NOT on the compose `dinernet` and cannot reach
`git-proxy` — that is intentional. In the operator deployment they are not
on the pod's own network stack in the way the agent/sandboxctl containers
are (see the security boundary below) — they get their own private network
namespace by default, separate from the pod's.

## Two execution surfaces — don't cross them

- **Git operations** (clone/fetch/push, PRs, CI, issues): go through **git-proxy**
  (see the `use-git-proxy` skill). Never `git clone` against github.com directly,
  and never try to reach the upstream token.
- **Running code & service deps**: use Docker/`$DOCKER_HOST` (this skill).

Do not use Docker to bypass git-proxy (e.g. cloning github.com inside a
container). Whichever engine answers `$DOCKER_HOST` is isolated from
git-proxy on purpose in both deployments.

## Security boundary (what you cannot do)

- You will **never** receive the upstream GitHub PAT. It is held by git-proxy;
  do not attempt to obtain it. In the compose stack it is additionally simply
  not reachable on the daemon's network (`dinernet`); in the operator
  deployment there is no git-proxy credential in the pod's engine at all —
  see the next point.
- **The engine sidecar/daemon holds nothing worth stealing.** In the
  compose stack, the `docker` service has no host privileges and cannot see
  git-proxy's bind mounts. In the operator deployment, the podman sidecar
  mounts nothing but the shared workspace and its own private layer cache —
  no Kubernetes credential, no Secret, no git-proxy token — so even a
  compromised workload container gains nothing beyond what the agent
  process already has.
- If `docker` commands fail with "Cannot connect to the Docker daemon", the
  engine may still be starting; wait a few seconds and retry. In the
  compose stack this is `depends_on: service_healthy` normally preventing
  it; in the operator deployment the podman sidecar is a native init
  container with a `startupProbe` that the kubelet requires to succeed
  before this agent container is even started at all, so a connection
  failure here means the sidecar itself is unhealthy, not merely slow — it
  is worth checking `kubectl describe pod`/`kubectl logs <pod> -c podman`
  from outside the sandbox if this persists.
- **Operator deployment only — `--network host` reaches the pod's own
  loopback, not just this engine's default network.** `docker run --network
  host` skips creating a new network namespace, so the workload container
  runs directly in the **pod's** own network namespace — which means it can
  reach `127.0.0.1:9099` (the sandboxctl control API) and `127.0.0.1:2375`
  (the podman API itself). This is not a privilege escalation — you (the
  agent) already hold both — but it does mean a workload container launched
  with `--network host` is not isolated from the sandbox's own control
  plane the way a default-network container is. It does **not** bypass the
  pod's NetworkPolicy for egress to anything else: that is still enforced
  identically, because the traffic still leaves on the pod's own interface
  either way. Use `--network host` only when you actually need it (e.g. to
  reach a service already bound to the pod's own loopback); the default
  network is the right choice otherwise.
