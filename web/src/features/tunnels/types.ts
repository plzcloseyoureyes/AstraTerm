/*
 * Tunnel types beyond the SPEC §5.2 `Tunnel` / `TunnelStatus` contract (extension fields documented in SPEC §9
 * "tunnels"). The backend returns these supersets from /api/tunnels and in {type:'tunnel'} events.
 */
import type { Tunnel, TunnelStatus, TunnelType } from '@/api/types'

/** Runtime flavour shown in the UI: `dynamic` + options.reverse is the reverse (remote) SOCKS proxy. */
export type TunnelKind = 'local' | 'remote' | 'dynamic' | 'rdynamic'

export interface TunnelOptions {
  /** Dynamic tunnels: serve the SOCKS proxy on the SSH server, exit through the NexTerm host (TUN-5). */
  reverse?: boolean
  /** Listen on a Unix socket (NexTerm host for local/dynamic, SSH server for remote) instead of bindHost:bindPort. */
  bindSocket?: string
  /** Connect to a Unix socket (SSH server for local, NexTerm host for remote) instead of destHost:destPort. */
  destSocket?: string
  /** SOCKS / HTTP proxy username (password: secret `socksPassword`). */
  socksUsername?: string
  /** Also accept HTTP CONNECT / absolute-URI requests and serve /proxy.pac (default true). */
  httpProxy?: boolean
  /** Reconnect with backoff when the SSH connection drops (default true). */
  autoReconnect?: boolean
  /** Local/dynamic: connect SSH only when a client arrives; disconnect after idleTimeoutSec. */
  onDemand?: boolean
  idleTimeoutSec?: number
  /** Concurrent connection cap (0 = default). */
  maxConns?: number
  /** Client allow list for listeners on the NexTerm host (IPs / CIDRs). */
  allowFrom?: string[]
  /** "Open in browser" scheme ('' = guess from the port). */
  scheme?: '' | 'http' | 'https'
  color?: string
  icon?: string
  notes?: string
}

export interface TunnelStatusEx extends TunnelStatus {
  /** The SSH link under the tunnel is up. */
  connected: boolean
  /** Actually bound listener addresses (auto ports resolved). */
  localAddr?: string
  remoteAddr?: string
  /** Current throughput in bytes/s (in = towards the client). */
  rateIn: number
  rateOut: number
  failedConns: number
  /** Last per-connection failure while the tunnel keeps running. */
  lastError?: string
  lastErrorAt?: string
  reconnects: number
  /** Next reconnect attempt while waiting. */
  retryAt?: string
  /** What a starting tunnel waits for: vault unlock, a window to answer a prompt, or (on-demand) a client. */
  waiting?: 'vault' | 'client' | 'demand'
  /** A running tunnel that works differently than configured (a remote forward bound to loopback only). */
  warning?: string
}

export interface ConnRef {
  id: string
  name: string
  protocol: string
  host: string
  port: number
  username?: string
}

export interface TunnelEx extends Omit<Tunnel, 'status'> {
  status: TunnelStatusEx
  options: TunnelOptions
  sortOrder: number
  /** Names of stored (write-only) secrets, e.g. ["socksPassword"]. */
  secretKeys: string[]
  connection?: ConnRef
}

export interface TunnelInput {
  name: string
  type: TunnelType
  connectionId: string
  bindHost: string
  bindPort: number
  destHost: string
  destPort: number
  autoStart: boolean
  options: TunnelOptions
  sortOrder?: number
  /** Write-only; "" deletes a stored secret. */
  secrets?: Record<string, string>
}

export type TunnelChange = 'created' | 'updated' | 'deleted'

export interface TunnelEvent {
  type: 'tunnel'
  id: string
  status: TunnelStatusEx
  change?: TunnelChange
}

/** Session-integrated forward definition (connection.options.forwards[] entries, TUN-7). */
export interface ForwardSpec {
  type: TunnelType
  bindHost?: string
  bindPort?: number
  destHost?: string
  destPort?: number
  reverse?: boolean
  bindSocket?: string
  destSocket?: string
  name?: string
  disabled?: boolean
}

export interface SessionForward {
  /** "<sessionId>:<forwardId>" */
  id: string
  sessionId: string
  sessionTitle: string
  connectionId?: string
  source: 'connection' | 'adhoc'
  spec: ForwardSpec
  status: TunnelStatusEx
}

export interface SessionForwardsEvent {
  type: 'tunnel.session'
  sessionId: string
  forwards: SessionForward[]
}

export interface RemotePort {
  address: string
  port: number
  scope: 'loopback' | 'all' | 'address'
  /** Destination host a local forward should use to reach the port from the SSH server. */
  connectHost: string
  process?: string
  pid?: number
}

export interface RemotePorts {
  method: string
  ports: RemotePort[]
  note?: string
}

export interface PortsEvent {
  type: 'tunnel.ports'
  sessionId: string
  initial?: boolean
  ports: RemotePort[]
  added?: RemotePort[]
  removed?: RemotePort[]
  error?: string
}

export interface CheckBindResult {
  available: boolean
  error?: string
  suggestion?: number
}

export interface BulkStartResult {
  started: number
  failed: { id: string; name: string; error: string }[]
}

export interface ExportFile {
  format: 'nexterm-tunnels'
  version: number
  exportedAt: string
  tunnels: {
    name: string
    type: TunnelType
    bindHost: string
    bindPort: number
    destHost: string
    destPort: number
    autoStart: boolean
    options: TunnelOptions
    sortOrder: number
    connection: Partial<ConnRef>
  }[]
}

export interface ImportResult {
  created: TunnelEx[]
  /** `warning`: the tunnel would listen on a non-loopback address (reachable from other machines). */
  planned: { name: string; connectionId: string; connectionName: string; matched: string; warning?: string }[]
  skipped: { name: string; reason: string }[]
}

/** Arguments of the `tunnels.new` command (other modules prefill the editor). */
export interface NewTunnelArgs {
  connectionId?: string
  type?: TunnelType | 'rdynamic'
  name?: string
  bindHost?: string
  bindPort?: number
  destHost?: string
  destPort?: number
  /** Start right after saving. */
  start?: boolean
}
