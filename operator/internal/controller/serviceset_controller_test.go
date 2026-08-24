package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sandboxv1alpha1 "github.com/psenna/ai-sandbox/operator/api/v1alpha1"
	"github.com/psenna/ai-sandbox/operator/internal/render"
)

func newServiceSetReconciler(t *testing.T) *ServiceSetReconciler {
	t.Helper()
	return &ServiceSetReconciler{Client: k8s}
}

func mustCreateServiceSet(t *testing.T, ss *sandboxv1alpha1.ServiceSet) *sandboxv1alpha1.ServiceSet {
	t.Helper()
	if err := k8s.Create(ctx, ss); err != nil {
		t.Fatalf("create ServiceSet: %v", err)
	}
	t.Cleanup(func() { _ = k8s.Delete(ctx, ss) })
	return ss
}

// reconcileServiceSetOnce calls r.Reconcile for key, returning the error so
// tests can assert on it. Unlike reconcileOnce (which fatals on error), this
// returns the error -- the no-op fetch test needs to distinguish a fetch
// failure from a nil-return success.
func reconcileServiceSetOnce(t *testing.T, r *ServiceSetReconciler, key types.NamespacedName) (ctrl.Result, error) {
	t.Helper()
	return r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
}

func TestServiceSetReconciler_NoOpFetchesAndReturns(t *testing.T) {
	ss := &sandboxv1alpha1.ServiceSet{
		Spec: sandboxv1alpha1.ServiceSetSpec{EnvironmentName: "env-x"},
	}
	ss.Name, ss.Namespace = "set-x", "default"
	mustCreateServiceSet(t, ss)

	r := newServiceSetReconciler(t)
	if _, err := reconcileServiceSetOnce(t, r, types.NamespacedName{Name: ss.Name, Namespace: ss.Namespace}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

func TestServiceSetReconciler_CreatesServicePodAndPVC(t *testing.T) {
	ss := &sandboxv1alpha1.ServiceSet{Spec: sandboxv1alpha1.ServiceSetSpec{
		EnvironmentName: "env-svc",
		Services: []sandboxv1alpha1.ServiceSpec{{
			Name:        "postgres",
			Image:       "postgres:18-alpine",
			Ports:       []int32{5432},
			Env:         map[string]string{"POSTGRES_USER": "e2e", "POSTGRES_PASSWORD": "e2e"},
			Storage:     &sandboxv1alpha1.ServiceStorageSpec{Size: "1Gi", MountPath: "/var/lib/postgresql/data"},
			Healthcheck: sandboxv1alpha1.HealthcheckSpec{Exec: []string{"pg_isready", "-U", "e2e"}, Interval: "5s"},
		}},
	}}
	ss.Name, ss.Namespace = "set-svc", "default"
	mustCreateServiceSet(t, ss)

	r := newServiceSetReconciler(t)
	key := types.NamespacedName{Name: ss.Name, Namespace: ss.Namespace}
	if _, err := reconcileServiceSetOnce(t, r, key); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// Pod created with image, env, readinessProbe, data-PVC mount, spec-hash label.
	var pod corev1.Pod
	if err := k8s.Get(ctx, types.NamespacedName{Name: "postgres", Namespace: "default"}, &pod); err != nil {
		t.Fatalf("get pod: %v", err)
	}
	if pod.Spec.Containers[0].Image != "postgres:18-alpine" {
		t.Fatalf("pod image = %q", pod.Spec.Containers[0].Image)
	}
	if pod.Spec.Containers[0].ReadinessProbe == nil || pod.Spec.Containers[0].ReadinessProbe.Exec == nil {
		t.Fatal("pod missing exec readinessProbe from healthcheck")
	}
	if got := envValue(pod, "POSTGRES_USER"); got != "e2e" {
		t.Fatalf("POSTGRES_USER env = %q", got)
	}
	if !hasVolumeMount(pod, "postgres-data", "/var/lib/postgresql/data") {
		t.Fatal("pod missing data PVC mount postgres-data at /var/lib/postgresql/data")
	}
	if pod.Annotations["ai-sandbox.io/spec-hash"] == "" {
		t.Fatal("pod missing ai-sandbox.io/spec-hash annotation")
	}
	if !ownedBy(&pod, ss) {
		t.Fatal("pod not owned by ServiceSet")
	}

	// Service created with port 5432 and selector matching the pod labels.
	var svc corev1.Service
	if err := k8s.Get(ctx, types.NamespacedName{Name: "postgres", Namespace: "default"}, &svc); err != nil {
		t.Fatalf("get svc: %v", err)
	}
	if len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Port != 5432 {
		t.Fatalf("svc ports = %+v", svc.Spec.Ports)
	}
	if !ownedBy(&svc, ss) {
		t.Fatal("svc not owned by ServiceSet")
	}

	// Data PVC created, RWO, size 1Gi, owned by ServiceSet.
	var pvc corev1.PersistentVolumeClaim
	if err := k8s.Get(ctx, types.NamespacedName{Name: "postgres-data", Namespace: "default"}, &pvc); err != nil {
		t.Fatalf("get pvc: %v", err)
	}
	if len(pvc.Spec.AccessModes) != 1 || pvc.Spec.AccessModes[0] != corev1.ReadWriteOnce {
		t.Fatalf("pvc accessmodes = %+v", pvc.Spec.AccessModes)
	}
	if pvc.Spec.Resources.Requests.Storage().String() != "1Gi" {
		t.Fatalf("pvc size = %q", pvc.Spec.Resources.Requests.Storage())
	}
	if !ownedBy(&pvc, ss) {
		t.Fatal("pvc not owned by ServiceSet")
	}
}

func envValue(pod corev1.Pod, name string) string {
	for _, e := range pod.Spec.Containers[0].Env {
		if e.Name == name {
			return e.Value
		}
	}
	return ""
}
func hasVolumeMount(pod corev1.Pod, name, path string) bool {
	for _, vm := range pod.Spec.Containers[0].VolumeMounts {
		if vm.Name == name && vm.MountPath == path {
			return true
		}
	}
	return false
}
func ownedBy(obj client.Object, owner *sandboxv1alpha1.ServiceSet) bool {
	for _, ref := range obj.GetOwnerReferences() {
		if ref.Kind == "ServiceSet" && ref.Name == owner.Name && ref.UID == owner.UID {
			return true
		}
	}
	return false
}

func TestServiceSetReconciler_CreatesRuntimePodWithWorkspace(t *testing.T) {
	// The workspace PVC is created by the environment controller in production;
	// in envtest pre-create it as a fixture so the runtime pod's volume resolves.
	ws := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "env-rt-workspace", Namespace: "default"},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}},
		},
	}
	if err := k8s.Create(ctx, ws); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create workspace pvc: %v", err)
	}
	t.Cleanup(func() { _ = k8s.Delete(ctx, ws) })

	ss := &sandboxv1alpha1.ServiceSet{Spec: sandboxv1alpha1.ServiceSetSpec{
		EnvironmentName: "env-rt",
		Runtimes: []sandboxv1alpha1.RuntimeSpec{{
			Name:    "python",
			Image:   "python:3.13-slim",
			Command: []string{"sleep", "infinity"},
		}},
	}}
	ss.Name, ss.Namespace = "set-rt", "default"
	mustCreateServiceSet(t, ss)

	r := newServiceSetReconciler(t)
	if _, err := reconcileServiceSetOnce(t, r, types.NamespacedName{Name: ss.Name, Namespace: ss.Namespace}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var pod corev1.Pod
	if err := k8s.Get(ctx, types.NamespacedName{Name: "python", Namespace: "default"}, &pod); err != nil {
		t.Fatalf("get pod: %v", err)
	}
	if !hasVolumeMount(pod, "env-rt-workspace", "/workspace") {
		t.Fatal("runtime pod missing workspace PVC mount env-rt-workspace at /workspace")
	}
	if pod.Spec.Containers[0].Image != "python:3.13-slim" {
		t.Fatalf("runtime image = %q", pod.Spec.Containers[0].Image)
	}
	if len(pod.Spec.Containers[0].Command) == 0 || pod.Spec.Containers[0].Command[0] != "sleep" {
		t.Fatalf("runtime command = %+v", pod.Spec.Containers[0].Command)
	}
	if !ownedBy(&pod, ss) {
		t.Fatal("runtime pod not owned by ServiceSet")
	}
}

func TestServiceSetReconciler_ReadyGatedByDependsOn(t *testing.T) {
	// A depends on B; B depends on nothing. Mark only B's pod Ready first.
	ss := &sandboxv1alpha1.ServiceSet{Spec: sandboxv1alpha1.ServiceSetSpec{
		EnvironmentName: "env-deps",
		Services: []sandboxv1alpha1.ServiceSpec{
			{Name: "b", Image: "alpine:3.21", Command: []string{"sleep", "infinity"}},
			{Name: "a", Image: "alpine:3.21", Command: []string{"sleep", "infinity"}, DependsOn: []string{"b"}},
		},
	}}
	ss.Name, ss.Namespace = "set-deps", "default"
	mustCreateServiceSet(t, ss)
	r := newServiceSetReconciler(t)
	key := types.NamespacedName{Name: ss.Name, Namespace: ss.Namespace}
	reconcileServiceSetOnce(t, r, key)

	// Nothing ready yet: A not ready (pod not ready AND dep b not ready).
	if _, err := reconcileServiceSetOnce(t, r, key); err != nil {
		t.Fatal(err)
	}
	assertEntryReady(t, key, "a", false)
	assertEntryReady(t, key, "b", false)
	assertReadyCondition(t, key, metav1.ConditionFalse)

	// Mark B's pod Ready: B becomes ready, A still not ready (its own pod not ready).
	markPodReady(t, "b", "default")
	if _, err := reconcileServiceSetOnce(t, r, key); err != nil {
		t.Fatal(err)
	}
	assertEntryReady(t, key, "b", true)
	assertEntryReady(t, key, "a", false)

	// Mark A's pod Ready too: now A ready (dep b ready AND own pod ready), aggregate Ready=True.
	markPodReady(t, "a", "default")
	if _, err := reconcileServiceSetOnce(t, r, key); err != nil {
		t.Fatal(err)
	}
	assertEntryReady(t, key, "a", true)
	assertReadyCondition(t, key, metav1.ConditionTrue)
}

func markPodReady(t *testing.T, name, ns string) {
	t.Helper()
	var pod corev1.Pod
	if err := k8s.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &pod); err != nil {
		t.Fatalf("get pod %s: %v", name, err)
	}
	pod.Status.Conditions = append(pod.Status.Conditions, corev1.PodCondition{
		Type: corev1.PodReady, Status: corev1.ConditionTrue, LastTransitionTime: metav1.Now(),
	})
	if err := k8s.Status().Update(ctx, &pod); err != nil {
		t.Fatalf("status update pod %s: %v", name, err)
	}
}
func assertEntryReady(t *testing.T, key types.NamespacedName, name string, want bool) {
	t.Helper()
	var ss sandboxv1alpha1.ServiceSet
	if err := k8s.Get(ctx, key, &ss); err != nil {
		t.Fatal(err)
	}
	for _, e := range ss.Status.Entries {
		if e.Name == name {
			if e.Ready != want {
				t.Fatalf("entry %s ready = %v, want %v", name, e.Ready, want)
			}
			return
		}
	}
	t.Fatalf("entry %s not found in status", name)
}
func assertReadyCondition(t *testing.T, key types.NamespacedName, want metav1.ConditionStatus) {
	t.Helper()
	var ss sandboxv1alpha1.ServiceSet
	if err := k8s.Get(ctx, key, &ss); err != nil {
		t.Fatal(err)
	}
	c := apimeta.FindStatusCondition(ss.Status.Conditions, "Ready")
	if c == nil {
		t.Fatal("Ready condition missing")
	}
	if c.Status != want {
		t.Fatalf("Ready condition = %s, want %s (reason=%s)", c.Status, want, c.Reason)
	}
}

func TestServiceSetReconciler_ImageChangeRecreatesPodRetainsPVC(t *testing.T) {
	ss := &sandboxv1alpha1.ServiceSet{Spec: sandboxv1alpha1.ServiceSetSpec{
		EnvironmentName: "env-recreate",
		Services: []sandboxv1alpha1.ServiceSpec{{
			Name: "python", Image: "python:3.11-slim",
			Storage: &sandboxv1alpha1.ServiceStorageSpec{Size: "1Gi", MountPath: "/data"},
		}},
	}}
	ss.Name, ss.Namespace = "set-rec", "default"
	mustCreateServiceSet(t, ss)
	r := newServiceSetReconciler(t)
	key := types.NamespacedName{Name: ss.Name, Namespace: ss.Namespace}
	reconcileServiceSetOnce(t, r, key)

	var podBefore corev1.Pod
	if err := k8s.Get(ctx, types.NamespacedName{Name: "python", Namespace: "default"}, &podBefore); err != nil {
		t.Fatal(err)
	}
	var pvcBefore corev1.PersistentVolumeClaim
	if err := k8s.Get(ctx, types.NamespacedName{Name: "python-data", Namespace: "default"}, &pvcBefore); err != nil {
		t.Fatal(err)
	}

	// Change the image and re-reconcile. Re-fetch first: the reconcile above
	// wrote .status (Task 5 writeStatus), bumping ss.resourceVersion, so an
	// Update with the stale in-memory object would 409-conflict
	// ("the object has been modified"). Get picks up the current RV.
	if err := k8s.Get(ctx, key, ss); err != nil {
		t.Fatal(err)
	}
	ss.Spec.Services[0].Image = "python:3.13-slim"
	if err := k8s.Update(ctx, ss); err != nil {
		t.Fatal(err)
	}
	reconcileServiceSetOnce(t, r, key)

	var podAfter corev1.Pod
	if err := k8s.Get(ctx, types.NamespacedName{Name: "python", Namespace: "default"}, &podAfter); err != nil {
		t.Fatal(err)
	}
	if podAfter.UID == podBefore.UID {
		t.Fatal("pod UID unchanged after image change; expected recreate")
	}
	if podAfter.Spec.Containers[0].Image != "python:3.13-slim" {
		t.Fatalf("pod image = %q after recreate", podAfter.Spec.Containers[0].Image)
	}
	if len(podAfter.Annotations[specHashAnnotation]) == 0 {
		t.Fatal("recreated pod missing spec-hash annotation")
	}

	// PVC retained: same UID as before.
	var pvcAfter corev1.PersistentVolumeClaim
	if err := k8s.Get(ctx, types.NamespacedName{Name: "python-data", Namespace: "default"}, &pvcAfter); err != nil {
		t.Fatal(err)
	}
	if pvcAfter.UID != pvcBefore.UID {
		t.Fatal("data PVC UID changed across pod recreate; PVC must be retained")
	}
}

func TestServiceSetReconciler_PrunesRemovedEntries(t *testing.T) {
	ss := &sandboxv1alpha1.ServiceSet{Spec: sandboxv1alpha1.ServiceSetSpec{
		EnvironmentName: "env-prune",
		Services: []sandboxv1alpha1.ServiceSpec{
			{Name: "keep", Image: "alpine:3.21", Command: []string{"sleep", "infinity"}},
			{Name: "drop", Image: "alpine:3.21", Command: []string{"sleep", "infinity"}},
		},
	}}
	ss.Name, ss.Namespace = "set-prune", "default"
	mustCreateServiceSet(t, ss)
	r := newServiceSetReconciler(t)
	key := types.NamespacedName{Name: ss.Name, Namespace: ss.Namespace}
	reconcileServiceSetOnce(t, r, key)

	// Both pods exist now.
	if err := k8s.Get(ctx, types.NamespacedName{Name: "drop", Namespace: "default"}, &corev1.Pod{}); err != nil {
		t.Fatalf("drop pod should exist: %v", err)
	}

	// Remove "drop" and re-reconcile. Re-fetch first: the reconcile above wrote
	// .status (writeStatus) and ran pruneChildren, bumping ss.resourceVersion, so
	// an Update with the stale in-memory object would 409-conflict.
	if err := k8s.Get(ctx, key, ss); err != nil {
		t.Fatal(err)
	}
	ss.Spec.Services = ss.Spec.Services[:1] // keep only "keep"
	if err := k8s.Update(ctx, ss); err != nil {
		t.Fatal(err)
	}
	reconcileServiceSetOnce(t, r, key)

	if err := k8s.Get(ctx, types.NamespacedName{Name: "drop", Namespace: "default"}, &corev1.Pod{}); !apierrors.IsNotFound(err) {
		t.Fatalf("drop pod should be pruned, got err=%v", err)
	}
	if err := k8s.Get(ctx, types.NamespacedName{Name: "keep", Namespace: "default"}, &corev1.Pod{}); err != nil {
		t.Fatalf("keep pod should remain: %v", err)
	}
}

func TestServiceSetReconciler_CyclicDependsOnTerminatesNotReady(t *testing.T) {
	ss := &sandboxv1alpha1.ServiceSet{Spec: sandboxv1alpha1.ServiceSetSpec{
		EnvironmentName: "env-cycle",
		Services: []sandboxv1alpha1.ServiceSpec{
			{Name: "a", Image: "alpine:3.21", Command: []string{"sleep", "infinity"}, DependsOn: []string{"b"}},
			{Name: "b", Image: "alpine:3.21", Command: []string{"sleep", "infinity"}, DependsOn: []string{"a"}},
		},
	}}
	ss.Name, ss.Namespace = "set-cycle", "default"
	mustCreateServiceSet(t, ss)
	r := newServiceSetReconciler(t)
	key := types.NamespacedName{Name: ss.Name, Namespace: ss.Namespace}
	reconcileServiceSetOnce(t, r, key) // must not hang or crash

	markPodReady(t, "a", "default")
	markPodReady(t, "b", "default")
	reconcileServiceSetOnce(t, r, key) // cycle now reachable: must still terminate

	assertReadyCondition(t, key, metav1.ConditionFalse)
	assertEntryReady(t, key, "a", false)
	assertEntryReady(t, key, "b", false)
}

func TestServiceSetReconciler_DiamondDependsOnNotACycle(t *testing.T) {
	ss := &sandboxv1alpha1.ServiceSet{Spec: sandboxv1alpha1.ServiceSetSpec{
		EnvironmentName: "env-diamond",
		Services: []sandboxv1alpha1.ServiceSpec{
			{Name: "a", Image: "alpine:3.21", Command: []string{"sleep", "infinity"}, DependsOn: []string{"b", "c"}},
			{Name: "b", Image: "alpine:3.21", Command: []string{"sleep", "infinity"}, DependsOn: []string{"d"}},
			{Name: "c", Image: "alpine:3.21", Command: []string{"sleep", "infinity"}, DependsOn: []string{"d"}},
			{Name: "d", Image: "alpine:3.21", Command: []string{"sleep", "infinity"}},
		},
	}}
	ss.Name, ss.Namespace = "set-diamond", "default"
	mustCreateServiceSet(t, ss)
	r := newServiceSetReconciler(t)
	key := types.NamespacedName{Name: ss.Name, Namespace: ss.Namespace}
	reconcileServiceSetOnce(t, r, key)
	for _, n := range []string{"a", "b", "c", "d"} {
		markPodReady(t, n, "default")
	}
	reconcileServiceSetOnce(t, r, key)
	assertReadyCondition(t, key, metav1.ConditionTrue)
	for _, n := range []string{"a", "b", "c", "d"} {
		assertEntryReady(t, key, n, true)
	}
}

func TestServiceSetReconcileDuplicateEntryName(t *testing.T) {
	// Defense-in-depth guard for the #2 defect: a service and runtime sharing
	// a name both target Pod/<name>, which would storm the reconciler (two
	// ensurePod calls delete+recreate each other's pod). The guard detects the
	// cross-list collision BEFORE reconciling children, writes Ready=False
	// reason DuplicateEntryName, and returns nil -- no children, no storm.
	ss := &sandboxv1alpha1.ServiceSet{Spec: sandboxv1alpha1.ServiceSetSpec{
		EnvironmentName: "env-1",
		Services:        []sandboxv1alpha1.ServiceSpec{{Name: "shared", Image: "a"}},
		Runtimes:        []sandboxv1alpha1.RuntimeSpec{{Name: "shared", Image: "b"}},
	}}
	ss.Name, ss.Namespace = "env-1", "default"
	mustCreateServiceSet(t, ss)

	r := newServiceSetReconciler(t)
	key := types.NamespacedName{Name: ss.Name, Namespace: ss.Namespace}

	// First reconcile: guard refuses to create children, returns nil, no requeue.
	res, err := reconcileServiceSetOnce(t, r, key)
	if err != nil {
		t.Fatalf("reconcile returned err=%v, want nil (collision is a bad spec, not a transient error)", err)
	}
	if res.Requeue {
		t.Fatal("reconcile requeued; a colliding name must not requeue")
	}

	// No Pod/shared exists: the guard refused to create any child.
	var pod corev1.Pod
	if err := k8s.Get(ctx, types.NamespacedName{Name: "shared", Namespace: "default"}, &pod); !apierrors.IsNotFound(err) {
		t.Fatalf("Pod/shared should not exist (guard refused), got err=%v", err)
	}

	// Status: Ready=False, reason DuplicateEntryName.
	var got sandboxv1alpha1.ServiceSet
	if err := k8s.Get(ctx, key, &got); err != nil {
		t.Fatalf("re-fetch ServiceSet: %v", err)
	}
	cond := apimeta.FindStatusCondition(got.Status.Conditions, "Ready")
	if cond == nil {
		t.Fatal("Ready condition missing")
	}
	if cond.Status != metav1.ConditionFalse {
		t.Fatalf("Ready status = %s, want False", cond.Status)
	}
	if cond.Reason != "DuplicateEntryName" {
		t.Fatalf("Ready reason = %q, want DuplicateEntryName", cond.Reason)
	}

	// Entries: both the service and the runtime are marked Ready=false,
	// Reason=DuplicateEntryName (writeDuplicateStatus populated them).
	if len(got.Status.Entries) != 2 {
		t.Fatalf("entries len = %d, want 2 (service+runtime)", len(got.Status.Entries))
	}
	for _, e := range got.Status.Entries {
		if e.Ready {
			t.Fatalf("entry %q Ready=true, want false", e.Name)
		}
		if e.Reason != "DuplicateEntryName" {
			t.Fatalf("entry %q reason = %q, want DuplicateEntryName", e.Name, e.Reason)
		}
	}

	// Second reconcile: idempotent -- still nil, still no Pod/shared (no storm).
	res2, err2 := reconcileServiceSetOnce(t, r, key)
	if err2 != nil {
		t.Fatalf("second reconcile returned err=%v, want nil (idempotent)", err2)
	}
	if res2.Requeue {
		t.Fatal("second reconcile requeued; must not requeue while collision persists")
	}
	var pod2 corev1.Pod
	if err := k8s.Get(ctx, types.NamespacedName{Name: "shared", Namespace: "default"}, &pod2); !apierrors.IsNotFound(err) {
		t.Fatalf("Pod/shared should still not exist after second reconcile, got err=%v", err)
	}
}

func TestServiceSetReconciler_PortlessAfterPortsPrunesService(t *testing.T) {
	ss := &sandboxv1alpha1.ServiceSet{Spec: sandboxv1alpha1.ServiceSetSpec{
		EnvironmentName: "env-stalesvc",
		Services: []sandboxv1alpha1.ServiceSpec{
			{Name: "web", Image: "alpine:3.21", Command: []string{"sleep", "infinity"}, Ports: []int32{8080}},
		},
	}}
	ss.Name, ss.Namespace = "set-stalesvc", "default"
	mustCreateServiceSet(t, ss)
	r := newServiceSetReconciler(t)
	key := types.NamespacedName{Name: ss.Name, Namespace: ss.Namespace}
	reconcileServiceSetOnce(t, r, key)

	// Service exists (had ports).
	if err := k8s.Get(ctx, types.NamespacedName{Name: "web", Namespace: "default"}, &corev1.Service{}); err != nil {
		t.Fatalf("web Service should exist: %v", err)
	}

	// Remove all ports and re-reconcile. Re-fetch first: the reconcile above
	// wrote .status + ran pruneChildren, bumping ss.resourceVersion, so a
	// stale-RV Update would 409-conflict.
	if err := k8s.Get(ctx, key, ss); err != nil {
		t.Fatal(err)
	}
	ss.Spec.Services[0].Ports = nil
	if err := k8s.Update(ctx, ss); err != nil {
		t.Fatal(err)
	}
	reconcileServiceSetOnce(t, r, key)

	// Stale Service pruned; Pod still present (portless service still gets a Pod).
	if err := k8s.Get(ctx, types.NamespacedName{Name: "web", Namespace: "default"}, &corev1.Service{}); !apierrors.IsNotFound(err) {
		t.Fatalf("web Service should be pruned after ports removed, got err=%v", err)
	}
	if err := k8s.Get(ctx, types.NamespacedName{Name: "web", Namespace: "default"}, &corev1.Pod{}); err != nil {
		t.Fatalf("web Pod should remain: %v", err)
	}
}

// TestServiceSetPodEnvLabelAndNoToken asserts every ServiceSet child pod
// carries the env label (so the namespace's Restricted NetworkPolicy selects
// it -- dep/runtime pods inherit env isolation, Task 8) and that
// AutomountServiceAccountToken is explicitly false (the pods hold no
// credential, same invariant as the agent pod).
func TestServiceSetPodEnvLabelAndNoToken(t *testing.T) {
	// The workspace PVC is created by the environment controller in production;
	// in envtest pre-create it as a fixture so the runtime pod's volume
	// resolves (mirrors TestServiceSetReconciler_CreatesRuntimePodWithWorkspace).
	ws := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "env-1-workspace", Namespace: "default"},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}},
		},
	}
	if err := k8s.Create(ctx, ws); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create workspace pvc: %v", err)
	}
	t.Cleanup(func() { _ = k8s.Delete(ctx, ws) })

	ss := &sandboxv1alpha1.ServiceSet{Spec: sandboxv1alpha1.ServiceSetSpec{
		EnvironmentName: "env-1",
		Services: []sandboxv1alpha1.ServiceSpec{{
			Name:    "envlbl-svc",
			Image:   "alpine:3.21",
			Command: []string{"sleep", "infinity"},
		}},
		Runtimes: []sandboxv1alpha1.RuntimeSpec{{
			Name:  "envlbl-rt",
			Image: "python:3.13-slim",
		}},
	}}
	ss.Name, ss.Namespace = "set-envlabel", "default"
	mustCreateServiceSet(t, ss)

	r := newServiceSetReconciler(t)
	if _, err := reconcileServiceSetOnce(t, r, types.NamespacedName{Name: ss.Name, Namespace: ss.Namespace}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	wantEnvLabel := render.EnvironmentLabelValue("env-1")
	for _, entry := range []string{"envlbl-svc", "envlbl-rt"} {
		var pod corev1.Pod
		if err := k8s.Get(ctx, types.NamespacedName{Name: entry, Namespace: "default"}, &pod); err != nil {
			t.Fatalf("get pod %s: %v", entry, err)
		}
		if got := pod.Labels["sandbox.psenna.dev/environment"]; got != wantEnvLabel {
			t.Errorf("pod %s env label = %q, want %q", entry, got, wantEnvLabel)
		}
		if pod.Spec.AutomountServiceAccountToken == nil {
			t.Errorf("pod %s AutomountServiceAccountToken is nil, want ptr to false", entry)
		} else if *pod.Spec.AutomountServiceAccountToken {
			t.Errorf("pod %s AutomountServiceAccountToken = true, want false", entry)
		}
	}
}
