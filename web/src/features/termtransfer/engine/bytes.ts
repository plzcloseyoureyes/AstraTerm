/*
 * Byte helpers shared by the transfer engines (no DOM, no app imports: unit-tested under Node).
 */

export const EMPTY: Uint8Array = new Uint8Array(0)

const utf8 = new TextEncoder()

/** UTF-8 for strings; typed arrays / buffers / octet arrays as bytes (no copy for a plain Uint8Array). */
export function toBytes(data: string | ArrayBuffer | ArrayBufferView | ArrayLike<number>): Uint8Array {
  if (typeof data === 'string') return utf8.encode(data)
  if (data instanceof Uint8Array) return data
  if (data instanceof ArrayBuffer) return new Uint8Array(data)
  if (ArrayBuffer.isView(data)) return new Uint8Array(data.buffer, data.byteOffset, data.byteLength)
  return Uint8Array.from(data as ArrayLike<number>)
}

/** Concatenate chunks (returns the only chunk as is). */
export function concatBytes(parts: readonly Uint8Array[]): Uint8Array {
  if (parts.length === 0) return EMPTY
  if (parts.length === 1) return parts[0]
  let n = 0
  for (const p of parts) n += p.length
  const out = new Uint8Array(n)
  let o = 0
  for (const p of parts) {
    out.set(p, o)
    o += p.length
  }
  return out
}

/** First index of `needle` in `hay` at or after `from`, or -1. */
export function indexOfBytes(hay: Uint8Array, needle: ArrayLike<number>, from = 0): number {
  const n = needle.length
  if (n === 0) return Math.min(Math.max(0, from), hay.length)
  const first = needle[0]
  for (let i = Math.max(0, from); i + n <= hay.length; i++) {
    i = hay.indexOf(first, i)
    if (i < 0 || i + n > hay.length) return -1
    let j = 1
    while (j < n && hay[i + j] === needle[j]) j++
    if (j === n) return i
  }
  return -1
}

/** Last index of `needle` in `hay`, or -1. */
export function lastIndexOfBytes(hay: Uint8Array, needle: ArrayLike<number>): number {
  const n = needle.length
  if (n === 0) return hay.length
  for (let i = hay.length - n; i >= 0; i--) {
    if (hay[i] !== needle[0]) continue
    let j = 1
    while (j < n && hay[i + j] === needle[j]) j++
    if (j === n) return i
  }
  return -1
}

/** Bytes as a latin-1 string (one char per byte) — for scanning ASCII markers with string methods. */
export function latin1(bytes: Uint8Array, start = 0, end = bytes.length): string {
  let s = ''
  for (let i = start; i < end; i += 0x2000) s += String.fromCharCode(...bytes.subarray(i, Math.min(end, i + 0x2000)))
  return s
}

/** Octet array (zmodem.js) → Uint8Array. */
export function fromOctets(octets: ArrayLike<number>): Uint8Array {
  return octets instanceof Uint8Array ? octets : Uint8Array.from(octets)
}

/** Split `bytes` into views of at most `max` bytes. */
export function* slices(bytes: Uint8Array, max: number): Generator<Uint8Array> {
  for (let i = 0; i < bytes.length; i += max) yield bytes.subarray(i, Math.min(bytes.length, i + max))
}
