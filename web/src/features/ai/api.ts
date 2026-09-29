/*
 * REST + streaming client of the AI module (internal/ai).
 *
 *   GET  /api/ai/status          → AiStatus (no provider call)
 *   GET  /api/ai/config?scope=   → AiConfigView (API key is write-only: hasKey only)
 *   PUT  /api/ai/config          {scope, …AiConfig, apiKey?, reset?}
 *   POST /api/ai/test            {scope, …draft, apiKey?} → {ok, latencyMs, model, reply}
 *   GET  /api/ai/models?scope=   → {models, source, error?}
 *   POST /api/ai/chat            {messages, context, mode, model?} → text/event-stream (meta/thinking/delta/result/done/error)
 */
import { useQuery } from '@tanstack/react-query'
import { api, ApiError, request } from '@/api/client'
import type { AiConfig, AiConfigView, AiMode, AiModel, AiStatus, ChatContextPayload, StreamEvent, WireMessage } from './types'

export const aiKeys = {
  status: ['ai', 'status'] as const,
  config: (scope: 'global' | 'user') => ['ai', 'config', scope] as const,
  models: (scope: string) => ['ai', 'models', scope] as const,
}

export const getStatus = () => api.get<AiStatus>('/api/ai/status')
export const getConfig = (scope: 'global' | 'user') => api.get<AiConfigView>('/api/ai/config', { query: { scope } })
export const putConfig = (body: AiConfig & { scope: 'global' | 'user'; apiKey?: string; reset?: boolean }) =>
  api.put<AiConfigView>('/api/ai/config', body)
export const testConfig = (body: AiConfig & { scope: 'global' | 'user'; apiKey?: string }) =>
  api.post<{ ok: boolean; latencyMs: number; model: string; reply?: string }>('/api/ai/test', body)
export const listModels = (scope?: 'global' | 'user') =>
  api.get<{ models: AiModel[]; source: 'provider' | 'builtin'; error?: string }>('/api/ai/models', { query: { scope } })

export function useAiConfig(scope: 'global' | 'user', enabled = true) {
  return useQuery({ queryKey: aiKeys.config(scope), queryFn: () => getConfig(scope), enabled, staleTime: 10_000 })
}

export function useAiModels(scope: 'global' | 'user' | undefined, enabled: boolean) {
  return useQuery({
    queryKey: aiKeys.models(scope ?? 'effective'),
    queryFn: () => listModels(scope),
    enabled,
    staleTime: 5 * 60_000,
    retry: false,
  })
}

export interface ChatRequest {
  messages: WireMessage[]
  context?: ChatContextPayload
  mode: AiMode
  model?: string
}

/**
 * POST /api/ai/chat and dispatch the server-sent events. Resolves when the stream ends; rejects with ApiError for
 * HTTP errors before streaming (the 423 unlock-and-retry flow of the API client applies) and with an AbortError when
 * `signal` fires.
 */
export async function streamChat(req: ChatRequest, onEvent: (ev: StreamEvent) => void, signal?: AbortSignal): Promise<void> {
  const res = await request<Response>('POST', '/api/ai/chat', req, { as: 'response', signal, headers: { Accept: 'text/event-stream' } })
  if (!res.body) throw new ApiError(0, 'network_error', 'The browser does not support streamed responses')
  const reader = res.body.pipeThrough(new TextDecoderStream()).getReader()
  let buf = ''
  let event = ''
  let data: string[] = []
  let ended = false
  const dispatch = () => {
    if (!event && !data.length) return
    const name = event || 'message'
    const raw = data.join('\n')
    event = ''
    data = []
    let payload: any = {}
    try {
      payload = raw ? JSON.parse(raw) : {}
    } catch {
      return
    }
    switch (name) {
      case 'meta':
        onEvent({ type: 'meta', data: payload })
        break
      case 'thinking':
        onEvent({ type: 'thinking' })
        break
      case 'delta':
        if (typeof payload.text === 'string') onEvent({ type: 'delta', text: payload.text })
        break
      case 'result':
        onEvent({ type: 'result', data: payload })
        break
      case 'done':
        ended = true
        onEvent({ type: 'done', stopReason: payload.stopReason, usage: payload.usage })
        break
      case 'error':
        ended = true
        onEvent({ type: 'error', error: String(payload.error ?? 'The answer was interrupted'), code: String(payload.code ?? 'error') })
        break
    }
  }
  try {
    for (;;) {
      const { value, done } = await reader.read()
      if (done) break
      buf += value
      let nl: number
      while ((nl = buf.indexOf('\n')) >= 0) {
        let line = buf.slice(0, nl)
        buf = buf.slice(nl + 1)
        if (line.endsWith('\r')) line = line.slice(0, -1)
        if (line === '') dispatch()
        else if (line.startsWith(':')) continue
        else {
          const i = line.indexOf(':')
          const field = i < 0 ? line : line.slice(0, i)
          let value = i < 0 ? '' : line.slice(i + 1)
          if (value.startsWith(' ')) value = value.slice(1)
          if (field === 'event') event = value
          else if (field === 'data') data.push(value)
        }
      }
    }
    dispatch()
  } finally {
    reader.releaseLock()
  }
  if (!ended) onEvent({ type: 'error', error: 'The connection to NexTerm was interrupted', code: 'network_error' })
}

/** Human-readable text for API / provider errors. */
export function describeAiError(code: string, message: string): { title: string; hint?: string } {
  switch (code) {
    case 'ai_not_configured':
      return { title: 'The assistant is not set up yet', hint: 'Choose a provider in Settings → AI assistant.' }
    case 'ai_disabled':
      return { title: 'The assistant is turned off', hint: 'An administrator can enable it in Settings → AI assistant.' }
    case 'provider_auth':
      return { title: 'The provider rejected the API key', hint: 'Check or replace the key in Settings → AI assistant.' }
    case 'provider_unreachable':
      return { title: 'The AI provider cannot be reached', hint: message }
    case 'provider_rate_limited':
      return { title: 'The provider is rate limiting requests', hint: 'Wait a moment, then retry.' }
    case 'provider_overloaded':
      return { title: 'The provider is overloaded right now', hint: 'Retry in a few seconds.' }
    case 'provider_not_found':
      return { title: 'The model or endpoint was not found', hint: message }
    case 'model_not_allowed':
      return { title: 'This model is not allowed', hint: 'Pick another model.' }
    case 'too_many_requests':
    case 'rate_limited':
      return { title: 'Request limit reached', hint: message }
    case 'locked':
      return { title: 'The vault is locked', hint: 'Unlock the vault to use the assistant.' }
    case 'network_error':
      return { title: 'Connection interrupted', hint: message }
    default:
      return { title: message || 'The request failed' }
  }
}
