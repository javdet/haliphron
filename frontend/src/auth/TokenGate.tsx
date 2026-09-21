// Sign-in, such as it is.
//
// The backend has no OIDC and no session: the only credential the public API
// accepts is a bearer token with scopes. So the gate asks for one, checks it
// against a cheap authenticated read, and keeps it in localStorage. When an
// identity provider does arrive this is the one component that has to change.

import { useEffect, useState, type ReactNode } from 'react'
import { ApiError, clearToken, getToken, request, setToken } from '../api/client'
import { Field } from '../components/ui'

type State = 'checking' | 'locked' | 'open'

export function useSignOut() {
  return () => {
    clearToken()
    window.location.reload()
  }
}

async function verify(): Promise<boolean> {
  try {
    // Any authenticated read will do; a one-row listing is the cheapest, and
    // admin implies runs:read, so no token that can use this UI is refused.
    await request('/runs', { query: { limit: 1 } })
    return true
  } catch (err) {
    if (err instanceof ApiError && (err.unauthenticated || err.forbidden)) return false
    // A backend that is down is not a bad token; let the app open and show
    // the real error where the data would be.
    return true
  }
}

export function TokenGate({ children }: { children: ReactNode }) {
  const [state, setState] = useState<State>(getToken() ? 'checking' : 'locked')
  const [value, setValue] = useState('')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)

  useEffect(() => {
    if (state !== 'checking') return
    let cancelled = false
    void verify().then((ok) => {
      if (cancelled) return
      if (!ok) clearToken()
      setState(ok ? 'open' : 'locked')
    })
    return () => {
      cancelled = true
    }
  }, [state])

  // A token that stops working mid-session — revoked, expired — should put the
  // gate back rather than paint 401s over every panel.
  useEffect(() => {
    const onUnauthorized = () => {
      clearToken()
      setState('locked')
      setError('The token is no longer accepted. Paste a current one.')
    }
    window.addEventListener('haliphron:unauthenticated', onUnauthorized)
    return () => window.removeEventListener('haliphron:unauthenticated', onUnauthorized)
  }, [])

  if (state === 'open') return <>{children}</>

  if (state === 'checking') {
    return (
      <div className="gate">
        <span className="spinner" />
      </div>
    )
  }

  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    const trimmed = value.trim()
    if (!trimmed) {
      setError('Paste a token.')
      return
    }
    setBusy(true)
    setError('')
    setToken(trimmed)
    const ok = await verify()
    setBusy(false)
    if (ok) {
      setValue('')
      setState('open')
      return
    }
    clearToken()
    setError('That token is not usable. Check that it has not been revoked or expired.')
  }

  return (
    <div className="gate">
      <form className="card" onSubmit={submit}>
        <header className="card-head">
          <div className="brand">
            <span className="brand-mark">H</span>
            <span className="brand-name">Haliphron</span>
          </div>
        </header>
        <div className="card-body stack">
          <p className="small muted" style={{ margin: 0 }}>
            The control plane authenticates with an API token. Create one with{' '}
            <code>POST /api/v1/tokens</code> or the MCP tools, then paste it here. It is kept in
            this browser only.
          </p>
          <Field label="API token" error={error} hint="Scopes: runs:read, runs:write, admin.">
            <input
              type="password"
              value={value}
              autoComplete="off"
              spellCheck={false}
              placeholder="hlt_…"
              onChange={(e) => setValue(e.target.value)}
            />
          </Field>
        </div>
        <footer className="dialog-foot">
          <button className="primary" type="submit" disabled={busy}>
            {busy ? 'Checking…' : 'Continue'}
          </button>
        </footer>
      </form>
    </div>
  )
}
