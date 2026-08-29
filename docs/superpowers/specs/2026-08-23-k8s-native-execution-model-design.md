# Design: k8s-native execution model (issue #24 re-scoped)

**Status:** Primary design for issue #24 (2026-08-23). Implements the engine as
k8s-native rather than rootless-podman. The previously-saved Option C design
(`docs/superpowers/specs/2026-08-23-rootless-podman-dev-tun-design.md`) is kept
as a recorded fallback only and is not implemented.

**Branch:** `feat/24-k8s-native-engine`, branched from `main`. On `main`,
`rootless-podman` is an unimplemented stub (`notImplementedEngine`, fails
closed — issue #24 was deferred). This design implements issue #24 as a
k8s-native engine instead of the rootless-podman-in-a-pod approach, so it is
greenfield addition on the `main` baseline, not retirement of a working engine.
The earlier `feat/24-rootless-podman-engine` branch (which did implement
podman-in-a-pod) is abandoned.

## Why k8s-native (not rootless-podman)

The rootless-podman approach nests a container runtime inside an unprivileged
k8s pod. Every hard problem in that approach comes from the **nesting**:

- `/dev/net/tun` — pasta (podman 5.x's default rootless network) needs it to
  create the TAP device; kind does not expose it to pods; only a hostPath
  `CharDevice` can provide it in an unprivileged pod (no
  `securityContext.devices`, device cgroup blocks `mknod`, privileged is
  worse, no DRA in kind).
- cgroup delegation — kind does not delegate cgroups; rootless podman needs
  `cgroups="disabled"` + `default_sysctls=[]` to get past the post-network
  step (`podman#16405`).
- the security argument — the agent inherits anything the sidecar sees via
  `docker run -v`, so the engine leans on "the sidecar holds nothing worth
  stealing," which a hostPath `/dev/net/tun` weakens.
- kind-vs-production divergence — the `containers.conf` overrides and the
  `/dev/net/tun` mount exist only because of kind's limitations; production
  nodes behave differently, so CI does not prove production behavior.

The cluster already runs containerd. The reframe: **don't nest a runtime;
provision dependencies and dev-tool runtimes as native k8s Pods/Services in
the agent's namespace.** This dissolves the entire problem class — no
`/dev/net/tun`, no hostPath, no cgroup delegation, no `containers.conf`, no
kind-vs-production divergence — and shifts isolation from pasta's per-container
network namespace to the cluster's NetworkPolicy, which is identical in kind
and production.

## Decisions (locked in brainstorming)

1. k8s-native **replaces** the rootless-podman engine. PR #54 is re-scoped to
   deliver the k8s-native engine. Option C stays a recorded fallback.
2. Dependencies and dev-tool runtimes are **native k8s Pods/Services** in the
   agent's namespace, declared as code in `services.yaml`, reconciled by the
   operator. The agent never nests a container runtime.
3. Dev-tool runtimes are **declared long-lived** pods the agent `exec`s into,
   not ephemeral run-pods. Switching a runtime version edits the file and
   recreates one pod — the agent pod is never recreated.
4. The agent also renders an **equivalent `docker-compose.yml`** from the same
   declaration for local/CI/human `docker compose up`.

## 1. The declaration — `services.yaml`

A simple YAML file in the workspace. Two top-level keys; both reconcile to
Pods but differ in what they expose and mount.

```yaml
# services.yaml — declared long-lived pods the operator reconciles in the
# agent's namespace. Edit at runtime and `sandboxctl services apply`; the
# operator diffs and recreates only what changed. The agent also renders an
# equivalent docker-compose.yml from this file for local/CI/human runs.
services:               # long-lived dependency pods (postgres, minio, ...)
  - name: postgres      # -> in-cluster Service postgres.<ns>.svc (the DNS the agent uses)
    image: postgres:18-alpine   # version = the tag; bump it to switch on the fly
    ports: [5432]        # -> Service port(s); cluster-internal by default
    env:
      POSTGRES_USER: e2e
      POSTGRES_PASSWORD: e2e
      POSTGRES_DB: e2e
    storage:            # persistent data; own PVC, retained by name across pod recreate
      size: 1Gi
      mountPath: /var/lib/postgresql/data
    healthcheck:        # readiness gate; the operator and agent know "postgres is ready"
      exec: ["pg_isready", "-U", "e2e"]
      interval: 5s
    expose: null         # opt-in host port for human inspection (default: cluster-only)
    dependsOn: []

runtimes:               # long-lived dev-tool pods (python, node) — exec targets
  - name: python
    image: python:3.13-slim
    mountWorkspace: true # -> shared RWX workspace PVC (agent + all runtimes)
    command: ["sleep", "infinity"]  # stay up so the agent can exec into it
    healthcheck: { exec: ["true"], interval: 30s }
```

### Field set

**Shared fields** (services and runtimes):
- `name` — identity key. Becomes the Pod + Service name and the in-cluster DNS
  (`<name>.<ns>.svc`) for services.
- `image` — the container image; the tag carries the version. Bump the tag to
  switch version on the fly (operator recreates only this pod).
- `env` — literal environment variables (a DB's config).
- `envFromSecret` — reference an operator-managed secret for real (non-sandbox)
  passwords instead of literals; reuses the operator's existing `Credentials`.
- `command`, `args` — override the image entrypoint (needed for images like
  minio, and for `sleep infinity` on runtimes).
- `resources` — CPU/memory requests/limits; sane defaults if omitted so a
  runaway dep cannot eat the node.
- `restart` — `always` for long-lived (default).
- `imagePullPolicy`, `runAsUser` — sensible defaults, overridable.
- `healthcheck` — readiness probe: `exec` (a command), `http` (a path + port),
  or `tcp` (a port); plus `interval`. Maps to the k8s readinessProbe. Drives
  the per-entry `Ready` status condition.
- `dependsOn` — names of other entries; the operator gates a dependent's
  `Ready` on its dependencies' healthchecks passing (so the agent does not
  race a not-yet-ready dependency).

**Service-only fields:**
- `ports` — list of ports exposed on the in-cluster Service.
- `storage` — `{ size, mountPath }`; creates a per-service PVC (RWO) so a
  stateful dep survives pod recreate. Retained by name. Without this, a dep's
  data is ephemeral (lost on recreate).
- `expose` — opt-in host port (NodePort) so a human can inspect a UI; default
  cluster-only. Aligns with the NetworkPolicy story (deps are private to the
  agent unless explicitly exposed).

**Runtime-only fields:**
- `mountWorkspace` (default `true`) — mount the shared RWX workspace PVC so
  files written by the agent are immediately visible to the runtime and
  vice-versa.

### Design notes

- `version` is the image tag, not a separate field — simplest, and maps 1:1 to
  compose's `image:`. If ergonomic version-switching needs a dedicated field
  later, it is additive.
- The field set is deliberately **compose-aligned**: every field maps to a
  `docker-compose.yml` service key (`image`, `ports`, `environment`, `volumes`,
  `healthcheck`, `depends_on`, `restart`, `command`), which keeps the
  dual-target generation honest. Fields that would not translate to compose are
  avoided.

## 2. Reconcile (operator, not agent)

The agent never holds k8s credentials and never calls the API server directly.

- The agent edits `services.yaml` in the workspace and runs
  `sandboxctl services apply`. This POSTs the desired list to the existing
  control API (`127.0.0.1:9099`, already in the agent pod).
- The operator **upserts a `ServiceSet` CR** — one per `SandboxEnvironment`,
  in the agent's namespace, owned by the environment. The CR is the source of
  truth the controller reconciles against, separate from the (largely
  immutable) environment spec so the agent can mutate it at runtime.
- A new controller reconciles `ServiceSet` → `Service` + `Pod` + (if
  `storage:`) `PVC` per entry:
  - **Identity = `name`.** A service `postgres` → `Service/postgres` + a managed
    `Pod` + `PVC/postgres`.
  - **Changed** `image`/`env`/`command`/`resources`/`healthcheck` → patch the
    Service if needed, **recreate the Pod**, retain the PVC (postgres keeps its
    data).
  - **Changed** `ports`/`expose` → patch the Service.
  - **Removed** entry → delete Pod + Service + PVC.
  - **Added** entry → create.
- **Per-entry `Ready` status conditions** (healthcheck passed) on the
  `ServiceSet`. `dependsOn` gates readiness ordering: a dependent is not `Ready`
  until its dependencies are.
- `sandboxctl services compose` renders the equivalent `docker-compose.yml`
  from the same declaration: services become compose `services:` (image, ports,
  environment, volumes, healthcheck, depends_on, restart); runtimes become
  `command: sleep infinity` services the operator `docker compose exec`s into.

### RBAC

The agent's only credential is the control-API socket. The operator owns the
k8s API access in the agent's namespace and applies the NetworkPolicy/ePSP
boundaries there — same boundary the podman sidecar's RBAC already enforced,
now without the sidecar.

## 3. Exec (runtimes)

- `sandboxctl exec <runtime> -- <cmd>` → the operator `exec`s into the
  long-lived runtime pod and streams stdout/stderr/stdin back to the agent.
- The **RWX workspace PVC** is mounted by the agent pod and every runtime pod,
  so a file the agent writes is immediately visible to `python` and vice-versa.
- The operator enforces a **consistent `fsGroup`/`runAsUser`** across all
  workspace-sharing pods (agent + runtimes) so UID mapping is clean — this is
  what rootless podman's `keep-id` used to handle; in k8s it is an explicit,
  operator-set field.
- Switching `python:3.11 → 3.13` = edit `image`, apply → recreate **only** the
  python pod (seconds). The agent pod is a thin, stable dispatcher that is
  never recreated to change a runtime.

## 4. Security & isolation — the payoff

- **No** `/dev/net/tun`, hostPath, cgroup delegation, `containers.conf`, or
  kind-vs-production divergence. The whole #24 problem class is dissolved.
- Isolation is the **existing NetworkPolicy model** (Restricted/Open), not
  pasta. Dep/runtime pods live in the agent's namespace and inherit the
  namespace's NetworkPolicy — identical in kind and production.
  - `Restricted` → default-deny egress still blocks the platform-doubles broker.
  - `Open` → still allows it.
- The control API lives on the **agent pod's** loopback (`127.0.0.1:9099`). Dep
  and runtime pods are **separate pods** with their own network namespaces, so
  they **cannot reach** the agent's loopback (it is not exposed as a Service).
  The `--network host` loopback risk — the known, documented residual risk from
  the podman engine — **dissolves**: there is no `--network host`, and no pod
  shares the agent's loopback by construction. This is a genuine security
  improvement, and the residual-risk row is deleted rather than preserved.
- "The agent holds nothing worth stealing" simplifies to: **the agent holds no
  k8s credentials — only a control-API socket — and deps/runtimes are isolated
  peers governed by the CNI**, not children that inherit everything the sidecar
  sees.

## 5. Workspace & storage

- The workspace PVC is shared by the agent pod and every runtime pod. The
  **access mode adapts to the cluster's storage**:
  - If the workspace storageClass provisions **RWX**, use it — runtimes may
    schedule on any node.
  - If only **RWO** is available (k3s `local-path` is RWO-only out of the box;
    see [local-path-provisioner#70](https://github.com/rancher/local-path-provisioner/issues/70)),
    the operator **pins the agent and all runtimes to one node via node
    affinity** so the single RWO volume is shared. `ReadWriteOnce` means
    read-write by one *node*, so multiple pods on that same node can all mount
    the same volume. This is automatic and free on a single-node cluster —
    which a single-control-plane kind setup and a single-node k3s both are —
    because every pod already lands on the one node.
  - RWX is only a **hard requirement on a multi-node cluster** where runtimes
    must be free to land on different nodes than the agent. There, without a
    RWX-capable storageClass (NFS, a CSI driver with `ReadWriteMany`, or k3s
    `local-path` configured with `sharedFileSystemPath` on a shared filesystem —
    [PR #183](https://github.com/rancher/local-path-provisioner/pull/183)), the
    RWO+affinity pin still works but concentrates all workspace pods on one
    node — a capacity trade-off, not a correctness problem. The operator never
    silently mounts an RWO volume on two nodes.
- Dep data PVCs are per-service, RWO, retained by name across pod recreates,
  and **not snapshotted** — they hold runtime state (a DB's data), not the
  agent's work.
- The snapshot/freeze archives **only the workspace PVC**. There is no in-pod
  layer cache to exclude, so the "layer cache never appears in a snapshot" test
  is moot (deleted). The teardown-before-freeze marker changes from
  `Destroyed.Containers` to `Destroyed.Pods`.

## 6. Test surface (written fresh on the `main` baseline)

On `main`, the only rootless-podman coverage is `lifecycle_test.go` asserting
the *not-implemented* posture (`EngineSecurityRelaxed=Unknown/EngineUnavailable`,
no pod created). The detailed podman e2e specs (`docker run` isolation, the
`--network host` loopback risk, `engine_test.go` AC1, the layer-cache/teardown
specs) lived only on the abandoned `feat/24-rootless-podman-engine` branch and
are not carried over. The k8s-native test surface is written fresh; the
security *properties* are the same ones that approach would have asserted:

- **`lifecycle_test.go` not-implemented spec** — changes from "rootless-podman
  is unimplemented" to "k8s-native is implemented": the engine renders a thin
  agent pod with no extra sidecar, and `EngineSecurityRelaxed` is
  `False/NoRelaxation` (k8s-native needs no security relaxations).
- **Isolation (new spec)** — declare an alpine runtime; `exec` a `wget` against
  the platform-doubles broker; assert egress **blocked under Restricted** and
  **allowed under Open**. Same property the podman approach tested, against a
  native pod.
- **Control-plane isolation (new spec)** — a dep/runtime pod (separate pod, own
  netns) cannot reach the agent's control API on the agent pod's loopback
  `127.0.0.1:9099` (it is not exposed as a Service). This replaces the
  podman-approach's "DEFAULT-network workload container denied the pod loopback"
  property; under k8s-native it is true by pod separation. The `--network host`
  loopback risk the podman approach would have introduced never exists here —
  there is no `--network host` — so it is not a residual risk to assert or
  document; it is avoided by construction.
- **CNI enforcement** — unchanged (namespace-level, not engine-specific).
- **Services (new spec)** — postgres declared as a service; the agent connects
  via `postgres.<ns>.svc` (k8s Service DNS). Version-switch recreates only the
  changed pod (apply `python:3.11`, then `python:3.13`; assert the python pod
  recreated, the postgres pod not).
- **Compose (new spec)** — `sandboxctl services compose` emits a
  `docker-compose.yml` equivalent to the declaration (matching image/ports/env/
  volumes/healthcheck/depends_on).
- **Snapshot/teardown (new spec)** — freeze archives only the workspace PVC
  (no in-pod layer cache to exclude); the teardown marker lists the destroyed
  pods (`Destroyed.Pods`, a new JSON field kept back-compatible with the
  existing `Destroyed.Containers`).
- **Unit/envtest** — the ServiceSet controller reconciles a ServiceSet CR to
  Pods/Services/PVCs with per-entry `Ready` conditions and `dependsOn` gating,
  covered in envtest by creating a ServiceSet and asserting the children.

## 7. Scope cuts (YAGNI, per brainstorming calls)

- **No ephemeral `sandboxctl run` path.** Running arbitrary/untrusted code =
  `exec` into a declared runtime, which is isolated from the control plane by
  pod separation. If ad-hoc one-off pods are needed later, they are additive and
  do not disturb this model.
- **No in-pod container runtime at all.** A task that must **build** container
  images (a CI task producing an image) is out of scope for this engine —
  flagged as a known limitation. There is no rootless-podman fallback on `main`
  (it is an unimplemented stub); building images inside a sandbox would require
  a separate engine and is not part of this design.

## 8. Open sub-decisions (stated, recommendation given; not blocking)

- **`ServiceSet` CR vs folding services into `SandboxEnvironment` spec.**
  Recommend the **separate `ServiceSet` CR**: the agent can mutate it at runtime
  without touching the (largely immutable) environment spec, and ownership is
  clean (the environment owns the ServiceSet, which owns the Pods/Services/PVCs).
- **RWX workspace vs RWO + node-affinity.** Recommend **adapt**: prefer RWX when
  the storageClass supports it (runtimes schedule freely across nodes); fall
  back to RWO + pin agent+runtimes to one node via node affinity otherwise. This
  keeps out-of-box kind and k3s `local-path` (both RWO-only, both typically
  single-node) working with zero storage configuration, and reserves the RWX
  requirement for multi-node clusters that need runtimes spread across nodes.
  No silent corruption: the operator never mounts an RWO volume on two nodes.

## Validation plan

The sandbox cannot run kind locally (nested DinD); all validation is via CI.
Implementation proceeds behind the writing-plans plan; the e2e suite runs on the
kind job. The first green run proves the reconciliation, exec, NetworkPolicy
isolation, compose generation, and version-switch paths end-to-end. Unlike the
podman path, there is no `/dev/net/tun`-style environmental prerequisite to
satisfy on the runner, so the surface that can fail opaquely is much smaller.