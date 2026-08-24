package render

import (
	"fmt"
	"sort"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	metav1ac "k8s.io/client-go/applyconfigurations/meta/v1"
	anetworkingv1 "k8s.io/client-go/applyconfigurations/networking/v1"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
)

// RenderNetworkPolicy renders the NetworkPolicy for one environment's agent
// pod. Pure: no client, no clock, deterministic ordering throughout.
//
//   - Open isolation: returns (nil, nil). The reconciler deletes any stale
//     NetworkPolicy (see ensureResources' deleteOwnedChild).
//   - Restricted isolation: returns one NetworkPolicyApplyConfiguration
//     named in.Names.NetworkPolicy in in.Env.Namespace, selecting the agent
//     pod by the sandbox.psenna.dev/environment label (labels.go) and
//     default-denying both ingress and egress except for the rules below.
//
// The egress rules are:
//
//  1. DNS to kube-dns -- FIXED in render, a deliberate, documented exception
//     to "render resolves nothing about the cluster": the kube-dns labels
//     (namespace kube-system, pod label k8s-app=kube-dns) are a universal
//     Kubernetes API convention, not cluster state, and every sandbox needs
//     DNS regardless of what the class declares.
//  2. The caller-resolved peers from in.Network.Egress (K8s API ipBlock,
//     platform service selectors, storage, extraEgress), emitted verbatim:
//     a ResolvedPeer with CIDR becomes an ipBlock peer; with Selector a
//     namespaceSelector+podSelector peer; Ports become the port list
//     (defaulting to TCP when a port's protocol is empty).
//
// The ingress rules are: if in.Network.OperatorIngress is non-nil, one rule
// allowing the operator's own pods (namespaceSelector on the operator's
// namespace + the operator's pod selector) to reach the sandbox pod on any
// port. If it is nil, no ingress rules are emitted -- an empty Ingress list
// with policyTypes: [Ingress] is default-deny ingress.
func RenderNetworkPolicy(in Inputs) (*anetworkingv1.NetworkPolicyApplyConfiguration, error) {
	if err := validateInputs(in); err != nil {
		return nil, err
	}
	if in.Class.Spec.Network.Isolation != v1alpha1.NetworkIsolationRestricted {
		return nil, nil // Open: no policy; the reconciler deletes any stale NP
	}
	names := ChildNames(in.Env.Name)

	// Egress rule 1: DNS to kube-dns (see the doc comment above).
	egress := []*anetworkingv1.NetworkPolicyEgressRuleApplyConfiguration{dnsEgressRule()}

	// Egress rule 2: the agent reaches other env-labeled pods in this
	// namespace -- the dependency/runtime pods the ServiceSet reconciles carry
	// the same env label (serviceset_controller.entryLabels), so the agent can
	// connect to deps via Service DNS and to runtimes by pod IP. A
	// podSelector-only peer (no namespaceSelector) selects pods in THIS
	// NetworkPolicy's namespace. No ports => all ports (the agent needs only the
	// declared service ports, but a single all-ports rule is simpler and the
	// namespace is already the trust boundary).
	egress = append(egress, envPodEgressRule(in.Env.Name))

	// Egress rule 3: the caller-resolved peers, sorted for determinism so two
	// renders are byte-identical. An empty Egress list is a controller bug
	// (the controller guarantees at least the K8s API peer), but render still
	// emits the DNS rule and does not error -- a DNS-only policy is a safe
	// failure mode, not a render failure.
	for _, p := range sortedPeers(in.Network.Egress) {
		if err := validateResolvedPeer(p); err != nil {
			return nil, err
		}
		egress = append(egress, peerEgressRule(p))
	}

	// Ingress: operator-only, or default-deny (empty list).
	var ingress []*anetworkingv1.NetworkPolicyIngressRuleApplyConfiguration
	if in.Network.OperatorIngress != nil {
		ingress = append(ingress, operatorIngressRule(in.Network.OperatorIngress))
	}

	return anetworkingv1.NetworkPolicy(names.NetworkPolicy, in.Env.Namespace).
		WithLabels(Labels(in.Env)).
		WithOwnerReferences(ownerReference(in.Env)).
		WithSpec(anetworkingv1.NetworkPolicySpec().
			WithPodSelector(metav1ac.LabelSelector().WithMatchLabels(map[string]string{
				"sandbox.psenna.dev/environment": EnvironmentLabelValue(in.Env.Name),
			})).
			WithPolicyTypes(networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress).
			WithIngress(ingress...).
			WithEgress(egress...)), nil
}

// dnsEgressRule is the fixed kube-dns egress rule every Restricted sandbox
// gets: namespaceSelector kube-system + podSelector k8s-app=kube-dns, ports
// 53 UDP and 53 TCP. The kube-dns labels are a universal Kubernetes API
// convention (not cluster state), so this is the one rule render hardcodes
// rather than resolving through the controller.
func dnsEgressRule() *anetworkingv1.NetworkPolicyEgressRuleApplyConfiguration {
	return anetworkingv1.NetworkPolicyEgressRule().
		WithTo(anetworkingv1.NetworkPolicyPeer().
			WithNamespaceSelector(metav1ac.LabelSelector().WithMatchLabels(map[string]string{"kubernetes.io/metadata.name": "kube-system"})).
			WithPodSelector(metav1ac.LabelSelector().WithMatchLabels(map[string]string{"k8s-app": "kube-dns"}))).
		WithPorts(
			anetworkingv1.NetworkPolicyPort().WithProtocol(corev1.ProtocolUDP).WithPort(intstr.FromInt(53)),
			anetworkingv1.NetworkPolicyPort().WithProtocol(corev1.ProtocolTCP).WithPort(intstr.FromInt(53)),
		)
}

// envPodEgressRule allows egress to pods in this namespace carrying the
// environment label (the agent + the ServiceSet's dep/runtime pods). A
// podSelector-only peer (no namespaceSelector) selects pods in THIS
// NetworkPolicy's namespace; no ports means all ports.
func envPodEgressRule(envName string) *anetworkingv1.NetworkPolicyEgressRuleApplyConfiguration {
	return anetworkingv1.NetworkPolicyEgressRule().
		WithTo(anetworkingv1.NetworkPolicyPeer().
			WithPodSelector(metav1ac.LabelSelector().WithMatchLabels(map[string]string{
				"sandbox.psenna.dev/environment": EnvironmentLabelValue(envName),
			})))
}

// peerEgressRule renders one caller-resolved peer as a single egress rule.
// Ports are emitted only when non-empty (empty = all ports).
func peerEgressRule(p ResolvedPeer) *anetworkingv1.NetworkPolicyEgressRuleApplyConfiguration {
	rule := anetworkingv1.NetworkPolicyEgressRule().WithTo(peerTo(p))
	if len(p.Ports) > 0 {
		rule = rule.WithPorts(convertPorts(p.Ports)...)
	}
	return rule
}

// peerTo renders one ResolvedPeer as a NetworkPolicyPeer: CIDR -> ipBlock,
// Selector -> namespaceSelector+podSelector (a nil PodSelector means all pods
// in the namespace).
func peerTo(p ResolvedPeer) *anetworkingv1.NetworkPolicyPeerApplyConfiguration {
	peer := anetworkingv1.NetworkPolicyPeer()
	if p.CIDR != "" {
		return peer.WithIPBlock(anetworkingv1.IPBlock().WithCIDR(p.CIDR))
	}
	peer = peer.WithNamespaceSelector(metav1ac.LabelSelector().WithMatchLabels(map[string]string{
		"kubernetes.io/metadata.name": p.Selector.Namespace,
	}))
	if ps := convertLabelSelector(p.Selector.PodSelector); ps != nil {
		peer = peer.WithPodSelector(ps)
	}
	return peer
}

// operatorIngressRule renders the single ingress rule allowing the operator's
// own pods (in the operator's namespace, selected by OperatorIngress's
// PodSelector) to reach the sandbox pod on any port.
func operatorIngressRule(op *v1alpha1.PeerSelector) *anetworkingv1.NetworkPolicyIngressRuleApplyConfiguration {
	peer := anetworkingv1.NetworkPolicyPeer().
		WithNamespaceSelector(metav1ac.LabelSelector().WithMatchLabels(map[string]string{
			"kubernetes.io/metadata.name": op.Namespace,
		}))
	if ps := convertLabelSelector(op.PodSelector); ps != nil {
		peer = peer.WithPodSelector(ps)
	}
	return anetworkingv1.NetworkPolicyIngressRule().WithFrom(peer)
}

// convertPorts projects v1alpha1 NetworkPolicyPorts into their apply-
// configuration equivalents. A port with an empty Protocol defaults to TCP
// (the plan's convention: the CRD enum allows TCP/UDP/SCTP, and an explicit
// protocol is always emitted so the rendered policy is unambiguous).
func convertPorts(ports []v1alpha1.NetworkPolicyPort) []*anetworkingv1.NetworkPolicyPortApplyConfiguration {
	out := make([]*anetworkingv1.NetworkPolicyPortApplyConfiguration, 0, len(ports))
	for _, p := range ports {
		proto := corev1.ProtocolTCP
		if p.Protocol != "" {
			proto = corev1.Protocol(p.Protocol)
		}
		out = append(out, anetworkingv1.NetworkPolicyPort().
			WithProtocol(proto).
			WithPort(portValue(p.Port)))
	}
	return out
}

// portValue converts a v1alpha1 port string into the intstr form the
// NetworkPolicy API accepts: a purely numeric string becomes an int port
// (the API rejects a numeric string as a named port -- "must contain at
// least one letter"), anything else is a named port.
func portValue(s string) intstr.IntOrString {
	if n, err := strconv.Atoi(s); err == nil {
		return intstr.FromInt(n)
	}
	return intstr.FromString(s)
}

// convertLabelSelector projects a metav1.LabelSelector into its
// apply-configuration equivalent, or nil when sel is nil.
func convertLabelSelector(sel *metav1.LabelSelector) *metav1ac.LabelSelectorApplyConfiguration {
	if sel == nil {
		return nil
	}
	out := metav1ac.LabelSelector()
	if len(sel.MatchLabels) > 0 {
		out = out.WithMatchLabels(sel.MatchLabels)
	}
	for _, e := range sel.MatchExpressions {
		out = out.WithMatchExpressions(metav1ac.LabelSelectorRequirement().
			WithKey(e.Key).
			WithOperator(e.Operator).
			WithValues(e.Values...))
	}
	return out
}

// sortedPeers returns peers sorted by (CIDR, then namespace) so two renders
// of the same Inputs are byte-identical regardless of the caller's slice
// order. Selector peers sort by their namespace; CIDR peers sort by CIDR.
func sortedPeers(peers []ResolvedPeer) []ResolvedPeer {
	out := make([]ResolvedPeer, len(peers))
	copy(out, peers)
	sort.SliceStable(out, func(i, j int) bool {
		ki, kj := peerSortKey(out[i]), peerSortKey(out[j])
		return ki < kj
	})
	return out
}

func peerSortKey(p ResolvedPeer) string {
	if p.CIDR != "" {
		return "cidr:" + p.CIDR
	}
	if p.Selector != nil {
		return "selector:" + p.Selector.Namespace
	}
	return "selector:"
}

// validateResolvedPeer enforces the "exactly one of CIDR or Selector"
// invariant defensively. The CRD's XValidation should catch a malformed
// EgressPeer before it ever reaches render, but render validates too (the
// same philosophy as validateInputs): a ResolvedPeer with both set, or
// neither set, is a controller bug that must fail loudly rather than render a
// policy that silently allows nothing (or everything).
func validateResolvedPeer(p ResolvedPeer) error {
	hasCIDR := p.CIDR != ""
	hasSelector := p.Selector != nil
	if hasCIDR == hasSelector {
		return fmt.Errorf("render: network peer must set exactly one of CIDR or Selector (got CIDR=%q Selector=%v)", p.CIDR, p.Selector != nil)
	}
	return nil
}
