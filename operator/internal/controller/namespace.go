package controller

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/psenna/ai-sandbox/operator/internal/render"
)

// namespacePodSecurityEnforce returns ns's pod-security.kubernetes.io/enforce
// label value, or "" when the label is absent OR the read fails.
//
// FAILS OPEN by design: a namespace we cannot read is not evidence that the
// namespace forbids anything, and blocking a pod on a failed metadata read
// would turn a transient RBAC/API blip into a stuck environment. When it
// fails open and the namespace really does enforce restricted, the API server
// rejects the pod with its own message -- the pre-#24 behaviour, no worse.
//
// Namespace is cluster-scoped, so controller-runtime routes it to the
// cluster-wide cache even under --watch-namespace (multi_namespace_cache.go's
// globalCache), exactly like the cluster-scoped SandboxClass resolveClass
// already reads. Cached, so the two calls per reconcile (here and in
// ensurePod) cost nothing.
func (r *Reconciler) namespacePodSecurityEnforce(ctx context.Context, ns string) string {
	var namespace corev1.Namespace
	if err := r.Get(ctx, client.ObjectKey{Name: ns}, &namespace); err != nil {
		return ""
	}
	return namespace.Labels[render.PodSecurityEnforceLabel]
}
