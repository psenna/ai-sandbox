# Design: rootless-podman — keep pasta isolation via `/dev/net/tun` (Option C)

**Status:** Recorded fallback design (2026-08-23). A parallel, larger rethink of the
dependency/dev-tool execution model is being brainstormed; this document captures
the minimal, security-preserving fix to the current rootless-podman engine so it is
not lost. If the rethink is deferred, this is the implementation plan.

**Branch:** `feat/24-rootless-podman-engine` (PR #54, issue #24)

## Goal

Fix the rootless-podman e2e failures **without** the security regression. Workload
containers keep pasta network isolation (cannot reach the pod loopback on
`127.0.0.1:9099/2375`). The cost is one hostPath `CharDevice` — `/dev/net/tun`, a
generic kernel device, not host data — which breaks the *letter* of the engine's
"no hostPath" rule but preserves its *spirit* ("the sidecar holds nothing worth
stealing").

## Root cause (both layers)

1. **Network layer (confirmed).** Default-network container start fails at
   `setting up Pasta: Failed to open() /dev/net/tun` (read in the AC1 podman
   sidecar log). Kind does not expose `/dev/net/tun` to pods; pasta needs it to
   create the TAP device for the new network namespace.
2. **Post-network layer (reasoned from the `--network host` 500).** The explicit
   `--network host` specs fail with the same agent-facing `500`, *downstream* of
   network setup. Userns works (AC1 reaches pasta; `--network host` skips network
   and fails at the next step). Once `/dev/net/tun` lets AC1 past pasta, AC1 would
   hit that same downstream step. [podman#16405](https://github.com/containers/podman/issues/16405)
   — rootless podman in an unprivileged k8s pod, our exact scenario — fixes that
   step with `cgroups="disabled"` (kind has no cgroup delegation,
   [kind#2916](https://github.com/kubernetes-sigs/kind/issues/2916)) and
   `default_sysctls=[]` (sysctl writes to read-only `/proc`).

Mechanism verification: kind `extraMounts` bind-mounts `/dev/net/tun` into the
(kind nodes run `--privileged`, rootful Docker provider) node; the pod's hostPath
`CharDevice` volume is added to the pod's device cgroup by the kubelet; pasta opens
it and creates the TAP in its *own* netns (CAP_NET_ADMIN over its own namespace via
userns — no host capability needed).

## Engine change (`internal/render/engine_podman.go`)

### a. New `containers.conf` `[containers]` block in `podmanBootstrapScript`

Keep the existing `[engine]` block untouched. Add:

```toml
[containers]
cgroups="disabled"
default_sysctls = []
```

`netns` is left empty → rootless default = **pasta** (isolation preserved). This is
the [#16405](https://github.com/containers/podman/issues/16405) post-network fix,
reasoned from confirmed evidence (the `--network host` 500), not speculative.

- `default_sysctls=[]` loses ICMP `ping` in workload containers (documented; specs
  use `wget`/`psql`).
- `cgroups="disabled"` loses per-workload *cgroup accounting* (workload containers
  are still bounded by the pod's k8s limits; the sandbox sets no per-container
  limits). Neither affects isolation/security.

### b. `/dev/net/tun` hostPath `CharDevice`

Added to `Contribute.Volumes` + a `VolumeMount` on `podmanContainer` (the sidecar
only — never the agent). Rendered with `type: CharDevice` so a node lacking the
device **fails loudly** (consistent with `resolvePodmanStorageContext`'s
no-silent-fallback philosophy). This is an engine `Contribution.Volume`, **not** a
`Relaxation` — the Relaxation enum stays securityContext-only and unchanged.

## Invariant + security-argument update

`TestPodmanEngine_SidecarMountsAndTokensNeverGrow` currently forbids hostPath in
three checks (exact mount set; "every contributed volume is a bare EmptyDir";
"whole-pod sweep, no HostPath anywhere"). Rewrite to keep the **exact-equality,
fails-on-growth** property but allow exactly one hostPath: `/dev/net/tun` as
`CharDevice` (assert it is a char device, not a `Directory` hostPath = host data).
Update the doc comment: the sidecar's one hostPath is a generic network device the
engine needs for rootless networking — plumbing, not stealable host data — so "the
sidecar holds nothing worth stealing" holds. `engine.go`'s "cannot express a
hostPath" comment becomes "cannot express a hostPath to host data; the podman
engine mounts one fixed generic char device."

## CI change

- **`test/e2e/manifests/kind-cluster.yaml`**: add
  `extraMounts: [{hostPath: /dev/net/tun, containerPath: /dev/net/tun}]`.
- **`hack/e2e-up.sh`**: loud precheck before `kind create cluster` —
  `[ -e /dev/net/tun ] || modprobe tun`; fail with an actionable message if still
  absent.

## Diagnostics fix (independently justified)

`test/e2e/diagnostics.go` `dumpNamespacePods` iterates only `p.Spec.Containers` —
it never logs `InitContainers`, and the podman sidecar is a native sidecar *init
container*. This is exactly why the `--network host` error was unreadable. Add an
`InitContainers` (and `EphemeralContainers`) loop so the podman sidecar log is
captured per-failing-spec (while pods are still alive).

## Tests

- All five `isolation_test.go` specs pass **unchanged** (pasta blocks loopback,
  `--network host` still works, NetworkPolicy governs egress, restricts/allows
  egress). No spec rewrites, no #24 acceptance-criteria changes.
- AC1 (`engine_test.go:153`) passes **unchanged** — named network + DNS now works
  with `/dev/net/tun`.
- One e2e assertion changes: `renders a podman sidecar…` mount-set grows to include
  the tun mount.
- Unit: regenerate golden pods (`podman-minimal/full/restore`); rewrite the
  invariant test per above. Grep for any other `HostPath` assertion (helm guards)
  and update.
- PSS guard (`podsecurity.go`): no change — driven by `RelaxationKind`; the engine
  is already rejected at `baseline`/`restricted` by AppArmor, so the hostPath adds
  no new PSS constraint. Docs note that the device is a further `baseline`
  violation, already covered by the privileged-namespace requirement.

## Docs

- `engines.md` / `operations.md`: `/dev/net/tun` node requirement (tun module
  loaded); `cgroups="disabled"`/`default_sysctls=[]` behavior.
- `security.md`: keep all loopback-isolation claims (they stay true); add
  `/dev/net/tun` to the "sidecar holds nothing worth stealing" argument. **No
  residual-risk regression** (unlike the host-networking alternative).
- `spike/podman-topology.yaml` + `spike-rootless-podman.md`: document `/dev/net/tun`
  + the `[containers]` overrides as a shipped divergence from the bare spike
  manifest (like the existing seccomp/APE divergences).
- `use-docker` skill: fix the inaccurate line ("default network provides name
  resolution between containers" — rootless pasta is outbound-only).

## Open sub-decision

`cgroups="disabled"` + `default_sysctls=[]`: ship **universally** (simplest;
CI=production behavior; minor accounting/ping loss everywhere) — recommended — or
keep cgroups enabled in production and disable only in kind via a bootstrap-script
probe (preserves production accounting, adds ~5 fragile lines).

## Validation plan (cannot run kind locally — sandbox is nested DinD)

Ship the above. The first CI run either goes green or, if a third downstream issue
exists, the diagnostics fix now captures the podman sidecar log → root-cause and
fix targeted. The two `containers.conf` fixes are reasoned from confirmed evidence
(AC1 sidecar log + the `--network host` 500); the diagnostics fix ensures any
remainder is visible, not guessed at.