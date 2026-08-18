package render

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	abatchv1 "k8s.io/client-go/applyconfigurations/batch/v1"
	acorev1 "k8s.io/client-go/applyconfigurations/core/v1"
)

const (
	snapshotJobBackoffLimit            int32 = 2
	snapshotJobTTLSecondsAfterFinished int32 = 3600
)

// RenderSnapshotJob renders the one-shot recovery snapshot Job: the SAME
// sandboxctl binary, in freeze-once mode, against the surviving workspace
// PVC. Created by internal/controller/freeze.go's ensureSnapshotJob when
// the pod that should have taken a freeze snapshot is gone but the
// environment's workspace PVC survives (renderPVC's PVC is retained across
// freeze/wake by design).
//
// HONEST LIMITATION, not an oversight: the agent home is an emptyDir, so
// when the pod is lost the Claude Code session transcript is lost with it.
// This Job can only produce a WORKSPACE-ONLY snapshot -- which is exactly
// what the epic describes ("the PVC still exists, so a snapshot Job can be
// run against it"). It passes --agent-home-path="" and the resulting
// manifest lists workspace.tar.zst only (storage.Manifest.Validate
// requires >= 1 file, not exactly 2).
//
// Returns an error unless the class's storage backend is S3 -- freeze (and
// therefore recovery) is unsupported for any other backend today (see
// snapshotconfig.go's BackendUnsupported fail-closed path).
func RenderSnapshotJob(in Inputs) (*abatchv1.JobApplyConfiguration, error) {
	if err := validateInputs(in); err != nil {
		return nil, err
	}
	if in.SidecarImage == "" {
		return nil, fmt.Errorf("render: SidecarImage is required (set the operator's --sidecar-image flag)")
	}
	if !isS3Backend(in) {
		return nil, fmt.Errorf("render: RenderSnapshotJob requires an s3 storage backend")
	}

	names := ChildNames(in.Env.Name)

	args := append([]string{
		"freeze-once",
		"--environment=" + in.Env.Name,
		"--namespace=" + in.Env.Namespace,
	}, sidecarSnapshotArgs(in)...)
	// Overrides sidecarSnapshotArgs' own --agent-home-path=AgentHomePath:
	// the recovery Job never had the agent-home emptyDir, so there is
	// nothing to archive under that path. flag.FlagSet's last-flag-wins
	// semantics makes this override correct positionally (appended last).
	args = append(args, "--agent-home-path=")

	container := acorev1.Container().
		WithName(SidecarContainerName).
		WithImage(in.SidecarImage).
		WithCommand(SidecarBinaryPath).
		WithArgs(args...).
		WithVolumeMounts(
			acorev1.VolumeMount().WithName(workspaceVolumeName).WithMountPath(WorkspaceMountPath),
			acorev1.VolumeMount().WithName(snapshotCredentialsVolumeName).
				WithMountPath(SnapshotCredentialsMountPath).WithReadOnly(true),
			acorev1.VolumeMount().WithName(sidecarTokenVolumeName).
				WithMountPath(SidecarTokenMountPath).WithReadOnly(true),
		).
		WithSecurityContext(sidecarSecurityContext())

	podTemplate := acorev1.PodTemplateSpec().
		WithLabels(Labels(in.Env)).
		WithSpec(acorev1.PodSpec().
			WithRestartPolicy(corev1.RestartPolicyNever).
			WithServiceAccountName(names.ServiceAccount).
			WithAutomountServiceAccountToken(false).
			WithSecurityContext(podSecurityContext()).
			WithVolumes(
				acorev1.Volume().WithName(workspaceVolumeName).
					WithPersistentVolumeClaim(acorev1.PersistentVolumeClaimVolumeSource().WithClaimName(names.PVC)),
				snapshotCredentialsVolume(names),
				sidecarTokenVolume(),
			).
			WithContainers(container))

	job := abatchv1.Job(names.SnapshotJob, in.Env.Namespace).
		WithLabels(Labels(in.Env)).
		WithOwnerReferences(ownerReference(in.Env)).
		WithSpec(abatchv1.JobSpec().
			WithBackoffLimit(snapshotJobBackoffLimit).
			WithTTLSecondsAfterFinished(snapshotJobTTLSecondsAfterFinished).
			WithTemplate(podTemplate))

	return job, nil
}
