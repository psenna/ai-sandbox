// Package metrics defines the operator's Prometheus metrics (#33): the
// series, their label allowlists, and the recording methods every other
// package calls to populate them.
//
// This is a new package, not internal/controller/metrics.go, for three
// reasons. First, this repo isolates dependency-light, table-testable
// concerns into their own packages (internal/render, internal/storage,
// internal/scheduler) -- Collectors has no Kubernetes client dependency and
// belongs with that family. Second, internal/controller/suite_test.go's
// TestMain calls os.Exit(0) when KUBEBUILDER_ASSETS is unset, skipping that
// package's ENTIRE test suite without envtest; a metrics package that always
// runs, cluster or no cluster, is what makes "every metric is asserted
// present by a test" a property CI can actually check on every run. Third,
// recording sites span both internal/controller and internal/operator, so a
// shared package avoids an import cycle between them.
//
// Registration model: New builds a Collectors with every series constructed
// but registered nowhere; MustRegister explicitly registers them into a
// prometheus.Registerer. Default is a package-level Collectors registered
// into sigs.k8s.io/controller-runtime/pkg/metrics's Registry at init time --
// the manager's existing /metrics listener (internal/operator/manager.go's
// metricsserver.Options) serves it with no new server, no new flag, no new
// auth. Production wiring (internal/operator/controllers.go) passes
// metrics.Default to every controller and Runnable that records a metric;
// tests that need to assert an absolute value inject a fresh metrics.New()
// registered into a private prometheus.NewRegistry() instead, since Default
// is shared process-wide across an entire test binary.
//
// Every recording method has a nil-receiver no-op guard, matching this
// codebase's existing r.Recorder == nil / r.Probes == nil convention: a
// struct field of type *Collectors left nil (the zero value) is silently
// inert rather than a nil-pointer panic, so a test that doesn't care about
// metrics need not construct one.
//
// No label ever carries an environment name, namespace, or UID -- that is a
// deliberate, permanent cardinality bound, not an oversight to "fix" later.
// Every label value that could conceivably carry unexpected data (a wait
// type, a probe/archive result, a wake source) passes through a closed
// allowlist (sanitize, mirroring internal/lifecycle/conditions.go's
// SanitizeReason) before it reaches a metric: an unrecognized value becomes
// "other" rather than creating an unbounded new series.
//
// internal/lifecycle MUST NOT import this package under any circumstance --
// see that package's own doc.go for its closed import list. Every metric
// tied to a state-machine transition is recorded in internal/controller, at
// the point the transition is durably persisted (events.go's
// observeTransition), never inside the pure lifecycle package itself.
package metrics
