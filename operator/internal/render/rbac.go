package render

import (
	acrbacv1 "k8s.io/client-go/applyconfigurations/rbac/v1"
)

// renderRole renders the namespaced Role granting the sidecar ServiceAccount
// only what it needs: get on its own SandboxEnvironment, get/patch on its
// own status subresource. resourceNames pins the grant to the environment's
// FULL metadata.name (never the truncated/hashed child name -- the
// SandboxEnvironment object itself is never renamed). No list/watch/
// create/update/delete, no secrets, nothing cluster-scoped.
func renderRole(in Inputs) *acrbacv1.RoleApplyConfiguration {
	names := ChildNames(in.Env.Name)
	return acrbacv1.Role(names.Role, in.Env.Namespace).
		WithLabels(Labels(in.Env)).
		WithOwnerReferences(ownerReference(in.Env)).
		WithRules(
			acrbacv1.PolicyRule().
				WithAPIGroups("sandbox.psenna.dev").
				WithResources("sandboxenvironments").
				WithResourceNames(in.Env.Name).
				WithVerbs("get"),
			acrbacv1.PolicyRule().
				WithAPIGroups("sandbox.psenna.dev").
				WithResources("sandboxenvironments/status").
				WithResourceNames(in.Env.Name).
				WithVerbs("get", "patch"),
		)
}

// renderRoleBinding renders the RoleBinding that binds renderRole's Role to
// the environment's rendered ServiceAccount.
func renderRoleBinding(in Inputs) *acrbacv1.RoleBindingApplyConfiguration {
	names := ChildNames(in.Env.Name)
	return acrbacv1.RoleBinding(names.RoleBinding, in.Env.Namespace).
		WithLabels(Labels(in.Env)).
		WithOwnerReferences(ownerReference(in.Env)).
		WithRoleRef(acrbacv1.RoleRef().
			WithAPIGroup("rbac.authorization.k8s.io").
			WithKind("Role").
			WithName(names.Role)).
		WithSubjects(acrbacv1.Subject().
			WithKind("ServiceAccount").
			WithName(names.ServiceAccount).
			WithNamespace(in.Env.Namespace))
}
