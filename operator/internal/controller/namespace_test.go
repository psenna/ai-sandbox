package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/psenna/ai-sandbox/operator/internal/render"
)

// mustCreateLabelledNamespace creates (or reuses) namespace name with the
// pod-security.kubernetes.io/enforce label set to enforce ("" omits the
// label entirely).
func mustCreateLabelledNamespace(t *testing.T, name, enforce string) {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if enforce != "" {
		ns.Labels = map[string]string{render.PodSecurityEnforceLabel: enforce}
	}
	if err := k8s.Create(ctx, ns); err != nil {
		if apierrors.IsAlreadyExists(err) {
			// Namespace labels are mutable; update in place so a re-run of
			// this test (or a shared envtest namespace) still gets the
			// enforce level this test case needs.
			var existing corev1.Namespace
			if err := k8s.Get(ctx, client.ObjectKey{Name: name}, &existing); err != nil {
				t.Fatalf("getting existing namespace %s: %v", name, err)
			}
			if enforce != "" {
				if existing.Labels == nil {
					existing.Labels = map[string]string{}
				}
				existing.Labels[render.PodSecurityEnforceLabel] = enforce
			} else {
				delete(existing.Labels, render.PodSecurityEnforceLabel)
			}
			if err := k8s.Update(ctx, &existing); err != nil {
				t.Fatalf("updating namespace %s labels: %v", name, err)
			}
			return
		}
		t.Fatalf("creating namespace %s: %v", name, err)
	}
}

func TestNamespacePodSecurityEnforce_LabelPresent(t *testing.T) {
	mustCreateLabelledNamespace(t, "pss-labelled", render.PodSecurityRestricted)
	r := &Reconciler{Client: k8s}
	got := r.namespacePodSecurityEnforce(ctx, "pss-labelled")
	if got != render.PodSecurityRestricted {
		t.Errorf("namespacePodSecurityEnforce = %q, want %q", got, render.PodSecurityRestricted)
	}
}

func TestNamespacePodSecurityEnforce_LabelAbsent(t *testing.T) {
	mustCreateLabelledNamespace(t, "pss-unlabelled", "")
	r := &Reconciler{Client: k8s}
	got := r.namespacePodSecurityEnforce(ctx, "pss-unlabelled")
	if got != "" {
		t.Errorf("namespacePodSecurityEnforce = %q, want \"\"", got)
	}
}

func TestNamespacePodSecurityEnforce_NamespaceMissing(t *testing.T) {
	r := &Reconciler{Client: k8s}
	got := r.namespacePodSecurityEnforce(ctx, "pss-does-not-exist")
	if got != "" {
		t.Errorf("namespacePodSecurityEnforce for a missing namespace = %q, want \"\" (fails open)", got)
	}
}
