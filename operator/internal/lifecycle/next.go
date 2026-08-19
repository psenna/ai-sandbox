package lifecycle

import (
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
)

// Next computes the Decision for env given observed facts, at wall-clock now.
// It is pure: no I/O, no client, fully deterministic for a given (env, facts,
// now) triple. See doc.go for the package's import contract.
//
// Pipeline (first match wins at each stage):
//  1. Terminal phases (Done/Failed) are sticky: see terminal().
//  2. Agent/pod completion or failure, only in Restoring/Running: see
//     agentOrPodTerminal(). Checked before timeouts so a completion that
//     lands exactly on a timeout boundary wins.
//  3. Timeouts: see timeoutFired(). Checked before the class-resolved gate
//     and before per-phase logic, so a timeout beats both an unresolved
//     class and a fresh freeze trigger landing on the same boundary.
//  4. An unresolved SandboxClass holds the phase: see hold().
//  5. Otherwise, dispatch to the phase-specific function.
//
// RequeueAfter is computed uniformly, after the above, from the *decided*
// phase -- see computeRequeueAfter.
func Next(env v1alpha1.SandboxEnvironment, facts ClusterFacts, now time.Time) Decision {
	phase := env.Status.Phase
	if phase == "" {
		phase = v1alpha1.PhasePending
	}

	d := decide(env, facts, now, phase)
	d.Actions = withEnsureResources(d.Actions, d.Phase, facts)
	d.RequeueAfter = computeRequeueAfter(d.Phase, env, facts, now)
	return d
}

// withEnsureResources prepends ActionEnsureResources for every non-terminal
// phase with a resolved class, so child objects are re-applied (and drift
// corrected) on every pass, not only while Pending. Terminal phases (Done,
// Failed) are excluded -- see TestNext_TerminalIsSticky's invariant that a
// terminal Decision performs no action other than Archive. An unresolved
// class is excluded: there is nothing to render from yet.
func withEnsureResources(actions []Action, phase v1alpha1.Phase, facts ClusterFacts) []Action {
	if isTerminal(phase) || !facts.ClassResolved {
		return actions
	}
	for _, a := range actions {
		if a == ActionEnsureResources {
			return actions // already present (e.g. nextPending's own emission)
		}
	}
	return append([]Action{ActionEnsureResources}, actions...)
}

func decide(env v1alpha1.SandboxEnvironment, facts ClusterFacts, now time.Time, phase v1alpha1.Phase) Decision {
	if isTerminal(phase) {
		return terminal(env, facts, now, phase)
	}
	if d, ok := agentOrPodTerminal(env, facts, now, phase); ok {
		return d
	}
	if d, ok := timeoutFired(env, facts, now, phase); ok {
		return d
	}
	if !facts.ClassResolved {
		return hold(env, facts, now, phase)
	}
	return dispatchPhase(env, facts, now, phase)
}

func dispatchPhase(env v1alpha1.SandboxEnvironment, facts ClusterFacts, now time.Time, phase v1alpha1.Phase) Decision {
	switch phase {
	case v1alpha1.PhasePending:
		return nextPending(env, facts, now)
	case v1alpha1.PhaseReady:
		return nextReady(env, facts, now)
	case v1alpha1.PhaseRestoring:
		return nextRestoring(env, facts, now)
	case v1alpha1.PhaseRunning:
		return nextRunning(env, facts, now)
	case v1alpha1.PhaseFreezing:
		return nextFreezing(env, facts, now)
	case v1alpha1.PhaseWaiting:
		return nextWaiting(env, facts, now)
	default:
		// Unknown/empty phase (shouldn't happen; Status.Phase is defaulted
		// to Pending by the API and by Next itself above) re-enters Pending
		// rather than panicking or getting stuck.
		return nextPending(env, facts, now)
	}
}

// terminal handles an already-terminal environment (Done/Failed). The phase
// never changes -- terminal phases are sticky. Conditions are still
// recomputed every call so Archived can flip True once #32 writes the
// archive; the other five condition types have phase/facts-derived defaults
// that reproduce the same value every call for a terminal phase (or, for
// PodReady/Ready on Failed, carry the original failure reason forward via
// carryForwardOr), so this never perturbs an already-settled status.
func terminal(env v1alpha1.SandboxEnvironment, facts ClusterFacts, now time.Time, phase v1alpha1.Phase) Decision {
	b := newBuilder(env, facts, now).
		phase(phase).
		slot(false).
		patch(StatusPatch{SetFinishedAt: newMetaTime(now), SetArchiveURI: facts.ArchiveURI})
	if !facts.ArchiveWritten {
		b.act(ActionArchive)
	}
	return b.build()
}

// agentOrPodTerminal detects agent/pod completion or failure. It only
// applies in Restoring or Running; every other phase returns ok=false
// immediately. PodReady and Ready conditions are left to their
// phase/facts-derived defaults (builder.go), which already implement the
// exact same reason selection described in conditions.go's tables for a
// Failed/Done phase -- no explicit cond() calls are needed here.
func agentOrPodTerminal(env v1alpha1.SandboxEnvironment, facts ClusterFacts, now time.Time, phase v1alpha1.Phase) (Decision, bool) {
	if phase != v1alpha1.PhaseRestoring && phase != v1alpha1.PhaseRunning {
		return Decision{}, false
	}
	var d Decision
	switch {
	case facts.AgentFailed:
		d = terminalOutcome(env, facts, now, v1alpha1.PhaseFailed, false)
	case facts.AgentDone:
		d = terminalOutcome(env, facts, now, v1alpha1.PhaseDone, true)
	case facts.PodObserved && facts.PodFailure != nil:
		d = terminalOutcome(env, facts, now, v1alpha1.PhaseFailed, false)
	case facts.PodObserved && facts.PodPhase == corev1.PodFailed:
		d = terminalOutcome(env, facts, now, v1alpha1.PhaseFailed, false)
	case facts.PodObserved && facts.PodPhase == corev1.PodSucceeded:
		d = terminalOutcome(env, facts, now, v1alpha1.PhaseDone, true)
	default:
		return Decision{}, false
	}
	// Count a wake that reaches a terminal outcome (Done or Failed) while
	// still Restoring, before nextRestoring could flip Restoring->Running.
	// nextRestoring increments WakeCount at the Restoring->Running transition
	// (gated on Status.Snapshot != nil), but that transition requires the
	// pod to be observed Running+Ready -- and a fast-completing agent can
	// finish (AgentResult set by the sidecar's /v1/done, or the restore init
	// container failing) before the controller ever observes PodReady, so
	// agentOrPodTerminal fires Done/Failed straight out of Restoring and the
	// Restoring->Running increment is skipped entirely, leaving WakeCount at
	// 0 on a real wake. This closes that gap: the only way to be in Restoring
	// with a populated Snapshot is a wake (a fresh start has no Snapshot and
	// never counts), so this fires exactly once per wake, on the path
	// nextRestoring's increment missed. It never double-counts: once
	// Restoring->Running fires the env never returns to Restoring, so this
	// branch (phase==Restoring) and nextRestoring's increment are mutually
	// exclusive across a single wake. Same Snapshot != nil gate as
	// nextRestoring, so the never-frozen first start stays WakeCount 0.
	if phase == v1alpha1.PhaseRestoring && env.Status.Snapshot != nil {
		d.StatusPatch.IncrementWakeCount = true
	}
	return d, true
}

// terminalOutcome builds the common shape shared by every agentOrPodTerminal
// branch: slot released, finishedAt set-once, and -- only for Done, never
// for Failed (the pod is kept around for log retrieval; #32 owns cleanup) --
// a pod deletion.
func terminalOutcome(env v1alpha1.SandboxEnvironment, facts ClusterFacts, now time.Time, phase v1alpha1.Phase, deletePod bool) Decision {
	b := newBuilder(env, facts, now).
		phase(phase).
		slot(false).
		patch(StatusPatch{SetFinishedAt: newMetaTime(now)})
	if deletePod {
		b.act(ActionDeletePod)
	}
	return b.build()
}

// hold keeps the phase unchanged while the SandboxClass cannot be resolved.
// It never grants or releases a slot on its own -- SlotWanted mirrors
// whatever the environment already holds, so an unresolvable class can
// neither strand a slot nor snatch one away.
func hold(env v1alpha1.SandboxEnvironment, facts ClusterFacts, now time.Time, phase v1alpha1.Phase) Decision {
	return newBuilder(env, facts, now).
		phase(phase).
		slot(env.Status.Slot.Granted).
		cond(ConditionReady, metav1.ConditionUnknown, ReasonClassNotResolved, facts.ClassProblem).
		build()
}

// nextPending: Pending -> Ready once child resources are usable, else stays
// Pending and (re-)requests them. spec.suspend does not block this
// transition -- it only changes the Scheduled/Ready condition reasons once
// the environment reaches Ready/Waiting.
func nextPending(env v1alpha1.SandboxEnvironment, facts ClusterFacts, now time.Time) Decision {
	if facts.ResourcesReady {
		return newBuilder(env, facts, now).
			phase(v1alpha1.PhaseReady).
			slot(false).
			patch(StatusPatch{SetQueuedSince: newMetaTime(now)}).
			build()
	}
	return newBuilder(env, facts, now).
		phase(v1alpha1.PhasePending).
		slot(false).
		act(ActionEnsureResources).
		cond(ConditionScheduled, metav1.ConditionFalse, ReasonResourcesNotReady, facts.ResourcesProblem).
		build()
}

// nextReady: Ready -> Restoring once a slot is granted; self-loops while
// queued or suspended.
func nextReady(env v1alpha1.SandboxEnvironment, facts ClusterFacts, now time.Time) Decision {
	if env.Spec.Suspend {
		return newBuilder(env, facts, now).
			phase(v1alpha1.PhaseReady).
			slot(false).
			cond(ConditionReady, metav1.ConditionFalse, ReasonSuspended, "").
			build()
	}
	if facts.SlotGranted {
		return newBuilder(env, facts, now).
			phase(v1alpha1.PhaseRestoring).
			slot(true).
			act(ActionEnsurePod).
			build()
	}
	return newBuilder(env, facts, now).
		phase(v1alpha1.PhaseReady).
		slot(true).
		cond(ConditionScheduled, metav1.ConditionFalse, ReasonQueued, queueMessage(facts)).
		build()
}

// queueMessage renders the advisory queue-position message carried by the
// Scheduled=False/Queued condition. Empty when the position is unknown
// (facts.QueuePosition <= 0), so a caller that cannot compute a position
// produces exactly the message this condition carried before #20.
func queueMessage(f ClusterFacts) string {
	if f.QueuePosition <= 0 || f.QueueDepth <= 0 {
		return ""
	}
	return fmt.Sprintf("queued at position %d of %d", f.QueuePosition, f.QueueDepth)
}

// nextRestoring: Restoring -> Running once the pod is up and ready; bails
// back to Ready if suspended or the slot was revoked mid-restore; otherwise
// keeps ensuring the pod exists. Restoring -> Done/Failed is handled
// upstream by agentOrPodTerminal, not here.
func nextRestoring(env v1alpha1.SandboxEnvironment, facts ClusterFacts, now time.Time) Decision {
	switch {
	case env.Spec.Suspend:
		return newBuilder(env, facts, now).
			phase(v1alpha1.PhaseReady).
			slot(false).
			act(ActionDeletePod).
			build()
	case !facts.SlotGranted:
		return newBuilder(env, facts, now).
			phase(v1alpha1.PhaseReady).
			slot(true).
			act(ActionDeletePod).
			build()
	case !facts.PodObserved:
		return newBuilder(env, facts, now).
			phase(v1alpha1.PhaseRestoring).
			slot(true).
			act(ActionEnsurePod).
			cond(ConditionPodReady, metav1.ConditionUnknown, ReasonPodNotObserved, "").
			build()
	case facts.PodPhase == corev1.PodRunning && facts.PodReady:
		return newBuilder(env, facts, now).
			phase(v1alpha1.PhaseRunning).
			slot(true).
			patch(StatusPatch{SetStartedAt: newMetaTime(now), IncrementWakeCount: env.Status.Snapshot != nil}).
			cond(ConditionPodReady, metav1.ConditionTrue, ReasonPodRunning, "").
			build()
	default:
		reason := ReasonPodPending
		if !facts.PodExists {
			reason = ReasonPodNotCreated
		}
		return newBuilder(env, facts, now).
			phase(v1alpha1.PhaseRestoring).
			slot(true).
			act(ActionEnsurePod).
			cond(ConditionPodReady, metav1.ConditionFalse, reason, "").
			build()
	}
}

// nextRunning: Running -> Freezing once a wait is declared or suspend is
// requested (a genuine wait outranks a suspension); otherwise self-loops.
// Running -> Done/Failed is handled upstream by agentOrPodTerminal.
func nextRunning(env v1alpha1.SandboxEnvironment, facts ClusterFacts, now time.Time) Decision {
	switch {
	case facts.AgentWaitDeclared || env.Status.WaitFor != nil:
		return newBuilder(env, facts, now).
			phase(v1alpha1.PhaseFreezing).
			slot(true).
			act(ActionFreezePod).
			build()
	case env.Spec.Suspend:
		return newBuilder(env, facts, now).
			phase(v1alpha1.PhaseFreezing).
			slot(true).
			act(ActionFreezePod).
			build()
	case !facts.PodObserved:
		return newBuilder(env, facts, now).
			phase(v1alpha1.PhaseRunning).
			slot(true).
			cond(ConditionPodReady, metav1.ConditionUnknown, ReasonPodNotObserved, "").
			cond(ConditionReady, metav1.ConditionTrue, ReasonRunning, "").
			build()
	default:
		return newBuilder(env, facts, now).
			phase(v1alpha1.PhaseRunning).
			slot(true).
			cond(ConditionPodReady, metav1.ConditionTrue, ReasonPodRunning, "").
			cond(ConditionReady, metav1.ConditionTrue, ReasonRunning, "").
			build()
	}
}

// nextFreezing: Freezing -> Waiting once the snapshot completes and the pod
// is gone. A failed snapshot holds in Freezing (never deletes the pod) so
// the agent's context isn't lost.
func nextFreezing(env v1alpha1.SandboxEnvironment, facts ClusterFacts, now time.Time) Decision {
	switch {
	case facts.SnapshotFailure != nil:
		return newBuilder(env, facts, now).
			phase(v1alpha1.PhaseFreezing).
			slot(true).
			cond(ConditionFrozen, metav1.ConditionFalse, ReasonSnapshotFailed, facts.SnapshotFailure.Message).
			cond(ConditionReady, metav1.ConditionFalse, ReasonSnapshotFailed, facts.SnapshotFailure.Message).
			build()
	case !facts.SnapshotComplete:
		return newBuilder(env, facts, now).
			phase(v1alpha1.PhaseFreezing).
			slot(true).
			act(ActionFreezePod).
			cond(ConditionFrozen, metav1.ConditionFalse, ReasonSnapshotInProgress, "").
			build()
	case facts.PodExists:
		return newBuilder(env, facts, now).
			phase(v1alpha1.PhaseFreezing).
			slot(true).
			act(ActionDeletePod).
			cond(ConditionFrozen, metav1.ConditionFalse, ReasonPodTerminating, "").
			build()
	default:
		reason := ReasonSuspended
		if env.Status.WaitFor != nil {
			reason = ReasonWaitDeclared
		}
		return newBuilder(env, facts, now).
			phase(v1alpha1.PhaseWaiting).
			slot(false).
			patch(StatusPatch{IncrementFreezeCount: true}).
			cond(ConditionFrozen, metav1.ConditionTrue, reason, "").
			build()
	}
}

// nextWaiting: Waiting -> Ready once the probe is satisfied (clearing
// waitFor) or, when the wait was suspend-originated (waitFor already nil),
// once suspend is cleared. An unevaluatable probe fails the environment
// rather than hanging forever.
func nextWaiting(env v1alpha1.SandboxEnvironment, facts ClusterFacts, now time.Time) Decision {
	switch {
	case facts.WaitProbeFailure != nil:
		reason := SanitizeReason(facts.WaitProbeFailure.Reason, StepFailureReasons, ReasonProbeFailed)
		return newBuilder(env, facts, now).
			phase(v1alpha1.PhaseFailed).
			slot(false).
			patch(StatusPatch{SetFinishedAt: newMetaTime(now), SetProbeAttempt: facts.ProbeAttempt}).
			cond(ConditionReady, metav1.ConditionFalse, reason, facts.WaitProbeFailure.Message).
			build()
	case env.Spec.Suspend:
		return newBuilder(env, facts, now).
			phase(v1alpha1.PhaseWaiting).
			slot(false).
			cond(ConditionReady, metav1.ConditionFalse, ReasonSuspended, "").
			build()
	case env.Status.WaitFor == nil:
		return newBuilder(env, facts, now).
			phase(v1alpha1.PhaseReady).
			slot(true).
			build()
	case !facts.ProbeObserved:
		return newBuilder(env, facts, now).
			phase(v1alpha1.PhaseWaiting).
			slot(false).
			cond(ConditionWaitSatisfied, metav1.ConditionUnknown, ReasonProbeNotEvaluated, "").
			build()
	case facts.WaitProbeSatisfied:
		// The satisfied branch clears waitFor AND stamps the attempt as
		// Satisfied, so status.probeAttempt records the terminal evaluation
		// even after the wait itself is gone.
		attempt := facts.ProbeAttempt
		if attempt != nil {
			attempt = attempt.DeepCopy()
			attempt.Phase = v1alpha1.ProbeAttemptSatisfied
			attempt.LastResult = "satisfied"
		}
		return newBuilder(env, facts, now).
			phase(v1alpha1.PhaseReady).
			slot(true).
			patch(StatusPatch{ClearWaitFor: true, SetProbeAttempt: attempt}).
			cond(ConditionWaitSatisfied, metav1.ConditionTrue, ReasonProbeSatisfied, "").
			build()
	default:
		return newBuilder(env, facts, now).
			phase(v1alpha1.PhaseWaiting).
			slot(false).
			patch(StatusPatch{SetProbeAttempt: facts.ProbeAttempt}).
			cond(ConditionWaitSatisfied, metav1.ConditionFalse, ReasonProbePending, "").
			build()
	}
}
