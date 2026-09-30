/*
 * Terminal commands (palette, menus, keybindings — all user-rebindable in Settings → Keyboard) and the keyboard claims
 * that keep shell keys away from app shortcuts while a terminal is focused.
 */
import {
  ClipboardCopy,
  ClipboardPaste,
  CopyPlus,
  Download,
  Eraser,
  History,
  LogOut,
  PanelBottom,
  PanelRight,
  Pause,
  RefreshCw,
  RotateCcw,
  Search,
  SquareTerminal,
  Workflow,
  ZoomIn,
  ZoomOut,
} from 'lucide-react'
import { matchKeybindingPress, parseKeybinding } from 'tinykeys'
import { registerCommand, registerMenu } from '@/app/registry'
import { getKeybindings, runCommand } from '@/app/commands'
import { claimKeys } from '@/app/keybindings'
import { isMac } from '@/lib/utils'
import { closeTab } from '@/stores/workspace'
import { getFocusedTabTerminal, listTerminals } from './bus'
import { toggleMultiExec, useMultiExecStore } from './multiexec'
import { attachSession, duplicateSession, openLocalShell } from './open'
import { terminalSettings } from './settings'
import type { TerminalHandle } from './types'

/** Tabs closed through "Detach" keep their session running (see onClose in index.ts). */
export const detachingTabs = new Set<string>()

/** Close a terminal tab but keep its session running on the server (re-attach later from Home / Sessions). */
export function detachTerminal(tabId: string): void {
  detachingTabs.add(tabId)
  void closeTab(tabId, { force: true }).then((closed) => {
    if (!closed) detachingTabs.delete(tabId)
  })
}

const hasTerminal = () => !!getFocusedTabTerminal()

function withTerminal(fn: (t: TerminalHandle) => unknown) {
  return () => {
    const t = getFocusedTabTerminal()
    if (t) return fn(t) as void
  }
}

function argString(args: unknown, key: string): string | undefined {
  if (args && typeof args === 'object' && typeof (args as Record<string, unknown>)[key] === 'string') return (args as Record<string, string>)[key]
  return undefined
}

const CATEGORY = 'Terminal'

registerCommand<{ shellId?: string } | undefined>({
  id: 'terminal.newLocal',
  title: 'New Local Terminal',
  category: CATEGORY,
  icon: SquareTerminal,
  keybinding: '$mod+Shift+l',
  keywords: ['shell', 'bash', 'zsh', 'powershell', 'local', 'console'],
  run: ({ args }) => openLocalShell(argString(args, 'shellId')).then(() => undefined),
})

registerCommand<{ sessionId: string }>({
  id: 'terminal.attach',
  title: 'Attach to Session',
  category: CATEGORY,
  hidden: true,
  run: async ({ args }) => {
    const id = argString(args, 'sessionId')
    if (id) await attachSession(id)
  },
})

registerCommand({
  id: 'terminal.duplicate',
  title: 'Duplicate Session',
  category: CATEGORY,
  icon: CopyPlus,
  when: hasTerminal,
  run: ({ activeTab }) => {
    const t = getFocusedTabTerminal()
    if (t) return duplicateSession(t.tabId).then(() => undefined)
    if (activeTab) return duplicateSession(activeTab.id).then(() => undefined)
  },
})

registerCommand({
  id: 'terminal.splitRight',
  title: 'Split Terminal Right',
  category: CATEGORY,
  icon: PanelRight,
  when: hasTerminal,
  run: withTerminal((t) => duplicateSession(t.tabId, { position: 'right' })),
})

registerCommand({
  id: 'terminal.splitDown',
  title: 'Split Terminal Down',
  category: CATEGORY,
  icon: PanelBottom,
  when: hasTerminal,
  run: withTerminal((t) => duplicateSession(t.tabId, { position: 'below' })),
})

registerCommand({
  id: 'terminal.find',
  title: 'Find in Terminal',
  category: CATEGORY,
  icon: Search,
  keybinding: '$mod+Shift+f',
  when: hasTerminal,
  run: withTerminal((t) => t.openSearch()),
})

registerCommand({
  id: 'terminal.clear',
  title: 'Clear Scrollback',
  category: CATEGORY,
  icon: Eraser,
  when: hasTerminal,
  run: withTerminal((t) => t.clear()),
})

registerCommand({
  id: 'terminal.reset',
  title: 'Reset Terminal',
  category: CATEGORY,
  icon: RotateCcw,
  when: hasTerminal,
  run: withTerminal((t) => t.reset()),
})

registerCommand({
  id: 'terminal.zoomIn',
  title: 'Terminal: Zoom In',
  category: CATEGORY,
  icon: ZoomIn,
  keybinding: ['$mod+Equal', '$mod+Shift+Equal', '$mod+NumpadAdd'],
  when: hasTerminal,
  run: withTerminal((t) => t.zoomBy(1)),
})

registerCommand({
  id: 'terminal.zoomOut',
  title: 'Terminal: Zoom Out',
  category: CATEGORY,
  icon: ZoomOut,
  keybinding: ['$mod+Minus', '$mod+NumpadSubtract'],
  when: hasTerminal,
  run: withTerminal((t) => t.zoomBy(-1)),
})

registerCommand({
  id: 'terminal.zoomReset',
  title: 'Terminal: Reset Zoom',
  category: CATEGORY,
  keybinding: ['$mod+Digit0', '$mod+Numpad0'],
  when: hasTerminal,
  run: withTerminal((t) => t.zoomReset()),
})

registerCommand({
  id: 'terminal.copy',
  title: 'Copy',
  category: CATEGORY,
  icon: ClipboardCopy,
  keybinding: isMac ? ['Control+Shift+c'] : ['Control+Shift+c', 'Control+Insert'],
  when: hasTerminal,
  run: withTerminal((t) => t.copySelection()),
})

registerCommand({
  id: 'terminal.paste',
  title: 'Paste',
  category: CATEGORY,
  icon: ClipboardPaste,
  keybinding: isMac ? ['Control+Shift+v'] : ['Control+Shift+v', 'Shift+Insert'],
  when: hasTerminal,
  run: withTerminal((t) => t.pasteFromClipboard()),
})

registerCommand({
  id: 'terminal.pasteSlow',
  title: 'Paste Line by Line',
  category: CATEGORY,
  when: hasTerminal,
  run: withTerminal((t) => t.pasteFromClipboard({ mode: 'paced' })),
})

registerCommand({
  id: 'terminal.clipboardHistory',
  title: 'Paste from Clipboard History…',
  category: CATEGORY,
  icon: History,
  keywords: ['clipboard', 'history', 'recent copies'],
  when: hasTerminal,
  run: withTerminal((t) => t.openClipboardHistory()),
})

registerCommand({
  id: 'terminal.selectAll',
  title: 'Select All',
  category: CATEGORY,
  when: hasTerminal,
  run: withTerminal((t) => t.selectAll()),
})

registerCommand({
  id: 'terminal.multiexec.toggle',
  title: 'Toggle Broadcast Input',
  category: CATEGORY,
  icon: Workflow,
  keywords: ['multiexec', 'multi exec', 'cluster', 'send to all', 'broadcast'],
  when: () => listTerminals().length > 0 || useMultiExecStore.getState().active,
  run: () => toggleMultiExec(),
})

registerCommand({
  id: 'terminal.reconnect',
  title: 'Reconnect Session',
  category: CATEGORY,
  icon: RefreshCw,
  when: hasTerminal,
  run: withTerminal((t) => t.reconnect()),
})

registerCommand({
  id: 'terminal.pauseOutput',
  title: 'Pause / Resume Output',
  category: CATEGORY,
  icon: Pause,
  keybinding: 'ScrollLock',
  keywords: ['scroll lock', 'freeze'],
  when: hasTerminal,
  run: withTerminal((t) => t.togglePause()),
})

registerCommand({
  id: 'terminal.saveOutput',
  title: 'Save Terminal Output…',
  category: CATEGORY,
  icon: Download,
  keybinding: '$mod+Shift+s',
  when: hasTerminal,
  run: withTerminal((t) => t.saveOutput('text')),
})

registerCommand({
  id: 'terminal.saveOutputHtml',
  title: 'Save Terminal Output as HTML…',
  category: CATEGORY,
  icon: Download,
  when: hasTerminal,
  run: withTerminal((t) => t.saveOutput('html')),
})

registerCommand({
  id: 'terminal.copyAll',
  title: 'Copy All Terminal Output',
  category: CATEGORY,
  icon: ClipboardCopy,
  when: hasTerminal,
  run: withTerminal((t) => t.copyAll('text')),
})

registerCommand({
  id: 'terminal.prevPrompt',
  title: 'Scroll to Previous Prompt',
  category: CATEGORY,
  when: hasTerminal,
  run: withTerminal((t) => t.jumpToPrompt(-1)),
})

registerCommand({
  id: 'terminal.nextPrompt',
  title: 'Scroll to Next Prompt',
  category: CATEGORY,
  when: hasTerminal,
  run: withTerminal((t) => t.jumpToPrompt(1)),
})

registerCommand({
  id: 'terminal.interrupt',
  title: 'Send Interrupt (SIGINT)',
  category: CATEGORY,
  when: hasTerminal,
  run: withTerminal((t) => t.sendSignal('INT')),
})

registerCommand({
  id: 'terminal.detach',
  title: 'Detach Terminal (Keep Session Running)',
  category: CATEGORY,
  icon: LogOut,
  when: hasTerminal,
  run: withTerminal((t) => detachTerminal(t.tabId)),
})

registerCommand({
  id: 'terminal.settings',
  title: 'Terminal Settings',
  category: 'Preferences',
  icon: SquareTerminal,
  keywords: ['font', 'colour', 'color', 'scheme', 'theme', 'cursor', 'bell', 'paste'],
  run: () => runCommand('settings.open', { section: 'terminal' }).then(() => undefined),
})

// ---------------------------------------------------------------------------------------------------------------------
// Menu bar contributions ("Terminal" menu)
// ---------------------------------------------------------------------------------------------------------------------

registerMenu({
  menu: 'terminal',
  order: 150,
  items: () => [
    { label: 'Find…', command: 'terminal.find' },
    { label: 'Copy', command: 'terminal.copy' },
    { label: 'Paste', command: 'terminal.paste' },
    { label: 'Paste line by line', command: 'terminal.pasteSlow' },
    { label: 'Paste from history…', command: 'terminal.clipboardHistory' },
    { type: 'separator' },
    { label: 'Clear scrollback', command: 'terminal.clear' },
    { label: 'Reset terminal', command: 'terminal.reset' },
    { label: 'Pause output', command: 'terminal.pauseOutput' },
    { label: 'Save output…', command: 'terminal.saveOutput' },
    {
      type: 'submenu',
      label: 'Font size',
      items: [
        { label: 'Zoom in', command: 'terminal.zoomIn' },
        { label: 'Zoom out', command: 'terminal.zoomOut' },
        { label: 'Reset', command: 'terminal.zoomReset' },
      ],
    },
    { type: 'separator' },
    { label: 'Duplicate session', command: 'terminal.duplicate' },
    { label: 'Split right', command: 'terminal.splitRight' },
    { label: 'Split down', command: 'terminal.splitDown' },
    { label: 'Reconnect', command: 'terminal.reconnect' },
    { label: 'Detach (keep running)', command: 'terminal.detach' },
    { type: 'separator' },
    { label: 'Broadcast input', checked: useMultiExecStore.getState().active, command: 'terminal.multiexec.toggle' },
    { label: 'Terminal settings', command: 'terminal.settings' },
  ],
})

// ---------------------------------------------------------------------------------------------------------------------
// Keyboard claims (only consulted while focus is inside a terminal)
// ---------------------------------------------------------------------------------------------------------------------

const parsed = new Map<string, ReturnType<typeof parseKeybinding>>()

function matches(e: KeyboardEvent, binding: string): boolean {
  let seq = parsed.get(binding)
  if (!seq) {
    try {
      seq = parseKeybinding(binding)
    } catch {
      return false
    }
    parsed.set(binding, seq)
  }
  // Only single-press bindings can be passed through.
  return seq.length === 1 && matchKeybindingPress(e, seq[0])
}

/** Combos the browser turns into a native `paste` event on the focused terminal (no clipboard permission needed). */
function isNativePasteCombo(e: KeyboardEvent): boolean {
  if (isMac) return false
  const ctrlShiftV = e.ctrlKey && e.shiftKey && !e.altKey && !e.metaKey && e.code === 'KeyV'
  const shiftInsert = e.shiftKey && !e.ctrlKey && !e.altKey && !e.metaKey && e.key === 'Insert'
  return ctrlShiftV || shiftInsert
}

claimKeys((e) => {
  const s = terminalSettings.get()
  if (s.altKeysToTerminal && e.altKey && !e.ctrlKey && !e.metaKey && (e.key.length === 1 || /^(Key|Digit)/.test(e.code))) return true
  for (const b of s.passthroughKeys) if (matches(e, b)) return true
  // Let the browser raise a paste event (handled by the terminal's paste pipeline) instead of reading the clipboard API.
  if (isNativePasteCombo(e) && getKeybindings('terminal.paste').some((b) => matches(e, b))) return true
  return false
})
