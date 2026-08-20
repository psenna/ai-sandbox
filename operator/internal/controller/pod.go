package controller

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
	"github.com/psenna/ai-sandbox/operator/internal/lifecycle"
	"github.com/psenna/ai-sandbox/operator/internal/render"
	"github.com/psenna/ai-sandbox/operator/internal/storage"
)

// ensurePod is the ActionEnsurePod handler. It reuses applyOne verbatim --
// the same pre-apply ownership guard matters here too: SSA ForceOwnership
// reclaims field ownership without regard to ownerReferences, so an apply
// against a same-named foreign Pod would annex it. applyOne's
// IsInvalid/IsForbidden "permanent, log and continue" classification is
// doubly important for a Pod: almost all of a Pod's spec is immutable after
// creation, so a class change mid-run yields a 422 that must NOT be treated
// as a hot-loopable error, and must NOT trigger a delete-and-recreate --
// that would destroy the agent's context, which freeze/wake exists
// specifically to avoid.
func (r *Reconciler) ensurePod(ctx context.Context, env *v1alpha1.SandboxEnvironment, class *v1alpha1.SandboxClass) error {
	if class == nil {
		return nil
	}
	// SpecHash is projected onto the sidecar's --spec-hash flag (#28) so a
	// freeze's manifest can record which class+env spec produced it. RenderPod
	// itself ignores in.Credentials (see its doc comment): the pod never
	// inlines a credential, it only names the operator-rendered
	// snapshot-credentials Secret by ChildNames.
	specHash, err := storage.SpecHash(&class.Spec, &env.Spec)
	if err != nil {
		ctrl.LoggerFrom(ctx).V(1).Info("agent pod not renderable: spec hash", "reason", err.Error())
		return nil
	}
	pod, err := render.RenderPod(render.Inputs{Env: env, Class: class, ClusterID: r.ClusterID, SidecarImage: r.SidecarImage, SpecHash: specHash, Restore: restorePlanFor(env)})
	if err != nil {
		ctrl.LoggerFrom(ctx).V(1).Info("agent pod not renderable", "reason", err.Error())
		return nil
	}
	names := render.ChildNames(env.Name)
	return r.applyOne(ctx, env, "Pod", client.ObjectKey{Namespace: env.Namespace, Name: names.Pod}, &corev1.Pod{}, pod)
}

// restorePlanFor computes the render.RestorePlan for a wake (#29): the
// snapshot status.snapshot names, formatted as the exact directory ID the
// freeze wrote (storage.SnapshotID truncates to whole seconds UTC and
// metav1.Time serialises RFC3339-seconds, so the ID reproduces the freeze's
// directory exactly -- the property freeze_test.go already relies on when it
// reads layout.Manifest(0, snap.TakenAt.Time)). nil means "nothing to
// restore" -- the first-ever run, or a snapshot whose TakenAt is unset --
// in which case RenderPod emits no restore init container at all. Stability
// within a phase: status.snapshot changes only during Freezing, so
// re-renders within one Restoring are identical and the immutable Pod spec
// is never re-applied with a diff.
func restorePlanFor(env *v1alpha1.SandboxEnvironment) *render.RestorePlan {
	s := env.Status.Snapshot
	if s == nil || s.TakenAt == nil {
		return nil
	}
	return &render.RestorePlan{
		SnapshotID: storage.SnapshotID(int(s.Seq), s.TakenAt.Time),
		Seq:        s.Seq,
	}
}

// deletePod is the ActionDeletePod handler. It only ever deletes a pod this
// environment owns, is idempotent (no-op if already gone or already
// terminating), and uses a UID precondition so a delete issued against a
// stale Grant/observation cannot delete a pod that was recreated since.
func (r *Reconciler) deletePod(ctx context.Context, env *v1alpha1.SandboxEnvironment, _ *v1alpha1.SandboxClass) error {
	names := render.ChildNames(env.Name)
	var pod corev1.Pod
	err := r.Get(ctx, client.ObjectKey{Namespace: env.Namespace, Name: names.Pod}, &pod)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !ownedByEnv(&pod, env) {
		ctrl.LoggerFrom(ctx).V(1).Info("skipping delete: pod is not owned by this environment", "name", names.Pod)
		return nil
	}
	if !pod.DeletionTimestamp.IsZero() {
		return nil
	}
	uid := pod.UID
	if err := r.Delete(ctx, &pod, client.Preconditions{UID: &uid}); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// observePod fills in the pod-related ClusterFacts. Called unconditionally
// from observeCluster -- even when class is nil -- because PodObserved=false
// blocks Restoring->Running and Running->Done/Failed; silently skipping this
// on an early return would look exactly like a hang.
func (r *Reconciler) observePod(ctx context.Context, env *v1alpha1.SandboxEnvironment, f *lifecycle.ClusterFacts) {
	var pod corev1.Pod
	key := client.ObjectKey{Namespace: env.Namespace, Name: render.ChildNames(env.Name).Pod}
	switch err := r.Get(ctx, key, &pod); {
	case apierrors.IsNotFound(err):
		f.PodObserved = true // we looked, and there is genuinely no pod
	case err != nil:
		// PodObserved stays false: "we cannot see pods" is the safe,
		// documented reading in facts.go. Swallowed here rather than
		// failing the reconcile, matching observeQueuePosition's pattern.
		ctrl.LoggerFrom(ctx).V(1).Info("pod observation unavailable", "error", err)
	default:
		f.PodObserved = true
		if !ownedByEnv(&pod, env) {
			return // a foreign pod squatting the name is not OUR pod
		}
		f.PodExists = true // true even while terminating: nextFreezing waits on this
		f.PodPhase = pod.Status.Phase
		f.PodReady = podReady(&pod)
		// PodAliveForArchive (#32): a pod whose agent-home emptyDir has not
		// been captured in a snapshot yet AND from which a freeze can still
		// capture it -- Running only. Succeeded/Failed is excluded because the
		// agent container is already gone. Pending is excluded for two
		// reasons that point the same way: the agent container has not
		// started, so the agent home is still empty and there is nothing to
		// capture; and the sidecar that performs the freeze is not running
		// either, so terminal()'s detour would set phase=Freezing and then
		// wait for a snapshot that can never arrive -- holding the slot, never
		// archiving, and never releasing the delete finalizer. A run that
		// terminates with a still-Pending pod (an image that will not pull, a
		// failing restore init container) must archive directly instead.
		if env.Status.Snapshot == nil && pod.Status.Phase == corev1.PodRunning {
			f.PodAliveForArchive = true
		}
		if pf := podFailure(&pod, r.now()); pf != nil {
			pf.Message = truncateMessage(pf.Message, podFailureMessageBytes)
			f.PodFailure = pf
		}
	}
}
