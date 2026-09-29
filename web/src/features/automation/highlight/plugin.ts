/*
 * Keyword highlighting (TERM-15) as a terminal plugin: non-destructive xterm decorations over the rows in view only.
 * Nothing is written into the stream, so copy, logs, recordings and re-attach stay exact and the cost is bounded by
 * the viewport. Skipped in the alternate screen (full-screen apps). Rules: the enabled built-in sets, the user's
 * custom rules and the "highlight" actions of triggers whose scope matches the terminal's session.
 *
 * Decorations use the renderer's *bottom* layer: xterm applies the selection colours after bottom-layer colours, so
 * selecting highlighted text looks exactly like selecting plain text (top-layer colours would paint over the
 * selection). Only lines that changed are re-decorated (entries are keyed by buffer markers, which move with the
 * text), and scans back off when they get expensive (flooding output), so throughput is never limited by this plugin.
 * Styles: foreground, background and underline. Bold is not possible with decorations (the WebGL renderer draws the
 * buffer's own attributes), so rules offer underline for emphasis instead.
 */
import type { IBufferCell, IDecoration, IMarker, Terminal } from '@xterm/xterm'
import { queryClient } from '@/api/queryClient'
import type { Connection, RuntimeSession } from '@/api/types'
import type { TerminalPluginContext } from '@/app/registry'
import { autoKeys } from '../api'
import { cachedConnection } from '../send'
import { automationSettings } from '../settings'
import type { Trigger } from '../types'
import { compileUserPattern, FALLBACK_ANSI, RULE_SETS, type RuleDef } from './rules'

interface CompiledRule extends RuleDef {
  /** Trigger scope (highlight actions of triggers). */
  scope?: Trigger['scope']
}

// ---------------------------------------------------------------------------------------------------------------------
// shared rule list (rebuilt when settings or triggers change)
// ---------------------------------------------------------------------------------------------------------------------

let compiled: CompiledRule[] = []
let version = 0
const listeners = new Set<() => void>()

function goPatternToJs(p: string): { pattern: string; caseSensitive?: boolean } {
  // Go (RE2) inline flags at the start: (?i) (?s) (?m) (?is)…
  const m = /^\(\?([imsU]+)\)/.exec(p)
  if (!m) return { pattern: p }
  return { pattern: p.slice(m[0].length), caseSensitive: !m[1].includes('i') }
}

function rebuild(): void {
  const s = automationSettings.get()
  const out: CompiledRule[] = []
  const enabledSets = new Set(s.highlightSets)
  // Custom rules first: they win over built-ins on overlapping text.
  for (const r of s.highlightCustom) {
    if (!r.enabled) continue
    const re = compileUserPattern(r.pattern, r.caseSensitive)
    if (re && (r.color || r.background)) out.push({ re, color: r.color, background: r.background, underline: r.underline })
  }
  if (s.highlightTriggers) {
    const triggers = queryClient.getQueryData<Trigger[]>(autoKeys.triggers) ?? []
    for (const t of triggers) {
      if (!t.enabled) continue
      for (const a of t.actions) {
        if (a.type !== 'highlight' || (!a.color && !a.background)) continue
        const { pattern, caseSensitive } = goPatternToJs(t.pattern)
        const re = compileUserPattern(pattern, caseSensitive ?? t.caseSensitive)
        if (re) out.push({ re, color: a.color, background: a.background, underline: a.underline, scope: t.scope })
      }
    }
  }
  for (const set of RULE_SETS) if (enabledSets.has(set.id)) out.push(...set.rules)
  compiled = out
  version++
  for (const l of Array.from(listeners)) l()
}

let installed = false

function ensureRules(): void {
  if (installed) return
  installed = true
  rebuild()
  automationSettings.subscribe(() => rebuild())
  queryClient.getQueryCache().subscribe((e) => {
    const key = e.query.queryKey
    if (key[0] === autoKeys.triggers[0] && key[1] === autoKeys.triggers[1] && e.type === 'updated') rebuild()
  })
}

// Per-tab manual override (command automation.highlight.toggle).
const tabOverride = new Map<string, boolean>()
const tabListeners = new Map<string, () => void>()

export function isHighlightEnabled(tabId: string, connectionId: string | undefined): boolean {
  const o = tabOverride.get(tabId)
  if (o !== undefined) return o
  const s = automationSettings.get()
  if (connectionId && s.highlightConnections[connectionId] !== undefined) return s.highlightConnections[connectionId]
  return s.highlightEnabled
}

export function toggleHighlight(tabId: string, connectionId: string | undefined): boolean {
  const next = !isHighlightEnabled(tabId, connectionId)
  tabOverride.set(tabId, next)
  tabListeners.get(tabId)?.()
  return next
}

// ---------------------------------------------------------------------------------------------------------------------
// plugin
// ---------------------------------------------------------------------------------------------------------------------

interface LineEntry {
  marker: IMarker
  text: string
  version: number
  decorations: IDecoration[]
}

const HEX = /^#[0-9a-fA-F]{6}$/
const MAX_LINE = 4000
const MAX_MATCHES_PER_LINE = 200

function scopeMatches(scope: Trigger['scope'] | undefined, session: RuntimeSession | undefined, conn: Connection | undefined): boolean {
  if (!scope) return true
  if (scope.connectionIds?.length && !(session?.connectionId && scope.connectionIds.includes(session.connectionId))) return false
  if (scope.protocols?.length && !scope.protocols.some((p) => p.toLowerCase() === session?.protocol)) return false
  if (scope.tags?.length) {
    const tags = new Set((conn?.tags ?? []).map((t) => t.toLowerCase()))
    if (!scope.tags.some((t) => tags.has(t.toLowerCase()))) return false
  }
  return true
}

export function setupHighlighter(term: Terminal, ctx: TerminalPluginContext): () => void {
  ensureRules()
  const entries: LineEntry[] = []
  let timer: ReturnType<typeof setTimeout> | null = null
  let lastScan = 0
  /** Minimum gap between scans: 90 ms, growing with the cost of the last scan (≤ 600 ms) so floods stay cheap. */
  let gap = 90
  let disposed = false

  const color = (c: string | undefined): string | undefined => {
    if (!c) return undefined
    if (HEX.test(c)) return c
    const theme = (term.options.theme ?? {}) as Record<string, string | undefined>
    const v = theme[c]
    if (v && HEX.test(v)) return v
    return FALLBACK_ANSI[c]
  }

  const clearAll = () => {
    for (const e of entries) {
      for (const d of e.decorations) d.dispose()
      e.marker.dispose()
    }
    entries.length = 0
  }

  const enabled = () => {
    const session = ctx.session()
    return isHighlightEnabled(ctx.tabId, session?.connectionId)
  }

  /** Logical line text starting at absolute row `start` (following wrapped rows), with a cell map. */
  const readLogical = (start: number): { text: string; cells: { row: number; col: number }[]; end: number } => {
    const buf = term.buffer.active
    let text = ''
    const cells: { row: number; col: number }[] = []
    let row = start
    const cell: IBufferCell | undefined = buf.getNullCell()
    for (;;) {
      const line = buf.getLine(row)
      if (!line) break
      for (let x = 0; x < line.length && text.length < MAX_LINE; x++) {
        const c = line.getCell(x, cell)
        if (!c) break
        const w = c.getWidth()
        if (w === 0) continue
        const chars = c.getChars() || ' '
        for (let k = 0; k < chars.length; k++) cells.push({ row, col: x })
        text += chars
      }
      const next = buf.getLine(row + 1)
      if (!next || !next.isWrapped || text.length >= MAX_LINE) break
      row++
    }
    // Trailing blanks never match anything useful.
    const trimmed = text.replace(/\s+$/, '')
    return { text: trimmed, cells: cells.slice(0, trimmed.length), end: row }
  }

  const decorate = (text: string, cells: { row: number; col: number }[], rules: CompiledRule[]): IDecoration[] => {
    const out: IDecoration[] = []
    if (!text.trim()) return out
    const taken = new Uint8Array(text.length)
    const buf = term.buffer.active
    const cursorAbs = buf.baseY + buf.cursorY
    for (const r of rules) {
      const fg = color(r.color)
      const bg = color(r.background)
      if (!fg && !bg) continue
      r.re.lastIndex = 0
      let m: RegExpExecArray | null
      let n = 0
      while ((m = r.re.exec(text)) && n++ < MAX_MATCHES_PER_LINE) {
        if (!m[0].length) {
          r.re.lastIndex++
          continue
        }
        const from = m.index
        const to = from + m[0].length
        let free = true
        for (let i = from; i < to; i++) if (taken[i]) free = false
        if (!free) continue
        taken.fill(1, from, to)
        // One decoration per row segment of the match.
        let segStart = from
        for (let i = from; i <= to; i++) {
          if (i === to || cells[i].row !== cells[segStart].row) {
            const first = cells[segStart]
            const last = cells[i - 1]
            const marker = term.registerMarker(first.row - cursorAbs)
            if (marker) {
              const d = term.registerDecoration({
                marker,
                x: first.col,
                width: last.col - first.col + 1,
                foregroundColor: fg,
                backgroundColor: bg,
                layer: 'bottom',
              })
              if (d) {
                if (r.underline && fg) {
                  d.onRender((el) => {
                    el.style.boxShadow = `inset 0 -1px 0 ${fg}`
                    el.style.pointerEvents = 'none'
                  })
                }
                d.onDispose(() => marker.dispose())
                out.push(d)
              } else {
                marker.dispose()
              }
            }
            segStart = i
          }
        }
      }
    }
    return out
  }

  const scan = () => {
    timer = null
    if (disposed) return
    const t0 = performance.now()
    try {
      scanNow()
    } finally {
      const cost = performance.now() - t0
      gap = Math.min(600, Math.max(90, cost * 8))
      lastScan = Date.now()
    }
  }

  const scanNow = () => {
    const buf = term.buffer.active
    if (!enabled() || buf.type !== 'normal') {
      if (entries.length) clearAll()
      return
    }
    const el = term.element
    if (!el || !el.isConnected || el.offsetWidth === 0) return
    const session = ctx.session()
    const conn = cachedConnection(session?.connectionId)
    const rules = compiled.filter((r) => scopeMatches(r.scope, session, conn))
    const top = buf.viewportY
    const bottom = top + term.rows - 1
    // Index existing entries by their current line.
    const byLine = new Map<number, LineEntry>()
    for (const e of entries) if (!e.marker.isDisposed) byLine.set(e.marker.line, e)
    const keep = new Set<LineEntry>()
    // Start at the logical line containing the first visible row.
    let row = top
    while (row > 0 && buf.getLine(row)?.isWrapped && top - row < 50) row--
    while (row <= bottom) {
      const { text, cells, end } = readLogical(row)
      const existing = byLine.get(row)
      if (existing && existing.text === text && existing.version === version) {
        keep.add(existing)
      } else {
        if (existing) keep.delete(existing)
        const marker = term.registerMarker(row - (buf.baseY + buf.cursorY))
        if (marker) {
          const e: LineEntry = { marker, text, version, decorations: rules.length ? decorate(text, cells, rules) : [] }
          entries.push(e)
          keep.add(e)
        }
      }
      row = end + 1
    }
    // Drop entries out of view (a little margin keeps small scrolls cheap).
    const margin = term.rows
    for (let i = entries.length - 1; i >= 0; i--) {
      const e = entries[i]
      const line = e.marker.line
      const stale = e.marker.isDisposed || (!keep.has(e) && line >= top && line <= bottom) || line < top - margin || line > bottom + margin
      if (stale) {
        for (const d of e.decorations) d.dispose()
        e.marker.dispose()
        entries.splice(i, 1)
      }
    }
  }

  const schedule = () => {
    if (timer || disposed) return
    const wait = Math.max(16, gap - (Date.now() - lastScan))
    timer = setTimeout(scan, wait)
  }

  const onRules = () => {
    // Rules changed: redraw everything.
    clearAll()
    schedule()
  }
  listeners.add(onRules)
  tabListeners.set(ctx.tabId, onRules)
  const unsubSettings = automationSettings.subscribe(() => schedule())
  const subs = [
    term.onWriteParsed(schedule),
    term.onScroll(schedule),
    term.onResize(onRules),
    term.onRender(schedule),
    term.buffer.onBufferChange(() => {
      clearAll()
      schedule()
    }),
  ]
  schedule()
  return () => {
    disposed = true
    if (timer) clearTimeout(timer)
    listeners.delete(onRules)
    if (tabListeners.get(ctx.tabId) === onRules) tabListeners.delete(ctx.tabId)
    unsubSettings()
    for (const s of subs) s.dispose()
    clearAll()
  }
}
