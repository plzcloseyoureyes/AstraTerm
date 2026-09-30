/*
 * Sending text to sessions: target resolution (active terminal, broadcast group, every terminal, chosen
 * sessions), snippet runs (variables → per-target rendering → dangerous-command guard → delivery) and plain text.
 *
 * Open terminals receive data on their own socket with TerminalHandle.send (raw, this session only — never the
 * broadcast fan-out, which would multiply multi-target sends); sessions without an open tab get POST /input.
 * Paste-mode snippets use bracketed paste when the application enabled it (mode 2004).
 */
import { toast } from 'sonner'
import { queryClient } from '@/api/queryClient'
import { queryKeys } from '@/api/queryKeys'
import { sendSessionInput } from '@/api/sessions'
import type { Connection, RuntimeSession, Snippet } from '@/api/types'
import { getActiveTerminal, getTerminalsBySession, listTerminals } from '@/features/terminal/bus'
import { multiExecTargets, useMultiExecStore } from '@/features/terminal/multiexec'
import type { TerminalHandle } from '@/features/terminal/types'
import { errorMessage, plural } from '@/lib/utils'
import { dangerousMatches, pacedSend, runSnippetOnSessions } from './api'
import { checkDangerous } from './guard'
import { askDangerous, askVariables, pickSessions } from './store'
import { builtinValues, promptVars, renderTemplate, usesClipboard } from './template'

export type TargetMode = 'active' | 'multiexec' | 'all'

export interface Target {
  sessionId: string
  title: string
  handle?: TerminalHandle
  session?: RuntimeSession
  connection?: Connection
}

function cachedSession(id: string): RuntimeSession | undefined {
  return queryClient.getQueryData<RuntimeSession[]>(queryKeys.sessions)?.find((s) => s.id === id)
}

export function cachedConnection(id: string | undefined): Connection | undefined {
  if (!id) return undefined
  return queryClient.getQueryData<Connection[]>(queryKeys.connections)?.find((c) => c.id === id)
}

function fromHandle(h: TerminalHandle): Target {
  const session = h.session() ?? cachedSession(h.sessionId)
  return { sessionId: h.sessionId, title: h.info().title || session?.title || 'Terminal', handle: h, session, connection: cachedConnection(session?.connectionId) }
}

/** A target for a session id (its open terminal when there is one). */
export function targetForSession(sessionId: string): Target {
  const h = getTerminalsBySession(sessionId)[0]
  if (h) return fromHandle(h)
  const session = cachedSession(sessionId)
  return { sessionId, title: session?.title || 'Session', session, connection: cachedConnection(session?.connectionId) }
}

/** Resolve targets for a mode. The broadcast group falls back to every terminal when broadcasting is off. */
export function resolveTargets(mode: TargetMode): Target[] {
  let handles: TerminalHandle[]
  switch (mode) {
    case 'active': {
      const h = getActiveTerminal()
      handles = h ? [h] : []
      break
    }
    case 'multiexec':
      handles = useMultiExecStore.getState().active ? multiExecTargets() : listTerminals()
      break
    default:
      handles = listTerminals()
  }
  const seen = new Set<string>()
  const out: Target[] = []
  for (const h of handles) {
    if (seen.has(h.sessionId)) continue
    seen.add(h.sessionId)
    out.push(fromHandle(h))
  }
  return out
}

const BRACKET_MARKERS = /\x1b\[20[01]~/g // oxlint-disable-line no-control-regex

/** Deliver raw data to one target. */
async function deliver(t: Target, data: string): Promise<boolean> {
  const h = t.handle ?? getTerminalsBySession(t.sessionId)[0]
  if (h && h.send(data)) return true
  try {
    await sendSessionInput(t.sessionId, data)
    return true
  } catch (err) {
    console.warn('[automation] send failed', err)
    return false
  }
}

/** The bytes a snippet sends in a terminal: execute = Enter at the end; paste keeps the text for editing. */
function snippetPayload(text: string, mode: Snippet['sendMode'], bracketed: boolean): string {
  const clean = text.replace(BRACKET_MARKERS, '')
  const body = clean.replace(/\r\n?/g, '\n')
  const multiline = body.replace(/\n$/, '').includes('\n')
  if (bracketed && (mode === 'paste' || multiline)) {
    const inner = mode === 'execute' ? body.replace(/\n$/, '') : body
    return `\x1b[200~${inner.replace(/\n/g, '\r')}\x1b[201~` + (mode === 'execute' ? '\r' : '')
  }
  const cr = body.replace(/\n/g, '\r')
  return mode === 'execute' && !cr.endsWith('\r') ? cr + '\r' : cr
}

function isBracketed(t: Target): boolean {
  try {
    return !!t.handle?.term.modes.bracketedPasteMode
  } catch {
    return false
  }
}

async function readClipboardText(): Promise<string> {
  try {
    return await navigator.clipboard.readText()
  } catch {
    toast.warning('The clipboard could not be read', { description: '{{clipboard}} was left empty.' })
    return ''
  }
}

const lastValues = new Map<string, Record<string, string>>()

/** Ask for a snippet's variables (remembering non-secret answers for next time). Null when cancelled. */
export async function collectVariables(snippet: Pick<Snippet, 'id' | 'name' | 'content'>): Promise<Record<string, string> | null> {
  const vars = promptVars(snippet.content)
  if (!vars.length) return {}
  const values = await askVariables(snippet.name, vars, lastValues.get(snippet.id) ?? {})
  if (!values) return null
  const keep: Record<string, string> = {}
  for (const v of vars) if (!v.secret && values[v.name] !== undefined) keep[v.name] = values[v.name]
  lastValues.set(snippet.id, keep)
  return values
}

type GuardDecision = 'clean' | 'confirmed' | 'cancel'

/** Run the guard on text for targets: nothing found, the user confirmed, or the user cancelled. */
async function guardDecision(texts: string[], targets: Target[]): Promise<GuardDecision> {
  const matches = checkDangerous(texts.join('\n'))
  if (!matches.length) return 'clean'
  return (await askDangerous(matches, targets.map((t) => t.title))) ? 'confirmed' : 'cancel'
}

/** Guard text for targets; resolves true when it may be sent. */
async function guardOk(texts: string[], targets: Target[]): Promise<boolean> {
  return (await guardDecision(texts, targets)) !== 'cancel'
}

/** Run a snippet on targets (open terminals or sessions). Resolves the number of sessions that got it. */
export async function sendSnippet(snippet: Snippet, targets: Target[], opts: { sendMode?: Snippet['sendMode'] } = {}): Promise<number> {
  if (!targets.length) {
    toast.info('No terminal to send the snippet to', { description: 'Open or focus a terminal first.' })
    return 0
  }
  const values = await collectVariables(snippet)
  if (!values) return 0
  const clipboard = usesClipboard(snippet.content) ? await readClipboardText() : ''
  const mode = opts.sendMode ?? snippet.sendMode
  const rendered = targets.map((t) => renderTemplate(snippet.content, values, builtinValues(t.session, t.connection, clipboard)).text)
  if (!(await guardOk(rendered, targets))) return 0
  const results = await Promise.all(targets.map((t, i) => deliver(t, snippetPayload(rendered[i], mode, isBracketed(t)))))
  const ok = results.filter(Boolean).length
  if (ok < targets.length) toast.warning(`Sent to ${ok} of ${plural(targets.length, 'session')}`)
  else if (targets.length > 1) toast.success(`“${snippet.name}” sent to ${plural(ok, 'session')}`)
  targets[0]?.handle?.focus()
  return ok
}

/**
 * Run a snippet on sessions through the backend (POST /api/snippets/{id}/run): used for the command contract
 * automation.runSnippet {id, sessionIds} and for sessions that are not open in this browser.
 */
export async function runSnippetServer(snippet: Snippet, sessionIds: string[]): Promise<number> {
  const values = await collectVariables(snippet)
  if (!values) return 0
  if (usesClipboard(snippet.content) && values.clipboard === undefined) values.clipboard = await readClipboardText()
  const titles = sessionIds.map((id) => targetForSession(id).title)
  const pre = checkDangerous(renderTemplate(snippet.content, values).text)
  let confirm = false
  if (pre.length) {
    if (!(await askDangerous(pre, titles))) return 0
    confirm = true
  }
  for (;;) {
    try {
      const { results } = await runSnippetOnSessions(snippet.id, { sessionIds, variables: values, confirmDangerous: confirm })
      const ok = results.filter((r) => r.ok).length
      const failed = results.filter((r) => !r.ok)
      if (failed.length) toast.warning(`Sent to ${ok} of ${plural(results.length, 'session')}`, { description: failed[0].error })
      else toast.success(`“${snippet.name}” sent to ${plural(ok, 'session')}`)
      return ok
    } catch (err) {
      const matches = dangerousMatches(err)
      if (matches && !confirm) {
        if (!(await askDangerous(matches, titles))) return 0
        confirm = true
        continue
      }
      toast.error('The snippet could not be run', { description: errorMessage(err) })
      return 0
    }
  }
}

export interface ComposeOptions {
  /** Press Enter after the last line. */
  enter: boolean
  lineDelayMs: number
  charDelayMs: number
  waitPrompt: boolean
}

/**
 * Send composed text. Without pacing it is delivered directly (lines separated by Enter); with pacing the backend
 * types it (reliable in background tabs) and the returned job id reports progress.
 */
export async function sendComposed(text: string, targets: Target[], opts: ComposeOptions): Promise<{ sent: number; jobId?: string }> {
  if (!targets.length || !text) return { sent: 0 }
  const paced = opts.lineDelayMs > 0 || opts.charDelayMs > 0 || opts.waitPrompt
  const decision = await guardDecision([text], targets)
  if (decision === 'cancel') return { sent: 0 }
  if (paced) {
    for (let confirm = decision === 'confirmed'; ; ) {
      try {
        const { jobId } = await pacedSend({
          sessionIds: targets.map((t) => t.sessionId),
          text,
          lineDelayMs: opts.lineDelayMs,
          charDelayMs: opts.charDelayMs,
          waitPrompt: opts.waitPrompt,
          enter: opts.enter,
          confirmDangerous: confirm,
        })
        return { sent: targets.length, jobId }
      } catch (err) {
        const matches = dangerousMatches(err)
        if (matches && !confirm) {
          if (!(await askDangerous(matches, targets.map((t) => t.title)))) return { sent: 0 }
          confirm = true
          continue
        }
        throw err
      }
    }
  }
  let data = text.replace(/\r\n?/g, '\n').replace(/\n/g, '\r')
  if (opts.enter && !data.endsWith('\r')) data += '\r'
  const results = await Promise.all(targets.map((t) => deliver(t, data)))
  return { sent: results.filter(Boolean).length }
}

/** Send raw data (button bar, escapes already decoded) after the guard. */
export async function sendRaw(data: string, targets: Target[]): Promise<number> {
  if (!targets.length) {
    toast.info('No terminal to send to', { description: 'Open or focus a terminal first.' })
    return 0
  }
  if (!(await guardOk([data], targets))) return 0
  const results = await Promise.all(targets.map((t) => deliver(t, data)))
  return results.filter(Boolean).length
}

/** Send a snippet to the active terminal, the broadcast group, every terminal or chosen sessions. */
export async function sendSnippetTo(s: Snippet, where: 'active' | 'multiexec' | 'all' | 'pick'): Promise<void> {
  if (where === 'pick') {
    const ids = await pickSessions({ title: `Send “${s.name}”`, confirmLabel: 'Send' })
    if (ids?.length) await runSnippetServer(s, ids)
    return
  }
  await sendSnippet(s, resolveTargets(where))
}
