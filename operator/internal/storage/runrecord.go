package storage

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// RunRecordSchemaVersion is the only schema version this package
// understands. ParseRunRecord rejects any other value.
const RunRecordSchemaVersion = 1

// maxPhaseHistoryEntries mirrors the CRD's +kubebuilder:validation:MaxItems=64
// cap on status.phaseHistory, so a record built from an over-capped status
// fails Validate rather than silently truncating.
const maxPhaseHistoryEntries = 64

// RunRecord is the terminal archive's run.json: a versioned, deterministic
// JSON record of one environment's entire run -- identity, task, class,
// image, the full phase-transition history with timestamps, durations,
// counts, snapshots, the declared wait + final probe evaluation, the final
// git state, and whether the agent-home context tarball was captured.
//
// It is assembled by the sandboxctl archive subcommand (internal/sandboxctl/
// archive.go) from the SandboxEnvironment's status, NOT by the controller --
// "archive creation inside a pod must be a sandboxctl command" (#32). The
// controller only creates the Job and reads status.archive.
type RunRecord struct {
	SchemaVersion int       `json:"schemaVersion"`
	CreatedAt     time.Time `json:"createdAt"` // archive creation time

	// --- identity ---
	ClusterID  string `json:"clusterID"`
	Namespace  string `json:"namespace"`
	Name       string `json:"name"`
	UID        string `json:"uid"`
	Generation int64  `json:"generation,omitempty"`

	// --- task + repo ---
	Repo     string     `json:"repo"`
	Task     TaskRecord `json:"task"`
	ClassRef string     `json:"classRef"` // the SandboxClass name

	// --- class + image ---
	AgentImage       string `json:"agentImage"`
	AgentImageDigest string `json:"agentImageDigest,omitempty"` // best-effort; "" if unresolved

	// --- final phase + reason ---
	// FinalPhase is the phase the environment was in when the archive was
	// written: a terminal phase (Done/Failed) for a completed run, or the
	// mid-run phase for an environment archived on delete. Empty ONLY for an
	// environment deleted before its first reconcile ever wrote a status.
	FinalPhase  string `json:"finalPhase"`
	FinalReason string `json:"finalReason,omitempty"`

	// --- phase-transition history with timestamps ---
	PhaseHistory []PhaseTransitionRecord `json:"phaseHistory,omitempty"`

	// --- durations ---
	QueuedDurationMillis  int64 `json:"queuedDurationMillis,omitempty"`
	RunningDurationMillis int64 `json:"runningDurationMillis,omitempty"`
	WaitingDurationMillis int64 `json:"waitingDurationMillis,omitempty"`
	SlotWaitMillis        int64 `json:"slotWaitMillis,omitempty"` // QueuedSince -> slot.grantedAt

	// --- counts ---
	FreezeCount int32 `json:"freezeCount"`
	WakeCount   int32 `json:"wakeCount"`

	// --- snapshots ---
	Snapshots []SnapshotRecord `json:"snapshots,omitempty"` // every snapshot's seq, uri, size, sha256, takenAt, durationMillis

	// --- probes declared + outcomes ---
	WaitFor      *WaitForRecord      `json:"waitFor,omitempty"`      // the declared wait, if any
	ProbeAttempt *ProbeAttemptRecord `json:"probeAttempt,omitempty"` // the final evaluation

	// --- final git state ---
	GitState *GitStateRecord `json:"gitState,omitempty"`

	// --- archive contents ---
	Context ContextRecord `json:"context"`
}

// TaskRecord is the task the agent was given.
type TaskRecord struct {
	Prompt   string       `json:"prompt,omitempty"`
	IssueRef *IssueRecord `json:"issueRef,omitempty"`
}

// IssueRecord references a single issue in a repository.
type IssueRecord struct {
	Repo   string `json:"repo"`
	Number int32  `json:"number"`
}

// PhaseTransitionRecord is one observed phase change.
type PhaseTransitionRecord struct {
	Phase  string    `json:"phase"`
	At     time.Time `json:"at"`
	Reason string    `json:"reason,omitempty"`
}

// SnapshotRecord is one snapshot taken for the environment.
type SnapshotRecord struct {
	Seq            int32     `json:"seq"`
	URI            string    `json:"uri,omitempty"`
	SizeBytes      int64     `json:"sizeBytes,omitempty"`
	SHA256         string    `json:"sha256,omitempty"`
	TakenAt        time.Time `json:"takenAt,omitempty"`
	DurationMillis int64     `json:"durationMillis,omitempty"`
}

// WaitForRecord is the declared external wait condition.
type WaitForRecord struct {
	Type       string            `json:"type"`
	Reason     string            `json:"reason,omitempty"`
	DeclaredAt *time.Time        `json:"declaredAt,omitempty"`
	Params     map[string]string `json:"params,omitempty"`
}

// ProbeAttemptRecord is the operator's final evaluation of the declared wait.
type ProbeAttemptRecord struct {
	Type              string     `json:"type"`
	Phase             string     `json:"phase"`
	Attempts          int32      `json:"attempts,omitempty"`
	ConsecutiveErrors int32      `json:"consecutiveErrors,omitempty"`
	LastResult        string     `json:"lastResult,omitempty"`
	Reason            string     `json:"reason,omitempty"`
	Message           string     `json:"message,omitempty"`
	LastAttemptAt     *time.Time `json:"lastAttemptAt,omitempty"`
}

// GitStateRecord is the agent's final git state, if it recorded one.
type GitStateRecord struct {
	Branch      string             `json:"branch,omitempty"`
	HeadSHA     string             `json:"headSHA,omitempty"`
	PullRequest *PullRequestRecord `json:"pullRequest,omitempty"`
}

// PullRequestRecord references a pull request the agent opened or updated.
type PullRequestRecord struct {
	Repo   string `json:"repo"`
	Number int32  `json:"number"`
}

// ContextRecord describes the archive's context.tar.zst: whether the agent's
// home (transcripts) was captured, and if not, why.
type ContextRecord struct {
	Present bool   `json:"present"`
	URI     string `json:"uri,omitempty"`
	// SizeBytes and SHA256 describe the uploaded context.tar.zst; set only
	// when Present. SHA256 is the lowercase hex SHA-256 of the tarball as
	// uploaded, so an auditor can verify a downloaded copy byte-for-byte.
	SizeBytes int64  `json:"sizeBytes,omitempty"`
	SHA256    string `json:"sha256,omitempty"`
	// Reason explains an absent context: "no agent home snapshot" when the
	// environment never froze, or "agent home not in snapshot" when the most
	// recent snapshot was a workspace-only recovery-Job snapshot.
	Reason string `json:"reason,omitempty"`
}

// runRecordPhases is every valid Phase value. Kept as plain strings so the
// record schema stays a standalone document even though the CRD enum lives
// in api/v1alpha1; if a phase is ever added, both must be updated.
var runRecordPhases = map[string]bool{
	"Pending":   true,
	"Ready":     true,
	"Running":   true,
	"Freezing":  true,
	"Waiting":   true,
	"Restoring": true,
	"Done":      true,
	"Failed":    true,
}

// Marshal serializes r deterministically: 2-space indent, HTML escaping
// disabled, a single trailing newline, matching Manifest.Marshal's
// conventions. Slices are preserved in their recorded order -- PhaseHistory
// and Snapshots are chronological, not sortable. Calling Marshal twice on
// equal values always produces byte-identical output.
func (r RunRecord) Marshal() ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(r); err != nil {
		return nil, fmt.Errorf("storage: marshaling run record: %w", err)
	}
	return buf.Bytes(), nil
}

// ParseRunRecord decodes and validates a run record from r. Unknown fields
// are rejected (DisallowUnknownFields) so a record written by a newer schema
// version fails loudly instead of being silently truncated.
func ParseRunRecord(r io.Reader) (RunRecord, error) {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	var rec RunRecord
	if err := dec.Decode(&rec); err != nil {
		return RunRecord{}, fmt.Errorf("storage: parsing run record: %w", err)
	}
	if err := rec.Validate(); err != nil {
		return RunRecord{}, err
	}
	return rec, nil
}

// Validate checks structural well-formedness. Identity fields (clusterID,
// namespace, name, uid) must be non-empty; FinalPhase, when set, must be a
// known Phase; PhaseHistory, when present, must be within the CRD cap, have
// valid phases, non-zero timestamps and no consecutive duplicates; snapshot
// seqs must be unique; and a present context must name its URI. An empty
// PhaseHistory and empty FinalPhase are BOTH permitted, because an
// environment deleted before its first reconcile is an honest "never
// observed" record -- the plan (#32) documents that adjustment explicitly.
func (r RunRecord) Validate() error {
	if r.SchemaVersion != RunRecordSchemaVersion {
		return fmt.Errorf("%w: run record schemaVersion %d, want %d", ErrInvalid, r.SchemaVersion, RunRecordSchemaVersion)
	}
	if r.CreatedAt.IsZero() {
		return fmt.Errorf("%w: run record createdAt must not be zero", ErrInvalid)
	}
	if r.ClusterID == "" || r.Namespace == "" || r.Name == "" || r.UID == "" {
		return fmt.Errorf("%w: run record identity fields (clusterID, namespace, name, uid) must all be non-empty", ErrInvalid)
	}
	if r.Generation < 0 {
		return fmt.Errorf("%w: run record generation %d must be >= 0", ErrInvalid, r.Generation)
	}
	if r.Repo == "" {
		return fmt.Errorf("%w: run record repo must not be empty", ErrInvalid)
	}
	if r.ClassRef == "" {
		return fmt.Errorf("%w: run record classRef must not be empty", ErrInvalid)
	}
	if r.AgentImage == "" {
		return fmt.Errorf("%w: run record agentImage must not be empty", ErrInvalid)
	}
	if r.FinalPhase != "" && !runRecordPhases[r.FinalPhase] {
		return fmt.Errorf("%w: run record finalPhase %q is not a known phase", ErrInvalid, r.FinalPhase)
	}
	if r.FreezeCount < 0 {
		return fmt.Errorf("%w: run record freezeCount %d must be >= 0", ErrInvalid, r.FreezeCount)
	}
	if r.WakeCount < 0 {
		return fmt.Errorf("%w: run record wakeCount %d must be >= 0", ErrInvalid, r.WakeCount)
	}
	if len(r.PhaseHistory) > maxPhaseHistoryEntries {
		return fmt.Errorf("%w: run record phaseHistory has %d entries, cap is %d", ErrInvalid, len(r.PhaseHistory), maxPhaseHistoryEntries)
	}
	var prev string
	for i, t := range r.PhaseHistory {
		if t.Phase == "" {
			return fmt.Errorf("%w: run record phaseHistory[%d] has empty phase", ErrInvalid, i)
		}
		if !runRecordPhases[t.Phase] {
			return fmt.Errorf("%w: run record phaseHistory[%d] phase %q is not a known phase", ErrInvalid, i, t.Phase)
		}
		if t.At.IsZero() {
			return fmt.Errorf("%w: run record phaseHistory[%d] has zero at timestamp", ErrInvalid, i)
		}
		if i > 0 && t.Phase == prev {
			return fmt.Errorf("%w: run record phaseHistory[%d] duplicates previous phase %q", ErrInvalid, i, t.Phase)
		}
		prev = t.Phase
	}
	seenSeq := make(map[int32]struct{}, len(r.Snapshots))
	for i, s := range r.Snapshots {
		if s.Seq < 0 {
			return fmt.Errorf("%w: run record snapshots[%d] has negative seq %d", ErrInvalid, i, s.Seq)
		}
		if _, dup := seenSeq[s.Seq]; dup {
			return fmt.Errorf("%w: run record has duplicate snapshot seq %d", ErrInvalid, s.Seq)
		}
		seenSeq[s.Seq] = struct{}{}
		if s.SHA256 != "" && !hex64.MatchString(s.SHA256) {
			return fmt.Errorf("%w: run record snapshots[%d] has malformed sha256 %q", ErrInvalid, i, s.SHA256)
		}
		if s.SizeBytes < 0 {
			return fmt.Errorf("%w: run record snapshots[%d] has negative sizeBytes %d", ErrInvalid, i, s.SizeBytes)
		}
		if s.DurationMillis < 0 {
			return fmt.Errorf("%w: run record snapshots[%d] has negative durationMillis %d", ErrInvalid, i, s.DurationMillis)
		}
	}
	if r.Context.Present && r.Context.URI == "" {
		return fmt.Errorf("%w: run record context.present is true but context.uri is empty", ErrInvalid)
	}
	if r.Context.SizeBytes < 0 {
		return fmt.Errorf("%w: run record context.sizeBytes %d must be >= 0", ErrInvalid, r.Context.SizeBytes)
	}
	if r.Context.SHA256 != "" && !hex64.MatchString(r.Context.SHA256) {
		return fmt.Errorf("%w: run record context has malformed sha256 %q", ErrInvalid, r.Context.SHA256)
	}
	if !r.Context.Present && r.Context.Reason == "" {
		return fmt.Errorf("%w: run record context is absent but context.reason is empty", ErrInvalid)
	}
	if r.WaitFor != nil {
		if r.WaitFor.Type == "" {
			return fmt.Errorf("%w: run record waitFor.type must not be empty", ErrInvalid)
		}
		if r.WaitFor.DeclaredAt != nil && r.WaitFor.DeclaredAt.IsZero() {
			return fmt.Errorf("%w: run record waitFor.declaredAt must not be zero", ErrInvalid)
		}
	}
	if r.ProbeAttempt != nil {
		if r.ProbeAttempt.Type == "" {
			return fmt.Errorf("%w: run record probeAttempt.type must not be empty", ErrInvalid)
		}
		if r.ProbeAttempt.Phase == "" {
			return fmt.Errorf("%w: run record probeAttempt.phase must not be empty", ErrInvalid)
		}
		if r.ProbeAttempt.Attempts < 0 {
			return fmt.Errorf("%w: run record probeAttempt.attempts %d must be >= 0", ErrInvalid, r.ProbeAttempt.Attempts)
		}
		if r.ProbeAttempt.ConsecutiveErrors < 0 {
			return fmt.Errorf("%w: run record probeAttempt.consecutiveErrors %d must be >= 0", ErrInvalid, r.ProbeAttempt.ConsecutiveErrors)
		}
	}
	if r.GitState != nil {
		if r.GitState.HeadSHA != "" && !hex40.MatchString(r.GitState.HeadSHA) {
			return fmt.Errorf("%w: run record gitState.headSHA %q is not 40 hex characters", ErrInvalid, r.GitState.HeadSHA)
		}
		if r.GitState.PullRequest != nil {
			if r.GitState.PullRequest.Repo == "" {
				return fmt.Errorf("%w: run record gitState.pullRequest.repo must not be empty", ErrInvalid)
			}
			if r.GitState.PullRequest.Number < 1 {
				return fmt.Errorf("%w: run record gitState.pullRequest.number %d must be >= 1", ErrInvalid, r.GitState.PullRequest.Number)
			}
		}
	}
	return nil
}
