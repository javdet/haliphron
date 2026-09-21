// A stand-in for the backend, for working on the UI without a control plane.
//
// It answers the public API's shapes — the ones in src/api/types.ts — from
// memory, and serves the built bundle from the same origin, which is the
// arrangement the packaged nginx produces. It is not a fake in the testing
// sense: nothing asserts against it, and it is not in the build.
//
//   npm run build && npm run mock     → http://127.0.0.1:8099  (token: dev)

import { createServer } from 'node:http'
import { readFile } from 'node:fs/promises'
import { extname, join, normalize } from 'node:path'
import { fileURLToPath } from 'node:url'

const DIST = fileURLToPath(new URL('../dist/', import.meta.url))
const PORT = Number(process.env.PORT ?? 8099)
const TOKEN = process.env.MOCK_TOKEN ?? 'dev'

// Relative to the moment of the request, not of the process start: a mock
// left running for an hour should not report every cluster as unreachable.
const iso = (msAgo) => new Date(Date.now() - msAgo).toISOString()

// Rebuilt per request for the same reason: the fixtures are written as "nine
// minutes ago", and a frozen snapshot turns that into "nine minutes ago plus
// however long the mock has been up".
function snapshot() {
const runs = [
  {
    run_id: '01JD8QK3R4ZP7WN2XA5T6M9BCD', status: 'Running', agent: 'claude-code',
    model: 'claude-opus-5', role: 'coder', repo: 'https://github.com/acme/checkout',
    base_branch: 'main', target_branch: 'agent/refund-flow',
    cluster_id: '01JD7A0000CLUSTEREUWEST1', epoch: 3, attempt: 1,
    observed_phase: 'Running', cost_usd: '1.8420', input_tokens: 184203,
    output_tokens: 20114, num_turns: 14, depth: 0,
    created_by: 'maria', created_via: 'api', created_at: iso(9 * 60000), started_at: iso(8 * 60000),
  },
  {
    run_id: '01JD8QJ0X1AA2BB3CC4DD5EEFF', status: 'Succeeded', agent: 'claude-code',
    model: 'claude-sonnet-5', role: 'code-reviewer', repo: 'https://github.com/acme/checkout',
    cluster_id: '01JD7A0000CLUSTEREUWEST1', epoch: 2, attempt: 1, observed_phase: 'Succeeded',
    result_summary: 'Reviewed 14 files; three findings, one blocking.',
    pr_url: 'https://github.com/acme/checkout/pull/4821', pr_number: 4821,
    commit_sha: '9f2c1ab4de77', cost_usd: '0.6130', input_tokens: 61004,
    output_tokens: 8210, num_turns: 6, depth: 1,
    parent_run_id: '01JD8QK3R4ZP7WN2XA5T6M9BCD',
    created_by: 'run:01JD8QK3R4ZP7WN2XA5T6M9BCD', created_via: 'agent',
    created_at: iso(41 * 60000), started_at: iso(40 * 60000), finished_at: iso(31 * 60000),
  },
  {
    run_id: '01JD8Q7NN9GG8HH7II6JJ5KKLL', status: 'Failed', agent: 'codex',
    model: 'gpt-5.1-codex', role: 'sre', repo: 'https://github.com/acme/platform',
    cluster_id: '01JD7A0000CLUSTERUSEAST1', epoch: 5, attempt: 3,
    observed_phase: 'Failed', failure_class: 'agent', status_reason: 'AgentExitNonZero',
    status_message: 'the agent exited 1 after the test suite failed twice',
    exit_code: 1, cost_usd: '3.0400', input_tokens: 301992, output_tokens: 44810,
    num_turns: 31, depth: 0, created_by: 'ci-pipeline', created_via: 'api',
    created_at: iso(3 * 3600000), started_at: iso(3 * 3600000 - 30000), finished_at: iso(2.4 * 3600000),
  },
  {
    run_id: '01JD8PZZAA1BB2CC3DD4EE5FFG', status: 'CompletedWithoutResult', agent: 'claude-code',
    model: 'claude-opus-5', role: 'devsecops', repo: 'https://github.com/acme/infra',
    cluster_id: '01JD7A0000CLUSTERUSEAST1', epoch: 4, attempt: 1, observed_phase: 'Succeeded',
    status_reason: 'ReportNotCollected', cost_usd: '0.0000', input_tokens: 0,
    output_tokens: 0, num_turns: 0, depth: 0, created_by: 'slack:dmitri', created_via: 'slack',
    created_at: iso(6 * 3600000), started_at: iso(6 * 3600000 - 20000), finished_at: iso(5.6 * 3600000),
  },
  {
    run_id: '01JD8PQQHH1JJ2KK3LL4MM5NNP', status: 'Queued', agent: 'claude-code',
    model: 'claude-opus-5', role: 'coder', repo: 'https://github.com/acme/billing',
    epoch: 0, attempt: 0, status_reason: 'NoEligibleCluster', cost_usd: '0.0000',
    input_tokens: 0, output_tokens: 0, num_turns: 0, depth: 0,
    created_by: 'maria', created_via: 'api', created_at: iso(70000),
  },
]

const attempts = {
  '01JD8Q7NN9GG8HH7II6JJ5KKLL': [
    { attempt: 1, epoch: 3, cluster_id: '01JD7A0000CLUSTERUSEAST1', phase: 'Failed',
      reason: 'ImagePullBackOff', failure_class: 'infra', job_name: 'hal-01jd8q7-1',
      pod_name: 'hal-01jd8q7-1-hm42x', node_name: 'ip-10-0-3-91',
      cost_usd: '0.0000', input_tokens: 0, output_tokens: 0,
      observed_duration_ms: 124000, started_at: iso(3 * 3600000) },
    { attempt: 2, epoch: 4, cluster_id: '01JD7A0000CLUSTERUSEAST1', phase: 'Failed',
      reason: 'AgentExitNonZero', message: 'go test ./... exited 1', failure_class: 'agent',
      exit_code: 1, job_name: 'hal-01jd8q7-2', pod_name: 'hal-01jd8q7-2-p8vqz',
      node_name: 'ip-10-0-3-91', cost_usd: '1.4100', input_tokens: 140881,
      output_tokens: 20390, declared_duration_ms: 611000, observed_duration_ms: 623400,
      started_at: iso(2.9 * 3600000), finished_at: iso(2.8 * 3600000) },
    { attempt: 3, epoch: 5, cluster_id: '01JD7A0000CLUSTERUSEAST1', phase: 'Failed',
      reason: 'AgentExitNonZero', message: 'the agent exited 1 after the test suite failed twice',
      failure_class: 'agent', exit_code: 1, job_name: 'hal-01jd8q7-3',
      pod_name: 'hal-01jd8q7-3-c2t7m', node_name: 'ip-10-0-4-12',
      cost_usd: '1.6300', input_tokens: 161111, output_tokens: 24420,
      declared_duration_ms: 702000, observed_duration_ms: 715900,
      started_at: iso(2.6 * 3600000), finished_at: iso(2.4 * 3600000),
      completion_received_at: iso(2.4 * 3600000 - 1200) },
  ],
}

const LOG = `[init]     haliphron agent-runtime 0.1.0 (claude-code)
[validate] run 01JD8QK3R4ZP7WN2XA5T6M9BCD, role coder, model claude-opus-5
[clone]    https://github.com/acme/checkout @ main → 9f2c1ab
[role]     8 skills, 3 mcp servers, tool policy: write-in-workspace
[run]      turn 1 · reading src/checkout/refund.go
[run]      turn 2 · reading src/checkout/refund_test.go
[run]      turn 6 · editing src/checkout/refund.go
[run]      turn 9 · go test ./src/checkout/... → ok (2.104s)
[run]      turn 14 · writing the summary
`

const roles = [
  { name: 'coder', spec: { agent: 'claude-code', model: 'claude-opus-5', allowed_tools: ['Read', 'Edit', 'Bash'] },
    created_by: 'maria', updated_at: iso(9 * 86400000) },
  { name: 'code-reviewer', spec: { agent: 'claude-code', model: 'claude-sonnet-5', allowed_tools: ['Read', 'Grep'] },
    created_by: 'maria', updated_at: iso(4 * 86400000) },
  { name: 'sre', spec: { agent: 'codex', model: 'gpt-5.1-codex', allowed_tools: ['Read', 'Bash'] },
    created_by: 'dmitri', updated_at: iso(2 * 86400000) },
  { name: 'devsecops', spec: { agent: 'claude-code', allowed_tools: ['Read'] },
    created_by: 'dmitri', updated_at: iso(86400000) },
]

const clusters = [
  { cluster_id: '01JD7A0000CLUSTEREUWEST1', name: 'prod-eu-west-1', status: 'Active',
    agent_namespace: 'haliphron-agents', controller_version: '0.1.0', k8s_version: '1.31.4',
    runtimes: ['claude-code', 'codex'], capacity_slots: 12, free_slots: 9,
    quota_exhausted: false, registered_at: iso(31 * 86400000), last_heartbeat_at: iso(4000) },
  { cluster_id: '01JD7A0000CLUSTERUSEAST1', name: 'prod-us-east-1', status: 'Active',
    agent_namespace: 'haliphron-agents', controller_version: '0.1.0', k8s_version: '1.30.8',
    runtimes: ['claude-code'], capacity_slots: 8, free_slots: 0, quota_exhausted: true,
    registered_at: iso(28 * 86400000), last_heartbeat_at: iso(9000) },
  { cluster_id: '01JD7A0000CLUSTERSANDBOX', name: 'sandbox', status: 'Active',
    agent_namespace: 'haliphron-agents', controller_version: '0.0.9',
    runtimes: ['claude-code'], capacity_slots: 4, free_slots: 4, quota_exhausted: false,
    registered_at: iso(12 * 86400000), last_heartbeat_at: iso(40 * 60000) },
  { cluster_id: '01JD7A0000CLUSTEROLDDEMO', name: 'demo-2024', status: 'Revoked',
    agent_namespace: 'haliphron-agents', controller_version: '0.0.4',
    capacity_slots: 2, free_slots: 0, quota_exhausted: false,
    registered_at: iso(120 * 86400000), last_heartbeat_at: iso(60 * 86400000),
    revoked_reason: 'decommissioned with the demo account' },
]

return { runs, attempts, LOG, roles, clusters }
}

const routes = [
  ['GET', /^\/api\/v1\/runs$/, (_m, url, { runs }) => {
    const wanted = url.searchParams.getAll('status')
    const role = url.searchParams.get('role')
    const agent = url.searchParams.get('agent')
    const q = (url.searchParams.get('q') ?? '').toLowerCase()
    let out = runs
    if (wanted.length) out = out.filter((r) => wanted.includes(r.status))
    if (role) out = out.filter((r) => r.role === role)
    if (agent) out = out.filter((r) => r.agent === agent)
    if (q) out = out.filter((r) => JSON.stringify(r).toLowerCase().includes(q))
    return { runs: out }
  }],
  ['GET', /^\/api\/v1\/runs\/([^/]+)$/, (m, _u, { runs }) => runs.find((r) => r.run_id === m[1]) ?? 404],
  ['GET', /^\/api\/v1\/runs\/([^/]+)\/attempts$/, (m, _u, { attempts }) => ({ attempts: attempts[m[1]] ?? [] })],
  ['GET', /^\/api\/v1\/runs\/([^/]+)\/logs$/, (m, _u, { LOG }) => ({
    chunks: [{ key: '000001.log', size_bytes: LOG.length, at: iso(60000), url: `/api/v1/runs/${m[1]}/logs/000001.log` }],
    next_after: '000001.log',
  })],
  ['GET', /^\/api\/v1\/runs\/([^/]+)\/logs\/([^/]+)$/, (_m, _u, { LOG }) => ({ text: LOG })],
  ['GET', /^\/api\/v1\/runs\/([^/]+)\/result$/, (m) => ({
    text: `# ${m[1]}\n\nRefund flow reworked: the partial-refund path now settles against the\noriginal capture instead of opening a second one.\n\n- src/checkout/refund.go — settle against the capture\n- src/checkout/refund_test.go — two cases for the partial path\n\nTests: ok (2.104s)\n`,
  })],
  ['POST', /^\/api\/v1\/runs\/([^/]+)\/(cancel|retry)$/, (m, _u, { runs }) => runs.find((r) => r.run_id === m[1]) ?? 404],
  ['POST', /^\/api\/v1\/runs$/, (_m, _u, { runs }) => runs[0]],
  ['GET', /^\/api\/v1\/roles$/, (_m, _u, { roles }) => ({ roles })],
  ['GET', /^\/api\/v1\/clusters$/, (_m, _u, { clusters }) => ({ clusters })],
  ['GET', /^\/api\/v1\/clusters\/bootstrap-tokens$/, () => ({
    bootstrap_tokens: [
      { token_id: '01JD7B0000BOOTSTRAP00001', name: 'prod-eu-west-1', expires_at: iso(-2 * 86400000),
        max_uses: 1, uses: 1, created_by: 'maria', created_at: iso(31 * 86400000) },
      { token_id: '01JD7B0000BOOTSTRAP00002', name: 'staging', expires_at: iso(-86400000),
        max_uses: 3, uses: 0, created_by: 'dmitri', created_at: iso(2 * 86400000) },
    ],
  })],
  ['GET', /^\/api\/v1\/tokens$/, () => ({
    tokens: [
      { token_id: '01JD7C0000TOKEN000000001', name: 'ci-pipeline', kind: 'service',
        scopes: ['runs:read', 'runs:write'], subject: 'ci', expires_at: iso(-60 * 86400000),
        created_at: iso(30 * 86400000) },
      { token_id: '01JD7C0000TOKEN000000002', name: 'maria-admin', kind: 'service',
        scopes: ['admin'], subject: 'maria', created_at: iso(44 * 86400000) },
      { token_id: '01JD7C0000TOKEN000000003', name: 'run-01JD8QK3', kind: 'run-mcp',
        scopes: ['runs:write'], run_id: '01JD8QK3R4ZP7WN2XA5T6M9BCD', created_at: iso(9 * 60000) },
      { token_id: '01JD7C0000TOKEN000000004', name: 'old-laptop', kind: 'service',
        scopes: ['runs:read'], created_at: iso(200 * 86400000), revoked_at: iso(20 * 86400000) },
    ],
  })],
  ['GET', /^\/api\/v1\/secrets$/, () => ({
    secrets: [
      { name: 'anthropic-api-key', kind: 'managed', updated_at: iso(60 * 86400000), rotated_at: iso(9 * 86400000) },
      { name: 'github-app-key', kind: 'managed', updated_at: iso(88 * 86400000) },
      { name: 'openai-api-key', kind: 'referenced', ref: 'vault://kv/data/agents#openai', updated_at: iso(12 * 86400000) },
    ],
  })],
]

const MIME = { '.html': 'text/html', '.js': 'text/javascript', '.css': 'text/css', '.map': 'application/json', '.svg': 'image/svg+xml' }

createServer(async (req, res) => {
  const url = new URL(req.url, 'http://localhost')

  if (url.pathname.startsWith('/api/')) {
    const auth = req.headers.authorization ?? ''
    if (auth !== `Bearer ${TOKEN}`) {
      res.writeHead(401, { 'content-type': 'application/json' })
      return res.end(JSON.stringify({ error: { code: 'unauthenticated', message: 'a bearer token is required' } }))
    }
    for (const [method, pattern, handle] of routes) {
      const m = url.pathname.match(pattern)
      if (!m || req.method !== method) continue
      const body = handle(m, url, snapshot())
      if (body === 404) {
        res.writeHead(404, { 'content-type': 'application/json' })
        return res.end(JSON.stringify({ error: { code: 'not_found', message: 'no such object' } }))
      }
      if (body?.text !== undefined) {
        res.writeHead(200, { 'content-type': 'text/plain; charset=utf-8' })
        return res.end(body.text)
      }
      res.writeHead(200, { 'content-type': 'application/json' })
      return res.end(JSON.stringify(body))
    }
    res.writeHead(404, { 'content-type': 'application/json' })
    return res.end(JSON.stringify({ error: { code: 'not_found', message: `no route for ${req.method} ${url.pathname}` } }))
  }

  // Everything else is the bundle, with the SPA fallback nginx also does.
  const path = normalize(url.pathname).replace(/^(\.\.[/\\])+/, '')
  const name = path === '/' ? 'index.html' : path
  try {
    const file = await readFile(join(DIST, name))
    // Keyed off the resolved name, not the request path: "/" has no extension,
    // and answering the document as octet-stream makes the browser download
    // the SPA instead of running it.
    res.writeHead(200, { 'content-type': MIME[extname(name)] ?? 'application/octet-stream' })
    res.end(file)
  } catch {
    try {
      res.writeHead(200, { 'content-type': 'text/html' })
      res.end(await readFile(join(DIST, 'index.html')))
    } catch {
      res.writeHead(500)
      res.end('run `npm run build` first')
    }
  }
}).listen(PORT, '127.0.0.1', () => {
  console.log(`mock haliphron on http://127.0.0.1:${PORT} — token: ${TOKEN}`)
})
