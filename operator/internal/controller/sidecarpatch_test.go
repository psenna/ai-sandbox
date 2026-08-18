package controller

import (
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
	"github.com/psenna/ai-sandbox/operator/internal/render"
	"github.com/psenna/ai-sandbox/operator/internal/sandboxctl"
)

// TestSidecarStatusPatch_ScopedToOwnEnvironment runs internal/sandboxctl's
// REAL status-patch code path (not a hand-rolled Patch call) under the
// rendered sidecar ServiceAccount's identity, against two environments: A's
// identity patches A's status successfully, and the identical call against
// B comes back Forbidden. Asserted, not assumed. This is the positive leg
// of the acceptance criterion "the sidecar cannot patch another
// environment" -- rbac_test.go's TestSidecarServiceAccountAuthorization and
// TestSidecarServiceAccountImpersonatedCalls already prove the negative leg
// with hand-rolled calls; this proves the real sandboxctl.Store code
// exercises the exact same RBAC boundary correctly.
func TestSidecarStatusPatch_ScopedToOwnEnvironment(t *testing.T) {
	mustCreateClass(t)
	envA := mustCreateEnv(t, "sidecar-patch-a")
	envB := mustCreateEnv(t, "sidecar-patch-b")

	r := newResourceReconciler(t, newFakeClock(fixedStart))
	reconcileOnce(t, r, types.NamespacedName{Namespace: envA.Namespace, Name: envA.Name})

	user := "system:serviceaccount:" + envA.Namespace + ":" + render.ChildNames(envA.Name).ServiceAccount
	imp := impersonatingClient(t, user, serviceAccountGroups(envA.Namespace))

	probe := sandboxctl.WaitProbe{
		Type:   v1alpha1.WaitTypeNotBefore,
		Reason: "waiting on the clock",
		Params: map[string]string{"duration": "1h"},
	}
	now := time.Now()

	t.Run("DeclareWait on own environment succeeds", func(t *testing.T) {
		storeA := sandboxctl.NewStore(imp, envA.Namespace, envA.Name)
		if err := storeA.DeclareWait(ctx, probe, now); err != nil {
			t.Fatalf("DeclareWait on own environment: %v", err)
		}

		var got v1alpha1.SandboxEnvironment
		if err := k8s.Get(ctx, types.NamespacedName{Namespace: envA.Namespace, Name: envA.Name}, &got); err != nil {
			t.Fatalf("Get (admin client): %v", err)
		}
		if got.Status.WaitFor == nil {
			t.Fatal("Status.WaitFor is nil after DeclareWait")
		}
		if got.Status.WaitFor.Type != probe.Type || got.Status.WaitFor.Reason != probe.Reason {
			t.Errorf("WaitFor = %+v, want Type=%q Reason=%q", got.Status.WaitFor, probe.Type, probe.Reason)
		}
		if got.Status.WaitFor.Params["duration"] != "1h" {
			t.Errorf("WaitFor.Params = %v, want duration=1h", got.Status.WaitFor.Params)
		}
		if got.Status.WaitFor.DeclaredAt == nil {
			t.Error("WaitFor.DeclaredAt is nil, want stamped")
		}
	})

	t.Run("DeclareWait against a foreign environment is Forbidden", func(t *testing.T) {
		storeB := sandboxctl.NewStore(imp, envB.Namespace, envB.Name)
		err := storeB.DeclareWait(ctx, probe, now)
		if !apierrors.IsForbidden(err) {
			t.Fatalf("DeclareWait against a FOREIGN environment: err = %v, want Forbidden", err)
		}
	})

	t.Run("ReportDone against a foreign environment is Forbidden", func(t *testing.T) {
		storeB := sandboxctl.NewStore(imp, envB.Namespace, envB.Name)
		_, err := storeB.ReportDone(ctx, sandboxctl.Result{Outcome: v1alpha1.AgentOutcomeSucceeded, Message: "done"}, now)
		if !apierrors.IsForbidden(err) {
			t.Fatalf("ReportDone against a FOREIGN environment: err = %v, want Forbidden", err)
		}
	})

	t.Run("ReportDone on own environment succeeds (different env, no prior wait)", func(t *testing.T) {
		envC := mustCreateEnv(t, "sidecar-patch-c")
		rc := newResourceReconciler(t, newFakeClock(fixedStart))
		reconcileOnce(t, rc, types.NamespacedName{Namespace: envC.Namespace, Name: envC.Name})

		userC := "system:serviceaccount:" + envC.Namespace + ":" + render.ChildNames(envC.Name).ServiceAccount
		impC := impersonatingClient(t, userC, serviceAccountGroups(envC.Namespace))
		storeC := sandboxctl.NewStore(impC, envC.Namespace, envC.Name)

		if _, err := storeC.ReportDone(ctx, sandboxctl.Result{Outcome: v1alpha1.AgentOutcomeFailed, Message: "boom"}, now); err != nil {
			t.Fatalf("ReportDone on own environment: %v", err)
		}

		var got v1alpha1.SandboxEnvironment
		if err := k8s.Get(ctx, types.NamespacedName{Namespace: envC.Namespace, Name: envC.Name}, &got); err != nil {
			t.Fatalf("Get (admin client): %v", err)
		}
		if got.Status.AgentResult == nil || got.Status.AgentResult.Outcome != v1alpha1.AgentOutcomeFailed {
			t.Errorf("AgentResult = %+v, want Outcome=Failed", got.Status.AgentResult)
		}
	})
}
