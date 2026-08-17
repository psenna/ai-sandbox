package controller

import (
	"context"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	sandboxv1alpha1 "github.com/psenna/ai-sandbox/operator/api/v1alpha1"
	"github.com/psenna/ai-sandbox/operator/internal/lifecycle"
)

// fakeAgentObserver is a stateless ObserveFunc test double for
// TestSlotScheduler_SerialisesThreeEnvironmentsWithCapacityOne: it always
// reports child resources ready, reports the pod Running/Ready as soon as a
// slot is granted, and reports AgentDone as soon as the environment has
// been PERSISTED as Running (the reconcile that first writes Restoring ->
// Running observes phase=Restoring, so this doesn't fire until the NEXT,
// watch-triggered reconcile -- one genuine pass at phase=Running). This
// deliberately does NOT gate on a dwell timer or a second observation:
// an earlier version required two persisted-Running observations before
// flipping AgentDone, using a short SandboxClass Running-timeout to force
// the second reconcile -- but that made the fixture race the very timeout
// it depended on (under load/-race, the watch-triggered second reconcile
// could itself land after the 1s timeout had already elapsed, failing the
// environment via RunningTimeoutExceeded instead of completing it, since
// internal/lifecycle.Next checks agent completion before timeouts and
// AgentDone wasn't yet true on that call). Flipping AgentDone on the first
// persisted-Running observation removes that race entirely, at no cost to
// what this test actually asserts (max concurrent {Restoring,Running} <=
// capacity): completion no longer depends on wall-clock timing at all.
func fakeAgentObserver(_ context.Context, env *sandboxv1alpha1.SandboxEnvironment, _ *sandboxv1alpha1.SandboxClass) (lifecycle.ClusterFacts, error) {
	f := lifecycle.ClusterFacts{
		ResourcesReady: true,
		SlotGranted:    env.Status.Slot.Granted,
	}
	if env.Status.Slot.Granted {
		f.PodObserved = true
		f.PodExists = true
		f.PodPhase = corev1.PodRunning
		f.PodReady = true
	}
	if env.Status.Phase == sandboxv1alpha1.PhaseRunning {
		f.AgentDone = true
	}
	return f, nil
}

// newSerialisationTestManager builds the real manager + Reconciler +
// SlotScheduler wiring shared by
// TestSlotScheduler_SerialisesThreeEnvironmentsWithCapacityOne, factored out
// to keep that test's own body focused on the assertions.
func newSerialisationTestManager(t *testing.T, ns string) manager.Manager {
	t.Helper()
	mgr, err := ctrl.NewManager(k8sCfg, ctrl.Options{
		Scheme:                 k8s.Scheme(),
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		LeaderElection:         false,
		// This package's other manager-based test
		// (TestSetupWithManager_ReconcilesOnCreate) also registers a
		// controller named "sandboxenvironment" via SetupWithManager's fixed
		// .Named(...) call. controller-runtime validates controller-name
		// uniqueness against a process-global registry (for metrics), which
		// otherwise collides across independent tests in the same test
		// binary even though each uses its own manager/cache.
		Controller: ctrlconfig.Controller{SkipNameValidation: ptr.To(true)},
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	rec := &Reconciler{
		Client:               mgr.GetClient(),
		ClassSecretNamespace: "default",
		ClusterID:            "test",
		Observe:              fakeAgentObserver,
	}
	if err := rec.SetupWithManager(mgr); err != nil {
		t.Fatalf("SetupWithManager: %v", err)
	}

	if err := mgr.Add(&SlotScheduler{
		Client:    mgr.GetClient(),
		Reader:    mgr.GetAPIReader(),
		Capacity:  1,
		Interval:  200 * time.Millisecond,
		Namespace: ns,
	}); err != nil {
		t.Fatalf("mgr.Add(SlotScheduler): %v", err)
	}
	return mgr
}

// mustCreateSoakEnvs creates one SandboxEnvironment per name in ns,
// referencing the "default" SandboxClass, with strictly decreasing priority
// so admission order is deterministic.
func mustCreateSoakEnvs(t *testing.T, ns string, names []string) {
	t.Helper()
	for i, name := range names {
		env := &sandboxv1alpha1.SandboxEnvironment{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: sandboxv1alpha1.SandboxEnvironmentSpec{
				ClassRef: sandboxv1alpha1.ClassRef{Name: "default"},
				Repo:     "org/repo",
				Task:     sandboxv1alpha1.TaskSpec{Prompt: "do stuff"},
				Priority: int32(100 - i), // deterministic admission order
			},
		}
		if err := k8s.Create(ctx, env); err != nil {
			t.Fatalf("creating %s: %v", name, err)
		}
	}
}

// sampleMaxConcurrentRestoringOrRunning polls the live (uncached) object
// list in ns every 25ms until stopped, tracking the running maximum number
// of environments simultaneously observed in {Restoring, Running}. The
// returned stop function cancels the sampler AND blocks until its goroutine
// has actually exited, so result() -- callable only after stop() returns --
// never races the sampler's own mutex-guarded write.
func sampleMaxConcurrentRestoringOrRunning(ctx context.Context, ns string) (result func() int, stop func()) {
	var mu sync.Mutex
	m := 0
	sampleCtx, cancel := context.WithCancel(ctx)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(25 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-sampleCtx.Done():
				return
			case <-ticker.C:
				n := countRestoringOrRunning(ns)
				mu.Lock()
				if n > m {
					m = n
				}
				mu.Unlock()
			}
		}
	}()
	return func() int {
			mu.Lock()
			defer mu.Unlock()
			return m
		}, func() {
			cancel()
			<-stopped
		}
}

func countRestoringOrRunning(ns string) int {
	var list sandboxv1alpha1.SandboxEnvironmentList
	if err := k8s.List(ctx, &list, client.InNamespace(ns)); err != nil {
		return 0
	}
	n := 0
	for _, e := range list.Items {
		if e.Status.Phase == sandboxv1alpha1.PhaseRestoring || e.Status.Phase == sandboxv1alpha1.PhaseRunning {
			n++
		}
	}
	return n
}

// waitAllDone blocks (bounded by deadline) until every environment in ns
// has reached Done, logging progress periodically and failing the test if
// the deadline is reached first.
func waitAllDone(t *testing.T, ns string, want int, deadline time.Duration) {
	t.Helper()
	end := time.Now().Add(deadline)
	lastLog := time.Now()
	for {
		var list sandboxv1alpha1.SandboxEnvironmentList
		if err := k8s.List(ctx, &list, client.InNamespace(ns)); err != nil {
			t.Fatalf("List: %v", err)
		}
		done := 0
		for _, e := range list.Items {
			if e.Status.Phase == sandboxv1alpha1.PhaseDone {
				done++
			}
		}
		if done == want {
			return
		}
		if time.Now().After(end) {
			logSoakProgress(t, "at timeout", list.Items)
			t.Fatalf("not all %d environments reached Done within %s (done=%d)", want, deadline, done)
		}
		if time.Since(lastLog) > 5*time.Second {
			logSoakProgress(t, "progress", list.Items)
			lastLog = time.Now()
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func logSoakProgress(t *testing.T, label string, items []sandboxv1alpha1.SandboxEnvironment) {
	t.Helper()
	for _, e := range items {
		t.Logf("%s: %s phase=%s granted=%v", label, e.Name, e.Status.Phase, e.Status.Slot.Granted)
	}
}

// TestSlotScheduler_SerialisesThreeEnvironmentsWithCapacityOne is the
// issue's own "Testing" line made concrete: envtest with capacity 1 and
// three environments, asserting serialised execution -- driven entirely by
// a real manager's watch loop plus SlotScheduler's grant writes. No test
// code anywhere in this test calls Reconcile manually.
func TestSlotScheduler_SerialisesThreeEnvironmentsWithCapacityOne(t *testing.T) {
	ns := testNamespace(t)
	mustCreateNamespace(t, ns)
	mustCreateClass(t) // name "default", 6h Running timeout -- irrelevant now, see fakeAgentObserver's comment

	mgr := newSerialisationTestManager(t, ns)

	mgrCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	startErr := make(chan error, 1)
	go func() { startErr <- mgr.Start(mgrCtx) }()

	if !mgr.GetCache().WaitForCacheSync(mgrCtx) {
		t.Fatalf("cache did not sync")
	}

	names := []string{"soak-a", "soak-b", "soak-c"}
	mustCreateSoakEnvs(t, ns, names)

	maxConcurrent, stopSampling := sampleMaxConcurrentRestoringOrRunning(mgrCtx, ns)
	defer stopSampling()

	waitAllDone(t, ns, len(names), 90*time.Second)

	stopSampling()
	if got := maxConcurrent(); got > 1 {
		t.Errorf("max concurrent {Restoring,Running} count = %d, want <= 1 (capacity 1 must serialise execution)", got)
	}

	for _, name := range names {
		env := &sandboxv1alpha1.SandboxEnvironment{}
		if err := k8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, env); err != nil {
			if apierrors.IsNotFound(err) {
				t.Errorf("%s: not found at end of test", name)
				continue
			}
			t.Fatalf("Get(%s): %v", name, err)
		}
		if env.Status.Phase != sandboxv1alpha1.PhaseDone {
			t.Errorf("%s: final Phase = %s, want Done", name, env.Status.Phase)
		}
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
