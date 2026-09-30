import { useState } from 'react'
import { useModelCredential, usePutModelCredential, usePutSecret, useSecrets } from '../api/hooks'
import { Card, Dialog, Empty, ErrorBanner, Field, Spinner, Time } from '../components/ui'
import type { ModelCredential, ModelCredentialType, Secret } from '../api/types'

type Kind = 'managed' | 'referenced'

/** The prefix of a Claude subscription token; the backend and the pod test the same one. */
const OAUTH_PREFIX = 'sk-ant-oat'

const TYPE_LABEL: Record<ModelCredentialType, string> = {
  api_key: 'API key',
  oauth_token: 'Claude subscription (OAuth)',
}

/** What is wrong with a pasted value for the chosen type, before the backend says the same. */
function mismatch(type: ModelCredentialType, value: string): string | undefined {
  const v = value.trim()
  if (!v) return undefined
  const oauth = v.startsWith(OAUTH_PREFIX)
  if (type === 'oauth_token' && !oauth) {
    return `Not an OAuth token: those start with ${OAUTH_PREFIX}… and come from \`claude setup-token\`.`
  }
  if (type === 'api_key' && oauth) {
    return 'This is a Claude subscription token, not an API key. Choose “Claude subscription (OAuth)”.'
  }
  return undefined
}

function ModelCredentialDialog({ current, onClose }: { current?: ModelCredential; onClose: () => void }) {
  const put = usePutModelCredential()
  const [type, setType] = useState<ModelCredentialType>(current?.type ?? 'api_key')
  const [kind, setKind] = useState<Kind>(current?.kind === 'referenced' ? 'referenced' : 'managed')
  const [value, setValue] = useState('')
  const [ref, setRef] = useState(current?.ref ?? '')

  const wrong = kind === 'managed' ? mismatch(type, value) : undefined
  const ready = (kind === 'managed' ? value.trim().length > 0 && !wrong : ref.trim().length > 0)

  const save = () =>
    put.mutate(
      kind === 'managed' ? { type, value: value.trim() } : { type, ref: ref.trim() },
      { onSuccess: onClose },
    )

  return (
    <Dialog
      title={current?.configured ? 'Replace the model credential' : 'Set the model credential'}
      onClose={onClose}
      footer={
        <>
          <button className="ghost" onClick={onClose}>
            Cancel
          </button>
          <button className="primary" onClick={save} disabled={!ready || put.isPending}>
            {put.isPending ? 'Saving…' : 'Save credential'}
          </button>
        </>
      }
    >
      <ErrorBanner error={put.error} what="save the model credential" />

      <Field label="Credential type">
        <div className="checks">
          {(Object.keys(TYPE_LABEL) as ModelCredentialType[]).map((t) => (
            <label key={t} className={type === t ? 'check on' : 'check'}>
              <input type="radio" checked={type === t} onChange={() => setType(t)} />
              {TYPE_LABEL[t]}
            </label>
          ))}
        </div>
      </Field>
      <div className="hint">
        {type === 'api_key' ? (
          <>
            An Anthropic key (<span className="mono">sk-ant-api…</span>) for claude-code, an OpenAI key for codex,
            or the token of a gateway in front of either. Billed per token by the provider.
          </>
        ) : (
          <>
            Run <span className="mono">claude setup-token</span> on a machine signed in to a Pro or Max plan and
            paste the <span className="mono">sk-ant-oat…</span> token it prints. Works for claude-code runs only —
            codex runs are refused — and runs count against the plan&apos;s usage limits, not an API bill.
          </>
        )}
      </div>

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
          label={type === 'oauth_token' ? 'OAuth token' : 'API key'}
          error={wrong}
          hint="Encrypted under a key of its own. No endpoint can read it back."
        >
          <input
            type="password"
            value={value}
            autoComplete="off"
            placeholder={type === 'oauth_token' ? 'sk-ant-oat01-…' : 'sk-ant-api03-…'}
            onChange={(e) => setValue(e.target.value)}
          />
        </Field>
      ) : (
        <Field
          label="Reference"
          hint="The backend does not resolve references, so leases cannot carry one yet. Prefer a managed value."
        >
          <input value={ref} onChange={(e) => setRef(e.target.value)} placeholder="vault://kv/data/agents#llm" />
        </Field>
      )}
    </Dialog>
  )
}

/** The one secret every run needs, shown on its own so it cannot be missed. */
function ModelCredentialCard() {
  const cred = useModelCredential()
  const [editing, setEditing] = useState(false)
  const c = cred.data
  const ok = c?.configured && !c.problem

  return (
    <Card
      title="Model credential"
      actions={
        c?.secret_name ? (
          <button className={ok ? 'sm ghost' : 'sm primary'} onClick={() => setEditing(true)}>
            {c.configured ? 'Replace' : 'Set credential'}
          </button>
        ) : undefined
      }
    >
      <ErrorBanner error={cred.error} what="read the model credential" />
      {c && (
        <div className="stack">
          {!c.configured && c.secret_name && (
            <div className="banner" role="alert">
              <div>
                <div className="banner-title">Not set — every run will fail</div>
                <div className="small muted">
                  Runs are leased without a model key and the agent pod stops in its validate phase with{' '}
                  <span className="mono">MissingSecret</span>.
                </div>
              </div>
            </div>
          )}
          {c.problem && (
            <div className="banner" role="alert">
              <div>
                <div className="banner-title">Stored, but runs will not receive it</div>
                <div className="small muted">{c.problem}</div>
              </div>
            </div>
          )}
          <dl className="kv">
            <dt>Status</dt>
            <dd>
              <span className={ok ? 'badge ok' : 'badge bad'}>{ok ? 'ready' : c.configured ? 'unusable' : 'missing'}</span>
            </dd>
            <dt>Type</dt>
            <dd>{c.type ? TYPE_LABEL[c.type] : '—'}</dd>
            <dt>Stored as</dt>
            <dd>
              <span className="mono small">{c.secret_name || '—'}</span>
              {c.kind && <span className="small muted"> · {c.kind}</span>}
            </dd>
            <dt>Updated</dt>
            <dd className="small muted">
              <Time at={c.updated_at} />
            </dd>
          </dl>
        </div>
      )}
      {editing && <ModelCredentialDialog current={c} onClose={() => setEditing(false)} />}
    </Card>
  )
}

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
  const modelSecret = useModelCredential().data?.secret_name
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
        <ModelCredentialCard />
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
                    <td className="small">
                      <span className="mono">{secret.name}</span>
                      {secret.name === modelSecret && (
                        <>
                          {' '}
                          <span className="badge idle">model credential</span>
                        </>
                      )}
                    </td>
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
