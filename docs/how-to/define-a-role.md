# How to define a role

A role is a reusable agent configuration: a model, a tool policy, MCP servers,
resource limits and a set of files. Define one when you find yourself sending
the same settings with every run.

You need a token carrying `admin`. Reading roles needs only `runs:read`.

## Create one

```sh
curl -sX PUT https://haliphron.example.com/api/v1/roles/coder \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{
    "agent": "claude-code",
    "model": "anthropic/claude-opus-5",
    "permissionMode": "acceptEdits",
    "maxTurns": 40,
    "resources": {"cpu": "2", "memory": "4Gi", "ephemeralStorage": "20Gi"}
  }'
```

The spec is **camelCase**, unlike the rest of the public API.

Then name it on a run:

```sh
curl -sX POST https://haliphron.example.com/api/v1/runs \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"prompt":"...","repo":"...","role":"coder"}'
```

Anything the request states wins over the role. Anything neither states comes
from the installation defaults.

The full field list is in the [role spec
reference](../reference/role-spec.md).

## Restrict what the agent may run

```sh
-d '{
  "permissionMode": "acceptEdits",
  "toolPolicy": {
    "deny": ["Bash(rm:*)", "Bash(curl:*)", "WebFetch"]
  }
}'
```

The role's policy is intersected with the installation's ceiling
(`agent.toolPolicy` on the chart). A deny in the ceiling cannot be lifted by a
role — so put anything that must never happen anywhere in the ceiling, and
per-role restrictions here.

The intersection is computed once, at admission. Neither the controller nor
the pod recomputes it.

## Give the agent extra tools

```sh
-d '{
  "mcpServers": [
    {"name": "jira", "transport": "http", "url": "https://mcp.internal/jira"},
    {"name": "grafana", "transport": "http", "url": "https://mcp.internal/grafana"}
  ]
}'
```

The `mcp-verify` phase proves the servers came up before the agent starts. A
server that does not answer fails the run with exit 30.

Agent pods reach only the CIDRs allowed by the agents' NetworkPolicy, which
excludes the cluster's own private ranges by default. An MCP server on an
internal address is unreachable until you add its CIDR to
`agents.networkPolicy.allowedCIDRs` on the runtime chart.

## Ship configuration files with the role

`configFiles` become a ConfigMap in the target cluster, mounted into
`/haliphron/role/`.

```sh
-d '{
  "configFiles": {
    ".claude/settings.json": "{\"outputStyle\":\"concise\"}",
    "CONVENTIONS.md": "Always add a regression test alongside a fix.\n"
  }
}'
```

These are the fallback layer of the entrypoint's role resolution chain. A
repository that carries its own equivalents wins over them, so a role can set
house defaults without overriding a team that has decided otherwise.

## Pin a role to particular clusters

```sh
-d '{"clusterSelector": {"region": "eu-central-1"}}'
```

Matched against a cluster's `cluster.labels`. Use it to keep work with data
residency requirements in one place.

A run whose role matches no `Active` cluster stays `Queued`.

## Size the pod

```sh
-d '{"resources": {"cpu": "4", "memory": "8Gi", "ephemeralStorage": "40Gi"}}'
```

Requests are set equal to limits, which puts the pod in the Guaranteed QoS
class.

Set `ephemeralStorage` for roles that clone large repositories or install
dependencies. Without it the pod is bounded only by the namespace's
LimitRange, and a pod that fills a node's disk shows up as unrelated pods
being evicted.

Check the limits against `agents.limitRange.max` on the runtime chart — 8 CPU
and 32Gi by default. A pod above the maximum is refused by admission, not
clamped.

## Change one

`PUT` the whole spec again. There are no merge semantics: a role is edited by
something holding the whole object, and a half-applied merge on a document
whose shape changes with the product is how a run gets the wrong tool policy.

```sh
curl -s https://haliphron.example.com/api/v1/roles/coder \
  -H "Authorization: Bearer $TOKEN" | jq .spec > coder.json
# edit coder.json
curl -sX PUT https://haliphron.example.com/api/v1/roles/coder \
  -H "Authorization: Bearer $ADMIN_TOKEN" -H 'Content-Type: application/json' \
  -d @coder.json
```

**Editing a role does not change runs already admitted under it**, including
their later attempts. Your edit applies to the next run.

## Retire one

```sh
curl -sX DELETE https://haliphron.example.com/api/v1/roles/coder \
  -H "Authorization: Bearer $ADMIN_TOKEN"
```

A soft delete. Runs already admitted under the role keep naming it, and their
history stays readable.

## List what exists

```sh
curl -s https://haliphron.example.com/api/v1/roles \
  -H "Authorization: Bearer $TOKEN"
```

Callers can also list roles over MCP with `list_roles`, which is how an agent
discovers what it may start a child run under.
