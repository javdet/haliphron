import type { FailureClass } from '../api/types'

// Which colour a status gets. CompletedWithoutResult is deliberately a warning
// and not a success: the run ended, and what it did is not known here.
const TONE: Record<string, string> = {
  Queued: 'idle',
  Leased: 'busy',
  Dispatched: 'busy',
  Starting: 'busy',
  Running: 'busy',
  Succeeded: 'ok',
  Failed: 'bad',
  TimedOut: 'bad',
  Cancelled: 'idle',
  CompletedWithoutResult: 'warn',
  Unknown: 'warn',
}

export function StatusBadge({ status }: { status: string }) {
  const tone = TONE[status] ?? 'idle'
  const label = status === 'CompletedWithoutResult' ? 'No result' : status
  return (
    <span className={`badge ${tone}`} title={status}>
      <span className="dot" />
      {label}
    </span>
  )
}

const CLASS_TONE: Record<FailureClass, string> = {
  none: 'idle',
  infra: 'warn',
  agent: 'bad',
  git: 'bad',
  config: 'bad',
  budget: 'warn',
}

export function FailureBadge({ failureClass }: { failureClass?: string }) {
  if (!failureClass || failureClass === 'none') return null
  const tone = CLASS_TONE[failureClass as FailureClass] ?? 'bad'
  return <span className={`badge ${tone}`}>{failureClass}</span>
}

export function ClusterBadge({ status, stale }: { status: string; stale: boolean }) {
  if (status === 'Revoked') return <span className="badge bad">Revoked</span>
  // The backend's own verdict wins where it has one; the heartbeat clock below
  // is a second opinion for the window between a cluster going quiet and the
  // sweeper noticing.
  if (status === 'Unreachable' || stale) return <span className="badge warn">Unreachable</span>
  return (
    <span className="badge ok">
      <span className="dot" />
      {status || 'Active'}
    </span>
  )
}
