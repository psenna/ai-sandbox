package render

import (
	"reflect"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	anetworkingv1 "k8s.io/client-go/applyconfigurations/networking/v1"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
)

// restrictedClass returns minimalClass with Restricted isolation (the CRD
// default, but the Go zero value is "" so tests must set it explicitly).
func restrictedClass() *v1alpha1.SandboxClass {
	c := minimalClass()
	c.Spec.Network.Isolation = v1alpha1.NetworkIsolationRestricted
	return c
}

// restrictedInputs builds a Restricted Inputs with two resolved egress peers
// (one selector, one CIDR) and an operator ingress selector.
func restrictedInputs() Inputs {
	return Inputs{
		Env:       baseEnv("restricted-env"),
		Class:     restrictedClass(),
		ClusterID: "test-cluster",
		Network: NetworkInputs{
			Egress: []ResolvedPeer{
				{Selector: &v1alpha1.PeerSelector{
					Namespace:   "ai-sandbox",
					PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "git-proxy"}},
				}},
				{CIDR: "140.82.112.0/20", Ports: []v1alpha1.NetworkPolicyPort{{Port: "443"}}},
			},
			OperatorIngress: &v1alpha1.PeerSelector{
				Namespace:   "ai-sandbox-operator-system",
				PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"control-plane": "controller-manager"}},
			},
		},
	}
}

func TestRenderNetworkPolicy_OpenReturnsNil(t *testing.T) {
	class := minimalClass()
	class.Spec.Network.Isolation = v1alpha1.NetworkIsolationOpen
	in := Inputs{Env: baseEnv("open-env"), Class: class, ClusterID: "test-cluster"}

	np, err := RenderNetworkPolicy(in)
	if err != nil {
		t.Fatalf("RenderNetworkPolicy: %v", err)
	}
	if np != nil {
		t.Errorf("Open isolation: got a NetworkPolicy, want nil")
	}
}

func TestRenderNetworkPolicy_Restricted(t *testing.T) {
	np, err := RenderNetworkPolicy(restrictedInputs())
	if err != nil {
		t.Fatalf("RenderNetworkPolicy: %v", err)
	}
	if np == nil {
		t.Fatal("Restricted isolation: got nil NetworkPolicy")
	}

	assertNPIdentity(t, np)
	spec := np.Spec
	if spec == nil {
		t.Fatal("Spec is nil")
	}
	assertNPSpec(t, spec)
	assertNPEgress(t, spec.Egress)
	assertNPIngress(t, spec.Ingress)
}

// assertNPIdentity checks the rendered policy's name, namespace, labels and
// owner reference -- the shape every child object shares.
func assertNPIdentity(t *testing.T, np *anetworkingv1.NetworkPolicyApplyConfiguration) {
	t.Helper()
	names := ChildNames("restricted-env")
	if np.Name == nil || *np.Name != names.NetworkPolicy {
		t.Errorf("Name = %v, want %q", np.Name, names.NetworkPolicy)
	}
	if np.Namespace == nil || *np.Namespace != "default" {
		t.Errorf("Namespace = %v, want default", np.Namespace)
	}
	wantLabels := Labels(baseEnv("restricted-env"))
	for k, v := range wantLabels {
		if np.Labels[k] != v {
			t.Errorf("label %s = %q, want %q", k, np.Labels[k], v)
		}
	}
	if len(np.OwnerReferences) != 1 || np.OwnerReferences[0].Name == nil || *np.OwnerReferences[0].Name != "restricted-env" {
		t.Errorf("OwnerReferences = %+v, want a single controller ref to restricted-env", np.OwnerReferences)
	}
}

// assertNPSpec checks the pod selector and the [Ingress Egress] policy types.
func assertNPSpec(t *testing.T, spec *anetworkingv1.NetworkPolicySpecApplyConfiguration) {
	t.Helper()
	if spec.PodSelector == nil || spec.PodSelector.MatchLabels["sandbox.psenna.dev/environment"] != EnvironmentLabelValue("restricted-env") {
		t.Errorf("PodSelector = %+v, want sandbox.psenna.dev/environment=%s", spec.PodSelector, EnvironmentLabelValue("restricted-env"))
	}
	if len(spec.PolicyTypes) != 2 ||
		spec.PolicyTypes[0] != networkingv1.PolicyTypeIngress ||
		spec.PolicyTypes[1] != networkingv1.PolicyTypeEgress {
		t.Errorf("PolicyTypes = %v, want [Ingress Egress]", spec.PolicyTypes)
	}
}

// assertNPEgress checks the four egress rules: rule 0 is the fixed kube-dns
// rule; rule 1 is the env-pod egress rule (agent -> env-labeled pods); rules
// 2-3 are the resolved peers (sorted: CIDR peers first, then selector peers by
// namespace).
func assertNPEgress(t *testing.T, egress []anetworkingv1.NetworkPolicyEgressRuleApplyConfiguration) {
	t.Helper()
	if len(egress) != 4 {
		t.Fatalf("Egress has %d rules, want 4 (kube-dns + env + 2 peers)", len(egress))
	}
	dns := egress[0]
	if len(dns.To) != 1 || dns.To[0].NamespaceSelector == nil || dns.To[0].PodSelector == nil {
		t.Fatalf("kube-dns rule To = %+v, want namespaceSelector+podSelector", dns.To)
	}
	if dns.To[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "kube-system" {
		t.Errorf("kube-dns namespaceSelector = %+v, want kube-system", dns.To[0].NamespaceSelector)
	}
	if dns.To[0].PodSelector.MatchLabels["k8s-app"] != "kube-dns" {
		t.Errorf("kube-dns podSelector = %+v, want k8s-app=kube-dns", dns.To[0].PodSelector)
	}
	if len(dns.Ports) != 2 {
		t.Errorf("kube-dns rule has %d ports, want 2 (53/UDP + 53/TCP)", len(dns.Ports))
	}

	envRule := egress[1]
	if len(envRule.To) != 1 || envRule.To[0].PodSelector == nil || envRule.To[0].NamespaceSelector != nil {
		t.Fatalf("env-pod rule To = %+v, want podSelector-only (no namespaceSelector)", envRule.To)
	}
	if envRule.To[0].PodSelector.MatchLabels["sandbox.psenna.dev/environment"] != EnvironmentLabelValue("restricted-env") {
		t.Errorf("env-pod rule podSelector = %+v, want sandbox.psenna.dev/environment=%s", envRule.To[0].PodSelector, EnvironmentLabelValue("restricted-env"))
	}
	if len(envRule.Ports) != 0 {
		t.Errorf("env-pod rule has %d ports, want 0 (all ports)", len(envRule.Ports))
	}

	cidrRule := egress[2]
	if len(cidrRule.To) != 1 || cidrRule.To[0].IPBlock == nil || cidrRule.To[0].IPBlock.CIDR == nil || *cidrRule.To[0].IPBlock.CIDR != "140.82.112.0/20" {
		t.Fatalf("cidr peer rule To = %+v, want ipBlock 140.82.112.0/20", cidrRule.To)
	}
	if len(cidrRule.Ports) != 1 || cidrRule.Ports[0].Port.String() != "443" {
		t.Errorf("cidr peer rule Ports = %+v, want [443]", cidrRule.Ports)
	}

	selRule := egress[3]
	if len(selRule.To) != 1 || selRule.To[0].IPBlock != nil {
		t.Fatalf("selector peer rule To = %+v, want a namespaceSelector peer", selRule.To)
	}
	if selRule.To[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "ai-sandbox" {
		t.Errorf("selector peer namespaceSelector = %+v, want ai-sandbox", selRule.To[0].NamespaceSelector)
	}
	if selRule.To[0].PodSelector == nil || selRule.To[0].PodSelector.MatchLabels["app"] != "git-proxy" {
		t.Errorf("selector peer podSelector = %+v, want app=git-proxy", selRule.To[0].PodSelector)
	}
}

// assertNPIngress checks the two ingress rules: the base intra-env rule
// (env-labeled pods reach each other) and the operator rule. Both are found by
// their peer characteristics, not by index, so ingress ordering does not break
// the assertion.
func assertNPIngress(t *testing.T, ingress []anetworkingv1.NetworkPolicyIngressRuleApplyConfiguration) {
	t.Helper()
	if len(ingress) != 2 {
		t.Fatalf("Ingress has %d rules, want 2 (env-pod + operator)", len(ingress))
	}

	// The intra-env rule: a podSelector-only peer (no namespaceSelector) on the
	// env label, no ports (all ports).
	var envRule *anetworkingv1.NetworkPolicyIngressRuleApplyConfiguration
	for i := range ingress {
		rule := &ingress[i]
		if len(rule.From) != 1 || rule.From[0].PodSelector == nil || rule.From[0].NamespaceSelector != nil {
			continue
		}
		if rule.From[0].PodSelector.MatchLabels["sandbox.psenna.dev/environment"] == EnvironmentLabelValue("restricted-env") {
			envRule = rule
			break
		}
	}
	if envRule == nil {
		t.Errorf("no intra-env ingress rule with podSelector{sandbox.psenna.dev/environment=%q} (no namespaceSelector) in ingress %+v", EnvironmentLabelValue("restricted-env"), ingress)
	} else if len(envRule.Ports) != 0 {
		t.Errorf("intra-env ingress rule has %d ports, want 0 (all ports)", len(envRule.Ports))
	}

	// The operator rule: namespaceSelector+podSelector peer.
	var op *anetworkingv1.NetworkPolicyIngressRuleApplyConfiguration
	for i := range ingress {
		rule := &ingress[i]
		if len(rule.From) != 1 || rule.From[0].NamespaceSelector == nil || rule.From[0].PodSelector == nil {
			continue
		}
		if rule.From[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] == "ai-sandbox-operator-system" {
			op = rule
			break
		}
	}
	if op == nil {
		t.Fatalf("no operator ingress rule in ingress %+v", ingress)
	}
	if op.From[0].PodSelector.MatchLabels["control-plane"] != "controller-manager" {
		t.Errorf("operator ingress podSelector = %+v, want control-plane=controller-manager", op.From[0].PodSelector)
	}
}

func TestRenderNetworkPolicy_ExtraEgressCIDR(t *testing.T) {
	in := restrictedInputs()
	in.Network.Egress = []ResolvedPeer{{CIDR: "10.0.0.0/8"}}

	np, err := RenderNetworkPolicy(in)
	if err != nil {
		t.Fatalf("RenderNetworkPolicy: %v", err)
	}
	if len(np.Spec.Egress) != 3 {
		t.Fatalf("Egress has %d rules, want 3 (kube-dns + env + cidr)", len(np.Spec.Egress))
	}
	peer := np.Spec.Egress[2]
	if len(peer.To) != 1 || peer.To[0].IPBlock == nil || peer.To[0].IPBlock.CIDR == nil || *peer.To[0].IPBlock.CIDR != "10.0.0.0/8" {
		t.Errorf("cidr peer To = %+v, want ipBlock 10.0.0.0/8", peer.To)
	}
}

func TestRenderNetworkPolicy_ExtraEgressSelector(t *testing.T) {
	in := restrictedInputs()
	in.Network.Egress = []ResolvedPeer{{Selector: &v1alpha1.PeerSelector{Namespace: "other-ns"}}}

	np, err := RenderNetworkPolicy(in)
	if err != nil {
		t.Fatalf("RenderNetworkPolicy: %v", err)
	}
	if len(np.Spec.Egress) != 3 {
		t.Fatalf("Egress has %d rules, want 3 (kube-dns + env + selector)", len(np.Spec.Egress))
	}
	peer := np.Spec.Egress[2]
	if len(peer.To) != 1 || peer.To[0].NamespaceSelector == nil {
		t.Fatalf("selector peer To = %+v, want a namespaceSelector peer", peer.To)
	}
	if peer.To[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "other-ns" {
		t.Errorf("selector peer namespaceSelector = %+v, want other-ns", peer.To[0].NamespaceSelector)
	}
	// A nil PodSelector means "all pods in the namespace" -- the peer must
	// not carry an empty-but-present podSelector.
	if peer.To[0].PodSelector != nil {
		t.Errorf("selector peer PodSelector = %+v, want nil (all pods)", peer.To[0].PodSelector)
	}
}

func TestRenderNetworkPolicy_NoOperatorIngress(t *testing.T) {
	in := restrictedInputs()
	in.Network.OperatorIngress = nil

	np, err := RenderNetworkPolicy(in)
	if err != nil {
		t.Fatalf("RenderNetworkPolicy: %v", err)
	}
	// The base intra-env ingress rule is always present under Restricted (the
	// agent must reach the ServiceSet's dep/runtime pods); only the operator
	// rule is conditional on OperatorIngress. So with no operator ingress the
	// list is the single intra-env rule, not empty.
	if len(np.Spec.Ingress) != 1 {
		t.Fatalf("Ingress has %d rules, want 1 (intra-env only; no operator)", len(np.Spec.Ingress))
	}
	rule := &np.Spec.Ingress[0]
	if len(rule.From) != 1 || rule.From[0].PodSelector == nil || rule.From[0].NamespaceSelector != nil {
		t.Fatalf("intra-env ingress From = %+v, want podSelector-only (no namespaceSelector)", rule.From)
	}
	if rule.From[0].PodSelector.MatchLabels["sandbox.psenna.dev/environment"] != EnvironmentLabelValue(in.Env.Name) {
		t.Errorf("intra-env ingress podSelector = %+v, want sandbox.psenna.dev/environment=%q", rule.From[0].PodSelector, EnvironmentLabelValue(in.Env.Name))
	}
	if len(np.Spec.PolicyTypes) != 2 {
		t.Errorf("PolicyTypes = %v, want [Ingress Egress]", np.Spec.PolicyTypes)
	}
}

func TestRenderNetworkPolicy_InvalidPeer(t *testing.T) {
	in := restrictedInputs()
	in.Network.Egress = []ResolvedPeer{{
		CIDR:     "10.0.0.0/8",
		Selector: &v1alpha1.PeerSelector{Namespace: "other-ns"},
	}}
	if _, err := RenderNetworkPolicy(in); err == nil {
		t.Error("peer with both CIDR and Selector: expected error, got nil")
	}

	in.Network.Egress = []ResolvedPeer{{}}
	if _, err := RenderNetworkPolicy(in); err == nil {
		t.Error("peer with neither CIDR nor Selector: expected error, got nil")
	}
}

func TestRenderNetworkPolicy_Deterministic(t *testing.T) {
	in := restrictedInputs()
	// Deliberately unsorted: the renderer must normalize.
	in.Network.Egress = []ResolvedPeer{
		{CIDR: "10.0.0.0/8"},
		{Selector: &v1alpha1.PeerSelector{Namespace: "b-ns"}},
		{Selector: &v1alpha1.PeerSelector{Namespace: "a-ns"}},
		{CIDR: "1.2.3.4/32"},
	}

	first, err := RenderNetworkPolicy(in)
	if err != nil {
		t.Fatalf("RenderNetworkPolicy: %v", err)
	}
	for i := 0; i < 10; i++ {
		got, err := RenderNetworkPolicy(in)
		if err != nil {
			t.Fatalf("RenderNetworkPolicy iteration %d: %v", i, err)
		}
		if !reflect.DeepEqual(first, got) {
			t.Fatalf("RenderNetworkPolicy is not deterministic at iteration %d", i)
		}
	}
}

func TestRenderNetworkPolicy_Golden(t *testing.T) {
	np, err := RenderNetworkPolicy(restrictedInputs())
	if err != nil {
		t.Fatalf("RenderNetworkPolicy: %v", err)
	}
	assertGolden(t, "networkpolicy_restricted.yaml", marshalForGolden(t, np))
}

// TestNetworkPolicyEnvEgress asserts the Restricted NetworkPolicy carries an
// env-egress rule letting the agent reach other env-labeled pods in this
// namespace (the ServiceSet's dep/runtime pods, which carry the same env label
// via serviceset_controller.entryLabels). The rule is a podSelector-only peer
// (no namespaceSelector => this namespace) with no ports (all ports). It is
// found by its podSelector matchLabels, not by index, so egress ordering does
// not break the assertion.
func TestNetworkPolicyEnvEgress(t *testing.T) {
	in := restrictedInputs()
	np, err := RenderNetworkPolicy(in)
	if err != nil {
		t.Fatalf("RenderNetworkPolicy: %v", err)
	}
	if np == nil || np.Spec == nil {
		t.Fatal("Restricted isolation: got nil NetworkPolicy/Spec")
	}

	wantLabel := EnvironmentLabelValue(in.Env.Name)
	var found bool
	for i := range np.Spec.Egress {
		rule := &np.Spec.Egress[i]
		if len(rule.Ports) != 0 {
			continue // the env rule is all-ports
		}
		if len(rule.To) != 1 || rule.To[0].PodSelector == nil {
			continue
		}
		if rule.To[0].PodSelector.MatchLabels["sandbox.psenna.dev/environment"] == wantLabel &&
			rule.To[0].NamespaceSelector == nil {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("no env-egress rule with podSelector{sandbox.psenna.dev/environment=%q} (no namespaceSelector, no ports) in egress %+v", wantLabel, np.Spec.Egress)
	}
}

// TestNetworkPolicyEnvIngress asserts the Restricted NetworkPolicy carries an
// intra-env ingress rule -- the symmetric counterpart to the env-egress rule.
// The policy's podSelector IS the env label, so it selects the ServiceSet's
// dep/runtime pods too; without a matching ingress rule their ingress is
// default-denied and the agent's egress to them (allowed by the env-egress
// rule) is dropped at the destination, hanging the connection (the e2e
// "reaches a declared service via Service DNS" failure this guards). The rule
// is a podSelector-only peer (no namespaceSelector => this namespace) with no
// ports (all ports). It is found by its podSelector matchLabels, not by index,
// so ingress ordering does not break the assertion.
func TestNetworkPolicyEnvIngress(t *testing.T) {
	in := restrictedInputs()
	np, err := RenderNetworkPolicy(in)
	if err != nil {
		t.Fatalf("RenderNetworkPolicy: %v", err)
	}
	if np == nil || np.Spec == nil {
		t.Fatal("Restricted isolation: got nil NetworkPolicy/Spec")
	}

	wantLabel := EnvironmentLabelValue(in.Env.Name)
	var found bool
	for i := range np.Spec.Ingress {
		rule := &np.Spec.Ingress[i]
		if len(rule.Ports) != 0 {
			continue // the env rule is all-ports
		}
		if len(rule.From) != 1 || rule.From[0].PodSelector == nil {
			continue
		}
		if rule.From[0].PodSelector.MatchLabels["sandbox.psenna.dev/environment"] == wantLabel &&
			rule.From[0].NamespaceSelector == nil {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("no intra-env ingress rule with podSelector{sandbox.psenna.dev/environment=%q} (no namespaceSelector, no ports) in ingress %+v", wantLabel, np.Spec.Ingress)
	}
}
