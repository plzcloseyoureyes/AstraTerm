/*
 * Shared JSON contract types — the single source of truth on the frontend.
 * Mirrors internal/model (Go) field-for-field; see docs/SPEC.md §5 and §6.
 * Feature-specific extra types belong in src/features/<f>/types.ts (SPEC §10).
 */

// ---------------------------------------------------------------------------------------------------------------------
// §5.2 Core entities
// ---------------------------------------------------------------------------------------------------------------------

export type Role = 'admin' | 'user'

export interface User {
  id: string
  username: string
  displayName: string
  role: Role
  totpEnabled: boolean
  disabled: boolean
  createdAt: string
  lastLoginAt?: string
}

export type Protocol =
  | 'ssh'
  | 'telnet'
  | 'rlogin'
  | 'raw'
  | 'serial'
  | 'local'
  | 'mosh'
  | 'docker'
  | 'kube'
  | 'sftp'
  | 'ftp'
  | 's3'
  | 'vnc'
  | 'rdp'
  | 'winrm'
  | 'ipmi'
  | 'web'

export const PROTOCOLS: readonly Protocol[] = [
  'ssh',
  'telnet',
  'rlogin',
  'raw',
  'serial',
  'local',
  'mosh',
  'docker',
  'kube',
  'sftp',
  'ftp',
  's3',
  'vnc',
  'rdp',
  'winrm',
  'ipmi',
  'web',
] as const

export interface Folder {
  id: string
  parentId?: string | null
  name: string
  color?: string
  icon?: string
  sortOrder: number
  shared: boolean
  ownerId: string
}

export type AuthMethod = 'auto' | 'password' | 'key' | 'agent' | 'keyboard-interactive' | 'none'

export interface Connection {
  id: string
  folderId?: string | null
  name: string
  protocol: Protocol
  host: string
  port: number
  username: string
  identityId?: string | null
  keyId?: string | null
  authMethod: AuthMethod
  color?: string
  icon?: string
  tags: string[]
  notes: string
  favorite: boolean
  sortOrder: number
  /** Protocol-specific options (§5.3). Unknown keys are preserved by the backend. */
  options: ConnectionOptions
  /** WRITE-ONLY. Omitted key = unchanged, "" = delete. */
  secrets?: Record<string, string>
  /** READ-ONLY. Names of secrets that are set, e.g. ["password", "passphrase"]. */
  secretKeys: string[]
  shared: boolean
  ownerId: string
  lastUsedAt?: string
  createdAt: string
  updatedAt: string
}

/** Known secret keys (§5.3). */
export type SecretKey =
  | 'password'
  | 'passphrase'
  | 'proxyPassword'
  | 'vncPassword'
  | 'secretAccessKey'
  | 'gatewayPassword'
  | 'sudoPassword'

export interface Identity {
  id: string
  name: string
  username: string
  keyId?: string | null
  secrets?: Record<string, string>
  secretKeys: string[]
  createdAt: string
  updatedAt: string
}

export interface SSHKey {
  id: string
  name: string
  type: string
  bits: number
  publicKey: string
  fingerprint: string
  comment: string
  hasPassphrase: boolean
  certificate?: string
  createdAt: string
}

export interface KnownHost {
  id: string
  host: string
  port: number
  keyType: string
  publicKey: string
  fingerprint: string
  comment: string
  createdAt: string
}

export interface Snippet {
  id: string
  name: string
  folder: string
  description: string
  content: string
  tags: string[]
  sendMode: 'paste' | 'execute'
  shortcut?: string
  createdAt: string
  updatedAt: string
}

export interface MacroStep {
  data: string
  delayMs: number
}

export interface Macro {
  id: string
  name: string
  steps: MacroStep[]
  createdAt: string
  updatedAt: string
}

export type TunnelType = 'local' | 'remote' | 'dynamic'

export interface TunnelStatus {
  state: 'stopped' | 'starting' | 'running' | 'error'
  error?: string
  activeConns: number
  totalConns: number
  bytesIn: number
  bytesOut: number
  startedAt?: string
}

export interface Tunnel {
  id: string
  name: string
  type: TunnelType
  connectionId: string
  bindHost: string
  bindPort: number
  destHost: string
  destPort: number
  autoStart: boolean
  status: TunnelStatus
  createdAt: string
  updatedAt: string
}

export type SessionKind = 'terminal' | 'vnc' | 'rdp'
export type SessionState = 'connecting' | 'authenticating' | 'connected' | 'disconnected' | 'closed' | 'error'

export interface RuntimeSession {
  id: string
  kind: SessionKind
  protocol: Protocol
  connectionId?: string
  title: string
  host?: string
  username?: string
  state: SessionState
  stateMessage?: string
  exitCode?: number
  cols: number
  rows: number
  clients: number
  cwd?: string
  recording: boolean
  recordingId?: string
  logging: boolean
  ownerId: string
  createdAt: string
  connectedAt?: string
}

// ---------------------------------------------------------------------------------------------------------------------
// §5.3 ConnectionOptions — one flat object; every protocol reads its own keys.
// ---------------------------------------------------------------------------------------------------------------------

export interface TerminalOverrides {
  fontFamily?: string
  fontSize?: number
  theme?: string
  cursorStyle?: 'block' | 'underline' | 'bar'
  scrollback?: number
  [key: string]: unknown
}

export interface ProxyOptions {
  type: 'none' | 'socks5' | 'socks4' | 'http'
  host: string
  port: number
  username: string
}

export type LineEnding = 'crlf' | 'lf' | 'cr'

export interface ConnectionOptions {
  // common
  term?: string
  encoding?: string
  backspace?: 'del' | 'ctrl-h'
  startupCommand?: string
  autoReconnect?: boolean
  record?: boolean
  log?: boolean
  terminal?: TerminalOverrides
  keepAliveSec?: number
  // ssh
  jumpHosts?: string[]
  proxy?: ProxyOptions
  agentForwarding?: boolean
  /** ssh: boolean (enable compression); vnc: 0-9 compression level. */
  compression?: boolean | number
  x11Forwarding?: boolean
  env?: Record<string, string>
  remoteCommand?: string
  followCwd?: boolean
  monitoring?: boolean
  sftpRoot?: string
  ciphers?: string[]
  kex?: string[]
  macs?: string[]
  hostKeyAlgorithms?: string[]
  legacyAlgorithms?: boolean
  connectTimeoutSec?: number
  portKnock?: { port: number; proto: 'tcp' | 'udp' }[]
  useAgent?: boolean
  // telnet
  negotiate?: boolean
  // rlogin / raw / serial
  localEcho?: boolean
  lineEnding?: LineEnding
  // serial
  device?: string
  baud?: number
  dataBits?: number
  parity?: 'none' | 'odd' | 'even' | 'mark' | 'space'
  stopBits?: '1' | '1.5' | '2'
  flowControl?: 'none' | 'rtscts' | 'xonxoff'
  hexView?: boolean
  // local
  shell?: string
  args?: string[]
  cwd?: string
  // mosh
  moshPorts?: string
  predict?: 'adaptive' | 'always' | 'never'
  // docker
  dockerHost?: string
  container?: string
  user?: string
  viaConnectionId?: string
  // kube
  context?: string
  namespace?: string
  pod?: string
  // sftp / ftp / s3
  ftpTls?: 'none' | 'explicit' | 'implicit'
  passive?: boolean
  insecureTls?: boolean
  endpoint?: string
  region?: string
  bucket?: string
  pathStyle?: boolean
  accessKeyId?: string
  initialPath?: string
  // vnc
  viewOnly?: boolean
  quality?: number
  shared?: boolean
  sshTunnelVia?: string
  scaling?: 'fit' | 'remote-resize' | 'none'
  // rdp
  rdpEngine?: 'ironrdp' | 'guacd'
  domain?: string
  security?: 'any' | 'nla' | 'tls' | 'rdp' | 'vmconnect'
  ignoreCert?: boolean
  width?: number
  height?: number
  dpi?: number
  colorDepth?: 8 | 16 | 24 | 32
  resizeMethod?: 'display-update' | 'reconnect'
  enableAudio?: boolean
  enableMic?: boolean
  enableDrive?: boolean
  driveName?: string
  enablePrinting?: boolean
  disableClipboard?: boolean
  console?: boolean
  initialProgram?: string
  serverLayout?: string
  timezone?: string
  gatewayHost?: string
  gatewayPort?: number
  gatewayUsername?: string
  gatewayDomain?: string
  enableWallpaper?: boolean
  enableTheming?: boolean
  enableFontSmoothing?: boolean
  recording?: boolean
  /** Unknown keys are preserved (forward compatibility). */
  [key: string]: unknown
}

// ---------------------------------------------------------------------------------------------------------------------
// §6.0 Auth & account
// ---------------------------------------------------------------------------------------------------------------------

export type RunMode = 'desktop' | 'server'

export interface ServerFeatures {
  guacd: boolean
  docker: boolean
  kubectl: boolean
  mosh: boolean
  wsl: boolean
  [key: string]: boolean
}

export interface AuthState {
  setupRequired: boolean
  /** Setup must present the one-time token from the startup banner's ?setup= link (server mode, non-loopback binds). */
  setupTokenRequired?: boolean
  authenticated: boolean
  user?: User
  mode: RunMode
  vaultLocked: boolean
  vaultHasMasterPassword: boolean
  version: string
  features: ServerFeatures
}

export interface SetupRequest {
  username: string
  password: string
  displayName?: string
  /** Required when AuthState.setupTokenRequired (403 `setup_token_required` otherwise). */
  setupToken?: string
}

export interface LoginRequest {
  username: string
  password: string
  /** 6-digit TOTP code, or a one-time recovery code (xxxxx-xxxxx) — the server distinguishes them by length. */
  totp?: string
  remember?: boolean
}

export interface LoginResponse {
  user: User
}

export interface ChangePasswordRequest {
  currentPassword: string
  newPassword: string
}

export interface TotpSetupResponse {
  secret: string
  otpauthUrl: string
}

export interface APIToken {
  id: string
  name: string
  createdAt: string
  lastUsedAt?: string
  expiresAt?: string
}

export interface APITokenCreated extends APIToken {
  /** Plaintext token — shown once. */
  token: string
}

export interface AuthSessionInfo {
  id: string
  createdAt: string
  expiresAt: string
  lastSeenAt: string
  ip: string
  userAgent: string
  remember?: boolean
  current: boolean
}

export interface VaultStatus {
  locked: boolean
  hasMasterPassword: boolean
}

export interface MasterPasswordRequest {
  currentPassword?: string
  /** Empty string removes the master password. */
  newPassword: string
}

// ---------------------------------------------------------------------------------------------------------------------
// Settings (GET/PUT /api/settings) — namespaced sections, see src/stores/settings.ts
// ---------------------------------------------------------------------------------------------------------------------

export type SettingsObject = Record<string, unknown>

// ---------------------------------------------------------------------------------------------------------------------
// Runtime sessions
// ---------------------------------------------------------------------------------------------------------------------

export interface CreateSessionRequest {
  connectionId?: string
  quick?: Partial<Connection> & { password?: string }
  cols: number
  rows: number
  title?: string
}

export interface ShareRequest {
  mode: 'read' | 'write'
  expiresInSec: number
}

export interface ShareResponse {
  token: string
  url: string
}

export interface ShareInfo {
  sessionId: string
  title: string
  mode: 'read' | 'write'
  protocol?: Protocol
  cols?: number
  rows?: number
  expiresAt?: string
  [key: string]: unknown
}

export interface LocalShell {
  id: string
  name: string
  path: string
  args: string[]
}

export interface SerialPortInfo {
  name: string
  description: string
  vid: string
  pid: string
  serial: string
}

export interface DockerContainer {
  id: string
  name: string
  image: string
  state: string
  status: string
  [key: string]: unknown
}

export interface KubeContext {
  name: string
  cluster?: string
  namespace?: string
  current?: boolean
  [key: string]: unknown
}

export interface KubePod {
  name: string
  namespace: string
  status?: string
  containers: string[]
  [key: string]: unknown
}

export interface RdpTicket {
  token: string
  destination: string
  username: string
  domain: string
  password: string
  width: number
  height: number
  [key: string]: unknown
}

export interface GuacdStatus {
  configured: boolean
  reachable: boolean
  version?: string
}

// ---------------------------------------------------------------------------------------------------------------------
// Files & transfers
// ---------------------------------------------------------------------------------------------------------------------

export type FsKind = 'sftp' | 'ftp' | 'local' | 's3'

export interface FsOpenRequest {
  sessionId?: string
  connectionId?: string
  local?: true
}

export interface FsCapabilities {
  chmod: boolean
  chown: boolean
  symlink: boolean
  exec: boolean
  checksum: boolean
}

export interface FsHandle {
  id: string
  kind: FsKind
  home: string
  root: string
  label: string
  capabilities: FsCapabilities
}

export interface FileEntry {
  name: string
  path: string
  type: 'file' | 'dir' | 'symlink' | 'other'
  size: number
  mode: number
  /** e.g. "drwxr-xr-x" */
  perm: string
  mtime: string
  owner?: string
  group?: string
  uid?: number
  gid?: number
  linkTarget?: string
  linkType?: 'file' | 'dir' | 'broken'
  hidden: boolean
}

export interface FsListResult {
  path: string
  parent: string
  entries: FileEntry[]
}

export interface FileReadResult {
  content: string
  encoding: 'utf-8' | 'base64'
  size: number
  mtime: string
  mode: number
}

export interface FileWriteRequest {
  path: string
  content: string
  encoding: 'utf-8' | 'base64'
  expectMtime?: string
}

export type OverwritePolicy = 'ask' | 'overwrite' | 'skip' | 'resume' | 'rename'

export interface TransferRequest {
  srcFs: string
  srcPaths: string[]
  dstFs: string
  dstDir: string
  overwrite: OverwritePolicy
}

export interface Transfer {
  id: string
  srcFs: string
  dstFs: string
  srcPaths: string[]
  dstDir: string
  label: string
  state: 'queued' | 'running' | 'done' | 'error' | 'canceled'
  error?: string
  totalBytes: number
  doneBytes: number
  totalFiles: number
  doneFiles: number
  currentFile?: string
  bytesPerSec: number
  createdAt: string
  finishedAt?: string
}

// ---------------------------------------------------------------------------------------------------------------------
// Monitoring
// ---------------------------------------------------------------------------------------------------------------------

export interface MonitorStats {
  ts: string
  os: string
  hostname: string
  kernel: string
  uptimeSec: number
  load: [number, number, number]
  cpu: { usage: number; cores: number; perCore: number[] }
  mem: { total: number; used: number; available: number; swapTotal: number; swapUsed: number }
  disks: { mount: string; fs: string; total: number; used: number }[]
  net: { iface: string; rxBps: number; txBps: number; rxTotal: number; txTotal: number }[]
  users: number
  processes: number
}

export interface Process {
  pid: number
  ppid: number
  user: string
  cpu: number
  mem: number
  rss: number
  state: string
  started: string
  command: string
}

// ---------------------------------------------------------------------------------------------------------------------
// Embedded servers, recordings, import/export, audit, tools
// ---------------------------------------------------------------------------------------------------------------------

export type ServerKind = 'http' | 'tftp' | 'sftp' | 'ftp'

export interface ServerStatus {
  kind: ServerKind | string
  running: boolean
  config: Record<string, unknown>
  error?: string
  addr?: string
  clients: number
  startedAt?: string
}

export interface Recording {
  id: string
  ownerId: string
  sessionId: string
  connectionId?: string
  title: string
  kind: 'asciicast' | 'log'
  size: number
  cols: number
  rows: number
  startedAt: string
  endedAt?: string
}

export type ImportFormat = 'ssh_config' | 'putty_reg' | 'mobaxterm' | 'json' | 'csv' | 'known_hosts' | 'termius_csv'

export interface ImportPreview {
  connections: Partial<Connection>[]
  folders: Partial<Folder>[]
  warnings: string[]
}

export interface AuditEntry {
  id: number
  ts: string
  userId?: string
  username?: string
  action: string
  target?: string
  details?: unknown
  ip?: string
}

export interface ShareLink {
  id: string
  sessionId: string
  ownerId: string
  mode: 'read' | 'write'
  createdAt: string
  expiresAt: string
}

/** Live SSH transport details (negotiated algorithms, host key, latency). */
export interface SSHConnInfo {
  serverVersion: string
  clientVersion: string
  kex: string
  hostKeyAlgo: string
  cipher: string
  mac: string
  hostKeyFingerprint: string
  latencyMs: number
}

export type ToolName =
  | 'ping'
  | 'traceroute'
  | 'portscan'
  | 'dns'
  | 'whois'
  | 'wol'
  | 'httpcheck'
  | 'tlscert'
  | 'netscan'
  | (string & {})

export interface JobStarted {
  jobId: string
}

// ---------------------------------------------------------------------------------------------------------------------
// Folder / connection mutations
// ---------------------------------------------------------------------------------------------------------------------

export interface ReorderItem {
  id: string
  folderId: string | null
  sortOrder: number
}

export type ConnectionInput = Omit<Connection, 'id' | 'secretKeys' | 'ownerId' | 'createdAt' | 'updatedAt' | 'lastUsedAt'>
export type ConnectionPatch = Partial<ConnectionInput>
export type FolderInput = Pick<Folder, 'name'> & Partial<Pick<Folder, 'parentId' | 'color' | 'icon' | 'sortOrder' | 'shared'>>
export type IdentityInput = Pick<Identity, 'name' | 'username'> & Partial<Pick<Identity, 'keyId' | 'secrets'>>

// ---------------------------------------------------------------------------------------------------------------------
// §6.1 Events WebSocket /ws/events
// ---------------------------------------------------------------------------------------------------------------------

export type PromptKind = 'hostkey' | 'password' | 'passphrase' | 'keyboard-interactive' | 'confirm'

export interface PromptField {
  label: string
  echo: boolean
  value?: string
}

export interface HostKeyInfo {
  host: string
  port: number
  keyType: string
  fingerprint: string
  fingerprintMd5: string
  status: 'unknown' | 'mismatch'
  knownFingerprint?: string
}

export interface Prompt {
  id: string
  kind: PromptKind
  title: string
  message?: string
  sessionId?: string
  connectionId?: string
  fields: PromptField[]
  hostKey?: HostKeyInfo
  allowSave: boolean
}

export type NotifyLevel = 'info' | 'success' | 'warning' | 'error'

export type ServerEvent =
  | { type: 'hello'; clientId: string; user: User }
  | { type: 'session.updated'; session: RuntimeSession }
  | { type: 'session.closed'; id: string }
  | { type: 'prompt'; prompt: Prompt }
  | { type: 'prompt.cancel'; id: string }
  | { type: 'transfer'; transfer: Transfer }
  | { type: 'tunnel'; id: string; status: TunnelStatus }
  | { type: 'monitor'; sessionId: string; stats?: MonitorStats; error?: string }
  | { type: 'job'; jobId: string; event: 'data' | 'done' | 'error'; data?: unknown; error?: string }
  | { type: 'notify'; level: NotifyLevel; title: string; message?: string }
  | { type: 'server'; status: ServerStatus }
  | { type: 'vault'; locked: boolean }
  | { type: 'pong' }
  /** Failed / unknown topic subscription (B0 extension, SPEC §9). */
  | { type: 'subscribe.error'; topic: string; params?: Record<string, unknown>; error: string }

export type ServerEventType = ServerEvent['type']
export type ServerEventOf<T extends ServerEventType> = Extract<ServerEvent, { type: T }>

export type ClientEvent =
  | { type: 'prompt.response'; id: string; accept: boolean; values?: string[]; save?: boolean }
  | { type: 'subscribe'; topic: string; sessionId?: string; [key: string]: unknown }
  | { type: 'unsubscribe'; topic: string; sessionId?: string; [key: string]: unknown }
  | { type: 'ping' }

// ---------------------------------------------------------------------------------------------------------------------
// §6.2 Terminal WebSocket /ws/terminal/{sessionId}?offset=<n>
// ---------------------------------------------------------------------------------------------------------------------

export type PromptMarkKind = 'A' | 'B' | 'C' | 'D'

export type TerminalServerMessage =
  | { type: 'attach'; mode: 'delta' | 'reset'; from: number; head: number }
  | { type: 'attach-end'; head: number }
  | { type: 'state'; state: SessionState; message?: string; exitCode?: number }
  | { type: 'title'; title: string }
  | { type: 'cwd'; path: string }
  | { type: 'bell' }
  | { type: 'readonly'; value: boolean }
  | { type: 'resize'; cols: number; rows: number }
  | { type: 'pong' }
  | { type: 'error'; message: string }
  | { type: 'prompt-mark'; kind: PromptMarkKind; offset: number; exitCode?: number }
  /** To the owner's views: administrators viewing the session read-only ([] = none any more). */
  | { type: 'shadow'; viewers: string[] }

export type TerminalClientMessage =
  | { type: 'resize'; cols: number; rows: number }
  | { type: 'ack'; offset: number }
  | { type: 'ping' }
  | { type: 'reconnect' }
  | { type: 'break' }
  | { type: 'signal'; name: string }

// ---------------------------------------------------------------------------------------------------------------------
// Error body
// ---------------------------------------------------------------------------------------------------------------------

export interface ErrorBody {
  error: string
  code?: string
  [key: string]: unknown
}
