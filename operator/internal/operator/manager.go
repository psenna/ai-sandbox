// Package operator wires the operator's controller-runtime manager:
// scheme registration, leader election, health/readiness probes and the
// metrics listener. Controller registration itself lives in
// controllers.go's SetupControllers, added in issue #18 -- kept separate
// from New so manager construction stays testable without a cluster (see
// manager_test.go).
package operator

import (
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	sandboxv1alpha1 "github.com/psenna/ai-sandbox/operator/api/v1alpha1"
	"github.com/psenna/ai-sandbox/operator/internal/config"
)

// Scheme builds the runtime.Scheme the manager uses to decode/encode
// objects. It registers the client-go built-in types plus the operator's
// own sandbox.psenna.dev/v1alpha1 API types (SandboxClass,
// SandboxEnvironment), added in issue #17. The controller for
// SandboxEnvironment is registered by SetupControllers (issue #18).
func Scheme() (*runtime.Scheme, error) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("registering client-go scheme: %w", err)
	}
	if err := sandboxv1alpha1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("registering sandbox.psenna.dev/v1alpha1 scheme: %w", err)
	}
	return scheme, nil
}

// Options builds the controller-runtime manager Options for cfg.
func Options(cfg config.Config, scheme *runtime.Scheme) ctrl.Options {
	opts := ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: cfg.MetricsAddr,
		},
		HealthProbeBindAddress:        cfg.HealthProbeAddr,
		LeaderElection:                cfg.EnableLeaderElection,
		LeaderElectionID:              cfg.LeaderElectionID,
		LeaderElectionNamespace:       cfg.LeaderElectionNamespace,
		LeaderElectionReleaseOnCancel: true,
	}

	if cfg.WatchNamespace != "" {
		opts.Cache = cache.Options{
			DefaultNamespaces: map[string]cache.Config{
				cfg.WatchNamespace: {},
			},
		}
	}

	return opts
}

// New constructs a controller-runtime manager configured from cfg, with
// /healthz and /readyz checks wired in. No controllers are registered here;
// call SetupControllers separately (see cmd/main.go).
func New(restCfg *rest.Config, cfg config.Config) (manager.Manager, error) {
	scheme, err := Scheme()
	if err != nil {
		return nil, fmt.Errorf("building scheme: %w", err)
	}

	mgr, err := ctrl.NewManager(restCfg, Options(cfg, scheme))
	if err != nil {
		return nil, fmt.Errorf("creating manager: %w", err)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return nil, fmt.Errorf("adding healthz check: %w", err)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return nil, fmt.Errorf("adding readyz check: %w", err)
	}

	return mgr, nil
}
