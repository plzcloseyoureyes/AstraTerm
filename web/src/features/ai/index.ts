/*
 * AI assistant feature (SPEC §10.3 "ai"; RESEARCH TOOL-10, TOOL-11, TOOL-12). Optional: until a provider is
 * configured nothing is sent anywhere and the assistant stays out of the way — only the Settings section, the
 * palette commands and a Tools-menu entry lead to its setup.
 *
 *   tab kind        "ai" (singleton) — the chat as a tab
 *   sidebar panel   "ai" — the chat (registered while the assistant is available)
 *   overlay         inline command bar (Ctrl/Cmd+I in a terminal), run confirmation
 *   terminal plugin "ai.assist" — Ctrl+I, `# request` + Enter, "Explain · Fix" chips on failed commands
 *   context menu    terminal: Explain / Fix with AI (in the menu's selection section), Ask AI about this terminal,
 *                   command bar
 *   settings        "AI assistant" (provider, key, model, policy, preferences)
 *
 * Commands: ai.open, ai.ask {text, context?, mode?}, ai.commandBar, ai.newChat, ai.explainSelection,
 * ai.fixSelection, ai.settings.
 */
import { lazy } from 'react'
import { MessageSquarePlus, Sparkles, Wand2 } from 'lucide-react'
import {
  registerCommand,
  registerContextMenu,
  registerMenu,
  registerOverlay,
  registerSettingsSection,
  registerSidebarPanel,
  registerTabKind,
  registerTerminalPlugin,
  type MenuItem,
} from '@/app/registry'
import { getActiveTerminal, getTerminalByTab } from '@/features/terminal/bus'
import { isPlainObject } from '@/lib/utils'
import { toast } from 'sonner'
import { AI_PANEL, AI_TAB, ask, ensureAvailable, explainSelection, openSettings, showAssistant, startChat } from './actions'
import { openCommandBar } from './barStore'
import { selectionChip, terminalChip } from './context'
import { AiOverlay } from './Overlay'
import { COMMAND_BAR_COMMAND, setupAiTerminal } from './plugin'
import { installAiState, isAiAvailable, useAiStatusStore } from './store'
import type { AiMode, ContextChip } from './types'

installAiState()

const CATEGORY = 'AI'

const ChatPanel = lazy(() => import('./ChatView'))
const ChatTab = lazy(() => import('./ChatView').then((m) => ({ default: m.AiTabView })))

registerTabKind({
  kind: AI_TAB,
  title: () => 'AI assistant',
  icon: Sparkles,
  singleton: true,
  component: ChatTab,
})

registerOverlay({ id: 'ai', component: AiOverlay, order: 70 })

registerSettingsSection({
  id: 'ai',
  title: 'AI assistant',
  icon: Sparkles,
  order: 60,
  keywords: ['ai', 'assistant', 'claude', 'anthropic', 'openai', 'ollama', 'lm studio', 'gemini', 'llm', 'gpt', 'copilot', 'api key', 'model', 'redaction'],
  component: lazy(() => import('./SettingsSection')),
})

registerTerminalPlugin({ id: 'ai.assist', order: 90, setup: setupAiTerminal })

// ---------------------------------------------------------------------------------------------------------------------
// commands
// ---------------------------------------------------------------------------------------------------------------------

registerCommand({
  id: 'ai.open',
  title: 'AI Assistant',
  category: CATEGORY,
  icon: Sparkles,
  keybinding: 'Alt+Shift+KeyA',
  keywords: ['chat', 'claude', 'copilot', 'help', 'llm'],
  description: 'Ask about your servers, errors and commands',
  run: async () => {
    if (await ensureAvailable()) showAssistant()
  },
})

registerCommand({
  id: 'ai.newChat',
  title: 'New AI Chat',
  category: CATEGORY,
  icon: MessageSquarePlus,
  run: async () => {
    if (await ensureAvailable()) startChat()
  },
})

registerCommand({
  id: COMMAND_BAR_COMMAND,
  title: 'AI: Command from Description…',
  category: CATEGORY,
  icon: Wand2,
  keybinding: '$mod+KeyI',
  global: false,
  keywords: ['natural language', 'generate command', 'shell', 'suggest'],
  description: 'Describe a task in plain words and get a command for the active terminal',
  run: async () => {
    if (!(await ensureAvailable())) return
    if (!openCommandBar()) toast.info('Focus a terminal first', { description: 'The command bar suggests commands for the active terminal.' })
  },
})

function argsObject(args: unknown): Record<string, unknown> {
  return isPlainObject(args) ? args : {}
}

/** Cross-module contract: `ai.ask {text, context?}` (context: {selection?, sessionId?, tabId?, terminal?: boolean}). */
registerCommand<{ text?: string; context?: Record<string, unknown>; mode?: AiMode } | string>({
  id: 'ai.ask',
  title: 'Ask AI…',
  category: CATEGORY,
  hidden: true,
  run: async ({ args }) => {
    const a = typeof args === 'string' ? { text: args } : argsObject(args)
    const text = typeof a.text === 'string' ? a.text : ''
    const c = argsObject(a.context)
    const chips: ContextChip[] = []
    const tabId = typeof c.tabId === 'string' ? c.tabId : undefined
    const h = (tabId && getTerminalByTab(tabId)) || undefined
    if (typeof c.selection === 'string' && c.selection.trim()) chips.push(selectionChip(c.selection, h))
    if (c.terminal === true || typeof c.sessionId === 'string') {
      const th = h ?? getActiveTerminal()
      if (th) chips.push(terminalChip(th))
    }
    const mode = a.mode === 'explain' || a.mode === 'command' ? (a.mode as AiMode) : 'chat'
    await ask(text, { chips, mode })
  },
})

registerCommand({
  id: 'ai.explainSelection',
  title: 'Explain Terminal Selection with AI',
  category: CATEGORY,
  when: isAiAvailable,
  run: () => {
    const h = getActiveTerminal()
    explainSelection(h?.getSelection() ?? '', h)
  },
})

registerCommand({
  id: 'ai.fixSelection',
  title: 'Fix Error in Terminal Selection with AI',
  category: CATEGORY,
  when: isAiAvailable,
  run: () => {
    const h = getActiveTerminal()
    explainSelection(h?.getSelection() ?? '', h, true)
  },
})

registerCommand({
  id: 'ai.settings',
  title: 'AI Assistant Settings',
  category: CATEGORY,
  keywords: ['set up', 'provider', 'api key', 'model'],
  run: () => openSettings(),
})

// ---------------------------------------------------------------------------------------------------------------------
// menus (Tools menu always: discoverability; the rest only while the assistant is available)
// ---------------------------------------------------------------------------------------------------------------------

registerMenu({
  menu: 'tools',
  order: 150,
  items: (): MenuItem[] => {
    const s = useAiStatusStore.getState().status
    if (s?.available) {
      return [
        { label: 'AI Assistant', icon: Sparkles, command: 'ai.open' },
        { label: 'AI Command from Description…', icon: Wand2, command: COMMAND_BAR_COMMAND },
      ]
    }
    if (s && (s.canConfigure || s.canConfigureGlobal)) return [{ label: 'Set up AI Assistant…', icon: Sparkles, command: 'ai.settings' }]
    return []
  },
})

let dynamicOff: (() => void)[] = []

function syncDynamic(available: boolean): void {
  for (const off of dynamicOff) off()
  dynamicOff = []
  if (!available) return
  dynamicOff.push(
    registerSidebarPanel({ id: AI_PANEL, title: 'AI Assistant', icon: Sparkles, order: 9, component: ChatPanel }),
    // Selection actions join the terminal menu's built-in selection section (right after Copy / Paste) …
    registerContextMenu({
      target: 'terminal',
      group: 'selection',
      order: 45,
      items: (ctx): MenuItem[] => {
        const selection = ctx.selection ?? ''
        if (!selection.trim()) return []
        const h = getTerminalByTab(ctx.tabId)
        return [
          { label: 'Explain with AI', icon: Sparkles, run: () => explainSelection(selection, h) },
          { label: 'Fix with AI', icon: Wand2, run: () => explainSelection(selection, h, true) },
        ]
      },
    }),
    // … the terminal-wide ones form their own section after the built-in ones.
    registerContextMenu({
      target: 'terminal',
      order: 45,
      items: (ctx): MenuItem[] => [
        {
          label: 'Ask AI about this terminal…',
          icon: MessageSquarePlus,
          run: () => {
            const h = getTerminalByTab(ctx.tabId)
            startChat(h ? [terminalChip(h)] : [])
          },
        },
        { label: 'Command from description…', icon: Wand2, command: COMMAND_BAR_COMMAND },
      ],
    }),
  )
}

let lastAvailable = false
useAiStatusStore.subscribe((s) => {
  const available = !!s.status?.available
  if (available === lastAvailable) return
  lastAvailable = available
  syncDynamic(available)
})
