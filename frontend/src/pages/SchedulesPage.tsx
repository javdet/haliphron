import { useMemo, useState } from 'react'
import { Link, useNavigate } from 'react-router-dom'
import {
  useCreateSchedule,
  useDeleteSchedule,
  useRoles,
  useSchedules,
  useTriggerSchedule,
  useUpdateSchedule,
} from '../api/hooks'
import {
  CONCURRENCY_POLICIES,
  type ConcurrencyPolicy,
  type Run,
  type RunTemplate,
  type Schedule,
  type ScheduleOutcome,
  type ScheduleRequest,
} from '../api/types'
import { compact, RunSpecFields } from '../components/RunSpecFields'
import { Card, Dialog, Empty, ErrorBanner, Field, Spinner, Time } from '../components/ui'

/**
 * Schedules: a run template the backend admits on a cron expression.
 *
 * The page says what a schedule will do next and what it did last, because
 * those are the two questions an operator has about something that runs while
 * nobody is looking — and "last" includes the fires that admitted nothing:
 * skipped by the policy, missed past the deadline, refused by admission.
 */

const PRESETS: { label: string; cron: string }[] = [
  { label: 'Hourly', cron: '0 * * * *' },
  { label: 'Daily 03:00', cron: '0 3 * * *' },
  { label: 'Weekdays 09:00', cron: '0 9 * * 1-5' },
  { label: 'Mondays 08:00', cron: '0 8 * * 1' },
]

function browserTimezone(): string {
  try {
    return Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC'
  } catch {
    return 'UTC'
  }
}

function timezones(): string[] {
  try {
    return ['UTC', ...Intl.supportedValuesOf('timeZone')]
  } catch {
    return ['UTC']
  }
}

interface ScheduleForm {
  name: string
  description: string
  cron: string
  timezone: string
  enabled: boolean
  concurrency_policy: ConcurrencyPolicy
  starting_deadline_seconds?: number
  run: RunTemplate
}

function formOf(schedule: Schedule | null): ScheduleForm {
  if (!schedule) {
    return {
      name: '',
      description: '',
      cron: '0 3 * * *',
      timezone: browserTimezone(),
      enabled: true,
      concurrency_policy: 'Forbid',
      run: { prompt: '', agent: 'claude-code' },
    }
  }
  return {
    name: schedule.name,
    description: schedule.description,
    cron: schedule.cron,
    timezone: schedule.timezone,
    enabled: schedule.enabled,
    concurrency_policy: schedule.concurrency_policy,
    starting_deadline_seconds: schedule.starting_deadline_seconds,
    run: { agent: 'claude-code', ...schedule.run },
  }
}

export function ScheduleDialog({
  schedule,
  roles,
  onClose,
}: {
  schedule: Schedule | null
  roles: string[]
  onClose: () => void
}) {
  const create = useCreateSchedule()
  const update = useUpdateSchedule()
  const [form, setForm] = useState<ScheduleForm>(() => formOf(schedule))
  const zones = useMemo(timezones, [])
  const saving = create.isPending || update.isPending

  const set = <K extends keyof ScheduleForm>(key: K, value: ScheduleForm[K]) =>
    setForm((f) => ({ ...f, [key]: value }))
  const setRun = <K extends keyof RunTemplate>(key: K, value: RunTemplate[K]) =>
    setForm((f) => ({ ...f, run: { ...f.run, [key]: value } }))

  const save = () => {
    const body: ScheduleRequest = {
      name: form.name.trim(),
      description: form.description.trim(),
      cron: form.cron.trim(),
      timezone: form.timezone.trim() || 'UTC',
      enabled: form.enabled,
      concurrency_policy: form.concurrency_policy,
      run: compact(form.run),
    }
    if (schedule) {
      // A PATCH leaves out what is not sent, so clearing the deadline is
      // said out loud: zero is "none".
      body.starting_deadline_seconds = form.starting_deadline_seconds ?? 0
      update.mutate({ id: schedule.id, patch: body }, { onSuccess: onClose })
      return
    }
    if (form.starting_deadline_seconds) body.starting_deadline_seconds = form.starting_deadline_seconds
    create.mutate(body, { onSuccess: onClose })
  }

  return (
    <Dialog
      title={schedule ? `Edit ${schedule.name}` : 'New schedule'}
      onClose={onClose}
      footer={
        <>
          <label className={form.enabled ? 'check on' : 'check'}>
            <input type="checkbox" checked={form.enabled} onChange={(e) => set('enabled', e.target.checked)} />
            Enabled
          </label>
          <span className="spacer" />
          <button className="ghost" onClick={onClose}>
            Cancel
          </button>
          <button
            className="primary"
            onClick={save}
            disabled={!form.name.trim() || !form.cron.trim() || !form.run.prompt.trim() || saving}
          >
            {saving ? 'Saving…' : 'Save schedule'}
          </button>
        </>
      }
    >
      <ErrorBanner error={create.error ?? update.error} what="save the schedule" />

      <div className="grid-2">
        <Field label="Name" hint="Lower-case letters, digits, '-', '_' and '.'.">
          <input value={form.name} onChange={(e) => set('name', e.target.value)} placeholder="nightly-deps" />
        </Field>
        <Field label="Description">
          <input value={form.description} onChange={(e) => set('description', e.target.value)} />
        </Field>
      </div>

      <div className="grid-2">
        <Field label="Cron" hint="minute hour day-of-month month day-of-week, or @hourly / @daily / @weekly.">
          <input className="mono" value={form.cron} onChange={(e) => set('cron', e.target.value)} />
        </Field>
        <Field label="Timezone" hint="The cron is read in this IANA zone, across daylight saving.">
          <input list="schedule-timezones" value={form.timezone} onChange={(e) => set('timezone', e.target.value)} />
          <datalist id="schedule-timezones">
            {zones.map((zone) => (
              <option key={zone} value={zone} />
            ))}
          </datalist>
        </Field>
      </div>
      <div className="inline">
        {PRESETS.map((preset) => (
          <button key={preset.cron} type="button" className="ghost sm" onClick={() => set('cron', preset.cron)}>
            {preset.label}
          </button>
        ))}
      </div>

      <div className="grid-2">
        <Field label="When a run is still live">
          <select
            value={form.concurrency_policy}
            onChange={(e) => set('concurrency_policy', e.target.value as ConcurrencyPolicy)}
          >
            {CONCURRENCY_POLICIES.map((p) => (
              <option key={p.value} value={p.value}>
                {p.label}
              </option>
            ))}
          </select>
        </Field>
        <Field
          label="Starting deadline (s)"
          hint="A fire later than this — after an outage — is dropped. Empty: always fire once."
        >
          <input
            type="number"
            min={0}
            value={form.starting_deadline_seconds ?? ''}
            onChange={(e) => set('starting_deadline_seconds', Number(e.target.value) || undefined)}
          />
        </Field>
      </div>

      <RunSpecFields form={form.run} set={setRun} roles={roles} />
    </Dialog>
  )
}

export function DeleteScheduleDialog({
  schedule,
  onClose,
  onDeleted,
}: {
  schedule: Schedule
  onClose: () => void
  onDeleted?: () => void
}) {
  const remove = useDeleteSchedule()
  return (
    <Dialog
      title={`Delete ${schedule.name}`}
      onClose={onClose}
      footer={
        <>
          <button className="ghost" onClick={onClose}>
            Keep it
          </button>
          <button
            className="danger"
            disabled={remove.isPending}
            onClick={() =>
              remove.mutate(schedule.id, {
                onSuccess: () => {
                  onClose()
                  onDeleted?.()
                },
              })
            }
          >
            {remove.isPending ? 'Deleting…' : 'Delete schedule'}
          </button>
        </>
      }
    >
      <ErrorBanner error={remove.error} what="delete the schedule" />
      <p className="small muted" style={{ margin: 0 }}>
        No further runs are started. The runs it already started are kept, and stop naming it. To pause
        it instead, disable it.
      </p>
    </Dialog>
  )
}

const OUTCOME_CLASS: Record<ScheduleOutcome, string> = {
  submitted: 'ok',
  skipped: 'idle',
  missed: 'warn',
  refused: 'bad',
}

/** What the last fire did, and why when it admitted nothing. */
export function LastFire({ schedule }: { schedule: Schedule }) {
  if (!schedule.last_outcome) return <span className="faint">never fired</span>
  return (
    <div>
      <span className={`badge ${OUTCOME_CLASS[schedule.last_outcome]}`} title={schedule.last_error}>
        <span className="dot" />
        {schedule.last_outcome}
      </span>{' '}
      <span className="small muted">
        <Time at={schedule.last_fired_at} />
      </span>
      {schedule.last_error && <div className="small muted trunc">{schedule.last_error}</div>}
      {schedule.last_outcome === 'submitted' && schedule.last_run_id && (
        <div className="small">
          <Link className="mono" to={`/runs/${schedule.last_run_id}`} onClick={(e) => e.stopPropagation()}>
            {schedule.last_run_id}
          </Link>
        </div>
      )}
    </div>
  )
}

/** The next fire, or why there is none. */
export function NextFire({ schedule }: { schedule: Schedule }) {
  if (!schedule.enabled || !schedule.next_run_at) return <span className="faint">disabled</span>
  return <Time at={schedule.next_run_at} />
}

/**
 * Run now, then go to the run: the operator asked to see it start, and a toast
 * on a list they then have to search is the slower way to show them.
 */
export function useRunNow() {
  const navigate = useNavigate()
  const trigger = useTriggerSchedule()
  return {
    trigger,
    runNow: (id: string) => trigger.mutate(id, { onSuccess: (run: Run) => navigate(`/runs/${run.run_id}`) }),
  }
}

export function SchedulesPage() {
  const navigate = useNavigate()
  const schedules = useSchedules()
  const roles = useRoles()
  const update = useUpdateSchedule()
  const { trigger, runNow } = useRunNow()
  const [creating, setCreating] = useState(false)
  const [editing, setEditing] = useState<Schedule | null>(null)
  const [deleting, setDeleting] = useState<Schedule | null>(null)
  const roleNames = roles.data?.map((r) => r.name) ?? []
  const rows = schedules.data ?? []

  return (
    <>
      <header className="topbar">
        <div className="topbar-title">
          <h1>Schedules</h1>
          {schedules.isFetching && <Spinner />}
        </div>
        <div className="topbar-actions">
          <button className="primary" onClick={() => setCreating(true)}>
            New schedule
          </button>
        </div>
      </header>

      <div className="content">
        <ErrorBanner error={schedules.error} what="load the schedules" />
        <ErrorBanner error={trigger.error} what="start the run" />
        <ErrorBanner error={update.error} what="change the schedule" />

        <Card tight>
          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th>Name</th>
                  <th>When</th>
                  <th>Run</th>
                  <th>If live</th>
                  <th>Next</th>
                  <th>Last fire</th>
                  <th>Enabled</th>
                  <th />
                </tr>
              </thead>
              <tbody>
                {rows.map((schedule) => (
                  <tr key={schedule.id} className="clickable" onClick={() => navigate(`/schedules/${schedule.id}`)}>
                    <td>
                      {schedule.name}
                      {schedule.description && <div className="small muted trunc">{schedule.description}</div>}
                    </td>
                    <td className="nowrap">
                      <span className="mono small">{schedule.cron}</span>
                      <div className="small faint">{schedule.timezone}</div>
                    </td>
                    <td>
                      {schedule.run.role || <span className="faint">default role</span>}
                      <div className="small muted trunc trunc-sm">{schedule.run.repo || 'no repository'}</div>
                    </td>
                    <td className="nowrap small muted">{schedule.concurrency_policy}</td>
                    <td className="small nowrap">
                      <NextFire schedule={schedule} />
                    </td>
                    <td>
                      <LastFire schedule={schedule} />
                    </td>
                    <td onClick={(e) => e.stopPropagation()}>
                      <input
                        type="checkbox"
                        aria-label={schedule.enabled ? `Disable ${schedule.name}` : `Enable ${schedule.name}`}
                        checked={schedule.enabled}
                        disabled={update.isPending}
                        onChange={(e) => update.mutate({ id: schedule.id, patch: { enabled: e.target.checked } })}
                      />
                    </td>
                    <td className="nowrap" onClick={(e) => e.stopPropagation()}>
                      <button className="sm" disabled={trigger.isPending} onClick={() => runNow(schedule.id)}>
                        Run now
                      </button>
                      <button className="sm ghost" onClick={() => setEditing(schedule)}>
                        Edit
                      </button>
                      <button className="sm danger" onClick={() => setDeleting(schedule)}>
                        Delete
                      </button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          {rows.length === 0 && !schedules.isLoading && (
            <Empty>No schedules yet. A schedule starts a run from a template on a cron expression.</Empty>
          )}
          {schedules.isLoading && (
            <Empty>
              <Spinner />
            </Empty>
          )}
        </Card>
      </div>

      {creating && <ScheduleDialog schedule={null} roles={roleNames} onClose={() => setCreating(false)} />}
      {editing && <ScheduleDialog schedule={editing} roles={roleNames} onClose={() => setEditing(null)} />}
      {deleting && <DeleteScheduleDialog schedule={deleting} onClose={() => setDeleting(null)} />}
    </>
  )
}
