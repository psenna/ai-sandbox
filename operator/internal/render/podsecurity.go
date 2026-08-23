package render

import (
	"fmt"
	"sort"
	"strings"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
)

// PodSecurityEnforceLabel is the Kubernetes Pod Security Admission label that
// declares which Pod Security Standard a namespace ENFORCES. It is the only
// signal available to a controller without reading the API server's own
// AdmissionConfiguration -- see CheckNamespacePodSecurity's doc comment for
// what that means for cluster-wide PSA defaults.
const PodSecurityEnforceLabel = "pod-security.kubernetes.io/enforce"

// Pod Security Standard levels, as they appear in PodSecurityEnforceLabel.
const (
	PodSecurityPrivileged = "privileged"
	PodSecurityBaseline   = "baseline"
	PodSecurityRestricted = "restricted"
)

// forbiddenAt reports the Pod Security Standard levels that reject relaxation
// r. Derived mechanically from the closed RelaxationKind enum so a future
// engine's relaxations are covered without touching this table's callers.
//
//	AppArmorUnconfined       -- baseline forbids an explicit Unconfined
//	                            AppArmor profile; restricted inherits that.
//	SeccompUnconfined        -- baseline forbids an explicit Unconfined
//	                            seccomp profile; restricted additionally
//	                            requires RuntimeDefault or Localhost.
//	AllowPrivilegeEscalation -- restricted only (baseline does not check it).
//	AddCapability            -- restricted allows adding NET_BIND_SERVICE
//	                            only. Baseline is in fact laxer (it allows
//	                            the whole default set: CHOWN, SETUID,
//	                            SETGID, SYS_CHROOT, ...), but this table
//	                            deliberately reports baseline as forbidding
//	                            anything beyond NET_BIND_SERVICE too: an
//	                            engine that needs an unusual capability
//	                            should fail loudly here rather than be
//	                            admitted at baseline on a rule this
//	                            operator does not model field by field.
//	                            Erring closed costs a clear error message;
//	                            erring open costs an opaque API-server
//	                            rejection. No engine uses this kind today.
func forbiddenAt(r Relaxation) []string {
	switch r.Kind {
	case RelaxAppArmorUnconfined:
		return []string{PodSecurityBaseline, PodSecurityRestricted}
	case RelaxSeccompUnconfined:
		return []string{PodSecurityBaseline, PodSecurityRestricted}
	case RelaxAllowPrivilegeEscalation:
		return []string{PodSecurityRestricted}
	case RelaxAddCapability:
		if r.Value == "NET_BIND_SERVICE" {
			return nil
		}
		return []string{PodSecurityBaseline, PodSecurityRestricted}
	default:
		return nil
	}
}

// PodSecurityIncompatibleError is the single source of truth for the
// "this engine cannot run in this namespace" wording. RenderPod returns it as
// a render error (the render-time guard #24 asks for), and
// internal/controller/podsecurity.go reuses its Error() VERBATIM as the
// EngineSecurityRelaxed condition message and the Warning Event note, so the
// three can never disagree.
type PodSecurityIncompatibleError struct {
	Namespace string
	Engine    v1alpha1.EngineType
	Enforce   string   // "baseline" | "restricted"
	Kinds     []string // sorted RelaxationKind strings this level forbids
}

func (e *PodSecurityIncompatibleError) Error() string {
	return fmt.Sprintf(
		"engine %q cannot run in namespace %q: the namespace enforces the %q Pod Security Standard (label %s=%s), which forbids the securityContext relaxations this engine requires on its %q container: %s. Label the namespace %s=%s, or set spec.engine.type: none on the SandboxClass.",
		e.Engine, e.Namespace, e.Enforce, PodSecurityEnforceLabel, e.Enforce,
		PodmanContainerName, strings.Join(e.Kinds, ", "),
		PodSecurityEnforceLabel, PodSecurityPrivileged)
}

// CheckNamespacePodSecurity is the render-time guard: it returns a
// *PodSecurityIncompatibleError when namespace ns enforces a Pod Security
// Standard that rejects any of relaxations. enforce is the namespace's
// pod-security.kubernetes.io/enforce label value, resolved by the controller
// (internal/controller/namespace.go); "" means "no label" and is always
// permitted.
//
// KNOWN GAP, stated rather than hidden: PSA can also be configured
// cluster-wide, via the API server's AdmissionConfiguration `defaults`, with
// NO namespace label at all. A controller cannot read that, so this guard
// cannot see it. In that configuration the API server still rejects the pod
// -- with the opaque message this guard exists to pre-empt. Documented in
// docs/engines.md and docs/operations.md.
func CheckNamespacePodSecurity(ns string, engine v1alpha1.EngineType, enforce string, relaxations []Relaxation) error {
	if enforce == "" || enforce == PodSecurityPrivileged {
		return nil
	}

	kindSet := map[string]bool{}
	for _, r := range relaxations {
		for _, level := range forbiddenAt(r) {
			if level == enforce {
				kindSet[string(r.Kind)] = true
			}
		}
	}
	if len(kindSet) == 0 {
		return nil
	}
	kinds := make([]string, 0, len(kindSet))
	for k := range kindSet {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)

	return &PodSecurityIncompatibleError{
		Namespace: ns,
		Engine:    engine,
		Enforce:   enforce,
		Kinds:     kinds,
	}
}
