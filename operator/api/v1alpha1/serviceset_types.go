package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ServiceSetSpec declares the long-lived dependency services and dev-tool
// runtimes for one SandboxEnvironment. The ServiceSetReconciler reconciles each
// entry to native Pods/Services/PVCs in the ServiceSet's namespace.
type ServiceSetSpec struct {
	// environmentName is the SandboxEnvironment this set belongs to. Used to
	// derive the shared workspace PVC name (<environmentName>-workspace) that
	// runtime pods mount.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	EnvironmentName string `json:"environmentName"`

	// services are long-lived dependency pods. Each gets a Service (cluster
	// DNS <name>.<ns>.svc) and, if storage is set, a retained data PVC.
	// +listType=map
	// +listMapKey=name
	Services []ServiceSpec `json:"services,omitempty"`

	// runtimes are long-lived dev-tool pods the agent execs into. They mount the
	// shared workspace PVC and have no Service.
	// +listType=map
	// +listMapKey=name
	Runtimes []RuntimeSpec `json:"runtimes,omitempty"`
}

// ServiceSpec describes one long-lived dependency pod.
type ServiceSpec struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`
	// +kubebuilder:validation:Required
	Image string            `json:"image"`
	Ports []int32           `json:"ports,omitempty"`
	Env   map[string]string `json:"env,omitempty"`
	// envFromSecret references a Secret name whose keys become env vars.
	EnvFromSecret   *string                     `json:"envFromSecret,omitempty"`
	Command         []string                    `json:"command,omitempty"`
	Args            []string                    `json:"args,omitempty"`
	Resources       corev1.ResourceRequirements `json:"resources,omitempty"`
	ImagePullPolicy corev1.PullPolicy           `json:"imagePullPolicy,omitempty"`
	RunAsUser       *int64                      `json:"runAsUser,omitempty"`
	Healthcheck     HealthcheckSpec             `json:"healthcheck,omitempty"`
	// dependsOn names other service/runtime entries that must be Ready first.
	DependsOn []string            `json:"dependsOn,omitempty"`
	Storage   *ServiceStorageSpec `json:"storage,omitempty"`
	// expose, if set, publishes the first port as a NodePort on this host port.
	Expose *int32 `json:"expose,omitempty"`
}

// RuntimeSpec describes one long-lived dev-tool pod the agent execs into.
type RuntimeSpec struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`
	// +kubebuilder:validation:Required
	Image string `json:"image"`
	// mountWorkspace mounts the shared workspace PVC at /workspace. Defaults to
	// true when omitted.
	// +kubebuilder:default=true
	MountWorkspace *bool                       `json:"mountWorkspace,omitempty"`
	Command        []string                    `json:"command,omitempty"`
	Args           []string                    `json:"args,omitempty"`
	Env            map[string]string           `json:"env,omitempty"`
	Resources      corev1.ResourceRequirements `json:"resources,omitempty"`
	RunAsUser      *int64                      `json:"runAsUser,omitempty"`
	Healthcheck    HealthcheckSpec             `json:"healthcheck,omitempty"`
	DependsOn      []string                    `json:"dependsOn,omitempty"`
}

// HealthcheckSpec maps to a k8s readinessProbe. Exactly one of exec/http/tcp.
type HealthcheckSpec struct {
	Exec []string   `json:"exec,omitempty"`
	HTTP *HTTPProbe `json:"http,omitempty"`
	TCP  *TCPProbe  `json:"tcp,omitempty"`
	// interval defaults to 5s when empty.
	Interval string `json:"interval,omitempty"`
}

type HTTPProbe struct {
	Path string `json:"path"`
	Port int32  `json:"port"`
}

type TCPProbe struct {
	Port int32 `json:"port"`
}

// ServiceStorageSpec creates a per-service RWO data PVC, retained by name across
// Pod recreates.
type ServiceStorageSpec struct {
	// size is a quantity string, e.g. "1Gi".
	// +kubebuilder:validation:Required
	Size string `json:"size"`
	// mountPath is the in-container mount point for the data PVC.
	// +kubebuilder:validation:Required
	MountPath string `json:"mountPath"`
}

// EntryStatus is the readiness of one service or runtime entry.
type EntryStatus struct {
	Name string `json:"name"`
	// kind is "service" or "runtime".
	Kind   string `json:"kind"`
	Ready  bool   `json:"ready"`
	Reason string `json:"reason,omitempty"`
}

// ServiceSetStatus is the reconciled state of a ServiceSet.
type ServiceSetStatus struct {
	// +patchStrategy=merge
	// +patchMergeKey=type
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	Entries    []EntryStatus      `json:"entries,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=sbset,categories=sandbox
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type ServiceSet struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ServiceSetSpec   `json:"spec,omitempty"`
	Status            ServiceSetStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type ServiceSetList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ServiceSet `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ServiceSet{}, &ServiceSetList{})
}
