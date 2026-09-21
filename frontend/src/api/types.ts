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

export interface Role {
  name: string
  /** An opaque document: the backend stores and replaces it whole. */
  spec: unknown
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
