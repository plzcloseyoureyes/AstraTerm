/*
 * REST calls of the RDP feature (internal/rdp): tickets, state reports, .rdp files, native launch, guacd status and
 * the guacd sidecar job.
 */
import { useQuery } from '@tanstack/react-query'
import { api, apiUrl, seg } from '@/api/client'
import type { JobStarted, RuntimeSession, SessionState } from '@/api/types'
import type { GuacdStatusInfo, LaunchNativeResult, RdpRecording, RdpTicketInfo, RdpTicketRequest } from './types'

export const rdpKeys = {
  guacdStatus: ['rdp', 'guacd-status'] as const,
  recordings: (all: boolean) => ['rdp', 'recordings', all] as const,
}

/** One-time ticket (+ connection parameters) for a viewer of an RDP session. */
export const getTicket = (sessionId: string, req: RdpTicketRequest) =>
  api.post<RdpTicketInfo>(`/api/sessions/${seg(sessionId)}/rdp-ticket`, req)

/** Report the viewer-side RDP state (the browser runs the protocol for IronRDP). */
export const reportState = (sessionId: string, state: SessionState, message?: string, authFailed?: boolean) =>
  api.post<RuntimeSession>(`/api/sessions/${seg(sessionId)}/rdp-state`, { state, message: message ?? '', authFailed: !!authFailed })

/** Download URLs of .rdp files (cookie-authenticated GETs). */
export const sessionRdpFileUrl = (sessionId: string) => apiUrl(`/api/sessions/${seg(sessionId)}/rdp-file`)
export const connectionRdpFileUrl = (connectionId: string) => apiUrl(`/api/connections/${seg(connectionId)}/rdp-file`)

export const launchNativeConnection = (connectionId: string) =>
  api.post<LaunchNativeResult>(`/api/connections/${seg(connectionId)}/launch-native`)
export const launchNativeSession = (sessionId: string) => api.post<LaunchNativeResult>(`/api/sessions/${seg(sessionId)}/launch-native`)

const getGuacdStatus = () => api.get<GuacdStatusInfo>('/api/guacd/status')
export const sidecarAction = (action: 'start' | 'stop') => api.post<JobStarted>('/api/guacd/sidecar', { action })

export function useGuacdStatus(enabled = true, refetchInterval: number | false = false) {
  return useQuery({ queryKey: rdpKeys.guacdStatus, queryFn: getGuacdStatus, enabled, staleTime: 5_000, refetchInterval })
}

/** Recorded guacd sessions: the caller's, or everybody's (administrators, all = true). */
const listRecordings = (all: boolean) => api.get<RdpRecording[]>(`/api/rdp/recordings${all ? '?all=1' : ''}`)
export const deleteRecording = (id: string) => api.del(`/api/rdp/recordings/${seg(id)}`)
export const recordingFileUrl = (id: string) => apiUrl(`/api/rdp/recordings/${seg(id)}/file`)

export function useRecordings(all: boolean, enabled = true) {
  return useQuery({ queryKey: rdpKeys.recordings(all), queryFn: () => listRecordings(all), enabled, staleTime: 10_000 })
}

/** Trigger a browser download of a same-origin URL. */
export function downloadUrl(url: string): void {
  const a = document.createElement('a')
  a.href = url
  a.rel = 'noopener'
  a.download = ''
  document.body.appendChild(a)
  a.click()
  a.remove()
}

/** Save a blob as a file. */
export function saveBlob(blob: Blob, name: string): void {
  const url = URL.createObjectURL(blob)
  const a = document.createElement('a')
  a.href = url
  a.download = name
  document.body.appendChild(a)
  a.click()
  a.remove()
  setTimeout(() => URL.revokeObjectURL(url), 60_000)
}
