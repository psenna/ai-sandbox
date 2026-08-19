package controller

import (
	"context"
	"errors"

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
	"github.com/psenna/ai-sandbox/operator/internal/lifecycle"
)

// errStaleDecision means the Decision was computed from an object state
// (generation, phase) that a concurrent write has since superseded. It is
// NOT a conflict -- retrying it would risk double-applying a StatusPatch
// (e.g. IncrementFreezeCount) computed against stale input. The caller
// should simply stop: the winning write's watch event will trigger a fresh
// reconcile with fresh facts.
var errStaleDecision = errors.New("decision computed from a superseded object state")

func (r *Reconciler) writeStatus(ctx context.Context, env *v1alpha1.SandboxEnvironment, d lifecycle.Decision) error {
	key := client.ObjectKeyFromObject(env)
	decidedFrom := env.Status.Phase
	decidedGen := env.Generation
	// decidedWaitFor guards against a narrower race than Generation/Phase
	// alone can catch: lifecycle.Next branches directly on
	// env.Status.WaitFor (nextWaiting's WaitFor==nil -> Ready transition,
	// nextRunning's WaitFor!=nil -> Freezing transition), and an external
	// writer (e.g. a future wake API, or -- today -- a test harness driving
	// the freeze/wake contract directly) can clear or set WaitFor without
	// changing Phase at all (the phase the stale Decision computed happens
	// to match the phase already on the server). Without this check, such
	// a write is silently absorbed by the DeepEqual no-op below: fresh
	// already reflects the external change, but desired.Phase is forced
	// back to the stale Decision's phase, and since that matches fresh's
	// current (also-stale) phase, nothing appears to differ and no write
	// -- and so no further watch event -- is ever issued, leaving the
	// object stuck with no other trigger to re-reconcile it.
	decidedWaitFor := env.Status.WaitFor
	// decidedHasSnapshot guards the last status field lifecycle.Next branches
	// on that Generation/Phase/WaitFor cannot catch: nextRestoring computes
	// IncrementWakeCount: env.Status.Snapshot != nil (next.go), and an
	// external writer can change whether status.snapshot is nil -- the
	// sidecar's own RecordSnapshot, or a test harness driving the
	// freeze/wake contract -- without touching Phase or Generation. Without
	// this check, a stale Decision could double- or non-increment
	// WakeCount exactly like the #28 bug this same guard was extended for.
	decidedHasSnapshot := env.Status.Snapshot != nil

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fresh := &v1alpha1.SandboxEnvironment{}
		if err := r.Get(ctx, key, fresh); err != nil {
			return err
		}
		if fresh.Generation != decidedGen || fresh.Status.Phase != decidedFrom ||
			!apiequality.Semantic.DeepEqual(fresh.Status.WaitFor, decidedWaitFor) ||
			(fresh.Status.Snapshot != nil) != decidedHasSnapshot {
			return errStaleDecision
		}
		desired := lifecycle.Apply(fresh, d)
		if apiequality.Semantic.DeepEqual(&fresh.Status, desired) {
			return nil // idempotent: nothing changed, no update issued
		}
		fresh.Status = *desired
		return r.Status().Update(ctx, fresh)
	})
}
