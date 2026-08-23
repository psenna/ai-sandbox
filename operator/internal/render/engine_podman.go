package render

import (
	"fmt"
	"net/url"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	acorev1 "k8s.io/client-go/applyconfigurations/core/v1"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
)

const (
	// PodmanContainerName is the engine sidecar's container name. Rendered as
	// a NATIVE sidecar (init container, restartPolicy: Always) -- see
	// podmanEngine.Contribute for why it must be an init container, and why
	// it must be ordered BEFORE sandboxctl.
	PodmanContainerName = "podman"

	// PodmanAPIPort is the loopback TCP port `podman system service` binds.
	// Never a containerPort, never a Service: loopback-only, exactly like
	// SidecarPort (9099). A packet from another pod arrives on eth0, not lo,
	// and is refused by the kernel before podman ever runs.
	PodmanAPIPort int32 = 2375

	// PodmanDockerHost is the DOCKER_HOST (and CONTAINER_HOST) value set on
	// the AGENT container, and the endpoint sandboxctl's podman EngineTeardown
	// dials (internal/sandboxctl/engine_podman.go). Single source of truth:
	// internal/render/pod.go's sidecarSnapshotArgs projects it as
	// --engine-endpoint, so the two can never drift.
	PodmanDockerHost = "tcp://127.0.0.1:2375"

	// PodmanHome is HOME inside the sidecar. quay.io/podman/stable sets NO
	// HOME in its image config (verified against the pinned digest's config
	// blob), and rootless podman derives every default path from it, so this
	// MUST be set explicitly -- exactly as spike/podman-topology.yaml does.
	PodmanHome = "/home/podman"

	// PodmanGraphMountPath is where the layer-cache emptyDir is mounted. It
	// matches the image's own declared VOLUME and spike/podman-topology.yaml.
	// Deliberately mounted ONLY into the podman sidecar, and under NEITHER
	// WorkspaceMountPath nor AgentHomePath, so internal/sandboxctl/snapshot.go
	// -- which archives exactly those two roots -- can never capture it. That
	// is the epic's layer-cache exclusion, enforced structurally rather than
	// by a filter. internal/sandboxctl/exclusions.go's cacheExcludePaths is
	// the belt-and-braces second guard.
	PodmanGraphMountPath = PodmanHome + "/.local/share/containers"

	// DefaultPodmanImage is quay.io/podman/stable:v5.8.2, pinned by its OCI
	// image-INDEX digest (not a per-arch manifest digest) so amd64 and arm64
	// nodes both resolve. 5.8.2 is the exact version spike #23 measured the
	// required securityContext against (docs/spike-rootless-podman.md,
	// "Cluster tested"); a bump can shift what is required, so bumping this
	// constant REQUIRES re-running the #23 privilege ladder
	// (spike/podman-privilege-ladder.yaml) -- spikeladder_test.go fails CI if
	// the ladder's own image refs do not match this constant.
	DefaultPodmanImage = "quay.io/podman/stable@sha256:663e0dbf407987b7db3f20d3588c283a8228db17b282d2029a482d4d47e36964"

	podmanGraphVolumeName = "podman-graph"

	// podmanConfDir / podmanRunDir live on the sidecar's own writable rootfs
	// (readOnlyRootFilesystem is false for this container, unlike sandboxctl).
	// Config files are written by the bootstrap script rather than mounted
	// from a ConfigMap so the engine's Contribution stays purely additive --
	// Contribution cannot create a ConfigMap object -- and so the entire
	// engine configuration is visible in the golden pod file.
	podmanConfDir = "/tmp/podman-conf"
	podmanRunDir  = "/tmp/podman-run"

	// podmanDigestSeparator is what a digest-pinned image reference must
	// contain. See validatePodmanImage.
	podmanDigestSeparator = "@sha256:"
)

// resolvePodmanStorageDriver maps class.spec.engine.storageDriver onto the
// containers/storage driver name written into storage.conf.
//
// "auto" resolves to "overlay" DETERMINISTICALLY, at render time, with NO
// node probe and NO vfs fallback. This is issue #24's own Update section
// ("Drop the degraded single-UID / vfs fallback") and spike #23's
// recommendation 3, both of which supersede the issue's original scope text
// ("auto resolves at startup by probing what the node supports, falling back
// to vfs"): #23 measured native rootless overlay working on kernel 6.8 with
// no /dev/fuse and no fuse-overlayfs, and measured postgres:18 running
// correctly as a service container -- so neither failure mode the fallback
// insured against actually occurs, and shipping untested fallback code for a
// hypothetical is strictly worse than failing loudly.
//
// "vfs" remains selectable EXPLICITLY (it is still in the CRD enum) for an
// operator who knows their kernel lacks rootless overlay. It is never
// selected automatically. It is very slow; engines.md says so.
func resolvePodmanStorageDriver(spec v1alpha1.EngineSpec) string {
	switch spec.StorageDriver {
	case "", "auto", "overlay":
		return "overlay"
	case "vfs":
		return "vfs"
	default:
		return "overlay"
	}
}

// podmanImage returns the sidecar image: class.spec.engine.image when set,
// DefaultPodmanImage otherwise.
func podmanImage(in Inputs) string {
	if in.Class != nil && in.Class.Spec.Engine.Image != "" {
		return in.Class.Spec.Engine.Image
	}
	return DefaultPodmanImage
}

// validatePodmanImage is a render-time guard: an override MUST be
// digest-pinned. The required securityContext was established empirically
// against podman 5.8.2 (spike #23); an unpinned tag silently changes the
// engine's security posture out from under the ladder that validated it, and
// there is no way to notice. Helm guard G21 mirrors this at template time,
// quoting this message.
func validatePodmanImage(ref string) error {
	if !strings.Contains(ref, podmanDigestSeparator) {
		return fmt.Errorf(
			"render: engine %q image %q is not pinned by digest: the securityContext this engine requires was established empirically against podman 5.8.2 (operator/docs/spike-rootless-podman.md) and a version change can invalidate it. Use a %s… reference, or leave spec.engine.image empty to get the operator's own pinned default (%s)",
			v1alpha1.EngineTypeRootlessPodman, ref, podmanDigestSeparator, DefaultPodmanImage)
	}
	return nil
}

// podmanEngine renders the rootless-podman engine (#24): one `podman system
// service` sidecar serving the Docker-compatible API on pod loopback, one
// emptyDir layer cache, DOCKER_HOST on the agent, and exactly three
// securityContext relaxations -- on the SIDECAR ONLY.
//
// The security argument for this engine is that the relaxation is confined to
// a container that holds nothing worth stealing: no ServiceAccount token, no
// Secret, no hostPath, no ConfigMap, no agent home. That is a MAINTENANCE
// invariant, not a mechanism -- the day someone adds a projected token "for
// convenience", the agent inherits it through the Docker API. It is pinned by
// TestPodmanEngine_SidecarMountsAndTokensNeverGrow (engine_podman_test.go),
// which asserts the sidecar's volume/mount/env set by EXACT EQUALITY, so it
// fails the moment the set grows at all.
type podmanEngine struct{}

func (podmanEngine) Type() v1alpha1.EngineType { return v1alpha1.EngineTypeRootlessPodman }

// Contribute is nil-safe on in.Class/in.Env: EngineRelaxations (engine.go)
// calls it with a ZERO Inputs to compute the EngineSecurityRelaxed condition
// without rendering a pod, and a nil dereference there would panic every
// reconcile.
func (e podmanEngine) Contribute(in Inputs) (Contribution, error) {
	img := podmanImage(in)
	if err := validatePodmanImage(img); err != nil {
		return Contribution{}, err
	}
	return Contribution{
		InitContainers: []*acorev1.ContainerApplyConfiguration{podmanContainer(in, img)},
		Volumes: []*acorev1.VolumeApplyConfiguration{
			acorev1.Volume().WithName(podmanGraphVolumeName).
				WithEmptyDir(acorev1.EmptyDirVolumeSource()),
		},
		AgentEnv: []*acorev1.EnvVarApplyConfiguration{
			acorev1.EnvVar().WithName("DOCKER_HOST").WithValue(PodmanDockerHost),
			acorev1.EnvVar().WithName("CONTAINER_HOST").WithValue(PodmanDockerHost),
		},
		Relaxations: podmanRelaxations(),
	}, nil
}

// podmanRelaxations is the exact, complete set spike #23's privilege ladder
// proved necessary -- and nothing else. Each Reason is surfaced VERBATIM in
// the EngineSecurityRelaxed condition message, which internal/controller/
// podsecurity.go truncates at maxRelaxationMessageBytes (512); the three
// below format to well under that.
func podmanRelaxations() []Relaxation {
	return []Relaxation{
		{
			Container: PodmanContainerName,
			Kind:      RelaxAppArmorUnconfined,
			Reason:    "spike #23 case G: the default AppArmor profile denies the mount() calls overlay storage needs",
		},
		{
			Container: PodmanContainerName,
			Kind:      RelaxSeccompUnconfined,
			Reason:    "spike #23 case H: RuntimeDefault denies clone(CLONE_NEWUSER); podman cannot start",
		},
		{
			Container: PodmanContainerName,
			Kind:      RelaxAllowPrivilegeEscalation,
			Reason:    "spike #23 case J: no-new-privileges voids newuidmap's cap_setuid file capability",
		},
	}
}

func podmanContainer(in Inputs, img string) *acorev1.ContainerApplyConfiguration {
	return acorev1.Container().
		WithName(PodmanContainerName).
		WithImage(img).
		// A NATIVE sidecar (KEP-753): an init container with restartPolicy
		// Always. Two reasons, both load-bearing:
		//  1. START ordering. A regular container gives the kubelet no
		//     ordering guarantee at all, so the agent's first `docker` call
		//     would race podman's bind and get ECONNREFUSED. As an init
		//     container with a startupProbe, the kubelet does not start the
		//     agent until podman answers its own API.
		//  2. TERMINATION ordering. Native sidecars are terminated AFTER
		//     regular containers, in REVERSE start order. RenderPod therefore
		//     orders engine init containers BEFORE sandboxctl, so the
		//     shutdown order is: agent, then sandboxctl, then podman -- i.e.
		//     sandboxctl's SIGTERM-path freeze can still reach the podman API
		//     to tear down workload containers before /workspace is tarred.
		//     The reverse order would let podman die first and leave the
		//     freeze tarring a workspace with live writers in it.
		WithRestartPolicy(corev1.ContainerRestartPolicyAlways).
		// command+args, not the image's own CMD (/bin/bash): the bootstrap
		// script below writes the three containers/*.conf files this engine
		// requires and then execs the service. Writing them here rather than
		// mounting a ConfigMap keeps Contribution purely additive (it cannot
		// create API objects) and keeps the ENTIRE engine configuration
		// visible and reviewable in the golden pod file.
		WithCommand("/bin/bash", "-c").
		WithArgs(podmanBootstrapScript(in)).
		WithEnv(
			acorev1.EnvVar().WithName("HOME").WithValue(PodmanHome),
			acorev1.EnvVar().WithName("XDG_RUNTIME_DIR").WithValue(podmanRunDir),
			// TMPDIR on the layer-cache emptyDir, not the container rootfs:
			// image extraction stages multi-hundred-MB blobs, and keeping
			// them on the same filesystem as the graph root avoids a
			// cross-device copy on commit.
			acorev1.EnvVar().WithName("TMPDIR").WithValue(PodmanGraphMountPath+"/tmp"),
			acorev1.EnvVar().WithName("CONTAINERS_CONF").WithValue(podmanConfDir+"/containers.conf"),
			acorev1.EnvVar().WithName("CONTAINERS_STORAGE_CONF").WithValue(podmanConfDir+"/storage.conf"),
			acorev1.EnvVar().WithName("CONTAINERS_REGISTRIES_CONF").WithValue(podmanConfDir+"/registries.conf"),
		).
		// EXACTLY two mounts. See the type doc comment and
		// TestPodmanEngine_SidecarMountsAndTokensNeverGrow. The workspace is
		// mounted at the IDENTICAL path the agent sees (WorkspaceMountPath),
		// because a `-v /workspace:/work` the agent types is resolved in THIS
		// container's filesystem, not the agent's.
		WithVolumeMounts(
			acorev1.VolumeMount().WithName(workspaceVolumeName).WithMountPath(WorkspaceMountPath),
			acorev1.VolumeMount().WithName(podmanGraphVolumeName).WithMountPath(PodmanGraphMountPath),
		).
		// An API-level probe, not a local `podman info`: --remote --url dials
		// the very socket the agent will use, so a service that started but
		// failed to bind is caught. 2s x 30 = 60s budget.
		WithStartupProbe(acorev1.Probe().
			WithExec(acorev1.ExecAction().WithCommand(
				"podman", "--remote", "--url", PodmanDockerHost, "info", "--format", "{{.Host.Arch}}")).
			WithPeriodSeconds(2).
			WithFailureThreshold(30)).
		WithResources(acorev1.ResourceRequirements().
			WithRequests(corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("100m"),
				corev1.ResourceMemory: resource.MustParse("128Mi"),
			}).
			WithLimits(corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("2"),
				corev1.ResourceMemory: resource.MustParse("2Gi"),
			})).
		WithSecurityContext(podmanSecurityContext())
}

// podmanSecurityContext is the podman sidecar's BASE securityContext, before
// RenderPod's applyRelaxations layers the three Relaxations on top. It is
// deliberately NOT sidecarSecurityContext() and NOT agentSecurityContext():
//
//   - NO `capabilities` field at all. sidecarSecurityContext sets
//     capabilities.drop: [ALL], which removes CAP_SETUID/CAP_SETGID from the
//     container's BOUNDING set -- so newuidmap's cap_setuid=ep FILE
//     capability could never be raised, reproducing ladder case J's
//     "newuidmap: write to uid_map failed" exactly. The Relaxation allowlist
//     has no kind that can undo a drop, so the base must simply not drop.
//     Ladder cases C and E independently show added capabilities do not help
//     either, so the correct posture is: do not touch capabilities at all.
//   - NO `seccompProfile` here. RelaxSeccompUnconfined sets it, explicitly,
//     to Unconfined -- see applyRelaxation's comment on why nil is WRONG on
//     this pod.
//   - readOnlyRootFilesystem: false, explicit rather than omitted (matching
//     agentSecurityContext's own convention). podman writes podmanConfDir and
//     podmanRunDir on the rootfs.
//   - runAsNonRoot/runAsUser/runAsGroup 1000: exactly ladder case G. The
//     pinned image sets no USER, so without this the container would run as
//     root -- which case E proved does not help and which would defeat the
//     "the relaxation is confined to an unprivileged container" argument.
func podmanSecurityContext() *acorev1.SecurityContextApplyConfiguration {
	return acorev1.SecurityContext().
		WithRunAsNonRoot(true).
		WithRunAsUser(agentUID).
		WithRunAsGroup(agentGID).
		WithReadOnlyRootFilesystem(false)
}

// podmanBootstrapScript is the deterministic bash program the sidecar runs:
// write the three containers/*.conf files this engine requires, then exec the
// service. Pure function of Inputs -- same Inputs, byte-identical script.
//
// bash (not sh) for `pipefail`, matching agentCommand's own reasoning. The
// heredocs are quoted ('EOF') so nothing in them is shell-expanded.
//
// storage.conf's [storage.options.overlay] block is entirely OMITTED, not
// just missing mount_program: verified directly against the pinned image
// (`docker run --rm --entrypoint sh quay.io/podman/stable@sha256:663e0d... -c
// 'cat /etc/containers/storage.conf'`), the shipped default sets ONLY
// mount_program = "/usr/bin/fuse-overlayfs" under that table -- no mountopt
// line at all. mount_program is dropped because the pod has no /dev/fuse
// (spike #23); with nothing else the image sets there, the whole table has
// nothing left to say.
func podmanBootstrapScript(in Inputs) string {
	var engineSpec v1alpha1.EngineSpec
	if in.Class != nil {
		engineSpec = in.Class.Spec.Engine
	}
	driver := resolvePodmanStorageDriver(engineSpec)
	registries := podmanRegistriesConf(in)

	return "set -euo pipefail\n" +
		"umask 0022\n" +
		"mkdir -p " + podmanConfDir + " " + podmanRunDir + " " +
		PodmanGraphMountPath + "/storage " + PodmanGraphMountPath + "/tmp\n" +
		"cat >" + podmanConfDir + "/storage.conf <<'EOF'\n" +
		"[storage]\n" +
		"driver = \"" + driver + "\"\n" +
		"runroot = \"" + podmanRunDir + "/containers\"\n" +
		"graphroot = \"" + PodmanGraphMountPath + "/storage\"\n" +
		"EOF\n" +
		"cat >" + podmanConfDir + "/containers.conf <<'EOF'\n" +
		"[engine]\n" +
		"cgroup_manager = \"cgroupfs\"\n" +
		"events_logger = \"file\"\n" +
		"runtime = \"crun\"\n" +
		"EOF\n" +
		"cat >" + podmanConfDir + "/registries.conf <<'EOF'\n" +
		registries +
		"EOF\n" +
		// PodmanDockerHost, not a third copy of the literal: this is the
		// same endpoint the agent's DOCKER_HOST and sandboxctl's
		// --engine-endpoint carry, and they must never drift apart.
		"exec podman system service --time=0 " + PodmanDockerHost + "\n"
}

// podmanRegistriesConf renders the registries.conf body. With no mirror
// declared it is just the unqualified-search + short-name lines; with one, it
// adds one [[registry]]/[[registry.mirror]] pair per upstream prefix, sorted.
//
// `insecure` is derived from the URL SCHEME, never configured separately: an
// http:// mirror is by definition not TLS-verifiable, and making that a
// second, independently-settable field invites a class that says https:// and
// insecure=true.
//
// DependaProxy is deliberately NOT referenced here. DependaProxy proxies npm,
// PyPI and Go modules (see the stack's dependaproxy.yaml: three registry
// blocks, prefixes /npm /pypi /goproxy) and has no OCI/distribution endpoint
// at all -- it cannot serve container images. The dependaproxy half of this
// issue's "registry configuration" scope item is env-var propagation into
// WORKLOAD containers (NPM_CONFIG_REGISTRY / PIP_INDEX_URL / GOPROXY, already
// on the agent's environment via internal/render/secret.go), which is the
// agent's job and is documented in the use-docker skill.
func podmanRegistriesConf(in Inputs) string {
	var b strings.Builder
	b.WriteString("unqualified-search-registries = [\"docker.io\"]\n")
	b.WriteString("short-name-mode = \"permissive\"\n")

	if in.Class == nil {
		return b.String()
	}
	rm := in.Class.Spec.Services.RegistryMirror
	if rm == nil || rm.URL == "" {
		return b.String()
	}

	u, err := url.Parse(rm.URL)
	if err != nil {
		return b.String()
	}
	location := u.Host + strings.TrimSuffix(u.Path, "/")
	insecure := u.Scheme == "http"

	registries := rm.Registries
	if len(registries) == 0 {
		registries = []string{"docker.io"}
	}
	registries = dedupeSorted(registries)

	for _, prefix := range registries {
		b.WriteString("\n[[registry]]\n")
		fmt.Fprintf(&b, "prefix = %q\n", prefix)
		fmt.Fprintf(&b, "location = %q\n", prefix)
		b.WriteString("\n[[registry.mirror]]\n")
		fmt.Fprintf(&b, "location = %q\n", location)
		if insecure {
			b.WriteString("insecure = true\n")
		}
	}
	return b.String()
}

// dedupeSorted returns a sorted copy of ss with duplicates removed.
func dedupeSorted(ss []string) []string {
	seen := make(map[string]bool, len(ss))
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}
