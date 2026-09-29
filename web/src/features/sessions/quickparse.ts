/*
 * Quick connect parser (UI-1). Pure: turns a typed string into an unsaved connection draft.
 *
 *   [proto://][user[:pass]@]host[:port][/path]     ssh://root@10.0.0.1:2222, rdp://host, vnc://host:5901, sftp://u@h/var/log
 *   [user[:pass]@]host[:port]                        bare host → SSH
 *   ssh [-p port] [-l user] [-J jump] [-o K=V] [-AXC] [user@]host [command]
 *   telnet [-l user] host [port] · rlogin [-l user] host · nc|raw host port · ftp [user@]host [port]
 *   sftp [-P port] [-J jump] [user@]host[:path] · mosh [-p ports] [--ssh="ssh -p N"] [user@]host
 *   rdp|mstsc|xfreerdp host · vnc|vncviewer host[:display|::port] · winrm host · ipmi|ipmitool -H host -U user
 *   serial:/dev/ttyUSB0@115200 · COM3@9600 · serial COM3 9600
 *   docker://container · docker exec -it [-u user] container [cmd] · kubectl exec -it pod [-n ns] [-c ctr] [-- sh]
 *   kube://[context/][namespace/]pod · local [shell]
 *
 * Passwords typed inline are returned separately and stripped from `sanitized` (what may be stored in history).
 */
import type { ConnectionOptions } from '@/api/types'
import type { ConnectionDraft, EditorProtocol } from './types'

export class QuickConnectError extends Error {
  constructor(message: string) {
    super(message)
    this.name = 'QuickConnectError'
  }
}

export type QuickParse =
  | {
      kind: 'connect'
      draft: ConnectionDraft & { protocol: EditorProtocol }
      /** Input with any inline password removed (safe for history). */
      sanitized: string
      warnings: string[]
    }
  | { kind: 'local'; shell?: string; sanitized: string }
  | { kind: 'help' }

export interface QuickParseContext {
  /** Map a jump host spec (session name or [user@]host[:port]) to a saved SSH connection id. */
  resolveJump?: (spec: string) => string | undefined
}

// ---------------------------------------------------------------------------------------------------------------------
// Tokenizer
// ---------------------------------------------------------------------------------------------------------------------

/**
 * Shell-like split: whitespace separates, single/double quotes group, backslash escapes only a following space, quote
 * or backslash (so Windows paths like C:\Tools\sh.exe survive).
 */
export function tokenize(input: string): string[] {
  const out: string[] = []
  let cur = ''
  let inToken = false
  let quote: '"' | "'" | null = null
  for (let i = 0; i < input.length; i++) {
    const ch = input[i]
    if (quote) {
      if (ch === quote) quote = null
      else if (ch === '\\' && quote === '"' && (input[i + 1] === '"' || input[i + 1] === '\\')) cur += input[++i]
      else cur += ch
      continue
    }
    if (ch === '"' || ch === "'") {
      quote = ch
      inToken = true
    } else if (ch === '\\' && i + 1 < input.length && /[\s"'\\]/.test(input[i + 1])) {
      cur += input[++i]
      inToken = true
    } else if (/\s/.test(ch)) {
      if (inToken) out.push(cur)
      cur = ''
      inToken = false
    } else {
      cur += ch
      inToken = true
    }
  }
  if (quote) throw new QuickConnectError('Unterminated quote')
  if (inToken) out.push(cur)
  return out
}

// ---------------------------------------------------------------------------------------------------------------------
// Pieces
// ---------------------------------------------------------------------------------------------------------------------

const HOSTNAME_RE = /^[A-Za-z0-9_](?:[A-Za-z0-9._-]*[A-Za-z0-9_])?$/
const IPV6_RE = /^[0-9A-Fa-f:.]+(?:%[A-Za-z0-9_.-]+)?$/

/** Hostnames, IPv4, IPv6 (no brackets) and ssh_config-style aliases. */
export function isValidHost(host: string): boolean {
  if (!host || host.length > 255) return false
  if (host.includes(':')) return IPV6_RE.test(host) && (host.match(/:/g) ?? []).length >= 2
  return HOSTNAME_RE.test(host)
}

export function parsePort(raw: string, what = 'Port'): number {
  if (!/^\d{1,5}$/.test(raw)) throw new QuickConnectError(`${what} "${raw}" is not a number`)
  const n = Number(raw)
  if (n < 1 || n > 65535) throw new QuickConnectError(`${what} must be between 1 and 65535`)
  return n
}

function parseBaud(raw: string): number {
  if (!/^\d{2,8}$/.test(raw)) throw new QuickConnectError(`Baud rate "${raw}" is not a number`)
  const n = Number(raw)
  if (n < 50 || n > 20_000_000) throw new QuickConnectError('Baud rate is out of range')
  return n
}

/** host, host:port, [v6]:port, bare v6 (no port). */
export function parseHostPort(s: string): { host: string; port?: number } {
  let host = s
  let port: number | undefined
  if (s.startsWith('[')) {
    const end = s.indexOf(']')
    if (end < 0) throw new QuickConnectError('Missing "]" in IPv6 address')
    host = s.slice(1, end)
    const rest = s.slice(end + 1)
    if (rest) {
      if (!rest.startsWith(':')) throw new QuickConnectError(`Unexpected "${rest}" after the address`)
      port = parsePort(rest.slice(1))
    }
  } else if ((s.match(/:/g) ?? []).length === 1) {
    const i = s.indexOf(':')
    host = s.slice(0, i)
    port = parsePort(s.slice(i + 1))
  }
  if (!host) throw new QuickConnectError('Host is missing')
  if (!isValidHost(host)) throw new QuickConnectError(`"${host}" is not a valid host name or address`)
  return { host, port }
}

interface UserHost {
  user?: string
  password?: string
  /** Raw (as typed) password, used to scrub it from history. */
  rawPassword?: string
  hostport: string
}

/** Split "[user[:pass]@]rest" (last "@" separates, first ":" splits user and password). */
function splitUserInfo(s: string, decode: boolean): UserHost {
  const at = s.lastIndexOf('@')
  if (at < 0) return { hostport: s }
  const info = s.slice(0, at)
  const colon = info.indexOf(':')
  const dec = (v: string) => {
    if (!decode) return v
    try {
      return decodeURIComponent(v)
    } catch {
      return v
    }
  }
  const user = dec(colon >= 0 ? info.slice(0, colon) : info)
  const rawPassword = colon >= 0 ? info.slice(colon + 1) : undefined
  return { user: user || undefined, password: rawPassword ? dec(rawPassword) : undefined, rawPassword, hostport: s.slice(at + 1) }
}

/** VNC addressing: host:N with N < 100 is display N (port 5900+N); host::P is port P. */
function parseVncHostPort(s: string): { host: string; port?: number } {
  const dbl = s.match(/^([^:[\]]+)::(\d+)$/)
  if (dbl) return { ...parseHostPort(dbl[1]), port: parsePort(dbl[2]) }
  const hp = parseHostPort(s)
  if (hp.port !== undefined && hp.port < 100) hp.port = 5900 + hp.port
  return hp
}

// ---------------------------------------------------------------------------------------------------------------------
// Schemes
// ---------------------------------------------------------------------------------------------------------------------

interface SchemeDef {
  protocol: EditorProtocol
  options?: ConnectionOptions
  port?: number
}

const SCHEMES: Record<string, SchemeDef> = {
  ssh: { protocol: 'ssh' },
  ssh2: { protocol: 'ssh' },
  sftp: { protocol: 'sftp' },
  scp: { protocol: 'sftp' },
  telnet: { protocol: 'telnet' },
  mosh: { protocol: 'mosh' },
  rlogin: { protocol: 'rlogin' },
  raw: { protocol: 'raw' },
  tcp: { protocol: 'raw' },
  socket: { protocol: 'raw' },
  serial: { protocol: 'serial' },
  docker: { protocol: 'docker' },
  kube: { protocol: 'kube' },
  k8s: { protocol: 'kube' },
  kubernetes: { protocol: 'kube' },
  ftp: { protocol: 'ftp' },
  ftps: { protocol: 'ftp', options: { ftpTls: 'implicit' }, port: 990 },
  ftpes: { protocol: 'ftp', options: { ftpTls: 'explicit' } },
  s3: { protocol: 's3' },
  vnc: { protocol: 'vnc' },
  rdp: { protocol: 'rdp' },
  winrm: { protocol: 'winrm' },
  winrms: { protocol: 'winrm', options: { https: true }, port: 5986 },
  ipmi: { protocol: 'ipmi' },
  local: { protocol: 'local' },
}

export const QUICK_SCHEMES = Object.keys(SCHEMES)

/** A quoted or bare command-line value (for scrubbing inline secrets such as `-P secret` or `/p:"my pass"`). */
const ARG_VALUE = String.raw`(?:"[^"]*"|'[^']*'|\S+)`

function connectResult(
  input: string,
  scheme: SchemeDef,
  fields: { host?: string; port?: number; username?: string; password?: string; rawPassword?: string; options?: ConnectionOptions },
  warnings: string[] = [],
  /** Patterns removed from the history string (inline passwords given as command-line flags). */
  scrub: RegExp[] = [],
): QuickParse {
  const draft: ConnectionDraft & { protocol: EditorProtocol } = {
    protocol: scheme.protocol,
    host: fields.host ?? '',
    port: fields.port ?? scheme.port,
    username: fields.username ?? '',
    options: { ...scheme.options, ...fields.options },
  }
  if (fields.password) draft.password = fields.password
  let sanitized = input.trim()
  if (fields.rawPassword) sanitized = sanitized.replace(`:${fields.rawPassword}@`, '@')
  for (const re of scrub) sanitized = sanitized.replace(re, ' ').replace(/\s{2,}/g, ' ').trim()
  return { kind: 'connect', draft, sanitized, warnings }
}

// ---------------------------------------------------------------------------------------------------------------------
// URL form
// ---------------------------------------------------------------------------------------------------------------------

function parseUrlForm(input: string, schemeName: string, rest: string, ctx: QuickParseContext): QuickParse {
  const scheme = SCHEMES[schemeName]
  if (!scheme) throw new QuickConnectError(`Unknown protocol "${schemeName}://" — type "help" for the supported forms`)
  const m = rest.match(/^([^/?#]*)(\/[^?#]*)?(?:\?([^#]*))?(?:#.*)?$/)
  if (!m) throw new QuickConnectError('Malformed address')
  const authority = m[1]
  const path = m[2] ?? ''
  const query = new URLSearchParams(m[3] ?? '')
  const ui = splitUserInfo(authority, true)
  const decodePath = (p: string) => {
    try {
      return decodeURIComponent(p)
    } catch {
      return p
    }
  }

  switch (scheme.protocol) {
    case 'local':
      return { kind: 'local', shell: decodePath(authority + path) || undefined, sanitized: input.trim() }
    case 'docker': {
      const container = decodePath(ui.hostport)
      if (!container) throw new QuickConnectError('Container name is missing (docker://container)')
      const opts: ConnectionOptions = { container }
      if (ui.user) opts.user = ui.user
      // docker://web/bin/bash → "/bin/bash"; docker://web/sh → "sh"
      const shell = path.length > 1 ? (path.indexOf('/', 1) > 0 ? path : path.slice(1)) : ''
      if (shell) opts.shell = decodePath(shell)
      return connectResult(input, scheme, { options: opts })
    }
    case 'kube': {
      const segs = [ui.hostport, ...path.split('/')].filter(Boolean).map(decodePath)
      if (!segs.length) throw new QuickConnectError('Pod name is missing (kube://[context/][namespace/]pod)')
      const opts: ConnectionOptions = {}
      if (segs.length >= 3) [opts.context, opts.namespace, opts.pod] = segs.slice(-3)
      else if (segs.length === 2) [opts.namespace, opts.pod] = segs
      else opts.pod = segs[0]
      for (const k of ['context', 'namespace', 'container', 'shell'] as const) {
        const v = query.get(k)
        if (v) opts[k] = v
      }
      return connectResult(input, scheme, { options: opts })
    }
    case 's3': {
      const bucket = decodePath(ui.hostport)
      const opts: ConnectionOptions = {}
      if (bucket) opts.bucket = bucket
      if (path && path !== '/') opts.initialPath = decodePath(path)
      for (const k of ['region', 'endpoint'] as const) {
        const v = query.get(k)
        if (v) opts[k] = v
      }
      if (query.get('pathStyle') === '1' || query.get('pathStyle') === 'true') opts.pathStyle = true
      return connectResult(input, scheme, { options: opts })
    }
    case 'serial':
      return parseSerialSpec(input, decodePath(`${ui.hostport}${path}`), query.get('baud'))
    default: {
      const hp = scheme.protocol === 'vnc' ? parseVncHostPort(ui.hostport) : parseHostPort(ui.hostport)
      const opts: ConnectionOptions = {}
      if ((scheme.protocol === 'sftp' || scheme.protocol === 'ftp') && path && path !== '/') opts.initialPath = decodePath(path)
      const jump = query.get('jump')
      if (jump && (scheme.protocol === 'ssh' || scheme.protocol === 'sftp' || scheme.protocol === 'mosh')) {
        opts.jumpHosts = resolveJumps(jump, ctx)
      }
      return connectResult(input, scheme, {
        host: hp.host,
        port: hp.port,
        username: ui.user,
        password: ui.password,
        rawPassword: ui.rawPassword,
        options: opts,
      })
    }
  }
}

// ---------------------------------------------------------------------------------------------------------------------
// Serial
// ---------------------------------------------------------------------------------------------------------------------

const SERIAL_DEVICE_RE = /^(?:COM\d{1,3}|\/dev\/[A-Za-z0-9._/-]+|\\\\\.\\COM\d{1,3})$/i

function parseSerialSpec(input: string, spec: string, baudRaw?: string | null): QuickParse {
  let device = spec.trim()
  let baud: number | undefined
  const at = device.lastIndexOf('@')
  if (at > 0) {
    baud = parseBaud(device.slice(at + 1))
    device = device.slice(0, at)
  }
  if (baudRaw) baud = parseBaud(baudRaw)
  if (!device) throw new QuickConnectError('Serial device is missing (e.g. serial:/dev/ttyUSB0@115200 or COM3@9600)')
  if (!SERIAL_DEVICE_RE.test(device)) throw new QuickConnectError(`"${device}" does not look like a serial port (COM3, /dev/ttyUSB0…)`)
  const options: ConnectionOptions = { device: /^com\d/i.test(device) ? device.toUpperCase() : device }
  if (baud) options.baud = baud
  return connectResult(input, { protocol: 'serial' }, { options })
}

// ---------------------------------------------------------------------------------------------------------------------
// Command forms
// ---------------------------------------------------------------------------------------------------------------------

/**
 * Jump hosts: saved SSH sessions (by name or address) become their connection id; anything else must be an ad-hoc
 * `[user@]host[:port]`, which the backend accepts as a hop too (SPEC §9 B1).
 */
function resolveJumps(spec: string, ctx: QuickParseContext): string[] {
  const out: string[] = []
  for (const part of spec.split(',').map((s) => s.trim()).filter(Boolean)) {
    const id = ctx.resolveJump?.(part)
    if (id) {
      out.push(id)
      continue
    }
    const ui = splitUserInfo(part, false)
    if (ui.password) throw new QuickConnectError(`Jump host "${part}": passwords cannot be given inline — you will be asked`)
    parseHostPort(ui.hostport) // validates (throws a readable error)
    out.push(part)
  }
  return out
}

function need(args: string[], i: number, flag: string): string {
  const v = args[i]
  if (v === undefined) throw new QuickConnectError(`Option ${flag} needs a value`)
  return v
}

/** ssh / sftp command lines (OpenSSH flags we can map; the rest are ignored). */
function parseSshCommand(input: string, args: string[], ctx: QuickParseContext, protocol: 'ssh' | 'sftp'): QuickParse {
  const WITH_ARG = new Set(['-b', '-B', '-c', '-D', '-E', '-e', '-F', '-I', '-i', '-J', '-L', '-l', '-m', '-O', '-o', '-p', '-P', '-Q', '-R', '-S', '-W', '-w'])
  const options: ConnectionOptions = {}
  const warnings: string[] = []
  let port: number | undefined
  let user: string | undefined
  let hostOverride: string | undefined
  let target: string | undefined
  const command: string[] = []

  const applyOption = (kv: string) => {
    const m = kv.match(/^\s*([A-Za-z]+)\s*[= ]\s*(.+?)\s*$/)
    if (!m) return
    const key = m[1].toLowerCase()
    const val = m[2]
    const yes = /^(yes|true|on|1)$/i.test(val)
    switch (key) {
      case 'port':
        port = parsePort(val)
        break
      case 'user':
        user = val
        break
      case 'hostname':
        hostOverride = val
        break
      case 'proxyjump':
        if (!/^none$/i.test(val)) options.jumpHosts = resolveJumps(val, ctx)
        break
      case 'compression':
        options.compression = yes
        break
      case 'forwardagent':
        options.agentForwarding = yes
        break
      case 'forwardx11':
        options.x11Forwarding = yes
        break
      case 'connecttimeout':
        if (/^\d+$/.test(val)) options.connectTimeoutSec = Number(val)
        break
      case 'serveraliveinterval':
        if (/^\d+$/.test(val)) options.keepAliveSec = Number(val)
        break
      default:
        warnings.push(`-o ${m[1]} is not supported and was ignored`)
    }
  }

  for (let i = 0; i < args.length; i++) {
    const a = args[i]
    if (target !== undefined) {
      command.push(a)
      continue
    }
    if (a === '--') {
      target = need(args, ++i, '--')
      continue
    }
    if (a.startsWith('-') && a.length > 1) {
      const flag = a.slice(0, 2)
      if (WITH_ARG.has(flag)) {
        const val = a.length > 2 ? a.slice(2) : need(args, ++i, flag)
        if ((flag === '-p' && protocol === 'ssh') || (flag === '-P' && protocol === 'sftp')) port = parsePort(val)
        else if (flag === '-l') user = val
        else if (flag === '-J') options.jumpHosts = resolveJumps(val, ctx)
        else if (flag === '-o') applyOption(val)
        else if (flag === '-i') warnings.push('-i is ignored: pick a stored key in the session editor')
        else warnings.push(`${flag} is not supported in quick connect and was ignored`)
        continue
      }
      for (const ch of a.slice(1)) {
        if (ch === 'A') options.agentForwarding = true
        else if (ch === 'a') options.agentForwarding = false
        else if (ch === 'X' || ch === 'Y') options.x11Forwarding = true
        else if (ch === 'x') options.x11Forwarding = false
        else if (ch === 'C') options.compression = true
      }
      continue
    }
    target = a
  }
  if (!target) throw new QuickConnectError(`${protocol} needs a destination, e.g. ${protocol} user@host`)
  if (/^[a-z][a-z0-9+.-]*:\/\//i.test(target)) {
    const res = parseQuickConnect(target, ctx)
    if (res.kind !== 'connect') return res
    const d = res.draft
    const out = connectResult(
      input,
      { protocol: d.protocol },
      { host: d.host, port: port ?? d.port, username: user ?? d.username, password: d.password, options: { ...d.options, ...options } },
      [...warnings, ...res.warnings],
    )
    // The nested URL scrubbed its own password; carry that into the full string.
    if (out.kind === 'connect') out.sanitized = input.trim().replace(target, res.sanitized)
    return out
  }

  const ui = splitUserInfo(target, false)
  let hostport = ui.hostport
  if (protocol === 'sftp') {
    // sftp user@host:path (a numeric suffix is taken as the port).
    const m = hostport.match(/^([^[\]:]+):(.*)$/)
    if (m && m[2] && !/^\d+$/.test(m[2])) {
      hostport = m[1]
      options.initialPath = m[2]
    }
  }
  const hp = parseHostPort(hostOverride ?? hostport)
  if (command.length && protocol === 'ssh') options.remoteCommand = command.join(' ')
  return connectResult(
    input,
    { protocol },
    { host: hp.host, port: port ?? hp.port, username: user ?? ui.user, password: ui.password, rawPassword: ui.rawPassword, options },
    warnings,
  )
}

function parseMoshCommand(input: string, args: string[]): QuickParse {
  const options: ConnectionOptions = {}
  let sshPort: number | undefined
  let target: string | undefined
  const command: string[] = []
  for (let i = 0; i < args.length; i++) {
    const a = args[i]
    if (target !== undefined) {
      command.push(a)
      continue
    }
    if (a === '-p' || a === '--port') options.moshPorts = need(args, ++i, a)
    else if (a.startsWith('--port=')) options.moshPorts = a.slice(7)
    else if (a === '-a') options.predict = 'always'
    else if (a === '-n') options.predict = 'never'
    else if (a.startsWith('--predict=')) {
      const v = a.slice(10)
      if (v === 'adaptive' || v === 'always' || v === 'never') options.predict = v
    } else if (a.startsWith('--ssh=') || a === '--ssh') {
      const sshCmd = a === '--ssh' ? need(args, ++i, a) : a.slice(6)
      const inner = tokenize(sshCmd)
      for (let j = 0; j < inner.length; j++) {
        if (inner[j] === '-p') sshPort = parsePort(need(inner, ++j, '-p'))
        else if (/^-p\d+$/.test(inner[j])) sshPort = parsePort(inner[j].slice(2))
      }
    } else if (a === '--') {
      target = need(args, ++i, '--')
    } else if (a.startsWith('-')) {
      /* other mosh flags are not relevant server-side */
    } else target = a
  }
  if (!target) throw new QuickConnectError('mosh needs a destination, e.g. mosh user@host')
  if (options.moshPorts !== undefined && !/^\d{1,5}(?::\d{1,5})?$/.test(String(options.moshPorts))) {
    throw new QuickConnectError('mosh port must be PORT or PORT:PORT')
  }
  if (command.length) options.remoteCommand = command.join(' ')
  const ui = splitUserInfo(target, false)
  const hp = parseHostPort(ui.hostport)
  return connectResult(input, { protocol: 'mosh' }, { host: hp.host, port: sshPort ?? hp.port, username: ui.user, options })
}

function parseSimpleHostCommand(
  input: string,
  args: string[],
  scheme: SchemeDef,
  opts: { portArg?: boolean; loginFlag?: boolean; vnc?: boolean } = {},
): QuickParse {
  let user: string | undefined
  const positional: string[] = []
  for (let i = 0; i < args.length; i++) {
    const a = args[i]
    if (opts.loginFlag && a === '-l') user = need(args, ++i, '-l')
    else if (a.startsWith('-') && a.length > 1 && positional.length === 0) {
      /* ignore unknown flags before the host */
    } else positional.push(a)
  }
  if (!positional.length) throw new QuickConnectError(`Host is missing, e.g. ${scheme.protocol} host`)
  const ui = splitUserInfo(positional[0], false)
  const hp = opts.vnc ? parseVncHostPort(ui.hostport) : parseHostPort(ui.hostport)
  let port = hp.port
  if (opts.portArg && positional[1] !== undefined) port = parsePort(positional[1])
  else if (positional.length > (opts.portArg ? 2 : 1)) throw new QuickConnectError(`Unexpected "${positional.slice(opts.portArg ? 2 : 1).join(' ')}"`)
  return connectResult(input, scheme, {
    host: hp.host,
    port,
    username: user ?? ui.user,
    password: ui.password,
    rawPassword: ui.rawPassword,
  })
}

/** mstsc /v:host[:port] and xfreerdp /v:host /u:user /d:domain /port:N (a /p: password is never kept in history). */
function parseRdpCommand(input: string, args: string[]): QuickParse {
  let target: string | undefined
  let user: string | undefined
  let domain: string | undefined
  let port: number | undefined
  let password: string | undefined
  for (const a of args) {
    const m = a.match(/^[/-]([a-z]+):(.*)$/i)
    if (m) {
      const k = m[1].toLowerCase()
      if (k === 'v') target = m[2]
      else if (k === 'u') user = m[2]
      else if (k === 'd') domain = m[2]
      else if (k === 'port') port = parsePort(m[2])
      else if (k === 'p') password = m[2]
    } else if (!a.startsWith('/') && !a.startsWith('-') && !target) target = a
  }
  if (!target) throw new QuickConnectError('RDP host is missing, e.g. rdp host or mstsc /v:host')
  const ui = splitUserInfo(target, false)
  const hp = parseHostPort(ui.hostport)
  const options: ConnectionOptions = {}
  let username = user ?? ui.user
  if (username && username.includes('\\') && !domain) {
    const [d, u] = username.split('\\', 2)
    domain = d
    username = u
  }
  if (domain) options.domain = domain
  return connectResult(
    input,
    { protocol: 'rdp' },
    { host: hp.host, port: port ?? hp.port, username, password: password ?? ui.password, rawPassword: ui.rawPassword, options },
    [],
    [new RegExp(String.raw`(^|\s)[/-]p:${ARG_VALUE}`, 'gi')],
  )
}

function parseDockerCommand(input: string, args: string[]): QuickParse {
  const rest = args[0] === 'exec' ? args.slice(1) : args
  const options: ConnectionOptions = {}
  let container: string | undefined
  const cmd: string[] = []
  for (let i = 0; i < rest.length; i++) {
    const a = rest[i]
    if (container !== undefined) {
      cmd.push(a)
      continue
    }
    if (a === '-u' || a === '--user') options.user = need(rest, ++i, a)
    else if (a.startsWith('--user=')) options.user = a.slice(7)
    else if (a === '-w' || a === '--workdir' || a === '-e' || a === '--env') i++
    else if (a.startsWith('-')) {
      /* -it, -d, --privileged… */
    } else container = a
  }
  if (!container) throw new QuickConnectError('Container is missing, e.g. docker exec -it web bash')
  options.container = container
  if (cmd.length) options.shell = cmd.join(' ')
  return connectResult(input, { protocol: 'docker' }, { options })
}

function parseKubeCommand(input: string, args: string[]): QuickParse {
  const rest = args[0] === 'exec' ? args.slice(1) : args
  const options: ConnectionOptions = {}
  let pod: string | undefined
  for (let i = 0; i < rest.length; i++) {
    const a = rest[i]
    if (a === '--') {
      const cmd = rest.slice(i + 1)
      if (cmd.length) options.shell = cmd.join(' ')
      break
    }
    if (a === '-n' || a === '--namespace') options.namespace = need(rest, ++i, a)
    else if (a.startsWith('--namespace=')) options.namespace = a.slice(12)
    else if (a === '-c' || a === '--container') options.container = need(rest, ++i, a)
    else if (a.startsWith('--container=')) options.container = a.slice(12)
    else if (a === '--context') options.context = need(rest, ++i, a)
    else if (a.startsWith('--context=')) options.context = a.slice(10)
    else if (a.startsWith('-')) {
      /* -it, --stdin, --tty */
    } else if (!pod) pod = a
  }
  if (!pod) throw new QuickConnectError('Pod is missing, e.g. kubectl exec -it my-pod -n default -- sh')
  // "ns/pod" or "pod/name" shorthands
  const segs = pod.split('/').filter(Boolean)
  if (segs[0] === 'pod' || segs[0] === 'pods') segs.shift()
  if (segs.length >= 2 && !options.namespace) options.namespace = segs[segs.length - 2]
  options.pod = segs[segs.length - 1]
  return connectResult(input, { protocol: 'kube' }, { options })
}

function parseIpmiCommand(input: string, args: string[]): QuickParse {
  let host: string | undefined
  let user: string | undefined
  let password: string | undefined
  let port: number | undefined
  let rawPassword: string | undefined
  const options: ConnectionOptions = {}
  const positional: string[] = []
  for (let i = 0; i < args.length; i++) {
    const a = args[i]
    if (a === '-H') host = need(args, ++i, a)
    else if (a === '-U') user = need(args, ++i, a)
    else if (a === '-p') port = parsePort(need(args, ++i, a))
    else if (a === '-I') {
      const v = need(args, ++i, a)
      if (v === 'lan' || v === 'lanplus') options.ipmiInterface = v
    } else if (a === '-C') {
      const v = need(args, ++i, a)
      if (/^\d{1,2}$/.test(v)) options.cipherSuite = Number(v)
    } else if (a === '-P') password = need(args, ++i, a)
    else if (!a.startsWith('-')) positional.push(a)
  }
  // ipmi [user@]host — or ipmitool … sol activate (verbs are ignored)
  if (!host) {
    const cand = positional.find((p) => !['sol', 'activate', 'deactivate', 'shell'].includes(p))
    if (cand) {
      const ui = splitUserInfo(cand, false)
      const hp = parseHostPort(ui.hostport)
      host = hp.host
      port = port ?? hp.port
      user = user ?? ui.user
      password = password ?? ui.password
      rawPassword = ui.rawPassword
    }
  } else if (!isValidHost(host)) throw new QuickConnectError(`"${host}" is not a valid host name or address`)
  if (!host) throw new QuickConnectError('BMC host is missing, e.g. ipmitool -I lanplus -H 10.0.0.9 -U admin')
  return connectResult(input, { protocol: 'ipmi' }, { host, port, username: user, password, rawPassword, options }, [], [
    new RegExp(String.raw`(^|\s)-P\s+${ARG_VALUE}`, 'g'),
  ])
}

// ---------------------------------------------------------------------------------------------------------------------
// Entry point
// ---------------------------------------------------------------------------------------------------------------------

export function parseQuickConnect(raw: string, ctx: QuickParseContext = {}): QuickParse {
  const input = raw.trim()
  if (!input) throw new QuickConnectError('Type a host, e.g. user@host:22')
  if (/^(help|\?|-h|--help)$/i.test(input)) return { kind: 'help' }

  // serial:DEVICE[@baud] (also serial:///dev/ttyUSB0?baud=…)
  const serial = input.match(/^serial:(?:\/\/)?([^?\s]+)(?:\?baud=(\d+))?$/i)
  if (serial) return parseSerialSpec(input, serial[1], serial[2])

  // URL form
  const url = input.match(/^([a-z][a-z0-9+.-]*):\/\/(\S*)$/i)
  if (url) return parseUrlForm(input, url[1].toLowerCase(), url[2], ctx)

  const args = tokenize(input)
  const cmd = args[0].toLowerCase()
  const rest = args.slice(1)
  switch (cmd) {
    case 'ssh':
      return parseSshCommand(input, rest, ctx, 'ssh')
    case 'sftp':
    case 'scp':
      return parseSshCommand(input, rest, ctx, 'sftp')
    case 'mosh':
      return parseMoshCommand(input, rest)
    case 'telnet':
      return parseSimpleHostCommand(input, rest, SCHEMES.telnet, { portArg: true, loginFlag: true })
    case 'rlogin':
      return parseSimpleHostCommand(input, rest, SCHEMES.rlogin, { loginFlag: true })
    case 'nc':
    case 'ncat':
    case 'netcat':
    case 'raw': {
      const res = parseSimpleHostCommand(input, rest, SCHEMES.raw, { portArg: true })
      if (res.kind === 'connect' && !res.draft.port) throw new QuickConnectError('Raw TCP needs a port, e.g. nc host 8080')
      return res
    }
    case 'ftp':
      return parseSimpleHostCommand(input, rest, SCHEMES.ftp, { portArg: true })
    case 'rdp':
    case 'mstsc':
    case 'xfreerdp':
    case 'wlfreerdp':
      return parseRdpCommand(input, rest)
    case 'vnc':
    case 'vncviewer':
      return parseSimpleHostCommand(input, rest, SCHEMES.vnc, { vnc: true })
    case 'winrm':
      return parseSimpleHostCommand(input, rest, SCHEMES.winrm, { portArg: true })
    case 'ipmi':
    case 'ipmitool':
      return parseIpmiCommand(input, rest)
    case 'serial': {
      if (!rest.length) throw new QuickConnectError('Serial device is missing, e.g. serial COM3 9600')
      const res = parseSerialSpec(input, rest[0], rest[1])
      if (rest.length > 2) throw new QuickConnectError(`Unexpected "${rest.slice(2).join(' ')}"`)
      return res
    }
    case 'docker':
      return parseDockerCommand(input, rest)
    case 'kubectl':
    case 'kube':
    case 'k8s':
      return parseKubeCommand(input, rest)
    case 'local':
    case 'shell':
      return { kind: 'local', shell: rest.length ? rest.join(' ') : undefined, sanitized: input }
  }

  // COM3[@9600] / /dev/ttyUSB0[@115200]
  if (args.length <= 2 && SERIAL_DEVICE_RE.test(args[0].replace(/@\d+$/, ''))) return parseSerialSpec(input, args[0], args[1])

  // host port → SSH on that port
  if (args.length === 2 && /^\d{1,5}$/.test(args[1])) {
    const ui = splitUserInfo(args[0], false)
    const hp = parseHostPort(ui.hostport)
    return connectResult(input, SCHEMES.ssh, {
      host: hp.host,
      port: parsePort(args[1]),
      username: ui.user,
      password: ui.password,
      rawPassword: ui.rawPassword,
    })
  }
  if (args.length > 1) throw new QuickConnectError(`Don't know how to connect to "${input}" — type "help" for examples`)

  // Bare [user[:pass]@]host[:port] → SSH
  const ui = splitUserInfo(args[0], false)
  const hp = parseHostPort(ui.hostport)
  return connectResult(input, SCHEMES.ssh, {
    host: hp.host,
    port: hp.port,
    username: ui.user,
    password: ui.password,
    rawPassword: ui.rawPassword,
  })
}

/** Examples shown by the `help` command. */
export const QUICK_CONNECT_HELP: { example: string; description: string }[] = [
  { example: 'user@host:2222', description: 'SSH (the default protocol)' },
  { example: 'ssh -p 2222 -J bastion user@host', description: 'OpenSSH syntax; -J takes saved session names or user@host:port' },
  { example: 'telnet 10.0.0.1 23', description: 'Telnet' },
  { example: 'rdp://admin@winbox', description: 'RDP (also mstsc /v:host)' },
  { example: 'vnc://host:5901', description: 'VNC (host:1 = display 1)' },
  { example: 'sftp://user@host/var/log', description: 'SFTP file browser at a path' },
  { example: 'serial:/dev/ttyUSB0@115200', description: 'Serial console (COM3@9600 on Windows)' },
  { example: 'docker://web', description: 'Shell in a container (docker exec -it web bash)' },
  { example: 'kubectl exec -it pod -n ns -- sh', description: 'Kubernetes pod shell' },
  { example: 'local', description: 'Local shell on the Termstead host' },
  { example: 'My saved session', description: 'A saved session name (or @name)' },
]
