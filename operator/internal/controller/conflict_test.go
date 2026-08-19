package controller

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sandboxv1alpha1 "github.com/psenna/ai-sandbox/operator/api/v1alpha1"
	"github.com/psenna/ai-sandbox/operator/internal/lifecycle"
)

// conflictClient wraps the real envtest client, overriding Status() so its
// SubResourceWriter's Update injects a genuine, server-verified conflict on
// its first invocation: it uses a second, independent client to fetch and
// mutate the same object (an unrelated foreign condition) before forwarding
// the original, now-stale Update -- which the real API server rejects with
// an actual HTTP 409, not a simulated one.
type conflictClient struct {
	client.Client
	scheme *runtime.Scheme
	cfg    *rest.Config

	mu    sync.Mutex
	calls int
}

func (c *conflictClient) Status() client.SubResourceWriter {
	return &conflictStatusWriter{SubResourceWriter: c.Client.Status(), parent: c}
}

type conflictStatusWriter struct {
	client.SubResourceWriter
	parent *conflictClient
}

func (w *conflictStatusWriter) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	w.parent.mu.Lock()
	w.parent.calls++
	isFirst := w.parent.calls == 1
	w.parent.mu.Unlock()

	if isFirst {
		env, ok := obj.(*sandboxv1alpha1.SandboxEnvironment)
		if !ok {
			return fmt.Errorf("conflictStatusWriter: unexpected object type %T", obj)
		}
		secondary, err := client.New(w.parent.cfg, client.Options{Scheme: w.parent.scheme})
		if err != nil {
			return fmt.Errorf("building secondary client: %w", err)
		}
		interloper := &sandboxv1alpha1.SandboxEnvironment{}
		if err := secondary.Get(ctx, client.ObjectKeyFromObject(env), interloper); err != nil {
			return fmt.Errorf("secondary Get: %w", err)
		}
		apimeta.SetStatusCondition(&interloper.Status.Conditions, metav1.Condition{
			Type:    "NetworkPolicyEnforced",
			Status:  metav1.ConditionTrue,
			Reason:  "PolicyApplied",
			Message: "injected by TestWriteStatus_RetriesOnConflict",
		})
		if err := secondary.Status().Update(ctx, interloper); err != nil {
			return fmt.Errorf("secondary Status().Update: %w", err)
		}
	}

	return w.SubResourceWriter.Update(ctx, obj, opts...)
}

func testScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = sandboxv1alpha1.AddToScheme(scheme)
	return scheme
}

// TestWriteStatus_RetriesOnConflict proves writeStatus's retry.RetryOnConflict
// loop survives a genuine, server-verified 409, re-fetches the object (now
// carrying a concurrent writer's foreign condition), and lands the
// reconciler's own decision on top of it -- without clobbering the foreign
// write and without losing LastTransitionTime on conditions that never
// flipped Status.
func TestWriteStatus_RetriesOnConflict(t *testing.T) {
	mustCreateClass(t)
	env := mustCreateEnv(t, "retries-on-conflict")
	key := types.NamespacedName{Namespace: env.Namespace, Name: env.Name}

	t0 := fixedStart
	t1 := fixedStart.Add(time.Minute)

	// Baseline: settle the environment in Pending with a real (unwrapped)
	// write, so its six conditions get a genuine first LastTransitionTime
	// (t0) to later prove is preserved.
	baseline := getEnv(t, key)
	d0 := lifecycle.Next(*baseline, lifecycle.ClusterFacts{ClassResolved: true}, t0)
	desired0 := lifecycle.Apply(baseline, d0)
	baseline.Status = *desired0
	if err := k8s.Status().Update(ctx, baseline); err != nil {
		t.Fatalf("baseline Status().Update: %v", err)
	}

	baselineTransitions := make(map[string]metav1.Time, len(baseline.Status.Conditions))
	for _, c := range baseline.Status.Conditions {
		baselineTransitions[c.Type] = c.LastTransitionTime
	}

	// The real decision under test: ResourcesReady flips true, so Pending ->
	// Ready. Every condition's Status stays the same as it was in Pending
	// (only Reason/Message differ -- e.g. Scheduled stays False, just
	// ResourcesNotReady -> Queued), so none of them should get a new
	// LastTransitionTime, even though the object as a whole genuinely
	// changes (forcing a real Update call).
	decidedFrom := getEnv(t, key)
	d1 := lifecycle.Next(*decidedFrom, lifecycle.ClusterFacts{ClassResolved: true, ResourcesReady: true}, t1)
	if d1.Phase != sandboxv1alpha1.PhaseReady {
		t.Fatalf("setup: d1.Phase = %s, want Ready", d1.Phase)
	}

	wrapped := &conflictClient{Client: k8s, scheme: testScheme(), cfg: k8sCfg}
	r := &Reconciler{Client: wrapped}

	if err := r.writeStatus(ctx, decidedFrom, d1); err != nil {
		t.Fatalf("writeStatus: %v", err)
	}

	wrapped.mu.Lock()
	calls := wrapped.calls
	wrapped.mu.Unlock()
	if calls != 2 {
		t.Errorf("Update call count = %d, want 2", calls)
	}

	final := getEnv(t, key)
	if final.Status.Phase != sandboxv1alpha1.PhaseReady {
		t.Errorf("final Phase = %s, want Ready", final.Status.Phase)
	}

	foreign := findCondition(final, "NetworkPolicyEnforced")
	if foreign == nil || foreign.Status != metav1.ConditionTrue {
		t.Errorf("foreign condition NetworkPolicyEnforced missing or wrong after retry: %+v", foreign)
	}

	for _, condType := range lifecycle.ConditionTypes {
		c := findCondition(final, condType)
		if c == nil {
			t.Errorf("condition %s missing after retry", condType)
			continue
		}
		want, ok := baselineTransitions[condType]
		if !ok {
			t.Errorf("condition %s had no baseline LastTransitionTime to compare", condType)
			continue
		}
		if !c.LastTransitionTime.Equal(&want) {
			t.Errorf("condition %s LastTransitionTime changed across the retry: %v -> %v (status never flipped)", condType, want, c.LastTransitionTime)
		}
	}
}

// TestWriteStatus_ConcurrentReconciles drives a single SandboxEnvironment
// through a real Running -> Freezing -> Waiting freeze via two goroutines
// racing Get -> lifecycle.Next -> writeStatus against the same object. It is
// the concrete regression test for the generation/phase staleness guard in
// writeStatus: without it, both goroutines could compute a decision from the
// same "still Freezing" snapshot and each land an IncrementFreezeCount
// write, double-counting FreezeCount.
func TestWriteStatus_ConcurrentReconciles(t *testing.T) {
	mustCreateClass(t)
	env := mustCreateEnv(t, "concurrent-writestatus")
	key := types.NamespacedName{Namespace: env.Namespace, Name: env.Name}

	// spec.suspend=true makes the Running -> Freezing -> Waiting sequence
	// self-driving and terminal at Waiting (no external waitFor needed): see
	// nextRunning/nextFreezing/nextWaiting's suspend branches in next.go.
	specUpdate := getEnv(t, key)
	specUpdate.Spec.Suspend = true
	if err := k8s.Update(ctx, specUpdate); err != nil {
		t.Fatalf("setting spec.suspend: %v", err)
	}

	statusUpdate := getEnv(t, key)
	statusUpdate.Status.Phase = sandboxv1alpha1.PhaseRunning
	if err := k8s.Status().Update(ctx, statusUpdate); err != nil {
		t.Fatalf("forcing Running: %v", err)
	}

	r := &Reconciler{Client: k8s}
	now := fixedStart

	factsFor := func(phase sandboxv1alpha1.Phase) lifecycle.ClusterFacts {
		f := lifecycle.ClusterFacts{ClassResolved: true}
		switch phase {
		case sandboxv1alpha1.PhaseRunning:
			f.PodObserved = true
			f.PodExists = true
			f.PodPhase = corev1.PodRunning
			f.PodReady = true
		case sandboxv1alpha1.PhaseFreezing:
			f.SnapshotComplete = true
		}
		return f
	}

	errs := make(chan error, 200)
	var wg sync.WaitGroup
	worker := func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			obj := &sandboxv1alpha1.SandboxEnvironment{}
			if err := k8s.Get(ctx, key, obj); err != nil {
				errs <- fmt.Errorf("Get: %w", err)
				continue
			}
			d := lifecycle.Next(*obj, factsFor(obj.Status.Phase), now)
			if err := r.writeStatus(ctx, obj, d); err != nil && !errors.Is(err, errStaleDecision) {
				errs <- fmt.Errorf("writeStatus: %w", err)
			}
		}
	}
	wg.Add(2)
	go worker()
	go worker()
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("unexpected error from a concurrent writeStatus call: %v", err)
	}

	final := getEnv(t, key)
	if final.Status.Phase != sandboxv1alpha1.PhaseWaiting {
		t.Fatalf("final Phase = %s, want Waiting", final.Status.Phase)
	}
	if final.Status.FreezeCount != 1 {
		t.Errorf("FreezeCount = %d, want exactly 1 (double-increment regression)", final.Status.FreezeCount)
	}
}

// TestWriteStatus_ExternalWaitForClearRace is the regression test for #28's
// e2e-discovered stuck-at-Waiting bug: an external status-only write that
// clears status.waitFor (the shape a real wake API, or this repo's own
// "sequence numbers" e2e spec, performs) without touching Phase or
// Generation must NOT be silently absorbed by a reconcile whose Decision
// was computed from the pre-clear object. Before the fix, the staleness
// check in writeStatus compared only Generation and Phase -- both of which
// are unchanged by a WaitFor-only clear -- so the stale Decision (still
// "Waiting", the same phase already on the server) sailed through the
// DeepEqual no-op check and Apply's unconditional `s.Phase = d.Phase`
// permanently overwrote what should have become Ready, with no further
// write (and so no further watch event) to ever correct it.
func TestWriteStatus_ExternalWaitForClearRace(t *testing.T) {
	mustCreateClass(t)
	env := mustCreateEnv(t, "waitfor-clear-race")
	key := types.NamespacedName{Namespace: env.Namespace, Name: env.Name}

	// Settle the environment in Waiting with a real WaitFor declared, as if
	// a freeze had just completed (see nextFreezing's default branch).
	settled := getEnv(t, key)
	settled.Status.Phase = sandboxv1alpha1.PhaseWaiting
	settled.Status.WaitFor = &sandboxv1alpha1.WaitForStatus{Type: sandboxv1alpha1.WaitTypeNotBefore}
	if err := k8s.Status().Update(ctx, settled); err != nil {
		t.Fatalf("settling Waiting with WaitFor: %v", err)
	}

	// The reconcile under test reads the object HERE, while WaitFor is still
	// set -- so its Decision (computed below) reflects the pre-clear state.
	decidedFrom := getEnv(t, key)
	d := lifecycle.Next(*decidedFrom, lifecycle.ClusterFacts{ClassResolved: true, ProbeObserved: true}, fixedStart)
	if d.Phase != sandboxv1alpha1.PhaseWaiting {
		t.Fatalf("setup: d.Phase = %s, want Waiting (probe not yet satisfied)", d.Phase)
	}

	// An external actor (a wake API in production; the e2e harness's direct
	// admin-client patch today) clears WaitFor AFTER the read above but
	// BEFORE writeStatus runs -- the exact interleaving a real reconcile
	// loop cannot prevent.
	external := getEnv(t, key)
	external.Status.WaitFor = nil
	if err := k8s.Status().Update(ctx, external); err != nil {
		t.Fatalf("external WaitFor clear: %v", err)
	}

	r := &Reconciler{Client: k8s}
	err := r.writeStatus(ctx, decidedFrom, d)
	if !errors.Is(err, errStaleDecision) {
		t.Fatalf("writeStatus with a stale WaitFor read: err = %v, want errStaleDecision", err)
	}

	// The stale write must not have clobbered the external clear.
	afterStale := getEnv(t, key)
	if afterStale.Status.WaitFor != nil {
		t.Fatalf("stale writeStatus clobbered the external WaitFor clear: %+v", afterStale.Status.WaitFor)
	}
	if afterStale.Status.Phase != sandboxv1alpha1.PhaseWaiting {
		t.Fatalf("stale writeStatus changed Phase to %s, want it untouched (Waiting)", afterStale.Status.Phase)
	}

	// The follow-up reconcile (as would be triggered by the external
	// write's own watch event) now reads truly fresh state and correctly
	// advances to Ready.
	fresh := getEnv(t, key)
	d2 := lifecycle.Next(*fresh, lifecycle.ClusterFacts{ClassResolved: true, ProbeObserved: true}, fixedStart)
	if d2.Phase != sandboxv1alpha1.PhaseReady {
		t.Fatalf("follow-up Decision.Phase = %s, want Ready", d2.Phase)
	}
	if err := r.writeStatus(ctx, fresh, d2); err != nil {
		t.Fatalf("follow-up writeStatus: %v", err)
	}

	final := getEnv(t, key)
	if final.Status.Phase != sandboxv1alpha1.PhaseReady {
		t.Errorf("final Phase = %s, want Ready", final.Status.Phase)
	}
}

// TestWriteStatus_ExternalSnapshotRace is the regression test for #29's
// extension of the same staleness guard: lifecycle.Next branches on
// status.snapshot's nil-ness (nextRestoring's IncrementWakeCount:
// env.Status.Snapshot != nil), and an external status-only write that
// clears status.snapshot -- the shape a test harness driving the
// freeze/wake contract performs -- without touching Phase or Generation
// must NOT be silently absorbed by a reconcile whose Decision was computed
// from the pre-clear object. Before the fix, the staleness check in
// writeStatus compared only Generation, Phase, and WaitFor -- none of which
// change when snapshot is cleared -- so the stale Decision (still carrying
// IncrementWakeCount=true) sailed through and double-incremented WakeCount
// against a fresh object that no longer has a snapshot, exactly the #28
// bug class this same guard was extended for.
func TestWriteStatus_ExternalSnapshotRace(t *testing.T) {
	mustCreateClass(t)
	env := mustCreateEnv(t, "snapshot-race")
	key := types.NamespacedName{Namespace: env.Namespace, Name: env.Name}

	// Settle the environment in Restoring with a real snapshot recorded, as
	// if a freeze had just completed and a wake had begun (see
	// nextRestoring's IncrementWakeCount branch).
	settled := getEnv(t, key)
	settled.Status.Phase = sandboxv1alpha1.PhaseRestoring
	settled.Status.Snapshot = &sandboxv1alpha1.SnapshotStatus{Seq: 0, TakenAt: &metav1.Time{Time: fixedStart}}
	if err := k8s.Status().Update(ctx, settled); err != nil {
		t.Fatalf("settling Restoring with snapshot: %v", err)
	}

	// The reconcile under test reads the object HERE, while snapshot is
	// still set -- so its Decision (computed below) reflects the pre-clear
	// state.
	decidedFrom := getEnv(t, key)
	d := lifecycle.Next(*decidedFrom, lifecycle.ClusterFacts{
		ClassResolved: true, SlotGranted: true, PodObserved: true,
		PodPhase: corev1.PodRunning, PodReady: true,
	}, fixedStart)
	if d.Phase != sandboxv1alpha1.PhaseRunning {
		t.Fatalf("setup: d.Phase = %s, want Running", d.Phase)
	}
	if !d.StatusPatch.IncrementWakeCount {
		t.Fatal("setup: d.StatusPatch.IncrementWakeCount = false, want true (snapshot present)")
	}

	// An external actor clears status.snapshot AFTER the read above but
	// BEFORE writeStatus runs -- the exact interleaving a real reconcile
	// loop cannot prevent.
	external := getEnv(t, key)
	external.Status.Snapshot = nil
	if err := k8s.Status().Update(ctx, external); err != nil {
		t.Fatalf("external snapshot clear: %v", err)
	}

	r := &Reconciler{Client: k8s}
	err := r.writeStatus(ctx, decidedFrom, d)
	if !errors.Is(err, errStaleDecision) {
		t.Fatalf("writeStatus with a stale snapshot read: err = %v, want errStaleDecision", err)
	}

	// The stale write must not have clobbered the external clear.
	afterStale := getEnv(t, key)
	if afterStale.Status.Snapshot != nil {
		t.Fatalf("stale writeStatus clobbered the external snapshot clear: %+v", afterStale.Status.Snapshot)
	}
	if afterStale.Status.WakeCount != 0 {
		t.Fatalf("stale writeStatus incremented WakeCount to %d, want 0 (no snapshot on the fresh object)", afterStale.Status.WakeCount)
	}

	// The follow-up reconcile (as would be triggered by the external
	// write's own watch event) now reads truly fresh state and correctly
	// computes IncrementWakeCount=false.
	fresh := getEnv(t, key)
	d2 := lifecycle.Next(*fresh, lifecycle.ClusterFacts{
		ClassResolved: true, SlotGranted: true, PodObserved: true,
		PodPhase: corev1.PodRunning, PodReady: true,
	}, fixedStart)
	if d2.StatusPatch.IncrementWakeCount {
		t.Fatal("follow-up Decision.IncrementWakeCount = true, want false (snapshot cleared)")
	}
	if err := r.writeStatus(ctx, fresh, d2); err != nil {
		t.Fatalf("follow-up writeStatus: %v", err)
	}

	final := getEnv(t, key)
	if final.Status.WakeCount != 0 {
		t.Errorf("final WakeCount = %d, want 0", final.Status.WakeCount)
	}
}
