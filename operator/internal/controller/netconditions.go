package controller

import (
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
)

// ConditionNetworkPosture reports the class's declared network isolation
// posture (#31): Restricted (a NetworkPolicy default-denies egress except for
// the resolved peers) or Open (no policy, unrestricted egress).
//
// Like ConditionEngineSecurity it is deliberately NOT a member of
// lifecycle.ConditionTypes: it is a pure function of the resolved
// SandboxClass, a RENDER-time fact, and Next must never reason about it.
// lifecycle.Apply writes conditions with apimeta.SetStatusCondition and never
// prunes unknown types, so appending it to the Decision's condition slice in
// Reconcile is safe and needs no second status write.
const ConditionNetworkPosture = "NetworkPosture"

const (
	ReasonPostureRestricted = "Restricted" // True: egress is restricted to declared peers
	ReasonPostureOpen       = "Open"       // False: egress is unrestricted
	ReasonPostureUnknown    = "Unknown"    // Unknown: class unresolved
)

// ConditionCNIEnforcement reports whether the cluster's CNI actually enforces
// NetworkPolicies (#31). True only after the CNIProbeRunnable has verified
// enforcement with a real connectivity test; False when the probe has run and
// enforcement could not be verified; Unknown until the probe has run once.
// Same lifecycle.ConditionTypes non-membership rationale as
// ConditionNetworkPosture.
const ConditionCNIEnforcement = "CNIEnforcement"

const (
	ReasonCNIEnforced    = "Enforced"    // True: the CNI enforces NetworkPolicies
	ReasonCNINotEnforced = "NotEnforced" // False: the CNI does not enforce NetworkPolicies
	ReasonCNIProbeFailed = "ProbeFailed" // probe result reason: the probe could not run to completion
	ReasonCNIUnconfirmed = "Unconfirmed" // Unknown: the probe has not run yet, or could not verify enforcement
)

// networkPostureCondition computes the NetworkPosture condition for env's
// resolved class. class may be nil (unresolved).
func networkPostureCondition(env *v1alpha1.SandboxEnvironment, class *v1alpha1.SandboxClass, now time.Time) metav1.Condition {
	base := metav1.Condition{
		Type:               ConditionNetworkPosture,
		ObservedGeneration: env.Generation,
		LastTransitionTime: metav1.NewTime(now).Rfc3339Copy(),
	}
	if class == nil {
		base.Status = metav1.ConditionUnknown
		base.Reason = ReasonPostureUnknown
		base.Message = "SandboxClass could not be resolved"
		return base
	}
	if class.Spec.Network.Isolation == v1alpha1.NetworkIsolationRestricted {
		base.Status = metav1.ConditionTrue
		base.Reason = ReasonPostureRestricted
		base.Message = "egress is restricted to the class's declared service, storage and extraEgress peers"
		return base
	}
	base.Status = metav1.ConditionFalse
	base.Reason = ReasonPostureOpen
	base.Message = "egress is unrestricted (no NetworkPolicy is rendered)"
	return base
}

// cniEnforcementCondition computes the CNIEnforcement condition from the
// latest probe result. cni may be nil (probe has not run yet).
//
// Routing is on cni.Reason, NOT on cni.Detail presence (Defect 1): a confirmed
// "not enforced" probe outcome carries Reason=ReasonCNINotEnforced and is
// reported as the loud, real status False; a transient probe failure
// (Reason=ReasonCNIProbeFailed) or a not-yet-run probe (nil) is reported as
// status Unknown -- we do not claim non-enforcement from an inability to probe.
func cniEnforcementCondition(env *v1alpha1.SandboxEnvironment, cni *CNIProbeResult, now time.Time) metav1.Condition {
	base := metav1.Condition{
		Type:               ConditionCNIEnforcement,
		ObservedGeneration: env.Generation,
		LastTransitionTime: metav1.NewTime(now).Rfc3339Copy(),
	}
	if cni == nil {
		base.Status = metav1.ConditionUnknown
		base.Reason = ReasonCNIUnconfirmed
		base.Message = "CNI enforcement has not been probed yet"
		return base
	}
	if cni.Enforced {
		base.Status = metav1.ConditionTrue
		base.Reason = ReasonCNIEnforced
		base.Message = "the CNI enforces NetworkPolicies"
		return base
	}
	if cni.Reason == ReasonCNINotEnforced {
		// The loud, real case: the probe ran and confirmed the CNI does NOT
		// enforce. Status False, not Unknown.
		base.Status = metav1.ConditionFalse
		base.Reason = ReasonCNINotEnforced
		base.Message = "the CNI does not enforce NetworkPolicies; Restricted isolation is not actually enforced"
		return base
	}
	// Reason == ReasonCNIProbeFailed || ReasonCNIUnconfirmed || "" -- the probe
	// could not verify enforcement (transient failure) or has not run. Status
	// Unknown: we don't know, so don't claim non-enforcement.
	base.Status = metav1.ConditionUnknown
	base.Reason = ReasonCNIUnconfirmed
	if cni.Detail != "" {
		base.Message = fmt.Sprintf("the CNI enforcement probe could not verify enforcement: %s", cni.Detail)
	} else {
		base.Message = "CNI enforcement has not been probed yet"
	}
	return base
}

// warnIfNetworkNotEnforced emits a Warning event on env when the class
// declares Restricted isolation but the CNI probe has not verified
// enforcement. The event recorder aggregates duplicate events, so emitting on
// every reconcile is not spammy. r.Recorder may be nil (unit tests); the
// call site guards.
func (r *Reconciler) warnIfNetworkNotEnforced(env *v1alpha1.SandboxEnvironment, class *v1alpha1.SandboxClass) {
	if r.Recorder == nil || class == nil {
		return
	}
	if class.Spec.Network.Isolation != v1alpha1.NetworkIsolationRestricted {
		return
	}
	// Fire the warning unless the probe has confirmed enforcement. Pre-probe
	// (nil), confirmed-not-enforced, and transient probe failure all warn --
	// in every one of those cases Restricted isolation is not actually being
	// enforced.
	cni := r.cniResult()
	if cni != nil && cni.Enforced {
		return
	}
	r.Recorder.Event(env, corev1.EventTypeWarning, "NetworkPolicyNotEnforced",
		"network isolation is Restricted but the CNI has not verified NetworkPolicy enforcement; egress may not actually be restricted")
}
