package controller

import (
	"context"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
	"github.com/psenna/ai-sandbox/operator/internal/lifecycle"
	"github.com/psenna/ai-sandbox/operator/internal/render"
	"github.com/psenna/ai-sandbox/operator/internal/storage"
)

// observeSnapshot fills in the two freeze facts internal/lifecycle already
// consumes (see facts.go's freeze-related doc comments).
//
// Scoping to the freeze IN FLIGHT is what makes repeated freeze/wake cycles
// correct: status.snapshot is non-nil from the PREVIOUS freeze the moment a
// second one starts, so a naive "snapshot != nil" check would report the new
// freeze complete instantly and delete the pod before anything was
// uploaded. The snapshot produced by freeze N (0-based) always carries
// Seq == N, and status.freezeCount is exactly N until nextFreezing's
// Freezing->Waiting transition increments it -- so Seq >= freezeCount is the
// precise test.
func (r *Reconciler) observeSnapshot(st *v1alpha1.SandboxEnvironmentStatus, f *lifecycle.ClusterFacts) {
	if s := st.Snapshot; s != nil && s.Seq >= st.FreezeCount {
		f.SnapshotComplete = true
	}
	if a := st.SnapshotAttempt; a != nil &&
		a.Phase == v1alpha1.SnapshotAttemptFailed && a.Seq >= st.FreezeCount {
		f.SnapshotFailure = &lifecycle.StepFailure{
			Reason:  lifecycle.ReasonSnapshotFailed,
			Message: truncateMessage(a.Reason+": "+a.Message, podFailureMessageBytes),
		}
	}
}

// freezePod is the ActionFreezePod handler. The freeze SIGNAL is the phase
// write itself (the sidecar polls status.phase; see internal/sandboxctl's
// poller.go), which writeStatus performs AFTER performActions -- so this
// handler deliberately signals nothing.
//
// Its one real job is the lost-pod recovery path: if the environment is
// freezing, the snapshot for THIS freeze has not landed, and the pod that
// was supposed to take it is gone, the workspace PVC still exists -- so a
// one-shot Job running the same sandboxctl binary can take the snapshot
// from the PVC instead. Idempotent in every branch.
func (r *Reconciler) freezePod(ctx context.Context, env *v1alpha1.SandboxEnvironment, class *v1alpha1.SandboxClass) error {
	if class == nil {
		return nil
	}
	if s := env.Status.Snapshot; s != nil && s.Seq >= env.Status.FreezeCount {
		return nil // this freeze's snapshot already landed
	}

	var pod corev1.Pod
	err := r.Get(ctx, client.ObjectKey{Namespace: env.Namespace, Name: render.ChildNames(env.Name).Pod}, &pod)
	switch {
	case err == nil && ownedByEnv(&pod, env) && !podTerminated(&pod):
		return nil // the sidecar in that pod is taking the snapshot
	case err != nil && !apierrors.IsNotFound(err):
		return err
	}

	return r.ensureSnapshotJob(ctx, env, class)
}

// podTerminated reports whether every container in pod has exited -- the
// sandboxctl sidecar that performs the freeze included. A terminated pod is,
// for snapshot purposes, exactly as unavailable as a deleted one: the freeze
// SIGNAL is the status.phase write that the sidecar polls (see freezePod's
// comment and internal/sandboxctl/poller.go), and a sidecar that has exited
// will never observe it.
//
// This is what makes the freeze detour recoverable. terminal()'s detour is
// gated on observing the pod as Running (pod.go's PodAliveForArchive), but
// the agent and the sidecar exit within the same second (both e2e artifact
// collections show exactly that), so a detour decided against a Running
// observation can land phase=Freezing after the sidecar is already gone.
// Without this check nothing recovers: nextFreezing only waits on
// SnapshotComplete, so the environment parks in Freezing forever, holding
// its slot and never archiving.
//
// The recovery Job cannot capture the agent-home emptyDir (snapshotjob.go
// documents that limitation -- it passes --agent-home-path=""), so the
// transcripts of a run that loses this race are still lost. That is
// unavoidable once both containers have exited: nothing can read another
// pod's emptyDir. A workspace-only snapshot that unsticks the environment
// strictly beats an environment wedged in Freezing.
func podTerminated(pod *corev1.Pod) bool {
	return pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed
}

// ensureSnapshotJob renders and applies the recovery snapshot Job (#28,
// snapshotjob.go). No-op (not an error) when the class's storage backend
// isn't S3 (freeze is unsupported there), when credentials aren't
// resolvable yet, or when the render itself errors -- matching
// ensurePod/ensureResources' "log at V(1) and let the observer report the
// resulting unready state" pattern, rather than hot-looping a reconcile
// that can never succeed.
func (r *Reconciler) ensureSnapshotJob(ctx context.Context, env *v1alpha1.SandboxEnvironment, class *v1alpha1.SandboxClass) error {
	log := ctrl.LoggerFrom(ctx)

	if class.Spec.Storage.Backend.Type != v1alpha1.StorageBackendTypeS3 {
		return nil
	}

	creds, err := r.resolveCredentials(ctx, class)
	if err != nil {
		log.V(1).Info("snapshot job credentials not resolvable", "reason", err.Error())
		return nil
	}
	specHash, err := storage.SpecHash(&class.Spec, &env.Spec)
	if err != nil {
		log.V(1).Info("snapshot job spec hash not computable", "reason", err.Error())
		return nil
	}

	job, err := render.RenderSnapshotJob(render.Inputs{
		Env:          env,
		Class:        class,
		Credentials:  creds,
		ClusterID:    r.ClusterID,
		SidecarImage: r.SidecarImage,
		SpecHash:     specHash,
	})
	if err != nil {
		log.V(1).Info("snapshot job not renderable", "reason", err.Error())
		return nil
	}

	names := render.ChildNames(env.Name)
	return r.applyOne(ctx, env, "Job", client.ObjectKey{Namespace: env.Namespace, Name: names.SnapshotJob}, &batchv1.Job{}, job)
}
