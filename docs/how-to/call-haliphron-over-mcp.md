# How to call Haliphron from another agent over MCP

The MCP listener exposes the same use cases as the REST API, spoken as tools.
Use it when the caller is an IDE, an agent, or an automation platform that
already speaks MCP and should not grow an HTTP client.

## Point a client at it

The endpoint is `POST /mcp` on the MCP listener — port 8081 by default, and a
separate entrypoint from the REST API.

```json
{
  "mcpServers": {
    "haliphron": {
      "type": "http",
      "url": "https://mcp.haliphron.example.com/mcp",
      "headers": {
        "Authorization": "Bearer hlt_..."
      }
    }
  }
}
```

The transport is JSON-RPC 2.0 over a single HTTP endpoint: one POST per
request, the answer in the body. There is no SSE and no session to keep alive.

## Issue the client a token

Give it the narrowest scope that does the job. A client that only submits and
reads runs does not need `admin`.

```sh
curl -sX POST https://haliphron.example.com/api/v1/tokens \
  -H "Authorization: Bearer $ADMIN_TOKEN" -H 'Content-Type: application/json' \
  -d '{"name":"ide-alice","scopes":["runs:read","runs:write"],"subject":"alice"}'
```

`subject` is recorded as `created_by` on every run the client starts, which is
what makes the audit log readable later.

`list_clusters` requires `runs:read` like the rest, but is offered only to
tokens that are not per-run tokens.

## Check it from the command line first

```sh
curl -sX POST https://mcp.haliphron.example.com/mcp \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/list"}'
```

Six tools come back — five if the token is a per-run token. `initialize` and
`ping` need no token; everything else does.

## Start a run

```sh
curl -sX POST https://mcp.haliphron.example.com/mcp \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{
    "jsonrpc": "2.0", "id": 2,
    "method": "tools/call",
    "params": {
      "name": "run_agent",
      "arguments": {
        "prompt": "Fix the flaky test in internal/queue and add a regression test.",
        "repo": "https://github.com/acme/widgets.git",
        "role": "coder"
      }
    }
  }'
```

The answer carries `structuredContent` — the run object — and a text block
rendering the same thing for a model to read.

`async` defaults to true, so this returns a `run_id` rather than waiting. The
arguments are listed in the [MCP tools
reference](../reference/mcp-tools.md#run_agent).

## Wait for the result instead of polling

```json
{"name": "get_run_result", "arguments": {"run_id": "01J...", "wait_seconds": 600}}
```

Or make the submission itself block:

```json
{"name": "run_agent", "arguments": {"prompt": "...", "async": false, "wait_seconds": 600}}
```

Both hold a connection open. For anything long, submit asynchronously and call
`get_run_result` on a loop.

## Let agents start their own runs

An agent pod is issued a per-run MCP token for the life of its run, so an
agent can delegate by calling `run_agent` itself.

Such a run is bound to its caller: `parent_run_id` is set, `depth` is one
greater, and an omitted `role` or `repo` is inherited from the parent. Cost is
charged to the right budget because the chain is recorded.

Two ceilings bound this, both on the control-plane chart:

```sh
helm upgrade haliphron deploy/charts/haliphron -n haliphron --reuse-values \
  --set agent.maxRunDepth=4 \
  --set agent.maxRunChildren=10
```

A run past the depth limit is refused at admission.

To trace a chain afterwards:

```sh
curl -s "https://haliphron.example.com/api/v1/runs?parent_run_id=$RUN_ID" \
  -H "Authorization: Bearer $TOKEN"
```

For the pod's own `mcp.json` to be written, the installation needs to know
where the MCP endpoint is reachable from inside a cluster:

```sh
helm upgrade haliphron ... --set mcpEndpoint=https://mcp.haliphron.example.com/mcp
```

## Expose MCP outward without exposing REST

Install the chart twice against the same database, one release per mode:

```sh
helm install haliphron-mcp deploy/charts/haliphron -n haliphron \
  --set backend.mode=mcp \
  --set ingress.mcp.enabled=true --set ingress.mcp.host=mcp.haliphron.example.com \
  ...
```

The mode is part of the Service selector, so the releases do not select each
other's pods. Exactly one release must serve `cluster` or `all`.

## Read an error

Failures come back as JSON-RPC errors, not as HTTP status codes.

| Code | Meaning |
|---|---|
| `-32001` | the token is missing or not usable |
| `-32602` | invalid arguments |
| `-32601` | no such method |

`-32001` does not distinguish unknown, revoked and expired tokens.
