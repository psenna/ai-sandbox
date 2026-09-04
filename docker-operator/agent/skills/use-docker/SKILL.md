---
name: use-docker
description: Use when the agent needs to run code or stand up service dependencies (postgres, mysql, minio, redis, etc.) inside containers. The ai-sandbox stack gives you a rootless Docker-in-Docker daemon at DOCKER_HOST=tcp://docker:2375; this skill gives the canonical run pattern, per-language one-liners, the /workspace sharing rule, and the security boundary. Use it any time a task says to run, build, test, or execute code in node/python/go (or any other image), or to launch a database/cache/service for development. NEVER run node/python/go directly on the agent container — always run them inside a container launched against this daemon.
---

# Use Docker (rootless DinD)

You are running inside the agent container of the ai-sandbox stack. A
**rootless Docker-in-Docker daemon** is available at `DOCKER_HOST=tcp://docker:2375`
(set in your environment). Use it to run code in any language and to stand up
service dependencies — without installing anything on the agent itself.

The agent container intentionally has **no python, no go, no yarn, and no dev
node** — only the Node runtime Claude Code itself runs on. Every dev task
(`node script.js`, `python script.py`, `go test`, `npm install`, running a
database) happens inside a container launched against this daemon. Do not try to
run dev tooling directly on the agent; it is not there by design.

> **This is a fork of the shared `use-docker` skill.** The shared copy
> (`claude-code/use-docker/SKILL.md`) hardcodes DependaProxy's address as a
> fixed number, which is only true in the compose stack where every agent shares
> one `dinernet`. Under the docker-operator, DependaProxy is a **shared**
> service but each agent gets its **own private** `dinernet` — so there is no
> single fixed address. The operator connects DependaProxy to your network when
> it creates you, reads back the address it was assigned, and the entrypoint
> writes that address to **`/workspace/dependaproxy-ip`**. Every `--add-host`
> below reads that file instead of a literal. Everything else matches the shared
> skill.

## The one rule that matters most: `/workspace` is the only shared path

The Docker daemon lives in a **separate container** (`docker`). It cannot see your
filesystem except for the shared `/workspace` volume. So:

- **Files you want a container to read or write MUST live under `/workspace`.**
- Mount the shared volume into every workload: `-v /workspace:/work`, and set the
  workdir: `-w /work`.
- **Never bind-mount any other path** (e.g. `-v /opt:/x`, `-v /tmp:/x`,
  `-v /home/node:/x`). The daemon will silently mount an **empty** directory and
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
the DinD daemon, so a workload that bypasses the proxy fails with a connection
error — do not try to work around the block.

```sh
# Node (use a fresh container for isolation, not the agent's node). Run as the
# `node` user (uid 1000) so files written under /workspace belong to the agent —
# see *File ownership* below.
docker run --rm -u node -v /workspace:/work -w /work node:22-alpine node script.js
# npm install/test: mount the entrypoint-generated .npmrc (registry + token) and
# add the dependaproxy host entry (the nested daemon cannot resolve the compose
# name — the entrypoint writes this agent's dinernet IP to
# /workspace/dependaproxy-ip). With -u node, HOME=/home/node, so npm reads
# /home/node/.npmrc.
docker run --rm -u node -v /workspace:/work -w /work \
  -v /workspace/.npmrc:/home/node/.npmrc:ro \
  --add-host="dependaproxy:$(cat /workspace/dependaproxy-ip)" \
  node:22-alpine sh -c 'npm install && npm test'

# Python: pass the entrypoint-generated pip.env (PIP_INDEX_URL + PIP_TRUSTED_HOST)
# via --env-file, and add the dependaproxy host entry. (pip 26.x ignores
# bind-mounted pip.conf files, so env vars are used.)
docker run --rm -v /workspace:/work -w /work python:3-alpine python script.py
docker run --rm -v /workspace:/work -w /work \
  --env-file /work/pip.env \
  --add-host="dependaproxy:$(cat /workspace/dependaproxy-ip)" \
  python:3-alpine sh -c 'pip install -r requirements.txt && python script.py'

# Go: pass the entrypoint-generated go.env (GOPROXY) via --env-file, and add the
# dependaproxy host entry. Keep module/build caches on the shared volume so they
# persist. Go still verifies module checksums against sum.golang.org directly
# (that host is intentionally not blocked).
docker run --rm -v /workspace:/work -w /work \
  --env-file /work/go.env \
  --add-host="dependaproxy:$(cat /workspace/dependaproxy-ip)" \
  -e GOMODCACHE=/work/.gocache/mod -e GOCACHE=/work/.gocache/build \
  golang:1-alpine go test ./...
docker run --rm -v /workspace:/work -w /work \
  --env-file /work/go.env \
  --add-host="dependaproxy:$(cat /workspace/dependaproxy-ip)" \
  golang:1-alpine go build -o /work/app .
```

Note: a workload's installed dependencies (e.g. `node_modules/`, a Python venv)
persist only if they are written under `/workspace` (the shared volume) OR a named
volume you create. State written elsewhere inside the container is lost when `--rm`
removes it. For Go, set `GOMODCACHE`/`GOCACHE` under `/work` if you want build
caches to persist.

## Registries → DependaProxy (mandatory)

Every dependency fetch in a workload container goes through the DependaProxy
service — supply-chain-safe: each package is validated and served only if it
matches the stored hash. Three hard constraints:

- **The public registries are blocked at the network layer** (the DinD daemon
  rejects egress to the npm hosts `registry.npmjs.org`/`.com`,
  `registry.yarnpkg.com`, `registry.npmmirror.com`; the pypi hosts `pypi.org`,
  `files.pythonhosted.org`, `pypi.python.org`; and the Go hosts
  `proxy.golang.org`, `goproxy.io`, `goproxy.cn`). Even if you override a
  registry, the fetch will fail with a connection error — do not try to work
  around the block.
- **Always mount/pass the generated config + host entry** (see the one-liners
  above). The claude entrypoint writes four artifacts to `/workspace` (DependaProxy
  auth is disabled in this stack, so they carry no token):
  - `/workspace/.npmrc` — npm registry `http://dependaproxy:8080/npm`. Mount as
    `/home/node/.npmrc:ro` and run as `node`.
  - `/workspace/pip.env` — `PIP_INDEX_URL=http://dependaproxy:8080/pypi/simple` +
    `PIP_TRUSTED_HOST=dependaproxy`. Pass via `--env-file /work/pip.env`.
  - `/workspace/go.env` — `GOPROXY=http://dependaproxy:8080/goproxy`. Pass via
    `--env-file /work/go.env`.
  - `/workspace/dependaproxy-ip` — the address DependaProxy answers on from
    inside the DinD daemon. It is **per-agent** (see the fork note at the top),
    so read it, never memorise it.
  The first three all need
  `--add-host="dependaproxy:$(cat /workspace/dependaproxy-ip)"` (the nested
  daemon cannot resolve the compose name).

A package blocked by DependaProxy's validation (default: published less than 7 days
ago) returns a 403 — read the error and pick a different version; the block is
intentional.

**Installing a committed lockfile** (`uv.lock`, `pdm.lock`, `package-lock.json`)
whose artifact URLs point at the public registry needs one extra step —
`uv sync --frozen` fetches the baked-in URL and ignores the index. For `uv` it is
a reversible one-line rewrite of `files.pythonhosted.org` → the proxy's
`/pypi/upstream/` alias; npm just needs `npm config set replace-registry-host always`;
Go and pip hash-pinning need nothing. The **`use-dependaproxy`** skill has the
recipe, the `.gitattributes` clean/smudge filter, and what each `403` class means.

## File ownership (run as uid 1000, not root)

Workload images (node, python, go) default to running as **root**. Files they
create under `/workspace` are then **root-owned** on the host — and because the
agent runs as uid 1000, it can read them but **cannot delete them** (e.g.
`rm -rf node_modules` fails with "Permission denied").

Fix: run workloads as **uid 1000** — the agent's uid, which owns `/workspace`:
- node images: `-u node` (node:22-alpine ships a `node` user, uid 1000)
- any image: `-u "$(id -u):$(id -g)"` (resolves to `1000:1000` on this host)

Then every file the workload writes under `/workspace` belongs to the agent and
can be managed or deleted normally. If you already ran as root and need to clean
up, use a root container (root owns those files):
`docker run --rm -v /workspace:/work alpine rm -rf /work/node_modules`.

## Standing up a service dependency (postgres, mysql, minio, redis, …)

Run the service detached on the daemon's internal bridge, then connect from
another workload container by name. **These are NOT part of the base compose
stack** — you launch them on demand inside the isolated DinD daemon.

```sh
# Start postgres (detached; keeps running). Give it a named volume for data.
docker run -d --name pg \
  -e POSTGRES_PASSWORD=secret \
  -v pgdata:/var/lib/postgresql/data \
  postgres:16

# Connect from a throwaway container on the same daemon network (linked by name):
docker run --rm --link pg postgres:16 \
  psql -h pg -U postgres -c '\l'
```

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

Workload containers you launch are on the daemon's default bridge network; they
reach each other by container name (`pg`, `mysql`, `minio`, …). They are NOT on
the agent's `dinernet` and cannot reach `git-proxy` — that is intentional.

## Two execution surfaces — don't cross them

- **Git operations** (clone/fetch/push, PRs, CI, issues): go through **git-proxy**
  (see the `use-git-proxy` skill). Never `git clone` against github.com directly,
  and never try to reach the upstream token.
- **Running code & service deps**: use **Docker** (this skill).

Do not use Docker to bypass git-proxy (e.g. cloning github.com inside a
container). The Docker daemon is isolated from git-proxy on purpose.

## Security boundary (what you cannot do)

- You will **never** receive the upstream GitHub PAT. It is held by git-proxy and
  is not on `dinernet`. Do not attempt to obtain it.
- The Docker daemon is **rootless from the host's perspective**: it has no host
  privileges and cannot see git-proxy's bind mounts. It can run containers and
  pull images — that is its whole scope.
- If `docker` commands fail with "Cannot connect to the Docker daemon", the
  `docker` service may still be starting; wait a few seconds and retry (the
  operator only starts you once the DinD sidecar reports healthy, so this
  normally cannot happen).
