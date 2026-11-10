import { useEffect, useState, type ReactNode } from 'react'
import { Link } from 'react-router-dom'
import { useCreateToken, useRemoveToken, useRevokeToken, useTokens } from '../api/hooks'
import { ApiError } from '../api/client'
import { Card, CopyButton, Dialog, Empty, ErrorBanner, Field, Spinner, Time } from './ui'
import { SCOPES, type ApiToken, type ApiTokenSecret } from '../api/types'

// API tokens are shown where they belong rather than on a page of their own:
// the ones minted by hand in the settings, a run's own token on the run, and a
// cluster's bootstrap tokens on the Clusters page (those are a different table
// and a different endpoint altogether).

function NewTokenDialog({ onClose }: { onClose: () => void }) {
  const create = useCreateToken()
  const [name, setName] = useState('')
  const [subject, setSubject] = useState('')
  const [days, setDays] = useState(90)
  const [scopes, setScopes] = useState<string[]>(['runs:read'])
  const [minted, setMinted] = useState<ApiTokenSecret | null>(null)

  const toggle = (scope: string) =>
    setScopes((current) =>
      current.includes(scope) ? current.filter((s) => s !== scope) : [...current, scope],
    )

  if (minted) {
    return (
      <Dialog
        title="API token"
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
              Only a digest is stored, so there is no second chance to read this.
            </div>
          </div>
        </div>
        <Field label={`Token for ${minted.name}`}>
          <input readOnly value={minted.token} onFocus={(e) => e.currentTarget.select()} />
        </Field>
        <CopyButton value={minted.token} label="Copy token" />
      </Dialog>
    )
  }

  return (
    <Dialog
      title="New API token"
      onClose={onClose}
      footer={
        <>
          <button className="ghost" onClick={onClose}>
            Cancel
          </button>
          <button
            className="primary"
            disabled={!name.trim() || scopes.length === 0 || create.isPending}
            onClick={() =>
              create.mutate(
                {
                  name: name.trim(),
                  scopes,
                  subject: subject.trim() || undefined,
                  ttl_seconds: days > 0 ? days * 86400 : undefined,
                },
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
      <Field label="Name">
        <input value={name} onChange={(e) => setName(e.target.value)} placeholder="ci-pipeline" />
      </Field>
      <Field label="Scopes" hint="admin implies the others.">
        <div className="checks">
          {SCOPES.map((scope) => (
            <label key={scope} className={scopes.includes(scope) ? 'check on' : 'check'}>
              <input type="checkbox" checked={scopes.includes(scope)} onChange={() => toggle(scope)} />
              {scope}
            </label>
          ))}
        </div>
      </Field>
      <div className="grid-2">
        <Field label="Subject" hint="Who runs created with it are attributed to.">
          <input value={subject} onChange={(e) => setSubject(e.target.value)} />
        </Field>
        <Field label="Valid for (days)" hint="0 for no expiry.">
          <input type="number" min={0} value={days} onChange={(e) => setDays(Number(e.target.value))} />
        </Field>
      </div>
    </Dialog>
  )
}

function RevokeDialog({
  token,
  onClose,
  children,
}: {
  token: ApiToken
  onClose: () => void
  children: ReactNode
}) {
  const revoke = useRevokeToken()
  return (
    <Dialog
      title={`Revoke ${token.name}`}
      onClose={onClose}
      footer={
        <>
          <button className="ghost" onClick={onClose}>
            Keep it
          </button>
          <button
            className="danger"
            disabled={revoke.isPending}
            onClick={() => revoke.mutate(token.token_id, { onSuccess: onClose })}
          >
            {revoke.isPending ? 'Revoking…' : 'Revoke token'}
          </button>
        </>
      }
    >
      <ErrorBanner error={revoke.error} what="revoke the token" />
      <p className="small muted" style={{ margin: 0 }}>
        {children}
      </p>
    </Dialog>
  )
}

function RemoveDialog({ token, onClose }: { token: ApiToken; onClose: () => void }) {
  const remove = useRemoveToken()
  return (
    <Dialog
      title={`Remove ${token.name}`}
      onClose={onClose}
      footer={
        <>
          <button className="ghost" onClick={onClose}>
            Keep it
          </button>
          <button
            className="danger"
            disabled={remove.isPending}
            onClick={() => remove.mutate(token.token_id, { onSuccess: onClose })}
          >
            {remove.isPending ? 'Removing…' : 'Remove token'}
          </button>
        </>
      }
    >
      <ErrorBanner error={remove.error} what="remove the token" />
      <p className="small muted" style={{ margin: 0 }}>
        The token has already stopped working. Removing it deletes it from this list for good;
        the audit log keeps a record that it was removed.
      </p>
    </Dialog>
  )
}

// The bootstrap row stays after it is revoked: it is what stops the next start
// from installing the value still in the chart's Secret as a fresh admin token.
const isBootstrap = (token: ApiToken) => token.kind === 'user' && token.name === 'bootstrap'

const hasExpired = (token: ApiToken) =>
  token.expires_at !== undefined && Date.parse(token.expires_at) <= Date.now()

const hasEnded = (token: ApiToken) => !!token.revoked_at || hasExpired(token)

function Scopes({ token }: { token: ApiToken }) {
  return (
    <div className="inline">
      {token.scopes?.map((scope) => (
        <span key={scope} className={scope === 'admin' ? 'badge warn' : 'badge idle'}>
          {scope}
        </span>
      ))}
    </div>
  )
}

function EndedBadge({ token }: { token: ApiToken }) {
  return <span className="badge bad">{token.revoked_at ? 'revoked' : 'expired'}</span>
}

/**
 * The tokens minted by hand — service tokens from this card, and the user row
 * the chart's bootstrap credential is installed as. Shown on the Settings page.
 */
export function ApiTokensCard() {
  const tokens = useTokens({ kind: ['user', 'service'] })
  const [creating, setCreating] = useState(false)
  const [revoking, setRevoking] = useState<ApiToken | null>(null)
  const [removing, setRemoving] = useState<ApiToken | null>(null)
  const forbidden = tokens.error instanceof ApiError && tokens.error.forbidden

  return (
    <>
      <Card
        tight
        title={
          <h2 className="inline">
            API tokens {tokens.isFetching && <Spinner />}
          </h2>
        }
        actions={
          !forbidden && (
            <button className="sm primary" onClick={() => setCreating(true)}>
              New token
            </button>
          )
        }
      >
        <p className="small muted" style={{ margin: 0, padding: '10px 14px' }}>
          Credentials for the REST API and MCP, minted by hand and removed by hand. Each run's own MCP
          token is on that run's page and is deleted with it; cluster bootstrap tokens are on the{' '}
          <Link to="/clusters">Clusters</Link> page.
        </p>
        {forbidden ? (
          <Empty>Listing and minting tokens needs an admin token.</Empty>
        ) : (
          <>
            {tokens.error && (
              <div style={{ padding: '0 14px' }}>
                <ErrorBanner error={tokens.error} what="load the tokens" />
              </div>
            )}
            <div className="table-wrap">
              <table>
                <thead>
                  <tr>
                    <th>Name</th>
                    <th>Kind</th>
                    <th>Scopes</th>
                    <th>Subject</th>
                    <th>Expires</th>
                    <th>Created</th>
                    <th />
                  </tr>
                </thead>
                <tbody>
                  {(tokens.data ?? []).map((token) => (
                    <tr key={token.token_id}>
                      <td>{token.name}</td>
                      <td className="small muted">{token.kind}</td>
                      <td>
                        <Scopes token={token} />
                      </td>
                      <td className="small muted">{token.subject || '—'}</td>
                      <td className="small muted">
                        <Time at={token.expires_at} />
                      </td>
                      <td className="small muted">
                        <Time at={token.created_at} />
                      </td>
                      <td className="nowrap">
                        {hasEnded(token) ? (
                          <div className="inline">
                            <EndedBadge token={token} />
                            {!isBootstrap(token) && (
                              <button className="sm ghost" onClick={() => setRemoving(token)}>
                                Remove
                              </button>
                            )}
                          </div>
                        ) : (
                          <button className="sm danger" onClick={() => setRevoking(token)}>
                            Revoke
                          </button>
                        )}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
            {(tokens.data?.length ?? 0) === 0 && !tokens.isLoading && !tokens.error && (
              <Empty>No token has been minted.</Empty>
            )}
          </>
        )}
      </Card>

      {creating && <NewTokenDialog onClose={() => setCreating(false)} />}
      {revoking && (
        <RevokeDialog token={revoking} onClose={() => setRevoking(null)}>
          Anything still holding this token stops being able to call the API — including this
          browser, if it is the one you signed in with.
        </RevokeDialog>
      )}
      {removing && <RemoveDialog token={removing} onClose={() => setRemoving(null)} />}
    </>
  )
}

/**
 * The run's own MCP token. Minted by the backend when the run's lease is
 * built, revoked when the run ends, deleted with the run — there is nothing to
 * mint or remove by hand. A live one can be revoked to stop the agent starting
 * child runs. Absent for a caller that cannot list tokens, and for a run that
 * never reached the MCP endpoint.
 */
export function RunTokensCard({ runId, live }: { runId: string; live: boolean }) {
  const tokens = useTokens({ kind: ['run-mcp'], run_id: runId })
  const [revoking, setRevoking] = useState<ApiToken | null>(null)
  const { refetch } = tokens

  // The backend revokes the token as the run ends; show it ended without
  // waiting for the next focus.
  useEffect(() => {
    if (!live) void refetch()
  }, [live, refetch])

  if (tokens.error instanceof ApiError && tokens.error.forbidden) return null
  if (!tokens.error && (tokens.data?.length ?? 0) === 0) return null

  return (
    <>
      <Card title="MCP token" tight>
        {tokens.error && (
          <div style={{ padding: 14 }}>
            <ErrorBanner error={tokens.error} what="load the run's token" />
          </div>
        )}
        {(tokens.data?.length ?? 0) > 0 && (
          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th>Scopes</th>
                  <th>Expires</th>
                  <th>Created</th>
                  <th />
                </tr>
              </thead>
              <tbody>
                {tokens.data!.map((token) => (
                  <tr key={token.token_id}>
                    <td>
                      <Scopes token={token} />
                    </td>
                    <td className="small muted">
                      <Time at={token.expires_at} />
                    </td>
                    <td className="small muted">
                      <Time at={token.created_at} />
                    </td>
                    <td className="nowrap">
                      {hasEnded(token) ? (
                        <EndedBadge token={token} />
                      ) : (
                        <button className="sm danger" onClick={() => setRevoking(token)}>
                          Revoke
                        </button>
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </Card>

      {revoking && (
        <RevokeDialog token={revoking} onClose={() => setRevoking(null)}>
          The agent stops being able to call Haliphron's MCP tools — starting child runs, taking
          locks, reading runs. The run itself keeps going; cancel it to stop it.
        </RevokeDialog>
      )}
    </>
  )
}
