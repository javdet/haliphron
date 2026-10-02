import { useState } from 'react'
import { Link, NavLink, Route, Routes, useLocation } from 'react-router-dom'
import { useGitCredential, useModelCredential } from './api/hooks'
import { gitProblems } from './api/gitToken'
import { RunsPage } from './pages/RunsPage'
import { RunDetailPage } from './pages/RunDetailPage'
import { ClustersPage } from './pages/ClustersPage'
import { StatsPage } from './pages/StatsPage'
import { RolesPage } from './pages/RolesPage'
import { TokensPage } from './pages/TokensPage'
import { SecretsPage } from './pages/SecretsPage'
import { useSignOut } from './auth/TokenGate'
import { ThemeToggle } from './components/ThemeToggle'

function Sidebar() {
  const signOut = useSignOut()
  return (
    <aside className="sidebar">
      <div className="brand">
        <span className="brand-mark">H</span>
        <span className="brand-name">Haliphron</span>
      </div>

      <nav className="nav">
        <NavLink to="/runs">Runs</NavLink>
        <NavLink to="/clusters">Clusters</NavLink>
        <NavLink to="/stats">Statistics</NavLink>
        <div className="nav-group">
          <h3>Configuration</h3>
        </div>
        <NavLink to="/roles">Roles</NavLink>
        <NavLink to="/secrets">Secrets</NavLink>
        <NavLink to="/tokens">Tokens</NavLink>
      </nav>

      <div className="sidebar-foot">
        <ThemeToggle />
        <button className="ghost sm" onClick={signOut}>
          Sign out
        </button>
      </div>
    </aside>
  )
}

/**
 * Shown on every page while the model credential is missing or unusable: every
 * run fails in the pod without it, and the failure surfaces minutes later on a
 * run, far from the page where it is fixed.
 */
function ModelCredentialWarning() {
  const cred = useModelCredential()
  const { pathname } = useLocation()
  const c = cred.data
  // The Secrets page carries the full card; a second banner there is noise.
  if (!c || (c.configured && !c.problem) || pathname === '/secrets') return null
  return (
    <div className="content-banner">
      <div className="banner" role="alert">
        <div>
          <div className="banner-title">
            {c.configured ? 'The model credential cannot reach runs' : 'No model credential is set'}
          </div>
          <div className="small muted">
            {c.problem ?? 'Every run will fail before the agent starts.'}{' '}
            <Link to="/secrets">Set it on the Secrets page</Link>
          </div>
        </div>
      </div>
    </div>
  )
}

const GIT_PROMPT_DISMISSED = 'haliphron.gitTokenPrompt.dismissed'

function readDismissed(): boolean {
  try {
    return window.localStorage.getItem(GIT_PROMPT_DISMISSED) === '1'
  } catch {
    return false
  }
}

/**
 * Asks for a git token while none is stored. The backend admits a run without
 * one, because a public repository without a pull request needs none, so
 * nothing else says it is missing until a run fails at clone or push. Most
 * installations do need one; the few that do not can dismiss this, in this
 * browser. A stored token that leases cannot use is not dismissible.
 */
function GitCredentialPrompt() {
  const cred = useGitCredential()
  const { pathname } = useLocation()
  const [dismissed, setDismissed] = useState(readDismissed)
  const c = cred.data
  if (!c || pathname === '/secrets') return null

  const problems = gitProblems(c)
  if (problems.length > 0) {
    return (
      <div className="content-banner">
        <div className="banner" role="alert">
          <div>
            <div className="banner-title">A stored git token cannot reach runs</div>
            <div className="small muted">
              {problems.join(' · ')} <Link to="/secrets">Fix it on the Secrets page</Link>
            </div>
          </div>
        </div>
      </div>
    )
  }
  if (c.configured || dismissed) return null

  const dismiss = () => {
    setDismissed(true)
    try {
      window.localStorage.setItem(GIT_PROMPT_DISMISSED, '1')
    } catch {
      // Not remembered past this tab; that is all.
    }
  }
  return (
    <div className="content-banner">
      <div className="banner warn" role="status">
        <div style={{ flex: 1 }}>
          <div className="banner-title">Set a git token</div>
          <div className="small muted">
            No git token is stored, so runs cannot clone a private repository or open a pull request.{' '}
            <Link to="/secrets">Add one on the Secrets page</Link>
          </div>
        </div>
        <button className="sm ghost" onClick={dismiss}>
          Not needed
        </button>
      </div>
    </div>
  )
}

export default function App() {
  return (
    <div className="shell">
      <Sidebar />
      <div className="main">
        <ModelCredentialWarning />
        <GitCredentialPrompt />
        <Routes>
          <Route path="/" element={<RunsPage />} />
          <Route path="/runs" element={<RunsPage />} />
          <Route path="/runs/:id" element={<RunDetailPage />} />
          <Route path="/clusters" element={<ClustersPage />} />
          <Route path="/stats" element={<StatsPage />} />
          <Route path="/roles" element={<RolesPage />} />
          <Route path="/secrets" element={<SecretsPage />} />
          <Route path="/tokens" element={<TokensPage />} />
          <Route
            path="*"
            element={
              <div className="content">
                <div className="empty">No such page.</div>
              </div>
            }
          />
        </Routes>
      </div>
    </div>
  )
}
