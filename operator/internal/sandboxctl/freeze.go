package sandboxctl

import (
	"context"

	"github.com/go-logr/logr"
)

// FreezeHook is invoked exactly once, when the sidecar first observes its
// environment enter Freezing (see poller.go's latchFreezing). This issue
// implements only the SIGNAL and the QUIESCE: the snapshot itself --
// workload-container teardown, tar+zstd of /workspace and the agent home,
// upload via internal/storage, manifest.json, latest.json, the
// status.snapshot write and the resume marker -- is issue #28's scope in
// full (its own issue body claims all of it explicitly). This package does
// not import internal/storage at all.
type FreezeHook interface {
	Freeze(ctx context.Context, s Snapshot) error
}

// noopFreezeHook logs and returns nil, mirroring
// internal/controller/actions.go's notImplemented convention: silently
// doing nothing would hide a missing subsystem; returning an error would
// wedge the sidecar in a restart loop for machinery that doesn't exist yet.
type noopFreezeHook struct {
	log logr.Logger
}

// NewNoopFreezeHook builds the default FreezeHook used by run.go until #28
// lands.
func NewNoopFreezeHook(log logr.Logger) FreezeHook {
	return noopFreezeHook{log: log}
}

func (h noopFreezeHook) Freeze(_ context.Context, s Snapshot) error {
	h.log.Info("freeze detected; snapshot/quiesce not implemented yet (see issue #28)",
		"environment", s.Environment.Name, "namespace", s.Environment.Namespace, "phase", s.Phase, "issue", 28)
	return nil
}
