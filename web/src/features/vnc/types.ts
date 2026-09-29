/*
 * VNC feature types (SPEC §9 "vnc"): tab params, GET /api/sessions/:id/vnc-info, reverse listeners, trusted
 * certificates, and the custom events pushed over /ws/events.
 */
import type { Connection, Protocol } from '@/api/types'

/** Params of a `vnc` tab (see terminal/open.ts openSessionTab). */
export interface VncTabParams {
  sessionId: string
  protocol?: Protocol
  connectionId?: string
  quick?: Partial<Connection>
  color?: string
  title?: string
  /** Incoming (reverse) connection: cannot be re-established from Termstead. */
  reverse?: boolean
  /** Start in view-only mode (listener option for incoming connections). */
  viewOnly?: boolean
}

export type ScalingMode = 'fit' | 'remote-resize' | 'none'

/**
 * options.encryption (internal/vnc/policy.go): `require` = VeNCrypt TLS only; `prefer` (default) = TLS when offered,
 * ask (close 4426) before weak TLS / an unencrypted fallback / a clear-text password; `allow-weak` = also accept a
 * 1024–2047-bit anonymous TLS group; `allow-unencrypted` = also fall back without asking.
 */
export type EncryptionPolicy = 'require' | 'prefer' | 'allow-weak' | 'allow-unencrypted'

/** Clipboard direction (options.clipboardDirection, admin vncPolicy.clipboardDirection, user setting). */
export type ClipboardDirection = 'both' | 'to-remote' | 'from-remote' | 'none'

/** A connection waiting for the user's confirmation of weaker security (vnc-info `confirm`, close 4426). */
export interface VncConfirmInfo {
  reason: string
  /** The server's anonymous TLS works with a weak key exchange (dhBits): reconnect with ?allow=weak. */
  weakTls: boolean
  dhBits?: number
  /** An unencrypted security type is available: reconnect with ?allow=unencrypted. */
  unencrypted: boolean
  /** That type sends the password in clear text (VeNCrypt Plain without TLS). */
  cleartext: boolean
}

export interface VncTLSInfo {
  version: string
  cipherSuite: string
  anonymous: boolean
  group?: string
  subject?: string
  issuer?: string
  notAfter?: string
  fingerprint?: string
  /** none (anonymous TLS) | system | saved | accepted | accepted-saved */
  trust?: string
  /** Finite-field Diffie-Hellman modulus size of anonymous TLS (absent for ECDH). */
  dhBits?: number
  /** Anonymous TLS with a group below 2048 bits (accepted only after confirmation). */
  weak?: boolean
  /** RFC 7366 encrypt-then-MAC (CBC suites). */
  encryptThenMac?: boolean
}

export interface VncInfo {
  sessionId: string
  connected: boolean
  viewers: number
  host?: string
  port?: number
  serverVersion?: string
  protocol?: string
  security?: string
  encrypted: boolean
  tls?: VncTLSInfo
  passthrough: boolean
  desktopName?: string
  width?: number
  height?: number
  route?: string
  reverse: boolean
  connectedAt?: string
  lastError?: string
  /** Effective options.encryption policy, including confirmations given for this session. */
  encryptionPolicy?: EncryptionPolicy
  /** Why the connection has less protection than the server offered. */
  downgrade?: string
  /** The login (VeNCrypt Plain without TLS) crossed the network in clear text. */
  passwordCleartext?: boolean
  /** Set while the session waits for the user's confirmation (close 4426). */
  confirm?: VncConfirmInfo
  /** Effective clipboard direction (connection option ∩ administrator policy). */
  clipboard?: ClipboardDirection
}

export interface VncListener {
  id: string
  ownerId: string
  bindHost: string
  port: number
  address: string
  hasPassword: boolean
  viewOnly: boolean
  accepted: number
  pending: number
  lastFrom?: string
  lastAt?: string
  createdAt: string
}

export interface StartListenerRequest {
  port: number
  bindHost?: string
  password?: string
  viewOnly?: boolean
}

export interface TrustedCert {
  id: string
  host: string
  port: number
  fingerprint: string
  subject: string
  issuer: string
  notAfter?: string
  comment: string
  createdAt: string
}

/** {type:'vnc.incoming'} — a VNC server connected to one of the user's listeners. */
export interface VncIncomingEvent {
  type: 'vnc.incoming'
  sessionId: string
  listenerId: string
  from: string
  title: string
  viewOnly?: boolean
}

/** WebSocket close codes of /ws/vnc/:id (internal/vnc/viewer.go). */
export const VNC_CLOSE = {
  serverEnded: 1000,
  shutdown: 1001,
  badRequest: 4400,
  authFailed: 4401,
  forbidden: 4403,
  notFound: 4404,
  unavailable: 4409,
  sessionClosed: 4410,
  locked: 4423,
  insecure: 4426,
  canceled: 4499,
  internal: 4500,
  connectFailed: 4502,
  timeout: 4504,
  unsupported: 4505,
} as const

/** Close codes after which retrying cannot help (PROTO-39: never auto-retry after an auth failure). */
export const PERMANENT_CLOSE_CODES: ReadonlySet<number> = new Set([
  VNC_CLOSE.insecure,
  VNC_CLOSE.badRequest,
  VNC_CLOSE.authFailed,
  VNC_CLOSE.forbidden,
  VNC_CLOSE.notFound,
  VNC_CLOSE.unavailable,
  VNC_CLOSE.sessionClosed,
  VNC_CLOSE.canceled,
  VNC_CLOSE.unsupported,
])
