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

// TestApply_PhaseHistory covers the #32 phase-transition history: exactly one
// entry is appended when the phase changes, nothing when it doesn't, the At
// timestamp mirrors the Decision's condition timestamps, and the Reason comes
// from the Ready summary condition.
func TestApply_PhaseHistory(t *testing.T) {
	t.Run("no entry on an unchanged phase", func(t *testing.T) {
		env := envAt(v1alpha1.PhasePending)
		d := Next(env, baseFacts(), fixedNow) // resources not ready: Pending stays
		s := Apply(&env, d)
		if len(s.PhaseHistory) != 0 {
			t.Errorf("PhaseHistory = %+v, want empty on an unchanged phase", s.PhaseHistory)
		}
	})

	t.Run("appends a transition on a phase change, and only once", func(t *testing.T) {
		env := envAt(v1alpha1.PhasePending)
		f := baseFacts()
		f.ResourcesReady = true
		d := Next(env, f, fixedNow)
		if d.Phase != v1alpha1.PhaseReady {
			t.Fatalf("setup: want Ready, got %s", d.Phase)
		}
		s := Apply(&env, d)
		if len(s.PhaseHistory) != 1 {
			t.Fatalf("PhaseHistory = %+v, want 1 entry", s.PhaseHistory)
		}
		e := s.PhaseHistory[0]
		if e.Phase != v1alpha1.PhaseReady {
			t.Errorf("entry phase = %s, want Ready", e.Phase)
		}
		wantAt := d.Conditions[0].LastTransitionTime
		if !e.At.Equal(&wantAt) {
			t.Errorf("entry at = %v, want condition timestamp %v", e.At, wantAt)
		}
		wantReason := findCond(d, ConditionReady).Reason
		if e.Reason != wantReason {
			t.Errorf("entry reason = %q, want Ready condition reason %q", e.Reason, wantReason)
		}

		// Idempotent re-apply on the settled phase appends nothing.
		env.Status = *s
		s2 := Apply(&env, d)
		if len(s2.PhaseHistory) != 1 {
			t.Errorf("PhaseHistory grew on a settled re-apply: %d entries", len(s2.PhaseHistory))
		}
	})

	t.Run("first reconcile on a fresh environment records the Pending entry", func(t *testing.T) {
		env := envAt("") // no status written yet
		d := Next(env, baseFacts(), fixedNow)
		s := Apply(&env, d)
		if len(s.PhaseHistory) != 1 || s.PhaseHistory[0].Phase != v1alpha1.PhasePending {
			t.Errorf("PhaseHistory = %+v, want a single Pending entry", s.PhaseHistory)
		}
	})

	t.Run("terminal transition records the terminal phase", func(t *testing.T) {
		env := envAt(v1alpha1.PhaseRunning)
		f := baseFacts()
		f.AgentDone = true
		d := Next(env, f, fixedNow)
		if d.Phase != v1alpha1.PhaseDone {
			t.Fatalf("setup: want Done, got %s", d.Phase)
		}
		s := Apply(&env, d)
		if len(s.PhaseHistory) != 1 || s.PhaseHistory[0].Phase != v1alpha1.PhaseDone {
			t.Errorf("PhaseHistory = %+v, want a single Done entry", s.PhaseHistory)
		}
	})
}

// TestApply_PhaseHistoryCappedAtCRDLimit drives an environment through more
// freeze/wake cycles than the CRD's maxItems allows and proves the list never
// exceeds it. Without the cap the API server would reject every subsequent
// status update as invalid, wedging the environment permanently -- no further
// transitions, no terminal archive, and a delete finalizer that can never be
// released. The newest entries are the ones kept.
func TestApply_PhaseHistoryCappedAtCRDLimit(t *testing.T) {
	env := envAt(v1alpha1.PhaseWaiting, withSnapshot(aSnapshot()))
	// The real wake cycle: Waiting -> Ready -> Restoring -> Running ->
	// Freezing -> Waiting, five transitions per lap. Enough laps to overshoot
	// the cap several times over.
	cycle := []v1alpha1.Phase{
		v1alpha1.PhaseReady, v1alpha1.PhaseRestoring, v1alpha1.PhaseRunning,
		v1alpha1.PhaseFreezing, v1alpha1.PhaseWaiting,
	}
	var last v1alpha1.Phase
	for i := 0; i < 200; i++ {
		next := cycle[i%len(cycle)]
		d := newBuilder(env, baseFacts(), fixedNow.Add(time.Duration(i)*time.Minute)).
			phase(next).
			build()
		env.Status = *Apply(&env, d)
		last = next
	}

	if got := len(env.Status.PhaseHistory); got != v1alpha1.MaxPhaseHistoryEntries {
		t.Fatalf("len(PhaseHistory) = %d, want exactly the cap %d", got, v1alpha1.MaxPhaseHistoryEntries)
	}
	if got := env.Status.PhaseHistory[len(env.Status.PhaseHistory)-1].Phase; got != last {
		t.Errorf("newest entry = %s, want the most recent transition %s", got, last)
	}
	// Truncation must never leave a zero timestamp or two identical adjacent
	// phases behind: storage.RunRecord.Validate rejects both, and an
	// unwritable run record would fail the archive Job.
	for i, e := range env.Status.PhaseHistory {
		if e.At.IsZero() {
			t.Errorf("PhaseHistory[%d] has a zero timestamp", i)
		}
		if i > 0 && e.Phase == env.Status.PhaseHistory[i-1].Phase {
			t.Errorf("PhaseHistory[%d] duplicates the previous phase %s", i, e.Phase)
		}
	}
}

// TestApply_TerminalPhaseSetOnce verifies status.terminalPhase is recorded on
// the first terminal transition and never overwritten, so the freeze-detour
// return (nextWaiting) always knows which terminal phase to go back to.
func TestApply_TerminalPhaseSetOnce(t *testing.T) {
	env := envAt(v1alpha1.PhaseRunning)
	f := baseFacts()
	f.AgentDone = true
	d := Next(env, f, fixedNow)
	if d.StatusPatch.SetTerminalPhase != v1alpha1.PhaseDone {
		t.Fatalf("setup: SetTerminalPhase = %q, want Done", d.StatusPatch.SetTerminalPhase)
	}
	s := Apply(&env, d)
	if s.TerminalPhase != v1alpha1.PhaseDone {
		t.Fatalf("TerminalPhase = %q, want Done", s.TerminalPhase)
	}

	// A later decision (e.g. a spurious Freezing detour patch) must not
	// overwrite it.
	env.Status = *s
	d2 := newBuilder(env, baseFacts(), fixedNow).
		phase(v1alpha1.PhaseFreezing).
		slot(true).
		patch(StatusPatch{SetTerminalPhase: v1alpha1.PhaseFailed}).
		build()
	s2 := Apply(&env, d2)
	if s2.TerminalPhase != v1alpha1.PhaseDone {
		t.Errorf("TerminalPhase overwritten to %q, want Done (set-once)", s2.TerminalPhase)
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
