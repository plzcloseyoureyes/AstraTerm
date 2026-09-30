/*
 * Context capture for AI requests: terminal output (rendered xterm lines, else the server ring buffer), the terminal
 * selection, an editor file, a failed command, and facts about the target host (session + monitor host probe).
 * Everything is redacted again server-side before it reaches the provider.
 */
import type { Terminal } from '@xterm/xterm'
import { api, seg } from '@/api/client'
import { getScrollback } from '@/api/sessions'
import { getTerminalByTab, getTerminalsBySession } from '@/features/terminal/bus'
import type { TerminalHandle } from '@/features/terminal/types'
import { listTabs } from '@/stores/workspace'
import { uid } from '@/lib/utils'
import { aiSettings } from './settings'
import type { ChatContextPayload, ContextChip, SessionInfo } from './types'

/** Rendered text of the last `lines` lines of a terminal (up to the cursor), trailing blank lines removed. */
function terminalTail(term: Terminal, lines: number): string {
  const buf = term.buffer.active
  const end = buf.baseY + buf.cursorY
  // Start at the beginning of a wrapped line: a secret cut in half at the range start would escape exact-value
  // redaction on the server.
  const start = unwrapStart(term, Math.max(0, end - lines + 1))
  const out: string[] = []
  for (let y = start; y <= end; y++) {
    const line = buf.getLine(y)
    if (!line) continue
    const text = line.translateToString(true)
    if (line.isWrapped && out.length) out[out.length - 1] += text
    else out.push(text)
  }
  while (out.length && !out[out.length - 1].trim()) out.pop()
  return out.join('\n')
}

/** First row of the logical (possibly wrapped) line that contains row y. */
function unwrapStart(term: Terminal, y: number): number {
  const buf = term.buffer.active
  let row = y
  while (row > 0 && y - row < 200 && buf.getLine(row)?.isWrapped) row--
  return row
}

/** Text of buffer lines [from, to] (inclusive), joining wrapped lines. */
export function bufferText(term: Terminal, from: number, to: number, fromX = 0): string {
  const buf = term.buffer.active
  if (fromX === 0) from = unwrapStart(term, Math.max(0, from))
  const out: string[] = []
  for (let y = Math.max(0, from); y <= Math.min(to, buf.length - 1); y++) {
    const line = buf.getLine(y)
    if (!line) continue
    const text = line.translateToString(true, y === from ? fromX : 0)
    if (line.isWrapped && out.length) out[out.length - 1] += text
    else out.push(text)
  }
  while (out.length && !out[out.length - 1].trim()) out.pop()
  return out.join('\n')
}

function lastLines(text: string, n: number): string {
  const lines = text.replace(/\r\n?/g, '\n').split('\n')
  while (lines.length && !lines[lines.length - 1].trim()) lines.pop()
  return lines.slice(-n).join('\n')
}

// ---- host facts ------------------------------------------------------------------------------------------------

interface HostFacts {
  os?: string
  kernel?: string
  platform?: string
  arch?: string
  hostname?: string
}

const hostCache = new Map<string, { at: number; value: Promise<HostFacts | null> }>()

/** OS facts from the monitor module's host probe (when installed and the session is SSH / local). Cached. */
function hostFacts(sessionId: string): Promise<HostFacts | null> {
  const hit = hostCache.get(sessionId)
  if (hit && Date.now() - hit.at < 10 * 60_000) return hit.value
  const ctrl = new AbortController()
  const timer = setTimeout(() => ctrl.abort(), 4000)
  const value = api
    .get<HostFacts>(`/api/monitor/${seg(sessionId)}/host`, { signal: ctrl.signal, noVaultPrompt: true })
    .then((h) => (h && typeof h === 'object' ? h : null))
    .catch(() => null)
    .finally(() => clearTimeout(timer))
  hostCache.set(sessionId, { at: Date.now(), value })
  if (hostCache.size > 100) hostCache.delete(hostCache.keys().next().value!)
  return value
}

function handleFor(tabId?: string, sessionId?: string): TerminalHandle | undefined {
  return getTerminalByTab(tabId) ?? (sessionId ? getTerminalsBySession(sessionId)[0] : undefined)
}

/** Describe the target host of a terminal. */
async function sessionInfoFor(tabId?: string, sessionId?: string): Promise<SessionInfo | undefined> {
  const h = handleFor(tabId, sessionId)
  const sid = sessionId ?? h?.sessionId
  if (!h && !sid) return undefined
  const s = h?.session()
  const info: SessionInfo = {
    title: h?.info().title || s?.title,
    protocol: s?.protocol,
    host: s?.host,
    username: s?.username,
    cwd: h?.info().cwd || s?.cwd,
  }
  if (sid && (s?.protocol === 'ssh' || s?.protocol === 'local' || !s)) {
    const facts = await hostFacts(sid)
    if (facts) {
      info.os = facts.os
      info.kernel = facts.kernel
      info.platform = facts.platform
      info.arch = facts.arch
      if (!info.host && facts.hostname) info.host = facts.hostname
    }
  }
  if (info.platform === 'windows' && !info.shell) info.shell = 'powershell'
  return info
}

// ---- chips -----------------------------------------------------------------------------------------------------

export function terminalChip(handle: TerminalHandle, lines = aiSettings.get().terminalLines): ContextChip {
  const title = handle.info().title || handle.session()?.title || 'Terminal'
  return { kind: 'terminal', id: uid('k'), tabId: handle.tabId, sessionId: handle.sessionId, label: title, lines }
}

export function selectionChip(text: string, handle?: TerminalHandle): ContextChip {
  const n = text.split('\n').length
  return {
    kind: 'selection',
    id: uid('k'),
    label: n > 1 ? `Selection · ${n} lines` : `Selection · ${text.trim().slice(0, 24)}${text.trim().length > 24 ? '…' : ''}`,
    text,
    tabId: handle?.tabId,
    sessionId: handle?.sessionId,
  }
}

export function failureChip(opts: { command: string; exitCode?: number; output: string; tabId?: string; sessionId?: string }): ContextChip {
  const cmd = opts.command.trim().replace(/\s+/g, ' ')
  return {
    kind: 'failure',
    id: uid('k'),
    label: `${cmd.length > 28 ? `${cmd.slice(0, 27)}…` : cmd || 'Command'}${opts.exitCode != null ? ` · exit ${opts.exitCode}` : ''}`,
    ...opts,
  }
}

/** Open editor tabs that can be attached as file context. */
export function editorTabs(): { tabId: string; fsId: string; path: string; title: string }[] {
  return listTabs()
    .filter((t) => t.kind === 'editor' && t.params && typeof t.params.fsId === 'string' && typeof t.params.path === 'string')
    .map((t) => ({ tabId: t.id, fsId: t.params.fsId as string, path: t.params.path as string, title: t.title }))
}

const LANG_BY_EXT: Record<string, string> = {
  sh: 'bash', bash: 'bash', zsh: 'bash', py: 'python', js: 'javascript', ts: 'typescript', go: 'go', rs: 'rust', rb: 'ruby',
  yml: 'yaml', yaml: 'yaml', json: 'json', toml: 'toml', ini: 'ini', conf: 'conf', cfg: 'conf', ps1: 'powershell', sql: 'sql',
  md: 'markdown', xml: 'xml', html: 'html', css: 'css', php: 'php', java: 'java', c: 'c', h: 'c', cpp: 'cpp', tf: 'hcl',
  service: 'ini', dockerfile: 'dockerfile', nginx: 'nginx',
}

function languageOf(path: string): string | undefined {
  const base = path.split('/').pop()?.toLowerCase() ?? ''
  if (base === 'dockerfile') return 'dockerfile'
  const ext = base.includes('.') ? base.split('.').pop()! : ''
  return LANG_BY_EXT[ext]
}

/** Read a remote/local file through the files API (for editor context). */
export async function fileChip(fsId: string, path: string): Promise<ContextChip> {
  const r = await api.get<{ content: string; encoding: 'utf-8' | 'base64'; size: number }>(`/api/fs/${seg(fsId)}/read`, {
    query: { path, maxBytes: 200_000 },
  })
  if (r.encoding !== 'utf-8') throw new Error('This file is binary and cannot be attached')
  const name = path.split('/').pop() || path
  return { kind: 'file', id: uid('k'), label: name, path, fsId, content: r.content, language: languageOf(path) }
}

export function chipLabelList(chips: ContextChip[]): { kind: ContextChip['kind']; label: string }[] | undefined {
  return chips.length ? chips.map((c) => ({ kind: c.kind, label: c.label })) : undefined
}

/** Build the request context from the composer chips (captures live terminal output at send time). */
export async function buildContext(chips: ContextChip[]): Promise<ChatContextPayload | undefined> {
  if (!chips.length) return undefined
  const ctx: ChatContextPayload = {}
  let target: { tabId?: string; sessionId?: string } | undefined
  for (const chip of chips) {
    switch (chip.kind) {
      case 'terminal': {
        const h = handleFor(chip.tabId, chip.sessionId)
        let text = ''
        if (h) text = terminalTail(h.term, chip.lines)
        else {
          try {
            text = lastLines(await getScrollback(chip.sessionId, false), chip.lines)
          } catch {
            /* session gone */
          }
        }
        if (text) ctx.terminalText = text
        target ??= { tabId: chip.tabId, sessionId: chip.sessionId }
        break
      }
      case 'selection':
        ctx.selection = ctx.selection ? `${ctx.selection}\n\n${chip.text}` : chip.text
        if (chip.sessionId) target ??= { tabId: chip.tabId, sessionId: chip.sessionId }
        break
      case 'file':
        if (chip.content) ctx.file = { path: chip.path, language: chip.language, content: chip.content }
        break
      case 'failure':
        ctx.command = chip.command
        if (chip.exitCode != null) ctx.exitCode = chip.exitCode
        ctx.selection = ctx.selection ? `${chip.output}\n\n${ctx.selection}` : chip.output
        if (chip.sessionId) target = { tabId: chip.tabId, sessionId: chip.sessionId }
        break
    }
  }
  if (target?.sessionId) {
    ctx.sessionId = target.sessionId
    ctx.sessionInfo = await sessionInfoFor(target.tabId, target.sessionId)
  }
  return ctx
}
