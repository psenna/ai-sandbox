package lifecycle

import (
	apimeta "k8s.io/apimachinery/pkg/api/meta"

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
	if d.StatusPatch.SetProbeAttempt != nil {
		s.ProbeAttempt = d.StatusPatch.SetProbeAttempt.DeepCopy()
	}

	// Slot: Apply implements only the RELEASE half. #20 owns granting.
	if !d.SlotWanted {
		s.Slot = v1alpha1.SlotStatus{}
	}

	return s
}
