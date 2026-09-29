/*
 * Special key combinations (GFX-3): each combination is described by KeyboardEvent.code values (the IronRDP web
 * component maps codes to scancodes) and X11 keysyms (guacd). Keys are pressed in order and released in reverse.
 */
import type { KeyCombo } from './types'

const K = {
  ControlLeft: 0xffe3,
  ShiftLeft: 0xffe1,
  AltLeft: 0xffe9,
  MetaLeft: 0xffeb, // Super_L: the Windows key
  Delete: 0xffff,
  Tab: 0xff09,
  Escape: 0xff1b,
  Enter: 0xff0d,
  Backspace: 0xff08,
  PrintScreen: 0xff61,
  ContextMenu: 0xff67,
  Home: 0xff50,
  End: 0xff57,
} as const

const fkey = (n: number) => 0xffbe + n - 1
const letter = (c: string) => c.toLowerCase().charCodeAt(0)

function combo(id: string, label: string, keys: Array<[code: string, keysym: number]>): KeyCombo {
  return { id, label, codes: keys.map((k) => k[0]), keysyms: keys.map((k) => k[1]) }
}

const ctrl: [string, number] = ['ControlLeft', K.ControlLeft]
const alt: [string, number] = ['AltLeft', K.AltLeft]
const shift: [string, number] = ['ShiftLeft', K.ShiftLeft]
const win: [string, number] = ['MetaLeft', K.MetaLeft]

export const CTRL_ALT_DEL = combo('ctrl-alt-del', 'Ctrl+Alt+Del', [ctrl, alt, ['Delete', K.Delete]])

/** Combinations offered in the "Send keys" menu. */
export const KEY_COMBOS: KeyCombo[] = [
  CTRL_ALT_DEL,
  combo('ctrl-shift-esc', 'Ctrl+Shift+Esc (Task Manager)', [ctrl, shift, ['Escape', K.Escape]]),
  combo('win', 'Windows key', [win]),
  combo('win-l', 'Win+L (lock)', [win, ['KeyL', letter('l')]]),
  combo('win-r', 'Win+R (run)', [win, ['KeyR', letter('r')]]),
  combo('win-d', 'Win+D (desktop)', [win, ['KeyD', letter('d')]]),
  combo('win-e', 'Win+E (explorer)', [win, ['KeyE', letter('e')]]),
  combo('alt-tab', 'Alt+Tab', [alt, ['Tab', K.Tab]]),
  combo('alt-f4', 'Alt+F4', [alt, ['F4', fkey(4)]]),
  combo('ctrl-esc', 'Ctrl+Esc (Start menu)', [ctrl, ['Escape', K.Escape]]),
  combo('print', 'Print Screen', [['PrintScreen', K.PrintScreen]]),
  combo('alt-print', 'Alt+Print Screen', [alt, ['PrintScreen', K.PrintScreen]]),
  combo('menu', 'Context menu key', [['ContextMenu', K.ContextMenu]]),
  combo('esc', 'Esc', [['Escape', K.Escape]]),
]

/** Ctrl+Alt+F1…F12 (Linux virtual consoles, BIOS / iLO consoles). */
export const CTRL_ALT_FN: KeyCombo[] = Array.from({ length: 12 }, (_, i) =>
  combo(`ctrl-alt-f${i + 1}`, `Ctrl+Alt+F${i + 1}`, [ctrl, alt, [`F${i + 1}`, fkey(i + 1)]]),
)

export function findCombo(id: string): KeyCombo | undefined {
  return [...KEY_COMBOS, ...CTRL_ALT_FN].find((c) => c.id === id)
}

/** X11 keysym typing a character (null for characters that cannot be typed). */
export function keysymForChar(ch: string): number | null {
  const cp = ch.codePointAt(0)
  if (cp === undefined) return null
  if (ch === '\n' || ch === '\r') return K.Enter
  if (ch === '\t') return K.Tab
  if (ch === '\b') return K.Backspace
  if (cp < 0x20 || cp === 0x7f) return null
  if (cp <= 0xff) return cp
  return 0x01000000 | cp
}

/** Longest text typed as keystrokes (typing is paced, so very long texts would take minutes). */
export const MAX_TYPED_TEXT = 20_000
