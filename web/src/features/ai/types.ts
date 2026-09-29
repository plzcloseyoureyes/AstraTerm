/*
 * AI assistant types (feature-local; mirrors internal/ai JSON).
 */

export type AiMode = 'chat' | 'command' | 'explain'
export type Risk = 'low' | 'medium' | 'high'

export interface AiModel {
  id: string
  label?: string
  hint?: string
}

/** GET /api/ai/status — never triggers a network call to the provider. */
export interface AiStatus {
  available: boolean
  enabled: boolean
  configured: boolean
  reason?: 'disabled' | 'not_configured' | 'locked'
  provider?: 'anthropic' | 'openai'
  preset?: string
  model?: string
  source: 'global' | 'user'
  models: AiModel[]
  canPickModel: boolean
  canConfigure: boolean
  canConfigureGlobal: boolean
  mode: 'desktop' | 'server'
  locked: boolean
  limits: { perMinute: number; perDay: number }
}

/** Stored provider configuration of one scope (GET/PUT /api/ai/config). */
export interface AiConfig {
  enabled?: boolean
  provider?: 'anthropic' | 'openai' | ''
  preset?: string
  baseUrl?: string
  model?: string
  effort?: '' | 'auto' | 'low' | 'medium' | 'high'
  maxTokens?: number
  models?: string[]
  allowUserConfig?: boolean
  allowUserModel?: boolean
  rateLimitPerMinute?: number
  rateLimitPerDay?: number
  redactPatterns?: string[]
}

export interface AiConfigView extends AiConfig {
  scope: 'global' | 'user'
  hasKey: boolean
  allowed: boolean
}

export interface SessionInfo {
  title?: string
  protocol?: string
  host?: string
  username?: string
  os?: string
  kernel?: string
  platform?: string
  arch?: string
  shell?: string
  cwd?: string
}

/** Context sent with a request (POST /api/ai/chat `context`). */
export interface ChatContextPayload {
  sessionId?: string
  terminalText?: string
  selection?: string
  sessionInfo?: SessionInfo
  file?: { path?: string; language?: string; content: string }
  command?: string
  exitCode?: number
}

export interface WireMessage {
  role: 'user' | 'assistant'
  content: string
}

export interface CommandResult {
  command: string
  explanation: string
  risk: Risk
  riskReason?: string
  parsed: boolean
}

export interface StreamMeta {
  provider: string
  model: string
  mode: AiMode
  redactions: number
  contextChars: number
}

export type StreamEvent =
  | { type: 'meta'; data: StreamMeta }
  | { type: 'thinking' }
  | { type: 'delta'; text: string }
  | { type: 'result'; data: CommandResult }
  | { type: 'done'; stopReason?: string; usage?: { inputTokens: number; outputTokens: number } }
  | { type: 'error'; error: string; code: string }

/** A context chip attached to the composer. */
export type ContextChip =
  | { kind: 'terminal'; id: string; tabId: string; sessionId: string; label: string; lines: number }
  | { kind: 'selection'; id: string; label: string; text: string; tabId?: string; sessionId?: string }
  | { kind: 'file'; id: string; label: string; path: string; fsId?: string; content?: string; language?: string }
  | { kind: 'failure'; id: string; label: string; command: string; exitCode?: number; output: string; tabId?: string; sessionId?: string }

export interface ChatMessage {
  id: string
  role: 'user' | 'assistant'
  content: string
  /** Prompt actually sent when `content` shows a short label instead (e.g. "Explain this output"). */
  prompt?: string
  createdAt: number
  mode?: AiMode
  /** Labels of the context attached to a user message (the payload itself is not persisted). */
  context?: { kind: ContextChip['kind']; label: string }[]
  redactions?: number
  status?: 'waiting' | 'thinking' | 'streaming' | 'done' | 'stopped' | 'error'
  error?: { message: string; code: string }
  stopReason?: string
  model?: string
}

export interface Conversation {
  id: string
  title: string
  createdAt: number
  updatedAt: number
  messages: ChatMessage[]
}
