package render

import (
	"fmt"
	"path"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	acorev1 "k8s.io/client-go/applyconfigurations/core/v1"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
)

const (
	// AgentContainerName is the name of the agent container in the rendered
	// pod.
	AgentContainerName = "agent"

	// SidecarContainerName is the always-present sandboxctl control-channel
	// sidecar (#27). Engine-independent by construction: rendered directly
	// here, never through Engine.Contribute, because the agent must be able
	// to declare a wait and report a result regardless of which container
	// engine (if any) the class selected.
	SidecarContainerName = "sandboxctl"

	// RestoreContainerName is the one-shot wake/restore init container
	// (#29): the SAME sandboxctl binary/image, `restore` subcommand,
	// ordered LAST among init containers. See restoreContainer's doc
	// comment for why this must be a plain init container (no
	// restartPolicy) rather than folded into the native sidecar.
	RestoreContainerName = "restore"

	// SidecarPort is the loopback port the control API binds. Matches
	// SidecarBaseURL (inputs.go), already projected into the rendered
	// ConfigMap's sandbox.json since #19.
	SidecarPort int32 = 9099

	// SidecarBinaryPath is where the operator image places the sandboxctl
	// binary -- see operator/Dockerfile.
	SidecarBinaryPath = "/sandboxctl"

	sidecarTokenVolumeName = "sandboxctl-token"

	// SidecarTokenMountPath is deliberately the STANDARD in-cluster
	// credential path, so sandboxctl can use rest.InClusterConfig()
	// unmodified (and get client-go's automatic bound-token reload for
	// free). automountServiceAccountToken is false at both the SA and pod
	// level; this projected volume is mounted into the sandboxctl
	// container ONLY, never the agent container.
	SidecarTokenMountPath = "/var/run/secrets/kubernetes.io/serviceaccount" //nolint:gosec // G101: a mount PATH, not a credential value

	sidecarTokenExpirationSeconds int64 = 3600

	// rootCAConfigMapName is the per-namespace ConfigMap kube-controller-
	// manager's root-ca-cert-publisher writes into every namespace (GA
	// since 1.21). Projecting it reproduces what the kubelet's own
	// automount would have provided.
	rootCAConfigMapName = "kube-root-ca.crt"

	workspaceVolumeName = "workspace"
	agentHomeVolumeName = "agent-home"
	configVolumeName    = "config"

	// snapshotCredentialsVolumeName is the projected S3 snapshot-credentials
	// Secret (#28, snapshotsecret.go), mounted into the sandboxctl container
	// ONLY, never the agent container -- resolving storage/doc.go's gap G1
	// without granting the sidecar's ServiceAccount any new RBAC (see
	// rbac.go's renderRole doc comment).
	snapshotCredentialsVolumeName = "snapshot-credentials"
	// SnapshotCredentialsMountPath matches sandboxctl's
	// defaultSnapshotCredentialsDir and the --snapshot-credentials-dir flag
	// default (internal/sandboxctl/snapshotconfig.go) -- kept in sync by
	// convention/tests, not by import: render must stay free of
	// internal/sandboxctl (which pulls in controller-runtime and the AWS
	// SDK transitively via internal/storage).
	SnapshotCredentialsMountPath = "/var/run/secrets/ai-sandbox/snapshot" //nolint:gosec // G101: a mount PATH, not a credential value

	// defaultQuiesceDelayFlag/defaultSnapshotRetriesFlag/
	// defaultSnapshotStepTimeoutFlag mirror sandboxctl's own SnapshotConfig
	// defaults (snapshotconfig.go). Emitted
	// explicitly on the sidecar's args (rather than left to sandboxctl's
	// own flag default) so the full freeze configuration is visible and
	// reviewable in the rendered/golden pod, per this issue's RBAC-gap
	// resolution: "no new sidecar RBAC, everything projected via CLI
	// flags."
	defaultQuiesceDelayFlag        = "2s"
	defaultSnapshotRetriesFlag     = "4"
	defaultSnapshotStepTimeoutFlag = "2m"

	// DefaultTerminationGracePeriodSeconds is how long the kubelet waits
	// between SIGTERM and SIGKILL on the agent pod. It must be long enough
	// for the sidecar to quiesce the agent, tar+zstd /workspace and the
	// agent home, and upload the result. What actually BOUNDS a
	// SIGTERM-path snapshot inside this window is the sidecar's own
	// ShutdownTimeout (internal/sandboxctl/config.go, default 100s,
	// enforced <=110s by Validate): run.go wraps poll.LatchFreezing in a
	// context bounded by ShutdownTimeout, so a snapshot that would overrun
	// it is aborted by context cancellation (leaving no manifest, and
	// therefore no snapshot a restore would ever accept) well before the
	// kubelet's SIGKILL at 120s. 120s itself remains a documented,
	// conservative ballpark for a moderate workspace over a cluster-local
	// S3 endpoint -- deliberately NOT a SandboxClass field, since there is
	// no evidence yet justifying a configurable value.
	DefaultTerminationGracePeriodSeconds int64 = 120

	agentUID int64 = 1000 // node:22-alpine's built-in `node` user
	agentGID int64 = 1000
)

// agentCommand runs the headless Claude Code CLI against the task prompt
// #19 already rendered into the ConfigMap at ConfigMountPath/TaskFileName.
// Command substitution ($(cat ...)), not a pipe: the CLI's documented
// headless form takes the prompt as the -p argument, and stdin is treated
// as ADDITIONAL piped context in the `cat file | claude -p "query"` idiom --
// command substitution is unambiguous under both readings. ARG_MAX (~2MB on
// Linux) comfortably exceeds the CRD's 8KB prompt cap, so there is no size
// risk. bash (not sh) is required for `pipefail`; set -euo pipefail + exec
// means a missing/unreadable task.md aborts non-zero (pod Failed) rather
// than silently invoking `claude -p ""`.
var agentCommand = fmt.Sprintf(
	"set -euo pipefail\nexec claude -p \"$(cat %s)\"\n",
	path.Join(ConfigMountPath, TaskFileName),
)

// RenderPod renders the agent pod for one environment: the agent container
// (image/resources/env from the class, envFrom the #19-rendered Secret,
// mounts for the workspace PVC/ConfigMap plus a fresh agent-home emptyDir),
// composed with the selected engine's Contribution. It ignores
// in.Credentials entirely -- the agent's environment reaches the pod via
// envFrom on the Secret #19 already renders, never inlined here, so this
// function needs no credential of its own.
//
// NO container `command` is set, only `args`: the agent image's own
// ENTRYPOINT (/entrypoint.sh) must run to configure git-proxy auth, drop
// skill files into /workspace/.claude/skills/, and write .npmrc/pip.env/
// go.env. Overriding command would skip all of that.
func RenderPod(in Inputs) (*acorev1.PodApplyConfiguration, error) {
	if err := validateInputs(in); err != nil {
		return nil, err
	}
	if in.SidecarImage == "" {
		return nil, fmt.Errorf("render: SidecarImage is required (set the operator's --sidecar-image flag)")
	}
	names := ChildNames(in.Env.Name)

	engine, err := engineFor(in.Class.Spec.Engine.Type)
	if err != nil {
		return nil, fmt.Errorf("render: resolving engine: %w", err)
	}
	contribution, err := engine.Contribute(in)
	if err != nil {
		return nil, fmt.Errorf("render: engine contribution: %w", err)
	}
	if err := validateNoReservedContainerNames(contribution); err != nil {
		return nil, err
	}

	agent := agentContainer(in, names)
	containers := append([]*acorev1.ContainerApplyConfiguration{agent}, contribution.Containers...)

	// sandboxctl is ALWAYS first among init containers: a restartable
	// (native) sidecar that must be running for the whole pod lifetime,
	// including during the restore init container (#29), so the agent
	// never observes a pod without a control channel. The restore
	// container (if any) is ALWAYS last: a plain (non-restartable) init
	// container under restartPolicy: Never is the only way "never start
	// the agent on a partially restored workspace" is enforced by the
	// kubelet itself -- see restoreContainer's doc comment.
	initContainers := append(
		[]*acorev1.ContainerApplyConfiguration{sidecarContainer(in, names)},
		contribution.InitContainers...,
	)
	if in.Restore != nil && isS3Backend(in) {
		initContainers = append(initContainers, restoreContainer(in, names))
	}

	pod := acorev1.Pod(names.Pod, in.Env.Namespace).
		WithLabels(Labels(in.Env)).
		WithOwnerReferences(ownerReference(in.Env)).
		WithSpec(acorev1.PodSpec().
			WithRestartPolicy(corev1.RestartPolicyNever).
			WithServiceAccountName(names.ServiceAccount).
			WithAutomountServiceAccountToken(false).
			WithTerminationGracePeriodSeconds(DefaultTerminationGracePeriodSeconds).
			WithSecurityContext(podSecurityContext()).
			WithVolumes(append(podVolumes(in, names), contribution.Volumes...)...).
			WithInitContainers(initContainers...).
			WithContainers(containers...))

	if err := applyRelaxations(pod, contribution.Relaxations); err != nil {
		return nil, fmt.Errorf("render: applying engine relaxations: %w", err)
	}
	return pod, nil
}

// validateNoReservedContainerNames errors if an engine Contribution names a
// container AgentContainerName or SidecarContainerName -- a collision would
// produce an invalid Pod (duplicate container names), or worse, silently
// let an engine contribution shadow the always-present sidecar. Fail at
// render time with a clear message naming the offending container.
func validateNoReservedContainerNames(c Contribution) error {
	check := func(name string) error {
		if name == AgentContainerName || name == SidecarContainerName || name == RestoreContainerName {
			return fmt.Errorf("render: engine contribution container name %q collides with a reserved container name", name)
		}
		return nil
	}
	for _, cc := range c.Containers {
		if cc.Name != nil {
			if err := check(*cc.Name); err != nil {
				return err
			}
		}
	}
	for _, cc := range c.InitContainers {
		if cc.Name != nil {
			if err := check(*cc.Name); err != nil {
				return err
			}
		}
	}
	return nil
}

func podSecurityContext() *acorev1.PodSecurityContextApplyConfiguration {
	return acorev1.PodSecurityContext().
		WithRunAsNonRoot(true).
		WithRunAsUser(agentUID).
		WithRunAsGroup(agentGID).
		WithFSGroup(agentGID).
		WithFSGroupChangePolicy(corev1.FSGroupChangeOnRootMismatch).
		WithSeccompProfile(acorev1.SeccompProfile().WithType(corev1.SeccompProfileTypeRuntimeDefault))
}

// sidecarTokenVolume builds the projected ServiceAccount token volume,
// reproducing what the kubelet's own automount would have provided, at the
// standard in-cluster credential path so rest.InClusterConfig() works
// unmodified. Factored out of podVolumes so RenderSnapshotJob
// (snapshotjob.go) can build the IDENTICAL volume without the two
// definitions drifting -- the recovery Job runs the same sandboxctl binary
// and needs the same in-cluster client.
func sidecarTokenVolume() *acorev1.VolumeApplyConfiguration {
	return acorev1.Volume().WithName(sidecarTokenVolumeName).
		WithProjected(acorev1.ProjectedVolumeSource().
			WithDefaultMode(0o444).
			WithSources(
				acorev1.VolumeProjection().WithServiceAccountToken(
					acorev1.ServiceAccountTokenProjection().
						WithPath("token").
						WithExpirationSeconds(sidecarTokenExpirationSeconds)),
				acorev1.VolumeProjection().WithConfigMap(
					acorev1.ConfigMapProjection().
						WithName(rootCAConfigMapName).
						WithItems(acorev1.KeyToPath().WithKey("ca.crt").WithPath("ca.crt"))),
				acorev1.VolumeProjection().WithDownwardAPI(
					acorev1.DownwardAPIProjection().WithItems(
						acorev1.DownwardAPIVolumeFile().
							WithPath("namespace").
							WithFieldRef(acorev1.ObjectFieldSelector().WithFieldPath("metadata.namespace")))),
			))
}

// snapshotCredentialsVolume builds the projected snapshot-credentials
// Secret volume (#28), mounted into the sandboxctl container ONLY. Shared
// by RenderPod and RenderSnapshotJob so the two definitions cannot drift.
func snapshotCredentialsVolume(names Names) *acorev1.VolumeApplyConfiguration {
	return acorev1.Volume().WithName(snapshotCredentialsVolumeName).
		WithSecret(acorev1.SecretVolumeSource().
			WithSecretName(names.SnapshotSecret).
			WithDefaultMode(0o400).
			WithOptional(false))
}

func podVolumes(in Inputs, names Names) []*acorev1.VolumeApplyConfiguration {
	vols := []*acorev1.VolumeApplyConfiguration{
		acorev1.Volume().WithName(workspaceVolumeName).
			WithPersistentVolumeClaim(acorev1.PersistentVolumeClaimVolumeSource().WithClaimName(names.PVC)),
		acorev1.Volume().WithName(agentHomeVolumeName).
			WithEmptyDir(acorev1.EmptyDirVolumeSource()),
		acorev1.Volume().WithName(configVolumeName).
			WithConfigMap(acorev1.ConfigMapVolumeSource().WithName(names.ConfigMap).WithDefaultMode(0o444)),
		// sidecarTokenVolumeName: mounted into the sandboxctl container ONLY
		// (never the agent container). automountServiceAccountToken is false
		// at both the SA (renderServiceAccount) and pod (RenderPod) level.
		sidecarTokenVolume(),
	}
	if isS3Backend(in) {
		vols = append(vols, snapshotCredentialsVolume(names))
	}
	return vols
}

// isS3Backend reports whether in.Class configures an S3 snapshot storage
// backend.
func isS3Backend(in Inputs) bool {
	b := in.Class.Spec.Storage.Backend
	return b.Type == v1alpha1.StorageBackendTypeS3 && b.S3 != nil
}

func agentContainer(in Inputs, names Names) *acorev1.ContainerApplyConfiguration {
	c := acorev1.Container().
		WithName(AgentContainerName).
		WithImage(in.Class.Spec.Agent.Image).
		WithArgs("bash", "-c", agentCommand).
		WithWorkingDir(WorkspaceMountPath).
		WithEnvFrom(acorev1.EnvFromSource().WithSecretRef(acorev1.SecretEnvSource().WithName(names.Secret))).
		WithVolumeMounts(
			acorev1.VolumeMount().WithName(workspaceVolumeName).WithMountPath(WorkspaceMountPath),
			acorev1.VolumeMount().WithName(agentHomeVolumeName).WithMountPath(AgentHomePath),
			acorev1.VolumeMount().WithName(configVolumeName).WithMountPath(ConfigMountPath).WithReadOnly(true),
		).
		WithSecurityContext(agentSecurityContext())
	// resources: omit the field entirely when the class sets none, so an
	// empty ResourceRequirements never becomes owned-but-empty SSA drift.
	if !isZeroResources(in.Class.Spec.Agent.Resources) {
		c = c.WithResources(convertResources(in.Class.Spec.Agent.Resources))
	}
	return c
}

// agentSecurityContext matches config/manager/manager.yaml's own container
// securityContext, with one deliberate divergence: readOnlyRootFilesystem
// is explicitly false, not omitted, because entrypoint.sh writes
// /home/node/.npmrc and `git config --global` writes /home/node/.gitconfig,
// neither of which lives on a mounted volume. This is exactly the
// no-relaxation posture spike #23 recommended for the agent container
// (docs/spike-rootless-podman.md: "no relaxation whatsoever") and will not
// need to change when #24 lands -- #24's relaxations apply to its OWN
// sidecar container, never to this one.
func agentSecurityContext() *acorev1.SecurityContextApplyConfiguration {
	return acorev1.SecurityContext().
		WithAllowPrivilegeEscalation(false).
		WithCapabilities(acorev1.Capabilities().WithDrop(corev1.Capability("ALL"))).
		WithSeccompProfile(acorev1.SeccompProfile().WithType(corev1.SeccompProfileTypeRuntimeDefault)).
		WithRunAsNonRoot(true).
		WithRunAsUser(agentUID).
		WithReadOnlyRootFilesystem(false)
}

// sidecarContainer renders the always-present sandboxctl control-channel
// sidecar as a native sidecar (KEP-753): an init container with
// restartPolicy Always. See RenderPod's doc comment for why a REGULAR
// container here would keep the pod out of Succeeded forever under
// restartPolicy: Never, breaking lifecycle.agentOrPodTerminal.
//
// Probes: exec, not httpGet. The sidecar binds 127.0.0.1:SidecarPort; a
// kubelet httpGet probe dials the pod IP, not loopback, so it would never
// reach a loopback-bound listener. Only a startupProbe is set -- no
// readinessProbe, no livenessProbe: the startupProbe gates the AGENT
// container's start (so the control API exists before the agent's first
// curl) while omitting a readinessProbe keeps the sidecar container
// immediately Ready, preserving today's single-container pod Ready
// semantics (nextRestoring's Restoring->Running gates on facts.PodReady).
//
// Explicitly rejected, not implemented here:
//   - A NetworkPolicy for "unreachable from outside the pod": the loopback
//     bind is already sufficient (no containerPort, no Service, no
//     Ingress; a packet from any other pod/node arrives on eth0, not lo,
//     and is refused by the kernel before userspace ever runs). A
//     NetworkPolicy would be security theatre implying the bind is
//     insufficient.
//   - A Unix domain socket instead of TCP loopback: would need a shared
//     volume the agent container could write to, and would break the
//     documented `curl http://localhost:9099/...` interface the epic and
//     the use-sandbox skill specify.
func sidecarContainer(in Inputs, names Names) *acorev1.ContainerApplyConfiguration {
	args := append([]string{
		"serve",
		"--environment=" + in.Env.Name,
		"--namespace=" + in.Env.Namespace,
		fmt.Sprintf("--listen=127.0.0.1:%d", SidecarPort),
	}, sidecarSnapshotArgs(in)...)

	mounts := []*acorev1.VolumeMountApplyConfiguration{
		acorev1.VolumeMount().WithName(sidecarTokenVolumeName).
			WithMountPath(SidecarTokenMountPath).WithReadOnly(true),
		acorev1.VolumeMount().WithName(workspaceVolumeName).WithMountPath(WorkspaceMountPath),
		acorev1.VolumeMount().WithName(agentHomeVolumeName).WithMountPath(AgentHomePath),
	}
	if isS3Backend(in) {
		mounts = append(mounts, acorev1.VolumeMount().WithName(snapshotCredentialsVolumeName).
			WithMountPath(SnapshotCredentialsMountPath).WithReadOnly(true))
	}

	return acorev1.Container().
		WithName(SidecarContainerName).
		WithImage(in.SidecarImage).
		WithRestartPolicy(corev1.ContainerRestartPolicyAlways).
		// The image's ENTRYPOINT is the operator manager binary; the
		// sidecar container must override command, not just args -- this
		// is the one place in this repo where that is correct (agentContainer's
		// "no command, only args" rule is specifically about the AGENT
		// image's own entrypoint doing its setup work; it does not apply
		// to this container).
		WithCommand(SidecarBinaryPath).
		WithArgs(args...).
		WithVolumeMounts(mounts...).
		WithStartupProbe(acorev1.Probe().
			WithExec(acorev1.ExecAction().WithCommand(SidecarBinaryPath, "healthcheck")).
			WithPeriodSeconds(1).
			WithFailureThreshold(30)).
		WithResources(acorev1.ResourceRequirements().
			WithRequests(corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("50m"),
				corev1.ResourceMemory: resource.MustParse("64Mi"),
			}).
			WithLimits(corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("500m"),
				corev1.ResourceMemory: resource.MustParse("256Mi"),
			})).
		WithSecurityContext(sidecarSecurityContext())
}

// sidecarSnapshotArgs builds the #28 freeze-configuration flags for the
// sandboxctl sidecar. The sidecar has no RBAC to read the cluster-scoped
// SandboxClass and does not mount the <env>-config ConfigMap (see
// sidecarContainer's own volume mounts), so this is the ONLY way it learns
// its snapshot backend configuration -- deliberately visible/reviewable in
// the rendered/golden pod. Deterministic order; empty-valued flags are
// omitted entirely (never emitted as e.g. "--s3-region=").
func sidecarSnapshotArgs(in Inputs) []string {
	var args []string
	add := func(flag, value string) {
		if value != "" {
			args = append(args, "--"+flag+"="+value)
		}
	}

	add("engine", string(in.Class.Spec.Engine.Type))
	add("cluster-id", in.ClusterID)
	add("spec-hash", in.SpecHash)
	add("agent-image", in.Class.Spec.Agent.Image)
	add("workspace-path", WorkspaceMountPath)
	add("agent-home-path", AgentHomePath)

	backend := in.Class.Spec.Storage.Backend
	add("snapshot-backend", string(backend.Type))
	if backend.Type == v1alpha1.StorageBackendTypeS3 && backend.S3 != nil {
		s3 := backend.S3
		add("s3-endpoint", s3.Endpoint)
		add("s3-bucket", s3.Bucket)
		add("s3-region", s3.Region)
		add("s3-prefix", s3.Prefix)
		forcePathStyle := true
		if s3.ForcePathStyle != nil {
			forcePathStyle = *s3.ForcePathStyle
		}
		args = append(args, fmt.Sprintf("--s3-force-path-style=%t", forcePathStyle))
		add("snapshot-credentials-dir", SnapshotCredentialsMountPath)
	}

	args = append(args,
		"--quiesce-delay="+defaultQuiesceDelayFlag,
		"--snapshot-retries="+defaultSnapshotRetriesFlag,
		"--snapshot-step-timeout="+defaultSnapshotStepTimeoutFlag,
	)
	return args
}

// restoreContainer renders the one-shot wake/restore init container (#29):
// the SAME sandboxctl binary/image as the sidecar, `restore` subcommand,
// ordered LAST among init containers by RenderPod.
//
// This MUST be a plain (regular) init container -- no restartPolicy field
// set at all, never restartPolicy: Always -- and it MUST run last. Both are
// required, not style choices:
//
//   - Kubernetes runs regular init containers to completion, in order, and
//     starts no regular container (the agent) until every init container,
//     including a native sidecar's own startupProbe, has succeeded. Placing
//     restore last as a plain init container is the ONLY way "never start
//     an agent on a partially restored workspace" is enforced by the
//     kubelet itself: a non-zero exit here fails the whole pod under
//     restartPolicy: Never.
//   - Folding restore into sandboxctl's own native-sidecar startup is
//     wrong: its startupProbe has a 30s budget (failureThreshold: 30,
//     periodSeconds: 1) gating on /healthz, which any real restore would
//     blow, and restartPolicy: Always containers can never fail the pod --
//     making "a corrupted snapshot must fail loudly" inexpressible there.
//
// Only rendered when in.Restore is non-nil AND the class's storage backend
// is S3 (isS3Backend) -- restore has no support for the pvc backend (Q7).
func restoreContainer(in Inputs, names Names) *acorev1.ContainerApplyConfiguration {
	mounts := []*acorev1.VolumeMountApplyConfiguration{
		acorev1.VolumeMount().WithName(sidecarTokenVolumeName).
			WithMountPath(SidecarTokenMountPath).WithReadOnly(true),
		acorev1.VolumeMount().WithName(workspaceVolumeName).WithMountPath(WorkspaceMountPath),
		acorev1.VolumeMount().WithName(agentHomeVolumeName).WithMountPath(AgentHomePath),
		acorev1.VolumeMount().WithName(snapshotCredentialsVolumeName).
			WithMountPath(SnapshotCredentialsMountPath).WithReadOnly(true),
	}

	return acorev1.Container().
		WithName(RestoreContainerName).
		WithImage(in.SidecarImage).
		WithCommand(SidecarBinaryPath).
		WithArgs(restoreArgs(in)...).
		WithVolumeMounts(mounts...).
		WithResources(acorev1.ResourceRequirements().
			WithRequests(corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("50m"),
				corev1.ResourceMemory: resource.MustParse("64Mi"),
			}).
			WithLimits(corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("1000m"),
				corev1.ResourceMemory: resource.MustParse("512Mi"), // zstd decompression headroom
			})).
		WithSecurityContext(sidecarSecurityContext())
}

// restoreArgs builds the restore container's args: the same backend/
// credentials/paths flags sidecarSnapshotArgs already builds (reused
// verbatim, never duplicated), plus the restore-specific flags naming which
// snapshot to restore. Defaults are emitted explicitly, matching
// sidecarSnapshotArgs' own "visible/reviewable in the golden pod"
// convention.
func restoreArgs(in Inputs) []string {
	args := append([]string{
		"restore",
		"--environment=" + in.Env.Name,
		"--namespace=" + in.Env.Namespace,
	}, sidecarSnapshotArgs(in)...)

	args = append(args,
		"--restore-snapshot-id="+in.Restore.SnapshotID,
		fmt.Sprintf("--restore-seq=%d", in.Restore.Seq),
		"--restore-warm=true",
		"--restore-retries=4",
		"--restore-step-timeout=2m",
	)
	return args
}

// sidecarSecurityContext is the agent's posture plus readOnlyRootFilesystem:
// true. sandboxctl is a static Go binary in a distroless image writing
// nothing outside its mounted volumes, so unlike the agent container (whose
// entrypoint writes ~/.npmrc and ~/.gitconfig) it needs no writable rootfs.
//
// Note the deliberate QoS trade-off: requests != limits here means a pod
// whose agent container would otherwise be Guaranteed becomes Burstable.
// Chosen because a future freeze snapshot (#28) running in this container
// needs burst headroom; nothing in this repo asserts pod QoS today.
//
// shareProcessNamespace is explicitly REJECTED: it would let the sidecar
// signal the agent's PID, but would also expose
// /proc/<sidecar-pid>/root/var/run/secrets/.../token to the agent container
// (same UID 1000) -- destroying this issue's central security property.
// "Stop the agent process" is achieved cooperatively (the skill tells the
// agent to exit right after /v1/wait or /v1/done) and, for the
// non-cooperative case, by the kubelet's own SIGTERM within
// DefaultTerminationGracePeriodSeconds.
func sidecarSecurityContext() *acorev1.SecurityContextApplyConfiguration {
	return acorev1.SecurityContext().
		WithAllowPrivilegeEscalation(false).
		WithCapabilities(acorev1.Capabilities().WithDrop(corev1.Capability("ALL"))).
		WithSeccompProfile(acorev1.SeccompProfile().WithType(corev1.SeccompProfileTypeRuntimeDefault)).
		WithRunAsNonRoot(true).
		WithRunAsUser(agentUID).
		WithReadOnlyRootFilesystem(true)
}

// isZeroResources reports whether r has neither Limits nor Requests set.
func isZeroResources(r corev1.ResourceRequirements) bool {
	return len(r.Limits) == 0 && len(r.Requests) == 0
}

// convertResources projects a corev1.ResourceRequirements (the shape
// AgentSpec.Resources already is, per #17) into its apply-configuration
// equivalent. Claims (DRA) are deliberately not projected -- nothing in
// this CRD exposes them, and the agent pod has no resource claims.
func convertResources(r corev1.ResourceRequirements) *acorev1.ResourceRequirementsApplyConfiguration {
	rr := acorev1.ResourceRequirements()
	if len(r.Limits) > 0 {
		rr = rr.WithLimits(r.Limits)
	}
	if len(r.Requests) > 0 {
		rr = rr.WithRequests(r.Requests)
	}
	return rr
}

// applyRelaxations mutates pod in place, applying each Relaxation to its
// named container. Errors on an unknown Kind, an empty Reason, or a
// Container that names no container in the rendered pod.
func applyRelaxations(pod *acorev1.PodApplyConfiguration, relaxations []Relaxation) error {
	for _, r := range relaxations {
		if r.Reason == "" {
			return fmt.Errorf("relaxation for container %q has no Reason", r.Container)
		}
		sc, err := findContainerSecurityContext(pod, r.Container)
		if err != nil {
			return err
		}
		if err := applyRelaxation(sc, r); err != nil {
			return err
		}
	}
	return nil
}

// applyRelaxation applies a single relaxation to sc, mutating it in place.
func applyRelaxation(sc *acorev1.SecurityContextApplyConfiguration, r Relaxation) error {
	switch r.Kind {
	case RelaxAppArmorUnconfined:
		// AppArmorProfile is a native securityContext field as of
		// Kubernetes 1.30+ (k8s.io/client-go@v0.35.0 confirms this: see
		// SecurityContextApplyConfiguration.AppArmorProfile), so this
		// project's envtest-pinned 1.35 target needs no annotation
		// fallback.
		sc.AppArmorProfile = acorev1.AppArmorProfile().WithType(corev1.AppArmorProfileTypeUnconfined)
	case RelaxSeccompUnset:
		sc.SeccompProfile = nil
	case RelaxAllowPrivilegeEscalation:
		t := true
		sc.AllowPrivilegeEscalation = &t
	case RelaxAddCapability:
		if r.Value == "" {
			return fmt.Errorf("RelaxAddCapability for container %q has no Value", r.Container)
		}
		if sc.Capabilities == nil {
			sc.Capabilities = acorev1.Capabilities()
		}
		sc.Capabilities.Add = append(sc.Capabilities.Add, corev1.Capability(r.Value))
	default:
		return fmt.Errorf("unknown relaxation kind %q for container %q", r.Kind, r.Container)
	}
	return nil
}

// findContainerSecurityContext returns the (possibly freshly-allocated)
// SecurityContext of the container named name within pod, addressing the
// slice element directly so mutations through the returned pointer persist
// into pod.Spec.Containers (WithContainers stores ContainerApplyConfiguration
// by value, not by pointer -- a range-copy would silently discard writes).
func findContainerSecurityContext(pod *acorev1.PodApplyConfiguration, name string) (*acorev1.SecurityContextApplyConfiguration, error) {
	for i := range pod.Spec.Containers {
		c := &pod.Spec.Containers[i]
		if c.Name != nil && *c.Name == name {
			if c.SecurityContext == nil {
				c.SecurityContext = acorev1.SecurityContext()
			}
			return c.SecurityContext, nil
		}
	}
	return nil, fmt.Errorf("relaxation names container %q, which is not in the rendered pod", name)
}
