package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sandboxv1alpha1 "github.com/psenna/ai-sandbox/operator/api/v1alpha1"
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
