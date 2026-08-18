// Command sandboxctl is the agent-to-operator control channel: a sidecar in
// every environment pod that holds the ServiceAccount token, validates every
// request from the agent against the allowlisted probe schema, and patches
// ONLY its own SandboxEnvironment's status. The agent container never holds
// a Kubernetes credential and never talks to the API server -- see
// internal/sandboxctl's doc.go for the full design record.
//
// Subcommands:
//
//	serve        run the localhost-only control API (the sidecar container's args)
//	healthcheck  dial 127.0.0.1:<port>/healthz and exit 0/1 (the startupProbe;
//	             distroless has no shell, so the probe must be this binary,
//	             not a curl/wget invocation)
//	freeze-once  take a single freeze snapshot and exit (the recovery Job's
//	             entire program; #28, see internal/sandboxctl/freezeonce.go)
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/go-logr/logr"

	"github.com/psenna/ai-sandbox/operator/internal/sandboxctl"
)

func main() {
	os.Exit(run(os.Args))
}

func run(args []string) int {
	if len(args) < 2 {
		_, _ = fmt.Fprintln(os.Stderr, "usage: sandboxctl <serve|healthcheck|freeze-once> [flags]")
		return 2
	}

	switch args[1] {
	case "serve":
		return runServe(args[2:])
	case "healthcheck":
		if err := sandboxctl.Healthcheck(args[2:], os.Getenv); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, "healthcheck: "+err.Error())
			return 1
		}
		return 0
	case "freeze-once":
		return runFreezeOnce(args[2:])
	default:
		_, _ = fmt.Fprintf(os.Stderr, "unknown subcommand %q; usage: sandboxctl <serve|healthcheck|freeze-once> [flags]\n", args[1])
		return 2
	}
}

func runServe(args []string) int {
	cfg, err := sandboxctl.Load(args, os.Getenv)
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "invalid configuration: "+err.Error())
		return 2
	}
	if err := cfg.Validate(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "invalid configuration: "+err.Error())
		return 2
	}

	log := logr.FromSlogHandler(slog.NewJSONHandler(os.Stderr, nil))

	if err := sandboxctl.Run(context.Background(), cfg, log); err != nil {
		log.Error(err, "sandboxctl exited with error")
		return 1
	}
	return 0
}

// runFreezeOnce reuses sandboxctl.Load (the freeze-once subcommand accepts
// the exact same flag set as serve; only Listen/PollInterval/ShutdownTimeout
// go unused) so the recovery Job's args stay identical in shape to the
// sidecar's own, keeping the two definitions from drifting.
func runFreezeOnce(args []string) int {
	cfg, err := sandboxctl.Load(args, os.Getenv)
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "invalid configuration: "+err.Error())
		return 2
	}
	if cfg.Environment == "" || cfg.Namespace == "" {
		_, _ = fmt.Fprintln(os.Stderr, "invalid configuration: --environment and --namespace are required")
		return 2
	}
	if err := cfg.Snapshot.Validate(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "invalid configuration: "+err.Error())
		return 2
	}

	log := logr.FromSlogHandler(slog.NewJSONHandler(os.Stderr, nil))

	if err := sandboxctl.RunFreezeOnce(context.Background(), cfg, log); err != nil {
		log.Error(err, "freeze-once exited with error")
		return 1
	}
	return 0
}
