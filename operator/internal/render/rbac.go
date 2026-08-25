package render

import (
	acrbacv1 "k8s.io/client-go/applyconfigurations/rbac/v1"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
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
//
// When the class engine is k8s-native (#24, Plan 2), the Role is widened with
// three extra rules gated on engine==k8s-native for least-privilege: none and
// rootless-podman envs get NO servicesets/pods-exec rules. k8s-native adds
// the ServiceSet control model: the sidecar upserts one ServiceSet CR named
// after the environment (the CR name == env.Name), and one-shot execs into
// declared runtime pods (POST /v1/exec -> SPDY pods/exec).
//
// The servicesets grant is split across two rules because Kubernetes RBAC
// does NOT honor resourceNames for the create verb: on a POST the object
// name lives in the request body, not the URL, so the authorizer evaluates a
// create with an empty resource name and a resourceNames-pinned rule matches
// NO real create -- the env's own included. Granting servicesets create WITH
// resourceNames=[env.Name] therefore authorizes nothing, and the sidecar's
// services-apply is Forbidden (the e2e failure this split fixes, guarded by
// controller/rbac_test.go's TestSidecarServiceAccountAuthorization_K8sNative
// via a real SubjectAccessReview and an impersonated Create). So:
//   - get/update keep resourceNames=[env.Name]: those verbs read the name
//     from the URL, resourceNames IS honored, and the pin scopes them to the
//     env's own ServiceSet -- this is what proves environment-scoping.
//   - create is granted WITHOUT resourceNames (namespace-scoped only), the
//     same treatment pods/exec gets below. The sidecar's code creates exactly
//     one ServiceSet named env.Name (internal/sandboxctl/serviceset_store.go),
//     so the unpinned create is inert in practice; the get/update name-pin
//     still proves environment-scoping for the verbs that can be pinned.
//
// pods/exec likewise cannot be name-pinned: runtime pod names are dynamic
// (declared in services.yaml at runtime), so it is namespace-scoped only --
// the same per-subresource-not-per-field honesty as the status rule above.
// No list/watch on servicesets: the sidecar upserts by name, never lists.
func renderRole(in Inputs) *acrbacv1.RoleApplyConfiguration {
	names := ChildNames(in.Env.Name)
	rules := []*acrbacv1.PolicyRuleApplyConfiguration{
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
	}
	if in.Class != nil && in.Class.Spec.Engine.Type == v1alpha1.EngineTypeK8sNative {
		rules = append(rules,
			// servicesets get/update: name-pinned to the env's own ServiceSet
			// (resourceNames IS honored for get/update -- the name is in the URL).
			acrbacv1.PolicyRule().
				WithAPIGroups("sandbox.psenna.dev").
				WithResources("servicesets").
				WithResourceNames(in.Env.Name).
				WithVerbs("get", "update"),
			// servicesets create: NOT name-pinned -- Kubernetes RBAC ignores
			// resourceNames for create (see the doc comment above). Namespace-
			// scoped only; the sidecar creates exactly one ServiceSet named
			// env.Name, so this is inert in practice.
			acrbacv1.PolicyRule().
				WithAPIGroups("sandbox.psenna.dev").
				WithResources("servicesets").
				WithVerbs("create"),
			acrbacv1.PolicyRule().
				WithAPIGroups("").
				WithResources("pods/exec").
				WithVerbs("create"),
		)
	}
	return acrbacv1.Role(names.Role, in.Env.Namespace).
		WithLabels(Labels(in.Env)).
		WithOwnerReferences(ownerReference(in.Env)).
		WithRules(rules...)
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
