package metrics

import (
	"strings"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// newRegistered returns a fresh Collectors registered into a private
// registry, so absolute-value assertions never see counters/gauges any
// other test (or Default's own registration) has already touched.
func newRegistered(t *testing.T) (*Collectors, *prometheus.Registry) {
	t.Helper()
	c := New()
	reg := prometheus.NewRegistry()
	c.MustRegister(reg)
	return c, reg
}

// TestCatalogue table-tests every declared series: its fully-qualified name
// carries the sandbox_operator_ prefix, its HELP text is non-empty, and its
// type matches the #33 design record's metric table. A CounterVec/GaugeVec/
// HistogramVec with no observed label combination yet emits NO metric family
// from Gather -- an empty Vec is invisible, not zero -- so every recording
// method is exercised once first, proving each metric is not just
// constructed but actually observable end to end.
func TestCatalogue(t *testing.T) {
	c, reg := newRegistered(t)
	c.SetEnvironmentsByPhase(nil)
	c.SetSlots(4, 1, 1)
	c.ObserveQueueWait(time.Second)
	c.RecordFreeze(time.Second, 1024)
	c.RecordWake(WakeSourceWarm, time.Second)
	c.RecordProbeEvaluation(v1alpha1.WaitTypeHTTPGet, ResultSatisfied)
	c.RecordArchive(ResultSucceeded)
	c.RecordReconcileError(ControllerSandboxEnvironment)
	c.AddWarmCacheReclaimed(1)
	c.AddRetentionDeleted(SweepRetention, 1)

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	byName := make(map[string]*dto.MetricFamily, len(families))
	for _, f := range families {
		byName[f.GetName()] = f
	}

	cases := []struct {
		name string
		typ  dto.MetricType
	}{
		{"sandbox_operator_environments", dto.MetricType_GAUGE},
		{"sandbox_operator_slot_capacity", dto.MetricType_GAUGE},
		{"sandbox_operator_slots_used", dto.MetricType_GAUGE},
		{"sandbox_operator_queue_depth", dto.MetricType_GAUGE},
		{"sandbox_operator_queue_wait_seconds", dto.MetricType_HISTOGRAM},
		{"sandbox_operator_freeze_duration_seconds", dto.MetricType_HISTOGRAM},
		{"sandbox_operator_wake_duration_seconds", dto.MetricType_HISTOGRAM},
		{"sandbox_operator_snapshot_size_bytes", dto.MetricType_HISTOGRAM},
		{"sandbox_operator_probe_evaluations_total", dto.MetricType_COUNTER},
		{"sandbox_operator_archives_total", dto.MetricType_COUNTER},
		{"sandbox_operator_reconcile_errors_total", dto.MetricType_COUNTER},
		{"sandbox_operator_warm_cache_reclaimed_total", dto.MetricType_COUNTER},
		{"sandbox_operator_retention_deleted_total", dto.MetricType_COUNTER},
	}
	if len(cases) != len(families) {
		t.Errorf("registered %d families, catalogue table has %d -- keep them in sync", len(families), len(cases))
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, ok := byName[tc.name]
			if !ok {
				t.Fatalf("metric %s not registered", tc.name)
			}
			if f.GetHelp() == "" {
				t.Errorf("metric %s has empty HELP text", tc.name)
			}
			if f.GetType() != tc.typ {
				t.Errorf("metric %s type = %v, want %v", tc.name, f.GetType(), tc.typ)
			}
		})
	}
}

// TestDefault_RegisteredOnControllerRuntimeRegistry asserts the package-level
// Default is registered into controller-runtime's own metrics.Registry (via
// init), which is what puts these series on the manager's existing /metrics
// listener with no new server.
func TestDefault_RegisteredOnControllerRuntimeRegistry(t *testing.T) {
	families, err := ctrlmetrics.Registry.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() == "sandbox_operator_slot_capacity" {
			return
		}
	}
	t.Fatal("sandbox_operator_slot_capacity not found on ctrlmetrics.Registry; Default.MustRegister did not run against it")
}

// TestHistogramBuckets checks the exact bucket boundaries the design record
// specifies for the three histograms with fixed (non-exponential) buckets.
func TestHistogramBuckets(t *testing.T) {
	c, _ := newRegistered(t)

	cases := []struct {
		name    string
		collect prometheus.Collector
		want    []float64
	}{
		{"queue_wait_seconds", c.queueWaitSeconds, queueWaitBuckets},
		{"freeze_duration_seconds", c.freezeDuration, freezeWakeBuckets},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := &dto.Metric{}
			if err := tc.collect.(prometheus.Metric).Write(m); err != nil {
				t.Fatalf("Write: %v", err)
			}
			buckets := m.GetHistogram().GetBucket()
			if len(buckets) != len(tc.want) {
				t.Fatalf("got %d buckets, want %d", len(buckets), len(tc.want))
			}
			for i, b := range buckets {
				if b.GetUpperBound() != tc.want[i] {
					t.Errorf("bucket[%d] = %v, want %v", i, b.GetUpperBound(), tc.want[i])
				}
			}
		})
	}

	// wake_duration_seconds is a Vec; observe once to materialize a child
	// and inspect its buckets the same way.
	c.RecordWake(WakeSourceWarm, time.Second)
	wakeChild, err := c.wakeDuration.GetMetricWithLabelValues(WakeSourceWarm)
	if err != nil {
		t.Fatalf("GetMetricWithLabelValues: %v", err)
	}
	m := &dto.Metric{}
	if err := wakeChild.(prometheus.Metric).Write(m); err != nil {
		t.Fatalf("Write: %v", err)
	}
	buckets := m.GetHistogram().GetBucket()
	if len(buckets) != len(freezeWakeBuckets) {
		t.Fatalf("wake_duration_seconds: got %d buckets, want %d", len(buckets), len(freezeWakeBuckets))
	}
	for i, b := range buckets {
		if b.GetUpperBound() != freezeWakeBuckets[i] {
			t.Errorf("wake_duration_seconds bucket[%d] = %v, want %v", i, b.GetUpperBound(), freezeWakeBuckets[i])
		}
	}
}

// TestSanitize checks the allowlist behavior every labeled metric relies on.
func TestSanitize(t *testing.T) {
	allow := []string{"a", "b"}
	if got := sanitize("a", allow); got != "a" {
		t.Errorf("sanitize(a) = %q, want %q", got, "a")
	}
	if got := sanitize("nonsense; DROP TABLE", allow); got != otherLabel {
		t.Errorf("sanitize(unknown) = %q, want %q", got, otherLabel)
	}
	if got := sanitize("", allow); got != otherLabel {
		t.Errorf("sanitize(empty) = %q, want %q", got, otherLabel)
	}
}

// TestRecordProbeEvaluation_LabelSanitization checks that an out-of-allowlist
// waitType/result collapses to "other" rather than creating a new series.
func TestRecordProbeEvaluation_LabelSanitization(t *testing.T) {
	c, _ := newRegistered(t)
	c.RecordProbeEvaluation("NotARealWaitType", "NotARealResult")
	got := testutil.ToFloat64(c.probeEvaluations.WithLabelValues(otherLabel, otherLabel))
	if got != 1 {
		t.Errorf("probe_evaluations_total{type=other,result=other} = %v, want 1", got)
	}
}

// TestSetEnvironmentsByPhase_ZeroesEmptiedPhases asserts every phase in
// v1alpha1.AllPhases is set on every call -- including to 0 -- so a phase
// that empties between passes drops to 0 rather than sticking at its last
// observed value.
func TestSetEnvironmentsByPhase_ZeroesEmptiedPhases(t *testing.T) {
	c, _ := newRegistered(t)

	c.SetEnvironmentsByPhase(map[v1alpha1.Phase]int{v1alpha1.PhaseRunning: 3})
	if got := testutil.ToFloat64(c.environments.WithLabelValues(string(v1alpha1.PhaseRunning))); got != 3 {
		t.Fatalf("environments{phase=Running} = %v, want 3", got)
	}
	if got := testutil.ToFloat64(c.environments.WithLabelValues(string(v1alpha1.PhasePending))); got != 0 {
		t.Fatalf("environments{phase=Pending} = %v, want 0", got)
	}

	// Running empties, Pending gains one: Running must drop to 0, not stay at 3.
	c.SetEnvironmentsByPhase(map[v1alpha1.Phase]int{v1alpha1.PhasePending: 1})
	if got := testutil.ToFloat64(c.environments.WithLabelValues(string(v1alpha1.PhaseRunning))); got != 0 {
		t.Errorf("environments{phase=Running} after emptying = %v, want 0", got)
	}
	if got := testutil.ToFloat64(c.environments.WithLabelValues(string(v1alpha1.PhasePending))); got != 1 {
		t.Errorf("environments{phase=Pending} = %v, want 1", got)
	}
}

// TestSetEnvironmentsByPhase_CoversEveryPhase asserts every member of
// v1alpha1.AllPhases gets its own gauge child after one call -- the
// "phase-label-coverage" property: a phase silently missing from AllPhases
// would never be zeroed and this test would catch that by checking the
// gathered series count.
func TestSetEnvironmentsByPhase_CoversEveryPhase(t *testing.T) {
	c, reg := newRegistered(t)
	c.SetEnvironmentsByPhase(nil)

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	var got int
	for _, f := range families {
		if f.GetName() == "sandbox_operator_environments" {
			got = len(f.GetMetric())
		}
	}
	if got != len(v1alpha1.AllPhases) {
		t.Errorf("environments has %d label combinations, want %d (one per v1alpha1.AllPhases)", got, len(v1alpha1.AllPhases))
	}
}

// TestNilReceiver_NoOp asserts every recording method on a nil *Collectors
// is a no-op, never a panic -- the same "left-nil-is-silently-inert"
// contract as Reconciler.Recorder/Reconciler.Probes.
func TestNilReceiver_NoOp(t *testing.T) {
	var c *Collectors
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("nil-receiver call panicked: %v", r)
		}
	}()
	c.SetEnvironmentsByPhase(map[v1alpha1.Phase]int{v1alpha1.PhaseRunning: 1})
	c.SetSlots(4, 2, 1)
	c.ObserveQueueWait(time.Second)
	c.RecordFreeze(time.Second, 1024)
	c.RecordWake(WakeSourceWarm, time.Second)
	c.RecordProbeEvaluation(v1alpha1.WaitTypeHTTPGet, ResultSatisfied)
	c.RecordArchive(ResultSucceeded)
	c.RecordReconcileError(ControllerSandboxEnvironment)
	c.AddWarmCacheReclaimed(1)
	c.AddRetentionDeleted(SweepRetention, 1)
}

// TestFQNamePrefix double-checks BuildFQName's output shape directly,
// independent of TestCatalogue's Gather-based check.
func TestFQNamePrefix(t *testing.T) {
	got := prometheus.BuildFQName(namespace, subsystem, "environments")
	if !strings.HasPrefix(got, "sandbox_operator_") {
		t.Errorf("BuildFQName = %q, want sandbox_operator_ prefix", got)
	}
}
