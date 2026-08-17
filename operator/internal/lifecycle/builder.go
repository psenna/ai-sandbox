package lifecycle

import (
	"time"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
)

// builder assembles a Decision. Per-phase functions in next.go call phase,
// cond, act, slot, patch and requeue to state only what differs from the
// phase's usual behavior; build() fills in every condition type not
// explicitly set via cond() with the phase/facts-derived default described
// in conditions.go's tables. This is what keeps every per-phase function a
// short, flat list of guards well under the gocyclo limit -- none of them
// spell out all six conditions by hand.
type builder struct {
	env   v1alpha1.SandboxEnvironment
	facts ClusterFacts
	now   time.Time

	ph          v1alpha1.Phase
	explicit    map[string]metav1.Condition
	actions     []Action
	slotWanted  bool
	statusPatch StatusPatch
	requeue_    time.Duration
}

func newBuilder(env v1alpha1.SandboxEnvironment, facts ClusterFacts, now time.Time) *builder {
	return &builder{env: env, facts: facts, now: now, explicit: make(map[string]metav1.Condition, len(ConditionTypes))}
}

func (b *builder) phase(p v1alpha1.Phase) *builder {
	b.ph = p
	return b
}

func (b *builder) cond(condType string, status metav1.ConditionStatus, reason, message string) *builder {
	b.explicit[condType] = metav1.Condition{Type: condType, Status: status, Reason: reason, Message: message}
	return b
}

func (b *builder) act(actions ...Action) *builder {
	b.actions = append(b.actions, actions...)
	return b
}

func (b *builder) slot(want bool) *builder {
	b.slotWanted = want
	return b
}

func (b *builder) patch(p StatusPatch) *builder {
	if p.SetQueuedSince != nil {
		b.statusPatch.SetQueuedSince = p.SetQueuedSince
	}
	if p.SetStartedAt != nil {
		b.statusPatch.SetStartedAt = p.SetStartedAt
	}
	if p.SetFinishedAt != nil {
		b.statusPatch.SetFinishedAt = p.SetFinishedAt
	}
	if p.IncrementFreezeCount {
		b.statusPatch.IncrementFreezeCount = true
	}
	if p.IncrementWakeCount {
		b.statusPatch.IncrementWakeCount = true
	}
	if p.ClearWaitFor {
		b.statusPatch.ClearWaitFor = true
	}
	if p.SetArchiveURI != "" {
		b.statusPatch.SetArchiveURI = p.SetArchiveURI
	}
	return b
}

// build fills every condition type not explicitly set via cond() with its
// phase/facts-derived default and returns the finished Decision.
func (b *builder) build() Decision {
	conditions := make([]metav1.Condition, 0, len(ConditionTypes))
	ts := metav1.NewTime(b.now).Rfc3339Copy()
	for _, t := range ConditionTypes {
		var status metav1.ConditionStatus
		var reason, message string
		if c, ok := b.explicit[t]; ok {
			status, reason, message = c.Status, c.Reason, c.Message
		} else {
			status, reason, message = defaultFor(t, b.ph, b.env, b.facts)
		}
		conditions = append(conditions, metav1.Condition{
			Type:               t,
			Status:             status,
			Reason:             reason,
			Message:            message,
			ObservedGeneration: b.env.Generation,
			LastTransitionTime: ts,
		})
	}
	return Decision{
		Phase:        b.ph,
		Conditions:   conditions,
		Actions:      b.actions,
		SlotWanted:   b.slotWanted,
		StatusPatch:  b.statusPatch,
		RequeueAfter: b.requeue_,
	}
}

// defaultFor dispatches to the per-type default rule. Every value returned
// here must be a documented reason from conditions.go.
func defaultFor(condType string, phase v1alpha1.Phase, env v1alpha1.SandboxEnvironment, facts ClusterFacts) (metav1.ConditionStatus, string, string) {
	switch condType {
	case ConditionScheduled:
		return defaultScheduled(phase, env, facts)
	case ConditionPodReady:
		return defaultPodReady(phase, env, facts)
	case ConditionFrozen:
		return defaultFrozen(phase, env, facts)
	case ConditionWaitSatisfied:
		return defaultWaitSatisfied(phase, env, facts)
	case ConditionArchived:
		return defaultArchived(phase, facts)
	case ConditionReady:
		return defaultReady(phase, env, facts)
	default:
		return metav1.ConditionUnknown, ReasonPending, ""
	}
}

// carryForwardOr returns the existing condition of type condType on env
// verbatim (Status/Reason/Message) if present, else the given fallback. Used
// where the phase/facts-derived rules in conditions.go don't cover the
// current combination -- typically a Failed environment being reconciled
// again after the facts that caused the failure are no longer being
// reported. Carrying the prior value forward is what keeps the "stable
// reason strings" contract and idempotent status writes: recomputing a
// generic guess here would silently replace the real failure reason on
// every subsequent reconcile.
func carryForwardOr(env v1alpha1.SandboxEnvironment, condType string, fallbackStatus metav1.ConditionStatus, fallbackReason, fallbackMessage string) (metav1.ConditionStatus, string, string) {
	if c := apimeta.FindStatusCondition(env.Status.Conditions, condType); c != nil {
		return c.Status, c.Reason, c.Message
	}
	return fallbackStatus, fallbackReason, fallbackMessage
}

func defaultScheduled(phase v1alpha1.Phase, env v1alpha1.SandboxEnvironment, facts ClusterFacts) (metav1.ConditionStatus, string, string) {
	if isTerminal(phase) {
		return metav1.ConditionFalse, ReasonTerminal, ""
	}
	if facts.SlotGranted {
		return metav1.ConditionTrue, ReasonSlotGranted, ""
	}
	if env.Spec.Suspend && (phase == v1alpha1.PhasePending || phase == v1alpha1.PhaseReady || phase == v1alpha1.PhaseWaiting) {
		return metav1.ConditionFalse, ReasonSuspended, ""
	}
	switch phase {
	case v1alpha1.PhasePending:
		return metav1.ConditionFalse, ReasonResourcesNotReady, facts.ResourcesProblem
	case v1alpha1.PhaseWaiting:
		return metav1.ConditionFalse, ReasonWaiting, ""
	default:
		return metav1.ConditionFalse, ReasonQueued, queueMessage(facts)
	}
}

func defaultPodReady(phase v1alpha1.Phase, env v1alpha1.SandboxEnvironment, facts ClusterFacts) (metav1.ConditionStatus, string, string) {
	switch phase {
	case v1alpha1.PhasePending, v1alpha1.PhaseReady:
		return metav1.ConditionFalse, ReasonPodNotCreated, ""
	case v1alpha1.PhaseRestoring:
		return podReadyDuringRestore(facts)
	case v1alpha1.PhaseRunning:
		if !facts.PodObserved {
			return metav1.ConditionUnknown, ReasonPodNotObserved, ""
		}
		return metav1.ConditionTrue, ReasonPodRunning, ""
	case v1alpha1.PhaseFreezing, v1alpha1.PhaseWaiting:
		return metav1.ConditionFalse, ReasonPodDeleted, ""
	case v1alpha1.PhaseDone:
		return podReadyOnSuccess(facts)
	case v1alpha1.PhaseFailed:
		return podReadyOnFailure(env, facts)
	default:
		return metav1.ConditionFalse, ReasonPodNotCreated, ""
	}
}

func podReadyDuringRestore(facts ClusterFacts) (metav1.ConditionStatus, string, string) {
	if !facts.PodObserved {
		return metav1.ConditionUnknown, ReasonPodNotObserved, ""
	}
	if !facts.PodExists {
		return metav1.ConditionFalse, ReasonPodNotCreated, ""
	}
	return metav1.ConditionFalse, ReasonPodPending, ""
}

// podReadyOnSuccess distinguishes the agent explicitly reporting completion
// (facts.AgentDone, via /v1/done) from a pod that merely exited 0 on its
// own -- meaningfully different operational signals, mirroring
// podReadyOnFailure/readyOnFailure's equivalent facts.AgentFailed check.
func podReadyOnSuccess(facts ClusterFacts) (metav1.ConditionStatus, string, string) {
	if facts.AgentDone {
		return metav1.ConditionFalse, ReasonAgentReportedSuccess, ""
	}
	return metav1.ConditionFalse, ReasonPodSucceeded, ""
}

func podReadyOnFailure(env v1alpha1.SandboxEnvironment, facts ClusterFacts) (metav1.ConditionStatus, string, string) {
	if facts.PodFailure != nil {
		return metav1.ConditionFalse, SanitizeReason(facts.PodFailure.Reason, PodFailureReasons, ReasonPodFailed), facts.PodFailure.Message
	}
	if facts.PodPhase == corev1.PodFailed {
		return metav1.ConditionFalse, ReasonPodFailed, ""
	}
	return carryForwardOr(env, ConditionPodReady, metav1.ConditionFalse, ReasonPodFailed, "")
}

func defaultFrozen(phase v1alpha1.Phase, env v1alpha1.SandboxEnvironment, facts ClusterFacts) (metav1.ConditionStatus, string, string) {
	switch phase {
	case v1alpha1.PhaseWaiting:
		if env.Status.WaitFor != nil {
			return metav1.ConditionTrue, ReasonWaitDeclared, ""
		}
		return metav1.ConditionTrue, ReasonSuspended, ""
	case v1alpha1.PhaseFreezing:
		return frozenDuringFreeze(facts)
	default:
		return metav1.ConditionFalse, ReasonNotFrozen, ""
	}
}

func frozenDuringFreeze(facts ClusterFacts) (metav1.ConditionStatus, string, string) {
	if facts.SnapshotFailure != nil {
		return metav1.ConditionFalse, ReasonSnapshotFailed, facts.SnapshotFailure.Message
	}
	if !facts.SnapshotComplete {
		return metav1.ConditionFalse, ReasonSnapshotInProgress, ""
	}
	if facts.PodExists {
		return metav1.ConditionFalse, ReasonPodTerminating, ""
	}
	return metav1.ConditionFalse, ReasonNotFrozen, ""
}

func defaultWaitSatisfied(phase v1alpha1.Phase, env v1alpha1.SandboxEnvironment, facts ClusterFacts) (metav1.ConditionStatus, string, string) {
	if facts.WaitProbeSatisfied {
		return metav1.ConditionTrue, ReasonProbeSatisfied, ""
	}
	if phase == v1alpha1.PhaseWaiting && env.Status.WaitFor != nil {
		if !facts.ProbeObserved {
			return metav1.ConditionUnknown, ReasonProbeNotEvaluated, ""
		}
		if facts.WaitProbeFailure != nil {
			return metav1.ConditionFalse, SanitizeReason(facts.WaitProbeFailure.Reason, StepFailureReasons, ReasonProbeFailed), facts.WaitProbeFailure.Message
		}
		return metav1.ConditionFalse, ReasonProbePending, ""
	}
	if facts.WaitProbeFailure != nil {
		return metav1.ConditionFalse, SanitizeReason(facts.WaitProbeFailure.Reason, StepFailureReasons, ReasonProbeFailed), facts.WaitProbeFailure.Message
	}
	if env.Status.WaitFor == nil {
		return metav1.ConditionFalse, ReasonNoWaitDeclared, ""
	}
	return carryForwardOr(env, ConditionWaitSatisfied, metav1.ConditionFalse, ReasonProbePending, "")
}

func defaultArchived(phase v1alpha1.Phase, facts ClusterFacts) (metav1.ConditionStatus, string, string) {
	if facts.ArchiveWritten {
		return metav1.ConditionTrue, ReasonArchiveWritten, ""
	}
	if phase == v1alpha1.PhaseDone || phase == v1alpha1.PhaseFailed {
		return metav1.ConditionFalse, ReasonArchivePending, ""
	}
	return metav1.ConditionFalse, ReasonNotTerminal, ""
}

func defaultReady(phase v1alpha1.Phase, env v1alpha1.SandboxEnvironment, facts ClusterFacts) (metav1.ConditionStatus, string, string) {
	switch phase {
	case v1alpha1.PhasePending:
		return readySuspendable(env, ReasonPending)
	case v1alpha1.PhaseReady:
		return readySuspendable(env, ReasonQueued)
	case v1alpha1.PhaseRestoring:
		return metav1.ConditionFalse, ReasonRestoring, ""
	case v1alpha1.PhaseRunning:
		return metav1.ConditionTrue, ReasonRunning, ""
	case v1alpha1.PhaseFreezing:
		if facts.SnapshotFailure != nil {
			return metav1.ConditionFalse, ReasonSnapshotFailed, facts.SnapshotFailure.Message
		}
		return metav1.ConditionFalse, ReasonFreezing, ""
	case v1alpha1.PhaseWaiting:
		return readySuspendable(env, ReasonWaiting)
	case v1alpha1.PhaseDone:
		return readyOnSuccess(facts)
	case v1alpha1.PhaseFailed:
		return readyOnFailure(env, facts)
	default:
		return metav1.ConditionFalse, ReasonPending, ""
	}
}

func readySuspendable(env v1alpha1.SandboxEnvironment, reason string) (metav1.ConditionStatus, string, string) {
	if env.Spec.Suspend {
		return metav1.ConditionFalse, ReasonSuspended, ""
	}
	return metav1.ConditionFalse, reason, ""
}

// readyOnSuccess mirrors readyOnFailure: it distinguishes the agent
// explicitly reporting completion from a pod that merely exited 0 on its
// own.
func readyOnSuccess(facts ClusterFacts) (metav1.ConditionStatus, string, string) {
	if facts.AgentDone {
		return metav1.ConditionFalse, ReasonAgentReportedSuccess, ""
	}
	return metav1.ConditionFalse, ReasonSucceeded, ""
}

func readyOnFailure(env v1alpha1.SandboxEnvironment, facts ClusterFacts) (metav1.ConditionStatus, string, string) {
	if facts.AgentFailed {
		return metav1.ConditionFalse, ReasonAgentReportedFailure, truncateMessage(facts.AgentMessage)
	}
	if facts.PodFailure != nil {
		return metav1.ConditionFalse, SanitizeReason(facts.PodFailure.Reason, PodFailureReasons, ReasonPodFailed), facts.PodFailure.Message
	}
	if facts.PodPhase == corev1.PodFailed {
		return metav1.ConditionFalse, ReasonPodFailed, ""
	}
	if facts.WaitProbeFailure != nil {
		return metav1.ConditionFalse, SanitizeReason(facts.WaitProbeFailure.Reason, StepFailureReasons, ReasonProbeFailed), facts.WaitProbeFailure.Message
	}
	return carryForwardOr(env, ConditionReady, metav1.ConditionFalse, ReasonPodFailed, "")
}

// maxMessageBytes bounds AgentMessage as it's surfaced into a condition
// message: the agent's own text, truncated defensively.
const maxMessageBytes = 512

func truncateMessage(s string) string {
	if len(s) <= maxMessageBytes {
		return s
	}
	return s[:maxMessageBytes]
}

func isTerminal(phase v1alpha1.Phase) bool {
	return phase == v1alpha1.PhaseDone || phase == v1alpha1.PhaseFailed
}
