package lifecycle

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
)

// nonTerminalPhases are the six phases the Total timeout applies to.
var nonTerminalPhases = []v1alpha1.Phase{
	v1alpha1.PhasePending, v1alpha1.PhaseReady, v1alpha1.PhaseRestoring,
	v1alpha1.PhaseRunning, v1alpha1.PhaseFreezing, v1alpha1.PhaseWaiting,
}

// TestNext_Timeouts_Firing covers the basic "each timeout fires with its own
// reason" cases, plus the exact boundary.
func TestNext_Timeouts_Firing(t *testing.T) {
	t.Run("Total fires in every non-terminal phase", func(t *testing.T) {
		for _, phase := range nonTerminalPhases {
			t.Run(string(phase), func(t *testing.T) {
				f := baseFacts()
				env := envAt(phase, withCreationTime(fixedNow.Add(-f.Timeouts.Total)))

				d := Next(env, f, fixedNow)

				if d.Phase != v1alpha1.PhaseFailed {
					t.Fatalf("Phase = %s, want Failed", d.Phase)
				}
				for _, ct := range []string{ConditionPodReady, ConditionReady} {
					c := findCond(d, ct)
					if c == nil || c.Status != metav1.ConditionFalse || c.Reason != ReasonTotalTimeoutExceeded {
						t.Errorf("%s = %+v, want False/%s", ct, c, ReasonTotalTimeoutExceeded)
					}
				}
				if d.SlotWanted {
					t.Errorf("SlotWanted = true, want false")
				}
				if d.StatusPatch.SetFinishedAt == nil {
					t.Errorf("SetFinishedAt is nil, want set")
				}
			})
		}
	})

	t.Run("Running timeout fires", func(t *testing.T) {
		f := baseFacts()
		env := envAt(v1alpha1.PhaseRunning,
			withCondition(ConditionPodReady, metav1.ConditionTrue, ReasonPodRunning, fixedNow.Add(-f.Timeouts.Running)))

		d := Next(env, f, fixedNow)

		if d.Phase != v1alpha1.PhaseFailed {
			t.Fatalf("Phase = %s, want Failed", d.Phase)
		}
		if c := findCond(d, ConditionReady); c == nil || c.Reason != ReasonRunningTimeoutExceeded {
			t.Errorf("Ready = %+v, want reason %s", c, ReasonRunningTimeoutExceeded)
		}
	})

	t.Run("Waiting timeout fires", func(t *testing.T) {
		f := baseFacts()
		env := envAt(v1alpha1.PhaseWaiting, withWaitFor(aWaitFor()),
			withCondition(ConditionFrozen, metav1.ConditionTrue, ReasonWaitDeclared, fixedNow.Add(-f.Timeouts.Waiting)))

		d := Next(env, f, fixedNow)

		if d.Phase != v1alpha1.PhaseFailed {
			t.Fatalf("Phase = %s, want Failed", d.Phase)
		}
		if c := findCond(d, ConditionReady); c == nil || c.Reason != ReasonWaitingTimeoutExceeded {
			t.Errorf("Ready = %+v, want reason %s", c, ReasonWaitingTimeoutExceeded)
		}
	})

	t.Run("Total boundary", func(t *testing.T) {
		f := baseFacts()
		cases := []struct {
			name      string
			elapsed   time.Duration
			wantFired bool
		}{
			{"just under", f.Timeouts.Total - time.Nanosecond, false},
			{"exactly at limit", f.Timeouts.Total, true},
			{"just over", f.Timeouts.Total + time.Nanosecond, true},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				env := envAt(v1alpha1.PhasePending, withCreationTime(fixedNow.Add(-c.elapsed)))
				d := Next(env, f, fixedNow)
				fired := d.Phase == v1alpha1.PhaseFailed
				if fired != c.wantFired {
					t.Errorf("fired = %v, want %v (phase=%s)", fired, c.wantFired, d.Phase)
				}
			})
		}
	})
}

// TestNext_Timeouts_Precedence covers which signal wins when more than one
// applies at once: Total outranks the phase-scoped timeouts, a genuine
// completion signal outranks a timeout, and a timeout outranks a freshly
// declared freeze trigger.
func TestNext_Timeouts_Precedence(t *testing.T) {
	t.Run("Total beats Running", func(t *testing.T) {
		f := baseFacts()
		env := envAt(v1alpha1.PhaseRunning,
			withCreationTime(fixedNow.Add(-f.Timeouts.Total-time.Hour)),
			withCondition(ConditionPodReady, metav1.ConditionTrue, ReasonPodRunning, fixedNow.Add(-f.Timeouts.Running-time.Hour)))

		d := Next(env, f, fixedNow)

		if d.Phase != v1alpha1.PhaseFailed {
			t.Fatalf("Phase = %s, want Failed", d.Phase)
		}
		if c := findCond(d, ConditionReady); c == nil || c.Reason != ReasonTotalTimeoutExceeded {
			t.Errorf("Ready = %+v, want reason %s (Total precedence)", c, ReasonTotalTimeoutExceeded)
		}
	})

	t.Run("Total beats Waiting", func(t *testing.T) {
		f := baseFacts()
		env := envAt(v1alpha1.PhaseWaiting, withWaitFor(aWaitFor()),
			withCreationTime(fixedNow.Add(-f.Timeouts.Total-time.Hour)),
			withCondition(ConditionFrozen, metav1.ConditionTrue, ReasonWaitDeclared, fixedNow.Add(-f.Timeouts.Waiting-time.Hour)))

		d := Next(env, f, fixedNow)

		if d.Phase != v1alpha1.PhaseFailed {
			t.Fatalf("Phase = %s, want Failed", d.Phase)
		}
		if c := findCond(d, ConditionReady); c == nil || c.Reason != ReasonTotalTimeoutExceeded {
			t.Errorf("Ready = %+v, want reason %s (Total precedence)", c, ReasonTotalTimeoutExceeded)
		}
	})

	t.Run("completion beats timeout", func(t *testing.T) {
		f := baseFacts()
		f.AgentDone = true
		env := envAt(v1alpha1.PhaseRunning,
			withCondition(ConditionPodReady, metav1.ConditionTrue, ReasonPodRunning, fixedNow.Add(-f.Timeouts.Running-time.Hour)))

		d := Next(env, f, fixedNow)

		if d.Phase != v1alpha1.PhaseDone {
			t.Fatalf("Phase = %s, want Done", d.Phase)
		}
		// facts.AgentDone selects the more specific ReasonAgentReportedSuccess
		// (mirroring readyOnFailure's facts.AgentFailed check) -- the point
		// of this case is Phase == Done, not Failed, despite the running
		// timeout also having fired.
		if c := findCond(d, ConditionReady); c == nil || c.Reason != ReasonAgentReportedSuccess {
			t.Errorf("Ready = %+v, want reason %s", c, ReasonAgentReportedSuccess)
		}
	})

	t.Run("timeout beats freeze trigger", func(t *testing.T) {
		f := baseFacts()
		f.AgentWaitDeclared = true
		env := envAt(v1alpha1.PhaseRunning,
			withCondition(ConditionPodReady, metav1.ConditionTrue, ReasonPodRunning, fixedNow.Add(-f.Timeouts.Running-time.Hour)))

		d := Next(env, f, fixedNow)

		if d.Phase != v1alpha1.PhaseFailed {
			t.Fatalf("Phase = %s, want Failed", d.Phase)
		}
		if c := findCond(d, ConditionReady); c == nil || c.Reason != ReasonRunningTimeoutExceeded {
			t.Errorf("Ready = %+v, want reason %s", c, ReasonRunningTimeoutExceeded)
		}
	})
}

// TestNext_Timeouts_ClockAnchors covers how the phase-scoped clocks are
// anchored: they reset on each fresh entry into their phase (derived from
// the PodReady/Frozen condition's LastTransitionTime), fail open when that
// anchor is absent, can be disabled with a zero duration, and Freezing only
// ever evaluates Total.
func TestNext_Timeouts_ClockAnchors(t *testing.T) {
	t.Run("running clock resets on wake: recent restart does not fire despite old creation", func(t *testing.T) {
		f := baseFacts()
		f.Timeouts.Running = 6 * time.Hour
		env := envAt(v1alpha1.PhaseRunning,
			withCreationTime(fixedNow.Add(-2*time.Hour)), // Total (72h) doesn't fire
			withCondition(ConditionPodReady, metav1.ConditionTrue, ReasonPodRunning, fixedNow.Add(-1*time.Minute)))

		d := Next(env, f, fixedNow)

		if d.Phase != v1alpha1.PhaseRunning {
			t.Fatalf("Phase = %s, want Running (no timeout should fire)", d.Phase)
		}
	})

	t.Run("running timeout fails open with no PodReady condition, but Total still bounds", func(t *testing.T) {
		f := baseFacts()
		env := envAt(v1alpha1.PhaseRunning, withCreationTime(fixedNow.Add(-1*time.Hour)))
		// No PodReady condition present at all -> running timeout doesn't fire.
		d := Next(env, f, fixedNow)
		if d.Phase != v1alpha1.PhaseRunning {
			t.Fatalf("Phase = %s, want Running (running timeout fails open)", d.Phase)
		}

		// Second sub-case: creation time old enough for Total to fire
		// separately -- Total still fires even though running has no anchor.
		env2 := envAt(v1alpha1.PhaseRunning, withCreationTime(fixedNow.Add(-f.Timeouts.Total)))
		d2 := Next(env2, f, fixedNow)
		if d2.Phase != v1alpha1.PhaseFailed {
			t.Fatalf("Phase = %s, want Failed (Total fires)", d2.Phase)
		}
		if c := findCond(d2, ConditionReady); c == nil || c.Reason != ReasonTotalTimeoutExceeded {
			t.Errorf("Ready = %+v, want reason %s", c, ReasonTotalTimeoutExceeded)
		}
	})

	t.Run("waiting clock resets on refreeze: recent refreeze does not fire despite old creation", func(t *testing.T) {
		f := baseFacts()
		f.Timeouts.Waiting = 24 * time.Hour
		env := envAt(v1alpha1.PhaseWaiting, withWaitFor(aWaitFor()),
			withCreationTime(fixedNow.Add(-2*time.Hour)), // Total (72h) doesn't fire
			withCondition(ConditionFrozen, metav1.ConditionTrue, ReasonWaitDeclared, fixedNow.Add(-1*time.Minute)))

		d := Next(env, f, fixedNow)

		if d.Phase != v1alpha1.PhaseWaiting {
			t.Fatalf("Phase = %s, want Waiting (no timeout should fire)", d.Phase)
		}
	})

	t.Run("zero duration disables the running timeout", func(t *testing.T) {
		f := baseFacts()
		f.Timeouts.Running = 0
		env := envAt(v1alpha1.PhaseRunning,
			withCondition(ConditionPodReady, metav1.ConditionTrue, ReasonPodRunning, fixedNow.Add(-1000*time.Hour)))

		d := Next(env, f, fixedNow)

		if d.Phase != v1alpha1.PhaseRunning {
			t.Fatalf("Phase = %s, want Running (timeout disabled)", d.Phase)
		}
	})

	t.Run("Freezing phase: only Total evaluated", func(t *testing.T) {
		f := baseFacts()
		// Running/Waiting would fire if they applied to Freezing -- they don't.
		env := envAt(v1alpha1.PhaseFreezing,
			withCreationTime(fixedNow.Add(-f.Timeouts.Total)),
			withCondition(ConditionPodReady, metav1.ConditionTrue, ReasonPodRunning, fixedNow.Add(-f.Timeouts.Running-time.Hour)),
			withCondition(ConditionFrozen, metav1.ConditionTrue, ReasonWaitDeclared, fixedNow.Add(-f.Timeouts.Waiting-time.Hour)))

		d := Next(env, f, fixedNow)

		if d.Phase != v1alpha1.PhaseFailed {
			t.Fatalf("Phase = %s, want Failed", d.Phase)
		}
		if c := findCond(d, ConditionReady); c == nil || c.Reason != ReasonTotalTimeoutExceeded {
			t.Errorf("Ready = %+v, want reason %s", c, ReasonTotalTimeoutExceeded)
		}

		t.Run("nothing exceeded stays Freezing", func(t *testing.T) {
			env2 := envAt(v1alpha1.PhaseFreezing)
			d2 := Next(env2, f, fixedNow)
			if d2.Phase != v1alpha1.PhaseFreezing {
				t.Fatalf("Phase = %s, want Freezing", d2.Phase)
			}
		})
	})

	t.Run("terminal phases: Total wildly exceeded, phase stays terminal", func(t *testing.T) {
		f := baseFacts()
		for _, phase := range []v1alpha1.Phase{v1alpha1.PhaseDone, v1alpha1.PhaseFailed} {
			t.Run(string(phase), func(t *testing.T) {
				env := envAt(phase, withCreationTime(fixedNow.Add(-1000*time.Hour)))
				d := Next(env, f, fixedNow)
				if d.Phase != phase {
					t.Errorf("Phase = %s, want %s (terminal stickiness)", d.Phase, phase)
				}
			})
		}
	})
}

// TestNext_Timeouts_RequeueAfter covers computeRequeueAfter's three cases:
// clamped to the nearest applicable deadline, a fixed short interval while
// the class is unresolved, and no further requeue once a terminal object is
// archived.
func TestNext_Timeouts_RequeueAfter(t *testing.T) {
	t.Run("RequeueAfter equals clamped deadline for an applicable timeout", func(t *testing.T) {
		f := baseFacts()
		env := envAt(v1alpha1.PhasePending, withCreationTime(fixedNow.Add(-1*time.Hour)))
		d := Next(env, f, fixedNow)

		deadline, ok := nextTimeoutDeadline(env, f, d.Phase)
		if !ok {
			t.Fatalf("nextTimeoutDeadline: no deadline found")
		}
		want := clampDuration(deadline.Sub(fixedNow), MinRequeueAfter, MaxRequeueAfter)
		if d.RequeueAfter != want {
			t.Errorf("RequeueAfter = %v, want %v", d.RequeueAfter, want)
		}
	})

	t.Run("RequeueAfter is ClassUnresolvedRequeue when class unresolved", func(t *testing.T) {
		f := baseFacts()
		f.ClassResolved = false
		env := envAt(v1alpha1.PhasePending)
		d := Next(env, f, fixedNow)
		if d.RequeueAfter != ClassUnresolvedRequeue {
			t.Errorf("RequeueAfter = %v, want %v", d.RequeueAfter, ClassUnresolvedRequeue)
		}
	})

	t.Run("RequeueAfter is 0 for an already-archived terminal object", func(t *testing.T) {
		f := baseFacts()
		f.ArchiveWritten = true
		env := envAt(v1alpha1.PhaseDone)
		d := Next(env, f, fixedNow)
		if d.RequeueAfter != 0 {
			t.Errorf("RequeueAfter = %v, want 0", d.RequeueAfter)
		}
	})

	t.Run("RequeueAfter honors NextEligibleAt when no timeout deadline applies", func(t *testing.T) {
		// Waiting with a waitFor but no Frozen condition: the waiting-timeout
		// clock has no anchor and Total is ~71h away, so without #30's
		// NextEligibleAt the reconciler would have no reason to wake. The
		// probe's backoff window must drive the requeue.
		f := baseFacts()
		env := envAt(v1alpha1.PhaseWaiting, withWaitFor(aWaitFor()))
		next := metav1.NewTime(fixedNow.Add(5 * time.Minute))
		env.Status.ProbeAttempt = &v1alpha1.ProbeAttemptStatus{NextEligibleAt: &next}
		d := Next(env, f, fixedNow)
		if want := clampDuration(5*time.Minute, MinRequeueAfter, MaxRequeueAfter); d.RequeueAfter != want {
			t.Errorf("RequeueAfter = %v, want %v", d.RequeueAfter, want)
		}
	})

	t.Run("RequeueAfter uses the earlier of NextEligibleAt and the waiting-timeout deadline", func(t *testing.T) {
		// Frozen anchored 23h50m ago -> waiting deadline fixedNow+10m.
		// NextEligibleAt at fixedNow+5m is earlier and must win.
		f := baseFacts()
		env := envAt(v1alpha1.PhaseWaiting, withWaitFor(aWaitFor()),
			withCondition(ConditionFrozen, metav1.ConditionTrue, ReasonWaitDeclared, fixedNow.Add(-23*time.Hour-50*time.Minute)))
		next := metav1.NewTime(fixedNow.Add(5 * time.Minute))
		env.Status.ProbeAttempt = &v1alpha1.ProbeAttemptStatus{NextEligibleAt: &next}
		d := Next(env, f, fixedNow)
		if want := clampDuration(5*time.Minute, MinRequeueAfter, MaxRequeueAfter); d.RequeueAfter != want {
			t.Errorf("RequeueAfter = %v, want %v", d.RequeueAfter, want)
		}
	})

	t.Run("RequeueAfter uses the waiting-timeout deadline when it is earlier than NextEligibleAt", func(t *testing.T) {
		// Same Frozen anchor -> waiting deadline fixedNow+10m; NextEligibleAt
		// at fixedNow+20m is later and must lose.
		f := baseFacts()
		env := envAt(v1alpha1.PhaseWaiting, withWaitFor(aWaitFor()),
			withCondition(ConditionFrozen, metav1.ConditionTrue, ReasonWaitDeclared, fixedNow.Add(-23*time.Hour-50*time.Minute)))
		next := metav1.NewTime(fixedNow.Add(20 * time.Minute))
		env.Status.ProbeAttempt = &v1alpha1.ProbeAttemptStatus{NextEligibleAt: &next}
		d := Next(env, f, fixedNow)
		if want := clampDuration(10*time.Minute, MinRequeueAfter, MaxRequeueAfter); d.RequeueAfter != want {
			t.Errorf("RequeueAfter = %v, want %v", d.RequeueAfter, want)
		}
	})
}
