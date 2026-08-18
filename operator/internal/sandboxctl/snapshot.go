package sandboxctl

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
	"time"

	"github.com/go-logr/logr"
	"github.com/klauspost/compress/zstd"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
	"github.com/psenna/ai-sandbox/operator/internal/storage"
)

// Machine-stable reasons for a failed or in-progress snapshot attempt. See
// api/v1alpha1.SnapshotAttemptStatus.Reason.
const (
	SnapshotReasonBackendUnreachable = "BackendUnreachable"
	SnapshotReasonBackendUnsupported = "BackendUnsupported"
	SnapshotReasonCredentialsInvalid = "CredentialsInvalid"
	SnapshotReasonArchiveFailed      = "ArchiveFailed"
	SnapshotReasonTeardownFailed     = "TeardownFailed"
	SnapshotReasonInternal           = "Internal"
)

// SnapshotHook is the real FreezeHook: engine teardown, quiesce, marker
// write, archive, manifest, latest pointer, and the status.snapshot(+
// snapshotAttempt) write. See doc comments throughout this file for the
// ordering guarantees that make a crash mid-freeze leave, at worst,
// orphaned objects a manifest-driven restore would never pick up.
type SnapshotHook struct {
	Store   Store
	Backend storage.Backend
	Engine  EngineTeardown
	Cfg     SnapshotConfig
	Now     func() time.Time
	Sleep   func(context.Context, time.Duration) error
	Log     logr.Logger
}

// NewSnapshotHook builds a SnapshotHook with real Now/Sleep implementations
// when the corresponding struct fields are left unset by the caller.
func NewSnapshotHook(store Store, be storage.Backend, eng EngineTeardown, cfg SnapshotConfig, log logr.Logger) *SnapshotHook {
	return &SnapshotHook{
		Store:   store,
		Backend: be,
		Engine:  eng,
		Cfg:     cfg,
		Now:     time.Now,
		Sleep: func(ctx context.Context, d time.Duration) error {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-t.C:
				return nil
			}
		},
		Log: log,
	}
}

// Freeze implements FreezeHook. See the numbered steps inline; this is the
// #28 freeze sequence in full.
func (h *SnapshotHook) Freeze(ctx context.Context, s Snapshot) error {
	now := h.Now
	if now == nil {
		now = time.Now
	}

	// Fail closed for the "pvc" backend BEFORE touching h.Backend (which is
	// nil in that case -- run.go never constructs an S3 backend for it).
	if h.Cfg.Backend == "pvc" {
		msg := "storage.backend.type=pvc is not supported by the freeze sidecar: the backend PVC is not mounted into the environment pod (see internal/storage/doc.go gap G5). Use storage.backend.type=s3."
		h.recordFailure(ctx, int(s.FreezeCount), 0, SnapshotReasonBackendUnsupported, msg)
		return fmt.Errorf("sandboxctl: %s", msg)
	}

	// 1. Pin the clock once: every key name for this attempt derives from
	// `at`, so a retry inside this one Freeze call reuses the same
	// snapshot directory instead of scattering partial uploads across
	// several timestamps.
	at := now().UTC().Truncate(time.Second)
	start := now()

	layout, err := snapshotLayout(h.Cfg, s.Environment.Namespace, s.Environment.Name, s.Environment.UID)
	if err != nil {
		h.recordFailure(ctx, int(s.FreezeCount), 0, SnapshotReasonInternal, err.Error())
		return fmt.Errorf("sandboxctl: building snapshot layout: %w", err)
	}

	// 2. Sequence number: exact and monotonic, since lifecycle.Apply
	// increments FreezeCount only on the Freezing->Waiting transition, so
	// during freeze N (0-based) FreezeCount == N. Cross-check defensively
	// against latest.json, covering a snapshot that landed but whose status
	// write failed.
	seq := int(s.FreezeCount)
	if latest, ok := h.readLatest(ctx, layout); ok && latest.Seq >= seq {
		h.Log.Info("latest.json seq is >= FreezeCount; using latest.Seq+1", "latestSeq", latest.Seq, "freezeCount", seq)
		seq = latest.Seq + 1
	}

	// 3. Record attempt.
	if err := h.Store.RecordSnapshotAttempt(ctx, SnapshotAttempt{Seq: seq, Phase: v1alpha1.SnapshotAttemptInProgress, Attempts: 0}, at); err != nil {
		h.Log.Info("recording snapshot attempt failed (continuing)", "error", err.Error())
	}

	// 4. Engine teardown: never archive a workspace whose workload
	// containers may still be writing to it.
	report, err := h.Engine.Teardown(ctx)
	if err != nil {
		h.recordFailure(ctx, seq, 0, SnapshotReasonTeardownFailed, err.Error())
		return fmt.Errorf("sandboxctl: engine teardown: %w", err)
	}

	// 5. Agent quiesce: a bounded settle window. The sidecar has no handle
	// on the agent process and no pods RBAC.
	if h.Cfg.QuiesceDelay > 0 {
		if err := h.sleep(ctx, h.Cfg.QuiesceDelay); err != nil {
			h.recordFailure(ctx, seq, 0, SnapshotReasonInternal, err.Error())
			return fmt.Errorf("sandboxctl: quiesce wait: %w", err)
		}
	}

	// 6. Write markers BEFORE archiving, so they land inside both tarballs.
	trigger := "suspend-or-timeout"
	reason := ""
	if s.WaitFor != nil {
		trigger = "agent-wait"
		reason = s.WaitFor.Reason
	} else if s.Result != nil {
		trigger = "agent-wait"
	}
	marker := TeardownMarker{
		SchemaVersion: 1,
		Seq:           seq,
		FrozenAt:      at,
		Trigger:       trigger,
		Reason:        reason,
		Engine:        report.Engine,
		Destroyed: Destroyed{
			Containers:      report.Containers,
			ImageLayerCache: true,
			Notes:           nonEmptyNotes(report.Note),
		},
		SnapshotURI: snapshotURI(h.Cfg.S3Bucket, layout, seq, at),
	}
	agentHomeArchived := h.Cfg.AgentHomePath != ""
	if err := WriteMarkers(h.Cfg.WorkspacePath, h.Cfg.AgentHomePath, marker); err != nil {
		h.recordFailure(ctx, seq, 0, SnapshotReasonInternal, err.Error())
		return fmt.Errorf("sandboxctl: writing teardown markers: %w", err)
	}

	// 7. Archive, one storage.ArchiveTo call per root, each retried.
	var wsInfo, ahInfo storage.ObjectInfo
	attempts := 0
	err = h.retrySnapshotStep(ctx, seq, "archive-workspace", func(stepCtx context.Context) error {
		attempts++
		h.recordAttemptCount(ctx, seq, attempts)
		var aerr error
		wsInfo, aerr = storage.ArchiveTo(stepCtx, h.Backend, layout.Workspace(seq, at), h.Cfg.WorkspacePath, h.archiveOpts())
		return aerr
	})
	if err != nil {
		h.recordFailure(ctx, seq, attempts, classifySnapshotErr(err), err.Error())
		return fmt.Errorf("sandboxctl: archiving workspace: %w", err)
	}

	if agentHomeArchived {
		err = h.retrySnapshotStep(ctx, seq, "archive-agent-home", func(stepCtx context.Context) error {
			attempts++
			h.recordAttemptCount(ctx, seq, attempts)
			var aerr error
			ahInfo, aerr = storage.ArchiveTo(stepCtx, h.Backend, layout.AgentHome(seq, at), h.Cfg.AgentHomePath, h.archiveOpts())
			return aerr
		})
		if err != nil {
			h.recordFailure(ctx, seq, attempts, classifySnapshotErr(err), err.Error())
			return fmt.Errorf("sandboxctl: archiving agent home: %w", err)
		}
	}

	// 8. Build manifest AFTER both archives land successfully.
	mb := storage.NewManifestBuilder(seq, at,
		storage.Source{
			ClusterID:  h.Cfg.ClusterID,
			Namespace:  s.Environment.Namespace,
			Name:       s.Environment.Name,
			UID:        s.Environment.UID,
			Generation: s.Generation,
		},
		h.Cfg.SpecHash, h.Cfg.AgentImage)
	mb.AddFile(wsInfo, storage.FileWorkspace)
	if agentHomeArchived {
		mb.AddFile(ahInfo, storage.FileAgentHome)
	}
	m, err := mb.Build()
	if err != nil {
		h.recordFailure(ctx, seq, attempts, SnapshotReasonInternal, err.Error())
		return fmt.Errorf("sandboxctl: building manifest: %w", err)
	}
	b, err := m.Marshal()
	if err != nil {
		h.recordFailure(ctx, seq, attempts, SnapshotReasonInternal, err.Error())
		return fmt.Errorf("sandboxctl: marshaling manifest: %w", err)
	}
	var mInfo storage.ObjectInfo
	err = h.retrySnapshotStep(ctx, seq, "put-manifest", func(stepCtx context.Context) error {
		attempts++
		h.recordAttemptCount(ctx, seq, attempts)
		var perr error
		mInfo, perr = h.Backend.Put(stepCtx, layout.Manifest(seq, at), bytes.NewReader(b), storage.PutOptions{ContentType: "application/json"})
		return perr
	})
	if err != nil {
		h.recordFailure(ctx, seq, attempts, classifySnapshotErr(err), err.Error())
		return fmt.Errorf("sandboxctl: uploading manifest: %w", err)
	}

	// 9. Write latest.json LAST -- so the pointer never names an incomplete
	// snapshot.
	l := storage.Latest{SchemaVersion: storage.ManifestSchemaVersion, Seq: seq, SnapshotID: storage.SnapshotID(seq, at), UpdatedAt: at}
	lb, err := l.Marshal()
	if err != nil {
		h.recordFailure(ctx, seq, attempts, SnapshotReasonInternal, err.Error())
		return fmt.Errorf("sandboxctl: marshaling latest pointer: %w", err)
	}
	err = h.retrySnapshotStep(ctx, seq, "put-latest", func(stepCtx context.Context) error {
		attempts++
		h.recordAttemptCount(ctx, seq, attempts)
		_, perr := h.Backend.Put(stepCtx, layout.Latest(), bytes.NewReader(lb), storage.PutOptions{ContentType: "application/json"})
		return perr
	})
	if err != nil {
		h.recordFailure(ctx, seq, attempts, classifySnapshotErr(err), err.Error())
		return fmt.Errorf("sandboxctl: uploading latest pointer: %w", err)
	}

	// 10. Record success -- flips status.snapshotAttempt to Succeeded in the
	// same patch as status.snapshot.
	totalSize := wsInfo.Size + ahInfo.Size
	if err := h.Store.RecordSnapshot(ctx, SnapshotRecord{
		Seq:            seq,
		URI:            snapshotURI(h.Cfg.S3Bucket, layout, seq, at),
		SizeBytes:      totalSize,
		SHA256:         mInfo.SHA256,
		TakenAt:        at,
		DurationMillis: now().Sub(start).Milliseconds(),
	}, now()); err != nil {
		h.Log.Info("recording successful snapshot failed", "error", err.Error())
		return fmt.Errorf("sandboxctl: recording snapshot: %w", err)
	}
	return nil
}

func nonEmptyNotes(note string) []string {
	if note == "" {
		return nil
	}
	return []string{note}
}

// snapshotURI formats "s3://<bucket>/<layout.SnapshotDir(seq, at)>".
func snapshotURI(bucket string, layout storage.Layout, seq int, at time.Time) string {
	return fmt.Sprintf("s3://%s/%s", bucket, layout.SnapshotDir(seq, at))
}

// readLatest fetches and parses layout.Latest(), returning ok=false on any
// error -- storage.IsNotFound(err) on the very first freeze is expected, not
// an error, and any other error here is defensive-only (the primary seq
// source is s.FreezeCount).
func (h *SnapshotHook) readLatest(ctx context.Context, layout storage.Layout) (storage.Latest, bool) {
	rc, _, err := h.Backend.Get(ctx, layout.Latest())
	if err != nil {
		return storage.Latest{}, false
	}
	defer func() { _ = rc.Close() }()
	l, err := storage.ParseLatest(rc)
	if err != nil {
		return storage.Latest{}, false
	}
	return l, true
}

func (h *SnapshotHook) archiveOpts() storage.ArchiveOptions {
	return storage.ArchiveOptions{Level: zstd.SpeedDefault, Concurrency: 2, Exclude: SnapshotExclude}
}

func (h *SnapshotHook) sleep(ctx context.Context, d time.Duration) error {
	if h.Sleep != nil {
		return h.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (h *SnapshotHook) recordAttemptCount(ctx context.Context, seq, attempts int) {
	if err := h.Store.RecordSnapshotAttempt(ctx, SnapshotAttempt{Seq: seq, Phase: v1alpha1.SnapshotAttemptInProgress, Attempts: attempts}, h.nowSafe()); err != nil {
		h.Log.Info("recording snapshot attempt count failed (continuing)", "error", err.Error())
	}
}

// snapshotAttemptMessageMaxBytes matches SnapshotAttemptStatus.Message's
// +kubebuilder:validation:MaxLength=512 (api/v1alpha1/sandboxenvironment_types.go).
// AWS SDK errors routinely exceed this once the full request URL and a
// chained cause ("request send failed, Put \"https://...\": ... connection
// reset by peer") are included -- without truncation, the API server
// rejects the status PATCH outright as invalid, so recordFailure silently
// never lands: status.snapshotAttempt is permanently stuck at InProgress
// (the previous, successfully-recorded state) forever, with no further
// write and so no further watch event to ever correct it. This was found
// live, mid-debugging this exact package's e2e coverage: the sidecar's own
// log showed "recording snapshot failure failed" with the API server's
// "Too long: may not be more than 512 bytes" validation error, right after
// it had correctly exhausted its retry budget in ~30s.
const snapshotAttemptMessageMaxBytes = 512

func (h *SnapshotHook) recordFailure(ctx context.Context, seq, attempts int, reason, message string) {
	if len(message) > snapshotAttemptMessageMaxBytes {
		message = message[:snapshotAttemptMessageMaxBytes]
	}
	if err := h.Store.RecordSnapshotAttempt(ctx, SnapshotAttempt{
		Seq: seq, Phase: v1alpha1.SnapshotAttemptFailed, Attempts: attempts, Reason: reason, Message: message,
	}, h.nowSafe()); err != nil {
		h.Log.Info("recording snapshot failure failed", "error", err.Error())
	}
}

func (h *SnapshotHook) nowSafe() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

// classifySnapshotErr maps a storage error into one of the SnapshotReason*
// machine reasons.
func classifySnapshotErr(err error) string {
	switch {
	case storage.IsPermission(err):
		return SnapshotReasonCredentialsInvalid
	case storage.IsUnreachable(err):
		return SnapshotReasonBackendUnreachable
	default:
		return SnapshotReasonArchiveFailed
	}
}

// retrySnapshotStep retries fn while storage.IsRetryable(err) -- i.e. only
// ErrUnreachable-kinded failures. ErrNotFound/ErrInvalid/ErrPermission/
// ErrCorrupt are permanent and surface immediately. Exponential backoff with
// jitter, bounded by Cfg.SnapshotRetries; h.Sleep is injected so tests run
// instantly.
//
// Each attempt runs under its own Cfg.SnapshotStepTimeout-bounded context,
// not the bare outer ctx: internal/storage's S3 client is built with a
// plain http.Client{} (no Timeout, see s3.go), so a single slow or wedged
// backend call would otherwise hang indefinitely -- defeating the whole
// point of a "bounded" retry budget, since the loop can never even reach
// its next attempt, let alone give up and report Failed. A context deadline
// hit mid-call is classified ErrUnreachable (retryable) by
// classifySnapshotErr/classifyS3Error, so a timed-out attempt is treated
// exactly like any other transient failure and counts toward maxRetries.
func (h *SnapshotHook) retrySnapshotStep(ctx context.Context, seq int, step string, fn func(context.Context) error) error {
	delays := []time.Duration{1 * time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second}
	maxRetries := h.Cfg.SnapshotRetries
	if maxRetries <= 0 {
		maxRetries = defaultSnapshotRetries
	}
	stepTimeout := h.Cfg.SnapshotStepTimeout
	if stepTimeout <= 0 {
		stepTimeout = defaultSnapshotStepTimeout
	}

	var err error
	for attempt := 0; ; attempt++ {
		stepCtx, cancel := context.WithTimeout(ctx, stepTimeout)
		err = fn(stepCtx)
		cancel()
		if err == nil {
			return nil
		}
		if !storage.IsRetryable(err) || attempt >= maxRetries {
			return err
		}
		delay := delays[len(delays)-1]
		if attempt < len(delays) {
			delay = delays[attempt]
		}
		// jitter: +/- 20%
		jitter := time.Duration(rand.Int63n(int64(delay) / 5)) //nolint:gosec // G404: jitter timing, not security-sensitive
		delay = delay - delay/10 + jitter
		h.Log.Info("retrying snapshot step", "seq", seq, "step", step, "attempt", attempt+1, "error", err.Error())
		if serr := h.sleep(ctx, delay); serr != nil {
			return serr
		}
	}
}
