# Spike: rootless podman inside an unprivileged Pod

Decision record for [#23](https://github.com/psenna/ai-sandbox/issues/23). Resolves whether
`rootless-podman` can be the default container engine for `SandboxEnvironment`
([#15](https://github.com/psenna/ai-sandbox/issues/15)).

> **Shipped.** This spike's recommendations were implemented in full by
> [#24](https://github.com/psenna/ai-sandbox/issues/24):
> `internal/render/engine_podman.go` is the engine, and this document's
> ladder became a permanent CI regression test
> (`internal/render/spikeladder_test.go`) rather than a one-off record. The
> **Results**, **Root cause**, **Minimum viable configuration** and **Pod
> Security Standards** sections below are the historical record of what was
> measured and are left as originally recorded — do not edit them to match
> the shipped implementation. Where the shipped pod diverges from what the
> ladder measured, a note says so explicitly (see "A correction: seccomp on
> the rendered pod" below). See [`engines.md`](engines.md#rootless-podman---what-you-get)
> and [`security.md`](security.md#engine-relaxations) for the shipped
> engine's own documentation.

## Verdict

**Rootless podman works, and the agent container stays fully hardened.** It is viable as the
default engine — but only in namespaces that do **not** enforce Pod Security Standards
`baseline` or `restricted`, because the podman sidecar requires `appArmorProfile: Unconfined`.

The important structural result is that the relaxation is confined to the **sidecar**. The agent
container — the one running LLM-authored code — keeps `runAsNonRoot`, `allowPrivilegeEscalation:
false`, `capabilities: drop: [ALL]` and `seccompProfile: RuntimeDefault`, and still drives podman
over the Docker-compatible API. The blast radius of the relaxation is a container that runs only
`podman system service`.

## Cluster tested

| | |
|---|---|
| Distribution | k3s v1.36.3+k3s1, single node, control-plane |
| OS | Ubuntu 24.04.4 LTS |
| Kernel | 6.8.0-137-generic (amd64) |
| CRI | containerd 2.3.2-k3s2 |
| LSM | **AppArmor** (Ubuntu default) |
| PSS enforcement | none by default — no `pod-security.kubernetes.io/*` labels on any namespace |
| podman | 5.8.2, crun, from `quay.io/podman/stable` |

## Results

| # | Pod configuration | `podman run` |
|---|---|---|
| A | uid 1000, no seccomp, no caps — the permissive baseline | ❌ |
| B | A + `hostUsers: false` (pod-level user namespace) | ❌ |
| C | uid 1000 + `capabilities: add: [SYS_ADMIN]` | ❌ |
| D | `privileged: true` | ✅ |
| E | uid 0 + `SYS_ADMIN`, all other caps dropped | ❌ |
| F | E + `seccompProfile: RuntimeDefault` | ❌ |
| **G** | **uid 1000, no caps, no seccomp override, `appArmorProfile: Unconfined`** | **✅** |
| H | G + `seccompProfile: RuntimeDefault` | ❌ |
| I | **G as a sidecar + hardened agent container over the Docker API** | ✅ |
| J | G + `allowPrivilegeEscalation: false` | ❌ |

## Root cause

The blocker is **AppArmor**, not capabilities and not user namespaces.

Every failing case died on a mount operation:

```
Error: configure storage: overlay: failed to make mount private:
mount /home/podman/.local/share/containers/storage/overlay:...,
flags: 0x1000: permission denied
```

Reduced to primitives inside the pod, `unshare -U -r` succeeded and produced a full capability set
(`CapEff: 000001ffffffffff`), so user-namespace creation was never the problem. But
`unshare -U -r -m` failed with `cannot change root filesystem propagation: Permission denied`, and
it still failed with `--propagation unchanged`, and a plain bind mount inside the new namespace
failed too. Mount operations were being denied wholesale.

Capabilities were ruled out directly: adding `SYS_ADMIN` changed nothing (C), including as uid 0
with an effective capability set (E). Only `privileged: true` worked (D) — and privileged differs
from "root + SYS_ADMIN" chiefly by also disabling the LSM profiles. Setting **only**
`appArmorProfile: Unconfined`, with no added capabilities and a non-root uid, made it work (G).

That is the containerd default AppArmor profile denying `mount`, which podman needs to set up
overlay storage.

Two secondary constraints fall out of the same tests:

- **`seccompProfile: RuntimeDefault` blocks podman independently** (H): `clone(CLONE_NEWUSER)` is
  denied, `cannot clone: Operation not permitted`. The profile must be left unset.
- **`allowPrivilegeEscalation: false` blocks podman independently** (J): `newuidmap` carries
  `cap_setuid=ep` as a file capability, and no-new-privileges prevents it from taking effect —
  `newuidmap: write to uid_map failed: Operation not permitted`.

`hostUsers: false` is supported on this cluster (the pod got a real mapping,
`0 3040870400 65536`) but does not help (B) — the inherited mounts are still owned by a parent
user namespace.

## Minimum viable configuration

Sidecar container — the **only** place any relaxation is needed:

```yaml
securityContext:
  runAsUser: 1000          # non-root
  runAsGroup: 1000
  appArmorProfile:
    type: Unconfined       # REQUIRED on AppArmor distros; the sole relaxation
  # seccompProfile: MUST be left unset
  # allowPrivilegeEscalation: MUST be left at its default (true)
  # capabilities: none added; NOT privileged
```

Agent container — no relaxation whatsoever:

```yaml
env:
  - name: DOCKER_HOST
    value: tcp://127.0.0.1:2375
securityContext:
  runAsUser: 1000
  allowPrivilegeEscalation: false
  capabilities: { drop: ["ALL"] }
  seccompProfile: { type: RuntimeDefault }
```

A working reference manifest is committed alongside this document at
[`operator/spike/podman-topology.yaml`](../spike/podman-topology.yaml).

## Pod Security Standards

Tested against the API server rather than inferred. Both rejections are quoted verbatim:

- **`baseline`** — rejected: `forbidden AppArmor profile (container must not set AppArmor profile
  type to "Unconfined")`. AppArmor `Unconfined` is the *only* baseline violation, so this is the
  single thing standing between the engine and a baseline namespace.
- **`restricted`** — rejected for AppArmor plus `allowPrivilegeEscalation`, `capabilities`,
  `runAsNonRoot` and `seccompProfile`.

**Supported:** namespaces with no PSS enforcement, or `pod-security.kubernetes.io/enforce:
privileged`. This must be documented as an install prerequisite and enforced by a render-time
guard in the chart.

## What works, measured

- **Storage driver: native rootless `overlay`** on kernel 6.8, not the `vfs` fallback. `/dev/fuse`
  is absent (no device plugin) and fuse-overlayfs is therefore unavailable, but it is not needed.
  This was the main performance risk and it did not materialise.
- **Bind mounts** work in both directions between the pod's volume and workload containers.
- **`postgres:18` runs as a service container** and accepts connections — so multi-UID mapping and
  chown-at-startup work, which was the failure mode feared for the degraded single-UID fallback.
  The fallback is therefore not needed.
- **Container-to-container networking** works.
- **The Docker API round-trip works**: a hardened agent container running `docker:27-cli` against
  `tcp://127.0.0.1:2375` pulled an image and ran a container with the workspace bind-mounted. The
  existing `use-docker` skill should need little or no change.
- **Graph root on an `emptyDir`** works and still selects native overlay — so the layer cache can
  live on a volume that is excluded from snapshots, as the epic assumes.
- **Cost:** `golang:1.25` pulled in 28s; the store reached **1.4 GB** after alpine + postgres:18 +
  golang:1.25. This directly supports excluding the layer cache from freeze snapshots.

## Not covered

- **Only one cluster.** #23 asked for two. Everything here is from Ubuntu/AppArmor + k3s.
- **SELinux distros are untested and may behave differently.** On RHEL/Fedora/Rocky/Amazon Linux
  there is no AppArmor profile to relax, so the sole `baseline` violation may simply not exist —
  which would make the engine `baseline`-compatible there. This is a concrete, testable hypothesis
  and the single highest-value follow-up.
- **No managed cluster** (EKS/GKE/AKS) was tested.
- **`npm ci` through dependaproxy** was not exercised: the spike cluster has no route to the
  sandbox's compose network. Registry configuration remains unvalidated end to end.
- **No NetworkPolicy testing.** k3s ships kube-router for enforcement, but whether workload
  containers inherit the pod's policy was not verified here — it is an acceptance criterion of
  [#31](https://github.com/psenna/ai-sandbox/issues/31).

## Recommendation

1. **Keep `rootless-podman` as the default engine** ([#24](https://github.com/psenna/ai-sandbox/issues/24)).
   It works, it is fast, and it needs one narrow, well-understood relaxation on a sidecar rather
   than on the agent.
2. **Keep the pluggable-engine design.** The PSS constraint is real, so
   [#25](https://github.com/psenna/ai-sandbox/issues/25) (k8s-native broker) remains necessary for
   clusters that enforce `baseline` or `restricted`. It is not optional.
3. **Drop the degraded single-UID/`vfs` fallback** from #24's scope. Native overlay and multi-UID
   mapping both work; the fallback would add code for a case that did not occur.
4. **Add a render-time guard** in the chart that fails clearly when the target namespace enforces
   `baseline` or `restricted` with `engine: rootless-podman`, quoting the AppArmor requirement.
5. **Surface the relaxation in a condition** on `SandboxEnvironment`, as #21 requires, so the
   weakened posture is visible in `kubectl describe`.
6. **Follow up on an SELinux cluster** to test whether `baseline` is achievable there.

## A correction: seccomp on the rendered pod

The ladder's own pods (`spike/podman-privilege-ladder.yaml`) set no
pod-level `securityContext` at all, so a container that left its own
`seccompProfile` unset there inherited the **kubelet's** `seccompDefault`
(`Unconfined` unless the node runs `--seccomp-default=true`) — which is why
case G's "no seccomp override" worked.

The pod `internal/render.RenderPod` actually builds is different: its
pod-level `securityContext.seccompProfile` is `RuntimeDefault`
(`podSecurityContext()` in `internal/render/pod.go`), and every container
that leaves its own `seccompProfile` nil **inherits that pod-level value**
— reproducing the ladder's failing case H, not case G's success. The
shipped engine therefore sets `seccompProfile.type: Unconfined`
**explicitly** on the `podman` sidecar (`RelaxSeccompUnconfined` in
`internal/render/engine.go`) rather than leaving the field unset. The
practical effect — podman starts, `baseline` still rejects it — is
identical to what the ladder measured; only the *mechanism* by which
`baseline` rejects it changed, from one AppArmor violation to two
(AppArmor **and** seccomp). See
[`engines.md`](engines.md#the-pss-constraint) for the resulting
`baseline`/`restricted` story on the shipped pod.

## Re-running the ladder on a podman bump

`internal/render/spikeladder_test.go` fails CI automatically if
`spike/podman-privilege-ladder.yaml`'s image references stop matching
`internal/render/engine_podman.go`'s `DefaultPodmanImage` — so a digest bump
cannot land silently. But the mechanical test only checks that the *ladder
file* and the *shipped constant* agree; it cannot re-verify that the ladder
itself still behaves the same way on a new podman version. Before merging a
`DefaultPodmanImage` bump, re-run the ladder for real against a live
cluster:

```sh
kubectl apply -f operator/spike/podman-privilege-ladder.yaml
for p in pm-baseline pm-hostusers pm-sysadmin pm-sysadmin-root \
         pm-apparmor pm-aa-seccomp pm-aa-noprivesc pm-privileged; do
  kubectl -n spike wait --for=condition=Ready pod/$p --timeout=120s
  echo -n "$p: "; kubectl -n spike exec $p -- \
    podman run --rm docker.io/library/alpine:3.20 echo RUN-OK || echo FAIL
done
kubectl delete ns spike
```

The expected column in the **Results** table above must still hold:

- If `pm-aa-seccomp` or `pm-aa-noprivesc` starts **passing** on the new
  version, the corresponding `Relaxation` in
  `internal/render/engine_podman.go`'s `podmanRelaxations` is no longer
  necessary and should be removed (a relaxation the engine no longer needs
  is an unnecessary PSS violation to carry).
- If `pm-apparmor` (case G) starts **failing**, this engine is broken on
  that podman version — do not merge the bump until you understand why.
