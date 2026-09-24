# Role spec

A role is a reusable agent configuration: a model, a tool policy, MCP servers,
resource limits and a set of files. A run names a role, and the role supplies
what the request did not state.

Roles are created and replaced with `PUT /api/v1/roles/{name}`; see the [REST
API reference](rest-api.md#roles). The request body is the spec object
documented here.

Defined in [`backend/app/role.go`](../../backend/app/role.go) and
[`api/run/v1/runspec.go`](../../api/run/v1/runspec.go).

## Naming

The spec is **camelCase**, unlike the rest of the public API. It is stored
whole as JSON and its shape follows the rendered run spec, which is a machine
contract.

Fields this build does not understand are ignored, not refused. A role written
for a newer control plane still runs.

## Top-level fields

Every field is optional.

| Field | Type | Notes |
|---|---|---|
| `agent` | string | `claude-code` or `codex` |
| `model` | string | |
| `image` | string | overrides the installation's agent image |
| `permissionMode` | string | `default`, `acceptEdits`, `bypassPermissions` or `plan` |
| `maxTurns` | integer | |
| `env` | array of [EnvVar](#envvar) | |
| `resources` | [Resources](#resources) | |
| `mcpServers` | array of [MCPServer](#mcpserver) | |
| `nodeSelector` | object of string to string | replaces the installation default outright |
| `tolerations` | array of [Toleration](#toleration) | replaces the installation default outright |
| `toolPolicy` | [ToolPolicy](#toolpolicy) | |
| `configFiles` | object of string to string | filename to contents |
| `clusterSelector` | object of string to string | restricts placement to clusters carrying these labels |

### Example

```json
{
  "agent": "claude-code",
  "model": "anthropic/claude-opus-5",
  "permissionMode": "acceptEdits",
  "maxTurns": 40,
  "resources": {"cpu": "2", "memory": "4Gi", "ephemeralStorage": "20Gi"},
  "toolPolicy": {"deny": ["Bash(rm:*)"]},
  "configFiles": {
    ".claude/settings.json": "{\"outputStyle\":\"concise\"}"
  },
  "clusterSelector": {"region": "eu-central-1"}
}
```

---

## `configFiles`

Filename to contents. These become a ConfigMap in the target cluster, mounted
into `/haliphron/role/`.

They travel beside the run spec and never inside it: whatever the controller
materialises is not a spec field.

They are the fallback layer of the entrypoint's role resolution chain — used
when the repository does not carry its own equivalents.

## `toolPolicy`

| Field | Type | Limits |
|---|---|---|
| `allow` | array of string | at most 256 entries, each at most 256 characters |
| `deny` | array of string | at most 256 entries, each at most 256 characters |

The stored policy is intersected with the installation's policy ceiling
(`HALIPHRON_TOOL_ALLOW` and `HALIPHRON_TOOL_DENY`; see
[configuration](configuration.md#tool-policy-ceiling)).

A deny in the ceiling cannot be lifted by a role.

The intersection is computed by the backend at admission. Neither the
controller nor the pod recomputes it.

## `resources`

Container limits. The controller sets requests equal to limits, which places
the pod in the Guaranteed QoS class.

| Field | Type | Pattern |
|---|---|---|
| `cpu` | string | `^[0-9]+(\.[0-9]+)?(m)?$` |
| `memory` | string | `^[0-9]+(\.[0-9]+)?(Ki\|Mi\|Gi\|Ti\|k\|M\|G\|T)?$` |
| `ephemeralStorage` | string | same as `memory` |

Each is at most 32 characters. A malformed quantity is refused, not dropped.

A pod with no `ephemeralStorage` limit is bounded only by the namespace's
[LimitRange](helm-values.md#limit-range).

## `mcpServers`

| Field | Type | Required | Limits |
|---|---|---|---|
| `name` | string | yes | 1 to 128 characters |
| `transport` | string | yes | `stdio`, `http` or `sse` |
| `url` | string | no | at most 2048 characters |
| `command` | string | no | at most 512 characters |
| `args` | array of string | no | at most 64 entries, each at most 1024 characters |

The entrypoint's `mcp-verify` phase proves the servers came up before the
agent runs, and exits 30 when they did not.

## `env`

| Field | Type | Limits |
|---|---|---|
| `name` | string | `^[A-Za-z_][A-Za-z0-9_]*$`, at most 128 characters |
| `value` | string | at most 4096 characters |

## `toleration`

Mirrors the Kubernetes toleration.

| Field | Type | Values |
|---|---|---|
| `key` | string | at most 253 characters |
| `operator` | string | `Exists` or `Equal` |
| `value` | string | at most 253 characters |
| `effect` | string | `NoSchedule`, `PreferNoSchedule` or `NoExecute` |

---

## Resolution order

For any one setting, the value that reaches the pod is the first of:

1. the run request
2. the role
3. the installation default

except `toolPolicy` and `permissionMode`, which are intersected with the
policy ceiling rather than overridden by it.

`nodeSelector` and `tolerations` do not merge with the installation's
`agent.nodeSelector` and `agent.tolerations`. A role that sets either replaces
it, so reading a role states where its runs go.

A run's spec is rendered once at admission and handed out byte-for-byte on
every lease afterwards. Editing a role does not change what an already
admitted run executes — including its later attempts.

Deleting a role is a soft delete. Runs admitted under it keep naming it.
