/*
 * Paced sending into a terminal (CC-8 "send file to session"): text line by line with a per-line / per-character
 * delay and optional wait-for-prompt, or raw binary in paced chunks.
 *
 * The server-side pacer of the automation module (POST /api/automation/send) is preferred for text (it survives
 * background tabs and sees the session's own prompt marks); this browser pacer is the fallback and handles binary.
 * Browsers throttle timers of background tabs to ≥ 1 s (and to 1/min after a while), so delays run on a tiny
 * dedicated Worker clock when one can be created (worker timers are not throttled), else on setTimeout.
 *
 * No app imports: unit-tested under Node.
 */

export class AbortedError extends Error {
  constructor(message = 'Canceled') {
    super(message)
    this.name = 'AbortError'
  }
}

export function isAbortError(err: unknown): boolean {
  return (err as { name?: string } | null)?.name === 'AbortError'
}

export function throwIfAborted(signal?: AbortSignal): void {
  if (signal?.aborted) throw new AbortedError()
}

// ---------------------------------------------------------------------------------------------------------------------
// clocks
// ---------------------------------------------------------------------------------------------------------------------

export interface Clock {
  /** Resolves after `ms`; rejects with AbortedError when `signal` aborts first. */
  sleep(ms: number, signal?: AbortSignal): Promise<void>
  readonly kind: 'worker' | 'timeout'
}

function abortable(signal: AbortSignal | undefined, start: (done: () => void) => () => void): Promise<void> {
  return new Promise<void>((resolve, reject) => {
    if (signal?.aborted) {
      reject(new AbortedError())
      return
    }
    let cancel: (() => void) | null = null
    const onAbort = () => {
      cancel?.()
      reject(new AbortedError())
    }
    signal?.addEventListener('abort', onAbort, { once: true })
    cancel = start(() => {
      signal?.removeEventListener('abort', onAbort)
      resolve()
    })
  })
}

export function timeoutClock(): Clock {
  return {
    kind: 'timeout',
    sleep: (ms, signal) =>
      abortable(signal, (done) => {
        const t = setTimeout(done, Math.max(0, ms))
        return () => clearTimeout(t)
      }),
  }
}

const WORKER_SOURCE = `
const timers = new Map()
onmessage = (e) => {
  const d = e.data || {}
  if (d.cancel) { clearTimeout(timers.get(d.id)); timers.delete(d.id); return }
  timers.set(d.id, setTimeout(() => { timers.delete(d.id); postMessage(d.id) }, d.ms))
}
`

let sharedWorkerClock: Clock | null | undefined

/** A clock whose timers run in a dedicated Worker (not throttled in background tabs); null when unavailable. */
export function workerClock(): Clock | null {
  if (sharedWorkerClock !== undefined) return sharedWorkerClock
  sharedWorkerClock = null
  try {
    if (typeof Worker === 'undefined' || typeof Blob === 'undefined' || typeof URL?.createObjectURL !== 'function') return null
    const url = URL.createObjectURL(new Blob([WORKER_SOURCE], { type: 'text/javascript' }))
    const worker = new Worker(url)
    const pending = new Map<number, () => void>()
    let seq = 0
    let broken = false
    const fallback = timeoutClock()
    worker.onmessage = (e: MessageEvent<number>) => {
      const fn = pending.get(e.data)
      pending.delete(e.data)
      fn?.()
    }
    worker.onerror = () => {
      // Resolve everything still waiting and fall back to setTimeout from now on.
      broken = true
      for (const fn of pending.values()) fn()
      pending.clear()
    }
    sharedWorkerClock = {
      kind: 'worker',
      sleep: (ms, signal) => {
        if (broken) return fallback.sleep(ms, signal)
        return abortable(signal, (done) => {
          const id = ++seq
          pending.set(id, done)
          worker.postMessage({ id, ms: Math.max(0, ms) })
          return () => {
            pending.delete(id)
            worker.postMessage({ id, cancel: true })
          }
        })
      },
    }
  } catch {
    sharedWorkerClock = null
  }
  return sharedWorkerClock
}

// ---------------------------------------------------------------------------------------------------------------------
// text helpers
// ---------------------------------------------------------------------------------------------------------------------

/** Lines of a text (CRLF, LF and CR separate lines); a trailing line break does not produce an empty last line. */
export function splitLines(text: string): { lines: string[]; endsWithNewline: boolean } {
  const endsWithNewline = /(\r\n|\r|\n)$/.test(text)
  const body = endsWithNewline ? text.replace(/(\r\n|\r|\n)$/, '') : text
  return { lines: text === '' ? [] : body.split(/\r\n|\r|\n/), endsWithNewline }
}

// oxlint-disable-next-line no-control-regex -- terminal escape sequences
const ANSI_RE = /\x1b\[[0-?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b[PX^_][^\x1b]*\x1b\\|\x1b[ -/]*[0-~]|\x9b[0-?]*[ -/]*[@-~]/g

/** Remove ANSI / VT escape sequences (CSI, OSC, DCS/APC/PM/SOS, other ESC sequences incl. nF / Fp like ESC =). */
export function stripAnsi(s: string): string {
  return s.replace(ANSI_RE, '')
}

/** Default "looks like a prompt" pattern (same idea as the automation module's default). */
export const DEFAULT_PROMPT_PATTERN = '[$#%>❯»:\\]]\\s*$'

/** Compile a user prompt pattern (JavaScript syntax); null when empty, throws on invalid syntax. */
export function compilePromptPattern(pattern: string): RegExp | null {
  const p = pattern.trim()
  if (!p) return null
  if (p.length > 2000) throw new Error('The prompt pattern is too long')
  return new RegExp(p)
}

// ---------------------------------------------------------------------------------------------------------------------
// prompt watcher
// ---------------------------------------------------------------------------------------------------------------------

const TAIL_MAX = 4096
const OSC133_A = '\x1b]133;A'

/**
 * Watches a session's output for the next shell prompt: an OSC 133 "A" mark (shell integration), or — once output
 * has been quiet for a moment — a last line matching the prompt pattern.
 */
export class PromptWatcher {
  private tail = ''
  private raw = ''
  private readonly decoder = new TextDecoder('utf-8', { fatal: false })
  private marks = 0
  private outputs = 0
  private lastOutputAt = 0

  feed(bytes: Uint8Array, now = Date.now()): void {
    if (!bytes.length) return
    const text = this.decoder.decode(bytes, { stream: true })
    // OSC 133;A may straddle chunks: scan the raw text with a small carry.
    const scan = this.raw + text
    let i = scan.indexOf(OSC133_A)
    while (i >= 0) {
      this.marks++
      i = scan.indexOf(OSC133_A, i + OSC133_A.length)
    }
    this.raw = scan.slice(-(OSC133_A.length - 1))
    this.tail = stripAnsi((this.tail + text).slice(-TAIL_MAX * 2)).slice(-TAIL_MAX)
    this.outputs++
    this.lastOutputAt = now
  }

  /** Marker to pass to wait(): output and prompt marks seen so far. */
  mark(): { marks: number; outputs: number } {
    return { marks: this.marks, outputs: this.outputs }
  }

  /** Last non-empty line of the recent output (escape sequences removed). */
  lastLine(): string {
    const parts = this.tail.split(/[\r\n]/)
    for (let i = parts.length - 1; i >= 0; i--) if (parts[i].trim()) return parts[i]
    return ''
  }

  /**
   * Resolve true when a prompt shows up after `since`; false after `timeoutMs`. `quietMs` of silence is required before
   * the pattern is tested (so the echo of the line just sent is not mistaken for a prompt).
   */
  async wait(
    since: { marks: number; outputs: number },
    opts: { re: RegExp; quietMs?: number; timeoutMs: number; clock: Clock; signal?: AbortSignal; now?: () => number },
  ): Promise<boolean> {
    const now = opts.now ?? Date.now
    const quiet = opts.quietMs ?? 150
    const deadline = now() + Math.max(0, opts.timeoutMs)
    for (;;) {
      throwIfAborted(opts.signal)
      if (this.marks > since.marks) return true
      const t = now()
      if (this.outputs > since.outputs && t - this.lastOutputAt >= quiet && opts.re.test(this.lastLine())) return true
      if (t >= deadline) return false
      await opts.clock.sleep(Math.min(50, Math.max(1, deadline - t)), opts.signal)
    }
  }
}

// ---------------------------------------------------------------------------------------------------------------------
// senders
// ---------------------------------------------------------------------------------------------------------------------

export interface TextPacing {
  /** Sent after each line ("\r" = Enter). */
  eol: string
  lineDelayMs: number
  charDelayMs: number
  /** Wait for the prompt before each next line. */
  prompt: { re: RegExp; timeoutMs: number; quietMs?: number } | null
  /** Press Enter after the last line too (default: only when the text ends with a line break). */
  enterAfterLast?: boolean
}

export interface TextSendIO {
  /** Raw input to the session; false when it could not be handed over (not connected). */
  send(data: string): boolean
  clock: Clock
  watcher?: PromptWatcher
  signal?: AbortSignal
  onProgress?(done: number, total: number): void
  onWarning?(message: string): void
}

export class NotConnectedError extends Error {
  constructor() {
    super('The session is not connected')
    this.name = 'NotConnectedError'
  }
}

/** Type `text` line by line. Resolves the number of lines sent and prompt warnings. */
export async function sendTextPaced(text: string, pacing: TextPacing, io: TextSendIO): Promise<{ lines: number; warnings: number }> {
  const { lines, endsWithNewline } = splitLines(text)
  const total = lines.length
  const enterLast = pacing.enterAfterLast ?? endsWithNewline
  let warnings = 0
  const put = (s: string) => {
    if (s && !io.send(s)) throw new NotConnectedError()
  }
  io.onProgress?.(0, total)
  for (let i = 0; i < total; i++) {
    throwIfAborted(io.signal)
    const last = i === total - 1
    const since = io.watcher?.mark() ?? { marks: 0, outputs: 0 }
    const line = lines[i]
    if (pacing.charDelayMs > 0) {
      for (const ch of line) {
        throwIfAborted(io.signal)
        put(ch)
        await io.clock.sleep(pacing.charDelayMs, io.signal)
      }
    } else {
      put(line)
    }
    if (!last || enterLast) put(pacing.eol)
    io.onProgress?.(i + 1, total)
    if (last) break
    if (pacing.prompt && io.watcher) {
      const ok = await io.watcher.wait(since, { re: pacing.prompt.re, quietMs: pacing.prompt.quietMs, timeoutMs: pacing.prompt.timeoutMs, clock: io.clock, signal: io.signal })
      if (!ok) {
        warnings++
        io.onWarning?.(`Line ${i + 1}: no prompt within ${Math.round(pacing.prompt.timeoutMs / 1000)} s, continuing`)
      }
    }
    if (pacing.lineDelayMs > 0) await io.clock.sleep(pacing.lineDelayMs, io.signal)
    else if (i % 64 === 63) await io.clock.sleep(0, io.signal) // stay responsive on huge files
  }
  return { lines: total, warnings }
}

export interface BinaryPacing {
  chunkBytes: number
  delayMs: number
}

export interface BinarySendIO {
  send(data: Uint8Array): boolean
  clock: Clock
  signal?: AbortSignal
  onProgress?(done: number, total: number): void
  /** Checked between chunks: a message aborts the send (e.g. the server reported a full input buffer). */
  problem?(): string | null
}

/** Stream a Blob as raw bytes in paced chunks (reads one chunk ahead). */
export async function sendBinaryPaced(blob: Blob, pacing: BinaryPacing, io: BinarySendIO): Promise<number> {
  const total = blob.size
  const chunk = Math.max(1, Math.min(1 << 20, Math.floor(pacing.chunkBytes) || 1024))
  const read = (at: number) => blob.slice(at, Math.min(total, at + chunk)).arrayBuffer()
  let offset = 0
  io.onProgress?.(0, total)
  let next: Promise<ArrayBuffer> | null = total > 0 ? read(0) : null
  while (next) {
    throwIfAborted(io.signal)
    const buf = new Uint8Array(await next)
    const at = offset + buf.length
    next = at < total ? read(at) : null
    throwIfAborted(io.signal)
    const problem = io.problem?.()
    if (problem) throw new Error(problem)
    if (!io.send(buf)) throw new NotConnectedError()
    offset = at
    io.onProgress?.(offset, total)
    if (next) await io.clock.sleep(Math.max(0, pacing.delayMs), io.signal)
  }
  return offset
}

/** Suggested raw pacing for a serial line: 10 bits per byte, 90 % of the baud rate, 64-byte chunks. */
export function serialPacing(baud: number): BinaryPacing {
  const bytesPerSec = Math.max(10, (baud / 10) * 0.9)
  const chunkBytes = 64
  return { chunkBytes, delayMs: Math.max(1, Math.round((chunkBytes / bytesPerSec) * 1000)) }
}
