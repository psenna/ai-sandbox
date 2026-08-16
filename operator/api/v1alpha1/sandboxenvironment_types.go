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

	// Snapshot records the most recent workspace snapshot taken for this
	// environment.
	// +optional
	Snapshot *SnapshotStatus `json:"snapshot,omitempty"`

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
// from being restored.
type WaitForStatus struct {
	// Type identifies the kind of condition being waited on.
	// +kubebuilder:validation:Enum=PullRequestMerged;PullRequestChecks;IssueClosed;Duration;Manual
	Type string `json:"type"`

	// Reason is a human-readable explanation of why this wait was
	// declared.
	// +optional
	Reason string `json:"reason,omitempty"`

	// DeclaredAt is when this wait condition was declared.
	// +optional
	DeclaredAt *metav1.Time `json:"declaredAt,omitempty"`

	// Params carries type-specific parameters for the wait condition (for
	// example a pull request number or a duration string).
	// +optional
	Params map[string]string `json:"params,omitempty"`
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
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=sbenv,categories=sandbox
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Slot",type=boolean,JSONPath=`.status.slot.granted`
// +kubebuilder:printcolumn:name="Freezes",type=integer,JSONPath=`.status.freezeCount`
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
