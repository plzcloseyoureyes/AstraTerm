/*
 * REST + react-query layer of the keys feature (internal/keys). Query keys extend the core factory: ['keys', …]
 * (so invalidations also refresh the session editor's key picker), ['known-hosts', …] and ['agent', …].
 */
import { useQuery } from '@tanstack/react-query'
import { api, seg } from '@/api/client'
import { queryClient } from '@/api/queryClient'
import { queryKeys } from '@/api/queryKeys'
import type { KnownHost } from '@/api/types'
import type {
  AgentKey,
  AgentStatus,
  ConvertRequest,
  ExportRequest,
  ExportResult,
  GenerateRequest,
  HostKeyMarker,
  ImportRequest,
  InspectResult,
  InstallResult,
  KeyDraft,
  KeyPatch,
  KnownHostsConflict,
  KnownHostsImportResult,
  SignRequest,
  SignResult,
  StoredKey,
} from './types'

export const keysQK = {
  all: queryKeys.keys,
  list: [...queryKeys.keys, 'list'] as const,
  knownHosts: [...queryKeys.knownHosts, 'list'] as const,
  markers: [...queryKeys.knownHosts, 'markers'] as const,
  agent: ['agent'] as const,
  agentStatus: ['agent', 'status'] as const,
  agentKeys: ['agent', 'keys'] as const,
}

// ---- keys -------------------------------------------------------------------------------------------------------------

const listKeys = () => api.get<StoredKey[]>('/api/keys')
export const generateDraft = (req: GenerateRequest) => api.post<KeyDraft>('/api/keys/generate', { ...req, store: false })
export const storeDraft = (draftId: string, body: { name?: string; rememberPassphrase?: boolean }) =>
  api.post<StoredKey>(`/api/keys/drafts/${seg(draftId)}/store`, body)
export const exportDraft = (draftId: string, body: { format: string; ppkVersion?: number; name?: string }) =>
  api.post<ExportResult>(`/api/keys/drafts/${seg(draftId)}/export`, body)
export const discardDraft = (draftId: string) => api.del<void>(`/api/keys/drafts/${seg(draftId)}`)
export const importKey = (req: ImportRequest) => api.post<StoredKey>('/api/keys/import', req)
export const inspectKey = (text: string, passphrase?: string) => api.post<InspectResult>('/api/keys/inspect', { text, passphrase })
export const convertKey = (req: ConvertRequest) => api.post<ExportResult>('/api/keys/convert', req)
export const updateKey = (id: string, patch: KeyPatch) => api.patch<StoredKey>(`/api/keys/${seg(id)}`, patch)
export const deleteKey = (id: string) => api.del<void>(`/api/keys/${seg(id)}`)
export const exportKey = (id: string, req: ExportRequest) => api.post<ExportResult>(`/api/keys/${seg(id)}/export`, req)
export const installKey = (id: string, connectionId: string) =>
  api.post<InstallResult>(`/api/keys/${seg(id)}/install`, { connectionId })
export const signCertificate = (caId: string, req: SignRequest) => api.post<SignResult>(`/api/keys/${seg(caId)}/sign`, req)

export function useKeys(enabled = true) {
  return useQuery({ queryKey: keysQK.list, queryFn: listKeys, enabled })
}

/** Invalidate everything derived from stored keys (list, session editor picker, agent keys). */
export function invalidateKeys(qc = queryClient): void {
  void qc.invalidateQueries({ queryKey: keysQK.all })
  void qc.invalidateQueries({ queryKey: keysQK.agent })
}

// ---- known hosts ------------------------------------------------------------------------------------------------------

const listKnownHosts = () => api.get<KnownHost[]>('/api/known-hosts')
export const addKnownHost = (body: { host: string; port?: number; publicKey: string; comment?: string; replace?: boolean }) =>
  api.post<KnownHost>('/api/known-hosts', body)
export const bulkDeleteKnownHosts = (ids: string[]) => api.post<{ deleted: number }>('/api/known-hosts/bulk-delete', { ids })
export const importKnownHosts = (body: { text?: string; source?: 'system'; format?: 'auto' | 'openssh' | 'putty'; onConflict?: KnownHostsConflict }) =>
  api.post<KnownHostsImportResult>('/api/known-hosts/import', body)
export const exportKnownHostsText = (hashed: boolean) =>
  api.get<string>('/api/known-hosts/export', { query: { hashed: hashed ? 1 : undefined }, as: 'text' })
const listMarkers = () => api.get<HostKeyMarker[]>('/api/known-hosts/markers')
export const addMarker = (body: { marker: string; hosts: string; publicKey?: string; keyId?: string; comment?: string }) =>
  api.post<HostKeyMarker>('/api/known-hosts/markers', body)
export const deleteMarker = (id: string) => api.del<void>(`/api/known-hosts/markers/${seg(id)}`)

export function useKnownHosts(enabled = true) {
  return useQuery({ queryKey: keysQK.knownHosts, queryFn: listKnownHosts, enabled })
}

export function useMarkers(enabled = true) {
  return useQuery({ queryKey: keysQK.markers, queryFn: listMarkers, enabled })
}

export function invalidateKnownHosts(): void {
  void queryClient.invalidateQueries({ queryKey: queryKeys.knownHosts })
}

// ---- agent ------------------------------------------------------------------------------------------------------------

export const agentStatus = () => api.get<AgentStatus>('/api/agent/status')
const agentKeys = () => api.get<AgentKey[]>('/api/agent/keys')
export const startAgent = () => api.post<AgentStatus>('/api/agent/start')
export const stopAgent = () => api.post<void>('/api/agent/stop')
export const reloadAgent = () => api.post<void>('/api/agent/reload')
export const lockAgent = () => api.post<void>('/api/agent/lock')
export const unlockAgent = () => api.post<void>('/api/agent/unlock')
export const removeAgentKey = (id: string) => api.del<void>(`/api/agent/keys/${seg(id)}`)

/** Agent status, polled every `pollMs` (false: no polling). */
export function useAgentStatus(pollMs: number | false = false) {
  return useQuery({ queryKey: keysQK.agentStatus, queryFn: agentStatus, refetchInterval: pollMs, staleTime: 3_000 })
}

export function useAgentKeys(enabled = true, poll = false) {
  return useQuery({ queryKey: keysQK.agentKeys, queryFn: agentKeys, enabled, refetchInterval: poll ? 5_000 : false })
}

export function invalidateAgent(): void {
  void queryClient.invalidateQueries({ queryKey: keysQK.agent })
}
