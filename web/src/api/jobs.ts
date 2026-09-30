/*
 * Long-running server jobs (network tools etc.): POST /api/tools/{tool} → {jobId}; results stream as `job` events on
 * the events socket; POST /api/jobs/{id}/cancel stops a job.
 */
import { api, seg } from './client'
import { events } from '@/lib/events'
import type { JobStarted, ServerEventOf, ToolName } from './types'

export const startTool = (tool: ToolName, params: Record<string, unknown>) =>
  api.post<JobStarted>(`/api/tools/${seg(tool)}`, params)
export const cancelJob = (jobId: string) => api.post<void>(`/api/jobs/${seg(jobId)}/cancel`)

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
