/*
 * Session editor form model: form values ⇄ Connection, zod schema (react-hook-form resolver) with protocol-aware
 * validation, and the create / PATCH payloads.
 */
import { z } from 'zod'
import { defaultPort } from '@/app/protocols'
import type { AuthMethod, Connection, ConnectionInput, ConnectionOptions, ConnectionPatch, Protocol } from '@/api/types'
import { isPlainObject, jsonEqual } from '@/lib/utils'
import { getProtocolProfile, getProtocolSpec, isSshFamily } from '../editors/define'
import { MAX_ICON_CHARS } from '../icons'
import { autoName } from '../model'
import { isValidHost } from '../quickparse'
import type { ConnectionDraft } from '../types'

export interface SessionFormValues {
  name: string
  protocol: string
  host: string
  /** null = protocol default. */
  port: number | null
  username: string
  authMethod: AuthMethod
  identityId: string | null
  keyId: string | null
  folderId: string | null
  color: string | null
  icon: string | null
  tags: string[]
  notes: string
  favorite: boolean
  shared: boolean
  options: ConnectionOptions
  /** Pending secret changes only (write-only; "" deletes a stored value). */
  secrets: Record<string, string>
}

const AUTH_METHODS = ['auto', 'password', 'key', 'agent', 'keyboard-interactive', 'none'] as const

// ---------------------------------------------------------------------------------------------------------------------
// Defaults
// ---------------------------------------------------------------------------------------------------------------------

function cloneOptions(o: ConnectionOptions | undefined): ConnectionOptions {
  return o && isPlainObject(o) ? (JSON.parse(JSON.stringify(o)) as ConnectionOptions) : {}
}

export function formDefaults(opts: { original?: Connection; initial?: ConnectionDraft; folderId?: string | null; protocol?: string }): SessionFormValues {
  const o = opts.original
  if (o) {
    return {
      name: o.name,
      protocol: o.protocol,
      host: o.host ?? '',
      port: o.port || null,
      username: o.username ?? '',
      authMethod: AUTH_METHODS.includes(o.authMethod) ? o.authMethod : 'auto',
      identityId: o.identityId || null,
      keyId: o.keyId || null,
      folderId: o.folderId || null,
      color: o.color || null,
      icon: o.icon || null,
      tags: [...(o.tags ?? [])],
      notes: o.notes ?? '',
      favorite: !!o.favorite,
      shared: !!o.shared,
      options: cloneOptions(o.options),
      secrets: {},
    }
  }
  const i = opts.initial ?? {}
  const protocol = (i.protocol as string | undefined) || opts.protocol || 'ssh'
  const secrets: Record<string, string> = { ...i.secrets }
  if (i.password) secrets[getProtocolProfile(protocol).passwordSecret ?? 'password'] = i.password
  return {
    name: i.name ?? '',
    protocol,
    host: i.host ?? '',
    port: i.port && i.port !== defaultPort(protocol) ? i.port : null,
    username: i.username ?? '',
    authMethod: i.authMethod && AUTH_METHODS.includes(i.authMethod) ? i.authMethod : 'auto',
    identityId: i.identityId || null,
    keyId: i.keyId || null,
    folderId: i.folderId !== undefined ? i.folderId || null : opts.folderId || null,
    color: i.color || null,
    icon: i.icon || null,
    tags: [...(i.tags ?? [])],
    notes: i.notes ?? '',
    favorite: !!i.favorite,
    shared: false,
    options: cloneOptions(i.options),
    secrets,
  }
}

const BLANK: Omit<Connection, 'protocol'> = {
  id: '',
  folderId: null,
  name: '',
  host: '',
  port: 0,
  username: '',
  identityId: null,
  keyId: null,
  authMethod: 'auto',
  tags: [],
  notes: '',
  favorite: false,
  sortOrder: 0,
  options: {},
  secretKeys: [],
  shared: false,
  ownerId: '',
  createdAt: '',
  updatedAt: '',
}

/** Draft as a Connection (what protocol editors receive). */
export function toConnection(v: SessionFormValues, base?: Connection): Connection {
  return {
    ...BLANK,
    ...base,
    name: v.name,
    protocol: v.protocol as Protocol,
    host: v.host,
    port: v.port ?? defaultPort(v.protocol),
    username: v.username,
    authMethod: v.authMethod,
    identityId: v.identityId,
    keyId: v.keyId,
    folderId: v.folderId,
    color: v.color ?? undefined,
    icon: v.icon ?? undefined,
    tags: v.tags,
    notes: v.notes,
    favorite: v.favorite,
    shared: v.shared,
    options: v.options ?? {},
    secrets: v.secrets ?? {},
    secretKeys: base?.secretKeys ?? [],
  }
}

/** Form fields changed by a protocol editor's onChange(next). */
export function diffFromConnection(next: Connection, cur: SessionFormValues): Partial<SessionFormValues> {
  const out: Partial<SessionFormValues> = {}
  // Editors see `port ?? default`; an unchanged value keeps "default" (null), a changed one is stored explicitly.
  const shownPort = cur.port ?? defaultPort(cur.protocol)
  const port = next.port === shownPort ? cur.port : next.port && next.port !== defaultPort(next.protocol) ? next.port : null
  const candidates: Partial<SessionFormValues> = {
    name: next.name,
    protocol: next.protocol,
    host: next.host,
    port,
    username: next.username,
    authMethod: next.authMethod,
    identityId: next.identityId ?? null,
    keyId: next.keyId ?? null,
    folderId: next.folderId ?? null,
    color: next.color ?? null,
    icon: next.icon ?? null,
    tags: next.tags,
    notes: next.notes,
    favorite: next.favorite,
    shared: next.shared,
    options: next.options ?? {},
    secrets: next.secrets ?? {},
  }
  for (const [k, v] of Object.entries(candidates) as [keyof SessionFormValues, unknown][]) {
    if (!jsonEqual(v, cur[k])) (out as Record<string, unknown>)[k] = v
  }
  return out
}

// ---------------------------------------------------------------------------------------------------------------------
// Protocol switching
// ---------------------------------------------------------------------------------------------------------------------

/** Options adjusted for a protocol change: hop settings follow the family, a mistyped `compression` is dropped. */
export function adaptOptionsForProtocol(options: ConnectionOptions, to: string): ConnectionOptions {
  const next = { ...options }
  const profile = getProtocolProfile(to)
  if (profile.jump !== 'chain') delete next.jumpHosts
  if (profile.jump !== 'gateway') delete next.sshTunnelVia
  if (!profile.network) {
    delete next.proxy
    delete next.portKnock
  }
  if ('compression' in next) {
    const c = next.compression
    const ok = isSshFamily(to) ? typeof c === 'boolean' : to === 'vnc' ? typeof c === 'number' : false
    if (!ok) delete next.compression
  }
  return next
}

// ---------------------------------------------------------------------------------------------------------------------
// Validation
// ---------------------------------------------------------------------------------------------------------------------

function stripBrackets(host: string): string {
  return host.startsWith('[') && host.endsWith(']') ? host.slice(1, -1) : host
}

function num(v: unknown): number | null {
  if (typeof v === 'number' && Number.isFinite(v)) return v
  if (typeof v === 'string' && v.trim() !== '' && Number.isFinite(Number(v))) return Number(v)
  return null
}

/** Network-tab checks (proxy, knocking, timeouts). */
function validateNetwork(o: ConnectionOptions, errors: Record<string, string>): void {
  const proxy = o.proxy
  if (proxy && isPlainObject(proxy) && proxy.type && proxy.type !== 'none') {
    const host = typeof proxy.host === 'string' ? proxy.host.trim() : ''
    if (!host) errors['options.proxy.host'] = 'Proxy host is required'
    else if (!isValidHost(stripBrackets(host))) errors['options.proxy.host'] = 'Enter a host name or IP address'
    const port = num(proxy.port)
    if (port === null || port < 1 || port > 65535) errors['options.proxy.port'] = 'Port must be between 1 and 65535'
  }
  if (Array.isArray(o.portKnock)) {
    const bad = o.portKnock.findIndex((k) => !k || !Number.isInteger(k.port) || k.port < 1 || k.port > 65535 || (k.proto !== 'tcp' && k.proto !== 'udp'))
    if (bad >= 0) errors['options.portKnock'] = `Knock ${bad + 1}: port must be 1–65535 over TCP or UDP`
  }
  const ct = num(o.connectTimeoutSec)
  if (ct !== null && (ct < 1 || ct > 600)) errors['options.connectTimeoutSec'] = 'Between 1 and 600 seconds'
  const ka = num(o.keepAliveSec)
  if (ka !== null && (ka < 0 || ka > 3600)) errors['options.keepAliveSec'] = 'Between 0 (off) and 3600 seconds'
}

function validateTerminal(o: ConnectionOptions, errors: Record<string, string>): void {
  const t = o.terminal
  if (!t || !isPlainObject(t)) return
  const fs = num(t.fontSize)
  if (fs !== null && (fs < 6 || fs > 72)) errors['options.terminal.fontSize'] = 'Font size must be between 6 and 72'
  const sb = num(t.scrollback)
  if (sb !== null && (sb < 0 || sb > 1_000_000)) errors['options.terminal.scrollback'] = 'Between 0 and 1,000,000 lines'
  const sc = o.startupCommand
  if (typeof sc === 'string' && sc.length > 16384) errors['options.startupCommand'] = 'Startup commands are too long'
}

/** All cross-field and protocol checks; keys are field paths. */
function validateDraft(v: SessionFormValues, base?: Connection): Record<string, string> {
  const errors: Record<string, string> = {}
  const profile = getProtocolProfile(v.protocol)
  const conn = toConnection(v, base)
  const host = stripBrackets(v.host.trim())

  if (profile.host !== 'hidden') {
    if (!host && profile.host === 'required') errors.host = `${profile.hostLabel ?? 'Remote host'} is required`
    else if (host && !isValidHost(host)) errors.host = 'Enter a host name or IP address (no scheme, user, port or spaces)'
  }
  if (profile.port) {
    if (v.port !== null && (!Number.isInteger(v.port) || v.port < 1 || v.port > 65535)) errors.port = 'Port must be between 1 and 65535'
    else if (profile.portRequired && !v.port) errors.port = 'Port is required'
  }
  if (profile.auth === 'ssh' && v.authMethod === 'key' && !v.keyId && !v.identityId) errors.keyId = 'Choose a stored key (or an identity with a key)'
  if (profile.network) validateNetwork(v.options, errors)
  if (profile.kind === 'terminal') validateTerminal(v.options, errors)
  const extra = getProtocolSpec(v.protocol)?.validate?.(conn)
  if (extra) for (const [k, m] of Object.entries(extra)) if (!errors[k]) errors[k] = m
  // The name is derived from the host / device / container; only ask for one when nothing else explains its absence.
  if (!v.name.trim() && !autoName(conn) && Object.keys(errors).length === 0) errors.name = 'Enter a name'
  return errors
}

export function buildSchema(base?: Connection) {
  return z
    .object({
      name: z.string().max(200, 'At most 200 characters'),
      protocol: z.string().min(1, 'Choose a protocol'),
      host: z.string().max(255, 'At most 255 characters'),
      port: z.number().nullable(),
      username: z.string().max(255, 'At most 255 characters'),
      authMethod: z.enum(AUTH_METHODS),
      identityId: z.string().nullable(),
      keyId: z.string().nullable(),
      folderId: z.string().nullable(),
      color: z.string().max(64).nullable(),
      icon: z.string().max(MAX_ICON_CHARS, 'The icon image is too large').nullable(),
      tags: z.array(z.string().max(64, 'Tags are at most 64 characters')).max(64, 'At most 64 tags'),
      notes: z.string().max(65536, 'Notes are limited to 64 KiB'),
      favorite: z.boolean(),
      shared: z.boolean(),
      options: z.record(z.string(), z.unknown()),
      secrets: z.record(z.string(), z.string().max(65536, 'Too long')),
    })
    .superRefine((v, ctx) => {
      const errors = validateDraft(v as SessionFormValues, base)
      for (const [path, message] of Object.entries(errors)) ctx.addIssue({ code: 'custom', path: path.split('.'), message })
    })
}

/** Which editor tab shows a field path (to reveal the first error). */
export function tabOfPath(path: string): 'basic' | 'protocol' | 'terminal' | 'network' | 'bookmark' | 'automation' {
  const [head, key] = path.split('.')
  if (head === 'options') {
    if (['term', 'encoding', 'backspace', 'terminal'].includes(key)) return 'terminal'
    if (['proxy', 'jumpHosts', 'sshTunnelVia', 'portKnock', 'connectTimeoutSec', 'keepAliveSec'].includes(key)) return 'network'
    if (['startupCommand', 'autoReconnect', 'record', 'log', 'logTimestamps'].includes(key)) return 'automation'
    return 'protocol'
  }
  if (head === 'secrets') return key === 'proxyPassword' ? 'network' : key === 'password' || key === 'passphrase' || key === 'vncPassword' ? 'basic' : 'protocol'
  if (['folderId', 'color', 'icon', 'tags', 'notes', 'favorite', 'shared'].includes(head)) return 'bookmark'
  return 'basic'
}

// ---------------------------------------------------------------------------------------------------------------------
// Payloads
// ---------------------------------------------------------------------------------------------------------------------

function isEmpty(v: unknown): boolean {
  return v === undefined || v === null || v === '' || (Array.isArray(v) && v.length === 0) || (isPlainObject(v) && Object.keys(v).length === 0)
}

/** Drop empty values (and a disabled proxy); nested objects are cleaned one level deep. Unknown keys survive. */
function cleanOptions(options: ConnectionOptions): ConnectionOptions {
  const out: ConnectionOptions = {}
  for (const [k, v] of Object.entries(options ?? {})) {
    if (isPlainObject(v)) {
      const inner: Record<string, unknown> = {}
      for (const [ik, iv] of Object.entries(v)) if (!isEmpty(iv)) inner[ik] = iv
      if (k === 'proxy' && (!inner.type || inner.type === 'none')) continue
      if (Object.keys(inner).length) out[k] = inner
      continue
    }
    if (!isEmpty(v)) out[k] = v
  }
  return out
}

function finalName(v: SessionFormValues): string {
  return (v.name.trim() || autoName(toConnection(v))).slice(0, 200)
}

function finalHost(v: SessionFormValues): string {
  return getProtocolProfile(v.protocol).host === 'hidden' ? '' : stripBrackets(v.host.trim())
}

function finalPort(v: SessionFormValues): number {
  return getProtocolProfile(v.protocol).port ? (v.port ?? 0) : 0
}

function nonEmptySecrets(s: Record<string, string>): Record<string, string> | undefined {
  const out: Record<string, string> = {}
  for (const [k, v] of Object.entries(s ?? {})) if (v !== '') out[k] = v
  return Object.keys(out).length ? out : undefined
}

export type CreatePayload = Partial<ConnectionInput> & Pick<ConnectionInput, 'name' | 'protocol'>

export function createPayload(v: SessionFormValues, sortOrder: number): CreatePayload {
  const profile = getProtocolProfile(v.protocol)
  return {
    name: finalName(v),
    protocol: v.protocol as Protocol,
    host: finalHost(v),
    port: finalPort(v),
    username: profile.username ? v.username.trim() : '',
    authMethod: v.authMethod,
    identityId: profile.identity ? v.identityId : null,
    keyId: profile.auth === 'ssh' ? v.keyId : null,
    folderId: v.folderId,
    color: v.color ?? '',
    icon: v.icon ?? '',
    tags: v.tags,
    notes: v.notes,
    favorite: v.favorite,
    shared: v.shared,
    sortOrder,
    options: cleanOptions(v.options),
    secrets: nonEmptySecrets(v.secrets),
  }
}

/** Only the fields that differ from the stored connection (PATCH semantics: options replace, secrets merge). */
export function patchPayload(v: SessionFormValues, o: Connection): ConnectionPatch {
  const profile = getProtocolProfile(v.protocol)
  const patch: ConnectionPatch = {}
  const set = <K extends keyof ConnectionPatch>(k: K, next: ConnectionPatch[K], prev: unknown) => {
    if (!jsonEqual(next, prev)) patch[k] = next
  }
  set('name', finalName(v), o.name)
  set('protocol', v.protocol as Protocol, o.protocol)
  set('host', finalHost(v), o.host ?? '')
  const port = finalPort(v)
  if (port !== (o.port ?? 0) && !(port === 0 && o.port === defaultPort(v.protocol))) patch.port = port
  set('username', profile.username ? v.username.trim() : '', o.username ?? '')
  set('authMethod', v.authMethod, o.authMethod)
  set('identityId', profile.identity ? v.identityId : null, o.identityId || null)
  set('keyId', profile.auth === 'ssh' ? v.keyId : null, o.keyId || null)
  set('folderId', v.folderId, o.folderId || null)
  set('color', v.color ?? '', o.color ?? '')
  set('icon', v.icon ?? '', o.icon ?? '')
  set('tags', v.tags, o.tags ?? [])
  set('notes', v.notes, o.notes ?? '')
  set('favorite', v.favorite, !!o.favorite)
  set('shared', v.shared, !!o.shared)
  set('options', cleanOptions(v.options), cleanOptions(o.options ?? {}))
  const secrets = Object.fromEntries(Object.entries(v.secrets ?? {}))
  if (Object.keys(secrets).length) patch.secrets = secrets
  return patch
}
