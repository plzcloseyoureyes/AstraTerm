/*
 * REST client + react-query cache for the embedded servers (GET/PUT /api/servers/…), kept live by the "servers"
 * events topic ({type:'server', status}).
 */
import { useQuery } from '@tanstack/react-query'
import { api, apiUrl, seg } from '@/api/client'
import { queryClient } from '@/api/queryClient'
import { queryKeys } from '@/api/queryKeys'
import { events } from '@/lib/events'
import { useAuthStore } from '@/stores/auth'
import type {
  HostInfo,
  ServerClient,
  ServerKindEx,
  ServerLogsReply,
  ServerStatusEx,
  ServerStatusEvent,
  SyslogPage,
  SyslogQuery,
} from './types'

export const serverKeys = {
  list: queryKeys.servers,
  host: [...queryKeys.servers, 'host'] as const,
  clients: (kind: ServerKindEx) => [...queryKeys.servers, kind, 'clients'] as const,
}

// ---- REST ---------------------------------------------------------------------------------------------------------

export const listServers = () => api.get<ServerStatusEx[]>('/api/servers')
export const getHostInfo = () => api.get<HostInfo>('/api/servers/host')
export const saveServerConfig = (kind: ServerKindEx, config: Record<string, unknown>) =>
  api.put<ServerStatusEx>(`/api/servers/${seg(kind)}`, config)
export const startServer = (kind: ServerKindEx) => api.post<ServerStatusEx>(`/api/servers/${seg(kind)}/start`)
export const stopServer = (kind: ServerKindEx) => api.post<ServerStatusEx>(`/api/servers/${seg(kind)}/stop`)
export const restartServer = (kind: ServerKindEx) => api.post<ServerStatusEx>(`/api/servers/${seg(kind)}/restart`)
export const stopAllServers = () => api.post<ServerStatusEx[]>('/api/servers/stop-all')
export const getServerLogs = (kind: ServerKindEx, after = 0, limit = 1000) =>
  api.get<ServerLogsReply>(`/api/servers/${seg(kind)}/logs`, { query: { after, limit } })
export const clearServerLogs = (kind: ServerKindEx) => api.del(`/api/servers/${seg(kind)}/logs`)
export const listServerClients = (kind: ServerKindEx) => api.get<ServerClient[]>(`/api/servers/${seg(kind)}/clients`)
export const disconnectClient = (kind: ServerKindEx, id: string) =>
  api.del(`/api/servers/${seg(kind)}/clients/${seg(id)}`)

function syslogParams(q: SyslogQuery): Record<string, string | number | undefined> {
  return {
    q: q.q || undefined,
    regex: q.regex && q.q ? 1 : undefined,
    severity: q.severity != null && q.severity < 7 ? q.severity : undefined,
    facility: q.facility != null && q.facility >= 0 ? q.facility : undefined,
    host: q.host || undefined,
    app: q.app || undefined,
    after: q.after || undefined,
    before: q.before || undefined,
    limit: q.limit || undefined,
  }
}

export const getSyslogMessages = (q: SyslogQuery, signal?: AbortSignal) =>
  api.get<SyslogPage>('/api/servers/syslog/messages', { query: syslogParams(q), signal })
export const clearSyslogMessages = () => api.del('/api/servers/syslog/messages')
export const syslogExportUrl = (q: SyslogQuery) => apiUrl('/api/servers/syslog/export', syslogParams(q))

// ---- cache ----------------------------------------------------------------------------------------------------------

/** May the current user manage the servers? (desktop mode: everyone; server mode: administrators.) */
export function serversAllowed(): boolean {
  const st = useAuthStore.getState()
  if (!st.user) return false
  return (st.state?.mode ?? 'desktop') === 'desktop' || st.user.role === 'admin'
}

export function useServersAllowed(): boolean {
  return useAuthStore((s) => !!s.user && ((s.state?.mode ?? 'desktop') === 'desktop' || s.user.role === 'admin'))
}

export function useServers(enabled = true) {
  const allowed = useServersAllowed()
  return useQuery({
    queryKey: serverKeys.list,
    queryFn: listServers,
    enabled: enabled && allowed,
    staleTime: 30_000,
    refetchOnWindowFocus: true,
  })
}

export function useServer(kind: ServerKindEx | undefined): ServerStatusEx | undefined {
  const { data } = useServers(!!kind)
  return kind ? data?.find((s) => s.kind === kind) : undefined
}

export function useHostInfo(enabled = true) {
  const allowed = useServersAllowed()
  return useQuery({ queryKey: serverKeys.host, queryFn: getHostInfo, enabled: enabled && allowed, staleTime: 60_000 })
}

export function cachedServers(): ServerStatusEx[] {
  return queryClient.getQueryData<ServerStatusEx[]>(serverKeys.list) ?? []
}

export async function ensureServers(): Promise<ServerStatusEx[]> {
  return queryClient.ensureQueryData({ queryKey: serverKeys.list, queryFn: listServers })
}

/** Replace one server's status in the cache. */
export function applyStatus(st: ServerStatusEx): void {
  if (!st || typeof st.kind !== 'string') return
  queryClient.setQueryData<ServerStatusEx[]>(serverKeys.list, (old) => {
    if (!old) return old
    const i = old.findIndex((s) => s.kind === st.kind)
    if (i < 0) return [...old, st]
    const next = old.slice()
    next[i] = st
    return next
  })
}

export function applyStatuses(list: ServerStatusEx[]): void {
  if (Array.isArray(list)) queryClient.setQueryData(serverKeys.list, list)
}

let installed: (() => void) | null = null

/**
 * Wire live status events into the cache and hold the "servers" topic subscription while the user may use the
 * servers (re-evaluated when the signed-in user or run mode changes).
 */
export function installServerEvents(): () => void {
  if (installed) return installed
  let release: (() => void) | null = null
  const sync = () => {
    const allowed = serversAllowed()
    if (allowed && !release) release = events.subscribe('servers')
    if (!allowed && release) {
      release()
      release = null
    }
  }
  sync()
  const offs = [
    useAuthStore.subscribe(sync),
    events.on('server', (ev) => applyStatus((ev as unknown as ServerStatusEvent).status)),
    events.on('hello', () => {
      if (serversAllowed()) void queryClient.invalidateQueries({ queryKey: serverKeys.list, exact: true })
    }),
  ]
  installed = () => {
    for (const off of offs) off()
    release?.()
    release = null
    installed = null
  }
  return installed
}
