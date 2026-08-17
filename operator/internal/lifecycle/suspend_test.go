package lifecycle

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"testing"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
)

// TestNext_SuspendCrossProduct exercises every phase x {suspend
// true/false} x {waitFor nil/non-nil} and asserts suspend's effect (or lack
// of one) matches the documented per-phase rules, independent of
// TestNext_Transitions' single-purpose cases.
func TestNext_SuspendCrossProduct(t *testing.T) {
	type tc struct {
		name         string
		phase        v1alpha1.Phase
		suspend      bool
		waitFor      *v1alpha1.WaitForStatus
		extraFacts   func(*ClusterFacts)
		wantPhase    v1alpha1.Phase
		wantSlotWant bool
	}

	cases := []tc{
		{name: "Pending, suspend=false", phase: v1alpha1.PhasePending, suspend: false, wantPhase: v1alpha1.PhasePending, wantSlotWant: false},
		{name: "Pending, suspend=true", phase: v1alpha1.PhasePending, suspend: true, wantPhase: v1alpha1.PhasePending, wantSlotWant: false},
		{name: "Ready, suspend=false", phase: v1alpha1.PhaseReady, suspend: false, wantPhase: v1alpha1.PhaseReady, wantSlotWant: true},
		{name: "Ready, suspend=true", phase: v1alpha1.PhaseReady, suspend: true, wantPhase: v1alpha1.PhaseReady, wantSlotWant: false},
		{
			name: "Restoring, suspend=false", phase: v1alpha1.PhaseRestoring, suspend: false,
			extraFacts: func(f *ClusterFacts) { f.SlotGranted = true },
			wantPhase:  v1alpha1.PhaseRestoring, wantSlotWant: true,
		},
		{
			name: "Restoring, suspend=true", phase: v1alpha1.PhaseRestoring, suspend: true,
			extraFacts: func(f *ClusterFacts) { f.SlotGranted = true },
			wantPhase:  v1alpha1.PhaseReady, wantSlotWant: false,
		},
		{
			name: "Running, suspend=false", phase: v1alpha1.PhaseRunning, suspend: false,
			wantPhase: v1alpha1.PhaseRunning, wantSlotWant: true,
		},
		{
			name: "Running, suspend=true", phase: v1alpha1.PhaseRunning, suspend: true,
			wantPhase: v1alpha1.PhaseFreezing, wantSlotWant: true,
		},
		{
			name: "Freezing, suspend=false, snapshot done, no pod", phase: v1alpha1.PhaseFreezing, suspend: false,
			extraFacts: func(f *ClusterFacts) { f.SnapshotComplete = true },
			wantPhase:  v1alpha1.PhaseWaiting, wantSlotWant: false,
		},
		{
			name: "Freezing, suspend=true, snapshot done, no pod", phase: v1alpha1.PhaseFreezing, suspend: true,
			extraFacts: func(f *ClusterFacts) { f.SnapshotComplete = true },
			wantPhase:  v1alpha1.PhaseWaiting, wantSlotWant: false,
		},
		{
			name: "Waiting nil waitFor, suspend=false", phase: v1alpha1.PhaseWaiting, suspend: false, waitFor: nil,
			wantPhase: v1alpha1.PhaseReady, wantSlotWant: true,
		},
		{
			name: "Waiting nil waitFor, suspend=true", phase: v1alpha1.PhaseWaiting, suspend: true, waitFor: nil,
			wantPhase: v1alpha1.PhaseWaiting, wantSlotWant: false,
		},
		{
			name: "Waiting with waitFor, suspend=false", phase: v1alpha1.PhaseWaiting, suspend: false, waitFor: aWaitFor(),
			wantPhase: v1alpha1.PhaseWaiting, wantSlotWant: false,
		},
		{
			name: "Waiting with waitFor, suspend=true", phase: v1alpha1.PhaseWaiting, suspend: true, waitFor: aWaitFor(),
			wantPhase: v1alpha1.PhaseWaiting, wantSlotWant: false,
		},
		{name: "Done ignores suspend", phase: v1alpha1.PhaseDone, suspend: true, wantPhase: v1alpha1.PhaseDone, wantSlotWant: false},
		{name: "Failed ignores suspend", phase: v1alpha1.PhaseFailed, suspend: true, wantPhase: v1alpha1.PhaseFailed, wantSlotWant: false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := baseFacts()
			if c.extraFacts != nil {
				c.extraFacts(&f)
			}
			env := envAt(c.phase, withSuspend(c.suspend), withWaitFor(c.waitFor))
			d := Next(env, f, fixedNow)
			if d.Phase != c.wantPhase {
				t.Errorf("Phase = %s, want %s", d.Phase, c.wantPhase)
			}
			if d.SlotWanted != c.wantSlotWant {
				t.Errorf("SlotWanted = %v, want %v", d.SlotWanted, c.wantSlotWant)
			}
		})
	}
}

// TestSuspendRoundTrip_FromRunning drives a mutable environment through a
// full suspend/resume cycle starting from Running, applying each Decision
// via Apply between calls exactly as the real reconciler would, and asserts
// the documented phase sequence and final counters.
func TestSuspendRoundTrip_FromRunning(t *testing.T) {
	env := envAt(v1alpha1.PhaseRunning, withCondition(ConditionPodReady, metav1.ConditionTrue, ReasonPodRunning, fixedNow))
	facts := baseFacts()

	step := func(mutateFacts func(*ClusterFacts)) v1alpha1.Phase {
		f := facts
		if mutateFacts != nil {
			mutateFacts(&f)
		}
		d := Next(env, f, fixedNow)
		s := Apply(&env, d)
		env.Status = *s
		return d.Phase
	}

	// Running -> Freezing (suspend requested).
	env.Spec.Suspend = true
	if got := step(nil); got != v1alpha1.PhaseFreezing {
		t.Fatalf("after suspend request: phase = %s, want Freezing", got)
	}

	// Freezing -> Freezing (snapshot in progress).
	if got := step(nil); got != v1alpha1.PhaseFreezing {
		t.Fatalf("mid-freeze: phase = %s, want Freezing", got)
	}

	// Freezing -> Freezing (snapshot complete, pod still terminating).
	if got := step(func(f *ClusterFacts) { f.SnapshotComplete = true; f.PodExists = true }); got != v1alpha1.PhaseFreezing {
		t.Fatalf("pod terminating: phase = %s, want Freezing", got)
	}

	// Freezing -> Waiting (snapshot complete, pod gone).
	if got := step(func(f *ClusterFacts) { f.SnapshotComplete = true }); got != v1alpha1.PhaseWaiting {
		t.Fatalf("after freeze completes: phase = %s, want Waiting", got)
	}
	if env.Status.FreezeCount != 1 {
		t.Fatalf("freezeCount = %d, want 1", env.Status.FreezeCount)
	}
	if !env.Status.Slot.Granted == false { //nolint:staticcheck // explicit for readability
	}
	if env.Status.Slot.Granted {
		t.Fatalf("slot should have been released entering Waiting")
	}

	// Waiting self-loop while still suspended.
	if got := step(nil); got != v1alpha1.PhaseWaiting {
		t.Fatalf("still suspended: phase = %s, want Waiting", got)
	}

	// Clear suspend -> Ready.
	env.Spec.Suspend = false
	if got := step(nil); got != v1alpha1.PhaseReady {
		t.Fatalf("after clearing suspend: phase = %s, want Ready", got)
	}

	// Ready -> Restoring once granted a slot again.
	if got := step(func(f *ClusterFacts) { f.SlotGranted = true }); got != v1alpha1.PhaseRestoring {
		t.Fatalf("after slot grant: phase = %s, want Restoring", got)
	}

	// Restoring -> Running once the pod is back up: wakeCount increments
	// because status.Snapshot is non-nil (taken during the freeze above, by
	// #28's future code -- simulated here directly since that subsystem
	// doesn't exist yet).
	env.Status.Snapshot = aSnapshot()
	if got := step(func(f *ClusterFacts) {
		f.SlotGranted = true
		f.PodExists = true
		f.PodPhase = corev1.PodRunning
		f.PodReady = true
	}); got != v1alpha1.PhaseRunning {
		t.Fatalf("after restore completes: phase = %s, want Running", got)
	}

	if env.Status.FreezeCount != 1 {
		t.Errorf("freezeCount = %d, want 1", env.Status.FreezeCount)
	}
	if env.Status.WakeCount != 1 {
		t.Errorf("wakeCount = %d, want 1", env.Status.WakeCount)
	}
}

// TestSuspendRoundTrip_FromWaiting drives a probe-originated Waiting
// environment through a suspend/resume cycle, asserting waitFor is cleared
// exactly once and the environment lands back in Ready.
func TestSuspendRoundTrip_FromWaiting(t *testing.T) {
	env := envAt(v1alpha1.PhaseWaiting,
		withWaitFor(aWaitFor()),
		withCondition(ConditionFrozen, metav1.ConditionTrue, ReasonWaitDeclared, fixedNow))
	facts := baseFacts()

	step := func(mutateFacts func(*ClusterFacts)) v1alpha1.Phase {
		f := facts
		if mutateFacts != nil {
			mutateFacts(&f)
		}
		d := Next(env, f, fixedNow)
		s := Apply(&env, d)
		env.Status = *s
		return d.Phase
	}

	// Suspend while already waiting on a probe: self-loop, waitFor untouched.
	env.Spec.Suspend = true
	if got := step(nil); got != v1alpha1.PhaseWaiting {
		t.Fatalf("suspended while waiting: phase = %s, want Waiting", got)
	}
	if env.Status.WaitFor == nil {
		t.Fatalf("waitFor should not be cleared while suspended and unsatisfied")
	}

	// Probe becomes satisfied while suspended: recorded, but does not
	// resume (per plan: "does not resume the run").
	if got := step(func(f *ClusterFacts) { f.WaitProbeSatisfied = true }); got != v1alpha1.PhaseWaiting {
		t.Fatalf("probe satisfied but still suspended: phase = %s, want Waiting", got)
	}

	// Clear suspend: now the (already satisfied) probe resumes it.
	env.Spec.Suspend = false
	waitForBefore := env.Status.WaitFor
	if waitForBefore == nil {
		t.Fatalf("waitFor unexpectedly nil before resume")
	}
	if got := step(func(f *ClusterFacts) { f.WaitProbeSatisfied = true }); got != v1alpha1.PhaseReady {
		t.Fatalf("after resume: phase = %s, want Ready", got)
	}
	if env.Status.WaitFor != nil {
		t.Fatalf("waitFor should be cleared exactly once on resume, still set: %+v", env.Status.WaitFor)
	}
}
