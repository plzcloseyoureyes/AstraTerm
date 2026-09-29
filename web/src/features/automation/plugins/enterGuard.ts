/*
 * Dangerous-command guard for typed input (SEC-21): when Enter is pressed in a terminal that broadcasts to other
 * terminals — or, per settings, on a production-tagged host / everywhere — the command line on the
 * screen is checked and a dangerous command needs confirmation before Enter reaches the shell(s). The command text
 * comes from the screen: from the OSC 133;B mark when the shell has integration, else the whole cursor line (the
 * rules are written so a prompt in front does not matter). Best effort, like the paste checks.
 */
import type { Terminal } from '@xterm/xterm'
import type { TerminalPluginContext } from '@/app/registry'
import { isParticipant, multiExecTargets } from '@/features/terminal/multiexec'
import type { TerminalPluginContextEx } from '@/features/terminal/types'
import { checkDangerous, isProduction } from '../guard'
import { cachedConnection } from '../send'
import { automationSettings } from '../settings'
import { askDangerous } from '../store'

/** The command line at the cursor (joined wrapped rows), starting at the OSC 133;B column when known. */
export function commandAtCursor(term: Terminal, commandStart: { row: number; col: number } | null): string[] {
  const buf = term.buffer.active
  let row = buf.baseY + buf.cursorY
  const rows: string[] = []
  let first = row
  for (;;) {
    const line = buf.getLine(row)
    if (!line) break
    rows.unshift(line.translateToString(row !== buf.baseY + buf.cursorY))
    first = row
    if (!line.isWrapped || row === 0) break
    row--
  }
  let text = rows.join('')
  const candidates: string[] = []
  if (commandStart && commandStart.row >= first && commandStart.row <= buf.baseY + buf.cursorY) {
    const offset = (commandStart.row - first) * term.cols + commandStart.col
    candidates.push(text.slice(offset))
  }
  text = text.replace(/\s+$/, '')
  candidates.push(text)
  // Network devices ("router#reload") and shells without a space after the prompt: also try after the prompt.
  const m = /^.{0,64}?[#>$%]\s*/.exec(text)
  if (m && m[0].length < text.length) candidates.push(text.slice(m[0].length))
  return candidates
}

/**
 * Apply typed input to a line buffer (what the user typed since the last Enter): printable text appends, Backspace
 * removes, Ctrl-U / Ctrl-C clear, Enter submits (clears); history / cursor keys make the buffer unreliable (cleared —
 * the screen line is checked as well).
 */
export function applyTyped(buf: string, data: string): string {
  const clean = data.replace(/\x1b\[20[01]~/g, '') // oxlint-disable-line no-control-regex
  let out = buf
  for (let i = 0; i < clean.length; i++) {
    const c = clean[i]
    const code = c.charCodeAt(0)
    if (c === '\r' || c === '\n' || code === 0x15 || code === 0x03) out = ''
    else if (code === 0x7f || code === 0x08) out = out.slice(0, -1)
    else if (code === 0x1b) {
      // Skip the escape sequence; arrows / history recall change the line in ways we cannot follow.
      out = ''
      if (clean[i + 1] === '[' || clean[i + 1] === 'O') {
        i += 2
        while (i < clean.length && !(clean[i] >= '@' && clean[i] <= '~')) i++
      } else i++
    } else if (c === '\t') out += ' '
    else if (code >= 0x20) out += c
  }
  return out.length > 8192 ? out.slice(-8192) : out
}

export function setupEnterGuard(term: Terminal, ctx: TerminalPluginContext): () => void {
  const ex = ctx as TerminalPluginContext & Partial<TerminalPluginContextEx>
  let commandStart: { row: number; col: number } | null = null
  let blockedKeypress = false
  let typed = ''
  const offInput = ctx.onInput((data) => {
    typed = applyTyped(typed, data)
  })

  const osc = term.parser.registerOscHandler(133, (data) => {
    if (data.startsWith('B')) {
      const buf = term.buffer.active
      commandStart = { row: buf.baseY + buf.cursorY, col: buf.cursorX }
    } else if (data.startsWith('A') || data.startsWith('D')) {
      commandStart = null
    }
    return false // let the terminal's own handler see it too
  })

  const applies = (): { broadcast: boolean } | null => {
    const s = automationSettings.get()
    if (!s.guardEnabled) return null
    const targets = isParticipant(ctx.tabId) ? multiExecTargets() : []
    const broadcast = targets.length > 1 || (targets.length === 1 && targets[0].tabId !== ctx.tabId)
    if (broadcast) return { broadcast }
    if (s.guardTyped === 'always') return { broadcast }
    if (s.guardTyped === 'production' && isProduction(cachedConnection(ctx.session()?.connectionId))) return { broadcast }
    return null
  }

  const onKeyDown = (e: KeyboardEvent) => {
    if (e.key !== 'Enter' || e.shiftKey || e.altKey || e.ctrlKey || e.metaKey || e.isComposing || e.repeat) return
    const info = ex.handle?.info()
    if (info && (info.readOnly || info.state !== 'connected')) return
    if (term.buffer.active.type !== 'normal') return
    const mode = applies()
    if (!mode) return
    const seen = new Set<string>()
    const matches = []
    // The screen line (history recall, completion) and what was typed (its echo may not have arrived yet).
    for (const c of [...commandAtCursor(term, commandStart), typed]) {
      for (const m of checkDangerous(c)) {
        const key = m.rule + m.message
        if (!seen.has(key)) {
          seen.add(key)
          matches.push(m)
        }
      }
    }
    if (!matches.length) return
    e.preventDefault()
    e.stopPropagation()
    blockedKeypress = true
    const targets = mode.broadcast ? multiExecTargets().map((h) => h.info().title) : [info?.title || ctx.session()?.title || 'this session']
    void askDangerous(matches, targets).then((ok) => {
      if (ok) ex.handle?.input('\r')
      ex.handle?.focus()
    })
  }
  const onKeyPress = (e: KeyboardEvent) => {
    if (blockedKeypress && e.key === 'Enter') {
      e.preventDefault()
      e.stopPropagation()
    }
    blockedKeypress = false
  }

  // Listen on the xterm element (capture phase, before xterm's textarea handler). Follow the element when the tab
  // moves to another window (the terminal re-opens there).
  let el: HTMLElement | undefined
  const detach = () => {
    el?.removeEventListener('keydown', onKeyDown, true)
    el?.removeEventListener('keypress', onKeyPress, true)
    el = undefined
  }
  const ensure = () => {
    if (term.element === el) return
    detach()
    el = term.element
    el?.addEventListener('keydown', onKeyDown, true)
    el?.addEventListener('keypress', onKeyPress, true)
  }
  ensure()
  const render = term.onRender(ensure)
  return () => {
    render.dispose()
    osc.dispose()
    offInput()
    detach()
  }
}
