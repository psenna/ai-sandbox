package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Phase is the coarse-grained lifecycle state of a SandboxEnvironment.
// +kubebuilder:validation:Enum=Pending;Ready;Running;Freezing;Waiting;Restoring;Done;Failed
type Phase string

const (
	// PhasePending means the environment has been created but has not yet
	// been granted a slot.
	PhasePending Phase = "Pending"

	// PhaseReady means the environment has been granted a slot and is being
	// prepared to run.
	PhaseReady Phase = "Ready"

	// PhaseRunning means the sandbox is actively running the agent.
	PhaseRunning Phase = "Running"

	// PhaseFreezing means the sandbox is being snapshotted and torn down.
	PhaseFreezing Phase = "Freezing"

	// PhaseWaiting means the sandbox has been frozen and is waiting on an
	// external condition (status.waitFor) before it can be restored.
	PhaseWaiting Phase = "Waiting"

	// PhaseRestoring means a frozen sandbox is being restored from its most
	// recent snapshot.
	PhaseRestoring Phase = "Restoring"

	// PhaseDone means the environment has completed and its resources have
	// been reclaimed.
	PhaseDone Phase = "Done"

	// PhaseFailed means the environment failed and will not be retried
	// automatically.
	PhaseFailed Phase = "Failed"
)

// FinalizerArchiveOnDelete is applied to every SandboxEnvironment on creation.
// Its presence guarantees the controller completes the terminal archive
// (archive/run.json + archive/context.tar.zst) before the object is removed
// from the API: Kubernetes will not garbage-collect a CR while a finalizer
// is present, so a backend outage during archiving blocks finalizer removal
// and retries on the next reconcile rather than dropping the run's context.
const FinalizerArchiveOnDelete = "sandbox.psenna.dev/environment-archiver"

// SandboxEnvironmentSpec describes a single agent run: which class to build
// the sandbox from, which repository it operates on, what task it should
// perform, and its scheduling priority.
type SandboxEnvironmentSpec struct {
	// ClassRef references the SandboxClass this environment's sandbox is
	// built from. Immutable after creation.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="classRef is immutable"
	ClassRef ClassRef `json:"classRef"`

	// Repo is the repository this environment's agent operates on, in
	// "owner/name" or "owner/name.git" form. Immutable after creation.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+(\.git)?$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="repo is immutable"
	Repo string `json:"repo"`

	// Task describes the work the agent should perform. Immutable after
	// creation.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="task is immutable"
	Task TaskSpec `json:"task"`

	// Priority influences scheduling order among environments competing for
	// a slot; higher values are scheduled first.
	// +kubebuilder:validation:Minimum=-1000
	// +kubebuilder:validation:Maximum=1000
	// +kubebuilder:default=0
	// +optional
	Priority int32 `json:"priority,omitempty"`

	// Suspend pauses scheduling of this environment: a suspended
	// environment will not be granted a slot, and a running sandbox will be
	// frozen, until suspend is cleared.
	// +kubebuilder:default=false
	// +optional
	Suspend bool `json:"suspend,omitempty"`
}

// ClassRef references a SandboxClass by name.
type ClassRef struct {
	// Name is the name of the referenced SandboxClass.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
}

// TaskSpec describes the work an agent should perform. At least one of
// prompt or issueRef must be set.
//
// The CEL rule below checks size(self.prompt) rather than comparing prompt
// against a CEL empty-string literal directly, because gofmt's doc-comment
// formatter (Go 1.19+) rewrites an adjacent pair of straight single quotes
// denoting an empty string into a single curly quote character, corrupting
// that syntax. Checking size() enforces the same non-empty-prompt condition
// without tripping that gofmt behavior.
// +kubebuilder:validation:XValidation:rule="(has(self.prompt) && size(self.prompt) > 0) || has(self.issueRef)",message="task requires at least one of prompt or issueRef"
type TaskSpec struct {
	// Prompt is free-form task instructions given directly to the agent.
	// +kubebuilder:validation:MaxLength=8192
	// +optional
	Prompt string `json:"prompt,omitempty"`

	// IssueRef references an issue whose title/body/comments the agent
	// should use as its task instructions.
	// +optional
	IssueRef *IssueRef `json:"issueRef,omitempty"`
}

// IssueRef references a single issue in a repository.
type IssueRef struct {
	// Repo is the repository the issue belongs to, in "owner/name" form.
	// +kubebuilder:validation:MinLength=1
	Repo string `json:"repo"`

	// Number is the issue number.
	// +kubebuilder:validation:Minimum=1
	Number int32 `json:"number"`
}

// SandboxEnvironmentStatus is the most recently observed state of a
// SandboxEnvironment.
type SandboxEnvironmentStatus struct {
	// Phase is the coarse-grained lifecycle state of the environment.
	// +kubebuilder:default=Pending
	// +optional
	Phase Phase `json:"phase,omitempty"`

	// Conditions represents the latest available observations of this
	// environment's state.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Slot records whether this environment currently holds a scheduling
	// slot.
	// +optional
	Slot SlotStatus `json:"slot,omitempty"`

	// WaitFor records the external condition, if any, blocking a frozen
	// sandbox from being restored.
	// +optional
	WaitFor *WaitForStatus `json:"waitFor,omitempty"`

	// ProbeAttempt records the operator's most recent evaluation of
	// status.waitFor (#30). Written ONLY by the operator's ProbeEvaluator
	// (internal/controller/probe.go), never by the sidecar or the agent. It
	// is how a human, and the requeue logic, tell "still pending, will
	// retry" from "unevaluatable, will fail" without inferring anything from
	// timing.
	// +optional
	ProbeAttempt *ProbeAttemptStatus `json:"probeAttempt,omitempty"`

	// AgentResult records the agent's own report of how its run ended, as
	// declared through the sandboxctl sidecar's POST /v1/done.
	// +optional
	AgentResult *AgentResultStatus `json:"agentResult,omitempty"`

	// Snapshot records the most recent workspace snapshot taken for this
	// environment.
	// +optional
	Snapshot *SnapshotStatus `json:"snapshot,omitempty"`

	// SnapshotAttempt records the freeze snapshot currently in flight, or
	// the last one that failed. Written ONLY by the sandboxctl sidecar (or
	// the recovery Job running the same binary). It is how the operator, and
	// a human, distinguish "still retrying" from "permanently failed": on
	// permanent failure the environment HOLDS in Freezing with
	// Frozen=False/SnapshotFailed and the pod is never deleted, so the
	// agent's context is never silently dropped.
	// +optional
	SnapshotAttempt *SnapshotAttemptStatus `json:"snapshotAttempt,omitempty"`

	// RestoreAttempt records the wake currently in flight, or the last one
	// that failed. Written ONLY by the restore init container (the same
	// sandboxctl binary, `restore` subcommand). It is how the operator, and
	// a human, tell a warm wake from a cold one -- and a corrupt snapshot
	// from a slow one -- without inferring anything from timing.
	// +optional
	RestoreAttempt *RestoreAttemptStatus `json:"restoreAttempt,omitempty"`

	// QueuedSince is when the environment started waiting for a slot.
	// +optional
	QueuedSince *metav1.Time `json:"queuedSince,omitempty"`

	// StartedAt is when the sandbox first started running.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`

	// FinishedAt is when the environment reached a terminal phase (Done or
	// Failed).
	// +optional
	FinishedAt *metav1.Time `json:"finishedAt,omitempty"`

	// FreezeCount is the number of times this environment's sandbox has
	// been frozen.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=0
	// +optional
	FreezeCount int32 `json:"freezeCount,omitempty"`

	// WakeCount is the number of times this environment's sandbox has been
	// restored from a frozen state.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=0
	// +optional
	WakeCount int32 `json:"wakeCount,omitempty"`

	// ArchiveURI is the location of this environment's final workspace
	// archive, once the environment has reached a terminal phase.
	// +optional
	ArchiveURI string `json:"archiveURI,omitempty"`

	// Archive records the terminal archive written for this environment.
	// Written by the sandboxctl archive Job; read by the controller to clear
	// the finalizer and by retention GC to select archives past their TTL.
	// +optional
	Archive *ArchiveStatus `json:"archive,omitempty"`

	// TerminalPhase records the terminal phase (Done or Failed) the
	// environment reached, set once when the run first terminated. Used by
	// the freeze-detour path (#32) to return to the correct terminal phase
	// after capturing the agent home, rather than re-running the agent.
	// +optional
	TerminalPhase Phase `json:"terminalPhase,omitempty"`

	// PhaseHistory is the full phase-transition history with timestamps,
	// appended on every phase change in lifecycle.Apply. It is the issue's
	// "full phase-transition history with timestamps" requirement: the Ready
	// condition only carries the LastTransitionTime for the *current* phase,
	// while this list preserves every transition so run.json can record them.
	// +kubebuilder:validation:MaxItems=64
	// +optional
	// +listType=atomic
	PhaseHistory []PhaseTransition `json:"phaseHistory,omitempty"`

	// GitState records the agent's final git state, if the agent recorded
	// one. Written by the sandboxctl sidecar from
	// /workspace/.sandbox/git-state.json during freeze; surfaced in run.json.
	// +optional
	GitState *GitStateStatus `json:"gitState,omitempty"`

	// ObservedGeneration is the most recent generation observed by the
	// controller reconciling this environment.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// SlotStatus records whether an environment currently holds a scheduling
// slot.
type SlotStatus struct {
	// Granted is true when this environment currently holds a scheduling
	// slot.
	// +kubebuilder:default=false
	// +optional
	Granted bool `json:"granted,omitempty"`

	// GrantedAt is when the slot was granted.
	// +optional
	GrantedAt *metav1.Time `json:"grantedAt,omitempty"`

	// LeaseName is the name of the lease resource backing the granted slot.
	// +optional
	LeaseName string `json:"leaseName,omitempty"`
}

// WaitForStatus records the external condition blocking a frozen sandbox
// from being restored. It is written ONLY by the sandboxctl sidecar
// (internal/sandboxctl), which validates every declaration against the
// allowlist below before patching it here; the agent container itself holds
// no Kubernetes credential and can never write this field directly.
//
// This type is the SHAPE contract only. Deciding when a declared probe is
// SATISFIED is #30's job: it reads this field and sets
// lifecycle.ClusterFacts.{ProbeObserved,WaitProbeSatisfied,WaitProbeFailure}.
// Nothing evaluates probes today, so a declared wait holds the environment
// in Freezing/Waiting with the operator's own facts unpopulated -- the
// documented, honest reading (see internal/lifecycle/next.go's nextWaiting).
type WaitForStatus struct {
	// Type identifies the kind of condition being waited on. The enum IS
	// the allowlist: the API server rejects anything else, and
	// internal/sandboxctl rejects it earlier with an actionable error.
	// The members are exactly the probe types issue #30 will evaluate.
	// +kubebuilder:validation:Enum=GitProxyCheck;HTTPGet;S3ObjectExists;NotBefore
	Type string `json:"type"`

	// Reason is a human-readable explanation of why this wait was
	// declared.
	// +kubebuilder:validation:MaxLength=512
	// +optional
	Reason string `json:"reason,omitempty"`

	// DeclaredAt is when this wait condition was declared. Stamped by the
	// sidecar, never supplied by the agent.
	// +optional
	DeclaredAt *metav1.Time `json:"declaredAt,omitempty"`

	// Params carries type-specific parameters for the wait condition. Which
	// keys are required/permitted per Type is enforced fail-closed by
	// internal/sandboxctl/probe.go; see that file for the per-type table.
	// +kubebuilder:validation:MaxProperties=16
	// +optional
	Params map[string]string `json:"params,omitempty"`
}

// Allowlisted WaitForStatus.Type values. Adding a member here requires a
// matching entry in internal/sandboxctl/probe.go's paramSchema and, once
// #30 lands, an evaluator -- see that package's allowlist round-trip test.
const (
	WaitTypeGitProxyCheck  = "GitProxyCheck"
	WaitTypeHTTPGet        = "HTTPGet"
	WaitTypeS3ObjectExists = "S3ObjectExists"
	WaitTypeNotBefore      = "NotBefore"
)

// ProbeAttemptPhase is the state of the probe evaluation being attempted.
// +kubebuilder:validation:Enum=Pending;Satisfied;Failed
type ProbeAttemptPhase string

const (
	// ProbeAttemptPending means the probe has been evaluated and is not yet
	// satisfied (or could not be evaluated this pass but is still within the
	// consecutive-error budget).
	ProbeAttemptPending ProbeAttemptPhase = "Pending"
	// ProbeAttemptSatisfied means the probe's condition has been met.
	ProbeAttemptSatisfied ProbeAttemptPhase = "Satisfied"
	// ProbeAttemptFailed means the probe could not be evaluated and the
	// consecutive-error budget was exhausted -- the environment fails.
	ProbeAttemptFailed ProbeAttemptPhase = "Failed"
)

// ProbeAttemptStatus records the operator's evaluation of status.waitFor
// (#30). Written ONLY by the operator's ProbeEvaluator
// (internal/controller/probe.go), never by the sidecar or the agent. It is
// how a human, and the requeue logic, tell "still pending, will retry" from
// "unevaluatable, will fail" without inferring anything from timing.
type ProbeAttemptStatus struct {
	// Type is the WaitForStatus.Type this attempt evaluated.
	// +kubebuilder:validation:Enum=GitProxyCheck;HTTPGet;S3ObjectExists;NotBefore
	Type string `json:"type"`

	// Attempts is how many evaluation attempts have been made so far.
	// +kubebuilder:validation:Minimum=0
	// +optional
	Attempts int32 `json:"attempts,omitempty"`

	// ConsecutiveErrors is how many consecutive unevaluatable results have
	// been observed. When it reaches the evaluator's MaxConsecutiveErrors
	// threshold the environment fails rather than hanging.
	// +kubebuilder:validation:Minimum=0
	// +optional
	ConsecutiveErrors int32 `json:"consecutiveErrors,omitempty"`

	// Phase is the current state of this probe attempt.
	Phase ProbeAttemptPhase `json:"phase"`

	// LastResult is the outcome of the most recent evaluation:
	// "satisfied", "pending", or "error".
	// +kubebuilder:validation:Enum=satisfied;pending;error
	// +optional
	LastResult string `json:"lastResult,omitempty"`

	// Reason is a short, stable machine reason. See
	// internal/lifecycle/conditions.go's ReasonProbeFailed.
	// +kubebuilder:validation:MaxLength=64
	// +optional
	Reason string `json:"reason,omitempty"`

	// Message is a human explanation. Never contains a credential.
	// +kubebuilder:validation:MaxLength=512
	// +optional
	Message string `json:"message,omitempty"`

	// LastAttemptAt is when the most recent evaluation ran.
	// +optional
	LastAttemptAt *metav1.Time `json:"lastAttemptAt,omitempty"`

	// NextEligibleAt is when the next evaluation may run. The evaluator
	// performs at most one I/O call per reconcile and suppresses calls
	// before this time; the requeue logic wakes the reconciler at it.
	// +optional
	NextEligibleAt *metav1.Time `json:"nextEligibleAt,omitempty"`
}

// AgentOutcome is how the agent reported its run ended.
// +kubebuilder:validation:Enum=Succeeded;Failed
type AgentOutcome string

const (
	AgentOutcomeSucceeded AgentOutcome = "Succeeded"
	AgentOutcomeFailed    AgentOutcome = "Failed"
)

// AgentResultStatus records the agent's own report of how its run ended,
// written by the sandboxctl sidecar on POST /v1/done. This and
// status.waitFor are the ONLY two fields the agent can influence, and it
// influences them only through the sidecar's validated, rate-limited,
// localhost-only API -- the agent container holds no Kubernetes credential.
type AgentResultStatus struct {
	Outcome AgentOutcome `json:"outcome"`
	// Message is the agent's own explanation, truncated to 512 bytes by the
	// sidecar before it is written. Surfaced verbatim in the Ready
	// condition message via lifecycle.ClusterFacts.AgentMessage.
	// +kubebuilder:validation:MaxLength=512
	// +optional
	Message string `json:"message,omitempty"`
	// ExitCode is the process exit code the agent intends to exit with.
	// Advisory only: the pod's real exit code is what
	// internal/controller/podstatus.go observes.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=255
	// +optional
	ExitCode *int32 `json:"exitCode,omitempty"`
	// ReportedAt is when this result was reported. Stamped by the sidecar,
	// never supplied by the agent.
	// +optional
	ReportedAt *metav1.Time `json:"reportedAt,omitempty"`
}

// SnapshotStatus records a single workspace snapshot taken for an
// environment.
type SnapshotStatus struct {
	// Seq is the monotonically increasing sequence number of this
	// snapshot.
	// +kubebuilder:validation:Minimum=0
	Seq int32 `json:"seq"`

	// URI is the location the snapshot was written to.
	// +optional
	URI string `json:"uri,omitempty"`

	// SizeBytes is the size of the snapshot in bytes.
	// +optional
	SizeBytes int64 `json:"sizeBytes,omitempty"`

	// SHA256 is the lowercase hex-encoded SHA-256 checksum of the snapshot.
	// +kubebuilder:validation:Pattern=`^[a-f0-9]{64}$`
	// +optional
	SHA256 string `json:"sha256,omitempty"`

	// TakenAt is when the snapshot was taken.
	// +optional
	TakenAt *metav1.Time `json:"takenAt,omitempty"`

	// DurationMillis is how long the snapshot took, from the start of the
	// freeze hook to the successful latest.json write. Milliseconds, not
	// seconds: a small workspace snapshots in well under a second and a
	// whole-second field would record a meaningless 0.
	// +kubebuilder:validation:Minimum=0
	// +optional
	DurationMillis int64 `json:"durationMillis,omitempty"`
}

// SnapshotAttemptPhase is the state of the freeze snapshot being attempted.
// +kubebuilder:validation:Enum=InProgress;Succeeded;Failed
type SnapshotAttemptPhase string

const (
	SnapshotAttemptInProgress SnapshotAttemptPhase = "InProgress"
	SnapshotAttemptSucceeded  SnapshotAttemptPhase = "Succeeded"
	SnapshotAttemptFailed     SnapshotAttemptPhase = "Failed"
)

// SnapshotAttemptStatus records one freeze's snapshot attempt.
type SnapshotAttemptStatus struct {
	// Seq is the snapshot sequence number this attempt is producing. It
	// always equals status.freezeCount for the freeze in flight.
	// +kubebuilder:validation:Minimum=0
	Seq int32 `json:"seq"`

	Phase SnapshotAttemptPhase `json:"phase"`

	// Attempts is how many upload attempts have been made so far.
	// +kubebuilder:validation:Minimum=0
	// +optional
	Attempts int32 `json:"attempts,omitempty"`

	// Reason is a short, stable machine reason. See
	// internal/sandboxctl/snapshot.go's SnapshotReason* constants.
	// +kubebuilder:validation:MaxLength=64
	// +optional
	Reason string `json:"reason,omitempty"`

	// Message is a human explanation. Never contains a credential -- see
	// internal/storage/doc.go's no-logging rule and credentials.go's
	// Secret redaction.
	// +kubebuilder:validation:MaxLength=512
	// +optional
	Message string `json:"message,omitempty"`

	// StartedAt is when this attempt (this Seq) was first recorded.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`

	// UpdatedAt is when this attempt was last patched (a retry, a phase
	// change).
	// +optional
	UpdatedAt *metav1.Time `json:"updatedAt,omitempty"`
}

// RestoreAttemptPhase is the state of the wake restore being attempted.
// +kubebuilder:validation:Enum=InProgress;Succeeded;Failed
type RestoreAttemptPhase string

const (
	RestoreAttemptInProgress RestoreAttemptPhase = "InProgress"
	RestoreAttemptSucceeded  RestoreAttemptPhase = "Succeeded"
	RestoreAttemptFailed     RestoreAttemptPhase = "Failed"
)

// RestoreAttemptStatus records one wake's restore attempt.
type RestoreAttemptStatus struct {
	// Seq is the snapshot sequence number this attempt is restoring.
	// +kubebuilder:validation:Minimum=0
	Seq int32 `json:"seq"`

	// SnapshotID is the snapshot directory name restored from
	// ("<seq:05d>-<RFC3339>"), so a human can find the exact objects.
	// +kubebuilder:validation:MaxLength=128
	// +optional
	SnapshotID string `json:"snapshotID,omitempty"`

	Phase RestoreAttemptPhase `json:"phase"`

	// Roots records the outcome per restored root. The workspace root is
	// the only one that can ever be Warm: the agent home is an emptyDir
	// that dies with the pod, so it is restored cold on every wake.
	// +optional
	// +listType=map
	// +listMapKey=name
	Roots []RestoredRootStatus `json:"roots,omitempty"`

	// Attempts is how many restore attempts have been made so far.
	// +kubebuilder:validation:Minimum=0
	// +optional
	Attempts int32 `json:"attempts,omitempty"`

	// DurationMillis is how long the restore took.
	// +kubebuilder:validation:Minimum=0
	// +optional
	DurationMillis int64 `json:"durationMillis,omitempty"`

	// Reason is a short, stable machine reason. See
	// internal/sandboxctl/restore.go's RestoreReason* constants.
	// +kubebuilder:validation:MaxLength=64
	// +optional
	Reason string `json:"reason,omitempty"`

	// Message is a human explanation. Never contains a credential.
	// +kubebuilder:validation:MaxLength=512
	// +optional
	Message string `json:"message,omitempty"`

	// StartedAt is when this attempt (this Seq) was first recorded.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
	// UpdatedAt is when this attempt was last patched (a retry, a phase
	// change, or a per-root outcome landing).
	// +optional
	UpdatedAt *metav1.Time `json:"updatedAt,omitempty"`
}

// RestoredRootStatus is one restored root's outcome.
type RestoredRootStatus struct {
	// Name is the restored root: "workspace" (the mounted workspace PVC,
	// the only root that can ever be Warm) or "agent-home" (the per-pod
	// agent home emptyDir, always restored cold).
	// +kubebuilder:validation:Enum=workspace;agent-home
	Name string `json:"name"`

	// Source is Warm when the retained PVC already held this exact
	// snapshot -- validated against the manifest, never inferred from the
	// PVC merely existing -- so no bytes were downloaded for this root.
	// +kubebuilder:validation:Enum=Warm;Cold
	Source string `json:"source"`

	// WarmMissReason, set only when Source is Cold, names why the warm
	// path was refused.
	// +kubebuilder:validation:MaxLength=64
	// +optional
	WarmMissReason string `json:"warmMissReason,omitempty"`

	// BytesDownloaded is the number of uncompressed bytes restored from
	// the backend into this root. Always 0 when Source is Warm -- this is
	// the acceptance criterion's "measurably skips the download", asserted
	// as a value rather than inferred from elapsed time.
	// +kubebuilder:validation:Minimum=0
	// +optional
	BytesDownloaded int64 `json:"bytesDownloaded,omitempty"`
}

// PhaseTransition records one observed phase change.
type PhaseTransition struct {
	// Phase is the phase the environment entered.
	Phase Phase `json:"phase"`

	// At is when the environment was first observed in this phase.
	At metav1.Time `json:"at"`

	// Reason is the Ready condition's Reason at the transition, if any (the
	// summary condition's reason carries the terminal/failure reason).
	// +optional
	Reason string `json:"reason,omitempty"`
}

// ArchiveStatus records the terminal archive written for this environment.
// Written by the sandboxctl archive Job; read by the controller to clear
// the finalizer and by retention GC to select archives past their TTL.
type ArchiveStatus struct {
	// URI is where the archive was written (e.g.
	// s3://<bucket>/<prefix>/<clusterID>/<ns>/<name>/<uid>/archive).
	// +kubebuilder:validation:MinLength=1
	URI string `json:"uri"`

	// FinishedAt is when the run reached its terminal phase (mirrors
	// status.finishedAt; duplicated here so retention GC need not parse
	// run.json to find the run's end time).
	// +optional
	FinishedAt *metav1.Time `json:"finishedAt,omitempty"`

	// ContextPresent is false when no agent-home snapshot existed to draw
	// context.tar.zst from (a never-frozen run whose pod was already gone).
	// +optional
	ContextPresent bool `json:"contextPresent,omitempty"`

	// RunJSONSHA256 is the lowercase hex SHA-256 of run.json, for audit.
	// +kubebuilder:validation:Pattern=`^[a-f0-9]{64}$`
	// +optional
	RunJSONSHA256 string `json:"runJSONSHA256,omitempty"`
}

// GitStateStatus records the agent's final git state, if the agent recorded
// one. Written by the sandboxctl sidecar from
// /workspace/.sandbox/git-state.json during freeze; surfaced in run.json.
type GitStateStatus struct {
	// Branch is the git branch the agent left the workspace on.
	// +optional
	Branch string `json:"branch,omitempty"`

	// HeadSHA is the lowercase hex SHA-1 of HEAD.
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{40}$`
	// +optional
	HeadSHA string `json:"headSHA,omitempty"`

	// PullRequest, if the agent opened/updated one, references it.
	// +optional
	PullRequest *PullRequestRef `json:"pullRequest,omitempty"`
}

// PullRequestRef references a pull request the agent opened or updated.
type PullRequestRef struct {
	// Repo is the repository the pull request belongs to, in "owner/name" form.
	// +kubebuilder:validation:MinLength=1
	Repo string `json:"repo"`

	// Number is the pull request number.
	// +kubebuilder:validation:Minimum=1
	Number int32 `json:"number"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=sbenv,categories=sandbox
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Slot",type=boolean,JSONPath=`.status.slot.granted`
// +kubebuilder:printcolumn:name="Freezes",type=integer,JSONPath=`.status.freezeCount`
// +kubebuilder:printcolumn:name="Wakes",type=integer,JSONPath=".status.wakeCount"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:printcolumn:name="Class",type=string,JSONPath=`.spec.classRef.name`,priority=1
// +kubebuilder:printcolumn:name="Repo",type=string,JSONPath=`.spec.repo`,priority=1

// SandboxEnvironment is a namespaced resource representing a single agent
// run: the class to build the sandbox from, the repository and task the
// agent operates on, and the observed lifecycle state of that run.
type SandboxEnvironment struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// Spec is the desired state of the environment.
	Spec SandboxEnvironmentSpec `json:"spec,omitempty"`

	// Status is the most recently observed state of the environment.
	// +kubebuilder:default={}
	// +optional
	Status SandboxEnvironmentStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// SandboxEnvironmentList is a list of SandboxEnvironment resources.
type SandboxEnvironmentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SandboxEnvironment `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SandboxEnvironment{}, &SandboxEnvironmentList{})
}
