package sandboxctl

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
	"github.com/psenna/ai-sandbox/operator/internal/storage"
)

// Machine-stable reasons for an ABSENT context.tar.zst -- recorded in
// run.json's context record and echoed as status.archive.contextPresent=false.
const (
	// ContextReasonNoSnapshot names a never-frozen run (no status.snapshot):
	// there is no agent-home tarball anywhere to copy from.
	ContextReasonNoSnapshot = "no agent home snapshot"
	// ContextReasonNotInSnapshot names a workspace-only (recovery-Job)
	// snapshot: agent-home.tar.zst is absent from the snapshot directory.
	ContextReasonNotInSnapshot = "agent home not in snapshot"
)

// conditionReady is the summary condition whose Reason run.json records as
// FinalReason. Mirrors lifecycle.ConditionReady, restated here to avoid
// importing the lifecycle package (a pure state machine sandboxctl has no
// other business importing).
const conditionReady = "Ready"

// RunArchive builds the same client/Store/backend the sidecar uses, refreshes
// once, and runs an ArchiveRunner -- the archive Job's entire program
// (cmd/sandboxctl's "archive" subcommand). "Archive creation inside a pod
// must be a sandboxctl command" (#32): the controller never assembles
// run.json and never touches the backend; it only creates this Job and reads
// status.archive to clear the finalizer. The archive subcommand exits 0 even
// when context.tar.zst is omitted (a never-frozen run is not a failure —
// run.json is still a complete record); it exits non-zero only on a real
// backend/CR error, so the Job retries via BackoffLimit.
func RunArchive(ctx context.Context, cfg Config, log logr.Logger) error {
	c, err := buildClient()
	if err != nil {
		return err
	}

	store := NewStore(c, cfg.Namespace, cfg.Environment)
	if err := store.Refresh(ctx); err != nil {
		return fmt.Errorf("refreshing environment %s/%s: %w", cfg.Namespace, cfg.Environment, err)
	}
	snap := store.Snapshot()

	runner, err := buildArchiveRunner(store, cfg, log)
	if err != nil {
		return err
	}

	if err := runner.Archive(ctx, snap); err != nil {
		return fmt.Errorf("archive: %w", err)
	}
	return nil
}

// buildArchiveRunner builds an ArchiveRunner. Unlike the freeze/restore hooks
// (which fail closed on "pvc" at freeze/restore time, inside the pod), the
// archive Job runs in its own pod with no backend PVC mounted, so a non-s3
// backend is rejected here, before anything runs.
func buildArchiveRunner(store Store, cfg Config, log logr.Logger) (*ArchiveRunner, error) {
	if cfg.Snapshot.Backend != "s3" {
		return nil, fmt.Errorf("snapshot-backend=%q is not supported by the archive subcommand (only s3): the archive Job mounts no workspace or backend PVC", cfg.Snapshot.Backend)
	}
	be, err := BuildBackend(cfg.Snapshot)
	if err != nil {
		return nil, fmt.Errorf("building archive backend: %w", err)
	}
	return NewArchiveRunner(store, be, cfg.Snapshot, log), nil
}

// ArchiveRunner is the real archive producer: it assembles run.json from the
// environment's status, streams the most recent freeze snapshot's
// agent-home.tar.zst into archive/context.tar.zst, uploads both under
// Layout.ArchivePrefix(), and patches status.archive/status.archiveURI.
type ArchiveRunner struct {
	Store   Store
	Backend storage.Backend
	Cfg     SnapshotConfig
	Now     func() time.Time
	Log     logr.Logger
}

// NewArchiveRunner builds an ArchiveRunner with a real Now implementation
// when the field is left unset by the caller.
func NewArchiveRunner(store Store, be storage.Backend, cfg SnapshotConfig, log logr.Logger) *ArchiveRunner {
	return &ArchiveRunner{Store: store, Backend: be, Cfg: cfg, Now: time.Now, Log: log}
}

// Archive performs the terminal archive: assemble run.json, copy the context
// tarball from the most recent snapshot's agent-home.tar.zst, upload both,
// and record status.archive. Exit-0 idempotency: an environment whose
// status.archive is already set AND whose archive/run.json still exists is
// left untouched (the Job re-ran after the controller restarted); if the
// object was deleted out from under us, we re-write it.
func (a *ArchiveRunner) Archive(ctx context.Context, s Snapshot) error {
	env := s.Env
	if env.Name == "" || env.Namespace == "" || string(env.UID) == "" {
		return fmt.Errorf("refreshed environment snapshot is incomplete (name=%q namespace=%q uid=%q)",
			env.Name, env.Namespace, string(env.UID))
	}

	layout, err := snapshotLayout(a.Cfg, env.Namespace, env.Name, string(env.UID))
	if err != nil {
		return fmt.Errorf("building archive layout: %w", err)
	}

	// Idempotency: a recorded archive that still exists is a no-op. Only the
	// Stat matters -- the status write is authoritative and was made by the
	// SAME run that uploaded the objects, so a present run.json implies the
	// whole archive landed (run.json is uploaded last, after context).
	if env.Status.Archive != nil {
		if _, err := a.Backend.Stat(ctx, layout.ArchiveRun()); err == nil {
			a.Log.Info("archive already present; nothing to do", "uri", env.Status.Archive.URI)
			return nil
		}
		a.Log.Info("status.archive set but archive/run.json missing; re-writing", "uri", env.Status.Archive.URI)
	}

	rec := a.assembleRunRecord(env)
	rec.Context, err = a.contextRecord(ctx, layout, env.Status.Snapshot)
	if err != nil {
		return err
	}

	// Validate BEFORE uploading: an unreadable run.json (e.g. a zero
	// timestamp in a hand-edited phase history) must fail the Job loudly and
	// hold the finalizer, not poison the archive with a corrupt record.
	if err := rec.Validate(); err != nil {
		return fmt.Errorf("assembled run record is invalid: %w", err)
	}
	b, err := rec.Marshal()
	if err != nil {
		return fmt.Errorf("marshaling run record: %w", err)
	}
	info, err := a.Backend.Put(ctx, layout.ArchiveRun(), bytes.NewReader(b), storage.PutOptions{ContentType: "application/json"})
	if err != nil {
		return fmt.Errorf("uploading run.json: %w", err)
	}

	// Publish last -- the controller removes the finalizer once status.archive
	// is non-nil, so a status write that fails leaves retryable objects.
	if err := a.Store.PatchArchive(ctx, ArchivePatch{
		URI:            archiveURI(a.Cfg.S3Bucket, layout),
		FinishedAt:     env.Status.FinishedAt,
		ContextPresent: rec.Context.Present,
		RunJSONSHA256:  info.SHA256,
	}); err != nil {
		return fmt.Errorf("recording archive: %w", err)
	}
	return nil
}

// contextRecord produces run.json's context record for status.snapshot by
// streaming the snapshot's agent-home.tar.zst into archive/context.tar.zst.
// The live emptyDir is never read -- transcripts survive only inside the most
// recent freeze snapshot (the controller guarantees one exists before a live
// pod is archived; see internal/controller/archive.go's freeze detour).
//
// Only two outcomes omit the context as a SUCCESS: no snapshot at all (a
// never-frozen run) and an ErrNotFound on agent-home.tar.zst (a
// workspace-only recovery-Job snapshot). Any other backend error FAILS the
// archive -- a transient outage must retry, never silently drop the
// transcripts.
func (a *ArchiveRunner) contextRecord(ctx context.Context, layout storage.Layout, snap *v1alpha1.SnapshotStatus) (storage.ContextRecord, error) {
	if snap == nil || snap.TakenAt == nil {
		return storage.ContextRecord{Present: false, Reason: ContextReasonNoSnapshot}, nil
	}
	srcKey := layout.AgentHome(int(snap.Seq), snap.TakenAt.Time)
	rc, _, err := a.Backend.Get(ctx, srcKey)
	switch {
	case storage.IsNotFound(err):
		// The recovery-Job case: a workspace-only snapshot whose agent home
		// was never captured. Honest omission, not an error.
		a.Log.Info("agent home archive missing from snapshot; context omitted", "key", srcKey)
		return storage.ContextRecord{Present: false, Reason: ContextReasonNotInSnapshot}, nil
	case err != nil:
		return storage.ContextRecord{}, fmt.Errorf("reading agent-home archive %s: %w", srcKey, err)
	}
	defer func() { _ = rc.Close() }()

	info, err := a.Backend.Put(ctx, layout.ArchiveContext(), rc, storage.PutOptions{ContentType: "application/octet-stream"})
	if err != nil {
		return storage.ContextRecord{}, fmt.Errorf("uploading context.tar.zst: %w", err)
	}
	return storage.ContextRecord{
		Present:   true,
		URI:       contextURI(a.Cfg.S3Bucket, layout),
		SizeBytes: info.Size,
		SHA256:    info.SHA256,
	}, nil
}

// assembleRunRecord turns the environment's status onto a RunRecord. Purely a
// shape conversion: every value is copied or derived from the CR, nothing is
// read from disk or the backend. Durations are computed here (see
// durationMillis helpers); snapshots come from status.snapshot, which records
// the most recent freeze (earlier snapshots remain in the backend's
// snapshots/ prefix but are intentionally not enumerated here, so a messy
// backend can never fail the delete-path archive).
func (a *ArchiveRunner) assembleRunRecord(env v1alpha1.SandboxEnvironment) storage.RunRecord {
	rec := storage.RunRecord{
		SchemaVersion: storage.RunRecordSchemaVersion,
		CreatedAt:     a.Now().UTC().Truncate(time.Second),
		ClusterID:     a.Cfg.ClusterID,
		Namespace:     env.Namespace,
		Name:          env.Name,
		UID:           string(env.UID),
		Generation:    env.Generation,
		Repo:          env.Spec.Repo,
		ClassRef:      env.Spec.ClassRef.Name,
		AgentImage:    a.Cfg.AgentImage,
		FinalPhase:    string(env.Status.Phase),
		FinalReason:   readyConditionReason(env.Status.Conditions),
		FreezeCount:   env.Status.FreezeCount,
		WakeCount:     env.Status.WakeCount,
	}
	if t := env.Spec.Task; t.Prompt != "" || t.IssueRef != nil {
		task := storage.TaskRecord{Prompt: t.Prompt}
		if t.IssueRef != nil {
			task.IssueRef = &storage.IssueRecord{Repo: t.IssueRef.Repo, Number: t.IssueRef.Number}
		}
		rec.Task = task
	}
	rec.PhaseHistory = make([]storage.PhaseTransitionRecord, 0, len(env.Status.PhaseHistory))
	for _, ph := range env.Status.PhaseHistory {
		rec.PhaseHistory = append(rec.PhaseHistory, storage.PhaseTransitionRecord{
			Phase:  string(ph.Phase),
			At:     ph.At.Time,
			Reason: ph.Reason,
		})
	}
	rec.QueuedDurationMillis = betweenMillis(env.Status.QueuedSince, env.Status.StartedAt)
	rec.RunningDurationMillis = betweenMillis(env.Status.StartedAt, env.Status.FinishedAt)
	rec.WaitingDurationMillis = waitingDurationMillis(env.Status.PhaseHistory)
	rec.SlotWaitMillis = betweenMillis(env.Status.QueuedSince, env.Status.Slot.GrantedAt)
	if s := env.Status.Snapshot; s != nil {
		var takenAt time.Time
		if s.TakenAt != nil {
			takenAt = s.TakenAt.Time
		}
		rec.Snapshots = []storage.SnapshotRecord{{
			Seq:            s.Seq,
			URI:            s.URI,
			SizeBytes:      s.SizeBytes,
			SHA256:         s.SHA256,
			TakenAt:        takenAt,
			DurationMillis: s.DurationMillis,
		}}
	}
	if w := env.Status.WaitFor; w != nil {
		wr := &storage.WaitForRecord{Type: w.Type, Reason: w.Reason, Params: w.Params}
		if w.DeclaredAt != nil {
			t := w.DeclaredAt.Time
			wr.DeclaredAt = &t
		}
		rec.WaitFor = wr
	}
	if p := env.Status.ProbeAttempt; p != nil {
		pr := &storage.ProbeAttemptRecord{
			Type:              p.Type,
			Phase:             string(p.Phase),
			Attempts:          p.Attempts,
			ConsecutiveErrors: p.ConsecutiveErrors,
			LastResult:        p.LastResult,
			Reason:            p.Reason,
			Message:           p.Message,
		}
		if p.LastAttemptAt != nil {
			t := p.LastAttemptAt.Time
			pr.LastAttemptAt = &t
		}
		rec.ProbeAttempt = pr
	}
	if g := env.Status.GitState; g != nil {
		gr := &storage.GitStateRecord{Branch: g.Branch, HeadSHA: g.HeadSHA}
		if g.PullRequest != nil {
			gr.PullRequest = &storage.PullRequestRecord{Repo: g.PullRequest.Repo, Number: g.PullRequest.Number}
		}
		rec.GitState = gr
	}
	return rec
}

// readyConditionReason returns the summary (Ready) condition's Reason, the
// terminal/failure reason a human sees on the CR. Mirrors the plan's "the
// Ready condition only carries the LastTransitionTime for the *current*
// phase" rationale: the summary reason is the most honest FinalReason.
func readyConditionReason(conds []metav1.Condition) string {
	if c := apimeta.FindStatusCondition(conds, conditionReady); c != nil {
		return c.Reason
	}
	return ""
}

// betweenMillis returns d2.Sub(d1) in whole milliseconds, or 0 when either
// bound is missing or the interval is negative (the timestamps are
// set-once/causal, but a hand-edited status must not record negative
// durations).
func betweenMillis(d1, d2 *metav1.Time) int64 {
	if d1 == nil || d2 == nil {
		return 0
	}
	d := d2.Sub(d1.Time)
	if d < 0 {
		return 0
	}
	return d.Milliseconds()
}

// waitingDurationMillis sums the time spent in the Waiting phase from the
// phase-transition history: for every consecutive pair whose earlier phase is
// Waiting, the span to the next transition. A trailing Waiting entry (still
// waiting when archived) contributes nothing.
func waitingDurationMillis(history []v1alpha1.PhaseTransition) int64 {
	var total int64
	for i := 0; i+1 < len(history); i++ {
		if history[i].Phase != v1alpha1.PhaseWaiting {
			continue
		}
		d := history[i+1].At.Sub(history[i].At.Time)
		if d < 0 {
			continue
		}
		total += d.Milliseconds()
	}
	return total
}

// archiveURI formats "s3://<bucket>/<layout.ArchivePrefix()>".
func archiveURI(bucket string, layout storage.Layout) string {
	return fmt.Sprintf("s3://%s/%s", bucket, layout.ArchivePrefix())
}

// contextURI is the key of the uploaded context.tar.zst, for run.json's
// context.uri.
func contextURI(bucket string, layout storage.Layout) string {
	return fmt.Sprintf("s3://%s/%s", bucket, layout.ArchiveContext())
}
