package lifecycle

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
)

// Action is an idempotent side effect the reconciler must perform. Every
// action is named so that performing it twice is harmless.
type Action string

const (
	ActionEnsureResources Action = "EnsureResources" // #19: PVC/SA/Secret/ConfigMap
	ActionEnsurePod       Action = "EnsurePod"       // #21/#24: create-or-update the agent pod
	ActionFreezePod       Action = "FreezePod"       // #28: signal the sidecar to quiesce + snapshot
	ActionDeletePod       Action = "DeletePod"       // #21/#28: tear the pod down
	ActionArchive         Action = "Archive"         // #32: write the terminal archive
)

// AllActions lists every declared Action, for exhaustiveness checks in
// internal/controller (the action dispatch map must cover all of these).
var AllActions = []Action{ActionEnsureResources, ActionEnsurePod, ActionFreezePod, ActionDeletePod, ActionArchive}

const (
	MinRequeueAfter        = 1 * time.Second
	MaxRequeueAfter        = 5 * time.Minute
	ClassUnresolvedRequeue = 30 * time.Second
	TerminalArchiveRequeue = 1 * time.Minute
)

// StatusPatch carries the non-condition status mutations a transition
// implies. Every field is "leave alone" in its zero value; every timestamp
// field is set-once (Apply ignores it if the target is already non-nil).
type StatusPatch struct {
	SetQueuedSince       *metav1.Time
	SetStartedAt         *metav1.Time
	SetFinishedAt        *metav1.Time
	IncrementFreezeCount bool
	IncrementWakeCount   bool
	ClearWaitFor         bool
	SetArchiveURI        string
	// SetTerminalPhase records the terminal phase (Done or Failed) the
	// environment reached. Set-once like the timestamp fields: the freeze
	// detour (#32) reads it from nextWaiting to return to the correct
	// terminal phase after capturing the agent home.
	SetTerminalPhase v1alpha1.Phase
	// SetProbeAttempt replaces status.probeAttempt with the evaluator's most
	// recent attempt record (#30). Unlike the timestamp fields it is NOT
	// set-once: the attempt is a mutable record that is replaced wholesale on
	// every evaluation.
	SetProbeAttempt *v1alpha1.ProbeAttemptStatus
}

// Decision is the complete output of Next.
type Decision struct {
	// Phase is the phase after this reconcile. Always set; equal to the
	// current phase when nothing moves.
	Phase v1alpha1.Phase

	// Conditions holds exactly one entry per type in ConditionTypes, in that
	// order, with LastTransitionTime=now (RFC3339-truncated) and
	// ObservedGeneration already filled in.
	Conditions []metav1.Condition

	// Actions are performed by the reconciler, in slice order, BEFORE the
	// status write.
	Actions []Action

	// SlotWanted declares whether this environment should be holding a
	// scheduling slot after this reconcile. Next NEVER grants -- #20 grants
	// when SlotWanted && !status.slot.granted && capacity remains, and
	// releases whenever !SlotWanted && status.slot.granted. Apply implements
	// the release half today; it never sets Granted=true.
	SlotWanted bool

	// StatusPatch is the non-condition status delta.
	StatusPatch StatusPatch

	// RequeueAfter is the reconciler's ctrl.Result.RequeueAfter.
	RequeueAfter time.Duration
}
