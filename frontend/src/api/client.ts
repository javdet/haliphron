// The transport. One place that knows about the bearer token, the error
// envelope and the two kinds of artifact URL.

import type { ErrorBody } from './types'

/** Same origin by default: nginx serves this bundle and proxies /api. */
const BASE = (import.meta.env.VITE_API_BASE ?? '').replace(/\/$/, '')
const PREFIX = `${BASE}/api/v1`

const TOKEN_KEY = 'haliphron.token'

/** An answer the API refused, carrying the envelope's code and field. */
export class ApiError extends Error {
  readonly status: number
  readonly code: string
  readonly field?: string

  constructor(status: number, code: string, message: string, field?: string) {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.code = code
    this.field = field
  }

  get unauthenticated(): boolean {
    return this.status === 401
  }

  get forbidden(): boolean {
    return this.status === 403
  }
}

// Storage can throw — a private window, blocked site data — and a token that
// cannot be remembered is still a token that works for this tab.
let memoryToken = ''

export function getToken(): string {
  if (memoryToken) return memoryToken
  try {
    return window.localStorage.getItem(TOKEN_KEY) ?? ''
  } catch {
    return ''
  }
}

export function setToken(token: string): void {
  memoryToken = token
  try {
    window.localStorage.setItem(TOKEN_KEY, token)
  } catch {
    /* kept in memory for this tab only */
  }
}

export function clearToken(): void {
  memoryToken = ''
  try {
    window.localStorage.removeItem(TOKEN_KEY)
  } catch {
    /* nothing to forget */
  }
}

interface RequestOptions {
  method?: string
  body?: unknown
  /** Sent on run creation so a double-submit cannot become two runs. */
  idempotencyKey?: string
  query?: Record<string, string | number | boolean | string[] | undefined>
  signal?: AbortSignal
}

function withQuery(path: string, query: RequestOptions['query']): string {
  if (!query) return `${PREFIX}${path}`
  const params = new URLSearchParams()
  for (const [key, value] of Object.entries(query)) {
    if (value === undefined || value === '') continue
    // Repeated rather than comma-joined: the backend reads query["status"] as
    // a list, and a comma would arrive as one status nobody has.
    if (Array.isArray(value)) value.forEach((v) => v !== '' && params.append(key, v))
    else params.set(key, String(value))
  }
  const qs = params.toString()
  return qs ? `${PREFIX}${path}?${qs}` : `${PREFIX}${path}`
}

async function toError(response: Response): Promise<ApiError> {
  let code = 'unknown'
  let message = response.statusText || `HTTP ${response.status}`
  let field: string | undefined
  try {
    const body = (await response.json()) as Partial<ErrorBody>
    if (body?.error) {
      code = body.error.code ?? code
      message = body.error.message ?? message
      field = body.error.field || undefined
    }
  } catch {
    /* a body that is not the envelope: the status is all there is */
  }
  const error = new ApiError(response.status, code, message, field)
  // One 401 anywhere means the credential is gone, not that one panel failed.
  // The gate listens for this and asks for a token again.
  if (error.unauthenticated) {
    window.dispatchEvent(new CustomEvent('haliphron:unauthenticated'))
  }
  return error
}

function authHeaders(): Record<string, string> {
  const token = getToken()
  return token ? { Authorization: `Bearer ${token}` } : {}
}

/** A JSON call against the API. Resolves to `undefined` on 204. */
export async function request<T>(path: string, options: RequestOptions = {}): Promise<T> {
  const headers: Record<string, string> = { Accept: 'application/json', ...authHeaders() }
  if (options.body !== undefined) headers['Content-Type'] = 'application/json'
  if (options.idempotencyKey) headers['Idempotency-Key'] = options.idempotencyKey

  const response = await fetch(withQuery(path, options.query), {
    method: options.method ?? 'GET',
    headers,
    body: options.body === undefined ? undefined : JSON.stringify(options.body),
    signal: options.signal,
  })

  if (!response.ok) throw await toError(response)
  if (response.status === 204) return undefined as T
  const text = await response.text()
  return (text ? JSON.parse(text) : undefined) as T
}

/**
 * Fetches a log chunk or a result as text.
 *
 * The bearer token goes to this backend and nowhere else: in object-store mode
 * the URL is a presigned link whose signature covers the headers, and an
 * Authorization header the signature did not expect is how a valid link starts
 * coming back as 403.
 */
export async function fetchArtifact(url: string, signal?: AbortSignal): Promise<string> {
  const sameOrigin = url.startsWith('/')
  const response = await fetch(sameOrigin ? `${BASE}${url}` : url, {
    headers: sameOrigin ? authHeaders() : {},
    signal,
  })
  if (!response.ok) throw await toError(response)
  return response.text()
}

/**
 * Fetches a stored result as text.
 *
 * In object-store mode this endpoint answers 302 to a presigned link. fetch
 * follows it, and the browser drops the Authorization header on the
 * cross-origin hop, which is exactly what the signature wants — so one call
 * works in both artifact modes and neither the caller nor this function has to
 * know which one is on.
 */
export async function fetchResult(runId: string, key?: string, signal?: AbortSignal): Promise<string> {
  const url = withQuery(`/runs/${encodeURIComponent(runId)}/result`, key ? { key } : undefined)
  const response = await fetch(url, { headers: authHeaders(), signal })
  if (!response.ok) throw await toError(response)
  return response.text()
}

/** A client-side Idempotency-Key, so a retried submit is one run. */
export function newIdempotencyKey(): string {
  return crypto.randomUUID()
}
