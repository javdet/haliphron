{{/*
Names. The truncation to 63 characters is the label-value limit; the trailing
"-" trim keeps a name that lands exactly on a hyphen from being invalid.
*/}}
{{- define "haliphron.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "haliphron.fullname" -}}
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

{{- define "haliphron.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "haliphron.labels" -}}
helm.sh/chart: {{ include "haliphron.chart" . }}
{{ include "haliphron.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: haliphron
{{- end -}}

{{/*
The selector. It carries the mode, so that installing the chart twice into one
namespace for a split deployment produces two Deployments whose Services do not
select each other's pods.
*/}}
{{- define "haliphron.selectorLabels" -}}
app.kubernetes.io/name: {{ include "haliphron.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: backend
haliphron.io/mode: {{ .Values.backend.mode }}
{{- end -}}

{{- define "haliphron.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "haliphron.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/*
The image. A digest wins over a tag when both are given, because a digest is
the only one of the two that names the same bytes tomorrow.
*/}}
{{- define "haliphron.image" -}}
{{- $registry := .Values.image.registry -}}
{{- $repo := .Values.image.repository -}}
{{- $ref := printf "%s/%s" $registry $repo -}}
{{- if not $registry -}}{{- $ref = $repo -}}{{- end -}}
{{- if .Values.image.digest -}}
{{- printf "%s@%s" $ref .Values.image.digest -}}
{{- else -}}
{{- printf "%s:%s" $ref (default .Chart.AppVersion .Values.image.tag) -}}
{{- end -}}
{{- end -}}

{{/*
Which listeners this mode runs. The health listener runs in every mode.
*/}}
{{- define "haliphron.servesAPI" -}}
{{- if or (eq .Values.backend.mode "all") (eq .Values.backend.mode "api") -}}true{{- end -}}
{{- end -}}
{{- define "haliphron.servesMCP" -}}
{{- if or (eq .Values.backend.mode "all") (eq .Values.backend.mode "mcp") -}}true{{- end -}}
{{- end -}}
{{- define "haliphron.servesCluster" -}}
{{- if or (eq .Values.backend.mode "all") (eq .Values.backend.mode "cluster") -}}true{{- end -}}
{{- end -}}

{{/*
The Secret this release creates for the credentials given as literals. Values
supplied through existingSecret never reach it.
*/}}
{{- define "haliphron.secretName" -}}
{{- printf "%s-credentials" (include "haliphron.fullname" .) -}}
{{- end -}}

{{/*
The DSN, in order of precedence: an existing Secret, a literal, or one derived
from the bundled PostgreSQL. Derivation is what makes `--set postgresql.enabled=true`
a complete instruction rather than half of one.
*/}}
{{- define "haliphron.derivedDSN" -}}
{{- $pg := .Values.postgresql -}}
{{- printf "postgres://%s:%s@%s-postgresql:5432/%s?sslmode=disable"
      $pg.auth.username $pg.auth.password .Release.Name $pg.auth.database -}}
{{- end -}}

{{- define "haliphron.dsnSecretName" -}}
{{- default (include "haliphron.secretName" .) .Values.database.existingSecret -}}
{{- end -}}
{{- define "haliphron.dsnSecretKey" -}}
{{- if .Values.database.existingSecret -}}{{ .Values.database.existingSecretKey }}{{- else -}}dsn{{- end -}}
{{- end -}}

{{- define "haliphron.s3SecretName" -}}
{{- default (include "haliphron.secretName" .) .Values.objectStorage.existingSecret -}}
{{- end -}}
{{- define "haliphron.s3AccessKeyKey" -}}
{{- if .Values.objectStorage.existingSecret -}}{{ .Values.objectStorage.existingSecretAccessKeyKey }}{{- else -}}accessKey{{- end -}}
{{- end -}}
{{- define "haliphron.s3SecretKeyKey" -}}
{{- if .Values.objectStorage.existingSecret -}}{{ .Values.objectStorage.existingSecretSecretKeyKey }}{{- else -}}secretKey{{- end -}}
{{- end -}}

{{- define "haliphron.kekEnabled" -}}
{{- if or .Values.encryption.key .Values.encryption.existingSecret -}}true{{- end -}}
{{- end -}}
{{- define "haliphron.kekSecretName" -}}
{{- default (include "haliphron.secretName" .) .Values.encryption.existingSecret -}}
{{- end -}}
{{- define "haliphron.kekSecretKey" -}}
{{- if .Values.encryption.existingSecret -}}{{ .Values.encryption.existingSecretKey }}{{- else -}}kek{{- end -}}
{{- end -}}

{{/*
The S3 endpoint, derived from the bundled MinIO when one is installed and no
endpoint was given. Without this, enabling the subchart would leave the backend
talking to AWS.
*/}}
{{- define "haliphron.s3Endpoint" -}}
{{- if .Values.objectStorage.endpoint -}}
{{- .Values.objectStorage.endpoint -}}
{{- else if .Values.minio.enabled -}}
{{- printf "http://%s-minio:9000" .Release.Name -}}
{{- end -}}
{{- end -}}

{{/*
What the chart refuses to render.

Every one of these surfaces at install time as a message naming the value,
rather than at runtime as a CrashLoopBackOff whose reason is one line in a log
nobody is watching yet.
*/}}
{{/*
A Gateway API duration ("120s", "2m", "0s") in seconds.

Returns -1 for anything it does not recognise, including the compound forms the
spec allows such as "1m30s". A check that cannot read the value skips rather
than guesses: refusing to install over a duration the chart misparsed would be
worse than the misconfiguration it is looking for.
*/}}
{{- define "haliphron.durationSeconds" -}}
{{- $d := toString . -}}
{{- if not (regexMatch "^[0-9]+(h|m|s|ms)$" $d) -}}-1
{{- else if hasSuffix "ms" $d -}}0
{{- else if hasSuffix "s" $d -}}{{ trimSuffix "s" $d | int }}
{{- else if hasSuffix "m" $d -}}{{ mul (trimSuffix "m" $d | int) 60 }}
{{- else if hasSuffix "h" $d -}}{{ mul (trimSuffix "h" $d | int) 3600 }}
{{- else -}}-1{{- end -}}
{{- end -}}

{{- define "haliphron.validate" -}}
{{- $mode := .Values.backend.mode -}}
{{- if not (has $mode (list "all" "api" "mcp" "cluster")) -}}
{{- fail (printf "backend.mode is %q; it must be one of all, api, mcp, cluster" $mode) -}}
{{- end -}}

{{- if not .Values.agent.image -}}
{{- fail "agent.image is required: it decides what every run executes, so the chart will not choose it for you. Pin it by digest." -}}
{{- end -}}

{{- if and (not .Values.database.dsn) (not .Values.database.existingSecret) (not .Values.postgresql.enabled) -}}
{{- fail "set database.existingSecret (preferred), database.dsn, or postgresql.enabled=true" -}}
{{- end -}}
{{- if and .Values.postgresql.enabled (not .Values.database.dsn) (not .Values.database.existingSecret) (not .Values.postgresql.auth.password) -}}
{{- fail "postgresql.enabled=true needs postgresql.auth.password, which the derived DSN is built from" -}}
{{- end -}}

{{- $s3 := .Values.objectStorage -}}
{{- if and (not $s3.existingSecret) (not $s3.accessKey) (not .Values.minio.enabled) -}}
{{- fail "set objectStorage.existingSecret (preferred), objectStorage.accessKey/secretKey, or minio.enabled=true" -}}
{{- end -}}
{{- if and .Values.minio.enabled (not $s3.existingSecret) (not $s3.accessKey) (not .Values.minio.auth.rootPassword) -}}
{{- fail "minio.enabled=true needs minio.auth.rootPassword, which becomes the backend's secret key" -}}
{{- end -}}

{{- $wait := int .Values.clusterProtocol.timings.maxWaitSeconds -}}
{{- if gt $wait 30 -}}
{{- fail (printf "clusterProtocol.timings.maxWaitSeconds is %d; the contract's ceiling is 30, and the backend refuses to start above it" $wait) -}}
{{- end -}}

{{/*
The long poll against a proxy that gives up first is the failure mode the
architecture calls out by name, and it is invisible until a cluster has been
idle for half a minute. Checked here so that it is caught by `helm install`,
whichever of the two ways the Cluster API is exposed.
*/}}
{{- $effective := $wait -}}
{{- if eq $effective 0 -}}{{- $effective = 30 -}}{{- end -}}

{{- if .Values.ingress.cluster.enabled -}}
{{- $timeout := index .Values.ingress.cluster.annotations "nginx.ingress.kubernetes.io/proxy-read-timeout" -}}
{{- if $timeout -}}
{{- if le (int $timeout) $effective -}}
{{- fail (printf "ingress.cluster proxy-read-timeout is %vs but a lease poll hangs for up to %ds; raise the timeout or the controllers will see disconnects instead of empty responses" $timeout $effective) -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
Gateway API. An HTTPRoute attaches to something; a route with no parentRef is
accepted by the API server and then does nothing at all, which is the kind of
success that costs an afternoon.
*/}}
{{- range $name := list "api" "mcp" "cluster" -}}
{{- $route := index $.Values.httpRoute $name -}}
{{- if $route.enabled -}}
{{- if not $route.parentRefs -}}
{{- fail (printf "httpRoute.%s.enabled needs httpRoute.%s.parentRefs: an HTTPRoute with no parent attaches to no Gateway and routes nothing" $name $name) -}}
{{- end -}}
{{- range $route.parentRefs -}}
{{- if not .name -}}
{{- fail (printf "every entry in httpRoute.%s.parentRefs needs a name: the Gateway to attach to" $name) -}}
{{- end -}}
{{- end -}}
{{- if not $route.matches -}}
{{- fail (printf "httpRoute.%s.matches is empty; a rule with no match is not a route" $name) -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- if .Values.httpRoute.cluster.enabled -}}
{{/* kindIs rather than dig: an omitted `timeouts` is nil, and `--set
     timeouts={}` makes it a list. Neither is a map, and dig raises on both. */}}
{{- $timeouts := .Values.httpRoute.cluster.timeouts -}}
{{- $request := "" -}}
{{- if kindIs "map" $timeouts -}}{{- $request = default "" (get $timeouts "request") -}}{{- end -}}
{{- if not $request -}}
{{/*
No timeout stated is not the same as no timeout. Several Gateway
implementations default to 15 or 30 seconds, and the resulting disconnects read
as network instability rather than as configuration.
*/}}
{{- fail (printf "httpRoute.cluster.timeouts.request is unset and a lease poll hangs for up to %ds; set it above that, or to \"0s\" for no timeout — the implementation's default is not safe to inherit here" $effective) -}}
{{- end -}}
{{- $seconds := int (include "haliphron.durationSeconds" $request) -}}
{{- if and (gt $seconds 0) (le $seconds $effective) -}}
{{- fail (printf "httpRoute.cluster.timeouts.request is %v but a lease poll hangs for up to %ds; raise it or the controllers will see disconnects instead of empty responses" $request $effective) -}}
{{- end -}}
{{- end -}}
{{- end -}}
