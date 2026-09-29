/*
 * Colour scheme import / export (TERM-9):
 *   - AstraTerm JSON (a TerminalScheme or an array of them)
 *   - Windows Terminal scheme JSON ({name, background, foreground, black, ..., brightWhite, cursorColor, selectionBackground},
 *     also a full settings.json with a "schemes" array)
 *   - iTerm2 .itermcolors (XML property list with "Ansi 0 Color" … "Ansi 15 Color" dictionaries)
 */
import { ANSI_KEYS, type AnsiKey, type TerminalScheme } from './types'
import { newSchemeId, normalizeScheme, rgbToHex, toHex } from './themes'

export class SchemeImportError extends Error {}

/** Parse a file's text into one or more schemes (format auto-detected). Throws SchemeImportError. */
export function parseSchemeFile(text: string, fileName = ''): TerminalScheme[] {
  const trimmed = text.replace(/^\ufeff/, '').trim()
  if (!trimmed) throw new SchemeImportError('The file is empty.')
  if (trimmed.startsWith('<')) return [parseItermColors(trimmed, baseName(fileName))]
  let data: unknown
  try {
    data = JSON.parse(trimmed)
  } catch {
    throw new SchemeImportError('Not a JSON or .itermcolors file.')
  }
  const list = Array.isArray(data)
    ? data
    : data && typeof data === 'object' && Array.isArray((data as { schemes?: unknown }).schemes)
      ? (data as { schemes: unknown[] }).schemes
      : [data]
  const out: TerminalScheme[] = []
  for (const item of list) {
    const s = fromJsonObject(item, baseName(fileName))
    if (s) out.push(s)
  }
  if (!out.length) throw new SchemeImportError('No colour scheme found in the file.')
  return out
}

function baseName(file: string): string {
  const b = file.split(/[\\/]/).pop() ?? ''
  return b.replace(/\.(json|itermcolors|xml)$/i, '') || 'Imported scheme'
}

const WT_KEYS: Record<AnsiKey, string[]> = {
  black: ['black'],
  red: ['red'],
  green: ['green'],
  yellow: ['yellow'],
  blue: ['blue'],
  magenta: ['magenta', 'purple'],
  cyan: ['cyan'],
  white: ['white'],
  brightBlack: ['brightBlack'],
  brightRed: ['brightRed'],
  brightGreen: ['brightGreen'],
  brightYellow: ['brightYellow'],
  brightBlue: ['brightBlue'],
  brightMagenta: ['brightMagenta', 'brightPurple'],
  brightCyan: ['brightCyan'],
  brightWhite: ['brightWhite'],
}

function pick(o: Record<string, unknown>, keys: string[]): string | undefined {
  for (const k of keys) {
    const v = o[k]
    if (typeof v === 'string') {
      const h = toHex(v)
      if (h) return h
    }
  }
  return undefined
}

function fromJsonObject(item: unknown, fallbackName: string): TerminalScheme | null {
  if (!item || typeof item !== 'object') return null
  const o = item as Record<string, unknown>
  const background = pick(o, ['background', 'backgroundColor'])
  const foreground = pick(o, ['foreground', 'foregroundColor'])
  if (!background || !foreground) return null
  const ansi: Partial<Record<AnsiKey, string>> = {}
  let found = 0
  for (const k of ANSI_KEYS) {
    const v = pick(o, WT_KEYS[k])
    if (v) {
      ansi[k] = v
      found++
    }
  }
  if (found < 8) return null
  const name = typeof o.name === 'string' && o.name.trim() ? o.name.trim().slice(0, 80) : fallbackName
  return normalizeScheme({
    id: newSchemeId(),
    name,
    background,
    foreground,
    cursor: pick(o, ['cursor', 'cursorColor']),
    cursorAccent: pick(o, ['cursorAccent', 'cursorTextColor']),
    selectionBackground: pick(o, ['selectionBackground', 'selection']),
    selectionForeground: pick(o, ['selectionForeground']),
    ...(ansi as Record<AnsiKey, string>),
  })
}

/** Parse an iTerm2 .itermcolors XML property list. */
export function parseItermColors(xml: string, name = 'Imported scheme'): TerminalScheme {
  let doc: Document
  try {
    doc = new DOMParser().parseFromString(xml, 'application/xml')
  } catch {
    throw new SchemeImportError('The .itermcolors file could not be parsed.')
  }
  if (doc.getElementsByTagName('parsererror').length) throw new SchemeImportError('The .itermcolors file is not valid XML.')
  const root = doc.querySelector('plist > dict')
  if (!root) throw new SchemeImportError('Not an iTerm2 colour preset (missing <plist><dict>).')
  const colors = new Map<string, string>()
  const children = Array.from(root.children)
  for (let i = 0; i < children.length - 1; i++) {
    const key = children[i]
    const val = children[i + 1]
    if (key.tagName !== 'key' || val.tagName !== 'dict') continue
    const hex = parseItermColor(val)
    if (hex) colors.set((key.textContent ?? '').trim(), hex)
  }
  const ansi = {} as Record<AnsiKey, string>
  for (let i = 0; i < 16; i++) {
    const c = colors.get(`Ansi ${i} Color`)
    if (!c) throw new SchemeImportError(`The preset has no "Ansi ${i} Color".`)
    ansi[ANSI_KEYS[i]] = c
  }
  const background = colors.get('Background Color')
  const foreground = colors.get('Foreground Color')
  if (!background || !foreground) throw new SchemeImportError('The preset has no background / foreground colour.')
  return normalizeScheme({
    id: newSchemeId(),
    name,
    background,
    foreground,
    cursor: colors.get('Cursor Color'),
    cursorAccent: colors.get('Cursor Text Color'),
    selectionBackground: colors.get('Selection Color'),
    selectionForeground: colors.get('Selected Text Color'),
    ...ansi,
  })
}

function parseItermColor(dict: Element): string | undefined {
  const comps: Record<string, number> = {}
  const kids = Array.from(dict.children)
  for (let i = 0; i < kids.length - 1; i++) {
    if (kids[i].tagName !== 'key') continue
    const v = kids[i + 1]
    if (v.tagName === 'real' || v.tagName === 'integer') {
      const n = Number((v.textContent ?? '').trim())
      if (Number.isFinite(n)) comps[(kids[i].textContent ?? '').trim()] = n
    }
  }
  const r = comps['Red Component']
  const g = comps['Green Component']
  const b = comps['Blue Component']
  if (r === undefined || g === undefined || b === undefined) return undefined
  // Components are 0..1 floats (sRGB or calibrated; treated as sRGB).
  return rgbToHex(r * 255, g * 255, b * 255)
}

/** Export as Windows-Terminal-compatible JSON (also re-importable by AstraTerm). */
export function schemeToJson(s: TerminalScheme): string {
  const n = normalizeScheme(s)
  const out: Record<string, string> = {
    name: n.name,
    background: n.background,
    foreground: n.foreground,
    cursorColor: n.cursor ?? n.foreground,
    selectionBackground: n.selectionBackground ?? n.foreground,
  }
  if (n.cursorAccent) out.cursorAccent = n.cursorAccent
  if (n.selectionForeground) out.selectionForeground = n.selectionForeground
  for (const k of ANSI_KEYS) out[k === 'magenta' ? 'purple' : k === 'brightMagenta' ? 'brightPurple' : k] = n[k]
  return JSON.stringify(out, null, 2)
}

/** Export as an iTerm2 .itermcolors property list. */
export function schemeToItermColors(s: TerminalScheme): string {
  const n = normalizeScheme(s)
  const entry = (key: string, hex: string | undefined) => {
    if (!hex) return ''
    const v = parseInt(hex.slice(1), 16)
    const c = (x: number) => ((x & 255) / 255).toFixed(6)
    return `\t<key>${key}</key>\n\t<dict>\n\t\t<key>Alpha Component</key>\n\t\t<real>1</real>\n\t\t<key>Blue Component</key>\n\t\t<real>${c(v)}</real>\n\t\t<key>Color Space</key>\n\t\t<string>sRGB</string>\n\t\t<key>Green Component</key>\n\t\t<real>${c(v >> 8)}</real>\n\t\t<key>Red Component</key>\n\t\t<real>${c(v >> 16)}</real>\n\t</dict>\n`
  }
  let body = ''
  ANSI_KEYS.forEach((k, i) => (body += entry(`Ansi ${i} Color`, n[k])))
  body += entry('Background Color', n.background)
  body += entry('Foreground Color', n.foreground)
  body += entry('Cursor Color', n.cursor)
  body += entry('Cursor Text Color', n.cursorAccent)
  body += entry('Selection Color', n.selectionBackground)
  body += entry('Selected Text Color', n.selectionForeground)
  return `<?xml version="1.0" encoding="UTF-8"?>\n<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">\n<plist version="1.0">\n<dict>\n${body}</dict>\n</plist>\n`
}

/** Offer a text file for download in the given window's document. */
export function downloadText(text: string, fileName: string, mime = 'application/json', win: Window = window): void {
  const blob = new Blob([text], { type: `${mime};charset=utf-8` })
  downloadBlob(blob, fileName, win)
}

export function downloadBlob(blob: Blob, fileName: string, win: Window = window): void {
  const doc = win.document
  const url = URL.createObjectURL(blob)
  const a = doc.createElement('a')
  a.href = url
  a.download = sanitizeFileName(fileName)
  a.rel = 'noopener'
  a.style.display = 'none'
  doc.body.appendChild(a)
  a.click()
  a.remove()
  setTimeout(() => URL.revokeObjectURL(url), 10_000)
}

/** Strip characters that are invalid in file names on common platforms. */
export function sanitizeFileName(name: string): string {
  const cleaned = name
    // oxlint-disable-next-line no-control-regex -- control characters are invalid in file names
    .replace(/[\u0000-\u001f\u007f<>:"/\\|?*]+/g, '_')
    .replace(/\s+/g, ' ')
    .trim()
    .slice(0, 150)
  return cleaned || 'download'
}
