/*
 * Opening sessions in tabs (SPEC §7 "Opening a connection", PROTO-40). Imported by the sessions feature; light module
 * (no xterm).
 *
 *   openConnection(connOrId, {position})   POST /api/sessions {connectionId, cols, rows} → tab by kind
 *                                          (terminal | vnc | rdp); sftp/ftp/s3 → a `files` tab {connectionId}
 *   openQuick(quick & {password?})         unsaved session (quick connect)
 *   openLocalShell(shellId?)               local terminal on the Termstead host
 *   attachSession(sessionId)               focus the tab showing a running/detached session, or open one
 *   duplicateSession(tabId)                a new session with the same parameters (split/duplicate/reopen)
 *
 * Every function resolves the new tab id, or null when it failed (the error is reported with a toast).
 */
import { toast } from 'sonner'
import { getConnection } from '@/api/connections'
import { isApiError } from '@/api/client'
import { queryClient } from '@/api/queryClient'
import { queryKeys } from '@/api/queryKeys'
import { upsertById } from '@/api/optimistic'
import { createSession, getSession } from '@/api/sessions'
import type { Connection, LocalShell, Protocol, RuntimeSession } from '@/api/types'
import { protocolLabel } from '@/app/protocols'
import { protocolOpeners, type ClosedTab, type TabPlacement, type TabPosition } from '@/app/registry'
import { errorMessage } from '@/lib/utils'
import { findTabs, focusTab, getDockviewApi, getTabParams, openTab, updateTabParams } from '@/stores/workspace'
import { estimateCell } from './fonts'
import { effectiveTerminalSettings, terminalSettings } from './settings'
import type { QuickSpec, TerminalTabParams } from './types'

export interface OpenOptions {
  /** Where the tab opens (default: a new tab in the active group). */
  position?: TabPosition
  /** Tab to position relative to (default: the active tab). */
  reference?: string
  /** Focus the new tab (default true). */
  activate?: boolean
  /** Explicit tab title. */
  title?: string
  /** Where a reopened tab was (ClosedTab.placement): it goes back there. */
  placement?: TabPlacement
}

/** Protocols browsed in a file manager tab instead of a runtime session. */
export const FILE_PROTOCOLS: readonly Protocol[] = ['sftp', 'ftp', 's3']
/** Graphical protocols (session kind vnc / rdp, tab kind of the same name). */
export const GRAPHICAL_PROTOCOLS: readonly Protocol[] = ['vnc', 'rdp']

export function isFileProtocol(p: string | undefined): boolean {
  return !!p && (FILE_PROTOCOLS as readonly string[]).includes(p)
}

// ---------------------------------------------------------------------------------------------------------------------
// size estimate
// ---------------------------------------------------------------------------------------------------------------------

const TAB_BAR_PX = 30
const SCROLLBAR_PX = 14

/**
 * Terminal size (cols × rows) a new tab will most likely get, from the active group's size and the terminal font.
 * The terminal re-sends its exact size as soon as it attaches; this only avoids a visible resize on connect.
 */
export function estimateTerminalSize(position: TabPosition = 'tab', overrides?: Connection['options']['terminal']): { cols: number; rows: number } {
  const s = effectiveTerminalSettings(terminalSettings.get(), overrides)
  let width = 0
  let height = 0
  const dv = getDockviewApi()
  const group = dv?.activeGroup
  if (group && group.api.location.type !== 'popout') {
    width = group.api.width
    height = group.api.height
  } else if (dv) {
    width = dv.width
    height = dv.height
  }
  if (!width || !height) {
    const ws = document.querySelector('[data-workspace]')
    const r = ws?.getBoundingClientRect()
    width = r?.width || window.innerWidth * 0.8
    height = r?.height || window.innerHeight * 0.75
  }
  if (position === 'right' || position === 'left') width /= 2
  if (position === 'below' || position === 'above') height /= 2
  if (position === 'float') {
    width = Math.min(820, Math.max(360, width - 120))
    height = Math.min(520, Math.max(240, height - 120))
  }
  const cell = estimateCell(s.fontFamily, s.fontSize, s.lineHeight, s.letterSpacing)
  const pad = s.padding * 2
  const cols = Math.floor((width - pad - SCROLLBAR_PX) / Math.max(1, cell.width))
  const rows = Math.floor((height - TAB_BAR_PX - pad) / Math.max(1, cell.height))
  return { cols: clampInt(cols, 20, 500, 80), rows: clampInt(rows, 5, 200, 24) }
}

function clampInt(v: number, min: number, max: number, fallback: number): number {
  if (!Number.isFinite(v)) return fallback
  return Math.min(max, Math.max(min, Math.floor(v)))
}

// ---------------------------------------------------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------------------------------------------------

function reportError(what: string, err: unknown): void {
  if (isApiError(err) && err.status === 423) {
    toast.error(`${what}: the vault is locked`, { description: 'Unlock the vault to use stored credentials.' })
    return
  }
  toast.error(what, { description: errorMessage(err) })
}

const freshSessions = new Map<string, number>()
const FRESH_MS = 60_000

/** Remember that this page just created a session: its first output is live, not history (see isFreshSession). */
function markFresh(id: string): void {
  const now = Date.now()
  for (const [k, t] of freshSessions) if (now - t > FRESH_MS) freshSessions.delete(k)
  freshSessions.set(id, now)
}

/**
 * True for a session created by this page within the last minute. A terminal attaching to it from offset 0 treats
 * the replayed bytes as live output (applications may be waiting for replies to terminal queries).
 */
export function isFreshSession(id: string): boolean {
  const t = freshSessions.get(id)
  return t !== undefined && Date.now() - t < FRESH_MS
}

function rememberSession(s: RuntimeSession): void {
  queryClient.setQueryData<RuntimeSession[]>(queryKeys.sessions, (old) => upsertById(old, s))
  if (s.connectionId) void queryClient.invalidateQueries({ queryKey: queryKeys.connections, exact: true })
}

async function resolveConnection(connOrId: Connection | string): Promise<Connection> {
  if (typeof connOrId !== 'string') return connOrId
  const cached = queryClient.getQueryData<Connection[]>(queryKeys.connections)?.find((c) => c.id === connOrId)
  if (cached) return cached
  return queryClient.fetchQuery({ queryKey: queryKeys.connection(connOrId), queryFn: () => getConnection(connOrId), staleTime: 30_000 })
}

/** Quick spec safe to persist in tab params (no password / secrets). */
export function sanitizeQuick(q: Partial<Connection> & { password?: string }): QuickSpec {
  const out: QuickSpec = {}
  if (q.name) out.name = q.name
  if (q.protocol) out.protocol = q.protocol
  if (q.host) out.host = q.host
  if (typeof q.port === 'number') out.port = q.port
  if (q.username) out.username = q.username
  if (q.identityId) out.identityId = q.identityId
  if (q.keyId) out.keyId = q.keyId
  if (q.authMethod) out.authMethod = q.authMethod
  if (q.color) out.color = q.color
  if (q.icon) out.icon = q.icon
  if (q.options && typeof q.options === 'object') out.options = JSON.parse(JSON.stringify(q.options)) as Connection['options']
  return out
}

function quickTitle(q: QuickSpec): string {
  if (q.name) return q.name
  if (q.protocol === 'local') return 'Local terminal'
  if (q.host) return `${q.username ? `${q.username}@` : ''}${q.host}`
  return protocolLabel(q.protocol) || 'Terminal'
}

function tabKindFor(s: RuntimeSession): string {
  return s.kind === 'vnc' || s.kind === 'rdp' ? s.kind : 'terminal'
}

interface SessionTabExtras {
  connectionId?: string
  quick?: QuickSpec
  shell?: string
  color?: string
  title: string
}

function openSessionTab(s: RuntimeSession, extras: SessionTabExtras, opts: OpenOptions): string {
  const kind = tabKindFor(s)
  const params: TerminalTabParams = {
    sessionId: s.id,
    protocol: s.protocol,
    ...(extras.connectionId ? { connectionId: extras.connectionId } : {}),
    ...(extras.quick ? { quick: extras.quick } : {}),
    ...(extras.shell ? { shell: extras.shell } : {}),
    ...(extras.color ? { color: extras.color } : {}),
    title: extras.title,
  }
  return openTab({ kind, params, title: opts.title ?? extras.title, position: opts.position, reference: opts.reference, activate: opts.activate, placement: opts.placement })
}

// ---------------------------------------------------------------------------------------------------------------------
// public API
// ---------------------------------------------------------------------------------------------------------------------

/** Open a saved connection (see module comment). */
export async function openConnection(connOrId: Connection | string, opts: OpenOptions = {}): Promise<string | null> {
  let conn: Connection
  try {
    conn = await resolveConnection(connOrId)
  } catch (err) {
    reportError('Could not load the session', err)
    return null
  }
  const custom = protocolOpeners.get(conn.protocol)
  if (custom) {
    try {
      return await custom.open(conn, opts)
    } catch (err) {
      reportError(`Could not open ${conn.name || 'the session'}`, err)
      return null
    }
  }
  if (isFileProtocol(conn.protocol)) {
    return openTab({
      kind: 'files',
      params: { connectionId: conn.id, protocol: conn.protocol },
      title: opts.title ?? conn.name,
      position: opts.position,
      reference: opts.reference,
      activate: opts.activate,
    })
  }
  const size = estimateTerminalSize(opts.position, conn.options?.terminal)
  let s: RuntimeSession
  try {
    s = await createSession({ connectionId: conn.id, cols: size.cols, rows: size.rows })
  } catch (err) {
    reportError(`Could not open ${conn.name || 'the session'}`, err)
    return null
  }
  markFresh(s.id)
  rememberSession(s)
  return openSessionTab(s, { connectionId: conn.id, color: conn.color, title: conn.name || s.title }, opts)
}

/** Open an unsaved session (quick connect). The password (if any) is sent once and never stored in the tab. */
export async function openQuick(quick: Partial<Connection> & { password?: string }, opts: OpenOptions = {}): Promise<string | null> {
  const spec = sanitizeQuick(quick)
  const protocol = spec.protocol ?? 'ssh'
  spec.protocol = protocol
  const title = opts.title ?? quickTitle(spec)
  const custom = protocolOpeners.get(protocol)?.openQuick
  if (custom) {
    try {
      return await custom({ ...quick, protocol }, { ...opts, title })
    } catch (err) {
      reportError(`Could not open ${title}`, err)
      return null
    }
  }
  if (isFileProtocol(protocol)) {
    // File sessions have no runtime session; the files tab opens the connection itself (credentials are prompted).
    return openTab({ kind: 'files', params: { quick: spec, protocol }, title, position: opts.position, reference: opts.reference, activate: opts.activate })
  }
  const size = estimateTerminalSize(opts.position, spec.options?.terminal)
  let s: RuntimeSession
  try {
    s = await createSession({ quick: { ...quick, protocol }, cols: size.cols, rows: size.rows, title })
  } catch (err) {
    reportError(`Could not connect to ${title}`, err)
    return null
  }
  markFresh(s.id)
  rememberSession(s)
  return openSessionTab(s, { quick: spec, color: spec.color, title }, opts)
}

/** Open a local shell on the Termstead host (default shell unless `shellId`). */
export async function openLocalShell(shellId?: string, opts: OpenOptions = {}): Promise<string | null> {
  const shells = queryClient.getQueryData<LocalShell[]>(queryKeys.localShells)
  const shell = shellId ? shells?.find((x) => x.id === shellId || x.path === shellId) : undefined
  const title = opts.title ?? shell?.name ?? 'Local terminal'
  const size = estimateTerminalSize(opts.position)
  let s: RuntimeSession
  try {
    s = await createSession({
      quick: { protocol: 'local', name: title, options: shellId ? { shell: shellId } : {} },
      cols: size.cols,
      rows: size.rows,
      title,
    })
  } catch (err) {
    reportError('Could not start a local terminal', err)
    return null
  }
  markFresh(s.id)
  rememberSession(s)
  return openSessionTab(s, { shell: shellId, quick: { protocol: 'local', name: title, options: shellId ? { shell: shellId } : {} }, title }, opts)
}

/** Tab ids showing a runtime session. */
export function findSessionTabs(sessionId: string): string[] {
  return findTabs((t) => (t.params as { sessionId?: unknown } | undefined)?.sessionId === sessionId).map((t) => t.id)
}

/** Focus the tab showing a session, or open a new tab attached to it (running or detached session). */
export async function attachSession(sessionId: string, opts: OpenOptions = {}): Promise<string | null> {
  const existing = findSessionTabs(sessionId)[0]
  if (existing) {
    focusTab(existing)
    return existing
  }
  let s = queryClient.getQueryData<RuntimeSession[]>(queryKeys.sessions)?.find((x) => x.id === sessionId)
  if (!s) {
    try {
      s = await getSession(sessionId)
      rememberSession(s)
    } catch (err) {
      reportError('Could not attach to the session', err)
      return null
    }
  }
  const conn = s.connectionId ? queryClient.getQueryData<Connection[]>(queryKeys.connections)?.find((c) => c.id === s.connectionId) : undefined
  const title = s.title || conn?.name || `${s.username ? `${s.username}@` : ''}${s.host ?? protocolLabel(s.protocol)}`
  return openSessionTab(s, { connectionId: s.connectionId, color: conn?.color, title }, opts)
}

/** Create a new runtime session with the parameters of a terminal tab (used by duplicate / restart / reopen). */
async function createLike(p: Partial<TerminalTabParams>, position: TabPosition = 'tab'): Promise<{ session: RuntimeSession; extras: SessionTabExtras } | null> {
  if (p.connectionId) {
    let conn: Connection | undefined
    try {
      conn = await resolveConnection(p.connectionId)
    } catch {
      conn = undefined
    }
    const size = estimateTerminalSize(position, conn?.options?.terminal)
    try {
      const session = await createSession({ connectionId: p.connectionId, cols: size.cols, rows: size.rows })
      markFresh(session.id)
      rememberSession(session)
      return { session, extras: { connectionId: p.connectionId, color: conn?.color ?? p.color, title: conn?.name ?? p.title ?? session.title } }
    } catch (err) {
      reportError('Could not open the session', err)
      return null
    }
  }
  const quick: QuickSpec | undefined = p.quick ?? (p.protocol === 'local' ? { protocol: 'local', options: p.shell ? { shell: p.shell } : {} } : undefined)
  if (quick) {
    const title = p.title ?? quickTitle(quick)
    const size = estimateTerminalSize(position, quick.options?.terminal)
    try {
      const session = await createSession({ quick: { ...quick }, cols: size.cols, rows: size.rows, title })
      markFresh(session.id)
      rememberSession(session)
      return { session, extras: { quick, shell: p.shell, color: p.color ?? quick.color, title } }
    } catch (err) {
      reportError(`Could not connect to ${title}`, err)
      return null
    }
  }
  toast.info('This session cannot be duplicated', { description: 'It was not created from a saved or quick-connect session.' })
  return null
}

/** New session with the same parameters as a terminal tab, opened next to it (PROTO-40). */
export async function duplicateSession(tabId: string, opts: OpenOptions = {}): Promise<string | null> {
  const p = getTabParams<TerminalTabParams>(tabId)
  if (!p) return null
  const created = await createLike(p, opts.position)
  if (!created) return null
  return openSessionTab(created.session, created.extras, { ...opts, reference: opts.reference ?? tabId })
}

/**
 * Replace the session of a tab by a fresh one with the same parameters (the old session vanished, e.g. after a
 * server restart). The tab keeps its place; the terminal view re-attaches to the new session id.
 */
export async function restartSessionInTab(tabId: string): Promise<boolean> {
  const p = getTabParams<TerminalTabParams>(tabId)
  if (!p) return false
  const created = await createLike(p)
  if (!created) return false
  updateTabParams<TerminalTabParams>(tabId, { sessionId: created.session.id, protocol: created.session.protocol })
  return true
}

/** Reopen a closed terminal tab (CC-7): re-attach when its session still runs (it was detached), else start anew. */
export async function reopenTerminal(closed: ClosedTab<TerminalTabParams>): Promise<void> {
  const p = closed.params
  if (!p) return
  if (p.sessionId) {
    try {
      const s = await getSession(p.sessionId)
      if (s && s.state !== 'closed') {
        rememberSession(s)
        openTab({ kind: 'terminal', params: { ...p }, title: closed.title, placement: closed.placement })
        return
      }
    } catch {
      /* gone: start a new session below */
    }
  }
  const created = await createLike(p)
  if (created) openSessionTab(created.session, { ...created.extras, title: closed.title || created.extras.title }, { placement: closed.placement })
}
