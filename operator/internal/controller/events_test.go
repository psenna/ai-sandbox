package controller

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	sandboxv1alpha1 "github.com/psenna/ai-sandbox/operator/api/v1alpha1"
	"github.com/psenna/ai-sandbox/operator/internal/lifecycle"
	"github.com/psenna/ai-sandbox/operator/internal/metrics"
)

// newTestEnv returns a minimal SandboxEnvironment for observeTransition's
// obj parameter -- only ObjectMeta matters (Eventf's "regarding"), never
// its Status (prev/next are passed separately).
func newTestEnv(name string) *sandboxv1alpha1.SandboxEnvironment {
	return &sandboxv1alpha1.SandboxEnvironment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", UID: types.UID("uid-" + name)},
	}
}

// condition is a tiny builder for the metav1.Condition slices prev/next
// carry, keeping each table case's fixture short.
func condition(condType string, status metav1.ConditionStatus, reason, message string) metav1.Condition {
	return metav1.Condition{Type: condType, Status: status, Reason: reason, Message: message}
}

// TestObserveTransition_EventTable drives observeTransition directly (no
// reconcile, no cluster) over every row of #33's Event table, asserting the
// captured Event's reason/type/action and a note substring, AND the paired
// metric side-effect the same transition edge is documented to record.
func TestObserveTransition_EventTable(t *testing.T) {
	t.Run("Starting: Ready->Restoring, no snapshot", func(t *testing.T) {
		r, capture, _ := newObserveTransitionFixture()
		prev := &sandboxv1alpha1.SandboxEnvironmentStatus{Phase: sandboxv1alpha1.PhaseReady}
		next := &sandboxv1alpha1.SandboxEnvironmentStatus{Phase: sandboxv1alpha1.PhaseRestoring}
		r.observeTransition(ctx, newTestEnv("e1"), prev, next)

		got := requireOneEvent(t, capture, ReasonStarting)
		assertEvent(t, got, corev1.EventTypeNormal, "Start", "cold start")
	})

	t.Run("Waking: Ready->Restoring, existing snapshot", func(t *testing.T) {
		r, capture, _ := newObserveTransitionFixture()
		prev := &sandboxv1alpha1.SandboxEnvironmentStatus{Phase: sandboxv1alpha1.PhaseReady}
		next := &sandboxv1alpha1.SandboxEnvironmentStatus{Phase: sandboxv1alpha1.PhaseRestoring, Snapshot: &sandboxv1alpha1.SnapshotStatus{Seq: 3}}
		r.observeTransition(ctx, newTestEnv("e1"), prev, next)

		got := requireOneEvent(t, capture, ReasonWaking)
		assertEvent(t, got, corev1.EventTypeNormal, "Wake", "seq 3")
	})

	t.Run("Started: Restoring->Running, records wake duration", func(t *testing.T) {
		r, capture, m := newObserveTransitionFixture()
		prev := &sandboxv1alpha1.SandboxEnvironmentStatus{Phase: sandboxv1alpha1.PhaseRestoring}
		next := &sandboxv1alpha1.SandboxEnvironmentStatus{
			Phase: sandboxv1alpha1.PhaseRunning, WakeCount: 2,
			Snapshot: &sandboxv1alpha1.SnapshotStatus{Seq: 1},
			RestoreAttempt: &sandboxv1alpha1.RestoreAttemptStatus{
				Seq: 1, Phase: sandboxv1alpha1.RestoreAttemptSucceeded, DurationMillis: 2500,
				Roots: []sandboxv1alpha1.RestoredRootStatus{{Name: "workspace", Source: "Warm"}},
			},
		}
		r.observeTransition(ctx, newTestEnv("e1"), prev, next)

		got := requireOneEvent(t, capture, ReasonStarted)
		assertEvent(t, got, corev1.EventTypeNormal, "Start", "wake #2")

		reg := newTestRegistry(t, m)
		if n := mustMetricValue(t, reg, "sandbox_operator_wake_duration_seconds", map[string]string{"source": metrics.WakeSourceWarm}); n != 1 {
			t.Errorf("wake_duration_seconds{source=warm} sample count = %v, want 1", n)
		}
	})

	t.Run("Freezing: wait declared", func(t *testing.T) {
		r, capture, _ := newObserveTransitionFixture()
		prev := &sandboxv1alpha1.SandboxEnvironmentStatus{Phase: sandboxv1alpha1.PhaseRunning}
		next := &sandboxv1alpha1.SandboxEnvironmentStatus{
			Phase: sandboxv1alpha1.PhaseFreezing, WaitFor: &sandboxv1alpha1.WaitForStatus{Type: sandboxv1alpha1.WaitTypeGitProxyCheck},
		}
		r.observeTransition(ctx, newTestEnv("e1"), prev, next)

		got := requireOneEvent(t, capture, ReasonFreezing)
		assertEvent(t, got, corev1.EventTypeNormal, "Freeze", "wait condition was declared")
	})

	t.Run("Freezing: archive detour", func(t *testing.T) {
		r, capture, _ := newObserveTransitionFixture()
		prev := &sandboxv1alpha1.SandboxEnvironmentStatus{Phase: sandboxv1alpha1.PhaseDone}
		now := metav1.Now()
		next := &sandboxv1alpha1.SandboxEnvironmentStatus{Phase: sandboxv1alpha1.PhaseFreezing, FinishedAt: &now}
		r.observeTransition(ctx, newTestEnv("e1"), prev, next)

		got := requireOneEvent(t, capture, ReasonFreezing)
		assertEvent(t, got, corev1.EventTypeNormal, "Freeze", "capturing final context")
	})

	t.Run("Frozen: Freezing->Waiting, records freeze duration and snapshot size", func(t *testing.T) {
		r, capture, m := newObserveTransitionFixture()
		prev := &sandboxv1alpha1.SandboxEnvironmentStatus{Phase: sandboxv1alpha1.PhaseFreezing}
		next := &sandboxv1alpha1.SandboxEnvironmentStatus{
			Phase:    sandboxv1alpha1.PhaseWaiting,
			Snapshot: &sandboxv1alpha1.SnapshotStatus{Seq: 1, SizeBytes: 2048, DurationMillis: 1500},
		}
		r.observeTransition(ctx, newTestEnv("e1"), prev, next)

		got := requireOneEvent(t, capture, ReasonFrozen)
		assertEvent(t, got, corev1.EventTypeNormal, "Freeze", "seq 1", "2048 bytes")

		reg := newTestRegistry(t, m)
		if n := mustMetricValue(t, reg, "sandbox_operator_freeze_duration_seconds", nil); n != 1 {
			t.Errorf("freeze_duration_seconds sample count = %v, want 1", n)
		}
		if n := mustMetricValue(t, reg, "sandbox_operator_snapshot_size_bytes", nil); n != 1 {
			t.Errorf("snapshot_size_bytes sample count = %v, want 1", n)
		}
	})

	t.Run("SnapshotFailed: fires once when the Frozen condition first becomes SnapshotFailed", func(t *testing.T) {
		r, capture, _ := newObserveTransitionFixture()
		prev := &sandboxv1alpha1.SandboxEnvironmentStatus{
			Phase:      sandboxv1alpha1.PhaseFreezing,
			Conditions: []metav1.Condition{condition(lifecycle.ConditionFrozen, metav1.ConditionFalse, lifecycle.ReasonSnapshotInProgress, "")},
		}
		next := &sandboxv1alpha1.SandboxEnvironmentStatus{
			Phase:      sandboxv1alpha1.PhaseFreezing,
			Conditions: []metav1.Condition{condition(lifecycle.ConditionFrozen, metav1.ConditionFalse, lifecycle.ReasonSnapshotFailed, "backend unreachable")},
		}
		r.observeTransition(ctx, newTestEnv("e1"), prev, next)
		got := requireOneEvent(t, capture, ReasonSnapshotFailed)
		assertEvent(t, got, corev1.EventTypeWarning, "Freeze", "backend unreachable")

		// A second pass with the SAME reason on both prev and next (the
		// self-loop nextFreezing performs while permanently stuck) must NOT
		// re-fire the event.
		r2, capture2, _ := newObserveTransitionFixture()
		samePrev := next
		r2.observeTransition(ctx, newTestEnv("e1"), samePrev, next)
		if got := capture2.ByReason(ReasonSnapshotFailed); len(got) != 0 {
			t.Errorf("SnapshotFailed re-fired on a self-loop pass with an unchanged reason: %+v", got)
		}
	})

	t.Run("WaitSatisfied: Waiting->Ready, probe satisfied", func(t *testing.T) {
		r, capture, _ := newObserveTransitionFixture()
		prev := &sandboxv1alpha1.SandboxEnvironmentStatus{
			Phase:   sandboxv1alpha1.PhaseWaiting,
			WaitFor: &sandboxv1alpha1.WaitForStatus{Type: sandboxv1alpha1.WaitTypeHTTPGet},
		}
		next := &sandboxv1alpha1.SandboxEnvironmentStatus{
			Phase:        sandboxv1alpha1.PhaseReady,
			ProbeAttempt: &sandboxv1alpha1.ProbeAttemptStatus{Type: sandboxv1alpha1.WaitTypeHTTPGet, Attempts: 5, Phase: sandboxv1alpha1.ProbeAttemptSatisfied},
			Conditions:   []metav1.Condition{condition(lifecycle.ConditionWaitSatisfied, metav1.ConditionTrue, lifecycle.ReasonProbeSatisfied, "")},
		}
		r.observeTransition(ctx, newTestEnv("e1"), prev, next)

		got := requireOneEvent(t, capture, ReasonWaitSatisfied)
		assertEvent(t, got, corev1.EventTypeNormal, "Wake", "HTTPGet", "5 evaluations")
	})

	t.Run("WaitSatisfied: does not fire for the suspend-originated Waiting->Ready path", func(t *testing.T) {
		r, capture, _ := newObserveTransitionFixture()
		prev := &sandboxv1alpha1.SandboxEnvironmentStatus{Phase: sandboxv1alpha1.PhaseWaiting}
		next := &sandboxv1alpha1.SandboxEnvironmentStatus{Phase: sandboxv1alpha1.PhaseReady} // no WaitSatisfied=True condition
		r.observeTransition(ctx, newTestEnv("e1"), prev, next)
		if got := capture.ByReason(ReasonWaitSatisfied); len(got) != 0 {
			t.Errorf("WaitSatisfied fired for a suspend-originated transition: %+v", got)
		}
	})

	// prev carries no finishedAt and next does, exactly as
	// lifecycle.terminalOutcome's set-once SetFinishedAt produces on the
	// completion write -- the guard in observeTransition reads prev, so a
	// fixture that stamped both would silently stop exercising this row.
	t.Run("Completed: *->Done", func(t *testing.T) {
		r, capture, _ := newObserveTransitionFixture()
		finished := metav1.Now()
		prev := &sandboxv1alpha1.SandboxEnvironmentStatus{Phase: sandboxv1alpha1.PhaseRunning}
		next := &sandboxv1alpha1.SandboxEnvironmentStatus{
			Phase:      sandboxv1alpha1.PhaseDone,
			FinishedAt: &finished,
			Conditions: []metav1.Condition{condition(lifecycle.ConditionReady, metav1.ConditionFalse, lifecycle.ReasonSucceeded, "")},
		}
		r.observeTransition(ctx, newTestEnv("e1"), prev, next)

		got := requireOneEvent(t, capture, ReasonCompleted)
		assertEvent(t, got, corev1.EventTypeNormal, "Complete", lifecycle.ReasonSucceeded)
	})

	t.Run("Failed: *->Failed", func(t *testing.T) {
		r, capture, _ := newObserveTransitionFixture()
		finished := metav1.Now()
		prev := &sandboxv1alpha1.SandboxEnvironmentStatus{Phase: sandboxv1alpha1.PhaseRunning}
		next := &sandboxv1alpha1.SandboxEnvironmentStatus{
			Phase:      sandboxv1alpha1.PhaseFailed,
			FinishedAt: &finished,
			Conditions: []metav1.Condition{condition(lifecycle.ConditionReady, metav1.ConditionFalse, lifecycle.ReasonPodFailed, "container exited 1")},
		}
		r.observeTransition(ctx, newTestEnv("e1"), prev, next)

		got := requireOneEvent(t, capture, ReasonFailed)
		assertEvent(t, got, corev1.EventTypeWarning, "Complete", lifecycle.ReasonPodFailed, "container exited 1")
	})

	// The #32 freeze detour makes a terminal phase reachable TWICE for one
	// run: Running -> Done (the completion) -> Freezing -> Waiting -> Done
	// (nextWaiting's FinishedAt guard returning to status.terminalPhase).
	// Only the first entry may emit Completed/Failed -- the detour return is
	// bookkeeping, not a second completion. status.finishedAt is what tells
	// them apart: nil on the completion write, already stamped on the
	// return.
	t.Run("Completed: does not re-fire on the freeze-detour return Waiting->Done", func(t *testing.T) {
		r, capture, _ := newObserveTransitionFixture()
		finished := metav1.Now()
		prev := &sandboxv1alpha1.SandboxEnvironmentStatus{
			Phase: sandboxv1alpha1.PhaseWaiting, FinishedAt: &finished,
			TerminalPhase: sandboxv1alpha1.PhaseDone,
			Conditions:    []metav1.Condition{condition(lifecycle.ConditionReady, metav1.ConditionFalse, lifecycle.ReasonSucceeded, "")},
		}
		next := &sandboxv1alpha1.SandboxEnvironmentStatus{
			Phase: sandboxv1alpha1.PhaseDone, FinishedAt: &finished,
			TerminalPhase: sandboxv1alpha1.PhaseDone,
			Conditions:    []metav1.Condition{condition(lifecycle.ConditionReady, metav1.ConditionFalse, lifecycle.ReasonSucceeded, "")},
		}
		r.observeTransition(ctx, newTestEnv("e1"), prev, next)
		if got := capture.ByReason(ReasonCompleted); len(got) != 0 {
			t.Errorf("Completed re-fired on the freeze-detour return (the run already completed once): %+v", got)
		}
	})

	t.Run("Failed: does not re-fire on the freeze-detour return Waiting->Failed", func(t *testing.T) {
		r, capture, _ := newObserveTransitionFixture()
		finished := metav1.Now()
		prev := &sandboxv1alpha1.SandboxEnvironmentStatus{
			Phase: sandboxv1alpha1.PhaseWaiting, FinishedAt: &finished,
			TerminalPhase: sandboxv1alpha1.PhaseFailed,
			Conditions:    []metav1.Condition{condition(lifecycle.ConditionReady, metav1.ConditionFalse, lifecycle.ReasonPodFailed, "container exited 1")},
		}
		next := &sandboxv1alpha1.SandboxEnvironmentStatus{
			Phase: sandboxv1alpha1.PhaseFailed, FinishedAt: &finished,
			TerminalPhase: sandboxv1alpha1.PhaseFailed,
			Conditions:    []metav1.Condition{condition(lifecycle.ConditionReady, metav1.ConditionFalse, lifecycle.ReasonPodFailed, "container exited 1")},
		}
		r.observeTransition(ctx, newTestEnv("e1"), prev, next)
		if got := capture.ByReason(ReasonFailed); len(got) != 0 {
			t.Errorf("Failed re-fired on the freeze-detour return (the run already failed once): %+v", got)
		}
	})

	t.Run("Archived: Archived condition becomes True, records archives_total succeeded", func(t *testing.T) {
		r, capture, m := newObserveTransitionFixture()
		prev := &sandboxv1alpha1.SandboxEnvironmentStatus{
			Phase:      sandboxv1alpha1.PhaseDone,
			Conditions: []metav1.Condition{condition(lifecycle.ConditionArchived, metav1.ConditionFalse, lifecycle.ReasonArchivePending, "")},
		}
		next := &sandboxv1alpha1.SandboxEnvironmentStatus{
			Phase:      sandboxv1alpha1.PhaseDone,
			ArchiveURI: "s3://bucket/prefix/archive",
			Conditions: []metav1.Condition{condition(lifecycle.ConditionArchived, metav1.ConditionTrue, lifecycle.ReasonArchiveWritten, "")},
		}
		r.observeTransition(ctx, newTestEnv("e1"), prev, next)

		got := requireOneEvent(t, capture, ReasonArchived)
		assertEvent(t, got, corev1.EventTypeNormal, "Archive", "s3://bucket/prefix/archive")

		reg := newTestRegistry(t, m)
		if n := mustMetricValue(t, reg, "sandbox_operator_archives_total", map[string]string{"result": metrics.ResultSucceeded}); n != 1 {
			t.Errorf("archives_total{result=succeeded} = %v, want 1", n)
		}
	})

	t.Run("Archived: does not re-fire once already True", func(t *testing.T) {
		r, capture, _ := newObserveTransitionFixture()
		already := []metav1.Condition{condition(lifecycle.ConditionArchived, metav1.ConditionTrue, lifecycle.ReasonArchiveWritten, "")}
		prev := &sandboxv1alpha1.SandboxEnvironmentStatus{Phase: sandboxv1alpha1.PhaseDone, Conditions: already}
		next := &sandboxv1alpha1.SandboxEnvironmentStatus{Phase: sandboxv1alpha1.PhaseDone, Conditions: already}
		r.observeTransition(ctx, newTestEnv("e1"), prev, next)
		if got := capture.ByReason(ReasonArchived); len(got) != 0 {
			t.Errorf("Archived re-fired though the condition was already True on prev: %+v", got)
		}
	})
}

// newObserveTransitionFixture builds a Reconciler wired to a fresh
// eventCapture and a private metrics.Collectors, for tests that drive
// observeTransition directly without a cluster.
func newObserveTransitionFixture() (*Reconciler, *eventCapture, *metrics.Collectors) {
	capture := newEventCapture()
	m := metrics.New()
	r := &Reconciler{Recorder: capture, Metrics: m}
	return r, capture, m
}

// requireOneEvent fails the test unless exactly one event with reason was
// captured, returning it.
func requireOneEvent(t *testing.T, capture *eventCapture, reason string) capturedEvent {
	t.Helper()
	got := capture.ByReason(reason)
	if len(got) != 1 {
		t.Fatalf("captured %d events with reason %s, want 1 (all events: %+v)", len(got), reason, capture.Events())
	}
	return got[0]
}

// assertEvent checks e's type/action and that every part appears somewhere
// in e.Note (substring, not exact match -- notes carry formatted durations/
// counts this test doesn't want to over-specify).
func assertEvent(t *testing.T, e capturedEvent, wantType, wantAction string, noteParts ...string) {
	t.Helper()
	if e.EventType != wantType {
		t.Errorf("EventType = %q, want %q", e.EventType, wantType)
	}
	if e.Action != wantAction {
		t.Errorf("Action = %q, want %q", e.Action, wantAction)
	}
	if e.Action == "" {
		t.Error("Action is empty -- every #33 Event call site must pass a non-empty action")
	}
	for _, part := range noteParts {
		if !strings.Contains(e.Note, part) {
			t.Errorf("Note = %q, want it to contain %q", e.Note, part)
		}
	}
}
