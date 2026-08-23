package lifecycle

const (
	ConditionScheduled     = "Scheduled"
	ConditionPodReady      = "PodReady"
	ConditionFrozen        = "Frozen"
	ConditionWaitSatisfied = "WaitSatisfied"
	ConditionArchived      = "Archived"
	ConditionReady         = "Ready" // summary
)

// ConditionTypes is the canonical emission order. Apply writes conditions in
// this order so the stored slice order is stable across reconciles, which is
// what makes the whole-struct semantic-equality idempotency check work.
var ConditionTypes = []string{ConditionScheduled, ConditionPodReady, ConditionFrozen,
	ConditionWaitSatisfied, ConditionArchived, ConditionReady}

const (
	// scheduling
	ReasonSlotGranted       = "SlotGranted"
	ReasonQueued            = "Queued"
	ReasonResourcesNotReady = "ResourcesNotReady"
	ReasonSuspended         = "Suspended"
	ReasonWaiting           = "Waiting"
	ReasonTerminal          = "Terminal"

	// pod
	ReasonPodRunning     = "PodRunning"
	ReasonPodPending     = "PodPending"
	ReasonPodNotCreated  = "PodNotCreated"
	ReasonPodDeleted     = "PodDeleted"
	ReasonPodNotObserved = "PodNotObserved"
	ReasonPodSucceeded   = "PodSucceeded"

	// pod failure (allowlist for PodFailure.Reason)
	ReasonPodFailed                 = "PodFailed"
	ReasonImagePullFailure          = "ImagePullFailure"
	ReasonUnschedulable             = "Unschedulable"
	ReasonRestoreVerificationFailed = "RestoreVerificationFailed"

	// freeze
	ReasonWaitDeclared       = "WaitDeclared"
	ReasonSnapshotInProgress = "SnapshotInProgress"
	ReasonPodTerminating     = "PodTerminating"
	ReasonSnapshotFailed     = "SnapshotFailed" // allowlisted StepFailure.Reason
	ReasonNotFrozen          = "NotFrozen"

	// probes
	ReasonProbeSatisfied    = "ProbeSatisfied"
	ReasonProbePending      = "ProbePending"
	ReasonProbeNotEvaluated = "ProbeNotEvaluated"
	ReasonNoWaitDeclared    = "NoWaitDeclared"
	ReasonProbeFailed       = "ProbeFailed" // allowlisted StepFailure.Reason

	// archive
	ReasonArchiveWritten = "ArchiveWritten"
	ReasonArchivePending = "ArchivePending"
	ReasonNotTerminal    = "NotTerminal"

	// summary / lifecycle
	ReasonPending          = "Pending"
	ReasonRestoring        = "Restoring"
	ReasonRunning          = "Running"
	ReasonFreezing         = "Freezing"
	ReasonSucceeded        = "Succeeded"
	ReasonClassNotResolved = "ClassNotResolved"

	// agent
	ReasonAgentReportedSuccess = "AgentReportedSuccess"
	ReasonAgentReportedFailure = "AgentReportedFailure"

	// timeouts
	ReasonRunningTimeoutExceeded = "RunningTimeoutExceeded"
	ReasonWaitingTimeoutExceeded = "WaitingTimeoutExceeded"
	ReasonTotalTimeoutExceeded   = "TotalTimeoutExceeded"
)

// PodFailureReasons is the allowlist for PodFailure.Reason: an unrecognized
// value is replaced with ReasonPodFailed by SanitizeReason.
var PodFailureReasons = []string{ReasonPodFailed, ReasonImagePullFailure, ReasonUnschedulable, ReasonRestoreVerificationFailed}

// StepFailureReasons is the allowlist for StepFailure.Reason: an
// unrecognized value is replaced with the caller-supplied fallback by
// SanitizeReason.
var StepFailureReasons = []string{ReasonSnapshotFailed, ReasonProbeFailed}

// AllReasons lists every reason string this package can put on a
// condition, in the same order as the const block above. Same "declared
// list of every member" idiom as ConditionTypes above and
// v1alpha1.AllPhases. internal/docs's reasons_test.go asserts (a) this
// list is complete against the const block itself, and (b) every entry
// appears in operator/docs/operations.md -- issue #35's "every condition
// reason the operator can emit appears in the troubleshooting guide".
var AllReasons = []string{
	// scheduling
	ReasonSlotGranted,
	ReasonQueued,
	ReasonResourcesNotReady,
	ReasonSuspended,
	ReasonWaiting,
	ReasonTerminal,

	// pod
	ReasonPodRunning,
	ReasonPodPending,
	ReasonPodNotCreated,
	ReasonPodDeleted,
	ReasonPodNotObserved,
	ReasonPodSucceeded,

	// pod failure
	ReasonPodFailed,
	ReasonImagePullFailure,
	ReasonUnschedulable,
	ReasonRestoreVerificationFailed,

	// freeze
	ReasonWaitDeclared,
	ReasonSnapshotInProgress,
	ReasonPodTerminating,
	ReasonSnapshotFailed,
	ReasonNotFrozen,

	// probes
	ReasonProbeSatisfied,
	ReasonProbePending,
	ReasonProbeNotEvaluated,
	ReasonNoWaitDeclared,
	ReasonProbeFailed,

	// archive
	ReasonArchiveWritten,
	ReasonArchivePending,
	ReasonNotTerminal,

	// summary / lifecycle
	ReasonPending,
	ReasonRestoring,
	ReasonRunning,
	ReasonFreezing,
	ReasonSucceeded,
	ReasonClassNotResolved,

	// agent
	ReasonAgentReportedSuccess,
	ReasonAgentReportedFailure,

	// timeouts
	ReasonRunningTimeoutExceeded,
	ReasonWaitingTimeoutExceeded,
	ReasonTotalTimeoutExceeded,
}

// SanitizeReason returns r if it appears in allow, else fallback. Next runs
// every externally-supplied reason (from PodFailure/StepFailure) through this
// so a later issue's subsystem can never inject an arbitrary string into a
// condition and break the "stable reason strings" contract.
func SanitizeReason(r string, allow []string, fallback string) string {
	for _, a := range allow {
		if r == a {
			return r
		}
	}
	return fallback
}
