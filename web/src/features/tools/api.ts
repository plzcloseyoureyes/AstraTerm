/*
 * REST helpers for the tools module. Long-running tools go through the shared job runner (POST /api/tools/{tool} →
 * {jobId}, streamed as `job` events); the sync endpoints return immediately.
 */
import { api } from '@/api/client'
import type { InterfaceInfo, KeygenResult, SocketInfo } from './types'

export const getInterfaces = () => api.get<InterfaceInfo[]>('/api/tools/interfaces')
export const getListening = (all = false) => api.get<SocketInfo[]>('/api/tools/listening', { query: { all: all ? 1 : undefined } })
export const killListener = (pid: number, signal: 'TERM' | 'KILL' = 'TERM') =>
  api.post<void>('/api/tools/listening/kill', { pid, signal })
export const generateKey = (body: { type: string; bits?: number; comment?: string; passphrase?: string }) =>
  api.post<KeygenResult>('/api/tools/keygen', body)
