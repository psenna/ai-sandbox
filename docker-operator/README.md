# docker-operator

A Docker-native multi-agent orchestrator for ai-sandbox: create, list, watch
and delete Claude Code agent containers through a REST API and a small web
UI with a live terminal per agent — the same multi-agent, web-UI-driven
experience as the [Kubernetes operator](../operator/README.md), but on
plain Docker, single host, no cluster required. See the root README's
[compose-vs-operator
comparison](../README.md#two-ways-to-run-this-the-compose-stack-or-the-kubernetes-operator)
for how this fits next to the other two ways to run this repo.

V1 is a local-only tool: it binds `127.0.0.1`, and one shared GitHub repo +
token serves every agent. The operator's own REST API and terminal
WebSockets take an optional static Bearer (`OPERATOR_API_TOKEN` — see
[Authenticating the API](#authenticating-the-api)); the operator sits on its
own Docker network that no agent joins. See [V2: network-egress
restriction](#v2-network-egress-restriction) for the one direction this
design deliberately leaves open.

## What it does

A single Go binary (`docker-operator`) that:

- Creates two containers per agent (the Claude agent itself + a private
  Docker-in-Docker sidecar, so the agent keeps its own `use-docker`
  capability, isolated from every other agent) plus three isolated volumes
  and one private network — never shared with any other agent.
- Serves a REST API (`/api/agents`) and a WebSocket terminal bridge
  (`/ws/agents/{id}/terminal`) backed by `tmux`, so a browser tab
  disconnecting — or the operator itself restarting — never kills the
  agent's session.
- Serves a small web UI (sidebar + terminal) at `/`, embedded in the binary.
- Enforces a hard `MAX_AGENTS` cap, checked atomically before any Docker
  resource is touched.
- Lets each agent be created against a **per-agent LLM backend** — the
  shared Ollama daemon (with per-agent model names) or a real Anthropic
  account — chosen on the create form. See [Choosing a
  backend](#choosing-a-backend).

## Architecture

```
                    ┌───────────────────────────────┐
                    │  docker-operator (1 process)    │
                    │                                 │
                    │  internal/agent.Manager          │
                    │   Create / Delete / Reconcile     │
                    │                                 │
                    │  internal/store (BoltDB)          │
                    │   agent records, atomic            │
                    │   MAX_AGENTS reservation            │
                    │                                 │
                    │  HTTP: /api/agents/*    (REST)    │
                    │        /ws/agents/*/terminal      │
                    │        /              (web UI)     │
                    └───────────────┬─────────────────┘
                                    │ Docker API (unix socket) — the operator
                                    │ drives everything below through this,
                                    │ never over the network; it sits alone on
                                    │ operatornet, joined by no agent.
                                    ▼
       ┌─────────────────────────────────────────────────────────┐
       │  shared singletons (created once, reused by every agent)  │
       │  ollama · git-proxy · postgres · dependaproxy               │
       │  networks: proxynet, dbnet   (operator NOT on either)      │
       └─────────────────────────────────────────────────────────┘
                                    │
                                    │ per agent, on demand
                                    ▼
       ┌─────────────────────────────────────────────────────────┐
       │  agent-<id>-dinernet (bridge, PRIVATE to this one agent)    │
       │                                                           │
       │  agent-<id>            (Claude Code + tmux, on proxynet     │
       │                          AND this dinernet)                 │
       │  dind-<id>             (docker:27-dind, sysbox-runc)        │
       │  volumes: <id>-workspace, <id>-claude-config, <id>-dind-cache│
       │  dependaproxy is connected into this dinernet at create      │
       │  time, so DinD workload containers can reach it too          │
       └─────────────────────────────────────────────────────────┘
```

**The operator** is a single process, no leader election, no cluster state
— `MAX_AGENTS` enforcement is one mutex-guarded reservation in a local
BoltDB file (`internal/store`), which is also the sole source of truth
`internal/agent.Reconcile` cross-references against every Docker resource
carrying the `ai-sandbox.docker-operator/managed` label on startup, so a
mid-operation crash never leaves an orphaned container/volume/network
untracked.

**Each agent** gets its own DinD sidecar (mirrors the root compose stack's
single `docker:27-dind` + `sysbox-runc`, templated per agent instead of a
singleton) so it keeps `use-docker` capability without sharing a daemon —
or a network — with any other agent. `docker-operator/agent/skills/
use-docker/SKILL.md` is a local fork of the root stack's skill for exactly
this reason: there is no longer one static DependaProxy IP shared by every
agent, so its examples read `/workspace/dependaproxy-ip` (written by
`entrypoint.sh`) instead.

**The terminal** is `tmux` inside the agent container, not a PTY the
operator owns: the WebSocket bridge (`internal/wsbridge`) is just a
`docker exec ... tmux attach-session -t main` wrapped in a two-way byte
pump, with resize forwarded as a small JSON control frame. Closing the
WebSocket ends only that exec — tmux, and the agent's `claude` process
inside it, keep running. This survives the operator process (or a browser
tab) restarting; it honestly does **not** survive the agent *container*
itself stopping or restarting, unlike the Kubernetes operator's
snapshot-based freeze/wake.

## Resource naming reference

| Resource | Name | Scope |
|---|---|---|
| Agent container | `docker-operator-agent-<id>` | per agent |
| DinD sidecar container | `docker-operator-dind-<id>` | per agent |
| Workspace volume | `docker-operator-agent-<id>-workspace` | per agent, isolated |
| Claude config volume | `docker-operator-agent-<id>-claude-config` | per agent, isolated |
| DinD image-cache volume | `docker-operator-agent-<id>-dind-cache` | per agent, isolated |
| Private network | `docker-operator-agent-<id>-dinernet` | per agent, private |
| Proxy network | `docker-operator-proxynet` | shared singleton (agents + shared services; **not** the operator) |
| DB network | `docker-operator-dbnet` | shared singleton |
| Operator network | `docker-operator-operatornet` | singleton, operator only — no agent joins it |

Every resource above (except the shared singleton networks and the
operator's own network, which no single agent owns) carries three labels —
`ai-sandbox.docker-operator/{managed,agent-id,role}` — the mechanism
`internal/agent.Reconcile` uses to tell an operator-owned resource from
anything else on the same Docker host.

## REST API

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/api/agents` | List agents + `max_agents` + the operator's `default_backend` / `default_model` / `default_fast_model` / `default_repo` (so the create form needs no second request). |
| `POST` | `/api/agents` | Create an agent. Body (all optional): `{"name","description","backend":"ollama"\|"anthropic","model","fast_model","repo"}`. `backend` defaults to the operator's `DEFAULT_AGENT_BACKEND`; `model`/`fast_model` are for `ollama` only (`400` with `anthropic`). `repo` is `owner/repo(.git)` (`400` otherwise) and falls back to the operator's `GITHUB_REPO` — blank on both means the agent boots as a bare terminal. `409` at capacity, or `409` (`no_anthropic_auth`) for an `anthropic` agent when no credential is configured. |
| `GET` | `/api/agents/{id}` | Get one agent's record (includes `backend`, `model`, `fast_model`, `repo`). |
| `PATCH` | `/api/agents/{id}` | Rename and/or re-describe (`{"name","description"}`, either or both). |
| `DELETE` | `/api/agents/{id}` | Delete an agent and every resource it owns. Idempotent — always `200`. |
| `GET` | `/api/agents/{id}/output?tail=N` | The agent's captured pane output (raw text, not JSON-wrapped). Unused by the UI today; exists for future automation. |
| `GET` | `/ws/agents/{id}/terminal` | WebSocket terminal bridge — binary frames are raw PTY bytes each way, a JSON text frame is `{"type":"resize","cols":N,"rows":N}`. |
| `GET`/`PUT`/`DELETE` | `/api/anthropic/auth` | Read / set / clear the shared Anthropic credential. `PUT` body: `{"kind":"api_key"\|"oauth","value":"…"}`. No response ever carries the value — only `{"configured","kind","updated_at"}`. |
| `GET`/`POST`/`DELETE` | `/api/anthropic/login` | Status / start / stop the `claude setup-token` helper container. `POST` returns `{"active":true,"ws":"/ws/anthropic/login/terminal"}`. |
| `GET` | `/ws/anthropic/login/terminal` | Terminal bridge into the login helper container (same frame protocol as the agent terminal). |

### Authenticating the API

Set `OPERATOR_API_TOKEN` (env / `--api-token`) to a long random string and
every `/api/*` request and both `/ws/*` terminal connections require it:

- **curl / scripts:** `-H "Authorization: Bearer $OPERATOR_API_TOKEN"`.
- **browser:** open the UI once as `http://127.0.0.1:8000/?token=<value>` —
  `web/auth.js` saves it to `localStorage`, strips it from the address bar,
  and attaches it to every request (and, as `?token=`, to the terminal
  WebSocket, which a browser cannot give a header). A `401` re-prompts.
- **`GET /healthz`** and the static web-UI assets (`/`, `/app.js`, …) stay
  open — they carry no data and ship no secret.

Leaving `OPERATOR_API_TOKEN` unset disables the check; the operator logs a
`SECURITY:` warning at startup. The token is **not** `AGENT_TOKEN` (that one
is git-proxy's, handed to agents); agents never receive `OPERATOR_API_TOKEN`
and — since the operator is on its own network — cannot reach this API at
all.

## Choosing a backend

Every agent is created against one LLM backend, picked on the **New Agent**
form:

- **Ollama** (the default) — the agent's model traffic goes through the
  shared `ollama` daemon. The form pre-fills two model names from the
  operator's `OLLAMA_MODEL` / `OLLAMA_FAST_MODEL`
  (`glm-5.3:cloud` / `glm-5.3-flash:cloud` by default) for the
  default/"opus" tier and the "sonnet"+"haiku" tiers; edit them per agent.
  The daemon authenticates `:cloud` models to ollama.com with the SSH
  keypair in `../.ollama` — no per-agent key.
- **Anthropic** — the agent talks to the real Anthropic API using the
  operator's **one shared credential** (see [Anthropic
  login](#anthropic-login)). Creating an `anthropic` agent before a
  credential is configured fails with `409 no_anthropic_auth`.

The backend and models are fixed once an agent is created (changing them
would need the container's environment rebuilt). `DEFAULT_AGENT_BACKEND`
sets which one the form (and an API request that names none) starts on.

## Choosing a repo

The **New Agent** form has an optional **Repository** field (`owner/repo` or
`owner/repo.git`). It is pre-filled from the operator's `GITHUB_REPO` and
overridable per agent; leave it blank on both and the agent boots as a bare
Claude terminal. **Nothing is auto-cloned** either way — whoever drives the
agent runs the first `git clone` in the terminal, which the entrypoint has
already routed through git-proxy (`https://github.com/… → git-proxy`, Bearer
attached). On-demand cloning only works for repos git-proxy is configured to
serve (its `credentials.yaml`). The chosen repo rides in the container as
`GITHUB_REPO` as a hint, and shows in the agent's detail header.

## Anthropic login

The shared Anthropic credential is set from the sidebar's **Anthropic
account** panel and used by **every** `anthropic` agent — injected into the
container at create time (changing it later only affects agents created
after). Two kinds:

- **API key** — paste an `sk-ant-…` Anthropic Console key. Injected as
  `ANTHROPIC_API_KEY`. Pay-per-token Console billing.
- **OAuth token (Claude subscription)** — click **Log in**: the operator
  spins a throwaway container running `claude setup-token`, wired to a
  terminal in the main area. Complete the sign-in in your browser, copy the
  token it prints, paste it into the field. Injected as
  `CLAUDE_CODE_OAUTH_TOKEN`; uses your Claude Pro/Max subscription. The
  helper container is torn down once the token is stored, on an explicit
  cancel, after a 20-minute idle timeout, and at operator startup.

Either value is whitespace-trimmed before it is stored; a paste that still
contains interior whitespace (a token hard-wrapped by an 80-column terminal,
say) is rejected rather than silently injected as an unusable bearer.

The credential lives in the operator's BoltDB state file (0600, same volume
and trust boundary as every agent record); no API response ever returns its
value. `bash ../scripts/check-no-secrets.sh` still passes — nothing lands in
a tracked file.

## Quickstart

Run these from the **`docker-operator/`** directory. You need `docker` and
`docker compose` on `PATH`, and this repo checked out one level up (the
compose file reads `../config.yaml`, `../credentials.yaml`,
`../dependaproxy.yaml` — git-proxy/DependaProxy configuration shared with
the root compose stack, already committed with safe placeholders, see the
root [README's Setup section](../README.md#setup)). Total time depends
mostly on pulling the `ollama` image.

**Every command in this section is executed verbatim in CI** by
`hack/quickstart-check.sh` (job `quickstart` in
`.github/workflows/docker-operator-docs.yml`), which extracts these fenced
blocks from this file and runs them against a real Docker daemon. If you
can read it here, CI ran it.

**1 — bring up the shared services + the operator**

```sh quickstart
export AGENT_TOKEN=agent-token-1
# Gate the operator's own API + terminals. Optional (blank = open, with a
# startup warning); generate a fresh random value rather than hardcoding one.
export OPERATOR_API_TOKEN="$(openssl rand -hex 32)"
# GITHUB_REPO is optional -- the default repo agents fall back to. Unset it
# and agents boot as bare terminals; each can still be pinned to its own repo
# on the create form.
export GITHUB_REPO=psenna/ai-sandbox.git
docker compose up -d --build
```

**2 — wait for it, then open the UI**

```sh quickstart
timeout 180 sh -c 'until curl -fsS http://127.0.0.1:8000/healthz >/dev/null; do sleep 2; done'
curl -fsS http://127.0.0.1:8000/healthz
```

Open <http://localhost:8000> in a browser: a sidebar with a **+ New Agent**
button and an empty agent list (`0 of 5 agents`).

**3 — the API the UI drives**

```sh quickstart
curl -fsS -H "Authorization: Bearer $OPERATOR_API_TOKEN" http://127.0.0.1:8000/api/agents
curl -fsS http://127.0.0.1:8000/ | grep -o '<title>[^<]*</title>'
```

The first line prints an empty agent list plus `max_agents` and the
operator's create-form defaults (`default_backend` / `default_model` /
`default_fast_model` / `default_repo`) on a fresh operator; the second
confirms the embedded web UI (not a 404 or an error page) is being served
at `/`.

**4 — create an agent (needs `sysbox-runc`, see below)**

Clicking **+ New Agent** in the UI — filling in the form (name, description,
optional [repo](#choosing-a-repo), [backend](#choosing-a-backend), and for
Ollama the two model names) — or `curl -X POST -H "Authorization: Bearer
$OPERATOR_API_TOKEN" http://127.0.0.1:8000/api/agents -d '{"backend":"ollama"}'` —
creates the two containers, three volumes and private network described in
[Architecture](#architecture) above, then opens a live terminal running
`claude` inside a `tmux` session. For an `anthropic` agent, set the shared
credential first (sidebar **Anthropic account** panel — see [Anthropic
login](#anthropic-login)).

**This step needs `sysbox-runc` installed on the Docker host** (unprivileged
Docker-in-Docker for the agent's own DinD sidecar; see
[`../setup-ubuntu-host.sh`](../setup-ubuntu-host.sh)) — most default Docker
installs do not have it, including plain GitHub Actions runners, which is
why this step is deliberately **not** tagged `quickstart` and does not run
in the automated check above (tracked in #80: a self-hosted sysbox runner
for the integration-test CI job). Everything above it — the compose stack,
the API, the embedded UI — needs no special runtime and is verified for
real on every push.

**5 — tear down**

```sh quickstart
docker compose down -v
```

`down -v` removes the shared singleton services and their volumes. Any
per-agent containers/volumes/networks the operator itself created (step 4)
are the operator's own responsibility to clean up via `DELETE
/api/agents/{id}` (or the UI's Delete button) **before** tearing down the
stack — `docker compose down` only ever touches what `docker-compose.yaml`
itself declares.

## V2: network-egress restriction

Each agent's `dinernet` is already private and per-agent (never shared, see
[Resource naming](#resource-naming-reference)) — the seam a squid-like
forward proxy would sit on to restrict an agent's DinD workload containers
to an allow-listed set of external hosts, the same way the root compose
stack's single shared `dinernet` restricts everyone today via
`scripts/dind-init.sh`. Nothing here implements that yet; the per-agent
network boundary exists specifically so it can be added later without
rearchitecting anything above it.

## Development

Copy [`.env.example`](.env.example) to `.env` and fill it in — see the root
README's [Setup section](../README.md#setup) for `config.yaml`/
`credentials.yaml`, which this stack shares with the root compose stack.

Before committing, run the repo-wide secret scan from the repository root:

```sh
bash ../scripts/check-no-secrets.sh
```

or wire it as a git pre-commit hook once, same as `scripts/check-no-secrets.sh`'s own header comment recommends:

```sh
ln -s ../../scripts/check-no-secrets.sh ../.git/hooks/pre-commit
```

See the [Makefile](Makefile) for every check CI runs (`vet`, `fmt-check`,
`lint`, `vuln`, `test`, `web-test`, `skill-check`, `dind-init-check`,
`web-embed-check`) — `make all` runs the lot. Go and Node both run inside
disposable containers (see the Makefile's header comment); no host
toolchain is assumed. `make agent-image` builds the agent image locally;
`make agent-image-smoke` checks its contents without needing
`AGENT_TOKEN`/`GITHUB_REPO` or a writable workspace.
