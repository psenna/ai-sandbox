package lifecycle

import (
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
)

type wantCond struct {
	status metav1.ConditionStatus
	reason string
}

type transitionCase struct {
	name           string
	env            v1alpha1.SandboxEnvironment
	facts          ClusterFacts
	wantPhase      v1alpha1.Phase
	wantSlotWanted bool
	wantActions    []Action
	wantConds      map[string]wantCond
}

func (tc transitionCase) run(t *testing.T) {
	t.Helper()
	d := Next(tc.env, tc.facts, fixedNow)

	if d.Phase != tc.wantPhase {
		t.Errorf("Phase = %q, want %q", d.Phase, tc.wantPhase)
	}
	if d.SlotWanted != tc.wantSlotWanted {
		t.Errorf("SlotWanted = %v, want %v", d.SlotWanted, tc.wantSlotWanted)
	}
	if ok, detail := allConditionsPresent(d); !ok {
		t.Errorf("condition shape: %s (got %d conditions)", detail, len(d.Conditions))
	}
	if !reflect.DeepEqual(d.Actions, tc.wantActions) && (len(d.Actions) != 0 || len(tc.wantActions) != 0) {
		t.Errorf("Actions = %v, want %v", d.Actions, tc.wantActions)
	}
	for condType, want := range tc.wantConds {
		c := findCond(d, condType)
		if c == nil {
			t.Errorf("condition %s missing", condType)
			continue
		}
		if c.Status != want.status || c.Reason != want.reason {
			t.Errorf("condition %s = %s/%s, want %s/%s", condType, c.Status, c.Reason, want.status, want.reason)
		}
	}
}

func TestNext_Transitions(t *testing.T) {
	cases := []transitionCase{
		// --- Pending ---
		{
			name:           "P1 resources ready -> Ready",
			env:            envAt(v1alpha1.PhasePending),
			facts:          func() ClusterFacts { f := baseFacts(); f.ResourcesReady = true; return f }(),
			wantPhase:      v1alpha1.PhaseReady,
			wantSlotWanted: false,
			wantActions:    []Action{ActionEnsureResources},
			wantConds:      map[string]wantCond{ConditionScheduled: {metav1.ConditionFalse, ReasonQueued}},
		},
		{
			name:           "P2 resources not ready -> stays Pending",
			env:            envAt(v1alpha1.PhasePending),
			facts:          baseFacts(),
			wantPhase:      v1alpha1.PhasePending,
			wantSlotWanted: false,
			wantActions:    []Action{ActionEnsureResources},
			wantConds:      map[string]wantCond{ConditionScheduled: {metav1.ConditionFalse, ReasonResourcesNotReady}},
		},
		{
			name:           "Pending suspended still needs resources first",
			env:            envAt(v1alpha1.PhasePending, withSuspend(true)),
			facts:          baseFacts(),
			wantPhase:      v1alpha1.PhasePending,
			wantSlotWanted: false,
			wantActions:    []Action{ActionEnsureResources},
			wantConds:      map[string]wantCond{ConditionReady: {metav1.ConditionFalse, ReasonSuspended}},
		},

		// --- Ready ---
		{
			name:           "R1 suspended self-loop",
			env:            envAt(v1alpha1.PhaseReady, withSuspend(true)),
			facts:          baseFacts(),
			wantPhase:      v1alpha1.PhaseReady,
			wantSlotWanted: false,
			wantActions:    []Action{ActionEnsureResources},
			wantConds:      map[string]wantCond{ConditionReady: {metav1.ConditionFalse, ReasonSuspended}},
		},
		{
			name:           "R2 slot granted -> Restoring",
			env:            envAt(v1alpha1.PhaseReady),
			facts:          func() ClusterFacts { f := baseFacts(); f.SlotGranted = true; return f }(),
			wantPhase:      v1alpha1.PhaseRestoring,
			wantSlotWanted: true,
			wantActions:    []Action{ActionEnsureResources, ActionEnsurePod},
		},
		{
			name:           "R3 queued, no slot",
			env:            envAt(v1alpha1.PhaseReady),
			facts:          baseFacts(),
			wantPhase:      v1alpha1.PhaseReady,
			wantSlotWanted: true,
			wantActions:    []Action{ActionEnsureResources},
			wantConds:      map[string]wantCond{ConditionScheduled: {metav1.ConditionFalse, ReasonQueued}},
		},

		// --- Restoring ---
		{
			name:           "S1 suspend cancels restore",
			env:            envAt(v1alpha1.PhaseRestoring, withSuspend(true)),
			facts:          func() ClusterFacts { f := baseFacts(); f.SlotGranted = true; return f }(),
			wantPhase:      v1alpha1.PhaseReady,
			wantSlotWanted: false,
			wantActions:    []Action{ActionEnsureResources, ActionDeletePod},
		},
		{
			name:           "S2 slot revoked mid-restore",
			env:            envAt(v1alpha1.PhaseRestoring),
			facts:          baseFacts(),
			wantPhase:      v1alpha1.PhaseReady,
			wantSlotWanted: true,
			wantActions:    []Action{ActionEnsureResources, ActionDeletePod},
		},
		{
			name: "S3 pod not observed self-loops",
			env:  envAt(v1alpha1.PhaseRestoring),
			facts: func() ClusterFacts {
				f := baseFacts()
				f.SlotGranted = true
				f.PodObserved = false
				return f
			}(),
			wantPhase:      v1alpha1.PhaseRestoring,
			wantSlotWanted: true,
			wantActions:    []Action{ActionEnsureResources, ActionEnsurePod},
			wantConds:      map[string]wantCond{ConditionPodReady: {metav1.ConditionUnknown, ReasonPodNotObserved}},
		},
		{
			name: "S4 pod running and ready -> Running, fresh restore has no wake",
			env:  envAt(v1alpha1.PhaseRestoring),
			facts: func() ClusterFacts {
				f := baseFacts()
				f.SlotGranted = true
				f.PodExists = true
				f.PodPhase = corev1.PodRunning
				f.PodReady = true
				return f
			}(),
			wantPhase:      v1alpha1.PhaseRunning,
			wantSlotWanted: true,
			wantActions:    []Action{ActionEnsureResources},
			wantConds:      map[string]wantCond{ConditionPodReady: {metav1.ConditionTrue, ReasonPodRunning}},
		},
		{
			name: "S5 pod exists but not ready yet",
			env:  envAt(v1alpha1.PhaseRestoring),
			facts: func() ClusterFacts {
				f := baseFacts()
				f.SlotGranted = true
				f.PodExists = true
				f.PodPhase = corev1.PodPending
				return f
			}(),
			wantPhase:      v1alpha1.PhaseRestoring,
			wantSlotWanted: true,
			wantActions:    []Action{ActionEnsureResources, ActionEnsurePod},
			wantConds:      map[string]wantCond{ConditionPodReady: {metav1.ConditionFalse, ReasonPodPending}},
		},
		{
			name: "S5 pod not created yet",
			env:  envAt(v1alpha1.PhaseRestoring),
			facts: func() ClusterFacts {
				f := baseFacts()
				f.SlotGranted = true
				return f
			}(),
			wantPhase:      v1alpha1.PhaseRestoring,
			wantSlotWanted: true,
			wantActions:    []Action{ActionEnsureResources, ActionEnsurePod},
			wantConds:      map[string]wantCond{ConditionPodReady: {metav1.ConditionFalse, ReasonPodNotCreated}},
		},

		// --- Running ---
		{
			name: "N1 wait declared beats suspend",
			env:  envAt(v1alpha1.PhaseRunning, withSuspend(true)),
			facts: func() ClusterFacts {
				f := baseFacts()
				f.AgentWaitDeclared = true
				return f
			}(),
			wantPhase:      v1alpha1.PhaseFreezing,
			wantSlotWanted: true,
			wantActions:    []Action{ActionEnsureResources, ActionFreezePod},
		},
		{
			name:           "N2 suspend triggers freeze",
			env:            envAt(v1alpha1.PhaseRunning, withSuspend(true)),
			facts:          baseFacts(),
			wantPhase:      v1alpha1.PhaseFreezing,
			wantSlotWanted: true,
			wantActions:    []Action{ActionEnsureResources, ActionFreezePod},
		},
		{
			name: "N3 pod not observed keeps Ready True",
			env:  envAt(v1alpha1.PhaseRunning),
			facts: func() ClusterFacts {
				f := baseFacts()
				f.PodObserved = false
				return f
			}(),
			wantPhase:      v1alpha1.PhaseRunning,
			wantSlotWanted: true,
			wantActions:    []Action{ActionEnsureResources},
			wantConds: map[string]wantCond{
				ConditionPodReady: {metav1.ConditionUnknown, ReasonPodNotObserved},
				ConditionReady:    {metav1.ConditionTrue, ReasonRunning},
			},
		},
		{
			name:           "N4 steady-state running",
			env:            envAt(v1alpha1.PhaseRunning),
			facts:          baseFacts(),
			wantPhase:      v1alpha1.PhaseRunning,
			wantSlotWanted: true,
			wantActions:    []Action{ActionEnsureResources},
			wantConds: map[string]wantCond{
				ConditionPodReady: {metav1.ConditionTrue, ReasonPodRunning},
				ConditionReady:    {metav1.ConditionTrue, ReasonRunning},
			},
		},

		// --- Freezing ---
		{
			name: "F1 snapshot failure holds, never deletes pod",
			env:  envAt(v1alpha1.PhaseFreezing),
			facts: func() ClusterFacts {
				f := baseFacts()
				f.SnapshotFailure = &StepFailure{Reason: ReasonSnapshotFailed, Message: "disk full"}
				f.PodExists = true
				return f
			}(),
			wantPhase:      v1alpha1.PhaseFreezing,
			wantSlotWanted: true,
			wantActions:    []Action{ActionEnsureResources},
			wantConds:      map[string]wantCond{ConditionFrozen: {metav1.ConditionFalse, ReasonSnapshotFailed}},
		},
		{
			name:           "F2 snapshot in progress",
			env:            envAt(v1alpha1.PhaseFreezing),
			facts:          baseFacts(),
			wantPhase:      v1alpha1.PhaseFreezing,
			wantSlotWanted: true,
			wantActions:    []Action{ActionEnsureResources, ActionFreezePod},
			wantConds:      map[string]wantCond{ConditionFrozen: {metav1.ConditionFalse, ReasonSnapshotInProgress}},
		},
		{
			name: "F3 snapshot complete, pod still around",
			env:  envAt(v1alpha1.PhaseFreezing),
			facts: func() ClusterFacts {
				f := baseFacts()
				f.SnapshotComplete = true
				f.PodExists = true
				return f
			}(),
			wantPhase:      v1alpha1.PhaseFreezing,
			wantSlotWanted: true,
			wantActions:    []Action{ActionEnsureResources, ActionDeletePod},
			wantConds:      map[string]wantCond{ConditionFrozen: {metav1.ConditionFalse, ReasonPodTerminating}},
		},
		{
			name: "F4 snapshot complete, pod gone, wait declared -> Waiting",
			env:  envAt(v1alpha1.PhaseFreezing, withWaitFor(aWaitFor())),
			facts: func() ClusterFacts {
				f := baseFacts()
				f.SnapshotComplete = true
				return f
			}(),
			wantPhase:      v1alpha1.PhaseWaiting,
			wantSlotWanted: false,
			wantActions:    []Action{ActionEnsureResources},
			wantConds:      map[string]wantCond{ConditionFrozen: {metav1.ConditionTrue, ReasonWaitDeclared}},
		},
		{
			name: "F4 snapshot complete, pod gone, suspend-originated -> Waiting",
			env:  envAt(v1alpha1.PhaseFreezing, withSuspend(true)),
			facts: func() ClusterFacts {
				f := baseFacts()
				f.SnapshotComplete = true
				return f
			}(),
			wantPhase:      v1alpha1.PhaseWaiting,
			wantSlotWanted: false,
			wantActions:    []Action{ActionEnsureResources},
			wantConds:      map[string]wantCond{ConditionFrozen: {metav1.ConditionTrue, ReasonSuspended}},
		},

		// --- Waiting ---
		{
			name: "W1 probe failure fails the environment before suspend is checked",
			env:  envAt(v1alpha1.PhaseWaiting, withWaitFor(aWaitFor()), withSuspend(true)),
			facts: func() ClusterFacts {
				f := baseFacts()
				f.WaitProbeFailure = &StepFailure{Reason: ReasonProbeFailed, Message: "bad url"}
				return f
			}(),
			wantPhase:      v1alpha1.PhaseFailed,
			wantSlotWanted: false,
			wantConds:      map[string]wantCond{ConditionReady: {metav1.ConditionFalse, ReasonProbeFailed}},
		},
		{
			name:           "W2 suspended self-loop",
			env:            envAt(v1alpha1.PhaseWaiting, withWaitFor(aWaitFor()), withSuspend(true)),
			facts:          baseFacts(),
			wantPhase:      v1alpha1.PhaseWaiting,
			wantSlotWanted: false,
			wantActions:    []Action{ActionEnsureResources},
			wantConds:      map[string]wantCond{ConditionReady: {metav1.ConditionFalse, ReasonSuspended}},
		},
		{
			name:           "W3 suspend cleared, no waitFor -> Ready",
			env:            envAt(v1alpha1.PhaseWaiting),
			facts:          baseFacts(),
			wantPhase:      v1alpha1.PhaseReady,
			wantSlotWanted: true,
			wantActions:    []Action{ActionEnsureResources},
		},
		{
			name: "W4 probe not observed",
			env:  envAt(v1alpha1.PhaseWaiting, withWaitFor(aWaitFor())),
			facts: func() ClusterFacts {
				f := baseFacts()
				f.ProbeObserved = false
				return f
			}(),
			wantPhase:      v1alpha1.PhaseWaiting,
			wantSlotWanted: false,
			wantActions:    []Action{ActionEnsureResources},
			wantConds:      map[string]wantCond{ConditionWaitSatisfied: {metav1.ConditionUnknown, ReasonProbeNotEvaluated}},
		},
		{
			name: "W5 probe satisfied -> Ready, clears waitFor",
			env:  envAt(v1alpha1.PhaseWaiting, withWaitFor(aWaitFor())),
			facts: func() ClusterFacts {
				f := baseFacts()
				f.WaitProbeSatisfied = true
				return f
			}(),
			wantPhase:      v1alpha1.PhaseReady,
			wantSlotWanted: true,
			wantActions:    []Action{ActionEnsureResources},
			wantConds:      map[string]wantCond{ConditionWaitSatisfied: {metav1.ConditionTrue, ReasonProbeSatisfied}},
		},
		{
			name:           "W6 probe pending",
			env:            envAt(v1alpha1.PhaseWaiting, withWaitFor(aWaitFor())),
			facts:          baseFacts(),
			wantPhase:      v1alpha1.PhaseWaiting,
			wantSlotWanted: false,
			wantActions:    []Action{ActionEnsureResources},
			wantConds:      map[string]wantCond{ConditionWaitSatisfied: {metav1.ConditionFalse, ReasonProbePending}},
		},

		// --- agent/pod terminal (Restoring/Running) ---
		{
			name: "agent reports failure while Running",
			env:  envAt(v1alpha1.PhaseRunning),
			facts: func() ClusterFacts {
				f := baseFacts()
				f.AgentFailed = true
				f.AgentMessage = "task failed"
				return f
			}(),
			wantPhase:      v1alpha1.PhaseFailed,
			wantSlotWanted: false,
			wantConds:      map[string]wantCond{ConditionReady: {metav1.ConditionFalse, ReasonAgentReportedFailure}},
		},
		{
			name: "agent reports success while Running",
			env:  envAt(v1alpha1.PhaseRunning),
			facts: func() ClusterFacts {
				f := baseFacts()
				f.AgentDone = true
				return f
			}(),
			wantPhase:      v1alpha1.PhaseDone,
			wantSlotWanted: false,
			wantActions:    []Action{ActionDeletePod},
		},
		{
			name: "pod failure reason surfaces sanitized",
			env:  envAt(v1alpha1.PhaseRunning),
			facts: func() ClusterFacts {
				f := baseFacts()
				f.PodFailure = &PodFailure{Reason: ReasonUnschedulable, Message: "insufficient cpu"}
				return f
			}(),
			wantPhase:      v1alpha1.PhaseFailed,
			wantSlotWanted: false,
			wantConds: map[string]wantCond{
				ConditionPodReady: {metav1.ConditionFalse, ReasonUnschedulable},
				ConditionReady:    {metav1.ConditionFalse, ReasonUnschedulable},
			},
		},
		{
			name: "pod failure reason not in allowlist falls back to PodFailed",
			env:  envAt(v1alpha1.PhaseRunning),
			facts: func() ClusterFacts {
				f := baseFacts()
				f.PodFailure = &PodFailure{Reason: "SomethingMadeUp", Message: "?"}
				return f
			}(),
			wantPhase:      v1alpha1.PhaseFailed,
			wantSlotWanted: false,
			wantConds: map[string]wantCond{
				ConditionPodReady: {metav1.ConditionFalse, ReasonPodFailed},
				ConditionReady:    {metav1.ConditionFalse, ReasonPodFailed},
			},
		},
		{
			name: "pod phase Failed with no PodFailure struct",
			env:  envAt(v1alpha1.PhaseRunning),
			facts: func() ClusterFacts {
				f := baseFacts()
				f.PodPhase = corev1.PodFailed
				return f
			}(),
			wantPhase:      v1alpha1.PhaseFailed,
			wantSlotWanted: false,
			wantConds:      map[string]wantCond{ConditionReady: {metav1.ConditionFalse, ReasonPodFailed}},
		},
		{
			name: "pod phase Succeeded -> Done",
			env:  envAt(v1alpha1.PhaseRunning),
			facts: func() ClusterFacts {
				f := baseFacts()
				f.PodPhase = corev1.PodSucceeded
				return f
			}(),
			wantPhase:      v1alpha1.PhaseDone,
			wantSlotWanted: false,
			wantActions:    []Action{ActionDeletePod},
		},
		{
			name: "agent failure while Restoring",
			env:  envAt(v1alpha1.PhaseRestoring),
			facts: func() ClusterFacts {
				f := baseFacts()
				f.SlotGranted = true
				f.AgentFailed = true
				return f
			}(),
			wantPhase:      v1alpha1.PhaseFailed,
			wantSlotWanted: false,
		},

		// --- class unresolved ---
		{
			name: "unresolved class holds Pending",
			env:  envAt(v1alpha1.PhasePending),
			facts: func() ClusterFacts {
				f := baseFacts()
				f.ClassResolved = false
				f.ClassProblem = "SandboxClass not found"
				return f
			}(),
			wantPhase:      v1alpha1.PhasePending,
			wantSlotWanted: false,
			wantConds:      map[string]wantCond{ConditionReady: {metav1.ConditionUnknown, ReasonClassNotResolved}},
		},
		{
			name: "unresolved class holds Running, keeps existing slot",
			env:  envAt(v1alpha1.PhaseRunning, withSlotGranted(true)),
			facts: func() ClusterFacts {
				f := baseFacts()
				f.ClassResolved = false
				return f
			}(),
			wantPhase:      v1alpha1.PhaseRunning,
			wantSlotWanted: true,
			wantConds:      map[string]wantCond{ConditionReady: {metav1.ConditionUnknown, ReasonClassNotResolved}},
		},

		// --- Done / Failed stickiness ---
		{
			name:           "Done stays Done, archives",
			env:            envAt(v1alpha1.PhaseDone),
			facts:          baseFacts(),
			wantPhase:      v1alpha1.PhaseDone,
			wantSlotWanted: false,
			wantActions:    []Action{ActionArchive},
		},
		{
			name: "Done with archive already written issues no action",
			env:  envAt(v1alpha1.PhaseDone),
			facts: func() ClusterFacts {
				f := baseFacts()
				f.ArchiveWritten = true
				return f
			}(),
			wantPhase:      v1alpha1.PhaseDone,
			wantSlotWanted: false,
			wantConds:      map[string]wantCond{ConditionArchived: {metav1.ConditionTrue, ReasonArchiveWritten}},
		},
		{
			name:           "Failed stays Failed, archives",
			env:            envAt(v1alpha1.PhaseFailed),
			facts:          baseFacts(),
			wantPhase:      v1alpha1.PhaseFailed,
			wantSlotWanted: false,
			wantActions:    []Action{ActionArchive},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, tc.run)
	}
}

// TestNext_Deterministic verifies Next has no hidden state: the same inputs
// twice yield byte-for-byte the same Decision.
func TestNext_Deterministic(t *testing.T) {
	env := envAt(v1alpha1.PhaseRunning, withWaitFor(nil))
	facts := baseFacts()

	d1 := Next(env, facts, fixedNow)
	d2 := Next(env, facts, fixedNow)

	if !reflect.DeepEqual(d1, d2) {
		t.Fatalf("Next is not deterministic:\n%#v\n%#v", d1, d2)
	}
}

// TestNext_WakeCountIncrementsOnlyWithPriorSnapshot covers the S4 branch's
// other half (next_test.go's table only exercises the fresh-restore, no-prior
// snapshot case): nextRestoring sets IncrementWakeCount based on whether
// env.Status.Snapshot is already populated, i.e. whether this Restoring
// entry followed a real freeze rather than the environment's first-ever
// start.
func TestNext_WakeCountIncrementsOnlyWithPriorSnapshot(t *testing.T) {
	restoringFacts := func() ClusterFacts {
		f := baseFacts()
		f.SlotGranted = true
		f.PodExists = true
		f.PodPhase = corev1.PodRunning
		f.PodReady = true
		return f
	}

	t.Run("no prior snapshot: fresh start, no wake increment", func(t *testing.T) {
		env := envAt(v1alpha1.PhaseRestoring)
		d := Next(env, restoringFacts(), fixedNow)
		if d.StatusPatch.IncrementWakeCount {
			t.Errorf("IncrementWakeCount = true, want false on a fresh (never-frozen) restore")
		}
	})

	t.Run("prior snapshot present: this is a real wake, increments", func(t *testing.T) {
		env := envAt(v1alpha1.PhaseRestoring, withSnapshot(aSnapshot()))
		d := Next(env, restoringFacts(), fixedNow)
		if !d.StatusPatch.IncrementWakeCount {
			t.Errorf("IncrementWakeCount = false, want true when Status.Snapshot is already populated")
		}
	})
}
