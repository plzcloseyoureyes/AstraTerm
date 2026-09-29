/*
 * Editor-scoped shortcuts. The editor is a keyboard owner (`.monaco-editor`, and the editor roots marked
 * `data-keyboard-owner="editor"`): inside it the global dispatcher (src/app/keybindings.ts) only fires `essential`
 * commands — close / next / previous / n-th tab, lock, the ⇧⌘P palette — and leaves every other key to Monaco. The
 * editor's own NexTerm commands (save, find, go to line, zoom, wrap, diff navigation, ...) are matched here instead, in
 * the capture phase on the editor's container, with the same (user-overridable) bindings; they win over Monaco's
 * keybindings for the same keys. Every other key (F1 command palette, ⌘K chords, multi-cursor, folding, ...) is
 * Monaco's.
 *
 * Vim mode: plain Ctrl+<key> combinations belong to vim (Ctrl-F, Ctrl-D, Ctrl-G...) on non-Mac platforms.
 */
import { matchKeybindingPress, parseKeybinding, type KeybindingPress } from 'tinykeys'
import { getKeybindings, isCommandEnabled, runCommand } from '@/app/commands'
import { commands } from '@/app/registry'
import { keybindingSettings, useSettingsStore } from '@/stores/settings'
import { isMac } from '@/lib/utils'

const EDITOR_COMMANDS = [
  'editor.save',
  'editor.saveAs',
  'editor.find',
  'editor.replace',
  'editor.gotoLine',
  'editor.goToFile',
  'editor.zoomIn',
  'editor.zoomOut',
  'editor.zoomReset',
  'editor.toggleWordWrap',
  'editor.nextChange',
  'editor.prevChange',
  'editor.toggleWhitespace',
  'editor.toggleMinimap',
  'editor.toggleVim',
  'editor.toggleReadOnly',
  'editor.reload',
  'editor.compareWithSaved',
  'editor.switchMode',
  'editor.exportHtml',
  'editor.print',
  // workspace (as in terminals; all have `when` guards and modifier-only bindings)
  'workspace.closeTab',
  'workspace.reopenClosed',
  'workspace.nextTab',
  'workspace.prevTab',
  ...Array.from({ length: 9 }, (_, i) => `workspace.goToTab${i + 1}`),
]

interface Compiled {
  id: string
  press: KeybindingPress
}

let compiled: Compiled[] | null = null
commands.subscribe(() => {
  compiled = null
})
useSettingsStore.subscribe((s, prev) => {
  if (s.values.keybindings !== prev.values.keybindings) compiled = null
})

function compile(): Compiled[] {
  const overrides = keybindingSettings.get()
  const out: Compiled[] = []
  for (const id of EDITOR_COMMANDS) {
    for (const b of getKeybindings(id, overrides)) {
      try {
        const seq = parseKeybinding(b)
        if (seq.length === 1) out.push({ id, press: seq[0] })
      } catch {
        /* invalid user binding */
      }
    }
  }
  return out
}

export interface AppKeyOptions {
  tabId: string
  /** Is vim mode on for this editor right now? */
  vim: () => boolean
}

/** Listen on an editor's container; returns the disposer. */
export function installAppKeys(el: HTMLElement, opts: AppKeyOptions): () => void {
  const onKeyDown = (event: KeyboardEvent) => {
    if (event.defaultPrevented || event.isComposing || (event.repeat && !/^(F7|Equal|Minus|NumpadAdd|NumpadSubtract)$/.test(event.code))) return
    if (!event.ctrlKey && !event.metaKey && !event.altKey && !/^F\d+$/.test(event.key)) return
    if (opts.vim() && !isMac && event.ctrlKey && !event.altKey && !event.metaKey) return
    if (!compiled) compiled = compile()
    for (const c of compiled) {
      if (!matchKeybindingPress(event, c.press) || !isCommandEnabled(c.id)) continue
      event.preventDefault()
      event.stopPropagation()
      // Editor commands target this editor's tab; workspace commands act on the active tab.
      void runCommand(c.id, c.id.startsWith('editor.') ? { tabId: opts.tabId } : undefined, { source: 'keybinding', event })
      return
    }
  }
  el.addEventListener('keydown', onKeyDown, true)
  return () => el.removeEventListener('keydown', onKeyDown, true)
}
