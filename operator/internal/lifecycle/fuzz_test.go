package lifecycle

import (
	"reflect"
	"regexp"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
)

// k8sReasonRE is the format Kubernetes expects of a condition Reason: a CamelCase
// identifier. Every reason Next ever emits must match this, including any
// externally-supplied PodFailure/StepFailure reason that survives
// SanitizeReason.
var k8sReasonRE = regexp.MustCompile(`^[A-Za-z]([A-Za-z0-9_,:]*[A-Za-z0-9_])?$`)

var fuzzPhases = []v1alpha1.Phase{
	v1alpha1.PhasePending, v1alpha1.PhaseReady, v1alpha1.PhaseRestoring, v1alpha1.PhaseRunning,
	v1alpha1.PhaseFreezing, v1alpha1.PhaseWaiting, v1alpha1.PhaseDone, v1alpha1.PhaseFailed,
}

// decodeFuzzInput turns fuzz bytes into a deterministic (phase, suspend,
// waitFor, ClusterFacts) tuple. Not exhaustive by design -- FuzzNext is a
// smoke test, not the primary correctness mechanism (that's
// next_test.go/legality_test.go).
func decodeFuzzInput(data []byte) (v1alpha1.Phase, bool, *v1alpha1.WaitForStatus, ClusterFacts) {
	b := func() byte {
		if len(data) == 0 {
			return 0
		}
		v := data[0]
		data = data[1:]
		return v
	}
	bit := func() bool { return b()&1 == 1 }

	phase := fuzzPhases[int(b())%len(fuzzPhases)]
	suspend := bit()
	var waitFor *v1alpha1.WaitForStatus
	if bit() {
		waitFor = aWaitFor()
	}

	f := baseFacts()
	f.ClassResolved = bit()
	f.ResourcesReady = bit()
	f.SlotGranted = bit()
	f.PodObserved = bit()
	f.PodExists = bit()
	f.PodReady = bit()
	if bit() {
		f.PodPhase = corev1.PodRunning
	}
	f.AgentWaitDeclared = bit()
	f.AgentDone = bit()
	f.AgentFailed = bit()
	f.SnapshotComplete = bit()
	f.ProbeObserved = bit()
	f.WaitProbeSatisfied = bit()
	f.ArchiveWritten = bit()
	if bit() {
		f.PodFailure = &PodFailure{Reason: ReasonPodFailed, Message: "fuzz"}
	}
	if bit() {
		f.SnapshotFailure = &StepFailure{Reason: ReasonSnapshotFailed, Message: "fuzz"}
	}
	if bit() {
		f.WaitProbeFailure = &StepFailure{Reason: ReasonProbeFailed, Message: "fuzz"}
	}
	// Vary the running/waiting timeouts occasionally so timeout evaluation
	// paths get exercised too, without ever hitting a negative duration.
	if bit() {
		f.Timeouts.Running = time.Duration(b()) * time.Minute
	}
	if bit() {
		f.Timeouts.Waiting = time.Duration(b()) * time.Minute
	}

	return phase, suspend, waitFor, f
}

func FuzzNext(f *testing.F) {
	seeds := [][]byte{
		{0, 0, 0},
		{1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1},
		{3, 1, 0, 1, 0, 1, 0, 1, 0, 1, 0, 1, 0},
		{4, 0, 1, 0, 1, 1, 1, 0, 0, 1, 0, 1, 0, 1, 1, 0},
		{6, 1, 1, 0, 0, 0, 1, 1, 1, 0, 0, 1},
		{7, 0, 0, 1, 1, 0, 1, 0, 1, 1, 0, 1, 0, 1},
		{2, 1, 0, 0, 1, 1, 0, 1, 0, 1, 0, 1, 0, 1},
		{5, 0, 1, 1, 0, 1, 0, 1, 0, 1, 0, 1, 0, 200, 50},
		{0, 1, 1},
		{7, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1},
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		phase, suspend, waitFor, facts := decodeFuzzInput(data)
		env := envAt(phase, withSuspend(suspend), withWaitFor(waitFor))

		d := Next(env, facts, fixedNow)

		if ok, detail := allConditionsPresent(d); !ok {
			t.Fatalf("phase=%s: %s (got %d conditions)", phase, detail, len(d.Conditions))
		}
		for _, c := range d.Conditions {
			if !k8sReasonRE.MatchString(c.Reason) {
				t.Fatalf("condition %s has invalid reason %q", c.Type, c.Reason)
			}
		}
		if phase == v1alpha1.PhaseDone || phase == v1alpha1.PhaseFailed {
			if d.Phase != phase {
				t.Fatalf("terminal phase %s transitioned to %s", phase, d.Phase)
			}
		}

		d2 := Next(env, facts, fixedNow)
		if !reflect.DeepEqual(d, d2) {
			t.Fatalf("Next is not deterministic for phase=%s:\n%#v\n%#v", phase, d, d2)
		}
	})
}
