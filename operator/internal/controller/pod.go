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
	pod, err := render.RenderPod(render.Inputs{Env: env, Class: class, ClusterID: r.ClusterID, SidecarImage: r.SidecarImage})
	if err != nil {
		ctrl.LoggerFrom(ctx).V(1).Info("agent pod not renderable", "reason", err.Error())
		return nil
	}
	names := render.ChildNames(env.Name)
	return r.applyOne(ctx, env, "Pod", client.ObjectKey{Namespace: env.Namespace, Name: names.Pod}, &corev1.Pod{}, pod)
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
		if pf := podFailure(&pod, r.now()); pf != nil {
			pf.Message = truncateMessage(pf.Message, podFailureMessageBytes)
			f.PodFailure = pf
		}
	}
}
