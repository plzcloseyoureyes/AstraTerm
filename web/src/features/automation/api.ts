/*
 * REST client of the automation module (internal/automation): snippets & macros (SPEC §6.0), scripts, triggers,
 * schedules, run history, secret injection, paced sends and helpers. React-query hooks share the keys below.
 */
import { useQuery } from '@tanstack/react-query'
import { api, isApiError, seg } from '@/api/client'
import { events } from '@/lib/events'
import { queryKeys } from '@/api/queryKeys'
import type { JobStarted, ServerEventOf, Snippet } from '@/api/types'
import type {
  Capabilities,
  DangerMatch,
  JobAction,
  JobStartedWithRun,
  Macro,
  MacroStep,
  RegexTestResult,
  Run,
  Schedule,
  SchedulePreview,
  Script,
  SecretKeys,
  SendResult,
  Trigger,
  TriggerInput,
  TriggerLogEntry,
} from './types'

export const autoKeys = {
  snippets: queryKeys.snippets,
  macros: queryKeys.macros,
  scripts: ['automation', 'scripts'] as const,
  script: (id: string) => ['automation', 'scripts', id] as const,
  triggers: ['automation', 'triggers'] as const,
  triggerLog: (id?: string) => ['automation', 'trigger-log', id ?? ''] as const,
  schedules: ['automation', 'schedules'] as const,
  runs: (filter?: Record<string, string | undefined>) => ['automation', 'runs', filter ?? {}] as const,
  runsAll: ['automation', 'runs'] as const,
  run: (id: string) => ['automation', 'run', id] as const,
  secretKeys: (sessionId: string) => ['automation', 'secret-keys', sessionId] as const,
  capabilities: ['automation', 'capabilities'] as const,
}

// ---- snippets -------------------------------------------------------------------------------------------------------

export type SnippetInput = Partial<Pick<Snippet, 'name' | 'folder' | 'description' | 'content' | 'tags' | 'sendMode' | 'shortcut'>>

export const listSnippets = () => api.get<Snippet[]>('/api/snippets')
export const createSnippet = (s: SnippetInput) => api.post<Snippet>('/api/snippets', s)
export const updateSnippet = (id: string, s: SnippetInput) => api.patch<Snippet>(`/api/snippets/${seg(id)}`, s)
export const deleteSnippet = (id: string) => api.del<void>(`/api/snippets/${seg(id)}`)
export const runSnippetOnSessions = (
  id: string,
  body: { sessionIds: string[]; variables?: Record<string, string>; sendMode?: Snippet['sendMode']; confirmDangerous?: boolean },
) => api.post<{ results: SendResult[] }>(`/api/snippets/${seg(id)}/run`, body)

export function useSnippets(enabled = true) {
  return useQuery({ queryKey: autoKeys.snippets, queryFn: listSnippets, enabled, staleTime: 30_000 })
}

// ---- macros ---------------------------------------------------------------------------------------------------------

export const listMacros = () => api.get<Macro[]>('/api/macros')
export const createMacro = (m: { name: string; steps: MacroStep[] }) => api.post<Macro>('/api/macros', m)
export const updateMacro = (id: string, m: Partial<{ name: string; steps: MacroStep[] }>) => api.patch<Macro>(`/api/macros/${seg(id)}`, m)
export const deleteMacro = (id: string) => api.del<void>(`/api/macros/${seg(id)}`)
export const runMacroOnSessions = (id: string, body: { sessionIds: string[]; speed?: number; confirmDangerous?: boolean }) =>
  api.post<JobStarted>(`/api/macros/${seg(id)}/run`, body)

export function useMacros(enabled = true) {
  return useQuery({ queryKey: autoKeys.macros, queryFn: listMacros, enabled, staleTime: 30_000 })
}

// ---- secrets, sends, guard ------------------------------------------------------------------------------------------

export const getSecretKeys = (sessionId: string) => api.get<SecretKeys>(`/api/sessions/${seg(sessionId)}/secret-keys`, { noVaultPrompt: true })
export const injectSecret = (sessionId: string, key: string, enter = true) =>
  api.post<void>(`/api/sessions/${seg(sessionId)}/inject-secret`, { key, enter })

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

export const pacedSend = (req: PacedSendRequest) => api.post<JobStarted>('/api/automation/send', req)
export const regexTest = (pattern: string, caseSensitive: boolean, text = '') =>
  api.post<RegexTestResult>('/api/automation/regex/test', { pattern, caseSensitive, text })
const getCapabilities = () => api.get<Capabilities>('/api/automation/capabilities')

export function useCapabilities() {
  return useQuery({ queryKey: autoKeys.capabilities, queryFn: getCapabilities, staleTime: 60_000 })
}

/** Matches of a 409 {code:'dangerous_command'} answer (null for other errors). */
export function dangerousMatches(err: unknown): DangerMatch[] | null {
  if (!isApiError(err) || err.status !== 409 || err.code !== 'dangerous_command') return null
  const body = err.body as { matches?: DangerMatch[] } | undefined
  return Array.isArray(body?.matches) ? body.matches : []
}

// ---- scripts --------------------------------------------------------------------------------------------------------

export type ScriptInput = Partial<Pick<Script, 'name' | 'description' | 'content'>>

export interface ScriptRunRequest {
  content?: string
  name?: string
  sessionId?: string
  connectionId?: string
  variables?: Record<string, string>
  timeoutSec?: number
}

const listScripts = () => api.get<Script[]>('/api/scripts')
export const createScript = (s: ScriptInput) => api.post<Script>('/api/scripts', s)
export const updateScript = (id: string, s: ScriptInput) => api.patch<Script>(`/api/scripts/${seg(id)}`, s)
export const deleteScript = (id: string) => api.del<void>(`/api/scripts/${seg(id)}`)
export const runScript = (id: string, req: ScriptRunRequest) => api.post<JobStartedWithRun>(`/api/scripts/${seg(id)}/run`, req)

export function useScripts(enabled = true) {
  return useQuery({ queryKey: autoKeys.scripts, queryFn: listScripts, enabled, staleTime: 30_000 })
}

// ---- triggers -------------------------------------------------------------------------------------------------------

const listTriggers = () => api.get<Trigger[]>('/api/automation/triggers')
export const createTrigger = (t: TriggerInput) => api.post<Trigger>('/api/automation/triggers', t)
export const updateTrigger = (id: string, t: TriggerInput) => api.patch<Trigger>(`/api/automation/triggers/${seg(id)}`, t)
export const deleteTrigger = (id: string) => api.del<void>(`/api/automation/triggers/${seg(id)}`)
const listTriggerLog = (triggerId?: string, limit = 200) =>
  api.get<TriggerLogEntry[]>('/api/automation/trigger-log', { query: { triggerId, limit } })
export const clearTriggerLog = () => api.del<void>('/api/automation/trigger-log')

export function useTriggers(enabled = true) {
  return useQuery({ queryKey: autoKeys.triggers, queryFn: listTriggers, enabled, staleTime: 30_000 })
}

export function useTriggerLog(triggerId?: string, enabled = true) {
  return useQuery({ queryKey: autoKeys.triggerLog(triggerId), queryFn: () => listTriggerLog(triggerId), enabled })
}

// ---- batch, schedules, runs -----------------------------------------------------------------------------------------

export interface BatchRequest extends JobAction {
  name?: string
  connectionIds: string[]
  confirmDangerous?: boolean
}

export type ScheduleInput = Partial<Pick<Schedule, 'name' | 'enabled' | 'spec' | 'action' | 'connectionIds' | 'notify'>> & {
  confirmDangerous?: boolean
}

export const startBatch = (req: BatchRequest) => api.post<JobStartedWithRun>('/api/automation/batch', req)
const listSchedules = () => api.get<Schedule[]>('/api/automation/schedules')
export const createSchedule = (s: ScheduleInput) => api.post<Schedule>('/api/automation/schedules', s)
export const updateSchedule = (id: string, s: ScheduleInput) => api.patch<Schedule>(`/api/automation/schedules/${seg(id)}`, s)
export const deleteSchedule = (id: string) => api.del<void>(`/api/automation/schedules/${seg(id)}`)
export const runScheduleNow = (id: string) => api.post<JobStartedWithRun>(`/api/automation/schedules/${seg(id)}/run`)
export const previewSchedule = (spec: string, count = 5) =>
  api.get<SchedulePreview>('/api/automation/schedules/preview', { query: { spec, count } })

const listRuns = (filter: { kind?: string; refId?: string; origin?: string; limit?: number; before?: string } = {}) =>
  api.get<Run[]>('/api/automation/runs', { query: filter })
const getRun = (id: string) => api.get<Run>(`/api/automation/runs/${seg(id)}`)
export const deleteRun = (id: string) => api.del<void>(`/api/automation/runs/${seg(id)}`)
export const clearRuns = () => api.del<void>('/api/automation/runs')

export function useSchedules(enabled = true) {
  return useQuery({ queryKey: autoKeys.schedules, queryFn: listSchedules, enabled, refetchInterval: 60_000 })
}

export function useRuns(filter: { kind?: string; refId?: string; origin?: string } = {}, enabled = true) {
  return useQuery({ queryKey: autoKeys.runs(filter), queryFn: () => listRuns({ ...filter, limit: 200 }), enabled })
}

export function useRun(id: string | undefined) {
  // placeholderData: undefined — never show another run's record while this one loads.
  return useQuery({ queryKey: autoKeys.run(id ?? ''), queryFn: () => getRun(id!), enabled: !!id, placeholderData: undefined })
}

// ---- generic mutation helper ----------------------------------------------------------------------------------------

// ---- job events -----------------------------------------------------------------------------------------------------

type JobEvent = ServerEventOf<'job'>

const recentJobEvents: JobEvent[] = []
let jobBufferInstalled = false

/** Keep the last job events so a watcher that subscribes after a (fast) job started still sees all of them. */
export function installJobBuffer(): void {
  if (jobBufferInstalled) return
  jobBufferInstalled = true
  events.on('job', (ev) => {
    recentJobEvents.push(ev)
    if (recentJobEvents.length > 1000) recentJobEvents.splice(0, recentJobEvents.length - 1000)
  })
}

/** Watch a job's events, replaying those that arrived before the call. Returns unsubscribe. */
export function watchJobEvents(jobId: string, cb: (ev: JobEvent) => void): () => void {
  installJobBuffer()
  for (const ev of recentJobEvents) if (ev.jobId === jobId) cb(ev)
  return events.on('job', (ev) => {
    if (ev.jobId === jobId) cb(ev)
  })
}
