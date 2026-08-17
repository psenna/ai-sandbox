package controller

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	sandboxv1alpha1 "github.com/psenna/ai-sandbox/operator/api/v1alpha1"
	"github.com/psenna/ai-sandbox/operator/internal/lifecycle"
)

var fixedStart = time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)

func getEnv(t *testing.T, key types.NamespacedName) *sandboxv1alpha1.SandboxEnvironment {
	t.Helper()
	env := &sandboxv1alpha1.SandboxEnvironment{}
	if err := k8s.Get(ctx, key, env); err != nil {
		t.Fatalf("Get(%s): %v", key, err)
	}
	return env
}

func findCondition(env *sandboxv1alpha1.SandboxEnvironment, condType string) *metav1.Condition {
	for i := range env.Status.Conditions {
		if env.Status.Conditions[i].Type == condType {
			return &env.Status.Conditions[i]
		}
	}
	return nil
}

func TestReconcile_NotFoundIsNoError(t *testing.T) {
	r := newReconciler(t, newFakeClock(fixedStart), newFactsStore())
	res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "does-not-exist"}})
	if err != nil {
		t.Fatalf("Reconcile returned error: %v", err)
	}
	if res != (ctrl.Result{}) {
		t.Errorf("Result = %+v, want zero value", res)
	}
}

func TestReconcile_BeingDeletedIsNoOp(t *testing.T) {
	env := mustCreateEnv(t, "being-deleted")
	key := types.NamespacedName{Namespace: env.Namespace, Name: env.Name}

	fresh := getEnv(t, key)
	controllerutil.AddFinalizer(fresh, "sandbox.psenna.dev/test-finalizer")
	if err := k8s.Update(ctx, fresh); err != nil {
		t.Fatalf("adding finalizer: %v", err)
	}
	if err := k8s.Delete(ctx, fresh); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	before := getEnv(t, key)
	if before.DeletionTimestamp.IsZero() {
		t.Fatalf("expected DeletionTimestamp to be set after Delete with a finalizer present")
	}
	beforeRV := before.ResourceVersion

	r := newReconciler(t, newFakeClock(fixedStart), newFactsStore())
	res := reconcileOnce(t, r, key)
	if res != (ctrl.Result{}) {
		t.Errorf("Result = %+v, want zero value", res)
	}

	after := getEnv(t, key)
	if after.ResourceVersion != beforeRV {
		t.Errorf("ResourceVersion changed: %s -> %s, want unchanged (no status write during deletion)", beforeRV, after.ResourceVersion)
	}

	// Clean up: remove the finalizer so envtest actually deletes the object.
	controllerutil.RemoveFinalizer(after, "sandbox.psenna.dev/test-finalizer")
	if err := k8s.Update(ctx, after); err != nil {
		t.Fatalf("removing finalizer: %v", err)
	}
}

func TestReconcile_MissingClassHoldsAndReportsUnknown(t *testing.T) {
	env := mustCreateEnv(t, "missing-class")
	key := types.NamespacedName{Namespace: env.Namespace, Name: env.Name}

	r := newReconciler(t, newFakeClock(fixedStart), newFactsStore())
	res := reconcileOnce(t, r, key)

	if res.RequeueAfter != lifecycle.ClassUnresolvedRequeue {
		t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, lifecycle.ClassUnresolvedRequeue)
	}

	got := getEnv(t, key)
	if got.Status.Phase != sandboxv1alpha1.PhasePending {
		t.Errorf("Phase = %s, want Pending (unchanged)", got.Status.Phase)
	}
	c := findCondition(got, lifecycle.ConditionReady)
	if c == nil || c.Status != metav1.ConditionUnknown || c.Reason != lifecycle.ReasonClassNotResolved {
		t.Errorf("Ready condition = %+v, want Unknown/%s", c, lifecycle.ReasonClassNotResolved)
	}
}

func TestReconcile_PendingHoldsWithStubObserver(t *testing.T) {
	mustCreateClass(t)
	env := mustCreateEnv(t, "pending-stub")
	key := types.NamespacedName{Namespace: env.Namespace, Name: env.Name}

	r := &Reconciler{Client: k8s, Clock: newFakeClock(fixedStart).Now, Observe: ObserveStub}
	reconcileOnce(t, r, key)

	got := getEnv(t, key)
	if got.Status.Phase != sandboxv1alpha1.PhasePending {
		t.Errorf("Phase = %s, want Pending", got.Status.Phase)
	}
	c := findCondition(got, lifecycle.ConditionScheduled)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != lifecycle.ReasonResourcesNotReady {
		t.Errorf("Scheduled condition = %+v, want False/%s", c, lifecycle.ReasonResourcesNotReady)
	}
	if got.Status.ObservedGeneration != 1 {
		t.Errorf("ObservedGeneration = %d, want 1", got.Status.ObservedGeneration)
	}
}

func TestReconcile_PendingToReady(t *testing.T) {
	mustCreateClass(t)
	env := mustCreateEnv(t, "pending-to-ready")
	key := types.NamespacedName{Namespace: env.Namespace, Name: env.Name}

	fs := newFactsStore()
	fs.set(key, lifecycle.ClusterFacts{ResourcesReady: true})
	r := newReconciler(t, newFakeClock(fixedStart), fs)
	reconcileOnce(t, r, key)

	got := getEnv(t, key)
	if got.Status.Phase != sandboxv1alpha1.PhaseReady {
		t.Fatalf("Phase = %s, want Ready", got.Status.Phase)
	}
	c := findCondition(got, lifecycle.ConditionScheduled)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != lifecycle.ReasonQueued {
		t.Errorf("Scheduled condition = %+v, want False/%s", c, lifecycle.ReasonQueued)
	}
	if got.Status.QueuedSince == nil {
		t.Errorf("QueuedSince is nil, want set")
	}
}

func TestReconcile_IdempotentStatusWrite(t *testing.T) {
	mustCreateClass(t)
	env := mustCreateEnv(t, "idempotent-write")
	key := types.NamespacedName{Namespace: env.Namespace, Name: env.Name}

	fs := newFactsStore()
	fs.set(key, lifecycle.ClusterFacts{ResourcesReady: true})
	clk := newFakeClock(fixedStart)
	r := newReconciler(t, clk, fs)

	// Reconcile until the phase settles at Ready.
	var got *sandboxv1alpha1.SandboxEnvironment
	for i := 0; i < 5; i++ {
		reconcileOnce(t, r, key)
		got = getEnv(t, key)
		if got.Status.Phase == sandboxv1alpha1.PhaseReady {
			break
		}
	}
	if got.Status.Phase != sandboxv1alpha1.PhaseReady {
		t.Fatalf("phase did not settle at Ready: %s", got.Status.Phase)
	}

	resourceVersion := got.ResourceVersion
	lastTransitions := make(map[string]metav1.Time, len(got.Status.Conditions))
	for _, c := range got.Status.Conditions {
		lastTransitions[c.Type] = c.LastTransitionTime
	}

	for i := 0; i < 5; i++ {
		clk.Advance(time.Second)
		reconcileOnce(t, r, key)

		again := getEnv(t, key)
		if again.ResourceVersion != resourceVersion {
			t.Fatalf("iteration %d: ResourceVersion changed: %s -> %s", i, resourceVersion, again.ResourceVersion)
		}
		for _, c := range again.Status.Conditions {
			want, ok := lastTransitions[c.Type]
			if !ok {
				t.Fatalf("iteration %d: unexpected new condition type %s", i, c.Type)
			}
			if !c.LastTransitionTime.Equal(&want) {
				t.Fatalf("iteration %d: condition %s LastTransitionTime changed: %v -> %v", i, c.Type, want, c.LastTransitionTime)
			}
		}
	}
}

func TestReconcile_ObservedGeneration(t *testing.T) {
	mustCreateClass(t)
	env := mustCreateEnv(t, "observed-generation")
	key := types.NamespacedName{Namespace: env.Namespace, Name: env.Name}

	r := newReconciler(t, newFakeClock(fixedStart), newFactsStore())
	reconcileOnce(t, r, key)

	got := getEnv(t, key)
	if got.Status.ObservedGeneration != 1 {
		t.Fatalf("ObservedGeneration = %d, want 1", got.Status.ObservedGeneration)
	}

	fresh := getEnv(t, key)
	fresh.Spec.Suspend = true
	if err := k8s.Update(ctx, fresh); err != nil {
		t.Fatalf("Update: %v", err)
	}
	bumped := getEnv(t, key)
	if bumped.Generation != 2 {
		t.Fatalf("Generation after spec update = %d, want 2", bumped.Generation)
	}

	reconcileOnce(t, r, key)
	got2 := getEnv(t, key)
	if got2.Status.ObservedGeneration != 2 {
		t.Errorf("ObservedGeneration = %d, want 2", got2.Status.ObservedGeneration)
	}
	for _, c := range got2.Status.Conditions {
		if c.ObservedGeneration != 2 {
			t.Errorf("condition %s ObservedGeneration = %d, want 2", c.Type, c.ObservedGeneration)
		}
	}
	c := findCondition(got2, lifecycle.ConditionReady)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != lifecycle.ReasonSuspended {
		t.Errorf("Ready condition = %+v, want False/%s", c, lifecycle.ReasonSuspended)
	}
}

func TestReconcile_TerminalIsSticky(t *testing.T) {
	mustCreateClass(t)
	env := mustCreateEnv(t, "terminal-sticky")
	key := types.NamespacedName{Namespace: env.Namespace, Name: env.Name}

	fresh := getEnv(t, key)
	fresh.Status.Phase = sandboxv1alpha1.PhaseDone
	if err := k8s.Status().Update(ctx, fresh); err != nil {
		t.Fatalf("forcing Done: %v", err)
	}

	fs := newFactsStore()
	fs.set(key, lifecycle.ClusterFacts{ResourcesReady: true, SlotGranted: true})
	r := newReconciler(t, newFakeClock(fixedStart), fs)

	var resourceVersions []string
	for i := 0; i < 3; i++ {
		reconcileOnce(t, r, key)
		got := getEnv(t, key)
		if got.Status.Phase != sandboxv1alpha1.PhaseDone {
			t.Fatalf("iteration %d: Phase = %s, want Done", i, got.Status.Phase)
		}
		resourceVersions = append(resourceVersions, got.ResourceVersion)
	}
	if resourceVersions[1] != resourceVersions[2] {
		t.Errorf("ResourceVersion changed between reconcile 2 and 3 after settling: %s -> %s", resourceVersions[1], resourceVersions[2])
	}
}

// driveToRunning forces env (identified by key) into phase Running via a
// direct status update, then reconciles once with steady-state Running
// facts so the reconciler's own Apply call stamps PodReady's
// LastTransitionTime from clk -- exactly what a real timeout evaluation
// needs as its anchor.
func driveToRunning(t *testing.T, r *Reconciler, fs *factsStore, key types.NamespacedName) {
	t.Helper()
	fresh := getEnv(t, key)
	fresh.Status.Phase = sandboxv1alpha1.PhaseRunning
	fresh.Status.Slot = sandboxv1alpha1.SlotStatus{Granted: true}
	if err := k8s.Status().Update(ctx, fresh); err != nil {
		t.Fatalf("forcing Running: %v", err)
	}
	fs.set(key, lifecycle.ClusterFacts{
		SlotGranted: true, PodObserved: true, PodExists: true,
		PodPhase: corev1.PodRunning, PodReady: true,
	})
	reconcileOnce(t, r, key)
	got := getEnv(t, key)
	if got.Status.Phase != sandboxv1alpha1.PhaseRunning {
		t.Fatalf("driveToRunning: Phase = %s, want Running", got.Status.Phase)
	}
}

func TestReconcile_SlotReleasedOnFreeze(t *testing.T) {
	mustCreateClass(t)
	env := mustCreateEnv(t, "slot-released-on-freeze")
	key := types.NamespacedName{Namespace: env.Namespace, Name: env.Name}

	fs := newFactsStore()
	clk := newFakeClock(fixedStart)
	r := newReconciler(t, clk, fs)

	driveToRunning(t, r, fs, key)

	// Running -> Freezing.
	fs.set(key, lifecycle.ClusterFacts{AgentWaitDeclared: true, SlotGranted: true, PodObserved: true, PodExists: true, PodPhase: corev1.PodRunning, PodReady: true})
	reconcileOnce(t, r, key)
	got := getEnv(t, key)
	if got.Status.Phase != sandboxv1alpha1.PhaseFreezing {
		t.Fatalf("Phase = %s, want Freezing", got.Status.Phase)
	}
	if !got.Status.Slot.Granted {
		t.Fatalf("Slot.Granted = false while Freezing, want true (still held)")
	}

	// Freezing -> Waiting.
	fs.set(key, lifecycle.ClusterFacts{SnapshotComplete: true, PodExists: false})
	reconcileOnce(t, r, key)
	got = getEnv(t, key)
	if got.Status.Phase != sandboxv1alpha1.PhaseWaiting {
		t.Fatalf("Phase = %s, want Waiting", got.Status.Phase)
	}
	if got.Status.Slot != (sandboxv1alpha1.SlotStatus{}) {
		t.Errorf("Slot = %+v, want zero value on reaching Waiting", got.Status.Slot)
	}
	if got.Status.FreezeCount != 1 {
		t.Errorf("FreezeCount = %d, want 1", got.Status.FreezeCount)
	}
}

func TestReconcile_RunningTimeoutWritesFailed(t *testing.T) {
	mustCreateClass(t)
	env := mustCreateEnv(t, "running-timeout")
	key := types.NamespacedName{Namespace: env.Namespace, Name: env.Name}

	fs := newFactsStore()
	clk := newFakeClock(fixedStart)
	r := newReconciler(t, clk, fs)

	driveToRunning(t, r, fs, key)

	clk.Advance(7 * time.Hour) // past the class's default 6h running timeout
	reconcileOnce(t, r, key)

	got := getEnv(t, key)
	if got.Status.Phase != sandboxv1alpha1.PhaseFailed {
		t.Fatalf("Phase = %s, want Failed", got.Status.Phase)
	}
	c := findCondition(got, lifecycle.ConditionReady)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != lifecycle.ReasonRunningTimeoutExceeded {
		t.Errorf("Ready condition = %+v, want False/%s", c, lifecycle.ReasonRunningTimeoutExceeded)
	}
	if got.Status.FinishedAt == nil {
		t.Errorf("FinishedAt is nil, want set")
	}
}

func TestSetupWithManager_ReconcilesOnCreate(t *testing.T) {
	mustCreateClass(t)

	mgr, err := ctrl.NewManager(k8sCfg, ctrl.Options{
		Scheme:                 k8s.Scheme(),
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		LeaderElection:         false,
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	rec := &Reconciler{Client: mgr.GetClient()}
	if err := rec.SetupWithManager(mgr); err != nil {
		t.Fatalf("SetupWithManager: %v", err)
	}

	mgrCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	startErr := make(chan error, 1)
	go func() { startErr <- mgr.Start(mgrCtx) }()

	if !mgr.GetCache().WaitForCacheSync(mgrCtx) {
		t.Fatalf("cache did not sync")
	}

	env := mustCreateEnv(t, "manager-reconciles")
	key := types.NamespacedName{Namespace: env.Namespace, Name: env.Name}

	deadline := time.Now().Add(10 * time.Second)
	var last *sandboxv1alpha1.SandboxEnvironment
	for time.Now().Before(deadline) {
		last = getEnv(t, key)
		if last.Status.ObservedGeneration == 1 {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if last == nil || last.Status.ObservedGeneration != 1 {
		t.Fatalf("observedGeneration did not reach 1 within 10s: %+v", last)
	}

	cancel()
	select {
	case err := <-startErr:
		if err != nil {
			t.Fatalf("mgr.Start returned error after shutdown: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("mgr.Start did not return within 30s of context cancellation")
	}
}

func TestActionsAreExhaustive(t *testing.T) {
	for _, a := range lifecycle.AllActions {
		if _, ok := actions[a]; !ok {
			t.Errorf("no handler registered in the actions map for %q", a)
		}
	}
}
