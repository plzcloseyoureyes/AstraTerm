import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api, seg } from './client'
import { removeByIds, upsertById } from './optimistic'
import { queryKeys } from './queryKeys'
import type {
  CreateSessionRequest,
  DockerContainer,
  KubeContext,
  KubePod,
  LocalShell,
  RdpTicket,
  RuntimeSession,
  SerialPortInfo,
  ShareInfo,
  ShareRequest,
  ShareResponse,
} from './types'

// --- runtime sessions ------------------------------------------------------------------------------------------------

export const listSessions = (all = false) => api.get<RuntimeSession[]>('/api/sessions', { query: { all: all ? 1 : undefined } })
export const getSession = (id: string) => api.get<RuntimeSession>(`/api/sessions/${seg(id)}`)
export const createSession = (req: CreateSessionRequest) => api.post<RuntimeSession>('/api/sessions', req)
export const closeSession = (id: string) => api.del<void>(`/api/sessions/${seg(id)}`)
export const reconnectSession = (id: string) => api.post<RuntimeSession | void>(`/api/sessions/${seg(id)}/reconnect`)
export const renameSession = (id: string, title: string) => api.patch<RuntimeSession>(`/api/sessions/${seg(id)}`, { title })
export const sendSessionInput = (id: string, data: string) => api.post<void>(`/api/sessions/${seg(id)}/input`, { data })
export const signalSession = (id: string, name: string) => api.post<void>(`/api/sessions/${seg(id)}/signal`, { name })
export const breakSession = (id: string) => api.post<void>(`/api/sessions/${seg(id)}/break`)
export const setSessionRecording = (id: string, enabled: boolean) =>
  api.post<RuntimeSession | void>(`/api/sessions/${seg(id)}/record`, { enabled })
export const setSessionLogging = (id: string, enabled: boolean) =>
  api.post<RuntimeSession | void>(`/api/sessions/${seg(id)}/log`, { enabled })
/** Ring buffer as text; raw=false strips ANSI sequences. */
export const getScrollback = (id: string, raw = true) =>
  api.get<string>(`/api/sessions/${seg(id)}/scrollback`, { as: 'text', query: { raw: raw ? undefined : 0 } })
export const shareSession = (id: string, req: ShareRequest) => api.post<ShareResponse>(`/api/sessions/${seg(id)}/share`, req)
export const getShareInfo = (token: string) => api.get<ShareInfo>(`/api/share/${seg(token)}`)
export const getRdpTicket = (id: string) => api.post<RdpTicket>(`/api/sessions/${seg(id)}/rdp-ticket`)

// --- local resources -------------------------------------------------------------------------------------------------

export const listLocalShells = () => api.get<LocalShell[]>('/api/local/shells')
export const listSerialPorts = () => api.get<SerialPortInfo[]>('/api/serial/ports')
export const listDockerContainers = (host?: string) =>
  api.get<DockerContainer[]>('/api/docker/containers', { query: { host: host || undefined } })
export const listKubeContexts = () => api.get<KubeContext[]>('/api/kube/contexts')
export const listKubePods = (context?: string, namespace?: string) =>
  api.get<KubePod[]>('/api/kube/pods', { query: { context: context || undefined, namespace: namespace || undefined } })

// --- hooks -----------------------------------------------------------------------------------------------------------

/**
 * Own runtime sessions. Kept live by the events socket (session.updated / session.closed patch this cache), so it is
 * never considered stale while the socket is up.
 */
export function useSessions(enabled = true) {
  return useQuery({ queryKey: queryKeys.sessions, queryFn: () => listSessions(false), enabled, staleTime: 60_000 })
}

/** A single live session from the list cache (no extra request). */
export function useRuntimeSession(id: string | undefined | null): RuntimeSession | undefined {
  const { data } = useSessions(!!id)
  return id ? data?.find((s) => s.id === id) : undefined
}

export function useLocalShells(enabled = true) {
  return useQuery({ queryKey: queryKeys.localShells, queryFn: listLocalShells, enabled, staleTime: 5 * 60_000 })
}

export function useSerialPorts(enabled = true) {
  return useQuery({ queryKey: queryKeys.serialPorts, queryFn: listSerialPorts, enabled, staleTime: 10_000 })
}

export function useCreateSession() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: createSession,
    onSuccess: (s) => {
      qc.setQueryData<RuntimeSession[]>(queryKeys.sessions, (old) => upsertById(old, s))
      // lastUsedAt changed server-side.
      if (s.connectionId) qc.invalidateQueries({ queryKey: queryKeys.connections })
    },
  })
}

export function useCloseSession() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: closeSession,
    onSuccess: (_d, id) => qc.setQueryData<RuntimeSession[]>(queryKeys.sessions, (old) => removeByIds(old, id)),
  })
}

export function useRenameSession() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: ({ id, title }: { id: string; title: string }) => renameSession(id, title),
    onSuccess: (s) => {
      if (s && typeof s === 'object') qc.setQueryData<RuntimeSession[]>(queryKeys.sessions, (old) => upsertById(old, s))
    },
  })
}

/** Update the sessions cache from an events-socket push (used by src/lib/events.ts). */
export function applySessionUpdate(qc: ReturnType<typeof useQueryClient>, s: RuntimeSession): void {
  qc.setQueryData<RuntimeSession[]>(queryKeys.sessions, (old) => (old ? upsertById(old, s) : old))
}

export function applySessionClosed(qc: ReturnType<typeof useQueryClient>, id: string): void {
  qc.setQueryData<RuntimeSession[]>(queryKeys.sessions, (old) => (old ? removeByIds(old, id) : old))
}

/** Sessions that are live (not closed) — "running sessions". */
export function isSessionRunning(s: RuntimeSession): boolean {
  return s.state !== 'closed'
}
