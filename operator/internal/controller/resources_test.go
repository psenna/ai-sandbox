package controller

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sandboxv1alpha1 "github.com/psenna/ai-sandbox/operator/api/v1alpha1"
	"github.com/psenna/ai-sandbox/operator/internal/render"
)

// mustGetObj fetches obj (a pointer to a concrete client.Object type) by
// namespace/name, failing the test on any error including NotFound.
func mustGetObj(t *testing.T, ns, name string, obj client.Object) {
	t.Helper()
	if err := k8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, obj); err != nil {
		t.Fatalf("Get(%s/%s): %v", ns, name, err)
	}
}

func boolPtr(b bool) *bool { return &b }

func TestEnsureResources_CreatesAllChildren(t *testing.T) {
	mustCreateSourceSecret(t, "default", "git-proxy-token", "token", "fake-token-create-all")
	mustCreateClassWithGitProxy(t, "default", "git-proxy-token", "token")
	env := mustCreateEnv(t, "create-all-children")
	key := types.NamespacedName{Namespace: env.Namespace, Name: env.Name}

	r := newResourceReconciler(t, newFakeClock(fixedStart))
	reconcileOnce(t, r, key)

	fresh := getEnv(t, key)
	names := render.ChildNames(fresh.Name)
	wantLabels := render.Labels(fresh)

	checkOwned := func(kind, name string, obj client.Object) {
		t.Helper()
		refs := obj.GetOwnerReferences()
		if len(refs) != 1 {
			t.Errorf("%s %s has %d owner references, want 1", kind, name, len(refs))
			return
		}
		ref := refs[0]
		if ref.UID != fresh.UID || ref.Controller == nil || !*ref.Controller || ref.BlockOwnerDeletion == nil || !*ref.BlockOwnerDeletion {
			t.Errorf("%s %s owner reference = %+v, want controller=true blockOwnerDeletion=true uid=%s", kind, name, ref, fresh.UID)
		}
		labels := obj.GetLabels()
		for k, v := range wantLabels {
			if labels[k] != v {
				t.Errorf("%s %s label %s = %q, want %q", kind, name, k, labels[k], v)
			}
		}
	}

	var sa corev1.ServiceAccount
	mustGetObj(t, key.Namespace, names.ServiceAccount, &sa)
	checkOwned("ServiceAccount", names.ServiceAccount, &sa)

	var role rbacv1.Role
	mustGetObj(t, key.Namespace, names.Role, &role)
	checkOwned("Role", names.Role, &role)

	var rb rbacv1.RoleBinding
	mustGetObj(t, key.Namespace, names.RoleBinding, &rb)
	checkOwned("RoleBinding", names.RoleBinding, &rb)

	var pvc corev1.PersistentVolumeClaim
	mustGetObj(t, key.Namespace, names.PVC, &pvc)
	checkOwned("PersistentVolumeClaim", names.PVC, &pvc)

	var cm corev1.ConfigMap
	mustGetObj(t, key.Namespace, names.ConfigMap, &cm)
	checkOwned("ConfigMap", names.ConfigMap, &cm)

	var secret corev1.Secret
	mustGetObj(t, key.Namespace, names.Secret, &secret)
	checkOwned("Secret", names.Secret, &secret)

	if string(secret.Data["AGENT_TOKEN"]) != "fake-token-create-all" {
		t.Errorf("Secret AGENT_TOKEN = %q, want fake-token-create-all", secret.Data["AGENT_TOKEN"])
	}
}

func TestEnsureResources_IsIdempotent(t *testing.T) {
	mustCreateClass(t)
	env := mustCreateEnv(t, "idempotent-resources")
	key := types.NamespacedName{Namespace: env.Namespace, Name: env.Name}

	r := newResourceReconciler(t, newFakeClock(fixedStart))
	// The very first apply of an object is a create; the API server's own
	// defaulting (e.g. PVC's volumeMode) settles field-manager ownership on
	// the SECOND apply against an already-existing object, which is a real,
	// one-time, harmless metadata write (confirmed empirically against
	// envtest: iteration 0 creates, iteration 1 bumps resourceVersion once,
	// iteration 2+ is stable). Reconcile twice before capturing the
	// "before" snapshot so this settling has already happened, and the
	// idempotency assertion below tests genuine steady-state no-op applies.
	reconcileOnce(t, r, key)
	reconcileOnce(t, r, key)

	names := render.ChildNames(env.Name)
	var cmBefore, cmAfter corev1.ConfigMap
	mustGetObj(t, key.Namespace, names.ConfigMap, &cmBefore)
	var pvcBefore, pvcAfter corev1.PersistentVolumeClaim
	mustGetObj(t, key.Namespace, names.PVC, &pvcBefore)

	for i := 0; i < 2; i++ {
		reconcileOnce(t, r, key)
	}

	mustGetObj(t, key.Namespace, names.ConfigMap, &cmAfter)
	mustGetObj(t, key.Namespace, names.PVC, &pvcAfter)

	if cmBefore.ResourceVersion != cmAfter.ResourceVersion {
		t.Errorf("ConfigMap resourceVersion changed: %s -> %s, want unchanged (idempotent apply)", cmBefore.ResourceVersion, cmAfter.ResourceVersion)
	}
	if pvcBefore.ResourceVersion != pvcAfter.ResourceVersion {
		t.Errorf("PVC resourceVersion changed: %s -> %s, want unchanged (idempotent apply)", pvcBefore.ResourceVersion, pvcAfter.ResourceVersion)
	}
}

func TestEnsureResources_RevertsManualEdits(t *testing.T) {
	mustCreateClass(t)
	env := mustCreateEnv(t, "reverts-manual-edits")
	key := types.NamespacedName{Namespace: env.Namespace, Name: env.Name}

	r := newResourceReconciler(t, newFakeClock(fixedStart))
	reconcileOnce(t, r, key)

	names := render.ChildNames(env.Name)

	var cm corev1.ConfigMap
	mustGetObj(t, key.Namespace, names.ConfigMap, &cm)
	originalSandboxJSON := cm.Data["sandbox.json"]
	cm.Data["sandbox.json"] = "tampered"
	if cm.Labels == nil {
		cm.Labels = map[string]string{}
	}
	cm.Labels["app.kubernetes.io/component"] = "tampered"
	// Also add an unrelated key -- SSA only reverts fields THIS operator
	// owns, so a foreign key addition to the same map is not removed. This
	// is correct, honest SSA behavior, not a bug: the field manager that
	// added "foreign-key" owns that map entry, and our apply's
	// ForceOwnership only reclaims fields it itself declares.
	cm.Data["foreign-key"] = "left-alone"
	if err := k8s.Update(ctx, &cm); err != nil {
		t.Fatalf("tampering with ConfigMap: %v", err)
	}

	var secret corev1.Secret
	mustGetObj(t, key.Namespace, names.Secret, &secret)
	originalRepo := string(secret.Data["GITHUB_REPO"])
	secret.Data["GITHUB_REPO"] = []byte("tampered-repo")
	if err := k8s.Update(ctx, &secret); err != nil {
		t.Fatalf("tampering with Secret: %v", err)
	}

	reconcileOnce(t, r, key)

	var cmAfter corev1.ConfigMap
	mustGetObj(t, key.Namespace, names.ConfigMap, &cmAfter)
	if cmAfter.Data["sandbox.json"] != originalSandboxJSON {
		t.Errorf("ConfigMap sandbox.json not reverted: got %q", cmAfter.Data["sandbox.json"])
	}
	if cmAfter.Labels["app.kubernetes.io/component"] != "sandbox-environment" {
		t.Errorf("ConfigMap label not reverted: got %q", cmAfter.Labels["app.kubernetes.io/component"])
	}
	if v, ok := cmAfter.Data["foreign-key"]; !ok || v != "left-alone" {
		t.Errorf("foreign ConfigMap key was removed (should be left alone): %q, ok=%v", v, ok)
	}

	var secretAfter corev1.Secret
	mustGetObj(t, key.Namespace, names.Secret, &secretAfter)
	if string(secretAfter.Data["GITHUB_REPO"]) != originalRepo {
		t.Errorf("Secret GITHUB_REPO not reverted: got %q, want %q", secretAfter.Data["GITHUB_REPO"], originalRepo)
	}
}

func TestEnsureResources_MissingSourceSecretHoldsPending(t *testing.T) {
	mustCreateClassWithGitProxy(t, "default", "does-not-exist", "token")
	env := mustCreateEnv(t, "missing-source-secret")
	key := types.NamespacedName{Namespace: env.Namespace, Name: env.Name}

	r := newResourceReconciler(t, newFakeClock(fixedStart))
	reconcileOnce(t, r, key)

	got := getEnv(t, key)
	if got.Status.Phase != sandboxv1alpha1.PhasePending {
		t.Errorf("Phase = %s, want Pending", got.Status.Phase)
	}
	c := findCondition(got, "Scheduled")
	if c == nil {
		t.Fatal("Scheduled condition missing")
	}
	for _, want := range []string{"default", "does-not-exist", "token"} {
		if !strings.Contains(c.Message, want) {
			t.Errorf("Scheduled condition message %q does not mention %q", c.Message, want)
		}
	}

	names := render.ChildNames(env.Name)
	var secret corev1.Secret
	err := k8s.Get(ctx, types.NamespacedName{Namespace: env.Namespace, Name: names.Secret}, &secret)
	if !apierrors.IsNotFound(err) {
		t.Errorf("expected no Secret child to be created, got err=%v", err)
	}
}

func TestEnsureResources_OwnershipGuard(t *testing.T) {
	mustCreateClass(t)
	env := mustCreateEnv(t, "ownership-guard")
	key := types.NamespacedName{Namespace: env.Namespace, Name: env.Name}

	names := render.ChildNames(env.Name)
	foreign := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      names.ConfigMap,
			Namespace: env.Namespace,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion:         "v1",
					Kind:               "ConfigMap",
					Name:               "unrelated-owner",
					UID:                types.UID("22222222-2222-2222-2222-222222222222"),
					Controller:         boolPtr(true),
					BlockOwnerDeletion: boolPtr(true),
				},
			},
		},
		Data: map[string]string{"do-not-touch": "original"},
	}
	if err := k8s.Create(ctx, foreign); err != nil {
		t.Fatalf("pre-creating foreign ConfigMap: %v", err)
	}
	t.Cleanup(func() { _ = k8s.Delete(ctx, foreign) })

	r := newResourceReconciler(t, newFakeClock(fixedStart))
	// First reconcile: observeCluster runs BEFORE ensureResources creates
	// anything, so it reports the FIRST missing child (ServiceAccount), not
	// yet the ConfigMap ownership conflict. That first reconcile's
	// ensureResources call then creates every child EXCEPT the ConfigMap
	// (skipped by the pre-apply ownership guard). The second reconcile's
	// observeCluster call is what actually observes the foreign ConfigMap.
	reconcileOnce(t, r, key)
	reconcileOnce(t, r, key)

	got := getEnv(t, key)
	c := findCondition(got, "Scheduled")
	if c == nil || !strings.Contains(c.Message, "ConfigMap") || !strings.Contains(c.Message, "owned by another object") {
		t.Errorf("Scheduled condition = %+v, want message mentioning ConfigMap owned by another object", c)
	}

	var after corev1.ConfigMap
	mustGetObj(t, env.Namespace, names.ConfigMap, &after)
	if after.Data["do-not-touch"] != "original" {
		t.Errorf("foreign ConfigMap was modified: %q", after.Data["do-not-touch"])
	}
	if len(after.OwnerReferences) != 1 || after.OwnerReferences[0].Name != "unrelated-owner" {
		t.Errorf("foreign ConfigMap ownerReferences changed: %+v", after.OwnerReferences)
	}
}

func TestEnsureResources_LongEnvironmentName(t *testing.T) {
	mustCreateClass(t)
	longName := strings.Repeat("a", 190) + "-" + strings.Repeat("b", 9)
	env := mustCreateEnv(t, longName)
	key := types.NamespacedName{Namespace: env.Namespace, Name: env.Name}

	r := newResourceReconciler(t, newFakeClock(fixedStart))
	reconcileOnce(t, r, key)

	names := render.ChildNames(longName)
	var sa corev1.ServiceAccount
	mustGetObj(t, key.Namespace, names.ServiceAccount, &sa)
	var pvc corev1.PersistentVolumeClaim
	mustGetObj(t, key.Namespace, names.PVC, &pvc)
	var cm corev1.ConfigMap
	mustGetObj(t, key.Namespace, names.ConfigMap, &cm)
	var secret corev1.Secret
	mustGetObj(t, key.Namespace, names.Secret, &secret)
	var role rbacv1.Role
	mustGetObj(t, key.Namespace, names.Role, &role)
	var rb rbacv1.RoleBinding
	mustGetObj(t, key.Namespace, names.RoleBinding, &rb)
}

func TestEnsureResources_OwnerReferencesEnableGC(t *testing.T) {
	// envtest runs no kube-controller-manager, so actual cascading deletion
	// on CR delete cannot be observed here; real end-to-end GC verification
	// belongs in issue #22's kind-based e2e harness. This test only asserts
	// the ownerReference SHAPE that garbage collection depends on.
	mustCreateClass(t)
	env := mustCreateEnv(t, "owner-refs-shape")
	key := types.NamespacedName{Namespace: env.Namespace, Name: env.Name}

	r := newResourceReconciler(t, newFakeClock(fixedStart))
	reconcileOnce(t, r, key)

	fresh := getEnv(t, key)
	names := render.ChildNames(fresh.Name)

	var pvc corev1.PersistentVolumeClaim
	mustGetObj(t, key.Namespace, names.PVC, &pvc)
	if len(pvc.OwnerReferences) != 1 {
		t.Fatalf("PVC has %d owner references, want 1", len(pvc.OwnerReferences))
	}
	ref := pvc.OwnerReferences[0]
	if ref.APIVersion != "sandbox.psenna.dev/v1alpha1" || ref.Kind != "SandboxEnvironment" ||
		ref.Name != fresh.Name || ref.UID != fresh.UID ||
		ref.Controller == nil || !*ref.Controller ||
		ref.BlockOwnerDeletion == nil || !*ref.BlockOwnerDeletion {
		t.Errorf("PVC owner reference = %+v, want a full controller reference to %s", ref, fresh.Name)
	}
}
