package controller

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sandboxv1alpha1 "github.com/psenna/ai-sandbox/operator/api/v1alpha1"
)

// +kubebuilder:rbac:groups=sandbox.psenna.dev,resources=servicesets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=sandbox.psenna.dev,resources=servicesets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=sandbox.psenna.dev,resources=servicesets/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete

// ServiceSetReconciler reconciles a ServiceSet to native Pods/Services/PVCs.
type ServiceSetReconciler struct {
	client.Client
}

func (r *ServiceSetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var ss sandboxv1alpha1.ServiceSet
	if err := r.Get(ctx, req.NamespacedName, &ss); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	// Filled in by later tasks: reconcile services, runtimes, readiness, prune.
	_ = ss
	return ctrl.Result{}, nil
}

func (r *ServiceSetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&sandboxv1alpha1.ServiceSet{}).
		Owns(&corev1.Pod{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Named("serviceset").
		Complete(r)
}
