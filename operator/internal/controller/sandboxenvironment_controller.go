package controller

// +kubebuilder:rbac:groups=sandbox.psenna.dev,resources=sandboxenvironments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=sandbox.psenna.dev,resources=sandboxenvironments/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=sandbox.psenna.dev,resources=sandboxenvironments/finalizers,verbs=update
// +kubebuilder:rbac:groups=sandbox.psenna.dev,resources=sandboxclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=sandbox.psenna.dev,resources=sandboxclasses/status,verbs=get;update;patch
//
// Markers for pods, PVCs, secrets, configmaps, serviceaccounts, roles,
// rolebindings, events, leases are deliberately NOT added here -- they
// belong to the issues that actually touch those objects (#19, #21, #20,
// #33). Adding them now would ship an over-broad ClusterRole with no code
// behind it.

import (
	"context"
	"errors"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
	"github.com/psenna/ai-sandbox/operator/internal/lifecycle"
)

type Reconciler struct {
	client.Client
	Clock   func() time.Time
	Observe ObserveFunc
}

func (r *Reconciler) now() time.Time {
	if r.Clock != nil {
		return r.Clock()
	}
	return time.Now()
}

func (r *Reconciler) resolveClass(ctx context.Context, env *v1alpha1.SandboxEnvironment) (*v1alpha1.SandboxClass, error) {
	var class v1alpha1.SandboxClass
	if err := r.Get(ctx, client.ObjectKey{Name: env.Spec.ClassRef.Name}, &class); err != nil {
		return nil, err
	}
	return &class, nil
}

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	var env v1alpha1.SandboxEnvironment
	if err := r.Get(ctx, req.NamespacedName, &env); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !env.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil // finalizer + archive-on-delete is #32
	}

	class, classErr := r.resolveClass(ctx, &env)

	facts, err := r.Observe(ctx, &env, class)
	if err != nil {
		return ctrl.Result{}, err
	}
	facts.ClassResolved = classErr == nil
	if classErr != nil {
		facts.ClassProblem = classErr.Error()
	}
	facts.Timeouts = lifecycle.ResolveTimeouts(class)

	d := lifecycle.Next(env, facts, r.now())

	if err := r.performActions(ctx, &env, d); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.writeStatus(ctx, &env, d); err != nil {
		if errors.Is(err, errStaleDecision) {
			log.V(1).Info("decision superseded by a concurrent write; awaiting requeue")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: d.RequeueAfter}, nil
}

// SetupWithManager registers this reconciler with mgr, watching only
// SandboxEnvironment.
//
// TODO(#19): watch SandboxClass via a spec.classRef.name field index once
// class changes require re-rendering; today the only thing read from the
// class is spec.timeouts, which changes rarely, and RequeueAfter already
// re-reads it at least every 5 minutes.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Observe == nil {
		r.Observe = ObserveStub
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.SandboxEnvironment{}).
		Named("sandboxenvironment").
		Complete(r)
}
