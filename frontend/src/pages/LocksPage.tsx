import { useState } from 'react'
import { Link } from 'react-router-dom'
import { useCancelDeferred, useDeferredRuns, useForceReleaseLock, useLocks } from '../api/hooks'
import type { DeferredRun, DeferredState, Lock } from '../api/types'
import { StatusBadge } from '../components/StatusBadge'
import { Card, Dialog, Empty, ErrorBanner, Spinner, Time } from '../components/ui'

/**
 * Locks and deferred runs: what agents say they are working on, and the work
 * waiting for it to be released.
 *
 * A lock is advisory. Its name and description are free-form text an agent
 * wrote, and the backend never decides whether two locks conflict — the agents
 * holding them do. The page shows each holder's status and when its cluster
 * last reported, so a lock held by a run that has gone quiet is visible as
 * such. Force-release is admin-only, because every agent pod holds runs:write.
 */

function ForceReleaseDialog({ lock, onClose }: { lock: Lock; onClose: () => void }) {
  const release = useForceReleaseLock()
  return (
    <Dialog
      title="Force-release this lock?"
      onClose={onClose}
      footer={
        <>
          <button className="ghost" onClick={onClose}>
            Keep it
          </button>
          <button
            className="danger"
            disabled={release.isPending}
            onClick={() => release.mutate(lock.lock_id, { onSuccess: onClose })}
          >
            Release
          </button>
        </>
      }
    >
      <ErrorBanner error={release.error} what="release the lock" />
      <p>
        Run <Link to={`/runs/${lock.held_by_run}`} className="mono">{lock.held_by_run}</Link> says it is working on{' '}
        <strong>{lock.name}</strong>. Releasing the lock does not stop the run: other agents will simply stop seeing
        it, and runs waiting on it may start.
      </p>
    </Dialog>
  )
}

function LocksCard() {
  const locks = useLocks()
  const [releasing, setReleasing] = useState<Lock | null>(null)
  const rows = locks.data ?? []
  return (
    <Card title="Locks" actions={locks.isFetching ? <Spinner /> : undefined} tight>
      <ErrorBanner error={locks.error} what="load the locks" />
      <div className="table-wrap">
        <table>
          <thead>
            <tr>
              <th>Lock</th>
              <th>Held by</th>
              <th>Holder status</th>
              <th>Last activity</th>
              <th>Acquired</th>
              <th>Expires</th>
              <th />
            </tr>
          </thead>
          <tbody>
            {rows.map((lock) => (
              <tr key={lock.lock_id}>
                <td>
                  {lock.name}
                  {lock.description && <div className="small muted">{lock.description}</div>}
                  <div className="small faint mono">{lock.lock_id}</div>
                </td>
                <td className="nowrap">
                  <Link to={`/runs/${lock.held_by_run}`} className="mono small">
                    {lock.held_by_run}
                  </Link>
                  {lock.holder_role && <div className="small faint">{lock.holder_role}</div>}
                </td>
                <td>
                  <StatusBadge status={lock.holder_status} />
                </td>
                <td className="small nowrap">
                  <Time at={lock.holder_last_activity_at} />
                </td>
                <td className="small nowrap">
                  <Time at={lock.acquired_at} />
                </td>
                <td className="small nowrap">
                  <Time at={lock.expires_at} />
                </td>
                <td className="nowrap">
                  <button className="sm danger" onClick={() => setReleasing(lock)}>
                    Force-release
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      {rows.length === 0 && !locks.isLoading && (
        <Empty>No live locks. Agents set them through the set_lock tool before changing shared infrastructure.</Empty>
      )}
      {releasing && <ForceReleaseDialog lock={releasing} onClose={() => setReleasing(null)} />}
    </Card>
  )
}

const STATE_FILTERS: { label: string; value?: DeferredState }[] = [
  { label: 'Waiting', value: 'waiting' },
  { label: 'All' },
]

export function DeferredRunsTable({ rows, compact }: { rows: DeferredRun[]; compact?: boolean }) {
  const cancel = useCancelDeferred()
  return (
    <>
      <ErrorBanner error={cancel.error} what="cancel the deferred run" />
      <div className="table-wrap">
        <table>
          <thead>
            <tr>
              <th>Task</th>
              {!compact && <th>Deferred by</th>}
              <th>Waiting for</th>
              <th>State</th>
              <th>Gives up</th>
              <th />
            </tr>
          </thead>
          <tbody>
            {rows.map((d) => (
              <tr key={d.deferred_id}>
                <td>
                  <div className="trunc">{d.run.prompt}</div>
                  <div className="small faint mono">{d.deferred_id}</div>
                </td>
                {!compact && (
                  <td className="nowrap">
                    <Link to={`/runs/${d.parent_run_id}`} className="mono small">
                      {d.parent_run_id}
                    </Link>
                  </td>
                )}
                <td className="small">
                  {d.wait_for_locks.map((l) => (
                    <div key={l.lock_id} className="nowrap">
                      <span className="mono">{l.lock_id}</span>{' '}
                      <span className={l.live ? '' : 'faint'}>{l.live ? 'held' : 'released'}</span>
                    </div>
                  ))}
                  {d.not_before && (
                    <div className="nowrap">
                      not before <Time at={d.not_before} />
                    </div>
                  )}
                </td>
                <td className="small">
                  {d.state}
                  {d.run_id && (
                    <div>
                      →{' '}
                      <Link to={`/runs/${d.run_id}`} className="mono">
                        {d.run_id}
                      </Link>
                    </div>
                  )}
                  {d.reason && <div className="faint">{d.reason}</div>}
                </td>
                <td className="small nowrap">
                  <Time at={d.give_up_at} />
                </td>
                <td className="nowrap">
                  {d.state === 'waiting' && (
                    <button className="sm danger" disabled={cancel.isPending} onClick={() => cancel.mutate(d.deferred_id)}>
                      Cancel
                    </button>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </>
  )
}

function DeferredCard() {
  const [state, setState] = useState<DeferredState | undefined>('waiting')
  const deferred = useDeferredRuns({ state })
  const rows = deferred.data ?? []
  return (
    <Card
      title="Deferred runs"
      tight
      actions={
        <>
          {deferred.isFetching && <Spinner />}
          {STATE_FILTERS.map((f) => (
            <button
              key={f.label}
              className={state === f.value ? 'sm' : 'sm ghost'}
              onClick={() => setState(f.value)}
            >
              {f.label}
            </button>
          ))}
        </>
      }
    >
      <ErrorBanner error={deferred.error} what="load the deferred runs" />
      <DeferredRunsTable rows={rows} />
      {rows.length === 0 && !deferred.isLoading && (
        <Empty>
          {state === 'waiting'
            ? 'Nothing is waiting. An agent defers the rest of its task with schedule_agent when what it needs is locked.'
            : 'No deferred runs.'}
        </Empty>
      )}
    </Card>
  )
}

export function LocksPage() {
  return (
    <>
      <header className="topbar">
        <div className="topbar-title">
          <h1>Locks</h1>
        </div>
      </header>
      <div className="content">
        <LocksCard />
        <DeferredCard />
      </div>
    </>
  )
}
