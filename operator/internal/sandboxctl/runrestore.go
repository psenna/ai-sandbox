package sandboxctl

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"

	"github.com/psenna/ai-sandbox/operator/internal/storage"
)

// RunRestore builds the same client/Store the sidecar uses, refreshes once,
// runs a RestoreHook, and returns. No HTTP server, no poller -- there is no
// agent to serve yet (the restore container runs and exits BEFORE the agent
// container ever starts; see internal/render/pod.go's init-container
// ordering doc comment). Mirrors RunFreezeOnce (freezeonce.go) exactly;
// this is the `restore` subcommand's entire program.
func RunRestore(ctx context.Context, cfg Config, log logr.Logger) error {
	c, err := buildClient()
	if err != nil {
		return err
	}

	store := NewStore(c, cfg.Namespace, cfg.Environment)
	if err := store.Refresh(ctx); err != nil {
		return fmt.Errorf("refreshing environment %s/%s: %w", cfg.Namespace, cfg.Environment, err)
	}
	snap := store.Snapshot()

	hook, err := buildRestoreHook(store, cfg, log)
	if err != nil {
		return err
	}

	if err := hook.Restore(ctx, snap); err != nil {
		return fmt.Errorf("restore: %w", err)
	}
	return nil
}

// buildRestoreHook builds the RestoreHook. cfg.Snapshot.Backend=="pvc"
// intentionally builds a RestoreHook with a nil Backend -- Restore fails
// closed on that case before ever touching it (see restore.go), matching
// buildFreezeHook's own pattern (run.go).
func buildRestoreHook(store Store, cfg Config, log logr.Logger) (*RestoreHook, error) {
	var be storage.Backend
	if cfg.Snapshot.Backend == "s3" {
		var err error
		be, err = BuildBackend(cfg.Snapshot)
		if err != nil {
			return nil, fmt.Errorf("building restore backend: %w", err)
		}
	}
	return NewRestoreHook(store, be, cfg, log), nil
}
