package sandboxctl

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-logr/logr"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
	"github.com/psenna/ai-sandbox/operator/internal/storage"
)

// fixedClock returns a deterministic, whole-second UTC time so a test can
// compute the exact SnapshotID a freeze will produce.
func fixedClock() time.Time {
	return time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
}

func testEnvSnapshotFor(uid string) Snapshot {
	return Snapshot{
		Environment: EnvironmentRef{Name: "env-a", Namespace: "ns-a", UID: uid},
		Phase:       v1alpha1.PhaseFreezing,
		FreezeCount: 0,
	}
}

// freezeFixture runs a REAL SnapshotHook.Freeze (with a fixed clock) against
// ws/ah, populating be with a valid manifest/latest.json/warm marker and
// leaving the teardown+warm markers inside ws -- exactly what a real freeze
// leaves on a retained PVC. Returns the snapshot ID a matching restore must
// request.
func freezeFixture(t *testing.T, be *fakeBackend, ws, ah, envUID string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(ws, "file.txt"), []byte("hello world"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	log := logr.FromSlogHandler(slog.NewJSONHandler(io.Discard, nil))
	hook := &SnapshotHook{
		Store: &fakeSnapshotStore{}, Backend: be, Engine: noopEngineTeardown{},
		Cfg: testSnapshotConfig(t, ws, ah), Now: fixedClock, Sleep: noSleep, Log: log,
	}
	if err := hook.Freeze(context.Background(), testEnvSnapshotFor(envUID)); err != nil {
		t.Fatalf("freezeFixture: Freeze: %v", err)
	}
	return storage.SnapshotID(0, fixedClock())
}

func testRestoreConfig(snapshotID string, warm bool) RestoreConfig {
	return RestoreConfig{SnapshotID: snapshotID, Seq: 0, Warm: warm, Retries: 2, StepTimeout: 0}
}

func TestRestoreHook_ColdRestore_Success(t *testing.T) {
	srcWS, srcAH := t.TempDir(), t.TempDir()
	be := newFakeBackend()
	snapshotID := freezeFixture(t, be, srcWS, srcAH, "uid-a")

	// A fresh workspace/agent-home, as if the PVC had just been (re)created
	// -- with an unrelated stale file that a REAL cold restore must purge,
	// proving the restored tree REPLACES rather than merges.
	dstWS, dstAH := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(dstWS, "stale.txt"), []byte("must be purged"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	store := &fakeSnapshotStore{}
	log := logr.FromSlogHandler(slog.NewJSONHandler(io.Discard, nil))
	cfg := Config{Snapshot: testSnapshotConfig(t, dstWS, dstAH), Restore: testRestoreConfig(snapshotID, false)}
	hook := &RestoreHook{Store: store, Backend: be, Cfg: cfg, Now: fixedClock, Sleep: noSleep, Log: log}

	if err := hook.Restore(context.Background(), testEnvSnapshotFor("uid-a")); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dstWS, "stale.txt")); !os.IsNotExist(err) {
		t.Errorf("stale.txt was not purged by cold restore: err=%v", err)
	}
	got, err := os.ReadFile(filepath.Join(dstWS, "file.txt")) //nolint:gosec // G304: test-controlled path
	if err != nil || string(got) != "hello world" {
		t.Errorf("restored file.txt = %q, err=%v, want %q", got, err, "hello world")
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.restoreAttempts) == 0 {
		t.Fatal("no RestoreAttempt was recorded")
	}
	final := store.restoreAttempts[len(store.restoreAttempts)-1]
	if final.Phase != v1alpha1.RestoreAttemptSucceeded {
		t.Fatalf("final phase = %v, want Succeeded", final.Phase)
	}
	if len(final.Roots) != 2 {
		t.Fatalf("Roots = %+v, want 2 entries (workspace, agent-home)", final.Roots)
	}
	for _, r := range final.Roots {
		if r.Source != "Cold" {
			t.Errorf("root %s Source = %q, want Cold", r.Name, r.Source)
		}
		if r.BytesDownloaded <= 0 {
			t.Errorf("root %s BytesDownloaded = %d, want > 0", r.Name, r.BytesDownloaded)
		}
	}

	if _, err := os.Stat(filepath.Join(dstWS, ".sandbox", "restore-in-progress")); !os.IsNotExist(err) {
		t.Errorf("restore-in-progress sentinel was not removed: err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(dstWS, ".sandbox", "last-wake.json")); err != nil {
		t.Errorf("wake marker was not written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dstWS, ".sandbox", "warm-cache.json")); err != nil {
		t.Errorf("warm-cache marker was not written: %v", err)
	}
}

// TestRestoreHook_WarmRestore_SkipsDownload restores into the SAME
// workspace directory a freeze just ran against (the "retained PVC" case):
// its warm-cache.json and last-freeze.json markers validate, so the
// workspace root must be reported Warm with zero bytes downloaded -- the
// measurable proof the download was skipped, not merely fast.
func TestRestoreHook_WarmRestore_SkipsDownload(t *testing.T) {
	ws, ah := t.TempDir(), t.TempDir()
	be := newFakeBackend()
	snapshotID := freezeFixture(t, be, ws, ah, "uid-a")

	// Prove the workspace archive is never even fetched: delete it from the
	// backend entirely. A cold-path bug would surface as a hard failure
	// here (object not found), not a silent pass.
	be.mu.Lock()
	for k := range be.data {
		if filepath.Base(k) == storage.FileWorkspace {
			delete(be.data, k)
		}
	}
	be.mu.Unlock()

	store := &fakeSnapshotStore{}
	log := logr.FromSlogHandler(slog.NewJSONHandler(io.Discard, nil))
	// AgentHomePath points elsewhere: the agent home is always cold and
	// must not interfere with the workspace-warm assertion.
	cfg := Config{Snapshot: testSnapshotConfig(t, ws, ""), Restore: testRestoreConfig(snapshotID, true)}
	hook := &RestoreHook{Store: store, Backend: be, Cfg: cfg, Now: fixedClock, Sleep: noSleep, Log: log}

	if err := hook.Restore(context.Background(), testEnvSnapshotFor("uid-a")); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	final := store.restoreAttempts[len(store.restoreAttempts)-1]
	if final.Phase != v1alpha1.RestoreAttemptSucceeded {
		t.Fatalf("final phase = %v, want Succeeded", final.Phase)
	}
	if len(final.Roots) != 1 || final.Roots[0].Name != "workspace" {
		t.Fatalf("Roots = %+v, want exactly [workspace]", final.Roots)
	}
	if final.Roots[0].Source != "Warm" {
		t.Errorf("workspace Source = %q, want Warm", final.Roots[0].Source)
	}
	if final.Roots[0].BytesDownloaded != 0 {
		t.Errorf("workspace BytesDownloaded = %d, want 0 (warm skips the download)", final.Roots[0].BytesDownloaded)
	}
}

// TestRestoreHook_WarmRequested_ButMarkerMissing_FallsBackToCold proves the
// fail-safe: asking for a warm restore against a workspace with no warm
// marker (e.g. after TTL GC recreated the PVC) falls back to a full cold
// restore rather than erroring or silently skipping data.
func TestRestoreHook_WarmRequested_ButMarkerMissing_FallsBackToCold(t *testing.T) {
	srcWS, srcAH := t.TempDir(), t.TempDir()
	be := newFakeBackend()
	snapshotID := freezeFixture(t, be, srcWS, srcAH, "uid-a")

	dstWS := t.TempDir() // fresh: no warm-cache.json, no last-freeze.json
	store := &fakeSnapshotStore{}
	log := logr.FromSlogHandler(slog.NewJSONHandler(io.Discard, nil))
	cfg := Config{Snapshot: testSnapshotConfig(t, dstWS, ""), Restore: testRestoreConfig(snapshotID, true)}
	hook := &RestoreHook{Store: store, Backend: be, Cfg: cfg, Now: fixedClock, Sleep: noSleep, Log: log}

	if err := hook.Restore(context.Background(), testEnvSnapshotFor("uid-a")); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if _, err := os.ReadFile(filepath.Join(dstWS, "file.txt")); err != nil { //nolint:gosec // G304: test-controlled path
		t.Errorf("cold fallback did not restore file.txt: %v", err)
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	final := store.restoreAttempts[len(store.restoreAttempts)-1]
	if final.Roots[0].Source != "Cold" {
		t.Errorf("Source = %q, want Cold (fallback)", final.Roots[0].Source)
	}
	if final.Roots[0].WarmMissReason != WarmMissNoMarker {
		t.Errorf("WarmMissReason = %q, want %q", final.Roots[0].WarmMissReason, WarmMissNoMarker)
	}
}

// TestRestoreHook_ChecksumMismatch_FailsClosed proves a corrupted archive
// (bytes on the wire don't match what the manifest declared) fails the
// restore outright -- never silently accepted -- and the failure is
// classified ChecksumMismatch.
func TestRestoreHook_ChecksumMismatch_FailsClosed(t *testing.T) {
	ws, ah := t.TempDir(), t.TempDir()
	be := newFakeBackend()
	snapshotID := freezeFixture(t, be, ws, ah, "uid-a")

	// Corrupt the uploaded workspace archive's bytes in place, so its
	// actual content no longer matches the SHA-256 the manifest recorded.
	be.mu.Lock()
	for k, v := range be.data {
		if filepath.Base(k) == storage.FileWorkspace && len(v) > 0 {
			v[len(v)-1] ^= 0xFF
		}
	}
	be.mu.Unlock()

	dstWS := t.TempDir()
	store := &fakeSnapshotStore{}
	log := logr.FromSlogHandler(slog.NewJSONHandler(io.Discard, nil))
	cfg := Config{Snapshot: testSnapshotConfig(t, dstWS, ""), Restore: testRestoreConfig(snapshotID, false)}
	hook := &RestoreHook{Store: store, Backend: be, Cfg: cfg, Now: fixedClock, Sleep: noSleep, Log: log}

	err := hook.Restore(context.Background(), testEnvSnapshotFor("uid-a"))
	if err == nil {
		t.Fatal("Restore: want error on checksum mismatch, got nil")
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	final := store.restoreAttempts[len(store.restoreAttempts)-1]
	if final.Phase != v1alpha1.RestoreAttemptFailed {
		t.Fatalf("final phase = %v, want Failed", final.Phase)
	}
	// The reason is either ChecksumMismatch or ExtractFailed depending on
	// exactly where zstd/tar decoding first notices the corruption; both
	// are acceptable failure classifications for a corrupted stream, but
	// the restore must never succeed.
	if final.Reason != RestoreReasonChecksumMismatch && final.Reason != RestoreReasonExtractFailed {
		t.Errorf("Reason = %q, want ChecksumMismatch or ExtractFailed", final.Reason)
	}

	// The sentinel must remain: this workspace is known-bad, and a freeze
	// must refuse to run against it (snapshot.go's top-of-Freeze check).
	if _, err := os.Stat(filepath.Join(dstWS, ".sandbox", "restore-in-progress")); err != nil {
		t.Errorf("restore-in-progress sentinel was removed despite failure: %v", err)
	}
}

func TestRestoreHook_ManifestSeqMismatch(t *testing.T) {
	ws, ah := t.TempDir(), t.TempDir()
	be := newFakeBackend()
	snapshotID := freezeFixture(t, be, ws, ah, "uid-a")

	dstWS := t.TempDir()
	store := &fakeSnapshotStore{}
	log := logr.FromSlogHandler(slog.NewJSONHandler(io.Discard, nil))
	cfg := Config{Snapshot: testSnapshotConfig(t, dstWS, ""), Restore: testRestoreConfig(snapshotID, false)}
	cfg.Restore.Seq = 99 // does not match the manifest's actual seq (0)
	hook := &RestoreHook{Store: store, Backend: be, Cfg: cfg, Now: fixedClock, Sleep: noSleep, Log: log}

	err := hook.Restore(context.Background(), testEnvSnapshotFor("uid-a"))
	if err == nil {
		t.Fatal("Restore: want error on manifest seq mismatch, got nil")
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	final := store.restoreAttempts[len(store.restoreAttempts)-1]
	if final.Reason != RestoreReasonManifestInvalid {
		t.Errorf("Reason = %q, want %q", final.Reason, RestoreReasonManifestInvalid)
	}
}

func TestRestoreHook_ManifestMissing(t *testing.T) {
	be := newFakeBackend() // empty: no manifest was ever uploaded
	dstWS := t.TempDir()
	store := &fakeSnapshotStore{}
	log := logr.FromSlogHandler(slog.NewJSONHandler(io.Discard, nil))
	cfg := Config{Snapshot: testSnapshotConfig(t, dstWS, ""), Restore: testRestoreConfig("00000-2026-01-01T12:00:00Z", false)}
	hook := &RestoreHook{Store: store, Backend: be, Cfg: cfg, Now: fixedClock, Sleep: noSleep, Log: log}

	err := hook.Restore(context.Background(), testEnvSnapshotFor("uid-a"))
	if err == nil {
		t.Fatal("Restore: want error, got nil")
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	final := store.restoreAttempts[len(store.restoreAttempts)-1]
	if final.Reason != RestoreReasonManifestMissing {
		t.Errorf("Reason = %q, want %q", final.Reason, RestoreReasonManifestMissing)
	}
}

// TestRestoreHook_PVCBackendFailsClosed proves storage.backend.type=pvc
// fails BEFORE touching h.Backend (nil in that case) -- mirroring
// SnapshotHook.Freeze's own pvc guard.
func TestRestoreHook_PVCBackendFailsClosed(t *testing.T) {
	dstWS := t.TempDir()
	store := &fakeSnapshotStore{}
	log := logr.FromSlogHandler(slog.NewJSONHandler(io.Discard, nil))
	cfg := Config{
		Snapshot: SnapshotConfig{Backend: "pvc", WorkspacePath: dstWS},
		Restore:  testRestoreConfig("00000-2026-01-01T12:00:00Z", false),
	}
	hook := &RestoreHook{Store: store, Backend: nil, Cfg: cfg, Now: fixedClock, Sleep: noSleep, Log: log}

	err := hook.Restore(context.Background(), testEnvSnapshotFor("uid-a"))
	if err == nil {
		t.Fatal("Restore: want error for pvc backend, got nil")
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	final := store.restoreAttempts[len(store.restoreAttempts)-1]
	if final.Reason != RestoreReasonBackendUnsupported {
		t.Errorf("Reason = %q, want %q", final.Reason, RestoreReasonBackendUnsupported)
	}
}
