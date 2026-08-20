package operator

import (
	"sync/atomic"

	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/psenna/ai-sandbox/operator/internal/config"
	"github.com/psenna/ai-sandbox/operator/internal/controller"
)

// SetupControllers registers every controller with mgr. Separate from New so
// manager construction stays testable without a cluster (manager_test.go
// starts a manager against a fake host; a registered controller's informer
// would never sync there).
func SetupControllers(mgr manager.Manager, cfg config.Config) error {
	// The CNI enforcement probe's shared result (#31): the runnable writes it,
	// the reconciler reads it to decide the CNIEnforcement condition and the
	// not-enforced warning event. A single atomic pointer shared by both, so
	// the reconciler always sees the latest completed pass without racing the
	// leader-elected Runnable goroutine. A nil Load (before the first pass
	// completes) routes the CNIEnforcement condition to Unknown.
	cniResult := &atomic.Pointer[controller.CNIProbeResult]{}
	if err := (&controller.Reconciler{
		Client:               mgr.GetClient(),
		ClassSecretNamespace: cfg.ClassSecretNamespace,
		ClusterID:            cfg.ClusterID,
		WatchNamespace:       cfg.WatchNamespace,
		SidecarImage:         cfg.SidecarImage,
		Probes:               controller.NewProbeEvaluator(),
		Recorder:             mgr.GetEventRecorderFor("ai-sandbox-operator"),
		OperatorIngressLabel: cfg.OperatorIngressLabel,
		CNI:                  cniResult,
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
	if err := mgr.Add(&controller.WarmCacheGC{
		Client:    mgr.GetClient(),
		Reader:    mgr.GetAPIReader(),
		Interval:  cfg.WarmCacheGCInterval,
		Namespace: cfg.WatchNamespace,
	}); err != nil {
		return err
	}
	// The CNI enforcement probe is a Runnable for the same reason (#31): it
	// is a wall-clock-driven, cluster-wide capability check, not a per-object
	// event. Leader-elected so exactly one instance ever runs cluster-wide,
	// and it writes the shared cniResult the reconciler reads.
	return mgr.Add(&controller.CNIProbeRunnable{
		Client:    mgr.GetClient(),
		Namespace: cfg.ClassSecretNamespace,
		Image:     cfg.SidecarImage,
		Interval:  cfg.CNIProbeInterval,
		Result:    cniResult,
	})
}
