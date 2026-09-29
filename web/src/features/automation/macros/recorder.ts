/*
 * Macro recording (AUTO-1): keystrokes typed into one terminal (terminal bus onTerminalInput, broadcast copies
 * excluded) are recorded with their timing; stopping opens the macro editor with the steps. What is typed at a
 * password prompt is never recorded: the step becomes "type the stored secret" (the value stays in the vault).
 */
import { toast } from 'sonner'
import { create } from 'zustand'
import type { MacroStep } from '../types'
import { getActiveTerminal, getTerminalByTab, onTerminalInput } from '@/features/terminal/bus'
import { isPasswordPrompt, promptBeforeCursor, secretForPrompt, secretLabel, sessionSecretKeys } from '../plugins/passwordChip'
import { automationSettings } from '../settings'
import { openMacroEditor } from '../store'

interface RecorderState {
  recording: boolean
  tabId?: string
  sessionId?: string
  title?: string
  startedAt?: number
  steps: MacroStep[]
}

export const useRecorder = create<RecorderState>(() => ({ recording: false, steps: [] }))

let unsubscribe: (() => void) | null = null
let lastAt = 0
/** Typing at a password prompt: the keystrokes are swallowed until Enter, then a secret step is recorded. */
let secretEntry: { key: string; delayMs: number } | null = null
let knownKeys: string[] | null = null
let secretToastShown = false

const DEFAULT_KEYS = ['password', 'sudoPassword', 'passphrase', 'enablePassword']

const PRINTABLE = /^[^\x00-\x1f\x7f]+$/ // oxlint-disable-line no-control-regex
const STARTS_PRINTABLE = /^[^\x00-\x1f\x7f]/ // oxlint-disable-line no-control-regex

export function isRecording(): boolean {
  return useRecorder.getState().recording
}

/** Start recording the active terminal (or the given tab). */
export function startRecording(tabId?: string): boolean {
  if (isRecording()) return false
  const h = tabId ? getTerminalByTab(tabId) : getActiveTerminal()
  if (!h) {
    toast.info('Open or focus a terminal to record a macro')
    return false
  }
  const title = h.info().title
  lastAt = 0
  secretEntry = null
  knownKeys = null
  secretToastShown = false
  void sessionSecretKeys(h.sessionId).then((k) => {
    if (k?.injectable) knownKeys = k.keys
  })
  useRecorder.setState({ recording: true, tabId: h.tabId, sessionId: h.sessionId, title, startedAt: Date.now(), steps: [] })
  unsubscribe = onTerminalInput((e) => {
    const st = useRecorder.getState()
    if (!st.recording || e.broadcast || e.tabId !== st.tabId) return
    // Terminal replies (cursor position reports…) are not keystrokes.
    if (/^\x1b\[\??[\d;]*[Rcn]$/.test(e.data)) return // oxlint-disable-line no-control-regex
    const now = Date.now()
    const delay = lastAt ? Math.min(now - lastAt, 600_000) : 0
    lastAt = now
    if (!secretEntry && STARTS_PRINTABLE.test(e.data)) {
      const term = getTerminalByTab(st.tabId)?.term
      const prompt = term ? promptBeforeCursor(term) : ''
      if (prompt && isPasswordPrompt(prompt)) secretEntry = { key: secretForPrompt(prompt, knownKeys ?? DEFAULT_KEYS) ?? 'password', delayMs: delay }
    }
    if (secretEntry) {
      if (e.data.includes('\x03')) {
        secretEntry = null // Ctrl+C: the prompt was abandoned
      } else if (/[\r\n]/.test(e.data)) {
        // Enter (also a pasted "secret\r"): record "type the stored secret, then Enter".
        const step: MacroStep = { data: '\r', delayMs: secretEntry.delayMs, secret: secretEntry.key }
        useRecorder.setState({ steps: [...st.steps, step] })
        if (!secretToastShown) {
          secretToastShown = true
          toast.info(`The ${secretLabel(step.secret!)} you typed was not recorded`, {
            description: 'The macro types the stored secret of the connection at this step instead.',
          })
        }
        secretEntry = null
        return
      } else {
        return // swallowed: never recorded
      }
    }
    const steps = st.steps.slice()
    const merge = automationSettings.get().macroMergeMs
    const prev = steps[steps.length - 1]
    if (prev && merge > 0 && delay <= merge && PRINTABLE.test(prev.data) && PRINTABLE.test(e.data)) {
      steps[steps.length - 1] = { ...prev, data: prev.data + e.data }
    } else {
      steps.push({ data: e.data, delayMs: delay })
    }
    useRecorder.setState({ steps })
  })
  toast.info(`Recording a macro in ${title}`, { description: 'Type in the terminal, then stop recording from the status bar or the Macros panel.' })
  h.focus()
  return true
}

function reset(): RecorderState {
  const st = useRecorder.getState()
  secretEntry = null
  unsubscribe?.()
  unsubscribe = null
  useRecorder.setState({ recording: false, tabId: undefined, sessionId: undefined, title: undefined, startedAt: undefined, steps: [] })
  return st
}

/** Stop recording and open the editor to name and save the macro. */
export function stopRecording(): void {
  const st = reset()
  if (!st.steps.length) {
    toast.info('Nothing was recorded')
    return
  }
  openMacroEditor({ steps: st.steps, name: `Recorded in ${st.title ?? 'terminal'}` })
}

export function cancelRecording(): void {
  reset()
}

export function toggleRecording(): void {
  if (isRecording()) stopRecording()
  else startRecording()
}
