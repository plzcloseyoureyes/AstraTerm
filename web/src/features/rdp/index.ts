/*
 * RDP feature (PROTO-16, GFX-1..9/11/18, CORE-16, CC-15): the "rdp" tab kind (lazy viewer: IronRDP WASM through the
 * Go RDCleanPath relay, or guacd through the Guacamole tunnel), remote-desktop commands, tab / session-tree menus,
 * the status bar item and Settings → Remote desktop (viewer preferences; guacd and its Docker sidecar for admins).
 */
import { lazy } from 'react'
import { Camera, ExternalLink, FileDown, Film, Keyboard, Maximize, Monitor, RefreshCw, Unplug } from 'lucide-react'
import { toast } from 'sonner'
import { createSession, getSession } from '@/api/sessions'
import { queryClient } from '@/api/queryClient'
import { queryKeys } from '@/api/queryKeys'
import type { RuntimeSession } from '@/api/types'
import { scheduleSessionClose } from '@/app/closeUndo'
import { claimKeys } from '@/app/keybindings'
import { protocolIcon } from '@/app/protocols'
import {
  registerCommand,
  registerContextMenu,
  registerOverlay,
  registerSettingsSection,
  registerStatusItem,
  registerTabKind,
  type CommandContext,
  type TabInfo,
  type TabPlacement,
} from '@/app/registry'
import { confirmEx } from '@/components/ui/dialog-host'
import { tintedIcon } from '@/features/terminal/icons'
import { attachSession, duplicateSession, findSessionTabs, estimateTerminalSize } from '@/features/terminal/open'
import { errorMessage } from '@/lib/utils'
import { useAuthStore } from '@/stores/auth'
import { generalSettings } from '@/stores/settings'
import { activeTab, openTab } from '@/stores/workspace'
import { connectionRdpFileUrl, downloadUrl, launchNativeConnection, launchNativeSession, sessionRdpFileUrl } from './api'
import { findCombo } from './keys'
import { getController, getViewer, openRecordings } from './store'
import { RdpStatusItem } from './StatusItem'
import type { RdpController, RdpScaling, RdpTabParams } from './types'

const RdpView = lazy(() => import('./RdpView'))
const RecordingsDialog = lazy(() => import('./RecordingsDialog'))

const CATEGORY = 'Remote desktop'

// ---- tab kind -------------------------------------------------------------------------------------------------------

function isLive(tab: TabInfo<RdpTabParams>): boolean {
  const v = getViewer(tab.id)
  if (v) return v.status === 'connected' || v.status === 'connecting' || v.status === 'authenticating' || v.status === 'loading'
  const s = queryClient.getQueryData<RuntimeSession[]>(queryKeys.sessions)?.find((x) => x.id === tab.params?.sessionId)
  return !!s && (s.state === 'connected' || s.state === 'connecting' || s.state === 'authenticating')
}

async function confirmClose(tab: TabInfo<RdpTabParams>): Promise<boolean> {
  // Closing a read-only view (administrator shadowing) leaves the session running: nothing to confirm.
  if (tab.params?.shadow || getViewer(tab.id)?.readOnly) return true
  if (!generalSettings.get().confirmCloseRunning || !isLive(tab)) return true
  const r = await confirmEx({
    title: `Close ${tab.title}?`,
    description: 'The remote desktop is connected. Closing the tab ends the session.',
    confirmLabel: 'Close session',
    destructive: true,
    checkboxLabel: "Don't ask again",
  })
  if (r.ok && r.checked) generalSettings.set({ confirmCloseRunning: false })
  return r.ok
}

function onClosed(tab: TabInfo<RdpTabParams>): void {
  const sessionId = tab.params?.sessionId
  if (!sessionId || findSessionTabs(sessionId).some((id) => id !== tab.id)) return
  if (tab.params?.shadow || getViewer(tab.id)?.readOnly) return // another user's session: only the view closes
  // The session ends a few seconds later, with Undo meanwhile (app/closeUndo).
  scheduleSessionClose(sessionId, tab.title)
}

/** Reopen a closed RDP tab (CC-7): re-attach when its session still exists, else start a new one. */
async function reopen(params: RdpTabParams, title: string, placement?: TabPlacement): Promise<void> {
  if (params.sessionId) {
    try {
      const s = await getSession(params.sessionId)
      if (s.state !== 'closed') {
        openTab({ kind: 'rdp', params: { ...params }, title, placement })
        return
      }
    } catch {
      /* gone: start a new session */
    }
  }
  const size = estimateTerminalSize()
  try {
    const s = params.connectionId
      ? await createSession({ connectionId: params.connectionId, cols: size.cols, rows: size.rows })
      : params.quick
        ? await createSession({ quick: { ...params.quick }, cols: size.cols, rows: size.rows, title })
        : null
    if (!s) {
      toast.info('This remote desktop cannot be reopened', { description: 'It was not created from a saved or quick-connect session.' })
      return
    }
    openTab({ kind: 'rdp', params: { ...params, sessionId: s.id, vmId: params.vmId }, title, placement })
  } catch (err) {
    toast.error('Could not reopen the remote desktop', { description: errorMessage(err) })
  }
}

registerTabKind<RdpTabParams>({
  kind: 'rdp',
  title: (p) => p?.title || 'Remote desktop',
  icon: Monitor,
  iconFor: (p) => (p?.color ? tintedIcon(protocolIcon('rdp'), p.color) : undefined),
  component: RdpView,
  canClose: confirmClose,
  onClose: onClosed,
  duplicate: (tab, position) => void duplicateSession(tab.id, { position }),
  reopen: (closed) => reopen(closed.params, closed.title, closed.placement),
})

// ---- keyboard: every key goes to the connected remote desktop while it has focus --------------------------------------

claimKeys((e) => {
  const vp = e.target instanceof Element ? e.target.closest<HTMLElement>('[data-rdp-viewport]') : null
  const v = vp ? getViewer(vp.dataset.rdpViewport ?? '') : undefined
  return v?.status === 'connected' && !v.readOnly // read-only views send no keys: app shortcuts keep working
})

// ---- commands -------------------------------------------------------------------------------------------------------

function activeController(ctx?: CommandContext): RdpController | undefined {
  const tab = ctx?.activeTab ?? activeTab()
  return tab?.kind === 'rdp' ? getController(tab.id) : undefined
}

const rdpActive = () => activeTab()?.kind === 'rdp'
const rdpConnected = () => {
  const tab = activeTab()
  return tab?.kind === 'rdp' && getViewer(tab.id)?.status === 'connected'
}

registerCommand<{ sessionId?: string }>({
  id: 'rdp.attach',
  title: 'Open remote desktop session',
  category: CATEGORY,
  hidden: true,
  run: async ({ args }) => {
    if (args?.sessionId) await attachSession(args.sessionId)
  },
})

registerCommand({
  id: 'rdp.ctrlAltDel',
  title: 'Send Ctrl+Alt+Del',
  category: CATEGORY,
  icon: Keyboard,
  keywords: ['rdp', 'secure attention', 'task manager', 'logon'],
  when: rdpConnected,
  run: (ctx) => activeController(ctx)?.ctrlAltDel(),
})

registerCommand<{ combo?: string }>({
  id: 'rdp.sendKeys',
  title: 'Send key combination',
  category: CATEGORY,
  hidden: true,
  when: rdpConnected,
  run: (ctx) => {
    const combo = ctx.args?.combo ? findCombo(ctx.args.combo) : undefined
    if (combo) activeController(ctx)?.sendCombo(combo)
  },
})

registerCommand({
  id: 'rdp.fullscreen',
  title: 'Toggle remote desktop fullscreen',
  category: CATEGORY,
  icon: Maximize,
  description: 'Ctrl+Alt+Enter while the desktop has focus. System keys are captured in fullscreen (Chromium).',
  when: rdpActive,
  run: (ctx) => void activeController(ctx)?.toggleFullscreen(),
})

registerCommand({
  id: 'rdp.screenshot',
  title: 'Save remote desktop screenshot',
  category: CATEGORY,
  icon: Camera,
  when: rdpConnected,
  run: (ctx) => void activeController(ctx)?.saveScreenshot(),
})

registerCommand({
  id: 'rdp.copyScreenshot',
  title: 'Copy remote desktop screenshot',
  category: CATEGORY,
  icon: Camera,
  when: rdpConnected,
  run: (ctx) => void activeController(ctx)?.copyScreenshot(),
})

registerCommand<{ mode?: RdpScaling }>({
  id: 'rdp.scaling',
  title: 'Remote desktop scaling',
  category: CATEGORY,
  hidden: true,
  when: rdpActive,
  run: (ctx) => {
    const mode = ctx.args?.mode
    if (mode === 'resize' || mode === 'fit' || mode === 'none') activeController(ctx)?.setScaling(mode)
  },
})

registerCommand({
  id: 'rdp.reconnect',
  title: 'Reconnect remote desktop',
  category: CATEGORY,
  icon: RefreshCw,
  when: rdpActive,
  run: (ctx) => activeController(ctx)?.reconnect(),
})

registerCommand({
  id: 'rdp.disconnect',
  title: 'Disconnect remote desktop',
  category: CATEGORY,
  icon: Unplug,
  when: rdpConnected,
  run: (ctx) => activeController(ctx)?.disconnect(),
})

registerCommand<{ connectionId?: string; sessionId?: string }>({
  id: 'rdp.downloadFile',
  title: 'Download .rdp file',
  category: CATEGORY,
  icon: FileDown,
  keywords: ['export', 'mstsc', 'rdp file'],
  when: () => rdpActive(),
  run: (ctx) => {
    const tab = ctx.activeTab
    const connectionId = ctx.args?.connectionId
    const sessionId = ctx.args?.sessionId ?? (tab?.kind === 'rdp' ? (tab.params as RdpTabParams)?.sessionId : undefined)
    if (connectionId) downloadUrl(connectionRdpFileUrl(connectionId))
    else if (sessionId) downloadUrl(sessionRdpFileUrl(sessionId))
  },
})

async function launchNative(args: { connectionId?: string; sessionId?: string }): Promise<void> {
  try {
    const r = args.connectionId ? await launchNativeConnection(args.connectionId) : await launchNativeSession(args.sessionId!)
    toast.success(`Opened in ${r.client}`, {
      description: [r.forwarded ? `Through the gateway at ${r.forwarded}.` : '', r.passwordInjected ? '' : 'Sign in when the client asks.']
        .filter(Boolean)
        .join(' '),
    })
  } catch (err) {
    toast.error('Could not open the native client', { description: errorMessage(err) })
  }
}

registerCommand<{ connectionId?: string; sessionId?: string }>({
  id: 'rdp.launchNative',
  title: 'Open in the native RDP client',
  category: CATEGORY,
  icon: ExternalLink,
  keywords: ['mstsc', 'windows app', 'xfreerdp', 'remmina', 'native'],
  when: () => useAuthStore.getState().state?.mode === 'desktop' && rdpActive(),
  run: (ctx) => {
    const tab = ctx.activeTab
    const sessionId = ctx.args?.sessionId ?? (tab?.kind === 'rdp' ? (tab.params as RdpTabParams)?.sessionId : undefined)
    if (ctx.args?.connectionId) void launchNative({ connectionId: ctx.args.connectionId })
    else if (sessionId) void launchNative({ sessionId })
  },
})

registerCommand({
  id: 'rdp.recordings',
  title: 'Remote desktop recordings',
  category: CATEGORY,
  icon: Film,
  keywords: ['rdp', 'guacd', 'recording', 'playback', 'replay'],
  description: 'Recorded guacd sessions (connections with "Record sessions").',
  run: () => openRecordings(),
})

registerOverlay({ id: 'rdp-recordings', component: RecordingsDialog })

// ---- menus ----------------------------------------------------------------------------------------------------------

registerContextMenu({
  target: 'tab',
  order: 55,
  items: (ctx) => {
    if (ctx.kind !== 'rdp') return []
    const p = ctx.params as RdpTabParams
    const c = getController(ctx.tabId)
    const status = getViewer(ctx.tabId)?.status
    const desktop = useAuthStore.getState().state?.mode === 'desktop'
    return [
      { label: 'Reconnect', icon: RefreshCw, run: () => c?.reconnect(), disabled: !c || status === 'gone' },
      { label: 'Send Ctrl+Alt+Del', icon: Keyboard, run: () => c?.ctrlAltDel(), disabled: status !== 'connected' },
      { label: 'Save screenshot', icon: Camera, run: () => void c?.saveScreenshot(), disabled: status !== 'connected' },
      { type: 'separator' },
      { label: 'Download .rdp file', icon: FileDown, run: () => downloadUrl(sessionRdpFileUrl(p.sessionId)) },
      ...(desktop ? [{ label: 'Open in the native client', icon: ExternalLink, run: () => void launchNative({ sessionId: p.sessionId }) }] : []),
    ]
  },
})

registerContextMenu({
  target: 'session-node',
  order: 60,
  items: (ctx) => {
    const conn = ctx.connection
    if (!conn || conn.protocol !== 'rdp' || (ctx.selection && ctx.selection.length > 1)) return []
    const desktop = useAuthStore.getState().state?.mode === 'desktop'
    return [
      { label: 'Download .rdp file', icon: FileDown, run: () => downloadUrl(connectionRdpFileUrl(conn.id)) },
      ...(desktop ? [{ label: 'Open in the native client', icon: ExternalLink, run: () => void launchNative({ connectionId: conn.id }) }] : []),
    ]
  },
})

// ---- status bar & settings --------------------------------------------------------------------------------------------

registerStatusItem({ id: 'rdp', align: 'right', order: 101, component: RdpStatusItem })

registerSettingsSection({
  id: 'rdp',
  title: 'Remote desktop',
  icon: Monitor,
  order: 42,
  group: 'connections',
  keywords: ['rdp', 'ironrdp', 'guacd', 'guacamole', 'docker', 'sidecar', 'clipboard', 'scaling', 'hidpi', 'keyboard lock', 'mstsc'],
  component: lazy(() => import('./SettingsSection')),
})
