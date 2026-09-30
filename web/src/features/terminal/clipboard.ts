/*
 * Clipboard helpers bound to a specific window (terminals can live in dockview pop-out windows, whose document must
 * be the focused one for the async clipboard API), plus the in-memory clipboard history (CC-20).
 */
import { create } from 'zustand'

/** Write text to the clipboard of `win`. Falls back to execCommand('copy') when the async API is refused. */
export async function writeClipboard(text: string, win: Window = window, html?: string): Promise<boolean> {
  const nav = win.navigator
  try {
    if (html && typeof (win as typeof window).ClipboardItem === 'function' && nav.clipboard?.write) {
      const Item = (win as typeof window).ClipboardItem
      await nav.clipboard.write([
        new Item({
          'text/plain': new Blob([text], { type: 'text/plain' }),
          'text/html': new Blob([html], { type: 'text/html' }),
        }),
      ])
      return true
    }
    if (nav.clipboard?.writeText) {
      await nav.clipboard.writeText(text)
      return true
    }
  } catch {
    /* fall through to the legacy path */
  }
  return legacyCopy(text, win.document)
}

function legacyCopy(text: string, doc: Document): boolean {
  const active = doc.activeElement as HTMLElement | null
  const ta = doc.createElement('textarea')
  ta.value = text
  ta.setAttribute('readonly', '')
  ta.style.position = 'fixed'
  ta.style.top = '-1000px'
  ta.style.opacity = '0'
  doc.body.appendChild(ta)
  ta.select()
  let ok = false
  try {
    ok = doc.execCommand('copy')
  } catch {
    ok = false
  }
  ta.remove()
  try {
    active?.focus({ preventScroll: true })
  } catch {
    /* ignore */
  }
  return ok
}

export type ClipboardReadResult = { ok: true; text: string } | { ok: false; reason: 'denied' | 'unsupported' | 'error' }

/** Read clipboard text of `win` (asks the browser for permission the first time). */
export async function readClipboard(win: Window = window): Promise<ClipboardReadResult> {
  const clip = win.navigator.clipboard
  if (!clip?.readText) return { ok: false, reason: 'unsupported' }
  try {
    return { ok: true, text: await clip.readText() }
  } catch (err) {
    const name = err instanceof DOMException ? err.name : ''
    return { ok: false, reason: name === 'NotAllowedError' || name === 'SecurityError' ? 'denied' : 'error' }
  }
}

// ---------------------------------------------------------------------------------------------------------------------
// Clipboard history (CC-20): recent copies from terminals, in memory only.
// ---------------------------------------------------------------------------------------------------------------------

export interface ClipboardEntry {
  id: number
  text: string
  at: number
  /** Where it came from: selection copy, OSC 52 (remote application), copy-all... */
  source: 'copy' | 'osc52' | 'select'
  /** Tab title at the time of the copy. */
  origin?: string
}

const MAX_ENTRIES = 50
/** Entries larger than this are not kept (memory). */
const MAX_ENTRY_CHARS = 1_000_000

interface ClipboardHistoryStore {
  entries: ClipboardEntry[]
}

export const useClipboardHistory = create<ClipboardHistoryStore>(() => ({ entries: [] }))

let seq = 0

/** Remember a copied text (deduplicated: an identical entry moves to the top). */
export function pushClipboardHistory(text: string, source: ClipboardEntry['source'], origin?: string): void {
  if (!text || !text.trim() || text.length > MAX_ENTRY_CHARS) return
  useClipboardHistory.setState((s) => {
    const rest = s.entries.filter((e) => e.text !== text)
    return { entries: [{ id: ++seq, text, at: Date.now(), source, origin }, ...rest].slice(0, MAX_ENTRIES) }
  })
}

export function removeClipboardEntry(id: number): void {
  useClipboardHistory.setState((s) => ({ entries: s.entries.filter((e) => e.id !== id) }))
}

export function clearClipboardHistory(): void {
  useClipboardHistory.setState({ entries: [] })
}
