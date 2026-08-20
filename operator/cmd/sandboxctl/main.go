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
//	restore      restore a snapshot into the mounted workspace/agent-home
//	             and exit (the restore init container's entire program;
//	             #29, see internal/sandboxctl/runrestore.go)
//	archive      assemble run.json + archive/context.tar.zst and exit (the
//	             terminal-archive Job's entire program; #32, see
//	             internal/sandboxctl/archive.go)
//	probe-tcp    dial host:port and exit 0/1 (the CNI enforcement probe's
//	             client; #31, see internal/sandboxctl/cniprobe.go)
//	probe-listen  listen on :port forever (the CNI enforcement probe's server;
//	             the pod-to-pod dial target; #31, see internal/sandboxctl/cniprobe.go)
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
		_, _ = fmt.Fprintln(os.Stderr, "usage: sandboxctl <serve|healthcheck|freeze-once|restore|archive|probe-tcp|probe-listen> [flags]")
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
	case "restore":
		return runRestore(args[2:])
	case "archive":
		return runArchive(args[2:])
	case "probe-tcp":
		if len(args) < 3 {
			_, _ = fmt.Fprintln(os.Stderr, "usage: sandboxctl probe-tcp <host:port>")
			return 2
		}
		if err := sandboxctl.ProbeTCP(args[2]); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, "probe-tcp: "+err.Error())
			return 1
		}
		return 0
	case "probe-listen":
		if len(args) < 3 {
			_, _ = fmt.Fprintln(os.Stderr, "usage: sandboxctl probe-listen <port>")
			return 2
		}
		if err := sandboxctl.ProbeListen(args[2]); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, "probe-listen: "+err.Error())
			return 1
		}
		return 0
	default:
		_, _ = fmt.Fprintf(os.Stderr, "unknown subcommand %q; usage: sandboxctl <serve|healthcheck|freeze-once|restore|archive|probe-tcp|probe-listen> [flags]\n", args[1])
		return 2
	}
}

// runArchive reuses sandboxctl.Load (the archive subcommand accepts the same
// base flag set as serve/freeze-once/restore), requires
// --environment/--namespace, and validates cfg.Snapshot before running --
// mirroring runFreezeOnce exactly.
func runArchive(args []string) int {
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

	if err := sandboxctl.RunArchive(context.Background(), cfg, log); err != nil {
		log.Error(err, "archive exited with error")
		return 1
	}
	return 0
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

// runRestore reuses sandboxctl.Load (the restore subcommand accepts the
// same base flag set as serve/freeze-once, plus the --restore-* flags
// registered by registerRestoreFlags), requires --environment/--namespace,
// and validates both cfg.Snapshot and cfg.Restore before running --
// mirroring runFreezeOnce exactly.
func runRestore(args []string) int {
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
	if err := cfg.Restore.Validate(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "invalid configuration: "+err.Error())
		return 2
	}

	log := logr.FromSlogHandler(slog.NewJSONHandler(os.Stderr, nil))

	if err := sandboxctl.RunRestore(context.Background(), cfg, log); err != nil {
		log.Error(err, "restore exited with error")
		return 1
	}
	return 0
}
