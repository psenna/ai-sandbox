package e2e

import (
	"context"

	. "github.com/onsi/ginkgo/v2"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sandboxv1alpha1 "github.com/psenna/ai-sandbox/operator/api/v1alpha1"
)

// The workload-container internet-isolation criterion for #31 is now
// end-to-end testable: #24's rootless-podman engine renders a real workload
// container the specs below drive with `docker run` over $DOCKER_HOST. The
// NetworkPolicy itself is already covered by
// internal/controller/networkpolicy_test.go (envtest) and the CNI gate in
// netpolicy_test.go; the specs below are the workload-level acceptance
// criterion those unit tests could not reach.
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

	It("restricts a workload container's egress under Restricted isolation and allows it under Open", func() {
		// #24: this is the acceptance criterion the spike explicitly did NOT
		// cover ("No NetworkPolicy testing") and which docs/security.md
		// lists as the one previously-unverified row in its verification
		// table. It is the difference between isolation: Restricted working
		// and silently enforcing nothing -- a security control that fails
		// open and looks fine. The blocked destination is the
		// platform-doubles broker: a real in-cluster Service that neither
		// class declares as a peer, so under Restricted it is denied and
		// under Open it is reachable. Both halves are asserted, so the spec
		// cannot pass vacuously by blocking everything.
		ctx := context.Background()
		ns := h.NewNamespace(ctx, "e2e-isolation")
		blocked := h.DoublesBrokerInClusterURL() + "/healthz"

		restricted := h.CreateClass(ctx, WithPodmanEngine(), WithNetworkIsolation(sandboxv1alpha1.NetworkIsolationRestricted))
		open := h.CreateClass(ctx, WithPodmanEngine(), WithNetworkIsolation(sandboxv1alpha1.NetworkIsolationOpen))

		// alpine's busybox wget; one small image, reused by both halves.
		restrictedEnv := h.CreateEnvironment(ctx, ns, restricted.Name, WithScript(
			"SCRIPT:docker-expect-fail run --rm docker.io/library/alpine:3.21 wget -q -T 5 -O- "+blocked,
			"SCRIPT:sandbox-done success blocked-as-expected",
		))
		openEnv := h.CreateEnvironment(ctx, ns, open.Name, WithScript(
			"SCRIPT:docker run --rm docker.io/library/alpine:3.21 wget -q -T 5 -O- "+blocked,
			"SCRIPT:sandbox-done success reachable-as-expected",
		))

		restrictedKey := client.ObjectKey{Namespace: ns, Name: restrictedEnv.Name}
		openKey := client.ObjectKey{Namespace: ns, Name: openEnv.Name}

		h.WaitForPhase(ctx, restrictedKey, sandboxv1alpha1.PhaseDone, h.Cfg.PhaseTimeout)
		h.WaitForPhase(ctx, openKey, sandboxv1alpha1.PhaseDone, h.Cfg.PhaseTimeout)
		h.ExpectAgentExitCode(ctx, restrictedKey, 0)
		h.ExpectAgentExitCode(ctx, openKey, 0)
	})

	It("governs a --network host workload container by the same NetworkPolicy", func() {
		// #24's Update explicitly calls this out. Rootless podman DOES
		// support --network host (it needs no privilege -- it simply
		// creates no new network namespace), and inside the pod "host" IS
		// the pod's network namespace. That is precisely why the
		// NetworkPolicy still governs it: the traffic leaves on the pod's
		// own interface, which is what the CNI enforces on. This spec
		// proves it rather than assuming it.
		ctx := context.Background()
		ns := h.NewNamespace(ctx, "e2e-isolation")
		blocked := h.DoublesBrokerInClusterURL() + "/healthz"

		restricted := h.CreateClass(ctx, WithPodmanEngine(), WithNetworkIsolation(sandboxv1alpha1.NetworkIsolationRestricted))
		env := h.CreateEnvironment(ctx, ns, restricted.Name, WithScript(
			"SCRIPT:docker-expect-fail run --rm --network host docker.io/library/alpine:3.21 wget -q -T 5 -O- "+blocked,
			"SCRIPT:sandbox-done success blocked-as-expected",
		))
		key := client.ObjectKey{Namespace: ns, Name: env.Name}

		h.WaitForPhase(ctx, key, sandboxv1alpha1.PhaseDone, h.Cfg.PhaseTimeout)
		h.ExpectAgentExitCode(ctx, key, 0)
	})

	It("gives a --network host workload container pod loopback -- a known, documented risk", func() {
		// The real consequence of --network host, asserted as a KNOWN risk
		// so it cannot change silently: the container can reach the
		// sandboxctl control API on 127.0.0.1:9099 (and the podman API on
		// 127.0.0.1:2375, not separately exercised here). This is NOT an
		// escalation -- the agent that launched it already holds both --
		// but it means "run untrusted code in a workload container to
		// isolate it from the sandbox control plane" is FALSE. It is
		// structurally unpreventable: podman has no knob to forbid
		// --network host, and the pod's loopback is shared by construction
		// (internal/render/pod.go's sidecarContainer already rejects a Unix
		// socket and a NetworkPolicy for this API, with reasons). See
		// docs/security.md's residual risks.
		ctx := context.Background()
		ns := h.NewNamespace(ctx, "e2e-isolation")

		class := h.CreateClass(ctx, WithPodmanEngine())
		env := h.CreateEnvironment(ctx, ns, class.Name, WithScript(
			"SCRIPT:docker run --rm --network host docker.io/library/alpine:3.21 wget -q -T 5 -O- http://127.0.0.1:9099/healthz",
			"SCRIPT:sandbox-done success loopback-reachable-as-expected",
		))
		key := client.ObjectKey{Namespace: ns, Name: env.Name}

		h.WaitForPhase(ctx, key, sandboxv1alpha1.PhaseDone, h.Cfg.PhaseTimeout)
		h.ExpectAgentExitCode(ctx, key, 0)
	})

	It("denies a DEFAULT-network workload container access to the pod's loopback", func() {
		// The positive-isolation counterpart: podman 5.x runs rootless
		// containers under pasta with the gateway unmapped, so a
		// default-network container cannot reach the pod's loopback
		// services. If this assertion ever flips on a podman bump that is a
		// REAL FINDING, not a flake -- re-read docs/security.md before
		// changing it.
		ctx := context.Background()
		ns := h.NewNamespace(ctx, "e2e-isolation")

		class := h.CreateClass(ctx, WithPodmanEngine())
		env := h.CreateEnvironment(ctx, ns, class.Name, WithScript(
			"SCRIPT:docker-expect-fail run --rm docker.io/library/alpine:3.21 wget -q -T 5 -O- http://127.0.0.1:9099/healthz",
			"SCRIPT:sandbox-done success loopback-blocked-as-expected",
		))
		key := client.ObjectKey{Namespace: ns, Name: env.Name}

		h.WaitForPhase(ctx, key, sandboxv1alpha1.PhaseDone, h.Cfg.PhaseTimeout)
		h.ExpectAgentExitCode(ctx, key, 0)
	})
})
