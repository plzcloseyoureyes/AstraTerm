/*
 * Client-side asciicast helpers: parse v2/v3 into absolute-time events (markers, search-to-seek, duration) and render
 * the terminal screen at a given time in a headless xterm ("copy text at time").
 */

export type CastEventCode = 'o' | 'i' | 'r' | 'm' | 'x' | string

export interface ParsedCast {
  version: 2 | 3
  cols: number
  rows: number
  title?: string
  timestamp?: number
  /** [absolute seconds, code, data] */
  events: [number, CastEventCode, string][]
  duration: number
}

export interface CastMarker {
  time: number
  label: string
  index: number
}

/** Parse asciicast v2 / v3 text; malformed lines (e.g. a partial last line) are skipped. */
export function parseCast(text: string): ParsedCast | null {
  const nl = text.indexOf('\n')
  const headLine = nl < 0 ? text : text.slice(0, nl)
  let head: Record<string, unknown>
  try {
    head = JSON.parse(headLine) as Record<string, unknown>
  } catch {
    return null
  }
  const version = head.version === 3 ? 3 : head.version === 2 ? 2 : null
  if (!version) return null
  const term = (head.term ?? {}) as { cols?: number; rows?: number }
  const cols = Number(version === 3 ? term.cols : head.width) || 80
  const rows = Number(version === 3 ? term.rows : head.height) || 24
  const events: ParsedCast['events'] = []
  let t = 0
  let pos = nl < 0 ? text.length : nl + 1
  while (pos < text.length) {
    let end = text.indexOf('\n', pos)
    if (end < 0) end = text.length
    const line = text.slice(pos, end)
    pos = end + 1
    if (!line || line[0] !== '[') continue
    let ev: unknown
    try {
      ev = JSON.parse(line)
    } catch {
      continue
    }
    if (!Array.isArray(ev) || ev.length < 3 || typeof ev[0] !== 'number' || typeof ev[1] !== 'string') continue
    t = version === 3 ? t + ev[0] : ev[0]
    events.push([t, ev[1], typeof ev[2] === 'string' ? ev[2] : String(ev[2])])
  }
  return {
    version,
    cols,
    rows,
    title: typeof head.title === 'string' ? head.title : undefined,
    timestamp: typeof head.timestamp === 'number' ? head.timestamp : undefined,
    events,
    duration: events.length ? events[events.length - 1][0] : 0,
  }
}

/**
 * Recording time ↔ player time when pauses longer than `limit` seconds are shortened (asciinema-player's
 * idleTimeLimit: every gap between events, the first one included, is capped at the limit). The player's clock,
 * `seek` and duration are in player time; markers, search hits and `startAt` are recorded (raw) times.
 */
export interface IdleTimeline {
  duration: number
  toPlayer: (raw: number) => number
  toRaw: (player: number) => number
}

export function idleTimeline(c: Pick<ParsedCast, 'events' | 'duration'>, limit: number): IdleTimeline {
  if (!(limit > 0)) return { duration: c.duration, toPlayer: (t) => Math.max(0, t), toRaw: (t) => Math.max(0, t) }
  // Every capped gap: raw start a, raw end b and the player time at a (outside the gaps time runs 1:1).
  const gaps: { a: number; b: number; pa: number }[] = []
  let prev = 0
  let shift = 0
  for (const [t] of c.events) {
    if (t - prev > limit) {
      gaps.push({ a: prev, b: t, pa: prev - shift })
      shift += t - prev - limit
    }
    prev = t
  }
  /** The last gap whose start (by `key`) is ≤ x. */
  const find = (key: 'a' | 'pa', x: number) => {
    let lo = 0
    let hi = gaps.length - 1
    let hit = -1
    while (lo <= hi) {
      const mid = (lo + hi) >> 1
      if (gaps[mid][key] <= x) {
        hit = mid
        lo = mid + 1
      } else hi = mid - 1
    }
    return hit < 0 ? null : gaps[hit]
  }
  const toPlayer = (t: number) => {
    t = Math.max(0, t)
    const g = find('a', t)
    if (!g) return t
    return t < g.b ? g.pa + Math.min(t - g.a, limit) : g.pa + limit + (t - g.b)
  }
  const toRaw = (p: number) => {
    p = Math.max(0, p)
    const g = find('pa', p)
    if (!g) return p
    return p < g.pa + limit ? g.a + (p - g.pa) : g.b + (p - g.pa - limit)
  }
  return { duration: toPlayer(c.duration), toPlayer, toRaw }
}

/** Terminal escape sequences (CSI, OSC, DCS/PM/APC, two-byte ESC). */
// eslint-disable-next-line no-control-regex
const ESC_SEQ = /\x1b\[[0-?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b[P^_X][^\x1b]*\x1b\\|\x1b[@-Z\\-_]/g
/** Escape sequences plus the remaining C0 controls except \t, \n and \r. */
// eslint-disable-next-line no-control-regex
const ANSI_SEQ = new RegExp(`${ESC_SEQ.source}|[\x00-\x08\x0b-\x0c\x0e-\x1f\x7f]`, 'g')

/** OSC 133;A — shell integration's "prompt starts here". */
// eslint-disable-next-line no-control-regex
const PROMPT_START = /\x1b\]133;A(?:;[^\x07\x1b]*)?(?:\x07|\x1b\\)/

/**
 * The line after every prompt start in the output — the prompt and the command typed at it ("~/app$ make build"), or
 * just the command when the shell also marks where it starts (OSC 133;B) — rendered by a tiny line editor: echoed text
 * overwrites at the cursor, \b moves it back, \r returns, ESC[K erases the rest.
 */
function promptLines(c: ParsedCast): string[] {
  const lines: string[] = []
  let line: string[] | null = null
  let cur = 0
  const finish = () => {
    if (line) lines.push(line.join('').trim().slice(0, 200))
    line = null
  }
  // Sequences the editor understands become private control characters; every other one is dropped.
  const token = (m: string) => (m === '\x1b[K' || m === '\x1b[0K' ? '\x0b' : m.startsWith('\x1b]133;B') ? '\x0c' : '')
  const feed = (text: string) => {
    for (const ch of text.replace(ESC_SEQ, token)) {
      if (!line) return
      if (ch === '\n') finish()
      else if (ch === '\x0c') [line, cur] = [[], 0] // OSC 133;B: the prompt ends, the command starts
      else if (ch === '\r') cur = 0
      else if (ch === '\b') cur = Math.max(0, cur - 1)
      else if (ch === '\x0b') line.length = cur
      else if (ch >= ' ') line[cur++] = ch
    }
  }
  for (const [, code, data] of c.events) {
    if (code !== 'o') continue
    const parts = data.split(PROMPT_START)
    feed(parts[0])
    for (const part of parts.slice(1)) {
      finish()
      line = []
      cur = 0
      feed(part)
    }
  }
  finish()
  return lines
}

/**
 * The markers of a recording. Prompt marks (the recorder writes one unlabelled marker per OSC 133;A) are named after
 * their prompt line.
 */
export function castMarkers(c: ParsedCast): CastMarker[] {
  const prompts = promptLines(c)
  let next = 0
  const out: CastMarker[] = []
  for (const [t, code, data] of c.events) if (code === 'm') out.push({ time: t, label: data || prompts[next++] || '', index: out.length })
  return out
}

/** Plain lines of the output with the time each line started. */
export function transcript(parsed: ParsedCast): { time: number; text: string }[] {
  const out: { time: number; text: string }[] = []
  let cur = ''
  let start = -1
  for (const [t, code, data] of parsed.events) {
    if (code !== 'o') continue
    const clean = data.replace(ANSI_SEQ, '')
    for (const ch of clean.split(/(\n)/)) {
      if (ch === '\n') {
        out.push({ time: start < 0 ? t : start, text: cur })
        cur = ''
        start = -1
      } else if (ch) {
        if (start < 0) start = t
        const cr = ch.lastIndexOf('\r')
        cur = cr >= 0 ? ch.slice(cr + 1) || cur : cur + ch
      }
    }
  }
  if (cur) out.push({ time: start < 0 ? 0 : start, text: cur })
  return out
}

/** The visible screen at `time` seconds (plus the scrollback when withScrollback), rendered by a headless xterm. */
export async function screenAt(c: ParsedCast, time: number, withScrollback = false): Promise<string> {
  const { Terminal } = await import('@xterm/xterm')
  const term = new Terminal({ cols: c.cols, rows: c.rows, scrollback: withScrollback ? 10000 : 0, allowProposedApi: true })
  try {
    let batch = ''
    const flush = () =>
      new Promise<void>((resolve) => {
        const data = batch
        batch = ''
        term.write(data, resolve)
      })
    for (const [t, code, data] of c.events) {
      if (t > time + 1e-6) break
      if (code === 'o') {
        batch += data
        if (batch.length > 1 << 20) await flush()
      } else if (code === 'r') {
        await flush()
        const m = /^(\d+)x(\d+)$/.exec(data)
        if (m) term.resize(Math.max(2, Math.min(1000, +m[1])), Math.max(1, Math.min(1000, +m[2])))
      }
    }
    await flush()
    const b = term.buffer.active
    const from = withScrollback ? 0 : b.baseY
    const lines: string[] = []
    for (let i = from; i < b.baseY + term.rows; i++) lines.push(b.getLine(i)?.translateToString(true) ?? '')
    while (lines.length && !lines[lines.length - 1].trim()) lines.pop()
    return lines.join('\n')
  } finally {
    term.dispose()
  }
}
