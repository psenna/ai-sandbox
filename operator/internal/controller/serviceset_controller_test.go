package controller

import (
	"testing"

	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	sandboxv1alpha1 "github.com/psenna/ai-sandbox/operator/api/v1alpha1"
)

func newServiceSetReconciler(t *testing.T) *ServiceSetReconciler {
	t.Helper()
	return &ServiceSetReconciler{Client: k8s}
}

func mustCreateServiceSet(t *testing.T, ss *sandboxv1alpha1.ServiceSet) *sandboxv1alpha1.ServiceSet {
	t.Helper()
	if err := k8s.Create(ctx, ss); err != nil {
		t.Fatalf("create ServiceSet: %v", err)
	}
	t.Cleanup(func() { _ = k8s.Delete(ctx, ss) })
	return ss
}

// reconcileServiceSetOnce calls r.Reconcile for key, returning the error so
// tests can assert on it. Unlike reconcileOnce (which fatals on error), this
// returns the error -- the no-op fetch test needs to distinguish a fetch
// failure from a nil-return success.
func reconcileServiceSetOnce(t *testing.T, r *ServiceSetReconciler, key types.NamespacedName) (ctrl.Result, error) {
	t.Helper()
	return r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
}

func TestServiceSetReconciler_NoOpFetchesAndReturns(t *testing.T) {
	ss := &sandboxv1alpha1.ServiceSet{
		Spec: sandboxv1alpha1.ServiceSetSpec{EnvironmentName: "env-x"},
	}
	ss.Name, ss.Namespace = "set-x", "default"
	mustCreateServiceSet(t, ss)

	r := newServiceSetReconciler(t)
	if _, err := reconcileServiceSetOnce(t, r, types.NamespacedName{Name: ss.Name, Namespace: ss.Namespace}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}
