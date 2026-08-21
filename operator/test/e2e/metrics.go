package e2e

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// OperatorPodName returns the name of the (single-replica, e2e overlay
// disables leader election so there is exactly one) operator Deployment's
// Pod, or an error if none is found.
func OperatorPodName(ctx context.Context, h *Harness) (string, error) {
	var pods corev1.PodList
	if err := h.Client.List(ctx, &pods, client.InNamespace(h.Cfg.OperatorNamespace), client.MatchingLabels{"control-plane": "controller-manager"}); err != nil {
		return "", fmt.Errorf("listing operator pods: %w", err)
	}
	if len(pods.Items) == 0 {
		return "", fmt.Errorf("no operator pod found in namespace %s with label control-plane=controller-manager", h.Cfg.OperatorNamespace)
	}
	return pods.Items[0].Name, nil
}

// ScrapeOperatorMetrics fetches the operator pod's own /metrics endpoint
// through the Kubernetes API server's pod-proxy subresource (Pods(ns).
// ProxyGet, PodExpansion's interface method on client-go's PodInterface) --
// exactly the path a cluster's kubelet-adjacent Prometheus scrape does NOT
// use (that goes pod-IP-direct), but the one every e2e client without a
// pod-IP route (this suite runs outside the cluster network) can. The
// listener is deliberately unauthenticated (#33 design record §0.1), so no
// bearer token is needed.
func ScrapeOperatorMetrics(ctx context.Context) (string, error) {
	podName, err := OperatorPodName(ctx, h)
	if err != nil {
		return "", err
	}
	data, err := h.Clientset.CoreV1().Pods(h.Cfg.OperatorNamespace).
		ProxyGet("http", podName, "8080", "/metrics", nil).
		DoRaw(ctx)
	if err != nil {
		return "", fmt.Errorf("scraping /metrics from pod %s/%s: %w", h.Cfg.OperatorNamespace, podName, err)
	}
	return string(data), nil
}

// Sample is one Prometheus exposition-format sample line: a metric name,
// its label set, and its value.
type Sample struct {
	Name   string
	Labels map[string]string
	Value  float64
}

// MetricFamily is one # HELP/# TYPE block plus every sample line under it.
type MetricFamily struct {
	Name    string
	Help    string
	Type    string
	Samples []Sample
}

// ParsePromText parses the Prometheus text exposition format (RFC-less but
// stable: https://github.com/prometheus/docs/blob/main/content/docs/instrumenting/exposition_formats.md)
// into one MetricFamily per declared metric name, keyed by name. This is a
// deliberately small, line-based parser -- test/e2e's go.mod has a narrow
// dependency surface by design (see its own header comment), so this avoids
// pulling in github.com/prometheus/client_golang's full text-parsing
// machinery for four assertions' worth of need. It handles exactly the
// shapes this operator's own /metrics output uses: HELP/TYPE comment lines,
// bare "name value" samples, and "name{k=\"v\",...} value" samples --
// histogram/summary _bucket/_sum/_count samples are ordinary samples here,
// not specially unpacked (MetricValue/SampleCount below do that lookup).
func ParsePromText(text string) map[string]*MetricFamily {
	families := make(map[string]*MetricFamily)
	getOrCreate := func(name string) *MetricFamily {
		f, ok := families[name]
		if !ok {
			f = &MetricFamily{Name: name}
			families[name] = f
		}
		return f
	}

	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "# HELP ") {
			rest := strings.TrimPrefix(line, "# HELP ")
			name, help, ok := strings.Cut(rest, " ")
			if ok {
				getOrCreate(name).Help = help
			}
			continue
		}
		if strings.HasPrefix(line, "# TYPE ") {
			rest := strings.TrimPrefix(line, "# TYPE ")
			name, typ, ok := strings.Cut(rest, " ")
			if ok {
				getOrCreate(name).Type = typ
			}
			continue
		}
		if strings.HasPrefix(line, "#") {
			continue // any other comment line
		}
		sample, ok := parseSampleLine(line)
		if !ok {
			continue
		}
		// The family a sample belongs to is its base name: a histogram's
		// "_bucket"/"_sum"/"_count" samples, and a summary's "_sum"/"_count",
		// all belong to the family declared by the un-suffixed # TYPE name.
		// Every metric this operator emits is a gauge, counter or histogram
		// with no quantile-suffixed summary samples, so stripping exactly
		// these three suffixes is sufficient.
		base := sample.Name
		for _, suffix := range []string{"_bucket", "_sum", "_count"} {
			if strings.HasSuffix(base, suffix) {
				candidate := strings.TrimSuffix(base, suffix)
				if _, declared := families[candidate]; declared {
					base = candidate
				}
				break
			}
		}
		f := getOrCreate(base)
		f.Samples = append(f.Samples, sample)
	}
	return families
}

// parseSampleLine parses one non-comment exposition line into a Sample.
func parseSampleLine(line string) (Sample, bool) {
	name := line
	var labelPart string
	if i := strings.IndexByte(line, '{'); i >= 0 {
		name = line[:i]
		j := strings.IndexByte(line[i:], '}')
		if j < 0 {
			return Sample{}, false
		}
		labelPart = line[i+1 : i+j]
		line = strings.TrimSpace(line[i+j+1:])
	} else {
		parts := strings.Fields(line)
		if len(parts) < 2 {
			return Sample{}, false
		}
		name = parts[0]
		line = parts[1]
	}
	// line is now just the value (a trailing exemplar/timestamp, if any, is
	// whitespace-separated -- this operator never emits either, so take the
	// first field only).
	valueField := strings.Fields(line)
	if len(valueField) == 0 {
		return Sample{}, false
	}
	value, err := strconv.ParseFloat(valueField[0], 64)
	if err != nil {
		return Sample{}, false
	}
	return Sample{Name: name, Labels: parseLabels(labelPart), Value: value}, true
}

// parseLabels parses a comma-separated k="v" label list. Sufficient for
// this operator's own label values (short enum-like strings -- see
// internal/metrics's sanitize allowlists), not a general Prometheus label-
// escaping implementation.
func parseLabels(s string) map[string]string {
	if s == "" {
		return nil
	}
	labels := make(map[string]string)
	for _, kv := range strings.Split(s, ",") {
		kv = strings.TrimSpace(kv)
		if kv == "" {
			continue
		}
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		labels[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"`)
	}
	return labels
}

// MetricFamilyPresent reports whether name was declared at all (a # HELP or
// # TYPE line, or at least one sample line, was seen for it).
func MetricFamilyPresent(families map[string]*MetricFamily, name string) bool {
	_, ok := families[name]
	return ok
}

// MetricHelp returns name's HELP text and whether the family exists.
func MetricHelp(families map[string]*MetricFamily, name string) (string, bool) {
	f, ok := families[name]
	if !ok {
		return "", false
	}
	return f.Help, true
}

// MetricValue returns the value of the sample named exactly name (which may
// carry a "_bucket"/"_sum"/"_count" suffix for a histogram) whose label set
// matches labels exactly (nil/empty matches an unlabeled sample), and
// whether one was found. ParsePromText files every "_bucket"/"_sum"/"_count"
// sample under its BASE family name (see its own doc comment), so a
// histogram's "_count" sample is looked up inside families[name] -- the base
// family -- never under a families[name+"_count"] key, which ParsePromText
// never creates.
func MetricValue(families map[string]*MetricFamily, name string, labels map[string]string) (float64, bool) {
	base := name
	for _, suffix := range []string{"_bucket", "_sum", "_count"} {
		if strings.HasSuffix(name, suffix) {
			base = strings.TrimSuffix(name, suffix)
			break
		}
	}
	f, ok := families[base]
	if !ok {
		return 0, false
	}
	for _, s := range f.Samples {
		if s.Name == name && labelsEqual(s.Labels, labels) {
			return s.Value, true
		}
	}
	return 0, false
}

// SampleCount returns a histogram or counter family's total sample count:
// the "<name>_count" sample for a histogram, or the counter's own value.
func SampleCount(families map[string]*MetricFamily, name string, labels map[string]string) (float64, bool) {
	f, ok := families[name]
	if !ok {
		return 0, false
	}
	if f.Type == "histogram" {
		return MetricValue(families, name+"_count", labels)
	}
	return MetricValue(families, name, labels)
}

func labelsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
