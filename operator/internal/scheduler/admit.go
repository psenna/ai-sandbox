package scheduler

import (
	"sort"

	"k8s.io/apimachinery/pkg/types"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
)

// Grant identifies one environment the scheduler decided to admit this
// pass. It carries identity only -- the writer re-reads the object anyway
// -- and UID lets the writer verify it is granting the same object that
// was admitted, not a same-named object created since the List (e.g. after
// a delete+recreate).
type Grant struct {
	Namespace string
	Name      string
	UID       types.UID
	Position  int // 1-based position in this pass's admission queue
}

// Occupies reports whether env is holding compute and must count against
// capacity. A granted-but-still-Ready environment (the grant write landed,
// but the environment's own reconcile hasn't yet advanced it to Restoring)
// MUST count as occupying -- otherwise the very next scheduler pass would
// see occupancy 0 for that environment and over-admit a second one into the
// same slot before the first has even started moving.
func Occupies(env *v1alpha1.SandboxEnvironment) bool {
	if env.Status.Slot.Granted {
		return true
	}
	switch env.Status.Phase {
	case v1alpha1.PhaseRestoring, v1alpha1.PhaseRunning, v1alpha1.PhaseFreezing:
		return true
	default:
		return false
	}
}

// IsCandidate reports whether env is queued and admissible this pass.
func IsCandidate(env *v1alpha1.SandboxEnvironment) bool {
	return env.Status.Phase == v1alpha1.PhaseReady &&
		!env.Spec.Suspend &&
		env.DeletionTimestamp.IsZero() &&
		!env.Status.Slot.Granted
}

// Partition classifies one live snapshot into the occupancy count and the
// (unordered) candidate set. Single pass; never mutates items.
func Partition(items []v1alpha1.SandboxEnvironment) (occupancy int, candidates []v1alpha1.SandboxEnvironment) {
	for i := range items {
		env := &items[i]
		if Occupies(env) {
			occupancy++
		} else if IsCandidate(env) {
			candidates = append(candidates, *env)
		}
	}
	return occupancy, candidates
}

// Order returns a NEW slice holding candidates in admission order:
// priority descending, then queuedSince ascending (nil sorts LAST -- an
// environment with no observed queuedSince yet is treated as the newest
// arrival, so it cannot jump a legitimately-waiting queue), then UID
// ascending as a total-order tiebreak, then namespace, then name. Never
// mutates in.
func Order(in []v1alpha1.SandboxEnvironment) []v1alpha1.SandboxEnvironment {
	out := make([]v1alpha1.SandboxEnvironment, len(in))
	copy(out, in)
	sort.SliceStable(out, func(i, j int) bool {
		return less(&out[i], &out[j])
	})
	return out
}

// less implements the total order described on Order.
func less(a, b *v1alpha1.SandboxEnvironment) bool {
	if a.Spec.Priority != b.Spec.Priority {
		return a.Spec.Priority > b.Spec.Priority
	}
	aq, bq := a.Status.QueuedSince, b.Status.QueuedSince
	switch {
	case aq == nil && bq == nil:
		// fall through to UID
	case aq == nil:
		return false // nil sorts last
	case bq == nil:
		return true
	case !aq.Time.Equal(bq.Time):
		return aq.Time.Before(bq.Time)
	}
	if a.UID != b.UID {
		return a.UID < b.UID
	}
	if a.Namespace != b.Namespace {
		return a.Namespace < b.Namespace
	}
	return a.Name < b.Name
}

// Admit returns the grants to issue this pass, in admission order: the
// first max(0, capacity-occupancy) entries of Order(candidates). Never
// returns removals -- release is lifecycle.Apply's job. Handles every edge
// case without panicking: empty candidates, capacity <= 0, occupancy >=
// capacity (capacity was lowered below current occupancy), capacity larger
// than len(candidates).
func Admit(candidates []v1alpha1.SandboxEnvironment, occupancy, capacity int) []Grant {
	remaining := capacity - occupancy
	if remaining <= 0 || len(candidates) == 0 {
		return nil
	}
	ordered := Order(candidates)
	if remaining > len(ordered) {
		remaining = len(ordered)
	}
	grants := make([]Grant, 0, remaining)
	for i := 0; i < remaining; i++ {
		e := ordered[i]
		grants = append(grants, Grant{Namespace: e.Namespace, Name: e.Name, UID: e.UID, Position: i + 1})
	}
	return grants
}

// QueuePosition returns uid's 1-based position within candidates (in
// admission order) and the total queue depth, or (0, len(candidates)) when
// uid is not present.
func QueuePosition(candidates []v1alpha1.SandboxEnvironment, uid types.UID) (position, depth int) {
	ordered := Order(candidates)
	depth = len(ordered)
	for i, e := range ordered {
		if e.UID == uid {
			return i + 1, depth
		}
	}
	return 0, depth
}
