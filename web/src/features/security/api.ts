/*
 * REST functions + react-query hooks of the security feature (account security) and the admin feature. Keys live
 * under ['security', …] and ['admin', …]; the core auth hooks (tokens, login sessions) are in src/api/auth.ts.
 */
import { keepPreviousData, useInfiniteQuery, useQuery } from '@tanstack/react-query'
import { api, seg } from '@/api/client'
import type { User } from '@/api/types'
import { withReauthOrThrow } from './store'
import type {
  AdminUser,
  AuditFilter,
  AuditSearchResponse,
  CeremonyResponse,
  CreateUserInput,
  GuacdStatus,
  LoginPolicy,
  LoginPolicyResponse,
  MeInfo,
  NetworkPolicy,
  NetworkPolicyView,
  NetworkTestResult,
  OidcProvider,
  OidcProviderInput,
  OidcProvidersResponse,
  OidcTestResult,
  Passkey,
  PasskeyConfig,
  PasskeyStatus,
  SsoIdentity,
  SystemInfo,
} from './types'

export const secQK = {
  me: ['security', 'me'] as const,
  passkeys: ['security', 'passkeys'] as const,
  passkeyStatus: ['security', 'passkey-status'] as const,
  identities: ['security', 'sso-identities'] as const,
  users: ['admin', 'users'] as const,
  policy: ['admin', 'auth-policy'] as const,
  oidc: ['admin', 'oidc'] as const,
  passkeyConfig: ['admin', 'passkey-config'] as const,
  system: ['admin', 'system'] as const,
  guacd: ['admin', 'guacd'] as const,
  audit: (f: AuditFilter) => ['admin', 'audit', f] as const,
  netPolicy: ['admin', 'network-policy'] as const,
}

// --- account ---------------------------------------------------------------------------------------------------------

export const getMe = () => api.get<MeInfo>('/api/auth/me')
export const updateMe = (patch: { displayName?: string }) => api.patch<User>('/api/auth/me', patch)
export const revokeOtherSessions = () => api.post<{ revoked: number }>('/api/auth/sessions/revoke-others')
export const changePassword = (currentPassword: string, newPassword: string) =>
  api.post<void>('/api/auth/password', { currentPassword, newPassword })

export function useMe(enabled = true) {
  return useQuery({ queryKey: secQK.me, queryFn: getMe, staleTime: 15_000, enabled })
}

// --- passkeys ----------------------------------------------------------------------------------------------------------

export const listPasskeys = () => api.get<Passkey[]>('/api/auth/webauthn/credentials')
export const passkeyStatus = () => api.get<PasskeyStatus>('/api/auth/webauthn/status')
export const renamePasskey = (id: string, name: string) => api.patch<Passkey>(`/api/auth/webauthn/credentials/${seg(id)}`, { name })
export const deletePasskey = (id: string, password?: string) =>
  api.del<void>(`/api/auth/webauthn/credentials/${seg(id)}`, password ? { password } : undefined)
export const registerBegin = (password?: string) =>
  api.post<CeremonyResponse>('/api/auth/webauthn/register/begin', password ? { password } : {})
export const registerFinish = (ceremonyId: string, name: string, credential: unknown) =>
  api.post<Passkey>('/api/auth/webauthn/register/finish', { ceremonyId, name, credential })
export const loginBegin = (conditional = false) => api.post<CeremonyResponse>('/api/auth/webauthn/login/begin', { conditional })
export const loginFinish = (ceremonyId: string, credential: unknown, remember: boolean) =>
  api.post<{ user: User }>('/api/auth/webauthn/login/finish', { ceremonyId, credential, remember })
export const mfaBegin = (mfaToken: string) => api.post<CeremonyResponse>('/api/auth/webauthn/mfa/begin', { mfaToken })
export const mfaFinish = (mfaToken: string, ceremonyId: string, credential: unknown) =>
  api.post<{ user: User }>('/api/auth/webauthn/mfa/finish', { mfaToken, ceremonyId, credential })
export const verifyBegin = () => api.post<CeremonyResponse>('/api/auth/webauthn/verify/begin')
export const verifyFinish = (ceremonyId: string, credential: unknown) =>
  api.post<void>('/api/auth/webauthn/verify/finish', { ceremonyId, credential })

export function usePasskeys(enabled = true) {
  return useQuery({ queryKey: secQK.passkeys, queryFn: listPasskeys, enabled, staleTime: 15_000 })
}

export function usePasskeyStatus() {
  return useQuery({ queryKey: secQK.passkeyStatus, queryFn: passkeyStatus, staleTime: 5 * 60_000 })
}

// --- second factor (login step) -------------------------------------------------------------------------------------------

export const mfaTotp = (mfaToken: string, code: string) => api.post<{ user: User }>('/api/auth/mfa/totp', { mfaToken, code })
export const enrollTotpSetup = (mfaToken: string) =>
  api.post<{ secret: string; otpauthUrl: string }>('/api/auth/mfa/enroll/totp/setup', { mfaToken })
export const enrollTotpEnable = (mfaToken: string, code: string) =>
  api.post<{ user: User; recoveryCodes: string[] }>('/api/auth/mfa/enroll/totp/enable', { mfaToken, code })

// --- SSO identities ---------------------------------------------------------------------------------------------------------

export const listIdentities = () => api.get<SsoIdentity[]>('/api/auth/oidc/identities')
export const unlinkIdentity = (id: string) => api.del<void>(`/api/auth/oidc/identities/${seg(id)}`)

export function useIdentities(enabled = true) {
  return useQuery({ queryKey: secQK.identities, queryFn: listIdentities, enabled, staleTime: 30_000 })
}

// --- admin: users ------------------------------------------------------------------------------------------------------------

export const adminUsers = () => api.get<AdminUser[]>('/api/admin/users')
// Actions that could take over accounts or change how everyone signs in need a recent sign-in on the server
// (403 reauth_required → "Confirm it's you" dialog, then one retry). They reject with ReauthCancelled on cancel.
const SUDO = 'Administrator changes to accounts and sign-in settings need a recent sign-in.'

export const adminCreateUser = (req: CreateUserInput) => withReauthOrThrow(() => api.post<User>('/api/admin/users', req), SUDO)
export const adminUpdateUser = (id: string, patch: Partial<Pick<User, 'displayName' | 'role' | 'disabled'>>) =>
  withReauthOrThrow(() => api.patch<User>(`/api/admin/users/${seg(id)}`, patch), SUDO)
export const adminDeleteUser = (id: string) => api.del<void>(`/api/admin/users/${seg(id)}`)
export const adminResetPassword = (id: string, password: string) =>
  withReauthOrThrow(() => api.post<void>(`/api/admin/users/${seg(id)}/reset-password`, { password }), SUDO)
export const adminResetMfa = (id: string, what: { totp?: boolean; passkeys?: boolean } = {}) =>
  withReauthOrThrow(() => api.post<{ totpReset: boolean; passkeysRemoved: number }>(`/api/admin/users/${seg(id)}/reset-mfa`, what), SUDO)
export const adminUnlockUser = (id: string) => api.post<void>(`/api/admin/users/${seg(id)}/unlock`)
export const adminRevokeUserSessions = (id: string) => api.post<void>(`/api/admin/users/${seg(id)}/revoke-sessions`)

export function useAdminUsers() {
  return useQuery({ queryKey: secQK.users, queryFn: adminUsers, staleTime: 10_000 })
}

// --- admin: authentication settings ----------------------------------------------------------------------------------------

export const getPolicy = () => api.get<LoginPolicyResponse>('/api/admin/auth/policy')
export const putPolicy = (patch: Partial<LoginPolicy>) => withReauthOrThrow(() => api.put<LoginPolicyResponse>('/api/admin/auth/policy', patch), SUDO)
export const getOidc = () => api.get<OidcProvidersResponse>('/api/admin/oidc/providers')
export const createOidc = (p: OidcProviderInput) => withReauthOrThrow(() => api.post<OidcProvider>('/api/admin/oidc/providers', p), SUDO)
export const updateOidc = (id: string, p: OidcProviderInput) =>
  withReauthOrThrow(() => api.patch<OidcProvider>(`/api/admin/oidc/providers/${seg(id)}`, p), SUDO)
export const deleteOidc = (id: string) => withReauthOrThrow(() => api.del<void>(`/api/admin/oidc/providers/${seg(id)}`), SUDO)
export const testOidc = (req: { id?: string; issuer?: string }) => api.post<OidcTestResult>('/api/admin/oidc/test', req)
export const getPasskeyConfig = () => api.get<PasskeyConfig>('/api/admin/webauthn/config')
export const putPasskeyConfig = (cfg: { rpId: string; origins: string[] }) =>
  withReauthOrThrow(() => api.put<PasskeyConfig>('/api/admin/webauthn/config', cfg), SUDO)

export function usePolicy() {
  return useQuery({ queryKey: secQK.policy, queryFn: getPolicy, staleTime: 30_000 })
}
export function useOidc() {
  return useQuery({ queryKey: secQK.oidc, queryFn: getOidc, staleTime: 30_000 })
}
export function usePasskeyConfig() {
  return useQuery({ queryKey: secQK.passkeyConfig, queryFn: getPasskeyConfig, staleTime: 60_000 })
}

// --- admin: system ---------------------------------------------------------------------------------------------------------

export const getSystem = () => api.get<SystemInfo>('/api/admin/system')
export const getGuacd = () => api.get<GuacdStatus>('/api/guacd/status')

export function useSystem(enabled: boolean) {
  return useQuery({ queryKey: secQK.system, queryFn: getSystem, enabled, refetchInterval: enabled ? 15_000 : false, placeholderData: keepPreviousData })
}
export function useGuacd(enabled: boolean) {
  return useQuery({ queryKey: secQK.guacd, queryFn: getGuacd, enabled, retry: false, staleTime: 30_000 })
}

// --- admin: audit ------------------------------------------------------------------------------------------------------------

const AUDIT_PAGE = 100

export const searchAudit = (f: AuditFilter, before?: number) =>
  api.get<AuditSearchResponse>('/api/admin/audit/search', {
    query: { ...f, q: f.q || undefined, userId: f.userId || undefined, action: f.action || undefined, limit: AUDIT_PAGE, before },
  })

export function useAuditSearch(f: AuditFilter, enabled: boolean) {
  return useInfiniteQuery({
    queryKey: secQK.audit(f),
    queryFn: ({ pageParam }) => searchAudit(f, pageParam),
    initialPageParam: undefined as number | undefined,
    getNextPageParam: (last) => last.nextBefore,
    enabled,
    placeholderData: keepPreviousData,
    staleTime: 5_000,
  })
}

// --- admin: network policy (netguard module; may be absent) --------------------------------------------------------------------

export const getNetworkPolicy = () => api.get<NetworkPolicyView>('/api/admin/network-policy')
export const putNetworkPolicy = (p: Partial<NetworkPolicy>) => api.put<NetworkPolicyView>('/api/admin/network-policy', p)
export const testNetworkPolicy = (req: { host: string; port: number; userId?: string; policy?: NetworkPolicy }) =>
  api.post<NetworkTestResult>('/api/admin/network-policy/test', req)

export function useNetworkPolicy(enabled: boolean) {
  return useQuery({ queryKey: secQK.netPolicy, queryFn: getNetworkPolicy, enabled, retry: false, staleTime: 30_000 })
}
