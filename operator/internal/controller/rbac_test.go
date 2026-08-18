package controller

import (
	"os"
	"testing"

	authorizationv1 "k8s.io/api/authorization/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	sandboxv1alpha1 "github.com/psenna/ai-sandbox/operator/api/v1alpha1"
	"github.com/psenna/ai-sandbox/operator/internal/render"
)

// serviceAccountGroups returns the standard group set the API server
// attaches to a real ServiceAccount token for namespace ns.
func serviceAccountGroups(ns string) []string {
	return []string{"system:authenticated", "system:serviceaccounts", "system:serviceaccounts:" + ns}
}

// impersonatingClient builds a client.Client that impersonates userName with
// groups, so real Get/List/Patch calls exercise the exact same authorization
// decision a minted ServiceAccount token would (envtest's admin identity is
// system:masters, which can impersonate any user).
func impersonatingClient(t *testing.T, userName string, groups []string) client.Client {
	t.Helper()
	cfg := rest.CopyConfig(k8sCfg)
	cfg.Impersonate = rest.ImpersonationConfig{UserName: userName, Groups: groups}
	c, err := client.New(cfg, client.Options{Scheme: k8s.Scheme()})
	if err != nil {
		t.Fatalf("building impersonating client for %s: %v", userName, err)
	}
	return c
}

// mustSubjectAccessReview posts a real SubjectAccessReview and returns
// whether it was allowed.
func mustSubjectAccessReview(t *testing.T, userName string, groups []string, attrs authorizationv1.ResourceAttributes) bool {
	t.Helper()
	sar := &authorizationv1.SubjectAccessReview{
		Spec: authorizationv1.SubjectAccessReviewSpec{
			User:               userName,
			Groups:             groups,
			ResourceAttributes: &attrs,
		},
	}
	if err := k8s.Create(ctx, sar); err != nil {
		t.Fatalf("creating SubjectAccessReview: %v", err)
	}
	return sar.Status.Allowed
}

// TestSidecarServiceAccountAuthorization is the acceptance criterion's core
// test: the rendered sidecar ServiceAccount can get/patch its OWN
// SandboxEnvironment's status and nothing else -- asserted via real
// SubjectAccessReviews against envtest's real RBAC authorizer.
func TestSidecarServiceAccountAuthorization(t *testing.T) {
	mustCreateClass(t)
	env := mustCreateEnv(t, "sidecar-authz")
	otherEnv := mustCreateEnv(t, "sidecar-authz-other")
	key := types.NamespacedName{Namespace: env.Namespace, Name: env.Name}

	r := newResourceReconciler(t, newFakeClock(fixedStart))
	reconcileOnce(t, r, key)

	names := render.ChildNames(env.Name)
	user := "system:serviceaccount:" + env.Namespace + ":" + names.ServiceAccount
	groups := serviceAccountGroups(env.Namespace)

	cases := []struct {
		name        string
		attrs       authorizationv1.ResourceAttributes
		wantAllowed bool
	}{
		{"own env get", authorizationv1.ResourceAttributes{Namespace: env.Namespace, Verb: "get", Group: "sandbox.psenna.dev", Resource: "sandboxenvironments", Name: env.Name}, true},
		{"own env status get", authorizationv1.ResourceAttributes{Namespace: env.Namespace, Verb: "get", Group: "sandbox.psenna.dev", Resource: "sandboxenvironments", Subresource: "status", Name: env.Name}, true},
		{"own env status patch", authorizationv1.ResourceAttributes{Namespace: env.Namespace, Verb: "patch", Group: "sandbox.psenna.dev", Resource: "sandboxenvironments", Subresource: "status", Name: env.Name}, true},
		{"different env get", authorizationv1.ResourceAttributes{Namespace: otherEnv.Namespace, Verb: "get", Group: "sandbox.psenna.dev", Resource: "sandboxenvironments", Name: otherEnv.Name}, false},
		{"different env status patch", authorizationv1.ResourceAttributes{Namespace: otherEnv.Namespace, Verb: "patch", Group: "sandbox.psenna.dev", Resource: "sandboxenvironments", Subresource: "status", Name: otherEnv.Name}, false},
		{"sandboxenvironments list, no name", authorizationv1.ResourceAttributes{Namespace: env.Namespace, Verb: "list", Group: "sandbox.psenna.dev", Resource: "sandboxenvironments"}, false},
		{"secrets get, own rendered secret", authorizationv1.ResourceAttributes{Namespace: env.Namespace, Verb: "get", Group: "", Resource: "secrets", Name: names.Secret}, false},
		{"secrets get, own rendered snapshot-credentials secret", authorizationv1.ResourceAttributes{Namespace: env.Namespace, Verb: "get", Group: "", Resource: "secrets", Name: names.SnapshotSecret}, false},
		{"secrets get, any secret", authorizationv1.ResourceAttributes{Namespace: env.Namespace, Verb: "get", Group: "", Resource: "secrets"}, false},
		{"secrets list", authorizationv1.ResourceAttributes{Namespace: env.Namespace, Verb: "list", Group: "", Resource: "secrets"}, false},
		{"pods create", authorizationv1.ResourceAttributes{Namespace: env.Namespace, Verb: "create", Group: "", Resource: "pods"}, false},
		{"sandboxclasses get, cluster-scoped", authorizationv1.ResourceAttributes{Verb: "get", Group: "sandbox.psenna.dev", Resource: "sandboxclasses", Name: "default"}, false},
		{"configmaps get", authorizationv1.ResourceAttributes{Namespace: env.Namespace, Verb: "get", Group: "", Resource: "configmaps", Name: names.ConfigMap}, false},
		{"own env status update is forbidden", authorizationv1.ResourceAttributes{Namespace: env.Namespace, Verb: "update", Group: "sandbox.psenna.dev", Resource: "sandboxenvironments", Subresource: "status", Name: env.Name}, false},
		{"own env delete is forbidden", authorizationv1.ResourceAttributes{Namespace: env.Namespace, Verb: "delete", Group: "sandbox.psenna.dev", Resource: "sandboxenvironments", Name: env.Name}, false},
		{"own env spec patch is forbidden", authorizationv1.ResourceAttributes{Namespace: env.Namespace, Verb: "patch", Group: "sandbox.psenna.dev", Resource: "sandboxenvironments", Name: env.Name}, false},
		{"tokenreviews create is forbidden", authorizationv1.ResourceAttributes{Verb: "create", Group: "authentication.k8s.io", Resource: "tokenreviews"}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mustSubjectAccessReview(t, user, groups, tc.attrs)
			if got != tc.wantAllowed {
				t.Errorf("SubjectAccessReview(%+v) allowed=%v, want %v", tc.attrs, got, tc.wantAllowed)
			}
		})
	}
}

// TestSidecarServiceAccountImpersonatedCalls exercises the same
// authorization decision via real Get/List/Patch calls through an
// impersonating client, rather than SubjectAccessReview.
func TestSidecarServiceAccountImpersonatedCalls(t *testing.T) {
	mustCreateClass(t)
	env := mustCreateEnv(t, "sidecar-impersonated")
	otherEnv := mustCreateEnv(t, "sidecar-impersonated-other")
	key := types.NamespacedName{Namespace: env.Namespace, Name: env.Name}

	r := newResourceReconciler(t, newFakeClock(fixedStart))
	reconcileOnce(t, r, key)

	names := render.ChildNames(env.Name)
	user := "system:serviceaccount:" + env.Namespace + ":" + names.ServiceAccount
	imp := impersonatingClient(t, user, serviceAccountGroups(env.Namespace))

	t.Run("get own environment succeeds", func(t *testing.T) {
		var got sandboxv1alpha1.SandboxEnvironment
		if err := imp.Get(ctx, key, &got); err != nil {
			t.Errorf("Get own environment: %v", err)
		}
	})

	t.Run("get own rendered secret is forbidden", func(t *testing.T) {
		var s corev1.Secret
		err := imp.Get(ctx, types.NamespacedName{Namespace: env.Namespace, Name: names.Secret}, &s)
		if !apierrors.IsForbidden(err) {
			t.Errorf("Get own rendered Secret: err = %v, want Forbidden", err)
		}
	})

	t.Run("list environments in namespace is forbidden", func(t *testing.T) {
		var list sandboxv1alpha1.SandboxEnvironmentList
		err := imp.List(ctx, &list, client.InNamespace(env.Namespace))
		if !apierrors.IsForbidden(err) {
			t.Errorf("List environments: err = %v, want Forbidden", err)
		}
	})

	t.Run("patch a different environment's status is forbidden", func(t *testing.T) {
		patch := client.MergeFrom(otherEnv.DeepCopy())
		modified := otherEnv.DeepCopy()
		modified.Status.Phase = sandboxv1alpha1.PhaseFailed
		err := imp.Status().Patch(ctx, modified, patch)
		if !apierrors.IsForbidden(err) {
			t.Errorf("Patch different environment's status: err = %v, want Forbidden", err)
		}
	})
}

// TestOperatorRBACIsSufficient reads and parses the real, generated
// config/rbac/role.yaml, creates it for real (under a test-local name) plus
// a ClusterRoleBinding, builds an impersonating client using that identity,
// and applies every rendered child object through it -- directly, not
// through ensureResources, because ensureResources deliberately treats a
// Forbidden apply as a permanent misconfiguration and swallows it (see
// resources.go's applyOne), which would make this test vacuously pass even
// with insufficient RBAC. This is what actually proves the +kubebuilder:rbac
// markers on sandboxenvironment_controller.go are correct and sufficient --
// the normal admin-privileged suite client would silently hide a missing
// marker.
func TestOperatorRBACIsSufficient(t *testing.T) {
	role := loadOperatorClusterRole(t)
	role.Name = "rbac-sufficiency-test"
	role.ResourceVersion = ""
	if err := k8s.Create(ctx, role); err != nil {
		t.Fatalf("creating test ClusterRole: %v", err)
	}
	t.Cleanup(func() { _ = k8s.Delete(ctx, role) })

	const testUser = "system:serviceaccount:default:operator-under-test"
	binding := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "rbac-sufficiency-test-binding"},
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: role.Name},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: "operator-under-test", Namespace: "default"}},
	}
	if err := k8s.Create(ctx, binding); err != nil {
		t.Fatalf("creating test ClusterRoleBinding: %v", err)
	}
	t.Cleanup(func() { _ = k8s.Delete(ctx, binding) })

	imp := impersonatingClient(t, testUser, []string{"system:authenticated", "system:serviceaccounts", "system:serviceaccounts:default"})

	class := mustCreateClass(t)
	env := mustCreateEnv(t, "rbac-sufficiency-env")

	objs, err := render.Render(render.Inputs{Env: env, Class: class, ClusterID: "test"})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	for _, ac := range objs.All() {
		if err := imp.Apply(ctx, ac, client.FieldOwner(fieldManager), client.ForceOwnership); err != nil {
			if apierrors.IsForbidden(err) {
				t.Errorf("apply forbidden under operator ClusterRole: %v", err)
				continue
			}
			t.Fatalf("apply failed for a non-RBAC reason: %v", err)
		}
	}

	// The operator also needs to read/patch the environment itself.
	var got sandboxv1alpha1.SandboxEnvironment
	if err := imp.Get(ctx, types.NamespacedName{Namespace: env.Namespace, Name: env.Name}, &got); err != nil {
		t.Errorf("Get SandboxEnvironment under operator ClusterRole: %v", err)
	}
	patch := client.MergeFrom(got.DeepCopy())
	modified := got.DeepCopy()
	modified.Status.ObservedGeneration = got.Generation
	if err := imp.Status().Patch(ctx, modified, patch); err != nil {
		t.Errorf("Status().Patch SandboxEnvironment under operator ClusterRole: %v", err)
	}

	// The batch/jobs marker (#28): freezePod's ensureSnapshotJob needs
	// get/list/watch/create/delete on Job -- exercised here directly,
	// exactly like the objs.All() loop above, so a missing/insufficient
	// marker fails this test rather than being silently swallowed by
	// applyOne's Forbidden-is-permanent handling.
	job, err := render.RenderSnapshotJob(render.Inputs{Env: env, Class: class, ClusterID: "test", SidecarImage: "test-sidecar:test"})
	if err != nil {
		t.Fatalf("RenderSnapshotJob: %v", err)
	}
	if err := imp.Apply(ctx, job, client.FieldOwner(fieldManager), client.ForceOwnership); err != nil {
		t.Errorf("apply Job forbidden under operator ClusterRole: %v", err)
	}
	names := render.ChildNames(env.Name)
	var gotJob batchv1.Job
	if err := imp.Get(ctx, types.NamespacedName{Namespace: env.Namespace, Name: names.SnapshotJob}, &gotJob); err != nil {
		t.Errorf("Get Job under operator ClusterRole: %v", err)
	}
	if err := imp.Delete(ctx, &gotJob); err != nil {
		t.Errorf("Delete Job under operator ClusterRole: %v", err)
	}
}

// TestOperatorRBACIsSufficient_CatchesMissingMarker is the positive control
// for TestOperatorRBACIsSufficient: it strips the secrets rule out of the
// parsed ClusterRole before creating it, and asserts that applying the
// Secret child object under that stripped role DOES come back Forbidden.
// This proves the surrounding test methodology actually distinguishes
// sufficient from insufficient RBAC, rather than passing vacuously.
func TestOperatorRBACIsSufficient_CatchesMissingMarker(t *testing.T) {
	role := loadOperatorClusterRole(t)
	role.Name = "rbac-sufficiency-negative-test"
	role.ResourceVersion = ""

	kept := role.Rules[:0]
	for _, rule := range role.Rules {
		isSecretsRule := false
		for _, res := range rule.Resources {
			if res == "secrets" {
				isSecretsRule = true
			}
		}
		if !isSecretsRule {
			kept = append(kept, rule)
		}
	}
	role.Rules = kept

	if err := k8s.Create(ctx, role); err != nil {
		t.Fatalf("creating stripped test ClusterRole: %v", err)
	}
	t.Cleanup(func() { _ = k8s.Delete(ctx, role) })

	const testUser = "system:serviceaccount:default:operator-under-test-negative"
	binding := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "rbac-sufficiency-negative-binding"},
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: role.Name},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: "operator-under-test-negative", Namespace: "default"}},
	}
	if err := k8s.Create(ctx, binding); err != nil {
		t.Fatalf("creating test ClusterRoleBinding: %v", err)
	}
	t.Cleanup(func() { _ = k8s.Delete(ctx, binding) })

	imp := impersonatingClient(t, testUser, []string{"system:authenticated", "system:serviceaccounts", "system:serviceaccounts:default"})

	class := mustCreateClass(t)
	env := mustCreateEnv(t, "rbac-negative-env")

	objs, err := render.Render(render.Inputs{Env: env, Class: class, ClusterID: "test"})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	err = imp.Apply(ctx, objs.Secret, client.FieldOwner(fieldManager), client.ForceOwnership)
	if !apierrors.IsForbidden(err) {
		t.Fatalf("applying Secret under a ClusterRole missing the secrets marker: err = %v, want Forbidden (positive control failed)", err)
	}
}

// loadOperatorClusterRole reads and parses the real, generated
// config/rbac/role.yaml into a rbacv1.ClusterRole.
func loadOperatorClusterRole(t *testing.T) *rbacv1.ClusterRole {
	t.Helper()
	raw, err := os.ReadFile("../../config/rbac/role.yaml")
	if err != nil {
		t.Fatalf("reading config/rbac/role.yaml: %v", err)
	}
	role := &rbacv1.ClusterRole{}
	if err := yaml.Unmarshal(raw, role); err != nil {
		t.Fatalf("unmarshaling config/rbac/role.yaml: %v", err)
	}
	return role
}
