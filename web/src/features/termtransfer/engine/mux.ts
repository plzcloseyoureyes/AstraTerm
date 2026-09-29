/*
 * Output multiplexer of one terminal view: decides which bytes reach xterm while in-terminal transfers are possible.
 *
 *   replayed bytes (history re-sent after attaching)  → rendered as they are, never inspected (an old `sz` in the
 *                                                        scrollback must not start a transfer)
 *   a transfer answered by another view / window       → swallowed (its protocol bytes are not text)
 *   draining after a cancelled / failed ZMODEM session → swallowed until the stream is quiet (the sender's in-flight
 *                                                        data would otherwise show as garbage); the text lines that
 *                                                        follow the last binary byte (the shell's messages and its
 *                                                        new prompt) are rendered when the drain ends
 *   otherwise                                          → trzsz stage, then ZMODEM stage (a running session of one
 *                                                        kind bypasses the other, so file contents that happen to
 *                                                        contain a magic string cannot start a second transfer)
 */
import { EMPTY, concatBytes } from './bytes'

export interface MuxStage {
  /** Returns the bytes to render. */
  filter(bytes: Uint8Array): Uint8Array
  /** A transfer of this stage is running (the stage owns every byte). */
  readonly busy: boolean
}

export interface MuxState {
  replay: boolean
  /** Another view answers a transfer on this session. */
  follower: boolean
}

const TAIL_KEEP = 16 * 1024

/** Bytes that never occur in terminal text (NUL, C0 controls except BEL BS TAB LF CR ESC, DEL). */
function isBinary(b: number): boolean {
  return (b < 0x20 && b !== 0x07 && b !== 0x08 && b !== 0x09 && b !== 0x0a && b !== 0x0d && b !== 0x1b) || b === 0x7f
}

/** The complete lines of text after the last binary byte (what the shell printed once the sender stopped). */
export function textTail(bytes: Uint8Array): Uint8Array {
  let i = bytes.length - 1
  while (i >= 0 && !isBinary(bytes[i])) i--
  if (i < 0) return bytes
  // Start after the line break that follows the garbage (a partial garbage line is not text).
  let j = i + 1
  while (j < bytes.length && bytes[j] !== 0x0a && bytes[j] !== 0x0d) j++
  while (j < bytes.length && (bytes[j] === 0x0a || bytes[j] === 0x0d)) j++
  // ZMODEM leftovers after the last header: its LF | 0x80, XON / XOFF, and the sender's final "OO".
  while (j < bytes.length && (bytes[j] === 0x8a || bytes[j] === 0x8d || bytes[j] === 0x11 || bytes[j] === 0x13)) j++
  if (bytes[j] === 0x4f && bytes[j + 1] === 0x4f) j += 2
  return bytes.subarray(j)
}

export class OutputMux {
  private drainUntil = 0
  private drainQuietMs = 0
  private lastDrainByteAt = 0
  private drained: Uint8Array[] = []
  private drainedBytes = 0
  private onDrained: ((tail: Uint8Array) => void) | null = null
  private drainTimer: ReturnType<typeof setInterval> | null = null
  private followed: Uint8Array[] = []
  private followedBytes = 0
  private readonly now: () => number

  constructor(now: () => number = Date.now) {
    this.now = now
  }

  /**
   * Swallow output until it has been quiet for `quietMs` (at most `maxMs`); then hand the text tail of what was
   * swallowed to `onDrained` (the controller writes it locally).
   */
  drain(quietMs = 400, maxMs = 3000, onDrained?: (tail: Uint8Array) => void): void {
    const t = this.now()
    this.drainUntil = t + maxMs
    this.drainQuietMs = quietMs
    this.lastDrainByteAt = t
    this.drained = []
    this.drainedBytes = 0
    this.onDrained = onDrained ?? null
    if (this.onDrained && !this.drainTimer) {
      this.drainTimer = setInterval(() => this.checkDrain(), Math.max(20, Math.min(100, quietMs / 4)))
    }
  }

  get draining(): boolean {
    return this.drainUntil > 0
  }

  /** End the drain if the stream went quiet (or the deadline passed); called by the timer and on new output. */
  checkDrain(): boolean {
    if (!this.drainUntil) return false
    const t = this.now()
    if (t < this.drainUntil && t - this.lastDrainByteAt < this.drainQuietMs) return false
    this.endDrain()
    return true
  }

  dispose(): void {
    if (this.drainTimer) clearInterval(this.drainTimer)
    this.drainTimer = null
    this.onDrained = null
  }

  private endDrain(): void {
    this.drainUntil = 0
    if (this.drainTimer) clearInterval(this.drainTimer)
    this.drainTimer = null
    const tail = textTail(concatBytes(this.drained))
    this.drained = []
    this.drainedBytes = 0
    const cb = this.onDrained
    this.onDrained = null
    if (cb && tail.length) cb(tail)
  }

  private keep(bytes: Uint8Array): void {
    this.drained.push(bytes.slice())
    this.drainedBytes += bytes.length
    while (this.drainedBytes > TAIL_KEEP && this.drained.length > 1) this.drainedBytes -= this.drained.shift()!.length
  }

  /**
   * Text lines hidden while another view answered a transfer (the shell's prompt after it usually arrives just before
   * that view's release): the caller renders them when it stops following.
   */
  takeFollowerTail(): Uint8Array {
    const tail = textTail(concatBytes(this.followed))
    this.followed = []
    this.followedBytes = 0
    return tail
  }

  filter(bytes: Uint8Array, state: MuxState, stages: readonly (MuxStage | null)[]): Uint8Array {
    if (!bytes.length) return bytes
    if (state.replay) return bytes
    if (this.drainUntil) {
      const t = this.now()
      if (t < this.drainUntil && t - this.lastDrainByteAt < this.drainQuietMs) {
        this.lastDrainByteAt = t
        this.keep(bytes)
        return EMPTY
      }
      this.endDrain()
    }
    if (state.follower) {
      this.followed.push(bytes.slice())
      this.followedBytes += bytes.length
      while (this.followedBytes > TAIL_KEEP && this.followed.length > 1) this.followedBytes -= this.followed.shift()!.length
      return EMPTY
    }
    const busy = stages.find((s) => s?.busy)
    if (busy) return busy.filter(bytes)
    let out = bytes
    for (const s of stages) {
      if (!s || !out.length) continue
      out = s.filter(out)
      // A stage that just became busy owns the rest of this chunk already (it returned what precedes its start).
      if (s.busy) break
    }
    return out
  }
}
