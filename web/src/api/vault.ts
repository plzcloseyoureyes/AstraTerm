import { useQuery } from '@tanstack/react-query'
import { api } from './client'
import { queryKeys } from './queryKeys'
import type { MasterPasswordRequest, VaultStatus } from './types'

export const getVaultStatus = () => api.get<VaultStatus>('/api/vault/status')
export const unlockVault = (password: string) => api.post<void>('/api/vault/unlock', { password })
export const lockVault = () => api.post<void>('/api/vault/lock')
/** Set / change / remove (newPassword = "") the master password. Admin only. */
export const setMasterPassword = (req: MasterPasswordRequest) => api.post<void>('/api/vault/master-password', req)

export function useVaultStatus(enabled = true) {
  return useQuery({ queryKey: queryKeys.vaultStatus, queryFn: getVaultStatus, enabled })
}
