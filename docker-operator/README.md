# docker-operator

A Docker-native multi-agent orchestrator for ai-sandbox: create, list, watch
and delete Claude Code agent containers through a REST API and a small web
UI with a live terminal per agent — the same multi-agent, web-UI-driven
experience as the [Kubernetes operator](../operator/README.md), but on
plain Docker, single host, no cluster required. This is the repository's
**default** stack — `docker compose up` at the repo root brings it up. See
the root README's [docker-operator-vs-Kubernetes-operator
comparison](../README.md#two-ways-to-run-this-the-docker-operator-or-the-kubernetes-operator)
for when to reach for the cluster operator instead.

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
       │  volume: docker-operator-filestore (per-agent subpaths)    │
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
       │  /workspace/store  ── agents/<id>/ subpath (RW) of the shared │
       │  /workspace/shared ── shared/ subpath (RO) of the same       │
       │                       docker-operator-filestore volume       │
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
untracked. The same startup pass also walks every agent the store believes
is `running` and starts back up whichever of its DinD sidecar or agent
container the daemon reports as not actually running — the state a host or
Docker-daemon restart leaves behind when nothing carries a restart policy.
A container that's already running is left completely untouched; only the
agent container, if it needs restarting, is recreated (never a plain
`docker start`, since a stopped container's `claude` args are fixed at
create time) with `claude --resume` so it picks its previous session back
up. `POST /api/agents/{id}/update` performs the same DinD health check
before recreating the agent container, so an update against a sidecar that
died for any reason doesn't just trade one broken state for another.

**Each agent** gets its own DinD sidecar (a `docker:27-dind` + `sysbox-runc`
pair templated per agent, rather than one daemon shared by everyone) so it
keeps `use-docker` capability without sharing a daemon — or a network — with
any other agent: there is no one static DependaProxy IP shared by every
agent, so `claude-code/use-docker/SKILL.md` and
`claude-code/use-dependaproxy/SKILL.md`'s examples read
`/workspace/dependaproxy-ip` (written by `entrypoint.sh`) instead of a
literal. This Dockerfile is the only one in the repo that bakes any
`claude-code/*/SKILL.md` file (the single-agent compose stack this once also
served was retired), so those two are plain, unforked files — the one actual
fork is `docker-operator/agent/skills/use-git-proxy/SKILL.md`, since
`use-git-proxy` ships in the separate git-proxy repository:
`claude-code/use-git-proxy/SKILL.md` stays an untouched vendored copy (to
diff against upstream on the next re-vendor) while the fork trims the two
sections (installing the skill elsewhere, operator-only config notes) that
don't apply to an agent that already has it baked in.

**The terminal** is `tmux` inside the agent container, not a PTY the
operator owns: the WebSocket bridge (`internal/wsbridge`) is just a
`docker exec ... tmux attach-session -t main` wrapped in a two-way byte
pump, with resize forwarded as a small JSON control frame. Closing the
WebSocket ends only that exec — tmux, and the agent's `claude` process
inside it, keep running. This survives the operator process (or a browser
tab) restarting; it honestly does **not** survive the agent *container*
itself stopping or restarting, unlike the Kubernetes operator's
snapshot-based freeze/wake.

A mouse-wheel / two-finger scroll over that terminal scrolls the pane: the
bridge execs `tmux set-option -g mouse on` before attaching, so tmux
forwards the wheel to a mouse-aware full-screen app (claude scrolls its own
transcript) or, at a plain shell, scrolls tmux's history in copy-mode (which
exits as soon as you reach the live bottom). Without that option the wheel
would instead be turned into history-walking arrow keys, which
`shouldForwardWheel` in `web/terminal.js` also guards against.

Because tmux mouse mode is on, a mouse-aware full-screen app (`claude`)
enables mouse tracking and xterm.js forwards a plain click-drag to it as a
mouse sequence instead of making a native text selection — the same
trade-off every terminal emulator has. To **select** text from the pane, hold
<kbd>Shift</kbd> (<kbd>⌥</kbd> on macOS, via xterm.js's
`macOptionClickForcesSelection`) while dragging. To **copy** it, use
<kbd>Ctrl</kbd>+<kbd>Shift</kbd>+<kbd>C</kbd> — the plain <kbd>Ctrl</kbd>+<kbd>C</kbd>
stays the pty's SIGINT — or, on macOS, <kbd>⌘</kbd><kbd>C</kbd> (a browser
accelerator xterm.js leaves alone). <kbd>Ctrl</kbd>+<kbd>Shift</kbd>+<kbd>V</kbd>
pastes (`isCopyShortcut` / `isPasteShortcut` in `web/terminal.js`). The detail
view shows this hint under the terminal.

The pane runs in a UTF-8 locale (`LANG=C.UTF-8`, set in the agent image, the
agent environment, and the terminal-attach exec). tmux chooses its charset
from `LANG` / `LC_*`; with none set it falls back to a C/ASCII locale and
renders every non-ASCII byte — `ç`, `á`, accented Latin text — as `_`, both
in the pane and in what a viewer types. An agent whose container predates
this picks it up on its next terminal reconnect, no recreate needed.

To read or
search back through a whole session, the detail header's **View context**
button opens the agent's captured transcript
(`GET /api/agents/{id}/output`) in a searchable overlay. The raw capture is mostly TUI redraw frames, so it is first
replayed through a headless xterm.js (`replayCaptureToText`) — the redraws
collapse to their final on-screen state and the scrolled-off conversation
lands in scrollback — then read back as plain text (bounded to the last
`MAX_CONTEXT_LINES`). Type to filter and highlight, Enter / Shift+Enter to
walk matches, Refresh to re-pull, Esc to close.

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
| File-store volume | `docker-operator-filestore` | shared singleton; per-agent `agents/<id>/` (RW) + `shared/` (RO in every agent) subpaths, **unlabelled** |

Every resource above (except the shared singleton networks, the operator's own
network, and the shared file-store volume, none of which a single agent owns)
carries three labels —
`ai-sandbox.docker-operator/{managed,agent-id,role}` — the mechanism
`internal/agent.Reconcile` uses to tell an operator-owned resource from
anything else on the same Docker host.

## REST API

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/api/agents` | List agents + `max_agents` + the operator's `default_backend` / `default_model` / `default_fast_model` / `default_ollama_url` / `default_repo` / `default_auto_mode` (so the create form needs no second request). |
| `POST` | `/api/agents` | Create an agent. Body (all optional): `{"name","description","backend":"ollama"\|"anthropic","model","fast_model","ollama_url","repo","auto_mode":"on"\|"off"}`. `backend` defaults to the operator's `DEFAULT_AGENT_BACKEND`; `model`/`fast_model`/`ollama_url` are for `ollama` only (`400` with `anthropic`). `ollama_url` is an `http(s)` URL (`400` otherwise) overriding the operator's `OLLAMA_URL` for this one agent; blank falls back to that default. `repo` is `owner/repo(.git)` (`400` otherwise) and falls back to the operator's `GITHUB_REPO` — blank on both means the agent boots as a bare terminal. `auto_mode` (`400` on any other value) overrides the operator's `AGENT_AUTO_MODE` for this one agent; blank falls back to that default. `image_tag` pins this agent to a tag of the operator's agent-image repository (`400` on a malformed tag; not required to be a discovered one); blank uses the operator's `AGENT_IMAGE`. `409` at capacity, or `409` (`no_anthropic_auth`) for an `anthropic` agent when no credential is configured. |
| `GET` | `/api/agents/{id}` | Get one agent's record (includes `backend`, `model`, `fast_model`, `ollama_url`, `repo`, `auto_mode`). |
| `PATCH` | `/api/agents/{id}` | Rename and/or re-describe (`{"name","description"}`, either or both). |
| `DELETE` | `/api/agents/{id}` | Delete an agent and every resource it owns. Idempotent — always `200`. `?purge_files=true` also removes the agent's centralized file-store directory (default: files are kept); response carries `"files_purged"`. |
| `POST` | `/api/agents/{id}/update` | In-place update: recreate **only** the agent container under the same agent ID. Body is the `POST /api/agents` body (every create-form field is editable here, including `backend` and `image_tag`). The DinD sidecar, private network, dependaproxy attachment and the three volumes are kept — so `/workspace`, the Claude config and the DinD cache all survive — but the running tmux/`claude` session ends; the new container resumes it with `claude --continue` (history is in the preserved config volume). Only a `running`/`stopped`/`error` agent is updatable (`409 not_updatable` otherwise); `404` if unknown; `400` on a bad field; a failure after the old container is gone leaves the agent `error` with its volumes intact for a retry (no auto-rollback). |
| `GET` | `/api/files?path=` | List a file-store directory (`path=""` is the root). `501 filestore_disabled` when unconfigured. |
| `DELETE` | `/api/files?path=` | Delete a file or directory tree. Already-gone is `200`. `""`, `"agents"` and `"shared"` are `400`. `501` when unconfigured. |
| `GET` | `/api/files/download?path=` | Download one file (`application/octet-stream`). A directory is `400`. `501` when unconfigured. |
| `POST` | `/api/files/upload?path=<dir>` | Upload one or more files (`multipart/form-data`, each part named `file`). Over `FILESTORE_MAX_UPLOAD_BYTES` is `413`. `501` when unconfigured. |
| `POST` | `/api/files/mkdir` | Create a directory. Body `{"path":"…"}`. `501` when unconfigured. |
| `GET` | `/api/agents/{id}/output?tail=N` | The agent's captured pane output (raw text, not JSON-wrapped). Drives the detail view's **View context** overlay (whole log, ANSI stripped, client-side search); also there for automation. |
| `GET` | `/ws/agents/{id}/terminal` | WebSocket terminal bridge — binary frames are raw PTY bytes each way, a JSON text frame is `{"type":"resize","cols":N,"rows":N}`. |
| `GET`/`PUT`/`DELETE` | `/api/anthropic/auth` | Read / set / clear the shared Anthropic credential. `PUT` body: `{"kind":"api_key"\|"oauth","value":"…"}`. No response ever carries the value — only `{"configured","kind","updated_at"}`. |
| `GET`/`POST`/`DELETE` | `/api/anthropic/login` | Status / start / stop the `claude setup-token` helper container. `POST` returns `{"active":true,"ws":"/ws/anthropic/login/terminal"}`. |
| `GET` | `/api/agent-image/tags` | The discovered `:YYYYMMDD-HHMMSS` agent-image tags, newest-first: `{"tags":[…],"newest":"…","operator_default":"…","checked_at":"…"\|null,"last_error":"…"}`. Polled on a timer (`AGENT_IMAGE_REFRESH_INTERVAL`, default `1h`, floored at `1m`). |
| `POST` | `/api/agent-image/refresh` | Force a registry poll now, then return the same body as `GET /api/agent-image/tags`. A poll failure is **not** fatal — still `200`, with the last-known list kept and `last_error` populated. |
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

- **Ollama** (the default) — the agent's model traffic goes through an
  Ollama server. The form has an **Ollama server** field, blank with the
  operator's `OLLAMA_URL` shown as its placeholder — leave it blank to use
  that default, or point one agent at a different server (any `http(s)` URL).
  It also pre-fills two model names from the operator's `OLLAMA_MODEL` /
  `OLLAMA_FAST_MODEL` (`glm-5.3:cloud` / `glm-5.3-flash:cloud` by default)
  for the default/"opus" tier and the "sonnet"+"haiku" tiers; edit them per
  agent. The shared daemon authenticates `:cloud` models to ollama.com with
  the SSH keypair in `../.ollama` — no per-agent key.
- **Anthropic** — the agent talks to the real Anthropic API using the
  operator's **one shared credential** (see [Anthropic
  login](#anthropic-login)). Creating an `anthropic` agent before a
  credential is configured fails with `409 no_anthropic_auth`.

The backend, Ollama server and models are fixed once an agent is created
(changing them would need the container's environment rebuilt).
`DEFAULT_AGENT_BACKEND` sets which one the form (and an API request that
names none) starts on.

## Auto mode

Every agent's `claude` process can start in **auto mode**
(`--permission-mode auto`), where a classifier reviews tool calls instead of
stopping for interactive approval — the intended way to run an unattended,
containerised agent. `AGENT_AUTO_MODE` (default `true`) sets the operator-wide
default; the **New Agent** form's **Auto mode** field (`on`/`off`, or
"Operator default") overrides it per agent, backend-agnostic. The setting is
fixed once an agent is created or last updated — recreating the container
(`POST /api/agents/{id}/update`, or the startup reconcile pass waking a
stopped agent back up) is what applies a changed value.

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

## Centralized file store

Every agent gets a private directory it can use to **persist files past its own
deletion** — mounted at `/workspace/store` (and handed to the agent as
`$AGENT_STORE_DIR`) — plus a **read-only common area** at `/workspace/shared`
(`$AGENT_SHARED_DIR`) that every agent sees and only the operator writes. The
rest of `/workspace` is destroyed when the agent is deleted; neither of these
is.

**Topology.** One shared Docker volume, `docker-operator-filestore`, holds an
`agents/<id>/` subtree per agent plus a single `shared/` tree. The operator
pre-creates those before the agent is created, then mounts them into the agent
container as volume **subpaths**:

```
docker-operator-filestore   (one shared volume)
├── agents/
│   ├── agt_7f3a9c2d/   ─── mounted RW at /workspace/store in agent agt_7f3a9c2d
│   └── agt_1b2c3d4e/   ─── mounted RW at /workspace/store in agent agt_1b2c3d4e
└── shared/             ─── mounted READ-ONLY at /workspace/shared in EVERY agent
```

Docker enforces the isolation: an agent sees only its own `agents/<id>/`
subtree (never the volume root or another agent's) and a read-only view of
`shared/`. Agents never touch the operator API — the file API below is the
operator's, behind the same `OPERATOR_API_TOKEN`, and it is also how the
operator writes `shared/` (`POST /api/files/upload?path=shared`, etc.).

**`FILESTORE_DIR` and `FILESTORE_VOLUME` are two names for the same storage.**
`FILESTORE_DIR` is the path the *operator* sees the volume at (where it
pre-creates `agents/<id>/`); `FILESTORE_VOLUME` is the volume *name* the daemon
resolves each agent's subpath mount against. `docker-compose.yaml` pairs them
with a single `- filestore:/var/lib/docker-operator/filestore` mount. A
mismatch surfaces at agent-create time as `container create: subpath not
found`.

**Persistence contract.** An agent's files survive `DELETE /api/agents/{id}`,
create-failure rollback, and the startup reconcile pass. They are removed only
by `DELETE /api/agents/{id}?purge_files=true` or the web UI's file browser
(sidebar **Files**). An orphan `agents/<id>/` left by a lost record is left
alone — clean it up from the web UI.

**Requires Docker Engine >= 26.0 (API v1.45).** Volume-subpath mounts landed
there; an older daemon **silently ignores the subpath** and mounts the whole
volume, so every agent would see every other agent's files. Check `docker
version` before relying on this.

A single upload is capped at 100 MiB (`FILESTORE_MAX_UPLOAD_BYTES`). Set
`FILESTORE_DIR=""` to disable the whole feature: no `/api/files*` routes (they
answer `501 filestore_disabled`), no `/workspace/store` or `/workspace/shared`
mount, no `AGENT_STORE_DIR` / `AGENT_SHARED_DIR`. `docker compose down -v`
**does** delete the volume.

Agents working with the store get the `store-file` skill (baked into the
docker-operator agent image only) describing the `cp` recipes both ways.

## Quickstart

Run these from the **`docker-operator/`** directory. You need `docker` and
`docker compose` on `PATH`, and this repo checked out one level up (the
compose file reads `../config.yaml`, `../credentials.yaml`,
`../dependaproxy.yaml` — git-proxy/DependaProxy configuration at the repo
root, already committed with safe placeholders, see the root
[README's Quickstart](../README.md#quickstart)). Total time depends
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
`default_fast_model` / `default_ollama_url` / `default_repo`) on a fresh
operator; the second
confirms the embedded web UI (not a 404 or an error page) is being served
at `/`.

**4 — create an agent (needs `sysbox-runc`, see below)**

Clicking **+ New Agent** in the UI — filling in the form (name, description,
optional [repo](#choosing-a-repo), [backend](#choosing-a-backend), and for
Ollama the server URL and two model names) — or `curl -X POST -H "Authorization: Bearer
$OPERATOR_API_TOKEN" http://127.0.0.1:8000/api/agents -d '{"backend":"ollama"}'` —
creates the two containers, three volumes and private network described in
[Architecture](#architecture) above, then opens a live terminal running
`claude` inside a `tmux` session. For an `anthropic` agent, set the shared
credential first (sidebar **Anthropic account** panel — see [Anthropic
login](#anthropic-login)).

The agent image (`AGENT_IMAGE`, default
`ghcr.io/psenna/ai-sandbox-agent:latest`) is pulled on first use — it is
published to GHCR by
[`.github/workflows/docker-operator-agent-image.yml`](../.github/workflows/docker-operator-agent-image.yml)
on every `docker-operator/agent/**` change to `main`, tagged with a UTC
date-time plus `:latest`. Pin a date-time tag in `.env` for a reproducible
default, or run `make agent-image` to build and shadow `:latest` locally.

The operator polls the registry for those date-time tags
(`AGENT_IMAGE_REFRESH_INTERVAL`, default `1h`; `AGENT_IMAGE_REGISTRY_URL` /
`AGENT_IMAGE_REGISTRY_TOKEN` override the derived registry root / supply a
Bearer for a private repo) and surfaces them in the sidebar **Agent image**
panel and a per-agent dropdown on the create form. Creating an agent with a
non-default tag stamps the resolved reference on its record (`image`). Pin
`AGENT_IMAGE` to a `:YYYYMMDD-HHMMSS` tag — or create agents with an explicit
tag — so the per-agent upgrade prompts planned in follow-up issues have a
known starting point.

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
to an allow-listed set of external hosts, on top of what
`scripts/dind-init.sh` already blocks (the public npm/PyPI/Go registries).
Nothing here implements that yet; the per-agent
network boundary exists specifically so it can be added later without
rearchitecting anything above it.

## Development

Copy [`.env.example`](.env.example) to `.env` and fill it in — see the root
README's [Quickstart](../README.md#quickstart) for `config.yaml` /
`credentials.yaml`, which live at the repo root and this stack mounts via `../`.

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
