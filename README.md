# ai-sandbox

Run **Claude Code** agents on a real GitHub repo **without ever handing them the
GitHub PAT**, each agent in its own container with a **rootless Docker-in-Docker**
daemon for dev work, and every dependency **supply-chain-gated** through a
validating proxy.

- **git-proxy** holds the GitHub PAT and attaches it only on the proxy→GitHub
  leg. Agents authenticate to the proxy with a low-value bearer, never see the
  PAT, have no `gh` CLI, and every push is policy-gated (`secret_scan`,
  `history_protect`, `branch_pattern`) and audited.
- **DependaProxy** validates and hash-verifies every npm / PyPI / Go package; the
  public registries are network-blocked from the DinD daemon, so a workload
  cannot fetch a dependency any other way.
- **Ollama** serves an Anthropic-compatible API (local models, or `:cloud`
  models proxied to ollama.com) — or point an agent at a real Anthropic account.

```
 proxynet:  ollama · git-proxy ──https──> github.com   (holds the PAT)
            dependaproxy ──https──> registry.npmjs.org / pypi.org / proxy.golang.org
            every agent container ── git/PR ──> git-proxy
                                  ├── /v1/messages ──> ollama
                                  └── npm/pip/go ────> dependaproxy
 per-agent dinernet (private):  that agent's DinD sidecar (sysbox-runc)
 dbnet:  postgres <── dependaproxy   (trust-anchor storage; isolated)
```

---

## Two ways to run this: the docker-operator, or the Kubernetes operator

This repository ships **two** ways to run agents. Pick one before reading
further.

| | **docker-operator** (the default — `docker compose up`) | **Kubernetes operator** (`operator/`) |
|---|---|---|
| What it is | A single Go binary that creates agent containers through the Docker API, with a REST API + web UI. One Ubuntu host, no cluster. | A Kubernetes operator with two CRDs, `SandboxClass` and `SandboxEnvironment`. |
| How you start a run | Click **New agent** in the web UI (or `POST /api/agents`); a terminal opens on it. | `kubectl apply` a `SandboxEnvironment`. |
| How many at once | Many — one Claude container + one private DinD sidecar per agent, capped by `MAX_AGENTS`. | As many as `slots.capacity` allows, queued by priority. |
| Long-running / paused work | Not modelled — the agent's `tmux` session survives the operator or a browser tab restarting, but not the agent container stopping. | Modelled: an agent can declare a wait, the sandbox is **frozen** (snapshotted, pod deleted, slot released) and **woken** when the wait clears. |
| Isolation | A private Docker bridge network + a rootless DinD daemon per agent, registry egress blocked. The operator drives Docker over a bind-mounted socket and shares no network with any agent. | Kubernetes `NetworkPolicy`, a hardened pod, and no Kubernetes credential in the agent container. |
| Nested containers for dev work | Yes — a rootless DinD sidecar per agent (`DOCKER_HOST=tcp://docker:2375`). | **Not yet** — only the `none` engine is implemented ([#24](https://github.com/psenna/ai-sandbox/issues/24)). |
| Operational surface | `docker compose logs`, the web UI, `GET /api/agents`. | Conditions, Events, Prometheus metrics, a Helm chart. |
| Maturity | Working, in daily use. | `v1alpha1`; no image or chart published yet. |

**Use the docker-operator** when you want one or many agents on one machine,
right now, with a browser UI — and especially when agents need to launch
containers of their own (databases, language runtimes).

**Use the Kubernetes operator** when you want policy-isolated agent runs on a
cluster, or runs that must survive being paused for hours while CI or a review
completes.

Both share the same trust model — git-proxy holds the PAT, DependaProxy gates
every dependency — and the Kubernetes operator consumes the same git-proxy,
DependaProxy and Ollama endpoints.

Deep docs:
- **docker-operator:** [`docker-operator/README.md`](docker-operator/README.md) ·
  [quickstart](docker-operator/README.md#quickstart) ·
  [REST API](docker-operator/README.md#rest-api) ·
  [authenticating the API](docker-operator/README.md#authenticating-the-api)
- **Kubernetes operator:** [`operator/README.md`](operator/README.md) ·
  [quickstart](operator/README.md#quickstart) ·
  [engines](operator/docs/engines.md) ·
  [operations](operator/docs/operations.md) ·
  [security](operator/docs/security.md) ·
  [CRD reference](operator/docs/crd-reference.md)

---

## Architecture

Shared, singleton services (one set, reused by every agent):

| Service | Image | Purpose |
|---|---|---|
| `ollama` | `ollama/ollama:latest` | LLM server, Anthropic-compatible `/v1/messages`. `:cloud` + local models. |
| `git-proxy` | `ghcr.io/psenna/git-proxy:v0.0.11` | Policy gateway holding the GitHub PAT. `8080` (git) + `8090` (broker) on `127.0.0.1`. |
| `postgres` | `postgres:18` | DependaProxy's trust-anchor storage. Reachable only by `dependaproxy`. |
| `dependaproxy` | `ghcr.io/psenna/dependaproxy:v0.0.7` | Validates + hash-verifies every npm / PyPI / Go package; serves `/npm` `/pypi` `/goproxy` + an admin dashboard at `/`. |
| `docker-operator` | built from `docker-operator/Dockerfile` | The orchestrator: REST API + web UI on `127.0.0.1:${LISTEN_PORT:-8000}`, drives Docker over a bind-mounted socket. |

Then, **per agent, on demand**, the operator creates: a Claude Code container
(node + claude-code + git + docker-cli + tmux, from
`docker-operator/agent/Dockerfile`), a private `docker:dind` sidecar under
`sysbox-runc`, a private bridge network joining just those two, and three
isolated volumes (workspace, Claude config, DinD cache). No agent shares a
network or a volume with any other agent, and none can reach the operator.

DependaProxy is connected into each agent's private network at create time, so
that agent's DinD workloads can reach it; the DinD daemon
(`scripts/dind-init.sh`) blocks egress to the public npm / PyPI / Go hosts, so
workloads physically cannot fetch a dependency outside DependaProxy.

Full topology, resource naming, and the security boundary:
[`docker-operator/README.md#architecture`](docker-operator/README.md#architecture).

---

## Prerequisites

1. **Ubuntu 24.04 LTS** host (amd64 or arm64), systemd. Each agent's DinD sidecar
   needs the `sysbox-runc` runtime, so:
   ```sh
   sudo bash setup-ubuntu-host.sh
   ```
   This installs Docker Engine + containerd (pinned + held) and sysbox-ce, and
   verifies `docker run --runtime=sysbox-runc --rm alpine echo ok`.

2. **`ghcr.io/psenna/git-proxy:v0.0.11`** published (the git-proxy repo's
   `release` workflow pushes it to GHCR on every GitHub release). v0.0.11 adds
   `ci.status` / `ci.log` graceful degradation when the PAT can read Actions but
   not Checks, plus a warn-only startup permission preflight. v0.0.10 landed the
   security review (read-protection bypasses, ref-update object-id validation,
   per-agent repo authorization, fail-closed auth when `auth.tokens` is unset).
   **Do not pin v0.0.9** — its release pipeline failed and published no image.
   If a newer tag is out, bump the `git-proxy` `image:` line in
   `docker-operator/docker-compose.yaml`.

3. **`ghcr.io/psenna/dependaproxy:v0.0.7`** published (the dependaproxy repo's
   `release` workflow pushes it on every GitHub release). v0.0.6/v0.0.7 land the
   security review (PyPI file-version binding, a per-project validated-artifact
   trust store, provenance bound to the served digest). The sandbox runs it with
   auth disabled (`auth.token: ""` in `dependaproxy.yaml`).

4. **A GitHub fine-grained PAT** for the repo(s) agents will work on
   (https://github.com/settings/personal-access-tokens):
   - Read access to **actions** and **metadata**
   - Read and write access to **code**, **issues**, **pull requests**, and **workflows**

5. **Ollama Cloud device auth** (only for `:cloud` models). The local daemon
   authenticates to ollama.com with the SSH keypair in `./.ollama` (`id_ed25519`
   + `config.json`) — **not** an API key. Either copy your existing `.ollama/`
   folder onto the host next to `docker-compose.yaml`, or start fresh:
   `docker compose up -d ollama` then `docker compose exec ollama ollama signin`
   once. (`OLLAMA_API_KEY` is only for direct ollama.com API calls, which this
   stack does not make.)

---

## Quickstart

```sh
# 1. Host runtime (once).
sudo bash setup-ubuntu-host.sh

# 2. Give git-proxy the GitHub PAT — edit credentials.yaml: put the PAT in
#    `password` AND `token`, set the `repos` pattern to your OWNER/REPO.git.
#    (Or export GITHUB_TOKEN and pass it to the git-proxy container.)
#    NEVER commit a real PAT — credentials.yaml is tracked with placeholders.

# 3. Configure the stack.
cp .env.example .env
#    edit .env: OPERATOR_API_TOKEN (openssl rand -hex 32), GITHUB_REPO,
#    OLLAMA_MODEL / DEFAULT_AGENT_BACKEND, MAX_AGENTS.
#    For :cloud models, put your registered .ollama/ folder next to this file.
mkdir -p docker-operator/data/mirror docker-operator/data/audit docker-operator/data/dependaproxy-cache

# 4. Bring up the shared services + the operator.
docker compose up -d
docker compose ps
```

Then open **`http://127.0.0.1:8000/?token=<OPERATOR_API_TOKEN>`** (the UI stores
the token and strips it from the URL), click **New agent**, and a terminal
opens on it. Or drive the REST API:

```sh
curl -fsS -H "Authorization: Bearer $OPERATOR_API_TOKEN" http://127.0.0.1:8000/api/agents
```

`docker compose up` here just `include:`s `docker-operator/docker-compose.yaml`;
run `docker compose` from `docker-operator/` instead if you prefer (put `.env`
there — see `docker-operator/.env.example`). Everything else — the web UI
walkthrough, per-agent backends, the full REST API, authenticating the API,
in-place agent image upgrades, troubleshooting — is in
[`docker-operator/README.md`](docker-operator/README.md).

> **uid note:** `git-proxy` runs as uid 1000, `dependaproxy` as uid 65532. If the
> `docker-operator/data/` dirs aren't writable by their uid, run
> `sudo chown -R 1000:1000 docker-operator/data/{mirror,audit} && sudo chown -R 65532:65532 docker-operator/data/dependaproxy-cache`.

---

## Credential-leak guarantees

- **Agents never receive the GitHub PAT.** Each has only `AGENT_TOKEN`, a
  low-value bearer mapped to an auditable identity in `config.yaml`
  (`auth.tokens`). The PAT lives only in `credentials.yaml` (bind-mounted
  read-only into git-proxy) or the `GITHUB_TOKEN` env var; git-proxy attaches it
  on the proxy→GitHub leg.
- **No `gh` CLI, no direct GitHub API.** `git` traffic is rewritten to the proxy;
  PRs / issues / CI go through the broker.
- **The operator's Docker socket never reaches an agent.** The operator sits on
  its own network, joined by nothing else, and drives agents through the socket —
  never over the network. It logs a startup warning if it finds itself reachable
  from the agent network.
- **Push policy** (`config.yaml`): `secret_scan` rejects secret-bearing pushes
  (redacted reasons), `history_protect` blocks force-push to `main`,
  `branch_pattern` restricts pushes to `main` + `feat/*`,
  `read.deny: ["secrets/**"]` withholds secret blobs from fetch.
- **Audit log** is append-only JSONL at `docker-operator/data/audit/audit.jsonl`
  with no credential content:
  ```sh
  grep -E 'ghp_|github_pat_|x-access-token' docker-operator/data/audit/audit.jsonl   # should be empty
  ```
- **No secrets in images.** All credentials are runtime env / bind-mounts; the
  root `.dockerignore` keeps `.env`, `credentials.yaml`, `config.yaml`,
  `dependaproxy.yaml` and runtime state out of the operator image build contexts.
- **Dependencies are proxy-only.** The public npm / PyPI / Go registries are
  network-blocked from every DinD daemon, and DependaProxy auth is disabled (the
  proxy is on isolated internal networks), so there is no proxy token to leak.
- **Repo self-check** before committing:
  ```sh
  bash scripts/check-no-secrets.sh
  ```

---

## Files

- `docker-compose.yaml` — the default stack; a thin `include:` of
  `docker-operator/docker-compose.yaml`.
- `docker-operator/` — the Docker-native multi-agent orchestrator: the Go binary,
  the agent image (`docker-operator/agent/`), its `docker-compose.yaml`, and its
  own README. **Start here.**
- `operator/` — the Kubernetes operator: `SandboxClass` / `SandboxEnvironment`
  CRDs, the reconciler, the `sandboxctl` sidecar, a Helm chart.
- `claude-code/` — assets baked into the agent images: `entrypoint.sh`,
  `agent-context/CLAUDE.md`, and the `use-git-proxy` / `use-dependaproxy` /
  `implement-issue` / `store-file` skills. (`use-docker` for the docker-operator
  agent is a local fork in `docker-operator/agent/skills/`.)
- `config.yaml` — git-proxy config (upstream, broker, policy, audit).
- `credentials.yaml` — GitHub PAT profile (PLACEHOLDERS ONLY — never commit a
  real PAT).
- `dependaproxy.yaml` — DependaProxy config (registries, postgres DSN).
- `scripts/dind-init.sh` — DinD entrypoint override that blocks egress to the
  public npm / PyPI / Go registries (the single source of truth; the operator
  embeds a copy).
- `scripts/check-no-secrets.sh` — pre-commit secret-scan backstop.
- `.env.example` — the stack's `.env` template (copy to `.env` at the repo root).
- `setup-ubuntu-host.sh` — installs Docker + sysbox-ce on Ubuntu 24.04.

---

## Teardown

```sh
docker compose down -v        # containers + volumes (docker-operator_pgdata,
                              # docker-operator_operator-state, docker-operator-filestore)
sudo rm -rf docker-operator/data   # git-proxy mirror + audit + dependaproxy cache
# ./.ollama (the registered device key + models) is a bind mount, not a named
# volume — `down -v` leaves it. Any running agents' containers/volumes are the
# operator's to remove (DELETE /api/agents/{id}, or the web UI) — `down` does not
# touch them.
```
