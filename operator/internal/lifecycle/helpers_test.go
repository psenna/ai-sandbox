package lifecycle

import (
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
)

// fixedNow is the reference "current time" used throughout this package's
// tests. Tests that care about relative timing derive from it explicitly;
// nothing here ever calls time.Now().
var fixedNow = time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)

type envOption func(*v1alpha1.SandboxEnvironment)

// envAt builds a SandboxEnvironment in the given phase, with sane defaults
// (created one hour before fixedNow, generation 1, a resolvable class ref),
// customized by opts.
func envAt(phase v1alpha1.Phase, opts ...envOption) v1alpha1.SandboxEnvironment {
	env := v1alpha1.SandboxEnvironment{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "env-1",
			Namespace:         "default",
			Generation:        1,
			CreationTimestamp: metav1.NewTime(fixedNow.Add(-1 * time.Hour)),
		},
		Spec: v1alpha1.SandboxEnvironmentSpec{
			ClassRef: v1alpha1.ClassRef{Name: "default"},
			Repo:     "org/repo",
			Task:     v1alpha1.TaskSpec{Prompt: "do stuff"},
		},
		Status: v1alpha1.SandboxEnvironmentStatus{
			Phase: phase,
		},
	}
	for _, opt := range opts {
		opt(&env)
	}
	return env
}

func withSuspend(v bool) envOption {
	return func(e *v1alpha1.SandboxEnvironment) { e.Spec.Suspend = v }
}

func withWaitFor(w *v1alpha1.WaitForStatus) envOption {
	return func(e *v1alpha1.SandboxEnvironment) { e.Status.WaitFor = w }
}

func aWaitFor() *v1alpha1.WaitForStatus {
	return &v1alpha1.WaitForStatus{Type: "PullRequestMerged", Reason: "waiting on review"}
}

func withSnapshot(s *v1alpha1.SnapshotStatus) envOption {
	return func(e *v1alpha1.SandboxEnvironment) { e.Status.Snapshot = s }
}

func aSnapshot() *v1alpha1.SnapshotStatus {
	return &v1alpha1.SnapshotStatus{Seq: 1, URI: "s3://bucket/snap-1"}
}

func withCreationTime(t time.Time) envOption {
	return func(e *v1alpha1.SandboxEnvironment) { e.CreationTimestamp = metav1.NewTime(t) }
}

func withGeneration(g int64) envOption {
	return func(e *v1alpha1.SandboxEnvironment) { e.Generation = g }
}

func withSlotGranted(v bool) envOption {
	return func(e *v1alpha1.SandboxEnvironment) { e.Status.Slot.Granted = v }
}

func withStartedAt(t time.Time) envOption {
	return func(e *v1alpha1.SandboxEnvironment) { mt := metav1.NewTime(t); e.Status.StartedAt = &mt }
}

func withFinishedAt(t time.Time) envOption {
	return func(e *v1alpha1.SandboxEnvironment) { mt := metav1.NewTime(t); e.Status.FinishedAt = &mt }
}

func withTerminalPhase(p v1alpha1.Phase) envOption {
	return func(e *v1alpha1.SandboxEnvironment) { e.Status.TerminalPhase = p }
}

func withQueuedSince(t time.Time) envOption {
	return func(e *v1alpha1.SandboxEnvironment) { mt := metav1.NewTime(t); e.Status.QueuedSince = &mt }
}

func withFreezeCount(n int32) envOption {
	return func(e *v1alpha1.SandboxEnvironment) { e.Status.FreezeCount = n }
}

func withWakeCount(n int32) envOption {
	return func(e *v1alpha1.SandboxEnvironment) { e.Status.WakeCount = n }
}

// withCondition sets a condition directly (bypassing SetStatusCondition's
// LastTransitionTime preservation logic), for constructing fixtures whose
// PodReady/Frozen LastTransitionTime drives timeout clocks in tests.
func withCondition(condType string, status metav1.ConditionStatus, reason string, lastTransition time.Time) envOption {
	return func(e *v1alpha1.SandboxEnvironment) {
		apimeta.SetStatusCondition(&e.Status.Conditions, metav1.Condition{
			Type:               condType,
			Status:             status,
			Reason:             reason,
			Message:            "",
			LastTransitionTime: metav1.NewTime(lastTransition),
			ObservedGeneration: e.Generation,
		})
		// SetStatusCondition only honors the passed LastTransitionTime when
		// the condition doesn't already exist or its Status changes. Force
		// it explicitly afterward so fixtures can set an arbitrary time
		// regardless of call order.
		if c := apimeta.FindStatusCondition(e.Status.Conditions, condType); c != nil {
			c.LastTransitionTime = metav1.NewTime(lastTransition)
		}
	}
}

// allConditionsPresent asserts d.Conditions has exactly one entry per
// ConditionTypes, in that exact order.
func allConditionsPresent(d Decision) (ok bool, detail string) {
	if len(d.Conditions) != len(ConditionTypes) {
		return false, "wrong condition count"
	}
	for i, want := range ConditionTypes {
		if d.Conditions[i].Type != want {
			return false, "wrong condition order/type at index " + want
		}
	}
	return true, ""
}

// findCond returns the condition of the given type from d.Conditions.
func findCond(d Decision, condType string) *metav1.Condition {
	for i := range d.Conditions {
		if d.Conditions[i].Type == condType {
			return &d.Conditions[i]
		}
	}
	return nil
}

// baseFacts returns a ClusterFacts with default (non-default-Timeouts) test
// timeouts and every *Observed flag set, so tests opt INTO the "not
// observed" degenerate states explicitly rather than tripping over them by
// accident.
func baseFacts() ClusterFacts {
	return ClusterFacts{
		Timeouts: Timeouts{Running: 6 * time.Hour, Waiting: 24 * time.Hour, Total: 72 * time.Hour},
	}.WithAllObserved()
}
