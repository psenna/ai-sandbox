package render

import (
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
	acorev1 "k8s.io/client-go/applyconfigurations/core/v1"
	anetworkingv1 "k8s.io/client-go/applyconfigurations/networking/v1"
	acrbacv1 "k8s.io/client-go/applyconfigurations/rbac/v1"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
)

// Path/name constants shared by the renderer and, eventually, the pod
// renderer (#21) that mounts these same objects.
const (
	WorkspaceMountPath = "/workspace"
	AgentHomePath      = "/home/node/.claude-sandbox" // == CLAUDE_CONFIG_DIR
	ConfigMountPath    = "/etc/ai-sandbox"
	TaskFileName       = "task.md"
	RunConfigFileName  = "sandbox.json"
	SidecarBaseURL     = "http://localhost:9099" // sandboxctl's control API (#27); loopback-only

	// FieldManager is the stable field manager name used for every
	// server-side apply this operator performs.
	FieldManager = "ai-sandbox-operator"
)

// Credentials carries values resolved by the caller (internal/controller)
// from class-referenced Secrets. Render never reads a Secret itself -- it
// only projects these values into the rendered output. Never logged;
// callers must not log this struct's fields either.
//
// Every stringification path is overridden to redact (#33), mirroring
// internal/storage.Credentials' own String/GoString/MarshalJSON/MarshalLog
// method set, plus a MarshalText override that type doesn't carry (only its
// Secret field type does) -- added here anyway so encoding.TextMarshaler
// call sites are covered too. AccessKeyID-shaped fields are withheld here
// too, even though a bare access key ID is not secret on its own, matching
// storage.Credentials.String's own reasoning: a %v of the whole struct must
// never be mistaken for including one.
type Credentials struct {
	GitProxyToken string

	// SnapshotAccessKeyID/SnapshotSecretAccessKey/SnapshotSessionToken are
	// the S3 snapshot credentials (#28), resolved by internal/controller's
	// resolveCredentials from the class-referenced Secret using
	// internal/storage's fixed data keys (SecretKeyAccessKeyID etc. --
	// storage/doc.go's gap G1). Projected into a NEW per-environment Secret
	// (snapshotsecret.go), mounted into the sandboxctl container ONLY.
	// Never logged, matching GitProxyToken's treatment.
	SnapshotAccessKeyID     string
	SnapshotSecretAccessKey string
	SnapshotSessionToken    string
}

// redactedCredentials is what every stringification path below emits
// instead of a field value.
const redactedCredentials = "render.Credentials{[REDACTED]}" //nolint:gosec // G101: this IS the redaction placeholder, not a credential

// String never includes any field value -- see the type's doc comment on why
// even GitProxyToken/SnapshotAccessKeyID are withheld here.
func (c Credentials) String() string { return redactedCredentials }

// GoString covers %#v.
func (c Credentials) GoString() string { return c.String() }

// MarshalJSON never includes any field value.
func (c Credentials) MarshalJSON() ([]byte, error) {
	return []byte(`"` + redactedCredentials + `"`), nil
}

// MarshalText implements encoding.TextMarshaler.
func (c Credentials) MarshalText() ([]byte, error) { return []byte(redactedCredentials), nil }

// MarshalLog implements logr's Marshaler interface, so a Credentials passed
// as a structured logging value is redacted even when the sink doesn't go
// through fmt or encoding/json.
func (c Credentials) MarshalLog() any { return redactedCredentials }

// NetworkInputs is the caller-resolved set of peers the Restricted policy
// allows egress to, plus the operator ingress selector. Render cannot resolve
// URLs to selectors/CIDRs itself; the controller does (resolveNetworkPeers,
// internal/controller/network.go), exactly like Credentials. Zero-value (all
// nil) is valid for Open isolation (no policy rendered).
type NetworkInputs struct {
	Egress []ResolvedPeer
	// OperatorIngress is the pod selector (in the operator's own namespace)
	// allowed to reach sandbox pods. nil means "no ingress rules" -- the
	// policy's empty Ingress list is default-deny ingress.
	OperatorIngress *v1alpha1.PeerSelector
}

// ResolvedPeer is one egress destination the Restricted policy allows,
// resolved by the controller from a service URL or an extraEgress entry.
// Exactly one of CIDR or Selector is set (render validates this too).
type ResolvedPeer struct {
	CIDR     string
	Selector *v1alpha1.PeerSelector
	Ports    []v1alpha1.NetworkPolicyPort
}

// SkillFile is a reserved seam for future image-independent skill delivery
// via the rendered ConfigMap. #27 delivers the use-sandbox skill through the
// agent image's own entrypoint instead (mirroring how use-git-proxy and
// use-docker are delivered), so nothing populates or reads this today; kept
// as a documented, empty seam for a future issue that needs it.
type SkillFile struct {
	Name    string
	Content string
}

// Inputs is everything Render needs to produce one environment's child
// objects.
type Inputs struct {
	Env         *v1alpha1.SandboxEnvironment
	Class       *v1alpha1.SandboxClass
	Credentials Credentials
	ClusterID   string
	Skills      []SkillFile

	// SidecarImage is the container image for the always-present sandboxctl
	// control-channel sidecar (#27). It is the OPERATOR's own image (the
	// sandboxctl binary ships alongside the manager -- see
	// operator/Dockerfile), supplied by internal/config's --sidecar-image
	// flag, NOT taken from the SandboxClass: the sidecar is operator
	// machinery, versioned with the operator, never with the workload. Only
	// required by RenderPod, not by Render (validateInputs is shared by
	// both and does not check it).
	SidecarImage string

	// SpecHash is "sha256:<hex>" of the class+env specs, computed by
	// internal/controller via storage.SpecHash and projected as a CLI flag
	// on the sandboxctl sidecar container (#28). Render itself stays pure
	// and never imports internal/storage -- this is a plain string, not a
	// storage.Manifest field.
	SpecHash string

	// Restore, when non-nil, describes the snapshot a wake must restore
	// (#29): internal/controller computes it from status.snapshot via
	// storage.SnapshotID (restorePlanFor). nil means "nothing to restore"
	// -- the first-ever run, or an unresolvable snapshot -- in which case
	// RenderPod emits no restore init container at all. Only consulted by
	// RenderPod, not by Render.
	Restore *RestorePlan

	// Network is the caller-resolved set of peers the Restricted policy
	// allows egress to, plus the operator ingress selector (#31). Render
	// cannot resolve URLs to selectors/CIDRs itself; the controller does
	// (resolveNetworkPeers). Zero-value (all nil) is valid for Open
	// isolation (no policy rendered).
	Network NetworkInputs

	// NamespacePodSecurityEnforce is the value of the target namespace's
	// pod-security.kubernetes.io/enforce label, resolved by
	// internal/controller (namespacePodSecurityEnforce) -- render stays pure
	// and never reads a Namespace itself, exactly like Credentials and
	// Network. "" means "no label / unknown" and never blocks a render.
	// Consulted only by RenderPod, via CheckNamespacePodSecurity.
	NamespacePodSecurityEnforce string
}

// RestorePlan describes the snapshot a wake must restore, computed by
// internal/controller from status.snapshot via storage.SnapshotID.
// internal/render stays pure and never imports internal/storage -- this is
// a plain string, exactly like SpecHash.
type RestorePlan struct {
	SnapshotID string
	Seq        int32
}

// Objects holds every rendered child object for one environment.
type Objects struct {
	ServiceAccount *acorev1.ServiceAccountApplyConfiguration
	Role           *acrbacv1.RoleApplyConfiguration
	RoleBinding    *acrbacv1.RoleBindingApplyConfiguration
	PVC            *acorev1.PersistentVolumeClaimApplyConfiguration
	ConfigMap      *acorev1.ConfigMapApplyConfiguration
	Secret         *acorev1.SecretApplyConfiguration
	// SnapshotSecret is nil unless the class's storage backend is S3 (#28).
	SnapshotSecret *acorev1.SecretApplyConfiguration
	// NetworkPolicy is nil for Open isolation (or an unrenderable class);
	// non-nil for Restricted (#31).
	NetworkPolicy *anetworkingv1.NetworkPolicyApplyConfiguration
}

// All returns every child object in a fixed order (ServiceAccount, Role,
// RoleBinding, PVC, ConfigMap, Secret, SnapshotSecret, NetworkPolicy), safe
// to apply in sequence. SnapshotSecret and NetworkPolicy are appended ONLY
// when non-nil: a typed-nil *acorev1.SecretApplyConfiguration (or
// *anetworkingv1.NetworkPolicyApplyConfiguration) stored in a
// []runtime.ApplyConfiguration interface slice is NOT an interface nil (a
// classic Go footgun), so an unconditional append would apply a nil object
// whenever the backend isn't S3 or the isolation is Open.
func (o Objects) All() []runtime.ApplyConfiguration {
	all := []runtime.ApplyConfiguration{o.ServiceAccount, o.Role, o.RoleBinding, o.PVC, o.ConfigMap, o.Secret}
	if o.SnapshotSecret != nil {
		all = append(all, o.SnapshotSecret)
	}
	if o.NetworkPolicy != nil {
		all = append(all, o.NetworkPolicy)
	}
	return all
}

// validateInputs performs the nil/empty checks common to every renderer
// entry point (Render, RenderPod): a non-nil Env and Class, and a
// non-empty Env.UID (the object must be persisted first -- owner references
// need a real UID).
func validateInputs(in Inputs) error {
	if in.Env == nil {
		return fmt.Errorf("render: Env is required")
	}
	if in.Class == nil {
		return fmt.Errorf("render: Class is required")
	}
	if in.Env.UID == "" {
		return fmt.Errorf("render: Env.UID is required (object must be persisted first)")
	}
	return nil
}

// Render renders every child object for one environment. Pure: no client, no
// clock, no randomness, deterministic map/slice ordering throughout. Returns
// an error only for permanent misconfiguration (nil Env/Class, empty
// Env.UID, an unparseable storage.workspace.size) -- never a transient one.
func Render(in Inputs) (Objects, error) {
	if err := validateInputs(in); err != nil {
		return Objects{}, err
	}

	sa := renderServiceAccount(in)
	role := renderRole(in)
	rb := renderRoleBinding(in)
	pvc, err := renderPVC(in)
	if err != nil {
		return Objects{}, fmt.Errorf("render: PVC: %w", err)
	}
	cm, err := renderConfigMap(in)
	if err != nil {
		return Objects{}, fmt.Errorf("render: ConfigMap: %w", err)
	}
	secret := renderSecret(in)
	snapshotSecret := renderSnapshotSecret(in)
	np, err := RenderNetworkPolicy(in)
	if err != nil {
		return Objects{}, fmt.Errorf("render: NetworkPolicy: %w", err)
	}

	return Objects{
		ServiceAccount: sa,
		Role:           role,
		RoleBinding:    rb,
		PVC:            pvc,
		ConfigMap:      cm,
		Secret:         secret,
		SnapshotSecret: snapshotSecret,
		NetworkPolicy:  np,
	}, nil
}
