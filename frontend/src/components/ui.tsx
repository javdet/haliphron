// The small shared pieces. Nothing here knows about the API.

import { useEffect, useRef, useState, type ReactNode } from 'react'
import { ApiError } from '../api/client'

export function Card({
  title,
  actions,
  children,
  tight,
}: {
  title?: ReactNode
  actions?: ReactNode
  children: ReactNode
  tight?: boolean
}) {
  return (
    <section className="card">
      {(title || actions) && (
        <header className="card-head">
          {typeof title === 'string' ? <h2>{title}</h2> : title}
          {actions && <div className="inline">{actions}</div>}
        </header>
      )}
      <div className={tight ? 'card-body tight' : 'card-body'}>{children}</div>
    </section>
  )
}

export function Field({
  label,
  hint,
  error,
  children,
}: {
  label: string
  hint?: string
  error?: string
  children: ReactNode
}) {
  return (
    <div className="field">
      <label>{label}</label>
      {children}
      {error ? <span className="err">{error}</span> : hint ? <span className="hint">{hint}</span> : null}
    </div>
  )
}

export function Empty({ children }: { children: ReactNode }) {
  return <div className="empty">{children}</div>
}

export function Spinner() {
  return <span className="spinner" aria-label="loading" />
}

/**
 * Renders whatever went wrong, and says what to do about the two cases an
 * operator can actually act on: a token that is not accepted, and a token that
 * is accepted but is not admin.
 */
export function ErrorBanner({ error, what }: { error: unknown; what?: string }) {
  if (!error) return null
  const api = error instanceof ApiError ? error : undefined
  const title = api?.forbidden
    ? 'This token lacks the scope for that'
    : api?.unauthenticated
      ? 'The token is not usable'
      : what
        ? `Could not ${what}`
        : 'Something went wrong'
  return (
    <div className="banner" role="alert">
      <div>
        <div className="banner-title">{title}</div>
        <div className="small muted">
          {error instanceof Error ? error.message : String(error)}
          {api?.field ? ` (field: ${api.field})` : ''}
        </div>
      </div>
    </div>
  )
}

export function Dialog({
  title,
  onClose,
  children,
  footer,
}: {
  title: string
  onClose: () => void
  children: ReactNode
  footer?: ReactNode
}) {
  const ref = useRef<HTMLDivElement>(null)

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => e.key === 'Escape' && onClose()
    window.addEventListener('keydown', onKey)
    ref.current?.querySelector<HTMLElement>('input, textarea, select, button')?.focus()
    return () => window.removeEventListener('keydown', onKey)
  }, [onClose])

  return (
    <div className="dialog-backdrop" onMouseDown={(e) => e.target === e.currentTarget && onClose()}>
      <div className="dialog" role="dialog" aria-modal="true" aria-label={title} ref={ref}>
        <header className="dialog-head">
          <h2>{title}</h2>
          <button className="ghost sm" onClick={onClose} aria-label="Close">
            ✕
          </button>
        </header>
        <div className="dialog-body">{children}</div>
        {footer && <footer className="dialog-foot">{footer}</footer>}
      </div>
    </div>
  )
}

/** Copies text and says so, because a silent copy button is untrustworthy. */
export function CopyButton({ value, label = 'Copy' }: { value: string; label?: string }) {
  const [done, setDone] = useState(false)
  return (
    <button
      className="ghost sm"
      onClick={() => {
        void navigator.clipboard.writeText(value).then(() => {
          setDone(true)
          setTimeout(() => setDone(false), 1400)
        })
      }}
    >
      {done ? 'Copied' : label}
    </button>
  )
}

const UNITS: [number, Intl.RelativeTimeFormatUnit][] = [
  [60, 'second'],
  [3600, 'minute'],
  [86400, 'hour'],
  [2592000, 'day'],
]

const rtf = new Intl.RelativeTimeFormat(undefined, { numeric: 'auto' })

export function relative(iso?: string): string {
  if (!iso) return '—'
  const then = new Date(iso).getTime()
  if (Number.isNaN(then)) return '—'
  const secs = (then - Date.now()) / 1000
  const abs = Math.abs(secs)
  if (abs < 45) return rtf.format(Math.round(secs), 'second')
  for (let i = 0; i < UNITS.length - 1; i++) {
    const [limit, unit] = UNITS[i + 1]
    if (abs < limit) return rtf.format(Math.round(secs / UNITS[i][0]), unit)
  }
  return new Date(iso).toLocaleDateString()
}

/** A timestamp shown as "3 minutes ago", with the exact value on hover. */
export function Time({ at }: { at?: string }) {
  if (!at) return <span className="faint">—</span>
  return (
    <time dateTime={at} title={new Date(at).toLocaleString()} className="nowrap">
      {relative(at)}
    </time>
  )
}

export function duration(ms?: number): string {
  if (ms === undefined || ms === null) return '—'
  const s = Math.round(ms / 1000)
  if (s < 60) return `${s}s`
  const m = Math.floor(s / 60)
  if (m < 60) return `${m}m ${s % 60}s`
  return `${Math.floor(m / 60)}h ${m % 60}m`
}

export function elapsed(from?: string, to?: string): string {
  if (!from) return '—'
  const start = new Date(from).getTime()
  const end = to ? new Date(to).getTime() : Date.now()
  if (Number.isNaN(start) || Number.isNaN(end)) return '—'
  return duration(end - start)
}

/** Money arrives as a decimal string and is shown as one: no float, ever. */
export function money(value?: string): string {
  if (!value) return '$0.00'
  const trimmed = value.trim()
  return trimmed.startsWith('$') ? trimmed : `$${trimmed}`
}

export function tokens(n: number): string {
  if (n < 1000) return String(n)
  if (n < 1_000_000) return `${(n / 1000).toFixed(1)}k`
  return `${(n / 1_000_000).toFixed(2)}M`
}

/**
 * A collapsible section. Used for the settings a role usually leaves alone, so
 * the fields that are actually edited are not buried among them.
 */
export function Section({
  title,
  hint,
  children,
  open,
}: {
  title: string
  hint?: string
  children: ReactNode
  open?: boolean
}) {
  return (
    <details className="section" open={open}>
      <summary>
        <span>{title}</span>
        {hint && <span className="small muted">{hint}</span>}
      </summary>
      <div className="stack">{children}</div>
    </details>
  )
}

/**
 * An editable list of strings, shown as removable chips.
 *
 * Entries are added on Enter, because the alternative — a free-text box the
 * caller splits on commas — makes an accidental comma a second, silently wrong
 * entry, and every list here is a name something else will be looked up by.
 */
export function StringList({
  value,
  onChange,
  placeholder,
  validate,
}: {
  value: string[]
  onChange: (next: string[]) => void
  placeholder?: string
  /** Returns a message when the entry may not be added. */
  validate?: (entry: string) => string | undefined
}) {
  const [draft, setDraft] = useState('')
  const [error, setError] = useState('')

  const add = () => {
    const entry = draft.trim()
    if (!entry) return
    if (value.includes(entry)) {
      setError('already in the list')
      return
    }
    const complaint = validate?.(entry)
    if (complaint) {
      setError(complaint)
      return
    }
    setError('')
    setDraft('')
    onChange([...value, entry])
  }

  return (
    <div className="stack" style={{ gap: 6 }}>
      {value.length > 0 && (
        <div className="chips">
          {value.map((entry) => (
            <span key={entry} className="chip">
              <span className="mono">{entry}</span>
              <button
                type="button"
                className="chip-x"
                aria-label={`Remove ${entry}`}
                onClick={() => onChange(value.filter((v) => v !== entry))}
              >
                ✕
              </button>
            </span>
          ))}
        </div>
      )}
      <div className="row" style={{ gap: 6, flexWrap: 'nowrap' }}>
        <input
          value={draft}
          placeholder={placeholder}
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
        <button type="button" className="ghost sm" onClick={add} disabled={!draft.trim()}>
          Add
        </button>
      </div>
      {error && <span className="err">{error}</span>}
    </div>
  )
}

/**
 * A repeatable row of structured entries: MCP servers, marketplaces, variables.
 *
 * The blank row is created by the caller's `empty`, so a row always arrives
 * with the shape the rest of the form expects rather than as a partial object
 * that every reader has to guard against.
 */
export function Rows<T>({
  value,
  onChange,
  empty,
  addLabel,
  children,
}: {
  value: T[]
  onChange: (next: T[]) => void
  empty: () => T
  addLabel: string
  children: (row: T, set: (next: T) => void, index: number) => ReactNode
}) {
  const replace = (index: number, next: T) =>
    onChange(value.map((row, i) => (i === index ? next : row)))

  return (
    <div className="stack" style={{ gap: 8 }}>
      {value.map((row, index) => (
        <div key={index} className="rowset">
          <div className="rowset-fields">{children(row, (next) => replace(index, next), index)}</div>
          <button
            type="button"
            className="ghost sm"
            aria-label="Remove row"
            onClick={() => onChange(value.filter((_, i) => i !== index))}
          >
            ✕
          </button>
        </div>
      ))}
      <div>
        <button type="button" className="ghost sm" onClick={() => onChange([...value, empty()])}>
          {addLabel}
        </button>
      </div>
    </div>
  )
}
