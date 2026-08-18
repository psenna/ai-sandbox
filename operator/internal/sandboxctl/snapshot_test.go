package sandboxctl

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
	"github.com/psenna/ai-sandbox/operator/internal/storage"
)

// fakeBackend is an in-memory storage.Backend for snapshot_test.go. failOn,
// if non-empty, makes Put fail (with a retryable ErrUnreachable-kinded
// error) for any key containing that substring. failMessage overrides the
// injected error's message (default "injected failure") -- used to
// reproduce an oversized, AWS-SDK-shaped error string.
type fakeBackend struct {
	mu          sync.Mutex
	data        map[string][]byte
	failOn      string
	failMessage string
}

func newFakeBackend() *fakeBackend { return &fakeBackend{data: map[string][]byte{}} }

func (b *fakeBackend) Put(_ context.Context, key string, r io.Reader, _ storage.PutOptions) (storage.ObjectInfo, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.failOn != "" && strings.Contains(key, b.failOn) {
		msg := b.failMessage
		if msg == "" {
			msg = "injected failure"
		}
		return storage.ObjectInfo{}, &storage.Error{Op: "Put", Backend: "fake", Key: key, Kind: storage.ErrUnreachable, Err: fmt.Errorf("%s", msg)}
	}
	buf, err := io.ReadAll(r)
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	b.data[key] = buf
	sum := sha256.Sum256(buf)
	return storage.ObjectInfo{Key: key, Size: int64(len(buf)), SHA256: hex.EncodeToString(sum[:]), ModTime: time.Now()}, nil
}

func (b *fakeBackend) Get(_ context.Context, key string) (io.ReadCloser, storage.ObjectInfo, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	buf, ok := b.data[key]
	if !ok {
		return nil, storage.ObjectInfo{}, &storage.Error{Op: "Get", Backend: "fake", Key: key, Kind: storage.ErrNotFound}
	}
	sum := sha256.Sum256(buf)
	return io.NopCloser(bytes.NewReader(buf)), storage.ObjectInfo{Key: key, Size: int64(len(buf)), SHA256: hex.EncodeToString(sum[:])}, nil
}

func (b *fakeBackend) Stat(ctx context.Context, key string) (storage.ObjectInfo, error) {
	_, info, err := b.Get(ctx, key)
	return info, err
}

func (b *fakeBackend) List(_ context.Context, prefix string) ([]storage.ObjectInfo, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []storage.ObjectInfo
	for k, v := range b.data {
		if strings.HasPrefix(k, prefix) {
			out = append(out, storage.ObjectInfo{Key: k, Size: int64(len(v))})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func (b *fakeBackend) Delete(_ context.Context, key string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.data, key)
	return nil
}

func (b *fakeBackend) DeletePrefix(_ context.Context, prefix string) (int, error) {
	if prefix == "" {
		return 0, &storage.Error{Op: "DeletePrefix", Backend: "fake", Kind: storage.ErrInvalid}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for k := range b.data {
		if strings.HasPrefix(k, prefix) {
			delete(b.data, k)
			n++
		}
	}
	return n, nil
}

func (b *fakeBackend) has(substr string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for k := range b.data {
		if strings.Contains(k, substr) {
			return true
		}
	}
	return false
}

// fakeSnapshotStore implements Store, capturing RecordSnapshotAttempt/
// RecordSnapshot calls for assertions.
type fakeSnapshotStore struct {
	mu        sync.Mutex
	attempts  []SnapshotAttempt
	recorded  *SnapshotRecord
	recordErr error
}

func (s *fakeSnapshotStore) Snapshot() Snapshot            { return Snapshot{} }
func (s *fakeSnapshotStore) Refresh(context.Context) error { return nil }
func (s *fakeSnapshotStore) DeclareWait(context.Context, WaitProbe, time.Time) error {
	return nil
}
func (s *fakeSnapshotStore) ReportDone(context.Context, Result, time.Time) (bool, error) {
	return false, nil
}
func (s *fakeSnapshotStore) RecordSnapshotAttempt(_ context.Context, a SnapshotAttempt, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attempts = append(s.attempts, a)
	return nil
}
func (s *fakeSnapshotStore) RecordSnapshot(_ context.Context, r SnapshotRecord, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.recordErr != nil {
		return s.recordErr
	}
	rc := r
	s.recorded = &rc
	return nil
}

func noSleep(context.Context, time.Duration) error { return nil }

func testSnapshotConfig(t *testing.T, ws, ah string) SnapshotConfig {
	t.Helper()
	return SnapshotConfig{
		Engine:          "none",
		ClusterID:       "default",
		SpecHash:        "sha256:abc",
		AgentImage:      "agent:latest",
		WorkspacePath:   ws,
		AgentHomePath:   ah,
		Backend:         "s3",
		S3Bucket:        "test-bucket",
		SnapshotRetries: 2,
	}
}

func testEnvSnapshot() Snapshot {
	return Snapshot{
		Environment: EnvironmentRef{Name: "env-a", Namespace: "ns-a", UID: "uid-a"},
		Phase:       v1alpha1.PhaseFreezing,
		FreezeCount: 0,
	}
}

func TestSnapshotHook_Freeze_Success(t *testing.T) {
	ws := t.TempDir()
	ah := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "file.txt"), []byte("hello world"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	be := newFakeBackend()
	store := &fakeSnapshotStore{}
	log := logr.FromSlogHandler(slog.NewJSONHandler(io.Discard, nil))
	hook := &SnapshotHook{
		Store: store, Backend: be, Engine: noopEngineTeardown{},
		Cfg: testSnapshotConfig(t, ws, ah), Now: time.Now, Sleep: noSleep, Log: log,
	}

	if err := hook.Freeze(context.Background(), testEnvSnapshot()); err != nil {
		t.Fatalf("Freeze: %v", err)
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if store.recorded == nil {
		t.Fatal("RecordSnapshot was never called")
	}
	if store.recorded.Seq != 0 {
		t.Errorf("recorded.Seq = %d, want 0", store.recorded.Seq)
	}
	if store.recorded.SizeBytes <= 0 {
		t.Errorf("recorded.SizeBytes = %d, want > 0", store.recorded.SizeBytes)
	}
	if !be.has("manifest.json") || !be.has("latest.json") || !be.has("workspace.tar.zst") || !be.has("agent-home.tar.zst") {
		t.Errorf("expected manifest.json, latest.json, workspace.tar.zst, agent-home.tar.zst to all exist")
	}

	// The marker must have landed inside the workspace root BEFORE it was
	// archived.
	if _, err := os.Stat(filepath.Join(ws, ".sandbox", "last-freeze.json")); err != nil {
		t.Errorf("expected teardown marker in workspace root: %v", err)
	}
}

// TestSnapshotHook_Freeze_ManifestPutFailure proves the ordering guarantee:
// a failure writing manifest.json must leave NO latest.json and NO
// status.snapshot -- exactly the "never publish a partial snapshot"
// property this issue exists to enforce.
func TestSnapshotHook_Freeze_ManifestPutFailure(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "file.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	be := newFakeBackend()
	be.failOn = "manifest.json"
	store := &fakeSnapshotStore{}
	log := logr.FromSlogHandler(slog.NewJSONHandler(io.Discard, nil))
	cfg := testSnapshotConfig(t, ws, "")
	cfg.SnapshotRetries = 1
	hook := &SnapshotHook{
		Store: store, Backend: be, Engine: noopEngineTeardown{},
		Cfg: cfg, Now: time.Now, Sleep: noSleep, Log: log,
	}

	err := hook.Freeze(context.Background(), testEnvSnapshot())
	if err == nil {
		t.Fatal("Freeze: expected error, got nil")
	}

	if be.has("latest.json") {
		t.Error("latest.json must not exist after a manifest.json failure")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.recorded != nil {
		t.Error("status.snapshot must not be recorded after a manifest.json failure")
	}

	found := false
	for _, a := range store.attempts {
		if a.Phase == v1alpha1.SnapshotAttemptFailed {
			found = true
		}
	}
	if !found {
		t.Error("expected a Failed snapshot attempt to be recorded")
	}
}

// TestSnapshotHook_Freeze_OversizedFailureMessageIsTruncated is the
// regression test for a real bug found live while debugging this package's
// e2e coverage: an AWS SDK error's full message (request URL plus a
// chained "connection reset by peer"-style cause) routinely exceeds
// SnapshotAttemptStatus.Message's 512-byte CRD limit. Without truncation,
// recordFailure's status write is itself rejected by the API server as
// invalid, so a genuinely-failed freeze can never be recorded as Failed --
// status.snapshotAttempt stays stuck at whatever it last successfully
// wrote (InProgress) forever, with no further write and so no further
// watch event to ever correct it.
func TestSnapshotHook_Freeze_OversizedFailureMessageIsTruncated(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "file.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	be := newFakeBackend()
	be.failOn = "workspace.tar.zst"
	be.failMessage = strings.Repeat("x", 2000) // comfortably over the 512-byte limit
	store := &fakeSnapshotStore{}
	log := logr.FromSlogHandler(slog.NewJSONHandler(io.Discard, nil))
	cfg := testSnapshotConfig(t, ws, "")
	cfg.SnapshotRetries = 1
	hook := &SnapshotHook{
		Store: store, Backend: be, Engine: noopEngineTeardown{},
		Cfg: cfg, Now: time.Now, Sleep: noSleep, Log: log,
	}

	if err := hook.Freeze(context.Background(), testEnvSnapshot()); err == nil {
		t.Fatal("Freeze: expected error, got nil")
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	var failed *SnapshotAttempt
	for i := range store.attempts {
		if store.attempts[i].Phase == v1alpha1.SnapshotAttemptFailed {
			failed = &store.attempts[i]
		}
	}
	if failed == nil {
		t.Fatal("expected a Failed snapshot attempt to be recorded")
	}
	if len(failed.Message) > snapshotAttemptMessageMaxBytes {
		t.Errorf("recorded Message is %d bytes, want <= %d (the CRD's MaxLength)", len(failed.Message), snapshotAttemptMessageMaxBytes)
	}
}

func TestSnapshotHook_Freeze_PVCBackendFailsClosed(t *testing.T) {
	store := &fakeSnapshotStore{}
	log := logr.FromSlogHandler(slog.NewJSONHandler(io.Discard, nil))
	cfg := testSnapshotConfig(t, t.TempDir(), "")
	cfg.Backend = "pvc"
	hook := &SnapshotHook{Store: store, Backend: nil, Engine: noopEngineTeardown{}, Cfg: cfg, Now: time.Now, Sleep: noSleep, Log: log}

	err := hook.Freeze(context.Background(), testEnvSnapshot())
	if err == nil {
		t.Fatal("expected an error for backend=pvc, got nil")
	}
	if !strings.Contains(err.Error(), "gap G5") {
		t.Errorf("error = %v, want it to name gap G5", err)
	}
}

func TestSnapshotHook_Freeze_TeardownFailure(t *testing.T) {
	store := &fakeSnapshotStore{}
	log := logr.FromSlogHandler(slog.NewJSONHandler(io.Discard, nil))
	hook := &SnapshotHook{
		Store: store, Backend: newFakeBackend(), Engine: failingEngineTeardown{},
		Cfg: testSnapshotConfig(t, t.TempDir(), ""), Now: time.Now, Sleep: noSleep, Log: log,
	}
	if err := hook.Freeze(context.Background(), testEnvSnapshot()); err == nil {
		t.Fatal("expected an error when engine teardown fails")
	}
}

type failingEngineTeardown struct{}

func (failingEngineTeardown) Teardown(context.Context) (TeardownReport, error) {
	return TeardownReport{}, errors.New("teardown boom")
}
