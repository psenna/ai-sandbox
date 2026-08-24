# CRD reference

<!-- GENERATED FILE - DO NOT EDIT.
     Regenerate:  cd operator && make crd-docs
     Source of truth: the Go doc comments and +kubebuilder markers on
     operator/api/v1alpha1/*.go, via controller-gen into
     operator/config/crd/bases/*.yaml.
     CI (.github/workflows/operator.yml, job `api`) regenerates this file
     and fails the build if the committed copy differs. -->

## SandboxClass

- **API group/version:** `sandbox.psenna.dev/v1alpha1`
- **Scope:** Cluster
- **Short names:** sbclass
- **Categories:** sandbox
- **Status subresource:** yes

SandboxClass is a cluster-scoped, reusable template describing the agent image, compute resources, model tiers, container engine, shared service endpoints, storage and network isolation policy for the sandboxes created from it.

### Printer columns

| Name | Type | JSONPath | Priority |
|---|---|---|---|
| Engine | string | `.spec.engine.type` | 0 |
| Backend | string | `.spec.storage.backend.type` | 0 |
| Age | date | `.metadata.creationTimestamp` | 0 |

### .spec

| Field | Type | Required | Default | Constraints | Description |
|---|---|---|---|---|---|
| `spec.agent` | object | yes | — | — | Agent configures the agent container image, compute resources and model tier routing. |
| `spec.agent.image` | string | yes | — | minLength: 1 | Image is the container image reference for the agent process run inside the sandbox. |
| `spec.agent.model` | object | no | — | — | Model configures which underlying model each requested model tier (default, haiku, sonnet, opus) resolves to. |
| `spec.agent.model.default` | string | no | — | — | Default is the model identifier used when the agent does not request a specific tier. |
| `spec.agent.model.haiku` | string | no | — | — | Haiku is the model identifier used when the agent requests the "haiku" (fastest/cheapest) tier. |
| `spec.agent.model.opus` | string | no | — | — | Opus is the model identifier used when the agent requests the "opus" (highest-capability) tier. |
| `spec.agent.model.sonnet` | string | no | — | — | Sonnet is the model identifier used when the agent requests the "sonnet" (balanced) tier. |
| `spec.agent.resources` | object | no | — | — | Resources sets the compute resource requests and limits for the agent container. |
| `spec.agent.resources.claims` | array<object> | no | — | — | Claims lists the names of resources, defined in spec.resourceClaims, that are used by this container. This field depends on the DynamicResourceAllocation feature gate. This field is immutable. It can only be set for containers. |
| `spec.agent.resources.claims[]` | object | no | — | — | ResourceClaim references one entry in PodSpec.ResourceClaims. |
| `spec.agent.resources.claims[].name` | string | yes | — | — | Name must match the name of one entry in pod.spec.resourceClaims of the Pod where this field is used. It makes that resource available inside a container. |
| `spec.agent.resources.claims[].request` | string | no | — | — | Request is the name chosen for a request in the referenced claim. If empty, everything from the claim is made available, otherwise only the result of this request. |
| `spec.agent.resources.limits` | map[string] | no | — | — | Limits describes the maximum amount of compute resources allowed. More info: https://kubernetes.io/docs/concepts/configuration/manage-resources-containers/ |
| `spec.agent.resources.limits{}` | (int-or-string) | no | — | pattern: `^(\+\|-)?(([0-9]+(\.[0-9]*)?)\|(\.[0-9]+))(([KMGTPE]i)\|[numkMGTPE]\|([eE](\+\|-)?(([0-9]+(\.[0-9]*)?)\|(\.[0-9]+))))?$` |  |
| `spec.agent.resources.requests` | map[string] | no | — | — | Requests describes the minimum amount of compute resources required. If Requests is omitted for a container, it defaults to Limits if that is explicitly specified, otherwise to an implementation-defined value. Requests cannot exceed Limits. More info: https://kubernetes.io/docs/concepts/configuration/manage-resources-containers/ |
| `spec.agent.resources.requests{}` | (int-or-string) | no | — | pattern: `^(\+\|-)?(([0-9]+(\.[0-9]*)?)\|(\.[0-9]+))(([KMGTPE]i)\|[numkMGTPE]\|([eE](\+\|-)?(([0-9]+(\.[0-9]*)?)\|(\.[0-9]+))))?$` |  |
| `spec.engine` | object | no | `{}` | — | Engine selects and configures the container engine sandboxes use to run workloads. |
| `spec.engine.image` | string | no | — | — | Image overrides the container image used to run the engine itself (for example, the rootless-podman sidecar image). If unset, the controller's built-in default for the selected engine type is used. |
| `spec.engine.storageDriver` | string | no | `"auto"` | enum: auto, overlay, vfs | StorageDriver selects the storage driver used by the nested container engine. |
| `spec.engine.type` | string | no | `"rootless-podman"` | enum: rootless-podman, none | Type selects the container engine implementation. |
| `spec.network` | object | no | `{}` | — | Network configures the network isolation policy applied to sandbox pods. |
| `spec.network.extraEgress` | array<object> | no | — | — | ExtraEgress lists additional egress destinations (for example an external host's CIDR, or an in-cluster pod selector) sandbox pods may reach beyond the shared service endpoints declared in spec.services. Only consulted when isolation is "Restricted". |
| `spec.network.extraEgress[]` | object | no | — | — | EgressPeer is one additional egress destination a Restricted sandbox may reach beyond the shared service endpoints. Exactly one of CIDR or Selector must be set. |
| `spec.network.extraEgress[].cidr` | string | no | — | — | CIDR is an IP block (e.g. "1.2.3.4/32", "10.0.0.0/8"). Mutually exclusive with Selector. External destinations (any host not an in-cluster Service) MUST use CIDR -- a selector cannot match an off-cluster host, and the operator rejects a Restricted class whose external service/storage endpoint is not covered by an extraEgress CIDR. |
| `spec.network.extraEgress[].ports` | array<object> | no | — | — | Ports restricts egress to the listed destination ports. Empty = all ports. |
| `spec.network.extraEgress[].ports[]` | object | no | — | — | NetworkPolicyPort mirrors the Kubernetes NetworkPolicyPort shape. |
| `spec.network.extraEgress[].ports[].port` | string | yes | — | minLength: 1 | Port is the destination port: a numeric port (e.g. "443") or a named port (e.g. "https"). |
| `spec.network.extraEgress[].ports[].protocol` | string | no | — | enum: TCP, UDP, SCTP | Protocol is the transport protocol the port applies to (TCP, UDP or SCTP). Empty defaults to TCP. |
| `spec.network.extraEgress[].selector` | object | no | — | — | Selector selects in-cluster pods. Namespace must be named; PodSelector is optional (empty = all pods in the namespace). Mutually exclusive with CIDR. |
| `spec.network.extraEgress[].selector.namespace` | string | yes | — | minLength: 1 | Namespace is the namespace whose pods the selector matches. |
| `spec.network.extraEgress[].selector.podSelector` | object | no | — | — | PodSelector is an optional label selector narrowing the match to a subset of the namespace's pods; empty matches all pods in the namespace. |
| `spec.network.extraEgress[].selector.podSelector.matchExpressions` | array<object> | no | — | — | matchExpressions is a list of label selector requirements. The requirements are ANDed. |
| `spec.network.extraEgress[].selector.podSelector.matchExpressions[]` | object | no | — | — | A label selector requirement is a selector that contains values, a key, and an operator that relates the key and values. |
| `spec.network.extraEgress[].selector.podSelector.matchExpressions[].key` | string | yes | — | — | key is the label key that the selector applies to. |
| `spec.network.extraEgress[].selector.podSelector.matchExpressions[].operator` | string | yes | — | — | operator represents a key's relationship to a set of values. Valid operators are In, NotIn, Exists and DoesNotExist. |
| `spec.network.extraEgress[].selector.podSelector.matchExpressions[].values` | array<string> | no | — | — | values is an array of string values. If the operator is In or NotIn, the values array must be non-empty. If the operator is Exists or DoesNotExist, the values array must be empty. This array is replaced during a strategic merge patch. |
| `spec.network.extraEgress[].selector.podSelector.matchExpressions[].values[]` | string | no | — | — |  |
| `spec.network.extraEgress[].selector.podSelector.matchLabels` | map[string]string | no | — | — | matchLabels is a map of {key,value} pairs. A single {key,value} in the matchLabels map is equivalent to an element of matchExpressions, whose key field is "key", the operator is "In", and the values array contains only "value". The requirements are ANDed. |
| `spec.network.extraEgress[].selector.podSelector.matchLabels{}` | string | no | — | — |  |
| `spec.network.isolation` | string | no | `"Restricted"` | enum: Restricted, Open | Isolation selects the network policy applied to sandbox pods. |
| `spec.services` | object | no | — | — | Services configures the shared service endpoints (git-proxy, DependaProxy, Ollama) sandboxes created from this class can reach. |
| `spec.services.dependaProxy` | object | no | — | — | DependaProxy configures the dependency proxy endpoints used for npm/pip/Go module installs. |
| `spec.services.dependaProxy.goproxyURL` | string | no | — | pattern: `^https?://` | GoproxyURL is the DependaProxy endpoint sandboxes use for Go module downloads. |
| `spec.services.dependaProxy.npmURL` | string | no | — | pattern: `^https?://` | NpmURL is the DependaProxy endpoint sandboxes use for npm package installs. |
| `spec.services.dependaProxy.pypiURL` | string | no | — | pattern: `^https?://` | PypiURL is the DependaProxy endpoint sandboxes use for pip package installs. |
| `spec.services.gitProxy` | object | no | — | — | GitProxy configures the git-proxy endpoint used for clone/fetch/push and the agent-facing broker REST API (PRs, CI status, issues). |
| `spec.services.gitProxy.brokerURL` | string | yes | — | pattern: `^https?://` | BrokerURL is the git-proxy agent-facing broker REST API endpoint used for pull requests, CI status, and issues. |
| `spec.services.gitProxy.gitURL` | string | yes | — | pattern: `^https?://` | GitURL is the git-proxy git-protocol endpoint (clone/fetch/push) sandboxes use to reach the upstream git host. |
| `spec.services.gitProxy.tokenSecretRef` | object | yes | — | — | TokenSecretRef references the Secret holding the bearer token sandboxes use to authenticate to git-proxy. |
| `spec.services.gitProxy.tokenSecretRef.key` | string | no | `"token"` | — | Key is the key within the referenced Secret's data whose value is used. |
| `spec.services.gitProxy.tokenSecretRef.name` | string | yes | — | minLength: 1 | Name is the name of the referenced Secret. |
| `spec.services.ollama` | object | no | — | — | Ollama configures the local model inference endpoint. |
| `spec.services.ollama.baseURL` | string | yes | — | pattern: `^https?://` | BaseURL is the Ollama endpoint sandboxes use for local model inference. |
| `spec.storage` | object | yes | — | — | Storage configures the sandbox workspace volume, the snapshot/archive backend, and the warm-cache retention window. |
| `spec.storage.backend` | object | yes | — | — | Backend configures where sandbox workspace snapshots and archives are persisted. |
| `spec.storage.backend.pvc` | object | no | — | — | PVC configures the PersistentVolumeClaim backend. Required when type is "pvc"; must be unset otherwise. |
| `spec.storage.backend.pvc.claimName` | string | yes | — | minLength: 1 | ClaimName is the name of the PersistentVolumeClaim snapshots and archives are written to. |
| `spec.storage.backend.pvc.subPath` | string | no | — | — | SubPath is a path within the PersistentVolumeClaim under which snapshots and archives are written. |
| `spec.storage.backend.s3` | object | no | — | — | S3 configures the S3-compatible object store backend. Required when type is "s3"; must be unset otherwise. |
| `spec.storage.backend.s3.bucket` | string | yes | — | minLength: 3 | Bucket is the name of the bucket snapshots and archives are stored in. |
| `spec.storage.backend.s3.credentialsSecretRef` | object | yes | — | — | CredentialsSecretRef references the Secret holding the S3 access credentials. |
| `spec.storage.backend.s3.credentialsSecretRef.key` | string | no | `"token"` | — | Key is the key within the referenced Secret's data whose value is used. |
| `spec.storage.backend.s3.credentialsSecretRef.name` | string | yes | — | minLength: 1 | Name is the name of the referenced Secret. |
| `spec.storage.backend.s3.endpoint` | string | yes | — | pattern: `^https?://` | Endpoint is the S3-compatible API endpoint URL. |
| `spec.storage.backend.s3.forcePathStyle` | boolean | no | `true` | — | ForcePathStyle selects path-style S3 addressing (https://endpoint/bucket/key) instead of virtual-hosted-style (https://bucket.endpoint/key). Most self-hosted S3-compatible stores (for example MinIO) require path-style addressing, hence the default of true. |
| `spec.storage.backend.s3.prefix` | string | no | — | — | Prefix is an object key prefix applied to all snapshots and archives written by sandboxes created from this class. |
| `spec.storage.backend.s3.region` | string | no | — | — | Region is the object store region, if required by the endpoint. |
| `spec.storage.backend.type` | string | no | `"s3"` | enum: s3, pvc | Type selects the storage backend implementation used for snapshots and archives. |
| `spec.storage.warmCacheTTL` | string | no | `"30m"` | pattern: `^([0-9]+(\.[0-9]+)?(ns\|us\|ms\|s\|m\|h))+$` | WarmCacheTTL is how long a sandbox's workspace snapshot is kept in a warm cache (eligible for fast restore) after the sandbox is frozen, before falling back to the slower archive backend. Expressed as a Go duration string (for example "30m", "1h30m"). |
| `spec.storage.workspace` | object | no | `{}` | — | Workspace configures the PersistentVolumeClaim sandboxes use as their working directory. |
| `spec.storage.workspace.size` | string | no | `"20Gi"` | — | Size is the requested storage capacity of the workspace volume, expressed as a Kubernetes quantity string (for example "20Gi"). |
| `spec.storage.workspace.storageClassName` | string | no | — | — | StorageClassName selects the StorageClass used to provision the workspace volume. A pointer distinguishes "unset" (use the cluster default StorageClass) from an explicit empty string (use no StorageClass). |
| `spec.timeouts` | object | no | `{}` | — | Timeouts configures how long a sandbox may remain in various lifecycle states before it is forcibly reclaimed. |
| `spec.timeouts.running` | string | no | `"6h"` | pattern: `^([0-9]+(\.[0-9]+)?(ns\|us\|ms\|s\|m\|h))+$` | Running is the maximum time a sandbox may remain in the Running phase before being frozen, expressed as a Go duration string. |
| `spec.timeouts.total` | string | no | `"72h"` | pattern: `^([0-9]+(\.[0-9]+)?(ns\|us\|ms\|s\|m\|h))+$` | Total is the maximum total lifetime of a sandbox, across all phases, before being reclaimed, expressed as a Go duration string. |
| `spec.timeouts.waiting` | string | no | `"24h"` | pattern: `^([0-9]+(\.[0-9]+)?(ns\|us\|ms\|s\|m\|h))+$` | Waiting is the maximum time a sandbox may remain in the Waiting phase before being reclaimed, expressed as a Go duration string. |

### .status

| Field | Type | Required | Default | Constraints | Description |
|---|---|---|---|---|---|
| `status.conditions` | array<object> | no | — | — | Conditions represents the latest available observations of this class's state. |
| `status.conditions[]` | object | no | — | — | Condition contains details for one aspect of the current state of this API Resource. |
| `status.conditions[].lastTransitionTime` | string | yes | — | format: date-time | lastTransitionTime is the last time the condition transitioned from one status to another. This should be when the underlying condition changed. If that is not known, then using the time when the API field changed is acceptable. |
| `status.conditions[].message` | string | yes | — | maxLength: 32768 | message is a human readable message indicating details about the transition. This may be an empty string. |
| `status.conditions[].observedGeneration` | integer | no | — | minimum: 0; format: int64 | observedGeneration represents the .metadata.generation that the condition was set based upon. For instance, if .metadata.generation is currently 12, but the .status.conditions[x].observedGeneration is 9, the condition is out of date with respect to the current state of the instance. |
| `status.conditions[].reason` | string | yes | — | minLength: 1; maxLength: 1024; pattern: `^[A-Za-z]([A-Za-z0-9_,:]*[A-Za-z0-9_])?$` | reason contains a programmatic identifier indicating the reason for the condition's last transition. Producers of specific condition types may define expected values and meanings for this field, and whether the values are considered a guaranteed API. The value should be a CamelCase string. This field may not be empty. |
| `status.conditions[].status` | string | yes | — | enum: True, False, Unknown | status of the condition, one of True, False, Unknown. |
| `status.conditions[].type` | string | yes | — | maxLength: 316; pattern: `^([a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*/)?(([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9])$` | type of condition in CamelCase or in foo.example.com/CamelCase. |
| `status.observedGeneration` | integer | no | — | format: int64 | ObservedGeneration is the most recent generation observed by the controller reconciling this class. |

### Validation rules (CEL)

| Path | Rule | Message |
|---|---|---|
| `spec.network.extraEgress[]` | `has(self.cidr) != (has(self.selector) && self.selector != null)` | network.extraEgress entries must set exactly one of cidr or selector |
| `spec.storage.backend` | `self.type != 's3' \|\| has(self.s3)` | storage.backend.s3 is required when type is 's3' |
| `spec.storage.backend` | `self.type != 'pvc' \|\| has(self.pvc)` | storage.backend.pvc is required when type is 'pvc' |
| `spec.storage.backend` | `self.type == 's3' \|\| !has(self.s3)` | storage.backend.s3 may only be set when type is 's3' |
| `spec.storage.backend` | `self.type == 'pvc' \|\| !has(self.pvc)` | storage.backend.pvc may only be set when type is 'pvc' |

## SandboxEnvironment

- **API group/version:** `sandbox.psenna.dev/v1alpha1`
- **Scope:** Namespaced
- **Short names:** sbenv
- **Categories:** sandbox
- **Status subresource:** yes

SandboxEnvironment is a namespaced resource representing a single agent run: the class to build the sandbox from, the repository and task the agent operates on, and the observed lifecycle state of that run.

### Printer columns

| Name | Type | JSONPath | Priority |
|---|---|---|---|
| Phase | string | `.status.phase` | 0 |
| Slot | boolean | `.status.slot.granted` | 0 |
| Freezes | integer | `.status.freezeCount` | 0 |
| Wakes | integer | `.status.wakeCount` | 0 |
| Age | date | `.metadata.creationTimestamp` | 0 |
| Class | string | `.spec.classRef.name` | 1 |
| Repo | string | `.spec.repo` | 1 |

### .spec

| Field | Type | Required | Default | Constraints | Description |
|---|---|---|---|---|---|
| `spec.classRef` | object | yes | — | — | ClassRef references the SandboxClass this environment's sandbox is built from. Immutable after creation. |
| `spec.classRef.name` | string | yes | — | minLength: 1; maxLength: 253 | Name is the name of the referenced SandboxClass. |
| `spec.priority` | integer | no | `0` | minimum: -1000; maximum: 1000; format: int32 | Priority influences scheduling order among environments competing for a slot; higher values are scheduled first. |
| `spec.repo` | string | yes | — | minLength: 1; pattern: `^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+(\.git)?$` | Repo is the repository this environment's agent operates on, in "owner/name" or "owner/name.git" form. Immutable after creation. |
| `spec.suspend` | boolean | no | `false` | — | Suspend pauses scheduling of this environment: a suspended environment will not be granted a slot, and a running sandbox will be frozen, until suspend is cleared. |
| `spec.task` | object | yes | — | — | Task describes the work the agent should perform. Immutable after creation. |
| `spec.task.issueRef` | object | no | — | — | IssueRef references an issue whose title/body/comments the agent should use as its task instructions. |
| `spec.task.issueRef.number` | integer | yes | — | minimum: 1; format: int32 | Number is the issue number. |
| `spec.task.issueRef.repo` | string | yes | — | minLength: 1 | Repo is the repository the issue belongs to, in "owner/name" form. |
| `spec.task.prompt` | string | no | — | maxLength: 8192 | Prompt is free-form task instructions given directly to the agent. |

### .status

| Field | Type | Required | Default | Constraints | Description |
|---|---|---|---|---|---|
| `status.agentResult` | object | no | — | — | AgentResult records the agent's own report of how its run ended, as declared through the sandboxctl sidecar's POST /v1/done. |
| `status.agentResult.exitCode` | integer | no | — | minimum: 0; maximum: 255; format: int32 | ExitCode is the process exit code the agent intends to exit with. Advisory only: the pod's real exit code is what internal/controller/podstatus.go observes. |
| `status.agentResult.message` | string | no | — | maxLength: 512 | Message is the agent's own explanation, truncated to 512 bytes by the sidecar before it is written. Surfaced verbatim in the Ready condition message via lifecycle.ClusterFacts.AgentMessage. |
| `status.agentResult.outcome` | string | yes | — | enum: Succeeded, Failed | AgentOutcome is how the agent reported its run ended. |
| `status.agentResult.reportedAt` | string | no | — | format: date-time | ReportedAt is when this result was reported. Stamped by the sidecar, never supplied by the agent. |
| `status.archive` | object | no | — | — | Archive records the terminal archive written for this environment. Written by the sandboxctl archive Job; read by the controller to clear the finalizer and by retention GC to select archives past their TTL. |
| `status.archive.contextPresent` | boolean | no | — | — | ContextPresent is false when no agent-home snapshot existed to draw context.tar.zst from (a never-frozen run whose pod was already gone). |
| `status.archive.finishedAt` | string | no | — | format: date-time | FinishedAt is when the run reached its terminal phase (mirrors status.finishedAt; duplicated here so retention GC need not parse run.json to find the run's end time). |
| `status.archive.runJSONSHA256` | string | no | — | pattern: `^[a-f0-9]{64}$` | RunJSONSHA256 is the lowercase hex SHA-256 of run.json, for audit. |
| `status.archive.uri` | string | yes | — | minLength: 1 | URI is where the archive was written (e.g. s3://<bucket>/<prefix>/<clusterID>/<ns>/<name>/<uid>/archive). |
| `status.archiveURI` | string | no | — | — | ArchiveURI is the location of this environment's final workspace archive, once the environment has reached a terminal phase. |
| `status.conditions` | array<object> | no | — | — | Conditions represents the latest available observations of this environment's state. |
| `status.conditions[]` | object | no | — | — | Condition contains details for one aspect of the current state of this API Resource. |
| `status.conditions[].lastTransitionTime` | string | yes | — | format: date-time | lastTransitionTime is the last time the condition transitioned from one status to another. This should be when the underlying condition changed. If that is not known, then using the time when the API field changed is acceptable. |
| `status.conditions[].message` | string | yes | — | maxLength: 32768 | message is a human readable message indicating details about the transition. This may be an empty string. |
| `status.conditions[].observedGeneration` | integer | no | — | minimum: 0; format: int64 | observedGeneration represents the .metadata.generation that the condition was set based upon. For instance, if .metadata.generation is currently 12, but the .status.conditions[x].observedGeneration is 9, the condition is out of date with respect to the current state of the instance. |
| `status.conditions[].reason` | string | yes | — | minLength: 1; maxLength: 1024; pattern: `^[A-Za-z]([A-Za-z0-9_,:]*[A-Za-z0-9_])?$` | reason contains a programmatic identifier indicating the reason for the condition's last transition. Producers of specific condition types may define expected values and meanings for this field, and whether the values are considered a guaranteed API. The value should be a CamelCase string. This field may not be empty. |
| `status.conditions[].status` | string | yes | — | enum: True, False, Unknown | status of the condition, one of True, False, Unknown. |
| `status.conditions[].type` | string | yes | — | maxLength: 316; pattern: `^([a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*/)?(([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9])$` | type of condition in CamelCase or in foo.example.com/CamelCase. |
| `status.finishedAt` | string | no | — | format: date-time | FinishedAt is when the environment reached a terminal phase (Done or Failed). |
| `status.freezeCount` | integer | no | `0` | minimum: 0; format: int32 | FreezeCount is the number of times this environment's sandbox has been frozen. |
| `status.gitState` | object | no | — | — | GitState records the agent's final git state, if the agent recorded one. Written by the sandboxctl sidecar from /workspace/.sandbox/git-state.json during freeze; surfaced in run.json. |
| `status.gitState.branch` | string | no | — | — | Branch is the git branch the agent left the workspace on. |
| `status.gitState.headSHA` | string | no | — | pattern: `^[0-9a-f]{40}$` | HeadSHA is the lowercase hex SHA-1 of HEAD. |
| `status.gitState.pullRequest` | object | no | — | — | PullRequest, if the agent opened/updated one, references it. |
| `status.gitState.pullRequest.number` | integer | yes | — | minimum: 1; format: int32 | Number is the pull request number. |
| `status.gitState.pullRequest.repo` | string | yes | — | minLength: 1 | Repo is the repository the pull request belongs to, in "owner/name" form. |
| `status.observedGeneration` | integer | no | — | format: int64 | ObservedGeneration is the most recent generation observed by the controller reconciling this environment. |
| `status.phase` | string | no | `"Pending"` | enum: Pending, Ready, Running, Freezing, Waiting, Restoring, Done, Failed | Phase is the coarse-grained lifecycle state of the environment. |
| `status.phaseHistory` | array<object> | no | — | maxItems: 64 | PhaseHistory is the phase-transition history with timestamps, appended on every phase change in lifecycle.Apply. It is the issue's "full phase-transition history with timestamps" requirement: the Ready condition only carries the LastTransitionTime for the *current* phase, while this list preserves every transition so run.json can record them. Capped at MaxPhaseHistoryEntries, oldest first: a long-lived environment that freezes and wakes repeatedly keeps its most recent transitions rather than wedging on the maxItems limit. |
| `status.phaseHistory[]` | object | no | — | — | PhaseTransition records one observed phase change. |
| `status.phaseHistory[].at` | string | yes | — | format: date-time | At is when the environment was first observed in this phase. |
| `status.phaseHistory[].phase` | string | yes | — | enum: Pending, Ready, Running, Freezing, Waiting, Restoring, Done, Failed | Phase is the phase the environment entered. |
| `status.phaseHistory[].reason` | string | no | — | — | Reason is the Ready condition's Reason at the transition, if any (the summary condition's reason carries the terminal/failure reason). |
| `status.probeAttempt` | object | no | — | — | ProbeAttempt records the operator's most recent evaluation of status.waitFor (#30). Written ONLY by the operator's ProbeEvaluator (internal/controller/probe.go), never by the sidecar or the agent. It is how a human, and the requeue logic, tell "still pending, will retry" from "unevaluatable, will fail" without inferring anything from timing. |
| `status.probeAttempt.attempts` | integer | no | — | minimum: 0; format: int32 | Attempts is how many evaluation attempts have been made so far. |
| `status.probeAttempt.consecutiveErrors` | integer | no | — | minimum: 0; format: int32 | ConsecutiveErrors is how many consecutive unevaluatable results have been observed. When it reaches the evaluator's MaxConsecutiveErrors threshold the environment fails rather than hanging. |
| `status.probeAttempt.lastAttemptAt` | string | no | — | format: date-time | LastAttemptAt is when the most recent evaluation ran. |
| `status.probeAttempt.lastResult` | string | no | — | enum: satisfied, pending, error | LastResult is the outcome of the most recent evaluation: "satisfied", "pending", or "error". |
| `status.probeAttempt.message` | string | no | — | maxLength: 512 | Message is a human explanation. Never contains a credential. |
| `status.probeAttempt.nextEligibleAt` | string | no | — | format: date-time | NextEligibleAt is when the next evaluation may run. The evaluator performs at most one I/O call per reconcile and suppresses calls before this time; the requeue logic wakes the reconciler at it. |
| `status.probeAttempt.phase` | string | yes | — | enum: Pending, Satisfied, Failed | Phase is the current state of this probe attempt. |
| `status.probeAttempt.reason` | string | no | — | maxLength: 64 | Reason is a short, stable machine reason. See internal/lifecycle/conditions.go's ReasonProbeFailed. |
| `status.probeAttempt.type` | string | yes | — | enum: GitProxyCheck, HTTPGet, S3ObjectExists, NotBefore | Type is the WaitForStatus.Type this attempt evaluated. |
| `status.queuedSince` | string | no | — | format: date-time | QueuedSince is when the environment started waiting for a slot. |
| `status.restoreAttempt` | object | no | — | — | RestoreAttempt records the wake currently in flight, or the last one that failed. Written ONLY by the restore init container (the same sandboxctl binary, `restore` subcommand). It is how the operator, and a human, tell a warm wake from a cold one -- and a corrupt snapshot from a slow one -- without inferring anything from timing. |
| `status.restoreAttempt.attempts` | integer | no | — | minimum: 0; format: int32 | Attempts is how many restore attempts have been made so far. |
| `status.restoreAttempt.durationMillis` | integer | no | — | minimum: 0; format: int64 | DurationMillis is how long the restore took. |
| `status.restoreAttempt.message` | string | no | — | maxLength: 512 | Message is a human explanation. Never contains a credential. |
| `status.restoreAttempt.phase` | string | yes | — | enum: InProgress, Succeeded, Failed | RestoreAttemptPhase is the state of the wake restore being attempted. |
| `status.restoreAttempt.reason` | string | no | — | maxLength: 64 | Reason is a short, stable machine reason. See internal/sandboxctl/restore.go's RestoreReason* constants. |
| `status.restoreAttempt.roots` | array<object> | no | — | — | Roots records the outcome per restored root. The workspace root is the only one that can ever be Warm: the agent home is an emptyDir that dies with the pod, so it is restored cold on every wake. |
| `status.restoreAttempt.roots[]` | object | no | — | — | RestoredRootStatus is one restored root's outcome. |
| `status.restoreAttempt.roots[].bytesDownloaded` | integer | no | — | minimum: 0; format: int64 | BytesDownloaded is the number of uncompressed bytes restored from the backend into this root. Always 0 when Source is Warm -- this is the acceptance criterion's "measurably skips the download", asserted as a value rather than inferred from elapsed time. |
| `status.restoreAttempt.roots[].name` | string | yes | — | enum: workspace, agent-home | Name is the restored root: "workspace" (the mounted workspace PVC, the only root that can ever be Warm) or "agent-home" (the per-pod agent home emptyDir, always restored cold). |
| `status.restoreAttempt.roots[].source` | string | yes | — | enum: Warm, Cold | Source is Warm when the retained PVC already held this exact snapshot -- validated against the manifest, never inferred from the PVC merely existing -- so no bytes were downloaded for this root. |
| `status.restoreAttempt.roots[].warmMissReason` | string | no | — | maxLength: 64 | WarmMissReason, set only when Source is Cold, names why the warm path was refused. |
| `status.restoreAttempt.seq` | integer | yes | — | minimum: 0; format: int32 | Seq is the snapshot sequence number this attempt is restoring. |
| `status.restoreAttempt.snapshotID` | string | no | — | maxLength: 128 | SnapshotID is the snapshot directory name restored from ("<seq:05d>-<RFC3339>"), so a human can find the exact objects. |
| `status.restoreAttempt.startedAt` | string | no | — | format: date-time | StartedAt is when this attempt (this Seq) was first recorded. |
| `status.restoreAttempt.updatedAt` | string | no | — | format: date-time | UpdatedAt is when this attempt was last patched (a retry, a phase change, or a per-root outcome landing). |
| `status.slot` | object | no | — | — | Slot records whether this environment currently holds a scheduling slot. |
| `status.slot.granted` | boolean | no | `false` | — | Granted is true when this environment currently holds a scheduling slot. |
| `status.slot.grantedAt` | string | no | — | format: date-time | GrantedAt is when the slot was granted. |
| `status.slot.leaseName` | string | no | — | — | LeaseName is the name of the lease resource backing the granted slot. |
| `status.snapshot` | object | no | — | — | Snapshot records the most recent workspace snapshot taken for this environment. |
| `status.snapshot.durationMillis` | integer | no | — | minimum: 0; format: int64 | DurationMillis is how long the snapshot took, from the start of the freeze hook to the successful latest.json write. Milliseconds, not seconds: a small workspace snapshots in well under a second and a whole-second field would record a meaningless 0. |
| `status.snapshot.seq` | integer | yes | — | minimum: 0; format: int32 | Seq is the monotonically increasing sequence number of this snapshot. |
| `status.snapshot.sha256` | string | no | — | pattern: `^[a-f0-9]{64}$` | SHA256 is the lowercase hex-encoded SHA-256 checksum of the snapshot. |
| `status.snapshot.sizeBytes` | integer | no | — | format: int64 | SizeBytes is the size of the snapshot in bytes. |
| `status.snapshot.takenAt` | string | no | — | format: date-time | TakenAt is when the snapshot was taken. |
| `status.snapshot.uri` | string | no | — | — | URI is the location the snapshot was written to. |
| `status.snapshotAttempt` | object | no | — | — | SnapshotAttempt records the freeze snapshot currently in flight, or the last one that failed. Written ONLY by the sandboxctl sidecar (or the recovery Job running the same binary). It is how the operator, and a human, distinguish "still retrying" from "permanently failed": on permanent failure the environment HOLDS in Freezing with Frozen=False/SnapshotFailed and the pod is never deleted, so the agent's context is never silently dropped. |
| `status.snapshotAttempt.attempts` | integer | no | — | minimum: 0; format: int32 | Attempts is how many upload attempts have been made so far. |
| `status.snapshotAttempt.message` | string | no | — | maxLength: 512 | Message is a human explanation. Never contains a credential -- see internal/storage/doc.go's no-logging rule and credentials.go's Secret redaction. |
| `status.snapshotAttempt.phase` | string | yes | — | enum: InProgress, Succeeded, Failed | SnapshotAttemptPhase is the state of the freeze snapshot being attempted. |
| `status.snapshotAttempt.reason` | string | no | — | maxLength: 64 | Reason is a short, stable machine reason. See internal/sandboxctl/snapshot.go's SnapshotReason* constants. |
| `status.snapshotAttempt.seq` | integer | yes | — | minimum: 0; format: int32 | Seq is the snapshot sequence number this attempt is producing. It always equals status.freezeCount for the freeze in flight. |
| `status.snapshotAttempt.startedAt` | string | no | — | format: date-time | StartedAt is when this attempt (this Seq) was first recorded. |
| `status.snapshotAttempt.updatedAt` | string | no | — | format: date-time | UpdatedAt is when this attempt was last patched (a retry, a phase change). |
| `status.startedAt` | string | no | — | format: date-time | StartedAt is when the sandbox first started running. |
| `status.terminalPhase` | string | no | — | enum: Pending, Ready, Running, Freezing, Waiting, Restoring, Done, Failed | TerminalPhase records the terminal phase (Done or Failed) the environment reached, set once when the run first terminated. Used by the freeze-detour path (#32) to return to the correct terminal phase after capturing the agent home, rather than re-running the agent. |
| `status.waitFor` | object | no | — | — | WaitFor records the external condition, if any, blocking a frozen sandbox from being restored. |
| `status.waitFor.declaredAt` | string | no | — | format: date-time | DeclaredAt is when this wait condition was declared. Stamped by the sidecar, never supplied by the agent. |
| `status.waitFor.params` | map[string]string | no | — | maxProperties: 16 | Params carries type-specific parameters for the wait condition. Which keys are required/permitted per Type is enforced fail-closed by internal/sandboxctl/probe.go; see that file for the per-type table. |
| `status.waitFor.params{}` | string | no | — | — |  |
| `status.waitFor.reason` | string | no | — | maxLength: 512 | Reason is a human-readable explanation of why this wait was declared. |
| `status.waitFor.type` | string | yes | — | enum: GitProxyCheck, HTTPGet, S3ObjectExists, NotBefore | Type identifies the kind of condition being waited on. The enum IS the allowlist: the API server rejects anything else, and internal/sandboxctl rejects it earlier with an actionable error. The members are exactly the probe types issue #30 will evaluate. |
| `status.wakeCount` | integer | no | `0` | minimum: 0; format: int32 | WakeCount is the number of times this environment's sandbox has been restored from a frozen state. |

### Validation rules (CEL)

| Path | Rule | Message |
|---|---|---|
| `spec.classRef` | `self == oldSelf` | classRef is immutable |
| `spec.repo` | `self == oldSelf` | repo is immutable |
| `spec.task` | `self == oldSelf` | task is immutable |
| `spec.task` | `(has(self.prompt) && size(self.prompt) > 0) \|\| has(self.issueRef)` | task requires at least one of prompt or issueRef |

## ServiceSet

- **API group/version:** `sandbox.psenna.dev/v1alpha1`
- **Scope:** Namespaced
- **Short names:** sbset
- **Categories:** sandbox
- **Status subresource:** yes

### Printer columns

| Name | Type | JSONPath | Priority |
|---|---|---|---|
| Ready | string | `.status.conditions[?(@.type=="Ready")].status` | 0 |
| Age | date | `.metadata.creationTimestamp` | 0 |

### .spec

| Field | Type | Required | Default | Constraints | Description |
|---|---|---|---|---|---|
| `spec.environmentName` | string | yes | — | minLength: 1 | environmentName is the SandboxEnvironment this set belongs to. Used to derive the shared workspace PVC name (<environmentName>-workspace) that runtime pods mount. |
| `spec.runtimes` | array<object> | no | — | — | runtimes are long-lived dev-tool pods the agent execs into. They mount the shared workspace PVC and have no Service. |
| `spec.runtimes[]` | object | no | — | — | RuntimeSpec describes one long-lived dev-tool pod the agent execs into. |
| `spec.runtimes[].args` | array<string> | no | — | — |  |
| `spec.runtimes[].args[]` | string | no | — | — |  |
| `spec.runtimes[].command` | array<string> | no | — | — |  |
| `spec.runtimes[].command[]` | string | no | — | — |  |
| `spec.runtimes[].dependsOn` | array<string> | no | — | — |  |
| `spec.runtimes[].dependsOn[]` | string | no | — | — |  |
| `spec.runtimes[].env` | map[string]string | no | — | — |  |
| `spec.runtimes[].env{}` | string | no | — | — |  |
| `spec.runtimes[].healthcheck` | object | no | — | — | HealthcheckSpec maps to a k8s readinessProbe. Exactly one of exec/http/tcp. |
| `spec.runtimes[].healthcheck.exec` | array<string> | no | — | — |  |
| `spec.runtimes[].healthcheck.exec[]` | string | no | — | — |  |
| `spec.runtimes[].healthcheck.http` | object | no | — | — |  |
| `spec.runtimes[].healthcheck.http.path` | string | yes | — | — |  |
| `spec.runtimes[].healthcheck.http.port` | integer | yes | — | format: int32 |  |
| `spec.runtimes[].healthcheck.interval` | string | no | — | — | interval defaults to 5s when empty. |
| `spec.runtimes[].healthcheck.tcp` | object | no | — | — |  |
| `spec.runtimes[].healthcheck.tcp.port` | integer | yes | — | format: int32 |  |
| `spec.runtimes[].image` | string | yes | — | — |  |
| `spec.runtimes[].mountWorkspace` | boolean | no | `true` | — | mountWorkspace mounts the shared workspace PVC at /workspace. Defaults to true when omitted. |
| `spec.runtimes[].name` | string | yes | — | minLength: 1; maxLength: 63 |  |
| `spec.runtimes[].resources` | object | no | — | — | ResourceRequirements describes the compute resource requirements. |
| `spec.runtimes[].resources.claims` | array<object> | no | — | — | Claims lists the names of resources, defined in spec.resourceClaims, that are used by this container. This field depends on the DynamicResourceAllocation feature gate. This field is immutable. It can only be set for containers. |
| `spec.runtimes[].resources.claims[]` | object | no | — | — | ResourceClaim references one entry in PodSpec.ResourceClaims. |
| `spec.runtimes[].resources.claims[].name` | string | yes | — | — | Name must match the name of one entry in pod.spec.resourceClaims of the Pod where this field is used. It makes that resource available inside a container. |
| `spec.runtimes[].resources.claims[].request` | string | no | — | — | Request is the name chosen for a request in the referenced claim. If empty, everything from the claim is made available, otherwise only the result of this request. |
| `spec.runtimes[].resources.limits` | map[string] | no | — | — | Limits describes the maximum amount of compute resources allowed. More info: https://kubernetes.io/docs/concepts/configuration/manage-resources-containers/ |
| `spec.runtimes[].resources.limits{}` | (int-or-string) | no | — | pattern: `^(\+\|-)?(([0-9]+(\.[0-9]*)?)\|(\.[0-9]+))(([KMGTPE]i)\|[numkMGTPE]\|([eE](\+\|-)?(([0-9]+(\.[0-9]*)?)\|(\.[0-9]+))))?$` |  |
| `spec.runtimes[].resources.requests` | map[string] | no | — | — | Requests describes the minimum amount of compute resources required. If Requests is omitted for a container, it defaults to Limits if that is explicitly specified, otherwise to an implementation-defined value. Requests cannot exceed Limits. More info: https://kubernetes.io/docs/concepts/configuration/manage-resources-containers/ |
| `spec.runtimes[].resources.requests{}` | (int-or-string) | no | — | pattern: `^(\+\|-)?(([0-9]+(\.[0-9]*)?)\|(\.[0-9]+))(([KMGTPE]i)\|[numkMGTPE]\|([eE](\+\|-)?(([0-9]+(\.[0-9]*)?)\|(\.[0-9]+))))?$` |  |
| `spec.runtimes[].runAsUser` | integer | no | — | format: int64 |  |
| `spec.services` | array<object> | no | — | — | services are long-lived dependency pods. Each gets a Service (cluster DNS <name>.<ns>.svc) and, if storage is set, a retained data PVC. |
| `spec.services[]` | object | no | — | — | ServiceSpec describes one long-lived dependency pod. |
| `spec.services[].args` | array<string> | no | — | — |  |
| `spec.services[].args[]` | string | no | — | — |  |
| `spec.services[].command` | array<string> | no | — | — |  |
| `spec.services[].command[]` | string | no | — | — |  |
| `spec.services[].dependsOn` | array<string> | no | — | — | dependsOn names other service/runtime entries that must be Ready first. |
| `spec.services[].dependsOn[]` | string | no | — | — |  |
| `spec.services[].env` | map[string]string | no | — | — |  |
| `spec.services[].env{}` | string | no | — | — |  |
| `spec.services[].envFromSecret` | string | no | — | — | envFromSecret references a Secret name whose keys become env vars. |
| `spec.services[].expose` | integer | no | — | format: int32 | expose, if set, publishes the first port as a NodePort on this host port. |
| `spec.services[].healthcheck` | object | no | — | — | HealthcheckSpec maps to a k8s readinessProbe. Exactly one of exec/http/tcp. |
| `spec.services[].healthcheck.exec` | array<string> | no | — | — |  |
| `spec.services[].healthcheck.exec[]` | string | no | — | — |  |
| `spec.services[].healthcheck.http` | object | no | — | — |  |
| `spec.services[].healthcheck.http.path` | string | yes | — | — |  |
| `spec.services[].healthcheck.http.port` | integer | yes | — | format: int32 |  |
| `spec.services[].healthcheck.interval` | string | no | — | — | interval defaults to 5s when empty. |
| `spec.services[].healthcheck.tcp` | object | no | — | — |  |
| `spec.services[].healthcheck.tcp.port` | integer | yes | — | format: int32 |  |
| `spec.services[].image` | string | yes | — | — |  |
| `spec.services[].imagePullPolicy` | string | no | — | — | PullPolicy describes a policy for if/when to pull a container image |
| `spec.services[].name` | string | yes | — | minLength: 1; maxLength: 63 |  |
| `spec.services[].ports` | array<integer> | no | — | — |  |
| `spec.services[].ports[]` | integer | no | — | format: int32 |  |
| `spec.services[].resources` | object | no | — | — | ResourceRequirements describes the compute resource requirements. |
| `spec.services[].resources.claims` | array<object> | no | — | — | Claims lists the names of resources, defined in spec.resourceClaims, that are used by this container. This field depends on the DynamicResourceAllocation feature gate. This field is immutable. It can only be set for containers. |
| `spec.services[].resources.claims[]` | object | no | — | — | ResourceClaim references one entry in PodSpec.ResourceClaims. |
| `spec.services[].resources.claims[].name` | string | yes | — | — | Name must match the name of one entry in pod.spec.resourceClaims of the Pod where this field is used. It makes that resource available inside a container. |
| `spec.services[].resources.claims[].request` | string | no | — | — | Request is the name chosen for a request in the referenced claim. If empty, everything from the claim is made available, otherwise only the result of this request. |
| `spec.services[].resources.limits` | map[string] | no | — | — | Limits describes the maximum amount of compute resources allowed. More info: https://kubernetes.io/docs/concepts/configuration/manage-resources-containers/ |
| `spec.services[].resources.limits{}` | (int-or-string) | no | — | pattern: `^(\+\|-)?(([0-9]+(\.[0-9]*)?)\|(\.[0-9]+))(([KMGTPE]i)\|[numkMGTPE]\|([eE](\+\|-)?(([0-9]+(\.[0-9]*)?)\|(\.[0-9]+))))?$` |  |
| `spec.services[].resources.requests` | map[string] | no | — | — | Requests describes the minimum amount of compute resources required. If Requests is omitted for a container, it defaults to Limits if that is explicitly specified, otherwise to an implementation-defined value. Requests cannot exceed Limits. More info: https://kubernetes.io/docs/concepts/configuration/manage-resources-containers/ |
| `spec.services[].resources.requests{}` | (int-or-string) | no | — | pattern: `^(\+\|-)?(([0-9]+(\.[0-9]*)?)\|(\.[0-9]+))(([KMGTPE]i)\|[numkMGTPE]\|([eE](\+\|-)?(([0-9]+(\.[0-9]*)?)\|(\.[0-9]+))))?$` |  |
| `spec.services[].runAsUser` | integer | no | — | format: int64 |  |
| `spec.services[].storage` | object | no | — | — | ServiceStorageSpec creates a per-service RWO data PVC, retained by name across Pod recreates. |
| `spec.services[].storage.mountPath` | string | yes | — | — | mountPath is the in-container mount point for the data PVC. |
| `spec.services[].storage.size` | string | yes | — | — | size is a quantity string, e.g. "1Gi". |

### .status

| Field | Type | Required | Default | Constraints | Description |
|---|---|---|---|---|---|
| `status.conditions` | array<object> | no | — | — |  |
| `status.conditions[]` | object | no | — | — | Condition contains details for one aspect of the current state of this API Resource. |
| `status.conditions[].lastTransitionTime` | string | yes | — | format: date-time | lastTransitionTime is the last time the condition transitioned from one status to another. This should be when the underlying condition changed. If that is not known, then using the time when the API field changed is acceptable. |
| `status.conditions[].message` | string | yes | — | maxLength: 32768 | message is a human readable message indicating details about the transition. This may be an empty string. |
| `status.conditions[].observedGeneration` | integer | no | — | minimum: 0; format: int64 | observedGeneration represents the .metadata.generation that the condition was set based upon. For instance, if .metadata.generation is currently 12, but the .status.conditions[x].observedGeneration is 9, the condition is out of date with respect to the current state of the instance. |
| `status.conditions[].reason` | string | yes | — | minLength: 1; maxLength: 1024; pattern: `^[A-Za-z]([A-Za-z0-9_,:]*[A-Za-z0-9_])?$` | reason contains a programmatic identifier indicating the reason for the condition's last transition. Producers of specific condition types may define expected values and meanings for this field, and whether the values are considered a guaranteed API. The value should be a CamelCase string. This field may not be empty. |
| `status.conditions[].status` | string | yes | — | enum: True, False, Unknown | status of the condition, one of True, False, Unknown. |
| `status.conditions[].type` | string | yes | — | maxLength: 316; pattern: `^([a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*/)?(([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9])$` | type of condition in CamelCase or in foo.example.com/CamelCase. |
| `status.entries` | array<object> | no | — | — |  |
| `status.entries[]` | object | no | — | — | EntryStatus is the readiness of one service or runtime entry. |
| `status.entries[].kind` | string | yes | — | — | kind is "service" or "runtime". |
| `status.entries[].name` | string | yes | — | — |  |
| `status.entries[].ready` | boolean | yes | — | — |  |
| `status.entries[].reason` | string | no | — | — |  |
