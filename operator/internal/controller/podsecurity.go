package controller

import (
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
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
	ReasonNoRelaxation             = "NoRelaxation"                     // False: engine relaxes nothing
	ReasonEngineRelaxed            = "EngineRelaxationApplied"          // True: engine weakened the baseline
	ReasonEngineUnavailable        = "EngineUnavailable"                // Unknown: class unresolved, or engine not implemented/unknown
	ReasonNamespacePSSIncompatible = "NamespacePodSecurityIncompatible" // Unknown: the namespace's PSS level rejects this engine
)

// AllEngineSecurityReasons lists every reason string this file can put on
// the EngineSecurityRelaxed condition. Same "declared list of every
// member" idiom as lifecycle.AllReasons and AllNetworkConditionReasons;
// internal/docs's reasons_test.go enforces both halves of that contract.
var AllEngineSecurityReasons = []string{
	ReasonNoRelaxation, ReasonEngineRelaxed, ReasonEngineUnavailable,
	ReasonNamespacePSSIncompatible,
}

const maxRelaxationMessageBytes = 512

// engineSecurityCondition computes the EngineSecurityRelaxed condition for
// env's resolved class. class may be nil (unresolved). nsEnforce is the
// target namespace's pod-security.kubernetes.io/enforce label value,
// resolved by namespacePodSecurityEnforce (namespace.go).
func engineSecurityCondition(env *v1alpha1.SandboxEnvironment, class *v1alpha1.SandboxClass, nsEnforce string, now time.Time) metav1.Condition {
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

	// The engine WOULD weaken the baseline -- but if the namespace's Pod
	// Security Admission level rejects those exact fields, no pod is ever
	// admitted, so nothing is actually relaxed. Reporting True here would be
	// a lie ("the baseline IS weakened"); Unknown with a distinct, actionable
	// reason is the honest answer, and it reuses this condition's existing
	// Unknown branch rather than inventing a parallel condition type.
	if err := render.CheckNamespacePodSecurity(env.Namespace, class.Spec.Engine.Type, nsEnforce, relaxations); err != nil {
		base.Status = metav1.ConditionUnknown
		base.Reason = ReasonNamespacePSSIncompatible
		base.Message = truncateMessage(err.Error(), maxRelaxationMessageBytes)
		return base
	}

	base.Status = metav1.ConditionTrue
	base.Reason = ReasonEngineRelaxed
	base.Message = truncateMessage(formatRelaxations(relaxations), maxRelaxationMessageBytes)
	return base
}

// warnIfEngineNamespaceIncompatible emits a Warning Event when the class's
// engine cannot run in env's namespace under that namespace's Pod Security
// Admission level. A condition alone is not enough here: ensurePod SWALLOWS
// the render error at V(1) (internal/controller/pod.go), so without an Event
// the only signal in `kubectl describe`/`kubectl get events` would be a
// condition a first-time user has no reason to read. Both, per #24.
func (r *Reconciler) warnIfEngineNamespaceIncompatible(env *v1alpha1.SandboxEnvironment, class *v1alpha1.SandboxClass, nsEnforce string) {
	if r.Recorder == nil || class == nil {
		return
	}
	relaxations, ok := render.EngineRelaxations(class.Spec.Engine.Type)
	if !ok {
		return
	}
	err := render.CheckNamespacePodSecurity(env.Namespace, class.Spec.Engine.Type, nsEnforce, relaxations)
	if err == nil {
		return
	}
	r.Recorder.Eventf(env, nil, corev1.EventTypeWarning, "EngineNamespaceIncompatible", "", "%s", err.Error())
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
