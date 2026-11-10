# MCP tools

The Model Context Protocol listener, served on `:8081` by default. It exposes
the same use cases as the [REST API](rest-api.md), spoken as tools, so that an
IDE, another agent or an automation platform can start runs without an HTTP
client.

Defined in [`backend/mcp/`](../../backend/mcp/).

## Endpoint

```
POST /mcp
```

| Property | Value |
|---|---|
| Transport | JSON-RPC 2.0 over HTTP, one POST per request |
| MCP protocol revision | `2025-06-18`; `2025-11-25` when the client asks for it |
| Server name | `haliphron`, version `1` |
| Capabilities | `tools`; plus `tasks` under `2025-11-25` — see [Tasks](#tasks) |
| Maximum request body | 8 MiB |
| Authentication | `Authorization: Bearer <token>`, same tokens and scopes as the REST API |

There is no SSE and no long-lived session. A run that takes ten minutes is
waited for by the caller, not pushed to it.

`initialize` answers `2025-11-25` to a client that requests exactly that, and
`2025-06-18` to every other request. Since there is no session, later requests
are read at the version in their `MCP-Protocol-Version` header: only a request
carrying `2025-11-25` sees anything to do with tasks. A request without the
header is answered as it was before tasks existed.

## Methods

| Method | Authenticated | Result |
|---|---|---|
| `initialize` | no | `protocolVersion`, `capabilities`, `serverInfo` |
| `ping` | no | `{}` |
| `tools/list` | yes | `{"tools": [ ... ]}` |
| `tools/call` | yes | a [tool result](#tool-results), or a task under `2025-11-25` |
| `tasks/get` | yes | a [task](#the-task-object) |
| `tasks/result` | yes | the run's tool result, once the task has ended |
| `tasks/cancel` | yes | the task, cancelled |

A JSON-RPC notification — a request with no `id` — is answered with HTTP `202`
and no body.

Any other method returns error `-32601`.

## Errors

| Code | Meaning |
|---|---|
| `-32700` | the body could not be read, or is not valid JSON |
| `-32600` | not a JSON-RPC 2.0 request |
| `-32601` | no such method |
| `-32602` | invalid parameters |
| `-32603` | internal error |
| `-32001` | unauthenticated: no bearer token, or the token is not usable |

`-32001` is outside the JSON-RPC reserved range, as the MCP specification
allows.

## Tool results

Every tool returns the same envelope:

```json
{
  "content": [{"type": "text", "text": "..."}],
  "structuredContent": { },
  "isError": false
}
```

`structuredContent` is the answer. The text block beside it is the same thing
rendered for a model to read.

---

## `run_agent`

Start one agent run. Returns its `run_id` immediately, or waits for the result
when `async` is false.

Scope: `runs:write`.

| Argument | Type | Required | Default | Notes |
|---|---|---|---|---|
| `prompt` | string | yes | — | what the agent should do |
| `agent` | string | no | installation default | `claude-code` or `codex` |
| `model` | string | no | installation default | |
| `role` | string | no | none | a configured role: its tools, model and files |
| `repo` | string | no | none | clone URL; omit for a run without a repository |
| `base_branch` | string | no | repository default | |
| `timeout_seconds` | integer | no | installation default | 60 to 86400 |
| `max_cost_usd` | string | no | none | ceiling for this run, as a decimal |
| `async` | boolean | no | `true` | `false` waits for the run to finish |
| `wait_seconds` | integer | no | — | how long to wait when `async` is false |

Returns a run object, the same shape the [REST API](rest-api.md#the-run-object)
returns.

## `get_run_result`

The state of a run, and its result when it has one.

Scope: `runs:read`.

| Argument | Type | Required | Notes |
|---|---|---|---|
| `run_id` | string | yes | |
| `wait_seconds` | integer | no | wait this long for the run to finish before answering |

## `cancel_run`

Ask a run to stop.

Scope: `runs:write`.

| Argument | Type | Required |
|---|---|---|
| `run_id` | string | yes |
| `reason` | string | no |

Delivery is asynchronous: the instruction reaches the cluster on its next
heartbeat.

## `list_runs`

Recent runs, most recent first.

Scope: `runs:read`.

| Argument | Type | Required | Default | Notes |
|---|---|---|---|---|
| `status` | array of string | no | all | |
| `role` | string | no | all | |
| `limit` | integer | no | — | 1 to 200 |

## `list_roles`

The roles a run may be started under. Takes no arguments.

Scope: `runs:read`.

Returns each role's name and [`description`](role-spec.md#description), and
nothing else of its spec. The structured content is:

```json
{
  "roles": [
    {"name": "coder", "description": "Implements a change and opens a pull request."},
    {"name": "reviewer", "description": ""}
  ]
}
```

A role saved without a description has `""`. The text block has one line per
role, `name — description`, or just the name when there is no description.
With no roles it is `(none)`.

Pass the chosen `name` as `run_agent`'s `role`.

## `list_clusters`

The clusters registered with this control plane, and their capacity. Takes no
arguments.

Scope: `runs:read`.

**Not offered to a per-run token.** `tools/list` omits this tool when the
caller is an agent inside a pod. It exists for an operator without a UI.

## Role configuration

Four tools let an operator's own agent set roles up without the UI. They are
the [REST role endpoints](rest-api.md#roles) under other names, with the same
scopes and the same validation: a role saved here and a role saved from the UI
are admitted by the same rules.

**Never offered to a per-run token**, whatever scopes it carries. `tools/list`
omits them for an agent inside a pod, and a call from one is refused. A role
decides which tools, MCP servers, secrets and system prompt an agent gets. An
agent that could edit roles could widen its own children's permissions, and it
may be acting on a prompt injection from the repository it works in.

| Tool | Scope | Offered when |
|---|---|---|
| `get_role` | `runs:read` | the token has `runs:read` |
| `put_role` | `admin` | the token has `admin` |
| `delete_role` | `admin` | the token has `admin` |
| `inspect_marketplace` | `admin` | the token has `admin` |

A refusal is a tool result with `isError` set. A validation refusal starts with
the field it concerns, such as `configFiles: ...` or `secretEnv[0].secret: ...`,
so the agent can correct the spec and call again.

### `get_role`

One role's whole spec.

| Argument | Type | Required |
|---|---|---|
| `name` | string | yes |

The structured content is the same object as `GET /api/v1/roles/{name}`:

```json
{"name": "coder", "spec": { }, "created_by": "alice", "updated_at": "2026-11-10T10:00:00Z"}
```

The text block is `role <name>`, then the spec as indented JSON. A role that
does not exist is `no role named "<name>"`.

### `put_role`

Creates a role, or replaces an existing role's spec whole.

| Argument | Type | Required | Notes |
|---|---|---|---|
| `name` | string | yes | lower-case letters, digits, `-`, `_` and `.`, starting and ending with a letter or digit; at most 253 characters |
| `spec` | object | yes | the [role spec](role-spec.md) |

Nothing is merged: a field left out of `spec` is removed from the role. To
edit, call `get_role`, change the spec, and send it back. Runs that were
already admitted keep the spec they were admitted with. A `spec` passed as a
string that holds a JSON object is accepted as that object.

The answer is the saved role, shaped as in `get_role`. `created_by` is the
token's `subject`, or `token:<name>`.

### `delete_role`

| Argument | Type | Required |
|---|---|---|
| `name` | string | yes |

A soft delete, as over REST. Runs already admitted under the role keep running
and keep naming it. A new run naming it is refused, and the name can be used
again. The answer is `{"name": "...", "deleted": true}`.

### `inspect_marketplace`

Reads a claude-code plugin marketplace's manifest with the installation's git
token. Use it to fill a role's `plugins` before `put_role`.

| Argument | Type | Required | Notes |
|---|---|---|---|
| `url` | string | yes | `owner/repo` or an https git URL, as `plugins.marketplaces[].url` takes it |
| `ref` | string | no | a branch or tag; default branch when omitted |

The structured content is the same as `POST /api/v1/plugin-marketplaces/inspect`:
`url`, `found`, `plugins` (`name`, and `version` and `description` when the
manifest has them), and, when set, `ref`, `name`, `description`,
`manifest_path` and `problem`. A marketplace that cannot be read is not a
refusal: `found` is `false` and `problem` says why.

## Resource locks

Four tools let agents that change shared infrastructure (a load balancer, a
database, DNS, a cluster) tell each other what they are working on, and hand
the rest of a task to a later run when what they need is busy. A lock is
advisory: a free-form name and description. The backend never compares two
locks, never refuses a lock because of another one, and never holds a run back
because of one. Each agent decides whether a lock concerns its own work. See
[Cooperative locks](../explanation/cooperative-locks.md) for the reasoning.

`set_lock`, `unset_lock` and `schedule_agent` need a per-run token: the holder
of a lock is the run whose token set it, never something named in the
arguments. `list_locks` is also offered to an operator's token with
`runs:read`.

### The protocol

1. Before you change shared infrastructure, call `set_lock`.
2. Read `earlier` in the answer. It lists the live locks of other runs that
   were set before yours. If any of them concerns the same infrastructure as
   your work, you are second: call `unset_lock`, then stop or call
   `schedule_agent` with that `lock_id`.
3. Locks set after yours are not your concern. Their holders see yours in their
   own `earlier`.
4. If any lock tool fails, do not start the change.

Every lock write takes one database-wide advisory lock, so locks commit in the
order of their `seq`. For any two locks, the answer to the later `set_lock`
contains the earlier one. When two agents agree that their work overlaps,
exactly one of them backs off.

Lock names and descriptions are written by other agents, so they can carry a
prompt injection. The text block puts them inside a
`<<< BEGIN LOCKS … >>>` / `<<< END LOCKS >>>` block that labels them as data,
and quotes each one.

### `set_lock`

Registers that the calling run is working on something, or refreshes a lock it
already holds.

| Argument | Type | Required | Default | Notes |
|---|---|---|---|---|
| `name` | string | yes | — | 1 to 128 bytes, one line. System and environment, such as `prod/db/orders` |
| `description` | string | yes | — | Up to 1024 bytes, one line. What is being done and for about how long |
| `ttl_seconds` | integer | no | the run's timeout | At least 60. A longer value is cut to the run's `timeout_seconds` |

Control characters, line breaks and bidirectional overrides are refused, not
removed. A run holds at most 8 live locks.

Calling it again with a name the run holds keeps the lock's `lock_id`, `seq`
and place in the order, and replaces its description and expiry. A lock that
had expired is replaced by a new one with a new place.

```json
{
  "lock": {"lock_id": "01JD…", "name": "prod/db/orders", "description": "…", "expires_at": "…", "seq": 42},
  "earlier": [
    {"lock_id": "01JC…", "seq": 41, "name": "prod/db/orders", "description": "…",
     "held_by_run": "01JB…", "holder_status": "Running",
     "holder_started_at": "…", "holder_last_activity_at": "…",
     "acquired_at": "…", "expires_at": "…"}
  ]
}
```

A conflict is not an error. `isError` is set only for a refusal: a bad
argument, the limit of 8, a run that has ended.

### `unset_lock`

Releases the calling run's locks.

| Argument | Type | Required | Notes |
|---|---|---|---|
| `lock_id` | string | no | |
| `name` | string | no | |

With neither argument, every lock of the calling run is released. Another
run's lock is never released, and naming one is not an error: `removed` is
simply empty. The answer is `{"removed": ["01JD…"]}`.

### `list_locks`

Every live lock, oldest first. Takes no arguments. Each entry has the same
fields as an `earlier` entry, plus `mine` when the caller is a run.
`holder_last_activity_at` moves with every heartbeat the holder's cluster
sends. A holder that still says `Running` but went quiet minutes ago is on a
cluster that stopped reporting.

No prompt, result or credential of the holder is returned.

### `schedule_agent`

Hands the rest of the task to a new run, which starts once none of the given
locks is held, at a time, or both. The calling run should then finish.

| Argument | Type | Required | Default | Notes |
|---|---|---|---|---|
| `prompt` | string | yes | — | The rest of the task, written to stand on its own |
| `wait_for_locks` | array of string | no | — | `lock_id` values. Names are not accepted: they are not unique across runs |
| `not_before` | string | no | — | RFC 3339 |
| `give_up_after_seconds` | integer | no | 86400 | 60 to 604800 |
| `model` | string | no | the parent's default | |
| `timeout_seconds` | integer | no | the default | 60 to 86400 |

At least one of `wait_for_locks` and `not_before` is required. The new run is
a child of the calling run. It inherits the role, the repository and the cost
ceiling, is one level deeper, and counts against the parent's limit of 10
children. A run started from chat replies in the same thread when the
continuation ends. The request is checked against admission when the tool is
called, so a deleted role or a bad timeout is refused now, not when the run
starts.

If every lock it names has already been released and there is no
`not_before`, nothing is saved, and the answer tells the agent to carry on
itself. If the run cannot start before `give_up_after_seconds` passes, it
expires. A run that admission refuses when it is due also expires, with the
reason. The answer is
`{"deferred_id": "…", "state": "waiting", "give_up_at": "…", "wait_for_locks": [...], "not_before": …}`.

When the locks go, every run waiting on them starts. If two of them want the
same thing, both call `set_lock`, and the protocol makes the later one yield
and defer again.

The calling run usually ends `Succeeded`, which means only that it exited with
code 0. The run's page in the UI shows that it deferred work.

---

## Tasks

A client that negotiated `2025-11-25` can call `run_agent` as an
[MCP task](https://modelcontextprotocol.io/specification/2025-11-25/basic/utilities/tasks):
the call returns at once with a task, and the host polls `tasks/get` and
collects the result with `tasks/result`, rather than the model calling
`get_run_result`. The feature is experimental in the specification.

| Property | Value |
|---|---|
| Declared capability | `tasks.requests.tools.call`, `tasks.cancel` |
| `tasks/list` | not offered: use `list_runs` |
| Tools that may be tasks | `run_agent`, with `execution.taskSupport: "optional"` |
| Task ID | the run ID |
| `ttl` | the shorter of the failed-run retention (3 days by default) and `HALIPHRON_RUN_RETENTION`, in milliseconds; `null` when both keep runs forever |
| `pollInterval` | 5000 |
| Status notifications | none: poll |

`run_agent` without a `task` field works exactly as documented above. With
one, `async` and `wait_seconds` are ignored, and the answer is:

```json
{
  "task": {
    "taskId": "01J...",
    "status": "working",
    "statusMessage": "run is Queued",
    "createdAt": "2026-09-30T10:30:00Z",
    "lastUpdatedAt": "2026-09-30T10:30:00Z",
    "ttl": null,
    "pollInterval": 5000
  },
  "_meta": {"io.modelcontextprotocol/model-immediate-response": "Run 01J... has started as a task. ..."}
}
```

A `task` field on any other tool is error `-32601`. A `task` field from a
client that did not negotiate `2025-11-25` is ignored, and the call is served
as a plain call.

### The task object

Nothing is stored for a task. Its status is derived from the run on every read:

| Task status | When |
|---|---|
| `working` | the run has not ended, or it has ended and its report is still being collected |
| `completed` | the run is `Succeeded`: the agent exited 0, and nothing more is implied |
| `failed` | the run is `Failed` or `TimedOut` |
| `cancelled` | a cancellation was requested, through any interface, or the run is `Cancelled` |

A cancelled task stays cancelled. Cancellation reaches the cluster on its next
heartbeat, so the run can still finish afterwards; the run shows what happened,
while the task stays `cancelled`.

A run that has ended while its report is still missing stays `working` for up to
five minutes after it finished. After that the task ends with the run as it
is, usually `CompletedWithoutResult`, so a lost report cannot block a waiter
forever.

The task is the run, so an operator retry, which puts the run back to
`Queued`, puts the task back to `working`. That is the one transition the
specification does not allow. A host that has already collected the result
does not see it.

### `tasks/result`

Blocks until the task has ended, then answers with what `run_agent` answers
after waiting for the end: the run object in `structuredContent`, and the same
text as `get_run_result`. `isError` is `true` when the task `failed`. Here
this differs from the plain `run_agent`, whose failed run is not a tool
error. The result carries `_meta["io.modelcontextprotocol/related-task"]`.

A blocked `tasks/result` holds the request open for as long as the run takes,
so a gateway in front of the listener can cut it off with its own timeout. The
run is unaffected: the host goes back to `tasks/get`, and calls `tasks/result`
again once the task has ended.

### `tasks/cancel`

`cancel_run` with no reason, answered with the task, now `cancelled`. Needs
`runs:write`. A task that has already ended, or a run that has ended while its
report is still being collected, is error `-32602`.

### Errors

| Case | Code |
|---|---|
| unknown task, malformed ID, or a run the token does not reach | `-32602` `Failed to retrieve task: Task not found` |
| a refused admission for `run_agent` as a task: bad argument, missing scope, a depth or child limit | `-32602` with the same message the plain call would return as a tool error |
| cancelling a task that has ended | `-32602` |
| an internal failure | `-32603` |

A refused admission is a protocol error, not a tool error. The call can only
be answered with a task, and when nothing is admitted there is no task.

A task is bound to the token's reach, the same bound as `get_run_result`. A
service token with `runs:read` reaches every run. A per-run token reaches its
own run and its children. A run outside that reach gets the same answer as one
that does not exist.

---

## Per-run tokens

An agent pod holds a `run-mcp` token, scoped to the life of one run. A run it
starts with `run_agent` is bound to the calling run as its parent, is one level
deeper, and inherits the parent's `role` and `repo` when it names neither.

The maximum depth is 8.

Such a caller does not see `list_clusters` or any of the
[role configuration](#role-configuration) tools, and does see the four lock
tools: `set_lock`, `unset_lock`, `list_locks` and `schedule_agent`. An
operator's token sees `list_locks` and `get_role` (with `runs:read`),
`put_role`, `delete_role` and `inspect_marketplace` (with `admin`), and
`list_clusters`, but cannot hold a lock or defer work.
