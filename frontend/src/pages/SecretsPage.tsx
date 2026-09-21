import { useState } from 'react'
import { usePutSecret, useSecrets } from '../api/hooks'
import { Card, Dialog, Empty, ErrorBanner, Field, Spinner, Time } from '../components/ui'
import type { Secret } from '../api/types'

type Kind = 'managed' | 'referenced'

function SecretDialog({ existing, onClose }: { existing: Secret | null; onClose: () => void }) {
  const put = usePutSecret()
  const [name, setName] = useState(existing?.name ?? '')
  const [kind, setKind] = useState<Kind>(existing?.ref ? 'referenced' : 'managed')
  const [value, setValue] = useState('')
  const [ref, setRef] = useState(existing?.ref ?? '')

  const save = () =>
    put.mutate(
      kind === 'managed'
        ? { name: name.trim(), value }
        : { name: name.trim(), ref: ref.trim() },
      { onSuccess: onClose },
    )

  const ready = name.trim() && (kind === 'managed' ? value.length > 0 : ref.trim().length > 0)

  return (
    <Dialog
      title={existing ? `Rotate ${existing.name}` : 'New secret'}
      onClose={onClose}
      footer={
        <>
          <button className="ghost" onClick={onClose}>
            Cancel
          </button>
          <button className="primary" onClick={save} disabled={!ready || put.isPending}>
            {put.isPending ? 'Saving…' : 'Save secret'}
          </button>
        </>
      }
    >
      <ErrorBanner error={put.error} what="save the secret" />
      <Field label="Name">
        <input value={name} disabled={!!existing} onChange={(e) => setName(e.target.value)} />
      </Field>

      <div className="checks">
        <label className={kind === 'managed' ? 'check on' : 'check'}>
          <input type="radio" checked={kind === 'managed'} onChange={() => setKind('managed')} />
          Managed here
        </label>
        <label className={kind === 'referenced' ? 'check on' : 'check'}>
          <input type="radio" checked={kind === 'referenced'} onChange={() => setKind('referenced')} />
          Referenced
        </label>
      </div>

      {kind === 'managed' ? (
        <Field
          label="Value"
          hint="Encrypted under a key of its own. No endpoint can read it back — not this one either."
        >
          <input type="password" value={value} autoComplete="off" onChange={(e) => setValue(e.target.value)} />
        </Field>
      ) : (
        <Field label="Reference" hint="A pointer into your Vault or External Secrets. The backend never resolves it.">
          <input value={ref} onChange={(e) => setRef(e.target.value)} placeholder="vault://kv/data/agents#token" />
        </Field>
      )}
    </Dialog>
  )
}

export function SecretsPage() {
  const secrets = useSecrets()
  const [editing, setEditing] = useState<Secret | null>(null)
  const [creating, setCreating] = useState(false)

  return (
    <>
      <header className="topbar">
        <div className="topbar-title">
          <h1>Secrets</h1>
          {secrets.isFetching && <Spinner />}
        </div>
        <div className="topbar-actions">
          <button className="primary" onClick={() => setCreating(true)}>
            New secret
          </button>
        </div>
      </header>

      <div className="content">
        <ErrorBanner error={secrets.error} what="load the secrets" />
        <div className="banner info">
          <div>
            <div className="banner-title">Values are never shown</div>
            <div className="small muted">
              The API returns names, kinds and references only. A secret can be replaced, not read.
            </div>
          </div>
        </div>

        <Card tight>
          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th>Name</th>
                  <th>Kind</th>
                  <th>Reference</th>
                  <th>Updated</th>
                  <th>Rotated</th>
                  <th />
                </tr>
              </thead>
              <tbody>
                {(secrets.data ?? []).map((secret) => (
                  <tr key={secret.name}>
                    <td className="mono small">{secret.name}</td>
                    <td>
                      <span className="badge idle">{secret.kind}</span>
                    </td>
                    <td className="small muted">
                      <span className="trunc">{secret.ref || '—'}</span>
                    </td>
                    <td className="small muted">
                      <Time at={secret.updated_at} />
                    </td>
                    <td className="small muted">
                      <Time at={secret.rotated_at} />
                    </td>
                    <td className="nowrap">
                      <button className="sm ghost" onClick={() => setEditing(secret)}>
                        Rotate
                      </button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          {(secrets.data?.length ?? 0) === 0 && !secrets.isLoading && (
            <Empty>No secret is stored.</Empty>
          )}
        </Card>
      </div>

      {(creating || editing) && (
        <SecretDialog
          existing={editing}
          onClose={() => {
            setCreating(false)
            setEditing(null)
          }}
        />
      )}
    </>
  )
}
