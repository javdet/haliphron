import { useState } from 'react'
import {
  useGitCredential,
  useModelCredential,
  usePutGitCredential,
  usePutModelCredential,
  usePutSecret,
  useSecrets,
} from '../api/hooks'
import { Card, Dialog, Empty, ErrorBanner, Field, Spinner, Time } from '../components/ui'
import type { GitCredential, GitToken, GitTokenProvider, ModelCredential, ModelCredentialType, Secret } from '../api/types'
import { FORGE_LABEL, forgeOf } from '../api/gitToken'

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

function GitTokenDialog({
  initial,
  cred,
  onClose,
}: {
  initial?: GitTokenProvider
  cred: GitCredential
  onClose: () => void
}) {
  const put = usePutGitCredential()
  const [provider, setProvider] = useState<GitTokenProvider | undefined>(initial)
  const [value, setValue] = useState('')

  const target = cred.tokens.find((t) => t.provider === provider)
  const v = value.trim()
  const forge = forgeOf(v)
  const wrong = /\s/.test(v)
    ? 'Paste the token alone — a token has no whitespace in it.'
    : provider && forge && forge !== provider
      ? `This looks like a ${FORGE_LABEL[forge]} token, not a ${FORGE_LABEL[provider]} one.`
      : undefined
  const ready = v.length > 0 && !wrong

  const save = () => put.mutate({ provider, value: v }, { onSuccess: onClose })

  return (
    <Dialog
      title={target?.configured ? `Replace ${target.secret_name}` : 'Set a git token'}
      onClose={onClose}
      footer={
        <>
          <button className="ghost" onClick={onClose}>
            Cancel
          </button>
          <button className="primary" onClick={save} disabled={!ready || put.isPending}>
            {put.isPending ? 'Saving…' : 'Save token'}
          </button>
        </>
      }
    >
      <ErrorBanner error={put.error} what="save the git token" />

      <Field label="Used for">
        <div className="checks">
          {cred.tokens.map((t) => (
            <label key={t.secret_name} className={provider === t.provider ? 'check on' : 'check'}>
              <input type="radio" checked={provider === t.provider} onChange={() => setProvider(t.provider)} />
              {t.provider ? `${FORGE_LABEL[t.provider]} only` : 'Every forge'}
            </label>
          ))}
        </div>
      </Field>
      <div className="hint">
        {provider === 'gitlab' ? (
          <>
            A project or group access token (<span className="mono">glpat-…</span>) with{' '}
            <span className="mono">write_repository</span>, and <span className="mono">api</span> if runs open merge
            requests.
          </>
        ) : provider === 'github' ? (
          <>
            A fine-grained personal access token (<span className="mono">github_pat_…</span>) limited to the
            repositories runs touch, with Contents and Pull requests set to read and write.
          </>
        ) : (
          <>
            Used for every repository whose forge has no token of its own. Give it the least access that pushes a
            branch and opens a pull request on the repositories runs touch.
          </>
        )}
      </div>

      <Field
        label="Token"
        error={wrong}
        hint="Encrypted under a key of its own and handed to a pod for one run. No endpoint can read it back."
      >
        <input
          type="password"
          value={value}
          autoComplete="off"
          placeholder={provider === 'gitlab' ? 'glpat-…' : 'github_pat_…'}
          onChange={(e) => setValue(e.target.value)}
        />
      </Field>
    </Dialog>
  )
}

function gitTokenStatus(token: GitToken, fallback?: GitToken): { label: string; className: string } {
  if (token.configured) return token.problem ? { label: 'unusable', className: 'badge bad' } : { label: 'ready', className: 'badge ok' }
  if (token.provider && fallback?.configured) return { label: 'uses fallback', className: 'badge idle' }
  return { label: 'missing', className: token.provider ? 'badge idle' : 'badge warn' }
}

/**
 * The secret most runs need and nothing asks for: its absence is allowed at
 * admission and found at clone or push. Shown on its own so it is set before
 * the first run rather than after the first failure.
 */
function GitCredentialCard() {
  const cred = useGitCredential()
  const [editing, setEditing] = useState<{ provider?: GitTokenProvider } | null>(null)
  const c = cred.data
  const fallback = c?.tokens.find((t) => !t.provider)

  return (
    <Card
      title="Git token"
      actions={
        c?.secret_name ? (
          <button className={c.configured ? 'sm ghost' : 'sm primary'} onClick={() => setEditing({})}>
            {c.configured ? 'Add or replace' : 'Set token'}
          </button>
        ) : undefined
      }
    >
      <ErrorBanner error={cred.error} what="read the git token" />
      {c && (
        <div className="stack">
          {c.problem && (
            <div className="banner" role="alert">
              <div>
                <div className="banner-title">Runs will not receive a git token</div>
                <div className="small muted">{c.problem}</div>
              </div>
            </div>
          )}
          {!c.configured && c.secret_name && (
            <div className="banner warn" role="alert">
              <div>
                <div className="banner-title">Not set — most runs with a repository will fail</div>
                <div className="small muted">
                  A private repository fails at <span className="mono">clone</span> and a pull request at{' '}
                  <span className="mono">push</span>, both with exit code 20. Only public repositories without a pull
                  request work without one.
                </div>
              </div>
            </div>
          )}
          {c.tokens.map(
            (t) =>
              t.problem && (
                <div key={t.secret_name} className="banner" role="alert">
                  <div>
                    <div className="banner-title">
                      {t.secret_name} is stored, but runs{t.provider ? ` on ${FORGE_LABEL[t.provider]}` : ''} will not
                      receive it
                    </div>
                    <div className="small muted">{t.problem}</div>
                  </div>
                </div>
              ),
          )}
          {c.tokens.length > 0 && (
            <div className="table-wrap">
              <table>
                <thead>
                  <tr>
                    <th>Used for</th>
                    <th>Stored as</th>
                    <th>Status</th>
                    <th>Updated</th>
                    <th />
                  </tr>
                </thead>
                <tbody>
                  {c.tokens.map((t) => {
                    const status = gitTokenStatus(t, fallback)
                    return (
                      <tr key={t.secret_name}>
                        <td className="small">{t.provider ? FORGE_LABEL[t.provider] : 'Every forge (fallback)'}</td>
                        <td className="small">
                          <span className="mono">{t.secret_name}</span>
                          {t.kind && <span className="muted"> · {t.kind}</span>}
                        </td>
                        <td>
                          <span className={status.className}>{status.label}</span>
                        </td>
                        <td className="small muted">
                          <Time at={t.updated_at} />
                        </td>
                        <td className="nowrap">
                          <button className="sm ghost" onClick={() => setEditing({ provider: t.provider })}>
                            {t.configured ? 'Replace' : 'Set'}
                          </button>
                        </td>
                      </tr>
                    )
                  })}
                </tbody>
              </table>
            </div>
          )}
        </div>
      )}
      {editing && c && <GitTokenDialog initial={editing.provider} cred={c} onClose={() => setEditing(null)} />}
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
  const gitSecrets = new Set(useGitCredential().data?.tokens.map((t) => t.secret_name))
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
        <GitCredentialCard />
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
                      {gitSecrets.has(secret.name) && (
                        <>
                          {' '}
                          <span className="badge idle">git token</span>
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
