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

{{/*
Which half of the ArtifactStore port is in force. Empty means relay, which is
what an installation gets without configuration.
*/}}
{{- define "haliphron.objectStore" -}}
{{- if eq .Values.artifacts.mode "object-store" -}}true{{- end -}}
{{- end -}}

{{/*
Whether the chart creates a PVC for artifacts. Relay mode with no existing
claim; in object-store mode there is nothing to mount.
*/}}
{{- define "haliphron.artifactPVC" -}}
{{- if and (not (include "haliphron.objectStore" .)) .Values.artifacts.pvc.enabled (not .Values.artifacts.pvc.existingClaim) -}}true{{- end -}}
{{- end -}}

{{/*
The claim the backend mounts: the one the chart makes, or one that already
exists.
*/}}
{{- define "haliphron.artifactClaim" -}}
{{- if .Values.artifacts.pvc.existingClaim -}}
{{- .Values.artifacts.pvc.existingClaim -}}
{{- else -}}
{{- printf "%s-artifacts" (include "haliphron.fullname" .) -}}
{{- end -}}
{{- end -}}

{{/*
maxBytesPerRun in bytes. Written as a quantity in values because "1Gi" is what
an operator wants to type and 1073741824 is not.
*/}}
{{- define "haliphron.artifactMaxBytes" -}}
{{- include "haliphron.quantityBytes" .Values.artifacts.maxBytesPerRun -}}
{{- end -}}

{{/*
A Kubernetes quantity as a plain byte count. Binary suffixes only, because that
is what a storage value is written in; a value the chart cannot read is a
failure rather than a guess, since guessing low silently truncates every log.
*/}}
{{- define "haliphron.quantityBytes" -}}
{{- $q := toString . -}}
{{- if regexMatch "^[0-9]+$" $q -}}{{ $q }}
{{- else if hasSuffix "Ki" $q -}}{{ mul (trimSuffix "Ki" $q | int64) 1024 }}
{{- else if hasSuffix "Mi" $q -}}{{ mul (trimSuffix "Mi" $q | int64) 1048576 }}
{{- else if hasSuffix "Gi" $q -}}{{ mul (trimSuffix "Gi" $q | int64) 1073741824 }}
{{- else if hasSuffix "Ti" $q -}}{{ mul (trimSuffix "Ti" $q | int64) 1099511627776 }}
{{- else -}}{{ fail (printf "%q is not a byte quantity the chart can read; use a plain number or a Ki/Mi/Gi/Ti suffix" $q) }}
{{- end -}}
{{- end -}}

{{/*
The key encryption key.

Three ways in, in this order: a Secret that already exists, a literal in
values, and — failing both — one the chart generates on the first install and
then leaves alone. The third is the default, because the alternative was an
installation that could not use managed secrets until an operator had run
`openssl` and a `kubectl create secret` by hand, and the value that ceremony
produces is the same 32 random bytes `randBytes` produces here.

Generated or literal, it lives in a Secret of its own rather than beside the
DSN, for the reason the bootstrap token does: the lifetimes differ. A DSN can
be rotated on a Tuesday; this key can never be rotated by being replaced,
because every managed secret in the database is wrapped under it and the
database does not hold a copy. That is also why the Secret carries
resource-policy: keep — `helm uninstall` is not a reason to destroy the only
copy of the key that makes the data readable.
*/}}
{{- define "haliphron.kekEnabled" -}}
{{- if or .Values.encryption.existingSecret .Values.encryption.key .Values.encryption.autoGenerate -}}true{{- end -}}
{{- end -}}

{{/*
Whether this release writes the Secret. It does not when the key was supplied
through one that already exists: copying a credential into a second Secret
makes this release's own storage one more place it can be read from.
*/}}
{{- define "haliphron.kekOwned" -}}
{{- if and (include "haliphron.kekEnabled" .) (not .Values.encryption.existingSecret) -}}true{{- end -}}
{{- end -}}

{{- define "haliphron.kekSecretName" -}}
{{- default (printf "%s-kek" (include "haliphron.fullname" .)) .Values.encryption.existingSecret -}}
{{- end -}}
{{- define "haliphron.kekSecretKey" -}}
{{- if .Values.encryption.existingSecret -}}{{ .Values.encryption.existingSecretKey }}{{- else -}}kek{{- end -}}
{{- end -}}

{{/*
The key itself: what values state, what this release wrote last time, what an
older release of this chart wrote into the credentials Secret — and only then
32 new random bytes.

The middle two are the whole point. A key that changed on `helm upgrade` would
not rotate anything; it would leave every managed secret in the database
wrapped under bytes nobody has any more. `randBytes 32` is base64 of 32 bytes,
which is what `openssl rand -base64 32` prints and what the backend's decoder
takes as a key rather than as a passphrase.

The read-back is a cluster lookup, and `helm template` and `--dry-run` do not
do those. They render a key that is never installed, which is harmless — and
it is why an installation driven by rendered manifests (Argo CD, Flux,
`helm template | kubectl apply`) must set encryption.key or
encryption.existingSecret instead of leaving this to autoGenerate: every
render would otherwise produce a different key and the last one would win.
*/}}
{{- define "haliphron.kekValue" -}}
{{- if .Values.encryption.key -}}
{{- .Values.encryption.key -}}
{{- else -}}
{{- $mine := lookup "v1" "Secret" .Release.Namespace (include "haliphron.kekSecretName" .) -}}
{{- $legacy := lookup "v1" "Secret" .Release.Namespace (include "haliphron.secretName" .) -}}
{{- if and $mine $mine.data (index $mine.data "kek") -}}
{{- index $mine.data "kek" | b64dec -}}
{{- else if and $legacy $legacy.data (index $legacy.data "kek") -}}
{{/* Charts before the split wrote the key into the credentials Secret. */}}
{{- index $legacy.data "kek" | b64dec -}}
{{- else -}}
{{- randBytes 32 -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
Whether this render invented the key rather than finding one. NOTES says so,
because an upgrade that reaches this state has just replaced a key that data
may be wrapped under.
*/}}
{{- define "haliphron.kekFresh" -}}
{{- if (include "haliphron.kekOwned" .) -}}
{{- if not .Values.encryption.key -}}
{{- $mine := lookup "v1" "Secret" .Release.Namespace (include "haliphron.kekSecretName" .) -}}
{{- $legacy := lookup "v1" "Secret" .Release.Namespace (include "haliphron.secretName" .) -}}
{{- if not (or (and $mine $mine.data (index $mine.data "kek")) (and $legacy $legacy.data (index $legacy.data "kek"))) -}}true{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
The first admin token.

The chart generates it because nothing else can: the API refuses every request
without a bearer token, and the endpoint that issues one is admin-scoped. This
is the Grafana-shaped answer — a credential in a Secret that an operator reads
once and replaces.
*/}}
{{- define "haliphron.bootstrapEnabled" -}}
{{- if .Values.bootstrapToken.enabled -}}true{{- end -}}
{{- end -}}

{{/*
Whether this release writes the Secret. It does not when the token was
supplied through one that already exists: copying a credential into a second
Secret makes `helm get values` and this release's own storage two more places
it can be read from.
*/}}
{{- define "haliphron.bootstrapOwned" -}}
{{- if and (include "haliphron.bootstrapEnabled" .) (not .Values.bootstrapToken.existingSecret) -}}true{{- end -}}
{{- end -}}

{{- define "haliphron.bootstrapSecretName" -}}
{{- default (printf "%s-bootstrap" (include "haliphron.fullname" .)) .Values.bootstrapToken.existingSecret -}}
{{- end -}}
{{- define "haliphron.bootstrapSecretKey" -}}
{{- if .Values.bootstrapToken.existingSecret -}}{{ .Values.bootstrapToken.existingSecretKey }}{{- else -}}token{{- end -}}
{{- end -}}

{{/*
The generated value, in order: what values state, what this release wrote last
time, and only then a new random one.

The middle case is what keeps `helm upgrade` from rotating the credential on
every run — and, worse, from leaving the previous row alive in the database
while the pods move on to a token nobody read. `lookup` returns nothing during
`helm template` and `--dry-run`, so those render a value that is never
installed; that is a property of dry runs and not of the install.

`hlt_` because every other platform token carries it, and a credential that
does not look like one is a credential somebody pastes into the wrong field.
*/}}
{{- define "haliphron.bootstrapToken" -}}
{{- if .Values.bootstrapToken.value -}}
{{- .Values.bootstrapToken.value -}}
{{- else -}}
{{- $key := include "haliphron.bootstrapSecretKey" . -}}
{{- $existing := lookup "v1" "Secret" .Release.Namespace (include "haliphron.bootstrapSecretName" .) -}}
{{- if and $existing $existing.data (index $existing.data $key) -}}
{{- index $existing.data $key | b64dec -}}
{{- else -}}
{{- printf "hlt_%s" (randAlphaNum 32) -}}
{{- end -}}
{{- end -}}
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

{{- $artifactMode := .Values.artifacts.mode -}}
{{- if not (has $artifactMode (list "relay" "object-store")) -}}
{{- fail (printf "artifacts.mode is %q; it must be relay or object-store" $artifactMode) -}}
{{- end -}}

{{- if eq $artifactMode "object-store" -}}
{{/*
Object-store mode without a bucket is the failure that is expensive to find
late: the backend refuses to start, and an operator who works around that by
disabling the check gets runs that work for an hour and cannot upload.
*/}}
{{- if and (not .Values.objectStorage.accessKey) (not .Values.objectStorage.existingSecret) (not .Values.minio.enabled) -}}
{{- fail "artifacts.mode is object-store but no object storage is configured: set objectStorage.accessKey/secretKey, or objectStorage.existingSecret, or minio.enabled — or leave artifacts.mode at its default of relay, which needs no object store at all" -}}
{{- end -}}
{{- else -}}
{{/*
Relay mode puts the backend in the artifact data path, and a ReadWriteOnce
volume can be mounted for writing by one node at a time. Two replicas against
one RWO claim is not a degraded configuration — the second pod does not
schedule — so it is refused here rather than discovered as a rollout that never
completes.
*/}}
{{- if and .Values.artifacts.pvc.enabled (gt (int .Values.backend.replicaCount) 1) (eq .Values.artifacts.pvc.accessMode "ReadWriteOnce") -}}
{{- fail (printf "backend.replicaCount is %d and artifacts.pvc.accessMode is ReadWriteOnce, which one node may mount for writing: set artifacts.pvc.accessMode to ReadWriteMany if your storage class offers it, or switch artifacts.mode to object-store, or run one replica" (int .Values.backend.replicaCount)) -}}
{{- end -}}
{{- if and (not .Values.artifacts.pvc.enabled) (not .Values.artifacts.pvc.existingClaim) -}}
{{/*
No volume at all means results land on the container filesystem and go with the
next restart. Allowed, because it is exactly right for a CI run of the chart,
and refused for anything with persistence turned off by accident.
*/}}
{{- fail "artifacts.mode is relay and artifacts.pvc is disabled with no existingClaim: results would be written to the container filesystem and lost on restart. Set artifacts.pvc.enabled, or artifacts.pvc.existingClaim, or switch to artifacts.mode=object-store" -}}
{{- end -}}
{{- end -}}

{{- if not .Values.agent.image -}}
{{- fail "agent.image is required: it decides what every run executes, so the chart will not choose it for you. Pin it by digest." -}}
{{- end -}}

{{/*
Two ways of supplying the first admin token is one too many: the chart would
write `value` into a Secret that the pods then do not read, and the credential
an operator copied out is not the one that works.
*/}}
{{- if and .Values.bootstrapToken.value .Values.bootstrapToken.existingSecret -}}
{{- fail "bootstrapToken.value and bootstrapToken.existingSecret are both set; the pods read the existing Secret, so the value would be written and never used. Keep one." -}}
{{- end -}}
{{/*
A token shorter than the backend's floor is refused at startup, which is a
CrashLoopBackOff whose reason is one line in a log nobody is watching yet.
*/}}
{{- if and .Values.bootstrapToken.value (lt (len .Values.bootstrapToken.value) 16) -}}
{{- fail (printf "bootstrapToken.value is %d characters; the backend refuses anything under 16, because this is an admin credential reachable over the network. Leave it empty to have one generated." (len .Values.bootstrapToken.value)) -}}
{{- end -}}

{{/*
Same contradiction, on the key encryption key: the pods read the existing
Secret, so the literal would be written into a Secret nothing mounts. Worse
than wasted — an operator who believes the key is the one they typed backs up
the wrong value.
*/}}
{{- if and .Values.encryption.key .Values.encryption.existingSecret -}}
{{- fail "encryption.key and encryption.existingSecret are both set; the pods read the existing Secret, so the key would be written and never used. Keep one." -}}
{{- end -}}
{{/*
The backend derives a key from anything of 16 characters or more that is not
32 bytes of base64 or hex, and refuses what is shorter. Refused here instead,
where the message names the value.
*/}}
{{- if and .Values.encryption.key (lt (len .Values.encryption.key) 16) -}}
{{- fail (printf "encryption.key is %d characters; the backend refuses anything under 16. Leave it empty to have 32 random bytes generated, or supply `openssl rand -base64 32`." (len .Values.encryption.key)) -}}
{{- end -}}

{{- if and (not .Values.database.dsn) (not .Values.database.existingSecret) (not .Values.postgresql.enabled) -}}
{{- fail "set database.existingSecret (preferred), database.dsn, or postgresql.enabled=true" -}}
{{- end -}}
{{- if and .Values.postgresql.enabled (not .Values.database.dsn) (not .Values.database.existingSecret) (not .Values.postgresql.auth.password) -}}
{{- fail "postgresql.enabled=true needs postgresql.auth.password, which the derived DSN is built from" -}}
{{- end -}}

{{/*
Object storage is checked only when it is used. The unconditional check that
stood here was correct when a bucket was mandatory, and it is now the thing that
would stop `helm install` with nothing set — which is the whole point of the
default mode. Object-store mode's own check is above, next to the other artifact
validation.
*/}}
{{- $s3 := .Values.objectStorage -}}
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
