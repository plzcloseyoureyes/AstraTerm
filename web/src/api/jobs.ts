/*
 * Long-running server jobs (network tools etc.): POST /api/tools/{tool} → {jobId}; results stream as `job` events on
 * the events socket; POST /api/jobs/{id}/cancel stops a job.
 */
import { useCallback, useEffect, useRef, useState } from 'react'
import { api, seg } from './client'
import { events } from '@/lib/events'
import type { JobStarted, ServerEventOf, ToolName } from './types'

export const startTool = (tool: ToolName, params: Record<string, unknown>) =>
  api.post<JobStarted>(`/api/tools/${seg(tool)}`, params)
export const cancelJob = (jobId: string) => api.post<void>(`/api/jobs/${seg(jobId)}/cancel`)

export type JobStatus = 'idle' | 'starting' | 'running' | 'done' | 'error' | 'canceled'

export interface JobState<T> {
  jobId?: string
  status: JobStatus
  items: T[]
  error?: string
}

type JobEvent = ServerEventOf<'job'>

/**
 * Watch a job's events. Events for unknown job ids that arrive before `startTool` resolves are buffered briefly so
 * nothing is lost to the race between the HTTP response and the first websocket frame.
 */
export function watchJob(jobId: string, onEvent: (ev: JobEvent) => void): () => void {
  return events.on('job', (ev) => {
    if (ev.jobId === jobId) onEvent(ev)
  })
}

/**
 * React helper to run a tool job and collect its streamed `data` items.
 *
 *   const job = useJob<PingReply>()
 *   job.start('ping', {host, count: 4})
 *   job.state.items / job.state.status / job.cancel()
 */
export function useJob<T = unknown>(opts: { maxItems?: number } = {}) {
  const maxItems = opts.maxItems ?? 10_000
  const [state, setState] = useState<JobState<T>>({ status: 'idle', items: [] })
  const jobRef = useRef<string | undefined>(undefined)
  const buffer = useRef<JobEvent[]>([])
  const unsubRef = useRef<(() => void) | null>(null)

  const apply = useCallback(
    (ev: JobEvent) => {
      setState((s) => {
        if (ev.event === 'data') {
          const items = s.items.length >= maxItems ? [...s.items.slice(1), ev.data as T] : [...s.items, ev.data as T]
          return { ...s, items }
        }
        if (ev.event === 'done') return { ...s, status: s.status === 'canceled' ? 'canceled' : 'done' }
        return { ...s, status: 'error', error: ev.error || 'Job failed' }
      })
    },
    [maxItems],
  )

  const stop = useCallback(() => {
    unsubRef.current?.()
    unsubRef.current = null
  }, [])

  useEffect(() => stop, [stop])

  const start = useCallback(
    async (tool: ToolName, params: Record<string, unknown>) => {
      stop()
      buffer.current = []
      jobRef.current = undefined
      setState({ status: 'starting', items: [] })
      // Listen before the request so early events are buffered.
      unsubRef.current = events.on('job', (ev) => {
        if (!jobRef.current) {
          buffer.current.push(ev)
          if (buffer.current.length > 1000) buffer.current.shift()
          return
        }
        if (ev.jobId === jobRef.current) apply(ev)
      })
      try {
        const { jobId } = await startTool(tool, params)
        jobRef.current = jobId
        setState((s) => ({ ...s, jobId, status: 'running' }))
        for (const ev of buffer.current) if (ev.jobId === jobId) apply(ev)
        buffer.current = []
        return jobId
      } catch (err) {
        stop()
        setState({ status: 'error', items: [], error: err instanceof Error ? err.message : String(err) })
        return undefined
      }
    },
    [apply, stop],
  )

  const cancel = useCallback(async () => {
    const id = jobRef.current
    if (!id) return
    setState((s) => ({ ...s, status: 'canceled' }))
    try {
      await cancelJob(id)
    } catch {
      /* job may already be finished */
    }
  }, [])

  const reset = useCallback(() => {
    stop()
    jobRef.current = undefined
    setState({ status: 'idle', items: [] })
  }, [stop])

  return { state, start, cancel, reset }
}
