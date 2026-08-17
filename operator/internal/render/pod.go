package render

import (
	"fmt"
	"path"

	corev1 "k8s.io/api/core/v1"
	acorev1 "k8s.io/client-go/applyconfigurations/core/v1"
)

const (
	// AgentContainerName is the name of the agent container in the rendered
	// pod.
	AgentContainerName = "agent"

	workspaceVolumeName = "workspace"
	agentHomeVolumeName = "agent-home"
	configVolumeName    = "config"

	// DefaultTerminationGracePeriodSeconds is how long the kubelet waits
	// between SIGTERM and SIGKILL on the agent pod. It must be long enough
	// for #28's sidecar to quiesce the agent, tar+zstd /workspace and the
	// agent home, and upload the result. #28 does not exist yet, so no
	// snapshot has been measured: 120s is a documented, conservative
	// ballpark for a moderate workspace over a cluster-local S3 endpoint --
	// deliberately NOT a SandboxClass field, since there is no evidence
	// behind any specific value yet. When #28 can measure a real snapshot
	// it either confirms this constant or justifies making it configurable
	// with real data behind the change.
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
	names := ChildNames(in.Env.Name)

	engine, err := engineFor(in.Class.Spec.Engine.Type)
	if err != nil {
		return nil, fmt.Errorf("render: resolving engine: %w", err)
	}
	contribution, err := engine.Contribute(in)
	if err != nil {
		return nil, fmt.Errorf("render: engine contribution: %w", err)
	}

	agent := agentContainer(in, names)
	containers := append([]*acorev1.ContainerApplyConfiguration{agent}, contribution.Containers...)

	pod := acorev1.Pod(names.Pod, in.Env.Namespace).
		WithLabels(Labels(in.Env)).
		WithOwnerReferences(ownerReference(in.Env)).
		WithSpec(acorev1.PodSpec().
			WithRestartPolicy(corev1.RestartPolicyNever).
			WithServiceAccountName(names.ServiceAccount).
			WithAutomountServiceAccountToken(false).
			WithTerminationGracePeriodSeconds(DefaultTerminationGracePeriodSeconds).
			WithSecurityContext(podSecurityContext()).
			WithVolumes(append(podVolumes(names), contribution.Volumes...)...).
			WithInitContainers(contribution.InitContainers...).
			WithContainers(containers...))

	if err := applyRelaxations(pod, contribution.Relaxations); err != nil {
		return nil, fmt.Errorf("render: applying engine relaxations: %w", err)
	}
	return pod, nil
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

func podVolumes(names Names) []*acorev1.VolumeApplyConfiguration {
	return []*acorev1.VolumeApplyConfiguration{
		acorev1.Volume().WithName(workspaceVolumeName).
			WithPersistentVolumeClaim(acorev1.PersistentVolumeClaimVolumeSource().WithClaimName(names.PVC)),
		acorev1.Volume().WithName(agentHomeVolumeName).
			WithEmptyDir(acorev1.EmptyDirVolumeSource()),
		acorev1.Volume().WithName(configVolumeName).
			WithConfigMap(acorev1.ConfigMapVolumeSource().WithName(names.ConfigMap).WithDefaultMode(0o444)),
	}
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
