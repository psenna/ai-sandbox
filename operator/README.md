# ai-sandbox operator

Kubernetes operator for ai-sandbox. See [issue #15](https://github.com/psenna/ai-sandbox/issues/15)
for the broader design context.

**To install the operator**, use the [Helm chart](deploy/helm/ai-sandbox-operator/README.md)
at `deploy/helm/ai-sandbox-operator` (#34) -- it is the supported install
path (values reference, RBAC table, guarded misconfiguration errors,
`crds/`, `helm test`). `config/default` (kustomize) remains for local/e2e
development (`make kustomize-build`, `hack/e2e-up.sh`) but is not a
packaged, versioned install artifact the way the chart is.

This directory started as a **scaffold only** (issue #16): a Go module,
kubebuilder v4 project layout, CI, and a manager that starts, serves health
probes, and exits cleanly. [Issue #17](https://github.com/psenna/ai-sandbox/issues/17)
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
readiness. [Issue #26](https://github.com/psenna/ai-sandbox/issues/26) added
`internal/storage`: the two snapshot/archive storage backends
(`s3`/`aws-sdk-go-v2` and `pvc`/mounted-filesystem), the object-key path
layout, and the snapshot manifest format -- a pure Go library, not yet
wired into the reconciler. [Issue #27](https://github.com/psenna/ai-sandbox/issues/27)
added the agent pod (`internal/render.RenderPod`, `#21/#24`'s engine seam
still stubbed) and the `sandboxctl` sidecar (`cmd/sandboxctl`,
`internal/sandboxctl`): an always-present native-sidecar init container
(KEP-753) that holds the environment's Kubernetes credential and exposes a
localhost-only control API (`/v1/wait`, `/v1/done`, `/v1/progress`,
`/v1/status`) so the agent container -- which holds no credential at all --
can declare a wait condition or report its result. See
`claude-code/use-sandbox/SKILL.md` for the agent-facing contract. Still
stubbed/not-implemented: wait probe *evaluation* (#30 -- #27/#29 only
validate and record a declared probe; nothing decides when one is satisfied
yet, so a declared wait does not clear itself), and the terminal archive
(#32) -- each lands incrementally, replacing one `notImplemented` action and
one piece of `observeCluster` without touching the state machine or the
reconcile loop itself. Freeze (#28) and wake/restore (#29) are both
implemented (see "Wake/restore (#29)" below).

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

## Layout

```
operator/
├── cmd/main.go              entrypoint: config, manager, signal handling
├── api/v1alpha1/            SandboxClass / SandboxEnvironment API types
├── internal/config/         flag/env configuration surface
├── internal/operator/       manager construction (scheme, options, probes)
├── internal/lifecycle/      pure SandboxEnvironment phase state machine (no controller-runtime import)
├── internal/render/         pure child-object renderer: PVC/ServiceAccount/Role/RoleBinding/ConfigMap/Secret (no controller-runtime import)
├── internal/storage/        S3/PVC snapshot+archive backends, path layout, manifest (no controller-runtime import, no Secret reads)
├── internal/controller/     the reconciler: Observe -> lifecycle.Next -> actions -> status write
├── internal/apitest/        envtest-backed API validation/round-trip tests
├── config/                  kustomize bases (CRDs, manager Deployment, RBAC)
├── config/samples/          example SandboxClass/SandboxEnvironment YAML
├── hack/                    boilerplate header, smoke test, tool scripts
├── hack/tools/              separate Go module that builds controller-gen
├── Dockerfile               multi-stage, cross-compiled, distroless
└── Makefile                 build/test/lint entrypoints (see below)
```

## Make targets

All Go/make commands run inside a `golang:1.25` container against the
sandbox's Docker-in-Docker daemon — there is no Go toolchain or `make` on
the agent host. Set `IN_CONTAINER=1` when `make` itself already runs inside
a container with Go installed (this is what CI and the golang container do);
leave it unset (default `0`) to have the Makefile wrap every command in its
own `docker run` against `$DOCKER_HOST`.

| target | purpose |
|---|---|
| `build` | compile `bin/manager` |
| `test` | `go test -race` with coverage |
| `vet` | `go vet ./...` |
| `fmt` / `fmt-check` | `gofmt -w` / drift check |
| `lint` | `golangci-lint run` |
| `vuln` | `govulncheck` |
| `tidy` | `go mod tidy` |
| `manifests` | CRD manifests (`config/crd/bases/*.yaml`) and RBAC (`config/rbac/role.yaml`) via `controller-gen`, built from `hack/tools` |
| `generate` | deepcopy generation (`api/v1alpha1/zz_generated.deepcopy.go`) via `controller-gen` |
| `envtest-assets` | download and checksum-verify real kube-apiserver/etcd/kubectl binaries for envtest |
| `test-envtest` | `go test -race` against `internal/apitest` and `internal/controller`, using the envtest binaries above |
| `kustomize-build` | render `config/default` |
| `cross` | cross-compile linux/amd64 and linux/arm64 |
| `docker-build` | build the operator image |
| `smoke` | build then run `hack/smoke.sh` — starts the manager outside a cluster, checks `/healthz` and `/readyz`, and confirms clean shutdown on SIGTERM |
| `clean` | remove build artifacts |
| `minio-up` / `minio-down` | start/stop a real MinIO container (on a dedicated docker network) for `internal/storage`'s s3 conformance leg |
| `test-minio` | `go test -race` against `internal/storage` only, with `SANDBOX_TEST_MINIO_*` pointed at `minio-up`'s container |
| `storage-conformance` | `minio-up` → `test-minio` → `minio-down`, tearing down even on failure (same shape as `e2e`) |

## Dependency versions

Go module versions in `go.mod` are pinned rather than left to `go mod tidy`
to pick freely: dependencies are fetched through DependaProxy
(`http://dependaproxy:8080/goproxy`), which enforces a minimum publication
age and denies known-CVE-affected versions. The pinned versions here were
confirmed to pass both gates at the time this scaffold was written.

A few versions differ from the initial plan because the plan's pins do not
build together, or a transitive dependency was denied by DependaProxy's
CVE gate:

- `sigs.k8s.io/controller-runtime` is `v0.23.3`, not `v0.24.1`: `v0.24.x`
  declares `go 1.26.0`, which the `golang:1.25` toolchain in use here
  cannot satisfy. `v0.23.3` is the highest release that declares `go
  1.25.0`.
- `k8s.io/api`, `k8s.io/apimachinery`, `k8s.io/client-go` are `v0.35.0`,
  not `v0.36.3`, to match the `k8s.io/*` versions controller-runtime
  `v0.23.3` itself requires (`v0.36.x` also declares `go 1.26.0`).
- `golang.org/x/net`, `golang.org/x/sys`, `golang.org/x/term`,
  `golang.org/x/text` and `github.com/klauspost/compress` are pinned as
  explicit indirect requires, each bumped to the lowest version that
  clears DependaProxy's CVE gate (the versions MVS would otherwise select
  from controller-runtime/client-go's own requirements are denied).
  `go mod tidy` needed `-e` to complete: `golang.org/x/mod` is denied by
  DependaProxy's CVE gate (`GO-2026-6179`/`GO-2026-6180`) at every
  released version, with no passing release available at the time of
  writing. `x/mod` only enters the graph as a *test* dependency of
  `sigs.k8s.io/controller-runtime`'s own test suite (via
  `ginkgo`→`golang.org/x/tools`→`golang.org/x/mod`) — this repository
  never imports it. `go build`, `go vet` and `go test ./...` all succeed
  fully offline against the committed `go.sum` (verified with
  `GOPROXY=off -mod=readonly`), confirming nothing on the real build path
  is missing.
- The same `golang.org/x/mod` gap blocks `go install` of `golangci-lint`,
  `govulncheck`, `controller-gen` and `kustomize` outright: each pulls in
  `golang.org/x/tools`, which requires `golang.org/x/mod`, and `go install
  pkg@version` builds against that tool's own pinned dependency graph, so
  this project's `go.mod` requires can't bump it out of the way the way
  they can for our own module. `make lint`, `make vuln` and
  `make kustomize-build` therefore still cannot succeed in this
  environment — not a code or Makefile defect, but an upstream/proxy gap
  (no `golang.org/x/mod` release currently clears DependaProxy's CVE
  gate). These are expected to start working again once a patched `x/mod`
  release is available through DependaProxy.
- `github.com/minio/minio-go/v7` — the "obvious" S3 client — is
  **unusable** here, not just dispreferred: it transitively imports
  `golang.org/x/crypto/argon2` via `pkg/encrypt`, and DependaProxy's CVE
  gate denies `golang.org/x/crypto` at every version tested
  (`GO-2026-5932`, an "unmaintained package" advisory with no fixed
  version). `internal/storage`'s S3 backend (#26) uses
  `github.com/aws/aws-sdk-go-v2/service/s3` instead, which resolves
  cleanly.
- The AWS SDK set must be added as a coherent group with one **pinned**
  command, not an unpinned `go get`/`go mod tidy`: an unpinned resolution
  re-resolves to `@latest`, which always 403s on DependaProxy's 7-day
  publication-age gate.
  ```
  go get github.com/aws/aws-sdk-go-v2/feature/s3/manager@v1.22.37 && go mod tidy -e
  ```
  This pulls in `aws-sdk-go-v2 v1.43.2`, `feature/s3/manager v1.22.37`,
  `service/s3 v1.106.2`, `credentials v1.19.32`, plus the internal
  protocol/endpoint packages those require — a coherent set that clears
  DependaProxy's gate. If a later `go mod tidy` re-resolves any of these
  to a newer, 403'd version, re-run the pinned `go get` for
  `feature/s3/manager@v1.22.37` again; do not loosen the pin to resolve a
  tidy conflict.
- `feature/s3/manager` itself carries an upstream deprecation notice
  ("superceded by feature/s3/transfermanager") — that replacement,
  `feature/s3/transfermanager`, is still pre-1.0 at the time of writing and
  was not used here; `manager.Uploader` is still the correct choice for a
  streaming multipart upload from a plain `io.Reader` (see
  `internal/storage/s3.go`'s `Put`). Its read-side counterpart,
  `manager.Downloader`, is deliberately NOT used — it requires an
  `io.WriterAt`, which would defeat streaming — `Get` uses plain
  `s3.GetObject` instead.
- `github.com/klauspost/compress` moved from a pinned **indirect** require
  (bumped only to clear controller-runtime/client-go's transitive CVE gate)
  to a **direct** require: `internal/storage/archive.go` now imports
  `github.com/klauspost/compress/zstd` directly, for the workspace/
  agent-home archive format. `go mod tidy` promotes it automatically; the
  version stays the same (`v1.18.7`).

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
does **not** make environments wake by themselves. Nothing evaluates wait
probes yet (#30): a declared wait does not clear itself — a human or a
controller must clear `status.waitFor` for a wake to be triggered at all, and
the e2e suite's wake specs all do exactly that via the admin client.

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
note (the metric catalogue itself is listed just below).

**The metric catalogue.** Every series is `sandbox_operator_<name>`
(`prometheus.BuildFQName("sandbox", "operator", name)`). No metric ever
carries an environment name, namespace, or UID as a label — a deliberate,
permanent cardinality bound — and every label value that could conceivably
carry unexpected data passes through a closed allowlist
(`internal/metrics`'s `sanitize`, mirroring `lifecycle.SanitizeReason`) that
collapses an unrecognized value to `"other"` rather than creating an
unbounded new series.

| name | type | labels | what it means |
|---|---|---|---|
| `sandbox_operator_environments` | Gauge | `phase` | environments currently in each lifecycle phase, in the watch scope — every phase is set on every pass (including 0), so an emptied phase drops to 0 rather than sticking |
| `sandbox_operator_slot_capacity` | Gauge | — | configured `--slot-capacity` |
| `sandbox_operator_slots_used` | Gauge | — | occupied scheduling slots (`scheduler.Occupies`) |
| `sandbox_operator_queue_depth` | Gauge | — | environments queued for a slot (`scheduler.IsCandidate`) |
| `sandbox_operator_queue_wait_seconds` | Histogram | — | time queued before a slot was granted |
| `sandbox_operator_freeze_duration_seconds` | Histogram | — | freeze-hook-to-published-snapshot duration |
| `sandbox_operator_wake_duration_seconds` | Histogram | `source` (`warm`/`cold`/`unknown`) | wake-restore duration, by warm-cache hit or cold download |
| `sandbox_operator_snapshot_size_bytes` | Histogram | — | published freeze snapshot size, uncompressed |
| `sandbox_operator_probe_evaluations_total` | Counter | `type`, `result` | wait-probe evaluations, by wait type and outcome (`satisfied`/`pending`/`error`/`skipped`) |
| `sandbox_operator_archives_total` | Counter | `result` (`succeeded`/`failed`/`skipped`) | terminal archive outcomes |
| `sandbox_operator_reconcile_errors_total` | Counter | `controller` | errors from a reconcile or control-loop pass, by controller/`Runnable` |
| `sandbox_operator_warm_cache_reclaimed_total` | Counter | — | workspace PVCs reclaimed by the warm-cache TTL GC |
| `sandbox_operator_retention_deleted_total` | Counter | `sweep` (`retention`/`orphan`) | storage roots deleted by retention GC |

**`archives_total`'s honest limitation.** The primary reconciler does not
`.Owns(&batchv1.Job{})` (see the child-resources note above), so an archive
Job's own failure is invisible except via the operator's dispatch-side
checks: `succeeded` is the `Archived` condition flipping `True`; `failed` is
`ensureArchiveJob` returning an error, one of its "not resolvable/
computable/renderable" soft-fail branches, or the escape-hatch path; `skipped`
is `archive()`'s no-op for a `nil`/non-`S3` class. `failed` therefore
increments **once per reconcile** an archive stays broken, not once per
distinct failure — the counter's rate, not a single crossing, is the signal
to alert on.

**The gauge/counter split.** `internal/controller/metricscollector.go`'s
`MetricsCollector` recomputes the four gauges above periodically
(`--metrics-collect-interval`, default 15s, valid 1s–5m) from one **cached**
List — the only `manager.Runnable` in this codebase whose
`NeedLeaderElection()` returns `false`, deliberately: every replica must keep
its own `/metrics` endpoint's gauges live, not just the leader's (see that
file's doc comment for why leader-electing it would be wrong, not merely
different). Every other metric is a point-in-time observation recorded
exactly where the thing happens — `slotscheduler.go`'s grant, the status-write
transition hook, `probe.go`'s `Evaluate`, `archive.go`, and each GC loop's
pass completion.

**Kubernetes Events.** The reconciler's status-write path
(`internal/controller/events.go`'s `observeTransition`, called from
`status.go`'s `writeStatus` right after a successful `Status().Update` —
never from `actions.go`, which re-issues every reconcile and is not
exactly-once) emits one `Event` per meaningful phase/condition transition:
`SlotGranted` (from `slotscheduler.go`'s own grant — the one Event this issue
emits outside the status-write hook, since granting is decided there, not in
`Reconcile`), `Starting`/`Waking` (cold start vs. resuming from a snapshot),
`Started`, `Freezing`/`Frozen`/`SnapshotFailed`, `WaitSatisfied`,
`Completed`/`Failed`, `Archived`. Every Event is also logged at `V(1)` for
free by controller-runtime's own recorder provider. RBAC needed a new
marker: `mgr.GetEventRecorder` sinks through controller-runtime's
`events.EventBroadcaster`, which talks to the `events.k8s.io/v1` API, not
the legacy core `""`-group `events` resource the pre-#33 `ClusterRole`
already granted — both markers are present now (`make manifests` merged them
into one `role.yaml` rule with two `apiGroups`).

**Structured logging.** `internal/controller/logkeys.go` states the
verbosity convention this whole package follows: `Error` is an actionable
failure; `V(0)`/`Info` is rare, significant, and irreversible (process
start/stop, permanent storage deletion — `retentiongc.go`'s own
deletion/dry-run lines are exactly that, and #33 leaves their verbosity
untouched); `V(1)` is per-reconcile/per-pass detail, including the one new
phase-transition line the status-write hook adds. #33 adds **no** new `V(0)`
output — new default-verbosity signal comes from Events, not logs. The
verbosity a shipped binary actually emits at was, before #33, silently stuck
at `V(0)` regardless of any `-v`-shaped flag: `cmd/main.go` hardcoded
`slog.NewJSONHandler(os.Stderr, nil)`, and logr's slog bridge maps `V(n)` to
`slog.Level(-n)` — `nil` options default to `slog.LevelInfo`, so every
`log.V(1)` line in the repository was unreachable in production. The new
`--log-verbosity` flag (`LOG_VERBOSITY` env, default 0, valid 0–4) now
actually reaches the handler.

### `make manifests` / `make generate`: `hack/tools`

Unlike `go install pkg@version`, a Go **module**'s own `go.mod` requires
*can* override a dependency's transitive pin via MVS (minimal version
selection). `hack/tools/` is a separate Go module (never referenced from
`operator/go.mod`, never part of `operator`'s own build, test, or
`govulncheck` scan) whose only purpose is building `controller-gen` from
source, with `golang.org/x/mod` explicitly bumped above every version
DependaProxy's CVE gate denies (`GO-2026-6179`/`GO-2026-6180`).

That explicit pin only resolves if the target version of `golang.org/x/mod`
is fetchable somewhere. DependaProxy denies it outright, so `hack/tools`'
build resolves it from a local, pre-fetched, DependaProxy-validated module
cache at `/workspace/gomodcache/cache/download` (this sandbox only) via a
`GOPROXY=file:///work/gomodcache/cache/download,http://dependaproxy:8080/goproxy`
chain — file cache first, DependaProxy second for everything else. The
`Makefile`'s `bin/controller-gen` target detects whether that cache exists
(`TOOLS_GOPROXY` in the `Makefile`) and only sets the file:// proxy when it
does; on a normal CI runner (no such path), it's unset and `hack/tools`
builds against the ordinary Go proxy chain with no special handling — see
the `api` job in `.github/workflows/operator.yml`.

If `/workspace/gomodcache/cache/download` is ever missing or lacks the
pinned `golang.org/x/mod` version, re-seed it by fetching
`golang.org/x/mod@<version>` through DependaProxy directly (it validates
and caches on first successful fetch) before running `make manifests` /
`make generate` in this sandbox.

`setup-envtest` (the usual way to fetch controller-gen *and* envtest
binaries together) is not used at all: both its released versions declare
`go 1.26.0`, unbuildable on the `golang:1.25` toolchain this project pins,
and both pin a denied `x/mod` version — the same dead end as `go install
controller-gen@version`, just one level removed.

### `make envtest-assets` / `make test-envtest`

Since `setup-envtest` is a dead end here, `hack/fetch-envtest.sh` downloads
the real `kube-apiserver`/`etcd`/`kubectl` binaries directly from
`controller-tools`' `envtest-v<version>` GitHub releases and verifies their
sha512 checksum before use — idempotent, and independent of `go install`
entirely. `internal/apitest/` is the envtest-backed test suite: it installs
both CRDs from `config/crd/bases/` into a real (locally started)
`kube-apiserver`/`etcd`, then round-trips fully-populated objects, checks
every documented defaulting/validation/immutability rule against the real
API server (not against controller code), and checks the printer columns
and short names/categories via discovery. It's skipped (not failed) when
`KUBEBUILDER_ASSETS` isn't set, so `make test` still passes without the
envtest binaries present.

`internal/controller/` mirrors the same `TestMain` skip-guard pattern for its
own envtest-backed suite: it exercises the real reconcile loop (`Reconcile`,
`writeStatus`'s conflict-retry and staleness guard, and the one
manager-based watch test) against a real `kube-apiserver`, with an injectable
clock and an injectable `ObserveFunc` so timeout- and facts-driven
transitions are deterministic without a real cluster clock or real
subsystems.
