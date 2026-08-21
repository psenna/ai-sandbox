package controller

import (
	"context"
	"errors"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sandboxv1alpha1 "github.com/psenna/ai-sandbox/operator/api/v1alpha1"
	"github.com/psenna/ai-sandbox/operator/internal/lifecycle"
	"github.com/psenna/ai-sandbox/operator/internal/metrics"
	"github.com/psenna/ai-sandbox/operator/internal/render"
)

// assertArchiveJobExists fails the test unless a Job named name exists in
// ns.
func assertArchiveJobExists(t *testing.T, ns, name string) {
	t.Helper()
	var job batchv1.Job
	if err := k8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &job); err != nil {
		t.Fatalf("Get(archive Job %s/%s): %v, want it to exist", ns, name, err)
	}
}

// assertNoArchiveJob fails the test if a Job named name exists in ns.
func assertNoArchiveJob(t *testing.T, ns, name string) {
	t.Helper()
	var job batchv1.Job
	err := k8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &job)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("Get(archive Job %s/%s): err=%v, want NotFound (no archive Job should have been created)", ns, name, err)
	}
}

// mustAddFinalizerAndDelete sets v1alpha1.FinalizerArchiveOnDelete on env
// (mirroring what ensureFinalizer would have added on an earlier, real
// reconcile) and deletes it, leaving the object present with a
// DeletionTimestamp -- the exact state reconcileDelete is invoked against.
func mustAddFinalizerAndDelete(t *testing.T, env *sandboxv1alpha1.SandboxEnvironment) {
	t.Helper()
	fresh := &sandboxv1alpha1.SandboxEnvironment{}
	if err := k8s.Get(ctx, types.NamespacedName{Namespace: env.Namespace, Name: env.Name}, fresh); err != nil {
		t.Fatalf("Get before adding finalizer: %v", err)
	}
	fresh.Finalizers = append(fresh.Finalizers, sandboxv1alpha1.FinalizerArchiveOnDelete)
	if err := k8s.Update(ctx, fresh); err != nil {
		t.Fatalf("adding archive finalizer: %v", err)
	}
	if err := k8s.Delete(ctx, fresh); err != nil {
		t.Fatalf("Delete: %v", err)
	}
}

// mustCreateOwnedPod creates env's agent Pod, owned by env via a controller
// owner reference (the exact shape RenderPod produces), with phase set
// directly via Status().Update -- envtest runs no kubelet, so nothing else
// ever advances a Pod's phase. Mirrors mustCreateOwnedPVC.
func mustCreateOwnedPod(t *testing.T, env *sandboxv1alpha1.SandboxEnvironment, phase corev1.PodPhase) *corev1.Pod {
	t.Helper()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      render.ChildNames(env.Name).Pod,
			Namespace: env.Namespace,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: sandboxv1alpha1.GroupVersion.String(),
				Kind:       "SandboxEnvironment",
				Name:       env.Name,
				UID:        env.UID,
				Controller: ptr.To(true),
			}},
		},
		Spec: corev1.PodSpec{
			Containers:    []corev1.Container{{Name: "agent", Image: "busybox"}},
			RestartPolicy: corev1.RestartPolicyNever,
		},
	}
	if err := k8s.Create(ctx, pod); err != nil {
		t.Fatalf("creating owned pod: %v", err)
	}
	pod.Status.Phase = phase
	if err := k8s.Status().Update(ctx, pod); err != nil {
		t.Fatalf("setting pod phase: %v", err)
	}
	return pod
}

// assertEnvGone polls once (envtest deletes are synchronous once the last
// finalizer clears) that key no longer exists.
func assertEnvGone(t *testing.T, key types.NamespacedName) {
	t.Helper()
	env := &sandboxv1alpha1.SandboxEnvironment{}
	err := k8s.Get(ctx, key, env)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("Get(%s) after finalizer removal: err=%v, want NotFound", key, err)
	}
}

func assertEnvStillPresent(t *testing.T, key types.NamespacedName) *sandboxv1alpha1.SandboxEnvironment {
	t.Helper()
	env := &sandboxv1alpha1.SandboxEnvironment{}
	if err := k8s.Get(ctx, key, env); err != nil {
		t.Fatalf("Get(%s): want the object still present (finalizer held), got: %v", key, err)
	}
	if env.DeletionTimestamp.IsZero() {
		t.Fatalf("Get(%s): DeletionTimestamp is zero, want set", key)
	}
	return env
}

// TestReconcile_AddsFinalizerOnFirstNonDeletingReconcile proves
// ensureFinalizer's wiring into the normal (non-deleting) Reconcile path: a
// freshly created environment carries no finalizer until its first
// reconcile, and a second reconcile is a no-op (idempotent, no spurious
// Update -- AddFinalizer's own contract).
func TestReconcile_AddsFinalizerOnFirstNonDeletingReconcile(t *testing.T) {
	env := mustCreateEnv(t, "gains-finalizer")
	key := types.NamespacedName{Namespace: env.Namespace, Name: env.Name}

	if controllerutilContainsFinalizer(getEnv(t, key)) {
		t.Fatal("a freshly created environment must not already carry the archive finalizer")
	}

	r := newReconciler(t, newFakeClock(fixedStart), newFactsStore())
	reconcileOnce(t, r, key) // missing class -> hold(), but ensureFinalizer runs first regardless

	after := getEnv(t, key)
	if !controllerutilContainsFinalizer(after) {
		t.Fatal("expected the archive finalizer to be added on the first non-deleting reconcile")
	}
	rv := after.ResourceVersion

	reconcileOnce(t, r, key)
	again := getEnv(t, key)
	if again.ResourceVersion != rv {
		t.Errorf("ResourceVersion changed on a second reconcile (%s -> %s): ensureFinalizer should have been a no-op", rv, again.ResourceVersion)
	}
}

func controllerutilContainsFinalizer(env *sandboxv1alpha1.SandboxEnvironment) bool {
	for _, f := range env.Finalizers {
		if f == sandboxv1alpha1.FinalizerArchiveOnDelete {
			return true
		}
	}
	return false
}

func TestReconcileDelete_EscapeHatch_RemovesFinalizerWithoutArchiving(t *testing.T) {
	mustCreateClass(t)
	env := mustCreateEnv(t, "escape-hatch")
	key := types.NamespacedName{Namespace: env.Namespace, Name: env.Name}

	fresh := getEnv(t, key)
	if fresh.Annotations == nil {
		fresh.Annotations = map[string]string{}
	}
	fresh.Annotations[escapeHatchAnnotation] = "true"
	if err := k8s.Update(ctx, fresh); err != nil {
		t.Fatalf("setting escape-hatch annotation: %v", err)
	}
	mustAddFinalizerAndDelete(t, fresh)

	r := newReconciler(t, newFakeClock(fixedStart), newFactsStore())
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	assertEnvGone(t, key)

	names := render.ChildNames(env.Name)
	assertNoArchiveJob(t, env.Namespace, names.ArchiveJob)
}

func TestReconcileDelete_AlreadyArchived_RemovesFinalizerWithoutCreatingJob(t *testing.T) {
	mustCreateClass(t)
	env := mustCreateEnv(t, "already-archived")
	key := types.NamespacedName{Namespace: env.Namespace, Name: env.Name}

	fresh := getEnv(t, key)
	fresh.Status.Archive = &sandboxv1alpha1.ArchiveStatus{URI: "s3://sandbox-snapshots/already"}
	if err := k8s.Status().Update(ctx, fresh); err != nil {
		t.Fatalf("setting status.archive: %v", err)
	}
	mustAddFinalizerAndDelete(t, getEnv(t, key))

	r := newReconciler(t, newFakeClock(fixedStart), newFactsStore())
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	assertEnvGone(t, key)

	names := render.ChildNames(env.Name)
	assertNoArchiveJob(t, env.Namespace, names.ArchiveJob)
}

func TestReconcileDelete_PVCBackedClass_RemovesFinalizerWithoutArchiving(t *testing.T) {
	mustCreateClassWithTTL(t, "pvc-delete-class", sandboxv1alpha1.StorageBackendTypePVC, "")
	ns := testNamespace(t)
	env := mustCreateEnvInWithClass(t, ns, "pvc-delete-env", "pvc-delete-class", 0)
	key := types.NamespacedName{Namespace: ns, Name: env.Name}

	mustAddFinalizerAndDelete(t, getEnv(t, key))

	r := newReconciler(t, newFakeClock(fixedStart), newFactsStore())
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	assertEnvGone(t, key)
}

func TestReconcileDelete_MissingClass_RemovesFinalizerWithoutArchiving(t *testing.T) {
	env := mustCreateEnv(t, "missing-class-delete")
	key := types.NamespacedName{Namespace: env.Namespace, Name: env.Name}

	mustAddFinalizerAndDelete(t, getEnv(t, key))

	r := newReconciler(t, newFakeClock(fixedStart), newFactsStore())
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	assertEnvGone(t, key)
}

func TestReconcileDelete_S3NoSnapshotNoPod_CreatesArchiveJobAndHoldsFinalizer(t *testing.T) {
	mustCreateClass(t)
	env := mustCreateEnv(t, "s3-archive-job")
	key := types.NamespacedName{Namespace: env.Namespace, Name: env.Name}

	mustAddFinalizerAndDelete(t, getEnv(t, key))

	r := newReconciler(t, newFakeClock(fixedStart), newFactsStore())
	res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter != lifecycle.TerminalArchiveRequeue {
		t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, lifecycle.TerminalArchiveRequeue)
	}
	assertEnvStillPresent(t, key)

	names := render.ChildNames(env.Name)
	assertArchiveJobExists(t, env.Namespace, names.ArchiveJob)
}

func TestReconcileDelete_LivePodNoSnapshot_FreezesBeforeArchiving(t *testing.T) {
	mustCreateClass(t)
	env := mustCreateEnv(t, "freeze-before-archive")
	key := types.NamespacedName{Namespace: env.Namespace, Name: env.Name}

	mustCreateOwnedPod(t, env, corev1.PodRunning)
	t.Cleanup(func() {
		pod := &corev1.Pod{}
		if err := k8s.Get(ctx, types.NamespacedName{Namespace: env.Namespace, Name: render.ChildNames(env.Name).Pod}, pod); err == nil {
			_ = k8s.Delete(ctx, pod)
		}
	})

	mustAddFinalizerAndDelete(t, getEnv(t, key))

	r := newReconciler(t, newFakeClock(fixedStart), newFactsStore())
	res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter != lifecycle.TerminalArchiveRequeue {
		t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, lifecycle.TerminalArchiveRequeue)
	}

	after := assertEnvStillPresent(t, key)
	if after.Status.Phase != sandboxv1alpha1.PhaseFreezing {
		t.Errorf("Phase = %s, want Freezing (freeze-before-archive detour)", after.Status.Phase)
	}

	// No archive Job yet: the freeze must land first.
	names := render.ChildNames(env.Name)
	assertNoArchiveJob(t, env.Namespace, names.ArchiveJob)
}

// TestReconcileDelete_PendingPodNoSnapshot_ArchivesWithoutFreezing covers the
// pod phase the freeze detour must NOT take: a Pending pod runs no sidecar to
// perform the freeze and holds an agent home the agent never started writing
// to, so detouring would set phase=Freezing and then wait forever for a
// snapshot that can never arrive -- holding the finalizer and the slot for
// good. The archive must go ahead directly instead.
func TestReconcileDelete_PendingPodNoSnapshot_ArchivesWithoutFreezing(t *testing.T) {
	mustCreateClass(t)
	env := mustCreateEnv(t, "pending-pod-archive")
	key := types.NamespacedName{Namespace: env.Namespace, Name: env.Name}

	mustCreateOwnedPod(t, env, corev1.PodPending)
	t.Cleanup(func() {
		pod := &corev1.Pod{}
		if err := k8s.Get(ctx, types.NamespacedName{Namespace: env.Namespace, Name: render.ChildNames(env.Name).Pod}, pod); err == nil {
			_ = k8s.Delete(ctx, pod)
		}
	})

	mustAddFinalizerAndDelete(t, getEnv(t, key))

	r := newReconciler(t, newFakeClock(fixedStart), newFactsStore())
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	after := assertEnvStillPresent(t, key)
	if after.Status.Phase == sandboxv1alpha1.PhaseFreezing {
		t.Error("Phase = Freezing: a Pending pod must not trigger the freeze detour")
	}
	assertArchiveJobExists(t, env.Namespace, render.ChildNames(env.Name).ArchiveJob)
}

// classGetErrorClient fails every SandboxClass Get with a non-NotFound error,
// standing in for the transient faults a real cache/API server produces (a
// cancelled context during shutdown, an unstarted cache, a throttled
// API server). Every other read/write passes straight through.
type classGetErrorClient struct {
	client.Client
	err error
}

func (c classGetErrorClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*sandboxv1alpha1.SandboxClass); ok {
		return c.err
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

// TestReconcileDelete_TransientClassError_HoldsFinalizer proves a class Get
// that fails for any reason OTHER than NotFound never releases the finalizer:
// the error says nothing about whether archiving is possible, so treating it
// like a deleted class would silently discard the run's context.
func TestReconcileDelete_TransientClassError_HoldsFinalizer(t *testing.T) {
	mustCreateClass(t)
	env := mustCreateEnv(t, "transient-class-error")
	key := types.NamespacedName{Namespace: env.Namespace, Name: env.Name}

	mustAddFinalizerAndDelete(t, getEnv(t, key))

	r := newReconciler(t, newFakeClock(fixedStart), newFactsStore())
	r.Client = classGetErrorClient{Client: k8s, err: errors.New("etcdserver: request timed out")}

	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err == nil {
		t.Fatal("Reconcile: want the class Get error surfaced (so the reconcile retries), got nil")
	}
	assertEnvStillPresent(t, key)
	assertNoArchiveJob(t, env.Namespace, render.ChildNames(env.Name).ArchiveJob)
}

// TestArchive_RecordsArchivesTotal_Skipped drives archive() directly (the
// ActionArchive handler) for a nil class -- the "unresolvable class"
// no-op path -- and asserts archives_total{result=skipped} (#33).
func TestArchive_RecordsArchivesTotal_Skipped(t *testing.T) {
	env := mustCreateEnv(t, "archive-skipped")
	m := metrics.New()
	r := newReconciler(t, newFakeClock(fixedStart), newFactsStore())
	r.Metrics = m

	if err := r.archive(ctx, env, nil); err != nil {
		t.Fatalf("archive: %v", err)
	}

	reg := newTestRegistry(t, m)
	if n := mustMetricValue(t, reg, "sandbox_operator_archives_total", map[string]string{"result": metrics.ResultSkipped}); n != 1 {
		t.Errorf("archives_total{result=skipped} = %v, want 1", n)
	}
}

// TestArchive_RecordsArchivesTotal_Failed drives archive() directly for an
// S3-backed class whose credentials Secret does not exist, exercising
// ensureArchiveJob's "credentials not resolvable" soft-fail branch, and
// asserts archives_total{result=failed} (#33).
func TestArchive_RecordsArchivesTotal_Failed(t *testing.T) {
	class := &sandboxv1alpha1.SandboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: "archive-failed-class"},
		Spec: sandboxv1alpha1.SandboxClassSpec{
			Agent: sandboxv1alpha1.AgentSpec{Image: "ghcr.io/psenna/ai-sandbox-agent:v1"},
			Storage: sandboxv1alpha1.StorageSpec{
				Backend: sandboxv1alpha1.BackendSpec{
					Type: sandboxv1alpha1.StorageBackendTypeS3,
					S3: &sandboxv1alpha1.S3Backend{
						Endpoint: "https://s3.example.com",
						Bucket:   "sandbox-snapshots",
						CredentialsSecretRef: sandboxv1alpha1.SecretKeyRef{
							Name: "does-not-exist",
						},
					},
				},
			},
		},
	}
	if err := k8s.Create(ctx, class); err != nil {
		t.Fatalf("creating SandboxClass: %v", err)
	}
	t.Cleanup(func() { _ = k8s.Delete(ctx, class) })

	ns := testNamespace(t)
	env := mustCreateEnvInWithClass(t, ns, "archive-failed-env", class.Name, 0)

	m := metrics.New()
	r := newReconciler(t, newFakeClock(fixedStart), newFactsStore())
	r.Metrics = m

	if err := r.archive(ctx, env, class); err != nil {
		t.Fatalf("archive: %v", err)
	}

	reg := newTestRegistry(t, m)
	if n := mustMetricValue(t, reg, "sandbox_operator_archives_total", map[string]string{"result": metrics.ResultFailed}); n != 1 {
		t.Errorf("archives_total{result=failed} = %v, want 1", n)
	}
}
