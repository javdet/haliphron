import { useEffect, useState } from 'react'
import { usePutRunRetention, usePutTokenRetention, useRunRetention, useTokenRetention } from '../api/hooks'
import { Card, ErrorBanner, Field, Spinner, Time } from '../components/ui'
import type { RunRetention, TokenRetention } from '../api/types'
import { ApiTokensCard } from '../components/ApiTokens'

/**
 * Installation-wide settings an operator changes from here rather than from the
 * deployment: how long failed runs are kept, and how long ended per-run tokens
 * are.
 */

const HOUR = 3600
const DAY = 24 * HOUR

type Unit = 'days' | 'hours'

interface FormState {
  forever: boolean
  amount: string
  unit: Unit
}

/** Days when the stored age is whole days, hours otherwise. */
function fromSeconds(s: number, foreverDays: number): FormState {
  if (s === 0) return { forever: true, amount: String(foreverDays), unit: 'days' }
  if (s % DAY === 0) return { forever: false, amount: String(s / DAY), unit: 'days' }
  return { forever: false, amount: String(Math.round((s / HOUR) * 100) / 100), unit: 'hours' }
}

const fromRetention = (r: RunRetention) => fromSeconds(r.failed_retention_seconds, 3)
const fromTokenRetention = (r: TokenRetention) => fromSeconds(r.run_token_retention_seconds, 14)

function toSeconds(form: FormState): number {
  if (form.forever) return 0
  return Math.round(Number(form.amount) * (form.unit === 'days' ? DAY : HOUR))
}

/** An age in words, for what the deployment sets and this page only shows. */
function age(seconds: number): string {
  if (seconds % DAY === 0) return `${seconds / DAY} day${seconds === DAY ? '' : 's'}`
  if (seconds % HOUR === 0) return `${seconds / HOUR} hour${seconds === HOUR ? '' : 's'}`
  return `${seconds} seconds`
}

/** The radio pair and the amount, shared by both retention cards. */
function AgeField({
  form,
  set,
  label,
  hint,
  error,
}: {
  form: FormState
  set: (next: Partial<FormState>) => void
  label: string
  hint: string
  error?: string
}) {
  return (
    <Field label={label} error={error} hint={hint}>
      <div className="checks">
        <label className={!form.forever ? 'check on' : 'check'}>
          <input type="radio" checked={!form.forever} onChange={() => set({ forever: false })} />
          Delete after
        </label>
        <label className={form.forever ? 'check on' : 'check'}>
          <input type="radio" checked={form.forever} onChange={() => set({ forever: true })} />
          Keep forever
        </label>
      </div>
      {!form.forever && (
        <div className="row" style={{ gap: 6, flexWrap: 'nowrap', maxWidth: 260 }}>
          <input
            value={form.amount}
            inputMode="decimal"
            aria-label="How long"
            onChange={(e) => set({ amount: e.target.value.replace(/[^0-9.]/g, '') })}
          />
          <select value={form.unit} aria-label="Unit" onChange={(e) => set({ unit: e.target.value as Unit })}>
            <option value="days">days</option>
            <option value="hours">hours</option>
          </select>
        </div>
      )}
    </Field>
  )
}

function RunRetentionCard() {
  const current = useRunRetention()
  const put = usePutRunRetention()
  const [form, setForm] = useState<FormState | undefined>()
  const [saved, setSaved] = useState(false)

  useEffect(() => {
    if (current.data && !form) setForm(fromRetention(current.data))
  }, [current.data, form])

  const r = current.data
  if (!r || !form) {
    return (
      <Card title="Run retention">
        <ErrorBanner error={current.error} what="read the run retention" />
        {current.isLoading && <Spinner />}
      </Card>
    )
  }

  const set = (next: Partial<FormState>) => {
    setSaved(false)
    setForm((f) => (f ? { ...f, ...next } : f))
  }
  const seconds = toSeconds(form)
  const problem =
    !form.forever && (!form.amount.trim() || !Number.isFinite(seconds) || seconds < HOUR)
      ? 'At least one hour. A failed run still owes a chat reply and a task result, and somebody the time to look at it.'
      : undefined
  const changed = seconds !== r.failed_retention_seconds

  const save = () =>
    put.mutate(seconds, {
      onSuccess: (next) => {
        setForm(fromRetention(next))
        setSaved(true)
      },
    })

  return (
    <Card
      title="Run retention"
      actions={<span className="badge">{r.source === 'default' ? 'default' : 'saved'}</span>}
    >
      <div className="stack">
        <ErrorBanner error={put.error} what="save the run retention" />
        {saved && !put.error && (
          <div className="banner good" role="status">
            <div className="small">Saved. The next retention pass, within the reap interval (an hour by default), applies it.</div>
          </div>
        )}

        <AgeField
          form={form}
          set={set}
          label="Failed runs"
          error={problem}
          hint="A run whose status is Failed is deleted this long after it finished, with its attempts, logs, results and artifacts. The audit log keeps a record of each one. Timed-out and cancelled runs are not affected."
        />

        <div className="small muted">
          {r.retention_seconds > 0 ? (
            <>
              Every finished run, whatever its status, is also deleted after {age(r.retention_seconds)} — the
              deployment’s <span className="mono">runRetention</span>. A failed run goes at whichever comes first.
            </>
          ) : (
            <>
              Runs of every other status are kept forever: the deployment’s{' '}
              <span className="mono">runRetention</span> is not set.
            </>
          )}
        </div>

        <div className="row" style={{ justifyContent: 'space-between' }}>
          <span className="small muted">
            {r.updated_at ? (
              <>
                Last saved by {r.updated_by} <Time at={r.updated_at} />
              </>
            ) : (
              'Never saved: the default of 3 days applies.'
            )}
          </span>
          <button
            className="primary"
            onClick={save}
            // The default can be saved unchanged, to pin it.
            disabled={!!problem || (!changed && r.source === 'database') || put.isPending}
          >
            {put.isPending ? 'Saving…' : 'Save'}
          </button>
        </div>
      </div>
    </Card>
  )
}

function TokenRetentionCard() {
  const current = useTokenRetention()
  const put = usePutTokenRetention()
  const [form, setForm] = useState<FormState | undefined>()
  const [saved, setSaved] = useState(false)

  useEffect(() => {
    if (current.data && !form) setForm(fromTokenRetention(current.data))
  }, [current.data, form])

  const r = current.data
  if (!r || !form) {
    return (
      <Card title="Token retention">
        <ErrorBanner error={current.error} what="read the token retention" />
        {current.isLoading && <Spinner />}
      </Card>
    )
  }

  const set = (next: Partial<FormState>) => {
    setSaved(false)
    setForm((f) => (f ? { ...f, ...next } : f))
  }
  const seconds = toSeconds(form)
  const problem =
    !form.forever && (!form.amount.trim() || !Number.isFinite(seconds) || seconds < HOUR)
      ? 'At least one hour.'
      : undefined
  const changed = seconds !== r.run_token_retention_seconds

  const save = () =>
    put.mutate(seconds, {
      onSuccess: (next) => {
        setForm(fromTokenRetention(next))
        setSaved(true)
      },
    })

  return (
    <Card
      title="Token retention"
      actions={<span className="badge">{r.source === 'default' ? 'default' : 'saved'}</span>}
    >
      <div className="stack">
        <ErrorBanner error={put.error} what="save the token retention" />
        {saved && !put.error && (
          <div className="banner good" role="status">
            <div className="small">Saved. The next retention pass, within the reap interval (an hour by default), applies it.</div>
          </div>
        )}

        <AgeField
          form={form}
          set={set}
          label="Per-run tokens"
          error={problem}
          hint="Every run gets its own MCP token, which stops working when the run ends. An ended per-run token is deleted this long after it was created, or with its run if that goes first. Tokens minted by hand are never deleted automatically."
        />

        <div className="row" style={{ justifyContent: 'space-between' }}>
          <span className="small muted">
            {r.updated_at ? (
              <>
                Last saved by {r.updated_by} <Time at={r.updated_at} />
              </>
            ) : (
              'Never saved: the default of 14 days applies.'
            )}
          </span>
          <button
            className="primary"
            onClick={save}
            // The default can be saved unchanged, to pin it.
            disabled={!!problem || (!changed && r.source === 'database') || put.isPending}
          >
            {put.isPending ? 'Saving…' : 'Save'}
          </button>
        </div>
      </div>
    </Card>
  )
}

export function SettingsPage() {
  return (
    <>
      <header className="topbar">
        <div className="topbar-title">
          <h1>Settings</h1>
        </div>
      </header>
      <div className="content">
        <ApiTokensCard />
        <RunRetentionCard />
        <TokenRetentionCard />
      </div>
    </>
  )
}
