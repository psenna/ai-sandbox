package lifecycle

import (
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
)

// runningSince returns the LastTransitionTime of the PodReady condition when
// it is True: the moment the environment MOST RECENTLY entered Running.
// status.startedAt cannot be used for this -- it is documented as the FIRST
// start and does not reset across freeze/wake cycles. This depends on the
// PodReady condition semantics in conditions.go (True/PodRunning <=> phase
// Running, exactly) -- do not loosen those semantics without revisiting this.
func runningSince(env v1alpha1.SandboxEnvironment) *time.Time {
	c := apimeta.FindStatusCondition(env.Status.Conditions, ConditionPodReady)
	if c == nil || c.Status != metav1.ConditionTrue {
		return nil
	}
	t := c.LastTransitionTime.Time
	return &t
}

// waitingSince returns the LastTransitionTime of the Frozen condition when it
// is True: the moment the environment most recently entered Waiting.
func waitingSince(env v1alpha1.SandboxEnvironment) *time.Time {
	c := apimeta.FindStatusCondition(env.Status.Conditions, ConditionFrozen)
	if c == nil || c.Status != metav1.ConditionTrue {
		return nil
	}
	t := c.LastTransitionTime.Time
	return &t
}

// firedTimeoutReason evaluates the three timeout clocks in precedence order
// -- Total, then Running, then Waiting -- and returns the reason for the
// first one that has fired ("elapsed >= limit"), or ok=false if none has. A
// zero duration disables the corresponding timeout. Running only applies in
// phase Running; Waiting only applies in phase Waiting; Total applies in
// every non-terminal phase and never pauses. If the phase-scoped clock's
// anchor condition (PodReady/Frozen) is absent, that timeout fails open
// (does not fire) -- Total still bounds the environment.
func firedTimeoutReason(env v1alpha1.SandboxEnvironment, facts ClusterFacts, now time.Time, phase v1alpha1.Phase) (string, bool) {
	if t := facts.Timeouts.Total; t > 0 && now.Sub(env.CreationTimestamp.Time) >= t {
		return ReasonTotalTimeoutExceeded, true
	}
	if phase == v1alpha1.PhaseRunning {
		if t := facts.Timeouts.Running; t > 0 {
			if since := runningSince(env); since != nil && now.Sub(*since) >= t {
				return ReasonRunningTimeoutExceeded, true
			}
		}
	}
	if phase == v1alpha1.PhaseWaiting {
		if t := facts.Timeouts.Waiting; t > 0 {
			if since := waitingSince(env); since != nil && now.Sub(*since) >= t {
				return ReasonWaitingTimeoutExceeded, true
			}
		}
	}
	return "", false
}

// timeoutFired evaluates firedTimeoutReason and, if a timeout has fired,
// builds the Failed Decision for it: PodReady and Ready both go
// False/<reason>, the slot is released, and finishedAt is set. Ordering in
// Next's pipeline (called AFTER agent/pod terminal signals, BEFORE the
// per-phase freeze-trigger logic) is what makes completion win a race with a
// timeout, and a timeout win a race with a fresh freeze trigger.
func timeoutFired(env v1alpha1.SandboxEnvironment, facts ClusterFacts, now time.Time, phase v1alpha1.Phase) (Decision, bool) {
	reason, ok := firedTimeoutReason(env, facts, now, phase)
	if !ok {
		return Decision{}, false
	}
	d := newBuilder(env, facts, now).
		phase(v1alpha1.PhaseFailed).
		slot(false).
		patch(StatusPatch{SetFinishedAt: newMetaTime(now)}).
		cond(ConditionPodReady, metav1.ConditionFalse, reason, "").
		cond(ConditionReady, metav1.ConditionFalse, reason, "").
		build()
	return d, true
}

// nextTimeoutDeadline returns the earliest deadline among the timeouts that
// apply to phase (mirroring firedTimeoutReason's applicability rules), used
// to compute RequeueAfter so the reconciler wakes up promptly once a timeout
// crosses its boundary rather than waiting for some unrelated event.
func nextTimeoutDeadline(env v1alpha1.SandboxEnvironment, facts ClusterFacts, phase v1alpha1.Phase) (time.Time, bool) {
	var deadline time.Time
	have := false
	consider := func(t time.Time) {
		if !have || t.Before(deadline) {
			deadline = t
			have = true
		}
	}

	if t := facts.Timeouts.Total; t > 0 {
		consider(env.CreationTimestamp.Add(t))
	}
	if phase == v1alpha1.PhaseRunning {
		if t := facts.Timeouts.Running; t > 0 {
			if since := runningSince(env); since != nil {
				consider(since.Add(t))
			}
		}
	}
	if phase == v1alpha1.PhaseWaiting {
		if t := facts.Timeouts.Waiting; t > 0 {
			if since := waitingSince(env); since != nil {
				consider(since.Add(t))
			}
		}
		// The probe evaluator's backoff window (#30): the reconciler must wake
		// at NextEligibleAt so the next evaluation runs promptly rather than
		// waiting for the waiting-timeout deadline (up to 24h away) or some
		// unrelated event. consider() keeps the earlier of the two.
		if pa := env.Status.ProbeAttempt; pa != nil && pa.NextEligibleAt != nil {
			consider(pa.NextEligibleAt.Time)
		}
	}
	return deadline, have
}

// computeRequeueAfter is the single place that decides ctrl.Result.RequeueAfter
// for every Decision, regardless of which code path in Next produced it:
// terminal phases requeue only until the archive is written (or not at all
// once it is); an unresolved class requeues on a fixed short interval; every
// other non-terminal phase requeues at the nearest applicable timeout
// deadline, clamped to [MinRequeueAfter, MaxRequeueAfter], or not at all if
// no timeout applies.
func computeRequeueAfter(phase v1alpha1.Phase, env v1alpha1.SandboxEnvironment, facts ClusterFacts, now time.Time) time.Duration {
	if isTerminal(phase) {
		if !facts.ArchiveWritten {
			return TerminalArchiveRequeue
		}
		return 0
	}
	if !facts.ClassResolved {
		return ClassUnresolvedRequeue
	}
	deadline, ok := nextTimeoutDeadline(env, facts, phase)
	if !ok {
		return 0
	}
	return clampDuration(deadline.Sub(now), MinRequeueAfter, MaxRequeueAfter)
}

func clampDuration(d, lo, hi time.Duration) time.Duration {
	switch {
	case d < lo:
		return lo
	case d > hi:
		return hi
	default:
		return d
	}
}

func newMetaTime(t time.Time) *metav1.Time {
	m := metav1.NewTime(t).Rfc3339Copy()
	return &m
}
