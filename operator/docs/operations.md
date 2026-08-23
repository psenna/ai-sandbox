# Operations

This is the day-two guide: installing, sizing, storage, retention, metrics,
events, logs, and troubleshooting by condition. For "what does this field
mean", see the [CRD reference](crd-reference.md). For engine choice and its
security consequences, see [`engines.md`](engines.md) and
[`security.md`](security.md).

## Installing

### Prerequisites

- A Kubernetes cluster **>= 1.25**.
- An **enforcing CNI** (Calico, Cilium, Antrea; kindnet also enforces) — only
  required if any `SandboxClass` uses `network.isolation: Restricted` (the
  CRD default). Without one, `Restricted` is declared but not actually
  enforced; see [`security.md`](security.md#cni-enforcement-is-measured-not-assumed).
- The **Pod Security Standard** level enforced on namespaces that will hold
  `SandboxEnvironment` pods matters: `engine.type: none` satisfies
  `restricted`; `engine.type: rootless-podman` (the CRD default) needs a
  namespace with **no** PSS enforcement or `enforce: privileged` — `baseline`
  and `restricted` both reject its sidecar relaxations. See
  [`engines.md`](engines.md) for what each engine needs.
- **Prometheus Operator CRDs** are only needed if you enable the chart's
  `metrics.serviceMonitor.enabled` (a `ServiceMonitor`); the operator's own
  `/metrics` endpoint works without them.

### Helm (the supported path)

Install via the chart at
[`deploy/helm/ai-sandbox-operator`](../deploy/helm/ai-sandbox-operator/README.md)
(#34) — it is the supported install path: a full values reference, an RBAC
table, guarded misconfiguration errors (rendered as `helm template`/`helm
install` failures, not runtime surprises), a `crds/` directory, and a `helm
test` suite. This document does not duplicate that README; read it for the
values reference.

**No operator image or OCI chart has been published yet** (see
`.github/workflows/release.yml`'s own header comment). Until a release is
cut, install from a local build — see the [quickstart](../README.md#quickstart)
for the exact `docker build` + `kind load` + `helm install` sequence. Once a
release exists, `helm install … oci://ghcr.io/psenna/charts/ai-sandbox-operator`
replaces the build-and-load steps.

### Kustomize (`config/default`)

`config/default` (kustomize) remains for local/e2e development
(`make kustomize-build`, `hack/e2e-up.sh`) but is **not** a packaged,
versioned install artifact the way the chart is. It has no `imageRef`/
`sidecarImageRef` helper — its `images:` transform and
`test/e2e/manifests/operator/kustomization.yaml`'s explicit
`--sidecar-image` argument must be kept in lockstep by hand. Prefer the chart
outside of this repo's own e2e/dev usage.

### The CRDs

Helm 3 installs everything under a chart's `crds/` directory on `helm
install`, and **never upgrades or deletes it** on `helm upgrade`/`helm
uninstall` (this is Helm's own documented behavior, not an operator choice).
To pick up a CRD change, apply the updated CRD yourself before upgrading:

```sh
kubectl apply --server-side -f deploy/helm/ai-sandbox-operator/crds/
helm upgrade ai-sandbox-operator deploy/helm/ai-sandbox-operator -n ai-sandbox-operator-system
```

### Upgrading / uninstalling

What survives a `helm uninstall`: the CRDs, all `SandboxClass`/
`SandboxEnvironment` custom resources, any still-running environment's pod
and workspace PVC, and a default `SandboxClass` marked
`helm.sh/resource-policy: keep`. What does not survive: the operator
Deployment, its metrics Service, and its RBAC (`ClusterRole`/
`ClusterRoleBinding`, the leader-election `Role`/`RoleBinding`). A `helm
upgrade` that only rolls the operator Deployment (new image, new flags) has
no effect on an already-`Running` environment's pod — the operator does not
own that pod via the Deployment.

`operator/hack/helm-kind-walkthrough.sh` asserts every one of these claims
against a real cluster (install → `helm test` → upgrade-does-not-disrupt →
uninstall-preserves-workloads → reinstall-resumes); read it for the exact
assertions, or run it via `make helm-kind` (only fully supported
`IN_CONTAINER=1`; see [`development.md`](development.md)).

## Sizing `slots.capacity`

A **slot** is held by exactly one environment in phase `Running`,
`Restoring`, or `Freezing` — the three phases where a pod is expected to
exist and consume real node resources. `Pending`, `Ready`, `Waiting`, `Done`
and `Failed` hold no slot.

The scheduler (`internal/controller/slotscheduler.go`, a leader-elected
`manager.Runnable`) runs every `--scheduler-interval` (default `5s`, valid
`100ms`..`5m`) and admits queued environments by `spec.priority` (`-1000`..
`1000`, higher first) then queue order (oldest first within the same
priority) until `slots.capacity` is reached. There is **no per-namespace
capacity** — one global pool, cluster-wide (or `--watch-namespace`-wide).

Size against three gauges:

- `sandbox_operator_slots_used` vs `sandbox_operator_slot_capacity` — if
  `slots_used` is pinned at `slot_capacity` for long stretches, you are
  capacity-bound.
- `sandbox_operator_queue_depth` — sustained non-zero means demand exceeds
  capacity.
- `sandbox_operator_queue_wait_seconds` (Histogram) — how long environments
  actually wait for a slot.

Per-slot cost, roughly: the agent container's own `resources.requests` (set
per `SandboxClass`, no operator default) + the `sandboxctl` sidecar's fixed
`50m`/`64Mi` request and `500m`/`256Mi` limit + the workspace PVC's
provisioned size (`spec.storage.workspace.size`, a capacity cost even while
the environment holds no slot, since the PVC persists across freeze/wake).

**"Queued" vs "stuck"**: `Scheduled=False/Queued` with a message like
`queued at position N of M` is normal backpressure — raise `slots.capacity`
or the environment's `spec.priority`. An environment that holds a slot (no
longer `Queued`) but never advances past `Restoring` is a **different**
problem — see [Troubleshooting](#stuck-in-restoring) below, most likely a
`rootless-podman` class in a namespace whose Pod Security Standard rejects
its relaxations.

## Configuring storage

### `s3` vs `pvc` — the honest table

| capability | `s3` | `pvc` |
|---|---|---|
| workspace PVC | yes | yes |
| freeze snapshot | yes | **NO** |
| wake/restore | yes | **NO** |
| terminal archive | yes | **NO** (finalizer removed with nothing written) |
| retention GC | yes | N/A (nothing to retain) |

A `pvc`-backed class is not a lesser version of `s3` — it is missing freeze,
wake, and archive entirely. Environments on a `pvc` class still run, still
report status, and still get a workspace PVC; they simply cannot pause and
resume, and leave no archived transcript.

### The S3 credentials trap

`storage.backend.s3.credentialsSecretRef` is resolved from the **operator's
own namespace** (`--class-secret-namespace` /
`SANDBOX_OPERATOR_CLASS_SECRET_NAMESPACE`, defaulting to the release
namespace, e.g. `ai-sandbox-operator-system`) — **not** the environment's
namespace. The Secret's data keys are **fixed**: `accessKeyID` and
`secretAccessKey` (plus optional `sessionToken`).
`credentialsSecretRef.key` is **ignored** for the `s3` backend (it only
applies to other credential kinds elsewhere in the API). Getting either of
these wrong — wrong namespace, or a Secret with different key names — parks
**every** environment on that class at `Scheduled=False/ResourcesNotReady`,
with a message naming the credential-resolution failure. This is the single
most common install-time mistake; see the
[quickstart](../README.md#quickstart)'s own S3 step for a worked example.

### Where the bytes live

Every object lives under:

```
<prefix>/<clusterID>/<namespace>/<envName>/<envUID>/
  snapshots/<seq>-<timestamp>/{workspace.tar.zst,agent-home.tar.zst,manifest.json}
  archive/{run.json,context.tar.zst}
  latest.json
```

`envUID` is mandatory in the layout: it is what keeps a deleted-and-recreated
environment (same cluster/namespace/name) from colliding with its
predecessor's snapshots — retention GC's orphan sweep (below) is what
reclaims the old UID's root.

### The workspace PVC and warm cache

Sized from `spec.storage.workspace.size` on the `SandboxClass` and rendered
with `spec.storage.workspace.storageClassName` if set. It is retained across
freeze/wake (that retention is what "warm cache" means) until either the
warm-cache TTL GC reclaims it (below) or the environment is deleted. If the
StorageClass has `volumeBindingMode: WaitForFirstConsumer`, the PVC pins a
woken environment's pod to whichever node the PV was originally provisioned
on — reclaiming the PVC (TTL expiry, or deletion) is what un-pins it.

## Retention and cost

### `warmCacheTTL`

`internal/controller/warmcachegc.go` (a leader-elected `manager.Runnable`,
`--warm-cache-gc-interval`, default `30m`) deletes a frozen environment's
workspace PVC only when **all five** of the following hold — every condition
is independently required, so a broken freeze never loses data:

1. The environment is in phase `Waiting` (exactly).
2. It is not being deleted.
3. Its snapshot is **complete and verified** in S3 (`status.snapshot`
   present with `Seq >= FreezeCount-1`, no in-flight/failed
   `snapshotAttempt`).
4. Its class is **S3-backed**.
5. It is past `spec.storage.warmCacheTTL` (default `30m`; `"0s"` disables GC
   for that class).

It will **never** delete the PVC of a `Running` environment, or one whose
freeze failed — before the snapshot exists, the PVC is the only copy of the
agent's context.

### `--retention-ttl`

`internal/controller/retentiongc.go` (also leader-elected, one loop
cluster-wide, `--retention-gc-interval` default `30m`) runs two independent
sweeps per S3-backed class:

- **Retention**: deletes a live environment's entire storage root
  (snapshots *and* archive) once `status.archive.finishedAt` is older than
  `--retention-ttl` (default `168h`; `0` disables this sweep only).
- **Orphan cleanup**: deletes any storage root whose `envUID` belongs to no
  currently-live environment, regardless of TTL — this is the mechanism
  that makes deleting an environment and recreating one with the *same
  name* safe.

Always run `--retention-dry-run` **first** against a new cluster or a
lowered `--retention-ttl` — it logs what either sweep would delete without
deleting anything.

### Orphan cleanup and `--watch-namespace`

Under `--watch-namespace`, orphan cleanup only considers storage roots whose
namespace segment is inside that watch — "currently-live" is only knowable
as far as the operator's own watch reaches. Running the operator
`--watch-namespace`-scoped against a bucket shared with environments in
other namespaces will over-report orphans.

### A cost model

Two cost axes: **PVC-hours** (workspace PVCs, sized per class, retained for
`warmCacheTTL` past each freeze) and **S3 object bytes** (snapshots +
archives, retained for `--retention-ttl`). Ranked knobs, most to least
impactful for a busy cluster:

1. `--retention-ttl` — bounds S3 bytes; the biggest lever for storage cost.
2. `spec.storage.warmCacheTTL` per class — bounds PVC-hours; lower it for
   classes whose agents rarely resume quickly.
3. `spec.timeouts.total` — bounds how long a stuck environment (e.g. a
   `rootless-podman` class parked on a PSS-incompatible namespace) holds a
   slot and a PVC before being force-failed.
4. `spec.storage.workspace.size` — the per-environment PVC floor; oversizing
   multiplies the PVC-hours cost directly.

## Reading the metrics

### The endpoint

`--metrics-bind-address` (default `:8080`), **unauthenticated**, on
controller-runtime's own `Registry` — no separate server, no new auth. The
chart exposes it as a stable `ClusterIP` Service; enable
`metrics.serviceMonitor.enabled` for a `ServiceMonitor` (needs the
Prometheus Operator CRDs).

### The catalogue

Every series is `sandbox_operator_<name>`. No metric ever carries an
environment name, namespace, or UID as a label; unexpected label values
collapse to `"other"` (see Cardinality guarantee below).

| name | type | labels | what it means |
|---|---|---|---|
| `sandbox_operator_environments` | Gauge | `phase` | environments currently in each lifecycle phase, in the watch scope — every phase is set on every pass (including 0) |
| `sandbox_operator_slot_capacity` | Gauge | — | configured `--slot-capacity` |
| `sandbox_operator_slots_used` | Gauge | — | occupied scheduling slots |
| `sandbox_operator_queue_depth` | Gauge | — | environments queued for a slot |
| `sandbox_operator_queue_wait_seconds` | Histogram | — | time queued before a slot was granted |
| `sandbox_operator_freeze_duration_seconds` | Histogram | — | freeze-hook-to-published-snapshot duration |
| `sandbox_operator_wake_duration_seconds` | Histogram | `source` (`warm`/`cold`/`unknown`) | wake-restore duration, by warm-cache hit or cold download |
| `sandbox_operator_snapshot_size_bytes` | Histogram | — | published freeze snapshot size, uncompressed |
| `sandbox_operator_probe_evaluations_total` | Counter | `type`, `result` | wait-probe evaluations, by wait type and outcome (`satisfied`/`pending`/`error`/`skipped`) |
| `sandbox_operator_archives_total` | Counter | `result` (`succeeded`/`failed`/`skipped`) | terminal archive outcomes |
| `sandbox_operator_reconcile_errors_total` | Counter | `controller` | errors from a reconcile or control-loop pass |
| `sandbox_operator_warm_cache_reclaimed_total` | Counter | — | workspace PVCs reclaimed by the warm-cache TTL GC |
| `sandbox_operator_retention_deleted_total` | Counter | `sweep` (`retention`/`orphan`) | storage roots deleted by retention GC |

### Cardinality guarantee

No environment name, namespace, or UID is ever a label value. Every label
value that could conceivably carry unexpected data passes through a closed
allowlist (`sanitize`, mirroring `lifecycle.SanitizeReason`) that collapses
an unrecognized value to `"other"` rather than creating an unbounded new
series.

### `archives_total`'s honest limitation

The primary reconciler does not watch archive Jobs directly, so `failed`
increments **once per reconcile** an archive stays broken, not once per
distinct failure. **Alert on the rate**, not on any single increment.

### The gauge/counter split

`MetricsCollector` (`internal/controller/metricscollector.go`) recomputes
the four gauges above periodically (`--metrics-collect-interval`, default
`15s`, valid `1s`–`5m`) from one cached List — the **only**
`manager.Runnable` in this codebase whose `NeedLeaderElection()` returns
`false`, deliberately: every replica must keep its own `/metrics`
endpoint's gauges live, not just the leader's. Every other metric is a
point-in-time observation recorded exactly where the thing happens (the
scheduler's grant, the status-write transition hook, the probe evaluator,
the archive controller, each GC loop's pass completion).

### Four alerts worth having

- `sandbox_operator_queue_depth` sustained above 0 for a long window —
  capacity-bound.
- `rate(sandbox_operator_reconcile_errors_total[5m]) > 0` — something is
  erroring on every pass.
- `rate(sandbox_operator_archives_total{result="failed"}[5m]) > 0` — a
  broken archive backend (credentials, bucket) that will silently strand
  transcripts.
- `sandbox_operator_environments` combined with `CNIEnforcement != Enforced`
  when any class declares `Restricted` — isolation is claimed but not
  verified.

## Kubernetes Events

The reconciler's status-write path emits one `Event` per meaningful
phase/condition transition. Recorder reasons: `SlotGranted` (emitted from
the scheduler's own grant, the one Event outside the status-write hook),
`Starting`/`Waking` (cold start vs. resuming from a snapshot), `Started`,
`Freezing`/`Frozen`/`SnapshotFailed`, `WaitSatisfied`, `Completed`/`Failed`,
`Archived` — eleven reasons. Two `Warning`-typed events exist outside that
set: `NetworkPolicyNotEnforced` (emitted on every reconcile while
`Restricted` is declared but CNI enforcement is unverified) and
`ArchiveSkippedByEscapeHatch` (emitted every time the
`sandbox.psenna.dev/remove-finalizer` annotation is honored). Every Event is
also logged at `V(1)` for free by controller-runtime's own recorder.

## Logs

`--log-verbosity` (`LOG_VERBOSITY` env, default `0`, valid `0`–`4`). `V(0)`/
`Info` is rare, significant, and irreversible (process start/stop, permanent
storage deletion). `V(1)` is per-reconcile/per-pass detail — this includes
`ensurePod`'s **swallowed render error**: when `RenderPod` fails — most
commonly a `rootless-podman` class in a namespace whose Pod Security
Standard rejects the engine's relaxations, or (rare) an unresolvable engine
type — `ensurePod` logs that failure at `V(1)` and returns `nil` rather than
surfacing it as a reconcile error. (The PSS-incompatibility case is *also*
surfaced as a `Warning EngineNamespaceIncompatible` Event and the
`EngineSecurityRelaxed=Unknown/NamespacePodSecurityIncompatible` condition,
so raising log verbosity is not the only way to see it — but the log is
still the most detailed record.) If an environment is stuck
`PodReady=False/PodNotCreated` in phase `Restoring`, check the
`EngineSecurityRelaxed` condition and `kubectl get events` first;
`--log-verbosity=1` on the operator is the most detailed record, and the
only one at all for an unresolvable-engine-type render failure (which
carries no condition or Event of its own).

## Troubleshooting

### Read this first

```sh
kubectl -n <ns> describe sandboxenvironment <name>
```

Nine conditions are ever set on a `SandboxEnvironment`: the six lifecycle
conditions `Scheduled`, `PodReady`, `Frozen`, `WaitSatisfied`, `Archived`,
`Ready` (always written in that order), plus three non-lifecycle conditions
appended by the reconciler and never pruned: `EngineSecurityRelaxed`,
`NetworkPosture`, `CNIEnforcement`. Also read `status.snapshotAttempt`,
`status.restoreAttempt`, and `status.probeAttempt` — these carry the
detailed, machine-readable reason behind several condition reasons below.

### Condition reasons

Every reason the operator can put on a condition. `Appears on` names the
condition(s) it can be set on.

**Scheduling** (`Scheduled`):

| Reason | Appears on | Status | What it means | What to do |
|---|---|---|---|---|
| `SlotGranted` | Scheduled | True | This environment holds an execution slot. | Nothing. |
| `Queued` | Scheduled (also Ready in phase `Ready`) | False | All `--slot-capacity` slots are taken. Message reads `queued at position N of M` when computable. | Compare `sandbox_operator_slots_used` to `sandbox_operator_slot_capacity`; raise `slots.capacity`, or raise this env's `spec.priority` (-1000..1000, higher first). |
| `ResourcesNotReady` | Scheduled | False | A child object is missing or a class reference could not be resolved. Message is one of: `waiting for <Kind> <name>`; `<Kind> <name> is owned by another object`; `workspace PVC <name> is Lost`; the credential-resolution error; the network-peer-resolution error. | Read the message. `waiting for` -> check the operator's RBAC and logs. `owned by another object` -> a name collision; delete the squatter. `Lost` -> the bound PV was destroyed. Credentials -> see "The S3 credentials trap" above. Network -> see engines.md/security.md. |
| `Suspended` | Scheduled, Frozen, Ready | False (Frozen: True) | `spec.suspend: true`. | `kubectl patch … -p '{"spec":{"suspend":false}}' --type=merge`. |
| `Waiting` | Scheduled, Ready | False | Phase `Waiting`; the environment is frozen and holds no slot by design. | See "Stuck in Waiting" below. |
| `Terminal` | Scheduled | False | Phase is `Done`/`Failed`; slots are never held. | Nothing. |

**Pod** (`PodReady`):

| Reason | On | Status | What it means | What to do |
|---|---|---|---|---|
| `PodRunning` | PodReady | True | Pod is `Running` and its `Ready` condition is True. | — |
| `PodPending` | PodReady | False | Pod exists but is not yet Running+Ready. | `kubectl describe pod`; usually image pull or scheduling. |
| `PodNotCreated` | PodReady | False | No pod exists yet. In phase `Pending`/`Ready` this is normal. **In phase `Restoring` it usually means a render error was swallowed at `V(1)` by `ensurePod`** — most commonly a `rootless-podman` class whose target namespace's Pod Security Standard rejects the engine's relaxations (see [engines.md](engines.md)). | Check `EngineSecurityRelaxed`; then `kubectl logs deploy/…-controller-manager` with `--log-verbosity=1`. |
| `PodDeleted` | PodReady | False | Phase `Freezing`/`Waiting`; the pod is intentionally gone. | — |
| `PodNotObserved` | PodReady | **Unknown** | The operator's `Get` on the pod **failed** — it cannot see pods at all. Deliberately distinct from "no pod". | Check operator RBAC on `pods` and API-server reachability. |
| `PodSucceeded` | PodReady | False (phase `Done`) | Pod exited 0 **without** the agent calling `POST /v1/done`. | Normal for agents that do not use the control API. |

**Pod failure** (allowlisted `PodFailure.Reason`):

| Reason | On | What it means | What to do |
|---|---|---|---|
| `PodFailed` | PodReady, Ready | Generic pod `Failed`. Message: `<pod.status.reason>: container <name> exited with code <N> (<reason>)`. | `kubectl logs <pod> -c agent`. The pod is **kept** on failure for exactly this. |
| `ImagePullFailure` | PodReady, Ready | A container is `Waiting` with `ImagePullBackOff` or `InvalidImageName`. `ErrImagePull`/`ImageInspectError`/`RegistryUnavailable` are deliberately **excluded** as transient. | Fix `agent.image` / `--sidecar-image`; add `imagePullSecrets`; `kind load docker-image` for a local image. |
| `Unschedulable` | PodReady, Ready | `PodScheduled=False/Unschedulable` persisted past the grace window. | `kubectl describe pod` -> node resources, `nodeSelector`, or a `WaitForFirstConsumer` PVC pinned to a full node. |
| `RestoreVerificationFailed` | PodReady, Ready | The `restore` init container exited non-zero: checksum mismatch, missing/invalid manifest, extract failure. | Read `status.restoreAttempt.reason`: `BackendUnreachable`, `BackendUnsupported`, `CredentialsInvalid`, `ManifestMissing`, `ManifestInvalid`, `ChecksumMismatch`, `ExtractFailed`, `PurgeFailed`, `Internal`. |

**Freeze** (`Frozen`):

| Reason | On | Status | What it means | What to do |
|---|---|---|---|---|
| `WaitDeclared` | Frozen | True | Frozen because the agent declared `status.waitFor`. | — |
| `SnapshotInProgress` | Frozen | False | The sidecar is quiescing/archiving/uploading. | Wait; watch `status.snapshotAttempt`. |
| `PodTerminating` | Frozen | False | Snapshot done, pod still terminating (grace period 120s). | Wait. |
| `SnapshotFailed` | Frozen **and** Ready | False | `status.snapshotAttempt.phase == Failed` for the freeze in flight. **The environment holds in `Freezing` forever and the pod is never deleted** — deliberate, so the agent's context is not lost. | Read `status.snapshotAttempt.reason`: `BackendUnreachable`, `BackendUnsupported`, `CredentialsInvalid`, `ArchiveFailed`, `TeardownFailed`, `Internal`, `RestoreInProgress`. Fix the backend/credentials; the sidecar retries. |
| `NotFrozen` | Frozen | False | Default outside `Freezing`/`Waiting`. | — |

**Probes** (`WaitSatisfied`):

| Reason | On | Status | What it means | What to do |
|---|---|---|---|---|
| `ProbeSatisfied` | WaitSatisfied | True | The wait cleared; `status.waitFor` is cleared and the env returns to `Ready`. | — |
| `ProbePending` | WaitSatisfied | False | Evaluated, not yet satisfied. Backoff 1s->2s->4s->8s->8s… with +-20% jitter. | Check `status.probeAttempt.{attempts,lastAttemptAt,nextEligibleAt}`. |
| `ProbeNotEvaluated` | WaitSatisfied | **Unknown** | The evaluator did **not** run this pass (`ProbeObserved=false`). | Only happens when the probe evaluator is nil (unit tests) or the phase/`waitFor` gate is closed — in a shipped binary this should not persist. |
| `NoWaitDeclared` | WaitSatisfied | False | `status.waitFor` is nil. Default outside `Waiting`. | — |
| `ProbeFailed` | WaitSatisfied **and** Ready | False -> phase `Failed` | 3 consecutive *unevaluatable* results. Transient (5xx/transport) errors neither increment nor reset the streak. | Read `status.probeAttempt.message`. Usual causes: bad `url`, broker auth failure, S3 key outside the env's prefix. |

**Archive** (`Archived`):

| Reason | On | Status | What it means | What to do |
|---|---|---|---|---|
| `ArchiveWritten` | Archived | True | `archive/run.json` (+ `archive/context.tar.zst` when a snapshot existed) landed. | `status.archive.uri` / `status.archive.runJSONSHA256`. |
| `ArchivePending` | Archived | False | Terminal but the archive Job has not completed. | If it sticks: the class must be S3-backed; check the archive Job and `sandbox_operator_archives_total{result="failed"}`'s **rate** (it increments once per reconcile while broken). |
| `NotTerminal` | Archived | False | Not `Done`/`Failed` yet. | — |

**Summary / lifecycle** (`Ready`):

| Reason | On | Status | What it means | What to do |
|---|---|---|---|---|
| `Pending` | Ready | False | Phase `Pending`. | — |
| `Restoring` | Ready | False | Phase `Restoring`. | — |
| `Running` | Ready | **True** | The only reason `Ready=True` is ever set. | — |
| `Freezing` | Ready | False | Phase `Freezing`, no snapshot failure. | — |
| `Succeeded` | Ready | False | `Done` via a pod that exited 0 without `/v1/done`. | — |
| `ClassNotResolved` | Ready | **Unknown** | `spec.classRef.name` could not be `Get`. The phase is **held**, the slot is neither granted nor revoked, and the timeouts still bound the env. Requeues on a fixed short interval. | `kubectl get sandboxclass` — `SandboxClass` is **cluster-scoped**. Check the operator's RBAC on `sandboxclasses`. |

**Agent**:

| Reason | On | Status | What it means | What to do |
|---|---|---|---|---|
| `AgentReportedSuccess` | PodReady, Ready | False (phase `Done`) | `POST /v1/done {"outcome":"success"}`. | — |
| `AgentReportedFailure` | Ready | False (phase `Failed`) | `POST /v1/done {"outcome":"failure"}`. Message is the agent's own text, truncated to 512 bytes. | Read the message, then the agent logs. |

**Timeouts** (precedence Total -> Running -> Waiting):

| Reason | On | What it means | What to do |
|---|---|---|---|
| `RunningTimeoutExceeded` | PodReady **and** Ready | `timeouts.running` (default `6h`) elapsed since `PodReady=True`. Only in phase `Running`. | Raise `spec.timeouts.running`. |
| `WaitingTimeoutExceeded` | PodReady **and** Ready | `timeouts.waiting` (default `24h`) since `Frozen=True`. Only in phase `Waiting`. | The wait never cleared — fix the probe. |
| `TotalTimeoutExceeded` | PodReady **and** Ready | `timeouts.total` (default `72h`) since `metadata.creationTimestamp`. Applies in **every** non-terminal phase and never pauses. **This is what eventually kills an environment stuck on a render failure that never produces a pod** (e.g. a `rootless-podman` class in a PSS-incompatible namespace). | Fix the real cause; do not just raise the timeout. |

**`EngineSecurityRelaxed`**:

| Reason | Status | What it means | What to do |
|---|---|---|---|
| `NoRelaxation` | False | The engine needs no `securityContext` weakening. Today this means `engine.type: none`. | — |
| `EngineRelaxationApplied` | True | The engine weakened the baseline; the message enumerates `<container>: <kind> (<reason>)` sorted by (container, kind). `engine.type: rootless-podman` produces this: three relaxations (`AppArmorUnconfined`, `SeccompUnconfined`, `AllowPrivilegeEscalation`) on the `podman` sidecar only. | Read the message; see [`security.md`](security.md). |
| `NamespacePodSecurityIncompatible` | **Unknown** | The engine *would* weaken the baseline, but the target namespace's `pod-security.kubernetes.io/enforce` label rejects the exact fields it needs — so `RenderPod` fails at render time and no pod is ever admitted. Nothing is actually relaxed, which is why this is `Unknown`, not `True`. Also emits a `Warning EngineNamespaceIncompatible` Event, since `ensurePod` swallows the underlying render error at `V(1)`. | Read the message (it names the namespace, the enforced level, and the exact relaxation kinds it rejects). Either label the namespace `pod-security.kubernetes.io/enforce=privileged` (or remove the label), or set `spec.engine.type: none` on the class. See [`engines.md`](engines.md#the-pss-constraint). |
| `EngineUnavailable` | **Unknown** | The class is unresolved, **or** the selected engine type is not one the operator implements (any value outside `none`/`rootless-podman`). This reason no longer applies to `rootless-podman` — that engine is implemented as of [#24](https://github.com/psenna/ai-sandbox/issues/24). | If the class is unresolved, check `spec.classRef.name`. If the engine type is genuinely unimplemented, that class needs a different `engine.type`. See [`engines.md`](engines.md). |

**`NetworkPosture`**:

| Reason | Status | What it means |
|---|---|---|
| `Restricted` | True | Egress restricted to declared peers; a NetworkPolicy is rendered. |
| `Open` | False | Egress unrestricted; **no NetworkPolicy is rendered at all**, and a stale one is deleted. |
| `Unknown` | Unknown | Class unresolved. |

**`CNIEnforcement`**:

| Reason | Status | What it means | What to do |
|---|---|---|---|
| `Enforced` | True | The probe ran a real pod-to-pod connectivity test and default-deny blocked it. | — |
| `NotEnforced` | **False** | The probe ran and **confirmed** the CNI does not enforce. `Restricted` isolation is not actually being enforced. | Install an enforcing CNI (Calico/Cilium/Antrea). kindnet does enforce; the plain AWS VPC CNI does not. |
| `Unconfirmed` | Unknown | The probe has not run yet, **or** could not run to completion (an internal probe-failure reason is mapped here, never surfaced directly). | Check the operator's namespace for `sandbox.psenna.dev/cni-probe` pods and RBAC on `pods`/`networkpolicies`. |

> **Naming collision to note:** an internal CNI-probe-only reason has the
> string value `"ProbeFailed"` — the same string as the wait-probe
> `ProbeFailed` reason above. It is never surfaced as a **condition**
> reason itself (it is always mapped to `Unconfirmed` first). A
> `ProbeFailed` you see on a condition is always the wait-probe one.

### Stuck in `Pending`

`Pending` is the initial phase before the operator has confirmed all child
resources exist. If it lingers: check `Scheduled` — usually
`ResourcesNotReady` (a missing/misconfigured class reference, or a
credential-resolution failure).

### Stuck in `Ready`

Ready but never granted a slot, or granted one and never transitioning:

- `Scheduled=False/Queued` — capacity. Check `slots_used`/`slot_capacity`/
  `queue_depth`; raise capacity or `spec.priority`. The message gives
  `queued at position N of M`.
- `Scheduled=False/Suspended` — `spec.suspend: true`.
- `Ready=Unknown/ClassNotResolved` — the class is missing. It is
  **cluster-scoped**; `kubectl get sandboxclass`.
- Slot granted but no transition at all — the operator may not be
  reconciling (check leader election, check for a crashloop on the operator
  Deployment).

### Stuck in `Restoring`

**Check this first: `EngineSecurityRelaxed=Unknown/NamespacePodSecurityIncompatible`.**
This is the most common cause today: `spec.engine.type: rootless-podman`
(the CRD default) requires three `securityContext` relaxations on its
`podman` sidecar, and the target namespace's `pod-security.kubernetes.io/enforce`
label — `baseline` or `restricted` — rejects them. `RenderPod` fails at
render time, `ensurePod` swallows that error at `V(1)` and returns `nil`, and
the environment parks in `Restoring` with `PodReady=False/PodNotCreated`
until `TotalTimeoutExceeded` fires (default 72h). A `Warning
EngineNamespaceIncompatible` Event is also emitted every reconcile, so
`kubectl get events` shows it even without raising the operator's log
verbosity. Fix: either label the namespace
`pod-security.kubernetes.io/enforce=privileged` (or leave it unlabelled), or
set `spec.engine.type: none` on the class. See
[`engines.md`](engines.md#the-pss-constraint) for the full explanation, and
"rootless-podman troubleshooting" below for the engine's other failure
modes.

If `EngineSecurityRelaxed` is `False/NoRelaxation` (engine is `none`) or
`True/EngineRelaxationApplied` (engine is `rootless-podman` and the
namespace is compatible) and you are still stuck: check `PodReady` for
`ImagePullFailure`, `Unschedulable`, or `RestoreVerificationFailed` (a
corrupt or incompatible snapshot — see "Wake failing" below).

### rootless-podman troubleshooting

Four symptoms specific to `engine.type: rootless-podman`, beyond the
PSS-incompatibility trap above:

- **The `podman` init container never becomes Ready, and the agent never
  starts.** Native sidecars gate every regular container on every init
  container's `startupProbe` succeeding first — the `podman` sidecar's own
  probe dials its own API (`podman --remote --url tcp://127.0.0.1:2375 info`),
  so a service that started but failed to bind is caught here rather than
  surfacing as a mysterious agent-side connection error. `kubectl logs <pod>
  -c podman` shows the bootstrap script's output and, usually, why `podman
  system service` did not come up (a bad `registryMirror` URL breaking
  `registries.conf`, for instance, or the node lacking rootless-overlay
  kernel support entirely).
- **`docker` commands from the agent container fail with a connection
  error.** `DOCKER_HOST`/`CONTAINER_HOST` are both `tcp://127.0.0.1:2375` on
  the agent — verify the pod actually rendered the `podman` init container at
  all (an unresolved class, or `engine.type: none`, means there is nothing
  listening on that port).
- **`docker run`/`docker pull` fails to reach any registry.** Under
  `network.isolation: Restricted`, the podman sidecar can pull only from
  peers the class declares — with no `services.registryMirror` configured, it
  can reach *no* registry at all. This is exactly what Helm guard **G22**
  catches at install time; if you are not using the chart, set
  `spec.services.registryMirror` on the class, add a covering `extraEgress`
  CIDR, or switch to `network.isolation: Open`.
- **A `spec.engine.image` override is rejected at render time** with `is not
  pinned by digest`. The `securityContext` this engine requires was
  established empirically against podman 5.8.2
  (`operator/docs/spike-rootless-podman.md`); an unpinned tag could silently
  change the engine's security posture out from under that validation, so
  `RenderPod` refuses it. Use a `repository@sha256:...` reference, or leave
  `spec.engine.image` empty for the operator's own pinned default.

### Stuck in `Running`

A pod is up but the environment never reaches `Done`/`Failed`: the agent has
not called `POST /v1/done` and the pod has not exited. This is expected for
a long-running task. If you expect completion: check the agent's own logs
(`kubectl logs <pod> -c agent`), and confirm `RunningTimeoutExceeded`
(default `6h`) has not already fired — if it has, the phase moves to
`Failed`, not "stuck".

### Freeze failing

`Frozen=False/SnapshotFailed` **and** `Ready=False/SnapshotFailed`. Message
format: `<snapshotAttempt.reason>: <message>`. The environment **holds in
`Freezing` and the pod is never deleted, by design** — the PVC is the only
copy of the agent's context until the snapshot exists. The seven
`status.snapshotAttempt.reason` values: `BackendUnreachable`,
`BackendUnsupported`, `CredentialsInvalid`, `ArchiveFailed`,
`TeardownFailed`, `Internal`, `RestoreInProgress`. Fix the backend or
credentials; the sidecar retries automatically. If the pod itself is
already gone but the PVC survives (an operator restart mid-freeze, for
example), the operator creates a one-shot recovery Job from the surviving
PVC rather than losing the freeze. Freeze, like wake and archive, is
**S3-only** — a `pvc`-backed class never reaches `Frozen=True`.

### Wake failing

`PodReady=False/RestoreVerificationFailed` and phase `Failed` — because the
`restore` init container is a *plain* (non-restartable) init container
ordered **last**, under `restartPolicy: Never`: a non-zero exit fails the
whole pod, which is exactly how a corrupt snapshot fails *loudly* instead of
starting an agent on a partially restored workspace. The nine
`status.restoreAttempt.reason` values: `BackendUnreachable`,
`BackendUnsupported`, `CredentialsInvalid`, `ManifestMissing`,
`ManifestInvalid`, `ChecksumMismatch`, `ExtractFailed`, `PurgeFailed`,
`Internal`.

**Warm vs cold**: `status.restoreAttempt.roots[]` reports `source:
Warm|Cold` per root (workspace, agent-home), plus `warmMissReason` and
`bytesDownloaded` (always `0` when `Warm`). A `Cold` restore where you
expected `Warm` is not necessarily a bug — the warm-cache TTL GC may
legitimately have already reclaimed the PVC.

### Stuck in `Waiting`

- `WaitSatisfied=False/ProbePending` — normal; read
  `status.probeAttempt.attempts` / `nextEligibleAt`; backoff is
  1s/2s/4s/8s/8s… +-20% jitter.
- `WaitSatisfied=False/ProbeFailed` -> phase goes `Failed` after **3
  consecutive unevaluatable** results (bad URL, broker auth failure, an S3
  key outside the environment's prefix — transient 5xx/transport errors do
  not count toward this).
- `WaitSatisfied=Unknown/ProbeNotEvaluated` — the evaluator did not run this
  pass; should not persist in a shipped binary.
- `Frozen=True/Suspended` with `waitFor: null` — clear `spec.suspend`.
- `WaitingTimeoutExceeded` at the class's `timeouts.waiting` (default 24h).

The four wait types and their parameters (`status.waitFor`, set by the agent
via `POST /v1/wait`):

| Type | Required params | Optional params |
|---|---|---|
| `GitProxyCheck` | `ref` | `repo` |
| `HTTPGet` | `url` | `expectStatus`, `expectBody` |
| `S3ObjectExists` | `key` | — |
| `NotBefore` | one of `time` or `duration` | — |

### Stuck in `Freezing` after the run already finished

An environment that reaches `Done`/`Failed` but has never been snapshotted
(agent home never captured, but a live pod still exists) takes a
**freeze-before-archive detour**: it re-enters `Freezing`, snapshots, then
proceeds through `Waiting` back to its terminal phase with the archive now
containing a real transcript. If it appears stuck there, it is following the
same freeze troubleshooting as above ("Freeze failing").

### Archive never appears

`Archived=False/ArchivePending` past the point you expect it: confirm the
class's `storage.backend.type` is `s3` (a `pvc`-backed class never gets an
archive — the finalizer is removed with nothing written). Check the archive
Job in the operator's namespace, and `sandbox_operator_archives_total{result="failed"}`'s
**rate** (it increments once per reconcile the archive stays broken, not
once per distinct failure).

### Deletion hangs

Every `SandboxEnvironment` carries a finalizer
(`sandbox.psenna.dev/environment-archiver`) that is only removed once a
terminal archive has been written (or a documented reason says archiving
was never possible or already happened). If a `kubectl delete` hangs: check
`Archived` — likely `ArchivePending` with the same causes as above. As a
last resort, the escape hatch removes the finalizer **without** archiving:

```sh
kubectl annotate sandboxenvironment <name> sandbox.psenna.dev/remove-finalizer=true
```

This is a deliberate, documented data-loss action — the operator emits a
`Warning ArchiveSkippedByEscapeHatch` Event every time it is honored, so it
is never silent.

### Collecting diagnostics

`operator/hack/e2e-collect.sh` shows the shape of a full diagnostics bundle
used by this repo's own CI: operator Deployment describe + logs, all
`SandboxClass`/`SandboxEnvironment` objects wide, the relevant pods
described, and recent Events, all written to an artifacts directory. It
redacts **Secret names and keys only, never values** — safe to attach to an
issue.
