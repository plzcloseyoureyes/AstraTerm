/*
 * VNC feature (PROTO-17, GFX-1..5, GFX-17, GFX-18): the `vnc` tab kind (lazy; noVNC loads on demand), commands,
 * View/Tools menu entries, tab context menu, Settings → VNC, the listeners / repeater dialogs (overlay), keyboard
 * capture for focused desktops and the {type:'vnc.incoming'} handler for reverse connections.
 *
 * Commands (category "VNC"): vnc.attach {sessionId} · vnc.reconnect · vnc.disconnect · vnc.detach · vnc.ctrlAltDel
 * (Ctrl+Alt+End) · vnc.sendKeys {combo} · vnc.clipboard · vnc.pasteClipboard · vnc.typeClipboard · vnc.screenshot ·
 * vnc.copyScreenshot · vnc.fullscreen (Ctrl+Alt+Enter) · vnc.viewOnly · vnc.scaleFit / scaleRemote / scaleNone ·
 * vnc.zoomIn / zoomOut / zoomReset · vnc.listen · vnc.connectRepeater · vnc.settings.
 */
import { lazy } from 'react'
import {
  Camera,
  ClipboardList,
  Ear,
  Eye,
  Keyboard,
  LogOut,
  Maximize,
  MonitorSmartphone,
  RefreshCw,
  Scan,
  Scaling,
  Expand,
  Unplug,
  Waypoints,
  ZoomIn,
  ZoomOut,
} from 'lucide-react'
import { toast } from 'sonner'
import { getSession } from '@/api/sessions'
import { queryClient } from '@/api/queryClient'
import { queryKeys } from '@/api/queryKeys'
import type { RuntimeSession } from '@/api/types'
import { scheduleSessionClose } from '@/app/closeUndo'
import { runCommand } from '@/app/commands'
import { claimKeys } from '@/app/keybindings'
import { protocolIcon } from '@/app/protocols'
import { registerCommand, registerContextMenu, registerMenu, registerOverlay, registerSettingsSection, registerTabKind, type TabInfo, type TabPlacement } from '@/app/registry'
import { confirmEx } from '@/components/ui/dialog-host'
import { tintedIcon } from '@/features/terminal/icons'
import { attachSession, duplicateSession, findSessionTabs, openConnection, openQuick } from '@/features/terminal/open'
import { events } from '@/lib/events'
import { errorMessage } from '@/lib/utils'
import { useAuthStore } from '@/stores/auth'
import { generalSettings } from '@/stores/settings'
import { closeTab, openTab } from '@/stores/workspace'
import { copyScreenshot, pasteLocalClipboard, saveScreenshot, toggleViewerFullscreen } from './actions'
import { vncKeys } from './api'
import { findCombo } from './keys'
import { VncOverlays } from './Overlays'
import { vncSettings } from './settings'
import { activeController, dropTabUI, getController, openListeners, openRepeater, setTabUI, useVncStore } from './store'
import type { VncIncomingEvent, VncTabParams } from './types'

const VncView = lazy(() => import('./VncView'))
const CATEGORY = 'VNC'

// ---- tab kind -------------------------------------------------------------------------------------------------------

/** Tabs closed through "Detach" keep their session (re-attach from Home). */
const detaching = new Set<string>()

async function confirmClose(tab: TabInfo<VncTabParams>): Promise<boolean> {
  if (detaching.has(tab.id) || !generalSettings.get().confirmCloseRunning) return true
  const c = getController(tab.id)
  if (!c || c.state.phase !== 'connected' || c.readOnly) return true
  const r = await confirmEx({
    title: `Close ${tab.title}?`,
    description: 'The remote desktop keeps running on the server. Closing the tab ends this NexTerm session (use Detach to keep it).',
    confirmLabel: 'Close session',
    destructive: true,
    checkboxLabel: "Don't ask again",
  })
  if (r.ok && r.checked) generalSettings.set({ confirmCloseRunning: false })
  return r.ok
}

/** Does the signed-in user own the session? (Admins viewing other users' sessions must not close them.) */
async function ownsSession(sessionId: string): Promise<boolean> {
  const own = queryClient.getQueryData<RuntimeSession[]>(queryKeys.sessions)
  if (own?.some((s) => s.id === sessionId)) return true
  try {
    const s = await getSession(sessionId)
    return s.ownerId === useAuthStore.getState().user?.id
  } catch {
    return false // already gone
  }
}

function onClosed(tab: TabInfo<VncTabParams>): void {
  dropTabUI(tab.id)
  const sessionId = tab.params?.sessionId
  if (!sessionId || detaching.delete(tab.id)) return
  if (findSessionTabs(sessionId).some((id) => id !== tab.id)) return
  // The session ends a few seconds later, with Undo meanwhile (app/closeUndo).
  void ownsSession(sessionId).then((own) => {
    if (own) scheduleSessionClose(sessionId, tab.title)
  })
}

function detach(tabId: string): void {
  detaching.add(tabId)
  void closeTab(tabId, { force: true }).then((closed) => {
    if (!closed) detaching.delete(tabId)
  })
}

async function reopen(p: VncTabParams | undefined, title: string, placement?: TabPlacement): Promise<void> {
  if (!p) return
  if (p.sessionId) {
    try {
      const s = await getSession(p.sessionId)
      if (s && s.state !== 'closed') {
        openTab({ kind: 'vnc', params: { ...p }, title, placement })
        return
      }
    } catch {
      /* gone */
    }
  }
  if (p.reverse) {
    toast.info('Incoming connections cannot be reopened', { description: 'Ask the server to connect again.' })
    return
  }
  if (p.connectionId) await openConnection(p.connectionId, { title, placement })
  else if (p.quick) await openQuick({ ...p.quick, protocol: 'vnc' }, { title, placement })
}

registerTabKind<VncTabParams>({
  kind: 'vnc',
  title: (p) => p?.title || 'VNC',
  icon: MonitorSmartphone,
  iconFor: (p) => (p?.color ? tintedIcon(protocolIcon('vnc'), p.color) : undefined),
  component: VncView,
  canClose: confirmClose,
  onClose: onClosed,
  duplicate: (tab, position) => {
    if (tab.params?.reverse) {
      toast.info('Incoming connections serve a single viewer')
      return
    }
    void duplicateSession(tab.id, { position })
  },
  reopen: (closed) => reopen(closed.params, closed.title, closed.placement),
})

// ---- commands -------------------------------------------------------------------------------------------------------

const hasViewer = () => !!activeController()
const hasInput = () => {
  const c = activeController()
  return !!c && !c.readOnly && c.state.phase === 'connected' && !c.state.viewOnly
}
/** Input allowed and the clipboard policy permits local → remote (also for typing clipboard text). */
const hasClipboardToRemote = () => hasInput() && !!activeController()?.state.clipboard.toRemote
const withViewer =
  (fn: (c: NonNullable<ReturnType<typeof activeController>>) => unknown) =>
  () => {
    const c = activeController()
    if (c) return fn(c) as void
  }

registerCommand<{ sessionId: string }>({
  id: 'vnc.attach',
  title: 'Attach to VNC Session',
  category: CATEGORY,
  hidden: true,
  run: async ({ args }) => {
    const id = args && typeof args === 'object' && typeof args.sessionId === 'string' ? args.sessionId : undefined
    if (id) await attachSession(id)
  },
})

registerCommand({ id: 'vnc.reconnect', title: 'Reconnect VNC', category: CATEGORY, icon: RefreshCw, when: hasViewer, run: withViewer((c) => c.reconnect()) })
registerCommand({ id: 'vnc.disconnect', title: 'Disconnect VNC', category: CATEGORY, icon: Unplug, when: hasViewer, run: withViewer((c) => c.disconnect()) })
registerCommand({
  id: 'vnc.detach',
  title: 'Detach VNC Tab (keep session)',
  category: CATEGORY,
  icon: LogOut,
  when: hasViewer,
  run: withViewer((c) => detach(c.tabId)),
})
registerCommand({
  id: 'vnc.ctrlAltDel',
  title: 'Send Ctrl+Alt+Del',
  category: CATEGORY,
  keybinding: 'Control+Alt+End',
  keywords: ['cad', 'secure attention', 'login'],
  when: hasInput,
  run: withViewer((c) => c.sendCtrlAltDel()),
})
registerCommand<{ combo: string }>({
  id: 'vnc.sendKeys',
  title: 'Send Key Combination',
  category: CATEGORY,
  hidden: true,
  when: hasInput,
  run: ({ args }) => {
    const c = activeController()
    const combo = args && typeof args === 'object' && typeof args.combo === 'string' ? findCombo(args.combo) : undefined
    if (c && combo) c.sendCombo(combo)
  },
})
registerCommand({
  id: 'vnc.clipboard',
  title: 'Toggle VNC Clipboard Panel',
  category: CATEGORY,
  icon: ClipboardList,
  when: () => !!activeController() && !activeController()!.readOnly,
  run: withViewer((c) => setTabUI(c.tabId, { clipboardOpen: !useVncStore.getState().ui[c.tabId]?.clipboardOpen })),
})
registerCommand({
  id: 'vnc.pasteClipboard',
  title: 'Send Local Clipboard to VNC',
  category: CATEGORY,
  when: hasClipboardToRemote,
  run: withViewer((c) => pasteLocalClipboard(c)),
})
registerCommand({
  id: 'vnc.typeClipboard',
  title: 'Type Local Clipboard as Keystrokes',
  category: CATEGORY,
  icon: Keyboard,
  keywords: ['bios', 'ilo', 'idrac', 'paste', 'keystrokes'],
  when: hasClipboardToRemote,
  run: withViewer(async (c) => {
    try {
      const text = await navigator.clipboard.readText()
      if (!text) {
        toast.info('The clipboard is empty')
        return
      }
      c.focus()
      const n = await c.typeText(text)
      if (n) toast.success(`Typed ${n} characters`)
    } catch (err) {
      toast.error('Cannot read the clipboard', { description: `${errorMessage(err)} — use the clipboard panel instead.` })
      setTabUI(c.tabId, { clipboardOpen: true })
    }
  }),
})
registerCommand({ id: 'vnc.screenshot', title: 'Save VNC Screenshot', category: CATEGORY, icon: Camera, when: hasViewer, run: withViewer((c) => saveScreenshot(c)) })
registerCommand({ id: 'vnc.copyScreenshot', title: 'Copy VNC Screenshot', category: CATEGORY, icon: Camera, when: hasViewer, run: withViewer((c) => copyScreenshot(c)) })
registerCommand({
  id: 'vnc.fullscreen',
  title: 'VNC Full Screen',
  category: CATEGORY,
  icon: Maximize,
  keybinding: 'Control+Alt+Enter',
  when: hasViewer,
  run: withViewer((c) => toggleViewerFullscreen(c.tabId)),
})
registerCommand({
  id: 'vnc.viewOnly',
  title: 'Toggle VNC View Only',
  category: CATEGORY,
  icon: Eye,
  when: () => !!activeController() && !activeController()!.readOnly,
  run: withViewer((c) => c.setViewOnly(!c.state.viewOnly)),
})
registerCommand({ id: 'vnc.scaleFit', title: 'VNC: Scale to Fit', category: CATEGORY, icon: Scaling, when: hasViewer, run: withViewer((c) => c.setScaling('fit')) })
registerCommand({
  id: 'vnc.scaleRemote',
  title: 'VNC: Resize Remote Desktop to Tab',
  category: CATEGORY,
  icon: Expand,
  when: hasViewer,
  run: withViewer((c) => c.setScaling('remote-resize')),
})
registerCommand({ id: 'vnc.scaleNone', title: 'VNC: Original Size (1:1)', category: CATEGORY, icon: Scan, when: hasViewer, run: withViewer((c) => c.zoomReset()) })
registerCommand({
  id: 'vnc.zoomIn',
  title: 'VNC: Zoom In',
  category: CATEGORY,
  icon: ZoomIn,
  keybinding: ['$mod+Equal', '$mod+Shift+Equal', '$mod+NumpadAdd'],
  when: hasViewer,
  run: withViewer((c) => c.zoomBy(1)),
})
registerCommand({
  id: 'vnc.zoomOut',
  title: 'VNC: Zoom Out',
  category: CATEGORY,
  icon: ZoomOut,
  keybinding: ['$mod+Minus', '$mod+NumpadSubtract'],
  when: hasViewer,
  run: withViewer((c) => c.zoomBy(-1)),
})
registerCommand({
  id: 'vnc.zoomReset',
  title: 'VNC: Reset Zoom',
  category: CATEGORY,
  keybinding: ['$mod+Digit0', '$mod+Numpad0'],
  when: hasViewer,
  run: withViewer((c) => c.zoomReset()),
})
registerCommand({
  id: 'vnc.listen',
  title: 'Listen for Incoming VNC Connections…',
  category: CATEGORY,
  icon: Ear,
  keywords: ['reverse', 'listening viewer', 'x11vnc -connect', 'ultravnc sc'],
  run: () => openListeners(true),
})
registerCommand({
  id: 'vnc.connectRepeater',
  title: 'Connect Through a VNC Repeater…',
  category: CATEGORY,
  icon: Waypoints,
  keywords: ['ultravnc', 'repeater', 'nat'],
  run: () => openRepeater(true),
})
registerCommand({
  id: 'vnc.settings',
  title: 'VNC Settings',
  category: CATEGORY,
  icon: MonitorSmartphone,
  run: () => void runCommand('settings.open', { section: 'vnc' }),
})

// ---- menus ----------------------------------------------------------------------------------------------------------

registerMenu({
  menu: 'tools',
  order: 60,
  items: () => [
    { label: 'Incoming VNC connections…', icon: Ear, command: 'vnc.listen' },
    { label: 'Connect through a VNC repeater…', icon: Waypoints, command: 'vnc.connectRepeater' },
  ],
})

registerContextMenu({
  target: 'tab',
  order: 55,
  items: (ctx) => {
    if (ctx.kind !== 'vnc') return []
    const c = getController(ctx.tabId)
    const phase = c?.state.phase
    return [
      { label: 'Detach (keep session)', icon: LogOut, run: () => detach(ctx.tabId), disabled: !!c?.readOnly },
      { label: 'Reconnect', icon: RefreshCw, run: () => c?.reconnect(), disabled: !c || (c.reverse && phase !== 'connected') },
      { label: 'Screenshot', icon: Camera, run: () => c && void saveScreenshot(c), disabled: phase !== 'connected' },
      { label: 'Full screen', icon: Maximize, run: () => void toggleViewerFullscreen(ctx.tabId), disabled: !c },
      {
        label: 'View only',
        icon: Eye,
        checked: !!c?.state.viewOnly,
        run: () => c?.setViewOnly(!c.state.viewOnly),
        disabled: !c || c.readOnly,
      },
    ]
  },
})

registerSettingsSection({
  id: 'vnc',
  title: 'VNC',
  icon: MonitorSmartphone,
  order: 45,
  group: 'connections',
  keywords: ['remote desktop', 'novnc', 'clipboard', 'scaling', 'quality', 'compression', 'certificate', 'vencrypt', 'listen', 'reverse'],
  component: lazy(() => import('./SettingsSection')),
})

registerOverlay({ id: 'vnc', component: VncOverlays })

// ---- keyboard capture -----------------------------------------------------------------------------------------------

// Only consulted while focus is inside a [data-terminal] element (the desktop viewport of a VNC tab).
claimKeys((e) => {
  const el = e.target instanceof Element ? e.target.closest('[data-vnc-view]') : null
  if (!el) return false
  const tabId = el.getAttribute('data-vnc-view') ?? ''
  const ui = useVncStore.getState().ui[tabId]
  return vncSettings.get().keyboardCapture === 'all' || !!ui?.keyboardLocked
})

// ---- incoming (reverse) connections ---------------------------------------------------------------------------------

function isIncoming(ev: unknown): ev is VncIncomingEvent {
  const e = ev as Partial<VncIncomingEvent> | null
  return !!e && e.type === 'vnc.incoming' && typeof e.sessionId === 'string' && typeof e.from === 'string'
}

function openIncoming(ev: VncIncomingEvent): void {
  if (findSessionTabs(ev.sessionId).length) return
  openTab({
    kind: 'vnc',
    params: { sessionId: ev.sessionId, protocol: 'vnc', title: ev.title, reverse: true, ...(ev.viewOnly ? { viewOnly: true } : {}) },
    title: ev.title,
  })
}

events.onAny((serverEvent) => {
  // Custom module events are not part of the shared ServerEvent union.
  const ev: unknown = serverEvent
  const type = (ev as { type?: string }).type
  if (type === 'vnc.listeners') {
    void queryClient.invalidateQueries({ queryKey: vncKeys.listeners })
    return
  }
  if (!isIncoming(ev)) return
  void queryClient.invalidateQueries({ queryKey: vncKeys.listeners })
  if (vncSettings.get().openIncoming) {
    openIncoming(ev)
    toast.info('Incoming VNC connection', { description: `From ${ev.from}` })
  } else {
    toast.info('Incoming VNC connection', {
      description: `From ${ev.from}`,
      duration: 60_000,
      action: { label: 'Open', onClick: () => openIncoming(ev) },
    })
  }
})

// Signing out must not leave viewer state behind.
useAuthStore.subscribe((s, prev) => {
  if (prev.status === 'authenticated' && s.status !== 'authenticated') {
    useVncStore.setState({ ui: {}, listenersOpen: false, repeaterOpen: false })
  }
})
