package controller

import (
	"context"
	"fmt"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sandboxv1alpha1 "github.com/psenna/ai-sandbox/operator/api/v1alpha1"
	"github.com/psenna/ai-sandbox/operator/internal/lifecycle"
)

// newScopedResourceReconciler mirrors newResourceReconciler, additionally
// setting WatchNamespace so observeQueuePosition's List is scoped
// consistently with a SlotScheduler{Namespace: ns} in the same test --
// otherwise it would see every other test's leftover fixtures too.
func newScopedResourceReconciler(t *testing.T, clk *fakeClock, ns string) *Reconciler {
	t.Helper()
	r := &Reconciler{
		Client:               k8s,
		Clock:                clk.Now,
		ClassSecretNamespace: "default",
		ClusterID:            "test",
		WatchNamespace:       ns,
	}
	r.Observe = r.observeCluster
	return r
}

func TestReconcile_QueuedEnvironmentGetsQueuePositionCondition(t *testing.T) {
	mustCreateClass(t)
	ns := testNamespace(t)
	clk := newFakeClock(fixedStart)
	r := newScopedResourceReconciler(t, clk, ns)

	names := []string{"high", "mid", "low"}
	priorities := []int32{30, 20, 10}
	keys := make([]types.NamespacedName, 3)
	for i, name := range names {
		mustCreateEnvIn(t, ns, name, priorities[i])
		keys[i] = types.NamespacedName{Namespace: ns, Name: name}
		// Two reconciles: the first creates child resources (ActionEnsureResources)
		// but observes them as not-yet-existing; the second observes them
		// present and transitions Pending -> Ready. Mirrors the pattern in
		// resources_test.go.
		reconcileOnce(t, r, keys[i])
		reconcileOnce(t, r, keys[i])
		env := getEnv(t, keys[i])
		if env.Status.Phase != sandboxv1alpha1.PhaseReady {
			t.Fatalf("%s: Phase = %s, want Ready after setup reconciles", name, env.Status.Phase)
		}
	}

	s := newSlotScheduler(t, 1, clk)
	s.Namespace = ns
	stats, err := s.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if stats.Admitted != 1 {
		t.Fatalf("Admitted = %d, want 1", stats.Admitted)
	}

	// Reconcile the two non-admitted environments -- this is what populates
	// the queue-position message via observeQueuePosition. Order doesn't
	// matter: Occupies() already excludes the granted "high" environment
	// from the candidate set regardless of whether its own reconcile (which
	// advances Ready -> Restoring) has happened yet.
	reconcileOnce(t, r, keys[1]) // mid
	reconcileOnce(t, r, keys[2]) // low
	reconcileOnce(t, r, keys[0]) // high: picks up its own grant via the ordinary observation path

	highEnv := getEnv(t, keys[0])
	midEnv := getEnv(t, keys[1])
	lowEnv := getEnv(t, keys[2])

	highCond := findCondition(highEnv, lifecycle.ConditionScheduled)
	if highCond == nil {
		t.Fatal("high: Scheduled condition missing")
	}
	if highCond.Status != metav1.ConditionTrue || highCond.Reason != lifecycle.ReasonSlotGranted {
		t.Errorf("high Scheduled = %s/%s, want True/SlotGranted", highCond.Status, highCond.Reason)
	}
	if highCond.Message != "" {
		t.Errorf("high Scheduled Message = %q, want empty", highCond.Message)
	}

	midCond := findCondition(midEnv, lifecycle.ConditionScheduled)
	if midCond == nil {
		t.Fatal("mid: Scheduled condition missing")
	}
	if midCond.Status != metav1.ConditionFalse || midCond.Reason != lifecycle.ReasonQueued {
		t.Errorf("mid Scheduled = %s/%s, want False/Queued", midCond.Status, midCond.Reason)
	}
	if midCond.Message != "queued at position 1 of 2" {
		t.Errorf("mid Scheduled Message = %q, want %q", midCond.Message, "queued at position 1 of 2")
	}

	lowCond := findCondition(lowEnv, lifecycle.ConditionScheduled)
	if lowCond == nil {
		t.Fatal("low: Scheduled condition missing")
	}
	if lowCond.Status != metav1.ConditionFalse || lowCond.Reason != lifecycle.ReasonQueued {
		t.Errorf("low Scheduled = %s/%s, want False/Queued", lowCond.Status, lowCond.Reason)
	}
	if lowCond.Message != "queued at position 2 of 2" {
		t.Errorf("low Scheduled Message = %q, want %q", lowCond.Message, "queued at position 2 of 2")
	}
}

// listFailClient wraps a real client.Client, injecting an error on every
// List of a SandboxEnvironmentList (what observeQueuePosition issues) while
// forwarding every other call (Get, Status().Update, etc.) to the real
// client -- so this exercises exactly the "queue position List fails"
// path without disturbing the rest of the reconcile.
type listFailClient struct {
	client.Client
}

func (c *listFailClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if _, ok := list.(*sandboxv1alpha1.SandboxEnvironmentList); ok {
		return fmt.Errorf("simulated list failure")
	}
	return c.Client.List(ctx, list, opts...)
}

func TestObserveQueuePosition_ListFailureIsAdvisoryOnly(t *testing.T) {
	mustCreateClass(t)
	ns := testNamespace(t)
	clk := newFakeClock(fixedStart)

	mustCreateEnvIn(t, ns, "solo", 10)
	key := types.NamespacedName{Namespace: ns, Name: "solo"}

	r := &Reconciler{
		Client:               &listFailClient{Client: k8s},
		Clock:                clk.Now,
		ClassSecretNamespace: "default",
		ClusterID:            "test",
		WatchNamespace:       ns,
	}
	r.Observe = r.observeCluster

	reconcileOnce(t, r, key) // Pending: creates resources
	reconcileOnce(t, r, key) // Pending -> Ready
	reconcileOnce(t, r, key) // Ready self-loop: this is the one that hits observeQueuePosition's List

	env := getEnv(t, key)
	if env.Status.Phase != sandboxv1alpha1.PhaseReady {
		t.Fatalf("Phase = %s, want Ready", env.Status.Phase)
	}
	cond := findCondition(env, lifecycle.ConditionScheduled)
	if cond == nil {
		t.Fatal("Scheduled condition missing")
	}
	if cond.Status != metav1.ConditionFalse || cond.Reason != lifecycle.ReasonQueued {
		t.Errorf("Scheduled = %s/%s, want False/Queued (a List failure must not fail the reconcile)", cond.Status, cond.Reason)
	}
	if cond.Message != "" {
		t.Errorf("Scheduled Message = %q, want empty (a List failure must never produce a stale/incorrect position)", cond.Message)
	}
}
