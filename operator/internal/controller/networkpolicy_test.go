package controller

import (
	"context"
	"fmt"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"

	sandboxv1alpha1 "github.com/psenna/ai-sandbox/operator/api/v1alpha1"
	"github.com/psenna/ai-sandbox/operator/internal/render"
)

// mustCreateEnvForClass creates a minimal valid SandboxEnvironment named name
// in the "default" namespace referencing the class named className.
func mustCreateEnvForClass(t *testing.T, name, className string) *sandboxv1alpha1.SandboxEnvironment {
	t.Helper()
	env := &sandboxv1alpha1.SandboxEnvironment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: sandboxv1alpha1.SandboxEnvironmentSpec{
			ClassRef: sandboxv1alpha1.ClassRef{Name: className},
			Repo:     "org/repo",
			Task:     sandboxv1alpha1.TaskSpec{Prompt: "do stuff"},
		},
	}
	if err := k8s.Create(ctx, env); err != nil {
		t.Fatalf("creating SandboxEnvironment %s: %v", name, err)
	}
	return env
}

// mustCreateNetworkClass creates a SandboxClass named name with the given
// isolation and extraEgress peers, an S3 storage backend (external endpoint,
// so a Restricted class needs an extraEgress CIDR -- see mustCreateClass),
// and no services. SandboxClass is cluster-scoped, so name must be unique per
// test.
func mustCreateNetworkClass(t *testing.T, name string, isolation sandboxv1alpha1.NetworkIsolation, extraEgress []sandboxv1alpha1.EgressPeer) *sandboxv1alpha1.SandboxClass {
	t.Helper()
	mustCreateS3CredsSecret(t, "default", "s3-creds")
	class := &sandboxv1alpha1.SandboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: sandboxv1alpha1.SandboxClassSpec{
			Agent: sandboxv1alpha1.AgentSpec{Image: "ghcr.io/psenna/ai-sandbox-agent:v1"},
			Storage: sandboxv1alpha1.StorageSpec{
				Backend: sandboxv1alpha1.BackendSpec{
					Type: sandboxv1alpha1.StorageBackendTypeS3,
					S3: &sandboxv1alpha1.S3Backend{
						Endpoint: "https://s3.example.com",
						Bucket:   "sandbox-snapshots",
						CredentialsSecretRef: sandboxv1alpha1.SecretKeyRef{
							Name: "s3-creds",
						},
					},
				},
			},
			Network: sandboxv1alpha1.NetworkSpec{
				Isolation:   isolation,
				ExtraEgress: extraEgress,
			},
		},
	}
	if err := k8s.Create(ctx, class); err != nil {
		t.Fatalf("creating SandboxClass %s: %v", name, err)
	}
	t.Cleanup(func() {
		_ = k8s.Delete(ctx, class)
	})
	return class
}

// TestNetworkPolicy_RestrictedCreatesPolicy: a Restricted class reconciles to
// a NetworkPolicy owned by the environment, default-denying ingress and egress
// except for the kube-dns rule, the API-server ipBlock peer, the extraEgress
// CIDR peer, and the operator ingress rule.
func TestNetworkPolicy_RestrictedCreatesPolicy(t *testing.T) {
	mustCreateNetworkClass(t, "restricted-np", sandboxv1alpha1.NetworkIsolationRestricted,
		[]sandboxv1alpha1.EgressPeer{{CIDR: "0.0.0.0/0"}})
	env := mustCreateEnvForClass(t, "restricted-np-env", "restricted-np")
	key := types.NamespacedName{Namespace: env.Namespace, Name: env.Name}

	r := newResourceReconciler(t, newFakeClock(fixedStart))
	reconcileOnce(t, r, key)

	names := render.ChildNames(env.Name)
	var np networkingv1.NetworkPolicy
	mustGetObj(t, env.Namespace, names.NetworkPolicy, &np)

	fresh := getEnv(t, key)
	if !ownedByEnv(&np, fresh) {
		t.Errorf("NetworkPolicy is not owned by the environment")
	}

	if np.Spec.PodSelector.MatchLabels["sandbox.psenna.dev/environment"] != render.EnvironmentLabelValue(env.Name) {
		t.Errorf("PodSelector = %+v, want sandbox.psenna.dev/environment=%s", np.Spec.PodSelector, render.EnvironmentLabelValue(env.Name))
	}
	if len(np.Spec.PolicyTypes) != 2 ||
		np.Spec.PolicyTypes[0] != networkingv1.PolicyTypeIngress ||
		np.Spec.PolicyTypes[1] != networkingv1.PolicyTypeEgress {
		t.Errorf("PolicyTypes = %v, want [Ingress Egress]", np.Spec.PolicyTypes)
	}

	// Egress: rule 0 = kube-dns, rule 1 = env-pods (agent -> env-labeled pods),
	// then the two CIDR peers sorted by CIDR (0.0.0.0/0 before 10.0.0.1/32 --
	// the external S3 endpoint is covered by the extraEgress CIDR, so it
	// contributes no selector peer of its own).
	if len(np.Spec.Egress) != 4 {
		t.Fatalf("Egress has %d rules, want 4 (kube-dns + env-pods + api-server + extraEgress)", len(np.Spec.Egress))
	}
	dns := np.Spec.Egress[0]
	if len(dns.To) != 1 || dns.To[0].NamespaceSelector == nil ||
		dns.To[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "kube-system" {
		t.Errorf("kube-dns rule To = %+v, want namespaceSelector kube-system", dns.To)
	}
	envRule := np.Spec.Egress[1]
	if len(envRule.To) != 1 || envRule.To[0].PodSelector == nil || envRule.To[0].NamespaceSelector != nil {
		t.Errorf("env-pod rule To = %+v, want podSelector-only (no namespaceSelector)", envRule.To)
	}
	if envRule.To[0].PodSelector.MatchLabels["sandbox.psenna.dev/environment"] != render.EnvironmentLabelValue(env.Name) {
		t.Errorf("env-pod rule podSelector = %+v, want sandbox.psenna.dev/environment=%s", envRule.To[0].PodSelector, render.EnvironmentLabelValue(env.Name))
	}
	if len(envRule.Ports) != 0 {
		t.Errorf("env-pod rule has %d ports, want 0 (all ports)", len(envRule.Ports))
	}
	extra := np.Spec.Egress[2]
	if len(extra.To) != 1 || extra.To[0].IPBlock == nil || extra.To[0].IPBlock.CIDR != "0.0.0.0/0" {
		t.Errorf("extraEgress rule To = %+v, want ipBlock 0.0.0.0/0", extra.To)
	}
	api := np.Spec.Egress[3]
	if len(api.To) != 1 || api.To[0].IPBlock == nil {
		t.Fatalf("api-server rule To = %+v, want ipBlock", api.To)
	}
	var k8sSvc corev1.Service
	if err := k8s.Get(ctx, types.NamespacedName{Namespace: "default", Name: "kubernetes"}, &k8sSvc); err != nil {
		t.Fatalf("reading kubernetes service: %v", err)
	}
	if api.To[0].IPBlock.CIDR != k8sSvc.Spec.ClusterIP+"/32" {
		t.Errorf("api-server ipBlock = %q, want %s/32", api.To[0].IPBlock.CIDR, k8sSvc.Spec.ClusterIP)
	}
	if len(api.Ports) != 1 || api.Ports[0].Port.String() != "443" {
		t.Errorf("api-server rule Ports = %+v, want [443]", api.Ports)
	}

	// Ingress: exactly one rule, the operator selector (in the reconciler's
	// ClassSecretNamespace, "default" in this suite).
	if len(np.Spec.Ingress) != 1 {
		t.Fatalf("Ingress has %d rules, want 1 (operator)", len(np.Spec.Ingress))
	}
	op := np.Spec.Ingress[0]
	if len(op.From) != 1 || op.From[0].NamespaceSelector == nil || op.From[0].PodSelector == nil {
		t.Fatalf("operator ingress From = %+v, want namespaceSelector+podSelector", op.From)
	}
	if op.From[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "default" {
		t.Errorf("operator ingress namespaceSelector = %+v, want default (ClassSecretNamespace)", op.From[0].NamespaceSelector)
	}
	if op.From[0].PodSelector.MatchLabels["control-plane"] != "controller-manager" {
		t.Errorf("operator ingress podSelector = %+v, want control-plane=controller-manager", op.From[0].PodSelector)
	}
}

// TestNetworkPolicy_OpenDeletesStalePolicy: an Open class reconciles to NO
// NetworkPolicy, and a stale policy left over from a previous Restricted
// class is deleted (ownership-guarded).
func TestNetworkPolicy_OpenDeletesStalePolicy(t *testing.T) {
	mustCreateNetworkClass(t, "open-np", sandboxv1alpha1.NetworkIsolationOpen, nil)
	env := mustCreateEnvForClass(t, "open-np-env", "open-np")
	key := types.NamespacedName{Namespace: env.Namespace, Name: env.Name}

	// Pre-create a stale NetworkPolicy owned by the environment, as if the
	// class had previously been Restricted.
	names := render.ChildNames(env.Name)
	stale := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      names.NetworkPolicy,
			Namespace: env.Namespace,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         "sandbox.psenna.dev/v1alpha1",
				Kind:               "SandboxEnvironment",
				Name:               env.Name,
				UID:                env.UID,
				Controller:         boolPtr(true),
				BlockOwnerDeletion: boolPtr(true),
			}},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
		},
	}
	if err := k8s.Create(ctx, stale); err != nil {
		t.Fatalf("pre-creating stale NetworkPolicy: %v", err)
	}

	r := newResourceReconciler(t, newFakeClock(fixedStart))
	reconcileOnce(t, r, key)

	var np networkingv1.NetworkPolicy
	err := k8s.Get(ctx, types.NamespacedName{Namespace: env.Namespace, Name: names.NetworkPolicy}, &np)
	if !apierrors.IsNotFound(err) {
		t.Errorf("stale NetworkPolicy still exists after Open reconcile: err=%v", err)
	}
}

// TestNetworkPolicy_ForeignPolicyNotClobbered: a same-named NetworkPolicy
// owned by an unrelated object is never modified or deleted, and the
// environment reports the conflict as a ResourcesProblem.
func TestNetworkPolicy_ForeignPolicyNotClobbered(t *testing.T) {
	mustCreateNetworkClass(t, "foreign-np", sandboxv1alpha1.NetworkIsolationRestricted,
		[]sandboxv1alpha1.EgressPeer{{CIDR: "0.0.0.0/0"}})
	env := mustCreateEnvForClass(t, "foreign-np-env", "foreign-np")
	key := types.NamespacedName{Namespace: env.Namespace, Name: env.Name}

	names := render.ChildNames(env.Name)
	foreign := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      names.NetworkPolicy,
			Namespace: env.Namespace,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         "networking.k8s.io/v1",
				Kind:               "NetworkPolicy",
				Name:               "unrelated-owner",
				UID:                types.UID("33333333-3333-3333-3333-333333333333"),
				Controller:         boolPtr(true),
				BlockOwnerDeletion: boolPtr(true),
			}},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"do-not-touch": "true"}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
		},
	}
	if err := k8s.Create(ctx, foreign); err != nil {
		t.Fatalf("pre-creating foreign NetworkPolicy: %v", err)
	}
	t.Cleanup(func() {
		_ = k8s.Delete(ctx, foreign)
	})

	r := newResourceReconciler(t, newFakeClock(fixedStart))
	// First reconcile: observeCluster runs before ensureResources, so it
	// reports the FIRST missing child (ServiceAccount), not yet the
	// NetworkPolicy conflict. The second reconcile's observeCluster call is
	// what actually observes the foreign NetworkPolicy.
	reconcileOnce(t, r, key)
	reconcileOnce(t, r, key)

	var after networkingv1.NetworkPolicy
	mustGetObj(t, env.Namespace, names.NetworkPolicy, &after)
	if after.Spec.PodSelector.MatchLabels["do-not-touch"] != "true" {
		t.Errorf("foreign NetworkPolicy was modified: %+v", after.Spec.PodSelector)
	}
	if len(after.OwnerReferences) != 1 || after.OwnerReferences[0].Name != "unrelated-owner" {
		t.Errorf("foreign NetworkPolicy ownerReferences changed: %+v", after.OwnerReferences)
	}

	got := getEnv(t, key)
	c := findCondition(got, "Scheduled")
	if c == nil || !strings.Contains(c.Message, "NetworkPolicy") || !strings.Contains(c.Message, "owned by another object") {
		t.Errorf("Scheduled condition = %+v, want message mentioning NetworkPolicy owned by another object", c)
	}
}

// TestNetworkPolicy_RestrictedToOpenRemovesPolicy: a class switching from
// Restricted to Open isolation removes the previously-applied NetworkPolicy.
func TestNetworkPolicy_RestrictedToOpenRemovesPolicy(t *testing.T) {
	mustCreateNetworkClass(t, "restricted-to-open", sandboxv1alpha1.NetworkIsolationRestricted,
		[]sandboxv1alpha1.EgressPeer{{CIDR: "0.0.0.0/0"}})
	env := mustCreateEnvForClass(t, "restricted-to-open-env", "restricted-to-open")
	key := types.NamespacedName{Namespace: env.Namespace, Name: env.Name}

	r := newResourceReconciler(t, newFakeClock(fixedStart))
	reconcileOnce(t, r, key)

	names := render.ChildNames(env.Name)
	var np networkingv1.NetworkPolicy
	mustGetObj(t, env.Namespace, names.NetworkPolicy, &np)

	// Switch the class to Open isolation.
	class := &sandboxv1alpha1.SandboxClass{}
	if err := k8s.Get(ctx, types.NamespacedName{Name: "restricted-to-open"}, class); err != nil {
		t.Fatalf("Get class: %v", err)
	}
	class.Spec.Network.Isolation = sandboxv1alpha1.NetworkIsolationOpen
	class.Spec.Network.ExtraEgress = nil
	if err := k8s.Update(ctx, class); err != nil {
		t.Fatalf("switching class to Open: %v", err)
	}

	reconcileOnce(t, r, key)

	err := k8s.Get(ctx, types.NamespacedName{Namespace: env.Namespace, Name: names.NetworkPolicy}, &np)
	if !apierrors.IsNotFound(err) {
		t.Errorf("NetworkPolicy still exists after class switched to Open: err=%v", err)
	}
}

// TestResolveNetworkPeers_InClusterServiceBecomesSelector: an in-cluster
// <svc>.<ns>.svc endpoint resolves to a pod-selector peer built from the
// Service's own selector; the `kubernetes` Service becomes the API-server
// ipBlock peer; extraEgress entries are folded in verbatim.
func TestResolveNetworkPeers_InClusterServiceBecomesSelector(t *testing.T) {
	mustCreateNamespace(t, "ai-sandbox")
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "git-proxy", Namespace: "ai-sandbox"},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": "git-proxy"},
			Ports:    []corev1.ServicePort{{Port: 8080}},
		},
	}
	if err := k8s.Create(ctx, svc); err != nil {
		t.Fatalf("creating git-proxy service: %v", err)
	}
	t.Cleanup(func() {
		_ = k8s.Delete(ctx, svc)
	})

	class := &sandboxv1alpha1.SandboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: "resolve-incluster"},
		Spec: sandboxv1alpha1.SandboxClassSpec{
			Agent: sandboxv1alpha1.AgentSpec{Image: "ghcr.io/psenna/ai-sandbox-agent:v1"},
			Services: sandboxv1alpha1.ServicesSpec{
				GitProxy: &sandboxv1alpha1.GitProxyService{
					GitURL:         "http://git-proxy.ai-sandbox.svc:8080",
					BrokerURL:      "http://git-proxy.ai-sandbox.svc:8090",
					TokenSecretRef: sandboxv1alpha1.SecretKeyRef{Name: "git-proxy-token", Key: "token"},
				},
			},
			Storage: sandboxv1alpha1.StorageSpec{
				Backend: sandboxv1alpha1.BackendSpec{
					Type: sandboxv1alpha1.StorageBackendTypeS3,
					S3: &sandboxv1alpha1.S3Backend{
						Endpoint: "https://s3.example.com",
						Bucket:   "sandbox-snapshots",
						CredentialsSecretRef: sandboxv1alpha1.SecretKeyRef{
							Name: "s3-creds",
						},
					},
				},
			},
			Network: sandboxv1alpha1.NetworkSpec{
				Isolation:   sandboxv1alpha1.NetworkIsolationRestricted,
				ExtraEgress: []sandboxv1alpha1.EgressPeer{{CIDR: "0.0.0.0/0"}},
			},
		},
	}
	if err := k8s.Create(ctx, class); err != nil {
		t.Fatalf("creating SandboxClass: %v", err)
	}
	t.Cleanup(func() {
		_ = k8s.Delete(ctx, class)
	})

	r := newResourceReconciler(t, newFakeClock(fixedStart))
	net, err := r.resolveNetworkPeers(ctx, class)
	if err != nil {
		t.Fatalf("resolveNetworkPeers: %v", err)
	}

	// Peers: git selector, broker selector, api-server ipBlock, extraEgress
	// CIDR (the external S3 endpoint is covered by the CIDR, so it contributes
	// no peer of its own).
	if len(net.Egress) != 4 {
		t.Fatalf("Egress has %d peers, want 4", len(net.Egress))
	}
	for i, want := range []string{"git", "broker"} {
		sel := net.Egress[i].Selector
		if sel == nil || sel.Namespace != "ai-sandbox" || sel.PodSelector == nil || sel.PodSelector.MatchLabels["app"] != "git-proxy" {
			t.Errorf("peer %d (%s) = %+v, want selector ai-sandbox app=git-proxy", i, want, net.Egress[i])
		}
	}
	api := net.Egress[2]
	if api.CIDR == "" || api.Selector != nil {
		t.Errorf("peer 2 = %+v, want an ipBlock peer", api)
	}
	var k8sSvc corev1.Service
	if err := k8s.Get(ctx, types.NamespacedName{Namespace: "default", Name: "kubernetes"}, &k8sSvc); err != nil {
		t.Fatalf("reading kubernetes service: %v", err)
	}
	if api.CIDR != k8sSvc.Spec.ClusterIP+"/32" {
		t.Errorf("api-server peer CIDR = %q, want %s/32", api.CIDR, k8sSvc.Spec.ClusterIP)
	}
	if len(api.Ports) != 1 || api.Ports[0].Port != "443" {
		t.Errorf("api-server peer Ports = %+v, want [443]", api.Ports)
	}
	extra := net.Egress[3]
	if extra.CIDR != "0.0.0.0/0" {
		t.Errorf("extraEgress peer = %+v, want CIDR 0.0.0.0/0", extra)
	}

	if net.OperatorIngress == nil || net.OperatorIngress.Namespace != "default" ||
		net.OperatorIngress.PodSelector == nil || net.OperatorIngress.PodSelector.MatchLabels["control-plane"] != "controller-manager" {
		t.Errorf("OperatorIngress = %+v, want default/control-plane=controller-manager", net.OperatorIngress)
	}
}

// TestResolveNetworkPeers_ExternalEndpointWithoutCIDRErrors: a Restricted
// class whose service endpoint is external (no <svc>.<ns>.svc match) and
// whose extraEgress declares no CIDR peer is a validation error -- a selector
// cannot match an off-cluster host.
func TestResolveNetworkPeers_ExternalEndpointWithoutCIDRErrors(t *testing.T) {
	class := &sandboxv1alpha1.SandboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: "resolve-external"},
		Spec: sandboxv1alpha1.SandboxClassSpec{
			Agent: sandboxv1alpha1.AgentSpec{Image: "ghcr.io/psenna/ai-sandbox-agent:v1"},
			Services: sandboxv1alpha1.ServicesSpec{
				GitProxy: &sandboxv1alpha1.GitProxyService{
					GitURL:         "http://git-proxy.example.com:8080",
					BrokerURL:      "http://git-proxy.example.com:8090",
					TokenSecretRef: sandboxv1alpha1.SecretKeyRef{Name: "git-proxy-token", Key: "token"},
				},
			},
			Storage: sandboxv1alpha1.StorageSpec{
				Backend: sandboxv1alpha1.BackendSpec{
					Type: sandboxv1alpha1.StorageBackendTypeS3,
					S3: &sandboxv1alpha1.S3Backend{
						Endpoint: "https://s3.example.com",
						Bucket:   "sandbox-snapshots",
						CredentialsSecretRef: sandboxv1alpha1.SecretKeyRef{
							Name: "s3-creds",
						},
					},
				},
			},
			Network: sandboxv1alpha1.NetworkSpec{
				Isolation: sandboxv1alpha1.NetworkIsolationRestricted,
			},
		},
	}
	if err := k8s.Create(ctx, class); err != nil {
		t.Fatalf("creating SandboxClass: %v", err)
	}
	t.Cleanup(func() {
		_ = k8s.Delete(ctx, class)
	})

	r := newResourceReconciler(t, newFakeClock(fixedStart))
	_, err := r.resolveNetworkPeers(ctx, class)
	if err == nil {
		t.Fatal("resolveNetworkPeers with an external endpoint and no extraEgress CIDR: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "external") || !strings.Contains(err.Error(), "extraEgress") {
		t.Errorf("error = %v, want it to mention external and extraEgress", err)
	}
}

// TestResolveNetworkPeers_ExternalEndpointCoveredByCIDR: an external endpoint
// whose resolved IP falls inside an extraEgress CIDR is accepted and dropped
// (the covering CIDR is itself emitted as a peer), so the policy allows exactly
// that block. The fake LookupHost stands in for DNS.
func TestResolveNetworkPeers_ExternalEndpointCoveredByCIDR(t *testing.T) {
	class := &sandboxv1alpha1.SandboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: "resolve-covered"},
		Spec: sandboxv1alpha1.SandboxClassSpec{
			Agent: sandboxv1alpha1.AgentSpec{Image: "ghcr.io/psenna/ai-sandbox-agent:v1"},
			Services: sandboxv1alpha1.ServicesSpec{
				GitProxy: &sandboxv1alpha1.GitProxyService{
					GitURL:         "http://git-proxy.example.com:8080",
					BrokerURL:      "http://git-proxy.example.com:8090",
					TokenSecretRef: sandboxv1alpha1.SecretKeyRef{Name: "git-proxy-token", Key: "token"},
				},
			},
			Storage: sandboxv1alpha1.StorageSpec{
				Backend: sandboxv1alpha1.BackendSpec{
					Type: sandboxv1alpha1.StorageBackendTypeS3,
					S3: &sandboxv1alpha1.S3Backend{
						Endpoint: "https://s3.example.com",
						Bucket:   "sandbox-snapshots",
						CredentialsSecretRef: sandboxv1alpha1.SecretKeyRef{
							Name: "s3-creds",
						},
					},
				},
			},
			Network: sandboxv1alpha1.NetworkSpec{
				Isolation: sandboxv1alpha1.NetworkIsolationRestricted,
				ExtraEgress: []sandboxv1alpha1.EgressPeer{
					{CIDR: "203.0.113.0/24"},
				},
			},
		},
	}
	if err := k8s.Create(ctx, class); err != nil {
		t.Fatalf("creating SandboxClass: %v", err)
	}
	t.Cleanup(func() {
		_ = k8s.Delete(ctx, class)
	})

	r := newResourceReconciler(t, newFakeClock(fixedStart))
	r.LookupHost = func(ctx context.Context, host string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("203.0.113.5")}, nil
	}
	net, err := r.resolveNetworkPeers(ctx, class)
	if err != nil {
		t.Fatalf("resolveNetworkPeers with a covering extraEgress CIDR: %v", err)
	}
	// The covered external endpoints are dropped; the peers are the api-server
	// ipBlock and the extraEgress CIDR itself.
	if len(net.Egress) != 2 {
		t.Fatalf("Egress = %+v, want 2 peers (api-server + extraEgress)", net.Egress)
	}
	if net.Egress[1].CIDR != "203.0.113.0/24" {
		t.Errorf("extraEgress peer = %+v, want CIDR 203.0.113.0/24", net.Egress[1])
	}
}

// TestResolveNetworkPeers_ExternalEndpointUncoveredByCIDR: an external endpoint
// that resolves to an IP outside every extraEgress CIDR is a loud error -- the
// policy would deny egress to it and the sandbox would hang.
func TestResolveNetworkPeers_ExternalEndpointUncoveredByCIDR(t *testing.T) {
	class := &sandboxv1alpha1.SandboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: "resolve-uncovered"},
		Spec: sandboxv1alpha1.SandboxClassSpec{
			Agent: sandboxv1alpha1.AgentSpec{Image: "ghcr.io/psenna/ai-sandbox-agent:v1"},
			Services: sandboxv1alpha1.ServicesSpec{
				GitProxy: &sandboxv1alpha1.GitProxyService{
					GitURL:         "http://git-proxy.example.com:8080",
					BrokerURL:      "http://git-proxy.example.com:8090",
					TokenSecretRef: sandboxv1alpha1.SecretKeyRef{Name: "git-proxy-token", Key: "token"},
				},
			},
			Storage: sandboxv1alpha1.StorageSpec{
				Backend: sandboxv1alpha1.BackendSpec{
					Type: sandboxv1alpha1.StorageBackendTypeS3,
					S3: &sandboxv1alpha1.S3Backend{
						Endpoint: "https://s3.example.com",
						Bucket:   "sandbox-snapshots",
						CredentialsSecretRef: sandboxv1alpha1.SecretKeyRef{
							Name: "s3-creds",
						},
					},
				},
			},
			Network: sandboxv1alpha1.NetworkSpec{
				Isolation: sandboxv1alpha1.NetworkIsolationRestricted,
				ExtraEgress: []sandboxv1alpha1.EgressPeer{
					{CIDR: "10.0.0.0/8"},
				},
			},
		},
	}
	if err := k8s.Create(ctx, class); err != nil {
		t.Fatalf("creating SandboxClass: %v", err)
	}
	t.Cleanup(func() {
		_ = k8s.Delete(ctx, class)
	})

	r := newResourceReconciler(t, newFakeClock(fixedStart))
	r.LookupHost = func(ctx context.Context, host string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("203.0.113.5")}, nil
	}
	_, err := r.resolveNetworkPeers(ctx, class)
	if err == nil {
		t.Fatal("resolveNetworkPeers with an uncovered external endpoint: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "covers") {
		t.Errorf("error = %v, want it to mention that no CIDR covers the endpoint", err)
	}
}

// TestResolveNetworkPeers_ExternalEndpointDNSFails: when the operator cannot
// resolve an external endpoint (no external DNS), the endpoint is accepted --
// coverage is unverifiable, not provably absent, and the user's CIDR is the
// best available statement of intent.
func TestResolveNetworkPeers_ExternalEndpointDNSFails(t *testing.T) {
	class := &sandboxv1alpha1.SandboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: "resolve-dnsfail"},
		Spec: sandboxv1alpha1.SandboxClassSpec{
			Agent: sandboxv1alpha1.AgentSpec{Image: "ghcr.io/psenna/ai-sandbox-agent:v1"},
			Services: sandboxv1alpha1.ServicesSpec{
				GitProxy: &sandboxv1alpha1.GitProxyService{
					GitURL:         "http://git-proxy.example.com:8080",
					BrokerURL:      "http://git-proxy.example.com:8090",
					TokenSecretRef: sandboxv1alpha1.SecretKeyRef{Name: "git-proxy-token", Key: "token"},
				},
			},
			Storage: sandboxv1alpha1.StorageSpec{
				Backend: sandboxv1alpha1.BackendSpec{
					Type: sandboxv1alpha1.StorageBackendTypeS3,
					S3: &sandboxv1alpha1.S3Backend{
						Endpoint: "https://s3.example.com",
						Bucket:   "sandbox-snapshots",
						CredentialsSecretRef: sandboxv1alpha1.SecretKeyRef{
							Name: "s3-creds",
						},
					},
				},
			},
			Network: sandboxv1alpha1.NetworkSpec{
				Isolation: sandboxv1alpha1.NetworkIsolationRestricted,
				ExtraEgress: []sandboxv1alpha1.EgressPeer{
					{CIDR: "203.0.113.0/24"},
				},
			},
		},
	}
	if err := k8s.Create(ctx, class); err != nil {
		t.Fatalf("creating SandboxClass: %v", err)
	}
	t.Cleanup(func() {
		_ = k8s.Delete(ctx, class)
	})

	r := newResourceReconciler(t, newFakeClock(fixedStart))
	r.LookupHost = func(ctx context.Context, host string) ([]netip.Addr, error) {
		return nil, fmt.Errorf("no such host")
	}
	net, err := r.resolveNetworkPeers(ctx, class)
	if err != nil {
		t.Fatalf("resolveNetworkPeers with unresolvable endpoints: %v", err)
	}
	if len(net.Egress) != 2 {
		t.Fatalf("Egress = %+v, want 2 peers (api-server + extraEgress)", net.Egress)
	}
}

// TestResolveNetworkPeers_ExtraEgressBothOrNeither: an extraEgress entry that
// sets both cidr and selector (or neither) is a validation error. The class is
// passed as a plain struct, NOT via k8s.Create -- the CRD's XValidation would
// reject a both-set entry at create time, and this test must exercise the
// controller's own defensive validation for direct apiserver writes.
func TestResolveNetworkPeers_ExtraEgressBothOrNeither(t *testing.T) {
	r := newResourceReconciler(t, newFakeClock(fixedStart))

	both := &sandboxv1alpha1.SandboxClass{
		Spec: sandboxv1alpha1.SandboxClassSpec{
			Network: sandboxv1alpha1.NetworkSpec{
				Isolation: sandboxv1alpha1.NetworkIsolationRestricted,
				ExtraEgress: []sandboxv1alpha1.EgressPeer{
					{CIDR: "10.0.0.0/8", Selector: &sandboxv1alpha1.PeerSelector{Namespace: "default"}},
				},
			},
		},
	}
	_, err := r.resolveNetworkPeers(ctx, both)
	if err == nil {
		t.Fatal("extraEgress with both cidr and selector: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "exactly one") {
		t.Errorf("error = %v, want it to mention exactly one of cidr or selector", err)
	}

	neither := &sandboxv1alpha1.SandboxClass{
		Spec: sandboxv1alpha1.SandboxClassSpec{
			Network: sandboxv1alpha1.NetworkSpec{
				Isolation: sandboxv1alpha1.NetworkIsolationRestricted,
				ExtraEgress: []sandboxv1alpha1.EgressPeer{
					{},
				},
			},
		},
	}
	_, err = r.resolveNetworkPeers(ctx, neither)
	if err == nil {
		t.Fatal("extraEgress with neither cidr nor selector: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "exactly one") {
		t.Errorf("error = %v, want it to mention exactly one of cidr or selector", err)
	}
}

// TestResolveNetworkPeers_OpenIsolationIsEmpty: Open isolation resolves to an
// empty NetworkInputs immediately -- no policy is rendered, so nothing needs
// resolving and an external endpoint without an extraEgress CIDR is not an
// error.
func TestResolveNetworkPeers_OpenIsolationIsEmpty(t *testing.T) {
	class := &sandboxv1alpha1.SandboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: "resolve-open"},
		Spec: sandboxv1alpha1.SandboxClassSpec{
			Agent: sandboxv1alpha1.AgentSpec{Image: "ghcr.io/psenna/ai-sandbox-agent:v1"},
			Services: sandboxv1alpha1.ServicesSpec{
				GitProxy: &sandboxv1alpha1.GitProxyService{
					GitURL:         "http://git-proxy.example.com:8080",
					BrokerURL:      "http://git-proxy.example.com:8090",
					TokenSecretRef: sandboxv1alpha1.SecretKeyRef{Name: "git-proxy-token", Key: "token"},
				},
			},
			Storage: sandboxv1alpha1.StorageSpec{
				Backend: sandboxv1alpha1.BackendSpec{
					Type: sandboxv1alpha1.StorageBackendTypeS3,
					S3: &sandboxv1alpha1.S3Backend{
						Endpoint: "https://s3.example.com",
						Bucket:   "sandbox-snapshots",
						CredentialsSecretRef: sandboxv1alpha1.SecretKeyRef{
							Name: "s3-creds",
						},
					},
				},
			},
			Network: sandboxv1alpha1.NetworkSpec{
				Isolation: sandboxv1alpha1.NetworkIsolationOpen,
			},
		},
	}
	if err := k8s.Create(ctx, class); err != nil {
		t.Fatalf("creating SandboxClass: %v", err)
	}
	t.Cleanup(func() {
		_ = k8s.Delete(ctx, class)
	})

	r := newResourceReconciler(t, newFakeClock(fixedStart))
	net, err := r.resolveNetworkPeers(ctx, class)
	if err != nil {
		t.Fatalf("resolveNetworkPeers: %v", err)
	}
	if len(net.Egress) != 0 || net.OperatorIngress != nil {
		t.Errorf("Open isolation: got %+v, want empty NetworkInputs", net)
	}
}

// TestNetworkPostureCondition covers the three NetworkPosture states:
// Restricted -> True, Open -> False, unresolved class -> Unknown.
func TestNetworkPostureCondition(t *testing.T) {
	env := &sandboxv1alpha1.SandboxEnvironment{ObjectMeta: metav1.ObjectMeta{Name: "cond-env", Namespace: "default"}}
	now := fixedStart

	restricted := &sandboxv1alpha1.SandboxClass{Spec: sandboxv1alpha1.SandboxClassSpec{
		Network: sandboxv1alpha1.NetworkSpec{Isolation: sandboxv1alpha1.NetworkIsolationRestricted},
	}}
	c := networkPostureCondition(env, restricted, now)
	if c.Status != metav1.ConditionTrue || c.Reason != ReasonPostureRestricted {
		t.Errorf("Restricted: got %s/%s, want True/%s", c.Status, c.Reason, ReasonPostureRestricted)
	}

	open := &sandboxv1alpha1.SandboxClass{Spec: sandboxv1alpha1.SandboxClassSpec{
		Network: sandboxv1alpha1.NetworkSpec{Isolation: sandboxv1alpha1.NetworkIsolationOpen},
	}}
	c = networkPostureCondition(env, open, now)
	if c.Status != metav1.ConditionFalse || c.Reason != ReasonPostureOpen {
		t.Errorf("Open: got %s/%s, want False/%s", c.Status, c.Reason, ReasonPostureOpen)
	}

	c = networkPostureCondition(env, nil, now)
	if c.Status != metav1.ConditionUnknown || c.Reason != ReasonPostureUnknown {
		t.Errorf("nil class: got %s/%s, want Unknown/%s", c.Status, c.Reason, ReasonPostureUnknown)
	}
}

// TestCNIEnforcementCondition covers the four CNIEnforcement states, routing
// on the probe's structured Reason (Defect 1): probe not run (nil) -> Unknown,
// enforced -> True, confirmed-not-enforced -> False/NotEnforced (the loud, real
// case), transient probe failure -> Unknown (we do NOT claim non-enforcement
// from an inability to probe).
func TestCNIEnforcementCondition(t *testing.T) {
	env := &sandboxv1alpha1.SandboxEnvironment{ObjectMeta: metav1.ObjectMeta{Name: "cni-env", Namespace: "default"}}
	now := fixedStart

	c := cniEnforcementCondition(env, nil, now)
	if c.Status != metav1.ConditionUnknown || c.Reason != ReasonCNIUnconfirmed {
		t.Errorf("nil probe: got %s/%s, want Unknown/%s", c.Status, c.Reason, ReasonCNIUnconfirmed)
	}

	c = cniEnforcementCondition(env, &CNIProbeResult{Enforced: true, Reason: ReasonCNIEnforced}, now)
	if c.Status != metav1.ConditionTrue || c.Reason != ReasonCNIEnforced {
		t.Errorf("enforced: got %s/%s, want True/%s", c.Status, c.Reason, ReasonCNIEnforced)
	}

	c = cniEnforcementCondition(env, &CNIProbeResult{Enforced: false, Reason: ReasonCNINotEnforced, Detail: "connectivity was not blocked"}, now)
	if c.Status != metav1.ConditionFalse || c.Reason != ReasonCNINotEnforced {
		t.Errorf("not enforced: got %s/%s, want False/%s", c.Status, c.Reason, ReasonCNINotEnforced)
	}

	c = cniEnforcementCondition(env, &CNIProbeResult{Enforced: false, Reason: ReasonCNIProbeFailed, Detail: "probe pod failed"}, now)
	if c.Status != metav1.ConditionUnknown || c.Reason != ReasonCNIUnconfirmed {
		t.Errorf("probe failed: got %s/%s, want Unknown/%s", c.Status, c.Reason, ReasonCNIUnconfirmed)
	}
}

// TestWarnIfNetworkNotEnforced covers the warning-event decision: Restricted
// isolation with the probe not yet verified emits a Warning event; Restricted
// with the probe verified, or Open isolation, emits nothing; a nil recorder
// never panics.
func TestWarnIfNetworkNotEnforced(t *testing.T) {
	env := &sandboxv1alpha1.SandboxEnvironment{ObjectMeta: metav1.ObjectMeta{Name: "warn-env", Namespace: "default"}}
	restricted := &sandboxv1alpha1.SandboxClass{Spec: sandboxv1alpha1.SandboxClassSpec{
		Network: sandboxv1alpha1.NetworkSpec{Isolation: sandboxv1alpha1.NetworkIsolationRestricted},
	}}
	open := &sandboxv1alpha1.SandboxClass{Spec: sandboxv1alpha1.SandboxClassSpec{
		Network: sandboxv1alpha1.NetworkSpec{Isolation: sandboxv1alpha1.NetworkIsolationOpen},
	}}

	// Restricted + probe never run -> warning.
	rec := events.NewFakeRecorder(10)
	r := &Reconciler{Recorder: rec, CNI: nil}
	r.warnIfNetworkNotEnforced(env, restricted)
	select {
	case ev := <-rec.Events:
		if !strings.Contains(ev, "NetworkPolicyNotEnforced") {
			t.Errorf("event = %q, want NetworkPolicyNotEnforced", ev)
		}
	default:
		t.Error("Restricted + nil CNI: expected a warning event, got none")
	}

	// Restricted + probe verified -> no warning.
	rec = events.NewFakeRecorder(10)
	r = &Reconciler{Recorder: rec, CNI: cniResultPtr(CNIProbeResult{Enforced: true, Reason: ReasonCNIEnforced})}
	r.warnIfNetworkNotEnforced(env, restricted)
	select {
	case ev := <-rec.Events:
		t.Errorf("Restricted + enforced CNI: unexpected event %q", ev)
	default:
	}

	// Open + nil CNI -> no warning.
	rec = events.NewFakeRecorder(10)
	r = &Reconciler{Recorder: rec, CNI: nil}
	r.warnIfNetworkNotEnforced(env, open)
	select {
	case ev := <-rec.Events:
		t.Errorf("Open isolation: unexpected event %q", ev)
	default:
	}

	// Nil recorder -> no panic.
	r = &Reconciler{Recorder: nil, CNI: nil}
	r.warnIfNetworkNotEnforced(env, restricted)
}

// cniResultPtr wraps a CNIProbeResult in the atomic.Pointer the Reconciler's
// CNI field expects, the way CNIProbeRunnable publishes a whole new struct per
// pass.
func cniResultPtr(res CNIProbeResult) *atomic.Pointer[CNIProbeResult] {
	p := &atomic.Pointer[CNIProbeResult]{}
	p.Store(&res)
	return p
}
