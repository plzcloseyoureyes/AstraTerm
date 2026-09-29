/*
 * Keybinding string helpers (tinykeys syntax): display formatting, canonical comparison, recording from events.
 */
import { isMac } from './utils'

const MAC_MODS: Record<string, string> = { Control: '⌃', Alt: '⌥', Shift: '⇧', Meta: '⌘' }
const PC_MODS: Record<string, string> = { Control: 'Ctrl', Alt: 'Alt', Shift: 'Shift', Meta: 'Win' }
const KEY_NAMES: Record<string, string> = {
  arrowleft: '←',
  arrowright: '→',
  arrowup: '↑',
  arrowdown: '↓',
  escape: 'Esc',
  enter: '↵',
  backspace: '⌫',
  delete: 'Del',
  pageup: 'PgUp',
  pagedown: 'PgDn',
  space: 'Space',
  ' ': 'Space',
  tab: 'Tab',
  home: 'Home',
  end: 'End',
  insert: 'Ins',
  comma: ',',
  period: '.',
  slash: '/',
  backslash: '\\',
  semicolon: ';',
  quote: "'",
  bracketleft: '[',
  bracketright: ']',
  minus: '-',
  equal: '=',
  backquote: '`',
}

function keyLabel(key: string): string {
  const k = key.toLowerCase()
  if (/^key[a-z]$/.test(k)) return k.slice(3).toUpperCase()
  if (/^digit\d$/.test(k)) return k.slice(5)
  if (/^numpad\d$/.test(k)) return `Num${k.slice(6)}`
  if (KEY_NAMES[k]) return KEY_NAMES[k]
  if (/^f\d{1,2}$/.test(k)) return k.toUpperCase()
  return key.length === 1 ? key.toUpperCase() : key
}

/** Split one press "Control+Shift+p" into modifiers and key (handles "+" as a key: "Control++"). */
function splitPress(press: string): { mods: string[]; key: string } {
  const parts = press.split(/(?<=\w|\])\+/)
  const key = parts.pop() ?? ''
  const mods = parts.map((m) => {
    const bare = m.replace(/^\[|\]$/g, '')
    if (bare === '$mod') return isMac ? 'Meta' : 'Control'
    if (bare === 'Ctrl') return 'Control'
    if (bare === 'Cmd' || bare === 'Command') return 'Meta'
    if (bare === 'Option') return 'Alt'
    return bare
  })
  return { mods, key }
}

const MOD_ORDER = ['Control', 'Alt', 'Shift', 'Meta']

/** Human-readable binding: "$mod+Shift+p" → "⌘⇧P" on macOS, "Ctrl+Shift+P" elsewhere. */
export function formatKeybinding(binding: string): string {
  return binding
    .trim()
    .split(/\s+/)
    .map((press) => {
      const { mods, key } = splitPress(press)
      const sorted = [...new Set(mods)].sort((a, b) => MOD_ORDER.indexOf(a) - MOD_ORDER.indexOf(b))
      if (isMac) return sorted.map((m) => MAC_MODS[m] ?? m).join('') + keyLabel(key)
      return [...sorted.map((m) => PC_MODS[m] ?? m), keyLabel(key)].join('+')
    })
    .join(' ')
}

/** Canonical form used for equality / conflict detection. */
export function normalizeKeybinding(binding: string): string {
  return binding
    .trim()
    .split(/\s+/)
    .map((press) => {
      const { mods, key } = splitPress(press)
      let k = key.toLowerCase()
      if (/^key[a-z]$/.test(k)) k = k.slice(3)
      else if (/^digit\d$/.test(k)) k = k.slice(5)
      else {
        const named = Object.entries(KEY_NAMES).find(([code]) => code === k)
        if (named && named[1].length === 1 && !/^[a-z]$/i.test(named[1])) k = named[1]
      }
      const sorted = [...new Set(mods)].sort((a, b) => MOD_ORDER.indexOf(a) - MOD_ORDER.indexOf(b))
      return [...sorted, k].join('+')
    })
    .join(' ')
}

/**
 * Convert a keydown event into a binding string (for "record shortcut"). Uses `$mod` for the platform's primary
 * modifier so recorded bindings work across macOS and Windows/Linux. Returns null for lone modifier presses.
 */
export function eventToKeybinding(e: KeyboardEvent): string | null {
  if (['Shift', 'Control', 'Alt', 'Meta', 'CapsLock', 'Dead'].includes(e.key)) return null
  const mods: string[] = []
  const primary = isMac ? e.metaKey : e.ctrlKey
  if (primary) mods.push('$mod')
  if (isMac ? e.ctrlKey : e.metaKey) mods.push(isMac ? 'Control' : 'Meta')
  if (e.altKey) mods.push('Alt')
  if (e.shiftKey) mods.push('Shift')
  let key: string
  if (/^Key[A-Z]$/.test(e.code)) key = e.code.slice(3).toLowerCase()
  else if (/^Digit\d$/.test(e.code)) key = e.code.slice(5)
  else if (/^(Comma|Period|Slash|Backslash|Semicolon|Quote|BracketLeft|BracketRight|Minus|Equal|Backquote)$/.test(e.code))
    key = e.code
  else if (e.key === ' ') key = 'Space'
  else key = e.key
  return [...mods, key].join('+')
}


/** Does `binding` stay active inside keyboard owners (Monaco…) for a command with this `essential` flag? */
export function essentialBinding(essential: boolean | string[] | undefined, binding: string): boolean {
  if (!essential) return false
  if (essential === true) return true
  const b = normalizeKeybinding(binding)
  return essential.some((e) => normalizeKeybinding(e) === b)
}
