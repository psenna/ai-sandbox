{{/*
Every fail() in this file follows one house style:

  ai-sandbox-operator: <what is wrong>. <why, quoting the Go source that would
  crash>. <how to fix>.

The chart's CI guard matrix greps each message by an exact substring, so the
wording here is load-bearing -- do not paraphrase a message without updating
.github/workflows/helm.yml's needle for that guard.

The single entry point below is included as line 1 of BOTH deployment.yaml and
sandboxclass-default.yaml, so `helm template -s <either>` still fires every
guard.
*/}}

{{/*
validateInterval is G16's shared body: one range check against
internal/config's Validate(), quoting that function's own error message.
dict keys: path, value, flag, min, max (seconds, float), minLabel, maxLabel.
*/}}
{{- define "ai-sandbox-operator.validateInterval" -}}
{{- $secs := float64 (include "ai-sandbox-operator.durationSeconds" (dict "value" .value "path" .path)) -}}
{{- if or (lt $secs .min) (gt $secs .max) -}}
{{- fail (printf "ai-sandbox-operator: %s=%q is outside the range the manager accepts. internal/config's Validate() returns \"%s: must be between %s and %s, got %s\" and cmd/main.go exits 2 before the manager is built, so the pod crash-loops. Set %s to a value between %s and %s." .path (toString .value) .flag .minLabel .maxLabel (toString .value) .path .minLabel .maxLabel) -}}
{{- end -}}
{{- end -}}

{{- define "ai-sandbox-operator.validate" -}}

{{/* ------------------------------------------------------------------ */}}
{{/* G1 -- slot capacity below 1                                        */}}
{{/* ------------------------------------------------------------------ */}}
{{- if lt (int .Values.slots.capacity) 1 -}}
{{- fail (printf "ai-sandbox-operator: slots.capacity=%d is not supported. The manager refuses to start: internal/config's Validate() returns \"slot-capacity: must be >= 1, got %d\" and cmd/main.go exits 2 before the manager is ever built, so the pod crash-loops with no reconciliation at all. Set slots.capacity to at least 1." (int .Values.slots.capacity) (int .Values.slots.capacity)) -}}
{{- end -}}

{{/* ------------------------------------------------------------------ */}}
{{/* G12 -- unresolvable image tag (lives in imageRef; forced here so it */}}
{{/* fires from sandboxclass-default.yaml too). Also covers the sidecar. */}}
{{/* ------------------------------------------------------------------ */}}
{{- $_ := include "ai-sandbox-operator.imageRef" . -}}
{{- $_ = include "ai-sandbox-operator.sidecarImageRef" . -}}

{{/* ------------------------------------------------------------------ */}}
{{/* G13 -- clusterID / classSecretNamespace must be DNS-1123 labels    */}}
{{/* ------------------------------------------------------------------ */}}
{{- if not (regexMatch "^[a-z0-9]([-a-z0-9]*[a-z0-9])?$" (toString .Values.clusterID)) -}}
{{- fail (printf "ai-sandbox-operator: clusterID=%q is not a valid DNS-1123 label. The manager refuses to start: internal/config's Validate() returns \"cluster-id: %q is not a valid DNS-1123 label (lowercase alphanumeric and '-')\" and cmd/main.go exits 2, so the pod crash-loops. Use lowercase alphanumerics and '-', starting and ending alphanumeric." (toString .Values.clusterID) (toString .Values.clusterID)) -}}
{{- end -}}
{{- if .Values.classSecretNamespace -}}
{{- if not (regexMatch "^[a-z0-9]([-a-z0-9]*[a-z0-9])?$" (toString .Values.classSecretNamespace)) -}}
{{- fail (printf "ai-sandbox-operator: classSecretNamespace=%q is not a valid DNS-1123 label. The manager refuses to start: internal/config's Validate() returns \"class-secret-namespace: %q is not a valid DNS-1123 label (lowercase alphanumeric and '-')\" and cmd/main.go exits 2, so the pod crash-loops. Use lowercase alphanumerics and '-', starting and ending alphanumeric." (toString .Values.classSecretNamespace) (toString .Values.classSecretNamespace)) -}}
{{- end -}}
{{- end -}}

{{/* ------------------------------------------------------------------ */}}
{{/* G14 -- class Secrets in a foreign namespace                        */}}
{{/* ------------------------------------------------------------------ */}}
{{- if and .Values.classSecretNamespace (ne (toString .Values.classSecretNamespace) .Release.Namespace) (not .Values.unsafeAllowForeignClassSecretNamespace) -}}
{{- fail (printf "ai-sandbox-operator: classSecretNamespace=%q differs from the release namespace %q. internal/controller/network.go's resolveNetworkPeers passes it as the NAMESPACE of the operator-ingress peer in every Restricted-isolation NetworkPolicy, so pointing it elsewhere silently blocks the operator's own probes from reaching sandbox pods. Leave classSecretNamespace empty to use the release namespace, or set unsafeAllowForeignClassSecretNamespace=true if you have independently arranged operator-to-sandbox reachability." (toString .Values.classSecretNamespace) .Release.Namespace) -}}
{{- end -}}

{{/* ------------------------------------------------------------------ */}}
{{/* G15 -- operatorIngressLabel must be one key=value pair             */}}
{{/* ------------------------------------------------------------------ */}}
{{- if not (regexMatch "^[^=,[:space:]]+=[^=,[:space:]]+$" (toString .Values.network.operatorIngressLabel)) -}}
{{- fail (printf "ai-sandbox-operator: network.operatorIngressLabel=%q must be a single \"key=value\" pair. internal/controller/network.go's operatorIngressSelector strings.Cut()s it on \"=\" and silently falls back to control-plane=controller-manager when it cannot, so a malformed value produces a NetworkPolicy that does not match this chart's own pods. Use a form like \"control-plane=controller-manager\"." (toString .Values.network.operatorIngressLabel)) -}}
{{- end -}}

{{/* ------------------------------------------------------------------ */}}
{{/* G16 -- intervals outside Validate()'s accepted range               */}}
{{/* ------------------------------------------------------------------ */}}
{{- include "ai-sandbox-operator.validateInterval" (dict "path" "slots.schedulerInterval" "value" .Values.slots.schedulerInterval "flag" "scheduler-interval" "min" 0.1 "max" 300.0 "minLabel" "100ms" "maxLabel" "5m") -}}
{{- include "ai-sandbox-operator.validateInterval" (dict "path" "warmCache.gcInterval" "value" .Values.warmCache.gcInterval "flag" "warm-cache-gc-interval" "min" 1.0 "max" 3600.0 "minLabel" "1s" "maxLabel" "1h") -}}
{{- include "ai-sandbox-operator.validateInterval" (dict "path" "cniProbe.interval" "value" .Values.cniProbe.interval "flag" "cni-probe-interval" "min" 1.0 "max" 3600.0 "minLabel" "1s" "maxLabel" "1h") -}}
{{- include "ai-sandbox-operator.validateInterval" (dict "path" "retention.gcInterval" "value" .Values.retention.gcInterval "flag" "retention-gc-interval" "min" 1.0 "max" 3600.0 "minLabel" "1s" "maxLabel" "1h") -}}
{{- include "ai-sandbox-operator.validateInterval" (dict "path" "metrics.collectInterval" "value" .Values.metrics.collectInterval "flag" "metrics-collect-interval" "min" 1.0 "max" 300.0 "minLabel" "1s" "maxLabel" "5m") -}}
{{- $ttl := float64 (include "ai-sandbox-operator.durationSeconds" (dict "value" .Values.retention.ttl "path" "retention.ttl")) -}}
{{- if lt $ttl 0.0 -}}
{{- fail (printf "ai-sandbox-operator: retention.ttl=%q is outside the range the manager accepts. internal/config's Validate() returns \"retention-ttl: must be >= 0, got %s\" and cmd/main.go exits 2 before the manager is built, so the pod crash-loops. Set retention.ttl to a value >= 0." (toString .Values.retention.ttl) (toString .Values.retention.ttl)) -}}
{{- end -}}

{{/* ------------------------------------------------------------------ */}}
{{/* G17 -- log verbosity out of range                                  */}}
{{/* ------------------------------------------------------------------ */}}
{{- if or (lt (int .Values.logging.verbosity) 0) (gt (int .Values.logging.verbosity) 4) -}}
{{- fail (printf "ai-sandbox-operator: logging.verbosity=%d is outside 0-4. internal/config's Validate() returns \"log-verbosity: must be between 0 and 4, got %d\" and cmd/main.go exits 2, so the pod crash-loops." (int .Values.logging.verbosity) (int .Values.logging.verbosity)) -}}
{{- end -}}

{{/* ------------------------------------------------------------------ */}}
{{/* G18 -- ServiceMonitor without a metrics listener                   */}}
{{/* ------------------------------------------------------------------ */}}
{{- if and .Values.metrics.serviceMonitor.enabled (not .Values.metrics.enabled) -}}
{{- fail "ai-sandbox-operator: metrics.serviceMonitor.enabled=true requires metrics.enabled=true. With metrics.enabled=false the chart passes --metrics-bind-address=0 -- no listener is bound, no Service is rendered, and the ServiceMonitor would target a Service that does not exist. Enable metrics, or disable the ServiceMonitor." -}}
{{- end -}}

{{/* ------------------------------------------------------------------ */}}
{{/* G19 -- multiple replicas with leader election off                  */}}
{{/* ------------------------------------------------------------------ */}}
{{- if and (gt (int .Values.replicaCount) 1) (not .Values.leaderElection.enabled) -}}
{{- fail (printf "ai-sandbox-operator: replicaCount=%d with leaderElection.enabled=false. The SlotScheduler, WarmCacheGC, CNIProbe and RetentionGC runnables are all leader-elected precisely because a double-run double-grants slots and double-deletes storage. Running more than one replica without leader election will over-admit sandboxes past slots.capacity and race archive deletion. Set replicaCount=1, or leave leaderElection.enabled=true." (int .Values.replicaCount)) -}}
{{- end -}}

{{/* ------------------------------------------------------------------ */}}
{{/* G20 -- empty default class name                                    */}}
{{/* ------------------------------------------------------------------ */}}
{{- if not .Values.defaultClass.name -}}
{{- fail "ai-sandbox-operator: defaultClass.name must not be empty. It is passed as --default-sandbox-class, and internal/config's Validate() returns \"default-sandbox-class: must not be empty\", so the manager exits 2 at startup." -}}
{{- end -}}

{{/* ------------------------------------------------------------------ */}}
{{/* G6 -- an engine the sandbox namespaces' PSA level cannot run.      */}}
{{/* Not gated on defaultClass.create: the declaration is about the     */}}
{{/* engine these values ask for, whoever creates the class.            */}}
{{/* ------------------------------------------------------------------ */}}
{{- if and (eq (toString .Values.defaultClass.engine.type) "rootless-podman") (has (toString .Values.sandboxNamespaces.podSecurityEnforce) (list "baseline" "restricted")) -}}
{{- fail (printf "ai-sandbox-operator: defaultClass.engine.type=\"rootless-podman\" cannot run in namespaces enforcing Pod Security Standard %q (sandboxNamespaces.podSecurityEnforce). \"baseline\" rejects the pod on TWO independent grounds -- an AppArmor Unconfined profile and an explicit seccompProfile.type=Unconfined, both of which internal/render/engine_podman.go's podmanRelaxations requires -- and \"restricted\" rejects those same two plus allowPrivilegeEscalation=true (the third relaxation) and the absence of capabilities.drop=[ALL] (the sidecar's base securityContext deliberately sets no capabilities field at all). Supported values are \"\" (no PSS enforcement) and \"privileged\". Either label the sandbox namespaces pod-security.kubernetes.io/enforce=privileged and set sandboxNamespaces.podSecurityEnforce accordingly, or set defaultClass.engine.type=none." (toString .Values.sandboxNamespaces.podSecurityEnforce)) -}}
{{- end -}}

{{/* ------------------------------------------------------------------ */}}
{{/* G21 -- rootless-podman with an image override that is not          */}}
{{/* digest-pinned. Not gated on defaultClass.create, like G6: this is  */}}
{{/* about the engine image these values ask for.                       */}}
{{/* ------------------------------------------------------------------ */}}
{{- if and (eq (toString .Values.defaultClass.engine.type) "rootless-podman") .Values.defaultClass.engine.image -}}
{{- if not (contains "@sha256:" (toString .Values.defaultClass.engine.image)) -}}
{{- fail (printf "ai-sandbox-operator: defaultClass.engine.image=%q is not pinned by digest. internal/render/engine_podman.go's validatePodmanImage returns \"engine \\\"rootless-podman\\\" image %q is not pinned by digest\" and RenderPod fails at render time, so no pod is ever created. The securityContext this engine requires was established empirically against podman 5.8.2 (operator/docs/spike-rootless-podman.md) and a version change can invalidate it. Use a repository@sha256:... reference, or leave defaultClass.engine.image empty to get the operator's own pinned default." (toString .Values.defaultClass.engine.image) (toString .Values.defaultClass.engine.image)) -}}
{{- end -}}
{{- end -}}

{{/* ================================================================== */}}
{{/* Guards that only apply to the SandboxClass this chart may create.  */}}
{{/* ================================================================== */}}
{{- if .Values.defaultClass.create -}}
{{- $dc := .Values.defaultClass -}}

{{/* G11 -- default class with no agent image */}}
{{- if not $dc.agent.image -}}
{{- fail "ai-sandbox-operator: defaultClass.create=true requires defaultClass.agent.image. AgentSpec.Image is non-optional on the CRD with MinLength=1. Set defaultClass.agent.image to the agent container image sandboxes run." -}}
{{- end -}}

{{/* G2/G4/G5 -- s3 backend completeness; G3 -- pvc backend completeness */}}
{{- if eq (toString $dc.storage.backend.type) "s3" -}}
{{- if or (not $dc.storage.backend.s3.endpoint) (not $dc.storage.backend.s3.bucket) -}}
{{- fail "ai-sandbox-operator: defaultClass.storage.backend.type=\"s3\" requires defaultClass.storage.backend.s3.endpoint and .bucket. The SandboxClass CRD enforces this server-side (\"storage.backend.s3 is required when type is 's3'\", api/v1alpha1/sandboxclass_types.go's BackendSpec XValidation), so the API server would reject the class this chart is about to create. Configure defaultClass.storage.backend.s3, or set defaultClass.storage.backend.type=pvc." -}}
{{- end -}}
{{- if not $dc.storage.backend.s3.credentialsSecretRef.name -}}
{{- fail "ai-sandbox-operator: defaultClass.storage.backend.type=\"s3\" requires defaultClass.storage.backend.s3.credentialsSecretRef.name. The chart never creates Secrets; the operator reads the FIXED keys \"accessKeyID\" and \"secretAccessKey\" (and optionally \"sessionToken\") out of a pre-existing Secret in the class-secret namespace -- see internal/storage/credentials.go and internal/controller/resources.go's resolveCredentials, which blocks every environment at Pending with \"reading class-referenced s3 credentials secret\" until it exists. Create the Secret and set credentialsSecretRef.name to it." -}}
{{- end -}}
{{- if and $dc.storage.backend.s3.bucket (lt (len (toString $dc.storage.backend.s3.bucket)) 3) -}}
{{- fail (printf "ai-sandbox-operator: defaultClass.storage.backend.s3.bucket=%q is shorter than 3 characters, which the SandboxClass CRD rejects server-side (S3Backend.Bucket has MinLength=3). Use the real bucket name." (toString $dc.storage.backend.s3.bucket)) -}}
{{- end -}}
{{- else if eq (toString $dc.storage.backend.type) "pvc" -}}
{{- if not $dc.storage.backend.pvc.claimName -}}
{{- fail "ai-sandbox-operator: defaultClass.storage.backend.type=\"pvc\" requires defaultClass.storage.backend.pvc.claimName. The SandboxClass CRD enforces this server-side and PVCBackend.ClaimName has MinLength=1. Set defaultClass.storage.backend.pvc.claimName to a PersistentVolumeClaim that exists in every namespace sandboxes run in." -}}
{{- end -}}
{{- end -}}

{{/* G7 -- gitProxy enabled but not configured */}}
{{- if $dc.services.gitProxy.enabled -}}
{{- if or (not $dc.services.gitProxy.gitURL) (not $dc.services.gitProxy.brokerURL) (not $dc.services.gitProxy.tokenSecretRef.name) -}}
{{- fail "ai-sandbox-operator: defaultClass.services.gitProxy.enabled=true requires gitURL, brokerURL and tokenSecretRef.name. All three are non-optional on the CRD, and internal/controller/resources.go's resolveCredentials blocks every environment at Pending until the named Secret exists. Fill all three in, or set defaultClass.services.gitProxy.enabled=false." -}}
{{- end -}}
{{- end -}}

{{/* G8 -- dependaProxy enabled but not configured */}}
{{- if $dc.services.dependaProxy.enabled -}}
{{- if and (not $dc.services.dependaProxy.npmURL) (not $dc.services.dependaProxy.pypiURL) (not $dc.services.dependaProxy.goproxyURL) -}}
{{- fail "ai-sandbox-operator: defaultClass.services.dependaProxy.enabled=true but npmURL, pypiURL and goproxyURL are all empty. That renders an empty dependaProxy block, which grants sandboxes nothing. Set at least one of npmURL/pypiURL/goproxyURL, or set defaultClass.services.dependaProxy.enabled=false." -}}
{{- end -}}
{{- end -}}

{{/* G9 -- ollama enabled but not configured */}}
{{- if $dc.services.ollama.enabled -}}
{{- if not $dc.services.ollama.baseURL -}}
{{- fail "ai-sandbox-operator: defaultClass.services.ollama.enabled=true requires defaultClass.services.ollama.baseURL. OllamaService.BaseURL is non-optional on the CRD. Set baseURL, or set defaultClass.services.ollama.enabled=false." -}}
{{- end -}}
{{- end -}}

{{/* --------------------------------------------------------------- */}}
{{/* G10 -- Restricted isolation with an off-cluster endpoint and no  */}}
{{/* covering CIDR. Checked over exactly the endpoint set             */}}
{{/* internal/controller/network.go's serviceEndpoints() enumerates.  */}}
{{/* --------------------------------------------------------------- */}}
{{- if eq (toString $dc.network.isolation) "Restricted" -}}
{{- $hasCIDR := false -}}
{{- range $dc.network.extraEgress -}}
{{- if .cidr -}}{{- $hasCIDR = true -}}{{- end -}}
{{- end -}}
{{- if not $hasCIDR -}}
{{- $eps := list -}}
{{- if $dc.services.gitProxy.enabled -}}
{{- $eps = append $eps (dict "what" "gitProxy git" "url" $dc.services.gitProxy.gitURL) -}}
{{- $eps = append $eps (dict "what" "gitProxy broker" "url" $dc.services.gitProxy.brokerURL) -}}
{{- end -}}
{{- if $dc.services.dependaProxy.enabled -}}
{{- $eps = append $eps (dict "what" "dependaProxy npm" "url" $dc.services.dependaProxy.npmURL) -}}
{{- $eps = append $eps (dict "what" "dependaProxy pypi" "url" $dc.services.dependaProxy.pypiURL) -}}
{{- $eps = append $eps (dict "what" "dependaProxy goproxy" "url" $dc.services.dependaProxy.goproxyURL) -}}
{{- end -}}
{{- if $dc.services.registryMirror.enabled -}}
{{- $eps = append $eps (dict "what" "registryMirror" "url" $dc.services.registryMirror.url) -}}
{{- end -}}
{{- if $dc.services.ollama.enabled -}}
{{- $eps = append $eps (dict "what" "ollama" "url" $dc.services.ollama.baseURL) -}}
{{- end -}}
{{- if eq (toString $dc.storage.backend.type) "s3" -}}
{{- $eps = append $eps (dict "what" "storage s3" "url" $dc.storage.backend.s3.endpoint) -}}
{{- end -}}
{{- range $eps -}}
{{- if .url -}}
{{- if not (include "ai-sandbox-operator.isInClusterHost" .url) -}}
{{- fail (printf "ai-sandbox-operator: defaultClass.network.isolation=\"Restricted\" with the %s endpoint %q, which is not an in-cluster <service>.<namespace>.svc host, but defaultClass.network.extraEgress declares no cidr peer. internal/controller/network.go's resolveServiceEndpoint fails the class outright -- every environment using this class stalls at Pending. Add a covering defaultClass.network.extraEgress entry with a cidr, point the endpoint at an in-cluster Service, or set defaultClass.network.isolation=Open." .what (toString .url)) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/* ------------------------------------------------------------------ */}}
{{/* G22 -- rootless-podman + Restricted isolation with no way to reach  */}}
{{/* a container registry (neither a registryMirror nor a covering      */}}
{{/* extraEgress cidr).                                                  */}}
{{/* ------------------------------------------------------------------ */}}
{{- if and (eq (toString $dc.engine.type) "rootless-podman") (eq (toString $dc.network.isolation) "Restricted") -}}
{{- $hasCIDR := false -}}
{{- range $dc.network.extraEgress -}}
{{- if .cidr -}}{{- $hasCIDR = true -}}{{- end -}}
{{- end -}}
{{- if and (not $dc.services.registryMirror.enabled) (not $hasCIDR) -}}
{{- fail "ai-sandbox-operator: defaultClass.engine.type=\"rootless-podman\" with network.isolation=\"Restricted\" needs a way to reach a container registry, and this class declares neither defaultClass.services.registryMirror nor an extraEgress cidr. internal/render/networkpolicy.go's RenderNetworkPolicy default-denies egress except to the class's declared peers, so the podman sidecar could pull no image at all and every `docker run` inside the sandbox would fail with a registry timeout. Set defaultClass.services.registryMirror.enabled=true with a url, add a covering extraEgress cidr, or set defaultClass.network.isolation=Open." -}}
{{- end -}}
{{- end -}}

{{- end -}}
{{- end -}}
