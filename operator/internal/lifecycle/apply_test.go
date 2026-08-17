package lifecycle

import (
	"testing"
	"time"

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
)

func TestApply_LastTransitionTimeOnlyOnStatusFlip(t *testing.T) {
	env := envAt(v1alpha1.PhaseRestoring)
	t0 := fixedNow
	t1 := fixedNow.Add(5 * time.Minute)

	d := newBuilder(env, baseFacts(), t0).
		phase(v1alpha1.PhaseRestoring).
		cond(ConditionPodReady, metav1.ConditionTrue, ReasonPodRunning, "").
		build()

	s1 := Apply(&env, d)
	c1 := apimeta.FindStatusCondition(s1.Conditions, ConditionPodReady)
	if c1 == nil {
		t.Fatalf("PodReady condition missing after first Apply")
	}
	firstTransition := c1.LastTransitionTime

	// Re-apply the SAME decision (status unchanged) at a later time: the
	// stamped-into-the-decision LastTransitionTime is t0, but even if it were
	// re-stamped with t1, SetStatusCondition preserves the existing
	// LastTransitionTime when the condition doesn't flip. Simulate the
	// reconciler's real flow: Next stamps conditions with "now" every call,
	// so call build() again with now=t1 to prove Apply/SetStatusCondition,
	// not builder, is what's responsible for preservation.
	env.Status = *s1
	d2 := newBuilder(env, baseFacts(), t1).
		phase(v1alpha1.PhaseRestoring).
		cond(ConditionPodReady, metav1.ConditionTrue, ReasonPodRunning, "").
		build()
	s2 := Apply(&env, d2)
	c2 := apimeta.FindStatusCondition(s2.Conditions, ConditionPodReady)
	if c2 == nil {
		t.Fatalf("PodReady condition missing after second Apply")
	}
	if !c2.LastTransitionTime.Equal(&firstTransition) {
		t.Errorf("LastTransitionTime changed on a non-flipping re-apply: %v -> %v", firstTransition, c2.LastTransitionTime)
	}

	// Now flip the status: LastTransitionTime must update.
	env.Status = *s2
	d3 := newBuilder(env, baseFacts(), t1).
		phase(v1alpha1.PhaseRestoring).
		cond(ConditionPodReady, metav1.ConditionFalse, ReasonPodPending, "").
		build()
	s3 := Apply(&env, d3)
	c3 := apimeta.FindStatusCondition(s3.Conditions, ConditionPodReady)
	if c3 == nil {
		t.Fatalf("PodReady condition missing after third Apply")
	}
	if c3.LastTransitionTime.Equal(&firstTransition) {
		t.Errorf("LastTransitionTime did not update on a genuine status flip")
	}
}

func TestApply_Idempotent(t *testing.T) {
	env := envAt(v1alpha1.PhaseRunning)
	facts := baseFacts()
	d := Next(env, facts, fixedNow)

	s1 := Apply(&env, d)
	env.Status = *s1
	s2 := Apply(&env, d)

	if !apiequality.Semantic.DeepEqual(s1, s2) {
		t.Errorf("Apply is not idempotent:\n%#v\n%#v", s1, s2)
	}
	if s1.FreezeCount != s2.FreezeCount {
		t.Errorf("FreezeCount changed across idempotent Apply: %d -> %d", s1.FreezeCount, s2.FreezeCount)
	}
	if s1.WakeCount != s2.WakeCount {
		t.Errorf("WakeCount changed across idempotent Apply: %d -> %d", s1.WakeCount, s2.WakeCount)
	}

	// Regression for the double-increment hazard: drive env through a real
	// Freezing -> Waiting transition (IncrementFreezeCount=true), Apply it,
	// then -- as the real reconciler does -- recompute Next on the SETTLED
	// state and Apply that. The freshly recomputed decision must not carry
	// IncrementFreezeCount a second time, because Next only sets it on the
	// transition itself (nextFreezing's terminal branch), not on Waiting's
	// steady-state self-loop. This is what "Apply is idempotent when fed its
	// own prior output" means in practice: Apply itself has no state to
	// dedupe against, so the guarantee comes from Next never re-emitting a
	// one-shot patch for an already-settled phase (the reconciler's
	// generation/phase staleness guard in status.go is the other, independent
	// half of this protection -- see TestWriteStatus_ConcurrentReconciles).
	freezingEnv := envAt(v1alpha1.PhaseFreezing, withWaitFor(aWaitFor()))
	fFreeze := baseFacts()
	fFreeze.SnapshotComplete = true

	dFreeze := Next(freezingEnv, fFreeze, fixedNow)
	if !dFreeze.StatusPatch.IncrementFreezeCount {
		t.Fatalf("setup: expected the Freezing->Waiting transition to set IncrementFreezeCount")
	}
	after1 := Apply(&freezingEnv, dFreeze)
	if after1.FreezeCount != 1 {
		t.Fatalf("first Apply: FreezeCount = %d, want 1", after1.FreezeCount)
	}
	freezingEnv.Status = *after1

	dSettled := Next(freezingEnv, fFreeze, fixedNow.Add(time.Minute))
	if dSettled.StatusPatch.IncrementFreezeCount {
		t.Fatalf("recomputed decision on settled Waiting state unexpectedly carries IncrementFreezeCount")
	}
	after2 := Apply(&freezingEnv, dSettled)
	if after2.FreezeCount != 1 {
		t.Errorf("second Apply (recomputed on settled state) changed FreezeCount: %d, want 1", after2.FreezeCount)
	}
}

func TestApply_ObservedGeneration(t *testing.T) {
	env := envAt(v1alpha1.PhasePending, withGeneration(5))
	d := Next(env, baseFacts(), fixedNow)
	s := Apply(&env, d)
	if s.ObservedGeneration != 5 {
		t.Errorf("ObservedGeneration = %d, want 5", s.ObservedGeneration)
	}
}

func TestApply_PreservesForeignConditions(t *testing.T) {
	env := envAt(v1alpha1.PhasePending)
	apimeta.SetStatusCondition(&env.Status.Conditions, metav1.Condition{
		Type:               "NetworkPolicyEnforced",
		Status:             metav1.ConditionTrue,
		Reason:             "PolicyApplied",
		LastTransitionTime: metav1.NewTime(fixedNow),
	})

	d := Next(env, baseFacts(), fixedNow)
	s := Apply(&env, d)

	c := apimeta.FindStatusCondition(s.Conditions, "NetworkPolicyEnforced")
	if c == nil {
		t.Fatalf("foreign condition NetworkPolicyEnforced was dropped")
	}
	if c.Status != metav1.ConditionTrue || c.Reason != "PolicyApplied" {
		t.Errorf("foreign condition mutated: %+v", c)
	}
}

func TestApply_SetOnceTimestamps(t *testing.T) {
	t0 := metav1.NewTime(fixedNow.Add(-time.Hour)).Rfc3339Copy()
	t1 := metav1.NewTime(fixedNow).Rfc3339Copy()

	t.Run("StartedAt", func(t *testing.T) {
		env := envAt(v1alpha1.PhaseRestoring, withStartedAt(t0.Time))
		d := newBuilder(env, baseFacts(), fixedNow).
			phase(v1alpha1.PhaseRunning).
			patch(StatusPatch{SetStartedAt: &t1}).
			build()
		s := Apply(&env, d)
		if s.StartedAt == nil || !s.StartedAt.Equal(&t0) {
			t.Errorf("StartedAt = %v, want unchanged %v", s.StartedAt, t0)
		}
	})

	t.Run("QueuedSince", func(t *testing.T) {
		env := envAt(v1alpha1.PhaseReady, withQueuedSince(t0.Time))
		d := newBuilder(env, baseFacts(), fixedNow).
			phase(v1alpha1.PhaseReady).
			patch(StatusPatch{SetQueuedSince: &t1}).
			build()
		s := Apply(&env, d)
		if s.QueuedSince == nil || !s.QueuedSince.Equal(&t0) {
			t.Errorf("QueuedSince = %v, want unchanged %v", s.QueuedSince, t0)
		}
	})

	t.Run("FinishedAt", func(t *testing.T) {
		env := envAt(v1alpha1.PhaseDone, withFinishedAt(t0.Time))
		d := newBuilder(env, baseFacts(), fixedNow).
			phase(v1alpha1.PhaseDone).
			patch(StatusPatch{SetFinishedAt: &t1}).
			build()
		s := Apply(&env, d)
		if s.FinishedAt == nil || !s.FinishedAt.Equal(&t0) {
			t.Errorf("FinishedAt = %v, want unchanged %v", s.FinishedAt, t0)
		}
	})
}

func TestApply_SlotReleaseOnly(t *testing.T) {
	t.Run("release zeroes the slot", func(t *testing.T) {
		env := envAt(v1alpha1.PhaseRunning)
		env.Status.Slot = v1alpha1.SlotStatus{Granted: true, LeaseName: "lease-1"}
		d := newBuilder(env, baseFacts(), fixedNow).phase(v1alpha1.PhaseFreezing).slot(false).build()
		s := Apply(&env, d)
		if s.Slot != (v1alpha1.SlotStatus{}) {
			t.Errorf("Slot = %+v, want zero value", s.Slot)
		}
	})

	t.Run("wanting a slot never grants it", func(t *testing.T) {
		env := envAt(v1alpha1.PhaseReady)
		env.Status.Slot = v1alpha1.SlotStatus{}
		d := newBuilder(env, baseFacts(), fixedNow).phase(v1alpha1.PhaseReady).slot(true).build()
		s := Apply(&env, d)
		if s.Slot.Granted {
			t.Errorf("Slot.Granted = true, want false (Apply never grants)")
		}
	})
}

// TestApply_PreservesUnrelatedCounters guards against a regression where
// Apply's counter handling (currently s.FreezeCount++ / s.WakeCount++ on the
// deep-copied prior status) is accidentally rewritten as an assignment that
// would clobber a pre-existing count instead of incrementing it.
func TestApply_PreservesUnrelatedCounters(t *testing.T) {
	env := envAt(v1alpha1.PhaseRunning, withFreezeCount(5), withWakeCount(3))
	d := Next(env, baseFacts(), fixedNow) // steady-state Running: no counter change

	s := Apply(&env, d)
	if s.FreezeCount != 5 {
		t.Errorf("FreezeCount = %d, want unchanged 5", s.FreezeCount)
	}
	if s.WakeCount != 3 {
		t.Errorf("WakeCount = %d, want unchanged 3", s.WakeCount)
	}
}

func TestApply_ConditionOrderStable(t *testing.T) {
	envs := []v1alpha1.SandboxEnvironment{
		envAt(v1alpha1.PhasePending),
		envAt(v1alpha1.PhaseReady, withSuspend(true)),
		envAt(v1alpha1.PhaseRunning),
		envAt(v1alpha1.PhaseWaiting, withWaitFor(aWaitFor())),
		envAt(v1alpha1.PhaseDone),
	}
	for _, env := range envs {
		d := Next(env, baseFacts(), fixedNow)
		if ok, detail := allConditionsPresent(d); !ok {
			t.Errorf("phase=%s: %s", env.Status.Phase, detail)
		}
	}
}
