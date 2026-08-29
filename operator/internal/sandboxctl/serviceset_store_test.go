package sandboxctl

import (
	"context"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
)

// TestServiceSetStore_UpsertCreatesThenUpdates exercises the ServiceSet store
// with controller-runtime's fake client (this package's convention -- see
// store_test.go). The store Gets the env for its UID, then create-or-updates
// the ServiceSet CR named after the env, owned by the env, with
// Spec.EnvironmentName stamped from the store's env identity.
func TestServiceSetStore_UpsertCreatesThenUpdates(t *testing.T) {
	scheme := testScheme(t)
	env := &v1alpha1.SandboxEnvironment{
		ObjectMeta: metav1.ObjectMeta{Name: "env-1", Namespace: "ns", UID: types.UID("env-uid-1")},
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(env).
		WithStatusSubresource(&v1alpha1.SandboxEnvironment{}).
		Build()

	store := newServiceSetStore(c, EnvironmentRef{Name: "env-1", Namespace: "ns"})

	spec := v1alpha1.ServiceSetSpec{
		Services: []v1alpha1.ServiceSpec{{Name: "postgres", Image: "postgres:18"}},
	}
	if err := store.Upsert(context.Background(), spec); err != nil {
		t.Fatalf("first Upsert: %v", err)
	}

	key := types.NamespacedName{Name: "env-1", Namespace: "ns"}
	var ss v1alpha1.ServiceSet
	if err := c.Get(context.Background(), key, &ss); err != nil {
		t.Fatalf("Get ServiceSet: %v", err)
	}
	if ss.Spec.EnvironmentName != "env-1" {
		t.Errorf("Spec.EnvironmentName = %q, want env-1 (server stamps it)", ss.Spec.EnvironmentName)
	}
	if len(ss.Spec.Services) != 1 || ss.Spec.Services[0].Name != "postgres" {
		t.Errorf("Spec.Services = %+v, want one postgres", ss.Spec.Services)
	}
	if len(ss.OwnerReferences) != 1 {
		t.Fatalf("OwnerReferences = %+v, want one", ss.OwnerReferences)
	}
	or := ss.OwnerReferences[0]
	if or.Name != "env-1" || or.UID != types.UID("env-uid-1") || or.Controller == nil || !*or.Controller {
		t.Errorf("OwnerReference = %+v, want env-1/env-uid-1/Controller=true", or)
	}
	if or.Kind != "SandboxEnvironment" || or.APIVersion != v1alpha1.GroupVersion.String() {
		t.Errorf("OwnerReference Kind/APIVersion = %q/%q, want SandboxEnvironment/%q", or.Kind, or.APIVersion, v1alpha1.GroupVersion.String())
	}

	// Second Upsert updates the same CR (by name) rather than duplicating it.
	spec2 := v1alpha1.ServiceSetSpec{
		Services: []v1alpha1.ServiceSpec{
			{Name: "postgres", Image: "postgres:18"},
			{Name: "redis", Image: "redis:7"},
		},
	}
	if err := store.Upsert(context.Background(), spec2); err != nil {
		t.Fatalf("second Upsert: %v", err)
	}

	var ss2 v1alpha1.ServiceSet
	if err := c.Get(context.Background(), key, &ss2); err != nil {
		t.Fatalf("Get ServiceSet after update: %v", err)
	}
	if len(ss2.Spec.Services) != 2 {
		t.Fatalf("Spec.Services len = %d, want 2 (updated, not duplicated)", len(ss2.Spec.Services))
	}
	// OwnerReference preserved across update.
	if len(ss2.OwnerReferences) != 1 || ss2.OwnerReferences[0].UID != types.UID("env-uid-1") {
		t.Errorf("OwnerReferences not preserved: %+v", ss2.OwnerReferences)
	}

	// No duplicate ServiceSet was created: a List must return exactly one.
	var list v1alpha1.ServiceSetList
	if err := c.List(context.Background(), &list); err != nil {
		t.Fatalf("List ServiceSet: %v", err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("ServiceSet count = %d, want 1 (update, not create)", len(list.Items))
	}
}

// TestServiceSetStore_MissingEnvironmentSurfacesError verifies the store
// reports a clean error (wrapping the NotFound) when the env it would own the
// ServiceSet under does not exist.
func TestServiceSetStore_MissingEnvironmentSurfacesError(t *testing.T) {
	scheme := testScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	store := newServiceSetStore(c, EnvironmentRef{Name: "no-env", Namespace: "ns"})
	err := store.Upsert(context.Background(), v1alpha1.ServiceSetSpec{
		Services: []v1alpha1.ServiceSpec{{Name: "a", Image: "x"}},
	})
	if err == nil {
		t.Fatal("Upsert: expected error for a missing environment, got nil")
	}
	if !apierrors.IsNotFound(err) {
		t.Errorf("error = %v, want a NotFound-wrapping error", err)
	}
}
