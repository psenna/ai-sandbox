package scheduler

import (
	"math/rand"
	"reflect"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
)

var fixedStart = time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)

// mkEnv builds a minimal candidate-shaped SandboxEnvironment for these
// tests: Ready phase, not suspended, not deleting, not yet granted.
// Callers override whichever fields the test cares about.
func mkEnv(name string, uid types.UID, priority int32, queuedSince *time.Time) v1alpha1.SandboxEnvironment {
	env := v1alpha1.SandboxEnvironment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			UID:       uid,
		},
		Spec: v1alpha1.SandboxEnvironmentSpec{Priority: priority},
		Status: v1alpha1.SandboxEnvironmentStatus{
			Phase: v1alpha1.PhaseReady,
		},
	}
	if queuedSince != nil {
		t := metav1.NewTime(*queuedSince)
		env.Status.QueuedSince = &t
	}
	return env
}

func TestAdmit_RespectsRemainingCapacity(t *testing.T) {
	sixCandidates := func() []v1alpha1.SandboxEnvironment {
		var out []v1alpha1.SandboxEnvironment
		for i := 0; i < 6; i++ {
			out = append(out, mkEnv(string(rune('a'+i)), types.UID(string(rune('a'+i))), int32(i), nil))
		}
		return out
	}

	cases := []struct {
		name       string
		candidates []v1alpha1.SandboxEnvironment
		occupancy  int
		capacity   int
		wantLen    int
	}{
		{"cap4 occ0 6cands -> 4", sixCandidates(), 0, 4, 4},
		{"cap4 occ4 6cands -> 0", sixCandidates(), 4, 4, 0},
		{"cap4 occ6 2cands -> 0 (lowered below occupancy)", sixCandidates()[:2], 6, 4, 0},
		{"cap0 -> 0", sixCandidates(), 0, 0, 0},
		{"cap4 0cands -> nil", nil, 0, 4, 0},
		{"cap10 3cands -> 3", sixCandidates()[:3], 0, 10, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			grants := Admit(tc.candidates, tc.occupancy, tc.capacity)
			if len(grants) != tc.wantLen {
				t.Fatalf("Admit() len = %d, want %d (grants=%+v)", len(grants), tc.wantLen, grants)
			}
			if tc.wantLen == 0 && grants != nil {
				// Not a hard requirement, but exercise the "never negative,
				// never panics, nil-safe" contract explicitly.
				t.Errorf("Admit() = %+v, want nil for zero admissions", grants)
			}
		})
	}
}

func TestAdmit_OrdersByPriorityThenQueuedSinceThenUID(t *testing.T) {
	t1 := fixedStart
	t2 := fixedStart.Add(time.Minute)
	t3 := fixedStart.Add(2 * time.Minute)

	// Priorities span the full valid range from #17's CRD validation:
	// -1000, -1, 0, 7, 1000.
	high := mkEnv("high", "uid-high", 1000, &t2)
	mid1 := mkEnv("mid1", "uid-mid1", 7, &t1)
	mid2 := mkEnv("mid2", "uid-mid2", 0, &t3)
	low := mkEnv("low", "uid-low", -1, &t1)
	lowest := mkEnv("lowest", "uid-lowest", -1000, &t1)

	// Shuffled input order.
	in := []v1alpha1.SandboxEnvironment{mid2, low, high, lowest, mid1}
	grants := Admit(in, 0, 5)

	wantOrder := []string{"high", "mid1", "mid2", "low", "lowest"}
	if len(grants) != len(wantOrder) {
		t.Fatalf("len(grants) = %d, want %d", len(grants), len(wantOrder))
	}
	for i, want := range wantOrder {
		if grants[i].Name != want {
			t.Errorf("grants[%d].Name = %q, want %q", i, grants[i].Name, want)
		}
		if grants[i].Position != i+1 {
			t.Errorf("grants[%d].Position = %d, want %d", i, grants[i].Position, i+1)
		}
	}
}

func TestAdmit_NilQueuedSinceSortsLast(t *testing.T) {
	t1 := fixedStart
	withQueue := mkEnv("with-queue", "uid-a", 5, &t1)
	noQueue := mkEnv("no-queue", "uid-b", 5, nil)

	grants := Admit([]v1alpha1.SandboxEnvironment{noQueue, withQueue}, 0, 2)
	if len(grants) != 2 {
		t.Fatalf("len(grants) = %d, want 2", len(grants))
	}
	if grants[0].Name != "with-queue" || grants[1].Name != "no-queue" {
		t.Errorf("order = [%s, %s], want [with-queue, no-queue] (nil QueuedSince must sort last)", grants[0].Name, grants[1].Name)
	}
}

func TestAdmit_TieBreaksOnUID(t *testing.T) {
	t1 := fixedStart
	b := mkEnv("env-b", "uid-b", 5, &t1)
	a := mkEnv("env-a", "uid-a", 5, &t1)
	c := mkEnv("env-c", "uid-c", 5, &t1)

	grants := Admit([]v1alpha1.SandboxEnvironment{b, c, a}, 0, 3)
	if len(grants) != 3 {
		t.Fatalf("len(grants) = %d, want 3", len(grants))
	}
	wantOrder := []string{"env-a", "env-b", "env-c"}
	for i, want := range wantOrder {
		if grants[i].Name != want {
			t.Errorf("grants[%d].Name = %q, want %q", i, grants[i].Name, want)
		}
	}
}

func TestAdmit_DeterministicAcrossPermutations(t *testing.T) {
	t1 := fixedStart
	t2 := fixedStart.Add(time.Minute)
	base := []v1alpha1.SandboxEnvironment{
		mkEnv("e1", "uid-1", 10, &t1),
		mkEnv("e2", "uid-2", 10, &t2),
		mkEnv("e3", "uid-3", 5, &t1),
		mkEnv("e4", "uid-4", 5, nil),
		mkEnv("e5", "uid-5", -3, &t1),
	}

	var reference []Grant
	rng := rand.New(rand.NewSource(42)) //nolint:gosec // G404: deterministic test shuffling, not security-sensitive
	for p := 0; p < 8; p++ {
		perm := make([]v1alpha1.SandboxEnvironment, len(base))
		copy(perm, base)
		rng.Shuffle(len(perm), func(i, j int) { perm[i], perm[j] = perm[j], perm[i] })

		grants := Admit(perm, 0, len(perm))
		if reference == nil {
			reference = grants
			continue
		}
		if !reflect.DeepEqual(grants, reference) {
			t.Fatalf("permutation %d produced a different admission order:\n got:  %+v\n want: %+v", p, grants, reference)
		}
	}
}

func TestAdmit_DoesNotMutateInput(t *testing.T) {
	t1 := fixedStart
	in := []v1alpha1.SandboxEnvironment{
		mkEnv("z", "uid-z", 1, &t1),
		mkEnv("a", "uid-a", 5, &t1),
		mkEnv("m", "uid-m", 3, &t1),
	}
	snapshot := make([]v1alpha1.SandboxEnvironment, len(in))
	copy(snapshot, in)

	_ = Admit(in, 0, 3)
	_ = Order(in)
	_, _ = QueuePosition(in, "uid-a")

	if !reflect.DeepEqual(in, snapshot) {
		t.Errorf("input slice was mutated:\n got:  %+v\n want: %+v", in, snapshot)
	}
}

func allPhases() []v1alpha1.Phase {
	return []v1alpha1.Phase{
		v1alpha1.PhasePending, v1alpha1.PhaseReady, v1alpha1.PhaseRunning,
		v1alpha1.PhaseFreezing, v1alpha1.PhaseWaiting, v1alpha1.PhaseRestoring,
		v1alpha1.PhaseDone, v1alpha1.PhaseFailed,
	}
}

func TestOccupiesAndIsCandidate_Table(t *testing.T) {
	occupying := map[v1alpha1.Phase]bool{
		v1alpha1.PhaseRestoring: true,
		v1alpha1.PhaseRunning:   true,
		v1alpha1.PhaseFreezing:  true,
	}

	for _, phase := range allPhases() {
		for _, granted := range []bool{false, true} {
			name := string(phase) + "/granted=" + boolStr(granted)
			t.Run(name, func(t *testing.T) {
				env := mkEnv("e", "uid", 0, nil)
				env.Status.Phase = phase
				env.Status.Slot.Granted = granted

				wantOccupies := granted || occupying[phase]
				wantCandidate := !granted && phase == v1alpha1.PhaseReady

				if got := Occupies(&env); got != wantOccupies {
					t.Errorf("Occupies() = %v, want %v", got, wantOccupies)
				}
				if got := IsCandidate(&env); got != wantCandidate {
					t.Errorf("IsCandidate() = %v, want %v", got, wantCandidate)
				}
			})
		}
	}

	t.Run("suspended Ready is not a candidate", func(t *testing.T) {
		env := mkEnv("e", "uid", 0, nil)
		env.Spec.Suspend = true
		if IsCandidate(&env) {
			t.Error("IsCandidate() = true for a suspended Ready environment")
		}
		if Occupies(&env) {
			t.Error("Occupies() = true for a suspended, non-granted Ready environment")
		}
	})

	t.Run("deleting Ready is not a candidate", func(t *testing.T) {
		env := mkEnv("e", "uid", 0, nil)
		now := metav1.NewTime(fixedStart)
		env.DeletionTimestamp = &now
		if IsCandidate(&env) {
			t.Error("IsCandidate() = true for a deleting Ready environment")
		}
	})
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func TestOccupiesAndIsCandidate_AreMutuallyExclusive(t *testing.T) {
	now := metav1.NewTime(fixedStart)
	for _, phase := range allPhases() {
		for _, granted := range []bool{false, true} {
			for _, suspend := range []bool{false, true} {
				for _, deleting := range []bool{false, true} {
					env := mkEnv("e", "uid", 0, nil)
					env.Status.Phase = phase
					env.Status.Slot.Granted = granted
					env.Spec.Suspend = suspend
					if deleting {
						env.DeletionTimestamp = &now
						env.Finalizers = []string{"keep-around-for-test"}
					}
					if Occupies(&env) && IsCandidate(&env) {
						t.Errorf("phase=%s granted=%v suspend=%v deleting=%v: Occupies and IsCandidate both true", phase, granted, suspend, deleting)
					}
				}
			}
		}
	}
}

func TestPartition_CountsGrantedReadyAsOccupied(t *testing.T) {
	granted := mkEnv("granted-ready", "uid-granted", 0, nil)
	granted.Status.Slot.Granted = true // Ready phase, but already granted

	ordinary := mkEnv("ordinary-ready", "uid-ordinary", 0, nil)

	occupancy, candidates := Partition([]v1alpha1.SandboxEnvironment{granted, ordinary})

	if occupancy != 1 {
		t.Errorf("occupancy = %d, want 1", occupancy)
	}
	if len(candidates) != 1 || candidates[0].Name != "ordinary-ready" {
		t.Errorf("candidates = %+v, want exactly [ordinary-ready]", candidates)
	}
}

func TestQueuePosition(t *testing.T) {
	t1 := fixedStart
	t2 := fixedStart.Add(time.Minute)
	candidates := []v1alpha1.SandboxEnvironment{
		mkEnv("a", "uid-a", 10, &t1),
		mkEnv("b", "uid-b", 5, &t1),
		mkEnv("c", "uid-c", 5, &t2),
	}

	pos, depth := QueuePosition(candidates, "uid-b")
	if pos != 2 || depth != 3 {
		t.Errorf("QueuePosition(uid-b) = (%d, %d), want (2, 3)", pos, depth)
	}

	pos, depth = QueuePosition(candidates, "uid-missing")
	if pos != 0 || depth != 3 {
		t.Errorf("QueuePosition(uid-missing) = (%d, %d), want (0, 3)", pos, depth)
	}
}
