import { isApiError } from '@/api/client'
import type { ProxySpec, ProxyTarget, ProxyVia } from './types'

export function errorCode(err: unknown): string {
  return isApiError(err) ? err.code : ''
}

function defaultPort(scheme: string): number {
  return scheme === 'https' ? 443 : 80
}

/** host[:port] as shown in an address (default port omitted, IPv6 bracketed). */
export function authority(t: Pick<ProxyTarget, 'scheme' | 'host' | 'port'>): string {
  const host = t.host.includes(':') ? `[${t.host}]` : t.host
  return t.port && t.port !== defaultPort(t.scheme) ? `${host}:${t.port}` : host
}

export function originOf(t: ProxyTarget): string {
  return `${t.scheme}://${authority(t)}`
}

/** The upstream URL a proxied page path corresponds to (what the address bar shows). */
export function displayUrl(t: ProxyTarget | undefined, path?: string): string {
  if (!t) return ''
  return originOf(t) + (path ?? t.path ?? '/')
}

export interface ParsedAddress {
  scheme: 'http' | 'https'
  host: string
  port: number
  path: string
}

/**
 * Parse what the user typed in an address field: "localhost:3000/x", "https://10.0.0.5", "grafana.lan". A bare port
 * (":8080" or "8080") means localhost. Returns null when it is not a usable http(s) address.
 */
export function parseAddress(input: string): ParsedAddress | null {
  let s = input.trim()
  if (!s) return null
  if (/^:?\d{1,5}(\/.*)?$/.test(s)) s = `localhost:${s.replace(/^:/, '')}`
  const hadScheme = /^[a-z][a-z0-9+.-]*:\/\//i.test(s)
  if (!hadScheme) s = `http://${s}`
  let u: URL
  try {
    u = new URL(s)
  } catch {
    return null
  }
  if (u.protocol !== 'http:' && u.protocol !== 'https:') return null
  if (!u.hostname || u.username || u.password) return null
  let scheme = u.protocol.slice(0, -1) as 'http' | 'https'
  let port = u.port ? Number(u.port) : 0
  if (!hadScheme && port === 443) scheme = 'https'
  if (!port) port = defaultPort(scheme)
  const host = u.hostname.replace(/^\[|\]$/g, '')
  return { scheme, host, port, path: `${u.pathname}${u.search}${u.hash}` || '/' }
}

export function addressToString(a: ParsedAddress): string {
  return `${a.scheme}://${authority(a)}${a.path}`
}

/** Normalise the `webproxy.open` args of other modules into a spec the backend accepts. */
export function specFromArgs(a: ProxySpec): ProxySpec {
  const spec: ProxySpec = {}
  if (a.tunnelId) spec.tunnelId = a.tunnelId
  else if (a.sessionId) spec.sessionId = a.sessionId
  else if (a.connectionId) spec.connectionId = a.connectionId
  for (const k of ['url', 'scheme', 'host', 'port', 'path', 'insecureTls', 'title'] as const) {
    if (a[k] !== undefined && a[k] !== null && a[k] !== '') (spec as Record<string, unknown>)[k] = a[k]
  }
  return spec
}

export function viaText(via: ProxyVia | undefined): string {
  if (!via) return ''
  switch (via.kind) {
    case 'direct':
      return 'from the NexTerm host'
    case 'session':
      return `through session ${via.label}`
    case 'tunnel':
      return `through tunnel ${via.label}`
    default:
      return `through ${via.label}`
  }
}

/** Default title of a web tab (before the page reports its own). */
export function webTabTitle(p: { title?: string; target?: ProxyTarget }): string {
  if (p.title) return p.title
  if (p.target) return originOf(p.target)
  return 'Web page'
}
