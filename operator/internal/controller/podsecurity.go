package controller

import (
	"fmt"
	"sort"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
	"github.com/psenna/ai-sandbox/operator/internal/render"
)

// ConditionEngineSecurity reports whether the selected engine weakens the
// pod's security baseline, and by exactly what.
//
// It is deliberately NOT a member of lifecycle.ConditionTypes. That slice is
// the Next-driven set: every member has a phase/facts-derived default in
// builder.go and is recomputed from ClusterFacts on every pass. This
// condition is neither -- it is a pure function of the resolved
// SandboxClass's engine type, a RENDER-time fact, and Next must never
// reason about it (a weakened posture is not a reason to hold, advance or
// fail a phase). lifecycle.Apply writes conditions with
// apimeta.SetStatusCondition and never prunes unknown types (see
// conflict_test.go's NetworkPolicyEnforced assertion), so appending this
// one to the Decision's condition slice in Reconcile is safe and needs no
// second status write.
const ConditionEngineSecurity = "EngineSecurityRelaxed"

const (
	ReasonNoRelaxation      = "NoRelaxation"            // False: engine relaxes nothing
	ReasonEngineRelaxed     = "EngineRelaxationApplied" // True: engine weakened the baseline
	ReasonEngineUnavailable = "EngineUnavailable"       // Unknown: class unresolved, or engine not implemented/unknown
)

// AllEngineSecurityReasons lists every reason string this file can put on
// the EngineSecurityRelaxed condition. Same "declared list of every
// member" idiom as lifecycle.AllReasons and AllNetworkConditionReasons;
// internal/docs's reasons_test.go enforces both halves of that contract.
var AllEngineSecurityReasons = []string{ReasonNoRelaxation, ReasonEngineRelaxed, ReasonEngineUnavailable}

const maxRelaxationMessageBytes = 512

// engineSecurityCondition computes the EngineSecurityRelaxed condition for
// env's resolved class. class may be nil (unresolved).
func engineSecurityCondition(env *v1alpha1.SandboxEnvironment, class *v1alpha1.SandboxClass, now time.Time) metav1.Condition {
	base := metav1.Condition{
		Type:               ConditionEngineSecurity,
		ObservedGeneration: env.Generation,
		LastTransitionTime: metav1.NewTime(now).Rfc3339Copy(),
	}
	if class == nil {
		base.Status = metav1.ConditionUnknown
		base.Reason = ReasonEngineUnavailable
		base.Message = "SandboxClass could not be resolved"
		return base
	}
	relaxations, ok := render.EngineRelaxations(class.Spec.Engine.Type)
	if !ok {
		base.Status = metav1.ConditionUnknown
		base.Reason = ReasonEngineUnavailable
		base.Message = fmt.Sprintf("engine %q is not implemented yet; its security posture is not yet known", class.Spec.Engine.Type)
		return base
	}
	if len(relaxations) == 0 {
		base.Status = metav1.ConditionFalse
		base.Reason = ReasonNoRelaxation
		base.Message = fmt.Sprintf("engine %q requires no securityContext relaxation", class.Spec.Engine.Type)
		return base
	}
	base.Status = metav1.ConditionTrue
	base.Reason = ReasonEngineRelaxed
	base.Message = truncateMessage(formatRelaxations(relaxations), maxRelaxationMessageBytes)
	return base
}

// formatRelaxations renders relaxations deterministically: sorted by
// (Container, Kind), "; "-joined as "<container>: <kind> (<reason>)".
func formatRelaxations(relaxations []render.Relaxation) string {
	sorted := make([]render.Relaxation, len(relaxations))
	copy(sorted, relaxations)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Container != sorted[j].Container {
			return sorted[i].Container < sorted[j].Container
		}
		return sorted[i].Kind < sorted[j].Kind
	})

	parts := make([]string, 0, len(sorted))
	for _, r := range sorted {
		parts = append(parts, fmt.Sprintf("%s: %s (%s)", r.Container, r.Kind, r.Reason))
	}
	return strings.Join(parts, "; ")
}
