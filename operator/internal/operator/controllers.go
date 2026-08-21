package operator

import (
	"sync/atomic"

	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/psenna/ai-sandbox/operator/internal/config"
	"github.com/psenna/ai-sandbox/operator/internal/controller"
	"github.com/psenna/ai-sandbox/operator/internal/metrics"
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
		Probes:               &controller.ProbeEvaluator{Metrics: metrics.Default},
		Recorder:             mgr.GetEventRecorder("ai-sandbox-operator"),
		OperatorIngressLabel: cfg.OperatorIngressLabel,
		CNI:                  cniResult,
		Metrics:              metrics.Default,
	}).SetupWithManager(mgr); err != nil {
		return err
	}
	// The slot scheduler is a Runnable, not a controller: admission compares
	// every environment in the watch scope, which no per-object Reconcile
	// can do deterministically (#20). mgr.Add places it in the
	// leader-election runnable group (see SlotScheduler.NeedLeaderElection),
	// so exactly one instance ever runs cluster-wide. Recorder is the same
	// "ai-sandbox-operator" event source string the Reconciler above uses
	// (#33): they are the same logical component's Events from the cluster's
	// point of view.
	if err := mgr.Add(&controller.SlotScheduler{
		Client:    mgr.GetClient(),
		Reader:    mgr.GetAPIReader(),
		Capacity:  cfg.SlotCapacity,
		Interval:  cfg.SchedulerInterval,
		Namespace: cfg.WatchNamespace,
		Metrics:   metrics.Default,
		Recorder:  mgr.GetEventRecorder("ai-sandbox-operator"),
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
		Metrics:   metrics.Default,
	}); err != nil {
		return err
	}
	// The CNI enforcement probe is a Runnable for the same reason (#31): it
	// is a wall-clock-driven, cluster-wide capability check, not a per-object
	// event. Leader-elected so exactly one instance ever runs cluster-wide,
	// and it writes the shared cniResult the reconciler reads.
	if err := mgr.Add(&controller.CNIProbeRunnable{
		Client:    mgr.GetClient(),
		Namespace: cfg.ClassSecretNamespace,
		Image:     cfg.SidecarImage,
		Interval:  cfg.CNIProbeInterval,
		Result:    cniResult,
		Metrics:   metrics.Default,
	}); err != nil {
		return err
	}
	// Retention GC is a Runnable for the same reason WarmCacheGC is (#32):
	// deleting a terminal archive's storage once it is older than the
	// configured TTL, and reclaiming orphaned storage nothing references any
	// more, are both wall-clock-driven, cross-object policy decisions, not
	// per-object events. Same leader-election group, so exactly one instance
	// ever runs cluster-wide.
	if err := mgr.Add(&controller.RetentionGC{
		Client:          mgr.GetClient(),
		Reader:          mgr.GetAPIReader(),
		SecretNamespace: cfg.ClassSecretNamespace,
		ClusterID:       cfg.ClusterID,
		TTL:             cfg.RetentionTTL,
		DryRun:          cfg.RetentionDryRun,
		Interval:        cfg.RetentionGCInterval,
		Namespace:       cfg.WatchNamespace,
		Metrics:         metrics.Default,
	}); err != nil {
		return err
	}
	// MetricsCollector recomputes the gauge metrics (environments by phase,
	// slot occupancy/capacity, queue depth) that have no natural per-event
	// trigger (#33). Deliberately added OUTSIDE the leader-election runnable
	// group (see MetricsCollector.NeedLeaderElection's doc comment): every
	// replica must keep its own /metrics endpoint's gauges live, not just
	// the leader's.
	return mgr.Add(&controller.MetricsCollector{
		Client:    mgr.GetClient(),
		Capacity:  cfg.SlotCapacity,
		Interval:  cfg.MetricsCollectInterval,
		Namespace: cfg.WatchNamespace,
		Metrics:   metrics.Default,
	})
}
