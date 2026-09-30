/*
 * Pure helpers for the tunnel manager: kinds, endpoint labels, status presentation, URLs, and the editor draft
 * (client-side mirror of the backend validation in internal/tunnel/spec.go).
 */
import type { Connection, TunnelType } from '@/api/types'
import { statusDotClass } from '@/components/ui/status-dot'
import type { ForwardSpec, TunnelEx, TunnelInput, TunnelKind, TunnelOptions, TunnelStatusEx } from './types'

// --- kinds -----------------------------------------------------------------------------------------------------------

export interface KindInfo {
  label: string
  /** Compact label for badges in dense tables. */
  short: string
  /** OpenSSH-style flag shown in badges. */
  flag: string
  description: string
  /** Where the listener lives. */
  listenOn: 'host' | 'server'
}

export const KINDS: Record<TunnelKind, KindInfo> = {
  local: {
    label: 'Local',
    short: 'Local',
    flag: '-L',
    description: 'Listen on this machine and forward connections through the SSH server to a destination it can reach.',
    listenOn: 'host',
  },
  remote: {
    label: 'Remote',
    short: 'Remote',
    flag: '-R',
    description: 'Listen on the SSH server and forward connections back to a destination reachable from this machine.',
    listenOn: 'server',
  },
  dynamic: {
    label: 'Dynamic',
    short: 'Dynamic',
    flag: '-D',
    description: 'A SOCKS / HTTP proxy on this machine; every connection exits through the SSH server.',
    listenOn: 'host',
  },
  rdynamic: {
    label: 'Reverse SOCKS',
    short: 'Rev. SOCKS',
    flag: '-R',
    description: 'A SOCKS / HTTP proxy on the SSH server; connections exit through this machine’s network.',
    listenOn: 'server',
  },
}

export const KIND_ORDER: TunnelKind[] = ['local', 'remote', 'dynamic', 'rdynamic']

export function kindOf(t: { type: TunnelType; options?: TunnelOptions; reverse?: boolean }): TunnelKind {
  if (t.type === 'dynamic') return t.options?.reverse || t.reverse ? 'rdynamic' : 'dynamic'
  return t.type === 'remote' ? 'remote' : 'local'
}

export function typeOfKind(k: TunnelKind): { type: TunnelType; reverse: boolean } {
  switch (k) {
    case 'remote':
      return { type: 'remote', reverse: false }
    case 'dynamic':
      return { type: 'dynamic', reverse: false }
    case 'rdynamic':
      return { type: 'dynamic', reverse: true }
    default:
      return { type: 'local', reverse: false }
  }
}

export const isProxyKind = (k: TunnelKind) => k === 'dynamic' || k === 'rdynamic'

// --- addresses -------------------------------------------------------------------------------------------------------

/** host:port with IPv6 brackets. */
export function hostPort(host: string | undefined, port: number | string | undefined | null): string {
  const h = host ?? ''
  const p = port === 0 || port === '0' ? 'auto' : String(port ?? '')
  return h.includes(':') ? `[${h}]:${p}` : `${h}:${p}`
}

export function isLoopbackHost(h: string | undefined): boolean {
  if (!h) return true // default 127.0.0.1
  const v = h.trim().toLowerCase().replace(/^\[|\]$/g, '')
  return v === 'localhost' || v === '::1' || /^127\.\d{1,3}\.\d{1,3}\.\d{1,3}$/.test(v)
}

/** The listen side of a tunnel (bound address when running). */
function listenLabel(t: TunnelEx): string {
  const k = kindOf(t)
  const bound = KINDS[k].listenOn === 'host' ? t.status.localAddr : t.status.remoteAddr
  if (bound && t.status.state !== 'stopped') return bound
  if (t.options.bindSocket) return t.options.bindSocket
  return hostPort(t.bindHost || '127.0.0.1', t.bindPort)
}

/** The destination side of a tunnel. */
function destLabel(t: { type: TunnelType; options?: TunnelOptions; destHost?: string; destPort?: number }): string {
  const k = kindOf(t)
  if (isProxyKind(k)) return k === 'dynamic' ? 'any host (via SSH server)' : 'any host (via this machine)'
  if (t.options?.destSocket) return t.options.destSocket
  return hostPort(t.destHost || 'localhost', t.destPort ?? 0)
}

/** Table columns "Local" / "Remote": what lives on this machine vs. on the SSH server side. */
export function endpoints(t: TunnelEx): { local: string; remote: string } {
  const k = kindOf(t)
  switch (k) {
    case 'local':
      return { local: listenLabel(t), remote: destLabel(t) }
    case 'remote':
      return { local: destLabel(t), remote: listenLabel(t) }
    case 'dynamic':
      return { local: `SOCKS ${listenLabel(t)}`, remote: 'any destination' }
    default:
      return { local: 'any destination', remote: `SOCKS ${listenLabel(t)}` }
  }
}

export function connectionLabel(c: { name?: string; username?: string; host?: string; port?: number } | undefined): string {
  if (!c) return 'Unknown SSH server'
  const target = `${c.username ? `${c.username}@` : ''}${c.host ?? ''}${c.port && c.port !== 22 ? `:${c.port}` : ''}`
  return c.name && c.name !== c.host ? `${c.name} (${target})` : target || c.name || ''
}

export function isSSHConnection(c: Connection): boolean {
  return c.protocol === 'ssh' || c.protocol === 'sftp' || c.protocol === 'mosh'
}

const CONN_ID_RE = /^[a-z2-7]{20}$/

/**
 * How AstraTerm reaches an SSH connection besides the direct route: its jump hosts (saved connections by name, ad-hoc
 * "[user@]host[:port]" hops as written; deleted ones are skipped) and its proxy, first hop first.
 */
export function connectionRoute(c: Connection | undefined, all: readonly Connection[] | undefined): string[] {
  if (!c) return []
  const byId = new Map((all ?? []).map((x) => [x.id, x]))
  const hops: string[] = []
  const o = c.options ?? {}
  const proxy = o.proxy
  if (proxy && proxy.type && proxy.type !== 'none' && proxy.host) hops.push(`${proxy.type.toUpperCase()} proxy ${hostPort(proxy.host, proxy.port)}`)
  else if (typeof o.proxyCommand === 'string' && o.proxyCommand.trim()) hops.push('ProxyCommand')
  for (const hop of Array.isArray(o.jumpHosts) ? o.jumpHosts : []) {
    if (typeof hop !== 'string' || !hop.trim()) continue
    const saved = byId.get(hop)
    if (saved) hops.push(saved.name || saved.host)
    else if (!CONN_ID_RE.test(hop)) hops.push(hop.trim())
  }
  return hops
}

// --- well-known destinations (TUN-6 presets, DOCKER_HOST) ------------------------------------------------------------

/** Docker API sockets (Docker Engine, Podman's Docker-compatible service). */
function isDockerSocket(path: string | undefined): boolean {
  return !!path && /(^|\/)(docker|podman)\.sock$/.test(path.trim())
}

/** DOCKER_HOST value for a listener: a socket path, or a bound / configured "host:port". */
function dockerHostFor(bindSocket: string | undefined, bound: string | undefined): string | null {
  if (bindSocket) return bindSocket.startsWith('/') ? `unix://${bindSocket}` : null
  const hp = bound ? urlHost(bound) : null
  if (!hp || hp.port === '0') return null
  return `tcp://${hp.host === 'localhost' ? '127.0.0.1' : hp.host}:${hp.port}`
}

export interface DockerHostHint {
  /** "tcp://127.0.0.1:2375" / "unix:///tmp/docker.sock"; null until an automatic port is assigned. */
  value: string | null
  /** Where the variable is to be set: this machine (local forward) or the SSH server (remote forward). */
  where: 'here' | 'server'
}

/** DOCKER_HOST for a saved tunnel to a Docker socket (null for other tunnels). */
export function dockerHost(t: TunnelEx): DockerHostHint | null {
  const k = kindOf(t)
  if ((k !== 'local' && k !== 'remote') || !isDockerSocket(t.options.destSocket)) return null
  const live = t.status.state !== 'stopped' ? (k === 'local' ? t.status.localAddr : t.status.remoteAddr) : undefined
  const bound = live || (t.bindPort ? hostPort(t.bindHost || '127.0.0.1', t.bindPort) : undefined)
  return { value: dockerHostFor(t.options.bindSocket, bound), where: k === 'local' ? 'here' : 'server' }
}

/** DOCKER_HOST for the tunnel being edited (null when the destination is not a Docker socket). */
export function draftDockerHost(d: TunnelDraft): DockerHostHint | null {
  if ((d.kind !== 'local' && d.kind !== 'remote') || !d.useDestSocket || !isDockerSocket(d.destSocket)) return null
  const bound = d.bindPort ? hostPort(d.bindHost || '127.0.0.1', d.bindPort) : undefined
  return { value: dockerHostFor(d.useBindSocket ? d.bindSocket.trim() : undefined, bound), where: d.kind === 'local' ? 'here' : 'server' }
}

export interface DestPreset {
  id: string
  label: string
  /** Socket path, or host + port, on the destination side. */
  socket?: string
  host?: string
  port?: number
  /** Listen port proposed when none is set. */
  listenPort: number
  /** Icon proposed when none is chosen (see TUNNEL_ICONS). */
  icon: string
}

export const DEST_PRESETS: { group: string; items: DestPreset[] }[] = [
  {
    group: 'Unix sockets',
    items: [
      { id: 'docker', label: 'Docker Engine', socket: '/var/run/docker.sock', listenPort: 2375, icon: 'docker' },
      { id: 'podman', label: 'Podman (Docker API)', socket: '/run/podman/podman.sock', listenPort: 2375, icon: 'docker' },
      { id: 'pg-socket', label: 'PostgreSQL', socket: '/var/run/postgresql/.s.PGSQL.5432', listenPort: 5432, icon: 'database' },
      { id: 'mysql-socket', label: 'MySQL / MariaDB', socket: '/var/run/mysqld/mysqld.sock', listenPort: 3306, icon: 'database' },
    ],
  },
  {
    group: 'TCP services',
    items: [
      { id: 'web', label: 'Web server (HTTP)', host: 'localhost', port: 80, listenPort: 8080, icon: 'web' },
      { id: 'https', label: 'Web server (HTTPS)', host: 'localhost', port: 443, listenPort: 8443, icon: 'web' },
      { id: 'postgres', label: 'PostgreSQL', host: 'localhost', port: 5432, listenPort: 5432, icon: 'database' },
      { id: 'mysql', label: 'MySQL / MariaDB', host: 'localhost', port: 3306, listenPort: 3306, icon: 'database' },
      { id: 'redis', label: 'Redis', host: 'localhost', port: 6379, listenPort: 6379, icon: 'database' },
      { id: 'mongodb', label: 'MongoDB', host: 'localhost', port: 27017, listenPort: 27017, icon: 'database' },
      { id: 'rdp', label: 'Remote Desktop (RDP)', host: 'localhost', port: 3389, listenPort: 3389, icon: 'desktop' },
      { id: 'vnc', label: 'VNC', host: 'localhost', port: 5900, listenPort: 5900, icon: 'desktop' },
    ],
  },
]

/** Apply a destination preset to a (local or remote) forward draft. */
export function applyDestPreset(d: TunnelDraft, p: DestPreset): TunnelDraft {
  const next: TunnelDraft = { ...d }
  if (next.kind !== 'local' && next.kind !== 'remote') next.kind = 'local'
  if (p.socket) {
    next.useDestSocket = true
    next.destSocket = p.socket
  } else {
    next.useDestSocket = false
    next.destHost = p.host ?? 'localhost'
    next.destPort = p.port ?? null
  }
  if (!next.useBindSocket && next.bindPort == null) next.bindPort = p.listenPort
  if (!next.icon) next.icon = p.icon
  return next
}

// --- web / URLs ------------------------------------------------------------------------------------------------------

const WEB_PORTS = new Set([80, 81, 443, 591, 1880, 2375, 3000, 3001, 3030, 4000, 4200, 5000, 5001, 5173, 5601, 7474, 8000, 8001,
  8008, 8069, 8080, 8081, 8082, 8086, 8088, 8123, 8181, 8443, 8787, 8888, 8889, 9000, 9001, 9090, 9091, 9200, 9443, 15672])

/** Scheme of an HTTP(S) destination, or null when the port does not look like a web service. */
export function webScheme(port: number | undefined, hint?: string): 'http' | 'https' | null {
  if (hint === 'http' || hint === 'https') return hint
  if (!port) return null
  if (port === 443 || port === 8443 || port === 9443) return 'https'
  return WEB_PORTS.has(port) ? 'http' : null
}

/** Host part usable in a URL for a bound address ("*:8080" → localhost). */
function urlHost(bound: string): { host: string; port: string } | null {
  const m = /^(.*):(\d+)$/.exec(bound)
  if (!m) return null
  let host = m[1]
  if (host === '*' || host === '' || host === '0.0.0.0' || host === '[::]' || host === '::') host = 'localhost'
  return { host, port: m[2] }
}

/**
 * Whether this browser runs on the AstraTerm host: the UI is served from a loopback address. Listeners of local tunnels
 * live on the AstraTerm host, so only then does "localhost:<port>" in the browser mean the tunnel.
 */
export function browserOnAstraTermHost(): boolean {
  const h = window.location.hostname.replace(/^\[|\]$/g, '').toLowerCase()
  return h === 'localhost' || h.endsWith('.localhost') || h === '::1' || h.startsWith('127.')
}

/**
 * Host this browser uses to reach a listener of the AstraTerm host bound to `boundHost` ('*' / '' / 0.0.0.0 / :: =
 * every interface), or null when it cannot: a loopback-only listener on another machine.
 */
export function browserReachableHost(boundHost: string): string | null {
  const h = boundHost.replace(/^\[|\]$/g, '')
  const local = browserOnAstraTermHost()
  if (h === '*' || h === '' || h === '0.0.0.0' || h === '::') return local ? 'localhost' : window.location.hostname.replace(/^\[|\]$/g, '')
  if (isLoopbackHost(h)) return local ? h : null
  return h
}

/** Scheme and bound address of a local tunnel to a web service (null when not a web tunnel or the port is unknown). */
export function webEndpoint(t: TunnelEx): { scheme: 'http' | 'https'; host: string; port: string } | null {
  if (kindOf(t) !== 'local' || t.options.bindSocket || t.options.destSocket) return null
  const scheme = webScheme(t.destPort, t.options.scheme)
  if (!scheme) return null
  const bound = t.status.localAddr || hostPort(t.bindHost || '127.0.0.1', t.bindPort)
  const m = /^(.*):(\d+)$/.exec(bound)
  if (!m || m[2] === '0') return null
  return { scheme, host: m[1].replace(/^\[|\]$/g, ''), port: m[2] }
}

/** Browser URL of a local tunnel to a web service (null when not applicable or not reachable from this browser). */
export function browserUrl(t: TunnelEx): string | null {
  const ep = webEndpoint(t)
  if (!ep) return null
  const host = browserReachableHost(ep.host)
  if (!host) return null
  return `${ep.scheme}://${hostPort(host, ep.port)}/`
}

/** Something useful to put on the clipboard for a tunnel's local end. */
export function copyTarget(t: TunnelEx): string | null {
  const k = kindOf(t)
  if (k === 'local') {
    if (t.options.bindSocket) return t.options.bindSocket
    return browserUrl(t) ?? (t.status.localAddr || (t.bindPort ? hostPort(t.bindHost, t.bindPort) : null))
  }
  if (k === 'dynamic') {
    if (t.options.bindSocket) return t.options.bindSocket
    const bound = t.status.localAddr || (t.bindPort ? hostPort(t.bindHost, t.bindPort) : '')
    const hp = bound ? urlHost(bound) : null
    return hp ? `socks5h://${hp.host === 'localhost' ? '127.0.0.1' : hp.host}:${hp.port}` : null
  }
  return t.status.remoteAddr || (t.options.bindSocket ?? null)
}

// --- status ----------------------------------------------------------------------------------------------------------

export type StatusTone = 'running' | 'idle' | 'starting' | 'error' | 'stopped'

export function statusTone(s: TunnelStatusEx | undefined): StatusTone {
  if (!s) return 'stopped'
  switch (s.state) {
    case 'running':
      if (s.waiting === 'demand' && !s.connected) return 'idle'
      return s.connected ? 'running' : 'starting'
    case 'starting':
      return 'starting'
    case 'error':
      return 'error'
    default:
      return 'stopped'
  }
}

export const TONE_DOT: Record<StatusTone, string> = {
  running: 'bg-success',
  idle: 'border border-success bg-transparent',
  starting: statusDotClass('warning', true),
  error: 'bg-destructive',
  stopped: 'bg-muted-foreground/40',
}

export const TONE_LABEL: Record<StatusTone, string> = {
  running: 'Running',
  idle: 'Idle — connects on demand',
  starting: 'Starting',
  error: 'Error',
  stopped: 'Stopped',
}

/** One-line status description (error, waiting reason, reconnect countdown…). */
export function statusText(s: TunnelStatusEx | undefined, now = Date.now()): string {
  const tone = statusTone(s)
  if (!s) return TONE_LABEL.stopped
  if (tone === 'error') return s.error || 'Error'
  if (s.state === 'starting') {
    if (s.waiting === 'vault') return 'Waiting for the vault to be unlocked'
    if (s.waiting === 'client') return 'Waiting for a login prompt to be answered'
    if (s.retryAt) {
      const secs = Math.max(0, Math.round((Date.parse(s.retryAt) - now) / 1000))
      const reason = (s.error ?? '').replace(/\s+—\s+retrying in .*$/, '')
      return `${reason ? `${reason} — ` : ''}retrying in ${secs}s`
    }
    return s.error || 'Connecting…'
  }
  if (tone === 'starting') return 'Connecting…'
  return TONE_LABEL[tone]
}

export function isActive(s: TunnelStatusEx | undefined): boolean {
  return !!s && (s.state === 'running' || s.state === 'starting')
}

// --- editor draft ----------------------------------------------------------------------------------------------------

export interface TunnelDraft {
  name: string
  kind: TunnelKind
  connectionId: string
  bindHost: string
  bindPort: number | null
  useBindSocket: boolean
  bindSocket: string
  destHost: string
  destPort: number | null
  useDestSocket: boolean
  destSocket: string
  autoStart: boolean
  autoReconnect: boolean
  onDemand: boolean
  idleTimeoutSec: number | null
  maxConns: number | null
  allowFrom: string[]
  socksAuth: boolean
  socksUsername: string
  /** New password ('' keeps the stored one when editing). */
  socksPassword: string
  httpProxy: boolean
  scheme: '' | 'http' | 'https'
  color: string
  /** Icon name (TUNNEL_ICONS), '' = none. */
  icon: string
  notes: string
}

export function emptyDraft(): TunnelDraft {
  return {
    name: '',
    kind: 'local',
    connectionId: '',
    bindHost: '127.0.0.1',
    bindPort: null,
    useBindSocket: false,
    bindSocket: '',
    destHost: 'localhost',
    destPort: null,
    useDestSocket: false,
    destSocket: '',
    autoStart: false,
    autoReconnect: true,
    onDemand: false,
    idleTimeoutSec: 300,
    maxConns: null,
    allowFrom: [],
    socksAuth: false,
    socksUsername: '',
    socksPassword: '',
    httpProxy: true,
    scheme: '',
    color: '',
    icon: '',
    notes: '',
  }
}

export function draftFromTunnel(t: TunnelEx): TunnelDraft {
  const o = t.options ?? {}
  return {
    name: t.name,
    kind: kindOf(t),
    connectionId: t.connectionId,
    bindHost: t.bindHost || '127.0.0.1',
    bindPort: t.options.bindSocket ? null : t.bindPort,
    useBindSocket: !!o.bindSocket,
    bindSocket: o.bindSocket ?? '',
    destHost: t.destHost || 'localhost',
    destPort: t.destPort || null,
    useDestSocket: !!o.destSocket,
    destSocket: o.destSocket ?? '',
    autoStart: t.autoStart,
    autoReconnect: o.autoReconnect !== false,
    onDemand: !!o.onDemand,
    idleTimeoutSec: o.idleTimeoutSec || 300,
    maxConns: o.maxConns || null,
    allowFrom: o.allowFrom ?? [],
    socksAuth: !!o.socksUsername,
    socksUsername: o.socksUsername ?? '',
    socksPassword: '',
    httpProxy: o.httpProxy !== false,
    scheme: o.scheme ?? '',
    color: o.color ?? '',
    icon: o.icon ?? '',
    notes: o.notes ?? '',
  }
}

/** Build the API input of a draft; `stored` tells whether a SOCKS password is already stored (edit mode). */
export function inputFromDraft(d: TunnelDraft, stored: boolean): TunnelInput {
  const { type, reverse } = typeOfKind(d.kind)
  const proxy = isProxyKind(d.kind)
  const hostListener = KINDS[d.kind].listenOn === 'host'
  const options: TunnelOptions = {
    reverse: reverse || undefined,
    bindSocket: d.useBindSocket ? d.bindSocket.trim() : undefined,
    destSocket: !proxy && d.useDestSocket ? d.destSocket.trim() : undefined,
    socksUsername: proxy && d.socksAuth ? d.socksUsername.trim() : undefined,
    httpProxy: proxy ? d.httpProxy : undefined,
    autoReconnect: d.autoReconnect,
    onDemand: hostListener && d.onDemand ? true : undefined,
    idleTimeoutSec: hostListener && d.onDemand ? (d.idleTimeoutSec ?? 300) : undefined,
    maxConns: d.maxConns || undefined,
    allowFrom: hostListener && d.allowFrom.length ? d.allowFrom : undefined,
    scheme: d.scheme || undefined,
    color: d.color || undefined,
    icon: d.icon || undefined,
    notes: d.notes.trim() || undefined,
  }
  const secrets: Record<string, string> = {}
  if (proxy && d.socksAuth && d.socksPassword) secrets.socksPassword = d.socksPassword
  if ((!proxy || !d.socksAuth) && stored) secrets.socksPassword = ''
  return {
    name: d.name.trim(),
    type,
    connectionId: d.connectionId,
    bindHost: d.useBindSocket ? '' : d.bindHost.trim(),
    bindPort: d.useBindSocket ? 0 : (d.bindPort ?? 0),
    destHost: proxy || d.useDestSocket ? '' : d.destHost.trim(),
    destPort: proxy || d.useDestSocket ? 0 : (d.destPort ?? 0),
    autoStart: d.autoStart,
    options,
    secrets: Object.keys(secrets).length ? secrets : undefined,
  }
}

const HOST_RE = /^[A-Za-z0-9._-]+$/
const IPV6_RE = /^[0-9A-Fa-f:.]+(%[\w.-]+)?$/

export function validHost(h: string): boolean {
  const v = h.trim().replace(/^\[|\]$/g, '')
  if (!v || v.length > 255) return false
  if (v.includes(':')) return IPV6_RE.test(v)
  return HOST_RE.test(v) && !v.startsWith('-') && !v.startsWith('.') && !v.includes('..')
}

/** A listen address the server accepts: '', '*', localhost or an IP — and host names for listeners on the SSH server. */
export function validBindHost(h: string, remote: boolean): boolean {
  const v = h.trim().replace(/^\[|\]$/g, '')
  if (v === '' || v === '*' || v.toLowerCase() === 'localhost') return true
  if (/^\d{1,3}(\.\d{1,3}){3}$/.test(v) || v.includes(':')) return validHost(v)
  return remote && validHost(v)
}

export type DraftErrors = Partial<Record<'name' | 'connectionId' | 'bindHost' | 'bindPort' | 'bindSocket' | 'destHost' | 'destPort' | 'destSocket' | 'socksUsername' | 'socksPassword' | 'allowFrom', string>>

/** Client-side validation (the server re-validates everything). */
export function validateDraft(d: TunnelDraft, storedPassword: boolean): DraftErrors {
  const e: DraftErrors = {}
  const proxy = isProxyKind(d.kind)
  const listenHere = KINDS[d.kind].listenOn === 'host'
  if (!d.name.trim()) e.name = 'Give the tunnel a name'
  else if (d.name.trim().length > 200) e.name = 'At most 200 characters'
  if (!d.connectionId) e.connectionId = 'Choose the SSH server to tunnel through'
  if (d.useBindSocket) {
    const p = d.bindSocket.trim()
    if (!p) e.bindSocket = 'Enter the socket path'
    else if (!listenHere && !p.startsWith('/')) e.bindSocket = 'Absolute path on the SSH server (starting with /)'
    else if (listenHere && !(p.startsWith('/') || /^[A-Za-z]:[\\/]/.test(p))) e.bindSocket = 'Absolute path on this machine'
    else if (p.length > 104) e.bindSocket = 'At most 104 characters'
  } else {
    if (!validBindHost(d.bindHost, !listenHere)) e.bindHost = listenHere ? 'An IP address of this machine, localhost or *' : 'Invalid address'
    if (d.bindPort != null && (d.bindPort < 0 || d.bindPort > 65535)) e.bindPort = '0–65535 (0 = automatic)'
  }
  if (!proxy) {
    if (d.useDestSocket) {
      const p = d.destSocket.trim()
      if (!p) e.destSocket = 'Enter the socket path'
      else if (d.kind === 'local' && !p.startsWith('/')) e.destSocket = 'Absolute path on the SSH server (starting with /)'
      else if (d.kind === 'remote' && !(p.startsWith('/') || /^[A-Za-z]:[\\/]/.test(p))) e.destSocket = 'Absolute path on this machine'
    } else {
      if (!validHost(d.destHost || 'localhost')) e.destHost = 'Invalid host name or IP address'
      if (!d.destPort || d.destPort < 1 || d.destPort > 65535) e.destPort = 'Destination port (1–65535)'
    }
  } else {
    if (d.socksAuth) {
      if (!d.socksUsername.trim()) e.socksUsername = 'Enter a username'
      if (!d.socksPassword && !storedPassword) e.socksPassword = 'Enter a password'
    } else if (!d.useBindSocket && !isLoopbackHost(d.bindHost)) {
      // Mirrors the server: an unauthenticated proxy on the network must be limited to known clients — which only a
      // listener on this machine can check (a reverse proxy's clients reach it through the SSH server).
      if (!listenHere) e.socksUsername = 'A proxy on the server’s network needs a username and password'
      else if (!restrictiveAllowList(d.allowFrom))
        e.socksUsername = 'A proxy reachable from other machines needs a username/password or an allowed-clients list (one that does not admit everyone)'
    }
  }
  for (const a of d.allowFrom) {
    if (!/^[0-9A-Fa-f.:]+(\/\d{1,3})?$/.test(a.trim())) {
      e.allowFrom = `“${a}” is not an IP address or CIDR`
      break
    }
  }
  return e
}

/** An allowed-clients list that limits who may connect: not empty, and no entry admits every address (/0). */
function restrictiveAllowList(list: readonly string[]): boolean {
  const entries = list.map((a) => a.trim()).filter(Boolean)
  return entries.length > 0 && !entries.some((a) => /\/0+$/.test(a))
}

/**
 * Whether a draft's listener accepts connections from other machines: a TCP listener on a non-loopback address of
 * this machine, or of the SSH server (subject to its GatewayPorts setting).
 */
export function draftExposed(d: Pick<TunnelDraft, 'useBindSocket' | 'bindHost'>): boolean {
  return !d.useBindSocket && !isLoopbackHost(d.bindHost)
}

/** Suggested tunnel name (used when the name is left empty). */
export function suggestName(d: TunnelDraft, conn: Connection | undefined): string {
  const server = conn?.name || conn?.host || ''
  const via = server ? ` via ${server}` : ''
  const sock = (p: string) => p.split('/').filter(Boolean).pop() || p
  const dest = d.useDestSocket
    ? d.destSocket.trim()
      ? sock(d.destSocket.trim())
      : ''
    : d.destPort
      ? `${d.destHost && d.destHost !== 'localhost' ? d.destHost : server || 'localhost'}:${d.destPort}`
      : ''
  switch (d.kind) {
    case 'dynamic':
      return `SOCKS proxy${via}`
    case 'rdynamic':
      return server ? `Reverse SOCKS on ${server}` : 'Reverse SOCKS proxy'
    case 'remote':
      return dest ? `${server ? `${server} → ` : ''}${d.useDestSocket ? dest : `${d.destHost || 'localhost'}:${d.destPort}`}` : `Remote forward${via}`
    default:
      return dest ? `${dest}${d.useDestSocket && server ? ` on ${server}` : ''}` : `Local forward${via}`
  }
}

/** Label of a session forward definition. */
export function forwardLabel(f: ForwardSpec): string {
  const k = kindOf({ type: f.type, reverse: f.reverse })
  const listen = f.bindSocket || hostPort(f.bindHost || '127.0.0.1', f.bindPort ?? 0)
  const dest = isProxyKind(k) ? 'SOCKS' : f.destSocket || hostPort(f.destHost || 'localhost', f.destPort ?? 0)
  return `${KINDS[k].flag} ${listen} → ${dest}`
}
