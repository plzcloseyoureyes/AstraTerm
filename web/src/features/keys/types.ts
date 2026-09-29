/*
 * Keys feature types: the module's extensions of SPEC §5.2 SSHKey / KnownHost and the request/response shapes of the
 * keys, known-hosts and agent endpoints (internal/keys, documented in SPEC §9 "keys").
 */
import type { SSHKey } from '@/api/types'

export type GeneratedKeyType = 'ed25519' | 'rsa' | 'ecdsa'

export type CertificateStatus = 'valid' | 'expired' | 'not_yet_valid'

export interface CertificateInfo {
  type: 'user' | 'host'
  keyId: string
  /** Decimal (uint64). */
  serial: string
  principals: string[]
  /** Absent: valid since forever. */
  validAfter?: string
  /** Absent: never expires. */
  validBefore?: string
  criticalOptions: Record<string, string>
  extensions: string[]
  keyType: string
  keyFingerprint: string
  caKeyType: string
  caFingerprint: string
  signatureType: string
  status: CertificateStatus
}

export interface KeyUsage {
  connections: number
  identities: number
}

/** GET /api/keys items: SSHKey plus module extensions. */
export interface StoredKey extends SSHKey {
  fingerprintMd5: string
  hasPrivateKey: boolean
  /** The passphrase of an encrypted key is remembered in the vault (no prompt when used). */
  passphraseSaved: boolean
  certificateInfo?: CertificateInfo
  usedBy: KeyUsage
}

export interface GenerateRequest {
  name?: string
  type: GeneratedKeyType
  bits?: number
  comment?: string
  passphrase?: string
  rememberPassphrase?: boolean
  /** false: keep the key as an in-memory draft (store it or export it afterwards). */
  store?: boolean
}

export interface KeyDraft {
  draftId: string
  type: string
  bits: number
  publicKey: string
  fingerprint: string
  fingerprintMd5: string
  comment: string
  hasPassphrase: boolean
  expiresAt: string
}

export interface ImportRequest {
  name?: string
  privateKey: string
  passphrase?: string
  comment?: string
  rememberPassphrase?: boolean
  certificate?: string
}

export interface InspectResult {
  kind: 'private' | 'public' | 'certificate'
  format?: 'openssh' | 'pem' | 'pkcs8' | 'ppk'
  ppkVersion?: number
  encrypted: boolean
  needsPassphrase: boolean
  wrongPassphrase: boolean
  type?: string
  bits?: number
  fingerprint?: string
  fingerprintMd5?: string
  publicKey?: string
  comment?: string
  certificate?: CertificateInfo
  existingKeyId?: string
  existingKeyName?: string
}

export type PrivateFormat = 'openssh' | 'ppk' | 'pem' | 'pkcs8'
export type PublicFormat = 'public' | 'rfc4716'
export type ExportFormat = PrivateFormat | PublicFormat

export interface ExportResult {
  content: string
  filename: string
  mime: string
  encrypted: boolean
}

export interface ExportRequest {
  format: ExportFormat
  /** Protect the output with the key's current passphrase. */
  keepPassphrase?: boolean
  /** Otherwise protect it with this passphrase ("" = unencrypted). */
  passphrase?: string
  /** Needed when the key's passphrase is not remembered. */
  currentPassphrase?: string
  ppkVersion?: 2 | 3
}

export interface ConvertRequest {
  privateKey: string
  passphrase?: string
  format: ExportFormat
  newPassphrase?: string
  comment?: string
  ppkVersion?: 2 | 3
  name?: string
}

export interface KeyPatch {
  name?: string
  comment?: string
  /** Certificate line; "" detaches. */
  certificate?: string
  /** Current passphrase: remember it ("" forgets the remembered one). */
  passphrase?: string
  /** Re-encrypt the stored key ("" removes its passphrase). */
  newPassphrase?: string
  rememberPassphrase?: boolean
}

export interface InstallResult {
  installed: boolean
  alreadyPresent: boolean
  method: 'sftp' | 'shell'
  path: string
  target: string
}

export interface SignRequest {
  publicKey?: string
  subjectKeyId?: string
  certType: 'user' | 'host'
  identity: string
  principals: string[]
  validAfter?: string
  validBefore?: string
  criticalOptions?: Record<string, string>
  extensions?: string[]
  caPassphrase?: string
  attach?: boolean
}

export interface SignResult {
  certificate: string
  info: CertificateInfo
  filename: string
  attached: boolean
}

export type MarkerKind = 'cert-authority' | 'revoked'

export interface HostKeyMarker {
  id: string
  marker: MarkerKind
  hosts: string
  keyType: string
  publicKey: string
  fingerprint: string
  comment: string
  createdAt: string
}

export interface KnownHostsLineError {
  line: number
  error: string
}

export interface KnownHostsImportResult {
  format: 'openssh' | 'putty'
  added: number
  replaced: number
  skipped: number
  conflicts: number
  hashed: number
  patterns: number
  markers: number
  invalid: number
  errors: KnownHostsLineError[]
}

export type KnownHostsConflict = 'skip' | 'replace' | 'add'

export interface AgentStatus {
  supported: boolean
  reason?: string
  running: boolean
  owner: boolean
  socketPath?: string
  platform: string
  startedAt?: string
  keyCount: number
  locked: boolean
  lockedByClient: boolean
  vaultLocked: boolean
  autostart: boolean
  clients: number
  signatures: number
  lastUsedAt?: string
}

export interface AgentKey {
  /** "k:<stored key id>" or "a:<hash>" (added with ssh-add). */
  id: string
  source: 'stored' | 'added'
  keyId?: string
  name: string
  type: string
  bits: number
  fingerprint: string
  comment: string
  loaded: boolean
  excluded: boolean
  unloaded: boolean
  unlocked: boolean
  needsPassphrase: boolean
  confirm: boolean
  certificate: boolean
  expiresAt?: string
}

/** Sub-tabs of the Keys tab. */
export type KeysTab = 'keys' | 'identities' | 'knownHosts' | 'agent'

export interface KeysTabParams {
  tab?: KeysTab
}
