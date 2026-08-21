package controller

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
	"github.com/psenna/ai-sandbox/operator/internal/lifecycle"
	"github.com/psenna/ai-sandbox/operator/internal/metrics"
)

// Event reasons this controller emits (#33), PascalCase, matching the
// existing warnIfNetworkNotEnforced/archive.go escape-hatch style
// ("NetworkPolicyNotEnforced", "ArchiveSkippedByEscapeHatch"). Every one of
// these is emitted by observeTransition below, EXCEPT ReasonSlotGranted --
// that one fires from the scheduling pass itself (slotscheduler.go), the
// only place that grant is actually decided.
const (
	ReasonSlotGranted    = "SlotGranted"
	ReasonStarting       = "Starting"
	ReasonWaking         = "Waking"
	ReasonStarted        = "Started"
	ReasonFreezing       = "Freezing"
	ReasonFrozen         = "Frozen"
	ReasonSnapshotFailed = "SnapshotFailed"
	ReasonWaitSatisfied  = "WaitSatisfied"
	ReasonCompleted      = "Completed"
	ReasonFailed         = "Failed"
	ReasonArchived       = "Archived"
)

// AllEventReasons lists every Event reason this package can emit, mirroring
// lifecycle.ConditionTypes' "declared list of every member" idiom.
var AllEventReasons = []string{
	ReasonSlotGranted, ReasonStarting, ReasonWaking, ReasonStarted,
	ReasonFreezing, ReasonFrozen, ReasonSnapshotFailed, ReasonWaitSatisfied,
	ReasonCompleted, ReasonFailed, ReasonArchived,
}

// emitEvent is the one call site every Event this package emits goes
// through. rec == nil is a no-op (unit tests without a Recorder wired). A
// non-empty action is required for every NEW call site -- the two pre-#33
// call sites (netconditions.go's NetworkPolicyNotEnforced, archive.go's
// ArchiveSkippedByEscapeHatch) are inconsistent about this (one passes ""),
// which is not a pattern to copy.
func emitEvent(rec events.EventRecorder, obj runtime.Object, eventtype, reason, action, note string, args ...any) {
	if rec == nil {
		return
	}
	rec.Eventf(obj, nil, eventtype, reason, action, note, args...)
}

// observeTransition is writeStatus's status-write hook (#33): called AFTER a
// successful Status().Update, with prev the status immediately before the
// write and next the status that was just persisted (same value now on the
// server). It emits every Event in the table above except SlotGranted, and
// records the freeze/wake-duration and snapshot-size metrics (#6-#8) at the
// exact same transition edges -- both are point-in-time observations of a
// status write that has just durably landed, so they belong at the same
// hook.
//
// obj is the environment AFTER the write (its ObjectMeta/UID is what
// Eventf's "regarding" needs; its Spec is read for Suspend when
// disambiguating a Freezing entry). Every Event note is built only from
// safe inputs: literal strings, integers, durations, the ArchiveURI/
// Snapshot fields storage.Layout already assembled, and condition Reason/
// Message values that were already sanitized/truncated before they reached
// status (lifecycle.SanitizeReason, this package's own truncateMessage) --
// never a raw error, credential, or unsanitized external string.
func (r *Reconciler) observeTransition(ctx context.Context, obj *v1alpha1.SandboxEnvironment, prev, next *v1alpha1.SandboxEnvironmentStatus) {
	prevPhase, nextPhase := prev.Phase, next.Phase
	log := ctrl.LoggerFrom(ctx)

	// Exactly one new V(1) line for phase transitions (#33's logging plan):
	// per-reconcile detail, not a rare/significant/irreversible V(0) event.
	if nextPhase != prevPhase {
		log.V(1).Info("phase transition", LogKeyPhase, nextPhase, "previousPhase", prevPhase)
	}

	switch {
	case prevPhase == v1alpha1.PhaseReady && nextPhase == v1alpha1.PhaseRestoring:
		r.observeRestoringEntry(obj, next)
	case prevPhase == v1alpha1.PhaseRestoring && nextPhase == v1alpha1.PhaseRunning:
		emitEvent(r.Recorder, obj, corev1.EventTypeNormal, ReasonStarted, "Start",
			startedNote(next))
	case prevPhase != v1alpha1.PhaseFreezing && nextPhase == v1alpha1.PhaseFreezing:
		emitEvent(r.Recorder, obj, corev1.EventTypeNormal, ReasonFreezing, "Freeze",
			"freezing: %s", freezingDetail(obj, next))
	case prevPhase == v1alpha1.PhaseFreezing && nextPhase == v1alpha1.PhaseWaiting:
		r.observeFrozen(obj, next)
	case prevPhase == v1alpha1.PhaseWaiting && nextPhase == v1alpha1.PhaseReady:
		r.observeWaitSatisfied(obj, prev, next)
	}

	if prevPhase == v1alpha1.PhaseRestoring && nextPhase != v1alpha1.PhaseRestoring {
		r.observeWakeComplete(next)
	}
	// Completed/Failed fire once, on the fresh entry into a terminal phase --
	// never on a settled terminal environment's later self-loop passes
	// (terminal()'s own archive/detour bookkeeping), which carry the same
	// phase on both prev and next.
	if prevPhase != nextPhase && (nextPhase == v1alpha1.PhaseDone || nextPhase == v1alpha1.PhaseFailed) {
		r.observeTerminal(obj, next, nextPhase)
	}

	r.observeSnapshotFailedEvent(obj, prev, next)
	r.observeArchivedEvent(obj, prev, next)
}

// observeRestoringEntry emits Starting (cold start) or Waking (from an
// existing snapshot) for a Ready->Restoring transition.
func (r *Reconciler) observeRestoringEntry(obj *v1alpha1.SandboxEnvironment, next *v1alpha1.SandboxEnvironmentStatus) {
	if next.Snapshot == nil {
		emitEvent(r.Recorder, obj, corev1.EventTypeNormal, ReasonStarting, "Start",
			"creating the agent pod for a cold start")
		return
	}
	emitEvent(r.Recorder, obj, corev1.EventTypeNormal, ReasonWaking, "Wake",
		"waking from snapshot seq %d", next.Snapshot.Seq)
}

// startedNote builds the Started event's note, mentioning the wake count
// when this completion followed a wake (WakeCount > 0).
func startedNote(next *v1alpha1.SandboxEnvironmentStatus) string {
	if next.WakeCount > 0 {
		return fmt.Sprintf("agent pod is running and ready (wake #%d)", next.WakeCount)
	}
	return "agent pod is running and ready"
}

// freezingDetail names why this Freezing entry happened, for the Freezing
// event's "freezing: %s" note -- derived from the same status fields
// terminal()/nextRunning (internal/lifecycle/next.go) branched on to reach
// this phase.
func freezingDetail(obj *v1alpha1.SandboxEnvironment, next *v1alpha1.SandboxEnvironmentStatus) string {
	switch {
	case next.FinishedAt != nil:
		// terminal()'s freeze detour: the run already reached Done/Failed
		// and is capturing the agent home before archiving.
		return "capturing final context before the terminal archive"
	case next.WaitFor != nil:
		return "a wait condition was declared"
	default:
		return "suspend was requested"
	}
}

// observeFrozen emits Frozen and records the freeze-duration/snapshot-size
// metrics (#6, #8) for a Freezing->Waiting transition. Both are skipped
// (event still fires, with a generic note) when status.snapshot didn't land
// -- should not happen (nextFreezing only reaches Waiting once
// facts.SnapshotComplete is true), but a defensive read here must never
// panic on a nil pointer.
func (r *Reconciler) observeFrozen(obj *v1alpha1.SandboxEnvironment, next *v1alpha1.SandboxEnvironmentStatus) {
	s := next.Snapshot
	if s == nil {
		emitEvent(r.Recorder, obj, corev1.EventTypeNormal, ReasonFrozen, "Freeze", "frozen (no snapshot recorded)")
		return
	}
	d := time.Duration(s.DurationMillis) * time.Millisecond
	emitEvent(r.Recorder, obj, corev1.EventTypeNormal, ReasonFrozen, "Freeze",
		"frozen: snapshot seq %d, %d bytes in %s", s.Seq, s.SizeBytes, d)
	if s.DurationMillis != 0 {
		r.Metrics.RecordFreeze(d, s.SizeBytes)
	}
}

// observeWaitSatisfied emits WaitSatisfied for a Waiting->Ready transition
// that was actually driven by a satisfied probe (nextWaiting's
// WaitProbeSatisfied branch) rather than the suspend-originated
// WaitFor==nil path, which shares the same phase edge but never satisfies a
// probe.
func (r *Reconciler) observeWaitSatisfied(obj *v1alpha1.SandboxEnvironment, prev, next *v1alpha1.SandboxEnvironmentStatus) {
	c := apimeta.FindStatusCondition(next.Conditions, lifecycle.ConditionWaitSatisfied)
	if c == nil || c.Status != metav1.ConditionTrue || c.Reason != lifecycle.ReasonProbeSatisfied {
		return
	}
	waitType := "wait"
	if prev.WaitFor != nil {
		waitType = prev.WaitFor.Type
	}
	attempts := int32(0)
	if next.ProbeAttempt != nil {
		attempts = next.ProbeAttempt.Attempts
	}
	emitEvent(r.Recorder, obj, corev1.EventTypeNormal, ReasonWaitSatisfied, "Wake",
		"wait condition %s satisfied after %d evaluations", waitType, attempts)
}

// observeWakeComplete records the wake-duration metric (#7) for a
// Restoring->(anything else) transition, gated on a snapshot existing (a
// wake, not a first-ever cold start) and a genuinely succeeded restore
// attempt for that snapshot's sequence -- a revoked slot or a suspend
// bailing out of Restoring carries no fresh succeeded attempt, so this
// naturally records nothing for those edges.
func (r *Reconciler) observeWakeComplete(next *v1alpha1.SandboxEnvironmentStatus) {
	if next.Snapshot == nil || next.RestoreAttempt == nil {
		return
	}
	a := next.RestoreAttempt
	if a.Phase != v1alpha1.RestoreAttemptSucceeded || a.Seq != next.Snapshot.Seq {
		return
	}
	r.Metrics.RecordWake(wakeSource(a), time.Duration(a.DurationMillis)*time.Millisecond)
}

// wakeSource reads the workspace root's restore Source ("Warm"/"Cold") from
// a, mapping it to metrics.WakeSource*. Absent (no workspace root recorded)
// maps to WakeSourceUnknown.
func wakeSource(a *v1alpha1.RestoreAttemptStatus) string {
	for _, root := range a.Roots {
		if root.Name != "workspace" {
			continue
		}
		switch root.Source {
		case "Warm":
			return metrics.WakeSourceWarm
		case "Cold":
			return metrics.WakeSourceCold
		}
	}
	return metrics.WakeSourceUnknown
}

// observeTerminal emits Completed or Failed for a fresh *->Done/*->Failed
// transition.
func (r *Reconciler) observeTerminal(obj *v1alpha1.SandboxEnvironment, next *v1alpha1.SandboxEnvironmentStatus, phase v1alpha1.Phase) {
	ready := apimeta.FindStatusCondition(next.Conditions, lifecycle.ConditionReady)
	reason, message := "", ""
	if ready != nil {
		reason, message = ready.Reason, ready.Message
	}
	if phase == v1alpha1.PhaseDone {
		emitEvent(r.Recorder, obj, corev1.EventTypeNormal, ReasonCompleted, "Complete",
			"run completed successfully (%s)", reason)
		return
	}
	emitEvent(r.Recorder, obj, corev1.EventTypeWarning, ReasonFailed, "Complete",
		"run failed: %s: %s", reason, message)
}

// observeSnapshotFailedEvent emits SnapshotFailed exactly once when the
// Frozen condition's reason BECOMES a snapshot-failure reason (was not
// already that reason on prev) -- nextFreezing re-emits the same False/
// SnapshotFailed condition on every self-loop pass while stuck, and this
// must fire once per failure, not once per reconcile.
func (r *Reconciler) observeSnapshotFailedEvent(obj *v1alpha1.SandboxEnvironment, prev, next *v1alpha1.SandboxEnvironmentStatus) {
	nc := apimeta.FindStatusCondition(next.Conditions, lifecycle.ConditionFrozen)
	if nc == nil || nc.Reason != lifecycle.ReasonSnapshotFailed {
		return
	}
	if pc := apimeta.FindStatusCondition(prev.Conditions, lifecycle.ConditionFrozen); pc != nil && pc.Reason == lifecycle.ReasonSnapshotFailed {
		return
	}
	emitEvent(r.Recorder, obj, corev1.EventTypeWarning, ReasonSnapshotFailed, "Freeze",
		"freeze snapshot failed: %s", nc.Message)
}

// observeArchivedEvent emits Archived exactly once when the Archived
// condition's Status BECOMES True.
func (r *Reconciler) observeArchivedEvent(obj *v1alpha1.SandboxEnvironment, prev, next *v1alpha1.SandboxEnvironmentStatus) {
	nc := apimeta.FindStatusCondition(next.Conditions, lifecycle.ConditionArchived)
	if nc == nil || nc.Status != metav1.ConditionTrue {
		return
	}
	if pc := apimeta.FindStatusCondition(prev.Conditions, lifecycle.ConditionArchived); pc != nil && pc.Status == metav1.ConditionTrue {
		return
	}
	r.Metrics.RecordArchive(metrics.ResultSucceeded)
	emitEvent(r.Recorder, obj, corev1.EventTypeNormal, ReasonArchived, "Archive",
		"terminal archive written to %s", next.ArchiveURI)
}
