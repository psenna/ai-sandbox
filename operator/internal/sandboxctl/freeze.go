package sandboxctl

import (
	"context"

	"github.com/go-logr/logr"
)

// FreezeHook is invoked exactly once, when the sidecar first observes its
// environment enter Freezing (see poller.go's latchFreezing). The real
// implementation, SnapshotHook (snapshot.go), does everything #28's issue
// body claims: workload-container teardown, tar+zstd of /workspace and the
// agent home, upload via internal/storage, manifest.json, latest.json, the
// status.snapshot write, and the resume marker.
type FreezeHook interface {
	Freeze(ctx context.Context, s Snapshot) error
}

// noopFreezeHook logs and returns nil, mirroring
// internal/controller/actions.go's notImplemented convention: silently
// doing nothing would hide a missing subsystem; returning an error would
// wedge the sidecar in a restart loop for machinery that doesn't exist yet.
//
// Selected by run.go when this sidecar was built for a class with no freeze
// support configured (--snapshot-backend="", e.g. no storage.backend was
// resolvable, or the class simply has none). NOT a placeholder for
// unimplemented machinery -- SnapshotHook is the real thing, selected
// whenever --snapshot-backend is non-empty.
type noopFreezeHook struct {
	log logr.Logger
}

// NewNoopFreezeHook builds the FreezeHook used by run.go when no snapshot
// backend is configured for this sidecar.
func NewNoopFreezeHook(log logr.Logger) FreezeHook {
	return noopFreezeHook{log: log}
}

func (h noopFreezeHook) Freeze(_ context.Context, s Snapshot) error {
	h.log.Info("freeze detected; no snapshot backend configured for this sidecar (--snapshot-backend is empty)",
		"environment", s.Environment.Name, "namespace", s.Environment.Namespace, "phase", s.Phase)
	return nil
}
