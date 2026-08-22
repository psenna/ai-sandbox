package controller

import (
	"testing"

	"k8s.io/apimachinery/pkg/types"

	sandboxv1alpha1 "github.com/psenna/ai-sandbox/operator/api/v1alpha1"
	"github.com/psenna/ai-sandbox/operator/internal/metrics"
)

// TestMetricsCollector_NeedLeaderElectionIsFalse pins the load-bearing
// property spelled out in metricscollector.go's own doc comment: this
// Runnable must NEVER be leader-elected, or every non-leader replica's own
// /metrics endpoint would report permanently-zero gauges.
func TestMetricsCollector_NeedLeaderElectionIsFalse(t *testing.T) {
	c := &MetricsCollector{}
	if c.NeedLeaderElection() {
		t.Fatal("MetricsCollector.NeedLeaderElection() = true, want false (see the doc comment on why this is load-bearing)")
	}
}

// TestMetricsCollector_RunOnce_CountsPhasesAndSlots creates environments in
// a mix of phases (including one left at its zero-value phase, which must
// count as Pending) and asserts RunOnce's gauges land at the expected
// values, using a private registry (metrics.New(), not metrics.Default) so
// this test's absolute values are never polluted by another test sharing
// the process-wide Default.
func TestMetricsCollector_RunOnce_CountsPhasesAndSlots(t *testing.T) {
	mustCreateClass(t)
	ns := testNamespace(t)

	// Two Running (occupy), one Ready-and-queued (candidate), one
	// zero-value phase (must default to Pending, matching lifecycle.Next's
	// own defaulting -- see metricscollector.go's RunOnce doc comment).
	mustCreateEnvIn(t, ns, "running-a", 0)
	mustSetPhase(t, types.NamespacedName{Namespace: ns, Name: "running-a"}, sandboxv1alpha1.PhaseRunning, true, fixedStart)
	mustCreateEnvIn(t, ns, "running-b", 0)
	mustSetPhase(t, types.NamespacedName{Namespace: ns, Name: "running-b"}, sandboxv1alpha1.PhaseRunning, true, fixedStart)
	mustCreateEnvIn(t, ns, "queued", 0)
	mustSetPhase(t, types.NamespacedName{Namespace: ns, Name: "queued"}, sandboxv1alpha1.PhaseReady, false, fixedStart)
	mustCreateEnvIn(t, ns, "fresh", 0) // never had mustSetPhase called: Status.Phase == ""

	m := metrics.New()
	c := &MetricsCollector{Client: k8s, Capacity: 5, Namespace: ns, Metrics: m}
	if err := c.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	reg := newTestRegistry(t, m)
	if got := mustMetricValue(t, reg, "sandbox_operator_environments", map[string]string{"phase": "Running"}); got != 2 {
		t.Errorf("environments{phase=Running} = %v, want 2", got)
	}
	if got := mustMetricValue(t, reg, "sandbox_operator_environments", map[string]string{"phase": "Pending"}); got != 1 {
		t.Errorf("environments{phase=Pending} = %v, want 1 (the zero-value-phase environment must default to Pending)", got)
	}
	if got := mustMetricValue(t, reg, "sandbox_operator_environments", map[string]string{"phase": "Ready"}); got != 1 {
		t.Errorf("environments{phase=Ready} = %v, want 1", got)
	}
	if got := mustMetricValue(t, reg, "sandbox_operator_slot_capacity", nil); got != 5 {
		t.Errorf("slot_capacity = %v, want 5", got)
	}
	if got := mustMetricValue(t, reg, "sandbox_operator_slots_used", nil); got != 2 {
		t.Errorf("slots_used = %v, want 2 (the two Running environments)", got)
	}
	if got := mustMetricValue(t, reg, "sandbox_operator_queue_depth", nil); got != 1 {
		t.Errorf("queue_depth = %v, want 1 (the Ready, not-yet-granted environment)", got)
	}
}
