/*
 * Getting AI suggestions into a terminal — never automatically:
 *   Insert  types the command at the prompt WITHOUT Enter (through the terminal's paste-safety pipeline).
 *   Run     always asks first (RunDialog), shows the risk rating and the automation module's dangerous-command guard
 *           verdict (POST /api/automation/guard/check, when that module is installed), then sends it with Enter to
 *           that one terminal only.
 */
import { create } from 'zustand'
import { toast } from 'sonner'
import { api, isApiError } from '@/api/client'
import { confirm } from '@/components/ui/dialog-host'
import { getActiveTerminal, getTerminalByTab, getTerminalsBySession } from '@/features/terminal/bus'
import type { TerminalHandle } from '@/features/terminal/types'
import { cleanCommand } from './heuristics'
import type { Risk } from './types'

export interface RunTarget {
  tabId?: string
  sessionId?: string
}

export interface GuardMatch {
  rule: string
  message: string
  severity: 'danger' | 'warning' | string
  /** Offending line (text) or line number, depending on the guard version. */
  line?: string | number
}

/** Whether a paste reaches this terminal's shell as bracketed paste (typed, not executed line by line). */
export function bracketedPasteActive(h: TerminalHandle): boolean {
  const modes = h.term.modes as { bracketedPasteMode?: boolean }
  const opts = h.term.options as { ignoreBracketedPasteMode?: boolean }
  return !!modes.bracketedPasteMode && !opts.ignoreBracketedPasteMode
}

export function resolveTerminal(target?: RunTarget): TerminalHandle | undefined {
  if (target?.tabId) {
    const h = getTerminalByTab(target.tabId)
    if (h) return h
  }
  if (target?.sessionId) {
    const h = getTerminalsBySession(target.sessionId)[0]
    if (h) return h
  }
  return getActiveTerminal()
}

function usable(h: TerminalHandle | undefined): h is TerminalHandle {
  if (!h) {
    toast.info('Open a terminal first', { description: 'Commands are inserted into the active terminal tab.' })
    return false
  }
  if (h.info().readOnly) {
    toast.info('This terminal is read-only')
    return false
  }
  if (!h.isRunning()) {
    toast.info('The session is not connected', { description: 'Reconnect it, then try again.' })
    return false
  }
  return true
}

export { cleanCommand } from './heuristics'

/**
 * Type the command at the prompt without pressing Enter. A multi-line command can only be typed without running it
 * when the shell accepts bracketed paste; otherwise every line but the last would execute immediately, so the user is
 * asked first (Insert must never run anything by surprise).
 */
export async function insertCommand(cmd: string, target?: RunTarget): Promise<boolean> {
  const h = resolveTerminal(target)
  if (!usable(h)) return false
  const text = cleanCommand(cmd)
  if (!text.trim()) return false
  const lines = text.split('\n').length
  const runsLines = lines > 1 && !bracketedPasteActive(h)
  if (runsLines) {
    const ok = await confirm({
      title: 'Insert would run this command',
      description: `This shell does not support bracketed paste, so ${lines - 1 === 1 ? 'the first line' : `the first ${lines - 1} lines`} would run as soon as they are typed. Use Run… to review and run it, or Copy to paste it yourself.`,
      confirmLabel: 'Insert and run',
      destructive: true,
    })
    if (!ok) {
      h.focus()
      return false
    }
  }
  // Already confirmed above when lines run: no second paste dialog (the paste pipeline still sanitises).
  const ok = await h.paste(text, { skipConfirm: runsLines })
  if (ok) h.focus()
  return ok
}

// ---- run confirmation ---------------------------------------------------------------------------------------------

interface RunRequest {
  command: string
  risk?: Risk
  riskReason?: string
  target: RunTarget
  title: string
  resolve: (ok: boolean) => void
}

interface RunStore {
  req: RunRequest | null
  guard: { loading: boolean; enabled: boolean; matches: GuardMatch[]; unavailable?: boolean }
}

export const useRunStore = create<RunStore>(() => ({ req: null, guard: { loading: false, enabled: false, matches: [] } }))

async function checkGuard(text: string): Promise<void> {
  useRunStore.setState({ guard: { loading: true, enabled: false, matches: [] } })
  const ctrl = new AbortController()
  const timer = setTimeout(() => ctrl.abort(), 5000)
  try {
    const r = await api.post<{ enabled: boolean; matches: GuardMatch[] | null }>('/api/automation/guard/check', { text }, { noVaultPrompt: true, signal: ctrl.signal })
    useRunStore.setState({ guard: { loading: false, enabled: !!r?.enabled, matches: r?.matches ?? [] } })
  } catch (err) {
    // The automation module may not be installed (404) — the confirmation still applies.
    useRunStore.setState({ guard: { loading: false, enabled: false, matches: [], unavailable: !isApiError(err) || err.status !== 404 } })
  } finally {
    clearTimeout(timer)
  }
}

/** Ask for confirmation, then run the command in the target terminal. Resolves true when it was sent. */
export function requestRun(command: string, opts: { risk?: Risk; riskReason?: string; target?: RunTarget } = {}): Promise<boolean> {
  const h = resolveTerminal(opts.target)
  if (!usable(h)) return Promise.resolve(false)
  const text = cleanCommand(command)
  if (!text.trim()) return Promise.resolve(false)
  const prev = useRunStore.getState().req
  prev?.resolve(false)
  return new Promise<boolean>((resolve) => {
    useRunStore.setState({
      req: {
        command: text,
        risk: opts.risk,
        riskReason: opts.riskReason,
        target: { tabId: h.tabId, sessionId: h.sessionId },
        title: h.info().title || h.session()?.title || 'Terminal',
        resolve,
      },
    })
    void checkGuard(text)
  })
}

/** Called by RunDialog. */
export function finishRun(ok: boolean): void {
  const req = useRunStore.getState().req
  if (!req) return
  useRunStore.setState({ req: null })
  if (!ok) {
    req.resolve(false)
    resolveTerminal(req.target)?.focus()
    return
  }
  const h = resolveTerminal(req.target)
  if (!usable(h)) {
    req.resolve(false)
    return
  }
  const multi = req.command.includes('\n')
  const bracketed = multi && bracketedPasteActive(h)
  const body = bracketed ? `\x1b[200~${req.command}\x1b[201~` : req.command.replace(/\n/g, '\r')
  // Raw send to this terminal only (no broadcast fan-out): the user confirmed exactly this target.
  const sent = h.send(`${body}\r`)
  if (!sent) toast.error('The command could not be sent', { description: 'The terminal connection is not open.' })
  h.focus()
  req.resolve(sent)
}
