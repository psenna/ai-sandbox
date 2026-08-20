package controller

import (
	"context"
	"fmt"
	"net"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

// probePodLabel is the label identifying the CNI probe's own pods and
// NetworkPolicy, so the probe never touches anything else in the operator's
// namespace.
const probePodLabel = "sandbox.psenna.dev/cni-probe"

const (
	// probePodTimeout bounds how long a probe pod may take to reach a
	// terminal phase (image pull + scheduling + the dial itself).
	probePodTimeout = 60 * time.Second
	probePoll       = time.Second

	// probeServerPort is the TCP port the probe's server pod listens on. The
	// probe pods run the operator's own distroless image with only the
	// sandboxctl binary, which listens on nothing by default, so 8080 is
	// free inside them.
	probeServerPort = "8080"

	// probeServerSettle waits after the server pod reaches Running+IP before
	// the baseline dial, so the listener is bound and the CNI has programmed
	// routing for the new pod.
	probeServerSettle = 2 * time.Second

	// probePolicySettle waits after the default-deny NetworkPolicy is applied
	// before the deny measurement: the CNI programs the policy asynchronously,
	// and a dial in the un-programmed window would read as "not enforced".
	// kindnetd programs in ~1s; 5s covers it with margin, and a pass that
	// still races is corrected by the next periodic pass (the interval is 5m
	// by default).
	probePolicySettle = 5 * time.Second
)

// CNIProbeResult is the shared result of the CNI enforcement probe (#31),
// published by the CNIProbeRunnable and read by the reconciler to decide the
// CNIEnforcement condition and the not-enforced warning event. Reason carries
// one of the ReasonCNI* constants so callers route on it rather than on
// Detail's presence:
//
//   - Enforced=true, Reason=ReasonCNIEnforced       -> the CNI enforces.
//   - Enforced=false, Reason=ReasonCNINotEnforced   -> the probe ran and
//     confirmed the CNI does NOT enforce (default-deny did not block). This
//     is the loud, real "not enforced" case, distinct from a transient
//     failure.
//   - Enforced=false, Reason=ReasonCNIProbeFailed   -> the probe could not run
//     to completion (pod create failed, timeout, cleanup, ...). Enforcement
//     is unconfirmed, not measured false.
//
// The shared result is published via atomic.Pointer[CNIProbeResult] so the
// leader-elected Runnable goroutine and the Reconciler goroutines never race
// on the same pointer: the Runnable publishes a whole new struct each pass
// (Store), and the Reconciler reads the latest (Load). A nil pointer means the
// probe has not run yet.
type CNIProbeResult struct {
	Enforced bool
	Reason   string
	Detail   string
}

// CNIProbeRunnable is a manager.Runnable that periodically probes whether the
// cluster's CNI actually enforces NetworkPolicies (#31). Leader-elected so
// exactly one instance ever runs cluster-wide, matching SlotScheduler and
// WarmCacheGC.
type CNIProbeRunnable struct {
	Client    client.Client
	Namespace string // operator's own namespace; the probe pods run here
	Image     string // operator's own image (SidecarImage); the probe pods run it
	Interval  time.Duration
	// Result is the shared, atomic probe result. The Runnable publishes a
	// fresh struct each pass via Store; the Reconciler reads via Load. A nil
	// pointer (Load returns nil) means the probe has not run yet.
	Result *atomic.Pointer[CNIProbeResult]
	// probeFn is the probe implementation, overridable for tests. nil defaults
	// to ProbeCNIEnforcement, so production wiring is unchanged.
	probeFn func(ctx context.Context, c client.Client, namespace, image string) (CNIProbeResult, error)
}

var (
	_ manager.Runnable               = (*CNIProbeRunnable)(nil)
	_ manager.LeaderElectionRunnable = (*CNIProbeRunnable)(nil)
)

// NeedLeaderElection ensures exactly one instance of this loop ever runs
// cluster-wide: two probes writing the shared Result concurrently would race
// on the same pointer. Stated explicitly, matching SlotScheduler.
func (p *CNIProbeRunnable) NeedLeaderElection() bool { return true }

// Start runs one probe pass immediately, then one per Interval, until ctx is
// cancelled. A failed pass is logged, never returned -- returning an error
// here would take the whole manager down for what is very likely a transient
// failure, and the Result is still written (Enforced=false with the error as
// Detail) so the reconciler's condition and warning stay honest.
func (p *CNIProbeRunnable) Start(ctx context.Context) error {
	log := ctrl.LoggerFrom(ctx).WithName("cniprobe")
	p.runAndStore(ctx, log)
	ticker := time.NewTicker(p.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			p.runAndStore(ctx, log)
		}
	}
}

func (p *CNIProbeRunnable) runAndStore(ctx context.Context, log logr.Logger) {
	probe := p.probeFn
	if probe == nil {
		probe = ProbeCNIEnforcement
	}
	res, err := probe(ctx, p.Client, p.Namespace, p.Image)
	if err != nil {
		log.Error(err, "CNI enforcement probe failed")
		// Transient/unconfirmed: the probe could not run to completion. Publish
		// a whole new struct (never mutate in place) so concurrent Reconciler
		// readers see a consistent value. Enforced=false with
		// Reason=ReasonCNIProbeFailed routes the condition to Unknown -- we
		// do not claim non-enforcement from a failure to probe.
		p.Result.Store(&CNIProbeResult{Enforced: false, Reason: ReasonCNIProbeFailed, Detail: err.Error()})
		return
	}
	p.Result.Store(&res)
	log.V(1).Info("CNI enforcement probe", "enforced", res.Enforced, "reason", res.Reason, "detail", res.Detail)
}

// ProbeCNIEnforcement runs one enforcement probe pass and returns the result.
// It mirrors test/e2e/netpolicy.go's AssertNetworkPolicyEnforced gate: a
// positive baseline (a probe pod reaches a peer pod with no policy), then a
// negative after a default-deny NetworkPolicy is applied (a probe pod can no
// longer reach that peer).
//
// The measurement is deliberately POD-TO-POD, not pod-to-API-server: the API
// server runs on the host network on kind/k3s/kubeadm-docker clusters, and
// several CNIs (kindnetd among them) do not enforce NetworkPolicy egress to
// host-network destinations -- so a default-deny never blocks a dial to the
// `kubernetes` Service, and the probe would report NotEnforced on a cluster
// that enforces correctly (observed on kind during development of #31). A
// pod-to-pod dial exercises exactly the traffic path the CNI enforces, which
// is what Restricted isolation's NetworkPolicy actually governs. The e2e
// gate's server/client pair does the same.
//
// The server peer is a probe pod running the operator's own distroless image
// with the sandboxctl probe-listen subcommand (no shell, no nc); the client
// peers dial its PodIP with probe-tcp. The probe runs in the operator's own
// namespace (the caller's Namespace) and cleans up after itself; a crashed
// previous pass's leftovers are removed at the start of the next pass.
func ProbeCNIEnforcement(ctx context.Context, c client.Client, namespace, image string) (CNIProbeResult, error) {
	probeCleanup(ctx, c, namespace)

	// 1. Server: a probe pod listening on the probe port, staying alive for
	// the whole pass. The client dials its PodIP -- the pod-to-pod path.
	serverIP, err := probeServer(ctx, c, namespace, image)
	if err != nil {
		probeCleanup(ctx, c, namespace)
		return CNIProbeResult{}, err
	}
	target := net.JoinHostPort(serverIP, probeServerPort)

	// 2. Positive baseline: no policy, a probe pod must reach the server.
	baselineOK, err := probeOnce(ctx, c, namespace, image, "cni-probe-baseline", target)
	if err != nil {
		probeCleanup(ctx, c, namespace)
		return CNIProbeResult{}, err
	}
	if !baselineOK {
		probeCleanup(ctx, c, namespace)
		return CNIProbeResult{}, fmt.Errorf("baseline connectivity failed: a probe pod could not reach its peer pod (%s) with no NetworkPolicy applied; cannot distinguish an enforcing CNI from a broken one", target)
	}

	// 3. Apply a default-deny NetworkPolicy selecting the probe pods.
	deny := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "cni-probe-deny", Namespace: namespace},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{probePodLabel: "true"}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
		},
	}
	if err := c.Create(ctx, deny); err != nil {
		probeCleanup(ctx, c, namespace)
		return CNIProbeResult{}, fmt.Errorf("applying default-deny NetworkPolicy: %w", err)
	}

	// 4. Negative: a probe pod must now be blocked. Settle first so the CNI
	// has programmed the policy (see probePolicySettle).
	time.Sleep(probePolicySettle)
	denyOK, err := probeOnce(ctx, c, namespace, image, "cni-probe-deny", target)
	if err != nil {
		probeCleanup(ctx, c, namespace)
		return CNIProbeResult{}, err
	}
	probeCleanup(ctx, c, namespace)

	// A successful probe distinguishes the two real outcomes by a nil error
	// carrying a structured Reason, so the condition routes on Reason rather
	// than on Detail presence (Defect 1):
	//   - denyOK==true  -> the default-deny did NOT block, so the CNI does NOT
	//     enforce. Return a nil error with Reason=ReasonCNINotEnforced; this is
	//     a confirmed measurement, NOT a probe failure.
	//   - denyOK==false -> the default-deny blocked, so the CNI enforces.
	if denyOK {
		return CNIProbeResult{
			Enforced: false,
			Reason:   ReasonCNINotEnforced,
			Detail:   "connectivity was not blocked after a default-deny NetworkPolicy; this cluster's CNI does not enforce NetworkPolicy",
		}, nil
	}
	return CNIProbeResult{Enforced: true, Reason: ReasonCNIEnforced, Detail: "baseline reachable, blocked after default-deny"}, nil
}

// probeOnce creates one probe pod that dials target and exits 0/1, waits for
// it to reach a terminal phase, and reports whether it succeeded (Succeeded =
// the dial connected, Failed = it did not). The pod carries no ServiceAccount
// token -- it needs no API access, and the operator's token must not ride
// along in a throwaway probe pod.
func probeOnce(ctx context.Context, c client.Client, namespace, image, name, target string) (bool, error) {
	automount := false
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    map[string]string{probePodLabel: "true"},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                corev1.RestartPolicyNever,
			AutomountServiceAccountToken: &automount,
			Containers: []corev1.Container{{
				Name:    "probe",
				Image:   image,
				Command: []string{"/sandboxctl", "probe-tcp", target},
			}},
		},
	}
	if err := c.Create(ctx, pod); err != nil {
		return false, fmt.Errorf("creating probe pod %s: %w", name, err)
	}
	phase, err := waitForTerminalPod(ctx, c, client.ObjectKey{Namespace: namespace, Name: name})
	if err != nil {
		return false, err
	}
	return phase == corev1.PodSucceeded, nil
}

// probeServer creates the probe's listening peer pod (sandboxctl
// probe-listen on probeServerPort), waits for it to be Running with a PodIP,
// and returns the IP. The pod carries no ServiceAccount token -- it needs no
// API access, and the operator's token must not ride along in a throwaway
// probe pod.
func probeServer(ctx context.Context, c client.Client, namespace, image string) (string, error) {
	automount := false
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cni-probe-server",
			Namespace: namespace,
			Labels:    map[string]string{probePodLabel: "true"},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                corev1.RestartPolicyNever,
			AutomountServiceAccountToken: &automount,
			Containers: []corev1.Container{{
				Name:    "probe",
				Image:   image,
				Command: []string{"/sandboxctl", "probe-listen", probeServerPort},
			}},
		},
	}
	if err := c.Create(ctx, pod); err != nil {
		return "", fmt.Errorf("creating probe server pod: %w", err)
	}
	ip, err := waitForRunningPodIP(ctx, c, client.ObjectKey{Namespace: namespace, Name: pod.Name})
	if err != nil {
		return "", err
	}
	// Let the listener bind and the CNI program routing for the new pod
	// before the client dials (see probeServerSettle).
	time.Sleep(probeServerSettle)
	return ip, nil
}

// waitForRunningPodIP polls pod until it is Running with a PodIP assigned,
// returning the IP. A pod that never reaches that state within
// probePodTimeout is an error. The server pod is never terminal, so this
// wait (unlike waitForTerminalPod) watches for the Running phase + IP pair.
func waitForRunningPodIP(ctx context.Context, c client.Client, key client.ObjectKey) (string, error) {
	deadline := time.Now().Add(probePodTimeout)
	for {
		var pod corev1.Pod
		if err := c.Get(ctx, key, &pod); err != nil {
			if apierrors.IsNotFound(err) && time.Now().Before(deadline) {
				time.Sleep(probePoll)
				continue
			}
			return "", fmt.Errorf("reading probe server pod %s: %w", key.Name, err)
		}
		if pod.Status.Phase == corev1.PodRunning && pod.Status.PodIP != "" {
			return pod.Status.PodIP, nil
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("probe server pod %s did not become Running with a PodIP within %s (phase %s)", key.Name, probePodTimeout, pod.Status.Phase)
		}
		time.Sleep(probePoll)
	}
}

// waitForTerminalPod polls pod until it reaches a terminal phase (Succeeded
// or Failed), returning the phase. A pod that never becomes terminal within
// probePodTimeout is an error.
func waitForTerminalPod(ctx context.Context, c client.Client, key client.ObjectKey) (corev1.PodPhase, error) {
	deadline := time.Now().Add(probePodTimeout)
	for {
		var pod corev1.Pod
		if err := c.Get(ctx, key, &pod); err != nil {
			if apierrors.IsNotFound(err) && time.Now().Before(deadline) {
				time.Sleep(probePoll)
				continue
			}
			return "", fmt.Errorf("reading probe pod %s: %w", key.Name, err)
		}
		switch pod.Status.Phase {
		case corev1.PodSucceeded, corev1.PodFailed:
			return pod.Status.Phase, nil
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("probe pod %s did not reach a terminal phase within %s (phase %s)", key.Name, probePodTimeout, pod.Status.Phase)
		}
		time.Sleep(probePoll)
	}
}

// probeCleanup deletes every object the probe may have left behind from a
// previous pass, ignoring NotFound. Best-effort: a cleanup failure is logged,
// not returned -- the next pass's cleanup retries it.
func probeCleanup(ctx context.Context, c client.Client, namespace string) {
	for _, name := range []string{"cni-probe-server", "cni-probe-baseline", "cni-probe-deny"} {
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
		if err := c.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
			ctrl.LoggerFrom(ctx).V(1).Info("cni probe cleanup: pod delete failed", "name", name, "error", err)
		}
	}
	np := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: "cni-probe-deny", Namespace: namespace}}
	if err := c.Delete(ctx, np); err != nil && !apierrors.IsNotFound(err) {
		ctrl.LoggerFrom(ctx).V(1).Info("cni probe cleanup: networkpolicy delete failed", "error", err)
	}
}
