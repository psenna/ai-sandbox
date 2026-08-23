# Security model

This page states the operator's trust boundary plainly: what the agent
container can and cannot reach, what network isolation actually restricts,
and — deliberately not softened — the residual risks that remain even when
everything above is working as designed. See [`engines.md`](engines.md) for
engine-specific relaxations and [`operations.md`](operations.md) for
troubleshooting the conditions this page describes.

## The trust boundary in one picture

```
 ┌─────────────────────────── pod ───────────────────────────┐
 │                                                             │
 │  init: sandboxctl (sidecar)      init: restore (S3 only)   │
 │   - holds the K8s SA token        - runs once, last        │
 │   - loopback API :9099            - non-restartable         │
 │   - patches status.*                                        │
 │                                                             │
 │  ── projected token volume ──►  (mounted ONLY here)         │
 │                                                             │
 │  container: agent                                          │
 │   - NO Kubernetes credential                                │
 │   - has: git-proxy bearer, DependaProxy URLs,                │
 │          model endpoint + key, SANDBOX_SIDECAR_URL           │
 │   - talks to sidecar over 127.0.0.1:9099 only                │
 │                                                             │
 └─────────────────────────────────────────────────────────────┘
        │ egress (Restricted: NetworkPolicy; Open: none)
        ▼
  DNS · API server (ipBlock) · resolved service peers · extraEgress
```

## What the agent container is

The `agent` container is the one place LLM-authored code actually runs. It
holds `/workspace` (the warm cache, persists across freeze/wake) and
`/home/<agent-home>` (restored cold from S3 on every wake), and talks to
the rest of the system through exactly two channels: the sidecar's loopback
control API, and whatever credentials are in its own process environment
(below).

## The agent holds no Kubernetes token

Verified in `internal/render/serviceaccount.go` and `internal/render/pod.go`,
enforced in three independent layers:

1. `renderServiceAccount` sets `automountServiceAccountToken: false` on the
   per-environment `ServiceAccount`.
2. `RenderPod` sets `automountServiceAccountToken: false` on the **pod
   spec** as well — a second, independent layer.
3. The credential itself is a **projected volume**
   (`sandboxctl-token`: a 3600s-expiry `serviceAccountToken`,
   `kube-root-ca.crt`, and the downward-API namespace) mounted at the
   standard path `/var/run/secrets/kubernetes.io/serviceaccount` into the
   `sandboxctl` and `restore` containers **only**. The `agent` container's
   volume mounts are exactly `workspace`, `agent-home`, `config` — no token
   volume is ever attached to it.

### The three enforcement layers

The three points above are independent, not redundant: (1) and (2) both
have to be bypassed by a rendering bug for automount to re-enable itself,
and even then (3) means there is no token *volume* mounted into the agent
container to automount in the first place.

### The one thing the agent CAN influence, and how

`shareProcessNamespace` is explicitly **rejected** in `pod.go`'s
`sidecarSecurityContext` doc comment — sharing the process namespace would
expose `/proc/<sidecar-pid>/root/var/run/secrets/kubernetes.io/serviceaccount/token`
to the agent container (same UID 1000 as the sidecar), because
`/proc/<pid>/root` follows a process's own mount namespace regardless of
which container's filesystem the reader is otherwise confined to. This is
the one credential-boundary decision that had to be made explicitly rather
than falling out of Kubernetes defaults.

The agent's only two channels into cluster state — `status.waitFor` and
`status.agentResult` — are reached only through the sidecar's
**loopback-bound** (`127.0.0.1:9099`) control API: `POST /v1/wait`, `POST
/v1/done`, `POST /v1/progress`, `GET /v1/status`. There is no
`containerPort`, no Service, no Ingress for this API — a packet from
another pod arrives on `eth0`, not `lo`, and is refused by the kernel
before it ever reaches the listener. A NetworkPolicy for this was
explicitly rejected as security theatre (loopback isolation does not need
a policy to enforce it). Covered by e2e specs `does not expose the control
API outside the pod` and `reaches Done via /v1/done with no Kubernetes
credential in the agent container`.

### What RBAC does and does not prove

The per-environment `Role` grants `get` on the `SandboxEnvironment` and
`get`/`patch` on its `status` subresource, `resourceNames`-pinned to this
environment's own name. **RBAC is per-subresource, not per-field** — this
grant technically permits the sidecar's identity to write *any* field
under `status`, not just the ones it is supposed to. The actual field-level
restriction is enforced by `internal/sandboxctl/store.go`'s patch
mutators and asserted by unit tests on the exact merge-patch body — **not**
by RBAC. What RBAC *does* prove, against a real authorizer: this identity
can never patch a **different** environment's status, ever, at any field.

## Credentials the agent DOES hold

Do not read the above as "the agent is credential-free" — it is not. Its
process environment (`internal/render/secret.go`) carries: `AGENT_TOKEN`/
`GIT_PROXY_TOKEN` (the git-proxy bearer — **not** the upstream GitHub PAT),
`GIT_PROXY_URL`, `GIT_PROXY_BROKER_URL`, `GITHUB_REPO`, the DependaProxy
URLs (`NPM_CONFIG_REGISTRY`, `PIP_INDEX_URL`, `GOPROXY`), the model
endpoint/key set (`ANTHROPIC_BASE_URL`/`ANTHROPIC_AUTH_TOKEN`/
`ANTHROPIC_API_KEY`), the four model-tier vars, `CLAUDE_CONFIG_DIR`, and
`SANDBOX_SIDECAR_URL`.

**Say this plainly: the git-proxy bearer token is a real credential in the
agent's environment.** The trust boundary here is not "the agent has no
credentials" — it is that git-proxy holds the upstream PAT and enforces
policy on what that bearer token can do (`secret_scan`, `history_protect`,
`branch_pattern`); the agent never sees the PAT itself.

The S3 snapshot credentials are a **separate** Secret, projected at mode
`0400` into the `sandboxctl`/`restore` containers only
(`SnapshotCredentialsMountPath = /var/run/secrets/ai-sandbox/snapshot`) —
never into the agent.

## Network isolation: `Open` vs `Restricted`

### What `Restricted` allows, exactly

A single `NetworkPolicy` selecting `sandbox.psenna.dev/environment=<env>`,
`policyTypes: [Ingress, Egress]`:

- **Egress rule 1** (hardcoded): DNS to `kube-system` /
  `k8s-app=kube-dns`, ports 53/UDP and 53/TCP.
- **Egress rules 2..N**, the controller-resolved peers, sorted for
  determinism:
  - the API server, always, as an **ipBlock**
    `<kubernetes.default ClusterIP>/32` on port 443 (the `kubernetes`
    Service has no pod selector, so a selector cannot express it);
  - one peer per configured in-cluster `<svc>.<ns>.svc[.cluster.local]`
    endpoint (git-proxy git+broker, DependaProxy npm/pypi/goproxy, Ollama,
    S3), resolved to that Service's own `spec.selector`;
  - every `spec.network.extraEgress` entry verbatim (CIDR -> ipBlock,
    selector -> namespaceSelector+podSelector, ports -> ports; empty ports
    means all ports).
- **Ingress**: exactly one rule allowing the operator's own pods
  (`--operator-ingress-label`, default `control-plane=controller-manager`,
  in `--class-secret-namespace`) — nothing else can reach the pod.

A class whose **external** endpoint is not covered by an `extraEgress` CIDR
is rejected loudly at observe time (`ResourcesNotReady`), not silently
allowed. If the operator cannot resolve the external host within its 3s DNS
budget, the endpoint is **accepted** — coverage is unverifiable, not
provably absent.

### What `Restricted` does NOT protect against

State these plainly — none of them is "mitigated", they are simply outside
what a `NetworkPolicy` can do:

- **It is egress-and-ingress at the pod-network layer only.** It says
  nothing about what the agent does *within* its own pod, its own PVC, or
  its own emptyDir.
- **It does not stop egress to the public internet if you allow it.** Any
  `extraEgress` CIDR you add is allowed wholesale. `0.0.0.0/0` in
  `extraEgress` reduces `Restricted` to `Open` plus a DNS rule, with no
  warning.
- **It cannot enforce anything on a non-enforcing CNI.** The operator
  *measures* this (`CNIEnforcement`) and emits a `Warning` Event
  `NetworkPolicyNotEnforced` on every reconcile when `Restricted` is
  declared but enforcement is unverified — but it cannot make a CNI
  enforce.
- **It does not protect the node.** NetworkPolicy is not a sandbox: it does
  not restrict the kubelet API, the node's cloud-metadata endpoint (the
  operator adds **no** metadata-deny rule of its own), kernel-level
  attacks, or container escape.
- **It does not isolate the agent from other pods on the same node** at any
  layer below the pod network — shared kernel, shared node filesystem via
  any `hostPath` a *different* workload mounts, noisy-neighbour side
  channels.
- **Nested workload containers are unverified.** Whether containers an
  engine launches *inside* the pod inherit the pod's NetworkPolicy was
  **not** verified: the spike explicitly lists "No NetworkPolicy testing"
  under *Not covered*, and the e2e spec that would prove it — `restricts a
  workload container's egress under Restricted isolation and allows it
  under Open` (`test/e2e/isolation_test.go`) — is **`PIt`, pending and
  skipped**. This is moot only because no engine that launches containers
  exists today; it becomes a live risk the day #24 ships.
- **It is per-environment, not per-namespace.** Other pods in the same
  namespace are unaffected by this policy, and no policy stops *them*.

### `Open`

`RenderNetworkPolicy` returns `(nil, nil)` and the reconciler **deletes**
any stale policy left from a prior `Restricted` setting. There is no policy
at all — egress and ingress are whatever the namespace's own defaults are.
`NetworkPosture=False/Open`.

### CNI enforcement is measured, not assumed

`CNIEnforcement` reflects a real, periodic pod-to-pod connectivity probe,
not an assumption drawn from the isolation setting: `Enforced` (True) means
the probe ran and default-deny actually blocked a connection it should
block; `NotEnforced` (False) means the probe ran and **confirmed** the CNI
does not enforce; `Unconfirmed` (Unknown) means the probe has not run yet,
or could not complete. kindnet enforces; the plain AWS VPC CNI does not.

## Engine relaxations

Today, none — the only implemented engine (`none`) requests zero
relaxations, so `EngineSecurityRelaxed` is always `False/NoRelaxation` on a
working class. The relaxation mechanism itself is a **closed allowlist**
(`RelaxationKind`): `AppArmorUnconfined`, `SeccompUnset`,
`AllowPrivilegeEscalation`, `AddCapability`. This closed set structurally
prevents an engine from ever expressing `privileged`, a `hostPath` mount, a
projected Kubernetes token, or a `runAsUser` change — an engine
implementation that tried would hit a **render error**, not a silent
escalation past the allowlist.

## Residual risks (read this section)

These are risks, stated as risks — not paired with a "but it's mitigated
by" clause, because in several cases the honest mitigation is partial or
nonexistent:

1. The agent holds a real git-proxy bearer token; a compromised agent can
   do anything git-proxy's policy permits for that token. git-proxy's
   policy gates (`secret_scan`, `history_protect`, `branch_pattern`) are
   the actual boundary here — not the operator, which has no visibility
   into what the agent does with that token.
2. The agent runs arbitrary LLM-authored code as UID 1000 with a
   **writable root filesystem** (`readOnlyRootFilesystem: false`, required
   by the agent image's own entrypoint) and a writable `/workspace` PVC
   that **persists across freeze/wake**. Anything it plants there comes
   back on the next wake.
3. `Restricted` is only as good as the CNI, and the operator can only
   *report* that fact — it cannot make an enforcing CNI appear.
4. Field-level restriction of the sidecar's status writes is code-enforced
   (in `internal/sandboxctl/store.go`), not RBAC-enforced. A defect in that
   code, not just in RBAC, could let the sidecar write a status field it
   should not.
5. The warm-cache marker (`.sandbox/warm-cache.json`) is agent-writable. It
   is an *optimization hint, not an authority*: a forged marker can cost a
   cold restore, never an unverified one — every warm path re-validates
   `EnvUID`, the manifest SHA, the teardown sequence, and the file list.
   This is a deliberately bounded risk, but it remains a risk surface an
   agent can influence.
6. The `sandbox.psenna.dev/remove-finalizer: "true"` annotation is a
   documented, deliberate data-loss escape hatch. It emits `Warning
   ArchiveSkippedByEscapeHatch` every time — never silent — but anyone with
   `patch` permission on the CR can invoke it and lose the transcript.
7. The operator's own `ClusterRole` grants cluster-wide `secrets`
   `get`/`list`/`watch` (environments live in arbitrary namespaces while
   class Secrets live in one, so the grant has to be cluster-wide). Reads
   bypass the informer cache, so no cluster-wide Secret cache is built in
   memory — but the RBAC **grant** itself is still cluster-wide, and that
   is a larger blast radius than any single reconcile actually uses.
8. `pods/exec` and `pods/log` are deliberately **never** granted to the
   operator — but this is a design choice to note, not a guarantee about
   anything else in the cluster that might hold those verbs.
9. Nested-workload NetworkPolicy inheritance is unverified (see above,
   "What `Restricted` does NOT protect against").

## What is verified, and by what

| Claim | Verified by |
|---|---|
| No Secret/ConfigMap leaks into agent-visible fields | `internal/controller/secretleak_test.go` |
| RBAC identity cannot patch a different environment | `internal/controller/rbac_test.go` |
| Sidecar's status patch is field-restricted | `internal/controller/sidecarpatch_test.go` |
| Rendered objects match golden expectations (incl. security context) | `internal/render/golden_test.go` |
| Control API is loopback-only | `test/e2e/lifecycle_test.go` ("does not expose the control API outside the pod") |
| `/v1/done` works with no Kubernetes credential in the agent container | `test/e2e/lifecycle_test.go` ("reaches Done via /v1/done with no Kubernetes credential in the agent container") |
| `Restricted` NetworkPolicy renders and enforces as declared | `test/e2e/netpolicy_test.go` ("enforces NetworkPolicy") |
| CNI enforcement is actually measured, not assumed | `test/e2e/isolation_test.go` ("reports CNI enforcement verified") |
| Chart never renders a Secret or ConfigMap it shouldn't | `.github/workflows/helm.yml`'s "no Secret or ConfigMap is ever rendered" assertion |
| Nested-workload NetworkPolicy inheritance | **Not verified.** `test/e2e/isolation_test.go`'s corresponding spec is `PIt` — pending and skipped. |

The last row is deliberate: this document would be less honest if that gap
were omitted rather than named.
