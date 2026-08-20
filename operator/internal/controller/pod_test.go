package controller

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	sandboxv1alpha1 "github.com/psenna/ai-sandbox/operator/api/v1alpha1"
	"github.com/psenna/ai-sandbox/operator/internal/lifecycle"
	"github.com/psenna/ai-sandbox/operator/internal/render"
)

// settleResources reconciles env once through the real resource reconciler
// so the ServiceAccount (and every other child object) the pod references
// actually exists before ensurePod is called directly -- envtest's
// ServiceAccount admission plugin can reject a pod naming a nonexistent
// ServiceAccount.
func settleResources(t *testing.T, r *Reconciler, key types.NamespacedName) {
	t.Helper()
	reconcileOnce(t, r, key)
}

func TestEnsurePod_CreatesPod(t *testing.T) {
	class := mustCreateClassWithEngine(t, sandboxv1alpha1.EngineTypeNone)
	env := mustCreateEnv(t, "ensure-pod-creates")
	key := types.NamespacedName{Namespace: env.Namespace, Name: env.Name}

	r := newResourceReconciler(t, newFakeClock(fixedStart))
	settleResources(t, r, key)

	fresh := getEnv(t, key)
	if err := r.ensurePod(ctx, fresh, class); err != nil {
		t.Fatalf("ensurePod: %v", err)
	}

	names := render.ChildNames(env.Name)
	pod := getPod(t, types.NamespacedName{Namespace: env.Namespace, Name: names.Pod})

	assertPodLabelsAndOwner(t, pod, fresh)
	assertPodSpecBasics(t, pod, names)

	agent := findAgentContainerStatus(t, pod)
	assertAgentContainer(t, agent, names)
}

// assertPodLabelsAndOwner checks pod's labels match render.Labels(env) and
// that its sole owner reference is a blocking controller ref to env.
func assertPodLabelsAndOwner(t *testing.T, pod *corev1.Pod, env *sandboxv1alpha1.SandboxEnvironment) {
	t.Helper()
	labels := render.Labels(env)
	for k, v := range labels {
		if pod.Labels[k] != v {
			t.Errorf("pod label %s = %q, want %q", k, pod.Labels[k], v)
		}
	}

	if len(pod.OwnerReferences) != 1 {
		t.Fatalf("pod has %d owner references, want 1", len(pod.OwnerReferences))
	}
	or := pod.OwnerReferences[0]
	if or.Controller == nil || !*or.Controller {
		t.Error("owner reference Controller is not true")
	}
	if or.BlockOwnerDeletion == nil || !*or.BlockOwnerDeletion {
		t.Error("owner reference BlockOwnerDeletion is not true")
	}
	if or.UID != env.UID {
		t.Errorf("owner reference UID = %s, want %s", or.UID, env.UID)
	}
}

// assertPodSpecBasics checks the pod-level (not container-level) spec
// fields RenderPod sets.
func assertPodSpecBasics(t *testing.T, pod *corev1.Pod, names render.Names) {
	t.Helper()
	if pod.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("RestartPolicy = %s, want Never", pod.Spec.RestartPolicy)
	}
	if pod.Spec.ServiceAccountName != names.ServiceAccount {
		t.Errorf("ServiceAccountName = %q, want %q", pod.Spec.ServiceAccountName, names.ServiceAccount)
	}
	if pod.Spec.TerminationGracePeriodSeconds == nil || *pod.Spec.TerminationGracePeriodSeconds != render.DefaultTerminationGracePeriodSeconds {
		t.Errorf("TerminationGracePeriodSeconds = %v, want %d", pod.Spec.TerminationGracePeriodSeconds, render.DefaultTerminationGracePeriodSeconds)
	}
	if pod.Spec.SecurityContext == nil || pod.Spec.SecurityContext.RunAsNonRoot == nil || !*pod.Spec.SecurityContext.RunAsNonRoot {
		t.Error("pod SecurityContext.RunAsNonRoot is not true")
	}
}

// findAgentContainerStatus returns a pointer to the agent container within
// pod.Spec.Containers, failing the test if it is not present.
func findAgentContainerStatus(t *testing.T, pod *corev1.Pod) *corev1.Container {
	t.Helper()
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == render.AgentContainerName {
			return &pod.Spec.Containers[i]
		}
	}
	t.Fatal("agent container not found")
	return nil
}

// assertAgentContainer checks the agent container's securityContext,
// envFrom, and volume mounts.
func assertAgentContainer(t *testing.T, agent *corev1.Container, names render.Names) {
	t.Helper()
	if agent.SecurityContext == nil || agent.SecurityContext.RunAsNonRoot == nil || !*agent.SecurityContext.RunAsNonRoot {
		t.Error("agent container SecurityContext.RunAsNonRoot is not true")
	}

	foundSecretEnvFrom := false
	for _, ef := range agent.EnvFrom {
		if ef.SecretRef != nil && ef.SecretRef.Name == names.Secret {
			foundSecretEnvFrom = true
		}
	}
	if !foundSecretEnvFrom {
		t.Error("agent container envFrom does not reference the rendered Secret")
	}

	wantMounts := map[string]bool{"workspace": false, "agent-home": false, "config": false}
	for _, m := range agent.VolumeMounts {
		if _, ok := wantMounts[m.Name]; ok {
			wantMounts[m.Name] = true
		}
	}
	for name, found := range wantMounts {
		if !found {
			t.Errorf("agent container missing volume mount %q", name)
		}
	}
}

func TestEnsurePod_IsIdempotent(t *testing.T) {
	class := mustCreateClassWithEngine(t, sandboxv1alpha1.EngineTypeNone)
	env := mustCreateEnv(t, "ensure-pod-idempotent")
	key := types.NamespacedName{Namespace: env.Namespace, Name: env.Name}

	r := newResourceReconciler(t, newFakeClock(fixedStart))
	settleResources(t, r, key)

	fresh := getEnv(t, key)
	if err := r.ensurePod(ctx, fresh, class); err != nil {
		t.Fatalf("ensurePod (1st): %v", err)
	}
	names := render.ChildNames(env.Name)
	podKey := types.NamespacedName{Namespace: env.Namespace, Name: names.Pod}
	rv1 := getPod(t, podKey).ResourceVersion

	if err := r.ensurePod(ctx, fresh, class); err != nil {
		t.Fatalf("ensurePod (2nd): %v", err)
	}
	rv2 := getPod(t, podKey).ResourceVersion

	if rv1 != rv2 {
		t.Errorf("resourceVersion changed across a no-op re-apply: %s -> %s (server-defaulted field drift)", rv1, rv2)
	}
}

func TestEnsurePod_OwnershipGuard(t *testing.T) {
	class := mustCreateClassWithEngine(t, sandboxv1alpha1.EngineTypeNone)
	env := mustCreateEnv(t, "ensure-pod-ownership-guard")
	key := types.NamespacedName{Namespace: env.Namespace, Name: env.Name}

	r := newResourceReconciler(t, newFakeClock(fixedStart))
	settleResources(t, r, key)

	names := render.ChildNames(env.Name)
	foreign := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: names.Pod, Namespace: env.Namespace},
		Spec: corev1.PodSpec{
			Containers:    []corev1.Container{{Name: "foreign", Image: "busybox"}},
			RestartPolicy: corev1.RestartPolicyNever,
		},
	}
	if err := k8s.Create(ctx, foreign); err != nil {
		t.Fatalf("creating foreign pod: %v", err)
	}
	t.Cleanup(func() { _ = k8s.Delete(ctx, foreign) })

	fresh := getEnv(t, key)
	if err := r.ensurePod(ctx, fresh, class); err != nil {
		t.Fatalf("ensurePod: %v", err)
	}

	after := getPod(t, types.NamespacedName{Namespace: env.Namespace, Name: names.Pod})
	if after.ResourceVersion != foreign.ResourceVersion {
		t.Errorf("foreign pod was modified by ensurePod: resourceVersion %s -> %s", foreign.ResourceVersion, after.ResourceVersion)
	}
	if len(after.Spec.Containers) != 1 || after.Spec.Containers[0].Name != "foreign" {
		t.Errorf("foreign pod spec was overwritten: %+v", after.Spec.Containers)
	}
}

func TestEnsurePod_UnimplementedEngineCreatesNoPod(t *testing.T) {
	// mustCreateClass leaves spec.engine.type unset, so the CRD default
	// (rootless-podman) applies -- the deliberately unimplemented engine.
	class := mustCreateClass(t)
	env := mustCreateEnv(t, "ensure-pod-unimplemented")
	key := types.NamespacedName{Namespace: env.Namespace, Name: env.Name}

	r := newResourceReconciler(t, newFakeClock(fixedStart))
	settleResources(t, r, key)

	fresh := getEnv(t, key)
	if err := r.ensurePod(ctx, fresh, class); err != nil {
		t.Fatalf("ensurePod: %v, want nil (permanent misconfiguration is logged, not returned)", err)
	}

	names := render.ChildNames(env.Name)
	var pod corev1.Pod
	err := k8s.Get(ctx, types.NamespacedName{Namespace: env.Namespace, Name: names.Pod}, &pod)
	if !apierrors.IsNotFound(err) {
		t.Errorf("Get pod: err = %v, want NotFound (no pod should be created)", err)
	}

	// Drive it through a real reconcile too, and check the condition.
	reconcileOnce(t, r, key)
	final := getEnv(t, key)
	c := findCondition(final, ConditionEngineSecurity)
	if c == nil {
		t.Fatal("EngineSecurityRelaxed condition missing")
	}
	if c.Status != metav1.ConditionUnknown || c.Reason != ReasonEngineUnavailable {
		t.Errorf("EngineSecurityRelaxed = %s/%s, want Unknown/%s", c.Status, c.Reason, ReasonEngineUnavailable)
	}
}

func TestDeletePod_NoOpWhenAbsent(t *testing.T) {
	class := mustCreateClassWithEngine(t, sandboxv1alpha1.EngineTypeNone)
	env := mustCreateEnv(t, "delete-pod-absent")
	key := types.NamespacedName{Namespace: env.Namespace, Name: env.Name}
	r := newResourceReconciler(t, newFakeClock(fixedStart))
	settleResources(t, r, key)

	fresh := getEnv(t, key)
	if err := r.deletePod(ctx, fresh, class); err != nil {
		t.Fatalf("deletePod on an absent pod: %v, want nil", err)
	}
}

func TestDeletePod_NoOpWhenForeignOwned(t *testing.T) {
	class := mustCreateClassWithEngine(t, sandboxv1alpha1.EngineTypeNone)
	env := mustCreateEnv(t, "delete-pod-foreign")
	key := types.NamespacedName{Namespace: env.Namespace, Name: env.Name}
	r := newResourceReconciler(t, newFakeClock(fixedStart))
	settleResources(t, r, key)

	names := render.ChildNames(env.Name)
	foreign := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: names.Pod, Namespace: env.Namespace},
		Spec: corev1.PodSpec{
			Containers:    []corev1.Container{{Name: "foreign", Image: "busybox"}},
			RestartPolicy: corev1.RestartPolicyNever,
		},
	}
	if err := k8s.Create(ctx, foreign); err != nil {
		t.Fatalf("creating foreign pod: %v", err)
	}
	t.Cleanup(func() { _ = k8s.Delete(ctx, foreign) })

	fresh := getEnv(t, key)
	if err := r.deletePod(ctx, fresh, class); err != nil {
		t.Fatalf("deletePod: %v", err)
	}

	if err := k8s.Get(ctx, types.NamespacedName{Namespace: env.Namespace, Name: names.Pod}, &corev1.Pod{}); err != nil {
		t.Errorf("foreign pod was deleted: %v", err)
	}
}

func TestDeletePod_NoOpWhenAlreadyTerminating(t *testing.T) {
	class := mustCreateClassWithEngine(t, sandboxv1alpha1.EngineTypeNone)
	env := mustCreateEnv(t, "delete-pod-terminating")
	key := types.NamespacedName{Namespace: env.Namespace, Name: env.Name}
	r := newResourceReconciler(t, newFakeClock(fixedStart))
	settleResources(t, r, key)

	fresh := getEnv(t, key)
	if err := r.ensurePod(ctx, fresh, class); err != nil {
		t.Fatalf("ensurePod: %v", err)
	}
	names := render.ChildNames(env.Name)
	podKey := types.NamespacedName{Namespace: env.Namespace, Name: names.Pod}

	// Add a finalizer so Delete marks it terminating without removing it,
	// letting us observe the "already terminating" no-op branch.
	pod := getPod(t, podKey)
	controllerutil.AddFinalizer(pod, "sandbox.psenna.dev/test-finalizer")
	if err := k8s.Update(ctx, pod); err != nil {
		t.Fatalf("adding finalizer: %v", err)
	}
	if err := k8s.Delete(ctx, pod); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	fresh = getEnv(t, key)
	if err := r.deletePod(ctx, fresh, class); err != nil {
		t.Fatalf("deletePod on an already-terminating pod: %v, want nil", err)
	}

	t.Cleanup(func() {
		p := getPod(t, podKey)
		controllerutil.RemoveFinalizer(p, "sandbox.psenna.dev/test-finalizer")
		_ = k8s.Update(ctx, p)
	})
}

func TestDeletePod_DeletesOwnedPod(t *testing.T) {
	class := mustCreateClassWithEngine(t, sandboxv1alpha1.EngineTypeNone)
	env := mustCreateEnv(t, "delete-pod-owned")
	key := types.NamespacedName{Namespace: env.Namespace, Name: env.Name}
	r := newResourceReconciler(t, newFakeClock(fixedStart))
	settleResources(t, r, key)

	fresh := getEnv(t, key)
	if err := r.ensurePod(ctx, fresh, class); err != nil {
		t.Fatalf("ensurePod: %v", err)
	}
	names := render.ChildNames(env.Name)
	podKey := types.NamespacedName{Namespace: env.Namespace, Name: names.Pod}
	_ = getPod(t, podKey) // confirm it exists

	if err := r.deletePod(ctx, fresh, class); err != nil {
		t.Fatalf("deletePod: %v", err)
	}

	err := k8s.Get(ctx, podKey, &corev1.Pod{})
	if err == nil {
		// No finalizers on a plain pod: envtest has no kubelet to hold it
		// open via a finalizer, so the object should be gone outright, or
		// at minimum have a DeletionTimestamp set.
		p := getPod(t, podKey)
		if p.DeletionTimestamp.IsZero() {
			t.Error("pod still exists with no DeletionTimestamp after deletePod")
		}
		return
	}
	if !apierrors.IsNotFound(err) {
		t.Errorf("Get pod after deletePod: err = %v, want NotFound", err)
	}
}

func TestEnsurePod_OwnerReferenceEnablesGC(t *testing.T) {
	class := mustCreateClassWithEngine(t, sandboxv1alpha1.EngineTypeNone)
	env := mustCreateEnv(t, "ensure-pod-gc")
	key := types.NamespacedName{Namespace: env.Namespace, Name: env.Name}
	r := newResourceReconciler(t, newFakeClock(fixedStart))
	settleResources(t, r, key)

	fresh := getEnv(t, key)
	if err := r.ensurePod(ctx, fresh, class); err != nil {
		t.Fatalf("ensurePod: %v", err)
	}
	names := render.ChildNames(env.Name)
	pod := getPod(t, types.NamespacedName{Namespace: env.Namespace, Name: names.Pod})
	if !ownedByEnv(pod, fresh) {
		t.Error("rendered pod is not recognized as owned by env (garbage collection would not fire)")
	}
}

// ---- observePod ----

func TestObservePod_NoPod(t *testing.T) {
	class := mustCreateClassWithEngine(t, sandboxv1alpha1.EngineTypeNone)
	env := mustCreateEnv(t, "observe-pod-none")
	key := types.NamespacedName{Namespace: env.Namespace, Name: env.Name}
	r := newResourceReconciler(t, newFakeClock(fixedStart))
	settleResources(t, r, key)
	_ = class

	fresh := getEnv(t, key)
	var f lifecycle.ClusterFacts
	r.observePod(ctx, fresh, &f)

	if !f.PodObserved {
		t.Error("PodObserved = false, want true (a real NotFound lookup happened)")
	}
	if f.PodExists {
		t.Error("PodExists = true, want false")
	}
}

func TestObservePod_Pending(t *testing.T) {
	class := mustCreateClassWithEngine(t, sandboxv1alpha1.EngineTypeNone)
	env := mustCreateEnv(t, "observe-pod-pending")
	key := types.NamespacedName{Namespace: env.Namespace, Name: env.Name}
	r := newResourceReconciler(t, newFakeClock(fixedStart))
	settleResources(t, r, key)

	fresh := getEnv(t, key)
	if err := r.ensurePod(ctx, fresh, class); err != nil {
		t.Fatalf("ensurePod: %v", err)
	}

	var f lifecycle.ClusterFacts
	r.observePod(ctx, fresh, &f)

	if !f.PodObserved || !f.PodExists {
		t.Fatalf("PodObserved=%v PodExists=%v, want true/true", f.PodObserved, f.PodExists)
	}
	if f.PodPhase != corev1.PodPending {
		t.Errorf("PodPhase = %s, want Pending (envtest sets no phase, which defaults to \"\" -- but a freshly-created pod with no status defaults to empty string, not Pending: see assertion below)", f.PodPhase)
	}
	if f.PodReady {
		t.Error("PodReady = true, want false")
	}
	if f.PodFailure != nil {
		t.Errorf("PodFailure = %+v, want nil", f.PodFailure)
	}
}

func TestObservePod_RunningAndReady(t *testing.T) {
	class := mustCreateClassWithEngine(t, sandboxv1alpha1.EngineTypeNone)
	env := mustCreateEnv(t, "observe-pod-running")
	key := types.NamespacedName{Namespace: env.Namespace, Name: env.Name}
	r := newResourceReconciler(t, newFakeClock(fixedStart))
	settleResources(t, r, key)

	fresh := getEnv(t, key)
	if err := r.ensurePod(ctx, fresh, class); err != nil {
		t.Fatalf("ensurePod: %v", err)
	}
	names := render.ChildNames(env.Name)
	podKey := types.NamespacedName{Namespace: env.Namespace, Name: names.Pod}
	mustSetPodStatus(t, podKey, func(s *corev1.PodStatus) {
		s.Phase = corev1.PodRunning
		s.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	})

	var f lifecycle.ClusterFacts
	r.observePod(ctx, fresh, &f)

	if f.PodPhase != corev1.PodRunning {
		t.Errorf("PodPhase = %s, want Running", f.PodPhase)
	}
	if !f.PodReady {
		t.Error("PodReady = false, want true")
	}
	if f.PodFailure != nil {
		t.Errorf("PodFailure = %+v, want nil", f.PodFailure)
	}
}

func TestObservePod_Failed(t *testing.T) {
	class := mustCreateClassWithEngine(t, sandboxv1alpha1.EngineTypeNone)
	env := mustCreateEnv(t, "observe-pod-failed")
	key := types.NamespacedName{Namespace: env.Namespace, Name: env.Name}
	r := newResourceReconciler(t, newFakeClock(fixedStart))
	settleResources(t, r, key)

	fresh := getEnv(t, key)
	if err := r.ensurePod(ctx, fresh, class); err != nil {
		t.Fatalf("ensurePod: %v", err)
	}
	names := render.ChildNames(env.Name)
	podKey := types.NamespacedName{Namespace: env.Namespace, Name: names.Pod}
	mustSetPodStatus(t, podKey, func(s *corev1.PodStatus) {
		s.Phase = corev1.PodFailed
		s.ContainerStatuses = []corev1.ContainerStatus{
			{Name: render.AgentContainerName, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error"}}},
		}
	})

	var f lifecycle.ClusterFacts
	r.observePod(ctx, fresh, &f)

	if f.PodPhase != corev1.PodFailed {
		t.Errorf("PodPhase = %s, want Failed", f.PodPhase)
	}
	if f.PodFailure == nil {
		t.Fatal("PodFailure is nil, want set")
	}
	if f.PodFailure.Reason != lifecycle.ReasonPodFailed {
		t.Errorf("PodFailure.Reason = %s, want %s", f.PodFailure.Reason, lifecycle.ReasonPodFailed)
	}
}

func TestObservePod_ImagePullBackOff(t *testing.T) {
	class := mustCreateClassWithEngine(t, sandboxv1alpha1.EngineTypeNone)
	env := mustCreateEnv(t, "observe-pod-imagepull")
	key := types.NamespacedName{Namespace: env.Namespace, Name: env.Name}
	r := newResourceReconciler(t, newFakeClock(fixedStart))
	settleResources(t, r, key)

	fresh := getEnv(t, key)
	if err := r.ensurePod(ctx, fresh, class); err != nil {
		t.Fatalf("ensurePod: %v", err)
	}
	names := render.ChildNames(env.Name)
	podKey := types.NamespacedName{Namespace: env.Namespace, Name: names.Pod}
	mustSetPodStatus(t, podKey, func(s *corev1.PodStatus) {
		s.Phase = corev1.PodPending
		s.ContainerStatuses = []corev1.ContainerStatus{
			{Name: render.AgentContainerName, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: "back-off"}}},
		}
	})

	var f lifecycle.ClusterFacts
	r.observePod(ctx, fresh, &f)

	if f.PodFailure == nil {
		t.Fatal("PodFailure is nil, want set")
	}
	if f.PodFailure.Reason != lifecycle.ReasonImagePullFailure {
		t.Errorf("PodFailure.Reason = %s, want %s", f.PodFailure.Reason, lifecycle.ReasonImagePullFailure)
	}
}

func TestObservePod_AgedUnschedulable(t *testing.T) {
	class := mustCreateClassWithEngine(t, sandboxv1alpha1.EngineTypeNone)
	env := mustCreateEnv(t, "observe-pod-unschedulable")
	key := types.NamespacedName{Namespace: env.Namespace, Name: env.Name}
	clk := newFakeClock(fixedStart)
	r := newResourceReconciler(t, clk)
	settleResources(t, r, key)

	fresh := getEnv(t, key)
	if err := r.ensurePod(ctx, fresh, class); err != nil {
		t.Fatalf("ensurePod: %v", err)
	}
	names := render.ChildNames(env.Name)
	podKey := types.NamespacedName{Namespace: env.Namespace, Name: names.Pod}
	mustSetPodStatus(t, podKey, func(s *corev1.PodStatus) {
		s.Phase = corev1.PodPending
		s.Conditions = []corev1.PodCondition{{
			Type:               corev1.PodScheduled,
			Status:             corev1.ConditionFalse,
			Reason:             corev1.PodReasonUnschedulable,
			Message:            "insufficient cpu",
			LastTransitionTime: metav1.NewTime(fixedStart),
		}}
	})

	var f lifecycle.ClusterFacts
	r.observePod(ctx, fresh, &f)
	if f.PodFailure != nil {
		t.Fatalf("PodFailure = %+v, want nil (inside grace window)", f.PodFailure)
	}

	clk.Advance(3 * time.Minute)
	f = lifecycle.ClusterFacts{}
	r.observePod(ctx, fresh, &f)
	if f.PodFailure == nil {
		t.Fatal("PodFailure is nil after the grace window elapsed, want set")
	}
	if f.PodFailure.Reason != lifecycle.ReasonUnschedulable {
		t.Errorf("PodFailure.Reason = %s, want %s", f.PodFailure.Reason, lifecycle.ReasonUnschedulable)
	}
}

func TestObservePod_ForeignOwnedPodNotObserved(t *testing.T) {
	class := mustCreateClassWithEngine(t, sandboxv1alpha1.EngineTypeNone)
	_ = class
	env := mustCreateEnv(t, "observe-pod-foreign")
	key := types.NamespacedName{Namespace: env.Namespace, Name: env.Name}
	r := newResourceReconciler(t, newFakeClock(fixedStart))
	settleResources(t, r, key)

	names := render.ChildNames(env.Name)
	foreign := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: names.Pod, Namespace: env.Namespace},
		Spec: corev1.PodSpec{
			Containers:    []corev1.Container{{Name: "foreign", Image: "busybox"}},
			RestartPolicy: corev1.RestartPolicyNever,
		},
	}
	if err := k8s.Create(ctx, foreign); err != nil {
		t.Fatalf("creating foreign pod: %v", err)
	}
	t.Cleanup(func() { _ = k8s.Delete(ctx, foreign) })

	fresh := getEnv(t, key)
	var f lifecycle.ClusterFacts
	r.observePod(ctx, fresh, &f)

	if !f.PodObserved {
		t.Error("PodObserved = false, want true (a lookup happened, it just wasn't ours)")
	}
	if f.PodExists {
		t.Error("PodExists = true, want false for a foreign-owned pod")
	}
}

// TestReconcile_RestoringToRunningToDone is the strongest achievable proxy
// for the unreachable kind-based e2e criterion ("with engine: none a real
// agent pod runs a trivial task to completion on kind and the environment
// reaches Done"): envtest has no kubelet, so nothing schedules, pulls, or
// runs a real container. This test drives the reconciler through the exact
// same phase machinery a real cluster would exercise, faking only the parts
// a kubelet would normally supply (pod phase transitions), via
// mustSetPodStatus.
func TestReconcile_RestoringToRunningToDone(t *testing.T) {
	mustCreateClassWithEngine(t, sandboxv1alpha1.EngineTypeNone)
	env := mustCreateEnvIn(t, "restoring-to-done-ns", "restoring-to-done", 0)
	key := types.NamespacedName{Namespace: env.Namespace, Name: env.Name}

	clk := newFakeClock(fixedStart)
	r := newResourceReconciler(t, clk)
	sched := newSlotScheduler(t, 1, clk)
	sched.Namespace = env.Namespace

	// Pending -> Ready (creates child resources).
	reconcileOnce(t, r, key)
	reconcileOnce(t, r, key)
	if got := getEnv(t, key).Status.Phase; got != sandboxv1alpha1.PhaseReady {
		t.Fatalf("phase after 2 reconciles = %s, want Ready", got)
	}

	// Grant a slot (#20's SlotScheduler), then Ready -> Restoring.
	if _, err := sched.RunOnce(ctx); err != nil {
		t.Fatalf("SlotScheduler pass: %v", err)
	}
	reconcileOnce(t, r, key)
	afterRestoringEnter := getEnv(t, key)
	if afterRestoringEnter.Status.Phase != sandboxv1alpha1.PhaseRestoring {
		t.Fatalf("phase after slot grant = %s, want Restoring", afterRestoringEnter.Status.Phase)
	}

	names := render.ChildNames(env.Name)
	podKey := types.NamespacedName{Namespace: env.Namespace, Name: names.Pod}
	_ = getPod(t, podKey) // ensurePod must have created it

	// Force the pod Running+Ready (what a kubelet would do), reconcile:
	// Restoring -> Running.
	mustSetPodStatus(t, podKey, func(s *corev1.PodStatus) {
		s.Phase = corev1.PodRunning
		s.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	})
	reconcileOnce(t, r, key)
	afterRunning := getEnv(t, key)
	if afterRunning.Status.Phase != sandboxv1alpha1.PhaseRunning {
		t.Fatalf("phase after pod Running+Ready = %s, want Running", afterRunning.Status.Phase)
	}
	podReadyCond := findCondition(afterRunning, lifecycle.ConditionPodReady)
	if podReadyCond == nil || podReadyCond.Status != metav1.ConditionTrue || podReadyCond.Reason != lifecycle.ReasonPodRunning {
		t.Errorf("PodReady condition = %+v, want True/%s", podReadyCond, lifecycle.ReasonPodRunning)
	}

	// Force the pod Succeeded, reconcile: Running -> Done. #32 changed the
	// terminal ordering: the pod is NOT deleted on the Running->Done
	// transition any more (that would destroy the agent-home emptyDir before
	// the terminal archive could capture it). terminal() deletes it only once
	// status.archive is written -- which no archive Job writes in this test --
	// so the pod must still be present, untouched.
	mustSetPodStatus(t, podKey, func(s *corev1.PodStatus) {
		s.Phase = corev1.PodSucceeded
	})
	reconcileOnce(t, r, key)
	final := getEnv(t, key)
	if final.Status.Phase != sandboxv1alpha1.PhaseDone {
		t.Fatalf("final phase = %s, want Done", final.Status.Phase)
	}

	p := getPod(t, podKey)
	if !p.DeletionTimestamp.IsZero() {
		t.Error("pod has a DeletionTimestamp after reaching Done; #32 keeps it alive until the archive is written")
	}
}

var _ = ctrl.Request{} // keep ctrl imported if unused by future edits
var _ = client.ObjectKey{}
