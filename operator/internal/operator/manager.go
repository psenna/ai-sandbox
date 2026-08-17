// Package operator wires the operator's controller-runtime manager:
// scheme registration, leader election, health/readiness probes and the
// metrics listener. Controller registration itself lives in
// controllers.go's SetupControllers, added in issue #18 -- kept separate
// from New so manager construction stays testable without a cluster (see
// manager_test.go).
package operator

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
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
		// Secret reads must always be live/uncached: the class-referenced
		// Secrets an environment's gitProxy service points at (see
		// Config.ClassSecretNamespace, internal/controller's
		// resolveCredentials) live in the operator's own namespace, but
		// environments -- and this ClusterRole's secrets grant -- span
		// arbitrary namespaces. Without this, an informer would cache every
		// Secret in every watched namespace.
		Client: client.Options{
			Cache: &client.CacheOptions{
				DisableFor: []client.Object{&corev1.Secret{}},
			},
		},
	}

	// Deliberately NOT setting Cache.ByObject to label-scope the
	// PVC/ConfigMap/ServiceAccount informers to app.kubernetes.io/managed-by
	// (as originally sketched for this issue): controller-runtime's
	// defaultOpts eagerly resolves a RESTMapping for every Cache.ByObject key
	// at manager-construction time (see
	// sigs.k8s.io/controller-runtime@v0.23.3/pkg/cache/cache.go's
	// defaultOpts -> apiutil.IsObjectNamespaced -> RESTMapping), which
	// requires live API server connectivity. That directly breaks this
	// package's own TestNew_ConstructsWithoutCluster /
	// TestManager_ServesProbesAndShutsDownCleanly contract ("New() alone
	// must not require connectivity"), which construct the manager against
	// an unreachable https://127.0.0.1:6443. managedByLabelSelector is kept
	// as a documented seam for #21 (or later) to revisit once that
	// constraint is either accepted or worked around (e.g. a static
	// RESTMapper). The informers for these three kinds remain unscoped
	// (namespace-restricted only, via DefaultNamespaces below, same as
	// before this issue) -- correctness is unaffected, this is purely a
	// cache-memory optimization left undone.
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
