package e2e

import (
	"context"
	"fmt"
	"time"

	gomega "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// NetworkPolicyGateResult records the outcome of AssertNetworkPolicyEnforced,
// so netpolicy_test.go can report it as a visible spec without re-running
// the (expensive, ~25-30s) gate sequence.
type NetworkPolicyGateResult struct {
	Enforced bool   `json:"enforced"`
	Detail   string `json:"detail"`
}

// networkPolicyGateResult is populated in every Ginkgo process by
// suite_test.go's SynchronizedBeforeSuite all-processes function, which
// JSON-decodes the []byte process 1's function returned (see
// AssertNetworkPolicyEnforced) -- NOT set directly by
// AssertNetworkPolicyEnforced itself, which runs ONLY in process 1 under
// --procs>1 and so cannot write a package-level var any other process would
// ever observe (separate OS processes, no shared memory).
var networkPolicyGateResult NetworkPolicyGateResult

const gateServerPort = 8080

// AssertNetworkPolicyEnforced proves this cluster's CNI actually enforces
// NetworkPolicy using a real positive+negative connectivity pair, and
// aborts the whole suite (via Gomega's fail handler, which
// SynchronizedBeforeSuite propagates as a suite failure) if it does not.
// Every later "isolation" assertion in this suite is meaningless if this
// gate does not hold, so it must run once, first, and hard. Called only
// from process 1 (see suite_test.go); the returned result must be threaded
// through SynchronizedBeforeSuite's []byte channel to every other process,
// NOT read from the networkPolicyGateResult package var directly (that var
// is process-1-local at the point this function returns).
func AssertNetworkPolicyEnforced(ctx context.Context, h *Harness) NetworkPolicyGateResult {
	ns := h.NewNamespace(ctx, "e2e-cni-gate")

	serverPodSpec := gatePod(h.Cfg.AgentImage, ns, "server", "app", "server", `while true; do printf 'HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nhi' | nc -l -p 8080; done`)
	clientPodSpec := gatePod(h.Cfg.AgentImage, ns, "client", "app", "client", "sleep 3600")
	gomega.Expect(h.Client.Create(ctx, serverPodSpec)).To(gomega.Succeed())
	gomega.Expect(h.Client.Create(ctx, clientPodSpec)).To(gomega.Succeed())

	h.waitPodReady(ctx, client.ObjectKey{Namespace: ns, Name: "server"}, h.Cfg.PodTimeout)
	h.waitPodReady(ctx, client.ObjectKey{Namespace: ns, Name: "client"}, h.Cfg.PodTimeout)

	var serverPod corev1.Pod
	gomega.Expect(h.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: "server"}, &serverPod)).To(gomega.Succeed())
	serverIP := serverPod.Status.PodIP
	gomega.Expect(serverIP).NotTo(gomega.BeEmpty(), "server pod has no IP")

	probe := func() error {
		_, _, err := h.Exec(ctx, ns, "client", "client", "nc", "-z", "-w", "3", serverIP, fmt.Sprintf("%d", gateServerPort))
		return err
	}

	// 1. Positive baseline (no policy yet): must succeed. If this fails,
	// the baseline itself is broken -- we cannot distinguish an enforcing
	// CNI from a broken one.
	gomega.Expect(probe()).To(gomega.Succeed(),
		"NetworkPolicy gate: baseline connectivity (no policy applied) failed -- cannot distinguish an enforcing CNI from a broken one")

	// 2. Apply default-deny.
	denyAll := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "default-deny", Namespace: ns},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
		},
	}
	gomega.Expect(h.Client.Create(ctx, denyAll)).To(gomega.Succeed())

	// 3. Negative: must start failing, and keep failing.
	gomega.Eventually(probe, 60*time.Second, 2*time.Second).Should(gomega.HaveOccurred(),
		"NetworkPolicy gate: this cluster's CNI does NOT enforce NetworkPolicy -- every isolation assertion in this suite would silently pass. "+
			"Check the kindnetd image version, or re-run with E2E_CNI=calico")
	gomega.Consistently(probe, 10*time.Second, 2*time.Second).Should(gomega.HaveOccurred(),
		"NetworkPolicy gate: connectivity flapped back after the default-deny policy was applied -- enforcement is not stable")

	// 4. Allow client->server on 8080, both directions of the policy.
	allowIngress := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "allow-client-to-server", Namespace: ns},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "server"}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				From: []networkingv1.NetworkPolicyPeer{{
					PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "client"}},
				}},
				Ports: []networkingv1.NetworkPolicyPort{{Port: intPtr(gateServerPort)}},
			}},
		},
	}
	allowEgress := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "allow-client-egress", Namespace: ns},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "client"}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress: []networkingv1.NetworkPolicyEgressRule{{
				To: []networkingv1.NetworkPolicyPeer{{
					PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "server"}},
				}},
				Ports: []networkingv1.NetworkPolicyPort{{Port: intPtr(gateServerPort)}},
			}},
		},
	}
	gomega.Expect(h.Client.Create(ctx, allowIngress)).To(gomega.Succeed())
	gomega.Expect(h.Client.Create(ctx, allowEgress)).To(gomega.Succeed())

	// 5. Positive again: proves the policies are selective, not a blanket
	// drop that happens to look like enforcement.
	gomega.Eventually(probe, 60*time.Second, 2*time.Second).Should(gomega.Succeed(),
		"NetworkPolicy gate: connectivity did not recover after allow policies were applied -- the CNI may be blanket-dropping traffic rather than selectively enforcing policy")

	// 6. Egress enforcement: a cluster-internal destination not in any
	// allow rule (MinIO) must still be unreachable from the client.
	minioIP := h.serviceClusterIP(ctx, h.Cfg.ServicesNamespace, "minio")
	if minioIP != "" {
		_, _, err := h.Exec(ctx, ns, "client", "client", "nc", "-z", "-w", "3", minioIP, "9000")
		gomega.Expect(err).To(gomega.HaveOccurred(),
			"NetworkPolicy gate: client could reach MinIO (%s:9000), which is outside any allow rule -- egress is not actually restricted", minioIP)
	}

	gomega.Expect(h.Client.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(gomega.Succeed())

	return NetworkPolicyGateResult{Enforced: true, Detail: "positive/negative/positive-after-allow/egress-blocked sequence passed"}
}

// gatePod builds a minimal raw pod for the NetworkPolicy gate: command is
// set directly (bypassing the SCRIPT: directive protocol entirely, and the
// image's own entrypoint.sh with it), since this is infrastructure
// connectivity testing, not an agent-lifecycle spec.
func gatePod(image, ns, name, labelKey, labelVal, script string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels:    map[string]string{labelKey: labelVal},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{{
				Name:    name,
				Image:   image,
				Command: []string{"sh", "-c", script},
			}},
		},
	}
}

func intPtr(p int) *intstr.IntOrString {
	v := intstr.FromInt(p)
	return &v
}

func (h *Harness) waitPodReady(ctx context.Context, key client.ObjectKey, timeout time.Duration) {
	gomega.Eventually(func() corev1.ConditionStatus {
		var pod corev1.Pod
		if err := h.Client.Get(ctx, key, &pod); err != nil {
			return corev1.ConditionFalse
		}
		for _, c := range pod.Status.Conditions {
			if c.Type == corev1.PodReady {
				return c.Status
			}
		}
		return corev1.ConditionFalse
	}, timeout, h.Cfg.Poll).Should(gomega.Equal(corev1.ConditionTrue), "pod %s never became Ready", key)
}

func (h *Harness) serviceClusterIP(ctx context.Context, ns, name string) string {
	var svc corev1.Service
	if err := h.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &svc); err != nil {
		return ""
	}
	return svc.Spec.ClusterIP
}
