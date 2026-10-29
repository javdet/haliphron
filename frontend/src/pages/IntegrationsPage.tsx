import { useEffect, useId, useState } from 'react'
import { useChatIntegration, usePutChatIntegration, useRoles } from '../api/hooks'
import { Card, CopyButton, ErrorBanner, Field, Section, Spinner, Time } from '../components/ui'
import type { ChatIntegration, ChatPlatform, ChatTransport } from '../api/types'

/**
 * The chat integrations: a Mattermost bot and a Slack app whose messages become
 * runs. Both route through the same rules; what differs is how the backend
 * reaches the chat and which credentials it needs.
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

interface FormState {
  enabled: boolean
  transport: ChatTransport
  url: string
  role: string
  repo: string
  baseBranch: string
  maxPostRunes: string
  maxThreadBytes: string
  botToken: string
  webhookTokens: string
  appToken: string
}

const DEFAULT_POST_RUNES: Record<ChatPlatform, number> = { mattermost: 16383, slack: 4000 }

function fromIntegration(c: ChatIntegration): FormState {
  return {
    enabled: c.enabled,
    transport: c.transport,
    url: c.url,
    role: c.role,
    repo: c.repo,
    baseBranch: c.base_branch,
    maxPostRunes: String(c.max_post_runes || DEFAULT_POST_RUNES[c.platform]),
    maxThreadBytes: String(c.max_thread_bytes ?? 65536),
    botToken: '',
    webhookTokens: '',
    appToken: '',
  }
}

/** The request a form saves. Empty token fields keep what is stored. */
function toRequest(form: FormState, platform: ChatPlatform) {
  return {
    enabled: form.enabled,
    transport: form.transport,
    url: form.url.trim(),
    role: form.role.trim(),
    repo: form.repo.trim(),
    base_branch: form.baseBranch.trim(),
    max_post_runes: Number(form.maxPostRunes) || DEFAULT_POST_RUNES[platform],
    max_thread_bytes: Number(form.maxThreadBytes) || 0,
    ...(form.botToken.trim() ? { bot_token: form.botToken.trim() } : {}),
    ...(form.webhookTokens.trim() ? { webhook_tokens: form.webhookTokens.trim() } : {}),
    ...(form.appToken.trim() ? { app_token: form.appToken.trim() } : {}),
  }
}

/**
 * The form state of one integration, loaded once and then edited locally;
 * saving replaces it with what the backend stored.
 */
function useIntegrationForm(platform: ChatPlatform) {
  const current = useChatIntegration(platform)
  const put = usePutChatIntegration(platform)
  const [form, setForm] = useState<FormState | undefined>()
  const [saved, setSaved] = useState(false)

  useEffect(() => {
    if (current.data && !form) setForm(fromIntegration(current.data))
  }, [current.data, form])

  const set = (next: Partial<FormState>) => {
    setSaved(false)
    setForm((f) => (f ? { ...f, ...next } : f))
  }
  const save = () => {
    if (!form) return
    put.mutate(toRequest(form, platform), {
      onSuccess: (next) => {
        setForm(fromIntegration(next))
        setSaved(true)
      },
    })
  }
  return { current, put, form, set, save, saved }
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
  const { current, put, form, set, save, saved } = useIntegrationForm('mattermost')

  const c = current.data
  if (!c || !form) {
    return (
      <Card title="Mattermost">
        <ErrorBanner error={current.error} what="read the Mattermost integration" />
        {current.isLoading && <Spinner />}
      </Card>
    )
  }

  const managed = c.source === 'environment'
  const webhook = form.transport !== 'websocket'
  const webhookURL = `${window.location.origin}${c.webhook_path}`
  const badURL = urlProblem(form.url)

  const missing: string[] = []
  if (form.enabled) {
    if (!form.url.trim()) missing.push('the server address')
    if (!c.bot_token_configured && !form.botToken.trim()) missing.push('the bot token')
    if (webhook && !c.webhook_token_configured && !form.webhookTokens.trim()) missing.push('a webhook token')
  }
  const ready = !managed && !badURL && missing.length === 0

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
        <StatusBanners
          managed={managed}
          env="HALIPHRON_MATTERMOST_*"
          values="mattermost"
          error={put.error}
          what="save the Mattermost integration"
          saved={saved}
        />

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

          <RoleField form={form} set={set} />
          <RepliesHint />
          <AdvancedSection form={form} set={set} postRunesHint="The server’s MaxPostSize." />
        </fieldset>

        <SaveRow c={c} missing={missing} managed={managed} ready={ready} pending={put.isPending} onSave={save} />
      </div>
    </Card>
  )
}

type SetForm = (next: Partial<FormState>) => void

function StatusBanners(props: {
  managed: boolean
  env: string
  values: string
  error: unknown
  what: string
  saved: boolean
}) {
  return (
    <>
      {props.managed && (
        <div className="banner warn" role="status">
          <div>
            <div className="banner-title">Configured by the deployment</div>
            <div className="small muted">
              The backend’s environment (<span className="mono">{props.env}</span>, the chart’s{' '}
              <span className="mono">{props.values}</span> values) sets this integration, so it is read-only here.
              Unset <span className="mono">{props.values}.enabled</span> to manage it from this page.
            </div>
          </div>
        </div>
      )}
      <ErrorBanner error={props.error} what={props.what} />
      {props.saved && !props.error && (
        <div className="banner good" role="status">
          <div className="small">Saved. Every replica picks the change up within about 15 seconds.</div>
        </div>
      )}
    </>
  )
}

function RoleField({ form, set }: { form: FormState; set: SetForm }) {
  const roles = useRoles()
  // Both cards carry this field, and a datalist is found by its id.
  const listID = useId()
  const roleKnown = !form.role || (roles.data ?? []).some((r) => r.name === form.role)
  return (
    <Field
      label="Role"
      error={roleKnown ? undefined : `No role named ${form.role} exists yet; every request is refused until it does.`}
      hint="Every run the bot starts is admitted under this role, and anyone who can reach the bot can use it. The default, classificator, routes each request to the role that fits."
    >
      <input value={form.role} list={listID} placeholder="classificator" onChange={(e) => set({ role: e.target.value })} />
      <datalist id={listID}>
        {(roles.data ?? []).map((r) => (
          <option key={r.name} value={r.name} />
        ))}
      </datalist>
    </Field>
  )
}

function RepliesHint() {
  return (
    <div className="hint">
      Replies: the bot reacts with 👀 when the run starts, and posts the agent’s answer in the thread when it ends —
      the answer alone, with no run identifier, status or cost. A run that fails, or ends without an answer, gets one
      plain sentence. When the classificator hands the request to another agent, it posts one sentence saying who
      will handle it, and that agent’s answer follows. The details are on the run’s page.
    </div>
  )
}

function AdvancedSection({ form, set, postRunesHint }: { form: FormState; set: SetForm; postRunesHint: string }) {
  return (
    <Section title="Advanced" hint="repository and limits">
      <div className="grid-2">
        <Field label="Repository" hint="Empty runs without one — the usual choice for the classificator.">
          <input value={form.repo} placeholder="https://github.com/org/repo" onChange={(e) => set({ repo: e.target.value })} />
        </Field>
        <Field label="Base branch" hint="Needs a repository.">
          <input value={form.baseBranch} placeholder="main" onChange={(e) => set({ baseBranch: e.target.value })} />
        </Field>
      </div>
      <div className="grid-2">
        <Field label="Longest post (runes)" hint={postRunesHint}>
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
  )
}

function SaveRow(props: {
  c: ChatIntegration
  missing: string[]
  managed: boolean
  ready: boolean
  pending: boolean
  onSave: () => void
}) {
  const { c } = props
  return (
    <div className="row" style={{ justifyContent: 'space-between' }}>
      <span className="small muted">
        {props.missing.length > 0 ? `Needs ${props.missing.join(', ')}.` : null}
        {c.updated_at && (
          <>
            {' '}Last saved by {c.updated_by} <Time at={c.updated_at} />
          </>
        )}
      </span>
      {!props.managed && (
        <button className="primary" onClick={props.onSave} disabled={!props.ready || props.pending}>
          {props.pending ? 'Saving…' : 'Save'}
        </button>
      )}
    </div>
  )
}

function SlackCard() {
  const { current, put, form, set, save, saved } = useIntegrationForm('slack')

  const c = current.data
  if (!c || !form) {
    return (
      <Card title="Slack">
        <ErrorBanner error={current.error} what="read the Slack integration" />
        {current.isLoading && <Spinner />}
      </Card>
    )
  }

  const managed = c.source === 'environment'
  const badURL = urlProblem(form.url)
  const missing: string[] = []
  if (form.enabled) {
    if (!c.bot_token_configured && !form.botToken.trim()) missing.push('the bot token')
    if (!c.app_token_configured && !form.appToken.trim()) missing.push('the app-level token')
  }
  const ready = !managed && !badURL && missing.length === 0

  return (
    <Card
      title="Slack"
      actions={
        <span className={c.enabled ? 'badge ok' : 'badge'}>
          {c.enabled ? 'enabled' : c.source === 'none' ? 'not configured' : 'disabled'}
        </span>
      }
    >
      <div className="stack">
        <StatusBanners
          managed={managed}
          env="HALIPHRON_SLACK_*"
          values="slack"
          error={put.error}
          what="save the Slack integration"
          saved={saved}
        />

        <fieldset disabled={managed} className="stack" style={{ border: 0, padding: 0, margin: 0 }}>
          <label className={form.enabled ? 'check on' : 'check'}>
            <input type="checkbox" checked={form.enabled} onChange={(e) => set({ enabled: e.target.checked })} />
            Enabled
          </label>
          <div className="hint">
            The backend opens a Socket Mode connection to Slack, so nothing needs to reach the backend. The app sees
            mentions in the channels it was invited to, and direct messages.
          </div>

          <Field
            label={c.bot_token_configured ? 'Bot token (stored — leave empty to keep it)' : 'Bot token'}
            hint={`The Bot User OAuth Token, xoxb-…, from the app’s OAuth & Permissions page. Reads threads and posts replies. Stored encrypted as the secret ${c.bot_token_secret}.`}
          >
            <input
              type="password"
              autoComplete="off"
              value={form.botToken}
              placeholder={c.bot_token_configured ? '••••••••' : 'xoxb-…'}
              onChange={(e) => set({ botToken: e.target.value })}
            />
          </Field>
          <Field
            label={c.app_token_configured ? 'App-level token (stored — leave empty to keep it)' : 'App-level token'}
            hint={`An app-level token, xapp-…, with the connections:write scope, from the app’s Basic Information page. Opens the Socket Mode connection and nothing else. Stored encrypted as the secret ${c.app_token_secret}.`}
          >
            <input
              type="password"
              autoComplete="off"
              value={form.appToken}
              placeholder={c.app_token_configured ? '••••••••' : 'xapp-…'}
              onChange={(e) => set({ appToken: e.target.value })}
            />
          </Field>

          <RoleField form={form} set={set} />
          <RepliesHint />

          <AdvancedSection form={form} set={set} postRunesHint="Slack recommends at most 4000." />
          <Field
            label="Web API address"
            error={badURL}
            hint="Empty is https://slack.com/api. Set it only for another Slack deployment, such as GovSlack."
          >
            <input value={form.url} placeholder="https://slack.com/api" onChange={(e) => set({ url: e.target.value })} />
          </Field>
        </fieldset>

        <SaveRow c={c} missing={missing} managed={managed} ready={ready} pending={put.isPending} onSave={save} />
      </div>
    </Card>
  )
}

function HowItWorks() {
  return (
    <Card title="How a message becomes an answer">
      <ol className="small" style={{ margin: 0, paddingLeft: 18, lineHeight: 1.6 }}>
        <li>
          A message reaches the bot, by mention, direct message or webhook. The bot reacts with 👀, reads the whole
          thread the message was posted in, and starts a run under the role above, with the thread quoted as context,
          the message as the request, and a header naming the platform, the channel and the sender.
        </li>
        <li>
          The <span className="mono">classificator</span> role reads it, picks a role with{' '}
          <span className="mono">list_roles</span> and starts that agent with <span className="mono">run_agent</span>.
          Its one-sentence answer, saying who will handle the request, is posted in the thread. The backend ties the
          child to the same thread; the agent never handles chat identifiers.
        </li>
        <li>
          When the run that has the answer ends, the bot posts that agent’s final answer in the thread, as written.
          Agents never post in chat and need no chat credentials. A reply in the thread is a new message, and starts
          again from the whole thread, earlier answers included.
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
        <SlackCard />
        <HowItWorks />
      </div>
    </>
  )
}
