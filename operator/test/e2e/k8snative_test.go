package e2e

import (
	"context"
	"sort"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sandboxv1alpha1 "github.com/psenna/ai-sandbox/operator/api/v1alpha1"
)

var _ = Describe("k8s-native engine", func() {
	var (
		ctx context.Context
		ns  string
	)

	BeforeEach(func() {
		ctx = context.Background()
		ns = h.NewNamespace(ctx, "e2e-k8snative")
	})

	// Replaces the "rootless-podman is unimplemented" posture on main. The
	// k8s-native engine IS implemented: it renders a thin agent pod (agent +
	// sandboxctl containers only -- no engine-specific sidecar) and needs no
	// security relaxations, so EngineSecurityRelaxed is False/NoRelaxation.
	It("renders a thin agent pod and reports NoRelaxation", func() {
		class := h.CreateClass(ctx, WithEngine(sandboxv1alpha1.EngineTypeK8sNative))
		env := h.CreateEnvironment(ctx, ns, class.Name, WithScript(
			"SCRIPT:require-file /etc/ai-sandbox/sandbox.json",
			"SCRIPT:require-no-file /var/run/secrets/kubernetes.io/serviceaccount/token",
			"SCRIPT:sandbox-done success k8snative-thin-agent",
			"SCRIPT:sleep 2",
			"SCRIPT:exit 0",
		))
		key := client.ObjectKey{Namespace: ns, Name: env.Name}

		// No security relaxation: k8s-native needs none.
		h.WaitForCondition(ctx, key, "EngineSecurityRelaxed", metav1.ConditionFalse, "NoRelaxation", h.Cfg.PhaseTimeout)

		// The agent pod exists (the engine rendered it -- unlike the not-implemented
		// rootless-podman posture, which never creates a pod).
		pod := h.GetAgentPod(ctx, key)
		var names []string
		for _, c := range pod.Spec.Containers {
			names = append(names, c.Name)
		}
		sort.Strings(names)
		// Exactly the always-present containers: agent + sandboxctl. No
		// engine-specific sidecar (a rootless-podman engine would add a podman
		// sidecar; k8s-native does not). Restore is an init container, excluded
		// from Spec.Containers.
		Expect(names).To(Equal([]string{"agent", "sandboxctl"}),
			"thin agent pod should have only agent+sandboxctl containers, got %v", names)

		// The agent holds no Kubernetes credential (headline invariant, re-asserted
		// for the k8s-native engine -- the spec reaches Done through the loopback
		// control API alone).
		h.WaitForPhase(ctx, key, sandboxv1alpha1.PhaseDone, h.Cfg.PhaseTimeout)
		h.ExpectCondition(ctx, key, "Ready", metav1.ConditionFalse, "AgentReportedSuccess")

		// No ServiceSet was applied in this spec, so there are no ServiceSet pods.
		Expect(h.ServiceSetEntries(ctx, ns, env.Name)).To(BeEmpty(),
			"no ServiceSet should exist until the agent applies one")
	})
})
