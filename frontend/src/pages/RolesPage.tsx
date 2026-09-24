import { useState } from 'react'
import { useDeleteRole, usePutRole, useRoles } from '../api/hooks'
import { Card, Dialog, Empty, ErrorBanner, Field, Spinner, Time } from '../components/ui'
import { merge, RawSpecEditor, RoleForm, split, useRoleForm } from '../components/RoleForm'
import type { Role, RoleSpec } from '../api/types'

/**
 * The role editor: a form for what this build understands, and a raw editor for
 * everything else.
 *
 * It used to be a raw editor only, and the reasoning was sound — a form with a
 * field per key is a second, private schema that goes out of date silently. The
 * reason it is a form now is that the template it offered had been wrong for
 * some time: it suggested `allowed_tools` and `mcp_servers`, which are not
 * fields of a role spec, and the backend accepted them with a 200 and discarded
 * them. A document editor only fails loudly if something reads the document
 * strictly, and nothing did.
 *
 * What makes the form safe is that it does not own the document: `split` keeps
 * every key it cannot render and `merge` puts them back, so a role written for
 * a newer control plane survives an edit here untouched.
 */
function RoleDialog({ role, onClose }: { role: Role | null; onClose: () => void }) {
  const put = usePutRole()
  const [name, setName] = useState(role?.name ?? '')
  const { form, set, replace } = useRoleForm(role?.spec)
  const [raw, setRaw] = useState(false)
  const [text, setText] = useState('')
  const [invalid, setInvalid] = useState('')

  // Switching to the raw editor renders what the form currently holds, so the
  // two views never disagree about what would be saved.
  const toRaw = () => {
    setText(JSON.stringify(merge(form), null, 2))
    setInvalid('')
    setRaw(true)
  }

  const toForm = () => {
    const parsed = parseSpec(text)
    if (typeof parsed === 'string') {
      setInvalid(parsed)
      return
    }
    replace(split(parsed))
    setInvalid('')
    setRaw(false)
  }

  const save = () => {
    if (!raw) {
      setInvalid('')
      put.mutate({ name: name.trim(), spec: merge(form) }, { onSuccess: onClose })
      return
    }
    const parsed = parseSpec(text)
    if (typeof parsed === 'string') {
      setInvalid(parsed)
      return
    }
    setInvalid('')
    put.mutate({ name: name.trim(), spec: parsed }, { onSuccess: onClose })
  }

  return (
    <Dialog
      title={role ? `Edit ${role.name}` : 'New role'}
      onClose={onClose}
      footer={
        <>
          <button className="ghost" onClick={raw ? toForm : toRaw}>
            {raw ? 'Back to the form' : 'Edit as JSON'}
          </button>
          <span className="spacer" />
          <button className="ghost" onClick={onClose}>
            Cancel
          </button>
          <button className="primary" onClick={save} disabled={!name.trim() || put.isPending}>
            {put.isPending ? 'Saving…' : 'Save role'}
          </button>
        </>
      }
    >
      <ErrorBanner error={put.error} what="save the role" />
      <Field label="Name" hint={role ? 'Renaming creates a second role.' : 'Referenced by runs as role=…'}>
        <input value={name} onChange={(e) => setName(e.target.value)} disabled={!!role} />
      </Field>
      {raw ? (
        <RawSpecEditor text={text} onChange={setText} error={invalid} />
      ) : (
        <RoleForm form={form} set={set} />
      )}
    </Dialog>
  )
}

/** Parses the raw editor's text, returning a message instead of throwing. */
function parseSpec(text: string): RoleSpec | string {
  let parsed: unknown
  try {
    parsed = JSON.parse(text)
  } catch (err) {
    return err instanceof Error ? err.message : 'not valid JSON'
  }
  if (typeof parsed !== 'object' || parsed === null || Array.isArray(parsed)) {
    return 'a role spec is a JSON object'
  }
  return parsed as RoleSpec
}

function DeleteDialog({ role, onClose }: { role: Role; onClose: () => void }) {
  const remove = useDeleteRole()
  return (
    <Dialog
      title={`Delete ${role.name}`}
      onClose={onClose}
      footer={
        <>
          <button className="ghost" onClick={onClose}>
            Keep it
          </button>
          <button
            className="danger"
            disabled={remove.isPending}
            onClick={() => remove.mutate(role.name, { onSuccess: onClose })}
          >
            {remove.isPending ? 'Deleting…' : 'Delete role'}
          </button>
        </>
      }
    >
      <ErrorBanner error={remove.error} what="delete the role" />
      <p className="small muted" style={{ margin: 0 }}>
        A soft delete. Runs admitted under this role keep naming it: their specs are frozen, and
        the role is part of what they froze.
      </p>
    </Dialog>
  )
}

export function RolesPage() {
  const roles = useRoles()
  const [editing, setEditing] = useState<Role | null>(null)
  const [creating, setCreating] = useState(false)
  const [deleting, setDeleting] = useState<Role | null>(null)

  return (
    <>
      <header className="topbar">
        <div className="topbar-title">
          <h1>Roles</h1>
          {roles.isFetching && <Spinner />}
        </div>
        <div className="topbar-actions">
          <button className="primary" onClick={() => setCreating(true)}>
            New role
          </button>
        </div>
      </header>

      <div className="content">
        <ErrorBanner error={roles.error} what="load the roles" />
        <Card tight>
          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th>Name</th>
                  <th>Spec</th>
                  <th>Updated</th>
                  <th />
                </tr>
              </thead>
              <tbody>
                {(roles.data ?? []).map((role) => (
                  <tr key={role.name}>
                    <td>{role.name}</td>
                    <td className="mono small muted">
                      <span className="trunc">{JSON.stringify(role.spec)}</span>
                    </td>
                    <td className="small muted">
                      <Time at={role.updated_at} />
                      <div className="faint">{role.created_by}</div>
                    </td>
                    <td className="nowrap">
                      <button className="sm ghost" onClick={() => setEditing(role)}>
                        Edit
                      </button>
                      <button className="sm danger" onClick={() => setDeleting(role)}>
                        Delete
                      </button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          {(roles.data?.length ?? 0) === 0 && !roles.isLoading && (
            <Empty>No role is defined. Runs without one take the platform default.</Empty>
          )}
        </Card>
      </div>

      {(creating || editing) && (
        <RoleDialog
          role={editing}
          onClose={() => {
            setCreating(false)
            setEditing(null)
          }}
        />
      )}
      {deleting && <DeleteDialog role={deleting} onClose={() => setDeleting(null)} />}
    </>
  )
}
