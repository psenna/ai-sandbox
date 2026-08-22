package controller

import (
	"context"
	"time"

	"github.com/go-logr/logr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
	"github.com/psenna/ai-sandbox/operator/internal/metrics"
	"github.com/psenna/ai-sandbox/operator/internal/scheduler"
)

// MetricsCollector is the periodic gauge-recomputation loop (#33) for the
// four metrics that have no natural per-event trigger: environments-by-
// phase, slot capacity/occupancy, and queue depth. It is a manager.Runnable,
// like SlotScheduler/WarmCacheGC/RetentionGC, for the same reason those are:
// a gauge snapshot compares every environment in the watch scope at once,
// which no per-object Reconcile can produce.
//
// Unlike every other Runnable in this package, MetricsCollector is NOT
// leader-elected -- see NeedLeaderElection's doc comment; this is
// deliberate and load-bearing, not an oversight to "fix".
type MetricsCollector struct {
	// Client is the manager's CACHED client (mgr.GetClient()), deliberately
	// NOT the live/uncached Reader every other Runnable in this package
	// uses: this loop only ever reads, for a metrics snapshot that is
	// re-computed every Interval anyway, so trading a little staleness for
	// avoiding a live List against the API server on every replica, every
	// Interval, is the right call -- see NeedLeaderElection.
	Client client.Client

	Capacity  int
	Interval  time.Duration
	Namespace string // "" = all namespaces; mirrors Config.WatchNamespace

	// Metrics is where RunOnce writes. Nil-guarded (Collectors' own
	// convention), so an unwired MetricsCollector never panics -- it just
	// computes and discards.
	Metrics *metrics.Collectors
}

var (
	_ manager.Runnable               = (*MetricsCollector)(nil)
	_ manager.LeaderElectionRunnable = (*MetricsCollector)(nil)
)

// NeedLeaderElection returns false: this is CRITICAL for per-replica gauge
// correctness, not a simplification to match SlotScheduler/WarmCacheGC/
// RetentionGC's own leader-elected NeedLeaderElection()==true. Those loops
// leader-elect because a double-run would double-grant or double-delete;
// MetricsCollector only reads and overwrites a gauge with a freshly
// computed value, which is idempotent no matter how many replicas run it.
// Leader-electing it would mean every NON-leader replica's own /metrics
// endpoint reports permanently-zero gauges (since only the leader's process
// would ever call RunOnce), which is wrong for any Prometheus setup that
// scrapes more than the leader (e.g. a per-pod ServiceMonitor, or a leader
// failover mid-scrape-interval).
func (c *MetricsCollector) NeedLeaderElection() bool { return false }

// Start runs one collection pass immediately, then one per Interval, until
// ctx is cancelled. A failed pass is logged, never returned -- mirrors
// WarmCacheGC.Start exactly: returning an error here would take the whole
// manager down for what is very likely a transient List failure.
func (c *MetricsCollector) Start(ctx context.Context) error {
	log := ctrl.LoggerFrom(ctx).WithName("metricscollector")
	c.runAndLog(ctx, log)
	ticker := time.NewTicker(c.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			c.runAndLog(ctx, log)
		}
	}
}

// runAndLog logs, never records reconcile_errors_total: MetricsCollector
// itself is not one of the controller-label allowlist's members (#11's
// table -- sandboxenvironment/slotscheduler/warmcachegc/cniprobe/
// retentiongc), since it exists purely to compute other metrics and has no
// state-changing side effect of its own to alert on.
func (c *MetricsCollector) runAndLog(ctx context.Context, log logr.Logger) {
	if err := c.RunOnce(ctx); err != nil {
		log.Error(err, "metrics collection pass failed")
	}
}

// RunOnce performs exactly one collection pass: one cached List, then
// SetEnvironmentsByPhase and SetSlots from it. Exported so tests and a
// manual/debug invocation can trigger a pass without waiting for the
// ticker, mirroring SlotScheduler.RunOnce/WarmCacheGC.RunOnce.
func (c *MetricsCollector) RunOnce(ctx context.Context) error {
	var list v1alpha1.SandboxEnvironmentList
	if err := c.Client.List(ctx, &list, client.InNamespace(c.Namespace)); err != nil {
		return err
	}

	counts := make(map[v1alpha1.Phase]int, len(v1alpha1.AllPhases))
	for i := range list.Items {
		// An empty phase (a freshly-created object the primary reconcile
		// hasn't touched yet) defaults to Pending -- matching
		// lifecycle.Next's own "phase == '' -> PhasePending" defaulting
		// (internal/lifecycle/next.go), so a gauge snapshot taken between
		// creation and the first reconcile counts it the same way the state
		// machine itself would.
		phase := list.Items[i].Status.Phase
		if phase == "" {
			phase = v1alpha1.PhasePending
		}
		counts[phase]++
	}
	c.Metrics.SetEnvironmentsByPhase(counts)

	occupancy, candidates := scheduler.Partition(list.Items)
	c.Metrics.SetSlots(c.Capacity, occupancy, len(candidates))
	return nil
}
