/* REST + events wiring of the web proxy (internal/webproxy). */
import { useQuery } from '@tanstack/react-query'
import { api, seg } from '@/api/client'
import { queryClient } from '@/api/queryClient'
import { events } from '@/lib/events'
import type { ProxyEvent, ProxyInfo, ProxySpec, XpraCheck, XpraStartRequest } from './types'

const webproxyKeys = {
  list: ['webproxy', 'list'] as const,
  xpraCheck: (target: { connectionId?: string; sessionId?: string }) => ['webproxy', 'xpra-check', target.connectionId ?? '', target.sessionId ?? ''] as const,
}

export const createProxy = (spec: ProxySpec & { mode?: 'path' | 'auto'; check?: boolean }) => api.post<ProxyInfo>('/api/webproxy', spec)
export const proxyEntry = (id: string, body: { path?: string; mode?: 'path' | 'auto' } = {}) =>
  api.post<ProxyInfo>(`/api/webproxy/${seg(id)}/url`, body)
export const closeProxy = (id: string) => api.del<void>(`/api/webproxy/${seg(id)}`)
const listProxies = () => api.get<ProxyInfo[]>('/api/webproxy')
const checkXpra = (target: { connectionId?: string; sessionId?: string }) =>
  api.get<XpraCheck>('/api/xpra/check', { query: { connectionId: target.connectionId, sessionId: target.sessionId } })
export const startXpra = (req: XpraStartRequest) => api.post<ProxyInfo>('/api/xpra/start', req)

export function useProxies(enabled = true) {
  return useQuery({ queryKey: webproxyKeys.list, queryFn: listProxies, enabled, staleTime: 5_000 })
}

export function useXpraCheck(target: { connectionId?: string; sessionId?: string } | null) {
  return useQuery({
    queryKey: webproxyKeys.xpraCheck(target ?? {}),
    queryFn: () => checkXpra(target!),
    enabled: !!target && !!(target.connectionId || target.sessionId),
    staleTime: 60_000,
    retry: false,
    // Another host's answer would be misleading while this one is checked (docs/UX.md "Loading states").
    placeholderData: undefined,
  })
}

type Listener = (ev: ProxyEvent) => void
const listeners = new Set<Listener>()

/** Observe proxy events (created / closed) of the current user. */
export function onProxyEvent(cb: Listener): () => void {
  listeners.add(cb)
  return () => listeners.delete(cb)
}

let installed = false

/** Keep the proxies list in sync with {type:'webproxy'} events (installed once). */
export function installProxyEvents(): void {
  if (installed) return
  installed = true
  events.onAny((raw) => {
    const ev = raw as unknown as ProxyEvent
    if (ev?.type !== 'webproxy' || !ev.proxy) return
    queryClient.setQueryData<ProxyInfo[]>(webproxyKeys.list, (prev) => {
      if (!prev) return prev
      const rest = prev.filter((p) => p.id !== ev.proxy.id)
      return ev.change === 'closed' ? rest : [...rest, ev.proxy]
    })
    for (const l of listeners) {
      try {
        l(ev)
      } catch (err) {
        console.error('[webproxy] listener failed', err)
      }
    }
  })
  events.on('hello', () => void queryClient.invalidateQueries({ queryKey: webproxyKeys.list }))
}
