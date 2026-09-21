{{/*
The UI's own names and labels.

They are separate from the backend's because the two are separate Deployments
with separate images and separate rollouts; sharing a selector would make a
frontend upgrade restart the control plane.
*/}}
{{- define "haliphron.frontend.fullname" -}}
{{- printf "%s-frontend" (include "haliphron.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "haliphron.frontend.selectorLabels" -}}
app.kubernetes.io/name: {{ include "haliphron.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: frontend
{{- end -}}

{{- define "haliphron.frontend.labels" -}}
helm.sh/chart: {{ include "haliphron.chart" . }}
{{ include "haliphron.frontend.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: haliphron
{{- end -}}

{{- define "haliphron.frontend.image" -}}
{{- $img := .Values.frontend.image -}}
{{- $ref := printf "%s/%s" $img.registry $img.repository -}}
{{- if not $img.registry -}}{{- $ref = $img.repository -}}{{- end -}}
{{- if $img.digest -}}
{{- printf "%s@%s" $ref $img.digest -}}
{{- else -}}
{{- printf "%s:%s" $ref (default .Chart.AppVersion $img.tag) -}}
{{- end -}}
{{- end -}}

{{/*
Whether the UI is installed.

Only alongside an API listener: the frontend is a static bundle plus a proxy to
/api, and in a split deployment the mcp or cluster release has nothing for it
to proxy to. Asking for it there is a mistake worth failing the render over
rather than shipping a UI whose every request 502s.
*/}}
{{- define "haliphron.frontend.enabled" -}}
{{- if .Values.frontend.enabled -}}
{{- if not (include "haliphron.servesAPI" .) -}}
{{- fail "frontend.enabled requires an API listener: set backend.mode to all or api, or install the UI with the release that serves the API" -}}
{{- end -}}
true
{{- end -}}
{{- end -}}

{{/*
Where the UI's nginx sends /api. The API Service of this same release, by
name, so the two move together.
*/}}
{{- define "haliphron.frontend.backendOrigin" -}}
{{- printf "http://%s:%v" (include "haliphron.fullname" .) .Values.service.ports.api -}}
{{- end -}}

{{/*
The Service an entrypoint's route points at.

For the API entrypoint with the UI installed that is the frontend, which serves
the page and proxies /api to the backend behind it — one host, one origin, and
therefore no CORS. Without the UI it is the backend directly.
*/}}
{{- define "haliphron.apiRouteService" -}}
{{- if include "haliphron.frontend.enabled" . -}}
{{- include "haliphron.frontend.fullname" . -}}
{{- else -}}
{{- include "haliphron.fullname" . -}}
{{- end -}}
{{- end -}}

{{- define "haliphron.apiRoutePort" -}}
{{- if include "haliphron.frontend.enabled" . -}}
{{- .Values.frontend.service.port -}}
{{- else -}}
{{- .Values.service.ports.api -}}
{{- end -}}
{{- end -}}
