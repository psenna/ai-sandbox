package controller

import (
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	sandboxv1alpha1 "github.com/psenna/ai-sandbox/operator/api/v1alpha1"
	"github.com/psenna/ai-sandbox/operator/internal/lifecycle"
)

// TestSlotScheduler_NoSlotLeakOverManyCycles is the acceptance criterion's
// soak test: capacity 2, starting with 4 environments, 40 cycles cycling
// every one of the four slot-release exit paths (freeze, completion,
// failure, deletion). Every cycle asserts occupancy never exceeds capacity
// and that no environment outside an occupying phase still holds a granted
// slot -- the concrete leak signature this issue's design guards against
// (release driven off observed state, not a trusted transition).
//
// Deviation from the issue's sketch: the completion path additionally
// creates a fresh replacement environment (not just failure/deletion). The
// plan's literal text only replenishes after failure/deletion, but a
// completion-only-consumes-no-replacement rule shrinks the candidate pool
// every 4th cycle with nothing replacing it, which would eventually starve
// admissions and make assertion (5) (>=40 total admissions across 40
// cycles) fail for a reason that has nothing to do with a slot leak. Also,
// the "failure" path here always uses AgentFailed (not alternated with a
// clock-driven timeout): exercising the timeout path would require a
// dedicated SandboxClass with a short Running timeout, which is orthogonal
// to what this soak test is actually proving (slot release/no-leak), so it
// is left to internal/lifecycle's own timeout-focused tests
// (TestNext_Timeouts_Firing et al.) rather than duplicated here.
func TestSlotScheduler_NoSlotLeakOverManyCycles(t *testing.T) {
	mustCreateClass(t)
	ns := testNamespace(t)
	clk := newFakeClock(fixedStart)
	fs := newFactsStore()
	r := newReconciler(t, clk, fs)
	r.WatchNamespace = ns

	const capacity = 2
	s := newSlotScheduler(t, capacity, clk)
	s.Namespace = ns

	counter := 0
	newReadyEnv := func() types.NamespacedName {
		name := fmt.Sprintf("env-%d", counter)
		counter++
		mustCreateEnvIn(t, ns, name, 0)
		key := types.NamespacedName{Namespace: ns, Name: name}
		mustSetPhase(t, key, sandboxv1alpha1.PhaseReady, false, clk.Now())
		return key
	}

	for i := 0; i < 4; i++ {
		newReadyEnv()
	}

	totalAdmitted := 0
	runningFacts := func() lifecycle.ClusterFacts {
		return lifecycle.ClusterFacts{
			SlotGranted: true, PodObserved: true, PodExists: true,
			PodPhase: corev1.PodRunning, PodReady: true,
		}
	}

	for cycle := 0; cycle < 40; cycle++ {
		// (c) independent occupancy check: PassStats.Occupancy is computed
		// from the List snapshot taken BEFORE this pass's own grants, so the
		// independent count must be taken at the same moment (immediately
		// before RunOnce), not after.
		preOccupancy := countOccupying(t, ns)

		stats, err := s.RunOnce(ctx)
		if err != nil {
			t.Fatalf("cycle %d: RunOnce: %v", cycle, err)
		}
		totalAdmitted += stats.Admitted

		if preOccupancy != stats.Occupancy {
			t.Errorf("cycle %d: RunOnce.Occupancy = %d, independently-counted (pre-pass) occupancy = %d", cycle, stats.Occupancy, preOccupancy)
		}

		var granted []types.NamespacedName
		for _, g := range stats.Grants {
			if g.Granted {
				granted = append(granted, types.NamespacedName{Namespace: g.Namespace, Name: g.Name})
			}
		}

		for _, key := range granted {
			fs.set(key, runningFacts())
			reconcileOnce(t, r, key) // Ready -> Restoring
			reconcileOnce(t, r, key) // Restoring -> Running
		}

		switch cycle % 4 {
		case 0: // freeze
			for _, key := range granted {
				f := runningFacts()
				f.AgentWaitDeclared = true
				fs.set(key, f)
				reconcileOnce(t, r, key) // Running -> Freezing

				fs.set(key, lifecycle.ClusterFacts{SlotGranted: true, SnapshotComplete: true, PodObserved: true, PodExists: false})
				reconcileOnce(t, r, key) // Freezing -> Waiting

				fs.set(key, lifecycle.ClusterFacts{})
				reconcileOnce(t, r, key) // Waiting -> Ready (re-enters the queue)
			}
		case 1: // completion
			for _, key := range granted {
				f := runningFacts()
				f.AgentDone = true
				fs.set(key, f)
				reconcileOnce(t, r, key) // Running -> Done
				newReadyEnv()            // see deviation note above
			}
		case 2: // failure
			for _, key := range granted {
				f := runningFacts()
				f.AgentFailed = true
				fs.set(key, f)
				reconcileOnce(t, r, key) // Running -> Failed
				newReadyEnv()
			}
		case 3: // deletion: slot never released through the normal status
			// path -- it just stops existing. #32's FinalizerArchiveOnDelete
			// means the object stays Terminating until its terminal archive is
			// written, so simulate the sandboxctl archive Job's own
			// status.archive write (nothing in this suite runs real Jobs) and
			// reconcile once more so reconcileDelete's "already archived" step
			// clears the finalizer and the object is actually GC'd -- exactly
			// what a real archive Job completing would eventually cause.
			for _, key := range granted {
				env := &sandboxv1alpha1.SandboxEnvironment{}
				if err := k8s.Get(ctx, key, env); err != nil {
					t.Fatalf("cycle %d: Get(%s) before delete: %v", cycle, key, err)
				}
				if err := k8s.Delete(ctx, env); err != nil {
					t.Fatalf("cycle %d: Delete(%s): %v", cycle, key, err)
				}
				fresh := &sandboxv1alpha1.SandboxEnvironment{}
				if err := k8s.Get(ctx, key, fresh); err != nil {
					t.Fatalf("cycle %d: Get(%s) after delete: %v", cycle, key, err)
				}
				fresh.Status.Archive = &sandboxv1alpha1.ArchiveStatus{URI: "s3://sandbox-snapshots/fake-archive"}
				if err := k8s.Status().Update(ctx, fresh); err != nil {
					t.Fatalf("cycle %d: faking archive completion for %s: %v", cycle, key, err)
				}
				reconcileOnce(t, r, key) // reconcileDelete: archive present -> finalizer cleared -> object GC'd
				newReadyEnv()
			}
		}

		// (a) never over capacity.
		if got := countGranted(t, ns); got > capacity {
			t.Fatalf("cycle %d: countGranted = %d, exceeds capacity %d", cycle, got, capacity)
		}
		// (b) the actual leak assertion: nothing outside an occupying phase
		// still holds a granted slot.
		for _, e := range listEnvs(t, ns) {
			occupying := e.Status.Phase == sandboxv1alpha1.PhaseRestoring ||
				e.Status.Phase == sandboxv1alpha1.PhaseRunning ||
				e.Status.Phase == sandboxv1alpha1.PhaseFreezing
			if !occupying && e.Status.Slot != (sandboxv1alpha1.SlotStatus{}) {
				t.Fatalf("cycle %d: %s is Phase=%s (non-occupying) but Slot=%+v (leaked)", cycle, e.Name, e.Status.Phase, e.Status.Slot)
			}
		}

		clk.Advance(time.Second)
	}

	if totalAdmitted < 40 {
		t.Errorf("totalAdmitted over 40 cycles = %d, want >= 40 (possible permanent slot leak/starvation)", totalAdmitted)
	}
}
