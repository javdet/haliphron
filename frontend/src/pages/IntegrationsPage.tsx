import { useEffect, useState } from 'react'
import { Link } from 'react-router-dom'
import { useMattermost, usePutMattermost, useRoles } from '../api/hooks'
import { Card, CopyButton, ErrorBanner, Field, Section, Spinner, Time } from '../components/ui'
import type { ChatIntegration, ChatReplyPolicy, ChatTransport } from '../api/types'

/**
 * The chat integrations. Today one: a Mattermost bot whose messages become runs.
 *
 * The tokens are write-only. The backend never returns them, so the form shows
 * whether one is stored and offers a field to replace it; leaving the field
 * empty keeps what is there.
 */

const TRANSPORT_LABEL: Record<ChatTransport, string> = {
  websocket: 'WebSocket',
  webhook: 'Outgoing webhook',
  both: 'Both',
}

const TRANSPORT_HINT: Record<ChatTransport, string> = {
  websocket:
    'The backend connects to Mattermost as the bot. Sees direct messages, private and public channels; nothing needs to reach the backend.',
  webhook:
    'Mattermost calls the backend through an outgoing webhook — the way n8n is usually fed. Public channels only, on a trigger word or every post of one channel.',
  both: 'Either way in. A post that arrives both ways starts one run.',
}

const POLICY_LABEL: Record<ChatReplyPolicy, string> = {
  if_unanswered: 'The agent answers; the bot steps in only if nobody did',
  always: 'The bot acknowledges and posts every result',
}

interface FormState {
  enabled: boolean
  transport: ChatTransport
  url: string
  role: string
  repo: string
  baseBranch: string
  runURL: string
  replyPolicy: ChatReplyPolicy
  maxPostRunes: string
  maxThreadBytes: string
  botToken: string
  webhookTokens: string
}

function fromIntegration(c: ChatIntegration): FormState {
  return {
    enabled: c.enabled,
    transport: c.transport,
    url: c.url,
    role: c.role,
    repo: c.repo,
    baseBranch: c.base_branch,
    runURL: c.run_url || `${window.location.origin}/runs/{id}`,
    replyPolicy: c.reply_policy,
    maxPostRunes: String(c.max_post_runes || 16383),
    maxThreadBytes: String(c.max_thread_bytes ?? 65536),
    botToken: '',
    webhookTokens: '',
  }
}

function urlProblem(raw: string): string | undefined {
  const v = raw.trim()
  if (!v) return undefined
  try {
    const u = new URL(v)
    if (u.protocol !== 'https:' && u.protocol !== 'http:') return 'Use the server’s http or https address.'
  } catch {
    return 'Not a URL. Use the address a browser opens, such as https://chat.example.com.'
  }
  return undefined
}

function MattermostCard() {
  const current = useMattermost()
  const put = usePutMattermost()
  const roles = useRoles()
  const [form, setForm] = useState<FormState | undefined>()
  const [saved, setSaved] = useState(false)

  useEffect(() => {
    if (current.data && !form) setForm(fromIntegration(current.data))
  }, [current.data, form])

  const c = current.data
  if (!c || !form) {
    return (
      <Card title="Mattermost">
        <ErrorBanner error={current.error} what="read the Mattermost integration" />
        {current.isLoading && <Spinner />}
      </Card>
    )
  }

  const set = (next: Partial<FormState>) => {
    setSaved(false)
    setForm({ ...form, ...next })
  }
  const managed = c.source === 'environment'
  const webhook = form.transport !== 'websocket'
  const webhookURL = `${window.location.origin}${c.webhook_path}`
  const badURL = urlProblem(form.url)
  const roleKnown = !form.role || (roles.data ?? []).some((r) => r.name === form.role)

  const missing: string[] = []
  if (form.enabled) {
    if (!form.url.trim()) missing.push('the server address')
    if (!c.bot_token_configured && !form.botToken.trim()) missing.push('the bot token')
    if (webhook && !c.webhook_token_configured && !form.webhookTokens.trim()) missing.push('a webhook token')
  }
  const ready = !managed && !badURL && missing.length === 0

  const save = () =>
    put.mutate(
      {
        enabled: form.enabled,
        transport: form.transport,
        url: form.url.trim(),
        role: form.role.trim(),
        repo: form.repo.trim(),
        base_branch: form.baseBranch.trim(),
        run_url: form.runURL.trim(),
        reply_policy: form.replyPolicy,
        max_post_runes: Number(form.maxPostRunes) || 16383,
        max_thread_bytes: Number(form.maxThreadBytes) || 0,
        ...(form.botToken.trim() ? { bot_token: form.botToken.trim() } : {}),
        ...(form.webhookTokens.trim() ? { webhook_tokens: form.webhookTokens.trim() } : {}),
      },
      {
        onSuccess: (next) => {
          setForm(fromIntegration(next))
          setSaved(true)
        },
      },
    )

  return (
    <Card
      title="Mattermost"
      actions={
        <span className={c.enabled ? 'badge ok' : 'badge'}>
          {c.enabled ? 'enabled' : c.source === 'none' ? 'not configured' : 'disabled'}
        </span>
      }
    >
      <div className="stack">
        {managed && (
          <div className="banner warn" role="status">
            <div>
              <div className="banner-title">Configured by the deployment</div>
              <div className="small muted">
                The backend’s environment (<span className="mono">HALIPHRON_MATTERMOST_*</span>, the chart’s{' '}
                <span className="mono">mattermost</span> values) sets this integration, so it is read-only here.
                Unset <span className="mono">mattermost.enabled</span> to manage it from this page.
              </div>
            </div>
          </div>
        )}
        <ErrorBanner error={put.error} what="save the Mattermost integration" />
        {saved && !put.error && (
          <div className="banner good" role="status">
            <div className="small">Saved. Every replica picks the change up within about 15 seconds.</div>
          </div>
        )}

        <fieldset disabled={managed} className="stack" style={{ border: 0, padding: 0, margin: 0 }}>
          <label className={form.enabled ? 'check on' : 'check'}>
            <input type="checkbox" checked={form.enabled} onChange={(e) => set({ enabled: e.target.checked })} />
            Enabled
          </label>

          <Field
            label="Server"
            error={badURL}
            hint="The address a browser opens. The bot account’s token below must be valid on it."
          >
            <input value={form.url} placeholder="https://chat.example.com" onChange={(e) => set({ url: e.target.value })} />
          </Field>

          <Field
            label={c.bot_token_configured ? 'Bot token (stored — leave empty to keep it)' : 'Bot token'}
            hint={`The bot account’s access token, from System Console → Integrations → Bot Accounts. It may be the bot n8n already uses. Stored encrypted as the secret ${c.bot_token_secret}.`}
          >
            <input
              type="password"
              autoComplete="off"
              value={form.botToken}
              placeholder={c.bot_token_configured ? '••••••••' : 'paste the token'}
              onChange={(e) => set({ botToken: e.target.value })}
            />
          </Field>

          <Field label="How messages arrive" hint={TRANSPORT_HINT[form.transport]}>
            <div className="checks">
              {(Object.keys(TRANSPORT_LABEL) as ChatTransport[]).map((t) => (
                <label key={t} className={form.transport === t ? 'check on' : 'check'}>
                  <input type="radio" checked={form.transport === t} onChange={() => set({ transport: t })} />
                  {TRANSPORT_LABEL[t]}
                </label>
              ))}
            </div>
          </Field>

          {webhook && (
            <>
              <Field
                label="Webhook URL"
                hint="Paste this as the outgoing webhook’s callback URL in Mattermost (Integrations → Outgoing Webhooks). Content type: either."
              >
                <div className="row" style={{ gap: 6, flexWrap: 'nowrap' }}>
                  <input readOnly value={webhookURL} className="mono" />
                  <CopyButton value={webhookURL} />
                </div>
              </Field>
              <Field
                label={c.webhook_token_configured ? 'Webhook tokens (stored — leave empty to keep them)' : 'Webhook tokens'}
                hint={`The token Mattermost shows after the webhook is saved, one per line if several webhooks feed this bot. A delivery with any other token is refused. Stored encrypted as ${c.webhook_token_secret}.`}
              >
                <textarea
                  rows={2}
                  autoComplete="off"
                  spellCheck={false}
                  value={form.webhookTokens}
                  placeholder={c.webhook_token_configured ? '••••••••' : 'one token per line'}
                  onChange={(e) => set({ webhookTokens: e.target.value })}
                />
              </Field>
            </>
          )}

          <Field
            label="Role"
            error={roleKnown ? undefined : `No role named ${form.role} exists yet; every request is refused until it does.`}
            hint="Every run the bot starts is admitted under this role, and anyone who can reach the bot can use it. The default, classificator, routes each request to the role that fits."
          >
            <input
              value={form.role}
              list="integration-roles"
              placeholder="classificator"
              onChange={(e) => set({ role: e.target.value })}
            />
            <datalist id="integration-roles">
              {(roles.data ?? []).map((r) => (
                <option key={r.name} value={r.name} />
              ))}
            </datalist>
          </Field>

          <Field label="Replies">
            <div className="checks">
              {(Object.keys(POLICY_LABEL) as ChatReplyPolicy[]).map((p) => (
                <label key={p} className={form.replyPolicy === p ? 'check on' : 'check'}>
                  <input type="radio" checked={form.replyPolicy === p} onChange={() => set({ replyPolicy: p })} />
                  {POLICY_LABEL[p]}
                </label>
              ))}
            </div>
          </Field>
          <div className="hint">
            {form.replyPolicy === 'if_unanswered' ? (
              <>
                The bot reacts with 👀 and stays quiet; the agent replies in the thread through its own Mattermost tools.
                When a run fails, or ends without anything answering the thread, the bot posts how it ended.
              </>
            ) : (
              <>The bot replies with the run’s identifier, and again with its result when it ends.</>
            )}
          </div>

          <Section title="Advanced" hint="repository, links and limits">
            <div className="grid-2">
              <Field label="Repository" hint="Empty runs without one — the usual choice for the classificator.">
                <input value={form.repo} placeholder="https://github.com/org/repo" onChange={(e) => set({ repo: e.target.value })} />
              </Field>
              <Field label="Base branch" hint="Needs a repository.">
                <input value={form.baseBranch} placeholder="main" onChange={(e) => set({ baseBranch: e.target.value })} />
              </Field>
            </div>
            <Field label="Run link" hint="Where a reply links to a run, with {id} for its identifier. Empty for none.">
              <input value={form.runURL} onChange={(e) => set({ runURL: e.target.value })} />
            </Field>
            <div className="grid-2">
              <Field label="Longest post (runes)" hint="The server’s MaxPostSize.">
                <input
                  value={form.maxPostRunes}
                  inputMode="numeric"
                  onChange={(e) => set({ maxPostRunes: e.target.value.replace(/[^0-9]/g, '') })}
                />
              </Field>
              <Field label="Thread context (bytes)" hint="How much of a thread is quoted into the prompt.">
                <input
                  value={form.maxThreadBytes}
                  inputMode="numeric"
                  onChange={(e) => set({ maxThreadBytes: e.target.value.replace(/[^0-9]/g, '') })}
                />
              </Field>
            </div>
          </Section>
        </fieldset>

        <div className="row" style={{ justifyContent: 'space-between' }}>
          <span className="small muted">
            {missing.length > 0 ? `Needs ${missing.join(', ')}.` : null}
            {c.updated_at && (
              <>
                {' '}Last saved by {c.updated_by} <Time at={c.updated_at} />
              </>
            )}
          </span>
          {!managed && (
            <button className="primary" onClick={save} disabled={!ready || put.isPending}>
              {put.isPending ? 'Saving…' : 'Save'}
            </button>
          )}
        </div>
      </div>
    </Card>
  )
}

function HowItWorks() {
  return (
    <Card title="How a message becomes an answer">
      <ol className="small" style={{ margin: 0, paddingLeft: 18, lineHeight: 1.6 }}>
        <li>
          A message reaches the bot, by mention, direct message or webhook. It starts a run under the role above, with
          the message as the prompt and a header naming the channel, the message, the thread and the sender.
        </li>
        <li>
          The <span className="mono">classificator</span> role reads it, picks a role with{' '}
          <span className="mono">list_roles</span> and starts that agent with <span className="mono">run_agent</span>.
          The child is told the same chat header.
        </li>
        <li>
          The agent answers in the thread through a Mattermost MCP server that a plugin brings. Give the plugin its
          token as a <em>secret variable</em> on the role (for example{' '}
          <span className="mono">MATTERMOST_TOKEN</span> from the secret{' '}
          <span className="mono">mattermost-agent-token</span>), and declare the plugin in the role’s{' '}
          <span className="mono">settings.json</span> — see <Link to="/roles">Roles</Link>.
        </li>
        <li>
          Use a separate, narrow bot account for that token: the agent can read its environment, and the agent reads
          untrusted text.
        </li>
      </ol>
    </Card>
  )
}

export function IntegrationsPage() {
  return (
    <>
      <header className="topbar">
        <div className="topbar-title">
          <h1>Integrations</h1>
        </div>
      </header>
      <div className="content">
        <MattermostCard />
        <HowItWorks />
      </div>
    </>
  )
}
