/*
 * Global keyboard shortcut dispatcher (UI-8). Bindings use tinykeys syntax ("$mod+Shift+P", "Control+Alt+t",
 * "g i" for sequences) and come from the command registry, overridable per user (settings.keybindings).
 *
 * Scopes decide whether a matching binding fires:
 *   - keyboard owners (embedded widgets with their own shortcuts: `.monaco-editor`, `[data-keyboard-owner]`): only
 *     commands marked `essential` (tab close / switch, lock, the ⇧⌘P palette) — the widget keeps ⌘K chords, ⇧⌘O…
 *   - text inputs / textareas / contenteditable: only commands marked `global`
 *   - terminals (.xterm / [data-terminal]): every command except keys the terminal claims. By default plain
 *     Ctrl+<key> (no Alt/Shift/Meta) belongs to the shell (^W, ^R, ^C, ^K...). Features add claimers with claimKeys().
 *   - elsewhere: every available command
 *
 * The listener runs in the capture phase on window so app shortcuts win over focused widgets.
 */
import { matchKeybindingPress, parseKeybinding, type KeybindingPress } from 'tinykeys'
import { commands } from './registry'
import { getKeybindings, runCommand } from './commands'
import { keybindingSettings, useSettingsStore } from '@/stores/settings'
import { isEditableTarget } from '@/lib/utils'
import { essentialBinding, normalizeKeybinding } from '@/lib/keys'

export { eventToKeybinding, formatKeybinding, normalizeKeybinding } from '@/lib/keys'

type Scope = 'owner' | 'input' | 'terminal' | 'other'

/** Elements whose keyboard shortcuts win over the app's (see CommandDef.essential). */
export const KEYBOARD_OWNER_SELECTOR = '[data-keyboard-owner], .monaco-editor'

interface Compiled {
  commandId: string
  binding: string
  seq: KeybindingPress[]
}

const claimers = new Set<(e: KeyboardEvent) => boolean>()

/**
 * Let a widget claim keys (return true to keep the event away from app shortcuts). Only consulted while focus is
 * inside a terminal. Returns an unregister function.
 */
export function claimKeys(pred: (e: KeyboardEvent) => boolean): () => void {
  claimers.add(pred)
  return () => claimers.delete(pred)
}

/** Plain Ctrl+<printable> produces a control character in terminals — leave it to the shell. */
function isTerminalControlKey(e: KeyboardEvent): boolean {
  return e.ctrlKey && !e.altKey && !e.metaKey && !e.shiftKey && (e.key.length === 1 || e.code.startsWith('Key'))
}

export function scopeOf(target: EventTarget | null): Scope {
  if (target instanceof Element && target.closest(KEYBOARD_OWNER_SELECTOR)) return 'owner'
  if (target instanceof Element && target.closest('.xterm, [data-terminal]')) return 'terminal'
  return isEditableTarget(target) ? 'input' : 'other'
}

let compiled: Compiled[] = []
let dirty = true

function compile(): void {
  const overrides = keybindingSettings.get()
  const out: Compiled[] = []
  for (const cmd of commands.list()) {
    for (const binding of getKeybindings(cmd.id, overrides)) {
      try {
        out.push({ commandId: cmd.id, binding, seq: parseKeybinding(binding) })
      } catch (err) {
        console.warn(`[keybindings] invalid binding "${binding}" for ${cmd.id}`, err)
      }
    }
  }
  compiled = out
  dirty = false
}

commands.subscribe(() => {
  dirty = true
})
useSettingsStore.subscribe((s, prev) => {
  if (s.values.keybindings !== prev.values.keybindings) dirty = true
})

let suspended = 0

/**
 * Temporarily disable all app shortcuts (e.g. while recording a new keybinding). Returns a function that re-enables
 * them; nested calls are counted.
 */
export function suspendKeybindings(): () => void {
  suspended++
  let done = false
  return () => {
    if (done) return
    done = true
    suspended = Math.max(0, suspended - 1)
  }
}

let pending = new Map<Compiled, number>()
let pendingTimer: ReturnType<typeof setTimeout> | null = null

function canFire(c: Compiled, scope: Scope, e: KeyboardEvent): boolean {
  const cmd = commands.get(c.commandId)
  if (!cmd) return false
  if (scope === 'owner' && !essentialBinding(cmd.essential, c.binding)) return false
  if (scope === 'input' && !cmd.global) return false
  // Terminal claims win over every command (even `global` ones): ^K, ^R, ^W... belong to the shell.
  if (scope === 'terminal') {
    if (isTerminalControlKey(e)) return false
    for (const claim of claimers) {
      try {
        if (claim(e)) return false
      } catch {
        /* ignore faulty claimer */
      }
    }
  }
  if (cmd.when) {
    try {
      if (!cmd.when()) return false
    } catch {
      return false
    }
  }
  return true
}

function onKeyDown(e: KeyboardEvent): void {
  if (suspended > 0 || !e.key || !e.code || e.isComposing || e.repeat) return
  if (e.key === 'Shift' || e.key === 'Control' || e.key === 'Alt' || e.key === 'Meta') return
  if (dirty) compile()
  const scope = scopeOf(e.target)
  const next = new Map<Compiled, number>()
  let fired: Compiled | null = null

  for (const c of compiled) {
    const idx = pending.get(c) ?? 0
    let matchedAt = -1
    if (matchKeybindingPress(e, c.seq[idx])) matchedAt = idx
    else if (idx > 0 && matchKeybindingPress(e, c.seq[0])) matchedAt = 0
    if (matchedAt < 0) continue
    if (matchedAt + 1 < c.seq.length) {
      next.set(c, matchedAt + 1)
    } else if (!fired && canFire(c, scope, e)) {
      fired = c
    }
  }

  if (pendingTimer) clearTimeout(pendingTimer)
  pending = fired ? new Map() : next
  if (pending.size) pendingTimer = setTimeout(() => (pending = new Map()), 1000)

  if (fired) {
    e.preventDefault()
    e.stopPropagation()
    void runCommand(fired.commandId, undefined, { source: 'keybinding', event: e })
  }
}

const installed = new WeakSet<Window>()

/** Install the dispatcher on a window (main window, dockview pop-outs). Returns a disposer. */
export function installKeybindings(win: Window = window): () => void {
  if (installed.has(win)) return () => undefined
  installed.add(win)
  win.addEventListener('keydown', onKeyDown, true)
  return () => {
    installed.delete(win)
    win.removeEventListener('keydown', onKeyDown, true)
  }
}

/** Commands (other than `exceptId`) whose effective bindings collide with `binding`. */
export function findConflicts(binding: string, exceptId?: string): string[] {
  const target = normalizeKeybinding(binding)
  const overrides = keybindingSettings.get()
  const out: string[] = []
  for (const cmd of commands.list()) {
    if (cmd.id === exceptId) continue
    if (getKeybindings(cmd.id, overrides).some((b) => normalizeKeybinding(b) === target)) out.push(cmd.id)
  }
  return out
}
