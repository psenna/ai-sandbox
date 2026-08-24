package e2e

import (
	"context"
	"fmt"
	"sort"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sandboxv1alpha1 "github.com/psenna/ai-sandbox/operator/api/v1alpha1"
	"github.com/psenna/ai-sandbox/operator/internal/render"
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

	// waitServiceSetReady waits until the env's ServiceSet has a Ready pod for
	// every entry named in `entries` (the reconciler writes per-entry Ready).
	waitServiceSetReady := func(ctx context.Context, ns, envName string, entries []string, timeout time.Duration) {
		Eventually(func() bool {
			var ss sandboxv1alpha1.ServiceSet
			if err := h.Client.Get(ctx, client.ObjectKey{Name: envName, Namespace: ns}, &ss); err != nil {
				return false
			}
			ready := map[string]bool{}
			for _, e := range ss.Status.Entries {
				if e.Ready {
					ready[e.Name] = true
				}
			}
			for _, want := range entries {
				if !ready[want] {
					return false
				}
			}
			return true
		}, timeout, h.Cfg.Poll).Should(BeTrue(), "ServiceSet %s/%s entries %v never all Ready", ns, envName, entries)
	}

	// podUID returns the name + UID of the ServiceSet pod named entryName
	// (pod name == entry name), or ("","") if it does not exist yet.
	podUID := func(ctx context.Context, ns, entryName string) (string, types.UID) {
		var pod corev1.Pod
		if err := h.Client.Get(ctx, client.ObjectKey{Name: entryName, Namespace: ns}, &pod); err != nil {
			return "", ""
		}
		return pod.Name, pod.UID
	}

	// A declared service is reachable via Service DNS (<name>.<ns>.svc).
	It("reaches a declared service via Service DNS", func() {
		class := h.CreateClass(ctx, WithEngine(sandboxv1alpha1.EngineTypeK8sNative))
		// Keep the env Running so the sidecar is up for the test to drive apply.
		env := h.CreateEnvironment(ctx, ns, class.Name, WithScript("SCRIPT:sleep 600"))
		key := client.ObjectKey{Namespace: ns, Name: env.Name}
		h.WaitForPhase(ctx, key, sandboxv1alpha1.PhaseRunning, h.Cfg.PhaseTimeout)

		// postgres:17-alpine is pre-loaded (e2e-up.sh). It listens on 5432 with
		// POSTGRES_PASSWORD set; the assertion is DNS resolve + TCP connect,
		// not a real SQL handshake.
		h.applyServices(ctx, ns, env.Name, `
services:
  - name: db
    image: postgres:17-alpine
    ports: [5432]
    env:
      POSTGRES_PASSWORD: secret
`)
		waitServiceSetReady(ctx, ns, env.Name, []string{"db"}, h.Cfg.PodTimeout)

		// Connect from the agent container (same namespace, so db.<ns>.svc
		// resolves) to the Service DNS name on its exposed port. busybox nc in
		// the alpine agent image lacks -z, so use bash's /dev/tcp connect probe
		// (bash is installed; timeout is in coreutils): exec 3<>/dev/tcp/HOST/P
		// opens a TCP socket and exits 0 on success.
		dbHost := fmt.Sprintf("db.%s.svc.cluster.local", ns)
		_, stderr, err := h.Exec(ctx, ns, render.ChildNames(env.Name).Pod, render.AgentContainerName,
			"timeout", "5", "bash", "-c", "exec 3<>/dev/tcp/"+dbHost+"/5432")
		Expect(err).NotTo(HaveOccurred(),
			"agent could not reach db.%s.svc:5432 via Service DNS: %s", ns, stderr)
	})

	// Version-switch (image change) recreates ONLY the changed pod; an unchanged
	// service pod is NOT recreated (same UID). Driven from the test so the two
	// applies are separated by a poll for the v1 UIDs.
	It("recreates only the changed pod on a version switch", func() {
		class := h.CreateClass(ctx, WithEngine(sandboxv1alpha1.EngineTypeK8sNative))
		env := h.CreateEnvironment(ctx, ns, class.Name, WithScript("SCRIPT:sleep 600"))
		key := client.ObjectKey{Namespace: ns, Name: env.Name}
		h.WaitForPhase(ctx, key, sandboxv1alpha1.PhaseRunning, h.Cfg.PhaseTimeout)

		// v1: postgres (stable) + python:3.11-alpine (the one we will switch).
		h.applyServices(ctx, ns, env.Name, `
services:
  - name: db
    image: postgres:17-alpine
    ports: [5432]
    env:
      POSTGRES_PASSWORD: secret
  - name: py
    image: python:3.11-alpine
    command: ["sleep", "infinity"]
`)
		waitServiceSetReady(ctx, ns, env.Name, []string{"db", "py"}, h.Cfg.PodTimeout)

		_, dbUIDv1 := podUID(ctx, ns, "db")
		_, pyUIDv1 := podUID(ctx, ns, "py")
		Expect(dbUIDv1).NotTo(BeEmpty(), "db pod UID v1")
		Expect(pyUIDv1).NotTo(BeEmpty(), "py pod UID v1")

		// v2: switch ONLY py to python:3.13-alpine. db unchanged.
		h.applyServices(ctx, ns, env.Name, `
services:
  - name: db
    image: postgres:17-alpine
    ports: [5432]
    env:
      POSTGRES_PASSWORD: secret
  - name: py
    image: python:3.13-alpine
    command: ["sleep", "infinity"]
`)

		// py pod recreated: it must exist again with a NEW, non-empty UID
		// (asserting only != pyUIDv1 would pass on the deletion intermediate,
		// where the pod is gone and the UID is "" -- masking a failed recreate).
		Eventually(func() bool {
			_, uid := podUID(ctx, ns, "py")
			return uid != "" && uid != pyUIDv1
		}, h.Cfg.PodTimeout, h.Cfg.Poll).Should(BeTrue(),
			"py pod should be recreated (new non-empty UID) on image change")

		Consistently(func() types.UID {
			_, uid := podUID(ctx, ns, "db")
			return uid
		}, 10*time.Second, h.Cfg.Poll).Should(Equal(dbUIDv1),
			"db pod should NOT be recreated when only py changed")
	})

	// Isolation: declare an alpine runtime and exec a wget against the
	// platform-doubles broker from inside it. Restricted -> egress BLOCKED
	// (wget fails -> expect-fail passes); Open -> egress ALLOWED (wget
	// succeeds -> plain exec passes). The probe hits the broker's
	// /healthz (200 {"status":"ok"}) -- the root path has no handler and
	// returns 404, which would make wget exit non-zero even when egress is
	// allowed and break the Open assertion. alpine:3 ships busybox wget.
	It("blocks egress to the broker under Restricted", func() {
		class := h.CreateClass(ctx,
			WithEngine(sandboxv1alpha1.EngineTypeK8sNative),
			WithNetworkIsolation(sandboxv1alpha1.NetworkIsolationRestricted),
		)
		broker := h.brokerURL()
		env := h.CreateEnvironment(ctx, ns, class.Name, WithScript(
			`SCRIPT:sandbox-services-apply {"runtimes":[{"name":"probe","image":"alpine:3","command":["sleep","infinity"]}]}`,
			"SCRIPT:sleep 10",
			`SCRIPT:sandbox-exec-expect-fail probe wget -T 3 -qO- `+broker+`/healthz`,
			"SCRIPT:sandbox-done success restricted-egress-blocked",
			"SCRIPT:sleep 2",
			"SCRIPT:exit 0",
		))
		key := client.ObjectKey{Namespace: ns, Name: env.Name}
		h.WaitForPhase(ctx, key, sandboxv1alpha1.PhaseDone, h.Cfg.PhaseTimeout)
	})

	It("allows egress to the broker under Open", func() {
		class := h.CreateClass(ctx,
			WithEngine(sandboxv1alpha1.EngineTypeK8sNative),
			WithNetworkIsolation(sandboxv1alpha1.NetworkIsolationOpen),
		)
		broker := h.brokerURL()
		env := h.CreateEnvironment(ctx, ns, class.Name, WithScript(
			`SCRIPT:sandbox-services-apply {"runtimes":[{"name":"probe","image":"alpine:3","command":["sleep","infinity"]}]}`,
			"SCRIPT:sleep 10",
			`SCRIPT:sandbox-exec probe wget -T 3 -qO- `+broker+`/healthz`,
			"SCRIPT:sandbox-done success open-egress-allowed",
			"SCRIPT:sleep 2",
			"SCRIPT:exit 0",
		))
		key := client.ObjectKey{Namespace: ns, Name: env.Name}
		h.WaitForPhase(ctx, key, sandboxv1alpha1.PhaseDone, h.Cfg.PhaseTimeout)
	})
})
