package controller

import (
	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
	"github.com/psenna/ai-sandbox/operator/internal/lifecycle"
)

// observeRestore enriches the PodFailure message with the actionable detail
// from status.restoreAttempt (#29). Purely informational: the phase
// transition to Failed is driven by the pod's own container-failure
// detection (restoreFailure in podstatus.go), not by this function -- it
// only replaces the generic "restore container exited with code N" line with
// the specific reason the restore container itself recorded (e.g.
// "ChecksumMismatch: want size=... got ..."), so the condition carries the
// actionable detail rather than an exit code. Mirrors observeSnapshot's
// truncation-via-truncateMessage convention exactly.
func (r *Reconciler) observeRestore(st *v1alpha1.SandboxEnvironmentStatus, f *lifecycle.ClusterFacts) {
	if a := st.RestoreAttempt; a != nil && a.Phase == v1alpha1.RestoreAttemptFailed && f.PodFailure != nil {
		f.PodFailure.Message = truncateMessage(a.Reason+": "+a.Message, podFailureMessageBytes)
	}
}
