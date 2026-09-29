/*
 * Inline command bar (TOOL-11): Ctrl/Cmd+I in a terminal (or `# what you want` + Enter at the prompt) → describe the
 * task → a suggested command for that host with an explanation and a risk rating → Insert (typed, no Enter) or Run
 * (always confirmed). Typing a new instruction refines the suggestion (the previous turns are kept).
 */
import { create } from 'zustand'
import { isApiError } from '@/api/client'
import { getActiveTerminal, getTerminalByTab, listTerminals } from '@/features/terminal/bus'
import { activeTab, focusTab } from '@/stores/workspace'
import { errorMessage, storage } from '@/lib/utils'
import { useAuthStore } from '@/stores/auth'
import { describeAiError, streamChat } from './api'
import { buildContext, terminalChip } from './context'
import { insertCommand, requestRun } from './run'
import { aiSettings } from './settings'
import { refreshAiStatus } from './store'
import type { CommandResult, WireMessage } from './types'

type Phase = 'idle' | 'waiting' | 'thinking' | 'done' | 'error'

interface BarState {
  open: boolean
  tabId?: string
  sessionId?: string
  input: string
  /** Input text the current result was generated for (Enter inserts while unchanged, else regenerates). */
  generatedFor: string
  phase: Phase
  result?: CommandResult
  /** Editable command (starts as result.command). */
  command: string
  error?: { title: string; hint?: string }
  turns: WireMessage[]
  historyIndex: number
  redactions: number
  /** Bumped to move focus back to the input. */
  focusTick: number
}

const INITIAL: BarState = {
  open: false,
  input: '',
  generatedFor: '',
  phase: 'idle',
  command: '',
  turns: [],
  historyIndex: -1,
  redactions: 0,
  focusTick: 0,
}

export const useCommandBar = create<BarState>(() => ({ ...INITIAL }))

const MAX_HISTORY = 40

/** Intent history is per user (a shared browser in server mode must not show one user's requests to the next). */
function historyKey(): string | null {
  const id = useAuthStore.getState().user?.id
  return id ? `astraterm:ai:intents:v1:${id}` : null
}

export function intentHistory(): string[] {
  const key = historyKey()
  const h = key ? storage.get<string[]>(key, []) : []
  return Array.isArray(h) ? h.filter((x) => typeof x === 'string') : []
}

function remember(intent: string): void {
  const key = historyKey()
  if (!key) return
  const h = intentHistory().filter((x) => x !== intent)
  storage.set(key, [intent, ...h].slice(0, MAX_HISTORY))
}

let ctrl: AbortController | null = null

export function openCommandBar(opts: { tabId?: string; sessionId?: string; text?: string; submit?: boolean } = {}): boolean {
  const all = listTerminals()
  const h = (opts.tabId && getTerminalByTab(opts.tabId)) || getActiveTerminal() || (all.length === 1 ? all[0] : undefined)
  if (!h) return false
  // The bar floats over its terminal: bring that terminal's tab to the front.
  if (activeTab()?.id !== h.tabId) focusTab(h.tabId)
  ctrl?.abort()
  const same = useCommandBar.getState().open && useCommandBar.getState().tabId === h.tabId
  useCommandBar.setState((s) => ({
    ...(same && !opts.text ? s : INITIAL),
    open: true,
    tabId: h.tabId,
    sessionId: h.sessionId,
    input: opts.text ?? (same ? s.input : ''),
    focusTick: s.focusTick + 1,
  }))
  if (opts.submit && opts.text?.trim()) void generate()
  return true
}

export function closeCommandBar(refocus = true): void {
  ctrl?.abort()
  ctrl = null
  const { tabId } = useCommandBar.getState()
  useCommandBar.setState({ ...INITIAL, focusTick: useCommandBar.getState().focusTick })
  if (refocus) getTerminalByTab(tabId)?.focus()
}

export function setBarInput(input: string): void {
  useCommandBar.setState({ input, historyIndex: -1 })
}

export function setBarCommand(command: string): void {
  useCommandBar.setState({ command })
}

/** ↑ / ↓ through previous intents. */
export function browseHistory(dir: 1 | -1): void {
  const list = intentHistory()
  if (!list.length) return
  const s = useCommandBar.getState()
  const next = Math.max(-1, Math.min(list.length - 1, s.historyIndex + dir))
  useCommandBar.setState({ historyIndex: next, input: next < 0 ? '' : list[next] })
}

export async function generate(): Promise<void> {
  const s = useCommandBar.getState()
  const intent = s.input.trim()
  if (!intent || !s.open) return
  ctrl?.abort()
  const my = new AbortController()
  ctrl = my
  remember(intent)
  const h = getTerminalByTab(s.tabId)
  // A refinement keeps the previous exchange so "only in /var/log" or "without sudo" work.
  const turns: WireMessage[] = s.result && s.phase === 'done' ? [...s.turns] : []
  const messages: WireMessage[] = [...turns, { role: 'user', content: intent }]
  useCommandBar.setState({ phase: 'waiting', error: undefined, generatedFor: intent })
  let text = ''
  let result: CommandResult | undefined
  try {
    const chips = h ? [terminalChip(h, Math.min(40, aiSettings.get().terminalLines))] : []
    const context = await buildContext(chips)
    if (context && !context.sessionId && s.sessionId) context.sessionId = s.sessionId
    await streamChat(
      { messages, context: context ?? (s.sessionId ? { sessionId: s.sessionId } : undefined), mode: 'command', model: aiSettings.get().chatModel || undefined },
      (ev) => {
        if (ctrl !== my) return
        switch (ev.type) {
          case 'meta':
            useCommandBar.setState({ redactions: ev.data.redactions })
            break
          case 'thinking':
            useCommandBar.setState((st) => (st.phase === 'waiting' ? { phase: 'thinking' } : st))
            break
          case 'delta':
            text += ev.text
            break
          case 'result':
            result = ev.data
            break
          case 'error':
            useCommandBar.setState({ phase: 'error', error: describeAiError(ev.code, ev.error) })
            break
          case 'done':
            if (!result) {
              useCommandBar.setState({ phase: 'error', error: { title: 'No suggestion was returned', hint: 'Rephrase the request and try again.' } })
              break
            }
            useCommandBar.setState({
              phase: 'done',
              result,
              command: result.command,
              turns: [...messages, { role: 'assistant', content: text || JSON.stringify(result) }],
            })
            break
        }
      },
      my.signal,
    )
  } catch (err) {
    if (err instanceof DOMException && err.name === 'AbortError') return
    if (ctrl !== my) return
    const code = isApiError(err) ? err.code : 'error'
    const message = errorMessage(err)
    useCommandBar.setState({ phase: 'error', error: describeAiError(code, message) })
    if (code === 'ai_not_configured' || code === 'ai_disabled') void refreshAiStatus()
  } finally {
    if (ctrl === my) ctrl = null
  }
}

export async function insertFromBar(): Promise<void> {
  const s = useCommandBar.getState()
  if (!s.command.trim()) return
  const target = { tabId: s.tabId, sessionId: s.sessionId }
  closeCommandBar(false)
  await insertCommand(s.command, target)
}

export async function runFromBar(): Promise<void> {
  const s = useCommandBar.getState()
  if (!s.command.trim()) return
  const target = { tabId: s.tabId, sessionId: s.sessionId }
  const edited = s.command !== s.result?.command
  closeCommandBar(false)
  const ok = await requestRun(s.command, {
    // An edited command gets no stale rating from the model: the guard still checks it.
    risk: edited ? undefined : s.result?.risk,
    riskReason: edited ? undefined : s.result?.riskReason,
    target,
  })
  if (!ok) getTerminalByTab(target.tabId)?.focus()
}

// Signing out (or switching user) closes the bar and cancels its request.
useAuthStore.subscribe((s, prev) => {
  if (s.user?.id !== prev.user?.id && useCommandBar.getState().open) closeCommandBar(false)
})
