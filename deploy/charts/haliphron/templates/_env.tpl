{{/*
The backend's environment.

It is one template rather than inline in the Deployment because the split
deployment installs this chart more than once and every copy has to agree, and
because a value that is only sometimes emitted — a timing left at 0, a KEK that
is not configured — is easier to get right in one place than in three.

Credentials are never values here. Each is a secretKeyRef, and the key
encryption key is not even that: it is a file, because a variable is visible in
`kubectl describe pod`.
*/}}
{{- define "haliphron.env" -}}
- name: HALIPHRON_MODE
  value: {{ .Values.backend.mode | quote }}
- name: HALIPHRON_PUBLIC_ADDR
  value: ":{{ .Values.service.ports.api }}"
- name: HALIPHRON_MCP_ADDR
  value: ":{{ .Values.service.ports.mcp }}"
- name: HALIPHRON_CLUSTER_ADDR
  value: ":{{ .Values.service.ports.cluster }}"
- name: HALIPHRON_METRICS_ADDR
  value: ":{{ .Values.service.ports.health }}"

- name: HALIPHRON_DSN
  valueFrom:
    secretKeyRef:
      name: {{ include "haliphron.dsnSecretName" . }}
      key: {{ include "haliphron.dsnSecretKey" . }}
- name: HALIPHRON_DB_MAX_CONNS
  value: {{ .Values.database.maxConns | quote }}
- name: HALIPHRON_DB_IDLE_CONNS
  value: {{ .Values.database.idleConns | quote }}
- name: HALIPHRON_DB_CONN_LIFETIME
  value: {{ .Values.database.connLifetime | quote }}
- name: HALIPHRON_MIGRATE
  value: {{ .Values.backend.migrate | quote }}

- name: HALIPHRON_ARTIFACT_MODE
  value: {{ .Values.artifacts.mode | quote }}
- name: HALIPHRON_ARTIFACT_PATH
  value: {{ .Values.artifacts.mountPath | quote }}
- name: HALIPHRON_ARTIFACT_MAX_BYTES_PER_RUN
  value: {{ include "haliphron.artifactMaxBytes" . | quote }}
- name: HALIPHRON_ARTIFACT_RETAIN_LOGS
  value: {{ .Values.artifacts.retention.logs | quote }}
- name: HALIPHRON_ARTIFACT_RETAIN_RESULTS
  value: {{ .Values.artifacts.retention.results | quote }}
- name: HALIPHRON_ARTIFACT_RETAIN_ARTIFACTS
  value: {{ .Values.artifacts.retention.artifacts | quote }}

{{- if (include "haliphron.objectStore" .) }}
{{- /*
  Object-store mode only. In relay mode none of these are set — not even as
  empty strings — because the backend refuses object-store mode without
  credentials, and a chart that always supplied blank ones would turn that
  refusal into a run that fails its upload an hour later.
*/}}
- name: HALIPHRON_S3_BUCKET
  value: {{ .Values.objectStorage.bucket | quote }}
- name: HALIPHRON_S3_REGION
  value: {{ .Values.objectStorage.region | quote }}
{{- with (include "haliphron.s3Endpoint" .) }}
- name: HALIPHRON_S3_ENDPOINT
  value: {{ . | quote }}
{{- end }}
- name: HALIPHRON_S3_PATH_STYLE
  value: {{ or .Values.objectStorage.pathStyle .Values.minio.enabled | quote }}
- name: HALIPHRON_S3_ACCESS_KEY
  valueFrom:
    secretKeyRef:
      name: {{ include "haliphron.s3SecretName" . }}
      key: {{ include "haliphron.s3AccessKeyKey" . }}
- name: HALIPHRON_S3_SECRET_KEY
  valueFrom:
    secretKeyRef:
      name: {{ include "haliphron.s3SecretName" . }}
      key: {{ include "haliphron.s3SecretKeyKey" . }}
{{- if .Values.objectStorage.sessionToken }}
- name: HALIPHRON_S3_SESSION_TOKEN
  valueFrom:
    secretKeyRef:
      name: {{ include "haliphron.secretName" . }}
      key: sessionToken
{{- end }}
{{- end }}

{{- if (include "haliphron.kekEnabled" .) }}
- name: HALIPHRON_KEK_ID
  value: {{ .Values.encryption.keyId | quote }}
- name: HALIPHRON_KEK_FILE
  value: /haliphron/kek/{{ include "haliphron.kekSecretKey" . }}
{{- end }}

- name: HALIPHRON_AGENT_IMAGE
  value: {{ .Values.agent.image | quote }}
- name: HALIPHRON_AGENT_IMAGE_PULL_POLICY
  value: {{ .Values.agent.imagePullPolicy | quote }}
- name: HALIPHRON_DEFAULT_MODEL
  value: {{ .Values.agent.defaultModel | quote }}
- name: HALIPHRON_DEFAULT_AGENT
  value: {{ .Values.agent.defaultAgentType | quote }}
- name: HALIPHRON_DEFAULT_TIMEOUT_SECONDS
  value: {{ .Values.agent.timeoutSeconds | quote }}
- name: HALIPHRON_RUN_TTL_SECONDS
  value: {{ .Values.agent.ttlSeconds | quote }}
- name: HALIPHRON_MAX_INFRA_RETRIES
  value: {{ .Values.agent.maxInfraRetries | quote }}
- name: HALIPHRON_MAX_RUN_DEPTH
  value: {{ .Values.agent.maxRunDepth | quote }}
- name: HALIPHRON_LOG_CHUNK_INTERVAL_SECONDS
  value: {{ .Values.agent.logChunkIntervalSeconds | quote }}
{{- if gt (int .Values.agent.maxPromptBytes) 0 }}
- name: HALIPHRON_MAX_PROMPT_BYTES
  value: {{ .Values.agent.maxPromptBytes | quote }}
{{- end }}
{{- with .Values.agent.otlpEndpoint }}
- name: HALIPHRON_OTLP_ENDPOINT
  value: {{ . | quote }}
{{- end }}
{{- with .Values.agent.toolPolicy.allow }}
- name: HALIPHRON_TOOL_ALLOW
  value: {{ join "," . | quote }}
{{- end }}
{{- with .Values.agent.toolPolicy.deny }}
- name: HALIPHRON_TOOL_DENY
  value: {{ join "," . | quote }}
{{- end }}

- name: HALIPHRON_LLM_SECRET
  value: {{ .Values.secretNames.llmApiKey | quote }}
- name: HALIPHRON_GIT_SECRET
  value: {{ .Values.secretNames.gitToken | quote }}
{{- with .Values.mcpEndpoint }}
- name: HALIPHRON_MCP_ENDPOINT
  value: {{ . | quote }}
{{- end }}
- name: HALIPHRON_RUN_TOKEN_TTL_MULTIPLIER
  value: {{ .Values.limits.runTokenTTLMultiplier | quote }}
- name: HALIPHRON_IDEMPOTENCY_TTL
  value: {{ .Values.limits.idempotencyTTL | quote }}

{{/* A timing left at 0 is not sent: the backend's own default is the contract's. */}}
{{- $t := .Values.clusterProtocol.timings }}
{{- range $env, $value := dict
      "HALIPHRON_HEARTBEAT_INTERVAL_SECONDS" $t.heartbeatIntervalSeconds
      "HALIPHRON_STALE_AFTER_SECONDS" $t.staleAfterSeconds
      "HALIPHRON_LEASE_TTL_SECONDS" $t.leaseTTLSeconds
      "HALIPHRON_ACK_TIMEOUT_SECONDS" $t.ackTimeoutSeconds
      "HALIPHRON_MAX_WAIT_SECONDS" $t.maxWaitSeconds
      "HALIPHRON_MAX_LEASES_PER_POLL" $t.maxLeasesPerPoll
      "HALIPHRON_ARTIFACT_TTL_MULTIPLIER" $t.artifactTTLMultiplier }}
{{- if $value }}
- name: {{ $env }}
  value: {{ $value | quote }}
{{- end }}
{{- end }}
- name: HALIPHRON_MIN_CONTROLLER_VERSION
  value: {{ .Values.clusterProtocol.minControllerVersion | quote }}
- name: HALIPHRON_MAX_CONTROLLER_VERSION
  value: {{ .Values.clusterProtocol.maxControllerVersion | quote }}

- name: HALIPHRON_SWEEP_INTERVAL
  value: {{ .Values.backend.sweepInterval | quote }}
- name: HALIPHRON_REAP_INTERVAL
  value: {{ .Values.backend.reapInterval | quote }}
- name: HALIPHRON_LOG_LEVEL
  value: {{ .Values.backend.logLevel | quote }}
- name: HALIPHRON_LOG_FORMAT
  value: {{ .Values.backend.logFormat | quote }}
{{- with .Values.backend.extraEnv }}
{{ toYaml . }}
{{- end }}
{{- end -}}
