package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/go-logr/logr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
	"github.com/psenna/ai-sandbox/operator/internal/metrics"
	"github.com/psenna/ai-sandbox/operator/internal/storage"
)

// RetentionGC is the terminal-archive TTL reclamation loop, plus orphan
// cleanup (#32). It is a manager.Runnable, NOT a reconciler, for the same
// reason WarmCacheGC isn't one: "delete storage older than a TTL" and
// "delete storage nothing references any more" are both wall-clock-driven,
// cross-object policy decisions with no natural per-object reconcile
// trigger.
//
// Two independent sweeps run every pass, per S3-backed SandboxClass:
//
//   - Retention: for every LIVE environment referencing that class whose
//     status.archive is set and status.archive.finishedAt is older than TTL,
//     delete the environment's entire storage.Layout.Root() (snapshots AND
//     the archive) -- TTL==0 disables this sweep entirely.
//   - Orphan cleanup: List the class's cluster-scoped key prefix and delete
//     any Root() whose EnvUID does not belong to a currently-live
//     environment (of ANY class, ANY phase) -- restricted to Namespace when
//     one is set, since that is how far "currently-live" can be known --
//     runs regardless of TTL. This
//     is what makes delete+recreate-with-the-same-name safe: the recreated
//     environment gets a fresh UID and therefore a disjoint Root(), and the
//     old UID's Root() is picked up here on the next pass rather than lingering
//     forever.
type RetentionGC struct {
	// Client issues the backend/Secret reads that build each class's
	// storage.Backend.
	Client client.Client
	// Reader is a LIVE, UNCACHED reader (mgr.GetAPIReader()), used for both
	// the environment and the class List -- mirrors WarmCacheGC's own
	// reasoning: eligibility (here, "which UIDs are live") must never be
	// computed from a stale cache.
	Reader client.Reader

	// SecretNamespace is the namespace holding class-referenced Secrets
	// (mirrors Config.ClassSecretNamespace / Reconciler.ClassSecretNamespace).
	SecretNamespace string
	// ClusterID scopes every storage.Layout this loop builds, exactly like
	// Reconciler.ClusterID.
	ClusterID string

	// TTL is how long a terminal archive is retained after
	// status.archive.finishedAt before its storage is deleted. 0 disables
	// the retention sweep; orphan cleanup still runs regardless.
	TTL time.Duration
	// DryRun logs what would be deleted without deleting anything, for both
	// sweeps.
	DryRun bool

	Interval  time.Duration
	Namespace string // "" = all namespaces; mirrors Config.WatchNamespace
	Clock     func() time.Time

	// BackendFor builds the storage.Backend for one S3-backed class.
	// Defaults to buildS3BackendForClass(ctx, g.Client, g.SecretNamespace,
	// class) when nil; injectable so tests can supply a fake backend
	// without dialing a real S3 endpoint -- mirrors Reconciler.Observe's own
	// injectable-field pattern.
	BackendFor func(ctx context.Context, class *v1alpha1.SandboxClass) (storage.Backend, error)

	// OnPass is the extension point #33 wires Prometheus metrics into,
	// added for symmetry with SlotScheduler.OnPass/WarmCacheGC.OnPass. Nil
	// is a no-op.
	OnPass func(RetentionStats)
	// Metrics records deletion counts and per-pass reconcile errors (#33).
	// Nil-guarded, matching Reconciler.Metrics.
	Metrics *metrics.Collectors
}

func (g *RetentionGC) backendFor(ctx context.Context, class *v1alpha1.SandboxClass) (storage.Backend, error) {
	if g.BackendFor != nil {
		return g.BackendFor(ctx, class)
	}
	return buildS3BackendForClass(ctx, g.Client, g.SecretNamespace, class)
}

// RetentionStats summarizes one GC pass, mirroring WarmCacheGC's GCStats
// shape/naming for consistency across this package's two GC loops.
type RetentionStats struct {
	Scanned        int
	Deleted        int
	WouldDelete    int
	OrphansDeleted int
	Errors         int
	Duration       time.Duration
}

var (
	_ manager.Runnable               = (*RetentionGC)(nil)
	_ manager.LeaderElectionRunnable = (*RetentionGC)(nil)
)

// NeedLeaderElection ensures exactly one instance of this loop ever runs
// cluster-wide, exactly like WarmCacheGC: two GCs deleting concurrently
// would race harmlessly (DeletePrefix is idempotent), but there is no
// benefit to running it more than once.
func (g *RetentionGC) NeedLeaderElection() bool { return true }

func (g *RetentionGC) now() time.Time {
	if g.Clock != nil {
		return g.Clock()
	}
	return time.Now()
}

// Start runs one GC pass immediately, then one per Interval, until ctx is
// cancelled. A failed pass is logged, never returned -- mirrors
// WarmCacheGC.Start exactly.
func (g *RetentionGC) Start(ctx context.Context) error {
	log := ctrl.LoggerFrom(ctx).WithName("retentiongc")
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

func (g *RetentionGC) runAndLog(ctx context.Context, log logr.Logger) {
	stats, err := g.RunOnce(ctx)
	if err != nil {
		log.Error(err, "retention gc pass failed")
		g.Metrics.RecordReconcileError(metrics.ControllerRetentionGC)
		return
	}
	if stats.Deleted > 0 || stats.WouldDelete > 0 || stats.OrphansDeleted > 0 || stats.Errors > 0 {
		log.V(1).Info("retention gc pass", "scanned", stats.Scanned, "deleted", stats.Deleted,
			"wouldDelete", stats.WouldDelete, "orphansDeleted", stats.OrphansDeleted, "errors", stats.Errors)
	}
	// DryRun's WouldDelete is deliberately NOT counted here: #13 is what was
	// actually deleted, and a dry-run pass never calls DeletePrefix.
	g.Metrics.AddRetentionDeleted(metrics.SweepRetention, stats.Deleted)
	g.Metrics.AddRetentionDeleted(metrics.SweepOrphan, stats.OrphansDeleted)
	for i := 0; i < stats.Errors; i++ {
		g.Metrics.RecordReconcileError(metrics.ControllerRetentionGC)
	}
	if g.OnPass != nil {
		g.OnPass(stats)
	}
}

// RunOnce performs exactly one GC pass across every S3-backed SandboxClass.
// Exported so tests and a manual/debug invocation can trigger a pass without
// waiting for the ticker.
func (g *RetentionGC) RunOnce(ctx context.Context) (RetentionStats, error) {
	start := g.now()
	log := ctrl.LoggerFrom(ctx).WithName("retentiongc")

	var envList v1alpha1.SandboxEnvironmentList
	if err := g.Reader.List(ctx, &envList, client.InNamespace(g.Namespace)); err != nil {
		return RetentionStats{}, fmt.Errorf("listing environments: %w", err)
	}
	stats := RetentionStats{Scanned: len(envList.Items)}
	now := g.now()

	// liveUIDs spans EVERY live environment, regardless of class or phase:
	// a Root() belongs to a live environment the instant that environment
	// exists, whether or not it has ever archived (or even started).
	liveUIDs := make(map[string]struct{}, len(envList.Items))
	byClass := make(map[string][]*v1alpha1.SandboxEnvironment)
	for i := range envList.Items {
		e := &envList.Items[i]
		liveUIDs[string(e.UID)] = struct{}{}
		byClass[e.Spec.ClassRef.Name] = append(byClass[e.Spec.ClassRef.Name], e)
	}

	var classList v1alpha1.SandboxClassList
	if err := g.Reader.List(ctx, &classList); err != nil {
		return RetentionStats{}, fmt.Errorf("listing classes: %w", err)
	}

	for ci := range classList.Items {
		class := &classList.Items[ci]
		b := class.Spec.Storage.Backend
		if b.Type != v1alpha1.StorageBackendTypeS3 || b.S3 == nil {
			continue // no warm/archived storage to reclaim on a pvc-backed class
		}
		backend, err := g.backendFor(ctx, class)
		if err != nil {
			stats.Errors++
			log.V(1).Info("backend unresolvable", "class", class.Name, "error", err.Error())
			continue
		}

		if g.TTL > 0 {
			g.sweepRetention(ctx, log, backend, class, byClass[class.Name], now, &stats)
		}
		g.sweepOrphans(ctx, log, backend, class, liveUIDs, &stats)
	}

	stats.Duration = g.now().Sub(start)
	return stats, nil
}

// sweepRetention deletes the entire storage.Layout.Root() (snapshots AND
// archive) of every environment in envs whose terminal archive is older
// than g.TTL. envs is already scoped to class -- the caller partitions by
// class.Name before calling this.
func (g *RetentionGC) sweepRetention(ctx context.Context, log logr.Logger, backend storage.Backend, class *v1alpha1.SandboxClass, envs []*v1alpha1.SandboxEnvironment, now time.Time, stats *RetentionStats) {
	for _, e := range envs {
		arc := e.Status.Archive
		if arc == nil || arc.FinishedAt == nil {
			continue
		}
		age := now.Sub(arc.FinishedAt.Time)
		if age < g.TTL {
			continue
		}
		layout, err := storage.NewLayout(class.Spec.Storage.Backend.S3.Prefix, storage.Identity{
			ClusterID: g.ClusterID, Namespace: e.Namespace, EnvName: e.Name, EnvUID: string(e.UID),
		})
		if err != nil {
			stats.Errors++
			log.V(1).Info("layout unbuildable", LogKeyNamespace, e.Namespace, LogKeyEnvironment, e.Name, "error", err.Error())
			continue
		}
		root := layout.Root()
		if g.DryRun {
			stats.WouldDelete++
			log.Info("retention gc dry-run: would delete archive", "uri", arc.URI, LogKeyNamespace, e.Namespace, LogKeyEnvironment, e.Name,
				"finishedAt", arc.FinishedAt.Time, "age", age)
			continue
		}
		n, err := backend.DeletePrefix(ctx, root)
		if err != nil {
			stats.Errors++
			log.V(1).Info("retention delete failed", LogKeyNamespace, e.Namespace, LogKeyEnvironment, e.Name, "root", root, "error", err.Error())
			continue
		}
		stats.Deleted++
		log.Info("retention gc: deleted archive", "uri", arc.URI, LogKeyNamespace, e.Namespace, LogKeyEnvironment, e.Name,
			"finishedAt", arc.FinishedAt.Time, "age", age, "objects", n)
	}
}

// sweepOrphans lists class's entire cluster-scoped key prefix and deletes
// every Root() whose EnvUID is not in liveUIDs. Runs regardless of TTL: an
// orphaned root (the environment that owned it is gone) has nothing left to
// wait for.
func (g *RetentionGC) sweepOrphans(ctx context.Context, log logr.Logger, backend storage.Backend, class *v1alpha1.SandboxClass, liveUIDs map[string]struct{}, stats *RetentionStats) {
	listPrefix := clusterPrefix(class.Spec.Storage.Backend.S3.Prefix, g.ClusterID)
	objs, err := backend.List(ctx, listPrefix)
	if err != nil {
		stats.Errors++
		log.V(1).Info("orphan list failed", "class", class.Name, "error", err.Error())
		return
	}

	seen := make(map[string]struct{})
	for _, o := range objs {
		root, ns, uid, ok := rootOf(listPrefix, o.Key)
		if !ok {
			continue
		}
		// liveUIDs is only as wide as the environment List that built it, and
		// that List is scoped to g.Namespace -- while listPrefix is
		// cluster-wide, spanning EVERY namespace's roots. A namespace-scoped
		// operator (Config.WatchNamespace set) would otherwise see every other
		// namespace's live environments as orphans and delete their storage.
		// Roots outside the watched namespace belong to whoever watches it.
		if g.Namespace != "" && ns != g.Namespace {
			continue
		}
		if _, dup := seen[root]; dup {
			continue // already handled (or skipped) this root from an earlier object
		}
		seen[root] = struct{}{}
		if _, live := liveUIDs[uid]; live {
			continue
		}
		if g.DryRun {
			stats.WouldDelete++
			log.Info("retention gc dry-run: would delete orphan", "root", root)
			continue
		}
		n, err := backend.DeletePrefix(ctx, root)
		if err != nil {
			stats.Errors++
			log.V(1).Info("orphan delete failed", "root", root, "error", err.Error())
			continue
		}
		stats.OrphansDeleted++
		log.Info("retention gc: deleted orphan", "root", root, "objects", n)
	}
}

// clusterPrefix is the key prefix scoping every environment root under one
// SandboxClass's storage prefix and this operator's ClusterID:
// "<s3Prefix>/<clusterID>/", with no leading "/" when s3Prefix is empty. The
// trailing "/" matters: without it, clusterID "a" would spuriously match a
// sibling cluster's "ab" prefix.
func clusterPrefix(s3Prefix, clusterID string) string {
	if s3Prefix == "" {
		return clusterID + "/"
	}
	return strings.TrimSuffix(s3Prefix, "/") + "/" + clusterID + "/"
}

// rootOf extracts the environment Root() key, namespace and EnvUID from one
// object key listed under listPrefix (itself clusterPrefix's output): the
// first three "/"-separated segments after listPrefix are
// namespace/envName/envUID, mirroring storage.Layout.Root()'s own
// "<prefix>/<clusterID>/<namespace>/<envName>/<envUID>" shape. Splitting on
// "/" (rather than prefix-matching the whole root) is what makes the orphan
// match exact: the uid is a whole segment, so two environments whose names
// share a prefix ("foo" and "foo-bar") can never be confused for one another.
// ok is false for a key that doesn't even have three segments after the
// prefix (should not happen for anything this package itself wrote, but a
// foreign or truncated key must never panic or wrongly match an empty uid).
func rootOf(listPrefix, key string) (root, namespace, uid string, ok bool) {
	rest := strings.TrimPrefix(key, listPrefix)
	if rest == key {
		return "", "", "", false
	}
	parts := strings.SplitN(rest, "/", 4)
	if len(parts) < 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", "", "", false
	}
	return listPrefix + strings.Join(parts[:3], "/"), parts[0], parts[2], true
}
