import { useState } from 'react'
import {
  useBootstrapTokens,
  useClusters,
  useCreateBootstrapToken,
  useRevokeCluster,
} from '../api/hooks'
import { ClusterBadge } from '../components/StatusBadge'
import { Card, CopyButton, Dialog, Empty, ErrorBanner, Field, Spinner, Time } from '../components/ui'
import type { BootstrapTokenSecret, Cluster } from '../api/types'

// A cluster with no recent heartbeat is not failed, it is unreachable — the
// distinction the architecture makes, so the UI makes it too.
//
// 90s is the cluster contract's own StaleAfterSeconds default
// (api/cluster/v1/register.go, DefaultTimings), which is nine heartbeats. It is
// duplicated here rather than fetched because no endpoint publishes the
// timings to a browser; if that default moves, this moves with it.
const STALE_AFTER_MS = 90_000

function isStale(cluster: Cluster): boolean {
  if (!cluster.last_heartbeat_at) return true
  return Date.now() - new Date(cluster.last_heartbeat_at).getTime() > STALE_AFTER_MS
}

function RevokeDialog({ cluster, onClose }: { cluster: Cluster; onClose: () => void }) {
  const revoke = useRevokeCluster()
  const [reason, setReason] = useState('')
  return (
    <Dialog
      title={`Revoke ${cluster.name}`}
      onClose={onClose}
      footer={
        <>
          <button className="ghost" onClick={onClose}>
            Keep it
          </button>
          <button
            className="danger"
            disabled={revoke.isPending}
            onClick={() => revoke.mutate({ id: cluster.cluster_id, reason }, { onSuccess: onClose })}
          >
            {revoke.isPending ? 'Revoking…' : 'Revoke cluster'}
          </button>
        </>
      }
    >
      <ErrorBanner error={revoke.error} what="revoke the cluster" />
      <p className="small muted" style={{ margin: 0 }}>
        The controller stops being able to lease work. Runs already in flight are left alone —
        what to do with them is your decision, not a side effect of this one.
      </p>
      <Field label="Reason" hint="Recorded in the audit log.">
        <input value={reason} onChange={(e) => setReason(e.target.value)} />
      </Field>
    </Dialog>
  )
}

function NewBootstrapTokenDialog({ onClose }: { onClose: () => void }) {
  const create = useCreateBootstrapToken()
  const [name, setName] = useState('')
  const [hours, setHours] = useState(24)
  const [maxUses, setMaxUses] = useState(1)
  const [minted, setMinted] = useState<BootstrapTokenSecret | null>(null)

  if (minted) {
    return (
      <Dialog
        title="Bootstrap token"
        onClose={onClose}
        footer={
          <button className="primary" onClick={onClose}>
            Done
          </button>
        }
      >
        <div className="banner good">
          <div>
            <div className="banner-title">Copy it now</div>
            <div className="small">
              Only a digest is stored. This is the one time the value can be read.
            </div>
          </div>
        </div>
        <Field label="Token">
          <input readOnly value={minted.token} onFocus={(e) => e.currentTarget.select()} />
        </Field>
        <CopyButton value={minted.token} label="Copy token" />
      </Dialog>
    )
  }

  return (
    <Dialog
      title="New bootstrap token"
      onClose={onClose}
      footer={
        <>
          <button className="ghost" onClick={onClose}>
            Cancel
          </button>
          <button
            className="primary"
            disabled={!name.trim() || create.isPending}
            onClick={() =>
              create.mutate(
                { name: name.trim(), ttl_seconds: hours * 3600, max_uses: maxUses },
                { onSuccess: setMinted },
              )
            }
          >
            {create.isPending ? 'Minting…' : 'Mint token'}
          </button>
        </>
      }
    >
      <ErrorBanner error={create.error} what="mint the token" />
      <p className="small muted" style={{ margin: 0 }}>
        This is the credential a controller registers with, once. Pass it to the target cluster's
        Helm release.
      </p>
      <Field label="Name" hint="How the cluster will be identified at registration.">
        <input value={name} onChange={(e) => setName(e.target.value)} placeholder="prod-eu-west-1" />
      </Field>
      <div className="grid-2">
        <Field label="Valid for (hours)">
          <input type="number" min={1} value={hours} onChange={(e) => setHours(Number(e.target.value))} />
        </Field>
        <Field label="Max uses">
          <input type="number" min={1} value={maxUses} onChange={(e) => setMaxUses(Number(e.target.value))} />
        </Field>
      </div>
    </Dialog>
  )
}

export function ClustersPage() {
  const clusters = useClusters()
  const bootstrap = useBootstrapTokens()
  const [revoking, setRevoking] = useState<Cluster | null>(null)
  const [minting, setMinting] = useState(false)

  return (
    <>
      <header className="topbar">
        <div className="topbar-title">
          <h1>Clusters</h1>
          {clusters.isFetching && <Spinner />}
        </div>
        <div className="topbar-actions">
          <button className="primary" onClick={() => setMinting(true)}>
            New bootstrap token
          </button>
        </div>
      </header>

      <div className="content">
        <ErrorBanner error={clusters.error} what="load the clusters" />

        <Card tight title={undefined}>
          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th>Status</th>
                  <th>Cluster</th>
                  <th>Namespace</th>
                  <th>Versions</th>
                  <th>Runtimes</th>
                  <th className="num">Slots</th>
                  <th>Heartbeat</th>
                  <th />
                </tr>
              </thead>
              <tbody>
                {(clusters.data ?? []).map((cluster) => (
                  <tr key={cluster.cluster_id}>
                    <td className="nowrap">
                      <ClusterBadge status={cluster.status} stale={isStale(cluster)} />
                      {cluster.quota_exhausted && <div className="badge warn">quota</div>}
                    </td>
                    <td>
                      <div>{cluster.name}</div>
                      <div className="mono small faint">{cluster.cluster_id}</div>
                      {cluster.revoked_reason && (
                        <div className="small muted">{cluster.revoked_reason}</div>
                      )}
                    </td>
                    <td className="small muted nowrap">{cluster.agent_namespace}</td>
                    <td className="small muted nowrap">
                      <div>controller {cluster.controller_version}</div>
                      {cluster.k8s_version && <div className="faint">k8s {cluster.k8s_version}</div>}
                    </td>
                    <td className="small muted">{cluster.runtimes?.join(', ') || '—'}</td>
                    <td className="num nowrap">
                      {cluster.free_slots} / {cluster.capacity_slots}
                    </td>
                    <td className="small muted">
                      <Time at={cluster.last_heartbeat_at} />
                    </td>
                    <td className="nowrap">
                      {cluster.status !== 'Revoked' && (
                        <button className="sm danger" onClick={() => setRevoking(cluster)}>
                          Revoke
                        </button>
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          {(clusters.data?.length ?? 0) === 0 && !clusters.isLoading && (
            <Empty>No cluster has registered. Mint a bootstrap token to add one.</Empty>
          )}
        </Card>

        <Card title="Bootstrap tokens">
          <ErrorBanner error={bootstrap.error} what="load the bootstrap tokens" />
          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th>Name</th>
                  <th className="num">Uses</th>
                  <th>Expires</th>
                  <th>Created</th>
                  <th>State</th>
                </tr>
              </thead>
              <tbody>
                {(bootstrap.data ?? []).map((token) => (
                  <tr key={token.token_id}>
                    <td>{token.name}</td>
                    <td className="num">
                      {token.uses} / {token.max_uses}
                    </td>
                    <td className="small muted">
                      <Time at={token.expires_at} />
                    </td>
                    <td className="small muted">
                      <Time at={token.created_at} />
                      <div className="faint">{token.created_by}</div>
                    </td>
                    <td>
                      {token.revoked_at ? (
                        <span className="badge bad">revoked</span>
                      ) : (
                        <span className="badge idle">live</span>
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          {(bootstrap.data?.length ?? 0) === 0 && !bootstrap.isLoading && (
            <Empty>No bootstrap token has been minted.</Empty>
          )}
        </Card>
      </div>

      {revoking && <RevokeDialog cluster={revoking} onClose={() => setRevoking(null)} />}
      {minting && <NewBootstrapTokenDialog onClose={() => setMinting(false)} />}
    </>
  )
}
