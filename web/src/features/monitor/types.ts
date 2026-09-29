/*
 * Monitor feature types. The backend payloads are JSON supersets of the SPEC contract types in @/api/types
 * (MonitorStats, Process, SSHConnInfo): the extra fields are declared here.
 */
import type { MonitorStats, Process, SSHConnInfo } from '@/api/types'

export type Platform = 'linux' | 'darwin' | 'freebsd' | 'openbsd' | 'netbsd' | 'dragonfly' | 'windows' | (string & {})

/** One monitor sample ({type:'monitor', sessionId, stats}). `disks[0]` is the primary volume. */
export interface Stats extends MonitorStats {
  cpu: MonitorStats['cpu'] & { user: number; system: number; iowait: number; steal: number }
  mem: MonitorStats['mem'] & { cached?: number }
  disks: { mount: string; fs: string; total: number; used: number; avail: number; device?: string }[]
  net: { iface: string; rxBps: number; txBps: number; rxTotal: number; txTotal: number; virtual?: boolean }[]
  platform: Platform
  arch?: string
  cpuModel?: string
  virt?: string
  threads?: number
  fds?: { used: number; max: number }
  diskIo?: { readBps: number; writeBps: number }
  /** Throughput of the physical interfaces (all non-loopback ones in containers). */
  netTotal: { rxBps: number; txBps: number }
  intervalSec: number
  /** First sample of a collector: rates are not available yet. */
  warmup?: boolean
}

/** Feed states sent with errors. */
export type FeedState = 'waiting' | 'unavailable' | 'error' | 'closed'

export interface MonitorEvent {
  type: 'monitor'
  sessionId: string
  stats?: Stats
  error?: string
  state?: FeedState
}

/** What the one-shot probe learned about a host (GET /api/monitor/{id}/host). */
export interface HostInfo {
  platform: Platform
  os: string
  kernel: string
  hostname: string
  arch: string
  cpuModel?: string
  cores?: number
  virt?: string
  model?: string
  tools?: Record<string, boolean>
}

/** Process row: SPEC Process plus details (rss / vsz in bytes, cpu = % of one core, cpuTime in seconds). */
export interface ProcessInfo extends Process {
  name?: string
  threads?: number
  nice: number
  vsz?: number
  cpuTime: number
}

export interface ServiceUnit {
  name: string
  description: string
  load: string
  active: string
  sub: string
  enabled?: string
  pid?: number
}

export interface ServiceList {
  manager: 'systemd' | 'windows' | ''
  services: ServiceUnit[]
  message?: string
}

export type ServiceAction = 'start' | 'stop' | 'restart' | 'reload' | 'enable' | 'disable'

export interface ListeningPort {
  proto: 'tcp' | 'udp'
  address: string
  port: number
  pid?: number
  process?: string
  user?: string
}

export interface DiskUsage {
  path: string
  parent?: string
  total: number
  entries: { name: string; path: string; size: number; dir: boolean }[]
  partial?: boolean
  warning?: string
}

export interface SSHInfo extends SSHConnInfo {
  probeMs: number
  remoteAddr?: string
  localAddr?: string
  user?: string
  host?: string
  port?: number
  pq: boolean
  jumpHosts?: string[]
  proxy?: string
  authMethod?: string
  compression: boolean
  agentForwarding: boolean
  x11Forwarding: boolean
  keepAliveSec: number
  connectedAt?: string
  measuredAt: string
  /** The server authenticated with an OpenSSH host certificate. */
  hostCert: boolean
  /** Reconnects of this runtime session, when it first connected, when / why it last dropped. */
  reconnects: number
  firstConnectedAt?: string
  lastDisconnectAt?: string
  lastDisconnect?: string
  /** Bytes of the terminal stream: output received from the server / input sent (not SFTP or port forwards). */
  termBytesIn: number
  termBytesOut: number
}

export interface SystemInfo {
  host: {
    hostname: string
    os: string
    platform: string
    platformFamily?: string
    platformVersion?: string
    kernelVersion?: string
    arch: string
    virtualization?: string
    hostId?: string
    bootTime?: string
    uptimeSec: number
    procs: number
    timezone: string
  }
  cpu: {
    model: string
    vendor?: string
    physicalCores: number
    logicalCores: number
    mhz?: number
    cacheKb?: number
    usage: number
    perCore: number[]
  }
  load: [number, number, number]
  mem: Stats['mem']
  disks: { mount: string; device: string; fs: string; total: number; used: number; avail: number; percent: number }[]
  net: { name: string; mtu: number; mac?: string; addrs: string[]; flags: string[]; up: boolean; rxBytes: number; txBytes: number; virtual?: boolean }[]
  users: { user: string; terminal?: string; host?: string; started?: string }[]
  topProcesses: ProcessInfo[]
  server: { version: string; mode: string; pid: number; goVersion: string; goroutines: number; heapBytes: number; dataDir?: string; listen?: string }
}

export interface CaffeineStatus {
  supported: boolean
  allowed: boolean
  enabled: boolean
  since?: string
  until?: string
  method?: string
  error?: string
}

/** A monitored target: a runtime session id or 'local' (the NexTerm host). */
export type TargetId = string

/** Panels of the monitor tab. */
export type MonitorPanel = 'overview' | 'processes' | 'services' | 'ports' | 'disk' | 'logs' | 'connection'

/**
 * Params of the 'monitor' tab kind. The monitored session is `target` — deliberately not `sessionId`: the shell and the
 * terminal feature treat a tab with `params.sessionId` as one showing the session (closing its terminal would then keep
 * the session alive). `sessionId` is still accepted when another module opens the tab directly.
 */
export interface MonitorTabParams {
  target?: string
  sessionId?: string
  panel?: MonitorPanel
  /** Title captured when the tab opened (host name / session title). */
  title?: string
  protocol?: string
  /** Disk usage start path (drill-down from the overview). */
  path?: string
}

/** Params of the 'sysinfo' tab kind (System information of the NexTerm host). */
export interface SysInfoTabParams {
  panel?: 'overview' | 'processes' | 'services' | 'ports' | 'disk' | 'logs'
}

/** Log follower frames (WS /ws/monitor/{id}/tail). */
export type TailFrame =
  | { type: 'start'; sources: string[] }
  | { type: 'lines'; lines: [number, string][] }
  | { type: 'error'; message: string }
  | { type: 'end'; code: number; message?: string }
