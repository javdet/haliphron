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
| `systemPrompt` | string | **appended** to the agent's system prompt, never substituted; at most 32 KiB; see below |
| `env` | array of [EnvVar](#envvar) | |
| `resources` | [Resources](#resources) | |
| `mcpServers` | array of [MCPServer](#mcpserver) | |
| `nodeSelector` | object of string to string | replaces the installation default outright |
| `tolerations` | array of [Toleration](#toleration) | replaces the installation default outright |
| `toolPolicy` | [ToolPolicy](#toolpolicy) | omit it, or leave `allow` empty, for everything the installation permits |
| `plugins` | [PluginSpec](#pluginspec) | marketplaces to fetch and plugins to install |
| `configFiles` | object of string to string | **flat** filename to contents — not a path; see below |
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
  "plugins": {
    "marketplaces": [{"name": "playneta", "url": "playneta/claude-plugin"}],
    "enabled": ["playneta-infra-coder@playneta"]
  },
  "configFiles": {
    "settings.coder.json": "{\"outputStyle\":\"concise\"}"
  },
  "clusterSelector": {"region": "eu-central-1"}
}
```

---

### PluginSpec

| Field | Type | Notes |
|---|---|---|
| `marketplaces` | array of [PluginMarketplace](#pluginmarketplace) | at most 16 |
| `enabled` | array of string | `plugin@marketplace`, at most 64 |
| `trustRepositorySources` | boolean | default **true**; see below |

### PluginMarketplace

| Field | Type | Notes |
|---|---|---|
| `name` | string | the name the catalogue declares for itself — the `@suffix` in `enabled` |
| `url` | string | `owner/repo` or an `https://` git URL. Required |
| `ref` | string | branch or tag; empty means the default branch |

The marketplace name is **not** derivable from the repository: a catalogue in
`playneta/claude-plugin` may well call itself `playneta`, and that is the half
after the `@`.

Every `enabled` entry must name a marketplace the **same document** declares, or
the role is refused with a 422 naming the entry. The resolution chain picks one
source and that source is the only one that speaks, so a plugin whose catalogue
is declared somewhere else would never have it registered: the install could only
fail in the pod, after the lease was cut.

Only `owner/repo` and `https://` are accepted. The pod authenticates to git with
an https token and has no key, so an `ssh://` or `git@` source could not be
fetched; it is refused when the role is saved rather than an hour later in a
pod. A private repository is fetched with the run's own git credential, so that
credential must reach both the run's repository and the marketplace.

`trustRepositorySources` decides whether the cloned repository's own
`.claude/settings.<role>.json` may choose marketplaces and plugins, overriding
the list above. It defaults to true. A plugin is arbitrary code that runs beside
the agent, so set it to `false` for a role that runs against repositories the
installation does not control. The full resolution order is in
[the agent runtime contract](../contracts/agent-runtime.md).

### A note on `configFiles` keys

The keys are flat file names — `settings.coder.json` — and not paths. They
become ConfigMap data keys verbatim, and a ConfigMap key may not contain a
separator, so a role written with `".claude/settings.json"` in it would fail
materialisation in the cluster. It is refused when the role is saved rather than
rewritten, because a key silently rewritten is a file the role believes it
shipped and the pod never sees.

## `systemPrompt`

Text added to the agent's system prompt. It extends what is already there and
replaces nothing. The agent sees, in order:

1. the CLI's own system prompt — claude-code's tool instructions, left intact
2. haliphron's instruction: where structured output goes, where artifacts go,
   that the branch already exists and is not the agent's to push, and the
   node's output schema when there is one
3. the role's `systemPrompt`

On claude-code, 2 and 3 are passed together through `--append-system-prompt`.
codex has no equivalent flag, so they become a prefix of the prompt in the same
order.

A role cannot switch the rules in step 2 off. They are what the git phases rely
on, and a role that could replace them would produce runs that exit 0 and push
somewhere nobody expected.

It is material, not spec: the backend renders it into the per-run ConfigMap
under the reserved key `system-prompt.md`, and a file the role ships under that
name in `configFiles` is dropped. A prompt that is empty or only whitespace
ships no file.

The limit is 32 KiB (`MaxRoleSystemPromptBytes`), measured in bytes. The prompt
reaches claude-code as a single command-line argument, and Linux caps one
argument at 128 KiB. A longer prompt is refused with a 422 when the role is
saved.

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
