import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api, seg } from './client'
import { optimisticList, removeByIds, upsertById } from './optimistic'
import { queryKeys } from './queryKeys'
import type { Identity, IdentityInput } from './types'

export const listIdentities = () => api.get<Identity[]>('/api/identities')
export const createIdentity = (input: IdentityInput) => api.post<Identity>('/api/identities', input)
export const updateIdentity = (id: string, patch: Partial<IdentityInput>) => api.patch<Identity>(`/api/identities/${seg(id)}`, patch)
export const deleteIdentity = (id: string) => api.del<void>(`/api/identities/${seg(id)}`)

export function useIdentities(enabled = true) {
  return useQuery({ queryKey: queryKeys.identities, queryFn: listIdentities, enabled })
}

export function useCreateIdentity() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: createIdentity,
    onSuccess: (idn) => qc.setQueryData<Identity[]>(queryKeys.identities, (old) => upsertById(old, idn)),
    onSettled: () => qc.invalidateQueries({ queryKey: queryKeys.identities }),
  })
}

export function useUpdateIdentity() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: ({ id, patch }: { id: string; patch: Partial<IdentityInput> }) => updateIdentity(id, patch),
    ...optimisticList<Identity, { id: string; patch: Partial<IdentityInput> }>(qc, queryKeys.identities, (items, { id, patch }) =>
      items.map((i) => {
        if (i.id !== id) return i
        const { secrets: _secrets, ...rest } = patch
        return { ...i, ...rest }
      }),
    ),
  })
}

export function useDeleteIdentity() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: deleteIdentity,
    ...optimisticList<Identity, string>(qc, queryKeys.identities, (items, id) => removeByIds(items, id), [queryKeys.connections]),
  })
}
