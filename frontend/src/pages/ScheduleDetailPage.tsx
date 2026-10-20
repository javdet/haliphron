import { useState } from 'react'
import { useNavigate, useParams } from 'react-router-dom'
import { useRoles, useRuns, useSchedule, useUpdateSchedule } from '../api/hooks'
import { StatusBadge } from '../components/StatusBadge'
import { Card, Empty, ErrorBanner, Spinner, Time, elapsed, money } from '../components/ui'
import { DeleteScheduleDialog, LastFire, NextFire, ScheduleDialog, useRunNow } from './SchedulesPage'

/** One schedule: what it is, what it will do, and every run it started. */
export function ScheduleDetailPage() {
  const { id = '' } = useParams()
  const navigate = useNavigate()
  const schedule = useSchedule(id)
  const roles = useRoles()
  const runs = useRuns({ schedule_id: id })
  const update = useUpdateSchedule()
  const { trigger, runNow } = useRunNow()
  const [editing, setEditing] = useState(false)
  const [deleting, setDeleting] = useState(false)

  const s = schedule.data
  const rows = runs.data?.pages.flatMap((page) => page.runs) ?? []

  return (
    <>
      <header className="topbar">
        <div className="topbar-title">
          <h1>{s?.name ?? 'Schedule'}</h1>
          {(schedule.isFetching || runs.isFetching) && <Spinner />}
        </div>
        {s && (
          <div className="topbar-actions">
            <button className="primary" disabled={trigger.isPending} onClick={() => runNow(s.id)}>
              {trigger.isPending ? 'Starting…' : 'Run now'}
            </button>
            <button
              disabled={update.isPending}
              onClick={() => update.mutate({ id: s.id, patch: { enabled: !s.enabled } })}
            >
              {s.enabled ? 'Disable' : 'Enable'}
            </button>
            <button className="ghost" onClick={() => setEditing(true)}>
              Edit
            </button>
            <button className="danger" onClick={() => setDeleting(true)}>
              Delete
            </button>
          </div>
        )}
      </header>

      <div className="content">
        <ErrorBanner error={schedule.error} what="load the schedule" />
        <ErrorBanner error={trigger.error} what="start the run" />
        <ErrorBanner error={update.error} what="change the schedule" />

        {s && (
          <div className="grid-2">
            <Card title="Schedule">
              <dl className="kv">
                {s.description && (
                  <>
                    <dt>About</dt>
                    <dd>{s.description}</dd>
                  </>
                )}
                <dt>Cron</dt>
                <dd>
                  <span className="mono">{s.cron}</span> <span className="faint">{s.timezone}</span>
                </dd>
                <dt>Next</dt>
                <dd>
                  <NextFire schedule={s} />
                </dd>
                <dt>Last fire</dt>
                <dd>
                  <LastFire schedule={s} />
                </dd>
                <dt>If live</dt>
                <dd>{s.concurrency_policy}</dd>
                <dt>Deadline</dt>
                <dd>
                  {s.starting_deadline_seconds ? (
                    `${s.starting_deadline_seconds}s`
                  ) : (
                    <span className="faint">none — a late fire always runs once</span>
                  )}
                </dd>
                <dt>Created</dt>
                <dd>
                  <Time at={s.created_at} /> <span className="faint">by {s.created_by}</span>
                </dd>
                <dt>Updated</dt>
                <dd>
                  <Time at={s.updated_at} /> <span className="faint">by {s.updated_by}</span>
                </dd>
              </dl>
            </Card>

            <Card title="Each run">
              <dl className="kv">
                <dt>Agent</dt>
                <dd>
                  {s.run.agent || <span className="faint">default</span>}{' '}
                  {s.run.model && <span className="faint">{s.run.model}</span>}
                </dd>
                <dt>Role</dt>
                <dd>{s.run.role || <span className="faint">default</span>}</dd>
                <dt>Repository</dt>
                <dd className="small">{s.run.repo || <span className="faint">none</span>}</dd>
                {s.run.base_branch && (
                  <>
                    <dt>Base</dt>
                    <dd className="mono small">{s.run.base_branch}</dd>
                  </>
                )}
                {s.run.target_branch && (
                  <>
                    <dt>Target</dt>
                    <dd className="mono small">{s.run.target_branch}</dd>
                  </>
                )}
                <dt>Pull request</dt>
                <dd>{s.run.create_pr ? 'opened when it finishes' : <span className="faint">no</span>}</dd>
                {s.run.max_cost_usd && (
                  <>
                    <dt>Max cost</dt>
                    <dd>{money(s.run.max_cost_usd)}</dd>
                  </>
                )}
                <dt>Prompt</dt>
                <dd>
                  <pre className="small" style={{ whiteSpace: 'pre-wrap', margin: 0 }}>
                    {s.run.prompt}
                  </pre>
                </dd>
              </dl>
            </Card>
          </div>
        )}

        <Card title="Runs" tight>
          <ErrorBanner error={runs.error} what="load the runs" />
          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th>Status</th>
                  <th>Run</th>
                  <th>Started by</th>
                  <th className="num">Cost</th>
                  <th className="num">Elapsed</th>
                  <th>Created</th>
                </tr>
              </thead>
              <tbody>
                {rows.map((run) => (
                  <tr key={run.run_id} className="clickable" onClick={() => navigate(`/runs/${run.run_id}`)}>
                    <td className="nowrap">
                      <StatusBadge status={run.status} />
                    </td>
                    <td>
                      <div className="mono small">{run.run_id}</div>
                      {run.result_summary && <div className="small muted trunc">{run.result_summary}</div>}
                    </td>
                    <td className="small muted">
                      {run.created_via === 'schedule' ? 'the schedule' : `${run.created_by} (run now)`}
                    </td>
                    <td className="num nowrap">{money(run.cost_usd)}</td>
                    <td className="num nowrap">{elapsed(run.started_at, run.finished_at)}</td>
                    <td className="small muted">
                      <Time at={run.created_at} />
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          {rows.length === 0 && !runs.isLoading && <Empty>This schedule has not started a run yet.</Empty>}
          {runs.hasNextPage && (
            <div style={{ padding: 12, display: 'flex', justifyContent: 'center' }}>
              <button onClick={() => void runs.fetchNextPage()} disabled={runs.isFetchingNextPage}>
                {runs.isFetchingNextPage ? 'Loading…' : 'Load more'}
              </button>
            </div>
          )}
        </Card>
      </div>

      {editing && s && (
        <ScheduleDialog
          schedule={s}
          roles={roles.data?.map((r) => r.name) ?? []}
          onClose={() => setEditing(false)}
        />
      )}
      {deleting && s && (
        <DeleteScheduleDialog schedule={s} onClose={() => setDeleting(false)} onDeleted={() => navigate('/schedules')} />
      )}
    </>
  )
}
