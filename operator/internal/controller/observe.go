package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
	"github.com/psenna/ai-sandbox/operator/internal/lifecycle"
	"github.com/psenna/ai-sandbox/operator/internal/render"
)

// ObserveFunc gathers the ClusterFacts for one environment. It is a field on
// Reconciler so tests can inject facts and so future issues (#20, #21, #27,
// #28, #29, #30, #32) can replace observeCluster incrementally without
// touching the reconcile loop.
type ObserveFunc func(ctx context.Context, env *v1alpha1.SandboxEnvironment, class *v1alpha1.SandboxClass) (lifecycle.ClusterFacts, error)

// resourceCheck names one child object this environment expects to exist.
type resourceCheck struct {
	kind string
	name string
	obj  client.Object
}

// observeCluster is the real observer, replacing the earlier ObserveStub. It
// keeps the two honest readings the stub already had (SlotGranted,
// AgentWaitDeclared read straight off status) and adds real child-resource
// observation (#19).
//
// ResourcesReady deliberately does NOT require the workspace PVC to be
// Bound. The default volumeBindingMode on most real StorageClasses
// (including k3s's local-path) is WaitForFirstConsumer: the PVC stays
// Pending until a pod that mounts it is scheduled, and no pod exists until
// #21 lands -- which itself only runs after Ready, which requires
// ResourcesReady. Requiring Bound here would deadlock Pending -> Ready
// forever on any normal cluster. Lost (the bound PV was destroyed) is a
// real terminal fault and DOES flip readiness false.
//
// Still stubbed, unchanged from before this issue: PodObserved (#21/#24),
// SnapshotComplete (#28), ProbeObserved (#30), ArchiveWritten (#32).
func (r *Reconciler) observeCluster(ctx context.Context, env *v1alpha1.SandboxEnvironment, class *v1alpha1.SandboxClass) (lifecycle.ClusterFacts, error) {
	f := lifecycle.ClusterFacts{
		SlotGranted:       env.Status.Slot.Granted,
		AgentWaitDeclared: env.Status.WaitFor != nil,
	}
	if class == nil {
		return f, nil
	}

	if _, err := r.resolveCredentials(ctx, class); err != nil {
		f.ResourcesProblem = err.Error()
		return f, nil
	}

	names := render.ChildNames(env.Name)
	checks := []resourceCheck{
		{"ServiceAccount", names.ServiceAccount, &corev1.ServiceAccount{}},
		{"Role", names.Role, &rbacv1.Role{}},
		{"RoleBinding", names.RoleBinding, &rbacv1.RoleBinding{}},
		{"PersistentVolumeClaim", names.PVC, &corev1.PersistentVolumeClaim{}},
		{"ConfigMap", names.ConfigMap, &corev1.ConfigMap{}},
		{"Secret", names.Secret, &corev1.Secret{}},
	}

	pvc, ok, err := r.observeResources(ctx, env, checks, &f)
	if err != nil || !ok {
		return f, err
	}

	if pvc.Status.Phase == corev1.ClaimLost {
		f.ResourcesProblem = fmt.Sprintf("workspace PVC %s is Lost", pvc.Name)
		return f, nil
	}

	f.ResourcesReady = true
	return f, nil
}

// observeResources looks up every child in checks, filling in f.ResourcesProblem
// and returning ok=false on the first missing or foreign-owned object. On
// success it returns the observed PVC (for the caller's Bound/Lost check).
func (r *Reconciler) observeResources(ctx context.Context, env *v1alpha1.SandboxEnvironment, checks []resourceCheck, f *lifecycle.ClusterFacts) (*corev1.PersistentVolumeClaim, bool, error) {
	var pvc *corev1.PersistentVolumeClaim
	for _, c := range checks {
		err := r.Get(ctx, client.ObjectKey{Namespace: env.Namespace, Name: c.name}, c.obj)
		if apierrors.IsNotFound(err) {
			f.ResourcesProblem = fmt.Sprintf("waiting for %s %s", c.kind, c.name)
			return nil, false, nil
		}
		if err != nil {
			return nil, false, err
		}
		if !ownedByEnv(c.obj, env) {
			f.ResourcesProblem = fmt.Sprintf("%s %s is owned by another object", c.kind, c.name)
			return nil, false, nil
		}
		if p, ok := c.obj.(*corev1.PersistentVolumeClaim); ok {
			pvc = p
		}
	}
	return pvc, true, nil
}

// ownedByEnv reports whether obj's controller owner reference matches env's
// UID. This is the actual collision-freedom enforcement for the (rare, hash
// suffix notwithstanding) case of two environments' truncated names
// colliding, or of a manually pre-created object squatting on a name this
// operator would otherwise render: identical names alone are never treated
// as "this environment's child".
func ownedByEnv(obj metav1.Object, env *v1alpha1.SandboxEnvironment) bool {
	for _, ref := range obj.GetOwnerReferences() {
		if ref.Controller != nil && *ref.Controller && ref.UID == env.UID {
			return true
		}
	}
	return false
}
