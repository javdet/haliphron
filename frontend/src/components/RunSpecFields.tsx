import { Link } from 'react-router-dom'
import { useGitCredential } from '../api/hooks'
import { FORGE_LABEL, hasUsableToken, providerFor } from '../api/gitToken'
import { AGENT_TYPES, type RunTemplate } from '../api/types'
import { Field } from './ui'

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
  set: <K extends keyof RunTemplate>(key: K, value: RunTemplate[K]) => void
  roles: string[]
}) {
  const git = useGitCredential().data
  const repo = form.repo?.trim() ?? ''
  // Only once the answer is in: a warning that flashes while it loads is noise.
  const noToken = !!repo && !!git && !hasUsableToken(git, repo)
  const forge = providerFor(repo)

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
        <Field label="Repository" hint="https:// or git@ — cloned before the agent starts.">
          <input value={form.repo ?? ''} onChange={(e) => set('repo', e.target.value)} />
        </Field>
        <Field label="Base branch">
          <input value={form.base_branch ?? ''} onChange={(e) => set('base_branch', e.target.value)} />
        </Field>
        <Field label="Target branch" hint="Where the work is pushed.">
          <input value={form.target_branch ?? ''} onChange={(e) => set('target_branch', e.target.value)} />
        </Field>
      </div>

      {noToken && (
        <div className="banner warn" role="status">
          <div>
            <div className="banner-title">
              No git token{forge ? ` for ${FORGE_LABEL[forge]}` : ''} will be handed to this run
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
  for (const key of Object.keys(body) as (keyof T)[]) {
    if (body[key] === '' || body[key] === undefined) delete body[key]
  }
  return body
}
