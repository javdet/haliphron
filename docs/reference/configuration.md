# Configuration

Both processes are configured entirely from the environment. Every variable is
prefixed `HALIPHRON_`.

The Helm charts set these from their values; see the [Helm values
reference](helm-values.md) for the mapping.

- Backend: [`backend/config/`](../../backend/config/)
- Controller: [`controller/config/`](../../controller/config/)

---

## Backend

### Listeners

| Variable | Default | Meaning |
|---|---|---|
| `HALIPHRON_MODE` | `all` | which listeners run: `all`, `api`, `mcp`, `cluster` |
| `HALIPHRON_PUBLIC_ADDR` | `:8080` | the REST API |
| `HALIPHRON_MCP_ADDR` | `:8081` | the MCP endpoint |
| `HALIPHRON_CLUSTER_ADDR` | `:8082` | the Cluster API |
| `HALIPHRON_METRICS_ADDR` | `:9090` | health and version |

Exactly one release must serve `cluster` or `all`: the expiry scanners run
wherever the Cluster API does.

### Database

| Variable | Default | Meaning |
|---|---|---|
| `HALIPHRON_DSN` | — | PostgreSQL connection string; required |
| `HALIPHRON_DB_MAX_CONNS` | `16` | maximum open connections |
| `HALIPHRON_DB_IDLE_CONNS` | `4` | maximum idle connections |
| `HALIPHRON_DB_CONN_LIFETIME` | `1h` | connection maximum lifetime |
| `HALIPHRON_MIGRATE` | `true` | apply the schema at startup |

Every replica calls the migration; an advisory lock serialises them.

### Artifacts

| Variable | Default | Meaning |
|---|---|---|
| `HALIPHRON_ARTIFACT_MODE` | `relay` | `relay` or the object-store mode |
| `HALIPHRON_ARTIFACT_PATH` | `/var/lib/haliphron/artifacts` | the volume, in relay mode |
| `HALIPHRON_ARTIFACT_MAX_BYTES_PER_RUN` | `1073741824` (1 GiB) | per-run byte budget, enforced in the cluster |
| `HALIPHRON_ARTIFACT_RETAIN_LOGS` | `720h` (30 days) | |
| `HALIPHRON_ARTIFACT_RETAIN_RESULTS` | `4320h` (180 days) | |
| `HALIPHRON_ARTIFACT_RETAIN_ARTIFACTS` | `2160h` (90 days) | everything else |
| `HALIPHRON_ARTIFACT_TTL_MULTIPLIER` | `2` | artifact credential lifetime, as a multiple of the lease TTL |

In relay mode the backend's reaper walks the volume by prefix age. In
object-store mode these three retentions are rendered into ILM or S3 lifecycle
rules by the chart instead.

### Object storage

Object-store mode only.

| Variable | Default | Meaning |
|---|---|---|
| `HALIPHRON_S3_BUCKET` | `haliphron` | |
| `HALIPHRON_S3_REGION` | `us-east-1` | |
| `HALIPHRON_S3_ENDPOINT` | — | set for MinIO or any non-AWS endpoint |
| `HALIPHRON_S3_ACCESS_KEY` | — | |
| `HALIPHRON_S3_SECRET_KEY` | — | |
| `HALIPHRON_S3_SESSION_TOKEN` | — | |
| `HALIPHRON_S3_PATH_STYLE` | `false` | required by MinIO |

### Encryption

| Variable | Default | Meaning |
|---|---|---|
| `HALIPHRON_KEK_ID` | `default` | names the wrapping key |
| `HALIPHRON_KEK_FILE` | — | path to the key encryption key; checked first |
| `HALIPHRON_KEK` | — | the key itself, base64 or hex |

`HALIPHRON_KEK_FILE` is checked before `HALIPHRON_KEK`.

With no key at all, the backend starts and says so. Managed secrets are
unavailable; referenced secrets still work.

### The first admin token

| Variable | Default | Meaning |
|---|---|---|
| `HALIPHRON_BOOTSTRAP_TOKEN_FILE` | — | path to the token; checked first |
| `HALIPHRON_BOOTSTRAP_TOKEN` | — | the token itself |
| `HALIPHRON_BOOTSTRAP_TOKEN_TTL` | `0` | zero means no expiry |

Both forms are whitespace-trimmed. Empty means the installation already has a
token: nothing is written and nothing is said.

### Cluster protocol timings

Set here and handed to every controller at registration.

| Variable | Default | Meaning |
|---|---|---|
| `HALIPHRON_HEARTBEAT_INTERVAL_SECONDS` | `10` | how often a controller reports |
| `HALIPHRON_STALE_AFTER_SECONDS` | `90` | silence after which a cluster is `Unreachable` |
| `HALIPHRON_LEASE_TTL_SECONDS` | `120` | how long a lease is held |
| `HALIPHRON_ACK_TIMEOUT_SECONDS` | `60` | how long an unacknowledged lease is kept |
| `HALIPHRON_MAX_WAIT_SECONDS` | `30` | the long poll's ceiling |
| `HALIPHRON_MAX_LEASES_PER_POLL` | `10` | leases handed out in one poll |
| `HALIPHRON_MAX_ACK_EXPIRIES` | `5` | ack expiries before a run is failed |

`HALIPHRON_MAX_WAIT_SECONDS` is the number every proxy in front of the Cluster
API must tolerate on a read.

### Controller version range

| Variable | Default | Meaning |
|---|---|---|
| `HALIPHRON_MIN_CONTROLLER_VERSION` | `0.1.0` | below this, a controller is told it is fatal |
| `HALIPHRON_MAX_CONTROLLER_VERSION` | `99.0.0` | above this, likewise |

### Run defaults

Filled in at admission when the request does not state them.

| Variable | Default | Meaning |
|---|---|---|
| `HALIPHRON_AGENT_IMAGE` | — | the agent image, by digest; required |
| `HALIPHRON_AGENT_IMAGE_PULL_POLICY` | `IfNotPresent` | |
| `HALIPHRON_DEFAULT_AGENT` | `claude-code` | |
| `HALIPHRON_DEFAULT_MODEL` | `anthropic/claude-opus-5` | |
| `HALIPHRON_DEFAULT_TIMEOUT_SECONDS` | `3600` | |
| `HALIPHRON_RUN_TTL_SECONDS` | `86400` | how long a finished run's objects survive in the cluster |
| `HALIPHRON_MAX_INFRA_RETRIES` | `3` | controller-local retries per run |
| `HALIPHRON_LOG_CHUNK_INTERVAL_SECONDS` | `5` | how often the pod uploads a log chunk |
| `HALIPHRON_OTLP_ENDPOINT` | — | passed to the pod |
| `HALIPHRON_AGENT_NODE_SELECTOR` | — | JSON object |
| `HALIPHRON_AGENT_TOLERATIONS` | — | JSON array |

### Limits

| Variable | Default | Meaning |
|---|---|---|
| `HALIPHRON_MAX_PROMPT_BYTES` | `524288` (512 KiB) | over this, admission answers 413 |
| `HALIPHRON_MAX_RUN_DEPTH` | `8` | how deep agent-started runs may nest |
| `HALIPHRON_MAX_RUN_CHILDREN` | `10` | children one run may start |
| `HALIPHRON_IDEMPOTENCY_TTL` | `24h` | how long an `Idempotency-Key` is remembered |
| `HALIPHRON_RUN_TOKEN_TTL_MULTIPLIER` | `2` | per-run MCP token life, as a multiple of the run timeout |

The prompt ceiling follows from the Kubernetes Secret the prompt is delivered
in, which is capped at 1 MiB across all its keys together.

### Tool policy ceiling

| Variable | Default | Meaning |
|---|---|---|
| `HALIPHRON_TOOL_DENY` | — | comma-separated; a deny here cannot be lifted by a role |
| `HALIPHRON_TOOL_ALLOW` | — | comma-separated |

### Secret names

| Variable | Default | Meaning |
|---|---|---|
| `HALIPHRON_LLM_SECRET` | `llm-api-key` | which stored secret carries the model key |
| `HALIPHRON_GIT_SECRET` | `git-token` | which stored secret carries the git credential; `<name>-github` and `<name>-gitlab` are tried first |
| `HALIPHRON_MCP_ENDPOINT` | — | the MCP URL written into a pod's `mcp.json` |

### Background work and logging

| Variable | Default | Meaning |
|---|---|---|
| `HALIPHRON_SWEEP_INTERVAL` | `5s` | lease and ack expiry scan |
| `HALIPHRON_REAP_INTERVAL` | `1h` | artifact retention scan, relay mode |
| `HALIPHRON_LOG_LEVEL` | `info` | |
| `HALIPHRON_LOG_FORMAT` | `json` | |

---

## Controller

### Required

The process refuses to start without all four.

| Variable | Meaning |
|---|---|
| `HALIPHRON_BACKEND_URL` | the control plane's Cluster API root; a trailing slash is trimmed |
| `HALIPHRON_CLUSTER_NAME` | this cluster's name |
| `HALIPHRON_CALLBACK_URL` | where agent pods post their completion report |
| `HALIPHRON_AGENT_NAMESPACE` | where agent Jobs are created |

`HALIPHRON_AGENT_NAMESPACE` falls back to the controller's own namespace, so
it is effectively required only when the two differ — which they should.

### Identity and registration

| Variable | Default | Meaning |
|---|---|---|
| `HALIPHRON_BOOTSTRAP_TOKEN_FILE` | — | the one-time registration token; keeps it out of `kubectl describe` |
| `HALIPHRON_BOOTSTRAP_TOKEN` | — | the same, as a variable |
| `HALIPHRON_IDENTITY_SECRET` | — | the Secret holding the Ed25519 pair |
| `HALIPHRON_NAMESPACE` | the service account's namespace | where the controller's own Secret lives |
| `HALIPHRON_CLUSTER_LABELS` | — | `key=value` pairs; placement facts |

When `HALIPHRON_NAMESPACE` is unset the controller reads
`/var/run/secrets/kubernetes.io/serviceaccount/namespace`.

### Capacity

| Variable | Default | Meaning |
|---|---|---|
| `HALIPHRON_CAPACITY_SLOTS` | `8` | concurrent runs this cluster accepts; maximum 256 |
| `HALIPHRON_RUNTIMES` | — | comma-separated agent types this cluster will run |

A `HALIPHRON_CAPACITY_SLOTS` above 256 is a startup failure, not a clamp.

### Agent Jobs

| Variable | Default | Meaning |
|---|---|---|
| `HALIPHRON_AGENT_SERVICE_ACCOUNT` | — | the ServiceAccount agent pods run as |
| `HALIPHRON_GRACE_SECONDS` | `60` | the pod's shutdown budget |
| `HALIPHRON_DEADLINE_SLACK_SECONDS` | `600` | added to the run timeout for the Job's backstop |
| `HALIPHRON_STARTUP_DEADLINE_SECONDS` | `600` | how long a pod may fail to start before the run fails |
| `HALIPHRON_PREFLIGHT_JOB` | `true` | check the cluster can create a Job before accepting work |
| `HALIPHRON_ARTIFACT_MAX_BYTES_PER_RUN` | `1073741824` (1 GiB) | per-run byte budget |

After `HALIPHRON_STARTUP_DEADLINE_SECONDS`, a pod that has not started fails
the run. A permanent `ImagePullBackOff` ends there rather than waiting.

### Callback, spool and links

| Variable | Default | Meaning |
|---|---|---|
| `HALIPHRON_CALLBACK_ADDR` | `:8083` | where the callback endpoint is served |
| `HALIPHRON_SPOOL_PATH` | `/var/lib/haliphron/spool` | where reports queue during a control-plane outage |
| `HALIPHRON_RUN_URL_TEMPLATE` | — | how a run's UI link is built |

Reports queue in the spool while the control plane is unreachable. It needs
durable storage.

### Observability

| Variable | Default | Meaning |
|---|---|---|
| `HALIPHRON_METRICS_ADDR` | `:9090` | Prometheus metrics |
| `HALIPHRON_HEALTH_ADDR` | `:8081` | health probes |
| `HALIPHRON_LOG_LEVEL` | `info` | |
| `HALIPHRON_K8S_VERSION` | — | reported at registration; read from the API server when unset |
