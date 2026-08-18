package controller

// +kubebuilder:rbac:groups=sandbox.psenna.dev,resources=sandboxenvironments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=sandbox.psenna.dev,resources=sandboxenvironments/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=sandbox.psenna.dev,resources=sandboxenvironments/finalizers,verbs=update
// +kubebuilder:rbac:groups=sandbox.psenna.dev,resources=sandboxclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=sandbox.psenna.dev,resources=sandboxclasses/status,verbs=get;update;patch
//
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles;rolebindings,verbs=get;list;watch;create;update;patch
//
// The sandboxenvironments{,/status} grants above are what makes it legal
// under the RBAC escalation/bind check to create the per-environment Role
// (which only grants a resourceNames-restricted subset of them) and its
// RoleBinding. The secrets grant is cluster-wide because environments run
// in arbitrary namespaces while the class-referenced source secret lives in
// the operator's own namespace (see Config.ClassSecretNamespace) -- Secret
// reads bypass the informer cache entirely (see manager.go's
// Cache.DisableFor) so this grant never causes a cluster-wide Secret cache.
//
// The pods marker lands with this issue (#21): ensurePod/deletePod/observePod
// need get/list/watch/create/update/patch/delete on pods in the environment's
// own namespace. delete is required (not just get/create/update/patch)
// because terminalOutcome/nextFreezing issue ActionDeletePod. pods/log and
// pods/exec are deliberately NOT granted -- log retrieval belongs to a later
// issue, and pods/exec (a shell into a running agent pod) must never be
// granted to the operator at all.
//
// Markers for events, leases are deliberately NOT added here -- they belong
// to the issues that actually touch those objects (#33). #20's
// SlotScheduler needed no new grants: it only lists sandboxenvironments and
// updates sandboxenvironments/status, both already covered above, and
// LeaseName is deliberately left unset (see slotscheduler.go), so no
// coordination.k8s.io marker is added either. Adding markers now for
// objects nothing in this codebase touches yet would ship an over-broad
// ClusterRole with no code behind it.

import (
	"context"
	"errors"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1 "k8s.io/api/core/v1"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
	"github.com/psenna/ai-sandbox/operator/internal/lifecycle"
)

type Reconciler struct {
	client.Client
	Clock   func() time.Time
	Observe ObserveFunc

	// ClassSecretNamespace is the namespace holding the Secrets referenced
	// by a SandboxClass; see internal/config.Config.ClassSecretNamespace.
	ClassSecretNamespace string
	// ClusterID is projected into the rendered sandbox.json.
	ClusterID string
	// WatchNamespace mirrors internal/config.Config.WatchNamespace ("" = all
	// namespaces). Used by observeQueuePosition to scope its cached List
	// consistently with SlotScheduler's own scope.
	WatchNamespace string
	// SidecarImage mirrors internal/config.Config.SidecarImage; passed into
	// render.Inputs by ensurePod.
	SidecarImage string
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
	d.Conditions = append(d.Conditions, engineSecurityCondition(&env, class, r.now()))

	if err := r.performActions(ctx, &env, class, d); err != nil {
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

// SetupWithManager registers this reconciler with mgr, watching
// SandboxEnvironment plus the child kinds this issue (#19) creates and owns.
//
// Secrets are deliberately NOT watched: an informer would cache every
// Secret in the cluster, exactly what manager.go's Client.Cache.DisableFor
// avoids by making Secret reads always live/uncached. Role/RoleBinding are
// not watched either -- low churn, and drift is corrected within
// lifecycle.MaxRequeueAfter by the unconditional ActionEnsureResources
// re-apply (see internal/lifecycle/next.go's withEnsureResources).
//
// TODO(#21): watch SandboxClass via a spec.classRef.name field index once
// class changes require re-rendering; today the only thing read from the
// class is spec.timeouts, which changes rarely, and RequeueAfter already
// re-reads it at least every 5 minutes.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Observe == nil {
		r.Observe = r.observeCluster
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.SandboxEnvironment{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&corev1.ServiceAccount{}).
		// Pods are watched (not just polled): a pod's phase transitions
		// (Pending->Running, Running->Succeeded/Failed) are exactly what
		// drives Restoring->Running and Running->Done/Failed. Without this
		// watch, those transitions would only be picked up on the
		// RequeueAfter timer -- up to lifecycle.MaxRequeueAfter (5 minutes)
		// late.
		Owns(&corev1.Pod{}).
		Named("sandboxenvironment").
		Complete(r)
}
