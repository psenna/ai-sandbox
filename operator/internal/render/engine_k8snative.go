package render

import "github.com/psenna/ai-sandbox/operator/api/v1alpha1"

// k8sNativeEngine is the k8s-native engine (#24): dependencies and dev-tool
// runtimes are native Pods/Services declared via the ServiceSet CRD and
// reconciled by the ServiceSetReconciler, NOT a container nested in the agent
// pod. The engine therefore contributes nothing to the agent pod -- no
// sidecar, no volumes, no security relaxation. The agent container runs by
// itself, exactly like noneEngine; the distinction is the ServiceSet control
// model, surfaced through the sidecar's /v1/services and /v1/exec endpoints
// (see internal/sandboxctl) and the RBAC widened for this engine (rbac.go).
type k8sNativeEngine struct{}

func (k8sNativeEngine) Type() v1alpha1.EngineType { return v1alpha1.EngineTypeK8sNative }

func (k8sNativeEngine) Contribute(Inputs) (Contribution, error) { return Contribution{}, nil }
