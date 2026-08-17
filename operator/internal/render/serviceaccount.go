package render

import (
	acorev1 "k8s.io/client-go/applyconfigurations/core/v1"
)

// renderServiceAccount renders the per-environment ServiceAccount the
// sidecar uses to patch its own SandboxEnvironment's status. automount is
// explicitly false: SA-level automount would inject a token into every
// container in the pod, but the agent container (running LLM-authored code)
// must never hold a Kubernetes credential -- #21 mounts this identity into
// the sidecar container only, via an explicit projected serviceAccountToken
// volume, and additionally sets automountServiceAccountToken: false on the
// pod spec itself as a second layer.
func renderServiceAccount(in Inputs) *acorev1.ServiceAccountApplyConfiguration {
	names := ChildNames(in.Env.Name)
	automount := false
	return acorev1.ServiceAccount(names.ServiceAccount, in.Env.Namespace).
		WithLabels(Labels(in.Env)).
		WithOwnerReferences(ownerReference(in.Env)).
		WithAutomountServiceAccountToken(automount)
}
