# ai-sandbox-operator

A Helm chart for the [ai-sandbox operator](https://github.com/psenna/ai-sandbox/tree/main/operator):
a Kubernetes operator that schedules, runs, freezes/wakes and archives
ephemeral, policy-isolated AI coding-agent sandboxes.

It ships two cluster-scoped CRDs (`sandbox.psenna.dev/v1alpha1`):

| Kind | Scope | What it is |
| --- | --- | --- |
| `SandboxClass` | Cluster | A reusable template: agent image, container engine, shared service endpoints, storage backend, network isolation, timeouts. |
| `SandboxEnvironment` | Namespaced | One sandbox instance. References a `SandboxClass`, gets a slot, a workspace PVC, a pod, a NetworkPolicy, and eventually an archive. |

---

## Prerequisites

- **Kubernetes >= 1.25.** `Chart.yaml`'s `kubeVersion` enforces this: the CRDs
  use CEL `x-kubernetes-validations` (`BackendSpec`'s four `XValidation` rules
  and `EgressPeer`'s), which only became GA-usable in 1.25.
- **A CNI that actually enforces NetworkPolicy** if you intend to use
  `Restricted` isolation. Calico, Cilium and Antrea do; kindnet and the plain
  AWS VPC CNI do not. The operator runs a periodic CNI enforcement probe
  (`--cni-probe-interval`) and reports the answer; it cannot make a
  non-enforcing CNI enforce.
- **Pod Security Admission level of the namespaces sandbox pods run in.** This
  is not the operator's own namespace. `engine.type: rootless-podman` needs
  the relaxations `internal/render/engine.go` applies (AppArmor profile,
  `allowPrivilegeEscalation`, added capabilities, seccomp), which `baseline`
  and `restricted` both reject. Declare the level you enforce in
  `sandboxNamespaces.podSecurityEnforce` and guard **G6** will refuse an
  impossible combination at render time instead of leaving you with pods the
  API server silently rejects. The chart never labels a namespace it does not
  own — the value is a *declaration*, not an action.
- **Prometheus Operator CRDs** (`monitoring.coreos.com/v1`) only if you set
  `metrics.serviceMonitor.enabled=true`.

---

## Quick start

```sh
helm install ai-sandbox-operator deploy/helm/ai-sandbox-operator \
  --namespace ai-sandbox-operator-system --create-namespace

kubectl -n ai-sandbox-operator-system \
  rollout status deployment/ai-sandbox-operator

helm test ai-sandbox-operator -n ai-sandbox-operator-system --logs
```

The bare chart installs with no overrides. It creates **no** `SandboxClass` by
default, so nothing can run yet — the manager is started with
`--default-sandbox-class=default` regardless, and every `SandboxEnvironment`
that does not name a class explicitly stalls until a cluster-scoped
`SandboxClass` named `default` exists. Either create one yourself, or enable
the chart's:

```sh
helm upgrade ai-sandbox-operator deploy/helm/ai-sandbox-operator \
  -n ai-sandbox-operator-system \
  -f deploy/helm/ai-sandbox-operator/ci/default-class-s3-values.yaml
```

---

## CRDs

`crds/` carries a **byte-identical copy** of `config/crd/bases/*.yaml` —
controller-gen's own output from the Go API types. `make helm-crds-check` runs
in CI and fails the build on any difference; refresh the copy with
`make helm-crds` after any `make manifests` run that changes the CRDs.

Helm 3 installs everything under `crds/` automatically on `helm install` and
**never upgrades or deletes it**
([Helm's CRD documentation](https://helm.sh/docs/chart_best_practices/custom_resource_definitions/)).
That is deliberate here, not an oversight: a CRD delete garbage-collects every
custom resource of that kind cluster-wide.

To pick up a CRD change from a chart upgrade, apply it yourself first:

```sh
kubectl apply --server-side -f deploy/helm/ai-sandbox-operator/crds/
helm upgrade ai-sandbox-operator deploy/helm/ai-sandbox-operator -n ai-sandbox-operator-system
```

`helm install --skip-crds` skips the install-time apply if you manage CRDs
entirely outside Helm.

---

## Secrets

**This chart creates zero `Secret` objects and templates zero credential
values.** Every credential is a reference to a Secret you create yourself, in
the class-secret namespace (`classSecretNamespace`, defaulting to the release
namespace). No secret value is ever written into a ConfigMap or a CR by this
chart — the chart renders no ConfigMap at all, which is why `deployment.yaml`
carries no `checksum/config` pod annotation.

| Referenced by | Namespace | Keys the operator reads |
| --- | --- | --- |
| `defaultClass.storage.backend.s3.credentialsSecretRef.name` | `classSecretNamespace` | **Fixed keys**: `accessKeyID` (required), `secretAccessKey` (required), `sessionToken` (optional). |
| `defaultClass.services.gitProxy.tokenSecretRef.{name,key}` | `classSecretNamespace` | Whatever `key` names (default `token`). |

> **Trap: the S3 keys are fixed, and `credentialsSecretRef.key` is ignored for
> the S3 backend.** `internal/storage/credentials.go` reads
> `accessKeyID`/`secretAccessKey`/`sessionToken` by those exact names. The
> `.key` field is carried in values only because the CRD's `SecretKeyRef`
> requires it. Naming your S3 Secret's data key `token` and expecting it to be
> used will silently fail: `internal/controller/resources.go`'s
> `resolveCredentials` blocks every environment at `Pending` with
> `reading class-referenced s3 credentials secret`.

```sh
kubectl -n ai-sandbox-operator-system create secret generic sandbox-s3-credentials \
  --from-literal=accessKeyID=... \
  --from-literal=secretAccessKey=...
```

**Rotation is live.** The operator reads these Secrets uncached on every use,
so rotating their contents needs no rollout restart. Renaming a Secret does
require editing the `SandboxClass`.

---

## RBAC

`rbac.create=true` (the default) renders a `ClusterRole` whose `rules:` block
is **byte-for-byte the rule set in `config/rbac/role.yaml`** — controller-gen's
output from the `+kubebuilder:rbac` markers on
`internal/controller/sandboxenvironment_controller.go`. `make helm-rbac-check`
fails CI the moment the two disagree.

| # | apiGroups | resources | verbs | Why |
| --- | --- | --- | --- | --- |
| 1 | `""` | `configmaps`, `secrets`, `serviceaccounts` | create, get, list, patch, update, watch | Per-environment children applied by server-side apply (ConfigMap: run config + `task.md`; Secret: the projected per-env Secret and the snapshot Secret; ServiceAccount: the per-env SA). `patch` — not just `update` — is what an SSA apply-patch needs. Secrets are cluster-wide because environments run in arbitrary namespaces while the class-referenced source Secret lives in `classSecretNamespace`. |
| 2 | `""` | `persistentvolumeclaims`, `pods` | create, delete, get, list, patch, update, watch | The workspace PVC (`delete` is warm-cache reclamation, #29); the sandbox pod from `RenderPod`, plus freeze deleting the frozen pod and the two short-lived CNI-probe pods. |
| 3 | `""` | `services` | get, list, watch | Reads each in-cluster endpoint's Service to lift its pod selector, and the `kubernetes` Service in `default` for its ClusterIP to build the API-server egress ipBlock. Read-only. |
| 4 | `""`, `events.k8s.io` | `events` | create, patch | The event recorder, in both API groups (`events.k8s.io` added in #33). |
| 5 | `batch` | `jobs` | create, delete, get, list, patch, watch | The snapshot Job (#28 recovery) and the archive Job (#32 terminal archive). **No `update`**: Job specs are immutable after creation. |
| 6 | `networking.k8s.io` | `networkpolicies` | create, delete, get, list, patch, update, watch | The Restricted-isolation policy; `delete` reclaims a stale policy when a class switches to `Open`. |
| 7 | `rbac.authorization.k8s.io` | `roles`, `rolebindings` | create, get, list, patch, update, watch | The per-environment Role letting the sandbox's restore container patch only its own `status.restoreAttempt`. Legal under the API server's RBAC escalation check precisely because rules 8/9/10 grant the operator the verbs it hands out a `resourceNames`-restricted subset of. |
| 8 | `sandbox.psenna.dev` | `sandboxclasses` | get, list, watch | Class resolution and retention GC's class list. Read-only. |
| 9 | `sandbox.psenna.dev` | `sandboxclasses/status`, `sandboxenvironments/status` | get, patch, update | Lifecycle writes phase/conditions; class-level Secret-problem conditions. |
| 10 | `sandbox.psenna.dev` | `sandboxenvironments` | create, delete, get, list, patch, update, watch | The primary watched kind; `create`/`delete` are needed for rule 7's escalation check and the archive finalizer path. |
| 11 | `sandbox.psenna.dev` | `sandboxenvironments/finalizers` | update | `FinalizerArchiveOnDelete`, added and removed by the archive controller. |

**Deliberately NOT granted** — if you see any of these in a diff of this
chart's ClusterRole, something has gone wrong:

- no `namespaces`, no `nodes`
- no `deployments` / `statefulsets` / `daemonsets`
- no `customresourcedefinitions`
- no `pods/exec`, no `pods/portforward`
- no `escalate`, no `bind`
- no `*` verb and no `*` resource, anywhere
- no `delete` on `secrets`

### Leader-election Role

`templates/role-leaderelection.yaml` renders only when
`rbac.create && leaderElection.enabled`, into
`leaderElection.namespace | default .Release.Namespace`, and grants **leases
only**:

```yaml
- apiGroups: ["coordination.k8s.io"]
  resources: ["leases"]
  verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
```

This is **narrower than `config/rbac/leader_election_role.yaml`** on purpose.
The kustomize base also grants `configmaps` (inherited kubebuilder scaffolding
for the long-dead `configmapsleases` lock — controller-runtime has defaulted to
`leases` since v0.11) and `events` (already covered by ClusterRole rule 4).
Both are dropped here.

### Managing RBAC outside the chart

Set `rbac.create=false` together with `serviceAccount.create=false` and
`serviceAccount.name` pointing at a ServiceAccount your own RBAC bundle binds.

---

## The default SandboxClass

`defaultClass.create=true` renders one cluster-scoped `SandboxClass` named
`defaultClass.name`. `--default-sandbox-class` is set to that same name
**whether or not `create` is true**, so the two can never drift apart.

The rendered class carries `helm.sh/resource-policy: keep`. This matters:
without it, `helm uninstall` would delete the class out from under any
still-running `SandboxEnvironment` that references it, which is exactly the
"uninstall must not disrupt running work" property the chart is built for.

Render rules worth knowing:

- `spec.agent.model` is omitted entirely when all four tiers are empty.
- `spec.engine.image` is omitted when empty.
- A service block (`gitProxy` / `dependaProxy` / `ollama`) is emitted **only**
  when its `enabled: true`. A disabled service is *absent*, never `{}` —
  `internal/controller/network.go`'s `serviceEndpoints` branches on `!= nil`,
  so an empty-but-present block changes behavior.
- `spec.storage.workspace.storageClassName` is omitted when `null` (use the
  cluster default StorageClass) and emitted as `""` when explicitly empty (use
  no StorageClass).
- Only the backend block matching `.type` is emitted, per `BackendSpec`'s four
  CEL rules.

Render-time guards (`templates/_validations.tpl`) turn every one of the CRD's
`MinLength`/required/`XValidation` constraints into a `helm template` error
with an explanation, rather than an `kubectl apply` rejection at install time.

Need more than one class? Leave `defaultClass.create=false` and manage your
classes separately — they are cluster-scoped and intentionally decoupled from
any single Helm release.

---

## Storage backends

| | `s3` | `pvc` |
| --- | --- | --- |
| Required | `endpoint`, `bucket` (>= 3 chars), `credentialsSecretRef.name` | `claimName` |
| Optional | `region`, `prefix`, `forcePathStyle`, `credentialsSecretRef.key` (ignored) | `subPath` |
| Contributes a network egress peer | yes (the endpoint) | no |

`forcePathStyle` defaults to **`true`**, matching the CRD default: most
self-hosted S3-compatible stores (MinIO above all) require path-style
addressing. Set it `false` for AWS S3 proper.

A `pvc` backend needs the named PersistentVolumeClaim to exist in **every
namespace sandboxes run in** — the operator does not create it.

**Restricted isolation + an external S3 endpoint needs an `extraEgress` CIDR.**
A label selector cannot match an off-cluster host, so
`resolveServiceEndpoint` fails the class outright and every environment using
it stalls at `Pending`. Guard **G10** catches this at render time. See
`ci/default-class-open-network-values.yaml` for the `Open`-isolation
counterpart that legitimately needs no CIDR.

---

## Network isolation

`defaultClass.network.isolation`:

- **`Restricted`** — sandbox pods may reach only the class's configured service
  endpoints, the Kubernetes API server, and whatever `extraEgress` declares.
  Every other egress is denied.
- **`Open`** — no NetworkPolicy is rendered; `extraEgress` is not consulted.

`extraEgress` entries set **exactly one** of `cidr` or `selector` (the CRD's
`EgressPeer` `XValidation` enforces this):

```yaml
defaultClass:
  network:
    isolation: Restricted
    extraEgress:
      - cidr: 140.82.112.0/20            # github.com
        ports: [{protocol: TCP, port: "443"}]
      - selector:
          namespace: shared-services
          podSelector:
            matchLabels: {app: artifact-cache}
```

### `network.operatorIngressLabel` must match this chart's own pod labels

`--operator-ingress-label` identifies the operator's own pods in the ingress
rule of every Restricted-isolation NetworkPolicy (so the operator can reach
sandbox pods). The chart guarantees the match **by construction**:
`_helpers.tpl`'s `podLabels` splits this value on `=` and puts that exact
key/value onto the Deployment's pod template. Guard **G15** rejects a value it
cannot parse as a single `key=value` pair — worth having, because
`operatorIngressSelector` silently falls back to
`control-plane=controller-manager` on a malformed value, producing a policy
that matches nothing.

Note it is *not* in `selectorLabels`: `Deployment.spec.selector` is immutable,
so welding the ingress label into it would make the value un-changeable after
install.

### `classSecretNamespace` and the operator-ingress peer

`classSecretNamespace` is also the **namespace** of the operator-ingress peer
in every Restricted NetworkPolicy (`resolveNetworkPeers`). Pointing it at a
namespace other than the one the operator actually runs in silently blocks the
operator's own probes from reaching sandbox pods. Guard **G14** refuses that
combination unless you set
`unsafeAllowForeignClassSecretNamespace=true` to acknowledge you have arranged
reachability some other way. See `ci/watch-namespace-values.yaml`.

---

## Metrics & ServiceMonitor

The manager's `/metrics` listener is unauthenticated by design (#33) and is
exposed as a plain `ClusterIP` Service named `<fullname>-metrics` when
`metrics.enabled=true`. `metrics.enabled=false` passes
`--metrics-bind-address=0` and renders no Service at all.

**Why a plain ClusterIP Service is correct even at `replicaCount > 1`** — this
is the non-obvious part. `internal/controller/metricscollector.go`'s
`MetricsCollector` deliberately returns `NeedLeaderElection() == false`, with
an explicit doc comment saying that leader-electing it would leave every
non-leader replica's gauges permanently zero. So **every** replica computes and
serves live gauges. A Prometheus `ServiceMonitor` on a ClusterIP Service
scrapes the *Endpoints* behind it, not the VIP — so all replicas get scraped.
No headless Service and no `PodMonitor` are needed.

The 13 metric families served:

| Metric | Kind |
| --- | --- |
| `sandbox_operator_environments` | gauge, by phase |
| `sandbox_operator_slot_capacity` | gauge |
| `sandbox_operator_slots_used` | gauge |
| `sandbox_operator_queue_depth` | gauge |
| `sandbox_operator_queue_wait_seconds` | histogram |
| `sandbox_operator_freeze_duration_seconds` | histogram |
| `sandbox_operator_wake_duration_seconds` | histogram |
| `sandbox_operator_snapshot_size_bytes` | histogram |
| `sandbox_operator_archives_total` | counter |
| `sandbox_operator_retention_deleted_total` | counter |
| `sandbox_operator_warm_cache_reclaimed_total` | counter |
| `sandbox_operator_reconcile_errors_total` | counter |
| `sandbox_operator_probe_evaluations_total` | counter |

`metrics.serviceMonitor.enabled=true` renders a `ServiceMonitor`. It is gated
on that value **alone**, not on `.Capabilities.APIVersions`: `helm template`
without `--api-versions` would silently drop a capability-gated object and
every CI render assertion for it would pass vacuously. The cost is that you
must install the Prometheus Operator's CRDs yourself first. Guard **G18**
rejects `serviceMonitor.enabled=true` with `metrics.enabled=false`.

`helm test` (`tests.enabled`, default on) curls `/metrics` through the Service
and asserts `sandbox_operator_slot_capacity` is present *and equals*
`slots.capacity`, and that `sandbox_operator_environments{` is present (which
proves `MetricsCollector` completed a pass). It uses `curlimages/curl`, not the
operator image — the operator runs on `gcr.io/distroless/static:nonroot`, which
has no shell and no curl. `/healthz` is deliberately not curled: the health
port is kubelet-only and is in no Service.

---

## Retention

**A fresh install deletes archives for real after 7 days.** `retention.ttl`
defaults to `168h` and `retention.dryRun` defaults to **`false`** — matching
the binary's own default in `internal/config/config.go` rather than picking a
different one at the chart layer, so `helm install` and a bare `manager` binary
behave identically.

For a first rollout, watch a sweep before trusting it:

```sh
helm install ... --set retention.dryRun=true
```

`dryRun` makes `RetentionGC` log what it *would* delete, in **both** the
retention and orphan sweeps, without deleting anything. `retention.ttl: 0`
disables the TTL sweep entirely; orphan cleanup still runs regardless.
`retention.gcInterval` (default `30m`) is how often a sweep runs.

---

## Leader election & replicas

`leaderElection.enabled` defaults to `true`. Extra replicas are hot standby:
only the leader reconciles.

Guard **G19** refuses `replicaCount > 1` with `leaderElection.enabled=false`,
and this is a correctness constraint rather than a style preference. The
`SlotScheduler`, `WarmCacheGC`, `CNIProbe` and `RetentionGC` runnables are all
leader-elected precisely because a double run double-grants slots and
double-deletes storage — so two un-elected replicas will over-admit sandboxes
past `slots.capacity` and race archive deletion.

The one thing that is *not* leader-elected is `MetricsCollector` (see
"Metrics & ServiceMonitor" above), which is why every replica still serves
useful gauges.

`leaderElection.enabled=false` also suppresses the leader-election
`Role`/`RoleBinding` — see `ci/no-leader-election-values.yaml`.

---

## Upgrading

`helm upgrade` performs a `RollingUpdate` on the operator Deployment. This is
safe by design and does **not** disrupt running sandboxes:

- Reconciliation is level-triggered: the new manager rebuilds its whole view
  from the API server on start.
- Sandbox pods, PVCs and NetworkPolicies are owned by `SandboxEnvironment`
  objects, not by the Deployment — restarting the manager does not touch them.
- Leader election hands over cleanly between the old and new replica.

The one caveat is CRDs: Helm never upgrades `crds/`. Apply them yourself first
when the chart version you are moving to changes them (see "CRDs" above).

---

## Uninstalling

```sh
helm uninstall ai-sandbox-operator -n ai-sandbox-operator-system
```

removes the Deployment, Service, ServiceAccount, RBAC objects and (if enabled)
the ServiceMonitor. It deliberately **leaves behind**:

- the **CRDs** (Helm never deletes `crds/`),
- every **`SandboxEnvironment`** and its pods, PVCs and NetworkPolicies,
- the **default `SandboxClass`**, if `defaultClass.create` was true — it
  carries `helm.sh/resource-policy: keep`.

Reinstalling the chart picks the surviving environments straight back up.

To tear everything down, delete the workloads first, then the CRDs — and be
sure, because deleting a CRD garbage-collects every resource of that kind
across the whole cluster:

```sh
kubectl delete sandboxenvironments --all --all-namespaces
kubectl delete sandboxclasses --all
kubectl delete crd sandboxenvironments.sandbox.psenna.dev sandboxclasses.sandbox.psenna.dev
```

---

## Values reference

See `values.yaml` for the full annotated set. Every operator flag below was
cross-checked against `internal/config/config.go`'s `flag.*Var` calls, and
every range against `Config.Validate()`.

### Image and deployment

| Value | Default | Notes |
| --- | --- | --- |
| `image.repository` | `ghcr.io/psenna/ai-sandbox-operator` | Required, non-empty (JSON-schema enforced). |
| `image.tag` | `""` | Falls back to `Chart.AppVersion`. There is no `:latest`. |
| `image.digest` | `""` | `sha256:...`; wins over `tag`. |
| `image.pullPolicy` | `IfNotPresent` | |
| `imagePullSecrets` | `[]` | |
| `replicaCount` | `1` | `> 1` requires leader election (G19). |
| `sidecarImage` | `""` | `--sidecar-image`. `""` resolves to the same ref as `image`. |
| `nameOverride` / `fullnameOverride` / `commonLabels` | `""` / `""` / `{}` | |

`sidecarImage` deserves a note: the `sandboxctl` control-channel sidecar ships
**inside the operator image** (`operator/Dockerfile` builds both `/manager` and
`/sandboxctl`), so it must stay version-locked to the manager. The chart does
that automatically. Overriding it independently is almost always a bug. Note
also that the binary's own default (`ghcr.io/psenna/ai-sandbox-operator:dev`)
is never in play — the chart always passes the flag explicitly.

### Operator configuration

| Value | Flag | Default | Constraint |
| --- | --- | --- | --- |
| `clusterID` | `--cluster-id` | `default` | DNS-1123 label (G13) |
| `watchNamespace` | `--watch-namespace` | `""` | `""` = all namespaces |
| `classSecretNamespace` | `--class-secret-namespace` | `""` | `""` = release namespace; foreign values need `unsafeAllowForeignClassSecretNamespace` (G14) |
| `unsafeAllowForeignClassSecretNamespace` | — | `false` | Escape hatch for G14 |
| `slots.capacity` | `--slot-capacity` | `4` | `>= 1` (G1) |
| `slots.schedulerInterval` | `--scheduler-interval` | `5s` | 100ms–5m (G16) |
| `warmCache.gcInterval` | `--warm-cache-gc-interval` | `1m` | 1s–1h (G16) |
| `retention.ttl` | `--retention-ttl` | `168h` | `>= 0`; `0` disables the TTL sweep |
| `retention.dryRun` | `--retention-dry-run` | `false` | |
| `retention.gcInterval` | `--retention-gc-interval` | `30m` | 1s–1h (G16) |
| `cniProbe.interval` | `--cni-probe-interval` | `5m` | 1s–1h (G16) |
| `network.operatorIngressLabel` | `--operator-ingress-label` | `control-plane=controller-manager` | single `key=value` (G15) |
| `logging.verbosity` | `--log-verbosity` | `0` | 0–4 (G17) |
| `leaderElection.enabled` / `.id` / `.namespace` | `--leader-elect` / `--leader-election-id` / `--leader-election-namespace` | `true` / `sandbox-operator.sandbox.psenna.dev` / `""` | |
| `metrics.enabled` / `.port` / `.collectInterval` | `--metrics-bind-address` / `--metrics-collect-interval` | `true` / `8080` / `15s` | interval 1s–5m (G16) |
| `healthProbe.port` | `--health-probe-bind-address` | `8081` | Probes always target it |
| `defaultClass.name` | `--default-sandbox-class` | `default` | non-empty (G20) |
| `extraArgs` | — | `[]` | Appended verbatim after the chart's own args |

Operator-flag durations must be **single-term** (`5s`, `30m`, `1h`) — the
JSON schema enforces that shape so the chart's own range guards can parse
them. The `SandboxClass` durations (`defaultClass.storage.warmCacheTTL`,
`defaultClass.timeouts.*`) use the CRD's own multi-term pattern (`1h30m` is
fine) because they pass straight through to the API server.

### Pod shaping

`podAnnotations`, `podLabels`, `podSecurityContext`, `containerSecurityContext`,
`resources`, `terminationGracePeriodSeconds`, `livenessProbe.*`,
`readinessProbe.*`, `priorityClassName`, `nodeSelector`, `tolerations`,
`affinity`, `topologySpreadConstraints`, `extraEnv`, `extraEnvFrom` — all
standard, all exercised together in `ci/scheduling-values.yaml`.

### The rest

| Value | Default | Notes |
| --- | --- | --- |
| `rbac.create` | `true` | See "RBAC" |
| `serviceAccount.create` / `.name` / `.annotations` / `.automountServiceAccountToken` | `true` / `""` / `{}` / `true` | The manager *is* an API-server client, hence automount `true` |
| `metrics.service.*`, `metrics.serviceMonitor.*` | see `values.yaml` | See "Metrics & ServiceMonitor" |
| `sandboxNamespaces.podSecurityEnforce` | `""` | Declaration only; drives G6 |
| `defaultClass.*` | `create: false` | See "The default SandboxClass" |
| `tests.enabled` / `tests.image.*` / `tests.resources` | `true` / `curlimages/curl:8.11.1` | See "Metrics & ServiceMonitor" |

---

## Render-time guards

`templates/_validations.tpl` is included as line 1 of both `deployment.yaml`
and `sandboxclass-default.yaml`, so `helm template -s` on either still fires
every guard. Each one names what is wrong, quotes the Go source that would
otherwise fail at runtime, and says how to fix it.

| Guard | Fires when |
| --- | --- |
| G1 | `slots.capacity < 1` |
| G2 | `backend.type=s3` without `s3.endpoint`/`.bucket` |
| G3 | `backend.type=pvc` without `pvc.claimName` |
| G4 | `backend.type=s3` without `s3.credentialsSecretRef.name` |
| G5 | `s3.bucket` shorter than 3 characters |
| G6 | `engine.type=rootless-podman` with `sandboxNamespaces.podSecurityEnforce` of `baseline`/`restricted` |
| G7 | `services.gitProxy.enabled` without `gitURL`/`brokerURL`/`tokenSecretRef.name` |
| G8 | `services.dependaProxy.enabled` with all three URLs empty |
| G9 | `services.ollama.enabled` without `baseURL` |
| G10 | `Restricted` isolation, an off-cluster endpoint, and no `extraEgress` `cidr` |
| G11 | `defaultClass.create=true` without `agent.image` |
| G12 | No image tag resolvable (`image.tag` empty and no `appVersion`) |
| G13 | `clusterID` or `classSecretNamespace` is not a DNS-1123 label |
| G14 | `classSecretNamespace` differs from the release namespace without the unsafe flag |
| G15 | `network.operatorIngressLabel` is not a single `key=value` pair |
| G16 | Any of the six operator intervals is outside `Validate()`'s range |
| G17 | `logging.verbosity` outside 0–4 |
| G18 | `metrics.serviceMonitor.enabled` with `metrics.enabled=false` |
| G19 | `replicaCount > 1` with `leaderElection.enabled=false` |
| G20 | `defaultClass.name` empty |

Guards, not the JSON schema, own every one of these cases: `values.schema.json`
is validated *before* templates render, so a `minimum`/`minLength` there would
pre-empt the guard's explanation with a generic schema message. The schema
carries only constraints no guard claims.

---

## Testing this chart locally

```sh
cd operator
make helm-lint          # helm lint --strict
make helm-template      # helm template --include-crds
make helm-crds-check    # crds/ == config/crd/bases/
make helm-rbac-check    # ClusterRole rules == config/rbac/role.yaml
```

`ci/*.yaml` are chart-testing fixtures; CI lints and renders the chart against
every one of them, then asserts each guard fires its own message.
