{{- define "haliphron-runtime.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "haliphron-runtime.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "haliphron-runtime.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "haliphron-runtime.labels" -}}
helm.sh/chart: {{ include "haliphron-runtime.chart" . }}
{{ include "haliphron-runtime.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: haliphron
{{- end -}}

{{- define "haliphron-runtime.selectorLabels" -}}
app.kubernetes.io/name: {{ include "haliphron-runtime.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: controller
{{- end -}}

{{- define "haliphron-runtime.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "haliphron-runtime.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{- define "haliphron-runtime.image" -}}
{{- $ref := .Values.image.repository -}}
{{- if .Values.image.registry -}}{{- $ref = printf "%s/%s" .Values.image.registry .Values.image.repository -}}{{- end -}}
{{- if .Values.image.digest -}}
{{- printf "%s@%s" $ref .Values.image.digest -}}
{{- else -}}
{{- printf "%s:%s" $ref (default .Chart.AppVersion .Values.image.tag) -}}
{{- end -}}
{{- end -}}

{{/*
The agents' namespace. Empty means the release's own, which the chart allows
and the NOTES warn about: the quota and the default-deny policy then apply to
the controller too.
*/}}
{{- define "haliphron-runtime.agentNamespace" -}}
{{- default .Release.Namespace .Values.agents.namespace -}}
{{- end -}}

{{- define "haliphron-runtime.agentServiceAccountName" -}}
{{- default (printf "%s-agent" (include "haliphron-runtime.fullname" .)) .Values.agents.serviceAccount.name -}}
{{- end -}}

{{- define "haliphron-runtime.callbackServiceName" -}}
{{- printf "%s-callback" (include "haliphron-runtime.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Where the pods post their report. Fully qualified, because the pod is in the
agents' namespace and the Service is not.
*/}}
{{/*
The base the pods post to. A base and not one endpoint: there are three paths
now — the completion, the phase reports and, in relay mode, the artifacts — and
the pod appends the contract's own constants to this. A CR carrying three URLs
that differ in their last segment is three chances to disagree.

A value given here that still names /completion is trimmed by the controller, so
an installation upgrading from the older shape keeps working.
*/}}
{{- define "haliphron-runtime.callbackURL" -}}
{{- if .Values.controller.callbackURL -}}
{{- trimSuffix "/" .Values.controller.callbackURL -}}
{{- else -}}
{{- printf "http://%s.%s.svc.cluster.local:%v"
      (include "haliphron-runtime.callbackServiceName" .) .Release.Namespace .Values.controller.callbackPort -}}
{{- end -}}
{{- end -}}

{{/*
Whether the chart creates a PVC for the artifact spool.
*/}}
{{- define "haliphron-runtime.spoolPVC" -}}
{{- if and .Values.controller.spool.persistence.enabled (not .Values.controller.spool.persistence.existingClaim) -}}true{{- end -}}
{{- end -}}

{{- define "haliphron-runtime.spoolClaim" -}}
{{- if .Values.controller.spool.persistence.existingClaim -}}
{{- .Values.controller.spool.persistence.existingClaim -}}
{{- else -}}
{{- printf "%s-spool" (include "haliphron-runtime.fullname" .) -}}
{{- end -}}
{{- end -}}

{{- define "haliphron-runtime.bootstrapSecretName" -}}
{{- default (printf "%s-bootstrap" (include "haliphron-runtime.fullname" .)) .Values.backend.bootstrapTokenExistingSecret -}}
{{- end -}}
{{- define "haliphron-runtime.bootstrapSecretKey" -}}
{{- if .Values.backend.bootstrapTokenExistingSecret -}}{{ .Values.backend.bootstrapTokenExistingSecretKey }}{{- else -}}token{{- end -}}
{{- end -}}

{{- define "haliphron-runtime.validate" -}}
{{- if not .Values.cluster.name -}}
{{- fail "cluster.name is required: it is the identity the control plane registers, and renaming it later registers a second cluster" -}}
{{- end -}}
{{- if not .Values.backend.url -}}
{{- fail "backend.url is required: the Cluster API as seen from inside this cluster" -}}
{{- end -}}
{{- if and (not .Values.backend.bootstrapToken) (not .Values.backend.bootstrapTokenExistingSecret) -}}
{{- fail "set backend.bootstrapTokenExistingSecret (preferred) or backend.bootstrapToken; issue one from the control plane's admin API" -}}
{{- end -}}
{{- if not (has .Values.rbac.scope (list "namespaced" "cluster")) -}}
{{- fail (printf "rbac.scope is %q; it must be namespaced or cluster" .Values.rbac.scope) -}}
{{- end -}}
{{- if not .Values.cluster.runtimes -}}
{{- fail "cluster.runtimes is empty, so this cluster would register and then never be offered any work" -}}
{{- end -}}
{{- if and .Values.crd.conversionWebhook.enabled (not .Values.crd.install) -}}
{{- fail "crd.conversionWebhook.enabled requires crd.install" -}}
{{- end -}}
{{- end -}}
