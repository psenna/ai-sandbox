package e2e

import (
	"context"
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"sigs.k8s.io/controller-runtime/pkg/client"

	sandboxv1alpha1 "github.com/psenna/ai-sandbox/operator/api/v1alpha1"
)

// probeBrokerURL is the in-cluster address of the fake git-proxy broker
// (test/e2e/doubles), which the operator's ProbeEvaluator (#30) reaches from
// inside the cluster through the platform-doubles Service.
func probeBrokerURL(h *Harness) string {
	return fmt.Sprintf("http://platform-doubles.%s.svc.cluster.local:8080", h.Cfg.ServicesNamespace)
}

// probeGitProxyClass creates a SandboxClass whose services.gitProxy points at
// the fake broker, with the git-proxy-token Secret the e2e manifests
// provision (test/e2e/manifests/doubles.yaml) in the operator's own namespace
// (Config.ClassSecretNamespace's default).
func probeGitProxyClass(ctx context.Context, h *Harness) *sandboxv1alpha1.SandboxClass {
	return h.CreateClass(ctx, WithGitProxy(
		"https://github.com/psenna/e2e-fixture",
		probeBrokerURL(h),
		"git-proxy-token",
		"token",
	))
}

// probeCheckHits counts the fake broker's request-log entries for the
// GitProxyCheck endpoint (GET /{repo}/checks/{ref}).
func probeCheckHits(ctx context.Context, h *Harness) int {
	var n int
	for _, r := range h.BrokerRequests(ctx) {
		if strings.HasPrefix(r.Path, "/psenna/e2e-fixture/checks/") {
			n++
		}
	}
	return n
}

var _ = Describe("wait probes", func() {
	var (
		ctx context.Context
		ns  string
	)

	BeforeEach(func() {
		ctx = context.Background()
		ns = h.NewNamespace(ctx, "e2e-probe")
	})

	It("satisfies a GitProxyCheck wait and restores the environment", func() {
		DeferCleanup(h.BrokerReset, ctx)
		class := probeGitProxyClass(ctx, h)
		// Program the broker to "pending" so the environment reaches Waiting
		// and HOLDS there: the probe keeps answering "not yet". An upfront
		// "success" would satisfy the probe on its very first evaluation, so
		// the env would zip through Waiting in a single reconcile and
		// WaitForPhase(Waiting) would race and miss it. The wait is satisfied
		// instead by flipping the broker to "success" below, once Waiting is
		// observed.
		h.BrokerSetCheckSummary(ctx, "psenna/e2e-fixture", "refs/heads/feat/x", "pending")
		env := h.CreateEnvironment(ctx, ns, class.Name, WithScript(
			// The wait is declared only on the first run: the woken run (the
			// restore init container wrote .sandbox/last-wake.json) must not
			// re-declare it, or the environment would re-freeze and cycle.
			`SCRIPT:unless-file /workspace/.sandbox/last-wake.json SCRIPT:sandbox-wait {"type":"GitProxyCheck","reason":"e2e probe green","params":{"ref":"refs/heads/feat/x"}}`,
			"SCRIPT:unless-file /workspace/.sandbox/last-wake.json SCRIPT:sleep 300",
			"SCRIPT:if-file /workspace/.sandbox/last-wake.json SCRIPT:sleep 300",
		))
		key := client.ObjectKey{Namespace: ns, Name: env.Name}

		// Freezing -> Waiting requires a real, successful snapshot (#28);
		// reaching Waiting is itself proof the freeze completed. With the
		// broker held at "pending" the env stays Waiting, so this is observable.
		h.WaitForPhase(ctx, key, sandboxv1alpha1.PhaseWaiting, h.Cfg.PhaseTimeout)

		// Flip the broker to "success": the next probe evaluation (at the
		// backoff window's NextEligibleAt) sees a terminal overall, satisfies
		// the wait, and the env restores. The probe evaluates the wait;
		// satisfied -> Ready -> Restoring -> Running. Ready is transient
		// (nextReady advances straight to Restoring), so accept any of the
		// three.
		h.BrokerSetCheckSummary(ctx, "psenna/e2e-fixture", "refs/heads/feat/x", "success")
		h.WaitForAnyPhase(ctx, key, h.Cfg.PhaseTimeout,
			sandboxv1alpha1.PhaseReady, sandboxv1alpha1.PhaseRestoring, sandboxv1alpha1.PhaseRunning)

		got := h.GetEnv(ctx, key)
		Expect(got.Status.ProbeAttempt).NotTo(BeNil(), "a GitProxyCheck wait must produce a probeAttempt")
		Expect(got.Status.ProbeAttempt.Phase).To(Equal(sandboxv1alpha1.ProbeAttemptSatisfied))
		Expect(got.Status.ProbeAttempt.LastResult).To(Equal("satisfied"))
		Expect(got.Status.WaitFor).To(BeNil(), "a satisfied wait must be cleared")
	})

	It("keeps a pending GitProxyCheck wait Waiting without hammering the checks endpoint", func() {
		DeferCleanup(h.BrokerReset, ctx)
		class := probeGitProxyClass(ctx, h)
		// Explicitly program "pending": the green-path spec above programs the
		// same (repo, ref) to "success", and the fake broker is stateful and
		// shared across specs (PUT /_control/state persists until cleared), so
		// relying on the unset default would inherit that "success" and the
		// probe would satisfy immediately -- cycling the env out of Waiting
		// instead of holding it there.
		h.BrokerSetCheckSummary(ctx, "psenna/e2e-fixture", "refs/heads/feat/x", "pending")
		env := h.CreateEnvironment(ctx, ns, class.Name, WithScript(
			`SCRIPT:sandbox-wait {"type":"GitProxyCheck","reason":"e2e probe pending","params":{"ref":"refs/heads/feat/x"}}`,
			"SCRIPT:sleep 300",
		))
		key := client.ObjectKey{Namespace: ns, Name: env.Name}

		h.WaitForPhase(ctx, key, sandboxv1alpha1.PhaseWaiting, h.Cfg.PhaseTimeout)

		// Snapshot the checks-endpoint hit count, then watch the environment
		// stay Waiting for a full backoff window.
		before := probeCheckHits(ctx, h)
		h.ExpectStablePhase(ctx, key, sandboxv1alpha1.PhaseWaiting, 10*time.Second)

		// The per-probe backoff schedule is [1s,2s,4s,8s] (+/-20% jitter), so
		// over a 10s window the checks endpoint is hit at most 4 times (at
		// ~0s, ~1s, ~3s, ~7s). A probe evaluated on every reconcile -- or
		// hammered on a hot loop -- would blow past this bound.
		Expect(probeCheckHits(ctx, h)-before).To(BeNumerically("<=", 4),
			"checks endpoint must be hit at most once per backoff interval")
	})
})
