---
name: use-docker
description: Use when the agent needs to run code or stand up service dependencies (postgres, mysql, minio, redis, etc.) inside containers. The ai-sandbox stack gives you a rootless Docker-in-Docker daemon at DOCKER_HOST=tcp://docker:2375; this skill gives the canonical run pattern, per-language one-liners, the /workspace sharing rule, and the security boundary. Use it any time a task says to run, build, test, or execute code in node/python/go (or any other image), or to launch a database/cache/service for development. NEVER run node/python/go directly on the agent container — always run them inside a container launched against this daemon.
---

# Use Docker (rootless DinD)

You are running inside the `claude` container of the ai-sandbox stack. A
**rootless Docker-in-Docker daemon** is available at `DOCKER_HOST=tcp://docker:2375`
(set in your environment). Use it to run code in any language and to stand up
service dependencies — without installing anything on the agent itself.

The agent container intentionally has **no python, no go, no yarn, and no dev
node** — only the Node runtime Claude Code itself runs on. Every dev task
(`node script.js`, `python script.py`, `go test`, `npm install`, running a
database) happens inside a container launched against this daemon. Do not try to
run dev tooling directly on the agent; it is not there by design.

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

```sh
# Node (use a fresh container for isolation, not the agent's node)
docker run --rm -v /workspace:/work -w /work node:22-alpine node script.js
docker run --rm -v /workspace:/work -w /work node:22-alpine sh -c 'npm install && npm test'

# Python
docker run --rm -v /workspace:/work -w /work python:3-alpine python script.py
docker run --rm -v /workspace:/work -w /work python:3-alpine sh -c 'pip install -r requirements.txt && python script.py'

# Go (keep module/build caches on the shared volume so they persist)
docker run --rm -v /workspace:/work -w /work \
  -e GOMODCACHE=/work/.gocache/mod -e GOCACHE=/work/.gocache/build \
  golang:1-alpine go test ./...
docker run --rm -v /workspace:/work -w /work golang:1-alpine go build -o /work/app .
```

Note: a workload's installed dependencies (e.g. `node_modules/`, a Python venv)
persist only if they are written under `/workspace` (the shared volume) OR a named
volume you create. State written elsewhere inside the container is lost when `--rm`
removes it. For Go, set `GOMODCACHE`/`GOCACHE` under `/work` if you want build
caches to persist.

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
the compose `dinernet` and cannot reach `git-proxy` — that is intentional.

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
  compose `depends_on: service_healthy` normally prevents this).