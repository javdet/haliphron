import { Link } from 'react-router-dom'
import { useGitCredential, useRunLimits } from '../api/hooks'
import { FORGE_LABEL, hasUsableToken, providerFor } from '../api/gitToken'
import { AGENT_TYPES, type RepoRequest, type RunTemplate } from '../api/types'
import { Field } from './ui'

type Setter = <K extends keyof RunTemplate>(key: K, value: RunTemplate[K]) => void

/**
 * The working repository and the ones cloned beside it, read from either shape
 * a template comes in: repo/base_branch for one, repos for several.
 */
function reposOf(form: RunTemplate): { working: RepoRequest; extras: RepoRequest[] } {
  if (form.repos && form.repos.length > 0) {
    const [first, ...extras] = form.repos
    return { working: { url: first.url, base_branch: first.base_branch }, extras }
  }
  return { working: { url: form.repo ?? '', base_branch: form.base_branch }, extras: [] }
}

/**
 * Writes the list back in the shape the backend wants: repo/base_branch while
 * there is one repository, repos — and neither of the others — once there are
 * more. The two are exclusive; sending both is a 422.
 */
function writeRepos(set: Setter, working: RepoRequest, extras: RepoRequest[]) {
  if (extras.length === 0) {
    set('repos', undefined)
    set('repo', working.url)
    set('base_branch', working.base_branch)
    return
  }
  set('repo', undefined)
  set('base_branch', undefined)
  set('repos', [{ url: working.url, base_branch: working.base_branch }, ...extras])
}

/**
 * The fields of a run: what the "New run" dialog submits once and what a
 * schedule submits on every fire. One block, so the two cannot drift apart in
 * what they let an operator ask for.
 */
export function RunSpecFields<T extends RunTemplate>({
  form,
  set,
  roles,
}: {
  form: T
  set: Setter
  roles: string[]
}) {
  const git = useGitCredential().data
  const limit = useRunLimits().data?.max_repos_per_run
  const { working, extras } = reposOf(form)
  const urls = [working.url, ...extras.map((r) => r.url)].map((u) => u.trim()).filter(Boolean)
  // Only once the answer is in: a warning that flashes while it loads is noise.
  const tokenless = git ? urls.filter((u) => !hasUsableToken(git, u)) : []
  const noToken = tokenless.length > 0
  const forge = noToken ? providerFor(tokenless[0]) : undefined

  const count = 1 + extras.length
  // Without the limit in hand the backend is the one to refuse.
  const atLimit = limit !== undefined && count >= limit
  const overLimit = limit !== undefined && count > limit
  const setWorking = (next: RepoRequest) => writeRepos(set, next, extras)
  const setExtra = (index: number, next: RepoRequest) =>
    writeRepos(set, working, extras.map((r, i) => (i === index ? next : r)))
  const removeExtra = (index: number) => writeRepos(set, working, extras.filter((_, i) => i !== index))
  const addExtra = () => writeRepos(set, working, [...extras, { url: '' }])
  const workingMissing = extras.some((r) => r.url.trim()) && !working.url.trim()

  return (
    <>
      <Field label="Prompt" hint="What the agent is asked to do.">
        <textarea rows={6} value={form.prompt} onChange={(e) => set('prompt', e.target.value)} />
      </Field>

      <div className="grid-2">
        <Field label="Agent">
          <select value={form.agent ?? ''} onChange={(e) => set('agent', e.target.value)}>
            {AGENT_TYPES.map((agent) => (
              <option key={agent} value={agent}>
                {agent}
              </option>
            ))}
          </select>
        </Field>
        <Field label="Role" hint="Decides the tools and skills the agent gets.">
          <select value={form.role ?? ''} onChange={(e) => set('role', e.target.value)}>
            <option value="">Default</option>
            {roles.map((role) => (
              <option key={role} value={role}>
                {role}
              </option>
            ))}
          </select>
        </Field>
        <Field label="Model" hint="Empty takes the role's default.">
          <input value={form.model ?? ''} onChange={(e) => set('model', e.target.value)} />
        </Field>
        <Field
          label={extras.length > 0 ? 'Working repository' : 'Repository'}
          error={workingMissing ? 'The other repositories are cloned beside this one; it is needed.' : undefined}
          hint={
            extras.length > 0
              ? 'Cloned to /workspace, where the agent starts.'
              : 'https:// or git@ — cloned before the agent starts.'
          }
        >
          <input value={working.url} onChange={(e) => setWorking({ ...working, url: e.target.value })} />
        </Field>
        <Field label="Base branch">
          <input
            value={working.base_branch ?? ''}
            onChange={(e) => setWorking({ ...working, base_branch: e.target.value })}
          />
        </Field>
        <Field
          label="Target branch"
          hint={extras.length > 0 ? 'Where the work is pushed, in every repository.' : 'Where the work is pushed.'}
        >
          <input value={form.target_branch ?? ''} onChange={(e) => set('target_branch', e.target.value)} />
        </Field>
      </div>

      <Field
        label="Other repositories"
        error={
          overLimit
            ? `This installation allows ${limit} repositor${limit === 1 ? 'y' : 'ies'} per run, the working one included; remove ${count - limit!} to save.`
            : undefined
        }
        hint={
          'Cloned to /repos/<name> beside the working repository. Name defaults to the URL’s last segment; depth to 1, and 0 clones the full history.' +
          (limit !== undefined
            ? ` At most ${limit} repositor${limit === 1 ? 'y' : 'ies'} per run, the working one included — set on the Settings page.`
            : '')
        }
      >
        <div className="stack" style={{ gap: 8 }}>
          {extras.map((row, index) => (
            <div key={index} className="rowset">
              <div className="rowset-fields">
                <input
                  value={row.url}
                  placeholder="https://… or git@…"
                  aria-label={`Repository ${index + 2} URL`}
                  onChange={(e) => setExtra(index, { ...row, url: e.target.value })}
                />
                <input
                  value={row.base_branch ?? ''}
                  placeholder="base branch"
                  aria-label={`Repository ${index + 2} base branch`}
                  onChange={(e) => setExtra(index, { ...row, base_branch: e.target.value })}
                />
                <input
                  value={row.name ?? ''}
                  placeholder="name (optional)"
                  aria-label={`Repository ${index + 2} directory name`}
                  onChange={(e) => setExtra(index, { ...row, name: e.target.value })}
                />
                <input
                  type="number"
                  min={0}
                  step={1}
                  value={row.depth ?? ''}
                  placeholder="depth (1)"
                  aria-label={`Repository ${index + 2} clone depth`}
                  onChange={(e) =>
                    setExtra(index, { ...row, depth: e.target.value === '' ? undefined : Number(e.target.value) })
                  }
                />
              </div>
              <button
                type="button"
                className="ghost sm"
                aria-label={`Remove repository ${index + 2}`}
                onClick={() => removeExtra(index)}
              >
                ✕
              </button>
            </div>
          ))}
          <div className="inline">
            <button type="button" className="ghost sm" onClick={addExtra} disabled={atLimit}>
              Add repository
            </button>
            {atLimit && !overLimit && (
              <span className="small muted">
                The limit of {limit} repositor{limit === 1 ? 'y' : 'ies'} per run is reached.
              </span>
            )}
          </div>
        </div>
      </Field>

      {noToken && (
        <div className="banner warn" role="status">
          <div>
            <div className="banner-title">
              No git token{forge ? ` for ${FORGE_LABEL[forge]}` : ''} will be handed to this run
              {urls.length > 1 ? ` for ${tokenless.length === 1 ? tokenless[0] : `${tokenless.length} of its repositories`}` : ''}
            </div>
            <div className="small muted">
              {form.create_pr
                ? 'It cannot push its branch or open the pull request, and a private repository fails at clone.'
                : 'A public repository still works; a private one fails at clone.'}{' '}
              <Link to="/secrets">Set a git token</Link>
            </div>
          </div>
        </div>
      )}

      <label className={form.create_pr ? 'check on' : 'check'} style={{ alignSelf: 'flex-start' }}>
        <input
          type="checkbox"
          checked={form.create_pr ?? false}
          onChange={(e) => set('create_pr', e.target.checked)}
        />
        Open a pull request when it finishes
      </label>

      <div className="grid-3">
        <Field label="Timeout (s)">
          <input
            type="number"
            min={0}
            value={form.timeout_seconds ?? ''}
            onChange={(e) => set('timeout_seconds', Number(e.target.value) || undefined)}
          />
        </Field>
        <Field label="Max cost (USD)">
          <input
            value={form.max_cost_usd ?? ''}
            placeholder="5.00"
            onChange={(e) => set('max_cost_usd', e.target.value)}
          />
        </Field>
        <Field label="Max turns">
          <input
            type="number"
            min={0}
            value={form.max_turns ?? ''}
            onChange={(e) => set('max_turns', Number(e.target.value) || undefined)}
          />
        </Field>
      </div>
    </>
  )
}

/**
 * Drops the fields left empty. Empty strings are not "unset" to a Go handler
 * that only omits on empty; stripping them lets the backend apply its own
 * defaults.
 */
export function compact<T extends RunTemplate>(form: T): T {
  const body: T = { ...form, prompt: form.prompt.trim() }
  if (body.repos) {
    const [working, ...rest] = body.repos.map(compactRepo)
    // An extra row left blank is not a repository; dropped, it does not cost a 422.
    const extras = rest.filter((r) => r.url)
    if (extras.length === 0) {
      body.repo = working?.url
      body.base_branch = working?.base_branch
      delete body.repos
    } else {
      // repos[0] is /workspace: it never carries a name or a depth.
      body.repos = [{ url: working.url, ...(working.base_branch ? { base_branch: working.base_branch } : {}) }, ...extras]
      delete body.repo
      delete body.base_branch
    }
  }
  for (const key of Object.keys(body) as (keyof T)[]) {
    if (body[key] === '' || body[key] === undefined) delete body[key]
  }
  return body
}

function compactRepo(r: RepoRequest): RepoRequest {
  const out: RepoRequest = { url: r.url.trim() }
  const base = r.base_branch?.trim()
  const name = r.name?.trim()
  if (base) out.base_branch = base
  if (name) out.name = name
  if (r.depth !== undefined && Number.isInteger(r.depth) && r.depth >= 0) out.depth = r.depth
  return out
}

/** The working repository a template names, in whichever shape it is stored. */
export function workingRepo(t: RunTemplate): string {
  return t.repos?.[0]?.url || t.repo || ''
}

/** How many repositories a template clones beside the working one. */
export function extraRepoCount(t: RunTemplate): number {
  return Math.max(0, (t.repos?.length ?? 0) - 1)
}

/** Where the backend clones an extra repository: its name, or the URL's last segment. */
function extraDir(r: RepoRequest): string {
  if (r.name) return r.name
  const last = r.url.replace(/\/+$/, '').split(/[/:]/).pop() ?? ''
  return last.replace(/\.git$/, '')
}

/**
 * A template's repositories, for the read-only views of a schedule or an alert
 * receiver: the working one, then each one cloned beside it.
 */
export function TemplateRepos({ template }: { template: RunTemplate }) {
  const { working, extras } = reposOf(template)
  if (!working.url) return <span className="faint">none</span>
  return (
    <div className="stack" style={{ gap: 4 }}>
      <div>
        {working.url}
        {working.base_branch && <span className="mono faint"> @ {working.base_branch}</span>}
        {extras.length > 0 && <span className="faint"> — /workspace</span>}
      </div>
      {extras.map((r, i) => (
        <div key={i}>
          {r.url}
          {r.base_branch && <span className="mono faint"> @ {r.base_branch}</span>}
          <span className="faint">
            {' '}
            — /repos/{extraDir(r)}
            {r.depth === 0 ? ', full clone' : r.depth && r.depth > 1 ? `, depth ${r.depth}` : ''}
          </span>
        </div>
      ))}
    </div>
  )
}
