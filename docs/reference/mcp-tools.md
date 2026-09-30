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
| `ttl` | `null`, or the run retention in milliseconds when `HALIPHRON_RUN_RETENTION` is set |
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

Such a caller sees five tools, not six.
