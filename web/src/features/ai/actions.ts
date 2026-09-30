/*
 * High-level assistant actions used by commands, menus, the command bar and terminal plugins.
 */
import { toast } from 'sonner'
import { runCommand } from '@/app/commands'
import { sidebarPanels } from '@/app/registry'
import { getActiveTerminal } from '@/features/terminal/bus'
import type { TerminalHandle } from '@/features/terminal/types'
import { showSidebarPanel } from '@/layout/Sidebar'
import { findTabs, focusTab, openTab } from '@/stores/workspace'
import { failureChip, selectionChip, terminalChip } from './context'
import { aiSettings } from './settings'
import { focusComposer, getConversation, isAiAvailable, newConversation, refreshAiStatus, sendMessage, setChips, useAiStatusStore, useChatStore } from './store'
import type { AiMode, ContextChip } from './types'

export const AI_TAB = 'ai'
export const AI_PANEL = 'ai'

/** Show the assistant: the AI tab when one is open, else the sidebar panel (else a new tab). */
export function showAssistant(): void {
  const tab = findTabs((t) => t.kind === AI_TAB)[0]
  if (tab) {
    focusTab(tab.id)
  } else if (sidebarPanels.get(AI_PANEL)) {
    showSidebarPanel(AI_PANEL)
  } else {
    openTab({ kind: AI_TAB, params: {} })
  }
  focusComposer()
}

export function openSettings(): void {
  void runCommand('settings.open', { section: 'ai' })
}

/** Guard for entry points: when the assistant is not usable, explain why (and offer setup) instead. */
export async function ensureAvailable(): Promise<boolean> {
  if (isAiAvailable()) return true
  const s = useAiStatusStore.getState().status ?? (await refreshAiStatus())
  if (s?.available) return true
  if (s?.reason === 'disabled' && !s.canConfigureGlobal && !s.canConfigure) {
    toast.info('The AI assistant is turned off', { description: 'Your administrator can enable it.' })
  } else {
    toast.info('Set up the AI assistant first', {
      description: 'Choose a provider (Anthropic, OpenAI, Ollama, …). Nothing is sent anywhere until you do.',
      action: { label: 'Set up', onClick: openSettings },
    })
  }
  return false
}

/** Default chips for a new chat: the active terminal's recent output (setting). */
function defaultChips(): ContextChip[] {
  const h = getActiveTerminal()
  return h && aiSettings.get().autoAttachTerminal ? [terminalChip(h)] : []
}

/** Start a fresh conversation (optionally with context) and show it. */
export function startChat(chips: ContextChip[] = defaultChips()): string {
  const id = newConversation()
  setChips(chips)
  showAssistant()
  return id
}

/** Ask a question (command `ai.ask`): new conversation unless the current one is empty. */
export async function ask(text: string, opts: { chips?: ContextChip[]; mode?: AiMode; display?: string } = {}): Promise<void> {
  if (!(await ensureAvailable())) return
  const cur = getConversation(useChatStore.getState().activeId)
  const convId = cur && cur.messages.length === 0 ? cur.id : newConversation()
  const chips = opts.chips ?? useChatStore.getState().chips
  setChips(chips)
  showAssistant()
  if (text.trim()) await sendMessage(convId, text, { mode: opts.mode, chips, display: opts.display })
}

const EXPLAIN_PROMPT =
  'Explain this terminal output. If it shows an error, explain what went wrong, the most likely cause and how to fix it.'
const FIX_PROMPT =
  'This failed. Give me the most likely fix — the exact commands to run, least invasive first — and briefly why it works.'

/** "Explain with AI" / "Fix with AI" on a terminal selection. */
export function explainSelection(text: string, handle: TerminalHandle | undefined, fix = false): void {
  if (!text.trim()) {
    toast.info('Select some terminal output first')
    return
  }
  const chips: ContextChip[] = [selectionChip(text, handle)]
  void ask(fix ? FIX_PROMPT : EXPLAIN_PROMPT, { chips, mode: 'explain', display: fix ? 'Fix this error' : 'Explain this output' })
}

/** Explain a failed command (TOOL-12: OSC 133 exit code or recognised error output). */
export function explainFailure(opts: { command: string; exitCode?: number; output: string; tabId?: string; sessionId?: string }, fix = false): void {
  const chip = failureChip(opts)
  void ask(fix ? FIX_PROMPT : EXPLAIN_PROMPT, {
    chips: [chip],
    mode: 'explain',
    display: fix ? 'How do I fix this?' : 'Why did this fail?',
  })
}
