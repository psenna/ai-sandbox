package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
	"github.com/psenna/ai-sandbox/operator/internal/metrics"
	"github.com/psenna/ai-sandbox/operator/internal/render"
)

// defaultWarmCacheTTL is the TTL applied when a class's warmCacheTTL is
// empty or fails to parse -- mirroring the CRD's +kubebuilder:default="30m"
// (sandboxclass_types.go), so a class that predates the field, or carries a
// malformed value, still gets the documented default rather than an
// unbounded warm cache.
const defaultWarmCacheTTL = 30 * time.Minute

// WarmCacheGC is the warm-PVC TTL reclamation loop (#29). It is a
// manager.Runnable, NOT a reconciler, for the same reason SlotScheduler
// isn't one: deleting a frozen environment's workspace PVC once its snapshot
// is older than the class's warmCacheTTL is a cross-object, wall-clock-driven
// policy decision, not a per-object event -- a per-object Reconcile has no
// natural trigger for "time passed while nothing else changed".
//
// What it deletes, and only what it deletes: the workspace PVC of an
// environment that is (a) exactly Waiting (frozen, pod gone, nothing mounts
// the workspace), (b) not being deleted, (c) holding a COMPLETE, VERIFIED
// snapshot in S3 (status.snapshot present with Seq >= FreezeCount-1, and no
// in-flight or failed snapshotAttempt), (d) on an S3-backed class, and (e)
// past the class's warmCacheTTL. Every condition is independently required:
// a Running environment's PVC is never touched, and an environment whose
// snapshot never landed is never touched -- the PVC is the only copy of the
// agent's context until the snapshot exists, so deleting it would be data
// loss, not reclamation.
//
// The delete is guarded against every race: a live re-Get of the environment
// immediately before the delete re-checks phase/snapshot (closing the
// "woke between List and Delete" window, exactly like SlotScheduler.grant's
// own re-Get pattern), the PVC is confirmed owned by the environment, and
// the delete carries a UID precondition so a PVC recreated since the Get
// can never be hit.
type WarmCacheGC struct {
	// Client issues the PVC deletes.
	Client client.Client
	// Reader is a LIVE, UNCACHED reader (mgr.GetAPIReader()). Both the
	// environment List and the pre-delete re-Get go through it: a
	// cache-backed Get can burn every retry re-reading the same stale
	// object, and eligibility is a delete-safety invariant that must never
	// be computed from stale data.
	Reader client.Reader

	Interval  time.Duration
	Namespace string // "" = all namespaces; mirrors Config.WatchNamespace
	Clock     func() time.Time

	// OnPass is the extension point #33 wires Prometheus metrics into
	// (warm-cache size, reclamation rate). Nil is a no-op. This issue emits
	// no metrics itself.
	OnPass func(GCStats)

	// Metrics records reclamation counts and per-pass reconcile errors
	// (#33). Nil-guarded, matching Reconciler.Metrics.
	Metrics *metrics.Collectors
}

// GCStats summarizes one GC pass, for OnPass and for tests.
type GCStats struct {
	Scanned, Eligible, Deleted, Skipped, Errors int
	Duration                                    time.Duration
	Reclaimed                                   []ReclaimedPVC
}

// ReclaimedPVC records one PVC this pass deleted.
type ReclaimedPVC struct {
	Namespace, Name, EnvName string
	Age                      time.Duration // now - snapshot.TakenAt at delete time
}

var (
	_ manager.Runnable               = (*WarmCacheGC)(nil)
	_ manager.LeaderElectionRunnable = (*WarmCacheGC)(nil)
)

// NeedLeaderElection ensures exactly one instance of this loop ever runs
// cluster-wide: two schedulers deleting PVCs concurrently would race on the
// same environment (both re-Get, both pass eligibility, one delete wins and
// the other 404s -- harmless but wasteful, and the UID precondition makes a
// wrong delete impossible regardless). Stated explicitly rather than relied
// upon implicitly, matching SlotScheduler.
func (g *WarmCacheGC) NeedLeaderElection() bool { return true }

func (g *WarmCacheGC) now() time.Time {
	if g.Clock != nil {
		return g.Clock()
	}
	return time.Now()
}

// Start runs one GC pass immediately, then one per Interval, until ctx is
// cancelled. A failed pass is logged, never returned -- returning an error
// here would take the whole manager down for what is very likely a transient
// List failure.
func (g *WarmCacheGC) Start(ctx context.Context) error {
	log := ctrl.LoggerFrom(ctx).WithName("warmcachegc")
	g.runAndLog(ctx, log)
	ticker := time.NewTicker(g.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			g.runAndLog(ctx, log)
		}
	}
}

func (g *WarmCacheGC) runAndLog(ctx context.Context, log logr.Logger) {
	stats, err := g.RunOnce(ctx)
	if err != nil {
		log.Error(err, "warm-cache GC pass failed")
		g.Metrics.RecordReconcileError(metrics.ControllerWarmCacheGC)
		return
	}
	if stats.Deleted > 0 || stats.Errors > 0 {
		log.V(1).Info("warm-cache GC pass", "scanned", stats.Scanned, "eligible", stats.Eligible,
			"deleted", stats.Deleted, "skipped", stats.Skipped, "errors", stats.Errors)
	}
	g.Metrics.AddWarmCacheReclaimed(stats.Deleted)
	for i := 0; i < stats.Errors; i++ {
		g.Metrics.RecordReconcileError(metrics.ControllerWarmCacheGC)
	}
	if g.OnPass != nil {
		g.OnPass(stats)
	}
}

// RunOnce performs exactly one GC pass: list, evaluate eligibility, delete.
// Exported so tests and a manual/debug invocation can trigger a pass without
// waiting for the ticker.
func (g *WarmCacheGC) RunOnce(ctx context.Context) (GCStats, error) {
	start := g.now()
	var list v1alpha1.SandboxEnvironmentList
	if err := g.Reader.List(ctx, &list, client.InNamespace(g.Namespace)); err != nil {
		return GCStats{}, fmt.Errorf("listing environments: %w", err)
	}

	stats := GCStats{Scanned: len(list.Items)}
	now := g.now()
	classes := make(map[string]*v1alpha1.SandboxClass) // resolved once per class per pass

	for i := range list.Items {
		env := &list.Items[i]
		class := g.resolveClass(ctx, env, classes)
		if class == nil {
			continue // unresolvable class: not eligible (and can't be)
		}
		if !g.eligible(env, class, now) {
			continue
		}
		stats.Eligible++
		if err := g.reclaim(ctx, env, class, now, &stats); err != nil {
			stats.Errors++
			ctrl.LoggerFrom(ctx).V(1).Info("warm-cache reclaim failed", LogKeyEnvironment, env.Name, LogKeyNamespace, env.Namespace, "error", err.Error())
		}
	}
	stats.Duration = g.now().Sub(start)
	return stats, nil
}

// resolveClass looks up env's class, caching the result within one pass (a
// Get per distinct class, not per environment). nil means the class is
// unreadable -- the environment is not eligible this pass.
func (g *WarmCacheGC) resolveClass(ctx context.Context, env *v1alpha1.SandboxEnvironment, classes map[string]*v1alpha1.SandboxClass) *v1alpha1.SandboxClass {
	if c := classes[env.Spec.ClassRef.Name]; c != nil {
		return c
	}
	var c v1alpha1.SandboxClass
	if err := g.Reader.Get(ctx, client.ObjectKey{Name: env.Spec.ClassRef.Name}, &c); err != nil {
		ctrl.LoggerFrom(ctx).V(1).Info("warm-cache GC: class unresolvable", "class", env.Spec.ClassRef.Name, "error", err.Error())
		return nil
	}
	classes[env.Spec.ClassRef.Name] = &c
	return &c
}

// eligible reports whether env's workspace PVC is a reclamation candidate
// right now. Every condition is independently required -- see WarmCacheGC's
// doc comment.
func (g *WarmCacheGC) eligible(env *v1alpha1.SandboxEnvironment, class *v1alpha1.SandboxClass, now time.Time) bool {
	if env.Status.Phase != v1alpha1.PhaseWaiting {
		return false
	}
	if !env.DeletionTimestamp.IsZero() {
		return false
	}
	// A complete, verified snapshot must genuinely exist in S3. Seq >=
	// FreezeCount-1 is the same scoping observeSnapshot (freeze.go) uses:
	// status.snapshot is non-nil from the PREVIOUS freeze the moment a new
	// one starts, and only a snapshot for a freeze that has actually
	// completed (or is the current one) counts. An in-flight or failed
	// snapshotAttempt means the snapshot may not have landed -- never
	// delete the only copy of the agent's context on a maybe.
	s := env.Status.Snapshot
	if s == nil || s.TakenAt == nil || s.Seq < env.Status.FreezeCount-1 {
		return false
	}
	if a := env.Status.SnapshotAttempt; a != nil && a.Phase != v1alpha1.SnapshotAttemptSucceeded {
		return false
	}

	if class.Spec.Storage.Backend.Type != v1alpha1.StorageBackendTypeS3 {
		return false // no warm cache to reclaim on a pvc-backed class
	}

	ttl := parseWarmCacheTTL(class.Spec.Storage.WarmCacheTTL)
	if ttl <= 0 {
		return false // "0s" or a zero duration disables GC for this class
	}
	return now.Sub(s.TakenAt.Time) >= ttl
}

// parseWarmCacheTTL parses a class's warmCacheTTL, applying
// defaultWarmCacheTTL on empty or unparseable values. A zero or negative
// result means "GC disabled for this class" -- the caller must not delete.
func parseWarmCacheTTL(s string) time.Duration {
	if s == "" {
		return defaultWarmCacheTTL
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return defaultWarmCacheTTL
	}
	return d
}

// reclaim deletes one eligible environment's workspace PVC, guarded against
// every race: ownership, already-terminating, and a live re-Get of the
// environment immediately before the delete re-checking eligibility
// (closing the "woke between List and Delete" window). The delete carries a
// UID precondition so a PVC recreated since the Get can never be hit.
func (g *WarmCacheGC) reclaim(ctx context.Context, env *v1alpha1.SandboxEnvironment, class *v1alpha1.SandboxClass, now time.Time, stats *GCStats) error {
	names := render.ChildNames(env.Name)
	var pvc corev1.PersistentVolumeClaim
	if err := g.Reader.Get(ctx, client.ObjectKey{Namespace: env.Namespace, Name: names.PVC}, &pvc); err != nil {
		if apierrors.IsNotFound(err) {
			return nil // already gone; nothing to reclaim
		}
		return err
	}
	if !ownedByEnv(&pvc, env) {
		return nil // a foreign PVC squatting the name is not ours to delete
	}
	if !pvc.DeletionTimestamp.IsZero() {
		return nil // already terminating
	}

	// Live re-Get the environment: it may have woken (or been deleted, or
	// started a new freeze) since the List. Re-check the exact eligibility
	// conditions that make deletion safe.
	fresh := &v1alpha1.SandboxEnvironment{}
	if err := g.Reader.Get(ctx, client.ObjectKey{Namespace: env.Namespace, Name: env.Name}, fresh); err != nil {
		if apierrors.IsNotFound(err) {
			return nil // environment gone; its PVC is being reclaimed by deletion anyway
		}
		return err
	}
	if fresh.UID != env.UID {
		return nil // replaced by a different object
	}
	if !g.eligible(fresh, class, now) {
		return nil // woke, or the snapshot situation changed -- leave it alone
	}

	uid := pvc.UID
	if err := g.Client.Delete(ctx, &pvc, client.Preconditions{UID: &uid}); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	stats.Deleted++
	stats.Reclaimed = append(stats.Reclaimed, ReclaimedPVC{
		Namespace: env.Namespace, Name: names.PVC, EnvName: env.Name,
		Age: now.Sub(fresh.Status.Snapshot.TakenAt.Time),
	})
	return nil
}
