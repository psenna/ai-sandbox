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
//
// Honest limitation, recorded rather than silently relied upon: RBAC is
// per-subresource, not per-field. This Role technically permits the sidecar
// to write ANY field on its own environment's status, not just waitFor/
// agentResult. The field-level restriction -- sandboxctl (#27) only ever
// touches those two fields -- is enforced by sandboxctl's own code
// (internal/sandboxctl/store.go's patchStatus mutators) and asserted by its
// unit tests checking the exact merge-patch body
// (internal/sandboxctl/store_test.go), not by RBAC. What RBAC DOES prove,
// and what internal/controller/sidecarpatch_test.go and rbac_test.go assert
// against a real authorizer, is the environment-scoping: this identity can
// never patch a DIFFERENT environment's status at all.
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
