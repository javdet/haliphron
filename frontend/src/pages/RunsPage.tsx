import { useMemo, useState } from 'react'
import { useNavigate, useSearchParams } from 'react-router-dom'
import { useCreateRun, useRoles, useRuns, type RunFilter } from '../api/hooks'
import { AGENT_TYPES, RUN_STATUSES, type CreateRunRequest, type Run } from '../api/types'
import { StatusBadge } from '../components/StatusBadge'
import { Card, Dialog, Empty, ErrorBanner, Field, Spinner, Time, elapsed, money } from '../components/ui'

// The filter lives in the URL. An operator who has narrowed a list to the
// thing that is on fire should be able to send that list to somebody else.
function useFilter(): [RunFilter, (next: Partial<RunFilter>) => void] {
  const [params, setParams] = useSearchParams()

  const filter = useMemo<RunFilter>(
    () => ({
      status: params.getAll('status'),
      role: params.get('role') ?? '',
      agent: params.get('agent') ?? '',
      cluster_id: params.get('cluster_id') ?? '',
      q: params.get('q') ?? '',
    }),
    [params],
  )

  const update = (next: Partial<RunFilter>) => {
    const merged = { ...filter, ...next }
    const out = new URLSearchParams()
    merged.status?.forEach((s) => out.append('status', s))
    for (const key of ['role', 'agent', 'cluster_id', 'q'] as const) {
      const value = merged[key]
      if (value) out.set(key, value)
    }
    setParams(out, { replace: true })
  }

  return [filter, update]
}

function Filters({
  filter,
  update,
  roles,
}: {
  filter: RunFilter
  update: (next: Partial<RunFilter>) => void
  roles: string[]
}) {
  const selected = new Set(filter.status ?? [])
  const toggle = (status: string) => {
    const next = new Set(selected)
    next.has(status) ? next.delete(status) : next.add(status)
    update({ status: [...next] })
  }

  return (
    <Card>
      <div className="stack">
        <div className="checks">
          {RUN_STATUSES.map((status) => (
            <label key={status} className={selected.has(status) ? 'check on' : 'check'}>
              <input
                type="checkbox"
                checked={selected.has(status)}
                onChange={() => toggle(status)}
              />
              {status === 'CompletedWithoutResult' ? 'No result' : status}
            </label>
          ))}
        </div>
        <div className="grid-3">
          <Field label="Search">
            <input
              value={filter.q ?? ''}
              placeholder="prompt, repo, summary…"
              onChange={(e) => update({ q: e.target.value })}
            />
          </Field>
          <Field label="Role">
            <select value={filter.role ?? ''} onChange={(e) => update({ role: e.target.value })}>
              <option value="">Any</option>
              {roles.map((role) => (
                <option key={role} value={role}>
                  {role}
                </option>
              ))}
            </select>
          </Field>
          <Field label="Agent">
            <select value={filter.agent ?? ''} onChange={(e) => update({ agent: e.target.value })}>
              <option value="">Any</option>
              {AGENT_TYPES.map((agent) => (
                <option key={agent} value={agent}>
                  {agent}
                </option>
              ))}
            </select>
          </Field>
        </div>
      </div>
    </Card>
  )
}

function NewRunDialog({ onClose, roles }: { onClose: () => void; roles: string[] }) {
  const navigate = useNavigate()
  const create = useCreateRun()
  const [form, setForm] = useState<CreateRunRequest>({ prompt: '', agent: 'claude-code' })

  const set = <K extends keyof CreateRunRequest>(key: K, value: CreateRunRequest[K]) =>
    setForm((f) => ({ ...f, [key]: value }))

  const submit = () => {
    const body: CreateRunRequest = { ...form, prompt: form.prompt.trim() }
    // Empty strings are not "unset" to a Go handler that only omits on empty;
    // strip them so the backend applies its own defaults.
    for (const key of Object.keys(body) as (keyof CreateRunRequest)[]) {
      if (body[key] === '' || body[key] === undefined) delete body[key]
    }
    create.mutate(body, { onSuccess: (run: Run) => navigate(`/runs/${run.run_id}`) })
  }

  return (
    <Dialog
      title="New run"
      onClose={onClose}
      footer={
        <>
          <button className="ghost" onClick={onClose}>
            Cancel
          </button>
          <button className="primary" onClick={submit} disabled={!form.prompt.trim() || create.isPending}>
            {create.isPending ? 'Submitting…' : 'Submit run'}
          </button>
        </>
      }
    >
      <ErrorBanner error={create.error} what="submit the run" />

      <Field label="Prompt" hint="What the agent is asked to do.">
        <textarea rows={6} value={form.prompt} onChange={(e) => set('prompt', e.target.value)} />
      </Field>

      <div className="grid-2">
        <Field label="Agent">
          <select value={form.agent ?? ''} onChange={(e) => set('agent', e.target.value)}>
            {AGENT_TYPES.map((agent) => (
              <option key={agent} value={agent}>
                {agent}
              </option>
            ))}
          </select>
        </Field>
        <Field label="Role" hint="Decides the tools and skills the agent gets.">
          <select value={form.role ?? ''} onChange={(e) => set('role', e.target.value)}>
            <option value="">Default</option>
            {roles.map((role) => (
              <option key={role} value={role}>
                {role}
              </option>
            ))}
          </select>
        </Field>
        <Field label="Model" hint="Empty takes the role's default.">
          <input value={form.model ?? ''} onChange={(e) => set('model', e.target.value)} />
        </Field>
        <Field label="Repository" hint="https:// or git@ — cloned before the agent starts.">
          <input value={form.repo ?? ''} onChange={(e) => set('repo', e.target.value)} />
        </Field>
        <Field label="Base branch">
          <input value={form.base_branch ?? ''} onChange={(e) => set('base_branch', e.target.value)} />
        </Field>
        <Field label="Target branch" hint="Where the work is pushed.">
          <input value={form.target_branch ?? ''} onChange={(e) => set('target_branch', e.target.value)} />
        </Field>
      </div>

      <label className={form.create_pr ? 'check on' : 'check'} style={{ alignSelf: 'flex-start' }}>
        <input
          type="checkbox"
          checked={form.create_pr ?? false}
          onChange={(e) => set('create_pr', e.target.checked)}
        />
        Open a pull request when it finishes
      </label>

      <div className="grid-3">
        <Field label="Timeout (s)">
          <input
            type="number"
            min={0}
            value={form.timeout_seconds ?? ''}
            onChange={(e) => set('timeout_seconds', Number(e.target.value) || undefined)}
          />
        </Field>
        <Field label="Max cost (USD)">
          <input
            value={form.max_cost_usd ?? ''}
            placeholder="5.00"
            onChange={(e) => set('max_cost_usd', e.target.value)}
          />
        </Field>
        <Field label="Max turns">
          <input
            type="number"
            min={0}
            value={form.max_turns ?? ''}
            onChange={(e) => set('max_turns', Number(e.target.value) || undefined)}
          />
        </Field>
      </div>
    </Dialog>
  )
}

export function RunsPage() {
  const navigate = useNavigate()
  const [filter, update] = useFilter()
  const [creating, setCreating] = useState(false)
  const roles = useRoles()
  const runs = useRuns(filter)

  const rows = runs.data?.pages.flatMap((page) => page.runs) ?? []
  const roleNames = roles.data?.map((r) => r.name) ?? []

  return (
    <>
      <header className="topbar">
        <div className="topbar-title">
          <h1>Runs</h1>
          {runs.isFetching && <Spinner />}
        </div>
        <div className="topbar-actions">
          <button className="primary" onClick={() => setCreating(true)}>
            New run
          </button>
        </div>
      </header>

      <div className="content">
        <Filters filter={filter} update={update} roles={roleNames} />
        <ErrorBanner error={runs.error} what="load the runs" />

        <Card tight>
          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th>Status</th>
                  <th>Run</th>
                  <th>Role</th>
                  <th>Agent</th>
                  <th>Repository</th>
                  <th className="num">Cost</th>
                  <th className="num">Elapsed</th>
                  <th>Created</th>
                </tr>
              </thead>
              <tbody>
                {rows.map((run) => (
                  <tr
                    key={run.run_id}
                    className="clickable"
                    onClick={() => navigate(`/runs/${run.run_id}`)}
                  >
                    <td className="nowrap">
                      <StatusBadge status={run.status} />
                    </td>
                    <td>
                      <div className="mono small">{run.run_id}</div>
                      {run.result_summary && <div className="small muted trunc">{run.result_summary}</div>}
                    </td>
                    <td className="nowrap">{run.role || <span className="faint">—</span>}</td>
                    <td className="nowrap small muted">{run.agent}</td>
                    <td className="small muted">
                      <span className="trunc">{run.repo || '—'}</span>
                    </td>
                    <td className="num nowrap">{money(run.cost_usd)}</td>
                    <td className="num nowrap">{elapsed(run.started_at, run.finished_at)}</td>
                    <td className="small muted">
                      <Time at={run.created_at} />
                      <div className="faint trunc trunc-sm">{run.created_by}</div>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>

          {rows.length === 0 && !runs.isLoading && <Empty>No runs match this filter.</Empty>}
          {runs.isLoading && (
            <Empty>
              <Spinner />
            </Empty>
          )}
          {runs.hasNextPage && (
            <div style={{ padding: 12, display: 'flex', justifyContent: 'center' }}>
              <button onClick={() => void runs.fetchNextPage()} disabled={runs.isFetchingNextPage}>
                {runs.isFetchingNextPage ? 'Loading…' : 'Load more'}
              </button>
            </div>
          )}
        </Card>
      </div>

      {creating && <NewRunDialog onClose={() => setCreating(false)} roles={roleNames} />}
    </>
  )
}
