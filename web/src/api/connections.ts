import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api, seg } from './client'
import { optimisticList, upsertById } from './optimistic'
import { queryKeys } from './queryKeys'
import type { Connection, ConnectionInput, ConnectionPatch, ReorderItem } from './types'

export const listConnections = () => api.get<Connection[]>('/api/connections')
export const getConnection = (id: string) => api.get<Connection>(`/api/connections/${seg(id)}`)
const createConnection = (input: Partial<ConnectionInput> & Pick<ConnectionInput, 'name' | 'protocol'>) =>
  api.post<Connection>('/api/connections', input)
export const updateConnection = (id: string, patch: ConnectionPatch) => api.patch<Connection>(`/api/connections/${seg(id)}`, patch)
export const deleteConnection = (id: string) => api.del<void>(`/api/connections/${seg(id)}`)
export const duplicateConnection = (id: string) => api.post<Connection>(`/api/connections/${seg(id)}/duplicate`)
export const reorderConnections = (items: ReorderItem[]) => api.post<void>('/api/connections/reorder', { items })
export const bulkDeleteConnections = (ids: string[]) => api.post<void>('/api/connections/bulk-delete', { ids })

export function useConnections(enabled = true) {
  return useQuery({ queryKey: queryKeys.connections, queryFn: listConnections, enabled })
}

export function useConnection(id: string | undefined | null) {
  const qc = useQueryClient()
  return useQuery({
    queryKey: queryKeys.connection(id ?? ''),
    placeholderData: undefined, // never show another connection's settings under this key
    queryFn: () => getConnection(id!),
    enabled: !!id,
    // Seed from the list cache so editors open instantly.
    initialData: () => (id ? qc.getQueryData<Connection[]>(queryKeys.connections)?.find((c) => c.id === id) : undefined),
  })
}

export function useCreateConnection() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: createConnection,
    onSuccess: (conn) => {
      qc.setQueryData<Connection[]>(queryKeys.connections, (old) => upsertById(old, conn))
      qc.setQueryData(queryKeys.connection(conn.id), conn)
    },
    onSettled: () => qc.invalidateQueries({ queryKey: queryKeys.connections }),
  })
}

/** Patch a connection with an optimistic update of the list cache (rename, favourite, move, tags...). */
export function useUpdateConnection() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: ({ id, patch }: { id: string; patch: ConnectionPatch }) => updateConnection(id, patch),
    ...optimisticList<Connection, { id: string; patch: ConnectionPatch }>(qc, queryKeys.connections, (items, { id, patch }) =>
      items.map((c) => {
        if (c.id !== id) return c
        // Secrets are write-only: never merge them into the cache.
        const { secrets: _secrets, ...rest } = patch
        return { ...c, ...rest }
      }),
    ),
  })
}

/** Connections ordered by most recent use (never-used last). */
export function recentConnections(conns: Connection[] | undefined, limit = 10): Connection[] {
  if (!conns) return []
  return conns
    .filter((c) => !!c.lastUsedAt)
    .sort((a, b) => Date.parse(b.lastUsedAt!) - Date.parse(a.lastUsedAt!))
    .slice(0, limit)
}
