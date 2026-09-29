/*
 * Server-side paced sending of the automation module (POST /api/automation/send, AUTO-6 / TERM-17 / CC-8): text is
 * typed line by line by the backend, so background-tab timer throttling cannot slow it down, and waiting for the
 * prompt uses the session's own OSC 133 marks. Progress arrives as `job` events; POST /api/jobs/{id}/cancel stops it.
 *
 * Optional: when the endpoint is missing (older server / module absent) the browser pacer is used instead.
 */
import { api, isApiError, seg } from '@/api/client'
import { events } from '@/lib/events'

/** The automation pacer's per-request limit (bytes of text). */
export const SERVER_PACER_MAX = 1024 * 1024

export interface PacedSendRequest {
  sessionIds: string[]
  text: string
  lineDelayMs?: number
  charDelayMs?: number
  waitPrompt?: boolean
  promptPattern?: string
  promptTimeoutMs?: number
  enter?: boolean
  confirmDangerous?: boolean
}

export interface PacedProgress {
  kind: 'progress' | 'warning' | 'done'
  sessionId?: string
  done?: number
  total?: number
  ok?: boolean
  error?: string
  message?: string
}

export interface DangerMatch {
  rule?: string
  message?: string
  severity?: string
  line?: string
}

export class DangerousTextError extends Error {
  readonly matches: DangerMatch[]
  constructor(message: string, matches: DangerousTextError['matches']) {
    super(message)
    this.name = 'DangerousTextError'
    this.matches = matches
  }
}

export class PacerUnavailableError extends Error {
  constructor(message = 'The server-side pacer is not available') {
    super(message)
    this.name = 'PacerUnavailableError'
  }
}

let available: { value: boolean; at: number } | null = null

/** Is the automation pacer mounted? Cached for five minutes. */
export async function serverPacerAvailable(): Promise<boolean> {
  if (available && Date.now() - available.at < 5 * 60_000) return available.value
  let value = false
  try {
    await api.get('/api/automation/capabilities', { noVaultPrompt: true })
    value = true
  } catch (err) {
    value = !(isApiError(err) && (err.status === 404 || err.status === 405))
  }
  available = { value, at: Date.now() }
  return value
}

function normalizeMatches(body: unknown): DangerMatch[] {
  const m = (body as { matches?: unknown } | null)?.matches
  if (!Array.isArray(m)) return []
  return m.slice(0, 20).map((x) => {
    const o = (x && typeof x === 'object' ? x : {}) as Record<string, unknown>
    const str = (k: string) => (typeof o[k] === 'string' ? (o[k] as string).slice(0, 500) : undefined)
    return { rule: str('rule'), message: str('message'), severity: str('severity'), line: str('line') }
  })
}

export interface PacedJob {
  jobId: string
  cancel(): Promise<void>
  done: Promise<{ ok: boolean; error?: string; warnings: number }>
}

/**
 * Start a paced send and follow its job events. Throws DangerousTextError (409 dangerous_command — resend with
 * confirmDangerous), PacerUnavailableError (endpoint missing) or the API error.
 */
export async function startPacedSend(req: PacedSendRequest, onProgress: (p: PacedProgress) => void): Promise<PacedJob> {
  // Listen before the request: job events may arrive before the response (SPEC §9 events note).
  let jobId: string | null = null
  const early: { event: string; data?: unknown; error?: string; jobId: string }[] = []
  let warnings = 0
  let settle: ((v: { ok: boolean; error?: string; warnings: number }) => void) | null = null
  const done = new Promise<{ ok: boolean; error?: string; warnings: number }>((r) => (settle = r))
  let finished = false
  let lastError: string | null = null
  const handle = (ev: { event: string; data?: unknown; error?: string }) => {
    if (finished) return
    if (ev.event === 'data') {
      const d = (ev.data ?? {}) as PacedProgress
      if (d.kind === 'warning') warnings++
      onProgress(d)
      if (d.kind === 'done' && d.ok === false && d.error) lastError = d.error
      return
    }
    finished = true
    unsubscribe()
    settle?.(ev.event === 'done' ? { ok: !lastError, error: lastError ?? undefined, warnings } : { ok: false, error: ev.error || lastError || 'The paced send failed', warnings })
  }
  const unsubscribe = events.on('job', (ev) => {
    if (!jobId) {
      early.push(ev)
      if (early.length > 500) early.shift()
      return
    }
    if (ev.jobId === jobId) handle(ev)
  })
  try {
    const res = await api.post<{ jobId: string }>('/api/automation/send', req)
    jobId = res.jobId
  } catch (err) {
    unsubscribe()
    if (isApiError(err) && err.code === 'dangerous_command') throw new DangerousTextError(err.message, normalizeMatches(err.body))
    if (isApiError(err) && (err.status === 404 || err.status === 405) && /no such endpoint|not allowed/i.test(err.message)) {
      available = { value: false, at: Date.now() }
      throw new PacerUnavailableError()
    }
    throw err
  }
  for (const ev of early.splice(0)) if (ev.jobId === jobId) handle(ev)
  const id = jobId
  return {
    jobId: id,
    done,
    cancel: async () => {
      await api.post(`/api/jobs/${seg(id)}/cancel`).catch(() => undefined)
    },
  }
}
