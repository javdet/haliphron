import { Link, NavLink, Route, Routes, useLocation } from 'react-router-dom'
import { useModelCredential } from './api/hooks'
import { RunsPage } from './pages/RunsPage'
import { RunDetailPage } from './pages/RunDetailPage'
import { ClustersPage } from './pages/ClustersPage'
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

export default function App() {
  return (
    <div className="shell">
      <Sidebar />
      <div className="main">
        <ModelCredentialWarning />
        <Routes>
          <Route path="/" element={<RunsPage />} />
          <Route path="/runs" element={<RunsPage />} />
          <Route path="/runs/:id" element={<RunDetailPage />} />
          <Route path="/clusters" element={<ClustersPage />} />
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
