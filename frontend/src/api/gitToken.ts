// Which git token a run would be handed. The backend decides this at lease
// time — ProviderFor in backend/run/spec.go and StoredGitToken in
// backend/app/materials.go — and this is the same rule, transcribed, so the UI
// can say before a run is submitted that it will fail at clone or push.

import type { GitCredential, GitToken, GitTokenProvider } from './types'

/** The forge a clone URL belongs to, by the backend's guess; undefined when it has no name of its own. */
export function providerFor(repo: string): GitTokenProvider | undefined {
  const host = repo.toLowerCase()
  if (host.includes('github.')) return 'github'
  if (host.includes('gitlab.')) return 'gitlab'
  return undefined
}

/**
 * The token a lease resolves for a forge: its own when one is stored, the
 * fallback otherwise. A stored token with a problem is still the one chosen —
 * the lease fails on it rather than trying the next name.
 */
export function tokenFor(cred: GitCredential, provider?: GitTokenProvider): GitToken | undefined {
  const own = provider && cred.tokens.find((t) => t.provider === provider && t.configured)
  if (own) return own
  const fallback = cred.tokens.find((t) => !t.provider)
  return fallback?.configured ? fallback : undefined
}

/** Whether a run against this repository would be handed a usable token. */
export function hasUsableToken(cred: GitCredential, repo: string): boolean {
  const token = tokenFor(cred, providerFor(repo))
  return !!token && !token.problem
}

/** The stored tokens a lease would fail on, and the installation-wide problem if there is one. */
export function gitProblems(cred: GitCredential): string[] {
  const out = cred.problem ? [cred.problem] : []
  for (const t of cred.tokens) if (t.problem) out.push(`${t.secret_name}: ${t.problem}`)
  return out
}

export const FORGE_LABEL: Record<GitTokenProvider, string> = { github: 'GitHub', gitlab: 'GitLab' }

/** The forge a pasted token's documented prefix names; undefined for anything unrecognised. */
export function forgeOf(value: string): GitTokenProvider | undefined {
  const v = value.trim()
  if (/^(ghp_|gho_|ghu_|ghs_|ghr_|github_pat_)/.test(v)) return 'github'
  if (v.startsWith('glpat-')) return 'gitlab'
  return undefined
}
