package e2e

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sandboxv1alpha1 "github.com/psenna/ai-sandbox/operator/api/v1alpha1"
)

// The workload-container internet-isolation criterion for #31 is NOT yet
// testable end to end: it requires a sandbox whose WORKLOAD CONTAINER (the
// rootless-podman engine, #24) attempts an outbound connection, so the
// NetworkPolicy's egress restriction is exercised on the nested container's
// traffic, not just the agent pod's. The rootless-podman engine is not
// implemented yet (engine.type=rootless-podman fails closed -- see
// lifecycle_test.go's "fails closed, visibly, for an engine that is not
// implemented yet"), so this spec is PENDING until #24 lands.
//
// TODO(#24): once the rootless-podman engine exists, un-pend this spec and
// drive the workload container (not the agent pod) to attempt an outbound
// connection: under a Restricted class the attempt must fail, under an Open
// class it must succeed. The NetworkPolicy itself is already covered by
// internal/controller/networkpolicy_test.go (envtest) and the CNI gate in
// netpolicy_test.go; this spec is the last, workload-level acceptance
// criterion.
var _ = Describe("network isolation", func() {
	// Regression guard for the CNI enforcement probe's API-server blind spot
	// (#31): the probe originally dialed the `kubernetes` Service, which on
	// kind/k3s runs on the host network where kindnetd does not enforce
	// egress -- so the default-deny never blocked the dial and the operator
	// reported NotEnforced on a cluster that enforces correctly. The probe
	// now measures pod-to-pod (a probe-listen peer pod dialed by probe-tcp
	// client pods), which is the traffic path NetworkPolicy actually governs.
	// This spec asserts the observable outcome on the kind cluster the suite
	// runs on: the CNIEnforcement condition must report True/Enforced.
	It("reports CNI enforcement verified on Restricted environments", func() {
		ctx := context.Background()
		ns := h.NewNamespace(ctx, "e2e-isolation")
		restricted := h.CreateClass(ctx, WithNetworkIsolation(sandboxv1alpha1.NetworkIsolationRestricted))
		env := h.CreateEnvironment(ctx, ns, restricted.Name, WithScript("SCRIPT:sleep 60"))
		key := client.ObjectKey{Namespace: ns, Name: env.Name}

		h.WaitForPhase(ctx, key, sandboxv1alpha1.PhaseRunning, h.Cfg.PhaseTimeout)
		h.WaitForCondition(ctx, key, "CNIEnforcement", metav1.ConditionTrue, "Enforced", h.Cfg.PhaseTimeout)
	})

	PIt("restricts a workload container's egress under Restricted isolation and allows it under Open", func() {
		ctx := context.Background()
		ns := h.NewNamespace(ctx, "e2e-isolation")

		restricted := h.CreateClass(ctx, WithNetworkIsolation(sandboxv1alpha1.NetworkIsolationRestricted))
		open := h.CreateClass(ctx, WithNetworkIsolation(sandboxv1alpha1.NetworkIsolationOpen))

		restrictedEnv := h.CreateEnvironment(ctx, ns, restricted.Name, WithScript("SCRIPT:sleep 60"))
		openEnv := h.CreateEnvironment(ctx, ns, open.Name, WithScript("SCRIPT:sleep 60"))

		h.WaitForPhase(ctx, client.ObjectKey{Namespace: ns, Name: restrictedEnv.Name}, sandboxv1alpha1.PhaseRunning, h.Cfg.PhaseTimeout)
		h.WaitForPhase(ctx, client.ObjectKey{Namespace: ns, Name: openEnv.Name}, sandboxv1alpha1.PhaseRunning, h.Cfg.PhaseTimeout)

		// TODO(#24): exec into the WORKLOAD container (not the agent pod) and
		// attempt an outbound connection to a public host. Restricted must
		// fail, Open must succeed.
		Expect(restrictedEnv.Name).NotTo(BeEmpty())
		Expect(openEnv.Name).NotTo(BeEmpty())
	})
})
