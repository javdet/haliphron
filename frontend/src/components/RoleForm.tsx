import { useMemo, useState } from 'react'
import { Field, Rows, Section, StringList } from './ui'
import {
  AGENT_TYPES,
  PERMISSION_MODES,
  type McpServer,
  type McpTransport,
  type PluginMarketplace,
  type RoleSpec,
} from '../api/types'

/**
 * The role editor.
 *
 * This replaced a JSON textarea, and the argument for that textarea is worth
 * answering rather than deleting: a form with a field per key is a second,
 * private schema that goes out of date silently, while a document editor goes
 * out of date loudly, at the backend's own validation.
 *
 * The answer is that this form does not own the document. `split` peels off the
 * keys it can render and keeps the rest verbatim; `merge` puts them back. A key
 * this build has never heard of survives an edit here, which is the same
 * forward-compatibility rule the backend applies when it ignores what it does
 * not understand. The raw editor is still one click away for everything else.
 */

/** The keys this form renders. Everything else is carried through untouched. */
const KNOWN = [
  'agent', 'model', 'image', 'permissionMode', 'maxTurns', 'systemPrompt', 'env',
  'mcpServers', 'toolPolicy', 'plugins', 'configFiles', 'clusterSelector',
] as const

export interface RoleFormState {
  agent: string
  model: string
  image: string
  permissionMode: string
  maxTurns: string
  systemPrompt: string
  allow: string[]
  deny: string[]
  marketplaces: PluginMarketplace[]
  plugins: string[]
  trustRepositorySources: boolean
  mcpServers: McpServer[]
  env: { name: string; value: string }[]
  configFiles: { name: string; body: string }[]
  /**
   * Everything this build does not render, kept so it survives a save: the
   * top-level keys, and the nested ones inside the two objects the form
   * rebuilds rather than edits in place.
   */
  rest: Record<string, unknown>
  restToolPolicy: Record<string, unknown>
  restPlugins: Record<string, unknown>
}

/** The sub-keys the form renders, per nested object. */
const KNOWN_TOOL_POLICY = ['allow', 'deny']
const KNOWN_PLUGINS = ['marketplaces', 'enabled', 'trustRepositorySources']

function others(value: unknown, known: string[]): Record<string, unknown> {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) return {}
  return Object.fromEntries(Object.entries(value).filter(([k]) => !known.includes(k)))
}

export function split(spec: RoleSpec | undefined): RoleFormState {
  const s = spec ?? {}
  const rest: Record<string, unknown> = {}
  for (const [key, value] of Object.entries(s)) {
    if (!(KNOWN as readonly string[]).includes(key)) rest[key] = value
  }
  return {
    agent: typeof s.agent === 'string' ? s.agent : '',
    model: s.model ?? '',
    image: s.image ?? '',
    permissionMode: s.permissionMode ?? '',
    maxTurns: s.maxTurns ? String(s.maxTurns) : '',
    systemPrompt: s.systemPrompt ?? '',
    allow: s.toolPolicy?.allow ?? [],
    deny: s.toolPolicy?.deny ?? [],
    marketplaces: s.plugins?.marketplaces ?? [],
    plugins: s.plugins?.enabled ?? [],
    // Unset means true. The default is the product decision and the form must
    // not turn "the role never said" into "the role said no".
    trustRepositorySources: s.plugins?.trustRepositorySources ?? true,
    mcpServers: s.mcpServers ?? [],
    env: s.env ?? [],
    configFiles: Object.entries(s.configFiles ?? {}).map(([name, body]) => ({ name, body })),
    rest,
    restToolPolicy: others(s.toolPolicy, KNOWN_TOOL_POLICY),
    restPlugins: others(s.plugins, KNOWN_PLUGINS),
  }
}

export function merge(form: RoleFormState): RoleSpec {
  const spec: RoleSpec = { ...form.rest }

  const set = <K extends keyof RoleSpec>(key: K, value: RoleSpec[K], keep: boolean) => {
    if (keep) spec[key] = value
    else delete spec[key]
  }

  set('agent', form.agent, !!form.agent)
  set('model', form.model.trim(), !!form.model.trim())
  set('image', form.image.trim(), !!form.image.trim())
  set('permissionMode', form.permissionMode as RoleSpec['permissionMode'], !!form.permissionMode)

  const turns = Number(form.maxTurns)
  set('maxTurns', turns, !!form.maxTurns && Number.isFinite(turns) && turns > 0)

  // Sent as written, not trimmed: whitespace inside a prompt can be meaningful,
  // and the pod trims the ends itself. Omitted when there is nothing but
  // whitespace, so an emptied field clears the prompt rather than storing "".
  set('systemPrompt', form.systemPrompt, !!form.systemPrompt.trim())

  // Omitted rather than sent empty. An absent toolPolicy means the role adds no
  // narrowing of its own and the installation's ceiling applies — which is what
  // "allow everything" means here. Sending {allow: [], deny: []} would say the
  // same thing today and is one backend change away from meaning "allow
  // nothing", so the form does not say it.
  const policy = {
    ...form.restToolPolicy,
    ...(form.allow.length ? { allow: form.allow } : {}),
    ...(form.deny.length ? { deny: form.deny } : {}),
  }
  set('toolPolicy', policy, Object.keys(policy).length > 0)

  const marketplaces = form.marketplaces.filter((m) => m.url.trim())
  const plugins = {
    ...form.restPlugins,
    ...(marketplaces.length ? { marketplaces } : {}),
    ...(form.plugins.length ? { enabled: form.plugins } : {}),
    // Only sent when it is not the default, so a role that never had an opinion
    // does not acquire one by being opened in this form.
    ...(form.trustRepositorySources ? {} : { trustRepositorySources: false }),
  }
  set('plugins', plugins, Object.keys(plugins).length > 0)

  const servers = form.mcpServers.filter((m) => m.name.trim())
  set('mcpServers', servers, servers.length > 0)

  const env = form.env.filter((e) => e.name.trim())
  set('env', env, env.length > 0)

  const files = form.configFiles.filter((f) => f.name.trim())
  set(
    'configFiles',
    Object.fromEntries(files.map((f) => [f.name.trim(), f.body])),
    files.length > 0,
  )

  return spec
}

/** Mirrors api/run/v1.MaxRoleSystemPromptBytes. Bytes, not characters. */
const MAX_SYSTEM_PROMPT_BYTES = 32 * 1024

function systemPromptError(prompt: string): string | undefined {
  const bytes = new TextEncoder().encode(prompt).length
  if (bytes <= MAX_SYSTEM_PROMPT_BYTES) return undefined
  return `${bytes} bytes; the limit is ${MAX_SYSTEM_PROMPT_BYTES}`
}

/** Mirrors api/run/v1.PluginID, so a typo is caught before the round trip. */
function badPluginID(entry: string, declared: string[]): string | undefined {
  const at = entry.indexOf('@')
  if (at < 0) return 'name the marketplace too: plugin@marketplace'
  const plugin = entry.slice(0, at)
  const marketplace = entry.slice(at + 1)
  const name = /^[A-Za-z0-9][A-Za-z0-9._-]*$/
  if (!name.test(plugin)) return `${plugin || '(empty)'} is not a usable plugin name`
  if (!name.test(marketplace)) return `${marketplace || '(empty)'} is not a usable marketplace name`
  // Unconditional, like the backend's own check: the pod resolves one source
  // and that source is the only one that speaks, so a plugin whose marketplace
  // this role does not name could never be installed.
  if (!declared.includes(marketplace)) {
    return `no marketplace named ${marketplace} is declared above`
  }
  return undefined
}

export function RoleForm({
  form,
  set,
}: {
  form: RoleFormState
  set: (next: Partial<RoleFormState>) => void
}) {
  const declared = useMemo(
    () => form.marketplaces.map((m) => m.name?.trim()).filter((n): n is string => !!n),
    [form.marketplaces],
  )

  return (
    <div className="stack">
      <div className="grid-3">
        <Field label="Agent" hint="Which CLI runs. A run may override it.">
          <select value={form.agent} onChange={(e) => set({ agent: e.target.value })}>
            <option value="">Platform default</option>
            {AGENT_TYPES.map((a) => (
              <option key={a} value={a}>{a}</option>
            ))}
          </select>
        </Field>
        <Field label="Model" hint="provider/model, or blank for the default.">
          <input
            value={form.model}
            placeholder="anthropic/claude-opus-5"
            onChange={(e) => set({ model: e.target.value })}
          />
        </Field>
        <Field label="Max turns" hint="Blank for the platform's limit.">
          <input
            value={form.maxTurns}
            inputMode="numeric"
            placeholder="150"
            onChange={(e) => set({ maxTurns: e.target.value.replace(/[^0-9]/g, '') })}
          />
        </Field>
      </div>

      <Field label="Permission mode" hint="What the agent may do without asking.">
        <select
          value={form.permissionMode}
          onChange={(e) => set({ permissionMode: e.target.value })}
        >
          {PERMISSION_MODES.map((m) => (
            <option key={m.value} value={m.value}>{m.label}</option>
          ))}
        </select>
      </Field>

      <Field
        label="System prompt"
        error={systemPromptError(form.systemPrompt)}
        hint="Added after the agent's built-in system prompt and haliphron's own instructions — it extends them, never replaces them. Blank for none."
      >
        <textarea
          rows={6}
          value={form.systemPrompt}
          placeholder="You are working in someone else's repository…"
          onChange={(e) => set({ systemPrompt: e.target.value })}
        />
      </Field>

      <Section title="Plugins" hint="marketplaces to fetch, and what to install from them" open>
        <Field
          label="Marketplaces"
          hint="owner/repo or an https URL. Private repositories are fetched with the run's own git credential."
        >
          <Rows
            value={form.marketplaces}
            onChange={(marketplaces) => set({ marketplaces })}
            empty={(): PluginMarketplace => ({ name: '', url: '', ref: '' })}
            addLabel="Add a marketplace"
          >
            {(row, setRow) => (
              <>
                <input
                  value={row.url}
                  placeholder="playneta/claude-plugin"
                  aria-label="Repository"
                  onChange={(e) => setRow({ ...row, url: e.target.value })}
                />
                <input
                  value={row.name ?? ''}
                  placeholder="name, e.g. playneta"
                  aria-label="Marketplace name"
                  onChange={(e) => setRow({ ...row, name: e.target.value })}
                />
                <input
                  value={row.ref ?? ''}
                  placeholder="branch or tag"
                  aria-label="Ref"
                  onChange={(e) => setRow({ ...row, ref: e.target.value })}
                />
              </>
            )}
          </Rows>
        </Field>

        <Field
          label="Plugins to install"
          hint={
            declared.length
              ? `plugin@marketplace — the marketplace half is one of: ${declared.join(', ')}.`
              : 'plugin@marketplace. Name a marketplace above first: a plugin is installed from a marketplace this role declares.'
          }
        >
          <StringList
            value={form.plugins}
            onChange={(plugins) => set({ plugins })}
            placeholder="playneta-infra-coder@playneta"
            validate={(entry) => badPluginID(entry, declared)}
          />
        </Field>

        <label className={form.trustRepositorySources ? 'check on' : 'check'}>
          <input
            type="checkbox"
            checked={form.trustRepositorySources}
            onChange={(e) => set({ trustRepositorySources: e.target.checked })}
          />
          Let a repository choose its own plugins
        </label>
        <span className="hint">
          When on, a repository's own <span className="mono">.claude/settings.&lt;role&gt;.json</span>{' '}
          overrides the list above. That is convenient, and it lets a repository decide which code
          runs beside the agent — turn it off for roles that run against code you do not control.
        </span>
      </Section>

      <Section title="Tools" hint="empty means everything the platform allows" open>
        <div className="grid-2">
          <Field label="Allow" hint="Empty = no narrowing by this role.">
            <StringList
              value={form.allow}
              onChange={(allow) => set({ allow })}
              placeholder="Read, Edit, Bash(git:*)…"
            />
          </Field>
          <Field label="Deny" hint="Applied on top of the platform's own deny list.">
            <StringList
              value={form.deny}
              onChange={(deny) => set({ deny })}
              placeholder="WebFetch, Bash(curl:*)…"
            />
          </Field>
        </div>
      </Section>

      <Section title="Advanced" hint="MCP servers, variables, images and files">
        <Field label="Image" hint="Blank for the platform's agent image.">
          <input
            value={form.image}
            placeholder="registry.example.com/agent:1.2.3"
            onChange={(e) => set({ image: e.target.value })}
          />
        </Field>

        <Field
          label="MCP servers"
          hint="Extra servers. The haliphron server is always present — it is how an agent starts child runs."
        >
          <Rows
            value={form.mcpServers}
            onChange={(mcpServers) => set({ mcpServers })}
            empty={(): McpServer => ({ name: '', transport: 'http' })}
            addLabel="Add a server"
          >
            {(row, setRow) => (
              <>
                <input
                  value={row.name}
                  placeholder="name"
                  aria-label="Server name"
                  onChange={(e) => setRow({ ...row, name: e.target.value })}
                />
                <select
                  value={row.transport}
                  aria-label="Transport"
                  onChange={(e) => setRow({ ...row, transport: e.target.value as McpTransport })}
                >
                  <option value="http">http</option>
                  <option value="sse">sse</option>
                  <option value="stdio">stdio</option>
                </select>
                {row.transport === 'stdio' ? (
                  <input
                    value={row.command ?? ''}
                    placeholder="command"
                    aria-label="Command"
                    onChange={(e) => setRow({ ...row, command: e.target.value })}
                  />
                ) : (
                  <input
                    value={row.url ?? ''}
                    placeholder="https://…"
                    aria-label="URL"
                    onChange={(e) => setRow({ ...row, url: e.target.value })}
                  />
                )}
              </>
            )}
          </Rows>
        </Field>

        <Field label="Environment" hint="Non-secret variables only; secrets arrive from the per-run Secret.">
          <Rows
            value={form.env}
            onChange={(env) => set({ env })}
            empty={() => ({ name: '', value: '' })}
            addLabel="Add a variable"
          >
            {(row, setRow) => (
              <>
                <input
                  value={row.name}
                  placeholder="NAME"
                  aria-label="Variable name"
                  onChange={(e) => setRow({ ...row, name: e.target.value })}
                />
                <input
                  value={row.value}
                  placeholder="value"
                  aria-label="Variable value"
                  onChange={(e) => setRow({ ...row, value: e.target.value })}
                />
              </>
            )}
          </Rows>
        </Field>

        <Field
          label="Config files"
          hint="Mounted flat into /haliphron/role. A flat file name — settings.coder.json — not a path."
        >
          <Rows
            value={form.configFiles}
            onChange={(configFiles) => set({ configFiles })}
            empty={() => ({ name: '', body: '' })}
            addLabel="Add a file"
          >
            {(row, setRow) => (
              <>
                <input
                  value={row.name}
                  placeholder="settings.coder.json"
                  aria-label="File name"
                  onChange={(e) => setRow({ ...row, name: e.target.value })}
                />
                <textarea
                  rows={3}
                  value={row.body}
                  spellCheck={false}
                  aria-label="File contents"
                  onChange={(e) => setRow({ ...row, body: e.target.value })}
                />
              </>
            )}
          </Rows>
        </Field>
      </Section>
    </div>
  )
}

/** The raw editor, kept for whatever the form does not render. */
export function RawSpecEditor({
  text,
  onChange,
  error,
}: {
  text: string
  onChange: (next: string) => void
  error?: string
}) {
  return (
    <Field label="Spec" error={error} hint="Replaced whole on save.">
      <textarea rows={18} value={text} spellCheck={false} onChange={(e) => onChange(e.target.value)} />
    </Field>
  )
}

export function useRoleForm(spec: RoleSpec | undefined) {
  const [form, setForm] = useState<RoleFormState>(() => split(spec))
  return {
    form,
    set: (next: Partial<RoleFormState>) => setForm((f) => ({ ...f, ...next })),
    replace: (next: RoleFormState) => setForm(next),
  }
}
