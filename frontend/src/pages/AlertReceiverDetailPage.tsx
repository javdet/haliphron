import { useState } from 'react'
import { Link, useNavigate, useParams } from 'react-router-dom'
import {
  useAlertDeliveries,
  useAlertReceiver,
  useAlertSuppressions,
  useClearAlertSuppressions,
  useRoles,
  useRotateAlertReceiverToken,
  useRuns,
  useUpdateAlertReceiver,
} from '../api/hooks'
import type { AlertReceiverWithToken } from '../api/types'
import { StatusBadge } from '../components/StatusBadge'
import { Card, Dialog, Empty, ErrorBanner, Spinner, Time, elapsed, money } from '../components/ui'
import {
  AlertReceiverDialog,
  CredentialDialog,
  DeleteAlertReceiverDialog,
  LastDelivery,
  OutcomeBadge,
  alertmanagerConfig,
  throttleLabel,
} from './AlertReceiversPage'

/**
 * One alert receiver: what a notification starts, what each delivery did and
 * why, which alerts are suppressed now, and the runs it started.
 */
export function AlertReceiverDetailPage() {
  const { id = '' } = useParams()
  const navigate = useNavigate()
  const receiver = useAlertReceiver(id)
  const roles = useRoles()
  const deliveries = useAlertDeliveries(id)
  const suppressions = useAlertSuppressions(id)
  const runs = useRuns({ alert_receiver_id: id })
  const update = useUpdateAlertReceiver()
  const rotate = useRotateAlertReceiverToken()
  const clear = useClearAlertSuppressions(id)
  const [editing, setEditing] = useState(false)
  const [deleting, setDeleting] = useState(false)
  const [rotating, setRotating] = useState(false)
  const [minted, setMinted] = useState<AlertReceiverWithToken | null>(null)

  const r = receiver.data
  const runRows = runs.data?.pages.flatMap((page) => page.runs) ?? []
  const deliveryRows = deliveries.data ?? []
  const suppressionRows = suppressions.data ?? []

  return (
    <>
      <header className="topbar">
        <div className="topbar-title">
          <h1>{r?.name ?? 'Alert receiver'}</h1>
          {(receiver.isFetching || runs.isFetching) && <Spinner />}
        </div>
        {r && (
          <div className="topbar-actions">
            <button disabled={update.isPending} onClick={() => update.mutate({ id: r.id, patch: { enabled: !r.enabled } })}>
              {r.enabled ? 'Disable' : 'Enable'}
            </button>
            <button onClick={() => setRotating(true)}>Rotate credential</button>
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
        <ErrorBanner error={receiver.error} what="load the alert receiver" />
        <ErrorBanner error={update.error} what="change the alert receiver" />
        <ErrorBanner error={clear.error} what="clear the suppression" />

        {r && (
          <div className="grid-2">
            <Card title="Receiver">
              <dl className="kv">
                {r.description && (
                  <>
                    <dt>About</dt>
                    <dd>{r.description}</dd>
                  </>
                )}
                <dt>Webhook</dt>
                <dd className="mono small">{`${window.location.origin}${r.webhook_path}`}</dd>
                <dt>Throttle</dt>
                <dd>
                  {throttleLabel(r.throttle_seconds)}{' '}
                  <span className="faint">per alert, from the run it started</span>
                </dd>
                <dt>Enabled</dt>
                <dd>{r.enabled ? 'yes' : <span className="faint">no — deliveries are answered 404</span>}</dd>
                <dt>Last</dt>
                <dd>
                  <LastDelivery receiver={r} />
                </dd>
                <dt>Created</dt>
                <dd>
                  <Time at={r.created_at} /> <span className="faint">by {r.created_by}</span>
                </dd>
                <dt>Updated</dt>
                <dd>
                  <Time at={r.updated_at} /> <span className="faint">by {r.updated_by}</span>
                </dd>
              </dl>
              <details>
                <summary className="small">Alertmanager configuration</summary>
                <pre className="small mono" style={{ whiteSpace: 'pre', overflowX: 'auto' }}>
                  {alertmanagerConfig(r)}
                </pre>
              </details>
            </Card>

            <Card title="Each run">
              <dl className="kv">
                <dt>Role</dt>
                <dd>{r.run.role}</dd>
                <dt>Agent</dt>
                <dd>
                  {r.run.agent || <span className="faint">default</span>}{' '}
                  {r.run.model && <span className="faint">{r.run.model}</span>}
                </dd>
                <dt>Repository</dt>
                <dd className="small">{r.run.repo || <span className="faint">none</span>}</dd>
                <dt>Pull request</dt>
                <dd>{r.run.create_pr ? 'opened when it finishes' : <span className="faint">no</span>}</dd>
                {r.run.max_cost_usd && (
                  <>
                    <dt>Max cost</dt>
                    <dd>{money(r.run.max_cost_usd)}</dd>
                  </>
                )}
                <dt>Instruction</dt>
                <dd>
                  <pre className="small" style={{ whiteSpace: 'pre-wrap', margin: 0 }}>
                    {r.run.prompt}
                  </pre>
                  <div className="small faint">The firing alerts of each notification are appended.</div>
                </dd>
              </dl>
            </Card>
          </div>
        )}

        <Card
          title="Suppressed now"
          tight
          actions={
            suppressionRows.length > 0 && (
              <button className="sm" disabled={clear.isPending} onClick={() => clear.mutate(undefined)}>
                Clear all
              </button>
            )
          }
        >
          <ErrorBanner error={suppressions.error} what="load the suppressions" />
          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th>Alert</th>
                  <th>Until</th>
                  <th>Handled by</th>
                  <th className="num">Runs</th>
                  <th className="num">Suppressed</th>
                  <th />
                </tr>
              </thead>
              <tbody>
                {suppressionRows.map((s) => (
                  <tr key={s.fingerprint}>
                    <td>
                      {s.alertname || <span className="faint">unnamed</span>}
                      <div className="mono small faint">{s.fingerprint}</div>
                    </td>
                    <td className="small nowrap">
                      <Time at={s.suppressed_until} />
                    </td>
                    <td className="small">
                      {s.run_id ? (
                        <Link className="mono" to={`/runs/${s.run_id}`}>
                          {s.run_id}
                        </Link>
                      ) : (
                        <span className="faint">a run since removed</span>
                      )}
                    </td>
                    <td className="num">{s.runs}</td>
                    <td className="num">{s.suppressed}</td>
                    <td className="nowrap">
                      <button className="sm ghost" disabled={clear.isPending} onClick={() => clear.mutate(s.fingerprint)}>
                        Clear
                      </button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          {suppressionRows.length === 0 && !suppressions.isLoading && (
            <Empty>Nothing is suppressed: the next notification of any alert starts a run.</Empty>
          )}
        </Card>

        <Card title="Notifications" tight>
          <ErrorBanner error={deliveries.error} what="load the notifications" />
          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th>Received</th>
                  <th>Outcome</th>
                  <th className="num">Firing</th>
                  <th className="num">Resolved</th>
                  <th className="num">Throttled</th>
                  <th>Run</th>
                </tr>
              </thead>
              <tbody>
                {deliveryRows.map((d) => (
                  <tr key={d.id}>
                    <td className="small nowrap">
                      <Time at={d.received_at} />
                      <div className="mono small faint trunc trunc-sm" title={d.group_key}>
                        {d.group_key}
                      </div>
                    </td>
                    <td>
                      <OutcomeBadge outcome={d.outcome} title={d.error} />
                      {d.error && <div className="small muted trunc">{d.error}</div>}
                    </td>
                    <td className="num">{d.firing}</td>
                    <td className="num">{d.resolved}</td>
                    <td className="num" title={d.throttled.join('\n')}>
                      {d.throttled.length}
                    </td>
                    <td className="small">
                      {d.run_id && (
                        <Link className="mono" to={`/runs/${d.run_id}`}>
                          {d.run_id}
                        </Link>
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          {deliveryRows.length === 0 && !deliveries.isLoading && (
            <Empty>No notification has arrived in the last two weeks.</Empty>
          )}
        </Card>

        <Card title="Runs" tight>
          <ErrorBanner error={runs.error} what="load the runs" />
          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th>Status</th>
                  <th>Run</th>
                  <th className="num">Cost</th>
                  <th className="num">Elapsed</th>
                  <th>Created</th>
                </tr>
              </thead>
              <tbody>
                {runRows.map((run) => (
                  <tr key={run.run_id} className="clickable" onClick={() => navigate(`/runs/${run.run_id}`)}>
                    <td className="nowrap">
                      <StatusBadge status={run.status} />
                    </td>
                    <td>
                      <div className="mono small">{run.run_id}</div>
                      {run.result_summary && <div className="small muted trunc">{run.result_summary}</div>}
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
          {runRows.length === 0 && !runs.isLoading && <Empty>This receiver has not started a run yet.</Empty>}
          {runs.hasNextPage && (
            <div style={{ padding: 12, display: 'flex', justifyContent: 'center' }}>
              <button onClick={() => void runs.fetchNextPage()} disabled={runs.isFetchingNextPage}>
                {runs.isFetchingNextPage ? 'Loading…' : 'Load more'}
              </button>
            </div>
          )}
        </Card>
      </div>

      {editing && r && (
        <AlertReceiverDialog receiver={r} roles={roles.data?.map((x) => x.name) ?? []} onClose={() => setEditing(false)} />
      )}
      {deleting && r && (
        <DeleteAlertReceiverDialog
          receiver={r}
          onClose={() => setDeleting(false)}
          onDeleted={() => navigate('/alert-receivers')}
        />
      )}
      {rotating && r && (
        <Dialog
          title={`Rotate the credential of ${r.name}`}
          onClose={() => setRotating(false)}
          footer={
            <>
              <button className="ghost" onClick={() => setRotating(false)}>
                Keep it
              </button>
              <button
                className="danger"
                disabled={rotate.isPending}
                onClick={() =>
                  rotate.mutate(r.id, {
                    onSuccess: (saved) => {
                      setRotating(false)
                      setMinted(saved)
                    },
                  })
                }
              >
                {rotate.isPending ? 'Rotating…' : 'Rotate credential'}
              </button>
            </>
          }
        >
          <ErrorBanner error={rotate.error} what="rotate the credential" />
          <p className="small muted" style={{ margin: 0 }}>
            The current credential stops working at once, and Alertmanager's deliveries are refused until it is
            given the new one.
          </p>
        </Dialog>
      )}
      {minted && <CredentialDialog receiver={minted} onClose={() => setMinted(null)} />}
    </>
  )
}
