package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
	"github.com/psenna/ai-sandbox/operator/internal/metrics"
	"github.com/psenna/ai-sandbox/operator/internal/scheduler"
)

// SlotScheduler is the fixed-capacity admission loop (#20). It is a
// manager.Runnable, NOT a reconciler: admission is a decision across every
// environment in the watch scope, which a per-object Reconcile cannot make
// deterministically -- see internal/scheduler's doc comment and D1-D5 of
// the #20 design record for the full rationale.
type SlotScheduler struct {
	// Client issues the grant writes (Status().Update).
	Client client.Client
	// Reader is a LIVE, UNCACHED reader (mgr.GetAPIReader()). Both the
	// occupancy List and the pre-write re-Get inside the retry loop go
	// through it: a RetryOnConflict loop over a cache-backed Get can burn
	// every retry re-reading the same stale object, and occupancy is the
	// one invariant in this issue that must never be computed from stale
	// data.
	Reader client.Reader

	Capacity  int
	Interval  time.Duration
	Namespace string // "" = all namespaces; mirrors Config.WatchNamespace
	Clock     func() time.Time

	// OnPass is the extension point #33 wires Prometheus metrics into
	// (queue depth, occupancy/utilisation, per-grant wait time). Nil is a
	// no-op. This issue emits no metrics itself.
	OnPass func(PassStats)

	// Metrics records queue-wait observations and per-pass reconcile errors
	// (#33). Nil-guarded, matching Reconciler.Metrics; the gauges
	// (occupancy/capacity/queue depth) are recomputed separately by
	// MetricsCollector (metricscollector.go), not here.
	Metrics *metrics.Collectors
	// Recorder emits the SlotGranted Event on this environment when a grant
	// lands (#33). Nil-guarded, matching Reconciler.Recorder.
	Recorder events.EventRecorder
}

// PassStats summarizes one admission pass, for OnPass and for tests.
type PassStats struct {
	Capacity, Occupancy, QueueDepth int
	Admitted, Skipped, Errors       int
	Duration                        time.Duration
	Grants                          []GrantResult
}

// GrantResult records the outcome of attempting one grant.
type GrantResult struct {
	Namespace, Name string
	Waited          time.Duration // GrantedAt - QueuedSince, zero if QueuedSince was nil
	Granted         bool
}

var (
	_ manager.Runnable               = (*SlotScheduler)(nil)
	_ manager.LeaderElectionRunnable = (*SlotScheduler)(nil)
)

// NeedLeaderElection ensures exactly one instance of this loop ever runs
// cluster-wide, which is layer 1 of this issue's double-grant prevention
// (see the design record). Stated explicitly here rather than relied upon
// implicitly.
func (s *SlotScheduler) NeedLeaderElection() bool { return true }

func (s *SlotScheduler) now() time.Time {
	if s.Clock != nil {
		return s.Clock()
	}
	return time.Now()
}

// Start runs one admission pass immediately, then one per Interval, until
// ctx is cancelled. A failed pass is logged, never returned -- returning an
// error here would take the whole manager down for what is very likely a
// transient List failure.
func (s *SlotScheduler) Start(ctx context.Context) error {
	log := ctrl.LoggerFrom(ctx).WithName("slotscheduler")
	s.runAndLog(ctx, log)
	ticker := time.NewTicker(s.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			s.runAndLog(ctx, log)
		}
	}
}

func (s *SlotScheduler) runAndLog(ctx context.Context, log logr.Logger) {
	stats, err := s.RunOnce(ctx)
	if err != nil {
		log.Error(err, "admission pass failed")
		s.Metrics.RecordReconcileError(metrics.ControllerSlotScheduler)
		return
	}
	if stats.Admitted > 0 || stats.Errors > 0 {
		log.V(1).Info("admission pass", "capacity", stats.Capacity, "occupancy", stats.Occupancy,
			"queueDepth", stats.QueueDepth, "admitted", stats.Admitted, "skipped", stats.Skipped, "errors", stats.Errors)
	}
	// Per-grant queue-wait observations (#5) and per-item admission errors
	// (#11) -- a List-level failure above is one reconcile_errors_total
	// increment; a per-grant error inside an otherwise-successful pass is one
	// increment per erroring item, since PassStats.Errors already counts them
	// individually.
	for _, g := range stats.Grants {
		if g.Granted && g.Waited > 0 {
			s.Metrics.ObserveQueueWait(g.Waited)
		}
	}
	for i := 0; i < stats.Errors; i++ {
		s.Metrics.RecordReconcileError(metrics.ControllerSlotScheduler)
	}
	if s.OnPass != nil {
		s.OnPass(stats)
	}
}

// RunOnce performs exactly one admission pass: list, partition, decide,
// write. Exported so tests and a manual/debug invocation can trigger a pass
// without waiting for the ticker.
func (s *SlotScheduler) RunOnce(ctx context.Context) (PassStats, error) {
	start := s.now()
	var list v1alpha1.SandboxEnvironmentList
	if err := s.Reader.List(ctx, &list, client.InNamespace(s.Namespace)); err != nil {
		return PassStats{}, fmt.Errorf("listing environments: %w", err)
	}

	occupancy, candidates := scheduler.Partition(list.Items)
	grants := scheduler.Admit(candidates, occupancy, s.Capacity)

	stats := PassStats{Capacity: s.Capacity, Occupancy: occupancy, QueueDepth: len(candidates)}
	now := s.now()
	for _, g := range grants {
		s.applyGrant(ctx, g, candidates, now, &stats)
	}
	stats.Duration = s.now().Sub(start)
	return stats, nil
}

// applyGrant attempts one grant and folds the outcome into stats.
func (s *SlotScheduler) applyGrant(ctx context.Context, g scheduler.Grant, candidates []v1alpha1.SandboxEnvironment, now time.Time, stats *PassStats) {
	queuedSince := queuedSinceFor(candidates, g.UID)
	waited := time.Duration(0)
	if queuedSince != nil {
		waited = now.Sub(*queuedSince)
	}
	// occupancyAfter approximates this environment's occupancy position once
	// its grant lands: the pass-start occupancy plus this grant's 1-based
	// position among this pass's own grants. Advisory only -- it's the
	// SlotGranted event's message, not a metric.
	occupancyAfter := stats.Occupancy + g.Position
	granted, err := s.grant(ctx, g, now, waited, occupancyAfter)
	if err != nil {
		stats.Errors++
		return
	}
	if granted {
		stats.Admitted++
	} else {
		stats.Skipped++
	}
	stats.Grants = append(stats.Grants, GrantResult{Namespace: g.Namespace, Name: g.Name, Waited: waited, Granted: granted})
}

func queuedSinceFor(candidates []v1alpha1.SandboxEnvironment, uid types.UID) *time.Time {
	for _, c := range candidates {
		if c.UID == uid && c.Status.QueuedSince != nil {
			t := c.Status.QueuedSince.Time
			return &t
		}
	}
	return nil
}

// grant re-validates and writes one admission decision, retrying on a
// genuine API-server conflict. It re-Gets (uncached) rather than trusting
// the List snapshot: another writer (a concurrently-running scheduler
// instance, or an interloper in a test) may have already granted, deleted,
// or replaced the object since the List. LeaseName is deliberately left
// unset: nothing in this codebase creates a real coordination.k8s.io/Lease
// object, and populating it with a synthetic string would advertise an
// object that doesn't exist.
func (s *SlotScheduler) grant(ctx context.Context, g scheduler.Grant, now time.Time, waited time.Duration, occupancyAfter int) (granted bool, err error) {
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fresh := &v1alpha1.SandboxEnvironment{}
		getErr := s.Reader.Get(ctx, client.ObjectKey{Namespace: g.Namespace, Name: g.Name}, fresh)
		if apierrors.IsNotFound(getErr) {
			granted = false
			return nil
		}
		if getErr != nil {
			return getErr
		}
		if fresh.UID != g.UID || !scheduler.IsCandidate(fresh) {
			granted = false
			return nil
		}
		grantedAt := metav1.NewTime(now).Rfc3339Copy()
		fresh.Status.Slot = v1alpha1.SlotStatus{Granted: true, GrantedAt: &grantedAt}
		if updErr := s.Client.Status().Update(ctx, fresh); updErr != nil {
			return updErr
		}
		granted = true
		// SlotGranted (#33): only the attempt that actually lands emits the
		// event, mirroring writeStatus/observeTransition's own
		// only-fire-on-success discipline. fresh is the object as of the
		// successful write, exactly what Eventf's "regarding" needs.
		emitEvent(s.Recorder, fresh, corev1.EventTypeNormal, ReasonSlotGranted, "Schedule",
			"scheduling slot granted after %s in the queue (occupancy %d of %d)", waited, occupancyAfter, s.Capacity)
		return nil
	})
	return granted, err
}
