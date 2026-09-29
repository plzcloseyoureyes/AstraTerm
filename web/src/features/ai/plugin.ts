/*
 * Terminal plugin "ai.assist" (TOOL-11, TOOL-12):
 *   - Ctrl+I (Cmd+I on macOS) opens the command bar for this terminal. Plain Ctrl+<key> belongs to the shell in
 *     NexTerm, so the plugin claims the key itself — only while the assistant is available (else Ctrl+I stays Tab).
 *   - `# find files over 1GB` + Enter at a prompt: the shell treats the line as a comment (harmless) and the command
 *     bar opens with a suggestion for it.
 *   - Failed commands get a small, calm "Explain · Fix" chip next to their output: exit codes from OSC 133 shell
 *     integration when the shell reports them, else recognisable error output after a typed command.
 */
import type { IDecoration, IDisposable, IMarker, Terminal } from '@xterm/xterm'
import { matchKeybindingPress, parseKeybinding } from 'tinykeys'
import type { TerminalPluginContext } from '@/app/registry'
import { getKeybindings } from '@/app/commands'
import type { TerminalPluginContextEx } from '@/features/terminal/types'
import { isMac } from '@/lib/utils'
import { explainFailure } from './actions'
import { openCommandBar } from './barStore'
import { bufferText } from './context'
import { aiSettings } from './settings'
import { isAiAvailable } from './store'
import { isShellPromptPrefix, looksLikeSecretEntry } from './heuristics'

export const COMMAND_BAR_COMMAND = 'ai.commandBar'

const ERROR_RE =
  /(command not found|: not found$|No such file or directory|Permission denied|Operation not permitted|is not recognized as|not recognized as the name of a cmdlet|syntax error|Segmentation fault|core dumped|Traceback \(most recent call last\)|^(?:error|fatal|ERROR|FATAL|E)(?:\[[^\]]*\])?:|^Error: |Connection refused|Could not resolve host|Unable to locate package|ModuleNotFoundError|npm ERR!|cannot access|Killed$)/m

const HASH_RE = /^\s*#\s*(\S.{2,})$/


/** Command text of a prompt line: everything after the first prompt character. */
function stripPrompt(line: string): string {
  const m = /^.{0,120}?[$#%>❯»➜λ→]\s+(.*)$/.exec(line)
  if (m) return m[1].trim()
  // Decorative prompts (box drawing, arrows, bullets): drop the leading symbols.
  return line.replace(/^[^\p{L}\p{N}./~"'_-]+/u, '').trim()
}

function lineText(term: Terminal, y: number): string {
  return term.buffer.active.getLine(y)?.translateToString(true) ?? ''
}

export function setupAiTerminal(term: Terminal, ctx: TerminalPluginContext): () => void {
  const ex = ctx as TerminalPluginContext & Partial<TerminalPluginContextEx>
  const disposables: IDisposable[] = []
  const cleanups: (() => void)[] = []
  const replaying = () => !!ex.isReplaying?.()
  const alt = () => term.buffer.active.type === 'alternate'

  // --- keyboard: Ctrl+I in the terminal (non-mac; on macOS Cmd+I reaches the global dispatcher) ------------------
  let keyTarget: HTMLElement | undefined
  const onKey = (e: KeyboardEvent) => {
    if (e.type !== 'keydown' || isMac || !e.ctrlKey || e.altKey || e.metaKey || e.shiftKey) return
    if (!isAiAvailable()) return
    const hit = getKeybindings(COMMAND_BAR_COMMAND).some((b) => {
      try {
        return parseKeybinding(b).length === 1 && matchKeybindingPress(e, parseKeybinding(b)[0])
      } catch {
        return false
      }
    })
    if (!hit) return
    e.preventDefault()
    e.stopPropagation()
    openCommandBar({ tabId: ctx.tabId, sessionId: ctx.sessionId })
  }
  const attachKeys = () => {
    const el = term.element
    if (!el || keyTarget === el) return
    keyTarget?.removeEventListener('keydown', onKey, true)
    keyTarget = el
    el.addEventListener('keydown', onKey, true)
  }
  attachKeys()
  const keyTimer = setInterval(attachKeys, 1000) // term.element exists only after open()
  cleanups.push(() => {
    clearInterval(keyTimer)
    keyTarget?.removeEventListener('keydown', onKey, true)
  })

  // --- failure chip ------------------------------------------------------------------------------------------------
  let chip: { deco: IDecoration; marker: IMarker } | null = null
  const clearChip = () => {
    if (!chip) return
    chip.deco.dispose()
    chip.marker.dispose()
    chip = null
  }
  cleanups.push(clearChip)

  const linesAbove = (y: number, n = 4) => {
    const out: string[] = []
    for (let i = Math.max(0, y - n); i < y; i++) out.push(lineText(term, i))
    return out
  }

  const showChip = (line: number, failure: { command: string; exitCode?: number; output: string }, commandLine: number) => {
    if (!aiSettings.get().offerOnError || !isAiAvailable() || replaying() || alt()) return
    if (looksLikeSecretEntry(failure.command, linesAbove(commandLine), failure.exitCode, failure.output)) return
    clearChip()
    const buf = term.buffer.active
    const offset = line - (buf.baseY + buf.cursorY)
    let marker: IMarker | undefined
    try {
      marker = term.registerMarker(offset)
    } catch {
      return
    }
    if (!marker) return
    // Right after the line's text (close to what it is about); right-aligned at the edge when the line is long.
    const textLen = lineText(term, line).replace(/\s+$/, '').length
    const inline = textLen + 18 < term.cols
    const x = inline ? textLen + 2 : Math.max(0, term.cols - 1)
    const deco = term.registerDecoration({ marker, anchor: 'left', x, width: 1, layer: 'top' })
    if (!deco) {
      marker.dispose()
      return
    }
    chip = { deco, marker }
    const payload = { ...failure, tabId: ctx.tabId, sessionId: ctx.sessionId }
    deco.onRender((el) => {
      if (el.dataset.nxAi) return
      el.dataset.nxAi = '1'
      el.style.overflow = 'visible'
      el.style.pointerEvents = 'none'
      const wrap = el.ownerDocument.createElement('div')
      wrap.className =
        `pointer-events-auto absolute top-1/2 ${inline ? 'left-0' : 'right-0'} flex -translate-y-1/2 items-center overflow-hidden rounded-full border border-border/70 whitespace-nowrap ` +
        'bg-popover/95 font-sans text-2xs text-muted-foreground shadow-sm opacity-0 transition-opacity duration-200'
      const mk = (label: string, title: string, fix: boolean) => {
        const b = el.ownerDocument.createElement('button')
        b.type = 'button'
        b.textContent = label
        b.title = title
        b.className = 'px-2 py-0.5 leading-4 hover:bg-accent hover:text-foreground focus-visible:outline-none focus-visible:bg-accent'
        b.addEventListener('mousedown', (ev) => ev.stopPropagation())
        b.addEventListener('click', (ev) => {
          ev.stopPropagation()
          clearChip()
          explainFailure(payload, fix)
        })
        return b
      }
      const sep = el.ownerDocument.createElement('span')
      sep.className = 'h-3 w-px bg-border'
      wrap.append(
        mk(`✦ Explain${failure.exitCode != null ? ` (exit ${failure.exitCode})` : ''}`, 'Explain this failure with AI', false),
        sep,
        mk('Fix', 'Suggest a fix with AI', true),
      )
      el.appendChild(wrap)
      requestAnimationFrame(() => wrap.classList.replace('opacity-0', 'opacity-100'))
    })
    // The chip is only relevant until the next command.
    marker.onDispose(() => {
      if (chip?.marker === marker) chip = null
    })
  }

  // --- OSC 133 shell integration -----------------------------------------------------------------------------------
  let osc133 = false
  /** OSC 133: a prompt is showing (between B "command start" and C "command executed"). */
  let atPrompt = false
  let sawB = false
  let cmdLine = -1
  let cmdStart: { marker: IMarker; x: number } | null = null
  let outStart: IMarker | null = null
  let lastCommand = ''
  disposables.push(
    term.parser.registerOscHandler(133, (data) => {
      const [kind, ...rest] = data.split(';')
      const buf = term.buffer.active
      if (kind === 'A') {
        osc133 = true
        atPrompt = false
      } else if (kind === 'B') {
        osc133 = true
        sawB = true
        atPrompt = true
        cmdStart?.marker.dispose()
        const m = term.registerMarker(0)
        cmdStart = m ? { marker: m, x: buf.cursorX } : null
      } else if (kind === 'C') {
        osc133 = true
        atPrompt = false
        clearChip()
        const cur = buf.baseY + buf.cursorY
        cmdLine = -1
        if (cmdStart && !cmdStart.marker.isDisposed && cmdStart.marker.line >= 0) {
          cmdLine = cmdStart.marker.line
          const end = buf.cursorX === 0 ? Math.max(cmdStart.marker.line, cur - 1) : cur
          lastCommand = bufferText(term, cmdStart.marker.line, end, cmdStart.x).trim()
        }
        outStart?.dispose()
        outStart = term.registerMarker(0) ?? null
      } else if (kind === 'D') {
        osc133 = true
        const code = Number.parseInt(rest[0] ?? '', 10)
        const cur = buf.baseY + buf.cursorY
        const from = outStart && !outStart.isDisposed && outStart.line >= 0 ? outStart.line : Math.max(0, cur - 30)
        const last = buf.cursorX === 0 ? cur - 1 : cur
        if (Number.isFinite(code) && code !== 0 && code !== 130 && lastCommand) {
          const output = bufferText(term, Math.max(from, last - 80), last)
          showChip(Math.max(from, last), { command: lastCommand, exitCode: code, output }, cmdLine >= 0 ? cmdLine : from)
        }
        outStart?.dispose()
        outStart = null
        lastCommand = ''
      }
      return false // let the terminal's own OSC 133 handling run too
    }),
  )

  // --- typed input: `#` trigger + heuristic error detection ------------------------------------------------------
  /**
   * The `# request` was typed at a shell prompt (not in a heredoc, REPL or editor that treats `#` as a comment). The
   * text in front of the cursor when the user started typing the line is the prompt (the echo of fast typing or a
   * paste may still be on its way when Enter is pressed, so the line is not read at Enter time).
   */
  const atShellPrompt = (): boolean => {
    if (sawB) return atPrompt
    return promptText !== null && isShellPromptPrefix(promptText)
  }
  /** Text left of the cursor when the current input line started ('' typed → first character). */
  let promptText: string | null = null
  const capturePrompt = () => {
    const buf = term.buffer.active
    // Only the end of the prompt matters ("$ ", "% ", "> "…): the row up to the cursor.
    promptText = buf.getLine(buf.baseY + buf.cursorY)?.translateToString(false, 0, buf.cursorX) ?? null
  }

  let line: string | null = ''
  let pending: { startLine: number; command: string; at: number } | null = null
  let quietTimer: ReturnType<typeof setTimeout> | null = null
  const checkPending = () => {
    quietTimer = null
    const p = pending
    if (!p || osc133 || alt()) return
    if (Date.now() - p.at > 30_000) {
      pending = null
      return
    }
    const buf = term.buffer.active
    const cur = buf.baseY + buf.cursorY
    if (cur <= p.startLine) return // nothing printed yet
    pending = null
    // Anchor the chip on the last line that looks like an error (within the command's last 15 output lines).
    for (let y = cur - 1; y > p.startLine && y >= cur - 16; y--) {
      if (!ERROR_RE.test(lineText(term, y))) continue
      const output = bufferText(term, Math.max(p.startLine + 1, cur - 80), y)
      showChip(y, { command: p.command, output }, p.startLine)
      return
    }
  }
  cleanups.push(() => {
    if (quietTimer) clearTimeout(quietTimer)
  })

  cleanups.push(
    ctx.onInput((data) => {
      if (data === '\r') {
        const typed = line
        line = ''
        clearChip()
        if (alt()) return
        const buf = term.buffer.active
        const cur = buf.baseY + buf.cursorY
        const m = typed !== null ? HASH_RE.exec(typed) : null
        const atPromptNow = atShellPrompt()
        promptText = null
        if (m && aiSettings.get().hashTrigger && isAiAvailable() && atPromptNow) {
          pending = null
          // Let the shell finish the (comment) line first, then open the bar with the request.
          setTimeout(() => openCommandBar({ tabId: ctx.tabId, sessionId: ctx.sessionId, text: m[1].trim(), submit: true }), 60)
          return
        }
        if (!osc133 && aiSettings.get().errorHeuristics) {
          const command = stripPrompt(lineText(term, cur))
          pending = command ? { startLine: cur, command, at: Date.now() } : null
        }
        return
      }
      if (data.includes('\r') || data.includes('\n')) {
        line = null // pasted multi-line input: unknown line
        pending = null
        return
      }
      if (data === '\x7f' || data === '\b') {
        if (line) line = line.slice(0, -1)
        return
      }
      if (data === '\x15' || data === '\x03') {
        line = ''
        promptText = null
        return
      }
      // eslint-disable-next-line no-control-regex
      if (/[\x00-\x1f]/.test(data)) {
        line = null // cursor keys, completion, history: the line is no longer known
        return
      }
      if (line === '') capturePrompt()
      if (line !== null) line = (line + data).slice(-500)
    }),
  )

  cleanups.push(
    ctx.onOutput(() => {
      if (!pending || osc133) return
      if (quietTimer) clearTimeout(quietTimer)
      quietTimer = setTimeout(checkPending, 450)
    }),
  )

  return () => {
    for (const d of disposables) d.dispose()
    for (const c of cleanups) c()
    cmdStart?.marker.dispose()
    outStart?.dispose()
  }
}
