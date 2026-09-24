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
| MCP protocol revision | `2025-06-18` |
| Server name | `haliphron`, version `1` |
| Capabilities | `tools` |
| Maximum request body | 8 MiB |
| Authentication | `Authorization: Bearer <token>`, same tokens and scopes as the REST API |

There is no SSE and no long-lived session. A run that takes ten minutes is
waited for by the caller, not pushed to it.

## Methods

| Method | Authenticated | Result |
|---|---|---|
| `initialize` | no | `protocolVersion`, `capabilities`, `serverInfo` |
| `ping` | no | `{}` |
| `tools/list` | yes | `{"tools": [ ... ]}` |
| `tools/call` | yes | a [tool result](#tool-results) |

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

## `list_clusters`

The clusters registered with this control plane, and their capacity. Takes no
arguments.

Scope: `runs:read`.

**Not offered to a per-run token.** `tools/list` omits this tool when the
caller is an agent inside a pod. It exists for an operator without a UI.

---

## Per-run tokens

An agent pod holds a `run-mcp` token, scoped to the life of one run. A run it
starts with `run_agent` is bound to the calling run as its parent, is one level
deeper, and inherits the parent's `role` and `repo` when it names neither.

The maximum depth is 8.

Such a caller sees five tools, not six.
