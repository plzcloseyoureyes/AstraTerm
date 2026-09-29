/*
 * REST client: JSON in/out, same-origin cookies, CSRF header on mutating requests, typed ApiError.
 *
 *   401 → the configured onUnauthorized hook (auth store flips to the login screen), except for the auth endpoints that
 *         legitimately answer 401 for bad credentials (login, verify-password, ...).
 *   423 → vault locked: the configured onVaultLocked hook opens the unlock dialog; if the user unlocks, the request is
 *         retried once transparently, otherwise the ApiError(423) propagates.
 *
 * Hooks are injected (configureApiClient) to keep this module free of store imports (no import cycles).
 */
import type { ErrorBody } from './types'

export class ApiError extends Error {
  readonly status: number
  readonly code: string
  readonly body?: unknown

  constructor(status: number, code: string, message: string, body?: unknown) {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.code = code
    this.body = body
  }

  get isNotFound() {
    return this.status === 404
  }
  get isLocked() {
    return this.status === 423
  }
  get isUnauthorized() {
    return this.status === 401
  }
}

export function isApiError(err: unknown): err is ApiError {
  return err instanceof ApiError
}

type Method = 'GET' | 'POST' | 'PUT' | 'PATCH' | 'DELETE'

export interface RequestOptions {
  /** Query parameters; undefined/null values are skipped, arrays repeat the key. */
  query?: Record<string, string | number | boolean | null | undefined | (string | number)[]>
  signal?: AbortSignal
  headers?: Record<string, string>
  /** Response handling: 'json' (default), 'text', 'blob', 'response' (raw Response, caller consumes). */
  as?: 'json' | 'text' | 'blob' | 'response'
  /** Raw body (Blob, ArrayBuffer, FormData, string) sent as-is instead of JSON. */
  rawBody?: BodyInit
  /** Skip the 401 → logout behaviour (for endpoints where 401 means "wrong password"). */
  noAuthRedirect?: boolean
  /** Skip the 423 → unlock-and-retry behaviour. */
  noVaultPrompt?: boolean
}

interface ApiHooks {
  onUnauthorized?: () => void
  /** Resolve true when the vault got unlocked (request is retried), false when the user cancelled. */
  onVaultLocked?: () => Promise<boolean>
}

let hooks: ApiHooks = {}

export function configureApiClient(h: ApiHooks): void {
  hooks = { ...hooks, ...h }
}

/**
 * Endpoints whose 401 means "bad credentials" rather than "session expired". (Re-auth endpoints such as
 * verify-password answer 403 `invalid_password`, so a 401 there really is an expired session.)
 */
const AUTH_401_EXEMPT = ['/api/auth/state', '/api/auth/login', '/api/auth/setup', '/api/auth/launch', '/api/auth/logout']

function buildUrl(path: string, query?: RequestOptions['query']): string {
  if (!query) return path
  const params = new URLSearchParams()
  for (const [k, v] of Object.entries(query)) {
    if (v === undefined || v === null) continue
    if (Array.isArray(v)) for (const item of v) params.append(k, String(item))
    else params.set(k, String(v))
  }
  const qs = params.toString()
  if (!qs) return path
  return path + (path.includes('?') ? '&' : '?') + qs
}

async function parseError(res: Response): Promise<ApiError> {
  let body: unknown
  let message = res.statusText || `HTTP ${res.status}`
  let code = statusCode(res.status)
  const ct = res.headers.get('content-type') || ''
  try {
    if (ct.includes('application/json')) {
      body = await res.json()
      const b = body as Partial<ErrorBody>
      if (b && typeof b.error === 'string' && b.error) message = b.error
      if (b && typeof b.code === 'string' && b.code) code = b.code
    } else {
      const text = (await res.text()).trim()
      if (text && text.length < 500) message = text
      body = text
    }
  } catch {
    /* unreadable body — keep status text */
  }
  return new ApiError(res.status, code, message, body)
}

function statusCode(status: number): string {
  switch (status) {
    case 400:
      return 'bad_request'
    case 401:
      return 'unauthorized'
    case 403:
      return 'forbidden'
    case 404:
      return 'not_found'
    case 409:
      return 'conflict'
    case 423:
      return 'locked'
    case 429:
      return 'rate_limited'
    default:
      return status >= 500 ? 'server_error' : 'error'
  }
}

/** Low-level request. Prefer the `api.get/post/...` helpers. */
export async function request<T = unknown>(
  method: Method,
  path: string,
  body?: unknown,
  opts: RequestOptions = {},
  retried = false,
): Promise<T> {
  const headers: Record<string, string> = { Accept: 'application/json', ...opts.headers }
  const mutating = method !== 'GET'
  if (mutating) headers['X-AstraTerm'] = '1'
  let payload: BodyInit | undefined
  if (opts.rawBody !== undefined) {
    payload = opts.rawBody
  } else if (body !== undefined) {
    headers['Content-Type'] = 'application/json'
    payload = JSON.stringify(body)
  }

  let res: Response
  try {
    res = await fetch(buildUrl(path, opts.query), {
      method,
      headers,
      body: payload,
      credentials: 'same-origin',
      signal: opts.signal,
      cache: 'no-store',
    })
  } catch (err) {
    if (err instanceof DOMException && err.name === 'AbortError') throw err
    throw new ApiError(0, 'network_error', 'Cannot reach the AstraTerm server. Check that it is running.')
  }

  if (!res.ok) {
    const err = await parseError(res)
    if (res.status === 401 && !opts.noAuthRedirect && !AUTH_401_EXEMPT.some((p) => path.startsWith(p))) {
      hooks.onUnauthorized?.()
    }
    if (res.status === 423 && !opts.noVaultPrompt && !retried && !path.startsWith('/api/vault/') && hooks.onVaultLocked) {
      const unlocked = await hooks.onVaultLocked()
      // One-shot streams cannot be replayed; everything else (JSON, Blob, FormData, string) can.
      const replayable = !(typeof ReadableStream !== 'undefined' && opts.rawBody instanceof ReadableStream)
      if (unlocked && replayable) return request<T>(method, path, body, opts, true)
    }
    throw err
  }

  switch (opts.as) {
    case 'response':
      return res as unknown as T
    case 'text':
      return (await res.text()) as unknown as T
    case 'blob':
      return (await res.blob()) as unknown as T
    default: {
      if (res.status === 204) return undefined as T
      const text = await res.text()
      if (!text) return undefined as T
      try {
        return JSON.parse(text) as T
      } catch {
        return text as unknown as T
      }
    }
  }
}

export const api = {
  get: <T = unknown>(path: string, opts?: RequestOptions) => request<T>('GET', path, undefined, opts),
  post: <T = unknown>(path: string, body?: unknown, opts?: RequestOptions) => request<T>('POST', path, body, opts),
  put: <T = unknown>(path: string, body?: unknown, opts?: RequestOptions) => request<T>('PUT', path, body, opts),
  patch: <T = unknown>(path: string, body?: unknown, opts?: RequestOptions) => request<T>('PATCH', path, body, opts),
  del: <T = unknown>(path: string, body?: unknown, opts?: RequestOptions) => request<T>('DELETE', path, body, opts),
}

/** Encode one path segment (ids, tokens). */
export function seg(v: string | number): string {
  return encodeURIComponent(String(v))
}

/**
 * Absolute ws(s):// URL for a backend WebSocket path, honouring the page protocol (wss on https) and host.
 * `wsUrl('/ws/terminal/abc', {offset: 10})` → "wss://host/ws/terminal/abc?offset=10".
 */
export function wsUrl(path: string, query?: RequestOptions['query']): string {
  const proto = location.protocol === 'https:' ? 'wss:' : 'ws:'
  return `${proto}//${location.host}${buildUrl(path.startsWith('/') ? path : `/${path}`, query)}`
}

/** URL for a GET endpoint (downloads, <img src>, iframes) — same-origin, cookie-authenticated. */
export function apiUrl(path: string, query?: RequestOptions['query']): string {
  return buildUrl(path, query)
}
