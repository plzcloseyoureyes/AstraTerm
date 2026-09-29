/*
 * "Is the shell at a prompt?" for "Open terminal here" (RESEARCH FILE-2: the manual `cd` is only sent when the shell
 * waits at a prompt — never into vim, less, a running command or a half-typed line). A tiny terminal plugin keeps the
 * xterm instances of each session plus two facts the screen alone cannot tell: whether the user has typed on the
 * current line (not submitted yet) and when output last arrived. Checks:
 *   busy     the alternate screen (full-screen programs), a half-typed line, an empty cursor line, text after the cursor
 *   prompt   the cursor line ends like a prompt ("…$ ", "…# ", "…% ", "…> ", "❯ "), or — themed prompts — any cursor
 *            line nobody typed on while the output has been quiet for a moment
 */
import type { Terminal } from '@xterm/xterm'
import { registerTerminalPlugin, type TerminalPluginContext } from '@/app/registry'
import { cursorLineState, endsLine } from './promptLine'

interface Probe {
  term: Terminal
  ctx: TerminalPluginContext
  /** The user typed on the current line and did not submit / discard it yet. */
  lineOpen: boolean
  lastOutputAt: number
}

const probes = new Set<Probe>()

/** Output quiet for this long counts as "the shell waits" for themed prompts. */
const QUIET_MS = 600

let installed = false

export function installTerminalProbe(): void {
  if (installed) return
  installed = true
  registerTerminalPlugin({
    id: 'files.promptProbe',
    order: 900,
    setup: (term, ctx) => {
      const probe: Probe = { term, ctx, lineOpen: false, lastOutputAt: Date.now() }
      probes.add(probe)
      const offInput = ctx.onInput((data) => {
        if (endsLine(data)) probe.lineOpen = false
        else if (data.length) probe.lineOpen = true // printable text, history recall (↑), paste…
      })
      const offOutput = ctx.onOutput(() => {
        probe.lastOutputAt = Date.now()
      })
      return () => {
        offInput()
        offOutput()
        probes.delete(probe)
      }
    },
  })
}

export type ShellReadiness = 'prompt' | 'busy' | 'unknown'

function readiness(p: Probe): ShellReadiness {
  const buf = p.term.buffer.active
  if (buf.type === 'alternate' || p.lineOpen) return 'busy'
  const line = buf.getLine(buf.baseY + buf.cursorY)
  if (!line) return 'unknown'
  const text = line.translateToString(false)
  const state = cursorLineState(text.slice(0, buf.cursorX), text.slice(buf.cursorX))
  if (state === 'maybe') return Date.now() - p.lastOutputAt >= QUIET_MS ? 'prompt' : 'busy'
  return state
}

/** Readiness of a session's shell, judged on the terminal(s) showing it ('unknown' when none is open here). */
export function shellReadiness(sessionId: string): ShellReadiness {
  let result: ShellReadiness = 'unknown'
  for (const p of probes) {
    if ((p.ctx.session()?.id ?? p.ctx.sessionId) !== sessionId) continue
    const r = readiness(p)
    if (r === 'busy') return 'busy'
    if (r === 'prompt') result = 'prompt'
  }
  return result
}
