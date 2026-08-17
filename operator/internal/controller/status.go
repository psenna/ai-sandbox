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

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fresh := &v1alpha1.SandboxEnvironment{}
		if err := r.Get(ctx, key, fresh); err != nil {
			return err
		}
		if fresh.Generation != decidedGen || fresh.Status.Phase != decidedFrom {
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
