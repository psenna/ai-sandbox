package controller

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
	"github.com/psenna/ai-sandbox/operator/internal/render"
)

// externalResolveTimeout bounds the DNS lookup the external-endpoint
// sharp-edge validation performs, so a slow or hanging DNS server cannot
// wedge a reconcile. On timeout the endpoint is accepted (coverage
// unverifiable), never errored -- the operator may legitimately lack
// external DNS.
const externalResolveTimeout = 3 * time.Second

// inClusterServiceRE matches an in-cluster Service DNS name of the form
// <svc>.<ns>.svc[.cluster.local] -- the only hostname form that can be
// resolved to a pod selector. Anything else is an external host.
var inClusterServiceRE = regexp.MustCompile(`^([^.]+)\.([^.]+)\.svc(\.cluster\.local)?$`)

// defaultOperatorIngressLabel is the label selector identifying the
// operator's own pods, allowed to reach sandbox pods under Restricted
// isolation. Overridable via the --operator-ingress-label flag (see
// internal/config/config.go and Reconciler.OperatorIngressLabel).
const defaultOperatorIngressLabel = "control-plane=controller-manager"

// serviceEndpoint names one configured service/storage URL the sandbox must
// be able to reach, with a human-readable description for error messages.
type serviceEndpoint struct {
	what string
	url  string
}

// serviceEndpoints enumerates every endpoint a sandbox created from class
// must reach: the git-proxy git+broker URLs, the DependaProxy npm/pypi/goproxy
// URLs, the Ollama base URL, and (for an S3 storage backend) the S3 endpoint.
// A PVC backend has no S3 endpoint, so it contributes no storage peer.
func serviceEndpoints(class *v1alpha1.SandboxClass) []serviceEndpoint {
	var eps []serviceEndpoint
	if gp := class.Spec.Services.GitProxy; gp != nil {
		eps = append(eps,
			serviceEndpoint{"gitProxy git", gp.GitURL},
			serviceEndpoint{"gitProxy broker", gp.BrokerURL},
		)
	}
	if dp := class.Spec.Services.DependaProxy; dp != nil {
		eps = append(eps,
			serviceEndpoint{"dependaProxy npm", dp.NpmURL},
			serviceEndpoint{"dependaProxy pypi", dp.PypiURL},
			serviceEndpoint{"dependaProxy goproxy", dp.GoproxyURL},
		)
	}
	if o := class.Spec.Services.Ollama; o != nil {
		eps = append(eps, serviceEndpoint{"ollama", o.BaseURL})
	}
	if b := class.Spec.Storage.Backend; b.Type == v1alpha1.StorageBackendTypeS3 && b.S3 != nil {
		eps = append(eps, serviceEndpoint{"storage s3", b.S3.Endpoint})
	}
	return eps
}

// resolveNetworkPeers resolves the class's configured service/storage
// endpoints and extraEgress entries into the render.NetworkInputs the
// Restricted NetworkPolicy needs, mirroring resolveCredentials: render stays
// pure, the controller does the cluster I/O (Service lookups) and the
// URL->selector/CIDR resolution.
//
// For Open isolation it returns an empty NetworkInputs immediately -- no
// policy is rendered, so nothing needs resolving and an external endpoint
// without an extraEgress CIDR is not an error.
//
// For Restricted isolation it always emits the K8s API peer (the
// `kubernetes` Service in `default`, as an ipBlock -- it has no pod
// selector), plus one peer per in-cluster service endpoint (resolved to the
// Service's own pod selector), plus every extraEgress entry verbatim. An
// external endpoint (no <svc>.<ns>.svc match) is a validation error unless
// the class declares at least one extraEgress CIDR peer: a selector cannot
// match an off-cluster host, so the class is simply misconfigured.
func (r *Reconciler) resolveNetworkPeers(ctx context.Context, class *v1alpha1.SandboxClass) (render.NetworkInputs, error) {
	if class.Spec.Network.Isolation != v1alpha1.NetworkIsolationRestricted {
		return render.NetworkInputs{}, nil // Open: no policy rendered, nothing to resolve
	}

	// Validate every extraEgress entry (exactly one of CIDR/Selector -- the
	// CRD's XValidation normally guarantees this, but a direct apiserver write
	// can bypass it, and the observer's validation pass must surface the real
	// cause rather than a swallowed render error) and collect the CIDRs for
	// the external-endpoint coverage check below.
	var cidrs []netip.Prefix
	for i, ep := range class.Spec.Network.ExtraEgress {
		if (ep.CIDR == "") == (ep.Selector == nil) {
			return render.NetworkInputs{}, fmt.Errorf("network: extraEgress[%d] must set exactly one of cidr or selector", i)
		}
		if ep.CIDR != "" {
			p, err := netip.ParsePrefix(ep.CIDR)
			if err != nil {
				return render.NetworkInputs{}, fmt.Errorf("network: extraEgress[%d] CIDR %q is not a valid CIDR: %w", i, ep.CIDR, err)
			}
			cidrs = append(cidrs, p)
		}
	}

	var peers []render.ResolvedPeer
	for _, ep := range serviceEndpoints(class) {
		if ep.url == "" {
			continue // optional endpoint not configured
		}
		peer, err := r.resolveServiceEndpoint(ctx, ep.what, ep.url, cidrs)
		if err != nil {
			return render.NetworkInputs{}, err
		}
		if peer.CIDR != "" || peer.Selector != nil {
			peers = append(peers, peer)
		}
	}

	// The K8s API server: the `kubernetes` Service in `default` fronts the
	// apiserver directly and has NO pod selector, so it must be an ipBlock
	// peer -- the same sharp edge as storage. The sandbox's in-cluster client
	// (rest.InClusterConfig) reaches this endpoint on 443.
	var k8sSvc corev1.Service
	if err := r.Get(ctx, client.ObjectKey{Namespace: "default", Name: "kubernetes"}, &k8sSvc); err != nil {
		return render.NetworkInputs{}, fmt.Errorf("network: reading kubernetes service in default: %w", err)
	}
	if k8sSvc.Spec.ClusterIP == "" {
		return render.NetworkInputs{}, fmt.Errorf("network: kubernetes service in default has no ClusterIP; cannot build the API-server egress peer")
	}
	peers = append(peers, render.ResolvedPeer{
		CIDR:  k8sSvc.Spec.ClusterIP + "/32",
		Ports: []v1alpha1.NetworkPolicyPort{{Port: "443"}},
	})

	// Fold in extraEgress peers verbatim (1:1: CIDR->CIDR, Selector->Selector,
	// Ports->Ports). Each entry was validated (exactly one of CIDR/Selector)
	// above; render validates again defensively.
	for _, ep := range class.Spec.Network.ExtraEgress {
		peers = append(peers, render.ResolvedPeer{CIDR: ep.CIDR, Selector: ep.Selector, Ports: ep.Ports})
	}

	return render.NetworkInputs{
		Egress:          peers,
		OperatorIngress: operatorIngressSelector(r.OperatorIngressLabel, r.ClassSecretNamespace),
	}, nil
}

// resolveServiceEndpoint resolves one configured endpoint URL to a
// render.ResolvedPeer. An in-cluster <svc>.<ns>.svc host becomes a pod
// selector peer built from the Service's own selector (a Get failure is a
// real misconfiguration, the same class as a missing Secret). Any other host
// is external: a selector cannot match an off-cluster host, so the peer MUST
// come from an extraEgress CIDR. The caller's cidrs are the class's parsed
// extraEgress CIDRs; the sharp-edge validation is:
//
//   - no CIDR at all -> the class cannot possibly work; loud error.
//   - CIDRs present -> resolve the host (bounded) and require at least one
//     CIDR to contain a resolved IP. If the operator cannot resolve the host
//     (no external DNS), accept -- coverage is unverifiable, not provably
//     absent, and the user's CIDR is the best available statement of intent.
//   - resolves but no CIDR covers any resolved IP -> loud error; the policy
//     would deny egress to this endpoint and the sandbox would hang.
//
// A covered external endpoint returns an empty peer (the caller drops it):
// the covering extraEgress CIDR is itself emitted as a peer, so the policy
// allows exactly that block.
func (r *Reconciler) resolveServiceEndpoint(ctx context.Context, what, rawURL string, cidrs []netip.Prefix) (render.ResolvedPeer, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return render.ResolvedPeer{}, fmt.Errorf("network: parsing %s endpoint %q: %w", what, rawURL, err)
	}
	host := u.Hostname()
	if m := inClusterServiceRE.FindStringSubmatch(host); m != nil {
		svcName, ns := m[1], m[2]
		var svc corev1.Service
		if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: svcName}, &svc); err != nil {
			return render.ResolvedPeer{}, fmt.Errorf("network: reading in-cluster service %s/%s for %s endpoint: %w", ns, svcName, what, err)
		}
		return render.ResolvedPeer{Selector: &v1alpha1.PeerSelector{
			Namespace:   ns,
			PodSelector: &metav1.LabelSelector{MatchLabels: svc.Spec.Selector},
		}}, nil
	}
	if len(cidrs) == 0 {
		return render.ResolvedPeer{}, fmt.Errorf("network: %s endpoint %q is external but spec.network.extraEgress declares no CIDR peer; a selector cannot match an off-cluster host", what, rawURL)
	}
	resolveCtx, cancel := context.WithTimeout(ctx, externalResolveTimeout)
	defer cancel()
	addrs, err := r.lookupHost(resolveCtx, host)
	if err != nil {
		// Unresolvable from the operator (no external DNS): coverage is
		// unverifiable, not provably absent. Accept; the user's CIDR is the
		// best available statement of intent.
		return render.ResolvedPeer{}, nil
	}
	for _, a := range addrs {
		for _, c := range cidrs {
			if c.Contains(a) {
				return render.ResolvedPeer{}, nil // covered by an extraEgress CIDR
			}
		}
	}
	return render.ResolvedPeer{}, fmt.Errorf("network: %s endpoint %q resolves to %v but no spec.network.extraEgress CIDR covers it; add a covering CIDR or point the endpoint at an in-cluster Service", what, rawURL, addrs)
}

// lookupHost resolves host to IP addresses, using the Reconciler's injectable
// LookupHost when set (tests fake resolution without DNS) and
// net.DefaultResolver.LookupHost otherwise.
func (r *Reconciler) lookupHost(ctx context.Context, host string) ([]netip.Addr, error) {
	if r.LookupHost != nil {
		return r.LookupHost(ctx, host)
	}
	ips, err := net.DefaultResolver.LookupHost(ctx, host)
	if err != nil {
		return nil, err
	}
	addrs := make([]netip.Addr, 0, len(ips))
	for _, ip := range ips {
		if a, err := netip.ParseAddr(ip); err == nil {
			addrs = append(addrs, a)
		}
	}
	return addrs, nil
}

// operatorIngressSelector builds the PeerSelector identifying the operator's
// own pods, allowed to reach sandbox pods under Restricted isolation. label
// is a single "key=value" selector (the --operator-ingress-label flag);
// empty or malformed values fall back to the default
// control-plane=controller-manager.
func operatorIngressSelector(label, namespace string) *v1alpha1.PeerSelector {
	if label == "" {
		label = defaultOperatorIngressLabel
	}
	k, v, ok := strings.Cut(label, "=")
	if !ok || k == "" {
		k, v = "control-plane", "controller-manager"
	}
	return &v1alpha1.PeerSelector{
		Namespace:   namespace,
		PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{k: v}},
	}
}
