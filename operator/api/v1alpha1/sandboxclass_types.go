package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EngineType selects the container engine a sandbox uses to run agent
// workloads. The allowed values are declared on the EngineSpec.Type field
// (not here) to avoid controller-gen emitting a duplicate/allOf-wrapped
// enum constraint.
type EngineType string

const (
	// EngineTypeRootlessPodman runs sandboxes with a rootless Podman engine
	// nested inside the sandbox pod, giving the agent its own container
	// runtime without host root privileges.
	EngineTypeRootlessPodman EngineType = "rootless-podman"

	// EngineTypeNone runs the agent directly in the sandbox pod with no
	// nested container engine.
	EngineTypeNone EngineType = "none"
)

// StorageBackendType selects where sandbox workspace snapshots and archives
// are persisted. The allowed values are declared on the BackendSpec.Type
// field (not here) to avoid controller-gen emitting a duplicate/allOf-wrapped
// enum constraint.
type StorageBackendType string

const (
	// StorageBackendTypeS3 persists snapshots and archives to an S3-compatible
	// object store.
	StorageBackendTypeS3 StorageBackendType = "s3"

	// StorageBackendTypePVC persists snapshots and archives to a
	// PersistentVolumeClaim.
	StorageBackendTypePVC StorageBackendType = "pvc"
)

// NetworkIsolation selects the network policy applied to sandbox pods. The
// allowed values are declared on the NetworkSpec.Isolation field (not here)
// to avoid controller-gen emitting a duplicate/allOf-wrapped enum
// constraint.
type NetworkIsolation string

const (
	// NetworkIsolationRestricted restricts sandbox pods to the shared
	// service endpoints declared in spec.services plus any spec.network.extraEgress
	// entries, denying all other egress.
	NetworkIsolationRestricted NetworkIsolation = "Restricted"

	// NetworkIsolationOpen allows sandbox pods unrestricted network egress.
	NetworkIsolationOpen NetworkIsolation = "Open"
)

// SandboxClassSpec defines a reusable template that SandboxEnvironments
// reference to configure the agent image, engine, shared services, storage
// and network policy of the sandboxes created from it.
type SandboxClassSpec struct {
	// Agent configures the agent container image, compute resources and
	// model tier routing.
	// +kubebuilder:validation:Required
	Agent AgentSpec `json:"agent"`

	// Engine selects and configures the container engine sandboxes use to
	// run workloads.
	// +kubebuilder:default={}
	// +optional
	Engine EngineSpec `json:"engine,omitempty"`

	// Services configures the shared service endpoints (git-proxy,
	// DependaProxy, Ollama) sandboxes created from this class can reach.
	// +optional
	Services ServicesSpec `json:"services,omitempty"`

	// Storage configures the sandbox workspace volume, the snapshot/archive
	// backend, and the warm-cache retention window.
	// +kubebuilder:validation:Required
	Storage StorageSpec `json:"storage"`

	// Network configures the network isolation policy applied to sandbox
	// pods.
	// +kubebuilder:default={}
	// +optional
	Network NetworkSpec `json:"network,omitempty"`

	// Timeouts configures how long a sandbox may remain in various lifecycle
	// states before it is forcibly reclaimed.
	// +kubebuilder:default={}
	// +optional
	Timeouts TimeoutsSpec `json:"timeouts,omitempty"`
}

// AgentSpec configures the agent container image, compute resources and
// model tier routing for sandboxes created from this class.
type AgentSpec struct {
	// Image is the container image reference for the agent process run
	// inside the sandbox.
	// +kubebuilder:validation:MinLength=1
	Image string `json:"image"`

	// Resources sets the compute resource requests and limits for the agent
	// container.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// Model configures which underlying model each requested model tier
	// (default, haiku, sonnet, opus) resolves to.
	// +optional
	Model *ModelSpec `json:"model,omitempty"`
}

// ModelSpec maps the agent's model tier names to the concrete model
// identifiers used to serve them.
type ModelSpec struct {
	// Default is the model identifier used when the agent does not request
	// a specific tier.
	// +optional
	Default string `json:"default,omitempty"`

	// Haiku is the model identifier used when the agent requests the
	// "haiku" (fastest/cheapest) tier.
	// +optional
	Haiku string `json:"haiku,omitempty"`

	// Sonnet is the model identifier used when the agent requests the
	// "sonnet" (balanced) tier.
	// +optional
	Sonnet string `json:"sonnet,omitempty"`

	// Opus is the model identifier used when the agent requests the "opus"
	// (highest-capability) tier.
	// +optional
	Opus string `json:"opus,omitempty"`
}

// EngineSpec selects and configures the container engine sandboxes use to
// run workloads.
type EngineSpec struct {
	// Type selects the container engine implementation.
	// +kubebuilder:validation:Enum=rootless-podman;none
	// +kubebuilder:default=rootless-podman
	// +optional
	Type EngineType `json:"type,omitempty"`

	// Image overrides the container image used to run the engine itself
	// (for example, the rootless-podman sidecar image). If unset, the
	// controller's built-in default for the selected engine type is used.
	// +optional
	Image string `json:"image,omitempty"`

	// StorageDriver selects the storage driver used by the nested container
	// engine.
	// +kubebuilder:validation:Enum=auto;overlay;vfs
	// +kubebuilder:default=auto
	// +optional
	StorageDriver string `json:"storageDriver,omitempty"`
}

// ServicesSpec configures the shared service endpoints sandboxes created
// from this class can reach.
type ServicesSpec struct {
	// GitProxy configures the git-proxy endpoint used for clone/fetch/push
	// and the agent-facing broker REST API (PRs, CI status, issues).
	// +optional
	GitProxy *GitProxyService `json:"gitProxy,omitempty"`

	// DependaProxy configures the dependency proxy endpoints used for
	// npm/pip/Go module installs.
	// +optional
	DependaProxy *DependaProxyService `json:"dependaProxy,omitempty"`

	// RegistryMirror configures a pull-through container-registry cache the
	// rootless-podman engine pulls workload images through (#24). Required
	// in practice for `engine.type: rootless-podman` under
	// `network.isolation: Restricted`: a Restricted NetworkPolicy allows
	// egress only to the peers this class declares, so without a mirror the
	// podman sidecar can reach no registry at all. Ignored by
	// `engine.type: none`.
	// +optional
	RegistryMirror *RegistryMirrorService `json:"registryMirror,omitempty"`

	// Ollama configures the local model inference endpoint.
	// +optional
	Ollama *OllamaService `json:"ollama,omitempty"`
}

// GitProxyService configures the git-proxy endpoints and credentials
// sandboxes use to reach the upstream git host.
type GitProxyService struct {
	// GitURL is the git-proxy git-protocol endpoint (clone/fetch/push)
	// sandboxes use to reach the upstream git host.
	// +kubebuilder:validation:Pattern=`^https?://`
	GitURL string `json:"gitURL"`

	// BrokerURL is the git-proxy agent-facing broker REST API endpoint used
	// for pull requests, CI status, and issues.
	// +kubebuilder:validation:Pattern=`^https?://`
	BrokerURL string `json:"brokerURL"`

	// TokenSecretRef references the Secret holding the bearer token
	// sandboxes use to authenticate to git-proxy.
	TokenSecretRef SecretKeyRef `json:"tokenSecretRef"`
}

// DependaProxyService configures the dependency proxy endpoints sandboxes
// use instead of talking to public package registries directly.
type DependaProxyService struct {
	// NpmURL is the DependaProxy endpoint sandboxes use for npm package
	// installs.
	// +kubebuilder:validation:Pattern=`^https?://`
	// +optional
	NpmURL string `json:"npmURL,omitempty"`

	// PypiURL is the DependaProxy endpoint sandboxes use for pip package
	// installs.
	// +kubebuilder:validation:Pattern=`^https?://`
	// +optional
	PypiURL string `json:"pypiURL,omitempty"`

	// GoproxyURL is the DependaProxy endpoint sandboxes use for Go module
	// downloads.
	// +kubebuilder:validation:Pattern=`^https?://`
	// +optional
	GoproxyURL string `json:"goproxyURL,omitempty"`
}

// RegistryMirrorService configures the pull-through container-registry cache
// the container engine pulls workload images through. Rendered into the
// podman sidecar's registries.conf as one `[[registry]]`/`[[registry.mirror]]`
// pair per entry in Registries, and resolved by
// internal/controller/network.go's serviceEndpoints into a NetworkPolicy
// egress peer -- both from this single declaration.
type RegistryMirrorService struct {
	// URL is the mirror's base URL. Scheme is load-bearing: an http:// URL
	// renders `insecure = true` on the mirror entry (a plain-HTTP in-cluster
	// cache), https:// does not. A path component is preserved, so a
	// multi-project mirror can be addressed as
	// https://harbor.example.com/dockerhub.
	// +kubebuilder:validation:Pattern=`^https?://`
	URL string `json:"url"`

	// Registries are the upstream registry prefixes this mirror serves.
	// Empty means ["docker.io"] -- the only upstream a stock
	// `registry:2` pull-through cache (REGISTRY_PROXY_REMOTEURL) can serve.
	// Rendered in the given order, deduplicated, sorted for determinism.
	// +optional
	// +listType=atomic
	Registries []string `json:"registries,omitempty"`
}

// OllamaService configures the local model inference endpoint sandboxes can
// use for on-cluster model serving.
type OllamaService struct {
	// BaseURL is the Ollama endpoint sandboxes use for local model
	// inference.
	// +kubebuilder:validation:Pattern=`^https?://`
	BaseURL string `json:"baseURL"`
}

// StorageSpec configures the sandbox workspace volume, the snapshot/archive
// backend, and the warm-cache retention window.
type StorageSpec struct {
	// Workspace configures the PersistentVolumeClaim sandboxes use as their
	// working directory.
	// +kubebuilder:default={}
	// +optional
	Workspace WorkspaceSpec `json:"workspace,omitempty"`

	// Backend configures where sandbox workspace snapshots and archives are
	// persisted.
	// +kubebuilder:validation:Required
	Backend BackendSpec `json:"backend"`

	// WarmCacheTTL is how long a sandbox's workspace snapshot is kept in a
	// warm cache (eligible for fast restore) after the sandbox is frozen,
	// before falling back to the slower archive backend. Expressed as a Go
	// duration string (for example "30m", "1h30m").
	// +kubebuilder:validation:Pattern=`^([0-9]+(\.[0-9]+)?(ns|us|ms|s|m|h))+$`
	// +kubebuilder:default="30m"
	// +optional
	WarmCacheTTL string `json:"warmCacheTTL,omitempty"`
}

// WorkspaceSpec configures the PersistentVolumeClaim sandboxes use as their
// working directory.
type WorkspaceSpec struct {
	// Size is the requested storage capacity of the workspace volume,
	// expressed as a Kubernetes quantity string (for example "20Gi").
	// +kubebuilder:default="20Gi"
	// +optional
	Size string `json:"size,omitempty"`

	// StorageClassName selects the StorageClass used to provision the
	// workspace volume. A pointer distinguishes "unset" (use the cluster
	// default StorageClass) from an explicit empty string (use no
	// StorageClass).
	// +optional
	StorageClassName *string `json:"storageClassName,omitempty"`
}

// BackendSpec configures where sandbox workspace snapshots and archives are
// persisted.
// +kubebuilder:validation:XValidation:rule="self.type != 's3' || has(self.s3)",message="storage.backend.s3 is required when type is 's3'"
// +kubebuilder:validation:XValidation:rule="self.type != 'pvc' || has(self.pvc)",message="storage.backend.pvc is required when type is 'pvc'"
// +kubebuilder:validation:XValidation:rule="self.type == 's3' || !has(self.s3)",message="storage.backend.s3 may only be set when type is 's3'"
// +kubebuilder:validation:XValidation:rule="self.type == 'pvc' || !has(self.pvc)",message="storage.backend.pvc may only be set when type is 'pvc'"
type BackendSpec struct {
	// Type selects the storage backend implementation used for snapshots
	// and archives.
	// +kubebuilder:validation:Enum=s3;pvc
	// +kubebuilder:default=s3
	// +optional
	Type StorageBackendType `json:"type,omitempty"`

	// S3 configures the S3-compatible object store backend. Required when
	// type is "s3"; must be unset otherwise.
	// +optional
	S3 *S3Backend `json:"s3,omitempty"`

	// PVC configures the PersistentVolumeClaim backend. Required when type
	// is "pvc"; must be unset otherwise.
	// +optional
	PVC *PVCBackend `json:"pvc,omitempty"`
}

// S3Backend configures an S3-compatible object store used to persist
// sandbox workspace snapshots and archives.
type S3Backend struct {
	// Endpoint is the S3-compatible API endpoint URL.
	// +kubebuilder:validation:Pattern=`^https?://`
	Endpoint string `json:"endpoint"`

	// Bucket is the name of the bucket snapshots and archives are stored
	// in.
	// +kubebuilder:validation:MinLength=3
	Bucket string `json:"bucket"`

	// Region is the object store region, if required by the endpoint.
	// +optional
	Region string `json:"region,omitempty"`

	// Prefix is an object key prefix applied to all snapshots and archives
	// written by sandboxes created from this class.
	// +optional
	Prefix string `json:"prefix,omitempty"`

	// CredentialsSecretRef references the Secret holding the S3 access
	// credentials.
	CredentialsSecretRef SecretKeyRef `json:"credentialsSecretRef"`

	// ForcePathStyle selects path-style S3 addressing
	// (https://endpoint/bucket/key) instead of virtual-hosted-style
	// (https://bucket.endpoint/key). Most self-hosted S3-compatible stores
	// (for example MinIO) require path-style addressing, hence the default
	// of true.
	// +kubebuilder:default=true
	// +optional
	ForcePathStyle *bool `json:"forcePathStyle,omitempty"`
}

// PVCBackend configures a PersistentVolumeClaim used to persist sandbox
// workspace snapshots and archives.
type PVCBackend struct {
	// ClaimName is the name of the PersistentVolumeClaim snapshots and
	// archives are written to.
	// +kubebuilder:validation:MinLength=1
	ClaimName string `json:"claimName"`

	// SubPath is a path within the PersistentVolumeClaim under which
	// snapshots and archives are written.
	// +optional
	SubPath string `json:"subPath,omitempty"`
}

// SecretKeyRef references a single key within a Secret in the same
// namespace as the referencing object.
type SecretKeyRef struct {
	// Name is the name of the referenced Secret.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Key is the key within the referenced Secret's data whose value is
	// used.
	// +kubebuilder:default=token
	// +optional
	Key string `json:"key,omitempty"`
}

// EgressPeer is one additional egress destination a Restricted sandbox may
// reach beyond the shared service endpoints. Exactly one of CIDR or
// Selector must be set.
// +kubebuilder:validation:XValidation:rule="has(self.cidr) != (has(self.selector) && self.selector != null)",message="network.extraEgress entries must set exactly one of cidr or selector"
type EgressPeer struct {
	// CIDR is an IP block (e.g. "1.2.3.4/32", "10.0.0.0/8"). Mutually exclusive
	// with Selector. External destinations (any host not an in-cluster Service)
	// MUST use CIDR -- a selector cannot match an off-cluster host, and the
	// operator rejects a Restricted class whose external service/storage
	// endpoint is not covered by an extraEgress CIDR.
	// +optional
	CIDR string `json:"cidr,omitempty"`
	// Selector selects in-cluster pods. Namespace must be named; PodSelector is
	// optional (empty = all pods in the namespace). Mutually exclusive with CIDR.
	// +optional
	Selector *PeerSelector `json:"selector,omitempty"`
	// Ports restricts egress to the listed destination ports. Empty = all ports.
	// +optional
	// +listType=atomic
	Ports []NetworkPolicyPort `json:"ports,omitempty"`
}

// PeerSelector selects in-cluster pods by namespace and optional labels.
type PeerSelector struct {
	// Namespace is the namespace whose pods the selector matches.
	// +kubebuilder:validation:MinLength=1
	Namespace string `json:"namespace"`
	// PodSelector is an optional label selector narrowing the match to a
	// subset of the namespace's pods; empty matches all pods in the
	// namespace.
	// +optional
	PodSelector *metav1.LabelSelector `json:"podSelector,omitempty"`
}

// NetworkPolicyPort mirrors the Kubernetes NetworkPolicyPort shape.
type NetworkPolicyPort struct {
	// Protocol is the transport protocol the port applies to (TCP, UDP or
	// SCTP). Empty defaults to TCP.
	// +kubebuilder:validation:Enum=TCP;UDP;SCTP
	// +optional
	Protocol string `json:"protocol,omitempty"`
	// Port is the destination port: a numeric port (e.g. "443") or a named
	// port (e.g. "https").
	// +kubebuilder:validation:MinLength=1
	Port string `json:"port"`
}

// NetworkSpec configures the network isolation policy applied to sandbox
// pods.
type NetworkSpec struct {
	// Isolation selects the network policy applied to sandbox pods.
	// +kubebuilder:validation:Enum=Restricted;Open
	// +kubebuilder:default=Restricted
	// +optional
	Isolation NetworkIsolation `json:"isolation,omitempty"`

	// ExtraEgress lists additional egress destinations (for example an
	// external host's CIDR, or an in-cluster pod selector) sandbox pods may
	// reach beyond the shared service endpoints declared in spec.services.
	// Only consulted when isolation is "Restricted".
	// +optional
	// +listType=atomic
	ExtraEgress []EgressPeer `json:"extraEgress,omitempty"`
}

// TimeoutsSpec configures how long a sandbox may remain in various
// lifecycle states before it is forcibly reclaimed.
type TimeoutsSpec struct {
	// Running is the maximum time a sandbox may remain in the Running phase
	// before being frozen, expressed as a Go duration string.
	// +kubebuilder:validation:Pattern=`^([0-9]+(\.[0-9]+)?(ns|us|ms|s|m|h))+$`
	// +kubebuilder:default="6h"
	// +optional
	Running string `json:"running,omitempty"`

	// Waiting is the maximum time a sandbox may remain in the Waiting phase
	// before being reclaimed, expressed as a Go duration string.
	// +kubebuilder:validation:Pattern=`^([0-9]+(\.[0-9]+)?(ns|us|ms|s|m|h))+$`
	// +kubebuilder:default="24h"
	// +optional
	Waiting string `json:"waiting,omitempty"`

	// Total is the maximum total lifetime of a sandbox, across all phases,
	// before being reclaimed, expressed as a Go duration string.
	// +kubebuilder:validation:Pattern=`^([0-9]+(\.[0-9]+)?(ns|us|ms|s|m|h))+$`
	// +kubebuilder:default="72h"
	// +optional
	Total string `json:"total,omitempty"`
}

// SandboxClassStatus surfaces class-level observations, such as problems
// with a referenced Secret, that controllers reconciling SandboxEnvironments
// built from this class may report.
type SandboxClassStatus struct {
	// ObservedGeneration is the most recent generation observed by the
	// controller reconciling this class.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions represents the latest available observations of this
	// class's state.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=sbclass,categories=sandbox
// +kubebuilder:printcolumn:name="Engine",type=string,JSONPath=`.spec.engine.type`
// +kubebuilder:printcolumn:name="Backend",type=string,JSONPath=`.spec.storage.backend.type`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// SandboxClass is a cluster-scoped, reusable template describing the agent
// image, compute resources, model tiers, container engine, shared service
// endpoints, storage and network isolation policy for the sandboxes created
// from it.
type SandboxClass struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// Spec is the desired state of the class.
	Spec SandboxClassSpec `json:"spec,omitempty"`

	// Status is the most recently observed state of the class.
	// +kubebuilder:default={}
	// +optional
	Status SandboxClassStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// SandboxClassList is a list of SandboxClass resources.
type SandboxClassList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SandboxClass `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SandboxClass{}, &SandboxClassList{})
}
