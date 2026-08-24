package render

import (
	"errors"
	"fmt"

	acorev1 "k8s.io/client-go/applyconfigurations/core/v1"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
)

// Engine contributes the pod fragments a container engine needs. The pod
// renderer composes an Engine's contribution with the agent container's own
// base definition; nothing else in the operator ever branches on engine
// type.
type Engine interface {
	// Type is the EngineType this implementation serves.
	Type() v1alpha1.EngineType

	// Contribute returns this engine's additions to the pod. It is pure and
	// deterministic: same Inputs, same output, every time. It returns an
	// error only for permanent, render-time misconfiguration -- never a
	// transient one.
	Contribute(in Inputs) (Contribution, error)
}

// Contribution is everything an Engine adds to the rendered pod. Every field
// is additive; an Engine can never remove or rewrite what the pod renderer
// built for the agent container, and can only weaken a securityContext
// through the closed Relaxations allowlist below.
type Contribution struct {
	// Containers are appended after the agent container, in slice order
	// (#24: the `podman system service` sidecar).
	Containers []*acorev1.ContainerApplyConfiguration
	// InitContainers are prepended before any the pod renderer adds itself
	// (#29's restore init container).
	InitContainers []*acorev1.ContainerApplyConfiguration
	// Volumes are extra pod volumes (#24: podman's emptyDir graph root,
	// deliberately excluded from snapshots per the epic).
	Volumes []*acorev1.VolumeApplyConfiguration
	// AgentEnv are extra env vars set on the AGENT container only, appended
	// after the base env (#24: DOCKER_HOST=tcp://127.0.0.1:2375).
	AgentEnv []*acorev1.EnvVarApplyConfiguration
	// AgentVolumeMounts are extra mounts on the AGENT container only.
	AgentVolumeMounts []*acorev1.VolumeMountApplyConfiguration
	// Relaxations are the deviations from the tight security baseline this
	// engine requires. The renderer applies them; the engine never hands
	// back a raw securityContext. See Relaxation.
	Relaxations []Relaxation
}

// RelaxationKind is the closed set of securityContext weakenings an Engine
// may request. The values are exactly what issue #23's privilege ladder
// (operator/spike/podman-privilege-ladder.yaml,
// operator/docs/spike-rootless-podman.md) proved rootless-podman needs, plus
// AddCapability for a future engine. Anything else is a render error -- an
// engine cannot express `privileged`, a hostPath, a projected token, or a
// runAsUser change through this interface at all. This is "construct, never
// filter": a closed enum makes the relaxation set enumerable (so the
// EngineSecurityRelaxed condition message is generated from the same data
// that gets applied, and cannot lie) and bounded (anything not in the
// allowlist is a render error, not a silent escalation).
type RelaxationKind string

const (
	// RelaxAppArmorUnconfined sets appArmorProfile.type=Unconfined. Spike
	// #23's ladder case G: the sole thing that unblocks podman on AppArmor
	// distros, and the sole PSS `baseline` violation it causes.
	RelaxAppArmorUnconfined RelaxationKind = "AppArmorUnconfined"
	// RelaxSeccompUnset OMITS seccompProfile entirely (rather than setting
	// it to Unconfined). Spike #23's ladder case H: seccomp RuntimeDefault
	// blocks clone(CLONE_NEWUSER) independently of AppArmor.
	RelaxSeccompUnset RelaxationKind = "SeccompUnset"
	// RelaxAllowPrivilegeEscalation sets allowPrivilegeEscalation=true.
	// Spike #23's ladder case J: newuidmap carries cap_setuid=ep as a FILE
	// capability and no-new-privileges prevents it from taking effect.
	RelaxAllowPrivilegeEscalation RelaxationKind = "AllowPrivilegeEscalation"
	// RelaxAddCapability adds the single capability named in Value to the
	// container's capabilities.add (capabilities.drop: [ALL] is retained).
	RelaxAddCapability RelaxationKind = "AddCapability"
)

// Relaxation is one documented deviation from the tight security baseline.
type Relaxation struct {
	// Container is the container name the relaxation applies to. Must name
	// a container that exists in the rendered pod, or RenderPod errors.
	Container string
	// Kind is the weakening requested.
	Kind RelaxationKind
	// Value is the capability name for RelaxAddCapability; empty otherwise.
	Value string
	// Reason is a short human justification, surfaced verbatim in the
	// EngineSecurityRelaxed condition message. Required to be non-empty.
	Reason string
}

// ErrEngineNotImplemented is returned by an engine that is registered (so
// the dispatch/seam machinery is real) but whose real implementation lands
// in a later issue.
var ErrEngineNotImplemented = errors.New("engine is not implemented yet")

// engineFor resolves the Engine for t, normalizing "" to the CRD default
// (rootless-podman), exactly mirroring how pvc.go defaults an empty
// storage.workspace.size.
func engineFor(t v1alpha1.EngineType) (Engine, error) {
	if t == "" {
		t = v1alpha1.EngineTypeRootlessPodman
	}
	e, ok := engineRegistry[t]
	if !ok {
		return nil, fmt.Errorf("render: unknown engine type %q", t)
	}
	return e, nil
}

var engineRegistry = map[v1alpha1.EngineType]Engine{
	v1alpha1.EngineTypeNone:           noneEngine{},
	v1alpha1.EngineTypeRootlessPodman: notImplementedEngine{typ: v1alpha1.EngineTypeRootlessPodman, issue: 24},
	v1alpha1.EngineTypeK8sNative:      k8sNativeEngine{},
}

// notImplementedEngine is registered (so the dispatch/seam machinery is real)
// but always fails closed, naming the issue that ships the real implementation.
// rootless-podman remains the unimplemented stub: issue #24 was re-scoped to a
// k8s-native engine (see engine_k8snative.go + the approved design), so the
// rootless-podman-in-a-pod approach is abandoned and its branch excluded.
type notImplementedEngine struct {
	typ   v1alpha1.EngineType
	issue int
}

func (e notImplementedEngine) Type() v1alpha1.EngineType { return e.typ }

func (e notImplementedEngine) Contribute(Inputs) (Contribution, error) {
	return Contribution{}, fmt.Errorf("render: engine %q %w (see issue #%d)", e.typ, ErrEngineNotImplemented, e.issue)
}

// EngineRelaxations returns the securityContext relaxations engine type t
// requires, without rendering a full pod. ok=false means the engine is
// unknown or not implemented yet, i.e. its posture is not yet knowable.
func EngineRelaxations(t v1alpha1.EngineType) (relaxations []Relaxation, ok bool) {
	e, err := engineFor(t)
	if err != nil {
		return nil, false
	}
	c, err := e.Contribute(Inputs{})
	if err != nil {
		return nil, false
	}
	return c.Relaxations, true
}
