package sandboxctl

import (
	"context"
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sandboxv1alpha1 "github.com/psenna/ai-sandbox/operator/api/v1alpha1"
)

func TestK8sNativeEngineTeardown_ListsEntryNamesAsPods(t *testing.T) {
	ss := &sandboxv1alpha1.ServiceSet{
		ObjectMeta: metav1.ObjectMeta{Name: "env-abc", Namespace: "ns"},
		Status: sandboxv1alpha1.ServiceSetStatus{Entries: []sandboxv1alpha1.EntryStatus{
			{Name: "python", Kind: "runtime", Ready: true},
			{Name: "db", Kind: "service", Ready: true},
			{Name: "redis", Kind: "service", Ready: false},
		}},
	}

	scheme := runtime.NewScheme()
	_ = sandboxv1alpha1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ss).Build()

	eng := NewEngineTeardown("k8s-native", c, "ns", "env-abc")
	report, err := eng.Teardown(context.Background())
	if err != nil {
		t.Fatalf("Teardown: %v", err)
	}

	if report.Engine != "k8s-native" {
		t.Errorf("Engine = %q, want %q", report.Engine, "k8s-native")
	}
	// Sorted, deterministic, every entry name present regardless of Ready.
	wantPods := []string{"db", "python", "redis"}
	if !reflect.DeepEqual(report.Pods, wantPods) {
		t.Errorf("Pods = %v, want %v (sorted entry names)", report.Pods, wantPods)
	}
	if len(report.Containers) != 0 {
		t.Errorf("Containers should be empty for k8s-native, got %v", report.Containers)
	}
	if report.Note == "" {
		t.Errorf("Note should be populated")
	}

	// The teardown is list-only: the ServiceSet is NOT deleted.
	var after sandboxv1alpha1.ServiceSet
	if err := c.Get(context.Background(), client.ObjectKey{Name: "env-abc", Namespace: "ns"}, &after); err != nil {
		t.Errorf("ServiceSet should still exist after a list-only teardown: %v", err)
	}
}

func TestK8sNativeEngineTeardown_NoServiceSetIsEmpty(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = sandboxv1alpha1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	eng := NewEngineTeardown("k8s-native", c, "ns", "never-applied")
	report, err := eng.Teardown(context.Background())
	if err != nil {
		t.Fatalf("Teardown on a missing ServiceSet: %v", err)
	}
	if len(report.Pods) != 0 {
		t.Errorf("Pods = %v, want empty (no ServiceSet)", report.Pods)
	}
	if report.Engine != "k8s-native" {
		t.Errorf("Engine = %q, want k8s-native", report.Engine)
	}
}
