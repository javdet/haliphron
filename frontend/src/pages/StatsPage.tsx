import { useSearchParams, Link } from 'react-router-dom'
import { useClusters, useRunStats } from '../api/hooks'
import type { Cluster, ClusterRunStats, Quantiles, RunStatus } from '../api/types'
import { ClusterBadge, StatusBadge } from '../components/StatusBadge'
import { Card, Empty, ErrorBanner, Spinner, Time, duration } from '../components/ui'
import { isStale } from './ClustersPage'

// Run statistics: counts per cluster and status over a window, and how long
// runs take and wait. The same numbers the backend exports to Prometheus, read
// from the same use case.

const RANGES = [
  { key: '24h', label: '24 hours', ms: 24 * 3600_000 },
  { key: '7d', label: '7 days', ms: 7 * 24 * 3600_000 },
  { key: '30d', label: '30 days', ms: 30 * 24 * 3600_000 },
  { key: 'all', label: 'All time', ms: 0 },
] as const

type RangeKey = (typeof RANGES)[number]['key']

// Column order: the run's life from left to right, then the endings.
const STATUS_ORDER: RunStatus[] = [
  'Queued',
  'Leased',
  'Dispatched',
  'Starting',
  'Running',
  'Unknown',
  'Succeeded',
  'Failed',
  'TimedOut',
  'Cancelled',
  'CompletedWithoutResult',
]

// The window lives in the URL, like the run list's filter, so a view can be
// sent to somebody.
function useRange(): [RangeKey, (next: RangeKey) => void] {
  const [params, setParams] = useSearchParams()
  const raw = params.get('range')
  const range = (RANGES.find((r) => r.key === raw)?.key ?? '7d') as RangeKey
  const set = (next: RangeKey) => setParams({ range: next }, { replace: true })
  return [range, set]
}

// Rounded to the minute, so the query key is stable between renders and moves
// at most once a minute.
function sinceFor(range: RangeKey): string | null {
  const ms = RANGES.find((r) => r.key === range)?.ms ?? 0
  if (!ms) return null
  const minute = 60_000
  return new Date(Math.floor((Date.now() - ms) / minute) * minute).toISOString()
}

function seconds(q: number | null): string {
  return q === null ? '—' : duration(q * 1000)
}

function QuantileCell({ q }: { q: Quantiles }) {
  if (q.p50 === null) return <span className="faint">—</span>
  return (
    <span className="nowrap" title="median / 95th percentile">
      {seconds(q.p50)} <span className="faint">/ {seconds(q.p95)}</span>
    </span>
  )
}

function clusterLabel(row: ClusterRunStats) {
  if (!row.cluster_id) return <span className="muted">Unplaced</span>
  return <span>{row.cluster_name}</span>
}

// A count links to the runs behind it, except where the run list cannot
// filter: CompletedWithoutResult is not a stored status, so no filter can
// select it.
function CountCell({ row, status }: { row: ClusterRunStats; status: RunStatus }) {
  const n = row.counts[status] ?? 0
  if (n === 0) return <span className="faint">0</span>
  if (status === 'CompletedWithoutResult' || !row.cluster_id) return <>{n}</>
  const to = `/runs?status=${encodeURIComponent(status)}&cluster_id=${encodeURIComponent(row.cluster_id)}`
  return (
    <Link to={to} title="Open these runs (the run list is not limited to this window)">
      {n}
    </Link>
  )
}

export function StatsPage() {
  const [range, setRange] = useRange()
  const since = sinceFor(range)
  const stats = useRunStats(since)
  const clusters = useClusters()

  const rows = stats.data?.clusters ?? []
  const shown = STATUS_ORDER.filter((s) => rows.some((r) => (r.counts[s] ?? 0) > 0))
  const totals = rows.reduce(
    (acc, r) => {
      acc.total += r.total
      for (const s of shown) acc.counts[s] = (acc.counts[s] ?? 0) + (r.counts[s] ?? 0)
      return acc
    },
    { total: 0, counts: {} as Partial<Record<RunStatus, number>> },
  )

  const byCluster = new Map<string, ClusterRunStats>()
  for (const r of rows) if (r.cluster_id) byCluster.set(r.cluster_id, r)

  return (
    <>
      <header className="topbar">
        <div className="topbar-title">
          <h1>Statistics</h1>
          {(stats.isFetching || clusters.isFetching) && <Spinner />}
        </div>
        <div className="topbar-actions">
          {RANGES.map((r) => (
            <button
              key={r.key}
              className={r.key === range ? 'sm primary' : 'sm ghost'}
              aria-pressed={r.key === range}
              onClick={() => setRange(r.key)}
            >
              {r.label}
            </button>
          ))}
        </div>
      </header>

      <div className="content">
        <ErrorBanner error={stats.error} what="load the statistics" />
        <ErrorBanner error={clusters.error} what="load the clusters" />

        <Card tight title="Runs by cluster">
          {rows.length === 0 ? (
            !stats.isLoading && <Empty>No runs were created in this window.</Empty>
          ) : (
            <div className="table-wrap">
              <table>
                <thead>
                  <tr>
                    <th>Cluster</th>
                    {shown.map((s) => (
                      <th key={s} className="num">
                        <StatusBadge status={s} />
                      </th>
                    ))}
                    <th className="num">Total</th>
                  </tr>
                </thead>
                <tbody>
                  {rows.map((row) => (
                    <tr key={row.cluster_id ?? 'unplaced'}>
                      <td>{clusterLabel(row)}</td>
                      {shown.map((s) => (
                        <td key={s} className="num">
                          <CountCell row={row} status={s} />
                        </td>
                      ))}
                      <td className="num">
                        <strong>{row.total}</strong>
                      </td>
                    </tr>
                  ))}
                  {rows.length > 1 && (
                    <tr>
                      <td className="muted">All clusters</td>
                      {shown.map((s) => (
                        <td key={s} className="num muted">
                          {totals.counts[s] ?? 0}
                        </td>
                      ))}
                      <td className="num">
                        <strong>{totals.total}</strong>
                      </td>
                    </tr>
                  )}
                </tbody>
              </table>
            </div>
          )}
        </Card>
        <p className="small muted" style={{ margin: 0 }}>
          Succeeded means the agent exited 0, not that the task was solved. No result means the run
          ended and its completion report never arrived.
        </p>

        <Card tight title="Timing and capacity">
          {(clusters.data ?? []).length === 0 ? (
            !clusters.isLoading && <Empty>No clusters are registered.</Empty>
          ) : (
            <div className="table-wrap">
              <table>
                <thead>
                  <tr>
                    <th>Status</th>
                    <th>Cluster</th>
                    <th className="num" title="From first Running to the end, for runs created in this window">
                      Duration p50 / p95
                    </th>
                    <th className="num" title="From queued to Running, for runs created in this window">
                      Queue wait p50 / p95
                    </th>
                    <th className="num" title="Runs the backend believes are on the cluster now">
                      Active
                    </th>
                    <th className="num">Free / capacity</th>
                    <th>Heartbeat</th>
                  </tr>
                </thead>
                <tbody>
                  {(clusters.data ?? []).map((cluster: Cluster) => {
                    const row = byCluster.get(cluster.cluster_id)
                    return (
                      <tr key={cluster.cluster_id}>
                        <td className="nowrap">
                          <ClusterBadge status={cluster.status} stale={isStale(cluster)} />
                        </td>
                        <td>{cluster.name}</td>
                        <td className="num">
                          {row ? <QuantileCell q={row.duration_seconds} /> : <span className="faint">—</span>}
                        </td>
                        <td className="num">
                          {row ? <QuantileCell q={row.queue_wait_seconds} /> : <span className="faint">—</span>}
                        </td>
                        <td className="num">{cluster.active_runs ?? 0}</td>
                        <td className="num nowrap">
                          {cluster.free_slots} / {cluster.capacity_slots}
                        </td>
                        <td>
                          <Time at={cluster.last_heartbeat_at} />
                        </td>
                      </tr>
                    )
                  })}
                </tbody>
              </table>
            </div>
          )}
        </Card>
      </div>
    </>
  )
}
