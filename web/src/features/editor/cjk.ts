/*
 * Encoders for the multi-byte legacy code pages (TextEncoder only writes UTF-8): Shift_JIS, EUC-JP, GBK, GB18030,
 * Big5 and EUC-KR. The reverse tables are derived from the platform's own WHATWG decoders (so both directions agree)
 * and follow the WHATWG encoders' choices where a character has several byte sequences. Tables are built on first
 * use of an encoding (a few ms). The editor only writes a file in such an encoding after checking that the loaded
 * text re-encodes to exactly the loaded bytes (codec.roundTrips); files that do not are opened read-only.
 */

type Table = Map<number, number> // code point → bytes (1..2 bytes packed as (b1 << 8) | b2, or a single byte)

const tables = new Map<string, Table>()

function decoder(encoding: string): TextDecoder {
  return new TextDecoder(encoding)
}

/** Decode one byte sequence to a single code point (null when invalid or more than one character). */
function single(dec: TextDecoder, bytes: Uint8Array): number | null {
  const s = dec.decode(bytes)
  const cp = s.codePointAt(0)
  if (cp === undefined || cp === 0xfffd || s.length !== (cp > 0xffff ? 2 : 1)) return null
  return cp
}

interface Layout {
  leads: [number, number][]
  trails: [number, number][]
  /** A pair maps to a code point that also has a preferred sequence (skip it unless nothing else maps there). */
  demote?: (lead: number, trail: number, cp: number) => boolean
  /** Code points whose last sequence wins (WHATWG Big5). */
  preferLast?: Set<number>
}

function inRanges(b: number, ranges: [number, number][]): boolean {
  for (const [lo, hi] of ranges) if (b >= lo && b <= hi) return true
  return false
}

function build(encoding: string, layout: Layout): Table {
  const dec = decoder(encoding)
  const t: Table = new Map()
  const demoted = new Map<number, number>()
  const pair = new Uint8Array(2)
  for (const [lo, hi] of layout.leads) {
    for (let lead = lo; lead <= hi; lead++) {
      for (const [tlo, thi] of layout.trails) {
        for (let trail = tlo; trail <= thi; trail++) {
          pair[0] = lead
          pair[1] = trail
          const cp = single(dec, pair)
          if (cp === null || cp < 0x80) continue
          const packed = (lead << 8) | trail
          if (layout.demote?.(lead, trail, cp)) {
            if (!demoted.has(cp)) demoted.set(cp, packed)
            continue
          }
          if (!t.has(cp) || layout.preferLast?.has(cp)) t.set(cp, packed)
        }
      }
    }
  }
  for (const [cp, packed] of demoted) if (!t.has(cp)) t.set(cp, packed)
  // single bytes above ASCII (half-width katakana, 0x80 = € in GBK, ...)
  for (let b = 0x80; b <= 0xff; b++) {
    if (inRanges(b, layout.leads)) continue
    const cp = single(dec, Uint8Array.of(b))
    if (cp !== null && cp >= 0x80 && !t.has(cp)) t.set(cp, b)
  }
  return t
}

const LAYOUTS: Record<string, () => Layout> = {
  shift_jis: () => ({
    leads: [
      [0x81, 0x9f],
      [0xe0, 0xfc],
    ],
    trails: [
      [0x40, 0x7e],
      [0x80, 0xfc],
    ],
    // WHATWG: the NEC-selected IBM extensions (0xED/0xEE) are never produced; 0xFA–0xFC are.
    demote: (lead) => lead === 0xed || lead === 0xee,
  }),
  'euc-jp': () => ({
    // 0x8E + byte: half-width katakana; JIS X 0212 (0x8F + 2 bytes) is decoded but — like WHATWG — never written
    leads: [
      [0x8e, 0x8e],
      [0xa1, 0xfe],
    ],
    trails: [[0xa1, 0xfe]],
  }),
  gbk: () => ({
    leads: [[0x81, 0xfe]],
    trails: [
      [0x40, 0x7e],
      [0x80, 0xfe],
    ],
  }),
  gb18030: () => ({
    leads: [[0x81, 0xfe]],
    trails: [
      [0x40, 0x7e],
      [0x80, 0xfe],
    ],
  }),
  big5: () => ({
    leads: [[0x81, 0xfe]],
    trails: [
      [0x40, 0x7e],
      [0xa1, 0xfe],
    ],
    // WHATWG: HKSCS sequences (lead < 0xA1) only when the character has no standard Big5 sequence…
    demote: (lead) => lead < 0xa1,
    // …and these six use their last sequence.
    preferLast: new Set([0x2550, 0x255e, 0x2561, 0x256a, 0x5341, 0x5345]),
  }),
  'euc-kr': () => ({
    leads: [[0x81, 0xfe]],
    trails: [[0x41, 0xfe]],
  }),
}

/** Multi-byte encodings AstraTerm can write. */
export const MULTIBYTE_ENCODINGS = new Set(Object.keys(LAYOUTS))

function tableFor(encoding: string): Table {
  let t = tables.get(encoding)
  if (!t) {
    const layout = LAYOUTS[encoding]
    if (!layout) throw new Error(`no encoder for ${encoding}`)
    t = build(encoding, layout())
    // WHATWG GBK writes the euro sign as the single byte 0x80 (GB18030 as 0xA2E3).
    if (encoding === 'gbk') t.set(0x20ac, 0x80)
    tables.set(encoding, t)
  }
  return t
}

// GB18030 four-byte sequences: pointer ↔ bytes, found by binary search through the decoder (the mapping of BMP code
// points without a two-byte sequence is monotonic); supplementary planes are linear from pointer 189000.
function gbFourBytes(pointer: number): Uint8Array {
  const b1 = Math.floor(pointer / 12600)
  const r1 = pointer % 12600
  const b2 = Math.floor(r1 / 1260)
  const r2 = r1 % 1260
  return Uint8Array.of(b1 + 0x81, b2 + 0x30, Math.floor(r2 / 10) + 0x81, (r2 % 10) + 0x30)
}

const gbCache = new Map<number, Uint8Array | null>()

function gb18030Four(cp: number): Uint8Array | null {
  if (cp >= 0x10000) return cp <= 0x10ffff ? gbFourBytes(189000 + cp - 0x10000) : null
  const hit = gbCache.get(cp)
  if (hit !== undefined) return hit
  const dec = decoder('gb18030')
  let lo = 0
  let hi = 39419
  let found: Uint8Array | null = null
  while (lo <= hi) {
    const mid = (lo + hi) >> 1
    const bytes = gbFourBytes(mid)
    const got = single(dec, bytes)
    if (got === null) {
      // unassigned pointer inside the range table: step over it
      hi = mid - 1
      continue
    }
    if (got === cp) {
      found = bytes
      break
    }
    if (got < cp) lo = mid + 1
    else hi = mid - 1
  }
  if (!found) {
    // the binary search can be thrown off by unassigned pointers: fall back to a scan around the last position
    for (let p = Math.max(0, lo - 64); p <= Math.min(39419, lo + 64); p++) {
      const bytes = gbFourBytes(p)
      if (single(dec, bytes) === cp) {
        found = bytes
        break
      }
    }
  }
  gbCache.set(cp, found)
  return found
}

export class UnencodableError extends Error {
  readonly index: number
  constructor(index: number) {
    super('unencodable')
    this.index = index
  }
}

/** Encode text with a multi-byte legacy code page. Throws UnencodableError at the first character it cannot write. */
export function encodeMultiByte(text: string, encoding: string): Uint8Array {
  const t = tableFor(encoding)
  const out = new Uint8Array(text.length * 4)
  let n = 0
  for (let i = 0; i < text.length; i++) {
    const cp = text.codePointAt(i) ?? 0
    const wide = cp > 0xffff
    if (cp < 0x80) {
      out[n++] = cp
    } else {
      const packed = t.get(cp)
      if (packed !== undefined) {
        if (packed > 0xff) out[n++] = packed >> 8
        out[n++] = packed & 0xff
      } else if (encoding === 'gb18030') {
        const four = gb18030Four(cp)
        if (!four) throw new UnencodableError(i)
        out.set(four, n)
        n += 4
      } else {
        throw new UnencodableError(i)
      }
    }
    if (wide) i++
  }
  return out.slice(0, n)
}
