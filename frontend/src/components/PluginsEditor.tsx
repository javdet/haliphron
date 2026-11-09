import { useEffect, useMemo, useState } from 'react'
import { useMarketplaceInspection } from '../api/hooks'
import type { MarketplaceInspection, PluginMarketplace } from '../api/types'
import { Spinner } from './ui'

/**
 * The role's marketplaces and the plugins installed from them.
 *
 * The document keeps its shape — `marketplaces[]` beside `enabled[]` as
 * `plugin@marketplace` — and the form stops asking the operator to spell it.
 * A repository is typed first; the backend reads the catalogue's manifest from
 * it and the name it declares fills in the name field, and the plugins it
 * offers become checkboxes that write the `@suffix` themselves.
 *
 * Reading the manifest is advisory. When it fails nothing is filled in, and the
 * name and the plugins are typed by hand exactly as before. A name already
 * saved in a role is never replaced by the one read: only a row whose name this
 * form filled in itself (`autoName`) follows the catalogue.
 */

/** A marketplace as the form holds it. The two extra keys never leave the form. */
export type MarketplaceRow = PluginMarketplace & {
  /** The name was filled in from the catalogue, not typed or saved. */
  autoName?: boolean
  /**
   * The name the row's plugins still carry while the field is empty mid-edit,
   * so that clearing the field and typing a new name moves them rather than
   * stranding them under the old one.
   */
  lastName?: string
}

export interface PluginsState {
  marketplaces: MarketplaceRow[]
  plugins: string[]
}

type Update = (fn: (s: PluginsState) => Partial<PluginsState>) => void

/** The row as it is saved: trimmed, empty optionals dropped, form-only keys gone. */
export function marketplaceOut(row: MarketplaceRow): PluginMarketplace {
  const out: MarketplaceRow = { ...row, url: row.url.trim() }
  delete out.autoName
  delete out.lastName
  const name = row.name?.trim()
  const ref = row.ref?.trim()
  if (name) out.name = name
  else delete out.name
  if (ref) out.ref = ref
  else delete out.ref
  return out
}

/** A row loaded from a saved role. A name it already has is the operator's. */
export function marketplaceIn(m: PluginMarketplace): MarketplaceRow {
  return { ...m, autoName: !m.name?.trim() }
}

// Mirrors of the allow-lists in api/run/v1/plugins.go, so a source that could
// not be saved is neither sent to the backend nor read.
const NAME = /^[A-Za-z0-9][A-Za-z0-9._-]*$/
const REPO = /^[A-Za-z0-9][A-Za-z0-9._-]*\/[A-Za-z0-9][A-Za-z0-9._-]*$/
const REF = /^[A-Za-z0-9][A-Za-z0-9._/-]*$/
const HOST = /^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?(:[0-9]{1,5})?$/

function sourceError(url: string): string | undefined {
  if (!url) return undefined
  if (/\s/.test(url)) return 'a repository address has no spaces in it'
  if (REPO.test(url)) return undefined
  if (url.startsWith('https://')) {
    const host = url.slice('https://'.length).split('/')[0]
    return HOST.test(host) ? undefined : `${url} names no usable host`
  }
  return 'owner/repo or an https:// URL — ssh is not supported, the pod has an https token and no key'
}

function refError(ref: string): string | undefined {
  if (!ref) return undefined
  if (ref.includes('..') || !REF.test(ref)) return `${ref} is not a usable branch or tag`
  return undefined
}

const marketplaceOf = (id: string) => id.slice(id.lastIndexOf('@') + 1)
const pluginOf = (id: string) => id.slice(0, id.lastIndexOf('@'))

/** The name a row's plugins are filed under, including while it is being retyped. */
const keyOf = (row: MarketplaceRow) => row.name?.trim() || row.lastName || ''

/**
 * Replaces row `index`, moving its plugins to the new name with it.
 *
 * Not when another row answers to the old name too: two rows sharing a name for
 * a keystroke would otherwise each take the other's plugins.
 */
function replaceRow(s: PluginsState, index: number, next: MarketplaceRow): Partial<PluginsState> {
  const before = s.marketplaces[index]
  if (!before) return {}
  const from = keyOf(before)
  const to = next.name?.trim() ?? ''
  const row = { ...next }
  delete row.lastName

  const shared = s.marketplaces.some((m, i) => i !== index && keyOf(m) === from)
  if (!from || from === to || shared) {
    return { marketplaces: s.marketplaces.map((m, i) => (i === index ? row : m)) }
  }
  if (!to) {
    row.lastName = from
    return { marketplaces: s.marketplaces.map((m, i) => (i === index ? row : m)) }
  }
  return {
    marketplaces: s.marketplaces.map((m, i) => (i === index ? row : m)),
    plugins: s.plugins.map((id) => (marketplaceOf(id) === from ? `${pluginOf(id)}@${to}` : id)),
  }
}

function useDebounced<T>(value: T, ms: number): T {
  const [settled, setSettled] = useState(value)
  useEffect(() => {
    const t = setTimeout(() => setSettled(value), ms)
    return () => clearTimeout(t)
  }, [value, ms])
  return settled
}

export function PluginsEditor({ state, update }: { state: PluginsState; update: Update }) {
  const keys = state.marketplaces.map(keyOf)
  const orphans = state.plugins.filter((id) => !keys.includes(marketplaceOf(id)))

  const remove = (index: number) =>
    update((s) => {
      const key = keyOf(s.marketplaces[index] ?? { url: '' })
      const marketplaces = s.marketplaces.filter((_, i) => i !== index)
      const stillNamed = marketplaces.some((m) => keyOf(m) === key)
      return {
        marketplaces,
        // A plugin whose marketplace is gone could never be installed, and the
        // backend refuses a role that says otherwise.
        plugins: key && !stillNamed ? s.plugins.filter((id) => marketplaceOf(id) !== key) : s.plugins,
      }
    })

  return (
    <div className="stack" style={{ gap: 10 }}>
      {state.marketplaces.map((row, index) => (
        <MarketplaceCard
          key={index}
          row={row}
          index={index}
          plugins={state.plugins}
          duplicate={!!row.name?.trim() && keys.filter((k) => k === row.name?.trim()).length > 1}
          update={update}
          onRemove={() => remove(index)}
        />
      ))}
      <div>
        <button
          type="button"
          className="ghost sm"
          onClick={() =>
            update((s) => ({
              marketplaces: [...s.marketplaces, { url: '', name: '', ref: '', autoName: true }],
            }))
          }
        >
          Add a marketplace
        </button>
      </div>

      {orphans.length > 0 && (
        <div className="field">
          <label>Plugins from a marketplace not declared above</label>
          <div className="chips">
            {orphans.map((id) => (
              <span key={id} className="chip">
                <span className="mono">{id}</span>
                <button
                  type="button"
                  className="chip-x"
                  aria-label={`Remove ${id}`}
                  onClick={() => update((s) => ({ plugins: s.plugins.filter((p) => p !== id) }))}
                >
                  ✕
                </button>
              </span>
            ))}
          </div>
          <span className="err">
            A plugin is installed from a marketplace this role declares. Add a marketplace with
            that name, or remove these; the role is refused as it is.
          </span>
        </div>
      )}
    </div>
  )
}

function MarketplaceCard({
  row,
  index,
  plugins,
  duplicate,
  update,
  onRemove,
}: {
  row: MarketplaceRow
  index: number
  plugins: string[]
  duplicate: boolean
  update: Update
  onRemove: () => void
}) {
  const url = row.url.trim()
  const ref = (row.ref ?? '').trim()
  const urlError = sourceError(url)
  const badRef = refError(ref)

  const settledUrl = useDebounced(url, 500)
  const settledRef = useDebounced(ref, 500)
  const settled = settledUrl === url && settledRef === ref
  const inspectable = !!url && !urlError && !badRef
  const inspection = useMarketplaceInspection(settledUrl, settledRef, inspectable && settled)
  // An answer for what the fields say now, and not for what they said half a
  // second ago.
  const current: MarketplaceInspection | undefined =
    inspectable && settled ? inspection.data : undefined
  const found = current?.found ? current : undefined
  const checking = inspectable && (!settled || inspection.isFetching)

  // Follow the catalogue, for a row whose name this form filled in itself.
  const foundName = found?.name
  useEffect(() => {
    if (!foundName || !row.autoName || row.name === foundName) return
    update((s) => {
      const cur = s.marketplaces[index]
      if (!cur || cur.url.trim() !== url || !cur.autoName) return {}
      return replaceRow(s, index, { ...cur, name: foundName, autoName: true })
    })
  }, [foundName, row.autoName, row.name, index, url, update])

  const name = row.name?.trim() ?? ''
  const key = keyOf(row)
  const mine = plugins.filter((id) => marketplaceOf(id) === key)
  const offered = found?.plugins ?? []
  const listed = new Set(offered.map((p) => p.name))
  const unlisted = mine.filter((id) => !found || !listed.has(pluginOf(id)))

  const setRow = (fn: (cur: MarketplaceRow) => MarketplaceRow) =>
    update((s) => {
      const cur = s.marketplaces[index]
      return cur ? replaceRow(s, index, fn(cur)) : {}
    })

  const toggle = (plugin: string, on: boolean) =>
    update((s) => {
      const id = `${plugin}@${name}`
      const without = s.plugins.filter((p) => p !== id)
      return { plugins: on ? [...without, id] : without }
    })

  return (
    <div className="mkt">
      <div className="mkt-head">
        <div className="field">
          <label>Repository</label>
          <input
            value={row.url}
            placeholder="owner/repo or https://…"
            aria-label="Marketplace repository"
            onChange={(e) => {
              const v = e.target.value
              setRow((cur) => ({ ...cur, url: v }))
            }}
          />
        </div>
        <div className="field">
          <label>Branch or tag</label>
          <input
            value={row.ref ?? ''}
            placeholder="default branch"
            aria-label="Marketplace ref"
            onChange={(e) => {
              const v = e.target.value
              setRow((cur) => ({ ...cur, ref: v }))
            }}
          />
        </div>
        <button type="button" className="ghost sm" aria-label="Remove marketplace" onClick={onRemove}>
          ✕
        </button>
      </div>

      <InspectionStatus
        urlError={urlError}
        refError={badRef}
        checking={checking}
        error={inspectable && settled ? inspection.error : null}
        result={current}
      />

      <div className="field">
        <label>Marketplace name</label>
        <input
          value={row.name ?? ''}
          placeholder={found?.name ?? 'the name at the top of its marketplace.json'}
          aria-label="Marketplace name"
          className="mono"
          onChange={(e) => {
            const v = e.target.value
            setRow((cur) => ({ ...cur, name: v, autoName: false }))
          }}
        />
        <NameHint row={row} found={found} duplicate={duplicate} onUseFound={(n) =>
          setRow((cur) => ({ ...cur, name: n, autoName: true }))
        } />
      </div>

      <div className="field">
        <label>Plugins to install</label>
        {!name ? (
          <span className="hint">
            {mine.length
              ? `${mine.length} plugin(s) are kept for this marketplace; give it a name to edit them.`
              : 'Name the marketplace first — its plugins are installed as plugin@name.'}
          </span>
        ) : (
          <>
            {offered.length > 0 && (
              <PluginChecklist
                offered={offered}
                selected={new Set(mine.map(pluginOf))}
                onToggle={toggle}
              />
            )}
            {found && offered.length === 0 && (
              <span className="hint">The catalogue lists no plugins.</span>
            )}
            {unlisted.length > 0 && (
              <div className="stack" style={{ gap: 4 }}>
                {found && <span className="hint">Not listed in the catalogue:</span>}
                <div className="chips">
                  {unlisted.map((id) => (
                    <span key={id} className="chip">
                      <span className="mono">{pluginOf(id)}</span>
                      <button
                        type="button"
                        className="chip-x"
                        aria-label={`Remove ${id}`}
                        onClick={() => toggle(pluginOf(id), false)}
                      >
                        ✕
                      </button>
                    </span>
                  ))}
                </div>
              </div>
            )}
            <AddPlugin
              marketplace={name}
              taken={new Set(mine.map(pluginOf))}
              onAdd={(plugin) => toggle(plugin, true)}
            />
          </>
        )}
      </div>
    </div>
  )
}

function InspectionStatus({
  urlError,
  refError,
  checking,
  error,
  result,
}: {
  urlError?: string
  refError?: string
  checking: boolean
  error: Error | null
  result?: MarketplaceInspection
}) {
  if (urlError) return <span className="err small">{urlError}</span>
  if (refError) return <span className="err small">{refError}</span>
  if (checking) {
    return (
      <span className="inline small muted">
        <Spinner /> Reading the catalogue…
      </span>
    )
  }
  if (error) {
    return (
      <span className="small muted">
        Could not check the repository ({error.message}). Enter the name and plugins by hand.
      </span>
    )
  }
  if (!result) {
    return (
      <span className="hint">
        The catalogue's name and plugins are read from the repository. Private repositories are
        read, and fetched by runs, with the installation's git token.
      </span>
    )
  }
  if (!result.found) {
    return (
      <span className="small warn-text">
        Couldn't read the catalogue: {result.problem}. Enter the name and plugins by hand.
      </span>
    )
  }
  return (
    <span className="small">
      <span className="ok-text">✓</span> Found <span className="mono">{result.name}</span>
      {' — '}
      {result.plugins.length} plugin{result.plugins.length === 1 ? '' : 's'}
      {result.description && <span className="muted"> · {result.description}</span>}
    </span>
  )
}

function NameHint({
  row,
  found,
  duplicate,
  onUseFound,
}: {
  row: MarketplaceRow
  found?: MarketplaceInspection
  duplicate: boolean
  onUseFound: (name: string) => void
}) {
  const name = row.name?.trim() ?? ''
  if (name && !NAME.test(name)) return <span className="err">{name} is not a usable marketplace name</span>
  if (duplicate) return <span className="err">another marketplace above has this name</span>
  if (found?.name && name && name !== found.name) {
    return (
      <span className="small warn-text">
        The catalogue calls itself <span className="mono">{found.name}</span>, and the agent's CLI
        registers it under that name — plugins saved as <span className="mono">…@{name}</span> may
        not install.{' '}
        <button type="button" className="ghost sm" onClick={() => onUseFound(found.name!)}>
          Use {found.name}
        </button>
      </span>
    )
  }
  if (found?.name && name === found.name) {
    return (
      <span className="hint">
        From the catalogue. Its plugins are installed as <span className="mono">plugin@{name}</span>.
      </span>
    )
  }
  return <span className="hint">The name declared at the top of the repository's marketplace.json.</span>
}

function PluginChecklist({
  offered,
  selected,
  onToggle,
}: {
  offered: MarketplaceInspection['plugins']
  selected: Set<string>
  onToggle: (plugin: string, on: boolean) => void
}) {
  const [filter, setFilter] = useState('')
  const shown = useMemo(() => {
    const f = filter.trim().toLowerCase()
    if (!f) return offered
    return offered.filter(
      (p) => p.name.toLowerCase().includes(f) || p.description?.toLowerCase().includes(f),
    )
  }, [offered, filter])

  return (
    <div className="stack" style={{ gap: 6 }}>
      {offered.length > 8 && (
        <input
          value={filter}
          placeholder={`Filter ${offered.length} plugins`}
          aria-label="Filter plugins"
          onChange={(e) => setFilter(e.target.value)}
        />
      )}
      <div className="mkt-plugins">
        {shown.map((p) => {
          const on = selected.has(p.name)
          return (
            <label key={p.name} className={on ? 'mkt-plugin on' : 'mkt-plugin'}>
              <input type="checkbox" checked={on} onChange={(e) => onToggle(p.name, e.target.checked)} />
              <span className="mkt-plugin-text">
                <span className="mono">{p.name}</span>
                {p.version && <span className="faint small"> {p.version}</span>}
                {p.description && <span className="muted small"> — {p.description}</span>}
              </span>
            </label>
          )
        })}
        {shown.length === 0 && <span className="hint">Nothing matches.</span>}
      </div>
    </div>
  )
}

function AddPlugin({
  marketplace,
  taken,
  onAdd,
}: {
  marketplace: string
  taken: Set<string>
  onAdd: (plugin: string) => void
}) {
  const [draft, setDraft] = useState('')
  const [error, setError] = useState('')

  const add = () => {
    let entry = draft.trim()
    if (!entry) return
    // A full id pasted in is fine when it names this marketplace.
    if (entry.includes('@')) {
      if (marketplaceOf(entry) !== marketplace) {
        setError(`the @${marketplace} part is added for you; for another marketplace, add it above`)
        return
      }
      entry = pluginOf(entry)
    }
    if (!NAME.test(entry)) {
      setError(`${entry} is not a usable plugin name`)
      return
    }
    if (taken.has(entry)) {
      setError('already selected')
      return
    }
    setError('')
    setDraft('')
    onAdd(entry)
  }

  return (
    <div className="stack" style={{ gap: 4 }}>
      <div className="row" style={{ gap: 6, flexWrap: 'nowrap', alignItems: 'center' }}>
        <input
          value={draft}
          placeholder="add a plugin by name"
          aria-label="Plugin name"
          onChange={(e) => {
            setDraft(e.target.value)
            setError('')
          }}
          onKeyDown={(e) => {
            if (e.key === 'Enter') {
              e.preventDefault()
              add()
            }
          }}
        />
        <span className="mono faint small" style={{ whiteSpace: 'nowrap' }}>
          @{marketplace}
        </span>
        <button type="button" className="ghost sm" onClick={add} disabled={!draft.trim()}>
          Add
        </button>
      </div>
      {error && <span className="err">{error}</span>}
    </div>
  )
}
