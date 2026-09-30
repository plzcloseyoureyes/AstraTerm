/*
 * REST for the VNC feature (SPEC §9 "vnc"): session info, reverse-connection listeners, trusted certificates.
 */
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api, seg } from '@/api/client'
import type { StartListenerRequest, TrustedCert, VncInfo, VncListener } from './types'

export const vncKeys = {
  info: (sessionId: string) => ['vnc', 'info', sessionId] as const,
  listeners: ['vnc', 'listeners'] as const,
  certs: ['vnc', 'certs'] as const,
}

export const getVncInfo = (sessionId: string) => api.get<VncInfo>(`/api/sessions/${seg(sessionId)}/vnc-info`)
const listListeners = () => api.get<VncListener[]>('/api/vnc/listen')
const startListener = (req: StartListenerRequest) => api.post<VncListener>('/api/vnc/listen', req)
const stopListener = (id: string) => api.del<void>(`/api/vnc/listen/${seg(id)}`)
const listTrustedCerts = () => api.get<TrustedCert[]>('/api/vnc/certs')
const deleteTrustedCert = (id: string) => api.del<void>(`/api/vnc/certs/${seg(id)}`)

export function useVncInfo(sessionId: string | undefined, enabled = true) {
  return useQuery({
    queryKey: vncKeys.info(sessionId ?? ''),
    placeholderData: undefined, // never show another session's details under this key
    queryFn: () => getVncInfo(sessionId!),
    enabled: !!sessionId && enabled,
    staleTime: 10_000,
    retry: false,
  })
}

export function useVncListeners(enabled = true) {
  return useQuery({ queryKey: vncKeys.listeners, queryFn: listListeners, enabled, staleTime: 5_000 })
}

export function useStartListener() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: startListener,
    onSuccess: () => qc.invalidateQueries({ queryKey: vncKeys.listeners }),
  })
}

export function useStopListener() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: stopListener,
    onSuccess: () => qc.invalidateQueries({ queryKey: vncKeys.listeners }),
  })
}

export function useTrustedCerts(enabled = true) {
  return useQuery({ queryKey: vncKeys.certs, queryFn: listTrustedCerts, enabled, staleTime: 30_000 })
}

export function useDeleteTrustedCert() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: deleteTrustedCert,
    onSuccess: () => qc.invalidateQueries({ queryKey: vncKeys.certs }),
  })
}
