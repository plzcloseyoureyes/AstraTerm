/*
 * "Place" of a file source: display label, bookmarks key, host info (scp:// URLs) and protocol — resolved from the
 * sessions / connections caches.
 */
import { useMemo } from 'react'
import { useConnections } from '@/api/connections'
import { useSessions } from '@/api/sessions'
import type { Connection, Protocol, RuntimeSession } from '@/api/types'
import { useFs, type UseFsResult } from './fsHandles'
import type { FsContext, FsSource } from './types'

export interface PlaceInfo {
  label: string
  placeKey: string
  protocol?: Protocol
  host?: { host: string; port?: number; username?: string }
  sessionId?: string
  connection?: Connection
  session?: RuntimeSession
}

export function describePlace(source: FsSource, sessions: RuntimeSession[] | undefined, connections: Connection[] | undefined): PlaceInfo {
  switch (source.kind) {
    case 'local':
      return { label: 'Local files', placeKey: 'local' }
    case 'session': {
      const s = sessions?.find((x) => x.id === source.sessionId)
      const c = s?.connectionId ? connections?.find((x) => x.id === s.connectionId) : undefined
      const host = s?.host ?? c?.host ?? ''
      const user = s?.username ?? c?.username ?? ''
      return {
        label: c?.name || (host ? `${user ? `${user}@` : ''}${host}` : s?.title || 'SSH session'),
        placeKey: c ? `conn:${c.id}` : `host:${user}@${host.toLowerCase()}:22`,
        protocol: s?.protocol ?? 'ssh',
        host: host ? { host, port: c?.port, username: user } : undefined,
        sessionId: source.sessionId,
        connection: c,
        session: s,
      }
    }
    case 'connection': {
      const c = connections?.find((x) => x.id === source.connectionId)
      return {
        label: (c?.name || 'Connection') + (source.sudo ? ' (root)' : ''),
        placeKey: `conn:${source.connectionId}`,
        protocol: c?.protocol,
        host: c ? { host: c.host, port: c.port, username: c.username } : undefined,
        connection: c,
      }
    }
    case 'quick': {
      const q = source.quick
      const host = q.host ?? ''
      return {
        label: q.name || `${q.username ? `${q.username}@` : ''}${host}`,
        placeKey: `host:${q.username ?? ''}@${host.toLowerCase()}:${q.port ?? ''}`,
        protocol: q.protocol,
        host: host ? { host, port: q.port, username: q.username } : undefined,
      }
    }
    case 'handle':
      return { label: 'Files', placeKey: `fs:${source.fsId}` }
  }
}

export function usePlace(source: FsSource | null): PlaceInfo | null {
  const sessions = useSessions()
  const conns = useConnections()
  return useMemo(() => (source ? describePlace(source, sessions.data, conns.data) : null), [source, sessions.data, conns.data])
}

/** Open a source and build its FsContext (null until the handle is ready). */
export function useFsContext(source: FsSource | null, enabled = true): { fs: UseFsResult; ctx: FsContext | null; place: PlaceInfo | null } {
  const fs = useFs(source, enabled)
  const place = usePlace(source)
  const ctx = useMemo<FsContext | null>(() => {
    if (!source || !fs.handle || !fs.key || !place) return null
    return {
      key: fs.key,
      handle: fs.handle,
      source,
      sessionId: place.sessionId,
      placeKey: place.placeKey,
      label: source.kind === 'handle' ? fs.handle.label || place.label : place.label,
      host: place.host,
    }
  }, [source, fs.handle, fs.key, place])
  return { fs, ctx, place }
}
