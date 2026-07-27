# ai-sandbox

A containerized sandbox that runs **Claude Code** with **Ollama** as the LLM
server, in front of **git-proxy** so the agent can work on a real GitHub repo
**without ever seeing the GitHub PAT**, and with a **rootless Docker-in-Docker**
daemon so dev tasks (node/python/go runs, databases) happen in isolated
containers that cannot reach the host or the proxy's credentials.

```
 proxynet (bridge):  ollama ──┐
   git-proxy ──────── https ──> github.com   (holds the PAT; agent never sees it)
   claude ─── git/push/PR ───> git-proxy (8080 git / 8090 broker)
        └── /v1/messages ───> ollama (11434, Anthropic-compatible API)
 dinernet (bridge):  docker (rootless DinD, sysbox-runc, no --privileged)
   claude ─── DOCKER_HOST=tcp://docker:2375 ──> runs node/python/go/services
```

The agent never gets the upstream PAT, never has `gh`, and never has python/go on
its container — dev tooling runs inside disposable DinD containers. Pushes are
policy-gated (`secret_scan`, `history_protect`, `branch_pattern`) and audited.

---

## Architecture

Four services on two isolated bridge networks (see `docker-compose.yaml`):

| Service | Image | Networks | Purpose |
|---|---|---|---|
| `ollama` | `ollama/ollama:latest` | proxynet | LLM server, Anthropic-compatible `/v1/messages` on `:11434`. `:cloud` + local models. |
| `git-proxy` | `ghcr.io/psenna/git-proxy:v0.0.0` | proxynet | Policy gateway holding the GitHub PAT. `8080` (git) + `8090` (broker) on `127.0.0.1`. |
| `docker` | `docker:27-dind` (`sysbox-runc`) | dinernet | Rootless DinD daemon for agent-launched dev workloads. |
| `claude` | built from `Dockerfile` | proxynet + dinernet | Slim agent: node + claude-code + git + docker-cli. No python/go. |

`git-proxy` is on **proxynet only**; the DinD daemon is on **dinernet only** — so
a compromised daemon has no route to the proxy's bind-mounted `credentials.yaml`.
`claude` is on both. The shared `workspace` volume (mounted in `claude` and
`docker`) is the only file-exchange point between the agent and the containers it
launches.

---

## Prerequisites

1. **Ubuntu 24.04 LTS** host (amd64 or arm64), systemd. The DinD daemon needs the
   `sysbox-runc` runtime, so:
   ```sh
   sudo bash setup-ubuntu-host.sh
   ```
   This installs Docker Engine 28.x + containerd 1.7.x (pinned + held) and
   sysbox-ce 0.7.0, and verifies `docker run --runtime=sysbox-runc --rm alpine echo ok`.

2. **`ghcr.io/psenna/git-proxy:v0.0.0`** published. The git-proxy repo's publish
   pipeline is not yet merged to `main`. Until that image exists, build it locally
   from a sibling checkout: in `docker-compose.yaml` comment out the `git-proxy`
   `image:` line and uncomment the `build:` block (`context: ../git-proxy`), then
   `docker compose build git-proxy`.

3. **A GitHub fine-grained PAT** for the repo(s) the agent will work on:
   https://github.com/settings/personal-access-tokens
   - Read access to **actions** and **metadata**
   - Read and write access to **code**, **issues**, **pull requests**, and **workflows**
   - (Discussions only if you use them.)

4. **Ollama Cloud device auth** (only if you use `:cloud` models). The local
   daemon authenticates to ollama.com with the SSH keypair in `./.ollama`
   (`id_ed25519` + `config.json`) — **not** an API key. Either:
   - copy your existing `.ollama/` folder (with the registered key) onto the host
     next to `docker-compose.yaml` (the `ollama` service bind-mounts it), **or**
   - start fresh: `docker compose up -d ollama` then
     `docker compose exec ollama ollama signin` once and approve the URL it
     prints. The key persists in `./.ollama`.
   (`OLLAMA_API_KEY` is only for *direct* ollama.com API calls, which this stack
   does not make — leave it unset.)

---

## Setup

```sh
# 1. Configure secrets (gitignored).
cp .env.example .env
#   edit .env: OLLAMA_MODEL, GITHUB_REPO, (AGENT_TOKEN if you rotate it)
#   For :cloud models, also put your registered .ollama/ folder here (see
#   prerequisites #4) — the ollama service bind-mounts ./.ollama.

# 2. Give git-proxy the GitHub PAT. Two options:
#    a) edit credentials.yaml: put the PAT in `password` AND `token`, and set the
#       `repos` pattern to your OWNER/REPO.git (must match GITHUB_REPO in .env); or
#    b) export GITHUB_TOKEN (env > file > empty — profile name GITHUB -> GITHUB_TOKEN)
#       and pass it to the git-proxy container via docker-compose.yaml environment.
#    NEVER commit a real PAT — credentials.yaml is tracked with placeholders only.

# 3. Create the bind-mount dirs git-proxy writes to (uid 1000 — see below if your
#    host uid differs).
mkdir -p data/mirror data/audit
```

> **uid note:** `git-proxy` runs as uid 1000. If `data/` is not writable by uid
> 1000, run `sudo chown -R 1000:1000 data`.

---

## Run

```sh
docker compose up -d --build
docker compose ps          # ollama + docker must be healthy; claude starts after them

# Interactive Claude Code session (backed by Ollama, routed through git-proxy):
docker compose exec claude claude

# Headless one-shot:
docker compose exec claude claude -p "Clone the repo, add hello.txt on feat/test, push, open a PR"
```

The `claude` entrypoint sets `git insteadOf` so every `https://github.com/<x>` URL
is rewritten to `http://git-proxy:8080/<x>` with the agent Bearer attached, and
drops the `use-git-proxy` + `use-docker` skills and `CLAUDE.md` into
`/workspace/.claude/`. So ordinary `git clone`/`push`/`fetch` flow through the
proxy with no extra flags, and PRs/CI/issues go through the broker via the
`use-git-proxy` skill (no `gh` CLI).

### Ollama: local vs cloud

- **Cloud (default):** `OLLAMA_MODEL=glm-5.2:cloud` with the `./.ollama`
  device key registered (prerequisites #4). The daemon proxies to ollama.com and
  authenticates with that key — no `OLLAMA_API_KEY` needed.
- **Local:** pull a model into the daemon, then switch the model:
  ```sh
  docker compose exec ollama ollama pull qwen3:8b
  # in .env: OLLAMA_MODEL=qwen3:8b
  docker compose up -d   # restart claude to pick up the new model
  ```
  Local models run on CPU (no GPU on the target host).

### Dev dependencies (mysql, minio, postgres, …)

Not in the base compose. The agent stands them up inside the isolated DinD daemon
on demand — see the `use-docker` skill (e.g. `docker run -d --name mysql -e
MYSQL_ROOT_PASSWORD=... mysql:8`). Workload containers share `/workspace` only
and cannot reach git-proxy.

### Claude Code plugins (optional)

Plugins persist on the `claude-config` volume. Install from inside a session:
```sh
claude plugin install superpowers@claude-plugins-official
claude plugin install code-simplifier@claude-plugins-official
# …
```

---

## Credential-leak guarantees

- **The agent never receives the GitHub PAT.** It only has `AGENT_TOKEN`, a
  low-value bearer mapped to an auditable identity in `config.yaml` (`auth.tokens`).
  The PAT lives only in `credentials.yaml` (bind-mounted read-only into git-proxy)
  or the `GITHUB_TOKEN` env var; git-proxy attaches it on the proxy→GitHub leg.
- **No `gh` CLI, no direct GitHub API.** `git` traffic is rewritten to the proxy;
  PRs/issues/CI go through the broker. The agent has no token that grants upstream
  access.
- **Push policy** (`config.yaml`): `secret_scan` rejects secret-bearing pushes
  (redacted reasons), `history_protect` blocks force-push to `main`, `branch_pattern`
  restricts pushes to `main` + `feat/*`, `read.deny: ["secrets/**"]` withholds
  secret blobs from fetch.
- **Audit log** is append-only JSONL at `data/audit/audit.jsonl` with no credential
  content. No-leak check:
  ```sh
  grep -E 'ghp_|github_pat_|x-access-token' data/audit/audit.jsonl   # should be empty
  ```
- **No secrets in images.** All credentials are runtime env/bind-mounts; the
  `.dockerignore` excludes `.env`, `credentials.yaml`, `data/`, and runtime state
  from the `claude` build context.
- **Repo self-check** before committing:
  ```sh
  bash scripts/check-no-secrets.sh
  ```
  Fails on real PAT/key patterns in tracked files (`credentials.yaml` and
  `.env.example` use placeholders that do not match).

---

## Verification (end-to-end, on the Ubuntu host)

1. `curl http://127.0.0.1:8090/healthz` → `{"status":"ok"}`.
2. `docker compose exec claude env | grep -i token` → only `AGENT_TOKEN`, no `ghp_`/`github_pat_`.
3. From inside a `claude` session, clone the configured repo (rewritten to the
   proxy), push a `feat/test` branch, open a PR via the `use-git-proxy` skill.
   Confirm a push to `main` and a `--force` are both rejected by policy.
4. DinD isolation:
   ```sh
   docker compose exec claude docker run --rm -v /workspace:/work -w /work node:22-alpine node -e 'console.log(42)'
   # the DinD daemon CANNOT reach git-proxy:
   docker compose exec claude docker run --rm alpine sh -c 'wget -qO- http://git-proxy:8080 || echo blocked'
   ```
5. `bash scripts/check-no-secrets.sh` passes on a clean tree.

---

## Files

- `docker-compose.yaml` — the 4-service stack.
- `Dockerfile` — slim agent image (node:22-alpine + claude-code + git + docker-cli).
- `claude-code/entrypoint.sh` — sets `insteadOf`/`extraHeader`, drops skills + CLAUDE.md, `exec "$@"`.
- `claude-code/agent-context/CLAUDE.md` — always-loaded agent context (two execution surfaces, rules).
- `claude-code/use-git-proxy/SKILL.md` — git-protocol + broker REST skill (sourced from the git-proxy repo).
- `claude-code/use-docker/SKILL.md` — rootless DinD skill with mysql/minio/postgres recipes.
- `config.yaml` — git-proxy config (github upstream, broker, policy, audit).
- `credentials.yaml` — GitHub PAT profile (PLACEHOLDERS ONLY — never commit a real PAT).
- `.env.example` — operator secrets template (copy to `.env`).
- `setup-ubuntu-host.sh` — installs Docker + sysbox-ce on Ubuntu 24.04.
- `scripts/check-no-secrets.sh` — pre-commit secret scan backstop.

## Teardown

```sh
docker compose down -v        # removes containers + named volumes (workspace, docker-cache, claude-config)
sudo rm -rf data              # mirror cache + audit log
# ./.ollama (the registered device key + models) is a bind mount, NOT a named
# volume — `down -v` leaves it on the host. Remove it only if you want to.
sudo rm -rf data              # mirror cache + audit log
```