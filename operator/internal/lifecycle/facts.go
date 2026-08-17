package lifecycle

import (
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
)

// Timeouts is the resolved, parsed form of SandboxClass.spec.timeouts. A zero
// duration disables that timeout.
type Timeouts struct {
	Running time.Duration
	Waiting time.Duration
	Total   time.Duration
}

// DefaultTimeouts mirrors the CRD defaults on TimeoutsSpec (6h/24h/72h). Used
// when the class cannot be resolved, so a missing class never leaves an
// environment unbounded.
var DefaultTimeouts = Timeouts{Running: 6 * time.Hour, Waiting: 24 * time.Hour, Total: 72 * time.Hour}

// ResolveTimeouts parses class.Spec.Timeouts, substituting the corresponding
// DefaultTimeouts field for any value that is empty or fails to parse via
// time.ParseDuration. class may be nil.
func ResolveTimeouts(class *v1alpha1.SandboxClass) Timeouts {
	if class == nil {
		return DefaultTimeouts
	}
	parse := func(s string, def time.Duration) time.Duration {
		if s == "" {
			return def
		}
		d, err := time.ParseDuration(s)
		if err != nil {
			return def
		}
		return d
	}
	return Timeouts{
		Running: parse(class.Spec.Timeouts.Running, DefaultTimeouts.Running),
		Waiting: parse(class.Spec.Timeouts.Waiting, DefaultTimeouts.Waiting),
		Total:   parse(class.Spec.Timeouts.Total, DefaultTimeouts.Total),
	}
}

// PodFailure describes an observed, terminal pod problem. Reason should be
// one of the PodFailureReasons constants in conditions.go; Next sanitizes it
// via SanitizeReason so an unrecognized value can never leak into a
// condition's Reason field.
type PodFailure struct {
	Reason  string
	Message string
}

// StepFailure describes a terminal failure of a subsystem step (snapshot,
// probe evaluation). Reason should be one of the StepFailureReasons
// constants in conditions.go.
type StepFailure struct {
	Reason  string
	Message string
}

// ClusterFacts is everything Next is allowed to know about the world beyond
// the SandboxEnvironment object itself. It is the single seam between the
// pure state machine (this package) and every subsystem that observes real
// state: #19 (child resources), #20 (slot scheduler), #21/#24 (pod
// rendering/engine), #27 (sidecar), #28 (freeze), #29 (wake), #30 (wait
// probes), #32 (archive).
//
// Design rules:
//  1. Observation only. No field expresses a desire or a command.
//  2. The zero value is always the safe reading: ClusterFacts{} must never
//     cause Next to advance a phase, fail an environment, or release a slot
//     it did not need to.
//  3. Every field names the issue that will eventually populate it with real
//     data, so a future implementer knows exactly what to wire up.
type ClusterFacts struct {
	// ---- SandboxClass resolution (this issue) ----

	// Timeouts is the resolved class timeout policy. The reconciler ALWAYS
	// populates this (falling back to DefaultTimeouts when the class is
	// unreadable), so timeout evaluation never itself depends on
	// ClassResolved.
	Timeouts Timeouts

	// ClassResolved reports whether spec.classRef resolved to a readable
	// SandboxClass. False holds the phase and drives Ready=Unknown; it does
	// not by itself fail the environment.
	ClassResolved bool
	// ClassProblem explains ClassResolved==false. Never contains secrets.
	ClassProblem string

	// ---- child resources: PVC / ServiceAccount / Secret / ConfigMap (#19) ----

	// ResourcesReady reports that every child object this environment needs
	// exists and is usable (in practice: the workspace PVC is Bound).
	ResourcesReady bool
	// ResourcesProblem explains ResourcesReady==false.
	ResourcesProblem string

	// ---- execution slot (#20) ----

	// SlotGranted reports that this environment currently holds a scheduling
	// slot. Next NEVER grants a slot -- granting needs global capacity state
	// and lives in #20. Next only observes the grant here and declares
	// Decision.SlotWanted. Until #20 exists, the reconciler reads this
	// straight off status.slot.granted (see internal/controller/observe.go)
	// -- a real, if degenerate, observation: it is precisely the field #20
	// will eventually write, so nothing about the seam changes when #20
	// lands.
	SlotGranted bool

	// ---- agent pod (#21, #24) ----

	// PodObserved reports that the pod for this environment was actually
	// looked up. False means "we cannot see pods" (this issue's stub, or a
	// failed List) and is deliberately distinct from "there is no pod": it
	// blocks Restoring->Running and Running->Done/Failed and surfaces as
	// PodReady=Unknown rather than pretending the pod is merely slow.
	PodObserved bool
	// PodExists reports that the environment's pod object exists.
	PodExists bool
	// PodPhase is the observed pod phase; "" when !PodExists.
	PodPhase corev1.PodPhase
	// PodReady reports that the agent container is Ready (pod condition
	// Ready==True). Restoring->Running requires PodPhase==Running && PodReady.
	PodReady bool
	// PodFailure, when non-nil, is a terminal pod problem with an actionable
	// reason (unschedulable, image pull, snapshot verification, plain
	// failure).
	PodFailure *PodFailure

	// ---- sandboxctl sidecar control channel (#27) ----

	// AgentWaitDeclared reports that the agent declared a wait since the last
	// freeze. Next also treats a non-nil status.waitFor as a wait
	// declaration, so the transition is correct even before #27 ships.
	AgentWaitDeclared bool
	// AgentDone reports a successful /v1/done.
	AgentDone bool
	// AgentFailed reports a failing /v1/done.
	AgentFailed bool
	// AgentMessage is the agent's own explanation, surfaced in the Ready
	// condition message. The reconciler truncates it; never a secret.
	AgentMessage string

	// ---- freeze (#28) ----

	// SnapshotComplete reports that the sidecar finished and verified the
	// snapshot for the freeze currently in flight.
	SnapshotComplete bool
	// SnapshotFailure, when non-nil, holds the environment in Freezing
	// rather than proceeding to delete the pod and lose the agent's context.
	SnapshotFailure *StepFailure

	// ---- wait probes (#30) ----

	// ProbeObserved reports that the probe evaluator actually evaluated
	// status.waitFor this pass. Like PodObserved, false is "nothing
	// evaluates probes" and surfaces as WaitSatisfied=Unknown, not as "still
	// pending".
	ProbeObserved bool
	// WaitProbeSatisfied reports that status.waitFor is satisfied.
	WaitProbeSatisfied bool
	// WaitProbeFailure, when non-nil, is an unevaluatable probe (bad URL,
	// auth failure) -- must fail the environment rather than hang.
	WaitProbeFailure *StepFailure

	// ---- archive (#32) ----

	// ArchiveWritten reports that the terminal archive has been written.
	ArchiveWritten bool
	// ArchiveURI is where it was written, mirrored into status.archiveURI.
	ArchiveURI string
}

// WithAllObserved returns a copy of f with every *Observed flag set. Exists so
// tests and later subsystems can build facts without repeating the flags;
// production observers must set them individually as they actually observe.
func (f ClusterFacts) WithAllObserved() ClusterFacts {
	f.ClassResolved = true
	f.PodObserved = true
	f.ProbeObserved = true
	return f
}
