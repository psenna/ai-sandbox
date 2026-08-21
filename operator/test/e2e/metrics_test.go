package e2e

import (
	"context"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sandboxv1alpha1 "github.com/psenna/ai-sandbox/operator/api/v1alpha1"
)

// e2eSlotCapacity mirrors test/e2e/manifests/operator/kustomization.yaml's
// --slot-capacity=8 (that patch REPLACES the whole args array, so this must
// be kept in sync by hand -- there is no way to read the flag back from the
// running Deployment without parsing its args, which would be more fragile
// than one constant).
const e2eSlotCapacity = 8

// expectedMetricFamilies is #33's full catalogue, name -> Prometheus type,
// used by the "every declared family is present" spec below. Mirrors
// internal/metrics/metrics.go's own table; kept as plain string literals
// rather than importing internal/metrics, since test/e2e's go.mod
// deliberately has a narrow dependency surface (see its own header comment)
// and internal/metrics pulls in github.com/prometheus/client_golang, which
// this module does not (and should not) depend on.
var expectedMetricFamilies = map[string]string{
	"sandbox_operator_environments":               "gauge",
	"sandbox_operator_slot_capacity":              "gauge",
	"sandbox_operator_slots_used":                 "gauge",
	"sandbox_operator_queue_depth":                "gauge",
	"sandbox_operator_queue_wait_seconds":         "histogram",
	"sandbox_operator_freeze_duration_seconds":    "histogram",
	"sandbox_operator_wake_duration_seconds":      "histogram",
	"sandbox_operator_snapshot_size_bytes":        "histogram",
	"sandbox_operator_probe_evaluations_total":    "counter",
	"sandbox_operator_archives_total":             "counter",
	"sandbox_operator_reconcile_errors_total":     "counter",
	"sandbox_operator_warm_cache_reclaimed_total": "counter",
	"sandbox_operator_retention_deleted_total":    "counter",
}

// expectedEventReasonsPlainRun is the Event reason set a plain run -- admitted,
// started cold, completes successfully, archives -- produces, mirroring
// internal/controller/events.go's Reason* constants (kept as literals for
// the same narrow-dependency-surface reason as expectedMetricFamilies
// above). Waking/Frozen/WaitSatisfied/SnapshotFailed/Failed are NOT in this
// set: they require a freeze/wake cycle or a failure this spec's plain run
// never exercises (see the freeze/wake spec below, which does drive those).
var expectedEventReasonsPlainRun = []string{"SlotGranted", "Starting", "Started", "Completed", "Archived"}

var _ = Describe("operator metrics", func() {
	var (
		ctx context.Context
		ns  string
	)

	BeforeEach(func() {
		ctx = context.Background()
		ns = h.NewNamespace(ctx, "e2e-metrics")
	})

	It("exposes every declared sandbox_operator_* family with non-empty HELP text, and slot_capacity matches the overlay", func() {
		text, err := ScrapeOperatorMetrics(ctx)
		Expect(err).NotTo(HaveOccurred())
		families := ParsePromText(text)

		for name, typ := range expectedMetricFamilies {
			Expect(MetricFamilyPresent(families, name)).To(BeTrue(), "expected metric family %s to be present in the scrape", name)
			help, _ := MetricHelp(families, name)
			Expect(help).NotTo(BeEmpty(), "metric family %s has empty HELP text", name)
			Expect(families[name].Type).To(Equal(typ), "metric family %s TYPE", name)
		}

		capacity, ok := MetricValue(families, "sandbox_operator_slot_capacity", nil)
		Expect(ok).To(BeTrue(), "sandbox_operator_slot_capacity sample")
		Expect(capacity).To(Equal(float64(e2eSlotCapacity)), "slot_capacity should match the e2e overlay's --slot-capacity flag")
	})

	It("advances freeze/snapshot/wake/probe metrics across a real freeze/wake cycle", func() {
		before, err := ScrapeOperatorMetrics(ctx)
		Expect(err).NotTo(HaveOccurred())
		beforeFamilies := ParsePromText(before)
		freezeCountBefore, _ := SampleCount(beforeFamilies, "sandbox_operator_freeze_duration_seconds", nil)
		snapshotCountBefore, _ := SampleCount(beforeFamilies, "sandbox_operator_snapshot_size_bytes", nil)
		wakeCountBefore, _ := SampleCount(beforeFamilies, "sandbox_operator_wake_duration_seconds", map[string]string{"source": "warm"})
		probeCountBefore, _ := SampleCount(beforeFamilies, "sandbox_operator_probe_evaluations_total", map[string]string{"type": "NotBefore", "result": "satisfied"})

		class := h.CreateClass(ctx)
		env := h.CreateEnvironment(ctx, ns, class.Name, WithScript(
			"SCRIPT:write hello.txt world",
			// This spec (unlike every other freeze/wait spec, which uses
			// "1h" and force-clears via h.ClearWaitFor) lets a short wait
			// elapse and satisfy itself for real -- it cares about the
			// probe_evaluations_total{result="satisfied"} metric, which only
			// records on a real evaluation, not on the wait being
			// force-cleared out from under it. h.WaitForWakeCount below (not
			// an intermediate h.WaitForPhase(Waiting) poll) is what proves
			// the freeze/wake cycle actually happened: WakeCount increments
			// only on Restoring->Running with a non-nil snapshot (next.go),
			// so it cannot be satisfied any other way. A phase poll here
			// would race -- a short enough NotBefore can let the environment
			// pass all the way through Waiting to Done between two 2s polls.
			`SCRIPT:unless-file /workspace/.sandbox/last-wake.json SCRIPT:sandbox-wait {"type":"NotBefore","reason":"e2e metrics test","params":{"duration":"5s"}}`,
			"SCRIPT:unless-file /workspace/.sandbox/last-wake.json SCRIPT:sleep 300",
			"SCRIPT:if-file /workspace/.sandbox/last-wake.json SCRIPT:sandbox-done success resumed",
		))
		key := client.ObjectKey{Namespace: ns, Name: env.Name}

		h.WaitForWakeCount(ctx, key, 1)
		h.WaitForPhase(ctx, key, sandboxv1alpha1.PhaseDone, h.Cfg.PhaseTimeout)
		h.ExpectAgentExitCode(ctx, key, 0)

		Eventually(func() (float64, error) {
			text, err := ScrapeOperatorMetrics(ctx)
			if err != nil {
				return 0, err
			}
			n, _ := SampleCount(ParsePromText(text), "sandbox_operator_freeze_duration_seconds", nil)
			return n, nil
		}, h.Cfg.PhaseTimeout, h.Cfg.Poll).Should(BeNumerically(">", freezeCountBefore), "freeze_duration_seconds should have gained an observation")

		text, err := ScrapeOperatorMetrics(ctx)
		Expect(err).NotTo(HaveOccurred())
		families := ParsePromText(text)

		snapshotCountAfter, _ := SampleCount(families, "sandbox_operator_snapshot_size_bytes", nil)
		Expect(snapshotCountAfter).To(BeNumerically(">", snapshotCountBefore), "snapshot_size_bytes should have gained an observation")

		wakeCountAfter, _ := SampleCount(families, "sandbox_operator_wake_duration_seconds", map[string]string{"source": "warm"})
		Expect(wakeCountAfter).To(BeNumerically(">", wakeCountBefore), "wake_duration_seconds{source=warm} should have gained an observation (the PVC survives an in-place freeze/wake, so the workspace root restores warm)")

		probeCountAfter, _ := SampleCount(families, "sandbox_operator_probe_evaluations_total", map[string]string{"type": "NotBefore", "result": "satisfied"})
		Expect(probeCountAfter).To(BeNumerically(">", probeCountBefore), "probe_evaluations_total{type=NotBefore,result=satisfied} should have gained an observation")
	})

	It("saturates and drains the scheduling slots, advancing queue_wait_seconds", Serial, func() {
		// Serial: this spec deliberately over-subscribes the whole cluster's
		// e2eSlotCapacity slots, which would starve every other spec's own
		// environments of a slot if run concurrently with them.
		before, err := ScrapeOperatorMetrics(ctx)
		Expect(err).NotTo(HaveOccurred())
		queueWaitCountBefore, _ := SampleCount(ParsePromText(before), "sandbox_operator_queue_wait_seconds", nil)

		const overSubscribeBy = 2
		total := e2eSlotCapacity + overSubscribeBy
		class := h.CreateClass(ctx)
		keys := make([]client.ObjectKey, 0, total)
		for i := 0; i < total; i++ {
			env := h.CreateEnvironment(ctx, ns, class.Name, WithScript("SCRIPT:sleep 20", "SCRIPT:sandbox-done success ok"))
			keys = append(keys, client.ObjectKey{Namespace: ns, Name: env.Name})
		}

		Eventually(func() (float64, error) {
			text, err := ScrapeOperatorMetrics(ctx)
			if err != nil {
				return 0, err
			}
			n, _ := MetricValue(ParsePromText(text), "sandbox_operator_slots_used", nil)
			return n, nil
		}, h.Cfg.PhaseTimeout, h.Cfg.Poll).Should(Equal(float64(e2eSlotCapacity)), "slots_used should saturate at capacity")

		Eventually(func() (float64, error) {
			text, err := ScrapeOperatorMetrics(ctx)
			if err != nil {
				return 0, err
			}
			n, _ := MetricValue(ParsePromText(text), "sandbox_operator_queue_depth", nil)
			return n, nil
		}, h.Cfg.PhaseTimeout, h.Cfg.Poll).Should(Equal(float64(overSubscribeBy)), "queue_depth should reflect the over-subscribed environments")

		// Drain: wait for every environment to complete, which frees slots
		// and admits the queued ones -- each admission is a queue_wait_seconds
		// observation.
		for _, key := range keys {
			h.WaitForAnyPhase(ctx, key, h.Cfg.PhaseTimeout, sandboxv1alpha1.PhaseDone, sandboxv1alpha1.PhaseFailed)
		}

		Eventually(func() (float64, error) {
			text, err := ScrapeOperatorMetrics(ctx)
			if err != nil {
				return 0, err
			}
			n, _ := SampleCount(ParsePromText(text), "sandbox_operator_queue_wait_seconds", nil)
			return n, nil
		}, h.Cfg.PhaseTimeout, h.Cfg.Poll).Should(BeNumerically(">=", queueWaitCountBefore+float64(overSubscribeBy)),
			"queue_wait_seconds should have gained one observation per queued environment as they drained and were admitted")
	})

	It("runs an environment to completion and records the full plain-run Event reason set with secret-free messages", func() {
		class := h.CreateClass(ctx)
		// A brief sleep before sandbox-done, not an immediate one: Started
		// fires only on the Restoring->Running transition (nextRestoring,
		// gated on the pod being observed Running+Ready), and
		// agentOrPodTerminal's own doc comment notes a fast-enough agent can
		// report /v1/done before the controller ever observes the pod ready,
		// skipping straight from Restoring to Done and never firing Started.
		// This is correct, common production behavior for a truly
		// instantaneous script -- not a bug -- so the fix belongs in this
		// spec's fixture, not in the state machine.
		env := h.CreateEnvironment(ctx, ns, class.Name, WithScript("SCRIPT:sleep 2", "SCRIPT:sandbox-done success ok"))
		key := client.ObjectKey{Namespace: ns, Name: env.Name}

		h.WaitForPhase(ctx, key, sandboxv1alpha1.PhaseDone, h.Cfg.PhaseTimeout)
		final := h.GetEnv(ctx, key)

		// The event broadcaster (client-go/tools/events) delivers Eventf calls
		// through a buffered channel and an async flush loop, so an Event that
		// was emitted the instant the environment reached Done is not
		// guaranteed to be visible to a List issued immediately after --
		// Eventually, not a single point-in-time List, is required.
		byReason := map[string]corev1.Event{}
		Eventually(func() bool {
			var eventsList corev1.EventList
			if err := h.Client.List(ctx, &eventsList, client.InNamespace(ns)); err != nil {
				return false
			}
			byReason = map[string]corev1.Event{}
			for _, e := range eventsList.Items {
				if e.InvolvedObject.UID != final.UID {
					continue
				}
				byReason[e.Reason] = e
			}
			for _, reason := range expectedEventReasonsPlainRun {
				if _, ok := byReason[reason]; !ok {
					return false
				}
			}
			return true
		}, h.Cfg.PhaseTimeout, h.Cfg.Poll).Should(BeTrue(),
			"environment %s/%s never accumulated all expected Event reasons %v (have %v)", ns, env.Name, expectedEventReasonsPlainRun, byReason)

		for _, reason := range expectedEventReasonsPlainRun {
			e, ok := byReason[reason]
			Expect(ok).To(BeTrue(), "expected an Event with reason %s regarding %s/%s", reason, ns, env.Name)
			Expect(e.Message).NotTo(BeEmpty(), "Event %s has an empty message", reason)
			// A secret-free spot check: no Event message should ever contain
			// what looks like a bearer token or an AWS-style access key ID --
			// mirrors internal/controller/secretleak_test.go's sentinel
			// approach, generalized here to "no message looks credential-
			// shaped" since this spec has no single sentinel value to grep
			// for (it never wires a fake credential of its own).
			Expect(strings.Contains(e.Message, "AKIA")).To(BeFalse(), "Event %s message looks like it contains an AWS access key ID: %q", reason, e.Message)
		}
	})
})
