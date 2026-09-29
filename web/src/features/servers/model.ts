/*
 * Static knowledge about the embedded servers: labels, icons, default ports, status tones, client command examples
 * and syslog facility / severity names.
 */
import { FolderSync, Globe, HardDriveDownload, ScrollText, ShieldCheck, SquareTerminal, type LucideIcon } from 'lucide-react'
import type { ServerKindEx, ServerState, ServerStatusEx } from './types'
import { statusDotClass } from '@/components/ui/status-dot'

export interface KindInfo {
  kind: ServerKindEx
  label: string
  /** Short label for menus and badges. */
  short: string
  icon: LucideIcon
  description: string
  defaultPort: number
  /** Transport(s) shown on the card. */
  transport: string
  /** File server with a shared folder. */
  files: boolean
  /** Has an accounts list. */
  users: boolean
}

export const KINDS: Record<ServerKindEx, KindInfo> = {
  http: {
    kind: 'http',
    label: 'HTTP file server',
    short: 'HTTP',
    icon: Globe,
    description: 'Share a folder over HTTP(S): directory listings, downloads, optional uploads and basic authentication.',
    defaultPort: 8080,
    transport: 'TCP',
    files: true,
    users: true,
  },
  ftp: {
    kind: 'ftp',
    label: 'FTP server',
    short: 'FTP',
    icon: FolderSync,
    description: 'FTP / FTPS with user accounts, optional anonymous access and a passive port range.',
    defaultPort: 2121,
    transport: 'TCP',
    files: true,
    users: true,
  },
  sftp: {
    kind: 'sftp',
    label: 'SSH / SFTP server',
    short: 'SFTP',
    icon: ShieldCheck,
    description: 'SFTP jailed to a folder for password or key users; optional shell access.',
    defaultPort: 2222,
    transport: 'TCP',
    files: true,
    users: true,
  },
  tftp: {
    kind: 'tftp',
    label: 'TFTP server',
    short: 'TFTP',
    icon: HardDriveDownload,
    description: 'Serve firmware and configuration files to network devices (read-only by default).',
    defaultPort: 69,
    transport: 'UDP',
    files: true,
    users: false,
  },
  telnet: {
    kind: 'telnet',
    label: 'Telnet server',
    short: 'Telnet',
    icon: SquareTerminal,
    description: 'A shell on this machine for legacy clients, with password login.',
    defaultPort: 2323,
    transport: 'TCP',
    files: false,
    users: true,
  },
  syslog: {
    kind: 'syslog',
    label: 'Syslog server',
    short: 'Syslog',
    icon: ScrollText,
    description: 'Receive RFC 3164 / 5424 syslog from routers, switches and appliances over UDP and TCP.',
    defaultPort: 514,
    transport: 'UDP / TCP',
    files: false,
    users: false,
  },
}

export const KIND_ORDER: ServerKindEx[] = ['http', 'ftp', 'sftp', 'tftp', 'telnet', 'syslog']

export function isKind(v: unknown): v is ServerKindEx {
  return typeof v === 'string' && v in KINDS
}

export type Tone = 'running' | 'stopped' | 'busy' | 'error'

export function statusTone(st: Pick<ServerStatusEx, 'state'> | undefined): Tone {
  switch (st?.state) {
    case 'running':
      return 'running'
    case 'starting':
    case 'stopping':
      return 'busy'
    case 'error':
      return 'error'
    default:
      return 'stopped'
  }
}

export const TONE_DOT: Record<Tone, string> = {
  running: 'bg-success shadow-[0_0_0_3px] shadow-success/20',
  stopped: 'bg-muted-foreground/40',
  // Pending only: a slow, shallow breathe (docs/UX.md), never a blink.
  busy: statusDotClass('warning', true),
  error: 'bg-destructive shadow-[0_0_0_3px] shadow-destructive/20',
}

export const STATE_LABEL: Record<ServerState, string> = {
  stopped: 'Stopped',
  starting: 'Starting…',
  running: 'Running',
  stopping: 'Stopping…',
  error: 'Error',
}

/** Host part of the bind address shown to users ("all interfaces" for wildcards). */
export function bindLabel(bind: string | undefined): string {
  if (!bind || bind === '127.0.0.1' || bind === '::1') return 'this machine only'
  if (bind === '0.0.0.0' || bind === '::') return 'all interfaces'
  return bind
}

export function isWildcard(bind: string | undefined): boolean {
  return bind === '0.0.0.0' || bind === '::'
}

export function isLoopbackBind(bind: string | undefined): boolean {
  return !bind || bind === '127.0.0.1' || bind === '::1' || bind.startsWith('127.')
}

/** Host shown in examples: the URL's host when running, else the bind address. */
export function exampleHost(st: ServerStatusEx): string {
  if (st.url) {
    try {
      const u = new URL(st.url.replace(/^(sftp|tftp|telnet|syslog|ftpes|ftps):/, 'http:'))
      return u.hostname.replace(/^\[|\]$/g, '')
    } catch {
      /* fall through */
    }
  }
  const b = String(st.config.bindAddress ?? '127.0.0.1')
  return isWildcard(b) ? '<this-machine>' : b
}

function hostForCmd(h: string): string {
  return h.includes(':') ? `[${h}]` : h
}

/** Copy-paste client commands for a server (first one is the most common). */
export function clientCommands(st: ServerStatusEx): { label: string; command: string }[] {
  const host = hostForCmd(exampleHost(st))
  const port = Number(st.config.port ?? KINDS[st.kind].defaultPort)
  const users = (st.config.users as { username: string }[] | undefined) ?? []
  const user = users[0]?.username ?? 'user'
  switch (st.kind) {
    case 'http': {
      const scheme = st.config.tls ? 'https' : 'http'
      const k = st.config.tls ? ' -k' : ''
      const auth = st.config.requireAuth ? ` -u ${user}` : ''
      const out = [
        { label: 'Download (curl)', command: `curl${k}${auth} -O ${scheme}://${host}:${port}/file.bin` },
        { label: 'Download (wget)', command: `wget${st.config.tls ? ' --no-check-certificate' : ''} ${scheme}://${host}:${port}/file.bin` },
      ]
      if (!st.config.readOnly) out.push({ label: 'Upload (curl PUT)', command: `curl${k}${auth} -T file.bin ${scheme}://${host}:${port}/` })
      return out
    }
    case 'ftp': {
      const tls = String(st.config.tls ?? 'off')
      const scheme = tls === 'implicit' ? 'ftps' : 'ftp'
      const ssl = tls === 'optional' || tls === 'required' ? ' --ssl-reqd -k' : tls === 'implicit' ? ' -k' : ''
      return [
        { label: 'Download (curl)', command: `curl${ssl} -u ${user} ${scheme}://${host}:${port}/file.bin -O` },
        { label: 'Upload (curl)', command: `curl${ssl} -u ${user} -T file.bin ${scheme}://${host}:${port}/` },
        { label: 'FileZilla / WinSCP', command: `${scheme}://${user}@${host}:${port}/` },
      ]
    }
    case 'sftp':
      return [
        { label: 'OpenSSH sftp', command: `sftp -P ${port} ${user}@${host}` },
        { label: 'scp (OpenSSH ≥ 9)', command: `scp -P ${port} file.bin ${user}@${host}:/` },
        ...(st.config.shell ? [{ label: 'Shell', command: `ssh -p ${port} ${user}@${host}` }] : []),
      ]
    case 'tftp':
      return [
        { label: 'Download (curl)', command: `curl -O tftp://${host}:${port}/file.bin` },
        { label: 'Cisco IOS', command: `copy tftp://${host}/file.bin flash:` },
        ...(!st.config.readOnly ? [{ label: 'Upload (curl)', command: `curl -T file.bin tftp://${host}:${port}/` }] : []),
      ]
    case 'telnet':
      return [{ label: 'Connect', command: `telnet ${host} ${port}` }]
    case 'syslog':
      return [
        { label: 'Test (Linux logger)', command: `logger -n ${host} -P ${port} -d "hello from $(hostname)"` },
        { label: 'Test (netcat)', command: `echo "<14>test message" | nc -u -w1 ${host} ${port}` },
        { label: 'Cisco IOS', command: `logging host ${host} transport udp port ${port}` },
      ]
  }
}

// ---- syslog ---------------------------------------------------------------------------------------------------------

export const SEVERITIES = ['Emergency', 'Alert', 'Critical', 'Error', 'Warning', 'Notice', 'Info', 'Debug'] as const
export const SEVERITY_SHORT = ['emerg', 'alert', 'crit', 'err', 'warn', 'notice', 'info', 'debug'] as const

export const FACILITIES = [
  'kern', 'user', 'mail', 'daemon', 'auth', 'syslog', 'lpr', 'news', 'uucp', 'cron', 'authpriv', 'ftp', 'ntp',
  'security', 'console', 'solaris-cron', 'local0', 'local1', 'local2', 'local3', 'local4', 'local5', 'local6', 'local7',
] as const

export function severityClass(sev: number): string {
  if (sev <= 2) return 'bg-destructive/15 text-destructive border-destructive/30'
  if (sev === 3) return 'bg-destructive/10 text-destructive border-transparent'
  if (sev === 4) return 'bg-warning/15 text-warning border-transparent'
  if (sev === 5) return 'bg-info/12 text-info border-transparent'
  if (sev === 6) return 'bg-muted text-foreground/80 border-transparent'
  return 'bg-transparent text-muted-foreground border-border'
}

export function facilityName(f: number): string {
  return FACILITIES[f] ?? String(f)
}

/** Highlight colours offered for syslog rules (token-based so they work in both themes). */
export const HIGHLIGHT_COLORS: { id: string; label: string; className: string }[] = [
  { id: 'red', label: 'Red', className: 'bg-destructive/25 text-destructive' },
  { id: 'amber', label: 'Amber', className: 'bg-warning/30 text-warning' },
  { id: 'green', label: 'Green', className: 'bg-success/25 text-success' },
  { id: 'blue', label: 'Blue', className: 'bg-info/25 text-info' },
  { id: 'accent', label: 'Accent', className: 'bg-primary/25 text-primary' },
]

export function highlightClass(color: string): string {
  return HIGHLIGHT_COLORS.find((c) => c.id === color)?.className ?? HIGHLIGHT_COLORS[1].className
}

/** Seconds → "10 min" / "2 h" for the auto-stop selector. */
export const STOP_AFTER_PRESETS: { value: number; label: string }[] = [
  { value: 0, label: 'Never' },
  { value: 300, label: 'After 5 minutes' },
  { value: 900, label: 'After 15 minutes' },
  { value: 1800, label: 'After 30 minutes' },
  { value: 3600, label: 'After 1 hour' },
  { value: 4 * 3600, label: 'After 4 hours' },
  { value: 8 * 3600, label: 'After 8 hours' },
  { value: 24 * 3600, label: 'After 24 hours' },
]
