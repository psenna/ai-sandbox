// Command manager is the ai-sandbox operator entrypoint: it loads
// configuration, builds a controller-runtime manager with leader election
// and health/readiness probes wired in, and runs it until SIGTERM/SIGINT.
package main

import (
	"log/slog"
	"os"

	"github.com/go-logr/logr"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/psenna/ai-sandbox/operator/internal/config"
	"github.com/psenna/ai-sandbox/operator/internal/operator"
)

func main() {
	cfg, err := config.Load(os.Args[1:], os.Getenv)
	if err != nil {
		_, _ = os.Stderr.WriteString("invalid configuration: " + err.Error() + "\n")
		os.Exit(2)
	}
	if err := cfg.Validate(); err != nil {
		_, _ = os.Stderr.WriteString("invalid configuration: " + err.Error() + "\n")
		os.Exit(2)
	}

	// logr's slog bridge maps logr's V(n) to slog.Level(-n): slog's built-in
	// levels are Debug=-4, Info=0, Warn=4, Error=8, so V(1) is slog level -1,
	// V(2) is -2, and so on. cfg.LogVerbosity (default 0) therefore leaves
	// the handler at the shipped slog.LevelInfo (every V(n>=1) line
	// dropped) unless explicitly raised via --log-verbosity/LOG_VERBOSITY.
	ctrl.SetLogger(logr.FromSlogHandler(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.Level(-cfg.LogVerbosity),
	})))
	log := ctrl.Log.WithName("setup")

	restCfg, err := ctrl.GetConfig()
	if err != nil {
		log.Error(err, "unable to get kubeconfig")
		os.Exit(1)
	}

	mgr, err := operator.New(restCfg, cfg)
	if err != nil {
		log.Error(err, "unable to create manager")
		os.Exit(1)
	}

	if err := operator.SetupControllers(mgr, cfg); err != nil {
		log.Error(err, "unable to set up controllers")
		os.Exit(1)
	}

	log.Info("starting operator",
		"slotCapacity", cfg.SlotCapacity,
		"schedulerInterval", cfg.SchedulerInterval,
		"warmCacheGCInterval", cfg.WarmCacheGCInterval,
		"clusterID", cfg.ClusterID,
		"watchNamespace", cfg.WatchNamespace,
		"defaultSandboxClass", cfg.DefaultSandboxClass,
		"leaderElect", cfg.EnableLeaderElection,
		"metricsCollectInterval", cfg.MetricsCollectInterval,
		"logVerbosity", cfg.LogVerbosity,
	)

	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		log.Error(err, "manager exited with error")
		os.Exit(1)
	}
	log.Info("operator stopped")
}
