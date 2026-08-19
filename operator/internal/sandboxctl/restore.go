package sandboxctl

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/go-logr/logr"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
	"github.com/psenna/ai-sandbox/operator/internal/storage"
)

// Machine-stable reasons for a failed or in-progress restore attempt. See
// api/v1alpha1.RestoreAttemptStatus.Reason. Mirrors SnapshotReason*'s
// naming style exactly (snapshot.go).
const (
	RestoreReasonBackendUnreachable = "BackendUnreachable"
	RestoreReasonBackendUnsupported = "BackendUnsupported"
	RestoreReasonCredentialsInvalid = "CredentialsInvalid"
	RestoreReasonManifestMissing    = "ManifestMissing"
	RestoreReasonManifestInvalid    = "ManifestInvalid"
	RestoreReasonChecksumMismatch   = "ChecksumMismatch"
	RestoreReasonExtractFailed      = "ExtractFailed"
	RestoreReasonPurgeFailed        = "PurgeFailed"
	RestoreReasonInternal           = "Internal"
)

// restoreInProgressFileName is the sentinel restore.go writes as its
// literal first action and removes on success, so a suspend arriving
// mid-restore can never let SnapshotHook.Freeze (snapshot.go) archive a
// half-extracted tree. Excluded from every archive (exclusions.go): a
// snapshot must never itself contain this sentinel.
const restoreInProgressFileName = "restore-in-progress"

// restoreInProgressPath is <workspaceRoot>/.sandbox/restore-in-progress.
func restoreInProgressPath(workspaceRoot string) string {
	return filepath.Join(workspaceRoot, markerDirName, restoreInProgressFileName)
}

// restoreInProgress reports whether the restore-in-progress sentinel is
// currently present under workspaceRoot.
func restoreInProgress(workspaceRoot string) bool {
	_, err := os.Stat(restoreInProgressPath(workspaceRoot))
	return err == nil
}

// writeRestoreInProgress creates the (empty) restore-in-progress sentinel,
// creating the .sandbox directory if needed.
func writeRestoreInProgress(workspaceRoot string) error {
	dir := filepath.Join(workspaceRoot, markerDirName)
	if err := os.MkdirAll(dir, markerDirPerm); err != nil { //nolint:gosec // G301: 0750, deliberately non-world-readable marker directory
		return fmt.Errorf("sandboxctl: creating marker directory %s: %w", dir, err)
	}
	if err := os.WriteFile(restoreInProgressPath(workspaceRoot), nil, markerFilePerm); err != nil { //nolint:gosec // G306: 0640
		return fmt.Errorf("sandboxctl: writing restore-in-progress sentinel: %w", err)
	}
	return nil
}

// removeRestoreInProgress removes the restore-in-progress sentinel. Not
// finding it is not an error (defensive only; Restore always writes it
// first).
func removeRestoreInProgress(workspaceRoot string) error {
	err := os.Remove(restoreInProgressPath(workspaceRoot))
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("sandboxctl: removing restore-in-progress sentinel: %w", err)
	}
	return nil
}

// RestoreHook is the real wake/restore implementation: fetch and verify the
// manifest, restore the workspace (warm or cold) and, if archived, the
// agent home (always cold -- it is an emptyDir that dies with the pod), and
// write the wake/warm-cache markers. Mirrors SnapshotHook's fields and
// error-handling/logging conventions exactly (snapshot.go).
type RestoreHook struct {
	Store   Store
	Backend storage.Backend
	Cfg     Config
	Now     func() time.Time
	Sleep   func(context.Context, time.Duration) error
	Log     logr.Logger
}

// NewRestoreHook builds a RestoreHook with real Now/Sleep implementations
// when the corresponding struct fields are left unset by the caller.
func NewRestoreHook(store Store, be storage.Backend, cfg Config, log logr.Logger) *RestoreHook {
	return &RestoreHook{
		Store:   store,
		Backend: be,
		Cfg:     cfg,
		Now:     time.Now,
		Sleep:   defaultSleep,
		Log:     log,
	}
}

// Restore runs a single wake: fetch+verify the manifest, restore the
// workspace root (attempting warm first if Cfg.Restore.Warm, falling back
// to cold), restore the agent home (always cold, if archived), and write
// the wake/warm-cache markers. See the numbered steps inline.
func (h *RestoreHook) Restore(ctx context.Context, s Snapshot) error {
	now := h.nowSafe()
	seq := h.Cfg.Restore.Seq
	snapshotID := h.Cfg.Restore.SnapshotID

	// 0. Fail closed for the "pvc" backend BEFORE touching h.Backend (which
	// is nil in that case -- run.go/runrestore.go never construct an S3
	// backend for it). Restore has no support for the pvc backend at all
	// (#29 scoping: Q7).
	if h.Cfg.Snapshot.Backend == "pvc" {
		msg := "storage.backend.type=pvc is not supported by the restore container: the backend PVC is not mounted into the environment pod. Use storage.backend.type=s3."
		h.recordRestoreFailure(ctx, seq, snapshotID, 0, RestoreReasonBackendUnsupported, msg)
		return fmt.Errorf("sandboxctl: %s", msg)
	}

	// 1. Pin the clock; build the Layout for this environment.
	layout, err := snapshotLayout(h.Cfg.Snapshot, s.Environment.Namespace, s.Environment.Name, s.Environment.UID)
	if err != nil {
		h.recordRestoreFailure(ctx, seq, snapshotID, 0, RestoreReasonInternal, err.Error())
		return fmt.Errorf("sandboxctl: building restore layout: %w", err)
	}

	// 2. Record attempt.
	if err := h.Store.RecordRestoreAttempt(ctx, RestoreAttempt{Seq: seq, SnapshotID: snapshotID, Phase: v1alpha1.RestoreAttemptInProgress, Attempts: 0}, now); err != nil {
		h.Log.Info("recording restore attempt failed (continuing)", "error", err.Error())
	}

	workspacePath := h.Cfg.Snapshot.WorkspacePath
	agentHomePath := h.Cfg.Snapshot.AgentHomePath

	// 3. Write the restore-in-progress sentinel as the literal first
	// action against the workspace -- see snapshot.go's top-of-Freeze
	// check. (The warm-cache marker is deliberately NOT removed here: step
	// 5 below still needs to read and validate it. It is removed only if
	// that validation fails or warm restore wasn't requested -- see the
	// cold branch below -- so a stale marker is never left describing
	// content a cold restore is about to overwrite anyway, per Q2's "the
	// stale marker is deleted" rule.)
	if err := writeRestoreInProgress(workspacePath); err != nil {
		h.recordRestoreFailure(ctx, seq, snapshotID, 0, RestoreReasonInternal, err.Error())
		return fmt.Errorf("sandboxctl: %w", err)
	}

	attempts := 0

	// 4. Fetch and verify the manifest.
	manifestKey, err := layout.SnapshotFileByID(snapshotID, storage.FileManifest)
	if err != nil {
		h.recordRestoreFailure(ctx, seq, snapshotID, attempts, RestoreReasonManifestInvalid, err.Error())
		return fmt.Errorf("sandboxctl: resolving manifest key: %w", err)
	}
	var manifest storage.Manifest
	var manifestSHA256 string
	err = retryStep(ctx, h.Log, h.Sleep, h.Cfg.Restore.Retries, h.Cfg.Restore.StepTimeout, seq, "get-manifest", func(stepCtx context.Context) error {
		attempts++
		h.recordRestoreAttemptCount(ctx, seq, snapshotID, attempts)
		b, sum, gerr := getAndHash(stepCtx, h.Backend, manifestKey)
		if gerr != nil {
			return gerr
		}
		m, perr := storage.ParseManifest(bytes.NewReader(b))
		if perr != nil {
			return &storage.Error{Op: "Get", Backend: "manifest", Key: manifestKey, Kind: storage.ErrInvalid, Err: perr}
		}
		manifest = m
		manifestSHA256 = sum
		return nil
	})
	if err != nil {
		h.recordRestoreFailure(ctx, seq, snapshotID, attempts, classifyRestoreErr(err), err.Error())
		return fmt.Errorf("sandboxctl: fetching manifest: %w", err)
	}
	if manifest.Seq != seq {
		msg := fmt.Sprintf("manifest seq %d does not match requested restore seq %d", manifest.Seq, seq)
		h.recordRestoreFailure(ctx, seq, snapshotID, attempts, RestoreReasonManifestInvalid, msg)
		return fmt.Errorf("sandboxctl: %s", msg)
	}

	teardownSeq, teardownOK := readTeardownMarkerSeq(workspacePath)

	var roots []RestoredRoot

	// 5. Workspace root: attempt warm first (if configured), else cold.
	wsEntry, ok := manifest.File(storage.FileWorkspace)
	if !ok {
		msg := "manifest does not list a workspace entry"
		h.recordRestoreFailure(ctx, seq, snapshotID, attempts, RestoreReasonManifestInvalid, msg)
		return fmt.Errorf("sandboxctl: %s", msg)
	}

	warmMissReason := ""
	warm := false
	if h.Cfg.Restore.Warm {
		m := loadWarmMarker(workspacePath)
		var ok bool
		ok, warmMissReason = ValidateWarmMarker(m, s.Environment.UID, snapshotID, seq, manifestSHA256, manifest.Files, teardownSeq, teardownOK)
		warm = ok
	} else {
		warmMissReason = "" // cold forced by config, not a validation miss
	}

	if warm {
		roots = append(roots, RestoredRoot{Name: "workspace", Source: "Warm"})
	} else {
		// Best-effort: delete any (now-proven-stale, or never applicable)
		// warm marker, so a crash between here and step 8's fresh write
		// never leaves a marker claiming warmth this attempt did not
		// verify. Step 8 overwrites it on success regardless; this only
		// matters if THIS restore itself fails partway through.
		if err := RemoveWarmMarker(workspacePath); err != nil {
			h.Log.Info("removing stale warm marker failed (continuing)", "error", err.Error())
		}
		workspaceKey, err := layout.SnapshotFileByID(snapshotID, storage.FileWorkspace)
		if err != nil {
			h.recordRestoreFailure(ctx, seq, snapshotID, attempts, RestoreReasonManifestInvalid, err.Error())
			return fmt.Errorf("sandboxctl: resolving workspace key: %w", err)
		}
		// Keep ".sandbox" across the purge, deviating from a literal
		// purgeRoot(workspacePath, "lost+found"): the restore-in-progress
		// sentinel written in step 3 lives at
		// workspacePath/.sandbox/restore-in-progress, and purging it here
		// would defeat its entire purpose (a crash mid-download would leave
		// no sentinel for Freeze's top-of-function check to find). The
		// archive being restored below still overwrites .sandbox's own
		// content files (last-freeze.json, RESUME.md) since those ARE
		// included in every snapshot -- only stray operator-owned marker
		// state that predates this restore and isn't part of the new
		// snapshot can survive, which is immaterial to the agent's own
		// workspace content.
		if _, err := purgeRoot(workspacePath, "lost+found", markerDirName); err != nil {
			h.recordRestoreFailure(ctx, seq, snapshotID, attempts, RestoreReasonPurgeFailed, err.Error())
			return fmt.Errorf("sandboxctl: purging workspace: %w", err)
		}
		err = retryStep(ctx, h.Log, h.Sleep, h.Cfg.Restore.Retries, h.Cfg.Restore.StepTimeout, seq, "restore-workspace", func(stepCtx context.Context) error {
			attempts++
			h.recordRestoreAttemptCount(ctx, seq, snapshotID, attempts)
			return storage.RestoreFrom(stepCtx, h.Backend, workspaceKey, workspacePath, wsEntry, h.extractOpts())
		})
		if err != nil {
			h.recordRestoreFailure(ctx, seq, snapshotID, attempts, classifyRestoreErr(err), err.Error())
			return fmt.Errorf("sandboxctl: restoring workspace: %w", err)
		}
		roots = append(roots, RestoredRoot{Name: "workspace", Source: "Cold", WarmMissReason: warmMissReason, BytesDownloaded: wsEntry.Size})
	}

	// 6. Agent home: ALWAYS cold -- no warm path exists for an emptyDir.
	// Only attempted when configured AND the manifest actually archived
	// one (an older/degenerate snapshot, or the recovery-Job path, may not
	// have one).
	if agentHomePath != "" {
		if ahEntry, ok := manifest.File(storage.FileAgentHome); ok {
			agentHomeKey, err := layout.SnapshotFileByID(snapshotID, storage.FileAgentHome)
			if err != nil {
				h.recordRestoreFailure(ctx, seq, snapshotID, attempts, RestoreReasonManifestInvalid, err.Error())
				return fmt.Errorf("sandboxctl: resolving agent-home key: %w", err)
			}
			if _, err := purgeRoot(agentHomePath); err != nil {
				h.recordRestoreFailure(ctx, seq, snapshotID, attempts, RestoreReasonPurgeFailed, err.Error())
				return fmt.Errorf("sandboxctl: purging agent home: %w", err)
			}
			err = retryStep(ctx, h.Log, h.Sleep, h.Cfg.Restore.Retries, h.Cfg.Restore.StepTimeout, seq, "restore-agent-home", func(stepCtx context.Context) error {
				attempts++
				h.recordRestoreAttemptCount(ctx, seq, snapshotID, attempts)
				return storage.RestoreFrom(stepCtx, h.Backend, agentHomeKey, agentHomePath, ahEntry, h.extractOpts())
			})
			if err != nil {
				h.recordRestoreFailure(ctx, seq, snapshotID, attempts, classifyRestoreErr(err), err.Error())
				return fmt.Errorf("sandboxctl: restoring agent home: %w", err)
			}
			roots = append(roots, RestoredRoot{Name: "agent-home", Source: "Cold", BytesDownloaded: ahEntry.Size})
		}
	}

	// 7. Write the wake marker: informational only for the agent. A
	// failure here is logged but does not fail the restore.
	specChanged := manifest.AgentImage != h.Cfg.Snapshot.AgentImage || manifest.SpecHash != h.Cfg.Snapshot.SpecHash
	wakeSource := "Cold"
	if warm {
		wakeSource = "Warm"
	}
	if err := WriteWakeMarker(workspacePath, WakeMarker{
		SchemaVersion:      WakeMarkerSchemaVersion,
		Seq:                seq,
		SnapshotID:         snapshotID,
		Source:             wakeSource,
		WarmMissReason:     warmMissReason,
		RestoredAt:         now,
		BytesDownloaded:    sumBytesDownloaded(roots),
		SnapshotAgentImage: manifest.AgentImage,
		CurrentAgentImage:  h.Cfg.Snapshot.AgentImage,
		SnapshotSpecHash:   manifest.SpecHash,
		CurrentSpecHash:    h.Cfg.Snapshot.SpecHash,
		SpecChanged:        specChanged,
	}); err != nil {
		h.Log.Info("writing wake marker failed (continuing; informational only)", "error", err.Error())
	}

	// 8. Write the warm-cache marker: THIS one failing is a hard failure
	// for the NEXT wake's optimization (though not for THIS restore, which
	// already succeeded at steps 5/6) -- log and continue rather than
	// undo an otherwise-successful restore.
	if err := WriteWarmMarker(workspacePath, WarmMarker{
		SchemaVersion:  WarmMarkerSchemaVersion,
		EnvUID:         s.Environment.UID,
		Seq:            seq,
		SnapshotID:     snapshotID,
		Root:           "workspace",
		ManifestSHA256: manifestSHA256,
		Files:          manifest.Files,
		WrittenAt:      now,
		WrittenBy:      "restore",
	}); err != nil {
		h.Log.Info("writing warm-cache marker failed (next wake will cold-restore; this restore still succeeded)", "error", err.Error())
	}

	// 9. Remove the restore-in-progress sentinel: only now is the tree
	// known-good.
	if err := removeRestoreInProgress(workspacePath); err != nil {
		h.Log.Info("removing restore-in-progress sentinel failed", "error", err.Error())
	}

	// 10. Record success.
	if err := h.Store.RecordRestoreAttempt(ctx, RestoreAttempt{
		Seq:        seq,
		SnapshotID: snapshotID,
		Phase:      v1alpha1.RestoreAttemptSucceeded,
		Roots:      roots,
		Attempts:   attempts,
		Duration:   h.nowSafe().Sub(now),
	}, h.nowSafe()); err != nil {
		h.Log.Info("recording successful restore failed", "error", err.Error())
		return fmt.Errorf("sandboxctl: recording restore: %w", err)
	}
	return nil
}

func (h *RestoreHook) extractOpts() storage.ExtractOptions {
	return storage.ExtractOptions{MaxEntries: h.Cfg.Restore.MaxEntries, MaxTotalBytes: h.Cfg.Restore.MaxTotalBytes}
}

func (h *RestoreHook) nowSafe() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

func (h *RestoreHook) recordRestoreAttemptCount(ctx context.Context, seq int, snapshotID string, attempts int) {
	if err := h.Store.RecordRestoreAttempt(ctx, RestoreAttempt{Seq: seq, SnapshotID: snapshotID, Phase: v1alpha1.RestoreAttemptInProgress, Attempts: attempts}, h.nowSafe()); err != nil {
		h.Log.Info("recording restore attempt count failed (continuing)", "error", err.Error())
	}
}

// restoreAttemptMessageMaxBytes matches RestoreAttemptStatus.Message's
// +kubebuilder:validation:MaxLength=512, exactly like
// snapshotAttemptMessageMaxBytes (snapshot.go) -- see that constant's doc
// comment for the CI-lesson this guards against: an untruncated message
// makes the API server silently reject the whole status PATCH, permanently
// wedging status.restoreAttempt at its last successfully-written value.
const restoreAttemptMessageMaxBytes = 512

func (h *RestoreHook) recordRestoreFailure(ctx context.Context, seq int, snapshotID string, attempts int, reason, message string) {
	if len(message) > restoreAttemptMessageMaxBytes {
		message = message[:restoreAttemptMessageMaxBytes]
	}
	if err := h.Store.RecordRestoreAttempt(ctx, RestoreAttempt{
		Seq: seq, SnapshotID: snapshotID, Phase: v1alpha1.RestoreAttemptFailed, Attempts: attempts, Reason: reason, Message: message,
	}, h.nowSafe()); err != nil {
		h.Log.Info("recording restore failure failed", "error", err.Error())
	}
}

// classifyRestoreErr maps a storage error into one of the RestoreReason*
// machine reasons.
func classifyRestoreErr(err error) string {
	switch {
	case storage.IsPermission(err):
		return RestoreReasonCredentialsInvalid
	case storage.IsUnreachable(err):
		return RestoreReasonBackendUnreachable
	case storage.IsCorrupt(err):
		return RestoreReasonChecksumMismatch
	case storage.IsNotFound(err):
		return RestoreReasonManifestMissing
	case storage.IsInvalid(err):
		return RestoreReasonManifestInvalid
	default:
		return RestoreReasonExtractFailed
	}
}

// getAndHash fetches key from b in full and returns its bytes alongside the
// lowercase-hex SHA-256 of exactly those bytes -- used for the manifest,
// which is always small enough to buffer whole (unlike the archived roots,
// which stream through storage.RestoreFrom's own verifying reader).
func getAndHash(ctx context.Context, b storage.Backend, key string) ([]byte, string, error) {
	rc, _, err := b.Get(ctx, key)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(rc)
	if err != nil {
		return nil, "", &storage.Error{Op: "Get", Backend: "manifest", Key: key, Kind: storage.ErrUnreachable, Err: err}
	}
	sum := sha256.Sum256(data)
	return data, hex.EncodeToString(sum[:]), nil
}

// loadWarmMarker reads and parses the workspace's warm-cache.json,
// returning a sentinel WarmMarker ValidateWarmMarker recognizes for both
// "no marker file" (the zero value) and "marker file present but
// unreadable" (unreadableWarmMarkerSchemaVersion) -- see warmmarker.go's
// doc comments on ValidateWarmMarker.
func loadWarmMarker(workspaceRoot string) WarmMarker {
	m, err := ReadWarmMarker(workspaceRoot)
	switch {
	case err == nil:
		return m
	case os.IsNotExist(err):
		return WarmMarker{}
	default:
		return WarmMarker{SchemaVersion: unreadableWarmMarkerSchemaVersion}
	}
}

// readTeardownMarkerSeq reads workspaceRoot/.sandbox/last-freeze.json (the
// TeardownMarker marker.go's Freeze step writes BEFORE archiving) and
// returns its recorded Seq as a second, independent witness for
// ValidateWarmMarker: a warm marker claiming a given seq is only trusted if
// the teardown marker -- written by a wholly separate step of the SAME
// freeze -- agrees. ok is false if the file is absent or unreadable.
func readTeardownMarkerSeq(workspaceRoot string) (seq int, ok bool) {
	b, err := os.ReadFile(filepath.Join(workspaceRoot, markerDirName, markerJSONName)) //nolint:gosec // G304: workspaceRoot is caller-controlled and trusted
	if err != nil {
		return 0, false
	}
	var m TeardownMarker
	if err := json.Unmarshal(b, &m); err != nil {
		return 0, false
	}
	return m.Seq, true
}

func sumBytesDownloaded(roots []RestoredRoot) int64 {
	var total int64
	for _, r := range roots {
		total += r.BytesDownloaded
	}
	return total
}
