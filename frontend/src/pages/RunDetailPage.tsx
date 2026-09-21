import { useEffect, useRef, useState } from 'react'
import { Link, useParams } from 'react-router-dom'
import {
  isLive,
  useAttempts,
  useCancelRun,
  useLogs,
  useResult,
  useRetryRun,
  useRun,
} from '../api/hooks'
import { FailureBadge, StatusBadge } from '../components/StatusBadge'
import {
  Card,
  CopyButton,
  Dialog,
  Empty,
  ErrorBanner,
  Field,
  Spinner,
  Time,
  duration,
  elapsed,
  money,
  tokens,
} from '../components/ui'
import type { Attempt, Run } from '../api/types'

type Tab = 'result' | 'logs' | 'attempts'

function Summary({ run }: { run: Run }) {
  return (
    <div className="grid-2">
      <Card title="Run">
        <dl className="kv">
          <dt>Status</dt>
          <dd className="inline">
            <StatusBadge status={run.status} />
            <FailureBadge failureClass={run.failure_class} />
          </dd>
          <dt>Phase</dt>
          <dd>{run.observed_phase || <span className="faint">—</span>}</dd>
          {run.status_reason && (
            <>
              <dt>Reason</dt>
              <dd>{run.status_reason}</dd>
            </>
          )}
          {run.status_message && (
            <>
              <dt>Message</dt>
              <dd className="small muted">{run.status_message}</dd>
            </>
          )}
          {run.exit_code !== undefined && (
            <>
              <dt>Exit code</dt>
              <dd className="mono">{run.exit_code}</dd>
            </>
          )}
          <dt>Attempt</dt>
          <dd>
            {run.attempt} <span className="faint">epoch {run.epoch}</span>
          </dd>
          <dt>Agent</dt>
          <dd>
            {run.agent} <span className="faint">{run.model}</span>
          </dd>
          <dt>Role</dt>
          <dd>{run.role || <span className="faint">default</span>}</dd>
          <dt>Cluster</dt>
          <dd className="mono small">{run.cluster_id || <span className="faint">unassigned</span>}</dd>
        </dl>
      </Card>

      <Card title="Accounting">
        <dl className="kv">
          <dt>Cost</dt>
          <dd>{money(run.cost_usd)}</dd>
          <dt>Tokens</dt>
          <dd>
            {tokens(run.input_tokens)} in · {tokens(run.output_tokens)} out
          </dd>
          <dt>Turns</dt>
          <dd>{run.num_turns}</dd>
          <dt>Created</dt>
          <dd>
            <Time at={run.created_at} /> <span className="faint">by {run.created_by} via {run.created_via}</span>
          </dd>
          <dt>Started</dt>
          <dd>
            <Time at={run.started_at} />
          </dd>
          <dt>Finished</dt>
          <dd>
            <Time at={run.finished_at} />
          </dd>
          <dt>Elapsed</dt>
          <dd>{elapsed(run.started_at, run.finished_at)}</dd>
          {run.parent_run_id && (
            <>
              <dt>Parent</dt>
              <dd>
                <Link className="mono small" to={`/runs/${run.parent_run_id}`}>
                  {run.parent_run_id}
                </Link>{' '}
                <span className="faint">depth {run.depth}</span>
              </dd>
            </>
          )}
        </dl>
      </Card>

      {(run.repo || run.pr_url || run.commit_sha) && (
        <Card title="Source">
          <dl className="kv">
            {run.repo && (
              <>
                <dt>Repository</dt>
                <dd className="small">{run.repo}</dd>
              </>
            )}
            {run.base_branch && (
              <>
                <dt>Base</dt>
                <dd className="mono small">{run.base_branch}</dd>
              </>
            )}
            {run.target_branch && (
              <>
                <dt>Target</dt>
                <dd className="mono small">{run.target_branch}</dd>
              </>
            )}
            {run.commit_sha && (
              <>
                <dt>Commit</dt>
                <dd className="mono small">{run.commit_sha}</dd>
              </>
            )}
            {run.pr_url && (
              <>
                <dt>Pull request</dt>
                <dd>
                  <a href={run.pr_url} target="_blank" rel="noreferrer noopener">
                    {run.pr_number ? `#${run.pr_number}` : run.pr_url}
                  </a>
                </dd>
              </>
            )}
          </dl>
        </Card>
      )}
    </div>
  )
}

function AttemptsTable({ attempts }: { attempts: Attempt[] }) {
  if (attempts.length === 0) return <Empty>No attempt has been recorded yet.</Empty>
  return (
    <div className="table-wrap">
      <table>
        <thead>
          <tr>
            <th className="num">#</th>
            <th>Phase</th>
            <th>Reason</th>
            <th>Pod</th>
            <th className="num">Cost</th>
            <th className="num">Declared</th>
            <th className="num">Observed</th>
            <th>Started</th>
          </tr>
        </thead>
        <tbody>
          {attempts.map((a) => (
            <tr key={`${a.attempt}-${a.epoch}`}>
              <td className="num">{a.attempt}</td>
              <td className="nowrap">
                {a.phase} <FailureBadge failureClass={a.failure_class} />
              </td>
              <td className="small muted">
                {a.reason}
                {a.message && <div className="faint trunc">{a.message}</div>}
              </td>
              <td className="mono small muted">
                <span className="trunc">{a.pod_name || '—'}</span>
                {a.node_name && <div className="faint trunc">{a.node_name}</div>}
              </td>
              <td className="num nowrap">{money(a.cost_usd)}</td>
              {/* Declared next to observed, because the check the contract
                  asks for is the comparison between them. */}
              <td className="num nowrap">{duration(a.declared_duration_ms)}</td>
              <td className="num nowrap">{duration(a.observed_duration_ms)}</td>
              <td className="small muted">
                <Time at={a.started_at} />
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

function LogsPane({ id, live }: { id: string; live: boolean }) {
  const logs = useLogs(id, live, true)
  const [follow, setFollow] = useState(true)
  const paneRef = useRef<HTMLPreElement>(null)

  useEffect(() => {
    if (follow && paneRef.current) paneRef.current.scrollTop = paneRef.current.scrollHeight
  }, [logs.data?.text, follow])

  if (logs.isLoading) {
    return (
      <Empty>
        <Spinner />
      </Empty>
    )
  }
  if (logs.error) return <div style={{ padding: 14 }}><ErrorBanner error={logs.error} what="read the log" /></div>
  if (!logs.data?.text) return <Empty>No log chunks have been collected for this run.</Empty>

  return (
    <>
      <div className="spread" style={{ padding: '10px 14px' }}>
        <span className="small faint">
          {logs.data.chunks.length} chunk{logs.data.chunks.length === 1 ? '' : 's'}
          {live && ' · refreshing'}
        </span>
        <div className="inline">
          <label className={follow ? 'check on' : 'check'}>
            <input type="checkbox" checked={follow} onChange={(e) => setFollow(e.target.checked)} />
            Follow
          </label>
          <CopyButton value={logs.data.text} label="Copy log" />
        </div>
      </div>
      <pre className="pane" ref={paneRef}>
        {logs.data.text}
      </pre>
    </>
  )
}

function ResultPane({ id, enabled }: { id: string; enabled: boolean }) {
  const [key, setKey] = useState<string | undefined>(undefined)
  const result = useResult(id, key, enabled)

  if (!enabled) return <Empty>The result appears once the run has finished.</Empty>

  return (
    <>
      <div className="spread" style={{ padding: '10px 14px' }}>
        <div className="inline">
          {/* The only keys the backend will serve. Anything else is refused
              as "not a result of a run", so they are offered rather than typed. */}
          {[
            { value: undefined, label: 'result.md' },
            { value: 'output.json', label: 'output.json' },
            { value: 'completion.json', label: 'completion.json' },
          ].map((choice) => (
            <button
              key={choice.label}
              className={key === choice.value ? 'sm primary' : 'sm ghost'}
              onClick={() => setKey(choice.value)}
            >
              {choice.label}
            </button>
          ))}
        </div>
        {result.data && <CopyButton value={result.data} />}
      </div>
      {result.isLoading && (
        <Empty>
          <Spinner />
        </Empty>
      )}
      {result.error && (
        <div style={{ padding: 14 }}>
          <ErrorBanner error={result.error} what="read the result" />
        </div>
      )}
      {/* Plain text, never HTML: a result is whatever an agent wrote, and
          rendering it as markup would run it. */}
      {result.data !== undefined && <pre className="pane">{result.data}</pre>}
    </>
  )
}

function CancelDialog({ id, onClose }: { id: string; onClose: () => void }) {
  const cancel = useCancelRun(id)
  const [reason, setReason] = useState('')
  return (
    <Dialog
      title="Cancel this run"
      onClose={onClose}
      footer={
        <>
          <button className="ghost" onClick={onClose}>
            Keep running
          </button>
          <button
            className="danger"
            disabled={cancel.isPending}
            onClick={() => cancel.mutate(reason, { onSuccess: onClose })}
          >
            {cancel.isPending ? 'Cancelling…' : 'Cancel run'}
          </button>
        </>
      }
    >
      <ErrorBanner error={cancel.error} what="cancel the run" />
      <p className="small muted" style={{ margin: 0 }}>
        The control plane has no path into a cluster: the instruction is recorded here and
        delivered on the cluster's next heartbeat.
      </p>
      <Field label="Reason" hint="Recorded in the audit log.">
        <input value={reason} onChange={(e) => setReason(e.target.value)} />
      </Field>
    </Dialog>
  )
}

export function RunDetailPage() {
  const { id = '' } = useParams()
  const run = useRun(id)
  const live = isLive(run.data)
  const attempts = useAttempts(id, live)
  const retry = useRetryRun(id)
  const [tab, setTab] = useState<Tab>('result')
  const [cancelling, setCancelling] = useState(false)

  if (run.isLoading) {
    return (
      <div className="content">
        <Empty>
          <Spinner />
        </Empty>
      </div>
    )
  }
  if (run.error || !run.data) {
    return (
      <div className="content">
        <ErrorBanner error={run.error} what="load the run" />
      </div>
    )
  }

  const data = run.data
  const finished = !!data.finished_at || !live

  return (
    <>
      <header className="topbar">
        <div className="topbar-title">
          <Link to="/runs" className="muted small">
            Runs
          </Link>
          <h1 className="mono" style={{ fontSize: 15 }}>
            {data.run_id}
          </h1>
          <CopyButton value={data.run_id} label="Copy id" />
        </div>
        <div className="topbar-actions">
          {live && <Spinner />}
          {live && (
            <button className="danger" onClick={() => setCancelling(true)}>
              Cancel
            </button>
          )}
          {!live && (
            <button onClick={() => retry.mutate()} disabled={retry.isPending}>
              {retry.isPending ? 'Retrying…' : 'Retry'}
            </button>
          )}
        </div>
      </header>

      <div className="content">
        <ErrorBanner error={retry.error} what="retry the run" />
        <Summary run={data} />

        <Card tight>
          <div className="tabs">
            <button className={tab === 'result' ? 'active' : ''} onClick={() => setTab('result')}>
              Result
            </button>
            <button className={tab === 'logs' ? 'active' : ''} onClick={() => setTab('logs')}>
              Logs
            </button>
            <button className={tab === 'attempts' ? 'active' : ''} onClick={() => setTab('attempts')}>
              Attempts {attempts.data ? `(${attempts.data.length})` : ''}
            </button>
          </div>
          {tab === 'result' && <ResultPane id={id} enabled={finished} />}
          {tab === 'logs' && <LogsPane id={id} live={live} />}
          {tab === 'attempts' && <AttemptsTable attempts={attempts.data ?? []} />}
        </Card>
      </div>

      {cancelling && <CancelDialog id={id} onClose={() => setCancelling(false)} />}
    </>
  )
}
