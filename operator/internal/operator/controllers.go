package operator

import (
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/psenna/ai-sandbox/operator/internal/config"
	"github.com/psenna/ai-sandbox/operator/internal/controller"
)

// SetupControllers registers every controller with mgr. Separate from New so
// manager construction stays testable without a cluster (manager_test.go
// starts a manager against a fake host; a registered controller's informer
// would never sync there).
func SetupControllers(mgr manager.Manager, cfg config.Config) error {
	if err := (&controller.Reconciler{
		Client:               mgr.GetClient(),
		ClassSecretNamespace: cfg.ClassSecretNamespace,
		ClusterID:            cfg.ClusterID,
		WatchNamespace:       cfg.WatchNamespace,
		SidecarImage:         cfg.SidecarImage,
	}).SetupWithManager(mgr); err != nil {
		return err
	}
	// The slot scheduler is a Runnable, not a controller: admission compares
	// every environment in the watch scope, which no per-object Reconcile
	// can do deterministically (#20). mgr.Add places it in the
	// leader-election runnable group (see SlotScheduler.NeedLeaderElection),
	// so exactly one instance ever runs cluster-wide.
	if err := mgr.Add(&controller.SlotScheduler{
		Client:    mgr.GetClient(),
		Reader:    mgr.GetAPIReader(),
		Capacity:  cfg.SlotCapacity,
		Interval:  cfg.SchedulerInterval,
		Namespace: cfg.WatchNamespace,
	}); err != nil {
		return err
	}
	// The warm-cache GC is a Runnable for the same reason (#29): deleting a
	// frozen environment's workspace PVC once its snapshot is older than the
	// class's warmCacheTTL is a cross-object, wall-clock-driven policy
	// decision, not a per-object event. Same leader-election group, so
	// exactly one instance ever runs cluster-wide.
	return mgr.Add(&controller.WarmCacheGC{
		Client:    mgr.GetClient(),
		Reader:    mgr.GetAPIReader(),
		Interval:  cfg.WarmCacheGCInterval,
		Namespace: cfg.WatchNamespace,
	})
}
