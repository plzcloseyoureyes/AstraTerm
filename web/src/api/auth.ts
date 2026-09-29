import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api, seg } from './client'
import { queryKeys } from './queryKeys'
import type {
  APIToken,
  APITokenCreated,
  AuthSessionInfo,
  AuthState,
  ChangePasswordRequest,
  LoginRequest,
  LoginResponse,
  Role,
  SetupRequest,
  TotpSetupResponse,
  User,
} from './types'

// --- account / session ------------------------------------------------------------------------------------------------

export const getAuthState = (signal?: AbortSignal) => api.get<AuthState>('/api/auth/state', { signal })
export const setup = (req: SetupRequest) => api.post<LoginResponse>('/api/auth/setup', req)
export const login = (req: LoginRequest) => api.post<LoginResponse>('/api/auth/login', req)
export const logout = () => api.post<void>('/api/auth/logout')
export const launch = (token: string) => api.post<LoginResponse>('/api/auth/launch', { token })
export const changePassword = (req: ChangePasswordRequest) => api.post<void>('/api/auth/password', req)
/** Re-authenticates the current user (lock screen, step-up). Resolves on success; wrong password → ApiError 403 `invalid_password`. */
export const verifyPassword = (password: string) => api.post<void>('/api/auth/verify-password', { password })

// --- TOTP ------------------------------------------------------------------------------------------------------------

export const totpSetup = () => api.post<TotpSetupResponse>('/api/auth/totp/setup')
export const totpEnable = (code: string) => api.post<{ recoveryCodes: string[] }>('/api/auth/totp/enable', { code })
export const totpDisable = (password: string) => api.post<void>('/api/auth/totp/disable', { password })
/** Replace the recovery codes (shown once). */
export const regenerateRecoveryCodes = (password: string) =>
  api.post<{ recoveryCodes: string[] }>('/api/auth/totp/recovery-codes', { password })

// --- API tokens --------------------------------------------------------------------------------------------------------

export const listTokens = () => api.get<APIToken[]>('/api/auth/tokens')
export const createToken = (req: { name: string; expiresInDays?: number }) => api.post<APITokenCreated>('/api/auth/tokens', req)
export const deleteToken = (id: string) => api.del<void>(`/api/auth/tokens/${seg(id)}`)

export function useApiTokens() {
  return useQuery({ queryKey: queryKeys.authTokens, queryFn: listTokens })
}

// --- browser login sessions ------------------------------------------------------------------------------------------

export const listAuthSessions = () => api.get<AuthSessionInfo[]>('/api/auth/sessions')
export const revokeAuthSession = (id: string) => api.del<void>(`/api/auth/sessions/${seg(id)}`)

export function useAuthSessions() {
  return useQuery({ queryKey: queryKeys.authSessions, queryFn: listAuthSessions })
}

export function useRevokeAuthSession() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: revokeAuthSession,
    onSettled: () => qc.invalidateQueries({ queryKey: queryKeys.authSessions }),
  })
}

// --- admin: users ----------------------------------------------------------------------------------------------------

export interface CreateUserRequest {
  username: string
  password: string
  displayName?: string
  role: Role
}
export type UpdateUserRequest = Partial<Pick<User, 'displayName' | 'role' | 'disabled'>>

export const adminListUsers = () => api.get<User[]>('/api/admin/users')
export const adminCreateUser = (req: CreateUserRequest) => api.post<User>('/api/admin/users', req)
export const adminUpdateUser = (id: string, patch: UpdateUserRequest) => api.patch<User>(`/api/admin/users/${seg(id)}`, patch)
export const adminDeleteUser = (id: string) => api.del<void>(`/api/admin/users/${seg(id)}`)
export const adminResetPassword = (id: string, password: string) =>
  api.post<void>(`/api/admin/users/${seg(id)}/reset-password`, { password })
