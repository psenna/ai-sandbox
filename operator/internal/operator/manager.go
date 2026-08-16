// Package operator wires the operator's controller-runtime manager:
// scheme registration, leader election, health/readiness probes and the
// metrics listener. Controllers are added in later issues (#17, #18).
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

	"github.com/psenna/ai-sandbox/operator/internal/config"
)

// Scheme builds the runtime.Scheme the manager uses to decode/encode
// objects. It currently registers only the client-go built-in types; the
// operator's own API types are added in issue #17.
func Scheme() (*runtime.Scheme, error) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("registering client-go scheme: %w", err)
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
// /healthz and /readyz checks wired in. No controllers are registered; that
// happens in issues #17 and #18.
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
