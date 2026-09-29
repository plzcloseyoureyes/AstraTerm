/* Types of the webproxy feature (SPEC §9 "webproxy"): per-user reverse proxies to HTTP(S) services and Xpra apps. */

/** What to proxy (POST /api/webproxy). One of connectionId / sessionId / tunnelId selects the route; none = direct. */
export interface ProxySpec {
  connectionId?: string
  sessionId?: string
  tunnelId?: string
  url?: string
  scheme?: 'http' | 'https'
  host?: string
  port?: number
  path?: string
  insecureTls?: boolean
  title?: string
}

export interface ProxyTarget {
  scheme: 'http' | 'https'
  host: string
  port: number
  path: string
}

export interface ProxyVia {
  kind: 'direct' | 'ssh' | 'session' | 'tunnel' | 'web'
  id?: string
  label: string
}

export interface ProxyInfo {
  id: string
  kind: 'web' | 'xpra'
  title: string
  target: ProxyTarget
  via: ProxyVia
  insecureTls: boolean
  connectionId?: string
  sessionId?: string
  tunnelId?: string
  spec: ProxySpec
  createdAt: string
  lastUsedAt: string
  active: number
  extra?: { command?: string; mode?: string; xpraVersion?: string }
  /** Entry fields (create / url responses). */
  url?: string
  mode?: 'host' | 'path'
  base?: string
}

export interface ProxyEvent {
  type: 'webproxy'
  change: 'created' | 'closed' | 'updated'
  proxy: ProxyInfo
  reason?: string
}

export interface XpraCheck {
  installed: boolean
  path?: string
  version?: string
  html5: boolean
  message?: string
}

export type XpraMode = 'seamless' | 'desktop'

export interface XpraStartRequest {
  connectionId?: string
  sessionId?: string
  command: string
  mode: XpraMode
  title?: string
}

/** Params of a 'web' tab (JSON-serialisable; persisted with the layout). */
export interface WebTabParams {
  proxyId: string
  /** Initial entry URL (one-time token; replaced by a fresh one whenever the tab (re)loads). */
  url?: string
  kind?: 'web' | 'xpra'
  title?: string
  /** How to recreate the proxy after it expired / NexTerm restarted. */
  spec?: ProxySpec
  /** Xpra app to start again after it ended. */
  xpra?: XpraStartRequest
  /** Last path shown (restored on reload). */
  path?: string
  /** Force path mode (browsers that cannot resolve *.localhost). */
  pathMode?: boolean
  zoom?: number
  /** Shown in the tab's address bar before the page reports its own location. */
  target?: ProxyTarget
  via?: ProxyVia
  /** Present so the shell / menus can associate the tab with its SSH session. */
  sessionId?: string
  connectionId?: string
}

/** Arguments of the `webproxy.open` command (cross-module contract, SPEC §10). */
export interface OpenArgs extends ProxySpec {
  /** Always show the dialog (prefilled) instead of opening directly. */
  dialog?: boolean
  position?: 'tab' | 'right' | 'below' | 'window'
}

/** Messages exchanged with the in-page bridge (internal/webproxy/pages.go). */
export interface BridgeMessage {
  source: 'nexterm-webproxy'
  proxyId?: string
  type: 'hello' | 'location' | 'unload' | 'error'
  path?: string
  title?: string
  error?: { code: string; message: string; status: number }
}
