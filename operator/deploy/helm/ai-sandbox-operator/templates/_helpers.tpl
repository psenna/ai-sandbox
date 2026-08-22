{{- define "ai-sandbox-operator.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "ai-sandbox-operator.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{- define "ai-sandbox-operator.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
selectorLabels deliberately does NOT include control-plane=controller-manager:
Deployment.spec.selector is immutable, so the operator-ingress label stays
free-form pod metadata (see podLabels below) rather than being welded into an
unchangeable selector.
*/}}
{{- define "ai-sandbox-operator.selectorLabels" -}}
app.kubernetes.io/name: {{ include "ai-sandbox-operator.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "ai-sandbox-operator.labels" -}}
helm.sh/chart: {{ include "ai-sandbox-operator.chart" . }}
{{ include "ai-sandbox-operator.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: ai-sandbox-operator
{{- with .Values.commonLabels }}
{{ toYaml . }}
{{- end }}
{{- end }}

{{/*
podLabels is what makes guard G15 satisfiable by construction: it puts the
key/value half of .Values.network.operatorIngressLabel onto the operator's own
pod template, so the Restricted-isolation NetworkPolicy's operator-ingress peer
selector (internal/controller/network.go's operatorIngressSelector) always
matches these pods. It also keeps control-plane: controller-manager on them by
default, which test/e2e/metrics.go's OperatorPodName and
config/prometheus/monitor.yaml both select on.
*/}}
{{- define "ai-sandbox-operator.podLabels" -}}
{{- include "ai-sandbox-operator.selectorLabels" . }}
{{- $parts := splitList "=" (toString .Values.network.operatorIngressLabel) }}
{{- if eq (len $parts) 2 }}
{{ index $parts 0 }}: {{ index $parts 1 | quote }}
{{- end }}
{{- with .Values.podLabels }}
{{ toYaml . }}
{{- end }}
{{- end }}

{{- define "ai-sandbox-operator.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "ai-sandbox-operator.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
imageRef resolves the operator's own image: image.digest wins over image.tag;
image.tag falls back to Chart.AppVersion. Guard G12 lives here.
*/}}
{{- define "ai-sandbox-operator.imageRef" -}}
{{- $repo := .Values.image.repository | default "" -}}
{{- if not $repo -}}
{{- fail "ai-sandbox-operator: image.repository is required and must not be empty (set it explicitly, e.g. ghcr.io/psenna/ai-sandbox-operator, or a private registry mirror)." -}}
{{- end -}}
{{- if .Values.image.digest -}}
{{- printf "%s@%s" $repo .Values.image.digest -}}
{{- else -}}
{{- $tag := .Values.image.tag | default .Chart.AppVersion | default "" -}}
{{- if not $tag -}}
{{- fail "ai-sandbox-operator: no image tag could be resolved: image.tag is empty and Chart.yaml has no appVersion. No :latest tag is published for ghcr.io/psenna/ai-sandbox-operator, so an explicit tag is required." -}}
{{- end -}}
{{- printf "%s:%s" $repo $tag -}}
{{- end -}}
{{- end -}}

{{/*
sidecarImageRef: .Values.sidecarImage wins verbatim; otherwise the sidecar is
version-locked to the manager image this chart resolved.
*/}}
{{- define "ai-sandbox-operator.sidecarImageRef" -}}
{{- .Values.sidecarImage | default (include "ai-sandbox-operator.imageRef" .) -}}
{{- end -}}

{{/*
classSecretNamespace: explicit value, else the release namespace. The flag is
always rendered, so the binary's POD_NAMESPACE fallback is never load-bearing.
*/}}
{{- define "ai-sandbox-operator.classSecretNamespace" -}}
{{- .Values.classSecretNamespace | default .Release.Namespace -}}
{{- end -}}

{{/*
durationSeconds converts a single-term Go duration ("5s", "30m", "1h", "100ms")
to a number of seconds. Takes a dict {"value": <dur>, "path": "<values path>"}
and fail()s naming the path when it cannot parse -- which is why
values.schema.json pins the operator-flag durations to a single-term pattern.
Used only by the interval range guards (G16).
*/}}
{{- define "ai-sandbox-operator.durationSeconds" -}}
{{- $v := toString .value -}}
{{- if not (regexMatch "^[0-9]+(ns|us|ms|s|m|h)$" $v) -}}
{{- fail (printf "ai-sandbox-operator: %s=%q is not a duration this chart can range-check. Use a single number and unit, e.g. \"100ms\", \"5s\", \"30m\", \"1h\"." .path $v) -}}
{{- end -}}
{{- $num := float64 (regexFind "^[0-9]+" $v) -}}
{{- $unit := regexFind "(ns|us|ms|s|m|h)$" $v -}}
{{- if eq $unit "h" -}}{{- mulf $num 3600.0 -}}
{{- else if eq $unit "m" -}}{{- mulf $num 60.0 -}}
{{- else if eq $unit "s" -}}{{- mulf $num 1.0 -}}
{{- else if eq $unit "ms" -}}{{- divf $num 1000.0 -}}
{{- else if eq $unit "us" -}}{{- divf $num 1000000.0 -}}
{{- else -}}{{- divf $num 1000000000.0 -}}
{{- end -}}
{{- end -}}

{{/*
isInClusterHost returns a non-empty string when a URL's host is an in-cluster
<service>.<namespace>.svc[.cluster.local] name -- the only hostname form
internal/controller/network.go's resolveServiceEndpoint can turn into a pod
selector. Mirrors that file's inClusterServiceRE exactly. Used by G10.
*/}}
{{- define "ai-sandbox-operator.isInClusterHost" -}}
{{- $u := toString . -}}
{{- $hostport := regexReplaceAll "^[a-zA-Z][a-zA-Z0-9+.-]*://" $u "" | splitList "/" | first -}}
{{- $host := splitList ":" $hostport | first -}}
{{- if regexMatch "^[^.]+\\.[^.]+\\.svc(\\.cluster\\.local)?$" $host -}}
{{- print "true" -}}
{{- end -}}
{{- end -}}
