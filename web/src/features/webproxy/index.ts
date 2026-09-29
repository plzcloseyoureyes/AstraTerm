/*
 * Web proxy feature (RESEARCH PROTO-28 "Browser" sessions, TUN-8 "open forwarded web services", PROTO-20 Xpra):
 * 'web' tabs showing HTTP(S) services reached from the NexTerm host or through SSH, the saved 'web' session type
 * (editor + opener), X11 applications through Xpra, and the dialogs / menus around them.
 *
 * Commands (category "Web"):
 *   webproxy.open {connectionId?|sessionId?|tunnelId?, host?, port?, scheme?, path?, url?, title?, insecureTls?, dialog?}
 *                  complete targets open directly (tools, tunnels, monitor), otherwise the prefilled dialog
 *   webproxy.browser                 open any address reachable from the NexTerm host
 *   webproxy.xpra {connectionId?|sessionId?, command?, mode?}   run an X11 application via Xpra
 *   webproxy.manage                  the user's open proxies
 *   webproxy.back|forward|reload|home|zoomIn|zoomOut|zoomReset|openExternal|focusAddress   the active web tab
 */
import { lazy } from 'react'
import { AppWindow, ArrowLeft, ArrowRight, ExternalLink, Globe, Home, Network, RotateCw, ZoomIn, ZoomOut } from 'lucide-react'
import type { Connection } from '@/api/types'
import {
  registerCommand,
  registerContextMenu,
  registerMenu,
  registerOverlay,
  registerProtocolOpener,
  registerRibbonButton,
  registerSettingsSection,
  registerTabKind,
  type MenuItem,
} from '@/app/registry'
import { confirm } from '@/components/ui/dialog-host'
import { activeTab } from '@/stores/workspace'
import './editor'
import { installProxyEvents } from './api'
import { specFromArgs, webTabTitle } from './model'
import { openWeb, releaseProxy, runOpenCommand } from './open'
import { WebproxyOverlay } from './Overlay'
import { openWebDialog, openXpraDialog, setManagerOpen, type XpraDialogInit } from './store'
import type { OpenArgs, WebTabParams } from './types'
import { getWebView, type WebViewHandle } from './views'

const WebView = lazy(() => import('./WebView'))
const CATEGORY = 'Web'

registerTabKind<WebTabParams>({
  kind: 'web',
  title: (p) => webTabTitle(p ?? ({} as WebTabParams)),
  icon: Globe,
  iconFor: (p) => (p?.kind === 'xpra' ? AppWindow : Globe),
  component: WebView,
  canClose: async (tab) => {
    if (tab.params?.kind !== 'xpra') return true
    return confirm({
      title: `Stop ${tab.params.xpra?.command ?? 'the application'}?`,
      description: 'Closing the tab stops the X11 application on the remote host. Unsaved work in it is lost.',
      confirmLabel: 'Stop and close',
      destructive: true,
    })
  },
  onClose: (tab) => {
    if (tab.params?.proxyId) releaseProxy(tab.params.proxyId, tab.id, tab.params.kind)
  },
})

registerOverlay({ id: 'webproxy', component: WebproxyOverlay })

registerSettingsSection({
  id: 'webproxy',
  title: 'Web pages & Xpra',
  icon: Globe,
  order: 57,
  keywords: ['browser', 'web proxy', 'xpra', 'x11', 'wildcard domain', 'iframe', 'http'],
  component: lazy(() => import('./SettingsSection')),
})

// --- commands ----------------------------------------------------------------------------------------------------------

registerCommand<OpenArgs>({
  id: 'webproxy.open',
  title: 'Open web page…',
  category: CATEGORY,
  icon: Globe,
  keywords: ['browser', 'http', 'url', 'proxy', 'forwarded web service', 'grafana', 'jupyter'],
  description: 'Open an HTTP(S) service in a tab, from the NexTerm host or through SSH',
  run: ({ args }) => runOpenCommand(args),
})

registerCommand({
  id: 'webproxy.browser',
  title: 'Browser: open address from the NexTerm host…',
  category: CATEGORY,
  icon: Globe,
  keywords: ['browser', 'url', 'web'],
  run: () => openWebDialog({ dialog: true }),
})

registerCommand<XpraDialogInit>({
  id: 'webproxy.xpra',
  title: 'Run X11 app via Xpra…',
  category: CATEGORY,
  icon: AppWindow,
  keywords: ['x11', 'xpra', 'gui', 'x server', 'graphical application', 'xterm'],
  description: 'Start a graphical application on an SSH host and show it in a tab',
  run: ({ args }) => openXpraDialog(args ?? {}),
})

registerCommand({
  id: 'webproxy.manage',
  title: 'Web proxies…',
  category: CATEGORY,
  icon: Network,
  keywords: ['proxies', 'open web pages', 'xpra applications'],
  run: () => setManagerOpen(true),
})

const isWebTab = () => activeTab()?.kind === 'web'
const onActive = (fn: (h: WebViewHandle) => void) => () => {
  const h = getWebView(activeTab()?.id)
  if (h) fn(h)
}

for (const c of [
  { id: 'webproxy.back', title: 'Web page: back', icon: ArrowLeft, keybinding: 'Alt+ArrowLeft', fn: (h: WebViewHandle) => h.back() },
  { id: 'webproxy.forward', title: 'Web page: forward', icon: ArrowRight, keybinding: 'Alt+ArrowRight', fn: (h: WebViewHandle) => h.forward() },
  { id: 'webproxy.reload', title: 'Web page: reload', icon: RotateCw, fn: (h: WebViewHandle) => h.reload() },
  { id: 'webproxy.home', title: 'Web page: home', icon: Home, fn: (h: WebViewHandle) => h.home() },
  { id: 'webproxy.zoomIn', title: 'Web page: zoom in', icon: ZoomIn, fn: (h: WebViewHandle) => h.zoomIn() },
  { id: 'webproxy.zoomOut', title: 'Web page: zoom out', icon: ZoomOut, fn: (h: WebViewHandle) => h.zoomOut() },
  { id: 'webproxy.zoomReset', title: 'Web page: reset zoom', fn: (h: WebViewHandle) => h.zoomReset() },
  { id: 'webproxy.openExternal', title: 'Web page: open in new window', icon: ExternalLink, fn: (h: WebViewHandle) => h.openExternal() },
  { id: 'webproxy.focusAddress', title: 'Web page: edit address', keybinding: '$mod+l', fn: (h: WebViewHandle) => h.focusAddress() },
]) {
  registerCommand({ id: c.id, title: c.title, category: CATEGORY, icon: c.icon, keybinding: c.keybinding, when: isWebTab, run: onActive(c.fn) })
}

// --- saved 'web' sessions --------------------------------------------------------------------------------------------

registerProtocolOpener({
  protocol: 'web',
  open: (conn: Connection, opts) => openWeb({ connectionId: conn.id }, { position: opts.position, reference: opts.reference, activate: opts.activate, title: opts.title || conn.name }),
  openQuick: (spec, opts) => {
    const url = (spec.options?.url as string | undefined) || (spec.host ? `${spec.port === 443 ? 'https' : 'http'}://${spec.host}${spec.port ? `:${spec.port}` : ''}/` : '')
    if (!url) {
      openWebDialog({ dialog: true })
      return Promise.resolve(null)
    }
    return openWeb(specFromArgs({ url, connectionId: (spec.options?.sshTunnelVia as string | undefined) || undefined }), opts)
  },
})

// --- menus -------------------------------------------------------------------------------------------------------------

const webMenu = (): MenuItem[] => [
  { label: 'Open web page…', icon: Globe, command: 'webproxy.open' },
  { label: 'Run X11 app via Xpra…', icon: AppWindow, command: 'webproxy.xpra' },
  { type: 'separator' },
  { label: 'Web proxies…', icon: Network, command: 'webproxy.manage' },
]

registerRibbonButton({ id: 'browser', label: 'Browser', icon: Globe, order: 85, command: 'webproxy.open', menu: webMenu, tooltip: 'Web pages and X11 apps through NexTerm' })

registerMenu({ menu: 'tools', order: 140, items: webMenu })

const SSH_FAMILY = new Set(['ssh', 'sftp', 'mosh'])

registerContextMenu({
  target: 'terminal',
  order: 62,
  items: (ctx) => {
    const s = ctx.session
    if (!s || s.protocol !== 'ssh' || s.state !== 'connected') return []
    return [
      { label: 'Open web service…', icon: Globe, command: 'webproxy.open', args: { sessionId: s.id, dialog: true } satisfies OpenArgs },
      { label: 'Run X11 app via Xpra…', icon: AppWindow, command: 'webproxy.xpra', args: { sessionId: s.id } satisfies XpraDialogInit },
    ]
  },
})

registerContextMenu({
  target: 'session-node',
  order: 62,
  items: (ctx) => {
    const c = ctx.connection
    if (!c || (ctx.selection && ctx.selection.length > 1) || !SSH_FAMILY.has(c.protocol)) return []
    return [
      { label: 'Open web service…', icon: Globe, command: 'webproxy.open', args: { connectionId: c.id, dialog: true } satisfies OpenArgs },
      { label: 'Run X11 app via Xpra…', icon: AppWindow, command: 'webproxy.xpra', args: { connectionId: c.id } satisfies XpraDialogInit },
    ]
  },
})

installProxyEvents()
