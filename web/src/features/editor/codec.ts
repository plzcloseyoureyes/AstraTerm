/*
 * Byte/text codecs for the editor: text encodings (decode + encode, incl. single- and multi-byte legacy code pages),
 * BOMs, line endings, base64 and binary sniffing. Pure functions, no DOM beyond TextDecoder/TextEncoder.
 */
import { encodeMultiByte, MULTIBYTE_ENCODINGS, UnencodableError } from './cjk'

// ---------------------------------------------------------------------------------------------------------------------
// line endings
// ---------------------------------------------------------------------------------------------------------------------

export type Eol = 'lf' | 'crlf' | 'cr'

export const EOL_SEQ: Record<Eol, string> = { lf: '\n', crlf: '\r\n', cr: '\r' }
export const EOL_LABEL: Record<Eol, string> = { lf: 'LF', crlf: 'CRLF', cr: 'CR' }

export interface EolInfo {
  eol: Eol
  /** More than one kind of line ending was found (saving normalises to `eol`). */
  mixed: boolean
  counts: Record<Eol, number>
}

/**
 * Every line break of raw text in order: its position in the "\n"-joined text (the editor model) and its kind.
 * indexOf-based: much faster than a per-character loop on large files.
 */
export function eachBreak(raw: string, fn: (pos: number, eol: Eol) => void): void {
  let removed = 0
  for (let i = raw.indexOf('\r'), j = raw.indexOf('\n'); i >= 0 || j >= 0; ) {
    // next break: the earlier of the next CR and LF
    if (i >= 0 && (j < 0 || i < j)) {
      if (raw.charCodeAt(i + 1) === 10) {
        fn(i - removed, 'crlf')
        removed++
        j = raw.indexOf('\n', i + 2)
        i = raw.indexOf('\r', i + 2)
      } else {
        fn(i - removed, 'cr')
        i = raw.indexOf('\r', i + 1)
      }
    } else {
      fn(j - removed, 'lf')
      j = raw.indexOf('\n', j + 1)
    }
  }
}

/** Detect the dominant line ending (ties and files without line breaks use `fallback`). */
export function detectEol(text: string, fallback: Eol = 'lf'): EolInfo {
  const counts: Record<Eol, number> = { lf: 0, crlf: 0, cr: 0 }
  eachBreak(text, (_pos, eol) => {
    counts[eol]++
  })
  const kinds = (Object.keys(counts) as Eol[]).filter((k) => counts[k] > 0)
  if (!kinds.length) return { eol: fallback, mixed: false, counts }
  let eol = kinds[0]
  for (const k of kinds) if (counts[k] > counts[eol]) eol = k
  if (counts[fallback] === counts[eol]) eol = fallback
  return { eol, mixed: kinds.length > 1, counts }
}

// ---------------------------------------------------------------------------------------------------------------------
// encodings
// ---------------------------------------------------------------------------------------------------------------------

export interface EncodingInfo {
  id: string
  label: string
  /** Group in pickers. */
  group: 'Unicode' | 'Western' | 'Central & Eastern European' | 'Cyrillic' | 'Other' | 'East Asian'
  /** Can be written back. */
  canEncode: boolean
}

export const ENCODINGS: EncodingInfo[] = [
  { id: 'utf-8', label: 'UTF-8', group: 'Unicode', canEncode: true },
  { id: 'utf-16le', label: 'UTF-16 LE', group: 'Unicode', canEncode: true },
  { id: 'utf-16be', label: 'UTF-16 BE', group: 'Unicode', canEncode: true },
  { id: 'windows-1252', label: 'Windows-1252 (Western)', group: 'Western', canEncode: true },
  { id: 'iso-8859-1', label: 'ISO-8859-1 (Latin-1)', group: 'Western', canEncode: true },
  { id: 'iso-8859-15', label: 'ISO-8859-15 (Latin-9)', group: 'Western', canEncode: true },
  { id: 'macintosh', label: 'Mac Roman', group: 'Western', canEncode: true },
  { id: 'ibm866', label: 'DOS 866 (Cyrillic)', group: 'Cyrillic', canEncode: true },
  { id: 'windows-1250', label: 'Windows-1250 (Central European)', group: 'Central & Eastern European', canEncode: true },
  { id: 'iso-8859-2', label: 'ISO-8859-2 (Central European)', group: 'Central & Eastern European', canEncode: true },
  { id: 'windows-1257', label: 'Windows-1257 (Baltic)', group: 'Central & Eastern European', canEncode: true },
  { id: 'windows-1251', label: 'Windows-1251 (Cyrillic)', group: 'Cyrillic', canEncode: true },
  { id: 'koi8-r', label: 'KOI8-R (Cyrillic)', group: 'Cyrillic', canEncode: true },
  { id: 'koi8-u', label: 'KOI8-U (Ukrainian)', group: 'Cyrillic', canEncode: true },
  { id: 'iso-8859-5', label: 'ISO-8859-5 (Cyrillic)', group: 'Cyrillic', canEncode: true },
  { id: 'windows-1253', label: 'Windows-1253 (Greek)', group: 'Other', canEncode: true },
  { id: 'iso-8859-7', label: 'ISO-8859-7 (Greek)', group: 'Other', canEncode: true },
  { id: 'windows-1254', label: 'Windows-1254 (Turkish)', group: 'Other', canEncode: true },
  { id: 'windows-1255', label: 'Windows-1255 (Hebrew)', group: 'Other', canEncode: true },
  { id: 'iso-8859-8', label: 'ISO-8859-8 (Hebrew)', group: 'Other', canEncode: true },
  { id: 'windows-1256', label: 'Windows-1256 (Arabic)', group: 'Other', canEncode: true },
  { id: 'windows-874', label: 'Windows-874 (Thai)', group: 'Other', canEncode: true },
  { id: 'windows-1258', label: 'Windows-1258 (Vietnamese)', group: 'Other', canEncode: true },
  { id: 'shift_jis', label: 'Shift JIS', group: 'East Asian', canEncode: true },
  { id: 'euc-jp', label: 'EUC-JP', group: 'East Asian', canEncode: true },
  { id: 'gbk', label: 'GBK (Simplified Chinese)', group: 'East Asian', canEncode: true },
  { id: 'gb18030', label: 'GB18030', group: 'East Asian', canEncode: true },
  { id: 'big5', label: 'Big5 (Traditional Chinese)', group: 'East Asian', canEncode: true },
  { id: 'euc-kr', label: 'EUC-KR (Korean)', group: 'East Asian', canEncode: true },
]

const BY_ID = new Map(ENCODINGS.map((e) => [e.id, e]))

export function encodingInfo(id: string): EncodingInfo {
  return BY_ID.get(id) ?? { id, label: id.toUpperCase(), group: 'Other', canEncode: false }
}

/** Status bar label, e.g. "UTF-8 with BOM". */
export function encodingLabel(id: string, bom: boolean): string {
  const base = id === 'utf-8' ? 'UTF-8' : id === 'utf-16le' ? 'UTF-16 LE' : id === 'utf-16be' ? 'UTF-16 BE' : encodingInfo(id).label.replace(/ \(.*\)$/, '')
  return bom ? `${base} with BOM` : base
}

export function hasBomSupport(id: string): boolean {
  return id === 'utf-8' || id === 'utf-16le' || id === 'utf-16be'
}

export interface Decoded {
  text: string
  bom: boolean
}

/** Decode bytes with an encoding; a matching BOM is stripped and reported. */
export function decodeBytes(bytes: Uint8Array, encoding: string): Decoded {
  if (encoding === 'utf-8') {
    const bom = bytes.length >= 3 && bytes[0] === 0xef && bytes[1] === 0xbb && bytes[2] === 0xbf
    return { text: new TextDecoder('utf-8', { ignoreBOM: true }).decode(bom ? bytes.subarray(3) : bytes), bom }
  }
  if (encoding === 'utf-16le' || encoding === 'utf-16be') {
    const le = encoding === 'utf-16le'
    const bom = bytes.length >= 2 && (le ? bytes[0] === 0xff && bytes[1] === 0xfe : bytes[0] === 0xfe && bytes[1] === 0xff)
    return { text: new TextDecoder(encoding, { ignoreBOM: true }).decode(bom ? bytes.subarray(2) : bytes), bom }
  }
  if (encoding === 'iso-8859-1') {
    // WHATWG maps "iso-8859-1" to windows-1252; real Latin-1 maps every byte to the same code point.
    let s = ''
    for (let i = 0; i < bytes.length; i += 0x8000) s += String.fromCharCode.apply(null, Array.from(bytes.subarray(i, i + 0x8000)))
    return { text: s, bom: false }
  }
  return { text: new TextDecoder(encoding).decode(bytes), bom: false }
}

export class EncodeError extends Error {
  readonly index: number
  readonly char: string
  constructor(encoding: string, index: number, char: string) {
    super(
      index < 0
        ? `${encodingInfo(encoding).label} cannot be written by AstraTerm. Save as UTF-8 instead.`
        : `The character "${char}" (U+${char.codePointAt(0)?.toString(16).toUpperCase().padStart(4, '0')}) cannot be represented in ${encodingInfo(encoding).label}.`,
    )
    this.name = 'EncodeError'
    this.index = index
    this.char = char
  }
}

const reverseTables = new Map<string, Map<number, number>>()

/** code point → byte for single-byte code pages, derived from the browser's own decoder. */
function reverseTable(encoding: string): Map<number, number> {
  let t = reverseTables.get(encoding)
  if (t) return t
  t = new Map()
  const dec = new TextDecoder(encoding)
  for (let b = 0; b < 256; b++) {
    const ch = dec.decode(Uint8Array.of(b))
    const cp = ch.codePointAt(0)
    if (ch.length === 1 && cp !== undefined && cp !== 0xfffd && !t.has(cp)) t.set(cp, b)
  }
  reverseTables.set(encoding, t)
  return t
}

/** Encode text (line endings already applied) into bytes. Throws EncodeError for unrepresentable characters. */
export function encodeText(text: string, encoding: string, bom: boolean): Uint8Array {
  if (encoding === 'utf-8') {
    const body = new TextEncoder().encode(text)
    if (!bom) return body
    const out = new Uint8Array(body.length + 3)
    out.set([0xef, 0xbb, 0xbf])
    out.set(body, 3)
    return out
  }
  if (encoding === 'utf-16le' || encoding === 'utf-16be') {
    const le = encoding === 'utf-16le'
    const off = bom ? 2 : 0
    const out = new Uint8Array(off + text.length * 2)
    if (bom) out.set(le ? [0xff, 0xfe] : [0xfe, 0xff])
    for (let i = 0; i < text.length; i++) {
      const c = text.charCodeAt(i)
      const p = off + i * 2
      if (le) {
        out[p] = c & 0xff
        out[p + 1] = c >> 8
      } else {
        out[p] = c >> 8
        out[p + 1] = c & 0xff
      }
    }
    return out
  }
  if (!encodingInfo(encoding).canEncode) throw new EncodeError(encoding, -1, '')
  if (MULTIBYTE_ENCODINGS.has(encoding)) {
    try {
      return encodeMultiByte(text, encoding)
    } catch (err) {
      if (!(err instanceof UnencodableError)) throw err
      throw new EncodeError(encoding, err.index, String.fromCodePoint(text.codePointAt(err.index) ?? 0xfffd))
    }
  }
  const out = new Uint8Array(text.length)
  if (encoding === 'iso-8859-1') {
    for (let i = 0; i < text.length; i++) {
      const c = text.charCodeAt(i)
      if (c > 0xff) throw new EncodeError(encoding, i, String.fromCodePoint(text.codePointAt(i) ?? c))
      out[i] = c
    }
    return out
  }
  const table = reverseTable(encoding)
  for (let i = 0; i < text.length; i++) {
    const c = text.charCodeAt(i)
    const b = table.get(c)
    if (b === undefined) throw new EncodeError(encoding, i, String.fromCodePoint(text.codePointAt(i) ?? c))
    out[i] = b
  }
  return out
}

/** Would `text` survive a round trip through `encoding`? Returns the first offending index or -1. */
export function findUnencodable(text: string, encoding: string): number {
  try {
    encodeText(text, encoding, false)
    return -1
  } catch (err) {
    return err instanceof EncodeError ? Math.max(0, err.index) : 0
  }
}

/**
 * Would saving `text` with `encoding` write exactly `bytes` back? False when decoding replaced bytes (invalid UTF-8,
 * undefined code page bytes...) or the encoding cannot be written; unchanged documents must never alter a file.
 */
export function roundTrips(bytes: Uint8Array, text: string, encoding: string, bom: boolean): boolean {
  let out: Uint8Array
  try {
    out = encodeText(text, encoding, bom)
  } catch {
    return false
  }
  if (out.length !== bytes.length) return false
  for (let i = 0; i < out.length; i++) if (out[i] !== bytes[i]) return false
  return true
}

export function isValidUtf8(bytes: Uint8Array): boolean {
  try {
    new TextDecoder('utf-8', { fatal: true }).decode(bytes)
    return true
  } catch {
    return false
  }
}

/** Number of bytes of `s` in UTF-8 (without allocating). */
export function utf8ByteLength(s: string): number {
  let n = 0
  for (let i = 0; i < s.length; i++) {
    const c = s.charCodeAt(i)
    if (c < 0x80) n += 1
    else if (c < 0x800) n += 2
    else if (c >= 0xd800 && c <= 0xdbff && i + 1 < s.length) {
      n += 4
      i++
    } else n += 3
  }
  return n
}

// ---------------------------------------------------------------------------------------------------------------------
// sniffing
// ---------------------------------------------------------------------------------------------------------------------

export type Sniff =
  | { kind: 'text'; encoding: string; reason?: string }
  | { kind: 'binary'; reason: string }

/**
 * Classify raw bytes (the backend returned base64 = not plain UTF-8): UTF-16 with BOM, UTF-8 with BOM, legacy 8-bit text
 * (no NULs, few control characters → Windows-1252), else binary.
 */
export function sniffBytes(bytes: Uint8Array): Sniff {
  if (bytes.length >= 2 && bytes[0] === 0xff && bytes[1] === 0xfe && !(bytes[2] === 0 && bytes[3] === 0)) return { kind: 'text', encoding: 'utf-16le' }
  if (bytes.length >= 2 && bytes[0] === 0xfe && bytes[1] === 0xff) return { kind: 'text', encoding: 'utf-16be' }
  if (bytes.length >= 3 && bytes[0] === 0xef && bytes[1] === 0xbb && bytes[2] === 0xbf && isValidUtf8(bytes)) return { kind: 'text', encoding: 'utf-8' }
  if (bytes.length === 0) return { kind: 'text', encoding: 'utf-8' }
  const sample = bytes.subarray(0, Math.min(bytes.length, 64 * 1024))
  let nul = 0
  let ctrl = 0
  let evenNul = 0
  let oddNul = 0
  for (let i = 0; i < sample.length; i++) {
    const b = sample[i]
    if (b === 0) {
      nul++
      if (i & 1) oddNul++
      else evenNul++
    } else if (b < 0x20 && b !== 9 && b !== 10 && b !== 13 && b !== 12 && b !== 11 && b !== 27 && b !== 8) ctrl++
  }
  // UTF-16 without BOM: ASCII text has a NUL in every other byte.
  if (sample.length >= 16 && nul > sample.length * 0.3) {
    if (oddNul > sample.length * 0.4 && evenNul < sample.length * 0.02) return { kind: 'text', encoding: 'utf-16le', reason: 'UTF-16 LE (no BOM) detected' }
    if (evenNul > sample.length * 0.4 && oddNul < sample.length * 0.02) return { kind: 'text', encoding: 'utf-16be', reason: 'UTF-16 BE (no BOM) detected' }
  }
  if (nul > 0) return { kind: 'binary', reason: 'The file contains NUL bytes.' }
  if (ctrl > sample.length * 0.01) return { kind: 'binary', reason: 'The file contains control characters.' }
  if (isValidUtf8(bytes)) return { kind: 'text', encoding: 'utf-8' }
  return { kind: 'text', encoding: 'windows-1252', reason: 'The file is not valid UTF-8; it was opened as Windows-1252.' }
}

// ---------------------------------------------------------------------------------------------------------------------
// base64
// ---------------------------------------------------------------------------------------------------------------------

type U8Static = typeof Uint8Array & { fromBase64?: (s: string) => Uint8Array }
type U8WithB64 = Uint8Array & { toBase64?: () => string }

export function base64ToBytes(b64: string): Uint8Array {
  const clean = b64.replace(/\s+/g, '')
  const native = (Uint8Array as U8Static).fromBase64
  if (native) {
    try {
      return native(clean)
    } catch {
      /* fall through (non-canonical padding) */
    }
  }
  const bin = atob(clean)
  const out = new Uint8Array(bin.length)
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i)
  return out
}

export function bytesToBase64(bytes: Uint8Array): string {
  const native = (bytes as U8WithB64).toBase64
  if (typeof native === 'function') return native.call(bytes)
  let s = ''
  for (let i = 0; i < bytes.length; i += 0x8000) s += String.fromCharCode.apply(null, Array.from(bytes.subarray(i, i + 0x8000)))
  return btoa(s)
}

export { baseName, dirName, joinPath } from './paths'
