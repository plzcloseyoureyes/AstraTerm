/*
 * REST client of the recordings feature (internal/recording). Keys live under ['recordings', …] (queryKeys.recordings).
 */
import { keepPreviousData, useQuery, useQueryClient } from '@tanstack/react-query'
import { api, apiUrl, isApiError, seg } from '@/api/client'
import { queryKeys } from '@/api/queryKeys'
import type {
  AdminSession,
  CommandRecord,
  CreateShareRequest,
  LogsResponse,
  PublicShareInfo,
  RdpRecording,
  RecordingFilters,
  RecordingItem,
  RecordingList,
  RecordingPolicy,
  RecordingUsage,
  RetentionResult,
  SearchAllResponse,
  SearchResponse,
  ShareView,
} from './types'

export const recKeys = {
  all: queryKeys.recordings,
  list: (f: RecordingFilters, limit: number) => [...queryKeys.recordings, 'list', f, limit] as const,
  item: (id: string) => [...queryKeys.recordings, 'item', id] as const,
  usage: (all: boolean) => [...queryKeys.recordings, 'usage', all] as const,
  policy: [...queryKeys.recordings, 'policy'] as const,
  rdp: (all: boolean) => [...queryKeys.recordings, 'rdp', all] as const,
  shares: (sessionId: string) => ['shares', 'session', sessionId] as const,
  myShares: ['shares', 'mine'] as const,
  commands: (sessionId: string) => ['sessions', 'commands', sessionId] as const,
  adminSessions: ['admin', 'live-sessions'] as const,
  audit: (all: boolean, q: string) => ['recordings', 'commands', all, q] as const,
}

// ---- recordings ---------------------------------------------------------------------------------------------------

function filterQuery(f: RecordingFilters, limit: number, offset = 0) {
  return {
    q: f.q || undefined,
    kind: f.kind && f.kind !== 'guac' ? f.kind : undefined,
    connectionId: f.connectionId || undefined,
    from: f.from || undefined,
    to: f.to || undefined,
    all: f.all ? 1 : undefined,
    sort: f.sort && f.sort !== 'started' ? f.sort : undefined,
    order: f.order === 'asc' ? 'asc' : undefined,
    limit,
    offset,
  }
}

export const listRecordings = (f: RecordingFilters, limit = 200, offset = 0) =>
  api.get<RecordingList>('/api/recordings', { query: filterQuery(f, limit, offset) })
export const getRecording = (id: string) => api.get<RecordingItem>(`/api/recordings/${seg(id)}`)
export const deleteRecording = (id: string) => api.del<void>(`/api/recordings/${seg(id)}`)
export const bulkDeleteRecordings = (ids: string[]) =>
  api.post<{ deleted: string[]; failed: { id: string; error: string; code?: string }[]; bytes: number }>('/api/recordings/bulk-delete', { ids })
export const getUsage = (all: boolean) => api.get<RecordingUsage>('/api/recordings/usage', { query: { all: all ? 1 : undefined } })
export const getPolicy = () => api.get<RecordingPolicy>('/api/recordings/policy')
export const putPolicy = (patch: Partial<RecordingPolicy>) => api.put<RecordingPolicy>('/api/admin/recordings/policy', patch)
export const runCleanup = (dryRun: boolean) => api.post<RetentionResult>('/api/admin/recordings/cleanup', { dryRun })
export const searchRecording = (id: string, q: string, opts: { regex?: boolean; caseSensitive?: boolean; limit?: number } = {}) =>
  api.get<SearchResponse>(`/api/recordings/${seg(id)}/search`, {
    query: { q, regex: opts.regex ? 1 : undefined, case: opts.caseSensitive ? 1 : undefined, limit: opts.limit },
  })
export const searchAll = (q: string, f: RecordingFilters, opts: { regex?: boolean; caseSensitive?: boolean } = {}) =>
  api.get<SearchAllResponse>('/api/recordings/search', {
    query: {
      q,
      regex: opts.regex ? 1 : undefined,
      case: opts.caseSensitive ? 1 : undefined,
      kind: f.kind && f.kind !== 'guac' ? f.kind : undefined,
      all: f.all ? 1 : undefined,
      connectionId: f.connectionId || undefined,
      from: f.from || undefined,
      to: f.to || undefined,
    },
  })

/** URL of a recording file (same-origin, cookie-authenticated). */
export function recordingFileUrl(id: string, format?: 'v3' | 'v2' | 'txt', download = false): string {
  return apiUrl(`/api/recordings/${seg(id)}/file`, { format, download: download ? 1 : undefined })
}

export const replayUrl = (sessionId: string, minutes: number) =>
  apiUrl(`/api/sessions/${seg(sessionId)}/replay`, { minutes: Math.max(0, Math.floor(minutes)) })

/** Recordings of the RDP module (404-tolerant: the module may not provide them). */
export async function listRdpRecordings(all: boolean): Promise<RecordingItem[]> {
  try {
    const list = await api.get<RdpRecording[]>('/api/rdp/recordings', { query: { all: all ? 1 : undefined } })
    return (list ?? []).map((r) => ({
      id: r.id,
      ownerId: r.ownerId,
      sessionId: r.sessionId,
      connectionId: r.connectionId,
      title: r.title,
      kind: 'guac' as const,
      size: r.size,
      cols: r.width,
      rows: r.height,
      startedAt: r.startedAt,
      endedAt: r.endedAt,
      live: !r.endedAt,
      durationMs: r.endedAt ? Math.max(0, Date.parse(r.endedAt) - Date.parse(r.startedAt)) : undefined,
      source: 'rdp' as const,
    }))
  } catch (err) {
    if (isApiError(err) && (err.status === 404 || err.status === 403)) return []
    throw err
  }
}
export const deleteRdpRecording = (id: string) => api.del<void>(`/api/rdp/recordings/${seg(id)}`)
export const rdpRecordingFileUrl = (id: string) => apiUrl(`/api/rdp/recordings/${seg(id)}/file`)

export function useRecordings(f: RecordingFilters, limit: number) {
  return useQuery({
    queryKey: recKeys.list(f, limit),
    queryFn: () => listRecordings(f, limit),
    placeholderData: keepPreviousData,
    refetchInterval: (q) => (q.state.data?.items.some((i) => i.live) ? 5000 : 30_000),
  })
}

export function useRdpRecordings(all: boolean, enabled: boolean) {
  return useQuery({ queryKey: recKeys.rdp(all), queryFn: () => listRdpRecordings(all), enabled, staleTime: 15_000, retry: false })
}

export function useRecording(id: string | undefined) {
  return useQuery({
    queryKey: recKeys.item(id ?? ''),
    queryFn: () => getRecording(id!),
    enabled: !!id,
    placeholderData: undefined, // never another recording's metadata
    refetchInterval: (q) => (q.state.data?.live ? 3000 : false),
  })
}

export function useUsage(all: boolean) {
  return useQuery({ queryKey: recKeys.usage(all), queryFn: () => getUsage(all), staleTime: 10_000 })
}

export function usePolicy() {
  return useQuery({ queryKey: recKeys.policy, queryFn: getPolicy, staleTime: 30_000 })
}

export function useInvalidateRecordings() {
  const qc = useQueryClient()
  return () => qc.invalidateQueries({ queryKey: queryKeys.recordings })
}

// ---- sessions: replay, commands, shares -----------------------------------------------------------------------------

export const getSessionCommands = (id: string) =>
  api.get<{ commands: CommandRecord[]; auditing: boolean }>(`/api/sessions/${seg(id)}/commands`)
export const listShares = (sessionId: string) => api.get<ShareView[]>(`/api/sessions/${seg(sessionId)}/shares`)
export const listMyShares = () => api.get<ShareView[]>('/api/shares')
export const createShare = (sessionId: string, req: CreateShareRequest) => api.post<ShareView>(`/api/sessions/${seg(sessionId)}/share`, req)
export const revokeShare = (id: string) => api.del<void>(`/api/shares/${seg(id)}`)
/** Pause or allow guest input of an interactive link (takes effect on open viewer sockets at once). */
export const setShareInput = (id: string, paused: boolean) => api.put<ShareView>(`/api/shares/${seg(id)}/input`, { paused })
export const revokeSessionShares = (sessionId: string) => api.del<{ revoked: number }>(`/api/sessions/${seg(sessionId)}/shares`)

/** Public share description (no login; the viewer page). */
export const getPublicShare = (token: string) =>
  api.get<PublicShareInfo>(`/api/share/${seg(token)}`, { noAuthRedirect: true, noVaultPrompt: true })

export function useShares(sessionId: string | null) {
  return useQuery({
    queryKey: recKeys.shares(sessionId ?? ''),
    queryFn: () => listShares(sessionId!),
    enabled: !!sessionId,
    refetchInterval: 15_000,
    placeholderData: undefined, // never another session's links
  })
}

export function useMyShares(enabled = true) {
  return useQuery({ queryKey: recKeys.myShares, queryFn: listMyShares, enabled, refetchInterval: 30_000 })
}

/** Absolute share link for a relative url ("/share/<token>"). */
export function absoluteShareUrl(url: string): string {
  return new URL(url, location.origin).toString()
}

// ---- admin ----------------------------------------------------------------------------------------------------------

export const listAdminSessions = () => api.get<AdminSession[]>('/api/admin/sessions')
export const terminateSession = (id: string, reason?: string) => api.post<void>(`/api/admin/sessions/${seg(id)}/terminate`, { reason })
export const messageSession = (id: string, text: string) => api.post<void>(`/api/admin/sessions/${seg(id)}/message`, { text })

export const getLogs = (q: { level?: string; q?: string; module?: string; after?: number; limit?: number }) =>
  api.get<LogsResponse>('/api/admin/logs', { query: q })
export const clearLogs = () => api.del<void>('/api/admin/logs')
export const logsExportUrl = (q: { level?: string; q?: string }) => apiUrl('/api/admin/logs/export', q)

// ---- command audit (audit log) -------------------------------------------------------------------------------------

export interface CommandAuditEntry {
  id: number
  ts: string
  userId?: string
  username?: string
  action: string
  target?: string
  ip?: string
  details?: {
    command?: string
    exitCode?: number
    durationMs?: number
    cwd?: string
    host?: string
    username?: string
    title?: string
    protocol?: string
    source?: string
    connectionId?: string
    recordingId?: string
    recordingTime?: number
    running?: boolean
    guest?: { shareId: string; viewerId: string; username?: string; ip?: string; label?: string }
  }
}

export const listCommandAudit = (all: boolean, before?: number, limit = 200) =>
  api.get<CommandAuditEntry[]>(all ? '/api/admin/audit' : '/api/audit/me', {
    query: { action: 'session.command', before, limit },
  })

/** Trigger a same-origin download without navigating (the browser's download manager handles it). */
export function downloadUrl(url: string): void {
  const a = document.createElement('a')
  a.href = url
  a.rel = 'noopener'
  a.download = ''
  document.body.appendChild(a)
  a.click()
  a.remove()
}
