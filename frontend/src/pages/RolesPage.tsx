import { useState } from 'react'
import { useDeleteRole, usePutRole, useRoles } from '../api/hooks'
import { Card, Dialog, Empty, ErrorBanner, Field, Spinner, Time } from '../components/ui'
import type { Role } from '../api/types'

const TEMPLATE = `{
  "agent": "claude-code",
  "model": "",
  "system_prompt": "",
  "allowed_tools": [],
  "mcp_servers": []
}`

/**
 * The role editor is a JSON editor on purpose.
 *
 * The backend stores a role's spec as an opaque document and replaces it
 * whole, and the product's idea of what belongs in one is still moving. A form
 * with a field per key would be a second, private schema that goes out of date
 * silently; a validated document editor goes out of date loudly, at the
 * backend's own validation.
 */
function RoleDialog({ role, onClose }: { role: Role | null; onClose: () => void }) {
  const put = usePutRole()
  const [name, setName] = useState(role?.name ?? '')
  const [text, setText] = useState(() =>
    role ? JSON.stringify(role.spec, null, 2) : TEMPLATE,
  )
  const [invalid, setInvalid] = useState('')

  const save = () => {
    let spec: unknown
    try {
      spec = JSON.parse(text)
    } catch (err) {
      setInvalid(err instanceof Error ? err.message : 'not valid JSON')
      return
    }
    if (typeof spec !== 'object' || spec === null || Array.isArray(spec)) {
      setInvalid('a role spec is a JSON object')
      return
    }
    setInvalid('')
    put.mutate({ name: name.trim(), spec }, { onSuccess: onClose })
  }

  return (
    <Dialog
      title={role ? `Edit ${role.name}` : 'New role'}
      onClose={onClose}
      footer={
        <>
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
      <Field label="Spec" error={invalid} hint="Replaced whole on save.">
        <textarea rows={18} value={text} spellCheck={false} onChange={(e) => setText(e.target.value)} />
      </Field>
    </Dialog>
  )
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
