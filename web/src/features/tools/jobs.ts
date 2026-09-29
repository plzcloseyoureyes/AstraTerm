/*
 * Tool job store. Every backend tool runs as a server job (POST /api/tools/{tool} → {jobId}); its rows stream as
 * `{type:'job', event:'data', data:{rows}}` events. State is kept per tool (not per panel), so switching tools in the
 * Tools tab never loses results or orphans a running job, and the tool list can show which tools are running.
 *
 * - Rows are applied in batches (every ~60 ms) so a fast scan does not re-render per event.
 * - "Latest-only" kinds (progress, round) and keyed kinds (mtr rows by TTL) replace instead of accumulating, so a
 *   one-hour mtr keeps a constant memory footprint.
 * - Events for a job id that is not known yet (they can overtake the POST response) are buffered briefly.
 * - Starting a tool again cancels its previous job; closing the Tools tab cancels every running tool job.
 * - After an events-socket reconnect, running jobs are re-checked against GET /api/jobs (events sent while the socket
 *   was down are lost; a job that ended meanwhile is marked finished instead of spinning forever).
 */
import { create } from 'zustand'
import { api } from '@/api/client'
import { cancelJob, startTool } from '@/api/jobs'
import type { ServerEventOf } from '@/api/types'
import { events } from '@/lib/events'
import { errorMessage } from '@/lib/utils'
import type { ToolRow } from './types'

export type JobStatus = 'idle' | 'starting' | 'running' | 'done' | 'error' | 'canceled'

export interface ToolJobState {
  status: JobStatus
  /** Accumulated rows (latest-only and keyed kinds excluded). */
  rows: ToolRow[]
  /** Last row of every kind seen (progress, round, summary, …). */
  latest: Record<string, ToolRow>
  /** Keyed rows that replace each other, e.g. keyed.mtr[ttl]. */
  keyed: Record<string, Record<string, ToolRow>>
  error?: string
  /** Informational note (e.g. results may be incomplete after a reconnect). */
  note?: string
  jobId?: string
  startedAt?: number
  finishedAt?: number
  /** More rows than MAX_ROWS arrived; the rest were dropped. */
  truncated?: boolean
  /** The user pressed Stop. */
  stopping?: boolean
}

const MAX_ROWS = 100_000
const LATEST_ONLY = new Set(['progress', 'round'])
const KEYED: Record<string, string> = { mtr: 'ttl' }
const EMPTY: ToolJobState = { status: 'idle', rows: [], latest: {}, keyed: {} }

interface JobsStore {
  jobs: Record<string, ToolJobState>
}

export const useToolJobs = create<JobsStore>(() => ({ jobs: {} }))

function getJob(tool: string): ToolJobState {
  return useToolJobs.getState().jobs[tool] ?? EMPTY
}

function setJob(tool: string, patch: Partial<ToolJobState> | ((s: ToolJobState) => Partial<ToolJobState>)): void {
  useToolJobs.setState((st) => {
    const cur = st.jobs[tool] ?? EMPTY
    const next = { ...cur, ...(typeof patch === 'function' ? patch(cur) : patch) }
    return { jobs: { ...st.jobs, [tool]: next } }
  })
}

// ---- event plumbing ------------------------------------------------------------------------------------------------

type JobEvent = ServerEventOf<'job'>

const jobTool = new Map<string, string>() // jobId → tool
const early: { ev: JobEvent; at: number }[] = [] // events for job ids not registered yet
const pending = new Map<string, ToolRow[]>() // tool → rows waiting for the next flush
let flushTimer: ReturnType<typeof setTimeout> | null = null
let listening = false
const runSeq = new Map<string, number>()

function ensureListening(): void {
  if (listening) return
  listening = true
  events.on('job', onJobEvent)
  events.on('hello', () => void recheckRunning())
}

function onJobEvent(ev: JobEvent): void {
  const tool = jobTool.get(ev.jobId)
  if (!tool) {
    const now = Date.now()
    while (early.length && (early.length > 2000 || now - early[0].at > 10_000)) early.shift()
    early.push({ ev, at: now })
    return
  }
  apply(tool, ev)
}

function extractRows(data: unknown): ToolRow[] {
  if (data && typeof data === 'object' && Array.isArray((data as { rows?: unknown }).rows)) {
    return (data as { rows: ToolRow[] }).rows.filter((r) => r && typeof r === 'object' && typeof r.kind === 'string')
  }
  return []
}

function apply(tool: string, ev: JobEvent): void {
  if (getJob(tool).jobId !== ev.jobId) return // a superseded job
  if (ev.event === 'data') {
    const rows = extractRows(ev.data)
    if (!rows.length) return
    const q = pending.get(tool)
    if (q) q.push(...rows)
    else pending.set(tool, rows.slice())
    scheduleFlush()
    return
  }
  flushTool(tool)
  const canceled = ev.event === 'error' && ev.error === 'canceled'
  setJob(tool, (s) => ({
    status: ev.event === 'done' ? (s.stopping ? 'canceled' : 'done') : canceled ? 'canceled' : 'error',
    error: ev.event === 'error' && !canceled ? ev.error || 'The tool failed' : undefined,
    finishedAt: Date.now(),
    stopping: false,
  }))
  jobTool.delete(ev.jobId)
}

function scheduleFlush(): void {
  if (flushTimer) return
  // A timer (not rAF) so hidden tabs keep consuming rows instead of buffering them without bound.
  flushTimer = setTimeout(() => {
    flushTimer = null
    for (const tool of Array.from(pending.keys())) flushTool(tool)
  }, 60)
}

function flushTool(tool: string): void {
  const rows = pending.get(tool)
  if (!rows?.length) return
  pending.delete(tool)
  setJob(tool, (s) => {
    let latest = s.latest
    let keyed = s.keyed
    const append: ToolRow[] = []
    for (const r of rows) {
      if (latest === s.latest) latest = { ...latest }
      latest[r.kind] = r
      const key = KEYED[r.kind]
      if (key) {
        if (keyed === s.keyed) keyed = { ...keyed }
        keyed[r.kind] = { ...keyed[r.kind], [String(r[key])]: r }
        continue
      }
      if (!LATEST_ONLY.has(r.kind)) append.push(r)
    }
    const room = MAX_ROWS - s.rows.length
    const truncated = s.truncated || append.length > room
    return {
      latest,
      keyed,
      rows: append.length ? s.rows.concat(room < append.length ? append.slice(0, Math.max(0, room)) : append) : s.rows,
      truncated,
    }
  })
}

async function recheckRunning(): Promise<void> {
  const running = Object.entries(useToolJobs.getState().jobs).filter(([, s]) => s.status === 'running' && s.jobId)
  if (!running.length) return
  try {
    const list = await api.get<{ id: string }[]>('/api/jobs')
    const alive = new Set(list.map((j) => j.id))
    for (const [tool, s] of running) {
      if (s.jobId && !alive.has(s.jobId)) {
        flushTool(tool)
        jobTool.delete(s.jobId)
        setJob(tool, {
          status: s.stopping ? 'canceled' : 'done',
          finishedAt: Date.now(),
          stopping: false,
          note: 'The connection to the server was interrupted; results may be incomplete.',
        })
      }
    }
  } catch {
    /* next reconnect retries */
  }
}

// ---- actions -------------------------------------------------------------------------------------------------------

/** Start (or restart) a tool; a previous job of the same tool is canceled. Resolves the new job id. */
export async function runTool(tool: string, params: Record<string, unknown>): Promise<string | undefined> {
  ensureListening()
  const prev = getJob(tool)
  if (prev.jobId && (prev.status === 'running' || prev.status === 'starting')) {
    jobTool.delete(prev.jobId)
    void cancelJob(prev.jobId).catch(() => undefined)
  }
  pending.delete(tool)
  const seq = (runSeq.get(tool) ?? 0) + 1
  runSeq.set(tool, seq)
  useToolJobs.setState((st) => ({ jobs: { ...st.jobs, [tool]: { ...EMPTY, status: 'starting', startedAt: Date.now() } } }))
  try {
    const { jobId } = await startTool(tool, params)
    if (runSeq.get(tool) !== seq) {
      void cancelJob(jobId).catch(() => undefined) // superseded while the request was in flight
      return undefined
    }
    jobTool.set(jobId, tool)
    setJob(tool, { jobId, status: 'running' })
    const mine = early.filter((e) => e.ev.jobId === jobId)
    for (let i = early.length - 1; i >= 0; i--) if (early[i].ev.jobId === jobId) early.splice(i, 1)
    for (const e of mine) apply(tool, e.ev)
    return jobId
  } catch (err) {
    if (runSeq.get(tool) === seq) setJob(tool, { status: 'error', error: errorMessage(err), finishedAt: Date.now() })
    return undefined
  }
}

/** Stop a tool's running job (its final rows, e.g. an mtr summary, still arrive). */
export async function cancelTool(tool: string): Promise<void> {
  const s = getJob(tool)
  if (s.status === 'starting') {
    runSeq.set(tool, (runSeq.get(tool) ?? 0) + 1)
    setJob(tool, { status: 'canceled', finishedAt: Date.now() })
    return
  }
  if (s.status !== 'running' || !s.jobId) return
  setJob(tool, { stopping: true })
  try {
    await cancelJob(s.jobId)
  } catch {
    // The job already ended; its final event settles the state. If it never comes, settle now.
    if (getJob(tool).status === 'running') {
      jobTool.delete(s.jobId)
      setJob(tool, { status: 'canceled', stopping: false, finishedAt: Date.now() })
    }
  }
}

/** Clear a tool's results (cancels it when running). */
export function resetTool(tool: string): void {
  const s = getJob(tool)
  if (s.jobId && (s.status === 'running' || s.status === 'starting')) {
    jobTool.delete(s.jobId)
    void cancelJob(s.jobId).catch(() => undefined)
  }
  runSeq.set(tool, (runSeq.get(tool) ?? 0) + 1)
  pending.delete(tool)
  useToolJobs.setState((st) => {
    const jobs = { ...st.jobs }
    delete jobs[tool]
    return { jobs }
  })
}

/** Cancel every running tool job (the Tools tab was closed). Results are dropped. */
export function cancelAllTools(): void {
  for (const tool of Object.keys(useToolJobs.getState().jobs)) resetTool(tool)
}

/** Whether a tool currently has a running job (for the tool list). */
export function useToolRunning(tool: string): boolean {
  return useToolJobs((st) => {
    const s = st.jobs[tool]
    return !!s && (s.status === 'running' || s.status === 'starting')
  })
}
