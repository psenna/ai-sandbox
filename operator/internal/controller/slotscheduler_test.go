package controller

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	sandboxv1alpha1 "github.com/psenna/ai-sandbox/operator/api/v1alpha1"
	"github.com/psenna/ai-sandbox/operator/internal/scheduler"
)

var nonAlnum = regexp.MustCompile(`[^a-z0-9-]+`)

// testNamespace returns a unique, RFC1123-safe namespace name derived from
// t.Name(), so each test's fixtures are isolated from every other test's
// (many of which reuse "default" without cleanup). It does NOT create the
// namespace -- callers go through mustCreateEnvIn/mustCreateNamespace for
// that.
func testNamespace(t *testing.T) string {
	t.Helper()
	name := "ns-" + nonAlnum.ReplaceAllString(strings.ToLower(t.Name()), "-")
	if len(name) > 63 {
		name = name[:63]
	}
	return strings.Trim(name, "-")
}

// ---- basic admission behaviour ----

func TestSlotScheduler_AdmitsExactlyRemainingCapacity(t *testing.T) {
	mustCreateClass(t)
	ns := testNamespace(t)
	names := []string{"env-a", "env-b", "env-c", "env-d", "env-e"}
	priorities := []int32{50, 40, 30, 20, 10}
	for i, name := range names {
		env := mustCreateEnvIn(t, ns, name, priorities[i])
		mustSetPhase(t, types.NamespacedName{Namespace: ns, Name: name}, sandboxv1alpha1.PhaseReady, false, fixedStart.Add(time.Duration(i)*time.Second))
		_ = env
	}

	clk := newFakeClock(fixedStart)
	s := newSlotScheduler(t, 2, clk)
	s.Namespace = ns

	stats, err := s.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if stats.Admitted != 2 {
		t.Fatalf("Admitted = %d, want 2 (stats=%+v)", stats.Admitted, stats)
	}

	granted := map[string]bool{}
	for _, e := range listEnvs(t, ns) {
		granted[e.Name] = e.Status.Slot.Granted
	}
	for _, name := range []string{"env-a", "env-b"} {
		if !granted[name] {
			t.Errorf("%s: want granted, got not granted", name)
		}
	}
	for _, name := range []string{"env-c", "env-d", "env-e"} {
		if granted[name] {
			t.Errorf("%s: want not granted, got granted", name)
		}
		var e sandboxv1alpha1.SandboxEnvironment
		if err := k8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &e); err != nil {
			t.Fatalf("Get(%s): %v", name, err)
		}
		if e.Status.Slot != (sandboxv1alpha1.SlotStatus{}) {
			t.Errorf("%s: Slot = %+v, want zero value", name, e.Status.Slot)
		}
		if e.Status.Phase != sandboxv1alpha1.PhaseReady {
			t.Errorf("%s: Phase = %s, want Ready", name, e.Status.Phase)
		}
	}
}

func TestSlotScheduler_SecondPassIsIdempotent(t *testing.T) {
	mustCreateClass(t)
	ns := testNamespace(t)
	for i, name := range []string{"env-a", "env-b", "env-c"} {
		mustCreateEnvIn(t, ns, name, int32(10-i))
		mustSetPhase(t, types.NamespacedName{Namespace: ns, Name: name}, sandboxv1alpha1.PhaseReady, false, fixedStart.Add(time.Duration(i)*time.Second))
	}

	clk := newFakeClock(fixedStart)
	s := newSlotScheduler(t, 2, clk)
	s.Namespace = ns

	if _, err := s.RunOnce(ctx); err != nil {
		t.Fatalf("first RunOnce: %v", err)
	}
	if got := countGranted(t, ns); got != 2 {
		t.Fatalf("after first pass: granted = %d, want 2", got)
	}

	stats2, err := s.RunOnce(ctx)
	if err != nil {
		t.Fatalf("second RunOnce: %v", err)
	}
	if stats2.Admitted != 0 {
		t.Errorf("second pass Admitted = %d, want 0 (D3(2) occupancy hazard: granted-but-Ready must count as occupied)", stats2.Admitted)
	}
	if got := countGranted(t, ns); got != 2 {
		t.Errorf("after second pass: granted = %d, want still 2", got)
	}
}

func TestSlotScheduler_LoweringCapacityEvictsNothing(t *testing.T) {
	mustCreateClass(t)
	ns := testNamespace(t)
	for i, name := range []string{"env-a", "env-b", "env-c"} {
		mustCreateEnvIn(t, ns, name, int32(i))
		mustSetPhase(t, types.NamespacedName{Namespace: ns, Name: name}, sandboxv1alpha1.PhaseRunning, true, fixedStart)
	}

	clk := newFakeClock(fixedStart)
	s := newSlotScheduler(t, 1, clk)
	s.Namespace = ns

	stats, err := s.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if stats.Occupancy != 3 {
		t.Errorf("Occupancy = %d, want 3", stats.Occupancy)
	}
	if stats.Admitted != 0 {
		t.Errorf("Admitted = %d, want 0", stats.Admitted)
	}
	for _, e := range listEnvs(t, ns) {
		if !e.Status.Slot.Granted {
			t.Errorf("%s: Slot.Granted = false, want still true (capacity lowered must never evict)", e.Name)
		}
		if e.Status.Phase != sandboxv1alpha1.PhaseRunning {
			t.Errorf("%s: Phase = %s, want still Running", e.Name, e.Status.Phase)
		}
	}
}

func TestSlotScheduler_SkipsSuspendedTerminatingAndNonReady(t *testing.T) {
	mustCreateClass(t)
	ns := testNamespace(t)

	suspended := mustCreateEnvIn(t, ns, "suspended", 100)
	suspended.Spec.Suspend = true
	if err := k8s.Update(ctx, suspended); err != nil {
		t.Fatalf("setting suspend: %v", err)
	}
	mustSetPhase(t, types.NamespacedName{Namespace: ns, Name: "suspended"}, sandboxv1alpha1.PhaseReady, false, fixedStart)

	terminating := mustCreateEnvIn(t, ns, "terminating", 100)
	terminating.Finalizers = []string{"sandbox.psenna.dev/test-hold"}
	if err := k8s.Update(ctx, terminating); err != nil {
		t.Fatalf("adding finalizer: %v", err)
	}
	mustSetPhase(t, types.NamespacedName{Namespace: ns, Name: "terminating"}, sandboxv1alpha1.PhaseReady, false, fixedStart)
	if err := k8s.Delete(ctx, terminating); err != nil {
		t.Fatalf("deleting: %v", err)
	}
	t.Cleanup(func() {
		obj := &sandboxv1alpha1.SandboxEnvironment{}
		if err := k8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: "terminating"}, obj); err == nil {
			obj.Finalizers = nil
			_ = k8s.Update(ctx, obj)
		}
	})

	mustCreateEnvIn(t, ns, "pending", 100) // stays Pending by default
	mustCreateEnvIn(t, ns, "waiting", 100)
	mustSetPhase(t, types.NamespacedName{Namespace: ns, Name: "waiting"}, sandboxv1alpha1.PhaseWaiting, false, fixedStart)
	mustCreateEnvIn(t, ns, "done", 100)
	mustSetPhase(t, types.NamespacedName{Namespace: ns, Name: "done"}, sandboxv1alpha1.PhaseDone, false, fixedStart)

	clk := newFakeClock(fixedStart)
	s := newSlotScheduler(t, 10, clk)
	s.Namespace = ns

	stats, err := s.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if stats.Admitted != 0 {
		t.Errorf("Admitted = %d, want 0 (none of the fixtures should be candidates)", stats.Admitted)
	}
	if stats.QueueDepth != 0 {
		t.Errorf("QueueDepth = %d, want 0", stats.QueueDepth)
	}
	if got := countGranted(t, ns); got != 0 {
		t.Errorf("granted = %d, want 0", got)
	}
}

// ---- namespace scoping ----

func TestSlotScheduler_ScopesListToNamespace(t *testing.T) {
	mustCreateClass(t)
	nsA := testNamespace(t) + "-a"
	nsB := testNamespace(t) + "-b"
	mustCreateEnvIn(t, nsA, "env-a", 10)
	mustSetPhase(t, types.NamespacedName{Namespace: nsA, Name: "env-a"}, sandboxv1alpha1.PhaseReady, false, fixedStart)
	mustCreateEnvIn(t, nsB, "env-b", 10)
	mustSetPhase(t, types.NamespacedName{Namespace: nsB, Name: "env-b"}, sandboxv1alpha1.PhaseReady, false, fixedStart)

	clk := newFakeClock(fixedStart)
	sScoped := newSlotScheduler(t, 10, clk)
	sScoped.Namespace = nsA
	statsScoped, err := sScoped.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce (scoped): %v", err)
	}
	if statsScoped.Admitted != 1 {
		t.Fatalf("scoped Admitted = %d, want 1", statsScoped.Admitted)
	}
	if got := countGranted(t, nsB); got != 0 {
		t.Errorf("nsB granted after nsA-scoped pass = %d, want 0", got)
	}

	// The unscoped pass shares the whole envtest apiserver with every other
	// test in this package (many of which leave granted/occupying fixtures
	// behind, accumulating real occupancy across the suite), so its exact
	// Admitted count isn't a reliable assertion here, and capacity must be
	// generous enough to not be exhausted by that accumulated occupancy.
	// What matters for this test is that it considers nsB at all -- and
	// that nsA's earlier grant is left untouched.
	sAll := newSlotScheduler(t, 1000, clk)
	sAll.Namespace = ""
	if _, err := sAll.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce (all): %v", err)
	}
	if got := countGranted(t, nsB); got != 1 {
		t.Errorf("nsB granted after unscoped pass = %d, want 1", got)
	}
	if got := countGranted(t, nsA); got != 1 {
		t.Errorf("nsA granted after unscoped pass = %d, want still 1 (must not be evicted)", got)
	}
}

// ---- concurrency / conflict handling ----

func TestSlotScheduler_ConcurrentPassesNeverDoubleGrant(t *testing.T) {
	mustCreateClass(t)
	ns := testNamespace(t)
	mustCreateEnvIn(t, ns, "high", 100)
	mustSetPhase(t, types.NamespacedName{Namespace: ns, Name: "high"}, sandboxv1alpha1.PhaseReady, false, fixedStart)
	mustCreateEnvIn(t, ns, "low", 10)
	mustSetPhase(t, types.NamespacedName{Namespace: ns, Name: "low"}, sandboxv1alpha1.PhaseReady, false, fixedStart)

	clk := newFakeClock(fixedStart)
	errs := make(chan error, 4000)
	var wg sync.WaitGroup
	worker := func() {
		defer wg.Done()
		s := newSlotScheduler(t, 1, clk)
		s.Namespace = ns
		for i := 0; i < 20; i++ {
			if _, err := s.RunOnce(ctx); err != nil {
				errs <- err
			}
		}
	}
	wg.Add(2)
	go worker()
	go worker()
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("unexpected error from a concurrent RunOnce: %v", err)
	}

	envs := listEnvs(t, ns)
	grantedCount := 0
	var winner string
	for _, e := range envs {
		if e.Status.Slot.Granted {
			grantedCount++
			winner = e.Name
		}
	}
	if grantedCount != 1 {
		t.Fatalf("granted count = %d, want exactly 1 (envs=%+v)", grantedCount, envs)
	}
	if winner != "high" {
		t.Errorf("granted env = %q, want %q (deterministic ordering: higher priority wins)", winner, "high")
	}
}

func TestSlotScheduler_UIDMismatchSkipsGrant(t *testing.T) {
	mustCreateClass(t)
	ns := testNamespace(t)
	env := mustCreateEnvIn(t, ns, "recreated", 10)
	key := types.NamespacedName{Namespace: ns, Name: env.Name}
	staleUID := env.UID

	if err := k8s.Delete(ctx, env); err != nil {
		t.Fatalf("deleting original: %v", err)
	}
	// No finalizer was added, so the delete completes immediately. Recreate
	// with the same name -- a fresh object, a fresh UID.
	recreated := mustCreateEnvIn(t, ns, "recreated", 10)
	if recreated.UID == staleUID {
		t.Fatalf("setup: recreated object got the same UID as the original (test is not exercising delete+recreate)")
	}
	mustSetPhase(t, key, sandboxv1alpha1.PhaseReady, false, fixedStart)

	clk := newFakeClock(fixedStart)
	s := newSlotScheduler(t, 1, clk)
	s.Namespace = ns

	g := scheduler.Grant{Namespace: ns, Name: env.Name, UID: staleUID}
	granted, err := s.grant(ctx, g, fixedStart)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if granted {
		t.Error("grant() returned granted=true for a stale UID, want false")
	}

	final := &sandboxv1alpha1.SandboxEnvironment{}
	if err := k8s.Get(ctx, key, final); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if final.Status.Slot.Granted {
		t.Error("the recreated object was granted a slot via a stale-UID Grant")
	}
}

// grantConflictClient wraps the real envtest client for the SlotScheduler's
// Client field, injecting a real, server-verified conflict on the first
// Status().Update call for a SandboxEnvironment: before forwarding the
// original (now-stale) Update, it uses a second, independent client to
// fetch and mutate the same object via interloperFn. Mirrors
// conflict_test.go's conflictClient, generalized to a caller-supplied
// interloper mutation.
type grantConflictClient struct {
	client.Client
	scheme       *runtime.Scheme
	cfg          *rest.Config
	interloperFn func(secondary client.Client, key client.ObjectKey) error

	mu    sync.Mutex
	calls int
}

func (c *grantConflictClient) Status() client.SubResourceWriter {
	return &grantConflictStatusWriter{SubResourceWriter: c.Client.Status(), parent: c}
}

type grantConflictStatusWriter struct {
	client.SubResourceWriter
	parent *grantConflictClient
}

func (w *grantConflictStatusWriter) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	w.parent.mu.Lock()
	w.parent.calls++
	isFirst := w.parent.calls == 1
	w.parent.mu.Unlock()

	if isFirst && w.parent.interloperFn != nil {
		secondary, err := client.New(w.parent.cfg, client.Options{Scheme: w.parent.scheme})
		if err != nil {
			return fmt.Errorf("building secondary client: %w", err)
		}
		if err := w.parent.interloperFn(secondary, client.ObjectKeyFromObject(obj)); err != nil {
			return fmt.Errorf("interloper mutation: %w", err)
		}
	}

	return w.SubResourceWriter.Update(ctx, obj, opts...)
}

func TestSlotScheduler_GrantRetriesOnConflict(t *testing.T) {
	mustCreateClass(t)
	ns := testNamespace(t)
	env := mustCreateEnvIn(t, ns, "conflict-env", 10)
	key := types.NamespacedName{Namespace: ns, Name: env.Name}
	mustSetPhase(t, key, sandboxv1alpha1.PhaseReady, false, fixedStart)

	wrapped := &grantConflictClient{
		Client: k8s, scheme: testScheme(), cfg: k8sCfg,
		interloperFn: func(secondary client.Client, key client.ObjectKey) error {
			interloper := &sandboxv1alpha1.SandboxEnvironment{}
			if err := secondary.Get(ctx, key, interloper); err != nil {
				return err
			}
			apimeta.SetStatusCondition(&interloper.Status.Conditions, metav1.Condition{
				Type: "NetworkPolicyEnforced", Status: metav1.ConditionTrue, Reason: "PolicyApplied",
				Message: "injected by TestSlotScheduler_GrantRetriesOnConflict",
			})
			return secondary.Status().Update(ctx, interloper)
		},
	}

	fresh := &sandboxv1alpha1.SandboxEnvironment{}
	if err := k8s.Get(ctx, key, fresh); err != nil {
		t.Fatalf("Get: %v", err)
	}

	s := &SlotScheduler{Client: wrapped, Reader: k8s, Capacity: 1, Clock: func() time.Time { return fixedStart }}
	g := scheduler.Grant{Namespace: ns, Name: env.Name, UID: fresh.UID}
	granted, err := s.grant(ctx, g, fixedStart)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if !granted {
		t.Fatal("grant() returned granted=false, want true")
	}

	wrapped.mu.Lock()
	calls := wrapped.calls
	wrapped.mu.Unlock()
	if calls != 2 {
		t.Errorf("Update call count = %d, want 2", calls)
	}

	final := &sandboxv1alpha1.SandboxEnvironment{}
	if err := k8s.Get(ctx, key, final); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !final.Status.Slot.Granted {
		t.Error("final Slot.Granted = false, want true")
	}
	foreign := findCondition(final, "NetworkPolicyEnforced")
	if foreign == nil || foreign.Status != metav1.ConditionTrue {
		t.Errorf("foreign condition NetworkPolicyEnforced missing or wrong after retry: %+v", foreign)
	}
}

func TestSlotScheduler_GrantSkipsWhenInterloperGrantsFirst(t *testing.T) {
	mustCreateClass(t)
	ns := testNamespace(t)
	env := mustCreateEnvIn(t, ns, "interloper-env", 10)
	key := types.NamespacedName{Namespace: ns, Name: env.Name}
	mustSetPhase(t, key, sandboxv1alpha1.PhaseReady, false, fixedStart)

	interloperGrantTime := fixedStart.Add(7 * time.Minute)

	wrapped := &grantConflictClient{
		Client: k8s, scheme: testScheme(), cfg: k8sCfg,
		interloperFn: func(secondary client.Client, key client.ObjectKey) error {
			interloper := &sandboxv1alpha1.SandboxEnvironment{}
			if err := secondary.Get(ctx, key, interloper); err != nil {
				return err
			}
			at := metav1.NewTime(interloperGrantTime)
			interloper.Status.Slot = sandboxv1alpha1.SlotStatus{Granted: true, GrantedAt: &at}
			return secondary.Status().Update(ctx, interloper)
		},
	}

	fresh := &sandboxv1alpha1.SandboxEnvironment{}
	if err := k8s.Get(ctx, key, fresh); err != nil {
		t.Fatalf("Get: %v", err)
	}

	s := &SlotScheduler{Client: wrapped, Reader: k8s, Capacity: 1, Clock: func() time.Time { return fixedStart }}
	g := scheduler.Grant{Namespace: ns, Name: env.Name, UID: fresh.UID}
	granted, err := s.grant(ctx, g, fixedStart)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if granted {
		t.Error("grant() returned granted=true, want false: our own write must not have happened")
	}

	wrapped.mu.Lock()
	calls := wrapped.calls
	wrapped.mu.Unlock()
	if calls != 1 {
		t.Errorf("Update call count = %d, want 1 (re-validation must skip before a second Update attempt)", calls)
	}

	final := &sandboxv1alpha1.SandboxEnvironment{}
	if err := k8s.Get(ctx, key, final); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !final.Status.Slot.Granted {
		t.Fatal("final Slot.Granted = false, want true (interloper's grant)")
	}
	if final.Status.Slot.GrantedAt == nil || !final.Status.Slot.GrantedAt.Time.Equal(interloperGrantTime) {
		t.Errorf("final GrantedAt = %v, want %v (must be the interloper's, not ours)", final.Status.Slot.GrantedAt, interloperGrantTime)
	}
}

// ---- lifecycle / plumbing ----

func TestSlotScheduler_StopsOnContextCancel(t *testing.T) {
	clk := newFakeClock(fixedStart)
	s := newSlotScheduler(t, 1, clk)
	s.Namespace = testNamespace(t) // empty namespace, no fixtures needed

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		done <- s.Start(runCtx)
	}()

	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Start() returned error after cancel: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start() did not return within 5s of context cancellation")
	}
}

func TestSlotScheduler_IsLeaderElectionRunnable(t *testing.T) {
	s := &SlotScheduler{}
	if !s.NeedLeaderElection() {
		t.Error("NeedLeaderElection() = false, want true")
	}
	var _ manager.Runnable = s
	var _ manager.LeaderElectionRunnable = s
}

func TestSlotScheduler_OnPassReportsStats(t *testing.T) {
	mustCreateClass(t)
	ns := testNamespace(t)
	queuedAt := fixedStart.Add(-3 * time.Minute)
	mustCreateEnvIn(t, ns, "env-a", 10)
	mustSetPhase(t, types.NamespacedName{Namespace: ns, Name: "env-a"}, sandboxv1alpha1.PhaseReady, false, queuedAt)
	mustCreateEnvIn(t, ns, "env-b", 5)
	mustSetPhase(t, types.NamespacedName{Namespace: ns, Name: "env-b"}, sandboxv1alpha1.PhaseReady, false, fixedStart)
	mustCreateEnvIn(t, ns, "env-occupied", 1)
	mustSetPhase(t, types.NamespacedName{Namespace: ns, Name: "env-occupied"}, sandboxv1alpha1.PhaseRunning, true, fixedStart)

	clk := newFakeClock(fixedStart)
	s := newSlotScheduler(t, 2, clk)
	s.Namespace = ns

	var got PassStats
	s.OnPass = func(p PassStats) { got = p }

	// OnPass is only invoked from runAndLog (Start's loop), RunOnce itself
	// doesn't call it -- exercise that path directly, in a single pass, so
	// occupancy/capacity accounting is easy to reason about.
	s.runAndLog(ctx, logr.Discard())

	if got.Capacity != 2 {
		t.Errorf("Capacity = %d, want 2", got.Capacity)
	}
	if got.Occupancy != 1 {
		t.Errorf("Occupancy = %d, want 1 (env-occupied)", got.Occupancy)
	}
	if got.QueueDepth != 2 {
		t.Errorf("QueueDepth = %d, want 2 (env-a, env-b)", got.QueueDepth)
	}
	if got.Admitted != 1 {
		t.Errorf("Admitted = %d, want 1 (env-a: higher priority)", got.Admitted)
	}
	var waited time.Duration
	var found bool
	for _, g := range got.Grants {
		if g.Name == "env-a" {
			waited, found = g.Waited, true
		}
	}
	if !found {
		t.Fatalf("env-a not present in Grants: %+v", got.Grants)
	}
	if waited != 3*time.Minute {
		t.Errorf("env-a Waited = %v, want 3m (GrantedAt - QueuedSince)", waited)
	}
}
