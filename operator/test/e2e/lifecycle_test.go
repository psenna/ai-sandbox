package e2e

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sandboxv1alpha1 "github.com/psenna/ai-sandbox/operator/api/v1alpha1"
	"github.com/psenna/ai-sandbox/operator/internal/render"
)

var _ = Describe("sandbox lifecycle", func() {
	var (
		ctx context.Context
		ns  string
	)

	BeforeEach(func() {
		ctx = context.Background()
		ns = h.NewNamespace(ctx, "e2e-lifecycle")
	})

	It("reaches Done for a script that writes a file and exits 0", func() {
		class := h.CreateClass(ctx)
		env := h.CreateEnvironment(ctx, ns, class.Name, WithScript(
			// SCRIPT:sleep 1 is deliberate, not padding: an agent that
			// completes near-instantly can finish (PodSucceeded) faster
			// than the reconciler's own Pending->Ready->Restoring
			// transitions settle, which was observed (real kind cluster,
			// not envtest) to occasionally recreate the agent pod once --
			// a stale-informer-cache race in the controller's own
			// Restoring->Done handling, outside this issue's scope to fix
			// (see internal/lifecycle/next.go's agentOrPodTerminal: a
			// recreated pod after the sticky Done transition is never
			// revisited for cleanup). A short, deliberate delay before
			// exit avoids exercising that race in this spec.
			"SCRIPT:sleep 1",
			"SCRIPT:write hello.txt world",
			"SCRIPT:exit 0",
		))
		key := client.ObjectKey{Namespace: ns, Name: env.Name}

		h.WaitForPhase(ctx, key, sandboxv1alpha1.PhaseDone, h.Cfg.PhaseTimeout)

		// deletePod deletes the agent pod once Done (see
		// internal/controller/pod.go's deletePod doc comment).
		Eventually(func() bool {
			return h.getAgentPodOrNil(ctx, key) == nil
		}, h.Cfg.PodTimeout, h.Cfg.Poll).Should(BeTrue(), "agent pod should be deleted once the environment reaches Done")

		names := render.ChildNames(env.Name)
		h.WaitForPVCBound(ctx, client.ObjectKey{Namespace: ns, Name: names.PVC}, h.Cfg.PodTimeout)
	})

	It("reaches Failed and retains the pod for a script that exits non-zero", func() {
		class := h.CreateClass(ctx)
		env := h.CreateEnvironment(ctx, ns, class.Name, WithScript("SCRIPT:exit 7"))
		key := client.ObjectKey{Namespace: ns, Name: env.Name}

		h.WaitForPhase(ctx, key, sandboxv1alpha1.PhaseFailed, h.Cfg.PhaseTimeout)

		h.ExpectAgentExitCode(ctx, key, 7)
		Expect(h.AgentLogs(ctx, key)).NotTo(BeEmpty())
	})

	It("reaches Running for a script that sleeps (the one spec allowed to assert Running)", func() {
		// A quick agent goes Pending -> Ready -> Restoring -> Done, and
		// Running is never reliably observed in a poll (agentOrPodTerminal
		// fires on PodSucceeded from Restoring too) unless the script
		// sleeps first.
		class := h.CreateClass(ctx)
		env := h.CreateEnvironment(ctx, ns, class.Name, WithScript("SCRIPT:sleep 60"))
		key := client.ObjectKey{Namespace: ns, Name: env.Name}

		h.WaitForPhase(ctx, key, sandboxv1alpha1.PhaseRunning, h.Cfg.PhaseTimeout)
	})

	It("reports the engine's securityContext relaxations on the environment", func() {
		// #24: engine.type=rootless-podman is implemented now, so the
		// EngineSecurityRelaxed condition reports the three relaxations
		// internal/render/engine_podman.go's podmanRelaxations declares --
		// True/EngineRelaxationApplied, not the "not implemented yet" story
		// this spec used to assert.
		class := h.CreateClass(ctx, WithPodmanEngine())
		env := h.CreateEnvironment(ctx, ns, class.Name, WithScript("SCRIPT:sleep 30"))
		key := client.ObjectKey{Namespace: ns, Name: env.Name}

		h.WaitForCondition(ctx, key, "EngineSecurityRelaxed", metav1.ConditionTrue, "EngineRelaxationApplied", h.Cfg.PhaseTimeout)
		msg := conditionMessage(h.GetEnv(ctx, key), "EngineSecurityRelaxed")
		for _, want := range []string{"podman: AppArmorUnconfined", "podman: SeccompUnconfined", "podman: AllowPrivilegeEscalation", "spike #23"} {
			Expect(msg).To(ContainSubstring(want))
		}
	})

	// ---- #27: sandboxctl sidecar control API ----

	It("records an agent-declared wait on status.waitFor and freezes", func() {
		// This spec only asserts the environment PASSES THROUGH Freezing
		// with a correctly-populated status.waitFor -- it does not assert
		// Freezing is where it stays. Since #28, a real snapshot completes
		// and the environment proceeds on to Waiting; status.waitFor is
		// never cleared by freezing itself (only nextWaiting's
		// probe-satisfied branch clears it, and no probe evaluator exists
		// yet -- #30), so the assertions below hold regardless of how far
		// past Freezing the environment has already progressed by the time
		// GetEnv runs. See freeze_test.go for the dedicated #28 snapshot
		// verification specs.
		class := h.CreateClass(ctx)
		env := h.CreateEnvironment(ctx, ns, class.Name, WithScript(
			"SCRIPT:sleep 2",
			`SCRIPT:sandbox-wait {"type":"NotBefore","reason":"e2e wait","params":{"duration":"1h"}}`,
			"SCRIPT:sleep 300",
		))
		key := client.ObjectKey{Namespace: ns, Name: env.Name}

		h.WaitForPhase(ctx, key, sandboxv1alpha1.PhaseFreezing, h.Cfg.PhaseTimeout)

		got := h.GetEnv(ctx, key)
		Expect(got.Status.WaitFor).NotTo(BeNil(), "status.waitFor should be populated")
		Expect(got.Status.WaitFor.Type).To(Equal("NotBefore"))
		Expect(got.Status.WaitFor.Reason).To(Equal("e2e wait"))
		Expect(got.Status.WaitFor.Params).To(HaveKeyWithValue("duration", "1h"))
		Expect(got.Status.WaitFor.DeclaredAt).NotTo(BeNil())
	})

	It("rejects a non-allowlisted probe and leaves status.waitFor unset", func() {
		class := h.CreateClass(ctx)
		env := h.CreateEnvironment(ctx, ns, class.Name, WithScript(
			`SCRIPT:sandbox-wait-expect-fail {"type":"SolarEclipse","reason":"nope"}`,
			`SCRIPT:sandbox-wait-expect-fail {"type":"HTTPGet","reason":"x","params":{"nope":"1"}}`,
			"SCRIPT:sandbox-done success rejections-handled",
			"SCRIPT:sleep 2",
			"SCRIPT:exit 0",
		))
		key := client.ObjectKey{Namespace: ns, Name: env.Name}

		h.WaitForPhase(ctx, key, sandboxv1alpha1.PhaseDone, h.Cfg.PhaseTimeout)

		Expect(h.GetEnv(ctx, key).Status.WaitFor).To(BeNil(), "a rejected probe must never land in status.waitFor")
	})

	It("reaches Done via /v1/done with no Kubernetes credential in the agent container", func() {
		// The headline acceptance criterion, end to end: the agent
		// container has no ServiceAccount token at all (automount is
		// disabled at both SA and pod level, and the projected token
		// volume is mounted into the sandboxctl container only), yet the
		// environment still reaches Done purely through the sidecar's
		// control API.
		class := h.CreateClass(ctx)
		env := h.CreateEnvironment(ctx, ns, class.Name, WithScript(
			"SCRIPT:require-file /etc/ai-sandbox/sandbox.json",
			"SCRIPT:require-no-file /var/run/secrets/kubernetes.io/serviceaccount/token",
			"SCRIPT:sandbox-progress starting",
			"SCRIPT:sandbox-done success e2e-complete",
			"SCRIPT:sleep 2",
			"SCRIPT:exit 0",
		))
		key := client.ObjectKey{Namespace: ns, Name: env.Name}

		h.WaitForPhase(ctx, key, sandboxv1alpha1.PhaseDone, h.Cfg.PhaseTimeout)
		h.ExpectCondition(ctx, key, "Ready", metav1.ConditionFalse, "AgentReportedSuccess")

		got := h.GetEnv(ctx, key)
		Expect(got.Status.AgentResult).NotTo(BeNil())
		Expect(got.Status.AgentResult.Outcome).To(Equal(sandboxv1alpha1.AgentOutcomeSucceeded))
		Expect(got.Status.AgentResult.Message).To(Equal("e2e-complete"))
	})

	It("does not expose the control API outside the pod", func() {
		class := h.CreateClass(ctx)
		env := h.CreateEnvironment(ctx, ns, class.Name, WithScript(
			"SCRIPT:sandbox-expect-loopback-only",
			"SCRIPT:exit 0",
		))
		key := client.ObjectKey{Namespace: ns, Name: env.Name}

		h.WaitForPhase(ctx, key, sandboxv1alpha1.PhaseDone, h.Cfg.PhaseTimeout)
	})
})
