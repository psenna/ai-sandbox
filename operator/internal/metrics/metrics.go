package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// namespace/subsystem give every series the sandbox_operator_ prefix, via
// prometheus.BuildFQName.
const (
	namespace = "sandbox"
	subsystem = "operator"
)

// Controller* is the label allowlist for reconcile_errors_total{controller}
// -- one member per controller/Runnable that can record a reconcile/pass
// error.
const (
	ControllerSandboxEnvironment = "sandboxenvironment"
	ControllerSlotScheduler      = "slotscheduler"
	ControllerWarmCacheGC        = "warmcachegc"
	ControllerCNIProbe           = "cniprobe"
	ControllerRetentionGC        = "retentiongc"
)

var controllerAllowlist = []string{
	ControllerSandboxEnvironment, ControllerSlotScheduler, ControllerWarmCacheGC,
	ControllerCNIProbe, ControllerRetentionGC,
}

// Result* is the label allowlist shared by probe_evaluations_total{result}
// and archives_total{result}. Not every member applies to every metric --
// probe_evaluations_total never uses ResultSucceeded/ResultFailed, and
// archives_total never uses ResultSatisfied/ResultPending/ResultError -- but
// sharing the constants keeps "skipped" spelled once.
const (
	ResultSatisfied = "satisfied"
	ResultPending   = "pending"
	ResultError     = "error"
	ResultSkipped   = "skipped"
	ResultSucceeded = "succeeded"
	ResultFailed    = "failed"
)

var probeResultAllowlist = []string{ResultSatisfied, ResultPending, ResultError, ResultSkipped}
var archiveResultAllowlist = []string{ResultSucceeded, ResultFailed, ResultSkipped}

// Sweep* is the label allowlist for retention_deleted_total{sweep}.
const (
	SweepRetention = "retention"
	SweepOrphan    = "orphan"
)

var sweepAllowlist = []string{SweepRetention, SweepOrphan}

// WakeSource* is the label allowlist for wake_duration_seconds{source}.
// Mirrors RestoredRootStatus.Source's CRD enum (Warm/Cold), lower-cased for
// metric-label convention, plus "unknown" for a wake whose workspace root
// outcome could not be found.
const (
	WakeSourceWarm    = "warm"
	WakeSourceCold    = "cold"
	WakeSourceUnknown = "unknown"
)

var wakeSourceAllowlist = []string{WakeSourceWarm, WakeSourceCold, WakeSourceUnknown}

// waitTypeAllowlist mirrors v1alpha1's WaitForStatus.Type enum exactly (the
// allowlist IS the CRD enum -- see that type's own doc comment).
var waitTypeAllowlist = []string{
	v1alpha1.WaitTypeGitProxyCheck, v1alpha1.WaitTypeHTTPGet,
	v1alpha1.WaitTypeS3ObjectExists, v1alpha1.WaitTypeNotBefore,
}

// otherLabel is the sanitize fallback for every allowlisted label in this
// package.
const otherLabel = "other"

// sanitize returns v if it appears in allow, else otherLabel. Mirrors
// internal/lifecycle/conditions.go's SanitizeReason: every label value that
// could conceivably carry unexpected data is routed through this before it
// reaches a metric, so an unrecognized value collapses into one bounded
// "other" series rather than creating an unbounded new one.
func sanitize(v string, allow []string) string {
	for _, a := range allow {
		if v == a {
			return v
		}
	}
	return otherLabel
}

// queueWaitBuckets/freezeWakeBuckets are the fixed bucket boundaries the
// metric catalogue (issue #33's design record) specifies, in seconds.
var (
	queueWaitBuckets  = []float64{1, 2.5, 5, 10, 30, 60, 120, 300, 600, 1800}
	freezeWakeBuckets = []float64{0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300}
)

// Collectors holds every Prometheus series this operator emits (#33). Every
// field is unexported -- callers only ever go through the typed recording
// methods below, never the raw prometheus.Collector, so a label allowlist
// can never be bypassed by a call site reaching in directly.
type Collectors struct {
	environments *prometheus.GaugeVec
	slotCapacity prometheus.Gauge
	slotsUsed    prometheus.Gauge
	queueDepth   prometheus.Gauge

	queueWaitSeconds  prometheus.Histogram
	freezeDuration    prometheus.Histogram
	wakeDuration      *prometheus.HistogramVec
	snapshotSizeBytes prometheus.Histogram

	probeEvaluations *prometheus.CounterVec
	archives         *prometheus.CounterVec
	reconcileErrors  *prometheus.CounterVec
	warmCacheReclaim prometheus.Counter
	retentionDeleted *prometheus.CounterVec
}

// New constructs a Collectors with every series built, registered nowhere.
// Call MustRegister to attach it to a prometheus.Registerer -- see doc.go's
// registration-model section for why construction and registration are
// separate calls.
func New() *Collectors {
	fqName := func(name string) string { return prometheus.BuildFQName(namespace, subsystem, name) }

	return &Collectors{
		environments: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: fqName("environments"),
			Help: "Number of SandboxEnvironments currently observed in each lifecycle phase, within the operator's watch scope.",
		}, []string{"phase"}),
		slotCapacity: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: fqName("slot_capacity"),
			Help: "Configured maximum number of sandbox scheduling slots.",
		}),
		slotsUsed: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: fqName("slots_used"),
			Help: "Number of scheduling slots currently occupied (scheduler.Occupies).",
		}),
		queueDepth: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: fqName("queue_depth"),
			Help: "Number of environments queued for a scheduling slot (scheduler.IsCandidate).",
		}),
		queueWaitSeconds: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    fqName("queue_wait_seconds"),
			Help:    "Time an environment spent queued before its scheduling slot was granted.",
			Buckets: queueWaitBuckets,
		}),
		freezeDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    fqName("freeze_duration_seconds"),
			Help:    "Time to complete a freeze, from the freeze hook to the published snapshot (status.snapshot.durationMillis).",
			Buckets: freezeWakeBuckets,
		}),
		wakeDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    fqName("wake_duration_seconds"),
			Help:    "Time to complete a wake restore (status.restoreAttempt.durationMillis), by warm-cache hit or cold download.",
			Buckets: freezeWakeBuckets,
		}, []string{"source"}),
		snapshotSizeBytes: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    fqName("snapshot_size_bytes"),
			Help:    "Uncompressed size of a published freeze snapshot (status.snapshot.sizeBytes).",
			Buckets: prometheus.ExponentialBuckets(1<<20, 4, 8), // 1Mi .. 16Gi
		}),
		probeEvaluations: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: fqName("probe_evaluations_total"),
			Help: "Wait-probe evaluations by wait type and outcome.",
		}, []string{"type", "result"}),
		archives: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: fqName("archives_total"),
			Help: "Terminal archive outcomes observed by the operator.",
		}, []string{"result"}),
		reconcileErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: fqName("reconcile_errors_total"),
			Help: "Errors returned by a reconcile or by one control-loop pass, by controller.",
		}, []string{"controller"}),
		warmCacheReclaim: prometheus.NewCounter(prometheus.CounterOpts{
			Name: fqName("warm_cache_reclaimed_total"),
			Help: "Workspace PVCs reclaimed by the warm-cache TTL GC.",
		}),
		retentionDeleted: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: fqName("retention_deleted_total"),
			Help: "Storage roots deleted by retention GC, by sweep.",
		}, []string{"sweep"}),
	}
}

// MustRegister registers every series in c into r, panicking on a duplicate
// or invalid registration -- the same fail-fast contract
// prometheus.Registerer.MustRegister itself documents. Called once, at
// process startup (Default's init below, or a test's own private registry).
func (c *Collectors) MustRegister(r prometheus.Registerer) {
	r.MustRegister(
		c.environments, c.slotCapacity, c.slotsUsed, c.queueDepth,
		c.queueWaitSeconds, c.freezeDuration, c.wakeDuration, c.snapshotSizeBytes,
		c.probeEvaluations, c.archives, c.reconcileErrors, c.warmCacheReclaim, c.retentionDeleted,
	)
}

// Default is the process-wide Collectors instance, registered into
// controller-runtime's own metrics.Registry at init time -- see doc.go. Every
// controller/Runnable wired by internal/operator/controllers.go records into
// this instance. A test asserting an absolute value must inject its own
// metrics.New() instead: Default is shared across the whole test binary.
var Default = New()

func init() {
	Default.MustRegister(ctrlmetrics.Registry)
}

// SetEnvironmentsByPhase sets the environments{phase} gauge for EVERY phase
// in v1alpha1.AllPhases, using 0 for a phase absent from counts. Setting
// every phase on every call (rather than only the phases present this pass)
// is what makes a phase that empties drop to 0 rather than sticking at its
// last observed value -- a GaugeVec left alone never un-sets a stale label
// combination on its own.
func (c *Collectors) SetEnvironmentsByPhase(counts map[v1alpha1.Phase]int) {
	if c == nil {
		return
	}
	for _, p := range v1alpha1.AllPhases {
		c.environments.WithLabelValues(string(p)).Set(float64(counts[p]))
	}
}

// SetSlots sets the slot_capacity/slots_used/queue_depth gauges from one
// MetricsCollector pass.
func (c *Collectors) SetSlots(capacity, used, queueDepth int) {
	if c == nil {
		return
	}
	c.slotCapacity.Set(float64(capacity))
	c.slotsUsed.Set(float64(used))
	c.queueDepth.Set(float64(queueDepth))
}

// ObserveQueueWait records how long one environment waited in queue before
// its scheduling slot was granted.
func (c *Collectors) ObserveQueueWait(d time.Duration) {
	if c == nil {
		return
	}
	c.queueWaitSeconds.Observe(d.Seconds())
}

// RecordFreeze records one completed freeze's duration and published
// snapshot size. Callers skip this entirely when status.snapshot.durationMillis
// is 0 (an unpopulated/never-set duration) -- see events.go's observeTransition.
func (c *Collectors) RecordFreeze(d time.Duration, sizeBytes int64) {
	if c == nil {
		return
	}
	c.freezeDuration.Observe(d.Seconds())
	c.snapshotSizeBytes.Observe(float64(sizeBytes))
}

// RecordWake records one completed wake restore's duration, labeled by
// whether the workspace root was restored warm (from the retained PVC) or
// cold (downloaded). source is sanitized against WakeSource* -- an
// unrecognized value (should not happen; the CRD enum is Warm/Cold) records
// as "other" rather than an unbounded new series.
func (c *Collectors) RecordWake(source string, d time.Duration) {
	if c == nil {
		return
	}
	c.wakeDuration.WithLabelValues(sanitize(source, wakeSourceAllowlist)).Observe(d.Seconds())
}

// RecordProbeEvaluation records one wait-probe evaluation. waitType is
// sanitized against v1alpha1's WaitForStatus.Type enum; result is sanitized
// against Result{Satisfied,Pending,Error,Skipped}.
func (c *Collectors) RecordProbeEvaluation(waitType, result string) {
	if c == nil {
		return
	}
	c.probeEvaluations.WithLabelValues(sanitize(waitType, waitTypeAllowlist), sanitize(result, probeResultAllowlist)).Inc()
}

// RecordArchive records one terminal archive outcome. result is sanitized
// against Result{Succeeded,Failed,Skipped}.
func (c *Collectors) RecordArchive(result string) {
	if c == nil {
		return
	}
	c.archives.WithLabelValues(sanitize(result, archiveResultAllowlist)).Inc()
}

// RecordReconcileError records one reconcile/pass error for controller.
// controller is sanitized against the Controller* constants.
func (c *Collectors) RecordReconcileError(controller string) {
	if c == nil {
		return
	}
	c.reconcileErrors.WithLabelValues(sanitize(controller, controllerAllowlist)).Inc()
}

// AddWarmCacheReclaimed adds n (a GCStats.Deleted count) to the warm-cache
// reclamation counter.
func (c *Collectors) AddWarmCacheReclaimed(n int) {
	if c == nil {
		return
	}
	c.warmCacheReclaim.Add(float64(n))
}

// AddRetentionDeleted adds n storage roots deleted by sweep ("retention" or
// "orphan", sanitized against Sweep*).
func (c *Collectors) AddRetentionDeleted(sweep string, n int) {
	if c == nil {
		return
	}
	c.retentionDeleted.WithLabelValues(sanitize(sweep, sweepAllowlist)).Add(float64(n))
}
