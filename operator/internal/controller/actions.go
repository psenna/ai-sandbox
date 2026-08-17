package controller

import (
	"context"
	"fmt"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
	"github.com/psenna/ai-sandbox/operator/internal/lifecycle"
)

type actionFunc func(ctx context.Context, r *Reconciler, env *v1alpha1.SandboxEnvironment, class *v1alpha1.SandboxClass) error

func notImplemented(name string, issue int) actionFunc {
	return func(ctx context.Context, r *Reconciler, env *v1alpha1.SandboxEnvironment, _ *v1alpha1.SandboxClass) error {
		ctrl.LoggerFrom(ctx).V(1).Info("action not implemented yet", "action", name, "issue", issue, "environment", env.Name)
		return nil
	}
}

// actions is the single place "not implemented yet" lives. Each entry logs
// at V(1) and returns nil -- returning an error would wedge the reconcile
// loop in backoff for machinery that simply doesn't exist yet; silently doing
// nothing with no log would hide it. Replace entries as the referenced issues
// land.
var actions = map[lifecycle.Action]actionFunc{
	lifecycle.ActionEnsureResources: func(ctx context.Context, r *Reconciler, env *v1alpha1.SandboxEnvironment, class *v1alpha1.SandboxClass) error {
		return r.ensureResources(ctx, env, class)
	},
	lifecycle.ActionEnsurePod: func(ctx context.Context, r *Reconciler, env *v1alpha1.SandboxEnvironment, class *v1alpha1.SandboxClass) error {
		return r.ensurePod(ctx, env, class)
	},
	lifecycle.ActionFreezePod: notImplemented("FreezePod", 28),
	lifecycle.ActionDeletePod: func(ctx context.Context, r *Reconciler, env *v1alpha1.SandboxEnvironment, class *v1alpha1.SandboxClass) error {
		return r.deletePod(ctx, env, class)
	},
	lifecycle.ActionArchive: notImplemented("Archive", 32),
}

func (r *Reconciler) performActions(ctx context.Context, env *v1alpha1.SandboxEnvironment, class *v1alpha1.SandboxClass, d lifecycle.Decision) error {
	for _, a := range d.Actions {
		fn, ok := actions[a]
		if !ok {
			return fmt.Errorf("no handler registered for action %q", a)
		}
		if err := fn(ctx, r, env, class); err != nil {
			return fmt.Errorf("action %q: %w", a, err)
		}
	}
	return nil
}
