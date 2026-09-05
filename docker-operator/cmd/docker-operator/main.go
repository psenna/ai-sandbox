// Command docker-operator is the entrypoint for the Docker-backed agent
// operator: a single local daemon that creates, deletes and reconciles
// per-agent Docker containers and serves the REST API and WebSocket
// terminal bridge for them.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/psenna/ai-sandbox/docker-operator/internal/agent"
	"github.com/psenna/ai-sandbox/docker-operator/internal/api"
	"github.com/psenna/ai-sandbox/docker-operator/internal/config"
	"github.com/psenna/ai-sandbox/docker-operator/internal/dockerclient"
	"github.com/psenna/ai-sandbox/docker-operator/internal/store"
	"github.com/psenna/ai-sandbox/docker-operator/internal/wsbridge"
)

// version is the build stamp reported on startup. It is a var, not a const,
// so a release build can override it with
// -ldflags "-X main.version=$(git describe --tags)"; nothing injects it yet.
var version = "dev"

// dockerPingTimeout bounds the startup fail-fast check: an operator that
// cannot reach Docker has nothing useful to do, and failing here turns every
// later "connection refused" deep in a request into one legible startup
// error instead.
const dockerPingTimeout = 10 * time.Second

// reconcileTimeout bounds the one-shot startup reconcile pass (task 6). It
// only lists Docker resources and, for records stuck mid-operation, tears
// down a handful of them -- generous headroom for a slow host, not a
// realistic budget.
const reconcileTimeout = time.Minute

// shutdownTimeout bounds how long graceful shutdown waits for in-flight
// requests (including open terminal WebSockets) to finish before the
// process exits anyway.
const shutdownTimeout = 10 * time.Second

func main() {
	log := slog.Default()
	if err := run(log); err != nil {
		log.Error("docker-operator exited with an error", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.Load(os.Args[1:], os.Getenv)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return fmt.Errorf("loading configuration: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}

	docker, err := dockerclient.NewFromEnv()
	if err != nil {
		return fmt.Errorf("building docker client: %w", err)
	}
	defer func() { _ = docker.Close() }()

	pingCtx, cancelPing := context.WithTimeout(context.Background(), dockerPingTimeout)
	err = docker.Ping(pingCtx)
	cancelPing()
	if err != nil {
		return fmt.Errorf("the docker daemon is not reachable: %w", err)
	}

	if err := ensureSharedNetworks(context.Background(), docker, cfg); err != nil {
		return err
	}

	st, err := store.Open(cfg.StateDBPath, cfg.MaxAgents)
	if err != nil {
		return fmt.Errorf("opening the state database: %w", err)
	}
	defer func() { _ = st.Close() }()

	mgr := agent.NewManager(docker, st, cfg, log, agent.Options{})

	reconcileCtx, cancelReconcile := context.WithTimeout(context.Background(), reconcileTimeout)
	report, err := mgr.Reconcile(reconcileCtx)
	cancelReconcile()
	if err != nil {
		// Reconcile already tried every record independently and returns a
		// joined error only for the ones it could not finish; one stuck
		// agent must not stop the operator from starting.
		log.Error("startup reconcile pass reported errors", "error", err)
	}
	log.Info("startup reconcile complete",
		"records", report.Records, "cleaned_up", len(report.CleanedUp), "unmanaged", len(report.Unmanaged))

	stopStatusSync := startStatusSync(docker, mgr, log)
	defer stopStatusSync()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", handleHealthz)
	mux.Handle("/api/", api.NewHandler(mgr, docker, log))
	mux.HandleFunc("GET /ws/agents/{id}/terminal", wsbridge.NewTerminalHandler(mgr, docker, log))

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	serveErr := make(chan error, 1)
	go func() {
		log.Info("docker-operator listening", "addr", cfg.ListenAddr, "version", version, "max_agents", cfg.MaxAgents)
		err := srv.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	select {
	case sig := <-sigCh:
		log.Info("received signal, shutting down", "signal", sig.String())
	case err := <-serveErr:
		if err != nil {
			return fmt.Errorf("http server error: %w", err)
		}
		// The server stopped on its own (Shutdown/Close called elsewhere);
		// nothing left to wait for.
		return nil
	}

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancelShutdown()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutting down http server: %w", err)
	}
	// Shutdown only returns once ListenAndServe has returned too, so the
	// goroutine above has already sent on serveErr; drain it so the
	// goroutine cannot leak blocked on the send (serveErr is buffered, so
	// this is a courtesy, not a requirement).
	<-serveErr
	log.Info("shutdown complete")
	return nil
}

func handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// startStatusSync subscribes to the Docker Events API, filtered to
// containers carrying the managed label, and reactively corrects an agent's
// store status when its OWN container (role=agent, not its DinD sidecar)
// leaves the running state without going through Delete -- e.g. the agent
// process crashed, or someone stopped the container by hand outside the
// operator. Returns a function that stops the subscription.
func startStatusSync(docker dockerclient.EventClient, mgr *agent.Manager, log *slog.Logger) func() {
	ctx, cancel := context.WithCancel(context.Background())
	filter := dockerclient.EventFilter{
		Types:  []dockerclient.EventType{dockerclient.EventTypeContainer},
		Labels: map[string]string{agent.LabelManaged: agent.LabelManagedValue},
	}
	events, errs := docker.Events(ctx, filter)

	go func() {
		for {
			select {
			case ev, ok := <-events:
				if !ok {
					return
				}
				handleContainerEvent(ctx, mgr, ev, log)
			case err, ok := <-errs:
				if !ok {
					return
				}
				if err != nil && !errors.Is(err, context.Canceled) {
					log.Warn("status-sync: docker event stream ended", "error", err)
				}
				return
			}
		}
	}()

	return cancel
}

// handleContainerEvent maps one container event to a store status
// correction, or ignores it. Only events on an agent's own container
// (role=agent) affect that agent's "running" status; its DinD sidecar
// stopping does not, by design -- the sidecar dying is a real problem the
// agent itself will surface (its DOCKER_HOST becomes unreachable), not a
// signal that the agent process is down.
func handleContainerEvent(ctx context.Context, mgr *agent.Manager, ev dockerclient.Event, log *slog.Logger) {
	if ev.Attributes[agent.LabelRole] != string(agent.RoleAgent) {
		return
	}
	id := ev.Attributes[agent.LabelAgentID]
	if id == "" {
		return
	}

	var newStatus store.Status
	switch ev.Action {
	case dockerclient.ActionDie, dockerclient.ActionOOM:
		newStatus = store.StatusError
	case dockerclient.ActionStop:
		newStatus = store.StatusStopped
	default:
		return
	}

	err := mgr.MarkUnexpectedExit(ctx, id, newStatus, "container "+string(ev.Action)+" unexpectedly")
	if err != nil && !store.IsNotFound(err) {
		log.Warn("status-sync: recording an unexpected container exit failed", "agent_id", id, "error", err)
	}
}

// ensureSharedNetworks creates the two shared, singleton networks (proxynet
// and dbnet) if they do not already exist, so the operator also works
// started bare (`docker run`), not only via docker-compose.yaml -- which
// creates them itself, declaratively, before the operator container ever
// starts, making this a no-op in the common case.
//
// These are NOT labeled ai-sandbox.docker-operator/managed: that label is
// the per-agent orphan-recovery mechanism (see internal/agent's Reconcile),
// and these two networks are intentionally shared infrastructure no single
// agent record owns -- labeling them would make every reconcile pass report
// them as "unmanaged", which they are not.
func ensureSharedNetworks(ctx context.Context, docker dockerclient.NetworkClient, cfg config.Config) error {
	for _, name := range []string{cfg.ProxynetName, cfg.DbnetName} {
		if err := ensureNetwork(ctx, docker, name); err != nil {
			return err
		}
	}
	return nil
}

// ensureNetwork creates a bridge network by name unless one already exists.
func ensureNetwork(ctx context.Context, docker dockerclient.NetworkClient, name string) error {
	_, err := docker.NetworkInspect(ctx, name)
	switch {
	case err == nil:
		return nil
	case !dockerclient.IsNotFound(err):
		return fmt.Errorf("inspecting network %q: %w", name, err)
	}
	if _, err := docker.NetworkCreate(ctx, dockerclient.NetworkSpec{Name: name}); err != nil {
		return fmt.Errorf("creating network %q: %w", name, err)
	}
	return nil
}
