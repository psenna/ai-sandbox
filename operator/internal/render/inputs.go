package render

import (
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
	acorev1 "k8s.io/client-go/applyconfigurations/core/v1"
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
	SidecarBaseURL     = "http://localhost:9099" // reserved for #27

	// FieldManager is the stable field manager name used for every
	// server-side apply this operator performs.
	FieldManager = "ai-sandbox-operator"
)

// Credentials carries values resolved by the caller (internal/controller)
// from class-referenced Secrets. Render never reads a Secret itself -- it
// only projects these values into the rendered output. Never logged;
// callers must not log this struct's fields either.
type Credentials struct {
	GitProxyToken string
}

// SkillFile is a reserved seam for #27's sidecar skill files. Empty today.
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
}

// Objects holds every rendered child object for one environment.
type Objects struct {
	ServiceAccount *acorev1.ServiceAccountApplyConfiguration
	Role           *acrbacv1.RoleApplyConfiguration
	RoleBinding    *acrbacv1.RoleBindingApplyConfiguration
	PVC            *acorev1.PersistentVolumeClaimApplyConfiguration
	ConfigMap      *acorev1.ConfigMapApplyConfiguration
	Secret         *acorev1.SecretApplyConfiguration
}

// All returns every child object in a fixed order (ServiceAccount, Role,
// RoleBinding, PVC, ConfigMap, Secret), safe to apply in sequence.
func (o Objects) All() []runtime.ApplyConfiguration {
	return []runtime.ApplyConfiguration{o.ServiceAccount, o.Role, o.RoleBinding, o.PVC, o.ConfigMap, o.Secret}
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

	return Objects{
		ServiceAccount: sa,
		Role:           role,
		RoleBinding:    rb,
		PVC:            pvc,
		ConfigMap:      cm,
		Secret:         secret,
	}, nil
}
