package render

import (
	metav1ac "k8s.io/client-go/applyconfigurations/meta/v1"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
)

// Labels returns the label set applied to every child object rendered for
// env: the standard app.kubernetes.io/* recommended labels plus
// sandbox.psenna.dev/environment (this environment's truncated,
// collision-resistant identity, matching the truncation applied to child
// object names) and sandbox.psenna.dev/class (same truncation applied to the
// referenced SandboxClass's name -- classes are cluster-scoped so their name
// alone is already a valid label value in practice, but the truncation is
// applied uniformly for safety).
func Labels(env *v1alpha1.SandboxEnvironment) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":         "ai-sandbox",
		"app.kubernetes.io/instance":     EnvironmentLabelValue(env.Name),
		"app.kubernetes.io/component":    "sandbox-environment",
		"app.kubernetes.io/part-of":      "ai-sandbox",
		"app.kubernetes.io/managed-by":   "ai-sandbox-operator",
		"sandbox.psenna.dev/environment": EnvironmentLabelValue(env.Name),
		"sandbox.psenna.dev/class":       childName(env.Spec.ClassRef.Name, ""),
	}
}

// ownerReference builds the single controller owner reference applied to
// every child object, so deleting env garbage-collects the lot.
func ownerReference(env *v1alpha1.SandboxEnvironment) *metav1ac.OwnerReferenceApplyConfiguration {
	return metav1ac.OwnerReference().
		WithAPIVersion("sandbox.psenna.dev/v1alpha1").
		WithKind("SandboxEnvironment").
		WithName(env.Name).
		WithUID(env.UID).
		WithController(true).
		WithBlockOwnerDeletion(true)
}
