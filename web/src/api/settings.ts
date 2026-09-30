import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api } from './client'
import { queryKeys } from './queryKeys'
import type { SettingsObject } from './types'

/** User settings merged over global defaults. Top-level keys are settings sections (see src/stores/settings.ts). */
export const getSettings = () => api.get<SettingsObject>('/api/settings')
/** Partial merge: each top-level key present in `patch` replaces that stored key. */
export const putSettings = (patch: SettingsObject) => api.put<SettingsObject | void>('/api/settings', patch)

const getAdminSettings = () => api.get<SettingsObject>('/api/admin/settings')
const putAdminSettings = (patch: SettingsObject) => api.put<SettingsObject | void>('/api/admin/settings', patch)

export function useAdminSettings(enabled = true) {
  return useQuery({ queryKey: queryKeys.adminSettings, queryFn: getAdminSettings, enabled })
}

export function useUpdateAdminSettings() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: putAdminSettings,
    onSettled: () => {
      qc.invalidateQueries({ queryKey: queryKeys.adminSettings })
      qc.invalidateQueries({ queryKey: queryKeys.settings })
    },
  })
}
