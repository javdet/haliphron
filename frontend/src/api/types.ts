// The wire shapes of the public API, transcribed from backend/restapi.
//
// Hand-written because the backend publishes an OpenAPI document for the
// cluster and runtime contracts but not for this one. Where a name here
// disagrees with a `json:` tag over there, this file is the one that is wrong.

export type RunStatus =
  | 'Queued'
  | 'Leased'
  | 'Dispatched'
  | 'Starting'
  | 'Running'
  | 'Succeeded'
  | 'Failed'
  | 'TimedOut'
  | 'Cancelled'
  | 'CompletedWithoutResult'
  | 'Unknown'

export const RUN_STATUSES: RunStatus[] = [
  'Queued',
  'Leased',
  'Dispatched',
  'Starting',
  'Running',
  'Succeeded',
  'Failed',
  'TimedOut',
  'Cancelled',
  'CompletedWithoutResult',
]

/** The statuses after which nothing more arrives, so polling can stop. */
export const TERMINAL_STATUSES: ReadonlySet<string> = new Set<RunStatus>([
  'Succeeded',
  'Failed',
  'TimedOut',
  'Cancelled',
  'CompletedWithoutResult',
])

export type FailureClass = 'none' | 'infra' | 'agent' | 'git' | 'config' | 'budget'

export type AgentType = 'claude-code' | 'codex'

export const AGENT_TYPES: AgentType[] = ['claude-code', 'codex']

export interface Run {
  run_id: string
  status: RunStatus
  agent: AgentType | string
  model: string
  role?: string

  repo?: string
  base_branch?: string
  target_branch?: string

  cluster_id?: string
  epoch: number
  attempt: number

  observed_phase?: string
  failure_class?: FailureClass
  status_reason?: string
  status_message?: string
  exit_code?: number

  result_summary?: string
  pr_url?: string
  pr_number?: number
  commit_sha?: string

  /** Decimal money as a string; never parsed into a float for display. */
  cost_usd: string
  input_tokens: number
  output_tokens: number
  num_turns: number

  parent_run_id?: string
  depth: number

  created_by: string
  created_via: string
  created_at: string
  started_at?: string
  finished_at?: string
}

export interface RunList {
  runs: Run[]
  next_before?: string
}

export interface CreateRunRequest {
  prompt: string
  agent?: string
  model?: string
  role?: string
  repo?: string
  base_branch?: string
  target_branch?: string
  create_pr?: boolean
  timeout_seconds?: number
  max_cost_usd?: string
  max_turns?: number
  priority?: number
  async?: boolean
  wait_seconds?: number
}

export interface Attempt {
  attempt: number
  epoch: number
  cluster_id: string

  phase?: string
  reason?: string
  message?: string
  exit_code?: number
  failure_class?: FailureClass

  job_name?: string
  pod_name?: string
  node_name?: string

  cost_usd: string
  input_tokens: number
  output_tokens: number

  declared_duration_ms?: number
  observed_duration_ms?: number

  started_at?: string
  finished_at?: string
  completion_received_at?: string
}

export interface LogChunk {
  key: string
  size_bytes: number
  at: string
  /**
   * Relative in relay mode (this backend's own chunk endpoint, which wants the
   * bearer token) and absolute in object-store mode (a presigned link, which
   * must not be sent one). See fetchArtifact.
   */
  url: string
}

export interface LogPage {
  chunks: LogChunk[]
  next_after?: string
}

export type PermissionMode = '' | 'plan' | 'acceptEdits' | 'bypassPermissions'

export const PERMISSION_MODES: { value: PermissionMode; label: string }[] = [
  { value: '', label: 'Platform default' },
  { value: 'plan', label: 'plan — read only' },
  { value: 'acceptEdits', label: 'acceptEdits — write in the workspace' },
  { value: 'bypassPermissions', label: 'bypassPermissions — no sandbox' },
]

export type McpTransport = 'stdio' | 'http' | 'sse'

export interface McpServer {
  name: string
  transport: McpTransport
  url?: string
  command?: string
  args?: string[]
}

export interface PluginMarketplace {
  /** The name the catalogue declares for itself — the `@suffix` in `enabled`. */
  name?: string
  /** `owner/repo` or an https git URL. */
  url: string
  /** A branch or tag; empty means the default branch. */
  ref?: string
}

export interface PluginSpec {
  marketplaces?: PluginMarketplace[]
  /** `plugin@marketplace`, one per plugin. */
  enabled?: string[]
  /** Unset means true: the repository's own settings may choose plugins. */
  trustRepositorySources?: boolean
}

export interface ToolPolicy {
  allow?: string[]
  deny?: string[]
}

/**
 * A role spec, as the backend parses it.
 *
 * camelCase, unlike the rest of this file: the envelope around a role is the
 * public API's snake_case, and the document inside it is the same vocabulary
 * the machine contracts use. See docs/reference/role-spec.md.
 *
 * The index signature is not laziness. The backend ignores keys it does not
 * understand so that a role written for a newer control plane still runs, and
 * the form preserves them for the same reason — it round-trips what it cannot
 * render rather than dropping it on save.
 */
export interface RoleSpec {
  agent?: AgentType | string
  model?: string
  image?: string

  permissionMode?: PermissionMode
  maxTurns?: number
  /**
   * Appended to the agent's system prompt, after the CLI's own and after
   * haliphron's instruction. It replaces neither.
   */
  systemPrompt?: string
  env?: { name: string; value: string }[]
  mcpServers?: McpServer[]

  toolPolicy?: ToolPolicy
  plugins?: PluginSpec

  configFiles?: Record<string, string>
  clusterSelector?: Record<string, string>

  [key: string]: unknown
}

export interface Role {
  name: string
  /** Stored and replaced whole; unknown keys survive a round trip. */
  spec: RoleSpec
  created_by: string
  updated_at: string
}

export interface Cluster {
  cluster_id: string
  name: string
  labels?: Record<string, string>
  status: string
  agent_namespace: string
  controller_version: string
  k8s_version?: string
  runtimes?: string[]
  capacity_slots: number
  free_slots: number
  quota_exhausted: boolean
  registered_at: string
  last_heartbeat_at?: string
  revoked_reason?: string
}

export interface BootstrapToken {
  token_id: string
  name: string
  expires_at?: string
  max_uses: number
  uses: number
  created_by: string
  created_at: string
  revoked_at?: string
}

/** The one-time answer to a bootstrap-token creation. */
export interface BootstrapTokenSecret {
  token_id: string
  name: string
  token: string
  expires_at?: string
  max_uses: number
}

export type Scope = 'runs:read' | 'runs:write' | 'admin'

export const SCOPES: Scope[] = ['runs:read', 'runs:write', 'admin']

export interface ApiToken {
  token_id: string
  name: string
  kind: string
  scopes: Scope[]
  subject?: string
  run_id?: string
  expires_at?: string
  created_at: string
  revoked_at?: string
}

/** The one-time answer to a token creation. */
export interface ApiTokenSecret {
  token_id: string
  name: string
  token: string
  scopes: Scope[]
  expires_at?: string
}

export interface Secret {
  name: string
  kind: string
  ref?: string
  updated_at?: string
  rotated_at?: string
}

/** The error envelope every non-2xx answer carries. */
export interface ErrorBody {
  error: { code: string; message: string; field?: string }
}
