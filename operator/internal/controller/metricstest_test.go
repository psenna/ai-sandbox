package controller

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/psenna/ai-sandbox/operator/internal/metrics"
)

// newTestRegistry registers m into a fresh, private prometheus.Registry and
// returns it, so a test asserting an absolute metric value is never
// polluted by another test sharing the process-wide metrics.Default --
// every test in this file injects its own metrics.New() rather than reading
// Default, per #33's testing plan.
func newTestRegistry(t *testing.T, m *metrics.Collectors) *prometheus.Registry {
	t.Helper()
	reg := prometheus.NewRegistry()
	m.MustRegister(reg)
	return reg
}

// metricValue returns the value of the metric family named name whose
// label set matches labels exactly (nil/empty matches an unlabeled series),
// and whether a matching series was found at all -- a Vec metric with no
// observed label combination yet is invisible to Gather, not zero (see
// internal/metrics/metrics_test.go's own TestCatalogue comment), so "not
// found" and "found with value 0" are deliberately distinguishable here.
// A Histogram's "value" is its sample count (how many Observe calls
// landed), not a bucket boundary or sum.
func metricValue(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string) (float64, bool) {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			if !labelsMatch(m.GetLabel(), labels) {
				continue
			}
			switch {
			case m.Gauge != nil:
				return m.GetGauge().GetValue(), true
			case m.Counter != nil:
				return m.GetCounter().GetValue(), true
			case m.Histogram != nil:
				return float64(m.GetHistogram().GetSampleCount()), true
			}
		}
	}
	return 0, false
}

// mustMetricValue is metricValue, failing the test if no matching series
// was gathered at all.
func mustMetricValue(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string) float64 {
	t.Helper()
	v, ok := metricValue(t, reg, name, labels)
	if !ok {
		t.Fatalf("metric %s with labels %v: no matching series gathered", name, labels)
	}
	return v
}

func labelsMatch(got []*dto.LabelPair, want map[string]string) bool {
	if len(got) != len(want) {
		return false
	}
	for _, lp := range got {
		if want[lp.GetName()] != lp.GetValue() {
			return false
		}
	}
	return true
}
