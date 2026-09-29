/*
 * Minimal ZIP writer (STORE method, UTF-8 names) used when a folder transfer (trzsz -d) is saved through the
 * browser's download manager, which cannot create folders. Entries are kept as Blob parts, so building the archive
 * does not copy file contents again. Classic ZIP only: every entry and the whole archive must stay below 4 GiB.
 * No DOM beyond Blob (available in Node too) — unit-tested under Node.
 */

const CRC_TABLE = (() => {
  const t = new Uint32Array(256)
  for (let n = 0; n < 256; n++) {
    let c = n
    for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1
    t[n] = c >>> 0
  }
  return t
})()

/** Incremental CRC-32 (IEEE 802.3), as used by ZIP. */
export class Crc32 {
  private c = 0xffffffff

  update(bytes: Uint8Array): this {
    let c = this.c
    for (let i = 0; i < bytes.length; i++) c = CRC_TABLE[(c ^ bytes[i]) & 0xff] ^ (c >>> 8)
    this.c = c
    return this
  }

  digest(): number {
    return (this.c ^ 0xffffffff) >>> 0
  }
}

export function crc32(bytes: Uint8Array): number {
  return new Crc32().update(bytes).digest()
}

const LIMIT = 0xffffffff
const utf8 = new TextEncoder()

interface Entry {
  name: Uint8Array
  dir: boolean
  crc: number
  size: number
  parts: (Uint8Array | Blob)[]
  dosTime: number
  dosDate: number
  offset: number
}

function dosDateTime(ms: number | undefined): { time: number; date: number } {
  const d = new Date(Number.isFinite(ms) ? (ms as number) : Date.now())
  const year = Math.min(2107, Math.max(1980, d.getFullYear()))
  return {
    time: ((d.getHours() & 0x1f) << 11) | ((d.getMinutes() & 0x3f) << 5) | ((d.getSeconds() >> 1) & 0x1f),
    date: (((year - 1980) & 0x7f) << 9) | (((d.getMonth() + 1) & 0x0f) << 5) | (d.getDate() & 0x1f),
  }
}

export class ZipTooLargeError extends Error {
  constructor() {
    super('The folder is too large for a ZIP download (limits: 4 GiB, 65,535 items). Save it to a folder instead.')
    this.name = 'ZipTooLargeError'
  }
}

export class ZipWriter {
  private readonly entries: Entry[] = []
  private readonly names = new Set<string>()
  private total = 0

  /** Archive-relative path with "/" separators (components must already be safe). */
  addFile(path: string, parts: readonly Uint8Array[], mtimeMs?: number): void {
    const crc = new Crc32()
    let size = 0
    for (const p of parts) {
      crc.update(p)
      size += p.length
    }
    this.push(path.replace(/^\/+/, ''), false, crc.digest(), size, parts.slice(), mtimeMs)
  }

  /** A file whose CRC-32 and size the caller computed while streaming (parts may be Blobs: no copy in JS memory). */
  addFileParts(path: string, parts: readonly (Uint8Array | Blob)[], crc: number, size: number, mtimeMs?: number): void {
    this.push(path.replace(/^\/+/, ''), false, crc, size, parts.slice(), mtimeMs)
  }

  addDirectory(path: string, mtimeMs?: number): void {
    const p = path.replace(/^\/+/, '').replace(/\/?$/, '/')
    if (this.names.has(p)) return
    this.push(p, true, 0, 0, [], mtimeMs)
  }

  get size(): number {
    return this.total
  }

  get count(): number {
    return this.entries.length
  }

  private push(path: string, dir: boolean, crc: number, size: number, parts: (Uint8Array | Blob)[], mtimeMs?: number): void {
    const name = utf8.encode(path)
    if (name.length > 0xffff) throw new Error('File name too long for ZIP')
    const { time, date } = dosDateTime(mtimeMs)
    const add = 30 + name.length + size + 46 + name.length
    if (size >= LIMIT || this.total + add >= LIMIT || this.entries.length >= 0xffff) throw new ZipTooLargeError()
    this.names.add(path)
    this.entries.push({ name, dir, crc, size, parts, dosTime: time, dosDate: date, offset: 0 })
    this.total += add
  }

  /** The archive as Blob parts (local headers + data, central directory, end record). */
  parts(): BlobPart[] {
    const out: BlobPart[] = []
    let offset = 0
    for (const e of this.entries) {
      e.offset = offset
      const h = new DataView(new ArrayBuffer(30))
      h.setUint32(0, 0x04034b50, true) // local file header signature
      h.setUint16(4, 20, true) // version needed (2.0)
      h.setUint16(6, 0x0800, true) // flags: UTF-8 names
      h.setUint16(8, 0, true) // method: store
      h.setUint16(10, e.dosTime, true)
      h.setUint16(12, e.dosDate, true)
      h.setUint32(14, e.crc, true)
      h.setUint32(18, e.size, true)
      h.setUint32(22, e.size, true)
      h.setUint16(26, e.name.length, true)
      h.setUint16(28, 0, true)
      out.push(new Uint8Array(h.buffer), e.name as Uint8Array<ArrayBuffer>)
      for (const p of e.parts) out.push(p as Uint8Array<ArrayBuffer> | Blob)
      offset += 30 + e.name.length + e.size
    }
    const cdStart = offset
    for (const e of this.entries) {
      const c = new DataView(new ArrayBuffer(46))
      c.setUint32(0, 0x02014b50, true) // central directory header signature
      c.setUint16(4, (3 << 8) | 20, true) // made by: UNIX, 2.0
      c.setUint16(6, 20, true)
      c.setUint16(8, 0x0800, true)
      c.setUint16(10, 0, true)
      c.setUint16(12, e.dosTime, true)
      c.setUint16(14, e.dosDate, true)
      c.setUint32(16, e.crc, true)
      c.setUint32(20, e.size, true)
      c.setUint32(24, e.size, true)
      c.setUint16(28, e.name.length, true)
      c.setUint16(30, 0, true) // extra
      c.setUint16(32, 0, true) // comment
      c.setUint16(34, 0, true) // disk
      c.setUint16(36, 0, true) // internal attributes
      // external attributes: UNIX mode in the high 16 bits (+ MS-DOS directory bit)
      c.setUint32(38, (((e.dir ? 0o040755 : 0o100644) << 16) | (e.dir ? 0x10 : 0)) >>> 0, true)
      c.setUint32(42, e.offset, true)
      out.push(new Uint8Array(c.buffer), e.name as Uint8Array<ArrayBuffer>)
      offset += 46 + e.name.length
    }
    const end = new DataView(new ArrayBuffer(22))
    end.setUint32(0, 0x06054b50, true)
    end.setUint16(4, 0, true)
    end.setUint16(6, 0, true)
    end.setUint16(8, this.entries.length, true)
    end.setUint16(10, this.entries.length, true)
    end.setUint32(12, offset - cdStart, true)
    end.setUint32(16, cdStart, true)
    end.setUint16(20, 0, true)
    out.push(new Uint8Array(end.buffer))
    return out
  }

  toBlob(): Blob {
    return new Blob(this.parts(), { type: 'application/zip' })
  }
}
