/*
 * Tunnels REST client (SPEC §6.0 "Tunnels" + §9 extensions), react-query hooks, and the events wiring that keeps the
 * caches live: {type:'tunnel'} status pushes and {type:'tunnel.session'} session-forward lists.
 */
import { useMutation, useQuery } from '@tanstack/react-query'
import { api, seg } from '@/api/client'
import { upsertById } from '@/api/optimistic'
import { queryClient } from '@/api/queryClient'
import { queryKeys } from '@/api/queryKeys'
import { events } from '@/lib/events'
import type {
  BulkStartResult,
  CheckBindResult,
  ExportFile,
  ForwardSpec,
  ImportResult,
  RemotePorts,
  SessionForward,
  SessionForwardsEvent,
  TunnelEvent,
  TunnelEx,
  TunnelInput,
} from './types'

export const tunnelKeys = {
  list: queryKeys.tunnels,
  sessionForwards: ['tunnel-session-forwards'] as const,
  remotePorts: (target: string) => ['tunnel-remote-ports', target] as const,
}

// --- REST ------------------------------------------------------------------------------------------------------------

export const listTunnels = () => api.get<TunnelEx[]>('/api/tunnels')
export const getTunnel = (id: string) => api.get<TunnelEx>(`/api/tunnels/${seg(id)}`)
export const createTunnel = (input: TunnelInput) => api.post<TunnelEx>('/api/tunnels', input)
export const updateTunnel = (id: string, patch: Partial<TunnelInput>) => api.patch<TunnelEx>(`/api/tunnels/${seg(id)}`, patch)
export const deleteTunnel = (id: string) => api.del<void>(`/api/tunnels/${seg(id)}`)
export const startTunnel = (id: string) => api.post<TunnelEx>(`/api/tunnels/${seg(id)}/start`)
export const stopTunnel = (id: string) => api.post<TunnelEx>(`/api/tunnels/${seg(id)}/stop`)
export const restartTunnel = (id: string) => api.post<TunnelEx>(`/api/tunnels/${seg(id)}/restart`)
export const duplicateTunnel = (id: string) => api.post<TunnelEx>(`/api/tunnels/${seg(id)}/duplicate`)
export const startAllTunnels = (ids?: string[]) => api.post<BulkStartResult>('/api/tunnels/start-all', ids ? { ids } : undefined)
export const stopAllTunnels = (ids?: string[]) => api.post<{ stopped: number }>('/api/tunnels/stop-all', ids ? { ids } : undefined)
export const reorderTunnels = (items: { id: string; sortOrder: number }[]) => api.post<void>('/api/tunnels/reorder', { items })
export const checkBind = (req: { bindHost: string; bindPort: number; bindSocket?: string; id?: string }, signal?: AbortSignal) =>
  api.post<CheckBindResult>('/api/tunnels/check-bind', req, { signal })
export const exportTunnels = () => api.get<ExportFile>('/api/tunnels/export')
export const importTunnels = (req: { file: unknown; defaultConnectionId?: string; dryRun?: boolean }) =>
  api.post<ImportResult>('/api/tunnels/import', req)
export const getRemotePorts = (target: { connectionId?: string; sessionId?: string }) =>
  api.get<RemotePorts>('/api/tunnels/remote-ports', { query: target })
export const listSessionForwards = (sessionId?: string) =>
  api.get<SessionForward[]>('/api/tunnels/session-forwards', { query: { sessionId } })
export const addSessionForward = (sessionId: string, spec: ForwardSpec) =>
  api.post<SessionForward>('/api/tunnels/session-forwards', { sessionId, ...spec })
export const removeSessionForward = (id: string) => api.del<void>(`/api/tunnels/session-forwards/${seg(id)}`)

// --- cache helpers ---------------------------------------------------------------------------------------------------

/** Replace one tunnel in the list cache (after a mutation returned it). */
export function putTunnel(t: TunnelEx): void {
  queryClient.setQueryData<TunnelEx[]>(tunnelKeys.list, (old) => (old ? upsertById(old, t) : old))
}

export function dropTunnel(id: string): void {
  queryClient.setQueryData<TunnelEx[]>(tunnelKeys.list, (old) => old?.filter((t) => t.id !== id))
}

export function cachedTunnels(): TunnelEx[] {
  return queryClient.getQueryData<TunnelEx[]>(tunnelKeys.list) ?? []
}

/** Fetch the list once if nothing is cached (menus, status bar). */
export function ensureTunnels(): Promise<TunnelEx[]> {
  return queryClient.ensureQueryData({ queryKey: tunnelKeys.list, queryFn: listTunnels })
}

// --- hooks -----------------------------------------------------------------------------------------------------------

/** The caller's tunnels; statuses are kept live by the events socket. */
export function useTunnels(enabled = true) {
  return useQuery({ queryKey: tunnelKeys.list, queryFn: listTunnels, enabled, staleTime: 60_000 })
}

export function useSessionForwards(enabled = true) {
  return useQuery({ queryKey: tunnelKeys.sessionForwards, queryFn: () => listSessionForwards(), enabled, staleTime: 60_000 })
}

export function useRemotePorts(target: { connectionId?: string; sessionId?: string } | null) {
  const key = target?.sessionId ? `s:${target.sessionId}` : target?.connectionId ? `c:${target.connectionId}` : ''
  return useQuery({
    queryKey: tunnelKeys.remotePorts(key),
    placeholderData: undefined, // never show another host's ports under this key
    queryFn: () => getRemotePorts(target!),
    enabled: !!key,
    staleTime: 0,
    gcTime: 30_000,
    retry: false,
  })
}

export function useCreateTunnel() {
  return useMutation({ mutationFn: createTunnel, onSuccess: putTunnel })
}

export function useUpdateTunnel() {
  return useMutation({
    mutationFn: ({ id, patch }: { id: string; patch: Partial<TunnelInput> }) => updateTunnel(id, patch),
    onSuccess: putTunnel,
  })
}

// --- events ----------------------------------------------------------------------------------------------------------

function sameStatus(a: unknown, b: unknown): boolean {
  return JSON.stringify(a) === JSON.stringify(b)
}

function applyTunnelEvent(ev: TunnelEvent): void {
  if (!ev || typeof ev.id !== 'string') return
  if (ev.change === 'deleted') {
    dropTunnel(ev.id)
    return
  }
  const list = queryClient.getQueryData<TunnelEx[]>(tunnelKeys.list)
  if (!list) return
  const i = list.findIndex((t) => t.id === ev.id)
  if (i >= 0 && ev.status && !sameStatus(list[i].status, ev.status)) {
    const next = list.slice()
    next[i] = { ...list[i], status: ev.status }
    queryClient.setQueryData(tunnelKeys.list, next)
  }
  // Definition changes (possibly from another window) and unknown tunnels: refetch the list.
  if (i < 0 || ev.change) void queryClient.invalidateQueries({ queryKey: tunnelKeys.list, exact: true })
}

function applySessionForwards(ev: SessionForwardsEvent): void {
  if (!ev || typeof ev.sessionId !== 'string' || !Array.isArray(ev.forwards)) return
  queryClient.setQueryData<SessionForward[]>(tunnelKeys.sessionForwards, (old) => {
    if (!old) return old
    return [...old.filter((f) => f.sessionId !== ev.sessionId), ...ev.forwards]
  })
}

let installed: (() => void) | null = null

/** Wire the events socket into the tunnel caches (idempotent). */
export function installTunnelEvents(): () => void {
  if (installed) return installed
  const offs = [
    events.on('tunnel', (ev) => applyTunnelEvent(ev as unknown as TunnelEvent)),
    events.onAny((ev) => {
      if ((ev as { type: string }).type === 'tunnel.session') applySessionForwards(ev as unknown as SessionForwardsEvent)
    }),
    events.on('session.closed', (ev) => {
      queryClient.setQueryData<SessionForward[]>(tunnelKeys.sessionForwards, (old) => old?.filter((f) => f.sessionId !== ev.id))
    }),
    // Statuses may have changed while the socket was down.
    events.on('hello', () => {
      void queryClient.invalidateQueries({ queryKey: tunnelKeys.list, exact: true })
      void queryClient.invalidateQueries({ queryKey: tunnelKeys.sessionForwards, exact: true })
    }),
  ]
  installed = () => {
    for (const off of offs) off()
    installed = null
  }
  return installed
}
