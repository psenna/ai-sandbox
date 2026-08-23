package sandboxctl

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
	"github.com/psenna/ai-sandbox/operator/internal/storage"
)

// buildClient constructs the direct, uncached client.Client every subcommand
// (serve, freeze-once) shares.
func buildClient() (client.Client, error) {
	restCfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("loading in-cluster config: %w", err)
	}
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("adding v1alpha1 to scheme: %w", err)
	}
	c, err := client.New(restCfg, client.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("building client: %w", err)
	}
	return c, nil
}

// buildFreezeHook selects the FreezeHook: noop when no snapshot backend is
// configured, otherwise the real SnapshotHook. cfg.Snapshot.Backend=="pvc"
// intentionally builds a SnapshotHook with a nil Backend -- Freeze fails
// closed on that case before ever touching it (see snapshot.go).
func buildFreezeHook(store Store, cfg Config, log logr.Logger) (FreezeHook, error) {
	if cfg.Snapshot.Backend == "" {
		return NewNoopFreezeHook(log), nil
	}
	var be storage.Backend
	if cfg.Snapshot.Backend == "s3" {
		var err error
		be, err = BuildBackend(cfg.Snapshot)
		if err != nil {
			return nil, fmt.Errorf("building snapshot backend: %w", err)
		}
	}
	return NewSnapshotHook(store, be, NewEngineTeardown(cfg.Snapshot.Engine, cfg.Snapshot.EngineEndpoint, log), cfg.Snapshot, log), nil
}

// Run wires together the direct Kubernetes client, the Store, the Poller,
// and the HTTP server, then blocks until SIGTERM/SIGINT, performing a
// bounded graceful shutdown. Returns nil on a clean shutdown, non-nil
// otherwise (main.go maps that to exit code 1).
//
// Deliberately builds everything by hand rather than via
// controller-runtime's manager: see doc.go for why (no cache, no informer,
// the Role grants no list/watch).
func Run(ctx context.Context, cfg Config, log logr.Logger) error {
	c, err := buildClient()
	if err != nil {
		return err
	}

	store := NewStore(c, cfg.Namespace, cfg.Environment)

	hook, err := buildFreezeHook(store, cfg, log)
	if err != nil {
		return err
	}
	logf := func(format string, args ...any) { log.Info(fmt.Sprintf(format, args...)) }
	poll := NewPoller(store, cfg.PollInterval, hook, logf)

	env := EnvironmentRef{Name: cfg.Environment, Namespace: cfg.Namespace}
	srv := NewServer(cfg, store, poll, env, time.Now, logf)

	sigCtx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	pollCtx, cancelPoll := context.WithCancel(context.Background())
	defer cancelPoll()
	go poll.Run(pollCtx)

	serveErr := make(chan error, 1)
	go func() {
		log.Info("sandboxctl control API listening", "addr", cfg.Listen)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	select {
	case <-sigCtx.Done():
		log.Info("signal received, quiescing and shutting down", "shutdownTimeout", cfg.ShutdownTimeout)
		// Bounded, not context.Background(): a real snapshot can now
		// (post-#28) outlive terminationGracePeriodSeconds and get
		// SIGKILLed mid-upload. A snapshot exceeding ShutdownTimeout is
		// aborted by this context's cancellation, leaving no manifest and
		// therefore no snapshot a restore would accept -- the failure mode
		// is "no snapshot", never "a snapshot a restore would wrongly
		// trust".
		freezeCtx, cancelFreeze := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		poll.LatchFreezing(freezeCtx)
		cancelFreeze()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("graceful shutdown: %w", err)
		}
		<-serveErr
		return nil
	case err := <-serveErr:
		return err
	}
}
