import { useState } from 'react'
import { Link, useNavigate } from 'react-router-dom'
import {
  useAlertReceivers,
  useCreateAlertReceiver,
  useDeleteAlertReceiver,
  useRoles,
  useUpdateAlertReceiver,
} from '../api/hooks'
import {
  type AlertOutcome,
  type AlertReceiver,
  type AlertReceiverRequest,
  type AlertReceiverWithToken,
  type RunTemplate,
} from '../api/types'
import { compact, RunSpecFields } from '../components/RunSpecFields'
import { Card, CopyButton, Dialog, Empty, ErrorBanner, Field, Spinner, Time } from '../components/ui'

/**
 * Alert receivers: Prometheus Alertmanager notifications that start runs.
 *
 * Each notification is one run carrying its firing alerts, under the role the
 * receiver names. An alert that started a run is suppressed for the receiver's
 * window, so a burst of the same alert is one run and not one per delivery.
 */

export const THROTTLE_PRESETS: { label: string; seconds: number }[] = [
  { label: 'Off', seconds: 0 },
  { label: '15 min', seconds: 15 * 60 },
  { label: '1 h', seconds: 60 * 60 },
  { label: '4 h', seconds: 4 * 60 * 60 },
  { label: '24 h', seconds: 24 * 60 * 60 },
]

/** A throttle window as a person reads it. */
export function throttleLabel(seconds: number): string {
  if (seconds === 0) return 'off'
  if (seconds % 3600 === 0) return `${seconds / 3600} h`
  if (seconds % 60 === 0) return `${seconds / 60} min`
  return `${seconds} s`
}

interface ReceiverForm {
  name: string
  description: string
  enabled: boolean
  throttle_seconds: number
  run: RunTemplate
}

function formOf(receiver: AlertReceiver | null): ReceiverForm {
  if (!receiver) {
    return {
      name: '',
      description: '',
      enabled: true,
      throttle_seconds: 3600,
      run: {
        prompt:
          'Investigate the firing alerts below. Find the likely cause and say what you found; ' +
          'if the cause is in this repository, open a pull request with the fix.',
        agent: 'claude-code',
      },
    }
  }
  return {
    name: receiver.name,
    description: receiver.description,
    enabled: receiver.enabled,
    throttle_seconds: receiver.throttle_seconds,
    run: { agent: 'claude-code', ...receiver.run },
  }
}

export function AlertReceiverDialog({
  receiver,
  roles,
  onClose,
  onCreated,
}: {
  receiver: AlertReceiver | null
  roles: string[]
  onClose: () => void
  onCreated?: (created: AlertReceiverWithToken) => void
}) {
  const create = useCreateAlertReceiver()
  const update = useUpdateAlertReceiver()
  const [form, setForm] = useState<ReceiverForm>(() => formOf(receiver))
  const saving = create.isPending || update.isPending

  const set = <K extends keyof ReceiverForm>(key: K, value: ReceiverForm[K]) =>
    setForm((f) => ({ ...f, [key]: value }))
  const setRun = <K extends keyof RunTemplate>(key: K, value: RunTemplate[K]) =>
    setForm((f) => ({ ...f, run: { ...f.run, [key]: value } }))

  const save = () => {
    const body: AlertReceiverRequest = {
      name: form.name.trim(),
      description: form.description.trim(),
      enabled: form.enabled,
      throttle_seconds: form.throttle_seconds,
      run: compact(form.run),
    }
    if (receiver) {
      update.mutate({ id: receiver.id, patch: body }, { onSuccess: onClose })
      return
    }
    create.mutate(body, { onSuccess: (created) => onCreated?.(created) })
  }

  return (
    <Dialog
      title={receiver ? `Edit ${receiver.name}` : 'New alert receiver'}
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
            disabled={!form.name.trim() || !form.run.prompt.trim() || !form.run.role || saving}
          >
            {saving ? 'Saving…' : 'Save receiver'}
          </button>
        </>
      }
    >
      <ErrorBanner error={create.error ?? update.error} what="save the alert receiver" />

      <div className="grid-2">
        <Field label="Name" hint="Lower-case letters, digits, '-', '_' and '.'.">
          <input value={form.name} onChange={(e) => set('name', e.target.value)} placeholder="on-call-triage" />
        </Field>
        <Field label="Description">
          <input value={form.description} onChange={(e) => set('description', e.target.value)} />
        </Field>
      </div>

      <Field
        label="Throttle (s)"
        hint="An alert that started a run is ignored for this long, by its Alertmanager fingerprint. 0: never."
      >
        <input
          type="number"
          min={0}
          max={604800}
          value={form.throttle_seconds}
          onChange={(e) => set('throttle_seconds', Math.max(0, Number(e.target.value) || 0))}
        />
      </Field>
      <div className="inline">
        {THROTTLE_PRESETS.map((preset) => (
          <button
            key={preset.seconds}
            type="button"
            className={form.throttle_seconds === preset.seconds ? 'sm' : 'ghost sm'}
            onClick={() => set('throttle_seconds', preset.seconds)}
          >
            {preset.label}
          </button>
        ))}
      </div>

      <p className="small muted" style={{ margin: 0 }}>
        The prompt is the instruction; the firing alerts of each notification are appended to it as data. The
        role is required: it decides what a run started by anyone holding this receiver's credential may do.
      </p>
      <RunSpecFields form={form.run} set={setRun} roles={roles} />
    </Dialog>
  )
}

/** The Alertmanager configuration for a receiver, filled in. */
export function alertmanagerConfig(receiver: AlertReceiver): string {
  const url = `${window.location.origin}${receiver.webhook_path}`
  return `receivers:
  - name: haliphron-${receiver.name}
    webhook_configs:
      - url: ${url}
        send_resolved: false
        max_alerts: 50
        http_config:
          authorization:
            type: Bearer
            credentials_file: /etc/alertmanager/haliphron-${receiver.name}-token`
}

/** The credential, shown the one time it exists outside a digest. */
export function CredentialDialog({ receiver, onClose }: { receiver: AlertReceiverWithToken; onClose: () => void }) {
  const config = alertmanagerConfig(receiver)
  return (
    <Dialog
      title={`Credential for ${receiver.name}`}
      onClose={onClose}
      footer={
        <button className="primary" onClick={onClose}>
          Done
        </button>
      }
    >
      <div className="banner good">
        <div>
          <div className="banner-title">Copy it now</div>
          <div className="small">
            Only a digest is stored, so there is no second chance to read this. It starts runs on this receiver
            and nothing else.
          </div>
        </div>
      </div>
      <Field label="Credential">
        <input readOnly className="mono" value={receiver.token} onFocus={(e) => e.currentTarget.select()} />
      </Field>
      <CopyButton value={receiver.token} label="Copy credential" />
      <Field
        label="Alertmanager configuration"
        hint={`Put the credential in /etc/alertmanager/haliphron-${receiver.name}-token and route alerts to this receiver.`}
      >
        <pre className="small mono" style={{ whiteSpace: 'pre', overflowX: 'auto', margin: 0 }}>
          {config}
        </pre>
      </Field>
      <CopyButton value={config} label="Copy configuration" />
    </Dialog>
  )
}

export function DeleteAlertReceiverDialog({
  receiver,
  onClose,
  onDeleted,
}: {
  receiver: AlertReceiver
  onClose: () => void
  onDeleted?: () => void
}) {
  const remove = useDeleteAlertReceiver()
  return (
    <Dialog
      title={`Delete ${receiver.name}`}
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
              remove.mutate(receiver.id, {
                onSuccess: () => {
                  onClose()
                  onDeleted?.()
                },
              })
            }
          >
            {remove.isPending ? 'Deleting…' : 'Delete receiver'}
          </button>
        </>
      }
    >
      <ErrorBanner error={remove.error} what="delete the alert receiver" />
      <p className="small muted" style={{ margin: 0 }}>
        Its credential stops working and Alertmanager's deliveries are refused. The runs it started are kept and
        stop naming it; its suppressions and delivery log go. To pause it instead, disable it.
      </p>
    </Dialog>
  )
}

const OUTCOME_CLASS: Record<AlertOutcome, string> = {
  submitted: 'ok',
  throttled: 'idle',
  resolved: 'idle',
  refused: 'bad',
}

export function OutcomeBadge({ outcome, title }: { outcome: AlertOutcome; title?: string }) {
  return (
    <span className={`badge ${OUTCOME_CLASS[outcome]}`} title={title}>
      <span className="dot" />
      {outcome}
    </span>
  )
}

/** What the last notification did, and why when it started nothing. */
export function LastDelivery({ receiver }: { receiver: AlertReceiver }) {
  if (!receiver.last_outcome) return <span className="faint">nothing received yet</span>
  return (
    <div>
      <OutcomeBadge outcome={receiver.last_outcome} title={receiver.last_error} />{' '}
      <span className="small muted">
        <Time at={receiver.last_received_at} />
      </span>
      {receiver.last_error && <div className="small muted trunc">{receiver.last_error}</div>}
      {receiver.last_run_id && (
        <div className="small">
          <Link className="mono" to={`/runs/${receiver.last_run_id}`} onClick={(e) => e.stopPropagation()}>
            {receiver.last_run_id}
          </Link>
        </div>
      )}
    </div>
  )
}

export function AlertReceiversPage() {
  const navigate = useNavigate()
  const receivers = useAlertReceivers()
  const roles = useRoles()
  const update = useUpdateAlertReceiver()
  const [creating, setCreating] = useState(false)
  const [minted, setMinted] = useState<AlertReceiverWithToken | null>(null)
  const [editing, setEditing] = useState<AlertReceiver | null>(null)
  const [deleting, setDeleting] = useState<AlertReceiver | null>(null)
  const roleNames = roles.data?.map((r) => r.name) ?? []
  const rows = receivers.data ?? []

  return (
    <>
      <header className="topbar">
        <div className="topbar-title">
          <h1>Alert receivers</h1>
          {receivers.isFetching && <Spinner />}
        </div>
        <div className="topbar-actions">
          <button className="primary" onClick={() => setCreating(true)}>
            New receiver
          </button>
        </div>
      </header>

      <div className="content">
        <ErrorBanner error={receivers.error} what="load the alert receivers" />
        <ErrorBanner error={update.error} what="change the alert receiver" />

        <Card tight>
          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th>Name</th>
                  <th>Run</th>
                  <th>Throttle</th>
                  <th>Last notification</th>
                  <th>Enabled</th>
                  <th />
                </tr>
              </thead>
              <tbody>
                {rows.map((receiver) => (
                  <tr key={receiver.id} className="clickable" onClick={() => navigate(`/alert-receivers/${receiver.id}`)}>
                    <td>
                      {receiver.name}
                      {receiver.description && <div className="small muted trunc">{receiver.description}</div>}
                    </td>
                    <td>
                      {receiver.run.role}
                      <div className="small muted trunc trunc-sm">{receiver.run.repo || 'no repository'}</div>
                    </td>
                    <td className="nowrap small">{throttleLabel(receiver.throttle_seconds)}</td>
                    <td>
                      <LastDelivery receiver={receiver} />
                    </td>
                    <td onClick={(e) => e.stopPropagation()}>
                      <input
                        type="checkbox"
                        aria-label={receiver.enabled ? `Disable ${receiver.name}` : `Enable ${receiver.name}`}
                        checked={receiver.enabled}
                        disabled={update.isPending}
                        onChange={(e) => update.mutate({ id: receiver.id, patch: { enabled: e.target.checked } })}
                      />
                    </td>
                    <td className="nowrap" onClick={(e) => e.stopPropagation()}>
                      <button className="sm ghost" onClick={() => setEditing(receiver)}>
                        Edit
                      </button>
                      <button className="sm danger" onClick={() => setDeleting(receiver)}>
                        Delete
                      </button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          {rows.length === 0 && !receivers.isLoading && (
            <Empty>
              No alert receivers yet. A receiver gives Prometheus Alertmanager a webhook that starts a run for each
              notification, with the same alert throttled for a window.
            </Empty>
          )}
          {receivers.isLoading && (
            <Empty>
              <Spinner />
            </Empty>
          )}
        </Card>
      </div>

      {creating && (
        <AlertReceiverDialog
          receiver={null}
          roles={roleNames}
          onClose={() => setCreating(false)}
          onCreated={(created) => {
            setCreating(false)
            setMinted(created)
          }}
        />
      )}
      {minted && <CredentialDialog receiver={minted} onClose={() => setMinted(null)} />}
      {editing && <AlertReceiverDialog receiver={editing} roles={roleNames} onClose={() => setEditing(null)} />}
      {deleting && <DeleteAlertReceiverDialog receiver={deleting} onClose={() => setDeleting(null)} />}
    </>
  )
}
