package controller

import (
	"context"
	"fmt"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
	"github.com/psenna/ai-sandbox/operator/internal/lifecycle"
)

type actionFunc func(ctx context.Context, r *Reconciler, env *v1alpha1.SandboxEnvironment, class *v1alpha1.SandboxClass) error

// actions is the single dispatch map from lifecycle.Action to its handler.
// Every entry in lifecycle.AllActions must have one here -- performActions
// errors out on a missing entry rather than silently doing nothing, so a
// newly declared Action with no wired handler fails loudly instead of
// wedging the reconcile loop in a way that looks like a hang.
var actions = map[lifecycle.Action]actionFunc{
	lifecycle.ActionEnsureResources: func(ctx context.Context, r *Reconciler, env *v1alpha1.SandboxEnvironment, class *v1alpha1.SandboxClass) error {
		return r.ensureResources(ctx, env, class)
	},
	lifecycle.ActionEnsurePod: func(ctx context.Context, r *Reconciler, env *v1alpha1.SandboxEnvironment, class *v1alpha1.SandboxClass) error {
		return r.ensurePod(ctx, env, class)
	},
	lifecycle.ActionFreezePod: func(ctx context.Context, r *Reconciler, env *v1alpha1.SandboxEnvironment, class *v1alpha1.SandboxClass) error {
		return r.freezePod(ctx, env, class)
	},
	lifecycle.ActionDeletePod: func(ctx context.Context, r *Reconciler, env *v1alpha1.SandboxEnvironment, class *v1alpha1.SandboxClass) error {
		return r.deletePod(ctx, env, class)
	},
	lifecycle.ActionArchive: func(ctx context.Context, r *Reconciler, env *v1alpha1.SandboxEnvironment, class *v1alpha1.SandboxClass) error {
		return r.archive(ctx, env, class)
	},
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
