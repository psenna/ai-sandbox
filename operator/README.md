# ai-sandbox operator

A Kubernetes operator that runs the same kind of AI-coding-agent sandbox as
the [root repository's compose stack](../README.md), but as a cluster
resource: `kubectl apply` a `SandboxEnvironment` instead of attaching to a
container, get many concurrent policy-isolated runs instead of one, and get
freeze/wake so a run can pause for hours without holding a slot. See the
root README's [compose-vs-operator
comparison](../README.md#two-ways-to-run-this-the-compose-stack-or-the-kubernetes-operator)
if you have not picked which one you want yet.

## What it does

Two CRDs. A `SandboxClass` is a reusable, cluster-scoped template (image,
resources, engine, storage backend, network isolation, timeouts); a
`SandboxEnvironment` is one run against one class.

| CRD | Scope | Purpose |
|---|---|---|
| `SandboxClass` | Cluster-scoped | Reusable template: agent image + resources, engine type, storage backend (`s3`/`pvc`), network isolation (`Open`/`Restricted`), default timeouts. Referenced by name from `SandboxEnvironment.spec.classRef`. |
| `SandboxEnvironment` | Namespaced | One run: which repo, what task (a prompt or an issue reference), which class. The operator drives it through an 8-phase state machine to `Done` or `Failed`. |

## Architecture

```
                     ┌─────────────────────────────┐
                     │   manager (Deployment,       │
                     │   leader-elected)             │
                     │                               │
                     │  SandboxEnvironment           │
                     │  reconciler:                  │
                     │  Observe -> lifecycle.Next     │
                     │  -> actions -> writeStatus    │
                     │                               │
                     │  + 5 manager.Runnables:        │
                     │    SlotScheduler    (leader)  │
                     │    WarmCacheGC       (leader)  │
                     │    RetentionGC       (leader)  │
                     │    CNIProbeRunnable  (leader)  │
                     │    MetricsCollector  (NOT      │
                     │      leader -- every replica   │
                     │      keeps its own gauges live)│
                     └───────────────┬───────────────┘
                                     │ owns / watches
                                     ▼
       ┌─────────────────────────────────────────────────────┐
       │  per-environment children (owner ref -> env)          │
       │                                                       │
       │  ServiceAccount · Role/RoleBinding · workspace PVC     │
       │  ConfigMap (sandbox.json, task.md) · Secret (env)      │
       │  NetworkPolicy (Restricted only) · <env>-snapshot      │
       │  Secret (S3 backend only)                              │
       │                                                       │
       │  Pod:                                                 │
       │   init: sandboxctl (always)  init: restore (S3, wake)  │
       │   container: agent  (sole regular container)           │
       └─────────────────────────────────────────────────────┘
```

**The manager** is a single Deployment, leader-elected. Its
`SandboxEnvironment` reconciler follows one fixed shape every pass:
`Observe` (read the cluster's current facts) -> `lifecycle.Next` (a pure
function: facts + spec -> phase decision, no I/O) -> `actions` (apply what
the decision calls for) -> `writeStatus` (persist conditions/phase, emit
Events). Five `manager.Runnable`s run alongside the reconciler: the slot
scheduler, warm-cache GC, retention GC, and the CNI enforcement probe are
all leader-elected (one cluster-wide loop); the metrics collector
deliberately is **not** — every replica needs its own `/metrics` gauges
live, not just the leader's.

**Per-environment children**, all owned by a single controller
`ownerReference` back to the environment (deleting the CR garbage-collects
the lot): a `ServiceAccount` (the sidecar's identity, automount disabled), a
`Role`+`RoleBinding` (get/patch on this environment's own status only), the
workspace `PersistentVolumeClaim` (the warm cache, retained across
freeze/wake), a `ConfigMap` (resolved run config + task instructions), a
`Secret` (the agent's process environment), a `NetworkPolicy` (only under
`Restricted` isolation), and a snapshot-credentials `Secret` (only on an
S3-backed class).

**The pod's three containers**: `sandboxctl` (native sidecar, KEP-753,
always present, holds the Kubernetes credential, exposes the loopback
control API), `restore` (one-shot init container, S3-backed classes only,
rendered **last** among init containers, only present on a wake), and
`agent` (the sole regular container — the only one holding no Kubernetes
credential at all). See [`docs/security.md`](docs/security.md) for exactly
what each container can and cannot reach.

## The state machine

Eight phases:

| Phase | Meaning | Holds a slot? |
|---|---|---|
| `Pending` | Resources not all ready yet. | No |
| `Ready` | Resources ready, waiting on a scheduling slot. | No |
| `Restoring` | Slot granted, pod starting (and, on a wake, restoring from a snapshot). | Yes |
| `Running` | Pod is `Running` and `Ready`. | Yes |
| `Freezing` | Snapshotting the workspace/agent-home, then deleting the pod. | Yes |
| `Waiting` | Frozen; waiting for a declared probe to clear (or suspend to lift). | No |
| `Done` | Terminal: succeeded. | No |
| `Failed` | Terminal: failed or timed out. | No |

```
Pending --resources ready--> Ready --slot granted--> Restoring --pod Running+Ready--> Running
Running --wait declared | suspend--> Freezing --snapshot done + pod gone--> Waiting
Waiting --probe satisfied | suspend cleared--> Ready
{Restoring,Running} --agent /v1/done | pod exit--> Done | Failed
{any non-terminal} --timeout--> Failed
{Done,Failed} --never snapshotted & pod alive--> Freezing (archive detour) --> Waiting --> back to terminal
```

Terminal phases (`Done`/`Failed`) are **sticky**, with exactly one
exception: the archive detour above, for an environment that reaches a
terminal phase with a live pod but no snapshot yet — it freezes once more
so the transcript is captured before the pod goes away, then proceeds back
to its terminal phase. Timeouts are checked **after** agent/pod completion
(so a completion landing right on the timeout boundary wins) and **before**
the class-resolved gate and the rest of the per-phase logic.

Nine conditions are ever set: the six lifecycle conditions `Scheduled`,
`PodReady`, `Frozen`, `WaitSatisfied`, `Archived`, `Ready` (always written
in that order), plus three non-lifecycle conditions the reconciler appends
and never prunes: `EngineSecurityRelaxed`, `NetworkPosture`,
`CNIEnforcement`. Every reason any of these nine can carry, what it means,
and what to do about it is the full table in
[`docs/operations.md`](docs/operations.md#condition-reasons).

## Quickstart

Run these from the **repository root**. You need `docker`, `kind`,
`kubectl` and `helm` on `PATH`. Total time ~8 minutes, most of it building
the operator image.

**No operator image has been published yet** (see
`.github/workflows/release.yml`), so the quickstart builds one from this
checkout and loads it into kind. Once a release is cut, `helm install …
oci://ghcr.io/psenna/charts/ai-sandbox-operator` replaces steps 2 and 4.

The agent image here is **`operator/test/e2e/agent` — a stand-in**, not a
real coding agent. Its `claude` binary interprets `SCRIPT:` directives so
the quickstart needs no model endpoint, no git-proxy and no credentials.
For a real agent against a real repository, see
[`docs/worked-example.md`](docs/worked-example.md).

**Every command in this section is executed verbatim in CI** by
`operator/hack/quickstart-check.sh` (job `quickstart` in
`.github/workflows/docs.yml`), which extracts these fenced blocks from this
file and runs them. If you can read it here, CI ran it.

**1 — create a cluster**

```sh quickstart
kind create cluster --name ai-sandbox-quickstart --image kindest/node:v1.34.0 --wait 120s
kubectl config use-context kind-ai-sandbox-quickstart
kubectl cluster-info
```

**2 — build and load the images**

The MinIO image used in step 3 must already be present on the node —
pulled and loaded here, alongside the two images this repo builds, so the
rest of the quickstart never depends on the kind cluster having registry
egress.

```sh quickstart
docker build -t ai-sandbox-operator:quickstart operator
docker build -t ai-sandbox-quickstart-agent:v1 operator/test/e2e/agent
docker pull minio/minio:RELEASE.2025-09-07T16-13-09Z
kind load docker-image --name ai-sandbox-quickstart \
  ai-sandbox-operator:quickstart ai-sandbox-quickstart-agent:v1 \
  minio/minio:RELEASE.2025-09-07T16-13-09Z
```

**3 — an S3 backend (MinIO) and its credentials Secret**

> The operator resolves `storage.backend.s3.credentialsSecretRef` from
> **its own namespace** (`--class-secret-namespace`, defaulting to the
> release namespace), reading the **fixed** keys `accessKeyID` /
> `secretAccessKey`. `credentialsSecretRef.key` is ignored for the S3
> backend.

```sh quickstart
kubectl create namespace ai-sandbox-quickstart
kubectl create namespace ai-sandbox-operator-system

kubectl -n ai-sandbox-operator-system create secret generic sandbox-s3-credentials \
  --from-literal=accessKeyID=quickstart \
  --from-literal=secretAccessKey=quickstart123

kubectl -n ai-sandbox-quickstart apply -f - <<'EOF'
apiVersion: apps/v1
kind: Deployment
metadata: {name: minio, namespace: ai-sandbox-quickstart}
spec:
  replicas: 1
  selector: {matchLabels: {app: minio}}
  template:
    metadata: {labels: {app: minio}}
    spec:
      containers:
        - name: minio
          image: minio/minio:RELEASE.2025-09-07T16-13-09Z
          imagePullPolicy: IfNotPresent
          args: ["server", "/data"]
          env:
            - {name: MINIO_ROOT_USER, value: quickstart}
            - {name: MINIO_ROOT_PASSWORD, value: quickstart123}
          ports: [{containerPort: 9000}]
          readinessProbe:
            httpGet: {path: /minio/health/ready, port: 9000}
            periodSeconds: 2
            failureThreshold: 60
          volumeMounts: [{name: data, mountPath: /data}]
      volumes: [{name: data, emptyDir: {}}]
---
apiVersion: v1
kind: Service
metadata: {name: minio, namespace: ai-sandbox-quickstart}
spec:
  selector: {app: minio}
  ports: [{name: api, port: 9000, targetPort: 9000}]
---
apiVersion: batch/v1
kind: Job
metadata: {name: minio-bootstrap, namespace: ai-sandbox-quickstart}
spec:
  backoffLimit: 6
  template:
    spec:
      restartPolicy: OnFailure
      containers:
        - name: bootstrap
          image: minio/minio:RELEASE.2025-09-07T16-13-09Z
          imagePullPolicy: IfNotPresent
          command: ["/bin/sh", "-c"]
          args:
            - |
              set -e
              until mc alias set local http://minio:9000 quickstart quickstart123; do sleep 2; done
              mc mb --ignore-existing local/sandbox-archives
              echo BOOTSTRAP_OK
EOF

kubectl -n ai-sandbox-quickstart rollout status deployment/minio --timeout=300s
kubectl -n ai-sandbox-quickstart wait --for=condition=complete job/minio-bootstrap --timeout=180s
```

**4 — install the operator**

```sh quickstart
helm install ai-sandbox-operator operator/deploy/helm/ai-sandbox-operator \
  --namespace ai-sandbox-operator-system \
  --set image.repository=ai-sandbox-operator \
  --set image.tag=quickstart \
  --set image.pullPolicy=IfNotPresent \
  --set sidecarImage=ai-sandbox-operator:quickstart \
  --set leaderElection.enabled=false \
  --wait --timeout 300s

kubectl -n ai-sandbox-operator-system rollout status deployment/ai-sandbox-operator --timeout=300s
helm test ai-sandbox-operator -n ai-sandbox-operator-system --logs
```

**5 — a `SandboxClass`**

> **`engine.type: none` is not optional here.** The CRD default is
> `rootless-podman`, which is **not implemented** — leave it at the default
> and no pod is ever created. See [`docs/engines.md`](docs/engines.md).
> `network.isolation: Open` keeps the quickstart to one moving part. The
> CRD default is `Restricted`; see [`docs/security.md`](docs/security.md)
> for exactly what it allows and what it does not.

```sh quickstart
kubectl apply -f - <<'EOF'
apiVersion: sandbox.psenna.dev/v1alpha1
kind: SandboxClass
metadata: {name: quickstart}
spec:
  agent:
    image: ai-sandbox-quickstart-agent:v1
    resources:
      requests: {cpu: 50m, memory: 64Mi}
  engine:
    type: none
  storage:
    workspace: {size: 128Mi}
    backend:
      type: s3
      s3:
        endpoint: http://minio.ai-sandbox-quickstart.svc.cluster.local:9000
        bucket: sandbox-archives
        credentialsSecretRef: {name: sandbox-s3-credentials}
  network:
    isolation: Open
  timeouts: {running: 1h, waiting: 1h, total: 2h}
EOF
kubectl get sandboxclass quickstart -o wide
```

**6 — a `SandboxEnvironment`**

```sh quickstart
kubectl apply -f - <<'EOF'
apiVersion: sandbox.psenna.dev/v1alpha1
kind: SandboxEnvironment
metadata: {name: hello, namespace: ai-sandbox-quickstart}
spec:
  classRef: {name: quickstart}
  repo: psenna/e2e-fixture
  task:
    prompt: |
      SCRIPT:echo hello from the sandbox
      SCRIPT:write REPORT.md the agent wrote this
      SCRIPT:sandbox-done Succeeded wrote REPORT.md
      SCRIPT:exit 0
EOF
```

**7 — watch it complete**

```sh quickstart
kubectl -n ai-sandbox-quickstart wait sandboxenvironment/hello \
  --for=jsonpath='{.status.phase}'=Done --timeout=420s
kubectl -n ai-sandbox-quickstart get sandboxenvironment hello -o wide
```

**8 — read the result**

```sh quickstart
kubectl -n ai-sandbox-quickstart get sandboxenvironment hello \
  -o jsonpath='{range .status.conditions[*]}{.type}{"\t"}{.status}{"\t"}{.reason}{"\n"}{end}'

kubectl -n ai-sandbox-quickstart get sandboxenvironment hello \
  -o jsonpath='{.status.agentResult.outcome}{"\n"}{.status.archive.uri}{"\n"}'
```

Expect nine conditions, roughly:

```console
Scheduled       True    Terminal
PodReady        False   PodSucceeded
Frozen          False   NotFrozen
WaitSatisfied   False   NoWaitDeclared
Archived        True    ArchiveWritten
Ready           False   Succeeded
EngineSecurityRelaxed  False   NoRelaxation
NetworkPosture         False   Open
CNIEnforcement         Unknown Unconfirmed
```

If any reason above surprises you, [`docs/operations.md`](docs/operations.md#condition-reasons)
has the full table of every reason the operator can emit, what it means,
and what to do.

**9 — prove the archive really landed**

```sh quickstart
kubectl -n ai-sandbox-quickstart run mc --rm -i --restart=Never \
  --image=minio/minio:RELEASE.2025-09-07T16-13-09Z --image-pull-policy=IfNotPresent \
  --command -- sh -c \
  'mc alias set q http://minio:9000 quickstart quickstart123 >/dev/null && mc ls --recursive q/sandbox-archives' \
  | tee /dev/stderr | grep -q 'archive/run.json'
```

**10 — clean up**

```sh quickstart
kind delete cluster --name ai-sandbox-quickstart
```

## Where to go next

- [`docs/engines.md`](docs/engines.md) — what `engine.type: none` gives you
  today, and why `rootless-podman` (the CRD default) does not work yet.
- [`docs/security.md`](docs/security.md) — the trust boundary: what the
  agent can and cannot reach, what `Restricted` isolation actually
  protects, and the residual risks stated plainly.
- [`docs/operations.md`](docs/operations.md) — installing, sizing slots,
  storage/retention, metrics, and troubleshooting by condition reason.
- [`docs/worked-example.md`](docs/worked-example.md) — a real agent against
  a real repository, issue to merged PR, honestly labelled as not
  CI-executed.
- [`docs/crd-reference.md`](docs/crd-reference.md) — the generated field
  reference for both CRDs.

## Layout

```
operator/
├── cmd/main.go              entrypoint: config, manager, signal handling
├── cmd/sandboxctl/          the sidecar/init-container binary entrypoint
├── api/v1alpha1/            SandboxClass / SandboxEnvironment API types
├── internal/config/         flag/env configuration surface
├── internal/operator/       manager construction (scheme, options, probes)
├── internal/lifecycle/      pure SandboxEnvironment phase state machine (no controller-runtime import)
├── internal/render/         pure child-object renderer: PVC/ServiceAccount/Role/RoleBinding/ConfigMap/Secret/Pod/NetworkPolicy (no controller-runtime import)
├── internal/storage/        S3/PVC snapshot+archive backends, path layout, manifest (no controller-runtime import, no Secret reads)
├── internal/controller/     the reconciler: Observe -> lifecycle.Next -> actions -> status write
├── internal/sandboxctl/     the sidecar's own logic: control API, freeze/restore/archive
├── internal/metrics/        the Prometheus metric catalogue
├── internal/crdref/         the CRD-reference markdown generator (library)
├── internal/docs/           doc-completeness enforcement tests (this issue)
├── internal/apitest/        envtest-backed API validation/round-trip tests
├── config/                  kustomize bases (CRDs, manager Deployment, RBAC)
├── config/samples/          example SandboxClass/SandboxEnvironment YAML
├── deploy/helm/              the supported Helm chart (#34)
├── docs/                    this directory: engines/security/operations/development/worked-example/crd-reference
├── hack/                    boilerplate header, smoke test, tool/e2e/quickstart scripts
├── hack/tools/              separate Go module that builds controller-gen
├── hack/crdref/              `main` wrapper for the CRD-reference generator
├── test/e2e/                kind-based end-to-end suite (separate Go module)
├── Dockerfile               multi-stage, cross-compiled, distroless
└── Makefile                 build/test/lint entrypoints (see docs/development.md)
```

## Status: what is and is not implemented

| Feature | Status |
|---|---|
| Phase state machine, conditions, events, metrics, logging | shipped |
| Child resources, agent pod, `sandboxctl` sidecar | shipped |
| Slot scheduler, queue position | shipped |
| Freeze / wake / restore, warm cache + TTL GC | shipped, **S3 backend only** |
| Wait probes (`GitProxyCheck`/`HTTPGet`/`S3ObjectExists`/`NotBefore`) | **shipped** (#30) — the operator evaluates them and clears the wait |
| Terminal archive, finalizer, retention GC | shipped, **S3 backend only** |
| NetworkPolicy + `Restricted` isolation + CNI probe | shipped |
| Helm chart | shipped (#34) |
| `engine.type: none` | shipped |
| `engine.type: rootless-podman` | **not implemented** (#24) — fails closed at render |
| Kubernetes-native workload broker | **not started** (#25, post-v1) |
| Published operator image / OCI chart | **not published yet** |

## Design notes

The sections below are the design record for each issue that shaped this
directory, kept for reference — they are not a getting-started guide (see
[Quickstart](#quickstart) and [Where to go next](#where-to-go-next) for
that). This directory started as a **scaffold only** (issue #16): a Go
module, kubebuilder v4 project layout, CI, and a manager that starts,
serves health probes, and exits cleanly. [Issue #17](https://github.com/psenna/ai-sandbox/issues/17)
added the `sandbox.psenna.dev/v1alpha1` API types (`SandboxClass`,
`SandboxEnvironment`), their CRDs, and envtest-backed validation/round-trip
tests. [Issue #18](https://github.com/psenna/ai-sandbox/issues/18) added the
`SandboxEnvironment` phase state machine as a pure function
(`internal/lifecycle`), the reconciler that drives it against a real cluster
(`internal/controller`), and the RBAC generated from its
`+kubebuilder:rbac` markers. [Issue #19](https://github.com/psenna/ai-sandbox/issues/19)
made the reconciler's `Observe` seam real (`observeCluster`, replacing the
earlier stub) for everything an environment needs besides its pod: a pure,
deterministic renderer (`internal/render`) produces the workspace PVC, the
sidecar's ServiceAccount/Role/RoleBinding, the agent's environment Secret,
and the entrypoint ConfigMap, applied via server-side apply and observed for
readiness. [Issue #27](https://github.com/psenna/ai-sandbox/issues/27)
added the agent pod (`internal/render.RenderPod`) and the `sandboxctl`
sidecar (`cmd/sandboxctl`, `internal/sandboxctl`): an always-present
native-sidecar init container (KEP-753) that holds the environment's
Kubernetes credential and exposes a localhost-only control API (`/v1/wait`,
`/v1/done`, `/v1/progress`, `/v1/status`) so the agent container -- which
holds no credential at all -- can declare a wait condition or report its
result. See `claude-code/use-sandbox/SKILL.md` (repo root) for the
agent-facing contract.

### Child resources (#19)

For a `SandboxEnvironment` named `<env>`, `internal/render.Render` produces
six objects, all labeled with the standard `app.kubernetes.io/*` set plus
`sandbox.psenna.dev/environment=<truncated-env>` and
`sandbox.psenna.dev/class=<truncated-class>`, and owned by a single
controller `ownerReference` back to the environment (so deleting the CR
garbage-collects the lot):

| kind | name | purpose |
|---|---|---|
| `ServiceAccount` | `<env>-agent` | the sidecar's identity; `automountServiceAccountToken: false` |
| `Role` + `RoleBinding` | `<env>-sidecar` | `get` on the environment, `get`/`patch` on its `status`, `resourceNames`-restricted to the environment's own name -- nothing else |
| `PersistentVolumeClaim` | `<env>-workspace` | the warm workspace cache, sized from `SandboxClass.spec.storage.workspace`, retained across freeze/wake |
| `ConfigMap` | `<env>-config` | `sandbox.json` (resolved run configuration) and `task.md` (the agent's task instructions) |
| `Secret` | `<env>-env` | the agent's process environment (git-proxy bearer, service URLs, model tier mapping, `CLAUDE_CONFIG_DIR`, ...) |

Names longer than 63 characters are truncated and suffixed with an 8-hex-char
SHA-256 hash of the full environment name (see `internal/render/names.go`);
label values use the same scheme. The class-referenced credentials (e.g.
`services.gitProxy.tokenSecretRef`) are read from a single operator-level
namespace (`--class-secret-namespace` / `SANDBOX_OPERATOR_CLASS_SECRET_NAMESPACE`,
falling back to the downward-API `POD_NAMESPACE`, then
`ai-sandbox-operator-system`) and projected into the environment's own
`Secret` -- the operator never mints or logs a credential, and Secret reads
always bypass the client cache (see `internal/operator/manager.go`'s
`Client.Cache.DisableFor`).

The `sandboxctl` sidecar container is rendered from `--sidecar-image` /
`SANDBOX_OPERATOR_SIDECAR_IMAGE` (default `ghcr.io/psenna/ai-sandbox-operator:dev`),
**not** from the SandboxClass -- it is operator machinery, versioned with the
operator, never with the workload. It must be kept in sync with whatever
image tag the operator Deployment itself runs: `operator/Dockerfile` builds
both `/manager` and `/sandboxctl` into the same image, so `--sidecar-image`
should simply repeat the operator's own image reference. The [Helm
chart](deploy/helm/ai-sandbox-operator/README.md) (#34) does this
automatically -- `sidecarImage: ""` resolves to the exact same ref as
`image.*` via the chart's `imageRef`/`sidecarImageRef` helpers, so there is
nothing to keep in sync by hand. The raw `config/default` kustomize base has
no such helper: its `images:` transform and
`test/e2e/manifests/operator/kustomization.yaml`'s explicit
`--sidecar-image` argument must still be kept in lockstep manually when
deploying that way instead of via the chart.

Rendering (`internal/render`) is covered by golden-file tests under
`internal/render/testdata/`; run `go test ./internal/render/... -update` to
regenerate them after an intentional rendering change (a rendered `Secret`'s
values are redacted to a `sha256:`-prefixed fingerprint before being written
to a golden file, since `testdata/*.yaml` is not covered by git-proxy's
`secret_scan` ignore-paths the way `*_test.go` is).

### `internal/storage` (#26)

`internal/storage` implements the two snapshot/archive storage backends a
`SandboxClass` can select (`storage.backend.type`: `s3` or `pvc`), the
object-key path layout both backends share, and the snapshot manifest
format. See `internal/storage/doc.go` for the full design record (purity
contract, credential-resolution boundary, the no-logging rule, the
NotFound-vs-Unreachable error taxonomy, and known gaps between this design
and the current CRD field set). In short:

- **Path layout**: every object lives under
  `<prefix>/<clusterID>/<namespace>/<envName>/<envUID>/`, with `snapshots/
  <seq>-<timestamp>/{workspace,agent-home}.tar.zst` + `manifest.json` per
  snapshot, an `archive/` directory for the terminal archive, and a
  `latest.json` pointer at the root. `EnvUID` is mandatory: it's what keeps
  a deleted-and-recreated environment (same cluster/namespace/name) from
  colliding with its predecessor's snapshots.
- **Manifest**: `manifest.json` records each snapshot's files (name, size,
  sha256), source environment, a `specHash` of the class+environment specs
  that produced it, and the agent image — enough to verify a restore is
  both intact and compatible with the environment it's restored into.
- **Two backends, one conformance suite**: `S3Backend` (`s3.go`) and
  `FSBackend` (`fs.go`, for a mounted PVC) both satisfy the `Backend`
  interface and are exercised by the same `conformance_test.go` suite, so
  "works on S3" and "works on a PVC" mean the same thing.
- **Running the s3 leg locally**: the s3 half of `conformance_test.go` is
  gated on the `SANDBOX_TEST_MINIO_ENDPOINT` env var (with `t.Skip`, not a
  build tag — the file always compiles/vets/lints regardless). Run
  `make storage-conformance` to start a real MinIO container, run both
  backends' full conformance suite against it, and tear the container down
  again afterward (even on failure); `make minio-up` / `make test-minio` /
  `make minio-down` are the same three steps split apart for iterating
  locally without repeatedly restarting MinIO.

### Wake/restore (#29)

Freeze (#28) snapshots `/workspace` and the agent home into `internal/storage`
and destroys the pod. Wake (#29) restores them into a fresh pod. The wake is
implemented as a **one-shot init container** — the same `sandboxctl` binary as
the sidecar, `restore` subcommand — rendered **last** among init containers,
as a *plain* (non-restartable) init container under `restartPolicy: Never`.
Both choices are load-bearing: Kubernetes starts no regular container (the
agent) until every init container has succeeded, so "never start an agent on a
partially restored workspace" is enforced by the kubelet itself; and a
non-zero restore exit fails the whole pod, which is what makes a corrupted
snapshot *loudly* fail the environment (`Failed`,
`RestoreVerificationFailed`) rather than silently degrade. Folding restore
into the sidecar's own startup was rejected: the sidecar's `startupProbe`
has a 30s budget and `restartPolicy: Always` containers can never fail a pod.
The restore container is rendered only when the class's storage backend is
`s3` (the `pvc` backend has no restore path; see `internal/storage/doc.go`'s
gap G5).

What a wake restores, and what it cannot: the workspace (warm or cold, see
below) and the agent home (always cold from S3) are restored byte-for-byte,
checksum-verified against the snapshot manifest *before* the agent starts.
Nothing else comes back — the image cache is cold, containers the agent
started are gone, `/tmp` and installed packages are gone. The four
`.sandbox/` markers the agent can read to reconcile against reality are:

| file | author | says |
|---|---|---|
| `.sandbox/RESUME.md` | freeze | prose: what the freeze destroyed / preserved |
| `.sandbox/last-freeze.json` | freeze | machine form of the same |
| `.sandbox/last-wake.json` | restore | which snapshot was restored, warm or cold, bytes downloaded, and whether the snapshot's agent image / spec hash differ from the environment it was restored into (`SpecChanged`) |
| `.sandbox/warm-cache.json` | freeze/restore | internal warm-cache marker — informational |

**The warm-cache contract, and its honest trust boundary.** A frozen
environment's workspace PVC is retained (that is the "warm cache"). On wake,
if `.sandbox/warm-cache.json` in that PVC validates — the recorded `EnvUID`
matches, the manifest SHA matches the just-downloaded manifest, the freeze's
teardown sequence matches, and the recorded file list matches the manifest —
the workspace is used as-is ("warm"), nothing is downloaded, and the
`status.restoreAttempt.roots[]` row reports `Source: Warm` with zero bytes.
Any mismatch, any missing marker, or a missing PVC falls through to a **cold**
restore from S3. The marker is an *optimization hint, not an authority*: a
warm restore is only ever a verification shortcut over a restore that could
have happened cold; a stale or forged marker can cost a cold restore, never
an unverified one.

**The TTL GC (what it deletes, and what it never will).** A warm cache that
is never reclaimed would pin every frozen environment's snapshot forever, so
`internal/controller/warmcachegc.go` (a `manager.Runnable`, like
`SlotScheduler` — one leader-elected loop cluster-wide, `--warm-cache-gc-interval`,
default 30m) deletes the workspace PVC of an environment that is (a) exactly
`Waiting`, (b) not being deleted, (c) holding a **complete, verified
snapshot** in S3 (`status.snapshot` present with `Seq >= FreezeCount-1`, no
in-flight/failed `snapshotAttempt`), (d) on an **S3-backed** class, and (e)
past its class's `warmCacheTTL` (`spec.storage.warmCacheTTL`, default 30m;
`"0s"` disables GC for that class). Every condition is independently
required: a `Running` environment's PVC is never touched, and an environment
whose freeze failed is never touched — before the snapshot exists, the PVC
is the *only* copy of the agent's context, so deleting it would be data loss,
not reclamation. One honest second-order consequence of retention, worth
documenting: a retained RWO PVC on a `WaitForFirstConsumer` StorageClass
pins a woken environment to the node holding its PV; the GC's reclamation
is what un-pins it.

**The scoping statement.** #29 makes the wake *correct when it happens*; it
does not by itself make environments wake on a schedule. Waking still
requires `status.waitFor` to clear — either the operator's own wait-probe
evaluator (#30, shipped — see below) satisfying a declared probe, or a
human/controller clearing `status.waitFor` directly via the admin client.

**Wait probes are implemented (#30).** `internal/operator/controllers.go`
wires a real `controller.NewProbeEvaluator()` into the reconciler; all four
probe types (`GitProxyCheck`, `HTTPGet`, `S3ObjectExists`, `NotBefore`) are
evaluated on a backoff schedule and clear the wait themselves once
satisfied — see [`docs/operations.md`](docs/operations.md#stuck-in-waiting)
for the parameter reference and troubleshooting.

The context-resumption property this all depends on — that a Claude Code
session transcript, keyed by working directory, survives a freeze → wake
cycle — was verified **before** the feature was built by a real experiment
against the stack's own agent image and model endpoint (no Kubernetes):
see [the spike doc](docs/spike-context-resumption.md) and its runnable
script at `operator/spike/context-resumption/run.sh`.

### Destroy / terminal archive (#32)

Every `SandboxEnvironment` carries `sandbox.psenna.dev/environment-archiver`
(`v1alpha1.FinalizerArchiveOnDelete`), added on its first non-deleting
reconcile. Deleting the object sets `DeletionTimestamp` but does **not**
remove it from the API until the controller has written a terminal archive
(or a documented reason says archiving was never possible, or is already
done) — a `kubectl delete` on a still-`Running` environment does not lose
the agent's transcript.

**What `reconcileDelete` does, in order:** (1) the escape hatch, below; (2)
if `status.archive` is already set — including by the *normal* completion
path, since every environment gets archived on reaching `Done`/`Failed`, not
only on deletion — the finalizer is removed with nothing left to do; (3) an
unresolvable class, or a class whose `storage.backend.type` isn't `s3`,
removes the finalizer without archiving (documented limitation, matching
freeze's own S3-only restriction — see below); (4) a backend that can't even
be constructed (e.g. a missing/malformed credentials Secret) holds the
finalizer and retries; (5) if the agent home has never been snapshotted and
a live pod still exists, the environment is frozen first — exactly the
freeze-before-archive detour a normally-completing environment goes through
(`internal/lifecycle/next.go`'s `terminal()`) — so the transcript is captured
before the pod goes away; (6) otherwise the one-shot `sandboxctl archive`
Job (`internal/render/archivejob.go`, `internal/controller/archive.go`) is
created or re-applied. The archive Job mounts no workspace/agent-home PVC —
it assembles `run.json` from `status.*` and copies `context.tar.zst`
backend-to-backend from the most recent freeze snapshot — only the
snapshot-credentials Secret and the sidecar token volume, so the Job's own
`sandboxctl` process can authenticate and patch `status.archive`.

**The escape hatch.** Set the annotation
`sandbox.psenna.dev/remove-finalizer: "true"` on a `SandboxEnvironment` to
force the finalizer off *without* archiving — for a genuinely unrecoverable
situation (credentials rotated away, the bucket itself deleted) that would
otherwise wedge deletion forever. This is a deliberate, documented
data-loss action: the controller emits a `Warning` Event
(`ArchiveSkippedByEscapeHatch`) every time it's honored, so it's never
silent.

**The archive key layout**, under the same `<prefix>/<clusterID>/<namespace>/
<envName>/<envUID>/` root every snapshot uses (see `internal/storage`
above): `archive/run.json` (the full run record — phase history, snapshot
list, git state, timing — see `internal/storage/runrecord.go`) and
`archive/context.tar.zst` (the most recent freeze's `agent-home.tar.zst`,
copied over; **omitted, not failed,** for a never-frozen run or a
workspace-only recovery-Job snapshot — `run.json`'s `context.reason` names
which). `status.archive.uri` / `status.archiveURI` point at the `archive/`
prefix; `status.archive.runJSONSHA256` lets an auditor verify a downloaded
`run.json` byte-for-byte.

**Retention GC** (`internal/controller/retentiongc.go`, a `manager.Runnable`
like `WarmCacheGC`/`SlotScheduler` — one leader-elected loop cluster-wide)
runs two independent sweeps per S3-backed class every
`--retention-gc-interval` (default 30m): **retention** deletes a live
environment's entire storage root (snapshots *and* archive) once
`status.archive.finishedAt` is older than `--retention-ttl` (default 168h;
`0` disables this sweep only); **orphan cleanup** deletes any storage root
whose `EnvUID` belongs to no currently-live environment, regardless of TTL —
the mechanism that makes deleting an environment and recreating one with the
*same name* safe (the new object gets a fresh UID and therefore a disjoint
root; the old UID's root is reclaimed here, not silently inherited or
colliding). Under `--watch-namespace`, orphan cleanup only considers roots in
that namespace — "currently-live" is only knowable as far as the operator's
own watch reaches. `--retention-dry-run` logs what either sweep would delete
without deleting anything.

**The PVC-backend limitation.** Archiving, like freeze, is S3-only: a class
whose `storage.backend.type` is `pvc` never gets a terminal archive — its
environments' finalizers are removed on deletion with nothing written. This
is the same restriction `internal/storage/doc.go`'s gap analysis already
documents for freeze/wake; it isn't new here.

**The never-frozen-context limitation.** An environment deleted (or that
fails) before its agent container ever starts — `Pending`/`Ready` still
queued, or a pod stuck `Pending` on an image that will not pull — has no
`agent-home.tar.zst` anywhere to copy from, and no running sidecar that could
produce one, so it archives directly instead of taking the freeze detour
(that is what `PodAliveForArchive` requires a `Running` pod for: detouring
into `Freezing` with nothing able to complete the snapshot would hold the
finalizer forever). Its `archive/run.json`
still gets written (a complete record of what *did* happen: spec, phase
history, timing), but `context.present` is `false` and `context.reason`
reads `"no agent home snapshot"`. This is not a bug: there was never a
session transcript to lose.

### Observability (#33)

The manager's `/metrics` listener (`--metrics-bind-address`, default
`:8080`) already existed and was already unauthenticated before #33 — #33 is
the first issue to put anything *on* it. `internal/metrics` registers every
series into `sigs.k8s.io/controller-runtime/pkg/metrics`'s own `Registry`,
so this rides the existing listener: no new server, no new flag, no new
auth. `config/manager/metrics_service.yaml` exposes it as a stable
`ClusterIP` Service; `config/prometheus/monitor.yaml` is a
`monitoring.coreos.com/v1` `ServiceMonitor` scraping it, referenced (commented
out) from `config/default/kustomization.yaml` since it requires the
Prometheus Operator's CRDs, which the e2e kind cluster does not install. The
[Helm chart](deploy/helm/ai-sandbox-operator/README.md) (#34) ships the
chart-native equivalent as `metrics.serviceMonitor.enabled` (default
`false`, since it also needs those CRDs) -- see that chart's README for the
"why a plain `ClusterIP` `Service` is correct even with multiple replicas"
note. The full metric catalogue, Events, and logging verbosity convention
are documented in [`docs/operations.md`](docs/operations.md#reading-the-metrics)
rather than duplicated here.

RBAC needed a new marker for Events: `mgr.GetEventRecorder` sinks through
controller-runtime's `events.EventBroadcaster`, which talks to the
`events.k8s.io/v1` API, not the legacy core `""`-group `events` resource the
pre-#33 `ClusterRole` already granted — both markers are present now
(`make manifests` merged them into one `role.yaml` rule with two
`apiGroups`).

## Development

Building, testing, and this repo's toolchain constraints (DependaProxy
pinning, `hack/tools`, envtest, the full Make target reference) are in
[`docs/development.md`](docs/development.md).
