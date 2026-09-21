import { useState } from 'react'
import { useCreateToken, useRevokeToken, useTokens } from '../api/hooks'
import { Card, CopyButton, Dialog, Empty, ErrorBanner, Field, Spinner, Time } from '../components/ui'
import { SCOPES, type ApiToken, type ApiTokenSecret } from '../api/types'

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

function RevokeDialog({ token, onClose }: { token: ApiToken; onClose: () => void }) {
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
        Anything still holding this token stops being able to call the API — including this
        browser, if it is the one you signed in with.
      </p>
    </Dialog>
  )
}

export function TokensPage() {
  const tokens = useTokens()
  const [creating, setCreating] = useState(false)
  const [revoking, setRevoking] = useState<ApiToken | null>(null)

  return (
    <>
      <header className="topbar">
        <div className="topbar-title">
          <h1>Tokens</h1>
          {tokens.isFetching && <Spinner />}
        </div>
        <div className="topbar-actions">
          <button className="primary" onClick={() => setCreating(true)}>
            New token
          </button>
        </div>
      </header>

      <div className="content">
        <ErrorBanner error={tokens.error} what="load the tokens" />
        <Card tight>
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
                    <td>
                      <div>{token.name}</div>
                      {token.run_id && (
                        <div className="mono small faint">run {token.run_id}</div>
                      )}
                    </td>
                    <td className="small muted">{token.kind}</td>
                    <td>
                      <div className="inline">
                        {token.scopes?.map((scope) => (
                          <span key={scope} className={scope === 'admin' ? 'badge warn' : 'badge idle'}>
                            {scope}
                          </span>
                        ))}
                      </div>
                    </td>
                    <td className="small muted">{token.subject || '—'}</td>
                    <td className="small muted">
                      <Time at={token.expires_at} />
                    </td>
                    <td className="small muted">
                      <Time at={token.created_at} />
                    </td>
                    <td className="nowrap">
                      {token.revoked_at ? (
                        <span className="badge bad">revoked</span>
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
          {(tokens.data?.length ?? 0) === 0 && !tokens.isLoading && <Empty>No token exists.</Empty>}
        </Card>
      </div>

      {creating && <NewTokenDialog onClose={() => setCreating(false)} />}
      {revoking && <RevokeDialog token={revoking} onClose={() => setRevoking(null)} />}
    </>
  )
}
