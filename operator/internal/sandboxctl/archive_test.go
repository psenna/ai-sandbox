package sandboxctl

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
	"github.com/psenna/ai-sandbox/operator/internal/storage"
)

// archiveFakeStore is a minimal Store double for ArchiveRunner tests: unlike
// fakeSnapshotStore (whose Snapshot() is hardcoded to Snapshot{}), this fake
// returns a caller-supplied Snapshot (carrying the .Env the archive
// subcommand assembles run.json from) and records the PatchArchive call for
// assertions.
type archiveFakeStore struct {
	snap     Snapshot
	patched  *ArchivePatch
	patchErr error
}

func (s *archiveFakeStore) Snapshot() Snapshot                                      { return s.snap }
func (s *archiveFakeStore) Refresh(context.Context) error                           { return nil }
func (s *archiveFakeStore) DeclareWait(context.Context, WaitProbe, time.Time) error { return nil }
func (s *archiveFakeStore) ReportDone(context.Context, Result, time.Time) (bool, error) {
	return false, nil
}
func (s *archiveFakeStore) RecordSnapshotAttempt(context.Context, SnapshotAttempt, time.Time) error {
	return nil
}
func (s *archiveFakeStore) RecordSnapshot(context.Context, SnapshotRecord, time.Time) error {
	return nil
}
func (s *archiveFakeStore) RecordRestoreAttempt(context.Context, RestoreAttempt, time.Time) error {
	return nil
}
func (s *archiveFakeStore) PatchArchive(_ context.Context, a ArchivePatch) error {
	if s.patchErr != nil {
		return s.patchErr
	}
	ap := a
	s.patched = &ap
	return nil
}

func testArchiveLogger() logr.Logger {
	return logr.FromSlogHandler(slog.NewJSONHandler(io.Discard, nil))
}

func testArchiveCfg() SnapshotConfig {
	return SnapshotConfig{
		ClusterID:  "default",
		AgentImage: "agent:latest",
		Backend:    "s3",
		S3Bucket:   "test-bucket",
	}
}

func testArchiveEnv() v1alpha1.SandboxEnvironment {
	finishedAt := metav1.NewTime(time.Date(2026, 1, 1, 13, 0, 0, 0, time.UTC))
	startedAt := metav1.NewTime(time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC))
	return v1alpha1.SandboxEnvironment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "env-a",
			Namespace: "ns-a",
			UID:       types.UID("uid-a"),
		},
		Spec: v1alpha1.SandboxEnvironmentSpec{
			ClassRef: v1alpha1.ClassRef{Name: "class-a"},
			Repo:     "owner/repo",
			Task:     v1alpha1.TaskSpec{Prompt: "do the thing"},
		},
		Status: v1alpha1.SandboxEnvironmentStatus{
			Phase:      v1alpha1.PhaseDone,
			StartedAt:  &startedAt,
			FinishedAt: &finishedAt,
			Conditions: []metav1.Condition{{
				Type:               conditionReady,
				Status:             metav1.ConditionTrue,
				Reason:             "AgentSucceeded",
				LastTransitionTime: finishedAt,
			}},
			PhaseHistory: []v1alpha1.PhaseTransition{
				{Phase: v1alpha1.PhaseRunning, At: startedAt},
				{Phase: v1alpha1.PhaseDone, At: finishedAt, Reason: "AgentSucceeded"},
			},
		},
	}
}

func TestArchiveRunner_NeverFrozen_ContextOmitted(t *testing.T) {
	be := newFakeBackend()
	store := &archiveFakeStore{snap: Snapshot{Env: testArchiveEnv()}}
	runner := NewArchiveRunner(store, be, testArchiveCfg(), testArchiveLogger())

	if err := runner.Archive(context.Background(), store.snap); err != nil {
		t.Fatalf("Archive: %v", err)
	}

	if store.patched == nil {
		t.Fatal("PatchArchive was not called")
	}
	if store.patched.ContextPresent {
		t.Error("ContextPresent = true, want false (never-frozen run)")
	}
	if store.patched.URI == "" {
		t.Error("patched URI is empty")
	}
	if store.patched.RunJSONSHA256 == "" {
		t.Error("patched RunJSONSHA256 is empty")
	}

	layout, err := snapshotLayout(testArchiveCfg(), "ns-a", "env-a", "uid-a")
	if err != nil {
		t.Fatalf("snapshotLayout: %v", err)
	}
	if !be.has(layout.ArchiveRun()) {
		t.Error("run.json was not uploaded")
	}
	if be.has(layout.ArchiveContext()) {
		t.Error("context.tar.zst was uploaded for a never-frozen run")
	}
}

func TestArchiveRunner_WithSnapshot_CopiesContext(t *testing.T) {
	be := newFakeBackend()
	cfg := testArchiveCfg()
	env := testArchiveEnv()
	takenAt := metav1.NewTime(time.Date(2026, 1, 1, 12, 30, 0, 0, time.UTC))
	env.Status.Snapshot = &v1alpha1.SnapshotStatus{Seq: 0, TakenAt: &takenAt}

	layout, err := snapshotLayout(cfg, env.Namespace, env.Name, string(env.UID))
	if err != nil {
		t.Fatalf("snapshotLayout: %v", err)
	}
	agentHomeKey := layout.AgentHome(0, takenAt.Time)
	if _, err := be.Put(context.Background(), agentHomeKey, strings.NewReader("agent home bytes"), storage.PutOptions{}); err != nil {
		t.Fatalf("seeding agent-home object: %v", err)
	}

	store := &archiveFakeStore{snap: Snapshot{Env: env}}
	runner := NewArchiveRunner(store, be, cfg, testArchiveLogger())

	if err := runner.Archive(context.Background(), store.snap); err != nil {
		t.Fatalf("Archive: %v", err)
	}

	if store.patched == nil {
		t.Fatal("PatchArchive was not called")
	}
	if !store.patched.ContextPresent {
		t.Error("ContextPresent = false, want true")
	}
	if !be.has(layout.ArchiveContext()) {
		t.Error("context.tar.zst was not uploaded")
	}
}

func TestArchiveRunner_SnapshotWithoutAgentHome_ContextOmitted(t *testing.T) {
	be := newFakeBackend()
	cfg := testArchiveCfg()
	env := testArchiveEnv()
	takenAt := metav1.NewTime(time.Date(2026, 1, 1, 12, 30, 0, 0, time.UTC))
	// A workspace-only recovery-Job snapshot: status.snapshot is set, but no
	// agent-home.tar.zst was ever uploaded for it.
	env.Status.Snapshot = &v1alpha1.SnapshotStatus{Seq: 0, TakenAt: &takenAt}

	store := &archiveFakeStore{snap: Snapshot{Env: env}}
	runner := NewArchiveRunner(store, be, cfg, testArchiveLogger())

	if err := runner.Archive(context.Background(), store.snap); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	if store.patched == nil {
		t.Fatal("PatchArchive was not called")
	}
	if store.patched.ContextPresent {
		t.Error("ContextPresent = true, want false (agent home missing from snapshot)")
	}
}

func TestArchiveRunner_AlreadyArchived_RunJSONPresent_NoOp(t *testing.T) {
	be := newFakeBackend()
	cfg := testArchiveCfg()
	env := testArchiveEnv()

	layout, err := snapshotLayout(cfg, env.Namespace, env.Name, string(env.UID))
	if err != nil {
		t.Fatalf("snapshotLayout: %v", err)
	}
	if _, err := be.Put(context.Background(), layout.ArchiveRun(), strings.NewReader("{}"), storage.PutOptions{}); err != nil {
		t.Fatalf("seeding run.json: %v", err)
	}
	env.Status.Archive = &v1alpha1.ArchiveStatus{URI: "s3://test-bucket/" + layout.ArchivePrefix()}

	store := &archiveFakeStore{snap: Snapshot{Env: env}}
	runner := NewArchiveRunner(store, be, cfg, testArchiveLogger())

	if err := runner.Archive(context.Background(), store.snap); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	if store.patched != nil {
		t.Error("PatchArchive was called, want no-op (archive already present)")
	}
}

func TestArchiveRunner_AlreadyArchived_RunJSONMissing_Rewrites(t *testing.T) {
	be := newFakeBackend()
	cfg := testArchiveCfg()
	env := testArchiveEnv()

	layout, err := snapshotLayout(cfg, env.Namespace, env.Name, string(env.UID))
	if err != nil {
		t.Fatalf("snapshotLayout: %v", err)
	}
	// status.archive is set but the backend object is gone (deleted out from
	// under the operator) -- the runner must re-write it, not skip.
	env.Status.Archive = &v1alpha1.ArchiveStatus{URI: "s3://test-bucket/" + layout.ArchivePrefix()}

	store := &archiveFakeStore{snap: Snapshot{Env: env}}
	runner := NewArchiveRunner(store, be, cfg, testArchiveLogger())

	if err := runner.Archive(context.Background(), store.snap); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	if store.patched == nil {
		t.Fatal("PatchArchive was not called; want re-write")
	}
	if !be.has(layout.ArchiveRun()) {
		t.Error("run.json was not re-uploaded")
	}
}

func TestArchiveRunner_IncompleteEnv_Errors(t *testing.T) {
	be := newFakeBackend()
	store := &archiveFakeStore{snap: Snapshot{}} // zero-value Env: no name/namespace/uid
	runner := NewArchiveRunner(store, be, testArchiveCfg(), testArchiveLogger())

	if err := runner.Archive(context.Background(), store.snap); err == nil {
		t.Fatal("Archive: want error for incomplete env, got nil")
	}
	if store.patched != nil {
		t.Error("PatchArchive was called despite the incomplete-env error")
	}
}

// getFailingBackend wraps a fakeBackend and fails Get for one key with a
// non-ErrNotFound error -- the transient-outage shape (a throttled or
// unreachable endpoint), as opposed to the "object genuinely absent" shape
// the archive is allowed to treat as an honest omission.
type getFailingBackend struct {
	*fakeBackend
	failKey string
}

func (b getFailingBackend) Get(ctx context.Context, key string) (io.ReadCloser, storage.ObjectInfo, error) {
	if key == b.failKey {
		return nil, storage.ObjectInfo{}, &storage.Error{Op: "Get", Backend: "fake", Key: key, Kind: storage.ErrUnreachable}
	}
	return b.fakeBackend.Get(ctx, key)
}

// TestArchiveRunner_AgentHomeReadError_FailsArchive is the error-
// classification guard the whole "never lose the transcripts" contract rests
// on: only ErrNotFound means "this snapshot has no agent home". Any other
// backend error must fail the Job so it retries -- recording a run.json that
// says the context was omitted would make a transient S3 outage permanently
// indistinguishable from a never-frozen run, and the controller would then
// drop the finalizer on that record.
func TestArchiveRunner_AgentHomeReadError_FailsArchive(t *testing.T) {
	cfg := testArchiveCfg()
	env := testArchiveEnv()
	takenAt := metav1.NewTime(time.Date(2026, 1, 1, 12, 30, 0, 0, time.UTC))
	env.Status.Snapshot = &v1alpha1.SnapshotStatus{Seq: 0, TakenAt: &takenAt}

	layout, err := snapshotLayout(cfg, env.Namespace, env.Name, string(env.UID))
	if err != nil {
		t.Fatalf("snapshotLayout: %v", err)
	}
	inner := newFakeBackend()
	be := getFailingBackend{fakeBackend: inner, failKey: layout.AgentHome(0, takenAt.Time)}

	store := &archiveFakeStore{snap: Snapshot{Env: env}}
	runner := NewArchiveRunner(store, be, cfg, testArchiveLogger())

	err = runner.Archive(context.Background(), store.snap)
	if err == nil {
		t.Fatal("Archive: want an error for a non-NotFound agent-home read failure, got nil")
	}
	if !strings.Contains(err.Error(), "agent-home") {
		t.Errorf("Archive error = %v, want it to name the agent-home read", err)
	}
	if store.patched != nil {
		t.Error("PatchArchive was called despite the backend read failure")
	}
	if inner.has(layout.ArchiveRun()) {
		t.Error("run.json was uploaded despite the backend read failure; the record would claim the context was omitted")
	}
}

// TestArchiveRunner_ContextUploadError_FailsArchive covers the other half of
// the same contract: a failed context.tar.zst upload must not fall through to
// a run.json + status.archive that together look like a finished archive.
func TestArchiveRunner_ContextUploadError_FailsArchive(t *testing.T) {
	cfg := testArchiveCfg()
	env := testArchiveEnv()
	takenAt := metav1.NewTime(time.Date(2026, 1, 1, 12, 30, 0, 0, time.UTC))
	env.Status.Snapshot = &v1alpha1.SnapshotStatus{Seq: 0, TakenAt: &takenAt}

	layout, err := snapshotLayout(cfg, env.Namespace, env.Name, string(env.UID))
	if err != nil {
		t.Fatalf("snapshotLayout: %v", err)
	}
	be := newFakeBackend()
	if _, err := be.Put(context.Background(), layout.AgentHome(0, takenAt.Time), strings.NewReader("agent home bytes"), storage.PutOptions{}); err != nil {
		t.Fatalf("seeding agent-home object: %v", err)
	}
	be.failOn = "context.tar.zst"

	store := &archiveFakeStore{snap: Snapshot{Env: env}}
	runner := NewArchiveRunner(store, be, cfg, testArchiveLogger())

	if err := runner.Archive(context.Background(), store.snap); err == nil {
		t.Fatal("Archive: want an error for a failed context upload, got nil")
	}
	if store.patched != nil {
		t.Error("PatchArchive was called despite the failed context upload")
	}
	if be.has(layout.ArchiveRun()) {
		t.Error("run.json was uploaded despite the failed context upload")
	}
}

func TestBuildArchiveRunner_RejectsNonS3Backend(t *testing.T) {
	store := &archiveFakeStore{}
	cfg := Config{Snapshot: SnapshotConfig{Backend: "pvc"}}
	if _, err := buildArchiveRunner(store, cfg, testArchiveLogger()); err == nil {
		t.Fatal("buildArchiveRunner: want error for pvc backend, got nil")
	}
}
