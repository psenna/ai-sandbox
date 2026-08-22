# Engines

`spec.engine.type` on a `SandboxClass` selects what runs a sandbox's
workload. There is currently exactly **one implemented engine**, and the
CRD's own default is a **different, unimplemented** one — read this page
before you write a `SandboxClass`.

## The matrix

| `spec.engine.type` | Status today | What runs in the pod | Cluster requirements | Pod Security Standard the sandbox namespace may enforce | securityContext relaxations |
|---|---|---|---|---|---|
| `none` | **Implemented** | The agent container alone, plus the always-present `sandboxctl` native sidecar (and, on a wake, the `restore` init container). No nested container runtime. | Nothing beyond a working kubelet, a StorageClass for the workspace PVC, and (for freeze/wake/archive) a reachable S3-compatible endpoint. | `privileged`, `baseline`, **`restricted`** | **None.** `EngineSecurityRelaxed=False/NoRelaxation`. |
| `rootless-podman` | **Not implemented** — and it is the **CRD default** ([#24](https://github.com/psenna/ai-sandbox/issues/24)) | Nothing. `RenderPod` fails at render time; `ensurePod` logs at `V(1)` and creates no pod. | *(when #24 ships)* AppArmor or no LSM on the node; kernel with rootless overlay support; a namespace with **no** PSS enforcement or `enforce: privileged`. | *(when #24 ships)* `privileged` **only** | *(when #24 ships, on the podman sidecar only)* `AppArmorUnconfined`, `SeccompUnset`, `AllowPrivilegeEscalation`. The **agent** container keeps `runAsNonRoot`, `allowPrivilegeEscalation:false`, `drop:[ALL]`, `seccompProfile:RuntimeDefault` regardless. |

**If you write a `SandboxClass` and omit `spec.engine.type` entirely, you
get `rootless-podman` — the not-implemented one.** `engine.type: none` must
be set explicitly. See "What happens if you select it" below for the exact
symptom.

## `none` — what you get

Three containers, always in this shape: the `sandboxctl` native sidecar
(init container, KEP-753, always present, holds the environment's
Kubernetes credential and exposes the loopback control API), an optional
`restore` init container (only on a wake, S3-backed classes only, runs
last, non-restartable), and the `agent` container (the only regular
container — the workload itself). No engine-launched nested container
runtime exists in this pod: if your agent needs to `docker run` or `podman
run` something itself, `none` cannot do it today (that is exactly what
`rootless-podman` is for, once implemented).

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

## `rootless-podman` — not implemented

### What happens if you select it

Nothing runs. `RenderPod` returns an error for an unimplemented engine;
`ensurePod` (`internal/controller/pod.go`) logs that error at `V(1)` and
returns `nil` rather than surfacing it as a reconcile error. No pod is ever
created, no phase transition happens beyond `Restoring`, and **no Event is
emitted** for this specific failure. The environment sits in
`Restoring`/`PodReady=False/PodNotCreated` with
`EngineSecurityRelaxed=Unknown/EngineUnavailable` (message: `engine
"rootless-podman" is not implemented yet; its security posture is not yet
known`) until `spec.timeouts.total` (default `72h`) fires
`TotalTimeoutExceeded` and the environment moves to `Failed`.

This is, by a wide margin, the most likely thing to trip a new user of this
operator: it is the CRD's own default, and the only symptom for the first
72 hours is silence.

**Fix:** set `spec.engine.type: none` explicitly on every `SandboxClass`
you write.

### What it will require (#24)

From the spike (below): AppArmor or no LSM on the node (verified on one
cluster only — see "What the spike did not cover"), a kernel with rootless
overlay support, and a namespace with **no** PSS enforcement or `enforce:
privileged` — `baseline`/`restricted` reject it (see "The PSS constraint").
On the security side, only the `rootless-podman` **sidecar** would carry
relaxed `securityContext` fields — `AppArmorUnconfined`, `SeccompUnset`,
`AllowPrivilegeEscalation`. The `agent` container's own hardening
(`runAsNonRoot`, `allowPrivilegeEscalation: false`, `capabilities.drop:
[ALL]`, `seccompProfile: RuntimeDefault`) is unaffected — the blast radius
of the relaxation is scoped to the sidecar that runs the nested runtime,
not the agent.

### The PSS constraint

The spike (`operator/docs/spike-rootless-podman.md`) ran a rootless podman
pod against a real API server enforcing each PSS level and recorded the
verbatim rejections: `baseline` rejects **only**

```
forbidden AppArmor profile (container must not set AppArmor profile type to "Unconfined")
```

`restricted` rejects that plus `allowPrivilegeEscalation`, `capabilities`,
`runAsNonRoot`, and `seccompProfile`. In short: `rootless-podman`, once
implemented, will only be usable in a namespace with **no** PSS enforcement
or `enforce: privileged` — `baseline` and `restricted` are both closed to
it.

### What the spike did not cover

Read this honestly, not as a footnote: the spike ran on **one cluster
only** (k3s on Ubuntu, AppArmor as the LSM). It explicitly did **not**
cover SELinux-based distros (where the sole `baseline` violation above may
not even apply the same way), any managed cluster (EKS/GKE/AKS), how npm/
pip/go installs inside the nested runtime would route through
DependaProxy, or NetworkPolicy inheritance for containers launched inside
the pod (see [`security.md`](security.md)'s residual-risks section on this
same gap). Treat every claim in this section as scoped to that one
environment until #24 re-verifies it more broadly.

## Inert fields today

`spec.engine.image` and `spec.engine.storageDriver` are read **nowhere** in
the current codebase — they are placeholders reserved for #24 and have no
effect on anything today. Setting them is harmless but does nothing.

## The three ways the operator tells you you picked the wrong engine

1. **`EngineSecurityRelaxed=Unknown/EngineUnavailable`** — the only signal
   from a running operator. There is no Event and no phase change beyond
   the eventual timeout.
2. **The Helm chart's render-time guard (G6)** — `helm template … --set
   defaultClass.engine.type=rootless-podman --set
   sandboxNamespaces.podSecurityEnforce=restricted` fails at template time
   with `cannot run in namespaces enforcing Pod Security Standard
   "restricted"`. `.github/workflows/helm.yml` asserts this.
3. **The e2e spec** `fails closed, visibly, for an engine that is not
   implemented yet` (`test/e2e/lifecycle_test.go`).

## How to choose

- If you only need the agent to run code in its own pod -> `engine.type:
  none`.
- If you need the agent to launch containers of its own (docker/podman-in-
  pod) -> `rootless-podman`, which **is not available yet**.
- If your cluster enforces `baseline`/`restricted` on sandbox namespaces ->
  `none` is your **only** option today, and will remain so until #25 (a
  Kubernetes-native workload broker) ships — which is **post-v1 and not
  started**. Do not document or plan around #25 as an available engine.

## Related

- [`security.md`](security.md) — the closed relaxation allowlist and what
  each relaxation kind can and cannot express.
- [`operations.md`](operations.md#stuck-in-restoring) — the exact
  troubleshooting steps for "stuck in Restoring".
- `operator/docs/spike-rootless-podman.md` — the full spike write-up this
  page summarizes.
