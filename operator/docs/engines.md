# Engines

`spec.engine.type` on a `SandboxClass` selects what runs a sandbox's
workload. There are **two implemented engines**. Read this page before you
write a `SandboxClass`.

## The matrix

| `spec.engine.type` | Status today | What runs in the pod | Cluster requirements | Pod Security Standard the sandbox namespace may enforce | securityContext relaxations |
|---|---|---|---|---|---|
| `none` | **Implemented** | The agent container alone, plus the always-present `sandboxctl` native sidecar (and, on a wake, the `restore` init container). No nested container runtime. | Nothing beyond a working kubelet, a StorageClass for the workspace PVC, and (for freeze/wake/archive) a reachable S3-compatible endpoint. | `privileged`, `baseline`, **`restricted`** | **None.** `EngineSecurityRelaxed=False/NoRelaxation`. |
| `rootless-podman` | **Implemented** ([#24](https://github.com/psenna/ai-sandbox/issues/24)) — and it is the **CRD default** | A `podman system service` native sidecar plus the agent container (and, on a wake, `restore`). The agent drives the sidecar's Docker-compatible API over `DOCKER_HOST=tcp://127.0.0.1:2375` to launch its own containers. | AppArmor or no LSM on the node; a kernel with rootless-overlay support; a namespace with **no** PSS enforcement or `enforce: privileged`; under `Restricted` isolation, a reachable registry route (`services.registryMirror` or a covering `extraEgress` CIDR). | `privileged` **only** | On the `podman` sidecar only: `AppArmorUnconfined`, `SeccompUnconfined`, `AllowPrivilegeEscalation`. The **agent** container keeps `runAsNonRoot`, `allowPrivilegeEscalation:false`, `drop:[ALL]`, `seccompProfile:RuntimeDefault` regardless. |

**If you write a `SandboxClass` and omit `spec.engine.type` entirely, you
get `rootless-podman`.** `engine.type: none` must be set explicitly if you
want the no-nested-runtime engine.

## `none` — what you get

Three containers, always in this shape: the `sandboxctl` native sidecar
(init container, KEP-753, always present, holds the environment's
Kubernetes credential and exposes the loopback control API), an optional
`restore` init container (only on a wake, S3-backed classes only, runs
last, non-restartable), and the `agent` container (the only regular
container — the workload itself). No engine-launched nested container
runtime exists in this pod: if your agent needs to `docker run` or `podman
run` something itself, `none` cannot do it — that is exactly what
`rootless-podman` is for.

## `none` under PSS `restricted`

The pod the `none` engine renders satisfies Kubernetes' `restricted` Pod
Security Standard, field by field:

- Pod-level: `runAsNonRoot: true`, `runAsUser`/`runAsGroup`/`fsGroup: 1000`,
  `seccompProfile: RuntimeDefault`.
- Every container: `allowPrivilegeEscalation: false`,
  `capabilities.drop: [ALL]`, `seccompProfile: RuntimeDefault`,
  `runAsNonRoot: true`.
- Volume types used are only `persistentVolumeClaim`, `emptyDir`,
  `configMap`, `secret`, `projected` — all on `restricted`'s allowlist. No
  `hostPath`, no `hostNetwork`/`hostPID`/`hostIPC`.

One deliberate exception: the agent container sets
`readOnlyRootFilesystem: false` (its image's entrypoint writes `~/.npmrc`
and `~/.gitconfig`). This is **not** a PSS requirement at any level — PSS
does not check `readOnlyRootFilesystem` — so it does not affect
`restricted` compliance.

The chart ships a fixture proving this combination renders cleanly under
enforcement:
`deploy/helm/ai-sandbox-operator/ci/engine-none-restricted-psa-values.yaml`.

## `rootless-podman` — what you get

### Topology

Four container "slots", in the order the pod actually renders them:

1. **`podman`** (init container, native sidecar, `restartPolicy: Always`) —
   runs `podman system service --time=0 tcp://127.0.0.1:2375`, bound to pod
   loopback only. Rendered **first** among init containers, before
   `sandboxctl`.
2. **`sandboxctl`** (init container, native sidecar, `restartPolicy:
   Always`) — the always-present control-channel sidecar, unchanged by this
   engine.
3. **`restore`** (plain init container, S3-backed classes only, present only
   on a wake) — runs last among init containers.
4. **`agent`** (the sole regular container) — gets `DOCKER_HOST` and
   `CONTAINER_HOST`, both `tcp://127.0.0.1:2375`, appended to its normal
   environment. Its own `securityContext` is completely unchanged by this
   engine (see "The PSS constraint" below).

The ordering of (1) and (2) is deliberate and load-bearing, not incidental.
Native sidecars **start** in declaration order and **terminate** in
*reverse* declaration order, so `podman` before `sandboxctl` gives:

```
start:     podman -> sandboxctl -> [restore] -> agent
terminate: agent  -> sandboxctl -> podman
```

That terminate order is what lets `sandboxctl`'s SIGTERM-path freeze reach
the `podman` API to stop workload containers *before* `/workspace` gets
archived (see "Teardown on freeze" below). The reverse order would let
`podman` die first and leave the freeze tarring a workspace with live
writers still in it.

Two mounts on the `podman` sidecar, and exactly two:

- The **workspace volume**, at the same path the agent sees it
  (`/workspace`) — so `docker run -v /workspace:/work` resolves to the same
  files the agent itself is editing.
- The **layer-cache `emptyDir`**, at `/home/podman/.local/share/containers`
  — see "Storage driver" below.

No ServiceAccount token, no Secret, no ConfigMap, no `hostPath` is ever
mounted into `podman`. This is the engine's actual security argument (see
[`security.md`](security.md#the-sidecar-holds-nothing-worth-stealing)): the
relaxed sidecar holds nothing worth stealing even though its
`securityContext` is weaker than the agent's.

### Storage driver

`spec.engine.storageDriver: auto` (the default) resolves to `overlay`
**deterministically, at render time** — there is no node probe and no `vfs`
fallback. Spike [#23](https://github.com/psenna/ai-sandbox/issues/23)
measured native rootless `overlay` working on kernel 6.8 with no
`/dev/fuse` and no `fuse-overlayfs`, and measured `postgres:18` running
correctly as a service container, so the failure modes a fallback would
insure against did not occur in practice — shipping untested fallback code
for a hypothetical is strictly worse than failing loudly. `vfs` remains
selectable *explicitly* (still in the CRD enum) for an operator who knows
their kernel lacks rootless overlay; it is never selected automatically,
and it is very slow.

The rendered `storage.conf` sets `driver`, `runroot`, and `graphroot` and
nothing else under `[storage]` — in particular, it does **not** set a
`[storage.options.overlay]` table at all, and does not set `mount_program`.
The pinned image's own shipped default sets exactly one entry under that
table (`mount_program = "/usr/bin/fuse-overlayfs"`), which this engine
deliberately drops: the pod has no `/dev/fuse`, so `fuse-overlayfs` is
unavailable, and with that one entry gone the table has nothing left to
say.

### Registries and the pull-through cache

`registries.conf` always sets `unqualified-search-registries =
["docker.io"]` and `short-name-mode = "permissive"` (the image's own default,
`enforcing`, prompts interactively and hangs the service). If the class
declares `spec.services.registryMirror`, one `[[registry]]`/
`[[registry.mirror]]` pair is added per upstream prefix (`registries`, or
`["docker.io"]` if empty), sorted for determinism. The mirror's `location`
is `host[:port][/path]` parsed from the URL; `insecure = true` is set
**iff** the URL scheme is `http://` — it is derived from the scheme, never
an independently settable field, so a class cannot declare `https://` and
`insecure: true` at the same time.

Under `network.isolation: Restricted`, this is not optional in practice: a
Restricted `NetworkPolicy` allows egress only to the peers the class
declares, so with no mirror configured the `podman` sidecar can reach no
registry at all — every `docker pull` inside the sandbox times out. Helm
guard **G22** catches a `rootless-podman` + `Restricted` class with no
mirror and no covering `extraEgress` CIDR at chart-render time.

DependaProxy is **not** referenced by `registries.conf` — DependaProxy
proxies npm/PyPI/Go modules and has no OCI/distribution endpoint at all, so
it cannot serve container images. DependaProxy's role in this engine is
propagating `NPM_CONFIG_REGISTRY`/`PIP_INDEX_URL`/`GOPROXY` into the
**agent's** environment (already true before this engine existed), which a
workload container inherits the same way any other env var is passed
through — see the `use-docker` skill.

### The image is pinned by digest

The default sidecar image is `quay.io/podman/stable`, pinned by its OCI
**image-index** digest (not a per-arch manifest digest, so both amd64 and
arm64 nodes resolve it) — `internal/render/engine_podman.go`'s
`DefaultPodmanImage`. `spec.engine.image` is empty by default and resolves
to that constant; an **override must also be digest-pinned**
(`repository@sha256:...`), or `RenderPod` fails at render time with an
actionable error. Helm guard **G21** enforces the identical rule at chart
level.

This is not pedantry: the `securityContext` this engine requires was
established empirically against one specific podman version (5.8.2, see the
spike below), and an unpinned tag could silently drift the image's behavior
out from under that validated configuration with no way to notice. Bumping
`DefaultPodmanImage` requires re-running the privilege ladder — see
[`spike-rootless-podman.md`](spike-rootless-podman.md#re-running-the-ladder-on-a-podman-bump).

### The PSS constraint

The spike (`operator/docs/spike-rootless-podman.md`) ran a rootless podman
pod against a real API server enforcing each PSS level and recorded the
verbatim rejections. On the pod the *spike's ladder* rendered, `baseline`
rejected on exactly one ground:

```
forbidden AppArmor profile (container must not set AppArmor profile type to "Unconfined")
```

**The pod this operator actually renders has a second `baseline` violation
the spike's bare ladder pods did not carry.** `RenderPod`'s pod-level
`securityContext.seccompProfile` is `RuntimeDefault`; every container that
leaves its own `seccompProfile` unset would inherit that pod-level value,
which is exactly the spike's failing ladder case H (`RuntimeDefault` blocks
`clone(CLONE_NEWUSER)`, so podman cannot start). The `podman` sidecar
therefore sets `seccompProfile.type: Unconfined` **explicitly**, which
`baseline` also forbids. So on the rendered pod, `baseline` rejects on
**two** independent grounds — AppArmor `Unconfined` and seccomp
`Unconfined` — not one. `restricted` rejects those same two (it requires
`seccompProfile` to be `RuntimeDefault` or `Localhost`, so `Unconfined`
fails there too) plus two more: `allowPrivilegeEscalation: true`, and the
absence of `capabilities.drop: [ALL]` — the sidecar deliberately sets no
`capabilities` field at all, and `restricted` requires the explicit drop.
Note that `runAsNonRoot`/`runAsUser` are *not* among the grounds: the
sidecar sets `runAsNonRoot: true` and `runAsUser: 1000`, which is exactly
what `restricted` asks for.

The practical conclusion is unchanged from the spike's: `rootless-podman`
is only usable in a namespace with **no** PSS enforcement or `enforce:
privileged` — `baseline` and `restricted` are both closed to it. Only the
reason list is longer.

**The render-time guard.** `internal/render.CheckNamespacePodSecurity`
rejects the render outright when the target namespace's
`pod-security.kubernetes.io/enforce` label is `baseline`/`restricted` and
the engine's relaxations conflict with it, returning an actionable error
naming the namespace, the enforced level, and the exact relaxation kinds
rejected.

**The runtime condition.** `ensurePod` swallows that render error at `V(1)`
(consistent with how every other render error is handled), so the
reconciler also surfaces it as its own signal:
`EngineSecurityRelaxed=Unknown/NamespacePodSecurityIncompatible`, with the
identical message text. See
[`operations.md`](operations.md#stuck-in-restoring) for the full
troubleshooting path.

**The Warning Event.** Because the condition alone is easy to miss on a
first pass, the reconciler also emits `Warning EngineNamespaceIncompatible`
every reconcile the class remains incompatible with its namespace — visible
in `kubectl describe`/`kubectl get events` without reading a condition at
all.

**Known gap, stated rather than hidden.** Pod Security Admission can also be
configured **cluster-wide**, via the API server's `AdmissionConfiguration`
`defaults`, with **no** namespace label at all. Neither the render-time
guard nor the runtime condition can see that configuration — a controller
has no API to read it — so in that setup the API server still rejects the
pod, with the opaque message this guard exists to pre-empt. If your cluster
uses cluster-wide PSA defaults rather than namespace labels, treat this
guard as advisory, not authoritative.

### Teardown on freeze

`internal/sandboxctl/engine_podman.go`'s `EngineTeardown` implementation
stops and removes every workload container the `podman` sidecar is running,
over its Docker-compatible REST API on pod loopback, as part of
`sandboxctl`'s SIGTERM-path freeze — *before* `/workspace` is archived. This
is why the `podman`-before-`sandboxctl` termination order above matters: if
`podman` died first, there would be no API left to call. If the engine API
is not listening at all (it never started, or the pod is on `engine.type:
none`), teardown is a documented no-op, not an error — there is nothing to
tear down.

### The layer cache never appears in a snapshot

The `podman` sidecar's graph root
(`/home/podman/.local/share/containers`) lives on its own `emptyDir`,
mounted **only** into the `podman` container — no other container in the
pod mounts it. `internal/sandboxctl/snapshot.go` archives exactly two roots
(`Cfg.WorkspacePath` and `Cfg.AgentHomePath`), neither of which the graph
root is under or inside — so the exclusion is structural, not a filter that
could regress. `internal/sandboxctl/exclusions.go`'s `cacheExcludePaths`
adds a second, belt-and-braces guard on top (`.local/share/containers`,
`.config/containers`, `.cache/containers`, `var/lib/containers`, …), tested
by `exclusions_test.go`.

### What the agent sees

The agent container's `securityContext` is byte-for-byte unchanged by this
engine — no relaxation whatsoever. What it gains is `DOCKER_HOST` and
`CONTAINER_HOST` in its process environment, both `tcp://127.0.0.1:2375`,
and the ability to run a real Docker-compatible CLI (`docker`, or the
`docker` Python/Node SDKs) against them. For the actual command patterns —
canonical `docker run` invocations, the `/workspace`-only sharing rule,
registries-through-DependaProxy for build-time dependency fetches vs.
registries-through-the-mirror for base images, file ownership, standing up
a service container like postgres — see the `use-docker` skill
(`claude-code/use-docker/SKILL.md`), which is what actually ships into the
agent image this class's pod runs.

### What the spike did not cover — now verified

The spike (below) explicitly listed two gaps under "Not covered". Both are
now verified by e2e specs added alongside this engine:

- **`npm ci` through DependaProxy from inside a nested workload container**
  — the spike's cluster had no route to the sandbox's compose network, so
  this was structurally unvalidatable there. Verified end to end by
  `test/e2e/engine_test.go`'s `installs an npm package through dependaproxy
  from inside a workload container`.
- **NetworkPolicy inheritance for containers launched inside the pod** — the
  spike's own "Not covered" list named this outright. Verified end to end by
  `test/e2e/isolation_test.go`'s `restricts a workload container's egress
  under Restricted isolation and allows it under Open`, plus two further
  specs specifically for `--network host`: `governs a --network host
  workload container by the same NetworkPolicy` (the policy still applies —
  `--network host` inside a rootless podman pod is the pod's own network
  namespace, not a bypass) and `gives a --network host workload container
  pod loopback -- a known, documented risk` (the real, structurally
  unpreventable consequence — see
  [`security.md`](security.md#residual-risks-read-this-section)).

What remains genuinely unverified, honestly: SELinux-based distros (no
AppArmor profile to relax there, which is a concrete, testable hypothesis
that the sole `baseline` violation might not apply the same way), any
managed cluster (EKS/GKE/AKS), and any cluster running with cluster-wide PSA
`defaults` rather than namespace labels (the known gap above). Read the
spike write-up honestly, not as a footnote, for the exact cluster it was
measured against.

## The three ways the operator tells you your engine choice does not fit

1. **`EngineSecurityRelaxed=Unknown/NamespacePodSecurityIncompatible`** —
   the condition, with a message naming the namespace, the enforced PSS
   level, and the exact relaxation kinds it rejects.
2. **`Warning EngineNamespaceIncompatible`** — an Event, emitted every
   reconcile the incompatibility persists, so it shows up in `kubectl get
   events` without reading a condition.
3. **The Helm chart's render-time guards (G6, G21, G22)** — `helm template`
   fails at template time rather than letting `helm install` succeed and the
   class silently never produce a pod. `.github/workflows/helm.yml` asserts
   all three.

## How to choose

- If you only need the agent to run code in its own pod -> `engine.type:
  none`.
- If you need the agent to launch containers of its own (docker/podman-in-
  pod: build images, run service dependencies like postgres, bind-mount
  `/workspace` into a workload) -> `engine.type: rootless-podman` (the CRD
  default), in a namespace with no PSS enforcement or `enforce: privileged`.
- If your cluster enforces `baseline`/`restricted` on sandbox namespaces ->
  `none` is your **only** option today, and will remain so until #25 (a
  Kubernetes-native workload broker) ships — which is **post-v1 and not
  started**. Do not document or plan around #25 as an available engine.

## Related

- [`security.md`](security.md) — the closed relaxation allowlist and what
  each relaxation kind can and cannot express.
- [`operations.md`](operations.md#stuck-in-restoring) — the exact
  troubleshooting steps for "stuck in Restoring", plus dedicated
  `rootless-podman` troubleshooting.
- `operator/docs/spike-rootless-podman.md` — the full spike write-up this
  page summarizes.
- `claude-code/use-docker/SKILL.md` — the agent-facing contract for driving
  this engine's Docker-compatible API.
