# REST API

The public HTTP API, served on `:8080` by default. It is the surface used by
scripts, CI, the Slack adapter and the web UI.

Defined in [`backend/restapi/`](../../backend/restapi/).

## Base path

```
/api/v1
```

The version is in the path, not in a header.

## Conventions

| Property | Value |
|---|---|
| Field naming | `snake_case` |
| Request and response media type | `application/json` |
| Maximum request body | 8 MiB (8388608 bytes) |
| Authentication | `Authorization: Bearer <token>` on every endpoint |
| Time format | RFC 3339 |
| Money | decimal string, e.g. `"1.250000"` |
| Identifiers | ULID strings |

The machine contracts — the Cluster API, the `AgentRun` CRD and the pod
webhook — use `camelCase`. This API does not.

## Authentication and scopes

Every request carries a bearer token. There are three scopes; `admin` implies
the other two.

| Scope | Value |
|---|---|
| Read runs, roles and clusters | `runs:read` |
| Create, cancel and retry runs | `runs:write` |
| Everything, including deleting runs, roles, secrets, tokens and clusters | `admin` |

Tokens have three kinds: `user`, `service` and `run-mcp`. A `run-mcp` token is
issued to an agent pod for the life of one run; requests made with it record
the run as the parent of anything it creates.

## Errors

Every failure returns this envelope:

```json
{
  "error": {
    "code": "invalid_request",
    "message": "a name is required",
    "field": "name"
  }
}
```

`field` is present when the refusal is attributable to one request field.

| Status | `code` | Meaning |
|---|---|---|
| 400 | `invalid_request` | the body could not be read or is not valid JSON |
| 401 | `unauthenticated` | no bearer token, or the token is not usable |
| 403 | `forbidden` | the token does not carry the required scope |
| 404 | `not_found` | no such object |
| 409 | `run_terminal` | the run has already ended |
| 409 | `run_live` | the run has not ended, so it cannot be deleted |
| 409 | `token_live` | the token has not been revoked and has not expired, so it cannot be removed |
| 409 | `bootstrap_token` | the bootstrap token's row is never removed |
| 409 | `in_flight` | a request with this `Idempotency-Key` is still being processed; `Retry-After: 1` is set |
| 413 | `too_large` | the request body exceeds 8 MiB, or the prompt exceeds 512 KiB |
| 422 | `invalid_request` | the request is well formed and refused |
| 422 | `idempotency_conflict` | this `Idempotency-Key` was used with a different body |
| 500 | `internal` | the request could not be completed |

401 does not distinguish unknown, revoked and expired tokens.

## Idempotency

`POST /api/v1/runs` accepts an `Idempotency-Key` request header. A repeat with
the same key and the same body returns the first request's response verbatim
with status `200`. The same key with a different body is refused with `422`
`idempotency_conflict`.

---

## Runs

### `POST /api/v1/runs`

Scope: `runs:write`. Admits one run.

Request fields:

| Field | Type | Required | Default | Notes |
|---|---|---|---|---|
| `prompt` | string | yes | — | maximum 512 KiB |
| `agent` | string | no | installation default | `claude-code` or `codex` |
| `model` | string | no | installation default | |
| `role` | string | no | none | a configured role name |
| `repo` | string | no | none | clone URL; omitted means a run with no repository |
| `base_branch` | string | no | repository default | |
| `target_branch` | string | no | generated | must match `^[A-Za-z0-9][A-Za-z0-9._/-]{0,254}$` |
| `create_pr` | boolean | no | installation default | |
| `timeout_seconds` | integer | no | installation default | 60 to 86400 |
| `max_cost_usd` | string | no | none | decimal, `^[0-9]{1,12}(\.[0-9]{1,6})?$` |
| `max_turns` | integer | no | role or none | |
| `priority` | integer | no | 0 | |
| `async` | boolean | no | `true` | `false` holds the connection until the run ends |
| `wait_seconds` | integer | no | — | bounds a synchronous call |

Returns a [run object](#the-run-object).

| Status | When |
|---|---|
| 202 | admitted; `async` was true, or the wait elapsed first |
| 200 | `async` was false and the run finished within the wait, or the request was an idempotent replay |

When the caller presents a `run-mcp` token, `parent_run_id` is set to that
token's run and `depth` to the parent's depth plus one. An omitted `role` or
`repo` is inherited from the parent. The maximum depth is 8.

### `GET /api/v1/runs`

Scope: `runs:read`. Most recent first.

| Query parameter | Type | Default | Notes |
|---|---|---|---|
| `status` | string, repeatable | all | |
| `role` | string | all | |
| `agent` | string | all | |
| `cluster_id` | ULID | all | |
| `parent_run_id` | ULID | all | |
| `q` | string | — | free text |
| `limit` | integer | 50 | |
| `before` | ULID | — | cursor |

```json
{
  "runs": [ /* run objects */ ],
  "next_before": "01J..."
}
```

`next_before` is present only when the page was full. Pass it as `before` for
the next page.

### `GET /api/v1/runs/{id}`

Scope: `runs:read`. Returns a [run object](#the-run-object).

### `GET /api/v1/runs/{id}/result`

Scope: `runs:read`. Serves a stored object.

| Query parameter | Type | Default |
|---|---|---|
| `key` | string | the run's result |

| Status | When |
|---|---|
| 200 | relay mode: the bytes are streamed, with `Cache-Control: no-store` and `X-Content-Type-Options: nosniff` |
| 302 | object-store mode: `Location` carries a presigned link valid for 15 minutes |

Callers must follow redirects.

### `GET /api/v1/runs/{id}/logs`

Scope: `runs:read`. Lists log chunks.

| Query parameter | Type | Default |
|---|---|---|
| `after` | string | — |
| `limit` | integer | 100 |

```json
{
  "chunks": [
    {"key": "...", "size_bytes": 4096, "at": "2026-09-24T10:00:00Z", "url": "..."}
  ],
  "next_after": "..."
}
```

`url` is a presigned GET in object-store mode and this API's own chunk
endpoint in relay mode.

### `GET /api/v1/runs/{id}/logs/{chunk}`

Scope: `runs:read`. Streams one chunk as `text/plain; charset=utf-8`.

Relay mode only. In object-store mode the listing hands out presigned links
and nothing reaches this endpoint.

### `GET /api/v1/runs/{id}/attempts`

Scope: `runs:read`. The attempt ledger.

```json
{"attempts": [ /* attempt objects */ ]}
```

See [the attempt object](#the-attempt-object).

### `POST /api/v1/runs/{id}/cancel`

Scope: `runs:write`. Body: `{"reason": "..."}` — `reason` is optional.

Returns `202` and a [run object](#the-run-object).

Delivery is asynchronous: the instruction is recorded and reaches the cluster
on its next heartbeat. See [the pull model](../explanation/the-pull-model.md).

### `POST /api/v1/runs/{id}/retry`

Scope: `runs:write`. No body. Returns `202` and a [run object](#the-run-object).

### `DELETE /api/v1/runs/{id}`

Scope: `admin`. Deletes a run that has ended. Returns `204`. No request body.

What is deleted: the run, its attempt ledger, its per-run token, and every
object under `runs/{id}/` in artifact storage (logs, result, artifacts). Runs
it started are kept, with `parent_run_id` cleared. The audit log keeps a
`run.deleted` record holding the run's final status, who created it and what
it cost.

A run that has not ended is refused with `409 run_live`. Cancel it first.

The scope is `admin` and not `runs:write` because every agent pod holds a
per-run token carrying `runs:write`.

A controller that reports on the run after it has been deleted gets the
`RunNotFound` code and the `abandon` action, and drops its copy.

Runs can also be deleted automatically, some time after they finish. See
`HALIPHRON_RUN_RETENTION` in [configuration](configuration.md#background-work-and-logging).

---

## The run object

| Field | Type | Notes |
|---|---|---|
| `run_id` | ULID | |
| `status` | string | see [statuses](statuses-and-exit-codes.md#backend-statuses) |
| `agent` | string | |
| `model` | string | |
| `role` | string | omitted when none |
| `repo`, `base_branch`, `target_branch` | string | omitted when none |
| `cluster_id` | ULID | omitted until leased |
| `epoch` | integer | ownership generation |
| `attempt` | integer | retry within the epoch |
| `observed_phase` | string | the CR phase last reported |
| `failure_class` | string | see [failure classes](statuses-and-exit-codes.md#failure-classes) |
| `status_reason`, `status_message` | string | |
| `exit_code` | integer | omitted until the pod reports one |
| `result_summary` | string | |
| `pr_url`, `pr_number`, `commit_sha` | string, integer, string | |
| `cost_usd` | string | decimal |
| `input_tokens`, `output_tokens`, `num_turns` | integer | |
| `parent_run_id` | ULID | omitted when the run has no parent |
| `depth` | integer | 0 for a run with no parent |
| `created_by` | string | the token subject, its name, `run:<id>`, or `mattermost:<username>` |
| `created_via` | string | `api`, `mcp`, `agent` or `mattermost` |
| `created_at`, `started_at`, `finished_at` | timestamp | the last two omitted until they exist |

`status` is the reported status, not the stored one. A terminal run whose
report has not been collected reads `CompletedWithoutResult`. See
[statuses](statuses-and-exit-codes.md#completedwithoutresult).

## The attempt object

| Field | Type | Notes |
|---|---|---|
| `attempt` | integer | |
| `epoch` | integer | |
| `cluster_id` | ULID | |
| `phase`, `reason`, `message` | string | |
| `exit_code` | integer | |
| `failure_class` | string | |
| `job_name`, `pod_name`, `node_name` | string | |
| `cost_usd` | string | decimal |
| `input_tokens`, `output_tokens` | integer | |
| `declared_duration_ms` | integer | what the pod reported |
| `observed_duration_ms` | integer | the window the lease covered |
| `started_at`, `finished_at`, `completion_received_at` | timestamp | |

---

## Roles

| Endpoint | Scope | Returns |
|---|---|---|
| `GET /api/v1/roles` | `runs:read` | `{"roles": [ ... ]}` |
| `GET /api/v1/roles/{name}` | `runs:read` | a role object |
| `PUT /api/v1/roles/{name}` | `admin` | `200` and the role object |
| `DELETE /api/v1/roles/{name}` | `admin` | `204` |

A role object is `{"name", "spec", "created_by", "updated_at"}`. `spec` is an
arbitrary JSON object; its fields are listed in the [role spec
reference](role-spec.md).

`PUT` replaces the whole spec. There are no merge semantics. A body that is
not a JSON object is refused with `422`.

`DELETE` is a soft delete. Runs already admitted under the role keep naming
it.

---

## Clusters

### `GET /api/v1/clusters`

Scope: `runs:read`.

Each entry carries `cluster_id`, `name`, `labels`, `status`,
`agent_namespace`, `controller_version`, `k8s_version`, `runtimes`,
`capacity_slots`, `free_slots`, `active_runs`, `quota_exhausted`,
`registered_at`, `last_heartbeat_at` and `revoked_reason`.

`active_runs` is what the backend believes is on the cluster now: runs that
are `Leased`, `Dispatched`, `Starting`, `Running` or `Unknown`. `free_slots`
is what the cluster itself last reported. The two can disagree for a moment.

No key material is returned.

### `POST /api/v1/clusters/bootstrap-tokens`

Scope: `admin`. Mints the credential a controller registers with.

| Field | Type | Required | Default |
|---|---|---|---|
| `name` | string | yes | — |
| `ttl_seconds` | integer | no | 86400 (24 hours) |
| `max_uses` | integer | no | unlimited |

Returns `201` with `token_id`, `name`, `token`, `expires_at` and `max_uses`.

`token` is returned once. Only its digest is stored; this API cannot show it
again.

### `GET /api/v1/clusters/bootstrap-tokens`

Scope: `admin`. Returns `token_id`, `name`, `expires_at`, `max_uses`, `uses`,
`created_by`, `created_at` and `revoked_at` per token. Never the token itself.

### `POST /api/v1/clusters/{id}/revoke`

Scope: `admin`. Body: `{"reason": "..."}` — optional. Returns `204`.

The cluster's runs are left alone.

### `DELETE /api/v1/clusters/{id}`

Scope: `admin`. Removes a cluster's row. Returns `204`.

Revoke first, then delete: a cluster that has not been revoked is refused
with `409` `cluster_live`. A cluster that any run records as where it ran,
as its current cluster or as the cluster of one of its attempts, is refused
with `409` `cluster_in_use`, and the message says how many runs. Delete those
runs first, or let run retention remove them. An unknown cluster is `404`.

The bootstrap token that admitted the cluster is kept. With the row gone, the
controller's key is unknown, and a controller that is still installed is told
to `reregister`. If its bootstrap token still has uses left and has not
expired, it comes back as a new cluster. Uninstall the controller first.

---

## Statistics

### `GET /api/v1/stats/runs`

Scope: `runs:read`. Counts runs per cluster and status.

| Query | Meaning |
|---|---|
| `since` | optional, RFC 3339. Counts only runs created at or after it. If you leave it out, every run is counted. A malformed value is a `400` with `field: "since"`. |

```json
{
  "since": "2026-09-25T00:00:00Z",
  "clusters": [
    {
      "cluster_id": "01JD7A0000CLUSTEREUWEST1",
      "cluster_name": "prod-eu-west-1",
      "counts": {"Succeeded": 12, "Failed": 3, "CompletedWithoutResult": 1, "Running": 2},
      "total": 18,
      "duration_seconds": {"p50": 412.0, "p95": 1730.5},
      "queue_wait_seconds": {"p50": 2.1, "p95": 30.0}
    },
    {
      "cluster_id": null,
      "cluster_name": "",
      "counts": {"Queued": 1},
      "total": 1,
      "duration_seconds": {"p50": null, "p95": null},
      "queue_wait_seconds": {"p50": null, "p95": null}
    }
  ]
}
```

- **Status.** `counts` is keyed by reported status, the same value a run
  object shows. A terminal run whose completion never arrived counts as
  `CompletedWithoutResult`, not as the phase it ended in. A status with no
  runs is absent from `counts`.
- **Unplaced runs.** Runs no cluster has taken yet are grouped under
  `cluster_id: null`.
- **`duration_seconds`.** Measured from the first `Running` observation to the
  end, over runs that finished.
- **`queue_wait_seconds`.** Measured from being queued, or queued again by a
  retry, to `Running`, over runs that started.
- Both are `null` when nothing in the window was measured.

## Secrets

### `GET /api/v1/secrets`

Scope: `admin`. Returns `name`, `kind`, `ref`, `updated_at` and `rotated_at`
per secret.

Values and ciphertexts are never returned.

### `PUT /api/v1/secrets/{name}`

Scope: `admin`. Exactly one of two fields:

| Field | Type | Meaning |
|---|---|---|
| `value` | string | a managed secret, encrypted under a data key wrapped by the installation's KEK |
| `ref` | string | a referenced secret: a pointer into an external manager, which the backend never resolves |

Both, or neither, is refused with `422`.

Returns `200` with `name` and `kind`, plus `ref` for a referenced secret.

`409` with code `no_kek` when a `value` is sent to an installation with no key
encryption key.

### `GET /api/v1/model-credential`

Scope: `runs:read`. The stored secret every lease reads the model key from,
without its value.

| Field | Type | Meaning |
|---|---|---|
| `secret_name` | string | the name leases read: `HALIPHRON_LLM_SECRET`, `llm-api-key` by default |
| `configured` | boolean | whether a secret by that name is stored |
| `kind` | string | `managed` or `referenced`; absent when not configured |
| `type` | string | `api_key` or `oauth_token`, derived from the value; absent for a reference or a value this process cannot decrypt |
| `ref` | string | the reference, for a referenced secret |
| `updated_at` | timestamp | |
| `problem` | string | why a stored credential will not reach a pod; absent when leases will carry it |

Runs:read rather than admin, because whoever submits runs is owed the reason
they will all fail.

### `PUT /api/v1/model-credential`

Scope: `admin`. Stores the credential under `secret_name`.

| Field | Type | Meaning |
|---|---|---|
| `type` | string | required: `api_key` or `oauth_token` |
| `value` | string | the credential, trimmed before it is stored |
| `ref` | string | a pointer into an external manager |

Exactly one of `value` and `ref`. A `value` whose shape contradicts `type` is
refused with `422`: an `oauth_token` starts with `sk-ant-oat`, and an
`api_key` does not. Returns `200` with the same body as the `GET`.

### `GET /api/v1/git-credential`

Scope: `runs:read`. Every name a lease tries for a run's git token, without
any value.

| Field | Type | Meaning |
|---|---|---|
| `secret_name` | string | the fallback name: `HALIPHRON_GIT_SECRET`, `git-token` by default |
| `configured` | boolean | whether any of the names below is stored, usable or not |
| `tokens` | array | the fallback first, then `github` and `gitlab` |
| `problem` | string | set when the deployment names no git secret |

Each entry of `tokens`:

| Field | Type | Meaning |
|---|---|---|
| `provider` | string | `github` or `gitlab`; absent for the fallback, which a forge uses when it has no token of its own |
| `secret_name` | string | `<secret_name>-<provider>`, or `secret_name` for the fallback |
| `configured` | boolean | whether a secret by that name is stored |
| `kind` | string | `managed` or `referenced`; absent when not configured |
| `updated_at` | timestamp | |
| `problem` | string | why a stored token will not reach a pod; absent when leases will carry it |

A lease uses the forge's own token when one is stored and the fallback
otherwise. A stored token with a `problem` fails the lease; the fallback is
not tried.

### `PUT /api/v1/git-credential`

Scope: `admin`. Stores one token.

| Field | Type | Meaning |
|---|---|---|
| `provider` | string | `github` or `gitlab`; absent to store the fallback |
| `value` | string | required: the token, trimmed before it is stored |

A reference is refused with `422`, because a lease fails on one. A value with
whitespace in it, or with a documented prefix naming the other forge
(`ghp_`, `github_pat_`, … for GitHub; `glpat-` for GitLab), is refused with
`422` too. Returns `200` with the same body as the `GET`.

---

## Tokens

### `POST /api/v1/tokens`

Scope: `admin`.

| Field | Type | Required | Notes |
|---|---|---|---|
| `name` | string | yes | |
| `scopes` | array of string | yes | `runs:read`, `runs:write`, `admin`; an unknown scope or an empty array is refused |
| `subject` | string | no | |
| `ttl_seconds` | integer | no | omitted or zero means no expiry |

Returns `201` with `token_id`, `name`, `token`, `scopes` and `expires_at`.

`token` is returned once.

### `GET /api/v1/tokens`

Scope: `admin`. Returns `token_id`, `name`, `kind`, `scopes`, `subject`,
`run_id`, `expires_at`, `created_at` and `revoked_at` per token.

### `DELETE /api/v1/tokens/{id}`

Scope: `admin`. Revokes the token. Returns `204`.

The row stays, with `revoked_at` set.

### `POST /api/v1/tokens/{id}/remove`

Scope: `admin`. Deletes the row of a token that has been revoked or has expired.
Returns `204`. No request body.

A token that still works is refused with `409 token_live`. Revoke it first.

The bootstrap token's row is refused with `409 bootstrap_token`, whether or
not it has been revoked. That row stops the next start from installing the
value still in the chart's Secret again.

Every removal writes a `token.removed` audit record naming the token and the
caller.

---

## Health endpoints

Served on the health port, outside `/api/v1`, and requiring no token.

| Endpoint | Meaning |
|---|---|
| `GET /healthz` | the process is alive; does not touch the database |
| `GET /readyz` | the process is ready to serve |
| `GET /version` | the build |

| `GET /metrics` | Prometheus metrics, below |

### Prometheus metrics

The run and cluster series are read from PostgreSQL at scrape time, not counted
in process. Every replica therefore reports the same values, so aggregate them
with `max without(instance, pod)`, never `sum`. A scrape result is cached for
15 seconds.

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `haliphron_runs` | gauge | `cluster`, `status` | Runs in the store. `status` is the reported status. `cluster` is `""` for runs not yet placed. It is a gauge because deleting a run lowers it. |
| `haliphron_run_duration_seconds` | histogram | `cluster` | First `Running` to the end, for finished runs |
| `haliphron_run_queue_wait_seconds` | histogram | `cluster` | Queued to `Running`, for started runs |
| `haliphron_cluster_active_runs` | gauge | `cluster` | `Leased` through `Running`, plus `Unknown` |
| `haliphron_cluster_capacity_slots` | gauge | `cluster` | As last reported by the cluster |
| `haliphron_cluster_free_slots` | gauge | `cluster` | As last reported by the cluster |
| `haliphron_cluster_last_heartbeat_timestamp_seconds` | gauge | `cluster` | Unix time of the last heartbeat |
| `haliphron_cluster_status` | gauge | `cluster`, `status` | `1` for the cluster's status, `0` for the other three |
| `haliphron_stats_up` | gauge | | `0` when the database could not be read. In that case the series above are absent from that scrape. |

The Go runtime (`go_*`) and process (`process_*`) collectors are included.
The controller exposes its own metrics, on its own metrics port.

Some queries:

```promql
# runs finished per hour, by cluster and status
sum by (cluster, status) (delta(max without(instance, pod) (haliphron_runs{status=~"Succeeded|Failed|TimedOut|Cancelled|CompletedWithoutResult"})[1h:]))

# p95 run duration per cluster over the last day
histogram_quantile(0.95, sum by (cluster, le) (delta(max without(instance, pod) (haliphron_run_duration_seconds_bucket)[1d:])))

# a cluster that has not heartbeated for five minutes
time() - max without(instance, pod) (haliphron_cluster_last_heartbeat_timestamp_seconds) > 300
```
