package lifecycle

import (
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
)

// Apply projects d onto a deep copy of env.Status and returns the desired
// status. Pure: no clock, no I/O -- every timestamp in d was already
// stamped by the builder in next.go/timeouts.go.
func Apply(env *v1alpha1.SandboxEnvironment, d Decision) *v1alpha1.SandboxEnvironmentStatus {
	s := env.Status.DeepCopy()
	s.Phase = d.Phase
	s.ObservedGeneration = env.Generation

	for _, c := range d.Conditions {
		apimeta.SetStatusCondition(&s.Conditions, c)
	}

	if d.StatusPatch.SetQueuedSince != nil && s.QueuedSince == nil {
		s.QueuedSince = d.StatusPatch.SetQueuedSince
	}
	if d.StatusPatch.SetStartedAt != nil && s.StartedAt == nil {
		s.StartedAt = d.StatusPatch.SetStartedAt
	}
	if d.StatusPatch.SetFinishedAt != nil && s.FinishedAt == nil {
		s.FinishedAt = d.StatusPatch.SetFinishedAt
	}

	if d.StatusPatch.IncrementFreezeCount {
		s.FreezeCount++
	}
	if d.StatusPatch.IncrementWakeCount {
		s.WakeCount++
	}
	if d.StatusPatch.ClearWaitFor {
		s.WaitFor = nil
	}
	if d.StatusPatch.SetArchiveURI != "" {
		s.ArchiveURI = d.StatusPatch.SetArchiveURI
	}
	if d.StatusPatch.SetTerminalPhase != "" && s.TerminalPhase == "" {
		s.TerminalPhase = d.StatusPatch.SetTerminalPhase
	}
	if d.StatusPatch.SetProbeAttempt != nil {
		s.ProbeAttempt = d.StatusPatch.SetProbeAttempt.DeepCopy()
	}

	// PhaseHistory: append exactly one transition whenever the phase changes,
	// and never when it doesn't, so repeated reconciles on a settled phase
	// cannot grow the list. The first reconcile on a fresh environment (whose
	// status.phase is "" until now) appends the creation->Pending entry --
	// the "creation entry" run.json documents. The timestamp reuses the
	// conditions' LastTransitionTime, which build() stamps with the decision
	// moment for every Decision. Reason is the Ready summary condition's
	// reason at the transition, per the field's contract.
	if d.Phase != env.Status.Phase {
		reason := ""
		if c := apimeta.FindStatusCondition(d.Conditions, ConditionReady); c != nil {
			reason = c.Reason
		}
		var at metav1.Time
		if len(d.Conditions) > 0 {
			at = d.Conditions[0].LastTransitionTime
		}
		s.PhaseHistory = append(s.PhaseHistory, v1alpha1.PhaseTransition{Phase: d.Phase, At: at, Reason: reason})
	}

	// Slot: Apply implements only the RELEASE half. #20 owns granting.
	if !d.SlotWanted {
		s.Slot = v1alpha1.SlotStatus{}
	}

	return s
}
