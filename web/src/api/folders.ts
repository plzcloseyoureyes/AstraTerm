import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api, seg } from './client'
import { optimisticList, upsertById } from './optimistic'
import { queryKeys } from './queryKeys'
import type { Folder, FolderInput } from './types'

export const listFolders = () => api.get<Folder[]>('/api/folders')
const createFolder = (input: FolderInput) => api.post<Folder>('/api/folders', input)
export const updateFolder = (id: string, patch: Partial<FolderInput>) => api.patch<Folder>(`/api/folders/${seg(id)}`, patch)
/** Deletes a folder; with recursive=true also its sub-folders and connections (else children move to the parent). */
export const deleteFolder = (id: string, recursive = false) =>
  api.del<void>(`/api/folders/${seg(id)}`, undefined, { query: { recursive: recursive ? 1 : undefined } })

export function useFolders(enabled = true) {
  return useQuery({ queryKey: queryKeys.folders, queryFn: listFolders, enabled })
}

export function useCreateFolder() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: createFolder,
    onSuccess: (folder) => qc.setQueryData<Folder[]>(queryKeys.folders, (old) => upsertById(old, folder)),
    onSettled: () => qc.invalidateQueries({ queryKey: queryKeys.folders }),
  })
}

/** Update a folder (rename, recolour, move = parentId/sortOrder) with an optimistic cache update. */
export function useUpdateFolder() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: ({ id, patch }: { id: string; patch: Partial<FolderInput> }) => updateFolder(id, patch),
    ...optimisticList<Folder, { id: string; patch: Partial<FolderInput> }>(qc, queryKeys.folders, (items, { id, patch }) =>
      items.map((f) => (f.id === id ? { ...f, ...patch } : f)),
    ),
  })
}
