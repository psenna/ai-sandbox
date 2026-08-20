package render

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	abatchv1 "k8s.io/client-go/applyconfigurations/batch/v1"
	acorev1 "k8s.io/client-go/applyconfigurations/core/v1"
)

const (
	archiveJobBackoffLimit            int32 = 2
	archiveJobTTLSecondsAfterFinished int32 = 3600
)

// RenderArchiveJob renders the one-shot terminal archive Job: the SAME
// sandboxctl binary, in archive mode, run against the backend only -- never
// against the workspace/agent-home PVC. Created by
// internal/controller/archive.go's ensureArchiveJob when a deleted
// environment needs its terminal archive written (#32).
//
// UNLIKE the snapshot/recovery Job, this Job mounts NO workspace PVC: the
// archive subcommand reads its inputs from the CR status (via Store.Refresh)
// and copies context.tar.zst backend-to-backend from the most recent freeze
// snapshot, never touching a live workspace. It DOES mount the
// snapshot-credentials Secret (S3 creds) and the sidecar token volume (so
// the archive subcommand's Store can authenticate against the API server and
// patch status.archive).
//
// Returns an error unless the class's storage backend is S3 -- archiving
// (like freeze) is unsupported for any other backend today; the controller
// treats a non-S3 class as a documented, deliberate limitation and never
// calls this renderer in that case (see reconcileDelete's step 3).
func RenderArchiveJob(in Inputs) (*abatchv1.JobApplyConfiguration, error) {
	if err := validateInputs(in); err != nil {
		return nil, err
	}
	if in.SidecarImage == "" {
		return nil, fmt.Errorf("render: SidecarImage is required (set the operator's --sidecar-image flag)")
	}
	if !isS3Backend(in) {
		return nil, fmt.Errorf("render: RenderArchiveJob requires an s3 storage backend")
	}

	names := ChildNames(in.Env.Name)

	args := append([]string{
		"archive",
		"--environment=" + in.Env.Name,
		"--namespace=" + in.Env.Namespace,
	}, sidecarSnapshotArgs(in)...)

	container := acorev1.Container().
		WithName(SidecarContainerName).
		WithImage(in.SidecarImage).
		WithCommand(SidecarBinaryPath).
		WithArgs(args...).
		WithVolumeMounts(
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
				snapshotCredentialsVolume(names),
				sidecarTokenVolume(),
			).
			WithContainers(container))

	job := abatchv1.Job(names.ArchiveJob, in.Env.Namespace).
		WithLabels(Labels(in.Env)).
		WithOwnerReferences(ownerReference(in.Env)).
		WithSpec(abatchv1.JobSpec().
			WithBackoffLimit(archiveJobBackoffLimit).
			WithTTLSecondsAfterFinished(archiveJobTTLSecondsAfterFinished).
			WithTemplate(podTemplate))

	return job, nil
}
