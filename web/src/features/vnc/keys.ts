/*
 * X11 keysyms and key combinations for the send-keys menu (GFX-3), plus text → keysym mapping for "type text as
 * keystrokes" (BIOS / iLO / iDRAC consoles without clipboard support). noVNC only exports RFB, so the keysyms used
 * here are defined locally (values from X11 keysymdef.h). Each key carries its KeyboardEvent.code so servers with the
 * QEMU extended key event get real scancodes.
 */

export interface Key {
  keysym: number
  code: string | null
}

export const K = {
  ControlL: { keysym: 0xffe3, code: 'ControlLeft' },
  AltL: { keysym: 0xffe9, code: 'AltLeft' },
  ShiftL: { keysym: 0xffe1, code: 'ShiftLeft' },
  SuperL: { keysym: 0xffeb, code: 'MetaLeft' },
  Delete: { keysym: 0xffff, code: 'Delete' },
  BackSpace: { keysym: 0xff08, code: 'Backspace' },
  Tab: { keysym: 0xff09, code: 'Tab' },
  Return: { keysym: 0xff0d, code: 'Enter' },
  Escape: { keysym: 0xff1b, code: 'Escape' },
  Print: { keysym: 0xff61, code: 'PrintScreen' },
  Pause: { keysym: 0xff13, code: 'Pause' },
  Insert: { keysym: 0xff63, code: 'Insert' },
  Home: { keysym: 0xff50, code: 'Home' },
  End: { keysym: 0xff57, code: 'End' },
  PageUp: { keysym: 0xff55, code: 'PageUp' },
  PageDown: { keysym: 0xff56, code: 'PageDown' },
  Left: { keysym: 0xff51, code: 'ArrowLeft' },
  Up: { keysym: 0xff52, code: 'ArrowUp' },
  Right: { keysym: 0xff53, code: 'ArrowRight' },
  Down: { keysym: 0xff54, code: 'ArrowDown' },
  Menu: { keysym: 0xff67, code: 'ContextMenu' },
  R: { keysym: 0x0072, code: 'KeyR' },
  L: { keysym: 0x006c, code: 'KeyL' },
  E: { keysym: 0x0065, code: 'KeyE' },
  D: { keysym: 0x0064, code: 'KeyD' },
} satisfies Record<string, Key>

/** F1..F12 */
export function fKey(n: number): Key {
  return { keysym: 0xffbe + (n - 1), code: `F${n}` }
}

/** Modifiers that can be held down from the toolbar (sticky keys for touch devices). */
export type HeldModifier = 'ctrl' | 'alt' | 'shift' | 'super'

export const MODIFIER_KEYS: Record<HeldModifier, Key> = {
  ctrl: K.ControlL,
  alt: K.AltL,
  shift: K.ShiftL,
  super: K.SuperL,
}

export const MODIFIER_LABELS: Record<HeldModifier, string> = {
  ctrl: 'Ctrl',
  alt: 'Alt',
  shift: 'Shift',
  super: 'Win / Super',
}

export interface Combo {
  id: string
  label: string
  /** Pressed in order, released in reverse order. */
  keys: Key[]
}

export const COMBOS: Combo[] = [
  { id: 'ctrl-esc', label: 'Ctrl+Esc', keys: [K.ControlL, K.Escape] },
  { id: 'alt-tab', label: 'Alt+Tab', keys: [K.AltL, K.Tab] },
  { id: 'alt-shift-tab', label: 'Alt+Shift+Tab', keys: [K.AltL, K.ShiftL, K.Tab] },
  { id: 'alt-f4', label: 'Alt+F4', keys: [K.AltL, fKey(4)] },
  { id: 'win', label: 'Win', keys: [K.SuperL] },
  { id: 'win-r', label: 'Win+R', keys: [K.SuperL, K.R] },
  { id: 'win-e', label: 'Win+E', keys: [K.SuperL, K.E] },
  { id: 'win-d', label: 'Win+D', keys: [K.SuperL, K.D] },
  { id: 'win-l', label: 'Win+L', keys: [K.SuperL, K.L] },
  { id: 'print', label: 'PrintScreen', keys: [K.Print] },
  { id: 'alt-print', label: 'Alt+PrintScreen', keys: [K.AltL, K.Print] },
  { id: 'ctrl-alt-backspace', label: 'Ctrl+Alt+Backspace', keys: [K.ControlL, K.AltL, K.BackSpace] },
  { id: 'escape', label: 'Esc', keys: [K.Escape] },
  { id: 'menu', label: 'Menu', keys: [K.Menu] },
]

export const F_KEYS: Combo[] = Array.from({ length: 12 }, (_, i) => ({ id: `f${i + 1}`, label: `F${i + 1}`, keys: [fKey(i + 1)] }))

/** Ctrl+Alt+F1..F12: switch virtual terminals on Linux consoles. */
export const CTRL_ALT_F_KEYS: Combo[] = Array.from({ length: 12 }, (_, i) => ({
  id: `ctrl-alt-f${i + 1}`,
  label: `Ctrl+Alt+F${i + 1}`,
  keys: [K.ControlL, K.AltL, fKey(i + 1)],
}))

export function findCombo(id: string): Combo | undefined {
  return [...COMBOS, ...F_KEYS, ...CTRL_ALT_F_KEYS].find((c) => c.id === id)
}

/**
 * Keysym for a character typed as text: Latin-1 maps directly, control characters to their keys, everything else to
 * the Unicode keysym range (0x01000000 + code point) that X servers and TigerVNC accept.
 */
export function charKey(ch: string): Key | null {
  const cp = ch.codePointAt(0)
  if (cp === undefined) return null
  switch (ch) {
    case '\n':
    case '\r':
      return K.Return
    case '\t':
      return K.Tab
    case '\b':
      return K.BackSpace
    case '\x1b':
      return K.Escape
  }
  if (cp < 0x20 || cp === 0x7f || (cp >= 0x80 && cp < 0xa0)) return null
  if (cp <= 0xff) return { keysym: cp, code: null }
  return { keysym: 0x01000000 + cp, code: null }
}

/** Split text into typeable keys (CRLF counts once); characters that cannot be typed are skipped. */
export function textToKeys(text: string): Key[] {
  const out: Key[] = []
  const normalized = text.replace(/\r\n?/g, '\n')
  for (const ch of normalized) {
    const k = charKey(ch)
    if (k) out.push(k)
  }
  return out
}
