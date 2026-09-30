import { api } from './client'
import type { MasterPasswordRequest } from './types'

export const unlockVault = (password: string) => api.post<void>('/api/vault/unlock', { password })
export const lockVault = () => api.post<void>('/api/vault/lock')
/** Set / change / remove (newPassword = "") the master password. Admin only. */
export const setMasterPassword = (req: MasterPasswordRequest) => api.post<void>('/api/vault/master-password', req)
