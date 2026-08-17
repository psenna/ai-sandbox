package lifecycle

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
)

// legalTransitions is INDEPENDENTLY restated here (not derived from next.go
// or decision.go) so this test can't become a tautology of the
// implementation it's checking.
var legalTransitions = map[v1alpha1.Phase]map[v1alpha1.Phase]bool{
	v1alpha1.PhasePending:   {v1alpha1.PhasePending: true, v1alpha1.PhaseReady: true, v1alpha1.PhaseFailed: true},
	v1alpha1.PhaseReady:     {v1alpha1.PhaseReady: true, v1alpha1.PhaseRestoring: true, v1alpha1.PhaseFailed: true},
	v1alpha1.PhaseRestoring: {v1alpha1.PhaseRestoring: true, v1alpha1.PhaseRunning: true, v1alpha1.PhaseReady: true, v1alpha1.PhaseDone: true, v1alpha1.PhaseFailed: true},
	v1alpha1.PhaseRunning:   {v1alpha1.PhaseRunning: true, v1alpha1.PhaseFreezing: true, v1alpha1.PhaseDone: true, v1alpha1.PhaseFailed: true},
	v1alpha1.PhaseFreezing:  {v1alpha1.PhaseFreezing: true, v1alpha1.PhaseWaiting: true, v1alpha1.PhaseFailed: true},
	v1alpha1.PhaseWaiting:   {v1alpha1.PhaseWaiting: true, v1alpha1.PhaseReady: true, v1alpha1.PhaseFailed: true},
	v1alpha1.PhaseDone:      {v1alpha1.PhaseDone: true},
	v1alpha1.PhaseFailed:    {v1alpha1.PhaseFailed: true},
}

var allPhases = []v1alpha1.Phase{
	v1alpha1.PhasePending, v1alpha1.PhaseReady, v1alpha1.PhaseRestoring, v1alpha1.PhaseRunning,
	v1alpha1.PhaseFreezing, v1alpha1.PhaseWaiting, v1alpha1.PhaseDone, v1alpha1.PhaseFailed,
}

// namedFacts is a corpus of ~40 meaningfully distinct ClusterFacts values.
func namedFacts() map[string]ClusterFacts {
	m := map[string]ClusterFacts{
		"zero value":                          {},
		"all observed, otherwise zero":        ClusterFacts{}.WithAllObserved(),
		"resources ready":                     withF(baseFacts(), func(f *ClusterFacts) { f.ResourcesReady = true }),
		"resources not ready":                 withF(baseFacts(), func(f *ClusterFacts) { f.ResourcesProblem = "pvc pending" }),
		"class unresolved":                    withF(baseFacts(), func(f *ClusterFacts) { f.ClassResolved = false }),
		"slot granted":                        withF(baseFacts(), func(f *ClusterFacts) { f.SlotGranted = true }),
		"slot not granted":                    baseFacts(),
		"pod not observed":                    withF(baseFacts(), func(f *ClusterFacts) { f.PodObserved = false }),
		"pod exists, pending":                 withF(baseFacts(), func(f *ClusterFacts) { f.PodExists = true; f.PodPhase = corev1.PodPending }),
		"pod running, ready":                  withF(baseFacts(), func(f *ClusterFacts) { f.PodExists = true; f.PodPhase = corev1.PodRunning; f.PodReady = true }),
		"pod running, not ready":              withF(baseFacts(), func(f *ClusterFacts) { f.PodExists = true; f.PodPhase = corev1.PodRunning }),
		"pod failed (phase)":                  withF(baseFacts(), func(f *ClusterFacts) { f.PodExists = true; f.PodPhase = corev1.PodFailed }),
		"pod succeeded (phase)":               withF(baseFacts(), func(f *ClusterFacts) { f.PodExists = true; f.PodPhase = corev1.PodSucceeded }),
		"pod failure unschedulable":           withF(baseFacts(), func(f *ClusterFacts) { f.PodFailure = &PodFailure{Reason: ReasonUnschedulable} }),
		"pod failure image pull":              withF(baseFacts(), func(f *ClusterFacts) { f.PodFailure = &PodFailure{Reason: ReasonImagePullFailure} }),
		"pod failure unknown reason":          withF(baseFacts(), func(f *ClusterFacts) { f.PodFailure = &PodFailure{Reason: "bogus"} }),
		"agent wait declared":                 withF(baseFacts(), func(f *ClusterFacts) { f.AgentWaitDeclared = true }),
		"agent done":                          withF(baseFacts(), func(f *ClusterFacts) { f.AgentDone = true }),
		"agent failed":                        withF(baseFacts(), func(f *ClusterFacts) { f.AgentFailed = true; f.AgentMessage = "boom" }),
		"snapshot complete":                   withF(baseFacts(), func(f *ClusterFacts) { f.SnapshotComplete = true }),
		"snapshot in progress":                baseFacts(),
		"snapshot failed":                     withF(baseFacts(), func(f *ClusterFacts) { f.SnapshotFailure = &StepFailure{Reason: ReasonSnapshotFailed} }),
		"snapshot complete, pod still exists": withF(baseFacts(), func(f *ClusterFacts) { f.SnapshotComplete = true; f.PodExists = true }),
		"probe not observed":                  withF(baseFacts(), func(f *ClusterFacts) { f.ProbeObserved = false }),
		"probe satisfied":                     withF(baseFacts(), func(f *ClusterFacts) { f.WaitProbeSatisfied = true }),
		"probe pending":                       baseFacts(),
		"probe failed":                        withF(baseFacts(), func(f *ClusterFacts) { f.WaitProbeFailure = &StepFailure{Reason: ReasonProbeFailed} }),
		"probe failed unknown reason":         withF(baseFacts(), func(f *ClusterFacts) { f.WaitProbeFailure = &StepFailure{Reason: "bogus"} }),
		"archive written":                     withF(baseFacts(), func(f *ClusterFacts) { f.ArchiveWritten = true; f.ArchiveURI = "s3://x" }),
		"archive not written":                 baseFacts(),
		"zero timeouts (disabled)":            ClusterFacts{}.WithAllObserved(),
		"all combined: slot+pod ready+resources": withF(baseFacts(), func(f *ClusterFacts) {
			f.SlotGranted = true
			f.ResourcesReady = true
			f.PodExists = true
			f.PodPhase = corev1.PodRunning
			f.PodReady = true
		}),
		"agent done AND pod failed (agent wins)": withF(baseFacts(), func(f *ClusterFacts) {
			f.AgentDone = true
			f.PodFailure = &PodFailure{Reason: ReasonPodFailed}
		}),
		"agent failed AND agent done (failed wins)": withF(baseFacts(), func(f *ClusterFacts) {
			f.AgentFailed = true
			f.AgentDone = true
		}),
		"resources not ready, class unresolved": withF(baseFacts(), func(f *ClusterFacts) {
			f.ClassResolved = false
			f.ResourcesReady = false
		}),
		"pod observed false, pod failure set (unobserved wins for PodReady)": withF(baseFacts(), func(f *ClusterFacts) {
			f.PodObserved = false
			f.PodFailure = &PodFailure{Reason: ReasonPodFailed}
		}),
		"snapshot failure AND snapshot complete (failure wins)": withF(baseFacts(), func(f *ClusterFacts) {
			f.SnapshotFailure = &StepFailure{Reason: ReasonSnapshotFailed}
			f.SnapshotComplete = true
		}),
		"wait probe satisfied AND failed (satisfied wins)": withF(baseFacts(), func(f *ClusterFacts) {
			f.WaitProbeSatisfied = true
			f.WaitProbeFailure = &StepFailure{Reason: ReasonProbeFailed}
		}),
		"resources problem message set": withF(baseFacts(), func(f *ClusterFacts) {
			f.ResourcesProblem = "PVC stuck pending"
		}),
		"class problem message set": withF(baseFacts(), func(f *ClusterFacts) {
			f.ClassResolved = false
			f.ClassProblem = "SandboxClass \"x\" not found"
		}),
	}
	return m
}

func withF(base ClusterFacts, mutate func(*ClusterFacts)) ClusterFacts {
	mutate(&base)
	return base
}

// TestNext_OnlyLegalTransitions crosses every named fact combination against
// every phase, {suspend true/false}, {waitFor nil/non-nil}, and asserts the
// resulting phase is always in the independently-declared legal set, and
// that every Decision has exactly the right condition shape.
func TestNext_OnlyLegalTransitions(t *testing.T) {
	facts := namedFacts()
	for factName, f := range facts {
		for _, phase := range allPhases {
			for _, suspend := range []bool{false, true} {
				for _, wf := range []*v1alpha1.WaitForStatus{nil, aWaitFor()} {
					opts := []envOption{withSuspend(suspend), withWaitFor(wf)}
					if phase == v1alpha1.PhaseRunning {
						opts = append(opts, withCondition(ConditionPodReady, metav1.ConditionTrue, ReasonPodRunning, fixedNow.Add(-30*time.Minute)))
					}
					env := envAt(phase, opts...)

					d := Next(env, f, fixedNow)

					legal := legalTransitions[phase]
					if legal == nil || !legal[d.Phase] {
						t.Errorf("facts=%q phase=%s suspend=%v waitFor=%v: illegal transition to %s",
							factName, phase, suspend, wf != nil, d.Phase)
					}
					if ok, detail := allConditionsPresent(d); !ok {
						t.Errorf("facts=%q phase=%s: %s", factName, phase, detail)
					}
				}
			}
		}
	}
}

// TestNext_TerminalIsSticky verifies Done/Failed never transition out,
// regardless of facts, and never want a slot or perform any action besides
// (conditionally) ActionArchive.
func TestNext_TerminalIsSticky(t *testing.T) {
	facts := namedFacts()
	for _, phase := range []v1alpha1.Phase{v1alpha1.PhaseDone, v1alpha1.PhaseFailed} {
		for factName, f := range facts {
			env := envAt(phase)
			d := Next(env, f, fixedNow)

			if d.Phase != phase {
				t.Errorf("facts=%q phase=%s: phase changed to %s", factName, phase, d.Phase)
			}
			if d.SlotWanted {
				t.Errorf("facts=%q phase=%s: SlotWanted=true in terminal phase", factName, phase)
			}
			for _, a := range d.Actions {
				if a != ActionArchive {
					t.Errorf("facts=%q phase=%s: unexpected action %s in terminal phase", factName, phase, a)
				}
			}
		}
	}
}
