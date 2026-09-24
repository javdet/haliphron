# Helm values

Two charts. They are versioned and upgraded independently, and a version
divergence between them is a normal state.

| Chart | Installed | What it installs |
|---|---|---|
| [`haliphron`](#the-control-plane-chart) | once | the backend, its Service and ingresses, optionally the frontend, PostgreSQL and MinIO |
| [`haliphron-runtime`](#the-runtime-chart) | in every target cluster | the controller, the `AgentRun` CRD, the agents' namespace and the RBAC for both |

Sources: [`deploy/charts/haliphron/values.yaml`](../../deploy/charts/haliphron/values.yaml)
and [`deploy/charts/haliphron-runtime/values.yaml`](../../deploy/charts/haliphron-runtime/values.yaml).

Values map onto the environment variables in the [configuration
reference](configuration.md).

---

## The control-plane chart

### Image

| Value | Default |
|---|---|
| `image.registry` | `""` |
| `image.repository` | `javdet/haliphron-backend` |
| `image.tag` | `""` — empty means the chart's `appVersion` |
| `image.digest` | `""` — wins over `tag` |
| `image.pullPolicy` | `IfNotPresent` |
| `imagePullSecrets` | `[]` |
| `nameOverride`, `fullnameOverride` | `""` |

An empty `image.registry` means Docker Hub, which is where CI publishes.

### Backend

| Value | Default | Notes |
|---|---|---|
| `backend.mode` | `all` | `all`, `api`, `mcp` or `cluster` |
| `backend.replicaCount` | `1` | |
| `backend.migrate` | `true` | apply the schema at startup |
| `backend.logLevel` | `info` | `debug`, `info`, `warn`, `error` |
| `backend.logFormat` | `json` | `json` or `text` |
| `backend.sweepInterval` | `5s` | lease and ack expiry scan |
| `backend.reapInterval` | `1h` | artifact retention scan |
| `backend.terminationGracePeriodSeconds` | `60` | |
| `backend.extraEnv`, `backend.extraEnvFrom` | `[]` | |
| `backend.extraVolumes`, `backend.extraVolumeMounts` | `[]` | |

`backend.mode` is part of the Service selector, so two releases installed
against the same database do not select each other's pods. Exactly one release
must serve `cluster` or `all`.

### Database

| Value | Default |
|---|---|
| `database.dsn` | `""` |
| `database.existingSecret` | `""` |
| `database.existingSecretKey` | `dsn` |
| `database.maxConns` | `16` |
| `database.idleConns` | `4` |
| `database.connLifetime` | `1h` |

`existingSecret` wins over `dsn`. A value passed here is written to a Secret
by the chart **and** kept in the release's own storage, where `helm get
values` reads it.

### Artifacts

| Value | Default | Notes |
|---|---|---|
| `artifacts.mode` | `relay` | |
| `artifacts.pvc.enabled` | `true` | |
| `artifacts.pvc.accessMode` | `ReadWriteOnce` | |
| `artifacts.pvc.size` | `50Gi` | |
| `artifacts.pvc.storageClass` | `""` | |
| `artifacts.pvc.existingClaim` | `""` | |
| `artifacts.mountPath` | `/var/lib/haliphron/artifacts` | |
| `artifacts.maxBytesPerRun` | `1Gi` | |
| `artifacts.retention.logs` | `720h` | 30 days |
| `artifacts.retention.results` | `4320h` | 180 days |
| `artifacts.retention.artifacts` | `2160h` | 90 days |

### Object storage

| Value | Default |
|---|---|
| `objectStorage.bucket` | `haliphron` |
| `objectStorage.region` | `us-east-1` |
| `objectStorage.endpoint` | `""` |
| `objectStorage.pathStyle` | `false` |
| `objectStorage.accessKey`, `objectStorage.secretKey`, `objectStorage.sessionToken` | `""` |
| `objectStorage.existingSecret` | `""` |
| `objectStorage.existingSecretAccessKeyKey` | `accessKey` |
| `objectStorage.existingSecretSecretKeyKey` | `secretKey` |

### Encryption

| Value | Default | Notes |
|---|---|---|
| `encryption.keyId` | `default` | |
| `encryption.autoGenerate` | `true` | generates 32 random bytes on the first install only |
| `encryption.key` | `""` | |
| `encryption.existingSecret` | `""` | |
| `encryption.existingSecretKey` | `kek` | |

`encryption.key` together with `encryption.existingSecret` is a render
failure, not a resolution.

The generated key is read back on every later render, so `helm upgrade` does
not rotate it. `helm template` cannot perform the read-back; under GitOps, set
`existingSecret` or `key` and leave `autoGenerate` off.

`helm uninstall` leaves the Secret behind (`helm.sh/resource-policy: keep`).

### The first admin token

| Value | Default | Notes |
|---|---|---|
| `bootstrapToken.enabled` | `true` | |
| `bootstrapToken.value` | `""` | |
| `bootstrapToken.existingSecret` | `""` | |
| `bootstrapToken.existingSecretKey` | `token` | |
| `bootstrapToken.ttl` | `""` | empty means no expiry |

`bootstrapToken.value` together with `bootstrapToken.existingSecret` is a
render failure.

The token is installed once, by digest. A revoked or expired bootstrap row is
not reinstated on the next start.

### Agent defaults

| Value | Default |
|---|---|
| `agent.image` | `javdet/haliphron-agent:latest` |
| `agent.imagePullPolicy` | `IfNotPresent` |
| `agent.nodeSelector` | `{}` |
| `agent.tolerations` | `[]` |
| `agent.defaultModel` | `anthropic/claude-opus-5` |
| `agent.defaultAgentType` | `claude-code` |
| `agent.timeoutSeconds` | `3600` |
| `agent.ttlSeconds` | `86400` |
| `agent.maxInfraRetries` | `3` |
| `agent.maxRunDepth` | `4` |
| `agent.maxRunChildren` | `10` |
| `agent.maxPromptBytes` | `0` — keeps the backend's own limit |
| `agent.logChunkIntervalSeconds` | `5` |
| `agent.otlpEndpoint` | `""` |
| `agent.toolPolicy.allow`, `agent.toolPolicy.deny` | `[]` |

`agent.toolPolicy` is the installation's policy ceiling. A deny here cannot be
lifted by a role.

`agent.nodeSelector` and `agent.tolerations` decide where **agent pods** land,
and they are set on this chart rather than on the runtime chart: placement
travels in the lease, and the spec the controller materialises is rendered by
the control plane. A role's own `nodeSelector` and `tolerations` replace these
outright rather than merging with them.

### Secret names

| Value | Default |
|---|---|
| `secretNames.llmApiKey` | `llm-api-key` |
| `secretNames.gitToken` | `git-token` |

These name entries in the backend's own secret store, not Kubernetes Secrets.

### Cluster protocol

| Value | Default | Notes |
|---|---|---|
| `clusterProtocol.timings.heartbeatIntervalSeconds` | `0` | 0 keeps the contract default, 10 |
| `clusterProtocol.timings.staleAfterSeconds` | `0` | contract default 90 |
| `clusterProtocol.timings.leaseTTLSeconds` | `0` | contract default 120 |
| `clusterProtocol.timings.ackTimeoutSeconds` | `0` | contract default 60 |
| `clusterProtocol.timings.maxWaitSeconds` | `0` | contract default 30 |
| `clusterProtocol.timings.maxLeasesPerPoll` | `0` | contract default 10 |
| `clusterProtocol.timings.artifactTTLMultiplier` | `0` | contract default 2 |
| `clusterProtocol.minControllerVersion` | `0.1.0` | |
| `clusterProtocol.maxControllerVersion` | `99.0.0` | |

`maxWaitSeconds` is the value every proxy in front of the cluster entrypoint
must tolerate on a read. The chart refuses to render an ingress or route
timeout below it.

### Limits

| Value | Default |
|---|---|
| `mcpEndpoint` | `""` |
| `limits.runTokenTTLMultiplier` | `2` |
| `limits.idempotencyTTL` | `24h` |
| `limits.maxAckExpiries` | `0` — contract default 5 |

### Frontend

| Value | Default |
|---|---|
| `frontend.enabled` | `false` |
| `frontend.image.repository` | `javdet/haliphron-frontend` |
| `frontend.image.registry`, `.tag`, `.digest` | `""` |
| `frontend.image.pullPolicy` | `IfNotPresent` |
| `frontend.replicaCount` | `1` |
| `frontend.service.type` | `ClusterIP` |
| `frontend.service.port` | `80` |
| `frontend.service.annotations` | `{}` |
| `frontend.cspConnectSrc` | `'self'` |
| `frontend.resources.requests.cpu` | `10m` |
| `frontend.resources.requests.memory` | `32Mi` |
| `frontend.resources.limits.memory` | `64Mi` |
| `frontend.podAnnotations`, `.nodeSelector`, `.affinity` | `{}` |
| `frontend.tolerations` | `[]` |

Enabling the frontend points the `api` entrypoint at the UI, which proxies
`/api` through to the backend.

### Service and networking

| Value | Default |
|---|---|
| `service.type` | `ClusterIP` |
| `service.annotations` | `{}` |
| `service.ports.api` | `8080` |
| `service.ports.mcp` | `8081` |
| `service.ports.cluster` | `8082` |
| `service.ports.health` | `9090` |

Three entrypoints — `api`, `mcp`, `cluster` — each exposable as an Ingress or
as an `HTTPRoute`, independently.

| Value | Default |
|---|---|
| `ingress.<entrypoint>.enabled` | `false` |
| `ingress.<entrypoint>.className` | `nginx` |
| `ingress.<entrypoint>.annotations` | `{}`, except `cluster` |
| `ingress.<entrypoint>.host` | `haliphron.example.com`, `mcp.haliphron.example.com`, `clusters.haliphron.example.com` |
| `ingress.<entrypoint>.path` | `/` |
| `ingress.<entrypoint>.pathType` | `Prefix` |
| `ingress.<entrypoint>.tls` | `[]` |

`ingress.cluster.annotations` defaults to `proxy-read-timeout: "120"` and
`proxy-send-timeout: "120"`.

| Value | Default |
|---|---|
| `httpRoute.apiVersion` | `gateway.networking.k8s.io/v1` |
| `httpRoute.<entrypoint>.enabled` | `false` |
| `httpRoute.<entrypoint>.parentRefs` | `[]` — a route with none is a render failure |
| `httpRoute.<entrypoint>.hostnames` | as for `ingress` |
| `httpRoute.<entrypoint>.matches` | `PathPrefix: /` |
| `httpRoute.<entrypoint>.timeouts` | `{}`, except `cluster` |
| `httpRoute.<entrypoint>.annotations`, `.labels`, `.filters` | `{}` / `[]` |

`httpRoute.cluster.timeouts` defaults to `request: 120s` and
`backendRequest: 120s`. Leaving it unset is a render failure, not a default.

The chart writes routes and never writes a Gateway.

### Scheduling, resources and security

| Value | Default |
|---|---|
| `resources.requests.cpu` | `100m` |
| `resources.requests.memory` | `256Mi` |
| `resources.limits.memory` | `1Gi` |
| `autoscaling.enabled` | `false` |
| `autoscaling.minReplicas` | `2` |
| `autoscaling.maxReplicas` | `10` |
| `autoscaling.targetCPUUtilizationPercentage` | `70` |
| `autoscaling.targetMemoryUtilizationPercentage` | `""` |
| `podDisruptionBudget.enabled` | `true` |
| `podDisruptionBudget.minAvailable` | `1` |
| `podDisruptionBudget.maxUnavailable` | `""` |
| `serviceAccount.create` | `true` |
| `serviceAccount.name`, `.annotations` | `""` / `{}` |
| `serviceAccount.automountServiceAccountToken` | `false` |
| `podSecurityContext.runAsNonRoot` | `true` |
| `podSecurityContext.runAsUser`, `.runAsGroup`, `.fsGroup` | `65532` |
| `podSecurityContext.seccompProfile.type` | `RuntimeDefault` |
| `securityContext.allowPrivilegeEscalation`, `.privileged` | `false` |
| `securityContext.readOnlyRootFilesystem` | `true` |
| `securityContext.capabilities.drop` | `["ALL"]` |
| `podAnnotations`, `podLabels`, `nodeSelector`, `affinity` | `{}` |
| `tolerations` | `[]` |
| `topologySpreadConstraints` | one `maxSkew: 1` over `kubernetes.io/hostname`, `ScheduleAnyway` |
| `priorityClassName` | `""` |
| `networkPolicy.enabled` | `false` |
| `networkPolicy.ingressFrom`, `.egressTo` | `[]` |

### Metrics

| Value | Default |
|---|---|
| `metrics.serviceMonitor.enabled` | `false` |
| `metrics.serviceMonitor.namespace` | `""` |
| `metrics.serviceMonitor.interval` | `30s` |
| `metrics.serviceMonitor.scrapeTimeout` | `10s` |
| `metrics.serviceMonitor.labels`, `.relabelings` | `{}` / `[]` |

The backend serves no Prometheus metrics. Enabling this ServiceMonitor points
Prometheus at a 404. The template ships so that the day the endpoint lands it
is a value change rather than a chart change.

### Subcharts

| Value | Default |
|---|---|
| `postgresql.enabled` | `enable` |
| `postgresql.auth.username` | `haliphron` |
| `postgresql.auth.password` | a literal in `values.yaml` |
| `postgresql.auth.database` | `haliphron` |
| `postgresql.primary.persistence.enabled` | `true` |
| `postgresql.primary.persistence.size` | `20Gi` |
| `minio.enabled` | `false` |
| `minio.auth.rootUser` | `haliphron` |
| `minio.auth.rootPassword` | `""` |
| `minio.defaultBuckets` | `haliphron` |
| `minio.persistence.enabled` | `true` |
| `minio.persistence.size` | `50Gi` |

`postgresql.enabled` is the string `enable`, which Helm treats as true: the
subchart is installed unless the value is set to `false`. Its default password
is a literal in `values.yaml`.

`helm dependency build` must run before install regardless of these
conditions. Helm resolves declared dependencies whether or not their condition
is met.

---

## The runtime chart

### Image

| Value | Default |
|---|---|
| `image.registry` | `""` |
| `image.repository` | `javdet/haliphron-controller` |
| `image.tag` | `""` — empty means the chart's `appVersion` |
| `image.digest` | `""` — wins over `tag`, and is the reproducible one |
| `image.pullPolicy` | `IfNotPresent` |
| `imagePullSecrets` | `[]` |
| `nameOverride`, `fullnameOverride` | `""` |

### Cluster identity

| Value | Default | Notes |
|---|---|---|
| `cluster.name` | `""` | required |
| `cluster.labels` | `{}` | placement facts, matched by a role's `clusterSelector` |
| `cluster.capacitySlots` | `8` | concurrent runs; maximum 256 |
| `cluster.runtimes` | `[claude-code, codex]` | |
| `identity.secretName` | `haliphron-cluster-identity` | holds the Ed25519 pair |

### Backend connection

| Value | Default | Notes |
|---|---|---|
| `backend.url` | `""` | required; the Cluster API root |
| `backend.bootstrapToken` | `""` | |
| `backend.bootstrapTokenExistingSecret` | `""` | keeps the token out of `kubectl describe` |
| `backend.bootstrapTokenExistingSecretKey` | `token` | |

### The agents' namespace

| Value | Default |
|---|---|
| `agents.namespace` | `haliphron-agents` |
| `agents.createNamespace` | `true` |
| `agents.serviceAccount.create` | `true` |
| `agents.serviceAccount.name` | `haliphron-agent` |
| `agents.serviceAccount.annotations` | `{}` |

This namespace must stay separate from the controller's; its NetworkPolicy
applies to everything in it. See [the untrusted agent
pod](../explanation/the-untrusted-agent-pod.md).

#### Resource quota

`agents.resourceQuota.enabled` defaults to `true`.

| `agents.resourceQuota.hard` key | Default |
|---|---|
| `pods` | `50` |
| `requests.cpu` | `20` |
| `requests.memory` | `64Gi` |
| `limits.cpu` | `40` |
| `limits.memory` | `128Gi` |
| `count/agentruns.haliphron.io` | `1000` |
| `count/jobs.batch` | `2000` |
| `count/secrets` | `1016` |
| `count/configmaps` | `1016` |

#### Limit range

`agents.limitRange.enabled` defaults to `true`.

| Value | Default |
|---|---|
| `agents.limitRange.default.cpu` | `1` |
| `agents.limitRange.default.memory` | `2Gi` |
| `agents.limitRange.defaultRequest.cpu` | `250m` |
| `agents.limitRange.defaultRequest.memory` | `512Mi` |
| `agents.limitRange.max.cpu` | `8` |
| `agents.limitRange.max.memory` | `32Gi` |

#### Network policy

`agents.networkPolicy.enabled` defaults to `true`. It denies all ingress and
allows egress only to DNS, to the controller's callback, and to the CIDRs
below.

| Value | Default |
|---|---|
| `agents.networkPolicy.allowedCIDRs` | `[0.0.0.0/0]` |
| `agents.networkPolicy.exceptCIDRs` | `10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`, `169.254.0.0/16` |
| `agents.networkPolicy.ports` | 443/TCP and 80/TCP |

The exceptions exclude the cluster's own private ranges, and so exclude the
API server.

There is no filtering by hostname: NetworkPolicy matches addresses. An agent
that can reach a permitted CIDR can reach any host inside it.

### Controller

| Value | Default | Notes |
|---|---|---|
| `controller.replicaCount` | `1` | |
| `controller.callbackURL` | `""` | required |
| `controller.callbackPort` | `8083` | |
| `controller.healthPort` | `8081` | |
| `controller.metricsPort` | `9090` | |
| `controller.spool.persistence.enabled` | `true` | |
| `controller.spool.persistence.accessMode` | `ReadWriteOnce` | |
| `controller.spool.persistence.size` | `20Gi` | |
| `controller.spool.persistence.storageClass`, `.existingClaim` | `""` | |
| `controller.spool.mountPath` | `/var/lib/haliphron/spool` | |
| `controller.runURLTemplate` | `""` | |
| `controller.graceSeconds` | `60` | the pod's shutdown budget |
| `controller.deadlineSlackSeconds` | `600` | added to the run timeout for the Job backstop |
| `controller.startupDeadlineSeconds` | `600` | how long a pod may fail to start |
| `controller.preflightJob` | `true` | |
| `controller.logLevel` | `info` | |
| `controller.terminationGracePeriodSeconds` | `60` | |
| `controller.extraEnv`, `.extraEnvFrom` | `[]` | |
| `controller.resources.requests.cpu` | `50m` | |
| `controller.resources.requests.memory` | `128Mi` | |
| `controller.resources.limits.memory` | `512Mi` | |
| `controller.podAnnotations`, `.podLabels`, `.nodeSelector`, `.affinity` | `{}` | |
| `controller.tolerations` | `[]` | |
| `controller.priorityClassName` | `""` | |
| `controller.networkPolicy.enabled` | `false` | |

The spool is where reports queue while the control plane is unreachable.
Disabling its persistence means a controller restart during an outage loses
them.

Security contexts match the control-plane chart: non-root 65532, read-only
root filesystem, all capabilities dropped, `RuntimeDefault` seccomp.

### RBAC, CRD and metrics

| Value | Default | Notes |
|---|---|---|
| `serviceAccount.create` | `true` | |
| `serviceAccount.name`, `.annotations` | `""` / `{}` | |
| `rbac.create` | `true` | |
| `rbac.scope` | `namespaced` | |
| `crd.install` | `true` | |
| `crd.conversionWebhook.enabled` | `false` | |
| `crd.conversionWebhook.issuerRef` | `{}` | |
| `metrics.serviceMonitor.enabled` | `false` | |
| `metrics.serviceMonitor.namespace` | `""` | |
| `metrics.serviceMonitor.interval` | `30s` | |
| `metrics.serviceMonitor.scrapeTimeout` | `10s` | |
| `metrics.serviceMonitor.labels` | `{}` | |

`crd.conversionWebhook.enabled` must stay `false` until the controller serves
`/convert`. Turning it on points the API server at a webhook that is not
there, and every read of an `AgentRun` then fails.

The CRD ships in `templates/` with `helm.sh/resource-policy: keep`, not in
`crds/`. `helm uninstall` therefore leaves the CRD and the agents' namespace
in place.

Unlike the backend, the controller does serve Prometheus metrics.
