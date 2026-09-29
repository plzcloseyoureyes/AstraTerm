/*
 * ZMODEM (FILE-19) on top of zmodem.js 0.1.10:
 *
 *   ZmodemStage     the output filter stage: a zmodem.js Sentry watches the stream for a ZRQINIT (remote `sz`) or
 *                   ZRINIT (remote `rz`) hex header, reports a detection (which the controller confirms, denies or
 *                   lets retract), and while a session runs feeds it every byte (nothing reaches the terminal but
 *                   "garbage" around it). The header that started a session is hidden from the screen.
 *   zmodemReceive   drives a receive session (`sz` on the remote): offers → save target, progress, cancel.
 *   zmodemSend      drives a send session (`rz` on the remote) with end-to-end flow control: zmodem.js streams a
 *                   file without waiting for the receiver, but Termstead's server queues at most 8 MiB of input per
 *                   session, so every 64 KiB piece ends with ZCRCQ (the receiver answers ZACK with its file
 *                   position) and at most 1 MiB is in flight.
 *
 * zmodem.js internals used (checked at runtime, pinned version): Send session `_send_file_part`,
 * `_next_header_handler`, `_consume_header`; header `_bytes4`. No DOM, no app imports: unit-tested under Node.
 */
import Zmodem from 'zmodem.js'
import type { ZmDetection, ZmHeader, ZmOctets, ZmOffer, ZmReceiveSession, ZmSendSession, ZmSentry, ZmSession, ZmTransfer } from 'zmodem.js'
import { EMPTY, concatBytes, fromOctets, lastIndexOfBytes, slices } from './bytes'
import { splitSafePath } from './names'
import { AbortedError, throwIfAborted } from './pacer'
import type { SaveFile } from './save'

export type { ZmDetection, ZmReceiveSession, ZmSendSession, ZmSession }

/** zmodem.js errors are strings or Error objects. */
export function zmodemErrorMessage(err: unknown): string {
  if (typeof err === 'string') return err
  if (err instanceof Error) {
    const t = (err as Error & { type?: string }).type
    if (t === 'peer_aborted') return 'The remote side canceled the transfer'
    return err.message || String(err)
  }
  return String(err)
}

export function isPeerAbort(err: unknown): boolean {
  return (err as { type?: string } | null)?.type === 'peer_aborted' || err instanceof PeerAbortError
}

export class PeerAbortError extends Error {
  constructor() {
    super('The remote side canceled the transfer')
    this.name = 'PeerAbortError'
  }
}

// ---------------------------------------------------------------------------------------------------------------------
// stage
// ---------------------------------------------------------------------------------------------------------------------

export interface ZmodemStageHooks {
  /** Bytes for the remote (the session's input). */
  send(bytes: Uint8Array): void
  /** Terminal output produced outside a filter() call. */
  writeAsync(bytes: Uint8Array): void
  /** A session start was seen (not yet confirmed). May be followed by retract() — even in the same filter() call. */
  detect(detection: ZmDetection): void
  /** The pending detection turned out not to be ZMODEM (more text followed) or was replaced by a new one. */
  retract(): void
  /** A confirmed session hit a protocol error; the stage aborted it (CAN sequence sent). */
  failed(err: unknown): void
}

/** "**\x18B" — start of a hex header. */
const HEX_HEADER = [0x2a, 0x2a, 0x18, 0x42]
/** "rz\r" — the auto-start string lrzsz `sz` prints before ZRQINIT. */
const RZ_CR = [0x72, 0x7a, 0x0d]

/** Remove the hex header that started a session (and sz's "rz\r" before it) from terminal output. */
export function stripSessionHeader(out: Uint8Array): Uint8Array {
  const at = lastIndexOfBytes(out, HEX_HEADER)
  if (at < 0) return out
  let cut = at
  if (cut >= 3 && out[cut - 3] === RZ_CR[0] && out[cut - 2] === RZ_CR[1] && out[cut - 1] === RZ_CR[2]) cut -= 3
  return out.subarray(0, cut)
}

const CAN = 0x18
const BS = 0x08
/** The full cancel sequence of the ZMODEM spec (8 × CAN, 10 × BS). */
export const CANCEL_SEQUENCE: Uint8Array = Uint8Array.from([...Array(8).fill(CAN), ...Array(10).fill(BS)])

/**
 * zmodem.js cancels with 5 × CAN (+ 5 × BS); lrzsz's sz, busy streaming, needs more before it gives up. Five
 * consecutive CANs never occur in encoded ZMODEM data (ZDLE = CAN is always followed by another byte), so the
 * library's abort output is recognised and replaced by the spec's full sequence.
 */
export function expandAbort(bytes: Uint8Array): Uint8Array {
  if (bytes.length !== 5 && bytes.length !== 10) return bytes
  for (let i = 0; i < 5; i++) if (bytes[i] !== CAN) return bytes
  for (let i = 5; i < bytes.length; i++) if (bytes[i] !== BS) return bytes
  return CANCEL_SEQUENCE
}

export class ZmodemStage {
  private readonly sentry: ZmSentry
  private readonly hooks: ZmodemStageHooks
  private collecting: Uint8Array[] | null = null
  private detectedInCall = false
  /** A rendered "rz\r" at the end of a chunk, held briefly: sz writes it just before its ZRQINIT header. */
  private heldRz: Uint8Array | null = null
  private heldTimer: ReturnType<typeof setTimeout> | null = null
  /** A detection waits for confirm / deny / retract: every byte must reach the Sentry. */
  private pending = false
  /** The previous chunk ended with a ZDLE nearby: a header may continue in the next one. */
  private feedNext = false

  constructor(hooks: ZmodemStageHooks) {
    this.hooks = hooks
    this.sentry = new Zmodem.Sentry({
      to_terminal: (octets: ZmOctets) => this.toTerminal(octets),
      sender: (octets: ZmOctets) => hooks.send(expandAbort(fromOctets(octets))),
      on_detect: (d: ZmDetection) => {
        this.detectedInCall = true
        this.pending = true
        hooks.detect(d)
      },
      on_retract: () => {
        this.pending = false
        hooks.retract()
      },
    })
  }

  private toTerminal(octets: ArrayLike<number>): void {
    if (!octets.length) return
    const bytes = fromOctets(octets)
    if (this.collecting) this.collecting.push(bytes)
    else this.hooks.writeAsync(bytes)
  }

  /** The confirmed session (null when none runs). */
  get session(): ZmSession | null {
    return this.sentry.get_confirmed_session()
  }

  get active(): boolean {
    const s = this.session
    return !!s && !s.has_ended()
  }

  /** OutputMux: a running session owns every byte. */
  get busy(): boolean {
    return this.active
  }

  /** Feed output; returns the bytes to render. */
  filter(bytes: Uint8Array): Uint8Array {
    if (!bytes.length) return bytes
    const out: Uint8Array[] = []
    this.detectedInCall = false
    // Fast path: every ZMODEM header contains ZDLE (0x18). While nothing is pending, output without one cannot start a
    // session, so it skips zmodem.js (which copies each chunk into a JS number array).
    const zdle = bytes.indexOf(0x18)
    if (zdle < 0 && !this.pending && !this.feedNext && !this.busy) {
      out.push(bytes)
    } else {
      this.collecting = out
      try {
        for (const part of slices(bytes, 32 * 1024)) {
          try {
            this.sentry.consume(part)
          } catch (err) {
            this.fail(err)
          }
        }
      } finally {
        this.collecting = null
      }
    }
    this.feedNext = bytes.lastIndexOf(0x18) >= bytes.length - 24 && bytes.lastIndexOf(0x18) >= 0
    if (this.busy) this.pending = false
    let rendered = concatBytes(out)
    if (this.heldRz) {
      rendered = concatBytes([this.heldRz, rendered])
      this.heldRz = null
      if (this.heldTimer) clearTimeout(this.heldTimer)
      this.heldTimer = null
    }
    if (this.detectedInCall) return stripSessionHeader(rendered)
    const n = rendered.length
    if (!this.busy && n >= 3 && rendered[n - 3] === RZ_CR[0] && rendered[n - 2] === RZ_CR[1] && rendered[n - 1] === RZ_CR[2]) {
      this.heldRz = rendered.slice(n - 3)
      this.heldTimer = setTimeout(() => {
        this.heldTimer = null
        const h = this.heldRz
        this.heldRz = null
        if (h) this.hooks.writeAsync(h)
      }, 60)
      return rendered.subarray(0, n - 3)
    }
    return rendered
  }

  dispose(): void {
    if (this.heldTimer) clearTimeout(this.heldTimer)
    this.heldTimer = null
    this.heldRz = null
  }

  /** Abort the running session (CAN sequence) after a protocol error or a local failure. */
  fail(err: unknown): void {
    const s = this.session
    if (s && !s.has_ended()) {
      try {
        s.abort()
      } catch {
        /* already aborted */
      }
    }
    this.hooks.failed(err)
  }
}

// ---------------------------------------------------------------------------------------------------------------------
// shared progress model
// ---------------------------------------------------------------------------------------------------------------------

export interface ZProgress {
  /** Current file (remote name for downloads, local name for uploads). */
  file: string
  /** 1-based index of the current file. */
  fileIndex: number
  /** Number of files when known. */
  fileCount: number | null
  fileBytes: number
  fileSize: number | null
  /** Bytes of the whole transfer so far. */
  bytes: number
  totalSize: number | null
}

export interface ZResult {
  files: string[]
  skipped: string[]
  bytes: number
}

// ---------------------------------------------------------------------------------------------------------------------
// receive (remote `sz`)
// ---------------------------------------------------------------------------------------------------------------------

export interface ZOfferInfo {
  /** Name as sent by the remote. */
  name: string
  /** Safe relative path components. */
  path: string[]
  size: number | null
  mtimeMs: number | null
  mode: number | null
}

export interface ReceiveIO {
  /** Where an offered file goes; null skips it. */
  open(info: ZOfferInfo): Promise<SaveFile | null>
  progress?(p: ZProgress): void
  signal?: AbortSignal
}

interface ReceiveInternals {
  _consume_first?: () => void
  _consume_ZFIN?: () => void
  _got_ZFIN?: boolean
  _input_buffer?: number[]
  _bytes_being_consumed?: number[]
  _bytes_after_OO?: number[] | null
  _on_session_end?: () => void
  has_ended(): boolean
}

/**
 * lrzsz's sz sometimes loses its final "OO" (it flushes its tty while exiting), and zmodem.js then throws although
 * every file arrived and ZFIN was exchanged. After ZFIN, anything but "OO" — or silence — ends the session normally;
 * those bytes are terminal output (the shell prompt).
 */
function patchReceiveSession(session: ZmReceiveSession, finishMs = 2500): void {
  const s = session as unknown as ReceiveInternals
  const first = s._consume_first
  const zfin = s._consume_ZFIN
  if (typeof first !== 'function' || typeof zfin !== 'function' || typeof s._on_session_end !== 'function') return
  const finish = (self: ReceiveInternals, trailing: number[]) => {
    if (self._bytes_after_OO || self.has_ended()) return
    self._bytes_after_OO = trailing
    self._on_session_end!()
  }
  s._consume_first = function (this: ReceiveInternals) {
    const buf = this._input_buffer ?? []
    if (this._got_ZFIN && buf.length > 0 && buf[0] !== 0x4f) {
      finish(this, (this._bytes_being_consumed ?? []).slice(0))
      return
    }
    if (this._got_ZFIN && buf.length >= 2 && buf[1] !== 0x4f) {
      finish(this, (this._bytes_being_consumed ?? []).slice(0))
      return
    }
    return first.call(this)
  }
  s._consume_ZFIN = function (this: ReceiveInternals) {
    zfin.call(this)
    setTimeout(() => finish(this, []), finishMs)
  }
}

export function zmodemReceive(session: ZmReceiveSession, io: ReceiveIO): Promise<ZResult> {
  patchReceiveSession(session)
  return new Promise<ZResult>((resolve, reject) => {
    const files: string[] = []
    const skipped: string[] = []
    let bytes = 0
    let index = 0
    let fileCount: number | null = null
    let totalSize: number | null = null
    let failure: unknown = null
    let pending: Promise<void> = Promise.resolve()
    let current: SaveFile | null = null
    let settled = false

    const fail = (err: unknown) => {
      if (failure === null) failure = err
      if (!session.has_ended()) {
        try {
          session.abort()
        } catch {
          /* ended meanwhile */
        }
      }
    }
    const onAbort = () => fail(new AbortedError())
    io.signal?.addEventListener('abort', onAbort, { once: true })

    session.on('offer', (offer: ZmOffer) => {
      pending = pending.then(() => receiveOne(offer)).catch(fail)
    })

    const receiveOne = async (offer: ZmOffer) => {
      if (failure !== null) return
      const d = offer.get_details()
      index++
      if (d.files_remaining && fileCount === null) fileCount = index - 1 + d.files_remaining
      if (d.bytes_remaining != null && totalSize === null) totalSize = bytes + d.bytes_remaining
      const info: ZOfferInfo = {
        name: d.name,
        path: splitSafePath(d.name),
        size: d.size,
        mtimeMs: d.mtime ? d.mtime.getTime() : null,
        mode: d.mode,
      }
      const file = await io.open(info)
      if (failure !== null) {
        await file?.abort().catch(() => undefined)
        return
      }
      if (!file) {
        skipped.push(d.name)
        void offer.skip()
        return
      }
      current = file
      let fileBytes = 0
      let chain: Promise<void> = Promise.resolve()
      let writeErr: unknown = null
      const report = () =>
        io.progress?.({ file: d.name, fileIndex: index, fileCount, fileBytes, fileSize: d.size, bytes, totalSize })
      report()
      const done = offer.accept({
        on_input: (payload: ZmOctets) => {
          const chunk = fromOctets(payload)
          fileBytes += chunk.length
          bytes += chunk.length
          chain = chain.then(() => (writeErr === null ? file.write(chunk) : undefined)).catch((e) => {
            if (writeErr === null) writeErr = e
          })
          report()
          if (writeErr !== null) fail(writeErr)
        },
      })
      await done
      await chain
      if (writeErr !== null) throw writeErr
      await file.close()
      current = null
      files.push(file.localName)
    }

    session.on('session_end', () => {
      void (async () => {
        // A normal end (ZFIN / OO) comes after the last file's data: let its close() finish. After an abort the
        // pending file never completes (its accept() promise stays open), so do not wait for it.
        const aborted = failure !== null || session.aborted()
        if (!aborted) await pending.catch(() => undefined)
        if (settled) return
        settled = true
        io.signal?.removeEventListener('abort', onAbort)
        if (current) await current.abort().catch(() => undefined)
        if (failure !== null) reject(failure)
        else if (session.aborted()) reject(new PeerAbortError())
        else resolve({ files, skipped, bytes })
      })()
    })

    try {
      void session.start()
    } catch (err) {
      settled = true
      reject(err)
    }
  })
}

// ---------------------------------------------------------------------------------------------------------------------
// send (remote `rz`)
// ---------------------------------------------------------------------------------------------------------------------

export interface SendItem {
  name: string
  blob: Blob
  mtimeMs?: number
}

export interface SendIO {
  progress?(p: ZProgress): void
  signal?: AbortSignal
  /** Bytes allowed in flight before waiting for the receiver's ZACK (default 1 MiB). */
  windowBytes?: number
  /** Piece size; each piece ends with ZCRCQ (default 64 KiB). */
  pieceBytes?: number
  /** Give up when the receiver acknowledges nothing for this long (default 60 s). */
  ackTimeoutMs?: number
}

interface SendInternals {
  _send_file_part?: (bytes: Uint8Array | ZmOctets, frameEnd: string) => void
  _next_header_handler?: Record<string, (hdr: ZmHeader) => void> | null
  _consume_header?: (hdr: ZmHeader) => void
  _consume_first?: () => void
  _parse_and_consume_header?: () => ZmHeader | undefined
  _input_buffer?: number[]
  _on_receive?: (hdr: ZmHeader) => void
}

interface TransferInternals {
  _file_offset?: number
}

function u32le(b: ArrayLike<number> | undefined): number {
  if (!b || b.length < 4) return 0
  return (b[0] | (b[1] << 8) | (b[2] << 16) | (b[3] << 24)) >>> 0
}

/**
 * Two fixes for zmodem.js send sessions:
 *  - it parses only ONE header per consume() call, so headers arriving together (the receiver's ZACKs coalesce in one
 *    read) pile up in its buffer and the transfer stalls once the receiver goes quiet: parse every complete header;
 *  - a ZACK nobody waits for (e.g. answering a keep-alive ZSINIT) throws "Unhandled header": ignore it.
 */
function patchSendSession(session: ZmSendSession): void {
  const s = session as unknown as SendInternals
  const consumeHeader = s._consume_header
  if (typeof consumeHeader === 'function') {
    s._consume_header = function (this: SendInternals, hdr: ZmHeader) {
      if (hdr?.NAME === 'ZACK' && !this._next_header_handler?.ZACK) {
        this._on_receive?.(hdr)
        return
      }
      return consumeHeader.call(this, hdr)
    }
  }
  if (typeof s._consume_first === 'function' && typeof s._parse_and_consume_header === 'function') {
    s._consume_first = function (this: SendInternals & ZmSendSession) {
      let parsed: ZmHeader | undefined
      do {
        parsed = this._parse_and_consume_header!()
      } while (parsed && !this.has_ended() && (this._input_buffer?.length ?? 0) > 0)
      if (!parsed && this._input_buffer?.join() === '67') throw 'Receiver has fallen back to YMODEM.'
    }
  }
}

export async function zmodemSend(session: ZmSendSession, items: readonly SendItem[], io: SendIO = {}): Promise<ZResult> {
  const win = Math.max(64 * 1024, io.windowBytes ?? 1024 * 1024)
  const pieceSize = Math.max(1024, Math.min(win, io.pieceBytes ?? 64 * 1024))
  const ackTimeout = io.ackTimeoutMs ?? 60_000
  const s = session as unknown as SendInternals
  const windowed = typeof s._send_file_part === 'function'
  patchSendSession(session)

  let ended = false
  let wake: (() => void) | null = null
  const kick = () => {
    const w = wake
    wake = null
    w?.()
  }
  session.on('session_end', () => {
    ended = true
    kick()
  })
  const onAbort = () => {
    if (!session.has_ended()) {
      try {
        session.abort()
      } catch {
        /* ended */
      }
    }
    kick()
  }
  io.signal?.addEventListener('abort', onAbort, { once: true })

  // A session also ends normally (close() → ZFIN/OO): only an aborted one is an error.
  const check = () => {
    throwIfAborted(io.signal)
    if (ended && session.aborted()) throw new PeerAbortError()
  }

  /** Await a zmodem.js promise, or fail when the session ends / the user cancels first. */
  const race = async <T>(p: Promise<T>): Promise<T> => {
    let resolved = false
    let value!: T
    let error: unknown = null
    p.then(
      (v) => {
        resolved = true
        value = v
        kick()
      },
      (e) => {
        resolved = true
        error = e ?? new Error('ZMODEM error')
        kick()
      },
    )
    while (!resolved) {
      check()
      await new Promise<void>((r) => (wake = r))
    }
    if (error !== null) throw error
    return value
  }

  const total = items.reduce((n, it) => n + it.blob.size, 0)
  let remaining = total
  let bytes = 0
  const files: string[] = []
  const skipped: string[] = []

  try {
    for (let i = 0; i < items.length; i++) {
      check()
      if (ended) throw new PeerAbortError()
      const it = items[i]
      const size = it.blob.size
      const report = (fileBytes: number) =>
        io.progress?.({ file: it.name, fileIndex: i + 1, fileCount: items.length, fileBytes, fileSize: size, bytes: bytes + fileBytes, totalSize: total })
      report(0)
      const xfer = await race(
        session.send_offer({
          name: it.name,
          size,
          mtime: Math.floor((it.mtimeMs ?? Date.now()) / 1000),
          files_remaining: items.length - i,
          bytes_remaining: remaining,
        }),
      )
      if (!xfer) {
        skipped.push(it.name)
      } else {
        await sendBody(xfer, it.blob, report)
        files.push(it.name)
        bytes += size
      }
      remaining -= size
    }
    check()
    await race(session.close())
    return { files, skipped, bytes }
  } catch (err) {
    if (!session.has_ended()) {
      try {
        session.abort()
      } catch {
        /* ended */
      }
    }
    throw err
  } finally {
    io.signal?.removeEventListener('abort', onAbort)
  }

  async function sendBody(xfer: ZmTransfer, blob: Blob, report: (n: number) => void): Promise<void> {
    const size = blob.size
    const start = Math.min(size, Math.max(0, xfer.get_offset() || 0)) // receiver may resume (ZRPOS > 0)
    let sent = start
    let acked = start
    let lastAckAt = Date.now()
    // zmodem.js starts every file's ZDATA at the session offset 0; a resumed file must start where ZRPOS asked.
    if (windowed && start > 0) (s as SendInternals & { _file_offset?: number })._file_offset = start
    const install = () => {
      s._next_header_handler = { ZACK: onAck, ZRPOS: onRpos }
    }
    function onAck(hdr: ZmHeader) {
      const off = u32le(hdr._bytes4)
      if (off > acked) acked = Math.min(off, sent)
      lastAckAt = Date.now()
      install()
      kick()
    }
    function onRpos() {
      // Mid-transfer ZRPOS: like zmodem.js, keep going (a reliable transport should never need it).
      install()
    }
    const waitAcks = async (until: (() => boolean)) => {
      while (!until()) {
        check()
        const remainingMs = ackTimeout - (Date.now() - lastAckAt)
        if (remainingMs <= 0) throw new Error('The receiver stopped acknowledging data')
        await new Promise<void>((r) => {
          const t = setTimeout(r, Math.min(remainingMs, 1000))
          wake = () => {
            clearTimeout(t)
            r()
          }
        })
      }
    }
    if (windowed) install()
    report(sent)
    while (sent < size) {
      check()
      const n = Math.min(pieceSize, size - sent)
      const piece = new Uint8Array(await blob.slice(sent, sent + n).arrayBuffer())
      check()
      if (sent + n >= size) {
        if (windowed) await waitAcks(() => acked >= sent)
        await race(xfer.end(piece))
        sent += n
        report(sent)
        return
      }
      if (windowed) {
        await waitAcks(() => sent - acked < win)
        s._send_file_part!(piece, 'no_end_ack')
        ;(xfer as unknown as TransferInternals)._file_offset = ((xfer as unknown as TransferInternals)._file_offset ?? 0) + n
      } else {
        xfer.send(piece)
      }
      sent += n
      report(sent)
    }
    // Empty file, or everything already there (resume).
    if (windowed) await waitAcks(() => acked >= sent)
    await race(xfer.end(EMPTY))
  }
}
