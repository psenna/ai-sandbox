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
		// rootless-podman posture, which never creates a pod). The pod is created at
		// the Ready->Restoring transition, AFTER the SlotScheduler admits the env --
		// which is after EngineSecurityRelaxed is first set at Pending->Ready -- so a
		// direct Get here would race pod creation and 404. Wait for the pod to exist
		// first; every sibling k8s-native spec below waits for PhaseRunning before
		// touching the pod for the same reason.
		Eventually(func() *corev1.Pod {
			return h.getAgentPodOrNil(ctx, key)
		}, h.Cfg.PodTimeout, h.Cfg.Poll).ShouldNot(BeNil(),
			"agent pod should be created by the k8s-native engine")
		pod := h.GetAgentPod(ctx, key)
		var names []string
		for _, c := range pod.Spec.Containers {
			names = append(names, c.Name)
		}
		sort.Strings(names)
		// The agent is the ONLY app container: k8s-native contributes no
		// engine-specific sidecar (a rootless-podman engine would add a "podman"
		// container here). The always-present sandboxctl control-channel sidecar
		// is a NATIVE sidecar (restartPolicy: Always) in initContainers, not a
		// regular container -- see render/pod.go's sidecarContainer and
		// pod_restore_test.go's init[0]==sandboxctl pin. Restore is also an init
		// container. Both are excluded from Spec.Containers.
		Expect(names).To(Equal([]string{"agent"}),
			"thin agent pod should have only the agent app container, got %v", names)
		var initNames []string
		for _, c := range pod.Spec.InitContainers {
			initNames = append(initNames, c.Name)
		}
		Expect(initNames).To(ContainElement("sandboxctl"),
			"agent pod should include the sandboxctl control-channel sidecar in initContainers, got %v", initNames)

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
		//
		// The TCP healthcheck on 5432 is load-bearing for the connect probe
		// below: without a healthcheck the ServiceSet pod has no readinessProbe,
		// so PodReady flips the moment the container starts -- before postgres
		// binds 5432 -- and waitServiceSetReady returns while the port is still
		// refusing (connect -> "Connection refused", a readiness race, not a
		// network-policy deny). The healthcheck makes PodReady mean "postgres is
		// accepting on 5432", so the probe runs only once the service is actually
		// connectable. This is the product's healthcheck opt-in doing its job,
		// not a workaround: a user who wants Ready to mean "reachable" declares a
		// probe, exactly as here.
		h.applyServices(ctx, ns, env.Name, `
services:
  - name: db
    image: postgres:17-alpine
    ports: [5432]
    env:
      POSTGRES_PASSWORD: secret
    healthcheck:
      tcp:
        port: 5432
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
			"SCRIPT:sandbox-exec-wait-ready probe",
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
			"SCRIPT:sandbox-exec-wait-ready probe",
			`SCRIPT:sandbox-exec probe wget -T 3 -qO- `+broker+`/healthz`,
			"SCRIPT:sandbox-done success open-egress-allowed",
			"SCRIPT:sleep 2",
			"SCRIPT:exit 0",
		))
		key := client.ObjectKey{Namespace: ns, Name: env.Name}
		h.WaitForPhase(ctx, key, sandboxv1alpha1.PhaseDone, h.Cfg.PhaseTimeout)
	})

	// Control-plane isolation: a runtime pod (separate pod, own netns) cannot
	// reach the agent's control API on the agent pod's loopback 127.0.0.1:9099.
	// The sidecar binds loopback only and is not exposed as a Service, so the
	// agent pod's IP:9099 is closed from any other pod. This replaces the podman
	// approach's "DEFAULT-network container denied the pod loopback" property;
	// under k8s-native it is true by pod separation, asserted end-to-end here.
	It("does not expose the control API to other pods", func() {
		class := h.CreateClass(ctx, WithEngine(sandboxv1alpha1.EngineTypeK8sNative))
		// Keep the env Running so the sidecar is up for the test to drive apply.
		env := h.CreateEnvironment(ctx, ns, class.Name, WithScript("SCRIPT:sleep 600"))
		key := client.ObjectKey{Namespace: ns, Name: env.Name}
		h.WaitForPhase(ctx, key, sandboxv1alpha1.PhaseRunning, h.Cfg.PhaseTimeout)

		// Apply a runtime named "probe" (alpine:3) so a separate pod exists.
		h.applyServices(ctx, ns, env.Name, `
runtimes:
  - name: probe
    image: alpine:3
    command: ["sleep", "infinity"]
`)
		// Wait for the probe pod to be running.
		Eventually(func() corev1.PodPhase {
			var pod corev1.Pod
			if err := h.Client.Get(ctx, client.ObjectKey{Name: "probe", Namespace: ns}, &pod); err != nil {
				return ""
			}
			return pod.Status.Phase
		}, h.Cfg.PodTimeout, h.Cfg.Poll).Should(Equal(corev1.PodRunning), "probe runtime pod never Running")

		// The agent pod's IP: the control API is loopback-bound, so reaching it
		// from the probe pod via the agent pod's IP must FAIL. alpine:3 ships
		// busybox wget (no bash /dev/tcp here, unlike the agent image); a refused
		// TCP connect (loopback-only bind -- nothing listens on the pod IP)
		// makes wget exit non-zero, which Exec surfaces as err.
		agentPod := h.GetAgentPod(ctx, key)
		agentIP := agentPod.Status.PodIP
		Expect(agentIP).NotTo(BeEmpty(), "agent pod has no IP")

		_, _, err := h.Exec(ctx, ns, "probe", "probe",
			"wget", "-T", "5", "-q", "-O", "/dev/null",
			"http://"+agentIP+":9099/healthz")
		Expect(err).To(HaveOccurred(),
			"runtime pod reached the agent control API at %s:9099 -- it is not loopback-isolated", agentIP)

		// Sanity: the control API IS reachable from the agent's OWN container
		// (loopback), proving it is up, not just absent.
		_, stderr, err := h.Exec(ctx, ns, render.ChildNames(env.Name).Pod, render.AgentContainerName,
			"curl", "-fsS", "--max-time", "5", "http://localhost:9099/healthz")
		Expect(err).NotTo(HaveOccurred(),
			"agent container could not reach its own control API on loopback: %s", stderr)
	})

	// Compose: `sandboxctl services compose` emits a docker-compose.yml
	// equivalent to the declaration (matching image/ports/env/depends_on).
	// Compose's pure render is unit-tested (Plan 2 Task 3); this spec exercises
	// the real binary path -- flag parsing, file read, stdout -- end-to-end.
	// The sidecar is distroless (no shell), so the declaration is written to the
	// shared workspace volume via the agent's shell and read by the sidecar CLI
	// from the same mounted path (same technique as applyServices). The
	// declaration omits healthcheck.tcp -- tcp has no compose equivalent
	// (compose only translates healthcheck.exec), so it would not round-trip.
	It("renders a docker-compose.yml equivalent to the declaration", func() {
		class := h.CreateClass(ctx, WithEngine(sandboxv1alpha1.EngineTypeK8sNative))
		env := h.CreateEnvironment(ctx, ns, class.Name, WithScript("SCRIPT:sleep 300"))
		key := client.ObjectKey{Namespace: ns, Name: env.Name}
		h.WaitForPhase(ctx, key, sandboxv1alpha1.PhaseRunning, h.Cfg.PhaseTimeout)

		servicesYAML := `services:
  - name: db
    image: postgres:17-alpine
    ports: [5432]
    env:
      POSTGRES_PASSWORD: secret
runtimes:
  - name: app
    image: python:3.11-alpine
    command: ["sleep", "infinity"]
    dependsOn: [db]
`
		podName := render.ChildNames(env.Name).Pod
		path := render.WorkspaceMountPath + "/.e2e-compose.yaml"
		// Write via the agent's shell to the shared workspace volume (the
		// distroless sidecar has no shell/cat to receive a heredoc).
		_, stderr, err := h.Exec(ctx, ns, podName, render.AgentContainerName, "sh", "-c",
			"cat > "+path+" <<'E2E_YAML'\n"+servicesYAML+"\nE2E_YAML")
		Expect(err).NotTo(HaveOccurred(), "writing compose.yaml into agent: %s", stderr)

		// Run the pure renderer in the sidecar; it reads the file from the
		// shared volume and writes docker-compose.yml to stdout.
		stdout, stderr, err := h.Exec(ctx, ns, podName, render.SidecarContainerName,
			"/sandboxctl", "services", "compose", path)
		Expect(err).NotTo(HaveOccurred(),
			"sandboxctl services compose failed: stderr=%q", stderr)

		// Assert the equivalent fields round-trip through the real binary.
		// Input uses dependsOn (camelCase); compose emits depends_on (snake).
		for _, want := range []string{
			"db:", // service name
			"postgres:17-alpine",
			"5432",
			"POSTGRES_PASSWORD: secret",
			"app:", // runtime name
			"python:3.11-alpine",
			"depends_on:", // dependsOn -> depends_on
		} {
			Expect(stdout).To(ContainSubstring(want),
				"compose output missing %q:\n%s", want, stdout)
		}
	})

	// Snapshot/teardown: freeze archives the workspace PVC; the teardown marker
	// lists the ServiceSet's pods in Destroyed.Pods (the new field, back-compatible
	// with Destroyed.Containers, which is empty for k8s-native -- there is no
	// in-pod container engine to tear down). The k8s-native teardown is LIST-ONLY
	// (reads the ServiceSet's Status.Entries, does NOT delete pods/PVCs), so the
	// marker honestly reports the pods present at freeze. The marker is written
	// by the snapshot freeze flow into <workspace>/.sandbox/last-freeze.json
	// DURING Freezing (before archive); read it from the agent's workspace while
	// the pod is still alive.
	It("writes Destroyed.Pods and archives the workspace", func() {
		class := h.CreateClass(ctx, WithEngine(sandboxv1alpha1.EngineTypeK8sNative))
		// Apply services+runtimes so the ServiceSet has pods, then declare a
		// 1h NotBefore wait to trigger Freezing. Keep running (sleep 300) so the
		// workspace persists for the test to read the marker during Freezing.
		env := h.CreateEnvironment(ctx, ns, class.Name, WithScript(
			`SCRIPT:sandbox-services-apply {"services":[{"name":"db","image":"postgres:17-alpine","ports":[5432],"env":{"POSTGRES_PASSWORD":"secret"}}],"runtimes":[{"name":"probe","image":"alpine:3","command":["sleep","infinity"]}]}`,
			"SCRIPT:sleep 15",
			`SCRIPT:sandbox-wait {"type":"NotBefore","reason":"e2e freeze","params":{"duration":"1h"}}`,
			"SCRIPT:sleep 300",
		))
		key := client.ObjectKey{Namespace: ns, Name: env.Name}
		h.WaitForPhase(ctx, key, sandboxv1alpha1.PhaseFreezing, h.Cfg.PhaseTimeout)

		// The ServiceSet has the db + probe pods; their names == entry names.
		// Asserting this before reading the marker guards that the entries the
		// list-only teardown reads are populated.
		entries := h.ServiceSetEntries(ctx, ns, env.Name)
		Expect(entries).To(ContainElements("db", "probe"),
			"ServiceSet should list db + probe entries before freeze, got %v", entries)

		// The teardown marker is written during Freezing, before the workspace is
		// archived. Read it from the agent container's workspace (polls for it).
		marker := h.readTeardownMarker(ctx, ns, env.Name)
		Expect(marker.Engine).To(Equal("k8s-native"),
			"marker engine should be k8s-native, got %q", marker.Engine)
		// Destroyed.Pods lists the ServiceSet's pods (db + probe), sorted.
		Expect(marker.Destroyed.Pods).To(ConsistOf("db", "probe"),
			"Destroyed.Pods should list db + probe, got %v", marker.Destroyed.Pods)
		// Back-compatible: Containers is empty for k8s-native (no in-pod engine).
		Expect(marker.Destroyed.Containers).To(BeEmpty(),
			"Destroyed.Containers should be empty for k8s-native, got %v", marker.Destroyed.Containers)
	})
})
