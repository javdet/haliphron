// The query layer. Every server read is a query key here, so that a mutation
// can say precisely what it invalidated instead of refetching the world.

import {
  useInfiniteQuery,
  useMutation,
  useQuery,
  useQueryClient,
  type UseQueryResult,
} from '@tanstack/react-query'
import { fetchArtifact, fetchResult, newIdempotencyKey, request } from './client'
import {
  TERMINAL_STATUSES,
  type ApiToken,
  type ApiTokenSecret,
  type Attempt,
  type BootstrapToken,
  type BootstrapTokenSecret,
  type Cluster,
  type CreateRunRequest,
  type LogPage,
  type Role,
  type Run,
  type RunList,
  type Secret,
} from './types'

export interface RunFilter {
  status?: string[]
  role?: string
  agent?: string
  cluster_id?: string
  parent_run_id?: string
  q?: string
  limit?: number
}

export const keys = {
  runs: (filter: RunFilter) => ['runs', filter] as const,
  run: (id: string) => ['run', id] as const,
  attempts: (id: string) => ['run', id, 'attempts'] as const,
  logs: (id: string) => ['run', id, 'logs'] as const,
  result: (id: string, key?: string) => ['run', id, 'result', key ?? ''] as const,
  roles: () => ['roles'] as const,
  clusters: () => ['clusters'] as const,
  bootstrapTokens: () => ['bootstrap-tokens'] as const,
  tokens: () => ['tokens'] as const,
  secrets: () => ['secrets'] as const,
}

/** How often a run that has not finished is re-read. */
const LIVE_MS = 4000

export function isLive(run: Run | undefined): boolean {
  return !!run && !TERMINAL_STATUSES.has(run.status)
}

// ---------------------------------------------------------------------------
// runs
// ---------------------------------------------------------------------------

export function useRuns(filter: RunFilter) {
  const limit = filter.limit ?? 50
  return useInfiniteQuery({
    queryKey: keys.runs(filter),
    initialPageParam: undefined as string | undefined,
    queryFn: ({ pageParam, signal }) =>
      request<RunList>('/runs', {
        signal,
        query: { ...filter, limit, before: pageParam },
      }),
    // The backend omits next_before on a short page, which is how a listing
    // says it has reached the end.
    getNextPageParam: (last) => last.next_before,
    // A run list is a dashboard: it is stale the moment it is drawn.
    refetchInterval: LIVE_MS,
  })
}

export function useRun(id: string): UseQueryResult<Run> {
  return useQuery({
    queryKey: keys.run(id),
    queryFn: ({ signal }) => request<Run>(`/runs/${encodeURIComponent(id)}`, { signal }),
    // Polling stops the moment the run is terminal: a finished run does not
    // change, and a tab left open on one should not keep a cluster's worth of
    // operators' browsers talking to the control plane.
    refetchInterval: (query) => (isLive(query.state.data) ? LIVE_MS : false),
  })
}

export function useAttempts(id: string, live: boolean) {
  return useQuery({
    queryKey: keys.attempts(id),
    queryFn: ({ signal }) =>
      request<{ attempts: Attempt[] }>(`/runs/${encodeURIComponent(id)}/attempts`, { signal }).then(
        (r) => r.attempts,
      ),
    refetchInterval: live ? LIVE_MS : false,
  })
}

/**
 * A run's log, assembled from its chunks.
 *
 * The listing is paged and the bytes live behind a second request per chunk,
 * so this fetches the page and then the chunks it names. There is no streaming
 * endpoint; while the run is live the whole thing is re-read on a timer, which
 * is honest about what the API offers.
 */
export function useLogs(id: string, live: boolean, enabled: boolean) {
  return useQuery({
    queryKey: keys.logs(id),
    enabled,
    queryFn: async ({ signal }) => {
      const page = await request<LogPage>(`/runs/${encodeURIComponent(id)}/logs`, {
        signal,
        query: { limit: 200 },
      })
      const bodies = await Promise.all(
        page.chunks.map((chunk) =>
          fetchArtifact(chunk.url, signal).catch(
            (err: unknown) => `\n[chunk ${chunk.key} could not be read: ${String(err)}]\n`,
          ),
        ),
      )
      return { chunks: page.chunks, text: bodies.join('') }
    },
    refetchInterval: live ? LIVE_MS : false,
  })
}

export function useResult(id: string, key: string | undefined, enabled: boolean) {
  return useQuery({
    queryKey: keys.result(id, key),
    enabled,
    queryFn: ({ signal }) => fetchResult(id, key, signal),
    retry: false,
  })
}

export function useCreateRun() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (body: CreateRunRequest) =>
      request<Run>('/runs', { method: 'POST', body, idempotencyKey: newIdempotencyKey() }),
    onSuccess: (run) => {
      qc.setQueryData(keys.run(run.run_id), run)
      void qc.invalidateQueries({ queryKey: ['runs'] })
    },
  })
}

export function useCancelRun(id: string) {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (reason: string) =>
      request<Run>(`/runs/${encodeURIComponent(id)}/cancel`, {
        method: 'POST',
        body: { reason },
      }),
    onSuccess: (run) => {
      qc.setQueryData(keys.run(id), run)
      void qc.invalidateQueries({ queryKey: ['runs'] })
    },
  })
}

export function useRetryRun(id: string) {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: () => request<Run>(`/runs/${encodeURIComponent(id)}/retry`, { method: 'POST' }),
    onSuccess: (run) => {
      qc.setQueryData(keys.run(id), run)
      void qc.invalidateQueries({ queryKey: keys.attempts(id) })
      void qc.invalidateQueries({ queryKey: ['runs'] })
    },
  })
}

// ---------------------------------------------------------------------------
// roles
// ---------------------------------------------------------------------------

export function useRoles() {
  return useQuery({
    queryKey: keys.roles(),
    queryFn: ({ signal }) =>
      request<{ roles: Role[] }>('/roles', { signal }).then((r) => r.roles),
  })
}

export function usePutRole() {
  const qc = useQueryClient()
  return useMutation({
    // The whole spec every time: the endpoint is a PUT because a role is
    // edited as one document.
    mutationFn: ({ name, spec }: { name: string; spec: unknown }) =>
      request<Role>(`/roles/${encodeURIComponent(name)}`, { method: 'PUT', body: spec }),
    onSuccess: () => void qc.invalidateQueries({ queryKey: keys.roles() }),
  })
}

export function useDeleteRole() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (name: string) =>
      request<void>(`/roles/${encodeURIComponent(name)}`, { method: 'DELETE' }),
    onSuccess: () => void qc.invalidateQueries({ queryKey: keys.roles() }),
  })
}

// ---------------------------------------------------------------------------
// clusters
// ---------------------------------------------------------------------------

export function useClusters() {
  return useQuery({
    queryKey: keys.clusters(),
    queryFn: ({ signal }) =>
      request<{ clusters: Cluster[] }>('/clusters', { signal }).then((r) => r.clusters),
    refetchInterval: 15000,
  })
}

export function useRevokeCluster() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: ({ id, reason }: { id: string; reason: string }) =>
      request<void>(`/clusters/${encodeURIComponent(id)}/revoke`, {
        method: 'POST',
        body: { reason },
      }),
    onSuccess: () => void qc.invalidateQueries({ queryKey: keys.clusters() }),
  })
}

export function useBootstrapTokens() {
  return useQuery({
    queryKey: keys.bootstrapTokens(),
    queryFn: ({ signal }) =>
      request<{ bootstrap_tokens: BootstrapToken[] }>('/clusters/bootstrap-tokens', {
        signal,
      }).then((r) => r.bootstrap_tokens),
  })
}

export function useCreateBootstrapToken() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (body: { name: string; ttl_seconds?: number; max_uses?: number }) =>
      request<BootstrapTokenSecret>('/clusters/bootstrap-tokens', { method: 'POST', body }),
    onSuccess: () => void qc.invalidateQueries({ queryKey: keys.bootstrapTokens() }),
  })
}

// ---------------------------------------------------------------------------
// tokens and secrets
// ---------------------------------------------------------------------------

export function useTokens() {
  return useQuery({
    queryKey: keys.tokens(),
    queryFn: ({ signal }) =>
      request<{ tokens: ApiToken[] }>('/tokens', { signal }).then((r) => r.tokens),
  })
}

export function useCreateToken() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (body: { name: string; scopes: string[]; subject?: string; ttl_seconds?: number }) =>
      request<ApiTokenSecret>('/tokens', { method: 'POST', body }),
    onSuccess: () => void qc.invalidateQueries({ queryKey: keys.tokens() }),
  })
}

export function useRevokeToken() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (id: string) =>
      request<void>(`/tokens/${encodeURIComponent(id)}`, { method: 'DELETE' }),
    onSuccess: () => void qc.invalidateQueries({ queryKey: keys.tokens() }),
  })
}

export function useSecrets() {
  return useQuery({
    queryKey: keys.secrets(),
    queryFn: ({ signal }) =>
      request<{ secrets: Secret[] }>('/secrets', { signal }).then((r) => r.secrets),
  })
}

export function usePutSecret() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: ({ name, value, ref }: { name: string; value?: string; ref?: string }) =>
      request<Secret>(`/secrets/${encodeURIComponent(name)}`, {
        method: 'PUT',
        body: value ? { value } : { ref },
      }),
    onSuccess: () => void qc.invalidateQueries({ queryKey: keys.secrets() }),
  })
}
