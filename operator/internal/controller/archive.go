package controller

import (
	"context"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
	"github.com/psenna/ai-sandbox/operator/internal/lifecycle"
	"github.com/psenna/ai-sandbox/operator/internal/render"
	"github.com/psenna/ai-sandbox/operator/internal/storage"
)

// escapeHatchAnnotation, set to "true", forces finalizer removal without a
// terminal archive -- the documented operator escape hatch for a genuinely
// unrecoverable backend (credentials rotated away, a deleted bucket) that
// would otherwise wedge deletion forever behind FinalizerArchiveOnDelete.
const escapeHatchAnnotation = "sandbox.psenna.dev/remove-finalizer"

// ensureFinalizer adds v1alpha1.FinalizerArchiveOnDelete if not already
// present. Called on every non-deleting reconcile, before the state machine
// runs, so every environment -- including one that predates this finalizer,
// after an operator upgrade -- eventually carries it. AddFinalizer's own
// return value makes this idempotent: a no-op Update is never issued once
// the finalizer is already there.
func (r *Reconciler) ensureFinalizer(ctx context.Context, env *v1alpha1.SandboxEnvironment) error {
	if controllerutil.AddFinalizer(env, v1alpha1.FinalizerArchiveOnDelete) {
		return r.Update(ctx, env)
	}
	return nil
}

// removeFinalizer drops v1alpha1.FinalizerArchiveOnDelete, letting
// Kubernetes garbage-collect the object once every other finalizer (if any)
// has also cleared. RemoveFinalizer's own return value makes this
// idempotent, matching ensureFinalizer.
func (r *Reconciler) removeFinalizer(ctx context.Context, env *v1alpha1.SandboxEnvironment) error {
	if controllerutil.RemoveFinalizer(env, v1alpha1.FinalizerArchiveOnDelete) {
		return r.Update(ctx, env)
	}
	return nil
}

// reconcileDelete is Reconcile's entire behavior once env.DeletionTimestamp
// is set (#32): it decides whether, and how, to complete the terminal
// archive before letting the finalizer go, so the object is only ever
// removed from the API once its context is durably archived -- or a
// documented reason (escape hatch, non-S3 class, already archived) says
// archiving was never possible, or is already done.
//
// Every branch is idempotent: a second call after DeletionTimestamp is
// already set (the normal "deleted twice" case -- Kubernetes re-delivers a
// delete against an object that already carries a DeletionTimestamp) always
// re-derives the same decision from the same status, and step 2 short-
// circuits immediately once status.archive is non-nil.
func (r *Reconciler) reconcileDelete(ctx context.Context, env *v1alpha1.SandboxEnvironment) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	// 1. Escape hatch: an operator can force finalizer removal without an
	// archive for a genuinely unrecoverable situation. This is a deliberate,
	// documented data-loss action -- the annotation exists specifically to
	// authorize it -- so it is warned loudly via an Event, never silently
	// honored.
	if env.Annotations[escapeHatchAnnotation] == "true" {
		if r.Recorder != nil {
			r.Recorder.Eventf(env, nil, corev1.EventTypeWarning, "ArchiveSkippedByEscapeHatch", "RemoveFinalizer",
				"the %s annotation forced finalizer removal without a terminal archive", escapeHatchAnnotation)
		}
		return ctrl.Result{}, r.removeFinalizer(ctx, env)
	}

	// 2. Already archived (including by the normal Done/Failed completion
	// path -- see actions.go's archive/terminal()'s ActionArchive -- which
	// runs regardless of deletion): nothing left to do.
	if env.Status.Archive != nil {
		return ctrl.Result{}, r.removeFinalizer(ctx, env)
	}

	// 3. Resolve the class; a missing class or a non-S3 backend is a
	// documented, deliberate limitation -- archiving is unsupported on a
	// pvc-backed class exactly like freeze already is, and a class deleted
	// before its environments leaves no S3 config to archive to.
	class, err := r.resolveClass(ctx, env)
	if err != nil || class.Spec.Storage.Backend.Type != v1alpha1.StorageBackendTypeS3 {
		return ctrl.Result{}, r.removeFinalizer(ctx, env)
	}

	// 4. Confirm the backend is at least constructible before doing anything
	// else: a credentials Secret that has gone missing, or a malformed class
	// config, must hold the finalizer and retry -- never silently skip
	// archiving.
	if _, err := r.archiveBackend(ctx, class); err != nil {
		log.V(1).Info("archive backend not resolvable; will retry", "reason", err.Error())
		return ctrl.Result{RequeueAfter: lifecycle.TerminalArchiveRequeue}, nil
	}

	// 5. If the agent home has never been snapshotted and a live pod still
	// exists, the transcripts are only capturable by freezing it now --
	// exactly the freeze-detour terminal() already performs for an
	// environment that reaches Done/Failed naturally (next.go). A deleted
	// environment can be caught here in ANY phase (mid-run, still Pending,
	// already Waiting with no live pod, ...), so this re-implements just
	// that one decision rather than routing through Next (which has no
	// concept of deletion).
	if env.Status.Snapshot == nil {
		alive, err := r.podAliveForArchive(ctx, env)
		if err != nil {
			return ctrl.Result{}, err
		}
		if alive {
			if err := r.freezeForArchive(ctx, env, class); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: lifecycle.TerminalArchiveRequeue}, nil
		}
	}

	// 6. Either a snapshot already exists to copy the context from, or there
	// is no live pod left to capture -- either way, the archive Job can run
	// now. status.archive flips non-nil once it succeeds; the primary watch
	// already picks up that status write, triggering the reconcile that
	// removes the finalizer at step 2.
	if err := r.ensureArchiveJob(ctx, env, class); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: lifecycle.TerminalArchiveRequeue}, nil
}

// podAliveForArchive mirrors observePod's own PodAliveForArchive gate
// (pod.go): a pod owned by env, Pending or Running. Succeeded/Failed is
// excluded -- the agent container is already gone from either, so nothing
// is capturable by freezing it.
func (r *Reconciler) podAliveForArchive(ctx context.Context, env *v1alpha1.SandboxEnvironment) (bool, error) {
	var pod corev1.Pod
	key := client.ObjectKey{Namespace: env.Namespace, Name: render.ChildNames(env.Name).Pod}
	err := r.Get(ctx, key, &pod)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !ownedByEnv(&pod, env) {
		return false, nil
	}
	return pod.Status.Phase == corev1.PodPending || pod.Status.Phase == corev1.PodRunning, nil
}

// freezeForArchive transitions env into Freezing exactly the way
// nextRunning's own freeze trigger does (next.go): the freeze SIGNAL is the
// phase write itself (the sidecar polls status.phase), so ActionFreezePod
// performs no I/O here -- a live, owned pod means its lost-pod recovery path
// (freeze.go) cannot apply. A single Frozen=False condition, stamped with a
// real LastTransitionTime, is enough to give the resulting PhaseHistory
// entry a non-zero timestamp (lifecycle.Apply's PhaseHistory append reads
// d.Conditions[0].LastTransitionTime) -- required because
// storage.RunRecord.Validate rejects a zero phaseHistory timestamp, and the
// eventual archive Job must never be handed an unwritable run record.
func (r *Reconciler) freezeForArchive(ctx context.Context, env *v1alpha1.SandboxEnvironment, class *v1alpha1.SandboxClass) error {
	now := metav1.NewTime(r.now()).Rfc3339Copy()
	d := lifecycle.Decision{
		Phase:      v1alpha1.PhaseFreezing,
		SlotWanted: true,
		Actions:    []lifecycle.Action{lifecycle.ActionFreezePod},
		Conditions: []metav1.Condition{{
			Type:               lifecycle.ConditionFrozen,
			Status:             metav1.ConditionFalse,
			Reason:             lifecycle.ReasonSnapshotInProgress,
			ObservedGeneration: env.Generation,
			LastTransitionTime: now,
		}},
	}
	if err := r.performActions(ctx, env, class, d); err != nil {
		return err
	}
	return r.writeStatus(ctx, env, d)
}

// ensureArchiveJob resolves credentials + specHash and applies the rendered
// terminal archive Job (render.RenderArchiveJob), mirroring ensureSnapshotJob
// (freeze.go) exactly.
func (r *Reconciler) ensureArchiveJob(ctx context.Context, env *v1alpha1.SandboxEnvironment, class *v1alpha1.SandboxClass) error {
	log := ctrl.LoggerFrom(ctx)

	creds, err := r.resolveCredentials(ctx, class)
	if err != nil {
		log.V(1).Info("archive job credentials not resolvable", "reason", err.Error())
		return nil
	}
	specHash, err := storage.SpecHash(&class.Spec, &env.Spec)
	if err != nil {
		log.V(1).Info("archive job spec hash not computable", "reason", err.Error())
		return nil
	}

	job, err := render.RenderArchiveJob(render.Inputs{
		Env:          env,
		Class:        class,
		Credentials:  creds,
		ClusterID:    r.ClusterID,
		SidecarImage: r.SidecarImage,
		SpecHash:     specHash,
	})
	if err != nil {
		log.V(1).Info("archive job not renderable", "reason", err.Error())
		return nil
	}

	names := render.ChildNames(env.Name)
	return r.applyOne(ctx, env, "Job", client.ObjectKey{Namespace: env.Namespace, Name: names.ArchiveJob}, &batchv1.Job{}, job)
}

// archive is the ActionArchive dispatch target (actions.go), replacing the
// #32 notImplemented stub. By the time this fires -- from terminal()'s
// non-detour branch, for an environment that reached Done/Failed on its own
// without being deleted -- a snapshot already exists, or the pod is already
// gone (terminal()'s own freeze detour guarantees one or the other), so this
// only ever needs to ensure the Job. class is nil, or non-S3, only for an
// unresolvable/pvc-backed class: a documented, deliberate limitation
// mirroring freeze's own S3-only restriction.
func (r *Reconciler) archive(ctx context.Context, env *v1alpha1.SandboxEnvironment, class *v1alpha1.SandboxClass) error {
	if class == nil || class.Spec.Storage.Backend.Type != v1alpha1.StorageBackendTypeS3 {
		return nil
	}
	return r.ensureArchiveJob(ctx, env, class)
}
