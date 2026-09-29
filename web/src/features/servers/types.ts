/*
 * Types of the embedded servers API (internal/servers; superset of ServerStatus in src/api/types.ts, see SPEC §9
 * "servers").
 */
import type { ServerStatus } from '@/api/types'

export type ServerKindEx = 'http' | 'ftp' | 'sftp' | 'tftp' | 'telnet' | 'syslog'

export type ServerState = 'stopped' | 'starting' | 'running' | 'stopping' | 'error'

export interface ServerStats {
  connections: number
  bytesIn: number
  bytesOut: number
  transfers: number
  authFailures: number
  messages?: number
}

export interface ServerStatusEx extends ServerStatus {
  kind: ServerKindEx
  name: string
  state: ServerState
  errorCode?: string
  addrs?: string[]
  url?: string
  stopAt?: string
  stats: ServerStats
  warnings: string[]
  fingerprint?: string
  config: Record<string, unknown>
}

/** A server account as returned by the API (the password is write-only). */
export interface ServerUser {
  id?: string
  /** Client-only React key of a user not saved yet (never sent). */
  key?: string
  username: string
  hasPassword?: boolean
  /** Write-only: undefined = unchanged, "" = remove, else the new password. */
  password?: string
  publicKeys?: string[]
  readOnly?: boolean
}

export interface CommonConfig {
  bindAddress: string
  port: number
  autoStart: boolean
  stopAfterSec: number
}

export interface HTTPConfig extends CommonConfig {
  root: string
  listing: boolean
  readOnly: boolean
  upload: boolean
  maxUploadMB: number
  requireAuth: boolean
  users: ServerUser[]
  tls: boolean
}

export type FTPTLSMode = 'off' | 'optional' | 'required' | 'implicit'

export interface FTPConfig extends CommonConfig {
  root: string
  readOnly: boolean
  anonymous: boolean
  anonymousWrite: boolean
  users: ServerUser[]
  passivePortMin: number
  passivePortMax: number
  publicHost: string
  tls: FTPTLSMode
  idleTimeoutSec: number
}

export interface SFTPConfig extends CommonConfig {
  root: string
  readOnly: boolean
  users: ServerUser[]
  shell: boolean
  shellCommand: string
  idleTimeoutSec: number
}

export interface TFTPConfig extends CommonConfig {
  root: string
  readOnly: boolean
  blockSize: number
  timeoutSec: number
  retries: number
  singlePort: boolean
}

export interface TelnetConfig extends CommonConfig {
  users: ServerUser[]
  shellCommand: string
  workingDir: string
  idleTimeoutSec: number
}

export interface SyslogConfig extends CommonConfig {
  udp: boolean
  tcp: boolean
  bufferSize: number
  logToFile: boolean
  logDir: string
  retentionDays: number
  maxFileMB: number
}

export type AnyServerConfig = HTTPConfig | FTPConfig | SFTPConfig | TFTPConfig | TelnetConfig | SyslogConfig

export type LogLevel = 'debug' | 'info' | 'warn' | 'error'

export interface ServerLogEntry {
  id: number
  ts: string
  level: LogLevel
  client?: string
  user?: string
  message: string
}

export interface ServerLogsReply {
  entries: ServerLogEntry[]
  lastId: number
}

export interface ServerClient {
  id: string
  addr: string
  user?: string
  since: string
  activity?: string
  bytesIn: number
  bytesOut: number
}

export interface HostInterface {
  name: string
  addresses: string[]
  up: boolean
  loopback: boolean
}

export interface HostInfo {
  hostname: string
  platform: string
  osUser: string
  home: string
  defaultRoot: string
  /** Ports below 1024 need elevated rights (Unix, not root). */
  privilegedPorts: boolean
  /** …except on the wildcard address (macOS). */
  privilegedWildcardOk: boolean
  nexTermPort: number
  interfaces: HostInterface[]
}

export interface SyslogMessage {
  id: number
  received: string
  timestamp?: string
  source: string
  transport: 'udp' | 'tcp'
  facility: number
  severity: number
  hostname?: string
  appName?: string
  procId?: string
  msgId?: string
  structuredData?: string
  message: string
  format: 'rfc5424' | 'rfc3164' | 'raw'
}

export interface SyslogPage {
  messages: SyslogMessage[]
  hasMore: boolean
  total: number
  lastId: number
  capacity: number
}

export interface SyslogQuery {
  q?: string
  regex?: boolean
  /** Show severities 0…severity. */
  severity?: number
  facility?: number
  host?: string
  app?: string
  after?: number
  before?: number
  limit?: number
}

/** Events pushed on /ws/events (topics "servers", "servers.log", "syslog"). */
export interface ServerStatusEvent {
  type: 'server'
  status: ServerStatusEx
}

export interface ServerLogEvent {
  type: 'server.log'
  kind: ServerKindEx
  entries: ServerLogEntry[]
  dropped?: number
}

export interface SyslogEvent {
  type: 'syslog'
  messages: SyslogMessage[]
  dropped?: number
}

export interface HighlightRule {
  id: string
  pattern: string
  regex: boolean
  color: string
  enabled: boolean
}
