package sandboxctl

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
)

// RunFreezeOnce builds the same client/Store the sidecar uses, refreshes
// once, runs the SAME SnapshotHook (or noop hook, if no backend is
// configured), and returns. No HTTP server, no poller -- there is no agent
// to serve. This is the recovery Job's entire program (cmd/sandboxctl's
// "freeze-once" subcommand): the one-shot snapshot taken against a
// surviving workspace PVC when the pod that should have taken the snapshot
// is gone (see internal/controller/freeze.go's ensureSnapshotJob).
func RunFreezeOnce(ctx context.Context, cfg Config, log logr.Logger) error {
	c, err := buildClient()
	if err != nil {
		return err
	}

	store := NewStore(c, cfg.Namespace, cfg.Environment)
	if err := store.Refresh(ctx); err != nil {
		return fmt.Errorf("refreshing environment %s/%s: %w", cfg.Namespace, cfg.Environment, err)
	}
	snap := store.Snapshot()

	hook, err := buildFreezeHook(store, cfg, log)
	if err != nil {
		return err
	}

	if err := hook.Freeze(ctx, snap); err != nil {
		return fmt.Errorf("freeze-once: %w", err)
	}
	return nil
}
