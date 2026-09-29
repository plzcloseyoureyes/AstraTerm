/*
 * Automation feature types (mirror internal/automation/models.go). Snippet / Macro come from @/api/types (SPEC §5.2).
 */

/**
 * A macro step (AUTO-1): the core {data, delayMs} (SPEC §5.2) plus optional wait-for-pattern and stored-secret steps
 * (internal/automation MacroStep). `data` holds the raw keystrokes (escapes decoded).
 */
export interface MacroStep {
  data: string
  delayMs: number
  /** Wait (after the delay) until this RE2 pattern appears in the output before typing. */
  waitFor?: string
  /** Wait timeout (default 30 000 ms). */
  timeoutMs?: number
  /** Type this stored secret of the session's connection (never saved in the macro) before `data`. */
  secret?: string
}

export interface Macro {
  id: string
  name: string
  steps: MacroStep[]
  createdAt: string
  updatedAt: string
}

export interface Script {
  id: string
  name: string
  description: string
  content: string
  createdAt: string
  updatedAt: string
}

export interface TriggerScope {
  connectionIds?: string[]
  protocols?: string[]
  tags?: string[]
}

export type TriggerActionType = 'highlight' | 'notify' | 'sound' | 'send' | 'log' | 'runScript' | 'runSnippet'

export interface TriggerAction {
  type: TriggerActionType
  color?: string
  background?: string
  underline?: boolean
  title?: string
  message?: string
  level?: 'info' | 'success' | 'warning' | 'error'
  desktop?: boolean
  sound?: string
  text?: string
  secret?: string
  enter?: boolean
  scriptId?: string
  snippetId?: string
}

export interface TriggerStats {
  hits: number
  lastHitAt?: string
}

/** What fires a trigger: output lines (regex), or a session event. `command` needs OSC 133 shell integration. */
export type TriggerEventKind = 'output' | 'connect' | 'disconnect' | 'command'

export interface Trigger {
  id: string
  name: string
  enabled: boolean
  event: TriggerEventKind
  /** command triggers: exit-code filter. */
  exit?: 'any' | 'ok' | 'error'
  /** command triggers: only commands that ran at least this long. */
  minDurationSec?: number
  pattern: string
  caseSensitive: boolean
  scope: TriggerScope
  actions: TriggerAction[]
  cooldownMs: number
  once: boolean
  sortOrder: number
  stats: TriggerStats
  createdAt: string
  updatedAt: string
}

export type TriggerInput = Partial<
  Pick<Trigger, 'name' | 'enabled' | 'event' | 'exit' | 'minDurationSec' | 'pattern' | 'caseSensitive' | 'scope' | 'actions' | 'cooldownMs' | 'once' | 'sortOrder'>
> & { confirmDangerous?: boolean }

export interface TriggerLogEntry {
  id: number
  triggerId: string
  triggerName: string
  sessionId?: string
  sessionTitle?: string
  connectionId?: string
  line: string
  ts: string
}

export type JobKind = 'command' | 'snippet' | 'script'
export type ExecMode = 'auto' | 'exec' | 'session'

export interface JobAction {
  kind: JobKind
  command?: string
  snippetId?: string
  scriptId?: string
  variables?: Record<string, string>
  mode?: ExecMode
  timeoutSec?: number
  parallel?: number
  stopOnError?: boolean
}

export type NotifyPolicy = 'never' | 'failure' | 'always'

export interface Schedule {
  id: string
  name: string
  enabled: boolean
  spec: string
  action: JobAction
  connectionIds: string[]
  notify: NotifyPolicy
  lastRunAt?: string
  lastStatus?: string
  lastRunId?: string
  nextRunAt?: string
  createdAt: string
  updatedAt: string
}

export type RunStatus = 'running' | 'ok' | 'error' | 'canceled'
export type HostStatus = 'pending' | 'running' | 'ok' | 'error' | 'skipped'

export interface HostResult {
  connectionId: string
  name: string
  host?: string
  status: HostStatus
  mode?: string
  exitCode?: number
  output?: string
  error?: string
  durationMs: number
}

export interface RunSummary {
  total: number
  ok: number
  failed: number
  skipped: number
}

export interface Run {
  id: string
  kind: 'script' | 'batch'
  refId?: string
  name: string
  origin: 'manual' | 'schedule' | 'trigger'
  target?: string
  status: RunStatus
  jobId?: string
  error?: string
  log?: string
  results?: HostResult[]
  summary: RunSummary
  startedAt: string
  finishedAt?: string
}

export interface LogonAction {
  expect?: string
  send?: string
  secret?: string
  enter?: boolean
  timeoutSec?: number
  optional?: boolean
  delayMs?: number
}

export interface DangerMatch {
  rule: string
  message: string
  severity: 'danger' | 'warning'
  line: string
}

export interface JobStartedWithRun {
  jobId: string
  runId?: string
}

export interface SendResult {
  sessionId: string
  ok: boolean
  error?: string
}

export interface SecretKeys {
  keys: string[]
  injectable: boolean
  locked?: boolean
}

export interface Capabilities {
  scripts: boolean
  mode: 'desktop' | 'server'
  maxTargets: number
  maxParallel: number
  defaultTimeoutSec: number
}

export interface RegexTestResult {
  valid: boolean
  error?: string
  matches: { line: string; match: string; groups: string[] }[]
}

export interface SchedulePreview {
  valid: boolean
  error?: string
  next: string[]
}

// ---- job event payloads (data of {type:'job'} events) ----------------------------------------------------------------

export interface ProgressEvent {
  kind: 'progress' | 'done' | 'warning'
  sessionId: string
  done: number
  total: number
  ok?: boolean
  error?: string
  message?: string
}

export interface ScriptLogEvent {
  kind: 'log'
  level: string
  text: string
  ts: string
  host?: string
}

export interface BatchEvent {
  kind: 'host' | 'summary' | 'log'
  result?: HostResult
  summary?: RunSummary
  level?: string
  text?: string
  host?: string
  ts: string
}

// ---- custom events on /ws/events ------------------------------------------------------------------------------------

export interface TriggerEvent {
  type: 'automation.trigger'
  triggerId: string
  name: string
  event?: TriggerEventKind
  exitCode?: number
  durationMs?: number
  /** The loop guard paused this rule's typing actions on the session. */
  paused?: boolean
  sessionId: string
  sessionTitle: string
  connectionId?: string
  line: string
  match: string
  notify?: { title: string; message?: string; level: 'info' | 'success' | 'warning' | 'error'; desktop?: boolean }
  sound?: string
  logged?: boolean
  stats: TriggerStats
}

export interface RunEvent {
  type: 'automation.run'
  run: Run
}

// ---- client-side configuration (settings section "automation") -------------------------------------------------------

export type AnsiColor =
  | 'black'
  | 'red'
  | 'green'
  | 'yellow'
  | 'blue'
  | 'magenta'
  | 'cyan'
  | 'white'
  | 'brightBlack'
  | 'brightRed'
  | 'brightGreen'
  | 'brightYellow'
  | 'brightBlue'
  | 'brightMagenta'
  | 'brightCyan'
  | 'brightWhite'

/** A keyword-highlighting rule (TERM-15). Colours are #rrggbb or an ANSI colour name of the terminal scheme. */
export interface HighlightRule {
  id: string
  name: string
  pattern: string
  caseSensitive?: boolean
  color?: string
  background?: string
  underline?: boolean
  enabled: boolean
}

export type ButtonActionType = 'send' | 'snippet' | 'macro' | 'script' | 'command'
export type ButtonTarget = 'active' | 'multiexec' | 'all'

export interface QuickButton {
  id: string
  label: string
  color?: string
  action: ButtonActionType
  /** send: text with C escapes (\r = Enter); command: command id. */
  text?: string
  refId?: string
  args?: string
  target?: ButtonTarget
}

export interface ButtonBar {
  id: string
  name: string
  /** Show only for these protocols / connection tags / connections (empty = everywhere). */
  protocols?: string[]
  tags?: string[]
  connectionIds?: string[]
  buttons: QuickButton[]
}

export interface GuardCustomRule {
  pattern: string
  message: string
}
