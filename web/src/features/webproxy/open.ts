/* Opening web tabs: proxies are created through the API, then shown in a 'web' tab. */
import { toast } from 'sonner'
import type { TabPosition } from '@/app/registry'
import { findTabs, openTab } from '@/stores/workspace'
import { errorMessage } from '@/lib/utils'
import { closeProxy, createProxy, startXpra } from './api'
import { errorCode, specFromArgs } from './model'
import { rememberAddress, webViewSettings } from './settings'
import { openWebDialog } from './store'
import type { OpenArgs, ProxyInfo, ProxySpec, WebTabParams, XpraStartRequest } from './types'

export interface OpenOptions {
  position?: TabPosition
  reference?: string
  activate?: boolean
  title?: string
}

/** Open a tab for an existing proxy (as returned by create / xpra start). */
export function openProxyTab(info: ProxyInfo, spec: ProxySpec, opts: OpenOptions = {}, xpra?: XpraStartRequest): string {
  const params: WebTabParams = {
    proxyId: info.id,
    url: info.url,
    kind: info.kind,
    title: opts.title || info.title,
    spec,
    xpra,
    target: info.target,
    via: info.via,
    pathMode: info.mode === 'path' || undefined,
    zoom: webViewSettings.get().defaultZoom || 1,
  }
  if (info.via.kind === 'web' && info.via.id) params.connectionId = info.via.id // "Edit session…" on the tab menu
  return openTab<WebTabParams>({
    kind: 'web',
    params,
    title: opts.title || info.title,
    position: opts.position,
    reference: opts.reference,
    activate: opts.activate,
  })
}

/** Create a proxy for spec and open it in a tab. Errors are toasted; resolves to the tab id or null. */
export async function openWeb(spec: ProxySpec, opts: OpenOptions = {}): Promise<string | null> {
  try {
    const info = await createProxy(spec)
    if (info.target && info.via.kind !== 'web') rememberAddress(`${info.target.scheme}://${info.target.host}:${info.target.port}${info.target.path}`)
    return openProxyTab(info, spec, opts)
  } catch (err) {
    toast.error('Could not open the web page', { description: errorMessage(err) })
    return null
  }
}

/** Start an X11 application through Xpra and open it in a tab. */
export async function openXpra(req: XpraStartRequest, opts: OpenOptions = {}): Promise<string | null> {
  const info = await startXpra(req) // errors are shown by the caller (dialog)
  return openProxyTab(info, { connectionId: req.connectionId, sessionId: req.sessionId }, { title: req.title || opts.title, ...opts }, req)
}

/** `webproxy.open` (SPEC §10 contract): open directly when the target is complete, else show the prefilled dialog. */
export async function runOpenCommand(args: OpenArgs | undefined): Promise<string | null> {
  const a = args ?? {}
  const complete = !a.dialog && (!!a.url || (!!a.port && (!!a.host || !!a.connectionId || !!a.sessionId || !!a.tunnelId)))
  if (!complete) {
    openWebDialog(a)
    return null
  }
  const spec = specFromArgs(a)
  return openWeb(spec, { position: a.position })
}

/**
 * Close the proxy of a web tab that was just closed, unless another tab still shows it. Xpra applications always stop
 * with their last tab (the close confirmation says so); plain web proxies follow the "close with tab" setting.
 */
export function releaseProxy(proxyId: string, closingTabId: string, kind?: string): void {
  if (kind !== 'xpra' && !webViewSettings.get().closeWithTab) return
  const others = findTabs((t) => t.kind === 'web' && t.id !== closingTabId && (t.params as WebTabParams)?.proxyId === proxyId)
  if (others.length) return
  closeProxy(proxyId).catch((err) => {
    if (errorCode(err) !== 'not_found') console.warn('[webproxy] close failed', err)
  })
}
