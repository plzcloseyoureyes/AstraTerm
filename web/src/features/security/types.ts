/*
 * Types of the security / admin endpoints (SPEC §9 "security"). Core types (User, APIToken, AuthSessionInfo,
 * AuditEntry, RuntimeSession) live in src/api/types.ts.
 */
import type { AuditEntry, Role, User } from '@/api/types'

/** GET /api/auth/me */
export interface MeInfo {
  user: User
  hasPassword: boolean
  passwordChangedAt?: string
  passkeys: number
  recoveryCodesLeft: number
  mfaRequired: boolean
  /** Sensitive changes are authorized without asking again (recent sign-in or re-verification). */
  reauthFresh: boolean
  authMethod: 'cookie' | 'token' | ''
}

/** GET /api/auth/state extras added by the security module. */
export interface LoginMethods {
  password: boolean
  passkey: boolean
  sso: { id: string; name: string }[]
  remember: boolean
}

export interface PasswordPolicyView {
  minLength: number
  requireClasses: number
  disallowUsername: boolean
}

export interface Passkey {
  id: string
  name: string
  rpId: string
  authenticator?: string
  aaguid?: string
  discoverable: boolean
  synced: boolean
  backupEligible: boolean
  userVerified: boolean
  transports: string[]
  createdAt: string
  lastUsedAt?: string
}

export interface PasskeyStatus {
  available: boolean
  rpId?: string
  origin?: string
  reason?: string
  code?: string
}

export interface CeremonyResponse {
  ceremonyId: string
  // PublicKeyCredential{Creation,Request}OptionsJSON
  options: any
  mediation?: string
}

/** 401 body of a password login that needs a second factor. */
export interface MfaChallenge {
  code: 'totp_required' | 'mfa_required' | 'mfa_enrollment_required'
  methods: ('totp' | 'webauthn')[]
  mfaToken: string
  expiresIn: number
}

export interface SsoIdentity {
  id: string
  providerId: string
  providerName: string
  subject: string
  email?: string
  username?: string
  createdAt: string
  lastLoginAt?: string
}

// --- admin ------------------------------------------------------------------------------------------------------------

export interface AdminUser extends User {
  hasPassword: boolean
  passkeys: number
  sso: string[]
  locked: boolean
  lockedUntil?: string
  failedLogins: number
  sessions: number
}

export type RequireMfa = 'off' | 'admins' | 'all'

export interface LoginPolicy {
  passwordMinLength: number
  passwordRequireClasses: number
  passwordDisallowUsername: boolean
  lockoutThreshold: number
  lockoutMaxMinutes: number
  accountLockThreshold: number
  accountLockMinutes: number
  sessionIdleHours: number
  rememberDays: number
  sessionMaxDays: number
  requireMfa: RequireMfa
  passwordLogin: boolean
  allowedNetworks: string[]
}

export interface LoginPolicyResponse extends LoginPolicy {
  defaults: LoginPolicy
}

export interface OidcProvider {
  id: string
  name: string
  enabled: boolean
  issuer: string
  clientId: string
  scopes: string[]
  usernameClaim: string
  emailClaim: string
  displayNameClaim: string
  groupsClaim: string
  adminGroups: string[]
  allowedGroups: string[]
  syncRole: boolean
  autoProvision: boolean
  linkByUsername: boolean
  requireVerifiedEmail: boolean
  prompt: '' | 'login' | 'consent' | 'select_account'
  hasClientSecret: boolean
  createdAt?: string
  updatedAt?: string
}

export type OidcProviderInput = Partial<Omit<OidcProvider, 'hasClientSecret' | 'createdAt' | 'updatedAt'>> & {
  /** Write-only: omitted = unchanged, "" = remove. */
  clientSecret?: string
}

export interface OidcProvidersResponse {
  providers: OidcProvider[]
  redirectUri: string
}

export interface OidcTestResult {
  ok: boolean
  issuer?: string
  authorizationEndpoint?: string
  tokenEndpoint?: string
  userinfoEndpoint?: string
  scopesSupported?: string[]
  claimsSupported?: string[]
  pkceMethods?: string[]
  error?: string
  latencyMs: number
  redirectUri: string
}

export interface PasskeyConfig {
  rpId: string
  origins: string[]
  requestOrigin: string
  requestHost: string
}

export interface SystemInfo {
  version: string
  mode: 'desktop' | 'server'
  dataDir: string
  listen: string
  tls: boolean
  tlsSelfSigned: boolean
  insecureHttp: boolean
  trustedProxies: string[]
  guacd: string
  goVersion: string
  os: string
  arch: string
  cpus: number
  pid: number
  hostname: string
  startedAt: string
  uptimeSec: number
  dbSizeBytes: number
  users: number
  admins: number
  loginSessions: number
  eventClients: number
  vaultLocked: boolean
  vaultHasMasterPassword: boolean
  detachedTtlSec: number
  scrollbackBytes: number
  features: Record<string, boolean>
}

export interface GuacdStatus {
  configured: boolean
  reachable: boolean
  version?: string
  defaultEngine?: string
  address?: string
  source?: string
  error?: string
}

export interface AuditSearchResponse {
  entries: AuditEntry[]
  nextBefore?: number
}

export interface AuditFilter {
  q?: string
  userId?: string
  action?: string
  since?: string
  until?: string
}

export interface CreateUserInput {
  username: string
  password: string
  displayName?: string
  role: Role
}

// --- network policy (SEC-7, netguard module) -----------------------------------------------------------------------------

export interface NetworkPolicy {
  allowPrivate: boolean
  blockHostAddresses: boolean
  deny: string[]
  allow: string[]
  allowedPorts: string
  applyToAdmins: boolean
}

export interface NetworkPolicyView {
  policy: NetworkPolicy
  default: NetworkPolicy
  mode: string
  enforced: boolean
  listen: string
  builtin: { cidr: string; class: string; reason: string }[]
  privateRanges: string[]
  hostAddresses: string[]
}

export interface NetworkTestResult {
  host: string
  port: number
  decision: 'allow' | 'deny' | 'unrestricted' | 'error'
  allowed: boolean
  restricted: boolean
  reason: string
  user?: { id: string; username: string; role: Role }
  addresses: { ip: string; port?: number; allowed: boolean; class: string; rule?: string; reason: string }[]
  error?: string
}
