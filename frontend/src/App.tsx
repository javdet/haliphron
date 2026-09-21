import { NavLink, Route, Routes } from 'react-router-dom'
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

export default function App() {
  return (
    <div className="shell">
      <Sidebar />
      <div className="main">
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
